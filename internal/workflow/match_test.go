package workflow

import (
	"errors"
	"testing"
)

// changed 返回一个固定的变更文件列表。
func changed(files ...string) func() ([]string, bool, error) {
	return func() ([]string, bool, error) { return files, true, nil }
}

func pushEvent(ref string, files ...string) Event {
	ev := Event{Name: EventPush, Ref: ref}
	if files != nil {
		ev.ChangedFiles = changed(files...)
	}
	return ev
}

// on.push 的 branches、tags、paths 过滤（含 -ignore 和 ! 否定），规则与 Actions 相同。
func TestMatchPush(t *testing.T) {
	cases := []struct {
		name string
		on   string
		ev   Event
		want MatchResult
	}{
		// 没有过滤条件：所有分支和 tag 都触发
		{"无过滤/分支", "push", pushEvent("refs/heads/x"), Matched},
		{"无过滤/tag", "push", pushEvent("refs/tags/v1"), Matched},
		{"无过滤/其他 ref", "push", pushEvent("refs/notes/x"), NoMatch},
		// 只写 branches：tag push 不触发
		{"branches/匹配", "{ push: { branches: [main, 'release/**'] } }", pushEvent("refs/heads/release/1/x"), Matched},
		{"branches/单层 * 不跨 /", "{ push: { branches: ['feat/*'] } }", pushEvent("refs/heads/feat/a/b"), NoMatch},
		{"branches/不匹配", "{ push: { branches: [main] } }", pushEvent("refs/heads/dev"), NoMatch},
		{"branches/tag 不触发", "{ push: { branches: [main] } }", pushEvent("refs/tags/v1"), NoMatch},
		{"branches/否定", "{ push: { branches: ['release/**', '!release/old/**'] } }", pushEvent("refs/heads/release/old/1"), NoMatch},
		{"branches/否定后再包含", "{ push: { branches: ['**', '!wip/**', 'wip/keep'] } }", pushEvent("refs/heads/wip/keep"), Matched},
		{"branches-ignore/排除", "{ push: { branches-ignore: ['dependabot/**'] } }", pushEvent("refs/heads/dependabot/npm/x"), NoMatch},
		{"branches-ignore/其他分支", "{ push: { branches-ignore: ['dependabot/**'] } }", pushEvent("refs/heads/main"), Matched},
		{"branches-ignore/否定", "{ push: { branches-ignore: ['wip/**', '!wip/ci'] } }", pushEvent("refs/heads/wip/ci"), Matched},
		{"branches-ignore/tag 不触发", "{ push: { branches-ignore: [x] } }", pushEvent("refs/tags/v1"), NoMatch},
		// 只写 tags：分支 push 不触发
		{"tags/匹配", "{ push: { tags: ['v*'] } }", pushEvent("refs/tags/v1.2.3"), Matched},
		{"tags/不匹配", "{ push: { tags: ['v*'] } }", pushEvent("refs/tags/x1"), NoMatch},
		{"tags/分支不触发", "{ push: { tags: ['v*'] } }", pushEvent("refs/heads/main"), NoMatch},
		{"tags/否定", "{ push: { tags: ['v*', '!v*-rc*'] } }", pushEvent("refs/tags/v1.0.0-rc1"), NoMatch},
		{"tags-ignore/排除", "{ push: { tags-ignore: ['nightly*'] } }", pushEvent("refs/tags/nightly-1"), NoMatch},
		{"tags-ignore/其他 tag", "{ push: { tags-ignore: ['nightly*'] } }", pushEvent("refs/tags/v1"), Matched},
		{"tags-ignore/分支不触发", "{ push: { tags-ignore: ['nightly*'] } }", pushEvent("refs/heads/main"), NoMatch},
		// branches 和 tags 都写：各自过滤
		{"branches+tags/分支", "{ push: { branches: [main], tags: ['v*'] } }", pushEvent("refs/heads/main"), Matched},
		{"branches+tags/tag", "{ push: { branches: [main], tags: ['v*'] } }", pushEvent("refs/tags/v2"), Matched},
		{"branches+tags/tag 不匹配", "{ push: { branches: [main], tags: ['v*'] } }", pushEvent("refs/tags/x"), NoMatch},
		// paths 只作用于分支 push
		{"paths/匹配", "{ push: { paths: ['src/**'] } }", pushEvent("refs/heads/main", "README.md", "src/a.go"), Matched},
		{"paths/不匹配", "{ push: { paths: ['src/**'] } }", pushEvent("refs/heads/main", "README.md"), PathsExcluded},
		{"paths/否定", "{ push: { paths: ['src/**', '!src/**/*_test.go'] } }", pushEvent("refs/heads/main", "src/a_test.go"), PathsExcluded},
		{"paths/** 匹配任意层", "{ push: { paths: ['**.md'] } }", pushEvent("refs/heads/main", "a/b/c.md"), Matched},
		{"paths/不作用于 tag", "{ push: { paths: ['src/**'] } }", pushEvent("refs/tags/v1", "README.md"), Matched},
		{"paths-ignore/全部被忽略", "{ push: { paths-ignore: ['docs/**', '*.md'] } }", pushEvent("refs/heads/main", "docs/a", "README.md"), PathsExcluded},
		{"paths-ignore/有文件没被忽略", "{ push: { paths-ignore: ['docs/**'] } }", pushEvent("refs/heads/main", "docs/a", "main.go"), Matched},
		{"paths-ignore/否定", "{ push: { paths-ignore: ['docs/**', '!docs/api/**'] } }", pushEvent("refs/heads/main", "docs/api/x"), Matched},
		{"paths/没有变更文件", "{ push: { paths: ['src/**'] } }", pushEvent("refs/heads/main", []string{}...), PathsExcluded},
		{"paths/不提供变更文件时视为匹配", "{ push: { paths: ['src/**'] } }", Event{Name: EventPush, Ref: "refs/heads/main"}, Matched},
		{"paths/分支先过滤", "{ push: { branches: [main], paths: ['src/**'] } }", pushEvent("refs/heads/dev", "src/a"), NoMatch},
		// 没有 push 触发
		{"只有 pull_request", "pull_request", pushEvent("refs/heads/main"), NoMatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := mustParse(t, "on: "+c.on+"\n"+okJob)
			got, err := wf.Match(c.ev)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Match(%+v) = %v，期望 %v", c.ev, got, c.want)
			}
		})
	}
}

func TestMatchPullRequest(t *testing.T) {
	pr := func(action, base string, files ...string) Event {
		ev := Event{Name: EventPullRequest, Action: action, BaseRef: base, Ref: PullRef(7)}
		if files != nil {
			ev.ChangedFiles = changed(files...)
		}
		return ev
	}
	cases := []struct {
		name string
		on   string
		ev   Event
		want MatchResult
	}{
		{"默认 types/opened", "pull_request", pr("opened", "main"), Matched},
		{"默认 types/synchronize", "pull_request", pr("synchronize", "main"), Matched},
		{"默认 types/reopened", "pull_request", pr("reopened", "main"), Matched},
		{"默认 types 不含 ready_for_review", "pull_request", pr("ready_for_review", "main"), NoMatch},
		{"默认 types 不含 closed", "pull_request", pr("closed", "main"), NoMatch},
		{"指定 types", "{ pull_request: { types: [ready_for_review] } }", pr("ready_for_review", "main"), Matched},
		{"指定 types 后 opened 不触发", "{ pull_request: { types: [ready_for_review] } }", pr("opened", "main"), NoMatch},
		{"branches 按目标分支过滤", "{ pull_request: { branches: [main] } }", pr("opened", "dev"), NoMatch},
		{"branches 匹配目标分支", "{ pull_request: { branches: ['rel/*'] } }", pr("opened", "rel/1"), Matched},
		{"branches-ignore", "{ pull_request: { branches-ignore: ['rel/*'] } }", pr("opened", "rel/1"), NoMatch},
		{"paths 匹配", "{ pull_request: { paths: ['src/**'] } }", pr("opened", "main", "src/a"), Matched},
		{"paths 不匹配", "{ pull_request: { paths: ['src/**'] } }", pr("opened", "main", "doc/a"), PathsExcluded},
		{"paths-ignore 全部忽略", "{ pull_request: { paths-ignore: ['doc/**'] } }", pr("opened", "main", "doc/a"), PathsExcluded},
		{"没有 pull_request 触发", "push", pr("opened", "main"), NoMatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := mustParse(t, "on: "+c.on+"\n"+okJob)
			got, err := wf.Match(c.ev)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Match(%+v) = %v，期望 %v", c.ev, got, c.want)
			}
		})
	}
}

func TestMatchOtherEvents(t *testing.T) {
	cases := []struct {
		name string
		on   string
		ev   string
		want MatchResult
	}{
		{"workflow_dispatch", "workflow_dispatch", EventWorkflowDispatch, Matched},
		{"没有 workflow_dispatch", "push", EventWorkflowDispatch, NoMatch},
		{"schedule", "{ schedule: [{ cron: '0 0 * * *' }] }", EventSchedule, Matched},
		{"schedule 为空列表", "{ push: {}, schedule: [] }", EventSchedule, NoMatch},
		{"没有 schedule", "push", EventSchedule, NoMatch},
		{"未知事件", "push", "release", NoMatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := mustParse(t, "on: "+c.on+"\n"+okJob)
			got, err := wf.Match(Event{Name: c.ev, Ref: "refs/heads/main"})
			if err != nil || got != c.want {
				t.Errorf("Match(%s) = %v, %v，期望 %v", c.ev, got, err, c.want)
			}
		})
	}
}

// 变更文件列表不完整时视为匹配；获取失败时返回错误。
func TestMatchChangedFiles(t *testing.T) {
	wf := mustParse(t, "on: { push: { paths: ['src/**'] } }\n"+okJob)
	calls := 0
	got, err := wf.Match(Event{Name: EventPush, Ref: "refs/heads/main", ChangedFiles: func() ([]string, bool, error) {
		calls++
		return []string{"README.md"}, false, nil
	}})
	if err != nil || got != Matched {
		t.Errorf("列表不完整时应匹配：%v, %v", got, err)
	}
	if calls != 1 {
		t.Errorf("ChangedFiles 应调用 1 次，实际 %d 次", calls)
	}

	boom := errors.New("boom")
	_, err = wf.Match(Event{Name: EventPush, Ref: "refs/heads/main", ChangedFiles: func() ([]string, bool, error) { return nil, false, boom }})
	if !errors.Is(err, boom) {
		t.Errorf("获取变更文件出错时应返回该错误：%v", err)
	}

	// 不需要 paths 过滤时不获取变更文件
	wf = mustParse(t, "on: { push: { branches: [main] } }\n"+okJob)
	_, err = wf.Match(Event{Name: EventPush, Ref: "refs/heads/main", ChangedFiles: func() ([]string, bool, error) {
		t.Error("没有 paths 过滤时不应获取变更文件")
		return nil, true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInputMatcherPatterns(t *testing.T) {
	cases := []struct {
		name   string
		inputs []string
		path   string
		want   bool
	}{
		{"精确文件", []string{"go.mod"}, "go.mod", true},
		{"精确文件不匹配前缀相同的文件", []string{"go.mod"}, "go.mod.bak", false},
		{"不带 / 的目录名也匹配目录下的文件", []string{"cmd"}, "cmd/a/b.go", true},
		{"去掉开头的 ./", []string{"./cmd/"}, "cmd/x", true},
		{"去掉开头的 /", []string{"/cmd/"}, "cmd/x", true},
		{". 表示整个仓库", []string{"."}, "a/b.go", true}, // 以前当成名为 . 的路径，什么都不匹配
		{"排除 .", []string{"src/", "!."}, "src/a.go", false},
		{"? 通配符", []string{"a?.txt"}, "ab.txt", true},
		{"{} 通配符", []string{"*.{go,mod}"}, "go.mod", true},
		{"后面的排除覆盖前面的包含", []string{"src/", "!src/gen/"}, "src/gen/a.go", false},
		{"后面的包含覆盖前面的排除", []string{"src/", "!src/gen/", "src/gen/keep.go"}, "src/gen/keep.go", true},
		{"只有排除时什么都不匹配", []string{"!x"}, "y", false},
		{"空列表什么都不匹配", []string{}, "a", false},
		{"表达式展开出的非法模式视为不匹配", []string{"src/[a"}, "src/[a", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewInputMatcher(c.inputs)
			if m.Default() {
				t.Error("显式的 inputs 不应是默认值")
			}
			if got := m.Match(c.path); got != c.want {
				t.Errorf("NewInputMatcher(%q).Match(%q) = %v，期望 %v", c.inputs, c.path, got, c.want)
			}
		})
	}

	d := NewInputMatcher(nil)
	if !d.Default() {
		t.Error("nil inputs 应是默认值")
	}
	if got := d.EmptyPatterns([]string{"a"}); got != nil {
		t.Errorf("默认 inputs 不报告空模式：%v", got)
	}
}

// 过滤模式的语法与 Actions 的 filter pattern cheat sheet 一致（回归测试：之前用 doublestar 匹配，
// **.md 这样与其他字符写在同一段中的 ** 只能匹配一层，docs/a.md 不被 paths: ['**.md'] 匹配）。
func TestFilterPattern(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"feature/*", "feature/my-branch", true},
		{"feature/*", "feature/your/branch", false},
		{"feature/**", "feature/beta-a/my-branch", true},
		{"*", "main", true},
		{"*", "releases/v1", false},
		{"**", "releases/v1", true},
		{"*feature", "mona-feature", true},
		{"*feature", "feature", true},
		{"v2*", "v2", true},
		{"v2*", "v2.9", true},
		{"v[12].[0-9]+.[0-9]+", "v1.10.1", true},
		{"v[12].[0-9]+.[0-9]+", "v2.0.0", true},
		{"v[12].[0-9]+.[0-9]+", "v3.0.0", false},
		{"v[12].[0-9]+.[0-9]+", "v1x0.0", false}, // . 按字面匹配
		{"*.jsx?", "page.js", true},
		{"*.jsx?", "page.jsx", true},
		{"*.jsx?", "page.jsxx", false},
		{"**.js", "index.js", true},
		{"**.js", "src/js/app.js", true},
		{"docs/*", "docs/README.md", true},
		{"docs/*", "docs/a/b.md", false},
		{"docs/**/*.md", "docs/README.md", true},
		{"docs/**/*.md", "docs/a/markdown/file.md", true},
		{"**/docs/**", "a/docs/b/c", true},
		{"**/README.md", "README.md", true},
		{"**/README.md", "a/b/README.md", true},
		{"**/*-post.md", "my-post.md", true},
		{"**/migrate-*.sql", "db/migrate-v1.sql", true},
		{"**/migrate-*.sql", "db/migrate-v1/x.sql", false},
		{`\*.md`, "*.md", true},
		{`\*.md`, "a.md", false},
		{"{a,b}", "{a,b}", true}, // 不支持花括号展开，按字面匹配
		{"(x)", "(x)", true},     // 正则的特殊字符按字面匹配
	}
	for _, c := range cases {
		t.Run(c.pattern+"/"+c.value, func(t *testing.T) {
			if !validFilterPattern(c.pattern) {
				t.Fatalf("模式 %q 应合法", c.pattern)
			}
			if got := compileFilter([]string{c.pattern}).match(c.value); got != c.want {
				t.Errorf("模式 %q 匹配 %q = %v，期望 %v", c.pattern, c.value, got, c.want)
			}
		})
	}

	for _, p := range []string{"[a", "a/[x", `a\`, "!*["} {
		t.Run("无效/"+p, func(t *testing.T) {
			if validFilterPattern(p) {
				t.Errorf("模式 %q 应无效", p)
			}
		})
	}
}

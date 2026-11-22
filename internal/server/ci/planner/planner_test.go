package planner

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestWorkflowFiles(t *testing.T) {
	tree := &gh.Tree{Tree: []gh.TreeEntry{
		{Path: ".kiln/workflows/ci.yml", Type: "blob", SHA: "a"},
		{Path: ".kiln/workflows/release.yaml", Type: "blob", SHA: "b"},
		{Path: ".kiln/workflows/README.md", Type: "blob", SHA: "c"},
		{Path: ".kiln/workflows/sub/x.yml", Type: "blob", SHA: "d"},
		{Path: ".kiln/workflows/dir.yml", Type: "tree", SHA: "e"},
		{Path: ".github/workflows/ci.yml", Type: "blob", SHA: "f"},
	}}
	got := workflowFiles(tree)
	want := map[string]string{"ci.yml": "a", "release.yaml": "b"}
	if !maps.Equal(got, want) {
		t.Fatalf("workflowFiles = %v，期望 %v", got, want)
	}
}

func TestGitHubContext(t *testing.T) {
	repo := &store.Repo{Owner: "o", Name: "r"}
	tests := []struct {
		name             string
		ev               Request
		refType, refName string
	}{
		{"分支", Request{Repo: repo, Event: "push", Ref: "refs/heads/main"}, "branch", "main"},
		{"tag", Request{Repo: repo, Event: "push", Ref: "refs/tags/v1.0.0"}, "tag", "v1.0.0"},
		{"PR", Request{Repo: repo, Event: "pull_request", Ref: "refs/pull/7/head", HeadRef: "feat", BaseRef: "main"}, "branch", "7/head"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := githubContext(&tt.ev)
			if g.RefType != tt.refType || g.RefName != tt.refName || g.Repository != "o/r" {
				t.Fatalf("githubContext = %+v", g)
			}
			if tt.ev.Event == "pull_request" && (g.HeadRef != "feat" || g.BaseRef != "main") {
				t.Fatalf("PR 的 head/base ref = %q/%q", g.HeadRef, g.BaseRef)
			}
			if tt.ev.Event != "pull_request" && g.HeadRef != "" {
				t.Fatalf("非 PR 事件不应填写 head_ref：%q", g.HeadRef)
			}
		})
	}
}

func TestDedupeKeyAndPriority(t *testing.T) {
	repo := &store.Repo{ID: 5, DefaultBranch: "main"}
	tests := []struct {
		name     string
		ev       Request
		key      string
		priority int
	}{
		{"tag push", Request{Repo: repo, Event: "push", Ref: "refs/tags/v1", SHA: "s"}, "5:ci.yml:push:refs/tags/v1:s", priorityTag},
		{"默认分支", Request{Repo: repo, Event: "push", Ref: "refs/heads/main", SHA: "s"}, "5:ci.yml:push:refs/heads/main:s", priorityDefaultBranch},
		{"其他分支", Request{Repo: repo, Event: "push", Ref: "refs/heads/dev", SHA: "s"}, "5:ci.yml:push:refs/heads/dev:s", priorityNormal},
		{"PR", Request{Repo: repo, Event: "pull_request", Ref: "refs/pull/1/head", SHA: "s", BaseRef: "main"}, "5:ci.yml:pull_request:refs/pull/1/head:s:main", priorityNormal},
		{"dispatch", Request{Repo: repo, Event: "workflow_dispatch", Ref: "refs/heads/main", RequestID: "abc"}, "dispatch:abc", priorityNormal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dedupeKey(&tt.ev, "ci.yml"); got != tt.key {
				t.Errorf("dedupeKey = %q，期望 %q", got, tt.key)
			}
			if got := priorityFor(&tt.ev); got != tt.priority {
				t.Errorf("priorityFor = %d，期望 %d", got, tt.priority)
			}
		})
	}
}

// chain 返回 a → b → c 和独立的 d。
func chain() []*workflow.PlannedJob {
	return []*workflow.PlannedJob{
		{Key: "a"},
		{Key: "b", NeedsKeys: []string{"a"}},
		{Key: "c", NeedsKeys: []string{"b"}},
		{Key: "d"},
	}
}

func TestDownstream(t *testing.T) {
	got := slices.Sorted(maps.Keys(downstream(chain(), "a")))
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("downstream(a) = %v，期望 %v", got, want)
	}
	got = slices.Sorted(maps.Keys(downstream(chain(), "d")))
	if want := []string{"d"}; !slices.Equal(got, want) {
		t.Fatalf("downstream(d) = %v，期望 %v", got, want)
	}
}

func TestRerunPlan(t *testing.T) {
	manifest := "m"
	prev := []*cistore.Job{
		{ID: 1, JobKey: "a", State: store.JobSucceeded, ResultManifest: &manifest},
		{ID: 2, JobKey: "b", State: store.JobFailed},
		{ID: 3, JobKey: "c", State: store.JobSkipped},
		{ID: 4, JobKey: "d", State: store.JobSucceeded, ResultManifest: &manifest},
		{ID: 5, JobKey: "p", State: store.JobSucceeded, Kind: workflow.JobKindPublish},
	}
	pr := &plannedRun{Jobs: append(chain(), &workflow.PlannedJob{Key: "p", NeedsKeys: []string{"d"}})}
	tests := []struct {
		name   string
		req    RerunRequest
		purged bool
		reuse  []string
		force  []string
	}{
		{"默认只重跑失败的", RerunRequest{RunID: 9}, false, []string{"a", "d", "p"}, nil},
		{"全部", RerunRequest{RunID: 9, Mode: store.RerunAll}, false, nil, nil},
		{"全部且不用缓存", RerunRequest{RunID: 9, Mode: store.RerunAllNoCache}, false, nil, []string{"a", "b", "c", "d", "p"}},
		{"成功的 job 及下游", RerunRequest{RunID: 9, Mode: store.RerunJob, JobID: 1}, false, []string{"d", "p"}, []string{"a"}},
		{"失败的 job 及下游", RerunRequest{RunID: 9, Mode: store.RerunJob, JobID: 2}, false, []string{"a", "d", "p"}, nil},
		// 过程数据已经清理：有产物的 job 不能沿用，只沿用成功的发布 job（不再次发布）
		{"已清理：只重跑失败的", RerunRequest{RunID: 9}, true, []string{"p"}, nil},
		{"已清理：失败的 job 及下游", RerunRequest{RunID: 9, Mode: store.RerunJob, JobID: 2}, true, []string{"p"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt, err := rerunPlan(tt.req, prev, pr, tt.purged)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(opt.reuse)); !slices.Equal(got, tt.reuse) {
				t.Errorf("reuse = %v，期望 %v", got, tt.reuse)
			}
			if got := slices.Sorted(maps.Keys(opt.force)); !slices.Equal(got, tt.force) {
				t.Errorf("force = %v，期望 %v", got, tt.force)
			}
		})
	}
	if _, err := rerunPlan(RerunRequest{RunID: 9, Mode: store.RerunJob, JobID: 42}, prev, pr, false); err == nil {
		t.Error("job 不属于 run 时应返回错误")
	}
	if _, err := rerunPlan(RerunRequest{RunID: 9, Mode: "bogus"}, prev, pr, false); err == nil {
		t.Error("未知的重跑方式应返回错误")
	}
}

func TestDedupStrings(t *testing.T) {
	got := dedupStrings([]string{"x", "y", "x", "z", "y"})
	if want := []string{"x", "y", "z"}; !slices.Equal(got, want) {
		t.Fatalf("dedupStrings = %v，期望 %v", got, want)
	}
}

// TestNoGitHubApp：没有配置 GitHub App 时，规划、手动触发和重跑都返回 errNoGitHubApp，不访问数据库。
func TestNoGitHubApp(t *testing.T) {
	t.Parallel()
	p := New(Deps{})
	ctx := t.Context()
	tests := []struct {
		name string
		call func() error
	}{
		{"PlanEvent", func() error { _, err := p.PlanEvent(ctx, &Request{Repo: &store.Repo{}}); return err }},
		{"Dispatch", func() error { _, err := p.Dispatch(ctx, &DispatchRequest{Repo: "o/r"}); return err }},
		{"DispatchWorkflows", func() error { _, err := p.DispatchWorkflows(ctx, "o/r", ""); return err }},
		{"Rerun", func() error { _, err := p.Rerun(ctx, RerunRequest{RunID: 1}); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, errNoGitHubApp) {
				t.Fatalf("应返回 errNoGitHubApp，实际 %v", err)
			}
		})
	}
}

// eventWorkflow：push 到 main、v* tag 和 PR 时执行；build → test。
const eventWorkflow = `name: ev
on:
  push:
    branches: [main]
    tags: ["v*"]
  pull_request:
jobs:
  build:
    runs-on: linux/arm64
    steps:
      - run: make build
  test:
    needs: [build]
    runs-on: linux/arm64
    steps:
      - run: make test
`

// TestPlanEventTriggers：push、tag、PR 各自产生一个 run（PLAN #7.4）：run 记录事件信息和优先级，
// 没有上游的 job 经就绪判定进入 pending，有上游的 waiting；Check Run 写入 outbox。分支过滤不匹配时不建 run。
func TestPlanEventTriggers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      func(e *testEnv, sha string) *Request
		priority int
		refType  string
		prNumber int
	}{
		{"默认分支 push", func(e *testEnv, sha string) *Request { return e.pushReq("refs/heads/main", sha) }, priorityDefaultBranch, "branch", 0},
		{"tag push", func(e *testEnv, sha string) *Request { return e.pushReq("refs/tags/v1.0.0", sha) }, priorityTag, "tag", 0},
		{"pull_request", func(e *testEnv, sha string) *Request { return e.prReq(7, "opened", sha) }, priorityNormal, "branch", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("ev.yml"): eventWorkflow, "main.go": "package main\n"})
			req := tt.req(e, sha)
			r := e.planOne(req)
			if r.State != store.RunActive || r.Event != req.Event || r.Ref != req.Ref || r.SHA != sha || r.Workflow != "ev.yml" || r.WorkflowName != "ev" {
				t.Fatalf("run = %+v", r)
			}
			if r.Priority != tt.priority {
				t.Errorf("run 的优先级 = %d，期望 %d", r.Priority, tt.priority)
			}
			if r.GitHubCtx.RefType != tt.refType || r.GitHubCtx.SHA != sha || r.GitHubCtx.Actor != testOwner {
				t.Errorf("github 上下文 = %+v", r.GitHubCtx)
			}
			if tt.prNumber != 0 && (r.PRNumber == nil || *r.PRNumber != tt.prNumber || r.GitHubCtx.HeadRef != "feat" || r.GitHubCtx.BaseRef != "main") {
				t.Errorf("PR 的 run：pr_number=%v head/base=%q/%q", r.PRNumber, r.GitHubCtx.HeadRef, r.GitHubCtx.BaseRef)
			}
			if tt.prNumber == 0 && r.PRNumber != nil {
				t.Errorf("非 PR 的 run 不应有 pr_number：%d", *r.PRNumber)
			}
			if r.CommitTime == 0 {
				t.Error("应记录 commit 时间")
			}
			if s := e.rerunState(r.ID, 0); s != store.RunActive {
				t.Errorf("run_reruns 第 0 次运行的状态 = %s", s)
			}
			e.wantBuildTestJobs(r.ID, tt.priority)
			e.wantRunOutbox(r.ID, 0)
		})
	}

	t.Run("分支过滤不匹配", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		sha := e.commit(map[string]string{wf("ev.yml"): eventWorkflow})
		if ids := e.plan(e.pushReq("refs/heads/dev", sha)); len(ids) != 0 {
			t.Fatalf("dev 分支不在 branches 中，不应建 run：%v", ids)
		}
		if ids := e.plan(e.pushReq("refs/tags/release-1", sha)); len(ids) != 0 {
			t.Fatalf("tag 不匹配 v*，不应建 run：%v", ids)
		}
		if ids := e.plan(e.prReq(3, "closed", sha)); len(ids) != 0 {
			t.Fatalf("PR closed 不在默认 types 中，不应建 run：%v", ids)
		}
		if n := e.countRuns(); n != 0 {
			t.Fatalf("不应有 run，实际 %d 个", n)
		}
	})
}

// TestPlanEventMatrix：矩阵展开为多个 job，key 带矩阵取值，下游的 needs 包含全部变体。
func TestPlanEventMatrix(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{wf("matrix.yml"): matrixWorkflow})
	r := e.planOne(e.pushReq("refs/heads/main", sha))
	jobs := e.jobs(r.ID, 0)
	want := []string{"build{os=darwin}", "build{os=linux}", "pack"}
	if got := slices.Sorted(maps.Keys(jobs)); !slices.Equal(got, want) {
		t.Fatalf("job key = %v，期望 %v", got, want)
	}
	var buildIDs []int64
	for _, k := range want[:2] {
		j := jobs[k]
		os := strings.TrimSuffix(strings.TrimPrefix(k, "build{os="), "}")
		if j.JobName != "build" || j.Matrix["os"] != os || j.State != store.JobPending || !j.Spec.FailFast {
			t.Errorf("%s：job_name=%s matrix=%v state=%s fail_fast=%v", k, j.JobName, j.Matrix, j.State, j.Spec.FailFast)
		}
		if !strings.Contains(j.DisplayName, os) {
			t.Errorf("%s 的显示名应包含矩阵取值：%q", k, j.DisplayName)
		}
		buildIDs = append(buildIDs, j.ID)
	}
	pack := jobs["pack"]
	if pack.State != store.JobWaiting || pack.Matrix != nil {
		t.Errorf("pack：state=%s matrix=%v", pack.State, pack.Matrix)
	}
	if got := slices.Sorted(slices.Values(pack.Needs)); !slices.Equal(got, slices.Sorted(slices.Values(buildIDs))) {
		t.Errorf("pack 的 needs = %v，应为全部 build 变体 %v", pack.Needs, buildIDs)
	}
	// 两个变体的命令不同，action key 也不同（不会互相跟随）
	if *jobs[want[0]].ActionKey == *jobs[want[1]].ActionKey {
		t.Error("不同矩阵变体的 action key 应不同")
	}
}

// wantBuildTestJobs 断言 eventWorkflow 的 job：没有上游的 build 进入 pending 并已计算 action key，
// test 等待 build；job 的优先级与 run 相同。
func (e *testEnv) wantBuildTestJobs(runID int64, priority int) {
	t := e.t
	t.Helper()
	jobs := e.jobs(runID, 0)
	build, test := jobs["build"], jobs["test"]
	if build == nil || test == nil || len(jobs) != 2 {
		t.Fatalf("job：%v", slices.Sorted(maps.Keys(jobs)))
	}
	if build.State != store.JobPending || build.ActionKey == nil || build.InputDigest == nil {
		t.Errorf("没有上游的 build 应进入 pending 并计算 action key：%s key=%v digest=%v", build.State, build.ActionKey, build.InputDigest)
	}
	if test.State != store.JobWaiting || !slices.Equal(test.Needs, []int64{build.ID}) {
		t.Errorf("test 应等待 build：%s needs=%v", test.State, test.Needs)
	}
	for _, j := range jobs {
		if j.Priority != priority {
			t.Errorf("job %s 的优先级 = %d，应与 run 相同（%d）", j.JobKey, j.Priority, priority)
		}
	}
}

// pathsWorkflow：只有 src/ 下的文件变化时执行（push 和 PR）。
const pathsWorkflow = `name: paths
on:
  push:
    branches: [main]
    paths: ["src/**"]
  pull_request:
    paths: ["src/**"]
jobs:
  build:
    runs-on: linux/arm64
    steps:
      - run: make
`

// TestPlanEventPaths：paths 过滤（PLAN #13.3）。变更文件都不匹配时仍然建一个 skipped 的 run（没有 job）；
// 新建分支（before 为全 0）拿不到变更文件，视为匹配。
func TestPlanEventPaths(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.commit(map[string]string{wf("paths.yml"): pathsWorkflow, "src/a.go": "package a\n"})
	docs := e.commit(map[string]string{"docs/readme.md": "doc\n"})
	src := e.commit(map[string]string{"src/a.go": "package a // v2\n"})

	tests := []struct {
		name string
		req  *Request
		want store.RunState
	}{
		{"新建分支视为匹配", e.pushReq("refs/heads/main", first), store.RunActive},
		{"push 只改了 docs", &Request{Repo: e.repo, Event: "push", Ref: "refs/heads/main", SHA: docs, Before: first, Source: SourceWebhook}, store.RunSkipped},
		{"push 改了 src", &Request{Repo: e.repo, Event: "push", Ref: "refs/heads/main", SHA: src, Before: docs, Source: SourceWebhook}, store.RunActive},
		{"PR 只改了 docs", e.prReq(1, "opened", docs), store.RunSkipped},
		{"PR 改了 src", e.prReq(2, "synchronize", src), store.RunActive},
	}
	// 共用一个仓库和数据库，按顺序执行
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.planOne(tt.req)
			if r.State != tt.want {
				t.Fatalf("run 状态 = %s，期望 %s", r.State, tt.want)
			}
			jobs := e.jobs(r.ID, 0)
			if tt.want == store.RunSkipped {
				if len(jobs) != 0 || r.FinishedAt == nil || e.rerunState(r.ID, 0) != store.RunSkipped {
					t.Errorf("skipped 的 run 应已结束且没有 job：%d 个 job，finished_at=%v", len(jobs), r.FinishedAt)
				}
				e.wantOutbox(fmt.Sprintf("checkrun:run:%d:0", r.ID))
			} else if len(jobs) != 1 {
				t.Errorf("应有 1 个 job，实际 %d 个", len(jobs))
			}
		})
	}
}

// TestPlanEventDedupe：同一个事件（仓库、workflow、事件、ref、sha）只建一个 run；ref 或 sha 不同时各建一个。
func TestPlanEventDedupe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{wf("ev.yml"): eventWorkflow, wf("matrix.yml"): matrixWorkflow})
	ids := e.plan(e.pushReq("refs/heads/main", sha))
	if len(ids) != 2 {
		t.Fatalf("两个 workflow 都被 main 的 push 触发，应建 2 个 run：%v", ids)
	}
	if again := e.plan(e.pushReq("refs/heads/main", sha)); len(again) != 0 {
		t.Fatalf("重复的事件不应再建 run：%v", again)
	}
	// 同一个 sha 的 tag push 是另一个事件（只触发 ev.yml）
	if tag := e.plan(e.pushReq("refs/tags/v1", sha)); len(tag) != 1 {
		t.Fatalf("tag push 应建 1 个 run：%v", tag)
	}
	if n := e.countRuns(); n != 3 {
		t.Fatalf("应共有 3 个 run，实际 %d 个", n)
	}
	var keys []string
	rows, err := e.db.Query(e.ctx, `SELECT dedupe_key FROM runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := []string{
		fmt.Sprintf("%d:ev.yml:push:refs/heads/main:%s", testRepoID, sha),
		fmt.Sprintf("%d:matrix.yml:push:refs/heads/main:%s", testRepoID, sha),
		fmt.Sprintf("%d:ev.yml:push:refs/tags/v1:%s", testRepoID, sha),
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("dedupe_key = %v，期望 %v", keys, want)
	}
}

// TestPlanEventConfigError：workflow 有配置错误时建一个已失败的 run，记录错误，没有 job（PLAN #4.9）。
// on 无法解析时不知道是否被触发，任何事件都报告；只有其他错误时仍按 on 判断是否触发。
func TestPlanEventConfigError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		ref     string
		created bool
		inErr   string
	}{
		{"uses 不支持", "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps:\n      - uses: actions/checkout@v4\n", "refs/heads/main", true, "uses"},
		{"YAML 无法解析", "on: [push\njobs: {", "refs/heads/dev", true, "ev.yml"},
		{"有错误但事件不匹配", "on:\n  push:\n    branches: [main]\njobs:\n  a:\n    runs-on: linux/arm64\n    steps:\n      - uses: x\n", "refs/heads/dev", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("ev.yml"): tt.content})
			ids := e.plan(e.pushReq(tt.ref, sha))
			if !tt.created {
				if len(ids) != 0 {
					t.Fatalf("不应建 run：%v", ids)
				}
				return
			}
			if len(ids) != 1 {
				t.Fatalf("应建 1 个 run：%v", ids)
			}
			r := e.run(ids[0])
			if r.State != store.RunFailed || r.ConfigError == nil || !strings.Contains(*r.ConfigError, tt.inErr) || r.FinishedAt == nil {
				t.Fatalf("配置错误的 run：state=%s config_error=%v finished_at=%v", r.State, r.ConfigError, r.FinishedAt)
			}
			if n := len(e.jobs(r.ID, 0)); n != 0 {
				t.Errorf("配置错误的 run 不应有 job，实际 %d 个", n)
			}
			var conclusion *string
			if err := e.db.QueryRow(e.ctx, `SELECT conclusion FROM run_reruns WHERE run_id=$1 AND rerun=0`, r.ID).Scan(&conclusion); err != nil {
				t.Fatal(err)
			}
			if conclusion == nil || *conclusion != string(store.ConclusionFailure) {
				t.Errorf("run_reruns 的结论 = %v，期望 failure", conclusion)
			}
			e.wantOutbox(fmt.Sprintf("checkrun:run:%d:0", r.ID))
		})
	}
}

// TestPlanEventNoWorkflows：commit 中没有 workflow，或只有手动触发的 workflow 时，push 不建 run。
func TestPlanEventNoWorkflows(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{"README.md": "x\n", ".github/workflows/ci.yml": eventWorkflow})
	if ids := e.plan(e.pushReq("refs/heads/main", sha)); len(ids) != 0 {
		t.Fatalf("没有 .kiln/workflows 时不应建 run：%v", ids)
	}
	sha = e.commit(map[string]string{wf("manual.yml"): dispatchWorkflow})
	if ids := e.plan(e.pushReq("refs/heads/main", sha)); len(ids) != 0 {
		t.Fatalf("只声明 workflow_dispatch 的 workflow 不应被 push 触发：%v", ids)
	}
}

// TestPlanEventCompletesRepo：仓库记录缺少 clone 地址（来自 installation 事件）时，规划前定向补齐。
func TestPlanEventCompletesRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.exec(`UPDATE repos SET clone_url='', default_branch='' WHERE id=$1`, e.repo.ID)
	e.repo.CloneURL, e.repo.DefaultBranch = "", ""
	sha := e.commit(map[string]string{wf("ev.yml"): eventWorkflow})
	e.planOne(e.pushReq("refs/heads/main", sha))
	got, err := store.GetRepo(e.ctx, e.db, e.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CloneURL != "file://"+e.fake.Dir || got.DefaultBranch != "main" {
		t.Fatalf("仓库记录应已补齐：clone_url=%q default_branch=%q", got.CloneURL, got.DefaultBranch)
	}
}

// TestPlanEventGitHubError：读不到仓库内容（例如仓库已从 App 的授权范围中移除）时规划返回错误、不建 run；
// 重跑优先使用按 sha 缓存的内容，没有缓存时同样返回错误。
func TestPlanEventGitHubError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{wf("ev.yml"): eventWorkflow})
	r := e.planOne(e.pushReq("refs/heads/main", sha))
	e.finishRun(r.ID, nil)
	next := e.commit(nil)
	e.gh.RemoveRepo(e.fake)

	if _, err := e.p.PlanEvent(e.ctx, e.pushReq("refs/heads/main", next)); err == nil || !strings.Contains(err.Error(), "读取 git tree") {
		t.Fatalf("应返回读取 git tree 的错误：%v", err)
	}
	if n := e.countRuns(); n != 1 {
		t.Fatalf("出错时不应建 run：%d 个", n)
	}
	// 已规划过的 commit 的 tree、workflow 文件和 commit 时间按 sha 缓存，重跑不再请求 GitHub
	before := len(e.gh.Requests())
	if n, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r.ID, Mode: store.RerunAll}); err != nil || n != 1 {
		t.Fatalf("重跑应使用缓存的内容：%d, %v", n, err)
	}
	if reqs := e.gh.Requests()[before:]; len(reqs) != 0 {
		t.Errorf("重跑不应请求 GitHub：%v", reqs)
	}
	// 新的 Planner 没有缓存：读不到仓库时重跑失败，run 不变
	e.finishRun(r.ID, nil)
	fresh := New(Deps{DB: e.db, GitHub: e.client, Engine: e.eng, Outbox: e.p.outbox, Hub: e.p.hub, Log: e.p.log})
	if _, err := fresh.Rerun(e.ctx, RerunRequest{RunID: r.ID}); !gh.IsNotFound(err) {
		t.Fatalf("读不到仓库时重跑应返回 404 错误：%v", err)
	}
	if got := e.run(r.ID); got.CurrentRerun != 1 {
		t.Fatalf("重跑失败不应改变 run：current_rerun=%d", got.CurrentRerun)
	}
}

// concurrencyWorkflow 返回声明了 run 级并发组的 workflow。
func concurrencyWorkflow(cancelInProgress bool) string {
	return fmt.Sprintf(`name: conc
on:
  push:
    branches: [main]
concurrency:
  group: deploy-${{ github.ref }}
  cancel-in-progress: %v
jobs:
  build:
    runs-on: linux/arm64
    steps:
      - run: make
`, cancelInProgress)
}

// TestPlanEventConcurrencyQueue：cancel-in-progress 为 false 时，组内已有执行中的 run，新 run 排队（job 保持 waiting）；
// 再来一个时取代排队中的那个（取消，cancel_reason=concurrency）；执行中的 run 结束后排队的 run 开始（PLAN #6.5）。
func TestPlanEventConcurrencyQueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	push := func(n int) *cistore.Run {
		t.Helper()
		sha := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(false), "v.txt": strconv.Itoa(n)})
		return e.planOne(e.pushReq("refs/heads/main", sha))
	}
	r1 := push(1)
	if r1.State != store.RunActive || r1.ConcurrencyKey == nil || *r1.ConcurrencyKey != "deploy-refs/heads/main" || r1.CancelInProgress {
		t.Fatalf("第一个 run：state=%s key=%v cancel_in_progress=%v", r1.State, r1.ConcurrencyKey, r1.CancelInProgress)
	}
	r2 := push(2)
	if r2 = e.run(r2.ID); r2.State != store.RunQueued || e.rerunState(r2.ID, 0) != store.RunQueued {
		t.Fatalf("组内已有执行中的 run，第二个应排队：%s", r2.State)
	}
	if j := e.jobs(r2.ID, 0)["build"]; j.State != store.JobWaiting {
		t.Fatalf("排队中的 run 的 job 应保持 waiting：%s", j.State)
	}
	r3 := push(3)
	if got := e.run(r2.ID); got.State != store.RunCancelled {
		t.Fatalf("排队中的 run 应被新的 run 取代（取消），实际 %s", got.State)
	}
	if j := e.jobs(r2.ID, 0)["build"]; j.State != store.JobCancelled || deref(j.CancelReason) != string(store.CancelByConcurrency) {
		t.Fatalf("被取代的 run 的 job：%s cancel_reason=%s", j.State, deref(j.CancelReason))
	}
	if got := e.run(r3.ID); got.State != store.RunQueued {
		t.Fatalf("第三个 run 应排队：%s", got.State)
	}
	if got := e.run(r1.ID); got.State != store.RunActive {
		t.Fatalf("执行中的 run 不受影响：%s", got.State)
	}

	e.finishRun(r1.ID, nil)
	if got := e.run(r3.ID); got.State != store.RunActive {
		t.Fatalf("执行中的 run 结束后，排队的 run 应开始：%s", got.State)
	}
	if j := e.jobs(r3.ID, 0)["build"]; j.State != store.JobPending {
		t.Fatalf("开始后的 run 的 job 应进入 pending：%s", j.State)
	}
}

// TestPlanEventConcurrencyCancelInProgress：cancel-in-progress 为 true 时，新 run 取消组内所有未结束的 run 并立即开始。
func TestPlanEventConcurrencyCancelInProgress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha1 := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(true), "v.txt": "1"})
	r1 := e.planOne(e.pushReq("refs/heads/main", sha1))
	sha2 := e.commit(map[string]string{"v.txt": "2"})
	r2 := e.planOne(e.pushReq("refs/heads/main", sha2))
	if got := e.run(r1.ID); got.State != store.RunCancelled {
		t.Fatalf("旧 run 应被取消：%s", got.State)
	}
	if j := e.jobs(r1.ID, 0)["build"]; j.State != store.JobCancelled || deref(j.CancelReason) != string(store.CancelByConcurrency) {
		t.Fatalf("旧 run 的 job：%s cancel_reason=%s", j.State, deref(j.CancelReason))
	}
	if got := e.run(r2.ID); got.State != store.RunActive || !got.CancelInProgress {
		t.Fatalf("新 run 应立即开始：%s cancel_in_progress=%v", got.State, got.CancelInProgress)
	}
	if j := e.jobs(r2.ID, 0)["build"]; j.State != store.JobPending {
		t.Fatalf("新 run 的 job 应进入 pending：%s", j.State)
	}
}

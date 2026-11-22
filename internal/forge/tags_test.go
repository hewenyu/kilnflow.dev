package forge

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

// gitRun 在 dir 中执行 git 命令，返回去掉首尾空白的输出。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v：%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newTagRepo 建一个有两个 commit 的仓库：第一个 commit 有轻量 tag v0.1.0 和 annotated tag v0.1.1、
// 以及不是版本的 tag nightly；返回 fake、Repo 接口和两个 commit。
func newTagRepo(t *testing.T) (fake *fakegithub.Server, f Repo, c1, c2 string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	c1 = gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "tag", "v0.1.0")
	gitRun(t, dir, "tag", "-a", "-m", "annotated", "v0.1.1")
	gitRun(t, dir, "tag", "nightly")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "two")
	c2 = gitRun(t, dir, "rev-parse", "HEAD")
	fake = fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: dir, Private: true}
	fake.AddRepo(repo)
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	return fake, GitHub(client, fake.InstallationID, repo.FullName()), c1, c2
}

// TestTags：按前缀列出，keep 过滤掉的不解开（不发请求），annotated tag 解开到 commit。
func TestTags(t *testing.T) {
	t.Parallel()
	fake, f, c1, _ := newTagRepo(t)
	var asked []string
	tags, err := f.Tags(t.Context(), "v", func(name string) bool {
		asked = append(asked, name)
		return name != "v0.1.0"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != (Tag{Name: "v0.1.1", Commit: c1}) {
		t.Fatalf("Tags = %+v，想要 annotated tag v0.1.1 → %s", tags, c1)
	}
	if strings.Join(asked, ",") != "v0.1.0,v0.1.1" {
		t.Errorf("keep 只应看到前缀匹配的 tag：%v", asked)
	}
	// 被 keep 去掉的轻量 tag 不需要请求；annotated tag 解开需要一次 git/tags 请求
	peels := 0
	for _, r := range fake.Requests() {
		if strings.Contains(r, "/git/tags/") {
			peels++
		}
	}
	if peels != 1 {
		t.Errorf("解开 tag 的请求 %d 次，想要 1 次", peels)
	}
}

// TestCreateTag：创建轻量 tag；已经存在时返回 ErrTagExists，并且不移动已有的 tag（带条件的写操作，可以放心重试）；
// 按名字读取 tag 指向的 commit，不存在时返回空。
func TestCreateTag(t *testing.T) {
	t.Parallel()
	_, f, c1, c2 := newTagRepo(t)
	ctx := t.Context()
	if err := f.CreateTag(ctx, "v0.2.0", c2); err != nil {
		t.Fatal(err)
	}
	if got, err := f.TagCommit(ctx, "v0.2.0"); err != nil || got != c2 {
		t.Fatalf("TagCommit = %q, %v，想要 %s", got, err, c2)
	}
	if err := f.CreateTag(ctx, "v0.2.0", c1); !errors.Is(err, ErrTagExists) {
		t.Fatalf("重复创建：err = %v，想要 ErrTagExists", err)
	}
	if got, _ := f.TagCommit(ctx, "v0.2.0"); got != c2 {
		t.Fatalf("已有的 tag 被移动到了 %s", got)
	}
	if got, err := f.TagCommit(ctx, "v9.9.9"); err != nil || got != "" {
		t.Fatalf("不存在的 tag：%q, %v", got, err)
	}
	if got, err := f.TagCommit(ctx, "v0.1.1"); err != nil || got != c1 {
		t.Fatalf("annotated tag 应解开到 commit：%q, %v", got, err)
	}
}

// TestHideRefs：列表读到旧数据时看不到刚建的 tag，按名字读取仍然能读到（打 tag 的重试按名字确认，PLAN #29.4）。
func TestHideRefs(t *testing.T) {
	t.Parallel()
	fake, f, _, c2 := newTagRepo(t)
	ctx := t.Context()
	if err := f.CreateTag(ctx, "v0.2.0", c2); err != nil {
		t.Fatal(err)
	}
	fake.HideRefs("refs/tags/v0.2.0")
	tags, err := f.Tags(ctx, "v", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range tags {
		if tg.Name == "v0.2.0" {
			t.Fatal("隐藏的 tag 不应出现在列表中")
		}
	}
	if got, _ := f.TagCommit(ctx, "v0.2.0"); got != c2 {
		t.Fatalf("按名字读取不受影响：%q", got)
	}
}

// TestUncertain：只有结果不明的错误（响应丢失：5xx、网络错误、ctx 取消）算"可能已经生效"；平台明确的回答
// （4xx、ErrHeadChanged、ErrTagExists、RejectedError、GraphQL 错误）和限流都不算（请求没有生效，可以重试）。
func TestUncertain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "没有错误", err: nil, want: false},
		{name: "422", err: &gh.APIError{Status: http.StatusUnprocessableEntity}, want: false},
		{name: "404", err: &gh.APIError{Status: http.StatusNotFound}, want: false},
		{name: "拒绝", err: &RejectedError{Message: "no"}, want: false},
		{name: "head 已变", err: ErrHeadChanged, want: false},
		{name: "tag 已存在", err: ErrTagExists, want: false},
		{name: "GraphQL 错误", err: &gh.GraphQLError{Type: gh.GraphQLForbidden}, want: false},
		{name: "限流", err: &gh.RateLimitError{Until: time.Now().Add(time.Minute)}, want: false},
		{name: "502", err: &gh.APIError{Status: http.StatusBadGateway}, want: true},
		{name: "500", err: &gh.APIError{Status: http.StatusInternalServerError}, want: true},
		{name: "网络错误", err: errors.New("connection reset by peer"), want: true},
		{name: "ctx 取消", err: context.Canceled, want: true},
	} {
		if got := Uncertain(tc.err); got != tc.want {
			t.Errorf("%s：Uncertain = %v，想要 %v", tc.name, got, tc.want)
		}
	}
}

// TestIsAuthError：仓库级的鉴权/权限失败（401、非限流的 403）要中止整轮；单个 PR 的 422/404、限流和其他错误不算。
func TestIsAuthError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "没有错误", err: nil, want: false},
		{name: "401", err: &gh.APIError{Status: http.StatusUnauthorized}, want: true},
		{name: "403", err: &gh.APIError{Status: http.StatusForbidden}, want: true},
		{name: "422", err: &gh.APIError{Status: http.StatusUnprocessableEntity}, want: false},
		{name: "404", err: &gh.APIError{Status: http.StatusNotFound}, want: false},
		{name: "500", err: &gh.APIError{Status: http.StatusInternalServerError}, want: false},
		{name: "限流 403", err: &gh.RateLimitError{Until: time.Now().Add(time.Minute), API: &gh.APIError{Status: http.StatusForbidden}}, want: false},
		{name: "拒绝", err: &RejectedError{Message: "no"}, want: false},
		{name: "网络错误", err: errors.New("connection reset by peer"), want: false},
	} {
		if got := IsAuthError(tc.err); got != tc.want {
			t.Errorf("%s：IsAuthError = %v，想要 %v", tc.name, got, tc.want)
		}
	}
}

// TestRejectForbiddenKeepsCause（审计）：rejectForbidden 把 403 转成 *RejectedError 时保留原始错误，
// IsAuthError 仍然认得出它是仓库级鉴权失败——Comment 返回的非限流 403 必须中止整轮，而不是当成单个 PR 的拒绝。
func TestRejectForbiddenKeepsCause(t *testing.T) {
	t.Parallel()
	forbidden := rejectForbidden(&gh.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"})
	if !IsRejected(forbidden) {
		t.Fatalf("403 应转成 RejectedError：%v", forbidden)
	}
	if !IsAuthError(forbidden) {
		t.Fatalf("403 的 RejectedError 应被 IsAuthError 认出：%v", forbidden)
	}
	// 404/410 只是单个对象的拒绝，不是仓库级鉴权失败
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		if err := rejectForbidden(&gh.APIError{Status: status}); !IsRejected(err) || IsAuthError(err) {
			t.Errorf("%d 的 RejectedError 不应算鉴权失败：%v", status, err)
		}
	}
}

// TestLostResponse：注入"写操作生效但响应丢失"后，评论已经写上，而客户端看到的是结果不明的错误。
func TestLostResponse(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
	fake.AddRepo(repo)
	fake.SetIssue("alice/demo", fakegithub.Issue{Number: 3})
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	f := GitHub(client, fake.InstallationID, repo.FullName())
	fake.SetFault("POST /repos/alice/demo/issues/3/comments", fakegithub.Fault{Lost: true})
	err := f.Comment(t.Context(), 3, "hello")
	if !Uncertain(err) {
		t.Fatalf("err = %v，想要结果不明", err)
	}
	if is, _ := fake.GetIssue("alice/demo", 3); len(is.Comments) != 1 {
		t.Fatalf("评论应已生效：%v", is.Comments)
	}
	// 故障只生效一次
	if err := f.Comment(t.Context(), 3, "again"); err != nil {
		t.Fatal(err)
	}
	if open, err := f.IssueOpen(t.Context(), 3); err != nil || !open {
		t.Fatalf("IssueOpen = %v, %v", open, err)
	}
	if _, err := f.IssueOpen(t.Context(), 99); !IsRejected(err) {
		t.Fatalf("不存在的 issue：err = %v，想要 RejectedError", err)
	}
}

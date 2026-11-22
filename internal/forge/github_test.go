package forge

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// TestUpdateBranchErrors：update-branch 的错误分类——期望的 head 不符；合并冲突单独区分（ErrMergeConflict，不算
// IsRejected、也不算结果不明）；其他明确拒绝（其他 422、没有权限）出队；限流和其他错误是暂时失败。
func TestUpdateBranchErrors(t *testing.T) {
	t.Parallel()
	const head = "aaaa"
	tests := []struct {
		name string
		err  *fakegithub.APIErr
		// limit 非 nil 时对 update-branch 限流（err 不生效）
		limit    *fakegithub.RateLimit
		want     error
		rejected string
	}{
		{name: "期望的 head 不符", err: &fakegithub.APIErr{Status: http.StatusUnprocessableEntity, Message: "expected head sha didn't match current head ref."}, want: ErrHeadChanged},
		{name: "冲突", err: &fakegithub.APIErr{Status: http.StatusUnprocessableEntity, Message: "merge conflict between base and head"}, want: ErrMergeConflict},
		{name: "其他 422", err: &fakegithub.APIErr{Status: http.StatusUnprocessableEntity, Message: "Validation Failed"}, rejected: "Validation Failed"},
		{name: "没有权限", err: &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}, rejected: "not accessible"},
		{name: "服务端错误", err: &fakegithub.APIErr{Status: http.StatusBadGateway, Message: "boom"}},
		{name: "primary 限流（403）", limit: &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true}},
		{name: "secondary 限流（403）", limit: &fakegithub.RateLimit{Status: http.StatusForbidden}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := fakegithub.New("alice")
			t.Cleanup(fake.Close)
			repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
			fake.AddRepo(repo)
			fake.SetPull(repo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: head, UpdateErr: tt.err})
			if tt.limit != nil {
				tt.limit.Until = time.Now().Add(time.Hour)
				tt.limit.Match = func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/update-branch") }
				fake.SetRateLimit(fake.InstallationID, tt.limit)
			}
			client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
			err := GitHub(client, fake.InstallationID, repo.FullName()).UpdateBranch(t.Context(), 7, head)
			switch {
			case tt.want != nil:
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v，想要 %v", err, tt.want)
				}
				if errors.Is(err, ErrMergeConflict) && (IsRejected(err) || Uncertain(err)) {
					t.Fatalf("冲突不能算 IsRejected（%v）或结果不明（%v）", IsRejected(err), Uncertain(err))
				}
			case tt.rejected != "":
				if re, ok := errors.AsType[*RejectedError](err); !ok || !strings.Contains(re.Message, tt.rejected) {
					t.Fatalf("err = %v，想要包含 %q 的 RejectedError", err, tt.rejected)
				}
			default:
				if err == nil || IsRejected(err) || errors.Is(err, ErrHeadChanged) {
					t.Fatalf("err = %v，想要暂时失败", err)
				}
			}
		})
	}
}

// TestMergeErrors：合并的错误分类——head 已变（409）下一轮重新判断；目标分支刚好前进（405 Base branch was modified）、
// 限流和服务端错误是暂时失败；其他拒绝（405、422、403）出队。
func TestMergeErrors(t *testing.T) {
	t.Parallel()
	const head = "aaaa"
	tests := []struct {
		name     string
		err      *fakegithub.APIErr
		limit    *fakegithub.RateLimit
		want     error
		rejected string
	}{
		{name: "head 已变", err: &fakegithub.APIErr{Status: http.StatusConflict, Message: "Head branch was modified. Review and try the merge again."}, want: ErrHeadChanged},
		{name: "目标分支前进", err: &fakegithub.APIErr{Status: http.StatusMethodNotAllowed, Message: "Base branch was modified. Review and try the merge again."}},
		{name: "不能合并", err: &fakegithub.APIErr{Status: http.StatusMethodNotAllowed, Message: "Pull Request is not mergeable"}, rejected: "not mergeable"},
		{name: "方式不允许", err: &fakegithub.APIErr{Status: http.StatusUnprocessableEntity, Message: "Merge commits are not allowed on this repository."}, rejected: "not allowed"},
		{name: "没有权限", err: &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}, rejected: "not accessible"},
		{name: "服务端错误", err: &fakegithub.APIErr{Status: http.StatusBadGateway, Message: "boom"}},
		{name: "primary 限流（403）", limit: &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true}},
		{name: "secondary 限流（403）", limit: &fakegithub.RateLimit{Status: http.StatusForbidden}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := fakegithub.New("alice")
			t.Cleanup(fake.Close)
			repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
			fake.AddRepo(repo)
			fake.SetPull(repo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: head, MergeErr: tt.err})
			if tt.limit != nil {
				tt.limit.Until = time.Now().Add(time.Hour)
				tt.limit.Match = func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/merge") }
				fake.SetRateLimit(fake.InstallationID, tt.limit)
			}
			client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
			_, err := GitHub(client, fake.InstallationID, repo.FullName()).Merge(t.Context(), 7, "squash", head)
			switch {
			case tt.want != nil:
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v，想要 %v", err, tt.want)
				}
				if errors.Is(err, ErrMergeConflict) && (IsRejected(err) || Uncertain(err)) {
					t.Fatalf("冲突不能算 IsRejected（%v）或结果不明（%v）", IsRejected(err), Uncertain(err))
				}
			case tt.rejected != "":
				if re, ok := errors.AsType[*RejectedError](err); !ok || !strings.Contains(re.Message, tt.rejected) {
					t.Fatalf("err = %v，想要包含 %q 的 RejectedError", err, tt.rejected)
				}
			default:
				if err == nil || IsRejected(err) {
					t.Fatalf("err = %v，想要暂时失败", err)
				}
			}
		})
	}
}

// TestGetPullFork：head 来自另一个仓库时 PR 算 fork（合并队列据此出队）。
func TestGetPullFork(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
	fake.AddRepo(repo)
	fake.SetPull(repo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: "aaaa"})
	fake.SetPull(repo, fakegithub.Pull{Number: 8, HeadRef: "feature", HeadSHA: "bbbb", ForkRepoID: 99})
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	f := GitHub(client, fake.InstallationID, repo.FullName())
	for n, want := range map[int]bool{7: false, 8: true} {
		p, err := f.GetPull(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		if p.Fork != want || p.State != PullOpen {
			t.Fatalf("PR #%d = %+v，想要 fork=%v", n, p, want)
		}
	}
}

// TestGetPullMergedByBot：合并者的账号类型按平台的 merged_by.type 映射。外部机器人的 login 不一定带
// [bot] 后缀，合并队列区分"自己合并"和"别人合并"时不能只看后缀。
func TestGetPullMergedByBot(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
	fake.AddRepo(repo)
	fake.SetPull(repo, fakegithub.Pull{
		Number: 7, HeadRef: "feature", HeadSHA: "aaaa",
		Merged: true, MergeCommitSHA: "sha7", MergedBy: "legacy-bot", MergedByBot: true,
	})
	fake.SetPull(repo, fakegithub.Pull{
		Number: 8, HeadRef: "feature", HeadSHA: "bbbb",
		Merged: true, MergeCommitSHA: "sha8", MergedBy: "legacy-bot",
	})
	fake.SetPull(repo, fakegithub.Pull{
		Number: 9, HeadRef: "feature", HeadSHA: "cccc",
		Merged: true, MergeCommitSHA: "sha9", MergedBy: "dependabot[bot]",
	})
	fake.SetPull(repo, fakegithub.Pull{Number: 10, HeadRef: "feature", HeadSHA: "dddd"})
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	f := GitHub(client, fake.InstallationID, repo.FullName())
	for n, want := range map[int]struct {
		mergedBy string
		bot      bool
	}{
		7:  {"legacy-bot", true},  // 类型 Bot、没有 [bot] 后缀
		8:  {"legacy-bot", false}, // 类型 User
		9:  {"dependabot[bot]", true},
		10: {"", false}, // 没有合并
	} {
		p, err := f.GetPull(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		if p.MergedBy != want.mergedBy || p.MergedByBot != want.bot {
			t.Fatalf("PR #%d：merged_by=%q bot=%v，想要 %q %v", n, p.MergedBy, p.MergedByBot, want.mergedBy, want.bot)
		}
	}
}

// TestGetPullMergeability：读取 PR 时带上平台的 mergeability。fakegithub 默认按 git 实际计算（两个分支改同一行是 dirty），
// 也可以指定（未计算时 mergeable 为 null）；只有 mergeable=false 且 dirty 才算冲突。
func TestGetPullMergeability(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(content, msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("commit", "-q", "-am", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	git("checkout", "-q", "-b", "clean")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "clean")
	clean := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "dirty", "main")
	dirty := commit("feature\n", "feature")
	git("checkout", "-q", "main")
	commit("main\n", "main 改了同一行")

	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: dir, Private: true}
	fake.AddRepo(repo)
	fake.SetPull(repo, fakegithub.Pull{Number: 7, HeadRef: "clean", HeadSHA: clean})
	fake.SetPull(repo, fakegithub.Pull{Number: 8, HeadRef: "dirty", HeadSHA: dirty})
	fake.SetPull(repo, fakegithub.Pull{Number: 9, HeadRef: "dirty", HeadSHA: dirty, Mergeability: fakegithub.MergeabilityUnknown})
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	f := GitHub(client, fake.InstallationID, repo.FullName())
	for n, want := range map[int]struct {
		mergeable *bool
		state     string
		conflict  bool
	}{7: {new(true), "clean", false}, 8: {new(false), MergeableDirty, true}, 9: {nil, "unknown", false}} {
		p, err := f.GetPull(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		if (p.Mergeable == nil) != (want.mergeable == nil) || (p.Mergeable != nil && *p.Mergeable != *want.mergeable) ||
			p.MergeableState != want.state || p.Conflicted() != want.conflict {
			t.Fatalf("PR #%d：mergeable=%v state=%q conflicted=%v，想要 %v %q %v", n, p.Mergeable, p.MergeableState, p.Conflicted(),
				want.mergeable, want.state, want.conflict)
		}
	}
}

// TestClosingIssues：只返回同一仓库中关联的 issue（仓库名不区分大小写），带上状态；
// 没有 Issues 权限时（GraphQL FORBIDDEN）返回 RejectedError。
func TestClosingIssues(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
	fake.AddRepo(repo)
	fake.SetIssue("alice/demo", fakegithub.Issue{Number: 3, Comments: []string{"kiln"}, UserComments: []string{"someone"}})
	fake.SetIssue("alice/demo", fakegithub.Issue{Number: 4, State: "closed"})
	fake.SetPull(repo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: "aaaa", ClosingIssues: []fakegithub.IssueRef{
		{Number: 3}, {Repo: "Alice/Demo", Number: 4}, {Repo: "alice/other", Number: 5},
	}})
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	f := GitHub(client, fake.InstallationID, repo.FullName())
	got, err := f.ClosingIssues(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 3 || !got[0].Open || got[1].Number != 4 || got[1].Open {
		t.Fatalf("ClosingIssues = %+v", got)
	}

	fake.UpdatePull(repo, 7, func(p *fakegithub.Pull) { p.ClosingIssuesForbidden = true })
	if _, err := f.ClosingIssues(t.Context(), 7); !IsRejected(err) || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("err = %v，想要 RejectedError", err)
	}
}

// TestCloseIssueErrors：关闭 issue 时没有权限（非限流的 403）、issue 不存在是拒绝；服务端错误是暂时失败；
// 成功时 state_reason 为 completed。
func TestCloseIssueErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      *fakegithub.APIErr
		missing  bool
		rejected bool
	}{
		{name: "成功"},
		{name: "没有权限", err: &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}, rejected: true},
		{name: "不存在", missing: true, rejected: true},
		{name: "服务端错误", err: &fakegithub.APIErr{Status: http.StatusBadGateway, Message: "boom"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := fakegithub.New("alice")
			t.Cleanup(fake.Close)
			repo := &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: t.TempDir(), Private: true}
			fake.AddRepo(repo)
			if !tt.missing {
				fake.SetIssue("alice/demo", fakegithub.Issue{Number: 3, CloseErr: tt.err})
			}
			client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
			err := GitHub(client, fake.InstallationID, repo.FullName()).CloseIssue(t.Context(), 3)
			switch {
			case tt.rejected:
				if !IsRejected(err) {
					t.Fatalf("err = %v，想要 RejectedError", err)
				}
			case tt.err != nil:
				if err == nil || IsRejected(err) {
					t.Fatalf("err = %v，想要暂时失败", err)
				}
			default:
				is, _ := fake.GetIssue("alice/demo", 3)
				if err != nil || is.State != "closed" || is.StateReason != "completed" {
					t.Fatalf("err = %v，issue = %+v", err, is)
				}
			}
		})
	}
}

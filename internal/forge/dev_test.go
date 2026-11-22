package forge

import (
	"errors"
	"slices"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

// newDevRepo 建一个仓库：main 上一个 commit（base），分支 feature 在它之上多一个改了 a.txt 的 commit（c1），
// 分支 other 从 base 分叉出另一个 commit（c2）。
func newDevRepo(t *testing.T) (fake *fakegithub.Server, repo *fakegithub.Repo, f Repo, base, c1, c2 string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	base = gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "c1")
	c1 = gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "checkout", "-q", "-b", "other", base)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "c2")
	c2 = gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "checkout", "-q", "main")
	fake = fakegithub.New("alice")
	t.Cleanup(fake.Close)
	repo = &fakegithub.Repo{ID: 1, Owner: "alice", Name: "demo", Dir: dir, Private: true}
	fake.AddRepo(repo)
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	return fake, repo, GitHub(client, fake.InstallationID, repo.FullName()), base, c1, c2
}

// TestBranchOps：创建分支已存在不移动；快进只接受后代，分叉返回 ErrNotFastForward 且分支不变；删除不存在的分支不算错误。
func TestBranchOps(t *testing.T) {
	_, _, f, base, c1, c2 := newDevRepo(t)
	ctx := t.Context()
	if h, err := f.BranchHead(ctx, "kiln/dev-1"); err != nil || h != "" {
		t.Fatalf("不存在的分支 = %q, %v", h, err)
	}
	if err := f.CreateBranch(ctx, "kiln/dev-1", base); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateBranch(ctx, "kiln/dev-1", c1); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("重复创建 err = %v", err)
	}
	if h, _ := f.BranchHead(ctx, "kiln/dev-1"); h != base {
		t.Fatalf("重复创建移动了分支：%s", h)
	}
	if err := f.FastForward(ctx, "kiln/dev-1", c1); err != nil {
		t.Fatal(err)
	}
	// 已经是目标 commit：照样成功（幂等）
	if err := f.FastForward(ctx, "kiln/dev-1", c1); err != nil {
		t.Fatalf("重复快进 err = %v", err)
	}
	if err := f.FastForward(ctx, "kiln/dev-1", c2); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("分叉 err = %v", err)
	}
	if h, _ := f.BranchHead(ctx, "kiln/dev-1"); h != c1 {
		t.Fatalf("分叉时分支被移动：%s", h)
	}
	if err := f.DeleteBranch(ctx, "kiln/dev-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteBranch(ctx, "kiln/dev-1"); err != nil {
		t.Fatalf("删除不存在的分支 err = %v", err)
	}
	if h, _ := f.BranchHead(ctx, "kiln/dev-1"); h != "" {
		t.Fatalf("删除后仍然存在：%s", h)
	}
}

// TestDiff：多出的提交数、改动的文件和提交说明；没有多出的提交时不读文件列表。
func TestDiff(t *testing.T) {
	fake, _, f, base, c1, _ := newDevRepo(t)
	d, err := f.Diff(t.Context(), base, c1)
	if err != nil || d.Ahead != 1 || !d.FilesComplete || !d.MessagesComplete || !slices.Equal(d.Messages, []string{"c1"}) {
		t.Fatalf("Diff = %+v, %v", d, err)
	}
	before := len(fake.Requests())
	if d, err := f.Diff(t.Context(), c1, base); err != nil || d.Ahead != 0 || len(d.Files) != 0 || len(fake.Requests())-before != 1 {
		t.Fatalf("反向 Diff = %+v, %v（%d 个请求）", d, err, len(fake.Requests())-before)
	}
}

// TestPulls：创建 PR；同一个 head 再创建得到 ErrPullExists；按 head 找到它，关闭后可以为同一个 head 再建。
func TestPulls(t *testing.T) {
	fake, repo, f, _, _, _ := newDevRepo(t)
	ctx := t.Context()
	n, err := f.CreatePull(ctx, "标题", "feature", "main", "描述")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreatePull(ctx, "标题", "feature", "main", "描述"); !errors.Is(err, ErrPullExists) {
		t.Fatalf("重复创建 err = %v", err)
	}
	list, err := f.PullsByHead(ctx, "feature")
	if err != nil || len(list) != 1 || list[0].Number != n || list[0].State != PullOpen {
		t.Fatalf("PullsByHead = %+v, %v", list, err)
	}
	if other, _ := f.PullsByHead(ctx, "other"); len(other) != 0 {
		t.Fatalf("其他分支的 PR：%+v", other)
	}
	if err := f.EditPullBody(ctx, n, "新的描述"); err != nil {
		t.Fatal(err)
	}
	if p, _ := fake.GetPull(repo, n); p.Body != "新的描述" {
		t.Fatalf("描述 = %q", p.Body)
	}
	fake.UpdatePull(repo, n, func(p *fakegithub.Pull) { p.State, p.Merged = "closed", true })
	if list, _ := f.PullsByHead(ctx, "feature"); len(list) != 1 || list[0].State != PullMerged {
		t.Fatalf("合并后 PullsByHead = %+v", list)
	}
	// 关闭后为同一个 head 再建：按编号升序返回（GitHub 默认倒序）
	n2, err := f.CreatePull(ctx, "标题", "feature", "main", "描述")
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := f.PullsByHead(ctx, "feature"); len(list) != 2 || list[0].Number != n || list[1].Number != n2 || n2 <= n {
		t.Fatalf("两个 PR 的 PullsByHead = %+v（%d、%d）", list, n, n2)
	}
	if _, err := f.CreatePull(ctx, "标题", "main", "main", ""); !IsRejected(err) {
		t.Fatalf("没有差异 err = %v", err)
	}
}

// TestIssues：按 label 列出打开的 Issue（不含 PR、不含关闭的），读取 Issue；按编号读到 PR 时被拒。
func TestIssues(t *testing.T) {
	fake, repo, f, _, _, _ := newDevRepo(t)
	fake.SetIssue(repo.FullName(), fakegithub.Issue{Number: 3, Title: "三", Body: "正文", Labels: []string{"kiln:ready"}})
	fake.SetIssue(repo.FullName(), fakegithub.Issue{Number: 1, Title: "一", Labels: []string{"kiln:ready", "bug"}})
	fake.SetIssue(repo.FullName(), fakegithub.Issue{Number: 2, Title: "二", Labels: []string{"bug"}})
	fake.SetIssue(repo.FullName(), fakegithub.Issue{Number: 4, Title: "四", State: "closed", Labels: []string{"kiln:ready"}})
	fake.SetPull(repo, fakegithub.Pull{Number: 5, HeadRef: "feature", Labels: []string{"kiln:ready"}})
	list, complete, err := f.ListIssues(t.Context(), "kiln:ready", 500)
	if err != nil || !complete {
		t.Fatal(complete, err)
	}
	var nums []int
	for _, is := range list {
		nums = append(nums, is.Number)
	}
	if !slices.Equal(nums, []int{1, 3}) {
		t.Fatalf("ListIssues = %v", nums)
	}
	// 逐页读取：超过一页（100 条）的也能读到；读满上限时 complete 为假
	for n := 10; n < 10+150; n++ {
		fake.SetIssue(repo.FullName(), fakegithub.Issue{Number: n, Title: "批量", Labels: []string{"kiln:ready"}})
	}
	if list, complete, err := f.ListIssues(t.Context(), "kiln:ready", 500); err != nil || !complete || len(list) != 152 || list[151].Number != 159 {
		t.Fatalf("两页：%d 个，complete=%v，%v", len(list), complete, err)
	}
	if list, complete, _ := f.ListIssues(t.Context(), "kiln:ready", 100); complete || len(list) != 99 {
		t.Fatalf("上限 100（含 PR）：%d 个，complete=%v", len(list), complete)
	}
	is, err := f.GetIssue(t.Context(), 3)
	if err != nil || is.Title != "三" || is.Body != "正文" || !is.Open || !is.HasLabel("kiln:ready") {
		t.Fatalf("GetIssue = %+v, %v", is, err)
	}
	if _, err := f.GetIssue(t.Context(), 5); !IsRejected(err) {
		t.Fatalf("PR err = %v", err)
	}
	if _, err := f.GetIssue(t.Context(), 999); !IsRejected(err) {
		t.Fatalf("不存在 err = %v", err)
	}
}

// TestCreateIssue：创建 issue 返回编号；读回标题、正文和 label；labels 能按 label 列出；再建时编号递增不重复。
func TestCreateIssue(t *testing.T) {
	_, _, f, _, _, _ := newDevRepo(t)
	ctx := t.Context()
	n, err := f.CreateIssue(ctx, "标题", "正文", []string{"kiln:doc-update"})
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("编号 = %d", n)
	}
	is, err := f.GetIssue(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "标题" || is.Body != "正文" || !is.HasLabel("kiln:doc-update") {
		t.Fatalf("GetIssue = %+v", is)
	}
	// 按 label 列出（走 ListIssues 路径，确认 labels 真的写进去了，而不是只在返回里）
	list, _, err := f.ListIssues(ctx, "kiln:doc-update", 500)
	if err != nil || len(list) != 1 || list[0].Number != n {
		t.Fatalf("ListIssues = %+v, %v", list, err)
	}
	// 再建一个：编号递增且不重复
	n2, err := f.CreateIssue(ctx, "标题2", "正文2", nil)
	if err != nil || n2 <= n {
		t.Fatalf("第二个 issue = %d, %v", n2, err)
	}
}

// TestGitHubWithToken：用给定 token 的 Repo 只以这个 token 的权限操作——contents: write 能建分支、不能建 PR；
// pull_requests: write 能建 PR、不能建分支；不签发、不使用共用客户端的 token。
func TestGitHubWithToken(t *testing.T) {
	fake, repo, _, base, c1, _ := newDevRepo(t)
	ctx := t.Context()
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	scoped := func(perm string) Repo {
		t.Helper()
		tok, err := client.CreateToken(ctx, fake.InstallationID, []int64{repo.ID}, map[string]string{perm: "write"})
		if err != nil {
			t.Fatal(err)
		}
		return GitHubWithToken(client, fake.InstallationID, tok.Token, repo.FullName())
	}
	contents, pulls := scoped("contents"), scoped("pull_requests")
	before := len(fake.IssuedTokens())
	if err := contents.CreateBranch(ctx, "kiln/dev-1", base); err != nil {
		t.Fatalf("contents: write 建分支：%v", err)
	}
	if err := contents.FastForward(ctx, "kiln/dev-1", c1); err != nil {
		t.Fatalf("contents: write 快进：%v", err)
	}
	if _, err := contents.CreatePull(ctx, "t", "kiln/dev-1", "main", "b"); !IsRejected(err) {
		t.Errorf("contents: write 建 PR 应被拒绝：%v", err)
	}
	n, err := pulls.CreatePull(ctx, "t", "kiln/dev-1", "main", "b")
	if err != nil {
		t.Fatalf("pull_requests: write 建 PR：%v", err)
	}
	if err := pulls.EditPullBody(ctx, n, "b2"); err != nil {
		t.Errorf("pull_requests: write 改描述：%v", err)
	}
	if err := pulls.AddLabel(ctx, n, "kiln:dev"); err != nil {
		t.Errorf("pull_requests: write 加 label：%v", err)
	}
	if err := pulls.CreateBranch(ctx, "kiln/dev-2", base); err == nil {
		t.Error("pull_requests: write 建分支应被拒绝")
	}
	if got := len(fake.IssuedTokens()); got != before {
		t.Errorf("用给定 token 的 Repo 不应签发 token：签发了 %d 枚", got-before)
	}
}

// TestGitHubWithToken_Empty：给定的 token 为空时，冷、热缓存下写操作都在发请求前失败，不改用共用客户端的 token。
func TestGitHubWithToken_Empty(t *testing.T) {
	fake, repo, shared, base, _, _ := newDevRepo(t)
	ctx := t.Context()
	client := &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL}
	empty := GitHubWithToken(client, fake.InstallationID, "", repo.FullName())
	check := func(stage string) {
		t.Helper()
		before := len(fake.Requests())
		if err := empty.CreateBranch(ctx, "kiln/dev-1", base); err == nil || !gh.NotApplied(err) {
			t.Errorf("%s：空 token 应在发请求前失败：%v", stage, err)
		}
		if got := len(fake.Requests()); got != before {
			t.Errorf("%s：空 token 发出了 %d 个请求", stage, got-before)
		}
	}
	check("冷缓存")
	// 热缓存：同一个 Client 的共用客户端已经缓存了完整权限的 token
	if _, err := GitHub(client, fake.InstallationID, repo.FullName()).BranchHead(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.BranchHead(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	check("热缓存")
	if h, _ := shared.BranchHead(ctx, "kiln/dev-1"); h != "" {
		t.Errorf("空 token 的写操作生效了：%s", h)
	}
}

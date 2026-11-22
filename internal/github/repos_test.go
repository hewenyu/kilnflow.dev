package github

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// App 级接口用 App JWT，installation 级接口用 installation token。
func TestAuthKindPerEndpoint(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	inst := map[string]any{"id": 7, "account": map[string]any{"id": 1, "login": "me", "type": "User"}}
	repo := map[string]any{"id": 3, "name": "r", "full_name": "o/r", "private": true, "default_branch": "main", "clone_url": "https://github.com/o/r.git", "owner": map[string]any{"login": "o"}}
	f.handle("GET /app/installations/7", jsonReply(http.StatusOK, inst))
	f.handle("GET /repos/o/r/installation", jsonReply(http.StatusOK, inst))
	f.handle("DELETE /app/installations/7", rawReply(http.StatusNoContent, ""))
	f.handle("GET /repos/o/r", jsonReply(http.StatusOK, repo))
	f.handle("GET /repositories/3", jsonReply(http.StatusOK, repo))

	ctx := t.Context()
	app := []func() error{
		func() error { _, err := c.GetInstallation(ctx, 7); return err },
		func() error { _, err := c.GetRepoInstallation(ctx, "o/r"); return err },
		func() error { return c.DeleteInstallation(ctx, 7) },
	}
	installation := []func() error{
		func() error { _, err := c.Inst(7).GetRepo(ctx, "o/r"); return err },
		func() error { _, err := c.Inst(7).GetRepoByID(ctx, 3); return err },
	}
	for _, call := range append(app, installation...) {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.requests()
	if len(reqs) != len(app)+len(installation) {
		t.Fatalf("请求数 %d", len(reqs))
	}
	for i, r := range reqs {
		auth := r.Header.Get("Authorization")
		if i < len(app) && !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("%s 应使用 App JWT：%q", r.key(), auth)
		}
		if i >= len(app) && auth != "token tok1" {
			t.Errorf("%s 应使用 installation token：%q", r.key(), auth)
		}
	}
}

func TestGetInstallation(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	suspended := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f.handle("GET /app/installations/7", jsonReply(http.StatusOK, map[string]any{
		"id": 7, "account": map[string]any{"id": 1, "login": "me", "type": "Organization"}, "suspended_at": suspended,
	}))
	got, err := c.GetInstallation(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Account != (Account{ID: 1, Login: "me", Type: "Organization"}) || got.SuspendedAt == nil || !got.SuspendedAt.Equal(suspended) {
		t.Errorf("installation = %+v", got)
	}
	// 已卸载的 installation 返回 404
	if _, err := c.GetInstallation(t.Context(), 8); !IsNotFound(err) {
		t.Errorf("应返回 404：%v", err)
	}
}

func TestListInstallations(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /app/installations", pagedReply(101, "", func(i int) any {
		return map[string]any{"id": i + 1, "account": map[string]any{"login": fmt.Sprintf("a%d", i)}}
	}))
	list, err := c.ListInstallations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 101 || list[100].Account.Login != "a100" {
		t.Errorf("返回 %d 个 installation", len(list))
	}
	for _, r := range f.requests() {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("列出 installation 应使用 App JWT")
		}
	}
}

// owner、name 分别转义。
func TestRepoPathEscaping(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o%20x/r%3Fy", jsonReply(http.StatusOK, map[string]any{"id": 1}))
	f.handle("GET /repos/o%20x/r%3Fy/installation", jsonReply(http.StatusOK, map[string]any{"id": 2}))
	if _, err := c.Inst(1).GetRepo(t.Context(), "o x/r?y"); err != nil {
		t.Errorf("GetRepo：%v", err)
	}
	if _, err := c.GetRepoInstallation(t.Context(), "o x/r?y"); err != nil {
		t.Errorf("GetRepoInstallation：%v", err)
	}
	// 不在任何 installation 中的仓库返回 404
	if _, err := c.GetRepoInstallation(t.Context(), "o/other"); !IsNotFound(err) {
		t.Errorf("应返回 404：%v", err)
	}
}

func TestGetRepo(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repositories/3", jsonReply(http.StatusOK, map[string]any{
		"id": 3, "name": "r", "full_name": "o/r", "private": true, "default_branch": "dev", "clone_url": "https://x/o/r.git",
		"owner": map[string]any{"id": 9, "login": "o", "type": "User"},
	}))
	got, err := c.Inst(1).GetRepoByID(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := Repository{ID: 3, Name: "r", FullName: "o/r", Owner: Account{ID: 9, Login: "o", Type: "User"}, Private: true, DefaultBranch: "dev", CloneURL: "https://x/o/r.git"}
	if *got != want {
		t.Errorf("repo = %+v", got)
	}
	if _, err := c.Inst(1).GetRepoByID(t.Context(), 4); !IsNotFound(err) {
		t.Errorf("不在授权范围内的仓库应返回 404：%v", err)
	}
}

// ListRepos 的数组包在 repositories 字段中。
func TestListRepos(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /installation/repositories", pagedReply(120, "repositories", func(i int) any {
		return map[string]any{"id": i + 1, "full_name": fmt.Sprintf("o/r%d", i)}
	}))
	repos, err := c.Inst(1).ListRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 120 || repos[119].FullName != "o/r119" {
		t.Errorf("返回 %d 个仓库", len(repos))
	}
	if got := strings.Join(pages(t, f.requests()), ","); got != "1,2" {
		t.Errorf("请求的页：%s", got)
	}
}

func TestGetFileContent(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/contents/.kiln/workflows/c%20i.yml", jsonReply(http.StatusOK, map[string]any{
		"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("on: push\n")) + "\n",
	}))
	got, err := c.Inst(1).GetFileContent(t.Context(), "o/r", ".kiln/workflows/c i.yml", "refs/heads/a&b")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "on: push\n" {
		t.Errorf("内容 = %q", got)
	}
	if ref := f.only(t).Query.Get("ref"); ref != "refs/heads/a&b" {
		t.Errorf("ref = %q", ref)
	}
	if _, err := c.Inst(1).GetFileContent(t.Context(), "o/r", "missing", "main"); !IsNotFound(err) {
		t.Errorf("不存在的文件应返回 404：%v", err)
	}
}

// 超过 1 MB 的文件 contents 接口返回 encoding "none" 和空内容：应报错，不能当成空文件。
func TestGetFileContentTooLarge(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/contents/CHANGELOG.md", jsonReply(http.StatusOK, map[string]any{
		"encoding": "none", "content": "", "size": 2 << 20,
	}))
	if got, err := c.Inst(1).GetFileContent(t.Context(), "o/r", "CHANGELOG.md", "main"); err == nil {
		t.Errorf("应报错，实际返回 %q", got)
	}
}

func TestListBranches(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/branches", jsonReply(http.StatusOK, []any{
		map[string]any{"name": "main", "commit": map[string]any{"sha": "a"}},
		map[string]any{"name": "dev", "commit": map[string]any{"sha": "b"}},
	}))
	bs, err := c.Inst(1).ListBranches(t.Context(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[1].Name != "dev" || bs[1].Commit.SHA != "b" {
		t.Errorf("branches = %+v", bs)
	}
}

func TestListOpenPulls(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/pulls", jsonReply(http.StatusOK, []any{map[string]any{
		"number": 4, "state": "open", "draft": true, "user": map[string]any{"login": "u"},
		"head": map[string]any{"ref": "feat", "sha": "h", "repo": map[string]any{"full_name": "o/r"}},
		"base": map[string]any{"ref": "main", "sha": "b", "repo": map[string]any{"full_name": "o/r"}},
	}}))
	prs, err := c.Inst(1).ListOpenPulls(t.Context(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("返回 %d 个 PR", len(prs))
	}
	p := prs[0]
	if p.Number != 4 || !p.Draft || p.Head.SHA != "h" || p.Head.Repo.FullName != "o/r" || p.Base.Ref != "main" || p.User.Login != "u" {
		t.Errorf("PR = %+v", p)
	}
}

// TestIsFork：只有 head 和 base 明确属于同一个仓库时才不是 fork；不知道仓库（fork 已删除、payload 不完整）时按 fork 丢弃。
func TestIsFork(t *testing.T) {
	ref := func(id int64) PRRef { return PRRef{Repo: Repository{ID: id}} }
	tests := []struct {
		name       string
		head, base PRRef
		want       bool
	}{
		{"同一个仓库", ref(1), ref(1), false},
		{"另一个仓库", ref(2), ref(1), true},
		{"fork 已删除（head.repo 为 null）", ref(0), ref(1), true},
		{"没有仓库信息", ref(0), ref(0), true},
		{"没有 base 仓库", ref(1), ref(0), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFork(tt.head, tt.base); got != tt.want {
				t.Fatalf("IsFork = %v，想要 %v", got, tt.want)
			}
		})
	}
}

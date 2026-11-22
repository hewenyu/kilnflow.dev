package fakegithub

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// newRepo 创建一个有两个 commit 和一个 annotated tag 的本地仓库。
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("a.txt", "a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "one")
	write("src/b.txt", "b\n")
	run("add", "-A")
	run("commit", "-q", "-m", "two")
	run("tag", "-a", "v1", "-m", "v1")
	return dir
}

// testKey 是测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// gitOut 在 dir 中执行 git 命令，返回去掉首尾空白的输出。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v：%v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// fixture 是一个模拟 GitHub、其中的仓库 tester/demo（内容来自 newRepo）和连接它的客户端。
type fixture struct {
	s    *Server
	c    *github.Client
	inst *github.Inst
	repo *Repo
	dir  string
}

const demo = "tester/demo"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := newRepo(t)
	s := New("tester")
	t.Cleanup(s.Close)
	r := &Repo{ID: 1, Owner: "tester", Name: "demo", Dir: dir, Private: true}
	s.AddRepo(r)
	c := &github.Client{AppID: s.AppID, Key: testKey(), APIURL: s.URL, UploadURL: s.URL}
	return &fixture{s: s, c: c, inst: c.Inst(s.InstallationID), repo: r, dir: dir}
}

func TestFakeWithClient(t *testing.T) {
	dir := newRepo(t)
	s := New("tester")
	t.Cleanup(s.Close)
	s.AddRepo(&Repo{ID: 1, Owner: "tester", Name: "demo", Dir: dir})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := &github.Client{AppID: s.AppID, Key: key, APIURL: s.URL, UploadURL: s.URL}
	inst := c.Inst(s.InstallationID)
	ctx := t.Context()
	const repo = "tester/demo"

	commit, obj, err := inst.ResolveTag(ctx, repo, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if commit == obj {
		t.Fatalf("annotated tag 应指向 tag 对象：%s %s", commit, obj)
	}
	tree, err := inst.GetTree(ctx, repo, commit, true)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range tree.Tree {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "a.txt,src,src/b.txt" {
		t.Fatalf("tree：%v", paths)
	}
	data, err := inst.GetFileContent(ctx, repo, "src/b.txt", commit)
	if err != nil || string(data) != "b\n" {
		t.Fatalf("文件内容：%q %v", data, err)
	}
	files, complete, err := inst.CompareFiles(ctx, repo, commit+"~1", commit)
	if err != nil || !complete || strings.Join(files, ",") != "src/b.txt" {
		t.Fatalf("compare：%v %v %v", files, complete, err)
	}
	if _, err := inst.GetRef(ctx, repo, "heads/ma"); !github.IsNotFound(err) {
		t.Fatalf("前缀不应匹配 ref：%v", err)
	}
	// 以 - 开头的 sha 不能被当成 git 选项
	if _, err := inst.GetBlob(ctx, repo, "--help"); !github.IsNotFound(err) {
		t.Fatalf("应返回 404：%v", err)
	}

	cr, err := inst.CreateCheckRun(ctx, repo, &github.CheckRun{Name: "ci", HeadSHA: commit, ExternalID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.UpdateCheckRun(ctx, repo, cr.ID, &github.CheckRun{Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	found, err := inst.FindCheckRun(ctx, repo, commit, "ci", "x", s.AppID)
	if err != nil || found == nil || found.ID != cr.ID {
		t.Fatalf("FindCheckRun：%+v %v", found, err)
	}
	if got := s.CheckRuns(commit); len(got) != 1 || got[0].Status != "completed" || got[0].UpdateCount != 1 {
		t.Fatalf("Check Run：%+v", got)
	}

	draft := true
	rel, err := inst.CreateRelease(ctx, repo, &github.Release{TagName: "v1", Draft: &draft})
	if err != nil {
		t.Fatal(err)
	}
	a, err := inst.UploadReleaseAsset(ctx, repo, rel.ID, "x.bin", strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AssetData(rel.ID, "x.bin"); string(got) != "hello" {
		t.Fatalf("asset：%q", got)
	}
	if err := inst.DeleteReleaseAsset(ctx, repo, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := inst.DeleteReleaseAsset(ctx, repo, a.ID); !github.IsNotFound(err) {
		t.Fatalf("重复删除应返回 404：%v", err)
	}
	if rels := s.Releases(repo); len(rels) != 1 || len(rels[0].Assets) != 0 {
		t.Fatalf("Release：%+v", rels)
	}
	if u := s.UnhandledRequests(); len(u) != 0 {
		t.Fatalf("没有处理的请求：%v", u)
	}
}

func TestBadRequests(t *testing.T) {
	s := New("tester")
	t.Cleanup(s.Close)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/repos/o/r/check-runs", "{", http.StatusBadRequest},
		{http.MethodPatch, "/repos/o/r/check-runs/x", "{}", http.StatusBadRequest},
		{http.MethodPatch, "/repos/o/r/releases/1", "not json", http.StatusBadRequest},
		{http.MethodGet, "/nope", "", http.StatusNotFound},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), c.method, s.URL+c.path, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("HTTP %d，应为 %d", resp.StatusCode, c.want)
			}
		})
	}
	if u := s.UnhandledRequests(); len(u) != 1 || u[0] != "GET /nope" {
		t.Fatalf("UnhandledRequests：%v", u)
	}
}

// 签发的 token 记下仓库范围和权限；DELETE /installation/token 吊销请求所用的 token，再吊销一次时是 401（RevokeToken 当作成功）。
func TestTokenScopeAndRevoke(t *testing.T) {
	s := New("tester")
	t.Cleanup(s.Close)
	c := &github.Client{AppID: s.AppID, Key: testKey(), APIURL: s.URL}
	tok, err := c.CreateToken(t.Context(), s.InstallationID, []int64{1, 2}, map[string]string{"contents": "read"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.IssuedToken(tok.Token)
	if !ok || got.Installation != s.InstallationID || len(got.RepoIDs) != 2 || got.RepoIDs[1] != 2 || got.Permissions["contents"] != "read" || got.Revoked {
		t.Fatalf("签发的 token = %+v（%v）", got, ok)
	}
	if err := c.RevokeToken(t.Context(), s.InstallationID, tok.Token); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.IssuedToken(tok.Token); !got.Revoked {
		t.Error("吊销之后 Revoked 应为 true")
	}
	if err := c.RevokeToken(t.Context(), s.InstallationID, tok.Token); err != nil {
		t.Errorf("再吊销一次：%v", err)
	}
	if n := strings.Count(strings.Join(s.Requests(), "\n"), "DELETE /installation/token"); n != 2 {
		t.Errorf("吊销请求 %d 次，期望 2 次", n)
	}
}

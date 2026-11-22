package runner

import (
	"bytes"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
)

// TestMain 让测试程序同时充当 kiln-runner：git exec 镜像中的远程助手（按 argv[0]）和 credential helper（git-credential
// 子命令）都指向测试程序本身（newTestRunner 的 exe），与 cmd/kiln-runner 的 main 一样经 GitCommand 分派。
func TestMain(m *testing.M) {
	if code, ok := GitCommand(os.Args, os.Stdin, os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	code := m.Run()
	removeStaticRunner()
	os.Exit(code)
}

// testExe 返回测试程序的真实路径（Runner.exe）。
func testExe(t *testing.T) string {
	t.Helper()
	p, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testWait 是等待事件（签发、吊销、请求到达）的时间上限：只防止测试卡死。
const testWait = 2 * time.Minute

// waitFor 等 ch 收到一个值。
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("等待%s超时", what)
	}
	var zero T
	return zero
}

// gitHTTP 是本地的 git HTTP 服务（git http-backend）：每个仓库都是私有的，请求要带 fakeServer 签发给这个仓库、还没有
// 吊销的 token（Basic 认证的密码，与 GitHub 的 installation token 相同）。
type gitHTTP struct {
	srv  *httptest.Server
	root string
	fs   *fakeServer

	mu sync.Mutex
	// used 是通过认证的 token。
	used []string
	// hold 不为 nil 时在每个带认证的请求校验 token 之前调用（参数是请求和仓库 owner/name）：测试据此让传输停在中途。
	hold func(r *http.Request, repo string)
}

// newGitHTTP 在 addr（为空时是 127.0.0.1:0）上启动 git HTTP 服务。
func newGitHTTP(t *testing.T, fs *fakeServer, addr string) *gitHTTP {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("需要 git")
	}
	g := &gitHTTP{root: t.TempDir(), fs: fs}
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + g.root, "GIT_HTTP_EXPORT_ALL=1"}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
		repo := ""
		if len(parts) >= 2 {
			repo = repoOfPath(parts[0] + "/" + parts[1])
		}
		_, tok, ok := r.BasicAuth()
		if ok {
			g.mu.Lock()
			hold := g.hold
			g.mu.Unlock()
			if hold != nil {
				hold(r, repo)
			}
		}
		if !ok || !fs.gitTokenValid(tok, repo) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		g.used = append(g.used, tok)
		g.mu.Unlock()
		backend.ServeHTTP(w, r)
	})
	g.srv = httptest.NewUnstartedServer(h)
	if addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = g.srv.Listener.Close() // 换成 addr 上的监听
		g.srv.Listener = ln
	}
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

// url 返回仓库 repo（owner/name）的地址。
func (g *gitHTTP) url(repo string) string { return g.srv.URL + "/" + repo + ".git" }

// setHold 设置 hold。
func (g *gitHTTP) setHold(f func(r *http.Request, repo string)) {
	g.mu.Lock()
	g.hold = f
	g.mu.Unlock()
}

// usedTokens 返回通过认证的 token。
func (g *gitHTTP) usedTokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.used...)
}

// addRepo 创建裸仓库 repo（一个提交，files 是路径 → 内容），返回提交的 sha。
func (g *gitHTTP) addRepo(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "", "init", "-q", "-b", "main")
	sha := commitFiles(t, work, files)
	bare := filepath.Join(g.root, filepath.FromSlash(repo)+".git")
	runGit(t, work, "", "clone", "-q", "--bare", work, bare)
	// Runner 按 sha 拉取（fetchSource）
	runGit(t, bare, "", "config", "uploadpack.allowAnySHA1InWant", "true")
	return sha
}

// gitSource 返回 gitHTTP 上仓库 repo 的提交 sha 的源码（RepoId 用于 bare 镜像的目录名）。
func (g *gitHTTP) source(repoID int64, repo, sha string) *runnerv1.Source {
	return &runnerv1.Source{RepoId: repoID, Repository: repo, CloneUrl: g.url(repo), Sha: sha}
}

// scanTokens 返回 tokens 中出现在 data 中的。
func scanTokens(data []byte, tokens []string) []string {
	var found []string
	for _, tok := range tokens {
		if bytes.Contains(data, []byte(tok)) {
			found = append(found, tok)
		}
	}
	return found
}

// scanTreeForTokens 扫描 dir 下全部普通文件的内容（不跟随符号链接），返回出现 token 的文件。
func scanTreeForTokens(t *testing.T, dir string, tokens []string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if found := scanTokens(b, tokens); len(found) > 0 {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// issuedTokens 返回 fakeServer 签发过的全部 token。
func issuedTokens(fs *fakeServer) []string {
	var out []string
	for _, s := range fs.gitOpStates() {
		out = append(out, s.token)
	}
	return out
}

// opsFor 返回签发给仓库 repo 的操作。
func opsFor(fs *fakeServer, repo string) []fakeGitOpState {
	var out []fakeGitOpState
	for _, s := range fs.gitOpStates() {
		if strings.EqualFold(s.repo, repo) {
			out = append(out, s)
		}
	}
	return out
}

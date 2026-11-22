package runner

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// channelFixture 是凭据通道的模块测试环境：fakeServer、gitHTTP（o/r、o/dep、o/other）、一个声明了 o/dep 的 job，以及
// 按 host executor 的方式准备好的 git 环境（git exec 镜像、helper）。
type channelFixture struct {
	fs      *fakeServer
	g       *gitHTTP
	ch      *gitChannel
	env     []string
	revoked chan *runnerv1.RevokeGitTokenRequest
}

func newChannelFixture(t *testing.T) *channelFixture {
	t.Helper()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	g.addRepo(t, "o/dep", map[string]string{"dep.txt": "dep\n"})
	r := newTestRunner(t, fs)
	spec := hostJob(1)
	spec.Source = g.source(1, "o/r", sha)
	spec.Repositories = []string{"o/dep"}
	fs.register(spec)
	f := &channelFixture{fs: fs, g: g, revoked: make(chan *runnerv1.RevokeGitTokenRequest, 16)}
	fs.mu.Lock()
	fs.revokeHook = func(m *runnerv1.RevokeGitTokenRequest) { f.revoked <- m }
	fs.mu.Unlock()
	j := r.newJob(spec)
	j.log = runnerkit.NewLogShipper(j.appendLog)
	t.Cleanup(j.log.Close)
	scope := newGitScope(spec)
	ch, err := openGitChannel(t.Context(), "", scope, j, j.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ch.close)
	f.ch = ch
	envMap := map[string]string{"PATH": os.Getenv("PATH"), "HOME": t.TempDir(), "GIT_CONFIG_NOSYSTEM": "1"}
	hg := newHostGit(gitAccess{scope: scope.url(), helper: gitHelperCmd(r.exe), socket: ch.sock}, t.TempDir(), r.exe)
	a := hg.access(t.Context(), envMap, "", j.log)
	if a.execPath == "" {
		t.Fatal("没有建 git exec 镜像")
	}
	a.apply(envMap)
	f.env = envList(envMap)
	return f
}

// git 启动 git（不等待），输出写到返回的 buffer。
func (f *channelFixture) git(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = f.env
	cmd.Dir = t.TempDir()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &out
}

// waitRevoke 等操作 op 的吊销请求。
func (f *channelFixture) waitRevoke(t *testing.T, op string) *runnerv1.RevokeGitTokenRequest {
	t.Helper()
	for {
		if m := waitFor(t, f.revoked, "吊销 "+op); m.OpId == op {
			return m
		}
	}
}

// openOps 返回仍打开的操作。
func (c *gitChannel) openOps() []*gitOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*gitOp
	for _, o := range c.ops {
		out = append(out, o)
	}
	return out
}

// V6：传输包装正常退出后操作关闭、token 吊销。
func TestGitChannelWrapperExit(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	cmd, out := f.git(t, "ls-remote", f.g.url("o/dep"))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("git ls-remote：%v\n%s", err, out)
	}
	ops := opsFor(f.fs, "o/dep")
	if len(ops) != 1 {
		t.Fatalf("签发了 %d 个操作", len(ops))
	}
	if m := f.waitRevoke(t, ops[0].op); m.Token != ops[0].token {
		t.Error("吊销请求没有带收到的 token")
	}
	if len(f.ch.openOps()) != 0 {
		t.Error("传输结束后操作仍打开")
	}
}

// V6：传输包装被 SIGKILL 时连接由内核关闭，操作随之关闭、吊销；真实的远程助手之后的认证失败。
func TestGitChannelKilledWrapper(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	held := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	// 断言失败时也放行，免得 gitHTTP 的请求一直挂着
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.g.setHold(func(_ *http.Request, repo string) {
		if repo == "o/dep" {
			held <- struct{}{}
			<-release
		}
	})
	cmd, out := f.git(t, "ls-remote", f.g.url("o/dep"))
	waitFor(t, held, "带凭据的请求")
	ops := f.ch.openOps()
	if len(ops) != 1 || ops[0].pid == 0 {
		t.Fatalf("打开的操作：%+v", ops)
	}
	if err := syscall.Kill(ops[0].pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	f.waitRevoke(t, ops[0].id)
	releaseOnce.Do(func() { close(release) })
	if err := cmd.Wait(); err == nil {
		t.Fatalf("包装被杀、token 吊销之后传输不应成功：\n%s", out)
	}
}

// V6：并发的两次传输各签各的 token，先结束的只吊销自己的：o/r 的带凭据请求等 o/dep 的操作吊销之后才校验 token。
func TestGitChannelConcurrentTransfers(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	depRevoked := make(chan struct{})
	var once sync.Once
	f.fs.mu.Lock()
	f.fs.revokeHook = func(m *runnerv1.RevokeGitTokenRequest) {
		for _, o := range opsFor(f.fs, "o/dep") {
			if o.op == m.OpId {
				once.Do(func() { close(depRevoked) })
			}
		}
	}
	f.fs.mu.Unlock()
	f.g.setHold(func(_ *http.Request, repo string) {
		if repo == "o/r" {
			waitFor(t, depRevoked, "o/dep 的吊销")
		}
	})
	self, selfOut := f.git(t, "ls-remote", f.g.url("o/r"))
	dep, depOut := f.git(t, "ls-remote", f.g.url("o/dep"))
	if err := dep.Wait(); err != nil {
		t.Fatalf("o/dep：%v\n%s", err, depOut)
	}
	if err := self.Wait(); err != nil {
		t.Fatalf("o/dep 的传输结束后 o/r 的传输失败（token 被别的操作吊销？）：%v\n%s", err, selfOut)
	}
	a, b := opsFor(f.fs, "o/r"), opsFor(f.fs, "o/dep")
	if len(a) != 1 || len(b) != 1 || a[0].token == b[0].token {
		t.Errorf("两次传输应各签一枚：o/r %+v，o/dep %+v", a, b)
	}
}

// helper 只回答仍打开、协议 / 主机 / 路径与批准的仓库一致的请求；不一致或没有操作时不签发。
func TestGitChannelCredentialChecks(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	conn, err := net.Dial("unix", f.ch.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if err := writeGitMsg(conn, gitMsg{Cmd: gitCmdOpen, URL: f.g.url("o/dep")}); err != nil {
		t.Fatal(err)
	}
	var open gitReply
	if err := readGitMsg(br, &open); err != nil || open.Op == "" {
		t.Fatalf("open = %+v，%v", open, err)
	}
	host := strings.TrimPrefix(f.g.srv.URL, "http://")
	get := func(m gitMsg) gitReply {
		m.Cmd = gitCmdGet
		return f.ch.credential(m)
	}
	for name, m := range map[string]gitMsg{
		"路径是另一个仓库": {Op: open.Op, Protocol: "http", Host: host, Path: "o/r.git"},
		"主机不同":     {Op: open.Op, Protocol: "http", Host: "example.com", Path: "o/dep.git"},
		"协议不同":     {Op: open.Op, Protocol: "https", Host: host, Path: "o/dep.git"},
		"没有这个操作":   {Op: "nope", Protocol: "http", Host: host, Path: "o/dep.git"},
	} {
		if r := get(m); r.Password != "" || r.Error == "" {
			t.Errorf("%s：不应给凭据：%+v", name, r)
		}
	}
	if n := len(f.fs.issueRequests()); n != 0 {
		t.Fatalf("不一致的请求触发了 %d 次签发", n)
	}
	r := get(gitMsg{Op: open.Op, Protocol: "http", Host: host, Path: "/O/Dep.git"})
	if r.Password == "" || r.Username != gitTokenUser {
		t.Fatalf("一致的请求应给凭据：%+v", r)
	}
	if again := get(gitMsg{Op: open.Op, Protocol: "http", Host: host, Path: "o/dep"}); again.Password != r.Password || len(f.fs.issueRequests()) != 1 {
		t.Error("同一操作的第二次请求应返回同一枚 token、不再签发")
	}
	// 未声明的仓库不开操作
	c2, err := net.Dial("unix", f.ch.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := writeGitMsg(c2, gitMsg{Cmd: gitCmdOpen, URL: f.g.srv.URL + "/o/other.git"}); err != nil {
		t.Fatal(err)
	}
	var denied gitReply
	if err := readGitMsg(bufio.NewReader(c2), &denied); err != nil || denied.Op != "" || denied.Denied == "" {
		t.Errorf("未声明的仓库：%+v，%v", denied, err)
	}
}

// job 结束时关闭通道：吊销仍打开的操作（带收到的 token），不再接受新连接，删除 socket 目录。
func TestGitChannelClose(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	conn, err := net.Dial("unix", f.ch.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if err := writeGitMsg(conn, gitMsg{Cmd: gitCmdOpen, URL: f.g.url("o/r")}); err != nil {
		t.Fatal(err)
	}
	var open gitReply
	if err := readGitMsg(br, &open); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(f.g.srv.URL, "http://")
	cred := f.ch.credential(gitMsg{Op: open.Op, Protocol: "http", Host: host, Path: "o/r.git"})
	if cred.Password == "" {
		t.Fatalf("没有拿到凭据：%+v", cred)
	}
	f.ch.close()
	revokes := f.fs.revokeRequests()
	if !slices.ContainsFunc(revokes, func(m *runnerv1.RevokeGitTokenRequest) bool { return m.OpId == open.Op && m.Token == cred.Password }) {
		t.Errorf("关闭通道时没有吊销仍打开的操作：%v", revokes)
	}
	if _, err := os.Stat(f.ch.dir); !os.IsNotExist(err) {
		t.Errorf("socket 目录没有删除：%v", err)
	}
	if c, err := net.Dial("unix", f.ch.sock); err == nil {
		_ = c.Close()
		t.Error("关闭之后仍能连接")
	}
}

// socket 路径超过平台上限时明确报错。
func TestOpenGitChannelLongPath(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), strings.Repeat("d", gitSocketPathMax()))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Skipf("文件系统不支持这么长的目录名：%v", err)
	}
	_, err := openGitChannel(t.Context(), parent, gitScope{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("err = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(parent, gitSocketDirPattern+"*")); len(left) != 0 {
		t.Errorf("留下了目录：%v", left)
	}
}

// job 可以访问的仓库：任务仓库所在的协议和主机下的任务仓库和 repositories（不区分大小写）。
func TestGitScope(t *testing.T) {
	t.Parallel()
	s := newGitScope(&runnerv1.JobSpec{
		Source:       &runnerv1.Source{Repository: "Org/Repo", CloneUrl: "https://github.com/Org/Repo.git"},
		Repositories: []string{"org/dep"},
	})
	if s.url() != "https://github.com" {
		t.Errorf("url = %q", s.url())
	}
	for raw, want := range map[string]string{
		"https://github.com/org/repo":         "Org/Repo",
		"https://GitHub.com/ORG/REPO.git/":    "Org/Repo",
		"https://github.com/org/dep.git":      "org/dep",
		"https://github.com/org/other.git":    "",
		"http://github.com/org/repo.git":      "",
		"https://evil.example/org/repo.git":   "",
		"https://github.com/org/repo/sub.git": "",
	} {
		if got, reason := s.approve(raw); got != want || (want == "" && reason == "") {
			t.Errorf("approve(%s) = %q（%s），期望 %q", raw, got, reason, want)
		}
	}
	if !s.matches("HTTPS", "github.com", "org/dep.git", "org/dep") || s.matches("https", "github.com", "org/dep.git", "Org/Repo") {
		t.Error("matches 结果不对")
	}
	if d := newGitScope(&runnerv1.JobSpec{Repositories: []string{"o/x"}}); d.url() != defaultGitURL {
		t.Errorf("没有源码的 job：url = %q", d.url())
	}
}

// 环境变量配置追加在 workflow 自己的之后；askpass 去掉，不提示输入。
func TestGitAccessApply(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "wf",
		"GIT_ASKPASS": "/x", "SSH_ASKPASS": "/y", gitOpEnv: "stale",
	}
	a := gitAccess{scope: "https://github.com", helper: "!h", socket: "/s", execPath: "/m", realExecPath: "/r"}
	a.apply(env)
	want := map[string]string{
		"GIT_CONFIG_COUNT": "5", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "wf",
		"GIT_CONFIG_KEY_1": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_1": "",
		"GIT_CONFIG_KEY_2": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_2": "!h",
		"GIT_CONFIG_KEY_3": "credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_3": "true",
		"GIT_CONFIG_KEY_4": "core.askPass", "GIT_CONFIG_VALUE_4": "",
		"GIT_TERMINAL_PROMPT": "0", gitSocketEnv: "/s", "GIT_EXEC_PATH": "/m", gitRealExecEnv: "/r",
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("%s = %q，期望 %q", k, got, v)
		}
	}
	for _, k := range []string{"GIT_ASKPASS", "SSH_ASKPASS", gitOpEnv} {
		if _, ok := env[k]; ok {
			t.Errorf("%s 应去掉", k)
		}
	}
	// git 会拒绝的 GIT_CONFIG_COUNT：什么都不改（git 照样失败），不从 0 开始覆盖已有的项
	bad := map[string]string{"GIT_CONFIG_COUNT": "x", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "img"}
	appendGitConfig(bad, [][2]string{{"a.b", "c"}})
	if len(bad) != 3 || bad["GIT_CONFIG_COUNT"] != "x" || bad["GIT_CONFIG_KEY_0"] != "user.name" {
		t.Errorf("无效的 GIT_CONFIG_COUNT：%v", bad)
	}
}

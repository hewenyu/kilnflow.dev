package runner

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 签发的重试分类（③ 的错误码表）：只有 Unavailable 用同一个 op 重试，其余（包括没有收到响应的 DeadlineExceeded）只调用一次。
func TestIssueRetryClassification(t *testing.T) {
	t.Parallel()
	once := map[string]connect.Code{
		"Aborted": connect.CodeAborted, "Internal": connect.CodeInternal, "FailedPrecondition": connect.CodeFailedPrecondition,
		"PermissionDenied": connect.CodePermissionDenied, "InvalidArgument": connect.CodeInvalidArgument,
		"Unimplemented": connect.CodeUnimplemented, "DeadlineExceeded": connect.CodeDeadlineExceeded,
	}
	for name, code := range once {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fs, j := newTokenJob(t)
			fs.mu.Lock()
			fs.issueErrs = []error{connect.NewError(code, errors.New("注入"))}
			fs.mu.Unlock()
			if _, _, err := j.issueGitToken(t.Context(), "op-1", "o/r"); err == nil {
				t.Fatal("应失败")
			}
			if n := len(fs.issueRequests()); n != 1 {
				t.Errorf("%s 调用了 %d 次，期望 1 次（不重试）", name, n)
			}
		})
	}
	t.Run("Unavailable", func(t *testing.T) {
		t.Parallel()
		fs, j := newTokenJob(t)
		unavailable := connect.NewError(connect.CodeUnavailable, errors.New("GitHub 确定没有签发"))
		fs.mu.Lock()
		fs.issueErrs = []error{unavailable, unavailable}
		fs.mu.Unlock()
		tok, _, err := j.issueGitToken(t.Context(), "op-1", "o/r")
		if err != nil || tok == "" {
			t.Fatalf("重试后应签发：%v", err)
		}
		reqs := fs.issueRequests()
		if len(reqs) != 3 {
			t.Fatalf("调用了 %d 次，期望 3 次", len(reqs))
		}
		for _, r := range reqs {
			if r.OpId != "op-1" {
				t.Errorf("重试换了 op：%s", r.OpId)
			}
		}
	})
}

// 签发的回复没有到达时（真实的 runnerv1connect 客户端经 HTTP）：连接直接断开、网关回复普通的 502，Connect 客户端都合成
// Unavailable（不是 Server 发来的），操作的状态不明，不重试；只有 Server 明确返回的 Unavailable 用同一个 op 重试。
func TestIssueRetryTransport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		setup func(fs *fakeServer)
		// requests 是 IssueGitToken 的 HTTP 请求数；ok 表示最终签发成功
		requests int
		ok       bool
	}{
		{"连接直接断开", func(fs *fakeServer) { fs.issueLost = 1 }, 1, false},
		{"网关回复普通的 502", func(fs *fakeServer) { fs.issueBadGateway = 1 }, 1, false},
		{"Server 已签发、回复的正文无法解码", func(fs *fakeServer) { fs.issueGarbled = 1 }, 1, false},
		// 认证层：来源不明的 401 不重新认证、不重放；Kiln 明确的鉴权拒绝（处理函数没有执行）重新认证后重放一次
		{"Server 已签发、代理回复普通的 401", func(fs *fakeServer) { fs.issueProxy401 = 1 }, 1, false},
		{"Kiln 明确的鉴权拒绝", func(fs *fakeServer) { fs.token = "rotated" }, 2, true},
		{"Server 明确返回 Unavailable", func(fs *fakeServer) {
			fs.issueErrs = []error{connect.NewError(connect.CodeUnavailable, errors.New("GitHub 确定没有签发"))}
		}, 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs, j := newTokenJob(t)
			// 先认证一次（客户端缓存 token）：Kiln 明确的鉴权拒绝要拒绝的是缓存的 token
			if _, err := j.r.c.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			fs.mu.Lock()
			c.setup(fs)
			fs.mu.Unlock()
			tok, _, err := j.issueGitToken(t.Context(), "op-1", "o/r")
			if (err == nil) != c.ok || (c.ok && tok == "") {
				t.Fatalf("token %q，err = %v", tok, err)
			}
			if !c.ok && (connect.IsWireError(err) || gitOpSettled(err)) {
				t.Errorf("err = %v（%v）：应是客户端合成的错误，且要按原 op 关闭", err, connect.CodeOf(err))
			}
			fs.mu.Lock()
			n := fs.issueHTTP
			fs.mu.Unlock()
			if n != c.requests {
				t.Errorf("IssueGitToken 请求了 %d 次，期望 %d 次", n, c.requests)
			}
		})
	}
}

// 真实的旧 Server（以及网关、代理）对不认识的 RPC 回复普通的 404：Connect 客户端合成 Unimplemented（不是 wire error）。
// 错误照样提示升级 Server，但这不是 Server 对这次签发的结论，不能据此跳过关闭。
func TestIssueOldServerNotFound(t *testing.T) {
	t.Parallel()
	fs, j := newTokenJob(t)
	fs.mu.Lock()
	fs.legacyGit = true
	fs.mu.Unlock()
	_, _, err := j.issueGitToken(t.Context(), "op-1", "o/r")
	if !errors.Is(err, errServerTooOld) || connect.CodeOf(err) != connect.CodeUnimplemented || connect.IsWireError(err) {
		t.Fatalf("err = %v", err)
	}
	if gitOpSettled(err) {
		t.Error("本地合成的 Unimplemented 不能跳过关闭")
	}
	if n := len(fs.issueRequests()); n != 1 {
		t.Errorf("签发了 %d 次，期望 1 次", n)
	}
	// Server 明确发来的 Unimplemented（wire error）才算已有终态
	if !gitOpSettled(connect.NewWireError(connect.CodeUnimplemented, errors.New("x"))) {
		t.Error("Server 发来的 Unimplemented 应算已有终态")
	}
}

// newTokenJob 返回登记在 fakeServer 上的 job（任务仓库 o/r，依赖 o/dep），日志随测试关闭。
func newTokenJob(t *testing.T) (*fakeServer, *job) {
	t.Helper()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	spec := hostJob(1)
	spec.Source = &runnerv1.Source{RepoId: 1, Repository: "o/r", CloneUrl: "http://127.0.0.1:1/o/r.git"}
	spec.Repositories = []string{"o/dep"}
	fs.register(spec)
	j := r.newJob(spec)
	j.log = runnerkit.NewLogShipper(j.appendLog)
	t.Cleanup(j.log.Close)
	return fs, j
}

// revokesOf 返回操作 op 的关闭请求。
func revokesOf(fs *fakeServer, op string) []*runnerv1.RevokeGitTokenRequest {
	var out []*runnerv1.RevokeGitTokenRequest
	for _, m := range fs.revokeRequests() {
		if m.OpId == op {
			out = append(out, m)
		}
	}
	return out
}

// M1（源码）：签发的响应没收到（Server 已签出 token）、签发被取消时，仍按原 op 关闭一次；确定不需要关闭的错误不关闭。
func TestSourceFetchClosesUnansweredIssue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		setup func(fs *fakeServer, cancel context.CancelFunc)
		close bool
	}{
		{name: "响应丢失", setup: func(fs *fakeServer, _ context.CancelFunc) { fs.issueLost = 1 }, close: true},
		{name: "Server 已签发、回复的正文无法解码", setup: func(fs *fakeServer, _ context.CancelFunc) { fs.issueGarbled = 1 }, close: true},
		{name: "Server 已签发、代理回复普通的 401", setup: func(fs *fakeServer, _ context.CancelFunc) { fs.issueProxy401 = 1 }, close: true},
		{name: "签发途中取消", setup: func(fs *fakeServer, cancel context.CancelFunc) {
			fs.issueHook = func(*runnerv1.IssueGitTokenRequest) { cancel() }
		}, close: true},
		{name: "未声明的仓库", setup: func(fs *fakeServer, _ context.CancelFunc) {
			fs.issueErrs = []error{connect.NewError(connect.CodePermissionDenied, errors.New("未声明"))}
		}},
		{name: "结果不明", setup: func(fs *fakeServer, _ context.CancelFunc) {
			fs.issueErrs = []error{connect.NewError(connect.CodeAborted, errors.New("结果不明"))}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			g := newGitHTTP(t, fs, "")
			sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
			r := newTestRunner(t, fs)
			spec := hostJob(1)
			spec.Source = g.source(1, "o/r", sha)
			fs.register(spec)
			j := r.newJob(spec)
			j.log = runnerkit.NewLogShipper(j.appendLog)
			defer j.log.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fs.mu.Lock()
			c.setup(fs, cancel)
			fs.mu.Unlock()
			if err := r.fetchSource(ctx, spec.Source, j, t.TempDir(), j.log); err == nil {
				t.Fatal("签发失败时拉取应失败")
			}
			issues := fs.issueRequests()
			if len(issues) != 1 {
				t.Fatalf("签发了 %d 次", len(issues))
			}
			got := revokesOf(fs, issues[0].OpId)
			switch {
			case c.close && (len(got) != 1 || got[0].Token != ""):
				t.Errorf("应按原 op 关闭一次（不带 token），实际 %v", got)
			case !c.close && len(got) != 0:
				t.Errorf("不应关闭：%v", got)
			}
			// Server 上已经签出的 token 都被这次关闭吊销了
			for _, o := range fs.gitOpStates() {
				if !o.revoked {
					t.Errorf("op %s 的 token 没有吊销", o.op)
				}
			}
		})
	}
}

// M1（step）：签发的响应没收到（或者收到了但正文无法解码）时操作也要关闭，两种先后都恰好一次：先收到 RPC 错误再断开连接；
// 先断开连接再收到错误。
func TestGitChannelClosesUnansweredIssue(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(fs *fakeServer){
		"先收到错误（响应丢失）":              func(fs *fakeServer) { fs.issueLost = 1 },
		"先收到错误（Server 已签发、正文无法解码）": func(fs *fakeServer) { fs.issueGarbled = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newChannelFixture(t)
			f.fs.mu.Lock()
			setup(f.fs)
			f.fs.mu.Unlock()
			cmd, out := f.git(t, "ls-remote", f.g.url("o/dep"))
			if err := cmd.Wait(); err == nil {
				t.Fatalf("签发失败时传输应失败：\n%s", out)
			}
			issues := f.fs.issueRequests()
			if len(issues) != 1 {
				t.Fatalf("签发了 %d 次，期望 1 次（不重试）", len(issues))
			}
			op := issues[0].OpId
			if m := f.waitRevoke(t, op); m.Token != "" {
				t.Error("没有收到 token，关闭时不应带 token")
			}
			f.ch.close()
			if n := len(revokesOf(f.fs, op)); n != 1 {
				t.Errorf("关闭了 %d 次，期望 1 次", n)
			}
			for _, o := range f.fs.gitOpStates() {
				if !o.revoked {
					t.Errorf("op %s 的 token 没有吊销", o.op)
				}
			}
		})
	}
	t.Run("先断开连接", func(t *testing.T) {
		t.Parallel()
		f := newChannelFixture(t)
		entered := make(chan string, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		f.fs.mu.Lock()
		f.fs.issueLost = 1
		f.fs.issueHook = func(m *runnerv1.IssueGitTokenRequest) {
			entered <- m.OpId
			<-release
		}
		f.fs.mu.Unlock()
		cmd, _ := f.git(t, "ls-remote", f.g.url("o/dep"))
		op := waitFor(t, entered, "签发")
		ops := f.ch.openOps()
		if len(ops) != 1 {
			t.Fatalf("打开的操作：%v", ops)
		}
		killWrapper(t, ops[0].pid)
		f.waitRevoke(t, op)
		releaseOnce.Do(func() { close(release) })
		f.ch.close()   // 等签发的错误返回
		_ = cmd.Wait() // 传输已经失败，退出状态没有意义（先放行签发：helper 还拿着 git 的输出管道）
		if n := len(revokesOf(f.fs, op)); n != 1 {
			t.Errorf("关闭了 %d 次，期望 1 次", n)
		}
	})
}

// killWrapper 结束传输包装（SIGKILL）；已经退出时忽略。
func killWrapper(t *testing.T, pid int) {
	t.Helper()
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill() // 可能已经随 git 退出
	}
}

// openRaw 以传输包装的身份开一个操作，返回连接（保持打开）和操作 id。
func openRaw(t *testing.T, ch *gitChannel, url string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("unix", ch.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := writeGitMsg(conn, gitMsg{Cmd: gitCmdOpen, URL: url}); err != nil {
		t.Fatal(err)
	}
	var r gitReply
	if err := readGitMsg(bufio.NewReader(conn), &r); err != nil || r.Op == "" {
		t.Fatalf("open = %+v，%v", r, err)
	}
	return conn, r.Op
}

// getRaw 以 credential helper 的身份要操作 op 的凭据（仓库 repo）。
func getRaw(t *testing.T, ch *gitChannel, host, op, repo string) gitReply {
	t.Helper()
	conn, err := net.Dial("unix", ch.sock)
	if err != nil {
		return gitReply{Error: err.Error()}
	}
	defer conn.Close()
	if err := writeGitMsg(conn, gitMsg{Cmd: gitCmdGet, Op: op, Protocol: "http", Host: host, Path: repo + ".git"}); err != nil {
		return gitReply{Error: err.Error()}
	}
	var r gitReply
	if err := readGitMsg(bufio.NewReader(conn), &r); err != nil {
		return gitReply{Error: err.Error()}
	}
	return r
}

// M4：收尾在锁内一次停止签发和交付——已经读到的 get 在收尾开始后不再签发；多个操作的关闭并发进行；在途的签发由 Server
// 收回（关闭后不交出）。
func TestGitChannelCloseInterleaving(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	host := strings.TrimPrefix(f.g.srv.URL, "http://")
	_, opA := openRaw(t, f.ch, f.g.url("o/r"))
	_, opB := openRaw(t, f.ch, f.g.url("o/dep"))
	_, opC := openRaw(t, f.ch, f.g.url("o/r"))
	_, opD := openRaw(t, f.ch, f.g.url("o/dep"))
	a := getRaw(t, f.ch, host, opA, "o/r")
	if a.Password == "" {
		t.Fatalf("A 没有拿到凭据：%+v", a)
	}
	// B：Server 慢，签发在途
	slow := make(chan struct{}, 1)
	releaseB := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseB) }) })
	// A、B 的关闭都到了 Server 才放行：串行关闭时第一个会一直等下去
	var mu sync.Mutex
	arrived := map[string]bool{}
	both := make(chan struct{})
	f.fs.mu.Lock()
	f.fs.issueHook = func(m *runnerv1.IssueGitTokenRequest) {
		if m.OpId == opB {
			slow <- struct{}{}
			<-releaseB
		}
	}
	f.fs.revokeHook = func(m *runnerv1.RevokeGitTokenRequest) {
		mu.Lock()
		arrived[m.OpId] = true
		if arrived[opA] && arrived[opB] && !arrived["done"] {
			arrived["done"] = true
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(testWait):
		}
	}
	f.fs.mu.Unlock()
	bDone := make(chan gitReply, 1)
	go func() { bDone <- getRaw(t, f.ch, host, opB, "o/dep") }()
	waitFor(t, slow, "B 的签发")
	// D：get 已经读到，停在处理之前
	dRead := make(chan struct{}, 1)
	dGo := make(chan struct{})
	f.ch.beforeGet = func() {
		dRead <- struct{}{}
		<-dGo
	}
	dDone := make(chan gitReply, 1)
	go func() { dDone <- getRaw(t, f.ch, host, opD, "o/dep") }()
	waitFor(t, dRead, "D 的 get")

	start := time.Now()
	closed := make(chan struct{})
	go func() {
		f.ch.close()
		close(closed)
	}()
	waitFor(t, f.ch.stop, "收尾开始")
	close(dGo)
	if d := waitFor(t, dDone, "D 的回复"); d.Password != "" {
		t.Error("收尾开始后已读到的 get 仍拿到了凭据")
	}
	waitFor(t, both, "A、B 的关闭同时到达 Server")
	releaseOnce.Do(func() { close(releaseB) })
	waitFor(t, closed, "收尾结束")
	if d := time.Since(start); d > testWait/2 {
		t.Errorf("收尾用了 %s：关闭不是并发的", d)
	}
	if b := waitFor(t, bDone, "B 的回复"); b.Password != "" {
		t.Error("收尾后 B 仍拿到了凭据")
	}
	if got := revokesOf(f.fs, opA); len(got) != 1 || got[0].Token != a.Password {
		t.Errorf("A 应带 token 关闭一次：%v", got)
	}
	if got := revokesOf(f.fs, opB); len(got) != 1 || got[0].Token != "" {
		t.Errorf("B 应只带 op 关闭一次：%v", got)
	}
	for _, op := range []string{opC, opD} {
		if got := revokesOf(f.fs, op); len(got) != 0 {
			t.Errorf("没有签发过的操作 %s 不应关闭：%v", op, got)
		}
	}
	for _, m := range f.fs.issueRequests() {
		if m.OpId == opD || m.OpId == opC {
			t.Errorf("收尾开始后为 %s 签发了", m.OpId)
		}
	}
}

// M4：收尾有总期限：Server 的签发和关闭都不返回时，到期取消，不无界地拖住 job 的结束。
func TestGitChannelCloseDeadline(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	host := strings.TrimPrefix(f.g.srv.URL, "http://")
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	entered := make(chan struct{}, 4)
	f.fs.mu.Lock()
	f.fs.issueHook = func(*runnerv1.IssueGitTokenRequest) { entered <- struct{}{}; <-hang }
	f.fs.revokeHook = func(*runnerv1.RevokeGitTokenRequest) { <-hang }
	f.fs.mu.Unlock()
	f.ch.closeTimeout = 200 * time.Millisecond
	for _, repo := range []string{"o/r", "o/dep"} {
		_, op := openRaw(t, f.ch, f.g.url(repo))
		go getRaw(t, f.ch, host, op, repo)
		waitFor(t, entered, "签发")
	}
	start := time.Now()
	f.ch.close()
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("收尾用了 %s，期望在总期限后很快结束", d)
	}
}

// M2：取消缓存缺失的 fetch 时结束整个进程组（真实的远程助手阻塞在请求上），关闭一次操作、释放仓库锁。
func TestSourceFetchCancelKillsHelper(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
	r := newTestRunner(t, fs)
	spec := hostJob(1)
	spec.Source = g.source(5, "o/r", sha)
	fs.register(spec)
	j := r.newJob(spec)
	j.log = runnerkit.NewLogShipper(j.appendLog)
	defer j.log.Close()
	held := make(chan struct{}, 1)
	gone := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	g.setHold(func(req *http.Request, _ string) {
		held <- struct{}{}
		select {
		case <-req.Context().Done(): // 远程助手的连接断开：进程被结束了
			gone <- struct{}{}
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.fetchSource(ctx, spec.Source, j, t.TempDir(), j.log) }()
	waitFor(t, held, "fetch 的请求")
	cancel()
	waitFor(t, gone, "远程助手被结束")
	if err := waitFor(t, done, "fetchSource 返回"); err == nil {
		t.Fatal("取消后应失败")
	}
	ops := opsFor(fs, "o/r")
	if len(ops) != 1 || !ops[0].revoked {
		t.Fatalf("操作：%+v", ops)
	}
	if n := len(revokesOf(fs, ops[0].op)); n != 1 {
		t.Errorf("关闭了 %d 次，期望 1 次", n)
	}
	mu := r.repoLock(5)
	if !mu.TryLock() {
		t.Fatal("仓库锁没有释放")
	}
	mu.Unlock()
	// 已经取消的 ctx：cat-file 失败不当作缓存缺失，不开操作（签发请求发不出去时也会按原 op 关闭，所以两者都要是 0）
	issues, revokes := len(fs.issueRequests()), len(fs.revokeRequests())
	if err := r.fetchSource(ctx, spec.Source, j, t.TempDir(), j.log); err == nil {
		t.Fatal("ctx 已取消时应失败")
	}
	if n, m := len(fs.issueRequests())-issues, len(fs.revokeRequests())-revokes; n != 0 || m != 0 {
		t.Errorf("ctx 已取消时签发了 %d 次、关闭了 %d 次", n, m)
	}
}

// M3：netrc 的来源（NETRC 指定的文件，相对路径按传输的工作目录；HOME 下的）都检查，default 和主机名不区分大小写。
func TestNetrcSources(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	cases := []struct {
		name  string
		setup func(t *testing.T, home, dir string) []string
		want  string
	}{
		{name: "NETRC 指定的文件", setup: func(t *testing.T, _, _ string) []string {
			t.Helper()
			f := filepath.Join(t.TempDir(), "n")
			mustWrite(t, f, "machine github.com login x password p\n")
			return []string{"NETRC=" + f}
		}, want: "netrc 中的主机 github.com"},
		{name: "NETRC 相对路径按工作目录", setup: func(t *testing.T, _, dir string) []string {
			t.Helper()
			mustWrite(t, filepath.Join(dir, "rel-netrc"), "default login x password p\n")
			return []string{"NETRC=rel-netrc"}
		}, want: "netrc 中的 default"},
		{name: "大写的 DEFAULT", setup: func(t *testing.T, home, _ string) []string {
			t.Helper()
			mustWrite(t, filepath.Join(home, ".netrc"), "DEFAULT login x password p\n")
			return nil
		}, want: "netrc 中的 default"},
		{name: "大写的主机名", setup: func(t *testing.T, home, _ string) []string {
			t.Helper()
			mustWrite(t, filepath.Join(home, ".netrc"), "machine GITHUB.COM login x password p\n")
			return nil
		}, want: "netrc 中的主机 github.com"},
		{name: "NETRC 和 HOME 都检查", setup: func(t *testing.T, home, _ string) []string {
			t.Helper()
			mustWrite(t, filepath.Join(home, ".netrc"), "machine github.com login x password p\n")
			return []string{"NETRC=" + filepath.Join(t.TempDir(), "missing")}
		}, want: "netrc 中的主机 github.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, home := gitTestEnv(t)
			dir := t.TempDir()
			env = append(env, c.setup(t, home, dir)...)
			if got := joinConflicts(gitAuthConflicts(u, dir, env)); !strings.Contains(got, c.want) || strings.Contains(got, "password p") {
				t.Errorf("冲突 = %q，期望包含 %q、不含内容", got, c.want)
			}
		})
	}
	if os.Geteuid() != 0 {
		env, home := gitTestEnv(t)
		f := filepath.Join(home, ".netrc")
		mustWrite(t, f, "machine example.com\n")
		if err := os.Chmod(f, 0); err != nil {
			t.Fatal(err)
		}
		if got := joinConflicts(gitAuthConflicts(u, t.TempDir(), env)); !strings.Contains(got, "查询失败："+f+" 读不了") {
			t.Errorf("读不了的 netrc 应拒绝：%q", got)
		}
	}
}

// M6：诊断不回显地址中的用户名、密码、query、fragment，解析不了的地址、git 的输出和 helper 给的原始字符串。
func TestGitDiagnosticsRedacted(t *testing.T) {
	t.Parallel()
	const mark = "SENSITIVEMARK"
	scope := newGitScope(&runnerv1.JobSpec{Source: &runnerv1.Source{Repository: "o/r", CloneUrl: "https://github.com/o/r.git"}})
	for raw, want := range map[string]string{
		"https://u" + mark + ":p" + mark + "@github.com/o/r.git?x=" + mark + "#" + mark: "https://github.com/o/r",
		"https://github.com/o/" + mark + "/x/y?q=" + mark:                               "https://github.com/…",
		"https://github.com/o/%72" + mark + ".git":                                      "https://github.com/…",
		"::" + mark + "%zz": unparsableGitURL,
	} {
		if got := safeGitURL(raw); got != want {
			t.Errorf("safeGitURL(%q) = %q，期望 %q", raw, got, want)
		}
		_, reason := scope.approve(raw)
		if strings.Contains(reason, mark) {
			t.Errorf("approve 的原因回显了内容：%s", reason)
		}
	}
	env, home := gitTestEnv(t)
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[http\n\textraHeader = X: "+mark+"\n")
	got := joinConflicts(gitAuthConflicts("https://u:"+mark+"@github.com/o/r.git?"+mark, t.TempDir(), env))
	if !strings.Contains(got, "查询失败") || !strings.Contains(got, "地址中的用户名或密码") || strings.Contains(got, mark) {
		t.Errorf("冲突的说明：%s", got)
	}
	// helper 给的地址与批准的仓库不一致：不拼接 helper 的原始字符串
	f := newChannelFixture(t)
	_, op := openRaw(t, f.ch, f.g.url("o/dep"))
	r := getRaw(t, f.ch, mark, op, "o/"+mark)
	if r.Password != "" || r.Error == "" || strings.Contains(r.Error, mark) {
		t.Errorf("不一致的请求：%+v", r)
	}
}

// 授权的边界：不规范化地址，显式的默认端口、主机名末尾的点、路径中间多余的 / 都不算声明的仓库。
func TestGitScopeNonCanonical(t *testing.T) {
	t.Parallel()
	s := newGitScope(&runnerv1.JobSpec{Source: &runnerv1.Source{Repository: "o/r", CloneUrl: "https://github.com/o/r.git"}})
	for raw, want := range map[string]string{
		"https://github.com/o/r":         "o/r",
		"https://github.com//o/r.git":    "o/r", // 开头多余的 / 去掉
		"https://github.com/o/r.git//":   "o/r", // 结尾多余的 / 去掉
		"https://github.com:443/o/r":     "",
		"https://github.com./o/r":        "",
		"https://github.com/o//r.git":    "",
		"https://github.com/o/r.GIT":     "",
		"https://github.com/o/./r.git":   "",
		"https://github.com/o/r/../r":    "",
		"https://github.com/o/r.git.git": "",
	} {
		if got, _ := s.approve(raw); got != want {
			t.Errorf("approve(%s) = %q，期望 %q", raw, got, want)
		}
	}
}

// host 的 git exec 镜像按每个 step 的 PATH 中的 git 选（换了 git 时用它的 exec 目录），同一个 exec 目录只建一次。
func TestHostGitPerStepPath(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	log := newTestShipper(t, fs)
	defer log.Close()
	exe := testExe(t)
	realDir, err := gitExecPath(t.Context(), "git", "", os.Environ())
	if err != nil {
		t.Skip(err)
	}
	// 另一个 git：exec 目录是 realDir 的副本（逐个符号链接），其余照常交给系统的 git
	other := t.TempDir()
	if err := buildGitExecMirror(filepath.Join(other, "core"), realDir, "/bin/false"); err != nil {
		t.Fatal(err)
	}
	sys, err := lookPathOrSkip(t, "git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(other, "bin")
	mustWrite(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = --exec-path ]; then echo '"+filepath.Join(other, "core")+"'; exit 0; fi\nexec '"+sys+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := newTestDirs(t)
	root := d.root
	h := &hostExec{d: d}
	if err := h.useGit(t.Context(), gitPlan{scope: "https://github.com", sock: "/s", exe: exe}, log); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	a1 := h.gitAccess(t.Context(), map[string]string{"PATH": path}, "", log)
	a2 := h.gitAccess(t.Context(), map[string]string{"PATH": bin + ":" + path}, "", log)
	a3 := h.gitAccess(t.Context(), map[string]string{"PATH": path}, "", log)
	if a1.realExecPath != realDir || a2.realExecPath != filepath.Join(other, "core") {
		t.Errorf("真实的 exec 目录：%s、%s", a1.realExecPath, a2.realExecPath)
	}
	if a1.execPath == a2.execPath || a3.execPath != a1.execPath {
		t.Errorf("镜像：%s、%s、%s", a1.execPath, a2.execPath, a3.execPath)
	}
	for _, a := range []*gitAccess{a1, a2} {
		target, err := os.Readlink(filepath.Join(a.execPath, "git-remote-https"))
		if err != nil {
			t.Fatalf("镜像 %s 中的 git-remote-https：%v", a.execPath, err)
		}
		if target != exe {
			t.Errorf("%s 中 git-remote-https → %s，期望 %s", a.execPath, target, exe)
		}
	}
	mirrors, err := filepath.Glob(filepath.Join(root, gitExecMirrorDir+"-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mirrors) != 2 {
		t.Errorf("建了 %d 个镜像，期望 2 个", len(mirrors))
	}
	// 第二个 git 实际走一次 HTTPS 的远程助手分派：到达 kiln-runner 的传输包装（没有凭据通道，包装在连接之前就停下，不联网）
	env, _ := gitTestEnv(t, "GIT_TERMINAL_PROMPT=0", "GIT_EXEC_PATH="+a2.execPath, gitSocketEnv+"=")
	env[0] = "PATH=" + bin + ":" + path
	cmd := exec.Command("git", "ls-remote", "https://127.0.0.1:9/o/r.git")
	cmd.Env, cmd.Dir = env, t.TempDir()
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "kiln-runner：无法连接 git 凭据通道") {
		t.Errorf("第二个 git 的 HTTPS 传输没有经过 kiln-runner 的包装：%v\n%s", err, out)
	}
}

// lookPathOrSkip 返回 name 的绝对路径（解析符号链接）；没有时跳过。
func lookPathOrSkip(t *testing.T, name string) (string, error) {
	t.Helper()
	p := lookPathIn(name, os.Getenv("PATH"), "")
	if !filepath.IsAbs(p) {
		t.Skipf("需要 %s", name)
	}
	return filepath.EvalSymlinks(p)
}

// 凭据通道没能打开（socket 路径太长）：没有声明依赖的 job 照常运行，git 要凭据时得到说明；声明了的 job 失败。
func TestGitChannelUnavailableDegrades(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
	r := newTestRunner(t, fs)
	r.gitSocketParent = filepath.Join(t.TempDir(), strings.Repeat("d", gitSocketPathMax()))
	if err := os.MkdirAll(r.gitSocketParent, 0o700); err != nil {
		t.Skipf("文件系统不支持这么长的目录名：%v", err)
	}
	step := &runnerv1.Step{
		Name: "fetch", Run: `if git ls-remote "$SELF" 2>err.txt; then exit 1; fi; cat err.txt; grep -q '凭据通道不可用' err.txt`,
		Env: map[string]string{"SELF": g.url("o/r")},
	}
	spec := hostJob(1, step)
	spec.Source = g.source(1, "o/r", sha)
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	spec2 := hostJob(2, bash("noop", "true"))
	spec2.Source = g.source(1, "o/r", sha)
	spec2.Repositories = []string{"o/dep"}
	got, log = runJob(t, r, fs, spec2)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classPrepare)
	if !strings.Contains(got.Message, "上限") {
		t.Errorf("结果说明 = %q", got.Message)
	}
	if slices.ContainsFunc(fs.issueRequests(), func(m *runnerv1.IssueGitTokenRequest) bool { return m.Ref.AttemptId == 2 }) {
		t.Error("声明了依赖、通道不可用的 job 不应签发")
	}
}

// netrc 的分界是 libcurl 的全部 ASCII 空白（含 \v、\f、\r）和引号；引号中有转义时无法按词法判断，也算冲突。
func TestNetrcSeparators(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	for name, content := range map[string]string{
		"垂直制表符":         "machine\vgithub.com\vlogin x password p\n",
		"换页":            "machine\fgithub.com\flogin x password p\n",
		"回车":            "machine github.com\rlogin x password p\r\n",
		"default 与 \\v": "x\vDEFAULT\vlogin x password p\n",
		"引号中的转义":        "machine \"git\\hub.com\" login x password p\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, home := gitTestEnv(t)
			mustWrite(t, filepath.Join(home, ".netrc"), content)
			if got := joinConflicts(gitAuthConflicts(u, t.TempDir(), env)); !strings.Contains(got, "netrc") || strings.Contains(got, "password p") {
				t.Errorf("冲突 = %q", got)
			}
		})
	}
	env, home := gitTestEnv(t)
	mustWrite(t, filepath.Join(home, ".netrc"), "machine\vgithub.community login x password p\n")
	if got := gitAuthConflicts(u, t.TempDir(), env); len(got) != 0 {
		t.Errorf("别的主机不应算冲突：%v", got)
	}
}

// netrcCases 与 kiln-orca/test/gitops.test.ts 的 netrcCases 是同一组用例（同样的输入、同样的期望），两边要保持同步：AI 与
// CI 的 netrc 判定语义相同（主机 github.com）。
var netrcCases = []struct {
	name     string
	content  string
	conflict bool
}{
	{"主机名", "machine github.com login x password p\n", true},
	{"带引号、大小写不同的主机名", "machine \"GitHub.com\" login x password p\n", true},
	{"单引号为界", "machine 'github.com' login x password p\n", true},
	{"垂直制表符分界", "machine\vgithub.com\vlogin x password p\n", true},
	{"换页分界", "machine\fgithub.com\flogin x password p\n", true},
	{"回车分界", "machine github.com\rlogin x password p\r\n", true},
	{"default", "machine a.example login x password p\n\ndefault login y password z\n", true},
	{"大写 DEFAULT 与垂直制表符", "x\vDEFAULT\vlogin x password p\n", true},
	{"宏正文中的主机名", "macdef init\ncd github.com\n\n", true},
	{"引号中的转义", "machine \"git\\hub.com\" login x password p\n", true},
	{"别的主机的值中有反斜杠", "machine a.example login x password \"p\\q\"\n", true},
	{"主机名是别的词的一部分", "machine notgithub.com login x password github.com.evil\n", false},
	{"垂直制表符后是别的主机", "machine\vgithub.community login x password p\n", false},
	{"不间断空格不是分界", "machine\u00a0github.com\u00a0login x password p\n", false},
	{"引号外的反斜杠", "machine a.example login x password p\\q\n", false},
	{"空文件", "", false},
}

func TestNetrcSharedCases(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	for _, c := range netrcCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, home := gitTestEnv(t)
			mustWrite(t, filepath.Join(home, ".netrc"), c.content)
			got := gitAuthConflicts(u, t.TempDir(), env)
			if (len(got) > 0) != c.conflict {
				t.Errorf("冲突 = %v，期望 %v", got, c.conflict)
			}
			if strings.Contains(joinConflicts(got), "password p") {
				t.Errorf("诊断中出现了 netrc 的内容：%v", got)
			}
		})
	}
}

// 真实的 git（libcurl）按 \v 分界读 netrc、带上其中的凭据时，冲突检查必须拒绝。
func TestNetrcVerticalTabRealGit(t *testing.T) {
	t.Parallel()
	env, home := gitTestEnv(t)
	seen := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			select {
			case seen <- a:
			default:
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	mustWrite(t, filepath.Join(home, ".netrc"), "machine\v127.0.0.1\vlogin user password pass\n")
	raw := srv.URL + "/o/r.git"
	cmd := exec.Command("git", "ls-remote", raw)
	cmd.Env = append(slices.Clone(env), "GIT_TERMINAL_PROMPT=0")
	out, _ := cmd.CombinedOutput()
	select {
	case <-seen:
	default:
		t.Skipf("这个平台的 libcurl 不按 \\v 分界读 netrc：%s", out)
	}
	if got := gitAuthConflicts(raw, t.TempDir(), env); len(got) == 0 {
		t.Fatal("git 用了 netrc 中的凭据，冲突检查却没有拒绝")
	}
}

// gitTokensFunc 是用函数实现的 gitTokens。
type gitTokensFunc struct {
	issue  func(ctx context.Context, op, repo string) (string, time.Time, error)
	revoke func(ctx context.Context, op, token string) error
}

func (f gitTokensFunc) issueGitToken(ctx context.Context, op, repo string) (string, time.Time, error) {
	return f.issue(ctx, op, repo)
}

func (f gitTokensFunc) revokeGitToken(ctx context.Context, op, token string) error {
	return f.revoke(ctx, op, token)
}

// 收尾中所有的关闭、吊销（收尾派发的、迟到 token 的）共用 gitCloseParallel 个位置。
func TestGitChannelRevokeParallelLimit(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	const n = gitCloseParallel + 1
	entered := make(chan struct{}, n)
	deliver := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var active, peak atomic.Int64
	f.ch.tokens = gitTokensFunc{
		issue: func(_ context.Context, op, _ string) (string, time.Time, error) {
			entered <- struct{}{}
			<-deliver
			return "ghs_late_" + op, time.Now().Add(time.Hour), nil
		},
		revoke: func(ctx context.Context, _, _ string) error {
			cur := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); cur > p && !peak.CompareAndSwap(p, cur); p = peak.Load() {
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	host := strings.TrimPrefix(f.g.srv.URL, "http://")
	for range n {
		_, op := openRaw(t, f.ch, f.g.url("o/dep"))
		go getRaw(t, f.ch, host, op, "o/dep")
		waitFor(t, entered, "签发")
	}
	done := make(chan struct{})
	go func() { f.ch.close(); close(done) }()
	waitFor(t, f.ch.stop, "收尾开始")
	close(deliver)
	// 收尾派发的 n 个关闭和 n 个迟到 token 的吊销都在等位置：占满之后不再增加
	if err := pollUntil(t.Context(), testWait, time.Millisecond, func() (bool, error) { return active.Load() == gitCloseParallel, nil }); err != nil {
		t.Fatalf("同时进行的吊销 = %d：%v", active.Load(), err)
	}
	releaseOnce.Do(func() { close(release) })
	waitFor(t, done, "收尾结束")
	if p := peak.Load(); p > gitCloseParallel {
		t.Errorf("同时进行的吊销最多 %d 个，期望不超过 %d", p, gitCloseParallel)
	}
}

// 收尾到了期限就返回：即使签发卡在 runnerkit.Client 的认证锁上（另一个请求持锁、认证的响应正文不返回），不响应取消。
func TestGitChannelCloseReturnsAtDeadline(t *testing.T) {
	t.Parallel()
	f := newChannelFixture(t)
	authing := make(chan struct{})
	var authOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		authOnce.Do(func() { close(authing) })
		<-r.Context().Done()
	}))
	// 先于下面放开认证锁的 Cleanup 注册、后于它执行：Close 要等阻塞的认证请求结束
	t.Cleanup(srv.Close)
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := runnerkit.NewClient(srv.URL, 1, key)
	actx, acancel := context.WithCancel(t.Context())
	authDone := make(chan struct{})
	go func() { _, _ = client.Authenticate(actx); close(authDone) }()
	// 测试结束时放开认证锁，留下的签发随之结束
	t.Cleanup(func() { acancel(); <-authDone })
	waitFor(t, authing, "另一个请求持有认证锁")
	j, ok := f.ch.tokens.(*job)
	if !ok {
		t.Fatal("通道的 tokens 不是 job")
	}
	f.ch.tokens = &job{r: &Runner{c: client}, spec: j.spec, log: j.log}
	f.ch.closeTimeout = 100 * time.Millisecond
	_, op := openRaw(t, f.ch, f.g.url("o/dep"))
	go getRaw(t, f.ch, strings.TrimPrefix(f.g.srv.URL, "http://"), op, "o/dep")
	if err := pollUntil(t.Context(), testWait, time.Millisecond, func() (bool, error) {
		f.ch.mu.Lock()
		defer f.ch.mu.Unlock()
		return f.ch.ops[op] != nil && f.ch.ops[op].ready != nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.ch.close(); close(done) }()
	waitFor(t, done, "收尾在期限后返回")
	j.log.Close()
	if log := f.fs.log(1); !strings.Contains(log, "不再等待") {
		t.Errorf("job 日志中没有记下没结束的操作：\n%s", log)
	}
}

// 检查缓存的 git cat-file 被信号终止（不是 git 正常退出说没有这个对象）：不当作缓存缺失，不签发、不 fetch，返回错误。
// 改 PATH（本进程中找 git 的路径）：不能与别的测试并行。
func TestSourceCatFileKilledIsNotMiss(t *testing.T) {
	sysGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("需要 git")
	}
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = cat-file ]; then kill -KILL $$; fi\nexec '"+sysGit+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newTestRunner(t, fs)
	spec := hostJob(1)
	spec.Source = g.source(1, "o/r", sha)
	fs.register(spec)
	j := r.newJob(spec)
	j.log = runnerkit.NewLogShipper(j.appendLog)
	defer j.log.Close()
	err = r.fetchSource(t.Context(), spec.Source, j, t.TempDir(), j.log)
	if err == nil || !strings.Contains(err.Error(), "检查 bare 镜像") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fs.issueRequests()); n != 0 {
		t.Errorf("被信号终止的 cat-file 不是缓存缺失，却签发了 %d 次", n)
	}
}

// 建 host 的 git exec 镜像时符号链接中途失败：不留下不完整的目录、不占用目录名，后面的 step 照常建出镜像；残留的同名目录
// （例如之前的版本留下的）被跳过。
func TestHostGitMirrorPartialFailure(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	log := newTestShipper(t, fs)
	defer log.Close()
	if _, err := gitExecPath(t.Context(), "git", "", os.Environ()); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	// 残留：git-exec-0 已经存在
	if err := os.Mkdir(filepath.Join(root, gitExecMirrorDir+"-0"), 0o700); err != nil {
		t.Fatal(err)
	}
	g := newHostGit(gitAccess{scope: "https://github.com", helper: "h", socket: "/s"}, root, testExe(t))
	n := 0
	g.link = func(oldname, newname string) error {
		if n++; n == 3 {
			return errors.New("注入的失败")
		}
		return os.Symlink(oldname, newname)
	}
	env := map[string]string{"PATH": os.Getenv("PATH")}
	if a := g.access(t.Context(), env, "", log); a.execPath != "" {
		t.Fatalf("建镜像失败时不应设置 GIT_EXEC_PATH：%+v", a)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != gitExecMirrorDir+"-0" {
		t.Errorf("失败后留下了：%v", entries)
	}
	g.link = os.Symlink
	a := g.access(t.Context(), env, "", log)
	if a.execPath == "" || filepath.Base(a.execPath) == gitExecMirrorDir+"-0" {
		t.Fatalf("之后的 step 应建出新的镜像（跳过残留的名字）：%+v", a)
	}
	target, err := os.Readlink(filepath.Join(a.execPath, "git-remote-https"))
	if err != nil {
		t.Fatal(err)
	}
	if target != testExe(t) {
		t.Errorf("git-remote-https → %s", target)
	}
}

// 选镜像目录名：正常时是 git-exec-0；残留的名字跳过；父目录不可遍历、不是目录时立即返回错误（不会一直循环）；已存在的名字
// 达到上限时返回错误。
func TestHostGitNextMirrorDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	g := newHostGit(gitAccess{}, root, "")
	if dir, err := g.nextMirrorDir(); err != nil || dir != filepath.Join(root, gitExecMirrorDir+"-0") {
		t.Errorf("正常：%q %v", dir, err)
	}
	// 残留的 git-exec-1、git-exec-2 跳过
	for _, n := range []string{"1", "2"} {
		if err := os.Mkdir(filepath.Join(root, gitExecMirrorDir+"-"+n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if dir, err := g.nextMirrorDir(); err != nil || dir != filepath.Join(root, gitExecMirrorDir+"-3") {
		t.Errorf("跳过残留：%q %v", dir, err)
	}
	// 已存在的名字达到上限
	full := t.TempDir()
	for i := range maxMirrorDirTries {
		if err := os.Mkdir(filepath.Join(full, gitExecMirrorDir+"-"+strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newHostGit(gitAccess{}, full, "").nextMirrorDir(); err == nil {
		t.Error("名字都被占用时应返回错误")
	}
	// 父目录不是目录
	file := filepath.Join(t.TempDir(), "f")
	mustWrite(t, file, "")
	if _, err := newHostGit(gitAccess{}, file, "").nextMirrorDir(); err == nil {
		t.Error("父目录不是目录时应返回错误")
	}
	// 父目录不可遍历（root 用户不受权限限制，跳过这一项）
	if os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		sub := filepath.Join(locked, "a")
		g := newHostGit(gitAccess{scope: "https://github.com", helper: "h", socket: "/s"}, sub, "")
		if _, err := g.nextMirrorDir(); err == nil || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("父目录不可遍历：%v", err)
		}
		// access 照常返回（不设置 GIT_EXEC_PATH），不会卡住
		log := newTestShipper(t, newFakeServer(t))
		defer log.Close()
		done := make(chan *gitAccess, 1)
		go func() { done <- g.access(t.Context(), map[string]string{"PATH": os.Getenv("PATH")}, "", log) }()
		select {
		case a := <-done:
			if a.execPath != "" {
				t.Errorf("建不成镜像时不应设置 GIT_EXEC_PATH：%+v", a)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("access 卡住")
		}
	}
}

// bare 镜像的配置损坏：cat-file 以 128 退出（对象其实在）。不能当作缓存缺失去签发：零签发，错误说明是本地镜像检查失败。
// 对象确实缺失时照常签发。
func TestSourceMirrorCheckFailureIsNotMiss(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
	r := newTestRunner(t, fs)
	run := func(id int64) error {
		spec := hostJob(id)
		spec.Source = g.source(1, "o/r", sha) // 同一个仓库（同一个 bare 镜像）
		fs.register(spec)
		j := r.newJob(spec)
		j.log = runnerkit.NewLogShipper(j.appendLog)
		defer j.log.Close()
		return r.fetchSource(t.Context(), spec.Source, j, t.TempDir(), j.log)
	}
	// 对象缺失：照常签发一次、fetch
	if err := run(1); err != nil {
		t.Fatal(err)
	}
	if n := len(fs.issueRequests()); n != 1 {
		t.Fatalf("缓存缺失时签发了 %d 次，期望 1 次", n)
	}
	// 损坏镜像的配置：对象在，但 cat-file 以 128 退出
	mirror := r.cfg.path(gitDirName, "1.git")
	mustWrite(t, filepath.Join(mirror, "config"), "[[[ broken\n")
	err := run(2)
	if err == nil || !strings.Contains(err.Error(), "检查 bare 镜像") || !strings.Contains(err.Error(), "没有申请凭据") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fs.issueRequests()); n != 1 {
		t.Errorf("镜像检查失败时又签发了（共 %d 次）", n)
	}
}

// step 的 PATH 中的相对项（以及表示当前目录的空项）按 step 的工作目录解析，与 step 的 shell 找到的 git 相同：项目自带的
// git（step 目录下的 tools/git）被选中，不是 Runner 当前目录下解析出来的系统 git。
func TestHostGitRelativePath(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	log := newTestShipper(t, fs)
	defer log.Close()
	realDir, err := gitExecPath(t.Context(), "git", "", os.Environ())
	if err != nil {
		t.Skip(err)
	}
	sys, err := lookPathOrSkip(t, "git")
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDirs(t)
	h := &hostExec{d: d}
	if err := h.useGit(t.Context(), gitPlan{scope: "https://github.com", sock: "/s", exe: testExe(t)}, log); err != nil {
		t.Fatal(err)
	}
	// 项目自带的 git：exec 目录是 realDir 的副本，其余照常交给系统的 git
	projectGit := func(bin, core string) {
		if err := buildGitExecMirror(core, realDir, "/bin/false"); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = --exec-path ]; then pwd > '"+core+".cwd'; echo '"+core+"'; exit 0; fi\nexec '"+sys+"' \"$@\"\n")
		if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	step := filepath.Join(d.ws, "sub")
	projectGit(filepath.Join(step, "tools"), filepath.Join(d.root, "core-tools"))
	path := os.Getenv("PATH")
	a := h.gitAccess(t.Context(), map[string]string{"PATH": "tools:" + path}, "sub", log)
	if a.realExecPath != filepath.Join(d.root, "core-tools") {
		t.Errorf("相对的 PATH 项：选中的 exec 目录是 %q，期望 step 目录下 tools/git 的", a.realExecPath)
	}
	// git --exec-path 在 step 的工作目录中执行
	if b, err := os.ReadFile(filepath.Join(d.root, "core-tools.cwd")); err != nil {
		t.Error(err)
	} else if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(b))); got != mustEvalSymlinks(t, step) {
		t.Errorf("git --exec-path 的工作目录是 %s，期望 %s", got, step)
	}
	// 空项表示当前目录：step 目录下的 git
	projectGit(step, filepath.Join(d.root, "core-empty"))
	if a := h.gitAccess(t.Context(), map[string]string{"PATH": ":" + path}, "sub", log); a.realExecPath != filepath.Join(d.root, "core-empty") {
		t.Errorf("空的 PATH 项：选中的 exec 目录是 %q，期望 step 目录下 git 的", a.realExecPath)
	}
	// 不合法的 working-directory：不建镜像（run 随后以同一个原因失败）
	if a := h.gitAccess(t.Context(), map[string]string{"PATH": path}, "../x", log); a.execPath != "" {
		t.Errorf("working-directory 不合法时不应建镜像：%+v", a)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// git --exec-path 的探针（可能是项目替换 git 的包装）留下仍持有输出管道的后台进程时，等待有界：直接子进程正常退出后
// 在 WaitDelay 到期时返回 exec.ErrWaitDelay（不一直等到后代退出）；取消时向整个进程组发 SIGTERM，连同后代一并终止。
func TestGitExecPathBoundedWait(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	// 后台子进程继承标准输出，先写下自己的 PID，再打开测试的 FIFO 写端发送 "ok" 就绪信号并 exec sleep 600 保持打开。
	// 用 FIFO 的 EOF（而不是 Kill(pid, 0)）判断后代是否退出：内核在进程终止时立即关闭其 fd，即使在无 init 回收孤儿的
	// Linux 容器（僵尸进程）中也能可靠观测到退出。
	mustWrite(t, git, `#!/bin/sh
sh -c 'printf "%s\n" "$$" > "$KILN_TEST_PID_FILE"; exec 3>"$KILN_TEST_HOLD_FIFO"; printf "ok\n" >&3; exec sleep 600' &
if [ "$KILN_TEST_HANG" = 1 ]; then exec sleep 600; fi
echo /x/git-core
`)
	if err := os.Chmod(git, 0o755); err != nil {
		t.Fatal(err)
	}
	killPIDFile := func(pidFile string) {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	openHoldFIFO := func(t *testing.T, fifoPath, pidFile string) (ready <-chan struct{}, exited <-chan struct{}) {
		t.Helper()
		if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
			t.Fatalf("mkfifo：%v", err)
		}
		readyCh := make(chan struct{})
		exitedCh := make(chan struct{})
		go func() {
			defer close(exitedCh)
			f, err := os.Open(fifoPath)
			if err != nil {
				return
			}
			defer f.Close()
			br := bufio.NewReader(f)
			if line, err := br.ReadString('\n'); err == nil && strings.TrimSpace(line) == "ok" {
				close(readyCh)
			}
			var buf [64]byte
			for {
				if _, err := br.Read(buf[:]); err != nil {
					return
				}
			}
		}()
		t.Cleanup(func() {
			killPIDFile(pidFile)
			// 写端可能从未打开（例如测试提前失败）：短轮询非阻塞打开写端以解除读端阻塞。
			for range 20 {
				select {
				case <-exitedCh:
					return
				default:
				}
				if w, err := os.OpenFile(fifoPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case <-exitedCh:
			case <-time.After(5 * time.Second):
			}
		})
		return readyCh, exitedCh
	}

	t.Run("直接子进程正常退出", func(t *testing.T) {
		sub := t.TempDir()
		pidFile := filepath.Join(sub, "pid")
		fifoPath := filepath.Join(sub, "hold.fifo")
		ready, exited := openHoldFIFO(t, fifoPath, pidFile)
		env := append(os.Environ(), "KILN_TEST_PID_FILE="+pidFile, "KILN_TEST_HOLD_FIFO="+fifoPath)
		done := make(chan error, 1)
		go func() {
			_, err := gitExecPath(t.Context(), git, "", env)
			done <- err
		}()
		// 宽松总期限仅防止 WaitDelay 失效时无限卡死，不拿包含进程启动和调度的紧墙钟卡 WaitDelay。
		var err error
		select {
		case err = <-done:
		case <-time.After(gitStopGrace + 20*time.Second):
			t.Fatal("后代持有管道时 gitExecPath 没有在宽松期限内因 WaitDelay 返回")
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("后代持有管道时应返回 exec.ErrWaitDelay，实际：%v", err)
		}
		// 确认后代已启动且返回时仍持有 FIFO 写端（证明是 WaitDelay 关闭管道返回，而非后代提前退出）。
		select {
		case <-ready:
		default:
			t.Error("后代进程未按预期启动并持有管道")
		}
		select {
		case <-exited:
			t.Error("返回时后代进程应仍存活（持有管道），却已退出")
		default:
		}
	})

	t.Run("取消", func(t *testing.T) {
		sub := t.TempDir()
		pidFile := filepath.Join(sub, "pid")
		fifoPath := filepath.Join(sub, "hold.fifo")
		ready, exited := openHoldFIFO(t, fifoPath, pidFile)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		env := append(os.Environ(), "KILN_TEST_PID_FILE="+pidFile, "KILN_TEST_HOLD_FIFO="+fifoPath, "KILN_TEST_HANG=1")
		done := make(chan error, 1)
		go func() {
			_, err := gitExecPath(ctx, git, "", env)
			done <- err
		}()
		// 握手确认探针的后台后代已启动并持有输出管道，再取消上下文。
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("探针尚未就绪就提前返回：%v", err)
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatal("等待探针启动超时")
		}
		cancel()
		var err error
		select {
		case err = <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("取消后 gitExecPath 没有在期限内返回")
		}
		if err == nil {
			t.Error("取消后应报错")
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			t.Errorf("取消应终止整个进程组而不是拖到 WaitDelay：%v", err)
		}
		// 确认后台后代已随进程组被 SIGTERM 终止（FIFO 写端关闭产生 EOF）。
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("取消后后代进程仍未退出（应随进程组一并终止）")
		}
	})
}

// lookPathIn 与真实的 /bin/sh 查找命令的结果一致（同一组构造的 PATH，在 step 的工作目录中执行 git）：空项在开头、中间、
// 末尾，整个 PATH 为空，重复的分隔符，相对项（含 ./），项是文件而不是目录，同名的是目录、不可执行、只有别人可执行，
// 目录不可进入，前面的项胜出，不存在的项，项中的 ..（前面的组件不存在时走不通；符号链接后的 .. 回到链接目标的父目录）。
// 每个 git 打印自己相对 root 的位置，两边都按实际执行 / 选中的文件的内容比较（不按路径的字面）。
func TestLookPathInMatchesShell(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "base") // step 的工作目录，里面也有一个 git
	gitIn := func(dir string, mode os.FileMode) string {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "git")
		id, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p, "#!/bin/sh\necho "+id+"\n")
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	gitIn(base, 0o755)
	gitIn(filepath.Join(base, "rel"), 0o755)
	a := gitIn(filepath.Join(root, "a"), 0o755)
	b := gitIn(filepath.Join(root, "b"), 0o755)
	noexec := gitIn(filepath.Join(root, "noexec"), 0o644)
	others := gitIn(filepath.Join(root, "others"), 0o611) // 本用户（属主）不可执行，同组和别人可执行
	dirgit := filepath.Join(root, "dirgit")
	if err := os.MkdirAll(filepath.Join(dirgit, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	mustWrite(t, file, "")
	// link 指向 far/inner：link/../x 是 far/x；按字面 Clean 会变成 root 下的 x（诱饵）
	gitIn(filepath.Join(root, "far", "x"), 0o755)
	gitIn(filepath.Join(root, "x"), 0o755)
	if err := os.MkdirAll(filepath.Join(root, "far", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "far", "inner"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	cases := []string{
		"", ":", "::", ":" + b, b + ":", noexec + ":", noexec + "::", noexec + "::" + b, a + ":" + b, b + ":" + a,
		"rel:" + b, "./rel:" + b, "rel", "missing:" + b, "missing", file + ":" + b, dirgit + ":" + b, dirgit,
		noexec + ":" + b, noexec, others + ":" + b, others, noexec + ":rel", ":" + noexec,
		"missing/../rel:" + b, root + "/missing/../a:" + b, "rel/../rel:" + b,
		link + "/../x:" + b, link + "/../x", link + "/../../x:" + b,
	}
	if os.Getuid() != 0 { // root 不受目录权限限制（只有别人可执行的那一项对 root 同样等价：access 与 execve 都只要求有 x 位）
		locked := gitIn(filepath.Join(root, "locked"), 0o755)
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		cases = append(cases, locked+":"+b, locked)
	}
	for _, p := range cases {
		cmd := exec.Command("/bin/sh", "-c", "git")
		cmd.Dir, cmd.Env = base, []string{"PATH=" + p}
		out, _ := cmd.Output()
		shell := strings.TrimSpace(string(out))
		got := lookPathIn("git", p, base)
		runner := ""
		if filepath.IsAbs(got) {
			b, err := os.ReadFile(got) // 按文件系统解析 got（不 Clean），读到的是 lookPathIn 选中的那个文件
			if err != nil {
				t.Fatalf("PATH=%q：读 lookPathIn 选中的 %s：%v", p, got, err)
			}
			runner = strings.TrimSpace(strings.TrimPrefix(strings.Split(string(b), "\n")[1], "echo "))
		}
		if shell != runner {
			t.Errorf("PATH=%q：/bin/sh 执行的是 %q，lookPathIn 选的是 %q（%s）", p, shell, runner, got)
		}
	}
}

// step 把 PATH 显式设为空字符串（单个空项，即当前目录）：选 step 工作目录中的 git，与 shell 相同；没有设置 PATH 时按找不到
// 处理（不模拟各 shell 的默认搜索路径）。
func TestHostGitEmptyPath(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	log := newTestShipper(t, fs)
	defer log.Close()
	realDir, err := gitExecPath(t.Context(), "git", "", os.Environ())
	if err != nil {
		t.Skip(err)
	}
	sys, err := lookPathOrSkip(t, "git")
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDirs(t)
	h := &hostExec{d: d}
	if err := h.useGit(t.Context(), gitPlan{scope: "https://github.com", sock: "/s", exe: testExe(t)}, log); err != nil {
		t.Fatal(err)
	}
	core := filepath.Join(d.root, "core-step")
	if err := buildGitExecMirror(core, realDir, "/bin/false"); err != nil {
		t.Fatal(err)
	}
	step := filepath.Join(d.ws, "sub")
	mustWrite(t, filepath.Join(step, "git"), "#!/bin/sh\nif [ \"$1\" = --exec-path ]; then echo '"+core+"'; exit 0; fi\nexec '"+sys+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(step, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if a := h.gitAccess(t.Context(), map[string]string{"PATH": ""}, "sub", log); a.realExecPath != core {
		t.Errorf("PATH 为空字符串：选中的 exec 目录是 %q，期望 step 目录下 git 的", a.realExecPath)
	}
	if a := h.gitAccess(t.Context(), map[string]string{}, "sub", log); a.execPath != "" {
		t.Errorf("没有设置 PATH 时不应建镜像：%+v", a)
	}
}

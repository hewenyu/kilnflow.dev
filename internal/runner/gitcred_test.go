package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 按操作签发的源码凭据在 host executor 上的模块功能测试（PLAN #8.4）：真实的 git、本地的 git HTTP 服务（gitHTTP，只接受 fakeServer 签发、没有吊销的 token），Server 是 fakeServer。

// V1：缓存缺失时为 fetch 签发任务仓库的只读 token，fetch 后吊销，之后检出、step 照常；缓存命中时不签发。
func TestSourceFetchTokenRevoked(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	r := newTestRunner(t, fs)
	job := func(attempt int64) *runnerv1.JobSpec {
		spec := hostJob(attempt, bash("build", `test "$(cat a.txt)" = hello`))
		spec.Source = g.source(1, "o/r", sha)
		return spec
	}
	got, log := runJob(t, r, fs, job(1))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	ops := opsFor(fs, "o/r")
	if len(ops) != 1 {
		t.Fatalf("签发了 %d 个操作，期望 1 个：%+v", len(ops), ops)
	}
	if !ops[0].revoked {
		t.Error("fetch 之后 token 没有吊销")
	}
	if !slices.Contains(g.usedTokens(), ops[0].token) {
		t.Error("fetch 没有用签发的 token")
	}
	for _, s := range []string{"git 凭据：签发只读 token（o/r，op " + ops[0].op, "git 凭据：已请求 Server 关闭操作、回收 token（o/r，op " + ops[0].op} {
		if !strings.Contains(log, s) {
			t.Errorf("日志中没有 %q：\n%s", s, log)
		}
	}
	// 关闭的 RPC 成功只说明 Server 关闭了操作（GitHub 侧的吊销可能失败、只在 Server 日志中有 warn）：任务日志不写成已吊销
	for _, s := range []string{"吊销 token", "已吊销"} {
		if strings.Contains(log, s) {
			t.Errorf("任务日志中有断言吊销结果的措辞 %q：\n%s", s, log)
		}
	}
	issued := len(fs.issueRequests())
	got, log = runJob(t, r, fs, job(2))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if n := len(fs.issueRequests()); n != issued {
		t.Errorf("命中缓存时又签发了 %d 次", n-issued)
	}
}

// V2、V7：step 中 git fetch 依赖仓库与本仓库各签一枚只读 token，每次传输结束即吊销（不等 job 结束）；job 的 HOME、
// 工作区、step 的环境、Runner 的数据目录和日志中都没有 token。
func TestStepGitFetchPerTransport(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	g.addRepo(t, "o/dep", map[string]string{"dep.txt": "dep\n"})
	r := newTestRunner(t, fs, func(c *Config) { c.KeepWorkspaces = true })
	step := &runnerv1.Step{Name: "fetch", Run: `git init -q d && cd d
git fetch -q "$DEP" main && test "$(git show FETCH_HEAD:dep.txt)" = dep
git fetch -q "$SELF" main && test "$(git show FETCH_HEAD:a.txt)" = hello
env > "$KILN_WORKSPACE/env.txt"`, Env: map[string]string{"DEP": g.url("o/dep"), "SELF": g.url("o/r")}}
	spec := hostJob(1, step)
	spec.Source = g.source(1, "o/r", sha)
	spec.Repositories = []string{"o/dep"}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")

	self, dep := opsFor(fs, "o/r"), opsFor(fs, "o/dep")
	if len(self) != 2 || len(dep) != 1 {
		t.Fatalf("o/r 签发了 %d 个操作（期望 2：拉取源码、step 补取），o/dep %d 个（期望 1）", len(self), len(dep))
	}
	used := g.usedTokens()
	for _, o := range append(self, dep...) {
		if !o.revoked {
			t.Errorf("op %s（%s）的 token 没有吊销", o.op, o.repo)
		}
		if !slices.Contains(used, o.token) {
			t.Errorf("op %s（%s）的 token 没有用于 git 传输", o.op, o.repo)
		}
	}
	// 依赖仓库的传输结束即吊销：吊销在下一次传输（补取本仓库）签发之前
	stepSelf := self[1]
	if rv, iss := fs.eventIndex("revoke "+dep[0].op), fs.eventIndex("issue "+stepSelf.op); rv < 0 || rv > iss {
		t.Errorf("o/dep 的 token 在传输结束后没有立即吊销（吊销在第 %d 个事件，下一次签发在第 %d 个）", rv, iss)
	}
	env, err := os.ReadFile(filepath.Join(r.workDir(1), attemptWorkspaceDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"GIT_EXEC_PATH=" + filepath.Join(r.workDir(1), gitExecMirrorDir), gitSocketEnv + "=", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(string(env), s) {
			t.Errorf("step 的环境中没有 %q：\n%s", s, env)
		}
	}
	// job 结束时关闭了凭据通道：socket 不在了，step 留下的进程连不上
	for line := range strings.SplitSeq(string(env), "\n") {
		if sock, ok := strings.CutPrefix(line, gitSocketEnv+"="); ok {
			if _, err := os.Stat(sock); !os.IsNotExist(err) {
				t.Errorf("job 结束后凭据通道的 socket %s 仍在：%v", sock, err)
			}
		}
	}
	tokens := issuedTokens(fs)
	if hits := scanTreeForTokens(t, r.cfg.Dir, tokens); len(hits) > 0 {
		t.Errorf("Runner 的数据目录（含 job 的 HOME、工作区、step 的环境）中有 token：%v", hits)
	}
	if found := scanTokens([]byte(log), tokens); len(found) > 0 {
		t.Errorf("日志中有 token：%v", found)
	}
}

// V2：未声明的仓库不签发，也不调用 askpass 或个人的 credential helper；没有声明依赖时仍可补取本仓库。
func TestStepGitUndeclaredRepo(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	g.addRepo(t, "o/other", map[string]string{"x": "x\n"})
	r := newTestRunner(t, fs)
	ws := workflow.WorkspacePlaceholder
	step := &runnerv1.Step{
		Name: "fetch", Run: `cat > "$KILN_WORKSPACE/askpass.sh" <<'EOF'
#!/bin/sh
touch "$KILN_WORKSPACE/askpass-called"
echo p
EOF
chmod +x "$KILN_WORKSPACE/askpass.sh"
git config --global credential.helper '!f() { touch "$KILN_WORKSPACE/personal-helper"; echo username=u; echo password=p; }; f'
git config --global core.askPass "$KILN_WORKSPACE/askpass.sh"
if git ls-remote "$OTHER" >/dev/null; then echo "未声明的仓库不应拿到凭据"; exit 1; fi
test ! -e "$KILN_WORKSPACE/personal-helper"
test ! -e "$KILN_WORKSPACE/askpass-called"
git ls-remote "$SELF" >/dev/null`,
		Env: map[string]string{
			"OTHER": g.url("o/other"), "SELF": g.url("o/r"),
			"GIT_ASKPASS": ws + "/askpass.sh", "SSH_ASKPASS": ws + "/askpass.sh",
		},
	}
	spec := hostJob(1, step)
	spec.Source = g.source(1, "o/r", sha)
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, m := range fs.issueRequests() {
		if m.Repository == "o/other" {
			t.Errorf("为未声明的仓库签发了 token：%v", m)
		}
	}
	if n := len(opsFor(fs, "o/r")); n != 2 {
		t.Errorf("o/r 签发了 %d 个操作，期望 2 个（拉取源码、step 补取）", n)
	}
	if !strings.Contains(log, "也不在 job 的 repositories 中") {
		t.Errorf("日志中没有不给凭据的原因：\n%s", log)
	}
}

// 认证冲突：执行环境中有对这个地址生效的 http.extraheader 时拒绝这次传输、不签发，说明中有键、没有值。
func TestStepGitAuthConflict(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	r := newTestRunner(t, fs)
	const header = "Authorization: Basic c2VjcmV0LWhlYWRlcg=="
	step := &runnerv1.Step{Name: "fetch", Run: `git config --global http.extraHeader "` + header + `"
if git ls-remote "$SELF" 2>err.txt; then exit 1; fi
cat err.txt
grep -q 'http.extraheader' err.txt`, Env: map[string]string{"SELF": g.url("o/r")}}
	spec := hostJob(1, step)
	spec.Source = g.source(1, "o/r", sha)
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if n := len(fs.issueRequests()); n != 1 {
		t.Errorf("签发了 %d 次，期望 1 次（只有拉取源码）", n)
	}
	if !strings.Contains(log, "拒绝这次 git 传输") || strings.Contains(log, "c2VjcmV0LWhlYWRlcg") {
		t.Errorf("日志中应有拒绝的说明、没有配置的值：\n%s", log)
	}
}

// V2 版本错配：新 Runner 遇只实现旧 RPC 的 Server，缓存命中、没有依赖也以"Server 版本过旧"失败，不调用 FetchCredentials。
func TestOldServerRejected(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	r := newTestRunner(t, fs)
	job := func(attempt int64) *runnerv1.JobSpec {
		spec := hostJob(attempt, bash("build", "true"))
		spec.Source = g.source(1, "o/r", sha)
		return spec
	}
	got, log := runJob(t, r, fs, job(1)) // 填充 bare 镜像
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	issued := len(fs.issueRequests())
	fs.mu.Lock()
	fs.legacy = true
	fs.mu.Unlock()
	got, log = runJob(t, r, fs, job(2))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classCredentials)
	if !strings.Contains(got.Message, errServerTooOld.Error()) {
		t.Errorf("结果说明 = %q", got.Message)
	}
	fs.mu.Lock()
	calls := fs.credCalls
	fs.mu.Unlock()
	if calls != 0 || len(fs.issueRequests()) != issued {
		t.Errorf("FetchCredentials 调用了 %d 次（期望 0），IssueGitToken 多调用了 %d 次", calls, len(fs.issueRequests())-issued)
	}
}

// V2：探测之后 Server 回滚（IssueGitToken 返回 Unimplemented），step 中这次 git 访问以同样的原因失败，不退回到旧方式。
func TestServerRolledBackAfterProbe(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	r := newTestRunner(t, fs)
	job := func(attempt int64, steps ...*runnerv1.Step) *runnerv1.JobSpec {
		spec := hostJob(attempt, steps...)
		spec.Source = g.source(1, "o/r", sha)
		return spec
	}
	got, log := runJob(t, r, fs, job(1, bash("build", "true")))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	fs.mu.Lock()
	fs.legacyGit = true
	fs.mu.Unlock()
	got, log = runJob(t, r, fs, job(2, &runnerv1.Step{Name: "fetch", Run: `git ls-remote "$SELF"`, Env: map[string]string{"SELF": g.url("o/r")}}))
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_FAILED, classUser)
	if !strings.Contains(log, "kiln-runner："+errServerTooOld.Error()) {
		t.Errorf("git 的输出中没有原因：\n%s", log)
	}
	fs.mu.Lock()
	calls := fs.credCalls
	fs.mu.Unlock()
	if calls != 0 {
		t.Errorf("FetchCredentials 调用了 %d 次", calls)
	}
}

// V6：job 退出与在途签发同时发生：退出时先关闭操作（只带 op），签发结果迟到时立即吊销（带 token）、不交出。
func TestJobExitRevokesLateToken(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, "")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	g.addRepo(t, "o/dep", map[string]string{"dep.txt": "dep\n"})
	r := newTestRunner(t, fs)
	entered := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	// 断言失败时也放行，免得 job 一直卡在签发上
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	revoked := make(chan *runnerv1.RevokeGitTokenRequest, 16)
	fs.mu.Lock()
	// token 已经交出、响应还在途时 job 退出（Server 不能再收回这枚 token，要由 Runner 带 token 吊销）
	fs.issueSentHook = func(m *runnerv1.IssueGitTokenRequest) {
		if m.Repository == "o/dep" {
			entered <- m.OpId
			<-release
		}
	}
	fs.revokeHook = func(m *runnerv1.RevokeGitTokenRequest) { revoked <- m }
	fs.mu.Unlock()
	spec := hostJob(1, &runnerv1.Step{Name: "fetch", Run: `git ls-remote "$DEP"`, Env: map[string]string{"DEP": g.url("o/dep")}})
	spec.Source = g.source(1, "o/r", sha)
	spec.Repositories = []string{"o/dep"}
	fs.register(spec)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.newJob(spec).run(ctx)
	}()
	op := waitFor(t, entered, "签发")
	cancel() // Runner 退出：step 被结束，job 结束
	revokeOf := func() *runnerv1.RevokeGitTokenRequest {
		for {
			if m := waitFor(t, revoked, "吊销"); m.OpId == op {
				return m
			}
		}
	}
	if m := revokeOf(); m.Token != "" {
		t.Fatalf("第一次关闭应只带 op（还没有收到 token），带了 token")
	}
	releaseOnce.Do(func() { close(release) })
	late := revokeOf()
	waitFor(t, done, "job 结束")
	var tok string
	for _, o := range opsFor(fs, "o/dep") {
		if o.op == op {
			tok = o.token
		}
	}
	if tok == "" || late.Token != tok {
		t.Errorf("迟到的 token 没有带 token 吊销（吊销请求带了 token：%t）", late.Token != "")
	}
	if slices.Contains(g.usedTokens(), tok) {
		t.Error("迟到的 token 被交给了 git")
	}
	got, log := completedJob(fs, 1)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_INFRA_ERROR, classRunnerShutdown)
	if !strings.Contains(log, "立即吊销、不交出") {
		t.Errorf("日志中没有迟到结果的说明：\n%s", log)
	}
}

// 外层 kiln-orca task exec / 凭据通道的私有变量不会继承到 step 的环境；workflow / step 用
// GIT_CONFIG_COUNT、GIT_CONFIG_KEY_<n> 追加配置的既有契约照常保留。
func TestGitAccessClearsOuterWrapper(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GIT_EXEC_PATH": "/outer", "KILN_ORCA_GIT_OP": "op", "KILN_GIT_OP": "op2", "KILN_GIT_OP_ERROR": "e",
		"KILN_GIT_SOCKET": "/s", "KILN_GIT_EXEC_REAL": "/r", "KILN_GIT_CONFIG_N": "4",
		"KILN_GIT_CONFIG_KEY_0": "k", "KILN_GIT_CONFIG_VALUE_0": "v", "KILN_GIT_CONFIG_KEY_7": "k7", "KILN_GIT_CONFIG_VALUE_7": "v7",
		"GIT_ASKPASS": "/ap", "SSH_ASKPASS": "/sap", "GIT_TERMINAL_PROMPT": "1",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "wf",
	}
	clearGitWrapperEnv(env)
	for _, k := range []string{
		"GIT_EXEC_PATH", "KILN_ORCA_GIT_OP", "KILN_GIT_OP", "KILN_GIT_OP_ERROR", "KILN_GIT_SOCKET", "KILN_GIT_EXEC_REAL", "KILN_GIT_CONFIG_N",
		"KILN_GIT_CONFIG_KEY_0", "KILN_GIT_CONFIG_VALUE_0", "KILN_GIT_CONFIG_KEY_7", "KILN_GIT_CONFIG_VALUE_7",
	} {
		if _, ok := env[k]; ok {
			t.Errorf("%s 没有被清掉", k)
		}
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "user.name" || env["GIT_CONFIG_VALUE_0"] != "wf" {
		t.Errorf("workflow / step 的 GIT_CONFIG_* 不应被清掉：%v", env)
	}
	// apply 先清掉污染，再注入本次合法的值
	env = map[string]string{"GIT_EXEC_PATH": "/outer", "KILN_ORCA_GIT_OP": "op", "KILN_GIT_SOCKET": "/old", "GIT_ASKPASS": "/ap", "GIT_TERMINAL_PROMPT": "1"}
	(&gitAccess{scope: "https://github.com", helper: "!h", socket: "/new", execPath: "/mirror", realExecPath: "/real"}).apply(env)
	if env["GIT_EXEC_PATH"] != "/mirror" || env[gitRealExecEnv] != "/real" || env[gitSocketEnv] != "/new" {
		t.Errorf("本次合法的值没有注入：%v", env)
	}
	for _, k := range []string{"KILN_ORCA_GIT_OP", "GIT_ASKPASS"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s 没有被清掉：%v", k, env)
		}
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q", env["GIT_TERMINAL_PROMPT"])
	}
	// 通道不可用（socket、execPath 为空）时同样清掉，且不残留 GIT_EXEC_PATH
	env = map[string]string{"GIT_EXEC_PATH": "/outer", "KILN_GIT_SOCKET": "/old", "GIT_ASKPASS": "/ap"}
	(&gitAccess{scope: "https://github.com", helper: "!h"}).apply(env)
	for _, k := range []string{"GIT_EXEC_PATH", gitSocketEnv, "GIT_ASKPASS"} {
		if _, ok := env[k]; ok {
			t.Errorf("通道不可用时 %s 仍然残留：%v", k, env)
		}
	}
}

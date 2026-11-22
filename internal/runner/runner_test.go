package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// waitTimeout 只是防止测试在出错时挂住的上限，正常情况下不会等这么久。
const waitTimeout = time.Minute

// recv 从 ch 中取一个值；超过 waitTimeout 时测试失败。
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("等待%s超时", what)
		panic("unreachable")
	}
}

// startRun 在后台运行 r.Run，返回取消函数和 Run 的返回值。
func startRun(t *testing.T, r *Runner) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- r.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, errc
}

// acquireSignal 让 fs 在收到第 n 次（从 1 开始）Acquire 请求时向返回的 channel 发送该请求。
func acquireSignal(fs *fakeServer, n int) <-chan *runnerv1.AcquireRequest {
	ch := make(chan *runnerv1.AcquireRequest, 1)
	var count atomic.Int32
	fs.mu.Lock()
	fs.acquireHook = func(req *runnerv1.AcquireRequest) {
		if count.Add(1) == int32(n) {
			ch <- req
		}
	}
	fs.mu.Unlock()
	return ch
}

// Run：长轮询领取 job 并执行，结果提交后继续领取；ctx 结束时等正在执行的 job 结束再返回。
func TestRunAcquiresAndExecutes(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	second := acquireSignal(fs, 2)
	spec := hostJob(1, bash("hi", "echo hello-from-run"))
	spec.Resources = &runnerv1.Resources{Cpu: 1, MemoryBytes: 1 << 30}
	fs.addJob(spec)
	cancel, errc := startRun(t, r)

	got := recv(t, fs.completed, "提交结果")
	recv(t, second, "第二次 Acquire")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got.Outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED || got.Ref.Epoch != testEpoch {
		t.Errorf("结果 = %v", got)
	}
	if !strings.Contains(fs.log(1), "hello-from-run") {
		t.Errorf("日志：%s", fs.log(1))
	}
	reqs := fs.acquireRequests()
	first := reqs[0]
	if first.MaxJobs != 2 || first.WaitSeconds != runnerkit.AcquireWaitSeconds || first.RequestId == "" {
		t.Errorf("第一次 Acquire = max_jobs %d、wait %d、request_id %q", first.MaxJobs, first.WaitSeconds, first.RequestId)
	}
	st := first.State
	if !slices.Equal(st.Platforms, []string{runnerkit.NativePlatform()}) || !slices.Equal(st.Executors, []string{"host"}) || st.RunningJobs != 0 {
		t.Errorf("上报的状态 = %v", st)
	}
	// 8 核 16GiB，保留 1 核 1GiB
	if st.Free.Cpu != 7 || st.Free.MemoryBytes != 15<<30 {
		t.Errorf("空闲资源 = %v", st.Free)
	}
	// 领到任务后换新的 request_id
	if reqs[1].RequestId == first.RequestId {
		t.Error("成功领取后应使用新的 request_id")
	}
}

// 槽位用满（max_jobs）时不再领取，等有 job 结束后再领取下一个。
func TestRunWaitsForFreeSlot(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs, func(c *Config) { c.MaxJobs = 1 })
	fs.addJob(hostJob(1, bash("one", "echo job-1")))
	fs.addJob(hostJob(2, bash("two", "echo job-2")))
	cancel, errc := startRun(t, r)
	first := recv(t, fs.completed, "第一个 job 的结果")
	second := recv(t, fs.completed, "第二个 job 的结果")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if first.Ref.AttemptId != 1 || second.Ref.AttemptId != 2 {
		t.Errorf("执行顺序 = %d、%d", first.Ref.AttemptId, second.Ref.AttemptId)
	}
	reqs := fs.acquireRequests()
	// 第一次只领到一个；第二次在第一个 job 结束后才发出
	if reqs[0].MaxJobs != 1 || reqs[1].MaxJobs != 1 || reqs[1].State.RunningJobs != 0 {
		t.Errorf("Acquire：max_jobs %d、%d，第二次时执行中 %d 个", reqs[0].MaxJobs, reqs[1].MaxJobs, reqs[1].State.RunningJobs)
	}
}

// Acquire 失败时退避重试，并保持同一个 request_id（Server 据此返回同一批任务）。
func TestRunAcquireRetryKeepsRequestID(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.acquireErrs = []error{connect.NewError(connect.CodeUnavailable, errors.New("暂时不可用"))}
	second := acquireSignal(fs, 2)
	cancel, errc := startRun(t, r)
	req := recv(t, second, "重试的 Acquire")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if first := fs.acquireRequests()[0]; req.RequestId != first.RequestId {
		t.Errorf("重试时 request_id 从 %q 变为 %q", first.RequestId, req.RequestId)
	}
}

// Runner 被吊销时 Run 返回 runnerkit.ErrRevoked：启动时认证失败，或领取时 Server 返回 RUNNER_REVOKED。
func TestRunRevoked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(fs *fakeServer)
	}{
		{name: "认证时", setup: func(fs *fakeServer) { fs.revoked = true }},
		{name: "领取时", setup: func(fs *fakeServer) { fs.acquireErrs = []error{rpcerr.RunnerRevoked()} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t)
			r := newTestRunner(t, fs)
			c.setup(fs)
			_, errc := startRun(t, r)
			if err := recv(t, errc, "Run 返回"); !errors.Is(err, runnerkit.ErrRevoked) {
				t.Fatalf("Run = %v，期望 runnerkit.ErrRevoked", err)
			}
		})
	}
}

// drain：本地 drain 文件存在时 Acquire 的 max_jobs 为 0（只上报状态），不领取新任务。
func TestRunDrainFile(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	if err := SetDrain(r.cfg, true); err != nil {
		t.Fatal(err)
	}
	fs.addJob(hostJob(1, bash("x", "echo should-not-run")))
	first := acquireSignal(fs, 1)
	cancel, errc := startRun(t, r)
	req := recv(t, first, "Acquire")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if req.MaxJobs != 0 {
		t.Errorf("drain 时 max_jobs = %d，期望 0", req.MaxJobs)
	}
	if n := len(fs.completeCalls()); n != 0 {
		t.Errorf("drain 时执行了 %d 个 job", n)
	}
}

// Server 下发 drain 指令时不再领取新任务，直到 ctx 结束。
func TestRunDrainDirective(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	fs.drain = true
	fs.addJob(hostJob(1, bash("x", "echo should-not-run")))
	first := acquireSignal(fs, 1)
	cancel, errc := startRun(t, r)
	recv(t, first, "Acquire")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := len(fs.completeCalls()); n != 0 {
		t.Errorf("drain 时执行了 %d 个 job", n)
	}
}

// start：同一个 attempt 已在执行时忽略；执行期间占用的资源计入上报的状态，结束后释放。
func TestRunnerStartDedupAndState(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "release")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(dir, "count")
	spec := hostJob(1, bash("wait", `echo run >> "$COUNT"; read -r _ < "$FIFO"`))
	spec.Env = map[string]string{"FIFO": fifo, "COUNT": count}
	spec.Resources = &runnerv1.Resources{Cpu: 2, MemoryBytes: 2 << 30}
	fs.register(spec)

	var wg sync.WaitGroup
	r.start(t.Context(), &wg, spec)
	r.start(t.Context(), &wg, spec) // 重复的 attempt
	st := r.stateMsg()
	if st.RunningJobs != 1 || len(st.Running) != 1 || st.Running[0].AttemptId != 1 {
		t.Errorf("执行中的 job = %d %v", st.RunningJobs, st.Running)
	}
	if st.Free.Cpu != 5 || st.Free.MemoryBytes != 13<<30 {
		t.Errorf("执行中的空闲资源 = %v", st.Free)
	}
	if n := r.freeSlots(); n != 1 {
		t.Errorf("freeSlots = %d，期望 1", n)
	}
	// 放行 step（阻塞到 step 打开 FIFO 为止）
	if err := os.WriteFile(fifo, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	recv(t, r.done, "job 结束的信号")
	if b, err := os.ReadFile(count); err != nil || string(b) != "run\n" {
		t.Errorf("step 执行次数：%q，%v", b, err)
	}
	st = r.stateMsg()
	if st.RunningJobs != 0 || st.Free.Cpu != 7 || st.Free.MemoryBytes != 15<<30 {
		t.Errorf("结束后的状态 = %v", st)
	}
}

func TestHostInfoState(t *testing.T) {
	t.Parallel()
	cfg := &Config{ReserveCPU: 2, ReserveMemory: "4Gi"}
	h := &hostInfo{platforms: []string{"darwin/arm64"}, executors: []string{"host"}, fingerprint: map[string]string{"macos": "15.5"}, totalCPU: 10, totalMem: 32 << 30}
	cases := []struct {
		name    string
		cpu     float64
		mem     int64
		wantCPU float64
		wantMem int64
	}{
		{name: "空闲", wantCPU: 8, wantMem: 28 << 30},
		{name: "部分占用", cpu: 3, mem: 8 << 30, wantCPU: 5, wantMem: 20 << 30},
		// 0 表示"未上报"，资源用满时上报极小值
		{name: "超出", cpu: 20, mem: 64 << 30, wantCPU: 0.001, wantMem: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := h.state(cfg, c.cpu, c.mem)
			if st.Free.Cpu != c.wantCPU || st.Free.MemoryBytes != c.wantMem {
				t.Errorf("free = %v，期望 cpu %v、memory %d", st.Free, c.wantCPU, c.wantMem)
			}
			if st.Version == "" || st.HostFingerprint["macos"] != "15.5" || st.Platforms[0] != "darwin/arm64" {
				t.Errorf("state = %v", st)
			}
		})
	}
}

// 能力探测（PLAN #8.2）：docker 可用时加上 oci（及 buildkit）和 oci 平台。
func TestDetectHost(t *testing.T) {
	t.Parallel()
	docker, _ := fakeDocker(t)
	linuxNative := "linux/" + runtime.GOARCH
	cases := []struct {
		name      string
		cfg       Config
		executors []string
		platforms []string
	}{
		{name: "关闭 docker", cfg: Config{Docker: dockerDisabled}, executors: []string{"host"}, platforms: []string{runnerkit.NativePlatform()}},
		{name: "docker 不可用", cfg: Config{Docker: filepath.Join(t.TempDir(), "no-docker")}, executors: []string{"host"}, platforms: []string{runnerkit.NativePlatform()}},
		{
			name:      "docker 可用",
			cfg:       Config{Docker: docker},
			executors: []string{"host", "oci"},
			platforms: dedup(runnerkit.NativePlatform(), linuxNative),
		},
		{
			name:      "开启 buildkit、指定 oci 平台",
			cfg:       Config{Docker: docker, Buildkit: true, OCIPlatforms: []string{"linux/amd64", "linux/arm64"}},
			executors: []string{"host", "oci", workflow.CapBuildkit},
			platforms: dedup(runnerkit.NativePlatform(), "linux/amd64", "linux/arm64"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.cfg.probeTimeout = testProbeTimeout
			h := detectHost(t.Context(), &c.cfg)
			if signingSupported() { // macOS 上总是可以执行 signing 的 job（PLAN #8.10）
				c.executors = append(slices.Clone(c.executors), workflow.CapSigning)
			}
			if !slices.Equal(h.executors, c.executors) {
				t.Errorf("executors = %v，期望 %v", h.executors, c.executors)
			}
			if !slices.Equal(h.platforms, c.platforms) {
				t.Errorf("platforms = %v，期望 %v", h.platforms, c.platforms)
			}
			if h.totalCPU < 1 || h.totalMem <= 0 {
				t.Errorf("资源 = %v CPU、%d 字节", h.totalCPU, h.totalMem)
			}
			for k, v := range h.fingerprint {
				if v == "" {
					t.Errorf("指纹 %s 为空，应删除", k)
				}
			}
		})
	}
}

// dedup 返回去掉重复项的列表（原生平台与 oci 平台相同时只出现一次）。
func dedup(items ...string) []string {
	var out []string
	for _, s := range items {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// 宿主机指纹（PLAN #8.2）：macOS 上有系统版本，Linux 上有发行版。
func TestHostFingerprint(t *testing.T) {
	t.Parallel()
	fp := runnerkit.HostFingerprint(t.Context())
	switch runtime.GOOS {
	case "darwin":
		if fp["macos"] == "" || fp["macos_build"] == "" {
			t.Errorf("macOS 指纹 = %v", fp)
		}
	case "linux":
		if fp["id"] == "" {
			t.Errorf("Linux 指纹 = %v", fp)
		}
	}
	if runnerkit.FirstLine("a\nb") != "a" || runnerkit.FirstLine("  x  ") != "x" {
		t.Error("firstLine")
	}
	if runnerkit.CmdOut(t.Context(), "kiln-no-such-command") != "" {
		t.Error("命令不存在时 cmdOut 应返回空字符串")
	}
}

// cmdOut 只取 stdout：stderr 上的警告不能混进探测到的值。
func TestCmdOutIgnoresStderr(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Warning: noise' >&2\necho ' 1.2.3 '\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runnerkit.CmdOutTimeout(t.Context(), testProbeTimeout, bin); got != "1.2.3" {
		t.Errorf("cmdOut = %q，想要 1.2.3", got)
	}
}

// Enroll 保存身份信息和私钥，New 之后用同一个私钥认证。
func TestEnrollAndNew(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Docker = dockerDisabled
	st, err := Enroll(t.Context(), cfg, fs.URL()+"/", "enroll-token", "mini-01")
	if err != nil {
		t.Fatal(err)
	}
	if *st != (State{Server: fs.URL(), RunnerID: 7, Name: "mini-01", Pool: "default"}) {
		t.Errorf("state = %+v", st)
	}
	for _, f := range []string{"runner.json", filepath.Join("keys", "runner.ed25519")} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s 的权限为 %o，期望 600", f, perm)
		}
	}
	// 注册请求带上公钥和本机状态
	fs.mu.Lock()
	enr := fs.enrolls[0]
	fs.mu.Unlock()
	if enr.Name != "mini-01" || len(enr.PublicKey) != 32 || enr.State == nil || len(enr.State.Platforms) == 0 {
		t.Errorf("注册请求 = %v", enr)
	}
	// config.toml 中保存了 server 和 name
	cfg2, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Server != fs.URL() || cfg2.Name != "mini-01" {
		t.Errorf("config = server %q、name %q", cfg2.Server, cfg2.Name)
	}
	cfg2.Docker = dockerDisabled
	r, err := New(t.Context(), cfg2, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Authenticate(t.Context()); err != nil {
		t.Fatalf("用注册时的私钥认证失败：%v", err)
	}
	for _, d := range []string{casDirName, rtDirName, gitDirName, workDirName, toolcacheDirName} {
		if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
			t.Errorf("New 没有创建 %s：%v", d, err)
		}
	}
	// 再次注册沿用同一个私钥
	if _, err := Enroll(t.Context(), cfg2, fs.URL(), "enroll-token", "mini-01"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	same := string(fs.enrolls[0].PublicKey) == string(fs.enrolls[1].PublicKey)
	fs.mu.Unlock()
	if !same {
		t.Error("再次注册时私钥变了")
	}
}

func TestEnrollErrors(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	cfg := &Config{Dir: t.TempDir(), Docker: dockerDisabled}
	if _, err := Enroll(t.Context(), cfg, fs.URL(), "wrong-token", "x"); err == nil || !strings.Contains(err.Error(), "注册失败") {
		t.Errorf("令牌错误时 Enroll = %v", err)
	}
	if _, err := cfg.LoadState(); err == nil {
		t.Error("注册失败时不应保存身份信息")
	}
	// 不给名称时用主机名（去掉域名部分）
	st, err := Enroll(t.Context(), cfg, fs.URL(), "enroll-token", "")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if want, _, _ := strings.Cut(host, "."); st.Name != want {
		t.Errorf("name = %q，期望 %q", st.Name, want)
	}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	cfg := &Config{Dir: t.TempDir(), Docker: dockerDisabled}
	if _, err := New(t.Context(), cfg, log); err == nil || !strings.Contains(err.Error(), "还没有注册") {
		t.Errorf("未注册时 New = %v", err)
	}
	if err := cfg.home().SaveState(&State{RunnerID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), cfg, log); err == nil || !strings.Contains(err.Error(), "读取私钥") {
		t.Errorf("没有私钥时 New = %v", err)
	}
}

// GC 只删除超过保留期（workRetention）的工作区（保留的失败现场），其中的只读目录也能删除。
func TestGCCleansOldWork(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir(), Docker: dockerDisabled, DiskHighWatermark: 100, DiskLowWatermark: 99}
	log := slog.New(slog.DiscardHandler)
	if rep := GC(t.Context(), cfg, GCOptions{}, log); rep.Err() != nil { // 工作区目录还不存在：什么也不做
		t.Fatal(rep.Err())
	}
	old := cfg.path(workDirName, "1")
	fresh := cfg.path(workDirName, "2")
	mustWrite(t, filepath.Join(old, "ws", "ro", "f"), "x")
	mustWrite(t, filepath.Join(fresh, "ws", "f"), "x")
	if err := os.Chmod(filepath.Join(old, "ws", "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-workRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if rep := GC(t.Context(), cfg, GCOptions{}, log); rep.Err() != nil {
		t.Fatal(rep.Err())
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("过期的工作区没有删除：%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("未过期的工作区被删除：%v", err)
	}
}

// kiln-runner drain 写入本地 drain 标记，Run 据此不再领取新任务（TestRunDrainFile）。
func TestSetDrain(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir()}
	if err := SetDrain(cfg, true); err != nil || !cfg.home().Draining() {
		t.Fatalf("SetDrain(true) = %v，Draining %v", err, cfg.home().Draining())
	}
	if err := SetDrain(cfg, false); err != nil || cfg.home().Draining() {
		t.Fatalf("SetDrain(false) = %v，Draining %v", err, cfg.home().Draining())
	}
}

func TestRepoLock(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	a, b := r.repoLock(1), r.repoLock(1)
	if a != b {
		t.Error("同一个仓库应返回同一把锁")
	}
	if r.repoLock(1) == r.repoLock(2) {
		t.Error("不同仓库应返回不同的锁")
	}
}

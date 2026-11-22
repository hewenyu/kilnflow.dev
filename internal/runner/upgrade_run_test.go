package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit/runnerkittest"
)

// 自升级在 CI Runner 领取循环中的行为（PLAN #27.7）。升级机制本身的测试在 runnerkit 中。

// testUpgrader 返回一个由启动器启动、版本为 version 的 Upgrader；execFn 代替切换时的 exec。
func testUpgrader(r *Runner, version string, execFn func(string, []string, []string) error) *runnerkit.Upgrader {
	via := true
	return runnerkit.NewUpgrader(r.cfg.home(), true, slog.New(slog.DiscardHandler), runnerkit.UpgraderOptions{
		Version: version, ViaLauncher: &via, Exec: execFn,
	})
}

// TestRunTrialConfirmBeforeJobs（PLAN #27.7 第 5 步，独立审查发现）：试用的新版本第一次 Acquire 不领取任务，确认写进
// state.json 之后才领取；确认写不进去时退出（由启动器计一次启动），不在没有确认的版本上执行任务。
func TestRunTrialConfirmBeforeJobs(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	setup := func(t *testing.T) (*fakeServer, *Runner) {
		t.Helper()
		fs := newFakeServer(t)
		r := newTestRunner(t, fs)
		runnerkittest.InstallLayout(t, r.cfg.Dir, Program.Name,
			&runnerkit.BootState{Current: "1.0.0", Trial: &runnerkit.BootTrial{Version: "1.1.0", Boots: 1, Generation: 2}},
			"1.0.0", "1.1.0")
		r.up = testUpgrader(r, "v1.1.0", nil)
		fs.addJob(hostJob(1, bash("hi", "echo trial-job")))
		return fs, r
	}

	t.Run("确认后领取", func(t *testing.T) {
		t.Parallel()
		fs, r := setup(t)
		cancel, errc := startRun(t, r)
		recv(t, fs.completed, "提交结果")
		cancel()
		if err := recv(t, errc, "Run 返回"); err != nil {
			t.Fatalf("Run = %v", err)
		}
		reqs := fs.acquireRequests()
		if reqs[0].MaxJobs != 0 || reqs[1].MaxJobs == 0 {
			t.Fatalf("确认之前不应领取任务：max_jobs %d、%d", reqs[0].MaxJobs, reqs[1].MaxJobs)
		}
		if st, err := runnerkit.LoadBootState(r.cfg.Dir, Program.Name); err != nil || st.Current != "1.1.0" || st.Trial != nil {
			t.Fatalf("应已确认：%+v（%v）", st, err)
		}
	})

	t.Run("确认失败时退出", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root 不受目录权限限制")
		}
		fs, r := setup(t)
		binDir := filepath.Dir(filepath.Dir(runnerkit.LauncherPath(r.cfg.Dir, Program.Name)))
		if err := os.Chmod(binDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(binDir, 0o755) })
		_, errc := startRun(t, r)
		err := recv(t, errc, "Run 返回")
		if err == nil || !strings.Contains(err.Error(), "确认新版本") {
			t.Fatalf("确认失败时应退出：%v", err)
		}
		if reqs := fs.acquireRequests(); len(reqs) != 1 || reqs[0].MaxJobs != 0 {
			t.Fatalf("确认之前不应领取任务：%d 次 Acquire", len(reqs))
		}
		if n := len(fs.completeCalls()); n != 0 {
			t.Fatalf("没有确认的版本不应执行任务：%d 个结果", n)
		}
	})
}

// runUpgradeSetup 在 r 的数据目录中建立升级布局（current 1.0.0，1.1.0 已经下载好、不需要再下载），让 fs 每次 Acquire
// 都下发升级到 1.1.0 的指令。返回的 channel 在每次切换（exec 启动器，被测试替换为失败）时收到一个值。
func runUpgradeSetup(t *testing.T, r *Runner, fs *fakeServer) <-chan struct{} {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("没有 sh")
	}
	runnerkittest.InstallLayout(t, r.cfg.Dir, Program.Name, &runnerkit.BootState{Current: "1.0.0"}, "1.0.0", "1.1.0")
	execs := make(chan struct{}, 64)
	r.up = testUpgrader(r, "v1.0.0", func(string, []string, []string) error {
		select {
		case execs <- struct{}{}:
		default:
		}
		return errors.New("exec 被测试替换")
	})
	r.idlePoll = 20 * time.Millisecond // 排空期间每一轮都会检查是否可以切换
	fs.mu.Lock()
	fs.upgrade = &runnerv1.UpgradeDirective{Version: "1.1.0", Generation: 1}
	fs.mu.Unlock()
	return execs
}

// waitUpgradeState 等待 r 的升级状态变为 state。
func waitUpgradeState(t *testing.T, r *Runner, state string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for r.up.StatusMsg().State != state {
		if time.Now().After(deadline) {
			t.Fatalf("升级状态没有变为 %s：%+v", state, r.up.StatusMsg())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noSwitch 断言一段时间内没有切换。
func noSwitch(t *testing.T, execs <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-execs:
		t.Fatalf("%s时不应切换", what)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRunUpgradeWaitsForJobs（U4，经过 Run 的领取循环，独立审查发现）：新版本准备好之后排空，正在执行的 job 结束、结果
// 提交之后才切换（exec 启动器）。
func TestRunUpgradeWaitsForJobs(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	execs := runUpgradeSetup(t, r, fs)
	gate := filepath.Join(t.TempDir(), "go")
	fs.addJob(hostJob(1, bash("wait", fmt.Sprintf("while [ ! -f %q ]; do sleep 0.05; done; echo done", gate))))
	cancel, errc := startRun(t, r)
	waitUpgradeState(t, r, runnerkit.UpgradeDraining)
	if n := len(fs.completeCalls()); n != 0 {
		t.Fatalf("job 应还在执行：%d 个结果", n)
	}
	noSwitch(t, execs, "job 还在执行")
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := recv(t, fs.completed, "提交结果")
	recv(t, execs, "job 结束后切换")
	cancel()
	if err := recv(t, errc, "Run 返回"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got.Outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED {
		t.Fatalf("job 的结果 = %v（切换不应打断 job）", got)
	}
}

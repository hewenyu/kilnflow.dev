package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/runner"
)

// 所有用例共用一个损坏的 config.toml：不需要配置的命令不应受影响。
func TestRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("this is = = not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_RUNNER_DIR", dir)

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "无参数", args: nil, wantCode: exitUsage, wantStderr: "用法：kiln-runner"},
		{name: "未知命令", args: []string{"bogus"}, wantCode: exitUsage, wantStderr: "未知的命令 bogus"},
		{name: "version 不读配置", args: []string{"version"}, wantCode: exitOK, wantStdout: "kiln-runner"},
		{name: "help 不读配置", args: []string{"--help"}, wantCode: exitOK, wantStdout: "用法：kiln-runner"},
		{name: "service 缺少子命令", args: []string{"service"}, wantCode: exitUsage, wantStderr: "service install"},
		{name: "service 未知子命令", args: []string{"service", "bogus"}, wantCode: exitUsage, wantStderr: "未知的子命令"},
		{name: "drain 配置损坏", args: []string{"drain"}, wantCode: exitError, wantStderr: "config.toml"},
		{name: "enroll 配置损坏", args: []string{"enroll", "--token", "x"}, wantCode: exitError, wantStderr: "config.toml"},
		{name: "enroll -h 不读配置", args: []string{"enroll", "-h"}, wantCode: exitOK, wantStderr: "-server"},
		{name: "toolchain info 不读配置", args: []string{"toolchain", "info"}, wantCode: exitOK, wantStdout: `{"format":1,"version":"`},
		{name: "toolchain 缺少子命令", args: []string{"toolchain"}, wantCode: exitUsage, wantStderr: "需要子命令"},
		{name: "toolchain ensure 缺少 --root", args: []string{"toolchain", "ensure"}, wantCode: exitUsage, wantStderr: "需要 --root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stderr：%s", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q，应包含 %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestEnrollUsage(t *testing.T) {
	t.Setenv("KILN_RUNNER_DIR", t.TempDir())
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "缺少 token", args: []string{"enroll", "--server", "https://kiln.example.com"}, wantCode: exitUsage},
		{name: "未知参数", args: []string{"enroll", "--bogus"}, wantCode: exitUsage},
		{name: "多余位置参数", args: []string{"enroll", "--server", "s", "--token", "t", "extra"}, wantCode: exitUsage},
		{name: "-h", args: []string{"enroll", "-h"}, wantCode: exitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stderr：%s", code, tt.wantCode, stderr.String())
			}
		})
	}
}

// gc 只清理数据目录中的工作区，未注册的机器上也能执行。
func TestGCWithoutEnroll(t *testing.T) {
	t.Setenv("KILN_RUNNER_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"gc"}, &stdout, &stderr); code != exitOK {
		t.Errorf("退出码 = %d，期望 %d；stderr：%s", code, exitOK, stderr.String())
	}
}

// gc 删除超过 12 小时的工作区，保留新的。磁盘用量注入为空闲（低于水位），只验证按时间的清理，与测试机器的磁盘占用无关
// （按磁盘水位回收见 runner 的 TestEvictDiskWatermark）。
func TestGCRemovesOldWorkspaces(t *testing.T) {
	dir := dataDir(t, "")
	old, fresh := filepath.Join(dir, "work", "1"), filepath.Join(dir, "work", "2")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Join(d, "ws"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-13 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	a := &app{stdout: &out, stderr: &errOut, runGC: func(ctx context.Context, c *runner.Config, opts runner.GCOptions, log *slog.Logger) *runner.GCReport {
		if !opts.Evict {
			t.Error("服务没有运行时应按容量回收")
		}
		opts.DiskUsage = func(string) (total, free int64, ok bool) { return 1 << 40, 1 << 40, true }
		return runner.GC(ctx, c, opts, log)
	}}
	if code := a.exitCode(a.execute(t.Context(), []string{"gc"})); code != exitOK {
		t.Fatalf("退出码 = %d；stderr：%s", code, errOut.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("过期的工作区没有删除：%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("新的工作区被删除：%v", err)
	}
}

// 服务正在运行（数据目录被锁住）时 gc 只按保留期清理并给出说明；服务没有运行时 gc 结束后释放锁。
func TestGCWhileServiceRunning(t *testing.T) {
	dir := dataDir(t, "")
	cfg, err := runner.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, running, err := runner.TryLockDataDir(cfg)
	if err != nil || running {
		t.Fatalf("TryLockDataDir = %v %v", running, err)
	}
	code, stdout, stderr := runCmd("gc")
	unlock()
	if code != exitOK || !strings.Contains(stdout, "服务正在运行") {
		t.Errorf("退出码 = %d，stdout = %q，stderr = %q", code, stdout, stderr)
	}

	if code, _, stderr := runCmd("gc"); code != exitOK {
		t.Fatalf("退出码 = %d；stderr：%s", code, stderr)
	}
	unlock, running, err = runner.TryLockDataDir(cfg)
	if err != nil || running {
		t.Errorf("gc 结束后应释放数据目录的锁：%v %v", running, err)
	}
	if unlock != nil {
		unlock()
	}
}

// 服务没有运行时 gc 在整个清理期间持有数据目录的锁：清理进行中服务无法启动（不会与按容量回收、残留容器的清理同时进行）。
func TestGCHoldsLockDuringCleanup(t *testing.T) {
	dir := dataDir(t, "")
	cfg, err := runner.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	called := false
	a := &app{stdout: &out, stderr: &errOut, runGC: func(ctx context.Context, c *runner.Config, opts runner.GCOptions, log *slog.Logger) *runner.GCReport {
		called = true
		// 在清理期间从另一个文件描述符取锁：应该已经被 gc 锁住
		unlock, running, err := runner.TryLockDataDir(cfg)
		if unlock != nil {
			unlock()
		}
		if err != nil || !running {
			t.Errorf("清理期间 TryLockDataDir = running %v，%v；数据目录应被 gc 锁住", running, err)
		}
		if !opts.Evict {
			t.Error("服务没有运行时应按容量回收")
		}
		return runner.GC(ctx, c, opts, log)
	}}
	if code := a.exitCode(a.execute(t.Context(), []string{"gc"})); code != exitOK {
		t.Fatalf("退出码 = %d；stderr：%s", code, errOut.String())
	}
	if !called {
		t.Fatal("没有执行清理")
	}
	unlock, running, err := runner.TryLockDataDir(cfg)
	if err != nil || running {
		t.Errorf("gc 结束后应释放数据目录的锁：%v %v", running, err)
	}
	if unlock != nil {
		unlock()
	}
}

// enroll：注册成功后保存身份信息并提示下一步；--server 可以省略（取 config.toml 中的 server）。
func TestEnroll(t *testing.T) {
	fs, url := newFakeServer(t)
	t.Run("参数给出 server", func(t *testing.T) {
		dir := dataDir(t, "")
		code, stdout, stderr := runCmd("enroll", "--server", url, "--token", "good-token", "--name", "mini-01")
		if code != exitOK {
			t.Fatalf("退出码 = %d；stderr：%s", code, stderr)
		}
		if !strings.Contains(stdout, "已注册：mini-01（id 7，pool default）") || !strings.Contains(stdout, "配置在 "+dir) {
			t.Errorf("stdout = %q", stdout)
		}
		if _, err := os.Stat(filepath.Join(dir, "runner.json")); err != nil {
			t.Errorf("没有保存身份信息：%v", err)
		}
	})
	t.Run("server 来自 config.toml", func(t *testing.T) {
		dataDir(t, "server = \""+url+"\"\n")
		if code, _, stderr := runCmd("enroll", "--token", "good-token", "--name", "mini-02"); code != exitOK {
			t.Fatalf("退出码 = %d；stderr：%s", code, stderr)
		}
	})
	t.Run("令牌无效", func(t *testing.T) {
		dataDir(t, "")
		code, _, stderr := runCmd("enroll", "--server", url, "--token", "bad-token")
		if code != exitError || !strings.Contains(stderr, "注册失败") {
			t.Errorf("退出码 = %d，stderr = %q", code, stderr)
		}
	})
	fs.mu.Lock()
	n := len(fs.enrolls)
	fs.mu.Unlock()
	if n != 3 {
		t.Errorf("Server 收到 %d 次注册，期望 3 次", n)
	}
}

// drain / undrain 设置和清除数据目录中的 drain 标记。
func TestDrainUndrain(t *testing.T) {
	dir := dataDir(t, "")
	marker := filepath.Join(dir, "drain")
	code, stdout, _ := runCmd("drain")
	if code != exitOK || !strings.Contains(stdout, "已设置 drain") {
		t.Fatalf("drain：%d %q", code, stdout)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("没有 drain 标记：%v", err)
	}
	code, stdout, _ = runCmd("undrain")
	if code != exitOK || !strings.Contains(stdout, "已恢复领取任务") {
		t.Fatalf("undrain：%d %q", code, stdout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("drain 标记没有删除：%v", err)
	}
}

// 需要注册的命令在未注册时退出码为 1，并提示先注册；service install 不会调用 launchctl / systemctl。
func TestCommandsRequireEnroll(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"service", "install"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dataDir(t, "")
			code, _, stderr := runCmd(args...)
			if code != exitError || !strings.Contains(stderr, "kiln-runner enroll") {
				t.Errorf("退出码 = %d，stderr = %q", code, stderr)
			}
		})
	}
}

// doctor 有检查项不通过时退出码为 1。
func TestDoctorNotEnrolled(t *testing.T) {
	dataDir(t, "")
	code, stdout, stderr := runCmd("doctor")
	if code != exitError || !strings.Contains(stderr, "运行环境检查未通过") {
		t.Errorf("退出码 = %d，stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "✗ 注册") || !strings.Contains(stdout, "✓ git") {
		t.Errorf("stdout = %q", stdout)
	}
}

// run：Runner 已被吊销时退出码为 0（避免服务管理器反复重启），日志说明需要重新注册。
func TestRunRevoked(t *testing.T) {
	fs, url := newFakeServer(t)
	dataDir(t, "")
	if code, _, stderr := runCmd("enroll", "--server", url, "--token", "good-token", "--name", "x"); code != exitOK {
		t.Fatalf("enroll：%d %s", code, stderr)
	}
	fs.mu.Lock()
	fs.revoked = true
	fs.mu.Unlock()
	code, _, stderr := runCmd("run")
	if code != exitOK || !strings.Contains(stderr, "已被吊销") {
		t.Errorf("退出码 = %d，stderr = %q", code, stderr)
	}
}

// KILN_LOG_FORMAT 无效是用法错误。
func TestLogFormatInvalid(t *testing.T) {
	for _, cmd := range []string{"run", "gc"} {
		t.Run(cmd, func(t *testing.T) {
			dataDir(t, "")
			t.Setenv("KILN_LOG_FORMAT", "xml")
			if code, _, stderr := runCmd(cmd); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d；stderr：%s", code, exitUsage, stderr)
			}
		})
	}
}

func TestVersionAndHelp(t *testing.T) {
	dataDir(t, "")
	for _, arg := range []string{"version", "--version", "-v"} {
		code, stdout, _ := runCmd(arg)
		if code != exitOK || stdout != "kiln-runner "+buildinfo.Version()+"\n" {
			t.Errorf("%s：%d %q", arg, code, stdout)
		}
	}
	for _, arg := range []string{"help", "-h", "--help"} {
		code, stdout, _ := runCmd(arg)
		if code != exitOK || stdout != usageText() {
			t.Errorf("%s：%d %q", arg, code, stdout)
		}
	}
}

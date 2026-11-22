package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
)

type result struct {
	code           int
	stdout, stderr string
}

// runApp 用临时配置文件执行一次 kiln，不受本机登录状态和环境变量影响。
func runApp(t *testing.T, configPath, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(stdin), &stdout, &stderr)
	a.configPath = configPath
	code := a.exitCode(t.Context(), a.execute(t.Context(), args))
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("KILN_SERVER", "")
	t.Setenv("KILN_ADMIN_TOKEN", "")
	return filepath.Join(t.TempDir(), "kiln", "cli.json")
}

func TestDispatch(t *testing.T) {
	cfg := isolate(t)
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "无参数输出用法", args: nil, wantCode: exitUsage, wantStderr: "用法：kiln"},
		{name: "未知命令", args: []string{"bogus"}, wantCode: exitUsage, wantStderr: "未知的命令 bogus"},
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "kiln " + buildinfo.Version()},
		{name: "-v", args: []string{"-v"}, wantCode: exitOK, wantStdout: "kiln " + buildinfo.Version()},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "用法：kiln"},
		{name: "run 缺少 ID", args: []string{"run"}, wantCode: exitUsage, wantStderr: "kiln run RUN_ID"},
		{name: "run 非数字 ID", args: []string{"run", "abc"}, wantCode: exitUsage, wantStderr: "需要数字 ID"},
		{name: "未知 flag", args: []string{"runs", "--bogus"}, wantCode: exitUsage, wantStderr: "bogus"},
		{name: "int32 越界", args: []string{"runs", "-n", "99999999999"}, wantCode: exitUsage, wantStderr: "超出范围"},
		{name: "子命令 -h", args: []string{"runs", "-h"}, wantCode: exitOK, wantStdout: "-repo"},
		{name: "未登录", args: []string{"runs"}, wantCode: exitError, wantStderr: "还没有登录"},
		{name: "cache 缺少参数", args: []string{"cache"}, wantCode: exitUsage, wantStderr: "cache bump REPO"},
		{name: "secret 缺少子命令", args: []string{"secret"}, wantCode: exitUsage, wantStderr: "secret set|list|delete"},
		{name: "secret set 缺少作用域", args: []string{"secret", "set", "NAME"}, wantCode: exitUsage, wantStderr: "--account 或 --repo"},
		{name: "var set 缺少值", args: []string{"var", "set", "NAME", "--repo", "o/r"}, wantCode: exitUsage, wantStderr: "var set NAME VALUE"},
		{name: "env set 缺少作用域", args: []string{"env", "set", "prod"}, wantCode: exitUsage, wantStderr: "--account 或 --repo"},
		{name: "dispatch 输入格式", args: []string{"dispatch", "o/r", "ci.yml", "-i", "novalue"}, wantCode: exitUsage, wantStderr: "key=value"},
		{name: "runtime 未知子命令", args: []string{"runtime", "x"}, wantCode: exitUsage, wantStderr: "runtime lock"},
		{name: "login 缺少 server", args: []string{"login"}, wantCode: exitUsage, wantStderr: "--server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runApp(t, cfg, "", tt.args...)
			if got.code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stderr：%s", got.code, tt.wantCode, got.stderr)
			}
			if !strings.Contains(got.stdout, tt.wantStdout) {
				t.Errorf("stdout = %q，应包含 %q", got.stdout, tt.wantStdout)
			}
			if !strings.Contains(got.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.wantStderr)
			}
		})
	}
}

func TestCorruptConfig(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runApp(t, cfg, "", "runs")
	if got.code != exitError || !strings.Contains(got.stderr, "已损坏") {
		t.Errorf("退出码 = %d，stderr = %q；期望 1 且提示配置已损坏", got.code, got.stderr)
	}
}

func TestExitCode(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		wantCode   int
		wantStderr string
	}{
		{name: "成功", ctx: t.Context(), err: nil, wantCode: exitOK},
		{name: "help", ctx: t.Context(), err: fmt.Errorf("x: %w", flag.ErrHelp), wantCode: exitOK},
		{name: "用法错误", ctx: t.Context(), err: usageErrorf("坏参数"), wantCode: exitUsage, wantStderr: "错误：坏参数"},
		{name: "普通错误", ctx: t.Context(), err: errors.New("失败"), wantCode: exitError, wantStderr: "错误：失败"},
		{
			name: "connect 错误保留错误码", ctx: t.Context(),
			err:      fmt.Errorf("登录失败：%w", connect.NewError(connect.CodePermissionDenied, errors.New("不允许"))),
			wantCode: exitError, wantStderr: "错误：登录失败：不允许（permission_denied）",
		},
		{name: "被中断", ctx: canceled, err: context.Canceled, wantCode: exitInterrupted, wantStderr: "已中断"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			a := newApp(strings.NewReader(""), &bytes.Buffer{}, &stderr)
			if got := a.exitCode(tt.ctx, tt.err); got != tt.wantCode {
				t.Errorf("exitCode() = %d，期望 %d", got, tt.wantCode)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestParseInterleaved(t *testing.T) {
	t.Parallel()
	a := &app{stdout: &bytes.Buffer{}}
	fs := newFlags("t")
	f := fs.Bool("f", false, "")
	pos, err := a.parse(fs, []string{"12", "-f", "34"})
	if err != nil {
		t.Fatal(err)
	}
	if !*f || len(pos) != 2 || pos[0] != "12" || pos[1] != "34" {
		t.Errorf("parse() = %q, f = %v；期望 [12 34], true", pos, *f)
	}
}

// run 是 main 的入口：返回退出码而不是直接退出。
func TestRunEntry(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", code, stderr.String())
	}
	if want := "kiln " + buildinfo.Version() + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q，期望 %q", stdout.String(), want)
	}
	stdout.Reset()
	if code := run([]string{"nope"}, strings.NewReader(""), &stdout, &stderr); code != exitUsage {
		t.Errorf("未知命令的退出码 = %d，期望 %d", code, exitUsage)
	}
}

// 用法说明列出了每个命令（help、version 等别名除外）。
func TestUsageListsCommands(t *testing.T) {
	t.Parallel()
	a := newApp(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	for name := range a.commands() {
		if strings.HasPrefix(name, "-") || name == "help" || name == "version" {
			continue
		}
		if !strings.Contains(usage, "  "+name+" ") && !strings.Contains(usage, "  "+name+"\n") {
			t.Errorf("usage 中没有命令 %s", name)
		}
	}
}

// 需要 Server 的命令在未登录时都返回 1 并提示 kiln login，用法错误优先于未登录。
func TestCommandsRequireLogin(t *testing.T) {
	cfg := isolate(t)
	tests := [][]string{
		{"runner", "token"},
		{"runner", "list"},
		{"runner", "drain", "r1"},
		{"runner", "undrain", "r1"},
		{"runner", "revoke", "r1"},
		{"repo", "list"},
		{"repo", "sync"},
		{"repo", "reconcile", "o/r"},
		{"cache", "bump", "o/r"},
		{"runs"},
		{"run", "1"},
		{"logs", "1"},
		{"cancel", "1"},
		{"rerun", "1"},
		{"rerun-job", "1"},
		{"dispatch", "o/r", "ci.yml"},
		{"secret", "set", "S", "--repo", "o/r"},
		{"secret", "list", "--repo", "o/r"},
		{"secret", "delete", "S", "--repo", "o/r"},
		{"var", "set", "V", "x", "--repo", "o/r"},
		{"var", "list", "--account", "a"},
		{"var", "delete", "V", "--repo", "o/r"},
		{"env", "set", "prod", "--repo", "o/r"},
		{"env", "list"},
		{"env", "delete", "prod", "--repo", "o/r"},
		{"outbox"},
		{"outbox", "retry", "1"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := runApp(t, cfg, "value\n", args...)
			if got.code != exitError || !strings.Contains(got.stderr, "还没有登录") {
				t.Errorf("退出码 = %d，stderr = %q；期望 1 并提示登录", got.code, got.stderr)
			}
		})
	}
}

func TestFormatError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "普通错误原样输出", err: errors.New("磁盘已满"), want: "磁盘已满"},
		{name: "connect 错误显示错误码", err: connect.NewError(connect.CodeNotFound, errors.New("run 不存在")), want: "run 不存在（not_found）"},
		{name: "connect 错误没有消息", err: connect.NewError(connect.CodeUnavailable, nil), want: "请求失败（unavailable）"},
		{name: "保留外层上下文", err: fmt.Errorf("访问 Server：%w", connect.NewError(connect.CodeInternal, errors.New("坏了"))), want: "访问 Server：坏了（internal）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatError(tt.err); got != tt.want {
				t.Errorf("formatError() = %q，期望 %q", got, tt.want)
			}
		})
	}
}

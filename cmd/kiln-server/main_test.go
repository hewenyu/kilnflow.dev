package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version 命令", args: []string{"version"}, wantCode: exitOK, wantStdout: "kiln-server"},
		{name: "--version", args: []string{"--version"}, wantCode: exitOK, wantStdout: "kiln-server"},
		{name: "-v", args: []string{"-v"}, wantCode: exitOK, wantStdout: "kiln-server"},
		{name: "help 命令", args: []string{"help"}, wantCode: exitOK, wantStdout: "KILN_DATABASE_URL"},
		{name: "--help", args: []string{"--help"}, wantCode: exitOK, wantStdout: "KILN_DATABASE_URL"},
		{name: "拼错的参数", args: []string{"--env-flie", "x"}, wantCode: exitUsage, wantStderr: "env-flie"},
		{name: "--env-file 缺少值", args: []string{"--env-file"}, wantCode: exitUsage, wantStderr: "env-file"},
		{name: "多余的位置参数", args: []string{"serve"}, wantCode: exitUsage, wantStderr: "多余的参数"},
		{name: "env 文件不存在", args: []string{"--env-file", filepath.Join(t.TempDir(), "missing.env")}, wantCode: exitUsage, wantStderr: "读取 env 文件"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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

// TestUsageCoversConfig 检查 usage 和 deploy/server.env.example 列出了 server 读取的所有环境变量。
func TestUsageCoversConfig(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "server", "config.go"))
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "server.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	vars := regexp.MustCompile(`"(KILN_[A-Z_]+?)(?:_FILE)?"`).FindAllStringSubmatch(string(src), -1)
	if len(vars) == 0 {
		t.Fatal("config.go 中没有找到环境变量")
	}
	for _, m := range vars {
		if !strings.Contains(usage, m[1]) {
			t.Errorf("usage 中缺少 %s", m[1])
		}
		if !strings.Contains(string(example), m[1]) {
			t.Errorf("deploy/server.env.example 中缺少 %s", m[1])
		}
	}
}

// masterKey 是合法的 32 字节主密钥（hex）。
var masterKey = strings.Repeat("ab", 32)

// clearEnv 清除进程中所有 KILN_ 开头的环境变量，以及 extra 中的变量；测试结束后恢复原值。
// --env-file 通过 os.Setenv 设置变量，所以它会设置的变量也要先登记到 extra 中，测试结束后才会被清除。
func clearEnv(t *testing.T, extra ...string) {
	t.Helper()
	keys := extra
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "KILN_") {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		t.Setenv(k, "") // 登记恢复
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConfigErrors 覆盖配置错误：退出码 2，错误信息说明缺少或无效的配置；不会尝试连接数据库。
func TestConfigErrors(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantStderr string
	}{
		{name: "日志格式无效", env: map[string]string{"KILN_LOG_FORMAT": "xml"}, wantStderr: `KILN_LOG_FORMAT="xml" 无效`},
		{name: "缺少数据库地址", env: map[string]string{}, wantStderr: "需要设置 KILN_DATABASE_URL"},
		{
			name:       "admin token 太短",
			env:        map[string]string{"KILN_DATABASE_URL": "postgres://x", "KILN_ADMIN_TOKEN": "short"},
			wantStderr: "KILN_ADMIN_TOKEN",
		},
		{
			name:       "缺少主密钥",
			env:        map[string]string{"KILN_DATABASE_URL": "postgres://x", "KILN_ADMIN_TOKEN": strings.Repeat("t", 16)},
			wantStderr: "KILN_MASTER_KEY",
		},
		{
			name:       "主密钥长度不对",
			env:        map[string]string{"KILN_DATABASE_URL": "postgres://x", "KILN_ADMIN_TOKEN": strings.Repeat("t", 16), "KILN_MASTER_KEY": "abcd"},
			wantStderr: "master key 应为 32 字节",
		},
		{
			name: "设置了 App ID 但没有私钥",
			env: map[string]string{
				"KILN_DATABASE_URL": "postgres://x", "KILN_ADMIN_TOKEN": strings.Repeat("t", 16), "KILN_MASTER_KEY": masterKey,
				"KILN_GITHUB_APP_ID": "123",
			},
			wantStderr: "KILN_GITHUB_PRIVATE_KEY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			if code := run(nil, &stdout, &stderr); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d；stderr = %s", code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// 配置错误通过日志输出，KILN_LOG_FORMAT=json 时是一行 JSON。
func TestConfigErrorJSONLog(t *testing.T) {
	clearEnv(t)
	t.Setenv("KILN_LOG_FORMAT", "json")
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", code, exitUsage)
	}
	if out := stderr.String(); !strings.HasPrefix(out, "{") || !strings.Contains(out, `"msg":"配置错误"`) {
		t.Errorf("stderr = %q，期望 JSON 格式的配置错误日志", out)
	}
}

func TestEnvFile(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		env        map[string]string // 运行前已经设置的环境变量（优先于文件）
		wantCode   int
		wantStderr string
	}{
		{
			name:       "格式错误的行",
			content:    "# 注释\nKILN_LISTEN=:9090\nnot-a-pair\n",
			wantCode:   exitUsage,
			wantStderr: "读取 env 文件：" + "{FILE}:3：应为 KEY=VALUE",
		},
		{
			name: "文件中的配置生效：数据库地址无效时启动失败",
			// 引号、export 前缀和空行都能处理；数据库地址无法解析，在连接前就失败
			content:    "export KILN_DATABASE_URL='postgres://%zz'\n\nKILN_ADMIN_TOKEN=\"" + strings.Repeat("t", 16) + "\"\nKILN_MASTER_KEY=" + masterKey + "\nKILN_DATA_DIR={DATA}\n",
			wantCode:   exitError,
			wantStderr: "启动失败",
		},
		{
			name:       "已设置的环境变量优先于文件",
			content:    "KILN_DATABASE_URL=postgres://x\nKILN_ADMIN_TOKEN=" + strings.Repeat("t", 16) + "\nKILN_MASTER_KEY=" + masterKey + "\n",
			env:        map[string]string{"KILN_ADMIN_TOKEN": "short"},
			wantCode:   exitUsage,
			wantStderr: "KILN_ADMIN_TOKEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t, "KILN_LISTEN", "KILN_DATABASE_URL", "KILN_ADMIN_TOKEN", "KILN_MASTER_KEY", "KILN_DATA_DIR")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			dir := t.TempDir()
			file := filepath.Join(dir, "server.env")
			content := strings.ReplaceAll(tt.content, "{DATA}", filepath.Join(dir, "data"))
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"--env-file", file}, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("退出码 = %d，期望 %d；stderr = %s", code, tt.wantCode, stderr.String())
			}
			if want := strings.ReplaceAll(tt.wantStderr, "{FILE}", file); !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q，应包含 %q", stderr.String(), want)
			}
		})
	}
}

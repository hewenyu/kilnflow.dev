package main

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// TestScopeCommands 覆盖 secret、var、env：作用域 flag、flag 与位置参数混排、请求内容和输出。
func TestScopeCommands(t *testing.T) {
	updated := time.Date(2026, 10, 1, 8, 30, 0, 0, time.Local).Unix()
	tests := []struct {
		name    string
		args    []string
		stdin   string
		setup   func(f *fakeAdmin)
		method  string
		request proto.Message
		stdout  []string
	}{
		{
			name: "secret set 仓库级，值从标准输入读取并去掉末尾换行", args: []string{"secret", "set", "TOKEN", "--repo", "o/r"}, stdin: "s3cr3t\n\n",
			method: "SetSecret", request: &apiv1.SetSecretRequest{Scope: &apiv1.Scope{Repo: "o/r"}, Name: "TOKEN", Value: "s3cr3t"},
			stdout: []string{"已保存 TOKEN"},
		},
		{
			name: "secret set environment 级，flag 在名称前，多行值保留中间的换行", args: []string{"secret", "set", "--repo", "o/r", "--env", "prod", "KEY"}, stdin: "line1\nline2\n",
			method: "SetSecret", request: &apiv1.SetSecretRequest{Scope: &apiv1.Scope{Repo: "o/r", Environment: "prod"}, Name: "KEY", Value: "line1\nline2"},
		},
		{
			name: "secret set 账号级", args: []string{"secret", "set", "NPM", "--account", "acme"}, stdin: "v",
			method: "SetSecret", request: &apiv1.SetSecretRequest{Scope: &apiv1.Scope{Account: "acme"}, Name: "NPM", Value: "v"},
		},
		{
			name: "secret list 只显示名称和更新时间", args: []string{"secret", "list", "--account", "acme"},
			setup: func(f *fakeAdmin) {
				f.reply("ListSecrets", &apiv1.ListSecretsResponse{Secrets: []*apiv1.SecretInfo{{Name: "NPM", UpdatedAt: updated}}})
			},
			method: "ListSecrets", request: &apiv1.ListSecretsRequest{Scope: &apiv1.Scope{Account: "acme"}},
			stdout: []string{"名称 更新时间", "NPM 2026-10-01 08:30"},
		},
		{
			name: "secret delete", args: []string{"secret", "delete", "--repo", "o/r", "NPM", "--env", "prod"},
			method: "DeleteSecret", request: &apiv1.DeleteSecretRequest{Scope: &apiv1.Scope{Repo: "o/r", Environment: "prod"}, Name: "NPM"},
			stdout: []string{"已删除 NPM"},
		},
		{
			name: "var set", args: []string{"var", "set", "--repo", "o/r", "GOFLAGS", "--", "-mod=mod"},
			method: "SetVariable", request: &apiv1.SetVariableRequest{Scope: &apiv1.Scope{Repo: "o/r"}, Name: "GOFLAGS", Value: "-mod=mod"},
			stdout: []string{"已保存 GOFLAGS"},
		},
		{
			name: "var set 用 -- 传以 - 开头的值", args: []string{"var", "set", "--account", "acme", "--", "LEVEL", "-1"},
			method: "SetVariable", request: &apiv1.SetVariableRequest{Scope: &apiv1.Scope{Account: "acme"}, Name: "LEVEL", Value: "-1"},
		},
		{
			name: "var list", args: []string{"var", "list", "--repo", "o/r", "--env", "staging"},
			setup: func(f *fakeAdmin) {
				f.reply("ListVariables", &apiv1.ListVariablesResponse{Variables: []*apiv1.Variable{{Name: "A", Value: "1"}, {Name: "B", Value: "two"}}})
			},
			method: "ListVariables", request: &apiv1.ListVariablesRequest{Scope: &apiv1.Scope{Repo: "o/r", Environment: "staging"}},
			stdout: []string{"名称 值", "A 1", "B two"},
		},
		{
			name: "var delete", args: []string{"var", "delete", "A", "--account", "acme"},
			method: "DeleteVariable", request: &apiv1.DeleteVariableRequest{Scope: &apiv1.Scope{Account: "acme"}, Name: "A"},
			stdout: []string{"已删除 A"},
		},
		{
			name:   "env set 带 ref 策略和发布目标（可重复）",
			args:   []string{"env", "set", "release", "--repo", "o/app", "--ref", "refs/tags/v*", "--publish-to", "o/app-dl", "--ref", "refs/heads/main"},
			method: "UpsertEnvironment", request: &apiv1.UpsertEnvironmentRequest{Environment: &apiv1.Environment{
				Name: "release", Repo: "o/app", RefPolicy: []string{"refs/tags/v*", "refs/heads/main"}, AllowedPublishRepos: []string{"o/app-dl"},
			}},
			stdout: []string{"已保存 environment release"},
		},
		{
			name: "env set 账号级", args: []string{"env", "set", "--account", "acme", "prod"},
			method: "UpsertEnvironment", request: &apiv1.UpsertEnvironmentRequest{Environment: &apiv1.Environment{Name: "prod", Account: "acme"}},
		},
		{
			name: "env list 区分账号级和仓库级", args: []string{"env", "list", "--account", "acme"},
			setup: func(f *fakeAdmin) {
				f.reply("ListEnvironments", &apiv1.ListEnvironmentsResponse{Environments: []*apiv1.Environment{
					{Name: "prod", Account: "acme", RefPolicy: []string{"refs/tags/v*"}},
					{Name: "release", Repo: "acme/app", RefPolicy: []string{"refs/tags/*", "refs/heads/main"}, AllowedPublishRepos: []string{"acme/dl", "acme/dl2"}},
				}})
			},
			method: "ListEnvironments", request: &apiv1.ListEnvironmentsRequest{Account: "acme"},
			stdout: []string{
				"名称 作用域 ref 允许发布到",
				"prod acme（账号级） refs/tags/v*",
				"release acme/app refs/tags/*,refs/heads/main acme/dl,acme/dl2",
			},
		},
		{
			name: "env 默认 list", args: []string{"env"},
			method: "ListEnvironments", request: &apiv1.ListEnvironmentsRequest{},
			stdout: []string{"名称 作用域 ref 允许发布到"},
		},
		{
			name: "env delete", args: []string{"env", "delete", "prod", "--repo", "o/r"},
			method: "DeleteEnvironment", request: &apiv1.DeleteEnvironmentRequest{Scope: &apiv1.Scope{Repo: "o/r", Environment: "prod"}},
			stdout: []string{"已删除 environment prod"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			got := runApp(t, cfg, tt.stdin, tt.args...)
			if got.code != exitOK {
				t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
			}
			wantRequest(t, f, tt.method, tt.request)
			if tt.stdout != nil {
				wantLines(t, got.stdout, tt.stdout...)
			}
		})
	}
}

// secret 的值不能出现在输出中。
func TestSecretValueNotEchoed(t *testing.T) {
	cfg, _, _ := loggedIn(t)
	const value = "do-not-print-me"
	got := runApp(t, cfg, value+"\n", "secret", "set", "S", "--repo", "o/r")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	if strings.Contains(got.stdout+got.stderr, value) {
		t.Errorf("输出中出现了 secret 的值：stdout = %q，stderr = %q", got.stdout, got.stderr)
	}
	// 值误写在命令行中时报用法错误，错误说明中同样不出现它
	got = runApp(t, cfg, "", "secret", "set", "S", value, "--repo", "o/r")
	if got.code != exitUsage || strings.Contains(got.stdout+got.stderr, value) {
		t.Errorf("退出码 = %d，stderr = %q；期望用法错误且不输出命令行中的值", got.code, got.stderr)
	}
}

// 恰好达到大小上限时完整保存；超过上限时拒绝，不用截断后的值覆盖已有 secret。
func TestSecretSetSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"恰好上限", maxSecretSize},
		{"超出一字节", maxSecretSize + 1},
		{"超出多字节", maxSecretSize + 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			value := strings.Repeat("a", tc.size)
			got := runApp(t, cfg, value, "secret", "set", "BIG", "--repo", "o/r")
			reqs := f.requests("SetSecret")
			if tc.size > maxSecretSize {
				if got.code != exitUsage || !strings.Contains(got.stderr, "超过") || len(reqs) != 0 {
					t.Fatalf("超限应报用法错误且不请求 Server：code=%d, stderr=%q, requests=%d", got.code, got.stderr, len(reqs))
				}
				return
			}
			if got.code != exitOK || len(reqs) != 1 {
				t.Fatalf("code=%d, stderr=%q, requests=%d", got.code, got.stderr, len(reqs))
			}
			r, ok := reqs[0].(*apiv1.SetSecretRequest)
			if !ok || r.GetValue() != value {
				t.Fatal("应完整保存 secret")
			}
		})
	}
}

// 作用域命令的用法错误：退出码 2，不请求 Server。
func TestScopeUsageErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{name: "secret 未知子命令", args: []string{"secret", "get", "X", "--repo", "o/r"}, want: "用法：kiln secret set|list|delete"},
		{name: "secret set 缺少名称", args: []string{"secret", "set", "--repo", "o/r"}, want: "用法：kiln secret set NAME"},
		{name: "secret set 缺少作用域", args: []string{"secret", "set", "X"}, stdin: "v", want: "需要 --account 或 --repo"},
		{name: "secret set 只有 --env 不够", args: []string{"secret", "set", "X", "--env", "prod"}, stdin: "v", want: "需要 --account 或 --repo"},
		{name: "secret set 值为空", args: []string{"secret", "set", "X", "--repo", "o/r"}, stdin: "\n", want: "值为空"},
		// 修复前命令行中的值被忽略，保存的是标准输入的内容
		{name: "secret set 值写在命令行中", args: []string{"secret", "set", "X", "s3cr3t", "--repo", "o/r"}, stdin: "other\n", want: "secret 的值从标准输入读取"},
		{name: "secret list 缺少作用域", args: []string{"secret", "list"}, want: "需要 --account 或 --repo"},
		{name: "secret delete 缺少名称", args: []string{"secret", "delete", "--repo", "o/r"}, want: "用法：kiln secret delete NAME"},
		{name: "secret delete 缺少作用域", args: []string{"secret", "delete", "X"}, want: "需要 --account 或 --repo"},
		{name: "secret 未知 flag", args: []string{"secret", "set", "X", "--scope", "o/r"}, want: "kiln secret set："},
		{name: "var 未知子命令", args: []string{"var", "get"}, want: "用法：kiln var set|list|delete"},
		{name: "var set 缺少值", args: []string{"var", "set", "A", "--repo", "o/r"}, want: "用法：kiln var set NAME VALUE"},
		{name: "var delete 缺少名称", args: []string{"var", "delete", "--repo", "o/r"}, want: "用法：kiln var delete NAME"},
		{name: "var list 缺少作用域", args: []string{"var", "list"}, want: "需要 --account 或 --repo"},
		{name: "var 以 - 开头的值需要放在 -- 之后", args: []string{"var", "set", "A", "-mod=mod", "--repo", "o/r"}, want: "-mod"},
		{name: "var -- 之后的参数不再当作 flag", args: []string{"var", "set", "A", "--", "-1", "--repo", "o/r"}, want: "需要 --account 或 --repo"},
		{name: "env 未知子命令", args: []string{"env", "show"}, want: "用法：kiln env set|list|delete"},
		{name: "env set 缺少名称", args: []string{"env", "set", "--repo", "o/r"}, want: "用法：kiln env set NAME"},
		{name: "env set 缺少作用域", args: []string{"env", "set", "prod", "--ref", "refs/tags/*"}, want: "需要 --account 或 --repo"},
		{name: "env delete 缺少名称", args: []string{"env", "delete", "--repo", "o/r"}, want: "用法：kiln env delete NAME"},
		{name: "env delete 缺少作用域", args: []string{"env", "delete", "prod"}, want: "需要 --account 或 --repo"},
		{name: "env 不支持 --env", args: []string{"env", "set", "prod", "--repo", "o/r", "--env", "x"}, want: "kiln env set："},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			got := runApp(t, cfg, tt.stdin, tt.args...)
			if got.code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d；stderr = %q", got.code, exitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.want)
			}
			wantNoCalls(t, f)
		})
	}
}

package main

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

func TestNpmCommands(t *testing.T) {
	const token = "npm_do-not-print-me"
	cfg, f, _ := loggedIn(t)
	f.reply("SetNpmAccount", &apiv1.SetNpmAccountResponse{Account: &apiv1.NpmAccount{
		Account: "octocat", Scope: "@acme", Registry: "https://registry.npmjs.org/",
	}})
	got := runApp(t, cfg, token+"\n", "npm", "set", "--account", "octocat", "--scope", "@acme")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "SetNpmAccount", &apiv1.SetNpmAccountRequest{Account: "octocat", Scope: "@acme", Token: token})
	wantLines(t, got.stdout, "已保存 octocat 的 npm 配置：@acme（https://registry.npmjs.org/）")
	if strings.Contains(got.stdout+got.stderr, token) {
		t.Errorf("输出中出现了 token：%q %q", got.stdout, got.stderr)
	}

	updated := time.Date(2026, 11, 8, 9, 0, 0, 0, time.Local).Unix()
	f.reply("ListNpmAccounts", &apiv1.ListNpmAccountsResponse{Accounts: []*apiv1.NpmAccount{{
		Account: "octocat", Scope: "@acme", Registry: "https://registry.npmjs.org/", UpdatedBy: "admin", UpdatedAt: updated,
	}}})
	got = runApp(t, cfg, "", "npm", "list")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantLines(t, got.stdout, "账号 scope registry 更新者 更新时间", "octocat @acme https://registry.npmjs.org/ admin 2026-11-08 09:00")

	got = runApp(t, cfg, "", "npm", "delete", "--account", "octocat")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	wantRequest(t, f, "DeleteNpmAccount", &apiv1.DeleteNpmAccountRequest{Account: "octocat"})
}

func TestNpmUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"set 缺少 scope", []string{"npm", "set", "--account", "a"}, "t", "kiln npm set --account A --scope @SCOPE"},
		{"set token 为空", []string{"npm", "set", "--account", "a", "--scope", "@s"}, "\n", "token 为空"},
		{"delete 缺少账号", []string{"npm", "delete"}, "", "kiln npm delete --account A"},
		{"未知子命令", []string{"npm", "show"}, "", "kiln npm set|list|delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			got := runApp(t, cfg, tc.stdin, tc.args...)
			if got.code != exitUsage || !strings.Contains(got.stderr, tc.want) {
				t.Errorf("退出码 %d，stderr %q，期望用法错误且包含 %q", got.code, got.stderr, tc.want)
			}
			if len(f.requests("SetNpmAccount")) != 0 {
				t.Error("不应发出请求")
			}
		})
	}
}

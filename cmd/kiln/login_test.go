package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

func TestLogin(t *testing.T) {
	tests := []struct {
		name       string
		args       []string // 不含 --server，测试中补上假 Server 的地址
		stdin      string
		wantCode   int
		wantStderr string
		wantSaved  bool
	}{
		{name: "--token 给出", args: []string{"--token", testToken}, wantCode: exitOK, wantSaved: true},
		{name: "非终端时从标准输入读一行", stdin: testToken + "\n多余的行\n", wantCode: exitOK, wantSaved: true},
		{name: "标准输入没有换行", stdin: "  " + testToken + "  ", wantCode: exitOK, wantSaved: true},
		{name: "token 错误时不保存", args: []string{"--token", "wrong"}, wantCode: exitError, wantStderr: "用这个 token 访问"},
		{name: "标准输入为空", stdin: "", wantCode: exitUsage, wantStderr: "token 为空"},
		{name: "多余的参数", args: []string{"extra", "--token", testToken}, wantCode: exitUsage, wantStderr: "多余的参数"},
		// 多余的参数可能就是忘了写 --token 的 token：错误说明中不能出现它
		{name: "token 忘了写 --token", args: []string{testToken}, wantCode: exitUsage, wantStderr: "多余的参数"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := isolate(t)
			f := newFakeAdmin()
			url := serveAdmin(t, f)
			args := append([]string{"login", "--server", url + "/"}, tt.args...)
			got := runApp(t, cfg, tt.stdin, args...)
			if got.code != tt.wantCode {
				t.Fatalf("退出码 = %d，期望 %d；stderr = %q", got.code, tt.wantCode, got.stderr)
			}
			if !strings.Contains(got.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.wantStderr)
			}
			if strings.Contains(got.stdout+got.stderr, testToken) {
				t.Errorf("输出中出现了 token：stdout = %q，stderr = %q", got.stdout, got.stderr)
			}
			_, statErr := os.Stat(cfg)
			if !tt.wantSaved {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("不应保存配置：%v", statErr)
				}
				return
			}
			c, err := loadConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if want := (cliConfig{Server: url, Token: testToken}); c != want {
				t.Errorf("保存的配置 = %+v，期望 %+v", c, want)
			}
			if !strings.Contains(got.stdout, "已登录 "+url) || !strings.Contains(got.stdout, cfg) {
				t.Errorf("stdout = %q，应说明登录的 Server 和配置位置", got.stdout)
			}
			// 登录时用 ListRunners 验证 token
			wantRequest(t, f, "ListRunners", &apiv1.ListRunnersRequest{})
		})
	}
}

// token 错误时错误信息带上 Server 返回的错误码。
func TestLoginShowsCode(t *testing.T) {
	cfg := isolate(t)
	url := serveAdmin(t, newFakeAdmin())
	got := runApp(t, cfg, "", "login", "--server", url, "--token", "wrong")
	if got.code != exitError || !strings.Contains(got.stderr, "（unauthenticated）") {
		t.Errorf("退出码 = %d，stderr = %q；期望 1 且包含错误码", got.code, got.stderr)
	}
}

// 登录后的命令使用保存的配置；之后换成别的 token 再登录会覆盖旧配置。
func TestLoginThenUse(t *testing.T) {
	cfg := isolate(t)
	f := newFakeAdmin()
	url := serveAdmin(t, f)
	f.reply("ListRuns", &apiv1.ListRunsResponse{Runs: []*apiv1.Run{{Id: 7, State: "success", Repo: "o/r"}}})
	if got := runApp(t, cfg, "", "login", "--server", url, "--token", testToken); got.code != exitOK {
		t.Fatalf("登录：退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	got := runApp(t, cfg, "", "runs", "--repo", "o/r")
	if got.code != exitOK || !strings.Contains(got.stdout, "#7") {
		t.Errorf("runs：退出码 = %d，stdout = %q，stderr = %q", got.code, got.stdout, got.stderr)
	}

	const newToken = "another-token-value"
	f.setToken(newToken)
	if got := runApp(t, cfg, "", "login", "--server", url, "--token", newToken); got.code != exitOK {
		t.Fatalf("重新登录：退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	c, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != newToken {
		t.Errorf("重新登录后 token = %q，期望 %q", c.Token, newToken)
	}
}

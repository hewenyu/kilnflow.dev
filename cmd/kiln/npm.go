package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// npmCmd 管理账号级 npm 配置（PLAN #27.2）：set（token 从标准输入读取）、list、delete。
// release.npm 发布到 @<scope>/<name>，registry、scope、token 都来自这里，workflow 中不写凭据。
func (a *app) npmCmd(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("npm " + sub)
	account := fs.String("account", "", "账号（GitHub 用户或组织）")
	scope := fs.String("scope", "", "npm scope，如 @your-scope")
	registry := fs.String("registry", "", "registry 地址（默认 https://registry.npmjs.org/）")
	if _, err := a.parse(fs, rest); err != nil {
		return err
	}
	switch sub {
	case "set":
		if *account == "" || *scope == "" {
			return usageErrorf("用法：kiln npm set --account A --scope @SCOPE [--registry URL]（token 从标准输入读取）")
		}
	case "delete":
		if *account == "" {
			return usageErrorf("用法：kiln npm delete --account A")
		}
	case "list", "":
	default:
		return usageErrorf("用法：kiln npm set|list|delete")
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		if _, ok := terminalFd(a.stdin); ok {
			fmt.Fprintln(a.stderr, "输入 npm token（需要能免 OTP 发布），以 Ctrl-D 结束：")
		}
		b, err := io.ReadAll(io.LimitReader(a.stdin, maxSecretSize+1))
		if err != nil {
			return fmt.Errorf("读取 token：%w", err)
		}
		if len(b) > maxSecretSize {
			return usageErrorf("token 超过 %d 字节", maxSecretSize)
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return usageErrorf("token 为空")
		}
		resp, err := cl.SetNpmAccount(ctx, req(&apiv1.SetNpmAccountRequest{Account: *account, Scope: *scope, Registry: *registry, Token: token}))
		if err != nil {
			return err
		}
		acct := resp.Msg.GetAccount()
		fmt.Fprintf(a.stdout, "已保存 %s 的 npm 配置：%s（%s）\n", acct.GetAccount(), acct.GetScope(), acct.GetRegistry())
	case "delete":
		if _, err := cl.DeleteNpmAccount(ctx, req(&apiv1.DeleteNpmAccountRequest{Account: *account})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已删除", *account, "的 npm 配置")
	default:
		resp, err := cl.ListNpmAccounts(ctx, req(&apiv1.ListNpmAccountsRequest{}))
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(resp.Msg.Accounts))
		for _, acct := range resp.Msg.Accounts {
			rows = append(rows, []string{acct.Account, acct.Scope, acct.Registry, acct.UpdatedBy, time.Unix(acct.UpdatedAt, 0).Format("2006-01-02 15:04")})
		}
		a.table("账号\tscope\tregistry\t更新者\t更新时间", rows)
	}
	return nil
}

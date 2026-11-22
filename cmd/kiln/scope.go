package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// maxSecretSize 是从标准输入读取 secret 值的上限。
const maxSecretSize = 1 << 20

// secret 管理 secret：set（值从标准输入读取）、list、delete。
func (a *app) secret(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("secret " + sub)
	scope := scopeFlags(fs)
	pos, err := a.parse(fs, rest)
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		return a.secretSet(ctx, pos, scope)
	case "list":
		sc, err := scope()
		if err != nil {
			return err
		}
		cl, err := a.client()
		if err != nil {
			return err
		}
		resp, err := cl.ListSecrets(ctx, req(&apiv1.ListSecretsRequest{Scope: sc}))
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(resp.Msg.Secrets))
		for _, s := range resp.Msg.Secrets {
			rows = append(rows, []string{s.Name, time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04")})
		}
		a.table("名称\t更新时间", rows)
	case "delete":
		if err := need(pos, 1, "secret delete NAME (--account A | --repo R) [--env E]"); err != nil {
			return err
		}
		sc, err := scope()
		if err != nil {
			return err
		}
		cl, err := a.client()
		if err != nil {
			return err
		}
		if _, err := cl.DeleteSecret(ctx, req(&apiv1.DeleteSecretRequest{Scope: sc, Name: pos[0]})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已删除", pos[0])
	default:
		return usageErrorf("用法：kiln secret set|list|delete")
	}
	return nil
}

func (a *app) secretSet(ctx context.Context, pos []string, scope func() (*apiv1.Scope, error)) error {
	if err := need(pos, 1, "secret set NAME (--account A | --repo R) [--env E]"); err != nil {
		return err
	}
	if len(pos) > 1 {
		// 例如 kiln secret set NAME VALUE：VALUE 会被忽略，实际保存的是标准输入的内容。多余的参数可能就是 secret 的值，不输出
		return usageErrorf("多余的参数：secret 的值从标准输入读取，不要写在命令行中")
	}
	sc, err := scope()
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, ok := terminalFd(a.stdin); ok {
		fmt.Fprintf(a.stderr, "输入 %s 的值，以 Ctrl-D 结束：\n", pos[0])
	}
	b, err := io.ReadAll(io.LimitReader(a.stdin, maxSecretSize+1))
	if err != nil {
		return fmt.Errorf("读取 secret 的值：%w", err)
	}
	if len(b) > maxSecretSize {
		return usageErrorf("secret 的值超过 %d 字节", maxSecretSize)
	}
	v := strings.TrimRight(string(b), "\n")
	if v == "" {
		return usageErrorf("值为空")
	}
	if _, err := cl.SetSecret(ctx, req(&apiv1.SetSecretRequest{Scope: sc, Name: pos[0], Value: v})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已保存", pos[0])
	return nil
}

// variable 管理变量：set、list、delete。
func (a *app) variable(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("var " + sub)
	scope := scopeFlags(fs)
	pos, err := a.parse(fs, rest)
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		if err := need(pos, 2, "var set NAME VALUE (--account A | --repo R) [--env E]"); err != nil {
			return err
		}
	case "delete":
		if err := need(pos, 1, "var delete NAME (--account A | --repo R) [--env E]"); err != nil {
			return err
		}
	case "list":
	default:
		return usageErrorf("用法：kiln var set|list|delete")
	}
	sc, err := scope()
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		if _, err := cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: sc, Name: pos[0], Value: pos[1]})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已保存", pos[0])
	case "delete":
		if _, err := cl.DeleteVariable(ctx, req(&apiv1.DeleteVariableRequest{Scope: sc, Name: pos[0]})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已删除", pos[0])
	default:
		resp, err := cl.ListVariables(ctx, req(&apiv1.ListVariablesRequest{Scope: sc}))
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(resp.Msg.Variables))
		for _, v := range resp.Msg.Variables {
			rows = append(rows, []string{v.Name, v.Value})
		}
		a.table("名称\t值", rows)
	}
	return nil
}

// environment 管理 environment：set、list、delete。
func (a *app) environment(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("env " + sub)
	acc := fs.String("account", "", "账号级 environment")
	repo := fs.String("repo", "", "仓库级 environment（owner/name）")
	var refs, publish multi
	fs.Var(&refs, "ref", "允许的 ref（glob，可重复），例如 'refs/tags/v*'")
	fs.Var(&publish, "publish-to", "允许发布到的仓库 owner/name（可重复）")
	pos, err := a.parse(fs, rest)
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		if err := need(pos, 1, "env set NAME (--account A | --repo R) [--ref P] [--publish-to R]"); err != nil {
			return err
		}
		if *acc == "" && *repo == "" {
			return usageErrorf("需要 --account 或 --repo")
		}
	case "delete":
		if err := need(pos, 1, "env delete NAME (--account A | --repo R)"); err != nil {
			return err
		}
		if *acc == "" && *repo == "" {
			return usageErrorf("需要 --account 或 --repo")
		}
	case "list", "":
	default:
		return usageErrorf("用法：kiln env set|list|delete")
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	switch sub {
	case "set":
		env := &apiv1.Environment{
			Name: pos[0], Account: *acc, Repo: *repo, RefPolicy: refs, AllowedPublishRepos: publish,
		}
		if _, err := cl.UpsertEnvironment(ctx, req(&apiv1.UpsertEnvironmentRequest{Environment: env})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已保存 environment", pos[0])
	case "delete":
		sc := &apiv1.Scope{Account: *acc, Repo: *repo, Environment: pos[0]}
		if _, err := cl.DeleteEnvironment(ctx, req(&apiv1.DeleteEnvironmentRequest{Scope: sc})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已删除 environment", pos[0])
	default:
		resp, err := cl.ListEnvironments(ctx, req(&apiv1.ListEnvironmentsRequest{Account: *acc}))
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(resp.Msg.Environments))
		for _, e := range resp.Msg.Environments {
			scope := e.Repo
			if scope == "" {
				scope = e.Account + "（账号级）"
			}
			rows = append(rows, []string{
				e.Name, scope, strings.Join(e.RefPolicy, ","), strings.Join(e.AllowedPublishRepos, ","),
			})
		}
		a.table("名称\t作用域\tref\t允许发布到", rows)
	}
	return nil
}

package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// admin：管理员绑定的 GitHub 账号和 Dashboard 的 admin token 登录（PLAN #16.5）。
// 绑定在 Dashboard 中完成（需要 GitHub 授权）；CLI 用于查看、恢复 token 登录（例如 GitHub 登录不可用时）和解除绑定。
func (a *app) admin(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "", "status":
		return a.adminStatus(ctx)
	case "token-login":
		if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") {
			return usageErrorf("用法：kiln admin token-login on|off")
		}
		return a.adminTokenLogin(ctx, rest[0] == "on")
	case "unbind":
		return a.adminUnbind(ctx)
	}
	return usageErrorf("未知的子命令 admin %s\n用法：kiln admin [status] | admin token-login on|off | admin unbind", sub)
}

func (a *app) adminStatus(ctx context.Context) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.GetAdminBinding(ctx, req(&apiv1.GetAdminBindingRequest{}))
	if err != nil {
		return err
	}
	m := resp.Msg
	if !m.Bound {
		fmt.Fprintln(a.stdout, "GitHub 账号：未绑定（在 Dashboard 的 设置 → 登录与身份 中绑定）")
		fmt.Fprintln(a.stdout, "Dashboard admin token 登录：开启（没有绑定时总是开启）")
		return nil
	}
	fmt.Fprintf(a.stdout, "GitHub 账号：%s（ID %d），%s 由 %s 绑定\n", m.GithubLogin, m.GithubId,
		time.Unix(m.BoundAt, 0).Local().Format("2006-01-02 15:04"), m.BoundBy)
	if m.TokenLogin {
		fmt.Fprintln(a.stdout, "Dashboard admin token 登录：开启")
	} else {
		fmt.Fprintln(a.stdout, "Dashboard admin token 登录：关闭（只能用 GitHub 登录；kiln admin token-login on 可以恢复）")
	}
	return nil
}

func (a *app) adminTokenLogin(ctx context.Context, on bool) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.SetTokenLogin(ctx, req(&apiv1.SetTokenLoginRequest{Enabled: on})); err != nil {
		return err
	}
	if on {
		fmt.Fprintln(a.stdout, "已开启 Dashboard 的 admin token 登录")
	} else {
		fmt.Fprintln(a.stdout, "已关闭 Dashboard 的 admin token 登录：只能用绑定的 GitHub 账号登录，已用 admin token 登录的会话随即失效。CLI 不受影响")
	}
	return nil
}

func (a *app) adminUnbind(ctx context.Context) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.UnbindAdmin(ctx, req(&apiv1.UnbindAdminRequest{})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已解除绑定，Dashboard 的 admin token 登录已恢复")
	return nil
}

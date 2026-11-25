package admin

import (
	"errors"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// TestBinding：绑定、关闭 / 开启 admin token 登录、解除绑定，以及绑定后 CLI 的操作者记为绑定的账号。
func TestBinding(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx

	got, err := cl.GetAdminBinding(ctx, req(&apiv1.GetAdminBindingRequest{}))
	e.must(err)
	if got.Msg.Bound || !got.Msg.TokenLogin {
		t.Fatalf("没有绑定时：%v", got.Msg)
	}
	// 没有绑定时不能关闭 token 登录（否则管理员无法登录 Dashboard）；开启不报错
	_, err = cl.SetTokenLogin(ctx, req(&apiv1.SetTokenLoginRequest{Enabled: false}))
	wantCode(t, "没有绑定时关闭 token 登录", err, connect.CodeFailedPrecondition)
	_, err = cl.SetTokenLogin(ctx, req(&apiv1.SetTokenLoginRequest{Enabled: true}))
	e.must(err)
	_, err = cl.UnbindAdmin(ctx, req(&apiv1.UnbindAdminRequest{}))
	wantCode(t, "没有绑定时解除", err, connect.CodeFailedPrecondition)
	// Dashboard 直接调用 Service：前提不满足是可以判断的 sentinel（包装在 FailedPrecondition 中），CLI 的说明不变
	for name, c := range map[string]struct {
		err  error
		want error
	}{
		"关闭 token 登录": {e.svc.SetTokenLogin(ctx, false, testActor), ErrNotBound},
		"解除绑定":        {e.svc.UnbindAdmin(ctx, testActor), ErrAlreadyUnbound},
	} {
		ce, ok := errors.AsType[*connect.Error](c.err)
		if !errors.Is(c.err, c.want) || !ok || ce.Code() != connect.CodeFailedPrecondition || ce.Message() != c.want.Error() {
			t.Errorf("%s：%#v，期望 FailedPrecondition 包装的 %v", name, c.err, c.want)
		}
	}

	// 没有绑定时 CLI 的操作者是 TokenActor
	_, err = cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "A", Value: "1"}))
	e.must(err)
	e.wantAudit("config.variable.set", TokenActor, nil)

	e.must(e.svc.BindAdmin(ctx, 9001, "octocat", TokenActor))
	e.wantAudit("admin.bind", TokenActor, map[string]any{"github_login": "octocat", "github_id": float64(9001)})
	got, err = cl.GetAdminBinding(ctx, req(&apiv1.GetAdminBindingRequest{}))
	e.must(err)
	if !got.Msg.Bound || got.Msg.GithubLogin != "octocat" || got.Msg.GithubId != 9001 || !got.Msg.TokenLogin || got.Msg.BoundBy != TokenActor || got.Msg.BoundAt == 0 {
		t.Fatalf("绑定后：%v", got.Msg)
	}

	// 绑定后 CLI 的操作记为绑定的账号
	_, err = cl.SetTokenLogin(ctx, req(&apiv1.SetTokenLoginRequest{Enabled: false}))
	e.must(err)
	e.wantAudit("admin.token_login.disable", "octocat", nil)
	got, _ = cl.GetAdminBinding(ctx, req(&apiv1.GetAdminBindingRequest{}))
	if got.Msg.TokenLogin {
		t.Fatalf("关闭后 token_login 仍为 true")
	}
	// CLI 不受 Dashboard 的 token 登录开关影响
	_, err = cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "B", Value: "2"}))
	e.must(err)
	if a := e.audits("config.variable.set"); len(a) != 2 || a[1].Actor != "octocat" {
		t.Fatalf("绑定后 CLI 的操作者：%+v", a)
	}

	// 解除绑定同时恢复 token 登录
	_, err = cl.UnbindAdmin(ctx, req(&apiv1.UnbindAdminRequest{}))
	e.must(err)
	e.wantAudit("admin.unbind", "octocat", map[string]any{"github_login": "octocat"})
	got, _ = cl.GetAdminBinding(ctx, req(&apiv1.GetAdminBindingRequest{}))
	if got.Msg.Bound || !got.Msg.TokenLogin {
		t.Fatalf("解除后：%v", got.Msg)
	}
}

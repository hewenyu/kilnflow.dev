package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 管理员绑定 GitHub 账号（PLAN #16.5）：管理员和他的 GitHub 账号是同一个人。
// 绑定后用这个 GitHub 账号登录 Dashboard 即为管理员；CLI（admin token）的操作在审计日志中记为这个账号。
// 确认 GitHub 登录可用后可以关闭 Dashboard 的 admin token 登录；CLI 不受影响，可以随时重新开启（恢复入口）。

// ErrNotBound 表示还没有绑定 GitHub 账号（关闭 token 登录的前提不满足，包装在 FailedPrecondition 的 *connect.Error 中）。
var ErrNotBound = errors.New("还没有绑定 GitHub 账号：先在 Dashboard 的 设置 → 登录与身份 中绑定")

// ErrAlreadyUnbound 表示解除绑定时还没有绑定（包装在 FailedPrecondition 的 *connect.Error 中）。
var ErrAlreadyUnbound = errors.New("没有绑定 GitHub 账号，不需要解除")

// AdminBinding 返回管理员绑定的 GitHub 账号；还没有绑定时返回 nil。
func (s *Service) AdminBinding(ctx context.Context) (*store.AdminBinding, error) {
	return store.GetAdminBinding(ctx, s.db)
}

// TokenIdentity 返回通过 admin token 操作时的身份：绑定了 GitHub 账号时是该账号的登录名，否则是 TokenActor。
func (s *Service) TokenIdentity(ctx context.Context) (string, error) {
	b, err := store.GetAdminBinding(ctx, s.db)
	if err != nil {
		return "", err
	}
	if b == nil {
		return TokenActor, nil
	}
	return b.GitHubLogin, nil
}

// BindAdmin 把管理员绑定到 GitHub 账号（已经绑定时更换为这个账号），并写审计日志。
func (s *Service) BindAdmin(ctx context.Context, githubID int64, login, actor string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		prev, err := store.GetAdminBinding(ctx, tx)
		if err != nil {
			return err
		}
		if err := store.PutAdminBinding(ctx, tx, githubID, login, actor); err != nil {
			return err
		}
		detail := map[string]any{"github_login": login, "github_id": githubID}
		if prev != nil {
			detail["previous"] = prev.GitHubLogin
		}
		return store.Audit(ctx, tx, actor, "admin.bind", detail)
	})
}

// UnbindAdmin 解除绑定，同时恢复 Dashboard 的 admin token 登录（没有绑定时 token 登录总是开启）。
func (s *Service) UnbindAdmin(ctx context.Context, actor string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		prev, err := store.GetAdminBinding(ctx, tx)
		if err != nil {
			return err
		}
		if prev == nil {
			return connect.NewError(connect.CodeFailedPrecondition, ErrAlreadyUnbound)
		}
		if _, err := store.DeleteAdminBinding(ctx, tx); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "admin.unbind", map[string]any{"github_login": prev.GitHubLogin})
	})
}

// SetTokenLogin 开启或关闭 Dashboard 的 admin token 登录。关闭前必须已经绑定 GitHub 账号，否则返回 ErrNotBound。
func (s *Service) SetTokenLogin(ctx context.Context, enabled bool, actor string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		ok, err := store.SetAdminTokenLogin(ctx, tx, enabled)
		if err != nil {
			return err
		}
		if !ok {
			if enabled {
				return nil // 没有绑定时 token 登录本来就开启
			}
			return connect.NewError(connect.CodeFailedPrecondition, ErrNotBound)
		}
		action := "admin.token_login.disable"
		if enabled {
			action = "admin.token_login.enable"
		}
		return store.Audit(ctx, tx, actor, action, map[string]any{})
	})
}

func (a *rpcAPI) GetAdminBinding(ctx context.Context, _ *connect.Request[apiv1.GetAdminBindingRequest]) (*connect.Response[apiv1.GetAdminBindingResponse], error) {
	b, err := a.s.AdminBinding(ctx)
	if err != nil {
		return nil, err
	}
	resp := &apiv1.GetAdminBindingResponse{TokenLogin: true}
	if b != nil {
		resp = &apiv1.GetAdminBindingResponse{
			Bound: true, GithubId: b.GitHubID, GithubLogin: b.GitHubLogin, TokenLogin: b.TokenLogin,
			BoundAt: b.BoundAt.Unix(), BoundBy: b.BoundBy,
		}
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) SetTokenLogin(ctx context.Context, req *connect.Request[apiv1.SetTokenLoginRequest]) (*connect.Response[apiv1.SetTokenLoginResponse], error) {
	if err := a.s.SetTokenLogin(ctx, req.Msg.Enabled, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.SetTokenLoginResponse{}), nil
}

func (a *rpcAPI) UnbindAdmin(ctx context.Context, _ *connect.Request[apiv1.UnbindAdminRequest]) (*connect.Response[apiv1.UnbindAdminResponse], error) {
	if err := a.s.UnbindAdmin(ctx, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.UnbindAdminResponse{}), nil
}

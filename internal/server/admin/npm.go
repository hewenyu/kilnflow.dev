package admin

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 账号级 npm 配置（PLAN #27.2）：release.npm 发布到 @<scope>/<name>，registry、scope、token 每个账号一份。

// maxNpmToken 是 npm token 的长度上限。
const maxNpmToken = 4096

// npm 配置的校验和前提错误（Dashboard 按类型显示当前语言的说明，CLI 用 Error()）。
var (
	// ErrNpmTokenEmpty 表示没有填写 token。
	ErrNpmTokenEmpty = errors.New("token 为空")
	// ErrNpmTokenTooLong 表示 token 超过 maxNpmToken。
	ErrNpmTokenTooLong = errors.New("token 太长")
)

// NpmNotConfiguredError 表示删除 npm 配置时账号还没有配置；Account 是账号名。
type NpmNotConfiguredError struct{ Account string }

func (e *NpmNotConfiguredError) Error() string { return "账号 " + e.Account + " 没有配置 npm" }

func (a *rpcAPI) SetNpmAccount(ctx context.Context, req *connect.Request[apiv1.SetNpmAccountRequest]) (*connect.Response[apiv1.SetNpmAccountResponse], error) {
	m := req.Msg
	acct, err := a.s.SetNpmAccount(ctx, m.Account, m.Scope, m.Registry, m.Token, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.SetNpmAccountResponse{Account: acct}), nil
}

// SetNpmAccount 加密保存（新建或覆盖）账号的 npm 配置。token 只写入密文，不进入审计日志和错误信息。
// scope 不合格时返回 *npm.ScopeError，registry 不合格时返回 *endpoint.Error，token 为空或太长时返回 ErrNpmTokenEmpty、ErrNpmTokenTooLong。
func (s *Service) SetNpmAccount(ctx context.Context, account, scope, registry, token, actor string) (*apiv1.NpmAccount, error) {
	scope, err := npm.NormalizeScope(strings.TrimSpace(scope))
	if err != nil {
		return nil, err
	}
	registry = strings.TrimSpace(registry)
	if registry == "" {
		registry = npm.DefaultRegistry
	}
	if err := npm.CheckRegistry(registry); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(registry, "/") {
		registry += "/"
	}
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return nil, ErrNpmTokenEmpty
	case len(token) > maxNpmToken:
		return nil, ErrNpmTokenTooLong
	}
	var out *apiv1.NpmAccount
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		ref, err := store.ResolveScope(ctx, tx, account, "", "")
		if err != nil {
			return err
		}
		if err := store.PutNpmAccount(ctx, tx, s.cfg.MasterKey, ref.OwnerID, ref.Label, scope, registry, token, actor); err != nil {
			return err
		}
		if err := store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "npm.set", map[string]any{
			"account": ref.Label, "scope": scope, "registry": registry,
		}); err != nil {
			return err
		}
		a, err := store.GetNpmAccount(ctx, tx, ref.OwnerID)
		if err != nil {
			return err
		}
		out = npmAccountMsg(a)
		return nil
	})
	return out, err
}

func (a *rpcAPI) DeleteNpmAccount(ctx context.Context, req *connect.Request[apiv1.DeleteNpmAccountRequest]) (*connect.Response[apiv1.DeleteNpmAccountResponse], error) {
	if err := a.s.DeleteNpmAccount(ctx, req.Msg.Account, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.DeleteNpmAccountResponse{}), nil
}

// DeleteNpmAccount 删除账号的 npm 配置（没有配置时报错）。
func (s *Service) DeleteNpmAccount(ctx context.Context, account, actor string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		ref, err := store.ResolveScope(ctx, tx, account, "", "")
		if err != nil {
			return err
		}
		ok, err := store.DeleteNpmAccount(ctx, tx, ref.OwnerID)
		if err != nil {
			return err
		}
		if !ok {
			return &NpmNotConfiguredError{Account: ref.Label}
		}
		return store.RecordConfigChange(ctx, tx, ref.OwnerID, actor, "npm.delete", map[string]any{"account": ref.Label})
	})
}

func (a *rpcAPI) ListNpmAccounts(ctx context.Context, _ *connect.Request[apiv1.ListNpmAccountsRequest]) (*connect.Response[apiv1.ListNpmAccountsResponse], error) {
	list, err := a.s.ListNpmAccounts(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.ListNpmAccountsResponse{Accounts: list}), nil
}

// ListNpmAccounts 返回全部账号的 npm 配置（不含 token）。
func (s *Service) ListNpmAccounts(ctx context.Context) ([]*apiv1.NpmAccount, error) {
	list, err := store.ListNpmAccounts(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.NpmAccount, 0, len(list))
	for i := range list {
		out = append(out, npmAccountMsg(&list[i]))
	}
	return out, nil
}

func npmAccountMsg(a *store.NpmAccount) *apiv1.NpmAccount {
	return &apiv1.NpmAccount{Account: a.Account, Scope: a.Scope, Registry: a.Registry, UpdatedBy: a.UpdatedBy, UpdatedAt: a.UpdatedAt.Unix()}
}

package admin

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
)

// rpcAPI 把 Service 适配为 AdminService（ConnectRPC），供 kiln CLI 使用。
// 认证由 authInterceptor 统一完成，操作者（rpcActor）是管理员绑定的 GitHub 账号，没有绑定时为 TokenActor。
// 各 RPC 方法放在对应操作的旁边。
type rpcAPI struct{ s *Service }

var _ apiv1connect.AdminServiceHandler = (*rpcAPI)(nil)

// NewHandler 返回 AdminService 的路由前缀和 handler；每个请求都要携带 admin token（keys 校验）。
// 内部错误由 errorInterceptor 记录日志并转换为 CodeInternal；它在最外层，认证时读取数据库失败也会被记录。
func NewHandler(svc *Service, keys *auth.Keys, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append(opts, connect.WithInterceptors(errorInterceptor(svc.log), &authInterceptor{svc: svc, keys: keys}))
	return apiv1connect.NewAdminServiceHandler(&rpcAPI{s: svc}, opts...)
}

// authInterceptor 校验每个 AdminService 请求的 admin token，并把操作者放进 ctx（rpcActor 读取）。
// AdminService 目前只有 unary 方法；streaming 请求同样校验，以后增加 streaming 方法时不会绕过认证。
type authInterceptor struct {
	svc  *Service
	keys *auth.Keys
}

var _ connect.Interceptor = (*authInterceptor)(nil)

// authenticate 校验请求头中的 admin token，返回带有操作者的 ctx。
func (i *authInterceptor) authenticate(ctx context.Context, h http.Header) (context.Context, error) {
	if err := i.keys.CheckAdmin(h); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	actor, err := i.svc.TokenIdentity(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, actorKey{}, actor), nil
}

func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient 只用于客户端，handler 不会调用。
func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// errorInterceptor 记录 AdminService 返回的内部错误（数据库等，见 rpcError）并包装为 CodeInternal（见 rpcerr.ErrorInterceptor）。
// 错误说明仍然返回给 CLI：调用方是管理员，排查时需要。
func errorInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return rpcerr.ErrorInterceptor(log, "AdminService 内部错误")
}

// rpcError 把操作返回的错误转换为 RPC 错误：
//   - 已经是 *connect.Error 的原样返回（操作自己已经选好了错误码）；
//   - 内部错误（见 IsInternal）原样返回，由 errorInterceptor 记录日志并转换为 CodeInternal，
//     不能当作请求的问题（例如 InvalidArgument）返回，否则 Server 端没有记录，CLI 也会误以为是参数错误；
//   - 其余是请求本身的问题（参数无效、仓库不存在、前提条件不满足等），使用 code。
func rpcError(code connect.Code, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*connect.Error](err); ok || IsInternal(err) {
		return err
	}
	return connect.NewError(code, err)
}

// IsInternal 判断 err 是否是 Server 的内部错误：数据库返回的错误、连接或网络错误、文件读写错误、超时，
// 以及 context 的取消和超时。这些错误不是请求的问题，不能把原文当作说明展示给用户（AdminService 和 Dashboard 共用）。
func IsInternal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) ||
		errors.Is(err, pgx.ErrTxClosed) {
		return true
	}
	_, pg := errors.AsType[*pgconn.PgError](err)
	_, conn := errors.AsType[*pgconn.ConnectError](err)
	_, netErr := errors.AsType[net.Error](err)
	_, pathErr := errors.AsType[*fs.PathError](err)
	return pg || conn || netErr || pathErr
}

// actorKey 是 ctx 中操作者的键。
type actorKey struct{}

// rpcActor 返回 AdminService 请求的操作者（由 authInterceptor 设置）：管理员绑定的 GitHub 账号，没有绑定时为 TokenActor。
func rpcActor(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return TokenActor
}

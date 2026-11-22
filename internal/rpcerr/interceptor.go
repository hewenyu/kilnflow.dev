package rpcerr

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
)

// ErrorInterceptor 处理 Server 端 RPC 服务（RunnerService、AdminService）返回的内部错误：
// 已经是 *connect.Error 的（包括本包约定的原因）和 context 的取消、超时原样返回；
// 其余错误（数据库等）以 msg 记一条 Error 日志后包装为 CodeInternal（保留错误链），
// 否则它们会变成 CodeUnknown，而且 Server 端没有任何记录。
func ErrorInterceptor(log *slog.Logger, msg string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err == nil {
				return resp, nil
			}
			if _, ok := errors.AsType[*connect.Error](err); ok {
				return resp, err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return resp, err
			}
			log.ErrorContext(ctx, msg, "procedure", req.Spec().Procedure, "err", err)
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
}

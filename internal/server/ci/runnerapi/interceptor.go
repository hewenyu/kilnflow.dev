package runnerapi

import (
	"log/slog"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
)

// ErrorInterceptor 记录 RunnerService 返回的内部错误并包装为 CodeInternal（见 rpcerr.ErrorInterceptor）。
// Runner 端把 CodeUnknown 和 CodeInternal 都当作可以重试，行为不变。
func ErrorInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return rpcerr.ErrorInterceptor(log, "Runner API 内部错误")
}

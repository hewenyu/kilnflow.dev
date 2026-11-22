package gitcred

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

// MaxOpLen 是 op_id 的最大长度（字节）：登记在内存中，按键保存到 attempt 失效。
const MaxOpLen = 128

// ValidOp 校验客户端给出的 op_id：不能为空，最长 MaxOpLen 字节。不通过时返回 InvalidArgument。
func ValidOp(op string) error {
	if op == "" || len(op) > MaxOpLen {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("op_id 不能为空，最长 %d 字节", MaxOpLen))
	}
	return nil
}

// RPCError 把 Issue 的错误转换为 Connect 错误码（AI、CI 的签发 RPC 共用）。只有 Unavailable（GitHub 确定没有签发，操作
// 保持打开）可以用同一 op_id、同一仓库重试，Canceled（发起请求的客户端取消，操作保持打开）可以重放；其余错误之后，客户端的
// 这次 git 访问失败，重新执行即是新的操作。最终检查的错误（已经是 Connect 错误）原样返回；数据库故障等内部错误原样返回，
// 由 ErrorInterceptor 记录并转换为 Internal。
func RPCError(err error) error {
	switch {
	case errors.Is(err, ErrClosed):
		// 操作已关闭或 attempt 已结束（包括签发在途时被关闭、结果不明的）：同一 op_id 不会再签发
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrCheckFailed):
		// Server 侧的最终检查没有完成（超时等）：操作已经关闭，终态的内部错误
		return connect.NewError(connect.CodeInternal, err)
	case errors.Is(err, ErrScopeMismatch):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrUnknown):
		// 操作已经以错误关闭：这次 git 访问失败，重新执行即是新的操作
		return connect.NewError(connect.CodeAborted, err)
	case gh.NotApplied(err):
		// GitHub 确定没有签发（操作不关闭，范围仍绑定）：可以用同一 op_id、同一仓库重试
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, context.Canceled):
		// 发起请求的客户端已经取消：这次不交出，操作保持打开，同一 op_id 重放拿到同一枚
		return connect.NewError(connect.CodeCanceled, err)
	}
	return err
}

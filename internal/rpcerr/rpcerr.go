// Package rpcerr 定义 Runner API 中 Server 与 Runner 约定的错误原因，以及 Server 端 RPC 服务共用的错误拦截器（ErrorInterceptor）。
//
// 原因字符串作为 connect 错误的消息传输，是线上协议的一部分：Runner 与 Server 的版本可能不同，不能修改。
package rpcerr

import (
	"errors"

	"connectrpc.com/connect"
)

const (
	// ReasonStaleEpoch 表示 (attempt_id, epoch) 已经不是当前值，Runner 应放弃这次 attempt（PLAN #6.1）。
	ReasonStaleEpoch = "STALE_EPOCH"
	// ReasonRunnerRevoked 表示 Runner 已被吊销，Runner 收到后停止工作。
	ReasonRunnerRevoked = "RUNNER_REVOKED"
	// ReasonAlreadyExists 表示同一个 attempt 已经提交过内容不同的结果（PLAN #33.6）：调用方上报的与已接受的不是同一个，
	// 不要重试。
	ReasonAlreadyExists = "ALREADY_EXISTS"
)

// StaleEpoch 返回 epoch 过期的错误。每次返回新值：*connect.Error 是可变的，不能共享。
func StaleEpoch() *connect.Error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(ReasonStaleEpoch))
}

// RunnerRevoked 返回 Runner 已被吊销的错误。
func RunnerRevoked() *connect.Error {
	return connect.NewError(connect.CodePermissionDenied, errors.New(ReasonRunnerRevoked))
}

// AlreadyExists 返回 attempt 已经提交过内容不同的结果的错误。
func AlreadyExists() *connect.Error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(ReasonAlreadyExists))
}

// IsStaleEpoch 判断 err 是否是 Server 返回的 epoch 过期错误。
func IsStaleEpoch(err error) bool {
	return is(err, connect.CodeFailedPrecondition, ReasonStaleEpoch)
}

// IsAlreadyExists 判断 err 是否是 Server 返回的"这次执行已经有内容不同的结果"错误。
func IsAlreadyExists(err error) bool {
	return is(err, connect.CodeAlreadyExists, ReasonAlreadyExists)
}

// IsRunnerRevoked 判断 err 是否是 Server 返回的 Runner 已被吊销错误。
func IsRunnerRevoked(err error) bool {
	return is(err, connect.CodePermissionDenied, ReasonRunnerRevoked)
}

func is(err error, code connect.Code, reason string) bool {
	ce, ok := errors.AsType[*connect.Error](err)
	return ok && ce.Code() == code && ce.Message() == reason
}

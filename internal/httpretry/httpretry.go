// Package httpretry 是发布用的 HTTP 客户端（npm registry、HTTP 上传、OCI registry）共用的重试工具：
// 判断状态码是否值得重试、解析 Retry-After、可取消的等待，以及为复用连接读完响应体。
// 每个客户端的重试次数和退避时间由它自己决定，这里只放三者完全相同的部分。
package httpretry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryAfter 是遵从 Retry-After 的上限，更长的等待按普通退避处理。
const MaxRetryAfter = time.Minute

// maxDrain 是 Discard 为复用连接最多读取的字节数。
const maxDrain = 1 << 20

// Retryable 判断状态码是否值得重试：429 和 5xx。
func Retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// RetryAfter 解析 Retry-After 中的秒数（不支持 HTTP 日期）；无效或超过 MaxRetryAfter 时返回 false。
func RetryAfter(v string) (time.Duration, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	// 先按秒数比较：过大的值乘以 time.Second 会溢出成负数
	if err != nil || n < 0 || n > int(MaxRetryAfter/time.Second) {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// Sleep 等待 d，ctx 结束时提前返回 ctx.Err()。d 不大于 0 时只检查 ctx。
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Discard 读完响应体 body（最多 1 MiB，更长的直接放弃），让关闭之后连接可以复用。不关闭 body：
// 各客户端在自己的包里调用 Discard 再关闭（bodyclose 只能在同一个包中确认响应体已经关闭）。
func Discard(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrain)) // 只为复用连接，失败无妨
}

// 发布客户端（npm registry、HTTP 上传）共用的重试参数与请求超时。
const (
	// DefaultAttempts 是可重试错误（网络错误、429、5xx）的最大尝试次数。
	DefaultAttempts = 5
	// DefaultBackoff 是第一次重试前的等待时间，之后每次翻倍，最长 MaxBackoff。
	DefaultBackoff = time.Second
	// MaxBackoff 是退避时间的上限。
	MaxBackoff = 30 * time.Second
	// RequestTimeout 是单个发布请求的超时（请求体中有整个 tarball 或大文件）。
	RequestTimeout = 30 * time.Minute
)

// Transport 把传输层错误换成去掉凭据的文案，同时保留 context.Canceled / DeadlineExceeded 的判别语义
// （errors.Is 仍然成立）。redact 为 nil 时原样保留错误文本。
func Transport(err error, redact func(string) string) error {
	msg := err.Error()
	if redact != nil {
		msg = redact(msg)
	}
	var cause error
	switch {
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	}
	return &transportError{msg: msg, cause: cause}
}

// transportError 是 Transport 的返回值：Error 是脱敏后的文本，Unwrap 暴露标准 context 哨兵错误。
type transportError struct {
	msg   string
	cause error
}

func (e *transportError) Error() string { return e.msg }
func (e *transportError) Unwrap() error { return e.cause }

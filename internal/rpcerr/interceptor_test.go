package rpcerr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestErrorInterceptor(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	call := func(err error) error {
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, err })
		_, got := ErrorInterceptor(log, "内部错误")(next)(t.Context(), connect.NewRequest(&struct{}{}))
		return got
	}
	if err := call(nil); err != nil {
		t.Fatalf("没有错误时应返回 nil：%v", err)
	}
	if err := call(fmt.Errorf("提交结果：%w", StaleEpoch())); !IsStaleEpoch(err) {
		t.Fatalf("connect 错误应原样返回：%v", err)
	}
	if err := call(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("context 错误应原样返回：%v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("上面的错误不应记日志：%s", logs.String())
	}
	internal := errors.New("数据库连接断开")
	err := call(internal)
	if connect.CodeOf(err) != connect.CodeInternal || !errors.Is(err, internal) {
		t.Fatalf("内部错误应包装为 CodeInternal 并保留错误链：%v", err)
	}
	if !strings.Contains(logs.String(), "数据库连接断开") || !strings.Contains(logs.String(), "内部错误") {
		t.Fatalf("内部错误应记日志：%q", logs.String())
	}
}

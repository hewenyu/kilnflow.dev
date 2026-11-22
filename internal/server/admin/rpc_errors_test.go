package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AdminService 的错误处理：内部错误与请求错误的区分、streaming 认证、请求参数的范围检查。

// syncBuffer 是可以并发写入的日志缓冲（handler 在 HTTP 服务的 goroutine 中写日志）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestRPC_InternalErrors：数据库出错时返回 CodeInternal 并在 Server 端记录日志，
// 而不是当作参数错误（InvalidArgument / FailedPrecondition）或没有记录的 CodeUnknown 返回。
func TestRPC_InternalErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logs syncBuffer
	e.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx
	e.failOn("variables", "INSERT", "true")

	_, err := cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "A", Value: "1"}))
	wantCode(t, "SetVariable 写入失败", err, connect.CodeInternal)
	// 参数错误仍然是 InvalidArgument
	_, err = cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "A-B", Value: "1"}))
	wantCode(t, "SetVariable 名称无效", err, connect.CodeInvalidArgument)
	if got := logs.String(); !strings.Contains(got, "kiln_test_fail") || !strings.Contains(got, "SetVariable") {
		t.Errorf("内部错误应记录日志（含过程名和原因）：%q", got)
	}

	// 没有经过 Service 的查询、Service 中查找仓库、认证时读取管理员绑定失败，同样是 CodeInternal
	e.exec(`ALTER TABLE repos RENAME TO repos_gone`)
	_, err = cl.ListRepos(ctx, req(&apiv1.ListReposRequest{}))
	wantCode(t, "ListRepos 查询失败", err, connect.CodeInternal)
	_, err = cl.BumpCacheSalt(ctx, req(&apiv1.BumpCacheSaltRequest{Repo: testRepo}))
	wantCode(t, "BumpCacheSalt 查找仓库失败", err, connect.CodeInternal)
	_, err = cl.ListRuns(ctx, req(&apiv1.ListRunsRequest{Repo: testRepo}))
	wantCode(t, "ListRuns 查找仓库失败", err, connect.CodeInternal)
	e.exec(`ALTER TABLE admin_binding RENAME TO admin_binding_gone`)
	_, err = cl.ListOutbox(ctx, req(&apiv1.ListOutboxRequest{}))
	wantCode(t, "认证时读取管理员绑定失败", err, connect.CodeInternal)
}

func TestRPCError(t *testing.T) {
	t.Parallel()
	pgErr := fmt.Errorf("写入：%w", &pgconn.PgError{Code: "57014", Message: "boom"})
	netErr := fmt.Errorf("查询：%w", &net.OpError{Op: "read", Err: errors.New("connection reset")})
	nf := connect.NewError(connect.CodeNotFound, errors.New("没有"))
	for _, c := range []struct {
		name string
		err  error
		code connect.Code // 0 表示原样返回
	}{
		{"参数错误", errors.New("名称不能为空"), connect.CodeInvalidArgument},
		{"仓库不存在", fmt.Errorf("%w：x/y", store.ErrRepoNotFound), connect.CodeInvalidArgument},
		{"已经是 connect 错误", nf, 0},
		{"数据库错误", pgErr, 0},
		{"网络错误", netErr, 0},
		{"连接数据库失败", &pgconn.ConnectError{}, 0},
		{"取消", fmt.Errorf("x：%w", context.Canceled), 0},
		{"超时", context.DeadlineExceeded, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := rpcError(connect.CodeInvalidArgument, c.err)
			if c.code == 0 {
				if got != c.err { //nolint:errorlint // 检查的是原样返回同一个值
					t.Errorf("rpcError = %v，应原样返回 %v", got, c.err)
				}
				return
			}
			if connect.CodeOf(got) != c.code || !errors.Is(got, c.err) {
				t.Errorf("rpcError = %v（%v），应为 %v 并包装原错误", got, connect.CodeOf(got), c.code)
			}
		})
	}
	if rpcError(connect.CodeInvalidArgument, nil) != nil {
		t.Error("rpcError(nil) 应为 nil")
	}
}

// streamConn 是只提供请求头的 StreamingHandlerConn。
type streamConn struct {
	connect.StreamingHandlerConn
	header http.Header
}

func (c *streamConn) RequestHeader() http.Header { return c.header }

// TestAuthInterceptorStreaming：streaming 请求同样要求 admin token，以后增加 streaming 方法时不会绕过认证。
func TestAuthInterceptorStreaming(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	i := &authInterceptor{svc: e.svc, keys: auth.NewKeys([]byte("token-key"), []byte("cas-key"), testAdminToken)}
	for _, c := range []struct {
		name   string
		header string
		ok     bool
	}{
		{"没有 token", "", false},
		{"错误的 token", "Bearer wrong", false},
		{"正确的 token", "Bearer " + testAdminToken, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var actor string
			called := false
			h := i.WrapStreamingHandler(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
				called, actor = true, rpcActor(ctx)
				return nil
			})
			conn := &streamConn{header: http.Header{}}
			if c.header != "" {
				conn.header.Set("Authorization", c.header)
			}
			err := h(e.ctx, conn)
			if !c.ok {
				wantCode(t, c.name, err, connect.CodeUnauthenticated)
				if called {
					t.Error("没有认证的请求不应执行")
				}
				return
			}
			if err != nil || !called || actor != TokenActor {
				t.Errorf("err=%v called=%v actor=%q", err, called, actor)
			}
		})
	}
}

// TestRPC_ListOutboxState：写错的状态返回 InvalidArgument，而不是当作没有记录。
func TestRPC_ListOutboxState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	e.addOutbox("k1", store.OutboxDone, 1)
	_, err := cl.ListOutbox(e.ctx, req(&apiv1.ListOutboxRequest{State: "deda"}))
	wantCode(t, "未知的状态", err, connect.CodeInvalidArgument)
	resp, err := cl.ListOutbox(e.ctx, req(&apiv1.ListOutboxRequest{State: store.OutboxDone}))
	e.must(err)
	if len(resp.Msg.Items) != 1 {
		t.Errorf("done 状态的记录 = %v", resp.Msg.Items)
	}
}

// TestRPC_CreateEnrollTokenTTL：有效期（分钟）大到换算成 time.Duration 会溢出时拒绝，不生成令牌；
// 修复前溢出后被当作默认值或任意的有效期。
func TestRPC_CreateEnrollTokenTTL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	for _, ttl := range []int32{math.MaxInt32, int32(maxEnrollTokenTTLMinutes + 1)} {
		_, err := cl.CreateEnrollToken(e.ctx, req(&apiv1.CreateEnrollTokenRequest{TtlMinutes: ttl}))
		wantCode(t, fmt.Sprintf("有效期 %d 分钟", ttl), err, connect.CodeInvalidArgument)
	}
	_, err := cl.CreateEnrollToken(e.ctx, req(&apiv1.CreateEnrollTokenRequest{Kind: "gpu"}))
	wantCode(t, "无效的类型", err, connect.CodeInvalidArgument)
	if n := e.count(`SELECT count(*) FROM enrollment_tokens`); n != 0 {
		t.Errorf("拒绝的请求生成了 %d 个令牌", n)
	}
	resp, err := cl.CreateEnrollToken(e.ctx, req(&apiv1.CreateEnrollTokenRequest{TtlMinutes: int32(maxEnrollTokenTTLMinutes)}))
	e.must(err)
	if exp := time.Unix(resp.Msg.ExpiresAt, 0); exp.Before(time.Now().AddDate(200, 0, 0)) {
		t.Errorf("上限内的有效期应原样使用：到期 %v", exp)
	}
}

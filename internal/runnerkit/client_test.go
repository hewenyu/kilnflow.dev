package runnerkit

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
)

// 认证：只有 Server 发来的 PermissionDenied 才说明 Runner 被吊销；反向代理的普通 403（Connect 客户端按 HTTP 状态合成）
// 只是认证失败，不能让 Runner 当成被吊销而停止。
func TestAuthenticateRevokedOnlyFromServer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		reply   func(w http.ResponseWriter)
		revoked bool
	}{
		{"代理的普通 403", func(w http.ResponseWriter) { http.Error(w, "Forbidden", http.StatusForbidden) }, false},
		{"Server 明确拒绝", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission_denied","message":"已吊销"}`))
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { c.reply(w) }))
			t.Cleanup(srv.Close)
			cl := NewClient(srv.URL, 7, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
			_, err := cl.Authenticate(t.Context())
			if err == nil || errors.Is(err, ErrRevoked) != c.revoked {
				t.Fatalf("err = %v，期望被吊销 = %v", err, c.revoked)
			}
			if !c.revoked && (connect.CodeOf(err) != connect.CodeUnavailable || !Retryable(err) || !NetworkError(err)) {
				t.Errorf("代理的 403 应按暂时性错误交回（Unavailable，可以重试）：%v", err)
			}
		})
	}
}

// Call：Server 明确回答 token 无效（wire error）时重新认证并重放一次；代理的普通 401 不重放，错误交回调用方。
func TestCallReplaysOnlyOnServerUnauthenticated(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		reply  func(w http.ResponseWriter)
		calls  int64
		replay bool
	}{
		{"代理的普通 401", func(w http.ResponseWriter) { http.Error(w, "Unauthorized", http.StatusUnauthorized) }, 1, false},
		{"Server 明确拒绝", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"token 无效"}`))
		}, 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var heartbeats, auths atomic.Int64
			mux := http.NewServeMux()
			mux.Handle(runnerv1connect.NewRunnerServiceHandler(authOnly{n: &auths}))
			mux.HandleFunc("/kiln.runner.v1.RunnerService/Heartbeat", func(w http.ResponseWriter, _ *http.Request) {
				heartbeats.Add(1)
				c.reply(w)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			cl := NewClient(srv.URL, 7, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
			_, err := Call(t.Context(), cl, cl.RPC().Heartbeat, &runnerv1.HeartbeatRequest{})
			if connect.CodeOf(err) != connect.CodeUnauthenticated || connect.IsWireError(err) == !c.replay {
				t.Fatalf("err = %v", err)
			}
			if got := heartbeats.Load(); got != c.calls {
				t.Errorf("请求发出 %d 次，期望 %d 次", got, c.calls)
			}
			if want := map[bool]int64{false: 1, true: 2}[c.replay]; auths.Load() != want {
				t.Errorf("认证 %d 次，期望 %d 次", auths.Load(), want)
			}
		})
	}
}

// authHandler 是用函数实现的 Authenticate（测试用来控制每次认证的时机与结果）。
type authHandler struct {
	runnerv1connect.UnimplementedRunnerServiceHandler
	fn func(context.Context) (string, error)
}

func newAuthHandler(fn func(context.Context) (string, error)) authHandler { return authHandler{fn: fn} }

func (a authHandler) Authenticate(ctx context.Context, _ *connect.Request[runnerv1.AuthenticateRequest]) (*connect.Response[runnerv1.AuthenticateResponse], error) {
	tok, err := a.fn(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&runnerv1.AuthenticateResponse{AccessToken: tok, ExpiresAt: time.Now().Add(time.Hour).Unix()}), nil
}

// TestAuthenticateLeaderCancelDoesNotKillSharedRefresh：leader 的 ctx 取消只让 leader 自己返回 ctx.Err()，
// 后台的共享刷新继续完成，等待者照样拿到 token，且不会因为 leader 取消再发一次认证。
func TestAuthenticateLeaderCancelDoesNotKillSharedRefresh(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.Handle(runnerv1connect.NewRunnerServiceHandler(newAuthHandler(func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return "tok-shared", nil
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := NewClient(srv.URL, 7, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := cl.Authenticate(leaderCtx)
		leaderErr <- err
	}()
	<-started

	type authResult struct {
		tok string
		err error
	}
	waiter := make(chan authResult, 1)
	go func() {
		tok, err := cl.Authenticate(context.Background())
		waiter <- authResult{tok, err}
	}()

	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader 取消后应返回 context.Canceled，实际 %v", err)
	}
	close(release)
	select {
	case r := <-waiter:
		if r.err != nil || r.tok != "tok-shared" {
			t.Fatalf("等待者 = (%q, %v)，期望拿到共享刷新的 token", r.tok, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待者没有拿到共享刷新的结果")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("认证请求 %d 次，leader 取消不应触发第二次", n)
	}
}

// TestAuthenticateWaiterGetsOwnFlightResult：flight 1 失败后 flight 2 立即启动并成功，flight 1 的等待者读到的仍是
// flight 1 的错误，而不是 ("", nil)（结果按 flight 保存，不共享字段）。
func TestAuthenticateWaiterGetsOwnFlightResult(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.Handle(runnerv1connect.NewRunnerServiceHandler(newAuthHandler(func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return "", errors.New("flight 1 失败")
		}
		return "tok-2", nil
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := NewClient(srv.URL, 7, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))

	go func() { _, _ = cl.Authenticate(context.Background()) }()
	<-started
	cl.mu.Lock()
	f1 := cl.authing
	cl.mu.Unlock()
	if f1 == nil {
		t.Fatal("第 1 次认证没有登记 flight")
	}
	close(release)
	<-f1.done

	// flight 2 立即启动并成功
	tok2 := make(chan string, 1)
	go func() {
		tok, err := cl.Authenticate(context.Background())
		if err != nil {
			tok = "err:" + err.Error()
		}
		tok2 <- tok
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cl.mu.Lock()
		tok := cl.token
		cl.mu.Unlock()
		if tok != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flight 2 没有完成")
		}
		time.Sleep(time.Millisecond)
	}
	if tok, err := waitAuth(context.Background(), f1); err == nil || tok != "" {
		t.Fatalf("flight 1 的等待者读到 (%q, %v)，应仍是 flight 1 的错误", tok, err)
	}
	if tok := <-tok2; tok != "tok-2" {
		t.Fatalf("flight 2 = %q，期望 tok-2", tok)
	}
}

// Runner 的重定向策略：任何一跳是 https 都不允许后续降级到 http、限制重定向次数、跨 origin 去掉 Authorization、
// 默认端口归一化。
func TestSafeRedirect(t *testing.T) {
	t.Parallel()
	newReq := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer access-token")
		return r
	}
	via := []*http.Request{newReq("https://kiln.example.com/x")}
	if err := safeRedirect(newReq("http://kiln.example.com/x"), via); err == nil {
		t.Error("https → http 应被拒绝")
	}
	explicit := newReq("https://kiln.example.com:443/y")
	if err := safeRedirect(explicit, via); err != nil {
		t.Fatal(err)
	}
	if h := explicit.Header.Get("Authorization"); h != "Bearer access-token" {
		t.Errorf("默认端口归一化后应保留凭据：%q", h)
	}
	// 原始是 http，中间升级到 https，再降回 http：任何一跳是 https 都不允许降级。
	upgraded := []*http.Request{newReq("http://kiln.example.com/x"), newReq("https://kiln.example.com/y")}
	if err := safeRedirect(newReq("http://kiln.example.com/z"), upgraded); err == nil {
		t.Error("历史跳为 https 时降级到 http 应被拒绝")
	}
	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = newReq("https://kiln.example.com/x")
	}
	if err := safeRedirect(newReq("https://kiln.example.com/y"), many); err == nil {
		t.Error("超过重定向上限应被拒绝")
	}
}

// authOnly 是只实现 Authenticate 的 Server（记下次数）。
type authOnly struct {
	runnerv1connect.UnimplementedRunnerServiceHandler
	n *atomic.Int64
}

func (a authOnly) Authenticate(context.Context, *connect.Request[runnerv1.AuthenticateRequest]) (*connect.Response[runnerv1.AuthenticateResponse], error) {
	a.n.Add(1)
	return connect.NewResponse(&runnerv1.AuthenticateResponse{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}), nil
}

package runnerkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
)

const (
	// tokenRefreshMargin 是 access token 过期前提前刷新的时间。
	tokenRefreshMargin = time.Minute
	// retryInitialDelay / retryMaxDelay 是 Retry 的退避区间。
	retryInitialDelay = 500 * time.Millisecond
	retryMaxDelay     = 15 * time.Second
	// maxIdleConnsPerHost 是与 Server 之间保持的空闲连接数（日志、心跳、上传可能同时进行）。
	maxIdleConnsPerHost = 16
	// idleConnTimeout 是空闲连接的保持时间。
	idleConnTimeout = 90 * time.Second
	// responseHeaderTimeout 是等待响应头的时间上限。必须大于 Acquire 长轮询的等待时间
	// （AcquireWaitSeconds），否则没有任务时长轮询总会超时。
	responseHeaderTimeout = 60 * time.Second
	// authRPCTimeout 是一次认证 RPC 的总超时：Server 长时间不回时按失败重试，不会一直占着 single-flight。
	authRPCTimeout = 30 * time.Second
	// maxRedirects 是允许的最大重定向次数（与 net/http 的默认值一致）。
	maxRedirects = 10
)

// Acquire 长轮询与失败退避（两种 Runner 的领取循环共用）。
const (
	// AcquireWaitSeconds 是 Acquire 长轮询的等待时间。
	AcquireWaitSeconds = 25
	// AcquireBackoffMin / AcquireBackoffMax 是 Acquire 失败后的退避区间。
	AcquireBackoffMin = time.Second
	AcquireBackoffMax = 30 * time.Second
)

var (
	// ErrRevoked 表示 Runner 已被吊销。
	ErrRevoked = errors.New("本 Runner 已被吊销，需要重新注册")
	// ErrStale 表示任务的这次执行（attempt）已经不属于本 Runner（epoch 过期），应立即放弃。
	ErrStale = errors.New("attempt 已不属于本 Runner（epoch 过期）")
)

// Client 是与 Server 的连接：持有 HTTP 客户端、Runner 身份和 access token。认证用 kiln.runner.v1 的 Authenticate；
// 各领域的服务客户端用 HTTP() 和 Server() 构造，调用时经过 Call 带上 token。
type Client struct {
	server string
	http   *http.Client
	rpc    runnerv1connect.RunnerServiceClient
	id     int64
	key    ed25519.PrivateKey

	mu    sync.Mutex
	token string
	exp   time.Time
	// authing 非 nil 表示有认证请求正在进行（single-flight）：其他调用者等同一个 flight 结束。
	authing *authFlight
}

// authFlight 是一次认证请求。结果按 flight 保存，等待者只读自己等的那一次：leader 取消不会误杀等待者，
// 也不会在下一轮认证启动后读到上一轮的错误或空 token。
type authFlight struct {
	done chan struct{}
	tok  string
	err  error
}

// NewClient 返回连接到 server 的客户端；id 为 0 时只能注册（Enroll）。
func NewClient(server string, id int64, key ed25519.PrivateKey) *Client {
	server = strings.TrimRight(server, "/")
	hc := &http.Client{Transport: cloneTransport(), CheckRedirect: safeRedirect}
	return &Client{
		server: server,
		http:   hc,
		id:     id,
		key:    key,
		rpc:    runnerv1connect.NewRunnerServiceClient(hc, server),
	}
}

// cloneTransport 复制 Go 的默认 Transport（保留拨号、TLS 握手的超时等默认值），再按 Runner 的需要调整连接池：
// 日志、心跳、上传可能同时进行，长轮询的响应头要等得久一些。
func cloneTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	tr := base.Clone()
	tr.MaxIdleConnsPerHost = maxIdleConnsPerHost
	tr.IdleConnTimeout = idleConnTimeout
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return tr
}

// safeRedirect 是 Runner 与 Server 通信的重定向策略：拒绝 https 降级到 http（任何一跳是 https 都不允许后续变回
// http）、限制重定向次数；跨 origin 时去掉 Authorization（access token 只发给配置的 Server）。
func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("重定向次数超过 %d 次", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		for _, v := range via {
			if v.URL.Scheme == "https" {
				return errors.New("拒绝从 https 重定向到 http（凭据会以明文传输）")
			}
		}
	}
	if !sameOrigin(via[0].URL, req.URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

// sameOrigin 判断两个地址的协议、主机名和有效端口是否相同。
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && portOf(a) == portOf(b)
}

// portOf 返回地址的端口，省略时按协议返回默认端口。
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// Server 返回 Server 地址（不带结尾的 /）。
func (c *Client) Server() string { return c.server }

// HTTP 返回与 Server 通信的 HTTP 客户端。
func (c *Client) HTTP() *http.Client { return c.http }

// RPC 返回 kiln.runner.v1 的客户端。
func (c *Client) RPC() runnerv1connect.RunnerServiceClient { return c.rpc }

// Authenticate 返回有效的 access token：快过期时用私钥签名重新认证。已被吊销时返回 ErrRevoked。
// 同一时刻只有一个认证请求发往 Server（single-flight），其他调用者等它的结果；等待期间 ctx 结束就返回 ctx.Err()，
// 不会因为别人在认证而忽略自己的取消。认证本身不持有 mutex，不会阻塞其他调用的读取。
//
// 认证在独立的后台 goroutine 中执行、只受 authRPCTimeout 约束：发起者（leader）中途取消不会中断共享的刷新，
// 等待者仍然能拿到 token；每个等待者读取自己等的那一个 flight 的结果。
func (c *Client) Authenticate(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Until(c.exp) > tokenRefreshMargin {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	if f := c.authing; f != nil {
		c.mu.Unlock()
		return waitAuth(ctx, f)
	}
	f := &authFlight{done: make(chan struct{})}
	c.authing = f
	c.mu.Unlock()

	go func() { //nolint:gosec // G118：共享的刷新要在发起者取消之后继续完成（其他等待者还在等），不继承它的 ctx；有 authRPCTimeout 兜底
		tok, exp, err := c.authenticate(context.Background())

		c.mu.Lock()
		if err == nil {
			c.token, c.exp = tok, exp
		}
		f.tok, f.err = tok, err
		if c.authing == f {
			c.authing = nil
		}
		c.mu.Unlock()
		close(f.done)
	}()
	return waitAuth(ctx, f)
}

// waitAuth 等一个认证 flight 结束；调用方自己取消时返回 ctx.Err()，不影响后台的认证。
func waitAuth(ctx context.Context, f *authFlight) (string, error) {
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// authenticate 向 Server 认证一次，返回新的 token 和到期时间。
func (c *Client) authenticate(ctx context.Context) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, authRPCTimeout)
	defer cancel()
	nonce := rand.Text()
	ts := time.Now().Unix()
	// 签名的消息格式必须与 Server 端的校验一致（internal/server/runnerauth/runnerauth.go）
	msg := fmt.Sprintf("kiln-runner-auth:%d:%d:%s", c.id, ts, nonce)
	resp, err := c.rpc.Authenticate(ctx, connect.NewRequest(&runnerv1.AuthenticateRequest{
		RunnerId: c.id, Timestamp: ts, Nonce: nonce, Signature: ed25519.Sign(c.key, []byte(msg)),
	}))
	if err != nil {
		// 只有 Server 发来的拒绝才说明 Runner 被吊销：代理、网关的普通 403（Connect 客户端按 HTTP 状态合成）不是
		code := connect.CodeOf(err)
		if code == connect.CodePermissionDenied && connect.IsWireError(err) {
			return "", time.Time{}, ErrRevoked
		}
		// 代理、网关的普通 401 / 403 不是 Server 的答复：按暂时性错误（Unavailable）交回，各调用方按自己的规则重试，
		// 不当作被拒绝（PermissionDenied 在 Retryable 中不重试，会让 Complete、FetchSecrets 等直接放弃）
		if (code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated) && !connect.IsWireError(err) {
			return "", time.Time{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("认证失败（没有收到 Server 的明确答复）：%w", err))
		}
		return "", time.Time{}, fmt.Errorf("认证失败：%w", err)
	}
	return resp.Msg.AccessToken, time.Unix(resp.Msg.ExpiresAt, 0), nil
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// Call 带上 access token 调用一个 RPC；Server 明确回答 token 无效（Server 发来的 Unauthenticated，connect.IsWireError）时
// 重新认证并重放一次：Kiln 的鉴权在处理函数之前，被拒绝的请求没有执行，重放是安全的。来源不明的 Unauthenticated（反向
// 代理、网关的普通 401，Connect 客户端按 HTTP 状态合成）不重放：请求可能已经到达 Server 并执行（例如签发了 token），
// 错误交回调用方，按各自的重试规则处理（签发按结果不明结束操作、按原 op 关闭）；缓存的 token 照样作废，下一次调用重新
// 认证。Server 约定的错误原因转换为 ErrRevoked（已吊销）、ErrStale（epoch 过期）。
func Call[Req, Resp any](ctx context.Context, c *Client, f func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error), msg *Req) (*Resp, error) {
	for i := 0; ; i++ {
		tok, err := c.Authenticate(ctx)
		if err != nil {
			return nil, err
		}
		req := connect.NewRequest(msg)
		req.Header().Set("Authorization", "Bearer "+tok)
		resp, err := f(ctx, req)
		if err == nil {
			return resp.Msg, nil
		}
		switch {
		case connect.CodeOf(err) == connect.CodeUnauthenticated:
			c.invalidate()
			if i == 0 && connect.IsWireError(err) {
				continue
			}
		case rpcerr.IsRunnerRevoked(err):
			return nil, ErrRevoked
		case rpcerr.IsStaleEpoch(err):
			return nil, ErrStale
		}
		return nil, err
	}
}

// Retry 对可重试的错误做指数退避重试，直到成功、遇到不可重试的错误、
// 达到 maxAttempts 次（0 表示不限次数，只受 ctx 约束）或 ctx 结束。
func Retry[T any](ctx context.Context, maxAttempts int, f func() (T, error)) (T, error) {
	return RetryWhen(ctx, maxAttempts, Retryable, f)
}

// RetryWhen 与 Retry 相同，只是由 retryable 判断错误是否值得重试（ErrStale、ErrRevoked 总是不重试）。
func RetryWhen[T any](ctx context.Context, maxAttempts int, retryable func(error) bool, f func() (T, error)) (T, error) {
	var zero T
	delay := retryInitialDelay
	for i := 0; ; i++ {
		v, err := f()
		if err == nil {
			return v, nil
		}
		permanent := errors.Is(err, ErrStale) || errors.Is(err, ErrRevoked) || !retryable(err)
		if permanent || (maxAttempts > 0 && i+1 >= maxAttempts) {
			return zero, err
		}
		select {
		case <-ctx.Done():
			return zero, err
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMaxDelay)
	}
}

// Retryable 判断 RPC 错误是否值得重试。
func Retryable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeFailedPrecondition, connect.CodeUnimplemented:
		return false
	}
	return true
}

// NetworkError 判断错误是否是网络问题（没有收到 Server 的响应）。
func NetworkError(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return true
	}
	return false
}

// SleepCtx 等待 d 或 ctx 结束。
func SleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// AuthenticateTrial 在启动时认证：试用的新版本连不上 Server（网络错误，没有收到任何响应）时在试用期限内重试，免得短暂的
// 网络故障被启动器当成新版本启动失败；收到响应但被拒绝、或超过试用期限时返回错误（PLAN #27.7 第 5 步）。
func AuthenticateTrial(ctx context.Context, c *Client, up *Upgrader, warn func(msg string, args ...any)) error {
	backoff := AcquireBackoffMin
	for {
		tctx, cancel := up.TrialContext(ctx)
		_, err := c.Authenticate(tctx)
		cancel()
		if err == nil || !up.Trial() || !up.TrialRetry(err, time.Now()) || ctx.Err() != nil {
			return err
		}
		warn("连不上 Server，稍后重试（新版本还没有确认）", "err", err)
		SleepCtx(ctx, backoff)
		backoff = min(backoff*2, AcquireBackoffMax)
	}
}

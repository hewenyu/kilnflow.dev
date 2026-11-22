package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestDownloadDisposition(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"kiln-demo":            `attachment; filename=kiln-demo`,
		"dist/kiln demo.tgz":   `attachment; filename="kiln demo.tgz"`,
		`..\..\evil.sh`:        `attachment; filename=evil.sh`,
		"a\r\nSet-Cookie: x=1": `attachment; filename="aSet-Cookie: x=1"`,
		"说明.txt":               `attachment; filename*=utf-8''%E8%AF%B4%E6%98%8E.txt`,
		"":                     "",
		"/":                    "",
		"..":                   "",
	} {
		if got := downloadDisposition(in); got != want {
			t.Errorf("downloadDisposition(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// ---------- 测试环境 ----------

const (
	// testAdminToken 是测试 Server 的 admin token。
	testAdminToken = "admin-token-0123456789"
	// testWebhookSecret 是配置了 GitHub App 时的 webhook 密钥。
	testWebhookSecret = "whsecret"
)

// testKey 是全部测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// testKeyPEM 返回 testKey 的 PEM（PKCS#1）。
func testKeyPEM(t *testing.T) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())}))
}

// newTestServer 用独立的数据库（pgtest）和临时数据目录创建 Server；默认不配置 GitHub App。
func newTestServer(t *testing.T, mutate ...func(c *Config)) *Server {
	t.Helper()
	c := DefaultConfig()
	c.DatabaseURL = pgtest.URL(t)
	c.DataDir = t.TempDir()
	c.AdminToken = testAdminToken
	c.MasterKey = testMasterKey
	c.PublicURL = "http://kiln.test"
	for _, f := range mutate {
		f(c)
	}
	s, err := New(t.Context(), c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// withGitHubApp 让 Server 使用模拟的 GitHub（fake）作为 GitHub App。
func withGitHubApp(t *testing.T, fake *fakegithub.Server) func(c *Config) {
	t.Helper()
	return func(c *Config) {
		c.GitHubAppID = fake.AppID
		c.GitHubPrivateKey = []byte(testKeyPEM(t))
		c.GitHubAPIURL, c.GitHubUploadURL = fake.URL, fake.URL
		c.GitHubWebhookSecret = testWebhookSecret
		c.AllowedAccounts = map[string]bool{"tester": true}
	}
}

// response 是一次请求的结果。
type response struct {
	Code   int
	Header http.Header
	Body   string
}

// send 发送请求并读取完整的响应。
func send(t *testing.T, method, u string, body io.Reader, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{Code: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

// do 发送请求并返回状态码和响应体。
func do(t *testing.T, method, u string, body io.Reader, header map[string]string) (int, string) {
	t.Helper()
	r := send(t, method, u, body, header)
	return r.Code, r.Body
}

// tryGet 发送 GET 请求并返回状态码；连接失败时返回错误（用于等待服务启动或确认已停止）。
func tryGet(ctx context.Context, u string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// ---------- 路由 ----------

// TestHandlerRoutes：healthz、webhook、Runner API、Admin API 和 Dashboard 都挂在各自的路径上。
func TestHandlerRoutes(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	rpcJSON := map[string]string{"Content-Type": "application/json"}
	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		header   map[string]string
		want     int
		wantBody string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz", want: http.StatusOK, wantBody: "ok\n"},
		{name: "没有配置 GitHub App 时 webhook 返回 503", method: http.MethodPost, path: "/webhook", body: "{}", want: http.StatusServiceUnavailable},
		{name: "webhook 只接受 POST（GET 落到 Dashboard 的 404）", method: http.MethodGet, path: "/webhook", want: http.StatusNotFound},
		{
			name: "Runner API：注册令牌无效", method: http.MethodPost, path: runnerv1connect.RunnerServiceEnrollProcedure,
			body: `{"token":"bad","name":"r1"}`, header: rpcJSON, want: http.StatusForbidden, wantBody: "permission_denied",
		},
		{
			name: "Admin API：没有 token", method: http.MethodPost, path: apiv1connect.AdminServiceListReposProcedure,
			body: `{}`, header: rpcJSON, want: http.StatusUnauthorized, wantBody: "unauthenticated",
		},
		{
			name: "Admin API：带 token", method: http.MethodPost, path: apiv1connect.AdminServiceListReposProcedure, body: `{}`,
			header: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testAdminToken}, want: http.StatusOK,
		},
		{name: "Dashboard 登录页", method: http.MethodGet, path: "/login", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := do(t, tt.method, srv.URL+tt.path, strings.NewReader(tt.body), tt.header)
			if code != tt.want {
				t.Fatalf("%s %s = %d，期望 %d：%s", tt.method, tt.path, code, tt.want, body)
			}
			if !strings.Contains(body, tt.wantBody) {
				t.Fatalf("响应中没有 %q：%s", tt.wantBody, body)
			}
		})
	}
}

// TestHealthzDatabaseDown：数据库不可用时 healthz 返回 503。
func TestHealthzDatabaseDown(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	h := s.Handler()
	s.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "db:") {
		t.Fatalf("healthz = %d %q，期望 503", rec.Code, rec.Body.String())
	}
}

// TestWebhookWithGitHubApp：配置了 GitHub App 时 /webhook 校验签名后接受事件。
func TestWebhookWithGitHubApp(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	s := newTestServer(t, withGitHubApp(t, fake))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	if err := fakegithub.SendWebhook(t.Context(), srv.URL+"/webhook", testWebhookSecret, "ping", map[string]any{"zen": "hi"}); err != nil {
		t.Fatalf("合法的 webhook：%v", err)
	}
	err := fakegithub.SendWebhook(t.Context(), srv.URL+"/webhook", "wrong", "ping", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("签名错误的 webhook：%v，期望 401", err)
	}
}

// TestLoadApp：启动时读取 App 信息，用于识别本 App 的 bot 账号和 Dashboard 中的安装链接；读取失败时不影响其他功能。
func TestLoadApp(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	s := newTestServer(t, withGitHubApp(t, fake))
	if s.appSlug() != "" {
		t.Fatal("读取 App 信息之前 appSlug 应为空")
	}
	// 首次查询尚未读到 App 信息（isSelf 复用 checkSelf）：返回 ready=false，并在后台安排懒加载。
	if self, ready := s.checkSelf("kiln-test[bot]"); self || ready {
		t.Fatalf("读取 App 信息之前 checkSelf = (%v, %v)，想要身份未就绪", self, ready)
	}
	s.loadApp(t.Context())
	if s.appSlug() != "kiln-test" {
		t.Fatalf("appSlug = %q", s.appSlug())
	}
	if self, ready := s.checkSelf("kiln-test[bot]"); !self || !ready {
		t.Fatalf("读取成功之后 checkSelf(kiln-test[bot]) = (%v, %v)，想要识别为本 App、身份已就绪", self, ready)
	}
	// 身份已经就绪、只是这个不是本 App：ready 仍为真（调用方据此区分“不是本 App”和“还不知道”）。
	if self, ready := s.checkSelf("other[bot]"); self || !ready {
		t.Fatalf("读取成功之后 checkSelf(other[bot]) = (%v, %v)，想要不是本 App、身份已就绪", self, ready)
	}
	for login, want := range map[string]bool{"kiln-test[bot]": true, "Kiln-Test[bot]": true, "kiln-test": false, "other[bot]": false} {
		if got := s.isSelf(login); got != want {
			t.Errorf("isSelf(%q) = %v，期望 %v", login, got, want)
		}
	}

	down := newTestServer(t, withGitHubApp(t, fake), func(c *Config) { c.GitHubAPIURL = "http://127.0.0.1:1" })
	down.loadApp(t.Context())
	if down.appSlug() != "" || down.isSelf("kiln-test[bot]") {
		t.Fatal("读取 App 信息失败时不应识别 bot 账号")
	}
	if self, ready := down.checkSelf("kiln-test[bot]"); self || ready {
		t.Fatalf("读取失败之后 checkSelf = (%v, %v)，想要身份未就绪", self, ready)
	}
}

// TestNewInvalidPrivateKey：App 私钥无效时 New 返回错误。
func TestNewInvalidPrivateKey(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.DatabaseURL = pgtest.URL(t)
	c.DataDir = t.TempDir()
	c.AdminToken = testAdminToken
	c.MasterKey = testMasterKey
	c.GitHubAppID = 1
	c.GitHubPrivateKey = []byte("not a pem")
	if s, err := New(t.Context(), c, slog.New(slog.DiscardHandler)); err == nil {
		s.Close()
		t.Fatal("私钥无效时 New 应返回错误")
	}
}

// ---------- CAS 预签名 ----------

// TestHandleCAS：预签名 URL 的上传和下载；签名错误、过期、digest 无效时返回 403，操作和方法不匹配时返回 405。
func TestHandleCAS(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	content := []byte("hello kiln")
	digest := cas.Digest(content)
	putURL := srv.URL + s.keys.CASURL(auth.CASOpPut, digest, time.Minute)
	getURL := srv.URL + s.keys.CASURL(auth.CASOpGet, digest, time.Minute)

	// 上传之前：下载返回 404
	if code, _ := do(t, http.MethodGet, getURL, nil, nil); code != http.StatusNotFound {
		t.Fatalf("上传之前下载 = %d，期望 404", code)
	}
	if code, body := do(t, http.MethodPut, putURL, bytes.NewReader(content), nil); code != http.StatusCreated {
		t.Fatalf("上传 = %d：%s", code, body)
	}
	var size int64
	if err := s.db.QueryRow(t.Context(), `SELECT size FROM blobs WHERE digest=$1`, digest).Scan(&size); err != nil || size != int64(len(content)) {
		t.Fatalf("blobs 记录 = %d, %v，期望大小 %d", size, err, len(content))
	}

	t.Run("下载", func(t *testing.T) {
		resp := send(t, http.MethodGet, getURL+"&filename="+url.QueryEscape("dist/app.tgz"), nil, nil)
		if resp.Code != http.StatusOK || resp.Body != string(content) {
			t.Fatalf("下载 = %d %q", resp.Code, resp.Body)
		}
		if got := resp.Header.Get("Content-Disposition"); got != "attachment; filename=app.tgz" {
			t.Errorf("Content-Disposition = %q", got)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("响应头 = %v", resp.Header)
		}
	})
	t.Run("HEAD", func(t *testing.T) {
		resp := send(t, http.MethodHead, getURL, nil, nil)
		if resp.Code != http.StatusOK || resp.Header.Get("Content-Length") != strconv.Itoa(len(content)) || resp.Body != "" {
			t.Fatalf("HEAD = %d，Content-Length %q，响应体 %q", resp.Code, resp.Header.Get("Content-Length"), resp.Body)
		}
	})

	q := func(op, exp, sig string) string {
		return fmt.Sprintf("%s/cas/%s?op=%s&exp=%s&sig=%s", srv.URL, digest, op, exp, sig)
	}
	signed, err := url.Parse(getURL)
	if err != nil {
		t.Fatal(err)
	}
	exp, sig := signed.Query().Get("exp"), signed.Query().Get("sig")
	tests := []struct {
		name   string
		method string
		url    string
		want   int
	}{
		{"签名错误", http.MethodGet, q(auth.CASOpGet, exp, "AAAA"), http.StatusForbidden},
		{"签名用于其他操作", http.MethodPut, q(auth.CASOpPut, exp, sig), http.StatusForbidden},
		{"有效期被篡改", http.MethodGet, q(auth.CASOpGet, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), sig), http.StatusForbidden},
		{"已过期", http.MethodGet, srv.URL + s.keys.CASURL(auth.CASOpGet, digest, -time.Minute), http.StatusForbidden},
		{"digest 无效", http.MethodGet, srv.URL + s.keys.CASURL(auth.CASOpGet, "sha256:xyz", time.Minute), http.StatusForbidden},
		{"下载签名用于 PUT", http.MethodPut, getURL, http.StatusMethodNotAllowed},
		{"上传签名用于 GET", http.MethodGet, putURL, http.StatusMethodNotAllowed},
		{"下载签名用于 POST", http.MethodPost, getURL, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, body := do(t, tt.method, tt.url, strings.NewReader("x"), nil); code != tt.want {
				t.Fatalf("%s = %d，期望 %d：%s", tt.method, code, tt.want, body)
			}
		})
	}

	t.Run("内容与 digest 不符", func(t *testing.T) {
		other := cas.Digest([]byte("other"))
		u := srv.URL + s.keys.CASURL(auth.CASOpPut, other, time.Minute)
		if code, _ := do(t, http.MethodPut, u, strings.NewReader("tampered"), nil); code != http.StatusBadRequest {
			t.Fatalf("上传 = %d，期望 400", code)
		}
	})
}

// ---------- 请求日志 ----------

// TestLogRequests：以 debug 级别记录请求的方法、路径和状态码；静态文件、healthz 和 Runner 成功的调用不记录，Runner 失败的调用记录。
func TestLogRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		path   string
		status int
		logged bool
	}{
		{"普通页面", "/runs", http.StatusOK, true},
		{"错误", "/runs/404", http.StatusNotFound, true},
		{"healthz", "/healthz", http.StatusOK, false},
		{"静态文件", "/assets/index-0f3a.js", http.StatusOK, false},
		{"图标", "/favicon.svg", http.StatusOK, false},
		{"Runner 成功", runnerv1connect.RunnerServiceHeartbeatProcedure, http.StatusOK, false},
		{"Runner 失败", runnerv1connect.RunnerServiceHeartbeatProcedure, http.StatusUnauthorized, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			h := logRequests(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != http.StatusOK {
					w.WriteHeader(tt.status)
				}
				_, _ = io.WriteString(w, "x")
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))
			if rec.Code != tt.status {
				t.Fatalf("状态码 %d，期望 %d", rec.Code, tt.status)
			}
			out := buf.String()
			if logged := out != ""; logged != tt.logged {
				t.Fatalf("记录了日志 %v，期望 %v：%s", logged, tt.logged, out)
			}
			if tt.logged && (!strings.Contains(out, "path="+tt.path) || !strings.Contains(out, "status="+strconv.Itoa(tt.status)) ||
				!strings.Contains(out, "method=GET")) {
				t.Fatalf("日志中缺少方法、路径或状态码：%s", out)
			}
		})
	}
}

// TestStatusWriterFlush：记录状态码的包装支持 Flush（日志流式输出需要）。
func TestStatusWriterFlush(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	h := logRequests(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("包装后的 ResponseWriter 不支持 Flush")
			return
		}
		f.Flush()
	}))
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if !rec.Flushed {
		t.Fatal("Flush 没有传给底层的 ResponseWriter")
	}
}

// ---------- Run ----------

// freeAddr 返回一个本机空闲的监听地址。
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestRun：Run 启动 HTTP 服务和后台循环；ctx 取消后停止 HTTP 服务、等待后台循环退出再返回。
func TestRun(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)
	s := newTestServer(t, func(c *Config) { c.Listen = addr })
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		code, err := tryGet(t.Context(), "http://"+addr+"/healthz")
		if err == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run 启动后 10 秒内 /healthz 不可用：%v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ctx 取消后 Run 返回 %v，期望 nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ctx 取消后 Run 没有返回")
	}
	if _, err := tryGet(t.Context(), "http://"+addr+"/healthz"); err == nil {
		t.Fatal("Run 返回后 HTTP 服务应已停止")
	}
}

// TestRunListenError：监听地址被占用时 Run 返回错误（并停止已经启动的后台循环）。
func TestRunListenError(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := newTestServer(t, func(c *Config) { c.Listen = l.Addr().String() })
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(t.Context()) }()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("监听地址被占用时 Run 应返回错误")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("监听失败后 Run 没有返回")
	}
}

// TestRunWithGitHubApp：配置了 GitHub App 时 Run 先读取 App 信息再启动。
func TestRunWithGitHubApp(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	addr := freeAddr(t)
	s := newTestServer(t, withGitHubApp(t, fake), func(c *Config) { c.Listen = addr })
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for s.appSlug() == "" {
		if time.Now().After(deadline) {
			t.Fatal("Run 启动后 10 秒内没有读取 App 信息")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run 返回 %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ctx 取消后 Run 没有返回")
	}
}

// 启动时没能读到 App 信息时，事件处理中的懒加载会重试并在成功之后缓存：之后能认出本 App 的 bot，而且不再请求 GitHub。
func TestIsSelfLazyLoadsApp(t *testing.T) {
	t.Parallel()
	fake := fakegithub.New("alice")
	t.Cleanup(fake.Close)
	s := &Server{
		gh:  &gh.Client{AppID: fake.AppID, Key: testKey(), APIURL: fake.URL, UploadURL: fake.URL},
		log: slog.New(slog.DiscardHandler),
	}
	// 第一次调用安排后台重试，这次还不能判断（不能在之后立刻再调 checkSelf，否则会与已启动的后台加载竞态）。
	if s.isSelf("alice") {
		t.Fatal("还没有读到 App 信息时不应认出 bot")
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.app.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	app := s.app.Load()
	if app == nil {
		t.Fatal("懒加载没有读到 App 信息")
	}
	if !s.isSelf(fake.AppSlug+"[bot]") || s.isSelf("alice") {
		t.Fatalf("懒加载之后的自识别：slug=%q，bot=%q", s.appSlug(), app.BotLogin)
	}
	if self, ready := s.checkSelf(fake.AppSlug + "[bot]"); !self || !ready {
		t.Fatalf("懒加载之后 checkSelf(bot) = (%v, %v)，想要识别为本 App、身份已就绪", self, ready)
	}
	if self, ready := s.checkSelf("alice"); self || !ready {
		t.Fatalf("懒加载之后 checkSelf(alice) = (%v, %v)，想要不是本 App、身份已就绪", self, ready)
	}
	// 成功之后不再请求 GitHub：把上次尝试时间推到很久以前，再调用也不会重新请求（app 已缓存）。
	s.appTried.Store(time.Now().Add(-time.Hour).UnixNano())
	if !s.isSelf(app.BotLogin) {
		t.Fatal("缓存之后应继续认出 bot")
	}
}

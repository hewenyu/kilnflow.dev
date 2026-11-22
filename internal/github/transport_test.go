package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey 是测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// recordedRequest 是模拟 GitHub 收到的一个请求。
type recordedRequest struct {
	Method string
	Path   string // 转义后的路径（EscapedPath），用来检查路径中各段的转义
	Query  url.Values
	Header http.Header
	Body   []byte
}

// key 返回 "METHOD path"，与 fakeAPI 的路由键相同。
func (r recordedRequest) key() string { return r.Method + " " + r.Path }

// jsonBody 把请求体解码为 map，便于比较字段。
func (r recordedRequest) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%s 的请求体不是 JSON 对象：%v（%q）", r.key(), err, r.Body)
	}
	return m
}

// fakeAPI 是记录请求、按 "METHOD 转义后的路径" 路由的模拟 GitHub。
// 签发 installation token 的请求单独记录：token 依次为 tok1、tok2……，有效期到模拟时钟起点之后 1 小时。
type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	start time.Time

	mu          sync.Mutex
	routes      map[string]http.HandlerFunc
	reqs        []recordedRequest
	tokenReqs   []recordedRequest
	tokenStatus int // 非 0 时签发 token 返回这个状态码
}

// newFakeAPI 启动模拟 GitHub，返回指向它的 Client 和可以修改的模拟时钟。
func newFakeAPI(t *testing.T) (*fakeAPI, *Client, *time.Time) {
	t.Helper()
	f := &fakeAPI{t: t, start: time.Unix(1_800_000_000, 0), routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	now := f.start
	c := &Client{AppID: 42, Key: testKey(), APIURL: f.srv.URL, UploadURL: f.srv.URL}
	c.clock = func() time.Time { return now }
	return f, c, &now
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec := recordedRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		f.mu.Lock()
		f.tokenReqs = append(f.tokenReqs, rec)
		n, status := len(f.tokenReqs), f.tokenStatus
		f.mu.Unlock()
		if status != 0 {
			writeTestJSON(w, status, map[string]any{"message": "Not Found"})
			return
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{"token": fmt.Sprintf("tok%d", n), "expires_at": f.start.Add(time.Hour)})
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	h := f.routes[rec.key()]
	f.mu.Unlock()
	if h == nil {
		writeTestJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	h(w, r)
}

// handle 注册路由，key 为 "METHOD 转义后的路径"。
func (f *fakeAPI) handle(key string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = h
}

// requests 返回收到的 API 请求（不含签发 token）。
func (f *fakeAPI) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.reqs...)
}

// only 断言只收到一个 API 请求并返回它。
func (f *fakeAPI) only(t *testing.T) recordedRequest {
	t.Helper()
	reqs := f.requests()
	if len(reqs) != 1 {
		keys := make([]string, len(reqs))
		for i, r := range reqs {
			keys[i] = r.key()
		}
		t.Fatalf("应只收到 1 个请求，实际 %d 个：%v", len(reqs), keys)
	}
	return reqs[0]
}

func (f *fakeAPI) tokens() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.tokenReqs...)
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonReply 返回固定状态码和 JSON 响应的 handler。
func jsonReply(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeTestJSON(w, status, v) }
}

// rawReply 返回固定状态码和原样响应体的 handler。
func rawReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// 每个请求都带上 GitHub 要求的头部；JSON 请求体带 Content-Type，GET 不带请求体。
func TestInstDo_RequestFormat(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("POST /repos/o/r/things", jsonReply(http.StatusCreated, map[string]any{"id": 7}))
	f.handle("GET /repos/o/r/things/7", jsonReply(http.StatusOK, map[string]any{"id": 7}))
	inst := c.Inst(5)

	var created struct{ ID int64 }
	if err := inst.Do(t.Context(), http.MethodPost, "/repos/o/r/things", map[string]any{"a": 1}, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != 7 {
		t.Errorf("响应解码：%+v", created)
	}
	if err := inst.Do(t.Context(), http.MethodGet, "/repos/o/r/things/7", nil, nil); err != nil {
		t.Fatal(err)
	}

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("请求数 %d，应为 2", len(reqs))
	}
	for _, r := range reqs {
		for h, want := range map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
			"User-Agent":           "kiln-server",
			"Authorization":        "token tok1", // token 被缓存复用
		} {
			if got := r.Header.Get(h); got != want {
				t.Errorf("%s 的头部 %s = %q，应为 %q", r.key(), h, got, want)
			}
		}
	}
	post, get := reqs[0], reqs[1]
	if ct := post.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("POST 的 Content-Type = %q", ct)
	}
	if string(post.Body) != `{"a":1}` {
		t.Errorf("POST 请求体 = %q", post.Body)
	}
	if ct := get.Header.Get("Content-Type"); ct != "" || len(get.Body) != 0 {
		t.Errorf("GET 不应带请求体：Content-Type=%q body=%q", ct, get.Body)
	}

	// 签发 token 用 App JWT，请求体为空对象（全部仓库、全部权限）
	toks := f.tokens()
	if len(toks) != 1 {
		t.Fatalf("应只签发 1 个 token，实际 %d 个", len(toks))
	}
	if toks[0].Path != "/app/installations/5/access_tokens" || !strings.HasPrefix(toks[0].Header.Get("Authorization"), "Bearer ") {
		t.Errorf("签发 token 的请求：%s Authorization=%q", toks[0].key(), toks[0].Header.Get("Authorization"))
	}
	if string(toks[0].Body) != "{}" {
		t.Errorf("签发 token 的请求体 = %q，应为 {}", toks[0].Body)
	}
}

// APIURL 末尾的 / 被去掉；没有配置时使用 GitHub 的默认地址。
func TestClientURLs(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	c.APIURL = f.srv.URL + "/"
	f.handle("GET /app", jsonReply(http.StatusOK, map[string]any{"id": 42}))
	if _, err := c.GetApp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := f.only(t).Path; p != "/app" {
		t.Errorf("路径 = %q，应为 /app", p)
	}

	var zero Client
	if zero.api() != "https://api.github.com" || zero.uploads() != "https://uploads.github.com" {
		t.Errorf("默认地址：%s %s", zero.api(), zero.uploads())
	}
	if zero.httpc() != defaultHTTP {
		t.Error("没有配置 HTTP 时应使用带超时和重定向策略的默认客户端")
	}
	if defaultHTTP.Timeout != defaultHTTPTimeout || defaultHTTP.CheckRedirect == nil {
		t.Error("默认客户端应带总超时和 CheckRedirect")
	}
}

// 错误响应转换为 *APIError：JSON 取 message（附带 errors），否则取截断后的原文。
func TestAPIError(t *testing.T) {
	long := strings.Repeat("x", 1000)
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		status   int
		wantMsg  string
		notFound bool
	}{
		{"404 JSON", jsonReply(http.StatusNotFound, map[string]any{"message": "Not Found"}), 404, "Not Found", true},
		{"422 带 errors", jsonReply(http.StatusUnprocessableEntity, map[string]any{
			"message": "Validation Failed", "errors": []any{map[string]any{"code": "already_exists", "field": "tag_name"}},
		}), 422, `Validation Failed [{"code":"already_exists","field":"tag_name"}]`, false},
		{"500 纯文本去掉首尾空白", rawReply(http.StatusInternalServerError, "  oops\n"), 500, "oops", false},
		{"502 过长的原文被截断", rawReply(http.StatusBadGateway, long), 502, long[:maxErrorMessage], false},
		{"409 JSON 没有 message 时取原文", rawReply(http.StatusConflict, `{"error":"x"}`), 409, `{"error":"x"}`, false},
		{"301 也是错误", rawReply(http.StatusMovedPermanently, ""), 301, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r", tt.handler)
			repo, err := c.Inst(1).GetRepo(t.Context(), "o/r")
			if repo != nil {
				t.Errorf("出错时应返回 nil，实际 %+v", repo)
			}
			ae, ok := errors.AsType[*APIError](err)
			if !ok {
				t.Fatalf("应返回 *APIError，实际 %T：%v", err, err)
			}
			if ae.Status != tt.status || ae.Message != tt.wantMsg || ae.Method != http.MethodGet || ae.URL != f.srv.URL+"/repos/o/r" {
				t.Errorf("APIError = %+v，期望状态 %d、消息 %q", ae, tt.status, tt.wantMsg)
			}
			if want := fmt.Sprintf("GitHub GET %s/repos/o/r：%d %s", f.srv.URL, tt.status, tt.wantMsg); ae.Error() != want {
				t.Errorf("Error() = %q，应为 %q", ae.Error(), want)
			}
			if IsNotFound(err) != tt.notFound {
				t.Errorf("IsNotFound = %v，应为 %v", !tt.notFound, tt.notFound)
			}
			if _, limited := RateLimited(err); limited {
				t.Error("不是限流错误")
			}
		})
	}
	if IsNotFound(nil) || IsNotFound(errors.New("404")) {
		t.Error("nil 和普通错误不是 404")
	}
	if !IsNotFound(fmt.Errorf("包装：%w", &APIError{Status: 404})) {
		t.Error("包装后的 404 也应识别")
	}
}

// 成功响应的解码：空响应体不解码，非 JSON 报错。
func TestDo_ResponseDecoding(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /bad", rawReply(http.StatusOK, "<html>"))
	f.handle("DELETE /empty", rawReply(http.StatusNoContent, ""))
	f.handle("GET /ignored", rawReply(http.StatusOK, "not json"))
	inst := c.Inst(1)

	var out map[string]any
	err := inst.Do(t.Context(), http.MethodGet, "/bad", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "解析 GitHub 响应 GET") {
		t.Errorf("非 JSON 响应应报解析错误：%v", err)
	}
	if err := inst.Do(t.Context(), http.MethodDelete, "/empty", nil, &out); err != nil || out != nil {
		t.Errorf("204 空响应：err=%v out=%v", err, out)
	}
	if err := inst.Do(t.Context(), http.MethodGet, "/ignored", nil, nil); err != nil {
		t.Errorf("out 为 nil 时不解码响应：%v", err)
	}
}

// 请求体无法编码、请求无法构造、连接失败时返回普通错误，不发送请求。
func TestDo_LocalErrors(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	inst := c.Inst(1)

	err := inst.Do(t.Context(), http.MethodPost, "/x", map[string]any{"ch": make(chan int)}, nil)
	if err == nil || !strings.Contains(err.Error(), "编码请求 POST /x") {
		t.Errorf("无法编码的请求体：%v", err)
	}
	if err := inst.Do(t.Context(), "BAD METHOD", "/x", nil, nil); err == nil || !strings.Contains(err.Error(), "构造请求") {
		t.Errorf("无效的 method：%v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("本地错误不应发出请求，实际 %d 个", n)
	}

	wantErr := errors.New("连接被拒绝")
	c.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, wantErr })}
	if err := c.App(t.Context(), http.MethodGet, "/app", nil, nil); !errors.Is(err, wantErr) {
		t.Errorf("连接失败应返回底层错误：%v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.HTTP = nil
	if err := c.App(ctx, http.MethodGet, "/app", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 context：%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 401 时清掉缓存的 token：JSON 请求体用新 token 重试一次；仍然 401 时返回错误，下次调用重新签发。
func TestInstDo_UnauthorizedRetry(t *testing.T) {
	tests := []struct {
		name       string
		reject     func(auth string) bool
		wantErr    bool
		wantCalls  int
		wantTokens int
	}{
		{"旧 token 被吊销，新 token 成功", func(auth string) bool { return auth == "token tok1" }, false, 2, 2},
		{"新 token 也被拒绝", func(string) bool { return true }, true, 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("PATCH /repos/o/r/check-runs/1", func(w http.ResponseWriter, r *http.Request) {
				if tt.reject(r.Header.Get("Authorization")) {
					writeTestJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
					return
				}
				writeTestJSON(w, http.StatusOK, map[string]any{})
			})
			err := c.Inst(1).UpdateCheckRun(t.Context(), "o/r", 1, &CheckRun{Status: CheckStatusCompleted})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v，wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && !isStatus(err, http.StatusUnauthorized) {
				t.Errorf("应返回 401：%v", err)
			}
			reqs := f.requests()
			if len(reqs) != tt.wantCalls || len(f.tokens()) != tt.wantTokens {
				t.Fatalf("请求 %d 次、签发 token %d 次，应为 %d、%d", len(reqs), len(f.tokens()), tt.wantCalls, tt.wantTokens)
			}
			// 重试时请求体完整重发
			for _, r := range reqs {
				if got := r.jsonBody(t)["status"]; got != CheckStatusCompleted {
					t.Errorf("第 %s 次请求体不完整：%q", r.Header.Get("Authorization"), r.Body)
				}
			}
			if tt.wantErr {
				// 被拒绝的 token 不再缓存：下次调用重新签发
				_ = c.Inst(1).UpdateCheckRun(t.Context(), "o/r", 1, &CheckRun{})
				if n := len(f.tokens()); n != 3 {
					t.Errorf("被拒绝后应重新签发 token，签发次数 %d", n)
				}
			}
		})
	}
}

// installation token 按 installation 缓存，剩余有效期不超过 5 分钟时重新签发。
func TestInstallationTokenCache(t *testing.T) {
	f, c, now := newFakeAPI(t)
	f.handle("GET /repos/o/r", jsonReply(http.StatusOK, map[string]any{"id": 1}))
	auth := func(inst int64) string {
		t.Helper()
		before := len(f.requests())
		if _, err := c.Inst(inst).GetRepo(t.Context(), "o/r"); err != nil {
			t.Fatal(err)
		}
		return f.requests()[before].Header.Get("Authorization")
	}
	if got := auth(1); got != "token tok1" {
		t.Fatalf("第一次请求：%q", got)
	}
	if got := auth(2); got != "token tok2" {
		t.Errorf("不同 installation 各自签发：%q", got)
	}
	expires := f.start.Add(time.Hour)
	*now = expires.Add(-tokenRefreshBefore - time.Second)
	if got := auth(1); got != "token tok1" {
		t.Errorf("剩余 5 分 1 秒时应复用：%q", got)
	}
	*now = expires.Add(-tokenRefreshBefore)
	if got := auth(1); got != "token tok3" {
		t.Errorf("剩余 5 分钟时应重新签发：%q", got)
	}
	if toks := f.tokens(); len(toks) != 3 || toks[1].Path != "/app/installations/2/access_tokens" {
		t.Errorf("签发记录：%d 个", len(toks))
	}
}

// 签发 token 失败时不发出 API 请求，错误原样返回。
func TestInstDo_TokenError(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.mu.Lock()
	f.tokenStatus = http.StatusNotFound
	f.mu.Unlock()
	_, err := c.Inst(9).GetRepo(t.Context(), "o/r")
	if !IsNotFound(err) {
		t.Fatalf("应返回签发 token 的 404：%v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("没有 token 时不应发出请求，实际 %d 个", n)
	}
}

// rawBody 原样发送，带 Content-Type 和 Content-Length。
func TestDo_RawBody(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("POST /raw", jsonReply(http.StatusCreated, map[string]any{}))
	body := rawBody{r: strings.NewReader("hello"), size: 5, contentType: "text/plain"}
	if err := c.Inst(1).Do(t.Context(), http.MethodPost, "/raw", body, nil); err != nil {
		t.Fatal(err)
	}
	r := f.only(t)
	if string(r.Body) != "hello" || r.Header.Get("Content-Type") != "text/plain" || r.Header.Get("Content-Length") != "5" {
		t.Errorf("rawBody：body=%q Content-Type=%q Content-Length=%q", r.Body, r.Header.Get("Content-Type"), r.Header.Get("Content-Length"))
	}
}

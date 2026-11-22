package oci

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/redact"
)

// 这些测试用内存中的 http.RoundTripper 模拟 registry 和 token 服务，可以使用任意主机名
// （例如必须走 https 的公网 registry），并精确控制每个响应。

const (
	testUser = "alice"
	testPass = "pa55-w0rd-secret"
	testHost = "registry.example.com"
)

// fakeTransport 按顺序记录请求，并把每个请求交给 handle 处理。
type fakeTransport struct {
	mu     sync.Mutex
	reqs   []*http.Request
	handle func(r *http.Request) reply
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	if r.Body != nil {
		// 读完请求体，模拟真实的传输
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	rep := f.handle(r)
	if rep.err != nil {
		return nil, rep.err
	}
	return &http.Response{StatusCode: rep.status, Header: rep.header, Body: io.NopCloser(strings.NewReader(rep.body))}, nil
}

// requests 返回 "METHOD host路径" 列表。
func (f *fakeTransport) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.Method+" "+r.URL.Host+r.URL.Path)
	}
	return out
}

func (f *fakeTransport) count(method, path string) int {
	n := 0
	for _, r := range f.requests() {
		if r == method+" "+testHost+path {
			n++
		}
	}
	return n
}

// reply 是假 registry 对一个请求的回应：响应或网络错误。
type reply struct {
	status int
	header http.Header
	body   string
	err    error
}

func respond(status int, header http.Header, body string) reply {
	if header == nil {
		header = http.Header{}
	}
	return reply{status: status, header: header, body: body}
}

// fail 模拟网络错误。
func fail(err error) reply { return reply{err: err} }

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// newFakeClient 返回访问 registry.example.com/team/app 的客户端；退避时间为 0，重试不等待。
func newFakeClient(t *testing.T, user, pass string, handle func(r *http.Request) reply) (*Client, *fakeTransport) {
	t.Helper()
	name, err := ParseName(testHost + "/team/app")
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{handle: handle}
	c := NewClient(name, user, pass, &http.Client{Transport: ft})
	c.backoff = 0
	return c, ft
}

// basicAuthed 模拟要求 Basic 认证的 registry：没有正确的凭据时返回 401 质询，否则交给 next。
func basicAuthed(next func(r *http.Request) reply) func(r *http.Request) reply {
	return func(r *http.Request) reply {
		if r.URL.Host == testHost {
			if u, p, ok := r.BasicAuth(); !ok || u != testUser || p != testPass {
				return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Basic realm="test"`), "")
			}
		}
		return next(r)
	}
}

func TestParseChallenge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		scheme string
		params map[string]string
	}{
		{
			name: "Docker Hub 的 Bearer 质询", in: `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a/b:pull"`,
			scheme: "Bearer", params: map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io", "scope": "repository:a/b:pull"},
		},
		{name: "Basic", in: `Basic realm="Registry Realm"`, scheme: "Basic", params: map[string]string{"realm": "Registry Realm"}},
		{name: "不带引号的值", in: `Bearer realm=https://a/token, service=reg`, scheme: "Bearer", params: map[string]string{"realm": "https://a/token", "service": "reg"}},
		{name: "参数名不区分大小写", in: `Bearer Realm="https://a"`, scheme: "Bearer", params: map[string]string{"realm": "https://a"}},
		{name: "引号没有闭合", in: `Bearer realm="https://a`, scheme: "Bearer", params: map[string]string{"realm": "https://a"}},
		{name: "只有方案", in: "Basic", scheme: "Basic", params: map[string]string{}},
		{name: "没有等号的参数被忽略", in: "Bearer garbage", scheme: "Bearer", params: map[string]string{}},
		{name: "空", in: "", scheme: "", params: map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			scheme, params := parseChallenge(c.in)
			if scheme != c.scheme {
				t.Errorf("scheme = %q，期望 %q", scheme, c.scheme)
			}
			if len(params) != len(c.params) {
				t.Fatalf("params = %v，期望 %v", params, c.params)
			}
			for k, v := range c.params {
				if params[k] != v {
					t.Errorf("params[%s] = %q，期望 %q", k, params[k], v)
				}
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	e := &Error{Method: "PUT", Path: "/v2/a/manifests/v1", Status: 400}
	if got, want := e.Error(), "PUT /v2/a/manifests/v1：HTTP 400"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	e.Detail = "MANIFEST_INVALID bad"
	if got, want := e.Error(), "PUT /v2/a/manifests/v1：HTTP 400：MANIFEST_INVALID bad"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	if got := pathOf("https://h/v2/a/blobs/uploads/1?digest=sha256:x&_state=secret"); got != "/v2/a/blobs/uploads/1" {
		t.Errorf("pathOf 应去掉查询参数：%q", got)
	}
	if got := pathOf("%zz"); got != "?" {
		t.Errorf("无法解析的地址应显示为 ?：%q", got)
	}
}

func TestNameInsecure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:5000", true},
		{"registry.localhost:5000", true},
		{"127.0.0.1:5000", true},
		{"127.0.0.2", true},
		{"[::1]:5000", true},
		{"10.0.0.1:5000", false},
		{"ghcr.io", false},
		{"localhost.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			t.Parallel()
			if got := (Name{Host: c.host}).Insecure(); got != c.want {
				t.Errorf("Insecure(%s) = %v，期望 %v", c.host, got, c.want)
			}
		})
	}
}

func TestParseNameEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want, host string
		ok                   bool
	}{
		{name: "Docker Hub 的实际地址", in: "registry-1.docker.io/nginx", want: "docker.io/library/nginx", host: dockerHubHost, ok: true},
		{name: "registry 转为小写", in: "GHCR.IO/o/r", want: "ghcr.io/o/r", host: "ghcr.io", ok: true},
		{name: "localhost 带端口", in: "localhost:5000/a/b", want: "localhost:5000/a/b", host: "localhost:5000", ok: true},
		{name: "允许的分隔符", in: "ghcr.io/o/a__b.c-d", want: "ghcr.io/o/a__b.c-d", host: "ghcr.io", ok: true},
		{name: "仓库路径正好 255 个字符", in: "ghcr.io/o/" + strings.Repeat("a", 253), want: "ghcr.io/o/" + strings.Repeat("a", 253), host: "ghcr.io", ok: true},
		{name: "仓库路径超过 255 个字符", in: "ghcr.io/o/" + strings.Repeat("a", 254), ok: false},
		{name: "仓库路径为空", in: "ghcr.io/", ok: false},
		{name: "带 digest", in: "ghcr.io/o/r@sha256:abc", ok: false},
		{name: "以分隔符开头", in: "ghcr.io/o/-r", ok: false},
		{name: "以斜杠结尾", in: "ghcr.io/o/r/", ok: false},
		{name: "只有 registry", in: "ghcr.io", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n, err := ParseName(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("ParseName(%q) err = %v，期望成功 = %v", c.in, err, c.ok)
			}
			if c.ok && (n.String() != c.want || n.Host != c.host) {
				t.Errorf("ParseName(%q) = %s（host %s），期望 %s（host %s）", c.in, n, n.Host, c.want, c.host)
			}
		})
	}
}

// TestBearerTokenFlow 按 Docker 的 token 认证流程：401 质询 → 用用户名密码向 realm 换 token → 带 token 重发。
func TestBearerTokenFlow(t *testing.T) {
	t.Parallel()
	const token = "tok-0123456789"
	var tokenReq *http.Request
	c, ft := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
		switch r.URL.Host {
		case "auth.example.com":
			tokenReq = r
			return respond(http.StatusOK, nil, `{"access_token":"`+token+`"}`)
		case testHost:
			if r.Header.Get("Authorization") != "Bearer "+token {
				return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token?x=1",service="reg.example"`), "")
			}
			return respond(http.StatusNotFound, nil, "")
		}
		return fail(errors.New("unexpected host " + r.URL.Host))
	})
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping：%v", err)
	}
	reqs := ft.requests()
	want := []string{
		"HEAD " + testHost + "/v2/team/app/manifests/kiln-auth-check",
		"GET auth.example.com/token",
		"HEAD " + testHost + "/v2/team/app/manifests/kiln-auth-check",
	}
	if strings.Join(reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("请求序列：\n%s\n期望：\n%s", strings.Join(reqs, "\n"), strings.Join(want, "\n"))
	}
	if ft.reqs[0].URL.Scheme != "https" {
		t.Errorf("非本机 registry 必须使用 https：%s", ft.reqs[0].URL)
	}
	q := tokenReq.URL.Query()
	if q.Get("service") != "reg.example" || q.Get("scope") != "repository:team/app:pull,push" || q.Get("x") != "1" {
		t.Errorf("token 请求的参数不对：%s", tokenReq.URL.RawQuery)
	}
	if u, p, ok := tokenReq.BasicAuth(); !ok || u != testUser || p != testPass {
		t.Errorf("token 请求应带用户名密码：%q %q %v", u, p, ok)
	}
	if got := ft.reqs[0].Header.Get("Accept"); !strings.Contains(got, MediaTypeOCIIndex) || !strings.Contains(got, MediaTypeDockerManifest) {
		t.Errorf("查询 manifest 的 Accept 头 = %q", got)
	}
}

// TestBearerTokenAnonymous 没有凭据时匿名换取 token（不带 Basic 头）。
func TestBearerTokenAnonymous(t *testing.T) {
	t.Parallel()
	c, ft := newFakeClient(t, "", "", func(r *http.Request) reply {
		if r.URL.Host == "auth.example.com" {
			return respond(http.StatusOK, nil, `{"token":"anon-token"}`)
		}
		if r.Header.Get("Authorization") == "" {
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
		}
		return respond(http.StatusNotFound, nil, "")
	})
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h := ft.reqs[1].Header.Get("Authorization"); h != "" {
		t.Errorf("没有凭据时不应发送 Authorization：%q", h)
	}
	if q := ft.reqs[1].URL.Query(); q.Has("service") {
		t.Errorf("质询中没有 service 时不应带 service 参数：%s", ft.reqs[1].URL.RawQuery)
	}
}

func TestAuthenticationErrors(t *testing.T) {
	t.Parallel()
	challenge := func(h string) func(r *http.Request) reply {
		return func(r *http.Request) reply {
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", h), "")
		}
	}
	// tokenServer 返回固定的 token 响应；registry 总是要求认证
	tokenServer := func(status int, body string) func(r *http.Request) reply {
		return func(r *http.Request) reply {
			if r.URL.Host == "auth.example.com" {
				return respond(status, nil, body)
			}
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
		}
	}
	cases := []struct {
		name         string
		user, pass   string
		handle       func(r *http.Request) reply
		unauthorized bool   // 是否应为 ErrUnauthorized
		want         string // 错误信息应包含的文字
	}{
		{name: "不支持的认证方式", user: testUser, pass: testPass, handle: challenge(`Negotiate`), want: "不支持的认证方式"},
		{name: "没有质询头", user: testUser, pass: testPass, handle: challenge(``), want: "不支持的认证方式"},
		{name: "Basic 但没有凭据", handle: challenge(`Basic realm="x"`), unauthorized: true},
		{name: "认证后仍然 401", user: testUser, pass: testPass, handle: challenge(`Basic realm="x"`), unauthorized: true},
		{name: "token 地址是 http", user: testUser, pass: testPass, handle: challenge(`Bearer realm="http://auth.example.com/token"`), want: "必须是 https"},
		{name: "token 地址为空", user: testUser, pass: testPass, handle: challenge(`Bearer service="x"`), want: "必须是 https"},
		{name: "token 地址无法解析", user: testUser, pass: testPass, handle: challenge(`Bearer realm="https://a b/%zz"`), want: "必须是 https"},
		{name: "token 服务拒绝凭据", user: testUser, pass: testPass, handle: tokenServer(http.StatusUnauthorized, ""), unauthorized: true},
		{name: "token 服务禁止访问", user: testUser, pass: testPass, handle: tokenServer(http.StatusForbidden, ""), unauthorized: true},
		{name: "token 服务出错", user: testUser, pass: testPass, handle: tokenServer(http.StatusInternalServerError, "boom"), want: "HTTP 500：boom"},
		{name: "token 响应不是 JSON", user: testUser, pass: testPass, handle: tokenServer(http.StatusOK, "<html>"), want: "响应不是 JSON"},
		{name: "token 响应中没有 token", user: testUser, pass: testPass, handle: tokenServer(http.StatusOK, `{"expires_in":300}`), want: "响应中没有 token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cl, _ := newFakeClient(t, c.user, c.pass, c.handle)
			err := cl.Ping(t.Context())
			if err == nil {
				t.Fatal("期望认证失败")
			}
			if c.unauthorized != errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v，期望 ErrUnauthorized = %v", err, c.unauthorized)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
			if strings.Contains(err.Error(), testPass) {
				t.Fatalf("错误信息中出现了密码：%v", err)
			}
		})
	}
}

// TestErrorsAreRedacted registry 或 token 服务在响应和网络错误中带回凭据时，错误信息中替换为 ***。
func TestErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	t.Run("token 服务的错误响应", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
			if r.URL.Host == "auth.example.com" {
				return respond(http.StatusBadRequest, nil, "bad password "+testPass)
			}
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
		})
		err := c.Ping(t.Context())
		if err == nil || strings.Contains(err.Error(), testPass) || !strings.Contains(err.Error(), redact.Mask) {
			t.Fatalf("err = %v，密码应被替换为 %s", err, redact.Mask)
		}
	})
	t.Run("token 服务的网络错误", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
			if r.URL.Host == "auth.example.com" {
				return fail(errors.New("proxy rejected credentials " + testPass))
			}
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
		})
		err := c.Ping(t.Context())
		if err == nil || strings.Contains(err.Error(), testPass) || !strings.Contains(err.Error(), "获取 registry token") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("registry 错误详情中的 token", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, testUser, testPass, basicAuthed(func(r *http.Request) reply {
			return respond(http.StatusForbidden, nil, "denied for "+r.Header.Get("Authorization"))
		}))
		_, err := c.BlobExists(t.Context(), "sha256:abc")
		var re *Error
		if !errors.As(err, &re) || re.Status != http.StatusForbidden {
			t.Fatalf("err = %v，期望 *Error 403", err)
		}
		if strings.Contains(err.Error(), testPass) || strings.Contains(re.Detail, strings.TrimPrefix(c.auth, "Basic ")) {
			t.Fatalf("错误详情中出现了凭据：%v", err)
		}
	})
	// 凭据正好落在截断位置（读取上限、token 服务的错误说明）时也不能留下它的开头：先脱敏再截断
	for _, tc := range []struct {
		desc string
		body string
	}{
		{"registry 错误详情的读取上限", strings.Repeat("x", maxErrorBody-6) + testPass},
		{"registry 错误详情的 JSON 说明", `{"errors":[{"code":"DENIED","message":"` + strings.Repeat("x", maxErrorBody-14) + testPass + `"}]}`},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			c, _ := newFakeClient(t, testUser, testPass, basicAuthed(func(*http.Request) reply {
				return respond(http.StatusForbidden, nil, tc.body)
			}))
			_, err := c.BlobExists(t.Context(), "sha256:abc")
			if err == nil || strings.Contains(err.Error(), testPass[:4]) {
				t.Fatalf("错误中不应有密码的一部分：%v", err)
			}
		})
	}
	t.Run("token 服务错误说明的截断位置", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
			if r.URL.Host == "auth.example.com" {
				return respond(http.StatusBadRequest, nil, strings.Repeat("x", maxErrorBody-6)+testPass)
			}
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
		})
		err := c.Ping(t.Context())
		if err == nil || strings.Contains(err.Error(), testPass[:4]) {
			t.Fatalf("错误中不应有密码的一部分：%v", err)
		}
	})
}

func TestRetry(t *testing.T) {
	t.Parallel()
	t.Run("429 和 5xx 重试后成功", func(t *testing.T) {
		t.Parallel()
		statuses := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusOK}
		var mu sync.Mutex
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			mu.Lock()
			defer mu.Unlock()
			s := statuses[0]
			statuses = statuses[1:]
			return respond(s, nil, "")
		})
		ok, err := c.BlobExists(t.Context(), "sha256:abc")
		if err != nil || !ok {
			t.Fatalf("BlobExists = %v, %v", ok, err)
		}
		if n := len(ft.requests()); n != 3 {
			t.Fatalf("请求了 %d 次，期望 3 次", n)
		}
	})
	t.Run("网络错误重试用完", func(t *testing.T) {
		t.Parallel()
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			return fail(errors.New("connection refused"))
		})
		_, err := c.BlobExists(t.Context(), "sha256:abc")
		if err == nil || !strings.Contains(err.Error(), "HEAD /v2/team/app/blobs/sha256:abc") || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("err = %v", err)
		}
		if n := len(ft.requests()); n != defaultAttempts {
			t.Fatalf("请求了 %d 次，期望 %d 次", n, defaultAttempts)
		}
	})
	t.Run("5xx 重试用完后返回最后的响应", func(t *testing.T) {
		t.Parallel()
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			return respond(http.StatusServiceUnavailable, nil, `{"errors":[{"code":"UNAVAILABLE","message":"later"}]}`)
		})
		_, err := c.BlobExists(t.Context(), "sha256:abc")
		var re *Error
		if !errors.As(err, &re) || re.Status != http.StatusServiceUnavailable || re.Detail != "UNAVAILABLE later" {
			t.Fatalf("err = %v", err)
		}
		if n := len(ft.requests()); n != defaultAttempts {
			t.Fatalf("请求了 %d 次，期望 %d 次", n, defaultAttempts)
		}
	})
	t.Run("退避期间取消", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			cancel()
			return respond(http.StatusServiceUnavailable, nil, "")
		})
		_, err := c.BlobExists(ctx, "sha256:abc")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v，期望 context.Canceled", err)
		}
		if n := len(ft.requests()); n != 1 {
			t.Fatalf("取消后不应重试：请求了 %d 次", n)
		}
	})
	t.Run("网络错误时已取消不重试", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			cancel()
			return fail(errors.New("reset"))
		})
		if _, err := c.BlobExists(ctx, "sha256:abc"); err == nil {
			t.Fatal("期望报错")
		}
		if n := len(ft.requests()); n != 1 {
			t.Fatalf("取消后不应重试：请求了 %d 次", n)
		}
	})
}

func TestRegistryErrorDetail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, want string
	}{
		{name: "多个错误码", body: `{"errors":[{"code":"DENIED","message":"no push"},{"code":"NAME_UNKNOWN","message":""}]}`, want: "DENIED no push；NAME_UNKNOWN"},
		{name: "纯文本", body: "  forbidden\n", want: "forbidden"},
		{name: "空的 errors 用原文", body: `{"errors":[]}`, want: `{"errors":[]}`},
		{name: "超长内容被截断", body: strings.Repeat("x", maxErrorBody+100), want: strings.Repeat("x", maxErrorBody)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cl, _ := newFakeClient(t, "", "", func(*http.Request) reply {
				return respond(http.StatusForbidden, nil, c.body)
			})
			_, _, err := cl.ManifestDigest(t.Context(), "v1")
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("err = %v，期望 *Error", err)
			}
			if re.Method != http.MethodHead || re.Path != "/v2/team/app/manifests/v1" || re.Detail != c.want {
				t.Fatalf("Error = %+v，期望 Detail %q", re, c.want)
			}
		})
	}
}

// TestManifestDigestWithoutHeader registry 不返回 Docker-Content-Digest 时取回内容自己计算。
func TestManifestDigestWithoutHeader(t *testing.T) {
	t.Parallel()
	body := `{"schemaVersion":2}`
	c, ft := newFakeClient(t, "", "", func(r *http.Request) reply {
		if r.Method == http.MethodHead {
			return respond(http.StatusOK, nil, "")
		}
		return respond(http.StatusOK, nil, body)
	})
	d, found, err := c.ManifestDigest(t.Context(), "v1")
	if err != nil || !found || d != Digest([]byte(body)) {
		t.Fatalf("ManifestDigest = %s, %v, %v，期望 %s", d, found, err, Digest([]byte(body)))
	}
	if ft.count(http.MethodGet, "/v2/team/app/manifests/v1") != 1 {
		t.Fatalf("应当 GET 一次 manifest：%v", ft.requests())
	}

	// GET 失败时返回 *Error
	c2, _ := newFakeClient(t, "", "", func(r *http.Request) reply {
		if r.Method == http.MethodHead {
			return respond(http.StatusOK, nil, "")
		}
		return respond(http.StatusNotFound, nil, "")
	})
	_, _, err = c2.ManifestDigest(t.Context(), "v1")
	var re *Error
	if !errors.As(err, &re) || re.Method != http.MethodGet || re.Status != http.StatusNotFound {
		t.Fatalf("err = %v，期望 GET 的 *Error", err)
	}
}

func TestPushManifest(t *testing.T) {
	t.Parallel()
	m := Manifest{MediaType: MediaTypeOCIManifest, Body: []byte(`{"schemaVersion":2}`)}
	m.Digest = Digest(m.Body)
	cases := []struct {
		name    string
		status  int
		digest  string // 响应中的 Docker-Content-Digest
		wantErr string
	}{
		{name: "201", status: http.StatusCreated, digest: m.Digest},
		{name: "200 也算成功", status: http.StatusOK},
		{name: "registry 记录的 digest 不一致", status: http.StatusCreated, digest: "sha256:other", wantErr: "不一致"},
		{name: "manifest 无效", status: http.StatusBadRequest, wantErr: "HTTP 400"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got *http.Request
			cl, _ := newFakeClient(t, "", "", func(r *http.Request) reply {
				got = r
				return respond(c.status, hdr("Docker-Content-Digest", c.digest), "")
			})
			err := cl.PushManifest(t.Context(), m, "v1.0.0")
			if c.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v，应包含 %q", err, c.wantErr)
			}
			if got.Method != http.MethodPut || got.URL.Path != "/v2/team/app/manifests/v1.0.0" ||
				got.Header.Get("Content-Type") != MediaTypeOCIManifest || got.ContentLength != int64(len(m.Body)) {
				t.Errorf("请求 = %s %s，Content-Type %q，长度 %d", got.Method, got.URL.Path, got.Header.Get("Content-Type"), got.ContentLength)
			}
		})
	}
}

// blobRegistry 模拟上传流程：POST 返回 Location，PUT 按 status 返回；记录 PUT 请求。
type blobRegistry struct {
	mu        sync.Mutex
	location  string
	putStatus []int // 依次使用；用完后返回 201
	puts      []*http.Request
}

func (b *blobRegistry) handle(r *http.Request) reply {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		return respond(http.StatusAccepted, hdr("Location", b.location), "")
	case http.MethodPut:
		b.puts = append(b.puts, r)
		s := http.StatusCreated
		if len(b.putStatus) > 0 {
			s, b.putStatus = b.putStatus[0], b.putStatus[1:]
		}
		return respond(s, nil, "")
	}
	return respond(http.StatusMethodNotAllowed, nil, "")
}

func TestPushBlob(t *testing.T) {
	t.Parallel()
	content := []byte("layer-bytes")
	d := Descriptor{Digest: Digest(content), Size: int64(len(content))}
	opener := func(opens *int) func(context.Context) (io.ReadCloser, error) {
		return func(context.Context) (io.ReadCloser, error) {
			*opens++
			return io.NopCloser(strings.NewReader(string(content))), nil
		}
	}

	t.Run("相对的上传地址，保留查询参数并加上 digest", func(t *testing.T) {
		t.Parallel()
		reg := &blobRegistry{location: "/v2/team/app/blobs/uploads/abc?_state=s1"}
		c, _ := newFakeClient(t, "", "", reg.handle)
		opens := 0
		if err := c.PushBlob(t.Context(), d, opener(&opens)); err != nil {
			t.Fatal(err)
		}
		put := reg.puts[0]
		if put.URL.Host != testHost || put.URL.Path != "/v2/team/app/blobs/uploads/abc" {
			t.Fatalf("PUT %s", put.URL)
		}
		if q := put.URL.Query(); q.Get("_state") != "s1" || q.Get("digest") != d.Digest {
			t.Fatalf("PUT 的查询参数 = %s", put.URL.RawQuery)
		}
		if put.ContentLength != d.Size || put.Header.Get("Content-Type") != "application/octet-stream" || opens != 1 {
			t.Fatalf("Content-Length %d，Content-Type %q，open %d 次", put.ContentLength, put.Header.Get("Content-Type"), opens)
		}
	})

	t.Run("PUT 失败时从 POST 重新开始", func(t *testing.T) {
		t.Parallel()
		reg := &blobRegistry{location: "/v2/team/app/blobs/uploads/abc", putStatus: []int{http.StatusServiceUnavailable}}
		c, ft := newFakeClient(t, "", "", reg.handle)
		opens := 0
		if err := c.PushBlob(t.Context(), d, opener(&opens)); err != nil {
			t.Fatal(err)
		}
		if n := ft.count(http.MethodPost, "/v2/team/app/blobs/uploads/"); n != 2 || len(reg.puts) != 2 || opens != 2 {
			t.Fatalf("POST %d 次，PUT %d 次，open %d 次，期望都是 2", n, len(reg.puts), opens)
		}
	})

	t.Run("PUT 返回 4xx 不重试", func(t *testing.T) {
		t.Parallel()
		reg := &blobRegistry{location: "/v2/team/app/blobs/uploads/abc", putStatus: []int{http.StatusBadRequest}}
		c, _ := newFakeClient(t, "", "", reg.handle)
		opens := 0
		err := c.PushBlob(t.Context(), d, opener(&opens))
		var re *Error
		if !errors.As(err, &re) || re.Status != http.StatusBadRequest || re.Method != http.MethodPut || len(reg.puts) != 1 {
			t.Fatalf("err = %v，PUT %d 次", err, len(reg.puts))
		}
		if strings.Contains(re.Path, "digest") {
			t.Errorf("错误中的路径不应带查询参数：%s", re.Path)
		}
	})

	t.Run("POST 返回非 202", func(t *testing.T) {
		t.Parallel()
		c, ft := newFakeClient(t, "", "", func(*http.Request) reply {
			return respond(http.StatusNotFound, nil, `{"errors":[{"code":"NAME_UNKNOWN","message":"repo"}]}`)
		})
		err := c.PushBlob(t.Context(), d, opener(new(int)))
		var re *Error
		if !errors.As(err, &re) || re.Method != http.MethodPost || re.Status != http.StatusNotFound || len(ft.requests()) != 1 {
			t.Fatalf("err = %v，请求 %v", err, ft.requests())
		}
	})

	t.Run("没有上传地址", func(t *testing.T) {
		t.Parallel()
		reg := &blobRegistry{location: ""}
		c, _ := newFakeClient(t, "", "", reg.handle)
		err := c.PushBlob(t.Context(), d, opener(new(int)))
		if err == nil || !strings.Contains(err.Error(), "上传地址无效") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("读取 blob 失败", func(t *testing.T) {
		t.Parallel()
		reg := &blobRegistry{location: "/v2/team/app/blobs/uploads/abc"}
		c, _ := newFakeClient(t, "", "", reg.handle)
		errDisk := errors.New("disk gone")
		opens := 0
		err := c.PushBlob(t.Context(), d, func(context.Context) (io.ReadCloser, error) {
			opens++
			return nil, errDisk
		})
		if err == nil || !strings.Contains(err.Error(), "disk gone") || len(reg.puts) != 0 {
			t.Fatalf("err = %v，PUT %d 次", err, len(reg.puts))
		}
		if opens != defaultAttempts {
			t.Fatalf("open 了 %d 次，期望重试到 %d 次", opens, defaultAttempts)
		}
	})

	t.Run("凭据无效时不重试", func(t *testing.T) {
		t.Parallel()
		c, ft := newFakeClient(t, testUser, "wrong-password", basicAuthed(func(*http.Request) reply {
			return respond(http.StatusAccepted, nil, "")
		}))
		err := c.PushBlob(t.Context(), d, opener(new(int)))
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v", err)
		}
		if n := ft.count(http.MethodPost, "/v2/team/app/blobs/uploads/"); n != 2 {
			t.Fatalf("POST %d 次，期望 2 次（质询一次，认证后一次）", n)
		}
	})
}

// TestCredentialsOnlySentToRegistry 上传地址指向其他主机（例如对象存储）时不带 Authorization。
func TestCredentialsOnlySentToRegistry(t *testing.T) {
	t.Parallel()
	content := []byte("blob")
	d := Descriptor{Digest: Digest(content), Size: int64(len(content))}
	var storagePut *http.Request
	c, _ := newFakeClient(t, testUser, testPass, basicAuthed(func(r *http.Request) reply {
		switch {
		case r.URL.Host == "storage.example.com":
			storagePut = r
			return respond(http.StatusCreated, nil, "")
		case r.Method == http.MethodPost:
			return respond(http.StatusAccepted, hdr("Location", "https://storage.example.com/upload/1?sig=xyz"), "")
		}
		return respond(http.StatusNotFound, nil, "")
	}))
	err := c.PushBlob(t.Context(), d, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(content))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if storagePut == nil {
		t.Fatal("没有向对象存储上传")
	}
	if h := storagePut.Header.Get("Authorization"); h != "" {
		t.Fatalf("凭据被发送到了其他主机：%q", h)
	}
	if q := storagePut.URL.Query(); q.Get("sig") != "xyz" || q.Get("digest") != d.Digest {
		t.Fatalf("上传地址的查询参数 = %s", storagePut.URL.RawQuery)
	}
}

// TestCredentialsNotSentOverHTTP https 的 registry 返回非本机的 http 上传地址时直接拒绝（不能把 blob 和凭据经明文发出去）。
func TestCredentialsNotSentOverHTTP(t *testing.T) {
	t.Parallel()
	content := []byte("blob")
	d := Descriptor{Digest: Digest(content), Size: int64(len(content))}
	var plainPut *http.Request
	c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
		switch {
		case r.URL.Scheme == "http":
			plainPut = r
			return respond(http.StatusCreated, nil, "")
		case r.Header.Get("Authorization") == "":
			return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Basic realm="test"`), "")
		case r.Method == http.MethodPost:
			return respond(http.StatusAccepted, hdr("Location", "http://"+testHost+"/upload/1"), "")
		}
		return respond(http.StatusNotFound, nil, "")
	})
	err := c.PushBlob(t.Context(), d, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(content))), nil
	})
	if err == nil || !strings.Contains(err.Error(), "必须是 https") {
		t.Fatalf("err = %v，期望拒绝非本机的 http 上传地址", err)
	}
	if plainPut != nil {
		t.Fatal("不应向明文地址发送内容")
	}
}

// TestPushAndPendingTagsErrors 推送流程中任一步出错时，错误说明是哪一步、哪个对象。
func TestPushAndPendingTagsErrors(t *testing.T) {
	t.Parallel()
	layer := []byte("layer")
	mf := []byte(`{"schemaVersion":2}`)
	img := &Image{
		Blobs:     []Descriptor{{Digest: Digest(layer), Size: int64(len(layer))}},
		Manifests: []Manifest{{MediaType: MediaTypeOCIManifest, Digest: Digest(mf), Size: int64(len(mf)), Body: mf}},
	}
	img.Root = img.Manifests[0].Descriptor
	open := func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(layer))), nil
	}
	forbidden := func(*http.Request) reply { return respond(http.StatusForbidden, nil, "") }

	t.Run("查询 blob 失败", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, "", "", forbidden)
		_, err := Push(t.Context(), c, img, open)
		if err == nil || !strings.Contains(err.Error(), "查询 blob "+img.Blobs[0].Digest) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("上传 blob 失败", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, "", "", func(r *http.Request) reply {
			if r.Method == http.MethodHead {
				return respond(http.StatusNotFound, nil, "")
			}
			return respond(http.StatusForbidden, nil, "")
		})
		st, err := Push(t.Context(), c, img, open)
		if err == nil || !strings.Contains(err.Error(), "上传 blob "+img.Blobs[0].Digest) || st.Uploaded != 0 {
			t.Fatalf("err = %v，stats = %+v", err, st)
		}
	})
	t.Run("推送 manifest 失败", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, "", "", func(r *http.Request) reply {
			switch r.Method {
			case http.MethodHead:
				return respond(http.StatusNotFound, nil, "")
			case http.MethodPost:
				return respond(http.StatusAccepted, hdr("Location", "/v2/team/app/blobs/uploads/1"), "")
			case http.MethodPut:
				if strings.Contains(r.URL.Path, "/blobs/") {
					return respond(http.StatusCreated, nil, "")
				}
			}
			return respond(http.StatusBadRequest, nil, "MANIFEST_INVALID")
		})
		st, err := Push(t.Context(), c, img, open)
		if err == nil || !strings.Contains(err.Error(), "推送 manifest "+img.Root.Digest) {
			t.Fatalf("err = %v", err)
		}
		if st.Uploaded != 1 || st.Bytes != int64(len(layer)) {
			t.Fatalf("失败前已上传的统计 = %+v", st)
		}
	})
	t.Run("查询 tag 失败", func(t *testing.T) {
		t.Parallel()
		c, _ := newFakeClient(t, "", "", forbidden)
		pending, err := PendingTags(t.Context(), c, []string{"v1"}, img.Root.Digest)
		if err == nil || !strings.Contains(err.Error(), "查询 tag v1") || pending != nil {
			t.Fatalf("pending = %v，err = %v", pending, err)
		}
	})
}

// TestRedirectCredentials registry 返回重定向时：不能从 https 降级到 http，
// 跳到其他 origin（跨端口、子域名）时不带凭据，同源的 https 跳转照常带凭据。
func TestRedirectCredentials(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		target   string
		wantErr  bool
		wantAuth bool
	}{
		{name: "同主机降级到 http", target: "http://" + testHost + "/redirected", wantErr: true},
		{name: "跨端口", target: "https://" + testHost + ":8443/redirected"},
		{name: "子域名", target: "https://cdn." + testHost + "/redirected"},
		{name: "其他主机", target: "https://storage.example.com/redirected"},
		{name: "同源 https", target: "https://" + testHost + "/redirected", wantAuth: true},
		{name: "同源 https（显式默认端口）", target: "https://" + testHost + ":443/redirected", wantAuth: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var hops []*http.Request
			c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
				if r.URL.Path == "/redirected" {
					mu.Lock()
					hops = append(hops, r)
					mu.Unlock()
					return respond(http.StatusNotFound, nil, "")
				}
				if r.Header.Get("Authorization") == "" {
					return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Basic realm="test"`), "")
				}
				return respond(http.StatusFound, hdr("Location", tc.target), "")
			})
			err := c.Ping(t.Context())
			if tc.wantErr != (err != nil) {
				t.Fatalf("Ping 的错误 = %v，期望出错 %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, h := range hops {
				if h.URL.Scheme == "http" {
					t.Fatalf("不应跟随到 http：%s（Authorization %q）", h.URL, h.Header.Get("Authorization"))
				}
				if got := h.Header.Get("Authorization") != ""; got != tc.wantAuth {
					t.Errorf("跳转到 %s 时带凭据 = %v，期望 %v", h.URL, got, tc.wantAuth)
				}
			}
			if !tc.wantErr && len(hops) == 0 {
				t.Fatal("没有跟随重定向")
			}
		})
	}
}

// TestTokenRedirectDowngrade token 地址把 https 重定向到 http 时不跟随，用户名密码不会以明文发出。
func TestTokenRedirectDowngrade(t *testing.T) {
	t.Parallel()
	c, ft := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
		switch {
		case r.URL.Host == "auth.example.com" && r.URL.Scheme == "https":
			return respond(http.StatusFound, hdr("Location", "http://auth.example.com/token"), "")
		case r.URL.Host == "auth.example.com":
			return respond(http.StatusOK, nil, `{"token":"leaked"}`)
		}
		return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://auth.example.com/token"`), "")
	})
	if err := c.Ping(t.Context()); err == nil {
		t.Fatal("token 地址降级到 http 时应该失败")
	}
	for _, r := range ft.reqs {
		if r.URL.Scheme == "http" {
			t.Fatalf("不应跟随到 http：%s（Authorization %q）", r.URL, r.Header.Get("Authorization"))
		}
	}
}

// TestRedirectKeepsCallerPolicy 调用方的重定向策略仍然生效。
func TestRedirectKeepsCallerPolicy(t *testing.T) {
	t.Parallel()
	name, err := ParseName(testHost + "/team/app")
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{handle: func(r *http.Request) reply {
		return respond(http.StatusFound, hdr("Location", "https://"+testHost+"/redirected"), "")
	}}
	stop := errors.New("调用方禁止重定向")
	hc := &http.Client{Transport: ft, CheckRedirect: func(*http.Request, []*http.Request) error { return stop }}
	c := NewClient(name, "", "", hc)
	c.backoff = 0
	if err := c.Ping(t.Context()); err == nil || !strings.Contains(err.Error(), stop.Error()) {
		t.Fatalf("Ping 的错误 = %v，期望来自调用方的重定向策略", err)
	}
	for _, r := range ft.requests() {
		if strings.HasSuffix(r, "/redirected") {
			t.Fatalf("调用方禁止重定向时不应跟随：%v", ft.requests())
		}
	}
}

// 换取 token 的服务回显 Basic Authorization（含 base64 编码）时，错误信息中不能出现密码或这段编码。
func TestTokenServiceEchoesBasicAuth(t *testing.T) {
	t.Parallel()
	content := []byte("blob")
	d := Descriptor{Digest: Digest(content), Size: int64(len(content))}
	basic := base64.StdEncoding.EncodeToString([]byte(testUser + ":" + testPass))
	c, _ := newFakeClient(t, testUser, testPass, func(r *http.Request) reply {
		if r.URL.Path == "/token" {
			// 故意把收到的 Authorization（Basic …）回显在响应体中
			return respond(http.StatusInternalServerError, nil, "bad credentials: "+r.Header.Get("Authorization"))
		}
		return respond(http.StatusUnauthorized, hdr("WWW-Authenticate", `Bearer realm="https://`+testHost+`/token",service="test"`), "")
	})
	err := c.PushBlob(t.Context(), d, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(content))), nil
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	for _, secret := range []string{testPass, basic} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误中泄露了凭据 %q：%v", secret, err)
		}
	}
}

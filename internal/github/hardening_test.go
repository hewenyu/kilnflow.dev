package github

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// 默认客户端的重定向策略：阻止 https 降级、跨 origin 去掉 Authorization、限制重定向次数。
func TestSafeRedirect(t *testing.T) {
	t.Parallel()
	newReq := func(s string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, s, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "token secret")
		return r
	}

	t.Run("https 降级到 http 被拒绝", func(t *testing.T) {
		t.Parallel()
		via := []*http.Request{newReq("https://api.github.com/x")}
		err := safeRedirect(newReq("http://api.github.com/x"), via)
		if !errors.Is(err, errInsecureRedirect) {
			t.Fatalf("err = %v，期望 errInsecureRedirect", err)
		}
	})

	t.Run("跨 origin 去掉 Authorization", func(t *testing.T) {
		t.Parallel()
		via := []*http.Request{newReq("https://api.github.com/x")}
		req := newReq("https://evil.example/x")
		if err := safeRedirect(req, via); err != nil {
			t.Fatal(err)
		}
		if h := req.Header.Get("Authorization"); h != "" {
			t.Fatalf("跨 origin 仍带凭据：%q", h)
		}
	})

	t.Run("同一 origin 保留 Authorization", func(t *testing.T) {
		t.Parallel()
		via := []*http.Request{newReq("https://api.github.com/x")}
		req := newReq("https://API.GITHUB.COM:443/y")
		if err := safeRedirect(req, via); err != nil {
			t.Fatal(err)
		}
		if h := req.Header.Get("Authorization"); h != "token secret" {
			t.Fatalf("同一 origin 应保留凭据：%q", h)
		}
	})

	t.Run("不同端口是不同 origin", func(t *testing.T) {
		t.Parallel()
		via := []*http.Request{newReq("https://api.github.com/x")}
		req := newReq("https://api.github.com:8443/y")
		if err := safeRedirect(req, via); err != nil {
			t.Fatal(err)
		}
		if h := req.Header.Get("Authorization"); h != "" {
			t.Fatalf("不同端口仍带凭据：%q", h)
		}
	})

	t.Run("重定向次数上限", func(t *testing.T) {
		t.Parallel()
		first := newReq("https://api.github.com/x")
		via := make([]*http.Request, maxHTTPRedirects)
		for i := range via {
			via[i] = first
		}
		if err := safeRedirect(newReq("https://api.github.com/y"), via); err == nil {
			t.Fatal("超过上限应报错")
		}
	})

	t.Run("没有之前的请求", func(t *testing.T) {
		t.Parallel()
		if err := safeRedirect(newReq("https://api.github.com/x"), nil); err != nil {
			t.Fatalf("第一次跳转不应报错：%v", err)
		}
	})
}

// sameOrigin 按协议、主机名和有效端口比较。
func TestSameOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://a.example/x", "https://a.example/y", true},
		{"https://a.example/x", "https://a.example:443/y", true},
		{"http://a.example/x", "http://a.example:80/y", true},
		{"https://a.example/x", "http://a.example/y", false},
		{"https://a.example/x", "https://b.example/y", false},
		{"https://a.example/x", "https://a.example:8443/y", false},
	}
	for _, c := range cases {
		a, err := url.Parse(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := url.Parse(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(a, b); got != c.want {
			t.Errorf("sameOrigin(%s, %s) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// 默认客户端带总超时和重定向策略。
func TestDefaultHTTPClient(t *testing.T) {
	t.Parallel()
	if defaultHTTP.Timeout != defaultHTTPTimeout {
		t.Errorf("Timeout = %v，期望 %v", defaultHTTP.Timeout, defaultHTTPTimeout)
	}
	if defaultHTTP.CheckRedirect == nil {
		t.Error("应设置 CheckRedirect")
	}
	var zero Client
	if zero.httpc() != defaultHTTP {
		t.Error("没有配置 HTTP 时应使用 defaultHTTP")
	}
}

// 签名长度不对时直接拒绝，不进入 hex 解码。
func TestVerifySignatureLength(t *testing.T) {
	t.Parallel()
	secret, body := []byte("s"), []byte("{}")
	good := Sign(secret, body)
	if !VerifySignature(secret, body, good) {
		t.Fatal("正确的签名应通过")
	}
	sum := sha256.Sum256(body)
	tests := []string{
		"",           // 没有前缀
		"sha256=",    // 空签名
		"sha256=abc", // 太短
		"sha256=" + hex.EncodeToString(sum[:]) + "0", // 太长
		"sha1=" + hex.EncodeToString(sum[:]),         // 算法不对
	}
	for _, h := range tests {
		if VerifySignature(secret, body, h) {
			t.Errorf("签名 %q 不应通过", h)
		}
	}
	if VerifySignature(nil, body, good) {
		t.Error("secret 为空时不应通过")
	}
}

// 只把 http:// 和 https:// 开头的地址当作完整地址；其他既不是 API 路径（/ 开头）也不是 http(s) 地址的值
// 直接拒绝，不会拼接成畸形 URL 发给 API。
func TestDoOnlyAbsoluteHTTPURLs(t *testing.T) {
	t.Parallel()
	f, c, _ := newFakeAPI(t)
	err := c.App(t.Context(), http.MethodGet, "httpx://evil.example/x", nil, nil)
	if err == nil {
		t.Fatal("不是 http(s) 地址也不是 API 路径时应报错")
	}
	if !NotApplied(err) {
		t.Errorf("请求没有发出，应满足 NotApplied：%v", err)
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Fatalf("不应发出请求：%v", reqs)
	}
	// / 开头的路径仍然拼接到 API 地址
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{})
	})
	if err := c.App(t.Context(), http.MethodGet, "/repos/o/r", nil, nil); err != nil {
		t.Fatalf("API 路径应正常发送：%v", err)
	}
	if reqs := f.requests(); len(reqs) != 1 {
		t.Fatalf("请求数 %d", len(reqs))
	}
}

// 异常大的 X-RateLimit-Reset 不能让 Server 停等好几年：恢复时间取 maxRetryAfterSeconds 的上限。
func TestRateLimitResetCapped(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	h := http.Header{}
	h.Set(headerRateLimitRemaining, "0")
	h.Set(headerRateLimitReset, "99999999999")
	kind, until, ok := headerRateLimit(h, now)
	if !ok || kind != RateLimitPrimary {
		t.Fatalf("kind = %v, ok = %v", kind, ok)
	}
	if want := now.Add(time.Duration(maxRetryAfterSeconds) * time.Second); !until.Equal(want) {
		t.Errorf("until = %v，期望 %v", until, want)
	}
}

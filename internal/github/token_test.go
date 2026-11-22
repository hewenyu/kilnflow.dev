package github

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 签发结果分三类：拿到 token；确定没有签发（NotApplied）；结果不明（请求已发出，没有可用的响应）。
func TestCreateToken_Outcome(t *testing.T) {
	expires := time.Unix(1_800_003_600, 0).UTC()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		apiURL  string // 非空时替换 APIURL（构造请求失败）
		want    string // ok、notIssued、unknown
		sent    bool   // 请求是否到达 GitHub
	}{
		{"拿到 token", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusCreated, map[string]any{"token": "ghs_x", "expires_at": expires})
		}, "", "ok", true},
		{"422 明确拒绝", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "permissions invalid"})
		}, "", "notIssued", true},
		{"403 明确拒绝", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible"})
		}, "", "notIssued", true},
		{"构造请求失败", nil, "http://[::1", "notIssued", false},
		{"502", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusBadGateway, map[string]any{"message": "bad gateway"})
		}, "", "unknown", true},
		{"连接断开", func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter 不支持 Hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, "", "unknown", true},
		{"响应无法解析", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{not json"))
		}, "", "unknown", true},
		{"响应中没有 token", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusCreated, map[string]any{"expires_at": expires})
		}, "", "unknown", true},
		{"响应中没有过期时间", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeTestJSON(w, http.StatusCreated, map[string]any{"token": "ghs_x"})
		}, "", "unknown", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				tt.handler(w, r)
			}))
			t.Cleanup(srv.Close)
			c := &Client{AppID: 42, Key: testKey(), APIURL: srv.URL}
			if tt.apiURL != "" {
				c.APIURL = tt.apiURL
			}
			tok, err := c.CreateToken(t.Context(), 7, []int64{1}, map[string]string{"contents": "read"})
			got := "unknown"
			switch {
			case err == nil:
				got = "ok"
			case NotApplied(err):
				got = "notIssued"
			}
			if got != tt.want {
				t.Fatalf("结果 %s，应为 %s（err=%v）", got, tt.want, err)
			}
			if got == "ok" && (tok.Token != "ghs_x" || !tok.ExpiresAt.Equal(expires)) {
				t.Errorf("token = %+v", tok)
			}
			if got != "ok" && tok != nil {
				t.Errorf("出错时应返回 nil：%+v", tok)
			}
			if sent := hits.Load() > 0; sent != tt.sent {
				t.Errorf("请求是否到达 GitHub：%v，应为 %v", sent, tt.sent)
			}
		})
	}
}

// 签发请求限定仓库和权限。
func TestCreateToken_Scope(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	if _, err := c.CreateToken(t.Context(), 7, []int64{11}, map[string]string{"pull_requests": "write"}); err != nil {
		t.Fatal(err)
	}
	toks := f.tokens()
	if len(toks) != 1 || toks[0].Path != "/app/installations/7/access_tokens" {
		t.Fatalf("签发请求：%v", toks)
	}
	body := toks[0].jsonBody(t)
	ids, _ := body["repository_ids"].([]any)
	perms, _ := body["permissions"].(map[string]any)
	if len(ids) != 1 || ids[0] != float64(11) || len(perms) != 1 || perms["pull_requests"] != "write" {
		t.Errorf("请求体 %s", toks[0].Body)
	}
}

// 用给定 token 的客户端：用这个 token 认证，不签发、不读写缓存；401 时不重试。
func TestInstWithToken(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "token revoked" {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"id": 1})
	})
	if _, err := c.InstWithToken(1, "given").GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	if got := f.only(t).Header.Get("Authorization"); got != "token given" {
		t.Errorf("Authorization = %q", got)
	}
	if n := len(f.tokens()); n != 0 {
		t.Errorf("不应签发 token，实际 %d 次", n)
	}
	_, err := c.InstWithToken(1, "revoked").GetRepo(t.Context(), "o/r")
	if !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("应返回 401：%v", err)
	}
	if n, toks := len(f.requests()), len(f.tokens()); n != 2 || toks != 0 {
		t.Errorf("401 后不应重试或签发：请求 %d 次、签发 %d 次", n, toks)
	}
	// 共用客户端不受影响：照常签发自己的 token，给定的 token 没有进缓存
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "token tok1" || len(f.tokens()) != 1 {
		t.Errorf("共用客户端 Authorization = %q，签发 %d 次", got, len(f.tokens()))
	}
	// 共用客户端缓存了 tok1 之后，给定 token 的客户端仍用自己的 token
	if _, err := c.InstWithToken(1, "given").GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	reqs = f.requests()
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "token given" {
		t.Errorf("给定 token 的客户端读了缓存：%q", got)
	}
}

// 请求没有交给 HTTP 客户端时 NotApplied 为真，App JWT 签名失败同样如此。
func TestNotApplied_NotSent(t *testing.T) {
	c := &Client{AppID: 42, Key: testKey(), APIURL: "http://[::1"}
	err := c.App(t.Context(), http.MethodPost, "/x", map[string]any{}, nil)
	if err == nil || !NotApplied(err) || !strings.Contains(err.Error(), "构造请求") {
		t.Errorf("构造请求失败：%v", err)
	}
	err = c.App(t.Context(), http.MethodPost, "/x", map[string]any{"bad": func() {}}, nil)
	if err == nil || !NotApplied(err) || !strings.Contains(err.Error(), "编码请求") {
		t.Errorf("编码请求体失败：%v", err)
	}
	if NotApplied(nil) || NotApplied(&APIError{Status: http.StatusInternalServerError}) {
		t.Error("nil、5xx 不应判为没有生效")
	}
}

// brokenKey 是无法签名的私钥（模数太短，签名在本地失败）。
func brokenKey() *rsa.PrivateKey {
	return &rsa.PrivateKey{N: big.NewInt(3233), E: 17, D: big.NewInt(2753)}
}

// App JWT 在本地签名失败：没有发出任何 HTTP 请求，NotApplied 为真（签发 token 同样是确定没有签发）。
func TestNotApplied_JWTFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	c := &Client{AppID: 42, Key: brokenKey(), APIURL: srv.URL}
	if err := c.App(t.Context(), http.MethodGet, "/app", nil, nil); err == nil || !NotApplied(err) {
		t.Errorf("JWT 签名失败：%v，NotApplied 应为真", err)
	}
	if _, err := c.CreateToken(t.Context(), 7, []int64{1}, map[string]string{"contents": "read"}); err == nil || !NotApplied(err) {
		t.Errorf("JWT 签名失败时签发 token：%v，应确定没有签发", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("JWT 签名失败时不应发出请求，实际 %d 个", n)
	}
}

// 固定凭据的客户端没有 token：在发请求前失败（NotApplied），冷、热缓存下都不签发、不用共用缓存的 token；forge 包装同理
// （forge 的测试见 internal/forge）。
func TestInstWithToken_Empty(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r", jsonReply(http.StatusOK, map[string]any{"id": 1}))
	check := func(stage string) {
		t.Helper()
		before, toks := len(f.requests()), len(f.tokens())
		_, err := c.InstWithToken(1, "").GetRepo(t.Context(), "o/r")
		if err == nil || !NotApplied(err) {
			t.Errorf("%s：空 token 应在发请求前失败：%v", stage, err)
		}
		if len(f.requests()) != before || len(f.tokens()) != toks {
			t.Errorf("%s：空 token 不应发出请求或签发", stage)
		}
	}
	check("冷缓存")
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	check("热缓存")
}

// 固定 token 收到 401：不清共用客户端缓存的 token。
func TestInstWithToken_UnauthorizedKeepsCache(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "token revoked" {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"id": 1})
	})
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstWithToken(1, "revoked").GetRepo(t.Context(), "o/r"); !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("应返回 401：%v", err)
	}
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "token tok1" || len(f.tokens()) != 1 {
		t.Errorf("固定 token 的 401 清了共用缓存：Authorization = %q，签发 %d 次", got, len(f.tokens()))
	}
}

// 凭据端点的错误不带响应正文和底层错误文本（可能回显 token），只保留分类、状态码和元数据。
func TestTokenEndpointErrors_NoSecrets(t *testing.T) {
	const secret = "ghs_SECRETSECRET"
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		revoke    bool // 测吊销而不是签发
		status    int  // 期望的 APIError 状态码，0 表示没有 APIError
		notIssued bool
	}{
		{"201 的过期时间无法解析", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusCreated, map[string]any{"token": secret, "expires_at": secret})
		}, false, 0, false},
		{"422 的消息回显", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": secret, "errors": []string{secret}})
		}, false, http.StatusUnprocessableEntity, true},
		{"500 的非 JSON 正文", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("oops " + secret))
		}, false, http.StatusInternalServerError, false},
		{"吊销 500 的正文", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"message": "bad " + secret})
		}, true, http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			c := &Client{AppID: 42, Key: testKey(), APIURL: srv.URL}
			var err error
			if tt.revoke {
				err = c.RevokeToken(t.Context(), 7, secret)
			} else {
				_, err = c.CreateToken(t.Context(), 7, []int64{1}, map[string]string{"contents": "read"})
			}
			if err == nil {
				t.Fatal("应返回错误")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%+v", err), secret) {
				t.Errorf("错误中出现了响应中的 token：%v", err)
			}
			if NotApplied(err) != tt.notIssued {
				t.Errorf("NotApplied = %v，应为 %v", NotApplied(err), tt.notIssued)
			}
			ae, ok := errors.AsType[*APIError](err)
			if got := 0; ok {
				got = ae.Status
				if got != tt.status {
					t.Errorf("状态码 %d，应为 %d", got, tt.status)
				}
			} else if tt.status != 0 {
				t.Errorf("应保留状态码 %d：%v", tt.status, err)
			}
		})
	}
}

// 吊销遇到限流时保留限流的分类（恢复时间），但不带响应正文。
func TestTokenEndpointErrors_RateLimit(t *testing.T) {
	const secret = "ghs_SECRETSECRET"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		writeTestJSON(w, http.StatusTooManyRequests, map[string]any{"message": secret})
	}))
	t.Cleanup(srv.Close)
	c := &Client{AppID: 42, Key: testKey(), APIURL: srv.URL}
	_, err := c.CreateToken(t.Context(), 7, nil, nil)
	rl, ok := RateLimited(err)
	if !ok || rl.Until.IsZero() || !NotApplied(err) {
		t.Fatalf("应保留限流分类：%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("限流错误中出现了响应正文：%v", err)
	}
}

// 201 中有 token 但没有过期时间：不交出，先用这枚 token 吊销它，再按结果不明报告（不是确定没有签发）。
// 缺过期时间、补吊销失败（被限流、403）：仍按结果不明返回（签发已经生效），吊销请求的错误类型不能带出来让 NotApplied
// 判为"确定没有签发"。
func TestCreateToken_MissingExpiryRevokeFails(t *testing.T) {
	t.Parallel()
	for name, revoke := range map[string]func(w http.ResponseWriter){
		"限流": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "60")
			writeTestJSON(w, http.StatusTooManyRequests, map[string]any{"message": "rate limited"})
		},
		"403": func(w http.ResponseWriter) {
			writeTestJSON(w, http.StatusForbidden, map[string]any{"message": "forbidden"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var creates, deletes atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
					deletes.Add(1)
					revoke(w)
					return
				}
				creates.Add(1)
				writeTestJSON(w, http.StatusCreated, map[string]any{"token": "ghs_noexp"})
			}))
			t.Cleanup(srv.Close)
			c := &Client{AppID: 42, Key: testKey(), APIURL: srv.URL}
			tok, err := c.CreateToken(t.Context(), 7, []int64{1}, map[string]string{"contents": "read"})
			if err == nil || tok != nil || NotApplied(err) {
				t.Fatalf("应按结果不明报告（不是确定没有签发）：%+v %v", tok, err)
			}
			if _, ok := errors.AsType[*RateLimitError](err); ok {
				t.Errorf("错误中带出了吊销请求的限流类型：%v", err)
			}
			if _, ok := errors.AsType[*APIError](err); ok {
				t.Errorf("错误中带出了吊销请求的 APIError：%v", err)
			}
			if !strings.Contains(err.Error(), "吊销这枚 token 失败") || strings.Contains(err.Error(), "ghs_noexp") {
				t.Errorf("错误：%v", err)
			}
			if creates.Load() != 1 || deletes.Load() != 1 {
				t.Errorf("签发 %d 次、吊销 %d 次", creates.Load(), deletes.Load())
			}
		})
	}
}

func TestCreateToken_MissingExpiryRevokes(t *testing.T) {
	var revoked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
			revoked.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{"token": "ghs_noexp"})
	}))
	t.Cleanup(srv.Close)
	c := &Client{AppID: 42, Key: testKey(), APIURL: srv.URL}
	tok, err := c.CreateToken(t.Context(), 7, []int64{1}, map[string]string{"contents": "read"})
	if err == nil || tok != nil || NotApplied(err) {
		t.Fatalf("应不交出并按结果不明报告：%+v %v", tok, err)
	}
	if got, _ := revoked.Load().(string); got != "token ghs_noexp" {
		t.Errorf("应吊销这枚 token，吊销请求的 Authorization = %q", got)
	}
	if strings.Contains(err.Error(), "ghs_noexp") {
		t.Errorf("错误中出现了 token：%v", err)
	}
}

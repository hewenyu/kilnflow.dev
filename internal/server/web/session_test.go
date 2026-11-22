package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
)

// 会话：登录（admin token、GitHub OAuth）、签名、过期、角色和退出。

// sessionCookieOf 返回响应设置的会话 cookie，没有时终止测试。
func sessionCookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("响应没有设置会话 cookie：%v", w.Header())
	return nil
}

// setsSessionCookie 表示响应是否设置了会话 cookie。
func setsSessionCookie(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return true
		}
	}
	return false
}

// sessionClient 返回使用 cookie 的客户端（CSRF token 从 cookie 中的会话计算）。
func (e *webEnv) sessionClient(c *http.Cookie) *client {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	se := e.d.getSession(r)
	if se == nil {
		e.t.Fatalf("cookie 不是有效的会话：%s", c.Value)
	}
	return &client{login: se.Login, cookie: c, csrf: e.d.csrfToken(se)}
}

func TestTokenLogin(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	w := serve(e.mux, testReq{method: http.MethodPost, target: "/login", form: url.Values{"token": {testAdminToken}, "next": {"/settings/ops"}}})
	wantRedirect(t, "admin token 登录", w, "/settings/ops")
	c := sessionCookieOf(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != int(sessionTTL/time.Second) {
		t.Errorf("会话 cookie 的属性 = %+v", c)
	}
	cl := e.sessionClient(c)
	if cl.login != admin.TokenActor {
		t.Errorf("admin token 登录的用户 = %q，应为 %q", cl.login, admin.TokenActor)
	}
	wantStatus(t, "用这个会话打开设置", e.get(cl, "/settings/ops"), http.StatusOK)
	if a := e.audits("login"); len(a) != 1 || a[0].Actor != admin.TokenActor || a[0].Detail["via"] != "token" {
		t.Errorf("登录的审计 = %+v", a)
	}
	// 用这个会话执行的操作，审计记录为 admin
	e.addRunner("mac-1", runnerOpts{})
	wantRedirect(t, "暂停", e.post(cl, "/runners/mac-1/drain", nil), "/runners")
	if a := e.audits("runner.drain"); len(a) != 1 || a[0].Actor != admin.TokenActor {
		t.Errorf("操作的审计 = %+v", a)
	}

	// 不安全的 next 回到默认页面
	w = serve(e.mux, testReq{method: http.MethodPost, target: "/login", form: url.Values{"token": {testAdminToken}, "next": {"//evil.example"}}})
	wantRedirect(t, "不安全的 next", w, "/")
}

func TestOAuthLogin(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	gh := fakeGitHubOAuth(t)
	e.d.githubURL, e.d.cfg.GitHubAPIURL, e.d.cfg.GitHubClientID = gh.URL, gh.URL, "client-id"

	for _, c := range []struct {
		login, role string
		settings    int
	}{
		{"alice", roleAdmin, http.StatusOK},
		{"bob", roleUser, http.StatusForbidden},
	} {
		state, browser := loginState(e.d, "/runners")
		w := serve(e.mux, testReq{target: "/auth/github/callback?code=ok-" + c.login + "&state=" + url.QueryEscape(state), cookie: browser})
		wantRedirect(t, c.login+" 用 GitHub 登录", w, "/runners")
		cl := e.sessionClient(sessionCookieOf(t, w))
		if cl.login != c.login {
			t.Errorf("登录的用户 = %q，应为 %q", cl.login, c.login)
		}
		wantStatus(t, c.login+" 打开运行列表", e.get(cl, "/runs"), http.StatusOK)
		wantStatus(t, c.login+" 打开设置", e.get(cl, "/settings"), c.settings)
	}
	a := e.audits("login")
	if len(a) != 2 || a[0].Actor != "alice" || a[1].Actor != "bob" || a[0].Detail["via"] != "github" {
		t.Errorf("GitHub 登录的审计 = %+v", a)
	}
	// 没有权限的账号不写审计、不设置会话
	state, browser := loginState(e.d, "/runs")
	w := serve(e.mux, testReq{target: "/auth/github/callback?code=ok-mallory&state=" + url.QueryEscape(state), cookie: browser})
	wantStatus(t, "没有权限的账号", w, http.StatusForbidden)
	if w.Header().Get("Set-Cookie") != "" || len(e.audits("login")) != 2 {
		t.Error("没有权限的账号不应登录")
	}
}

func TestSessionValidation(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	sign := func(payload string) *http.Cookie {
		p := base64.RawURLEncoding.EncodeToString([]byte(payload))
		return &http.Cookie{Name: sessionCookie, Value: p + "." + e.d.keys.SessionSig(p)}
	}
	// 篡改：把 bob 的会话改成管理员 alice，沿用原来的签名
	p, sig, _ := strings.Cut(e.bob.cookie.Value, ".")
	raw, err := base64.RawURLEncoding.DecodeString(p)
	e.must(err)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"l":"bob"`, `"l":"alice"`, 1)))

	for _, c := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"篡改用户", &http.Cookie{Name: sessionCookie, Value: forged + "." + sig}},
		{"没有签名", &http.Cookie{Name: sessionCookie, Value: p}},
		{"其他密钥的签名", &http.Cookie{Name: sessionCookie, Value: p + ".AAAA"}},
		{"签名正确但不是 base64", &http.Cookie{Name: sessionCookie, Value: "!!!." + e.d.keys.SessionSig("!!!")}},
		{"签名正确但不是 JSON", sign("not json")},
		{"已过期", cookieFor(e.d, "alice", time.Now().Add(-time.Second))},
	} {
		w := serve(e.mux, testReq{target: "/settings", cookie: c.cookie})
		wantRedirect(t, c.name, w, "/login?next=%2Fsettings&reason=expired")
		if w := serve(e.mux, testReq{target: "/jobs/1/log", cookie: c.cookie}); w.Code != http.StatusUnauthorized {
			t.Errorf("%s：日志接口 HTTP %d，应为 401", c.name, w.Code)
		}
	}
	// 有效的会话：用户来自签名的会话
	if se := e.sessionClient(e.bob.cookie); se.login != "bob" {
		t.Errorf("bob 的会话 = %+v", se)
	}

	// CSRF token 绑定会话：同一用户重新登录（到期时间不同）后，旧页面的表单失效
	e.addRunner("mac-1", runnerOpts{})
	relogin := e.login("alice")
	relogin.cookie = cookieFor(e.d, "alice", time.Now().Add(2*time.Hour))
	w := e.post(relogin, "/runners/mac-1/drain", nil)
	wantStatus(t, "旧会话的 CSRF", w, http.StatusForbidden)
	mustHave(t, "旧会话的 CSRF", pageTextZH(t, w.Body.String()), "页面已过期")

	// 退出只接受 POST 并校验 CSRF：其他网站不能让用户被动退出
	w = serve(e.mux, testReq{target: "/logout", cookie: e.alice.cookie})
	if setsSessionCookie(w) {
		t.Error("GET /logout 不应清除会话 cookie")
	}
	w = e.post(e.alice, "/logout", url.Values{"csrf": {"wrong"}})
	wantStatus(t, "退出时 CSRF 不对", w, http.StatusForbidden)
	if setsSessionCookie(w) {
		t.Error("CSRF 不对时不应清除会话 cookie")
	}

	// 退出：清除 cookie
	w = e.post(e.alice, "/logout", nil)
	wantRedirect(t, "退出", w, "/login?reason=logout")
	if c := sessionCookieOf(t, w); c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("退出应清除会话 cookie：%+v", c)
	}
}

// githubLogin："我发起的"使用的 GitHub 登录名。没有绑定的 admin token 登录没有 GitHub 账号；
// 名为 admin（与 admin.TokenActor 相同）的 GitHub 用户仍然是 GitHub 账号。
func TestSessionGitHubLogin(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		se   session
		want string
	}{
		{"GitHub 登录", session{Login: "alice", ID: 1, Via: viaGitHub}, "alice"},
		{"名为 admin 的 GitHub 用户", session{Login: admin.TokenActor, ID: 2, Via: viaGitHub}, admin.TokenActor},
		{"没有绑定的 token 登录", session{Login: admin.TokenActor, Via: viaToken}, ""},
		{"绑定后的 token 登录", session{Login: "alice", ID: 1, Via: viaToken}, "alice"},
	} {
		if got := tt.se.githubLogin(); got != tt.want {
			t.Errorf("%s：githubLogin() = %q，期望 %q", tt.name, got, tt.want)
		}
	}
}

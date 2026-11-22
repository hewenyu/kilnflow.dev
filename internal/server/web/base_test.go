package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
)

// 登录、错误页和全站外壳的单元测试。

const testAdminToken = "admin-token-0123456789abcdef"

// testDashboard 返回只有密钥和日志的 Dashboard（不访问数据库）和注册了全部路由的 mux。clientID 为空时关闭 GitHub 登录。
func testDashboard(t *testing.T, clientID string) (*Dashboard, *http.ServeMux, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	d := &Dashboard{
		cfg: Config{
			PublicURL: "https://kiln.example.com", GitHubClientID: clientID, GitHubClientSecret: "client-secret",
			Admins: map[string]bool{"alice": true}, Users: map[string]bool{"bob": true},
		},
		keys:       auth.NewKeys([]byte("token-key"), []byte("cas-key"), testAdminToken),
		log:        slog.New(slog.NewTextHandler(&logs, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		githubURL:  "https://github.invalid",
		reveals:    newEnrollReveals(),
	}
	mux := http.NewServeMux()
	d.Register(mux)
	return d, mux, &logs
}

// loginState 返回登录用的 OAuth state 和发起登录的浏览器带着的 cookie（与 handleOAuthStart 相同）。
func loginState(d *Dashboard, next string) (string, *http.Cookie) {
	id := newRequestID()
	return d.oauthState(loginPurposePrefix+id, next), &http.Cookie{Name: oauthBrowserCookie, Value: id}
}

// responseCookie 返回响应设置的名为 name 的 cookie，没有时返回 nil。
func responseCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// cookieFor 返回 login 用 GitHub 登录的会话 cookie（角色按配置和绑定在每次请求时确定）；exp 为会话的到期时间。
func cookieFor(d *Dashboard, login string, exp time.Time) *http.Cookie {
	return sessionCookieFor(d, session{Login: login, Via: viaGitHub, Exp: exp.Unix()})
}

// sessionCookieFor 返回会话 se 的 cookie。
func sessionCookieFor(d *Dashboard, se session) *http.Cookie {
	p := base64.RawURLEncoding.EncodeToString(mustJSON(se))
	return &http.Cookie{Name: sessionCookie, Value: p + "." + d.keys.SessionSig(p)}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type testReq struct {
	method, target string
	form           url.Values
	cookie         *http.Cookie
	referer        string
	// browser 表示浏览器直接打开（不带 X-Kiln-UI 请求头，返回 index.html）；默认是前端取页面数据。
	browser bool
}

// dumpN 是 KILN_UI_DUMP 中已写入的页面数据的序号。
var dumpN atomic.Int64

// dumpUI 在设置了环境变量 KILN_UI_DUMP（目录）时把页面数据写到这个目录，供前端的组件测试和开发参考。
func dumpUI(target string, w *httptest.ResponseRecorder) {
	dir := os.Getenv("KILN_UI_DUMP")
	if dir == "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		return
	}
	var p struct{ Page string }
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Page == "" {
		return
	}
	name := fmt.Sprintf("%04d-%s.json", dumpN.Add(1), p.Page)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name), append([]byte("// "+target+"\n"), w.Body.Bytes()...), 0o644)
}

func serve(h http.Handler, q testReq) *httptest.ResponseRecorder {
	if q.method == "" {
		q.method = http.MethodGet
	}
	var r *http.Request
	if q.form != nil {
		r = httptest.NewRequest(q.method, "https://kiln.example.com"+q.target, strings.NewReader(q.form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(q.method, "https://kiln.example.com"+q.target, nil)
	}
	if q.cookie != nil {
		r.AddCookie(q.cookie)
	}
	if q.referer != "" {
		r.Header.Set("Referer", q.referer)
	}
	if !q.browser {
		r.Header.Set(uiHeader, "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	dumpUI(q.method+" "+q.target, w)
	return w
}

// testPage 是响应中的页面数据（uiPage）；Data 留给各测试按页面类型解析（pageData）。
type testPage struct {
	Page, Nav, CSRF string
	// Title、Flash 是消息或原样的文字（检查显示的中文用 textZH）。
	Title, Flash   uiText
	Status, Active int
	OAuth          bool
	Session        *uiSession
	Data           json.RawMessage
}

// parsePage 解析响应正文中的页面数据：前端请求的 JSON，或浏览器请求的 index.html 中内嵌的 <script id="kiln-page">。
func parsePage(t *testing.T, body string) testPage {
	t.Helper()
	raw := body
	if strings.HasPrefix(raw, "<") {
		_, rest, ok := strings.Cut(raw, `<script id="kiln-page" type="application/json">`)
		raw, _, _ = strings.Cut(rest, "</script>")
		if !ok {
			t.Fatalf("index.html 中没有内嵌的页面数据：%.300s", body)
		}
	}
	var p testPage
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Page == "" {
		t.Fatalf("不是页面数据（%v）：%.300s", err, body)
	}
	return p
}

// pageData 把响应正文中页面数据的 Data 解析为 T（页面类型，或只含要检查的字段的结构体、map[string]any）。
func pageData[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(parsePage(t, body).Data, &v); err != nil {
		t.Fatalf("页面数据的 Data 无法解析为 %T：%v\n%.300s", v, err, body)
	}
	return v
}

// expect 检查 conds 都成立；不成立时报告 what、第几个条件和 v。返回是否都成立。
// 条件在调用前就已求值：要解引用可能为 nil 的指针时，先单独检查它。
func expect(t *testing.T, what string, v any, conds ...bool) bool {
	t.Helper()
	for i, ok := range conds {
		if !ok {
			t.Errorf("%s：第 %d 个条件不成立：%+v", what, i+1, v)
			return false
		}
	}
	return true
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/runners":                   "/runners",
		"/runs/1?back=%2Fruns":       "/runs/1?back=%2Fruns",
		"/settings/scope?repo=a%2Fb": "/settings/scope?repo=a%2Fb",
		"":                           defaultNext,
		"runs":                       defaultNext,
		"//evil.example":             defaultNext,
		"///evil.example":            defaultNext,
		"/\\evil.example":            defaultNext,
		"/x\\y":                      defaultNext,
		"https://evil.example/":      defaultNext,
		"javascript:alert(1)":        defaultNext,
		"/\t/evil.example":           defaultNext,
		"/\n/evil.example":           defaultNext,
		"/\x00":                      defaultNext,
		"/login?next=/x":             defaultNext,
		"/logout":                    defaultNext,
		"/auth/github":               defaultNext,
		"/%2F/evil.example":          defaultNext, // 解码后以 // 开头，保守拒绝
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q，应为 %q", next, got, want)
		}
	}
}

// loginData 是登录页的页面数据（loginPage）。
type loginData struct {
	Next, GitHubURL, Bound string
	Notice                 uiText
	TokenLogin             bool
	Form                   struct {
		Values map[string]string
		Errors map[string]uiText
		Failed bool
	}
}

func TestLoginPage(t *testing.T) {
	t.Run("OAuth 开启", func(t *testing.T) {
		_, mux, _ := testDashboard(t, "client-id")
		w := serve(mux, testReq{target: "/login?next=%2Frunners%3Fstate%3Donline"})
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d", w.Code)
		}
		pg := parsePage(t, w.Body.String())
		p := pageData[loginData](t, w.Body.String())
		if pg.Page != "login" || !pg.OAuth || pg.Session != nil || pg.CSRF != "" {
			t.Errorf("登录页（OAuth 开启）：Page %q OAuth %v Session %v CSRF %q", pg.Page, pg.OAuth, pg.Session, pg.CSRF)
		}
		// GitHub 登录带上 next；admin token 是备用入口，没有出错时不展开（Form.Failed 为 false）
		if p.GitHubURL != "/auth/github?next=%2Frunners%3Fstate%3Donline" || p.Next != "/runners?state=online" || !p.TokenLogin || p.Form.Failed {
			t.Errorf("登录页（OAuth 开启）的数据不对：%+v", p)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("登录页应为 no-store")
		}
	})
	t.Run("OAuth 关闭", func(t *testing.T) {
		_, mux, _ := testDashboard(t, "")
		body := serve(mux, testReq{target: "/login?next=//evil.example"}).Body.String()
		p := pageData[loginData](t, body)
		// OAuth 关闭时前端不显示 GitHub 登录入口（OAuth 为 false），只有 admin token
		if parsePage(t, body).OAuth || !p.TokenLogin || p.Next != "/" {
			t.Errorf("登录页（OAuth 关闭）的数据不对：%+v", p)
		}
		if strings.Contains(body, "evil.example") {
			t.Errorf("不安全的 next 不应出现在页面数据中")
		}
	})
	t.Run("原因", func(t *testing.T) {
		_, mux, _ := testDashboard(t, "")
		if p := pageData[loginData](t, serve(mux, testReq{target: "/login?next=%2Fruns&reason=expired"}).Body.String()); !strings.Contains(textZH(t, p.Notice), "登录已过期") || p.Next != "/runs" {
			t.Errorf("会话过期：%+v", p)
		}
		if p := pageData[loginData](t, serve(mux, testReq{target: "/login?reason=logout"}).Body.String()); !strings.Contains(textZH(t, p.Notice), "已退出登录") {
			t.Errorf("退出：%+v", p)
		}
		if p := pageData[loginData](t, serve(mux, testReq{target: "/login"}).Body.String()); !p.Notice.IsZero() {
			t.Errorf("没有原因时不应有说明：%+v", p.Notice)
		}
		// 没有会话时不需要 CSRF，直接跳转
		w := serve(mux, testReq{method: http.MethodPost, target: "/logout"})
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?reason=logout" {
			t.Errorf("退出：HTTP %d → %q", w.Code, w.Header().Get("Location"))
		}
	})
	t.Run("浏览器打开", func(t *testing.T) {
		_, mux, _ := testDashboard(t, "client-id")
		w := serve(mux, testReq{target: "/login?next=%2Fruns", browser: true})
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || strings.Contains(w.Body.String(), "kiln-page") {
			t.Errorf("浏览器打开登录页应返回 index.html（不内嵌页面数据，由前端取数据）：HTTP %d %q", w.Code, w.Header().Get("Content-Type"))
		}
	})
}

func TestLoginFailure(t *testing.T) {
	for _, oauth := range []string{"", "client-id"} {
		_, mux, _ := testDashboard(t, oauth)
		for _, c := range []struct{ token, next, wantNext, wantErr string }{
			{"wrong-token-secret-value", "/runners?state=online", "/runners?state=online", "admin token 不正确"},
			{"wrong-token-secret-value", "https://evil.example/", "/", "admin token 不正确"},
			{"", "/runs/7", "/runs/7", "请填写 admin token"},
		} {
			w := serve(mux, testReq{method: http.MethodPost, target: "/login", form: url.Values{"token": {c.token}, "next": {c.next}}})
			body := w.Body.String()
			if w.Code != http.StatusUnauthorized {
				t.Errorf("OAuth=%q token=%q：HTTP %d，应为 401", oauth, c.token, w.Code)
			}
			// 错误在 token 字段上（前端就地提示、展开备用入口），保留安全的 next
			p := pageData[loginData](t, body)
			if pg := parsePage(t, body); pg.Page != "login" || pg.Status != http.StatusUnauthorized {
				t.Errorf("登录失败：Page %q Status %d", pg.Page, pg.Status)
			}
			if p.Next != c.wantNext || !p.Form.Failed || !strings.Contains(textZH(t, p.Form.Errors["token"]), c.wantErr) {
				t.Errorf("OAuth=%q token=%q：登录失败的数据不对：%+v", oauth, c.token, p)
			}
			if c.token != "" && strings.Contains(body, c.token) {
				t.Errorf("登录失败的页面回显了 token")
			}
			if strings.Contains(body, "evil.example") {
				t.Errorf("不安全的 next 出现在页面数据中")
			}
			if w.Header().Get("Set-Cookie") != "" {
				t.Errorf("登录失败不应设置会话")
			}
		}
	}
}

// wantErrorPage 检查 w 是统一的错误页（页面数据的 Page 为 error）：状态码、标题和原因中包含 texts、
// 恢复入口的地址依次为 links，响应正文中不包含 leaks。返回错误页的数据。
// 前端请求返回 JSON；浏览器直接打开（GitHub 登录回调）返回内嵌了页面数据的 index.html。
func wantErrorPage(t *testing.T, what string, w *httptest.ResponseRecorder, status int, texts, links []string, leaks ...string) *errorPage {
	t.Helper()
	body := w.Body.String()
	if w.Code != status {
		t.Errorf("%s：HTTP %d，应为 %d", what, w.Code, status)
	}
	switch ct := w.Header().Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s：错误页应为 no-store", what)
		}
	case !strings.HasPrefix(ct, "text/html"):
		t.Errorf("%s：Content-Type %q", what, ct)
	}
	if pg := parsePage(t, body); pg.Page != "error" || pg.Status != status {
		t.Errorf("%s：Page %q Status %d，应为错误页 %d", what, pg.Page, pg.Status, status)
	}
	p := pageData[errorPage](t, body)
	if p.Status != status {
		t.Errorf("%s：错误页的 Status %d，应为 %d", what, p.Status, status)
	}
	mustHave(t, what, textZH(t, p.Heading)+"\n"+textZH(t, p.Reason), texts...)
	var got []string
	for _, l := range p.Links {
		got = append(got, l.URL)
	}
	if !slices.Equal(got, links) {
		t.Errorf("%s：恢复入口 %q，应为 %q", what, got, links)
	}
	for _, l := range leaks {
		if strings.Contains(body, l) {
			t.Errorf("%s：页面包含 %q", what, l)
		}
	}
	return &p
}

func TestErrorPages(t *testing.T) {
	d, mux, logs := testDashboard(t, "")
	now := time.Now()
	alice, bob := cookieFor(d, "alice", now.Add(time.Hour)), cookieFor(d, "bob", now.Add(time.Hour))
	expired := cookieFor(d, "alice", now.Add(-time.Minute))

	// 未登录时提供登录入口（登录后回到这个地址）
	wantErrorPage(t, "未登录打开不存在的地址", serve(mux, testReq{target: "/no/such/page"}), http.StatusNotFound,
		[]string{"找不到这个页面"}, []string{"/login?next=%2Fno%2Fsuch%2Fpage", "/runs"})
	// 浏览器打开不存在的地址：状态码为 404 的 index.html，内嵌错误页的数据
	wantErrorPage(t, "浏览器打开不存在的地址", serve(mux, testReq{target: "/no/such/page", browser: true}), http.StatusNotFound,
		[]string{"找不到这个页面"}, []string{"/login?next=%2Fno%2Fsuch%2Fpage", "/runs"})
	// 已登录时不需要登录入口，页面外壳显示当前身份
	w := serve(mux, testReq{target: "/no/such/page", cookie: bob})
	wantErrorPage(t, "已登录打开不存在的地址", w, http.StatusNotFound, []string{"找不到这个页面"}, []string{"/runs"})
	if se := parsePage(t, w.Body.String()).Session; se == nil || se.Login != "bob" || se.Admin {
		t.Errorf("已登录的错误页应带上当前身份（bob，成员）：%+v", se)
	}
	wantErrorPage(t, "成员打开设置", serve(mux, testReq{target: "/settings/ops", cookie: bob}), http.StatusForbidden,
		[]string{"需要管理员权限", "当前登录的 bob 是成员"}, []string{"/runs", "/login?next=%2Fsettings%2Fops"})
	bobCSRF := d.csrfToken(&session{Login: "bob", Exp: now.Add(time.Hour).Unix()})
	w = serve(mux, testReq{method: http.MethodPost, target: "/sync", form: url.Values{"csrf": {bobCSRF}}, cookie: bob, referer: "https://kiln.example.com/runs"})
	wantErrorPage(t, "成员的写请求", w, http.StatusForbidden, []string{"需要管理员权限"}, []string{"/runs", "/login?next=%2Fruns"})
	wantErrorPage(t, "CSRF 失败", serve(mux, testReq{
		method: http.MethodPost, target: "/sync", form: url.Values{"csrf": {"bad"}}, cookie: alice,
		referer: "https://kiln.example.com/settings/repos?q=x",
	}), http.StatusForbidden, []string{"页面已过期"}, []string{"/settings/repos?q=x", "/runs"})
	// 来自其他网站：没有"回到原来的页面"
	w = serve(mux, testReq{method: http.MethodPost, target: "/sync", form: url.Values{"csrf": {"bad"}}, cookie: alice, referer: "https://evil.example/settings"})
	wantErrorPage(t, "CSRF 失败（其他网站）", w, http.StatusForbidden, []string{"页面已过期"}, []string{"/runs"}, "evil.example")
	wantErrorPage(t, "日志下载参数无效", serve(mux, testReq{target: "/jobs/1/log/download?attempt=x", cookie: alice}), http.StatusBadRequest,
		[]string{"参数无效"}, []string{"/runs"})

	// 会话过期：跳转到登录页并说明原因；POST 回到提交表单的页面，不重放 POST
	for _, c := range []struct {
		q    testReq
		want string
	}{
		{testReq{target: "/runs/5?back=%2Fruns", cookie: expired}, "/login?next=%2Fruns%2F5%3Fback%3D%252Fruns&reason=expired"},
		{testReq{method: http.MethodPost, target: "/sync", form: url.Values{}, cookie: expired, referer: "https://kiln.example.com/settings/repos"}, "/login?next=%2Fsettings%2Frepos&reason=expired"},
		{testReq{method: http.MethodPost, target: "/sync", form: url.Values{}, cookie: expired, referer: "https://evil.example/x"}, "/login?reason=expired"},
		{testReq{target: "/runs"}, "/login?next=%2Fruns"},
		{testReq{target: "/runs", cookie: &http.Cookie{Name: sessionCookie, Value: "forged.sig"}}, "/login?next=%2Fruns&reason=expired"},
		// 浏览器提交表单（不经过前端）时是 303
		{testReq{method: http.MethodPost, target: "/sync", form: url.Values{}, cookie: expired, referer: "https://kiln.example.com/settings/repos", browser: true}, "/login?next=%2Fsettings%2Frepos&reason=expired"},
	} {
		w := serve(mux, c.q)
		if c.q.browser && w.Code != http.StatusSeeOther {
			t.Errorf("%s %s（浏览器）：HTTP %d，应为 303", c.q.method, c.q.target, w.Code)
		}
		if got := redirectTarget(t, c.q.method+" "+c.q.target, w); got != c.want {
			t.Errorf("%s %s：跳转到 %q，应为 %q", c.q.method, c.q.target, got, c.want)
		}
	}
	// 页面脚本调用的接口仍返回 401 纯文本
	if w := serve(mux, testReq{target: "/jobs/1/log", cookie: expired}); w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("日志接口：HTTP %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	// 内部错误：500，只显示错误编号，可以重试（GET）；日志中有同一个编号和错误内容
	r := httptest.NewRequest(http.MethodGet, "https://kiln.example.com/runs?state=failed", nil)
	r.Header.Set(uiHeader, "1")
	r.AddCookie(alice)
	w = httptest.NewRecorder()
	d.serverError(w, r, errors.New(`ERROR: relation "secret_internal_table" does not exist (SQLSTATE 42P01)`))
	p := wantErrorPage(t, "内部错误", w, http.StatusInternalServerError, []string{"服务器出错了", "错误编号"}, []string{"/runs?state=failed", "/runs"},
		"secret_internal_table", "SQLSTATE")
	if p.Ref == "" || !strings.Contains(logs.String(), "ref="+p.Ref) || !strings.Contains(logs.String(), "secret_internal_table") {
		t.Errorf("日志中应有错误编号 %q 和错误内容：%s", p.Ref, logs.String())
	}
	if l := p.Links[0]; textZH(t, l.Label) != "重试" || !l.Primary {
		t.Errorf("GET 出错时主要操作应为重试：%+v", l)
	}
	// POST 出错不提供"重试"（不能重放），回到原来的页面
	r = httptest.NewRequest(http.MethodPost, "https://kiln.example.com/sync", nil)
	r.Header.Set(uiHeader, "1")
	r.Header.Set("Referer", "https://kiln.example.com/settings/repos")
	w = httptest.NewRecorder()
	d.serverError(w, r, errors.New("boom"))
	wantErrorPage(t, "操作出错", w, http.StatusInternalServerError, []string{"操作可能没有完成"}, []string{"/settings/repos", "/runs"}, "boom")
}

// fakeGitHubOAuth 是模拟的 GitHub OAuth：code 为 "ok-<login>" 时换到 <login> 的令牌；"down" 时换取令牌返回 500（带内部信息）。
func fakeGitHubOAuth(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			code := r.PostForm.Get("code")
			if code == "down" {
				http.Error(w, "upstream-internal-detail 10.0.0.7", http.StatusInternalServerError)
				return
			}
			login, ok := strings.CutPrefix(code, "ok-")
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code", "error_description": "The code passed is incorrect"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho-" + login})
		case "/user":
			login := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer gho-")
			_ = json.NewEncoder(w).Encode(map[string]any{"login": login, "id": 42})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthPages(t *testing.T) {
	gh := fakeGitHubOAuth(t)
	d, mux, logs := testDashboard(t, "client-id")
	d.githubURL, d.cfg.GitHubAPIURL = gh.URL, gh.URL

	// 开始登录：跳转到 GitHub，state 中带上安全的 next
	for next, want := range map[string]string{"/runners?state=online": "/runners?state=online", "//evil.example": "/"} {
		w := serve(mux, testReq{target: "/auth/github?next=" + url.QueryEscape(next), browser: true})
		loc, err := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusSeeOther || err != nil || !strings.HasPrefix(loc.String(), gh.URL+"/login/oauth/authorize?") {
			t.Fatalf("开始 GitHub 登录：HTTP %d → %q", w.Code, w.Header().Get("Location"))
		}
		p, _, _ := strings.Cut(loc.Query().Get("state"), ".")
		raw, _ := base64.RawURLEncoding.DecodeString(p)
		parts := strings.SplitN(string(raw), "|", 3)
		bc := responseCookie(w, oauthBrowserCookie)
		if bc == nil || !bc.HttpOnly || !bc.Secure || bc.Path != "/auth/github" || !oauthBrowserPattern.MatchString(bc.Value) {
			t.Fatalf("开始 GitHub 登录应设置浏览器标识的 cookie：%+v", bc)
		}
		if len(parts) != 3 || parts[1] != loginPurposePrefix+bc.Value || parts[2] != want {
			t.Errorf("next=%q：state 为 %q，用途应为 %q、next 应为 %q", next, raw, loginPurposePrefix+bc.Value, want)
		}
		// 同一个浏览器再次发起登录（例如另一个标签页）沿用同一个标识，先发起的登录仍然有效
		again := serve(mux, testReq{target: "/auth/github", cookie: bc, browser: true})
		if c := responseCookie(again, oauthBrowserCookie); c == nil || c.Value != bc.Value {
			t.Errorf("再次发起登录应沿用浏览器标识：%+v，原来是 %q", c, bc.Value)
		}
	}

	// 回调由 GitHub 跳转回来（浏览器直接打开）：错误页是内嵌了页面数据的 index.html
	state, browser := loginState(d, "/runners")
	cb := func(q string) *httptest.ResponseRecorder {
		return serve(mux, testReq{target: "/auth/github/callback?state=" + url.QueryEscape(state) + "&" + q, cookie: browser, browser: true})
	}
	// 登录 CSRF：回调不是来自发起登录的浏览器（没有 cookie、cookie 不同，或 state 中没有浏览器标识）时不登录
	other := &http.Cookie{Name: oauthBrowserCookie, Value: newRequestID()}
	for name, q := range map[string]testReq{
		"没有浏览器标识":        {target: "/auth/github/callback?code=ok-alice&state=" + url.QueryEscape(state), browser: true},
		"浏览器标识不同":        {target: "/auth/github/callback?code=ok-alice&state=" + url.QueryEscape(state), cookie: other, browser: true},
		"state 中没有浏览器标识": {target: "/auth/github/callback?code=ok-alice&state=" + url.QueryEscape(d.oauthState("login", "/runners")), cookie: browser, browser: true},
	} {
		w := serve(mux, q)
		wantErrorPage(t, name, w, http.StatusBadRequest, []string{"登录链接无效", "不是在当前浏览器中发起的"}, []string{"/auth/github?next=%2Frunners", "/login?next=%2Frunners"})
		if responseCookie(w, sessionCookie) != nil {
			t.Errorf("%s：不应设置会话", name)
		}
	}
	wantErrorPage(t, "没有权限的 GitHub 账号", cb("code=ok-mallory"), http.StatusForbidden,
		[]string{"这个 GitHub 账号没有权限", "mallory"}, []string{"/login?next=%2Frunners"})
	w := cb("code=down")
	p := wantErrorPage(t, "GitHub 不可用", w, http.StatusBadGateway, []string{"GitHub 登录失败", "错误编号"},
		[]string{"/auth/github?next=%2Frunners", "/login?next=%2Frunners"}, "upstream-internal-detail", "10.0.0.7")
	if p.Ref == "" || !strings.Contains(logs.String(), "ref="+p.Ref) || !strings.Contains(logs.String(), "upstream-internal-detail") {
		t.Errorf("GitHub 登录失败的详细信息应只写在日志中（编号 %q）：%s", p.Ref, logs.String())
	}
	wantErrorPage(t, "授权码无效", cb("code=bad"), http.StatusBadGateway, []string{"GitHub 登录失败"},
		[]string{"/auth/github?next=%2Frunners", "/login?next=%2Frunners"}, "The code passed is incorrect")
	wantErrorPage(t, "用户取消授权", cb("error=access_denied"), http.StatusBadRequest, []string{"没有完成 GitHub 授权"},
		[]string{"/auth/github?next=%2Frunners", "/login?next=%2Frunners"})
	wantErrorPage(t, "state 无效", serve(mux, testReq{target: "/auth/github/callback?state=forged.sig&code=ok-alice", browser: true}), http.StatusBadRequest,
		[]string{"登录链接无效"}, []string{"/login"})
	old := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d|login|/runners", time.Now().Add(-time.Minute).Unix()))
	old += "." + d.keys.SessionSig("oauth:"+old)
	wantErrorPage(t, "state 过期", serve(mux, testReq{target: "/auth/github/callback?code=ok-alice&state=" + url.QueryEscape(old), browser: true}), http.StatusBadRequest,
		[]string{"登录已过期"}, []string{"/login?next=%2Frunners"})

	// OAuth 关闭时开始登录：404 错误页，回到登录页
	_, off, _ := testDashboard(t, "")
	wantErrorPage(t, "OAuth 关闭", serve(off, testReq{target: "/auth/github?next=%2Frunners", browser: true}), http.StatusNotFound,
		[]string{"没有启用 GitHub 登录"}, []string{"/login?next=%2Frunners"})
}

func TestLayoutShell(t *testing.T) {
	d, mux, _ := testDashboard(t, "")
	now := time.Now()
	// 页面外壳的数据：当前身份（成员没有设置入口，由前端按 Session.Admin 决定）、CSRF、导航
	pg := parsePage(t, serve(mux, testReq{target: "/no/such/page", cookie: cookieFor(d, "bob", now.Add(time.Hour))}).Body.String())
	if pg.Session == nil || *pg.Session != (uiSession{Login: "bob"}) || pg.CSRF == "" || pg.Nav != "error" || textZH(t, pg.Title) != "找不到这个页面" {
		t.Errorf("成员的页面外壳：%+v", pg)
	}
	// 导航高亮页面所属的一项；flash 提示来自地址中的 flash 参数
	tok := sessionCookieFor(d, session{Login: admin.TokenActor, Via: viaToken, Exp: now.Add(time.Hour).Unix()})
	for page, nav := range map[string]string{"runner_new": "runners", "job": "runs", "settings_scope": "settings", "overview": "overview"} {
		r := httptest.NewRequest(http.MethodGet, "https://kiln.example.com/x?flash="+url.QueryEscape("已暂停接单"), nil)
		r.Header.Set(uiHeader, "1")
		r.AddCookie(tok)
		w := httptest.NewRecorder()
		d.render(w, r, page, "标题", struct{}{})
		pg := parsePage(t, w.Body.String())
		if pg.Page != page || pg.Nav != nav || textZH(t, pg.Flash) != "已暂停接单" || textZH(t, pg.Title) != "标题" {
			t.Errorf("页面 %s：Page %q Nav %q Flash %q，导航应为 %q", page, pg.Page, pg.Nav, pg.Flash, nav)
		}
		if se := pg.Session; se == nil || !se.Admin || !se.ViaToken || se.Bound {
			t.Errorf("用 admin token 登录（没有绑定）的身份：%+v", se)
		}
	}
	if strings.Contains(fmt.Sprint(pageNavs), "approval") {
		t.Errorf("导航中不应再有审批入口")
	}
}

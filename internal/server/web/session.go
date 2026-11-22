package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// Dashboard 的登录与会话（PLAN #16.5）：
//   - GitHub OAuth 登录：管理员绑定的 GitHub 账号（admin_binding）和 KILN_ADMINS 中的账号为管理员，KILN_USERS 中的为成员；
//   - admin token 登录：管理员的备用入口。管理员绑定 GitHub 账号后，token 登录的身份就是这个账号（同一个人）；
//     确认 GitHub 登录可用后可以关闭 token 登录（CLI 不受影响，kiln admin token-login on 可以恢复）。
// 会话 cookie 只保存身份（登录名、GitHub ID、登录方式），角色在每次请求时按当前的绑定和名单计算：
// 关闭 token 登录、解除绑定或调整名单后，受影响的会话随即失效或降级，不用等会话过期。

const (
	sessionCookie = "kiln_session"
	sessionTTL    = 7 * 24 * time.Hour
	oauthStateTTL = 10 * time.Minute
	// maxOAuthResponse 是 GitHub OAuth 响应体的读取上限。
	maxOAuthResponse = 1 << 20
)

// 会话中的角色（session.Role）。
const (
	roleAdmin = "admin"
	roleUser  = "user"
)

// 登录方式（session.Via）。
const (
	viaToken  = "token"
	viaGitHub = "github"
)

// 登录页说明的原因（/login?reason=）。
const (
	// reasonExpired：会话已过期或无效。
	reasonExpired = "expired"
	// reasonRevoked：会话因为权限变化失效（关闭了 admin token 登录、解除了绑定或不在名单中了）。
	reasonRevoked = "revoked"
	// reasonLogout：已退出。
	reasonLogout = "logout"
)

type session struct {
	Login string `json:"l"`
	// ID 是 GitHub 用户 ID（GitHub 登录，或绑定后的 admin token 登录）；0 表示未知。
	ID int64 `json:"i,omitempty"`
	// Via 是登录方式（viaToken、viaGitHub）；旧的会话没有这一项，按登录名推断。
	Via string `json:"v,omitempty"`
	Exp int64  `json:"e"`

	// 以下不保存在 cookie 中，每次请求时计算（resolveSession）。

	// Role 是 roleAdmin 或 roleUser。
	Role string `json:"-"`
	// Bound 表示这是管理员绑定的 GitHub 账号。
	Bound bool `json:"-"`
}

func (s *session) Admin() bool { return s != nil && s.Role == roleAdmin }

// ViaToken 表示用 admin token 登录。
func (s *session) ViaToken() bool { return s != nil && s.Via == viaToken }

// githubLogin 返回会话对应的 GitHub 登录名（"我发起的"按它筛选）；还没有绑定 GitHub 账号的 admin token 登录为空，
// 这时的登录名是 admin.TokenActor，不是 GitHub 账号。按登录方式和 ID 判断，不按登录名：GitHub 上也可能有叫这个名字的用户。
func (s *session) githubLogin() string {
	if s.Via == viaToken && s.ID == 0 {
		return ""
	}
	return s.Login
}

// secureCookies 判断是否给 Cookie 加 Secure：只有通过 https 访问时才能加，否则本地 http 部署无法登录。
func (d *Dashboard) secureCookies() bool { return strings.HasPrefix(d.cfg.PublicURL, "https://") }

func (d *Dashboard) setSession(w http.ResponseWriter, s session) {
	s.Exp = time.Now().Add(sessionTTL).Unix()
	p := base64.RawURLEncoding.EncodeToString(store.MustJSON(s))
	//nolint:gosec // Secure 取决于 PublicURL 是否为 https（见 secureCookies），HttpOnly 和 SameSite 已设置
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: p + "." + d.keys.SessionSig(p), Path: "/", HttpOnly: true,
		Secure: d.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second),
	})
}

// adminBinding 返回管理员绑定的 GitHub 账号；还没有绑定时返回 nil。没有 admin.Service 时（不连接数据库的测试）视为没有绑定。
func (d *Dashboard) adminBinding(ctx context.Context) (*store.AdminBinding, error) {
	if d.admin == nil {
		return nil, nil
	}
	return d.admin.AdminBinding(ctx)
}

// defaultNext 是没有指定（或指定的不安全）时登录后进入的页面。
const defaultNext = "/"

// safeNext 只允许站内的绝对路径作为登录后的跳转目标，防止开放重定向；不安全时返回 defaultNext。
//   - 必须以单个 / 开头：拒绝 //host、/\host 和 https://host、javascript: 等；
//   - 拒绝反斜杠和控制字符：浏览器会把 \ 当作 /，并删除地址中的制表符和换行（/<TAB>/evil 会变成 //evil）；
//   - 不跳回登录、退出和 OAuth 地址（避免登录后立即退出或循环）。
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') ||
		strings.IndexFunc(next, unicode.IsControl) >= 0 {
		return defaultNext
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return defaultNext
	}
	if u.Path == "/login" || u.Path == "/logout" || strings.HasPrefix(u.Path, "/auth/") {
		return defaultNext
	}
	return next
}

// returnTo 返回登录后应回到的站内地址：GET 为当前地址，其他方法为提交表单的页面（同源的 Referer），没有时为空。
func returnTo(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return r.URL.RequestURI()
	}
	return refererPath(r)
}

// refererPath 返回同源 Referer 的站内路径（经 safeNext 检查），不同源或没有 Referer 时返回空字符串。
func refererPath(r *http.Request) string {
	u, err := url.Parse(r.Referer())
	if err != nil || u.Host == "" || u.Host != r.Host {
		return ""
	}
	p := u.RequestURI()
	if safeNext(p) != p {
		return ""
	}
	return p
}

// loginURL 返回登录页的地址：next 为登录后回到的站内地址（可以为空），reason 为登录页说明的原因（reasonExpired 等，可以为空）。
func loginURL(next, reason string) string {
	var q []string
	if next != "" && safeNext(next) == next {
		q = append(q, "next="+url.QueryEscape(next))
	}
	if reason != "" {
		q = append(q, "reason="+reason)
	}
	if len(q) == 0 {
		return "/login"
	}
	return "/login?" + strings.Join(q, "&")
}

// sessionKey 是请求 ctx 中已经计算好的会话的键（web 包装器设置，渲染页面时复用，不再查一次数据库）。
type sessionKey struct{}

// getSession 返回当前请求的有效会话，没有时返回 nil。查询绑定失败时记录告警并视为未登录。
func (d *Dashboard) getSession(r *http.Request) *session {
	if se, ok := r.Context().Value(sessionKey{}).(*session); ok {
		return se
	}
	se, _, err := d.sessionState(r)
	if err != nil {
		d.log.Warn("读取会话", "err", err)
	}
	return se
}

// sessionState 返回有效的会话。没有有效会话但请求带着会话 cookie 时，reason 说明原因：
// reasonExpired（已过期、签名无效），或 reasonRevoked（权限变化：关闭了 admin token 登录、解除了绑定、不在名单中了）。
func (d *Dashboard) sessionState(r *http.Request) (se *session, reason string, err error) {
	s, present := d.decodeSession(r)
	switch {
	case !present:
		return nil, "", nil
	case s == nil:
		return nil, reasonExpired, nil
	}
	bind, err := d.adminBinding(r.Context())
	if err != nil {
		return nil, "", fmt.Errorf("读取管理员绑定：%w", err)
	}
	if !d.resolveSession(s, bind) {
		return nil, reasonRevoked, nil
	}
	return s, "", nil
}

// decodeSession 校验并解码请求中的会话 cookie。present 表示请求带着会话 cookie；cookie 签名无效、无法解码或已过期时 s 为 nil。
func (d *Dashboard) decodeSession(r *http.Request) (s *session, present bool) {
	c, cerr := r.Cookie(sessionCookie)
	if cerr != nil || c.Value == "" {
		return nil, false
	}
	p, sig, ok := strings.Cut(c.Value, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(sig), []byte(d.keys.SessionSig(p))) != 1 {
		return nil, true
	}
	b, derr := base64.RawURLEncoding.DecodeString(p)
	if derr != nil {
		return nil, true
	}
	var v session
	if json.Unmarshal(b, &v) != nil || time.Now().Unix() > v.Exp {
		return nil, true
	}
	return &v, true
}

// resolveSession 按当前的绑定和名单计算会话的角色，返回会话是否仍然有效。
//   - admin token 登录：关闭了 token 登录时失效；否则为管理员，绑定后身份是绑定的账号；
//   - GitHub 登录：绑定的账号和 KILN_ADMINS 中的为管理员，KILN_USERS 中的为成员，都不是时失效。
func (d *Dashboard) resolveSession(s *session, bind *store.AdminBinding) bool {
	if s.Via == "" { // 旧的会话：admin token 登录的登录名是 TokenActor
		s.Via = viaGitHub
		if s.Login == admin.TokenActor {
			s.Via = viaToken
		}
	}
	switch s.Via {
	case viaToken:
		if bind != nil && !bind.TokenLogin {
			return false
		}
		s.Role = roleAdmin
		s.Bound = bind != nil && s.Login == bind.GitHubLogin
		return true
	case viaGitHub:
		s.Bound = boundTo(bind, s.Login, s.ID)
		s.Role = d.roleOf(bind, s.Login, s.ID)
		return s.Role != ""
	}
	return false
}

// boundTo 判断 GitHub 账号是否为管理员绑定的账号：有 ID 时按 ID 比较（登录名可以改），否则按登录名（不区分大小写）。
func boundTo(bind *store.AdminBinding, login string, id int64) bool {
	if bind == nil {
		return false
	}
	if id != 0 {
		return id == bind.GitHubID
	}
	return strings.EqualFold(login, bind.GitHubLogin)
}

// validCSRF 检查表单的 csrf 字段是否与会话的 CSRF token 一致（常量时间比较）。
func (d *Dashboard) validCSRF(r *http.Request, se *session) bool {
	return subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(d.csrfToken(se))) == 1
}

func (d *Dashboard) csrfToken(se *session) string {
	return d.keys.SessionSig(fmt.Sprintf("csrf:%s:%d", se.Login, se.Exp))[:22]
}

// oauthEnabled 表示是否可以用 GitHub 登录。
func (d *Dashboard) oauthEnabled() bool { return d.cfg.GitHubClientID != "" }

// loginPage 是登录页的数据。
type loginPage struct {
	// Next 是登录后回到的站内地址（已经过 safeNext 检查）。
	Next string
	// GitHubURL 是 GitHub 登录的入口（带上 Next）。
	GitHubURL string
	// Notice 是登录页顶部的说明（会话已过期、已失效、已退出；消息）。
	Notice uiText
	// TokenLogin 表示可以用 admin token 登录（管理员绑定 GitHub 账号后可以关闭）。
	TokenLogin bool
	// Bound 是管理员绑定的 GitHub 账号（没有绑定时为空），关闭 token 登录后登录页提示用它登录。
	Bound string
	// Form 是 admin token 表单的状态：token 错误时 Errors["token"] 有说明；不保存、不回显输入的 token。
	Form *formState
}

func (d *Dashboard) newLoginPage(ctx context.Context, next string) (*loginPage, error) {
	next = safeNext(next)
	p := &loginPage{Next: next, GitHubURL: githubLoginURL(next), TokenLogin: true, Form: newForm()}
	bind, err := d.adminBinding(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取管理员绑定：%w", err)
	}
	if bind != nil {
		p.Bound, p.TokenLogin = bind.GitHubLogin, bind.TokenLogin
	}
	return p, nil
}

// pageLogin 显示登录页：OAuth 可用时 GitHub 登录是主要入口，admin token 是标明的管理员备用入口（关闭后不显示）；
// OAuth 关闭时只有 admin token。
func (d *Dashboard) pageLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := d.newLoginPage(r.Context(), q.Get("next"))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	switch q.Get("reason") {
	case reasonExpired:
		p.Notice = message(msgLoginNoticeExpired)
	case reasonRevoked:
		p.Notice = message(msgLoginNoticeRevoked)
	case reasonLogout:
		p.Notice = message(msgLoginNoticeLogout)
	}
	w.Header().Set("Cache-Control", "no-store")
	d.renderPage(w, r, "login", message(msgLoginTitle), p, http.StatusOK)
}

// handleLogin 用 admin token 登录。token 不正确时重新显示登录页（401），在字段旁说明，保留安全的 next，不回显 token；
// 关闭了 token 登录时返回 403。管理员绑定了 GitHub 账号时，登录的身份就是这个账号。
func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	tok := r.PostFormValue("token")
	p, err := d.newLoginPage(r.Context(), r.PostFormValue("next"))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	status := http.StatusUnauthorized
	switch {
	case !p.TokenLogin:
		status = http.StatusForbidden
		p.Form.failText(formGeneral, message(msgLoginErrAdminOff))
	case tok == "":
		p.Form.failText("token", message(msgLoginErrAdminEmpty))
	case !d.keys.ValidAdminToken(tok):
		p.Form.failText("token", message(msgLoginErrAdminWrong))
	}
	if p.Form.Failed() {
		w.Header().Set("Cache-Control", "no-store")
		d.renderPage(w, r, "login", message(msgLoginTitle), p, status)
		return
	}
	se := session{Login: admin.TokenActor, Via: viaToken}
	if bind, err := d.adminBinding(r.Context()); err != nil {
		d.serverError(w, r, err)
		return
	} else if bind != nil {
		se.Login, se.ID = bind.GitHubLogin, bind.GitHubID
	}
	d.setSession(w, se)
	d.auditBestEffort(r.Context(), se.Login, "login", map[string]any{"via": viaToken, "ip": r.RemoteAddr})
	d.redirect(w, r, p.Next)
}

// handleLogout 退出登录：清除会话 cookie 后跳转到登录页。只接受 POST 并校验 CSRF，其他网站不能让用户被动退出；
// 没有有效会话（已过期或已经退出）时不需要 CSRF，直接清除并跳转。
func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	if se := d.getSession(r); se != nil && !d.validCSRF(r, se) {
		d.csrfFailed(w, r)
		return
	}
	//nolint:gosec // Secure 取决于 PublicURL 是否为 https（见 secureCookies），HttpOnly 和 SameSite 已设置
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: d.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, loginURL("", reasonLogout), http.StatusSeeOther)
}

// OAuth state 的用途：登录（loginPurposePrefix + 发起登录的浏览器的标识，见 oauthBrowser），
// 或把管理员绑定到 GitHub 账号（bindPurposePrefix + 发起绑定的会话的指纹）。
const (
	loginPurposePrefix = "login:"
	bindPurposePrefix  = "bind:"
)

// oauthBrowserCookie 保存发起 GitHub 登录的浏览器的随机标识（RFC 6749 #10.12）：登录的 state 中带着它，
// 回调时要求同一个浏览器带着相同的 cookie，别人不能把自己的授权回调链接发给受害者、让受害者登录成他的账号。
const oauthBrowserCookie = "kiln_oauth"

// oauthBrowserPattern 是 oauthBrowserCookie 的格式（与请求 ID 相同：32 位 hex）。
var oauthBrowserPattern = requestIDPattern

// oauthBrowser 返回发起登录的浏览器的标识并（重新）设置 cookie：已有有效的 cookie 时沿用，
// 同一个浏览器同时在几个标签页中登录也不会互相作废。
func (d *Dashboard) oauthBrowser(w http.ResponseWriter, r *http.Request) string {
	id := ""
	if c, err := r.Cookie(oauthBrowserCookie); err == nil && oauthBrowserPattern.MatchString(c.Value) {
		id = c.Value
	} else {
		id = newRequestID()
	}
	//nolint:gosec // Secure 取决于 PublicURL 是否为 https（见 secureCookies），HttpOnly 和 SameSite 已设置
	http.SetCookie(w, &http.Cookie{
		Name: oauthBrowserCookie, Value: id, Path: "/auth/github", HttpOnly: true,
		Secure: d.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: int(oauthStateTTL / time.Second),
	})
	return id
}

// sameOAuthBrowser 判断回调请求是否来自发起登录的浏览器（带着 state 中的标识）。
func sameOAuthBrowser(r *http.Request, id string) bool {
	c, err := r.Cookie(oauthBrowserCookie)
	return err == nil && id != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(id)) == 1
}

// oauthState 返回签名的 OAuth state：<到期时间>|<用途>|<next>。
func (d *Dashboard) oauthState(purpose, next string) string {
	exp := strconv.FormatInt(time.Now().Add(oauthStateTTL).Unix(), 10)
	p := base64.RawURLEncoding.EncodeToString([]byte(exp + "|" + purpose + "|" + next))
	return p + "." + d.keys.SessionSig("oauth:"+p)
}

// bindFingerprint 把绑定的 OAuth state 和发起绑定的会话关联：只有同一个会话完成授权才会绑定，
// 别人拿到授权链接也不能把自己的账号绑定为管理员。
func (d *Dashboard) bindFingerprint(se *session) string {
	return d.keys.SessionSig(fmt.Sprintf("bind:%s:%d", se.Login, se.Exp))[:16]
}

// authorizeURL 返回 GitHub 授权页的地址。
func (d *Dashboard) authorizeURL(state string) string {
	q := url.Values{"client_id": {d.cfg.GitHubClientID}, "redirect_uri": {d.cfg.PublicURL + "/auth/github/callback"}, "state": {state}}
	return d.githubURL + "/login/oauth/authorize?" + q.Encode()
}

func (d *Dashboard) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if !d.oauthEnabled() {
		d.showError(w, r, &errorPage{
			Status: http.StatusNotFound, Heading: message(msgErrOAuthOffHeading), Reason: message(msgErrOAuthOffReason),
			Links: []errorLink{loginLink(msgErrLinkBackToLogin, next, true)},
		})
		return
	}
	http.Redirect(w, r, d.authorizeURL(d.oauthState(loginPurposePrefix+d.oauthBrowser(w, r), next)), http.StatusSeeOther)
}

// handleOAuthCallback 完成 GitHub 登录或绑定。失败时显示错误页：说明原因，提供重新登录；不显示 GitHub 返回的原始内容（只写日志）。
func (d *Dashboard) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, sig, _ := strings.Cut(q.Get("state"), ".")
	var raw []byte
	var err error
	if subtle.ConstantTimeCompare([]byte(sig), []byte(d.keys.SessionSig("oauth:"+p))) == 1 {
		raw, err = base64.RawURLEncoding.DecodeString(p)
	}
	expS, rest, ok1 := strings.Cut(string(raw), "|")
	purpose, next, ok2 := strings.Cut(rest, "|")
	if raw == nil || err != nil || !ok1 || !ok2 {
		d.showError(w, r, &errorPage{
			Status: http.StatusBadRequest, Heading: message(msgErrOAuthBadHeading), Reason: message(msgErrOAuthBadReason),
			Links: []errorLink{loginLink(msgErrLinkRelogin, "", true)},
		})
		return
	}
	next = safeNext(next)
	binding := strings.HasPrefix(purpose, bindPurposePrefix)
	if exp, err := strconv.ParseInt(expS, 10, 64); err != nil || time.Now().Unix() > exp {
		d.showError(w, r, &errorPage{
			Status: http.StatusBadRequest, Heading: message(msgErrOAuthExpHeading),
			Reason: message(msgErrOAuthExpReason, intArg(argCount, int64(oauthStateTTL/time.Minute))),
			Links:  []errorLink{loginLink(msgErrLinkRelogin, next, true)},
		})
		return
	}
	if e := q.Get("error"); e != "" {
		// 用户在 GitHub 上拒绝了授权（access_denied）等
		d.log.Info("GitHub 授权没有完成", "error", shorten.Text(e, 100))
		links := []errorLink{{Label: message(msgErrLinkGitHubRelogin), URL: githubLoginURL(next), Primary: true}, loginLink(msgErrLinkBackToLogin, next, false)}
		if binding {
			links = []errorLink{{Label: message(msgErrLinkAccount), URL: accountURL, Primary: true}}
		}
		d.showError(w, r, &errorPage{
			Status: http.StatusBadRequest, Heading: message(msgErrOAuthDenyHeading), Reason: message(msgErrOAuthDenyReason),
			Links: links,
		})
		return
	}
	if binding {
		d.finishBind(w, r, strings.TrimPrefix(purpose, bindPurposePrefix), q.Get("code"))
		return
	}
	if browser, ok := strings.CutPrefix(purpose, loginPurposePrefix); !ok || !sameOAuthBrowser(r, browser) {
		d.showError(w, r, &errorPage{
			Status: http.StatusBadRequest, Heading: message(msgErrOAuthBadHeading), Reason: message(msgErrOAuthBrowserReason),
			Links: []errorLink{{Label: message(msgErrLinkGitHubRelogin), URL: githubLoginURL(next), Primary: true}, loginLink(msgErrLinkBackToLogin, next, false)},
		})
		return
	}
	login, id, ok := d.exchangeOrFail(w, r, q.Get("code"), next)
	if !ok {
		return
	}
	bind, err := d.adminBinding(r.Context())
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if d.roleOf(bind, login, id) == "" {
		d.log.Info("GitHub 账号没有权限登录", "login", login, "id", id)
		d.showError(w, r, &errorPage{
			Status: http.StatusForbidden, Heading: message(msgErrOAuthNoAccHeading), Reason: message(msgErrOAuthNoAccReason, strArg(argLogin, login)),
			Links: []errorLink{loginLink(msgErrLinkBackToLogin, next, true)},
		})
		return
	}
	d.setSession(w, session{Login: login, ID: id, Via: viaGitHub})
	d.auditBestEffort(r.Context(), login, "login", map[string]any{"via": viaGitHub, "ip": r.RemoteAddr})
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // next 已经过 safeNext，只允许站内路径
}

// exchangeOrFail 用授权码换取 GitHub 账号；失败时显示错误页（详细信息只写日志）并返回 false。
func (d *Dashboard) exchangeOrFail(w http.ResponseWriter, r *http.Request, code, next string) (string, int64, bool) {
	login, id, err := d.oauthExchange(r.Context(), code)
	if err == nil {
		return login, id, true
	}
	ref := newErrorRef()
	d.log.Warn("GitHub 登录失败", "ref", ref, "err", err)
	d.showError(w, r, &errorPage{
		Status: http.StatusBadGateway, Heading: message(msgErrOAuthFailHeading), Ref: ref, Reason: message(msgErrOAuthFailReason),
		Links: []errorLink{{Label: message(msgErrLinkGitHubRetry), URL: githubLoginURL(next), Primary: true}, loginLink(msgErrLinkBackToLogin, next, false)},
	})
	return "", 0, false
}

// roleOf 返回 GitHub 用户在 Dashboard 中的角色（roleAdmin、roleUser），没有权限时返回空字符串。
// 管理员绑定的账号和 KILN_ADMINS 中的账号是管理员，KILN_USERS 中的是成员。
func (d *Dashboard) roleOf(bind *store.AdminBinding, login string, id int64) string {
	l, ids := strings.ToLower(login), strconv.FormatInt(id, 10)
	switch {
	case boundTo(bind, login, id), d.cfg.Admins[l] || d.cfg.Admins[ids]:
		return roleAdmin
	case d.cfg.Users[l] || d.cfg.Users[ids]:
		return roleUser
	}
	return ""
}

// oauthExchange 用授权码换取 access token，并读取 GitHub 用户的登录名和 ID。
func (d *Dashboard) oauthExchange(ctx context.Context, code string) (string, int64, error) {
	form := url.Values{"client_id": {d.cfg.GitHubClientID}, "client_secret": {d.cfg.GitHubClientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.githubURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := d.doJSON(req, &tok); err != nil {
		return "", 0, fmt.Errorf("换取令牌失败：%w", err)
	}
	if tok.AccessToken == "" {
		return "", 0, fmt.Errorf("换取令牌失败：%s", tok.Error)
	}
	api := d.cfg.GitHubAPIURL
	if api == "" {
		api = githubAPIURL
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, api+"/user", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := d.doJSON(req, &u); err != nil {
		return "", 0, fmt.Errorf("读取用户信息失败：%w", err)
	}
	if u.Login == "" {
		return "", 0, errors.New("读取用户信息失败：响应中没有 login")
	}
	return u.Login, u.ID, nil
}

// doJSON 发送请求，检查状态码并解析 JSON 响应。
func (d *Dashboard) doJSON(req *http.Request, v any) error {
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxOAuthResponse)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(body)
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, shorten.Text(strings.TrimSpace(string(msg)), 200))
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("解析响应：%w", err)
	}
	return nil
}

// auditBestEffort 记录审计日志；用于操作本身已经完成、审计失败不应让请求失败的场景（只记录告警）。
func (d *Dashboard) auditBestEffort(ctx context.Context, actor, action string, detail any) {
	if err := store.Audit(ctx, d.db, actor, action, detail); err != nil {
		d.log.Warn("记录审计日志失败", "action", action, "actor", actor, "err", err)
	}
}

package web

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 登录与身份（管理员绑定 GitHub 账号、关闭 admin token 登录）、成员和活动分区，以及会话的即时失效。

// tokenLogin 用 admin token 登录，返回会话。
func (e *webEnv) tokenLogin() *client {
	e.t.Helper()
	w := serve(e.mux, testReq{method: http.MethodPost, target: "/login", form: url.Values{"token": {testAdminToken}, "next": {accountURL}}})
	wantRedirect(e.t, "admin token 登录", w, accountURL)
	return e.sessionClient(sessionCookieOf(e.t, w))
}

// withOAuth 让 Dashboard 使用模拟的 GitHub OAuth（任何 code=ok-<login> 都换到 <login>，ID 为 42）。
func (e *webEnv) withOAuth() {
	gh := fakeGitHubOAuth(e.t)
	e.d.githubURL, e.d.cfg.GitHubAPIURL, e.d.cfg.GitHubClientID = gh.URL, gh.URL, "client-id"
}

// bind 以 c 的身份完成一次绑定（GitHub 账号 login），返回绑定后的会话。
func (e *webEnv) bind(c *client, login string) *client {
	e.t.Helper()
	loc, err := url.Parse(redirectTarget(e.t, "开始绑定", e.post(c, accountURL+"/bind", nil)))
	e.must(err)
	state := loc.Query().Get("state")
	if !strings.HasPrefix(loc.Path, "/login/oauth/authorize") || state == "" {
		e.t.Fatalf("开始绑定应跳转到 GitHub 授权页：%s", loc)
	}
	// GitHub 授权后浏览器回到回调地址
	w := serve(e.mux, testReq{target: "/auth/github/callback?code=ok-" + login + "&state=" + url.QueryEscape(state), cookie: c.cookie, browser: true})
	if flash := wantRedirect(e.t, "完成绑定", w, accountURL); !strings.Contains(flashZH(e.t, flash), "已绑定 GitHub 账号 "+login) {
		e.t.Errorf("绑定后的提示 = %q", flash)
	}
	return e.sessionClient(sessionCookieOf(e.t, w))
}

// accountData 是登录与身份页的页面数据（accountPage），另带计算的 Step（第几步）。
type accountData struct {
	accountPage
	Step int
}

func TestAccountBindFlow(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.withOAuth()
	tok := e.tokenLogin()

	// 还没有绑定：第 1 步
	body := e.get(tok, accountURL).Body.String()
	p := pageData[accountData](t, body)
	expect(t, "还没有绑定", p, p.Step == 1, p.Binding == nil, p.OAuth, !p.SignedInAsBound)
	if se := parsePage(t, body).Session; se == nil || *se != (uiSession{Login: admin.TokenActor, Admin: true, ViaToken: true}) {
		t.Errorf("admin token 登录的身份：%+v", se)
	}

	// 不能在绑定之前关闭 admin token 登录
	flash := wantRedirect(t, "绑定前关闭", e.post(tok, accountURL+"/token-login", url.Values{"enabled": {"0"}}), accountURL)
	if !strings.Contains(flash, "没有关闭") {
		t.Errorf("绑定前关闭的提示 = %q", flash)
	}

	// 绑定 octocat（不在 KILN_ADMINS 中）：绑定后当前会话换成 octocat 的 GitHub 登录，且是管理员
	gh := e.bind(tok, "octocat")
	if gh.login != "octocat" {
		t.Fatalf("绑定后的会话 = %q", gh.login)
	}
	wantStatus(t, "绑定的账号打开设置", e.get(gh, "/settings/ops"), http.StatusOK)
	if a := e.audits("admin.bind"); len(a) != 1 || a[0].Actor != admin.TokenActor || a[0].Detail["github_login"] != "octocat" {
		t.Errorf("绑定的审计 = %+v", a)
	}
	body = e.get(gh, accountURL).Body.String()
	p = pageData[accountData](t, body)
	if expect(t, "绑定后（可以关闭 admin token 登录）", p, p.Step == 3, p.Binding != nil, p.SignedInAsBound) {
		expect(t, "绑定", p.Binding, p.Binding.GitHubLogin == "octocat", p.Binding.GitHubID == 42, p.Binding.TokenLogin)
	}
	if se := parsePage(t, body).Session; se == nil || *se != (uiSession{Login: "octocat", Admin: true, Bound: true}) {
		t.Errorf("绑定后的身份：%+v", se)
	}

	// admin token 会话不能关闭（还没有证明 GitHub 登录可用）；绑定的 GitHub 会话可以
	wantRedirect(t, "token 会话关闭", e.post(tok, accountURL+"/token-login", url.Values{"enabled": {"0"}}), accountURL)
	if b, _ := store.GetAdminBinding(e.ctx, e.db); b == nil || !b.TokenLogin {
		t.Fatalf("admin token 会话不应能关闭 token 登录：%+v", b)
	}
	wantRedirect(t, "GitHub 会话关闭", e.post(gh, accountURL+"/token-login", url.Values{"enabled": {"0"}}), accountURL)
	if b, _ := store.GetAdminBinding(e.ctx, e.db); b == nil || b.TokenLogin {
		t.Fatalf("应已关闭 token 登录：%+v", b)
	}
	if a := e.audits("admin.token_login.disable"); len(a) != 1 || a[0].Actor != "octocat" {
		t.Errorf("关闭的审计 = %+v", a)
	}

	// 关闭后：已有的 admin token 会话立即失效，登录页不再提供 admin token，POST /login 被拒绝
	wantRedirect(t, "失效的 token 会话", e.get(tok, "/runs"), "/login?next=%2Fruns&reason=revoked")
	body = serve(e.mux, testReq{target: "/login?reason=revoked"}).Body.String()
	lp := pageData[loginData](t, body)
	expect(t, "关闭后的登录页不应再提供 admin token", lp, !lp.TokenLogin, lp.Bound == "octocat", parsePage(t, body).OAuth, strings.Contains(textZH(t, lp.Notice), "登录已失效"))
	w := serve(e.mux, testReq{method: http.MethodPost, target: "/login", form: url.Values{"token": {testAdminToken}}})
	wantStatus(t, "关闭后用 admin token 登录", w, http.StatusForbidden)
	if lf := pageData[loginData](t, w.Body.String()); !strings.Contains(textZH(t, lf.Form.Errors[formGeneral]), "admin token 登录已关闭") {
		t.Errorf("关闭后用 admin token 登录：%+v", lf.Form)
	}
	p = pageData[accountData](t, e.get(gh, accountURL).Body.String())
	expect(t, "全部完成", p, p.Step == 4, p.Binding != nil && !p.Binding.TokenLogin)

	// 重新开启后可以再用 admin token 登录，身份是绑定的账号
	wantRedirect(t, "重新开启", e.post(gh, accountURL+"/token-login", url.Values{"enabled": {"1"}}), accountURL)
	if again := e.tokenLogin(); again.login != "octocat" {
		t.Errorf("绑定后 admin token 登录的身份 = %q，应为绑定的账号", again.login)
	}

	// 解除绑定需要确认；解除后 octocat 不在名单中，会话随即失效
	if flash := wantRedirect(t, "没有确认", e.post(gh, accountURL+"/unbind", nil), accountURL); !strings.Contains(flash, "请勾选确认") {
		t.Errorf("没有确认的提示 = %q", flash)
	}
	wantRedirect(t, "解除绑定", e.post(gh, accountURL+"/unbind", url.Values{"confirm": {"1"}}), accountURL)
	if b, _ := store.GetAdminBinding(e.ctx, e.db); b != nil {
		t.Fatalf("应已解除绑定：%+v", b)
	}
	wantRedirect(t, "解除后的会话", e.get(gh, "/runs"), "/login?next=%2Fruns&reason=revoked")
}

func TestAccountBindRequiresSameSession(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.withOAuth()
	tok := e.tokenLogin()
	loc, err := url.Parse(redirectTarget(t, "开始绑定", e.post(tok, accountURL+"/bind", nil)))
	e.must(err)
	state := url.QueryEscape(loc.Query().Get("state"))

	// 另一个管理员（alice）的浏览器打开同一个授权回调：不绑定
	w := serve(e.mux, testReq{target: "/auth/github/callback?code=ok-mallory&state=" + state, cookie: e.alice.cookie, browser: true})
	wantStatus(t, "其他会话完成绑定", w, http.StatusBadRequest)
	if p := pageData[errorPage](t, w.Body.String()); !strings.Contains(textZH(t, p.Heading), "绑定链接无效") {
		t.Errorf("其他会话完成绑定：%+v", p)
	}
	// 没有登录时也不绑定
	wantStatus(t, "未登录完成绑定", serve(e.mux, testReq{target: "/auth/github/callback?code=ok-mallory&state=" + state, browser: true}), http.StatusBadRequest)
	if b, _ := store.GetAdminBinding(e.ctx, e.db); b != nil {
		t.Fatalf("不应绑定：%+v", b)
	}
	// 篡改用途（把登录的 state 当成绑定）：签名不对
	p, _, _ := strings.Cut(loc.Query().Get("state"), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(p)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "|bind:", "|login:", 1)))
	wantStatus(t, "篡改 state", serve(e.mux, testReq{target: "/auth/github/callback?code=ok-mallory&state=" + forged + ".x", cookie: tok.cookie, browser: true}), http.StatusBadRequest)

	// 成员不能发起绑定
	wantStatus(t, "成员发起绑定", e.post(e.bob, accountURL+"/bind", nil), http.StatusForbidden)
}

func TestSessionRevokedWhenRemoved(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	wantStatus(t, "bob 打开运行列表", e.get(e.bob, "/runs"), http.StatusOK)
	// 从 KILN_USERS 中移除后，已有会话在下一个请求就失效（不等会话到期）
	delete(e.d.cfg.Users, "bob")
	wantRedirect(t, "移除后", e.get(e.bob, "/runs"), "/login?next=%2Fruns&reason=revoked")
	mustHave(t, "移除后的登录页", pageTextZH(t, serve(e.mux, testReq{target: "/login?reason=revoked"}).Body.String()), "登录已失效")
	// 从 KILN_ADMINS 改到 KILN_USERS：立即失去设置权限
	delete(e.d.cfg.Admins, "alice")
	e.d.cfg.Users["alice"] = true
	wantStatus(t, "降为成员后打开设置", e.get(e.alice, "/settings"), http.StatusForbidden)
}

func TestMembersAndActivityPages(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.must(store.PutAdminBinding(e.ctx, e.db, 42, "octocat", "admin"))
	e.must(store.Audit(e.ctx, e.db, "bob", "login", map[string]any{"via": "github"}))
	run := e.addRun(runOpts{state: store.RunFailed})
	e.must(store.Audit(e.ctx, e.db, "alice", "run.cancel", map[string]any{"run_id": run}))
	e.must(store.Audit(e.ctx, e.db, "bob", "runner.drain", map[string]any{"name": "mac-1"}))

	mp := pageData[membersPage](t, e.get(e.alice, membersURL).Body.String())
	source := map[string]string{}
	for _, m := range mp.Members {
		source[m.Login] = m.Source
	}
	if len(mp.Members) == 0 || mp.Members[0].Login != "octocat" || !mp.Members[0].Bound || !mp.Bound {
		t.Errorf("绑定的账号应排在最前面：%+v", mp.Members)
	}
	if source["octocat"] != "绑定的 GitHub 账号" || source["alice"] != "KILN_ADMINS" || source["bob"] != "KILN_USERS" || source["carol"] == "" {
		t.Errorf("成员的权限来源：%v", source)
	}
	for _, m := range mp.Members {
		if m.Me != (m.Login == "alice") {
			t.Errorf("Me 应只标出当前用户 alice：%+v", m)
		}
	}

	// activityData 是活动页的页面数据，另带每条活动计算的说明和链接。
	type activityData struct {
		activityPage
		Entries []struct {
			Actor, Action, Text, Link string
		}
	}
	ap := pageData[activityData](t, e.get(e.alice, activityURL).Body.String())
	texts := map[string]string{}
	for _, en := range ap.Entries {
		texts[en.Action] = en.Text
		if en.Action == "run.cancel" && en.Link != runURL(run) {
			t.Errorf("取消 run 的活动应链接到 run：%+v", en)
		}
	}
	if texts["run.cancel"] != fmt.Sprintf("取消了 run #%d", run) || texts["runner.drain"] != "暂停了 Runner mac-1 接单" || texts["login"] != "用 GitHub 登录了 Dashboard" {
		t.Errorf("活动：%v", texts)
	}
	ap = pageData[activityData](t, e.get(e.alice, activityURL+"?actor=bob").Body.String())
	if ap.Actor != "bob" || !slices.Contains(ap.Actors, "bob") || len(ap.Entries) != 2 {
		t.Errorf("按操作者筛选：%+v", ap)
	}
	for _, en := range ap.Entries {
		if en.Actor != "bob" {
			t.Errorf("按 bob 筛选时不应有其他人的活动：%+v", en)
		}
	}
	wantStatus(t, "成员打开活动", e.get(e.bob, activityURL), http.StatusForbidden)
}

func TestOverviewPage(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	active := e.addRun(runOpts{state: store.RunActive})
	e.addJob(jobOpts{runID: active, state: store.JobRunning})
	e.addRun(runOpts{state: store.RunSucceeded})
	failed := e.addRun(runOpts{state: store.RunFailed})
	e.exec(`UPDATE runs SET sender_login='bob' WHERE id=$1`, failed)
	e.addRunner("mac-1", runnerOpts{})

	// 管理员：初始设置待办（还没有绑定），已同步仓库、已注册 Runner 算完成
	// overviewData 是总览的页面数据，另带统计计算的成功率。
	type overviewData struct {
		overviewPage
		Stats struct {
			overviewStats
			Rate string
		}
	}
	ids := func(list []runView) []int64 {
		var out []int64
		for _, v := range list {
			out = append(out, v.ID)
		}
		return out
	}
	p := pageData[overviewData](t, e.get(e.alice, "/").Body.String())
	todo := map[string]bool{}
	for _, s := range p.Setup {
		if !s.Done {
			todo[s.URL] = true
		}
	}
	if len(p.Setup) != 4 || p.SetupDone != 2 || !todo[accountURL] {
		t.Errorf("管理员的初始设置（已完成 2 / 4，待办中有绑定 GitHub 账号）：%+v", p.Setup)
	}
	if !slices.Equal(ids(p.Running), []int64{active}) || !slices.Equal(ids(p.Failures), []int64{failed}) || p.Stats.Rate != "50%" ||
		p.Stats.Total != 3 || len(p.Runners) != 1 || p.Runners[0].Name != "mac-1" || p.RunnerCounts.Total != 1 {
		t.Errorf("管理员的总览：%+v", p)
	}
	// 成员：没有初始设置，有"我最近发起的"
	p = pageData[overviewData](t, e.get(e.bob, "/").Body.String())
	if p.Me != "bob" || p.MineURL != "/runs?actor=bob" || !slices.Equal(ids(p.Mine), []int64{failed}) || len(p.Setup) != 0 {
		t.Errorf("成员的总览：%+v", p)
	}
	// 全部完成后不再显示待办
	e.must(store.PutAdminBinding(e.ctx, e.db, 42, "alice", "alice"))
	_, err := store.SetAdminTokenLogin(e.ctx, e.db, false)
	e.must(err)
	if p := pageData[overviewData](t, e.get(e.alice, "/").Body.String()); len(p.Setup) != 0 {
		t.Errorf("初始设置全部完成后不应再有待办：%+v", p.Setup)
	}
}

func TestRunActivity(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	run := e.addRun(runOpts{state: store.RunActive})
	e.addJob(jobOpts{runID: run, state: store.JobRunning})
	id := strings.TrimPrefix(runURL(run), "/runs/")
	wantRedirect(t, "取消", e.post(e.bob, "/runs/"+id+"/cancel", nil), "/runs/"+id)
	p := pageData[runData](t, e.get(e.alice, "/runs/"+id).Body.String())
	if len(p.Activity) != 1 || p.Activity[0].Actor != "bob" || p.Activity[0].RunText != "取消了这个 run" {
		t.Errorf("run 的活动：%+v", p.Activity)
	}
}

func TestUIShellAndAssets(t *testing.T) {
	_, mux, _ := testDashboard(t, "")
	// 浏览器打开页面地址返回 index.html（没有构建前端时是提示页），不需要登录
	w := serve(mux, testReq{target: "/runs/12", browser: true})
	wantStatus(t, "浏览器打开页面", w, http.StatusOK)
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Content-Type %q，Cache-Control %q", w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
	// 不允许被其他网站嵌入（点击劫持）
	if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Errorf("index.html 应禁止被嵌入：X-Frame-Options %q，CSP %q", w.Header().Get("X-Frame-Options"), w.Header().Get("Content-Security-Policy"))
	}
	// 前端取数据：未登录时跳转到登录页（JSON）
	if got := redirectTarget(t, "前端取数据", serve(mux, testReq{target: "/runs/12"})); got != "/login?next=%2Fruns%2F12" {
		t.Errorf("未登录时跳转到 %q", got)
	}
	// 浏览器打开不存在的地址：404 的 index.html，内嵌页面数据，前端不用再请求一次
	w = serve(mux, testReq{target: "/nope", browser: true})
	wantStatus(t, "不存在的地址", w, http.StatusNotFound)
	mustHave(t, "内嵌的页面数据", w.Body.String(), `<script id="kiln-page" type="application/json">`, `"Page":"error"`, `"Status":404`)
	// 内嵌了页面数据（登录身份、csrf）时不缓存
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("内嵌页面数据的 index.html：Cache-Control %q，期望 no-store", got)
	}
	wantStatus(t, "不存在的构建产物", serve(mux, testReq{target: "/assets/nope.js", browser: true}), http.StatusNotFound)
	wantStatus(t, "不提供 index.html 本身", serve(mux, testReq{target: "/index.html", browser: true}), http.StatusNotFound)
}

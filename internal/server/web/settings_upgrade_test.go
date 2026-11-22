package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Runner 按台升级（PLAN #27.14）使用数据库的功能测试：设置页（registry、全部检查更新）、Runner 详情页（升级、取消、
// 检查更新）的页面数据、权限、校验和审计。

// testRunnerUpgradePage 是设置了 registry 的 Runner 升级页（TestPagesRender）。
func testRunnerUpgradePage(now time.Time) *runnerUpgradePage {
	return &runnerUpgradePage{
		Section: sectionUpgrade, Registry: "https://npm.example/", DefaultRegistry: "https://registry.npmjs.org/",
		Runners: testRunners(now)[:4], Form: failedForm(),
	}
}

// testRunnerPage 是一台正在升级的 Runner 的详情页（TestPagesRender）。
func testRunnerPage(now time.Time) *runnerPage {
	r := testRunners(now)[0]
	return &runnerPage{
		Runner: r, Registry: "https://registry.npmjs.org/", RegistryDefault: true, OfflineAfter: 120, LongPoll: 25,
		History: []*upgradeRequestView{
			r.Upgrade.Request,
			{ID: 1000, Version: "0.1.11", From: "0.1.10", State: "failed", Error: "下载失败：503", Actor: "alice", Created: now.Add(-time.Hour), Finished: &now},
		},
	}
}

// reportUpgrade 写入 Runner 上报的升级状态（protojson）。
func (e *webEnv) reportUpgrade(id int64, version, status string) {
	e.t.Helper()
	e.exec(`UPDATE runners SET version=$2, upgrade=$3 WHERE id=$1`, id, version, status)
}

// 设置页：只有管理员；registry 的校验标在字段旁（422），保存、恢复官方都记审计；全部检查更新增加检查序号。
func TestRunnerUpgradePage(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRunner("mini-01", runnerOpts{})
	e.addRunner("gone", runnerOpts{state: "revoked"})
	// AI 执行节点没有按台升级（连接器用 npm 升级，PLAN #33.3），不列在这个页面
	aiNode := e.addAIRunner("reviewer", store.RunnerRoleReview, nil)

	wantStatus(t, "成员打开 Runner 升级", e.get(e.bob, runnerUpgradeURL), http.StatusForbidden)
	wantStatus(t, "成员修改 registry", e.post(e.bob, runnerUpgradeURL+"/registry", url.Values{"registry": {"https://npm.example/"}}), http.StatusForbidden)

	w := e.get(e.alice, runnerUpgradeURL)
	wantStatus(t, "打开 Runner 升级", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "settings_upgrade" || pg.Nav != "settings" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[runnerUpgradePage](t, w.Body.String())
	expect(t, "初始页面", p, p.Section == sectionUpgrade, p.Registry == "", p.DefaultRegistry == "https://registry.npmjs.org/",
		len(p.Runners) == 1 && p.Runners[0].Name == "mini-01", p.Runners[0].Upgrade != nil, p.Form != nil,
		p.Runners[0].Kind == store.RunnerKindCI && p.Runners[0].URL == runnerURL("mini-01"),
		// AI 执行节点不在这个页面
		aiNode != 0, !slices.ContainsFunc(p.Runners, func(r *runnerView) bool { return r.Name == "reviewer" }))

	for _, c := range []struct{ what, registry, want string }{
		{"http 的公网地址", "http://npm.example/", "必须是 https"},
		{"带用户信息", "https://u:p@npm.example/", "用户信息"},
	} {
		w := e.post(e.alice, runnerUpgradeURL+"/registry", url.Values{"registry": {c.registry}})
		wantStatus(t, c.what, w, http.StatusUnprocessableEntity)
		f := pageData[runnerUpgradePage](t, w.Body.String()).Form
		if f == nil || !strings.Contains(textZH(t, f.Errors["registry"]), c.want) || f.Values["registry"] != c.registry {
			t.Errorf("%s：表单 = %+v", c.what, f)
		}
	}
	flash := wantRedirect(t, "保存 registry", e.post(e.alice, runnerUpgradeURL+"/registry", url.Values{"registry": {" https://npm.example/ "}}), runnerUpgradeURL)
	if p := pageData[runnerUpgradePage](t, e.get(e.alice, runnerUpgradeURL).Body.String()); p.Registry != "https://npm.example/" ||
		!strings.Contains(flash, "https://npm.example/") {
		t.Errorf("保存 registry：%q %+v", flash, p.Registry)
	}
	flash = wantRedirect(t, "恢复官方", e.post(e.alice, runnerUpgradeURL+"/registry", url.Values{"registry": {""}}), runnerUpgradeURL)
	if !strings.Contains(flash, "官方 registry") || e.count(`SELECT count(*) FROM kv WHERE key='runner_upgrade.registry'`) != 0 {
		t.Errorf("恢复官方：%q", flash)
	}
	if a := e.audits("runner.upgrade.registry"); len(a) != 2 || a[0].Actor != "alice" {
		t.Errorf("registry 的审计：%+v", a)
	}

	flash = wantRedirect(t, "全部检查更新", e.post(e.alice, runnerUpgradeURL+"/check", nil), runnerUpgradeURL)
	// CI Runner 和 AI 执行节点都检查
	if !strings.Contains(flash, "已请求 2 台") || e.count(`SELECT count(*) FROM runners WHERE upgrade_check=1`) != 2 {
		t.Errorf("全部检查更新（不含已吊销的）：%q", flash)
	}
}

// Runner 详情页：任何登录用户可以查看；管理员按台升级到 Runner 查到的最新版本、重复点击不重复请求、取消、检查更新；
// 不能升级时给出原因；成员不能操作。
func TestRunnerDetail(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id := e.addRunner("mini-01", runnerOpts{})
	old := e.addRunner("old", runnerOpts{})
	e.reportUpgrade(id, "v0.1.3", `{"state":"idle","launcher":true,"package":"@yuebanlaosiji/kilnflow-runner-linux-amd64","latest":"0.1.9","checkedAt":"1700000000"}`)
	e.reportUpgrade(old, "v0.1.3", `{"state":"idle","latest":"0.1.9"}`)

	wantStatus(t, "不存在的 Runner", e.get(e.bob, "/runners/detail/nope"), http.StatusNotFound)
	w := e.get(e.bob, "/runners/detail/mini-01")
	wantStatus(t, "成员查看详情", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "runner" || pg.Nav != "runners" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[runnerPage](t, w.Body.String())
	u := p.Runner.Upgrade
	expect(t, "详情页", p, p.Runner.Name == "mini-01", p.RegistryDefault, p.Registry == "https://registry.npmjs.org/", len(p.History) == 0,
		u.Latest == "0.1.9", u.Package == "@yuebanlaosiji/kilnflow-runner-linux-amd64", u.CheckedAt != nil, u.CanUpgrade, u.Request == nil)
	wantStatus(t, "成员升级", e.post(e.bob, "/runners/upgrade/mini-01/start", nil), http.StatusForbidden)

	// 页面确认的是 0.1.8，期间 Runner 查到了 0.1.9：拒绝，不创建请求
	flash := flashZH(t, wantRedirect(t, "确认之后 latest 变了", e.post(e.alice, "/runners/upgrade/mini-01/start", url.Values{"version": {"0.1.8"}}), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "最新版本是 0.1.9") || e.count(`SELECT count(*) FROM runner_upgrade_requests`) != 0 {
		t.Errorf("确认的版本与 latest 不同时应拒绝：%q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "升级", e.post(e.alice, "/runners/upgrade/mini-01/start", url.Values{"version": {"0.1.9"}}), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "升级到 0.1.9") || !strings.Contains(flash, "离线") {
		t.Errorf("升级的提示（这台测试 Runner 离线）：%q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "重复点击", e.post(e.alice, "/runners/upgrade/mini-01/start", nil), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "已经在升级到 0.1.9") || e.count(`SELECT count(*) FROM runner_upgrade_requests`) != 1 {
		t.Errorf("重复点击：%q", flash)
	}
	p = pageData[runnerPage](t, e.get(e.alice, "/runners/detail/mini-01").Body.String())
	u = p.Runner.Upgrade
	expect(t, "进行中", p, len(p.History) == 1, u.Request != nil && u.Request.State == "pending" && u.Request.Version == "0.1.9" && u.Request.Actor == "alice",
		!u.CanUpgrade, u.Reason.id == msgUpgradeReasonUpgrading && textZH(t, u.Reason) == "正在升级到 0.1.9")

	flash = flashZH(t, wantRedirect(t, "取消", e.post(e.alice, "/runners/upgrade/mini-01/cancel", nil), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "已取消") {
		t.Errorf("取消：%q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "再取消", e.post(e.alice, "/runners/upgrade/mini-01/cancel", nil), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "没有进行中的升级请求") {
		t.Errorf("没有进行中的请求时：%q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "检查更新", e.post(e.alice, "/runners/upgrade/mini-01/check", nil), "/runners/detail/mini-01"))
	if !strings.Contains(flash, "查询最新版本") {
		t.Errorf("检查更新：%q", flash)
	}
	if p := pageData[runnerPage](t, e.get(e.alice, "/runners/detail/mini-01").Body.String()); !p.Runner.Upgrade.CheckPending {
		t.Errorf("请求检查后应显示检查中：%+v", p.Runner.Upgrade)
	}
	// 不经过启动器运行：给出处理步骤，不创建请求
	flash = flashZH(t, wantRedirect(t, "不能升级", e.post(e.alice, "/runners/upgrade/old/start", nil), "/runners/detail/old"))
	if !strings.Contains(flash, "不能自动切换") || !strings.Contains(flash, "kiln-runner service install") {
		t.Errorf("不能升级时的原因：%q", flash)
	}
	wantStatus(t, "未知操作", e.post(e.alice, "/runners/upgrade/mini-01/bogus", nil), http.StatusNotFound)
	for _, action := range []string{"runner.upgrade.requested", "runner.upgrade.cancelled", "runner.upgrade.check"} {
		if a := e.audits(action); len(a) != 1 || a[0].Actor != "alice" {
			t.Errorf("%s 的审计：%+v", action, a)
		}
	}
}

// Runners 页面的升级状态来自 Runner 上报的状态：本地关闭、不经过启动器分开显示，没有上报的 State 为空。
func TestRunnersUpgradeState(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id := e.addRunner("mini-01", runnerOpts{})
	e.addRunner("old", runnerOpts{})
	e.reportUpgrade(id, "v0.1.3",
		`{"state":"rolled_back","target":"0.1.9","error":"连续 3 次启动失败","launcher":true,"disabled":true,"current":"0.1.3","previous":"0.1.2","latest":"0.1.9"}`)
	p := pageData[runnersPage](t, e.get(e.bob, "/runners").Body.String())
	byName := map[string]*runnerUpgradeView{}
	for _, r := range p.Runners {
		byName[r.Name] = r.Upgrade
	}
	m, old := byName["mini-01"], byName["old"]
	if m == nil || old == nil {
		t.Fatalf("应有升级状态：%+v", byName)
	}
	expect(t, "上报了状态的 Runner", m, m.State == "rolled_back", m.Target == "0.1.9", m.Error == "连续 3 次启动失败", m.Launcher, m.Disabled,
		m.Current == "0.1.3", m.Previous == "0.1.2", !m.CanUpgrade, m.Reason.id == msgUpgradeReasonDisabled && strings.Contains(textZH(t, m.Reason), "upgrade = false"))
	expect(t, "没有上报的 Runner", old, old.State == "", !old.Launcher, !old.CanUpgrade,
		old.Reason.id == msgUpgradeReasonUnsupported && strings.Contains(textZH(t, old.Reason), "kiln-runner service install"))
}

// 名称与其他页面的地址相同的 Runner（new、cache、enroll 等都是合法名称）也能进入详情页并按台升级：
// 详情页在 /runners/detail/ 下，列表和升级操作的回跳地址都用 runnerURL。
func TestRunnerDetailReservedNames(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	for _, name := range []string{"new", "cache", "enroll", "upgrade", "detail", "token"} {
		id := e.addRunner(name, runnerOpts{})
		e.reportUpgrade(id, "v0.1.3", `{"state":"idle","launcher":true,"latest":"0.1.9"}`)
		w := e.get(e.alice, runnerURL(name))
		wantStatus(t, name+" 的详情页", w, http.StatusOK)
		if pg := parsePage(t, w.Body.String()); pg.Page != "runner" || pageData[runnerPage](t, w.Body.String()).Runner.Name != name {
			t.Errorf("%s：页面 = %q", name, pg.Page)
		}
		flash := flashZH(t, wantRedirect(t, name+" 升级", e.post(e.alice, "/runners/upgrade/"+name+"/start", nil), runnerURL(name)))
		if !strings.Contains(flash, "升级到 0.1.9") {
			t.Errorf("%s 升级：%q", name, flash)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_upgrade_requests WHERE state='pending'`); n != 6 {
		t.Errorf("pending 请求 = %d，期望 6", n)
	}
}

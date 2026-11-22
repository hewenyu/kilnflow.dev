package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/npm"
)

// 账号级 npm 配置（连接与发布凭据中的 npm 卡片和编辑页 /settings/npm）使用数据库的功能测试。

// npmPageURL 是 tester 的 npm 配置编辑页。
const npmPageURL = "/settings/npm?account=tester"

// npmData 是页面数据中的 npm 配置（npmView）。
type npmData struct {
	Account string
	Config  *struct {
		Scope, Registry, UpdatedBy string
		UpdatedAt                  time.Time
	}
	URL string
}

// npmPageData 是 npm 配置编辑页的页面数据（npmPage）。
type npmPageData struct {
	Section         string
	Npm             npmData
	Form            *formData
	DefaultRegistry string
}

// npmVersion 返回账号的 npm 配置的版本，没有配置时返回 0。
func (e *webEnv) npmVersion() int64 {
	e.t.Helper()
	var v int64
	if err := e.db.QueryRow(e.ctx, `SELECT coalesce((SELECT version FROM npm_accounts WHERE owner_id=$1), 0)`, testAccountID).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// npmTestToken 是测试中保存的 npm token（页面、审计和错误信息中都不应出现）。
const npmTestToken = "npm_s3cret-token-123"

// postNpm 以 alice 的身份为 tester 提交 npm 表单。
func (e *webEnv) postNpm(form url.Values) *httptest.ResponseRecorder {
	form.Set("account", testAccount)
	return e.post(e.alice, "/settings/npm", form)
}

// 校验失败：字段旁标出错误，保留 scope 和 registry，token 不回显，不保存。
func TestNpmSettingsValidation(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	const token = npmTestToken

	// 没有配置：编辑页在连接与发布凭据分区，表单为空，registry 预填 npm 官方 registry
	p := pageData[npmPageData](t, e.get(e.alice, npmPageURL).Body.String())
	if p.Section != sectionConn || p.Npm.Account != testAccount || p.Npm.Config != nil || p.Npm.URL != npmPageURL ||
		p.Form == nil || p.Form.Values["registry"] != npm.DefaultRegistry || p.Form.Failed || p.DefaultRegistry != npm.DefaultRegistry {
		t.Errorf("没有配置时：%+v %+v", p, p.Form)
	}
	for _, c := range []struct {
		what   string
		form   url.Values
		field  string
		want   []string
		values map[string]string
	}{
		{"scope 为空", url.Values{"scope": {" "}, "token": {token}}, "scope", []string{"请填写 npm scope", "刚才输入的 token 没有保留"}, nil},
		{"scope 有大写字母", url.Values{"scope": {"Acme"}, "token": {token}}, "scope", []string{"不能有大写字母"}, map[string]string{"scope": "Acme"}},
		{
			"registry 不是 https",
			url.Values{"scope": {"acme"}, "registry": {"http://npm.example.com/"}, "token": {token}},
			"registry",
			[]string{"必须是 https"},
			map[string]string{"scope": "@acme", "registry": "http://npm.example.com/"},
		},
		{"token 为空", url.Values{"scope": {"@acme"}, "token": {"  "}}, "token", []string{"请填写 npm token"}, map[string]string{"registry": npm.DefaultRegistry}},
	} {
		w := e.postNpm(c.form)
		wantStatus(t, c.what, w, http.StatusUnprocessableEntity)
		body := w.Body.String()
		mustHave(t, c.what, body, c.want...)
		if strings.Contains(body, token) {
			t.Errorf("%s：校验失败的页面不应回显 token", c.what)
		}
		if pg := parsePage(t, body); pg.Page != "settings_npm" {
			t.Errorf("%s：应重新渲染 npm 编辑页，实际 %s", c.what, pg.Page)
		}
		p := pageData[npmPageData](t, body)
		if p.Form == nil || !p.Form.Failed || p.Form.Errors[c.field] == "" || p.Form.Values["token"] != "" {
			t.Errorf("%s：表单 %+v", c.what, p.Form)
			continue
		}
		for k, v := range c.values {
			if p.Form.Values[k] != v {
				t.Errorf("%s：表单值 %s = %q，应为 %q", c.what, k, p.Form.Values[k], v)
			}
		}
	}
	if v := e.npmVersion(); v != 0 {
		t.Errorf("校验失败不应保存：版本 %d", v)
	}

	// 账号无效：回到连接与发布凭据并说明，不保存
	flash := wantRedirect(t, "账号无效", e.post(e.alice, "/settings/npm", url.Values{"account": {"ghost"}, "scope": {"@acme"}, "token": {token}}), "/settings")
	mustHave(t, "账号无效", flash, "账号不在 Kiln 中")
	if n := e.count(`SELECT count(*) FROM npm_accounts`); n != 0 {
		t.Errorf("账号无效时不应保存：%d", n)
	}
	flash = wantRedirect(t, "打开无效账号", e.get(e.alice, "/settings/npm?account=ghost"), "/settings")
	mustHave(t, "打开无效账号", flash, "账号不在 Kiln 中")
}

// 保存、替换和删除账号的 npm 配置。
func TestNpmSettings(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.upsertEnv(testAccount, "", "release")
	const token = npmTestToken
	post := e.postNpm

	// 保存：scope 补上 @，registry 留空时用 npm 官方 registry，token 加密保存
	flash := wantRedirect(t, "保存", post(url.Values{"scope": {"acme"}, "registry": {""}, "token": {token}}), npmPageURL)
	mustHave(t, "提示", flash, "已保存 npm 配置：release.npm 发布到 @acme/<name>")
	if n := e.count(`SELECT count(*) FROM npm_accounts WHERE owner_id=$1 AND position($2::bytea in ciphertext) = 0`, testAccountID, []byte(token)); n != 1 {
		t.Error("npm token 应加密保存")
	}
	v1 := e.npmVersion()
	body := e.get(e.alice, npmPageURL).Body.String()
	p := pageData[npmPageData](t, body)
	if c := p.Npm.Config; c == nil || c.Scope != "@acme" || c.Registry != npm.DefaultRegistry || c.UpdatedBy != "alice" || c.UpdatedAt.IsZero() {
		t.Errorf("保存后的 npm 配置：%+v", c)
	}
	if p.Form == nil || p.Form.Values["scope"] != "@acme" || p.Form.Values["registry"] != npm.DefaultRegistry {
		t.Errorf("替换表单应预填当前的 scope 和 registry：%+v", p.Form)
	}
	if strings.Contains(body, token) {
		t.Error("页面不应显示 npm token")
	}

	// 替换：私有 registry 补上结尾的 /，版本改变
	const private = "https://npm.example.com/api/npm/repo"
	wantRedirect(t, "替换", post(url.Values{"scope": {"@acme"}, "registry": {private}, "token": {"other-token"}}), npmPageURL)
	p = pageData[npmPageData](t, e.get(e.alice, npmPageURL).Body.String())
	if p.Npm.Config == nil || p.Npm.Config.Registry != private+"/" || e.npmVersion() == v1 {
		t.Errorf("替换后：%+v，版本 %d → %d", p.Npm.Config, v1, e.npmVersion())
	}

	// 删除：要确认；删除后回到连接与发布凭据；再删除说明没有配置
	flash = wantRedirect(t, "未确认的删除", e.post(e.alice, "/settings/npm/delete", url.Values{"account": {testAccount}}), npmPageURL)
	if flash != "没有删除：请先确认" || e.npmVersion() == 0 {
		t.Errorf("未确认的删除：提示 %q", flash)
	}
	del := url.Values{"account": {testAccount}, "confirm": {"1"}}
	flash = wantRedirect(t, "删除", e.post(e.alice, "/settings/npm/delete", del), "/settings")
	if flash != "已删除 tester 的 npm 配置；重新配置之前 release.npm 无法发布" || e.npmVersion() != 0 {
		t.Errorf("删除：提示 %q", flash)
	}
	flash = wantRedirect(t, "再次删除", e.post(e.alice, "/settings/npm/delete", del), npmPageURL)
	mustHave(t, "再次删除", flash, "删除 npm 配置失败", "没有配置 npm")

	// 审计：操作者是登录用户，不含 token
	sets, dels := e.audits("config.npm.set"), e.audits("config.npm.delete")
	if len(sets) != 2 || len(dels) != 1 {
		t.Errorf("审计：npm.set %d 条、npm.delete %d 条，应为 2、1", len(sets), len(dels))
	}
	for _, a := range append(sets, dels...) {
		if a.Actor != "alice" || strings.Contains(fmt.Sprint(a.Detail), token) || strings.Contains(fmt.Sprint(a.Detail), "other-token") {
			t.Errorf("npm 配置的审计应记录 alice、不含 token：%+v", a)
		}
	}
}

// npm 配置与 Docker Hub 平级：连接与发布凭据中每个账号一张 npm 卡片（从不包含 token），作用域配置页没有 npm。
func TestNpmSettingsPlacement(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.upsertEnv(testAccount, "", "release")

	conn := pageData[struct{ Npm []npmData }](t, e.get(e.alice, "/settings").Body.String())
	if len(conn.Npm) != 1 || conn.Npm[0].Account != testAccount || conn.Npm[0].Config != nil || conn.Npm[0].URL != npmPageURL {
		t.Errorf("没有配置时的 npm 卡片：%+v", conn.Npm)
	}
	wantRedirect(t, "保存", e.postNpm(url.Values{"scope": {"acme"}, "token": {npmTestToken}}), npmPageURL)
	body := e.get(e.alice, "/settings").Body.String()
	conn = pageData[struct{ Npm []npmData }](t, body)
	if len(conn.Npm) != 1 || conn.Npm[0].Config == nil || conn.Npm[0].Config.Scope != "@acme" {
		t.Errorf("保存后的 npm 卡片：%+v", conn.Npm)
	}
	if strings.Contains(body, npmTestToken) {
		t.Error("连接与发布凭据不应显示 npm token")
	}

	// 作用域配置页（账号级、仓库级、environment 级）都不带 npm 配置
	for _, target := range []string{accountScopeURL, repoScopeURL, "/settings/scope?account=tester&env=release"} {
		if p := pageData[map[string]any](t, e.get(e.alice, target).Body.String()); p["Npm"] != nil || p["NpmForm"] != nil {
			t.Errorf("%s 不应带 npm 配置：%v", target, p["Npm"])
		}
	}
}

func TestNpmPackageURL(t *testing.T) {
	t.Parallel()
	detail := func(name, version, tarball string) map[string]any {
		return map[string]any{"package": name, "version": version, "tarball": tarball}
	}
	for _, c := range []struct {
		what   string
		detail map[string]any
		want   string
	}{
		{
			"npm 官方 registry", detail("@acme/cli", "1.2.0", "https://registry.npmjs.org/@acme/cli/-/cli-1.2.0.tgz"),
			"https://www.npmjs.com/package/@acme/cli/v/1.2.0",
		},
		{
			"预发布版本", detail("@acme/cli", "2.0.0-rc.1", "https://registry.npmjs.org/@acme/cli/-/cli-2.0.0-rc.1.tgz"),
			"https://www.npmjs.com/package/@acme/cli/v/2.0.0-rc.1",
		},
		{"其他 registry", detail("@acme/cli", "1.2.0", "https://npm.pkg.github.com/@acme/cli/-/cli-1.2.0.tgz"), ""},
		{"不是 https", detail("@acme/cli", "1.2.0", "http://registry.npmjs.org/@acme/cli/-/cli-1.2.0.tgz"), ""},
		{"包名无效", detail("@acme/<x>", "1.2.0", "https://registry.npmjs.org/x.tgz"), ""},
		{"没有版本", detail("@acme/cli", "", "https://registry.npmjs.org/x.tgz"), ""},
		{"没有结果", nil, ""},
	} {
		if got := npmPackageURL(c.detail); got != c.want {
			t.Errorf("%s：npmPackageURL = %q，应为 %q", c.what, got, c.want)
		}
	}
	// 只有已发布的 npm 目标才有链接
	d := detail("@acme/cli", "1.2.0", "https://registry.npmjs.org/@acme/cli/-/cli-1.2.0.tgz")
	for _, c := range []struct {
		t    releaseTargetView
		want bool
	}{
		{releaseTargetView{Kind: "npm", State: "committed", Detail: d}, true},
		{releaseTargetView{Kind: "npm", State: "staged", Detail: d}, false},
		{releaseTargetView{Kind: "upload", State: "committed", Detail: d}, false},
	} {
		if got := c.t.URL() != ""; got != c.want {
			t.Errorf("%s/%s 的 URL = %q", c.t.Kind, c.t.State, c.t.URL())
		}
	}
}

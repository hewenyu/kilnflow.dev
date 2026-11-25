package web

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 设置页（连接、变量与密钥、环境、仓库、运维）使用数据库的功能测试。

// 作用域配置页的地址（与 scopeURL 相同，查询参数按名称排序）。
const (
	accountScopeURL = "/settings/scope?account=tester"
	repoScopeURL    = "/settings/scope?repo=tester%2Fdemo"
	dockerPageURL   = "/settings/docker?account=tester"
)

// varValue 返回名为 name 的变量的值（按作用域种类 kind 查找），不存在时返回 "<none>"。
func (e *webEnv) varValue(kind store.ScopeKind, name string) string {
	e.t.Helper()
	var v string
	err := e.db.QueryRow(e.ctx, `SELECT coalesce((SELECT value FROM variables WHERE scope_kind=$1 AND name=$2), '<none>')`, kind, name).Scan(&v)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// secretVersion 返回名为 name 的 secret 的版本，不存在时返回 0。
func (e *webEnv) secretVersion(name string) int64 {
	e.t.Helper()
	var v int64
	if err := e.db.QueryRow(e.ctx, `SELECT coalesce((SELECT version FROM secrets WHERE name=$1), 0)`, name).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// configAudits 返回 config.<op> 的审计日志的操作者（按写入顺序）。
func (e *webEnv) configActors(op string) []string {
	e.t.Helper()
	var actors []string
	for _, a := range e.audits("config." + op) {
		actors = append(actors, a.Actor)
	}
	return actors
}

// upsertEnv 直接通过 admin.Service 创建或修改 environment（准备测试数据，操作者为 setup）。
func (e *webEnv) upsertEnv(account, repo, name string, refPolicy ...string) {
	e.t.Helper()
	e.must(e.svc.UpsertEnvironment(e.ctx, &apiv1.Environment{Account: account, Repo: repo, Name: name, RefPolicy: refPolicy}, "setup"))
}

// scopeProto 返回 admin.Service 使用的作用域。
func scopeProto(account, repo, env string) *apiv1.Scope {
	return &apiv1.Scope{Account: account, Repo: repo, Environment: env}
}

// formData 是页面数据中的表单状态（formState），另带计算的 Failed。
type formData struct {
	Values map[string]string
	Errors map[string]uiText
	Failed bool
}

// envData 是页面数据中的 environment（envView）：内嵌的 apiv1.Environment 按它的 json 标签编码（小写、下划线）。
type envData struct {
	Name                string   `json:"name"`
	Account             string   `json:"account"`
	Repo                string   `json:"repo"`
	RefPolicy           []string `json:"ref_policy"`
	AllowedPublishRepos []string `json:"allowed_publish_repos"`
	OverriddenIn        []string
	Overrides, Docker   bool
	URL                 string
}

// scopeData 是作用域配置页的页面数据（scopePage），另带计算的 URL、Query。
type scopeData struct {
	Account, Repo, Env, Label string
	Kind                      store.ScopeKind
	Environment               *envData
	Vars                      []store.Variable
	Secrets                   []store.SecretMeta
	EnvForm                   *formData
	URL, Query                string
}

// itemData 是变量或 secret 编辑页的页面数据（itemPage）。
type itemData struct {
	scopeData
	Kind      itemKind
	ScopeKind store.ScopeKind
	Name      string
	Secret    *store.SecretMeta
	Form      *formData
	IsSecret  bool
}

// noLeak 检查响应（页面数据、跳转地址和其中的提示）中没有提交的 secret 或令牌（包括 URL 编码后的形式）。
// 太短的值（少于 8 个字符）容易与页面上的其他文字重合，不检查。
func noLeak(t *testing.T, what string, w *httptest.ResponseRecorder, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		for _, v := range []string{s, url.QueryEscape(s)} {
			if strings.Contains(w.Body.String(), v) || strings.Contains(w.Header().Get("Location"), v) {
				t.Errorf("%s：响应中有提交的 secret 或令牌", what)
			}
		}
	}
}

func TestSettings_AdminOnly(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.upsertEnv(testAccount, "", "release")
	for _, target := range []string{
		"/settings", dockerPageURL, npmPageURL, "/settings/variables", accountScopeURL, repoScopeURL,
		"/settings/scope?account=tester&env=release", "/settings/variables/new?account=tester", "/settings/secrets/new?account=tester",
		"/settings/environments", "/settings/environments/new", "/settings/repos", "/settings/ops",
	} {
		wantStatus(t, "管理员打开 "+target, e.get(e.alice, target), http.StatusOK)
		w := e.get(e.bob, target)
		wantStatus(t, "成员打开 "+target, w, http.StatusForbidden)
		mustHave(t, "成员打开 "+target, pageTextZH(t, w.Body.String()), "需要管理员权限")
	}
	// 成员的写请求一律拒绝，不产生任何修改
	for _, target := range []string{
		"/settings/variables", "/settings/variables/delete", "/settings/secrets", "/settings/secrets/delete",
		"/settings/environments", "/settings/environments/delete", "/settings/docker", "/settings/docker/delete",
		"/settings/npm", "/settings/npm/delete",
		"/sync", "/settings/repos/reconcile", "/outbox/1/retry",
	} {
		form := url.Values{
			"account": {testAccount}, "name": {"X"}, "value": {"v"}, "mode": {"new"}, "create": {"1"}, "scope": {"account:tester"},
			"env": {"release"}, "confirm": {"1"}, "username": {"u"}, "namespace": {"ns"}, "token": {"t"},
		}
		wantStatus(t, "成员提交 "+target, e.post(e.bob, target, form), http.StatusForbidden)
	}
	if n := e.count(`SELECT count(*) FROM variables`) + e.count(`SELECT count(*) FROM secrets`) + e.count(`SELECT count(*) FROM npm_accounts`); n != 0 {
		t.Errorf("成员的写请求不应产生修改：%d 个变量、secret 和 npm 配置", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE actor='bob'`); n != 0 {
		t.Errorf("成员的写请求不应写审计：%d", n)
	}
}

func TestPostVariable(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	post := func(form url.Values) *httptest.ResponseRecorder { return e.post(e.alice, "/settings/variables", form) }

	// 新建
	flash := wantRedirect(t, "新建变量", post(url.Values{"account": {testAccount}, "mode": {"new"}, "name": {"FOO"}, "value": {"bar"}}), accountScopeURL)
	if flashZH(t, flash) != "已创建变量 FOO。" || e.varValue(store.ScopeOwner, "FOO") != "bar" {
		t.Fatalf("新建变量：提示 %q，值 %q", flash, e.varValue(store.ScopeOwner, "FOO"))
	}
	sp := pageData[scopeData](t, e.get(e.alice, accountScopeURL).Body.String())
	if sp.Kind != store.ScopeOwner || !slices.Equal(sp.Vars, []store.Variable{{Name: "FOO", Value: "bar"}}) || sp.URL != accountScopeURL || sp.Query != "account=tester" {
		t.Errorf("账号级配置页：%+v", sp)
	}

	// 校验失败：422，保留输入，不修改
	for _, c := range []struct {
		name string
		form url.Values
		want []string
		// values 是保留的输入
		values map[string]string
	}{
		{"新建同名", url.Values{"mode": {"new"}, "name": {"FOO"}, "value": {"other"}}, []string{"变量 FOO 已存在"}, map[string]string{"value": "other"}},
		{"名称不合法", url.Values{"mode": {"new"}, "name": {"1-bad"}, "value": {"keep me"}}, []string{"名称 1-bad 只能包含字母、数字和 _"}, map[string]string{"name": "1-bad", "value": "keep me"}},
		{"名称为空", url.Values{"mode": {"new"}, "name": {" "}, "value": {"v"}}, []string{"名称不能为空：只能包含字母、数字和 _"}, nil},
	} {
		c.form.Set("account", testAccount)
		w := post(c.form)
		wantStatus(t, c.name, w, http.StatusUnprocessableEntity)
		mustHave(t, c.name, bodyZH(t, w.Body.String()), c.want...)
		p := pageData[itemData](t, w.Body.String())
		if p.Form == nil || !p.Form.Failed || p.Kind != kindVariables || p.IsSecret {
			t.Errorf("%s：%+v", c.name, p.Form)
			continue
		}
		for k, v := range c.values {
			if p.Form.Values[k] != v {
				t.Errorf("%s：应保留 %s 的输入 %q：%q", c.name, k, v, p.Form.Values[k])
			}
		}
	}
	if e.varValue(store.ScopeOwner, "FOO") != "bar" || e.count(`SELECT count(*) FROM variables`) != 1 {
		t.Error("校验失败时不应修改变量")
	}

	// 编辑页预填当前的值；编辑后保存
	if p := pageData[itemData](t, e.get(e.alice, "/settings/variables/edit?account=tester&name=FOO").Body.String()); p.Name != "FOO" || p.Kind != kindVariables || p.ScopeKind != store.ScopeOwner ||
		p.Form == nil || p.Form.Values["value"] != "bar" || p.Account != testAccount {
		t.Errorf("编辑页（Name 非空表示编辑）：%+v", p)
	}
	flash = wantRedirect(t, "编辑变量", post(url.Values{"account": {testAccount}, "mode": {"edit"}, "name": {"FOO"}, "value": {"baz"}}), accountScopeURL)
	if flashZH(t, flash) != "已保存变量 FOO。" || e.varValue(store.ScopeOwner, "FOO") != "baz" {
		t.Errorf("编辑变量：提示 %q，值 %q", flash, e.varValue(store.ScopeOwner, "FOO"))
	}
	// 编辑不存在的变量、地址中没有名称
	flash = wantRedirect(t, "编辑不存在的变量", e.get(e.alice, "/settings/variables/edit?account=tester&name=NOPE"), accountScopeURL)
	mustHave(t, "提示", flashZH(t, flash), "变量 NOPE 不存在（可能已被删除）")
	wantStatus(t, "地址中没有名称", e.get(e.alice, "/settings/variables/edit?account=tester"), http.StatusNotFound)

	// 仓库级和 environment 级：跳回各自的配置页
	wantRedirect(t, "仓库级变量", post(url.Values{"repo": {testRepo}, "mode": {"new"}, "name": {"FOO"}, "value": {"repo"}}), repoScopeURL)
	e.upsertEnv(testAccount, "", "release")
	wantRedirect(t, "environment 级变量", post(url.Values{"account": {testAccount}, "env": {"release"}, "mode": {"new"}, "name": {"FOO"}, "value": {"env"}}),
		"/settings/scope?account=tester&env=release")
	if e.varValue(store.ScopeRepo, "FOO") != "repo" || e.varValue(store.ScopeEnvironment, "FOO") != "env" {
		t.Error("应分别保存在仓库级和 environment 级")
	}
	// 账号不存在：回到变量与密钥分区并说明
	flash = wantRedirect(t, "账号不存在", post(url.Values{"account": {"ghost"}, "mode": {"new"}, "name": {"X"}, "value": {"v"}}), "/settings/variables")
	mustHave(t, "提示", flashZH(t, flash), "账号 ghost 不在 Kiln 中")

	// 删除
	flash = wantRedirect(t, "删除变量", e.post(e.alice, "/settings/variables/delete", url.Values{"account": {testAccount}, "name": {"FOO"}}), accountScopeURL)
	if flashZH(t, flash) != "已删除变量 FOO。" || e.varValue(store.ScopeOwner, "FOO") != "<none>" {
		t.Errorf("删除变量：提示 %q", flash)
	}
	// 删除在不存在的作用域中：说明失败
	flash = wantRedirect(t, "在不存在的仓库中删除", e.post(e.alice, "/settings/variables/delete", url.Values{"repo": {"tester/nope"}, "name": {"FOO"}}),
		"/settings/scope?repo=tester%2Fnope")
	mustHave(t, "提示", flashZH(t, flash), "仓库 tester/nope 不在 Kiln 中")

	// 审计记录登录的用户
	if got := e.configActors("variable.set"); fmt.Sprint(got) != "[alice alice alice alice]" {
		t.Errorf("variable.set 的审计操作者 = %v", got)
	}
	if got := e.configActors("variable.delete"); fmt.Sprint(got) != "[alice]" {
		t.Errorf("variable.delete 的审计操作者 = %v", got)
	}
}

func TestPostSecret(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	const value = "s3cret-value-123"
	post := func(form url.Values) *httptest.ResponseRecorder {
		form.Set("account", testAccount)
		w := e.post(e.alice, "/settings/secrets", form)
		noLeak(t, "提交 secret", w, form.Get("value"))
		if w.Code == http.StatusUnprocessableEntity {
			if p := pageData[itemData](t, w.Body.String()); p.Form == nil || p.Form.Values["value"] != "" {
				t.Errorf("校验失败的表单不应保留 secret 的值：%+v", p.Form)
			}
		}
		return w
	}

	// 新建时值不能为空；其他字段出错时值不保留
	w := post(url.Values{"mode": {"new"}, "name": {"TOKEN"}, "value": {"  "}})
	wantStatus(t, "值为空", w, http.StatusUnprocessableEntity)
	mustHave(t, "值为空", bodyZH(t, w.Body.String()), "请填写 secret 的值")
	if p := pageData[itemData](t, w.Body.String()); p.Form == nil || p.Form.Values["name"] != "TOKEN" || !p.IsSecret {
		t.Errorf("值为空应保留名称：%+v", p.Form)
	}
	w = post(url.Values{"mode": {"new"}, "name": {"bad-name"}, "value": {value}})
	wantStatus(t, "名称不合法", w, http.StatusUnprocessableEntity)
	mustHave(t, "名称不合法", bodyZH(t, w.Body.String()), "名称 bad-name 只能包含字母、数字和 _", "为了安全，刚才输入的值没有保留，请重新输入")
	if strings.Contains(w.Body.String(), value) {
		t.Error("校验失败的页面不应回显 secret 的值")
	}

	// 新建：值加密保存，页面数据中只有名称
	flash := wantRedirect(t, "新建 secret", post(url.Values{"mode": {"new"}, "name": {"TOKEN"}, "value": {value}}), accountScopeURL)
	mustHave(t, "提示", flashZH(t, flash), "已保存 secret TOKEN（值不会再显示）")
	if n := e.count(`SELECT count(*) FROM secrets WHERE name='TOKEN' AND position($1::bytea in ciphertext) = 0`, []byte(value)); n != 1 {
		t.Error("secret 应加密保存")
	}
	v1 := e.secretVersion("TOKEN")
	body := e.get(e.alice, accountScopeURL).Body.String()
	if sp := pageData[scopeData](t, body); len(sp.Secrets) != 1 || sp.Secrets[0].Name != "TOKEN" || sp.Secrets[0].UpdatedBy != "alice" || sp.Secrets[0].Version != v1 {
		t.Errorf("配置页的 secret：%+v", sp.Secrets)
	}
	edit := e.get(e.alice, "/settings/secrets/edit?account=tester&name=TOKEN").Body.String()
	if p := pageData[itemData](t, edit); !p.IsSecret || p.Name != "TOKEN" || p.Secret == nil || p.Secret.UpdatedBy != "alice" || p.Form == nil || p.Form.Values["value"] != "" {
		t.Errorf("替换页：%+v", p)
	}
	if strings.Contains(body, value) || strings.Contains(edit, value) {
		t.Error("页面不应显示 secret 的值")
	}
	w = post(url.Values{"mode": {"new"}, "name": {"TOKEN"}, "value": {"x"}})
	wantStatus(t, "新建同名", w, http.StatusUnprocessableEntity)
	mustHave(t, "新建同名", bodyZH(t, w.Body.String()), "secret TOKEN 已存在")

	// 替换时留空：不修改
	flash = wantRedirect(t, "留空替换", post(url.Values{"mode": {"edit"}, "name": {"TOKEN"}, "value": {""}}), accountScopeURL)
	if flashZH(t, flash) != "没有填写新值，secret TOKEN 保持不变。" || e.secretVersion("TOKEN") != v1 {
		t.Errorf("留空替换：提示 %q，版本 %d → %d", flash, v1, e.secretVersion("TOKEN"))
	}
	// 替换：版本改变
	wantRedirect(t, "替换", post(url.Values{"mode": {"edit"}, "name": {"TOKEN"}, "value": {"new-value"}}), accountScopeURL)
	if e.secretVersion("TOKEN") == v1 {
		t.Error("替换后 secret 的版本应改变")
	}
	flash = wantRedirect(t, "替换不存在的 secret", e.get(e.alice, "/settings/secrets/edit?account=tester&name=NOPE"), accountScopeURL)
	mustHave(t, "提示", flashZH(t, flash), "secret NOPE 不存在（可能已被删除）")

	// 删除
	flash = wantRedirect(t, "删除", e.post(e.alice, "/settings/secrets/delete", url.Values{"account": {testAccount}, "name": {"TOKEN"}}), accountScopeURL)
	if flashZH(t, flash) != "已删除 secret TOKEN。" || e.secretVersion("TOKEN") != 0 {
		t.Errorf("删除 secret：提示 %q", flash)
	}
	for _, a := range append(e.audits("config.secret.set"), e.audits("config.secret.delete")...) {
		if a.Actor != "alice" || strings.Contains(fmt.Sprint(a.Detail), "value") {
			t.Errorf("secret 的审计应记录 alice、不含值：%+v", a)
		}
	}
	if n := len(e.audits("config.secret.set")); n != 2 {
		t.Errorf("config.secret.set 的审计 = %d 条，应为 2（留空替换不写）", n)
	}
}

func TestPostEnvironment(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	create := func(form url.Values) *httptest.ResponseRecorder {
		form.Set("create", "1")
		return e.post(e.alice, "/settings/environments", form)
	}

	// 新建：校验失败时 422，保留输入
	for _, c := range []struct {
		name string
		form url.Values
		want []string
		// field、value 是保留的输入（为空时不检查）
		field, value string
	}{
		{"没有作用域", url.Values{"name": {"release"}}, []string{"请选择 environment 的作用域"}, "name", "release"},
		{"没有名称", url.Values{"scope": {"account:tester"}}, []string{"请填写 environment 名称"}, "scope", "account:tester"},
		{
			"发布仓库格式不对",
			url.Values{"scope": {"account:tester"}, "name": {"release"}, "allowed_publish_repos": {"tester/pub, bad"}},
			[]string{"每个仓库都应写成 owner/name：bad"},
			"allowed_publish_repos", "tester/pub, bad",
		},
		{"仓库不存在", url.Values{"scope": {"repo:tester/ghost"}, "name": {"release"}}, []string{"仓库 tester/ghost 不在 Kiln 中"}, "", ""},
	} {
		w := create(c.form)
		wantStatus(t, c.name, w, http.StatusUnprocessableEntity)
		mustHave(t, c.name, bodyZH(t, w.Body.String()), c.want...)
		p := pageData[struct{ Form *formData }](t, w.Body.String())
		if p.Form == nil || !p.Form.Failed || (c.field != "" && p.Form.Values[c.field] != c.value) {
			t.Errorf("%s：应保留 %s 的输入 %q：%+v", c.name, c.field, c.value, p.Form)
		}
	}
	if n := e.count(`SELECT count(*) FROM environments`); n != 0 {
		t.Fatalf("校验失败时不应创建 environment：%d", n)
	}

	// 新建账号级和仓库级的同名 environment
	flash := wantRedirect(t, "新建账号级", create(url.Values{"scope": {"account:tester"}, "name": {"release"}, "ref_policy": {"refs/tags/v*"}}),
		"/settings/scope?account=tester&env=release")
	mustHave(t, "提示", flashZH(t, flash), "已创建 environment release")
	wantRedirect(t, "新建仓库级", create(url.Values{"scope": {"repo:" + testRepo}, "name": {"release"}}), "/settings/scope?env=release&repo=tester%2Fdemo")
	// 同一作用域中已存在（名称不区分大小写）
	w := create(url.Values{"scope": {"account:tester"}, "name": {"RELEASE"}})
	wantStatus(t, "已存在", w, http.StatusUnprocessableEntity)
	mustHave(t, "已存在", bodyZH(t, w.Body.String()), "environment RELEASE 已存在于这个作用域")
	if n := e.count(`SELECT count(*) FROM environments`); n != 2 {
		t.Fatalf("应有 2 个 environment：%d", n)
	}

	// 覆盖关系：环境列表和两个配置页都说明
	list := pageData[struct{ Envs []envData }](t, e.get(e.alice, "/settings/environments").Body.String()).Envs
	if len(list) != 2 {
		t.Fatalf("环境列表：%+v", list)
	}
	for _, v := range list {
		switch v.Repo {
		case "":
			expect(t, "账号级 environment 在 tester/demo 中被同名仓库级 environment 整体覆盖", v,
				slices.Equal(v.OverriddenIn, []string{testRepo}), !v.Overrides, slices.Equal(v.RefPolicy, []string{"refs/tags/v*"}), v.Account == testAccount)
		case testRepo:
			expect(t, "仓库级 environment 整体覆盖账号级同名 environment", v,
				v.Overrides, len(v.OverriddenIn) == 0, v.URL == "/settings/scope?env=release&repo=tester%2Fdemo")
		default:
			t.Errorf("环境列表中的 %+v", v)
		}
	}
	sp := pageData[scopeData](t, e.get(e.alice, "/settings/scope?account=tester&env=release").Body.String())
	if expect(t, "账号级配置页", sp, sp.Kind == store.ScopeEnvironment, sp.Repo == "", sp.Environment != nil) {
		expect(t, "账号级配置页", sp.Environment, slices.Equal(sp.Environment.OverriddenIn, []string{testRepo}), slices.Equal(sp.Environment.RefPolicy, []string{"refs/tags/v*"}))
	}
	sp = pageData[scopeData](t, e.get(e.alice, "/settings/scope?repo=tester/demo&env=RELEASE").Body.String())
	expect(t, "仓库级配置页", sp, sp.Kind == store.ScopeEnvironment, sp.Repo == testRepo, sp.Environment != nil && sp.Environment.Overrides, sp.Env == "release")

	// 修改保护规则
	edit := func(form url.Values) *httptest.ResponseRecorder {
		form.Set("account", testAccount)
		form.Set("name", "release")
		return e.post(e.alice, "/settings/environments", form)
	}
	w = edit(url.Values{"ref_policy": {"refs/heads/main\nrefs/tags/*"}, "allowed_publish_repos": {"tester/pub nope"}})
	wantStatus(t, "修改时发布仓库格式不对", w, http.StatusUnprocessableEntity)
	mustHave(t, "修改失败", bodyZH(t, w.Body.String()), "每个仓库都应写成 owner/name：nope")
	f := pageData[scopeData](t, w.Body.String()).EnvForm
	expect(t, "修改失败应保留保护规则的输入", f, f != nil && f.Failed && f.Values["ref_policy"] == "refs/heads/main\nrefs/tags/*")
	flash = wantRedirect(t, "修改", edit(url.Values{"ref_policy": {"refs/heads/main\nrefs/tags/*"}, "allowed_publish_repos": {"tester/pub"}}),
		"/settings/scope?account=tester&env=release")
	mustHave(t, "提示", flashZH(t, flash), "已保存 environment release 的保护规则")
	envs, err := e.svc.ListEnvironments(e.ctx, testAccount)
	e.must(err)
	var got string
	for _, x := range envs {
		if x.Repo == "" {
			got = fmt.Sprint(x.RefPolicy, x.AllowedPublishRepos)
		}
	}
	if got != "[refs/heads/main refs/tags/*] [tester/pub]" {
		t.Errorf("修改后的保护规则 = %s", got)
	}

	// 删除：连同其中的变量
	e.must(e.svc.SetVariable(e.ctx, scopeProto(testAccount, "", "release"), "FOO", "bar", "setup"))
	flash = wantRedirect(t, "删除", e.post(e.alice, "/settings/environments/delete", url.Values{"account": {testAccount}, "env": {"release"}}), "/settings/environments")
	mustHave(t, "提示", flashZH(t, flash), "已删除 environment release 及其中的变量和 secret")
	if e.count(`SELECT count(*) FROM environments WHERE repo_id IS NULL`) != 0 || e.count(`SELECT count(*) FROM variables`) != 0 {
		t.Error("应删除 environment 及其中的变量")
	}
	flash = wantRedirect(t, "删除不存在的", e.post(e.alice, "/settings/environments/delete", url.Values{"account": {testAccount}, "env": {"release"}}),
		"/settings/scope?account=tester&env=release")
	mustHave(t, "提示", flashZH(t, flash), "environment release 不存在")
	if got := e.configActors("environment.upsert"); fmt.Sprint(got) != "[alice alice alice]" {
		t.Errorf("environment.upsert 的审计操作者 = %v", got)
	}
}

func TestPageScope(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.upsertEnv(testAccount, "", "release", "refs/tags/v*")
	for _, c := range []struct {
		target, label string
		kind          store.ScopeKind
		env           string
	}{
		{accountScopeURL, "tester", store.ScopeOwner, ""},
		{"/settings/scope?account=TESTER", "", store.ScopeOwner, ""},
		{repoScopeURL, "tester/demo", store.ScopeRepo, ""},
		// environment 名称不区分大小写，页面使用保存的名称
		{"/settings/scope?account=tester&env=Release", "", store.ScopeEnvironment, "release"},
	} {
		w := e.get(e.alice, c.target)
		wantStatus(t, c.target, w, http.StatusOK)
		p := pageData[scopeData](t, w.Body.String())
		if (c.label != "" && p.Label != c.label) || p.Kind != c.kind || p.Env != c.env || len(p.Vars) != 0 || len(p.Secrets) != 0 {
			t.Errorf("%s：%+v", c.target, p)
		}
		if c.env != "" && (p.Environment == nil || p.Environment.Name != c.env || !slices.Equal(p.Environment.RefPolicy, []string{"refs/tags/v*"})) {
			t.Errorf("%s：environment %+v", c.target, p.Environment)
		}
	}
	// 作用域无效：回到变量与密钥分区并说明
	for target, want := range map[string]string{
		"/settings/scope":                  "需要指定账号",
		"/settings/scope?account=ghost":    "账号 ghost 不在 Kiln 中",
		"/settings/scope?repo=tester/nope": "tester/nope",
	} {
		flash := wantRedirect(t, target, e.get(e.alice, target), "/settings/variables")
		mustHave(t, target, flashZH(t, flash), want)
	}
	// environment 不存在：404
	w := e.get(e.alice, "/settings/scope?account=tester&env=nope")
	wantErrorPage(t, "environment 不存在", w, http.StatusNotFound, []string{"没有这个 environment"}, []string{"/runs"})
	w = e.post(e.alice, "/settings/variables", url.Values{"account": {testAccount}, "env": {"nope"}, "mode": {"new"}, "name": {"X"}, "value": {"v"}})
	wantErrorPage(t, "在不存在的 environment 中新建变量", w, http.StatusNotFound, []string{"没有这个 environment"}, []string{"/runs"})
}

func TestPageSettingsIndexes(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.upsertEnv(testAccount, "", "release")
	e.must(e.svc.SetVariable(e.ctx, scopeProto(testAccount, "", ""), "A", "1", "setup"))
	e.must(e.svc.SetVariable(e.ctx, scopeProto(testAccount, "", ""), "B", "2", "setup"))
	e.must(e.svc.SetSecret(e.ctx, scopeProto("", testRepo, ""), "S", "x", "setup"))

	p := pageData[varsIndexPage](t, e.get(e.alice, "/settings/variables").Body.String())
	if !slices.Contains(p.Accounts, scopeCount{Name: testAccount, URL: accountScopeURL, Vars: 2}) ||
		!slices.Contains(p.Repos, scopeCount{Name: testRepo, URL: repoScopeURL, Secrets: 1}) ||
		len(p.Envs) != 1 || p.Envs[0].Name != "tester / release" {
		t.Errorf("变量与密钥：%+v", p)
	}
	// 变量和 secret 是两个独立的一对多集合：各自先聚合再连接，2 个变量 × 3 个 secret 不能数成 6 个（2026-11-17 代码审计的
	// JOIN 扇出检查）；environment 级的不算进账号和仓库
	for _, n := range []string{"S1", "S2", "S3"} {
		e.must(e.svc.SetSecret(e.ctx, scopeProto(testAccount, "", ""), n, "x", "setup"))
	}
	for _, n := range []string{"V1", "V2"} {
		e.must(e.svc.SetVariable(e.ctx, scopeProto("", testRepo, ""), n, "1", "setup"))
	}
	e.must(e.svc.SetSecret(e.ctx, scopeProto("", testRepo, ""), "T", "x", "setup"))
	e.must(e.svc.SetVariable(e.ctx, scopeProto(testAccount, "", "release"), "E", "1", "setup"))
	p = pageData[varsIndexPage](t, e.get(e.alice, "/settings/variables").Body.String())
	if !slices.Equal(p.Accounts, []scopeCount{{Name: testAccount, URL: accountScopeURL, Vars: 2, Secrets: 3}}) ||
		!slices.Equal(p.Repos, []scopeCount{{Name: testRepo, URL: repoScopeURL, Vars: 2, Secrets: 2}}) {
		t.Errorf("变量与密钥的数量（2×3）：账号 %+v，仓库 %+v", p.Accounts, p.Repos)
	}
	type envNewData struct {
		Accounts []string
		Repo     string
		Form     *formData
	}
	if p := pageData[envNewData](t, e.get(e.alice, "/settings/environments/new?repo=tester/demo").Body.String()); p.Repo != testRepo ||
		!slices.Equal(p.Accounts, []string{testAccount}) || p.Form == nil || p.Form.Values["scope"] != "repo:tester/demo" {
		t.Errorf("新建 environment（预选仓库）：%+v", p)
	}
	if p := pageData[envNewData](t, e.get(e.alice, "/settings/environments/new?account=tester").Body.String()); p.Repo != "" ||
		p.Form == nil || p.Form.Values["scope"] != "account:tester" {
		t.Errorf("新建 environment（预选账号）：%+v", p)
	}
}

func TestPageRepos(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	for i := range 55 {
		e.addRepo(int64(1000+i), fmt.Sprintf("svc-%02d", i))
	}
	e.exec(`UPDATE repos SET active=false WHERE name='svc-54'`)

	// reposData 是仓库页的页面数据（reposPage，内嵌的 repoSearch 的字段在同一层）。
	type reposData struct {
		repoSearch
		Pick string
		Sync *store.SyncStatus
	}
	names := func(p *reposData) []string {
		var out []string
		for _, r := range p.Repos {
			out = append(out, r.FullName)
		}
		return out
	}
	p := pageData[reposData](t, e.get(e.alice, "/settings/repos").Body.String())
	if p.Total != 56 || p.Page != 1 || p.Pages != 2 || p.NextURL != "/settings/repos?page=2" || p.PrevURL != "" || p.Sync != nil || p.Pick != "" {
		t.Errorf("第一页（还没有手动刷新过）：%+v", p)
	}
	if got := names(&p); len(got) != 50 || got[0] != testRepo || !slices.Contains(got, "tester/svc-48") || slices.Contains(got, "tester/svc-49") {
		t.Errorf("第一页应列出按名称排序的前 50 个仓库：%v", got)
	}
	p = pageData[reposData](t, e.get(e.alice, "/settings/repos?page=99").Body.String())
	if p.Page != 2 || p.Pages != 2 || p.PrevURL != "/settings/repos" || p.NextURL != "" {
		t.Errorf("超出页数取最后一页：%+v", p)
	}
	if i := slices.IndexFunc(p.Repos, func(r repoView) bool { return r.FullName == "tester/svc-54" }); i < 0 || p.Repos[i].Active {
		t.Errorf("最后一页应有已停用的 svc-54：%+v", p.Repos)
	}
	if p := pageData[reposData](t, e.get(e.alice, "/settings/repos?q=SVC-0").Body.String()); p.Query != "SVC-0" || p.Total != 10 || len(p.Repos) != 10 {
		t.Errorf("搜索（不区分大小写）：%+v", p)
	}
	if p := pageData[reposData](t, e.get(e.alice, "/settings/repos?q=zzz").Body.String()); p.Total != 0 || len(p.Repos) != 0 {
		t.Errorf("没有匹配：%+v", p)
	}

	// 选择器模式：为新建仓库级 environment 选择仓库，分页链接保留 pick
	p = pageData[reposData](t, e.get(e.alice, "/settings/repos?pick=env").Body.String())
	if p.Pick != "env" || p.NextURL != "/settings/repos?page=2&pick=env" {
		t.Errorf("选择仓库：Pick %q NextURL %q", p.Pick, p.NextURL)
	}
}

func TestPageOps(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	if p := pageData[opsPage](t, e.get(e.alice, "/settings/ops").Body.String()); len(p.Outbox) != 0 || p.App != "kiln-test" || p.PublicURL != "https://kiln.example.com" {
		t.Errorf("没有 outbox（全部已送达）：%+v", p)
	}
	var dead, pending, done int64
	for key, p := range map[string]*int64{"checkrun:dead": &dead, "checkrun:pending": &pending, "checkrun:done": &done} {
		state := strings.TrimPrefix(key, "checkrun:")
		e.must(e.db.QueryRow(e.ctx, `INSERT INTO outbox (dedupe_key, kind, state, attempts, last_error) VALUES ($1,'checkrun.job',$2,3,'boom-'||$2) RETURNING id`,
			key, state).Scan(p))
	}
	// 已送达的不列出；前端只为 dead 的记录提供重试
	states := map[int64]string{}
	for _, it := range pageData[opsPage](t, e.get(e.alice, "/settings/ops").Body.String()).Outbox {
		states[it.ID] = it.State + " " + it.DedupeKey + " " + it.LastError
	}
	if want := map[int64]string{dead: "dead checkrun:dead boom-dead", pending: "pending checkrun:pending boom-pending"}; !maps.Equal(states, want) {
		t.Errorf("outbox = %v，应为 %v", states, want)
	}

	flash := wantRedirect(t, "重试", e.post(e.alice, fmt.Sprintf("/outbox/%d/retry", dead), nil), "/settings/ops")
	if flashZH(t, flash) != fmt.Sprintf("已重新排队 outbox #%d。", dead) {
		t.Errorf("重试的提示 = %q", flash)
	}
	var state string
	var attempts int
	e.must(e.db.QueryRow(e.ctx, `SELECT state, attempts FROM outbox WHERE id=$1`, dead).Scan(&state, &attempts))
	if state != store.OutboxPending || attempts != 0 {
		t.Errorf("重试后 state=%s attempts=%d", state, attempts)
	}
	if a := e.audits("outbox.retry"); len(a) != 1 || a[0].Actor != "alice" {
		t.Errorf("重试的审计 = %+v", a)
	}
	flash = wantRedirect(t, "重试不存在的", e.post(e.alice, "/outbox/99999/retry", nil), "/settings/ops")
	if flashZH(t, flash) == "" || flashZH(t, flash) == "已重新排队" {
		t.Errorf("重试不存在的记录应说明原因：%q", flash)
	}
}

func TestDocker(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	post := func(form url.Values) *httptest.ResponseRecorder {
		if !form.Has("account") {
			form.Set("account", testAccount)
		}
		w := e.post(e.alice, "/settings/docker", form)
		noLeak(t, "提交 Docker Hub 连接", w, form.Get("token"))
		if w.Code == http.StatusUnprocessableEntity {
			if f := pageData[struct{ Form *formData }](t, w.Body.String()).Form; f == nil || f.Values["token"] != "" {
				t.Errorf("校验失败的表单不应保留访问令牌：%+v", f)
			}
		}
		return w
	}
	// dockerData 是页面数据中的 Docker Hub 连接（dockerView），另带计算的编辑页地址 URL。
	type dockerData struct {
		Account, Username, Namespace, Status, URL string
		Token                                     *store.SecretMeta
		Missing                                   []string
	}
	// status 返回连接卡片中 tester 的连接状态。
	status := func() string {
		t.Helper()
		list := pageData[struct{ Docker []dockerData }](t, e.get(e.alice, "/settings").Body.String()).Docker
		if len(list) != 1 || list[0].Account != testAccount || list[0].URL != dockerPageURL {
			t.Fatalf("连接卡片：%+v", list)
		}
		return list[0].Status
	}
	if s := status(); s != dockerUnconfigured {
		t.Errorf("初始状态 = %s", s)
	}

	// 账号不在 Kiln 中
	flash := wantRedirect(t, "打开不存在的账号", e.get(e.alice, "/settings/docker?account=ghost"), "/settings")
	mustHave(t, "提示", flashZH(t, flash), "账号不在 Kiln 中")
	flash = wantRedirect(t, "保存到不存在的账号", post(url.Values{"account": {"ghost"}, "username": {"u"}, "namespace": {"ns"}, "token": {"t"}}), "/settings")
	mustHave(t, "提示", flashZH(t, flash), "账号不在 Kiln 中，没有保存")

	// 校验失败：422，令牌不回显
	const token = "dckr_pat_secret_value"
	for _, c := range []struct {
		name string
		form url.Values
		want []string
	}{
		{"首次没有令牌", url.Values{"username": {"bot"}, "namespace": {"acme"}}, []string{"首次配置需要填写访问令牌"}},
		{
			"命名空间大写",
			url.Values{"username": {"bot"}, "namespace": {"Acme"}, "token": {token}},
			[]string{"命名空间只能包含小写字母", "刚才输入的访问令牌没有保留"},
		},
		{"用户名含 @", url.Values{"username": {"bot@x"}, "namespace": {"acme"}, "token": {token}}, []string{"用户名不能包含空格、/、: 或 @"}},
		{"都没有填", url.Values{}, []string{"请填写 Docker Hub 用户名", "请填写镜像命名空间"}},
	} {
		w := post(c.form)
		wantStatus(t, c.name, w, http.StatusUnprocessableEntity)
		mustHave(t, c.name, bodyZH(t, w.Body.String()), c.want...)
		if strings.Contains(w.Body.String(), token) {
			t.Errorf("%s：页面回显了访问令牌", c.name)
		}
		// 保留用户名和命名空间的输入
		if f := pageData[struct{ Form *formData }](t, w.Body.String()).Form; expect(t, c.name, f, f != nil) {
			expect(t, c.name+"：应保留输入", f, f.Failed, f.Values["username"] == c.form.Get("username"), f.Values["namespace"] == c.form.Get("namespace"))
		}
	}
	if n := e.count(`SELECT count(*) FROM environments`); n != 0 {
		t.Fatalf("校验失败时不应创建 environment：%d", n)
	}

	// 首次保存：创建 docker-release（默认 ref 策略）和三项配置
	flash = wantRedirect(t, "首次保存", post(url.Values{"username": {"bot"}, "namespace": {"acme"}, "token": {token}, "ref_policy": {""}}), dockerPageURL)
	mustHave(t, "提示", flashZH(t, flash), "已创建 environment docker-release（只允许 refs/tags/v*）并保存 Docker Hub 配置。",
		"保存只把配置存进 Kiln")
	if e.varValue(store.ScopeEnvironment, admin.DockerUserVar) != "bot" || e.varValue(store.ScopeEnvironment, admin.DockerNamespaceVar) != "acme" || e.secretVersion(admin.DockerTokenSecret) == 0 {
		t.Fatal("应保存用户名、命名空间和令牌")
	}
	if s := status(); s != dockerSaved {
		t.Errorf("保存后的状态 = %s", s)
	}
	body := e.get(e.alice, "/settings/docker?account=TESTER").Body.String()
	if p := pageData[struct {
		Docker *dockerData
		Form   *formData
	}](t, body); expect(t, "编辑页（账号不区分大小写）", p, p.Docker != nil, p.Form != nil) {
		expect(t, "编辑页", p, p.Docker.Account == testAccount, p.Docker.Token != nil, p.Docker.Username == "bot", p.Docker.Namespace == "acme",
			p.Form.Values["username"] == "bot", p.Form.Values["namespace"] == "acme")
	}
	if strings.Contains(body, token) {
		t.Error("编辑页不应显示访问令牌")
	}

	// 没有修改
	flash = wantRedirect(t, "没有修改", post(url.Values{"username": {"bot"}, "namespace": {"acme"}}), dockerPageURL)
	mustHave(t, "提示", flashZH(t, flash), "没有修改，Docker Hub 配置保持不变；访问令牌继续使用原来的。")

	// 用当前这套配置推送成功后显示已验证；修改命名空间后回到未验证
	e.exec(`INSERT INTO image_publishes (name, version, digest, job_id, username, password_secret_version) VALUES ('docker.io/acme/app','v1.0.0','sha256:x',1,'bot',$1)`,
		e.secretVersion(admin.DockerTokenSecret))
	if s := status(); s != dockerVerified {
		t.Errorf("推送成功后的状态 = %s", s)
	}
	flash = wantRedirect(t, "修改命名空间", post(url.Values{"username": {"bot"}, "namespace": {"acme2"}}), dockerPageURL)
	mustHave(t, "提示", flashZH(t, flash), "已保存 Docker Hub 配置（DOCKERHUB_NAMESPACE）；访问令牌没有修改，继续使用原来的。")
	if s := status(); s != dockerSaved {
		t.Errorf("修改命名空间后的状态 = %s", s)
	}

	// 删除：需要确认；保留 environment 和其中的其他变量
	e.must(e.svc.SetVariable(e.ctx, scopeProto(testAccount, "", admin.DockerEnvName), "EXTRA", "keep", "setup"))
	flash = wantRedirect(t, "没有确认", e.post(e.alice, "/settings/docker/delete", url.Values{"account": {testAccount}}), dockerPageURL)
	mustHave(t, "提示", flashZH(t, flash), "没有删除：请先勾选确认")
	flash = wantRedirect(t, "删除", e.post(e.alice, "/settings/docker/delete", url.Values{"account": {testAccount}, "confirm": {"1"}}), "/settings")
	mustHave(t, "提示", flashZH(t, flash), "已删除 tester 的 Docker Hub 凭据；environment docker-release 及其保护规则保留")
	if e.secretVersion(admin.DockerTokenSecret) != 0 || e.varValue(store.ScopeEnvironment, admin.DockerUserVar) != "<none>" ||
		e.varValue(store.ScopeEnvironment, "EXTRA") != "keep" || e.count(`SELECT count(*) FROM environments`) != 1 {
		t.Error("删除应只去掉连接的三项配置")
	}
	if s := status(); s != dockerUnconfigured {
		t.Errorf("删除后的状态 = %s", s)
	}
	for _, a := range e.audits("config.secret.set") {
		if a.Actor != "alice" {
			t.Errorf("审计的操作者 = %s", a.Actor)
		}
	}
}

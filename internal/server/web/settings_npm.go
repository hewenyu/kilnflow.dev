package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 账号级 npm 配置（PLAN #27.2）：与 Docker Hub 连接平级，在"连接与发布凭据"分区中每个账号一张卡片，
// 编辑页 /settings/npm?account=…。release.npm 发布到 @<scope>/<name>，registry、scope 和 token 每个账号一份。
// token 只写不读：页面只显示是否设置和更新记录，从不查询或渲染 token；替换配置时要重新填写 token
// （admin.Service.SetNpmAccount 整体覆盖，与 CLI kiln npm set 相同）。
// 保存和删除经 admin.Service（与 CLI 共用校验和审计），权限与 secret 相同（只有管理员）。

// npmConfig 是账号已保存的 npm 配置（不含 token）。
type npmConfig struct {
	Scope, Registry, UpdatedBy string
	UpdatedAt                  time.Time
}

// npmView 是一个账号的 npm 配置。
type npmView struct {
	Account string
	// Config 是已保存的配置（没有配置时为 nil）。
	Config *npmConfig
}

// URL 返回 npm 配置编辑页的地址。
func (v *npmView) URL() string { return npmURL(v.Account) }

func npmURL(account string) string { return "/settings/npm?account=" + url.QueryEscape(account) }

// npmStatus 读取账号的 npm 配置。
func (d *Dashboard) npmStatus(ctx context.Context, account string) (*npmView, error) {
	ref, err := store.ResolveScope(ctx, d.db, account, "", "")
	if err != nil {
		return nil, err
	}
	v := &npmView{Account: account}
	a, err := store.GetNpmAccount(ctx, d.db, ref.OwnerID)
	if err != nil || a == nil {
		return v, err
	}
	v.Config = &npmConfig{Scope: a.Scope, Registry: a.Registry, UpdatedBy: a.UpdatedBy, UpdatedAt: a.UpdatedAt}
	return v, nil
}

// npmPage 是 npm 配置编辑页的数据。
type npmPage struct {
	settingsBase
	Npm  *npmView
	Form *formState
	// DefaultRegistry 是 registry 留空时使用的 npm 官方 registry。
	DefaultRegistry string
}

func (d *Dashboard) pageNpm(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.URL.Query().Get("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashMsg(w, r, "/settings", message(msgConnEnterFromCard))
		return
	}
	v, err := d.npmStatus(ctx, account)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	f := newForm()
	f.Values["registry"] = npm.DefaultRegistry
	if v.Config != nil {
		f.Values["scope"], f.Values["registry"] = v.Config.Scope, v.Config.Registry
	}
	d.renderNpm(w, r, v, f, http.StatusOK)
}

func (d *Dashboard) renderNpm(w http.ResponseWriter, r *http.Request, v *npmView, f *formState, status int) {
	p := &npmPage{Section: sectionConn, Npm: v, Form: f, DefaultRegistry: npm.DefaultRegistry}
	d.renderPage(w, r, "settings_npm", message(msgSetTitleNpm, strArg(argAccount, v.Account)), p, status)
}

// readNpmForm 读取并校验 npm 表单（scope、registry、token）。token 只返回，不写入表单状态。
// scope 不带 @ 时补上；registry 留空时用 npm 官方 registry。规则与 admin.Service.SetNpmAccount 相同，这里先校验是为了把错误标在字段旁。
func readNpmForm(r *http.Request) (f *formState, token string) {
	f = newForm()
	scope, registry := strings.TrimSpace(r.PostFormValue("scope")), strings.TrimSpace(r.PostFormValue("registry"))
	f.Values["scope"], f.Values["registry"] = scope, registry
	switch s, err := npm.NormalizeScope(scope); {
	case scope == "":
		f.failText("scope", message(msgNpmErrScopeEmpty))
	case err != nil:
		f.failText("scope", settingsErrText(err))
	default:
		f.Values["scope"] = s
	}
	if registry == "" {
		registry = npm.DefaultRegistry
		f.Values["registry"] = registry
	}
	if err := npm.CheckRegistry(registry); err != nil {
		f.failText("registry", settingsErrText(err))
		dropUserInfo(f, "registry", err)
	}
	token = strings.TrimSpace(r.PostFormValue("token"))
	switch {
	case token == "":
		f.failText("token", message(msgNpmErrTokenEmpty))
	case f.Failed():
		f.failText("token", message(msgNpmErrTokenNotKept))
	}
	return f, token
}

// postNpm 新建或替换账号的 npm 配置（scope、registry 和 token 整体覆盖）。校验失败时重新渲染编辑页（HTTP 422），
// 保留 scope 和 registry，token 不保留。
func (d *Dashboard) postNpm(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.PostFormValue("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashMsg(w, r, "/settings", message(msgConnNotSaved))
		return
	}
	v, err := d.npmStatus(ctx, account)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	f, token := readNpmForm(r)
	scope := f.Values["scope"]
	if !f.Failed() {
		acct, err := d.admin.SetNpmAccount(ctx, account, scope, f.Values["registry"], token, se.Login)
		if err != nil {
			f.failText(formGeneral, d.settingsFormFailure(r, err, msgNpmSaveFailed))
		} else {
			scope = acct.Scope
		}
	}
	if f.Failed() {
		d.renderNpm(w, r, v, f, http.StatusUnprocessableEntity)
		return
	}
	d.flashMsg(w, r, npmURL(account), message(msgNpmSaved, strArg(argScope, scope)))
}

// postNpmDelete 删除账号的 npm 配置。需要确认（confirm=1）。
func (d *Dashboard) postNpmDelete(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.PostFormValue("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashMsg(w, r, "/settings", message(msgConnAccountMissing))
		return
	}
	if r.PostFormValue("confirm") != "1" {
		d.flashMsg(w, r, npmURL(account), message(msgNpmDeleteUnconfirmed))
		return
	}
	if err := d.admin.DeleteNpmAccount(ctx, account, se.Login); err != nil {
		d.flashMsg(w, r, npmURL(account), d.settingsFailureFlash(r, err, msgNpmDeleteFailed, msgNpmDeleteFailedRef, strArg(argAccount, account)))
		return
	}
	d.flashMsg(w, r, "/settings", message(msgNpmDeleted, strArg(argAccount, account)))
}

// npmjsHost 是 npm 官方 registry 的主机：发布到这里的包在 www.npmjs.com 上有页面。
var npmjsHost = func() string {
	u, _ := url.Parse(npm.DefaultRegistry)
	return u.Host
}()

// npmPackageURL 返回已发布的 npm 包在 www.npmjs.com 上的版本页。只有发布到 npm 官方 registry（tarball 地址的主机是
// registry.npmjs.org）时才有，其他 registry 返回空字符串。
func npmPackageURL(detail map[string]any) string {
	name, _ := detail["package"].(string)
	version, _ := detail["version"].(string)
	tarball, _ := detail["tarball"].(string)
	u, err := url.Parse(tarball)
	if npm.ValidName(name) != nil || version == "" || err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, npmjsHost) {
		return ""
	}
	return "https://www.npmjs.com/package/" + name + "/v/" + url.PathEscape(version)
}

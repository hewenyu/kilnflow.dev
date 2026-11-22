package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 作用域配置页（账号级、仓库级、environment 级）以及其中变量和 secret 的新建、编辑页。
// 列表只负责展示和进入；新建、编辑（替换）和删除是分开的动作：编辑页预填非敏感的值，删除放在编辑页的危险操作区。

// errScopeNotFound 表示作用域配置页要找的 environment 不存在。
var errScopeNotFound = errors.New("作用域不存在")

// badScopeError 是解析作用域失败（账号、仓库或 environment 无效）的错误，消息可以直接显示给用户。
type badScopeError struct{ err error }

func (e *badScopeError) Error() string { return e.err.Error() }
func (e *badScopeError) Unwrap() error { return e.err }

// itemKind 是变量与密钥页面中条目的种类，也是地址中的一段（/settings/<种类>/...）。
type itemKind string

const (
	kindVariables itemKind = "variables"
	kindSecrets   itemKind = "secrets"
)

// itemMode 是变量、secret 表单的提交方式（表单字段 mode）；空表示新建或覆盖（兼容旧表单和脚本）。
type itemMode string

const (
	modeNew  itemMode = "new"
	modeEdit itemMode = "edit"
)

// scopePage 是作用域配置页的数据，也是变量、secret 编辑页的上下文。
type scopePage struct {
	settingsBase
	// Account / Repo / Env 是表单中回传的作用域（Repo 非空时 Account 为空）。
	Account, Repo, Env string
	Label              string
	Kind               store.ScopeKind
	// KindLabel 是作用域的类型说明，例如"仓库级"。
	KindLabel string
	// Environment 是 Env 对应的 environment（只用于 environment 作用域）。
	Environment *envView
	Vars        []store.Variable
	Secrets     []store.SecretMeta
	// Docker 表示这是 Docker Hub 连接使用的账号级 environment，DockerURL 是连接编辑页。
	Docker    bool
	DockerURL string
	// EnvForm 是 environment 保护规则表单（校验失败时带着输入和错误重新渲染）。
	EnvForm *formState
	// CrumbLabel / CrumbURL 是返回的上一级分区。
	CrumbLabel, CrumbURL string
}

// URL 返回作用域配置页的地址。
func (p *scopePage) URL() string { return scopeURL(p.Account, p.Repo, p.Env) }

// HasVar 表示作用域中有名为 name 的变量。
func (p *scopePage) HasVar(name string) bool {
	return slices.ContainsFunc(p.Vars, func(v store.Variable) bool { return v.Name == name })
}

// secret 返回作用域中名为 name 的 secret 的元数据，没有时返回 nil。
func (p *scopePage) secret(name string) *store.SecretMeta {
	for i := range p.Secrets {
		if p.Secrets[i].Name == name {
			return &p.Secrets[i]
		}
	}
	return nil
}

// loadScope 读取作用域配置页的数据。environment 不存在时返回 errScopeNotFound；账号或仓库无效时返回 *badScopeError
// （ResolveScope 的说明）；内部错误原样返回。
func (d *Dashboard) loadScope(ctx context.Context, account, repo, env string) (*scopePage, error) {
	p := &scopePage{Account: account, Repo: repo, Env: env, EnvForm: newForm()}
	if p.Repo != "" {
		p.Account = ""
	}
	ref, err := store.ResolveScope(ctx, d.db, p.Account, p.Repo, p.Env)
	if errors.Is(err, store.ErrEnvironmentNotFound) {
		// 账号或仓库有效，只是 environment 不存在（名称写错或已被删除）
		return nil, errScopeNotFound
	}
	if err != nil {
		if internalError(err) {
			// 数据库错误等不是作用域无效，不能作为提示显示，交给 scopeError 返回 500 错误页
			return nil, err
		}
		return nil, &badScopeError{err: err}
	}
	p.Label, p.Kind = ref.Label, ref.Kind
	p.Section, p.CrumbLabel, p.CrumbURL = sectionVars, "变量与密钥", "/settings/variables"
	switch ref.Kind {
	case store.ScopeOwner:
		p.KindLabel = "账号级（账号下的所有仓库可见）"
	case store.ScopeRepo:
		p.KindLabel = "仓库级"
	case store.ScopeEnvironment:
		p.Section, p.CrumbLabel, p.CrumbURL = sectionEnvs, "环境", "/settings/environments"
		if err := d.findEnvironment(ctx, p); err != nil {
			return nil, err
		}
	}
	if p.Vars, err = store.ListVariables(ctx, d.db, ref); err != nil {
		return nil, err
	}
	if p.Secrets, err = store.ListSecretMeta(ctx, d.db, ref); err != nil {
		return nil, err
	}
	return p, nil
}

// findEnvironment 为 environment 作用域找到 environment 本身及其覆盖关系。
func (d *Dashboard) findEnvironment(ctx context.Context, p *scopePage) error {
	envs, err := d.admin.ListEnvironments(ctx, "")
	if err != nil {
		return err
	}
	for _, v := range envViews(envs) {
		if strings.EqualFold(v.Name, p.Env) && strings.EqualFold(v.Repo, p.Repo) && (p.Repo != "" || strings.EqualFold(v.Account, p.Account)) {
			p.Environment = &v
			p.Env = v.Name
			break
		}
	}
	if p.Environment == nil {
		return errScopeNotFound
	}
	p.KindLabel = "账号级 environment（账号下的所有仓库共用）"
	if p.Repo != "" {
		p.KindLabel = "仓库级 environment"
	}
	if p.Environment.Docker {
		p.Docker = true
		p.DockerURL = dockerURL(p.Environment.Account)
	}
	envFormValues(p.EnvForm, p.Environment.Environment)
	return nil
}

// scopeFromQuery 读取查询参数中的作用域。
func scopeFromQuery(q url.Values) (account, repo, env string) {
	return strings.TrimSpace(q.Get("account")), strings.TrimSpace(q.Get("repo")), strings.TrimSpace(q.Get("env"))
}

// scopeError 处理 loadScope 的错误：environment 不存在时返回 404，作用域无效时回到变量与密钥分区并提示（与之前的设置页相同），
// 其他错误返回 500。
func (d *Dashboard) scopeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errScopeNotFound) {
		d.notFound(w, r, "没有这个 environment：名称可能写错了，或者它已经被删除。可以在设置的“环境”分区中查看全部 environment。")
		return
	}
	if _, ok := errors.AsType[*badScopeError](err); ok {
		d.flashBack(w, r, "/settings/variables", err.Error())
		return
	}
	d.serverError(w, r, err)
}

// scopeArgs 是 renderScope 的参数。
type scopeArgs struct {
	Account, Repo, Env string
	// EnvForm 非空时代替 environment 当前的配置填进保护规则表单（校验失败后重新渲染）。
	EnvForm *formState
}

func (d *Dashboard) pageScope(w http.ResponseWriter, r *http.Request, _ *session) {
	account, repo, env := scopeFromQuery(r.URL.Query())
	d.renderScope(w, r, &scopeArgs{Account: account, Repo: repo, Env: env}, http.StatusOK)
}

func (d *Dashboard) renderScope(w http.ResponseWriter, r *http.Request, a *scopeArgs, status int) {
	p, err := d.loadScope(r.Context(), a.Account, a.Repo, a.Env)
	if err != nil {
		d.scopeError(w, r, err)
		return
	}
	if a.EnvForm != nil {
		p.EnvForm = a.EnvForm
	}
	d.renderStatus(w, r, "settings_scope", p.Label, p, status)
}

// itemPage 是变量或 secret 的新建、编辑页的数据。
type itemPage struct {
	*scopePage
	// Kind 是 variables 或 secrets；Name 为空表示新建。
	Kind itemKind
	Name string
	// Secret 是被替换的 secret 的元数据（只用于编辑 secret）。
	Secret *store.SecretMeta
	Form   *formState
}

// IsSecret 表示这是 secret 的页面（页面数据中的 IsSecret，见 json.go）。
func (p *itemPage) IsSecret() bool { return p.Kind == kindSecrets }

// itemArgs 是 renderItem 的参数。
type itemArgs struct {
	// Account / Repo / Env 是条目所在的作用域。
	Account, Repo, Env string
	Kind               itemKind
	// Name 为空表示新建。
	Name string
	// Form 为空时按当前保存的值填写表单。
	Form *formState
}

func (d *Dashboard) pageItem(kind itemKind, edit bool) func(w http.ResponseWriter, r *http.Request, se *session) {
	return func(w http.ResponseWriter, r *http.Request, _ *session) {
		a := &itemArgs{Kind: kind}
		if edit {
			if a.Name = strings.TrimSpace(r.URL.Query().Get("name")); a.Name == "" {
				d.notFound(w, r, "地址中没有要编辑的名称（name 参数）。请从变量与密钥的列表中打开。")
				return
			}
		}
		a.Account, a.Repo, a.Env = scopeFromQuery(r.URL.Query())
		d.renderItem(w, r, a, http.StatusOK)
	}
}

func (d *Dashboard) renderItem(w http.ResponseWriter, r *http.Request, a *itemArgs, status int) {
	sp, err := d.loadScope(r.Context(), a.Account, a.Repo, a.Env)
	if err != nil {
		d.scopeError(w, r, err)
		return
	}
	p := &itemPage{scopePage: sp, Kind: a.Kind, Name: a.Name, Form: a.Form}
	if p.Form == nil {
		p.Form = newForm()
	}
	if a.Name != "" {
		switch {
		case a.Kind == kindSecrets:
			if p.Secret = sp.secret(a.Name); p.Secret == nil {
				d.flashBack(w, r, sp.URL(), fmt.Sprintf("secret %s 不存在（可能已被删除）", a.Name))
				return
			}
		case !sp.HasVar(a.Name):
			d.flashBack(w, r, sp.URL(), fmt.Sprintf("变量 %s 不存在（可能已被删除）", a.Name))
			return
		case a.Form == nil:
			for _, v := range sp.Vars {
				if v.Name == a.Name {
					p.Form.Values["value"] = v.Value
				}
			}
		}
	}
	title := "新建变量"
	switch {
	case a.Kind == kindSecrets && a.Name != "":
		title = "替换 secret " + a.Name
	case a.Kind == kindSecrets:
		title = "新建 secret"
	case a.Name != "":
		title = "编辑变量 " + a.Name
	}
	d.renderStatus(w, r, "settings_item", title+" · "+sp.Label, p, status)
}

// itemForm 读取变量或 secret 表单的公共部分：mode（new、edit；空表示新建或覆盖，兼容旧表单和脚本）和名称。
func itemForm(r *http.Request, sp *scopePage, kind itemKind) (mode itemMode, name string, f *formState) {
	mode, name, f = itemMode(r.PostFormValue("mode")), strings.TrimSpace(r.PostFormValue("name")), newForm()
	f.Values["name"] = name
	if admin.ValidName(name) != nil {
		f.fail("name", "名称只能包含字母、数字和 _，不能为空")
	}
	exists := sp.HasVar(name)
	what := "变量"
	if kind == kindSecrets {
		exists, what = sp.secret(name) != nil, "secret"
	}
	if mode == modeNew && exists {
		f.fail("name", fmt.Sprintf("%s %s 已存在。要修改它，请回到列表点它的编辑或替换", what, name))
	}
	return mode, name, f
}

// itemName 返回编辑页回填的名称：编辑时名称固定，新建时为空（名称在表单值中）。
func itemName(mode itemMode, name string) string {
	if mode == modeEdit {
		return name
	}
	return ""
}

func (d *Dashboard) postVariable(w http.ResponseWriter, r *http.Request, se *session) {
	sc := formScope(r)
	sp, err := d.loadScope(r.Context(), sc.Account, sc.Repo, sc.Environment)
	if err != nil {
		d.scopeError(w, r, err)
		return
	}
	mode, name, f := itemForm(r, sp, kindVariables)
	value := r.PostFormValue("value")
	f.Values["value"] = value
	if !f.Failed() {
		if err := d.admin.SetVariable(r.Context(), sc, name, value, se.Login); err != nil {
			f.fail(formGeneral, "保存变量失败："+d.failure(r, err))
		}
	}
	if f.Failed() {
		a := &itemArgs{Account: sc.Account, Repo: sc.Repo, Env: sc.Environment, Kind: kindVariables, Name: itemName(mode, name), Form: f}
		d.renderItem(w, r, a, http.StatusUnprocessableEntity)
		return
	}
	msg := "已保存变量 " + name
	if mode == modeNew {
		msg = "已创建变量 " + name
	}
	d.flashBack(w, r, scopeBack(sc), msg)
}

func (d *Dashboard) postVariableDelete(w http.ResponseWriter, r *http.Request, se *session) {
	sc := formScope(r)
	name := r.PostFormValue("name")
	if err := d.admin.DeleteVariable(r.Context(), sc, name, se.Login); err != nil {
		d.flashBack(w, r, scopeBack(sc), "删除变量失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, scopeBack(sc), "已删除变量 "+name)
}

// postSecret 新建（mode=new）或替换 secret。替换时值为空（或只有空白）不做任何修改，已有的值保持不变；新建时值不能为空。
// 值从不写入表单状态，校验失败后重新渲染的页面中密码框为空。
func (d *Dashboard) postSecret(w http.ResponseWriter, r *http.Request, se *session) {
	sc := formScope(r)
	sp, err := d.loadScope(r.Context(), sc.Account, sc.Repo, sc.Environment)
	if err != nil {
		d.scopeError(w, r, err)
		return
	}
	mode, name, f := itemForm(r, sp, kindSecrets)
	value := r.PostFormValue("value")
	blank := strings.TrimSpace(value) == ""
	if blank && mode != modeNew && !f.Failed() {
		d.flashBack(w, r, scopeBack(sc), fmt.Sprintf("没有填写新值，secret %s 保持不变", name))
		return
	}
	switch {
	case blank:
		f.fail("value", "请填写 secret 的值")
	case f.Failed():
		f.fail("value", "为了安全，刚才输入的值没有保留，请重新输入")
	}
	if !f.Failed() {
		if err := d.admin.SetSecret(r.Context(), sc, name, value, se.Login); err != nil {
			f.fail(formGeneral, "保存 secret 失败："+d.failure(r, err))
		}
	}
	if f.Failed() {
		a := &itemArgs{Account: sc.Account, Repo: sc.Repo, Env: sc.Environment, Kind: kindSecrets, Name: itemName(mode, name), Form: f}
		d.renderItem(w, r, a, http.StatusUnprocessableEntity)
		return
	}
	d.flashBack(w, r, scopeBack(sc), fmt.Sprintf("已保存 secret %s（值不会再显示）", name))
}

func (d *Dashboard) postSecretDelete(w http.ResponseWriter, r *http.Request, se *session) {
	sc := formScope(r)
	name := r.PostFormValue("name")
	if err := d.admin.DeleteSecret(r.Context(), sc, name, se.Login); err != nil {
		d.flashBack(w, r, scopeBack(sc), "删除 secret 失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, scopeBack(sc), "已删除 secret "+name)
}

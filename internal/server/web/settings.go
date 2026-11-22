package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 设置（只有管理员能访问，PLAN #16.4）。按任务分成几个分区，每个分区是一个页面，左侧（窄屏时在顶部）是分区导航：
//
//   - 连接与发布凭据 /settings：每个账号一张 Docker Hub 连接卡片和一张 npm 配置卡片，编辑页 /settings/docker（见 settings_docker.go）
//     和 /settings/npm（见 settings_npm.go）。
//   - 变量与密钥 /settings/variables：选择作用域；作用域配置页 /settings/scope 及变量、secret 的新建和编辑页（见 settings_scope.go）。
//   - 环境 /settings/environments：environment 列表；新建页 /settings/environments/new。
//   - 仓库 /settings/repos：可搜索、分页的仓库列表，也是新建仓库级 environment 时的仓库选择器。
//   - 运维 /settings/ops：版本信息和 outbox。
//
// 写操作经 admin.Service（与 CLI 共用同一套存储和审计），操作者记为登录用户。
// 表单校验失败时重新渲染表单（HTTP 422），字段旁显示错误并保留非敏感的输入；成功后重定向并带上提示。
// secret 只写不读：页面只列出名称和更新信息，从不查询或渲染 secret 的值；secret 的值和访问令牌只出现在 POST 表单正文中，
// 不进入 URL、提示信息、错误信息、日志和重新渲染的页面。

// 设置分区（分区导航中高亮的一项）。
const (
	sectionConn  = "conn"
	sectionVars  = "vars"
	sectionEnvs  = "envs"
	sectionRepos = "repos"
	sectionOps   = "ops"
)

// settingsOutboxLimit 是运维分区列出的 outbox 记录数上限。
const settingsOutboxLimit = 100

// reposPageSize 是仓库分区每页的仓库数。
const reposPageSize = 50

// settingsBase 是所有设置页共有的数据。
type settingsBase struct {
	// Section 是当前所在的分区（sectionConn 等）。
	Section string
}

// repoView 是仓库分区中的一个仓库。
type repoView struct {
	FullName, DefaultBranch string
	Private, Active         bool
	InstallationID          int64
	// 外部设计文档的简要状态：是否绑定、验证状态（空表示尚未验证）、是否配了待决策清单，
	// 以及外部文档页的地址。停用的仓库同样有 DocsURL（只读入口保留）。
	DocBound       bool
	DocCheckStatus string
	DecisionDoc    bool
	DocsURL        string
}

// envView 是一个 environment 及其同名覆盖关系。
type envView struct {
	*apiv1.Environment
	// OverriddenIn 只用于账号级 environment：定义了同名仓库级 environment 的仓库。
	// 这些仓库的 job 只使用仓库级的那一份，看不到这里的配置、变量和 secret。
	OverriddenIn []string
	// Overrides 表示这个仓库级 environment 整体取代了同名的账号级 environment。
	Overrides bool
	// URL 是这个 environment 的配置页。
	URL string
	// Docker 表示这是 Docker Hub 连接使用的账号级 environment。
	Docker bool
}

// envViews 为 environment 列表标出同名覆盖关系：仓库级 environment 整体取代所属账号下同名的账号级 environment。
func envViews(list []*apiv1.Environment) []envView {
	out := make([]envView, 0, len(list))
	for _, e := range list {
		v := envView{
			Environment: e,
			URL:         scopeURL(e.Account, e.Repo, e.Name),
			Docker:      e.Repo == "" && strings.EqualFold(e.Name, admin.DockerEnvName),
		}
		for _, o := range list {
			if !strings.EqualFold(o.Name, e.Name) {
				continue
			}
			switch {
			case e.Repo == "" && o.Repo != "" && strings.EqualFold(repoOwner(o.Repo), e.Account):
				v.OverriddenIn = append(v.OverriddenIn, o.Repo)
			case e.Repo != "" && o.Repo == "" && strings.EqualFold(repoOwner(e.Repo), o.Account):
				v.Overrides = true
			}
		}
		out = append(out, v)
	}
	return out
}

// repoOwner 返回 owner/name 中的 owner。
func repoOwner(fullName string) string {
	owner, _, _ := strings.Cut(fullName, "/")
	return owner
}

// scopeQuery 返回作用域的查询参数：repo 非空时是仓库级，否则是账号级；env 非空时是其中的 environment。
func scopeQuery(account, repo, env string) url.Values {
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	} else {
		q.Set("account", account)
	}
	if env != "" {
		q.Set("env", env)
	}
	return q
}

// scopeURL 返回作用域配置页的地址。
func scopeURL(account, repo, env string) string {
	return "/settings/scope?" + scopeQuery(account, repo, env).Encode()
}

// splitList 把以换行、逗号或空白分隔的列表拆开，去掉空项。
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' })
}

// formScope 读取表单中的作用域（account、repo、env）。
func formScope(r *http.Request) *apiv1.Scope {
	return &apiv1.Scope{
		Account:     strings.TrimSpace(r.PostFormValue("account")),
		Repo:        strings.TrimSpace(r.PostFormValue("repo")),
		Environment: strings.TrimSpace(r.PostFormValue("env")),
	}
}

func scopeBack(sc *apiv1.Scope) string { return scopeURL(sc.Account, sc.Repo, sc.Environment) }

// accounts 返回已安装 App 的账号。
func (d *Dashboard) accounts(ctx context.Context) ([]string, error) {
	rows, err := d.db.Query(ctx, `SELECT DISTINCT account_login FROM installations WHERE NOT deleted ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// findAccount 返回已安装 App 的账号中与 name 相同（不区分大小写）的那个，没有时返回空字符串。
func (d *Dashboard) findAccount(ctx context.Context, name string) (string, error) {
	accounts, err := d.accounts(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range accounts {
		if strings.EqualFold(a, name) {
			return a, nil
		}
	}
	return "", nil
}

// connPage 是"连接与发布凭据"分区的数据。
type connPage struct {
	settingsBase
	Docker []*dockerView
	Npm    []*npmView
}

func (d *Dashboard) pageSettings(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	accounts, err := d.accounts(ctx)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &connPage{Section: sectionConn}
	for _, a := range accounts {
		v, err := d.dockerStatus(ctx, a)
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		p.Docker = append(p.Docker, v)
		n, err := d.npmStatus(ctx, a)
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		p.Npm = append(p.Npm, n)
	}
	d.render(w, r, "settings", "设置 · 连接与发布凭据", p)
}

// repoSearch 是一页仓库搜索结果（服务端搜索、分页），仓库分区和"启动构建"的仓库选择器共用。
type repoSearch struct {
	Repos []repoView
	// Query 是搜索词（不区分大小写的子串匹配）。
	Query       string
	Page, Pages int
	Total       int
	// PrevURL / NextURL 是上一页和下一页的地址（没有时为空）。
	PrevURL, NextURL string
}

// searchRepos 按搜索词 query 返回第 page 页（从 1 开始，超出时取最后一页）的仓库。activeOnly 为 true 时不列出已停用的仓库。
// pageURL 返回第 n 页的地址，用于上一页和下一页的链接。
func (d *Dashboard) searchRepos(ctx context.Context, query string, page int, activeOnly bool, pageURL func(n int) string) (*repoSearch, error) {
	s := &repoSearch{Query: query, Page: max(page, 1)}
	where := `strpos(lower(owner||'/'||name), lower($1)) > 0`
	if activeOnly {
		where += ` AND active`
	}
	if err := d.db.QueryRow(ctx, `SELECT count(*) FROM repos WHERE `+where, query).Scan(&s.Total); err != nil {
		return nil, err
	}
	s.Pages = max(1, (s.Total+reposPageSize-1)/reposPageSize)
	s.Page = min(s.Page, s.Pages)
	rows, err := d.db.Query(ctx, `SELECT owner||'/'||name, default_branch, private, active, installation_id FROM repos WHERE `+where+`
		ORDER BY 1 LIMIT $2 OFFSET $3`, query, reposPageSize, (s.Page-1)*reposPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r repoView
		if err := rows.Scan(&r.FullName, &r.DefaultBranch, &r.Private, &r.Active, &r.InstallationID); err != nil {
			return nil, err
		}
		s.Repos = append(s.Repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.Page > 1 {
		s.PrevURL = pageURL(s.Page - 1)
	}
	if s.Page < s.Pages {
		s.NextURL = pageURL(s.Page + 1)
	}
	return s, nil
}

// reposPage 是仓库分区的数据。
type reposPage struct {
	settingsBase
	*repoSearch
	// Pick 非空时页面是选择器（"env"：为新建仓库级 environment 选择仓库）。
	Pick string
	// Sync 是最近一次"刷新仓库"的结果（从没有刷新过时为 nil）。
	Sync *store.SyncStatus
}

// reposURL 返回仓库分区第 page 页的地址，保留搜索词和选择器模式。
func reposURL(query, pick string, page int) string {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if pick != "" {
		q.Set("pick", pick)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/settings/repos"
	}
	return "/settings/repos?" + q.Encode()
}

func (d *Dashboard) pageRepos(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	p := &reposPage{Section: sectionRepos}
	if q.Get("pick") == "env" {
		p.Pick = "env"
	}
	query := strings.TrimSpace(q.Get("q"))
	page, _ := strconv.Atoi(q.Get("page"))
	var err error
	p.repoSearch, err = d.searchRepos(r.Context(), query, page, false, func(n int) string { return reposURL(query, p.Pick, n) })
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.Sync, err = store.LoadSyncStatus(r.Context(), d.db); err != nil {
		d.serverError(w, r, err)
		return
	}
	if err := d.loadRepoDocStatuses(r.Context(), p.Repos); err != nil {
		d.serverError(w, r, err)
		return
	}
	d.render(w, r, "settings_repos", "设置 · 仓库", p)
}

// scopeCount 是"变量与密钥"分区中的一个作用域及其中的变量数和 secret 数。
type scopeCount struct {
	Name, URL     string
	Vars, Secrets int
	// Docker 表示这是 Docker Hub 连接使用的账号级 environment。
	Docker bool
}

// varsIndexPage 是"变量与密钥"分区的数据：账号级作用域、已有变量或 secret 的仓库，以及 environment。
type varsIndexPage struct {
	settingsBase
	Accounts, Repos, Envs []scopeCount
}

// 变量与密钥分区的统计查询：账号级作用域（每个已安装的账号）和有变量或 secret 的仓库。
const (
	accountCountsSQL = `SELECT i.account_login, coalesce(v.n, 0)::int, coalesce(s.n, 0)::int
		FROM (SELECT DISTINCT ON (account_login) account_login, account_id FROM installations WHERE NOT deleted ORDER BY account_login, id) i
		LEFT JOIN (SELECT scope_id, count(*) n FROM variables WHERE scope_kind='owner' GROUP BY 1) v ON v.scope_id=i.account_id
		LEFT JOIN (SELECT scope_id, count(*) n FROM secrets WHERE scope_kind='owner' GROUP BY 1) s ON s.scope_id=i.account_id
		ORDER BY 1`
	repoCountsSQL = `SELECT r.owner||'/'||r.name, coalesce(v.n, 0)::int, coalesce(s.n, 0)::int FROM repos r
		LEFT JOIN (SELECT scope_id, count(*) n FROM variables WHERE scope_kind='repo' GROUP BY 1) v ON v.scope_id=r.id
		LEFT JOIN (SELECT scope_id, count(*) n FROM secrets WHERE scope_kind='repo' GROUP BY 1) s ON s.scope_id=r.id
		WHERE r.active AND (v.n IS NOT NULL OR s.n IS NOT NULL) ORDER BY 1`
)

// scopeCounts 执行 accountCountsSQL 或 repoCountsSQL，urlFor 返回每个作用域的配置页地址。
func (d *Dashboard) scopeCounts(ctx context.Context, sql string, urlFor func(name string) string) ([]scopeCount, error) {
	rows, err := d.db.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (scopeCount, error) {
		var c scopeCount
		err := row.Scan(&c.Name, &c.Vars, &c.Secrets)
		c.URL = urlFor(c.Name)
		return c, err
	})
}

func (d *Dashboard) pageVarsIndex(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	p := &varsIndexPage{Section: sectionVars}
	var err error
	if p.Accounts, err = d.scopeCounts(ctx, accountCountsSQL, func(n string) string { return scopeURL(n, "", "") }); err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.Repos, err = d.scopeCounts(ctx, repoCountsSQL, func(n string) string { return scopeURL("", n, "") }); err != nil {
		d.serverError(w, r, err)
		return
	}
	envs, err := d.admin.ListEnvironments(ctx, "")
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	for _, e := range envViews(envs) {
		owner := e.Account
		if e.Repo != "" {
			owner = e.Repo
		}
		p.Envs = append(p.Envs, scopeCount{Name: owner + " / " + e.Name, URL: e.URL, Docker: e.Docker})
	}
	d.render(w, r, "settings_vars", "设置 · 变量与密钥", p)
}

// envsPage 是环境分区的数据。
type envsPage struct {
	settingsBase
	Envs []envView
}

func (d *Dashboard) pageEnvs(w http.ResponseWriter, r *http.Request, _ *session) {
	envs, err := d.admin.ListEnvironments(r.Context(), "")
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.render(w, r, "settings_envs", "设置 · 环境", &envsPage{Section: sectionEnvs, Envs: envViews(envs)})
}

// envNewPage 是新建 environment 页的数据。
type envNewPage struct {
	settingsBase
	Accounts []string
	// Repo 是从仓库选择器选中的仓库（新建仓库级 environment），为空时只能选账号级。
	Repo string
	Form *formState
}

func (d *Dashboard) pageEnvNew(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	f := newForm()
	repo, account := strings.TrimSpace(q.Get("repo")), strings.TrimSpace(q.Get("account"))
	switch {
	case repo != "":
		f.Values["scope"] = "repo:" + repo
	case account != "":
		f.Values["scope"] = "account:" + account
	}
	d.renderEnvNew(w, r, repo, f, http.StatusOK)
}

func (d *Dashboard) renderEnvNew(w http.ResponseWriter, r *http.Request, repo string, f *formState, status int) {
	accounts, err := d.accounts(r.Context())
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &envNewPage{Section: sectionEnvs, Accounts: accounts, Repo: repo, Form: f}
	d.renderStatus(w, r, "settings_env_new", "设置 · 新建 environment", p, status)
}

// opsPage 是运维分区的数据。
type opsPage struct {
	settingsBase
	App, PublicURL, Version string
	Outbox                  []*outbox.Item
}

func (d *Dashboard) pageOps(w http.ResponseWriter, r *http.Request, _ *session) {
	outbox, err := d.admin.ListOutbox(r.Context(), []string{store.OutboxPending, store.OutboxDead}, settingsOutboxLimit)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.render(w, r, "settings_ops", "设置 · 运维", &opsPage{
		Section: sectionOps,
		App:     d.appSlug(), PublicURL: d.cfg.PublicURL, Version: buildinfo.Version(), Outbox: outbox,
	})
}

// envFormValues 把 environment 的配置填进表单（编辑页的初始值）。
func envFormValues(f *formState, e *apiv1.Environment) {
	f.Values["ref_policy"] = strings.Join(e.RefPolicy, "\n")
	f.Values["allowed_publish_repos"] = strings.Join(e.AllowedPublishRepos, ", ")
}

// readEnvForm 读取并校验 environment 的保护规则字段，输入值记入 f。
func readEnvForm(r *http.Request, f *formState) *apiv1.Environment {
	for _, k := range []string{"ref_policy", "allowed_publish_repos"} {
		f.Values[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	publish := splitList(f.Values["allowed_publish_repos"])
	for _, p := range publish {
		if !admin.ValidRepoFullName(p) {
			f.fail("allowed_publish_repos", "每个仓库都应写成 owner/name："+p)
		}
	}
	return &apiv1.Environment{
		RefPolicy: splitList(f.Values["ref_policy"]), AllowedPublishRepos: publish,
	}
}

// postEnvironment 新建（create=1）或修改 environment。新建时的作用域来自 scope（account:<登录名> 或 repo:<owner/name>），
// 修改时来自 account / repo。校验失败时重新渲染新建页或作用域配置页。
func (d *Dashboard) postEnvironment(w http.ResponseWriter, r *http.Request, se *session) {
	if r.PostFormValue("create") == "1" {
		d.createEnvironment(w, r, se)
		return
	}
	ctx := r.Context()
	sc := formScope(r)
	sc.Environment = strings.TrimSpace(r.PostFormValue("name"))
	f := newForm()
	e := readEnvForm(r, f)
	e.Name, e.Account, e.Repo = sc.Environment, sc.Account, sc.Repo
	if !f.Failed() {
		if err := d.admin.UpsertEnvironment(ctx, e, se.Login); err != nil {
			f.fail(formGeneral, "保存 environment 失败："+d.failure(r, err))
		}
	}
	if f.Failed() {
		d.renderScope(w, r, &scopeArgs{Account: sc.Account, Repo: sc.Repo, Env: sc.Environment, EnvForm: f}, http.StatusUnprocessableEntity)
		return
	}
	d.flashBack(w, r, scopeBack(sc), "已保存 environment "+sc.Environment+" 的保护规则")
}

func (d *Dashboard) createEnvironment(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	f := newForm()
	f.Values["scope"] = r.PostFormValue("scope")
	f.Values["name"] = strings.TrimSpace(r.PostFormValue("name"))
	var sc apiv1.Scope
	kind, target, _ := strings.Cut(f.Values["scope"], ":")
	switch kind {
	case "account":
		sc.Account = target
	case "repo":
		sc.Repo = target
	default:
		f.fail("scope", "请选择 environment 的作用域")
	}
	sc.Environment = f.Values["name"]
	if sc.Environment == "" {
		f.fail("name", "请填写 environment 名称")
	}
	e := readEnvForm(r, f)
	e.Name, e.Account, e.Repo = sc.Environment, sc.Account, sc.Repo
	if !f.Failed() {
		_, err := store.ResolveScope(ctx, d.db, sc.Account, sc.Repo, sc.Environment)
		switch {
		case err == nil:
			f.fail("name", fmt.Sprintf("environment %s 已存在于这个作用域，请在它的配置页修改", sc.Environment))
		case errors.Is(err, store.ErrEnvironmentNotFound):
			// 账号或仓库存在、environment 还没有：创建
			if err := d.admin.UpsertEnvironment(ctx, e, se.Login); err != nil {
				f.fail(formGeneral, "创建 environment 失败："+d.failure(r, err))
			}
		default:
			// 账号或仓库无效（ResolveScope 的说明可以直接显示，与 UpsertEnvironment 报告的相同）
			f.fail(formGeneral, "创建 environment 失败："+d.failure(r, err))
		}
	}
	if f.Failed() {
		d.renderEnvNew(w, r, sc.Repo, f, http.StatusUnprocessableEntity)
		return
	}
	d.flashBack(w, r, scopeBack(&sc), "已创建 environment "+sc.Environment)
}

func (d *Dashboard) postEnvironmentDelete(w http.ResponseWriter, r *http.Request, se *session) {
	sc := formScope(r)
	if err := d.admin.DeleteEnvironment(r.Context(), sc, se.Login); err != nil {
		d.flashBack(w, r, scopeBack(sc), "删除 environment 失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, "/settings/environments", fmt.Sprintf("已删除 environment %s 及其中的变量和 secret", sc.Environment))
}

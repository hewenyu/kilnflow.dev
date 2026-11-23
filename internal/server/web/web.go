// Package web 实现 kiln-server 的 Dashboard 服务端（PLAN #19、#25.1）：页面地址、登录、权限、表单处理和页面数据。
// 界面是仓库根目录 web/ 中的 React 单页应用（构建产物内嵌在 ui/dist/），页面数据以 JSON 提供，见 ui.go。
//
// 管理操作（取消、重跑、Runner 管理等）通过 admin.Service 执行，与 CLI 共用；
// 页面展示的数据直接通过 store 查询。
package web

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 文件布局：
//   - web.go：Dashboard、渲染页面数据、中间件和路由；ui.go：前端（index.html、静态文件）和 JSON 模式；json.go：页面数据的 JSON 编码；
//     funcs.go：少量共用的文字；form.go：表单状态；errors.go：错误页。
//   - session.go：登录、会话和登录后的跳转；account.go：登录与身份（绑定 GitHub 账号）、成员、活动；actions.go：run、job、Runner、outbox 和仓库的表单操作。
//   - runs.go：运行列表和启动构建；run_detail.go、job_detail.go：run 和 job 详情页（日志、产物下载）。
//   - merge_queue.go：合并队列页、入队和移除，以及 run 详情页上 PR 的队列状态。
//   - ai_requests.go：AI 请求和 AI 任务页（日志、审计和开发报告、取消和重跑、旧链接的跳转）；ai_runners.go：AI 执行节点页面。
//   - ai_dev.go：AI 开发页（工作项列表、任务池和详情）、同步任务池、暂停 / 恢复、终止和重试。
//   - runners.go：CI Runner 页面和注册新的 Runner（CI 和 AI）；runner_cache.go：Runner 的本地缓存页和清理请求。
//   - settings.go：设置分区页面和 environment；settings_scope.go：作用域配置页、变量和 secret；settings_npm.go：账号级 npm 配置；settings_docker.go：Docker Hub 连接；
//     settings_upgrade.go：Runner 升级（registry、每台 Runner 的版本一览）。
//   - runner_detail.go：Runner 详情页和按台升级。

// pageNavs 是页面在顶部导航中高亮的一项（不在表中的页面高亮与页面同名的一项）。
var pageNavs = map[string]string{
	"run_new": "runs", "run": "runs", "job": "runs", "runner_new": "runners", "runner_cache": "runners", "runner": "runners",
	"settings_docker": "settings", "settings_npm": "settings", "settings_vars": "settings", "settings_scope": "settings", "settings_item": "settings",
	"settings_envs": "settings", "settings_env_new": "settings", "settings_repos": "settings", "settings_repo_docs": "settings", "settings_ops": "settings",
	"settings_account": "settings", "settings_members": "settings", "settings_activity": "settings", "settings_upgrade": "settings",
	"ai_dev_item": "ai_dev", "ai_request": "ai_requests", "ai_task": "ai_requests", "ai_runner": "ai_runners",
}

// GitHub 的地址。
const (
	// githubWebURL 是 GitHub 网站（OAuth 授权和换取令牌、页面上指向 GitHub 的链接）。
	githubWebURL = "https://github.com"
	// githubAPIURL 是 Config.GitHubAPIURL 为空时使用的 GitHub API。
	githubAPIURL = "https://api.github.com"
)

// Config 是 Dashboard 的配置。
type Config struct {
	// PublicURL 是 Server 的公网地址（OAuth 回调、Runner 注册命令；https 时 Cookie 加 Secure）。
	PublicURL string
	// GitHubClientID / GitHubClientSecret 是 GitHub OAuth 登录使用的 App 凭据；ClientID 为空时只能用 admin token 登录。
	GitHubClientID     string
	GitHubClientSecret string
	// GitHubAPIURL 是 GitHub API 地址（空表示 api.github.com）。
	GitHubAPIURL string
	// Admins / Users 是可以登录 Dashboard 的 GitHub 登录名或 ID（小写）。
	Admins map[string]bool
	Users  map[string]bool
	// LeaseTTL 是 Runner 租约的有效期（Runners 页面说明吊销后正在执行的任务多久被判为失联）；0 表示默认的 60 秒。
	LeaseTTL time.Duration
	// AcquireLongPoll 是 Runner 一轮领取（长轮询）的最长时间：暂停接单在 Runner 正在进行的一轮领取结束后才生效；0 表示默认的 25 秒。
	AcquireLongPoll time.Duration
}

// Deps 是 Dashboard 的依赖。
type Deps struct {
	DB    *store.DB
	Admin *admin.Service
	// AI 是 AI 领域的只读视图（请求、任务、工作项的设计依据）。
	AI *ai.Core
	// Keys 签名会话、CSRF token 和 OAuth state，并校验 admin token 登录。
	Keys *auth.Keys
	Log  *slog.Logger
	// AppSlug 返回 GitHub App 的 slug（未知时为空），显示在设置页。
	AppSlug func() string
}

// Dashboard 是 Dashboard 的 HTTP 处理器集合，用 Register 注册路由。
type Dashboard struct {
	cfg     Config
	db      *store.DB
	admin   *admin.Service
	ai      *ai.Core
	keys    *auth.Keys
	log     *slog.Logger
	appSlug func() string
	// httpClient 是访问外部服务（GitHub OAuth）使用的 HTTP 客户端。
	httpClient *http.Client
	// githubURL 是 GitHub 网站的地址（OAuth 授权和换取令牌），测试中替换为模拟服务器。
	githubURL string
	// reveals 保存刚生成、等待一次性展示的 Runner 注册令牌（见 runners.go）。
	reveals *enrollReveals
	// gzipWriters 复用页面数据压缩用的 gzip.Writer（见 ui.go 的 writeJSON）。
	gzipWriters sync.Pool
	// assetsMu 保护 assetsGz：内嵌静态资产不可变，按文件名懒压缩一次后复用（见 ui.go 的 serveAsset）。
	assetsMu sync.Mutex
	assetsGz map[string][]byte
	// ui 是前端构建产物的文件系统；nil 时用内嵌的 uiFS（测试替换为内存文件）。
	ui fs.FS
}

// New 返回 Dashboard。
func New(cfg Config, deps Deps) (*Dashboard, error) {
	return &Dashboard{
		cfg: cfg, db: deps.DB, admin: deps.Admin, ai: deps.AI, keys: deps.Keys, log: deps.Log, appSlug: deps.AppSlug,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		githubURL:  githubWebURL,
		reveals:    newEnrollReveals(),
	}, nil
}

// render 渲染页面。先渲染到缓冲区，出错时返回 500 而不是半个页面。title 是原样显示的标题（还没有迁移成消息的页面）。
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	d.renderPage(w, r, page, rawText(title), data, http.StatusOK)
}

// renderStatus 与 render 相同，使用状态码 status（例如表单校验失败时的 422）。
func (d *Dashboard) renderStatus(w http.ResponseWriter, r *http.Request, page, title string, data any, status int) {
	d.renderPage(w, r, page, rawText(title), data, status)
}

// renderPage 渲染页面并使用状态码 status，title 是消息或原样的文字（见 text.go）：前端请求返回页面数据（JSON）；
// 浏览器直接打开（例如下载地址或 GitHub 登录回调出错）返回内嵌了这份页面数据的 index.html，前端直接显示，不再请求一次。
func (d *Dashboard) renderPage(w http.ResponseWriter, r *http.Request, page string, title uiText, data any, status int) {
	se := d.getSession(r)
	nav := page
	if n := pageNavs[page]; n != "" {
		nav = n
	}
	p := &uiPage{
		Page: page, Title: title, Nav: nav, Status: status, Session: newUISession(se),
		OAuth: d.oauthEnabled(), Flash: parseFlash(r.URL.Query().Get("flash")), Data: data,
	}
	if se != nil {
		p.CSRF = d.csrfToken(se)
		// 页面处理器已经算过活跃 run 数时（总览）直接复用，不再查一次。
		if n, ok := activeRunsFrom(r.Context()); ok {
			p.Active = n
		} else {
			p.Active = d.activeRuns(r.Context())
		}
	}
	if isUI(r) {
		d.writeJSON(w, r, status, p)
		return
	}
	d.serveIndexWith(w, status, p)
}

// web 包装需要登录的页面；adminOnly 为 true 时要求管理员；POST 请求还检查 CSRF。
// 未登录（或会话已过期）时跳转到登录页，登录后回到原来的页面：GET 回到这个地址；POST 不能重放，回到提交表单的页面（同源的 Referer）。
// 权限不足和 CSRF 失败显示统一的错误页（403）。
func (d *Dashboard) web(adminOnly bool, h func(w http.ResponseWriter, r *http.Request, se *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se, reason, err := d.sessionState(r)
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		if se == nil {
			d.redirect(w, r, loginURL(returnTo(r), reason))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, se))
		if adminOnly && !se.Admin() {
			d.forbidden(w, r, se)
			return
		}
		if r.Method == http.MethodPost && !d.validCSRF(r, se) {
			d.csrfFailed(w, r)
			return
		}
		h(w, r, se)
	}
}

// webAPI 包装页面脚本调用的只读接口：未登录时返回 401（而不是跳转到登录页，正文是消息的 JSON），脚本据此提示重新登录。
func (d *Dashboard) webAPI(h func(w http.ResponseWriter, r *http.Request, se *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se := d.getSession(r)
		if se == nil {
			apiError(w, http.StatusUnauthorized, message(msgAPIUnauthorized))
			return
		}
		h(w, r, se)
	}
}

// Register 在 mux 上注册 Dashboard 的全部路由。
func (d *Dashboard) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", d.page(d.web(false, d.pageOverview)))
	// 没有注册的地址显示统一的 404 页面（不需要登录，页面中没有任何数据）
	// 浏览器打开时返回状态码为 404 的 index.html，前端取数据后显示
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { d.notFound(w, r, "") })
	// 前端构建产物：带内容摘要的脚本和样式，以及图标
	mux.HandleFunc("GET /assets/{file...}", d.serveAsset)
	mux.HandleFunc("GET /favicon.svg", d.serveAsset)
	mux.HandleFunc("GET /login", d.page(d.pageLogin))
	mux.HandleFunc("POST /login", d.handleLogin)
	mux.HandleFunc("POST /logout", d.handleLogout)
	mux.HandleFunc("GET /auth/github", d.handleOAuthStart)
	mux.HandleFunc("GET /auth/github/callback", d.handleOAuthCallback)

	mux.HandleFunc("GET /runs", d.page(d.web(false, d.pageRuns)))
	mux.HandleFunc("GET /runs/new", d.page(d.web(false, d.pageRunNew)))
	mux.HandleFunc("POST /runs/new", d.web(false, d.postRunNewLoad))
	mux.HandleFunc("GET /runs/{id}", d.page(d.web(false, d.pageRun)))
	mux.HandleFunc("POST /runs/{id}/cancel", d.web(false, d.postCancelRun))
	mux.HandleFunc("POST /runs/{id}/rerun", d.web(false, d.postRerun))
	mux.HandleFunc("GET /jobs/{id}", d.page(d.web(false, d.pageJob)))
	mux.HandleFunc("GET /jobs/{id}/log", d.webAPI(d.getLog))
	mux.HandleFunc("GET /jobs/{id}/log/download", d.web(false, d.getLogDownload))
	mux.HandleFunc("GET /jobs/{id}/download", d.web(false, d.getOutput))
	mux.HandleFunc("POST /jobs/{id}/rerun", d.web(false, d.postRerunJob))
	mux.HandleFunc("GET "+mergeQueuePath, d.page(d.web(false, d.pageMergeQueue)))
	mux.HandleFunc("POST "+mergeQueuePath+"/add", d.web(true, d.postMergeQueueAdd))
	mux.HandleFunc("POST "+mergeQueuePath+"/remove", d.web(true, d.postMergeQueueRemove))
	mux.HandleFunc("POST "+mergeQueuePath+"/autotag/retry", d.web(true, d.postAutoTagRetry))
	mux.HandleFunc("GET "+aiDevPath, d.page(d.web(false, d.pageAIDev)))
	mux.HandleFunc("GET "+aiDevPath+"/{id}", d.page(d.web(false, d.pageAIDevItem)))
	mux.HandleFunc("GET "+aiDevPath+"/{id}/docs/content", d.docsContent(d.getWorkItemDocContent))
	mux.HandleFunc("POST "+aiDevPath+"/sync", d.web(true, d.postAIDevSync))
	mux.HandleFunc("POST "+aiDevPath+"/pause", d.web(true, d.postAIDevPause))
	mux.HandleFunc("POST "+aiDevPath+"/resume", d.web(true, d.postAIDevResume))
	mux.HandleFunc("POST "+aiDevPath+"/stop", d.web(true, d.postAIDevStop))
	mux.HandleFunc("POST "+aiDevPath+"/retry", d.web(true, d.postAIDevRetry))
	mux.HandleFunc("GET "+aiRequestsPath, d.page(d.web(false, d.pageAIRequests)))
	mux.HandleFunc("GET "+aiRequestsPath+"/{id}", d.page(d.web(false, d.pageAIRequest)))
	mux.HandleFunc("GET "+aiRequestsPath+"/{id}/docs/content", d.docsContent(d.getRequestDocContent))
	mux.HandleFunc("POST "+aiRequestsPath+"/{id}/cancel", d.web(false, d.postCancelAIRequest))
	mux.HandleFunc("POST "+aiRequestsPath+"/{id}/rerun", d.web(false, d.postRerunAIRequest))
	mux.HandleFunc("GET /ai/tasks/{id}", d.page(d.web(false, d.pageAITask)))
	mux.HandleFunc("GET /ai/tasks/{id}/docs/content", d.docsContent(d.getTaskDocContent))
	mux.HandleFunc("GET /ai/tasks/{id}/log", d.webAPI(d.getAITaskLog))
	mux.HandleFunc("GET /ai/tasks/{id}/log/download", d.web(false, d.getAITaskLogDownload))
	mux.HandleFunc("POST /ai/tasks/{id}/rerun", d.web(false, d.postRerunAITask))
	mux.HandleFunc("GET "+aiRunnersPath, d.page(d.web(false, d.pageAIRunners)))
	mux.HandleFunc("GET "+aiRunnersPath+"/{name}", d.page(d.web(false, d.pageAIRunner)))
	mux.HandleFunc("GET /runners", d.page(d.web(false, d.pageRunners)))
	mux.HandleFunc("GET /runners/new", d.page(d.web(true, d.pageRunnerNew)))
	mux.HandleFunc("GET /runners/enroll/{id}", d.page(d.web(true, d.pageRunnerEnroll)))
	mux.HandleFunc("POST /runners/token", d.web(true, d.postRunnerToken))
	mux.HandleFunc("POST /runners/{name}/{op}", d.web(true, d.postRunnerOp))
	// Runner 详情页（PLAN #27.9）不用 /runners/{name}：名为 new 的 Runner 会被注册页占用
	mux.HandleFunc("GET /runners/detail/{name}", d.page(d.web(false, d.pageRunner)))
	// 升级操作不用 /runners/{name}/upgrade/{op}：会与 /runners/cache/{name}/purge 冲突
	mux.HandleFunc("POST /runners/upgrade/{name}/{op}", d.web(true, d.postRunnerUpgradeOp))
	// 缓存页不用 /runners/{name}/cache：会与 /runners/enroll/{id} 冲突
	mux.HandleFunc("GET /runners/cache/{name}", d.page(d.web(false, d.pageRunnerCache)))
	mux.HandleFunc("POST /runners/cache/{name}/purge", d.web(true, d.postRunnerCachePurge))
	mux.HandleFunc("GET /settings", d.page(d.web(true, d.pageSettings)))
	mux.HandleFunc("GET /settings/docker", d.page(d.web(true, d.pageDocker)))
	mux.HandleFunc("POST /settings/docker", d.web(true, d.postDocker))
	mux.HandleFunc("POST /settings/docker/delete", d.web(true, d.postDockerDelete))
	mux.HandleFunc("GET /settings/npm", d.page(d.web(true, d.pageNpm)))
	mux.HandleFunc("POST /settings/npm", d.web(true, d.postNpm))
	mux.HandleFunc("POST /settings/npm/delete", d.web(true, d.postNpmDelete))
	mux.HandleFunc("GET "+runnerUpgradeURL, d.page(d.web(true, d.pageRunnerUpgrade)))
	mux.HandleFunc("POST "+runnerUpgradeURL+"/registry", d.web(true, d.postRunnerUpgradeRegistry))
	mux.HandleFunc("POST "+runnerUpgradeURL+"/check", d.web(true, d.postRunnerUpgradeCheck))
	mux.HandleFunc("GET /settings/variables", d.page(d.web(true, d.pageVarsIndex)))
	mux.HandleFunc("GET /settings/scope", d.page(d.web(true, d.pageScope)))
	mux.HandleFunc("GET /settings/variables/new", d.page(d.web(true, d.pageItem(kindVariables, false))))
	mux.HandleFunc("GET /settings/variables/edit", d.page(d.web(true, d.pageItem(kindVariables, true))))
	mux.HandleFunc("POST /settings/variables", d.web(true, d.postVariable))
	mux.HandleFunc("POST /settings/variables/delete", d.web(true, d.postVariableDelete))
	mux.HandleFunc("GET /settings/secrets/new", d.page(d.web(true, d.pageItem(kindSecrets, false))))
	mux.HandleFunc("GET /settings/secrets/edit", d.page(d.web(true, d.pageItem(kindSecrets, true))))
	mux.HandleFunc("POST /settings/secrets", d.web(true, d.postSecret))
	mux.HandleFunc("POST /settings/secrets/delete", d.web(true, d.postSecretDelete))
	mux.HandleFunc("GET /settings/environments", d.page(d.web(true, d.pageEnvs)))
	mux.HandleFunc("GET /settings/environments/new", d.page(d.web(true, d.pageEnvNew)))
	mux.HandleFunc("POST /settings/environments", d.web(true, d.postEnvironment))
	mux.HandleFunc("POST /settings/environments/delete", d.web(true, d.postEnvironmentDelete))
	mux.HandleFunc("GET /settings/repos", d.page(d.web(true, d.pageRepos)))
	mux.HandleFunc("GET /settings/repos/docs", d.page(d.web(true, d.pageRepoDocs)))
	mux.HandleFunc("POST /settings/repos/docs/verify", d.web(true, d.postRepoDocsVerify))
	mux.HandleFunc("GET /settings/ops", d.page(d.web(true, d.pageOps)))
	mux.HandleFunc("GET "+accountURL, d.page(d.web(true, d.pageAccount)))
	mux.HandleFunc("POST "+accountURL+"/bind", d.web(true, d.postBindStart))
	mux.HandleFunc("POST "+accountURL+"/unbind", d.web(true, d.postUnbind))
	mux.HandleFunc("POST "+accountURL+"/token-login", d.web(true, d.postTokenLogin))
	mux.HandleFunc("GET "+membersURL, d.page(d.web(true, d.pageMembers)))
	mux.HandleFunc("GET "+activityURL, d.page(d.web(true, d.pageActivity)))
	mux.HandleFunc("POST /dispatch", d.web(false, d.postDispatch))
	mux.HandleFunc("POST /outbox/{id}/retry", d.web(true, d.postOutboxRetry))
	mux.HandleFunc("POST /sync", d.web(true, d.postSync))
	mux.HandleFunc("POST /settings/repos/reconcile", d.web(true, d.postReconcile))
}

// flashBack 重定向到站内页面 to，并带上一条原样显示的提示（还没有迁移成消息的操作；新的操作用 flashMsg）。
// to 由调用方用常量或 ID 拼成，不来自用户输入。
func (d *Dashboard) flashBack(w http.ResponseWriter, r *http.Request, to, msg string) {
	d.redirect(w, r, withFlash(to, msg))
}

// flashMsg 重定向到站内页面 to，并带上一条消息作为提示（前端按当前语言显示）。消息必须登记为 flash（见 text.go）；
// 文字参数（原样的对象名、路径、原因）由 fitFlash 整理和截断，长度总能符合上限；
// 不合格时（编程错误）记录错误并照常跳转，只是不带提示。
func (d *Dashboard) flashMsg(w http.ResponseWriter, r *http.Request, to string, t uiText) {
	v, err := flashValue(fitFlash(t))
	if err != nil {
		d.log.Error("跳转后的提示不合格", "path", r.URL.Path, "id", t.id, "err", err)
		d.redirect(w, r, to)
		return
	}
	d.redirect(w, r, withFlash(to, v))
}

// withFlash 在站内地址 to 上加 flash 参数。
func withFlash(to, v string) string {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	return to + sep + "flash=" + url.QueryEscape(v)
}

// runURL 返回 run 详情页的地址。
func runURL(id int64) string { return "/runs/" + strconv.FormatInt(id, 10) }

// jobURL 返回 job 详情页的地址。
func jobURL(id int64) string { return "/jobs/" + strconv.FormatInt(id, 10) }

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// activeRuns 返回正在进行或排队的 run 数；查询失败（或没有数据库的测试）时返回 0，不影响页面。
// activeRunsKey 让页面处理器把已经查到的活跃 run 数交给 renderPage，避免同一次渲染再查一次。
type activeRunsKey struct{}

// withActiveRuns 记录页面处理器已经得到的活跃 run 数。
func withActiveRuns(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, activeRunsKey{}, n)
}

// activeRunsFrom 返回页面处理器记录的活跃 run 数；没有记录时 ok 为 false。
func activeRunsFrom(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(activeRunsKey{}).(int)
	return n, ok
}

func (d *Dashboard) activeRuns(ctx context.Context) int {
	if d.db == nil {
		return 0
	}
	var n int
	// state 的取值写成字面量而不是参数：部分索引依赖字面量才能命中
	if err := d.db.QueryRow(ctx, `SELECT count(*) FROM runs WHERE state IN ('queued', 'active')`).Scan(&n); err != nil {
		d.log.Debug("统计进行中的 run", "err", err)
		return 0
	}
	return n
}

// Package ingest 是 kiln-server 的事件接入（PLAN #7.2）：接收并校验 webhook、事件落库与处理
// （push、PR、rerequest、installation、repository），用户显式的"刷新仓库"，以及可选的引用对账（PLAN #7.3）。
//
// 默认事件驱动：没有 webhook 或显式操作时不向 GitHub 发请求。单个事件只读取它需要的信息——
// 本地没有记录的 installation 用 GET /app/installations/{id} 定向校验，第一次出现的仓库用 repodir.Verify
// 校验授权范围后记录；installation / installation_repositories 事件按 payload 增量更新仓库目录。
//
// 文件划分：webhook.go 接收 webhook 和事件处理循环，event.go 事件分派与过滤，push.go push 和 PR，
// rerun.go GitHub 上的 Re-run，installation.go installation 与仓库目录，reconcile.go 引用对账。
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// errNoGitHubApp 表示 Server 没有配置 GitHub App（KILN_GITHUB_APP_ID）。
var errNoGitHubApp = errors.New("没有配置 GitHub App")

// senderTypeBot 是 GitHub 账号类型（sender.type、user.type）中表示 bot 的取值。
const senderTypeBot = "Bot"

// githubActor 返回 GitHub 用户在 Kiln 审计日志等处的操作者名称。
func githubActor(login string) string {
	return "github:" + login
}

// Config 是事件接入需要的配置（由 server.Config 映射）。
type Config struct {
	// WebhookSecret 用于校验 webhook 签名。
	WebhookSecret string
	// AllowedAccounts 是允许安装的 GitHub 账号（ID 或登录名，小写）。
	AllowedAccounts map[string]bool
	// ReconcileInterval 是可选的周期引用对账的间隔；0（默认）表示关闭，只在用户显式操作时对单个仓库补对账。
	ReconcileInterval time.Duration
	// ReconcileDelay 是启动后第一次周期对账前的等待时间。
	ReconcileDelay time.Duration
	// ReconcileRepos 是周期对账的范围：仓库全名（owner/name，小写）；包含 "*" 时为全部启用中的仓库。
	// 为空时即使设置了 ReconcileInterval 也不对账。
	ReconcileRepos []string
}

// Deps 是 Ingester 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 没有配置 GitHub App 时为 nil，此时 webhook 返回 503，对账和刷新仓库不执行。
	GitHub  *gh.Client
	Planner *planner.Planner
	// Reviews 规划、重跑和取消 AI 审计（PLAN #30.6）；为 nil 时不处理 AI 审计。
	Reviews *review.Reviews
	// Outbox 在记下 PR 规划完成的事务中提名自动入队。
	Outbox *outbox.Writer
	// Engine 用于取消被删除分支和已关闭 PR 的 run。
	Engine *engine.Engine
	Hub    *notify.Hub
	Log    *slog.Logger
	// IsSelf 判断 login 是否是本 App 的 bot 账号（本 App 推送的 tag 只更新 ref_heads，PLAN #7.2）。
	IsSelf func(login string) bool
	// MergeQueue 是合并队列（PLAN #28）；为 nil 时不处理 kiln:merge，也不通知队列。
	MergeQueue MergeQueue
	// AutoTag 记下合并进默认分支的 commit（PLAN #29.4）；为 nil 时不记录。
	AutoTag AutoTag
	// AIDev 处理 AI 开发的 Issue 和 PR 事件（PLAN #26.11）；为 nil 时不处理。
	AIDev AIDev
}

// AIDev 是 ingest 用到的 AI 开发操作（*aidev.Service，经 server 适配）。
type AIDev interface {
	// IssueEvent 处理 issues 事件（action、Issue 编号和内容、变化的 label、发送者），返回处理结果的说明。
	IssueEvent(ctx context.Context, repoID int64, ev IssueEvent) (string, error)
	// PullClosed 处理工作项的 PR 被关闭（merged 为真表示合并，mergeSHA 是合并得到的 commit），返回处理结果的说明。headRef
	// 是 PR 的来源分支（fork 来的 PR 为空），headSHA 是合并时 PR 的 head（没有合并时为空）。
	PullClosed(ctx context.Context, repoID int64, pr int, headRef, headSHA string, merged bool, mergeSHA, sender string) (string, error)
	// PullHead 记下工作项的 PR 的新 head。
	PullHead(ctx context.Context, repoID int64, pr int, head string) error
}

// IssueEvent 是一个 issues 事件中 AI 开发需要的内容：Issue 的当前内容（任务池按它更新）、变化的 label、发送者。
type IssueEvent struct {
	Action, Title, Body, Label, Sender string
	Number                             int
	Open                               bool
	Labels                             []string
	URL                                string
	UpdatedAt                          time.Time
	FromSelf                           bool
}

// AutoTag 是 ingest 用到的自动打 tag 操作（*autotag.Service）。
type AutoTag interface {
	Record(ctx context.Context, r mqevents.AutoTagRecord) (bool, error)
}

// MergeQueue 是 ingest 用到的合并队列操作（*mergequeue.Service）。
type MergeQueue interface {
	// Enqueue 把 PR 加入队列（baseRef 是事件中的目标分支），返回是否新加入。
	Enqueue(ctx context.Context, repoID int64, pr int, head, baseRef, actor string) (bool, error)
	// Notify 在 PR（0 表示任意 PR）在队列中时通知执行者。
	Notify(ctx context.Context, repoID int64, pr int) error
	// Nominate 记下 PR 可能可以自动入队（PLAN #29.5），由执行者检查。
	Nominate(ctx context.Context, repoID int64, pr int) error
}

// Ingester 接收 webhook、处理事件，执行显式的刷新仓库和补对账，以及（开启时）周期对账。
type Ingester struct {
	cfg     Config
	db      *store.DB
	gh      *gh.Client
	planner *planner.Planner
	reviews *review.Reviews
	outbox  *outbox.Writer
	engine  *engine.Engine
	hub     *notify.Hub
	log     *slog.Logger
	isSelf  func(login string) bool
	queue   MergeQueue
	autoTag AutoTag
	aiDev   AIDev
	// pushChainLimit 是规划 push 时最多向前追溯的过时事件数（maxPushChain，测试中调小）。
	pushChainLimit int
	// stateTimeout 是记录事件结果的时限；测试中可调小，验证慢处理返回后仍能落盘。
	stateTimeout time.Duration
}

// New 返回 Ingester。
func New(cfg Config, d Deps) *Ingester {
	return &Ingester{
		cfg: cfg, db: d.DB, gh: d.GitHub, planner: d.Planner, reviews: d.Reviews, outbox: d.Outbox, engine: d.Engine, hub: d.Hub, log: d.Log, isSelf: d.IsSelf, queue: d.MergeQueue, autoTag: d.AutoTag, aiDev: d.AIDev,
		pushChainLimit: maxPushChain,
		stateTimeout:   eventStateTimeout,
	}
}

// dropEvent 表示事件被消费但不做任何事：payload 无法解析、找不到对象、或操作本身失败且重试没有意义。
// 原因记录在 events.result 中；只有返回 error 的事件才会重试。
func dropEvent(reason string) (string, error) { return reason, nil }

// accountAllowed 判断账号是否在白名单中（ID 或登录名）。
func (i *Ingester) accountAllowed(id int64, login string) bool {
	return i.cfg.AllowedAccounts[strconv.FormatInt(id, 10)] || i.cfg.AllowedAccounts[strings.ToLower(login)]
}

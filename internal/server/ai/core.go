package ai

import (
	"context"
	"log/slog"
	"sync/atomic"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Deps 是 Core 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 用于同步 Check Run；没有配置 GitHub App 时为 nil（不同步）。
	GitHub *gh.Client
	// GitCred 是按操作签发 git 凭据的登记（AI、CI 共用一个实例）；没有配置 GitHub App 时为 nil（不签发）。
	GitCred *gitcred.Registry
	// Logs 是 AI attempt 的日志（<日志目录>/ai/<attempt>.log）。
	Logs *logstore.Store
	// Outbox 在 AI 的事务中写入 Check Run 等同步记录。
	Outbox *outbox.Writer
	// Auth 校验 Runner 的 access token。
	Auth *runnerauth.Auth
	Hub  *notify.Hub
	Log  *slog.Logger
	// Types 是注册的任务类型。
	Types *Registry
}

// Core 是 AI 领域的核心：请求的规划与取消、调度、生命周期和 sweeper。
type Core struct {
	cfg Config
	db  *store.DB
	gh  *gh.Client
	// gitCred 见 Deps.GitCred。
	gitCred *gitcred.Registry
	logs    *logstore.Store
	outbox  *outbox.Writer
	auth    *runnerauth.Auth
	hub     *notify.Hub
	log     *slog.Logger
	types   *Registry
	// idle 在某个角色的 AI 执行节点空闲、没有可领取的任务时调用（AI 开发据此开始选题）；组装时设置，可以为 nil。
	idle func(ctx context.Context, role string) error
	// dev 处理开发任务的候选和领取（OfferDevCandidates、ClaimDevIssues）；组装时设置。
	dev DevHandler
	// docs 读取任务所属工作项冻结的外部设计文档快照（ListTaskDocs、GetTaskDoc）；组装时设置，可以为 nil。
	docs *docsync.Service
	// verifications 是 CI 验证的只读查询（GetVerification）；组装时设置。
	verifications Verifications
	// timeoutsFrom 是开始判定超时的时间（UnixNano）：Server 启动后的 2 × lease_ttl 内不判定（ExtendLeasesOnStartup）。
	timeoutsFrom atomic.Int64
}

// New 返回 Core。
func New(cfg Config, d Deps) *Core {
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = defaultSweepInterval
	}
	return &Core{cfg: cfg, db: d.DB, gh: d.GitHub, gitCred: d.GitCred, logs: d.Logs, outbox: d.Outbox, auth: d.Auth, hub: d.Hub, log: d.Log, types: d.Types}
}

// SetIdleHook 设置 AI 执行节点空闲时的通知（组装时调用，在 Server 开始服务之前）。
func (c *Core) SetIdleHook(f func(ctx context.Context, role string) error) { c.idle = f }

// SetDevHandler 设置开发任务的候选与领取的处理者（组装时调用，在 Server 开始服务之前）。
func (c *Core) SetDevHandler(h DevHandler) { c.dev = h }

// Types 返回注册的任务类型。
func (c *Core) Types() *Registry { return c.types }

// Logs 返回 AI attempt 的日志存储。
func (c *Core) Logs() *logstore.Store { return c.logs }

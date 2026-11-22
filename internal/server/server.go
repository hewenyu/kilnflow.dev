package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2/airunnerv2connect"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/db"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/publish"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/retention"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/runnerapi"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/ingest"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/web"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
)

const (
	// getAppTimeout 是启动时读取 App 信息的超时。
	getAppTimeout = 30 * time.Second
	// appRetryInterval 是启动时没能读到 App 信息之后的懒加载重试间隔：事件处理的热路径不能每个事件都请求 GitHub。
	appRetryInterval = time.Minute
	// githubHTTPTimeout 是单个 GitHub API 请求的超时（包括上传 Release asset）。
	githubHTTPTimeout = 5 * time.Minute
	// readHeaderTimeout 是读取 HTTP 请求头的超时。
	readHeaderTimeout = 30 * time.Second
	// idleTimeout 是 keep-alive 连接空闲多久后关闭（不影响进行中的长轮询和上传）。
	idleTimeout = 2 * time.Minute
	// shutdownTimeout 是退出时等待进行中的 HTTP 请求结束的时间。
	shutdownTimeout = 10 * time.Second
	// shutdownGrace 是退出时让进行中的请求自然结束的时间，之后取消仍未结束的请求的 ctx：
	// Runner 的长轮询（Acquire）最长要等 AcquireLongPoll（默认 25 秒），不取消就等不到它们结束。
	// 小于 shutdownTimeout，给被取消的请求留出返回的时间。
	shutdownGrace = 7 * time.Second
	// maxRPCMessage 是 Connect 请求体的上限。
	maxRPCMessage = 16 << 20
	// maxCASObject 是通过预签名 URL 上传的单个对象的上限。
	maxCASObject = 10 << 30
)

// appInfo 是 GitHub App 自身的信息，启动时读取一次。
type appInfo struct {
	Slug string
	// BotLogin 是 App 的 bot 账号（<slug>[bot]），用于识别本 App 触发的事件。
	BotLogin string
}

// Server 是 kiln-server。
type Server struct {
	cfg *Config
	db  *store.DB
	gh  *gh.Client
	app atomic.Pointer[appInfo]
	// appTried 是上次尝试读取 App 信息的时间（UnixNano）：启动时失败之后按 appRetryInterval 懒加载重试。
	appTried atomic.Int64
	cas      *cas.FSStore
	logs     *logstore.Store
	log      *slog.Logger

	keys *auth.Keys
	// hub 唤醒长轮询中的 Acquire 和各后台循环。
	hub *notify.Hub

	engine *engine.Engine
	// outbox 在业务事务中写入 Check Run 同步记录，outboxWorker 负责投递。
	outbox       *outbox.Writer
	outboxWorker *outbox.Worker
	planner      *planner.Planner
	// ingest 接收 webhook、处理事件，执行显式的刷新仓库和补对账，以及可选的周期对账。
	ingest *ingest.Ingester
	// runnerAPI 实现 RunnerService（注册、认证、调度、结果提交）。
	runnerAPI *runnerapi.Service
	// publish 领取并执行发布 job。
	publish *publish.Worker
	// mergeQueue 是合并队列（PLAN #28）。
	mergeQueue *mergequeue.Service
	// autoTag 给经过 PR 合入默认分支的 commit 打 tag 并规划发布（PLAN #29.4）。
	autoTag *autotag.Service
	// runnerAuth 是 Runner 的注册与认证（CI、AI 共用，PLAN #30.5）。
	runnerAuth *runnerauth.Auth
	// gitCred 是按操作签发 git 凭据的登记（PLAN #33.13），CI、AI 共用这一个实例；
	// 没有配置 GitHub App 时为 nil。
	gitCred *gitcred.Registry
	// ai 是 AI 领域的核心（请求、任务、调度、生命周期，PLAN #30.6），reviews 规划 AI 审计，aiDev 是 AI 开发。
	ai      *ai.Core
	reviews *review.Reviews
	aiDev   *aidev.Service
	// decisionInbox 把 AI 开发的决策疑难点单向同步到 Outline「待拍板」清单。
	decisionInbox *decisioninbox.Service
	// admin 执行管理操作（CLI 和 Dashboard 共用），dashboard 是 Web 界面。
	admin     *admin.Service
	dashboard *web.Dashboard
	// retention 按保留策略清理过程数据（日志、产物等，PLAN #10.3）。
	retention *retention.Collector
	// ops 是同步操作（AI 规划、文档冻结）的生命周期门闩：信号退出、HTTP 异常退出与 Close 都先停止接纳并取消在途操作，
	// 再等在途操作结束、关闭数据库（审计后的修正）。
	ops *opGate
}

// New 连接数据库、执行迁移并初始化 Server。
func New(ctx context.Context, cfg *Config, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	logs, err := logstore.New(filepath.Join(cfg.DataDir, logDir))
	if err != nil {
		pool.Close()
		return nil, err
	}
	sdb := store.New(pool)
	sdb.Log = log
	s := &Server{
		cfg:  cfg,
		db:   sdb,
		cas:  &cas.FSStore{Root: filepath.Join(cfg.DataDir, "cas")},
		logs: logs,
		log:  log,
		hub:  notify.NewHub(),
		ops:  newOpGate(),
	}
	if err := s.init(ctx); err != nil {
		s.ops.StopAccepting()
		pool.Close()
		return nil, err
	}
	return s, nil
}

// init 读取密钥、创建 GitHub 客户端，并组装 CI、AI、合并三个领域和管理操作、Dashboard（PLAN #30.3：领域之间只通过这里注入的
// 接口和 outbox 事件交互）。
func (s *Server) init(ctx context.Context) error {
	var err error
	if s.keys, err = auth.Load(ctx, s.db, s.cfg.AdminToken); err != nil {
		return err
	}
	if s.cfg.GitHubAppID != 0 {
		key, err := gh.ParsePrivateKey(s.cfg.GitHubPrivateKey)
		if err != nil {
			return err
		}
		s.gh = &gh.Client{
			AppID: s.cfg.GitHubAppID, Key: key, APIURL: s.cfg.GitHubAPIURL, UploadURL: s.cfg.GitHubUploadURL,
			HTTP: &http.Client{Timeout: githubHTTPTimeout},
		}
	}
	// 共用的机制
	s.outbox = outbox.NewWriter(s.gh != nil)
	s.outboxWorker = outbox.NewWorker(outbox.Deps{DB: s.db, Hub: s.hub, Log: s.log})
	s.runnerAuth = runnerauth.New(s.db, s.keys, map[string]runnerauth.EnrollHook{
		store.RunnerKindCI: runnerapi.EnrollHook,
		store.RunnerKindAI: ai.EnrollHook,
	})
	git := gitread.New()
	if s.gh != nil {
		s.gitCred = gitcred.New(gitcred.Config{}, gitcred.Deps{GitHub: s.gh, Log: s.log})
	}
	var forgeFor mergequeue.ForgeFunc
	var tokenForge aidev.TokenForgeFunc
	if s.gh != nil {
		forgeFor = func(r *store.Repo) forge.Repo { return forge.GitHub(s.gh, r.InstallationID, r.FullName()) }
		tokenForge = func(r *store.Repo, token string) forge.Repo {
			return forge.GitHubWithToken(s.gh, r.InstallationID, token, r.FullName())
		}
	}

	// CI
	s.engine = engine.New(engine.Config{
		InfraRetryMax:  s.cfg.InfraRetryMax,
		LeaseTTL:       s.cfg.LeaseTTL,
		CancelGrace:    s.cfg.CancelGrace,
		SweepInterval:  s.cfg.SweepInterval,
		NonceRetention: runnerauth.NonceRetention,
	}, engine.Deps{
		DB: s.db, CAS: s.cas, Outbox: s.outbox, Hub: s.hub, Log: s.log,
		AttemptEnded: s.ciAttemptEnded, ReconcileGitCred: s.ciReconcileGitCred,
	})
	s.planner = planner.New(planner.Deps{DB: s.db, GitHub: s.gh, Git: git, Engine: s.engine, Outbox: s.outbox, Hub: s.hub, Log: s.log})
	cicheck.New(cicheck.Config{AppID: s.cfg.GitHubAppID, PublicURL: s.cfg.PublicURL},
		cicheck.Deps{DB: s.db, GitHub: s.gh, Logs: s.logs}).Register(s.outboxWorker)
	// 调度核心（PLAN #32）：现在没有托管池（Managed 为 nil），只有 Native Runner 的领取经过它
	sched := scheduler.New(scheduler.Config{LeaseTTL: s.cfg.LeaseTTL},
		scheduler.Deps{DB: s.db, Engine: s.engine, Outbox: s.outbox, Log: s.log})
	s.runnerAPI = runnerapi.New(runnerapi.Config{
		LeaseTTL:        s.cfg.LeaseTTL,
		AcquireLongPoll: s.cfg.AcquireLongPoll,
		MasterKey:       s.cfg.MasterKey,
	}, runnerapi.Deps{
		DB: s.db, GitHub: s.gh, GitCred: s.gitCred, CAS: s.cas, Keys: s.keys, Auth: s.runnerAuth, Logs: s.logs,
		Engine: s.engine, Scheduler: sched, Hub: s.hub, Log: s.log,
	})
	publishDir := filepath.Join(s.cfg.DataDir, "publish")
	s.publish = publish.New(publish.Config{WorkDir: publishDir, MasterKey: s.cfg.MasterKey}, publish.Deps{
		DB: s.db, GitHub: s.gh, CAS: s.cas, Engine: s.engine, Outbox: s.outbox, Logs: s.logs, Hub: s.hub, Log: s.log,
	})

	// AI
	aiLogs, err := logstore.New(filepath.Join(s.cfg.DataDir, logDir, aiLogDir))
	if err != nil {
		return err
	}
	// 外部设计文档快照：按仓库绑定的 Outline 文档集冻结，供 AI 开发与审计读取。
	docs := docsync.New(docsync.Deps{DB: s.db, MasterKey: s.cfg.MasterKey, BaseCtx: s.ops.Context(), Track: s.ops.Track})
	// 「待拍板」清单同步：用仓库配置的独立受限写 token 往一篇专用文档追加 AI 开发的决策疑难点。
	s.decisionInbox = decisioninbox.New(decisioninbox.Deps{
		DB: s.db, MasterKey: s.cfg.MasterKey, BaseCtx: s.ops.Context(), Track: s.ops.Track,
		Writer: func(baseURL, token string) (docprovider.DocumentWriter, error) {
			return docprovider.NewOutline(docprovider.Config{BaseURL: baseURL, Token: token})
		},
	})
	s.aiDev = aidev.New(aidev.Deps{
		DB: s.db, Docs: docs, Forge: aidev.ForgeFunc(forgeFor), TokenForge: tokenForge, GitHub: s.gh, CIResults: ciResults{}, Outbox: s.outbox, Hub: s.hub, Log: s.log,
		HoldLabel: mergecfg.HoldLabel, ReleaseSkipLabel: autotag.LabelSkip, BranchTTL: s.cfg.Retention,
		CheckSelf: s.checkSelf,
		PullRecorded: func(ctx context.Context, tx pgx.Tx, repoID int64, pr int) error {
			return mqevents.KickMergeQueue(ctx, s.outbox, tx, repoID, pr)
		},
	})
	s.ai = ai.New(ai.Config{
		LeaseTTL:        s.cfg.LeaseTTL,
		CancelGrace:     s.cfg.CancelGrace,
		AcquireLongPoll: s.cfg.AcquireLongPoll,
		SweepInterval:   s.cfg.SweepInterval,
		PublicURL:       s.cfg.PublicURL,
		AppID:           s.cfg.GitHubAppID,
	}, ai.Deps{
		DB: s.db, GitHub: s.gh, GitCred: s.gitCred, Logs: aiLogs, Outbox: s.outbox, Auth: s.runnerAuth, Hub: s.hub, Log: s.log,
		Types: ai.NewRegistry(review.NewType(s.outbox, s.outbox), s.aiDev.TaskType()),
	})
	s.aiDev.SetCore(s.ai)
	s.aiDev.SetDecisionInbox(s.decisionInbox)
	s.ai.SetDevHandler(s.aiDev)
	s.ai.SetDocs(docs)
	s.ai.SetIdleHook(s.aiDev.RunnerIdle)
	s.ai.SetVerifications(ciVerifications{db: s.db, logs: s.logs, publicURL: s.cfg.PublicURL})
	s.reviews = review.New(review.Deps{
		DB: s.db, GitHub: s.gh, Core: s.ai, Git: git, Docs: docs, UpdatedFrom: mergequeue.UpdatedFrom, Log: s.log,
		BaseCtx: s.ops.Context(), Track: s.ops.Track,
	})
	s.outboxWorker.Handle(ai.KindTaskCheck, s.ai.DeliverTaskCheck)
	s.outboxWorker.Handle(ai.KindRequestCheck, s.ai.DeliverRequestCheck)
	s.outboxWorker.Handle(review.KindPRReview, review.NewPublisher(s.db, s.gh, s.cfg.PublicURL).Deliver)
	s.outboxWorker.HandleDead(review.KindPRReview, review.OnDead)
	s.outboxWorker.Handle(aidev.KindRepo, s.aiDev.HandleOutbox)
	s.outboxWorker.Handle(decisioninbox.KindWake, func(context.Context, []byte) error {
		s.decisionInbox.Wake()
		return nil
	})

	// 合并
	s.mergeQueue = mergequeue.New(mergequeue.Deps{
		DB: s.db, Forge: forgeFor, Outbox: s.outbox, Hub: s.hub, Log: s.log, PublicURL: s.cfg.PublicURL,
		Verifications: []store.VerificationSource{engine.PRVerifications, review.PRVerifications},
		HoldPending:   aidev.HoldPending,
		DevConflicts:  devConflicts{s.aiDev},
	})
	s.outboxWorker.Handle(mqevents.KindMergeQueue, s.mergeQueue.HandleOutbox)
	s.autoTag = autotag.New(autotag.Deps{
		DB: s.db, Forge: autotag.ForgeFunc(forgeFor), Planner: tagPlanner{s.planner}, Outbox: s.outbox, Hub: s.hub, Log: s.log,
		DevOrigin: devTagOrigin{},
	})
	s.outboxWorker.Handle(mqevents.KindAutoTag, s.autoTag.HandleOutbox)
	s.outboxWorker.Handle(outbox.KindPRVerified, s.handlePRVerified)

	// 事件接入、保留策略、管理操作、Dashboard
	s.ingest = ingest.New(ingest.Config{
		WebhookSecret:     s.cfg.GitHubWebhookSecret,
		AllowedAccounts:   s.cfg.AllowedAccounts,
		ReconcileInterval: s.cfg.ReconcileInterval,
		ReconcileDelay:    s.cfg.ReconcileDelay,
		ReconcileRepos:    s.cfg.ReconcileRepos,
	}, ingest.Deps{
		DB: s.db, GitHub: s.gh, Planner: s.planner, Reviews: s.reviews, Outbox: s.outbox, Engine: s.engine, Hub: s.hub, Log: s.log,
		IsSelf: s.isSelf, MergeQueue: s.mergeQueue, AutoTag: s.autoTag, AIDev: aiDevEvents{s.aiDev},
	})
	s.retention = retention.New(retention.Config{
		Retention: s.cfg.Retention, Interval: s.cfg.RetentionInterval, PublishWorkDir: publishDir,
	}, retention.Deps{DB: s.db, CAS: s.cas, Logs: s.logs, Log: s.log, Purgers: []retention.Purger{s.ai}})
	s.admin = admin.New(admin.Config{MasterKey: s.cfg.MasterKey}, admin.Deps{
		DB: s.db, Engine: s.engine, Planner: s.planner, Ingest: s.ingest, Logs: s.logs, CAS: s.cas, MergeQueue: s.mergeQueue, AutoTag: s.autoTag,
		AIDev: s.aiDev, AI: s.ai, Reviews: s.reviews, DecisionInbox: s.decisionInbox,
		BaseCtx: s.ops.Context(), Track: s.ops.Track,
		OutboxRetryHooks: map[string]outbox.RetryHook{review.KindPRReview: review.OnRetry},
		Hub:              s.hub, Log: s.log,
	})
	s.dashboard, err = web.New(web.Config{
		PublicURL:          s.cfg.PublicURL,
		GitHubClientID:     s.cfg.GitHubClientID,
		GitHubClientSecret: s.cfg.GitHubClientSecret,
		GitHubAPIURL:       s.cfg.GitHubAPIURL,
		Admins:             s.cfg.Admins,
		Users:              s.cfg.Users,
		LeaseTTL:           s.cfg.LeaseTTL,
		AcquireLongPoll:    s.cfg.AcquireLongPoll,
	}, web.Deps{DB: s.db, Admin: s.admin, AI: s.ai, Keys: s.keys, Log: s.log, AppSlug: s.appSlug})
	return err
}

// ciAttemptEnded 在 CI attempt 结束的事务提交之后使它在签发登记中失效：吊销它的全部源码 token，之后不再签发
// （engine.Deps.AttemptEnded）。
func (s *Server) ciAttemptEnded(ctx context.Context, attemptID int64) {
	if s.gitCred != nil {
		s.gitCred.Invalidate(ctx, runnerapi.GitCredAttempt(attemptID))
	}
}

// ciReconcileGitCred 对账 CI 在签发登记中的 attempt（engine.Deps.ReconcileGitCred）：running 是 engine 对 attempts 表的
// 只读查询，已经结束的失效。
func (s *Server) ciReconcileGitCred(ctx context.Context, running func(ctx context.Context, ids []int64) ([]int64, error)) error {
	if s.gitCred == nil {
		return nil
	}
	_, err := s.gitCred.Reconcile(ctx, gitcred.DomainCI, running)
	return err
}

// waitGitCred 在关闭时有界地等待签发登记在后台的吊销（attempt 失效时发起），最多等一次失效的总时限；等不到的 token
// 到期自然失效。
func (s *Server) waitGitCred() {
	if s.gitCred == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitcred.DefaultInvalidateTimeout)
	defer cancel()
	if err := s.gitCred.Wait(ctx); err != nil {
		s.log.Warn("关闭时没有等到 git 凭据的后台吊销结束，未吊销的到期自然失效", "err", err)
	}
}

// logDir 是数据目录中的日志目录（CI attempt 的日志），aiLogDir 是其中 AI attempt 的日志的子目录（PLAN #30.4）。
const (
	logDir   = "logs"
	aiLogDir = "ai"
)

// handlePRVerified 处理 PR 的一个验证（CI 的 PR run、AI 审计请求）结束的事件（PLAN #30.3）：通过时提名自动入队；通知合并队列
// 和 AI 开发的执行者。在一个事务中写入，失败时整条记录重试（写入都按 key 合并，重复无害）。
func (s *Server) handlePRVerified(ctx context.Context, payload []byte) error {
	var p outbox.PRVerifiedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("outbox 记录的 payload 无法解析：%w", err)
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if p.Passed {
			if err := mqevents.NominateAutoMerge(ctx, s.outbox, tx, p.RepoID, p.PR); err != nil {
				return err
			}
		}
		if err := mqevents.KickMergeQueue(ctx, s.outbox, tx, p.RepoID, p.PR); err != nil {
			return err
		}
		return s.aiDev.Kick(ctx, tx, p.RepoID, p.PR)
	})
	if err == nil {
		s.hub.KickOutbox()
	}
	return err
}

// ciResults 用 CI 的数据实现 aidev.CIResults（AI 开发的返修读取 PR head 上的 CI 结果，PLAN #30.3）。
type ciResults struct{}

func (ciResults) Settled(ctx context.Context, q store.Querier, repoID int64, pr int, head string) (int, bool, []aidev.CIFailure, error) {
	runs, done, failures, err := engine.PRSettled(ctx, q, repoID, pr, head)
	out := make([]aidev.CIFailure, 0, len(failures))
	for _, f := range failures {
		out = append(out, aidev.CIFailure{JobID: f.JobID, Name: f.Name, Reason: f.Reason, ErrClass: f.ErrClass})
	}
	return runs, done, out, err
}

// ciVerifications 用 CI 的数据实现 ai.Verifications（AI 的 Agent 查询 PR 或 commit 最新的 CI 验证，PLAN #33.9）。
type ciVerifications struct {
	db        *store.DB
	logs      *logstore.Store
	publicURL string
}

func (v ciVerifications) Latest(ctx context.Context, repoID int64, sha string, logTailBytes int64) (*ai.Verification, error) {
	res, err := engine.LatestVerification(ctx, v.db, v.logs, v.publicURL, repoID, sha, logTailBytes)
	if err != nil {
		return nil, err
	}
	out := &ai.Verification{SHA: res.SHA, State: res.State}
	for _, j := range res.FailedJobs {
		out.FailedJobs = append(out.FailedJobs, ai.FailedJob{
			Name: j.Name, Reason: j.Reason, ErrClass: j.ErrClass, LogTail: j.LogTail, LogExpired: j.LogExpired, URL: j.URL,
		})
	}
	return out, nil
}

// tagPlanner 用 CI 的规划器实现 autotag.Planner：自动打出的 tag 按 push 事件规划，保留真实的发布发起者和它是否是机器人
// （AI 开发的可信实现 PR 由 Kiln 的 Bot 合并，可信自动 tag 的规则放行它的 secret 和发布 job，PLAN #29.4）。
type tagPlanner struct{ p *planner.Planner }

func (t tagPlanner) PlanTagPush(ctx context.Context, ev autotag.TagPush) ([]int64, error) {
	return t.p.PlanEvent(ctx, &planner.Request{
		Repo: ev.Repo, Event: gitref.EventPush, Ref: gitref.TagPrefix + ev.Tag, SHA: ev.SHA,
		SenderLogin: ev.Sender, SenderIsBot: ev.SenderIsBot, Source: store.SourceAutoTag,
	})
}

// devTagOrigin 用 AI 开发的归属判定实现 autotag.OriginResolver（PLAN #29.4、#30.3）：合并领域不依赖 AI 领域的类型。
type devTagOrigin struct{}

// devOriginKinds 把 aidev 的来源类别换成自动打 tag 的。
var devOriginKinds = map[aidev.AutoTagOriginKind]autotag.OriginKind{
	aidev.AutoTagOriginUnknown:        autotag.OriginUnknown,
	aidev.AutoTagOriginImplementation: autotag.OriginImplementation,
	aidev.AutoTagOriginDesign:         autotag.OriginDesign,
	aidev.AutoTagOriginRecording:      autotag.OriginRecording,
	aidev.AutoTagOriginDenied:         autotag.OriginDenied,
}

func (devTagOrigin) Resolve(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (autotag.DevOrigin, error) {
	o, err := aidev.ResolveAutoTagOrigin(ctx, q, repoID, pr, headRef, baseRef)
	if err != nil {
		return autotag.DevOrigin{}, err
	}
	kind, ok := devOriginKinds[o.Kind]
	if !ok {
		return autotag.DevOrigin{}, fmt.Errorf("未知的 PR 来源 %d", o.Kind)
	}
	return autotag.DevOrigin{
		Kind: kind, WorkItemID: o.WorkItemID, Phase: o.Phase, ImplRound: o.ImplRound, BaseRef: o.BaseRef,
		CreatorLogin: o.CreatorLogin, DeliveryTaskID: o.DeliveryTaskID, Issues: o.Issues, Reason: o.Reason,
	}, nil
}

// devConflicts 用 AI 开发实现 mergequeue.DevConflicts（合并冲突按 PR 的归属交回 AI 开发返修，PLAN #28.3、#30.3）：合并领域
// 不依赖 aidev 的类型。
type devConflicts struct{ s *aidev.Service }

// devOwnerKinds 把 aidev 的归属换成合并队列的。
var devOwnerKinds = map[aidev.OwnerKind]mergequeue.DevOwnerKind{
	aidev.OwnerNone: mergequeue.DevOwnerNone, aidev.OwnerActive: mergequeue.DevOwnerActive,
	aidev.OwnerEnded: mergequeue.DevOwnerEnded, aidev.OwnerRecording: mergequeue.DevOwnerRecording,
}

func (devConflicts) Owner(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (mergequeue.DevOwner, error) {
	o, err := aidev.Owner(ctx, q, repoID, pr, headRef)
	if err != nil {
		return mergequeue.DevOwner{}, err
	}
	kind, ok := devOwnerKinds[o.Kind]
	if !ok {
		return mergequeue.DevOwner{}, fmt.Errorf("未知的 PR 归属 %d", o.Kind)
	}
	return mergequeue.DevOwner{Kind: kind, WorkItem: o.WorkItem, State: o.State, BaseRef: o.BaseRef}, nil
}

func (d devConflicts) HandBack(ctx context.Context, tx pgx.Tx, c mergequeue.DevConflict) (bool, error) {
	return d.s.HandBack(ctx, tx, aidev.Conflict{RepoID: c.RepoID, PR: c.PR, HeadRef: c.HeadRef, Head: c.Head, BaseRef: c.BaseRef, Base: c.Base})
}

// aiDevEvents 把 ingest 的 AI 开发事件交给 aidev（ingest 不依赖 aidev 的类型）。
type aiDevEvents struct{ s *aidev.Service }

func (a aiDevEvents) IssueEvent(ctx context.Context, repoID int64, ev ingest.IssueEvent) (string, error) {
	return a.s.HandleIssueEvent(ctx, aidev.IssueEvent{
		RepoID: repoID, Action: ev.Action, Number: ev.Number, Title: ev.Title, Body: ev.Body, Open: ev.Open, Labels: ev.Labels, URL: ev.URL,
		UpdatedAt: ev.UpdatedAt, Label: ev.Label, Sender: ev.Sender, FromSelf: ev.FromSelf,
	})
}

func (a aiDevEvents) PullClosed(ctx context.Context, repoID int64, pr int, headRef, headSHA string, merged bool, mergeSHA, sender string) (string, error) {
	return a.s.HandlePullClosed(ctx, repoID, aidev.ClosedPull{Number: pr, HeadRef: headRef, HeadSHA: headSHA, Merged: merged, MergeSHA: mergeSHA}, sender)
}

func (a aiDevEvents) PullHead(ctx context.Context, repoID int64, pr int, head string) error {
	return a.s.HandlePullHead(ctx, repoID, pr, head)
}

// Close 停止接纳新的同步规划 / 文档冻结、取消在途操作，等它们结束后关闭数据库。
func (s *Server) Close() {
	s.ops.Close()
	s.db.Close()
}

// checkSelf 判断 login 是否是本 App 的 bot 账号，同时返回 App 身份是否已经加载。启动时没能读取 App 信息时，之后在事件
// 处理中懒加载（成功后缓存，失败后按 appRetryInterval 重试）：读到之前 ready 为假，调用方必须保留现场、稍后重试——绝不能
// 把“还不知道作者是谁”当成“不是本 App”，否则会删掉合法的待核验证据、误转工作项、丢掉归属。
func (s *Server) checkSelf(login string) (self, ready bool) {
	app := s.appNow()
	if app == nil {
		return false, false
	}
	return strings.EqualFold(login, app.BotLogin), true
}

// isSelf 判断 login 是否是本 App 的 bot 账号（只关心结果；身份未就绪时为 false）。规则见 checkSelf。
func (s *Server) isSelf(login string) bool {
	self, _ := s.checkSelf(login)
	return self
}

// appSlug 返回 App 的 slug（未知时为空）。只读缓存：不触发后台请求，轮询它不会拉起懒加载。
func (s *Server) appSlug() string {
	if app := s.app.Load(); app != nil {
		return app.Slug
	}
	return ""
}

// appNow 返回 App 信息：启动时没有读到（失败）时安排一次后台重试，这次返回 nil。重试不阻塞事件处理、不在热路径上
// 请求 GitHub；成功之后不再请求，失败后至少隔 appRetryInterval 再试。
func (s *Server) appNow() *appInfo {
	if app := s.app.Load(); app != nil {
		return app
	}
	if s.gh == nil {
		return nil // 没有配置 GitHub App：读不到，也不重试
	}
	now := time.Now().UnixNano()
	last := s.appTried.Load()
	if last != 0 && now-last < int64(appRetryInterval) {
		return nil
	}
	if !s.appTried.CompareAndSwap(last, now) {
		return nil // 别的 goroutine 正在重试
	}
	// 后台重试属于服务生命周期：服务退出时取消并等它结束，不泄漏 goroutine。测试中直接构造的 Server
	// 没有 ops（nil），退回 Background。
	ctx := context.Background()
	done := func() {}
	if s.ops != nil {
		var err error
		if done, err = s.ops.Track(); err != nil {
			return nil // 服务正在退出，不再发起新的请求
		}
		ctx = s.ops.Context()
	}
	go func() {
		defer done()
		s.loadApp(ctx)
	}()
	return nil
}

// loadApp 读取 App 信息。失败时只记录错误：此时无法识别本 App 触发的事件，其他功能不受影响。
func (s *Server) loadApp(ctx context.Context) {
	// 记录尝试时间：启动时的读取和懒加载重试都从这里开始计时，失败后至少隔 appRetryInterval 再试。
	s.appTried.Store(time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(ctx, getAppTimeout)
	defer cancel()
	app, err := s.gh.GetApp(ctx)
	if err != nil {
		s.log.Error("读取 App 信息失败（检查 KILN_GITHUB_APP_ID 和私钥）", "err", err)
		return
	}
	s.app.Store(&appInfo{Slug: app.Slug, BotLogin: app.Slug + "[bot]"})
	s.log.Info("GitHub App", "slug", app.Slug, "id", app.ID)
}

// Handler 返回 Server 的全部 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.ingest.HandleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.Ping(r.Context()); err != nil {
			// healthz 不需要认证：错误详情（可能含数据库地址和用户名）只写日志
			s.log.Warn("healthz：数据库不可用", "err", err)
			http.Error(w, "db: unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	route, h := runnerv1connect.NewRunnerServiceHandler(s.runnerAPI, connect.WithReadMaxBytes(maxRPCMessage),
		connect.WithInterceptors(runnerapi.ErrorInterceptor(s.log)))
	mux.Handle(route, h)
	route, h = airunnerv2connect.NewAIRunnerServiceHandler(s.ai.RunnerService(), connect.WithReadMaxBytes(maxRPCMessage),
		connect.WithInterceptors(rpcerr.ErrorInterceptor(s.log, "AI 执行节点 API 内部错误")))
	mux.Handle(route, h)
	route, h = admin.NewHandler(s.admin, s.keys, connect.WithReadMaxBytes(maxRPCMessage))
	mux.Handle(route, h)
	mux.HandleFunc("/cas/{digest}", s.handleCAS)
	s.dashboard.Register(mux)
	return mux
}

// Run 启动后台循环和 HTTP 服务，直到 ctx 结束。返回前先停止接纳并取消进行中的同步规划 / 文档冻结，再停止后台循环并等它们退出；
// 数据库要等 Close 里等完在途同步操作后才关闭（审计后的修正）。
func (s *Server) Run(ctx context.Context) error {
	if err := s.engine.ExtendLeasesOnStartup(ctx); err != nil {
		return err
	}
	if err := s.ai.ExtendLeasesOnStartup(ctx); err != nil {
		return err
	}
	if err := s.publish.ResetClaims(ctx); err != nil {
		return err
	}
	if s.gh != nil {
		// 在启动 ingest 之前读取 App 信息，事件过滤需要 bot 账号。
		// 启动时不列出 installation 和仓库：仓库目录按事件增量更新，或由用户显式"刷新仓库"
		s.loadApp(ctx)
	} else {
		s.log.Warn("没有配置 GitHub App（KILN_GITHUB_APP_ID），只能使用 Runner 和管理接口")
	}
	// 后台循环都在 bgCtx 结束时退出；HTTP 服务异常退出时也要停止它们，所以用单独的 ctx
	bgCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
		s.waitGitCred()
	}()
	// 先停止接纳并取消在途同步操作，再停止后台循环（defer 后进先出）。
	defer s.ops.StopAccepting()
	wg.Go(func() { s.ingest.Run(bgCtx) })
	wg.Go(func() { s.outboxWorker.Run(bgCtx) })
	wg.Go(func() { s.engine.RunSweeper(bgCtx) })
	wg.Go(func() { s.ai.RunSweeper(bgCtx) })
	wg.Go(func() { s.publish.Run(bgCtx) })
	wg.Go(func() { s.ingest.RunReconcile(bgCtx) })
	wg.Go(func() { s.retention.Run(bgCtx) })
	wg.Go(func() { s.mergeQueue.Run(bgCtx) })
	wg.Go(func() { s.autoTag.Run(bgCtx) })
	wg.Go(func() { s.aiDev.Run(bgCtx) })
	wg.Go(func() { s.decisionInbox.Run(bgCtx) })

	srv := &http.Server{Addr: s.cfg.Listen, Handler: logRequests(s.log, s.Handler()), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout}
	return serveHTTP(ctx, srv, shutdownGrace, shutdownTimeout, func() {
		s.log.Info("kiln-server 已启动", "listen", s.cfg.Listen, "public_url", s.cfg.PublicURL, "version", buildinfo.Version())
	}, s.ops.StopAccepting)
}

// serveHTTP 运行 srv 直到 ctx 结束，然后优雅退出：先等进行中的请求自然结束 grace，再取消仍未结束的请求的 ctx
// （主要是 Runner 的长轮询），最多共等 timeout。请求的 ctx 不随 ctx 立即取消（WithoutCancel），已经开始的上传、提交结果
// 有机会完成。started 在开始监听后调用；stopping 在开始优雅退出（等待 Shutdown）之前调用，用来先停止接纳并取消在途的
// 同步规划 / 文档冻结，免得关闭时要等它们跑满预算。监听失败时返回错误。
func serveHTTP(ctx context.Context, srv *http.Server, grace, timeout time.Duration, started, stopping func()) error {
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv.BaseContext = func(net.Listener) context.Context { return reqCtx }
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	started()
	select {
	case <-ctx.Done():
		stopping()
		t := time.AfterFunc(grace, cancelRequests)
		defer t.Stop()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errCh:
		stopping()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleCAS 处理预签名的上传和下载（v0：对象存放在 Server 本地磁盘）。
func (s *Server) handleCAS(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	q := r.URL.Query()
	op := q.Get("op")
	if !cas.ValidDigest(digest) || !s.keys.CheckCASSig(op, digest, q.Get("exp"), q.Get("sig")) {
		http.Error(w, "签名无效或已过期", http.StatusForbidden)
		return
	}
	switch {
	case op == auth.CASOpPut && r.Method == http.MethodPut:
		// 先记录再写文件：保留策略的清理在 blobs 行锁下删除文件（PLAN #10.4），先刷新 last_ref_at 才不会
		// 在写入之后被正在进行的清理删掉。写入失败时留下的记录没有文件，PrepareUpload 仍会要求上传
		if _, err := s.db.Exec(r.Context(), `INSERT INTO blobs (digest, size) VALUES ($1, 0) ON CONFLICT (digest) DO UPDATE SET last_ref_at=now()`, digest); err != nil {
			s.casServerError(w, "记录对象", digest, err)
			return
		}
		body := &bodyReader{r: http.MaxBytesReader(w, r.Body, maxCASObject)}
		n, err := s.cas.Put(r.Context(), digest, body)
		if err != nil {
			s.casPutError(w, digest, body.err, err)
			return
		}
		if _, err := s.db.Exec(r.Context(), `UPDATE blobs SET size=$2, last_ref_at=now() WHERE digest=$1`, digest, n); err != nil {
			s.casServerError(w, "记录对象", digest, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case op == auth.CASOpGet && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.serveCASObject(w, r, digest)
	default:
		http.Error(w, "不支持的操作", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveCASObject(w http.ResponseWriter, r *http.Request, digest string) {
	rc, size, err := s.cas.Get(r.Context(), digest)
	if errors.Is(err, cas.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.casServerError(w, "读取对象", digest, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if cd := downloadDisposition(r.URL.Query().Get("filename")); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if r.Method == http.MethodHead {
		return
	}
	if f, ok := rc.(*os.File); ok {
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		// 响应头已经发出，只能记录日志
		s.log.Warn("发送 CAS 对象", "digest", digest, "err", err)
	}
}

// bodyReader 记录读取请求体时的错误（不含 EOF），用来区分客户端的问题（请求体中断、超过上限）和 Server 写入失败。
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// casPutError 返回上传失败的响应：请求体超过上限为 413，请求体读取失败或内容与 digest 不符为 400（说明可以给客户端看）；
// Server 的磁盘错误为 500，详细信息（含数据目录的路径）只写日志。readErr 是读取请求体时的错误。
func (s *Server) casPutError(w http.ResponseWriter, digest string, readErr, err error) {
	_, pathErr := errors.AsType[*fs.PathError](err)
	_, linkErr := errors.AsType[*os.LinkError](err)
	switch {
	case readErr != nil:
		if _, tooLarge := errors.AsType[*http.MaxBytesError](readErr); tooLarge {
			http.Error(w, "对象超过上限", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "读取上传内容失败", http.StatusBadRequest)
	case pathErr || linkErr:
		s.casServerError(w, "写入对象", digest, err)
	default:
		// 内容与声明的 digest 不符
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// casServerError 记录 CAS 请求的内部错误并返回 500；响应中不含错误详情（数据库、文件路径）。
func (s *Server) casServerError(w http.ResponseWriter, what, digest string, err error) {
	s.log.Error("CAS "+what+"失败", "digest", digest, "err", err)
	http.Error(w, "服务器"+what+"失败，详细信息见 Server 日志", http.StatusInternalServerError)
}

// downloadDisposition 返回下载时的 Content-Disposition。filename 来自 Dashboard 签发的下载地址，不在签名范围内：
// 只取最后一段路径、去掉控制字符，只影响保存的文件名，不影响内容和授权。为空或不合法时返回空字符串。
func downloadDisposition(filename string) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, path.Base(strings.ReplaceAll(filename, "\\", "/")))
	if name == "" || name == "." || name == ".." || name == "/" || len(name) > 255 {
		return ""
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap 让 http.ResponseController 能找到底层的 ResponseWriter（设置读写超时、全双工等）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// runnerServicePath、aiRunnerServicePath 是 Runner 的服务（CI 的 RunnerService、AI 的 AIRunnerService）的路由前缀
// （Connect 的路径为 /<服务全名>/<方法>）。
const (
	runnerServicePath   = "/" + runnerv1connect.RunnerServiceName + "/"
	aiRunnerServicePath = "/" + airunnerv2connect.AIRunnerServiceName + "/"
)

// logRequests 以 debug 级别记录 HTTP 请求；不记录 Dashboard 前端的静态文件、healthz 和 Runner 成功的高频调用。
func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		runner := strings.HasPrefix(r.URL.Path, runnerServicePath) || strings.HasPrefix(r.URL.Path, aiRunnerServicePath)
		if runner && sw.status < http.StatusBadRequest {
			return // Runner 的高频调用不记录
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/favicon.svg" || r.URL.Path == "/healthz" {
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

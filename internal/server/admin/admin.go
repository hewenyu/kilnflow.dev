// Package admin 实现 kiln-server 的管理操作（PLAN #16.4、PLAN #19）：取消、重跑、手动触发、
// Runner 管理与注册令牌、变量/secret/environment、仓库与 installation 同步、cache salt、outbox 查看与重试、
// run/job/日志查询。
//
// Service 是这些操作的唯一实现，Dashboard（web 包）和 AdminService 的 ConnectRPC 适配（NewHandler）共用。
// 操作者（actor）由调用方传入：Dashboard 使用登录名；CLI 使用 admin token，记为管理员绑定的 GitHub 账号（没有绑定时为 TokenActor，见 binding.go）。
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/randtoken"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/ingest"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TokenActor 是管理员还没有绑定 GitHub 账号时，通过 admin token 操作（CLI，或 Dashboard 用 admin token 登录）记录在审计日志中的操作者。
const TokenActor = "admin"

// Config 是管理操作需要的配置。
type Config struct {
	// MasterKey 用于加密写入的 secret。
	MasterKey []byte
}

// Deps 是 Service 的依赖。
type Deps struct {
	DB      *store.DB
	Engine  *engine.Engine
	Planner *planner.Planner
	Ingest  *ingest.Ingester
	Logs    *logstore.Store
	// CAS 用于读取 job 的产物清单（Dashboard 的产物列表和下载）。
	CAS cas.Store
	// MergeQueue 是合并队列（PLAN #28）；为 nil 时合并队列的操作返回错误。
	MergeQueue *mergequeue.Service
	// AutoTag 是自动打 tag（PLAN #29.4）；为 nil 时重试返回错误。
	AutoTag *autotag.Service
	// AIDev 是 AI 开发（PLAN #26.11）；为 nil 时开始、暂停、终止、重试返回错误（查看照常）。
	AIDev *aidev.Service
	// AI 是 AI 领域的核心（取消请求、任务的日志，PLAN #30）；Reviews 触发和重跑 AI 审计。为 nil 时这些操作返回 FailedPrecondition。
	AI      *ai.Core
	Reviews *review.Reviews
	// DecisionInbox 是「待拍板」同步的配置与人工对账；为 nil 时新 RPC 返回 FailedPrecondition。
	DecisionInbox *decisioninbox.Service
	// DocProvider 按绑定信息构造只读文档客户端（显式连接验证用）；为 nil 时用 Outline 客户端。
	DocProvider docsync.Factory
	// BaseCtx 是服务生命周期 context：显式验证在 HTTP 断开后仍要在预算内完成落库，但服务退出时立即取消。
	BaseCtx context.Context
	// Track 登记一次将要进行的同步操作，返回的 done 在操作结束时调用；服务正在退出时返回错误。
	Track func() (done func(), err error)
	// OutboxRetryHooks 是手动重试 outbox 记录时按记录类型调用的钩子（例如 PR review 恢复为 pending，由组装层注入）。
	OutboxRetryHooks map[string]outbox.RetryHook
	Hub              *notify.Hub
	Log              *slog.Logger
}

// Service 执行管理操作。按职责分布在 admin.go、binding.go、runs.go、outputs.go、runners.go、runner_cache.go、settings.go、
// docker.go、npm.go、mergequeue.go、ai.go 和 aidev.go 中；AdminService 的认证和错误处理见 rpc.go。
type Service struct {
	cfg         Config
	db          *store.DB
	engine      *engine.Engine
	planner     *planner.Planner
	ingest      *ingest.Ingester
	logs        *logstore.Store
	cas         cas.Store
	mergeQueue  *mergequeue.Service
	autoTag     *autotag.Service
	aiDev       *aidev.Service
	ai          *ai.Core
	reviews     *review.Reviews
	decision    *decisioninbox.Service
	docProvider docsync.Factory
	baseCtx     context.Context
	track       func() (done func(), err error)
	// 显式文档验证的时限：总预算、外部只读探测期限、收尾落库期限。构造时给默认值，
	// 测试可以缩短；绝不作为包级可变状态。
	docVerifyTimeout         time.Duration
	docVerifyExternalTimeout time.Duration
	docVerifySaveTimeout     time.Duration
	retryHooks               map[string]outbox.RetryHook
	hub                      *notify.Hub
	log                      *slog.Logger
}

// New 返回 Service。
func New(cfg Config, deps Deps) *Service {
	prov := deps.DocProvider
	if prov == nil {
		prov = func(baseURL, token string) (docprovider.Provider, error) {
			return docprovider.NewOutline(docprovider.Config{BaseURL: baseURL, Token: token})
		}
	}
	return &Service{
		cfg: cfg, db: deps.DB, engine: deps.Engine, planner: deps.Planner, ingest: deps.Ingest,
		logs: deps.Logs, cas: deps.CAS, mergeQueue: deps.MergeQueue, autoTag: deps.AutoTag, aiDev: deps.AIDev,
		ai: deps.AI, reviews: deps.Reviews, retryHooks: deps.OutboxRetryHooks, hub: deps.Hub, log: deps.Log,
		decision: deps.DecisionInbox, docProvider: prov, baseCtx: deps.BaseCtx, track: deps.Track,
		docVerifyTimeout: docVerifyTimeout, docVerifyExternalTimeout: docVerifyExternalTimeout,
		docVerifySaveTimeout: docVerifySaveTimeout,
	}
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

func notFound(format string, args ...any) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf(format, args...))
}

func (a *rpcAPI) ListRepos(ctx context.Context, _ *connect.Request[apiv1.ListReposRequest]) (*connect.Response[apiv1.ListReposResponse], error) {
	rows, err := a.s.db.Query(ctx, `SELECT id, owner||'/'||name, installation_id, private, default_branch, active FROM repos ORDER BY owner, name`)
	if err != nil {
		return nil, err
	}
	repos, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*apiv1.Repo, error) {
		var r apiv1.Repo
		err := row.Scan(&r.Id, &r.FullName, &r.InstallationId, &r.Private, &r.DefaultBranch, &r.Active)
		return &r, err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.ListReposResponse{Repos: repos}), nil
}

// SyncInstallations 刷新仓库目录（用户显式操作）：从 GitHub 列出 installation 和仓库，不读取分支、tag、PR。
// 结果（包括失败）记录在 kv 中供仓库页显示"最近刷新"，并写审计日志。
func (s *Service) SyncInstallations(ctx context.Context, actor string) (ingest.SyncResult, error) {
	res, err := s.ingest.SyncInstallations(ctx)
	st := &store.SyncStatus{At: time.Now(), Actor: actor, Installations: res.Installations, Repos: res.Repos}
	detail := map[string]any{"installations": res.Installations, "repos": res.Repos}
	if err != nil {
		st.Error = err.Error()
		detail["error"] = err.Error()
	}
	// 结果记录与操作本身无关：失败只记录告警，不改变返回值
	if serr := store.SaveSyncStatus(ctx, s.db, st); serr != nil {
		s.log.Warn("记录刷新仓库的结果", "err", serr)
	}
	s.auditBestEffort(ctx, actor, "repos.sync", detail)
	return res, err
}

func (a *rpcAPI) SyncInstallations(ctx context.Context, _ *connect.Request[apiv1.SyncInstallationsRequest]) (*connect.Response[apiv1.SyncInstallationsResponse], error) {
	res, err := a.s.SyncInstallations(ctx, rpcActor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.SyncInstallationsResponse{Installations: clampInt32(res.Installations), Repos: clampInt32(res.Repos)}), nil
}

// ReconcileRepo 对一个仓库补对账（用户显式操作，PLAN #7.3）：只请求这个仓库的分支、tag 和打开的 PR，补上漏掉的事件，并写审计日志。
// 被 GitHub 限流时返回可读的错误，稍后再试即可（已经补上的不会重复）。
func (s *Service) ReconcileRepo(ctx context.Context, fullName, actor string) (ingest.ReconcileResult, error) {
	repo, err := store.FindRepo(ctx, s.db, fullName)
	if err != nil {
		return ingest.ReconcileResult{}, rpcError(connect.CodeInvalidArgument, err)
	}
	res, err := s.ingest.ReconcileRepo(ctx, repo.ID)
	detail := map[string]any{"repo": repo.FullName(), "baseline": res.Baseline, "runs": res.Runs}
	if err != nil {
		detail["error"] = err.Error()
	}
	s.auditBestEffort(ctx, actor, "repo.reconcile", detail)
	if rl, ok := gh.RateLimited(err); ok {
		return res, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("被 GitHub 限流，%s 后再试（已经补上的 run 不会重复）", rl.Until.Local().Format(time.DateTime)))
	}
	return res, err
}

func (a *rpcAPI) ReconcileRepo(ctx context.Context, req *connect.Request[apiv1.ReconcileRepoRequest]) (*connect.Response[apiv1.ReconcileRepoResponse], error) {
	res, err := a.s.ReconcileRepo(ctx, req.Msg.Repo, rpcActor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.ReconcileRepoResponse{
		Repo: res.Repo, Baseline: res.Baseline,
		Branches: clampInt32(res.Branches), Tags: clampInt32(res.Tags), Pulls: clampInt32(res.Pulls), Runs: clampInt32(res.Runs),
	}), nil
}

// cacheSaltBytes 是 cache salt 的随机字节数。
const cacheSaltBytes = 8

func (a *rpcAPI) BumpCacheSalt(ctx context.Context, req *connect.Request[apiv1.BumpCacheSaltRequest]) (*connect.Response[apiv1.BumpCacheSaltResponse], error) {
	s := a.s
	repo, err := store.FindRepo(ctx, s.db, req.Msg.Repo)
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE repos SET cache_salt=$2 WHERE id=$1`, repo.ID, randtoken.New(cacheSaltBytes)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, rpcActor(ctx), "cache.bump_salt", map[string]any{"repo": repo.FullName()})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.BumpCacheSaltResponse{}), nil
}

// ListOutbox 返回状态在 states 中的 outbox 记录（按 ID 倒序，最多 limit 条）。
func (s *Service) ListOutbox(ctx context.Context, states []string, limit int) ([]*outbox.Item, error) {
	return outbox.List(ctx, s.db, states, limit)
}

// rpcOutboxLimit 是 CLI 一次列出的 outbox 记录数上限。
const rpcOutboxLimit = 200

func (a *rpcAPI) ListOutbox(ctx context.Context, req *connect.Request[apiv1.ListOutboxRequest]) (*connect.Response[apiv1.ListOutboxResponse], error) {
	states := []string{store.OutboxPending, store.OutboxDead}
	switch st := req.Msg.State; st {
	case "":
	case store.OutboxPending, store.OutboxDead, store.OutboxDone:
		states = []string{st}
	default:
		// 写错的状态不能当作"没有记录"
		return nil, invalid(fmt.Errorf("未知的 outbox 状态 %q（%s、%s 或 %s）", st, store.OutboxPending, store.OutboxDead, store.OutboxDone))
	}
	list, err := a.s.ListOutbox(ctx, states, rpcOutboxLimit)
	if err != nil {
		return nil, err
	}
	items := make([]*apiv1.OutboxItem, 0, len(list))
	for _, it := range list {
		items = append(items, &apiv1.OutboxItem{
			Id: it.ID, DedupeKey: it.DedupeKey, Kind: it.Kind, State: it.State, Attempts: it.Attempts,
			// 与原来 SQL 中 extract(epoch ...)::bigint 一致：四舍五入到秒
			LastError: it.LastError, UpdatedAt: it.UpdatedAt.Round(time.Second).Unix(),
		})
	}
	return connect.NewResponse(&apiv1.ListOutboxResponse{Items: items}), nil
}

func (a *rpcAPI) RetryOutbox(ctx context.Context, req *connect.Request[apiv1.RetryOutboxRequest]) (*connect.Response[apiv1.RetryOutboxResponse], error) {
	if err := a.s.RetryOutbox(ctx, req.Msg.Id, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.RetryOutboxResponse{}), nil
}

// RetryOutbox 把一条 outbox 记录重新排队并唤醒投递 worker。
func (s *Service) RetryOutbox(ctx context.Context, id int64, actor string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := outbox.Retry(ctx, tx, id, s.retryHooks); errors.Is(err, outbox.ErrNotFound) {
			return notFound("没有 outbox 记录 #%d", id)
		} else if err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "outbox.retry", map[string]any{"id": id})
	})
	if err != nil {
		return err
	}
	s.hub.All()
	return nil
}

func scopeOf(sc *apiv1.Scope) (account, repo, env string) {
	if sc == nil {
		return "", "", ""
	}
	return sc.Account, sc.Repo, sc.Environment
}

// isNotFound 判断错误是否表示查询的对象不存在。
func isNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// auditBestEffort 记录审计日志；用于操作本身已经完成、审计失败不应让请求失败的场景（只记录告警）。
func (s *Service) auditBestEffort(ctx context.Context, actor, action string, detail any) {
	if err := store.Audit(ctx, s.db, actor, action, detail); err != nil {
		s.log.Warn("记录审计日志失败", "action", action, "actor", actor, "err", err)
	}
}

// clampInt32 把 int 转换为 int32，超出范围时取边界值（用于填写 protobuf 的计数字段）。
func clampInt32[T int | int64](n T) int32 {
	switch {
	case int64(n) > math.MaxInt32:
		return math.MaxInt32
	case int64(n) < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

// unixOrZero 返回 t 的 Unix 秒数，nil 时为 0（protobuf 的计数字段没有"未知"）。
func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

package aidev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ForgeFunc 返回仓库的平台接口。
type ForgeFunc func(repo *store.Repo) forge.Repo

// DecisionInbox 把交付报告里的决策疑难点登记到「待拍板」清单。由组装层注入；为 nil 时不同步。
type DecisionInbox interface {
	EnqueueReportTx(ctx context.Context, tx pgx.Tx, in decisioninbox.ReportInput) (bool, error)
}

// TokenForgeFunc 返回只用给定 installation token 的仓库平台接口（交付的写操作用按操作签发的窄 token）。
type TokenForgeFunc func(repo *store.Repo, token string) forge.Repo

// Deps 是 Service 的依赖。
type Deps struct {
	DB *store.DB
	// Docs 冻结仓库绑定的外部设计文档快照；为 nil 时开发任务不带文档。
	Docs *docsync.Service
	// Forge 为 nil（没有配置 GitHub App）时不处理。
	Forge ForgeFunc
	// TokenForge 构造交付的写操作所用的平台接口（推进正式分支、写 PR，token 由 GitHub 签发、用后吊销，executor 的 writer）；
	// 为 nil 时不能推进交付。
	TokenForge TokenForgeFunc
	// GitHub 用于在规划前补齐仓库的 clone 地址（AI 执行节点按它找到仓库），以及签发、吊销交付的写凭据；为 nil 时不补、
	// 不能推进交付。
	GitHub *gh.Client
	// CIResults 是 PR head 上 CI 的结果（返修判断用），由组装层用 CI 领域的数据实现（PLAN #30.3）。为 nil 时只按 AI 审计判断。
	CIResults CIResults
	Outbox    *outbox.Writer
	Hub       *notify.Hub
	Log       *slog.Logger
	// HoldLabel 是 PR 上表示不自动入队的 label（合并领域的 mergecfg.HoldLabel，由组装层传入，PLAN #29.5、#30.3）：改动了
	// 敏感路径等情况下给交付的 PR 加上它。
	HoldLabel string
	// ReleaseSkipLabel 是 PR 上表示合并后不打版本 tag 的 label（合并领域的 autotag.LabelSkip，由组装层传入，PLAN #31.5、
	// #30.3）：给设计 PR 加上它。为空时不加。
	ReleaseSkipLabel string
	// BranchTTL 是 attempt 分支（交付、checkpoint）的保留时间：与过程数据相同（PLAN #26.13），0 表示 DefaultBranchTTL。
	BranchTTL time.Duration
	// PullRecorded 在推进交付、记下工作项的 PR 的事务中调用（通知合并队列：它可能在等这个 PR 的归属，PLAN #28.3），由组装层
	// 注入（PLAN #30.3）；为 nil 时不通知。
	PullRecorded func(ctx context.Context, tx pgx.Tx, repoID int64, pr int) error
	// CheckSelf 判断 login 是否是本 App 的 bot 账号，同时返回 App 身份是否已经加载（核验正式分支上的 PR 是否由本 App 创建，
	// PLAN #29.4）；ready 为假时无法核验，调用方保留现场、稍后重试（绝不把“还不知道”当成“不是本 App”）。为 nil 时视为
	// 身份未就绪。
	CheckSelf SelfCheckFunc
}

// DefaultBranchTTL 是 attempt 分支默认的保留时间（与过程数据的保留时间相同）。
const DefaultBranchTTL = 12 * time.Hour

// Service 是 AI 开发的 Server 端：规划选题和返修、领取、记录结果、仓库级执行者（PLAN #26.11）。
type Service struct {
	db    *store.DB
	docs  *docsync.Service
	forge ForgeFunc
	// tokenForge 见 Deps.TokenForge。
	tokenForge TokenForgeFunc
	gh         *gh.Client
	core       *ai.Core
	ci         CIResults
	holdLabel  string
	skipLabel  string
	outbox     *outbox.Writer
	hub        *notify.Hub
	log        *slog.Logger
	branchTTL  time.Duration
	// pullRecorded 见 Deps.PullRecorded。
	pullRecorded func(ctx context.Context, tx pgx.Tx, repoID int64, pr int) error
	// checkSelf 见 Deps.CheckSelf。
	checkSelf SelfCheckFunc
	// afterPlan 在开发请求规划成功之后、调用方把任务关联到工作项之前调用（模块测试模拟两步之间发生的事）；生产中为 nil。
	afterPlan func(t *ai.Task)
	// decisionInbox 见 DecisionInbox。
	decisionInbox DecisionInbox
}

// SetDecisionInbox 注入「待拍板」同步（组装时调用，在开始服务之前）。
func (s *Service) SetDecisionInbox(in DecisionInbox) { s.decisionInbox = in }

// SetCore 设置 AI 核心（开发请求的规划与取消，PLAN #30）。组装时调用，在开始服务之前：Core 的 Registry 要先有
// Service 的任务类型（TaskType），所以 Core 在 Service 之后创建。
func (s *Service) SetCore(c *ai.Core) { s.core = c }

// BranchTTL 返回 attempt 分支的保留时间（Dashboard 据此说明分支何时删除）。
func (s *Service) BranchTTL() time.Duration { return s.branchTTL }

// New 返回 Service。
func New(d Deps) *Service {
	ttl := d.BranchTTL
	if ttl <= 0 {
		ttl = DefaultBranchTTL
	}
	// 日志不能是 nil：执行过程中的告警（label 同步被拒等）照常记录，测试和工具里没有注入时用默认 logger
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db: d.DB, docs: d.Docs, forge: d.Forge, tokenForge: d.TokenForge, gh: d.GitHub, ci: d.CIResults, outbox: d.Outbox, hub: d.Hub, log: log,
		holdLabel: d.HoldLabel, skipLabel: d.ReleaseSkipLabel, branchTTL: ttl, pullRecorded: d.PullRecorded, checkSelf: d.CheckSelf,
	}
}

// ErrBusy 表示另一次处理正在进行。
var ErrBusy = errors.New("AI 开发的执行者正在处理这个仓库，稍后重试")

// sweepInterval 是兜底检查的间隔：有未完成事项的仓库每隔这么久处理一次（outbox 的重试之外）。
const sweepInterval = 10 * time.Minute

func lockKey(repoID int64) string { return fmt.Sprintf("kiln:aidev:%d", repoID) }

// poolLockKey 是任务池事务的独立锁：更新 dev_pool 的入口（webhook、手动同步）互相串行，但不和执行者的长锁 lockKey 混用，
// 避免持有执行者锁的事务被外部调用拖住。锁序仍是先拿这把锁，再按 issue_number 升序锁 dev_pool 行，最后写 ai_repo_state。
func poolLockKey(repoID int64) string { return fmt.Sprintf("kiln:aidev:pool:%d", repoID) }

// HandleOutbox 投递 KindRepo 记录（注册到 outbox.Worker）。
func (s *Service) HandleOutbox(ctx context.Context, payload []byte) error {
	var p repoPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("AI 开发通知的 payload 无法解析：%w", err)
	}
	return s.Process(ctx, p.RepoID)
}

// Run 是兜底检查：每 sweepInterval 通知一次有未完成事项（未结束的工作项、待发的评论、待删的 attempt 分支、
// 任务池中有 Issue——选题请求在领取前结束时由这里结束那一轮）的仓库，直到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("AI 开发兜底检查", "err", err)
		}
	}
}

func (s *Service) sweep(ctx context.Context) error {
	ids, err := store.QueryIDs(ctx, s.db, `SELECT repo_id FROM dev_work_items WHERE active OR labels_state<>state
		UNION SELECT repo_id FROM dev_pool WHERE in_pool
		UNION SELECT repo_id FROM dev_comments WHERE state IN ('pending','attempted')
		UNION SELECT repo_id FROM dev_branches WHERE deleted_at IS NULL
		UNION SELECT repo_id FROM dev_pull_origin_checks`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.Enqueue(ctx, s.db, id); err != nil {
			return err
		}
	}
	return nil
}

// lockRetry 是等待仓库锁时重试的间隔。
const lockRetry = 100 * time.Millisecond

// lockWait 是人的操作（终止工作项）等待仓库锁的最长时间：执行者一次处理中的外部调用都有超时，通常很快释放。
const lockWait = time.Minute

// withRepoLock 在仓库级的 advisory lock 下执行 f（每个仓库一个执行者，C19）。持有锁期间的查询和事务都用 conn
// （不从连接池借第二个连接）。wait 为 0 时拿不到锁立即返回 ErrBusy，否则最多等 wait。
func (s *Service) withRepoLock(ctx context.Context, repoID int64, wait time.Duration, f func(conn *pgxpool.Conn) error) (err error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	unlock, err := s.repoLock(ctx, conn, repoID, wait)
	if err != nil {
		return err
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil {
			err = errors.Join(err, fmt.Errorf("释放 AI 开发的锁：%w", uerr))
		}
	}()
	return f(conn)
}

// repoLock 等仓库锁：wait 为 0 时只尝试一次，否则按 lockRetry 的间隔重试到期限。只用一个定时器，不每一轮新建。
func (s *Service) repoLock(ctx context.Context, conn *pgxpool.Conn, repoID int64, wait time.Duration) (func(context.Context) error, error) {
	deadline := time.Now().Add(wait)
	timer := time.NewTimer(lockRetry)
	defer timer.Stop()
	for {
		unlock, locked, err := store.TryAdvisoryLock(ctx, conn, lockKey(repoID))
		if err != nil {
			return nil, err
		}
		if locked {
			return unlock, nil
		}
		if !time.Now().Before(deadline) {
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			timer.Reset(lockRetry)
		}
	}
}

// tx 在 conn 上执行事务（conn 为 nil 时用连接池）。
func (s *Service) tx(ctx context.Context, conn *pgxpool.Conn, f func(tx pgx.Tx) error) error {
	if conn == nil {
		return s.db.Tx(ctx, f)
	}
	return store.TxOn(ctx, conn, f)
}

// notify 唤醒 Dashboard 等组件。
func (s *Service) notify() {
	if s.hub != nil {
		s.hub.All()
	}
}

// docsFor 冻结（或复用已有的）工作项外部设计文档快照，转成任务 spec 里的引用。没有配置文档服务时为空。
func (s *Service) docsFor(ctx context.Context, repoID, workItemID int64) ([]aispec.DocRef, error) {
	if s.docs == nil {
		return nil, nil
	}
	snaps, err := s.docs.Freeze(ctx, repoID, workItemID)
	if err != nil {
		return nil, err
	}
	return docRefs(snaps), nil
}

// docRefs 把冻结的快照转成任务 spec 里的文档引用（只带元数据，正文由 Agent 按需经 kiln-orca task doc get 读取）。
func docRefs(snaps []docsync.Snapshot) []aispec.DocRef {
	if len(snaps) == 0 {
		return nil
	}
	out := make([]aispec.DocRef, 0, len(snaps))
	for _, d := range snaps {
		out = append(out, aispec.DocRef{ID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	return out
}

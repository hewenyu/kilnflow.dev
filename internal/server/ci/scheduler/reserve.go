package scheduler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 预留（PLAN #32.4）：Native 的领取和托管的预留经过同一个事务入口 Reserve。事务中依次：锁触发方自己的资源（Native 锁
// runners 行，托管取池的 advisory lock）→ 取一页候选 job（FOR UPDATE SKIP LOCKED）→ 按仓库 ID 升序锁仓库配额 → 读快照
// → 核心选择 → 写跳过原因 → 每个选中的 job：Prepare 回调 → 领取（Native）或预留（托管）。两种触发方各自只锁自己的资源，
// 之后的加锁顺序相同，不会互相死锁；同一个 job 只有一个事务能锁到（I1）。

// Config 是 Reserver 的配置。
type Config struct {
	// LeaseTTL 是 Native Runner 租约的有效期，也是托管预留的期限（期间没有任何外部调用，PLAN #32.6）。
	LeaseTTL time.Duration
}

// ManagedSource 提供托管池的快照（PLAN #32.4）。由组装层注入；没有托管池时为 nil。
type ManagedSource interface {
	Pools(ctx context.Context) ([]ManagedPool, error)
}

// Deps 是 Reserver 的依赖。
type Deps struct {
	DB *store.DB
	// Engine 把无法准备的 job 置为失败。
	Engine *engine.Engine
	// Outbox 在领取、预留时写入 Check Run 同步记录。
	Outbox *outbox.Writer
	// Managed 是托管池的来源；nil 表示没有托管池。
	Managed ManagedSource
	Log     *slog.Logger
}

// Reserver 是原子预留入口。调用方在事务提交后负责唤醒（notify.Hub.All）。
type Reserver struct {
	cfg     Config
	db      *store.DB
	engine  *engine.Engine
	outbox  *outbox.Writer
	managed ManagedSource
	log     *slog.Logger
}

// New 返回 Reserver。
func New(cfg Config, d Deps) *Reserver {
	return &Reserver{cfg: cfg, db: d.DB, engine: d.Engine, outbox: d.Outbox, managed: d.Managed, log: d.Log}
}

var (
	// ErrDrained 表示领取时 Native Runner 已被暂停接单。
	ErrDrained = errors.New("runner 已暂停接单")
	// ErrRevoked 表示领取时 Native Runner 已被吊销或删除。
	ErrRevoked = errors.New("runner 已被吊销或不存在")
)

// NativeReport 是 Native Runner 在这次领取中上报的状态（覆盖数据库中记录的）。
type NativeReport struct {
	Platforms, Executors []string
	Cap                  *store.RunnerCapacity
	LegacyFree           *store.Resources
	// Running 是 Runner 上报的正在执行的 job 数。
	Running int
}

// NativeTrigger 是 Native Runner 的一次领取（长轮询中的一页）。
type NativeTrigger struct {
	// Runner 是 Runner 的身份和数据库中记录的能力（ID、Name、Pool、Drain、Platforms、Executors）。
	Runner NativeRunner
	// Report 是这次上报的状态；nil 表示没有上报，用数据库中记录的。
	Report *NativeReport
	// RequestID 是领取的幂等键：同一个 request_id 已经领取过时返回那批 job。
	RequestID string
	MaxJobs   int
}

// platforms 返回这次领取用的平台和 executor（上报的优先）。
func (t *NativeTrigger) platforms() ([]string, []string) {
	if t.Report != nil {
		return t.Report.Platforms, t.Report.Executors
	}
	return t.Runner.Platforms, t.Runner.Executors
}

// ManagedTrigger 是 Server 为一个托管池派发（PLAN #32.4）。
type ManagedTrigger struct {
	// Pool 是托管池的 Key。
	Pool string
	Max  int
}

// Trigger 是一次预留的触发方（恰好一个不为 nil）。
type Trigger struct {
	Native  *NativeTrigger
	Managed *ManagedTrigger
}

// Cursor 是分页考察候选 job 时上一页最后一个 job 在排队顺序中的位置（cistore.QueueKey，PLAN #14.2）。一页的候选全部因为
// 资源、配额等原因领不了时，从它之后继续考察下一页，后面能执行的 job 不会被前面的饿死。
type Cursor = cistore.QueueKey

// CandidateLimit 是一次预留（一个事务）最多考察的候选 job 数，即一页。
const CandidateLimit = 300

// candPageSQL 返回按 Cursor 翻页的条件和排序（排队顺序 cistore.QueueOrderSQL）：参数从 $n 开始，依次为 cursor 的字段
// （第一页时都为 NULL，pageArgs）和每页的数量。pending 的 job 都有 queued_at（排队时写入）。
func candPageSQL(n int) string {
	return fmt.Sprintf(` AND ($%d::int IS NULL OR %s) ORDER BY %s LIMIT $%d FOR UPDATE SKIP LOCKED`,
		n, cistore.QueueAfterSQL(n), cistore.QueueOrderSQL, n+cistore.QueueKeyArgs)
}

// pageArgs 返回 candPageSQL 的参数。
func pageArgs(c *Cursor, limit int) []any {
	if c == nil {
		return append(make([]any, cistore.QueueKeyArgs), limit)
	}
	return append(c.Args(), limit)
}

// nextCursor 返回一页候选之后的游标：这一页满了（后面可能还有）时为最后一个 job 的排队位置，否则为 nil。
func nextCursor(cands []*cistore.Job, limit int) *Cursor {
	if len(cands) < limit {
		return nil
	}
	k, ok := cands[len(cands)-1].QueueKey()
	if !ok {
		return nil
	}
	return &k
}

// candidateFilter 是候选 job 的预过滤（$1–$8 来自 filterArgs）：pool 和 executor/平台的配对、能力在 SQL 中过滤，避免领不了的
// job 占满候选窗口。它是核心判断 accepts.mismatch 的超集（I7）。state='pending' 写成字面量而不是参数：部分索引 jobs_pending
// 依赖字面量才能命中。
const candidateFilter = `state='pending' AND kind='normal' AND platform = ANY($1) AND executor_kind = ANY($2)
	AND coalesce(spec->>'pool', '') IN ('', $3)
	AND ((executor_kind='host' AND platform=$4) OR (executor_kind='oci' AND platform LIKE 'linux/%') OR executor_kind NOT IN ('host','oci'))
	AND (coalesce(spec->>'buildkit_image', '') = '' OR $5::boolean)
	AND (spec->'signing' IS NULL OR $6::boolean)
	AND (jsonb_array_length(coalesce(spec->'services', '[]')) = 0 OR $7::boolean)
	AND (NOT effects OR $8::boolean)`

// candidates 取从 after 开始的一页候选 job（按排队顺序，锁住取到的行）。
func candidates(ctx context.Context, tx pgx.Tx, acc *accepts, after *Cursor) ([]*cistore.Job, error) {
	args := acc.filterArgs()
	return cistore.QueryJobs(ctx, tx, candidateFilter+candPageSQL(len(args)+1), append(args, pageArgs(after, CandidateLimit)...)...)
}

// filterArgs 返回 candidateFilter 的参数。
func (a *accepts) filterArgs() []any {
	return []any{
		a.platforms, a.executors, a.pool, a.native,
		a.caps[workflow.CapBuildkit], a.caps[workflow.CapSigning], a.caps[CapServices], a.caps[CapEffects],
	}
}

// Claim 是一个领取或预留到的 job。
type Claim[T any] struct {
	Job       *cistore.Job
	AttemptID int64
	Epoch     int64
	// Prepared 是 Prepare 回调的结果（Native：下发给 Runner 的 JobSpec）。
	Prepared T
}

// Prepare 是触发方在领取之前准备一个选中的 job（Native 生成 JobSpec）。attemptID、epoch 在领取之前为 0，重放已经领取的 job
// 时为它的 attempt。job 本身有问题时返回 *PrepareError：只把这个 job 置为失败，不回滚整次预留（I10）。
type Prepare[T any] func(ctx context.Context, q store.Querier, j *cistore.Job, attemptID, epoch int64) (T, error)

// PrepareError 表示 job 本身的问题导致无法准备（重试也不会好），与数据库等暂时性错误区分。Error() 写进 job 的原因。
type PrepareError struct{ Err error }

func (e *PrepareError) Error() string { return e.Err.Error() }
func (e *PrepareError) Unwrap() error { return e.Err }

// Result 是一次预留（考察一页候选）的结果。
type Result[T any] struct {
	Claims []Claim[T]
	// Replayed 表示 Claims 是同一个 request_id 之前已经领取的那批 job（Native）。
	Replayed bool
	// Failed 是这一页中因为 job 本身的问题（*PrepareError）被置为失败的候选数：队列因此变了，调用方要唤醒其他等待的
	// Runner 并重新考察。
	Failed int
	// RetryAt 是为了让更合适的资源领取而跳过的 job 中最早可以不再跳过的时间（零值表示没有）。
	RetryAt time.Time
	// Next 不为 nil 表示这一页满了却没有选中 job：从这里继续考察下一页。
	Next *Cursor
	// Unavailable 非空表示托管池不在快照中或不可用，没有考察候选（内容即原因）。
	Unavailable string
}

// Reserve 在一个事务中考察从 after 开始的一页候选（after 为 nil 时从队首开始），为触发方的资源选出并领取（Native）或预留
// （托管）job。Native Runner 已被暂停接单、吊销时返回 ErrDrained、ErrRevoked。
func Reserve[T any](ctx context.Context, r *Reserver, t Trigger, after *Cursor, prepare Prepare[T]) (res Result[T], err error) {
	if t.Native != nil {
		if p, e := t.Native.platforms(); len(p) == 0 || len(e) == 0 {
			return res, nil
		}
	}
	// 托管池的来源在事务外读取（以后可能要访问数据库之外的状态，不放进持锁的事务）
	list, err := r.pools(ctx, t)
	if err != nil {
		return res, err
	}
	err = r.db.Tx(ctx, func(tx pgx.Tx) error {
		// 每次尝试（store.DB.Tx 遇到可重试的错误会重来）都从零建立结果和账本：上一次尝试中选中计入的不能留下
		res = Result[T]{}
		return reserveTx(ctx, r, tx, t, poolMap(list), after, prepare, &res)
	})
	return res, err
}

// pools 读取托管池的快照。Native 领取时托管池只用于等待原因（自有机器总是优先），读取失败不影响 Native 领取。
func (r *Reserver) pools(ctx context.Context, t Trigger) ([]ManagedPool, error) {
	if r.managed == nil {
		return nil, nil
	}
	list, err := r.managed.Pools(ctx)
	if err != nil {
		if t.Native != nil {
			r.log.Warn("读取托管池失败，这次领取不考虑托管池", "err", err)
			return nil, nil
		}
		return nil, fmt.Errorf("读取托管池：%w", err)
	}
	return list, nil
}

// poolMap 为一次事务尝试复制托管池的快照，账本（Running）从 0 开始，由 loadPoolLedger 按数据库填写、选择时累加。
func poolMap(list []ManagedPool) map[string]*ManagedPool {
	out := make(map[string]*ManagedPool, len(list))
	for i := range list {
		p := list[i]
		p.Running = 0
		out[p.Key] = &p
	}
	return out
}

func reserveTx[T any](ctx context.Context, r *Reserver, tx pgx.Tx, t Trigger, pools map[string]*ManagedPool, after *Cursor, prepare Prepare[T], res *Result[T]) error {
	var (
		self    Resource
		acc     *accepts
		maxJobs int
	)
	switch {
	case t.Native != nil:
		replay, err := r.lockNative(ctx, tx, t.Native)
		if err != nil {
			return err
		}
		if len(replay) > 0 {
			for _, c := range replay {
				p, err := prepare(ctx, tx, c.Job, c.AttemptID, c.Epoch)
				if err != nil {
					return err
				}
				res.Claims = append(res.Claims, Claim[T]{Job: c.Job, AttemptID: c.AttemptID, Epoch: c.Epoch, Prepared: p})
			}
			res.Replayed = true
			return nil
		}
		// 预过滤用这次上报的平台和 executor（快照中的 self 也用它们）
		n := t.Native.Runner
		n.Platforms, n.Executors = t.Native.platforms()
		acc, maxJobs = n.accepts(), t.Native.MaxJobs
	default:
		// 同一个池的预留串行：槽位不会被两个事务同时用掉（I2）
		if err := store.LockXact(ctx, tx, poolLockKey(t.Managed.Pool)); err != nil {
			return err
		}
		p := pools[t.Managed.Pool]
		if p == nil {
			res.Unavailable = cistore.ManagedPoolLabel + t.Managed.Pool + " 不存在"
			return nil
		}
		if why := p.Unusable(); why != "" {
			res.Unavailable = why
			return nil
		}
		self, acc, maxJobs = Resource{Managed: p}, p.accepts(), t.Managed.Max
	}
	cands, err := candidates(ctx, tx, acc, after)
	if err != nil || len(cands) == 0 {
		return err
	}
	// 仓库并发上限和矩阵 max-parallel 是全部资源共用的配额：检查和占用期间锁住相关仓库
	if err := lockRepoQuotas(ctx, tx, cands); err != nil {
		return err
	}
	// 锁住之后一次性预加载配额现状，选择时在内存中读取：不为一页候选逐个发查询（300–600 次）重复读同样的仓库和矩阵
	quotas, err := loadQuotas(ctx, tx, cands)
	if err != nil {
		return err
	}
	snap := &Snapshot{Pools: pools}
	if snap.Natives, err = loadNatives(ctx, tx, t.Native); err != nil {
		return err
	}
	if err := loadPoolLedger(ctx, tx, pools); err != nil {
		return err
	}
	reported := 0
	if t.Native != nil {
		self = Resource{Native: snap.Natives[t.Native.Runner.ID]}
		if t.Native.Report != nil {
			reported = t.Native.Report.Running
		}
	}
	sel := NewSelector(snap, self, time.Now(), maxJobs, reported, quotas)
	if sel.Blocked() {
		return nil
	}
	stopped := false
	for _, j := range cands {
		if sel.Full() {
			stopped = true
			break
		}
		stop, err := sel.Consider(j)
		if err != nil {
			return err
		}
		if stop {
			stopped = true
			break
		}
	}
	if err := setSkipReasons(ctx, tx, sel.Skip); err != nil {
		return err
	}
	for _, j := range sel.Chosen {
		// 先准备再领取：job 本身有问题（例如上游缺少声明的输出）时让这个 job 失败，而不是回滚整次预留——否则它每次都排在
		// 候选最前面，所有兼容的资源都领不到 job（I10）
		p, err := prepare(ctx, tx, j, 0, 0)
		if bad, ok := errors.AsType[*PrepareError](err); ok {
			r.log.Warn("无法准备 job，置为失败", "job", j.ID, "err", bad)
			// run 由 sweeper 推进（不在这里调用 AdvanceRun：预留已锁住候选 job，再锁 run 与其他路径的加锁顺序相反）
			if err := r.engine.SetTerminal(ctx, tx, j, store.JobFailed, bad.Error()); err != nil {
				return err
			}
			res.Failed++
			continue
		}
		if err != nil {
			return err
		}
		aid, epoch, err := r.claim(ctx, tx, t, self, j)
		if err != nil {
			return err
		}
		res.Claims = append(res.Claims, Claim[T]{Job: j, AttemptID: aid, Epoch: epoch, Prepared: p})
	}
	res.RetryAt = sel.RetryAt
	if len(sel.Chosen) == 0 && !stopped {
		res.Next = nextCursor(cands, CandidateLimit)
	}
	return nil
}

// applyTo 把这次上报的状态（与 runnerapi 的 touchRunner 写入的相同）用到快照中触发领取的 Runner 上。必须在统计账本之前：
// 账本是否累计资源取决于有没有容量（Cap），数据库中的容量可能已被清空（例如同名重新注册），而这次上报了容量。
func (t *NativeTrigger) applyTo(natives map[int64]*NativeRunner) {
	me, ok := natives[t.Runner.ID]
	if !ok {
		me = &NativeRunner{ID: t.Runner.ID, Used: map[string]store.Resources{}}
		natives[t.Runner.ID] = me
	}
	me.Name, me.Pool, me.Drain, me.Online = t.Runner.Name, t.Runner.Pool, t.Runner.Drain, true
	if rep := t.Report; rep != nil {
		me.Platforms, me.Executors = rep.Platforms, rep.Executors
		me.Cap, me.LegacyFree = rep.Cap, rep.LegacyFree
	}
}

// lockNative 锁住 Runner 行并重新检查状态：同一台 Runner 的领取串行进行，账本不会被并发的领取同时用掉；长轮询期间可能被
// 暂停接单或吊销，与之并发的管理操作等到领取提交后才生效。同一个 request_id 已经领取过时返回那批 job：与它重叠的长轮询
// （例如 Runner 在网络错误后用同一个 request_id 重试，旧的长轮询还没有结束）不会再分配一批。
func (r *Reserver) lockNative(ctx context.Context, tx pgx.Tx, t *NativeTrigger) ([]Claim[struct{}], error) {
	var state string
	var drain bool
	if err := tx.QueryRow(ctx, `SELECT state, drain FROM runners WHERE id=$1 FOR UPDATE`, t.Runner.ID).Scan(&state, &drain); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRevoked
		}
		return nil, err
	}
	switch {
	case state != store.RunnerActive:
		return nil, ErrRevoked
	case drain:
		return nil, ErrDrained
	}
	if t.RequestID == "" {
		return nil, nil
	}
	return Replayed(ctx, tx, t.Runner.ID, t.RequestID)
}

// Replayed 返回 Runner 用 requestID 已经领取、还在执行的那批 job（没有时为空）。
func Replayed(ctx context.Context, q store.Querier, runnerID int64, requestID string) ([]Claim[struct{}], error) {
	type claimed struct{ AttemptID, Epoch, JobID int64 }
	rows, err := q.Query(ctx, `SELECT a.id, a.epoch, a.job_id FROM attempts a JOIN jobs j ON j.id=a.job_id
		WHERE a.runner_id=$1 AND a.acquire_request_id=$2 AND j.state='running' AND j.epoch=a.epoch ORDER BY a.id`, runnerID, requestID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[claimed])
	if err != nil {
		return nil, err
	}
	out := make([]Claim[struct{}], 0, len(list))
	for _, c := range list {
		j, err := cistore.GetJob(ctx, q, c.JobID, false)
		if err != nil {
			return nil, err
		}
		out = append(out, Claim[struct{}]{Job: j, AttemptID: c.AttemptID, Epoch: c.Epoch})
	}
	return out, nil
}

// Reservation 是托管池中一个还没有提交的预留。
type Reservation struct {
	JobID, AttemptID, Epoch int64
}

// Reserved 返回托管池中还没有提交的预留（phase=reserved、仍是 job 当前的 attempt），按 attempt 排序。Server 重启后派发者
// 用它接着处理，不会为同一个 job 再建预留。
func Reserved(ctx context.Context, q store.Querier, pool string) ([]Reservation, error) {
	rows, err := q.Query(ctx, `SELECT j.id, a.id, a.epoch FROM attempts a JOIN jobs j ON j.current_attempt_id=a.id
		WHERE a.managed_pool=$1 AND a.phase='reserved' AND a.outcome IS NULL AND j.state='running' AND j.epoch=a.epoch ORDER BY a.id`, pool)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Reservation])
}

// poolLockKey 是托管池 advisory lock 的键（与仓库配额 repoQuotaLockKey、并发组的键不会重复）。
func poolLockKey(key string) string { return "kiln:ci-pool:" + key }

// setSkipReasons 批量记录 job 没有被选中的原因（与已记录的相同时不写，翻页考察长队列时不产生大量无用的更新）：一条 UPDATE
// 覆盖整页被跳过的候选，而不是逐个 job 各发一条。按 job ID 升序传入，更新顺序稳定。
func setSkipReasons(ctx context.Context, tx pgx.Tx, skip map[int64]string) error {
	if len(skip) == 0 {
		return nil
	}
	ids := slices.SortedFunc(maps.Keys(skip), cmp.Compare[int64])
	whys := make([]string, len(ids))
	for i, id := range ids {
		whys[i] = skip[id]
	}
	_, err := tx.Exec(ctx, `UPDATE jobs j SET last_skip_reason=u.why
		FROM unnest($1::bigint[], $2::text[]) AS u(id, why)
		WHERE j.id=u.id AND j.last_skip_reason IS DISTINCT FROM u.why`, ids, whys)
	return err
}

// lockRepoQuotas 锁住候选 job 所在、需要检查共用配额（仓库设置了 max_concurrency，或者 job 有矩阵 max-parallel）的仓库，
// 直到事务结束。否则两个预留事务可能都读到 running=0，各自选中后超过上限。
//
// 用事务级 advisory lock，按仓库 ID 从小到大一次锁完，多个预留事务之间不会死锁；只锁这些配额，不锁 repos 行，不影响其他
// 修改仓库的操作。锁住后再统计 running 的 job：READ COMMITTED 下每条语句读最新提交的数据，能看到先提交的事务的结果。
func lockRepoQuotas(ctx context.Context, tx pgx.Tx, cands []*cistore.Job) error {
	runIDs := make([]int64, 0, len(cands))
	var parallelRuns []int64
	for _, j := range cands {
		runIDs = append(runIDs, j.RunID)
		if j.Spec.MaxParallel > 0 {
			parallelRuns = append(parallelRuns, j.RunID)
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT r.repo_id FROM runs r JOIN repos p ON p.id=r.repo_id
		WHERE r.id = ANY($1) AND (p.max_concurrency > 0 OR r.id = ANY($2)) ORDER BY r.repo_id`, runIDs, parallelRuns)
	if err != nil {
		return err
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range repos {
		if err := store.LockXact(ctx, tx, RepoQuotaLockKey(id)); err != nil {
			return err
		}
	}
	return nil
}

// RepoQuotaLockKey 是仓库配额 advisory lock 的键（与并发组的键 cistore.LockConcurrencyGroup 不会重复）。
func RepoQuotaLockKey(repoID int64) string { return fmt.Sprintf("kiln:repo-quota:%d", repoID) }

// quotaSnapshot 是在持有仓库配额锁的事务中一次性读到的配额现状，实现 Quotas：考察一页候选时不再逐个 job 发查询。
type quotaSnapshot struct {
	repoOf   map[int64]int64 // run_id → repo_id
	max      map[int64]int   // repo_id → max_concurrency（0 为不限）
	running  map[int64]int   // repo_id → running 的 job 数（预留也计入）
	siblings map[siblingKey]int
}

// siblingKey 标识一个矩阵（一次运行的一次重跑中的一个 job 名）。
type siblingKey struct {
	runID   int64
	rerun   int
	jobName string
}

func (q *quotaSnapshot) Counts(j *cistore.Job) (QuotaCounts, error) {
	repoID := q.repoOf[j.RunID]
	c := QuotaCounts{RepoID: repoID, RepoRunning: q.running[repoID], RepoMax: q.max[repoID]}
	if j.Spec.MaxParallel > 0 {
		c.Siblings = q.siblings[siblingKey{runID: j.RunID, rerun: j.Rerun, jobName: j.JobName}]
	}
	return c, nil
}

// loadQuotas 在锁住仓库配额之后，用批量查询预加载候选 job 的配额现状：仓库并发（max_concurrency 与 running 数）以及
// （仅当候选含矩阵 max-parallel 时）各矩阵 running 的变体数。返回的 Quotas 在内存中应答 Counts。
func loadQuotas(ctx context.Context, tx pgx.Tx, cands []*cistore.Job) (*quotaSnapshot, error) {
	q := &quotaSnapshot{
		repoOf:   make(map[int64]int64, len(cands)),
		max:      map[int64]int{},
		running:  map[int64]int{},
		siblings: map[siblingKey]int{},
	}
	runIDs := make([]int64, 0, len(cands))
	seen := make(map[int64]struct{}, len(cands))
	var parallelRuns []int64
	var parallelSeen map[int64]struct{}
	for _, j := range cands {
		if _, ok := seen[j.RunID]; !ok {
			seen[j.RunID] = struct{}{}
			runIDs = append(runIDs, j.RunID)
		}
		if j.Spec.MaxParallel > 0 {
			if parallelSeen == nil {
				parallelSeen = map[int64]struct{}{}
			}
			if _, ok := parallelSeen[j.RunID]; !ok {
				parallelSeen[j.RunID] = struct{}{}
				parallelRuns = append(parallelRuns, j.RunID)
			}
		}
	}
	// run_id → repo_id
	rows, err := tx.Query(ctx, `SELECT id, repo_id FROM runs WHERE id = ANY($1)`, runIDs)
	if err != nil {
		return nil, err
	}
	var runID, repoID int64
	if _, err := pgx.ForEachRow(rows, []any{&runID, &repoID}, func() error {
		q.repoOf[runID] = repoID
		return nil
	}); err != nil {
		return nil, err
	}
	repos := make([]int64, 0, len(q.repoOf))
	repoSeen := make(map[int64]struct{}, len(q.repoOf))
	for _, id := range q.repoOf {
		if _, ok := repoSeen[id]; !ok {
			repoSeen[id] = struct{}{}
			repos = append(repos, id)
		}
	}
	// repo_id → max_concurrency
	rows, err = tx.Query(ctx, `SELECT id, max_concurrency FROM repos WHERE id = ANY($1)`, repos)
	if err != nil {
		return nil, err
	}
	var maxConc int
	if _, err := pgx.ForEachRow(rows, []any{&repoID, &maxConc}, func() error {
		q.max[repoID] = maxConc
		return nil
	}); err != nil {
		return nil, err
	}
	// repo_id → running 的 job 数（仓库并发、矩阵 max-parallel 都按 jobs.state='running' 计数，预留也计入）
	rows, err = tx.Query(ctx, `SELECT r.repo_id, count(*) FROM jobs j JOIN runs r ON r.id=j.run_id
		WHERE j.state='running' AND r.repo_id = ANY($1) GROUP BY 1`, repos)
	if err != nil {
		return nil, err
	}
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&repoID, &n}, func() error {
		q.running[repoID] = n
		return nil
	}); err != nil {
		return nil, err
	}
	if len(parallelRuns) > 0 {
		rows, err = tx.Query(ctx, `SELECT run_id, rerun, job_name, count(*) FROM jobs
			WHERE state='running' AND run_id = ANY($1) GROUP BY 1, 2, 3`, parallelRuns)
		if err != nil {
			return nil, err
		}
		var k siblingKey
		if _, err := pgx.ForEachRow(rows, []any{&k.runID, &k.rerun, &k.jobName, &n}, func() error {
			q.siblings[k] = n
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// loadNatives 读取全部 active 的 Native Runner 的能力、容量和账本。只读 ci_runners：AI 执行节点不会成为 CI 的候选（I8）。
// self 不为 nil 时，触发领取的 Runner 用它这次上报的状态，并且在统计账本之前应用（见 NativeTrigger.applyTo）。
func loadNatives(ctx context.Context, tx pgx.Tx, self *NativeTrigger) (map[int64]*NativeRunner, error) {
	rows, err := tx.Query(ctx, `SELECT r.id, r.name, c.pool, r.drain, c.platforms, c.executors, c.capacity, c.capabilities,
		coalesce(r.last_seen_at > now() - make_interval(secs => $1), false)
		FROM runners r JOIN ci_runners c ON c.runner_id=r.id WHERE r.state='active'`, store.RunnerOnlineWindow.Seconds())
	if err != nil {
		return nil, err
	}
	fl := map[int64]*NativeRunner{}
	for rows.Next() {
		f := &NativeRunner{Used: map[string]store.Resources{}}
		var capRaw, capsRaw []byte
		if err := rows.Scan(&f.ID, &f.Name, &f.Pool, &f.Drain, &f.Platforms, &f.Executors, &capRaw, &capsRaw, &f.Online); err != nil {
			rows.Close()
			return nil, err
		}
		if len(capRaw) > 0 {
			var c store.RunnerCapacity
			if json.Unmarshal(capRaw, &c) == nil {
				f.Cap = &c
			}
		}
		if f.Cap == nil {
			f.LegacyFree = LegacyFree(capsRaw)
		}
		fl[f.ID] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if self != nil {
		self.applyTo(fl)
	}
	// 账本：正在执行的 job 声明的资源（旧版 Runner 上报的空闲资源已经扣掉了它们，只计数）。托管 attempt 没有 Runner，
	// 由 loadPoolLedger 统计
	lrows, err := tx.Query(ctx, `SELECT a.runner_id, j.executor_kind, count(*),
		count(*) FILTER (WHERE j.executor_kind='host' AND jsonb_array_length(coalesce(j.spec->'secrets','[]'))>0),
		coalesce(sum((j.spec->>'cpu')::float8), 0), coalesce(sum((j.spec->>'memory_bytes')::bigint), 0)
		FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id WHERE j.state='running' AND a.runner_id IS NOT NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var id int64
		var executor string
		var n, excl int
		var used store.Resources
		if err := lrows.Scan(&id, &executor, &n, &excl, &used.CPU, &used.Memory); err != nil {
			return nil, err
		}
		f, ok := fl[id]
		if !ok {
			continue
		}
		f.Running += n
		f.Exclusive += excl
		if f.Cap != nil {
			f.Used[executor] = f.Used[executor].Add(used)
		}
	}
	return fl, lrows.Err()
}

// loadPoolLedger 统计每个托管池的账本：当前 attempt 属于这个池、job 处于 running 的数量（phase 不限：预留、已提交、执行中、
// 结果未知都占用执行槽）。
func loadPoolLedger(ctx context.Context, tx pgx.Tx, pools map[string]*ManagedPool) error {
	if len(pools) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT a.managed_pool, count(*) FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id
		WHERE j.state='running' AND a.managed_pool IS NOT NULL GROUP BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		if p := pools[key]; p != nil {
			p.Running = n
		}
	}
	return rows.Err()
}

// LegacyFree 解析旧版 Runner 上报、记录在 ci_runners.capabilities 中的空闲资源；没有上报时返回 nil。
func LegacyFree(raw []byte) *store.Resources {
	var caps struct {
		CPU    *float64 `json:"free_cpu"`
		Memory int64    `json:"free_memory"`
	}
	if json.Unmarshal(raw, &caps) != nil || caps.CPU == nil || *caps.CPU <= 0 {
		return nil
	}
	return &store.Resources{CPU: *caps.CPU, Memory: caps.Memory}
}

// claim 领取（Native）或预留（托管）一个选中的 job：同一事务中置为 running、epoch+1、创建 attempt（PLAN #32.6）。
//   - Native：签发租约、写 started_at（领取即开始执行），attempt 为 running；
//   - 托管：租约为预留期限，started_at 为空（还没有开始执行，Check Run 显示 queued），attempt 为 reserved。
func (r *Reserver) claim(ctx context.Context, tx pgx.Tx, t Trigger, self Resource, j *cistore.Job) (attemptID, epoch int64, err error) {
	var (
		runnerID  *int64
		pool      *string
		requestID string
		phase     = cistore.PhaseRunning
		reason    string
	)
	started := t.Native != nil
	if started {
		runnerID, requestID = &t.Native.Runner.ID, t.Native.RequestID
		reason = "在 Runner " + t.Native.Runner.Name + " 上执行"
	} else {
		pool, phase = &self.Managed.Key, cistore.PhaseReserved
		reason = "已预留到" + self.Managed.label() + "，等待开始执行"
	}
	var startedAt *time.Time
	if err := tx.QueryRow(ctx, `UPDATE jobs SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
		lease_expires_at=now()+make_interval(secs => $2), started_at=CASE WHEN $4 THEN now() END, started_step=false, last_skip_reason=NULL,
		reason=$3 WHERE id=$1 RETURNING epoch, started_at`, j.ID, r.cfg.LeaseTTL.Seconds(), reason, started).Scan(&epoch, &startedAt); err != nil {
		return 0, 0, err
	}
	var aid int64
	if err := tx.QueryRow(ctx, `INSERT INTO attempts (job_id, epoch, runner_id, managed_pool, phase, acquire_request_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, j.ID, epoch, runnerID, pool, phase, requestID).Scan(&aid); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET current_attempt_id=$2 WHERE id=$1`, j.ID, aid); err != nil {
		return 0, 0, err
	}
	if started {
		if _, err := tx.Exec(ctx, `UPDATE run_reruns SET started_at=coalesce(started_at, now()) WHERE run_id=$1 AND rerun=$2`, j.RunID, j.Rerun); err != nil {
			return 0, 0, err
		}
	}
	j.State, j.Epoch, j.CurrentAttemptID, j.StartedAt, j.Reason = store.JobRunning, epoch, &aid, startedAt, &reason
	j.AttemptCount++
	return aid, epoch, cicheck.EnqueueJob(ctx, r.outbox, tx, j)
}

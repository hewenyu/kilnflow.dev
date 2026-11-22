package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 测试环境：真实的 Postgres（pgtest）、Engine 和 Reserver；托管池由 fakePools 提供（PLAN #32.10）。Native Runner 直接写入
// runners 和 ci_runners，领取直接调用 Reserve（Native 的协议适配由 runnerapi 的测试覆盖）。

const (
	testLeaseTTL = 2 * time.Minute
	gib          = int64(1 << 30)
	// testPoolKey 是默认托管池的 Key，testPoolName 是它的 pool 名称（job 的 pool 指向它即只在托管上执行）。
	testPoolKey  = "gcp-a"
	testPoolName = "cloud"
)

// fakePools 是托管池的来源（ManagedSource 的替身），测试中可以随时修改。
type fakePools struct {
	mu   sync.Mutex
	list []ManagedPool
	err  error
}

func (f *fakePools) Pools(context.Context) ([]ManagedPool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ManagedPool(nil), f.list...), f.err
}

func (f *fakePools) set(list ...ManagedPool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = list
}

// pool 返回一个可用的托管池：linux 的 oci、具备 services 和 effects、每个执行槽 4 CPU / 16 GiB。
func pool(key string, maxConcurrent int, opts ...func(*ManagedPool)) ManagedPool {
	p := ManagedPool{
		Key: key, Name: key, Pool: testPoolName, Platforms: []string{"linux/arm64", "linux/amd64"}, Executors: []string{"oci"},
		Caps: []string{CapServices, CapEffects}, PerJob: store.Resources{CPU: 4, Memory: 16 * gib}, MaxConcurrent: maxConcurrent, State: PoolReady,
	}
	for _, f := range opts {
		f(&p)
	}
	return p
}

type harness struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	eng   *engine.Engine
	ob    *outbox.Writer
	pools *fakePools
	r     *Reserver
	seq   atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), ob: outbox.NewWriter(true), pools: &fakePools{}}
	h.restart()
	return h
}

// restart 模拟 Server 重启：新建 Engine 和 Reserver（数据库不变）。
func (h *harness) restart() {
	log := slog.New(slog.DiscardHandler)
	h.eng = engine.New(engine.Config{InfraRetryMax: 2, LeaseTTL: testLeaseTTL, CancelGrace: 30 * time.Second, SweepInterval: time.Hour},
		engine.Deps{DB: h.db, CAS: &cas.FSStore{Root: h.t.TempDir()}, Outbox: h.ob, Hub: notify.NewHub(), Log: log})
	h.r = New(Config{LeaseTTL: testLeaseTTL}, Deps{DB: h.db, Engine: h.eng, Outbox: h.ob, Managed: h.pools, Log: log})
}

func (h *harness) next() int64 { return h.seq.Add(1) }

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

func (h *harness) queryInt(sql string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.db.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
	return n
}

// nativeOpts 描述 Native Runner。
type nativeOpts struct {
	pool      string
	platforms []string
	executors []string
	cap       *store.RunnerCapacity
}

// addNative 登记一台在线的 CI Runner（默认 linux/arm64、oci、8 CPU / 32 GiB、max_jobs 4），返回它的领取触发方。
func (h *harness) addNative(opts ...func(*nativeOpts)) *NativeTrigger {
	h.t.Helper()
	o := nativeOpts{
		pool: "default", platforms: []string{"linux/arm64"}, executors: []string{"oci"},
		cap: &store.RunnerCapacity{Machine: store.Resources{CPU: 8, Memory: 32 * gib}, MaxJobs: 4},
	}
	for _, f := range opts {
		f(&o)
	}
	name := fmt.Sprintf("native-%d", h.next())
	var id int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runners (name, public_key, last_seen_at) VALUES ($1, '\x00', now()) RETURNING id`, name).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO ci_runners (runner_id, pool, platforms, executors, capacity) VALUES ($1,$2,$3,$4,$5)`,
		id, o.pool, o.platforms, o.executors, store.MustJSON(o.cap))
	return &NativeTrigger{
		Runner:    NativeRunner{ID: id, Name: name, Pool: o.pool, Platforms: o.platforms, Executors: o.executors},
		Report:    &NativeReport{Platforms: o.platforms, Executors: o.executors, Cap: o.cap},
		RequestID: fmt.Sprintf("req-%d", h.next()),
		MaxJobs:   o.cap.MaxJobs,
	}
}

// addRepo 登记一个仓库，返回仓库 ID。
func (h *harness) addRepo(maxConcurrency int) int64 {
	h.t.Helper()
	id := 1000 + h.next()
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, max_concurrency) VALUES ($1, 1, 1, 'o', $2, 'x', $3)`,
		id, fmt.Sprintf("r%d", id), maxConcurrency)
	return id
}

// addRun 在仓库中创建一个 active 的 run（rerun 0），返回 run ID。
func (h *harness) addRun(repoID int64) int64 {
	h.t.Helper()
	var id int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state)
		VALUES ($1, 'ci.yml', 'push', 'refs/heads/main', 'abc', 'webhook', $2, 'active') RETURNING id`, repoID, fmt.Sprintf("run-%d", h.next())).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, 'active')`, id)
	return id
}

// jobRow 描述要插入的 job。
type jobRow struct {
	name      string
	matrix    map[string]any
	spec      workflow.Spec
	effects   bool
	actionKey string
	state     store.JobState
	// fresh 为 true 时刚刚排队（延迟调度生效）；默认排队已超过 SchedDelay。
	fresh    bool
	priority int
	// submitted 是提交时间（jobs.created_at）；nil 时为插入的时间。
	submitted *time.Time
	// needs 是上游 job；rerun 是所属的那次运行。
	needs []int64
	rerun int
}

// addJob 在 run 中插入一个 job（默认 pending、linux/arm64、oci、1 CPU / 1 GiB），返回 job ID。
func (h *harness) addJob(runID int64, opts ...func(*jobRow)) int64 {
	h.t.Helper()
	n := h.next()
	j := jobRow{
		name: fmt.Sprintf("job%d", n), state: store.JobPending, priority: 1,
		spec: workflow.Spec{
			Platform: "linux/arm64", Executor: workflow.ExecutorOCI, Container: "alpine", CPU: 1, MemoryBytes: gib,
			Steps: []workflow.StepSpec{{Name: "build", Run: "make", Shell: "sh"}}, TimeoutMinutes: 10,
		},
	}
	for _, f := range opts {
		f(&j)
	}
	var matrix []byte
	if j.matrix != nil {
		matrix = store.MustJSON(j.matrix)
	}
	j.spec.Effects = j.effects
	queuedAgo := 2 * SchedDelay
	if j.fresh {
		queuedAgo = 0
	}
	var id int64
	if j.needs == nil {
		j.needs = []int64{}
	}
	// waiting 的 job 没有 queued_at（就绪时由 engine.MarkPending 写入）
	err := h.db.QueryRow(h.ctx, `INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state,
		priority, effects, action_key, input_digest, queued_at, created_at, needs, rerun)
		VALUES ('normal', $1, $2, $3, $2, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), 'in',
		CASE WHEN $8 <> 'waiting' THEN now() - make_interval(secs => $12) END, coalesce($13, now()), $14, $15) RETURNING id`,
		runID, fmt.Sprintf("%s-%d", j.name, n), j.name, matrix, store.MustJSON(j.spec), j.spec.Platform, j.spec.Executor, j.state,
		j.priority, j.effects, j.actionKey, queuedAgo.Seconds(), j.submitted, j.needs, j.rerun).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// pendingJob 是"一个仓库、一个 run、一个 pending job"的快捷方式。
func (h *harness) pendingJob(opts ...func(*jobRow)) int64 {
	h.t.Helper()
	return h.addJob(h.addRun(h.addRepo(8)), opts...)
}

func inPool(p string) func(*jobRow) { return func(j *jobRow) { j.spec.Pool = p } }

func fresh(j *jobRow) { j.fresh = true }

// submittedAt 设置 job 的提交时间。
func submittedAt(at time.Time) func(*jobRow) { return func(j *jobRow) { j.submitted = &at } }

// noPrepare 是不需要准备的触发方的 Prepare 回调。
func noPrepare(context.Context, store.Querier, *cistore.Job, int64, int64) (struct{}, error) {
	return struct{}{}, nil
}

// reserveManaged 为托管池 key 预留最多 maxJobs 个 job（一页候选）。
func (h *harness) reserveManaged(key string, maxJobs int, after *Cursor) Result[struct{}] {
	h.t.Helper()
	res, err := Reserve(h.ctx, h.r, Trigger{Managed: &ManagedTrigger{Pool: key, Max: maxJobs}}, after, noPrepare)
	if err != nil {
		h.t.Fatalf("托管预留：%v", err)
	}
	return res
}

// acquireNative 为 Native Runner 领取（一页候选，每次用新的 request_id）。
func (h *harness) acquireNative(t *NativeTrigger, maxJobs int) Result[struct{}] {
	h.t.Helper()
	tr := *t
	tr.MaxJobs, tr.RequestID = maxJobs, fmt.Sprintf("req-%d", h.next())
	res, err := Reserve(h.ctx, h.r, Trigger{Native: &tr}, nil, noPrepare)
	if err != nil {
		h.t.Fatalf("Native 领取：%v", err)
	}
	return res
}

// jobState 是测试关心的 job 字段。
type jobState struct {
	State      store.JobState
	Epoch      int64
	Attempts   int
	StartedAt  *time.Time
	Lease      *time.Time
	QueuedAt   *time.Time
	Reason     string
	SkipReason string
	Follows    *int64
	// Pool、Phase、Runner 是当前 attempt 的（没有时为空）。
	Pool   string
	Phase  string
	Runner *int64
}

func (h *harness) job(id int64) jobState {
	h.t.Helper()
	var s jobState
	err := h.db.QueryRow(h.ctx, `SELECT j.state, j.epoch, (SELECT count(*) FROM attempts WHERE job_id=j.id), j.started_at, j.lease_expires_at, j.queued_at,
		coalesce(j.reason, ''), coalesce(j.last_skip_reason, ''), j.follows_job_id, coalesce(a.managed_pool, ''), coalesce(a.phase, ''), a.runner_id
		FROM jobs j LEFT JOIN attempts a ON a.id=j.current_attempt_id WHERE j.id=$1`, id).
		Scan(&s.State, &s.Epoch, &s.Attempts, &s.StartedAt, &s.Lease, &s.QueuedAt, &s.Reason, &s.SkipReason, &s.Follows, &s.Pool, &s.Phase, &s.Runner)
	if err != nil {
		h.t.Fatalf("读取 job %d：%v", id, err)
	}
	return s
}

// claimedIDs 返回结果中的 job ID。
func claimedIDs[T any](res Result[T]) []int64 {
	out := make([]int64, 0, len(res.Claims))
	for _, c := range res.Claims {
		out = append(out, c.Job.ID)
	}
	return out
}

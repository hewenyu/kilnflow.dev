package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 测试用的配置：与默认值一致（PLAN #6.3），sweeper 的间隔很长，由测试直接调用各阶段。
const (
	testInfraRetryMax = 2
	testLeaseTTL      = 60 * time.Second
	testCancelGrace   = 30 * time.Second
	testRepoID        = 101
	testPlatform      = "linux/amd64"
)

// env 是一个测试的数据库、CAS 和 Engine。每个测试使用独立的数据库，可以并行。
type env struct {
	t        *testing.T
	ctx      context.Context
	db       *store.DB
	cas      *cas.FSStore
	hub      *notify.Hub
	eng      *Engine
	runnerID int64
	// seq 生成唯一的 dedupe_key；子测试共用同一个计数器。
	seq *atomic.Int64
}

// newEnv 准备数据库（含一个仓库和一个 Runner）和 Engine。
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), cas: &cas.FSStore{Root: t.TempDir()}, hub: notify.NewHub(), seq: new(atomic.Int64)}
	e.eng = New(Config{
		InfraRetryMax:  testInfraRetryMax,
		LeaseTTL:       testLeaseTTL,
		CancelGrace:    testCancelGrace,
		SweepInterval:  time.Hour,
		NonceRetention: 10 * time.Minute,
	}, Deps{DB: e.db, CAS: e.cas, Outbox: outbox.NewWriter(true), Hub: e.hub, Log: slog.New(slog.DiscardHandler)})
	if err := store.UpsertRepo(e.ctx, e.db, store.RepoMeta{
		ID: testRepoID, InstallationID: 1, OwnerID: 1, Owner: "o", Name: "r", Private: true,
		DefaultBranch: "main", CloneURL: "https://example.invalid/o/r.git",
	}); err != nil {
		t.Fatalf("写入仓库：%v", err)
	}
	e.exec(`INSERT INTO runners (name, public_key) VALUES ('r1', '\x00')`)
	e.runnerID = e.queryInt(`SELECT id FROM runners WHERE name='r1'`)
	return e
}

// sub 返回在子测试 t 中使用同一个数据库的 env（断言失败时终止的是子测试）。
func (e *env) sub(t *testing.T) *env {
	t.Helper()
	c := *e
	c.t, c.ctx = t, t.Context()
	return &c
}

// exec 执行一条 SQL，失败时终止测试。
func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("执行 %q：%v", sql, err)
	}
}

// queryInt 执行只返回一个整数的查询。
func (e *env) queryInt(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("查询 %q：%v", sql, err)
	}
	return n
}

// tx 在一个事务中执行 f，失败时终止测试。
func (e *env) tx(f func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := e.db.Tx(e.ctx, f); err != nil {
		e.t.Fatalf("事务失败：%v", err)
	}
}

// runOpts 是 newRun 的参数。
type runOpts struct {
	state       store.RunState // 默认 active
	concurrency string
}

// newRun 插入一个 run（及其第 0 次运行），返回 run ID。
func (e *env) newRun(o runOpts) int64 {
	e.t.Helper()
	if o.state == "" {
		o.state = store.RunActive
	}
	var key *string
	if o.concurrency != "" {
		key = &o.concurrency
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, concurrency_key, state)
		VALUES ($1, 'ci.yml', 'push', 'refs/heads/main', 'sha1', 'webhook', $2, $3, $4) RETURNING id`,
		testRepoID, fmt.Sprintf("test:%d", e.seq.Add(1)), key, o.state).Scan(&id)
	if err != nil {
		e.t.Fatalf("插入 run：%v", err)
	}
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, $2)`, id, o.state)
	return id
}

// jobOpts 是 addJob 的参数。零值是一个没有上游、不可缓存的普通 job。
type jobOpts struct {
	key     string // 默认与 name 相同
	name    string // job_name，默认 "build"
	kind    workflow.JobKind
	matrix  map[string]any
	needs   []int64
	state   store.JobState // 默认 waiting
	spec    workflow.Spec  // Executor、Platform 为空时填默认值
	effects bool
	force   bool
	input   string // input_digest，默认 "in"
}

// addJob 向 run 的第 0 次运行插入一个 job，返回 job ID。
func (e *env) addJob(runID int64, o jobOpts) int64 {
	e.t.Helper()
	if o.name == "" {
		o.name = "build"
	}
	if o.key == "" {
		o.key = o.name
	}
	if o.kind == "" {
		o.kind = workflow.JobKindNormal
	}
	if o.state == "" {
		o.state = store.JobWaiting
	}
	if o.input == "" {
		o.input = "in"
	}
	sp := o.spec
	if sp.Executor == "" {
		sp.Executor = workflow.ExecutorHost
	}
	if sp.Platform == "" {
		sp.Platform = testPlatform
	}
	if sp.TimeoutMinutes == 0 {
		sp.TimeoutMinutes = workflow.DefaultTimeoutMinutes
	}
	sp.JobID, sp.Effects = o.name, o.effects
	matrix := []byte("null")
	if len(o.matrix) > 0 {
		matrix = store.MustJSON(o.matrix)
	}
	needs := o.needs
	if needs == nil {
		needs = []int64{}
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, matrix, spec, platform, executor_kind,
		needs, input_digest, state, effects, force_execute, finished_at)
		VALUES ($1,$2,$3,$4,$3,$5,$6,$7,$8,$9,$10,$11,$12,$13, CASE WHEN $14 THEN now() END) RETURNING id`,
		o.kind, runID, o.key, o.name, matrix, store.MustJSON(sp), sp.Platform, sp.Executor,
		needs, o.input, o.state, o.effects, o.force, o.state.Terminal()).Scan(&id)
	if err != nil {
		e.t.Fatalf("插入 job %s：%v", o.key, err)
	}
	return id
}

// advance 在一个事务中推进 run。
func (e *env) advance(runID int64) {
	e.t.Helper()
	e.tx(func(tx pgx.Tx) error { return e.eng.AdvanceRun(e.ctx, tx, runID) })
}

// claim 模拟 Runner 领取 pending 的 job（与 runnerapi.claimJob 相同：epoch+1、创建 attempt、签发租约），返回 attempt ID。
func (e *env) claim(jobID int64) int64 {
	e.t.Helper()
	if s := e.job(jobID).State; s != store.JobPending {
		e.t.Fatalf("job %d 的状态为 %s，只能领取 pending 的 job", jobID, s)
	}
	var aid int64
	e.tx(func(tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(e.ctx, `UPDATE jobs SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
			lease_expires_at=now()+make_interval(secs => $2), started_at=now(), started_step=false WHERE id=$1 RETURNING epoch`,
			jobID, testLeaseTTL.Seconds()).Scan(&epoch); err != nil {
			return err
		}
		if err := tx.QueryRow(e.ctx, `INSERT INTO attempts (job_id, epoch, runner_id, acquire_request_id) VALUES ($1,$2,$3,'req') RETURNING id`,
			jobID, epoch, e.runnerID).Scan(&aid); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `UPDATE jobs SET current_attempt_id=$2 WHERE id=$1`, jobID, aid)
		return err
	})
	return aid
}

// finish 在一个事务中锁住 job 并结束它当前的 attempt。
func (e *env) finish(jobID int64, res AttemptResult) {
	e.t.Helper()
	e.tx(func(tx pgx.Tx) error {
		runs, err := cistore.LockRunsOfJob(e.ctx, tx, jobID)
		if err != nil {
			return err
		}
		j, err := cistore.GetJob(e.ctx, tx, jobID, true)
		if err != nil {
			return err
		}
		return e.eng.FinishAttempt(e.ctx, tx, j, res, runs)
	})
}

// succeed 领取并成功结束 job，结果 manifest 为 manifest（可以为空）。
func (e *env) succeed(jobID int64, manifest string) {
	e.t.Helper()
	e.claim(jobID)
	e.finish(jobID, AttemptResult{Outcome: store.OutcomeSucceeded, Manifest: manifest})
}

// job 读取 job 的当前状态。
func (e *env) job(id int64) *cistore.Job {
	e.t.Helper()
	j, err := cistore.GetJob(e.ctx, e.db, id, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// run 读取 run 的当前状态。
func (e *env) run(id int64) *cistore.Run {
	e.t.Helper()
	r, err := cistore.GetRun(e.ctx, e.db, id, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// wantJobState 断言 job 的状态。
func (e *env) wantJobState(id int64, want store.JobState) *cistore.Job {
	e.t.Helper()
	j := e.job(id)
	if j.State != want {
		e.t.Fatalf("job %d（%s）的状态为 %s（原因 %q），期望 %s", id, j.JobKey, j.State, deref(j.Reason), want)
	}
	return j
}

// wantRunState 断言 run 及其当前运行（run_reruns）的状态和结论。run 未结束时 conclusion 应为空。
func (e *env) wantRunState(id int64, want store.RunState) {
	e.t.Helper()
	r := e.run(id)
	if r.State != want {
		e.t.Fatalf("run %d 的状态为 %s，期望 %s", id, r.State, want)
	}
	var state string
	var conclusion *string
	if err := e.db.QueryRow(e.ctx, `SELECT state, conclusion FROM run_reruns WHERE run_id=$1 AND rerun=$2`, id, r.CurrentRerun).
		Scan(&state, &conclusion); err != nil {
		e.t.Fatal(err)
	}
	wantConclusion := ""
	if c := want.ConclusionOrNil(); c != nil {
		wantConclusion = *c
	}
	if state != string(want) || deref(conclusion) != wantConclusion {
		e.t.Fatalf("run %d 第 %d 次运行的状态为 %s / %q，期望 %s / %q", id, r.CurrentRerun, state, deref(conclusion), want, wantConclusion)
	}
	if want.Terminal() != (r.FinishedAt != nil) {
		e.t.Fatalf("run %d 的 finished_at = %v，终态 = %v", id, r.FinishedAt, want.Terminal())
	}
}

// wantOutbox 断言 outbox 中有 dedupe_key 为 key 的待投递记录。
func (e *env) wantOutbox(key string) {
	e.t.Helper()
	if n := e.queryInt(`SELECT count(*) FROM outbox WHERE dedupe_key=$1 AND state='pending'`, key); n != 1 {
		e.t.Fatalf("outbox 中应有 %s", key)
	}
}

// jobCheckKey、runCheckKey 是 outbox 中 Check Run 同步记录的 dedupe_key。
func jobCheckKey(id int64) string            { return fmt.Sprintf("checkrun:job:%d", id) }
func runCheckKey(id int64, rerun int) string { return fmt.Sprintf("checkrun:run:%d:%d", id, rerun) }

// attempt 是 attempts 表中的一行。
type attempt struct {
	Epoch    int64
	Outcome  *string
	ErrClass *string
	Message  *string
}

// attempts 返回 job 的全部 attempt，按 epoch 排序。
func (e *env) attempts(jobID int64) []attempt {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT epoch, outcome, error_class, message FROM attempts WHERE job_id=$1 ORDER BY epoch`, jobID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[attempt])
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// putJSON 把 v 以 JSON 写入 CAS，返回 digest。
func (e *env) putJSON(v any) string {
	e.t.Helper()
	b := store.MustJSON(v)
	d := cas.Digest(b)
	if _, err := e.cas.Put(e.ctx, d, bytes.NewReader(b)); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// resultManifest 写入一个结果 manifest（输出名 → 输出 manifest 的 digest），返回它的 digest。
func (e *env) resultManifest(outputs map[string]string) string {
	e.t.Helper()
	return e.putJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: outputs})
}

// cacheEntry 是 action_cache 中的一行。
type cacheEntry struct {
	Manifest string
	Producer int64
	Suspect  bool
	Hit      bool
}

// cacheEntry 返回 action key 的缓存条目；不存在时返回 nil。
func (e *env) cacheEntry(key string) *cacheEntry {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT manifest_digest, producer_job_id, suspect, last_hit_at IS NOT NULL FROM action_cache WHERE action_key=$1`, key)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[cacheEntry])
	if err != nil {
		e.t.Fatal(err)
	}
	if len(out) == 0 {
		return nil
	}
	return &out[0]
}

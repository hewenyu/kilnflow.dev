package scheduler

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 排队顺序（PLAN #14.2）：同 priority 内先提交的 run 先，后继 job 继承 run 的提交时间；重跑按重跑的时间；基础设施重试、撤销预留
// 不改变提交时间；不引入队首阻塞，翻页按同一个排序键。

// oneSlot 是只能同时执行一个 job 的 Runner（issue 中的 linux/arm64 机器）。
func oneSlot(o *nativeOpts) { o.cap.MaxJobs = 1 }

// waitingOn 是等待上游 up 的 job（action key 预先给定，就绪时不需要计算）。
func waitingOn(up int64) func(*jobRow) {
	return func(j *jobRow) {
		j.state, j.needs, j.actionKey = store.JobWaiting, []int64{up}, fmt.Sprintf("key-after-%d", up)
	}
}

// acquireOne 为 Runner 领取一次，期望领到 want（want 为 0 时期望没有领到）。
func (h *harness) acquireOne(n *NativeTrigger, want int64, what string) {
	h.t.Helper()
	got := claimedIDs(h.acquireNative(n, 1))
	if want == 0 {
		if len(got) != 0 {
			h.t.Fatalf("%s：不应领到 job，实际 %v", what, got)
		}
		return
	}
	if !slices.Equal(got, []int64{want}) {
		h.t.Fatalf("%s：应领到 job %d，实际 %v", what, want, got)
	}
}

// succeed 让 running 的 job 成功并推进它的 run（下游就绪时由 engine.MarkPending 写 queued_at=now()）。
func (h *harness) succeed(jobID int64) {
	h.t.Helper()
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		var runID int64
		if err := tx.QueryRow(h.ctx, `UPDATE jobs SET state='succeeded', finished_at=now(), lease_expires_at=NULL WHERE id=$1 AND state='running'
			RETURNING run_id`, jobID).Scan(&runID); err != nil {
			return err
		}
		return h.eng.AdvanceRun(h.ctx, tx, runID)
	})
	if err != nil {
		h.t.Fatalf("结束 job %d：%v", jobID, err)
	}
}

// TestEarlierRunFirst（issue 的回归场景）：单 Runner、同 priority，Run A 先提交（A1 → A2 → A3），Run B 后提交（B1、B2）。
// A1 执行期间 B 的 job 已在排队；A1 结束、A2 就绪（queued_at 比 B 的晚）后，下一次领取拿到 A2；之后不断有新的 run 提交，
// A 的尾部仍然先于它们，A 结束后按提交顺序轮到 B、C。
func TestEarlierRunFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative(oneSlot)
	repo := h.addRepo(0)
	base := time.Now().Add(-time.Hour)
	runA := h.addRun(repo)
	a1 := h.addJob(runA, submittedAt(base))
	a2 := h.addJob(runA, submittedAt(base), waitingOn(a1))
	a3 := h.addJob(runA, submittedAt(base), waitingOn(a2))
	runB := h.addRun(repo)
	b1 := h.addJob(runB, submittedAt(base.Add(time.Minute)))
	b2 := h.addJob(runB, submittedAt(base.Add(time.Minute)))

	h.acquireOne(runner, a1, "A1 最早提交")
	h.acquireOne(runner, 0, "A1 执行期间 Runner 没有空槽")
	h.succeed(a1)
	if s, b := h.job(a2), h.job(b1); s.State != store.JobPending || !s.QueuedAt.After(*b.QueuedAt) {
		t.Fatalf("A2 应在 B1 之后才进入 pending：A2 %s %v，B1 %v", s.State, s.QueuedAt, b.QueuedAt)
	}
	h.acquireOne(runner, a2, "A2 就绪后先于更晚提交的 B")

	// A2 执行期间又提交了 Run C：A 的尾部仍先于它
	runC := h.addRun(repo)
	c1 := h.addJob(runC, submittedAt(base.Add(2*time.Minute)))
	h.succeed(a2)
	h.acquireOne(runner, a3, "A3 先于更晚提交的 B、C")
	h.succeed(a3)
	h.acquireOne(runner, b1, "A 结束后轮到 B")
	h.succeed(b1)
	h.addJob(h.addRun(repo), submittedAt(base.Add(3*time.Minute)))
	h.acquireOne(runner, b2, "B 的另一个 job 先于 C、D")
	h.succeed(b2)
	h.acquireOne(runner, c1, "然后是 C")
}

// TestPriorityBeforeSubmission：显式 priority 仍然最先比较：晚提交的高 priority job 先于早提交的低 priority job。
func TestPriorityBeforeSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative(oneSlot)
	repo := h.addRepo(0)
	base := time.Now().Add(-time.Hour)
	early := h.addJob(h.addRun(repo), submittedAt(base))
	late := h.addJob(h.addRun(repo), submittedAt(base.Add(time.Minute)), func(j *jobRow) { j.priority = 3 })
	h.acquireOne(runner, late, "高 priority 先")
	h.succeed(late)
	h.acquireOne(runner, early, "然后是早提交的低 priority")
}

// TestNoHeadOfLineBlocking：早提交的 run 中平台不兼容、资源不可满足的 job 不挡住晚提交的 run 中兼容的 job。
func TestNoHeadOfLineBlocking(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative(oneSlot)
	repo := h.addRepo(0)
	base := time.Now().Add(-time.Hour)
	early := h.addRun(repo)
	amd64 := h.addJob(early, submittedAt(base), func(j *jobRow) { j.spec.Platform = "linux/amd64" })
	huge := h.addJob(early, submittedAt(base), func(j *jobRow) { j.spec.CPU = 64 })
	late := h.addJob(h.addRun(repo), submittedAt(base.Add(time.Minute)))
	h.acquireOne(runner, late, "晚提交但兼容的 job")
	if s := h.job(huge); s.State != store.JobPending || !strings.Contains(s.SkipReason, "CPU") {
		t.Fatalf("资源不可满足的 job 应留在队列并记下原因：%+v", s)
	}
	if s := h.job(amd64); s.State != store.JobPending {
		t.Fatalf("平台不兼容的 job 应留在队列：%+v", s)
	}
}

// TestRerunQueuesAtRerunTime：重跑插入新的 job 行，按重跑的时间排队；不继承原 run 的提交时间（否则对很旧的 run 点重跑会插到
// 所有人前面）。
func TestRerunQueuesAtRerunTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative(oneSlot)
	repo := h.addRepo(0)
	base := time.Now().Add(-time.Hour)
	old := h.addRun(repo)
	h.addJob(old, submittedAt(base), func(j *jobRow) { j.state = store.JobFailed })
	between := h.addJob(h.addRun(repo), submittedAt(base.Add(time.Minute)))
	h.exec(`UPDATE runs SET current_rerun=1 WHERE id=$1`, old)
	h.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 1, 'active')`, old)
	rerun := h.addJob(old, submittedAt(base.Add(2*time.Minute)), func(j *jobRow) { j.rerun = 1 })
	// 重跑的 job 的 queued_at 早于 between（例如 between 刚做了基础设施重试）：仍按提交时间排在后面
	h.exec(`UPDATE jobs SET queued_at=now() - interval '1 hour' WHERE id=$1`, rerun)
	h.acquireOne(runner, between, "重跑之前提交的 run 先")
	h.succeed(between)
	h.acquireOne(runner, rerun, "然后是重跑")
}

// TestRequeueKeepsSubmission：基础设施重试（FinishAttempt 回到 pending，queued_at 更新）和撤销托管预留（Release）后，job
// 仍排在更晚提交的 run 前面。
func TestRequeueKeepsSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 1))
	runner := h.addNative(oneSlot)
	repo := h.addRepo(0)
	base := time.Now().Add(-time.Hour)
	early := h.addJob(h.addRun(repo), submittedAt(base))
	late := h.addJob(h.addRun(repo), submittedAt(base.Add(time.Minute)))

	h.acquireOne(runner, early, "早提交的先")
	h.requeue(early, func(tx pgx.Tx, j *cistore.Job, runs []int64) error {
		return h.eng.FinishAttempt(h.ctx, tx, j, engine.AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost"}, runs)
	})
	if s, l := h.job(early), h.job(late); s.State != store.JobPending || s.Attempts != 1 || !s.QueuedAt.After(*l.QueuedAt) {
		t.Fatalf("基础设施重试后应回到 pending，queued_at 晚于后提交的 job：%+v", s)
	}
	h.acquireOne(runner, early, "基础设施重试后仍先于更晚提交的 run")

	// 撤销托管预留：同样不退到更晚的 run 之后
	h.succeed(early)
	again := h.addJob(h.addRun(repo), submittedAt(base), func(j *jobRow) { j.spec.CPU = 2 })
	if got := claimedIDs(h.reserveManaged(testPoolKey, 1, nil)); !slices.Equal(got, []int64{again}) {
		t.Fatalf("托管应预留早提交的 job %d：%v", again, got)
	}
	h.requeue(again, func(tx pgx.Tx, j *cistore.Job, _ []int64) error { return h.eng.Release(h.ctx, tx, j, "池被停用") })
	h.acquireOne(runner, again, "撤销预留后仍先于更晚提交的 run")
}

// requeue 在加锁的事务中对 running 的 job 执行 f（让它回到 pending）。
func (h *harness) requeue(jobID int64, f func(pgx.Tx, *cistore.Job, []int64) error) {
	h.t.Helper()
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		runs, err := cistore.LockRunsOfJob(h.ctx, tx, jobID)
		if err != nil {
			return err
		}
		j, err := cistore.GetJob(h.ctx, tx, jobID, true)
		if err != nil {
			return err
		}
		return f(tx, j, runs)
	})
	if err != nil {
		h.t.Fatalf("job %d 回到队列：%v", jobID, err)
	}
	if s := h.job(jobID); s.State != store.JobPending {
		h.t.Fatalf("job %d 应回到 pending：%+v", jobID, s)
	}
}

// TestPagingByQueueKey：候选超过 CandidateLimit 时按排队顺序翻页，不漏不重。页的边界落在提交时间相同、queued_at 不同的
// 两个 job 之间，和提交时间、queued_at 都相同只有 id 不同的两个 job 之间；id 的顺序与排队顺序相反，晚提交的 job 的 queued_at
// 最早。前面的 job 都放不下时，最后一页中唯一放得下的 job 在翻页后被领取。
func TestPagingByQueueKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative()
	run := h.addRun(h.addRepo(0))
	base := time.Now().Add(-time.Hour)
	t1, t2 := base, base.Add(time.Minute)
	q0, q1, q2 := base.Add(-time.Hour), base.Add(10*time.Second), base.Add(20*time.Second)
	// 按 id 的顺序插入：晚提交的（T2）最先，然后是 T1/Q2，最后是 T1/Q1
	huge := func(j *jobRow) { j.spec.CPU = 64 }
	bulk := func(n int, submitted, queued time.Time) {
		first := h.addJob(run, submittedAt(submitted), huge)
		h.exec(`UPDATE jobs SET queued_at=$2 WHERE id=$1`, first, queued)
		h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest,
			queued_at, created_at)
			SELECT kind, run_id, job_key || '-' || g, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest,
			queued_at, created_at FROM jobs, generate_series(1, $2) g WHERE id=$1 ORDER BY g`, first, n-1)
	}
	bulk(9, t2, q0)
	small := h.addJob(run, submittedAt(t2))
	h.exec(`UPDATE jobs SET queued_at=$2 WHERE id=$1`, small, q0)
	bulk(450, t1, q2)
	bulk(CandidateLimit, t1, q1)

	// 期望的顺序：按 (priority DESC, 提交时间, queued_at, id) 排序全部 pending job
	type key struct {
		ID                int64
		Submitted, Queued time.Time
	}
	rows, err := h.db.Query(h.ctx, `SELECT id, created_at, queued_at FROM jobs WHERE state='pending'`)
	if err != nil {
		t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[key])
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(all, func(a, b key) int {
		return cmp.Or(a.Submitted.Compare(b.Submitted), a.Queued.Compare(b.Queued), cmp.Compare(a.ID, b.ID))
	})
	want := make([]int64, 0, len(all))
	for _, k := range all {
		want = append(want, k.ID)
	}
	acc := runner.Runner.accepts()

	// 1. 逐页取候选（与 Reserve 同一个查询）：拼起来与期望的顺序相同，页的边界落在设计的位置
	var got []int64
	var cur *Cursor
	var bounds [][2]key
	for page := 0; ; page++ {
		if page > len(want)/CandidateLimit+1 {
			t.Fatalf("翻了 %d 页仍没有结束（游标与排序不一致时会重复取同一批）", page)
		}
		var cands []*cistore.Job
		if err := h.db.Tx(h.ctx, func(tx pgx.Tx) (err error) {
			cands, err = candidates(h.ctx, tx, acc, cur)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if page > 0 && len(cands) > 0 {
			bounds = append(bounds, [2]key{{cur.ID, cur.Submitted, cur.QueuedAt}, {cands[0].ID, cands[0].CreatedAt, *cands[0].QueuedAt}})
		}
		for _, c := range cands {
			got = append(got, c.ID)
		}
		if cur = nextCursor(cands, CandidateLimit); cur == nil {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("翻页拼起来的候选（%d 个）与排队顺序（%d 个）不同", len(got), len(want))
	}
	if len(bounds) != 2 {
		t.Fatalf("应有 2 个页的边界：%v", bounds)
	}
	if b := bounds[0]; !b[0].Submitted.Equal(b[1].Submitted) || b[0].Queued.Equal(b[1].Queued) {
		t.Errorf("第 1 个边界应是提交时间相同、queued_at 不同：%v", b)
	}
	if b := bounds[1]; !b[0].Submitted.Equal(b[1].Submitted) || !b[0].Queued.Equal(b[1].Queued) {
		t.Errorf("第 2 个边界应是提交时间、queued_at 都相同：%v", b)
	}

	// 2. Native 领取：前两页都放不下，翻到第三页领到唯一放得下的 job
	tr := *runner
	var claimed []int64
	cur = nil
	for pages := range 5 {
		tr.RequestID = fmt.Sprintf("req-page-%d", pages)
		res, err := Reserve(h.ctx, h.r, Trigger{Native: &tr}, cur, noPrepare)
		if err != nil {
			t.Fatal(err)
		}
		if claimed = claimedIDs(res); len(claimed) > 0 {
			if pages != 2 {
				t.Errorf("应在第 3 页领到，实际第 %d 页", pages+1)
			}
			break
		}
		if cur = res.Next; cur == nil {
			break
		}
	}
	if !slices.Equal(claimed, []int64{small}) {
		t.Fatalf("翻页后应领到 job %d：%v", small, claimed)
	}
}

// TestCandidateIndex：候选查询（第一页和按游标翻页）用与排队顺序一致的部分索引 jobs_pending：第一页按索引顺序读取、不另外
// 排序；翻页时游标的行比较作为索引条件。
func TestCandidateIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.addNative()
	run := h.addRun(h.addRepo(0))
	first := h.addJob(run)
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at)
		SELECT kind, run_id, job_key || '-' || g, job_name, display_name, spec, platform, executor_kind,
		CASE WHEN g % 50 = 0 THEN 'pending' ELSE 'succeeded' END, priority, input_digest, queued_at FROM jobs, generate_series(1, 5000) g WHERE id=$1`, first)
	h.exec(`ANALYZE jobs`)
	j, err := cistore.GetJob(h.ctx, h.db, first, false)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := j.QueueKey()
	acc := runner.Runner.accepts()
	explain := func(after *Cursor) string {
		t.Helper()
		args := append(acc.filterArgs(), pageArgs(after, CandidateLimit)...)
		rows, err := h.db.Query(h.ctx, `EXPLAIN SELECT id FROM jobs WHERE `+candidateFilter+candPageSQL(len(acc.filterArgs())+1), args...)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(plan, "\n")
	}
	if p := explain(nil); !strings.Contains(p, "Index Scan using jobs_pending") || strings.Contains(p, "Sort") {
		t.Errorf("第一页应按 jobs_pending 的顺序读取、不另外排序：\n%s", p)
	} else {
		t.Logf("第一页的计划：\n%s", p)
	}
	if p := explain(&k); !strings.Contains(p, "jobs_pending") || !strings.Contains(p, "ROW(created_at, queued_at, id) >") || strings.Contains(p, "Seq Scan") {
		t.Errorf("翻页应用 jobs_pending，游标的行比较作为索引条件：\n%s", p)
	} else {
		t.Logf("翻页的计划：\n%s", p)
	}
}

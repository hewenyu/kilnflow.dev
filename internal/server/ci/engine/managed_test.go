package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 托管执行的状态转移和本地判定（PLAN #32.6、#32.10）。预留与 scheduler.Reserver 写入的相同：job 置为 running、epoch+1、
// 租约为预留期限、started_at 为空，attempt 属于托管池、phase 为 reserved。

// testPool 是测试用的托管池 Key。
const testPool = "gcp-a"

// reserve 模拟把 pending 的 job 预留到托管池，返回 attempt ID。
func (e *env) reserve(jobID int64) int64 {
	e.t.Helper()
	if s := e.job(jobID).State; s != store.JobPending {
		e.t.Fatalf("job %d 的状态为 %s，只能预留 pending 的 job", jobID, s)
	}
	var aid int64
	e.tx(func(tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(e.ctx, `UPDATE jobs SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
			lease_expires_at=now()+make_interval(secs => $2), started_at=NULL, started_step=false WHERE id=$1 RETURNING epoch`,
			jobID, testLeaseTTL.Seconds()).Scan(&epoch); err != nil {
			return err
		}
		if err := tx.QueryRow(e.ctx, `INSERT INTO attempts (job_id, epoch, managed_pool, phase, acquire_request_id)
			VALUES ($1,$2,$3,'reserved','') RETURNING id`, jobID, epoch, testPool).Scan(&aid); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `UPDATE jobs SET current_attempt_id=$2 WHERE id=$1`, jobID, aid)
		return err
	})
	return aid
}

// reservedJob 建一个 run 和一个 job，推进并预留到托管池，返回 run 和 job 的 ID。
func (e *env) reservedJob(opts jobOpts) (int64, int64) {
	e.t.Helper()
	run := e.newRun(runOpts{})
	id := e.addJob(run, opts)
	e.advance(run)
	e.reserve(id)
	return run, id
}

// submit 模拟派发者把托管 attempt 推进到 phase（越过 reserved 时与 #138 相同：清空租约）；startedAgo 不为 0 时
// 同时写 started_at（已经开始执行了这么久）。
func (e *env) submit(jobID int64, phase cistore.Phase, startedAgo time.Duration) {
	e.t.Helper()
	e.exec(`UPDATE attempts SET phase=$2 WHERE id=(SELECT current_attempt_id FROM jobs WHERE id=$1)`, jobID, phase)
	e.exec(`UPDATE jobs SET lease_expires_at=NULL, started_at=CASE WHEN $2 > 0 THEN now() - make_interval(secs => $2) END WHERE id=$1`,
		jobID, startedAgo.Seconds())
}

// currentAttempt 返回 job 当前 attempt 的阶段和结果。
func (e *env) currentAttempt(jobID int64) (cistore.Phase, *string, *string) {
	e.t.Helper()
	var phase cistore.Phase
	var outcome, errClass *string
	if err := e.db.QueryRow(e.ctx, `SELECT a.phase, a.outcome, a.error_class FROM attempts a JOIN jobs j ON j.current_attempt_id=a.id WHERE j.id=$1`,
		jobID).Scan(&phase, &outcome, &errClass); err != nil {
		e.t.Fatal(err)
	}
	return phase, outcome, errClass
}

// TestLocalJudgmentOnlyLocallyLeased：Server 的本地判定（启动顺延租约、失联、超时）只作用于本地判活的 attempt（Native 的
// running、托管的 reserved）；已提交的托管执行（submitting、queued、托管 running、unknown）即使过了超时时间也不被结束，
// 继续占用仓库配额（I11）。
func TestLocalJudgmentOnlyLocallyLeased(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, native := e.runningJob(jobOpts{name: "native"}, "")
	_, reserved := e.reservedJob(jobOpts{name: "reserved"})
	submitted := map[cistore.Phase]int64{}
	for _, ph := range []cistore.Phase{cistore.PhaseSubmitting, cistore.PhaseQueued, cistore.PhaseRunning, cistore.PhaseUnknown} {
		_, id := e.reservedJob(jobOpts{name: string(ph)})
		var ago time.Duration
		if ph == cistore.PhaseRunning || ph == cistore.PhaseUnknown {
			ago = 48 * time.Hour // 远超超时时间
		}
		e.submit(id, ph, ago)
		submitted[ph] = id
	}

	// 单独的 Engine 记下日志：已提交的托管 attempt 在第一层（SQL 条件）就不应被选中，加锁后的复查（第二层）只在数据
	// 违反 I11 时才记错误日志
	var logs bytes.Buffer
	eng := New(e.eng.cfg, Deps{DB: e.db, CAS: e.cas, Outbox: e.eng.outbox, Hub: e.hub, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err := eng.ExtendLeasesOnStartup(e.ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{native, reserved} {
		if e.job(id).LeaseExpiresAt == nil {
			t.Fatalf("job %d：本地判活的租约应被顺延", id)
		}
	}
	for ph, id := range submitted {
		if l := e.job(id).LeaseExpiresAt; l != nil {
			t.Fatalf("%s 的托管 job 没有租约，启动时不能凭空生成：%v", ph, l)
		}
	}

	// 超时宽限置零、本地判活的租约改到过去
	eng.timeoutsFrom.Store(0)
	e.exec(`UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id = ANY($1)`, []int64{native, reserved})
	eng.SweepOnce(e.ctx)
	if strings.Contains(logs.String(), "托管 attempt 不应") {
		t.Fatalf("已提交的托管 attempt 不应被租约或超时的扫描选中：\n%s", logs.String())
	}

	for ph, id := range submitted {
		e.wantJobState(id, store.JobRunning)
		if got, outcome, _ := e.currentAttempt(id); got != ph || outcome != nil {
			t.Fatalf("%s 的托管 attempt 被结束：phase=%s outcome=%v", ph, got, deref(outcome))
		}
		if n := len(e.attempts(id)); n != 1 {
			t.Fatalf("%s 的托管 job 有 %d 个 attempt，不应重排", ph, n)
		}
	}
	if n := e.queryInt(`SELECT count(*) FROM jobs j JOIN runs r ON r.id=j.run_id WHERE r.repo_id=$1 AND j.state='running'`, testRepoID); n != 4 {
		t.Fatalf("仓库中 running 的 job = %d，已提交的四个托管 job 应继续占用配额", n)
	}
	e.wantJobState(native, store.JobPending)
	if a := e.attempts(native); deref(a[0].ErrClass) != ErrClassLost {
		t.Fatalf("Native 租约过期应按失联结束：%+v", a[0])
	}
	e.wantJobState(reserved, store.JobPending)
	if a := e.attempts(reserved); deref(a[0].ErrClass) != ErrClassReleased {
		t.Fatalf("预留到期应撤销：%+v", a[0])
	}

	// 违反 I11 的数据：已提交的托管 attempt 带着过期的租约，加锁后复查仍不结束
	queued := submitted[cistore.PhaseQueued]
	e.exec(`UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, queued)
	eng.SweepOnce(e.ctx)
	e.wantJobState(queued, store.JobRunning)
	if !strings.Contains(logs.String(), "托管 attempt 不应有 Server 本地的租约") {
		t.Fatalf("违反 I11 的数据应记错误日志：\n%s", logs.String())
	}
	if _, outcome, _ := e.currentAttempt(queued); outcome != nil {
		t.Fatalf("手工写入租约的 queued 被结束：%s", *outcome)
	}
}

// TestReservedNotTimedOut：预留还没有开始执行（started_at 为空），过了超时时间也不判超时（I4）。
func TestReservedNotTimedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, id := e.reservedJob(jobOpts{})
	e.exec(`UPDATE attempts SET started_at=now()-interval '2 days' WHERE job_id=$1`, id)
	e.eng.timeoutsFrom.Store(0)
	e.eng.SweepOnce(e.ctx)
	e.wantJobState(id, store.JobRunning)
	if ph, outcome, _ := e.currentAttempt(id); ph != cistore.PhaseReserved || outcome != nil {
		t.Fatalf("预留被结束：%s %v", ph, outcome)
	}
}

// TestMarkStarted：托管执行开始时写 started_at、清空租约（之后只由 Provider 确认结束）；重复或过期的推进返回 ErrStale。
func TestMarkStarted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run, id := e.reservedJob(jobOpts{})
	j := e.job(id)
	e.tx(func(tx pgx.Tx) error { return e.eng.MarkStarted(e.ctx, tx, id, *j.CurrentAttemptID, j.Epoch) })
	j = e.wantJobState(id, store.JobRunning)
	if j.StartedAt == nil || j.LeaseExpiresAt != nil || !strings.Contains(deref(j.Reason), testPool) {
		t.Fatalf("开始执行后 started_at=%v lease=%v reason=%q", j.StartedAt, j.LeaseExpiresAt, deref(j.Reason))
	}
	if ph, _, _ := e.currentAttempt(id); ph != cistore.PhaseRunning {
		t.Fatalf("attempt 的 phase = %s", ph)
	}
	if n := e.queryInt(`SELECT count(*) FROM run_reruns WHERE run_id=$1 AND started_at IS NOT NULL`, run); n != 1 {
		t.Fatal("run_reruns.started_at 应写入")
	}
	e.wantOutbox(jobCheckKey(id))
	for name, epoch := range map[string]int64{"重复": j.Epoch, "过期的 epoch": j.Epoch - 1} {
		err := e.db.Tx(e.ctx, func(tx pgx.Tx) error { return e.eng.MarkStarted(e.ctx, tx, id, *j.CurrentAttemptID, epoch) })
		if !errors.Is(err, ErrStale) {
			t.Fatalf("%s的推进应返回 ErrStale：%v", name, err)
		}
	}
	// Native attempt 不能被推进
	_, native := e.runningJob(jobOpts{name: "native"}, "")
	nj := e.job(native)
	if err := e.db.Tx(e.ctx, func(tx pgx.Tx) error { return e.eng.MarkStarted(e.ctx, tx, native, *nj.CurrentAttemptID, nj.Epoch) }); !errors.Is(err, ErrStale) {
		t.Fatalf("Native attempt 的推进应返回 ErrStale：%v", err)
	}
}

// TestRelease：撤销预留后 job 回到 pending，queued_at 不变、attempt_count 复原（I6）；不是 reserved 时返回 ErrStale。
func TestRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, id := e.reservedJob(jobOpts{})
	e.exec(`UPDATE jobs SET queued_at=now()-interval '1 hour' WHERE id=$1`, id)
	before := e.job(id)
	release := func(id int64) error {
		return e.db.Tx(e.ctx, func(tx pgx.Tx) error {
			j, err := cistore.GetJob(e.ctx, tx, id, true)
			if err != nil {
				return err
			}
			return e.eng.Release(e.ctx, tx, j, "托管池已停用")
		})
	}
	if err := release(id); err != nil {
		t.Fatal(err)
	}
	j := e.wantJobState(id, store.JobPending)
	if !j.QueuedAt.Equal(*before.QueuedAt) || j.AttemptCount != 0 || j.LeaseExpiresAt != nil {
		t.Fatalf("撤销后 queued_at=%v（原 %v）attempt_count=%d lease=%v", j.QueuedAt, before.QueuedAt, j.AttemptCount, j.LeaseExpiresAt)
	}
	if !strings.Contains(deref(j.Reason), "托管池已停用") {
		t.Fatalf("原因 = %q", deref(j.Reason))
	}
	if a := e.attempts(id); len(a) != 1 || deref(a[0].Outcome) != string(store.OutcomeInfraError) || deref(a[0].ErrClass) != ErrClassReleased {
		t.Fatalf("attempt = %+v", a)
	}
	e.wantOutbox(jobCheckKey(id))
	if err := release(id); !errors.Is(err, ErrStale) {
		t.Fatalf("已撤销的预留再次撤销应返回 ErrStale：%v", err)
	}
	// 已提交的托管 attempt 不能撤销（可能已在外部执行，I5）
	_, sub := e.reservedJob(jobOpts{name: "submitted"})
	e.submit(sub, cistore.PhaseSubmitting, 0)
	if err := release(sub); !errors.Is(err, ErrStale) {
		t.Fatalf("submitting 的 attempt 撤销应返回 ErrStale：%v", err)
	}
	e.wantJobState(sub, store.JobRunning)
}

// TestFinishManagedAttempt：结果未知的托管执行遇到基础设施错误不重排（I5）；有副作用的托管 job 越过 reserved 后不自动重试；
// 外部已接受、由 Provider 报告结束的基础设施错误按现有规则重排。
func TestFinishManagedAttempt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	infra := AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.provider", Message: "超时"}
	tests := []struct {
		name    string
		phase   cistore.Phase // 空表示保持 reserved
		effects bool
		want    store.JobState
		inMsg   string
		phaseAt cistore.Phase // 结束后当前 attempt 的 phase
	}{
		{"提交中", cistore.PhaseSubmitting, false, store.JobRunning, "等待对账", cistore.PhaseUnknown},
		{"结果未知", cistore.PhaseUnknown, false, store.JobRunning, "不会自动重新提交", cistore.PhaseUnknown},
		{"有副作用、提交中也不重排", cistore.PhaseSubmitting, true, store.JobRunning, "等待对账", cistore.PhaseUnknown},
		{"外部排队中报告失败可以重排", cistore.PhaseQueued, false, store.JobPending, "重新排队", cistore.PhaseQueued},
		{"有副作用、已在外部排队", cistore.PhaseQueued, true, store.JobFailed, "不自动重试", cistore.PhaseQueued},
		{"有副作用、执行中", cistore.PhaseRunning, true, store.JobFailed, "不自动重试", cistore.PhaseRunning},
		{"有副作用、只预留过", "", true, store.JobPending, "重新排队", cistore.PhaseReserved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := e.sub(t)
			_, id := e.reservedJob(jobOpts{name: "j", effects: tt.effects})
			if tt.phase != "" {
				e.submit(id, tt.phase, 0)
			}
			e.finish(id, infra)
			j := e.wantJobState(id, tt.want)
			if !strings.Contains(deref(j.Reason), tt.inMsg) {
				t.Fatalf("原因 %q，期望包含 %q", deref(j.Reason), tt.inMsg)
			}
			ph, outcome, _ := e.currentAttempt(id)
			if ph != tt.phaseAt {
				t.Fatalf("attempt 的 phase = %s，期望 %s", ph, tt.phaseAt)
			}
			if (outcome == nil) != (tt.want == store.JobRunning) {
				t.Fatalf("attempt 的 outcome = %v", deref(outcome))
			}
			if n := len(e.attempts(id)); n != 1 {
				t.Fatalf("有 %d 个 attempt", n)
			}
		})
	}
}

// TestCancelManaged：取消时 reserved 的 job 直接 cancelled（没有外部调用，不等确认）；已提交的置 cancel_requested，
// 等 Provider 确认；fail-fast 走同一个函数。
func TestCancelManaged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	reserved := e.addJob(run, jobOpts{name: "reserved"})
	queued := e.addJob(run, jobOpts{name: "queued"})
	e.advance(run)
	e.reserve(reserved)
	e.reserve(queued)
	e.submit(queued, cistore.PhaseQueued, 0)
	e.tx(func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, run, store.CancelByUser) })
	j := e.wantJobState(reserved, store.JobCancelled)
	if j.FinishedAt == nil || j.LeaseExpiresAt != nil {
		t.Fatalf("取消的预留 finished_at=%v lease=%v", j.FinishedAt, j.LeaseExpiresAt)
	}
	if _, outcome, _ := e.currentAttempt(reserved); deref(outcome) != string(store.OutcomeCancelled) {
		t.Fatalf("取消的预留的 attempt outcome = %v", deref(outcome))
	}
	q := e.wantJobState(queued, store.JobRunning)
	if !q.CancelRequested || !strings.Contains(deref(q.Reason), "等待托管执行确认结束") {
		t.Fatalf("已提交的托管 job：cancel_requested=%v reason=%q", q.CancelRequested, deref(q.Reason))
	}

	// fail-fast：矩阵中一个变体失败，预留中的另一个变体立即取消
	run2 := e.newRun(runOpts{})
	a := e.addJob(run2, jobOpts{key: "m-a", name: "m", matrix: map[string]any{"v": "a"}, spec: workflow.Spec{FailFast: true}})
	b := e.addJob(run2, jobOpts{key: "m-b", name: "m", matrix: map[string]any{"v": "b"}, spec: workflow.Spec{FailFast: true}})
	e.advance(run2)
	e.claim(a)
	e.reserve(b)
	e.finish(a, AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 1})
	if j := e.wantJobState(b, store.JobCancelled); deref(j.CancelReason) != string(store.CancelFailFast) {
		t.Fatalf("fail-fast 取消原因 = %v", deref(j.CancelReason))
	}
}

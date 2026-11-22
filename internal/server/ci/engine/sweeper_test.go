package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 时间相关的条件都用 SQL 直接修改 started_at、lease_expires_at、updated_at、created_at 来构造，不等待。

// runningJob 建一个 run 和一个 job，推进并领取它，返回 run 和 job 的 ID。prepare 是领取后对 job 执行的 SQL（$1 为 job ID）。
func (e *env) runningJob(opts jobOpts, prepare string) (int64, int64) {
	e.t.Helper()
	run := e.newRun(runOpts{})
	id := e.addJob(run, opts)
	e.advance(run)
	e.claim(id)
	if prepare != "" {
		e.exec(prepare, id)
	}
	return run, id
}

// TestRunSweeper：RunSweeper 按间隔执行 sweep，状态变化后唤醒各组件；ctx 取消后返回。
func TestRunSweeper(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, id := e.runningJob(jobOpts{}, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`)
	eng := New(Config{InfraRetryMax: testInfraRetryMax, LeaseTTL: testLeaseTTL, CancelGrace: testCancelGrace, SweepInterval: time.Millisecond},
		Deps{DB: e.db, CAS: e.cas, Outbox: e.eng.outbox, Hub: e.hub, Log: e.eng.log})
	woke := e.hub.JobsChanged()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.RunSweeper(ctx)
	}()
	select {
	case <-woke:
	case <-time.After(30 * time.Second):
		t.Fatal("sweeper 处理过期租约后应唤醒各组件")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ctx 取消后 RunSweeper 应返回")
	}
	e.wantJobState(id, store.JobPending)
}

// TestExtendLeasesOnStartup：Server 启动时为 Runner 持有的租约延期到至少 now() + 2 × lease_ttl，更长的租约不缩短；
// 发布 job 没有 Runner 租约，不处理。
func TestExtendLeasesOnStartup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, short := e.runningJob(jobOpts{}, `UPDATE jobs SET lease_expires_at=now()+interval '1 second' WHERE id=$1`)
	_, long := e.runningJob(jobOpts{}, `UPDATE jobs SET lease_expires_at=now()+interval '1 hour' WHERE id=$1`)
	_, pub := e.runningJob(jobOpts{kind: workflow.JobKindPublish}, `UPDATE jobs SET lease_expires_at=now()+interval '1 second' WHERE id=$1`)
	if err := e.eng.ExtendLeasesOnStartup(e.ctx); err != nil {
		t.Fatal(err)
	}
	remaining := func(id int64) time.Duration {
		t.Helper()
		var secs float64
		if err := e.db.QueryRow(e.ctx, `SELECT extract(epoch FROM lease_expires_at - now()) FROM jobs WHERE id=$1`, id).Scan(&secs); err != nil {
			t.Fatal(err)
		}
		return time.Duration(secs * float64(time.Second))
	}
	if got := remaining(short); got < 2*testLeaseTTL-5*time.Second {
		t.Errorf("短租约应延期到 2 × lease_ttl：剩余 %s", got)
	}
	if got := remaining(long); got < 50*time.Minute {
		t.Errorf("更长的租约不应缩短：剩余 %s", got)
	}
	if got := remaining(pub); got > 5*time.Second {
		t.Errorf("发布 job 不应延期：剩余 %s", got)
	}
}

// TestSweep_ExpiredLeases：租约过期（Runner 失联）的 job 按 PLAN #6.1 处理：重试、已请求取消的取消、
// 已开始执行 step 的 effects job 失败；租约未过期的和发布 job 不处理；读取失败的 job 跳过，不影响其他 job。
func TestSweep_ExpiredLeases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const expire = `UPDATE jobs SET lease_expires_at=now()-interval '1 second'`
	// 第一个：spec 损坏，无法读取，记录日志后继续处理后面的 job
	_, broken := e.runningJob(jobOpts{}, expire+`, spec='[]' WHERE id=$1`)
	_, retry := e.runningJob(jobOpts{}, expire+` WHERE id=$1`)
	_, cancelled := e.runningJob(jobOpts{}, expire+`, cancel_requested=true WHERE id=$1`)
	_, effects := e.runningJob(jobOpts{effects: true}, expire+`, started_step=true WHERE id=$1`)
	_, alive := e.runningJob(jobOpts{}, `UPDATE jobs SET lease_expires_at=now()+interval '1 minute' WHERE id=$1`)
	_, pub := e.runningJob(jobOpts{kind: workflow.JobKindPublish}, expire+` WHERE id=$1`)

	changed, err := e.eng.sweepExpiredLeases(e.ctx, "过期租约")
	if err != nil || !changed {
		t.Fatalf("sweepExpiredLeases = %v, %v，期望有变化", changed, err)
	}
	j := e.wantJobState(retry, store.JobPending)
	if !strings.Contains(deref(j.Reason), "infra.lost：Runner 失联（租约过期）") {
		t.Errorf("重试原因 = %q", deref(j.Reason))
	}
	if as := e.attempts(retry); len(as) != 1 || deref(as[0].Outcome) != string(store.OutcomeInfraError) || deref(as[0].ErrClass) != "infra.lost" {
		t.Errorf("attempt 记录 = %+v", as)
	}
	e.wantJobState(cancelled, store.JobCancelled)
	if j := e.wantJobState(effects, store.JobFailed); !strings.Contains(deref(j.Reason), "不自动重试") {
		t.Errorf("effects job 的原因 = %q", deref(j.Reason))
	}
	e.exec(`UPDATE jobs SET spec='{}' WHERE id=$1`, broken)
	for _, id := range []int64{broken, alive, pub} {
		e.wantJobState(id, store.JobRunning)
	}

	// 没有过期的租约时没有变化
	e.exec(`UPDATE jobs SET lease_expires_at=now()+interval '1 minute' WHERE state='running'`)
	if changed, err := e.eng.sweepExpiredLeases(e.ctx, "过期租约"); err != nil || changed {
		t.Fatalf("没有过期租约时 sweepExpiredLeases = %v, %v", changed, err)
	}
}

// TestSweep_Timeouts：started_at + timeout + cancel_grace + lease_ttl 之后仍在执行的 job 判为 timed_out；
// spec 中没有 timeout_minutes（或为 0）时按默认的 60 分钟；还在宽限期内的、发布 job 不处理。
func TestSweep_Timeouts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	grace := testCancelGrace + testLeaseTTL
	set := func(id int64, ago time.Duration) {
		t.Helper()
		e.exec(`UPDATE jobs SET started_at=now()-make_interval(secs => $2) WHERE id=$1`, id, ago.Seconds())
	}
	_, defaultTimeout := e.runningJob(jobOpts{}, `UPDATE jobs SET spec=spec-'timeout_minutes' WHERE id=$1`)
	set(defaultTimeout, time.Duration(workflow.DefaultTimeoutMinutes)*time.Minute+grace+time.Second)
	// timeout_minutes 为 0：Runner 按默认超时执行（buildJobSpec），Server 也要按默认超时判断，不能宽限期一过就判超时
	_, zeroTimeout := e.runningJob(jobOpts{}, `UPDATE jobs SET spec=jsonb_set(spec, '{timeout_minutes}', '0') WHERE id=$1`)
	set(zeroTimeout, 10*time.Minute+grace)
	_, inGrace := e.runningJob(jobOpts{}, "")
	set(inGrace, time.Duration(workflow.DefaultTimeoutMinutes)*time.Minute+grace-10*time.Second)
	_, custom := e.runningJob(jobOpts{spec: workflow.Spec{TimeoutMinutes: 5}}, "")
	set(custom, 5*time.Minute+grace+time.Second)
	_, pub := e.runningJob(jobOpts{kind: workflow.JobKindPublish}, "")
	set(pub, 24*time.Hour)

	changed, err := e.eng.sweepTimeouts(e.ctx, "超时")
	if err != nil || !changed {
		t.Fatalf("sweepTimeouts = %v, %v，期望有变化", changed, err)
	}
	for _, id := range []int64{defaultTimeout, custom} {
		e.wantJobState(id, store.JobTimedOut)
		if as := e.attempts(id); len(as) != 1 || deref(as[0].Outcome) != string(store.OutcomeTimedOut) || deref(as[0].Message) != "超时（Server 判定）" {
			t.Errorf("job %d 的 attempt 记录 = %+v", id, as)
		}
	}
	e.wantJobState(inGrace, store.JobRunning)
	e.wantJobState(zeroTimeout, store.JobRunning)
	e.wantJobState(pub, store.JobRunning)
}

// Server 停机期间按时完成的 job：重启后的 2 × lease_ttl 内 Runner 会重新提交结果，sweeper 不能在第一轮就按
// started_at 判为超时（超时 10 分钟的 job 在第 9 分钟完成，Server 第 8 到第 13 分钟停机）。
func TestSweep_TimeoutRightAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, id := e.runningJob(jobOpts{spec: workflow.Spec{TimeoutMinutes: 10}}, `UPDATE jobs SET started_at=now()-interval '13 minutes' WHERE id=$1`)
	if err := e.eng.ExtendLeasesOnStartup(e.ctx); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.eng.sweepTimeouts(e.ctx, "超时"); err != nil || changed {
		t.Fatalf("重启后的宽限期内 sweepTimeouts = %v, %v，不应判定超时", changed, err)
	}
	e.wantJobState(id, store.JobRunning)

	// 宽限期过后照常判定
	e.eng.timeoutsFrom.Store(time.Now().Add(-time.Second).UnixNano())
	if changed, err := e.eng.sweepTimeouts(e.ctx, "超时"); err != nil || !changed {
		t.Fatalf("宽限期过后 sweepTimeouts = %v, %v，应判定超时", changed, err)
	}
	e.wantJobState(id, store.JobTimedOut)
}

// 扫描选中的超时 attempt 在加锁前失败并被重新领取时，不能结束新 attempt。
func TestSweep_TimeoutsRechecksRetriedAttempt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run, id := e.runningJob(jobOpts{spec: workflow.Spec{TimeoutMinutes: 1}},
		`UPDATE jobs SET started_at=now()-interval '1 hour' WHERE id=$1`)
	ids, err := store.QueryIDs(e.ctx, e.db, `SELECT id FROM jobs WHERE `+timedOutJobs,
		int64((testCancelGrace+testLeaseTTL)/time.Second), workflow.DefaultTimeoutMinutes)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("超时候选 = %v, %v，期望 job %d", ids, err, id)
	}
	// 在候选扫描与加锁处理之间，旧 attempt 已失败，另一个 Runner 领取了重试。
	e.finish(id, AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost"})
	aid := e.claim(id)
	e.tx(func(tx pgx.Tx) error {
		ended, err := e.eng.timeoutJob(e.ctx, tx, ids[0])
		if ended != 0 {
			t.Errorf("没有结束任何 attempt，却返回 %d", ended)
		}
		return err
	})
	j := e.wantJobState(id, store.JobRunning)
	if j.CurrentAttemptID == nil || *j.CurrentAttemptID != aid {
		t.Fatalf("current attempt = %v，期望 %d", j.CurrentAttemptID, aid)
	}
	e.wantRunState(run, store.RunActive)
	if as := e.attempts(id); len(as) != 2 || deref(as[0].Outcome) != string(store.OutcomeInfraError) || as[1].Outcome != nil {
		t.Fatalf("不应修改新 attempt 的结果：%+v", as)
	}
}

// TestSweep_StuckRuns：兜底推进 run：所有 job 已结束但 run 仍未结束的、上游已全部结束的 waiting job、
// 被跟随的 job 已结束的跟随者。正常执行中的 run 不处理。
func TestSweep_StuckRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// 1. 所有 job 已结束
	allDone := e.newRun(runOpts{})
	e.addJob(allDone, jobOpts{state: store.JobSucceeded})
	// 2. 上游已成功，下游还在 waiting
	upDone := e.newRun(runOpts{})
	up := e.addJob(upDone, jobOpts{name: "up", state: store.JobSucceeded})
	down := e.addJob(upDone, jobOpts{name: "down", needs: []int64{up}})
	// 3. 被跟随的 job 已失败，跟随者还在 waiting
	leaderRun := e.newRun(runOpts{})
	leader := e.addJob(leaderRun, jobOpts{state: store.JobFailed})
	followerRun := e.newRun(runOpts{})
	follower := e.addJob(followerRun, jobOpts{})
	e.exec(`UPDATE jobs SET follows_job_id=$2 WHERE id=$1`, follower, leader)
	// 4. 正常执行中
	busy, busyJob := e.runningJob(jobOpts{}, "")

	changed, err := e.eng.sweepStuckRuns(e.ctx, "兜底推进 run")
	if err != nil || !changed {
		t.Fatalf("sweepStuckRuns = %v, %v，期望有变化", changed, err)
	}
	e.wantRunState(allDone, store.RunSucceeded)
	e.wantJobState(down, store.JobPending)
	if j := e.wantJobState(follower, store.JobPending); j.Follows != nil {
		t.Errorf("跟随者应清空 follows_job_id：%d", *j.Follows)
	}
	e.wantRunState(leaderRun, store.RunFailed)
	e.wantRunState(busy, store.RunActive)
	e.wantJobState(busyJob, store.JobRunning)
}

// TestSweep_FollowerOfCancelledLeader：被跟随的 job 所在的 run 被取消后，其他 run 中的跟随者最迟由 sweeper 重新做就绪判定，
// 不继承取消。
func TestSweep_FollowerOfCancelledLeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cached := jobOpts{spec: workflow.Spec{Cache: true}}
	leaderRun := e.newRun(runOpts{})
	leader := e.addJob(leaderRun, cached)
	e.advance(leaderRun)
	followerRun := e.newRun(runOpts{})
	follower := e.addJob(followerRun, cached)
	e.advance(followerRun)
	if j := e.wantJobState(follower, store.JobWaiting); j.Follows == nil || *j.Follows != leader {
		t.Fatalf("应跟随 job %d：%v", leader, j.Follows)
	}
	e.tx(func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, leaderRun, store.CancelByUser) })
	e.wantRunState(leaderRun, store.RunCancelled)

	if _, err := e.eng.sweepStuckRuns(e.ctx, "兜底推进 run"); err != nil {
		t.Fatal(err)
	}
	if j := e.wantJobState(follower, store.JobPending); j.Follows != nil {
		t.Errorf("跟随者应清空 follows_job_id：%d", *j.Follows)
	}
	e.wantRunState(followerRun, store.RunActive)
}

// TestSweep_ConcurrencyGroups：组内已经没有 active 的 run 时，激活排队超过宽限时间的、最早的 run；
// 刚排队的（可能在等正常唤醒）和组内还有 active 的不处理。
func TestSweep_ConcurrencyGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	queued := func(group string, age time.Duration) (int64, int64) {
		t.Helper()
		run := e.newRun(runOpts{state: store.RunQueued, concurrency: group})
		id := e.addJob(run, jobOpts{})
		e.exec(`UPDATE runs SET updated_at=now()-make_interval(secs => $2) WHERE id=$1`, run, age.Seconds())
		return run, id
	}
	old := time.Minute
	stuck, stuckJob := queued("stuck", old)
	second, _ := queued("stuck", old) // 同一组只激活最早的一个
	fresh, _ := queued("fresh", time.Second)
	busyActive := e.newRun(runOpts{concurrency: "busy"})
	e.addJob(busyActive, jobOpts{state: store.JobRunning})
	busyQueued, _ := queued("busy", old)

	changed, err := e.eng.sweepConcurrencyGroups(e.ctx, "并发组兜底")
	if err != nil || !changed {
		t.Fatalf("sweepConcurrencyGroups = %v, %v，期望有变化", changed, err)
	}
	e.wantRunState(stuck, store.RunActive)
	e.wantJobState(stuckJob, store.JobPending)
	for _, id := range []int64{second, fresh, busyQueued} {
		e.wantRunState(id, store.RunQueued)
	}
}

// TestSweep_Nonces：清理超过保存时间的 Runner 认证随机数，未过期的保留。
func TestSweep_Nonces(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.exec(`INSERT INTO auth_nonces (runner_id, nonce, created_at) VALUES ($1, 'old', now()-interval '1 hour'), ($1, 'new', now())`, e.runnerID)
	if changed, err := e.eng.sweepNonces(e.ctx, "清理随机数"); err != nil || changed {
		t.Fatalf("sweepNonces = %v, %v（清理随机数不算状态变化）", changed, err)
	}
	rows, err := e.db.Query(e.ctx, `SELECT nonce FROM auth_nonces ORDER BY nonce`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		left = append(left, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(left, ",") != "new" {
		t.Fatalf("剩余的随机数 = %v，期望只有 new", left)
	}
}

// TestSweep_Notify：sweep 依次执行全部阶段，有状态变化时唤醒各组件，没有变化时不唤醒。
func TestSweep_Notify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	woke := e.hub.JobsChanged()
	e.eng.sweep(e.ctx)
	select {
	case <-woke:
		t.Fatal("没有状态变化时不应唤醒")
	default:
	}

	_, id := e.runningJob(jobOpts{}, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`)
	e.eng.sweep(e.ctx)
	select {
	case <-woke:
	default:
		t.Fatal("有状态变化时应唤醒各组件")
	}
	e.wantJobState(id, store.JobPending)

	// ctx 已取消时不执行任何阶段
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.exec(`UPDATE jobs SET state='running', lease_expires_at=now()-interval '1 second' WHERE id=$1`, id)
	e.eng.sweep(ctx)
	e.wantJobState(id, store.JobRunning)
}

// 加锁后复查发现没有可处理的对象（扫描之后租约已续期）时不能算作"改变了状态"：否则 sweeper 每轮都唤醒全部循环。
func TestEndEachInTxOnlyReportsRealChange(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(runOpts{})
	job := e.addJob(run, jobOpts{})
	e.advance(run)
	aid := e.claim(job)
	// f 报告没有改变：不能被当成改变（旧实现只要事务成功就算改变）。
	changed := e.eng.endEachInTx(e.ctx, "测试", []int64{job}, func(pgx.Tx, int64) (int64, bool, error) { return 0, false, nil })
	if changed {
		t.Fatal("f 报告没有改变时不应报告改变")
	}
	// 真正结束 attempt 时：报告改变，并且 attempt 被结束。
	changed = e.eng.endEachInTx(e.ctx, "测试", []int64{job}, func(tx pgx.Tx, id int64) (int64, bool, error) {
		runs, err := cistore.LockRunsOfJob(e.ctx, tx, id)
		if err != nil {
			return 0, false, err
		}
		j, err := cistore.GetJob(e.ctx, tx, id, true)
		if err != nil {
			return 0, false, err
		}
		ended, err := e.eng.finishNative(e.ctx, tx, j, AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: ErrClassLost, Message: "测试"}, runs)
		return ended, true, err
	})
	if !changed {
		t.Fatal("结束 attempt 应报告改变")
	}
	if got := e.queryInt(`SELECT count(*) FROM attempts WHERE id=$1 AND outcome IS NOT NULL`, aid); got != 1 {
		t.Fatal("attempt 应被结束")
	}
}

package engine

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const (
	// sweepBatch 是 sweeper 处理 job 的阶段（过期租约、超时）每轮最多处理的对象数。
	sweepBatch = 100
	// sweepRunBatch 是处理 run 的阶段（兜底推进 run、并发组兜底）每轮最多处理的 run 数。
	// 推进一个 run 要锁住并处理它的全部 job，比处理单个 job 重得多，所以是 sweepBatch 的一半。
	sweepRunBatch = sweepBatch / 2
	// concurrencyGroupGrace 是并发组兜底前 run 至少排队的时间：刚排队的 run 可能正在等待上一个 run 结束时的正常唤醒。
	concurrencyGroupGrace = 5 * time.Second
)

// sweeper 结束 attempt 时记录的错误分类（attempts.error_class）。超时与 Runner 自己判定超时时上报的分类相同；
// ErrClassLost 只由 sweeper 写入（Runner 失联），这样的 attempt 已被取代，Runner API 不再接受它的日志。
const (
	ErrClassLost    = "infra.lost"
	errClassTimeout = "timeout"
)

// locallyLeasedJobs 是 SQL 条件：由 Server 在本地判断是否还活着的 job（PLAN #32.6）——Native Runner 执行中的（领取时签发、
// 心跳续期的租约），和托管池中还没有提交的预留（预留期限）。可能已在外部执行的托管 attempt（submitting、queued、托管
// running、unknown）没有租约，不在其中：Server 没有收到消息不等于远端已经停止，只能由 Provider 确认远端结束（I11）。
// 发布 job 由 Server 执行，它的租约标记的是发布 worker，也不在其中。
// 启动顺延租约、过期租约（失联）、超时三处判定都只用这一个条件。状态必须写成字面量，不能改成参数：规划器要据此选用
// 部分索引 jobs_leased（WHERE state = 'running'）。
const locallyLeasedJobs = `state='running' AND kind<>'publish' AND lease_expires_at IS NOT NULL`

// timedOutJobs 在候选扫描和加锁处理时使用同一超时条件；$1 为宽限时间（秒），$2 为默认超时（分钟）。
// timeout_minutes 没有或不是正数时按默认超时，与下发给 Runner 的超时一致（runnerapi 的 buildJobSpec）。
// 按 started_at 判定：还没有开始执行的预留（started_at 为空）不会超时。
const timedOutJobs = locallyLeasedJobs + `
	AND started_at + make_interval(secs => (CASE WHEN (spec->>'timeout_minutes')::int > 0
		THEN (spec->>'timeout_minutes')::int ELSE $2 END * 60 + $1)) < now()`

// RunSweeper 每隔 SweepInterval（默认 5 秒）处理过期租约、超时，并兜底推进 run（PLAN #6.2、#6.3）。
func (e *Engine) RunSweeper(ctx context.Context) {
	t := time.NewTicker(e.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.sweep(ctx)
	}
}

// ExtendLeasesOnStartup 在 Server 启动时为所有活动租约延期，避免停机期间的 job 被判定为失联（PLAN #6.5）。只顺延非空的租约
// （locallyLeasedJobs）：已提交的托管 attempt 没有租约，不能凭空生成一个（PLAN #32.6）；托管的预留因此有 2 × lease_ttl 的
// 时间由派发者接手。
// 超时判定同样推迟到启动后 2 × lease_ttl：停机期间按时完成的 job 要等 Runner 重新提交结果，
// 不能因为超时从 started_at 算起就在第一轮被判为超时。
func (e *Engine) ExtendLeasesOnStartup(ctx context.Context) error {
	window := 2 * e.cfg.LeaseTTL
	// 这个截止时间只表示 Server 启动后的本地保护窗口，与数据库的时间列无关，两端都必须用 Server 本地时钟比较。
	e.timeoutsFrom.Store(time.Now().Add(window).UnixNano())
	_, err := e.db.Exec(ctx, `UPDATE jobs SET lease_expires_at = greatest(lease_expires_at, now() + make_interval(secs => $1))
		WHERE `+locallyLeasedJobs, window.Seconds())
	return err
}

// SweepOnce 立即执行一轮 sweeper（模块测试用：不等 SweepInterval）。
func (e *Engine) SweepOnce(ctx context.Context) { e.sweep(ctx) }

// sweepStage 是 sweeper 的一个阶段：run 返回是否改变了状态，stage 是阶段名（用于日志）。
type sweepStage struct {
	name string
	run  func(ctx context.Context, stage string) (bool, error)
}

// sweep 依次执行各阶段；某个阶段出错时记录日志并继续执行后面的阶段。
func (e *Engine) sweep(ctx context.Context) {
	stages := []sweepStage{
		{name: "过期租约", run: e.sweepExpiredLeases},
		{name: "超时", run: e.sweepTimeouts},
		{name: "兜底推进 run", run: e.sweepStuckRuns},
		{name: "并发组兜底", run: e.sweepConcurrencyGroups},
		{name: "清理随机数", run: e.sweepNonces},
		{name: "git 凭据对账", run: e.sweepGitCred},
	}
	changed := false
	for _, st := range stages {
		if ctx.Err() != nil {
			return
		}
		c, err := st.run(ctx, st.name)
		if err != nil && ctx.Err() == nil {
			e.log.Error("sweeper", "stage", st.name, "err", err)
		}
		changed = changed || c
	}
	if changed {
		e.hub.All()
	}
}

// sweepGitCred 对账签发登记（Deps.ReconcileGitCred）：登记中还没有失效、数据库中已经结束的 attempt 在这里失效，补上
// endEachInTx 或 Complete 在事务提交之后没能发出的 AttemptEnded（例如提交确认丢失），延迟最多一个 sweeper 周期。
func (e *Engine) sweepGitCred(ctx context.Context, _ string) (bool, error) {
	if e.reconcile == nil {
		return false, nil
	}
	return false, e.reconcile(ctx, e.runningAttempts)
}

// runningAttempts 返回 ids 中还没有结束（outcome 为空）的 attempt，只读。不存在的 attempt 不在结果中。
func (e *Engine) runningAttempts(ctx context.Context, ids []int64) ([]int64, error) {
	rows, err := e.db.Query(ctx, `SELECT id FROM attempts WHERE id = ANY($1) AND outcome IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// eachInTx 对每个 ID 在单独的事务中执行 f。单个对象出错时记录日志并继续处理下一个，
// 返回是否有事务成功执行。
func (e *Engine) eachInTx(ctx context.Context, stage string, ids []int64, f func(tx pgx.Tx, id int64) error) bool {
	return e.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) { return 0, true, f(tx, id) })
}

// endEachInTx 与 eachInTx 相同，f 返回事务中结束的 attempt（0 表示没有）和是否真的改变了状态：事务提交后调用
// AttemptEnded，只有改变过状态才唤醒其他循环（加锁后复查发现没有可处理的对象时不算）。
func (e *Engine) endEachInTx(ctx context.Context, stage string, ids []int64, f func(tx pgx.Tx, id int64) (ended int64, changed bool, err error)) bool {
	changed := false
	for _, id := range ids {
		var ended int64
		var itemChanged bool
		if err := e.db.Tx(ctx, func(tx pgx.Tx) (err error) {
			ended, itemChanged, err = f(tx, id)
			return err
		}); err != nil {
			if ctx.Err() != nil {
				return changed
			}
			e.log.Warn("sweeper", "stage", stage, "id", id, "err", err)
			continue
		}
		if ended != 0 {
			e.AttemptEnded(ctx, ended)
		}
		changed = changed || itemChanged
	}
	return changed
}

// finishNative 结束 Runner 执行的 attempt（FinishAttempt），返回结束的 attempt。runs 是加锁时预先锁定的 run 集合。
func (e *Engine) finishNative(ctx context.Context, tx pgx.Tx, j *cistore.Job, res AttemptResult, runs []int64) (int64, error) {
	if err := e.FinishAttempt(ctx, tx, j, res, runs); err != nil || j.CurrentAttemptID == nil {
		return 0, err
	}
	return *j.CurrentAttemptID, nil
}

// sweepExpiredLeases 处理租约过期（Runner 失联）的 job。
func (e *Engine) sweepExpiredLeases(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, e.db, `SELECT id FROM jobs WHERE `+locallyLeasedJobs+` AND lease_expires_at < now() LIMIT $1`, sweepBatch)
	if err != nil {
		return false, err
	}
	return e.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) {
		runs, err := cistore.LockRunsOfJob(ctx, tx, id)
		if err != nil {
			return 0, false, err
		}
		j, err := cistore.GetJob(ctx, tx, id, true)
		if err != nil {
			return 0, false, err
		}
		if j.State != store.JobRunning || j.LeaseExpiresAt == nil {
			return 0, false, nil
		}
		// 加锁后按数据库的时钟复查（扫描与加锁之间租约可能已经被续期；本地时钟与数据库可能有偏差）。
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at < now() FROM jobs WHERE id=$1`, id).Scan(&expired); err != nil || !expired {
			return 0, false, err
		}
		exec, err := cistore.GetAttemptExec(ctx, tx, j.CurrentAttemptID)
		if err != nil {
			return 0, false, err
		}
		if exec.Managed() {
			// 加锁后复查：托管的预留到期撤销（没有发出任何外部调用）；其他托管阶段不应有租约，不由 Server 在本地结束（I11）
			if exec.Phase == cistore.PhaseReserved {
				e.log.Info("托管预留到期，撤销", "job", j.ID, "epoch", j.Epoch, "pool", exec.ManagedPool)
				return 0, true, e.Release(ctx, tx, j, "预留到期，没有派发")
			}
			e.log.Error("托管 attempt 不应有 Server 本地的租约，不按失联结束", "job", j.ID, "epoch", j.Epoch, "pool", exec.ManagedPool, "phase", exec.Phase)
			return 0, false, nil
		}
		e.log.Warn("租约过期", "job", j.ID, "epoch", j.Epoch)
		ended, err := e.finishNative(ctx, tx, j, AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: ErrClassLost, Message: "Runner 失联（租约过期）"}, runs)
		return ended, true, err
	}), nil
}

// sweepTimeouts 处理超时的 job：started_at + timeout + cancel_grace + lease_ttl 之后仍未收到结果。
// spec 中没有 timeout_minutes（或不是正数）时按 workflow.DefaultTimeoutMinutes 计算。
// Server 启动后的 2 × lease_ttl 内不判定（见 ExtendLeasesOnStartup）。
func (e *Engine) sweepTimeouts(ctx context.Context, stage string) (bool, error) {
	if time.Now().UnixNano() < e.timeoutsFrom.Load() {
		return false, nil
	}
	ids, err := store.QueryIDs(ctx, e.db, `SELECT id FROM jobs WHERE `+timedOutJobs+` LIMIT $3`,
		int64((e.cfg.CancelGrace+e.cfg.LeaseTTL)/time.Second), workflow.DefaultTimeoutMinutes, sweepBatch)
	if err != nil {
		return false, err
	}
	return e.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) {
		ended, err := e.timeoutJob(ctx, tx, id)
		return ended, ended != 0, err
	}), nil
}

// timeoutJob 加锁后重新检查超时条件，返回结束的 attempt（0 表示没有）。扫描后原 attempt 可能已结束并重新领取，新的 attempt
// 虽然也处于 running，但 started_at 已更新，不能被旧的超时检查结束。
func (e *Engine) timeoutJob(ctx context.Context, tx pgx.Tx, id int64) (int64, error) {
	runs, err := cistore.LockRunsOfJob(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	jobs, err := cistore.QueryJobs(ctx, tx, timedOutJobs+` AND id=$3 FOR UPDATE`,
		int64((e.cfg.CancelGrace+e.cfg.LeaseTTL)/time.Second), workflow.DefaultTimeoutMinutes, id)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	exec, err := cistore.GetAttemptExec(ctx, tx, jobs[0].CurrentAttemptID)
	if err != nil {
		return 0, err
	}
	if exec.Managed() {
		// 托管执行的时限由 Provider 在远端强制，超时后由对账请求取消（PLAN #32.6）；Server 不在本地判定超时（I11）
		e.log.Error("托管 attempt 不应由 Server 判定超时，不结束", "job", jobs[0].ID, "pool", exec.ManagedPool, "phase", exec.Phase)
		return 0, nil
	}
	return e.finishNative(ctx, tx, jobs[0], AttemptResult{Outcome: store.OutcomeTimedOut, ErrClass: errClassTimeout, Message: "超时（Server 判定）"}, runs)
}

// sweepStuckRuns 兜底推进 run：所有 job 已结束但 run 仍未结束、跟随的 job 已结束、或上游已全部结束的 waiting job。
func (e *Engine) sweepStuckRuns(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, e.db, `SELECT r.id FROM runs r WHERE r.state='active' AND (
		NOT EXISTS (SELECT 1 FROM jobs j WHERE j.run_id=r.id AND j.rerun=r.current_rerun AND j.state <> ALL($1))
		OR EXISTS (SELECT 1 FROM jobs j JOIN jobs f ON f.id=j.follows_job_id WHERE j.run_id=r.id AND j.rerun=r.current_rerun AND j.state='waiting'
			AND f.state = ANY($1))
		OR EXISTS (SELECT 1 FROM jobs j WHERE j.run_id=r.id AND j.rerun=r.current_rerun AND j.state='waiting' AND j.follows_job_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM jobs u WHERE u.id = ANY(j.needs) AND u.state <> ALL($1))))
		LIMIT $2`, store.TerminalJobStateArgs(), sweepRunBatch)
	if err != nil {
		return false, err
	}
	return e.eachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) error { return e.AdvanceRun(ctx, tx, id) }), nil
}

// sweepConcurrencyGroups 是并发组兜底：组内已经没有 active 的 run，却还有 queued 的
// （创建 run 与上一个 run 结束并发时可能漏掉唤醒）。
func (e *Engine) sweepConcurrencyGroups(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, e.db, `SELECT DISTINCT ON (repo_id, concurrency_key) id FROM runs q
		WHERE state='queued' AND concurrency_key IS NOT NULL AND updated_at < now() - make_interval(secs => $2)
		AND NOT EXISTS (SELECT 1 FROM runs a WHERE a.repo_id=q.repo_id AND a.concurrency_key=q.concurrency_key AND a.state='active')
		ORDER BY repo_id, concurrency_key, id LIMIT $1`, sweepRunBatch, concurrencyGroupGrace.Seconds())
	if err != nil {
		return false, err
	}
	return e.eachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) error {
		run, err := cistore.GetRun(ctx, tx, id, false)
		if err != nil || run.State != store.RunQueued || run.ConcurrencyKey == nil {
			return err
		}
		// 先锁并发组，再锁 run 并复查状态
		if err := cistore.LockConcurrencyGroup(ctx, tx, run.RepoID, *run.ConcurrencyKey); err != nil {
			return err
		}
		if run, err = cistore.GetRun(ctx, tx, id, true); err != nil || run.State != store.RunQueued {
			return err
		}
		active, err := cistore.CountActiveInGroup(ctx, tx, run.RepoID, *run.ConcurrencyKey, 0)
		if err != nil || active > 0 {
			return err
		}
		e.log.Info("激活并发组中排队的 run", "run", id)
		return e.activateQueued(ctx, tx, id)
	}), nil
}

// sweepNonces 清理过期的认证随机数。
func (e *Engine) sweepNonces(ctx context.Context, _ string) (bool, error) {
	_, err := e.db.Exec(ctx, `DELETE FROM auth_nonces WHERE created_at < now() - make_interval(secs => $1)`, e.cfg.NonceRetention.Seconds())
	return false, err
}

package engine

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 托管执行的状态转移（PLAN #32.6）。托管 attempt 在实际开始执行前依次经过 reserved（已预留，没有外部调用）、submitting、
// queued；每次推进都带 (attempt, epoch, phase) 条件，0 行即过期（ErrStale），与 #6.2 的 epoch 规则相同。

// ErrStale 表示 attempt 已经不是 job 当前的 attempt，或者不在可以这样转移的阶段。
var ErrStale = errors.New("attempt 已过期或不在可以转移的阶段")

// ErrClassReleased 是撤销预留时 attempt 记录的错误分类：预留没有执行过，不算一次尝试。
const ErrClassReleased = "infra.released"

// Release 撤销一个还没有提交的托管预留（池被禁用、预留到期；取消见 cancelJob，PLAN #32.6）：只接受当前 attempt 处于 reserved，
// 否则返回 ErrStale。attempt 记 infra_error / infra.released；job 回到 pending，提交时间和 queued_at 都不变（排队位置不变，
// cistore.QueueOrderSQL），attempt_count 减一（没有执行过，不算重试，I6）。j 由调用方锁住（加锁顺序见 cistore.LockRuns）；调用方在提交后唤醒。
func (e *Engine) Release(ctx context.Context, tx pgx.Tx, j *cistore.Job, message string) error {
	if j.State != store.JobRunning || j.CurrentAttemptID == nil {
		return ErrStale
	}
	tag, err := tx.Exec(ctx, `UPDATE attempts SET outcome=$3, error_class=$4, message=nullif($5,''), finished_at=now()
		WHERE id=$1 AND epoch=$2 AND managed_pool IS NOT NULL AND phase='reserved' AND outcome IS NULL`,
		*j.CurrentAttemptID, j.Epoch, store.OutcomeInfraError, ErrClassReleased, message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStale
	}
	reason := "预留已撤销（" + message + "），按原来的排队位置等待调度"
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state=$3, lease_expires_at=NULL, attempt_count=attempt_count-1, started_step=false, reason=$2
		WHERE id=$1 AND epoch=$4`, j.ID, reason, store.JobPending, j.Epoch); err != nil {
		return err
	}
	j.State, j.Reason, j.LeaseExpiresAt = store.JobPending, &reason, nil
	j.AttemptCount--
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// MarkStarted 记录托管执行实际开始（reserved、submitting、queued → running）：写 jobs.started_at（Check Run 变为 in_progress）
// 并清空租约——之后它是否还在执行只能由 Provider 的观察和对账确定，Server 不在本地判定失联或超时（I11）。
// attempt 不是 job 当前的、不是托管的或不在这些阶段时返回 ErrStale。调用方在提交后唤醒。
func (e *Engine) MarkStarted(ctx context.Context, tx pgx.Tx, jobID, attemptID, epoch int64) error {
	j, err := cistore.GetJob(ctx, tx, jobID, true)
	if err != nil {
		return err
	}
	if j.State != store.JobRunning || j.Epoch != epoch || j.CurrentAttemptID == nil || *j.CurrentAttemptID != attemptID {
		return ErrStale
	}
	var pool string
	err = tx.QueryRow(ctx, `UPDATE attempts SET phase='running' WHERE id=$1 AND epoch=$2 AND managed_pool IS NOT NULL
		AND phase IN ('reserved', 'submitting', 'queued') AND outcome IS NULL RETURNING managed_pool`, attemptID, epoch).Scan(&pool)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	if err != nil {
		return err
	}
	reason := "在" + cistore.ManagedPoolLabel + pool + " 上执行"
	if err := tx.QueryRow(ctx, `UPDATE jobs SET started_at=now(), lease_expires_at=NULL, reason=$2 WHERE id=$1 RETURNING started_at`,
		j.ID, reason).Scan(&j.StartedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE run_reruns SET started_at=coalesce(started_at, now()) WHERE run_id=$1 AND rerun=$2`, j.RunID, j.Rerun); err != nil {
		return err
	}
	j.Reason, j.LeaseExpiresAt = &reason, nil
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// markUnknown 把提交中或结果未知的托管 attempt 置为 unknown：attempt 不结束（outcome 仍为空），job 保持 running、继续占用配额，
// 等待对账确认远端是否在执行（PLAN #32.6，I5）。
func (e *Engine) markUnknown(ctx context.Context, tx pgx.Tx, j *cistore.Job, reason string) error {
	if j.CurrentAttemptID == nil {
		return ErrStale
	}
	tag, err := tx.Exec(ctx, `UPDATE attempts SET phase='unknown' WHERE id=$1 AND epoch=$2 AND outcome IS NULL`, *j.CurrentAttemptID, j.Epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStale
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET reason=$2 WHERE id=$1`, j.ID, reason); err != nil {
		return err
	}
	j.Reason = &reason
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

// cancelReserved 取消一个当前 attempt 处于 reserved 的 job：预留没有发出任何外部调用，直接撤销并置为 cancelled。
// attempt 必须仍是 job 当前的 attempt、处于 reserved（带 epoch 条件，0 行即 ErrStale）：与 Provider 的提交并发时
// 不能把已经推进到 submitting 的预留当作没发出去（I5/I11）。
func (e *Engine) cancelReserved(ctx context.Context, tx pgx.Tx, j *cistore.Job, reason store.CancelReason) error {
	if j.CurrentAttemptID == nil {
		return ErrStale
	}
	tag, err := tx.Exec(ctx, `UPDATE attempts SET outcome=$3, finished_at=now()
		WHERE id=$1 AND epoch=$2 AND managed_pool IS NOT NULL AND phase='reserved' AND outcome IS NULL`,
		*j.CurrentAttemptID, j.Epoch, store.OutcomeCancelled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStale
	}
	tag, err = tx.Exec(ctx, `UPDATE jobs SET state=$4, cancel_reason=$2, reason=$3, finished_at=now(), lease_expires_at=NULL, follows_job_id=NULL
		WHERE id=$1 AND epoch=$5`, j.ID, reason, reason.Text(), store.JobCancelled, j.Epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStale
	}
	r, text := string(reason), reason.Text()
	j.State, j.CancelReason, j.Reason, j.LeaseExpiresAt = store.JobCancelled, &r, &text, nil
	return cicheck.EnqueueJob(ctx, e.outbox, tx, j)
}

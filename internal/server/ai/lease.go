package ai

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// attempt 级写入的租约规则（PLAN #33.6、#33.9 O6）：任务在执行中、当前的 attempt 就是这一次领取、租约没有过期。规则只在这里
// 定义一次，心跳的续约、日志、开发任务的候选和领取、提交结果都用它判断。租约过期即拒绝，不等 sweeper 把任务判为失联。
//
// 判断必须发生在拿到任务行锁之后，并且用 clock_timestamp()（语句执行的时刻）而不是 now()（事务开始的时刻）：等待行锁的期间
// 租约可能过期，用事务开始时刻、或者在等锁之前求值的条件都会掩盖这段等待。$1 是任务 ID，$2 是 attempt ID。

// leaseLiveSQL 是租约规则的 SQL 形式：可以作为判断条件（SELECT ... FROM ai_tasks WHERE id=$1），也可以作为写入自己的条件
// （UPDATE ... WHERE id=$1 AND ...）。
const leaseLiveSQL = `state='running' AND current_attempt_id=$2 AND lease_expires_at > clock_timestamp()`

// leaseHolds 判断任务是否还接受 attemptID 的写入（leaseLiveSQL）：任务不存在、不在执行中、当前 attempt 不是它或者租约已经
// 过期时返回 false。已经锁住任务行的事务中调用就是租约规则的最终判断；没有加锁时只能作为预检。
func leaseHolds(ctx context.Context, q store.Querier, taskID, attemptID int64) (bool, error) {
	var live bool
	err := q.QueryRow(ctx, `SELECT `+leaseLiveSQL+` FROM ai_tasks WHERE id=$1`, taskID, attemptID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return live, err
}

// requireLease 在已经锁住任务行的事务中应用租约规则：不满足时返回 STALE_EPOCH。
func requireLease(ctx context.Context, q store.Querier, taskID, attemptID int64) error {
	live, err := leaseHolds(ctx, q, taskID, attemptID)
	if err != nil {
		return err
	}
	if !live {
		return rpcerr.StaleEpoch()
	}
	return nil
}

// lockTaskRow 锁住任务行（等待并发的事务释放）并返回持锁后读到的任务：attempt 级的写入在判断租约之前先拿到行锁。
func lockTaskRow(ctx context.Context, tx pgx.Tx, taskID int64) (*Task, error) {
	return GetTask(ctx, tx, taskID, true)
}

// lockLiveAttempt 锁住任务行，持锁之后复查租约，返回持锁后读到的任务；租约规则不满足时返回 STALE_EPOCH。attempt 级的写入
// 在自己的事务中调用它，等待行锁期间到期的租约同样被拒绝。
func lockLiveAttempt(ctx context.Context, tx pgx.Tx, taskID, attemptID int64) (*Task, error) {
	t, err := lockTaskRow(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if err := requireLease(ctx, tx, taskID, attemptID); err != nil {
		return nil, err
	}
	return t, nil
}

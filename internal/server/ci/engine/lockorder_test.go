package engine

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 结束 run 时要锁并发组（releaseConcurrency），加入并发组时要取消组内的 run（先锁并发组再锁 run）。
// sweeper 判定超时结束 run 的最后一个 job 时，必须先锁并发组、再锁 run 和 job（cistore.LockRuns 的加锁顺序）；
// 否则它持有 run 和 job、等待并发组，而规划器持有并发组、等待 run，形成死锁。
//
// 时序是确定的：规划器的事务先拿到并发组的锁，sweeper 开始处理并阻塞一段时间后，规划器再取消 run。
func TestLockOrder_finishRunVsConcurrencyCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{concurrency: "deploy"})
	id := e.addJob(run, jobOpts{})
	e.advance(run)
	e.claim(id)
	e.exec(`UPDATE jobs SET started_at=now()-interval '1 day' WHERE id=$1`, id)

	locked := make(chan struct{})
	cancelled := make(chan error, 1)
	go func() {
		cancelled <- e.db.Tx(e.ctx, func(tx pgx.Tx) error {
			// 与 planner.joinConcurrencyGroup 相同：先锁并发组，再取消组内的 run
			if err := cistore.LockConcurrencyGroup(e.ctx, tx, testRepoID, "deploy"); err != nil {
				return err
			}
			select {
			case <-locked:
			default:
				close(locked)
			}
			time.Sleep(300 * time.Millisecond) // 让 sweeper 开始处理并阻塞
			return e.eng.CancelRun(e.ctx, tx, run, store.CancelByConcurrency)
		})
	}()
	<-locked
	if _, err := e.eng.sweepTimeouts(e.ctx, "超时"); err != nil {
		t.Fatalf("sweepTimeouts：%v", err)
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("CancelRun：%v", err)
	}
	if r := e.db.Retries(); r != 0 {
		t.Fatalf("事务重试了 %d 次（死锁），应为 0", r)
	}
	e.wantJobState(id, store.JobTimedOut)
}

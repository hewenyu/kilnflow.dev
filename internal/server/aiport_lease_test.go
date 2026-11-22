package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
)

// attempt 级写入的租约规则（PLAN #33.6）：租约过期即拒绝，不等 sweeper；判断发生在拿到任务行锁之后、用数据库的当前时间，
// 因此等待行锁期间过期的租约也不会被续活。

// 租约已经过期、sweeper 还没处理时，心跳、日志、提交和开发任务的事务内锁都返回 STALE_EPOCH，且什么也不改变。
func TestAIPort_expiredLeaseRejected(t *testing.T) {
	p := newSplitPort(t, portConfig)
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	if err := p.appendLog(r, task, 0, "ok\n"); err != nil {
		t.Fatalf("租约有效时的日志：%v", err)
	}
	// 直接让租约过期，不跑 sweeper（与审计报告 F1 第 2 条相同的条件）
	p.exec(`UPDATE ai_tasks SET lease_expires_at=clock_timestamp() - interval '1 minute' WHERE id=$1`, id)
	if err := p.appendLog(r, task, 3, "expired\n"); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("租约过期的日志：%v，应为 STALE_EPOCH", err)
	}
	if _, err := p.heartbeat(r, task); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("租约过期的心跳：%v，应为 STALE_EPOCH", err)
	}
	if _, err := p.complete(r, task, succeeded(okReport)); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("租约过期的提交：%v，应为 STALE_EPOCH", err)
	}
	// 开发任务的候选和领取在自己的事务中用同一条规则（LockCurrentAttempt）
	err := p.s.db.Tx(p.ctx, func(tx pgx.Tx) error {
		_, err := ai.LockCurrentAttempt(p.ctx, tx, id, task.attempt)
		return err
	})
	if !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("租约过期的任务行锁：%v，应为 STALE_EPOCH", err)
	}
	if n := p.count(`SELECT count(*) FROM ai_tasks WHERE id=$1 AND lease_expires_at > clock_timestamp()`, id); n != 0 {
		t.Fatal("过期的心跳续租了租约")
	}
	if n := p.count(`SELECT count(*) FROM ai_attempts WHERE id=$1 AND outcome IS NULL AND log_size=$2`, task.attempt, 3); n != 1 {
		t.Fatal("过期的 attempt 的结果或日志大小被改动了")
	}
	size, err := p.s.ai.Logs().Size(task.attempt)
	if err != nil || size != 3 {
		t.Fatalf("日志长度 = %d（%v），应为第一次写入的 3 字节", size, err)
	}
	wantTask(t, p, id, "running")
}

// 心跳的预检通过后等待任务行锁，等锁期间租约到期：拿到锁之后续约被拒（STALE_EPOCH），过期执行没有重新得到授权。
func TestAIPort_heartbeatLeaseExpiresWhileWaiting(t *testing.T) {
	p := newSplitPort(t, portConfig)
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	// 1 秒后过期：让心跳在等锁期间跨过这个时刻
	p.exec(`UPDATE ai_tasks SET lease_expires_at=clock_timestamp() + interval '1 second' WHERE id=$1`, id)
	locked, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- p.s.db.Tx(p.ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(p.ctx, `SELECT id FROM ai_tasks WHERE id=$1 FOR UPDATE`, id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	done := make(chan error, 1)
	go func() {
		_, err := p.heartbeat(r, task)
		done <- err
	}()
	// 心跳已经通过预检、停在任务行锁上
	waitFor(t, "心跳等待任务行锁", func() bool {
		return p.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE '%FROM ai_tasks WHERE id=$1 FOR UPDATE%'`) > 0
	})
	// 等租约自然过期再放行
	waitFor(t, "租约过期", func() bool {
		return p.count(`SELECT count(*) FROM ai_tasks WHERE id=$1 AND lease_expires_at < clock_timestamp()`, id) > 0
	})
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("等锁期间租约过期后的心跳：%v，应为 STALE_EPOCH", err)
	}
	if n := p.count(`SELECT count(*) FROM ai_tasks WHERE id=$1 AND lease_expires_at > clock_timestamp()`, id); n != 0 {
		t.Fatal("心跳把等锁期间过期的租约续活了")
	}
	// 任务仍由 sweeper 判失联
	wantTask(t, p, id, "running")
	p.sweep()
	wantTask(t, p, id, "failed")
}

// waitFor 等到 check 返回真（最多 10 秒）。
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatalf("等待%s超时", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

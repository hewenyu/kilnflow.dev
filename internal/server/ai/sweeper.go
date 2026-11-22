package ai

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 的 sweeper（PLAN #30.6）：租约过期（失联）、超时、兜底汇总请求、并发组兜底。与 CI 的 sweeper 各自独立，只处理 AI 的表。

const (
	// defaultSweepInterval 是配置没有给出（或不是正数）时的 sweeper 周期：time.NewTicker 不接受非正数。
	defaultSweepInterval = 5 * time.Second
	// sweepBatch 是每个阶段每轮最多处理的对象数。
	sweepBatch = 100
	// groupGrace 是并发组兜底前请求至少排队的时间：刚排队的请求可能正在等待上一个请求结束时的正常唤醒。
	groupGrace = 5 * time.Second
	// startupWindowLeases 是启动后不判定超时、为租约延期的时长（以 lease_ttl 计）：停机期间按时完成的任务要等 Runner
	// 重新提交结果。
	startupWindowLeases = 2
)

// timedOutTasks 是超时条件：开始执行后 timeout + cancel_grace + lease_ttl 仍未收到结果。$1 为宽限时间（秒）。
// 状态写成字面量，规划器据此选用部分索引 ai_tasks_leased。
const timedOutTasks = `state='running' AND started_at + make_interval(secs => timeout_minutes * 60 + $1) < now()`

// RunSweeper 每隔 SweepInterval 执行一轮，直到 ctx 结束。
func (c *Core) RunSweeper(ctx context.Context) {
	t := time.NewTicker(c.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.sweep(ctx)
	}
}

// ExtendLeasesOnStartup 在 Server 启动时为执行中任务的租约延期，并把超时判定推迟到启动后 2 × lease_ttl，避免停机期间的任务
// 被判定为失联或超时。
func (c *Core) ExtendLeasesOnStartup(ctx context.Context) error {
	window := startupWindowLeases * c.cfg.LeaseTTL
	// 这个截止时间只表示 Server 启动后的本地保护窗口，与数据库的时间列无关，两端都必须用 Server 本地时钟比较。
	c.timeoutsFrom.Store(time.Now().Add(window).UnixNano())
	_, err := c.db.Exec(ctx, `UPDATE ai_tasks SET lease_expires_at = greatest(lease_expires_at, now() + make_interval(secs => $1))
		WHERE state='running'`, window.Seconds())
	return err
}

// SweepOnce 立即执行一轮（模块测试用）。
func (c *Core) SweepOnce(ctx context.Context) { c.sweep(ctx) }

func (c *Core) sweep(ctx context.Context) {
	stages := []struct {
		name string
		run  func(ctx context.Context, stage string) (bool, error)
	}{
		{"AI 过期租约", c.sweepExpiredLeases},
		{"AI 超时", c.sweepTimeouts},
		{"AI 兜底汇总请求", c.sweepStuckRequests},
		{"AI 并发组兜底", c.sweepGroups},
		{"AI git 凭据对账", c.sweepGitCred},
	}
	changed := false
	for _, st := range stages {
		if ctx.Err() != nil {
			return
		}
		ch, err := st.run(ctx, st.name)
		if err != nil && ctx.Err() == nil {
			c.log.Error("sweeper", "stage", st.name, "err", err)
		}
		changed = changed || ch
	}
	if changed {
		c.hub.All()
	}
}

// eachInTx 对每个 ID 在单独的事务中执行 f；单个对象出错时记录日志并继续。返回是否有事务成功执行。
func (c *Core) eachInTx(ctx context.Context, stage string, ids []int64, f func(tx pgx.Tx, id int64) error) bool {
	return c.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) { return 0, true, f(tx, id) })
}

// sweepGitCred 对账签发登记（gitcred.Registry.Reconcile）：登记中还没有失效、数据库中已经结束的 attempt 在这里失效，补上
// endEachInTx 或 Complete 在事务提交之后没能发出的 attemptEnded（例如提交确认丢失），延迟最多一个 sweeper 周期。
func (c *Core) sweepGitCred(ctx context.Context, _ string) (bool, error) {
	if c.gitCred == nil {
		return false, nil
	}
	_, err := c.gitCred.Reconcile(ctx, gitcred.DomainAI, c.runningAttempts)
	return false, err
}

// runningAttempts 返回 ids 中还没有结束（outcome 为空）的 attempt，只读。不存在的 attempt 不在结果中。
func (c *Core) runningAttempts(ctx context.Context, ids []int64) ([]int64, error) {
	rows, err := c.db.Query(ctx, `SELECT id FROM ai_attempts WHERE id = ANY($1) AND outcome IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// endEachInTx 与 eachInTx 相同，f 返回事务中结束的 attempt（0 表示没有）和是否真的改变了状态：事务提交后调用
// attemptEnded，只有改变过状态才唤醒其他循环（加锁后复查发现没有可处理的对象时不算）。
func (c *Core) endEachInTx(ctx context.Context, stage string, ids []int64, f func(tx pgx.Tx, id int64) (ended int64, changed bool, err error)) bool {
	changed := false
	for _, id := range ids {
		var ended int64
		var itemChanged bool
		err := c.db.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			ended, itemChanged, err = f(tx, id)
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return changed
			}
			c.log.Warn("sweeper", "stage", stage, "id", id, "err", err)
			continue
		}
		if ended != 0 {
			c.attemptEnded(ctx, ended)
		}
		changed = changed || itemChanged
	}
	return changed
}

// currentAttempt 返回任务当前的 attempt（没有时为 0）。
func currentAttempt(t *Task) int64 {
	if t.CurrentAttemptID == nil {
		return 0
	}
	return *t.CurrentAttemptID
}

// sweepExpiredLeases 结束租约过期（Runner 失联）的任务。
func (c *Core) sweepExpiredLeases(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, c.db, `SELECT id FROM ai_tasks WHERE state='running' AND lease_expires_at < now() LIMIT $1`, sweepBatch)
	if err != nil {
		return false, err
	}
	return c.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) {
		r, t, err := lockTaskForFinish(ctx, tx, id)
		if err != nil {
			return 0, false, err
		}
		if t.State != TaskRunning || t.LeaseExpiresAt == nil {
			return 0, false, nil
		}
		// 加锁后按数据库的时钟复查（扫描与加锁之间租约可能已经被续期；本地时钟与数据库可能有偏差）。
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at < now() FROM ai_tasks WHERE id=$1`, id).Scan(&expired); err != nil || !expired {
			return 0, false, err
		}
		return currentAttempt(t), true, c.lost(ctx, tx, r, t)
	}), nil
}

// sweepTimeouts 结束超时的任务。Server 启动后的 2 × lease_ttl 内不判定（ExtendLeasesOnStartup）。
func (c *Core) sweepTimeouts(ctx context.Context, stage string) (bool, error) {
	if time.Now().UnixNano() < c.timeoutsFrom.Load() {
		return false, nil
	}
	grace := int64((c.cfg.CancelGrace + c.cfg.LeaseTTL) / time.Second)
	ids, err := store.QueryIDs(ctx, c.db, `SELECT id FROM ai_tasks WHERE `+timedOutTasks+` LIMIT $2`, grace, sweepBatch)
	if err != nil {
		return false, err
	}
	return c.endEachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) (int64, bool, error) {
		r, t, err := lockTaskForFinish(ctx, tx, id)
		if err != nil {
			return 0, false, err
		}
		// 加锁后复查：扫描之后原 attempt 可能已经结束
		var still bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ai_tasks WHERE `+timedOutTasks+` AND id=$2)`, grace, id).Scan(&still); err != nil || !still {
			return 0, false, err
		}
		return currentAttempt(t), true, c.finishTask(ctx, tx, r, t, Result{Outcome: store.OutcomeTimedOut, ErrClass: errClassTimeout, Message: "超时（Server 判定）"})
	}), nil
}

// sweepStuckRequests 兜底汇总任务都已结束、却仍是 active 的请求（例如迁移时以 infra.upgrade 结束的任务）。
func (c *Core) sweepStuckRequests(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, c.db, `SELECT r.id FROM ai_requests r WHERE r.state='active'
		AND NOT EXISTS (SELECT 1 FROM ai_tasks t WHERE t.request_id=r.id AND t.state <> ALL($1)) ORDER BY r.id LIMIT $2`,
		terminalTaskArgs(), sweepBatch)
	if err != nil {
		return false, err
	}
	return c.eachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) error {
		r, err := lockRequest(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := RequestTasks(ctx, tx, id, true); err != nil {
			return err
		}
		return c.settle(ctx, tx, r)
	}), nil
}

// sweepGroups 是并发组兜底：组内已经没有 active 的请求，却还有排队的（规划与上一个请求结束并发时可能漏掉唤醒）。
func (c *Core) sweepGroups(ctx context.Context, stage string) (bool, error) {
	ids, err := store.QueryIDs(ctx, c.db, `SELECT DISTINCT ON (repo_id, group_key) id FROM ai_requests q
		WHERE state='queued' AND group_key IS NOT NULL AND updated_at < now() - make_interval(secs => $2)
		AND NOT EXISTS (SELECT 1 FROM ai_requests a WHERE a.repo_id=q.repo_id AND a.group_key=q.group_key AND a.state='active')
		ORDER BY repo_id, group_key, id DESC LIMIT $1`, sweepBatch, groupGrace.Seconds())
	if err != nil {
		return false, err
	}
	return c.eachInTx(ctx, stage, ids, func(tx pgx.Tx, id int64) error {
		r, err := lockRequest(ctx, tx, id)
		if err != nil || r.State != RequestQueued {
			return err
		}
		return c.releaseGroup(ctx, tx, r)
	}), nil
}

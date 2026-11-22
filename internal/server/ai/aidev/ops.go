package aidev

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 人的操作（PLAN #26.15 失控的处理）：暂停 / 恢复仓库的新领取、终止工作项、让受阻的工作项再返修一轮。

// ErrNotRetryable 表示工作项不是受阻状态，不能 retry。
var ErrNotRetryable = errors.New("只有受阻（blocked）的工作项可以重试")

// ErrRepoNotFound 表示仓库不存在。
var ErrRepoNotFound = errors.New("仓库不存在")

// ErrFinished 表示工作项已经结束。
var ErrFinished = errors.New("工作项已经结束")

// SetPaused 暂停或恢复仓库的 AI 开发（暂停时不再领取新的 Issue，已有的工作项照常收尾）。
func (s *Service) SetPaused(ctx context.Context, repoID int64, paused bool, actor string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := setPaused(ctx, tx, repoID, paused); err != nil {
			return err
		}
		action := "aidev.resume"
		if paused {
			action = "aidev.pause"
		}
		return store.Audit(ctx, tx, actor, action, map[string]any{"repo_id": repoID})
	})
	if err == nil {
		s.notify()
	}
	return err
}

// Stop 终止工作项（PLAN #26.9 任意未结束状态 → stopped）：取消执行中的任务（Runner 保存 checkpoint），释放 Issue 并记入
// 跳过记录（内容不变就不再选，重新打上 kiln:ready 清除）。PR 保持原样，由人决定关闭还是接手。
func (s *Service) Stop(ctx context.Context, id int64, reason, actor string) error {
	return s.stop(ctx, id, EvStop, reason, actor)
}

// stop 让工作项收到 ev（EvStop 或 EvMerged），先取消它执行中的任务。在仓库锁下进行（与执行者串行）：执行者正在推进这个
// 工作项（创建正式分支、PR）时等它做完——推进的结果（PR 编号）记在工作项上之后再终止，终止成功之后不会再由推进建出
// PR。加锁顺序：仓库锁 → 并发组 → 请求 → 任务 → 工作项（与结束任务相同）。
func (s *Service) stop(ctx context.Context, id int64, ev Event, reason, actor string) error {
	w0, err := GetWorkItem(ctx, s.db, id)
	if err != nil {
		return err
	}
	var cancelReq int64
	err = s.withRepoLock(ctx, w0.RepoID, lockWait, func(conn *pgxpool.Conn) error {
		return s.tx(ctx, conn, func(tx pgx.Tx) error {
			var err error
			cancelReq, err = s.stopTx(ctx, tx, id, ev, reason, actor)
			return err
		})
	})
	if err != nil {
		return err
	}
	// 等待工作项锁期间关联上来的新任务不在已经取消的请求里：事务提交后再取消（见 stopTx 的加锁顺序说明）。
	s.cancelAfterCommit(ctx, id, cancelReq)
	s.notify()
	return nil
}

// cancelAfterCommit 取消事务提交之后才发现的请求（best-effort：工作项已经是终态，任务再跑也没有结果可以推进）。
func (s *Service) cancelAfterCommit(ctx context.Context, id, requestID int64) {
	if requestID == 0 || s.core == nil {
		return
	}
	if err := s.core.Cancel(ctx, requestID, ai.CancelWorkItemDone, ""); err != nil && ctx.Err() == nil && s.log != nil {
		s.log.Warn("终态的工作项还有新任务的请求，取消失败", "work_item", id, "request", requestID, "err", err)
	}
}

// stopTx 在事务 tx 中终止工作项（调用方持有仓库锁），返回需要在事务提交后取消的请求（0 表示没有）。
func (s *Service) stopTx(ctx context.Context, tx pgx.Tx, id int64, ev Event, reason, actor string) (cancelReq int64, err error) {
	w, err := GetWorkItem(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if !Active(w.State) {
		return 0, ErrFinished
	}
	// 先取消执行中的任务，再加锁工作项：结束任务的事务持有请求、任务锁并等待工作项锁，反过来做会死锁（加锁顺序：
	// 并发组 → 请求 → 任务 → 工作项）。
	var cancelledTaskID int64
	if w.TaskID != nil {
		t, err := ai.GetTask(ctx, tx, *w.TaskID, false)
		if err != nil {
			return 0, err
		}
		if err := s.core.CancelTx(ctx, tx, t.RequestID, ai.CancelWorkItemDone, ""); err != nil {
			return 0, err
		}
		cancelledTaskID = *w.TaskID
	}
	// 锁住之后复查：等待工作项锁期间工作项可能已经结束（别的路径终止、合并）。
	w, err = queryWorkItem(ctx, tx, `id=$1`+forUpdate, id)
	if err != nil {
		return 0, err
	}
	if w == nil || !Active(w.State) {
		return 0, ErrFinished
	}
	// 期间关联上来的新任务（结束任务的事务把另一轮的任务记到工作项上）不在刚才取消的请求里，提交后取消。
	if w.TaskID != nil && *w.TaskID != cancelledTaskID {
		t, err := ai.GetTask(ctx, tx, *w.TaskID, false)
		if err != nil {
			return 0, err
		}
		cancelReq = t.RequestID
	}
	moved, err := s.transition(ctx, tx, w, ev, reason, "")
	if err != nil || !moved {
		return 0, err
	}
	if err := store.Audit(ctx, tx, actor, "aidev."+string(ev), map[string]any{"work_item": id, "repo_id": w.RepoID, "reason": reason}); err != nil {
		return 0, err
	}
	return cancelReq, nil
}

// Retry 让受阻的工作项继续（PLAN #26.9、#31.3）：本轮执行过任务时再返修一轮（blocked → fixing，从本轮正式分支的 head、
// 本轮的 checkpoint 或本轮的起点继续）；部分完成后开始了新的轮次、还没有执行过任务时开始本轮的实现（blocked →
// developing，从最新的默认分支，round_base 也加一：本轮的返修从零算）。fix_rounds 加一（人的决定，不受轮数上限限制；也让返修、
// 实现任务的去重键不命中旧任务），执行者规划任务。
func (s *Service) Retry(ctx context.Context, id int64, actor string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		w, err := queryWorkItem(ctx, tx, `id=$1`+forUpdate, id)
		if err != nil {
			return err
		}
		if w == nil {
			return ErrNotFound
		}
		if w.State != StateBlocked {
			return fmt.Errorf("%w（当前是 %s）", ErrNotRetryable, w.State)
		}
		why, set := "人工重试：再返修一轮", `fix_rounds=fix_rounds+1, task_id=NULL`
		if !w.RoundRan {
			// 开始新的轮次：round_base 一起加一，本轮的返修从零算
			why, set = fmt.Sprintf("人工重试：开始第 %d 轮实现", w.ImplRound), set+`, round_base=round_base+1`
		}
		moved, err := s.transition(ctx, tx, w, EvRetry, why, set)
		if err != nil || !moved {
			return err
		}
		return store.Audit(ctx, tx, actor, "aidev.retry", map[string]any{"work_item": id, "repo_id": w.RepoID})
	})
	if err == nil {
		s.notify()
	}
	return err
}

// List 返回工作项（repoID 为 0 时是全部仓库），新的在前，最多 limit 个。
func List(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*WorkItem, error) {
	rows, err := q.Query(ctx, `SELECT `+workItemCols+` FROM dev_work_items WHERE ($1=0 OR repo_id=$1) ORDER BY id DESC LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	return scanWorkItems(rows)
}

package ai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 结束任务（PLAN #30.6 的任务状态转移表）：Complete、租约过期、超时都走 finishTask。只更新 outcome 为空的 attempt，结果至多
// 提交一次；任务类型的 Finish 给出最终结果（审计的结论、开发的交付）；之后汇总请求。

// ErrClassLost 是 Server 判定 Runner 失联（租约过期）时 attempt 的错误分类；这样的 attempt 已被取代，不再接受日志。
const ErrClassLost = "infra.lost"

// AI 任务的错误分类（ai_attempts.error_class）：审计与开发共用同一套名字，同一条规则只定义一次。
// 审计的任务类型写入，AI 开发的返修按它们判断（ai.findings 返修，其余不返修）。
const (
	// ErrClassReport 是任务没有提交有效的报告。
	ErrClassReport = "ai.report"
	// ErrClassFindings 是 AI 审计发现了问题。
	ErrClassFindings = "ai.findings"
	// ErrClassIncomplete 是 AI 审计没有完成。
	ErrClassIncomplete = "ai.incomplete"
)

// errClassTimeout 是 Server 判定超时时的错误分类（与 Runner 自己判定超时时上报的相同）。
const errClassTimeout = "timeout"

// lockTaskForFinish 按加锁顺序（并发组 → 请求 → 任务）锁住任务和它的请求。
func lockTaskForFinish(ctx context.Context, tx pgx.Tx, taskID int64) (*Request, *Task, error) {
	t, err := GetTask(ctx, tx, taskID, false)
	if err != nil {
		return nil, nil, err
	}
	r, err := lockRequest(ctx, tx, t.RequestID)
	if err != nil {
		return nil, nil, err
	}
	t, err = GetTask(ctx, tx, taskID, true)
	return r, t, err
}

// finishTask 结束执行中的任务的当前 attempt。调用方已经锁住请求和任务，并确认任务在执行中、res 属于当前的 attempt；
// 事务提交之后调用 attemptEnded（吊销这个 attempt 的 git 凭据）。
func (c *Core) finishTask(ctx context.Context, tx pgx.Tx, r *Request, t *Task, res Result) error {
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return err
	}
	if t.CurrentAttemptID == nil {
		return fmt.Errorf("AI 任务 #%d 在执行中，却没有当前的 attempt", t.ID)
	}
	a, err := GetAttempt(ctx, tx, *t.CurrentAttemptID)
	if err != nil {
		return err
	}
	fin, err := typ.Finish(ctx, tx, t, a, res)
	if err != nil {
		return err
	}
	res = keepSubmission(fin, res)
	var orca []byte
	if res.Orca != nil {
		b, err := json.Marshal(res.Orca)
		if err != nil {
			return err
		}
		orca = b
	}
	tag, err := tx.Exec(ctx, `UPDATE ai_attempts SET outcome=$2, error_class=nullif($3, ''), message=nullif($4, ''),
		agent_version=coalesce(nullif($5, ''), agent_version), agent_model=coalesce(nullif($6, ''), agent_model), finished_at=now(),
		result_digest=nullif($7, ''), orca=coalesce($8, orca)
		WHERE id=$1 AND outcome IS NULL`, a.ID, res.Outcome, res.ErrClass, res.Message, res.AgentVersion, res.AgentModel, res.Digest, orca)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // 已经提交过（并发的 Complete 与 sweeper），结果至多一次
	}
	state, retry := nextState(t, typ.Policy(), res)
	reason := resultReason(res)
	if retry {
		if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET state='pending', queued_at=now(), lease_expires_at=NULL, reason=$2 WHERE id=$1`,
			t.ID, "基础设施错误，重新排队："+reason); err != nil {
			return err
		}
		return c.enqueueTaskCheck(ctx, tx, typ, t.ID, t.RequestID)
	}
	if t.CancelRequested && state == TaskCancelled {
		reason = CancelReason(deref(t.CancelReason)).Text()
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET state=$2, reason=$3, finished_at=now(), lease_expires_at=NULL WHERE id=$1`,
		t.ID, state, reason); err != nil {
		return err
	}
	t.State = state
	if err := c.enqueueTaskCheck(ctx, tx, typ, t.ID, t.RequestID); err != nil {
		return err
	}
	return c.settle(ctx, tx, r)
}

// nextState 返回 attempt 结束后任务的状态，retry 为真表示按任务类型的规则重新排队（基础设施错误）。已请求取消时，
// 没有成功完成的都记为 cancelled；成功完成的保留结果（PLAN #30.6）。
func nextState(t *Task, pol Policy, res Result) (state TaskState, retry bool) {
	switch {
	case res.Outcome == store.OutcomeSucceeded:
		return TaskSucceeded, false
	case t.CancelRequested || res.Outcome == store.OutcomeCancelled:
		return TaskCancelled, false
	case res.Outcome == store.OutcomeTimedOut:
		return TaskTimedOut, false
	case res.Outcome == store.OutcomeInfraError && t.AttemptCount <= pol.InfraRetries:
		return TaskPending, true
	}
	return TaskFailed, false
}

// resultReason 是任务结束时的说明：结果说明，带上错误分类。
func resultReason(res Result) string {
	switch {
	case res.Message != "" && res.ErrClass != "":
		return fmt.Sprintf("%s（%s）", res.Message, res.ErrClass)
	case res.Message != "":
		return res.Message
	case res.ErrClass != "":
		return res.ErrClass
	}
	return string(res.Outcome)
}

// lost 结束租约过期（Runner 失联）的任务。调用方已经锁住请求和任务，并确认租约确实过期。
func (c *Core) lost(ctx context.Context, tx pgx.Tx, r *Request, t *Task) error {
	c.log.Warn("AI 任务的租约过期", "task", t.ID, "epoch", t.Epoch)
	return c.finishTask(ctx, tx, r, t, Result{Outcome: store.OutcomeInfraError, ErrClass: ErrClassLost, Message: "AI 执行节点失联（租约过期）"})
}

// FinishAttempt 在 Server 端提交 attemptID 的结果（与 airunner.v2 Complete 相同的规则：attempt 必须是任务执行中的当前 attempt，
// 否则返回 STALE_EPOCH；结果至多提交一次）。模块测试和不经 RPC 的调用方使用。
func (c *Core) FinishAttempt(ctx context.Context, attemptID int64, res Result) error {
	a, err := GetAttempt(ctx, c.db, attemptID)
	if err != nil {
		return err
	}
	err = c.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := LockCurrentAttempt(ctx, tx, a.TaskID, attemptID); err != nil {
			return err
		}
		r, t, err := lockTaskForFinish(ctx, tx, a.TaskID)
		if err != nil {
			return err
		}
		return c.finishTask(ctx, tx, r, t, res)
	})
	if err == nil {
		c.attemptEnded(ctx, attemptID)
		c.hub.All()
	}
	return err
}

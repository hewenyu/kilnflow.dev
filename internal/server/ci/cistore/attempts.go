package cistore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Phase 是 attempt 的执行阶段（attempts.phase，PLAN #32.6）。Native attempt 只能是 PhaseRunning（领取即开始执行，
// 数据库约束 attempts_native_running）；托管 attempt 在实际开始执行前依次经过 reserved、submitting、queued。
type Phase string

const (
	// PhaseReserved 是已预留、没有发出任何外部调用：Server 在本地判断它是否还活着（预留期限）。
	PhaseReserved Phase = "reserved"
	// PhaseSubmitting 是已写下派发意图，外部调用可能已经发生。
	PhaseSubmitting Phase = "submitting"
	// PhaseQueued 是外部已接受，还没有开始执行。
	PhaseQueued Phase = "queued"
	// PhaseRunning 是正在执行。
	PhaseRunning Phase = "running"
	// PhaseUnknown 是提交或观察的结果未知，等待对账（不会自动重新提交）。
	PhaseUnknown Phase = "unknown"
)

// AttemptExec 是 attempt 属于哪种资源、处于哪个阶段。
type AttemptExec struct {
	// ManagedPool 是托管池的 Key；Native attempt 为空。
	ManagedPool string
	Phase       Phase
}

// Managed 判断 attempt 属于托管池。
func (a AttemptExec) Managed() bool { return a.ManagedPool != "" }

// Submitted 判断托管 attempt 已经越过 reserved：外部调用可能已经发生，Server 不能在本地判定它已结束（PLAN #32.6）。
func (a AttemptExec) Submitted() bool { return a.Managed() && a.Phase != PhaseReserved }

// GetAttemptExec 读取 attempt 的资源和阶段；id 为 nil（job 还没有 attempt，例如发布 job）时返回 Native 的 running。
func GetAttemptExec(ctx context.Context, q store.Querier, id *int64) (AttemptExec, error) {
	if id == nil {
		return AttemptExec{Phase: PhaseRunning}, nil
	}
	var a AttemptExec
	err := q.QueryRow(ctx, `SELECT coalesce(managed_pool, ''), phase FROM attempts WHERE id=$1`, *id).Scan(&a.ManagedPool, &a.Phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptExec{Phase: PhaseRunning}, nil
	}
	if err != nil {
		return a, fmt.Errorf("读取 attempt %d：%w", *id, err)
	}
	return a, nil
}

// ManagedPoolLabel 是托管池在页面和原因中的写法（例如"托管池 gcp-a"）。
const ManagedPoolLabel = "托管池 "

// AttemptExecutorSQL 是 attempt 的执行者名称的 SQL 表达式（attempts a LEFT JOIN runners r ON r.id=a.runner_id）：
// 托管 attempt 是"托管池 <Key>"，Native 是 Runner 的名称（删除后记在 runner_name 中）。
const AttemptExecutorSQL = `CASE WHEN a.managed_pool IS NOT NULL THEN '` + ManagedPoolLabel + `' || a.managed_pool ELSE coalesce(r.name, a.runner_name, '') END`

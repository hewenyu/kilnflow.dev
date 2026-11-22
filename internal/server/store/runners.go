package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runner 的查询（共用的身份部分）。CI Runner 的平台、容量等在 ci_runners（cistore.ListCIRunners），AI 执行节点的角色、
// Agent 等在 ai_runners（ai.ListRunners）。在线、暂停接单等显示状态由 admin 推出。

// Runner 是 runners 表中的一台 Runner（共用的身份字段）。
type Runner struct {
	ID   int64
	Name string
	// Kind 是 Runner 的类型（RunnerKindCI 或 RunnerKindAI），注册后不变。
	Kind    string
	Version string
	// State 是 runners.state（RunnerActive 或 RunnerRevoked）。
	State string
	// Drain 表示已暂停接单。
	Drain bool
	// LastSeen 是最后一次联系 Server 的时间（从没有联系过时为 nil）。
	LastSeen *time.Time
}

// Revoked 表示 Runner 已被吊销。
func (r *Runner) Revoked() bool { return r.State == RunnerRevoked }

// RunnerFilter 是 ListRunners 的条件；零值表示全部。
type RunnerFilter struct {
	ID   int64
	Name string
	// Kind 不为空时只列出这一类。
	Kind string
}

// RunnerCols 是 Runner 从 runners 表（别名 r）读取的列，顺序与 Runner.ScanDest 一致。
const RunnerCols = `r.id, r.name, r.kind, r.version, r.state, r.drain, r.last_seen_at`

// ScanDest 返回 RunnerCols 对应的扫描目标。
func (r *Runner) ScanDest() []any {
	return []any{&r.ID, &r.Name, &r.Kind, &r.Version, &r.State, &r.Drain, &r.LastSeen}
}

// RunnerWhere 是 RunnerFilter 的条件（参数依次为 ID、名称、类型）。
const RunnerWhere = `($1::bigint=0 OR r.id=$1) AND ($2::text='' OR r.name=$2) AND ($3::text='' OR r.kind=$3)`

// Args 返回 RunnerWhere 的参数。
func (f RunnerFilter) Args() []any { return []any{f.ID, f.Name, f.Kind} }

// ListRunners 返回满足条件的 Runner，按名称排序。
func ListRunners(ctx context.Context, q Querier, f RunnerFilter) ([]*Runner, error) {
	rows, err := q.Query(ctx, `SELECT `+RunnerCols+` FROM runners r WHERE `+RunnerWhere+` ORDER BY r.name`, f.Args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Runner, error) {
		var r Runner
		err := row.Scan(r.ScanDest()...)
		return &r, err
	})
}

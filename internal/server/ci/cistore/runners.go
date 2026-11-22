package cistore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// CIRunner 是一台 CI Runner：共用的身份和 ci_runners 中的平台、容量、缓存清单。
type CIRunner struct {
	store.Runner
	Pool                 string
	Platforms, Executors []string
	HostFingerprint      map[string]string
	// Capacity 是上报的规格与容量（PLAN #8.6）；旧版 Runner 没有上报时为 nil。
	Capacity *store.RunnerCapacity
	// CacheReport 是最近一次上报的缓存清单（kiln.runner.v1.CacheReport 的 protojson），没有时为 nil。
	CacheReport     []byte
	CacheReportedAt *time.Time
}

// ListCIRunners 返回满足条件的 CI Runner，按名称排序（f.Kind 被忽略）。
func ListCIRunners(ctx context.Context, q store.Querier, f store.RunnerFilter) ([]*CIRunner, error) {
	f.Kind = store.RunnerKindCI
	rows, err := q.Query(ctx, `SELECT `+store.RunnerCols+`, c.pool, c.platforms, c.executors, c.capabilities->'host_fingerprint',
		c.capacity, c.cache_report, c.cache_reported_at
		FROM runners r JOIN ci_runners c ON c.runner_id=r.id WHERE `+store.RunnerWhere+` ORDER BY r.name`, f.Args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*CIRunner, error) {
		var r CIRunner
		var capRaw []byte
		err := row.Scan(append(r.ScanDest(), &r.Pool, &r.Platforms, &r.Executors, &r.HostFingerprint, &capRaw, &r.CacheReport, &r.CacheReportedAt)...)
		if err == nil && len(capRaw) > 0 {
			var c store.RunnerCapacity
			if json.Unmarshal(capRaw, &c) == nil {
				r.Capacity = &c
			}
		}
		return &r, err
	})
}

// RunnerAllocations 返回 Runner（runnerIDs）上正在执行的 job 声明的资源之和（调度的账本，PLAN #14.1）。
func RunnerAllocations(ctx context.Context, q store.Querier, runnerIDs []int64) (map[int64]store.Resources, error) {
	rows, err := q.Query(ctx, `SELECT a.runner_id, coalesce(sum((j.spec->>'cpu')::float8), 0), coalesce(sum((j.spec->>'memory_bytes')::bigint), 0)
		FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id WHERE j.state='running' AND a.runner_id = ANY($1) GROUP BY 1`, runnerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]store.Resources{}
	for rows.Next() {
		var id int64
		var r store.Resources
		if err := rows.Scan(&id, &r.CPU, &r.Memory); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// RunningJob 是一台 Runner 正在执行的一个 job。
type RunningJob struct {
	RunnerID int64
	JobID    int64
	Name     string
	RunID    int64
	// Workflow 是 workflow 的名称（没有时为文件名）。
	Workflow string
	Repo     string
	Started  time.Time
}

// RunningJobs 返回 Runner（runnerIDs）正在执行的 job：job 正在执行，当前尝试属于这台 Runner。按开始时间排序。
func RunningJobs(ctx context.Context, q store.Querier, runnerIDs []int64) ([]RunningJob, error) {
	rows, err := q.Query(ctx, `SELECT a.runner_id, j.id, j.display_name, j.run_id,
		CASE WHEN ru.workflow_name <> '' THEN ru.workflow_name ELSE ru.workflow END, rp.owner || '/' || rp.name, coalesce(j.started_at, a.started_at)
		FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id JOIN runs ru ON ru.id=j.run_id JOIN repos rp ON rp.id=ru.repo_id
		WHERE j.state='running' AND a.runner_id = ANY($1) ORDER BY 7, j.id`, runnerIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RunningJob])
}

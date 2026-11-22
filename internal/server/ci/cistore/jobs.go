package cistore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Job 是 jobs 表中的一个 job（包括发布 job）。
type Job struct {
	ID               int64
	Kind             workflow.JobKind
	RunID            int64
	Rerun            int
	JobKey           string
	JobName          string
	DisplayName      string
	Matrix           map[string]any
	Spec             workflow.Spec
	Platform         string
	Executor         workflow.ExecutorKind
	Needs            []int64
	ActionKey        *string
	InputDigest      *string
	Environment      *string
	State            store.JobState
	Reason           *string
	Epoch            int64
	AttemptCount     int
	Priority         int
	CurrentAttemptID *int64
	CancelRequested  bool
	CancelReason     *string
	ResultManifest   *string
	CachedFrom       *int64
	ReusedFrom       *int64
	Follows          *int64
	Effects          bool
	ForceExecute     bool
	KeepOnFailure    bool
	CheckRunID       *int64
	StartedStep      bool
	LeaseExpiresAt   *time.Time
	QueuedAt         *time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
	CreatedAt        time.Time
	// WaitReason 是调度器最近一次跳过这个排队中的 job 的原因（jobs.last_skip_reason，领取时清空），
	// 例如资源不够时需要多少、各 Runner 能提供多少（PLAN #14.1）。
	WaitReason *string
}

// Reusable 表示按"重跑未成功的 job"重跑时这个 job 沿用结果、不重新规划：成功且有结果，或成功的发布 job。
// purged 表示 run 的过程数据已经按保留策略清理（Run.PurgedAt）：产物已经不在了，只有发布 job 可以沿用。
// planner 的重跑规划和 Dashboard 的重跑说明都用它判断。
func (j *Job) Reusable(purged bool) bool {
	if j.State != store.JobSucceeded {
		return false
	}
	return j.Kind == workflow.JobKindPublish || (j.ResultManifest != nil && !purged)
}

// LogsPerAttempt 判断 kind 的 job 的日志是否按 attempt 保存：发布 job 由 Server 执行、没有 attempt，日志按 job 保存
// （logstore.PublishLogID），其他 job 每个 attempt 一份日志。
func LogsPerAttempt(kind workflow.JobKind) bool { return kind != workflow.JobKindPublish }

// JobLogID 返回 job 当前的日志在 logstore 中的编号：发布 job 是 logstore.PublishLogID(jobID)；其他 job 是当前 attempt，
// 还没有 attempt 时为 0（没有日志）。读取 job 日志的地方（管理接口、Check Run 摘要、AI 的验证查询）都用它，不各自判断。
func JobLogID(kind workflow.JobKind, jobID int64, currentAttempt *int64) int64 {
	switch {
	case !LogsPerAttempt(kind):
		return logstore.PublishLogID(jobID)
	case currentAttempt == nil:
		return 0
	}
	return *currentAttempt
}

// LogID 返回 job 当前的日志编号（JobLogID）。
func (j *Job) LogID() int64 { return JobLogID(j.Kind, j.ID, j.CurrentAttemptID) }

const jobCols = `id, kind, run_id, rerun, job_key, job_name, display_name, matrix, spec, platform, executor_kind, needs,
	action_key, input_digest, environment, state, reason, epoch, attempt_count, priority, current_attempt_id,
	cancel_requested, cancel_reason, result_manifest, cached_from_job_id, reused_from_job_id, follows_job_id, effects,
	force_execute, keep_on_failure, check_run_id, started_step, lease_expires_at,
	queued_at, started_at, finished_at, created_at, last_skip_reason`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var matrix, spec []byte
	var runID *int64
	err := row.Scan(&j.ID, &j.Kind, &runID, &j.Rerun, &j.JobKey, &j.JobName, &j.DisplayName, &matrix, &spec, &j.Platform, &j.Executor, &j.Needs,
		&j.ActionKey, &j.InputDigest, &j.Environment, &j.State, &j.Reason, &j.Epoch, &j.AttemptCount, &j.Priority, &j.CurrentAttemptID,
		&j.CancelRequested, &j.CancelReason, &j.ResultManifest, &j.CachedFrom, &j.ReusedFrom, &j.Follows, &j.Effects,
		&j.ForceExecute, &j.KeepOnFailure, &j.CheckRunID, &j.StartedStep, &j.LeaseExpiresAt,
		&j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.CreatedAt, &j.WaitReason)
	if err != nil {
		return nil, err
	}
	if runID != nil {
		j.RunID = *runID
	}
	if len(matrix) > 0 {
		if err := json.Unmarshal(matrix, &j.Matrix); err != nil {
			return nil, fmt.Errorf("job %d 的 matrix 无法解析：%w", j.ID, err)
		}
	}
	if err := json.Unmarshal(spec, &j.Spec); err != nil {
		return nil, fmt.Errorf("job %d 的 spec 无法解析：%w", j.ID, err)
	}
	return &j, nil
}

// GetJob 按 ID 读取 job；lock 为 true 时加行锁（FOR UPDATE）。不存在时返回的错误包装了 pgx.ErrNoRows。
func GetJob(ctx context.Context, q store.Querier, id int64, lock bool) (*Job, error) {
	sql := `SELECT ` + jobCols + ` FROM jobs WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	j, err := scanJob(q.QueryRow(ctx, sql, id))
	if err != nil {
		return nil, fmt.Errorf("读取 job %d：%w", id, err)
	}
	return j, nil
}

// QueryJobs 读取满足 where 条件的 job（where 是 SQL 片段，参数用 $n 引用 args）。
// 条件中的状态（如 state='pending'）直接写成字面量而不是参数：jobs 表的部分索引依赖字面量才能命中。
func QueryJobs(ctx context.Context, q store.Querier, where string, args ...any) ([]*Job, error) {
	rows, err := q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Job, error) { return scanJob(row) })
}

// RerunJobs 返回 run 某一次运行（rerun）的全部 job，按 ID 排序；lock 为 true 时按 ID 顺序加行锁（FOR UPDATE）。
func RerunJobs(ctx context.Context, q store.Querier, runID int64, rerun int, lock bool) ([]*Job, error) {
	where := `run_id=$1 AND rerun=$2 ORDER BY id`
	if lock {
		where += ` FOR UPDATE`
	}
	return QueryJobs(ctx, q, where, runID, rerun)
}

// JobsNamed 返回 run 某一次运行中名为 name 的 job（矩阵的全部变体），按 ID 排序。
func JobsNamed(ctx context.Context, q store.Querier, runID int64, rerun int, name string) ([]*Job, error) {
	return QueryJobs(ctx, q, `run_id=$1 AND rerun=$2 AND job_name=$3 ORDER BY id`, runID, rerun, name)
}

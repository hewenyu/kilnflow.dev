// Package ai 是 AI 领域的核心（PLAN #30）：AI 任务的三层模型（请求 → 任务 → attempt）、调度、生命周期、airunner.v2 服务、
// sweeper、AI 执行节点的状态和 Check Run 投影。审计、开发等任务类型实现 TaskType，在组装时注册（Registry）；
// 生命周期代码不认识具体的类型。
//
// 与 CI 的关系：不依赖 CI 的任何包、不访问 runs / jobs / attempts / ci_runners（依赖测试和表归属测试检查）。
// 跨领域的通知只经 outbox 事件（Events）和组装层注入的接口（例如 Verifications）。
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// RequestState 是请求的状态（ai_requests.state）。
type RequestState string

const (
	// RequestQueued：并发组中有执行中的请求，排队等待（返修）。
	RequestQueued    RequestState = "queued"
	RequestActive    RequestState = "active"
	RequestSucceeded RequestState = "succeeded"
	RequestFailed    RequestState = "failed"
	RequestCancelled RequestState = "cancelled"
	// RequestSkipped：全部任务跳过（不满足合并条件）。
	RequestSkipped RequestState = "skipped"
)

// Terminal 判断是否为终态。
func (s RequestState) Terminal() bool {
	switch s {
	case RequestSucceeded, RequestFailed, RequestCancelled, RequestSkipped:
		return true
	}
	return false
}

// Conclusion 返回终态对应的 Check Run 结论。
func (s RequestState) Conclusion() store.Conclusion {
	switch s {
	case RequestSucceeded:
		return store.ConclusionSuccess
	case RequestFailed:
		return store.ConclusionFailure
	case RequestCancelled:
		return store.ConclusionCancelled
	case RequestSkipped:
		return store.ConclusionSkipped
	}
	return ""
}

// TaskState 是任务的状态（ai_tasks.state）。
type TaskState string

const (
	// TaskWaiting：请求在并发组中排队，任务还不能领取。
	TaskWaiting   TaskState = "waiting"
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskTimedOut  TaskState = "timed_out"
	TaskCancelled TaskState = "cancelled"
	TaskSkipped   TaskState = "skipped"
)

// terminalTaskStates 是任务的终态。
func terminalTaskStates() []TaskState {
	return []TaskState{TaskSucceeded, TaskFailed, TaskTimedOut, TaskCancelled, TaskSkipped}
}

// terminalTaskArgs 是终态的 []string 形式（SQL 参数 state = ANY($n)）。
func terminalTaskArgs() []string {
	out := make([]string, 0, len(terminalTaskStates()))
	for _, s := range terminalTaskStates() {
		out = append(out, string(s))
	}
	return out
}

// Terminal 判断是否为终态。
func (s TaskState) Terminal() bool { return slices.Contains(terminalTaskStates(), s) }

// Failed 判断是否为失败类终态。
func (s TaskState) Failed() bool { return s == TaskFailed || s == TaskTimedOut }

// Queued 判断任务是否还没有开始执行。
func (s TaskState) Queued() bool { return s == TaskWaiting || s == TaskPending }

// Title 是 Check Run 标题中的中文状态名。
func (s TaskState) Title() string {
	switch s {
	case TaskWaiting:
		return "等待并发组"
	case TaskPending:
		return "排队中"
	case TaskRunning:
		return "执行中"
	case TaskSucceeded:
		return "成功"
	case TaskFailed:
		return "失败"
	case TaskTimedOut:
		return "超时"
	case TaskCancelled:
		return "已取消"
	case TaskSkipped:
		return "已跳过"
	}
	return string(s)
}

// Conclusion 返回终态对应的 Check Run 结论。
func (s TaskState) Conclusion() store.Conclusion {
	switch s {
	case TaskSucceeded:
		return store.ConclusionSuccess
	case TaskFailed:
		return store.ConclusionFailure
	case TaskTimedOut:
		return store.ConclusionTimedOut
	case TaskCancelled:
		return store.ConclusionCancelled
	case TaskSkipped:
		return store.ConclusionSkipped
	}
	return ""
}

// Request 是 ai_requests 中的一个请求。
type Request struct {
	ID          int64
	Type        string
	RepoID      int64
	Event       string
	Ref         string
	SHA         string
	Trigger     string
	Source      string
	Sender      string
	DedupeKey   *string
	FamilyID    int64
	DeliveryID  *string
	GroupKey    *string
	RerunOf     *int64
	State       RequestState
	ConfigError *string
	Warnings    *string
	CheckRunID  *int64
	CreatedAt   time.Time
	FinishedAt  *time.Time
	PurgedAt    *time.Time
}

const requestCols = `id, type, repo_id, event, ref, sha, trigger, source, sender, dedupe_key, family_id,
	delivery_id, group_key, rerun_of, state, config_error, warnings, check_run_id, created_at, finished_at, purged_at`

func scanRequest(row pgx.Row) (*Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.Type, &r.RepoID, &r.Event, &r.Ref, &r.SHA, &r.Trigger, &r.Source, &r.Sender,
		&r.DedupeKey, &r.FamilyID, &r.DeliveryID, &r.GroupKey, &r.RerunOf, &r.State, &r.ConfigError, &r.Warnings, &r.CheckRunID,
		&r.CreatedAt, &r.FinishedAt, &r.PurgedAt)
	return &r, err
}

// ErrNotFound 表示请求、任务或 attempt 不存在。
var ErrNotFound = errors.New("AI 请求或任务不存在")

// ErrTaskSpec 表示任务记录中的 spec 无法解析（记录损坏）：重试不会成功，按永久性错误处理。
var ErrTaskSpec = errors.New("AI 任务的 spec 无法解析")

// GetRequest 读取请求；lock 为真时加行锁（FOR UPDATE）。
func GetRequest(ctx context.Context, q store.Querier, id int64, lock bool) (*Request, error) {
	sql := `SELECT ` + requestCols + ` FROM ai_requests WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanRequest(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("AI 请求 #%d：%w", id, ErrNotFound)
	}
	return r, err
}

// QueryRequests 按条件（WHERE 之后的部分）读取请求。
func QueryRequests(ctx context.Context, q store.Querier, where string, args ...any) ([]*Request, error) {
	rows, err := q.Query(ctx, `SELECT `+requestCols+` FROM ai_requests WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Request, error) { return scanRequest(row) })
}

// Task 是 ai_tasks 中的一个任务。
type Task struct {
	ID        int64
	RequestID int64
	Key       string
	Type      string
	// Platforms 是可以执行这个任务的平台（Runner 平台，例如 darwin/arm64、linux/amd64；至少一个）：节点上报的平台在其中即可
	// 领取（PLAN #30.7）。实际执行的平台记在 attempt 上（Attempt.Platform）。
	Platforms   []string
	Agent       string
	DisplayName string
	// Spec 是冻结的执行输入（任务类型的内容，JSON）。
	Spec             json.RawMessage
	State            TaskState
	Reason           *string
	LastSkipReason   *string
	Priority         int
	TimeoutMinutes   int
	Epoch            int64
	AttemptCount     int
	CurrentAttemptID *int64
	LeaseExpiresAt   *time.Time
	CancelRequested  bool
	CancelReason     *string
	ReusedFrom       *int64
	CheckRunID       *int64
	CreatedAt        time.Time
	QueuedAt         *time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// PlatformsText 返回可以执行的平台的说明（例如 darwin/arm64 或 linux/amd64），用于等待原因、Check Run 等给人看的地方。
func PlatformsText(platforms []string) string { return strings.Join(platforms, " 或 ") }

// DecodeSpec 把任务的执行输入解析到 v。
func (t *Task) DecodeSpec(v any) error {
	if err := json.Unmarshal(t.Spec, v); err != nil {
		return fmt.Errorf("%w：AI 任务 #%d 的内容无法解析：%w", ErrTaskSpec, t.ID, err)
	}
	return nil
}

const taskCols = `id, request_id, key, type, platforms, agent, display_name, spec, state, reason, last_skip_reason, priority,
	timeout_minutes, epoch, attempt_count, current_attempt_id, lease_expires_at, cancel_requested, cancel_reason, reused_from, check_run_id,
	created_at, queued_at, started_at, finished_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.RequestID, &t.Key, &t.Type, &t.Platforms, &t.Agent, &t.DisplayName, &t.Spec, &t.State, &t.Reason,
		&t.LastSkipReason, &t.Priority, &t.TimeoutMinutes, &t.Epoch, &t.AttemptCount, &t.CurrentAttemptID, &t.LeaseExpiresAt,
		&t.CancelRequested, &t.CancelReason, &t.ReusedFrom, &t.CheckRunID, &t.CreatedAt, &t.QueuedAt, &t.StartedAt, &t.FinishedAt)
	return &t, err
}

// GetTask 读取任务；lock 为真时加行锁。
func GetTask(ctx context.Context, q store.Querier, id int64, lock bool) (*Task, error) {
	sql := `SELECT ` + taskCols + ` FROM ai_tasks WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	t, err := scanTask(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("AI 任务 #%d：%w", id, ErrNotFound)
	}
	return t, err
}

// QueryTasks 按条件（WHERE 之后的部分）读取任务。
func QueryTasks(ctx context.Context, q store.Querier, where string, args ...any) ([]*Task, error) {
	rows, err := q.Query(ctx, `SELECT `+taskCols+` FROM ai_tasks WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Task, error) { return scanTask(row) })
}

// RequestTasks 返回请求的任务（按 id）；lock 为真时按 id 升序加行锁（加锁顺序：请求 → 任务，PLAN #30.6）。
func RequestTasks(ctx context.Context, q store.Querier, requestID int64, lock bool) ([]*Task, error) {
	where := `request_id=$1 ORDER BY id`
	if lock {
		where += ` FOR UPDATE`
	}
	return QueryTasks(ctx, q, where, requestID)
}

// OrcaBinding 是 attempt 在 Orca 中的执行关联（ai_attempts.orca，PLAN #33.4）：用于恢复、取消和从 Dashboard 定位。
// 连接器在心跳和 Complete 中上报，Server 只保存、不改写。
type OrcaBinding struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	// StartRequestID 是启动请求的幂等键（连接器预先生成的 UUID）。
	StartRequestID string `json:"start_request_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	DispatchID     string `json:"dispatch_id,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeName   string `json:"worktree_name,omitempty"`
}

// AttemptProgress 是 AI attempt 的进度（ai_attempts.progress）：连接器心跳上报的阶段和说明（PLAN #33.6）。
type AttemptProgress struct {
	// Stage 是 prepare | start | running | finishing。
	Stage string `json:"stage,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Attempt 是 ai_attempts 中的一次领取。
type Attempt struct {
	ID         int64
	TaskID     int64
	Epoch      int64
	RunnerID   *int64
	RunnerName *string
	HostID     string
	// Platform 是领取时节点上报的平台（实际执行的平台）。
	Platform         string
	AcquireRequestID string
	Outcome          *store.Outcome
	ErrorClass       *string
	Message          *string
	Progress         []byte
	// Orca 是执行在 Orca 中的关联（jsonb）；ResultDigest 是提交的结果摘要（同一个 attempt 用相同内容重试时返回 replayed，
	// PLAN #33.6）。
	Orca          []byte
	ResultDigest  *string
	LogSize       int64
	RunnerVersion string
	AgentVersion  string
	AgentModel    string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

const attemptCols = `id, task_id, epoch, runner_id, runner_name, host_id, platform, acquire_request_id, outcome, error_class, message, progress,
	log_size, runner_version, agent_version, agent_model, started_at, finished_at, orca, result_digest`

func scanAttempt(row pgx.Row) (*Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.TaskID, &a.Epoch, &a.RunnerID, &a.RunnerName, &a.HostID, &a.Platform, &a.AcquireRequestID, &a.Outcome, &a.ErrorClass,
		&a.Message, &a.Progress, &a.LogSize, &a.RunnerVersion, &a.AgentVersion, &a.AgentModel, &a.StartedAt, &a.FinishedAt, &a.Orca, &a.ResultDigest)
	return &a, err
}

// DecodeOrca 解析保存的 Orca 关联（没有上报时是零值）。
func (a *Attempt) DecodeOrca() (OrcaBinding, error) {
	var b OrcaBinding
	if len(a.Orca) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(a.Orca, &b); err != nil {
		return b, fmt.Errorf("AI attempt #%d 的 Orca 关联无法解析：%w", a.ID, err)
	}
	return b, nil
}

// DecodeProgress 解析保存的进度（没有上报时是零值）。
func (a *Attempt) DecodeProgress() (AttemptProgress, error) {
	var p AttemptProgress
	if len(a.Progress) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(a.Progress, &p); err != nil {
		return p, fmt.Errorf("AI attempt #%d 的进度无法解析：%w", a.ID, err)
	}
	return p, nil
}

// GetAttempt 读取 attempt。
func GetAttempt(ctx context.Context, q store.Querier, id int64) (*Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx, `SELECT `+attemptCols+` FROM ai_attempts WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("AI attempt #%d：%w", id, ErrNotFound)
	}
	return a, err
}

// TaskAttempts 返回任务的全部 attempt（按 epoch）。
func TaskAttempts(ctx context.Context, q store.Querier, taskID int64) ([]*Attempt, error) {
	rows, err := q.Query(ctx, `SELECT `+attemptCols+` FROM ai_attempts WHERE task_id=$1 ORDER BY epoch`, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Attempt, error) { return scanAttempt(row) })
}

// groupLockKey 是并发组 advisory lock 的键（PLAN #30.6，与 CI 的并发组、配额的键不会重复）。
func groupLockKey(repoID int64, groupKey string) string {
	return fmt.Sprintf("kiln:ai-group:%d:%s", repoID, groupKey)
}

// lockGroup 锁住请求所在的并发组（没有并发组时什么也不做）。
func lockGroup(ctx context.Context, tx pgx.Tx, repoID int64, groupKey *string) error {
	if groupKey == nil || *groupKey == "" {
		return nil
	}
	return store.LockXact(ctx, tx, groupLockKey(repoID, *groupKey))
}

// lockRequest 按加锁顺序（并发组 → 请求）锁住请求，返回加锁后读到的请求。
func lockRequest(ctx context.Context, tx pgx.Tx, id int64) (*Request, error) {
	r, err := GetRequest(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	if err := lockGroup(ctx, tx, r.RepoID, r.GroupKey); err != nil {
		return nil, err
	}
	return GetRequest(ctx, tx, id, true)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

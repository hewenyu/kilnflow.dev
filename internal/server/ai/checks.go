package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/checkrun"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// AI 的 Check Run（PLAN #30.5）：名称与拆分前相同（ai-review (<event>)、ai-review / <显示名> (<event>)）；external_id 新建的为
// ai-request:<id>、ai-task:<id>，迁移自 run、job 的请求和任务继续使用 run:<id>:<rerun>、job:<id>，GitHub 上已经有的 Check Run
// 照常更新。outbox 记录只保存"要同步哪个对象"，投递时读取最新状态。

// outbox 记录的类型（由 Core.RegisterOutbox 注册投递函数）。
const (
	KindTaskCheck    = "ai.checkrun.task"
	KindRequestCheck = "ai.checkrun.request"
)

type taskCheckPayload struct {
	TaskID int64 `json:"task_id"`
}

type requestCheckPayload struct {
	RequestID int64 `json:"request_id"`
}

// external_id 的前缀。
const (
	externalIDTask    = "ai-task:"
	externalIDRequest = "ai-request:"
)

// maxTaskNoteBytes 是请求汇总表格中每个任务说明的上限。
const maxTaskNoteBytes = 120

// enqueueTaskCheck 写入同步任务 Check Run 的意图（类型声明需要 Check Run 时），同时同步所属请求 requestID 的汇总 Check Run：
// 汇总中的状态和任务表随任务变化（领取后为执行中，部分平台结束时更新表格），领取路径不写请求行也能更新。
func (c *Core) enqueueTaskCheck(ctx context.Context, tx pgx.Tx, typ TaskType, taskID, requestID int64) error {
	if !typ.Policy().CheckRuns {
		return nil
	}
	if err := c.outbox.Enqueue(ctx, tx, fmt.Sprintf("ai-checkrun:task:%d", taskID), KindTaskCheck, taskCheckPayload{TaskID: taskID}); err != nil {
		return err
	}
	return c.enqueueRequestCheckID(ctx, tx, requestID)
}

// enqueueRequestCheck 写入同步请求汇总 Check Run 的意图（类型声明需要 Check Run 时）。
func (c *Core) enqueueRequestCheck(ctx context.Context, tx pgx.Tx, typ TaskType, r *Request) error {
	if !typ.Policy().CheckRuns {
		return nil
	}
	return c.enqueueRequestCheckID(ctx, tx, r.ID)
}

func (c *Core) enqueueRequestCheckID(ctx context.Context, tx pgx.Tx, requestID int64) error {
	return c.outbox.Enqueue(ctx, tx, fmt.Sprintf("ai-checkrun:request:%d", requestID), KindRequestCheck, requestCheckPayload{RequestID: requestID})
}

// DeliverTaskCheck 投递 KindTaskCheck 记录。
func (c *Core) DeliverTaskCheck(ctx context.Context, payload []byte) error {
	if c.gh == nil {
		return nil
	}
	var p taskCheckPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("outbox 记录的 payload 无法解析：%w", err)
	}
	return c.syncTaskCheck(ctx, p.TaskID)
}

// DeliverRequestCheck 投递 KindRequestCheck 记录。
func (c *Core) DeliverRequestCheck(ctx context.Context, payload []byte) error {
	if c.gh == nil {
		return nil
	}
	var p requestCheckPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("outbox 记录的 payload 无法解析：%w", err)
	}
	return c.syncRequestCheck(ctx, p.RequestID)
}

// TaskExternalID 返回任务的 Check Run 的 external_id。
func TaskExternalID(t *Task) string {
	return externalIDTask + strconv.FormatInt(t.ID, 10)
}

// RequestExternalID 返回请求汇总 Check Run 的 external_id。
func RequestExternalID(r *Request) string {
	return externalIDRequest + strconv.FormatInt(r.ID, 10)
}

// CheckTarget 是 Check Run 的 external_id 指向的 AI 对象：TaskID 非 0 时是任务，否则是请求。
type CheckTarget struct {
	TaskID    int64
	RequestID int64
}

// ErrUnknownExternalID 表示 external_id 不指向 AI 的对象。
var ErrUnknownExternalID = errors.New("不是 AI 的 external_id")

// ErrInvalidExternalID 表示 external_id 是 AI 的格式，但内容无效（重试也不会好）；其他错误（数据库等）可以重试。
var ErrInvalidExternalID = errors.New("AI 的 external_id 无效")

// ResolveExternalID 解析 Check Run 的 external_id（GitHub 上的 Re-run）。不是 AI 的格式（例如 CI 的 job:<id>）时返回
// ErrUnknownExternalID；是 AI 的格式但 ID 无效时返回 ErrInvalidExternalID。
func ResolveExternalID(id string) (CheckTarget, error) {
	if rest, ok := strings.CutPrefix(id, externalIDTask); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return CheckTarget{}, fmt.Errorf("external_id %q 中的任务 ID：%w", id, ErrInvalidExternalID)
		}
		return CheckTarget{TaskID: n}, nil
	}
	if rest, ok := strings.CutPrefix(id, externalIDRequest); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return CheckTarget{}, fmt.Errorf("external_id %q 中的请求 ID：%w", id, ErrInvalidExternalID)
		}
		return CheckTarget{RequestID: n}, nil
	}
	return CheckTarget{}, ErrUnknownExternalID
}

// checkName 返回 Check Run 的名称：前缀是任务类型的 Check Run 名称（例如 ai-review）。
func checkName(prefix string, r *Request, display string) string {
	if display == "" {
		return fmt.Sprintf("%s (%s)", prefix, r.Event)
	}
	return fmt.Sprintf("%s / %s (%s)", prefix, display, r.Event)
}

// CheckPrefix 返回任务类型的 Check Run 名称前缀（ai-<类型>，例如 ai-review）。
func CheckPrefix(typ string) string { return "ai-" + typ }

// RequestURL 返回 Dashboard 中请求的地址（TaskURL 是任务的）。
func (c *Core) RequestURL(id int64) string {
	return fmt.Sprintf("%s/ai/requests/%d", c.cfg.PublicURL, id)
}

// TaskURL 返回 Dashboard 中任务的地址。
func (c *Core) TaskURL(id int64) string { return fmt.Sprintf("%s/ai/tasks/%d", c.cfg.PublicURL, id) }

func (c *Core) target(ctx context.Context, r *Request) (checkrun.Target, error) {
	repo, err := store.GetRepo(ctx, c.db, r.RepoID)
	if err != nil {
		return checkrun.Target{}, err
	}
	return checkrun.Target{Inst: c.gh.Inst(repo.InstallationID), Repo: repo.FullName(), SHA: r.SHA, AppID: c.cfg.AppID}, nil
}

func (c *Core) syncTaskCheck(ctx context.Context, taskID int64) error {
	t, err := GetTask(ctx, c.db, taskID, false)
	if err != nil {
		return err
	}
	r, err := GetRequest(ctx, c.db, t.RequestID, false)
	if err != nil {
		return err
	}
	tg, err := c.target(ctx, r)
	if err != nil {
		return err
	}
	cr := &gh.CheckRun{
		Name:       checkName(CheckPrefix(t.Type), r, t.DisplayName),
		HeadSHA:    r.SHA,
		ExternalID: TaskExternalID(t),
		DetailsURL: c.TaskURL(t.ID),
	}
	var b strings.Builder
	if reason := deref(t.Reason); reason != "" {
		b.WriteString(reason + "\n\n")
	}
	where, err := c.taskWhere(ctx, t)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "- 任务：`%s`\n- 平台：%s，Agent：`%s`\n", t.Key, where, t.Agent)
	if t.State.Failed() && t.CurrentAttemptID != nil {
		b.WriteString(checkrun.LogTail(c.logs.Tail(*t.CurrentAttemptID, checkrun.FailedLogTailLines)))
	}
	cr.Output = &gh.CheckRunOutput{Title: t.State.Title(), Summary: shorten.Text(b.String(), checkrun.MaxSummaryBytes)}
	switch {
	case t.State.Queued():
		cr.Status = gh.CheckStatusQueued
	case t.State == TaskRunning:
		cr.Status = gh.CheckStatusInProgress
		cr.StartedAt = t.StartedAt
	default:
		checkrun.SetCompleted(cr, string(t.State.Conclusion()), t.FinishedAt)
		cr.StartedAt = t.StartedAt
	}
	return checkrun.Upsert(ctx, tg, cr, t.CheckRunID, func(id int64) error {
		_, err := c.db.Exec(ctx, `UPDATE ai_tasks SET check_run_id=$2 WHERE id=$1`, t.ID, id)
		return err
	})
}

// taskWhere 返回 Check Run 中任务的平台：已经领取的是实际执行的平台和节点（一组平台中由哪台领到），还没有领取的是可以
// 执行的平台。
func (c *Core) taskWhere(ctx context.Context, t *Task) (string, error) {
	if t.CurrentAttemptID != nil {
		a, err := GetAttempt(ctx, c.db, *t.CurrentAttemptID)
		if err != nil {
			return "", err
		}
		if a.Platform != "" {
			return fmt.Sprintf("`%s`（节点 %s）", a.Platform, deref(a.RunnerName)), nil
		}
	}
	quoted := make([]string, 0, len(t.Platforms))
	for _, p := range t.Platforms {
		quoted = append(quoted, "`"+p+"`")
	}
	return PlatformsText(quoted), nil
}

func (c *Core) syncRequestCheck(ctx context.Context, requestID int64) error {
	r, err := GetRequest(ctx, c.db, requestID, false)
	if err != nil {
		return err
	}
	tasks, err := RequestTasks(ctx, c.db, r.ID, false)
	if err != nil {
		return err
	}
	tg, err := c.target(ctx, r)
	if err != nil {
		return err
	}
	cr := &gh.CheckRun{
		Name:       checkName(CheckPrefix(r.Type), r, ""),
		HeadSHA:    r.SHA,
		ExternalID: RequestExternalID(r),
		DetailsURL: c.RequestURL(r.ID),
	}
	started := false
	for _, t := range tasks {
		started = started || t.State == TaskRunning || (t.State.Terminal() && t.StartedAt != nil)
	}
	switch {
	case r.State.Terminal():
		checkrun.SetCompleted(cr, string(r.State.Conclusion()), r.FinishedAt)
		if !r.CreatedAt.After(*cr.CompletedAt) {
			cr.StartedAt = &r.CreatedAt
		}
	case started:
		cr.Status = gh.CheckStatusInProgress
	default:
		cr.Status = gh.CheckStatusQueued
	}
	title, summary := c.requestSummary(r, tasks)
	cr.Output = &gh.CheckRunOutput{Title: title, Summary: summary}
	return checkrun.Upsert(ctx, tg, cr, r.CheckRunID, func(id int64) error {
		_, err := c.db.Exec(ctx, `UPDATE ai_requests SET check_run_id=$2 WHERE id=$1`, r.ID, id)
		return err
	})
}

// requestSummary 返回请求汇总 Check Run 的标题和摘要：配置错误、警告、排队说明，以及每个任务的状态表。
func (c *Core) requestSummary(r *Request, tasks []*Task) (string, string) {
	var b strings.Builder
	if r.ConfigError != nil {
		b.WriteString("AI 配置错误：\n\n```\n" + *r.ConfigError + "\n```\n")
	}
	if w := deref(r.Warnings); w != "" {
		b.WriteString("警告：\n\n```\n" + w + "\n```\n\n")
	}
	if r.State == RequestQueued {
		b.WriteString(waitingReason + "。\n\n")
	}
	counts := map[TaskState]int{}
	if len(tasks) > 0 {
		b.WriteString("| 任务 | 状态 | 说明 |\n|---|---|---|\n")
		for _, t := range tasks {
			counts[t.State]++
			note := strings.ReplaceAll(shorten.Text(deref(t.Reason), maxTaskNoteBytes), "|", "\\|")
			note = strings.ReplaceAll(note, "\n", " ")
			fmt.Fprintf(&b, "| [%s](%s) | %s | %s |\n", t.DisplayName, c.TaskURL(t.ID), t.State, note)
		}
	}
	summary := shorten.Text(b.String(), checkrun.MaxSummaryBytes)
	if r.ConfigError != nil {
		return "AI 配置错误", summary
	}
	var parts []string
	for _, st := range append(terminalTaskStates(), TaskRunning, TaskPending, TaskWaiting) {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
		}
	}
	return fmt.Sprintf("%d 个任务：", len(tasks)) + strings.Join(parts, "，"), summary
}

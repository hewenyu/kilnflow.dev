package cicheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/checkrun"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Check Run 的渲染与同步（PLAN #13.3）：CI 的 run、job 的 Check Run 由 outbox 投递（Register 注册的处理函数）。

// outbox 记录的类型（已写入的记录按它投递，不能修改）。
const (
	kindJobCheck = "checkrun.job"
	kindRunCheck = "checkrun.run"
)

type checkPayload struct {
	JobID int64 `json:"job_id,omitempty"`
	RunID int64 `json:"run_id,omitempty"`
	Rerun int   `json:"rerun,omitempty"`
}

// EnqueueJob 同步 job 的 Check Run（以及它所在 run 的汇总 Check Run）。发布等没有 run 的 job、直接沿用的 job 不写入
// （沿用的 job 不新建 Check Run，PLAN #6.6）。
func EnqueueJob(ctx context.Context, w *outbox.Writer, tx pgx.Tx, j *cistore.Job) error {
	if !w.Enabled() || j.RunID == 0 || j.ReusedFrom != nil {
		return nil
	}
	if err := w.Enqueue(ctx, tx, fmt.Sprintf("checkrun:job:%d", j.ID), kindJobCheck, checkPayload{JobID: j.ID}); err != nil {
		return err
	}
	return EnqueueRun(ctx, w, tx, j.RunID, j.Rerun)
}

// EnqueueRun 同步 run 的汇总 Check Run。
func EnqueueRun(ctx context.Context, w *outbox.Writer, q store.Execer, runID int64, rerun int) error {
	return w.Enqueue(ctx, q, fmt.Sprintf("checkrun:run:%d:%d", runID, rerun), kindRunCheck, checkPayload{RunID: runID, Rerun: rerun})
}

// Config 是 Syncer 的配置。
type Config struct {
	// AppID 是 GitHub App 的 ID，用于按 external_id 查找本 App 创建的 Check Run。
	AppID int64
	// PublicURL 是 Dashboard 的地址，用于 Check Run 的 details_url。
	PublicURL string
}

// Deps 是 Syncer 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 为 nil（没有配置 GitHub App）时记录直接视为投递成功。
	GitHub *gh.Client
	// Logs 用于在失败 job 的 Check Run 摘要中附上日志末尾。
	Logs *logstore.Store
}

// Syncer 把 run、job 的最新状态同步到 GitHub Check Run。
type Syncer struct {
	db        *store.DB
	gh        *gh.Client
	appID     int64
	logs      *logstore.Store
	publicURL string
}

// New 返回 Syncer。
func New(cfg Config, d Deps) *Syncer {
	return &Syncer{db: d.DB, gh: d.GitHub, appID: cfg.AppID, logs: d.Logs, publicURL: cfg.PublicURL}
}

// Register 在 outbox 的投递 worker 上注册 Check Run 记录的处理函数。必须在 Worker.Run 之前调用。
func (s *Syncer) Register(w *outbox.Worker) {
	w.Handle(kindJobCheck, s.handle)
	w.Handle(kindRunCheck, s.handle)
}

func (s *Syncer) handle(ctx context.Context, payload []byte) error {
	if s.gh == nil {
		return nil
	}
	var p checkPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("outbox 记录的 payload 无法解析：%w", err)
	}
	if p.JobID != 0 {
		return s.syncJobCheck(ctx, p.JobID)
	}
	return s.syncRunCheck(ctx, p.RunID, p.Rerun)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// maxJobNoteBytes 是 run 汇总表格中每个 job 说明的上限。
const maxJobNoteBytes = 120

// Check Run 的 external_id：job 的为 job:<job ID>，run 汇总的为 run:<run ID>:<rerun>。
// GitHub 上的 Re-run 据此找到要重跑的对象（ParseExternalID），已经创建的 Check Run 也按它查找，格式不能修改。
const (
	externalIDJobPrefix = "job:"
	externalIDRunPrefix = "run:"
)

// ErrUnknownExternalID 表示 external_id 不是 Kiln 生成的格式（例如其他 App 创建的 Check Run）。
var ErrUnknownExternalID = errors.New("不是 Kiln 的 external_id")

// jobExternalID 返回 job 的 Check Run 的 external_id。
func jobExternalID(jobID int64) string {
	return externalIDJobPrefix + strconv.FormatInt(jobID, 10)
}

// runExternalID 返回 run 某一次运行的汇总 Check Run 的 external_id。
func runExternalID(runID int64, rerun int) string {
	return fmt.Sprintf("%s%d:%d", externalIDRunPrefix, runID, rerun)
}

// CheckTarget 是 Check Run 的 external_id 指向的对象：JobID 非 0 时是 job 的 Check Run，否则是 run（RunID）的汇总 Check Run。
type CheckTarget struct {
	JobID int64
	RunID int64
}

// ParseExternalID 解析 Check Run 的 external_id。不是 Kiln 的格式时返回 ErrUnknownExternalID，
// 格式正确但 ID 无效时返回其他错误。run 的汇总 Check Run 只取 run ID，忽略 rerun。
func ParseExternalID(id string) (CheckTarget, error) {
	if rest, ok := strings.CutPrefix(id, externalIDJobPrefix); ok {
		jobID, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return CheckTarget{}, fmt.Errorf("external_id %q 中的 job ID 无效：%w", id, err)
		}
		return CheckTarget{JobID: jobID}, nil
	}
	if rest, ok := strings.CutPrefix(id, externalIDRunPrefix); ok {
		runPart, _, _ := strings.Cut(rest, ":")
		runID, err := strconv.ParseInt(runPart, 10, 64)
		if err != nil {
			return CheckTarget{}, fmt.Errorf("external_id %q 中的 run ID 无效：%w", id, err)
		}
		return CheckTarget{RunID: runID}, nil
	}
	return CheckTarget{}, ErrUnknownExternalID
}

// JobURL 返回 Dashboard 中 job 的地址；RunURL 返回 run 的地址。Check Run 的 Details URL 和 AI 报告里的链接共用这两条规则。
// 没有配置 PublicURL 时返回空串：拼出 "/jobs/5" 这样的相对地址在 Dashboard 之外没有意义。
func JobURL(publicURL string, jobID int64) string {
	if publicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/jobs/%d", publicURL, jobID)
}

// RunURL 返回 Dashboard 中 run 的地址（没有配置 PublicURL 时为空）。
func RunURL(publicURL string, runID int64) string {
	if publicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/runs/%d", publicURL, runID)
}

func (s *Syncer) detailsURL(runID int64, jobID int64) string {
	if jobID != 0 {
		return JobURL(s.publicURL, jobID)
	}
	return RunURL(s.publicURL, runID)
}

// checkName 返回 Check Run 的名称（PLAN #13.3）。
func checkName(run *cistore.Run, jobDisplay string) string {
	wf := workflow.BaseName(run.Workflow)
	if jobDisplay == "" {
		return fmt.Sprintf("%s (%s)", wf, run.Event)
	}
	return fmt.Sprintf("%s / %s (%s)", wf, jobDisplay, run.Event)
}

func (s *Syncer) syncJobCheck(ctx context.Context, jobID int64) error {
	j, err := cistore.GetJob(ctx, s.db, jobID, false)
	if err != nil {
		return err
	}
	run, err := cistore.GetRun(ctx, s.db, j.RunID, false)
	if err != nil {
		return err
	}
	repo, err := store.GetRepo(ctx, s.db, run.RepoID)
	if err != nil {
		return err
	}
	cr := &gh.CheckRun{
		Name:       checkName(run, j.DisplayName),
		HeadSHA:    run.SHA,
		ExternalID: jobExternalID(j.ID),
		DetailsURL: s.detailsURL(run.ID, j.ID),
	}
	title, summary := s.jobSummary(j)
	cr.Output = &gh.CheckRunOutput{Title: title, Summary: summary}
	switch {
	case j.State.Queued() || notStarted(j):
		// 已预留到托管池、还没有开始执行的 job 也是 queued：不显示为已在机器上运行（PLAN #32.6）
		cr.Status = gh.CheckStatusQueued
	case j.State == store.JobRunning:
		cr.Status = gh.CheckStatusInProgress
		cr.StartedAt = j.StartedAt
	default:
		checkrun.SetCompleted(cr, string(j.State.Conclusion()), j.FinishedAt)
		cr.StartedAt = j.StartedAt // 没有执行过（命中缓存、跳过等）时为 nil，创建时由 upsertCheck 补上
	}
	return s.upsertCheck(ctx, repo, run, cr, j.CheckRunID, func(id int64) error {
		_, err := s.db.Exec(ctx, `UPDATE jobs SET check_run_id=$2 WHERE id=$1`, j.ID, id)
		return err
	})
}

func (s *Syncer) jobSummary(j *cistore.Job) (string, string) {
	reason := deref(j.Reason)
	var b strings.Builder
	title := j.State.Title()
	switch {
	case j.State == store.JobSucceeded && j.CachedFrom != nil:
		title = "成功（命中缓存）"
	case notStarted(j):
		title = titleNotStarted
	}
	if reason != "" {
		b.WriteString(reason + "\n\n")
	}
	fmt.Fprintf(&b, "- job：`%s`\n- 平台：`%s`，executor：`%s`\n", j.JobKey, j.Platform, j.Executor)
	if j.ActionKey != nil {
		fmt.Fprintf(&b, "- action key：`%s`\n", shorten.Hash(*j.ActionKey))
	}
	if id := j.LogID(); j.State.Failed() && id != 0 {
		b.WriteString(checkrun.LogTail(s.logs.Tail(id, checkrun.FailedLogTailLines)))
	}
	return title, shorten.Text(b.String(), checkrun.MaxSummaryBytes)
}

// rerunCheck 是 run 某一次运行（run_reruns 中的一行）与汇总 Check Run 有关的部分。
type rerunCheck struct {
	State      store.RunState
	Conclusion *string
	CheckRunID *int64
	FinishedAt *time.Time
	// StartedAt 是这次运行开始（没有记录时为创建）的时间。
	StartedAt *time.Time
}

func (s *Syncer) syncRunCheck(ctx context.Context, runID int64, rerun int) error {
	run, err := cistore.GetRun(ctx, s.db, runID, false)
	if err != nil {
		return err
	}
	repo, err := store.GetRepo(ctx, s.db, run.RepoID)
	if err != nil {
		return err
	}
	var rr rerunCheck
	if err := s.db.QueryRow(ctx, `SELECT state, conclusion, check_run_id, finished_at, coalesce(started_at, created_at) FROM run_reruns WHERE run_id=$1 AND rerun=$2`, runID, rerun).
		Scan(&rr.State, &rr.Conclusion, &rr.CheckRunID, &rr.FinishedAt, &rr.StartedAt); err != nil {
		return err
	}
	jobs, err := cistore.RerunJobs(ctx, s.db, runID, rerun, false)
	if err != nil {
		return err
	}
	cr := &gh.CheckRun{
		Name:       checkName(run, ""),
		HeadSHA:    run.SHA,
		ExternalID: runExternalID(runID, rerun),
		DetailsURL: s.detailsURL(run.ID, 0),
	}
	switch {
	case rr.State.Terminal():
		conclusion := deref(rr.Conclusion)
		if conclusion == "" {
			conclusion = string(rr.State.Conclusion())
		}
		checkrun.SetCompleted(cr, conclusion, rr.FinishedAt)
		if rr.StartedAt != nil && !rr.StartedAt.After(*cr.CompletedAt) {
			cr.StartedAt = rr.StartedAt
		}
	case jobsStarted(jobs):
		cr.Status = gh.CheckStatusInProgress
	default:
		cr.Status = gh.CheckStatusQueued
	}
	title, summary := s.runSummary(run, rr.State, jobs)
	cr.Output = &gh.CheckRunOutput{Title: title, Summary: summary}
	if run.ConfigError != nil {
		cr.Output.Annotations = configAnnotations(run)
	}
	return s.upsertCheck(ctx, repo, run, cr, rr.CheckRunID, func(id int64) error {
		_, err := s.db.Exec(ctx, `UPDATE run_reruns SET check_run_id=$3 WHERE run_id=$1 AND rerun=$2`, runID, rerun, id)
		return err
	})
}

// titleNotStarted 是已预留、还没有开始执行的 job 的 Check Run 标题。
const titleNotStarted = "已预留，等待开始执行"

// notStarted 判断 job 处于 running 但还没有开始执行（托管执行已预留、已提交或在外部排队，jobs.started_at 为空，PLAN #32.6）。
// Native 领取时写 started_at，所以 Native 的 running 总是已开始。
func notStarted(j *cistore.Job) bool { return j.State == store.JobRunning && j.StartedAt == nil }

// jobsStarted 判断是否有 job 已经开始执行（正在执行，或执行过后结束）。
func jobsStarted(jobs []*cistore.Job) bool {
	for _, j := range jobs {
		if (j.State == store.JobRunning || j.State.Terminal()) && j.StartedAt != nil {
			return true
		}
	}
	return false
}

// runSummary 返回 run 汇总 Check Run 的标题和摘要：配置错误、警告、排队说明，以及每个 job 的状态表。
func (s *Syncer) runSummary(run *cistore.Run, state store.RunState, jobs []*cistore.Job) (string, string) {
	var b strings.Builder
	if run.ConfigError != nil {
		b.WriteString("workflow 配置错误：\n\n```\n" + *run.ConfigError + "\n```\n")
	}
	if run.Warnings != nil && *run.Warnings != "" {
		b.WriteString("警告：\n\n```\n" + *run.Warnings + "\n```\n\n")
	}
	if state == store.RunQueued {
		b.WriteString("并发组中已有正在执行的 run，排队等待。\n\n")
	}
	counts := map[store.JobState]int{}
	if len(jobs) > 0 {
		b.WriteString("| job | 状态 | 说明 |\n|---|---|---|\n")
		for _, j := range jobs {
			counts[j.State]++
			note := strings.ReplaceAll(shorten.Text(deref(j.Reason), maxJobNoteBytes), "|", "\\|")
			note = strings.ReplaceAll(note, "\n", " ")
			fmt.Fprintf(&b, "| [%s](%s) | %s | %s |\n", j.DisplayName, s.detailsURL(run.ID, j.ID), j.State, note)
		}
	}
	summary := shorten.Text(b.String(), checkrun.MaxSummaryBytes)
	switch {
	case run.ConfigError != nil:
		return "workflow 配置错误", summary
	case state == store.RunSkipped:
		return "被 paths 过滤，没有 job 执行", summary
	}
	var parts []string
	for _, st := range store.JobStateOrder() {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
		}
	}
	return fmt.Sprintf("%d 个 job：", len(jobs)) + strings.Join(parts, "，"), summary
}

// upsertCheck 创建或更新 run 或 job 的 Check Run（checkrun.Upsert）。
func (s *Syncer) upsertCheck(ctx context.Context, repo *store.Repo, run *cistore.Run, cr *gh.CheckRun, existing *int64, save func(int64) error) error {
	return checkrun.Upsert(ctx, checkrun.Target{Inst: s.gh.Inst(repo.InstallationID), Repo: repo.FullName(), SHA: run.SHA, AppID: s.appID}, cr, existing, save)
}

// maxAnnotations 是一次 Check Run 更新最多携带的 annotation 数（GitHub 的限制）。
const maxAnnotations = 50

// configAnnotations 把配置错误中带行号的条目转换成 annotation（PLAN #4.9）。
func configAnnotations(run *cistore.Run) []gh.Annotation {
	var out []gh.Annotation
	for line := range strings.SplitSeq(deref(run.ConfigError), "\n") {
		// 格式：<file>:<line>:<col>: 错误：<msg>
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 {
			continue
		}
		var ln int
		if _, err := fmt.Sscanf(parts[1], "%d", &ln); err != nil || ln <= 0 {
			continue
		}
		out = append(out, gh.Annotation{
			Path: workflow.Dir + "/" + parts[0], StartLine: ln, EndLine: ln,
			AnnotationLevel: "failure", Message: strings.TrimSpace(parts[3]),
		})
		if len(out) >= maxAnnotations {
			break
		}
	}
	return out
}

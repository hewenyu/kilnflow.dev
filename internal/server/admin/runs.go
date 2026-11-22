package admin

import (
	"context"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// run、job 和日志的查询与操作（CLI 和 Dashboard 共用）。

const (
	defaultRunListLimit = 30
	maxRunListLimit     = 500
	// logChunk 是一次读取日志的最大字节数。
	logChunk = 1 << 20
)

func toAPIRun(r *cistore.Run, repo string) *apiv1.Run {
	return &apiv1.Run{
		Id: r.ID, Repo: repo, Workflow: r.Workflow, Event: r.Event, Ref: r.Ref, Sha: r.SHA, State: string(r.State),
		Rerun: clampInt32(r.CurrentRerun), CreatedAt: r.CreatedAt.Unix(), Source: r.Source, Error: deref(r.ConfigError),
	}
}

func toAPIJob(j *cistore.Job, runner string) *apiv1.Job {
	out := &apiv1.Job{
		Id: j.ID, JobKey: j.JobKey, DisplayName: j.DisplayName, State: string(j.State), Kind: string(j.Kind), Platform: j.Platform,
		Executor: string(j.Executor), Attempts: clampInt32(j.AttemptCount), Reason: deref(j.Reason), ActionKey: deref(j.ActionKey),
		Rerun: clampInt32(j.Rerun), Runner: runner,
	}
	if j.CachedFrom != nil {
		out.CachedFromJobId = *j.CachedFrom
	}
	if j.State == store.JobPending {
		out.WaitReason = deref(j.WaitReason)
	}
	if j.StartedAt != nil {
		out.StartedAt = j.StartedAt.Unix()
	}
	if j.FinishedAt != nil {
		out.FinishedAt = j.FinishedAt.Unix()
	}
	return out
}

// RunnerNames 一次查出各 job 当前 attempt 所在的 Runner 名称（job ID → 名称）。
func (s *Service) RunnerNames(ctx context.Context, jobs []*cistore.Job) (map[int64]string, error) {
	attempts := map[int64]int64{} // attempt → job
	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		if j.CurrentAttemptID != nil {
			attempts[*j.CurrentAttemptID] = j.ID
			ids = append(ids, *j.CurrentAttemptID)
		}
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	type attemptRunner struct {
		AttemptID int64
		Name      string
	}
	rows, err := s.db.Query(ctx, `SELECT a.id, `+cistore.AttemptExecutorSQL+` FROM attempts a LEFT JOIN runners r ON r.id=a.runner_id WHERE a.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[attemptRunner])
	if err != nil {
		return nil, err
	}
	for _, ar := range list {
		out[attempts[ar.AttemptID]] = ar.Name
	}
	return out, nil
}

func (a *rpcAPI) ListRuns(ctx context.Context, req *connect.Request[apiv1.ListRunsRequest]) (*connect.Response[apiv1.ListRunsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = defaultRunListLimit
	}
	limit = min(limit, maxRunListLimit)
	runs, repos, err := a.s.ListRuns(ctx, req.Msg.Repo, limit)
	if err != nil {
		return nil, err
	}
	resp := &apiv1.ListRunsResponse{}
	for i, r := range runs {
		resp.Runs = append(resp.Runs, toAPIRun(r, repos[i]))
	}
	return connect.NewResponse(resp), nil
}

// ListRuns 返回最近的 run 及其仓库全名。repo 为空时列出全部仓库。
func (s *Service) ListRuns(ctx context.Context, repo string, limit int) ([]*cistore.Run, []string, error) {
	var repoID int64
	if repo != "" {
		r, err := store.FindRepo(ctx, s.db, repo)
		if err != nil {
			return nil, nil, rpcError(connect.CodeInvalidArgument, err)
		}
		repoID = r.ID
	}
	return cistore.ListRuns(ctx, s.db, repoID, limit)
}

func (a *rpcAPI) GetRun(ctx context.Context, req *connect.Request[apiv1.GetRunRequest]) (*connect.Response[apiv1.GetRunResponse], error) {
	s := a.s
	run, err := cistore.GetRun(ctx, s.db, req.Msg.Id, false)
	if isNotFound(err) {
		return nil, notFound("没有 run #%d", req.Msg.Id)
	}
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, s.db, run.RepoID)
	if err != nil {
		return nil, err
	}
	jobs, err := cistore.RerunJobs(ctx, s.db, run.ID, run.CurrentRerun, false)
	if err != nil {
		return nil, err
	}
	runners, err := s.RunnerNames(ctx, jobs)
	if err != nil {
		return nil, err
	}
	resp := &apiv1.GetRunResponse{Run: toAPIRun(run, repo.FullName())}
	for _, j := range jobs {
		resp.Jobs = append(resp.Jobs, toAPIJob(j, runners[j.ID]))
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) CancelRun(ctx context.Context, req *connect.Request[apiv1.CancelRunRequest]) (*connect.Response[apiv1.CancelRunResponse], error) {
	if err := a.s.CancelRun(ctx, req.Msg.Id, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.CancelRunResponse{}), nil
}

// CancelRun 取消 run（用户操作）并记录审计日志。run 不存在时返回 NotFound。
func (s *Service) CancelRun(ctx context.Context, id int64, actor string) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := s.engine.CancelRun(ctx, tx, id, store.CancelByUser); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "run.cancel", map[string]any{"run_id": id})
	})
	if isNotFound(err) {
		return notFound("没有 run #%d", id)
	}
	if err == nil {
		s.hub.All()
	}
	return err
}

// DispatchRequest 是一次手动触发（workflow_dispatch）。
type DispatchRequest struct {
	Repo     string // owner/name
	Workflow string // 文件名，可以省略 .yml
	Ref      string // 分支、tag 或完整的 ref；空表示默认分支
	Inputs   map[string]string
	// RequestID 用于幂等：同一个 RequestID 只创建一个 run。空时自动生成。
	RequestID string
	Actor     string
	Source    string // dashboard | cli
}

// Dispatch 手动触发 workflow_dispatch（CLI 和 Dashboard 共用），返回 run ID。
func (s *Service) Dispatch(ctx context.Context, req *DispatchRequest) (int64, error) {
	return s.planner.Dispatch(ctx, &planner.DispatchRequest{
		Repo: req.Repo, Workflow: req.Workflow, Ref: req.Ref, Inputs: req.Inputs, RequestID: req.RequestID,
		Actor: req.Actor, Source: req.Source,
	})
}

// DispatchWorkflows 解析仓库 repo 的 ref，列出其中的 workflow 及能否手动触发（Dashboard 的"启动构建"表单使用）。
func (s *Service) DispatchWorkflows(ctx context.Context, repo, ref string) (*planner.DispatchTarget, error) {
	return s.planner.DispatchWorkflows(ctx, repo, ref)
}

func (a *rpcAPI) Dispatch(ctx context.Context, req *connect.Request[apiv1.DispatchRequest]) (*connect.Response[apiv1.DispatchResponse], error) {
	m := req.Msg
	id, err := a.s.Dispatch(ctx, &DispatchRequest{
		Repo: m.Repo, Workflow: m.Workflow, Ref: m.Ref, Inputs: m.Inputs, RequestID: m.RequestId,
		Actor: rpcActor(ctx), Source: planner.SourceCLI,
	})
	if err != nil {
		return nil, rpcError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&apiv1.DispatchResponse{RunId: id}), nil
}

// RerunRun 为已结束的 run 创建新的 rerun（mode 为空等同于 failed），返回新的 rerun 序号。
// run 不存在时返回 NotFound，其他原因（例如 run 还没有结束）返回 FailedPrecondition，内部错误原样返回。
func (s *Service) RerunRun(ctx context.Context, runID int64, mode store.RerunMode, actor string, keepOnFailure bool) (int, error) {
	n, err := s.planner.Rerun(ctx, planner.RerunRequest{RunID: runID, Mode: mode, Actor: actor, KeepOnFailure: keepOnFailure})
	switch {
	case isNotFound(err):
		return 0, notFound("没有 run #%d", runID)
	case err != nil:
		return 0, rpcError(connect.CodeFailedPrecondition, err)
	}
	return n, nil
}

func (a *rpcAPI) RerunRun(ctx context.Context, req *connect.Request[apiv1.RerunRunRequest]) (*connect.Response[apiv1.RerunRunResponse], error) {
	n, err := a.s.RerunRun(ctx, req.Msg.Id, store.RerunMode(req.Msg.Mode), rpcActor(ctx), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.RerunRunResponse{Rerun: clampInt32(n)}), nil
}

func (a *rpcAPI) RerunJob(ctx context.Context, req *connect.Request[apiv1.RerunJobRequest]) (*connect.Response[apiv1.RerunJobResponse], error) {
	_, n, err := a.s.RerunJob(ctx, req.Msg.JobId, rpcActor(ctx), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.RerunJobResponse{Rerun: clampInt32(n)}), nil
}

// RerunJob 重跑一个 job 及其下游，返回 job 所属的 run 和新的 rerun 序号。
func (s *Service) RerunJob(ctx context.Context, jobID int64, actor string, keepOnFailure bool) (runID int64, rerun int, err error) {
	runID, err = s.jobRunID(ctx, jobID)
	if err != nil {
		return 0, 0, err
	}
	rerun, err = s.planner.Rerun(ctx, planner.RerunRequest{RunID: runID, Mode: store.RerunJob, JobID: jobID, Actor: actor, KeepOnFailure: keepOnFailure})
	if err != nil {
		return runID, 0, rpcError(connect.CodeFailedPrecondition, err)
	}
	return runID, rerun, nil
}

// jobRunID 返回 job 所属的 run。
func (s *Service) jobRunID(ctx context.Context, jobID int64) (int64, error) {
	var runID int64
	err := s.db.QueryRow(ctx, `SELECT run_id FROM jobs WHERE id=$1 AND run_id IS NOT NULL`, jobID).Scan(&runID)
	if isNotFound(err) {
		return 0, notFound("没有 job #%d", jobID)
	}
	return runID, err
}

func (a *rpcAPI) GetJobLog(ctx context.Context, req *connect.Request[apiv1.GetJobLogRequest]) (*connect.Response[apiv1.GetJobLogResponse], error) {
	l, err := a.s.JobLog(ctx, req.Msg.JobId, req.Msg.AttemptId, req.Msg.Offset)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.GetJobLogResponse{Data: l.Data, NextOffset: l.Next, Finished: l.Finished}), nil
}

// LogChunk 是一次日志读取的结果。
type LogChunk struct {
	Data []byte
	Next int64
	// Finished 表示日志不会再增长（attempt 已结束且已读到末尾）。
	Finished bool
	// State 是 job（或 AI 任务）当前的状态，Terminal 表示已经结束（Dashboard 用它们判断是否需要刷新页面）。
	State    string
	Terminal bool
	// Attempt 是实际读取的 attempt（发布 job、还没有 attempt 时为 0）。
	Attempt int64
	// CurrentAttempt 是 job 当前的 attempt（没有时为 0）。attempt 自动重试后与 Attempt 不同。
	CurrentAttempt int64
}

// logSource 是一次日志读取的对象。
type logSource struct {
	job *cistore.Job
	// id 是 logstore 中的日志编号；0 表示还没有日志（job 还没有 attempt）。
	id int64
	// attempt 是读取的 attempt（发布 job 为 0）。
	attempt int64
	// ended 表示日志不会再增长：attempt 已有结果，或发布 job、没有 attempt 的 job 已到终态。
	ended bool
}

// resolveLog 找出 job 的日志。attemptID 为 0 时取当前 attempt；指定的 attempt 不属于这个 job 时返回 NotFound。
func (s *Service) resolveLog(ctx context.Context, jobID, attemptID int64) (*logSource, error) {
	j, err := cistore.GetJob(ctx, s.db, jobID, false)
	if isNotFound(err) {
		return nil, notFound("没有 job #%d", jobID)
	}
	if err != nil {
		return nil, err
	}
	src := &logSource{job: j}
	if !cistore.LogsPerAttempt(j.Kind) {
		// 发布 job 没有 attempt，日志按 job 保存
		if attemptID != 0 {
			return nil, notFound("job #%d 是发布 job，没有 attempt #%d", jobID, attemptID)
		}
		src.id, src.ended = j.LogID(), j.State.Terminal()
		return src, nil
	}
	if attemptID == 0 {
		if attemptID = j.LogID(); attemptID == 0 {
			src.ended = j.State.Terminal()
			return src, nil
		}
	}
	var outcome *string
	err = s.db.QueryRow(ctx, `SELECT outcome FROM attempts WHERE id=$1 AND job_id=$2`, attemptID, jobID).Scan(&outcome)
	if isNotFound(err) {
		return nil, notFound("job #%d 没有 attempt #%d", jobID, attemptID)
	}
	if err != nil {
		return nil, err
	}
	src.id, src.attempt, src.ended = attemptID, attemptID, outcome != nil
	return src, nil
}

// JobLog 从 offset 开始读取 job 的日志。attemptID 为 0 时读取当前 attempt。
// offset 为负数时返回 InvalidArgument；job 不存在、attempt 不属于这个 job 时返回 NotFound。
func (s *Service) JobLog(ctx context.Context, jobID, attemptID, offset int64) (*LogChunk, error) {
	if offset < 0 {
		return nil, invalid(fmt.Errorf("offset 不能为负数：%d", offset))
	}
	src, err := s.resolveLog(ctx, jobID, attemptID)
	if err != nil {
		return nil, err
	}
	res := &LogChunk{State: string(src.job.State), Terminal: src.job.State.Terminal(), Attempt: src.attempt}
	if src.job.CurrentAttemptID != nil {
		res.CurrentAttempt = *src.job.CurrentAttemptID
	}
	if src.id == 0 {
		res.Finished = src.ended
		return res, nil
	}
	res.Data, res.Next, err = s.logs.Read(src.id, offset, logChunk)
	if err != nil {
		return nil, err
	}
	res.Finished = src.ended && len(res.Data) == 0
	return res, nil
}

// LogFile 是一份完整的日志（下载用）。
type LogFile struct {
	io.ReadCloser
	Size int64
	// Attempt 是日志所属的 attempt（发布 job、还没有 attempt 时为 0）。
	Attempt int64
}

// OpenJobLog 打开 job 某个 attempt（0 为当前 attempt）的完整日志。错误与 JobLog 相同。
func (s *Service) OpenJobLog(ctx context.Context, jobID, attemptID int64) (*LogFile, error) {
	src, err := s.resolveLog(ctx, jobID, attemptID)
	if err != nil {
		return nil, err
	}
	if src.id == 0 {
		return &LogFile{ReadCloser: io.NopCloser(strings.NewReader(""))}, nil
	}
	rc, size, err := s.logs.Open(src.id)
	if err != nil {
		return nil, err
	}
	return &LogFile{ReadCloser: rc, Size: size, Attempt: src.attempt}, nil
}

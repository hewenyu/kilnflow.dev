package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 请求、任务和 AI 执行节点的管理操作（PLAN #30.9，CLI 和 Dashboard 共用）。AI 的数据只在 AI 的表中，与 CI 的 run、job 分开。

// defaultAIRequestLimit、maxAIRequestLimit 是列出 AI 请求的默认条数和上限。
const (
	defaultAIRequestLimit = 50
	maxAIRequestLimit     = 500
)

// errNoAI 表示 Server 没有组装 AI 领域（只在测试中出现）。
var errNoAI = errors.New("AI 领域没有启用")

// TriggerAIReview 手动触发一次 AI 审计（CLI 和 Dashboard 共用），返回 AI 审计请求的 ID 和被审计的 commit。
func (s *Service) TriggerAIReview(ctx context.Context, repo, ref, requestID, actor, source string) (int64, string, error) {
	if s.reviews == nil {
		return 0, "", connect.NewError(connect.CodeFailedPrecondition, errNoAI)
	}
	return s.reviews.Trigger(ctx, &review.TriggerRequest{Repo: repo, Ref: ref, RequestID: requestID, Actor: actor, Source: source})
}

func (a *rpcAPI) TriggerAIReview(ctx context.Context, req *connect.Request[apiv1.TriggerAIReviewRequest]) (*connect.Response[apiv1.TriggerAIReviewResponse], error) {
	m := req.Msg
	id, sha, err := a.s.TriggerAIReview(ctx, m.Repo, m.Ref, m.RequestId, rpcActor(ctx), store.SourceCLI)
	if err != nil {
		return nil, rpcError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&apiv1.TriggerAIReviewResponse{AiRequestId: id, Sha: sha}), nil
}

// ---- AI 执行节点 ----

// AIRunner 是一个 AI 执行节点（PLAN #33）。没有自升级状态：连接器用 npm 升级，Server 不参与（PLAN #33.3）。
type AIRunner struct{ *ai.RunnerView }

// ListAIRunners 返回 AI 执行节点（id 不为 0 时只返回这一台），按名称排序。
func (s *Service) ListAIRunners(ctx context.Context, id int64) ([]*AIRunner, error) {
	list, err := ai.ListRunners(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	out := make([]*AIRunner, len(list))
	for i, r := range list {
		out[i] = &AIRunner{RunnerView: r}
	}
	return out, nil
}

// AIRunnerState 返回 AI 执行节点的显示状态（与 RunnerState 相同的规则）：已吊销、暂停接单、离线、接单中。
func AIRunnerState(r *ai.RunnerView) string {
	switch {
	case r.State != store.RunnerActive:
		return r.State
	case r.Drain:
		return store.RunnerDraining
	case !r.Online:
		return store.RunnerOffline
	}
	return store.RunnerActive
}

func (a *rpcAPI) ListAIRunners(ctx context.Context, _ *connect.Request[apiv1.ListAIRunnersRequest]) (*connect.Response[apiv1.ListAIRunnersResponse], error) {
	list, err := a.s.ListAIRunners(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.AIRunner, 0, len(list))
	for _, r := range list {
		v := &apiv1.AIRunner{
			Id: r.ID, Name: r.Name, Role: r.Role, Version: r.Version, State: AIRunnerState(r.RunnerView), LastSeenAt: unixOrZero(r.LastSeen),
			Platform: r.Platform, Protocol: clampInt32(r.Protocol), NeedsUpgrade: r.NeedsUpgrade, HostId: r.HostID,
			Capacity: clampInt32(r.Capacity), OrcaState: r.OrcaState, OrcaReason: r.OrcaReason, OrcaVersion: r.OrcaVersion,
			OrcaRuntimeId: r.OrcaRuntimeID, DockerState: r.DockerState, DockerReason: r.DockerReason, Running: clampInt32(r.Running),
		}
		for _, ag := range r.Agents {
			v.Agents = append(v.Agents, &apiv1.AIRunnerAgent{
				Agent: ag.Agent, Name: ag.Name, Version: ag.Version, State: ag.Show, Reason: ag.Reason,
				CheckedAt: unixOrZero(ag.CheckedAt), BackoffUntil: unixOrZero(ag.BackoffUntil),
			})
		}
		out = append(out, v)
	}
	return connect.NewResponse(&apiv1.ListAIRunnersResponse{Runners: out}), nil
}

// ---- AI 请求 ----

// AIRequestFilter 是列出 AI 请求的条件；零值表示全部（最近 defaultAIRequestLimit 条）。
type AIRequestFilter struct {
	// Repo 是 owner/name；RepoID 不为 0 时优先使用。
	Repo   string
	RepoID int64
	// Type 是 review 或 develop。
	Type string
	// PR 不为 0 时只列出这个 PR 的审计。
	PR    int
	State string
	Limit int
}

// AIRequestView 是一个 AI 请求和它的显示信息。
type AIRequestView struct {
	*ai.Request
	Repo string
	// PRNumber 是审计的 PR（手动审计、开发请求为 0）；WorkItemID 是关联的 AI 开发工作项（没有时为 0）。
	PRNumber   int
	WorkItemID int64
	// DevMode、DevRound 是开发请求的方式（select、fix）和返修轮次。
	DevMode  string
	DevRound int
}

// ListAIRequests 返回满足条件的 AI 请求，按 ID 倒序。
func (s *Service) ListAIRequests(ctx context.Context, f AIRequestFilter) ([]*AIRequestView, error) {
	if f.Limit <= 0 {
		f.Limit = defaultAIRequestLimit
	}
	f.Limit = min(f.Limit, maxAIRequestLimit)
	if f.Type != "" && f.Type != agentcfg.TaskReview && f.Type != agentcfg.TaskDevelop {
		return nil, invalid(fmt.Errorf("未知的 AI 请求类型 %q（%s 或 %s）", f.Type, agentcfg.TaskReview, agentcfg.TaskDevelop))
	}
	if f.State != "" && !validRequestState(ai.RequestState(f.State)) {
		return nil, invalid(fmt.Errorf("未知的 AI 请求状态 %q", f.State))
	}
	if f.RepoID == 0 && f.Repo != "" {
		r, err := store.FindRepo(ctx, s.db, f.Repo)
		if errors.Is(err, store.ErrRepoNotFound) {
			return nil, notFound("没有仓库 %s", f.Repo)
		}
		if err != nil {
			return nil, err
		}
		f.RepoID = r.ID
	}
	list, err := ai.QueryRequests(ctx, s.db, `($1::bigint=0 OR repo_id=$1) AND ($2::text='' OR type=$2) AND ($3::text='' OR state=$3)
		AND ($4::int=0 OR id IN (SELECT request_id FROM ai_review_requests WHERE pr_number=$4)) ORDER BY id DESC LIMIT $5`,
		f.RepoID, f.Type, f.State, f.PR, f.Limit)
	if err != nil {
		return nil, err
	}
	return s.aiRequestViews(ctx, list)
}

func validRequestState(st ai.RequestState) bool {
	switch st {
	case ai.RequestQueued, ai.RequestActive, ai.RequestSucceeded, ai.RequestFailed, ai.RequestCancelled, ai.RequestSkipped:
		return true
	}
	return false
}

// aiRequestViews 补上请求的仓库名、PR、工作项。
func (s *Service) aiRequestViews(ctx context.Context, list []*ai.Request) ([]*AIRequestView, error) {
	if len(list) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(list))
	out := make([]*AIRequestView, len(list))
	byID := make(map[int64]*AIRequestView, len(list))
	for i, r := range list {
		ids[i] = r.ID
		out[i] = &AIRequestView{Request: r}
		byID[r.ID] = out[i]
	}
	rows, err := s.db.Query(ctx, `SELECT r.id, p.owner||'/'||p.name, coalesce(v.pr_number, 0), coalesce(v.work_item_id, d.work_item_id, 0),
		coalesce(d.mode, ''), coalesce(d.round, 0)
		FROM ai_requests r JOIN repos p ON p.id=r.repo_id
		LEFT JOIN ai_review_requests v ON v.request_id=r.id LEFT JOIN ai_dev_requests d ON d.request_id=r.id
		WHERE r.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var id int64
	var repo, mode string
	var pr, round int
	var item int64
	_, err = pgx.ForEachRow(rows, []any{&id, &repo, &pr, &item, &mode, &round}, func() error {
		v := byID[id]
		v.Repo, v.PRNumber, v.WorkItemID, v.DevMode, v.DevRound = repo, pr, item, mode, round
		return nil
	})
	return out, err
}

// AIRequestDetail 是一个 AI 请求的详情：请求、各平台任务和任务当前 attempt 的 Runner。
type AIRequestDetail struct {
	*AIRequestView
	Tasks []*ai.Task
	// Attempts 是任务当前 attempt 的执行信息（节点、实际执行的平台、Orca 关联和进度，按任务 ID）。
	Attempts map[int64]*AITaskAttempt
}

// AIRequest 返回 AI 请求的详情；不存在时返回 NotFound。
func (s *Service) AIRequest(ctx context.Context, id int64) (*AIRequestDetail, error) {
	r, err := ai.GetRequest(ctx, s.db, id, false)
	if errors.Is(err, ai.ErrNotFound) {
		return nil, notFound("没有 AI 请求 #%d", id)
	}
	if err != nil {
		return nil, err
	}
	views, err := s.aiRequestViews(ctx, []*ai.Request{r})
	if err != nil {
		return nil, err
	}
	tasks, err := ai.RequestTasks(ctx, s.db, id, false)
	if err != nil {
		return nil, err
	}
	attempts, err := s.AITaskAttempts(ctx, tasks)
	if err != nil {
		return nil, err
	}
	return &AIRequestDetail{AIRequestView: views[0], Tasks: tasks, Attempts: attempts}, nil
}

// AITaskRunners 返回任务当前 attempt 的节点名称（已删除的节点用 attempt 记下的名称）。
func (s *Service) AITaskRunners(ctx context.Context, tasks []*ai.Task) (map[int64]string, error) {
	views, err := s.AITaskAttempts(ctx, tasks)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for id, v := range views {
		out[id] = v.Runner
	}
	return out, nil
}

// AITaskAttempt 是任务当前 attempt 的执行信息（节点、实际执行的平台、Orca 关联、进度）。
type AITaskAttempt struct {
	Runner   string
	Platform string
	Orca     ai.OrcaBinding
	Stage    string
	Text     string
}

// AITaskAttempts 返回任务当前 attempt 的执行信息（按任务 ID）。
func (s *Service) AITaskAttempts(ctx context.Context, tasks []*ai.Task) (map[int64]*AITaskAttempt, error) {
	var attempts []int64
	for _, t := range tasks {
		if t.CurrentAttemptID != nil {
			attempts = append(attempts, *t.CurrentAttemptID)
		}
	}
	out := map[int64]*AITaskAttempt{}
	if len(attempts) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT a.task_id, coalesce(r.name, a.runner_name, ''), a.platform, a.orca, a.progress
		FROM ai_attempts a LEFT JOIN runners r ON r.id=a.runner_id WHERE a.id = ANY($1)`, attempts)
	if err != nil {
		return nil, err
	}
	var task int64
	var runner, platform string
	var orca, progress []byte
	_, err = pgx.ForEachRow(rows, []any{&task, &runner, &platform, &orca, &progress}, func() error {
		a := &ai.Attempt{Orca: orca, Progress: progress}
		b, err := a.DecodeOrca()
		if err != nil {
			return err
		}
		p, err := a.DecodeProgress()
		if err != nil {
			return err
		}
		out[task] = &AITaskAttempt{Runner: runner, Platform: platform, Orca: b, Stage: p.Stage, Text: p.Text}
		return nil
	})
	return out, err
}

// CancelAIRequest 取消 AI 请求（已经结束的不变）；不存在时返回 NotFound。
func (s *Service) CancelAIRequest(ctx context.Context, id int64, actor string) error {
	if s.ai == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errNoAI)
	}
	err := s.ai.Cancel(ctx, id, ai.CancelByUser, actor)
	if errors.Is(err, ai.ErrNotFound) {
		return notFound("没有 AI 请求 #%d", id)
	}
	return err
}

// RerunAIRequest 重跑已结束的 AI 审计请求，返回新的请求。mode 为空时只重跑没有成功的任务；mode 为 task 时只重跑 taskID。
func (s *Service) RerunAIRequest(ctx context.Context, id int64, mode review.RerunMode, taskID int64, actor, source string) (*ai.Request, error) {
	if s.reviews == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoAI)
	}
	if mode == "" {
		mode = review.RerunFailed
	}
	switch mode {
	case review.RerunAll, review.RerunFailed:
		if taskID != 0 {
			return nil, invalid(fmt.Errorf("只有 mode=%s 时才能指定任务", review.RerunTask))
		}
	case review.RerunTask:
		if taskID == 0 {
			return nil, invalid(errors.New("mode=task 时需要指定任务"))
		}
	default:
		return nil, invalid(fmt.Errorf("未知的重跑方式 %q（%s、%s 或 %s）", mode, review.RerunAll, review.RerunFailed, review.RerunTask))
	}
	r, err := s.reviews.Rerun(ctx, review.RerunRequest{RequestID: id, Mode: mode, TaskID: taskID, Actor: actor, Source: source})
	switch {
	case errors.Is(err, ai.ErrNotFound):
		return nil, notFound("没有 AI 请求 #%d", id)
	case err != nil:
		return nil, rpcError(connect.CodeFailedPrecondition, err)
	}
	s.hub.All()
	return r, nil
}

// ---- AI 任务的日志 ----

// aiLogSource 是一次 AI 任务日志的读取对象。
type aiLogSource struct {
	task *ai.Task
	// attempt 是读取的 attempt；0 表示任务还没有 attempt（没有日志）。
	attempt int64
	// ended 表示日志不会再增长。
	ended bool
}

// resolveAITaskLog 找出任务的日志。attemptID 为 0 时取当前 attempt；指定的 attempt 不属于这个任务时返回 NotFound。
func (s *Service) resolveAITaskLog(ctx context.Context, taskID, attemptID int64) (*aiLogSource, error) {
	if s.ai == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoAI)
	}
	t, err := ai.GetTask(ctx, s.db, taskID, false)
	if errors.Is(err, ai.ErrNotFound) {
		return nil, notFound("没有 AI 任务 #%d", taskID)
	}
	if err != nil {
		return nil, err
	}
	src := &aiLogSource{task: t}
	if attemptID == 0 {
		if t.CurrentAttemptID == nil {
			src.ended = t.State.Terminal()
			return src, nil
		}
		attemptID = *t.CurrentAttemptID
	}
	a, err := ai.GetAttempt(ctx, s.db, attemptID)
	if errors.Is(err, ai.ErrNotFound) || (err == nil && a.TaskID != taskID) {
		return nil, notFound("AI 任务 #%d 没有 attempt #%d", taskID, attemptID)
	}
	if err != nil {
		return nil, err
	}
	src.attempt, src.ended = attemptID, a.Outcome != nil
	return src, nil
}

// AITaskLog 从 offset 开始读取 AI 任务的日志（attemptID 为 0 时读取当前 attempt）。错误与 JobLog 相同。
func (s *Service) AITaskLog(ctx context.Context, taskID, attemptID, offset int64) (*LogChunk, error) {
	if offset < 0 {
		return nil, invalid(fmt.Errorf("offset 不能为负数：%d", offset))
	}
	src, err := s.resolveAITaskLog(ctx, taskID, attemptID)
	if err != nil {
		return nil, err
	}
	res := &LogChunk{State: string(src.task.State), Terminal: src.task.State.Terminal(), Attempt: src.attempt}
	if src.task.CurrentAttemptID != nil {
		res.CurrentAttempt = *src.task.CurrentAttemptID
	}
	if src.attempt == 0 {
		res.Finished = src.ended
		return res, nil
	}
	res.Data, res.Next, err = s.ai.Logs().Read(src.attempt, offset, logChunk)
	if err != nil {
		return nil, err
	}
	res.Finished = src.ended && len(res.Data) == 0
	return res, nil
}

// OpenAITaskLog 打开 AI 任务某个 attempt（0 为当前 attempt）的完整日志。
func (s *Service) OpenAITaskLog(ctx context.Context, taskID, attemptID int64) (*LogFile, error) {
	src, err := s.resolveAITaskLog(ctx, taskID, attemptID)
	if err != nil {
		return nil, err
	}
	if src.attempt == 0 {
		return &LogFile{ReadCloser: io.NopCloser(strings.NewReader(""))}, nil
	}
	rc, size, err := s.ai.Logs().Open(src.attempt)
	if err != nil {
		return nil, err
	}
	return &LogFile{ReadCloser: rc, Size: size, Attempt: src.attempt}, nil
}

// ---- RPC ----

func toAPIAIRequest(v *AIRequestView) *apiv1.AIRequest {
	r := &apiv1.AIRequest{
		Id: v.ID, Type: v.Type, Repo: v.Repo, Event: v.Event, Ref: v.Ref, Sha: v.SHA, Trigger: v.Trigger, Sender: v.Sender,
		State: string(v.State), PrNumber: clampInt32(v.PRNumber), CreatedAt: v.CreatedAt.Unix(), FinishedAt: unixOrZero(v.FinishedAt),
	}
	if v.RerunOf != nil {
		r.RerunOf = *v.RerunOf
	}
	if v.ConfigError != nil {
		r.ConfigError = *v.ConfigError
	}
	return r
}

func toAPIAITask(t *ai.Task, at *AITaskAttempt) *apiv1.AITask {
	v := &apiv1.AITask{
		Id: t.ID, RequestId: t.RequestID, Key: t.Key, DisplayName: t.DisplayName, Platforms: t.Platforms, Agent: t.Agent,
		State: string(t.State), AttemptCount: clampInt32(t.AttemptCount),
		StartedAt: unixOrZero(t.StartedAt), FinishedAt: unixOrZero(t.FinishedAt),
	}
	if at != nil {
		v.Runner, v.Platform, v.Stage, v.Text = at.Runner, at.Platform, at.Stage, at.Text
		v.Orca = &apiv1.AIOrca{
			RuntimeId: at.Orca.RuntimeID, RunId: at.Orca.RunID, TaskId: at.Orca.TaskID, DispatchId: at.Orca.DispatchID,
			WorktreeName: at.Orca.WorktreeName, WorktreePath: at.Orca.WorktreePath,
		}
	}
	if t.Reason != nil {
		v.Reason = *t.Reason
	}
	if t.ReusedFrom != nil {
		v.ReusedFrom = *t.ReusedFrom
	}
	return v
}

func (a *rpcAPI) ListAIRequests(ctx context.Context, req *connect.Request[apiv1.ListAIRequestsRequest]) (*connect.Response[apiv1.ListAIRequestsResponse], error) {
	m := req.Msg
	list, err := a.s.ListAIRequests(ctx, AIRequestFilter{Repo: m.Repo, Type: m.Type, PR: int(m.PrNumber), State: m.State, Limit: int(m.Limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.AIRequest, 0, len(list))
	for _, v := range list {
		out = append(out, toAPIAIRequest(v))
	}
	return connect.NewResponse(&apiv1.ListAIRequestsResponse{Requests: out}), nil
}

func (a *rpcAPI) GetAIRequest(ctx context.Context, req *connect.Request[apiv1.GetAIRequestRequest]) (*connect.Response[apiv1.GetAIRequestResponse], error) {
	d, err := a.s.AIRequest(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	resp := &apiv1.GetAIRequestResponse{Request: toAPIAIRequest(d.AIRequestView)}
	for _, t := range d.Tasks {
		resp.Tasks = append(resp.Tasks, toAPIAITask(t, d.Attempts[t.ID]))
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) CancelAIRequest(ctx context.Context, req *connect.Request[apiv1.CancelAIRequestRequest]) (*connect.Response[apiv1.CancelAIRequestResponse], error) {
	if err := a.s.CancelAIRequest(ctx, req.Msg.Id, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.CancelAIRequestResponse{}), nil
}

func (a *rpcAPI) RerunAIRequest(ctx context.Context, req *connect.Request[apiv1.RerunAIRequestRequest]) (*connect.Response[apiv1.RerunAIRequestResponse], error) {
	m := req.Msg
	r, err := a.s.RerunAIRequest(ctx, m.Id, review.RerunMode(m.Mode), m.TaskId, rpcActor(ctx), store.SourceCLI)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.RerunAIRequestResponse{Id: r.ID}), nil
}

func (a *rpcAPI) GetAITaskLog(ctx context.Context, req *connect.Request[apiv1.GetAITaskLogRequest]) (*connect.Response[apiv1.GetAITaskLogResponse], error) {
	l, err := a.s.AITaskLog(ctx, req.Msg.TaskId, req.Msg.AttemptId, req.Msg.Offset)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.GetAITaskLogResponse{Data: l.Data, NextOffset: l.Next, Finished: l.Finished}), nil
}

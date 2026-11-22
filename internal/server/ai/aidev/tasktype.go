package aidev

import (
	"context"
	"errors"
	"fmt"
	"math"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aiprompt"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 开发的任务类型（PLAN #30.7）：一次选题或一轮返修是一个请求，其中一个任务，由开发角色的 AI 执行节点领取。不建
// Check Run（不是对某个 commit 的检查），不能重跑（由执行者按工作项规划）；同一个工作项的返修在并发组中排队、只留最新的。
// 结果（交付报告、attempt 分支）由 Finish 记录并推进工作项；请求结束时通知执行者核对。

// Type 是开发的任务类型（由 Service 实现）。
type Type struct{ s *Service }

// TaskType 返回开发的任务类型（组装时注册到 ai.Registry）。
func (s *Service) TaskType() *Type { return &Type{s: s} }

var (
	_ ai.TaskType   = (*Type)(nil)
	_ ai.DevHandler = (*Service)(nil)
)

// waitReason 是开发任务排队时的说明。
const waitReason = "等待 AI 开发节点领取"

func (*Type) Name() string { return agentcfg.TaskDevelop }
func (*Type) Role() string { return store.RunnerRoleDevelopment }

func (*Type) Policy() ai.Policy {
	return ai.Policy{GroupMode: ai.GroupQueue, GitWrite: true}
}

func (*Type) WaitReason() string { return waitReason }

// developSpec 解析任务的开发内容。
func developSpec(t *ai.Task) (*aispec.DevelopSpec, error) {
	var d aispec.DevelopSpec
	if err := t.DecodeSpec(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (*Type) Services(t *ai.Task) []string {
	d, err := developSpec(t)
	if err != nil {
		return nil
	}
	return d.Services
}

// Tools 是任务在 platform 上的受管理的工具链（规划时为开发可以执行的每个平台各解析一份，PLAN #33.12）。
func (*Type) Tools(t *ai.Task, platform string) []aispec.ToolRef {
	d, err := developSpec(t)
	if err != nil {
		return nil
	}
	return d.PlatformTools[platform]
}

// Repositories 是任务冻结的依赖仓库（requirements.repositories，PLAN #33.13）。
func (*Type) Repositories(t *ai.Task) []string {
	d, err := developSpec(t)
	if err != nil {
		return nil
	}
	return d.Repositories
}

func (*Type) Limit(*ai.Task) int { return 0 }

// Mismatch 没有开发特有的排除：任何 Agent 都可以开发（开发过的 Agent 记入工作项，不参与它的 PR 的审计，PLAN #33.7）。
func (*Type) Mismatch(context.Context, pgx.Tx, *ai.Runner, []*ai.Task) (map[int64]string, error) {
	return nil, nil
}

// Spec 生成下发给节点的开发任务：工作项的数据合并成 work_data（与旧的 kiln-ai-runner 写出的 JSON 结构相同），Agent
// 的说明在 prompt 中（PLAN #33.5、#33.6）。
func (*Type) Spec(_ context.Context, _ store.Querier, in ai.SpecInput, out *airunnerv2.AITask) error {
	d, err := developSpec(in.Task)
	if err != nil {
		return &ai.SpecError{Err: err}
	}
	if len(d.PlatformTools) > 0 && len(d.PlatformTools[in.Platform]) == 0 {
		// 规划时为每个可以执行的平台都解析了工具链，缺的只能是任务本身有问题：不下发没有工具链的任务
		return &ai.SpecError{Err: fmt.Errorf("任务没有平台 %s 的受管理的工具链", in.Platform)}
	}
	out.Kind = &airunnerv2.AITask_Develop{Develop: &airunnerv2.DevTask{
		Mode: d.Mode, ConfigSha: d.ConfigSHA, MaxIssues: clampInt32(d.MaxIssues), DryRun: d.DryRun, WorkItemId: d.WorkItemID,
		Branch: d.Branch, BaseRef: d.BaseRef, PrNumber: clampInt32(d.PRNumber), ReportVersion: devreport.Version,
		Phase: d.Phase, ImplRound: clampInt32(d.ImplRound), Round: clampInt32(d.Round), WorkData: workData(d),
	}}
	shape := aiprompt.DevInput{Repository: in.Repo.FullName(), CloneURL: in.Repo.CloneURL, SHA: in.Request.SHA, Spec: d, Platform: in.Platform}
	switch d.Mode {
	case aispec.DevSelect:
		out.Prompt = aiprompt.Select(shape)
	case aispec.DevImplement:
		out.Prompt = aiprompt.Implement(shape)
	default:
		out.Prompt = aiprompt.Fix(shape)
	}
	return nil
}

// WorkData 是实现、返修时写给 Agent 的工作项数据（DevTask.work_data → 任务目录的 work.json）：与旧的 kiln-ai-runner
// 写出的 JSON 结构相同（PLAN #33.6）。
type WorkData struct {
	Issues     []WorkIssueData `json:"issues"`
	Findings   []WorkFinding   `json:"findings"`
	CIFailures []string        `json:"ci_failures"`
	Handoff    string          `json:"handoff"`
	Round      int             `json:"round"`
	// RequirementsChanged 表示领取之后 Issue 的标题或正文被改过。
	RequirementsChanged bool     `json:"requirements_changed"`
	Phase               string   `json:"phase"`
	ImplRound           int      `json:"impl_round"`
	DesignPR            int      `json:"design_pr"`
	DesignSHA           string   `json:"design_sha"`
	DesignDocuments     []string `json:"design_documents"`
	// Conflict 是返修要一起解决的合并冲突（没有时省略）。
	Conflict *WorkConflict `json:"conflict,omitempty"`
}

// WorkConflict 是返修要解决的合并冲突：PR head 与目标分支 base_ref 的 commit base_sha 冲突（合并队列更新 PR 分支时平台确认）。
type WorkConflict struct {
	Head    string `json:"head"`
	BaseRef string `json:"base_ref"`
	BaseSHA string `json:"base_sha"`
	Note    string `json:"note"`
}

// conflictWorkNote 是 work.json 中冲突的说明。
const conflictWorkNote = "合并队列把目标分支合并进 PR 分支时平台报告冲突：把目标分支（至少包含 base_sha）合并进当前分支、解决冲突，" +
	"不要 rebase、不要改写已有的提交"

// WorkIssueData 是工作项数据中的一个 Issue（需求数据）。
type WorkIssueData struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
	URL    string   `json:"url"`
}

// WorkFinding 是返修要处理的一个审计问题。
type WorkFinding struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Detail   string `json:"detail"`
}

// workData 编码工作项的数据（DevTask.work_data）。
func workData(d *aispec.DevelopSpec) []byte {
	out := WorkData{
		Issues: make([]WorkIssueData, 0, len(d.Issues)), Findings: make([]WorkFinding, 0, len(d.Findings)),
		CIFailures: append([]string{}, d.CIFailures...), Handoff: d.Handoff, Round: d.Round,
		RequirementsChanged: d.RequirementsChanged, Phase: aiPhase(d.Phase), ImplRound: d.ImplRound,
		DesignPR: d.DesignPR, DesignSHA: d.DesignSHA, DesignDocuments: append([]string{}, d.DesignDocs...),
	}
	if c := d.Conflict; c != nil {
		out.Conflict = &WorkConflict{Head: c.Head, BaseRef: c.BaseRef, BaseSHA: c.BaseSHA, Note: conflictWorkNote}
	}
	for _, i := range d.Issues {
		out.Issues = append(out.Issues, WorkIssueData{
			Number: i.Number, Title: i.Title, Body: i.Body, Labels: append([]string{}, i.Labels...), URL: i.URL,
		})
	}
	for _, f := range d.Findings {
		out.Findings = append(out.Findings, WorkFinding{
			ID: f.ID, Title: f.Title, Severity: f.Severity, Path: f.Path, Line: f.Line, Detail: f.Detail,
		})
	}
	return store.MustJSON(out)
}

// aiPhase 返回工作项的阶段（旧的任务没有阶段时是实现）。
func aiPhase(p string) string {
	if p == devreport.PhaseDesign {
		return p
	}
	return devreport.PhaseImplement
}

// Claimed 在领取实现、返修任务的事务中把执行的 Agent 记入工作项开发过的 Agent（PLAN #33.7）：执行之后失联、超时、被取消
// 时也要留下，否则这个 Agent 之后能审计这个工作项的 PR。选题在领取 Issue 时记下。
func (*Type) Claimed(ctx context.Context, tx pgx.Tx, t *ai.Task, _ *ai.Runner) error {
	d, err := developSpec(t)
	if err != nil || d.WorkItemID == 0 {
		return err
	}
	return addDevAgent(ctx, tx, d.WorkItemID, t.Agent)
}

// addDevAgent 把 Agent 记入工作项开发过的 Agent（PLAN #33.7）。
func addDevAgent(ctx context.Context, tx pgx.Tx, workItem int64, agent string) error {
	if agent == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE dev_work_items SET dev_agents=array_append(dev_agents, $2) WHERE id=$1 AND NOT ($2 = ANY(dev_agents))`,
		workItem, agent)
	return err
}

// Finish 记录执行的结果（交付报告、attempt 分支）并推进工作项，返回最终的结果（交付报告无效时改为失败）。Server 判定的
// 失联、超时也经过这里：工作项转为受阻。
func (ty *Type) Finish(ctx context.Context, tx pgx.Tx, t *ai.Task, a *ai.Attempt, res ai.Result) (ai.Result, error) {
	del := &Delivery{RunnerVersion: a.RunnerVersion, AgentVersion: res.AgentVersion, Model: res.AgentModel, Agent: t.Agent}
	if d := res.Delivery; d != nil {
		del.Report, del.Branch, del.SHA, del.Checkpoint = d.GetReport(), d.GetBranch(), d.GetSha(), d.GetCheckpoint()
	}
	return ty.s.RecordResult(ctx, tx, t, a.ID, del, res)
}

// AfterFinish 在请求结束时通知执行者核对工作项（Complete 没有推进它时转为受阻，PLAN #26.11）。
func (ty *Type) AfterFinish(ctx context.Context, tx pgx.Tx, r *ai.Request) error {
	return ty.s.Enqueue(ctx, tx, r.RepoID)
}

// ValidateReport 校验 Agent 写出的选题报告和交付报告（PLAN #33.5）：选题报告要针对这次执行拿到过的候选，交付报告要回应
// 工作项的每个 Issue 和每个要处理的问题。不改变状态。
func (ty *Type) ValidateReport(ctx context.Context, t *ai.Task, attemptID int64, kind ai.ReportKind, report []byte) ([]string, error) {
	return ty.s.ValidateReport(ctx, t, attemptID, kind, report)
}

// ValidateReport 实现任务的报告校验（由 Type.ValidateReport 转交）。
func (s *Service) ValidateReport(ctx context.Context, t *ai.Task, attemptID int64, kind ai.ReportKind, report []byte) ([]string, error) {
	d, err := developSpec(t)
	if err != nil {
		return nil, err
	}
	switch kind {
	case ai.ReportSelection:
		if d.Mode != aispec.DevSelect {
			return nil, ai.ErrUnknownReportKind
		}
		offers, err := s.offers(ctx, s.db, attemptID)
		if errors.Is(err, ErrInvalidClaim) {
			return []string{err.Error()}, nil // 还没有要过候选：选题报告无从校验
		}
		if err != nil {
			return nil, err
		}
		if _, err := parseSelection(report, offers, d.MaxIssues); err != nil {
			// 报告不合格是校验的结果（给 Agent 的问题），不是处理请求的错误
			return []string{"选题报告无效：" + err.Error()}, nil //nolint:nilerr // 见上
		}
		return nil, nil
	case ai.ReportDelivery:
		w, err := workItemOfAttempt(ctx, s.db, d, attemptID)
		if err != nil {
			return nil, err
		}
		if w == nil {
			return []string{"这次执行还没有领取工作项：先选题并领取"}, nil
		}
		exp, err := deliveryExpect(ctx, s.db, d, w)
		if err != nil {
			return nil, err
		}
		if _, err := devreport.ParseDelivery(report, exp); err != nil {
			return []string{"交付报告无效：" + err.Error()}, nil //nolint:nilerr // 报告不合格是校验的结果
		}
		return nil, nil
	}
	return nil, ai.ErrUnknownReportKind
}

// workItemOfAttempt 返回这次执行的工作项：实现、返修用任务内容中的编号，选题用领取时记下的 attempt。
func workItemOfAttempt(ctx context.Context, q store.Querier, d *aispec.DevelopSpec, attemptID int64) (*WorkItem, error) {
	if d.WorkItemID != 0 {
		return queryWorkItem(ctx, q, `id=$1`, d.WorkItemID)
	}
	return queryWorkItem(ctx, q, `claim_attempt_id=$1`, attemptID)
}

// deliveryExpect 返回校验交付报告时的期望（要回应的 Issue、要处理的问题和阶段）：Issue 来自工作项（选题任务在领取之后
// 才有），问题来自任务的内容。
func deliveryExpect(ctx context.Context, q store.Querier, d *aispec.DevelopSpec, w *WorkItem) (devreport.Expect, error) {
	is, err := workIssues(ctx, q, w.ID)
	if err != nil {
		return devreport.Expect{}, err
	}
	findings := make([]string, 0, len(d.Findings))
	for _, f := range d.Findings {
		findings = append(findings, f.ID)
	}
	return devreport.Expect{Issues: issueNumbers(is), Findings: findings, Phase: w.Phase}, nil
}

// Offer 实现 ai.DevHandler：给出选题的候选。
func (s *Service) Offer(ctx context.Context, t *ai.Task, attemptID int64) (*airunnerv2.OfferDevCandidatesResponse, error) {
	res, err := s.OfferCandidates(ctx, t, attemptID)
	if errors.Is(err, ErrInvalidClaim) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, err
	}
	return &airunnerv2.OfferDevCandidatesResponse{
		WaitSeconds: clampInt32(int64(math.Ceil(res.Wait.Seconds()))), Candidates: devIssues(res.Candidates), InProgress: devInProgress(res.InProgress),
	}, nil
}

// Claim 实现 ai.DevHandler：原子领取选中的 Issue，并给出给 Agent 的下一步说明（PLAN #33.5）。
func (s *Service) Claim(ctx context.Context, t *ai.Task, attemptID int64, req *airunnerv2.ClaimDevIssuesRequest) (*airunnerv2.ClaimDevIssuesResponse, error) {
	a, err := ai.GetAttempt(ctx, s.db, attemptID)
	if err != nil {
		return nil, err
	}
	d, err := developSpec(t)
	if err != nil {
		return nil, err
	}
	r, err := ai.GetRequest(ctx, s.db, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, s.db, r.RepoID)
	if err != nil {
		return nil, err
	}
	issues := make([]int, 0, len(req.GetIssues()))
	for _, n := range req.GetIssues() {
		issues = append(issues, int(n))
	}
	res, err := s.ClaimIssues(ctx, t, &ClaimRequest{
		AttemptID: attemptID, Issues: issues, Selection: req.GetSelection(), Agent: t.Agent, RunnerVersion: a.RunnerVersion,
	})
	if errors.Is(err, ErrInvalidClaim) {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, err
	}
	// 首次领取（选题任务在同一 attempt 中直接开始开发 / 设计）也要冻结仓库绑定的设计文档，并在领取后的说明里给出入口。
	// 之后的实现 / 返修由 planImpl / planFix 冻结（freeze-once，读到的是同一份）。
	//
	// 只有支持文档工具（协议 9 及以上，agentcfg.TaskDocMinProtocol）的连接器才注入：升级 Server 后，协议 8 的在途选题任务
	// 仍可能在这条动态回复里开始开发，它没有 kiln-orca task doc，不能把"必须读取设计依据"的说明发给它。这类任务按旧行为
	// 继续，文档在它升级后、后续的实现 / 审计规划时再冻结。
	if res.Claimed && s.runnerSupportsDoc(ctx, a) {
		docs, derr := s.docsFor(ctx, repo.ID, res.WorkItemID)
		if derr != nil {
			return nil, derr
		}
		d.Docs = docs
	}
	shape := aiprompt.DevInput{Repository: repo.FullName(), CloneURL: repo.CloneURL, SHA: r.SHA, Spec: d}
	var message string
	switch {
	case res.Claimed:
		message = aiprompt.Develop(shape, res.WorkItemID, res.Branch, res.Phase)
	case res.Conflict && len(res.Candidates) > 0:
		message = aiprompt.Reselect(shape, res.Reason)
	default:
		message = aiprompt.Ended(res.Reason)
	}
	return &airunnerv2.ClaimDevIssuesResponse{
		Claimed: res.Claimed, WorkItemId: res.WorkItemID, Branch: res.Branch, Reason: res.Reason, Candidates: devIssues(res.Candidates),
		InProgress: devInProgress(res.InProgress), Phase: res.Phase, Message: message,
	}, nil
}

// runnerSupportsDoc 判断这次 attempt 的执行节点是否支持外部设计文档工具（kiln-orca task doc）：协议 9 及以上
// （agentcfg.TaskDocMinProtocol）支持。
// 取不到节点或协议时按不支持处理（宁可这次不给文档，也不把不支持的命令发出去）。用固定的 TaskDocMinProtocol，
// 不跟着 AIProtocol 走：Server 升级协议后，升级前派发的协议 9 在途选题任务仍要能冻结、读取文档。
func (s *Service) runnerSupportsDoc(ctx context.Context, a *ai.Attempt) bool {
	if a == nil || a.RunnerID == nil {
		return false
	}
	var protocol int
	if err := s.db.QueryRow(ctx, `SELECT protocol FROM ai_runners WHERE runner_id=$1`, *a.RunnerID).Scan(&protocol); err != nil {
		return false
	}
	return protocol >= agentcfg.TaskDocMinProtocol
}

func devIssues(is []aispec.DevIssue) []*airunnerv2.DevIssue {
	out := make([]*airunnerv2.DevIssue, 0, len(is))
	for _, i := range is {
		out = append(out, &airunnerv2.DevIssue{Number: clampInt32(i.Number), Title: i.Title, Body: i.Body, Labels: i.Labels, Digest: i.Digest, Url: i.URL})
	}
	return out
}

func devInProgress(list []InProgress) []*airunnerv2.DevWorkInProgress {
	out := make([]*airunnerv2.DevWorkInProgress, 0, len(list))
	for _, w := range list {
		out = append(out, &airunnerv2.DevWorkInProgress{WorkItemId: w.ID, State: w.State, Issues: devIssues(w.Issues), Plan: w.Plan})
	}
	return out
}

// clampInt32 把整数转换为 int32，超出范围时取边界值（用于填写 protobuf 的字段）。
func clampInt32[T int | int64](n T) int32 {
	switch {
	case int64(n) > math.MaxInt32:
		return math.MaxInt32
	case int64(n) < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

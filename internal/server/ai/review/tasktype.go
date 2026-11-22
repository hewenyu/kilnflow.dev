package review

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aiprompt"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// AI 审计的任务类型（PLAN #30.7）：review.required_platforms 的每一项一个任务（一组平台时组内任一平台的节点都可以领取），
// 由审计角色的 AI 执行节点领取；结论 no_issues 才算通过，报告
// 保存到 ai_reviews，PR 审计按配置发布为 PR review；PR 审计请求结束时写 PR 验证结束的事件（合并队列、aidev）。
// 审计 Agent 在规划时选好（PLAN #33.7），开发过这个工作项的 Agent 不能再审计它的 PR。

// maxVerdictMessage 是审计未完成时结果说明中摘要的长度上限。
const maxVerdictMessage = 500

// waitReason 是审计任务排队时的说明。
const waitReason = "等待 AI 审计节点领取"

// Events 是审计请求结束时通知其他领域的事件（组装层经 outbox 转交合并队列和 aidev）。
type Events interface {
	PRVerified(ctx context.Context, q store.Execer, repoID int64, pr int, passed bool) error
}

// Type 是审计的任务类型。
type Type struct {
	outbox *outbox.Writer
	events Events
}

// NewType 返回审计的任务类型。w 写入 PR review 的发布意图，events 是请求结束的事件。
func NewType(w *outbox.Writer, events Events) *Type { return &Type{outbox: w, events: events} }

var _ ai.TaskType = (*Type)(nil)

func (*Type) Name() string { return agentcfg.TaskReview }
func (*Type) Role() string { return store.RunnerRoleReview }

func (*Type) Policy() ai.Policy {
	return ai.Policy{CheckRuns: true, Rerunnable: true, GroupMode: ai.GroupSupersede}
}

func (*Type) WaitReason() string { return waitReason }

// spec 解析任务的审计内容。
func spec(t *ai.Task) (*aispec.ReviewSpec, error) {
	var s aispec.ReviewSpec
	if err := t.DecodeSpec(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (*Type) Services(t *ai.Task) []string {
	s, err := spec(t)
	if err != nil {
		return nil
	}
	return s.Services
}

// Tools 是任务在 platform 上的受管理的工具链（规划时为每个平台各解析一份，PLAN #33.12）。
func (*Type) Tools(t *ai.Task, platform string) []aispec.ToolRef {
	s, err := spec(t)
	if err != nil {
		return nil
	}
	return s.PlatformTools[platform]
}

// Repositories 是任务冻结的依赖仓库（requirements.repositories，PLAN #33.13）。
func (*Type) Repositories(t *ai.Task) []string {
	s, err := spec(t)
	if err != nil {
		return nil
	}
	return s.Repositories
}

func (*Type) Limit(t *ai.Task) int {
	s, err := spec(t)
	if err != nil {
		return 0
	}
	return s.MaxConcurrency
}

// Mismatch 实现独立审计按 Agent（PLAN #33.7）：审计 Agent 不能是这个工作项开发时用过的 Agent（不再要求不同的机器）。
// 工作项按 PR 编号关联；PR 来自正式分支 kiln/dev-<id> 时也按分支关联（Server 先建 PR 再记下编号）。
func (*Type) Mismatch(ctx context.Context, tx pgx.Tx, _ *ai.Runner, cands []*ai.Task) (map[int64]string, error) {
	var taskIDs, prs, branchItems []int64
	agent := map[int64]string{}
	for _, t := range cands {
		s, err := spec(t)
		if err != nil || s.PRNumber == 0 {
			continue
		}
		b, _ := devreport.ParseWorkItemBranch(s.HeadRef)
		taskIDs, prs, branchItems = append(taskIDs, t.ID), append(prs, int64(s.PRNumber)), append(branchItems, b.ID)
		agent[t.ID] = s.Agent
	}
	out := map[int64]string{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT c.task_id, w.id, w.dev_agents FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS c(task_id, pr, branch_item)
		JOIN ai_tasks t ON t.id=c.task_id JOIN ai_requests q ON q.id=t.request_id
		JOIN dev_work_items w ON w.repo_id=q.repo_id AND (w.pr_number=c.pr OR w.design_pr=c.pr OR w.id=c.branch_item)`,
		taskIDs, prs, branchItems)
	if err != nil {
		return nil, err
	}
	var taskID, item int64
	var agents []string
	_, err = pgx.ForEachRow(rows, []any{&taskID, &item, &agents}, func() error {
		if a := agent[taskID]; slices.Contains(agents, a) {
			out[taskID] = fmt.Sprintf("这是工作项 #%d 的 PR，审计 Agent（%s）与开发 Agent 相同，需要配置另一个审计 Agent"+
				"（.kiln/agent/config.yml 的 review.agent，可以写一个列表）", item, agentcfg.AgentName(a))
		}
		return nil
	})
	return out, err
}

// Spec 生成下发给节点的审计任务：规则（说明、上一轮的问题、审计方式）在 prompt 中，Server 生成一次（PLAN #33.5）。
func (*Type) Spec(_ context.Context, _ store.Querier, in ai.SpecInput, out *airunnerv2.AITask) error {
	s, err := spec(in.Task)
	if err != nil {
		return &ai.SpecError{Err: err}
	}
	if len(s.PlatformTools) > 0 && len(s.PlatformTools[in.Platform]) == 0 {
		// 规划时为每个可以执行的平台都解析了工具链，缺的只能是任务本身有问题：不下发没有工具链的任务
		return &ai.SpecError{Err: fmt.Errorf("任务没有平台 %s 的受管理的工具链", in.Platform)}
	}
	out.Kind = &airunnerv2.AITask_Review{Review: &airunnerv2.ReviewTask{
		Trigger: s.Trigger, PrNumber: int32(min(s.PRNumber, maxInt32)), BaseRef: s.BaseRef, BaseSha: s.BaseSHA, //nolint:gosec // 已限制范围
		HeadRef: s.HeadRef, ReportVersion: aireport.Version, WorkItemId: s.WorkItemID, Phase: s.Phase,
	}}
	out.Prompt = aiprompt.Review(aiprompt.ReviewInput{
		Repository: in.Repo.FullName(), CloneURL: in.Repo.CloneURL, SHA: in.Request.SHA,
		Platform: in.Platform, Spec: s,
	})
	return nil
}

// ValidateReport 校验审计报告（aireport）：必填字段、字段取值和证据格式。不改变状态。
func (*Type) ValidateReport(_ context.Context, _ *ai.Task, _ int64, kind ai.ReportKind, report []byte) ([]string, error) {
	if kind != ai.ReportReview {
		return nil, ai.ErrUnknownReportKind
	}
	if _, err := aireport.ParseAgent(report); err != nil {
		return []string{err.Error()}, nil //nolint:nilerr // 报告不合格是校验的结果（给 Agent 的问题）
	}
	return nil, nil
}

// maxInt32 是 protobuf int32 字段能表示的最大值。
const maxInt32 = 1<<31 - 1

func (*Type) Claimed(context.Context, pgx.Tx, *ai.Task, *ai.Runner) error { return nil }

// Finish 按审计的结果规则（PLAN #30.6）处理结果：Runner 报告成功（Agent 写出了报告）时校验报告、保存到 ai_reviews，
// 按结论决定任务的结果——没有发现问题才算通过；发现问题、审计未完成或报告无效都不通过。其他结果原样返回。
func (ty *Type) Finish(ctx context.Context, tx pgx.Tx, t *ai.Task, a *ai.Attempt, res ai.Result) (ai.Result, error) {
	if res.Outcome != store.OutcomeSucceeded {
		return res, nil
	}
	if len(res.ReviewReport) == 0 {
		return res.Failed(ai.ErrClassReport, "AI 执行节点没有提交审计报告"), nil
	}
	rep, err := aireport.ParseAgent(res.ReviewReport)
	if err != nil {
		// 报告无效是审计的结果（任务失败），不是处理 Complete 的错误
		return res.Failed(ai.ErrClassReport, "审计报告无效："+err.Error()), nil //nolint:nilerr // 见上
	}
	// 报告来自 Agent：保存前去掉残留的本机路径（AI 执行节点已经替换了工作区路径，这里兜底）
	rep = rep.Rewrite(aireport.ScrubLocalPaths)
	switch rep.Verdict {
	case aireport.VerdictIssuesFound:
		res = res.Failed(ai.ErrClassFindings, fmt.Sprintf("AI 审计发现 %d 个问题", len(rep.Findings)))
	case aireport.VerdictIncomplete:
		res = res.Failed(ai.ErrClassIncomplete, "AI 审计未完成："+shorten.Text(rep.Summary, maxVerdictMessage))
	}
	return res, ty.save(ctx, tx, t, a, res, rep)
}

// save 保存审计报告和它的适用范围（审计规则、说明来源、Runner / Agent 版本、模型，PLAN #26.14）；PR 审计并且配置了
// review 评论时写入发布 PR review 的意图。
func (ty *Type) save(ctx context.Context, tx pgx.Tx, t *ai.Task, a *ai.Attempt, res ai.Result, rep *aireport.Report) error {
	s, err := spec(t)
	if err != nil {
		return err
	}
	r, err := ai.GetRequest(ctx, tx, t.RequestID, false)
	if err != nil {
		return err
	}
	var pr *int
	if s.PRNumber != 0 {
		pr = &s.PRNumber
	}
	publish := s.ReviewComments && pr != nil
	state := StateNone
	if publish {
		state = StatePending
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ai_reviews (task_id, repo_id, attempt_id, pr_number, head_sha, base_sha, agent, verdict, summary,
		findings, report, review_state, policy_digest, instructions_from, runner_version, agent_version, model, work_item_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,nullif($18, 0))
		ON CONFLICT (task_id) DO UPDATE SET attempt_id=EXCLUDED.attempt_id, verdict=EXCLUDED.verdict, summary=EXCLUDED.summary,
		  findings=EXCLUDED.findings, report=EXCLUDED.report, review_state=EXCLUDED.review_state, review_error=NULL, created_at=now(),
		  runner_version=EXCLUDED.runner_version, agent_version=EXCLUDED.agent_version, model=EXCLUDED.model`,
		t.ID, r.RepoID, a.ID, pr, r.SHA, s.BaseSHA, s.Agent, rep.Verdict, rep.Summary, len(rep.Findings), rep.Marshal(), state,
		s.PolicyDigest, s.InstructionsFrom, a.RunnerVersion, res.AgentVersion, res.AgentModel, s.WorkItemID); err != nil {
		return fmt.Errorf("保存 AI 审计报告：%w", err)
	}
	if !publish {
		return nil
	}
	return ty.outbox.Enqueue(ctx, tx, prReviewKey(t.ID), KindPRReview, prReviewPayload{TaskID: t.ID})
}

// AfterFinish 在 PR 审计请求结束时写 PR 验证结束的事件（通过 = succeeded 或 skipped）。
func (ty *Type) AfterFinish(ctx context.Context, tx pgx.Tx, r *ai.Request) error {
	pr, err := requestPR(ctx, tx, r)
	if err != nil || pr == 0 {
		return err
	}
	return ty.events.PRVerified(ctx, tx, r.RepoID, pr, r.State == ai.RequestSucceeded || r.State == ai.RequestSkipped)
}

// requestPR 返回 PR 审计请求的 PR 编号（手动审计为 0）。
func requestPR(ctx context.Context, q store.Querier, r *ai.Request) (int, error) {
	if r.Event != eventPullRequest {
		return 0, nil
	}
	var pr *int
	err := q.QueryRow(ctx, `SELECT pr_number FROM ai_review_requests WHERE request_id=$1`, r.ID).Scan(&pr)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if pr == nil {
		return 0, nil
	}
	return *pr, nil
}

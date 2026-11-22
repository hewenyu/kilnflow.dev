package aidev

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 开发的任务类型（PLAN #30.7）：规则、领取条件、下发内容和结束钩子。

// TestTaskTypePolicy：开发不建 Check Run、不能重跑、基础设施错误不自动重试；返修在并发组中排队。基础设施错误结束的执行
// 转为受阻（不重新排队）。
func TestTaskTypePolicy(t *testing.T) {
	h := newHarness(t, devConfigYML)
	pol := h.svc.TaskType().Policy()
	if pol.CheckRuns || pol.Rerunnable || pol.InfraRetries != 0 || pol.GroupMode != ai.GroupQueue {
		t.Fatalf("规则 = %+v", pol)
	}
	h.issue(1, "一", "a")
	w, aid := h.claimed(1)
	if out := h.complete(aid, store.OutcomeInfraError, nil); out.Outcome != store.OutcomeInfraError {
		t.Fatalf("结果 = %+v", out)
	}
	if task := h.task(*w.TaskID); task.State != ai.TaskFailed {
		t.Fatalf("基础设施错误后任务 = %s，想要 failed（不重试）", task.State)
	}
	h.wantState(w.ID, StateBlocked)
}

// TestTaskTypeMismatch：开发不按机器排除（PLAN #33.7）：任何节点都能领取开发任务，开发过的 Agent 记入工作项。
func TestTaskTypeMismatch(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	task := h.taskOf(h.startSelect())
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		got, err := h.svc.TaskType().Mismatch(h.ctx, tx, &ai.Runner{ID: h.runnerID}, []*ai.Task{task})
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Errorf("开发不应该被排除：%v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTaskTypeSpec：下发的开发任务带上返修的内容（工作项、轮次、问题、CI 失败）和报告格式版本。
func TestTaskTypeSpec(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	review := h.failingRun(pr, w.HeadSHA)
	h.mustProcess()
	task := h.fixTask(h.wantState(w.ID, StateFixing))
	req, err := ai.GetRequest(h.ctx, h.db, task.RequestID, false)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.GetRepo(h.ctx, h.db, req.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	var out airunnerv2.AITask
	if err := h.svc.TaskType().Spec(h.ctx, h.db, ai.SpecInput{Task: task, Request: req, Repo: repo}, &out); err != nil {
		t.Fatal(err)
	}
	dt := out.GetDevelop()
	if dt == nil || dt.GetMode() != aispec.DevFix || dt.GetWorkItemId() != w.ID || dt.GetRound() != 1 || int(dt.GetPrNumber()) != pr ||
		dt.GetReportVersion() != devreport.Version {
		t.Fatalf("下发的开发任务 = %v", dt)
	}
	// 工作项的数据在 work_data 中（连接器原样写成任务目录的 work.json）
	var work WorkData
	if err := json.Unmarshal(dt.GetWorkData(), &work); err != nil {
		t.Fatal(err)
	}
	if len(work.Findings) != 1 || work.Findings[0].ID != FindingID(review, 0) || len(work.CIFailures) != 1 || len(work.Issues) != 1 ||
		work.Issues[0].Number != 1 || work.Round != 1 {
		t.Fatalf("工作项数据 = %+v", work)
	}
	// 说明在 prompt 中（占位符由连接器替换），Server 不把 Issue 的正文写进 prompt
	if p := out.GetPrompt(); !strings.Contains(p, "{{KILN_TASK_DIR}}/work.json") || !strings.Contains(p, "{{KILN_TOOL}} deliver") ||
		strings.Contains(p, "要做 a") {
		t.Errorf("prompt = %q", p)
	}
	// 仓库的开发说明（默认分支的 .kiln/agent/development.md，冻结在任务中）写进 prompt
	if p := out.GetPrompt(); !strings.Contains(p, "## 仓库的开发说明（.kiln/agent/development.md，来自默认分支）\n\n开发说明\n") {
		t.Errorf("prompt 中没有仓库的开发说明：%q", p)
	}
	if h.svc.TaskType().WaitReason() != waitReason || h.svc.TaskType().Limit(task) != 0 {
		t.Fatal("排队说明或并发上限")
	}

	// 工具链按平台冻结（PLAN #33.12）：下发给哪个平台，prompt 就写哪个平台；任务中没有这个平台的工具链时不下发
	spec := h.specOf(task.ID)
	spec.PlatformTools = map[string][]aispec.ToolRef{testDevPlatform: {{Name: "go", Version: "1.27.3", Platform: testDevPlatform, Digest: "sha256:aa"}}}
	frozen := *task
	if frozen.Spec, err = json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
	in := ai.SpecInput{Task: &frozen, Request: req, Repo: repo, Platform: testDevPlatform}
	if err := h.svc.TaskType().Spec(h.ctx, h.db, in, &out); err != nil || !strings.Contains(out.GetPrompt(), "执行平台："+testDevPlatform) ||
		!strings.Contains(out.GetPrompt(), "受管理的工具链：go 1.27.3") {
		t.Errorf("Spec = %v，prompt 中应写明执行平台和它的工具链：%q", err, out.GetPrompt())
	}
	in.Platform = "linux/amd64"
	var se *ai.SpecError
	if err := h.svc.TaskType().Spec(h.ctx, h.db, in, &out); !errors.As(err, &se) || !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("没有 linux/amd64 的工具链时 Spec = %v，应为写明平台的 SpecError", err)
	}
}

// TestValidateReport：选题报告要针对这次执行拿到过的候选；交付报告要回应工作项的 Issue。都不改变状态（PLAN #33.5）。
func TestValidateReport(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	task := h.taskOf(h.startSelect())
	aid := h.acquire(task.ID)
	ty := h.svc.TaskType()

	// 还没有要过候选：选题报告无从校验
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportSelection, selection([]int{1}, nil, nil)); err != nil || len(problems) == 0 {
		t.Fatalf("没有候选：%v，%v", problems, err)
	}
	h.offer(aid)
	// 候选 #3 不存在于这次执行拿到的候选
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportSelection, selection([]int{3}, nil, nil)); err != nil || len(problems) == 0 {
		t.Fatalf("不在候选中的 Issue：%v，%v", problems, err)
	}
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportSelection, selection([]int{1}, []int{2}, nil)); err != nil || len(problems) != 0 {
		t.Fatalf("合法的选题报告：%v，%v", problems, err)
	}
	// 还不要交付报告：这一轮只选题
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportDelivery, delivered([]int{1}, "abc")); err != nil || len(problems) != 1 {
		t.Fatalf("选题任务没有工作项：%v，%v", problems, err)
	}
	if _, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportReview, []byte("{}")); !errors.Is(err, ai.ErrUnknownReportKind) {
		t.Errorf("审计报告不属于开发任务：%v", err)
	}

	// 领取之后：交付报告要回应工作项的 Issue
	res, err := h.svc.Claim(h.ctx, task, aid, &airunnerv2.ClaimDevIssuesRequest{Issues: []int32{1}, Selection: selection([]int{1}, []int{2}, nil)})
	if err != nil || !res.GetClaimed() {
		t.Fatalf("领取：%v，%v", res, err)
	}
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportDelivery, delivered([]int{1}, "abc")); err != nil || len(problems) != 0 {
		t.Fatalf("合法的交付报告：%v，%v", problems, err)
	}
	// 没有回应工作项的 Issue
	if problems, err := ty.ValidateReport(h.ctx, task, aid, ai.ReportDelivery, delivered([]int{2}, "abc")); err != nil || len(problems) == 0 {
		t.Fatalf("缺少 Issue：%v，%v", problems, err)
	}
}

// TestAfterFinish：选题请求没有领取就结束时，结束钩子通知执行者（结束这一轮选题，PLAN #26.11）。
func TestAfterFinish(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	h.exec(`DELETE FROM outbox`)
	h.complete(aid, store.OutcomeFailed, nil)
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1 AND dedupe_key=$2`, KindRepo, repoKey(testRepoID)); n != 1 {
		t.Fatalf("选题请求结束后没有通知执行者（%d 条）", n)
	}
}

// TestDevHandler：OfferDevCandidates、ClaimDevIssues 的处理：无效的领取是 InvalidArgument（Runner 不重试），领取的机器
// 取自 attempt。
func TestDevHandler(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	task := h.taskOf(h.startSelect())
	aid := h.acquire(task.ID)
	task = h.task(task.ID)
	h.settle()
	if _, err := h.svc.Claim(h.ctx, task, aid, &airunnerv2.ClaimDevIssuesRequest{Issues: []int32{1}, Selection: selection([]int{1}, nil, nil)}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("还没有要过候选：err = %v", err)
	}
	offer, err := h.svc.Offer(h.ctx, task, aid)
	if err != nil || offer.GetWaitSeconds() != 0 || len(offer.GetCandidates()) != 1 || offer.GetCandidates()[0].GetNumber() != 1 {
		t.Fatalf("候选 = %v, %v", offer, err)
	}
	res, err := h.svc.Claim(h.ctx, task, aid, &airunnerv2.ClaimDevIssuesRequest{Issues: []int32{1}, Selection: selection([]int{1}, nil, nil)})
	if err != nil || !res.GetClaimed() || res.GetBranch() != devreport.WorkItemBranch(res.GetWorkItemId(), devreport.PhaseImplement, 1) {
		t.Fatalf("领取 = %v, %v", res, err)
	}
	if w := h.workItem(res.GetWorkItemId()); !slices.Equal(w.DevAgents, []string{devConfigAgent}) {
		t.Fatalf("开发过的 Agent = %v", w.DevAgents)
	}
	// 领取成功的下一步说明由 Server 生成（PLAN #33.5）：说明分支、交付方式和交付报告的校验
	if m := res.GetMessage(); !strings.HasPrefix(m, "领取成功") || !strings.Contains(m, res.GetBranch()) ||
		!strings.Contains(m, "{{KILN_TASK_DIR}}/delivery.json") || !strings.Contains(m, "{{KILN_TOOL}} deliver") {
		t.Fatalf("领取成功的说明 = %q", m)
	}
}

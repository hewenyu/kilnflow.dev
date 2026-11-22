package aidev

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自主设计（PLAN #31）：先设计的工作项先交付只改 Markdown 的设计 PR，合并后同一个工作项开始实现；部分完成的实现 PR
// 合并后受阻，retry 开始下一轮实现；声明改已确认的决定时加 kiln:hold，人的确认不延续到下一个 PR。

const designDoc = "docs/design.md"

// report 返回交付报告：issues 的完成度都是 res（设计阶段写 none），edit 可以再改。
func report(issues []int, res string, edit func(d *devreport.Delivery)) []byte {
	d := devreport.Delivery{
		Version: devreport.Version, Status: devreport.StatusDelivered, Summary: "交付", PRTitle: "实现功能",
		Handoff: devreport.Handoff{Remaining: "按状态筛选"},
	}
	for _, n := range issues {
		is := devreport.IssueResult{Number: n, Resolution: res, Acceptance: []devreport.AcceptanceItem{{Item: "能用"}}}
		switch res {
		case devreport.ResolutionFull:
			is.Acceptance[0] = devreport.AcceptanceItem{Item: "能用", Done: true, Evidence: "单元测试"}
		case devreport.ResolutionPartial:
			is.Acceptance = append(is.Acceptance, devreport.AcceptanceItem{Item: "显示历史", Done: true, Evidence: "单元测试"})
		}
		d.Issues = append(d.Issues, is)
	}
	if edit != nil {
		edit(&d)
	}
	return devreport.Marshal(&d)
}

// designReport 返回设计阶段的交付报告：设计文档是 docs。
func designReport(issues []int, docs ...string) []byte {
	return report(issues, devreport.ResolutionNone, func(d *devreport.Delivery) {
		d.PRTitle = "升级历史的设计"
		d.Design.Documents = docs
	})
}

// deliverTask 在任务 taskID 上交付：领取、从 from 推送 files 到 attempt 分支、提交报告 rep，再处理一次。
func (h *harness) deliverTask(taskID int64, from string, files map[string]string, rep []byte) {
	h.t.Helper()
	aid := h.acquire(taskID)
	h.deliverOn(aid, from, files, rep)
}

func (h *harness) deliverOn(aid int64, from string, files map[string]string, rep []byte) {
	h.t.Helper()
	branch := devreport.AttemptBranch(aid)
	sha := h.push(branch, from, files)
	if out := h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: rep, Branch: branch, SHA: sha}); out.Outcome != store.OutcomeSucceeded {
		h.t.Fatalf("交付的结果 = %+v", out)
	}
	h.mustProcess()
}

// claimDesign 选题领取 issues，Agent 选择先设计；返回领取结果和 attempt。
func (h *harness) claimDesign(issues ...int) (*ClaimResult, int64) {
	h.t.Helper()
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res, err := h.claimSel(aid, issues, selectionAs(true, issues, nil, nil))
	if err != nil || !res.Claimed {
		h.t.Fatalf("领取：%+v, %v", res, err)
	}
	return res, aid
}

// merge 模拟 PR 合并：把 files 提交到默认分支（squash），发送合并事件。返回合并的 commit 和事件的说明。
func (h *harness) merge(pr int, headRef string, files map[string]string) (string, string) {
	h.t.Helper()
	sha := h.commitOn("main", files, "合并")
	msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: pr, HeadRef: headRef, Merged: true, MergeSHA: sha}, "bob")
	if err != nil {
		h.t.Fatal(err)
	}
	return sha, msg
}

func (h *harness) pull(n int) fakegithub.Pull {
	h.t.Helper()
	pr, ok := h.fake.GetPull(h.fakeRepo(), n)
	if !ok {
		h.t.Fatalf("PR #%d 不存在", n)
	}
	return pr
}

func (h *harness) developRequests() int64 {
	return h.queryInt(`SELECT count(*) FROM ai_requests WHERE type=$1`, agentcfg.TaskDevelop)
}

// TestDesignFirstFlow：先设计 → 设计 PR（只改 Markdown，Refs、不打 tag）→ 合并（重复的事件只生效一次）→ 规划实现任务
// （带已合并的设计，从默认分支开始）→ 实现 PR（Fixes）→ 合并结束。
func TestDesignFirstFlow(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	w, res, designPR := h.designPROpen()
	w, mergeSHA := h.mergeDesign(w, res, designPR)
	w = h.wantImplPlanned(w, designPR, mergeSHA)

	h.deliverTask(*w.TaskID, mergeSHA, map[string]string{"feature.go": "package demo\n"}, delivered([]int{1}, ""))
	w = h.wantState(w.ID, StatePROpen)
	implPR := *w.PRNumber
	pr := h.pull(implPR)
	if implPR == designPR || pr.HeadRef != w.Branch() || strings.HasPrefix(pr.Title, designTitlePrefix) || slices.Contains(pr.Labels, testSkipLabel) ||
		!strings.Contains(pr.Body, "Fixes #1") {
		t.Fatalf("实现 PR = %+v", pr)
	}
	if _, msg := h.merge(implPR, w.Branch(), map[string]string{"feature.go": "package demo\n"}); !strings.Contains(msg, "已合并") {
		t.Fatalf("实现合并：%q", msg)
	}
	h.wantState(w.ID, StateMerged)
}

// designPROpen 领取 #1（先设计），交付设计，确认设计 PR：设计分支、标题前缀、release:skip、只写 Refs。返回工作项、
// 领取结果和设计 PR。
func (h *harness) designPROpen() (*WorkItem, *ClaimResult, int) {
	t := h.t
	t.Helper()
	res, aid := h.claimDesign(1)
	if res.Phase != PhaseDesign || res.Branch != "kiln/design-"+itoa(res.WorkItemID) {
		t.Fatalf("领取结果 = %+v", res)
	}
	// 重复的领取请求（响应丢失后重试）得到同一个阶段和分支
	if again, err := h.claimSel(aid, []int{1}, selectionAs(true, []int{1}, nil, nil)); err != nil || again.Phase != PhaseDesign || again.Branch != res.Branch {
		t.Fatalf("重复领取 = %+v, %v", again, err)
	}
	w := h.wantState(res.WorkItemID, StateDeveloping)
	if w.Phase != PhaseDesign || w.Branch() != res.Branch || !w.DesignFirst() {
		t.Fatalf("工作项 = %s", jsonString(w))
	}
	h.deliverOn(aid, w.BaseSHA, map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w = h.wantState(w.ID, StatePROpen)
	pr := h.pull(*w.PRNumber)
	if pr.HeadRef != res.Branch || !strings.HasPrefix(pr.Title, designTitlePrefix) || !slices.Contains(pr.Labels, testSkipLabel) ||
		slices.Contains(pr.Labels, mergecfg.HoldLabel) || !strings.Contains(pr.Body, "Refs #1") || strings.Contains(pr.Body, "Fixes #1") {
		t.Fatalf("设计 PR = %+v", pr)
	}
	return w, res, *w.PRNumber
}

// mergeDesign 合并设计 PR：工作项进入实现（新的轮次），重复的合并事件不再转移。返回工作项和合并得到的 commit。
func (h *harness) mergeDesign(w *WorkItem, res *ClaimResult, designPR int) (*WorkItem, string) {
	t := h.t
	t.Helper()
	mergeSHA, msg := h.merge(designPR, res.Branch, map[string]string{designDoc: "# 设计\n"})
	if !strings.Contains(msg, "开始实现") {
		t.Fatalf("合并的说明 = %q", msg)
	}
	w = h.wantState(w.ID, StateDeveloping)
	if w.Phase != PhaseImplement || w.DesignPR == nil || *w.DesignPR != designPR || w.DesignSHA != mergeSHA || w.PRNumber != nil ||
		w.TaskID != nil || w.RoundRan || w.ImplRound != 1 || w.Branch() != "kiln/dev-"+itoa(w.ID) {
		t.Fatalf("设计合并后 = %s", jsonString(w))
	}
	// 重复的合并事件（webhook 重投、按分支找到的）不再转移
	for _, cp := range []ClosedPull{{Number: designPR, HeadRef: res.Branch, Merged: true}, {Number: designPR, Merged: true}} {
		if msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, cp, "bob"); err != nil || msg != "" {
			t.Fatalf("重复的合并事件：%q, %v", msg, err)
		}
	}
	if again := h.workItem(w.ID); again.Phase != PhaseImplement || again.State != StateDeveloping || *again.DesignPR != designPR {
		t.Fatalf("重复的事件后 = %s", jsonString(again))
	}
	return w, mergeSHA
}

// wantImplPlanned 处理一次，确认规划了实现任务：带已合并的设计，从合并得到的 commit 开始；再处理不重复规划。
func (h *harness) wantImplPlanned(w *WorkItem, designPR int, mergeSHA string) *WorkItem {
	t := h.t
	t.Helper()
	h.mustProcess()
	w = h.workItem(w.ID)
	if w.TaskID == nil || !w.RoundRan || w.BaseSHA != mergeSHA {
		t.Fatalf("规划实现后 = %s", jsonString(w))
	}
	spec := h.specOf(*w.TaskID)
	if spec.Mode != aispec.DevImplement || spec.Phase != PhaseImplement || spec.Branch != w.Branch() || spec.DesignPR != designPR ||
		spec.DesignSHA != mergeSHA || !slices.Equal(spec.DesignDocs, []string{designDoc}) || spec.StartSHA != mergeSHA || spec.ImplRound != 1 {
		t.Fatalf("实现 spec = %s", jsonString(spec))
	}
	if run := h.request(h.task(*w.TaskID).RequestID); run.SHA != mergeSHA {
		t.Fatalf("实现从 %s 开始，想要已合并的设计 %s", run.SHA, mergeSHA)
	}
	h.mustProcess()
	if n := h.developRequests(); n != 2 {
		t.Fatalf("AI 开发的请求有 %d 个，想要 2 个（选题、实现）", n)
	}
	return w
}

// TestDesignViolations：设计阶段的交付改了 Markdown 以外的文件、列出的设计文档不在改动中时受阻；一个 PR 完成时列出的
// 设计文档也要在改动中。
func TestDesignViolations(t *testing.T) {
	cases := []struct {
		name   string
		design bool
		files  map[string]string
		rep    []byte
		want   string
	}{
		{"设计改了代码", true, map[string]string{designDoc: "x\n", "a.go": "package a\n"}, designReport([]int{1}, designDoc), "只能改 Markdown，改了 a.go"},
		{"设计文档不在改动中", true, map[string]string{"docs/other.md": "x\n"}, designReport([]int{1}, designDoc), "设计文档不在改动中：" + designDoc},
		{
			"一个 PR 完成时设计文档不在改动中", false,
			map[string]string{"a.go": "package a\n"},
			report([]int{1}, devreport.ResolutionFull, func(d *devreport.Delivery) { d.Design.Documents = []string{designDoc} }), "不在改动中",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "升级历史", "显示升级历史")
			aid := h.acquire(h.taskOf(h.startSelect()).ID)
			res, err := h.claimSel(aid, []int{1}, selectionAs(c.design, []int{1}, nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			h.deliverOn(aid, h.branchHead("main"), c.files, c.rep)
			w := h.wantState(res.WorkItemID, StateBlocked)
			if !strings.Contains(w.Reason, c.want) || w.PRNumber != nil || h.branchHead(w.Branch()) != "" {
				t.Fatalf("受阻原因 %q（想要 %q），PR %v", w.Reason, c.want, w.PRNumber)
			}
		})
	}
}

// TestPartialMergeNextRound：一个 PR 完成的工作项部分完成：声明改已确认的决定的 PR 加 kiln:hold；合并后受阻（原因写剩余
// 和外部输入），旧 PR 的事件不再影响工作项；retry 开始第 2 轮实现：新的分支、从最新的默认分支开始、本轮返修从零算、
// 上一个 PR 的 hold 不延续。
func TestPartialMergeNextRound(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w, round1, pr1 := h.partialWithDecision()
	if _, msg := h.merge(pr1, round1, map[string]string{"feature.go": "package demo\n"}); !strings.Contains(msg, "没有完整解决") {
		t.Fatalf("部分完成的合并：%q", msg)
	}
	w = h.wantNextRoundBlocked(w, pr1, round1)

	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	w = h.wantState(w.ID, StateDeveloping)
	if w.RoundFixes() != 0 || !strings.Contains(w.Reason, "开始第 2 轮实现") {
		t.Fatalf("retry 后 = %s（本轮返修 %d）", jsonString(w), w.RoundFixes())
	}
	// 第 2 轮还没有 PR：第 1 轮分支上的 PR 关闭（没有合并）不能终止它
	if msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: pr1 + 1, HeadRef: round1}, "bob"); err != nil || msg != "" {
		t.Fatalf("第 1 轮分支上的 PR：%q, %v", msg, err)
	}
	h.wantState(w.ID, StateDeveloping)
	h.mustProcess()
	w = h.workItem(w.ID)
	main := h.branchHead("main")
	spec := h.specOf(*w.TaskID)
	if spec.Mode != aispec.DevImplement || spec.ImplRound != 2 || spec.Branch != "kiln/dev-"+itoa(w.ID)+"-r2" || spec.StartSHA != main ||
		spec.DesignPR != 0 || w.BaseSHA != main {
		t.Fatalf("第 2 轮的 spec = %s", jsonString(spec))
	}

	h.deliverTask(*w.TaskID, main, map[string]string{"feature.go": "package demo\n\n// 第 2 轮\n"}, delivered([]int{1}, ""))
	w = h.wantState(w.ID, StatePROpen)
	if p := h.pull(*w.PRNumber); *w.PRNumber == pr1 || p.HeadRef != spec.Branch || w.HoldLabel || slices.Contains(p.Labels, mergecfg.HoldLabel) ||
		!strings.Contains(p.Body, "Fixes #1") {
		t.Fatalf("第 2 轮 PR = %+v，hold %v", p, w.HoldLabel)
	}
	// 本轮的返修上限从零算：第 1 轮用掉的不算
	h.failingRun(*w.PRNumber, w.HeadSHA)
	h.mustProcess()
	if w = h.wantState(w.ID, StateFixing); w.RoundFixes() != 1 {
		t.Fatalf("第 2 轮的返修 = %d", w.RoundFixes())
	}
}

// partialWithDecision 一个 PR 完成、部分完成的第 1 轮：交付声明要改已确认的决定（PR 加 kiln:hold）和外部输入，用掉了
// 返修轮数。返回 PR 打开的工作项、第 1 轮的分支和 PR。
func (h *harness) partialWithDecision() (*WorkItem, string, int) {
	t := h.t
	t.Helper()
	h.issue(1, "升级历史", "显示升级历史")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	partial := report([]int{1}, devreport.ResolutionPartial, func(d *devreport.Delivery) {
		d.Design.Decisions = []devreport.Decision{{Category: devreport.DecisionConfirmed, Ref: "C19", Choice: "改成异步发评论"}}
		d.Design.ExternalInputs = []devreport.ExternalInput{{What: "一台 Linux Runner", Blocks: "真实升级验证", Resume: "注册后 retry"}}
	})
	h.deliverOn(aid, h.branchHead("main"), map[string]string{"feature.go": "package demo\n"}, partial)
	w := h.wantState(res.WorkItemID, StatePROpen)
	if p := h.pull(*w.PRNumber); !w.HoldLabel || !slices.Contains(p.Labels, mergecfg.HoldLabel) || !strings.Contains(p.Body, "C19") ||
		strings.Contains(p.Body, "Fixes #1") {
		t.Fatalf("第 1 轮 PR = %+v，hold %v", p, w.HoldLabel)
	}
	h.exec(`UPDATE dev_work_items SET fix_rounds=2 WHERE id=$1`, w.ID)
	return w, w.Branch(), *w.PRNumber
}

// wantNextRoundBlocked 确认部分完成的合并之后：受阻、开始了第 2 轮（清空 PR 和 hold、本轮返修从零算），原因写剩余和
// 外部输入，Issue 仍被占用；受阻时不规划实现；第 1 轮 PR 的事件重投不起作用。
func (h *harness) wantNextRoundBlocked(w *WorkItem, pr1 int, round1 string) *WorkItem {
	t := h.t
	t.Helper()
	w = h.wantState(w.ID, StateBlocked)
	if w.Phase != PhaseImplement || w.ImplRound != 2 || w.RoundRan || w.PRNumber != nil || w.HoldLabel || w.RoundFixes() != 0 ||
		!strings.Contains(w.Reason, "按状态筛选") || !strings.Contains(w.Reason, "一台 Linux Runner") {
		t.Fatalf("部分完成后 = %s", jsonString(w))
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_item_issues WHERE work_item_id=$1 AND active`, w.ID); n != 1 {
		t.Fatal("部分完成后 Issue 不能释放")
	}
	h.mustProcess()
	if w = h.workItem(w.ID); w.TaskID != nil || h.developRequests() != 1 {
		t.Fatalf("受阻时规划了任务：%s", jsonString(w))
	}
	for _, cp := range []ClosedPull{{Number: pr1, HeadRef: round1, Merged: true}, {Number: pr1, HeadRef: round1}} {
		if msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, cp, "bob"); err != nil || msg != "" {
			t.Fatalf("旧 PR 的事件：%q, %v", msg, err)
		}
	}
	return w
}

// TestRetryBeforeAnyTaskRanStartsSameRound：设计合并后规划实现失败（受阻、本轮没有执行过任务）时，retry 开始本轮的实现，
// 而不是返修。
func TestRetryBeforeAnyTaskRanStartsSameRound(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	res, aid := h.claimDesign(1)
	h.deliverOn(aid, h.branchHead("main"), map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w := h.wantState(res.WorkItemID, StatePROpen)
	// 合并时默认分支上的配置被删掉：规划实现时受阻
	h.merge(*w.PRNumber, res.Branch, map[string]string{".kiln/agent/config.yml": "version: 1\n"})
	h.mustProcess()
	w = h.wantState(w.ID, StateBlocked)
	if w.RoundRan || w.TaskID != nil {
		t.Fatalf("规划失败后 = %s", jsonString(w))
	}
	h.commitOn("main", map[string]string{".kiln/agent/config.yml": devConfigYML}, "恢复配置")
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	w = h.wantState(w.ID, StateDeveloping)
	h.mustProcess()
	w = h.workItem(w.ID)
	if w.TaskID == nil || w.ImplRound != 1 || w.RoundFixes() != 0 || h.specOf(*w.TaskID).Mode != aispec.DevImplement {
		t.Fatalf("retry 后 = %s", jsonString(w))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// partialRound 走完一个 PR 完成、部分完成的第 1 轮：返回合并后受阻（第 2 轮还没开始）的工作项。
func (h *harness) partialRound() *WorkItem {
	h.t.Helper()
	h.issue(1, "升级历史", "显示升级历史")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	h.deliverOn(aid, h.branchHead("main"), map[string]string{"feature.go": "package demo\n"}, report([]int{1}, devreport.ResolutionPartial, nil))
	w := h.wantState(res.WorkItemID, StatePROpen)
	h.merge(*w.PRNumber, w.Branch(), map[string]string{"feature.go": "package demo\n"})
	return h.wantState(w.ID, StateBlocked)
}

// TestCheckpointPerRound：retry 只用本轮任务保存的 checkpoint：第 2 轮的实现中断、没有保存 checkpoint 时，返修从第 2 轮的
// 起点开始，不用第 1 轮的 checkpoint。
func TestCheckpointPerRound(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.partialRound()
	old := h.push("kiln/attempt-old", h.branchHead("main"), map[string]string{"old.go": "package demo\n"})
	h.exec(`INSERT INTO dev_branches (repo_id, branch, sha, work_item_id, kind, phase, impl_round) VALUES ($1, 'kiln/attempt-old', $2, $3, 'checkpoint', 'implement', 1)`,
		testRepoID, old, w.ID)

	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.workItem(w.ID)
	aid := h.acquire(*w.TaskID)
	h.complete(aid, store.OutcomeTimedOut, nil)
	w = h.wantState(w.ID, StateBlocked)
	if !w.RoundRan {
		t.Fatal("第 2 轮执行过任务")
	}
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if run := h.request(h.fixTask(w).RequestID); run.SHA != w.BaseSHA || run.SHA == old {
		t.Fatalf("第 2 轮的返修从 %s 开始，想要第 2 轮的起点 %s（不用第 1 轮的 checkpoint %s）", run.SHA, w.BaseSHA, old)
	}
}

// TestFixAcrossPhases：设计 PR 返修过之后，实现 PR 的第一次返修建出新任务（不复用设计阶段的任务），本轮返修从零算。
func TestFixAcrossPhases(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	res, aid := h.claimDesign(1)
	h.deliverOn(aid, h.branchHead("main"), map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w := h.wantState(res.WorkItemID, StatePROpen)
	designPR := *w.PRNumber
	h.failingRun(designPR, w.HeadSHA)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	designFix := h.fixTask(w)
	if spec := h.specOf(designFix.ID); spec.Phase != PhaseDesign || spec.Branch != res.Branch || spec.BaseRef != "main" {
		t.Fatalf("设计的返修 spec = %s", jsonString(spec))
	}
	var fids []string
	for _, f := range h.specOf(designFix.ID).Findings {
		fids = append(fids, f.ID)
	}
	fixAid := h.acquire(designFix.ID)
	fixSHA := h.push(devreport.AttemptBranch(fixAid), w.HeadSHA, map[string]string{designDoc: "# 设计（修改）\n"})
	rep := report([]int{1}, devreport.ResolutionNone, func(d *devreport.Delivery) {
		d.Design.Documents = []string{designDoc}
		for _, f := range fids {
			d.Dispositions = append(d.Dispositions, devreport.Disposition{Finding: f, Action: devreport.ActionFixed, Commit: fixSHA, Reason: "改了"})
		}
	})
	h.complete(fixAid, store.OutcomeSucceeded, &Delivery{Report: rep, Branch: devreport.AttemptBranch(fixAid), SHA: fixSHA})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)

	h.merge(designPR, res.Branch, map[string]string{designDoc: "# 设计（修改）\n"})
	h.mustProcess()
	w = h.workItem(w.ID)
	if w.RoundFixes() != 0 || w.FixRounds != 1 {
		t.Fatalf("进入实现后返修 %d（共 %d）", w.RoundFixes(), w.FixRounds)
	}
	h.deliverTask(*w.TaskID, w.BaseSHA, map[string]string{"feature.go": "package demo\n"}, delivered([]int{1}, ""))
	w = h.wantState(w.ID, StatePROpen)
	h.failingRun(*w.PRNumber, w.HeadSHA)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	implFix := h.fixTask(w)
	if implFix.ID == designFix.ID || w.RoundFixes() != 1 {
		t.Fatalf("实现的第一次返修：任务 %d（设计的返修 %d），本轮返修 %d", implFix.ID, designFix.ID, w.RoundFixes())
	}
	if spec := h.specOf(implFix.ID); spec.Phase != PhaseImplement || spec.Branch != w.Branch() || spec.DesignPR != designPR {
		t.Fatalf("实现的返修 spec = %s", jsonString(spec))
	}
}

// TestMergedBeforePromoted：设计交付还没推进完（编号没记下）时合并事件先到：按设计分支找到工作项，只推进一次。
func TestMergedBeforePromoted(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	res, aid := h.claimDesign(1)
	h.deliverOnly(aid, map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	h.wantState(res.WorkItemID, StateDelivering)
	for range 2 {
		h.merge(99, res.Branch, map[string]string{designDoc: "# 设计\n"})
	}
	w := h.wantState(res.WorkItemID, StateDeveloping)
	if w.Phase != PhaseImplement || w.DesignPR == nil || *w.DesignPR != 99 || w.ImplRound != 1 || w.DeliverySHA != "" {
		t.Fatalf("合并后 = %s", jsonString(w))
	}
	if n := h.queryInt(`SELECT count(*) FROM audit_log WHERE action='aidev.merged' AND detail->>'work_item'=$1`, itoa(w.ID)); n != 1 {
		t.Fatalf("合并推进了 %d 次", n)
	}
}

// deliverOnly 交付但不处理（停在 delivering）。
func (h *harness) deliverOnly(aid int64, files map[string]string, rep []byte) {
	h.t.Helper()
	branch := devreport.AttemptBranch(aid)
	sha := h.push(branch, h.branchHead("main"), files)
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: rep, Branch: branch, SHA: sha})
}

// TestPartialMergeClosedIssue：部分完成的 Issue 在合并前被人关闭（人认为已经够了）：合并后工作项结束，不进入下一轮。
func TestPartialMergeClosedIssue(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	h.deliverOn(aid, h.branchHead("main"), map[string]string{"feature.go": "package demo\n"}, report([]int{1}, devreport.ResolutionPartial, nil))
	w := h.wantState(res.WorkItemID, StatePROpen)
	h.closeIssue(1)
	h.event(1, IssueClosed, "")
	h.wantState(w.ID, StatePROpen)
	h.merge(*w.PRNumber, w.Branch(), map[string]string{"feature.go": "package demo\n"})
	if w = h.wantState(w.ID, StateMerged); w.ImplRound != 1 {
		t.Fatalf("合并后 = %s", jsonString(w))
	}
}

// TestImplLinkRecovery：实现任务已经提交、还没关联到工作项时（两步之间失败）：再处理时按去重键得到同一个任务并关联；
// 任务先结束时在记录结果时关联，交付照常推进。
func TestImplLinkRecovery(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.partialRound()
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	task := *h.workItem(w.ID).TaskID
	unlink := func() { h.exec(`UPDATE dev_work_items SET task_id=NULL, round_ran=false WHERE id=$1`, w.ID) }

	start := h.specOf(task).StartSHA
	unlink()
	// 两次规划之间默认分支推进了：关联的是之前提交的任务，本轮的起点以它为准
	h.commitOn("main", map[string]string{"other.go": "package demo\n"}, "别的改动")
	h.mustProcess()
	if cur := h.workItem(w.ID); cur.TaskID == nil || *cur.TaskID != task || h.developRequests() != 2 || cur.BaseSHA != start {
		t.Fatalf("重新规划后任务 %v（想要 %d），请求 %d 个，起点 %s（想要任务的起点 %s）", cur.TaskID, task, h.developRequests(), cur.BaseSHA, start)
	}

	unlink()
	h.deliverTask(task, start, map[string]string{"feature.go": "package demo\n\n// 第 2 轮\n"}, delivered([]int{1}, ""))
	if cur := h.wantState(w.ID, StatePROpen); cur.TaskID == nil || *cur.TaskID != task || !cur.RoundRan || cur.Branch() != "kiln/dev-"+itoa(w.ID)+"-r2" {
		t.Fatalf("结束时关联后 = %s", jsonString(cur))
	}
}

// TestLateResultAfterNewRound：设计 PR 返修时设计 PR 被合并（进入实现、取消返修）：被取消的返修晚到的结果不推进工作项，
// 但它推到远端的 checkpoint 照样登记（属于设计阶段，保留期满后删除，不被实现阶段的 retry 使用）。
func TestLateResultAfterNewRound(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	res, aid := h.claimDesign(1)
	h.deliverOn(aid, h.branchHead("main"), map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w := h.wantState(res.WorkItemID, StatePROpen)
	h.failingRun(*w.PRNumber, w.HeadSHA)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	fixAid, head := h.acquire(h.fixTask(w).ID), w.HeadSHA
	h.merge(*w.PRNumber, res.Branch, map[string]string{designDoc: "# 设计\n"})
	w = h.wantState(w.ID, StateDeveloping)

	branch := devreport.AttemptBranch(fixAid)
	cp := h.push(branch, head, map[string]string{designDoc: "# 设计（改了一半）\n"})
	h.complete(fixAid, store.OutcomeCancelled, &Delivery{Branch: branch, SHA: cp, Checkpoint: true})
	if cur := h.workItem(w.ID); cur.Phase != PhaseImplement || cur.State != StateDeveloping {
		t.Fatalf("晚到的结果推进了工作项：%s", jsonString(cur))
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_branches WHERE branch=$1 AND sha=$2 AND work_item_id=$3 AND kind='checkpoint' AND phase='design' AND impl_round=1`,
		branch, cp, w.ID); n != 1 {
		t.Fatal("晚到的 checkpoint 没有登记（不会被清理）")
	}
}

// TestDesignMergedElsewhere：设计 PR 被改了目标分支、合并到了别的分支（设计不在默认分支中）：不从缺少设计的默认分支
// 开始实现，转为受阻并说明；设计进入默认分支后 retry 开始实现。
func TestDesignMergedElsewhere(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	w, res, designPR := h.designPROpen()
	h.git("branch", "release", "main")
	elsewhere := h.commitOn("release", map[string]string{designDoc: "# 设计\n"}, "合并到 release")
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: designPR, HeadRef: res.Branch, Merged: true, MergeSHA: elsewhere}, "bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateBlocked)
	if w.TaskID != nil || w.RoundRan || !strings.Contains(w.Reason, "不在默认分支") || h.developRequests() != 1 {
		t.Fatalf("设计不在默认分支时 = %s", jsonString(w))
	}
	h.git("checkout", "-q", "main")
	h.git("merge", "-q", "--no-edit", "release")
	h.git("checkout", "-q", "--detach")
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	if w = h.workItem(w.ID); w.TaskID == nil || w.State != StateDeveloping {
		t.Fatalf("设计进入默认分支后 retry = %s", jsonString(w))
	}
}

// TestMergeBeforeFixPromoted：部分完成的实现 PR 返修中，完整的返修已经交付、还没推进到正式分支时，人合并了 PR（旧的
// head，仍是部分完成）：按合并的 head 判断，受阻并开始下一轮，不按还没推进的返修报告结束。合并的 head 就是待推进的
// 交付时按这次的报告。
func TestMergeBeforeFixPromoted(t *testing.T) {
	setup := func(t *testing.T) (*harness, *WorkItem, string, string) {
		t.Helper()
		h := newHarness(t, devConfigYML)
		h.issue(1, "升级历史", "显示升级历史")
		aid := h.acquire(h.taskOf(h.startSelect()).ID)
		res := h.mustClaim(aid, []int{1})
		h.deliverOn(aid, h.branchHead("main"), map[string]string{"feature.go": "package demo\n"}, report([]int{1}, devreport.ResolutionPartial, nil))
		w := h.wantState(res.WorkItemID, StatePROpen)
		old := w.HeadSHA
		h.failingRun(*w.PRNumber, old)
		h.mustProcess()
		w = h.wantState(w.ID, StateFixing)
		var fids []string
		for _, f := range h.specOf(*w.TaskID).Findings {
			fids = append(fids, f.ID)
		}
		fixAid := h.acquire(*w.TaskID)
		fix := h.push(devreport.AttemptBranch(fixAid), old, map[string]string{"feature.go": "package demo\n\n// 完整\n"})
		h.complete(fixAid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, fids...), Branch: devreport.AttemptBranch(fixAid), SHA: fix})
		w = h.wantState(w.ID, StateDelivering)
		if got := h.queryString(`SELECT resolution FROM dev_work_item_issues WHERE work_item_id=$1 AND issue_number=1`, w.ID); got != devreport.ResolutionPartial {
			t.Fatalf("推进之前完成度 = %s，应仍是正式分支上的 partial", got)
		}
		return h, w, old, fix
	}
	t.Run("合并旧 head", func(t *testing.T) {
		h, w, old, _ := setup(t)
		if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: *w.PRNumber, HeadRef: w.Branch(), HeadSHA: old, Merged: true}, "bob"); err != nil {
			t.Fatal(err)
		}
		if w = h.wantState(w.ID, StateBlocked); w.ImplRound != 2 {
			t.Fatalf("合并部分完成的旧 head 后 = %s", jsonString(w))
		}
	})
	t.Run("合并的就是待推进的交付", func(t *testing.T) {
		h, w, _, fix := setup(t)
		if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: *w.PRNumber, HeadRef: w.Branch(), HeadSHA: fix, Merged: true}, "bob"); err != nil {
			t.Fatal(err)
		}
		h.wantState(w.ID, StateMerged)
	})
}

// TestDefaultBranchChanged：设计合并后仓库改了默认分支：实现从新的默认分支开始，PR 也开到新的默认分支上。
func TestDefaultBranchChanged(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	w, res, designPR := h.designPROpen()
	h.merge(designPR, res.Branch, map[string]string{designDoc: "# 设计\n"})
	h.git("branch", "trunk", "main")
	trunk := h.commitOn("trunk", map[string]string{"trunk.go": "package demo\n"}, "trunk 独有")
	h.exec(`UPDATE repos SET default_branch='trunk' WHERE id=$1`, testRepoID)
	h.repo.DefaultBranch = "trunk"
	h.mustProcess()
	w = h.workItem(w.ID)
	if spec := h.specOf(*w.TaskID); spec.BaseRef != "trunk" || spec.StartSHA != trunk || w.BaseRef != "trunk" || w.BaseSHA != trunk {
		t.Fatalf("实现 spec = %s，工作项 = %s", jsonString(spec), jsonString(w))
	}
	h.deliverTask(*w.TaskID, trunk, map[string]string{"feature.go": "package demo\n"}, delivered([]int{1}, ""))
	w = h.wantState(w.ID, StatePROpen)
	if p := h.pull(*w.PRNumber); p.BaseRef != "trunk" {
		t.Fatalf("实现 PR 的目标分支 = %s", p.BaseRef)
	}
}

// TestMergedReportFeedsImplementation：实现任务的设计文档和交接取实际合并的那次交付：设计 PR 的返修已经交付、还没推进时
// 合并旧 head，实现用旧 head 上的设计文档和交接，不用返修报告里的；之后的轮次（报告没有设计文档）继续用已合并的设计，
// 交接取上一轮合并的实现。
func TestMergedReportFeedsImplementation(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "升级历史", "显示升级历史")
	w, res, designPR := h.designPROpen()
	old := w.HeadSHA
	h.failingRun(designPR, old)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	fix := h.fixTask(w)
	var fids []string
	for _, f := range h.specOf(fix.ID).Findings {
		fids = append(fids, f.ID)
	}
	const revised = "docs/revised.md"
	fixAid := h.acquire(fix.ID)
	fixSHA := h.push(devreport.AttemptBranch(fixAid), old, map[string]string{revised: "# 返修\n"})
	rep := report([]int{1}, devreport.ResolutionNone, func(d *devreport.Delivery) {
		d.Design.Documents = []string{revised}
		d.Handoff.Remaining = "返修的交接"
		for _, f := range fids {
			d.Dispositions = append(d.Dispositions, devreport.Disposition{Finding: f, Action: devreport.ActionFixed, Commit: fixSHA, Reason: "改了"})
		}
	})
	h.complete(fixAid, store.OutcomeSucceeded, &Delivery{Report: rep, Branch: devreport.AttemptBranch(fixAid), SHA: fixSHA})
	h.wantState(w.ID, StateDelivering)
	mergeSHA := h.commitOn("main", map[string]string{designDoc: "# 设计\n"}, "合并旧 head")
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: designPR, HeadRef: res.Branch, HeadSHA: old, Merged: true, MergeSHA: mergeSHA},
		"bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.workItem(w.ID)
	if spec := h.specOf(*w.TaskID); !slices.Equal(spec.DesignDocs, []string{designDoc}) || strings.Contains(spec.Handoff, "返修的交接") ||
		!strings.Contains(spec.Handoff, "按状态筛选") {
		t.Fatalf("合并旧 head 后的实现 spec = %s", jsonString(spec))
	}

	h.deliverTask(*w.TaskID, mergeSHA, map[string]string{"feature.go": "package demo\n"}, report([]int{1}, devreport.ResolutionPartial,
		func(d *devreport.Delivery) { d.Handoff.Remaining = "第 1 轮剩余" }))
	w = h.wantState(w.ID, StatePROpen)
	h.merge(*w.PRNumber, w.Branch(), map[string]string{"feature.go": "package demo\n"})
	w = h.wantState(w.ID, StateBlocked)
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.workItem(w.ID)
	if spec := h.specOf(*w.TaskID); spec.ImplRound != 2 || !slices.Equal(spec.DesignDocs, []string{designDoc}) || !strings.Contains(spec.Handoff, "第 1 轮剩余") {
		t.Fatalf("第 2 轮的实现 spec = %s", jsonString(spec))
	}
}

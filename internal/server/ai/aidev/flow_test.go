package aidev

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestDevelopFlow：选题 → 领取 → 交付 → 推进的完整路径。选题的候选只有打开、有 ready、没有 skip 的 Issue；领取写入工作项、
// 未选的跳过记录、计划评论；交付只到 delivering，执行者核对后才创建正式分支和 PR（Fixes 只给 full），删除 attempt 分支，
// Issue 的状态 label 跟随状态。
func TestDevelopFlow(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	h.issue(2, "不清楚的需求", "?")
	h.issue(3, "被排除", "x", LabelReady, LabelSkip)
	h.issue(4, "没有授权", "y", "bug")
	h.issue(5, "已关闭", "z")
	h.editIssue(5, func(is *fakegithub.Issue) { is.State = "closed" })
	if msg := h.event(5, IssueClosed, ""); !strings.Contains(msg, "离开任务池") {
		t.Fatalf("关闭的 Issue：%q", msg)
	}

	job, aid := h.wantSelectTask(1, 2)
	res := h.mustClaim(aid, []int{1}, 2)
	if res.Branch != devreport.WorkItemBranch(res.WorkItemID, devreport.PhaseImplement, 1) {
		t.Fatalf("分支 = %s", res.Branch)
	}
	w := h.wantState(res.WorkItemID, StateDeveloping)
	if w.BaseSHA != h.branchHead("main") || w.MaxFixRounds != 2 || !slices.Equal(w.DevAgents, []string{devConfigAgent}) || w.TaskID == nil ||
		*w.TaskID != job.ID {
		t.Fatalf("工作项 = %s", jsonString(w))
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_issue_skips WHERE repo_id=$1 AND issue_number=2 AND digest=$2`, testRepoID, IssueDigest("不清楚的需求", "?")); n != 1 {
		t.Fatal("未选的 #2 没有记入跳过记录")
	}
	// 重复的领取请求返回同一个工作项（I3）
	if again := h.mustClaim(aid, []int{1}, 2); again.WorkItemID != res.WorkItemID {
		t.Fatalf("重复领取得到 #%d", again.WorkItemID)
	}
	h.mustProcess()
	h.wantLabels(1, LabelClaimed, LabelReady)
	if c := h.comments(); !slices.Equal(c, []string{"1:claimed:done"}) {
		t.Fatalf("评论 = %v", c)
	}

	branch := devreport.AttemptBranch(aid)
	sha := h.push(branch, w.BaseSHA, map[string]string{"feature.go": "package demo\n"})
	h.exec(`DELETE FROM outbox`)
	out := h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: branch, SHA: sha, AgentVersion: "2.1.0", Model: "opus"})
	if out.Outcome != store.OutcomeSucceeded {
		t.Fatalf("任务结果 = %+v", out)
	}
	// 请求结束时通知执行者核对（AfterFinish）
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1 AND dedupe_key=$2`, KindRepo, repoKey(testRepoID)); n != 1 {
		t.Fatalf("开发请求结束后没有通知执行者（%d 条）", n)
	}
	w = h.wantState(res.WorkItemID, StateDelivering)
	if w.DeliverySHA != sha || h.branchHead(w.Branch()) != "" {
		t.Fatal("交付之后、核对之前不能动正式分支")
	}
	h.mustProcess()
	w = h.wantState(res.WorkItemID, StatePROpen)
	if h.branchHead(w.Branch()) != sha || w.HeadSHA != sha || w.PRNumber == nil {
		t.Fatalf("推进后 = %s，正式分支 %s", jsonString(w), h.branchHead(w.Branch()))
	}
	pr, _ := h.fake.GetPull(h.fakeRepo(), *w.PRNumber)
	if pr.HeadRef != w.Branch() || pr.BaseRef != "main" || pr.Title != "实现功能" || !strings.Contains(pr.Body, "Fixes #1") ||
		!strings.Contains(pr.Body, "Agent 自报") || slices.Contains(pr.Labels, mergecfg.HoldLabel) {
		t.Fatalf("PR = %+v", pr)
	}
	if h.branchHead(branch) != "" {
		t.Fatal("推进后 attempt 分支没有删除")
	}
	h.wantLabels(1, LabelPROpen, LabelReady)
	h.wantScope(job.ID, "2.1.0", "opus")
	// 再处理一次不会重复做任何事
	before := len(h.fake.Requests())
	h.mustProcess()
	if reqs := h.fake.Requests()[before:]; len(reqs) != 0 {
		t.Fatalf("没有变化时又请求了 GitHub：%v", reqs)
	}

	// PR 合并：工作项结束，状态 label 去掉，人加的 ready 保留；任务池中没有可选的（#2 在跳过记录中），不再选题
	msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: *w.PRNumber, HeadRef: w.Branch(), Merged: true}, "bob")
	if err != nil || !strings.Contains(msg, "已合并") {
		t.Fatalf("PR 合并：%q, %v", msg, err)
	}
	h.wantState(res.WorkItemID, StateMerged)
	h.editIssue(1, func(is *fakegithub.Issue) { is.State = "closed" }) // GitHub 按 Fixes 关闭
	h.event(1, IssueClosed, "")
	h.mustProcess()
	h.wantLabels(1, LabelReady)
	if n := h.queryInt(`SELECT count(*) FROM dev_work_item_issues WHERE work_item_id=$1 AND active`, res.WorkItemID); n != 0 {
		t.Fatal("结束后 Issue 仍被占用（I1）")
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_requests WHERE type=$1`, agentcfg.TaskDevelop); n != 1 {
		t.Fatalf("没有候选时又建了选题请求（共 %d 个）", n)
	}
}

// wantSelectTask 规划选题，确认任务的内容（配置来自默认分支）并且不创建 Check Run；开发机领取后要到的候选是 cands。
// 返回任务和 attempt。
func (h *harness) wantSelectTask(cands ...int) (*ai.Task, int64) {
	h.t.Helper()
	job := h.taskOf(h.startSelect())
	if job.Type != agentcfg.TaskDevelop || !slices.Equal(job.Platforms, []string{"darwin/arm64"}) {
		h.t.Fatalf("任务 = %s %v", job.Type, job.Platforms)
	}
	spec := h.specOf(job.ID)
	if spec.Mode != aispec.DevSelect || spec.Agent != "claude-code" || spec.MaxIssues != 2 ||
		spec.MaxFixRounds != 2 || spec.Instructions != "开发说明\n" {
		h.t.Fatalf("选题 spec = %s", jsonString(spec))
	}
	aid := h.acquire(job.ID)
	offer := h.offer(aid)
	if got := candidates(offer.Candidates); !slices.Equal(got, cands) || offer.Candidates[0].Digest != IssueDigest("加功能", "要做 a") {
		h.t.Fatalf("候选 = %v，想要 %v", got, cands)
	}
	// AI 开发不创建 Check Run（任务和请求都不写同步记录）
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind = ANY($1)`, []string{ai.KindTaskCheck, ai.KindRequestCheck}); n != 0 {
		h.t.Fatalf("写了 %d 条 Check Run 同步记录", n)
	}
	return job, aid
}

// wantScope 确认任务的交付报告记下的 Agent 版本和模型（适用范围，#26.14）。
func (h *harness) wantScope(taskID int64, agentVersion, model string) {
	h.t.Helper()
	var v, m string
	if err := h.db.QueryRow(h.ctx, `SELECT agent_version, model FROM dev_reports WHERE task_id=$1 AND kind='delivery'`, taskID).Scan(&v, &m); err != nil ||
		v != agentVersion || m != model {
		h.t.Fatalf("交付报告的 Agent 版本 = %q、模型 %q, %v", v, m, err)
	}
}

// wantLabels 确认 Issue n 的 label（排序后）。
func (h *harness) wantLabels(n int, want ...string) {
	h.t.Helper()
	if l := h.labels(n); !slices.Equal(l, want) {
		h.t.Fatalf("#%d 的 label = %v，想要 %v", n, l, want)
	}
}

// TestSensitivePathsHold：交付改动了 CI / Kiln 配置或给 AI 的说明时 PR 加 kiln:hold，不自动入队。
func TestSensitivePathsHold(t *testing.T) {
	for _, path := range []string{".kiln/workflows/ci.yml", "AGENTS.md", ".github/workflows/x.yml", "CLAUDE.md", ".claude/settings.json", ".codex/config.toml"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "t", "b")
			run := h.startSelect()
			aid := h.acquire(h.taskOf(run).ID)
			res := h.mustClaim(aid, []int{1})
			sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{path: "x\n"})
			h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
			h.mustProcess()
			w := h.wantState(res.WorkItemID, StatePROpen)
			pr, _ := h.fake.GetPull(h.fakeRepo(), *w.PRNumber)
			if !slices.Contains(pr.Labels, mergecfg.HoldLabel) || !w.HoldLabel || !strings.Contains(pr.Body, "kiln:hold") {
				t.Fatalf("改动 %s 后 PR 的 label = %v", path, pr.Labels)
			}
		})
	}
	if touchesSensitive([]string{"internal/kiln/x.go", "docs/AGENTS.md.bak", "src/.kiln"}) {
		t.Fatal("不是敏感路径也加了 hold")
	}
}

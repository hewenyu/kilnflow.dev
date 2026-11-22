package aidev

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// devRequests 返回 AI 开发请求的数量；open 为真时只数排队中、进行中的。
func (h *harness) devRequests(open bool) int64 {
	h.t.Helper()
	return h.queryInt(`SELECT count(*) FROM ai_requests WHERE type=$1 AND (NOT $2 OR state IN ('queued','active'))`, agentcfg.TaskDevelop, open)
}

// claimed 走到 developing：选题、领取 issues，返回工作项和 attempt。
func (h *harness) claimed(issues ...int) (*WorkItem, int64) {
	h.t.Helper()
	job := h.taskOf(h.startSelect())
	aid := h.acquire(job.ID)
	var later []int
	for _, c := range h.offer(aid).Candidates {
		if !slices.Contains(issues, c.Number) {
			later = append(later, c.Number)
		}
	}
	res, err := h.claimSel(aid, issues, selection(issues, nil, later))
	if err != nil || !res.Claimed {
		h.t.Fatalf("领取：%+v, %v", res, err)
	}
	return h.wantState(res.WorkItemID, StateDeveloping), aid
}

// TestResultFailures：执行没有交付时转为受阻：超时（保存了 checkpoint：记下分支，重试从它继续）、报告无效（任务改为失败
// ai.delivery）、Agent 自报受阻；Agent 判断不需要改代码时结束为 no_change 并记入跳过记录。
func TestResultFailures(t *testing.T) {
	t.Run("超时保存 checkpoint", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, aid := h.claimed(1)
		branch := devreport.AttemptBranch(aid)
		cp := h.push(branch, w.BaseSHA, map[string]string{"half.go": "package demo\n"})
		h.complete(aid, store.OutcomeTimedOut, &Delivery{Branch: branch, SHA: cp, Checkpoint: true})
		w = h.wantState(w.ID, StateBlocked)
		if !strings.Contains(w.Reason, branch) || w.DeliverySHA != "" {
			t.Fatalf("原因 = %q", w.Reason)
		}
		if n := h.queryInt(`SELECT count(*) FROM dev_branches WHERE branch=$1 AND sha=$2 AND kind='checkpoint' AND deleted_at IS NULL`, branch, cp); n != 1 {
			t.Fatal("没有记下 checkpoint 分支")
		}
		h.mustProcess()
		if h.branchHead(branch) != cp {
			t.Fatal("checkpoint 分支在保留时间内被删除")
		}
		if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		w = h.wantState(w.ID, StateFixing)
		j := h.fixTask(w)
		if run, d := h.request(j.RequestID), h.specOf(j.ID); run.SHA != cp || !strings.Contains(d.Handoff, branch) {
			t.Fatalf("重试从 %s 开始（想要 checkpoint %s），交接 %q", run.SHA, cp, d.Handoff)
		}
		// 重试只对受阻的工作项有效
		if err := h.svc.Retry(h.ctx, w.ID, "admin"); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("报告无效", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, aid := h.claimed(1)
		sha := h.push(devreport.AttemptBranch(aid), w.BaseSHA, map[string]string{"a.go": "x"})
		// 报告声称处理了没有领取的 Issue
		out := h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1, 9}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
		if out.Outcome != store.OutcomeFailed || out.ErrClass != errClassAIDelivery {
			t.Fatalf("任务结果 = %+v", out)
		}
		h.wantState(w.ID, StateBlocked)
		if n := h.queryInt(`SELECT count(*) FROM dev_reports WHERE kind='delivery'`); n != 0 {
			t.Fatal("保存了无效的交付报告")
		}
	})
	t.Run("Agent 自报受阻", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, aid := h.claimed(1)
		rep := devreport.Delivery{
			Version: devreport.Version, Status: devreport.StatusBlocked, Summary: "缺少密钥",
			Issues:  []devreport.IssueResult{{Number: 1, Resolution: devreport.ResolutionNone, Notes: "缺少密钥"}},
			Handoff: devreport.Handoff{Remaining: "需要人配置 API key"},
		}
		h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: devreport.Marshal(&rep)})
		if w = h.wantState(w.ID, StateBlocked); !strings.Contains(w.Reason, "需要人配置 API key") {
			t.Fatalf("原因 = %q", w.Reason)
		}
	})
	t.Run("不需要改代码", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, aid := h.claimed(1)
		rep := devreport.Delivery{
			Version: devreport.Version, Status: devreport.StatusNoChange, Summary: "已经实现了",
			Issues: []devreport.IssueResult{{Number: 1, Resolution: devreport.ResolutionNone, Notes: "main 上已经有了"}},
		}
		h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: devreport.Marshal(&rep)})
		h.wantState(w.ID, StateNoChange)
		if n := h.queryInt(`SELECT count(*) FROM dev_issue_skips WHERE issue_number=1 AND reason LIKE $1`, skipNoChange+"%"); n != 1 {
			t.Fatal("没有记入跳过记录")
		}
		if n := h.queryInt(`SELECT count(*) FROM dev_work_item_issues WHERE active`); n != 0 {
			t.Fatal("结束后 Issue 仍被占用")
		}
	})
}

// TestReconcileLostTask：任务结束了而结果没有推进工作项时（例如迁移时以 infra.upgrade 结束的任务），执行者转为受阻（AI 开发的
// 基础设施错误不自动重试）。
func TestReconcileLostTask(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _ := h.claimed(1)
	h.mustProcess()
	h.wantState(w.ID, StateDeveloping)
	h.exec(`UPDATE ai_tasks SET state='failed', reason='Runner 失联：租约过期', finished_at=now() WHERE id=$1`, *w.TaskID)
	h.mustProcess()
	if w = h.wantState(w.ID, StateBlocked); !strings.Contains(w.Reason, "租约过期") {
		t.Fatalf("原因 = %q", w.Reason)
	}
	if c := h.comments(); !slices.Contains(c, "1:blocked:done") {
		t.Fatalf("评论 = %v", c)
	}
}

// TestStop：终止取消执行中的任务，释放 Issue 并记入跳过记录，去掉状态 label；重复终止返回 ErrFinished。开发机确认取消
// （任务结束）之前它不空闲，不开始下一轮选题；确认之后从剩下的 Issue 开始下一轮（取消是异步的，不能在确认之前把"结束后
// 选题"用掉）。
func TestStop(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, aid := h.claimed(1)
	h.issue(2, "二", "b") // 领取之后才授权的 Issue
	h.mustProcess()
	if err := h.svc.Stop(h.ctx, w.ID, "不做了", "admin"); err != nil {
		t.Fatal(err)
	}
	h.wantState(w.ID, StateStopped)
	if j := h.task(*w.TaskID); j.State != ai.TaskCancelled && !j.CancelRequested {
		t.Fatalf("任务没有取消：%s", j.State)
	}
	if err := h.svc.Stop(h.ctx, w.ID, "again", "admin"); !errors.Is(err, ErrFinished) {
		t.Fatalf("err = %v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_issue_skips WHERE issue_number=1 AND reason LIKE $1`, skipStopped+"%"); n != 1 {
		t.Fatal("终止的 Issue 没有记入跳过记录")
	}
	// 开发机还没有确认取消：它不空闲，不选题
	h.mustProcess()
	if n := h.devRequests(false); n != 1 {
		t.Fatalf("开发机确认取消之前就选题了（%d 个请求）", n)
	}
	// 确认取消之后：label 去掉，开发机空闲，从剩下的 #2 开始下一轮
	h.complete(aid, store.OutcomeCancelled, nil)
	h.mustProcess()
	if l := h.labels(1); slices.Contains(l, LabelClaimed) {
		t.Fatalf("#1 的 label = %v", l)
	}
	runs, err := store.QueryIDs(h.ctx, h.db, `SELECT id FROM ai_requests WHERE type=$1 AND state IN ('queued','active') ORDER BY id`, agentcfg.TaskDevelop)
	if err != nil || len(runs) != 1 {
		t.Fatalf("新的选题请求：%v, %v", runs, err)
	}
	if got := candidates(h.offer(h.acquire(h.taskOf(runs[0]).ID)).Candidates); !slices.Equal(got, []int{2}) {
		t.Fatalf("候选 = %v", got)
	}
}

// TestIssueEvents：重新打上 ready 清除跳过记录并触发选题；加 skip、去掉 ready 终止；没有 PR 的工作项的 Issue 全部关闭时
// 终止，有 PR 时不终止（由 PR 决定）；内容变了记 requirements_changed；本 App 自己的操作忽略。
func TestIssueEvents(t *testing.T) {
	// setOpen 在 GitHub 上关闭或重新打开 Issue n，并投递对应的事件
	setOpen := func(h *harness, n int, open bool) {
		h.t.Helper()
		state, action := "closed", IssueClosed
		if open {
			state, action = "open", IssueReopened
		}
		h.editIssue(n, func(is *fakegithub.Issue) { is.State = state })
		h.event(n, action, "")
	}
	t.Run("ready 清除跳过记录", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		h.exec(`INSERT INTO dev_issue_skips (repo_id, issue_number, digest, reason) VALUES ($1, 1, $2, 'x')`, testRepoID, IssueDigest("一", "a"))
		if r, _ := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); r.RequestID != 0 || r.Reason != whyNoCandidates {
			t.Fatalf("跳过的 Issue 进入了候选：%+v", r)
		}
		is, _ := h.fake.GetIssue(repoName, 1)
		self := IssueEvent{
			RepoID: testRepoID, Action: IssueLabeled, Number: 1, Title: is.Title, Body: is.Body, Open: true, Labels: is.Labels,
			UpdatedAt: is.UpdatedAt, Label: LabelReady, FromSelf: true,
		}
		if msg, err := h.svc.HandleIssueEvent(h.ctx, self); err != nil || !strings.Contains(msg, "本 App") {
			t.Fatalf("本 App 的事件：%q, %v", msg, err)
		}
		if r, _ := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); r.RequestID != 0 {
			t.Fatal("本 App 的事件清除了跳过记录")
		}
		if msg := h.event(1, IssueLabeled, LabelReady); !strings.Contains(msg, "任务池") {
			t.Fatalf("人重新打 ready：%q", msg)
		}
		run := h.startSelect()
		// 重复投递的事件、同时到达的触发不重复建请求（这一轮已经有选题请求）
		h.event(1, IssueLabeled, LabelReady)
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID != run {
			t.Fatalf("重复触发：%+v, %v", r, err)
		}
		if n := h.devRequests(false); n != 1 {
			t.Fatalf("建了 %d 个选题请求", n)
		}
	})
	human := func(change func(is *fakegithub.Issue)) func(h *harness) IssueEvent {
		return func(h *harness) IssueEvent {
			h.editIssue(1, change)
			is, _ := h.fake.GetIssue(repoName, 1)
			return IssueEvent{
				RepoID: testRepoID, Number: 1, Title: is.Title, Body: is.Body, Open: is.State == "open", Labels: is.Labels,
				UpdatedAt: is.UpdatedAt, Sender: "bob",
			}
		}
	}
	for _, tt := range []struct {
		name   string
		action string
		label  string
		change func(is *fakegithub.Issue)
	}{
		{"加 skip", IssueLabeled, LabelSkip, func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelSkip) }},
		{"去掉 ready", IssueUnlabeled, LabelReady, func(is *fakegithub.Issue) {
			is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return l == LabelReady })
		}},
		{"关闭（没有 PR）", IssueClosed, "", func(is *fakegithub.Issue) { is.State = "closed" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _ := h.claimed(1)
			e := human(tt.change)(h)
			e.Action, e.Label = tt.action, tt.label
			if _, err := h.svc.HandleIssueEvent(h.ctx, e); err != nil {
				t.Fatal(err)
			}
			h.wantState(w.ID, StateStopped)
			if st, _ := readPool(h.ctx, h.db, testRepoID); len(st.candidates) != 0 {
				t.Fatalf("撤回授权的 Issue 还是候选：%v", candidates(st.candidates))
			}
		})
	}
	t.Run("关闭（有 PR、多个 Issue）", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		h.issue(2, "二", "b")
		w, _ := h.developed(1, 2)
		for _, n := range []int{1, 2} {
			setOpen(h, n, false)
		}
		h.wantState(w.ID, StatePROpen)
		if n := h.queryInt(`SELECT count(*) FROM dev_work_item_issues WHERE work_item_id=$1 AND closed`, w.ID); n != 2 {
			t.Fatalf("记下关闭的 Issue %d 个，应为 2", n)
		}
	})
	t.Run("关闭一部分（没有 PR）", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		h.issue(2, "二", "b")
		w, _ := h.claimed(1, 2)
		setOpen(h, 1, false)
		h.wantState(w.ID, StateDeveloping)
		setOpen(h, 1, true)
		setOpen(h, 2, false)
		h.wantState(w.ID, StateDeveloping) // #1 重新打开了
		setOpen(h, 1, false)
		h.wantState(w.ID, StateStopped) // 都关闭了
	})
	t.Run("内容变了", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, _ := h.claimed(1)
		same := IssueEvent{RepoID: testRepoID, Action: IssueEdited, Number: 1, Title: "一", Body: "a", Open: true, Labels: []string{LabelReady}, UpdatedAt: h.tick()}
		if _, err := h.svc.HandleIssueEvent(h.ctx, same); err != nil || h.workItem(w.ID).RequirementsChanged {
			t.Fatalf("内容没变也记了：%v", err)
		}
		same.Body, same.UpdatedAt = "加了验收标准", h.tick()
		if _, err := h.svc.HandleIssueEvent(h.ctx, same); err != nil || !h.workItem(w.ID).RequirementsChanged {
			t.Fatalf("内容变了没有记：%v", err)
		}
		h.wantState(w.ID, StateDeveloping) // 不自动取消
	})
}

// TestPullClosed：工作项的 PR 没有合并就关闭时终止；不是工作项的 PR 什么也不做。
func TestPullClosed(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	if msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: pr + 100, HeadRef: "feature", Merged: false}, "bob"); err != nil || msg != "" {
		t.Fatalf("别的 PR：%q, %v", msg, err)
	}
	// 用正式分支开的另一个 PR：工作项已经记下了自己的 PR，不按分支关联
	if msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: pr + 101, HeadRef: w.Branch(), Merged: false}, "bob"); err != nil || msg != "" {
		t.Fatalf("同一分支上的别的 PR：%q, %v", msg, err)
	}
	h.wantState(w.ID, StatePROpen)
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: pr, HeadRef: "feature", Merged: false}, "bob"); err != nil {
		t.Fatal(err)
	}
	h.wantState(w.ID, StateStopped)
}

// TestSelectGuards：没有启用开发、没有配置文件、仓库停用时不选题；没有启用时记下"已经看过"，空闲的开发机反复来领取也不
// 再读 GitHub。没有空闲的开发机时不选题（Server 不预先排选题请求）；同一个仓库同时只有一个选题请求，它在领取前结束后
// 不自动重来，任务池变化或手动同步后再选。
func TestSelectGuards(t *testing.T) {
	t.Run("没有启用", func(t *testing.T) {
		h := newHarness(t, "version: 1\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n")
		h.issue(1, "一", "a")
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID != 0 || !strings.Contains(r.Reason, "没有启用") {
			t.Fatalf("%+v, %v", r, err)
		}
		before := len(h.fake.Requests())
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyExamined {
			t.Fatalf("%+v, %v", r, err)
		}
		if err := h.svc.RunnerIdle(h.ctx, store.RunnerRoleDevelopment); err != nil {
			t.Fatal(err)
		}
		if reqs := h.fake.Requests()[before:]; len(reqs) != 0 {
			t.Fatalf("任务池没有变化又读了 GitHub：%v", reqs)
		}
	})
	t.Run("没有配置文件", func(t *testing.T) {
		h := newHarness(t, "")
		h.issue(1, "一", "a")
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID != 0 || r.Reason == "" {
			t.Fatalf("%+v, %v", r, err)
		}
	})
	t.Run("仓库停用", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		h.exec(`UPDATE repos SET active=false WHERE id=$1`, testRepoID)
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyRepoInactive {
			t.Fatalf("%+v, %v", r, err)
		}
	})
	t.Run("没有空闲的开发机", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		h.exec(`UPDATE runners SET last_seen_at = now() - interval '1 hour'`) // 离线
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyNoFreeSlot {
			t.Fatalf("离线：%+v, %v", r, err)
		}
		h.exec(`UPDATE runners SET last_seen_at = now(), drain = true`)
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyNoFreeSlot {
			t.Fatalf("暂停接单：%+v, %v", r, err)
		}
		h.exec(`UPDATE runners SET drain = false`)
		h.startSelect()
	})
	t.Run("并发触发", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if n := h.devRequests(false); n != 1 {
			t.Fatalf("建了 %d 个选题请求", n)
		}
		// 选题请求在领取前失败：这一轮结束，但不在同一个任务池上反复重来
		h.exec(`UPDATE ai_requests SET state='failed', finished_at=now() WHERE type=$1`, agentcfg.TaskDevelop)
		h.exec(`UPDATE ai_tasks SET state='failed', finished_at=now()`)
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyExamined {
			t.Fatalf("失败后：%+v, %v", r, err)
		}
		if n := h.queryInt(`SELECT dev_select_seq FROM ai_repo_state WHERE repo_id=$1`, testRepoID); n != 1 {
			t.Fatalf("选题轮次 = %d，想要 1", n)
		}
		if _, err := h.svc.SyncPool(h.ctx, testRepoID, "admin"); err != nil {
			t.Fatal(err)
		}
		if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
			t.Fatalf("同步后：%+v, %v", r, err)
		}
	})
}

// TestBranchGC：超过保留时间的 attempt 分支删除（checkpoint 不 pin）；不是 attempt 前缀的分支即使记录被改坏也不删除。
func TestBranchGC(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, aid := h.claimed(1)
	branch := devreport.AttemptBranch(aid)
	cp := h.push(branch, w.BaseSHA, map[string]string{"half.go": "x"})
	h.complete(aid, store.OutcomeTimedOut, &Delivery{Branch: branch, SHA: cp, Checkpoint: true})
	h.git("branch", "keep", cp)
	h.exec(`INSERT INTO dev_branches (repo_id, branch, sha, work_item_id, kind) VALUES ($1, 'keep', $2, $3, 'checkpoint')`, testRepoID, cp, w.ID)
	h.mustProcess()
	if h.branchHead(branch) == "" {
		t.Fatal("保留时间内删除了 checkpoint")
	}
	h.exec(`UPDATE dev_branches SET created_at = now() - make_interval(secs => $1) - interval '1 minute'`, DefaultBranchTTL.Seconds())
	h.mustProcess()
	if h.branchHead(branch) != "" {
		t.Fatal("过期的 checkpoint 没有删除")
	}
	if h.branchHead("keep") == "" {
		t.Fatal("删除了不是 attempt 前缀的分支")
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_branches WHERE branch=$1 AND deleted_at IS NOT NULL`, branch); n != 1 {
		t.Fatal("没有记下已删除")
	}
	// 分支已经被人删掉：照常记为已删除
	h.exec(`UPDATE dev_branches SET deleted_at=NULL WHERE branch=$1`, branch)
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM dev_branches WHERE branch=$1 AND deleted_at IS NOT NULL`, branch); n != 1 {
		t.Fatal("分支已不存在时没有记为已删除")
	}
}

// TestNewerSelfEventCarriesWithdrawal：人撤回授权（去掉 kiln:ready、加 kiln:skip、关闭 Issue，t1）之后
// 本 App 同步了状态 label（t2），两个事件乱序到达：先到的 t2 是本 App 的事件，Issue 的当前内容已经反映了撤回，按内容
// 终止工作项；后到的 t1 是旧事件，忽略。
func TestNewerSelfEventCarriesWithdrawal(t *testing.T) {
	for _, tt := range []struct {
		name          string
		action, label string
		change        func(is *fakegithub.Issue)
	}{
		{"去掉 ready", IssueUnlabeled, LabelReady, func(is *fakegithub.Issue) {
			is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return l == LabelReady })
		}},
		{"加 skip", IssueLabeled, LabelSkip, func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelSkip) }},
		{"关闭", IssueClosed, "", func(is *fakegithub.Issue) { is.State = "closed" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _ := h.claimed(1)
			snapshot := func(action, label string, self bool) IssueEvent {
				is, _ := h.fake.GetIssue(repoName, 1)
				return IssueEvent{
					RepoID: testRepoID, Action: action, Number: 1, Title: is.Title, Body: is.Body, Open: is.State == "open", Labels: is.Labels,
					UpdatedAt: is.UpdatedAt, Label: label, Sender: "bob", FromSelf: self,
				}
			}
			h.editIssue(1, tt.change)
			withdraw := snapshot(tt.action, tt.label, false)
			h.editIssue(1, func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelClaimed) })
			self := snapshot(IssueLabeled, LabelClaimed, true)
			if msg, err := h.svc.HandleIssueEvent(h.ctx, self); err != nil || !strings.Contains(msg, "终止") {
				t.Fatalf("本 App 的较新事件：%q, %v", msg, err)
			}
			h.wantState(w.ID, StateStopped)
			if msg, err := h.svc.HandleIssueEvent(h.ctx, withdraw); err != nil || !strings.Contains(msg, "旧") {
				t.Fatalf("晚到的撤回事件：%q, %v", msg, err)
			}
			h.wantState(w.ID, StateStopped)
		})
	}
}

// 终止工作项与结束任务并发：等待请求锁期间，结束任务的事务把另一轮的任务记到工作项上。终止成功后这个新任务的请求也必须
// 取消（工作项已经是终态，任务不能继续执行），并且终止本身照常生效。
func TestStopCancelsTaskLinkedWhileWaiting(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _ := h.claimed(1)
	oldTask := h.task(*w.TaskID)
	// 另一轮的任务（在另一个仓库排队、有独立的请求）：模拟结束旧任务的事务在等锁期间把它记到工作项上。
	linked := h.queueElsewhere(devConfigAgent, testDevPlatform)

	// 占住旧任务的请求行，让终止停在取消任务的 FOR UPDATE 上。
	lockConn, err := h.db.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	var pid int
	if err := lockConn.QueryRow(h.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	lockTx, err := lockConn.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(context.WithoutCancel(h.ctx)) }()
	if _, err := lockTx.Exec(h.ctx, `SELECT id FROM ai_requests WHERE id=$1 FOR UPDATE`, oldTask.RequestID); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.svc.Stop(h.ctx, w.ID, "不做了", "admin") }()
	waitBlockedBy(t, h, pid)
	// 等锁期间（此时还没有锁工作项）另一轮的任务被关联上来。
	h.exec(`UPDATE dev_work_items SET task_id=$2 WHERE id=$1`, w.ID, linked)
	if err := lockTx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-stopped; err != nil {
		t.Fatalf("终止：%v", err)
	}
	h.wantState(w.ID, StateStopped)
	if j := h.task(linked); j.State != ai.TaskCancelled && !j.CancelRequested {
		t.Fatalf("终止时关联上来的新任务没有取消：%s", j.State)
	}
}

// waitBlockedBy 等到有会话被 pid 阻塞（测试数据库是独立的，按会话 pid 精确判定）。
func waitBlockedBy(t *testing.T, h *harness, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.queryInt(`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("终止没有等在请求锁上")
}

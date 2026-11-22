package aidev

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 返修（PLAN #26.11）：PR head 的 CI 和 AI 审计全部结束后，审计发现问题或 CI 有非基础设施失败时规划一轮返修；同一个 head 只返修一次；
// 超过轮数转为受阻。

// failingRun 在 PR 的 head 上建失败的验证：一个发现问题的审计、一个失败的测试、一个基础设施失败。返回审计任务。
func (h *harness) failingRun(pr int, head string) int64 {
	h.t.Helper()
	ids := h.prRun(pr, head, true,
		prJob{review: true, name: "ai-review", state: ai.TaskFailed, errClass: ai.ErrClassFindings, report: findingsReport},
		prJob{name: "test", state: ai.TaskFailed},
		prJob{name: "lint", state: ai.TaskFailed, errClass: "infra.lost"},
		prJob{name: "build", state: ai.TaskSucceeded},
	)
	return ids[0]
}

// fixTask 返回工作项当前的返修任务（没有时 t.Fatal）。
func (h *harness) fixTask(w *WorkItem) *ai.Task {
	h.t.Helper()
	if w.TaskID == nil {
		h.t.Fatalf("工作项 #%d 没有任务（%s）", w.ID, w.State)
	}
	return h.task(*w.TaskID)
}

// request 返回请求。
func (h *harness) request(id int64) *ai.Request {
	h.t.Helper()
	r, err := ai.GetRequest(h.ctx, h.db, id, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func TestFixLoop(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	head := w.HeadSHA

	// 还有 CI 运行没结束：不判断
	h.prRun(pr, head, false)
	h.mustProcess()
	h.wantState(w.ID, StatePROpen)
	h.finishPR(pr)

	review := h.failingRun(pr, head)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if w.FixRounds != 1 || w.FixedHead != head {
		t.Fatalf("返修轮次 %d，fixed_head %s", w.FixRounds, w.FixedHead)
	}
	fid := FindingID(review, 0)
	job := h.wantFixTask(w, pr, head, fid)
	// 再处理一次：同一个 head 不再返修，也不重复规划
	h.mustProcess()
	if again := h.workItem(w.ID); again.FixRounds != 1 || *again.TaskID != job.ID {
		t.Fatalf("重复处理后轮次 %d、任务 %d", again.FixRounds, *again.TaskID)
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_requests WHERE type=$1`, agentcfg.TaskDevelop); n != 2 {
		t.Fatalf("AI 开发的请求有 %d 个，想要 2 个（选题、一轮返修）", n)
	}

	// 交付返修：快进正式分支，处置汇总评论在 PR 上
	// 返修由另一个节点执行：工作项记下开发用过的 Agent（PLAN #33.7，同一个 Agent 只记一次）
	aid := h.acquireBy(job.ID, h.addRunner("dev2", "host-2"))
	if w = h.workItem(w.ID); !slices.Equal(w.DevAgents, []string{devConfigAgent}) {
		t.Fatalf("领取后开发过的 Agent = %v", w.DevAgents)
	}
	fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, fid), Branch: devreport.AttemptBranch(aid), SHA: fix})
	if w = h.wantState(w.ID, StateDelivering); !slices.Equal(w.DevAgents, []string{devConfigAgent}) {
		t.Fatalf("开发过的 Agent = %v", w.DevAgents)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if h.branchHead(w.Branch()) != fix || w.HeadSHA != fix || *w.PRNumber != pr {
		t.Fatalf("返修后正式分支 %s、head %s、PR #%d", h.branchHead(w.Branch()), w.HeadSHA, *w.PRNumber)
	}
	want := strconv.Itoa(pr) + ":" + commentDispositions + ":done"
	if c := h.comments(); !slices.Contains(c, want) {
		t.Fatalf("评论 = %v，想要 %s", c, want)
	}
	prs, _ := h.fake.GetPull(h.fakeRepo(), pr)
	if body := strings.Join(prs.Comments, "\n"); !strings.Contains(body, fid) {
		t.Fatalf("PR 评论没有处置 %s：%q", fid, body)
	}

	// 第二轮
	h.failingRun(pr, fix)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	aid = h.acquire(h.fixTask(w).ID)
	fix2 := h.push(devreport.AttemptBranch(aid), fix, map[string]string{"feature.go": "package demo\n\n// 又修了\n"})
	fids := []string{}
	for _, f := range h.specOf(h.fixTask(w).ID).Findings {
		fids = append(fids, f.ID)
	}
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix2, fids...), Branch: devreport.AttemptBranch(aid), SHA: fix2})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)

	// 轮数用完：受阻，在 PR 上说明
	h.failingRun(pr, fix2)
	h.mustProcess()
	w = h.wantState(w.ID, StateBlocked)
	if !strings.Contains(w.Reason, "返修 2 轮") || w.FixedHead != fix2 {
		t.Fatalf("受阻原因 %q", w.Reason)
	}
	if c := h.comments(); !slices.Contains(c, strconv.Itoa(pr)+":"+commentBlocked+":done") {
		t.Fatalf("评论 = %v", c)
	}

	// 人重试：不受轮数限制，从正式分支继续
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if d := h.specOf(h.fixTask(w).ID); d.Round != 3 {
		t.Fatalf("重试的轮次 = %d", d.Round)
	}
}

// wantFixTask 确认第一轮返修的任务：带上审计问题 fid、失败的测试（不含基础设施失败）、工作项的 Issue，从正式分支 head
// 开始，在按工作项的并发组中。
func (h *harness) wantFixTask(w *WorkItem, pr int, head, fid string) *ai.Task {
	h.t.Helper()
	job := h.fixTask(w)
	spec := h.specOf(job.ID)
	if spec.Mode != aispec.DevFix || spec.WorkItemID != w.ID || spec.PRNumber != pr || spec.Round != 1 ||
		len(spec.Findings) != 1 || spec.Findings[0].ID != fid || spec.Findings[0].Path != "feature.go" ||
		len(spec.CIFailures) != 1 || !strings.Contains(spec.CIFailures[0], "test") || len(spec.Issues) != 1 || spec.Issues[0].Number != 1 {
		h.t.Fatalf("返修 spec = %s", jsonString(spec))
	}
	run := h.request(job.RequestID)
	if run.SHA != head || run.Ref != gitref.BranchPrefix+w.Branch() || run.GroupKey == nil || *run.GroupKey != fixGroup(w.ID) {
		h.t.Fatalf("返修从 %s %s 开始（并发组 %v），想要正式分支 %s", run.Ref, run.SHA, run.GroupKey, head)
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_dev_requests WHERE request_id=$1 AND mode=$2 AND work_item_id=$3 AND round=$4`,
		run.ID, aispec.DevFix, w.ID, spec.Round); n != 1 {
		h.t.Fatal("返修请求没有明细")
	}
	return job
}

// TestFixIgnores：基础设施失败、审计未完成（不是 ai.findings）、取消不返修；全部通过时什么也不做。
func TestFixIgnores(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	h.prRun(pr, w.HeadSHA, true,
		prJob{name: "lint", state: ai.TaskFailed, errClass: "infra.lease_expired"},
		prJob{review: true, name: "ai-review", state: ai.TaskFailed, errClass: "ai.incomplete"},
		prJob{name: "test", state: ai.TaskCancelled},
		prJob{name: "build", state: ai.TaskSucceeded},
	)
	h.mustProcess()
	if w = h.wantState(w.ID, StatePROpen); w.FixRounds != 0 || w.FixedHead != "" {
		t.Fatalf("不该返修：%s", jsonString(w))
	}
}

// TestFixTimedOutOnly：head 上只有一个超时的 CI job（错误分类 timeout，不是基础设施）、没有 AI 审计时也返修，返修任务带上这个 job。
func TestFixTimedOutOnly(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	ids := h.prRun(pr, w.HeadSHA, true,
		prJob{name: "slow-test", state: ai.TaskFailed, errClass: "timeout"},
		prJob{name: "build", state: ai.TaskSucceeded},
	)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	spec := h.specOf(h.fixTask(w).ID)
	if len(spec.Findings) != 0 || len(spec.CIFailures) != 1 ||
		!strings.Contains(spec.CIFailures[0], "job #"+strconv.FormatInt(ids[0], 10)+" slow-test") {
		t.Fatalf("返修 spec = %s", jsonString(spec))
	}
}

// TestHumanPush：人推送 PR（head 变了）后按新的 head 判断；旧 head 上的失败不再返修。
func TestHumanPush(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	old := w.HeadSHA
	human := h.commitOn(w.Branch(), map[string]string{"human.go": "package demo\n"}, "人改的")
	if err := h.svc.HandlePullHead(h.ctx, testRepoID, pr, human); err != nil {
		t.Fatal(err)
	}
	if w = h.workItem(w.ID); w.HeadSHA != human {
		t.Fatalf("head = %s", w.HeadSHA)
	}
	h.failingRun(pr, old)
	h.mustProcess()
	h.wantState(w.ID, StatePROpen)
	// 新 head 失败：返修从人推送的 commit 开始
	h.failingRun(pr, human)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if run := h.request(h.fixTask(w).RequestID); run.SHA != human {
		t.Fatalf("返修起点 %s，想要 %s", run.SHA, human)
	}
}

// TestFixCompleteBeforeAssociation：规划返修时请求已经提交、记到工作项上之前失败（task_id 仍为空），
// 任务照样被领取并完成：Complete 关联这一轮的任务，保存交付，之后照常推进；别的轮次的任务不关联。
func TestFixCompleteBeforeAssociation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		oldRound bool
	}{{"这一轮的任务", false}, {"别的轮次的任务", true}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "加功能", "要做 a")
			w, pr := h.developed(1)
			head := w.HeadSHA
			review := h.failingRun(pr, head)
			h.mustProcess()
			w = h.wantState(w.ID, StateFixing)
			job := h.wantFixTask(w, pr, head, FindingID(review, 0))
			h.exec(`UPDATE dev_work_items SET task_id=NULL WHERE id=$1`, w.ID)
			if tt.oldRound {
				h.exec(`UPDATE ai_tasks SET spec = jsonb_set(spec, '{round}', to_jsonb($2::int)) WHERE id=$1`, job.ID, w.FixRounds-1)
			}

			aid := h.acquire(job.ID)
			fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
			h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, FindingID(review, 0)), Branch: devreport.AttemptBranch(aid), SHA: fix})
			if tt.oldRound {
				if w = h.wantState(w.ID, StateFixing); w.TaskID != nil {
					t.Fatalf("别的轮次的任务不应关联：%v", *w.TaskID)
				}
				return
			}
			w = h.wantState(w.ID, StateDelivering)
			if w.TaskID == nil || *w.TaskID != job.ID || w.DeliverySHA != fix {
				t.Fatalf("Complete 后任务 %v、交付 %s", w.TaskID, w.DeliverySHA)
			}
			if n := h.queryInt(`SELECT count(*) FROM dev_branches WHERE work_item_id=$1 AND branch=$2 AND kind='delivery'`, w.ID, devreport.AttemptBranch(aid)); n != 1 {
				t.Fatalf("这一轮的交付分支记录 %d 个", n)
			}
			h.mustProcess()
			if w = h.wantState(w.ID, StatePROpen); w.HeadSHA != fix {
				t.Fatalf("推进后 head = %s，应为 %s", w.HeadSHA, fix)
			}
		})
	}
}

// TestFixAssociationAfterRetry：第 1 轮返修的请求已经提交、还没关联时，任务被领取并失败（Complete 关联它、
// 工作项受阻），人随即重试开了第 2 轮；之后第 1 轮的关联不能写到第 2 轮上，第 2 轮规划自己的任务。
func TestFixAssociationAfterRetry(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	h.failingRun(pr, w.HeadSHA)
	var first int64
	// 规划成功之后、关联到工作项之前执行一次（模拟两步之间发生的事）
	h.svc.afterPlan = func(task *ai.Task) {
		h.svc.afterPlan = nil
		first = task.ID
		h.complete(h.acquire(task.ID), store.OutcomeFailed, nil)
		h.wantState(w.ID, StateBlocked)
		if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
			h.t.Fatal(err)
		}
	}
	h.mustProcess()
	if w = h.wantState(w.ID, StateFixing); w.FixRounds != 2 || w.TaskID != nil {
		t.Fatalf("重试后轮次 %d、任务 %v（第 1 轮的任务是 %d）", w.FixRounds, w.TaskID, first)
	}
	h.mustProcess()
	if w = h.wantState(w.ID, StateFixing); w.TaskID == nil || *w.TaskID == first || h.specOf(*w.TaskID).Round != 2 {
		t.Fatalf("第 2 轮应规划自己的任务：%v（第 1 轮 %d）", w.TaskID, first)
	}
}

// 返修进行中到达的 PR head 事件不能改掉判断依据：乱序的旧 head（或者返修期间的分支变化）会让返修从错的 head 开始、
// 或者在交付后按它重复判断。只在 pr_open 记录 PR head。
func TestPullHeadIgnoredWhileFixing(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	head := w.HeadSHA
	h.failingRun(pr, head)
	h.mustProcess()
	h.wantState(w.ID, StateFixing) // 返修任务在飞

	newer := h.push(w.Branch(), head, map[string]string{"queue.txt": "队列更新的分支\n"})
	if err := h.svc.HandlePullHead(h.ctx, testRepoID, pr, newer); err != nil {
		t.Fatal(err)
	}
	if w = h.workItem(w.ID); w.HeadSHA != head {
		t.Fatalf("返修进行中 head 被改成了 %s（想要 %s）", w.HeadSHA, head)
	}
}

// TestCIFailureText：CI 失败说明带 job 编号；run 因配置错误直接失败（JobID 为 0）时只写 workflow 名和原因，
// 不能写成 "job #0"。
func TestCIFailureText(t *testing.T) {
	if got := ciFailureText(CIFailure{JobID: 12, Name: "test", Reason: "退出码 1"}); got != "job #12 test：退出码 1" {
		t.Errorf("有 job 时 = %q", got)
	}
	if got := ciFailureText(CIFailure{Name: "ci", Reason: "运行配置错误：未知的字段 foo"}); got != "ci：运行配置错误：未知的字段 foo" {
		t.Errorf("没有 job 时 = %q", got)
	}
}

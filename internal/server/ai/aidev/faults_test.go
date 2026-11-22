package aidev

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 外部写操作的失败处理（PLAN #26.12）：每一步注入"写入生效但响应丢失""平台明确失败""读到的状态被别人改过"，
// 推进可以重复执行，不会建出第二个分支、第二个 PR，评论至多发一次。

// deliver 走到 delivering（交付已记录、还没有推进），返回工作项、attempt 分支和交付的 commit。files 是交付改动的文件。
func (h *harness) deliver(files map[string]string, issues ...int) (w *WorkItem, branch, sha string) {
	h.t.Helper()
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, issues)
	branch = devreport.AttemptBranch(aid)
	sha = h.push(branch, h.branchHead("main"), files)
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered(issues, ""), Branch: branch, SHA: sha})
	return h.wantState(res.WorkItemID, StateDelivering), branch, sha
}

// TestPromoteLostResponses：创建正式分支、创建 PR、快进分支的响应丢失（请求已生效）时，下一次处理按平台上的状态继续，
// 只有一个分支、一个 PR。
func TestPromoteLostResponses(t *testing.T) {
	for _, route := range []string{"POST /repos/alice/demo/git/refs", "POST /repos/alice/demo/pulls", "PATCH /repos/alice/demo/pulls/"} {
		t.Run(route, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _, sha := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			if strings.HasPrefix(route, "PATCH") {
				// 已有 PR（例如上一次创建成功、记下之前崩溃）：找到它并更新描述，更新描述的响应丢失
				h.git("branch", w.Branch(), sha)
				n, err := h.svc.forge(h.repoRow()).CreatePull(h.ctx, "t", w.Branch(), "main", "旧的描述")
				if err != nil {
					t.Fatal(err)
				}
				route += strconv.Itoa(n)
			}
			h.fake.SetFault(route, fakegithub.Fault{Lost: true})
			if err := h.process(); err == nil {
				t.Fatal("响应丢失时没有返回错误")
			}
			h.wantState(w.ID, StateDelivering)
			h.mustProcess()
			w = h.wantState(w.ID, StatePROpen)
			if h.branchHead(w.Branch()) != sha {
				t.Fatalf("正式分支 = %s", h.branchHead(w.Branch()))
			}
			pulls, err := h.svc.forge(h.repoRow()).PullsByHead(h.ctx, w.Branch())
			if err != nil || len(pulls) != 1 || pulls[0].Number != *w.PRNumber {
				t.Fatalf("PR = %+v, %v（想要恰好一个 #%d）", pulls, err, *w.PRNumber)
			}
			if pr, _ := h.fake.GetPull(h.repo, *w.PRNumber); !strings.Contains(pr.Body, "Fixes #1") {
				t.Fatalf("PR 描述没有更新：%q", pr.Body)
			}
		})
	}
}

// TestPromoteTransientFailure：平台暂时失败（5xx，没有生效）时保持 delivering，下一次照常推进。
func TestPromoteTransientFailure(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	h.fake.SetFault("GET /repos/alice/demo/compare/main..."+w.DeliverySHA, fakegithub.Fault{Status: http.StatusBadGateway})
	if err := h.process(); err == nil {
		t.Fatal("暂时失败时没有返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	h.mustProcess()
	h.wantState(w.ID, StatePROpen)
}

// TestPromoteRejects：核对不通过时转为受阻（不动正式分支），原因说明给人看，评论在 Issue 上：attempt 分支不是交付的 commit、
// 交付相对目标分支没有新的提交、正式分支被人推送过（分叉，I5：只快进）。
func TestPromoteRejects(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(h *harness, w *WorkItem, branch, sha string)
		reason string
	}{
		{"attempt 分支被删除", func(h *harness, _ *WorkItem, branch, _ string) { h.git("branch", "-D", branch) }, "attempt 分支"},
		{"attempt 分支被改动", func(h *harness, _ *WorkItem, branch, sha string) {
			h.commitOn(branch, map[string]string{"evil.go": "x"}, "别人推送")
		}, "attempt 分支"},
		{"正式分支分叉", func(h *harness, w *WorkItem, _, _ string) {
			h.git("branch", w.Branch(), "main")
			h.commitOn(w.Branch(), map[string]string{"other.go": "x"}, "人推送")
		}, "分叉"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, branch, sha := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			tt.setup(h, w, branch, sha)
			formal := h.branchHead(w.Branch())
			h.mustProcess()
			w = h.wantState(w.ID, StateBlocked)
			if !strings.Contains(w.Reason, tt.reason) || w.PRNumber != nil {
				t.Fatalf("受阻原因 = %q", w.Reason)
			}
			if h.branchHead(w.Branch()) != formal {
				t.Fatal("核对不通过时动了正式分支")
			}
			if c := h.comments(); !slices.Contains(c, "1:blocked:done") {
				t.Fatalf("评论 = %v", c)
			}
			if l := h.labels(1); !slices.Contains(l, LabelBlocked) {
				t.Fatalf("label = %v", l)
			}
		})
	}

	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	// 交付的 commit 就是目标分支的 head（Runner 校验被绕过）：没有新的提交
	branch := devreport.AttemptBranch(aid)
	h.git("branch", branch, "main")
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: branch, SHA: h.branchHead("main")})
	h.mustProcess()
	if w := h.wantState(res.WorkItemID, StateBlocked); !strings.Contains(w.Reason, "没有新的提交") {
		t.Fatalf("原因 = %q", w.Reason)
	}
}

// TestCommentsAtMostOnce：评论没有幂等键（C19）：响应丢失、5xx 记为 unknown 不重发；被拒记为 failed；发送时崩溃
// （attempted 留在库里）下一次记为 unknown；限流（没有执行）回到 pending 稍后再发。
func TestCommentsAtMostOnce(t *testing.T) {
	route := "POST /repos/alice/demo/issues/1/comments"
	tests := []struct {
		name  string
		fault *fakegithub.Fault
		crash bool
		want  string
		sent  int
	}{
		{name: "响应丢失", fault: &fakegithub.Fault{Lost: true}, want: CommentUnknown, sent: 1},
		{name: "服务端错误", fault: &fakegithub.Fault{Status: http.StatusBadGateway}, want: CommentUnknown, sent: 0},
		{name: "被拒", fault: &fakegithub.Fault{Status: http.StatusForbidden}, want: CommentFailed, sent: 0},
		{name: "发送时崩溃", crash: true, want: CommentUnknown, sent: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			aid := h.acquire(h.taskOf(h.startSelect()).ID)
			h.mustClaim(aid, []int{1})
			if tt.crash {
				h.exec(`UPDATE dev_comments SET state=$1`, CommentAttempted)
			}
			if tt.fault != nil {
				h.fake.SetFault(route, *tt.fault)
			}
			_ = h.process()
			h.mustProcess()
			h.mustProcess()
			if c := h.comments(); !slices.Equal(c, []string{"1:claimed:" + tt.want}) {
				t.Fatalf("评论 = %v", c)
			}
			is, _ := h.fake.GetIssue(repoName, 1)
			if len(is.Comments) != tt.sent {
				t.Fatalf("实际发出 %d 条评论，想要 %d", len(is.Comments), tt.sent)
			}
			if n := h.countRequests(route); n > 1 {
				t.Fatalf("评论请求了 %d 次", n)
			}
		})
	}

	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	h.mustClaim(aid, []int{1})
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{
		Status: http.StatusTooManyRequests, Until: time.Now().Add(time.Hour), Match: func(r *http.Request) bool { return r.Method+" "+r.URL.Path == route },
	})
	if err := h.process(); err == nil {
		t.Fatal("限流时没有返回错误")
	}
	if c := h.comments(); !slices.Equal(c, []string{"1:claimed:pending"}) {
		t.Fatalf("限流后评论 = %v", c)
	}
	// 限流结束（客户端记住了限流的截止时间，换一个客户端代替等待）
	h.fake.SetRateLimit(h.fake.InstallationID, nil)
	h.svc.forge, h.svc.tokenForge = h.newForge(), h.newTokenForge()
	h.mustProcess()
	if c := h.comments(); !slices.Equal(c, []string{"1:claimed:done"}) {
		t.Fatalf("恢复后评论 = %v", c)
	}
}

// TestHoldPendingUntilLabeled：要加 kiln:hold 的交付，移动正式分支、建 PR 之前先记下 hold_pending；
// 加 label 失败时 PR 已经开了、没有 label，hold_pending 仍为真（合并队列按它不自动入队）；加上之后才清除。
func TestHoldPendingUntilLabeled(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"AGENTS.md": "x\n"}, 1)
	// Issue #1 之后的第一个编号是 PR #2
	h.fake.SetFault("POST /repos/alice/demo/issues/2/labels", fakegithub.Fault{Status: http.StatusBadGateway})
	if err := h.process(); err == nil {
		t.Fatal("加 label 失败时没有返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	pr, ok := h.fake.GetPull(h.repo, 2)
	if !ok || slices.Contains(pr.Labels, mergecfg.HoldLabel) {
		t.Fatalf("PR #2 应已开、没有 %s：%+v", mergecfg.HoldLabel, pr)
	}
	if !h.queryBool(`SELECT hold_pending FROM dev_work_items WHERE id=$1`, w.ID) {
		t.Fatal("kiln:hold 还没有加上时 hold_pending 应为真")
	}
	h.mustProcess()
	h.wantState(w.ID, StatePROpen)
	if pr, _ := h.fake.GetPull(h.repo, 2); !slices.Contains(pr.Labels, mergecfg.HoldLabel) {
		t.Fatalf("PR #2 的 label = %v", pr.Labels)
	}
	if h.queryBool(`SELECT hold_pending FROM dev_work_items WHERE id=$1`, w.ID) {
		t.Fatal("加上 kiln:hold 之后 hold_pending 应已清除")
	}
}

// TestPullClosedBeforeRecorded（第四、五轮 AI 审计）：建 PR 的响应丢失（PR 已建、编号没有记下）之后人关闭了它：关闭事件
// 按正式分支找到工作项并终止；事件丢失（或还没到）时，下一次推进看到正式分支上只有关闭了的 PR，同样终止，不再建新的
// PR。PR 已经合并时转 merged，晚到的合并事件不再改变。都记下 PR 编号。
func TestPullClosedBeforeRecorded(t *testing.T) {
	for _, tt := range []struct {
		name   string
		event  bool
		merged bool
	}{{"关闭事件按分支关联", true, false}, {"关闭事件丢失", false, false}, {"已经合并、合并事件晚到", false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
			if err := h.process(); err == nil {
				t.Fatal("响应丢失时没有返回错误")
			}
			h.wantState(w.ID, StateDelivering)
			// Issue #1 之后的第一个编号是 PR #2
			want := StateStopped
			if tt.merged {
				if _, err := h.fake.MergePull(h.repo, 2, "squash"); err != nil {
					t.Fatal(err)
				}
				want = StateMerged
			} else {
				h.fake.UpdatePull(h.repo, 2, func(p *fakegithub.Pull) { p.State = "closed" })
			}
			if tt.event {
				msg, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: 2, HeadRef: w.Branch(), Merged: false}, "bob")
				if err != nil || !strings.Contains(msg, "被关闭") {
					t.Fatalf("关闭事件：%q, %v", msg, err)
				}
				h.wantState(w.ID, StateStopped)
			}
			h.mustProcess()
			if w = h.wantState(w.ID, want); w.PRNumber == nil || *w.PRNumber != 2 {
				t.Fatalf("PR 编号 = %v，应为 2", w.PRNumber)
			}
			if tt.merged {
				if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: 2, HeadRef: w.Branch(), Merged: true}, "bob"); err != nil {
					t.Fatal(err)
				}
				h.wantState(w.ID, StateMerged)
			}
			pulls, err := h.svc.forge(h.repoRow()).PullsByHead(h.ctx, w.Branch())
			if err != nil || len(pulls) != 1 {
				t.Fatalf("正式分支上的 PR = %+v（%v），只应有关闭了的 #2", pulls, err)
			}
		})
	}
}

// TestSettleUnrecorded：PR 编号还没记下（建 PR 的响应丢失）时 PR 已经合并：合并时 GitHub 关闭 Issue 的
// 事件先到也不终止工作项，合并事件按分支找到它转 merged；普通合并（交付已经在目标分支上）且事件都丢失时，执行者先按
// 正式分支上的 PR 结束为 merged，不按"没有新的提交"判为不通过。没有 PR、Issue 却全部关闭时由执行者终止，不再建 PR。
func TestSettleUnrecorded(t *testing.T) {
	closeIssue := func(h *harness) {
		h.editIssue(1, func(is *fakegithub.Issue) { is.State = "closed" })
		h.event(1, IssueClosed, "")
	}
	lostCreate := func(h *harness, w *WorkItem) {
		h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
		if err := h.process(); err == nil {
			h.t.Fatal("响应丢失时没有返回错误")
		}
		h.wantState(w.ID, StateDelivering)
	}
	wantPR := func(h *harness, w *WorkItem, state string) {
		h.t.Helper()
		if w = h.wantState(w.ID, state); w.PRNumber == nil || *w.PRNumber != 2 {
			h.t.Fatalf("PR 编号 = %v，应为 2", w.PRNumber)
		}
	}
	t.Run("squash 合并，Issue 关闭事件先到", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
		lostCreate(h, w)
		if _, err := h.fake.MergePull(h.repo, 2, "squash"); err != nil {
			t.Fatal(err)
		}
		closeIssue(h)
		h.wantState(w.ID, StateDelivering)
		if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{Number: 2, HeadRef: w.Branch(), Merged: true}, "bob"); err != nil {
			t.Fatal(err)
		}
		wantPR(h, w, StateMerged)
	})
	t.Run("普通合并，事件都丢失", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
		lostCreate(h, w)
		if _, err := h.fake.MergePull(h.repo, 2, "merge"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		wantPR(h, w, StateMerged)
	})
	t.Run("关闭之后另开的 PR 合并，事件都丢失", func(t *testing.T) {
		// 按最新的 PR 结束：#2 关闭、同一个分支另开的 #3 合并
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
		lostCreate(h, w)
		h.fake.UpdatePull(h.repo, 2, func(p *fakegithub.Pull) { p.State = "closed" })
		n, err := h.svc.forge(h.repoRow()).CreatePull(h.ctx, "另开", w.Branch(), h.repo.DefaultBranch, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.fake.MergePull(h.repo, n, "squash"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		if w = h.wantState(w.ID, StateMerged); w.PRNumber == nil || *w.PRNumber != n {
			t.Fatalf("PR 编号 = %v，应为 %d", w.PRNumber, n)
		}
	})
	t.Run("没有 PR，Issue 全部关闭", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "一", "a")
		w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
		closeIssue(h)
		h.wantState(w.ID, StateDelivering)
		h.mustProcess()
		if w = h.wantState(w.ID, StateStopped); w.Reason != reasonIssuesClosed {
			t.Fatalf("原因 = %q", w.Reason)
		}
		if pulls, err := h.svc.forge(h.repoRow()).PullsByHead(h.ctx, w.Branch()); err != nil || len(pulls) != 0 {
			t.Fatalf("Issue 全部关闭后不应再建 PR：%+v（%v）", pulls, err)
		}
	})
}

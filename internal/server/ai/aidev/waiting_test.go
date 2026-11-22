package aidev

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
)

// 依赖等待（PLAN #31.6）：选题时未选、写了 wait_for 的 Issue 在依赖关闭之前不回到候选。依赖的关闭不管在复查之前、
// 复查之后（issues.closed 事件）还是事件丢了（手动同步任务池后复查）都不会错过。

// claimWaiting 领取 #1，#2 等 #3 关闭（#3 不在任务池中）。
func (h *harness) claimWaiting() {
	h.t.Helper()
	h.issue(1, "升级历史", "a")
	h.issue(2, "按状态筛选", "依赖 #3")
	h.issue(3, "历史表", "b", "bug")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	f := false
	sel := devreport.Selection{
		Version: devreport.Version, Selected: []int{1}, Plan: "先改 a", BatchReason: "一个", DesignFirst: &f, DesignReason: "小",
		Skipped: []devreport.Skip{{Number: 2, Reason: "要等历史表", WaitFor: []int{3, 3}}},
	}
	if res, err := h.claimSel(aid, []int{1}, devreport.Marshal(&sel)); err != nil || !res.Claimed {
		h.t.Fatalf("领取：%+v, %v", res, err)
	}
}

// wantPool 确认任务池中 #2 的情况。
func (h *harness) wantPool2(status string, waiting ...int) {
	h.t.Helper()
	view, err := GetPool(h.ctx, h.db, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range view.Issues {
		if p.Number != 2 {
			continue
		}
		if p.Status != status || !slices.Equal(p.WaitingOn, waiting) {
			h.t.Fatalf("#2 在任务池中是 %s %v（%s），想要 %s %v", p.Status, p.WaitingOn, p.Reason, status, waiting)
		}
		if status == PoolWaiting && !strings.Contains(p.Reason, "等待 #3 关闭") {
			h.t.Fatalf("等待的说明 = %q", p.Reason)
		}
		return
	}
	h.t.Fatal("#2 不在任务池中")
}

func (h *harness) closeIssue(n int) {
	h.editIssue(n, func(is *fakegithub.Issue) { is.State = "closed" })
}

func (h *harness) waitChecked() bool {
	return h.queryBool(`SELECT waiting_checked FROM dev_issue_skips WHERE repo_id=$1 AND issue_number=2`, testRepoID)
}

func TestWaitFor(t *testing.T) {
	t.Run("复查之后关闭：事件放出等待", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.claimWaiting()
		if h.waitChecked() {
			t.Fatal("登记的等待要标记为未复查")
		}
		h.wantPool2(PoolWaiting, 3)
		h.mustProcess()
		if !h.waitChecked() {
			t.Fatal("执行者没有复查")
		}
		h.wantPool2(PoolWaiting, 3)
		// 别的 Issue 关闭不影响
		h.closeIssue(1)
		h.event(1, IssueClosed, "")
		h.wantPool2(PoolWaiting, 3)
		h.closeIssue(3)
		h.event(3, IssueClosed, "")
		h.wantPool2(PoolCandidate)
	})
	t.Run("复查之前已经关闭", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.claimWaiting()
		h.closeIssue(3) // 事件还没到（或者丢了）
		h.mustProcess()
		h.wantPool2(PoolCandidate)
	})
	t.Run("事件丢了：手动同步后复查", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.claimWaiting()
		h.mustProcess()
		h.closeIssue(3)
		before := h.countRequests("GET /repos/" + repoName + "/issues/3")
		h.mustProcess()
		if h.countRequests("GET /repos/"+repoName+"/issues/3") != before {
			t.Fatal("已经复查过的等待不能每次都读 GitHub")
		}
		h.wantPool2(PoolWaiting, 3)
		if _, err := h.svc.SyncPool(h.ctx, testRepoID, "admin"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		h.wantPool2(PoolCandidate)
	})
	t.Run("依赖读不到：不再等待", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.claimWaiting()
		h.exec(`UPDATE dev_issue_skips SET waiting_on='{3,999}' WHERE repo_id=$1 AND issue_number=2`, testRepoID)
		h.mustProcess()
		h.wantPool2(PoolWaiting, 3)
		h.closeIssue(3)
		h.event(3, IssueClosed, "")
		h.wantPool2(PoolCandidate)
	})
}

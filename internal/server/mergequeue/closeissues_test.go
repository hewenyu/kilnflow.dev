package mergequeue

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

const (
	thisRepo  = testAccount + "/demo"
	otherRepo = testAccount + "/other"
)

// issuesState 返回 PR 最近一个队列项的 issues_state。
func (h *harness) issuesState(n int) string {
	h.t.Helper()
	var st string
	if err := h.db.QueryRow(h.ctx, `SELECT issues_state FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 ORDER BY id DESC LIMIT 1`,
		repoID, n).Scan(&st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// wantIssues 断言 PR 的队列项已合并、issues_state 为 state、状态包含 status。
func (h *harness) wantIssues(n int, state, status string) *Entry {
	h.t.Helper()
	e := h.entry(n)
	if st := h.issuesState(n); e.State != StateMerged || e.Reason != "" || st != state || !strings.Contains(e.Status, status) {
		h.t.Fatalf("PR #%d：state=%s reason=%q issues_state=%s status=%q，想要已合并、%s、状态包含 %q", n, e.State, e.Reason, st, e.Status, state, status)
	}
	return e
}

// mergeWithIssues 让 PR #7 关联 refs，setup 非空时在合并前调用，然后由合并队列合并（同一次处理中关闭关联的 issue）。
func (h *harness) mergeWithIssues(setup func(p *fakegithub.Pull), refs ...fakegithub.IssueRef) {
	h.t.Helper()
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.ClosingIssues = refs
		if setup != nil {
			setup(p)
		}
	})
	h.verified(7, head, store.RunSucceeded)
	h.process()
}

func (h *harness) issue(repo string, n int) fakegithub.Issue {
	h.t.Helper()
	is, ok := h.fake.GetIssue(repo, n)
	if !ok {
		h.t.Fatalf("没有 issue %s#%d", repo, n)
	}
	return is
}

// TestCloseLinkedIssues：队列合并 PR 后，执行者关闭它关联的、同一仓库中打开的 issue（state_reason completed）并留言说明
// PR 和合并提交；已经关闭的 issue 和其他仓库的 issue 不处理；完成后不再请求平台。
func TestCloseLinkedIssues(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3, UserComments: []string{"复现步骤"}})
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 4, State: "closed"})
	h.fake.SetIssue(otherRepo, fakegithub.Issue{Number: 5})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3}, fakegithub.IssueRef{Number: 4}, fakegithub.IssueRef{Repo: otherRepo, Number: 5})

	e := h.wantIssues(7, issuesDone, "已关闭关联的 issue #3")
	is := h.issue(thisRepo, 3)
	if is.State != "closed" || is.StateReason != "completed" || is.Closes != 1 {
		t.Fatalf("#3 应以 completed 关闭：%+v", is)
	}
	if len(is.Comments) != 1 || !strings.Contains(is.Comments[0], "PR #7") || !strings.Contains(is.Comments[0], e.MergeSHA) {
		t.Fatalf("#3 的留言应说明 PR 和合并提交 %s：%q", e.MergeSHA, is.Comments)
	}
	if is := h.issue(thisRepo, 4); is.Closes != 0 || len(is.Comments) != 0 {
		t.Fatalf("已经关闭的 #4 不应处理：%+v", is)
	}
	if is := h.issue(otherRepo, 5); is.State != "open" || len(is.Comments) != 0 {
		t.Fatalf("其他仓库的 issue 不应处理：%+v", is)
	}
	for _, r := range h.fake.Requests() {
		if strings.Contains(r, "/"+otherRepo+"/") {
			t.Fatalf("不应请求其他仓库：%s", r)
		}
	}
	var audits int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM audit_log WHERE action='mergequeue.issue_closed' AND (detail->>'issue')::int=3`).
		Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("审计日志 %d 条（%v），想要一条", audits, err)
	}
	if p := h.pull(7); len(p.Comments) != 0 {
		t.Fatalf("不应在 PR 上评论：%q", p.Comments)
	}

	// 已经完成：之后的处理不再请求平台
	mark := len(h.fake.Requests())
	h.process()
	if reqs := h.fake.Requests()[mark:]; len(reqs) != 0 {
		t.Fatalf("完成后不应再请求平台：%v", reqs)
	}
}

// TestCloseIssueFailure：关闭被拒（App 没有 Issues 写权限）时合并结果不受影响，原因和权限说明写在状态中，下一次处理重试；
// 有了权限后重试关闭，不重复留言。
func TestCloseIssueFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	forbidden := &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3, CloseErr: forbidden})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3})

	h.wantIssues(7, issuesPending, "失败（第 1 次")
	if e := h.entry(7); !strings.Contains(e.Status, "not accessible") || !strings.Contains(e.Status, "Issues 的读写权限") {
		t.Fatalf("状态应包含原因和权限说明：%q", e.Status)
	}
	if p := h.pull(7); !p.Merged {
		t.Fatalf("PR 应已合并：%+v", p)
	}
	if is := h.issue(thisRepo, 3); is.State != "open" || len(is.Comments) != 1 {
		t.Fatalf("留言成功、关闭失败：%+v", is)
	}
	h.process()
	h.wantIssues(7, issuesPending, "失败（第 2 次")

	// 有了权限之后重试：关闭；留言已经尝试过（commenting），不再留言
	h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.CloseErr = nil })
	h.process()
	h.wantIssues(7, issuesDone, "#3")
	if is := h.issue(thisRepo, 3); is.State != "closed" || is.Closes != 1 || len(is.Comments) != 1 {
		t.Fatalf("重试应关闭 issue 且不重复留言：%+v", is)
	}
}

// TestCloseIssuesForbidden：没有 Issues 权限时读取关联被拒，记下原因（带权限说明）；合并超过一天仍失败时放弃。
func TestCloseIssuesForbidden(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.mergeWithIssues(func(p *fakegithub.Pull) { p.ClosingIssuesForbidden = true }, fakegithub.IssueRef{Number: 3})

	h.wantIssues(7, issuesPending, "Issues 的读写权限")
	if is := h.issue(thisRepo, 3); is.State != "open" || len(is.Comments) != 0 {
		t.Fatalf("没有权限时不应修改 issue：%+v", is)
	}
	// 兜底检查也会通知有待关闭项的仓库
	h.exec(`DELETE FROM outbox`)
	if err := h.svc.sweep(h.ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind='mergequeue.repo'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("兜底检查应通知仓库：%d 条（%v）", n, err)
	}

	h.exec(`UPDATE merge_queue SET finished_at=now()-interval '25 hours' WHERE pr_number=7`)
	h.process()
	h.wantIssues(7, issuesFailed, "已放弃")
	mark := len(h.fake.Requests())
	h.process()
	if reqs := h.fake.Requests()[mark:]; len(reqs) != 0 {
		t.Fatalf("放弃后不应再请求平台：%v", reqs)
	}
}

// TestCloseIssuesRateLimited：读取关联被限流时不计失败次数、不报错（不阻塞队列），下一次处理再关闭。合并前的读取也被限流时
// 照常合并，合并后再读取。
func TestCloseIssuesRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.limitIssues.Store(2)
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3})
	h.wantIssues(7, issuesPending, statusClosingIssues)
	var tries int
	if err := h.db.QueryRow(h.ctx, `SELECT issues_tries FROM merge_queue WHERE pr_number=7`).Scan(&tries); err != nil || tries != 0 {
		t.Fatalf("限流不计失败次数：%d（%v）", tries, err)
	}
	h.process()
	h.wantIssues(7, issuesDone, "#3")
}

// TestReopenedIssueNotClosedAgain：Kiln 关闭后被人重新打开的 issue 不会再被关闭（即使队列项又被处理）；
// 明确被拒（没有权限）的 #4 在之后的处理中关闭。
func TestReopenedIssueNotClosedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 4, CloseErr: &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3}, fakegithub.IssueRef{Number: 4})
	h.wantIssues(7, issuesPending, "关闭 issue #4")
	var closed []int32
	if err := h.db.QueryRow(h.ctx, `SELECT issues_closed FROM merge_queue WHERE pr_number=7`).Scan(&closed); err != nil || !slices.Equal(closed, []int32{3}) {
		t.Fatalf("issues_closed = %v（%v）", closed, err)
	}

	h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.State = "open" })
	h.fake.UpdateIssue(thisRepo, 4, func(is *fakegithub.Issue) { is.CloseErr = nil })
	h.process()
	h.wantIssues(7, issuesDone, "#3、#4")
	if is := h.issue(thisRepo, 3); is.State != "open" || is.Closes != 1 || len(is.Comments) != 1 {
		t.Fatalf("重新打开的 issue 不应被再次关闭：%+v", is)
	}
	if is := h.issue(thisRepo, 4); is.State != "closed" || len(is.Comments) != 1 {
		t.Fatalf("#4 应在重试时关闭、不重复留言：%+v", is)
	}
}

// TestCloseIssueUncertainNotRetried：关闭请求的结果不明（5xx，平台实际上没有关闭）时不再请求关闭：之后读到仍然打开，
// 记为结果不明、交给人处理（最多尝试一次，PLAN #29.8）。
func TestCloseIssueUncertainNotRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3, CloseErr: &fakegithub.APIErr{Status: http.StatusBadGateway, Message: "boom"}})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3})
	h.wantIssues(7, issuesPending, "结果不明")
	h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.CloseErr = nil })
	h.process()
	h.wantIssues(7, issuesDone, "#3 的结果不明")
	if is := h.issue(thisRepo, 3); is.State != "open" || is.Closes != 0 || len(is.Comments) != 1 {
		t.Fatalf("结果不明后不应再关闭：%+v", is)
	}
	mark := len(h.fake.Requests())
	h.process()
	if reqs := h.fake.Requests()[mark:]; len(reqs) != 0 {
		t.Fatalf("完成后不应再请求平台：%v", reqs)
	}
}

// TestUpgradeUncertainIssueNotClosedAgain（migration 0021，独立审查发现）：升级前旧版本已经留言并请求关闭 #3、结果不明
// （issues_uncertain），之后用户重新打开了它：新版本不再留言、不再关闭，记为结果不明交给人；没有尝试过的 #4 照常关闭。
func TestUpgradeUncertainIssueNotClosedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	forbidden := &fakegithub.APIErr{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3, CloseErr: forbidden})
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 4})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3}, fakegithub.IssueRef{Number: 4})
	h.wantIssues(7, issuesPending, "关闭 issue #3")
	// 模拟升级前的状态：旧版本对 #3 留了言、请求过关闭（结果不明），还没有处理 #4；迁移后 issues 为 NULL
	h.exec(`UPDATE merge_queue SET issues=NULL, issues_uncertain='{3}' WHERE pr_number=7`)
	h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.CloseErr = nil })
	h.process()
	h.wantIssues(7, issuesDone, "#3 的结果不明")
	if is := h.issue(thisRepo, 3); is.State != "open" || is.Closes != 0 || len(is.Comments) != 1 {
		t.Fatalf("升级前请求过关闭的 issue 不应再留言或关闭：%+v", is)
	}
	if is := h.issue(thisRepo, 4); is.State != "closed" || is.Closes != 1 || len(is.Comments) != 1 {
		t.Fatalf("没有尝试过的 #4 应照常留言并关闭：%+v", is)
	}
}

// TestIssueCommentCrashNotRepeated（崩溃点，PLAN #29.8，独立审查发现）：在 issue 上的留言已经生效、记录下一步（closing）
// 之前进程中断：留言之前已经记下 commenting，重启后的处理不再留言，直接关闭。
func TestIssueCommentCrashNotRepeated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.ClosingIssues = []fakegithub.IssueRef{{Number: 3}} })
	h.verified(7, head, store.RunSucceeded)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.afterComment = cancel // 留言生效后"进程中断"：之后的数据库写入和请求都失败
	_ = h.svc.Process(ctx, repoID)
	if is := h.issue(thisRepo, 3); len(is.Comments) != 1 || is.State != "open" {
		t.Fatalf("中断时应已留言、还没关闭：%+v", is)
	}
	h.process()
	h.wantIssues(7, issuesDone, "#3")
	if is := h.issue(thisRepo, 3); len(is.Comments) != 1 || is.State != "closed" || is.Closes != 1 {
		t.Fatalf("重启后不应再留言，直接关闭：%+v", is)
	}
}

// TestAnotherPRClosesReopenedIssue：PR #7 合并后关闭了 #3，用户重新打开（问题没有修好），随后 PR #8 也关联 #3 并合并：
// PR #7 那次关闭不算 PR #8 的，PR #8 照常留言并关闭 #3。
func TestAnotherPRClosesReopenedIssue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3})
	h.wantIssues(7, issuesDone, "#3")
	h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.State = "open" })

	head := h.branch("fix-again")
	h.openPR(8, "fix-again", head)
	h.fake.UpdatePull(h.repo, 8, func(p *fakegithub.Pull) { p.ClosingIssues = []fakegithub.IssueRef{{Number: 3}} })
	h.verified(8, head, store.RunSucceeded)
	h.process()
	h.wantIssues(8, issuesDone, "#3")
	is := h.issue(thisRepo, 3)
	if is.State != "closed" || is.Closes != 2 || len(is.Comments) != 2 || !strings.Contains(is.Comments[1], "PR #8") {
		t.Fatalf("PR #8 应留言并再次关闭 #3：%+v", is)
	}
}

// TestInactiveRepoGivesUpIssues：仓库停用后不再关闭关联的 issue。
func TestInactiveRepoGivesUpIssues(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.mergeWithIssues(func(p *fakegithub.Pull) { p.ClosingIssuesForbidden = true }, fakegithub.IssueRef{Number: 3})
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	h.process()
	h.wantIssues(7, issuesFailed, "仓库已停用")
}

// TestMergeResponseLost：平台已经合并、但合并请求的响应丢失时，下一次处理看到 PR 已合并，仍按队列自己合并的处理并关闭关联的 issue。
func TestMergeResponseLost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.ClosingIssues = []fakegithub.IssueRef{{Number: 3}} })
	h.verified(7, head, store.RunSucceeded)
	h.loseMergeResponses.Store(1)

	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("合并的响应丢失应返回错误（稍后重试）")
	}
	if e := h.entry(7); e.State != StateQueued || e.Action != actionMerging {
		t.Fatalf("应记下合并意图：%+v", e)
	}

	h.process()
	e := h.wantIssues(7, issuesDone, "#3")
	if p := h.pull(7); e.MergeSHA != p.MergeCommitSHA {
		t.Fatalf("合并提交 %s，想要 %s", e.MergeSHA, p.MergeCommitSHA)
	}
	// 平台记录的合并者是 App 自己：自动打 tag 的发送者仍是入队的人（PLAN #29.4）
	h.wantAutoTag(7, e.MergeSHA, testAccount, mqevents.AutoTagFromQueue, Label)
	if is := h.issue(thisRepo, 3); is.State != "closed" || len(is.Comments) != 1 {
		t.Fatalf("应关闭关联的 issue：%+v", is)
	}
}

// TestMergeHeadChangedClearsIntent：合并时平台确认 head 已经变化（没有合并），清掉合并意图，之后在队列之外合并的不当成队列合并的。
func TestMergeHeadChangedClearsIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunSucceeded)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {
		p.MergeErr = &fakegithub.APIErr{Status: http.StatusConflict, Message: "Head branch was modified."}
	})
	// head 没有真的变化，下一轮仍会请求合并；只处理一步，看合并意图是否清掉
	p := &pass{s: h.svc, repo: mustRepo(t, h), f: h.svc.forge(mustRepo(t, h))}
	conn, err := h.db.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	p.conn = conn
	if err := p.loadConfig(h.ctx); err != nil {
		t.Fatal(err)
	}
	e := h.entry(7)
	if wait, err := p.merge(h.ctx, e, nil); err != nil || wait {
		t.Fatalf("merge = %v, %v", wait, err)
	}
	if e := h.entry(7); e.Action != actionNone {
		t.Fatalf("head 变化时应清掉合并意图：%+v", e)
	}
}

func mustRepo(t *testing.T, h *harness) *store.Repo {
	t.Helper()
	r, err := store.GetRepo(h.ctx, h.db, repoID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAuditRemovalCommentLostResponse：出队评论已经生效、响应丢失时，重试不会再评论一次，PR 上只有一条评论。
func TestAuditRemovalCommentLostResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunFailed)
	h.fake.SetFault("POST /repos/"+thisRepo+"/issues/7/comments", fakegithub.Fault{Lost: true})
	h.process()
	h.process()
	if e := h.entry(7); e.State != StateRemoved {
		t.Fatalf("应出队：%+v", e)
	}
	if p := h.pull(7); len(p.Comments) != 1 {
		t.Fatalf("PR 上应只有一条出队评论：%q", p.Comments)
	}
}

// TestRemovalCommentRejectedRetried：出队评论被平台明确拒绝（没有生效）时清掉"已尝试"，下一次处理重新评论。
func TestRemovalCommentRejectedRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunFailed)
	h.fake.SetFault("POST /repos/"+thisRepo+"/issues/7/comments", fakegithub.Fault{Status: http.StatusForbidden})
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("评论被拒应返回错误（稍后重试）")
	}
	if e := h.entry(7); e.State != StateQueued || e.CommentState != "" {
		t.Fatalf("评论被拒时不应出队、应清掉已尝试：%+v", e)
	}
	h.process()
	if e := h.entry(7); e.State != StateRemoved {
		t.Fatalf("应出队：%+v", e)
	}
	if p := h.pull(7); len(p.Comments) != 1 {
		t.Fatalf("重试后应有一条评论：%q", p.Comments)
	}
}

// TestAuditIssueCommentStaleRead：在 issue 上的留言已经生效、响应丢失（之后读到的数据里也看不到这条留言）：
// 不会再留言，issue 照常关闭。
func TestAuditIssueCommentStaleRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3})
	h.fake.SetFault("POST /repos/"+thisRepo+"/issues/3/comments", fakegithub.Fault{Lost: true})
	h.mergeWithIssues(nil, fakegithub.IssueRef{Number: 3})
	h.process()
	h.wantIssues(7, issuesDone, "#3")
	if is := h.issue(thisRepo, 3); is.State != "closed" || len(is.Comments) != 1 {
		t.Fatalf("应只留言一次并关闭：%+v", is)
	}
}

// TestAuditIssueClosedAtMergeThenReopened：合并时 issue 已经关闭，之后用户重新打开：Kiln 不会留言也不会关闭它
// （GitHub 也只在合并那一刻判断）。合并后第一次处理被限流、晚于重新打开，结果也一样（合并之前已经冻结）。
func TestAuditIssueClosedAtMergeThenReopened(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.SetIssue(thisRepo, fakegithub.Issue{Number: 3, State: "closed"})
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.ClosingIssues = []fakegithub.IssueRef{{Number: 3}} })
	h.verified(7, head, store.RunSucceeded)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) {}) // 无变化：PR 已验证
	// 合并之后第一次读取关联被限流：只限流合并之后的那一次（合并前的读取照常）
	h.afterMerge = func() {
		h.limitIssues.Store(1)
		h.fake.UpdateIssue(thisRepo, 3, func(is *fakegithub.Issue) { is.State = "open" })
	}
	h.process()
	h.process()
	h.wantIssues(7, issuesDone, "")
	if is := h.issue(thisRepo, 3); is.State != "open" || is.Closes != 0 || len(is.Comments) != 0 {
		t.Fatalf("合并时已经关闭、之后重新打开的 issue 不应被处理：%+v", is)
	}
}

// TestRemovalCommentAttemptedBeforeCrash：崩溃点注入——自动出队的评论已经发出（comment_state=attempted）、记录出队之前
// Server 重启：重启后不再评论，直接完成出队。
func TestRemovalCommentAttemptedBeforeCrash(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.verified(7, head, store.RunFailed)
	h.exec(`UPDATE merge_queue SET action='removing', reason='验证没有通过', comment_state='attempted' WHERE pr_number=7`)
	h.process()
	if e := h.entry(7); e.State != StateRemoved {
		t.Fatalf("应出队：%+v", e)
	}
	if p := h.pull(7); len(p.Comments) != 0 {
		t.Fatalf("已经尝试过的评论不应再发：%q", p.Comments)
	}
}

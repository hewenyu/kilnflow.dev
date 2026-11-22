package aidev

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestClaimVerifies：领取前逐个读选中的 Issue；关闭、不再 ready、加了 skip、内容在选题后变了的不领取，给出原因和最新的候选
// （存为这个 attempt 的候选，之后按新的快照核对）。webhook 没有到（丢了）时，领取读到的内容也会更新任务池。
func TestClaimVerifies(t *testing.T) {
	tests := []struct {
		name   string
		change func(is *fakegithub.Issue)
		reason string
	}{
		{"关闭", func(is *fakegithub.Issue) { is.State = "closed" }, "已关闭"},
		{"去掉 ready", func(is *fakegithub.Issue) { is.Labels = nil }, "不再有 " + LabelReady},
		{"加了 skip", func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelSkip) }, "有 " + LabelSkip},
		{"正文变了", func(is *fakegithub.Issue) { is.Body = "改过的需求" }, "变了"},
		{"标题变了", func(is *fakegithub.Issue) { is.Title = "新标题" }, "变了"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			h.issue(2, "二", "b")
			aid := h.acquire(h.taskOf(h.startSelect()).ID)
			h.offer(aid)
			h.editIssue(1, tt.change) // 没有投递 webhook
			res, err := h.claim(aid, []int{1}, 2)
			if err != nil {
				t.Fatal(err)
			}
			if res.Claimed || !strings.Contains(res.Reason, tt.reason) {
				t.Fatalf("结果 = %+v，想要原因包含 %q", res, tt.reason)
			}
			if n := h.queryInt(`SELECT count(*) FROM dev_work_items`); n != 0 {
				t.Fatal("冲突时建了工作项")
			}
			var got []int
			for _, c := range res.Candidates {
				got = append(got, c.Number)
			}
			want := []int{1, 2}
			if tt.name == "关闭" || tt.name == "去掉 ready" || tt.name == "加了 skip" {
				want = []int{2}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("新的候选 = %v，想要 %v", got, want)
			}
			// 按新的候选重新选题：内容变了的 Issue 用新的摘要可以领取
			sel := []int{2}
			skip := []int{}
			if slices.Contains(want, 1) {
				sel, skip = []int{1}, []int{2}
			}
			if res := h.mustClaim(aid, sel, skip...); res.WorkItemID == 0 {
				t.Fatal("重选后没有领取")
			}
		})
	}
}

// TestClaimInvalid：不是候选的 Issue、请求与报告不一致、报告不合法、不是选题 job 都是无效请求（重试没有意义）。
func TestClaimInvalid(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	if _, err := h.svc.ClaimIssues(h.ctx, h.taskOfAttempt(aid), &ClaimRequest{AttemptID: aid, Issues: []int{1}, Selection: selection([]int{1}, nil, nil)}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("还没有要过候选：err = %v", err)
	}
	h.offer(aid)
	cases := map[string]*ClaimRequest{
		"不是候选":    {AttemptID: aid, Issues: []int{9}, Selection: selection([]int{9}, []int{1}, nil)},
		"与报告不一致":  {AttemptID: aid, Issues: []int{}, Selection: selection([]int{1}, nil, nil)},
		"报告不合法":   {AttemptID: aid, Issues: []int{1}, Selection: []byte(`{"selected":[1],"extra":1}`)},
		"没有 plan": {AttemptID: aid, Issues: []int{1}, Selection: []byte(`{"version":1,"selected":[1],"plan":"","skipped":[]}`)},
	}
	for name, req := range cases {
		if _, err := h.svc.ClaimIssues(h.ctx, h.taskOfAttempt(aid), req); !errors.Is(err, ErrInvalidClaim) {
			t.Fatalf("%s：err = %v", name, err)
		}
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items`); n != 0 {
		t.Fatal("无效请求建了工作项")
	}
}

// TestClaimStaleEpoch：attempt 不再是当前的（租约过期后被重新领取、任务已结束）时领取被拒（I4），什么也不写。
func TestClaimStaleEpoch(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	job := h.taskOf(h.startSelect())
	aid := h.acquire(job.ID)
	h.offer(aid)
	h.exec(`UPDATE ai_tasks SET state='pending', current_attempt_id=NULL WHERE id=$1`, job.ID)
	if _, err := h.claim(aid, []int{1}); !isStale(err) {
		t.Fatalf("err = %v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items`) + h.queryInt(`SELECT count(*) FROM dev_reports`); n != 0 {
		t.Fatal("过期的 attempt 写入了数据")
	}
	if _, err := h.svc.OfferCandidates(h.ctx, job, aid); !isStale(err) {
		t.Fatalf("过期的 attempt 要候选：err = %v", err)
	}
}

// TestClaimExpiredLease：租约过期、sweeper 还没处理时，要候选和领取在自己的事务中持锁复查租约被拒（PLAN #33.6），
// 什么也不写。
func TestClaimExpiredLease(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	job := h.taskOf(h.startSelect())
	aid := h.acquire(job.ID)
	h.offer(aid)
	h.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 minute' WHERE id=$1`, job.ID)
	if _, err := h.claim(aid, []int{1}); !isStale(err) {
		t.Fatalf("租约过期的领取：err = %v，应为 STALE_EPOCH", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items`) + h.queryInt(`SELECT count(*) FROM dev_reports`); n != 0 {
		t.Fatal("租约过期的 attempt 写入了数据")
	}
	if _, err := h.svc.OfferCandidates(h.ctx, job, aid); !isStale(err) {
		t.Fatalf("租约过期的要候选：err = %v，应为 STALE_EPOCH", err)
	}
}

// TestClaimNothing：没有选中时只保存选题报告，写入"现在不适合做"的跳过记录，结束这一轮选题；"只是不放进这一批"（later）
// 的留在任务池中。任务池在这之后没有变化就不再选题（不会反复选同一批）。dry_run 只保存报告（不写跳过记录），同样不反复选题。
// 跳过记录让同样内容的 Issue 不再进入候选，直到人重新打上 kiln:ready（或者内容变了）。
func TestClaimNothing(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res, err := h.claimSel(aid, []int{}, selection(nil, []int{1}, []int{2}))
	if err != nil || res.Claimed || res.Reason == "" {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_reports WHERE kind='selection'`); n != 1 {
		t.Fatal("没有保存选题报告")
	}
	if n := h.queryInt(`SELECT dev_select_seq FROM ai_repo_state WHERE repo_id=$1`, testRepoID); n != 1 {
		t.Fatalf("没有领取后选题轮次 = %d，想要 1（这一轮结束）", n)
	}
	h.complete(aid, store.OutcomeSucceeded, nil)
	st, err := readPool(h.ctx, h.db, testRepoID)
	if err != nil || !slices.Equal(candidates(st.candidates), []int{2}) {
		t.Fatalf("候选 = %v（#1 不适合做，#2 留在任务池）, %v", candidates(st.candidates), err)
	}
	if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID != 0 || r.Reason != whyExamined {
		t.Fatalf("任务池没有变化又选题：%+v, %v", r, err)
	}
	// 内容变了（webhook）：#1 重新进入候选，重新选题
	h.editIssue(1, func(is *fakegithub.Issue) { is.Body = "补充了验收标准" })
	h.event(1, IssueEdited, "")
	if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
		t.Fatalf("内容变了没有重新选题：%+v, %v", r, err)
	}

	// 选题期间加入的 Issue（没有交给这次选题）：不算看过，接着选
	n := newHarness(t, devConfigYML)
	n.issue(1, "一", "a")
	aid = n.acquire(n.taskOf(n.startSelect()).ID)
	n.offer(aid)
	n.issue(2, "二", "b")
	if res, err := n.claim(aid, nil, 1); err != nil || res.Claimed {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	n.complete(aid, store.OutcomeSucceeded, nil)
	if r, err := n.svc.PlanSelect(n.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
		t.Fatalf("选题期间加入的 #2 没有交给 Agent，应接着选：%+v, %v", r, err)
	}

	d := newHarness(t, strings.Replace(devConfigYML, "max_issues: 2\n", "max_issues: 2\n  dry_run: true\n", 1))
	d.issue(1, "一", "a")
	job := d.taskOf(d.startSelect())
	if !d.specOf(job.ID).DryRun {
		t.Fatal("spec 没有 dry_run")
	}
	aid = d.acquire(job.ID)
	res, err = d.claim(aid, []int{1})
	if err != nil || res.Claimed || !strings.Contains(res.Reason, "dry_run") {
		t.Fatalf("dry_run 结果 = %+v, %v", res, err)
	}
	if n := d.queryInt(`SELECT count(*) FROM dev_work_items`) + d.queryInt(`SELECT count(*) FROM dev_issue_skips`); n != 0 {
		t.Fatal("dry_run 建了工作项或跳过记录")
	}
	d.complete(aid, store.OutcomeSucceeded, nil)
	if r, err := d.svc.PlanSelect(d.ctx, testRepoID, "bob"); err != nil || r.RequestID != 0 || r.Reason != whyExamined {
		t.Fatalf("dry_run 之后又选同一批：%+v, %v", r, err)
	}
	// 手动同步：重新判断（例如改了配置）
	if _, err := d.svc.SyncPool(d.ctx, testRepoID, "admin"); err != nil {
		t.Fatal(err)
	}
	if r, err := d.svc.PlanSelect(d.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
		t.Fatalf("同步后没有重新选题：%+v, %v", r, err)
	}
}

// TestClaimNothingUnoffered：没有领取时，写完跳过记录之后的候选里有没交给这次选题的（内容在交给它之后
// 变了、超过候选上限、选题期间新加入），不算看过，接着选。
func TestClaimNothingUnoffered(t *testing.T) {
	// 交给 Agent 之后内容变了的候选（留到之后的 #2 在选题期间被编辑）：Agent 看到的是旧内容，同样接着选
	c := newHarness(t, devConfigYML)
	c.issue(1, "一", "a")
	c.issue(2, "二", "b")
	aid := c.acquire(c.taskOf(c.startSelect()).ID)
	c.offer(aid)
	c.editIssue(2, func(is *fakegithub.Issue) { is.Body = "补充了需求" })
	c.event(2, IssueEdited, "")
	if res, err := c.claimSel(aid, []int{}, selection(nil, []int{1}, []int{2})); err != nil || res.Claimed {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	c.complete(aid, store.OutcomeSucceeded, nil)
	if r, err := c.svc.PlanSelect(c.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
		t.Fatalf("#2 交给 Agent 之后内容变了，应接着选：%+v, %v", r, err)
	}

	// 超过候选上限：交给 Agent 的前 candidateLimit 个都跳过后露出第 candidateLimit+1 个，同样接着选
	o := newHarness(t, devConfigYML)
	var all []int
	for i := 1; i <= candidateLimit+1; i++ {
		o.issue(i, fmt.Sprintf("第 %d 个", i), "x")
		all = append(all, i)
	}
	aid = o.acquire(o.taskOf(o.startSelect()).ID)
	if got := candidates(o.offer(aid).Candidates); len(got) != candidateLimit {
		t.Fatalf("候选 %d 个，应为 %d", len(got), candidateLimit)
	}
	if res, err := o.claim(aid, nil, all[:candidateLimit]...); err != nil || res.Claimed {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	o.complete(aid, store.OutcomeSucceeded, nil)
	if r, err := o.svc.PlanSelect(o.ctx, testRepoID, "bob"); err != nil || r.RequestID == 0 {
		t.Fatalf("第 %d 个没有交给过 Agent，应接着选：%+v, %v", candidateLimit+1, r, err)
	}
}

// TestConcurrentClaims：同一个仓库的两次选题（两台空闲的开发机）同时领取：同一个 Issue 只有一个成功（I1）；不同的 Issue
// 也只有一个成功（I14：仓库按工作项串行，领取锁下判断仓库是否被占用），另一个得到占用的原因、结束这一轮；同一个 attempt
// 并发重复领取只建一个工作项（I3）。用真实的 Postgres、advisory lock 和唯一索引，多轮重复。
func TestConcurrentClaims(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	const rounds = 5
	for round := range rounds {
		h.exec(`DELETE FROM dev_comments`)
		h.exec(`DELETE FROM dev_work_item_issues`)
		h.exec(`DELETE FROM dev_reports`)
		h.exec(`DELETE FROM dev_work_items`)
		// 两次选题：不同的去重键（模拟并发的触发绕过了去重）
		var attempts []int64
		for k := range 2 {
			_, task, err := h.svc.plan(h.ctx, &devRequest{
				platforms: []string{testDevPlatform}, repo: h.repoRow(), ref: "refs/heads/main", sha: h.branchHead("main"), dedupeKey: fmt.Sprintf("test:%d:%d", round, k),
				timeout: testTimeout, spec: h.specOf(h.taskOf(h.firstSelectRequest()).ID),
			})
			if err != nil {
				t.Fatal(err)
			}
			aid := h.acquire(task.ID)
			h.offer(aid)
			attempts = append(attempts, aid)
		}
		selections := [][]int{{1}, {1}}
		if round%2 == 1 {
			selections = [][]int{{1}, {2}}
		}
		var wg sync.WaitGroup
		results := make([]*ClaimResult, 2+3)
		errs := make([]error, len(results))
		for i := range results {
			wg.Go(func() {
				a, sel := attempts[min(i, 1)], selections[min(i, 1)]
				if i >= 2 {
					a, sel = attempts[0], selections[0] // 同一个 attempt 的重复请求
				}
				later := []int{1}
				if sel[0] == 1 {
					later = []int{2}
				}
				results[i], errs[i] = h.claimSel(a, sel, selection(sel, nil, later))
			})
		}
		wg.Wait()
		claimed := map[int64]bool{}
		for i, r := range results {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮请求 %d：%v", round, i, errs[i])
			}
			if r.Claimed {
				claimed[r.WorkItemID] = true
			}
		}
		// 同一个 attempt 的请求（0 和 2..4）结果相同（I3）：重复的请求不能被自己领到的工作项当成占用者挡住
		for i := 2; i < len(results); i++ {
			if results[i].Claimed != results[0].Claimed || results[i].WorkItemID != results[0].WorkItemID {
				t.Fatalf("第 %d 轮：同一个 attempt 的请求 %d 得到 %+v，与请求 0 的 %+v 不同", round, i, results[i], results[0])
			}
		}
		const want = 1 // 同一个 Issue（I1）、不同的 Issue（I14）都只有一个工作项
		if len(claimed) != want || h.queryInt(`SELECT count(*) FROM dev_work_items WHERE active`) != int64(want) {
			t.Fatalf("第 %d 轮：领取到 %d 个工作项，想要 %d 个", round, len(claimed), want)
		}
		if round%2 == 1 {
			// 选了不同 Issue 的另一次领取：没有领取，原因是仓库被占用（不是 Issue 冲突，不让 Agent 重新选）
			for i, r := range results[:2] {
				if !r.Claimed && (r.Conflict || !strings.Contains(r.Reason, "按工作项串行")) {
					t.Fatalf("第 %d 轮请求 %d：%+v，想要因为仓库被占用而不领取", round, i, r)
				}
			}
		}
		if h.queryInt(`SELECT count(*) FROM dev_work_item_issues WHERE active`) != int64(want) {
			t.Fatalf("第 %d 轮：被占用的 Issue 数不对", round)
		}
		for _, aid := range attempts {
			h.exec(`UPDATE ai_tasks SET state='cancelled', finished_at=now() WHERE current_attempt_id=$1`, aid)
		}
	}
}

// TestDuplicateClaimAfterPrecheck：同一个 attempt 的两个领取请求都过了事务外的预查（都没看到工作项）后才先后取得 attempt 锁：
// 后一个返回前一个领取的同一个工作项（I3），不被它当成仓库的占用者（I14）挡住。用屏障确定地复现这个交错。
func TestDuplicateClaimAfterPrecheck(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	h.offer(aid)
	task := h.taskOfAttempt(aid)
	a, err := ai.GetAttempt(h.ctx, h.db, aid)
	if err != nil {
		t.Fatal(err)
	}
	const n = 2
	var barrier sync.WaitGroup
	barrier.Add(n)
	inner := attemptLock(task, aid)
	lock := func(ctx context.Context, tx pgx.Tx) error {
		barrier.Done()
		barrier.Wait() // 两个请求都过了预查才取锁
		return inner(ctx, tx)
	}
	results := make([]*ClaimResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			results[i], errs[i] = h.svc.claimIssues(h.ctx, task, &ClaimRequest{
				AttemptID: aid, Issues: []int{1}, Selection: selection([]int{1}, nil, nil), Agent: task.Agent, RunnerVersion: a.RunnerVersion,
			}, lock)
		})
	}
	wg.Wait()
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("请求 %d：%v", i, errs[i])
		}
		if !r.Claimed || r.WorkItemID != results[0].WorkItemID || r.Branch != results[0].Branch || r.Phase != results[0].Phase {
			t.Fatalf("请求 %d 得到 %+v，想要与请求 0 相同的已领取结果 %+v", i, r, results[0])
		}
	}
	if got := h.queryInt(`SELECT count(*) FROM dev_work_items WHERE claim_attempt_id=$1`, aid); got != 1 {
		t.Fatalf("attempt 建了 %d 个工作项，想要 1 个", got)
	}
}

func (h *harness) repoRow() *store.Repo {
	h.t.Helper()
	r, err := store.GetRepo(h.ctx, h.db, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// testTimeout 是测试中直接规划的开发请求的超时（分钟）。
const testTimeout = 60

// firstSelectRequest 返回第一个选题请求（没有时规划一个）。
func (h *harness) firstSelectRequest() int64 {
	h.t.Helper()
	ids, err := store.QueryIDs(h.ctx, h.db, `SELECT id FROM ai_requests WHERE type=$1 ORDER BY id LIMIT 1`, agentcfg.TaskDevelop)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(ids) == 0 {
		return h.startSelect()
	}
	return ids[0]
}

// TestClaimPaused：仓库暂停后不再领取（已经在跑的选题也不领取）。
func TestClaimPaused(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	if err := h.svc.SetPaused(h.ctx, testRepoID, true, "admin"); err != nil {
		t.Fatal(err)
	}
	res, err := h.claim(aid, []int{1})
	if err != nil || res.Claimed || res.Reason != whyPaused {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	if r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob"); err != nil || r.Reason != whyPaused {
		t.Fatalf("暂停后选题：%+v, %v", r, err)
	}
	if !strings.HasPrefix(devreport.WorkItemBranch(1, devreport.PhaseImplement, 1), devreport.WorkItemBranchPrefix) {
		t.Fatal("分支前缀")
	}
}

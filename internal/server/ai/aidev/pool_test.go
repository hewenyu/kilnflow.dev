package aidev

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// poolNumbers 返回在任务池中的 Issue（按编号）。
func (h *harness) poolNumbers() []int {
	h.t.Helper()
	ids, err := store.QueryIDs(h.ctx, h.db, `SELECT issue_number FROM dev_pool WHERE repo_id=$1 AND in_pool ORDER BY issue_number`, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		out = append(out, int(id))
	}
	return out
}

// TestPoolMirror：任务池按 issues 事件中 Issue 的当前内容更新：打开且有 kiln:ready 的在池中；关闭、去掉 ready 的离开，
// 重新打开、重新打上的回来；乱序到达的旧事件不会把已经离开的 Issue 加回来；本 App 的事件也更新 label（不清除跳过记录）。
func TestPoolMirror(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b", "bug") // 没有 ready
	if got := h.poolNumbers(); !slices.Equal(got, []int{1}) {
		t.Fatalf("任务池 = %v", got)
	}
	old, _ := h.fake.GetIssue(repoName, 1)
	h.editIssue(1, func(is *fakegithub.Issue) { is.State = "closed" })
	h.event(1, IssueClosed, "")
	if got := h.poolNumbers(); len(got) != 0 {
		t.Fatalf("关闭后任务池 = %v", got)
	}
	// 晚到的旧事件（关闭之前的 labeled）：丢弃
	if _, err := h.svc.HandleIssueEvent(h.ctx, IssueEvent{
		RepoID: testRepoID, Action: IssueLabeled, Number: 1, Title: old.Title, Body: old.Body,
		Open: true, Labels: old.Labels, UpdatedAt: old.UpdatedAt, Label: LabelReady, Sender: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.poolNumbers(); len(got) != 0 {
		t.Fatalf("旧事件把关闭的 Issue 加回来了：%v", got)
	}
	h.editIssue(1, func(is *fakegithub.Issue) { is.State = "open" })
	h.event(1, IssueReopened, "")
	h.editIssue(2, func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelReady) })
	h.event(2, IssueLabeled, LabelReady)
	if got := h.poolNumbers(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("重新打开、打上 ready 后任务池 = %v", got)
	}
	// 内容变了：快照更新
	h.editIssue(2, func(is *fakegithub.Issue) { is.Body = "补充" })
	h.event(2, IssueEdited, "")
	st, err := readPool(h.ctx, h.db, testRepoID)
	if err != nil || len(st.candidates) != 2 || st.candidates[1].Digest != IssueDigest("二", "补充") {
		t.Fatalf("候选 = %+v, %v", st, err)
	}
	// kiln:skip：还在任务池中，但不是候选
	h.editIssue(2, func(is *fakegithub.Issue) { is.Labels = append(is.Labels, LabelSkip) })
	h.event(2, IssueLabeled, LabelSkip)
	if st, _ := readPool(h.ctx, h.db, testRepoID); !slices.Equal(candidates(st.candidates), []int{1}) {
		t.Fatalf("有 skip 的还是候选：%v", candidates(st.candidates))
	}
	// 去掉 ready：离开
	h.editIssue(1, func(is *fakegithub.Issue) { is.Labels = nil })
	h.event(1, IssueUnlabeled, LabelReady)
	if got := h.poolNumbers(); !slices.Equal(got, []int{2}) {
		t.Fatalf("去掉 ready 后任务池 = %v", got)
	}
}

// TestSyncPool：手动同步从 GitHub 全量读取（逐页）：webhook 丢了的 Issue 加入，GitHub 上已经没有 ready 的移出；同步期间
// webhook 带来的更新的内容不被移出。同步记下时间，并让任务池重新判断。
func TestSyncPool(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	// webhook 丢了：#1 去掉了 ready、#2..#151 加了 ready（超过一页）
	h.editIssue(1, func(is *fakegithub.Issue) { is.Labels = nil })
	for n := 2; n <= 151; n++ {
		h.fake.SetIssue(repoName, fakegithub.Issue{Number: n, Title: "批量", Body: "x", Labels: []string{LabelReady}, UpdatedAt: h.tick()})
	}
	// 同步开始之后才到的 webhook 内容（GitHub 的列表里还没有）：保留
	h.exec(`INSERT INTO dev_pool (repo_id, issue_number, in_pool, title, digest, issue_updated_at) VALUES ($1, 500, true, '新', 'd', now() + interval '1 hour')`, testRepoID)
	res, err := h.svc.SyncPool(h.ctx, testRepoID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Listed != 150 || !res.Complete || res.Changed != 151 || res.Candidates != candidateLimit {
		t.Fatalf("同步结果 = %+v", res)
	}
	got := h.poolNumbers()
	if len(got) != 151 || got[0] != 2 || got[len(got)-1] != 500 {
		t.Fatalf("任务池 = %d 个（%v…）", len(got), got[:3])
	}
	var synced *time.Time
	if err := h.db.QueryRow(h.ctx, `SELECT dev_synced_at FROM ai_repo_state WHERE repo_id=$1`, testRepoID).Scan(&synced); err != nil || synced == nil {
		t.Fatalf("没有记下同步时间：%v", err)
	}
	view, err := GetPool(h.ctx, h.db, testRepoID)
	if err != nil || len(view.Issues) != 151 || view.Issues[0].Status != PoolCandidate || view.SyncedAt == nil {
		t.Fatalf("任务池视图 = %d 个, %v", len(view.Issues), err)
	}
	// 再同步一次：没有变化
	if res, err := h.svc.SyncPool(h.ctx, testRepoID, "admin"); err != nil || res.Changed != 0 {
		t.Fatalf("第二次同步：%+v, %v", res, err)
	}
}

// TestCandidatesAfterExcluded：先排除不可选的（有 kiln:skip、跳过记录、在进行中的工作项里），再取前 candidateLimit 个：
// 排除的 Issue 再多也不会挡住后面的。
func TestCandidatesAfterExcluded(t *testing.T) {
	h := newHarness(t, devConfigYML)
	for n := 1; n <= candidateLimit+5; n++ {
		h.issue(n, "排除", "x", LabelReady, LabelSkip)
	}
	h.issue(100, "可以做", "y")
	h.issue(101, "跳过记录", "z")
	h.exec(`INSERT INTO dev_issue_skips (repo_id, issue_number, digest, reason) VALUES ($1, 101, $2, 'x')`, testRepoID, IssueDigest("跳过记录", "z"))
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	if got := candidates(h.offer(aid).Candidates); !slices.Equal(got, []int{100}) {
		t.Fatalf("候选 = %v，想要 [100]", got)
	}
	view, err := GetPool(h.ctx, h.db, testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	status := map[int]string{}
	for _, is := range view.Issues {
		status[is.Number] = is.Status
	}
	if status[1] != PoolSkipLabel || status[100] != PoolCandidate || status[101] != PoolSkipped {
		t.Fatalf("任务池的情况 = %v", status)
	}
}

// TestOfferLateBinding：候选在 Runner 准备好之后才确定。在 GitHub 上批量打 label 时第一个事件就规划了选题，之后到达的
// 也在候选中；任务池刚变化时 Runner 要等它静默（从 attempt 开始最多等 maxSettleWait）；同一个 attempt 重复要得到同一批。
func TestOfferLateBinding(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	run := h.startSelect()
	if spec := h.specOf(h.taskOf(run).ID); spec.SelectSeq != 0 {
		t.Fatalf("选题轮次 = %d", spec.SelectSeq)
	}
	h.issue(2, "二", "b")
	h.issue(3, "三", "c")
	aid := h.acquire(h.taskOf(run).ID)
	// 刚刚还有 webhook 到达：等
	res, err := h.svc.OfferCandidates(h.ctx, h.taskOfAttempt(aid), aid)
	if err != nil || res.Wait <= 0 || res.Wait > poolSettle || len(res.Candidates) != 0 {
		t.Fatalf("任务池刚变化：%+v, %v", res, err)
	}
	// 一直在变，但已经等了 maxSettleWait：不再等
	h.exec(`UPDATE ai_attempts SET started_at = now() - make_interval(secs => $2) WHERE id=$1`, aid, maxSettleWait.Seconds())
	res, err = h.svc.OfferCandidates(h.ctx, h.taskOfAttempt(aid), aid)
	if err != nil || res.Wait != 0 || !slices.Equal(candidates(res.Candidates), []int{1, 2, 3}) {
		t.Fatalf("等够了：%+v, %v", res, err)
	}
	// 之后到达的不改变这一次的候选（领取按它核对）
	h.issue(4, "四", "d")
	again := h.offer(aid)
	if !slices.Equal(candidates(again.Candidates), []int{1, 2, 3}) {
		t.Fatalf("重复要候选 = %v", candidates(again.Candidates))
	}
	// 选完 #1、#2：剩下的 #3（留给之后）和新来的 #4 在下一轮
	res2, err := h.claimSel(aid, []int{1, 2}, selection([]int{1, 2}, nil, []int{3}))
	if err != nil || !res2.Claimed {
		t.Fatalf("领取：%+v, %v", res2, err)
	}
	st, _ := readPool(h.ctx, h.db, testRepoID)
	if !slices.Equal(candidates(st.candidates), []int{3, 4}) {
		t.Fatalf("领取后的候选 = %v", candidates(st.candidates))
	}
}

// TestRunnerIdle：空闲的开发机来领取时，只通知能开始选题的仓库的执行者，不请求 GitHub；正在选题、任务池没有变化、暂停的
// 仓库不通知。
func TestRunnerIdle(t *testing.T) {
	h := newHarness(t, devConfigYML)
	outbox := func() int64 {
		return h.queryInt(`SELECT count(*) FROM outbox WHERE kind='aidev.repo' AND state='pending'`)
	}
	clearOutbox := func() { h.exec(`DELETE FROM outbox`) }
	h.issue(1, "一", "a")
	clearOutbox()
	before := len(h.fake.Requests())
	if err := h.svc.RunnerIdle(h.ctx, store.RunnerRoleDevelopment); err != nil {
		t.Fatal(err)
	}
	if outbox() != 1 || len(h.fake.Requests()) != before {
		t.Fatalf("可以选题时：outbox %d，GitHub 请求 %v", outbox(), h.fake.Requests()[before:])
	}
	clearOutbox()
	h.startSelect()
	if err := h.svc.RunnerIdle(h.ctx, store.RunnerRoleDevelopment); err != nil || outbox() != 0 {
		t.Fatalf("正在选题时通知了：%d, %v", outbox(), err)
	}
	if err := h.svc.SetPaused(h.ctx, testRepoID, true, "admin"); err != nil {
		t.Fatal(err)
	}
	clearOutbox()
	h.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now()`)
	h.exec(`UPDATE ai_tasks SET state='succeeded', finished_at=now()`)
	h.exec(`UPDATE ai_repo_state SET dev_examined='' WHERE repo_id=$1`, testRepoID)
	if err := h.svc.RunnerIdle(h.ctx, store.RunnerRoleDevelopment); err != nil || outbox() != 0 {
		t.Fatalf("暂停时通知了：%d, %v", outbox(), err)
	}
}

// TestStopDuringPromotion：执行者正在推进交付（创建正式分支的请求很慢）时终止工作项：终止等推进做完（与执行者串行），
// 推进建的 PR 记在工作项上之后才终止；终止成功之后不会再建 PR。
func TestStopDuringPromotion(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, aid := h.claimed(1)
	h.mustProcess()
	sha := h.push(devreport.AttemptBranch(aid), w.BaseSHA, map[string]string{"a.go": "package a\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	reached, release := make(chan struct{}), make(chan struct{})
	// 断言失败时也放行暂停的请求：否则 fakegithub 关闭时一直等它
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	h.fake.SetFault("POST /repos/"+repoName+"/git/refs", fakegithub.Fault{Pause: func() {
		close(reached)
		<-release
	}})
	processed := make(chan error, 1)
	go func() { processed <- h.process() }()
	<-reached
	stopped := make(chan error, 1)
	go func() { stopped <- h.svc.Stop(h.ctx, w.ID, "不做了", "admin") }()
	select {
	case err := <-stopped:
		t.Fatalf("推进还没做完终止就返回了：%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	releaseOnce()
	if err := <-processed; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	got := h.wantState(w.ID, StateStopped)
	if got.PRNumber == nil {
		t.Fatal("推进建的 PR 没有记在工作项上")
	}
	prs := h.countRequests("POST /repos/" + repoName + "/pulls")
	h.mustProcess()
	if n := h.countRequests("POST /repos/" + repoName + "/pulls"); n != prs || prs != 1 {
		t.Fatalf("建 PR 的请求 %d → %d", prs, n)
	}
	if err := h.svc.Stop(h.ctx, w.ID, "again", "admin"); !errors.Is(err, ErrFinished) {
		t.Fatalf("err = %v", err)
	}
}

// TestIdleRunnerAgent：空闲的开发机要能用仓库的开发 Agent；读到 Agent 之后，空闲的开发机来领取时按它判断，不再读 GitHub。
// 排队中的别的 Agent 的任务不占用这个 Agent 的开发机（一个 Agent 不可用的仓库不能挡住别的仓库）。
func TestIdleRunnerAgent(t *testing.T) {
	h := newHarness(t, devConfigYML) // claude-code
	h.exec(`DELETE FROM runners`)
	codex := h.addRunnerWith("codex-only", "host-c", agentcfg.AgentCodex)
	h.issue(1, "一", "a")
	plan := func() *SelectResult {
		t.Helper()
		res, err := h.svc.PlanSelect(h.ctx, testRepoID, "test")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := plan(); res.RequestID != 0 || res.Reason != whyNoFreeSlot {
		t.Fatalf("只有 Codex 的开发机：%+v", res)
	}
	if got := h.queryString(`SELECT dev_agent FROM ai_repo_state WHERE repo_id=$1`, testRepoID); got != agentcfg.AgentClaudeCode {
		t.Fatalf("记下的 Agent = %q", got)
	}
	before := len(h.fake.Requests())
	if res := plan(); res.RequestID != 0 || len(h.fake.Requests()) != before {
		t.Fatalf("Agent 已知、没有能用它的开发机时又读了 GitHub：%+v，%v", res, h.fake.Requests()[before:])
	}

	// 另一个仓库排队中的 Codex 任务（Codex 的开发机已离线，领不走）不占用 Claude Code 的开发机
	h.queueElsewhere(agentcfg.AgentCodex, testDevPlatform)
	h.exec(`UPDATE runners SET last_seen_at = now() - interval '1 hour' WHERE id=$1`, codex)
	h.addRunnerWith("claude", "host-k", agentcfg.AgentClaudeCode)
	if res := plan(); res.RequestID == 0 {
		t.Fatalf("Claude Code 的开发机空闲，却没有规划：%s", res.Reason)
	}
}

// TestFreeDevSlots：空闲槽位是各个开发机的并发上限减去执行中的任务（Server 的账本与节点上报的取较大的，与领取的规则相同），
// 求和后减去排队中的同一个 Agent 的开发任务；别的 Agent 排队中的任务不扣减。
func TestFreeDevSlots(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.exec(`UPDATE ai_runners SET capacity=2`)
	queue := func(agent string) int64 { return h.queueElsewhere(agent, testDevPlatform) }
	busy := func(runner int64) { h.acquireBy(queue(devConfigAgent), runner) }
	want := func(what string, n int) {
		t.Helper()
		got, err := freeDevSlots(h.ctx, h.db, devConfigAgent, []string{testDevPlatform})
		if err != nil || got != n {
			t.Fatalf("%s：空闲槽位 = %d（%v），想要 %d", what, got, err, n)
		}
	}
	want("capacity=2、没有执行", 2)
	busy(h.runnerID)
	want("capacity=2、执行中 1 个", 1)
	busy(h.runnerID)
	want("capacity=2、执行中 2 个", 0)
	second := h.addRunner("dev2", "host-2")
	h.exec(`UPDATE ai_runners SET capacity=2 WHERE runner_id=$1`, second)
	busy(second)
	want("两台各 capacity=2、共执行 3 个", 1)
	h.exec(`UPDATE ai_runners SET running=2 WHERE runner_id=$1`, second)
	want("节点上报执行中 2 个（账本 1 个）", 0)
	h.exec(`UPDATE ai_runners SET running=0 WHERE runner_id=$1`, second)
	queue(agentcfg.AgentCodex)
	want("排队中的别的 Agent 的任务", 1)
	queue(devConfigAgent)
	want("排队中的同一个 Agent 的任务", 0)
}

// TestStaleIssueEvent：比任务池中已记录的内容旧的事件（webhook 乱序）整个忽略：旧的"去掉 kiln:ready"不终止按较新的授权
// 领取的工作项，也不改任务池；较新的照常终止。
func TestStaleIssueEvent(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _ := h.claimed(1)
	var recorded time.Time
	if err := h.db.QueryRow(h.ctx, `SELECT issue_updated_at FROM dev_pool WHERE repo_id=$1 AND issue_number=1`, testRepoID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	unready := func(at time.Time) string {
		t.Helper()
		res, err := h.svc.HandleIssueEvent(h.ctx, IssueEvent{
			RepoID: testRepoID, Action: IssueUnlabeled, Number: 1, Title: "一", Body: "a", Open: true, UpdatedAt: at, Label: LabelReady, Sender: "bob",
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := unready(recorded.Add(-time.Second)); !strings.Contains(res, "乱序") {
		t.Errorf("旧事件的结果 = %q", res)
	}
	h.wantState(w.ID, StateDeveloping)
	if n := h.queryInt(`SELECT count(*) FROM dev_pool WHERE repo_id=$1 AND issue_number=1 AND in_pool`, testRepoID); n != 1 {
		t.Fatal("旧事件把 Issue 移出了任务池")
	}
	unready(recorded.Add(time.Second))
	h.wantState(w.ID, StateStopped)
}

// TestClaimAfterWithdrawal：核对过 GitHub 之后、领取的事务提交之前，人去掉了 kiln:ready（事件已经处理完，那时还没有
// 工作项可以终止）：领取的事务按任务池的当前状态核对，不领取，给出剩下的候选。
func TestClaimAfterWithdrawal(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	h.offer(aid)
	task := h.taskOfAttempt(aid)
	inner, withdrawn := attemptLock(task, aid), false
	lock := func(ctx context.Context, tx pgx.Tx) error {
		if !withdrawn {
			withdrawn = true
			h.editIssue(1, func(is *fakegithub.Issue) {
				is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return l == LabelReady })
			})
			h.event(1, IssueUnlabeled, LabelReady)
		}
		return inner(ctx, tx)
	}
	res, err := h.svc.claimIssues(h.ctx, task, &ClaimRequest{
		AttemptID: aid, Issues: []int{1}, Selection: selection([]int{1}, nil, []int{2}), Agent: devConfigAgent, RunnerVersion: testRunnerVersion,
	}, lock)
	if err != nil || !withdrawn {
		t.Fatalf("领取：%v（撤回 %v）", err, withdrawn)
	}
	if res.Claimed || !strings.Contains(res.Reason, "不在任务池") || !slices.Equal(candidates(res.Candidates), []int{2}) {
		t.Fatalf("撤回授权后的领取 = %+v", res)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items`); n != 0 {
		t.Fatalf("撤回授权后建了 %d 个工作项", n)
	}
}

// TestPoolMirrorConcurrentKeepsNewest 是并发回归测试：同一个 Issue 的多个事件（时间戳不同）并发到达时，任务池最终必须是
// 时间戳最大的那个内容，旧快照不会覆盖新版本（mirror 的 ON CONFLICT / UPDATE 带版本单调条件）。
func TestPoolMirrorConcurrentKeepsNewest(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	base := time.Now().Truncate(time.Second)
	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, _ = h.svc.HandleIssueEvent(h.ctx, IssueEvent{
				RepoID: testRepoID, Action: IssueEdited, Number: 1, Title: "一", Body: "v" + string(rune('a'+i)),
				Open: true, Labels: []string{LabelReady}, UpdatedAt: base.Add(time.Duration(i) * time.Second), Sender: "bob",
			})
		})
	}
	wg.Wait()
	st, err := readPool(h.ctx, h.db, testRepoID)
	if err != nil || len(st.candidates) != 1 {
		t.Fatalf("任务池 = %+v, %v", st, err)
	}
	want := IssueDigest("一", "v"+string(rune('a'+n-1)))
	if st.candidates[0].Digest != want {
		t.Fatalf("并发事件后任务池的内容摘要 = %s，应为时间戳最大者的 %s", st.candidates[0].Digest, want)
	}
}

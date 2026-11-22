package aidev

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 仓库级串行（PLAN #26.9 I14、C23）：同一个仓库按完整的工作项串行，不同仓库并行；等待期间不占执行槽位。

// otherRepo 是测试中另一个启用了开发的仓库（与 h.repo 在同一个 fakegithub 中）。
type otherRepo struct {
	h    *harness
	id   int64
	name string // owner/name
}

// addDevRepo 登记另一个启用了开发的仓库（自己的 git 目录、默认分支上有 devConfigYML）。
func (h *harness) addDevRepo(id int64, name string) *otherRepo {
	h.t.Helper()
	dir := h.t.TempDir()
	run := func(args ...string) {
		h.t.Helper()
		cmd := exec.CommandContext(h.ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("git %v：%v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for f, c := range map[string]string{".kiln/agent/config.yml": devConfigYML, ".kiln/agent/development.md": "开发说明\n", "README.md": name + "\n"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	h.fake.AddRepo(&fakegithub.Repo{ID: id, Owner: testAccount, Name: name, Dir: dir, Private: true})
	if err := store.UpsertRepo(h.ctx, h.db, store.RepoMeta{
		ID: id, InstallationID: h.fake.InstallationID, OwnerID: 1, Owner: testAccount, Name: name, Private: true,
		DefaultBranch: "main", CloneURL: "https://example.invalid/" + testAccount + "/" + name + ".git",
	}); err != nil {
		h.t.Fatal(err)
	}
	return &otherRepo{h: h, id: id, name: testAccount + "/" + name}
}

// issue 在仓库上登记一个有 kiln:ready 的 Issue 并投递 issues 事件。
func (o *otherRepo) issue(n int, title string) {
	h := o.h
	h.t.Helper()
	h.fake.SetIssue(o.name, fakegithub.Issue{Number: n, Title: title, Body: title, Labels: []string{LabelReady}, UpdatedAt: h.tick()})
	if _, err := h.svc.HandleIssueEvent(h.ctx, IssueEvent{
		RepoID: o.id, Action: IssueLabeled, Number: n, Title: title, Body: title, Open: true, Labels: []string{LabelReady},
		UpdatedAt: h.clock, Label: LabelReady, Sender: "bob",
	}); err != nil {
		h.t.Fatal(err)
	}
}

// wantOccupied 断言仓库 repoID 被工作项 id 占用：不规划选题，原因写明占用者和它的状态。
func (h *harness) wantOccupied(repoID, id int64, state string) {
	h.t.Helper()
	r, err := h.svc.PlanSelect(h.ctx, repoID, "bob")
	want := "工作项 #" + strconv.FormatInt(id, 10) + " 进行中"
	if err != nil || r.RequestID != 0 || !strings.Contains(r.Reason, want) || !strings.Contains(r.Reason, state) {
		h.t.Fatalf("PlanSelect = %+v, %v，想要因为 %s（%s）被占用而不选题", r, err, want, state)
	}
}

// selectRequests 返回仓库 repoID 的选题请求（按 id）。
func (h *harness) selectRequests(repoID int64) []int64 {
	h.t.Helper()
	ids, err := store.QueryIDs(h.ctx, h.db, `SELECT r.id FROM ai_requests r JOIN ai_dev_requests d ON d.request_id=r.id
		WHERE r.repo_id=$1 AND d.mode='select' ORDER BY r.id`, repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

// TestSerialSelection：同一个仓库有多个 ready Issue、两台空闲的开发机时只形成一个工作项：领取后不开始第二轮选题；PR 等待
// CI / 审计 / 合并（不占执行槽位）、返修、受阻时也不开始，选题门槛和空闲开发机的通知都写明占用者；工作项合并后释放，
// 下一轮选题从当时默认分支的 head 开始。
func TestSerialSelection(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.addRunner("dev2", "host-b")
	for n := 1; n <= 3; n++ {
		h.issue(n, "需求", strings.Repeat("x", n))
	}
	w, aid := h.claimed(1)
	h.mustProcess()
	if n := len(h.selectRequests(testRepoID)); n != 1 {
		t.Fatalf("领取后又开始了选题：%d 个选题请求", n)
	}
	h.wantOccupied(testRepoID, w.ID, StateDeveloping)
	idle := func(what string) {
		t.Helper()
		h.exec(`DELETE FROM outbox`)
		if err := h.svc.RunnerIdle(h.ctx, store.RunnerRoleDevelopment); err != nil {
			t.Fatal(err)
		}
		if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1`, KindRepo); n != 0 {
			t.Fatalf("%s：空闲的开发机通知了被占用的仓库", what)
		}
	}
	idle("开发中")

	// 交付、开出 PR：工作项没有任务、不占执行槽位（两台开发机都空闲），仓库仍被占用
	sha := h.push(devreport.AttemptBranch(aid), w.BaseSHA, map[string]string{"a.go": "package a\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if free, err := freeDevSlots(h.ctx, h.db, devConfigAgent, []string{testDevPlatform}); err != nil || free != 2 {
		t.Fatalf("PR 等待验证时的空闲槽位 = %d（%v），想要 2", free, err)
	}
	h.wantOccupied(testRepoID, w.ID, StatePROpen)
	idle("等待验证")
	if n := len(h.selectRequests(testRepoID)); n != 1 {
		t.Fatalf("等待验证时开始了选题：%d 个选题请求", n)
	}
	for _, state := range []string{StateFixing, StateBlocked} {
		h.exec(`UPDATE dev_work_items SET state=$2 WHERE id=$1`, w.ID, state)
		h.wantOccupied(testRepoID, w.ID, state)
	}
	h.exec(`UPDATE dev_work_items SET state=$2 WHERE id=$1`, w.ID, StatePROpen)

	// 合并（关闭 #1）：释放仓库；默认分支在这期间前进了，下一轮选题从合并后的 head 开始
	h.commitOn("main", map[string]string{"other.txt": "人合并的改动\n"}, "别的 PR")
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID, ClosedPull{
		Number: *w.PRNumber, HeadRef: w.Branch(), HeadSHA: w.HeadSHA, Merged: true,
		MergeSHA: h.branchHead("main"),
	}, "bob"); err != nil {
		t.Fatal(err)
	}
	h.wantState(w.ID, StateMerged)
	h.editIssue(1, func(is *fakegithub.Issue) { is.State = "closed" })
	h.event(1, IssueClosed, "")
	h.mustProcess()
	reqs := h.selectRequests(testRepoID)
	if len(reqs) != 2 {
		r, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob")
		t.Fatalf("合并后没有开始下一轮选题：%v（%+v, %v）", reqs, r, err)
	}
	if got := h.request(reqs[1]).SHA; got != h.branchHead("main") {
		t.Fatalf("下一轮选题的起点 %s，想要最新的默认分支 %s", got, h.branchHead("main"))
	}
}

// TestCrossRepoParallel：仓库 A 有工作项时，仓库 B 照常开始选题（容量允许时不同仓库同时开发）；A 的工作项等待验证时
// 不占执行槽位，B 用满两个槽位时才没有空闲；A 的 PR 审计发现问题时照常返修（已有 PR 的返修不受占用限制），A 不开始
// 第二个工作项。
func TestCrossRepoParallel(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.exec(`UPDATE ai_runners SET capacity=2`)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	w, aid := h.claimed(1)
	b := h.addDevRepo(testRepoID+2, "second")
	b.issue(1, "B 的需求")
	// A 开发中（一个槽位），B 用另一个槽位开始选题
	rb, err := h.svc.PlanSelect(h.ctx, b.id, "bob")
	if err != nil || rb.RequestID == 0 {
		t.Fatalf("A 开发时 B 没有开始选题：%+v, %v", rb, err)
	}
	h.wantOccupied(testRepoID, w.ID, StateDeveloping)
	h.acquire(h.taskOf(rb.RequestID).ID)
	if free, err := freeDevSlots(h.ctx, h.db, devConfigAgent, []string{testDevPlatform}); err != nil || free != 0 {
		t.Fatalf("A、B 各执行一个任务时空闲槽位 = %d（%v），想要 0", free, err)
	}
	// A 开出 PR：释放 A 的槽位
	sha := h.push(devreport.AttemptBranch(aid), w.BaseSHA, map[string]string{"a.go": "package a\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if free, err := freeDevSlots(h.ctx, h.db, devConfigAgent, []string{testDevPlatform}); err != nil || free != 1 {
		t.Fatalf("A 等待验证时空闲槽位 = %d（%v），想要 1", free, err)
	}
	h.wantOccupied(testRepoID, w.ID, StatePROpen)
	// A 的审计照常：发现问题时返修原 PR，不开始第二个工作项
	h.failingRun(*w.PRNumber, w.HeadSHA)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	h.fixTask(w)
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items WHERE repo_id=$1`, testRepoID); n != 1 {
		t.Fatalf("A 有 %d 个工作项，想要 1 个", n)
	}
}

// TestOccupancyLifecycle：设计 PR 合并后开始实现、部分完成的实现合并后受阻、hold 都仍占用仓库；明确终止后释放；重复的
// 合并事件只释放一次，不开始两轮选题。
func TestOccupancyLifecycle(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	// 先设计：设计 PR 合并，进入实现，仍占用
	res, aid := h.claimDesign(1)
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"docs/design.md": "# 设计\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: designReport([]int{1}, "docs/design.md"), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.mustProcess()
	w := h.wantState(res.WorkItemID, StatePROpen)
	h.merge(*w.PRNumber, w.Branch(), map[string]string{"docs/design.md": "# 设计\n"})
	_ = sha
	h.mustProcess()
	w = h.wantState(w.ID, StateDeveloping)
	if w.Phase != PhaseImplement || w.TaskID == nil {
		t.Fatalf("设计合并后：%s / %s，任务 %v", w.Phase, w.State, w.TaskID)
	}
	h.issue(2, "二", "b")
	h.wantOccupied(testRepoID, w.ID, PhaseImplement)
	if n := len(h.selectRequests(testRepoID)); n != 1 {
		t.Fatalf("设计合并后开始了选题：%d", n)
	}
	// 人终止：释放；重复的终止不再改变
	if err := h.svc.Stop(h.ctx, w.ID, "人工终止", "bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	h.mustProcess()
	if reqs := h.selectRequests(testRepoID); len(reqs) != 2 {
		t.Fatalf("终止后的选题请求 = %v，想要恰好一轮新的", reqs)
	}
	if err := h.svc.Stop(h.ctx, w.ID, "再终止", "bob"); err == nil {
		t.Fatal("已经结束的工作项又被终止")
	}
}

// TestUpgradeConvergence：升级前同一个仓库留下三个未结束的工作项（并行开发的）时：不选题；编号最小的是占用者，只有它
// 开始被推迟的实现，其余保持 developing、没有任务、原因写明在等谁；其余的已有 PR 照常返修；占用者结束后下一个开始实现；
// 全部结束后才选题。已有的分支、PR 不受影响。
func TestUpgradeConvergence(t *testing.T) {
	h := newHarness(t, devConfigYML)
	for n := 1; n <= 4; n++ {
		h.issue(n, "需求", strings.Repeat("y", n))
	}
	// 模拟升级前并行领取的三个工作项（升级前没有仓库占用的判断）：#1 等待实现（设计已合并），#2 有 PR、等待验证，
	// #3 等待实现（设计已合并）
	ids := make([]int64, 3)
	task := h.taskOf(h.startSelect())
	for i := range ids {
		attempt := h.queryInt(`INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id) VALUES ($1,$2,$3,'req') RETURNING id`,
			task.ID, i+1, h.runnerID)
		state, phase, pr := StateDeveloping, PhaseImplement, any(nil)
		if i == 1 {
			state, pr = StatePROpen, 100+i
		}
		// 等待实现的是新的轮次（round_ran=false，设计合并后的状态）
		ids[i] = h.queryInt(`INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, phase,
			pr_number, head_sha, reason, round_ran) VALUES ($1,$2,'main',$3,2,'claude-code',$4,$5,$6,$7,'升级前',$8)
			RETURNING id`, testRepoID, state, h.branchHead("main"), attempt, phase, pr, h.branchHead("main"), pr != nil)
		h.exec(`INSERT INTO dev_work_item_issues (work_item_id, repo_id, issue_number, title, digest)
			SELECT $1, $2, $3, '需求', digest FROM dev_pool WHERE repo_id=$2 AND issue_number=$3`, ids[i], testRepoID, i+1)
	}
	h.exec(`UPDATE ai_tasks SET state='cancelled', finished_at=now()`)
	h.exec(`UPDATE ai_requests SET state='cancelled', finished_at=now()`)
	h.mustProcess()
	first := h.wantState(ids[0], StateDeveloping)
	if first.TaskID == nil {
		t.Fatalf("占用者没有开始实现：%+v", first)
	}
	third := h.wantState(ids[2], StateDeveloping)
	if third.TaskID != nil || !strings.Contains(third.Reason, "等工作项 #"+strconv.FormatInt(ids[0], 10)) {
		t.Fatalf("排在后面的工作项：任务 %v，原因 %q", third.TaskID, third.Reason)
	}
	h.wantOccupied(testRepoID, ids[0], StateDeveloping)
	// 不是占用者的已有 PR 照常返修
	h.failingRun(100+1, h.branchHead("main"))
	h.mustProcess()
	h.wantState(ids[1], StateFixing)
	// 重复处理不重复规划
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM ai_dev_requests WHERE work_item_id=$1`, ids[2]); n != 0 {
		t.Fatalf("排在后面的工作项被规划了 %d 次", n)
	}
	// 占用者终止：#2 成为占用者（它在返修，不需要规划实现），#3 仍等
	if err := h.svc.Stop(h.ctx, ids[0], "人工终止", "bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	if w := h.workItem(ids[2]); w.TaskID != nil || !strings.Contains(w.Reason, "等工作项 #"+strconv.FormatInt(ids[1], 10)) {
		t.Fatalf("#2 成为占用者之后 #3：任务 %v，原因 %q", w.TaskID, w.Reason)
	}
	h.wantOccupied(testRepoID, ids[1], StateFixing)
	// #2 终止：#3 开始实现；仍不选题
	if err := h.svc.Stop(h.ctx, ids[1], "人工终止", "bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	if w := h.workItem(ids[2]); w.TaskID == nil {
		t.Fatalf("前面的都结束后 #3 没有开始实现：%q", w.Reason)
	}
	h.wantOccupied(testRepoID, ids[2], StateDeveloping)
	selects := len(h.selectRequests(testRepoID))
	if err := h.svc.Stop(h.ctx, ids[2], "人工终止", "bob"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	if n := len(h.selectRequests(testRepoID)); n != selects+1 {
		t.Fatalf("全部结束后选题请求 %d → %d，想要多一轮", selects, n)
	}
}

// TestClaimRefusedWhenOccupied：升级前规划的选题（或与领取并发的另一轮）在仓库已有未结束的工作项时领取：不建工作项、
// 结束这一轮，原因写明占用者；Agent 收到结束说明而不是重新选题。
func TestClaimRefusedWhenOccupied(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.issue(2, "二", "b")
	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	h.offer(aid)
	// 期间另一个工作项被领取（模拟升级前的并行选题）
	other := h.queryInt(`INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id) VALUES ($1,99,$2,'req') RETURNING id`,
		h.taskOf(run).ID, h.runnerID)
	occupant := h.queryInt(`INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, phase)
		VALUES ($1,'pr_open','main','x',2,'claude-code',$2,'implement') RETURNING id`, testRepoID, other)
	seq := h.queryInt(`SELECT dev_select_seq FROM ai_repo_state WHERE repo_id=$1`, testRepoID)
	res, err := h.claimSel(aid, []int{2}, selection([]int{2}, nil, []int{1}))
	if err != nil || res.Claimed || res.Conflict || !strings.Contains(res.Reason, "工作项 #"+strconv.FormatInt(occupant, 10)) {
		t.Fatalf("领取 = %+v, %v，想要因为仓库被占用而不领取", res, err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_work_items WHERE repo_id=$1`, testRepoID); n != 1 {
		t.Fatalf("建了 %d 个工作项", n)
	}
	if got := h.queryInt(`SELECT dev_select_seq FROM ai_repo_state WHERE repo_id=$1`, testRepoID); got != seq+1 {
		t.Fatalf("选题轮次 %d → %d，想要结束这一轮", seq, got)
	}
}

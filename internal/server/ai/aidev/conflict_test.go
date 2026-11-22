package aidev

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aiprompt"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 合并冲突的返修（PLAN #26.11、C23）：合并队列交回的冲突按 Server 记录的归属开始一轮冲突返修，修复原 PR、只快进；
// 重复的交回只形成一轮；冲突返修单独计预算；结束的工作项不复活。

// handBack 模拟合并队列在出队的事务中交回冲突（PR head head 与默认分支的 base 冲突），返回是否交回。
func (h *harness) handBack(w *WorkItem, head, base string) bool {
	h.t.Helper()
	var ok bool
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		var err error
		ok, err = h.svc.HandBack(h.ctx, tx, Conflict{RepoID: testRepoID, PR: *w.PRNumber, HeadRef: w.Branch(), Head: head, BaseRef: "main", Base: base})
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return ok
}

// mergeMain 模拟开发机解决冲突：从 from 把默认分支合并进来（merge commit，不改写 from），推到 attempt 分支，返回 commit。
func (h *harness) mergeMain(branch, from string) string {
	h.t.Helper()
	h.git("branch", "-f", branch, from)
	h.git("checkout", "-q", branch)
	defer h.git("checkout", "-q", "main")
	h.git("merge", "-q", "--no-ff", "--no-edit", "main")
	return h.git("rev-parse", "HEAD")
}

// conflictRound 交回一个冲突（工作项当前的 head 与默认分支新的 commit），完成这一轮冲突返修并推进，返回新的 head。
func (h *harness) conflictRound(w *WorkItem, file string) (*WorkItem, string) {
	h.t.Helper()
	base := h.commitOn("main", map[string]string{file: "别人合并的改动\n"}, "别的 PR 合并："+file)
	if !h.handBack(w, w.HeadSHA, base) {
		h.t.Fatal("没有交回")
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	aid := h.acquire(h.fixTask(w).ID)
	fix := h.mergeMain(devreport.AttemptBranch(aid), w.HeadSHA)
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: fix})
	h.mustProcess()
	return h.wantState(w.ID, StatePROpen), fix
}

// TestPullOwner：归属只按 Server 记录的工作项与 PR（编号和本轮的正式分支都要匹配）：分支名像正式分支、编号对不上的
// （伪造、外部 Bot）和第三方分支都不算；推进中还没记下编号的是 Recording；结束的工作项是 Ended（不复活）。
func TestPullOwner(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	owner := func(pr int, ref string) PullOwner {
		t.Helper()
		o, err := Owner(h.ctx, h.db, testRepoID, pr, ref)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := owner(pr, w.Branch()); o.Kind != OwnerActive || o.WorkItem != w.ID || o.BaseRef != "main" {
		t.Fatalf("工作项的 PR：%+v", o)
	}
	for name, c := range map[string]struct {
		pr  int
		ref string
	}{
		"编号对、分支不是本轮的":   {pr, devreport.WorkItemBranch(w.ID, PhaseImplement, 2)},
		"分支像正式分支、编号对不上": {pr + 1, w.Branch()},
		"别的工作项的分支":      {pr, devreport.WorkItemBranch(w.ID+1, PhaseImplement, 1)},
		"第三方分支":         {pr, "feature"},
		"外部 Bot 的分支":    {pr, "renovate/go-1.x"},
	} {
		if o := owner(c.pr, c.ref); o.Kind != OwnerNone {
			t.Fatalf("%s：%+v，想要没有归属", name, o)
		}
	}
	// 推进中、还没记下编号
	h.exec(`UPDATE dev_work_items SET state=$2, pr_number=NULL WHERE id=$1`, w.ID, StateDelivering)
	if o := owner(pr, w.Branch()); o.Kind != OwnerRecording || o.WorkItem != w.ID {
		t.Fatalf("还没记下编号：%+v", o)
	}
	h.exec(`UPDATE dev_work_items SET state=$2, pr_number=$3 WHERE id=$1`, w.ID, StatePROpen, pr)
	if err := h.svc.Stop(h.ctx, w.ID, "人工终止", "bob"); err != nil {
		t.Fatal(err)
	}
	if o := owner(pr, w.Branch()); o.Kind != OwnerEnded || o.State != StateStopped {
		t.Fatalf("终止后：%+v", o)
	}
	// 结束的工作项不接受交回，不复活
	requests := h.devRequests(false)
	if h.handBack(w, w.HeadSHA, h.branchHead("main")) {
		t.Fatal("终止的工作项接受了交回")
	}
	h.mustProcess()
	if h.wantState(w.ID, StateStopped); h.devRequests(false) != requests {
		t.Fatal("终止后建了开发请求")
	}
}

// wantConflictTask 检查工作项 w 的冲突返修任务：从 PR head 开始，spec、work.json 和 prompt 都带上冲突（head、base）和
// 解决方式，没有审计问题；返回任务。
func (h *harness) wantConflictTask(w *WorkItem, pr int, head, base string) *ai.Task {
	h.t.Helper()
	task := h.fixTask(w)
	spec := h.specOf(task.ID)
	if c := spec.Conflict; c == nil || c.Head != head || c.BaseSHA != base || c.BaseRef != "main" || len(spec.Findings) != 0 || spec.PRNumber != pr {
		h.t.Fatalf("返修 spec = %s", jsonString(spec))
	}
	if r := h.request(task.RequestID); r.SHA != head {
		h.t.Fatalf("返修的起点 %s，想要 PR head %s", r.SHA, head)
	}
	var wd WorkData
	if err := json.Unmarshal(workData(spec), &wd); err != nil || wd.Conflict == nil || wd.Conflict.BaseSHA != base || wd.Conflict.Head != head {
		h.t.Fatalf("work.json = %s（%v）", workData(spec), err)
	}
	prompt := aiprompt.Fix(aiprompt.DevInput{Repository: repoName, CloneURL: "https://example.invalid/alice/demo.git", SHA: head, Spec: spec})
	for _, want := range []string{"## 合并冲突", "exec -- git fetch origin`", "git merge refs/remotes/origin/main", "不要 rebase", base} {
		if !strings.Contains(prompt, want) {
			h.t.Fatalf("返修 prompt 没有 %q：\n%s", want, prompt)
		}
	}
	return task
}

// TestConflictFix：交回的冲突开始一轮冲突返修（等待开发机领取，任务带上冲突的 head、base 和解决方式）；重复的交回、
// 再处理只形成一轮；开发机把默认分支合并进正式分支（merge commit，旧的提交保留），Server 只快进，PR 的新 head 等待新的
// 验证；冲突返修不占审计返修的预算；PR 有 kiln:hold 也照常返修（hold 只影响入队）。
func TestConflictFix(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	h.exec(`UPDATE dev_work_items SET hold_label=true WHERE id=$1`, w.ID)
	head := w.HeadSHA
	base := h.commitOn("main", map[string]string{"other.txt": "别人合并的改动\n"}, "别的 PR")
	for range 2 { // 重复的交回（合并队列重试、重复的事件）
		if !h.handBack(w, head, base) {
			t.Fatal("没有交回")
		}
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if w.FixRounds != 1 || w.RoundConflicts != 1 || w.RoundCheckFixes() != 0 || w.FixedConflict != conflictKey(head, base) ||
		w.FixedHead != head || !strings.Contains(w.Reason, "冲突返修") || !strings.Contains(w.Reason, "等待开发机领取") {
		t.Fatalf("开始冲突返修后：%+v", w)
	}
	task := h.wantConflictTask(w, pr, head, base)
	// 返修期间再次交回同一个冲突、重复处理：不另开一轮
	h.handBack(w, head, base)
	h.mustProcess()
	if again := h.workItem(w.ID); again.FixRounds != 1 || *again.TaskID != task.ID || h.devRequests(false) != 2 {
		t.Fatalf("重复交回后：轮次 %d、任务 %v、请求 %d", again.FixRounds, *again.TaskID, h.devRequests(false))
	}
	// 交付 merge commit：正式分支快进，旧 head 是新 head 的父提交（没有改写）
	aid := h.acquire(task.ID)
	fix := h.mergeMain(devreport.AttemptBranch(aid), head)
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: fix})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if h.branchHead(w.Branch()) != fix || w.HeadSHA != fix || *w.PRNumber != pr {
		t.Fatalf("推进后正式分支 %s、head %s、PR %v，想要 %s", h.branchHead(w.Branch()), w.HeadSHA, *w.PRNumber, fix)
	}
	if parents := strings.Fields(h.git("rev-list", "--parents", "-n", "1", fix)); len(parents) != 3 || parents[1] != head || parents[2] != base {
		t.Fatalf("新 head 的父提交 = %v，想要旧 head %s 和 base %s", parents[1:], head, base)
	}
	// 新 head 的验证还没有：不复用旧 head 的结果，不返修；冲突已过期，清空
	h.mustProcess()
	if w = h.wantState(w.ID, StatePROpen); w.ConflictHead != "" || w.FixRounds != 1 {
		t.Fatalf("新 head 等待验证时：%+v", w)
	}
	// 新 head 审计发现问题：照常审计返修（审计返修的预算没有被冲突返修占用）
	h.failingRun(pr, fix)
	h.mustProcess()
	if w = h.wantState(w.ID, StateFixing); w.RoundCheckFixes() != 1 || !strings.Contains(w.Reason, "第 1 轮返修") {
		t.Fatalf("审计返修：%+v", w)
	}
}

// TestConflictBudget：base 连续前进：同一个 head 面对新的 base 是新的冲突；冲突返修用完 max_fix_rounds 轮时受阻，原因可见；
// 人 retry 时这一轮带上冲突。
func TestConflictBudget(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _ := h.developed(1)
	w, _ = h.conflictRound(w, "a.txt")
	w, _ = h.conflictRound(w, "b.txt")
	if w.RoundConflicts != 2 || w.RoundCheckFixes() != 0 {
		t.Fatalf("两轮冲突返修后：%+v", w)
	}
	base := h.commitOn("main", map[string]string{"c.txt": "又一个\n"}, "第三个 PR")
	h.handBack(w, w.HeadSHA, base)
	h.mustProcess()
	w = h.wantState(w.ID, StateBlocked)
	if !strings.Contains(w.Reason, "冲突返修 2 轮后仍冲突") || w.FixRounds != 2 || w.FixedConflict != conflictKey(w.HeadSHA, base) {
		t.Fatalf("预算用完：%+v", w)
	}
	// 重复交回同一个冲突：不追加、不重复评论
	h.handBack(w, w.HeadSHA, base)
	h.mustProcess()
	if again := h.workItem(w.ID); again.Reason != w.Reason || again.State != StateBlocked {
		t.Fatalf("重复交回后：%+v", again)
	}
	if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if c := h.specOf(h.fixTask(w).ID).Conflict; c == nil || c.BaseSHA != base {
		t.Fatalf("retry 的返修没有带上冲突：%+v", c)
	}
}

// TestConflictCoordination：一个工作项同一时刻只有一轮返修：审计返修进行中交回的冲突不另开一轮，新的交付推进后冲突
// 过期；交回的冲突之后人推送了新的 commit（head 变了）时冲突过期、不返修，新 head 由正常的验证判断；受阻的工作项记下
// 冲突，原因追加说明。
func TestConflictCoordination(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	head := w.HeadSHA
	h.failingRun(pr, head)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	task := h.fixTask(w)
	base := h.commitOn("main", map[string]string{"other.txt": "x\n"}, "别的 PR")
	h.handBack(w, head, base)
	h.mustProcess()
	if again := h.wantState(w.ID, StateFixing); again.FixRounds != 1 || *again.TaskID != task.ID || again.ConflictHead != head {
		t.Fatalf("审计返修进行中交回冲突：%+v", again)
	}
	aid := h.acquire(task.ID)
	fids := []string{}
	for _, f := range h.specOf(task.ID).Findings {
		fids = append(fids, f.ID)
	}
	fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, fids...), Branch: devreport.AttemptBranch(aid), SHA: fix})
	h.mustProcess()
	if w = h.wantState(w.ID, StatePROpen); w.ConflictHead != "" || w.RoundConflicts != 0 {
		t.Fatalf("推进后冲突应过期：%+v", w)
	}

	// 人推送了新的 commit：交回的冲突过期，不返修
	base2 := h.commitOn("main", map[string]string{"other2.txt": "y\n"}, "又一个 PR")
	h.handBack(w, fix, base2)
	human := h.push(w.Branch(), fix, map[string]string{"human.txt": "人改的\n"})
	if err := h.svc.HandlePullHead(h.ctx, testRepoID, pr, human); err != nil {
		t.Fatal(err)
	}
	requests := h.devRequests(false)
	h.mustProcess()
	if w = h.wantState(w.ID, StatePROpen); w.ConflictHead != "" || h.devRequests(false) != requests || w.HeadSHA != human {
		t.Fatalf("人推送之后：%+v，请求 %d → %d", w, requests, h.devRequests(false))
	}

	// 受阻时交回：原因追加说明，不开始返修
	h.exec(`UPDATE dev_work_items SET state=$2, reason='等人处理' WHERE id=$1`, w.ID, StateBlocked)
	w = h.workItem(w.ID)
	h.handBack(w, human, base2)
	h.mustProcess()
	h.mustProcess()
	if w = h.wantState(w.ID, StateBlocked); w.Reason != "等人处理；"+conflictNote || h.devRequests(false) != requests {
		t.Fatalf("受阻时交回：%+v", w)
	}
}

// TestHandBackNewerHead：交回的冲突的 head 比工作项记下的新（PR 的 synchronize 事件还没处理）：交回不改记下的 head，执行者
// 读到正式分支就是冲突的 head，记下并返修这个 head，不丢掉冲突（被移出队列的 head 不会再自动入队）。
func TestHandBackNewerHead(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _ := h.developed(1)
	newer := h.push(w.Branch(), w.HeadSHA, map[string]string{"queue.txt": "队列更新的分支\n"})
	base := h.commitOn("main", map[string]string{"other.txt": "x\n"}, "别的 PR")
	if !h.handBack(w, newer, base) {
		t.Fatal("没有交回")
	}
	h.mustProcess()
	if w = h.wantState(w.ID, StateFixing); w.HeadSHA != newer || w.FixedConflict != conflictKey(newer, base) {
		t.Fatalf("交回较新的 head 后：%+v", w)
	}
	if c := h.specOf(h.fixTask(w).ID).Conflict; c == nil || c.Head != newer {
		t.Fatalf("返修的冲突 = %+v", c)
	}
	if spec := h.specOf(h.fixTask(w).ID); spec.Mode != aispec.DevFix {
		t.Fatalf("任务模式 = %s", spec.Mode)
	}
}

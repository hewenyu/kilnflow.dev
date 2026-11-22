package aidev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 合并冲突的返修闭环（PLAN #26.11、#28.3、C23）的模块测试：AI 开发与真实的合并队列（同一个数据库、同一个 fakegithub，
// 冲突由 git 真实产生），经与组装层相同的接口（mergequeue.DevConflicts）衔接。不依赖维护者的任何操作：冲突交回 → 开发机
// 解决冲突（merge commit）→ 正式分支快进、原 PR 更新 → 新 head 重新验证 → 自动入队 → 合并。

// flowVerifications 是合并队列的验证来源的替身：verified 中的 head 的 CI 和 AI 审计都已通过，其余还没有验证。
type flowVerifications struct {
	mu       sync.Mutex
	verified map[string]bool
}

func (v *flowVerifications) source(_ context.Context, _ store.Querier, _ int64, _ int, head, _ string, _ bool) ([]store.Verification, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.verified[head] {
		return nil, nil
	}
	return []store.Verification{
		{ID: 1, Name: "CI", Source: "ci", State: string(store.RunSucceeded)},
		{ID: 2, Name: "AI 审计", Source: mergecfg.AIReview, Builtin: true, State: string(store.RunSucceeded)},
	}, nil
}

// flowDevConflicts 与组装层的 devConflicts 相同：把 aidev 的归属和交回接到合并队列。failNext 不为空时下一次交回返回它
// （模拟交回的事务暂时失败）。
type flowDevConflicts struct {
	s        *Service
	mu       sync.Mutex
	failNext error
}

func (*flowDevConflicts) Owner(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (mergequeue.DevOwner, error) {
	o, err := Owner(ctx, q, repoID, pr, headRef)
	kinds := map[OwnerKind]mergequeue.DevOwnerKind{
		OwnerNone: mergequeue.DevOwnerNone, OwnerActive: mergequeue.DevOwnerActive, OwnerEnded: mergequeue.DevOwnerEnded,
		OwnerRecording: mergequeue.DevOwnerRecording,
	}
	return mergequeue.DevOwner{Kind: kinds[o.Kind], WorkItem: o.WorkItem, State: o.State, BaseRef: o.BaseRef}, err
}

func (d *flowDevConflicts) HandBack(ctx context.Context, tx pgx.Tx, c mergequeue.DevConflict) (bool, error) {
	d.mu.Lock()
	err := d.failNext
	d.failNext = nil
	d.mu.Unlock()
	if err != nil {
		return false, err
	}
	return d.s.HandBack(ctx, tx, Conflict{RepoID: c.RepoID, PR: c.PR, HeadRef: c.HeadRef, Head: c.Head, BaseRef: c.BaseRef, Base: c.Base})
}

// flow 是 harness 加上合并队列。
type flow struct {
	*harness
	mq  *mergequeue.Service
	ver *flowVerifications
	dev *flowDevConflicts
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{mergecfg.Path: "auto: true\nrequired: [ci, ai-review]\n"}, "开启自动入队")
	f := &flow{harness: h, ver: &flowVerifications{verified: map[string]bool{}}, dev: &flowDevConflicts{s: h.svc}}
	client := h.client()
	f.mq = mergequeue.New(mergequeue.Deps{
		DB: h.db, Forge: func(r *store.Repo) forge.Repo { return forge.GitHub(client, r.InstallationID, r.FullName()) },
		Outbox: outbox.NewWriter(true), Log: slog.New(slog.DiscardHandler),
		Verifications: []store.VerificationSource{f.ver.source}, HoldPending: HoldPending, DevConflicts: f.dev,
	})
	return f
}

// verify 模拟 PR pr 的 head 按默认分支规划完成、CI 和 AI 审计都通过（验证结束的事件提名自动入队）。
func (f *flow) verify(pr int, head string) {
	f.t.Helper()
	f.exec(`INSERT INTO pr_plans (repo_id, pr_number, sha, base_ref) VALUES ($1,$2,$3,'main') ON CONFLICT DO NOTHING`, testRepoID, pr, head)
	f.ver.mu.Lock()
	f.ver.verified[head] = true
	f.ver.mu.Unlock()
	if err := f.mq.Nominate(f.ctx, testRepoID, pr); err != nil {
		f.t.Fatal(err)
	}
}

func (f *flow) processQueue() {
	f.t.Helper()
	if err := f.mq.Process(f.ctx, testRepoID); err != nil {
		f.t.Fatalf("合并队列：%v", err)
	}
}

func (f *flow) queueEntry(pr int) *mergequeue.Entry {
	f.t.Helper()
	e, err := mergequeue.Latest(f.ctx, f.db, testRepoID, pr)
	if err != nil || e == nil {
		f.t.Fatalf("PR #%d 的队列项：%v, %v", pr, e, err)
	}
	return e
}

// resolve 模拟开发机解决冲突：从 head 把 main 合并进来（git 报告冲突），写入解决后的 feature.go，提交 merge commit，
// 推到 attempt 分支 branch，返回 commit。
func (f *flow) resolve(branch, head, resolved string) string {
	f.t.Helper()
	f.git("branch", "-f", branch, head)
	// 合并队列在 fakegithub 中直接移动了 main（工作区没有跟着变）：强制切换
	f.git("checkout", "-q", "-f", branch)
	defer f.git("checkout", "-q", "-f", "main")
	if _, err := f.gitTry("merge", "--no-edit", "main"); err == nil {
		f.t.Fatal("合并 main 应该冲突")
	}
	if err := os.WriteFile(filepath.Join(f.dir, "feature.go"), []byte(resolved), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git("add", "-A")
	f.git("commit", "-q", "--no-edit")
	return f.git("rev-parse", "HEAD")
}

// gitTry 执行 git，失败时返回错误（不结束测试）。
func (f *flow) gitTry(args ...string) (string, error) {
	cmd := exec.CommandContext(context.WithoutCancel(f.ctx), "git", args...)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// developedWith 与 developed 相同，交付的 feature.go 是 content。
func (f *flow) developedWith(issue int, content string) (*WorkItem, int) {
	f.t.Helper()
	aid := f.acquire(f.taskOf(f.startSelect()).ID)
	res := f.mustClaim(aid, []int{issue})
	sha := f.push(devreport.AttemptBranch(aid), f.branchHead("main"), map[string]string{"feature.go": content})
	f.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{issue}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	f.mustProcess()
	w := f.wantState(res.WorkItemID, StatePROpen)
	return w, *w.PRNumber
}

// fixConflict 完成工作项 w 的冲突返修：开发机解决冲突并交付，执行者推进（正式分支快进，PR 的 head 随之更新）。返回新的 head。
func (f *flow) fixConflict(w *WorkItem, resolved string) string {
	f.t.Helper()
	w = f.wantState(w.ID, StateFixing)
	task := f.fixTask(w)
	if c := f.specOf(task.ID).Conflict; c == nil || c.Head != w.HeadSHA || c.BaseSHA != f.branchHead("main") {
		f.t.Fatalf("返修的冲突 = %+v，想要 head %s、base %s", c, w.HeadSHA, f.branchHead("main"))
	}
	aid := f.acquireBy(task.ID, f.addRunner(fmt.Sprintf("dev-fix-%d", task.ID), "host-fix")) // 另一台合适的开发机接手
	fix := f.resolve(devreport.AttemptBranch(aid), w.HeadSHA, resolved)
	issues, err := workIssues(f.ctx, f.db, w.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered(issueNumbers(issues), ""), Branch: devreport.AttemptBranch(aid), SHA: fix})
	f.mustProcess()
	w = f.wantState(w.ID, StatePROpen)
	if f.branchHead(w.Branch()) != fix || w.HeadSHA != fix {
		f.t.Fatalf("正式分支 %s、工作项 head %s，想要 %s", f.branchHead(w.Branch()), w.HeadSHA, fix)
	}
	// GitHub 的 PR head 跟随分支，synchronize 事件带来新 head
	f.fake.UpdatePull(f.fakeRepo(), *w.PRNumber, func(p *fakegithub.Pull) { p.HeadSHA = fix })
	if err := f.svc.HandlePullHead(f.ctx, testRepoID, *w.PRNumber, fix); err != nil {
		f.t.Fatal(err)
	}
	return fix
}

// TestConflictLoopThirdPartyMerge：第三方 PR 合并让 Bot PR 与 main 冲突（两边都改了 feature.go）：合并队列交回 AI 开发，
// 原 PR 被更新（merge commit，第三方的改动和 Bot 的改动都在），新 head 重新验证通过后自动入队并合并；整个过程没有维护者
// 的操作。
func TestConflictLoopThirdPartyMerge(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w, pr := f.developed(1)
	head := w.HeadSHA
	f.commitOn("main", map[string]string{"feature.go": "package demo\n\n// 第三方的改动\n"}, "第三方 PR 合并")
	f.verify(pr, head)
	f.processQueue()
	e := f.queueEntry(pr)
	if e.State != mergequeue.StateRemoved || e.Removal != mergequeue.RemovalConflictDev {
		t.Fatalf("冲突的 Bot PR：%+v", e)
	}
	requests := f.devRequests(false)
	f.mustProcess()
	if f.devRequests(false) != requests+1 {
		t.Fatalf("交回后开发请求 %d → %d，想要多一个返修", requests, f.devRequests(false))
	}
	fix := f.fixConflict(w, "package demo\n\n// 第三方的改动\n// Bot 的改动\n")
	// 没有验证之前不入队（不复用旧 head 的通过结果）
	if err := f.mq.Nominate(f.ctx, testRepoID, pr); err != nil {
		t.Fatal(err)
	}
	f.processQueue()
	if e := f.queueEntry(pr); e.State != mergequeue.StateRemoved || e.HeadSHA != head {
		t.Fatalf("新 head 验证之前：%+v", e)
	}
	f.verify(pr, fix)
	f.processQueue()
	if e := f.queueEntry(pr); e.State != mergequeue.StateMerged || e.HeadSHA != fix || e.EnqueuedBy != mergequeue.AutoActor {
		t.Fatalf("新 head 验证通过后：%+v", e)
	}
	p := f.pull(pr)
	if !p.Merged || len(p.Comments) != 1 || !strings.Contains(p.Comments[0], "已交给 AI 开发返修") {
		t.Fatalf("PR = %+v", p)
	}
	content := f.git("show", "main:feature.go")
	if !strings.Contains(content, "第三方的改动") || !strings.Contains(content, "Bot 的改动") {
		t.Fatalf("合并后的 feature.go 丢了改动：%q", content)
	}
}

// TestConflictRetargetedPR：工作项在 main 上开了 PR，之后仓库的默认分支改为 next、PR 也改投 next，next 上的提交与 PR
// 冲突。工作项的目标分支、交付核对和返修输入都按 main，交回会让 AI 开发合并旧的 main、解决不了 next 上的冲突：合并队列
// 出队并说明目标分支变了、需要人处理（conflict_author），不交回、不建返修任务；直接交回 next 的冲突也被拒绝。
func TestConflictRetargetedPR(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w, pr := f.developed(1)
	f.git("branch", "next", "main")
	base := f.commitOn("next", map[string]string{"feature.go": "package demo\n\n// next 上的改动\n"}, "next 上的提交")
	f.fake.SetDefaultBranch(f.fakeRepo(), "next")
	f.exec(`UPDATE repos SET default_branch='next' WHERE id=$1`, testRepoID)
	f.fake.UpdatePull(f.fakeRepo(), pr, func(p *fakegithub.Pull) { p.BaseRef = "next" })
	f.exec(`INSERT INTO pr_plans (repo_id, pr_number, sha, base_ref) VALUES ($1,$2,$3,'next')`, testRepoID, pr, w.HeadSHA)
	f.verify(pr, w.HeadSHA)
	requests := f.devRequests(false)
	f.processQueue()
	e := f.queueEntry(pr)
	if e.State != mergequeue.StateRemoved || e.Removal != mergequeue.RemovalConflictAuthor || e.ConflictBase != base ||
		!strings.Contains(e.Reason, "PR 的目标分支已从 AI 开发工作项") || !strings.Contains(e.Reason, "main 改为 next") {
		t.Fatalf("改投后冲突的 Bot PR：%+v", e)
	}
	if p := f.pull(pr); len(p.Comments) != 1 || strings.Contains(p.Comments[0], "已交给 AI 开发返修") || !strings.Contains(p.Comments[0], "需要人处理") {
		t.Fatalf("PR = %+v", p)
	}
	f.mustProcess()
	if w := f.wantState(w.ID, StatePROpen); w.ConflictHead != "" || w.ConflictBase != "" || f.devRequests(false) != requests {
		t.Fatalf("工作项 %s，开发请求 %d → %d：不应交回、不应返修", jsonString(w), requests, f.devRequests(false))
	}
	// 直接交回 next 上的冲突（例如合并队列记下交回意图之后 PR 才改投）：目标分支不是工作项的，拒绝
	if err := f.db.Tx(f.ctx, func(tx pgx.Tx) error {
		ok, err := f.svc.HandBack(f.ctx, tx, Conflict{RepoID: testRepoID, PR: pr, HeadRef: w.Branch(), Head: w.HeadSHA, BaseRef: "next", Base: base})
		if err == nil && ok {
			err = errors.New("接受了目标分支不同的冲突")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := f.workItem(w.ID); w.ConflictHead != "" {
		t.Fatalf("工作项记下了目标分支不同的冲突：%s", jsonString(w))
	}
}

// TestConflictLoopTwoBotPRs：升级前同一个仓库留下两个都验证通过的 Bot PR（同一个文件），先合并第一个让第二个冲突：第二个
// 自动交回、更新、重新验证、入队合并；第一个合并后释放占用，第二个成为占用者。
func TestConflictLoopTwoBotPRs(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w1, pr1 := f.developed(1)
	f.issue(10, "二", "b") // 编号避开第一个 PR
	// 升级前的并行：第一个工作项还占着仓库时第二个也被领取（这里暂时让它看起来已结束，模拟升级前没有占用判断）
	f.exec(`UPDATE dev_work_items SET state='stopped', active=false WHERE id=$1`, w1.ID)
	w2, pr2 := f.developedWith(10, "package demo\n\n// 第二个工作项\n")
	f.exec(`UPDATE dev_work_items SET state='pr_open', active=true WHERE id=$1`, w1.ID)
	for _, c := range []struct {
		pr   int
		head string
	}{{pr1, w1.HeadSHA}, {pr2, w2.HeadSHA}} {
		f.verify(c.pr, c.head)
	}
	f.processQueue() // 第一个合并；第二个落后于 main，更新分支时冲突，交回
	if e := f.queueEntry(pr1); e.State != mergequeue.StateMerged {
		t.Fatalf("第一个 PR：%+v", e)
	}
	if e := f.queueEntry(pr2); e.State != mergequeue.StateRemoved || e.Removal != mergequeue.RemovalConflictDev {
		t.Fatalf("第二个 PR：%+v", e)
	}
	// 第一个的合并事件：结束、释放占用
	if _, err := f.svc.HandlePullClosed(f.ctx, testRepoID, ClosedPull{
		Number: pr1, HeadRef: w1.Branch(), HeadSHA: w1.HeadSHA, Merged: true,
		MergeSHA: f.branchHead("main"),
	}, "kiln[bot]"); err != nil {
		t.Fatal(err)
	}
	f.wantState(w1.ID, StateMerged)
	f.mustProcess()
	f.wantOccupied(testRepoID, w2.ID, StateFixing)
	fix := f.fixConflict(w2, "package demo\n\n// 两个工作项的改动都在\n")
	f.verify(pr2, fix)
	f.processQueue()
	if e := f.queueEntry(pr2); e.State != mergequeue.StateMerged || e.HeadSHA != fix {
		t.Fatalf("第二个 PR 返修后：%+v", e)
	}
}

// TestConflictStaleAfterRemovalRetry：合并队列记下冲突出队（action=removing）后交回的事务暂时失败；重试之前人把 main
// 合并进正式分支解决了冲突，新 head 已经由 synchronize 事件记下。重试按队列项记下的旧 head 交回：工作项保留新 head、
// 旧 head 的冲突按过期清掉，不另开返修、不消耗预算；冲突返修预算已经用完时也不会被误判为仍冲突而受阻（占住仓库）。
func TestConflictStaleAfterRemovalRetry(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w, pr := f.developed(1)
	old := w.HeadSHA
	f.exec(`UPDATE dev_work_items SET round_conflicts=max_fix_rounds WHERE id=$1`, w.ID)
	f.commitOn("main", map[string]string{"feature.go": "package demo\n\n// 第三方的改动\n"}, "第三方 PR 合并")
	f.verify(pr, old)
	f.dev.mu.Lock()
	f.dev.failNext = errors.New("交回的事务暂时失败")
	f.dev.mu.Unlock()
	if err := f.mq.Process(f.ctx, testRepoID); err == nil {
		t.Fatal("交回失败时合并队列应返回错误（稍后重试）")
	}
	if e := f.queueEntry(pr); e.State != mergequeue.StateQueued || e.Removal != mergequeue.RemovalConflictDev {
		t.Fatalf("交回失败后的队列项：%+v", e)
	}
	// 人把 main 合并进正式分支解决冲突，PR 的 synchronize 事件记下新 head
	resolved := f.resolve(w.Branch(), old, "package demo\n\n// 第三方的改动\n// 人解决的\n")
	f.fake.UpdatePull(f.fakeRepo(), pr, func(p *fakegithub.Pull) { p.HeadSHA = resolved })
	if err := f.svc.HandlePullHead(f.ctx, testRepoID, pr, resolved); err != nil {
		t.Fatal(err)
	}
	requests := f.devRequests(false)
	f.processQueue() // 重试出队，按记下的旧 head 交回
	if e := f.queueEntry(pr); e.State != mergequeue.StateRemoved || e.HeadSHA != old {
		t.Fatalf("重试出队后：%+v", e)
	}
	f.mustProcess()
	w = f.wantState(w.ID, StatePROpen)
	if w.HeadSHA != resolved || w.ConflictHead != "" || w.FixedConflict != "" || f.devRequests(false) != requests {
		t.Fatalf("旧 head 的冲突应过期：old=%s resolved=%s，工作项 %+v，开发请求 %d → %d", old, resolved, w, requests, f.devRequests(false))
	}
	// 新 head 验证通过后自动入队合并
	f.verify(pr, resolved)
	f.processQueue()
	if e := f.queueEntry(pr); e.State != mergequeue.StateMerged || e.HeadSHA != resolved {
		t.Fatalf("新 head 验证通过后：%+v", e)
	}
}

// TestConflictDelayedSynchronize：冲突交回时工作项记下的 head 就是冲突的 head；执行者处理之前人把 main 合并进正式分支解决了
// 冲突，PR 的 synchronize 事件还没处理（记下的 head 仍是旧的）。执行者按正式分支判断冲突已过期：清空、保持 pr_open，不另开
// 返修、不消耗预算；冲突返修预算已经用完时也不以冲突为由受阻（F253-1）。迟到的 synchronize 事件记下新 head 后照常验证、
// 自动入队合并。
func TestConflictDelayedSynchronize(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w, pr := f.developed(1)
	old := w.HeadSHA
	f.exec(`UPDATE dev_work_items SET fix_rounds=max_fix_rounds, round_conflicts=max_fix_rounds WHERE id=$1`, w.ID)
	f.commitOn("main", map[string]string{"feature.go": "package demo\n\n// 第三方的改动\n"}, "第三方 PR 合并")
	f.verify(pr, old)
	f.processQueue()
	if e := f.queueEntry(pr); e.State != mergequeue.StateRemoved || e.Removal != mergequeue.RemovalConflictDev {
		t.Fatalf("冲突的 Bot PR：%+v", e)
	}
	if got := f.workItem(w.ID); got.ConflictHead != old || got.HeadSHA != old {
		t.Fatalf("交回后：%+v", got)
	}
	// 人解决了冲突（正式分支已经指向新 head），synchronize 事件还没到
	resolved := f.resolve(w.Branch(), old, "package demo\n\n// 第三方的改动\n// 人解决的\n")
	f.fake.UpdatePull(f.fakeRepo(), pr, func(p *fakegithub.Pull) { p.HeadSHA = resolved })
	requests := f.devRequests(false)
	f.mustProcess()
	w = f.wantState(w.ID, StatePROpen)
	if w.ConflictHead != "" || w.FixedConflict != "" || w.HeadSHA != old || f.devRequests(false) != requests {
		t.Fatalf("旧 head 的冲突应过期：old=%s resolved=%s，工作项 %+v，开发请求 %d → %d", old, resolved, w, requests, f.devRequests(false))
	}
	if err := f.svc.HandlePullHead(f.ctx, testRepoID, pr, resolved); err != nil {
		t.Fatal(err)
	}
	f.mustProcess()
	if w = f.wantState(w.ID, StatePROpen); w.HeadSHA != resolved {
		t.Fatalf("synchronize 之后：%+v", w)
	}
	f.verify(pr, resolved)
	f.processQueue()
	if e := f.queueEntry(pr); e.State != mergequeue.StateMerged || e.HeadSHA != resolved {
		t.Fatalf("新 head 验证通过后：%+v", e)
	}
}

// TestConflictLoopThirdPartyPR：第三方 PR 出现同样的冲突时只提示提交者，AI 开发的请求数不变；分支名伪装成工作项的正式
// 分支、编号对不上的 PR 同样按第三方处理。
func TestConflictLoopThirdPartyPR(t *testing.T) {
	f := newFlow(t)
	f.issue(1, "一", "a")
	w, _ := f.developed(1)
	for _, c := range []struct {
		n      int
		branch string
	}{{50, "feature"}, {51, w.Branch()}} {
		f.git("branch", "-f", fmt.Sprintf("third-%d", c.n), "main")
		head := f.commitOn(fmt.Sprintf("third-%d", c.n), map[string]string{"README.md": fmt.Sprintf("第三方 %d\n", c.n)}, "第三方")
		f.fake.SetPull(f.fakeRepo(), fakegithub.Pull{Number: c.n, Title: "第三方", HeadRef: c.branch, HeadSHA: head, Author: "carol"})
		f.verify(c.n, head)
	}
	f.commitOn("main", map[string]string{"README.md": "main 改了同一行\n"}, "别的 PR 合并")
	requests := f.devRequests(false)
	f.processQueue()
	f.processQueue()
	for _, n := range []int{50, 51} {
		e := f.queueEntry(n)
		if e.State != mergequeue.StateRemoved || e.Removal != mergequeue.RemovalConflictAuthor || !strings.Contains(e.Reason, "请提交者") {
			t.Fatalf("第三方 PR #%d：%+v", n, e)
		}
	}
	f.mustProcess()
	if w = f.wantState(w.ID, StatePROpen); f.devRequests(false) != requests || w.ConflictHead != "" {
		t.Fatalf("第三方 PR 冲突后：请求 %d → %d，工作项 %+v", requests, f.devRequests(false), w)
	}
}

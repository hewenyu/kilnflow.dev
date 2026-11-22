package mergequeue

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 合并队列的目标分支（PLAN #28.5）：branches 配置多个分支时各条分支独立推进、各自排队；配置错误时保持队列不动；
// 调度以平台上 PR 的目标分支为准。

// targetBranch 从 main 建一条目标分支 name 并提交一个文件，返回新提交（切回 main）。
func (h *harness) targetBranch(name string) string {
	h.t.Helper()
	h.git("checkout", "-q", "-b", name, "main")
	sha := h.commit(map[string]string{name + ".md": name + "\n"}, "初始化目标分支 "+name)
	h.git("checkout", "-q", "main")
	return sha
}

// branchFrom 从 base 建分支 name，提交一个文件后切回 main，返回分支的 head（不需要 checkout 的目标分支用）。
func (h *harness) branchFrom(name, base string) string {
	h.t.Helper()
	h.git("checkout", "-q", "-b", name, base)
	sha := h.commit(map[string]string{name + ".txt": name + "\n"}, name)
	h.git("checkout", "-q", "main")
	return sha
}

// openPRBase 登记一个打开的、带 kiln:merge 的 PR（目标分支 base），并加入队列。
func (h *harness) openPRBase(n int, branch, head, base string) {
	h.t.Helper()
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: n, Title: "PR " + branch, HeadRef: branch, HeadSHA: head, BaseRef: base, Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, n, head, base, "github:"+testAccount); err != nil {
		h.t.Fatal(err)
	}
}

// verifiedBase 模拟 Kiln 按目标分支 base 规划完 PR head 的验证，run 处于 states（每个一个 run）。
func (h *harness) verifiedBase(n int, head, base string, states ...store.RunState) {
	h.t.Helper()
	h.planned(n, head, base)
	for i, st := range states {
		h.exec(`INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
			VALUES ($1,'ci.yml',$2,'pull_request',$3,$4,'webhook',gen_random_uuid()::text,$5,$6,$7)`,
			repoID, "CI "+string(rune('A'+i)), workflow.PullRef(n), head, n, string(st), fmt.Sprintf(`{"base_ref":%q}`, base))
	}
}

// TestConfigErrorKeepsQueue：配置错误时队列保持不动（包括目标是其他分支的项）：不按分支出队、不发更新或合并请求，
// 只更新显示的状态；与配置无关的收尾（PR 已经合并）照常完成。
func TestConfigErrorKeepsQueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "method: octopus\n"}, "坏配置")
	h.targetBranch("dev")
	devHead := h.branchFrom("feature-dev", "dev")
	h.openPRBase(7, "feature-dev", devHead, "dev")
	h.verifiedBase(7, devHead, "dev", store.RunSucceeded)
	mainHead := h.branch("feature-main")
	h.openPR(8, "feature-main", mainHead)
	h.verified(8, mainHead, store.RunSucceeded)
	doneHead := h.branch("done")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 5, Title: "done", HeadRef: "done", HeadSHA: doneHead, State: "closed", Merged: true, MergeCommitSHA: doneHead})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 5, doneHead, "main", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}

	h.process()
	h.wantState(5, StateMerged, "")
	h.wantStatus(7, "配置错误：")
	h.wantStatus(8, "配置错误：")
	if n := h.countRequests("PUT /repos/alice/demo/pulls/"); n != 0 {
		t.Fatalf("配置错误时不应发更新或合并请求：%d 个", n)
	}
	if !slices.Contains(h.pull(7).Labels, Label) || !slices.Contains(h.pull(8).Labels, Label) {
		t.Error("配置错误时不应去掉 label")
	}
	if len(h.pull(7).Comments) != 0 || len(h.pull(8).Comments) != 0 {
		t.Error("配置错误时不应评论")
	}
}

// TestMultiBranch：配置多个目标分支时各自独立推进：一条分支的队首在等验证时，另一条分支照常合并；每条分支按入队
// 顺序合并（后面的即使验证通过也不越过队首）。
func TestMultiBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "允许多个目标分支")
	dev := h.targetBranch("dev")
	d7 := h.branchFrom("dev-7", "dev")
	m8 := h.branch("main-8")
	h.openPRBase(7, "dev-7", d7, "dev")
	h.verifiedBase(7, d7, "dev", store.RunSucceeded)
	h.openPR(8, "main-8", m8)
	h.verified(8, m8, store.RunSucceeded)

	h.process()
	h.wantState(7, StateMerged, "")
	h.wantState(8, StateMerged, "")
	if e := h.entry(7); e.BaseRef != "dev" {
		t.Errorf("PR #7 的目标分支：%q", e.BaseRef)
	}
	if h.pull(8).MergeCommitSHA == "" || h.pull(7).MergeCommitSHA == "" {
		t.Fatalf("两条分支都应合并：main=%q dev=%q", h.pull(8).MergeCommitSHA, h.pull(7).MergeCommitSHA)
	}
	if got := h.git("rev-parse", "dev"); got == dev {
		t.Error("dev 应已前进")
	}
	if n := h.countRequests(fmt.Sprintf("PUT /repos/alice/demo/pulls/%d/merge", 8)); n != 1 {
		t.Errorf("main 分支的合并请求：%d", n)
	}

	// 队首没有按 dev 规划完时，后面的 PR 即使验证通过、也不越过队首；另一条分支不受影响
	d9 := h.branchFrom("dev-9", "dev")
	d10 := h.branchFrom("dev-10", "dev")
	h.openPRBase(9, "dev-9", d9, "dev") // 还没有按 dev 规划
	h.openPRBase(10, "dev-10", d10, "dev")
	h.verifiedBase(10, d10, "dev", store.RunSucceeded)
	h.process()
	h.wantStatus(9, "等待 Kiln 按 dev 规划")
	h.wantStatus(10, "目标 dev · 排在第 2 个")
	if h.pull(10).Merged || h.pull(9).Merged {
		t.Fatal("队首没有完成验证时后面的 PR 不应合并")
	}

	// 队首完成后按顺序处理
	h.verifiedBase(9, d9, "dev", store.RunSucceeded)
	h.process()
	h.wantState(9, StateMerged, "")
	if h.pull(10).Merged {
		t.Fatal("PR #10 落后于 dev，应先更新分支，不应直接合并")
	}
}

// TestPerBranchBudget：一条分支的队首反复无法推进时，用完这条分支的步数预算就交给下一轮（返回错误），不影响其他
// 分支在这次处理中合并。
func TestPerBranchBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "允许多个目标分支")
	h.targetBranch("dev")
	d7 := h.branchFrom("dev-7", "dev")
	m8 := h.branch("main-8")
	h.openPR(8, "main-8", m8)
	h.verified(8, m8, store.RunSucceeded)
	h.openPRBase(7, "dev-7", d7, "dev")
	h.verifiedBase(7, d7, "dev", store.RunSucceeded)
	// 主分支的合并总是回答 head 已经变化（平台确认没有合并）：这一项一直原地打转
	base := h.svc.forge
	h.svc.forge = func(r *store.Repo) forge.Repo { return stuckMerge{Repo: base(r), pr: 8} }

	err := h.svc.Process(h.ctx, repoID)
	if err == nil || !strings.Contains(err.Error(), "超过 20 步") {
		t.Fatalf("用完步数预算应返回错误：%v", err)
	}
	h.wantState(7, StateMerged, "")
	if e := h.entry(8); e.State != StateQueued {
		t.Fatalf("一直打转的 PR 应保持排队：%+v", e)
	}
}

// stuckMerge 让 PR pr 的合并总是回答 head 已经变化（平台确认没有合并），其余 PR 照常合并。
type stuckMerge struct {
	forge.Repo
	pr int
}

func (s stuckMerge) Merge(ctx context.Context, number int, method, expectedHead string) (string, error) {
	if number == s.pr {
		return "", forge.ErrHeadChanged
	}
	return s.Repo.Merge(ctx, number, method, expectedHead)
}

// TestUpdatingBaseChange：正在更新 PR 分支时用户把目标分支改成另一条允许的分支（head 没变）：重置更新意图，按新的
// 目标分支重新评估并合并，不会拿着旧的目标分支去更新。
func TestUpdatingBaseChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "允许多个目标分支")
	h.targetBranch("dev")
	head := h.branchFrom("feature", "dev")
	h.openPRBase(7, "feature", head, "main")
	h.exec(`UPDATE merge_queue SET action='updating', action_head=$2, action_at=now(), action_tries=1
		WHERE repo_id=$1 AND pr_number=7`, repoID, head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.BaseRef = "dev" })
	h.verifiedBase(7, head, "dev", store.RunSucceeded)

	h.process()
	e := h.wantState(7, StateMerged, "")
	if e.BaseRef != "dev" || e.Action != actionNone {
		t.Fatalf("应按新的目标分支重新评估：%+v", e)
	}
	if h.pull(7).BaseRef != "dev" || !h.pull(7).Merged {
		t.Fatalf("PR 应合入 dev：%+v", h.pull(7))
	}
	if n := h.countRequests("PUT /repos/alice/demo/pulls/7/update-branch"); n != 0 {
		t.Errorf("不应按旧的目标分支更新 PR：%d 个请求", n)
	}
}

// conflictPRBase 制造与目标分支 base 冲突的 PR n（feature 与 base 改同一行），验证通过、排在队列中，返回 PR 的 head。
// base 之外的 main 也前进一个 commit，保证 PR 落后于 main。
func (h *harness) conflictPRBase(n int, branch, base string) string {
	h.t.Helper()
	h.git("checkout", "-q", "-b", branch, "main")
	head := h.commit(map[string]string{"README.md": branch + "\n"}, branch)
	h.git("checkout", "-q", base)
	h.commit(map[string]string{"README.md": "目标分支的改动 " + branch + "\n"}, "别的 PR 合并："+branch)
	h.git("checkout", "-q", "main")
	if base != "main" {
		h.commit(map[string]string{"main.txt": "main 前进\n"}, "main 前进")
	}
	h.openPRBase(n, branch, head, base)
	h.verifiedBase(n, head, base, store.RunSucceeded)
	return head
}

// TestConflictOnOtherBase：非默认目标分支上的冲突按当前的目标分支归因：读取的是这条分支的 head，记下的冲突目标分支、
// 交回 AI 开发的 BaseRef 都是这条分支，评论也说明这条分支。
func TestConflictOnOtherBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "允许多个目标分支")
	h.targetBranch("dev")
	head := h.conflictPRBase(7, "kiln/dev-3", "dev")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "dev", WorkItem: 3, State: "pr_open"})

	h.process()
	e := h.wantState(7, StateRemoved, "已交给 AI 开发返修（工作项 #3）")
	base := h.git("rev-parse", "dev")
	if e.Removal != RemovalConflictDev || e.ConflictBase != base {
		t.Fatalf("出队记录：removal=%q base=%q，dev 的 head 是 %s", e.Removal, e.ConflictBase, base)
	}
	want := []DevConflict{{RepoID: repoID, PR: 7, HeadRef: "kiln/dev-3", Head: head, BaseRef: "dev", Base: base}}
	if got := h.dev.backs(); !slices.Equal(got, want) {
		t.Fatalf("交回 = %+v，想要 %+v", got, want)
	}
	p := h.pull(7)
	if len(p.Comments) != 1 || !strings.Contains(p.Comments[0], "目标分支 dev") || slices.Contains(p.Labels, Label) {
		t.Fatalf("评论应按当前的目标分支说明：%+v", p)
	}
}

// rebaseForge 在平台回答更新分支有冲突之前把 PR 的目标分支改成 main（模拟期间有人编辑了 PR）。
type rebaseForge struct {
	forge.Repo
	h *harness
}

func (f rebaseForge) UpdateBranch(_ context.Context, number int, _ string) error {
	f.h.fake.UpdatePull(f.h.repo, number, func(p *fakegithub.Pull) { p.BaseRef = "main" })
	return forge.ErrMergeConflict
}

// TestConflictAfterBaseChange：请求更新时读到的目标分支和平台回答冲突时 PR 的目标分支不同（期间改投了分支）：这次
// 冲突不再属于这条分支，不归因、不交回、不评论，按最新的目标分支重新评估。
func TestConflictAfterBaseChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "允许多个目标分支")
	h.targetBranch("dev")
	h.conflictPRBase(7, "kiln/dev-3", "dev")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "dev", WorkItem: 3, State: "pr_open"})
	base := h.svc.forge
	h.svc.forge = func(r *store.Repo) forge.Repo { return rebaseForge{Repo: base(r), h: h} }

	h.process()
	e := h.entry(7)
	if e.State != StateQueued || e.BaseRef != "main" {
		t.Fatalf("应按改投后的目标分支重新评估：%+v", e)
	}
	if got := h.dev.backs(); len(got) != 0 {
		t.Fatalf("改投目标分支后不应交回 AI 开发：%+v", got)
	}
	p := h.pull(7)
	if len(p.Comments) != 0 || !slices.Contains(p.Labels, Label) {
		t.Fatalf("改投目标分支后不应出队：评论 %v，label %v", p.Comments, p.Labels)
	}
	if !strings.Contains(e.Status, "等待 Kiln 按 main 规划") {
		t.Fatalf("状态：%q", e.Status)
	}
}

// TestUpdatedFromBaseIsolation（PLAN #28.5）：审计沿用（UpdatedFrom）只在目标分支一致时返回队列更新前的 head；
// 升级前的旧行（base_ref 为空）照常返回。
func TestUpdatedFromBaseIsolation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, e := range []struct {
		pr   int
		head string
		base string
	}{
		{7, "new7", "dev"},
		{8, "new8", ""},
	} {
		if _, err := h.svc.Enqueue(h.ctx, repoID, e.pr, e.head, e.base, "github:"+testAccount); err != nil {
			t.Fatal(err)
		}
		h.exec(`UPDATE merge_queue SET action='updating', action_head=$2 WHERE repo_id=$1 AND pr_number=$3`, repoID, "old", e.pr)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, "new7", "dev"); err != nil || from != "old" {
		t.Fatalf("目标分支一致时应返回更新前的 head：%q（%v）", from, err)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, "new7", "main"); err != nil || from != "" {
		t.Fatalf("目标分支不同时不应沿用：%q（%v）", from, err)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 8, "new8", "main"); err != nil || from != "old" {
		t.Fatalf("旧行（base_ref 为空）应照常返回：%q（%v）", from, err)
	}
}

// openAutoPRBase 打开一个没有 kiln:merge 的 PR（目标分支 base，等待自动入队）。
func (h *harness) openAutoPRBase(n int, branch, head, base string) {
	h.t.Helper()
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: n, Title: "PR " + branch, HeadRef: branch, HeadSHA: head, BaseRef: base})
}

// autoRunsBase 记下 head 按目标分支 base 规划完成，CI 和 AI 审计的 run 分别处于 ci、ai。
func (h *harness) autoRunsBase(n int, head, base string, ci, ai store.RunState) {
	h.t.Helper()
	h.planned(n, head, base)
	for _, r := range []struct {
		wf    string
		state store.RunState
	}{{"ci.yml", ci}, {mergecfg.AIReview, ai}} {
		h.exec(`INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
			VALUES ($1,$2,$2,'pull_request',$3,$4,'webhook',gen_random_uuid()::text,$5,$6,$7)`,
			repoID, r.wf, workflow.PullRef(n), head, n, string(r.state), fmt.Sprintf(`{"base_ref":%q}`, base))
	}
}

// TestAutoEnqueueAllowedBranches：开启 auto 且 branches 只允许 dev 时，目标是 dev 的人或 Kiln Bot 的 PR 验证通过后
// 自动入队并合入 dev；目标是 main（默认分支）或其他分支的 PR 不自动入队。
func TestAutoEnqueueAllowedBranches(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "auto: true\nrequired: [ci, ai-review]\nbranches: [dev]\ntag: {}\n"}, "开启 auto")
	h.targetBranch("dev")
	devHead := h.branchFrom("feature-dev", "dev")
	h.openAutoPRBase(7, "feature-dev", devHead, "dev")
	h.autoRunsBase(7, devHead, "dev", store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)

	botHead := h.branchFrom("kiln/dev-4", "dev")
	h.openAutoPRBase(9, "kiln/dev-4", botHead, "dev")
	h.fake.UpdatePull(h.repo, 9, func(p *fakegithub.Pull) { p.Author = h.fake.AppSlug + "[bot]" })
	h.autoRunsBase(9, botHead, "dev", store.RunSucceeded, store.RunSucceeded)
	h.nominate(9)

	mainHead := h.branch("feature-main")
	h.openAutoPRBase(8, "feature-main", mainHead, "main")
	h.autoRunsBase(8, mainHead, "main", store.RunSucceeded, store.RunSucceeded)
	h.nominate(8)

	h.targetBranch("other")
	otherHead := h.branchFrom("feature-other", "other")
	h.openAutoPRBase(10, "feature-other", otherHead, "other")
	h.autoRunsBase(10, otherHead, "other", store.RunSucceeded, store.RunSucceeded)
	h.nominate(10)

	h.process()
	e := h.wantState(7, StateMerged, "")
	if e.BaseRef != "dev" || e.EnqueuedBy != AutoActor || !h.pull(7).Merged {
		t.Fatalf("PR #7 应自动合入 dev：%+v", e)
	}
	// PR #9 排在 #7 后面，落后于前进后的 dev：先更新分支，新 head 验证通过、重新提名后再合并
	if e := h.entry(9); e.State != StateQueued || e.EnqueuedBy != AutoActor || e.Action != actionUpdating {
		t.Fatalf("PR #9 应自动入队并更新分支：%+v", e)
	}
	newHead := h.pull(9).HeadSHA
	h.autoRunsBase(9, newHead, "dev", store.RunSucceeded, store.RunSucceeded)
	h.nominate(9)
	h.process()
	e = h.wantState(9, StateMerged, "")
	if e.BaseRef != "dev" || !h.pull(9).Merged {
		t.Fatalf("PR #9 应自动合入 dev：%+v", e)
	}
	if e, _ := Latest(h.ctx, h.db, repoID, 8); e != nil {
		t.Errorf("目标是默认分支 main 的 PR 不应自动入队：%+v", e)
	}
	if e, _ := Latest(h.ctx, h.db, repoID, 10); e != nil {
		t.Errorf("目标是 other 的 PR 不应自动入队：%+v", e)
	}
	var base string
	if err := h.db.QueryRow(h.ctx, `SELECT base_ref FROM auto_tags WHERE repo_id=$1 AND pr_number=7`, repoID).Scan(&base); err != nil || base != "dev" {
		t.Fatalf("自动打 tag 的目标分支：%q（%v）", base, err)
	}
}

// ---- 改投目标分支与单 PR 错误的隔离（PLAN #28.5） ----

// prepareForTest 用真实 prepare 处理仓库的排队项（不推进队列），返回这次处理和要推进的目标分支。
func (h *harness) prepareForTest() (*pass, []string) {
	h.t.Helper()
	conn, err := h.db.Acquire(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(conn.Release)
	repo, err := store.GetRepo(h.ctx, conn, repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	p := &pass{s: h.svc, conn: conn, repo: repo}
	p.f = h.svc.forge(repo)
	entries, err := Queued(h.ctx, conn, repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	groups, err := p.prepare(h.ctx, entries)
	if err != nil {
		h.t.Fatalf("prepare：%v", err)
	}
	return p, groups
}

// TestRetargetRejectsUpdateFromOldBase：PR 在 dev 上发起更新（action_base=dev，action_head=H1），更新产生 H2=(H1, dev)
// 之后 PR 改投回 main：H2 不能被当成 main 的更新接受（否则 dev 的改动混进 main），审计沿用也不能跨分支。真实的
// review.queueReuse 正是用 UpdatedFrom 判断来源，这里断言它的判断依据。
func TestRetargetRejectsUpdateFromOldBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
	h.targetBranch("dev")
	// feature 上先有 H1（按 main 验证、审计过），随后把 dev 合并进来，队列的更新产生了 H2
	h.git("checkout", "-q", "-b", "feature", "main")
	h1 := h.commit(map[string]string{"feature.txt": "x\n"}, "H1")
	h.git("merge", "-q", "--no-ff", "-m", "Merge dev", "dev")
	h2 := h.git("rev-parse", "HEAD")
	h.git("checkout", "-q", "main")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, Title: "PR", HeadRef: "feature", HeadSHA: h2, BaseRef: "main", Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 7, h1, "dev", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE merge_queue SET action='updating', action_head=$2, action_base='dev', base_ref='dev', action_at=now()
		WHERE repo_id=$1 AND pr_number=7`, repoID, h1)

	// 审计沿用不跨分支：main 拿不到 dev 那次更新的旧 head，dev 仍拿得到
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, h2, "main"); err != nil || from != "" {
		t.Fatalf("改投 main 后不应沿用 dev 那次更新：%q（%v）", from, err)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, h2, "dev"); err != nil || from != h1 {
		t.Fatalf("dev 的更新旧 head = %q（%v），想要 %s", from, err, h1)
	}

	h.process()
	h.wantState(7, StateRemoved, "推送了新的 commit")
	if h.pull(7).Merged {
		t.Fatal("旧分支更新产生的 head 不能被接受，更不能合入 main")
	}
}

// TestRetargetAfterAcceptHeadDoesNotReuse：PR 在 dev 上接受了更新产生的 H2（action=none，action_head=H1 保留），之后改投
// main（head 不变）：真实的 prepare 只回填目标分支并清掉更新意图，审计沿用按 action_base 判断，不会跨分支沿用 H1。
func TestRetargetAfterAcceptHeadDoesNotReuse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
	h.targetBranch("dev")
	h1 := h.branch("feature")
	h.git("checkout", "-q", "feature")
	h.git("merge", "-q", "--no-ff", "-m", "Merge dev", "dev")
	h2 := h.git("rev-parse", "HEAD")
	h.git("checkout", "-q", "main")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, Title: "PR", HeadRef: "feature", HeadSHA: h2, BaseRef: "dev", Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 7, h2, "dev", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}
	// acceptHead 之后：head 是 H2，action 回到 none，action_head 保留 H1，action_base 仍是那次更新的 dev
	h.exec(`UPDATE merge_queue SET action='none', action_head=$2, action_base='dev' WHERE repo_id=$1 AND pr_number=7`, repoID, h1)
	// PR 改投 main，head 不变
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.BaseRef = "main" })

	_, groups := h.prepareForTest()
	if !slices.Equal(groups, []string{"main"}) {
		t.Fatalf("分组 = %v，想要 [main]", groups)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, h2, "main"); err != nil || from != "" {
		t.Fatalf("acceptHead 后改投 main 不应沿用 dev 那次更新：%q（%v）", from, err)
	}
	e := h.entry(7)
	if e.BaseRef != "main" || e.Action != actionNone || e.ActionHead != "" || e.ActionBase != "" {
		t.Fatalf("改投后：base=%q action=%q action_head=%q action_base=%q，想要只回填 main 并清掉更新意图",
			e.BaseRef, e.Action, e.ActionHead, e.ActionBase)
	}
}

// TestUpdatedFromPrefersActionBase：升级或并发写留下的行可能 base_ref 已经是新分支（展示用），但更新意图里的
// action_base 还是旧分支；审计沿用必须按 action_base 判断，否则 main 会沿用 dev 那次更新的旧 head。
func TestUpdatedFromPrefersActionBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h1 := h.branch("feature")
	h2 := h.branchFrom("feature-2", h1)
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, Title: "PR", HeadRef: "feature-2", HeadSHA: h2, BaseRef: "main", Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 7, h2, "main", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}
	// 展示用的 base_ref 已换成 main，但这次更新是在 dev 上发起的
	h.exec(`UPDATE merge_queue SET action='none', action_head=$2, action_base='dev', base_ref='main' WHERE repo_id=$1 AND pr_number=7`, repoID, h1)

	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, h2, "main"); err != nil || from != "" {
		t.Fatalf("main 不应沿用 dev 那次更新：%q（%v）", from, err)
	}
	if from, err := UpdatedFrom(h.ctx, h.db, repoID, 7, h2, "dev"); err != nil || from != h1 {
		t.Fatalf("dev 应沿用旧 head %s：%q（%v）", h1, from, err)
	}
}

// TestLegacyBaseRefBackfillKeepsUpdating：升级前的旧行（base_ref 为空、action=updating）第一次读到目标分支时只回填
// base_ref，不能当成改投而清空更新意图（否则会重复请求更新分支）。
func TestLegacyBaseRefBackfillKeepsUpdating(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, Title: "PR", HeadRef: "feature", HeadSHA: head, BaseRef: "main", Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 7, head, "", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE merge_queue SET action='updating', action_head=$2 WHERE repo_id=$1 AND pr_number=7`, repoID, head)

	if _, groups := h.prepareForTest(); !slices.Equal(groups, []string{"main"}) {
		t.Fatalf("分组 = %v，想要 [main]", groups)
	}
	e := h.entry(7)
	if e.BaseRef != "main" || e.Action != actionUpdating || e.ActionHead != head || e.ActionBase != "" {
		t.Fatalf("旧行回填后：base=%q action=%q action_head=%q action_base=%q，想要只回填 base_ref、保留更新意图",
			e.BaseRef, e.Action, e.ActionHead, e.ActionBase)
	}
}

// retargetPull 在第 n 次读取 PR pr 之前执行 swap（模拟处理期间用户在平台上改了 PR 的目标分支）。
type retargetPull struct {
	forge.Repo
	pr   int
	n    *int
	swap func()
}

func (r retargetPull) GetPull(ctx context.Context, number int) (*forge.Pull, error) {
	if number == r.pr {
		*r.n++
		if *r.n == 2 {
			r.swap()
		}
	}
	return r.Repo.GetPull(ctx, number)
}

// TestRetargetMidAdvanceDoesNotJumpQueue：dev 的队首 B 在推进期间改投 main 时，绝不能越过 main 已有的队首 A 直接合入；
// 原 dev 组的下一项 C 照常推进，不被饿死。
func TestRetargetMidAdvanceDoesNotJumpQueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
	h.targetBranch("dev")
	// main 的队首 A 还在等 CI
	aHead := h.branch("feature-a")
	h.openPRBase(1, "feature-a", aHead, "main")
	h.verifiedBase(1, aHead, "main", store.RunActive)
	// dev 的队首 B（head 已按 main 验证通过）和排在后面的 C
	bHead := h.branchFrom("feature-b", "main")
	h.openPRBase(2, "feature-b", bHead, "dev")
	h.verifiedBase(2, bHead, "main", store.RunSucceeded)
	cHead := h.branchFrom("feature-c", "dev")
	h.openPRBase(3, "feature-c", cHead, "dev")
	h.verifiedBase(3, cHead, "dev", store.RunSucceeded)

	base := h.svc.forge
	n := 0
	h.svc.forge = func(r *store.Repo) forge.Repo {
		return retargetPull{Repo: base(r), pr: 2, n: &n, swap: func() {
			h.fake.UpdatePull(h.repo, 2, func(p *fakegithub.Pull) { p.BaseRef = "main" })
		}}
	}
	h.process()

	if h.pull(2).Merged {
		t.Fatal("推进期间改投 main 的 PR 不能越过 main 的队首 A 直接合入")
	}
	if e := h.entry(1); e.State != StateQueued {
		t.Fatalf("main 的队首 A 应继续等待 CI：%+v", e)
	}
	if e := h.entry(2); e.State != StateQueued || e.BaseRef != "main" {
		t.Fatalf("改投后的 B 应回到 main 组排队：%+v", e)
	}
	// 原 dev 组的下一项照常推进，不被改投的队首饿死
	e := h.wantState(3, StateMerged, "")
	if h.git("rev-parse", "dev") != e.MergeSHA {
		t.Fatalf("dev 应该前进到 PR #3 的合并提交：%s", e.MergeSHA)
	}
}

// TestPrepareIsolatesPullErrors：读取一个 PR 暂时失败（500）只影响它所在的目标分支，其他分支照常推进；失败的项让同分支
// 后面的 PR 不越过队首，错误返回给 outbox 稍后重试。
func TestPrepareIsolatesPullErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
	h.targetBranch("dev")
	d1 := h.branchFrom("feature-d1", "dev")
	h.openPRBase(1, "feature-d1", d1, "dev")
	h.verifiedBase(1, d1, "dev", store.RunSucceeded)
	d3 := h.branchFrom("feature-d3", "dev")
	h.openPRBase(3, "feature-d3", d3, "dev")
	h.verifiedBase(3, d3, "dev", store.RunSucceeded)
	mainHead := h.branch("feature-main")
	h.openPR(2, "feature-main", mainHead)
	h.verified(2, mainHead, store.RunSucceeded)

	h.fake.SetFault("GET /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/1", fakegithub.Fault{Status: http.StatusInternalServerError})
	err := h.svc.Process(h.ctx, repoID)
	if err == nil || !strings.Contains(err.Error(), "读取 PR #1") {
		t.Fatalf("Process 应返回 dev 队首的错误供重试：%v", err)
	}
	// 健康的 main 分支照常合并
	h.wantState(2, StateMerged, "")
	// dev 的第二项不能越过读取失败的队首
	if e := h.entry(3); e.State != StateQueued || h.pull(3).Merged {
		t.Fatalf("dev 的第二项不能越过失败的队首：%+v", e)
	}
	if e := h.entry(1); e.State != StateQueued {
		t.Fatalf("读取失败的项应继续排队：%+v", e)
	}
}

// TestUnknownBaseFailureBlocksLater（审计）：升级前的旧行（base_ref 为空）读取失败时无法判断它属于哪条目标
// 分支；后面入队的 PR 绝不能越过它直接合并。本轮准备直接中止并返回错误，交给 outbox 稍后重试。
func TestUnknownBaseFailureBlocksLater(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	legacyHead := h.branch("feature-legacy")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 1, Title: "旧行", HeadRef: "feature-legacy", HeadSHA: legacyHead, BaseRef: "main", Labels: []string{Label}})
	// 旧行 PR #1：base_ref 为空（升级前写入、还没回填目标分支）
	if _, err := h.svc.Enqueue(h.ctx, repoID, 1, legacyHead, "", "github:"+testAccount); err != nil {
		t.Fatal(err)
	}
	// 后入队的 PR #2：目标 main，验证全绿
	newHead := h.branch("feature-new")
	h.openPRBase(2, "feature-new", newHead, "main")
	h.verifiedBase(2, newHead, "main", store.RunSucceeded)
	// 旧行读取失败：无法判断它属于哪条目标分支
	h.fake.SetFault("GET /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/1", fakegithub.Fault{Status: http.StatusInternalServerError})

	err := h.svc.Process(h.ctx, repoID)
	if err == nil || !strings.Contains(err.Error(), "读取 PR #1") {
		t.Fatalf("Process 应返回旧行的错误供重试：%v", err)
	}
	if h.pull(2).Merged {
		t.Fatal("后入队的 PR 不能越过目标分支未知的旧行直接合并")
	}
	if e := h.entry(2); e.State != StateQueued {
		t.Fatalf("PR #2 应保持排队：%+v", e)
	}
	if e := h.entry(1); e.State != StateQueued {
		t.Fatalf("旧行应保持排队：%+v", e)
	}
}

// TestAuthErrorsAbortPass（审计）：GetPull 返回原始的 GitHub 401/403（仓库级鉴权失败、非限流）时整轮中止，
// 不能把仓库不可访问当成普通的单 PR 错误而继续合并其他分支。
func TestAuthErrorsAbortPass(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(h *harness){
		"401": func(h *harness) {
			// 客户端遇到 401 会换 token 重试一次，两次都失败才算鉴权失败
			h.fake.SetFault("GET /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/1", fakegithub.Fault{Status: http.StatusUnauthorized, Times: 2})
		},
		"403": func(h *harness) {
			h.fake.SetFault("GET /repos/"+h.repo.Owner+"/"+h.repo.Name+"/pulls/1", fakegithub.Fault{Status: http.StatusForbidden})
		},
		"限流": func(h *harness) {
			h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: time.Now().Add(time.Hour)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
			h.targetBranch("dev")
			devHead := h.branchFrom("feature-dev", "dev")
			h.openPRBase(1, "feature-dev", devHead, "dev")
			h.verifiedBase(1, devHead, "dev", store.RunSucceeded)
			mainHead := h.branch("feature-main")
			h.openPR(2, "feature-main", mainHead)
			h.verified(2, mainHead, store.RunSucceeded)
			prepare(h)

			if err := h.svc.Process(h.ctx, repoID); err == nil {
				t.Fatal("仓库级鉴权失败/限流时应返回错误")
			}
			// 健康的 main 分支也不能在这一轮合并：整轮交给 outbox 稍后重试
			if h.pull(2).Merged {
				t.Fatal("仓库级鉴权失败/限流时应中止整轮，不能继续合并其他分支")
			}
			if e := h.entry(2); e.State != StateQueued {
				t.Fatalf("PR #2 应保持排队：%+v", e)
			}
		})
	}
}

// TestRemoveLabelFailureIsolatesBranch（审计）：不属于允许目标分支的 PR 在出队时去掉 label 失败（单个 PR 的
// 500），不能拖垮其他健康的分支：main 分支照常合并，错误返回给 outbox 稍后重试。
func TestRemoveLabelFailureIsolatesBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.targetBranch("dev")
	devHead := h.branchFrom("feature-dev", "dev")
	h.openPRBase(7, "feature-dev", devHead, "dev")
	mainHead := h.branch("feature-main")
	h.openPR(8, "feature-main", mainHead)
	h.verified(8, mainHead, store.RunSucceeded)
	h.fake.SetFault("DELETE /repos/"+h.repo.Owner+"/"+h.repo.Name+"/issues/7/labels/"+Label, fakegithub.Fault{Status: http.StatusInternalServerError})

	err := h.svc.Process(h.ctx, repoID)
	if err == nil || !strings.Contains(err.Error(), "PR #7") {
		t.Fatalf("Process 应返回出队失败的错误供重试：%v", err)
	}
	// 健康的 main 分支照常合并
	h.wantState(8, StateMerged, "")
	// 出队失败的 PR 留在队列里，下一轮重试
	if e := h.entry(7); e.State != StateQueued {
		t.Fatalf("出队失败的 PR 应继续排队：%+v", e)
	}
	if !slices.Contains(h.pull(7).Labels, Label) {
		t.Fatal("出队失败时不应去掉 label")
	}
}

// TestCommentForbiddenAbortsPass（审计）：出队时在 PR 上评论返回非限流的 403，是仓库级的鉴权失败（不是单个
// PR 的拒绝）：RejectedError 必须保留原始错误，让 IsAuthError 认出它并中止整轮，不能继续推进健康的目标分支。
func TestCommentForbiddenAbortsPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": "branches: [main, dev]\n"}, "config")
	h.targetBranch("dev")
	h.targetBranch("other")
	otherHead := h.branch("feature-other")
	h.openPRBase(7, "feature-other", otherHead, "other") // 不在允许的目标分支里，prepare 第二阶段出队
	mainHead := h.branch("feature-main")
	h.openPR(8, "feature-main", mainHead)
	h.verified(8, mainHead, store.RunSucceeded)
	h.fake.SetFault("POST /repos/"+h.repo.Owner+"/"+h.repo.Name+"/issues/7/comments", fakegithub.Fault{Status: http.StatusForbidden})

	err := h.svc.Process(h.ctx, repoID)
	if err == nil || !strings.Contains(err.Error(), "PR #7") {
		t.Fatalf("Process 应返回评论失败的错误：%v", err)
	}
	// 仓库级鉴权失败：整轮中止，健康的 main 分支这一轮也不能合并
	if h.pull(8).Merged {
		t.Fatal("评论返回 403 时应中止整轮，不能继续推进健康的分支")
	}
	if e := h.entry(8); e.State != StateQueued {
		t.Fatalf("PR #8 应保持排队：%+v", e)
	}
	if e := h.entry(7); e.State != StateQueued {
		t.Fatalf("出队失败的 PR #7 应保持排队：%+v", e)
	}
}

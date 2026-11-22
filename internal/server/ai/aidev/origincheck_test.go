package aidev

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// PR 创建归属的待核验记录：创建 PR 的响应丢失、PR 先被合并、合并事件先到时，合并事件按正式分支找到工作项，
// 在推进它之前冻结一条待核验记录；仓库执行者经平台核验后补记永久归属（绝不按分支直接授权）。核验前自动打 tag 不放行。

// originCheckPlanner 记录自动打 tag 的发布规划。
type originCheckPlanner struct {
	mu   sync.Mutex
	reqs []autotag.TagPush
}

func (p *originCheckPlanner) PlanTagPush(_ context.Context, ev autotag.TagPush) ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, ev)
	return []int64{int64(len(p.reqs))}, nil
}

func (p *originCheckPlanner) requests() []autotag.TagPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]autotag.TagPush(nil), p.reqs...)
}

// originCheckResolver 是 autotag.OriginResolver 的适配：用真实的 aidev.ResolveAutoTagOrigin，类别逐个映射（与组装层相同）。
type originCheckResolver struct{}

func originCheckKind(k AutoTagOriginKind) autotag.OriginKind {
	switch k {
	case AutoTagOriginImplementation:
		return autotag.OriginImplementation
	case AutoTagOriginDesign:
		return autotag.OriginDesign
	case AutoTagOriginRecording:
		return autotag.OriginRecording
	case AutoTagOriginDenied:
		return autotag.OriginDenied
	}
	return autotag.OriginUnknown
}

func (originCheckResolver) Resolve(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (autotag.DevOrigin, error) {
	o, err := ResolveAutoTagOrigin(ctx, q, repoID, pr, headRef, baseRef)
	if err != nil {
		return autotag.DevOrigin{}, err
	}
	return devOriginOf(o), nil
}

// devOriginOf 把 aidev 的来源类别映射成自动打 tag 的来源（与组装层相同）。
func devOriginOf(o AutoTagOrigin) autotag.DevOrigin {
	return autotag.DevOrigin{
		Kind: originCheckKind(o.Kind), WorkItemID: o.WorkItemID, Phase: o.Phase, ImplRound: o.ImplRound, BaseRef: o.BaseRef,
		CreatorLogin: o.CreatorLogin, DeliveryTaskID: o.DeliveryTaskID, Issues: o.Issues, Reason: o.Reason,
	}
}

// newAutoTag 返回用这个测试的数据库和模拟 GitHub 的自动打 tag 服务，来源判定用真实的 ResolveAutoTagOrigin。
func (h *harness) newAutoTag() (*autotag.Service, *originCheckPlanner) {
	return h.newAutoTagWithResolver(originCheckResolver{})
}

// newAutoTagWithResolver 与 newAutoTag 相同，来源判定换成 r。
func (h *harness) newAutoTagWithResolver(r autotag.OriginResolver) (*autotag.Service, *originCheckPlanner) {
	h.t.Helper()
	pl := &originCheckPlanner{}
	s := autotag.New(autotag.Deps{
		DB: h.db, Forge: autotag.ForgeFunc(h.newForge()), Planner: pl, Outbox: outbox.NewWriter(true),
		Log: h.svc.log, DevOrigin: r,
	})
	return s, pl
}

// resolveOrigin 用真实的 ResolveAutoTagOrigin 判断 PR 的来源。
func (h *harness) resolveOrigin(pr int, head, base string) AutoTagOrigin {
	h.t.Helper()
	o, err := ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, pr, head, base)
	if err != nil {
		h.t.Fatal(err)
	}
	return o
}

// autoTagState 返回记录的状态、tag 和原因。
func (h *harness) autoTagState(sha string) (state, tag, reason string) {
	h.t.Helper()
	if err := h.db.QueryRow(h.ctx, `SELECT state, tag, reason FROM auto_tags WHERE repo_id=$1 AND sha=$2`, testRepoID, sha).
		Scan(&state, &tag, &reason); err != nil {
		h.t.Fatal(err)
	}
	return state, tag, reason
}

// lostPull 模拟创建 PR 的响应丢失（PR 已经建好、编号没有记下），接着把这个 PR 合并，返回 PR 编号、合并得到的 commit 和
// 被合并 PR 的 head commit（交付的 commit）。合并事件不投递。
func (h *harness) lostPull(w *WorkItem) (pr int, mergeSHA, head string) {
	h.t.Helper()
	head = h.workItem(w.ID).DeliverySHA
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		h.t.Fatal("创建 PR 的响应丢失时应返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	// Issue #1 之后的第一个编号是 PR #2
	pr = 2
	mergeSHA, err := h.fake.MergePull(h.repo, pr, "squash")
	if err != nil {
		h.t.Fatal(err)
	}
	return pr, mergeSHA, head
}

// lostPullAndMerged 与 lostPull 相同，再投递先到的合并事件。
func (h *harness) lostPullAndMerged(w *WorkItem) (pr int, mergeSHA, head string) {
	h.t.Helper()
	pr, mergeSHA, head = h.lostPull(w)
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID,
		ClosedPull{Number: pr, HeadRef: w.Branch(), HeadSHA: head, Merged: true, MergeSHA: mergeSHA}, "bob"); err != nil {
		h.t.Fatal(err)
	}
	return pr, mergeSHA, head
}

// TestOriginCheckVerifiedAfterMerge 正向：本 App 创建 PR 的响应丢失、PR 先被合并、合并事件先到 —— 先把冻结的待核验记录
// 写下来、自动打 tag 在核验前不放行（Recording）；执行者经平台核验后补记永久归属、清理记录，Retry 之后正常打 tag 并规划发布。
func TestOriginCheckVerifiedAfterMerge(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{".kiln/merge.yml": "tag: {}\n"}, "开启 tag")
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, mergeSHA, _ := h.lostPullAndMerged(w)
	h.wantState(w.ID, StateMerged)

	// 推进工作项换轮、结束之前冻结的待核验记录还在，永久归属还没有
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 1 {
		t.Fatalf("待核验记录 %d 条，想要 1 条", n)
	}
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("核验前不应写永久归属")
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind != AutoTagOriginRecording || o.WorkItemID != w.ID {
		t.Fatalf("核验前来源 = %+v，想要归属正在核验", o)
	}

	// 自动打 tag：核验前跳过，不放行
	tagSvc, planner := h.newAutoTag()
	if _, err := tagSvc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: testRepoID, SHA: mergeSHA, PR: pr, MergedBy: "kiln-test[bot]", SenderIsBot: true,
		HeadRef: w.Branch(), BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if state, _, reason := h.autoTagState(mergeSHA); state != autotag.StateSkipped || !strings.Contains(reason, "正在核验") {
		t.Fatalf("核验前应跳过并写明原因：state=%q reason=%q", state, reason)
	}
	if len(planner.requests()) != 0 || h.git("tag", "--list") != "" {
		t.Fatal("核验前不应创建 tag 或规划发布")
	}

	// 执行者核验：通过后补记永久归属并清理待核验记录（即使工作项已经结束）
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 0 {
		t.Fatalf("核验之后待核验记录应清理：%d 条", n)
	}
	id, phase, round, head, base, _, ok := h.originRow(pr)
	if !ok || id != w.ID || phase != PhaseImplement || round != w.ImplRound || head != w.Branch() || base != "main" {
		t.Fatalf("补记的归属 = %d %s %d %q %q（%v）", id, phase, round, head, base, ok)
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation || o.WorkItemID != w.ID {
		t.Fatalf("核验后来源 = %+v，想要可信的实现 PR", o)
	}

	// 补记之后再重试：正常打 tag 并规划发布
	if err := tagSvc.Retry(h.ctx, testRepoID, mergeSHA, testAccount); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if state, tag, reason := h.autoTagState(mergeSHA); state != autotag.StatePlanned || tag != "v0.1.0" {
		t.Fatalf("补记之后应打 tag 并规划发布：state=%q tag=%q reason=%q", state, tag, reason)
	}
	if reqs := planner.requests(); len(reqs) != 1 || !reqs[0].SenderIsBot {
		t.Fatalf("应规划一次发布、保留机器人发送者：%+v", reqs)
	}
}

// TestOriginCheckRejectsThirdParty 负向：第三方在正式分支上建 PR 并先合入 —— 合并事件同样冻结待核验记录；执行者核验作者
// 不通过，清理记录、绝不写永久归属，自动打 tag（含 Retry）持续拒绝。
func TestOriginCheckRejectsThirdParty(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{".kiln/merge.yml": "tag: {}\n"}, "开启 tag")
	h.issue(1, "一", "a")
	// 交付记下了、推进还没成功（创建 PR 被平台拒绝），工作项停在 delivering、编号还没记下
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"a.go": "package a\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Status: 422})
	if err := h.process(); err == nil {
		t.Fatal("创建 PR 被平台拒绝时应返回错误")
	}
	w := h.wantState(res.WorkItemID, StateDelivering)
	// 有人用同一个正式分支开了 PR（编号 #2）并先合入
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 2, Title: "第三方", HeadRef: w.Branch(), HeadSHA: sha, BaseRef: "main", Author: "carol"})
	mergeSHA, err := h.fake.MergePull(h.repo, 2, "squash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID,
		ClosedPull{Number: 2, HeadRef: w.Branch(), HeadSHA: sha, Merged: true, MergeSHA: mergeSHA}, "bob"); err != nil {
		t.Fatal(err)
	}
	h.wantState(w.ID, StateMerged)

	tagSvc, planner := h.newAutoTag()
	if _, err := tagSvc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: testRepoID, SHA: mergeSHA, PR: 2, MergedBy: "kiln-test[bot]", SenderIsBot: true,
		HeadRef: w.Branch(), BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if state, _, reason := h.autoTagState(mergeSHA); state != autotag.StateSkipped || !strings.Contains(reason, "正在核验") {
		t.Fatalf("核验前应跳过：state=%q reason=%q", state, reason)
	}

	// 执行者核验作者不通过：清理记录，绝不写永久归属
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, 2); n != 0 {
		t.Fatalf("核验不通过时应清理待核验记录：%d 条", n)
	}
	if _, _, _, _, _, _, ok := h.originRow(2); ok {
		t.Fatal("第三方的 PR 不应写永久归属")
	}
	if o := h.resolveOrigin(2, w.Branch(), "main"); o.Kind == AutoTagOriginImplementation || o.Kind == AutoTagOriginRecording {
		t.Fatalf("第三方 PR 不应放行：%+v", o)
	}
	// 之后（含 Retry）持续拒绝
	for range 2 {
		if err := tagSvc.Retry(h.ctx, testRepoID, mergeSHA, testAccount); err != nil {
			t.Fatal(err)
		}
		if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
			t.Fatalf("自动打 tag：%v", err)
		}
		if state, _, reason := h.autoTagState(mergeSHA); state != autotag.StateSkipped || !strings.Contains(reason, "机器人") {
			t.Fatalf("第三方 PR 应持续拒绝：state=%q reason=%q", state, reason)
		}
	}
	if len(planner.requests()) != 0 || h.git("tag", "--list") != "" {
		t.Fatal("不应创建 tag 或规划发布")
	}
}

// TestFixDoesNotPromoteLegacyThirdPartyPull：升级前已经记下 pr_number、却没有永久归属的 PR 被第三方 Bot 占着
// 时，完成一次返修不会把它提升为可信的发布来源：返修照常推进代码，但不写 dev_pull_origins，ResolveAutoTagOrigin 不放行。
func TestFixDoesNotPromoteLegacyThirdPartyPull(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	head := w.HeadSHA
	// 模拟升级前的状态：归属表里没有这条记录，平台上的 PR 作者是第三方 Bot
	h.exec(`DELETE FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)
	h.fake.UpdatePull(h.repo, pr, func(p *fakegithub.Pull) { p.Author = "other[bot]" })

	// 一轮返修：返修交付推进同一个 PR
	review := h.failingRun(pr, head)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	aid := h.acquire(h.fixTask(w).ID)
	fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, FindingID(review, 0)), Branch: devreport.AttemptBranch(aid), SHA: fix})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)

	// 返修推进了代码，但绝不把它记成可信来源
	if h.workItem(w.ID).HeadSHA != fix {
		t.Fatalf("返修应推进 PR 的 head：%s", h.workItem(w.ID).HeadSHA)
	}
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("升级前收养的第三方 PR 不应因为一次返修被记为可信来源")
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind == AutoTagOriginImplementation || o.Kind == AutoTagOriginRecording {
		t.Fatalf("第三方 Bot 的 PR 不应放行：%+v", o)
	}
}

// 保证这个文件里的适配器确实实现自动打 tag 的来源判定接口。
var _ autotag.OriginResolver = originCheckResolver{}

// TestClosedBeforeRecordedVerified：编号还没有记下、PR 就已经合并（合并事件没到或者先到），执行者按平台上的 PR 发现它已经
// 合并时核验通过就记下永久归属，工作项转为 merged。
func TestClosedBeforeRecordedVerified(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, _, _ := h.lostPull(w)
	// 合并事件还没到：按平台上的 PR 发现已经合并
	h.mustProcess()
	w = h.wantState(w.ID, StateMerged)
	id, phase, round, head, base, creator, ok := h.originRow(pr)
	if !ok || id != w.ID || phase != PhaseImplement || round != w.ImplRound || base != "main" || creator == "" {
		t.Fatalf("归属 = %d %s %d %q %q %q（%v）", id, phase, round, head, base, creator, ok)
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation || o.WorkItemID != w.ID {
		t.Fatalf("来源 = %+v，想要可信的实现 PR", o)
	}
}

// TestClosedBeforeRecordedThirdParty：同样的情形，但正式分支上的 PR 是第三方开的：核验不通过，工作项照样结束，但绝不写
// 永久归属。
func TestClosedBeforeRecordedThirdParty(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	aid := h.acquire(h.taskOf(h.startSelect()).ID)
	res := h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"a.go": "package a\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Status: 422})
	if err := h.process(); err == nil {
		t.Fatal("创建 PR 被平台拒绝时应返回错误")
	}
	w := h.wantState(res.WorkItemID, StateDelivering)
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 2, Title: "第三方", HeadRef: w.Branch(), HeadSHA: sha, BaseRef: "main", Author: "carol"})
	if _, err := h.fake.MergePull(h.repo, 2, "squash"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	w = h.wantState(w.ID, StateMerged)
	if _, _, _, _, _, _, ok := h.originRow(2); ok {
		t.Fatal("第三方的 PR 不应写永久归属")
	}
	if o := h.resolveOrigin(2, w.Branch(), "main"); o.Kind == AutoTagOriginImplementation || o.Kind == AutoTagOriginRecording {
		t.Fatalf("第三方 PR 不应放行：%+v", o)
	}
}

// TestDesignOriginCheckNotReleased：设计阶段的 delivering 工作项创建 PR 的响应丢失、PR 被人类合并后，合并事件
// 冻结一条设计阶段的待核验记录，工作项进入实现并记下 design_pr。在 aidev 执行者核验之前，自动打 tag 必须认出这是设计 PR
// ——绝不能把已知的设计身份遮成"归属正在核验"、更不能因为人类合并就放行打 tag。执行者补记设计归属之后，Retry 仍然跳过。
func TestDesignOriginCheckNotReleased(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{".kiln/merge.yml": "tag: {}\n"}, "开启 tag")
	h.issue(1, "升级历史", "显示升级历史")

	// 领取设计、交付设计（停在 delivering），创建 PR 的响应丢失
	res, aid := h.claimDesign(1)
	h.deliverOnly(aid, map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w := h.wantState(res.WorkItemID, StateDelivering)
	designBranch, head := res.Branch, w.DeliverySHA
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("创建设计 PR 的响应丢失时应返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	pr := 2 // Issue #1 之后的第一个 PR
	mergeSHA, err := h.fake.MergePull(h.repo, pr, "squash")
	if err != nil {
		t.Fatal(err)
	}
	// 人类合并设计 PR（SenderIsBot=false）：冻结阶段为设计的待核验记录，工作项进入实现、记下 design_pr
	if _, err := h.svc.HandlePullClosed(h.ctx, testRepoID,
		ClosedPull{Number: pr, HeadRef: designBranch, HeadSHA: head, Merged: true, MergeSHA: mergeSHA}, "bob"); err != nil {
		t.Fatal(err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2 AND phase=$3`, testRepoID, pr, PhaseDesign); n != 1 {
		t.Fatalf("设计阶段的待核验记录 %d 条，想要 1 条", n)
	}
	w = h.wantState(w.ID, StateDeveloping)
	if w.Phase != PhaseImplement || w.DesignPR == nil || *w.DesignPR != pr {
		t.Fatalf("设计合并后 = %s", jsonString(w))
	}
	// 归属还没有永久记录（aidev 执行者还没核验）
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("核验前不应写永久归属")
	}

	tagSvc, planner := h.newAutoTag()
	if _, err := tagSvc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: testRepoID, SHA: mergeSHA, PR: pr, MergedBy: "bob", SenderIsBot: false,
		HeadRef: designBranch, BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	process := func() {
		if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
			t.Fatalf("自动打 tag：%v", err)
		}
	}
	assertSkipped := func(when string) {
		if state, _, reason := h.autoTagState(mergeSHA); state != autotag.StateSkipped || !strings.Contains(reason, "设计 PR") {
			t.Fatalf("%s 应作为设计 PR 跳过：state=%q reason=%q", when, state, reason)
		}
		if h.git("tag", "--list") != "" || len(planner.requests()) != 0 {
			t.Fatalf("%s 不应创建 tag 或规划发布", when)
		}
		var workItem *int64
		if err := h.db.QueryRow(h.ctx, `SELECT work_item_id FROM auto_tags WHERE repo_id=$1 AND sha=$2`, testRepoID, mergeSHA).Scan(&workItem); err != nil {
			t.Fatal(err)
		}
		if workItem != nil {
			t.Fatalf("%s 不应写可信工作项列：%d", when, *workItem)
		}
	}
	process()
	assertSkipped("核验前（人类合并）")

	// 执行者核验设计归属：补记 dev_pull_origins(phase='design')、清理待核验记录
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 0 {
		t.Fatalf("核验之后待核验记录应清理：%d 条", n)
	}
	if id, phase, _, _, _, _, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseDesign {
		t.Fatalf("补记的设计归属 = %d %s（%v），想要工作项 #%d、%s", id, phase, ok, w.ID, PhaseDesign)
	}
	// 补记之后再重试，仍然设计 PR，继续跳过
	if err := tagSvc.Retry(h.ctx, testRepoID, mergeSHA, testAccount); err != nil {
		t.Fatal(err)
	}
	process()
	assertSkipped("核验后 Retry")
}

// TestImplementationOriginCheckHumanMergeNotReleased：实现阶段的 delivering 工作项创建 PR 的响应丢失、PR 被人类
// 合并后，自动打 tag 在归属核验前先跳过（等核验），不会因为"人类合并"就放行；执行者核验补记之后 Retry 正常打 tag 发布。
func TestImplementationOriginCheckHumanMergeNotReleased(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{".kiln/merge.yml": "tag: {}\n"}, "开启 tag")
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, mergeSHA, _ := h.lostPullAndMerged(w) // 人类（bob）合并
	h.wantState(w.ID, StateMerged)
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 1 {
		t.Fatalf("待核验记录 %d 条，想要 1 条", n)
	}

	// 核验之前：人类合并也必须先跳过，等核验
	tagSvc, planner := h.newAutoTag()
	if _, err := tagSvc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: testRepoID, SHA: mergeSHA, PR: pr, MergedBy: "bob", SenderIsBot: false,
		HeadRef: w.Branch(), BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if state, _, reason := h.autoTagState(mergeSHA); state != autotag.StateSkipped || !strings.Contains(reason, "正在核验") {
		t.Fatalf("核验前应跳过等核验：state=%q reason=%q", state, reason)
	}
	if h.git("tag", "--list") != "" || len(planner.requests()) != 0 {
		t.Fatal("核验前不应创建 tag 或规划发布")
	}

	// 执行者核验：补记永久归属、清理待核验记录
	h.mustProcess()
	if id, phase, _, _, _, _, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseImplement {
		t.Fatalf("补记的实现归属 = %d %s（%v），想要工作项 #%d、%s", id, phase, ok, w.ID, PhaseImplement)
	}
	// 补记之后再重试：人类合并的可信实现 PR 正常打 tag 并发布
	if err := tagSvc.Retry(h.ctx, testRepoID, mergeSHA, testAccount); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if state, tag, reason := h.autoTagState(mergeSHA); state != autotag.StatePlanned || tag != "v0.1.0" {
		t.Fatalf("核验后应打 tag 并规划发布：state=%q tag=%q reason=%q", state, tag, reason)
	}
	if reqs := planner.requests(); len(reqs) != 1 || reqs[0].SenderIsBot {
		t.Fatalf("应为人类合并规划一次发布：%+v", reqs)
	}
}

// countingQuerier 包装 store.Querier：统计来源判定发起的查询次数（Query / QueryRow），并可以在读到设计事实
// （SQL 含 design_pr）的那次查询 Scan 完成之后执行一次 hook——模拟合并事件的处理在两次查询之间提交。
type countingQuerier struct {
	store.Querier
	queries int
	after   func()
	fired   bool
}

func (q *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.queries++
	rows, err := q.Querier.Query(ctx, sql, args...)
	q.fire(sql)
	return rows, err
}

func (q *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.queries++
	return hookRow{Row: q.Querier.QueryRow(ctx, sql, args...), fire: func() { q.fire(sql) }}
}

func (q *countingQuerier) fire(sql string) {
	if q.fired || q.after == nil || !strings.Contains(sql, "design_pr") {
		return
	}
	q.fired = true
	q.after()
}

// hookRow 在底层查询 Scan 完成（快照已经读到）之后执行一次 fire。
type hookRow struct {
	pgx.Row
	fire func()
}

func (r hookRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.fire()
	return err
}

// snapshotResolver 用真实的 ResolveAutoTagOrigin 判定来源，统计它发起的查询数，并让 hook 在读到设计事实的那次查询之后
// 执行（模拟合并事件的事务在这之间提交）。
type snapshotResolver struct {
	hook    func()
	queries int
	origin  AutoTagOrigin
}

func (r *snapshotResolver) Resolve(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (autotag.DevOrigin, error) {
	cq := &countingQuerier{Querier: q, after: r.hook}
	o, err := ResolveAutoTagOrigin(ctx, cq, repoID, pr, headRef, baseRef)
	r.queries += cq.queries
	r.origin = o
	if err != nil {
		return autotag.DevOrigin{}, err
	}
	return devOriginOf(o), nil
}

// TestResolveAutoTagOriginSingleSnapshot：来源判定只发起一条 SQL 语句，在一次一致的 MVCC 快照上完成。
// 合并事件的处理（真实的 HandlePullClosed，持有另一把仓库锁）在来源判定读到设计事实之后提交时，设计 PR 仍然判为设计 PR
// （绝不是 Unknown），自动打 tag 跳过，零 tag、零发布规划。拆成多条查询时，这条测试会看到前后两个快照拼出的“未知”。
func TestResolveAutoTagOriginSingleSnapshot(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.commitOn("main", map[string]string{".kiln/merge.yml": "tag: {}\n"}, "开启 tag")
	h.issue(1, "升级历史", "显示升级历史")

	// 设计阶段的 delivering 工作项，创建 PR 的响应丢失（PR 已经建好、编号没记下）
	res, aid := h.claimDesign(1)
	h.deliverOnly(aid, map[string]string{designDoc: "# 设计\n"}, designReport([]int{1}, designDoc))
	w := h.wantState(res.WorkItemID, StateDelivering)
	designBranch, head := res.Branch, w.DeliverySHA
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("创建设计 PR 的响应丢失时应返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	pr := 2 // Issue #1 之后的第一个 PR

	// 合并事件的处理与来源判定并发：在判定读到设计事实之后、返回之前才提交
	var mergeErr error
	r := &snapshotResolver{hook: func() {
		mergeSHA, err := h.fake.MergePull(h.repo, pr, "squash")
		if err == nil {
			_, err = h.svc.HandlePullClosed(h.ctx, testRepoID,
				ClosedPull{Number: pr, HeadRef: designBranch, HeadSHA: head, Merged: true, MergeSHA: mergeSHA}, "bob")
		}
		mergeErr = err
	}}
	tagSvc, planner := h.newAutoTagWithResolver(r)
	if _, err := tagSvc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: testRepoID, SHA: head, PR: pr, MergedBy: "bob", SenderIsBot: false,
		HeadRef: designBranch, BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.Process(h.ctx, testRepoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	if mergeErr != nil {
		t.Fatalf("并发提交的合并事件：%v", mergeErr)
	}
	if r.queries != 1 {
		t.Fatalf("来源判定发起了 %d 次查询，想要 1 次（必须在同一个快照上完成）", r.queries)
	}
	if r.origin.Kind != AutoTagOriginDesign && r.origin.Kind != AutoTagOriginRecording {
		t.Fatalf("并发的合并事件提交后的来源 = %+v，想要设计 PR 或归属正在核验（绝不未知）", r.origin)
	}
	if state, _, reason := h.autoTagState(head); state != autotag.StateSkipped {
		t.Fatalf("应跳过：state=%q reason=%q", state, reason)
	}
	if h.git("tag", "--list") != "" || len(planner.requests()) != 0 {
		t.Fatal("不应创建 tag 或规划发布")
	}
	// 合并事件确实已经提交：工作项进入实现、记下了 design_pr
	w = h.wantState(w.ID, StateDeveloping)
	if w.Phase != PhaseImplement || w.DesignPR == nil || *w.DesignPR != pr {
		t.Fatalf("设计合并后 = %s", jsonString(w))
	}
}

// TestCheckOriginKeepsEvidenceWhenIdentityNotReady：App 身份还没加载出来时，执行者不能把“还不知道作者”
// 当成核验不通过、删掉合法的待核验记录：保留记录、返回错误稍后重试；身份恢复后补记归属，来源判定为可信的实现 PR。
func TestCheckOriginKeepsEvidenceWhenIdentityNotReady(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, _, _ := h.lostPullAndMerged(w)
	h.wantState(w.ID, StateMerged)

	h.setSelfReady(false)
	if err := h.process(); !errors.Is(err, errAppIdentityNotReady) {
		t.Fatalf("App 身份未就绪时应返回 errAppIdentityNotReady、稍后重试：%v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 1 {
		t.Fatalf("身份未就绪时待核验记录应保留 1 条：%d 条", n)
	}
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("身份未就绪时不应写永久归属，也不应丢弃记录")
	}

	// 身份恢复：同样的处理补记归属、清理记录，来源判定为可信的实现 PR
	h.setSelfReady(true)
	h.mustProcess()
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 0 {
		t.Fatalf("身份恢复后应清理待核验记录：%d 条", n)
	}
	if id, phase, _, _, _, _, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseImplement {
		t.Fatalf("补记的归属 = %d %s（%v），想要工作项 #%d、%s", id, phase, ok, w.ID, PhaseImplement)
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation || o.WorkItemID != w.ID {
		t.Fatalf("身份恢复后的来源 = %+v，想要可信的实现 PR", o)
	}
}

// TestVerifiedPullRefsNilCheckSelf：没有注入 CheckSelf（身份视为未就绪）时不能把作者判成“不是本 App”：分支、fork 都对得上
// 时返回 errAppIdentityNotReady；fork 或分支对不上与身份无关，直接判否、不报错（避免把“还不知道”误当成拒绝）。
func TestVerifiedPullRefsNilCheckSelf(t *testing.T) {
	pr := &forge.Pull{Number: 2, HeadRef: "kiln/dev-1", BaseRef: "main", Author: "kiln-test[bot]"}
	verified, err := verifiedPullRefs(pr, "kiln/dev-1", "main", nil)
	if verified || !errors.Is(err, errAppIdentityNotReady) {
		t.Fatalf("CheckSelf 为 nil：verified=%v err=%v，想要 false、errAppIdentityNotReady", verified, err)
	}
	if verified, err := verifiedPullRefs(&forge.Pull{Number: 2, HeadRef: "other", BaseRef: "main", Author: "kiln-test[bot]"},
		"kiln/dev-1", "main", nil); verified || err != nil {
		t.Fatalf("分支对不上：verified=%v err=%v，想要 false、nil", verified, err)
	}
	if verified, err := verifiedPullRefs(&forge.Pull{Number: 2, HeadRef: "kiln/dev-1", BaseRef: "main", Author: "kiln-test[bot]", Fork: true},
		"kiln/dev-1", "main", nil); verified || err != nil {
		t.Fatalf("fork：verified=%v err=%v，想要 false、nil", verified, err)
	}
}

// TestNilCheckSelfKeepsOriginCheck：服务没有注入 CheckSelf 时按“身份未就绪”处理：执行者核验待核验记录返回
// errAppIdentityNotReady、保留记录、不写永久归属（稍后重试），绝不把它当成核验不通过删掉证据。
func TestNilCheckSelfKeepsOriginCheck(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, _, _ := h.lostPullAndMerged(w)
	h.wantState(w.ID, StateMerged)

	h.svc.checkSelf = nil
	if err := h.process(); !errors.Is(err, errAppIdentityNotReady) {
		t.Fatalf("CheckSelf 为 nil 时应返回 errAppIdentityNotReady、稍后重试：%v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr); n != 1 {
		t.Fatalf("身份未就绪时应保留待核验记录 1 条：%d 条", n)
	}
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("身份未就绪时不应写永久归属")
	}
}

// TestEnsurePullWaitsForIdentity：正式分支上已经有本 App 打开的 PR、App 身份还没加载出来时，执行者返回错误
// 稍后重试，绝不把工作项误转为受阻；身份恢复后接管 PR、记下归属。
func TestEnsurePullWaitsForIdentity(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	// 创建 PR 的响应丢失：PR 已经建好、打开着，编号没有记下
	h.fake.SetFault("POST /repos/alice/demo/pulls", fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("创建 PR 的响应丢失时应返回错误")
	}
	h.wantState(w.ID, StateDelivering)
	pr := 2

	h.setSelfReady(false)
	if err := h.process(); err == nil {
		t.Fatal("App 身份未就绪时应返回错误、稍后重试")
	}
	got := h.wantState(w.ID, StateDelivering) // 不转 blocked
	if got.PRNumber != nil {
		t.Fatalf("身份未就绪时不应记下 PR：%d", *got.PRNumber)
	}
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("身份未就绪时不应写永久归属")
	}

	h.setSelfReady(true)
	h.mustProcess()
	got = h.wantState(w.ID, StatePROpen)
	if got.PRNumber == nil || *got.PRNumber != pr {
		t.Fatalf("身份恢复后应接管 PR #%d：%+v", pr, got.PRNumber)
	}
	if id, phase, _, _, _, creator, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseImplement || creator == "" {
		t.Fatalf("身份恢复后应补记归属：%d %s %q（%v）", id, phase, creator, ok)
	}
}

// TestExistingPullVerifiedWaitsForIdentity：返修推进一个升级前没有归属的 PR、App 身份还没加载出来时，
// 执行者返回错误稍后重试，不以“未核验”静默跳过归属记录；身份恢复后正常补记。
func TestExistingPullVerifiedWaitsForIdentity(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, pr := h.developed(1)
	head := w.HeadSHA
	// 模拟升级前的状态：归属表里没有这条记录
	h.exec(`DELETE FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)

	review := h.failingRun(pr, head)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	aid := h.acquire(h.fixTask(w).ID)
	fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, FindingID(review, 0)), Branch: devreport.AttemptBranch(aid), SHA: fix})

	h.setSelfReady(false)
	if err := h.process(); err == nil {
		t.Fatal("App 身份未就绪时应返回错误、稍后重试")
	}
	h.wantState(w.ID, StateDelivering) // 仍停在推进中，不转 blocked
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("身份未就绪时不应写永久归属")
	}

	h.setSelfReady(true)
	h.mustProcess()
	h.wantState(w.ID, StatePROpen)
	if id, phase, _, _, _, creator, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseImplement || creator == "" {
		t.Fatalf("身份恢复后应补记归属：%d %s %q（%v）", id, phase, creator, ok)
	}
}

// TestClosedBeforeRecordedWaitsForIdentity：编号还没有记下、PR 已经合并（合并事件没到）、App 身份还没加载
// 出来时，执行者返回错误稍后重试，不丢掉这个已经合并的 PR 的归属；身份恢复后补记。
func TestClosedBeforeRecordedWaitsForIdentity(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	pr, _, _ := h.lostPull(w) // PR 已合并，合并事件还没到

	h.setSelfReady(false)
	if err := h.process(); err == nil {
		t.Fatal("App 身份未就绪时应返回错误、稍后重试")
	}
	h.wantState(w.ID, StateDelivering)
	if _, _, _, _, _, _, ok := h.originRow(pr); ok {
		t.Fatal("身份未就绪时不应写永久归属")
	}

	h.setSelfReady(true)
	h.mustProcess()
	w = h.wantState(w.ID, StateMerged)
	if id, phase, _, _, _, creator, ok := h.originRow(pr); !ok || id != w.ID || phase != PhaseImplement || creator == "" {
		t.Fatalf("身份恢复后应补记归属：%d %s %q（%v）", id, phase, creator, ok)
	}
	if o := h.resolveOrigin(pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation || o.WorkItemID != w.ID {
		t.Fatalf("身份恢复后的来源 = %+v，想要可信的实现 PR", o)
	}
}

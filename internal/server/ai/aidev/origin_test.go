package aidev

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
)

// AI 开发 PR 的永久归属（PLAN #29.4）：Server 创建或核验过的正式 PR 记在 dev_pull_origins；自动打 tag 只按
// 这份记录判断可信来源。返修推进同一个 PR 时只更新交付引用，身份列不变；第三方或伪造的 PR 不会被记为可信来源。

// openedWorkItem 走一轮实现交付并推进，返回 pr_open 的工作项（它的 PR 已经记下归属）。
func (h *harness) openedWorkItem() *WorkItem {
	h.t.Helper()
	h.issue(1, "一", "a")
	w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	h.mustProcess()
	return h.wantState(w.ID, StatePROpen)
}

// originRow 返回 PR 的归属（没有时 ok 为 false）。
func (h *harness) originRow(pr int) (workItem int64, phase string, round int, head, base, creator string, ok bool) {
	h.t.Helper()
	err := h.db.QueryRow(h.ctx, `SELECT work_item_id, phase, impl_round, head_ref, base_ref, creator_login
		FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr).
		Scan(&workItem, &phase, &round, &head, &base, &creator)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", 0, "", "", "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return workItem, phase, round, head, base, creator, true
}

// TestPullOriginRecorded：推进交付、创建 PR 时在同一事务中记下它的永久归属；合并之后（工作项和 Issue 都不再 active）
// 仍然认得出来，自动打 tag 可以发布。
func TestPullOriginRecorded(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.openedWorkItem()
	if w.PRNumber == nil {
		t.Fatal("工作项没有 PR")
	}
	id, phase, round, head, base, _, ok := h.originRow(*w.PRNumber)
	if !ok || id != w.ID || phase != PhaseImplement || round != w.ImplRound || head != w.Branch() || base != "main" {
		t.Fatalf("归属 = %d %s %d %q %q（%v），想要工作项 #%d、%s、第 %d 轮、%q、main",
			id, phase, round, head, base, ok, w.ID, PhaseImplement, w.ImplRound, w.Branch())
	}

	// 合并之后工作项和关联 Issue 都不再 active，归属仍然有效
	h.exec(`UPDATE dev_work_items SET state=$2, active=false WHERE id=$1`, w.ID, StateMerged)
	h.exec(`UPDATE dev_work_item_issues SET active=false WHERE work_item_id=$1`, w.ID)
	o, err := ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, *w.PRNumber, w.Branch(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != AutoTagOriginImplementation || o.WorkItemID != w.ID || o.ImplRound != w.ImplRound || o.BaseRef != "main" {
		t.Fatalf("合并后的归属 = %+v", o)
	}
	if len(o.Issues) != 1 || o.Issues[0] != 1 {
		t.Fatalf("Issue = %v", o.Issues)
	}
}

// TestPullOriginFixUpdatesDeliveryOnly：返修推进同一个 PR 时只更新交付引用，身份列（工作项、阶段、轮次、分支）不变；
// 身份列对不上的写入什么也不改。
func TestPullOriginFixUpdatesDeliveryOnly(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.openedWorkItem()
	pr := *w.PRNumber
	id, phase, round, head, base, _, _ := h.originRow(pr)

	task := int64(77)
	write := func(o pullOrigin) error {
		return h.db.Tx(h.ctx, func(tx pgx.Tx) error { return recordPullOrigin(h.ctx, tx, o) })
	}
	baseOrigin := pullOrigin{
		RepoID: testRepoID, PR: pr, WorkItemID: w.ID, Phase: PhaseImplement, ImplRound: w.ImplRound, HeadRef: w.Branch(),
		BaseRef: "main", DeliveryTaskID: &task, DeliverySHA: "2222222222222222222222222222222222222222",
	}
	if err := write(baseOrigin); err != nil {
		t.Fatal(err)
	}
	var gotSHA string
	var gotTask *int64
	if err := h.db.QueryRow(h.ctx, `SELECT delivery_sha, delivery_task_id FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`,
		testRepoID, pr).Scan(&gotSHA, &gotTask); err != nil {
		t.Fatal(err)
	}
	if gotSHA != baseOrigin.DeliverySHA || gotTask == nil || *gotTask != task {
		t.Fatalf("交付引用 = %q %v", gotSHA, gotTask)
	}
	if gotID, gotPhase, gotRound, gotHead, gotBase, _, _ := h.originRow(pr); gotID != id || gotPhase != phase || gotRound != round || gotHead != head || gotBase != base {
		t.Fatal("返修推进不应改写身份列")
	}

	// 身份列不一致（换了阶段/分支）：不更新
	other := baseOrigin
	other.HeadRef = "kiln/dev-999"
	other.DeliverySHA = "3333333333333333333333333333333333333333"
	if err := write(other); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(h.ctx, `SELECT delivery_sha FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr).
		Scan(&gotSHA); err != nil {
		t.Fatal(err)
	}
	if gotSHA != baseOrigin.DeliverySHA {
		t.Fatalf("身份列不一致时不应更新交付引用：%q", gotSHA)
	}
}

// TestResolveAutoTagOrigin：来源判定的各个分支（可信实现、设计、归属未确认、拒绝、未知）。
func TestResolveAutoTagOrigin(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.openedWorkItem()
	pr := *w.PRNumber
	resolve := func(repoID int64, pr int, head, base string) AutoTagOrigin {
		h.t.Helper()
		o, err := ResolveAutoTagOrigin(h.ctx, h.db, repoID, pr, head, base)
		if err != nil {
			h.t.Fatal(err)
		}
		return o
	}
	if o := resolve(testRepoID, pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation {
		t.Fatalf("实现 PR：%+v", o)
	}
	// 合并事件没有带分支：仍然按永久归属判断，不做分支一致性检查（也覆盖 headRef 解析不出的参数路径）
	if o := resolve(testRepoID, pr, "", ""); o.Kind != AutoTagOriginImplementation {
		t.Fatalf("没有带分支的实现 PR：%+v", o)
	}
	// 分支/目标分支与记录不一致
	if o := resolve(testRepoID, pr, "kiln/dev-42", "main"); o.Kind != AutoTagOriginDenied {
		t.Fatalf("来源分支不一致：%+v", o)
	}
	if o := resolve(testRepoID, pr, w.Branch(), "release"); o.Kind != AutoTagOriginDenied {
		t.Fatalf("目标分支不一致：%+v", o)
	}
	// 错 PR 号、错仓库
	if o := resolve(testRepoID, pr+999, w.Branch(), "main"); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("错 PR 号：%+v", o)
	}
	if o := resolve(testRepoID, pr+999, "", ""); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("错 PR 号、没有分支：%+v", o)
	}
	if o := resolve(testRepoID+1, pr, w.Branch(), "main"); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("错仓库：%+v", o)
	}
	// 伪造的正式分支名，没有工作项
	if o := resolve(testRepoID, pr+999, "kiln/dev-9999", "main"); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("伪造分支：%+v", o)
	}

	// 设计 PR
	h.exec(`UPDATE dev_pull_origins SET phase=$2 WHERE repo_id=$1 AND pr_number=$3`, testRepoID, PhaseDesign, pr)
	if o := resolve(testRepoID, pr, w.Branch(), "main"); o.Kind != AutoTagOriginDesign || o.WorkItemID != w.ID {
		t.Fatalf("设计 PR：%+v", o)
	}
	// 设计 PR 的历史身份（design_pr 兜底，例如升级前没有归属记录）。design_pr 只在这个工作项进入实现时才写入，此时工作项的
	// phase 是 implement、impl_round>=1；这个 PR 在设计阶段合并，来源判定必须仍然返回 design / 第 0 轮，不能把工作项当前的
	// 阶段、轮次读出来拼出自相矛盾的审计记录（AGY 复审）。
	h.exec(`DELETE FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)
	h.exec(`UPDATE dev_work_items SET design_pr=$2 WHERE id=$1`, w.ID, pr)
	if got := h.workItem(w.ID); got.Phase != PhaseImplement || got.ImplRound < 1 {
		t.Fatalf("前置条件：工作项应已进入实现（phase=%s round=%d）", got.Phase, got.ImplRound)
	}
	if o := resolve(testRepoID, pr, w.Branch(), "main"); o.Kind != AutoTagOriginDesign || o.WorkItemID != w.ID ||
		o.Phase != PhaseDesign || o.ImplRound != 0 {
		t.Fatalf("design_pr：%+v，想要设计 PR、%s、第 0 轮", o, PhaseDesign)
	}

	// 工作项结束后仍然认得历史实现 PR（归属记录在，工作项状态是 merged）
	h.exec(`UPDATE dev_work_items SET design_pr=NULL, state=$2, active=false WHERE id=$1`, w.ID, StateMerged)
	h.exec(`INSERT INTO dev_pull_origins (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref, delivery_sha)
		VALUES ($1,$2,$3,$4,$5,$6,'main','2222222222222222222222222222222222222222')`, testRepoID, pr, w.ID, PhaseImplement, w.ImplRound, w.Branch())
	if o := resolve(testRepoID, pr, w.Branch(), "main"); o.Kind != AutoTagOriginImplementation {
		t.Fatalf("合并后的历史实现 PR：%+v", o)
	}

	// 终止、没有改动：拒绝
	for _, state := range []string{StateStopped, StateNoChange} {
		h.exec(`UPDATE dev_work_items SET state=$2 WHERE id=$1`, w.ID, state)
		if o := resolve(testRepoID, pr, w.Branch(), "main"); o.Kind != AutoTagOriginDenied || o.Reason == "" {
			t.Fatalf("%s：%+v", state, o)
		}
	}

	// 工作项建了 PR、编号还没记下（delivering）：归属未确认
	h.exec(`UPDATE dev_work_items SET state=$2, pr_number=NULL, active=true WHERE id=$1`, w.ID, StateDelivering)
	h.exec(`DELETE FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)
	if o := resolve(testRepoID, pr+999, w.Branch(), "main"); o.Kind != AutoTagOriginRecording || o.WorkItemID != w.ID {
		t.Fatalf("归属未确认：%+v", o)
	}
	if o := resolve(testRepoID, pr+999, "kiln/dev-9999", "main"); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("伪造分支：%+v", o)
	}
}

// TestResolveAutoTagOriginBigWorkItemID：工作项编号超过 int4 上限（kiln/dev-2147483648）时来源判定不能崩：没有对应工作项
// 返回未知且不报错；有对应的推进中工作项返回归属未确认。判定 SQL 里 $3 必须在第一次出现时就按 bigint 推导（$3::bigint<>0），
// 否则 PostgreSQL 会按整数常量 0 把它定成 int4，pgx 编码 2147483648 时失败，auto_tags 卡在 pending 重试（Codex 复审）。
func TestResolveAutoTagOriginBigWorkItemID(t *testing.T) {
	h := newHarness(t, devConfigYML)
	bigID := int64(math.MaxInt32) + 1
	headRef := devreport.WorkItemBranch(bigID, PhaseImplement, 1)
	if headRef != "kiln/dev-2147483648" {
		t.Fatalf("正式分支 = %q", headRef)
	}

	// 没有对应的工作项：未知，且不因为参数类型错误返回 error
	o, err := ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, 2, headRef, "main")
	if err != nil {
		t.Fatalf("没有对应工作项时不应报错：%v", err)
	}
	if o.Kind != AutoTagOriginUnknown {
		t.Fatalf("没有对应工作项：%+v，想要未知", o)
	}

	// 插一条编号相同、推进中的工作项：认得出它、归属未确认（claim_attempt_id 外键在插入前去掉，本测试不建 ai_attempt）
	h.exec(`ALTER TABLE dev_work_items DROP CONSTRAINT dev_work_items_claim_attempt_id_fkey`)
	h.exec(`INSERT INTO dev_work_items (id, repo_id, state, active, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES ($1, $2, 'delivering', true, 'main', 'abc', 2, 'claude-code', 7)`, bigID, testRepoID)
	o, err = ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, 2, headRef, "main")
	if err != nil {
		t.Fatalf("有对应工作项时不应报错：%v", err)
	}
	if o.Kind != AutoTagOriginRecording || o.WorkItemID != bigID || o.Phase != PhaseImplement || o.ImplRound != 1 {
		t.Fatalf("推进中的大编号工作项：%+v，想要归属未确认、工作项 #%d、%s、第 1 轮", o, bigID, PhaseImplement)
	}
}

// TestEnsurePullDoesNotAdoptThirdParty：正式分支上有一个不是 Kiln 创建的 PR 时不接管（工作项受阻、没有归属记录），
// 绝不把第三方或伪造的 PR 变成可信发布来源。
func TestEnsurePullDoesNotAdoptThirdParty(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	w, _, sha := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	// 有人用同一个正式分支开了一个 PR
	h.git("branch", w.Branch(), sha)
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 9, Title: "第三方", HeadRef: w.Branch(), HeadSHA: sha, BaseRef: "main", Author: "carol"})
	h.mustProcess()
	got := h.wantState(w.ID, StateBlocked)
	if got.PRNumber != nil {
		t.Fatalf("不应接管第三方的 PR：%+v", got.PRNumber)
	}
	if _, _, _, _, _, _, ok := h.originRow(9); ok {
		t.Fatal("不应记下第三方 PR 的归属")
	}
	if h.countRequests("POST /repos/alice/demo/pulls") != 0 {
		t.Error("不应再创建一个 PR")
	}
}

// TestPartialRoundKeepsPreviousOrigin（PLAN #29.4、#31.3）：一个 PR 完成、部分完成的第 1 轮合并后开始第 2 轮
// （清空 PR 编号、换分支），第 1 轮的实现 PR 仍然认作可信来源；第 2 轮的新 PR 没有归属，也不凭阶段、分支推断。
func TestPartialRoundKeepsPreviousOrigin(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.partialRound()
	if w.ImplRound != 2 || w.PRNumber != nil || w.Branch() == "" {
		t.Fatalf("第 2 轮 = %s", jsonString(w))
	}
	var pr int
	if err := h.db.QueryRow(h.ctx, `SELECT pr_number FROM dev_pull_origins WHERE work_item_id=$1`, w.ID).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	id, phase, round, head, base, _, ok := h.originRow(pr)
	if !ok || id != w.ID || phase != PhaseImplement || round != 1 || head == "" || base != "main" {
		t.Fatalf("第 1 轮的归属 = %d %s %d %q %q（%v）", id, phase, round, head, base, ok)
	}
	if o := h.resolveOrigin(pr, head, base); o.Kind != AutoTagOriginImplementation || o.ImplRound != 1 {
		t.Fatalf("第 1 轮的来源 = %+v，想要可信的实现 PR", o)
	}
	// 第 2 轮的新 PR 还没有归属：未知，不按正式分支名推断
	if o := h.resolveOrigin(pr+1, w.Branch(), "main"); o.Kind != AutoTagOriginUnknown {
		t.Fatalf("第 2 轮还没有归属的 PR：%+v", o)
	}
}

// TestResolveRequiresClaimAndDelivery：只读判定依赖工作项的领取记录和交付引用；缺任意一个都拒绝（不自动发布）。
func TestResolveRequiresClaimAndDelivery(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.openedWorkItem()
	pr := *w.PRNumber
	// 这个测试只验证判定逻辑，不建 AI 任务和 attempt
	h.exec(`ALTER TABLE dev_work_items DROP CONSTRAINT dev_work_items_claim_attempt_id_fkey`)
	resolve := func() AutoTagOrigin {
		h.t.Helper()
		o, err := ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, pr, w.Branch(), "main")
		if err != nil {
			h.t.Fatal(err)
		}
		return o
	}
	// 没有领取记录
	h.exec(`UPDATE dev_work_items SET claim_attempt_id=0 WHERE id=$1`, w.ID)
	if o := resolve(); o.Kind != AutoTagOriginDenied || !strings.Contains(o.Reason, "领取") {
		t.Fatalf("缺领取记录：%+v", o)
	}
	// 没有交付引用（任务和 commit 都为空）
	h.exec(`UPDATE dev_work_items SET claim_attempt_id=5 WHERE id=$1`, w.ID)
	h.exec(`UPDATE dev_pull_origins SET delivery_task_id=NULL, delivery_sha='' WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)
	if o := resolve(); o.Kind != AutoTagOriginDenied || !strings.Contains(o.Reason, "交付") {
		t.Fatalf("缺交付引用：%+v", o)
	}
}

// TestDesignPhaseWithoutRecordIsUnknown：设计阶段、既没有归属记录也没有 design_pr 兜底时，不按工作项的阶段或正式分支名
// 推断，判定为未知（不放行）。
func TestDesignPhaseWithoutRecordIsUnknown(t *testing.T) {
	h := newHarness(t, devConfigYML)
	w := h.openedWorkItem()
	pr := *w.PRNumber
	h.exec(`DELETE FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2`, testRepoID, pr)
	h.exec(`UPDATE dev_work_items SET phase=$2, design_pr=NULL, pr_number=NULL WHERE id=$1`, w.ID, PhaseDesign)
	o, err := ResolveAutoTagOrigin(h.ctx, h.db, testRepoID, pr, w.Branch(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != AutoTagOriginUnknown {
		t.Fatalf("设计阶段没有记录：%+v，想要未知", o)
	}
}

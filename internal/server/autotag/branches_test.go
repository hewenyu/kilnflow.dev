package autotag

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
)

// 合并队列允许合并多个目标分支之后，自动打 tag 要按合并进的分支裁决是否发版（PLAN #29.4）：只有合入发版分支的合并才
// 打 tag；不是发版分支的记录跳过但不刷屏；同一个 commit 随后从发版分支合入（fast-forward）时重新裁决。

// recordBase 记下 PR pr 从 base 合入 commit sha；mergeDefault 是合并那一刻仓库的默认分支（未知时为空）。返回是否新写入
// 或重新激活了记录。
func (h *harness) recordBase(sha string, pr int, base, mergeDefault string, labels ...string) bool {
	h.t.Helper()
	added, err := h.svc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: repoID, SHA: sha, PR: pr, Labels: labels, MergedBy: "bob", BaseRef: base,
		DefaultBranchAtMerge: mergeDefault, Source: mqevents.AutoTagFromWebhook,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return added
}

// TestBranchNotRelease：tag.branch 是 master 时，合入 dev 的合并跳过并记下原因码（不在页面刷屏、不建 tag、不规划），
// 合入 master 的合照相常打 tag。
func TestBranchNotRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "branches: [dev]\ntag:\n  branch: master\n")
	dev := h.commit(nil, "合入 dev")
	h.recordBase(dev, 1, "dev", "main")
	h.mustProcess()
	r := h.wantState(dev, StateSkipped, "")
	if r.SkipCode != SkipCodeBranchNotRelease || !strings.Contains(r.Reason, "dev") || !strings.Contains(r.Reason, "master") ||
		r.ReasonNote.Params.Branch != "dev" || r.ReasonNote.Params.Release != "master" {
		t.Fatalf("跳过的记录：skip_code=%q reason=%q note=%+v", r.SkipCode, r.Reason, r.ReasonNote)
	}
	if h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
		t.Error("不是发版分支的合并不应建 tag 或规划发布")
	}
	visible, err := ListVisible(h.ctx, h.db, repoID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("ListVisible 返回了 %d 条（应排除不是发版分支的记录）", len(visible))
	}
	all, err := List(h.ctx, h.db, repoID, 50)
	if err != nil || len(all) != 1 {
		t.Fatalf("List 返回 %d 条：%v", len(all), err)
	}

	master := h.commit(nil, "合入 master")
	h.recordBase(master, 2, "master", "main")
	h.mustProcess()
	h.wantState(master, StatePlanned, "v0.1.0")
	if h.countRequests(refsRoute) != 1 || len(h.planner.requests()) != 1 {
		t.Errorf("发版分支的合并应建 tag 并规划一次：refs=%d 规划=%d", h.countRequests(refsRoute), len(h.planner.requests()))
	}
	visible, err = ListVisible(h.ctx, h.db, repoID, 50)
	if err != nil || len(visible) != 1 || visible[0].SHA != master {
		t.Fatalf("ListVisible = %+v：%v", visible, err)
	}
}

// TestFastForwardPromotion：同一个 commit 先合入 dev（跳过），随后 dev -> master 的合并（fast-forward）从另一个分支合入，
// 记录重新裁决为 pending 并打上 tag；已经 planned / tagging / 其他原因跳过的记录不被覆盖。
func TestFastForwardPromotion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  branch: master\n")
	c1 := h.commit(nil, "fast-forward")
	h.recordBase(c1, 10, "dev", "main")
	h.mustProcess()
	h.wantState(c1, StateSkipped, "")

	if !h.recordBase(c1, 11, "master", "main") {
		t.Fatal("同一个 commit 从 master 合入时应重新激活记录")
	}
	if r := h.get(c1); r.State != StatePending || r.SkipCode != "" || r.PR != 11 {
		t.Fatalf("重新激活后：state=%s skip_code=%q pr=%d", r.State, r.SkipCode, r.PR)
	}
	h.mustProcess()
	h.wantState(c1, StatePlanned, "v0.1.0")
	if got := h.tagsAt(c1); len(got) != 1 || got[0] != "v0.1.0" {
		t.Fatalf("commit 上的 tag：%v", got)
	}

	// 已经 planned 的记录不因再次从别的分支合入而重来
	if h.recordBase(c1, 12, "main", "main") {
		t.Fatal("planned 的记录不应被覆盖")
	}
	if r := h.get(c1); r.State != StatePlanned || r.PR != 11 {
		t.Fatalf("planned 的记录被改动：%+v", r)
	}

	// release:skip 等业务跳过（skip_code 为空）不因分支不同而重新激活
	c2 := h.commit(nil, "release:skip")
	h.recordBase(c2, 20, "master", "main", LabelSkip)
	h.mustProcess()
	h.wantState(c2, StateSkipped, "")
	if h.recordBase(c2, 21, "dev", "main") {
		t.Fatal("release:skip 的记录不应被覆盖")
	}
	if r := h.get(c2); r.State != StateSkipped || r.PR != 20 {
		t.Fatalf("release:skip 的记录被改动：%+v", r)
	}

	// 已经进入 tagging（tag 可能已经建好）的记录不覆盖
	c3 := h.commit(nil, "tagging")
	h.recordBase(c3, 30, "master", "main")
	h.fake.SetFault(refsRoute, fakegithub.Fault{Lost: true})
	if err := h.process(); err == nil {
		t.Fatal("响应丢失时应返回错误（稍后重试）")
	}
	h.fake.SetFault(refsRoute, fakegithub.Fault{})
	h.wantState(c3, StateTagging, "v0.1.1")
	if h.recordBase(c3, 31, "dev", "main") {
		t.Fatal("tagging 的记录不应被覆盖")
	}
	if r := h.get(c3); r.State != StateTagging || r.PR != 30 {
		t.Fatalf("tagging 的记录被改动：%+v", r)
	}
}

// TestTagBranchExplicit：显式配置的 tag.branch 不受默认分支影响；合入其他分支的合并跳过。
func TestTagBranchExplicit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "branches: [main, release]\ntag:\n  branch: release\n")
	rel := h.commit(nil, "合入 release")
	h.recordBase(rel, 1, "release", "main")
	h.mustProcess()
	h.wantState(rel, StatePlanned, "v0.1.0")

	main := h.commit(nil, "合入 main")
	h.recordBase(main, 2, "main", "main")
	h.mustProcess()
	if r := h.wantState(main, StateSkipped, ""); r.SkipCode != SkipCodeBranchNotRelease {
		t.Fatalf("合入 main 应因为不是发版分支 release 而跳过：%+v", r)
	}
}

// TestDefaultBranchRenamed：省略 tag.branch 时按合并那一刻的默认分支裁决；默认分支改过名后，迟到的 webhook 仍按合并当时
// 的默认分支判断。
func TestDefaultBranchRenamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	oldDefault := h.commit(nil, "改名前合入默认分支 release")
	h.recordBase(oldDefault, 1, "release", "release")
	h.mustProcess()
	h.wantState(oldDefault, StatePlanned, "v0.1.0")

	newDefault := h.commit(nil, "改名后合入默认分支 main")
	h.recordBase(newDefault, 2, "main", "main")
	h.mustProcess()
	h.wantState(newDefault, StatePlanned, "v0.1.1")

	// 合并当时默认分支是 main，却从旧默认分支 release 合入：不是发版分支
	other := h.commit(nil, "从 release 合入")
	h.recordBase(other, 3, "release", "main")
	h.mustProcess()
	if r := h.wantState(other, StateSkipped, ""); r.SkipCode != SkipCodeBranchNotRelease {
		t.Fatalf("旧的默认分支不是发版分支：%+v", r)
	}
}

// TestLegacyBranchRecords：升级前的记录没有目标分支。省略 tag.branch 时照常发版（Retry 也放行）；显式配置 tag.branch
// 时跳过并说明无法确认，Retry 仍无法确定。
func TestLegacyBranchRecords(t *testing.T) {
	t.Parallel()
	t.Run("省略 tag.branch", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, tagConfig)
		c := h.commit(nil, "旧记录")
		h.record(c, 1) // BaseRef 为空
		h.mustProcess()
		h.wantState(c, StatePlanned, "v0.1.0")
	})
	t.Run("显式 tag.branch", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, "tag:\n  branch: master\n")
		c := h.commit(nil, "旧记录")
		h.record(c, 1)
		h.mustProcess()
		r := h.wantState(c, StateSkipped, "")
		if r.SkipCode != "" || !strings.Contains(r.Reason, "历史记录未记下目标分支") {
			t.Fatalf("旧记录应说明无法确认目标分支：%+v", r)
		}
		if err := h.svc.Retry(h.ctx, repoID, c, "alice"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		if r := h.wantState(c, StateSkipped, ""); !strings.Contains(r.Reason, "历史记录未记下目标分支") {
			t.Fatalf("Retry 后仍无法确认目标分支：%+v", r)
		}
	})
}

// TestTaggedResumeAfterTagBranchChange：已经建好 tag、等待规划发布的记录（tagged）不受之后的 tag.branch 修改影响，照常
// 规划发布（T2）；此后的新记录按新的 tag.branch 裁决。
func TestTaggedResumeAfterTagBranchChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  branch: master\n")
	c := h.commit(nil, "已经建好 tag")
	h.git("tag", "v0.1.0", c)
	h.recordBase(c, 1, "master", "main")
	h.planner.fail = 1
	if err := h.process(); err == nil {
		t.Fatal("规划失败时应返回错误（稍后重试）")
	}
	h.wantState(c, StateTagged, "v0.1.0")

	// 把发版分支改成 dev
	h.commit(map[string]string{".kiln/merge.yml": "tag:\n  branch: dev\n"}, "改发版分支")
	h.mustProcess()
	h.wantState(c, StatePlanned, "v0.1.0")
	reqs := h.planner.requests()
	if len(reqs) != 2 || reqs[1].Tag != "v0.1.0" || reqs[1].SHA != c {
		t.Fatalf("规划请求：%+v", reqs)
	}

	// 改完之后从 master 合入的新记录按新的 tag.branch 跳过
	c2 := h.commit(nil, "改完之后合入 master")
	h.recordBase(c2, 2, "master", "main")
	h.mustProcess()
	if r := h.wantState(c2, StateSkipped, ""); r.SkipCode != SkipCodeBranchNotRelease {
		t.Fatalf("新的 tag.branch 是 dev：%+v", r)
	}
}

// TestNoTagConfigBranch：没有 tag: 配置时同样记下原因码（合入的不是默认分支），ListVisible 不显示。
func TestNoTagConfigBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "method: squash\n")
	c := h.commit(nil, "合入 dev")
	h.recordBase(c, 1, "dev", "main")
	h.mustProcess()
	r := h.wantState(c, StateSkipped, "")
	if r.SkipCode != SkipCodeBranchNotRelease || !strings.Contains(r.Reason, "没有 tag:") {
		t.Fatalf("记录：%+v", r)
	}
	visible, err := ListVisible(h.ctx, h.db, repoID, 50)
	if err != nil || len(visible) != 0 {
		t.Fatalf("ListVisible = %d 条：%v", len(visible), err)
	}
	if h.countRequests(refsRoute) != 0 {
		t.Error("不应建 tag")
	}
}

// countCandidates 返回这个 commit 的待裁决候选数。
func (h *harness) countCandidates(sha string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM auto_tag_candidates WHERE repo_id=$1 AND sha=$2`, repoID, sha).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// TestFastForwardWhilePending（审计）：先记下从 dev 的合并、还没处理，同一个 commit 又从 master 合入（fast-forward）：
// pending 期间的记录不能被直接覆盖，也不能丢掉 master 的合并事实。候选留下，执行者离开 pending 之前按能发版的 master 裁决
// 并打 tag，候选清空。
func TestFastForwardWhilePending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  branch: master\n")
	// 先有一个版本 tag，release:minor 才能体现为 v0.2.0（第一个版本 tag 总是 initial）
	h.git("tag", "v0.1.0")
	c1 := h.commit(nil, "pending 窗口内的 fast-forward")
	if !h.recordBase(c1, 10, "dev", "main") {
		t.Fatal("第一次记录应写入")
	}
	// 不先处理：记录还是 pending，同一个 commit 从 master 合入
	if !h.recordBase(c1, 11, "master", "main", LabelMinor) {
		t.Fatal("pending 期间从 master 合入应留下候选（并通知执行者）")
	}
	if r := h.get(c1); r.State != StatePending || r.PR != 10 || r.BaseRef != "dev" {
		t.Fatalf("pending 的记录不应被后来的 dev/master 事实直接覆盖：state=%s pr=%d base=%q", r.State, r.PR, r.BaseRef)
	}
	if n := h.countCandidates(c1); n != 1 {
		t.Fatalf("候选数 = %d，想要 1", n)
	}

	h.mustProcess()
	r := h.wantState(c1, StatePlanned, "v0.2.0") // master 的 label release:minor
	if r.PR != 11 || r.BaseRef != "master" || !slices.Equal(r.Labels, []string{LabelMinor}) {
		t.Fatalf("应按 master 的合并打 tag：pr=%d base=%q labels=%v", r.PR, r.BaseRef, r.Labels)
	}
	if got := h.tagsAt(c1); len(got) != 1 || got[0] != "v0.2.0" {
		t.Fatalf("commit 上的 tag：%v", got)
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("离开 pending 后候选应清空，实际 %d 条", n)
	}
}

// TestOutOfOrderRetargetKeepsReleaseBranch（审计）：乱序到达——先记下从 master 的合并、还没处理，晚到的 dev webhook
// 不能反过来覆盖 master 的合并事实；执行者仍按 master 打 tag，候选清空。
func TestOutOfOrderRetargetKeepsReleaseBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  branch: master\n")
	c1 := h.commit(nil, "乱序的 webhook")
	if !h.recordBase(c1, 12, "master", "main") {
		t.Fatal("master 的合并应写入")
	}
	if !h.recordBase(c1, 13, "dev", "main") {
		t.Fatal("晚到的 dev 应留下候选")
	}
	if r := h.get(c1); r.State != StatePending || r.PR != 12 || r.BaseRef != "master" {
		t.Fatalf("晚到的 dev 不应覆盖 pending 的 master：state=%s pr=%d base=%q", r.State, r.PR, r.BaseRef)
	}
	h.mustProcess()
	r := h.wantState(c1, StatePlanned, "v0.1.0")
	if r.PR != 12 || r.BaseRef != "master" {
		t.Fatalf("应按 master 的合并打 tag：pr=%d base=%q", r.PR, r.BaseRef)
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("离开 pending 后候选应清空，实际 %d 条", n)
	}
	// 已经 planned 的记录不因晚到的 dev 再被激活
	if h.recordBase(c1, 14, "dev", "main") {
		t.Fatal("planned 的记录不应被覆盖")
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("planned 之后不应留下候选，实际 %d 条", n)
	}
}

// TestCandidateUsesOwnDefaultBranch（审计）：默认分支改名叠加 pending fast-forward——原记录 base_ref=dev、
// default_branch_at_merge=main（当时默认分支是 main），候选 base_ref=release、default_branch_at_merge=release（当时默认
// 分支是 release）。按候选自己的默认分支快照，release 就是当时的发版分支，应正确晋级并打 tag，不能被原记录的 main 误判。
func TestCandidateUsesOwnDefaultBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig) // tag: {}：发版分支是合并那一刻的默认分支
	c1 := h.commit(nil, "改名后的 fast-forward")
	if !h.recordBase(c1, 10, "dev", "main") {
		t.Fatal("dev 的合并应写入")
	}
	if !h.recordBase(c1, 11, "release", "release") {
		t.Fatal("默认分支还是 release 时从 release 合入应留下候选")
	}
	h.mustProcess()
	r := h.wantState(c1, StatePlanned, "v0.1.0")
	if r.PR != 11 || r.BaseRef != "release" || r.DefaultBranchAtMerge != "release" {
		t.Fatalf("应按候选自己的默认分支快照晋级：pr=%d base=%q default=%q", r.PR, r.BaseRef, r.DefaultBranchAtMerge)
	}
	if got := h.tagsAt(c1); len(got) != 1 || got[0] != "v0.1.0" {
		t.Fatalf("commit 上的 tag：%v", got)
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("晋级后候选应清空，实际 %d 条", n)
	}
}

// TestInactiveRepoClearsCandidates（审计）：仓库停用时把 pending 记为 failed 的同时必须清掉待裁决候选；否则
// 恢复后 Retry 会把停用期间残留的候选当成有效事实，打出一个本不该有的 tag。
func TestInactiveRepoClearsCandidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	c1 := h.commit(nil, "停用期间的合并")
	if !h.recordBase(c1, 10, "dev", "main") {
		t.Fatal("dev 的合并应写入")
	}
	if !h.recordBase(c1, 11, "main", "main") {
		t.Fatal("pending 期间从发版分支 main 合入应留下候选")
	}
	if n := h.countCandidates(c1); n != 1 {
		t.Fatalf("候选数 = %d，想要 1", n)
	}
	h.exec(`UPDATE repos SET active=false WHERE id=$1`, repoID)
	h.mustProcess()
	if r := h.wantState(c1, StateFailed, ""); !strings.Contains(r.Reason, "仓库已停用") || r.ReasonNote.Code != ReasonRepoInactive {
		t.Fatalf("停用后的原因：%q %+v", r.Reason, r.ReasonNote)
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("停用时应清空候选，实际 %d 条", n)
	}
	h.exec(`UPDATE repos SET active=true WHERE id=$1`, repoID)
	if err := h.svc.Retry(h.ctx, repoID, c1, "admin"); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	// 只剩原记录 base_ref=dev：按 branch_not_release 跳过，绝不能消费停用期残留的 main 候选来打 tag
	r := h.wantState(c1, StateSkipped, "")
	if r.SkipCode != SkipCodeBranchNotRelease || r.PR != 10 || r.BaseRef != "dev" {
		t.Fatalf("Retry 后应按原记录跳过：skip_code=%q pr=%d base=%q", r.SkipCode, r.PR, r.BaseRef)
	}
	if h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
		t.Fatalf("停用期的残留候选不应被消费：建 tag %d 次，规划 %d 次", h.countRequests(refsRoute), len(h.planner.requests()))
	}
}

// waitLockWait 等 want 个独立的连接真实阻塞在 auto_tags 的行锁上（按 pid 去重：一个连接可能同时等多个锁）。独立的连接
// 持有 FOR UPDATE 时，RecordAutoTag / 执行者的 resolveCandidates 会各自排在锁等待队列里，必须各自真正排上才算进入
// 竞争窗口。用 pg_stat_activity 的 Lock 等待配合 pg_locks 里未授予的行锁确认，避免靠 sleep 猜时序。
func (h *harness) waitLockWait(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := h.db.QueryRow(h.ctx, `SELECT count(DISTINCT a.pid) FROM pg_stat_activity a
			JOIN pg_locks l ON l.pid = a.pid
			WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
			AND a.wait_event_type='Lock' AND NOT l.granted AND l.locktype IN ('tuple','transactionid')
			AND a.query LIKE '%auto_tags%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 auto_tags 行锁的连接数没有达到 %d", want)
}

// TestConcurrentRecordAndDecideKeepsRelease（审计）：真实的双连接并发——RecordAutoTag(master) 与执行者的
// decide(dev) 串行化后绝不丢失发版分支上的合并事实。这里用一个独立连接先锁住父行，让真实的 RecordAutoTag(master) 先
// 排进锁等待队列，再启动真实的 Process；释放后两者按队列顺序串行执行，无论谁先提交，最终都必须按 master 打出 tag。
func TestConcurrentRecordAndDecideKeepsRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "tag:\n  branch: master\n") // 发版分支是 master；原记录从 dev 合入，候选来自 master
	c1 := h.commit(nil, "并发窗口内的 fast-forward")
	if !h.recordBase(c1, 10, "dev", "main") {
		t.Fatal("dev 的合并应写入")
	}
	conn, err := h.db.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(h.ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(h.ctx, `SELECT 1 FROM auto_tags WHERE repo_id=$1 AND sha=$2 FOR UPDATE`, repoID, c1); err != nil {
		t.Fatal(err)
	}
	// 先让真实的 RecordAutoTag(master) 排进锁等待队列
	type recordResult struct {
		added bool
		err   error
	}
	recorded := make(chan recordResult, 1)
	go func() {
		added, err := h.svc.Record(h.ctx, mqevents.AutoTagRecord{
			RepoID: repoID, SHA: c1, PR: 11, MergedBy: "bob", BaseRef: "master",
			DefaultBranchAtMerge: "main", Source: mqevents.AutoTagFromWebhook,
		})
		recorded <- recordResult{added: added, err: err}
	}()
	h.waitLockWait(t, 1) // RecordAutoTag(master) 已经阻塞在这行上
	// 再启动真实的执行者（decide），它的 resolveCandidates 也在同一行上 FOR UPDATE，必须真正排上锁等待
	processed := make(chan error, 1)
	go func() { processed <- h.svc.Process(h.ctx, repoID) }()
	h.waitLockWait(t, 2)
	if _, err := conn.Exec(h.ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if got := <-recorded; got.err != nil {
		t.Fatalf("RecordAutoTag：%v", got.err)
	} else if !got.added {
		t.Fatal("并发窗口内 master 的合并应记下（重新激活或候选）")
	}
	if err := <-processed; err != nil {
		t.Fatalf("Process：%v", err)
	}
	// 无论两者谁先提交，master 的合并事实都不能丢：补一次处理把记录推进完
	h.mustProcess()
	r := h.wantState(c1, StatePlanned, "v0.1.0")
	if r.PR != 11 || r.BaseRef != "master" {
		t.Fatalf("并发后应按 master 打 tag：state=%s pr=%d base=%q reason=%q", r.State, r.PR, r.BaseRef, r.Reason)
	}
	if got := h.tagsAt(c1); len(got) != 1 || got[0] != "v0.1.0" {
		t.Fatalf("commit 上的 tag：%v", got)
	}
	if n := h.countCandidates(c1); n != 0 {
		t.Fatalf("候选应清空，实际 %d 条", n)
	}
}

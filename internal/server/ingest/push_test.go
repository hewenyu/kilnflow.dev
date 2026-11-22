package ingest

import (
	"maps"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestPushBranch：分支 push 规划 run 并记录 ref_heads；同一次推送的其他 delivery 不重复建 run；
// 没有被 workflow 匹配的分支只记录 ref_heads。
func TestPushBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.head("main")
	if got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha)); got != "新建 1 个 run" {
		t.Fatalf("结果 %q", got)
	}
	r := h.lastRun(demoRepoID, 1)
	if r.Event != workflow.EventPush || r.Ref != "refs/heads/main" || r.SHA != sha || r.Source != planner.SourceWebhook || r.Workflow != "ci.yml" {
		t.Fatalf("run = %+v", r)
	}
	if got, obj, ok := h.refHead(demoRepoID, "refs/heads/main"); !ok || got != sha || obj != sha {
		t.Fatalf("ref_heads = (%s, %s, %v)，期望 %s", got, obj, ok, sha)
	}

	// GitHub 重新投递（新的 delivery ID）：dedupe_key 去重
	if got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha)); got != "新建 0 个 run" {
		t.Fatalf("重复推送的结果 %q", got)
	}
	h.lastRun(demoRepoID, 1)

	// 新的 commit：before 为上一次的 sha
	next := h.git.commit(map[string]string{"a.txt": "a\n"}, "second")
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", sha, next))
	if r := h.lastRun(demoRepoID, 2); r.SHA != next {
		t.Fatalf("第二个 run 的 sha = %s，期望 %s", r.SHA, next)
	}

	// workflow 不匹配的分支：不建 run，但记录 ref_heads（对账时不会当成漏掉的推送）
	if got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, next)); got != "新建 0 个 run" {
		t.Fatalf("feature 分支的结果 %q", got)
	}
	if _, _, ok := h.refHead(demoRepoID, "refs/heads/feature"); !ok {
		t.Fatal("没有记录 feature 分支")
	}
}

// TestPushTag：tag push 把 tag 解析到 commit（附注 tag 记录 tag 对象），按 on.push.tags 规划。
func TestPushTag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	commit := h.git.head("main")
	h.git.git("tag", "-a", "v1.0.0", "-m", "release")
	h.git.git("tag", "v1.0.1")
	tagObj := h.git.git("rev-parse", "v1.0.0")
	if tagObj == commit {
		t.Fatal("附注 tag 应指向 tag 对象")
	}
	tests := []struct {
		name, ref, wantObj string
	}{
		{"附注 tag", "refs/tags/v1.0.0", tagObj},
		{"轻量 tag", "refs/tags/v1.0.1", commit},
	}
	for n, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// payload 中的 after 对 tag 不可信（附注 tag 是 tag 对象），以 GitHub 解析的结果为准
			if got := h.deliverOK("push", h.pushPayload(h.demo, tt.ref, gitref.ZeroSHA, tt.wantObj)); got != "新建 1 个 run" {
				t.Fatalf("结果 %q", got)
			}
			r := h.lastRun(demoRepoID, n+1)
			if r.Ref != tt.ref || r.SHA != commit {
				t.Fatalf("run = %+v，期望 sha 为 commit %s", r, commit)
			}
			if sha, obj, ok := h.refHead(demoRepoID, tt.ref); !ok || sha != commit || obj != tt.wantObj {
				t.Fatalf("ref_heads = (%s, %s, %v)，期望 (%s, %s)", sha, obj, ok, commit, tt.wantObj)
			}
		})
	}
	t.Run("tag 已不存在", func(t *testing.T) {
		if got := h.deliverOK("push", h.pushPayload(h.demo, "refs/tags/v9", gitref.ZeroSHA, commit)); got != "tag 已不存在" {
			t.Fatalf("结果 %q", got)
		}
		if _, _, ok := h.refHead(demoRepoID, "refs/tags/v9"); ok {
			t.Fatal("不存在的 tag 不应记录")
		}
	})
	t.Run("删除 tag", func(t *testing.T) {
		if got := h.deliverOK("push", h.deletePayload(h.demo, "refs/tags/v1.0.1", commit)); got != "tag 已删除" {
			t.Fatalf("结果 %q", got)
		}
		if _, _, ok := h.refHead(demoRepoID, "refs/tags/v1.0.1"); ok {
			t.Fatal("删除的 tag 应从 ref_heads 中移除")
		}
		h.lastRun(demoRepoID, 2)
	})
}

// TestPushBranchDeleted：删除分支时取消它未结束的 push run，并从 ref_heads 中移除。
func TestPushBranchDeleted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.head("main")
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
	if r := h.lastRun(demoRepoID, 1); r.State != "active" {
		t.Fatalf("run 状态 %s，期望 active（没有 Runner 领取）", r.State)
	}
	if got := h.deliverOK("push", h.deletePayload(h.demo, "refs/heads/main", sha)); got != "分支已删除，取消 1 个 run" {
		t.Fatalf("结果 %q", got)
	}
	if r := h.lastRun(demoRepoID, 1); r.State != "cancelled" {
		t.Fatalf("run 状态 %s，期望 cancelled", r.State)
	}
	if _, _, ok := h.refHead(demoRepoID, "refs/heads/main"); ok {
		t.Fatal("删除的分支应从 ref_heads 中移除")
	}
	// 再次删除：没有要取消的 run
	if got := h.deliverOK("push", h.deletePayload(h.demo, "refs/heads/main", sha)); got != "分支已删除，取消 0 个 run" {
		t.Fatalf("再次删除的结果 %q", got)
	}
}

// TestPushBySelf：本 App 的 bot 账号推送的 tag（跨仓库发布时创建的）只更新 ref_heads，不规划（PLAN #7.2）；
// 推送的分支（合并队列合并、更新 PR 分支）照常规划，不算 Bot（PLAN #28.4）。
func TestPushBySelf(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	commit := h.git.head("main")
	h.git.git("tag", "-a", "v2.0.0", "-m", "by app")
	tagObj := h.git.git("rev-parse", "v2.0.0")

	p := withSender(h.pushPayload(h.demo, "refs/tags/v2.0.0", gitref.ZeroSHA, tagObj), testBot, "Bot")
	if got := h.deliverOK("push", p); got != "本 App 创建的 tag，只更新 ref_heads" {
		t.Fatalf("tag：结果 %q", got)
	}
	if sha, obj, ok := h.refHead(demoRepoID, "refs/tags/v2.0.0"); !ok || sha != commit || obj != tagObj {
		t.Fatalf("ref_heads = (%s, %s, %v)，期望 (%s, %s)", sha, obj, ok, commit, tagObj)
	}
	if runs := h.runs(demoRepoID); len(runs) != 0 {
		t.Fatalf("本 App 推送的 tag 不应产生 run：%+v", runs)
	}

	p = withSender(h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, commit), testBot, "Bot")
	if got := h.deliverOK("push", p); got != "新建 1 个 run" {
		t.Fatalf("分支：结果 %q", got)
	}
	if r := h.lastRun(demoRepoID, 1); r.SenderIsBot {
		t.Fatalf("本 App 推送的分支不算 Bot：%+v", r)
	}
	if sha, _, ok := h.refHead(demoRepoID, "refs/heads/main"); !ok || sha != commit {
		t.Fatalf("ref_heads[main] = %s，期望 %s", sha, commit)
	}
}

// TestPushPlanFailureKeepsRefHead：规划失败时不更新 ref_heads（对账会重试），事件稍后重试。
func TestPushPlanFailureKeepsRefHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	missing := strings.Repeat("cd", 20)
	e := h.deliver("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, missing))
	if e.Processed || e.Attempts != 1 {
		t.Fatalf("事件 = %+v，期望失败后等待重试", e)
	}
	if _, _, ok := h.refHead(demoRepoID, "refs/heads/main"); ok {
		t.Fatal("规划失败时不应更新 ref_heads")
	}
}

// TestPullRequest：PR 的 opened / synchronize / reopened 规划 run；edited 只在修改了目标分支时重新规划；
// closed 取消 run；fork 来的 PR 丢弃；本 App 触发的照常规划；PR 作者是 Bot 时标记 sender_is_bot（PLAN #7.2、#16.1、#28.4）。
func TestPullRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	head1 := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature 1")
	head2 := h.git.commit(map[string]string{"f.txt": "2\n"}, "feature 2")
	const prRef = "refs/pull/7/head"

	step := func(name, action, head, base string, mutate func(p map[string]any), wantResult string, wantRuns int) {
		t.Helper()
		p := h.prPayload(h.demo, action, 7, "feature", head, base)
		if mutate != nil {
			mutate(p)
		}
		if got := h.deliverOK("pull_request", p); got != wantResult {
			t.Fatalf("%s：结果 %q，期望 %q", name, got, wantResult)
		}
		if n := len(h.runs(demoRepoID)); n != wantRuns {
			t.Fatalf("%s：有 %d 个 run，期望 %d", name, n, wantRuns)
		}
	}

	step("打开", "opened", head1, "main", nil, "新建 1 个 run", 1)
	r := h.lastRun(demoRepoID, 1)
	if r.Event != workflow.EventPullRequest || r.Ref != prRef || r.SHA != head1 || r.PRNumber == nil || *r.PRNumber != 7 || r.SenderIsBot {
		t.Fatalf("PR run = %+v", r)
	}
	if sha, _, ok := h.refHead(demoRepoID, prRef); !ok || sha != head1 {
		t.Fatalf("ref_heads[%s] = %s，期望 %s", prRef, sha, head1)
	}
	step("修改标题", "edited", head1, "main", nil, "", 1)
	step("重复的 opened", "opened", head1, "main", nil, "新建 0 个 run", 1)
	step("推送", "synchronize", head2, "main", nil, "新建 1 个 run", 2)
	step("fork 来的 PR", "synchronize", head2, "main", func(p map[string]any) {
		head := field(t, field(t, p, "pull_request"), "head")
		fork := maps.Clone(field(t, head, "repo"))
		fork["id"] = 999
		head["repo"] = fork
	}, "fork 来的 PR，丢弃", 2)
	// 不知道 head、base 属于哪个仓库（fork 已删除时 head.repo 为 null）：按 fork 丢弃，不规划
	head3 := h.git.commit(map[string]string{"f.txt": "3\n"}, "feature 3")
	step("没有仓库信息的 PR", "synchronize", head3, "main", func(p map[string]any) {
		pr := field(t, p, "pull_request")
		field(t, pr, "head")["repo"] = nil
		field(t, pr, "base")["repo"] = nil
	}, "fork 来的 PR，丢弃", 2)
	// 本 App 触发（合并队列更新 PR 分支）：照常规划，不算 Bot；同一个 head 已经规划过
	step("本 App 触发", "synchronize", head2, "main", func(p map[string]any) { withSender(p, testBot, "Bot") }, "新建 0 个 run", 2)

	// 关闭：取消 PR 的全部未结束的 run，移除 ref_heads
	step("关闭", "closed", head2, "main", nil, "PR 已关闭，取消 2 个 run", 2)
	for _, r := range h.runs(demoRepoID) {
		if r.State != "cancelled" {
			t.Fatalf("关闭 PR 后 run %d 状态 %s，期望 cancelled", r.ID, r.State)
		}
	}
	if _, _, ok := h.refHead(demoRepoID, prRef); ok {
		t.Fatal("关闭的 PR 应从 ref_heads 中移除")
	}
	// 重新打开，PR 作者是 Bot：sender 是用户也标记 sender_is_bot
	step("重新打开", "reopened", head3, "main", func(p map[string]any) {
		field(t, p, "pull_request")["user"] = map[string]any{"login": "renovate[bot]", "id": 2, "type": "Bot"}
	}, "新建 1 个 run", 3)
	if r := h.lastRun(demoRepoID, 3); !r.SenderIsBot {
		t.Fatal("Bot 创建的 PR 的 run 应标记 sender_is_bot")
	}
}

// TestPullRequestBaseChanged：目标分支从不匹配改为匹配（edited 且 changes.base 存在）时按推送重新规划。
func TestPullRequestBaseChanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	if got := h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 3, "feature", head, "develop")); got != "新建 0 个 run" {
		t.Fatalf("目标分支不匹配时的结果 %q", got)
	}
	// ref_heads 记下 head 是按哪个目标分支规划的（合并队列只认按默认分支规划过的 head，PLAN #28.3）
	if base := h.refBase(demoRepoID, "refs/pull/3/head"); base != "develop" {
		t.Fatalf("ref_heads 的 base_ref = %q，期望 develop", base)
	}
	p := h.prPayload(h.demo, "edited", 3, "feature", head, "main")
	p["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": "develop"}}}
	if got := h.deliverOK("pull_request", p); got != "新建 1 个 run" {
		t.Fatalf("修改目标分支后的结果 %q", got)
	}
	if r := h.lastRun(demoRepoID, 1); r.Event != workflow.EventPullRequest || r.SHA != head {
		t.Fatalf("run = %+v", r)
	}
	if base := h.refBase(demoRepoID, "refs/pull/3/head"); base != "main" {
		t.Fatalf("改目标分支后 ref_heads 的 base_ref = %q，期望 main", base)
	}
}

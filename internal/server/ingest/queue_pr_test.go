package ingest

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
)

// reviewConfig 是开启 AI 审计的 .kiln/agent/config.yml（PR 事件同时规划 AI 审计请求）。
const reviewConfig = `version: 1
agent: codex
review:
  required_platforms:
    - os: macos
      arch: arm64
`

// planned 返回 PR 的 head 是否按目标分支 base 规划完成（pr_plans）。
func (h *harness) planned(pr int, head, base string) bool {
	h.t.Helper()
	var ok bool
	if err := h.db.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM pr_plans WHERE repo_id=$1 AND pr_number=$2 AND sha=$3 AND base_ref=$4)`,
		demoRepoID, pr, head, base).Scan(&ok); err != nil {
		h.t.Fatal(err)
	}
	return ok
}

// TestPullRequestLateEvent：旧 head 的事件晚到（GitHub 上的 head 已经是更新的 B）时，B 按默认分支规划完成的记录不受影响
// （合并队列据此判断，不看会被晚到事件覆盖的 ref_heads，PLAN #28.3）。
func TestPullRequestLateEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	x := h.git.commit(map[string]string{"f.txt": "x\n"}, "x")
	a := h.git.commit(map[string]string{"f.txt": "a\n"}, "a")
	b := h.git.commit(map[string]string{"f.txt": "b\n"}, "b")

	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", x, "main"))
	h.deliverOK("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", b, "main"))
	h.deliverOK("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", a, "main")) // 晚到的旧事件
	if sha, _, _ := h.refHead(demoRepoID, "refs/pull/7/head"); sha != a {
		t.Fatalf("ref_heads = %s，期望被晚到的事件改成 %s（这正是不能用它判断的原因）", sha, a)
	}
	for _, head := range []string{x, a, b} {
		if !h.planned(7, head, "main") {
			t.Fatalf("%s 应记为按 main 规划完成", head)
		}
	}
	if h.planned(7, b, "release") {
		t.Fatal("没有按 release 规划过")
	}
}

// TestPullRequestPlanFailureNotMarked：规划失败时不记为规划完成（合并队列继续等待），事件稍后重试。
func TestPullRequestPlanFailureNotMarked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	missing := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	if e := h.deliver("pull_request", h.prPayload(h.demo, "opened", 7, "feature", missing, "main")); e.Processed {
		t.Fatalf("事件 = %+v，期望失败后等待重试", e)
	}
	if h.planned(7, missing, "main") {
		t.Fatal("规划失败时不应记为规划完成")
	}
}

// TestPullRequestFromSelfReusesAudit：合并队列更新 PR 分支后，本 App 触发的 synchronize 把"来自本 App"传给审计的规划，
// 新 head 沿用旧 head 的 AI 审计（PLAN #28.4）；同样的提交由其他人触发时照常审计。
func TestPullRequestFromSelfReusesAudit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		sender string
		reuse  bool
	}{{"本 App 触发", testBot, true}, {"其他 Bot 触发", "other[bot]", false}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.git.commit(map[string]string{agentcfg.ConfigPath: reviewConfig}, "开启 AI 审计")
			h.git.git("checkout", "-q", "-b", "feature")
			old := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
			h.git.git("checkout", "-q", "main")
			h.git.commit(map[string]string{"main.txt": "moved\n"}, "main 前进")
			h.git.git("checkout", "-q", "feature")
			h.git.git("merge", "-q", "--no-edit", "main")
			merged := h.git.head("HEAD")

			h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", old, "main"))
			olds := h.reviewRequests(old)
			if len(olds) != 1 {
				t.Fatalf("旧 head 的审计请求 = %+v", olds)
			}
			h.exec(`UPDATE ai_tasks SET state='succeeded', finished_at=now() WHERE request_id=$1`, olds[0].id)
			h.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() WHERE id=$1`, olds[0].id)
			h.exec(`INSERT INTO merge_queue (repo_id, pr_number, head_sha, action, action_head) VALUES ($1,7,$2,'updating',$2)`, demoRepoID, old)

			h.deliverOK("pull_request", withSender(h.prPayload(h.demo, "synchronize", 7, "feature", merged, "main"), tt.sender, "Bot"))
			news := h.reviewRequests(merged)
			if len(news) != 1 {
				t.Fatalf("新 head 的审计请求 = %+v", news)
			}
			reused := news[0].reused
			if reused != tt.reuse {
				t.Fatalf("沿用审计 = %v，期望 %v", reused, tt.reuse)
			}
			if news[0].sender != tt.sender {
				t.Fatalf("审计请求的发送者 = %q，期望 %q", news[0].sender, tt.sender)
			}
		})
	}
}

// TestPullRequestOtherBasePlanned：目标是其他分支（不是默认分支）的 PR 也按它的目标分支记录规划完成，并记为自动入队的
// 候选（PLAN #28.5、#29.5）。目标分支是否允许合并、允许自动合并由执行者按 .kiln/merge.yml 裁决，事件接入不按分支过滤。
func TestPullRequestOtherBasePlanned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "dev")
	h.git.git("checkout", "-q", "main")
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	h.git.git("checkout", "-q", "main")

	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "dev"))
	if !h.planned(7, head, "dev") {
		t.Fatal("应按目标分支 dev 记录规划完成")
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM merge_auto_candidates WHERE repo_id=$1 AND pr_number=7`, demoRepoID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应记为自动入队的候选：%d 条", n)
	}
}

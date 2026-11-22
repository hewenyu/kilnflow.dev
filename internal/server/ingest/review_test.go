package ingest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// reviewPR 开启 AI 审计、在 feature 分支上提交，并投递 PR #7 的 opened 事件；返回 head 和事件结果。
func (h *harness) reviewPR() (head, result string) {
	h.t.Helper()
	h.git.commit(map[string]string{agentcfg.ConfigPath: reviewConfig}, "开启 AI 审计")
	h.git.git("checkout", "-q", "-b", "feature")
	head = h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	return head, h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "main"))
}

// finishReview 把审计请求和它的任务改为 state（模拟审计结束）。
func (h *harness) finishReview(id int64, state, taskState string) {
	h.t.Helper()
	h.exec(`UPDATE ai_tasks SET state=$2, finished_at=now() WHERE request_id=$1`, id, taskState)
	h.exec(`UPDATE ai_requests SET state=$2, finished_at=now() WHERE id=$1`, id, state)
}

// TestPullRequestPlansReview：PR 事件同时规划 CI 和 AI 审计，结果说明两者的数量，并记下按目标分支规划完成。
func TestPullRequestPlansReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head, got := h.reviewPR()
	if got != "新建 1 个 run、1 个 AI 审计请求" {
		t.Fatalf("结果 %q", got)
	}
	reqs := h.reviewRequests(head)
	if len(reqs) != 1 || reqs[0].state != "active" {
		t.Fatalf("审计请求 = %+v", reqs)
	}
	if !h.planned(7, head, "main") {
		t.Fatal("应记为按 main 规划完成")
	}
	// 同一个事件重投不会建出第二个请求
	h.deliverOK("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", head, "main"))
	if n := len(h.reviewRequests(head)); n != 1 {
		t.Fatalf("重复的事件新建了审计请求：%d", n)
	}
}

// TestPullRequestClosedCancelsReview：PR 关闭时取消它进行中的 AI 审计。
func TestPullRequestClosedCancelsReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head, _ := h.reviewPR()
	got := h.deliverOK("pull_request", h.prPayload(h.demo, "closed", 7, "feature", head, "main"))
	if got != "PR 已关闭，取消 1 个 run、1 个 AI 审计请求" {
		t.Fatalf("结果 %q", got)
	}
	if r := h.reviewRequests(head); len(r) != 1 || r[0].state != "cancelled" {
		t.Fatalf("审计请求 = %+v，期望已取消", r)
	}
}

// TestCheckRunRerequestReview：AI 的 Check Run 上的 Re-run 重跑审计：任务的 Check Run 重跑这个任务，请求的 Check Run 重跑
// 没有成功的任务；同一个 delivery 只重跑一次；其他仓库的事件不能重跑。
func TestCheckRunRerequestReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head, _ := h.reviewPR()
	first := h.reviewRequests(head)[0]
	h.finishReview(first.id, "failed", "failed")
	var taskID int64
	if err := h.db.QueryRow(h.ctx, `SELECT id FROM ai_tasks WHERE request_id=$1`, first.id).Scan(&taskID); err != nil {
		t.Fatal(err)
	}

	d := h.newDelivery()
	h.send("check_run", d, h.checkRunPayload(fmt.Sprintf("ai-task:%d", taskID)))
	h.process()
	if e := h.event(d); !e.Processed || e.Result != fmt.Sprintf("重跑 AI 审计（请求 #%d）", first.id+1) {
		t.Fatalf("事件 = %+v", e)
	}
	// 同一个 delivery 重试：不再重跑
	h.exec(`UPDATE events SET processed_at=NULL, next_attempt_at=now() WHERE delivery_id=$1`, d)
	h.process()
	if n := len(h.reviewRequests(head)); n != 2 {
		t.Fatalf("同一个 delivery 重跑了 %d 次", n-1)
	}

	other, otherGit := h.addRepo(202, "other", 0, true)
	h.deliverOK("push", h.pushPayload(other, "refs/heads/feature", gitref.ZeroSHA, otherGit.head("main")))
	p := h.fake.RepositoryPayload(other, "rerequested")
	p["check_run"] = map[string]any{"external_id": fmt.Sprintf("ai-request:%d", first.id)}
	if got := h.deliverOK("check_run", p); got != "找不到 AI 审计" {
		t.Fatalf("其他仓库的事件：%q", got)
	}
	if got := h.deliverOK("check_run", h.checkRunPayload("ai-request:987654")); got != "找不到 AI 审计" {
		t.Fatalf("不存在的请求：%q", got)
	}
}

// TestCheckSuiteRerequestReview：Check Suite 上的 "Re-run all checks" 同时重跑该 commit 上没有成功的 AI 审计。
func TestCheckSuiteRerequestReview(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head, _ := h.reviewPR()
	first := h.reviewRequests(head)[0]
	h.finishReview(first.id, "failed", "failed")
	for _, r := range h.runs(demoRepoID) {
		h.cancelRun(r.ID)
	}
	got := h.deliverOK("check_suite", h.fake.CheckSuitePayload(h.demo, "rerequested", head))
	if got != "重跑 1 个 run、1 个 AI 审计请求" {
		t.Fatalf("结果 %q", got)
	}
	if n := len(h.reviewRequests(head)); n != 2 {
		t.Fatalf("审计请求 %d 个，期望重跑出第 2 个", n)
	}
}

// Check Run 的 Re-run：读数据库失败时返回错误让事件稍后重试，不当作无效的 external_id 丢弃；AI 格式但 ID 无效的
// external_id 才丢弃。
func TestRerequestResolveErrorRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	repo := &store.Repo{ID: demoRepoID}
	canceled, cancel := context.WithCancel(h.ctx)
	cancel()
	if msg, err := h.ing.handleRerequest(canceled, "d-1", repo, "job:1", "alice"); err == nil {
		t.Fatalf("读数据库失败时应返回错误（事件重试），实际丢弃：%q", msg)
	}
	if msg, err := h.ing.handleRerequest(h.ctx, "d-2", repo, "ai-task:x", "alice"); err != nil || !strings.Contains(msg, "external_id 无效") {
		t.Fatalf("格式无效的 AI external_id 应丢弃：%q、%v", msg, err)
	}
}

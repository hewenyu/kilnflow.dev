package review

import (
	"errors"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// finishedTwoTasks 规划 PR #7 的两平台审计并结束：m1 通过、m2 发现问题。返回请求和两个任务（m1、m2）。
func (h *harness) finishedTwoTasks() (int64, *ai.Task, *ai.Task) {
	h.t.Helper()
	runner := h.addRunner("review1", "host-review")
	base := h.enableReview(twoPlatformConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	reqID := h.planPR(h.prEvent("opened", head, base))
	ts := h.tasks(reqID)
	if len(ts) != 2 || ts[0].Key != "review-m1" || ts[1].Key != "review-m2" {
		h.t.Fatalf("任务 = %+v", ts)
	}
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); !errors.Is(err, ErrNotFinished) {
		h.t.Fatalf("还没有结束时重跑：%v，应为 ErrNotFinished", err)
	}
	h.finish(ts[0].ID, runner, reportResult(cleanReport()))
	h.finish(ts[1].ID, runner, reportResult(testReport()))
	if r := h.request(reqID); r.State != ai.RequestFailed {
		h.t.Fatalf("请求 = %s，应为 failed", r.State)
	}
	return reqID, ts[0], ts[1]
}

// wantTask 检查任务的状态和沿用来源（from 为 0 表示不沿用）。
func wantTask(t *testing.T, got *ai.Task, state ai.TaskState, from int64) {
	t.Helper()
	switch {
	case got.State != state:
		t.Errorf("任务 %s = %s，应为 %s", got.Key, got.State, state)
	case from == 0 && got.ReusedFrom != nil:
		t.Errorf("任务 %s 不应沿用（沿用了 #%d）", got.Key, *got.ReusedFrom)
	case from != 0 && (got.ReusedFrom == nil || *got.ReusedFrom != from):
		t.Errorf("任务 %s 沿用 %v，应沿用 #%d", got.Key, got.ReusedFrom, from)
	}
}

// TestRerun_modes：重跑建新请求（rerun_of 指向被重跑的请求，family_id 指向首次请求，明细沿用 PR 和 base），按方式沿用：
// 全部重跑都重新执行；只重跑失败的沿用通过的任务；重跑单个任务沿用其余任务（不论结果）。
func TestRerun_modes(t *testing.T) {
	h := newHarness(t)
	reqID, m1, m2 := h.finishedTwoTasks()

	all, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob", Source: store.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	if all.ID == reqID || all.RerunOf == nil || *all.RerunOf != reqID || all.FamilyID != reqID || all.State != ai.RequestActive || all.SHA != h.request(reqID).SHA {
		t.Fatalf("重跑的请求 = %+v", all)
	}
	ts := h.tasks(all.ID)
	wantTask(t, ts[0], ai.TaskPending, 0)
	wantTask(t, ts[1], ai.TaskPending, 0)
	var pr int
	var headRef, baseRef, baseSHA string
	if err := h.db.QueryRow(h.ctx, `SELECT pr_number, head_ref, base_ref, base_sha FROM ai_review_requests WHERE request_id=$1`, all.ID).
		Scan(&pr, &headRef, &baseRef, &baseSHA); err != nil {
		t.Fatal(err)
	}
	if pr != aiPR || headRef != featureRef || baseRef != "main" || baseSHA == "" {
		t.Fatalf("重跑的明细 = PR %d、%s、%s、%s", pr, headRef, baseRef, baseSHA)
	}
	if n := h.queryInt(`SELECT count(*) FROM audit_log WHERE action='ai_request.rerun'`); n != 1 {
		t.Fatalf("审计日志 %d 条", n)
	}

	failed, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunFailed, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	ts = h.tasks(failed.ID)
	wantTask(t, ts[0], ai.TaskSucceeded, m1.ID)
	wantTask(t, ts[1], ai.TaskPending, 0)

	one, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunTask, TaskID: m1.ID, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	ts = h.tasks(one.ID)
	wantTask(t, ts[0], ai.TaskPending, 0)
	wantTask(t, ts[1], ai.TaskFailed, m2.ID)

	// 重跑不取代同一 head 的其他审计（只有新 head 的首次审计取代）
	for _, id := range []int64{all.ID, failed.ID, one.ID} {
		if r := h.request(id); r.State != ai.RequestActive {
			t.Errorf("请求 #%d = %s，重跑之间不应互相取代", id, r.State)
		}
	}
	// 重跑的请求也能再重跑：沿用不叠加，来源是最初的任务
	h.exec(`UPDATE ai_tasks SET state='failed', finished_at=now() WHERE request_id=$1 AND state='pending'`, failed.ID)
	h.exec(`UPDATE ai_requests SET state='failed', finished_at=now() WHERE id=$1`, failed.ID)
	again, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: failed.ID, Mode: RerunFailed, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if again.FamilyID != reqID {
		t.Errorf("再次重跑的 family_id = %d，应为首次请求 %d", again.FamilyID, reqID)
	}
	wantTask(t, h.tasks(again.ID)[0], ai.TaskSucceeded, m1.ID)
}

// TestRerun_errors：不属于请求的任务、不存在的请求、开发请求不能重跑。
func TestRerun_errors(t *testing.T) {
	h := newHarness(t)
	reqID, _, _ := h.finishedTwoTasks()
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunTask, TaskID: reqID + 1000}); err == nil {
		t.Fatal("不属于请求的任务应返回错误")
	}
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID + 1000, Mode: RerunAll}); !errors.Is(err, ai.ErrNotFound) {
		t.Fatalf("不存在的请求：%v", err)
	}
	dev, _ := h.insertRequest(fixture{typ: agentcfg.TaskDevelop, event: eventManual, sha: "x", state: ai.RequestFailed})
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: dev, Mode: RerunAll}); !errors.Is(err, ai.ErrNotRerunnable) {
		t.Fatalf("开发请求：%v", err)
	}
	if n := h.requestCount(); n != 1 {
		t.Fatalf("出错时不应建请求：%d 个审计请求", n)
	}
}

// TestRerun_delivery：GitHub 触发的重跑按 webhook delivery 幂等：同一个 delivery 对同一个请求族至多重跑一次，重试时返回
// 已有的请求（先于"还没有结束"的检查：重试时被重跑的请求可能正是这次建的、还在执行）。
func TestRerun_delivery(t *testing.T) {
	h := newHarness(t)
	reqID, _, _ := h.finishedTwoTasks()
	first, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunFailed, Actor: "gh", Source: store.SourceWebhook, DeliveryID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunFailed, Actor: "gh", Source: store.SourceWebhook, DeliveryID: "d1"})
	if err != nil || again.ID != first.ID {
		t.Fatalf("同一个 delivery 重试 = %v（%v），应返回 #%d", again, err, first.ID)
	}
	// 重试时按族里的请求（执行中的重跑）找到：不返回 ErrNotFinished
	same, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: first.ID, Mode: RerunFailed, Actor: "gh", Source: store.SourceWebhook, DeliveryID: "d1"})
	if err != nil || same.ID != first.ID {
		t.Fatalf("按族去重 = %v（%v），应返回 #%d", same, err, first.ID)
	}
	if n := h.requestCount(); n != 2 {
		t.Fatalf("审计请求 %d 个，应为 2", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM audit_log WHERE action='ai_request.rerun'`); n != 1 {
		t.Fatalf("重复的 delivery 不应再记审计日志：%d 条", n)
	}
	// 另一个 delivery：再重跑一次（被重跑的请求已经结束时）
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunFailed, Actor: "gh", Source: store.SourceWebhook, DeliveryID: "d2"}); err != nil {
		t.Fatal(err)
	}
	if n := h.requestCount(); n != 3 {
		t.Fatalf("审计请求 %d 个，应为 3", n)
	}
}

// TestRerunHead：Check Suite 的 "Re-run all checks" 重跑 commit 上每个请求族最新、已结束的审计（只重跑失败的）；未结束、
// 跳过的不重跑；同一个 delivery 重试不再重跑。
func TestRerunHead(t *testing.T) {
	h := newHarness(t)
	reqID, m1, _ := h.finishedTwoTasks()
	sha := h.request(reqID).SHA
	h.insertRequest(fixture{sha: sha, pr: aiPR, baseRef: "main", state: ai.RequestSkipped})
	h.insertRequest(fixture{sha: sha, pr: aiPR, baseRef: "main", state: ai.RequestActive})

	n, err := h.rev.RerunHead(h.ctx, testRepoID, sha, "gh", "d1")
	if err != nil || n != 1 {
		t.Fatalf("RerunHead = %d（%v），应重跑 1 个", n, err)
	}
	reruns, err := ai.QueryRequests(h.ctx, h.db, `rerun_of=$1`, reqID)
	if err != nil || len(reruns) != 1 {
		t.Fatalf("重跑：%v（%v）", reruns, err)
	}
	if reruns[0].DeliveryID == nil || *reruns[0].DeliveryID != "d1" || reruns[0].Source != store.SourceWebhook {
		t.Fatalf("重跑的请求 = %+v", reruns[0])
	}
	wantTask(t, h.tasks(reruns[0].ID)[0], ai.TaskSucceeded, m1.ID)

	// 重试：族里最新的是刚建的重跑（还在执行），不再重跑
	if n, err := h.rev.RerunHead(h.ctx, testRepoID, sha, "gh", "d1"); err != nil || n != 0 {
		t.Fatalf("重试 = %d（%v）", n, err)
	}
	if n := h.requestCount(); n != 4 {
		t.Fatalf("审计请求 %d 个，应为 4", n)
	}
}

// TestRerequest：Check Run 的 Re-run：任务的 Check Run 重跑这个任务，请求的 Check Run 重跑没有成功的任务；对象不存在或
// 不属于事件的仓库时 found=false，不重跑。
func TestRerequest(t *testing.T) {
	h := newHarness(t)
	reqID, m1, m2 := h.finishedTwoTasks()

	r, found, err := h.rev.Rerequest(h.ctx, testRepoID, ai.CheckTarget{TaskID: m1.ID}, "gh", "d1")
	if err != nil || !found {
		t.Fatalf("任务的 Re-run：%v（found=%v）", err, found)
	}
	ts := h.tasks(r.ID)
	wantTask(t, ts[0], ai.TaskPending, 0)
	wantTask(t, ts[1], ai.TaskFailed, m2.ID)

	r, found, err = h.rev.Rerequest(h.ctx, testRepoID, ai.CheckTarget{RequestID: reqID}, "gh", "d2")
	if err != nil || !found {
		t.Fatalf("请求的 Re-run：%v（found=%v）", err, found)
	}
	ts = h.tasks(r.ID)
	wantTask(t, ts[0], ai.TaskSucceeded, m1.ID)
	wantTask(t, ts[1], ai.TaskPending, 0)

	before := h.requestCount()
	for _, c := range []struct {
		repo   int64
		target ai.CheckTarget
	}{
		{testRepoID + 1, ai.CheckTarget{RequestID: reqID}},
		{testRepoID + 1, ai.CheckTarget{TaskID: m1.ID}},
		{testRepoID, ai.CheckTarget{TaskID: m1.ID + 1000}},
		{testRepoID, ai.CheckTarget{RequestID: reqID + 1000}},
	} {
		if _, found, err := h.rev.Rerequest(h.ctx, c.repo, c.target, "gh", "d3"); err != nil || found {
			t.Errorf("仓库 %d、%+v：found=%v（%v），应找不到", c.repo, c.target, found, err)
		}
	}
	if n := h.requestCount(); n != before {
		t.Fatalf("找不到时不应重跑：%d → %d", before, n)
	}
}

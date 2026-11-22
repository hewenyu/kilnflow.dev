package review

import (
	"errors"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// 迁移前的请求族在重跑时由 legacyFamily 固化。这里走完整链路：固化 → 原旧任务的任务页、工作项关联关系、
// 正文接口都按固化后的依据解析，不把合法的历史任务误判为"依据冲突"。

// viewCore 给 harness 的 AI 核心装上只读文档视图（只查数据库，不访问外部文档系统）。
func (h *harness) viewCore() {
	h.core.SetDocs(docsync.New(docsync.Deps{DB: h.db, MasterKey: docsKey}))
}

// legacyReviewTask 把请求与它的任务改成迁移前的形态：请求没有记录来源，任务 spec 没有 #205 的字段。
// 返回任务的 id。
func (h *harness) legacyReviewTask(reqID int64) int64 {
	h.t.Helper()
	h.exec(`UPDATE ai_review_requests SET docs_source='legacy', doc_snapshot_set_id=NULL, docs_manifest_digest=''
		WHERE request_id=$1`, reqID)
	h.exec(`UPDATE ai_tasks SET spec = spec - 'docs_source' - 'doc_snapshot_set_id' - 'docs_manifest_digest'
		WHERE request_id=$1`, reqID)
	tasks := h.tasks(reqID)
	if len(tasks) != 1 {
		h.t.Fatalf("请求 #%d 的任务数 = %d，期望 1", reqID, len(tasks))
	}
	return tasks[0].ID
}

// workItemOfTask 返回任务 spec 中的 work_item_id。
func (h *harness) workItemOfTask(taskID int64) int64 {
	h.t.Helper()
	var wi int64
	if err := h.db.QueryRow(h.ctx, `SELECT coalesce((spec->>'work_item_id')::bigint, 0) FROM ai_tasks WHERE id=$1`, taskID).Scan(&wi); err != nil {
		h.t.Fatal(err)
	}
	return wi
}

func (h *harness) freezeWorkItemDocs(workItemID int64, docID, content string) {
	h.t.Helper()
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, $2, 'a', 1, $3, $4, 'u1')`, workItemID, docID, docprovider.Sum(content), content)
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, workItemID)
}

// TestLegacyRerunPinsWorkItemView：族根固化为 work_item 后，原旧任务的详情、关联关系与正文接口都读同一份
// 工作项快照，关系标为 legacy_pinned。
func TestLegacyRerunPinsWorkItemView(t *testing.T) {
	h := newHarness(t)
	h.docsService(&docFetch{})
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	h.freezeWorkItemDocs(wi, "d1", "正文")
	reqID := h.planPR(h.prEvent("opened", head, base))
	h.completeRequest("legacy-view", reqID)
	taskID := h.legacyReviewTask(reqID)
	if wiOfTask := h.workItemOfTask(taskID); wiOfTask != wi {
		t.Fatalf("旧任务 spec 的工作项 = %d，期望 %d", wiOfTask, wi)
	}

	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); err != nil {
		t.Fatalf("重跑迁移前的请求族：%v", err)
	}
	if source, _, _, _ := h.reviewDocs(reqID); source != docsync.SourceWorkItem {
		t.Fatalf("族根应固化为 work_item，实际 %s", source)
	}

	h.viewCore()
	// 旧请求页：记录的来源就是固化后的 work_item，依据可读、没有冲突。
	reqView, err := h.core.RequestDocsView(h.ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if reqView.Error != "" || reqView.View == nil || reqView.View.Source != docsync.SourceWorkItem {
		t.Fatalf("固化后的请求依据 = %+v", reqView)
	}
	// 旧任务页：spec 仍是迁移前格式，但按任务记录中的引用核对工作项快照后是合法依据。
	taskView, err := h.core.TaskDocsView(h.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if taskView.Error != "" || taskView.BasisSource != docsync.SourceWorkItem || taskView.View == nil {
		t.Fatalf("旧任务的依据 = %+v", taskView)
	}
	// 正文接口照常可读。
	d, err := h.core.TaskDocContent(h.ctx, taskID, "d1")
	if err != nil || d == nil || d.Content != "正文" {
		t.Fatalf("TaskDocContent = %+v，%v", d, err)
	}
	// 工作项页的关联关系：旧审计任务标为迁移前核对一致。
	list, err := h.core.RelatedTasks(h.ctx, wi)
	if err != nil {
		t.Fatal(err)
	}
	rel := ""
	for _, v := range list {
		if v.TaskID == taskID {
			rel = v.Relation
		}
	}
	if rel != "legacy_pinned" {
		t.Fatalf("旧审计任务的关系 = %q，期望 legacy_pinned（全部：%+v）", rel, list)
	}
}

// TestLegacyRerunPinsNoneView：族根固化为 none 后，原旧任务没有依据但也不报冲突；正文接口明确返回没有依据。
func TestLegacyRerunPinsNoneView(t *testing.T) {
	h := newHarness(t)
	h.docsService(&docFetch{})
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	reqID := h.planPR(h.prEvent("opened", head, base))
	h.completeRequest("legacy-none-view", reqID)
	taskID := h.legacyReviewTask(reqID)
	if wiOfTask := h.workItemOfTask(taskID); wiOfTask != wi {
		t.Fatalf("旧任务 spec 的工作项 = %d，期望 %d", wiOfTask, wi)
	}

	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); err != nil {
		t.Fatalf("重跑迁移前的请求族：%v", err)
	}
	if source, _, _, _ := h.reviewDocs(reqID); source != docsync.SourceNone {
		t.Fatalf("族根应固化为 none，实际 %s", source)
	}

	h.viewCore()
	taskView, err := h.core.TaskDocsView(h.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if taskView.Error != "" || taskView.BasisSource != docsync.SourceNone {
		t.Fatalf("旧任务的依据 = %+v", taskView)
	}
	if _, err := h.core.TaskDocContent(h.ctx, taskID, "d1"); !errors.Is(err, ai.ErrDocsBasisMissing) {
		t.Fatalf("没有依据时读取正文应返回 ErrDocsBasisMissing：%v", err)
	}
	list, err := h.core.RelatedTasks(h.ctx, wi)
	if err != nil {
		t.Fatal(err)
	}
	rel := ""
	for _, v := range list {
		if v.TaskID == taskID {
			rel = v.Relation
		}
	}
	if rel != "no_basis" {
		t.Fatalf("旧审计任务的关系 = %q，期望 no_basis（全部：%+v）", rel, list)
	}
}

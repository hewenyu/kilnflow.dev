package review

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// TestReviewSpecIncludesFrozenDocs：工作项已有冻结快照时，审计规划产生的任务 spec 必须带上这些文档（ID、标题、修订、摘要），
// 来源记为 work_item。这是通过真实规划链路（PlanEvent → build → WorkItemBasis）验证「开发与审计读到同一份」；
// 删除 Docs 传递或改按当前仓库重新冻结时本测试必须失败。
func TestReviewSpecIncludesFrozenDocs(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	sha := docprovider.Sum("正文")
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', '需求文档', 2, $2, '正文', 'https://outline.example/doc/d1')`, wi, sha)
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	// 与组装层相同：给规划器注入文档服务（审计只读取已冻结的快照，不重新拉取）。
	h.rev.docs = docsync.New(docsync.Deps{DB: h.db, MasterKey: make([]byte, 32)})

	spec := h.specOf(h.prEvent("opened", head, base))
	if len(spec.Docs) != 1 {
		t.Fatalf("审计 spec 应带 1 篇冻结文档，实际 %+v", spec.Docs)
	}
	d := spec.Docs[0]
	if d.ID != "d1" || d.Title != "需求文档" || d.Revision != 2 || d.SHA256 != sha {
		t.Fatalf("文档引用 = %+v", d)
	}
	if spec.DocsSource != docsync.SourceWorkItem || spec.DocsManifestDigest == "" {
		t.Fatalf("文档来源 = %q，摘要 = %q", spec.DocsSource, spec.DocsManifestDigest)
	}
}

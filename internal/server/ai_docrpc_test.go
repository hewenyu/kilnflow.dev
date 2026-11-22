package server

import (
	"testing"

	"connectrpc.com/connect"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// TestDocRPCAuthAndSnapshot：外部设计文档 RPC（ListTaskDocs / GetTaskDoc）必须校验身份和 attempt 归属/epoch；
// 合法读取返回该工作项冻结的文档。删除 authorize 或任务归属校验时本测试必须失败。
func TestDocRPCAuthAndSnapshot(t *testing.T) {
	p := newSplitPort(t, portConfig)
	const pr = 7
	head := p.prHead(pr)
	wi := p.addWorkItem(pr, head)
	sha := docprovider.Sum("正文")
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', '需求文档', 2, $2, '正文', 'https://outline.example/doc/d1')`, wi, sha)
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	// 另一个工作项的文档：本任务的正文 RPC 不能读到它（任务隔离）。
	wi2 := p.addWorkItem(8, head)
	sha2 := docprovider.Sum("别的")
	p.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd2', '别的需求', 1, $2, '别的', 'https://outline.example/doc/d2')`, wi2, sha2)
	p.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi2)

	req := p.planReview(pr, head)
	// 规划时工作项还没有冻结，记录为 none；这里把该请求改为引用工作项快照（与 detail 写入的形态一致），
	// 让本测试专注在 RPC 的授权与隔离上。
	digest := docsync.ManifestDigest(docsync.SourceWorkItem, docsync.ManifestSource{Unknown: true}, []docsync.Snapshot{
		{DocID: "d1", Title: "需求文档", Revision: 2, SHA256: sha, URL: "https://outline.example/doc/d1"},
	})
	p.exec(`UPDATE ai_review_requests SET docs_source='work_item', docs_manifest_digest=$2 WHERE request_id=$1`, req.id, digest)
	p.exec(`UPDATE ai_tasks SET spec = spec || jsonb_build_object('docs_source','work_item','docs_manifest_digest',$2::text)
		WHERE request_id=$1`, req.id, digest)

	r := p.runner(runnerSpec{})
	task, err := p.acquire(r, "doc-rpc-1")
	if err != nil || task == nil {
		t.Fatalf("领取审计任务：%v %+v", err, task)
	}
	tref := ref(task)

	// 没有 Authorization：拒绝。
	if _, err := p.ai.ListTaskDocs(p.ctx, connect.NewRequest(&airunnerv2.ListTaskDocsRequest{Ref: tref})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("无认证应 Unauthenticated，得到 %v", err)
	}
	// 别的节点的 token：attempt 不属于它，拒绝。
	other := p.runner(runnerSpec{})
	if _, err := p.ai.ListTaskDocs(p.ctx, withToken(other.token, &airunnerv2.ListTaskDocsRequest{Ref: tref})); err == nil {
		t.Fatal("别的节点不应读到这个任务的文档")
	}
	// 旧 epoch：拒绝。
	stale := &airunnerv2.TaskRef{AttemptId: task.attempt, Epoch: task.epoch + 1}
	if _, err := p.ai.ListTaskDocs(p.ctx, withToken(r.token, &airunnerv2.ListTaskDocsRequest{Ref: stale})); err == nil {
		t.Fatal("旧 epoch 不应读到文档")
	}
	// 合法：列出该工作项冻结的文档。
	lr, err := p.ai.ListTaskDocs(p.ctx, withToken(r.token, &airunnerv2.ListTaskDocsRequest{Ref: tref}))
	if err != nil {
		t.Fatalf("ListTaskDocs：%v", err)
	}
	if len(lr.Msg.Docs) != 1 || lr.Msg.Docs[0].Id != "d1" || lr.Msg.Docs[0].Revision != 2 || lr.Msg.Docs[0].Title != "需求文档" {
		t.Fatalf("ListTaskDocs = %+v", lr.Msg.Docs)
	}
	// 合法：读取正文。
	gr, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: tref, DocId: "d1"}))
	if err != nil || gr.Msg.Content != "正文" {
		t.Fatalf("GetTaskDoc = %+v, %v", gr.Msg, err)
	}
	// 返回正文的 RPC 同样要校验身份、归属与 epoch（不能只靠列表 RPC 的测试）。
	if _, err := p.ai.GetTaskDoc(p.ctx, connect.NewRequest(&airunnerv2.GetTaskDocRequest{Ref: tref, DocId: "d1"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("GetTaskDoc 无认证应 Unauthenticated，得到 %v", err)
	}
	if _, err := p.ai.GetTaskDoc(p.ctx, withToken(other.token, &airunnerv2.GetTaskDocRequest{Ref: tref, DocId: "d1"})); err == nil {
		t.Fatal("别的节点不应读到这个任务的正文")
	}
	if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: stale, DocId: "d1"})); err == nil {
		t.Fatal("旧 epoch 不应读到正文")
	}
	// 任务隔离：本任务读不到别的工作项的文档。
	if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: tref, DocId: "d2"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("读别的工作项的文档应 NotFound，得到 %v", err)
	}
	// 不存在的文档：NotFound。
	if _, err := p.ai.GetTaskDoc(p.ctx, withToken(r.token, &airunnerv2.GetTaskDocRequest{Ref: tref, DocId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("不存在的文档应 NotFound，得到 %v", err)
	}
}

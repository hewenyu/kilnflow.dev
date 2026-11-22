package admin

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
)

// 「待决策清单」的 Admin 接口：参数的校验、错误码、列表不泄露 token。

const (
	decisionDocID  = "44444444-4444-4444-8444-444444444444"
	decisionSecret = "outline-write-token-0123456789"
	decisionBase   = "https://outline.example.com"
)

// fakeDecisionWriter 是固定返回一篇文档的写接口。
type fakeDecisionWriter struct{ content string }

func (w *fakeDecisionWriter) GetDocument(context.Context, string) (docprovider.Document, error) {
	content := w.content
	if content == "" {
		content = "## 待拍板\n\n"
	}
	return docprovider.Document{
		ID: decisionDocID, Title: "待拍板", Content: content, Revision: 3,
		CollectionID: "col-decision", Published: true,
	}, nil
}

func (w *fakeDecisionWriter) UpdateDocument(_ context.Context, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
	w.content = req.Text
	return docprovider.Document{ID: decisionDocID, Content: req.Text, Revision: req.LastRevision + 1, CollectionID: "col-decision", Published: true}, nil
}

// withDecision 给测试环境装上真实的同步服务（写接口是假的，不访问网络）。
func (e *testEnv) withDecision() *fakeDecisionWriter {
	e.t.Helper()
	w := &fakeDecisionWriter{}
	e.svc.decision = decisioninbox.New(decisioninbox.Deps{
		DB: e.db, MasterKey: testMasterKey, Log: slog.New(slog.DiscardHandler),
		Writer: func(string, string) (docprovider.DocumentWriter, error) { return w, nil },
	})
	return w
}

// TestDecisionDocRPC_RequiresAdminToken：与其它 Admin 接口一致，没有 admin token 的请求被拒绝。
func TestDecisionDocRPC_RequiresAdminToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDecision()
	if _, err := e.rpcClient("").ListDecisionDocs(t.Context(), req(&apiv1.ListDecisionDocsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("没有 token 的请求 = %v", err)
	}
}

// TestDecisionDocRPC_SetListClear：设置时用 token 只读校验目标文档，保存后列表里只有元数据（不含 token），
// 审计日志也不记录 token；清除后回到未配置。
func TestDecisionDocRPC_SetListClear(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDecision()
	c := e.rpcClient(testAdminToken)

	set, err := c.SetDecisionDoc(t.Context(), req(&apiv1.SetDecisionDocRequest{
		Repo: "tester/demo", BaseUrl: decisionBase, DocId: decisionDocID, Token: decisionSecret,
	}))
	if err != nil {
		t.Fatalf("SetDecisionDoc：%v", err)
	}
	doc := set.Msg.DecisionDoc
	if doc.Repo != "tester/demo" || doc.DocId != decisionDocID || doc.CollectionId != "col-decision" || !doc.HasToken {
		t.Fatalf("返回 = %+v", doc)
	}
	if set.Msg.CancelledCount != 0 || set.Msg.UnknownCount != 0 {
		t.Errorf("首次设置不应影响条目：%+v", set.Msg)
	}

	list, err := c.ListDecisionDocs(t.Context(), req(&apiv1.ListDecisionDocsRequest{}))
	if err != nil {
		t.Fatalf("ListDecisionDocs：%v", err)
	}
	if len(list.Msg.DecisionDocs) != 1 || list.Msg.DecisionDocs[0].DocId != decisionDocID {
		t.Fatalf("列表 = %+v", list.Msg.DecisionDocs)
	}
	// 列表和审计日志都不含 token。
	if text := list.Msg.DecisionDocs[0].String(); strings.Contains(text, decisionSecret) {
		t.Error("列表中出现了 token")
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE detail::text LIKE '%' || $1 || '%'`, decisionSecret); n != 0 {
		t.Error("审计日志中出现了 token")
	}
	// 密文里也不应出现明文。
	if n := e.count(`SELECT count(*) FROM repo_decision_docs WHERE encode(token_ciphertext, 'escape') LIKE '%' || $1 || '%'`, decisionSecret); n != 0 {
		t.Error("密文中出现了明文 token")
	}

	cleared, err := c.ClearDecisionDoc(t.Context(), req(&apiv1.ClearDecisionDocRequest{Repo: "tester/demo"}))
	if err != nil {
		t.Fatalf("ClearDecisionDoc：%v", err)
	}
	if cleared.Msg.CancelledCount != 0 || cleared.Msg.UnknownCount != 0 {
		t.Errorf("清除计数 = %+v", cleared.Msg)
	}
	list, err = c.ListDecisionDocs(t.Context(), req(&apiv1.ListDecisionDocsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.DecisionDocs) != 0 {
		t.Errorf("清除后仍有配置：%+v", list.Msg.DecisionDocs)
	}
}

// TestDecisionDocRPC_InvalidArguments：参数不合法时返回 InvalidArgument（CLI 据此显示原因）。
func TestDecisionDocRPC_InvalidArguments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDecision()
	c := e.rpcClient(testAdminToken)
	for name, m := range map[string]*apiv1.SetDecisionDocRequest{
		"文档 id 不是 UUID": {Repo: "tester/demo", BaseUrl: decisionBase, DocId: "x", Token: decisionSecret},
		"token 为空":      {Repo: "tester/demo", BaseUrl: decisionBase, DocId: decisionDocID},
		"地址不是 http":     {Repo: "tester/demo", BaseUrl: "ftp://x", DocId: decisionDocID, Token: decisionSecret},
	} {
		if _, err := c.SetDecisionDoc(t.Context(), req(m)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s：%v", name, err)
		}
	}
	if _, err := c.ClearDecisionDoc(t.Context(), req(&apiv1.ClearDecisionDocRequest{Repo: "tester/missing"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("不存在的仓库：%v", err)
	}
}

// seedDecisionWork 写入同步条目依赖的最小数据（请求、任务、attempt、工作项）。
func (e *testEnv) seedDecisionWork() {
	e.t.Helper()
	for _, sql := range []string{
		`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
		 VALUES (11, 'develop', 501, 'manual', 'refs/heads/main', 'a', 11, 'succeeded')`,
		`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		 VALUES (12, 11, 'develop', 'develop', '{macos-arm64}', 'codex', '开发', '{}', 'succeeded', 60)`,
		`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (13, 12, 1, 'node', 'acq1')`,
		`INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id,
			task_id, phase, impl_round) VALUES (14, 501, 'developing', 'main', 'a', 2, 'codex', 13, 12, 'implement', 1)`,
	} {
		e.exec(sql)
	}
}

// seedDecisionSync 写入一条 unknown 的同步记录，返回它的 id。
func (e *testEnv) seedDecisionSync(entryIdx int) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO dev_decision_syncs (repo_id, work_item_id, task_id, entry_idx,
			target_provider, target_base_url, target_doc_id, target_collection_id, target_generation, marker,
			entry_kind, category, report_sha256, payload, state, attempts, payload_md, attempted_revision,
			attempted_at, next_retry_at)
		VALUES (501, 14, 12, $3::int, 'outline', $1, $2, 'col-decision', 1,
			'kiln-dec:501:wi14:t12:' || ($3::int)::text, 'decision', 'other', repeat('a', 64), '{}', 'unknown', 1, '正文', 3, now(), NULL)
		RETURNING id`, decisionBase, decisionDocID, entryIdx).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// TestDecisionSyncRPC_ReconcileAndResolve：unknown 条目可以在 Admin 接口上只读对账、人工结案；列表带仓库名。
func TestDecisionSyncRPC_ReconcileAndResolve(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.withDecision()
	if _, _, _, err := e.svc.SetDecisionDoc(t.Context(), "tester/demo", decisionBase, decisionDocID, decisionSecret, "tester"); err != nil {
		t.Fatalf("配置：%v", err)
	}
	e.seedDecisionWork()
	id := e.seedDecisionSync(0)
	c := e.rpcClient(testAdminToken)

	list, err := c.ListDecisionSyncs(t.Context(), req(&apiv1.ListDecisionSyncsRequest{Repo: "tester/demo", State: "unknown"}))
	if err != nil {
		t.Fatalf("ListDecisionSyncs：%v", err)
	}
	if len(list.Msg.Syncs) != 1 || list.Msg.Syncs[0].Repo != "tester/demo" || list.Msg.Syncs[0].State != "unknown" {
		t.Fatalf("列表 = %+v", list.Msg.Syncs)
	}

	// 文档里没有 marker：对账保持 unknown，且不写文档。
	rec, err := c.ReconcileDecisionSync(t.Context(), req(&apiv1.ReconcileDecisionSyncRequest{Id: id}))
	if err != nil {
		t.Fatalf("ReconcileDecisionSync：%v", err)
	}
	if rec.Msg.Found || rec.Msg.Sync.State != "unknown" {
		t.Fatalf("对账 = %+v", rec.Msg)
	}
	// 文档里有 marker 时转为 delivered。
	w.content = "## 待拍板\n\n`kiln-dec:501:wi14:t12:0`\n"
	rec, err = c.ReconcileDecisionSync(t.Context(), req(&apiv1.ReconcileDecisionSyncRequest{Id: id}))
	if err != nil || !rec.Msg.Found || rec.Msg.Sync.State != "delivered" {
		t.Fatalf("对账（已送达）= %+v, %v", rec.Msg, err)
	}

	// 已结案的条目不能再人工结案；unknown 结案必须写明原因。
	if _, err := c.ResolveDecisionSync(t.Context(), req(&apiv1.ResolveDecisionSyncRequest{Id: id, State: "cancelled", Note: "再改"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("已结案的条目：%v", err)
	}
	other := e.seedDecisionSync(1)
	if _, err := c.ResolveDecisionSync(t.Context(), req(&apiv1.ResolveDecisionSyncRequest{Id: other, State: "cancelled"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("没有原因的结案：%v", err)
	}
	res, err := c.ResolveDecisionSync(t.Context(), req(&apiv1.ResolveDecisionSyncRequest{Id: other, State: "cancelled", Note: "确认不需要"}))
	if err != nil {
		t.Fatalf("ResolveDecisionSync：%v", err)
	}
	if res.Msg.Sync.State != "cancelled" || res.Msg.Sync.ResolvedBy == "" || res.Msg.Sync.ResolutionNote != "确认不需要" {
		t.Errorf("结案 = %+v", res.Msg.Sync)
	}
}

// 只读绑定与待决策清单的集合冲突检查按规范化后的地址比较：大小写、默认端口和结尾斜杠不能绕过。
func TestBindDocRejectsDecisionCollectionSpiteOfSpelling(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDecision()
	c := e.rpcClient(testAdminToken)
	if _, err := c.SetDecisionDoc(t.Context(), req(&apiv1.SetDecisionDocRequest{
		Repo: "tester/demo", BaseUrl: decisionBase, DocId: decisionDocID, Token: decisionSecret,
	})); err != nil {
		t.Fatalf("SetDecisionDoc：%v", err)
	}
	_, err := c.BindDocCollection(t.Context(), req(&apiv1.BindDocCollectionRequest{
		Repo: "tester/demo", BaseUrl: "https://OUTLINE.example.com:443/", CollectionId: "col-decision", Token: "read-token",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "待决策清单") {
		t.Fatalf("应拒绝与待决策清单落同一集合：%v", err)
	}
}

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Dashboard 的设计依据视图的功能测试：真实 Postgres；不访问外部文档系统。

type docsFixture struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	core   *Core
	docs   *docsync.Service
	repoID int64
}

func newDocsFixture(t *testing.T) *docsFixture {
	t.Helper()
	ctx := t.Context()
	db := store.New(pgtest.Pool(t))
	if _, err := db.Exec(ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES (100, 1, 100, 'alice', 'app', true, 'main', 'https://example.com/alice/app.git')`); err != nil {
		t.Fatal(err)
	}
	docs := docsync.New(docsync.Deps{DB: db, MasterKey: []byte("0123456789abcdef0123456789abcdef")})
	core := New(Config{}, Deps{DB: db, Log: slog.New(slog.DiscardHandler)})
	core.SetDocs(docs)
	return &docsFixture{t: t, ctx: ctx, db: db, core: core, docs: docs, repoID: 100}
}

// insertRequest 插入一个 AI 请求（family_id 指向自己）。
func (f *docsFixture) insertRequest(id int64, typ string) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES ($1,$2,$3,'manual','refs/heads/main','abc','succeeded',$1)`, id, typ, f.repoID); err != nil {
		f.t.Fatal(err)
	}
}

// insertTask 插入一个 AI 任务并返回 attempt id（每个任务一个 attempt）。
func (f *docsFixture) insertTask(id, requestID int64, typ string, spec any) int64 {
	f.t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES ($1,$2,$3,$4,ARRAY['darwin/arm64'],'claude-code','任务',$5,'succeeded',60)`,
		id, requestID, typ+"-"+itoa(id), typ, raw); err != nil {
		f.t.Fatal(err)
	}
	var attemptID int64
	if err := f.db.QueryRow(f.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_name, acquire_request_id)
		VALUES ($1, 1, 'node', $2) RETURNING id`, id, "acq-"+itoa(id)).Scan(&attemptID); err != nil {
		f.t.Fatal(err)
	}
	return attemptID
}

// insertReviewTask 插入一个审计任务，返回 attempt id。
func (f *docsFixture) insertReviewTask(id, requestID int64, spec map[string]any) int64 {
	return f.insertTask(id, requestID, "review", spec)
}

func (f *docsFixture) insertDevTask(id, requestID int64, spec map[string]any) int64 {
	return f.insertTask(id, requestID, "develop", spec)
}

// insertWorkItem 插入一个工作项，领取 attempt 是 attemptID。
func (f *docsFixture) insertWorkItem(id, claimAttempt int64) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES ($1,$2,'developing','main','abc',2,'claude-code',$3)`, id, f.repoID, claimAttempt); err != nil {
		f.t.Fatal(err)
	}
}

func (f *docsFixture) insertDevRequest(requestID int64, workItemID *int64) {
	f.t.Helper()
	f.insertDevRequestMode(requestID, "select", workItemID)
}

// insertDevRequestMode 按指定的开发方式插入开发请求。
func (f *docsFixture) insertDevRequestMode(requestID int64, mode string, workItemID *int64) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `INSERT INTO ai_dev_requests (request_id, mode, work_item_id) VALUES ($1,$2,$3)`,
		requestID, mode, workItemID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *docsFixture) insertReviewRequest(requestID int64, source string, workItemID *int64, setID *int64, digest string) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, `INSERT INTO ai_review_requests (request_id, docs_source, work_item_id, doc_snapshot_set_id, docs_manifest_digest)
		VALUES ($1,$2,$3,$4,$5)`, requestID, source, workItemID, setID, digest); err != nil {
		f.t.Fatal(err)
	}
}

// freezeWorkItem 直接写入工作项快照与冻结标记（摘要与真实冻结一致），返回整批摘要。
func (f *docsFixture) freezeWorkItem(workItemID int64, docs []docsync.Snapshot) string {
	f.t.Helper()
	src := docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 3}
	digest := docsync.ManifestDigest(docsync.SourceWorkItem, src, docs)
	if _, err := f.db.Exec(f.ctx, `INSERT INTO work_item_doc_freezes
		(work_item_id, doc_count, provider, base_url, collection_id, collection_name, binding_generation, manifest_digest)
		VALUES ($1,$2,'outline',$3,'col-1','设计',3,$4)`,
		workItemID, len(docs), src.BaseURL, digest); err != nil {
		f.t.Fatal(err)
	}
	for _, d := range docs {
		if _, err := f.db.Exec(f.ctx, `INSERT INTO work_item_doc_snapshots
			(work_item_id, doc_id, title, revision, content_sha256, content, url)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, workItemID, d.DocID, d.Title, d.Revision, d.SHA256, d.Content, d.URL); err != nil {
			f.t.Fatal(err)
		}
	}
	return digest
}

// insertSet 写入一个独立快照集，返回它的摘要。
func (f *docsFixture) insertSet(id int64, docs []docsync.Snapshot) string {
	f.t.Helper()
	src := docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 3}
	digest := docsync.ManifestDigest(docsync.SourceSnapshotSet, src, docs)
	if _, err := f.db.Exec(f.ctx, `INSERT INTO doc_snapshot_sets
		(id, repo_id, provider, base_url, collection_id, collection_name, binding_generation, doc_count, manifest_digest)
		VALUES ($1,$2,'outline',$3,'col-1','设计',3,$4,$5)`,
		id, f.repoID, src.BaseURL, len(docs), digest); err != nil {
		f.t.Fatal(err)
	}
	for _, d := range docs {
		if _, err := f.db.Exec(f.ctx, `INSERT INTO doc_snapshot_docs
			(snapshot_set_id, doc_id, title, revision, content_sha256, content, url)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, d.DocID, d.Title, d.Revision, d.SHA256, d.Content, d.URL); err != nil {
			f.t.Fatal(err)
		}
	}
	return digest
}

func docSnap(id, title, content string) docsync.Snapshot {
	return docsync.Snapshot{
		DocID: id, Title: title, Revision: 1, SHA256: docprovider.Sum(content), Content: content,
		URL: "https://outline.example.com/doc/" + id,
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func ptr64(n int64) *int64 { return new(n) }

func TestWorkItemDocsView(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	f.insertRequest(1, "develop")
	attempt := f.insertDevTask(1, 1, map[string]any{"mode": "select"})
	f.insertWorkItem(10, attempt)
	f.freezeWorkItem(10, []docsync.Snapshot{docSnap("d1", "甲", "正文")})

	v, err := f.core.WorkItemDocsView(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != docsync.SourceWorkItem || !v.Frozen || len(v.Docs) != 1 || v.Docs[0].DocID != "d1" {
		t.Fatalf("工作项依据视图 = %+v", v)
	}
	// 按需读取正文。
	d, err := f.core.WorkItemDocContent(f.ctx, 10, "d1")
	if err != nil || d == nil || d.Content != "正文" {
		t.Fatalf("WorkItemDocContent = %+v, %v", d, err)
	}
	if d, err := f.core.WorkItemDocContent(f.ctx, 10, "nope"); err != nil || d != nil {
		t.Errorf("不存在的文档应返回 nil：%+v, %v", d, err)
	}
	// 不存在的工作项应报错。
	if _, err := f.core.WorkItemDocsView(f.ctx, 11); err == nil {
		t.Error("不存在的工作项应报错")
	}
}

func TestRequestDocsViewSources(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)

	// 1. 独立审计的冻结快照集。
	f.insertRequest(20, "review")
	digest := f.insertSet(300, []docsync.Snapshot{docSnap("d1", "甲", "正文")})
	setID := int64(300)
	f.insertReviewRequest(20, "snapshot_set", nil, &setID, digest)
	f.insertReviewTask(21, 20, map[string]any{
		"docs_source": "snapshot_set", "doc_snapshot_set_id": 300, "docs_manifest_digest": digest,
	})
	v, err := f.core.RequestDocsView(f.ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if v.DeclaredSource != "snapshot_set" || v.Error != "" || v.View == nil || v.View.Source != "snapshot_set" {
		t.Fatalf("独立快照集 = %+v", v)
	}
	if d, err := f.core.RequestDocContent(f.ctx, 20, "d1"); err != nil || d == nil || d.Content != "正文" {
		t.Fatalf("RequestDocContent = %+v, %v", d, err)
	}
	// 跨父对象：不属于这个请求的文档读不到。
	if d, err := f.core.RequestDocContent(f.ctx, 20, "other"); err != nil || d != nil {
		t.Errorf("不属于依据的文档应返回 nil：%+v, %v", d, err)
	}

	// 2. 摘要与快照集不一致：明确报冲突，不展示为正常依据。
	f.insertRequest(22, "review")
	f.insertReviewRequest(22, "snapshot_set", nil, &setID, strings.Repeat("a", 64))
	v, err = f.core.RequestDocsView(f.ctx, 22)
	if err != nil {
		t.Fatal(err)
	}
	if v.Error == "" || v.View == nil || v.View.Source != "none" {
		t.Fatalf("摘要不一致应报错且不展示依据：%+v", v)
	}
	if v.Issue == nil || v.Issue.Code != IssueRequestSetDigest || v.Error != "请求记录的摘要与文档快照集不一致" {
		t.Errorf("摘要不一致的原因码与中文说明：%+v，%q", v.Issue, v.Error)
	}
	if _, err := f.core.RequestDocContent(f.ctx, 22, "d1"); !errors.Is(err, ErrDocsBasisConflict) {
		t.Errorf("摘要不一致时读取正文应返回冲突：%v", err)
	}

	// 3. 迁移前的 legacy：没有记录依据，不借用工作项或当前绑定。
	f.insertRequest(23, "review")
	f.insertReviewRequest(23, "legacy", nil, nil, "")
	v, err = f.core.RequestDocsView(f.ctx, 23)
	if err != nil {
		t.Fatal(err)
	}
	if v.DeclaredSource != "legacy" || v.Declared || v.View.Source != "none" {
		t.Fatalf("legacy = %+v", v)
	}
	if _, err := f.core.RequestDocContent(f.ctx, 23, "d1"); !errors.Is(err, ErrDocsBasisMissing) {
		t.Errorf("legacy 读取正文应返回没有依据：%v", err)
	}
}

// 开发请求通过工作项快照读取：选题时还没有工作项，返修请求按工作项的冻结快照。
func TestRequestDocsViewDevelop(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	f.insertRequest(24, "develop")
	attempt := f.insertDevTask(24, 24, map[string]any{"mode": "select"})
	f.insertDevRequest(24, nil)
	f.insertWorkItem(30, attempt)
	f.freezeWorkItem(30, []docsync.Snapshot{docSnap("w1", "设计", "工作项正文")})
	// 开发请求在选题时还没有工作项（work_item_id 为空）：没有依据，不按 claim_attempt 反推。
	v, err := f.core.RequestDocsView(f.ctx, 24)
	if err != nil {
		t.Fatal(err)
	}
	if v.DeclaredSource != "none" || v.View.Source != "none" {
		t.Fatalf("选题请求（没有工作项） = %+v", v)
	}
	// 有了工作项之后（返修请求）按工作项快照。
	f.insertRequest(25, "develop")
	f.insertDevRequest(25, ptr64(30))
	v, err = f.core.RequestDocsView(f.ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if v.DeclaredSource != "work_item" || v.View.Source != "work_item" || v.View.DocCount != 1 {
		t.Fatalf("返修请求 = %+v", v)
	}
}

func TestTaskDocsView(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	// 选题任务（spec 没有 work_item_id）：按领取 attempt 解析到工作项冻结。
	f.insertRequest(40, "develop")
	attempt := f.insertDevTask(40, 40, map[string]any{"mode": "select"})
	f.insertDevRequest(40, nil)
	f.insertWorkItem(41, attempt)
	f.freezeWorkItem(41, []docsync.Snapshot{docSnap("d1", "甲", "正文")})

	v, err := f.core.TaskDocsView(f.ctx, 40)
	if err != nil {
		t.Fatal(err)
	}
	if v.BasisSource != "work_item" || v.DeclaredSource != "none" || v.Error != "" || v.View.Source != "work_item" {
		t.Fatalf("选题任务依据 = %+v", v)
	}

	// 审计任务：spec 与请求明细一致。
	f.insertRequest(42, "review")
	digest := f.insertSet(400, []docsync.Snapshot{docSnap("r1", "审计设计", "正文")})
	setID := int64(400)
	f.insertReviewRequest(42, "snapshot_set", nil, &setID, digest)
	f.insertReviewTask(43, 42, map[string]any{
		"docs_source": "snapshot_set", "doc_snapshot_set_id": 400, "docs_manifest_digest": digest,
	})
	v, err = f.core.TaskDocsView(f.ctx, 43)
	if err != nil {
		t.Fatal(err)
	}
	if v.BasisSource != "snapshot_set" || v.DeclaredSource != "snapshot_set" || v.Error != "" {
		t.Fatalf("审计任务依据 = %+v", v)
	}
	if d, err := f.core.TaskDocContent(f.ctx, 43, "r1"); err != nil || d == nil {
		t.Fatalf("TaskDocContent = %+v, %v", d, err)
	}

	// 任务的 spec 与请求明细不一致：展示错误，读取正文返回冲突。
	f.insertReviewTask(44, 42, map[string]any{"docs_source": "none"})
	v, err = f.core.TaskDocsView(f.ctx, 44)
	if err != nil {
		t.Fatal(err)
	}
	if v.Error == "" {
		t.Fatalf("spec 与明细不一致应给出错误：%+v", v)
	}
	if v.Issue == nil || v.Issue.Code != IssueTaskSpecMismatch || v.Issue.TaskID != 44 || v.Error != "设计依据记录不一致：AI 任务 #44 的文档依据与请求明细不一致" {
		t.Errorf("spec 不一致的原因码与中文说明：%+v，%q", v.Issue, v.Error)
	}
	if _, err := f.core.TaskDocContent(f.ctx, 44, "r1"); !errors.Is(err, ErrDocsBasisConflict) {
		t.Errorf("不一致的任务读取正文应返回冲突：%v", err)
	}
}

// TestDevTaskDocsBasisAuthoritative：开发任务的依据以 ai_dev_requests.work_item_id 为权威。请求已经绑定工作项时，
// 任务 spec 缺失或指向别的工作项（哪怕别的工作项的文档与请求一致）都是冲突；只有合法的选题任务可以按领取 attempt
// 回退；跨仓库的工作项同样拒绝。
func TestDevTaskDocsBasisAuthoritative(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	// A：选题任务领取到的工作项；B：同一仓库的另一个工作项。
	f.insertRequest(50, "develop")
	claimA := f.insertDevTask(50, 50, map[string]any{"mode": "select"})
	f.insertDevRequestMode(50, "select", nil)
	f.insertWorkItem(50, claimA)
	docsA := []docsync.Snapshot{docSnap("a1", "甲", "A 的正文")}
	f.freezeWorkItem(50, docsA)
	f.insertRequest(60, "develop")
	claimB := f.insertDevTask(60, 60, map[string]any{"mode": "select"})
	f.insertDevRequestMode(60, "select", nil)
	f.insertWorkItem(60, claimB)
	f.freezeWorkItem(60, []docsync.Snapshot{docSnap("b1", "乙", "B 的正文")})
	refsA := []map[string]any{{"id": "a1", "title": "甲", "revision": 1, "sha256": docsA[0].SHA256, "url": docsA[0].URL}}

	// 请求已经绑定 A，任务 spec 却写 B（引用的还是 A 的文档）。
	f.insertRequest(61, "develop")
	f.insertDevRequestMode(61, "implement", ptr64(50))
	f.insertDevTask(61, 61, map[string]any{"mode": "implement", "work_item_id": 60, "docs": refsA})
	// 请求已经绑定 A，任务 spec 缺失工作项：非选题任务不能缺字段。
	f.insertRequest(62, "develop")
	f.insertDevRequestMode(62, "implement", ptr64(50))
	f.insertDevTask(62, 62, map[string]any{"mode": "implement"})
	// 合法的选题任务（请求还没有绑定）只能按领取 attempt 回退；spec 又写了 B 且与该 attempt 领取到的工作项不同。
	f.insertRequest(63, "develop")
	claim63 := f.insertDevTask(63, 63, map[string]any{"mode": "select", "work_item_id": 60})
	f.insertDevRequestMode(63, "select", nil)
	f.insertWorkItem(63, claim63)
	// 不是选题任务，却想按领取 attempt 回退到工作项。
	f.insertRequest(64, "develop")
	claim64 := f.insertDevTask(64, 64, map[string]any{"mode": "implement"})
	f.insertDevRequestMode(64, "implement", nil)
	f.insertWorkItem(64, claim64)
	// 选题任务还没有领取工作项，spec 却凭工作项编号去读同仓无关的工作项 B。
	f.insertRequest(66, "develop")
	f.insertDevTask(66, 66, map[string]any{"mode": "select", "work_item_id": 60})
	f.insertDevRequestMode(66, "select", nil)
	// 非选题任务缺失请求绑定的工作项，spec 却写了 B。
	f.insertRequest(67, "develop")
	f.insertDevTask(67, 67, map[string]any{"mode": "fix", "work_item_id": 60})
	f.insertDevRequestMode(67, "fix", nil)
	// 请求绑定的工作项属于另一个仓库：不能读到别仓的文档。
	if _, err := f.db.Exec(f.ctx, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES (101, 1, 101, 'bob', 'other', true, 'main', 'https://example.com/bob/other.git')`); err != nil {
		t.Fatal(err)
	}
	f.insertRequest(65, "develop")
	claim70 := f.insertDevTask(65, 65, map[string]any{"mode": "implement", "work_item_id": 70})
	if _, err := f.db.Exec(f.ctx, `INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES (70,101,'developing','main','abc',2,'claude-code',$1)`, claim70); err != nil {
		t.Fatal(err)
	}
	f.insertDevRequestMode(65, "implement", ptr64(70))

	for _, taskID := range []int64{61, 62, 63, 64, 66, 67, 65} {
		v, err := f.core.TaskDocsView(f.ctx, taskID)
		if err != nil {
			t.Fatalf("任务 #%d 的视图：%v", taskID, err)
		}
		if v.Error == "" {
			t.Errorf("任务 #%d 的记录不一致，应展示错误：%+v", taskID, v)
		}
		// 正文接口同样拒绝，绝不返回 A 或 B 的正文。
		for _, docID := range []string{"a1", "b1"} {
			if d, err := f.core.TaskDocContent(f.ctx, taskID, docID); !errors.Is(err, ErrDocsBasisConflict) {
				t.Errorf("任务 #%d 读取 %s 应返回冲突，实际 %+v，%v", taskID, docID, d, err)
			}
		}
	}
}

// TestTaskDocsBasisRejectsForeignWorkItemInSpec：请求关联工作项 A（或没有工作项），旧 spec 却显式写 B 时，
// 即使 docs 与 A 相同，也必须冲突：legacy、固化后的 work_item、固化后的 none 都一样。
func TestTaskDocsBasisRejectsForeignWorkItemInSpec(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	f.insertRequest(50, "develop")
	claim := f.insertDevTask(50, 50, map[string]any{"mode": "select"})
	f.insertDevRequestMode(50, "select", nil)
	f.insertWorkItem(50, claim)
	docsA := []docsync.Snapshot{docSnap("a1", "甲", "A 的正文")}
	digestA := f.freezeWorkItem(50, docsA)
	refsA := []map[string]any{{"id": "a1", "title": "甲", "revision": 1, "sha256": docsA[0].SHA256, "url": docsA[0].URL}}

	// legacy：请求关联 A，任务 spec 记录的是 B（引用与 A 相同）。
	f.insertRequest(71, "review")
	f.insertReviewRequest(71, "legacy", ptr64(50), nil, "")
	f.insertReviewTask(71, 71, map[string]any{"work_item_id": 60, "docs": refsA})
	// 固化后的 work_item：旧格式 spec，却把工作项写成 B。
	f.insertRequest(72, "review")
	f.insertReviewRequest(72, "work_item", ptr64(50), nil, digestA)
	f.insertReviewTask(72, 72, map[string]any{"work_item_id": 60, "docs": refsA})
	// 固化后的 none：旧格式 spec 没有引用，但工作项写成 B。
	f.insertRequest(73, "review")
	f.insertReviewRequest(73, "none", ptr64(50), nil, docsync.NoneManifestDigest())
	f.insertReviewTask(73, 73, map[string]any{"work_item_id": 60, "docs": []map[string]any{}})

	for _, taskID := range []int64{71, 72, 73} {
		v, err := f.core.TaskDocsView(f.ctx, taskID)
		if err != nil {
			t.Fatalf("任务 #%d 的视图：%v", taskID, err)
		}
		if v.Error == "" {
			t.Errorf("任务 #%d 的 spec 指向另一个工作项，应展示错误：%+v", taskID, v)
		}
		if d, err := f.core.TaskDocContent(f.ctx, taskID, "a1"); !errors.Is(err, ErrDocsBasisConflict) {
			t.Errorf("任务 #%d 读取正文应返回冲突，实际 %+v，%v", taskID, d, err)
		}
	}
	list, err := f.core.RelatedTasks(f.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	rel := map[int64]string{}
	for _, v := range list {
		rel[v.TaskID] = v.Relation
	}
	for _, taskID := range []int64{71, 72, 73} {
		if rel[taskID] != "mismatch" {
			t.Errorf("任务 #%d 与工作项 A 的关系 = %q，期望 mismatch", taskID, rel[taskID])
		}
	}
}

func TestRelatedTasksRelations(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	// 工作项 50：选题任务 50（领取 attempt），实现任务 51（spec 固定了文档），审计任务 52（legacy 重跑时核对固化），
	// 再有一个不一致的返修任务 53。
	f.insertRequest(50, "develop")
	claimAttempt := f.insertDevTask(50, 50, map[string]any{"mode": "select"})
	f.insertDevRequest(50, nil)
	f.insertWorkItem(50, claimAttempt)
	docs := []docsync.Snapshot{docSnap("d1", "甲", "正文")}
	f.freezeWorkItem(50, docs)

	refs := []map[string]any{{"id": "d1", "title": "甲", "revision": 1, "sha256": docs[0].SHA256, "url": docs[0].URL}}
	f.insertRequest(51, "develop")
	f.insertDevRequest(51, ptr64(50))
	f.insertDevTask(51, 51, map[string]any{"mode": "implement", "work_item_id": 50, "docs": refs})

	f.insertRequest(52, "review")
	f.insertReviewRequest(52, "legacy", ptr64(50), nil, "")
	f.insertReviewTask(52, 52, map[string]any{"work_item_id": 50, "docs": refs})

	f.insertRequest(53, "develop")
	f.insertDevRequest(53, ptr64(50))
	f.insertDevTask(53, 53, map[string]any{
		"mode": "fix", "work_item_id": 50,
		"docs": []map[string]any{{"id": "d1", "title": "改", "revision": 9, "sha256": docs[0].SHA256, "url": docs[0].URL}},
	})

	// 审计任务在 spec 里记录了来源与整批摘要，摘要与工作项冻结一致：这才是"同一份依据"。
	src := docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 3}
	digest := docsync.ManifestDigest(docsync.SourceWorkItem, src, docs)
	f.insertRequest(54, "review")
	f.insertReviewRequest(54, "work_item", ptr64(50), nil, digest)
	f.insertReviewTask(54, 54, map[string]any{
		"docs_source": "work_item", "work_item_id": 50, "docs_manifest_digest": digest, "docs": refs,
	})

	// 另一个工作项 B：快照与 A 不同。任务挂在 A 的请求上，spec 却指向 B（或没有引用但解析到 B）。
	f.insertRequest(60, "develop")
	attemptB := f.insertDevTask(60, 60, map[string]any{"mode": "select"})
	f.insertDevRequest(60, nil)
	f.insertWorkItem(60, attemptB)
	docsB := []docsync.Snapshot{docSnap("b1", "乙", "B 的正文")}
	f.freezeWorkItem(60, docsB)
	refsB := []map[string]any{{"id": "b1", "title": "乙", "revision": 1, "sha256": docsB[0].SHA256, "url": docsB[0].URL}}
	f.insertRequest(56, "develop")
	f.insertDevRequest(56, ptr64(50))
	f.insertDevTask(56, 56, map[string]any{"mode": "implement", "work_item_id": 60, "docs": refsB})
	// 没有引用但 spec 指向 B：也不能按 A 标成"领取后关联"。
	f.insertRequest(57, "develop")
	f.insertDevRequest(57, ptr64(50))
	f.insertDevTask(57, 57, map[string]any{"mode": "implement", "work_item_id": 60})

	list, err := f.core.RelatedTasks(f.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, v := range list {
		got[v.TaskID] = v.Relation
	}
	want := map[int64]string{
		50: "claimed_dynamic", 51: "refs_match", 52: "legacy_pinned", 53: "mismatch",
		54: "same_basis", 56: "mismatch", 57: "mismatch",
	}
	for id, rel := range want {
		if got[id] != rel {
			t.Errorf("任务 #%d 的关系 = %q，期望 %q（全部：%v）", id, got[id], rel, got)
		}
	}
	// 关系说明给出原因码（页面按语言显示），Note 是同一个原因的中文说明；依据记录不一致时带上具体原因。
	notes := map[int64]RelationNote{
		50: NoteClaimedDynamic, 51: NoteItemRefsNoDigest, 52: NoteLegacyPinned, 53: NoteItemRefsMismatch,
		54: NoteSameBasis, 56: NoteBasisConflict, 57: NoteBasisConflict,
	}
	for _, v := range list {
		want, ok := notes[v.TaskID]
		if !ok {
			continue
		}
		if v.NoteCode != want {
			t.Errorf("任务 #%d 的说明 = %q，期望 %q", v.TaskID, v.NoteCode, want)
			continue
		}
		if want != NoteBasisConflict {
			if v.Note != want.String() || v.Issue != nil || v.Detail != "" {
				t.Errorf("任务 #%d：说明 %q，原因 %+v %q", v.TaskID, v.Note, v.Issue, v.Detail)
			}
			continue
		}
		if v.Issue == nil || v.Issue.Code != IssueTaskDevRequestMismatch || v.Issue.TaskID != v.TaskID || v.Issue.RequestID != v.RequestID {
			t.Errorf("任务 #%d 的具体原因 = %+v", v.TaskID, v.Issue)
		}
		if v.Detail != v.Issue.Error() || v.Note != want.String()+"："+v.Detail {
			t.Errorf("任务 #%d：说明 %q，原文 %q", v.TaskID, v.Note, v.Detail)
		}
	}
}

// TestDocsIssueText：每个原因码都有自己的中文说明；作为错误时是 ErrDocsBasisConflict，说明带前缀。
func TestDocsIssueText(t *testing.T) {
	t.Parallel()
	e := &DocsIssue{Code: IssueTaskWorkItemMissing, TaskID: 3, WorkItemID: 9}
	if e.Detail() != "AI 任务 #3 引用的工作项 #9 不存在" || e.Error() != "设计依据记录不一致：AI 任务 #3 引用的工作项 #9 不存在" {
		t.Errorf("说明 = %q / %q", e.Detail(), e.Error())
	}
	if !errors.Is(fmt.Errorf("包装：%w", e), ErrDocsBasisConflict) {
		t.Error("应是 ErrDocsBasisConflict")
	}
	if got := (&DocsIssue{Code: "future"}).Detail(); got != "future" {
		t.Errorf("不认识的原因码原样显示：%q", got)
	}
	seen := map[string]RelationNote{}
	for _, n := range RelationNotes() {
		if n.String() == string(n) {
			t.Errorf("%s 没有中文说明", n)
		}
		if prev, dup := seen[n.String()]; dup {
			t.Errorf("%s 与 %s 的说明相同：同一条说明只定义一次", n, prev)
		}
		seen[n.String()] = n
	}
}

// 没有配置文档服务时，正文与关联任务的读取返回 ErrDocsBasisMissing，而不是因为 c.docs 为 nil 崩溃。
func TestDocsContentWithoutDocsService(t *testing.T) {
	f := newDocsFixture(t)
	c := New(Config{}, Deps{DB: f.db, Log: slog.New(slog.DiscardHandler)})
	f.insertRequest(60, "review")
	if _, err := c.WorkItemDocContent(f.ctx, 1, "d1"); !errors.Is(err, ErrDocsBasisMissing) {
		t.Fatalf("WorkItemDocContent：%v", err)
	}
	if _, err := c.RequestDocContent(f.ctx, 60, "d1"); !errors.Is(err, ErrDocsBasisMissing) {
		t.Fatalf("RequestDocContent：%v", err)
	}
	if _, err := c.TaskDocContent(f.ctx, 1, "d1"); !errors.Is(err, ErrDocsBasisMissing) {
		t.Fatalf("TaskDocContent：%v", err)
	}
	if _, err := c.RelatedTasks(f.ctx, 1); !errors.Is(err, ErrDocsBasisMissing) {
		t.Fatalf("RelatedTasks：%v", err)
	}
}

package review

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 独立 PR 审计与手动全面审计的文档冻结、重跑语义。

// fetchDoc 返回一篇处于 col1、已发布的文档。测试数据必须显式给出完整状态（集合、发布状态）：
// 假 Provider 原样返回，不替测试补任何字段。
func fetchDoc(id, title string, revision int64, content string) docprovider.Document {
	return docprovider.Document{
		ID: id, Title: title, Revision: revision, Content: content,
		CollectionID: "col1", Published: true,
	}
}

// docFetch 是进程内的假文档源，记录被调用次数（正文读取是并发的，计数加锁）；返回的文档就是测试数据本身。
type docFetch struct {
	docs     []docprovider.Document
	lists    int
	gets     int
	listHook func()
	// delay 是 GetDocument 的固定耗时（模拟慢文档集）；ctx 取消时立即返回。
	delay time.Duration
	mu    sync.Mutex
}

func (f *docFetch) ListCollections(context.Context) ([]docprovider.Collection, error) {
	return nil, nil
}

func (f *docFetch) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	if f.listHook != nil {
		f.listHook()
	}
	return slices.Clone(f.docs), nil
}

func (f *docFetch) GetDocument(ctx context.Context, id string) (docprovider.Document, error) {
	f.mu.Lock()
	f.gets++
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return docprovider.Document{}, ctx.Err()
		}
	}
	for _, d := range f.docs {
		if d.ID == id {
			return d, nil
		}
	}
	return docprovider.Document{}, errors.New("not found")
}

// docsKey 是测试用的文档绑定主密钥。
var docsKey = []byte("0123456789abcdef0123456789abcdef")

// docsService 注入文档服务（不建立绑定）。
func (h *harness) docsService(f *docFetch) {
	h.t.Helper()
	h.rev.docs = docsync.New(docsync.Deps{
		DB: h.db, MasterKey: docsKey,
		Provider: func(string, string) (docprovider.Provider, error) { return f, nil },
	})
}

// bindDocs 建立仓库到 col1 的文档绑定。
func (h *harness) bindDocs(collection string) {
	h.t.Helper()
	if err := store.PutDocBinding(h.ctx, h.db, docsKey, store.DocBinding{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: collection, CollectionName: "设计",
	}, "tok"); err != nil {
		h.t.Fatal(err)
	}
}

// reviewDocs 读取审计请求记录的文档依据。
func (h *harness) reviewDocs(reqID int64) (source string, setID, workItem *int64, digest string) {
	h.t.Helper()
	if err := h.db.QueryRow(h.ctx, `SELECT docs_source, doc_snapshot_set_id, work_item_id, docs_manifest_digest
		FROM ai_review_requests WHERE request_id=$1`, reqID).Scan(&source, &setID, &workItem, &digest); err != nil {
		h.t.Fatal(err)
	}
	return source, setID, workItem, digest
}

// completeRequest 让请求的任务全部成功（模拟 Runner 提交无问题的报告）。
func (h *harness) completeRequest(name string, reqID int64) {
	h.t.Helper()
	runner := h.addRunner(name, "host-"+name)
	for _, t := range h.tasks(reqID) {
		if t.State == ai.TaskPending {
			h.finish(t.ID, runner, reportResult(cleanReport()))
		}
	}
}

// TestIndependentAuditFreezesDocs：独立 PR 审计在规划时冻结自己的快照集，任务 spec、请求明细与快照集三处一致，
// 正文可经 ListBasis / GetBasis 读取。
func TestIndependentAuditFreezesDocs(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{
		fetchDoc("d2", "b", 1, "正文二"),
		fetchDoc("d1", "a", 2, "正文一"),
	}}
	h.docsService(fp)
	h.bindDocs("col1")
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	reqID := h.planPR(h.prEvent("opened", head, base))

	source, setID, workItem, digest := h.reviewDocs(reqID)
	if source != docsync.SourceSnapshotSet || setID == nil || workItem != nil || digest == "" {
		t.Fatalf("请求的文档依据 = %s / %v / %v / %q", source, setID, workItem, digest)
	}
	ts := h.tasks(reqID)
	var spec aispec.ReviewSpec
	if err := ts[0].DecodeSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.DocsSource != docsync.SourceSnapshotSet || spec.DocSnapshotSetID != *setID || spec.DocsManifestDigest != digest {
		t.Fatalf("任务 spec 与请求明细不一致：%+v", spec)
	}
	if len(spec.Docs) != 2 || spec.Docs[0].ID != "d1" || spec.Docs[1].ID != "d2" {
		t.Fatalf("任务 spec 的文档引用 = %+v", spec.Docs)
	}
	var repoID int64
	var count int
	var manifest string
	if err := h.db.QueryRow(h.ctx, `SELECT repo_id, doc_count, manifest_digest FROM doc_snapshot_sets WHERE id=$1`, *setID).
		Scan(&repoID, &count, &manifest); err != nil {
		t.Fatal(err)
	}
	if repoID != testRepoID || count != 2 || manifest != digest {
		t.Fatalf("快照集 = repo %d count %d digest %q", repoID, count, manifest)
	}
	if fp.lists != 1 || fp.gets != 2 {
		t.Fatalf("provider 调用 = lists %d gets %d，应为 1 / 2", fp.lists, fp.gets)
	}

	basis := docsync.Basis{Source: docsync.SourceSnapshotSet, RepoID: testRepoID, SnapshotSetID: *setID}
	docs, err := h.rev.docs.ListBasis(h.ctx, h.db, basis)
	if err != nil || len(docs) != 2 || docs[0].DocID != "d1" {
		t.Fatalf("ListBasis = %+v, %v", docs, err)
	}
	got, err := h.rev.docs.GetBasis(h.ctx, h.db, basis, "d1")
	if err != nil || got == nil || got.Content != "正文一" {
		t.Fatalf("GetBasis = %+v, %v", got, err)
	}
}

// TestManualAuditFreezesDocs：手动全面审计（没有工作项）同样冻结快照集。
func TestManualAuditFreezesDocs(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	h.enableReview(reviewConfig)

	id, sha, err := h.rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", Actor: "bob", Source: store.SourceCLI})
	if err != nil {
		t.Fatalf("手动审计：%v", err)
	}
	if id == 0 || sha == "" {
		t.Fatalf("手动审计 = %d, %q", id, sha)
	}
	source, setID, workItem, digest := h.reviewDocs(id)
	if source != docsync.SourceSnapshotSet || setID == nil || workItem != nil || digest == "" {
		t.Fatalf("手动审计的文档依据 = %s / %v / %v / %q", source, setID, workItem, digest)
	}
	var spec aispec.ReviewSpec
	if err := h.tasks(id)[0].DecodeSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.DocsSource != docsync.SourceSnapshotSet || spec.DocSnapshotSetID != *setID || len(spec.Docs) != 1 {
		t.Fatalf("手动审计的 spec = %+v", spec)
	}
	if fp.lists != 1 || fp.gets != 1 {
		t.Fatalf("provider 调用 = lists %d gets %d", fp.lists, fp.gets)
	}
}

// TestWorkItemAuditDoesNotFetchDocs：工作项 PR 的审计只读工作项的冻结快照，不访问外部文档系统。
func TestWorkItemAuditDoesNotFetchDocs(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	sha := docprovider.Sum("正文")
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, $2, '正文', 'u1')`, wi, sha)
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)

	reqID := h.planPR(h.prEvent("opened", head, base))
	source, setID, workItem, digest := h.reviewDocs(reqID)
	if source != docsync.SourceWorkItem || setID != nil || workItem == nil || *workItem != wi || digest == "" {
		t.Fatalf("工作项审计的文档依据 = %s / %v / %v / %q", source, setID, workItem, digest)
	}
	if fp.lists != 0 || fp.gets != 0 {
		t.Fatalf("工作项审计不应访问外部文档系统：lists %d gets %d", fp.lists, fp.gets)
	}
}

// TestRerunKeepsFamilyDocBasis：同一个请求族的多轮重跑（全部 / 只失败 / 单个任务）固定使用族根的快照集，不重新拉取。
func TestRerunKeepsFamilyDocBasis(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	reqID, task := h.prReviewed(reviewConfig)
	h.completeRequest("rerun-docs", reqID)
	_, rootSet, _, rootDigest := h.reviewDocs(reqID)
	if rootSet == nil {
		t.Fatal("族根应有快照集")
	}
	lists := fp.lists

	check := func(r *ai.Request) {
		t.Helper()
		source, setID, _, digest := h.reviewDocs(r.ID)
		if source != docsync.SourceSnapshotSet || setID == nil || *setID != *rootSet || digest != rootDigest {
			t.Fatalf("重跑的文档依据 = %s / %v / %q，应为快照集 %d", source, setID, digest, *rootSet)
		}
		if fp.lists != lists {
			t.Fatalf("重跑不应重新拉取文档：lists %d → %d", lists, fp.lists)
		}
	}
	for _, mode := range []RerunMode{RerunAll, RerunFailed} {
		r, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: mode, Actor: "bob"})
		if err != nil {
			t.Fatalf("重跑 %s：%v", mode, err)
		}
		check(r)
	}
	r, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunTask, TaskID: task.ID, Actor: "bob"})
	if err != nil {
		t.Fatalf("重跑单个任务：%v", err)
	}
	check(r)
}

// TestRerunNoneStaysNone：首次审计时没有绑定（none）；之后绑定文档集也不改变旧家族的依据。
func TestRerunNoneStaysNone(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	reqID, _ := h.prReviewed(reviewConfig)
	if source, setID, _, _ := h.reviewDocs(reqID); source != docsync.SourceNone || setID != nil {
		t.Fatalf("首次审计（未绑定）= %s / %v", source, setID)
	}
	h.completeRequest("none-docs", reqID)
	h.bindDocs("col1")
	r, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if source, setID, _, _ := h.reviewDocs(r.ID); source != docsync.SourceNone || setID != nil {
		t.Fatalf("绑定之后重跑仍是 none：%s / %v", source, setID)
	}
	if fp.lists != 0 {
		t.Fatalf("旧家族不应因后来绑定而拉取文档：lists %d", fp.lists)
	}
}

// TestRerunUnplannedRequiresNewAudit：首次因配置错误没有进入冻结的请求族要求发起新审计。
func TestRerunUnplannedRequiresNewAudit(t *testing.T) {
	h := newHarness(t)
	reqID, _ := h.prReviewed(reviewConfig)
	h.completeRequest("unplanned-docs", reqID)
	h.exec(`UPDATE ai_review_requests SET docs_source='unplanned', docs_manifest_digest='' WHERE request_id=$1`, reqID)
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); !errors.Is(err, ErrUnplannedDocsBasis) {
		t.Fatalf("重跑 unplanned 应为 ErrUnplannedDocsBasis，实际 %v", err)
	}
}

// TestRerunLegacyIndependent：迁移前的独立审计没有记录文档依据，重跑要求发起新审计。
func TestRerunLegacyIndependent(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	reqID, _ := h.prReviewed(reviewConfig)
	h.completeRequest("legacy-independent", reqID)
	h.exec(`UPDATE ai_review_requests SET docs_source='legacy', doc_snapshot_set_id=NULL, docs_manifest_digest='' WHERE request_id=$1`, reqID)
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); !errors.Is(err, ErrLegacyDocsBasis) {
		t.Fatalf("重跑 legacy 独立审计应为 ErrLegacyDocsBasis，实际 %v", err)
	}
}

// TestRerunLegacyWorkItemUpgrades：迁移前的工作项审计原引用与工作项快照逐篇一致时，在族根一次性固化为 work_item。
func TestRerunLegacyWorkItemUpgrades(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{}
	h.docsService(fp)
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	sha := docprovider.Sum("正文")
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, $2, '正文', 'u1')`, wi, sha)
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	reqID := h.planPR(h.prEvent("opened", head, base))
	h.completeRequest("legacy-workitem", reqID)
	h.exec(`UPDATE ai_review_requests SET docs_source='legacy', docs_manifest_digest='' WHERE request_id=$1`, reqID)

	r, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"})
	if err != nil {
		t.Fatalf("重跑 legacy 工作项审计：%v", err)
	}
	source, _, _, digest := h.reviewDocs(r.ID)
	if source != docsync.SourceWorkItem || digest == "" {
		t.Fatalf("固化后的来源 = %s / %q", source, digest)
	}
	// 族根也被固化，后续重跑读到同一个 canonical 值。
	if source, _, _, _ := h.reviewDocs(reqID); source != docsync.SourceWorkItem {
		t.Fatalf("族根应固化为 work_item，实际 %s", source)
	}
	if fp.lists != 0 {
		t.Fatalf("固化不应重新拉取文档：lists %d", fp.lists)
	}
}

// TestRerunLegacyEmptyDocsStaysNone：迁移前的请求原本没有文档引用时，即使工作项后来才冻结，也不把新快照塞进旧家族。
func TestRerunLegacyEmptyDocsStaysNone(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	reqID := h.planPR(h.prEvent("opened", head, base))
	if source, _, _, _ := h.reviewDocs(reqID); source != docsync.SourceNone {
		t.Fatalf("首次（未冻结）= %s", source)
	}
	h.completeRequest("legacy-empty", reqID)
	// 之后工作项才冻结：旧家族不应因此升级。
	sha := docprovider.Sum("正文")
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, $2, '正文', 'u1')`, wi, sha)
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi)
	h.exec(`UPDATE ai_review_requests SET docs_source='legacy', docs_manifest_digest='' WHERE request_id=$1`, reqID)

	r, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	source, setID, _, _ := h.reviewDocs(r.ID)
	if source != docsync.SourceNone || setID != nil {
		t.Fatalf("旧家族应保持 none，实际 %s / %v", source, setID)
	}
	if fp.lists != 0 {
		t.Fatalf("不应拉取文档：lists %d", fp.lists)
	}
}

// TestQueueReuseDocBasis：合并队列沿用审计时，文档依据也是审计规则摘要的一部分：文档没变（同一份绑定与正文）时沿用，
// 重绑或正文变了就不再沿用（旧结论只覆盖了旧快照）。
func TestQueueReuseDocBasis(t *testing.T) {
	mainMoved := map[string]string{"main.txt": "新的默认分支\n"}
	// setup 审计旧 head 并通过，返回旧 head。
	setup := func(t *testing.T) (*harness, *docFetch, string) {
		t.Helper()
		h := newHarness(t)
		fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
		h.docsService(fp)
		h.bindDocs("col1")
		runner := h.addRunner("queue-docs", "host-queue-docs")
		reqID, task := h.prReviewed(reviewConfig)
		h.finish(task.ID, runner, reportResult(cleanReport()))
		return h, fp, h.request(reqID).SHA
	}
	queueEvent := func(h *harness, head string) *Event {
		ev := h.prEvent("synchronize", head, h.branchHead("main"))
		ev.FromSelf = true
		return ev
	}

	t.Run("文档没变也沿用", func(t *testing.T) {
		h, _, old := setup(t)
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		h.events.events = nil
		ts := h.tasks(h.planPR(queueEvent(h, head)))
		if len(ts) != 1 || ts[0].State != ai.TaskSucceeded || ts[0].ReusedFrom == nil {
			t.Fatalf("任务 = %+v，文档没变应沿用旧结论", ts)
		}
	})
	t.Run("重绑后不再沿用", func(t *testing.T) {
		h, fp, old := setup(t)
		// 重绑到另一个集合：正文可能相同，但依据已经不是同一份（绑定代次与集合都变了）。
		// 假文档源原样返回测试数据，所以新集合里的文档要显式写出它所属的集合。
		h.bindDocs("col2")
		fp.docs = []docprovider.Document{{
			ID: "d9", Title: "b", Revision: 1, Content: "另一个集合的正文", CollectionID: "col2", Published: true,
		}}
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		ts := h.tasks(h.planPR(queueEvent(h, head)))
		if len(ts) != 1 || ts[0].State != ai.TaskPending || ts[0].ReusedFrom != nil {
			t.Fatalf("任务 = %s（沿用 %v），重绑后应照常审计", ts[0].State, ts[0].ReusedFrom)
		}
	})
	t.Run("正文变了不再沿用", func(t *testing.T) {
		h, fp, old := setup(t)
		fp.docs = []docprovider.Document{fetchDoc("d1", "a", 2, "改过的正文")}
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		ts := h.tasks(h.planPR(queueEvent(h, head)))
		if len(ts) != 1 || ts[0].State != ai.TaskPending || ts[0].ReusedFrom != nil {
			t.Fatalf("任务 = %s（沿用 %v），正文变了应照常审计", ts[0].State, ts[0].ReusedFrom)
		}
	})
}

// TestReuseRequiresSamePolicyAndPlatforms：沿用还要求审计规则摘要（含文档依据）与平台集合相同；不满足时重新执行并给出说明。
func TestReuseRequiresSamePolicyAndPlatforms(t *testing.T) {
	t.Parallel()
	prev := &ai.Task{
		ID: 5, Key: "review-m1", State: ai.TaskSucceeded, Platforms: []string{"darwin/arm64"},
		Spec: []byte(`{"policy_digest":"old"}`),
	}
	cases := []struct {
		name string
		task ai.NewTask
	}{
		{"摘要不同", ai.NewTask{Key: "review-m1", Platforms: []string{"darwin/arm64"}, Spec: &aispec.ReviewSpec{PolicyDigest: "new"}}},
		{"摘要缺失", ai.NewTask{Key: "review-m1", Platforms: []string{"darwin/arm64"}, Spec: &aispec.ReviewSpec{}}},
		{"平台不同", ai.NewTask{Key: "review-m1", Platforms: []string{"linux/amd64"}, Spec: &aispec.ReviewSpec{PolicyDigest: "old"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tasks := []ai.NewTask{c.task}
			warnings := reuse(tasks, []*ai.Task{prev}, RerunRequest{Mode: RerunFailed})
			if tasks[0].ReuseFrom != nil {
				t.Fatalf("不应沿用：%+v", tasks[0])
			}
			if len(warnings) != 1 {
				t.Fatalf("应给出一条重新执行的说明，实际 %v", warnings)
			}
		})
	}
	// 一致时沿用。
	ok := []ai.NewTask{{Key: "review-m1", Platforms: []string{"darwin/arm64"}, Spec: &aispec.ReviewSpec{PolicyDigest: "old"}}}
	if w := reuse(ok, []*ai.Task{prev}, RerunRequest{Mode: RerunFailed}); len(w) != 0 || ok[0].ReuseFrom != prev {
		t.Fatalf("一致时应沿用：%+v %v", ok[0], w)
	}
}

// commitDocs 冻结并提交一个独立审计快照集，返回依据（测试准备用）。
func (h *harness) commitDocs() docsync.Basis {
	h.t.Helper()
	prepared, err := h.rev.docs.PrepareFreeze(h.ctx, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	var basis docsync.Basis
	h.tx(func(tx pgx.Tx) error {
		var err error
		basis, err = h.rev.docs.CommitSetTx(h.ctx, tx, testRepoID, prepared)
		return err
	})
	return basis
}

// bareRequest 只写入一行 ai_requests（detail 要插入的明细还没有写）。
func (h *harness) bareRequest() int64 {
	h.t.Helper()
	var id int64
	h.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(h.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
			INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, source, family_id, state)
			SELECT id, 'review', $1, 'pull_request', 'refs/heads/x', 'abc', 'webhook', id, 'succeeded' FROM n RETURNING id`,
			testRepoID).Scan(&id)
	})
	return id
}

// detailEvent 返回 detail 需要的 PR 事件。
func (h *harness) detailEvent() *Event {
	h.t.Helper()
	repo, err := store.GetRepo(h.ctx, h.db, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	return &Event{
		Repo: repo, Event: eventPullRequest, Ref: "refs/pull/7/head", SHA: "abc", PRNumber: aiPR,
		BaseRef: "main", BaseSHA: "base", HeadRef: featureRef, Sender: "bob", Source: store.SourceWebhook,
	}
}

// TestTriggerAuditAfterCallerCancel：调用方（CLI / HTTP）在规划途中取消时，审计日志仍在受生命周期追踪的服务端预算下写入。
func TestTriggerAuditAfterCallerCancel(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	h.enableReview(reviewConfig)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	var once sync.Once
	fp.listHook = func() { once.Do(cancel) } // 拉取文档时调用方取消

	id, _, err := h.rev.Trigger(ctx, &TriggerRequest{Repo: testRepo, Ref: "main", Actor: "bob", Source: store.SourceCLI})
	if err != nil {
		t.Fatalf("调用方取消不应让已完成的规划失败：%v", err)
	}
	if id == 0 {
		t.Fatal("应建立审计请求")
	}
	if n := h.queryInt(`SELECT count(*) FROM audit_log WHERE action='ai_review.trigger'`); n != 1 {
		t.Fatalf("审计日志应在调用方取消后仍然写入，实际 %d 条", n)
	}
}

// TestDetailLocksExistingBasis：重跑（含配置错误、没有任务的情况）附着已有的快照集时，在明细事务里持有快照集的锁，
// 与保留清理严格串行：清理持锁期间明细等待，释放后才提交。
func TestDetailLocksExistingBasis(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	basis := h.commitDocs()
	reqID := h.bareRequest()
	fixed := docBasis{Source: docsync.SourceSnapshotSet, SnapshotSetID: basis.SnapshotSetID, ManifestDigest: basis.ManifestDigest}
	det := h.rev.detail(h.detailEvent(), nil, nil, &fixed)

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- h.db.WithAdvisoryLock(h.ctx, docsync.DocSetLockKey(basis.SnapshotSetID), func(context.Context, *pgxpool.Conn) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	errc := make(chan error, 1)
	go func() { errc <- h.db.Tx(h.ctx, func(tx pgx.Tx) error { return det(h.ctx, tx, reqID) }) }()
	committed := false
	var early error
	select {
	case early = <-errc:
		committed = true
	case <-time.After(300 * time.Millisecond):
	}
	// 先释放锁再断言：即使明细没有等待，清理侧的锁也不会一直挂在测试里。
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatalf("清理持锁时明细不应提交：%v", early)
	}
	if err := <-errc; err != nil {
		t.Fatalf("释放锁后明细应提交：%v", err)
	}
	source, setID, _, _ := h.reviewDocs(reqID)
	if source != docsync.SourceSnapshotSet || setID == nil || *setID != basis.SnapshotSetID {
		t.Fatalf("明细应附着已有快照集：%s / %v", source, setID)
	}
}

// TestDetailExpiredBasis：正文过了保留期后，重跑只要有任务真正执行就在建任务前失败；全部沿用结论时允许（不读取正文）。
func TestDetailExpiredBasis(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	basis := h.commitDocs()
	h.exec(`UPDATE doc_snapshot_docs SET content='' WHERE snapshot_set_id=$1`, basis.SnapshotSetID)
	stale := func(tasks ...ai.NewTask) *built {
		return &built{
			docsSource: docsync.SourceSnapshotSet, docsManifestDigest: basis.ManifestDigest,
			snapshotSetID: basis.SnapshotSetID, tasks: tasks,
		}
	}

	reqID := h.bareRequest()
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		return h.rev.detail(h.detailEvent(), stale(ai.NewTask{Key: "review-0"}), nil, nil)(h.ctx, tx, reqID)
	})
	if !errors.Is(err, docsync.ErrSnapshotExpired) {
		t.Fatalf("有真实任务时应返回 ErrSnapshotExpired，实际 %v", err)
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_review_requests WHERE request_id=$1`, reqID); n != 0 {
		t.Fatalf("失败不应写明细，实际 %d 行", n)
	}

	reqID = h.bareRequest()
	err = h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		return h.rev.detail(h.detailEvent(), stale(ai.NewTask{Key: "review-0", ReuseFrom: &ai.Task{ID: 1}}), nil, nil)(h.ctx, tx, reqID)
	})
	if err != nil {
		t.Fatalf("全部沿用结论时应允许（不读取正文）：%v", err)
	}
	if source, setID, _, _ := h.reviewDocs(reqID); source != docsync.SourceSnapshotSet || setID == nil || *setID != basis.SnapshotSetID {
		t.Fatalf("沿用的明细应保留依据：%s / %v", source, setID)
	}
}

// TestDetailRetryRegeneratesSetID：规划事务遇到序列化失败重试时，明细回调重新执行、重新提交快照集并覆盖任务 spec 的
// doc_snapshot_set_id：绝不留下上一次（已回滚）事务里的编号。
func TestDetailRetryRegeneratesSetID(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	// 第一次插入任务时抛序列化失败。故障点在明细回填之后：此时 CommitSetTx 已经把 setID1 写进任务 spec，
	// 整个事务回滚并重试后必须重新提交快照集（setID2）并覆写 spec，不能留下上一次的编号。
	// 序列的 nextval 不随事务回滚，所以只失败一次。
	h.exec(`CREATE SEQUENCE plan_retry_seq`)
	h.exec(`CREATE FUNCTION plan_retry_once() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('plan_retry_seq') = 1 THEN
				RAISE EXCEPTION 'serialization failure' USING ERRCODE = '40001';
			END IF;
			RETURN NEW;
		END $$`)
	h.exec(`CREATE TRIGGER plan_retry BEFORE INSERT ON ai_tasks FOR EACH ROW EXECUTE FUNCTION plan_retry_once()`)

	reqID := h.planPR(h.prEvent("opened", head, base))
	if h.db.Retries() == 0 {
		t.Fatal("故障点没有触发事务重试")
	}
	if n := h.queryInt(`SELECT count(*) FROM doc_snapshot_sets`); n != 1 {
		t.Fatalf("重试后应只有一个快照集，实际 %d", n)
	}
	setID := h.queryInt(`SELECT doc_snapshot_set_id FROM ai_review_requests WHERE request_id=$1`, reqID)
	if setID == 0 {
		t.Fatal("明细没有记录快照集")
	}
	// 上一次（已回滚）事务里的 setID1 不能留在数据库里。
	if n := h.queryInt(`SELECT count(*) FROM doc_snapshot_sets WHERE id < $1`, setID); n != 0 {
		t.Fatalf("回滚前的快照集不应落库：%d", n)
	}
	// 所有任务的 spec 都必须是本次提交的 setID（覆盖回填，而不是保留第一次的编号）。
	if n := h.queryInt(`SELECT count(*) FROM ai_tasks WHERE request_id=$1 AND (spec->>'doc_snapshot_set_id')::bigint <> $2`, reqID, setID); n != 0 {
		t.Fatalf("%d 个任务的 spec 快照集与明细不一致", n)
	}
}

// TestRerunLegacyDuplicateDocs：迁移前的家族引用里出现重复的文档（[A, A]）时不算与工作项快照一致，要求发起新审计。
func TestRerunLegacyDuplicateDocs(t *testing.T) {
	h := newHarness(t)
	h.docsService(&docFetch{})
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	wi := h.devWorkItem(aiPR, "claude-code")
	h.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', 'a', 1, $2, '正文一', 'u1'), ($1, 'd2', 'b', 1, $3, '正文二', 'u2')`,
		wi, docprovider.Sum("正文一"), docprovider.Sum("正文二"))
	h.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 2)`, wi)
	reqID := h.planPR(h.prEvent("opened", head, base))
	h.completeRequest("legacy-dup", reqID)
	h.exec(`UPDATE ai_review_requests SET docs_source='legacy', docs_manifest_digest='' WHERE request_id=$1`, reqID)
	// 把家族任务的引用改成 [A, A]：数量与快照相同，但 B 没有被引用（按 ID 查表的旧判断会误放行）。
	h.exec(`UPDATE ai_tasks SET spec = jsonb_set(spec, '{docs}', jsonb_build_array(spec->'docs'->0, spec->'docs'->0)) WHERE request_id=$1`, reqID)
	if _, err := h.rev.Rerun(h.ctx, RerunRequest{RequestID: reqID, Mode: RerunAll, Actor: "bob"}); !errors.Is(err, ErrLegacyDocsBasis) {
		t.Fatalf("重复的迁移前引用应为 ErrLegacyDocsBasis，实际 %v", err)
	}
}

package docsync

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// testCollection 是绑定与测试文档共用的集合 id。
const testCollection = "col1"

// docState 返回一篇处于绑定集合中、已发布的文档。测试数据必须显式给出完整状态（集合、发布状态）：
// 假 Provider 原样返回，不替测试补任何字段。
func docState(id, title string, revision int64, content string) docprovider.Document {
	return docprovider.Document{
		ID: id, Title: title, Revision: revision, Content: content,
		CollectionID: testCollection, Published: true,
	}
}

// fakeProvider 是进程内的假文档源，记录被调用次数；返回的文档就是测试数据本身。
type fakeProvider struct {
	docs        []docprovider.Document
	lists       int
	gets        int
	failOn      string        // "list" | "get" 时对应调用返回错误
	listHook    func()        // 非 nil 时在 ListDocuments 中调用（并发测试的屏障）
	delay       time.Duration // GetDocument 的固定耗时（模拟慢文档集）
	failID      string        // 指定这一篇的 GetDocument 返回错误（模拟同批中单篇正文读取失败）
	badID       bool          // 为真时 GetDocument 返回的 ID 与列表不一致
	mu          sync.Mutex    // 保护计数（GetDocument 会被并发调用）
	cur         int           // 当前在途的 GetDocument 数
	maxInFlight int           // 观测到的最大并发（证明有界并发真正生效）
}

func (f *fakeProvider) ListCollections(context.Context) ([]docprovider.Collection, error) {
	return []docprovider.Collection{{ID: testCollection, Name: "设计"}}, nil
}

func (f *fakeProvider) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	if f.listHook != nil {
		f.listHook()
	}
	if f.failOn == "list" {
		return nil, errors.New("boom")
	}
	return slices.Clone(f.docs), nil
}

func (f *fakeProvider) GetDocument(ctx context.Context, id string) (docprovider.Document, error) {
	f.mu.Lock()
	f.gets++
	f.cur++
	if f.cur > f.maxInFlight {
		f.maxInFlight = f.cur
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.cur--
		f.mu.Unlock()
	}()
	if f.failOn == "get" || (f.failID != "" && id == f.failID) {
		return docprovider.Document{}, errors.New("boom")
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return docprovider.Document{}, ctx.Err()
		}
	}
	for _, d := range f.docs {
		if d.ID == id {
			// 与 Outline 的 documents.info 一致：正文摘要由提供方按返回的正文计算。
			d.SHA256 = docprovider.Sum(d.Content)
			if f.badID {
				d.ID = "other"
			}
			return d, nil
		}
	}
	return docprovider.Document{}, errors.New("not found")
}

var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 字节

func newService(t *testing.T, fp *fakeProvider) *Service {
	t.Helper()
	return New(Deps{
		DB:        store.New(pgtest.Pool(t)),
		MasterKey: testKey,
		Provider:  func(string, string) (docprovider.Provider, error) { return fp, nil },
	})
}

func insertRepo(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	_, err := db.Exec(context.Background(), `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES ($1, 1, 100, 'alice', 'app', true, 'main', 'https://example.com/alice/app.git')`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func insertWorkItem(t *testing.T, db *store.DB, repoID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var reqID int64
	if err := db.QueryRow(ctx, `INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		SELECT n, 'develop', $1, 'push', 'refs/heads/main', 'abc', 'active', n
		FROM (SELECT nextval('ai_requests_id_seq') AS n) s RETURNING id`, repoID).Scan(&reqID); err != nil {
		t.Fatalf("插入 ai_requests：%v", err)
	}
	var taskID int64
	if err := db.QueryRow(ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES ($1, 'develop', 'develop', ARRAY['darwin/arm64'], 'claude-code', 'dev', '{}'::jsonb, 'running', 60) RETURNING id`,
		reqID).Scan(&taskID); err != nil {
		t.Fatalf("插入 ai_tasks：%v", err)
	}
	var attemptID int64
	if err := db.QueryRow(ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_name, acquire_request_id)
		VALUES ($1, 1, 'node', 'req') RETURNING id`, taskID).Scan(&attemptID); err != nil {
		t.Fatalf("插入 ai_attempts：%v", err)
	}
	var id int64
	if err := db.QueryRow(ctx, `INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES ($1, 'developing', 'main', 'abc123', 3, 'claude-code', $2) RETURNING id`, repoID, attemptID).Scan(&id); err != nil {
		t.Fatalf("插入 dev_work_items：%v", err)
	}
	return id
}

func bind(t *testing.T, db *store.DB, repoID int64) {
	t.Helper()
	err := store.PutDocBinding(context.Background(), db, testKey, store.DocBinding{
		RepoID: repoID, Provider: "outline", BaseURL: "https://outline.example.com",
		CollectionID: testCollection, CollectionName: "设计", CreatedBy: "admin",
	}, "tok-123")
	if err != nil {
		t.Fatal(err)
	}
}

// TestFreezeListGet：绑定后冻结得到快照；List 不含正文；Get 带正文；水位线推进。
func TestFreezeListGet(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{
		docState("d1", "需求", 3, "# 需求\n正文一"),
		docState("d2", "方案", 1, "# 方案\n正文二"),
	}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 1)
	wi := insertWorkItem(t, db, 1)
	bind(t, db, 1)

	snaps, err := svc.Freeze(context.Background(), 1, wi)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("冻结得到 %d 篇，应为 2", len(snaps))
	}
	if fp.lists != 1 || fp.gets != 2 {
		t.Fatalf("provider 调用次数 lists=%d gets=%d，应为 1/2", fp.lists, fp.gets)
	}

	list, err := svc.List(context.Background(), wi)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Content != "" {
		t.Fatalf("List 应返回 2 篇且不含正文：%+v", list)
	}
	// 按标题、doc_id 排序：编号标题也按字典序（"方案" 在 "需求" 之前）。
	if list[0].DocID != "d2" || list[1].DocID != "d1" {
		t.Fatalf("List 应按 (title, doc_id) 排序：%+v", list)
	}

	got, err := svc.Get(context.Background(), wi, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Content != "# 需求\n正文一" || got.SHA256 != docprovider.Sum("# 需求\n正文一") {
		t.Fatalf("Get d1 = %+v", got)
	}
	if missing, err := svc.Get(context.Background(), wi, "nope"); err != nil || missing != nil {
		t.Fatalf("Get 不存在的文档应为 nil,nil：%v %v", missing, err)
	}
}

// TestFreezeOnce：再次 Freeze 不再访问 provider，返回已有快照（开发与审计读到同一份）。
func TestFreezeOnce(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "A", 1, "x")}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 2)
	wi := insertWorkItem(t, db, 2)
	bind(t, db, 2)

	if _, err := svc.Freeze(context.Background(), 2, wi); err != nil {
		t.Fatal(err)
	}
	// 改变 fake 的内容：已冻结的快照不应随之改变，也不应再次调用 provider。
	fp.docs = []docprovider.Document{docState("d1", "A", 9, "changed")}
	if _, err := svc.Freeze(context.Background(), 2, wi); err != nil {
		t.Fatal(err)
	}
	if fp.lists != 1 || fp.gets != 1 {
		t.Fatalf("第二次 Freeze 不应再访问 provider：lists=%d gets=%d", fp.lists, fp.gets)
	}
	got, err := svc.Get(context.Background(), wi, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.Content != "x" {
		t.Fatalf("快照应保持首次冻结的内容：%+v", got)
	}
}

// TestFreezeNoBinding：没有绑定时返回空，不构造 provider。
func TestFreezeNoBinding(t *testing.T) {
	t.Parallel()
	svc := newService(t, &fakeProvider{})
	db := svc.db
	insertRepo(t, db, 3)
	wi := insertWorkItem(t, db, 3)
	snaps, err := svc.Freeze(context.Background(), 3, wi)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("没有绑定时应为空：%+v", snaps)
	}
}

// TestFreezeProviderError：拉取失败时返回错误，不落快照。
func TestFreezeProviderError(t *testing.T) {
	t.Parallel()
	svc := newService(t, &fakeProvider{failOn: "list"})
	db := svc.db
	insertRepo(t, db, 4)
	wi := insertWorkItem(t, db, 4)
	bind(t, db, 4)
	if _, err := svc.Freeze(context.Background(), 4, wi); err == nil {
		t.Fatal("拉取失败应返回错误")
	}
	list, err := svc.List(context.Background(), wi)
	if err != nil || len(list) != 0 {
		t.Fatalf("失败时不应落快照：%v %v", list, err)
	}
}

// TestFreezeEmptyCollectionOnce：绑定存在但集合为空时也要记完成标记，之后不再重新拉取、也不会把新文档混进来。
func TestFreezeEmptyCollectionOnce(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 5)
	wi := insertWorkItem(t, db, 5)
	bind(t, db, 5)
	ctx := context.Background()

	if s1, err := svc.Freeze(ctx, 5, wi); err != nil || len(s1) != 0 {
		t.Fatalf("空集合首次 Freeze = %v, %v", s1, err)
	}
	if fp.lists != 1 {
		t.Fatalf("首次应拉取一次：lists=%d", fp.lists)
	}
	// 集合之后有文档，也不应重新拉取（空结果已冻结）
	fp.docs = []docprovider.Document{docState("x", "X", 1, "x")}
	if s2, err := svc.Freeze(ctx, 5, wi); err != nil || len(s2) != 0 {
		t.Fatalf("空集合第二次 Freeze = %v, %v", s2, err)
	}
	if fp.lists != 1 {
		t.Fatalf("空集合冻结后不应再次拉取：lists=%d", fp.lists)
	}
}

// TestFreezeConcurrentDoesNotAppend：两个调用在首次冻结前并发进入、各自拉到不同集合时，只有一个提交，
// 另一个丢弃自己的批次并读取已提交的集合，最终只有一个完整的集合（不混合）。
func TestFreezeConcurrentDoesNotAppend(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertRepo(t, db, 6)
	wi := insertWorkItem(t, db, 6)
	bind(t, db, 6)

	var barrier sync.WaitGroup
	barrier.Add(2)
	hook := func() { barrier.Done(); barrier.Wait() }
	fa := &fakeProvider{docs: []docprovider.Document{docState("a", "A", 1, "a")}, listHook: hook}
	fb := &fakeProvider{docs: []docprovider.Document{docState("b", "B", 1, "b")}, listHook: hook}
	mk := func(fp *fakeProvider) *Service {
		return New(Deps{DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil }})
	}
	sa, sb := mk(fa), mk(fb)

	var ra, rb []Snapshot
	var ea, eb error
	var done sync.WaitGroup
	done.Add(2)
	go func() { defer done.Done(); ra, ea = sa.Freeze(context.Background(), 6, wi) }()
	go func() { defer done.Done(); rb, eb = sb.Freeze(context.Background(), 6, wi) }()
	done.Wait()

	if ea != nil || eb != nil {
		t.Fatalf("并发 Freeze 出错：%v / %v", ea, eb)
	}
	if len(ra) != 1 || len(rb) != 1 || ra[0].DocID != rb[0].DocID {
		t.Fatalf("两个调用应读到同一个集合：%v / %v", ra, rb)
	}
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, wi).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("快照应只有 1 篇（不追加另一批）：%d", n)
	}
}

// TestFreezeBatchConcurrency：一批文档正文用有界并发读取，整批的墙钟时间不是逐篇串行相加（4 篇各 200ms，串行约 800ms）。
func TestFreezeBatchConcurrency(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{delay: 200 * time.Millisecond}
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		fp.docs = append(fp.docs, docState(id, id, 1, id))
	}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 7)
	wi := insertWorkItem(t, db, 7)
	bind(t, db, 7)
	snaps, err := svc.Freeze(context.Background(), 7, wi)
	if err != nil || len(snaps) != 4 {
		t.Fatalf("Freeze = %d, %v", len(snaps), err)
	}
	if fp.maxInFlight < 2 {
		t.Fatalf("整批正文应并发读取，最大并发只有 %d", fp.maxInFlight)
	}
}

// TestFreezeSurvivesCallerDeadline：冻结在独立的服务端预算下完成，调用方的 RPC 期限到期不会让整批读取失败；
// 这样首次领取超时后重试能复用已完成的结果（而不是从头再拉、反复超时）。
func TestFreezeSurvivesCallerDeadline(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{delay: time.Second, docs: []docprovider.Document{
		docState("d1", "a", 1, "a"),
		docState("d2", "b", 1, "b"),
	}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 8)
	wi := insertWorkItem(t, db, 8)
	bind(t, db, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	snaps, err := svc.Freeze(ctx, 8, wi)
	if err != nil {
		t.Fatalf("调用方 RPC 超时不应让冻结失败：%v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("应冻结 2 篇：%d", len(snaps))
	}
}

// TestCompletedFreezesReleaseLocks：冻结完成后按工作项的互斥锁条目要被回收，进程内表不随历史工作项增长。
func TestCompletedFreezesReleaseLocks(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "a", 1, "a")}}
	svc := newService(t, fp)
	db := svc.db
	for i := range 3 {
		repoID := int64(20 + i)
		insertRepo(t, db, repoID)
		wi := insertWorkItem(t, db, repoID)
		bind(t, db, repoID)
		if _, err := svc.Freeze(context.Background(), repoID, wi); err != nil {
			t.Fatal(err)
		}
	}
	svc.inflightMu.Lock()
	n := len(svc.inflight)
	svc.inflightMu.Unlock()
	if n != 0 {
		t.Fatalf("冻结完成后应回收锁条目，仍有 %d 个", n)
	}
}

// TestFreezeBodyFailureDoesNotFreeze：集合列举成功、但同批中有一篇正文读取失败时，整批不提交（快照 / 完成标记 / 水位线都不写），
// 外部服务恢复后可重新冻结。若 fetchAll 吞掉单篇错误（只提交成功的那些），本测试必须失败。
func TestFreezeBodyFailureDoesNotFreeze(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{
		docs: []docprovider.Document{
			docState("d1", "a", 1, "a"),
			docState("d2", "b", 1, "b"),
			docState("d3", "c", 1, "c"),
		},
		failID: "d2",
	}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 9)
	wi := insertWorkItem(t, db, 9)
	bind(t, db, 9)
	ctx := context.Background()

	count := func(sql string, args ...any) int {
		var n int
		if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("统计 %q：%v", sql, err)
		}
		return n
	}
	if _, err := svc.Freeze(ctx, 9, wi); err == nil {
		t.Fatal("正文读取失败时 Freeze 应返回错误")
	}
	if n := count(`SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, wi); n != 0 {
		t.Fatalf("失败时不应写入快照：%d", n)
	}
	if n := count(`SELECT count(*) FROM work_item_doc_freezes WHERE work_item_id=$1`, wi); n != 0 {
		t.Fatalf("失败时不应写完成标记：%d", n)
	}
	// 外部恢复后，同一个工作项可以重新冻结（失败没有留下半成品）。
	fp.failID = ""
	snaps, err := svc.Freeze(ctx, 9, wi)
	if err != nil || len(snaps) != 3 {
		t.Fatalf("恢复后应冻结 3 篇：%d, %v", len(snaps), err)
	}
	if n := count(`SELECT count(*) FROM work_item_doc_freezes WHERE work_item_id=$1`, wi); n != 1 {
		t.Fatalf("恢复后应写完成标记：%d", n)
	}
}

// TestFreezeCancellableWhileWaitingLock：Freeze 的工作项锁等待在它自己的操作 context 下。即使调用方传的是
// context.Background()、锁被另一个使用者占住，服务生命周期取消后 Freeze 也立即返回（Close 不会死等）。
func TestFreezeCancellableWhileWaitingLock(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertRepo(t, db, 70)
	wi := insertWorkItem(t, db, 70)
	bind(t, db, 70)
	base, cancelBase := context.WithCancel(context.Background())
	var tracked atomic.Int32
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "a", 1, "a")}}
	svc := New(Deps{
		DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil },
		BaseCtx: base,
		Track: func() (func(), error) {
			tracked.Add(1)
			return func() { tracked.Add(-1) }, nil
		},
	})
	// 另一个使用者占住工作项的冻结锁（例如正在拉取的另一路冻结）。
	holding, err := svc.acquireWorkItemLock(context.Background(), wi)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := svc.Freeze(context.Background(), 70, wi)
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tracked.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Freeze 没有登记在途操作")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelBase()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("生命周期取消后 Freeze 应返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待工作项锁的 Freeze 没有随服务生命周期取消（Close 会死等）")
	}
	svc.releaseWorkItemLock(wi, holding)
	if n := tracked.Load(); n != 0 {
		t.Fatalf("Freeze 结束后应释放登记，仍有 %d", n)
	}
	svc.inflightMu.Lock()
	_, ok := svc.inflight[wi]
	svc.inflightMu.Unlock()
	if ok {
		t.Fatal("等待者取消、持有者释放后应回收锁条目")
	}
}

// TestFreezeCancellableDuringFetch：外部文档拉取也在 Freeze 的操作 context 下：服务生命周期取消时立即中断，
// 不会等满一次慢文档集的读取（Close 不会死等外部文档系统）。
func TestFreezeCancellableDuringFetch(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertRepo(t, db, 71)
	wi := insertWorkItem(t, db, 71)
	bind(t, db, 71)
	base, cancelBase := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "a", 1, "a")}, delay: 30 * time.Second}
	fp.listHook = func() { once.Do(func() { close(started) }) }
	svc := New(Deps{
		DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil },
		BaseCtx: base,
	})
	errc := make(chan error, 1)
	go func() {
		_, err := svc.Freeze(context.Background(), 71, wi)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("冻结没有开始拉取文档")
	}
	start := time.Now()
	cancelBase()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("生命周期取消后 Freeze 应失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("拉取中的 Freeze 没有随服务生命周期取消（Close 会死等）")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("取消后 %v 才返回", d)
	}
}

// TestEmptyContentSHA256：常量必须等于 docprovider.Sum("")（读取与展示都按它区分合法的空文档与已被清空的正文）。
func TestEmptyContentSHA256(t *testing.T) {
	t.Parallel()
	if got := docprovider.Sum(""); EmptyContentSHA256 != got {
		t.Fatalf("EmptyContentSHA256 = %s，应为 %s", EmptyContentSHA256, got)
	}
	if !validDigest(EmptyContentSHA256) {
		t.Fatal("EmptyContentSHA256 应是合法的 64 位小写 hex 摘要")
	}
}

// TestFreezeResolvesBindingAfterLock：等锁期间仓库解绑时，冻结按解绑后的状态进行：不读凭据、不请求外部文档系统、
// 不记完成标记（解绑不是冻结失败，之后重新绑定时仍能读到文档），而不是按锁之前读到的旧绑定白拉一批再整批失败。
func TestFreezeResolvesBindingAfterLock(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "甲", 1, "正文")}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 72)
	wi := insertWorkItem(t, db, 72)
	bind(t, db, 72)
	holding, err := svc.acquireWorkItemLock(context.Background(), wi)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		snaps []Snapshot
		err   error
	}
	resc := make(chan result, 1)
	go func() {
		snaps, err := svc.Freeze(context.Background(), 72, wi)
		resc <- result{snaps, err}
	}()
	// 等 Freeze 登记为使用者并在等锁（持有者 + 等待者 = 2）
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.inflightMu.Lock()
		lk := svc.inflight[wi]
		refs := 0
		if lk != nil {
			refs = lk.refs
		}
		svc.inflightMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Freeze 没有在等锁（使用者数 %d）", refs)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := store.DeleteDocBinding(context.Background(), db, 72); err != nil {
		t.Fatal(err)
	}
	svc.releaseWorkItemLock(wi, holding)
	res := <-resc
	if res.err != nil {
		t.Fatalf("解绑后冻结应返回空、不报错：%v", res.err)
	}
	if len(res.snaps) != 0 {
		t.Fatalf("解绑后不应冻结任何文档：%+v", res.snaps)
	}
	if fp.lists != 0 || fp.gets != 0 {
		t.Fatalf("解绑后不应请求外部文档系统：lists=%d gets=%d", fp.lists, fp.gets)
	}
	if frozen, err := svc.isFrozen(context.Background(), wi); err != nil || frozen {
		t.Fatalf("解绑后不应记完成标记：frozen=%v err=%v", frozen, err)
	}
	// 重新绑定后照常冻结：解绑没有留下完成标记。
	bind(t, db, 72)
	if snaps, err := svc.Freeze(context.Background(), 72, wi); err != nil || len(snaps) != 1 {
		t.Fatalf("重新绑定后的冻结 = %+v, %v", snaps, err)
	}
}

// TestFreezeReturnsFrozenSnapshotAfterUnbindWhileWaiting：等待者 B 在等锁期间，持锁者 A 提交了非空快照、管理员随后解绑；
// B 拿到锁后必须先复查冻结状态，返回与 A 相同的已冻结快照，而不是按解绑后的空绑定返回空。
func TestFreezeReturnsFrozenSnapshotAfterUnbindWhileWaiting(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "甲", 1, "正文")}}
	svc := newService(t, fp)
	db := svc.db
	const repoID = 74
	insertRepo(t, db, repoID)
	wi := insertWorkItem(t, db, repoID)
	bind(t, db, repoID)

	// 持锁者 A 先占住工作项锁；此时还没有冻结，B 的锁前快检不会命中。
	holding, err := svc.acquireWorkItemLock(context.Background(), wi)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		snaps []Snapshot
		err   error
	}
	resc := make(chan result, 1)
	go func() {
		snaps, err := svc.Freeze(context.Background(), repoID, wi)
		resc <- result{snaps, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.inflightMu.Lock()
		lk := svc.inflight[wi]
		refs := 0
		if lk != nil {
			refs = lk.refs
		}
		svc.inflightMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Freeze 没有在等锁（使用者数 %d）", refs)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// A 在持锁期间提交非空快照，随后管理员解绑。
	binding, err := store.ResolveDocToken(context.Background(), db, testKey, repoID)
	if err != nil || binding == nil {
		t.Fatalf("读取绑定：%v", err)
	}
	docs, err := svc.fetchBinding(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.writeOnce(context.Background(), binding, wi, docs, ManifestDigest(SourceWorkItem, bindingSource(binding), docs)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteDocBinding(context.Background(), db, repoID); err != nil {
		t.Fatal(err)
	}
	svc.releaseWorkItemLock(wi, holding)

	res := <-resc
	if res.err != nil {
		t.Fatalf("拿到锁后应返回已冻结快照，实际报错：%v", res.err)
	}
	if len(res.snaps) != 1 || res.snaps[0].DocID != "d1" {
		t.Fatalf("等待者应返回 A 已冻结的快照，实际 %+v", res.snaps)
	}
}

// TestReadWorkItemDocUsesQuerier：读取工作项的文档正文用调用方给的查询器（ReadWorkItemDoc 与 GetBasis 的 SourceWorkItem 都是），
// 在读事务里能读到这个事务自己还没提交的快照；用服务连接池上的另一次查询则读不到（B11）。
func TestReadWorkItemDocUsesQuerier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := store.New(pgtest.Pool(t))
	insertRepo(t, db, 73)
	wi := insertWorkItem(t, db, 73)
	bind(t, db, 73)
	fp := &fakeProvider{docs: []docprovider.Document{docState("a", "A", 1, "a")}}
	svc := New(Deps{DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil }})
	if _, err := svc.Freeze(ctx, 73, wi); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 还没提交的新快照：只有这个事务看得到
	if _, err := tx.Exec(ctx, `INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content)
		VALUES ($1, 'b', 'B', 1, $2, 'b')`, wi, docprovider.Sum("b")); err != nil {
		t.Fatal(err)
	}
	doc, err := svc.ReadWorkItemDoc(ctx, tx, 73, wi, "b")
	if err != nil || doc == nil || doc.Content != "b" {
		t.Fatalf("ReadWorkItemDoc 没有使用入参查询器：%+v, %v", doc, err)
	}
	basis, err := svc.GetBasis(ctx, tx, Basis{Source: SourceWorkItem, RepoID: 73, WorkItemID: wi}, "b")
	if err != nil || basis == nil || basis.Content != "b" {
		t.Fatalf("GetBasis（SourceWorkItem）没有使用入参查询器：%+v, %v", basis, err)
	}
}

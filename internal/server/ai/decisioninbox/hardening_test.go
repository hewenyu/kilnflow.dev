package decisioninbox

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 加固（审计）：远端错误脱敏、收尾落盘的有限期限、服务生命周期取消、Retry-After 与解锁。

// newServiceWith 返回注入了收尾时限 / 生命周期 context 的服务。
func newServiceWith(base context.Context, t *testing.T, db *store.DB, w docprovider.DocumentWriter, commit time.Duration) *Service {
	t.Helper()
	return New(Deps{
		DB: db, MasterKey: testMasterKey, Log: slog.New(slog.DiscardHandler),
		Writer:        func(string, string) (docprovider.DocumentWriter, error) { return w, nil },
		CommitTimeout: commit, BaseCtx: base,
	})
}

// 远端在错误响应里回显 Authorization 时，last_error 和列表都不能带出凭据。
func TestRemoteErrorIsSanitizedBeforePersisting(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	const secret = "supersecret-write-token"
	w := &fakeWriter{doc: newDoc()}
	// 读取临时失败：错误文本里带着远端回显的 Authorization。
	w.get = func(*fakeWriter) (docprovider.Document, error) {
		return docprovider.Document{}, errors.New("读取失败：Bearer " + secret)
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	v := mustSync(t, db, id)
	if v.State != "pending" || strings.Contains(v.LastError, secret) {
		t.Fatalf("last_error 泄露了凭据：%+v", v)
	}
	if !strings.Contains(v.LastError, "[已隐藏]") {
		t.Errorf("凭据应被替换：%q", v.LastError)
	}

	// 明确拒绝：远端 message 原样带凭据时同样不能落库。
	if _, err := db.Exec(t.Context(), `UPDATE dev_decision_syncs SET next_retry_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	w.get = nil
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, &docprovider.WriteError{
			Effect: docprovider.EffectNotApplied, Status: 403, Code: "authorization_error",
			Message: "denied Bearer " + secret,
		}
	}
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	v = mustSync(t, db, id)
	if v.State != "cancelled" || strings.Contains(v.LastError, secret) {
		t.Fatalf("last_error 泄露了凭据：%+v", v)
	}
	list, err := ListSyncs(t.Context(), db, nil, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list[0].LastError, secret) {
		t.Errorf("列表返回了凭据：%+v", list[0])
	}
}

// 收尾状态落盘有有限期限：行锁被占用时快速失败，不无限期挂住处理循环。
func TestCommitStatusHasFiniteTimeout(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	svc := newServiceWith(context.Background(), t, db, &fakeWriter{doc: newDoc()}, 100*time.Millisecond)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	lockConn, err := db.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT id FROM dev_decision_syncs WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()

	workConn, err := db.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer workConn.Release()
	it := &dueItem{ID: id, RepoID: testRepoID}
	start := time.Now()
	err = svc.cancel(context.Background(), workConn, it, "pending", "test", "x")
	if err == nil {
		t.Fatal("行锁被占用时收尾应超时失败")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("收尾耗时 %s，说明没有有限的期限", elapsed)
	}
	if v := mustSync(t, db, id); v.State != "pending" {
		t.Errorf("收尾失败后应保持原状态：%+v", v)
	}
}

// blockingWriter 在 GetDocument 上阻塞，直到调用方的 context 结束。
type blockingWriter struct {
	started chan struct{}
}

func (w *blockingWriter) GetDocument(ctx context.Context, _ string) (docprovider.Document, error) {
	close(w.started)
	<-ctx.Done()
	return docprovider.Document{}, ctx.Err()
}

func (w *blockingWriter) UpdateDocument(context.Context, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
	return docprovider.Document{}, errors.New("不应写入")
}

// 服务生命周期结束时，正在等 Outline 的配置操作立即返回（与 opGate.Close 的取消联动）。
func TestSetDecisionDocCancelledWithService(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	w := &blockingWriter{started: make(chan struct{})}
	svc := newServiceWith(base, t, db, w, 0)

	errc := make(chan error, 1)
	go func() {
		_, err := svc.SetDecisionDoc(context.Background(), testRepoID, testBaseURL, testDocID, "token", "admin")
		errc <- err
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("SetDecisionDoc 没有开始读取文档")
	}
	cancelBase()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("服务退出后操作应失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BaseCtx 取消后操作没有返回")
	}
	// 已经退出时不再开始新的操作。
	if _, err := svc.ResolveSync(context.Background(), 1, "delivered", "x", "admin"); err == nil {
		t.Error("服务已退出时应直接拒绝新操作")
	}
}

// Retry-After 是服务端给的等待时间，不能被硬压成 10 分钟。
func TestRateLimitHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, &docprovider.WriteError{
			Effect: docprovider.EffectNotApplied, Retryable: true, Status: 429,
			Code: "rate_limit_exceeded", RetryAfter: time.Hour,
		}
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	var wait float64
	if err := db.QueryRow(t.Context(), `SELECT extract(epoch FROM next_retry_at - now()) FROM dev_decision_syncs WHERE id=$1`, id).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 3500 || wait > 3700 {
		t.Errorf("等待 %0.f 秒，应尊重 1 小时的 Retry-After", wait)
	}
}

// 异常大的 Retry-After 有上限，也不会在换算时溢出。
func TestRateLimitCapsHugeRetryAfter(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, &docprovider.WriteError{
			Effect: docprovider.EffectNotApplied, Retryable: true, Status: 429,
			Code: "rate_limit_exceeded", RetryAfter: 48 * time.Hour,
		}
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	var wait float64
	if err := db.QueryRow(t.Context(), `SELECT extract(epoch FROM next_retry_at - now()) FROM dev_decision_syncs WHERE id=$1`, id).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 86000 || wait > 86500 {
		t.Errorf("等待 %0.f 秒，应封顶在 24 小时", wait)
	}
}

// 远端把 token 反射进 302 的 Location 时，读取和写入的错误都不能把 Location 带进 last_error 或列表。
func TestRedirectLocationNeverReachesLastError(t *testing.T) {
	t.Parallel()
	const secret = "secret-tok-xyz"
	// 真实的 Outline 客户端：3xx 不跟随，错误由 docprovider 转成本地固定文案。
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/documents.info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"data":{"id":"` + testDocID + `","revision":5,"collectionId":"` + testCollection +
				`","text":"## 待拍板\n\n","publishedAt":"2026-01-01T00:00:00Z"}}`))
		default:
			w.Header().Set("Location", "http://127.0.0.1:"+secret)
			w.WriteHeader(http.StatusFound)
		}
	}))
	defer docs.Close()

	// 写入阶段：documents.update 返回 302。
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	svc := newServiceWith(context.Background(), t, db, outlineAt(t, docs.URL), 0)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	for _, v := range []*Sync{mustSync(t, db, id)} {
		if strings.Contains(v.LastError, secret) {
			t.Fatalf("last_error 泄露了 Location：%+v", v)
		}
	}
	list, err := ListSyncs(t.Context(), db, nil, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if strings.Contains(v.LastError, secret) {
			t.Errorf("列表泄露了 Location：%+v", v)
		}
	}

	// 读取阶段：documents.info 也返回 302。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:"+secret)
		w.WriteHeader(http.StatusFound)
	}))
	defer failing.Close()
	db2 := newTestDB(t)
	insertFixture(t, db2)
	cfg2 := putConfig(t, db2, testDocID)
	svc2 := newServiceWith(context.Background(), t, db2, outlineAt(t, failing.URL), 0)
	id2 := insertSync(t, db2, syncSeed{TargetGen: cfg2.Generation})
	if err := svc2.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if v := mustSync(t, db2, id2); strings.Contains(v.LastError, secret) {
		t.Fatalf("last_error 泄露了 Location：%+v", v)
	}
}

// outlineAt 返回指向 url 的真实 Outline 写接口。
func outlineAt(t *testing.T, url string) docprovider.DocumentWriter {
	t.Helper()
	o, err := docprovider.NewOutline(docprovider.Config{BaseURL: url, Token: "write-token"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// 连接池被占满时，人工对账 / 结案的首尾数据库查询也必须随服务退出一起取消（不能被 opGate.Close 挂住）。
func TestAdminOpsCancelWhenPoolExhausted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	// 只有一条连接：占住它，之后任何 GetSync 都只能等。
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	base, cancelBase := context.WithCancel(ctx)
	defer cancelBase()
	svc := newServiceWith(base, t, db, &fakeWriter{doc: newDoc()}, 0)

	reconcileErr := make(chan error, 1)
	resolveErr := make(chan error, 1)
	go func() { _, err := svc.ReconcileSync(ctx, 1, "admin"); reconcileErr <- err }()
	go func() { _, err := svc.ResolveSync(ctx, 1, "cancelled", "确认不需要", "admin"); resolveErr <- err }()

	// 先确认两者都卡在数据库上（连接被占住）。
	select {
	case err := <-reconcileErr:
		t.Fatalf("ReconcileSync 不应在连接被占住时返回：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancelBase()
	for name, ch := range map[string]chan error{"ReconcileSync": reconcileErr, "ResolveSync": resolveErr} {
		select {
		case err := <-ch:
			if err == nil {
				t.Errorf("%s：服务退出后应返回错误", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s：服务退出后没有取消（首尾的数据库查询没有联结 BaseCtx）", name)
		}
	}
}

// 围栏里的 marker 不算送达，也不能让 InsertEntry 插进代码块：service 层复核一次（错误地写 delivered 是不可逆的）。
func TestDecisionDocFenceContainersAtServiceLevel(t *testing.T) {
	t.Parallel()
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	// 列表项围栏里带空行、引用内容以 > 开头：都是代码，不是条目。
	content := "## 待拍板\n\n- ```\n\n  `" + marker + "`\n  ```\n\n```\n> 示例\n`kiln-dec:9:wi9:t9:9`\n```\n"
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	unknownID := insertSync(t, db, syncSeed{State: "unknown", TargetGen: cfg.Generation})
	doc := docWith(testDocID)
	doc.Content, doc.Revision = content, 9
	svc := newService(t, db, &fakeWriter{doc: doc})

	// 只读查证：围栏里的 marker 不算送达。
	res, err := svc.ReconcileSync(t.Context(), unknownID, "admin")
	if err != nil {
		t.Fatalf("ReconcileSync：%v", err)
	}
	if res.Found {
		t.Error("围栏里的 marker 不应被当成已送达")
	}
	if v := mustSync(t, db, unknownID); v.State != "unknown" {
		t.Errorf("状态不应改变：%+v", v)
	}

	// 投递：条目要插在真实的 `## 待拍板` 下，围栏内容原样保留。
	db2 := newTestDB(t)
	insertFixture(t, db2)
	cfg2 := putConfig(t, db2, testDocID)
	pendingID := insertSync(t, db2, syncSeed{TargetGen: cfg2.Generation})
	doc2 := docWith(testDocID)
	doc2.Content, doc2.Revision = content, 9
	w := &fakeWriter{doc: doc2}
	svc2 := newService(t, db2, w)
	if err := svc2.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}
	if v := mustSync(t, db2, pendingID); v.State != "delivered" {
		t.Errorf("应写入成功：%+v", v)
	}
	head := strings.Index(w.doc.Content, "## 待拍板")
	entry := strings.Index(w.doc.Content, "`"+marker+"`")
	fence := strings.Index(w.doc.Content, "- ```")
	if head < 0 || entry < head || fence < 0 || entry > fence {
		t.Errorf("条目应插在围栏外的标题与围栏之间（head=%d entry=%d fence=%d）：\n%s", head, entry, fence, w.doc.Content)
	}
	if !strings.Contains(w.doc.Content, "- ```\n\n  `"+marker+"`\n  ```") {
		t.Errorf("围栏里的示例应原样保留：\n%s", w.doc.Content)
	}
}

// 缩进代码（4 空格 / Tab / 列表标记后 5 个空格）里的 marker 不算送达：service 层复核，避免误写 delivered 终态。
func TestDecisionDocIndentedCodeAtServiceLevel(t *testing.T) {
	t.Parallel()
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	for name, code := range map[string]string{
		"4 空格":      "    - `" + marker + "`\n",
		"Tab":       "\t- `" + marker + "`\n",
		"标记后 5 个空格": "-     `" + marker + "`\n",
	} {
		content := "## 待拍板\n\n" + code
		db := newTestDB(t)
		insertFixture(t, db)
		cfg := putConfig(t, db, testDocID)
		unknownID := insertSync(t, db, syncSeed{State: "unknown", TargetGen: cfg.Generation})
		pendingID := insertSync(t, db, syncSeed{EntryIdx: 1, TargetGen: cfg.Generation})
		doc := docWith(testDocID)
		doc.Content, doc.Revision = content, 9
		w := &fakeWriter{doc: doc}
		svc := newService(t, db, w)

		res, err := svc.ReconcileSync(t.Context(), unknownID, "admin")
		if err != nil {
			t.Fatalf("%s：ReconcileSync：%v", name, err)
		}
		if res.Found {
			t.Errorf("%s：缩进代码里的 marker 不应被当成已送达", name)
		}
		if v := mustSync(t, db, unknownID); v.State != "unknown" {
			t.Errorf("%s：状态不应改变：%+v", name, v)
		}

		// 投递仍正常工作：条目插到标题下，缩进代码原样保留。
		if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
			t.Fatalf("%s：ProcessRepo：%v", name, err)
		}
		w.mu.Lock()
		got := w.doc.Content
		w.mu.Unlock()
		if v := mustSync(t, db, pendingID); v.State != "delivered" {
			t.Errorf("%s：应写入成功：%+v", name, v)
		}
		entry := strings.Index(got, "`"+mustSync(t, db, pendingID).Marker+"`")
		codeIdx := strings.Index(got, code)
		if entry < 0 || codeIdx < 0 || entry > codeIdx {
			t.Errorf("%s：条目应插在缩进代码之前（entry=%d code=%d）：\n%s", name, entry, codeIdx, got)
		}
	}
}

// 单次 ProcessRepo 对每条到期条目最多尝试一次：条目处理失败但状态和 next_retry_at 都没推进时，游标必须前进，
// 不能在同一个持锁循环里反复取到同一批（持锁空转的活锁）。
func TestProcessRepoAttemptsEachDueItemOnce(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	const total = batchSize + 5
	for i := range total {
		id := insertSync(t, db, syncSeed{EntryIdx: i, TargetGen: cfg.Generation})
		// 坏 payload：读取文档之后解析失败，条目既不前进也不退避，下一轮仍然到期。
		if _, err := db.Exec(t.Context(), `UPDATE dev_decision_syncs SET payload='{"schema_version":"bad"}' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if gets, sets := w.calls(); gets != total || sets != 0 {
		t.Errorf("GetDocument %d 次、UpdateDocument %d 次，期望每条只尝试一次（%d、0）", gets, sets, total)
	}
}

// 超长中文错误落盘不能报 22021：safeErrorText 先去掉 Postgres 文本不接受的 NUL 字节，再按 UTF-8 边界截断。
func TestSafeErrorTextLongChinesePersists(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	msg := "远端错误：\x00" + strings.Repeat("中文错误信息", 1000)
	w := &fakeWriter{doc: newDoc()}
	w.get = func(*fakeWriter) (docprovider.Document, error) {
		return docprovider.Document{}, errors.New(msg)
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatalf("超长中文错误落盘失败：%v", err)
	}
	v := mustSync(t, db, id)
	if v.State != "pending" || v.LastErrorCode != "read_failed" {
		t.Fatalf("状态 = %+v，期望回到 pending 等下一轮", v)
	}
	if v.LastError == "" || len(v.LastError) > maxErrorLen+len(shorten.Ellipsis) {
		t.Errorf("last_error 长度 = %d，期望 (0, %d]（截断后追加省略号）", len(v.LastError), maxErrorLen+len(shorten.Ellipsis))
	}
	if !utf8.ValidString(v.LastError) || strings.ContainsRune(v.LastError, 0) {
		t.Errorf("last_error 含非法字节：%q", v.LastError)
	}
}

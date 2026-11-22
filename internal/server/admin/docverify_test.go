package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 显式只读连接验证的功能测试：真实 Postgres、假的文档客户端（不访问网络）。

const docVerifyToken = "outline-read-token-0123456789"

// fakeDocProvider 是只读文档客户端的替身。
type fakeDocProvider struct {
	collections []docprovider.Collection
	collErr     error
	docs        []docprovider.Document
	docsErr     error
	full        map[string]docprovider.Document
	docErr      error
	// onCollections 在 collections.list 返回之前调用（模拟验证期间的重绑）。
	onCollections func()
	// block 为 true 时 collections.list 一直等到 ctx 结束，模拟外部调用耗尽期限。
	block bool
	// getCalls 记录 GetDocument 被调用的 id。
	getCalls []string
}

func (p *fakeDocProvider) ListCollections(ctx context.Context) ([]docprovider.Collection, error) {
	if p.onCollections != nil {
		p.onCollections()
	}
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.collections, p.collErr
}

func (p *fakeDocProvider) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	return p.docs, p.docsErr
}

func (p *fakeDocProvider) GetDocument(_ context.Context, id string) (docprovider.Document, error) {
	p.getCalls = append(p.getCalls, id)
	if p.docErr != nil {
		return docprovider.Document{}, p.docErr
	}
	if d, ok := p.full[id]; ok {
		return d, nil
	}
	return docprovider.Document{}, &docprovider.Error{Status: 404}
}

// bindDoc 用 Service 绑定仓库的文档集（tester/demo）。
func (e *testEnv) bindDoc(url, collectionID string) {
	e.t.Helper()
	if _, err := e.svc.BindDocCollection(e.ctx, "tester/demo", url, collectionID, "文档", docVerifyToken, "admin"); err != nil {
		e.t.Fatal(err)
	}
}

// withDocProvider 给 Service 装上假的文档客户端与操作门闩。
func (e *testEnv) withDocProvider(p docprovider.Provider) {
	e.t.Helper()
	e.svc.docProvider = func(string, string) (docprovider.Provider, error) { return p, nil }
}

func publishedDoc(id, title, collectionID string) docprovider.Document {
	return docprovider.Document{
		ID: id, Title: title, Revision: 1, Content: "正文 " + id, CollectionID: collectionID,
		Published: true,
	}
}

func TestVerifyDocBindingOK(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	p := &fakeDocProvider{
		collections: []docprovider.Collection{{ID: "col-1", Name: "产品文档", URL: "https://outline.example.com/collection/col-1"}},
		docs: []docprovider.Document{
			{ID: "b", Title: "乙", CollectionID: "col-1", Published: true},
			{ID: "a", Title: "甲", CollectionID: "col-1", Published: true},
		},
		full: map[string]docprovider.Document{"a": publishedDoc("a", "甲", "col-1"), "b": publishedDoc("b", "乙", "col-1")},
	}
	e.withDocProvider(p)

	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Status != docsync.DocCheckStatusOK {
		t.Fatalf("VerifyDocBinding = %+v", res)
	}
	if res.DocCount == nil || *res.DocCount != 2 || res.SampleDocID != "a" {
		t.Errorf("篇数 / 抽查应为 2 / a：%+v", res)
	}
	if res.CollectionName != "产品文档" || res.CollectionURL == "" {
		t.Errorf("应记录观测到的文档集：%+v", res)
	}
	if len(p.getCalls) != 1 || p.getCalls[0] != "a" {
		t.Errorf("抽查应按 doc_id 升序只读一篇：%v", p.getCalls)
	}

	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus == nil || *b.CheckStatus != "ok" || b.CheckedBy == nil || *b.CheckedBy != "alice" {
		t.Errorf("验证结果没有落库：%+v", b)
	}
	if b.CheckedAt == nil || b.CheckStartedAt == nil || b.CheckDocCount == nil || *b.CheckDocCount != 2 {
		t.Errorf("检查时间与篇数没有落库：%+v", b)
	}
	if b.ObservedCollectionName != "产品文档" || b.CollectionURL == "" {
		t.Errorf("观测字段没有落库：%+v", b)
	}
	// token 不出现在结果、检查字段和审计日志中。
	if strings.Contains(fmt.Sprintf("%+v", res), docVerifyToken) || strings.Contains(fmt.Sprintf("%+v", *b), docVerifyToken) {
		t.Error("验证结果不应包含 token")
	}
	var detail string
	if err := e.db.QueryRow(e.ctx, `SELECT detail::text FROM audit_log WHERE action='doc.verify' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, docVerifyToken) || !strings.Contains(detail, `"status": "ok"`) || !strings.Contains(detail, `"doc_count": 2`) {
		t.Errorf("审计日志应只有状态与篇数、没有 token：%s", detail)
	}
}

func TestVerifyDocBindingEmpty(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	p := &fakeDocProvider{collections: []docprovider.Collection{{ID: "col-1", Name: "空文档集"}}}
	e.withDocProvider(p)

	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != docsync.DocCheckStatusEmpty || res.DocCount == nil || *res.DocCount != 0 || res.SampleDocID != "" {
		t.Fatalf("空文档集应给出 empty / 0 / 不抽查：%+v", res)
	}
	if len(p.getCalls) != 0 {
		t.Errorf("空文档集不应读取正文：%v", p.getCalls)
	}
}

func TestVerifyDocBindingClassifiedFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    *fakeDocProvider
		code string
	}{
		{"文档集不在可访问列表", &fakeDocProvider{
			collections: []docprovider.Collection{{ID: "other"}},
		}, docsync.DocCheckCollectionNotFound},
		{"认证失败", &fakeDocProvider{
			collErr: &docprovider.Error{Status: 401},
		}, docsync.DocCheckAuthFailed},
		{"限流", &fakeDocProvider{
			collections: []docprovider.Collection{{ID: "col-1"}},
			docsErr:     &docprovider.Error{Status: 429},
		}, docsync.DocCheckRateLimited},
		{"列举里有未发布文档", &fakeDocProvider{
			collections: []docprovider.Collection{{ID: "col-1"}},
			docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1"}},
		}, docsync.DocCheckInvalidDocument},
		{"抽查的文档读不到", &fakeDocProvider{
			collections: []docprovider.Collection{{ID: "col-1"}},
			docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1", Published: true}},
			docErr:      &docprovider.Error{Status: 404},
		}, docsync.DocCheckSampleNotFound},
		{"抽查的文档跨集合", &fakeDocProvider{
			collections: []docprovider.Collection{{ID: "col-1"}},
			docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1", Published: true}},
			full:        map[string]docprovider.Document{"a": publishedDoc("a", "甲", "col-2")},
		}, docsync.DocCheckSampleInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.bindDoc("https://outline.example.com", "col-1")
			e.withDocProvider(c.p)
			res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != docsync.DocCheckStatusFailed || res.ErrorCode != c.code {
				t.Fatalf("验证结论 = %+v，期望 failed / %s", res, c.code)
			}
			b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
			if err != nil {
				t.Fatal(err)
			}
			if b.CheckStatus == nil || *b.CheckStatus != "failed" || b.CheckErrorCode != c.code {
				t.Errorf("失败结论没有落库：%+v", b)
			}
			// 失败时远端原始信息不进错误码与说明。
			if strings.Contains(b.CheckErrorCode, " ") || strings.Contains(docsync.DocCheckErrorText(b.CheckErrorCode), "HTTP") {
				t.Errorf("错误码 / 说明应是固定枚举：%q", b.CheckErrorCode)
			}
		})
	}
}

func TestVerifyDocBindingNoBinding(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.withDocProvider(&fakeDocProvider{})
	if _, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice"); err == nil || !strings.Contains(err.Error(), "没有文档绑定") {
		t.Fatalf("没有绑定时应明确报错：%v", err)
	}
}

func TestVerifyDocBindingChangedDuringVerify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	p := &fakeDocProvider{
		collections: []docprovider.Collection{{ID: "col-1", Name: "产品文档"}},
		docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1", Published: true}},
		full:        map[string]docprovider.Document{"a": publishedDoc("a", "甲", "col-1")},
	}
	// 在拉取期间重绑（真实场景：另一个管理员同时保存了新配置）。
	p.onCollections = func() {
		actor := "bob"
		if err := store.PutDocBinding(e.ctx, e.db, testMasterKey, store.DocBinding{
			RepoID: mustRepoID(t, e, "tester/demo"), BaseURL: "https://outline.example.com", CollectionID: "col-2",
			CreatedBy: "admin", UpdatedBy: &actor,
		}, "new-token"); err != nil {
			t.Errorf("重绑：%v", err)
		}
	}
	e.withDocProvider(p)

	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	if !errors.Is(err, ErrDocBindingChangedDuringVerify) {
		t.Fatalf("验证期间绑定变了应返回哨兵错误：%v", err)
	}
	if res == nil || res.Applied {
		t.Fatalf("结果应标记为未应用：%+v", res)
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CollectionID != "col-2" || b.CheckStatus != nil {
		t.Errorf("新配置不应被旧验证结果覆盖：%+v", b)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='doc.verify'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("未应用的验证不应写审计：%d 条", n)
	}
}

// TestVerifyDocBindingExternalTimeoutStillSaved：外部探测耗尽自己的期限后，failed / timeout 的结论仍要用
// 独立的收尾预算写进数据库，页面不能停留在旧状态。
func TestVerifyDocBindingExternalTimeoutStillSaved(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	e.svc.docVerifyTimeout = 2 * time.Second
	e.svc.docVerifyExternalTimeout = 20 * time.Millisecond
	e.svc.docVerifySaveTimeout = time.Second
	e.withDocProvider(&fakeDocProvider{block: true})

	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Status != docsync.DocCheckStatusFailed || res.ErrorCode != docsync.DocCheckTimeout {
		t.Fatalf("外部超时应落库为 failed / timeout：%+v", res)
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus == nil || *b.CheckStatus != docsync.DocCheckStatusFailed || b.CheckErrorCode != docsync.DocCheckTimeout {
		t.Errorf("超时结论没有落库：%+v", b)
	}
	if b.CheckedBy == nil || *b.CheckedBy != "alice" || b.CheckedAt == nil {
		t.Errorf("落库的验证记录缺少操作者或时间：%+v", b)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='doc.verify'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("超时结论也应写一条审计：%d 条", n)
	}
}

// TestVerifyDocBindingTotalBudget：外部探测最多只能吃掉「总预算 - 落库预算」，落库仍必须落在总预算之内。
// 这里让外部调用一直阻塞，断言整次验证在总预算附近结束（而不是总预算 + 落库预算），并且 failed / timeout 已经落库。
func TestVerifyDocBindingTotalBudget(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	e.svc.docVerifyTimeout = 400 * time.Millisecond
	e.svc.docVerifyExternalTimeout = time.Second // 外部期限比总预算宽，只能靠总预算收紧
	e.svc.docVerifySaveTimeout = 150 * time.Millisecond
	e.withDocProvider(&fakeDocProvider{block: true})

	start := time.Now()
	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Status != docsync.DocCheckStatusFailed || res.ErrorCode != docsync.DocCheckTimeout {
		t.Fatalf("外部超时应落库为 failed / timeout：%+v", res)
	}
	// 落库预算不能被加在总预算之后：旧实现会跑满 400ms 的外部期限再加 150ms 落库。
	if elapsed > 500*time.Millisecond {
		t.Errorf("整次验证耗时 %v，超过了总预算（400ms）加少量调度余量", elapsed)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("外部探测没有用掉探测预算就结束了：%v", elapsed)
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus == nil || *b.CheckStatus != docsync.DocCheckStatusFailed || b.CheckErrorCode != docsync.DocCheckTimeout {
		t.Errorf("超时结论没有在总预算内落库：%+v", b)
	}
}

// TestSaveContextHonorsTotalDeadline：收尾 context 必须继承总预算已有的截止时间与取消，不能另起一份完整的
// saveTimeout，也不能脱离父 context 的取消。
func TestSaveContextHonorsTotalDeadline(t *testing.T) {
	t.Parallel()
	s := &Service{docVerifySaveTimeout: time.Hour}
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	total, totalCancel := context.WithTimeout(parent, 50*time.Millisecond)
	defer totalCancel()

	save, saveCancel, err := s.saveContext(total)
	if err != nil {
		t.Fatal(err)
	}
	defer saveCancel()
	totalDeadline, ok := total.Deadline()
	if !ok {
		t.Fatal("总预算没有截止时间")
	}
	saveDeadline, ok := save.Deadline()
	if !ok || saveDeadline.After(totalDeadline) {
		t.Fatalf("落库期限 %v 晚于总预算期限 %v：没有继承总截止时间", saveDeadline, totalDeadline)
	}
	parentCancel()
	select {
	case <-save.Done():
		if !errors.Is(save.Err(), context.Canceled) {
			t.Errorf("父 context 取消后落库 context 应被取消，得到 %v", save.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("父 context 取消后落库 context 没有立即取消")
	}
}

// TestVerifyDocBindingSaveBlockedByLock：落库事务被外部行锁挡住时，验证在总预算到期就取消并返回错误，
// 不会另起一份完整的 saveTimeout；释放锁后数据库里没有写入这次旧结果。
func TestVerifyDocBindingSaveBlockedByLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	e.svc.docVerifyTimeout = 400 * time.Millisecond
	e.svc.docVerifyExternalTimeout = 20 * time.Millisecond
	e.svc.docVerifySaveTimeout = 3 * time.Second
	e.withDocProvider(&fakeDocProvider{
		collections: []docprovider.Collection{{ID: "col-1", Name: "产品文档"}},
		docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1", Published: true}},
		full:        map[string]docprovider.Document{"a": publishedDoc("a", "甲", "col-1")},
	})

	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := tx.QueryRow(e.ctx, `SELECT 1 FROM repo_doc_bindings WHERE repo_id=$1 FOR UPDATE`,
		mustRepoID(t, e, "tester/demo")).Scan(&one); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("落库被锁住且总预算到期，应返回错误：%+v", res)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应因总预算到期取消，得到 %v", err)
	}
	// 若落库脱离总预算（另起 3 秒），这里会等到 3 秒左右。
	if elapsed > time.Second {
		t.Errorf("落库耗时 %v，超过了总预算（400ms）加调度余量", elapsed)
	}

	if err := tx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus != nil {
		t.Errorf("取消的验证不应写入结果：%v", *b.CheckStatus)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='doc.verify'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("取消的验证不应写审计：%d 条", n)
	}
}

// TestVerifyDocBindingBaseCtxCancel：服务退出时立即中断外部请求，并且绝不落库。
func TestVerifyDocBindingBaseCtxCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.baseCtx = base
	started := make(chan struct{})
	e.withDocProvider(&fakeDocProvider{block: true, onCollections: func() { close(started) }})

	errCh := make(chan error, 1)
	go func() {
		_, err := e.svc.VerifyDocBinding(context.Background(), "tester/demo", "alice")
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("验证没有开始外部请求")
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("服务退出时应返回取消错误：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务退出后验证没有结束")
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus != nil {
		t.Errorf("服务退出时不应落库：%v", *b.CheckStatus)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='doc.verify'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("服务退出时不应写审计：%d 条", n)
	}
}

// TestVerifyDocBindingUnbindRebindRejected：验证期间解绑再重绑成同一份配置（ABA），代次已经变化，
// 旧连接的结论也不能写到新配置上。
func TestVerifyDocBindingUnbindRebindRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	p := &fakeDocProvider{
		collections: []docprovider.Collection{{ID: "col-1", Name: "产品文档"}},
		docs:        []docprovider.Document{{ID: "a", CollectionID: "col-1", Published: true}},
		full:        map[string]docprovider.Document{"a": publishedDoc("a", "甲", "col-1")},
	}
	p.onCollections = func() {
		repoID := mustRepoID(t, e, "tester/demo")
		if _, err := store.DeleteDocBinding(e.ctx, e.db, repoID); err != nil {
			t.Errorf("解绑：%v", err)
		}
		actor := "bob"
		if err := store.PutDocBinding(e.ctx, e.db, testMasterKey, store.DocBinding{
			RepoID: repoID, BaseURL: "https://outline.example.com", CollectionID: "col-1",
			CreatedBy: "admin", UpdatedBy: &actor,
		}, "new-token"); err != nil {
			t.Errorf("重绑：%v", err)
		}
	}
	e.withDocProvider(p)

	res, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice")
	if !errors.Is(err, ErrDocBindingChangedDuringVerify) || res == nil || res.Applied {
		t.Fatalf("解绑再重绑后旧结果不应应用：%+v，%v", res, err)
	}
	b, err := store.GetDocBinding(e.ctx, e.db, mustRepoID(t, e, "tester/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus != nil {
		t.Errorf("新配置不应带上旧验证结果：%+v", b)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='doc.verify'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("未应用的验证不应写审计：%d 条", n)
	}
}

// TestVerifyDocBindingInvalidResponse：真实的 Outline 客户端遇到 HTTP 200 但响应损坏 / data 类型不对时，
// 验证结论是 invalid_response（不是 network_error），且结果、落库与审计都不含 token。
func TestVerifyDocBindingInvalidResponse(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body string }{
		{"损坏的 JSON", `{"ok":true,"data":[` + docVerifyToken},
		{"data 类型不对", `{"ok":true,"data":{"token":"` + docVerifyToken + `"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			e := newEnv(t)
			e.bindDoc(srv.URL, "col-1") // 用真实的 Outline 客户端（不覆盖 docProvider）

			res, err := e.svc.VerifyDocBinding(t.Context(), "tester/demo", "alice")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != docsync.DocCheckStatusFailed || res.ErrorCode != docsync.DocCheckInvalidResponse {
				t.Fatalf("损坏的响应应归为 failed / invalid_response：%+v", res)
			}
			b, err := store.GetDocBinding(t.Context(), e.db, mustRepoID(t, e, "tester/demo"))
			if err != nil {
				t.Fatal(err)
			}
			if b.CheckStatus == nil || *b.CheckStatus != docsync.DocCheckStatusFailed ||
				b.CheckErrorCode != docsync.DocCheckInvalidResponse {
				t.Errorf("结论没有按 invalid_response 落库：%+v", b)
			}
			var detail string
			if err := e.db.QueryRow(t.Context(),
				`SELECT detail::text FROM audit_log WHERE action='doc.verify' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
				t.Fatal(err)
			}
			// 结果、落库字段、错误说明与审计日志都不含 token 或响应体。
			for _, text := range []string{
				fmt.Sprintf("%+v", res), fmt.Sprintf("%+v", *b), detail,
				docsync.DocCheckErrorText(b.CheckErrorCode),
			} {
				if strings.Contains(text, docVerifyToken) || strings.Contains(text, `"ok":true`) {
					t.Errorf("验证结论泄露了 token 或响应体：%s", text)
				}
			}
		})
	}
}

func TestVerifyDocBindingGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bindDoc("https://outline.example.com", "col-1")
	p := &fakeDocProvider{collections: []docprovider.Collection{{ID: "col-1"}}}
	e.withDocProvider(p)
	// 服务正在退出：门闩已关，必须在查库与外部请求之前拒绝。
	e.svc.track = func() (func(), error) { return nil, context.Canceled }
	if _, err := e.svc.VerifyDocBinding(e.ctx, "tester/demo", "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("门闩已关时应立即拒绝：%v", err)
	}
	if len(p.getCalls) != 0 {
		t.Error("门闩已关时不应访问外部文档")
	}
	var status *string
	if err := e.db.QueryRow(e.ctx, `SELECT check_status FROM repo_doc_bindings WHERE repo_id=$1`,
		mustRepoID(t, e, "tester/demo")).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != nil {
		t.Errorf("门闩已关时不应写验证结果：%v", *status)
	}
}

// mustRepoID 返回仓库的 id。
func mustRepoID(t *testing.T, e *testEnv, fullName string) int64 {
	t.Helper()
	repo, err := store.FindRepo(e.ctx, e.db, fullName)
	if err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

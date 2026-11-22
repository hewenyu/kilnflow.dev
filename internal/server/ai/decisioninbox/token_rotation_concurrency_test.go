package decisioninbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 同目标轮换 token 的并发回归：管理员在投递在途期间对同一篇文档换了一份写 token 并撤销旧凭据。
// 旧凭据随后读 / 写都会得到 401 authentication_required，但条目绝不能因此被永久取消（旧代码会把 pending 直接
// 标成 cancelled/read_rejected 或 cancelled/write_rejected）。投递应当换用新凭据完成；写请求结果不明时仍然遵守
// C19：只做只读查证，绝不重发 UpdateDocument。
//
// 这些用例用真实 Postgres、真实 SetDecisionDoc（真正轮换代次并撤销旧凭据）和 httptest 上的真实 Outline 客户端，
// 请求停在 hook 上，从而确定性地复现「在途期间轮换」的时序。

const (
	rotationOldToken = "old-write-token"
	rotationMidToken = "mid-write-token"
	rotationNewToken = "new-write-token"
)

// stubUpdateRequest 是 documents.update 的请求体（只含协议允许的字段）。
type stubUpdateRequest struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	LastRevision int64  `json:"lastRevision"`
}

// stubHooks 是一次请求上可挂的钩子（可阻塞）。整组钩子由 rotateStub.mu 保护：用例可以在请求在途时换掉后继请求的
// 钩子（例如连续轮换 A→B→C），正在阻塞的请求不会重新读取它们，-race 下也没有数据竞争。
type stubHooks struct {
	// infoAfterAuth 在 documents.info 通过鉴权、取到快照之后、返回响应之前调用：请求已经在途。
	infoAfterAuth func(token string)
	// infoBeforeAuth 在 documents.info 鉴权之前调用：请求到达时 token 已被撤销。
	infoBeforeAuth func(token string)
	// updateBeforeRespond 在 documents.update 鉴权之前调用：此时 beginAttempt 已经提交。
	updateBeforeRespond func(token string)
	// updateResponder 覆盖 documents.update 的响应，用于模拟「旧凭据被撤销 / 结果不明」。
	updateResponder func(s *rotateStub, token string, req stubUpdateRequest) (status int, body string)
}

// rotateStub 是一个最小的 Outline 站点：按 Bearer token 鉴权，维护一篇文档的正文与修订号。
type rotateStub struct {
	docID      string
	collection string

	mu       sync.Mutex
	token    string
	content  string
	revision int64
	reads    map[string]int
	writes   map[string]int
	lastText map[string]string
	hooks    stubHooks
}

func newRotateStub() *rotateStub {
	return &rotateStub{
		docID: testDocID, collection: testCollection, token: rotationOldToken,
		content: "## 待拍板\n\n", revision: 5,
		reads: map[string]int{}, writes: map[string]int{}, lastText: map[string]string{},
	}
}

// setHooks 替换请求钩子（在请求在途时也可以调用：钩子只在请求入口读取一次）。
func (s *rotateStub) setHooks(mutate func(*stubHooks)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.hooks)
}

// hooksNow 返回当前钩子的快照，调用方在锁外执行（钩子可能阻塞）。
func (s *rotateStub) hooksNow() stubHooks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hooks
}

// setContent 直接设置文档正文（用来预置 marker 等）。
func (s *rotateStub) setContent(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = text
}

// start 启动 httptest 站点并返回它的地址。
func (s *rotateStub) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

// revokeTo 把唯一有效的 token 换成 token：旧的立即失效（对齐真实轮换的“撤销旧凭据”）。
func (s *rotateStub) revokeTo(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

// apply 模拟远端把正文落盘。
func (s *rotateStub) apply(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = text
	s.revision++
}

func (s *rotateStub) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return token == s.token
}

func (s *rotateStub) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docLocked()
}

func (s *rotateStub) docLocked() map[string]any {
	return map[string]any{
		"id": s.docID, "title": "待拍板", "text": s.content, "revision": s.revision,
		"collectionId": s.collection, "publishedAt": "2026-01-01T00:00:00Z",
	}
}

func (s *rotateStub) countRead(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[token]++
}

func (s *rotateStub) countWrite(token, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[token]++
	s.lastText[token] = text
}

func (s *rotateStub) writesFor(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[token]
}

func (s *rotateStub) readsFor(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[token]
}

// totalReads 返回所有 token 上的 documents.info 次数。
func (s *rotateStub) totalReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.reads {
		total += n
	}
	return total
}

func (s *rotateStub) lastTextFor(token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastText[token]
}

func (s *rotateStub) contentNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

func (s *rotateStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	// 钩子只在请求入口读取一次：用例可以在请求在途时换掉后继请求的钩子。
	hooks := s.hooksNow()
	switch r.URL.Path {
	case "/api/documents.info":
		s.countRead(token)
		if hooks.infoBeforeAuth != nil {
			hooks.infoBeforeAuth(token)
		}
		if !s.valid(token) {
			writeStubError(w, http.StatusUnauthorized, "authentication_required")
			return
		}
		doc := s.snapshot()
		if hooks.infoAfterAuth != nil {
			hooks.infoAfterAuth(token)
		}
		writeStubJSON(w, http.StatusOK, map[string]any{"ok": true, "data": doc})
	case "/api/documents.update":
		var req stubUpdateRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			writeStubError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		s.countWrite(token, req.Text)
		if hooks.updateBeforeRespond != nil {
			hooks.updateBeforeRespond(token)
		}
		if hooks.updateResponder != nil {
			status, respBody := hooks.updateResponder(s, token, req)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
			return
		}
		if !s.valid(token) {
			writeStubError(w, http.StatusUnauthorized, "authentication_required")
			return
		}
		s.mu.Lock()
		if req.LastRevision != s.revision {
			s.mu.Unlock()
			writeStubError(w, http.StatusConflict, "document_conflict")
			return
		}
		s.content = req.Text
		s.revision++
		doc := s.docLocked()
		s.mu.Unlock()
		writeStubJSON(w, http.StatusOK, map[string]any{"ok": true, "data": doc})
	default:
		http.NotFound(w, r)
	}
}

func writeStubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStubError(w http.ResponseWriter, status int, code string) {
	writeStubJSON(w, status, map[string]any{"ok": false, "error": code})
}

// stubDocBody 把文档序列化成 documents.update 的成功响应体。
func stubDocBody(doc map[string]any) string {
	b, err := json.Marshal(map[string]any{"ok": true, "data": doc})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// gate 把一次匹配 token 的请求停在 started 上，直到用例打开 release。
// open 是幂等的，用例把它注册成 Cleanup：断言失败提前退出时也不会把停在 hook 上的请求（以及 httptest.Close）挂死。
type gate struct {
	match       string
	startedOnce sync.Once
	releaseOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func newGate(match string) *gate {
	return &gate{match: match, started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) hook(token string) {
	if token != g.match {
		return
	}
	g.startedOnce.Do(func() {
		close(g.started)
		<-g.release
	})
}

// open 放行停在 hook 上的请求（幂等）。
func (g *gate) open() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// newGateFor 返回一个请求钩子，并在用例结束时保证放行。
func newGateFor(t *testing.T, match string) *gate {
	t.Helper()
	g := newGate(match)
	t.Cleanup(g.open)
	return g
}

func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(stateWaitTimeout):
		t.Fatalf("等待 %s 超时", what)
	}
}

// newOutlineService 返回注入真实 Outline 客户端的服务：写接口按配置里的地址与 token 现场构造。
func newOutlineService(t *testing.T, db *store.DB) *Service {
	t.Helper()
	return New(Deps{
		DB: db, MasterKey: testMasterKey, Log: slog.New(slog.DiscardHandler),
		Writer: func(baseURL, token string) (docprovider.DocumentWriter, error) {
			return docprovider.NewOutline(docprovider.Config{BaseURL: baseURL, Token: token})
		},
		BaseCtx: context.Background(),
	})
}

// putConfigAt 写入指向指定站点与 token 的「待决策清单」配置。
func putConfigAt(t *testing.T, db *store.DB, baseURL, token string) *store.DecisionDoc {
	t.Helper()
	if err := store.PutDecisionDoc(t.Context(), db, testMasterKey, store.DecisionDoc{
		RepoID: testRepoID, Provider: "outline", BaseURL: baseURL, DocID: testDocID,
		CollectionID: testCollection, CreatedBy: "admin", UpdatedBy: "admin",
	}, token); err != nil {
		t.Fatalf("PutDecisionDoc：%v", err)
	}
	d, err := store.GetDecisionDoc(t.Context(), db, testRepoID)
	if err != nil || d == nil {
		t.Fatalf("GetDecisionDoc = %+v, %v", d, err)
	}
	return d
}

// rotateToken 先撤销旧凭据，再调用真实的 SetDecisionDoc 轮换同一目标的 token（代次推进、队列不动）。
func rotateToken(t *testing.T, svc *Service, stub *rotateStub, url, newToken string) {
	t.Helper()
	stub.revokeTo(newToken)
	res, err := svc.SetDecisionDoc(t.Context(), testRepoID, url, testDocID, newToken, "admin")
	if err != nil {
		t.Fatalf("SetDecisionDoc 轮换 token：%v", err)
	}
	if res.Cancelled != 0 || res.Unknown != 0 {
		t.Fatalf("只轮换 token 不应取消条目：%+v", res)
	}
}

// runProcessRepo 在后台执行一次 ProcessRepo，返回结果通道。用例结束时 ctx 取消，后台投递随之中止。
func runProcessRepo(t *testing.T, svc *Service) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	ctx := t.Context()
	go func() { out <- svc.ProcessRepo(ctx, testRepoID) }()
	return out
}

// 用例 1（复现审计 finding）：documents.info 在途期间轮换 token。这次读取拿到的是轮换前的快照（成功返回），
// 但随后的写入绝不能再用旧凭据——beginAttempt 必须发现代次已变，换新凭据重来。
func TestTokenRotationDuringReadDoesNotWriteWithOldToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	cfg := putConfigAt(t, db, url, rotationOldToken)
	readGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) { h.infoAfterAuth = readGate.hook })
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, readGate.started, "documents.info")
	rotateToken(t, svc, stub, url, rotationNewToken)
	readGate.open()
	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}

	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationOldToken); got != 0 {
		t.Errorf("旧凭据写入了 %d 次，轮换后绝不能再用旧凭据写", got)
	}
	if got := stub.writesFor(rotationNewToken); got != 1 {
		t.Errorf("新凭据写入 %d 次，期望 1 次", got)
	}
	if !strings.Contains(stub.contentNow(), "`"+v.Marker+"`") {
		t.Errorf("文档里没有条目的精确 marker：\n%s", stub.contentNow())
	}
}

// 用例 2：documents.info 在途期间轮换并撤销旧凭据，这次读取直接 401。投递不能取消，必须换新凭据重读后完成。
func TestTokenRotationDuringReadWith401RetriesWithNewToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	cfg := putConfigAt(t, db, url, rotationOldToken)
	readGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) { h.infoBeforeAuth = readGate.hook })
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, readGate.started, "documents.info")
	rotateToken(t, svc, stub, url, rotationNewToken)
	readGate.open()
	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}

	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationOldToken); got != 0 {
		t.Errorf("旧凭据写入了 %d 次，期望 0 次", got)
	}
	if got := stub.writesFor(rotationNewToken); got != 1 {
		t.Errorf("新凭据写入 %d 次，期望 1 次", got)
	}
}

// 用例 3：beginAttempt 已提交、documents.update 在途期间轮换并撤销旧凭据。旧凭据的写得到结构化 401
// （确定没写）：条目回到 pending、复用冻结的正文，用新凭据完成唯一一次成功写入，绝不取消。
func TestTokenRotationDuringWriteRetriesWithNewToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	cfg := putConfigAt(t, db, url, rotationOldToken)
	updateGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) { h.updateBeforeRespond = updateGate.hook })
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, updateGate.started, "documents.update")
	rotateToken(t, svc, stub, url, rotationNewToken)
	updateGate.open()
	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}

	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationOldToken); got != 1 {
		t.Errorf("旧凭据写入 %d 次，期望 1 次（被撤销后拒绝）", got)
	}
	if got := stub.writesFor(rotationNewToken); got != 1 {
		t.Errorf("新凭据写入 %d 次，期望 1 次", got)
	}
	if oldText, newText := stub.lastTextFor(rotationOldToken), stub.lastTextFor(rotationNewToken); oldText == "" || oldText != newText {
		t.Errorf("重试应复用冻结的正文：old=%q new=%q", oldText, newText)
	}
	if !strings.Contains(stub.contentNow(), "`"+v.Marker+"`") {
		t.Errorf("文档里没有条目的精确 marker：\n%s", stub.contentNow())
	}
}

// 用例 4a（C19 红线）：documents.update 在途期间轮换，旧凭据的写结果不明（500），远端其实已经落盘。
// 绝不能重发写请求：用新凭据只读查证，看到 marker 后收敛为 delivered。
func TestTokenRotationWriteUncertainVerifiesWithNewToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	cfg := putConfigAt(t, db, url, rotationOldToken)
	updateGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) {
		h.updateBeforeRespond = updateGate.hook
		h.updateResponder = func(s *rotateStub, token string, req stubUpdateRequest) (int, string) {
			// 远端在返回 500 之前已经把正文落盘：结果不明。
			s.apply(req.Text)
			if token != rotationOldToken {
				return http.StatusOK, stubDocBody(s.snapshot())
			}
			return http.StatusInternalServerError, `{"ok":false,"error":"boom"}`
		}
	})
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, updateGate.started, "documents.update")
	rotateToken(t, svc, stub, url, rotationNewToken)
	updateGate.open()
	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}

	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望只读查证后 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationNewToken); got != 0 {
		t.Fatalf("结果不明时重发了 %d 次写请求，违反 C19", got)
	}
	if got := stub.writesFor(rotationOldToken); got != 1 {
		t.Errorf("旧凭据写入 %d 次，期望 1 次", got)
	}
}

// 用例 4b（C19 红线）：旧凭据的写返回损坏的 401（结果不明），远端没有落盘。仍然绝不重发写请求：
// 用新凭据只读查证，看不到 marker 就收敛为 unknown，交给人对账。
func TestTokenRotationWriteUncertainWithoutMarkerBecomesUnknown(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	cfg := putConfigAt(t, db, url, rotationOldToken)
	updateGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) {
		h.updateBeforeRespond = updateGate.hook
		h.updateResponder = func(s *rotateStub, token string, req stubUpdateRequest) (int, string) {
			if token != rotationOldToken {
				// 新凭据不应收到写请求；真收到了也让断言把它抓出来。
				s.apply(req.Text)
				return http.StatusOK, stubDocBody(s.snapshot())
			}
			// 响应体不是有效的 Outline 错误：401 也只能算结果不明。
			return http.StatusUnauthorized, "not json"
		}
	})
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, updateGate.started, "documents.update")
	rotateToken(t, svc, stub, url, rotationNewToken)
	updateGate.open()
	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}

	v := mustSync(t, db, id)
	if v.State != "unknown" {
		t.Fatalf("状态 = %s（%s：%s），期望 unknown", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationNewToken); got != 0 {
		t.Fatalf("结果不明时重发了 %d 次写请求，违反 C19", got)
	}
	// 新凭据必须真的被用来做过一次只读查证：SetDecisionDoc 的校验读取 1 次 + 投递侧的查证读取 1 次。
	// 只断言 unknown 不足以区分「查证后没找到 marker」和「跳过查证直接转 unknown」。
	if got := stub.readsFor(rotationNewToken); got != 2 {
		t.Errorf("新凭据读取 %d 次，期望 2 次（SetDecisionDoc 校验 1 次 + 投递侧只读查证 1 次）", got)
	}
}

// 用例 5（反例）：没有轮换 token 时，401 仍然正常进入 cancelled，不会因为新逻辑无限重试。
func TestAuthRejectedWithoutRotationStillCancels(t *testing.T) {
	t.Parallel()
	t.Run("读取 401", func(t *testing.T) {
		t.Parallel()
		db := newTestDB(t)
		insertFixture(t, db)
		stub := newRotateStub()
		url := stub.start(t)
		// 配置里是一个站点根本不接受的 token：没有轮换，就是纯粹的鉴权失败。
		cfg := putConfigAt(t, db, url, "revoked-token")
		svc := newOutlineService(t, db)
		id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

		if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
			t.Fatalf("ProcessRepo：%v", err)
		}
		v := mustSync(t, db, id)
		if v.State != "cancelled" || v.LastErrorCode != "read_rejected" {
			t.Fatalf("状态 = %s / %s，期望 cancelled / read_rejected", v.State, v.LastErrorCode)
		}
		if got := stub.writesFor("revoked-token"); got != 0 {
			t.Errorf("读取被拒绝后仍写入了 %d 次", got)
		}
	})

	t.Run("写入 401", func(t *testing.T) {
		t.Parallel()
		db := newTestDB(t)
		insertFixture(t, db)
		stub := newRotateStub()
		url := stub.start(t)
		cfg := putConfigAt(t, db, url, rotationOldToken)
		// 读没问题，写被提供方确定拒绝：没有发生轮换，应正常取消。
		stub.setHooks(func(h *stubHooks) {
			h.updateResponder = func(*rotateStub, string, stubUpdateRequest) (int, string) {
				return http.StatusUnauthorized, `{"ok":false,"error":"authentication_required"}`
			}
		})
		svc := newOutlineService(t, db)
		id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

		if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
			t.Fatalf("ProcessRepo：%v", err)
		}
		v := mustSync(t, db, id)
		if v.State != "cancelled" || v.LastErrorCode != "write_rejected" {
			t.Fatalf("状态 = %s / %s，期望 cancelled / write_rejected", v.State, v.LastErrorCode)
		}
		if got := stub.writesFor(rotationOldToken); got != 1 {
			t.Errorf("写入 %d 次，期望 1 次", got)
		}
	})
}

// 用例 6：只读查证的连续轮换（A→B→C）。条目从 attempted 恢复，每次读到的都是刚被撤销的旧凭据（401），
// 但配置依次推进到新代次：必须一路换到最新凭据、读到 marker 后收敛 delivered，全过程不写文档。
func TestVerifySurvivesConsecutiveRotations(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	stub := newRotateStub()
	url := stub.start(t)
	stub.setContent("## 待拍板\n\n`" + marker + "`\n")
	cfg := putConfigAt(t, db, url, rotationOldToken)
	gateA := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) { h.infoBeforeAuth = gateA.hook })
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{State: "attempted", TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, gateA.started, "documents.info (A)")

	// A→B：真实 SetDecisionDoc 轮换并撤销 A；它的校验读取用 B，不会被 gateA 拦住。换好钩子再放行 A。
	rotateToken(t, svc, stub, url, rotationMidToken)
	gateB := newGateFor(t, rotationMidToken)
	stub.setHooks(func(h *stubHooks) { h.infoBeforeAuth = gateB.hook })
	gateA.open()

	// 投递侧读 B 时停在钩子上；此时再轮换 B→C。
	waitForSignal(t, gateB.started, "documents.info (B)")
	rotateToken(t, svc, stub, url, rotationNewToken)
	gateB.open()

	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}
	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if v.LastErrorCode != "" {
		t.Errorf("送达后不应留下错误码：%s", v.LastErrorCode)
	}
	for _, token := range []string{rotationOldToken, rotationMidToken, rotationNewToken} {
		if got := stub.writesFor(token); got != 0 {
			t.Errorf("只读查证期间用 %s 写入了 %d 次", token, got)
		}
	}
	// A 只读了 1 次；B、C 各 2 次（SetDecisionDoc 的校验读取 + 投递侧的只读查证）。
	for token, want := range map[string]int{rotationOldToken: 1, rotationMidToken: 2, rotationNewToken: 2} {
		if got := stub.readsFor(token); got != want {
			t.Errorf("%s 读取 %d 次，期望 %d 次", token, got, want)
		}
	}
}

// 用例 7：只读查证的轮换预算用尽。每次读取都伴随一次新的轮换（配置推进、本次读取 401），超过
// tokenRotationRetries 后必须保持 attempted 并推迟到下一轮，绝不能把不明结果打成 unknown。
func TestVerifyRotationBudgetExhaustedStaysAttempted(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	stub.revokeTo("") // 任何 token 都读不到：模拟旧凭据一律被撤销
	cfg := putConfigAt(t, db, url, "tok-0")
	ctx := t.Context()
	var rotation atomic.Int64
	stub.setHooks(func(h *stubHooks) {
		h.infoBeforeAuth = func(string) {
			// 每次读取都模拟管理员又轮换了一次：先推进代次，再让本次读取 401。
			token := fmt.Sprintf("tok-%d", rotation.Add(1))
			if err := store.PutDecisionDoc(ctx, db, testMasterKey, store.DecisionDoc{
				RepoID: testRepoID, Provider: "outline", BaseURL: url, DocID: testDocID,
				CollectionID: testCollection, CreatedBy: "admin", UpdatedBy: "admin",
			}, token); err != nil {
				t.Errorf("轮换配置：%v", err)
			}
		}
	})
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{State: "attempted", TargetGen: cfg.Generation, TargetBaseURL: url})

	if err := svc.ProcessRepo(ctx, testRepoID); err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}
	v := mustSync(t, db, id)
	if v.State != "attempted" {
		t.Fatalf("状态 = %s（%s：%s），期望 attempted", v.State, v.LastErrorCode, v.LastError)
	}
	var retryLater bool
	if err := db.QueryRow(ctx, `SELECT next_retry_at > now() FROM dev_decision_syncs WHERE id=$1`, id).Scan(&retryLater); err != nil {
		t.Fatal(err)
	}
	if !retryLater {
		t.Error("预算用尽后应推迟到下一轮（next_retry_at 在未来）")
	}
	if got, want := stub.totalReads(), 1+tokenRotationRetries; got != want {
		t.Errorf("读取 %d 次，期望 %d 次（首次 + 每次轮换重载）", got, want)
	}
}

// 用例 8：收尾判定与配置代次核对的原子性。管理员正在用 SetDecisionDoc 轮换（事务已写入新 token、还没提交）时，
// 投递收到旧凭据的 401：普通 SELECT 看不到未提交的新代次，会把条目误取消。收尾判定必须先对配置行加 FOR SHARE，
// 等轮换事务提交后读到新代次，识别为轮换并用新凭据送达。
func TestAuthRejectionIsAtomicWithTokenRotation(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	stub := newRotateStub()
	url := stub.start(t)
	stub.revokeTo(rotationNewToken) // 旧 token 从一开始就失效
	cfg := putConfigAt(t, db, url, rotationOldToken)
	readGate := newGateFor(t, rotationOldToken)
	stub.setHooks(func(h *stubHooks) { h.infoBeforeAuth = readGate.hook })
	svc := newOutlineService(t, db)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: url})

	out := runProcessRepo(t, svc)
	waitForSignal(t, readGate.started, "documents.info")

	// 模拟 SetDecisionDoc 已经写入新 token、但事务尚未提交：持有配置行的排他锁。
	conn, err := db.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire：%v", err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		conn.Release()
		t.Fatalf("Begin：%v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(t.Context()))
		}
		conn.Release()
	})
	if err := store.PutDecisionDoc(t.Context(), tx, testMasterKey, store.DecisionDoc{
		RepoID: testRepoID, Provider: "outline", BaseURL: url, DocID: testDocID,
		CollectionID: testCollection, CreatedBy: "admin", UpdatedBy: "admin",
	}, rotationNewToken); err != nil {
		t.Fatalf("轮换配置：%v", err)
	}

	readGate.open()
	// 投递收到 401 后进入收尾判定：必须阻塞在配置行的 FOR SHARE 上，而不是用旧快照直接取消。
	waitForConfigRowWaiter(t, db)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("提交轮换：%v", err)
	}
	committed = true

	if err := <-out; err != nil {
		t.Fatalf("ProcessRepo：%v", err)
	}
	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Fatalf("状态 = %s（%s：%s），期望识别为轮换后用新凭据 delivered", v.State, v.LastErrorCode, v.LastError)
	}
	if got := stub.writesFor(rotationOldToken); got != 0 {
		t.Errorf("旧凭据写入了 %d 次，期望 0 次", got)
	}
	if got := stub.writesFor(rotationNewToken); got != 1 {
		t.Errorf("新凭据写入 %d 次，期望 1 次", got)
	}
}

// waitForConfigRowWaiter 等到某个会话阻塞在配置行锁上：收尾判定对 repo_decision_docs 加了 FOR SHARE，正在等用例
// 尚未提交的轮换事务。用无锁 SELECT 做判定的实现不会阻塞，这个等待会超时。
func waitForConfigRowWaiter(t *testing.T, db *store.DB) {
	t.Helper()
	deadline := time.Now().Add(stateWaitTimeout)
	for {
		var waiting int
		if err := db.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event IN ('transactionid','tuple')`).Scan(&waiting); err != nil {
			t.Fatalf("查询 pg_stat_activity：%v", err)
		}
		if waiting >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("投递没有阻塞在配置行锁上：收尾判定没有和配置代次核对放在同一个事务里")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

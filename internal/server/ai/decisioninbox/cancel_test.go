package decisioninbox

import (
	"context"
	"encoding/json"
	"errors"
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

// 发送前取消（审计）：写请求在发出之前就被调用方取消时，条目必须退回 pending（保留冻结正文），
// 绝不能被当成提供方的永久拒绝写成 cancelled。时序靠包装真实的 Outline 客户端确定性地复现。

// deadlineContext 是一个测试用的 context：done 关闭后 Err() 返回 context.DeadlineExceeded。
// 这样可以在 beginAttempt 提交之后、写请求之前"恰好到期"，既确定又不必真的等待。
type deadlineContext struct {
	context.Context
	done chan struct{}
}

func (c *deadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *deadlineContext) Done() <-chan struct{}       { return c.done }
func (c *deadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// cancelBeforeSendWriter 在进入真实写请求之前核对条目已经是 attempted（beginAttempt 已提交），
// 然后结束父 context 并等取消真正传播到派生的 context，再委托给真实的 Outline 客户端。
type cancelBeforeSendWriter struct {
	t      *testing.T
	db     *store.DB
	id     int64
	inner  docprovider.DocumentWriter
	cancel func()
	once   sync.Once
}

func (w *cancelBeforeSendWriter) GetDocument(ctx context.Context, docID string) (docprovider.Document, error) {
	return w.inner.GetDocument(ctx, docID)
}

func (w *cancelBeforeSendWriter) UpdateDocument(ctx context.Context, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
	var state string
	if err := w.db.QueryRow(context.WithoutCancel(ctx),
		`SELECT state FROM dev_decision_syncs WHERE id=$1`, w.id).Scan(&state); err != nil {
		w.t.Fatalf("读取条目状态：%v", err)
	}
	if state != "attempted" {
		w.t.Fatalf("进入写请求时条目状态 = %q，期望 attempted（beginAttempt 已在短事务里提交）", state)
	}
	// 只在第一次写请求时结束 context：派生 context 的取消经 propagateCancel 异步传播，必须等它真正结束，
	// 否则发送前检查可能读到 Err()==nil 的瞬间。第二轮（正常 context）不等待，照常投递。
	first := false
	w.once.Do(func() {
		first = true
		w.cancel()
	})
	if first {
		select {
		case <-ctx.Done():
		case <-time.After(stateWaitTimeout):
			w.t.Fatalf("取消 / 超时没有在写请求之前传播到派生 context")
		}
	}
	return w.inner.UpdateDocument(ctx, req)
}

// docServer 是一个最小的 Outline 站点：documents.info 读正文，documents.update 写正文并计数。
type docServer struct {
	mu      sync.Mutex
	content string
	rev     int64
	updates atomic.Int32
}

func (s *docServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/documents.info":
		s.writeDoc(w)
	case "/api/documents.update":
		s.updates.Add(1)
		var req struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.content, s.rev = req.Text, s.rev+1
		s.writeDoc(w)
	default:
		http.NotFound(w, r)
	}
}

func (s *docServer) writeDoc(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{
		"id": testDocID, "title": "待拍板", "text": s.content, "revision": s.rev,
		"collectionId": testCollection, "publishedAt": "2026-01-01T00:00:00Z",
	}})
}

// lastContent 返回远端当前的正文。
func (s *docServer) lastContent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

func TestWriteCanceledBeforeSendReturnsToPending(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// ctx 返回传给 ProcessRepo 的 context 和"让它在写请求之前结束"的函数。
		ctx  func() (context.Context, func())
		want error
	}{
		{"取消", func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(t.Context())
			return ctx, cancel
		}, context.Canceled},
		{"超时", func() (context.Context, func()) {
			ctx := &deadlineContext{Context: t.Context(), done: make(chan struct{})}
			return ctx, func() { close(ctx.done) }
		}, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t)
			insertFixture(t, db)

			docs := &docServer{content: "## 待拍板\n\n", rev: 5}
			srv := httptest.NewServer(docs)
			defer srv.Close()
			cfg := putConfigAt(t, db, srv.URL, "write-token")
			id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetBaseURL: srv.URL})

			ctx, end := tt.ctx()
			w := &cancelBeforeSendWriter{t: t, db: db, id: id, inner: outlineAt(t, srv.URL), cancel: end}
			svc := newService(t, db, w)

			if err := svc.ProcessRepo(ctx, testRepoID); !errors.Is(err, tt.want) {
				t.Fatalf("ProcessRepo = %v，期望 %v（调用方取消应立即中止该仓库本轮后续条目）", err, tt.want)
			}
			if n := docs.updates.Load(); n != 0 {
				t.Fatalf("发送前取消不应发出 documents.update，实际 %d 次", n)
			}
			got := mustSync(t, db, id)
			if got.State != "pending" {
				t.Fatalf("状态 = %q，期望退回 pending 而不是 cancelled", got.State)
			}
			if got.LastErrorCode != "canceled_before_send" {
				t.Errorf("last_error_code = %q，期望 canceled_before_send", got.LastErrorCode)
			}
			var payload *string
			if err := db.QueryRow(t.Context(), `SELECT payload_md FROM dev_decision_syncs WHERE id=$1`, id).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if payload == nil || *payload == "" {
				t.Fatal("退回 pending 时必须保留冻结的 payload_md")
			}
			frozen := *payload

			// 下一轮用正常 context：必须复用冻结正文只写一次并收敛为 delivered。
			if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
				t.Fatalf("第二轮 ProcessRepo：%v", err)
			}
			if n := docs.updates.Load(); n != 1 {
				t.Errorf("第二轮应只写一次，实际 %d 次", n)
			}
			if content := docs.lastContent(); !strings.Contains(content, frozen) || !strings.Contains(content, got.Marker) {
				t.Errorf("写入内容应复用冻结正文并包含 marker：\n%s", content)
			}
			if v := mustSync(t, db, id); v.State != "delivered" || v.Attempts != 2 {
				t.Errorf("第二轮之后 = %+v，期望 delivered 且 attempts=2", v)
			}
		})
	}
}

// 裸的 context.Canceled（不是 *WriteError）仍按 C19 当作结果不明：绝不退回 pending 重发，
// 而是保持 attempted、等下一轮只读查证。
func TestBareCanceledWriteErrorKeepsC19VerifyPath(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &fakeWriter{doc: newDoc()}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		// 写请求返回裸的 context.Canceled，同时调用方的 context 结束：结果可能已经写出一部分。
		cancel()
		return docprovider.Document{}, context.Canceled
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(ctx, testRepoID); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessRepo：%v", err)
	}
	if _, sets := w.calls(); sets != 1 {
		t.Errorf("UpdateDocument 调用 %d 次，期望 1 次（绝不重发）", sets)
	}
	v := mustSync(t, db, id)
	if v.State != "attempted" || v.LastErrorCode != "update_outcome_unknown" {
		t.Errorf("状态 = %+v，期望保持 attempted 等只读查证", v)
	}
}

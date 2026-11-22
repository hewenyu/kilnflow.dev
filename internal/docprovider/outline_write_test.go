package docprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// UpdateDocument 的协议与错误分类：只发 id / text / lastRevision；只有可信的 CAS 冲突与限流算"确定没写"，
// 其余不确定的结果一律保守归为"可能已写"，绝不能重发。

const testDocID = "9f1c2f7e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

func newOutline(t *testing.T, url string) *Outline {
	t.Helper()
	o, err := NewOutline(Config{BaseURL: url, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestUpdateDocumentSendsOnlyAllowedFields(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r, http.MethodPost, "/api/documents.update")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("解析请求体：%v", err)
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
			"id": testDocID, "title": "清单", "text": "新正文", "revision": 8,
			"collectionId": "col-1", "publishedAt": "2026-01-01T00:00:00Z",
		}})
	}))
	defer srv.Close()

	doc, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{
		ID: testDocID, Text: "新正文", LastRevision: 7,
	})
	if err != nil {
		t.Fatalf("UpdateDocument：%v", err)
	}
	if doc.ID != testDocID || doc.Revision != 8 || doc.Content != "新正文" || !doc.Published {
		t.Errorf("返回的文档 = %+v", doc)
	}
	if len(got) != 3 {
		t.Errorf("请求体字段 = %v，只应包含 id / text / lastRevision", got)
	}
	if got["id"] != testDocID || got["text"] != "新正文" || got["lastRevision"] != float64(7) {
		t.Errorf("请求体 = %v", got)
	}
	for _, banned := range []string{"title", "publish", "collectionId"} {
		if _, ok := got[banned]; ok {
			t.Errorf("请求体不应包含 %s：%v", banned, got)
		}
	}
}

func TestUpdateDocumentValidationDoesNotSend(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	o := newOutline(t, srv.URL)

	for name, req := range map[string]UpdateDocumentRequest{
		"缺少 id":        {Text: "x", LastRevision: 1},
		"lastRevision": {ID: testDocID, Text: "x"},
	} {
		_, err := o.UpdateDocument(context.Background(), req)
		we, ok := errors.AsType[*WriteError](err)
		if !ok || we.Effect != EffectNotApplied || we.Retryable {
			t.Errorf("%s：err = %v，期望确定没写且不可重试", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("本地校验失败时不应发请求，实际 %d 次", calls.Load())
	}
}

func TestUpdateDocumentConflictClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		code      string
		effect    Effect
		retryable bool
		conflict  bool
	}{
		{"CAS 冲突", http.StatusConflict, documentConflictCode, EffectNotApplied, true, true},
		{"未知 409", http.StatusConflict, "other_conflict", EffectMayHaveApplied, false, false},
		{"限流", http.StatusTooManyRequests, rateLimitCode, EffectNotApplied, true, false},
		{"未知 429", http.StatusTooManyRequests, "proxy_limited", EffectMayHaveApplied, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				writeJSON(w, map[string]any{"ok": false, "error": tt.code, "message": "说明"})
			}))
			defer srv.Close()
			_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
			we, ok := errors.AsType[*WriteError](err)
			if !ok {
				t.Fatalf("err = %v，期望 *WriteError", err)
			}
			// 只有白名单错误码保留；未知错误码（及其远端 message）一律丢弃。
			wantCode := tt.code
			if !safeOutlineCodes[wantCode] {
				wantCode = ""
			}
			if we.Effect != tt.effect || we.Retryable != tt.retryable || we.Status != tt.status || we.Code != wantCode {
				t.Errorf("WriteError = %+v", we)
			}
			if strings.Contains(err.Error(), "说明") {
				t.Errorf("错误文本不应包含远端 message：%v", err)
			}
			if IsDocumentConflict(err) != tt.conflict {
				t.Errorf("IsDocumentConflict = %t，期望 %t", IsDocumentConflict(err), tt.conflict)
			}
		})
	}
}

func TestUpdateDocumentRateLimitRetryAfter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, map[string]any{"ok": false, "error": rateLimitCode, "message": "慢一点"})
	}))
	defer srv.Close()
	_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
	we, ok := errors.AsType[*WriteError](err)
	if !ok || we.RetryAfter != 42*time.Second || !IsRateLimited(err) {
		t.Fatalf("err = %v，期望可信限流且 Retry-After=42s", err)
	}
}

func TestUpdateDocumentDeterministicRejection(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			writeJSON(w, map[string]any{"ok": false, "error": "invalid_request", "message": "拒绝"})
		}))
		_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
		srv.Close()
		we, ok := errors.AsType[*WriteError](err)
		if !ok || we.Effect != EffectNotApplied || we.Retryable {
			t.Errorf("HTTP %d：err = %v，期望确定没写且不可重试", status, err)
		}
	}
}

func TestUpdateDocumentUncertainResults(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			writeJSON(w, map[string]any{"ok": false, "error": "boom", "message": "服务端错误"})
		}))
		_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
		srv.Close()
		we, ok := errors.AsType[*WriteError](err)
		if !ok || we.Effect != EffectMayHaveApplied {
			t.Errorf("HTTP %d：err = %v，期望结果不明", status, err)
		}
	}
}

func TestUpdateDocumentBadSuccessBody(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"不是 JSON":     "not json",
		"缺少 id":       `{"ok":true,"data":{"revision":3}}`,
		"缺少 revision": `{"ok":true,"data":{"id":"` + testDocID + `"}}`,
		// 返回的是另一篇文档：绝不能当作我们写的那篇成功。
		"文档 id 不一致": `{"ok":true,"data":{"id":"00000000-0000-4000-8000-000000000000","revision":3}}`,
	}
	for name, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
		srv.Close()
		we, ok := errors.AsType[*WriteError](err)
		if !ok || we.Effect != EffectMayHaveApplied {
			t.Errorf("%s：err = %v，期望结果不明", name, err)
		}
	}
}

func TestUpdateDocumentRedirectNotReplayed(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/api/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
	if calls.Load() != 1 {
		t.Errorf("重定向后不应重放 POST，实际请求 %d 次", calls.Load())
	}
	we, ok := errors.AsType[*WriteError](err)
	if !ok || we.Effect != EffectMayHaveApplied {
		t.Fatalf("err = %v，期望结果不明", err)
	}
}

func TestUpdateDocumentNetworkErrorAndNoTokenLeak(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := newOutline(t, url).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
	we, ok := errors.AsType[*WriteError](err)
	if !ok || we.Effect != EffectMayHaveApplied {
		t.Fatalf("err = %v，期望网络错误归为结果不明", err)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("错误文本泄露了 token：%v", err)
	}
}

func TestPermanentReadError(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]bool{
		http.StatusUnauthorized: true, http.StatusForbidden: true, http.StatusNotFound: true,
		http.StatusBadRequest: true, http.StatusUnprocessableEntity: true,
		http.StatusTooManyRequests: false, http.StatusInternalServerError: false, http.StatusBadGateway: false,
	} {
		err := &Error{Status: status}
		if got := PermanentReadError(err); got != want {
			t.Errorf("PermanentReadError(HTTP %d) = %t，期望 %t", status, got, want)
		}
	}
	if PermanentReadError(errors.New("网络错误")) {
		t.Error("非 Outline 错误不应判为永久拒绝")
	}
}

// 远端在错误响应里回显 Authorization 或正文时，错误文本和字段都不能带出来。
func TestUpdateDocumentRemoteEchoDoesNotLeak(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests, http.StatusBadGateway} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			writeJSON(w, map[string]any{
				"ok":      false,
				"error":   "some_proxy_error",
				"message": "Bearer test-token 被拒绝；正文 " + r.Header.Get("Authorization"),
			})
		}))
		_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "机密正文", LastRevision: 1})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d：期望错误", status)
		}
		if strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "机密正文") {
			t.Errorf("HTTP %d：错误文本泄露了凭据或正文：%v", status, err)
		}
		we, ok := errors.AsType[*WriteError](err)
		if !ok {
			t.Fatalf("HTTP %d：err = %v", status, err)
		}
		if strings.Contains(we.Message, "test-token") || we.Code == "some_proxy_error" {
			t.Errorf("HTTP %d：WriteError = %+v", status, we)
		}
	}
}

// 409 / 429 与确定性 4xx 的响应体损坏或部分解码失败时，不能因为字段恰好匹配就当作"确定没写"。
func TestUpdateDocumentPartialErrorEnvelopeIsUncertain(t *testing.T) {
	t.Parallel()
	bodies := map[string]func(code string) string{
		"截断的 JSON": func(code string) string { return `{"ok":false,"error":"` + code + `"` },
		"字段类型错误":   func(code string) string { return `{"ok":false,"error":"` + code + `","message":123}` },
	}
	// 每个状态配一个"如果解析成功就会命中对应分支"的错误码：删除 decodeErr 判断时所有分支都会失败。
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusConflict, documentConflictCode},
		{http.StatusTooManyRequests, rateLimitCode},
		{http.StatusBadRequest, "invalid_request"},
		{http.StatusUnauthorized, "authentication_required"},
		{http.StatusForbidden, "authorization_error"},
		{http.StatusNotFound, "not_found"},
		{http.StatusUnprocessableEntity, "validation_error"},
	}
	for bodyName, body := range bodies {
		for _, tt := range cases {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(body(tt.code)))
			}))
			_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
			srv.Close()
			we, ok := errors.AsType[*WriteError](err)
			if !ok || we.Effect != EffectMayHaveApplied {
				t.Errorf("%s / HTTP %d：err = %v，期望结果不明", bodyName, tt.status, err)
			}
		}
	}
}

// 异常大的 Retry-After 不能溢出成负数或 0。
func TestParseRetryAfterOverflow(t *testing.T) {
	t.Parallel()
	if d := parseRetryAfter("999999999999999999999"); d != 0 {
		t.Errorf("无法解析的秒数 = %s，期望 0", d)
	}
	if d := parseRetryAfter("99999999999"); d <= 0 {
		t.Errorf("超大的秒数应得到正的上限值，实际 %s", d)
	}
	if d := parseRetryAfter("42"); d != 42*time.Second {
		t.Errorf("parseRetryAfter(42) = %s", d)
	}
}

// 远端把 token 反射进 302 的 Location 时，读写错误都不能把它带出来；重定向本身也不能被跟随。
func TestRedirectLocationIsNotLeaked(t *testing.T) {
	t.Parallel()
	const secret = "secret-tok-xyz"
	calls := map[string]func(*Outline) error{
		"documents.info": func(o *Outline) error {
			_, err := o.GetDocument(context.Background(), testDocID)
			return err
		},
		"documents.update": func(o *Outline) error {
			_, err := o.UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
			return err
		},
	}
	// 审计场景：token 被反射成 Location 的端口（非法地址），跟随重定向会让 Go 把 Location 拼进错误。
	for name, call := range calls {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Location", "http://127.0.0.1:"+secret)
			w.WriteHeader(http.StatusFound)
		}))
		err := call(newOutline(t, srv.URL))
		srv.Close()
		if err == nil {
			t.Fatalf("%s：3xx 应返回错误", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s：错误文本泄露了 Location：%v", name, err)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("%s：请求 %d 次，重定向不应被跟随", name, n)
		}
	}
	// 合法地址的重定向也不能跟随：Location 指向另一个服务器，读和写都不应碰它。
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})
	}))
	defer target.Close()
	for name, call := range calls {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target.URL+"/"+secret)
			w.WriteHeader(http.StatusFound)
		}))
		err := call(newOutline(t, srv.URL))
		srv.Close()
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%s：err = %v", name, err)
		}
	}
	if n := followed.Load(); n != 0 {
		t.Errorf("重定向被跟随了 %d 次，读写都不应跟随 3xx", n)
	}
}

// 传输层错误（DNS / 连接失败 / 超时）只保留固定文案，不携带 URL。
func TestTransportErrorIsRedacted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := newOutline(t, url).GetDocument(context.Background(), testDocID)
	if err == nil {
		t.Fatal("连接失败应返回错误")
	}
	if strings.Contains(err.Error(), url) || strings.Contains(err.Error(), "connection refused") {
		t.Errorf("错误文本不应包含底层网络细节：%v", err)
	}
	if !errors.Is(err, errOutlineTransport) {
		t.Errorf("错误应是本地固定的传输错误：%v", err)
	}
	_, werr := newOutline(t, url).UpdateDocument(context.Background(), UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
	we, ok := errors.AsType[*WriteError](werr)
	if !ok {
		t.Fatalf("写错误 = %v，期望 *WriteError", werr)
	}
	if we.Effect != EffectMayHaveApplied || strings.Contains(we.Message, "connection refused") || strings.Contains(we.Message, "127.0.0.1") {
		t.Errorf("写错误 = %+v", we)
	}
}

// IsAuthRejected 只把「确定没有写入的鉴权拒绝」当作换凭据重试的信号（同目标轮换 token 的并发场景）。
func TestIsAuthRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"读取 401", &Error{Status: http.StatusUnauthorized}, true},
		{"读取 403", &Error{Status: http.StatusForbidden}, true},
		{"读取 404", &Error{Status: http.StatusNotFound}, false},
		{"写 401（确定没写）", &WriteError{Effect: EffectNotApplied, Status: http.StatusUnauthorized}, true},
		{"写 403（确定没写）", &WriteError{Effect: EffectNotApplied, Status: http.StatusForbidden}, true},
		{"写 401 但结果不明", &WriteError{Effect: EffectMayHaveApplied, Status: http.StatusUnauthorized}, false},
		{"写 400", &WriteError{Effect: EffectNotApplied, Status: http.StatusBadRequest}, false},
		{"非 Outline 错误", errors.New("网络错误"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsAuthRejected(tt.err); got != tt.want {
				t.Errorf("IsAuthRejected(%v) = %t，期望 %t", tt.err, got, tt.want)
			}
		})
	}

	// 响应损坏的 401 无法确认是否写入：必须归为结果不明，不能当成鉴权拒绝去换凭据重试。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	_, err := newOutline(t, srv.URL).UpdateDocument(context.Background(), UpdateDocumentRequest{
		ID: testDocID, Text: "x", LastRevision: 1,
	})
	we, ok := errors.AsType[*WriteError](err)
	if !ok || we.Effect != EffectMayHaveApplied {
		t.Fatalf("err = %v，期望结果不明", err)
	}
	if IsAuthRejected(err) {
		t.Error("损坏的 401 响应不能当成鉴权拒绝")
	}
}

// 发送前本地取消 / 超时：确定没有写、可重试，errors.Is 能识别取消，且绝不发 HTTP 请求。
// 调用方据此退回 pending（保留冻结正文），而不是把本地取消当成提供方的永久拒绝。
func TestUpdateDocumentCanceledBeforeSend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{"取消", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, context.Canceled},
		{"超时", func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			return ctx
		}, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			_, err := newOutline(t, srv.URL).UpdateDocument(tt.ctx(), UpdateDocumentRequest{
				ID: testDocID, Text: "机密正文", LastRevision: 1,
			})
			we, ok := errors.AsType[*WriteError](err)
			if !ok || we.Effect != EffectNotApplied || !we.Retryable {
				t.Fatalf("err = %v，期望确定没写且可重试", err)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("errors.Is(%v) 应为 %v", err, tt.want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("发送前取消不应发请求，实际 %d 次", n)
			}
			if strings.Contains(err.Error(), "机密正文") || strings.Contains(we.Message, srv.URL) {
				t.Errorf("错误不应泄露正文或地址：%v", err)
			}
		})
	}
}

// 传输中取消仍可能已经写出：EffectMayHaveApplied，但 errors.Is 依然能识别取消；底层 *url.Error 的
// URL / token 不能出现在错误文本里（cause 只保留标准哨兵错误）。
func TestUpdateDocumentCanceledInFlight(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	defer srv.Close()
	o := newOutline(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := o.UpdateDocument(ctx, UpdateDocumentRequest{ID: testDocID, Text: "x", LastRevision: 1})
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("请求没有到达服务端")
	}
	cancel()
	err := <-errc
	close(release)
	we, ok := errors.AsType[*WriteError](err)
	if !ok || we.Effect != EffectMayHaveApplied {
		t.Fatalf("err = %v，期望结果不明", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("传输中取消应保留 context.Canceled 判别：%v", err)
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "test-token") {
		t.Errorf("错误文本泄露了地址或 token：%v", err)
	}
	if strings.Contains(we.Message, "http") {
		t.Errorf("Message 不应带底层网络细节：%q", we.Message)
	}
}

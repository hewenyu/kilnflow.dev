package docprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeJSON 写一个 JSON 响应（测试用）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// assertAuth 检查请求的认证头、方法和路径（测试用）。
func assertAuth(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method {
		t.Errorf("方法 = %s，期望 %s", r.Method, method)
	}
	if r.URL.Path != path {
		t.Errorf("路径 = %s，期望 %s", r.URL.Path, path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q，期望 %q", got, "Bearer test-token")
	}
}

func TestListCollectionsPagination(t *testing.T) {
	page1 := make([]map[string]string, 0, outlinePageLimit)
	for i := range outlinePageLimit {
		page1 = append(page1, map[string]string{
			"id":   fmt.Sprintf("c%03d", i),
			"name": fmt.Sprintf("集合 %d", i),
			"url":  fmt.Sprintf("https://outline.example.com/collection/c%03d", i),
		})
	}
	page2 := []map[string]string{
		{"id": "c100", "name": "集合 100", "url": "https://outline.example.com/collection/c100"},
		{"id": "c101", "name": "集合 101", "url": "https://outline.example.com/collection/c101"},
	}

	var offsets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r, http.MethodPost, "/api/collections.list")
		var req struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败：%v", err)
			return
		}
		if req.Limit != outlinePageLimit {
			t.Errorf("limit = %d，期望 %d", req.Limit, outlinePageLimit)
		}
		offsets = append(offsets, req.Offset)
		switch req.Offset {
		case 0:
			writeJSON(w, map[string]any{"ok": true, "data": page1})
		case outlinePageLimit:
			writeJSON(w, map[string]any{"ok": true, "data": page2})
		default:
			t.Errorf("意外的 offset %d", req.Offset)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	got, err := o.ListCollections(context.Background())
	if err != nil {
		t.Fatalf("ListCollections 失败：%v", err)
	}
	if len(got) != 102 {
		t.Fatalf("文档集数量 = %d，期望 102", len(got))
	}
	if got[0] != (Collection{ID: "c000", Name: "集合 0", URL: "https://outline.example.com/collection/c000"}) {
		t.Errorf("第一个 = %+v", got[0])
	}
	if got[101] != (Collection{ID: "c101", Name: "集合 101", URL: "https://outline.example.com/collection/c101"}) {
		t.Errorf("最后一个 = %+v", got[101])
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != outlinePageLimit {
		t.Errorf("offsets = %v，期望 [0 %d]", offsets, outlinePageLimit)
	}
}

func TestListDocumentsPaginationAndMapping(t *testing.T) {
	page1 := make([]map[string]any, 0, outlinePageLimit)
	for i := range outlinePageLimit {
		page1 = append(page1, map[string]any{
			"id":          fmt.Sprintf("d%03d", i),
			"title":       fmt.Sprintf("文档 %d", i),
			"revision":    i + 1,
			"url":         fmt.Sprintf("https://outline.example.com/doc/%d", i),
			"text":        "列表接口不应返回正文，这段即使出现也不能被采用",
			"publishedAt": "2026-01-02T03:04:05.000Z",
		})
	}
	page2 := []map[string]any{
		{"id": "d100", "title": "文档 100", "revision": 7, "url": "https://outline.example.com/doc/100"},
	}

	var offsets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r, http.MethodPost, "/api/documents.list")
		var req struct {
			CollectionID string `json:"collectionId"`
			Limit        int    `json:"limit"`
			Offset       int    `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败：%v", err)
			return
		}
		if req.CollectionID != "col-1" {
			t.Errorf("collectionId = %q，期望 col-1", req.CollectionID)
		}
		if req.Limit != outlinePageLimit {
			t.Errorf("limit = %d，期望 %d", req.Limit, outlinePageLimit)
		}
		offsets = append(offsets, req.Offset)
		switch req.Offset {
		case 0:
			writeJSON(w, map[string]any{"ok": true, "data": page1})
		case outlinePageLimit:
			writeJSON(w, map[string]any{"ok": true, "data": page2})
		default:
			t.Errorf("意外的 offset %d", req.Offset)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	got, err := o.ListDocuments(context.Background(), "col-1")
	if err != nil {
		t.Fatalf("ListDocuments 失败：%v", err)
	}
	if len(got) != 101 {
		t.Fatalf("文档数量 = %d，期望 101", len(got))
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != outlinePageLimit {
		t.Errorf("offsets = %v，期望 [0 %d]", offsets, outlinePageLimit)
	}
	first := got[0]
	if first.ID != "d000" || first.Title != "文档 0" || first.Revision != 1 || first.URL != "https://outline.example.com/doc/0" {
		t.Errorf("第一个文档映射错误：%+v", first)
	}
	if first.Content != "" || first.SHA256 != "" {
		t.Errorf("列表结果不应有正文或摘要：%+v", first)
	}
	last := got[100]
	if last.ID != "d100" || last.Revision != 7 {
		t.Errorf("最后一个文档映射错误：%+v", last)
	}
	// page2 的条目没有 publishedAt：缺字段一律为未发布，不能把零值当成已发布。
	if !first.Published || first.Archived || first.Deleted {
		t.Errorf("page1 的发布状态映射错误：%+v", first)
	}
	if last.Published || last.Archived || last.Deleted {
		t.Errorf("缺 publishedAt 的条目应为未发布：%+v", last)
	}
	for i, d := range got {
		if d.Content != "" || d.SHA256 != "" {
			t.Fatalf("第 %d 个文档不应有正文或摘要：%+v", i, d)
		}
	}
}

func TestGetDocument(t *testing.T) {
	const content = "# 设计文档\n\n正文内容。"
	var gotID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r, http.MethodPost, "/api/documents.info")
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败：%v", err)
			return
		}
		gotID = req.ID
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{
			"id":           "doc-42",
			"title":        "设计文档",
			"text":         content,
			"revision":     13,
			"url":          "https://outline.example.com/doc/doc-42",
			"collectionId": "col-1",
			"publishedAt":  "2026-01-02T03:04:05.000Z",
			"archivedAt":   nil,
			"deletedAt":    nil,
		}})
	}))
	defer srv.Close()

	o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	doc, err := o.GetDocument(context.Background(), "doc-42")
	if err != nil {
		t.Fatalf("GetDocument 失败：%v", err)
	}
	if gotID != "doc-42" {
		t.Errorf("请求 id = %q，期望 doc-42", gotID)
	}
	if doc.ID != "doc-42" || doc.Title != "设计文档" || doc.Revision != 13 || doc.URL != "https://outline.example.com/doc/doc-42" {
		t.Errorf("文档映射错误：%+v", doc)
	}
	// collectionId 是「文档是否仍属于绑定的文档集」的唯一依据（文档可能被移到别的集合），必须映射出来。
	if doc.CollectionID != "col-1" {
		t.Errorf("CollectionID = %q，期望 col-1", doc.CollectionID)
	}
	if doc.Content != content {
		t.Errorf("Content = %q，期望 %q", doc.Content, content)
	}
	if !doc.Published || doc.Archived || doc.Deleted {
		t.Errorf("发布状态映射错误：Published=%v Archived=%v Deleted=%v", doc.Published, doc.Archived, doc.Deleted)
	}
	if want := Sum(content); doc.SHA256 != want {
		t.Errorf("SHA256 = %q，期望 %q", doc.SHA256, want)
	}
}

func TestErrorPropagation(t *testing.T) {
	t.Run("HTTP 401 且 ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"ok": false, "error": "unauthorized", "message": "token 无效或已过期"})
		}))
		defer srv.Close()

		o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
		if err != nil {
			t.Fatalf("NewOutline 失败：%v", err)
		}
		_, err = o.ListCollections(context.Background())
		if err == nil {
			t.Fatal("期望返回错误，实际为 nil")
		}
		// 远端 message 可能回显凭据，一律不透传；只保留本地说明和白名单错误码。
		if strings.Contains(err.Error(), "token 无效或已过期") {
			t.Errorf("错误不应包含服务端 message，实际：%v", err)
		}
		if !strings.Contains(err.Error(), "重新认证") {
			t.Errorf("错误应包含本地说明，实际：%v", err)
		}
		var oe *Error
		if !errors.As(err, &oe) {
			t.Fatalf("错误类型应为 *Error，实际 %T", err)
		}
		if oe.Status != http.StatusUnauthorized || oe.Code != "" {
			t.Errorf("Error = %+v", oe)
		}
	})

	t.Run("HTTP 200 但 ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": false, "error": "not_found", "message": "找不到文档"})
		}))
		defer srv.Close()

		o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
		if err != nil {
			t.Fatalf("NewOutline 失败：%v", err)
		}
		_, err = o.GetDocument(context.Background(), "missing")
		if err == nil {
			t.Fatal("期望返回错误，实际为 nil")
		}
		if strings.Contains(err.Error(), "找不到文档") {
			t.Errorf("错误不应包含服务端 message，实际：%v", err)
		}
		if !strings.Contains(err.Error(), "提供方返回错误") {
			t.Errorf("错误应包含本地说明，实际：%v", err)
		}
	})
}

func TestNewOutlineValidationAndTrim(t *testing.T) {
	t.Run("空 BaseURL", func(t *testing.T) {
		if _, err := NewOutline(Config{BaseURL: "   ", Token: "x"}); err == nil {
			t.Fatal("BaseURL 为空时应返回错误")
		}
	})
	t.Run("空 Token", func(t *testing.T) {
		if _, err := NewOutline(Config{BaseURL: "https://outline.example.com", Token: "  "}); err == nil {
			t.Fatal("Token 为空时应返回错误")
		}
	})
	t.Run("去掉 BaseURL 结尾斜杠", func(t *testing.T) {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			var req struct {
				Offset int `json:"offset"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			writeJSON(w, map[string]any{"ok": true, "data": []map[string]string{}})
		}))
		defer srv.Close()

		o, err := NewOutline(Config{BaseURL: srv.URL + "///", Token: "test-token"})
		if err != nil {
			t.Fatalf("NewOutline 失败：%v", err)
		}
		if _, err := o.ListCollections(context.Background()); err != nil {
			t.Fatalf("ListCollections 失败：%v", err)
		}
		if path != "/api/collections.list" {
			t.Errorf("路径 = %q，期望 /api/collections.list", path)
		}
	})
}

// ValidateBaseURL：严格校验主机名、凭据、查询参数、片段和明文 http（本机地址除外）。
func TestValidateBaseURL(t *testing.T) {
	t.Parallel()
	good := []string{
		"https://outline.example.com",
		"https://outline.example.com/outline",
		"https://outline.example.com:8443",
		"http://127.0.0.1:8080",
		"http://localhost:3000",
		"http://[::1]:3000",
		"https://127.0.0.1",
	}
	for _, u := range good {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v，应为 nil", u, err)
		}
	}
	bad := []string{
		"", "   ", "outline.example.com", "ftp://outline.example.com",
		"https://", "https://:443", // 只有端口，没有主机名
		"https://user:pw@outline.example.com", // userinfo
		"https://outline.example.com?x=1",     // 查询参数
		"https://outline.example.com#frag",    // 片段
		"https://outline.example.com?",        // 空的查询参数（ForceQuery）
		"https://outline.example.com#",        // 空的片段
		"http://outline.example.com",          // 非本机不允许明文 http
		"https://outline.example.com/\x00",    // 控制字符
		"://bad",
	}
	for _, u := range bad {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("ValidateBaseURL(%q) 应报错", u)
		}
	}
	// 错误信息不回显原始地址中的可疑内容
	err := ValidateBaseURL("https://user:secret@outline.example.com")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("错误不应回显 userinfo：%v", err)
	}
}

// 超过上限的响应在解析前失败，错误里不含远端响应体。
func TestPostRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"data":"`)
		_, _ = io.WriteString(w, strings.Repeat("A", outlineMaxResponseBytes))
		_, _ = io.WriteString(w, `"}`)
	}))
	defer srv.Close()

	o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	_, err = o.ListCollections(context.Background())
	if err == nil {
		t.Fatal("超大响应应报错")
	}
	if !errors.Is(err, errOutlineTooLarge) {
		t.Errorf("错误应包装 errOutlineTooLarge：%v", err)
	}
	if strings.Contains(err.Error(), "AAAA") {
		t.Error("错误不应包含远端响应体")
	}
}

// 读取响应体时出错（例如调用方取消）要保留 context 错误链，不能吞成普通的传输错误。
func TestPostReadErrorPreservesContext(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(failingBody{})}, nil
	})}
	o, err := NewOutline(Config{BaseURL: "https://outline.example.com", Token: "test-token", HTTP: hc})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	_, err = o.ListCollections(context.Background())
	if err == nil {
		t.Fatal("读取响应体失败应报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误应保留 context.Canceled：%v", err)
	}
	if !strings.Contains(err.Error(), "读取 Outline") {
		t.Errorf("错误应说明是读取响应失败：%v", err)
	}
}

// failingBody 在读取时返回 context.Canceled（模拟取消 / 超时导致的读取中断）。
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, context.Canceled }
func (failingBody) Close() error             { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "data": []map[string]string{}})
	}))
	defer srv.Close()

	o, err := NewOutline(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewOutline 失败：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := o.ListCollections(ctx); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("错误应可识别为 context.Canceled，实际：%v", err)
	}
}

// TestSuccessWithoutOKField：Outline 的成功响应不一定带 ok（collections.list / documents.list 就没有），
// 不能因为没有 ok 就当作失败。这里模拟真实响应的形状（只有 data，没有 ok）。
func TestSuccessWithoutOKField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/collections.list":
			writeJSON(w, map[string]any{
				"pagination": map[string]any{"limit": 100, "offset": 0, "total": 1},
				"data":       []map[string]any{{"id": "c1", "name": "设计"}},
			})
		case "/api/documents.list":
			writeJSON(w, map[string]any{
				"pagination": map[string]any{"limit": 100, "offset": 0, "total": 1},
				"data":       []map[string]any{{"id": "d1", "title": "需求", "revision": 3}},
			})
		case "/api/documents.info":
			writeJSON(w, map[string]any{"data": map[string]any{"id": "d1", "title": "需求", "text": "# hi", "revision": 3}})
		default:
			t.Errorf("意外的请求 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p, err := NewOutline(Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cols, err := p.ListCollections(ctx)
	if err != nil || len(cols) != 1 || cols[0].ID != "c1" {
		t.Fatalf("ListCollections = %+v, %v", cols, err)
	}
	docs, err := p.ListDocuments(ctx, "c1")
	if err != nil || len(docs) != 1 || docs[0].Revision != 3 {
		t.Fatalf("ListDocuments = %+v, %v", docs, err)
	}
	full, err := p.GetDocument(ctx, "d1")
	if err != nil || full.Content != "# hi" || full.SHA256 != Sum("# hi") {
		t.Fatalf("GetDocument = %+v, %v", full, err)
	}
	// 响应中没有 publishedAt：按未发布处理（冻结时整批拒绝，不把零值当成已发布）。
	if full.Published || full.Archived || full.Deleted {
		t.Fatalf("缺状态字段时应为未发布：%+v", full)
	}
}

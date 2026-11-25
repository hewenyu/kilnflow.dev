package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 外部文档的可视化入口：设置页的绑定与验证、设计依据页面数据、按需读取正文、安全链接、权限与零外部调用。

const docsMasterKey = "0123456789abcdef0123456789abcdef"

func TestSafeDocHref(t *testing.T) {
	t.Parallel()
	const base = "https://outline.example.com"
	other := "https://evil.example.com"
	// baseSub 模拟反向代理下的子路径部署（例如 https://example.com/outline）。
	const baseSub = "https://example.com/outline"
	cases := []struct{ name, base, raw, want string }{
		{"相对路径", base, "/doc/abc", base + "/doc/abc"},
		{"集合路径", base, "/collection/c1", base + "/collection/c1"},
		{"同源绝对地址", base, base + "/doc/abc", base + "/doc/abc"},
		{"查询保留", base, "/doc/abc?rev=2", base + "/doc/abc?rev=2"},
		{"默认端口同源", "https://outline.example.com:443", "https://outline.example.com/doc/a", "https://outline.example.com/doc/a"},
		{"历史未知来源的绝对地址", "", "https://outline.example.com/doc/a", "https://outline.example.com/doc/a"},
		{"历史未知来源的子路径地址", "", "https://example.com/outline/doc/a", "https://example.com/outline/doc/a"},
		{"历史未知来源的相对路径", "", "/doc/a", ""},
		{"跨站绝对地址", base, other + "/doc/a", ""},
		{"javascript", base, "javascript:alert(1)", ""},
		{"协议相对", base, "//evil.example.com/doc/a", ""},
		{"userinfo", base, "https://user:pass@outline.example.com/doc/a", ""},
		{"反斜杠", base, `https://outline.example.com\doc\a`, ""},
		{"控制字符", base, "/doc/a\x00b", ""},
		{"其他相对路径", base, "/admin/users", ""},
		{"目录穿越", base, "/doc/../admin", ""},
		{"编码穿越", base, "/doc/%2e%2e/admin", ""},
		{"大小写编码穿越", base, "/doc/%2E%2E/admin", ""},
		{"编码分隔符", base, "/doc/a%2fb", ""},
		{"编码反斜杠", base, "/doc/a%5Cb", ""},
		{"空", base, "", ""},
		// 子路径部署：相对路径要拼在 /outline 之后，不能丢掉前缀。
		{"子路径相对地址", baseSub, "/doc/a", baseSub + "/doc/a"},
		{"子路径带尾斜杠", baseSub + "/", "/doc/a", baseSub + "/doc/a"},
		{"子路径已带前缀", baseSub, "/outline/doc/a", baseSub + "/doc/a"},
		{"子路径集合地址", baseSub, "/collection/c1", baseSub + "/collection/c1"},
		{"子路径绝对地址", baseSub, baseSub + "/doc/a", baseSub + "/doc/a"},
		{"相似前缀相对地址", baseSub, "/outline2/doc/a", ""},
		{"相似前缀绝对地址", baseSub, "https://example.com/outline2/doc/a", ""},
		{"同源非文档路径", baseSub, "https://example.com/admin", ""},
		{"同源根路径文档", baseSub, "https://example.com/doc/a", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := safeDocHref(c.base, c.raw); got != c.want {
				t.Errorf("safeDocHref(%q, %q) = %q，期望 %q", c.base, c.raw, got, c.want)
			}
		})
	}
}

// TestSafeSiteHref：站点首页链接保留子路径部署的前缀，非法地址不生成链接。
func TestSafeSiteHref(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"普通站点", "https://outline.example.com", "https://outline.example.com"},
		{"子路径部署", "https://example.com/outline/", "https://example.com/outline/"},
		{"userinfo", "https://user:pass@example.com", ""},
		{"非 http", "javascript:alert(1)", ""},
		{"反斜杠", `https://example.com\outline`, ""},
		{"目录穿越", "https://example.com/outline/../admin", ""},
		{"空", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := safeSiteHref(c.in); got != c.want {
				t.Errorf("safeSiteHref(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// fakeWebDocProvider 是显式验证用的假文档客户端。
type fakeWebDocProvider struct {
	collections []docprovider.Collection
	docs        []docprovider.Document
	full        map[string]docprovider.Document
}

func (p *fakeWebDocProvider) ListCollections(context.Context) ([]docprovider.Collection, error) {
	return p.collections, nil
}

func (p *fakeWebDocProvider) ListDocuments(context.Context, string) ([]docprovider.Document, error) {
	return p.docs, nil
}

func (p *fakeWebDocProvider) GetDocument(_ context.Context, id string) (docprovider.Document, error) {
	if d, ok := p.full[id]; ok {
		return d, nil
	}
	return docprovider.Document{}, &docprovider.Error{Status: http.StatusNotFound}
}

// bindDocs 给 tester/demo 绑定文档集。
func (e *webEnv) bindDocs() {
	e.t.Helper()
	actor := "alice"
	if err := store.PutDocBinding(e.ctx, e.db, []byte(docsMasterKey), store.DocBinding{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example.com",
		CollectionID: "col-1", CollectionName: "设计", CreatedBy: "alice", UpdatedBy: &actor,
	}, "outline-read-token-0123456789"); err != nil {
		e.t.Fatal(err)
	}
}

func TestSettingsRepoDocsVerify(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.bindDocs()

	// 未验证：仓库列表显示"已绑定（未验证）"的状态字段，并有外部文档入口。
	w := e.get(e.alice, "/settings/repos")
	if w.Code != http.StatusOK {
		t.Fatalf("仓库列表 = %d：%s", w.Code, w.Body.String())
	}
	data := pageData[struct {
		Repos []struct {
			FullName       string
			DocBound       bool
			DocCheckStatus string
			DecisionDoc    bool
			DocsURL        string
		}
	}](t, w.Body.String())
	if len(data.Repos) == 0 || !data.Repos[0].DocBound || data.Repos[0].DocCheckStatus != "" ||
		data.Repos[0].DocsURL != "/settings/repos/docs?repo=tester%2Fdemo" {
		t.Fatalf("仓库行状态 = %+v", data.Repos)
	}

	// 显式验证：管理员提交表单，只读列举并抽查一篇，完成后回到外部文档页。
	e.docProvider = func(string, string) (docprovider.Provider, error) {
		atomic.AddInt32(&e.docProviderCalls, 1)
		return &fakeWebDocProvider{
			collections: []docprovider.Collection{{ID: "col-1", Name: "设计", URL: "https://outline.example.com/collection/col-1"}},
			docs: []docprovider.Document{
				{ID: "d1", Title: "甲", CollectionID: "col-1", Published: true},
			},
			full: map[string]docprovider.Document{"d1": {ID: "d1", Title: "甲", CollectionID: "col-1", Published: true, Content: "正文"}},
		}, nil
	}
	w = e.post(e.alice, "/settings/repos/docs/verify", url.Values{"repo": {"tester/demo"}})
	if w.Code != http.StatusOK {
		t.Fatalf("验证 = %d：%s", w.Code, w.Body.String())
	}
	var red struct{ Redirect string }
	if err := json.Unmarshal(w.Body.Bytes(), &red); err != nil || !strings.HasPrefix(red.Redirect, "/settings/repos/docs?repo=tester%2Fdemo") {
		t.Fatalf("验证后应回到外部文档页：%v / %s", err, w.Body.String())
	}
	if !strings.Contains(red.Redirect, url.QueryEscape("验证通过")) {
		t.Errorf("提示应说明验证通过：%s", red.Redirect)
	}
	if strings.Contains(w.Body.String(), "outline-read-token") {
		t.Error("提示不应包含 token")
	}
	b, err := store.GetDocBinding(e.ctx, e.db, testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if b.CheckStatus == nil || *b.CheckStatus != "ok" || b.CheckDocCount == nil || *b.CheckDocCount != 1 {
		t.Fatalf("验证结果没有落库：%+v", b)
	}
	if atomic.LoadInt32(&e.docProviderCalls) != 1 {
		t.Errorf("显式验证应只构造一次外部客户端：%d", e.docProviderCalls)
	}

	// 页面展示验证状态与观测到的文档集链接；不泄露 token。
	w = e.get(e.alice, "/settings/repos/docs?repo=tester/demo")
	if w.Code != http.StatusOK {
		t.Fatalf("外部文档页 = %d：%s", w.Code, w.Body.String())
	}
	page := pageData[struct {
		Repo    string
		Active  bool
		Binding *struct {
			CheckStatus      string
			CheckErrorText   string
			CheckDocCount    *int
			CheckSampleDocID string
			CollectionURL    string
			UpdatedBy        string
			CreatedBy        string
			BaseURL          string
			SiteURL          string
		}
		Decision *struct{ Configured bool }
	}](t, w.Body.String())
	if page.Repo != "tester/demo" || page.Binding == nil || page.Binding.CheckStatus != "ok" ||
		page.Binding.CheckDocCount == nil || *page.Binding.CheckDocCount != 1 || page.Binding.CheckSampleDocID != "d1" {
		t.Fatalf("页面绑定 = %+v", page)
	}
	if page.Binding.CollectionURL == "" || page.Binding.SiteURL == "" || page.Binding.UpdatedBy != "alice" {
		t.Errorf("外部链接 / 更新者 = %+v", page.Binding)
	}
	if page.Decision == nil || page.Decision.Configured {
		t.Errorf("未配置待决策清单时 = %+v", page.Decision)
	}
	if strings.Contains(w.Body.String(), "outline-read-token") {
		t.Error("页面不应包含 token")
	}
}

// 验证时的本地内部错误（数据库等）不显示原文：提示里只有错误编号，详细信息只写日志。
func TestRepoDocsVerifyHidesInternalError(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.bindDocs()
	e.exec(`ALTER TABLE repo_doc_bindings RENAME TO repo_doc_bindings_gone`)
	w := e.post(e.alice, "/settings/repos/docs/verify", url.Values{"repo": {"tester/demo"}})
	var red struct{ Redirect string }
	if err := json.Unmarshal(w.Body.Bytes(), &red); err != nil {
		t.Fatalf("解析重定向失败：%v / %s", err, w.Body.String())
	}
	u, err := url.Parse(red.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	flash := u.Query().Get("flash")
	if !strings.Contains(flash, "错误编号") {
		t.Errorf("提示应只给出错误编号：%q", flash)
	}
	if strings.Contains(flash, "repo_doc_bindings") || strings.Contains(flash, "SQL") {
		t.Errorf("提示不应包含数据库错误原文：%q", flash)
	}
	if out := e.log.String(); !strings.Contains(out, "repo_doc_bindings") {
		t.Errorf("详细信息应写进日志：%s", out)
	}
}

// 没有配置 AI 核心时，正文读取接口返回 404，而不是空指针崩溃。
func TestDocContentWithoutAI(t *testing.T) {
	t.Parallel()
	d, _, _ := testDashboard(t, "")
	handlers := map[string]func(http.ResponseWriter, *http.Request, *session){
		"工作项": d.getWorkItemDocContent,
		"请求":  d.getRequestDocContent,
		"任务":  d.getTaskDocContent,
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x/1/docs/content?doc_id=d1", nil)
			req.SetPathValue("id", "1")
			h(rec, req, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("状态 = %d，期望 404：%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSettingsRepoDocsPermissions(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.bindDocs()
	// 成员不能访问设置页，也不能提交验证。
	if w := e.get(e.bob, "/settings/repos/docs?repo=tester/demo"); w.Code != http.StatusForbidden {
		t.Errorf("成员访问外部文档页 = %d，期望 403", w.Code)
	}
	if w := e.post(e.bob, "/settings/repos/docs/verify", url.Values{"repo": {"tester/demo"}}); w.Code != http.StatusForbidden {
		t.Errorf("成员提交验证 = %d，期望 403", w.Code)
	}
}

func TestRepoDocsPageUnbound(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	w := e.get(e.alice, "/settings/repos/docs?repo=tester/demo")
	if w.Code != http.StatusOK {
		t.Fatalf("未绑定时 = %d：%s", w.Code, w.Body.String())
	}
	page := pageData[struct {
		Binding  any
		Decision struct{ Configured bool }
	}](t, w.Body.String())
	if page.Binding != nil || page.Decision.Configured {
		t.Fatalf("未绑定时应显示空状态：%+v", page)
	}
	if w := e.get(e.alice, "/settings/repos/docs"); w.Code != http.StatusNotFound {
		t.Errorf("没有 repo 参数 = %d，期望 404", w.Code)
	}
	if w := e.get(e.alice, "/settings/repos/docs?repo=bad/name"); w.Code != http.StatusNotFound {
		t.Errorf("不存在的仓库 = %d，期望 404", w.Code)
	}
}

// seedDocSet 插入一个独立快照集与一篇文档，返回摘要。
func (e *webEnv) seedDocSet(setID int64, docID, content string, purged bool) string {
	e.t.Helper()
	src := docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 1}
	doc := docsync.Snapshot{
		DocID: docID, Title: "设计", Revision: 1, SHA256: docprovider.Sum(content), Content: content,
		URL: "https://outline.example.com/doc/" + docID,
	}
	digest := docsync.ManifestDigest(docsync.SourceSnapshotSet, src, []docsync.Snapshot{doc})
	storedContent := content
	if purged {
		storedContent = ""
	}
	e.exec(`INSERT INTO doc_snapshot_sets (id, repo_id, provider, base_url, collection_id, collection_name, binding_generation, doc_count, manifest_digest, content_purged_at)
		VALUES ($1,$2,'outline',$3,'col-1','设计',1,1,$4,CASE WHEN $5::boolean THEN now() END)`,
		setID, testRepoID, src.BaseURL, digest, purged)
	e.exec(`INSERT INTO doc_snapshot_docs (snapshot_set_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1,$2,'设计',1,$3,$4,$5)`, setID, docID, doc.SHA256, storedContent, doc.URL)
	return digest
}

// seedReviewRequest 插入一个审计请求并引用快照集。
func (e *webEnv) seedReviewRequest(requestID, setID int64, source, digest string) {
	e.t.Helper()
	e.exec(`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES ($1,'review',$2,'manual','refs/heads/main','abc','succeeded',$1)`, requestID, testRepoID)
	e.exec(`INSERT INTO ai_review_requests (request_id, docs_source, doc_snapshot_set_id, docs_manifest_digest)
		VALUES ($1,$2,NULLIF($3,0),$4)`, requestID, source, setID, digest)
}

func TestDocsContentAPI(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	digest := e.seedDocSet(910, "d1", "正文一", false)
	e.seedReviewRequest(900, 910, "snapshot_set", digest)
	e.seedDocSet(911, "x1", "别的文档", false)
	e.seedReviewRequest(901, 911, "snapshot_set", docsync.ManifestDigest(docsync.SourceSnapshotSet,
		docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 1},
		[]docsync.Snapshot{{DocID: "x1", Title: "设计", Revision: 1, SHA256: docprovider.Sum("别的文档"), Content: "别的文档", URL: "https://outline.example.com/doc/x1"}}))
	e.seedReviewRequest(902, 0, "legacy", "")
	e.seedDocSet(912, "p1", "会被清理", true)
	e.seedReviewRequest(903, 912, "snapshot_set", docsync.ManifestDigest(docsync.SourceSnapshotSet,
		docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 1},
		[]docsync.Snapshot{{DocID: "p1", Title: "设计", Revision: 1, SHA256: docprovider.Sum("会被清理"), Content: "会被清理", URL: "https://outline.example.com/doc/p1"}}))
	// 摘要与快照集不一致。
	e.exec(`INSERT INTO doc_snapshot_sets (id, repo_id, provider, base_url, collection_id, collection_name, binding_generation, doc_count, manifest_digest)
		VALUES (913,$1,'outline','https://outline.example.com','col-1','设计',1,0,$2)`, testRepoID, strings.Repeat("a", 64))
	e.seedReviewRequest(904, 913, "snapshot_set", strings.Repeat("b", 64))

	// 未登录 401（JSON）。
	w := e.get(nil, "/ai/requests/900/docs/content?doc_id=d1")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 = %d：%s", w.Code, w.Body.String())
	}
	var e401 docContentError
	if json.Unmarshal(w.Body.Bytes(), &e401) != nil || e401.Code != "unauthorized" {
		t.Errorf("未登录的 JSON 错误 = %s", w.Body.String())
	}

	// 成员可以读取。
	w = e.get(e.bob, "/ai/requests/900/docs/content?doc_id=d1")
	if w.Code != http.StatusOK {
		t.Fatalf("读取正文 = %d：%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("响应头 = %v", w.Header())
	}
	var body docContent
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Content != "正文一" || body.DocID != "d1" {
		t.Fatalf("正文 = %s", w.Body.String())
	}

	// 参数与各种错误。
	cases := []struct {
		name   string
		target string
		status int
		code   string
	}{
		{"缺少 doc_id", "/ai/requests/900/docs/content", 400, "invalid_request"},
		{"控制字符", "/ai/requests/900/docs/content?doc_id=a%00b", 400, "invalid_request"},
		{"不存在", "/ai/requests/900/docs/content?doc_id=nope", 404, "doc_not_found"},
		{"跨父对象", "/ai/requests/900/docs/content?doc_id=x1", 404, "doc_not_found"},
		{"legacy 没有依据", "/ai/requests/902/docs/content?doc_id=d1", 404, "no_basis"},
		{"正文已清理", "/ai/requests/903/docs/content?doc_id=p1", 410, "content_purged"},
		{"摘要冲突", "/ai/requests/904/docs/content?doc_id=d1", 409, "basis_conflict"},
		{"不存在的请求", "/ai/requests/9999/docs/content?doc_id=d1", 404, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := e.get(e.bob, c.target)
			if w.Code != c.status {
				t.Fatalf("%s = %d，期望 %d：%s", c.name, w.Code, c.status, w.Body.String())
			}
			var ec docContentError
			if err := json.Unmarshal(w.Body.Bytes(), &ec); err != nil || ec.Code != c.code {
				t.Errorf("%s 的错误码 = %s", c.name, w.Body.String())
			}
		})
	}

	// 页面数据与刷新不构造外部客户端、不带正文。
	before := atomic.LoadInt32(&e.docProviderCalls)
	for range 3 {
		if w := e.get(e.bob, "/ai/requests/900"); w.Code != http.StatusOK {
			t.Fatalf("请求页 = %d：%s", w.Code, w.Body.String())
		}
	}
	if got := atomic.LoadInt32(&e.docProviderCalls); got != before {
		t.Errorf("普通页面 GET 不应构造外部文档客户端：%d -> %d", before, got)
	}
	w = e.get(e.bob, "/ai/requests/900")
	page := pageData[struct {
		DesignBasis *struct {
			DeclaredSource string
			Error          uiText
			Basis          *struct {
				Source   string
				DocCount int
				Docs     []struct {
					DocID, Title, URL, SHA256 string
					Purged                    bool
				}
			}
		}
	}](t, w.Body.String())
	if page.DesignBasis == nil || page.DesignBasis.Basis == nil || page.DesignBasis.Basis.Source != "snapshot_set" {
		t.Fatalf("请求页的设计依据 = %+v", page.DesignBasis)
	}
	if len(page.DesignBasis.Basis.Docs) != 1 || page.DesignBasis.Basis.Docs[0].DocID != "d1" || page.DesignBasis.Basis.Docs[0].URL == "" {
		t.Errorf("页面文档元数据 = %+v", page.DesignBasis.Basis.Docs)
	}
	if strings.Contains(w.Body.String(), `"content"`) {
		t.Error("页面数据不应包含正文")
	}
}

func TestDocsContentTaskAndWorkItem(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	digest := e.seedDocSet(920, "d1", "正文", false)
	e.seedReviewRequest(920, 920, "snapshot_set", digest)
	e.exec(`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES (921,920,'review','review',ARRAY['darwin/arm64'],'codex','审计',$1, 'succeeded',60)`,
		`{"docs_source":"snapshot_set","doc_snapshot_set_id":920,"docs_manifest_digest":"`+digest+`"}`)
	e.exec(`INSERT INTO ai_attempts (task_id, epoch, runner_name, acquire_request_id) VALUES (921,1,'node','acq')`)

	// 任务详情页有依据；任务正文可以读。
	w := e.get(e.bob, "/ai/tasks/921")
	if w.Code != http.StatusOK {
		t.Fatalf("任务页 = %d：%s", w.Code, w.Body.String())
	}
	page := pageData[struct {
		DesignBasis *struct {
			BasisSource string
			Error       uiText
			Basis       *struct {
				Source   string
				DocCount int
			}
		}
	}](t, w.Body.String())
	if page.DesignBasis == nil || !page.DesignBasis.Error.IsZero() || page.DesignBasis.Basis == nil || page.DesignBasis.Basis.Source != "snapshot_set" {
		t.Fatalf("任务页的设计依据 = %+v", page.DesignBasis)
	}
	w = e.get(e.bob, "/ai/tasks/921/docs/content?doc_id=d1")
	if w.Code != http.StatusOK {
		t.Fatalf("任务正文 = %d：%s", w.Code, w.Body.String())
	}
	// 别的任务不能借这份依据（任务不属于该请求）。
	if w := e.get(e.bob, "/ai/tasks/9999/docs/content?doc_id=d1"); w.Code == http.StatusOK {
		t.Error("不存在的任务不应读到正文")
	}

	// 工作项页面的依据与关联任务。
	e.exec(`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES (930,'develop',$1,'push','refs/heads/main','abc','succeeded',930)`, testRepoID)
	e.exec(`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES (931,930,'develop','develop',ARRAY['darwin/arm64'],'claude-code','选题','{"mode":"select"}','succeeded',60)`)
	e.exec(`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (932,931,1,'node','acq')`)
	e.exec(`INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES (933,$1,'developing','main','abc',2,'claude-code',932)`, testRepoID)
	e.exec(`INSERT INTO ai_dev_requests (request_id, mode, work_item_id) VALUES (930,'select',NULL)`)
	src := docsync.ManifestSource{Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "col-1", BindingGeneration: 1}
	doc := docsync.Snapshot{
		DocID: "w1", Title: "工作项设计", Revision: 1, SHA256: docprovider.Sum("工作项正文"), Content: "工作项正文",
		URL: "https://outline.example.com/doc/w1",
	}
	e.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count, provider, base_url, collection_id, collection_name, binding_generation, manifest_digest)
		VALUES (933,1,'outline',$1,'col-1','设计',1,$2)`, src.BaseURL, docsync.ManifestDigest(docsync.SourceWorkItem, src, []docsync.Snapshot{doc}))
	e.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES (933,'w1','工作项设计',1,$1,$2,$3)`, doc.SHA256, doc.Content, doc.URL)

	w = e.get(e.bob, "/ai-dev/933")
	if w.Code != http.StatusOK {
		t.Fatalf("工作项页 = %d：%s", w.Code, w.Body.String())
	}
	wi := pageData[struct {
		DesignBasis *struct {
			Source string
			Docs   []struct {
				DocID, URL string
				Purged     bool
			}
		}
		RelatedTasks []struct {
			TaskID   int64
			Relation string
			TaskURL  string
		}
	}](t, w.Body.String())
	if wi.DesignBasis == nil || wi.DesignBasis.Source != "work_item" || len(wi.DesignBasis.Docs) != 1 {
		t.Fatalf("工作项页的设计依据 = %+v", wi.DesignBasis)
	}
	if len(wi.RelatedTasks) != 1 || wi.RelatedTasks[0].TaskID != 931 || wi.RelatedTasks[0].Relation != "claimed_dynamic" ||
		wi.RelatedTasks[0].TaskURL != "/ai/tasks/931" {
		t.Fatalf("关联任务 = %+v", wi.RelatedTasks)
	}
	if w := e.get(e.bob, "/ai-dev/933/docs/content?doc_id=w1"); w.Code != http.StatusOK {
		t.Errorf("工作项正文 = %d：%s", w.Code, w.Body.String())
	}

	// 另一个工作项 B：开发请求已经绑定 A（933），任务 spec 却写 B，正文接口必须 409，绝不返回 B 的正文。
	e.exec(`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES (935,'develop',$1,'push','refs/heads/main','abc','succeeded',935)`, testRepoID)
	e.exec(`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES (936,935,'fix','develop',ARRAY['darwin/arm64'],'claude-code','返修',
			'{"mode":"implement","work_item_id":934}','succeeded',60)`)
	e.exec(`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (937,936,1,'node','acq')`)
	e.exec(`INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
		VALUES (934,$1,'developing','main','abc',2,'claude-code',937)`, testRepoID)
	e.exec(`INSERT INTO work_item_doc_freezes (work_item_id, doc_count, provider, base_url, collection_id, collection_name, binding_generation, manifest_digest)
		VALUES (934,1,'outline',$1,'col-1','设计',1,$2)`, src.BaseURL,
		docsync.ManifestDigest(docsync.SourceWorkItem, src, []docsync.Snapshot{{
			DocID: "b1", Title: "B", Revision: 1, SHA256: docprovider.Sum("B 的正文"), Content: "B 的正文",
		}}))
	e.exec(`INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES (934,'b1','B',1,$1,'B 的正文','https://outline.example.com/doc/b1')`, docprovider.Sum("B 的正文"))
	e.exec(`INSERT INTO ai_dev_requests (request_id, mode, work_item_id) VALUES (935,'implement',933)`)
	// 请求还没有绑定工作项：选题任务还没领取工作项、非选题任务缺失工作项时，spec 都不能凭编号去读工作项 A。
	e.exec(`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES (938,'develop',$1,'push','refs/heads/main','abc','succeeded',938)`, testRepoID)
	e.exec(`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES (938,938,'select','develop',ARRAY['darwin/arm64'],'claude-code','选题',
			'{"mode":"select","work_item_id":933}','succeeded',60)`)
	e.exec(`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (942,938,1,'node','acq')`)
	e.exec(`INSERT INTO ai_dev_requests (request_id, mode, work_item_id) VALUES (938,'select',NULL)`)
	e.exec(`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, state, family_id)
		VALUES (939,'develop',$1,'push','refs/heads/main','abc','succeeded',939)`, testRepoID)
	e.exec(`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES (939,939,'fix','develop',ARRAY['darwin/arm64'],'claude-code','返修',
			'{"mode":"fix","work_item_id":933}','succeeded',60)`)
	e.exec(`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (943,939,1,'node','acq')`)
	e.exec(`INSERT INTO ai_dev_requests (request_id, mode, work_item_id) VALUES (939,'fix',NULL)`)
	for _, taskID := range []int64{936, 938, 939} {
		for _, docID := range []string{"w1", "b1"} {
			w := e.get(e.bob, fmt.Sprintf("/ai/tasks/%d/docs/content?doc_id=%s", taskID, docID))
			if w.Code != http.StatusConflict {
				t.Errorf("不一致的开发任务 #%d 读取 %s = %d，期望 409：%s", taskID, docID, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "B 的正文") || strings.Contains(w.Body.String(), "工作项正文") {
				t.Errorf("冲突时不应返回正文：%s", w.Body.String())
			}
		}
	}
	wantDocsConflictTexts(t, e)
}

// wantDocsConflictTexts 检查依据冲突的任务 #936（见 TestDocsContentTaskAndWorkItem）在任务页和工作项 #933 的页面上的说明：
// 依据错误是按原因码给出的消息（前端按语言显示），不是领域层拼好的中文；关联任务的关系说明是消息，具体原因单独给出。
func wantDocsConflictTexts(t *testing.T, e *webEnv) {
	t.Helper()
	w := e.get(e.bob, "/ai/tasks/936")
	if w.Code != http.StatusOK {
		t.Fatalf("冲突任务页 = %d：%s", w.Code, w.Body.String())
	}
	conflict := pageData[struct {
		DesignBasis *struct {
			Error uiText
		}
	}](t, w.Body.String())
	// 依据错误是按原因码给出的消息（前端按语言显示），不是领域层拼好的中文
	if conflict.DesignBasis == nil || conflict.DesignBasis.Error.id != msgIssueTaskDevRequest ||
		textZH(t, conflict.DesignBasis.Error) != "AI 任务 #936 的工作项与开发请求 #935 的记录不一致" {
		t.Errorf("冲突任务页应展示依据错误：%+v", conflict.DesignBasis)
	}
	// 工作项页上的关联任务：关系说明是消息，具体原因单独给出
	item := pageData[aiDevItemPage](t, e.get(e.bob, "/ai-dev/933").Body.String())
	var rel *relatedTaskView
	for i := range item.RelatedTasks {
		if item.RelatedTasks[i].TaskID == 936 {
			rel = &item.RelatedTasks[i]
		}
	}
	if rel == nil || rel.Relation != "mismatch" || rel.Note.id != msgNoteBasisConflict || textZH(t, rel.Note) != "任务的文档依据记录与工作项不一致" ||
		textZH(t, rel.NoteDetail) != "AI 任务 #936 的工作项与开发请求 #935 的记录不一致" {
		t.Errorf("关联任务 #936 = %+v", rel)
	}
}

func TestRepoDocsInactiveAndCSRF(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.bindDocs()
	b, err := store.GetDocBinding(e.ctx, e.db, testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	applied, err := store.SaveDocBindingCheck(e.ctx, e.db, testRepoID, b.Generation, store.DocCheckResult{
		StartedAt: now.Add(-time.Second), CheckedAt: now, CheckedBy: "alice",
		Status: "failed", ErrorCode: docsync.DocCheckAuthFailed,
	})
	if err != nil || !applied {
		t.Fatalf("保存验证结果 = %v, %v", applied, err)
	}
	e.exec(`UPDATE repos SET active=false WHERE id=$1`, testRepoID)

	// 停用的仓库仍然有外部文档入口和验证状态，只是不能配置。
	w := e.get(e.alice, "/settings/repos")
	if w.Code != http.StatusOK {
		t.Fatalf("仓库列表 = %d：%s", w.Code, w.Body.String())
	}
	list := pageData[struct {
		Repos []struct {
			FullName       string
			Active         bool
			DocBound       bool
			DocCheckStatus string
			DocsURL        string
		}
	}](t, w.Body.String())
	if len(list.Repos) == 0 || list.Repos[0].Active || !list.Repos[0].DocBound ||
		list.Repos[0].DocCheckStatus != "failed" || list.Repos[0].DocsURL == "" {
		t.Fatalf("停用仓库的文档入口 = %+v", list.Repos)
	}

	w = e.get(e.alice, "/settings/repos/docs?repo=tester/demo")
	if w.Code != http.StatusOK {
		t.Fatalf("停用仓库的外部文档页 = %d：%s", w.Code, w.Body.String())
	}
	page := pageData[struct {
		Active  bool
		Binding *struct{ CheckStatus, CheckErrorText string }
	}](t, w.Body.String())
	if page.Active || page.Binding == nil || page.Binding.CheckStatus != "failed" || page.Binding.CheckErrorText == "" {
		t.Fatalf("停用仓库的验证状态 = %+v", page)
	}
	if strings.Contains(w.Body.String(), "outline-read-token") {
		t.Error("页面不应包含 token")
	}

	// 没有 CSRF token 的验证请求被拒绝，且不访问外部。
	before := atomic.LoadInt32(&e.docProviderCalls)
	resp := serve(e.mux, testReq{
		method: http.MethodPost, target: "/settings/repos/docs/verify",
		form: url.Values{"repo": {"tester/demo"}}, cookie: e.alice.cookie, referer: "https://kiln.example.com/runs",
	})
	if resp.Code != http.StatusForbidden {
		t.Errorf("没有 CSRF 的验证请求 = %d，期望 403", resp.Code)
	}
	if got := atomic.LoadInt32(&e.docProviderCalls); got != before {
		t.Errorf("被拒绝的请求不应访问外部文档：%d -> %d", before, got)
	}
}

func TestRepoDocsDecisionStatus(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	if err := store.PutDecisionDoc(e.ctx, e.db, []byte(docsMasterKey), store.DecisionDoc{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example.com",
		DocID: "11111111-1111-4111-8111-111111111111", CollectionID: "col-d", CreatedBy: "alice", UpdatedBy: "alice",
	}, "write-token-secret"); err != nil {
		t.Fatal(err)
	}
	w := e.get(e.alice, "/settings/repos/docs?repo=tester/demo")
	if w.Code != http.StatusOK {
		t.Fatalf("外部文档页 = %d：%s", w.Code, w.Body.String())
	}
	page := pageData[struct {
		Decision *struct {
			Configured   bool
			DocID        string
			CollectionID string
			DocURL       string
			UpdatedBy    string
		}
	}](t, w.Body.String())
	if page.Decision == nil || !page.Decision.Configured || page.Decision.DocID == "" || page.Decision.DocURL == "" {
		t.Fatalf("待决策清单状态 = %+v", page.Decision)
	}
	if strings.Contains(w.Body.String(), "write-token-secret") {
		t.Error("页面不应包含写 token")
	}
	// 仓库列表也标出待决策清单。
	w = e.get(e.alice, "/settings/repos")
	data := pageData[struct {
		Repos []struct{ DecisionDoc bool }
	}](t, w.Body.String())
	if len(data.Repos) == 0 || !data.Repos[0].DecisionDoc {
		t.Errorf("仓库列表应标出待决策清单：%+v", data.Repos)
	}
}

// 保证测试文件里用到的辅助函数在代码变更时仍然可用。
var (
	_ = fmt.Sprintf
	_ = errors.Is
)

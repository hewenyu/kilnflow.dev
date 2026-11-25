package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 外部设计文档的可视化入口：
//   - /settings/repos/docs：管理员查看一个仓库的只读文档绑定、显式只读连接验证的结果，以及「待决策清单」的配置状态；
//   - /ai-dev/{id}、/ai/requests/{id}、/ai/tasks/{id}：展示冻结的设计依据（不含正文）；
//   - .../docs/content?doc_id=...：按需读取一篇正文。
//
// 普通页面 GET 与 5 秒自动刷新绝不访问外部 Outline：只有管理员显式提交验证表单时才调用只读接口。
// 展示用的外部链接一律由 safeDocHref 校验后生成，前端不直接把原始 URL 放进 href。

// docsContentMaxID 是 doc_id 的长度上限（Outline 的 id 是短字符串 / UUID）。
const docsContentMaxID = 256

// safeSiteHref 校验文档系统站点首页地址：必须是 http / https、有主机名、没有 userinfo、控制字符或目录穿越。
// baseURL 的路径（例如反向代理下的 /outline）会被保留。
func safeSiteHref(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" || hasControl(raw) || strings.Contains(raw, "\\") {
		return ""
	}
	u, ok := parseSafeBase(raw)
	if !ok || traversal(u) {
		return ""
	}
	return u.String()
}

// safeDocHref 把文档系统里的文档 / 文档集地址转换为可以安全放进 <a href> 的地址；不安全时返回空串。
//
// 只允许 http / https，拒绝 userinfo、控制字符、反斜杠、协议相对地址（//）和编码的目录穿越 / 分隔符
// （%2e、%2f、%5c，大小写不敏感）。baseURL 有效时：
//   - 相对路径只接受 /doc/... 或 /collection/...，并拼接到 baseURL 的路径前缀之后（已是同一前缀时不重复拼接）；
//   - 绝对地址必须与 baseURL 同源，且路径在 basePath + /doc/ 或 basePath + /collection/ 下（按段边界判断，
//     /outline2/doc/a 不能绕过 /outline）。
//
// baseURL 为空（历史快照没有记录来源）时只接受路径中带有 /doc/ 或 /collection/ 段的绝对地址；相对路径不生成链接。
func safeDocHref(baseURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || hasControl(raw) || strings.Contains(raw, "\\") || hasBadEncoding(raw) {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	base, baseOK := parseSafeBase(baseURL)
	if u.Scheme == "" {
		// 相对路径：历史来源未知（没有 baseURL）时不生成链接。
		if !baseOK || !strings.HasPrefix(u.Path, "/") || traversal(u) {
			return ""
		}
		basePath := sitePath(base)
		finalPath, ok := docRelativePath(u.Path, basePath)
		if !ok {
			return ""
		}
		resolved := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: finalPath, RawQuery: u.RawQuery}
		return finalDocURL(resolved, base, basePath)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" || traversal(u) {
		return ""
	}
	if baseOK {
		if !sameOrigin(u, base) || !underDocPrefix(u.Path, sitePath(base)) {
			return ""
		}
		return finalDocURL(u, base, sitePath(base))
	}
	if !hasDocSegment(u.Path) {
		return ""
	}
	return finalDocURL(u, nil, "")
}

// parseSafeBase 解析文档系统站点地址：必须是 http/https、有主机名、没有 userinfo。
func parseSafeBase(baseURL string) (*url.URL, bool) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" || hasControl(baseURL) || strings.Contains(baseURL, "\\") {
		return nil, false
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}

// sitePath 返回站点地址的规范化路径前缀：去掉结尾斜杠，根路径为空串。
func sitePath(base *url.URL) string {
	return strings.TrimRight(base.Path, "/")
}

// docRelativePath 把 Outline 返回的站点根相对路径拼到 basePath 之后；不是文档 / 文档集路径时返回 false。
// 已经带有 basePath 前缀的地址不重复拼接。
func docRelativePath(p, basePath string) (string, bool) {
	if basePath != "" && underDocPrefix(p, basePath) {
		return p, true
	}
	if !underDocPrefix(p, "") {
		return "", false
	}
	return basePath + p, true
}

// underDocPrefix 判断路径是否在 basePath + /doc/ 或 basePath + /collection/ 下（按段边界）。
func underDocPrefix(p, basePath string) bool {
	for _, kind := range []string{"/doc/", "/collection/"} {
		if strings.HasPrefix(p, basePath+kind) {
			return true
		}
	}
	return false
}

// hasDocSegment 判断路径中是否有 doc / collection 段，且后面还有内容（历史来源未知时使用）。
func hasDocSegment(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range segs {
		if (seg == "doc" || seg == "collection") && i+1 < len(segs) && segs[i+1] != "" {
			return true
		}
	}
	return false
}

// hasBadEncoding 拒绝编码后的 . / 分隔符 / 反斜杠（%2e、%2f、%5c），防止解码后才出现的穿越或跨段路径。
func hasBadEncoding(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// finalDocURL 返回最终地址并二次校验：必须仍是 http/https、没有 userinfo、没有穿越；base 有效时还要求同源并落在
// 文档路径下。
func finalDocURL(u, base *url.URL, basePath string) string {
	if u == nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if traversal(u) {
		return ""
	}
	if base != nil {
		if !sameOrigin(u, base) || !underDocPrefix(u.Path, basePath) {
			return ""
		}
	} else if !hasDocSegment(u.Path) {
		return ""
	}
	return u.String()
}

// sameOrigin 比较 scheme、host 和端口（默认端口归一）。
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return originPort(a) == originPort(b)
}

func originPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// traversal 拒绝路径中编码的 . / .. 段（解码后再按段判断，防止 %2e%2e 绕过）。
func traversal(u *url.URL) bool {
	p, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return true
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// hasControl 判断字符串里是否有控制字符。
func hasControl(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// docMetaView 是一篇冻结文档的展示元数据（不含正文）。
type docMetaView struct {
	DocID    string
	Title    string
	Revision int64
	SHA256   string
	// URL 是校验后的原文链接；不能安全生成时为空（前端不渲染链接）。
	URL string
	// Purged 表示正文已按保留策略清理。
	Purged bool
}

// designBasisView 是一份冻结依据的展示数据（不含正文）。
type designBasisView struct {
	// Source 是 work_item / snapshot_set / none。
	Source string
	Frozen bool
	// FrozenAt 是冻结时间；没有冻结时为 nil。
	FrozenAt *time.Time
	// Provider / SiteURL / CollectionID / CollectionName 是冻结时记录的来源。
	Provider       string
	SiteURL        string
	CollectionID   string
	CollectionName string
	// CollectionURL 是校验后的文档集链接（没有记录来源或无法安全生成时为空）。
	CollectionURL string
	// SourceKnown 为假表示历史快照没有记录来源，页面不要用当前绑定补造。
	SourceKnown       bool
	BindingGeneration int64
	DocCount          int
	ManifestDigest    string
	ContentPurgedAt   *time.Time
	Docs              []docMetaView
	// IntegrityError 非空表示记录的元数据与实际快照不一致（按原因码给出的消息）。
	IntegrityError uiText
}

// newDesignBasisView 把文档依据视图转换为页面 DTO，并生成校验后的外部链接。
func newDesignBasisView(v *docsync.SnapshotView) *designBasisView {
	if v == nil {
		return nil
	}
	out := &designBasisView{
		Source: v.Source, Frozen: v.Frozen, FrozenAt: v.FrozenAt, Provider: v.Provider,
		CollectionID: v.CollectionID, CollectionName: v.CollectionName, SourceKnown: v.SourceKnown,
		BindingGeneration: v.BindingGeneration, DocCount: v.DocCount, ManifestDigest: v.ManifestDigest,
		ContentPurgedAt: v.ContentPurgedAt, IntegrityError: integrityText(v), Docs: make([]docMetaView, 0, len(v.Docs)),
	}
	out.SiteURL = safeSiteHref(v.BaseURL)
	if v.CollectionID != "" {
		out.CollectionURL = safeDocHref(v.BaseURL, "/collection/"+url.PathEscape(v.CollectionID))
	}
	for _, d := range v.Docs {
		out.Docs = append(out.Docs, docMetaView{
			DocID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256,
			URL: safeDocHref(v.BaseURL, d.URL), Purged: d.Purged,
		})
	}
	return out
}

// requestDocsPageView 是 AI 请求页面的设计依据。
type requestDocsPageView struct {
	RequestID      int64
	Type           string
	DeclaredSource string
	Declared       bool
	WorkItemID     int64
	WorkItemURL    string
	SnapshotSetID  int64
	// Basis 是实际可读的依据；没有时 Source=none。
	Basis *designBasisView
	// Error 非空表示声明与实际快照不一致（按原因码给出的消息）。
	Error uiText
}

func newRequestDocsPageView(v *ai.RequestDocsView) *requestDocsPageView {
	if v == nil {
		return nil
	}
	out := &requestDocsPageView{
		RequestID: v.RequestID, Type: v.Type, DeclaredSource: v.DeclaredSource, Declared: v.Declared,
		WorkItemID: v.WorkItemID, SnapshotSetID: v.SnapshotSetID, Basis: newDesignBasisView(v.View), Error: docsErrorText(v.Issue, v.Error),
	}
	if v.WorkItemID != 0 {
		out.WorkItemURL = aiDevItemURL(v.WorkItemID)
	}
	return out
}

// taskDocsPageView 是 AI 任务页面的设计依据。
type taskDocsPageView struct {
	TaskID         int64
	RequestID      int64
	Type           string
	DeclaredSource string
	BasisSource    string
	RequestURL     string
	Basis          *designBasisView
	// Error 非空表示任务的依据不可核对（有原因码时是消息，否则是原样的说明）。
	Error uiText
}

func newTaskDocsPageView(v *ai.TaskDocsView) *taskDocsPageView {
	if v == nil {
		return nil
	}
	return &taskDocsPageView{
		TaskID: v.TaskID, RequestID: v.RequestID, Type: v.Type, DeclaredSource: v.DeclaredSource,
		BasisSource: v.BasisSource, RequestURL: aiRequestURL(v.RequestID), Basis: newDesignBasisView(v.View),
		Error: docsErrorText(v.Issue, v.Error),
	}
}

// relatedTaskView 是工作项的一个关联任务及其依据关系。
type relatedTaskView struct {
	TaskID    int64
	RequestID int64
	Type      string
	Mode      string
	Relation  string
	// Note 是关系的说明；NoteDetail 是依据无法解析时的具体原因（没有时为空）。
	Note, NoteDetail uiText
	TaskURL          string
	RequestURL       string
}

func newRelatedTaskView(v ai.RelatedTask) relatedTaskView {
	out := relatedTaskView{
		TaskID: v.TaskID, RequestID: v.RequestID, Type: v.Type, Mode: v.Mode, Relation: v.Relation,
		Note: relationNoteText(v.NoteCode, v.Note), TaskURL: aiTaskURL(v.TaskID), RequestURL: aiRequestURL(v.RequestID),
	}
	if out.Note.id != "" && (v.Issue != nil || v.Detail != "") {
		out.NoteDetail = docsErrorText(v.Issue, v.Detail)
	}
	return out
}

// ---- 按需读取正文 ----

// docContent 是正文接口的响应；不含 token 或外部凭据。
type docContent struct {
	DocID    string `json:"doc_id"`
	Title    string `json:"title"`
	Revision int64  `json:"revision"`
	SHA256   string `json:"sha256"`
	Content  string `json:"content"`
}

// docContentError 是正文接口的错误响应。
type docContentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeDocContentError 返回带明确 code 与 message 的 JSON 错误；不缓存、不嗅探。
func (d *Dashboard) writeDocContentError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	d.writeJSON(w, r, status, docContentError{Code: code, Message: msg})
}

// docsContent 包装正文读取接口：未登录返回 JSON 401（而不是跳转到登录页）。
func (d *Dashboard) docsContent(h func(w http.ResponseWriter, r *http.Request, se *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se := d.getSession(r)
		if se == nil {
			d.writeDocContentError(w, r, http.StatusUnauthorized, "unauthorized", "登录已过期，请重新登录")
			return
		}
		h(w, r, se)
	}
}

// docIDFromQuery 校验并返回 doc_id：非空、长度上限、没有控制字符。
func docIDFromQuery(r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("doc_id"))
	if id == "" || len(id) > docsContentMaxID || hasControl(id) {
		return "", false
	}
	return id, true
}

// writeDocContent 把一篇正文或解析错误写成响应。错误码：
// 400 参数无效、404 没有这篇文档或没有依据、409 依据记录不一致、410 正文已按保留策略清理、500 内部错误。
func (d *Dashboard) writeDocContent(w http.ResponseWriter, r *http.Request, doc *docsync.Snapshot, err error) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case errors.Is(err, docsync.ErrSnapshotExpired):
		d.writeDocContentError(w, r, http.StatusGone, "content_purged", "这篇设计文档的正文已按保留策略清理，只保留了元数据")
	case errors.Is(err, ai.ErrDocsBasisConflict):
		d.writeDocContentError(w, r, http.StatusConflict, "basis_conflict", "设计依据的记录与实际快照不一致，无法确认这篇文档属于它")
	case errors.Is(err, ai.ErrDocsBasisMissing):
		d.writeDocContentError(w, r, http.StatusNotFound, "no_basis", "这个对象没有可读的设计文档依据")
	case errors.Is(err, ai.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		d.writeDocContentError(w, r, http.StatusNotFound, "not_found", "没有这个对象")
	case err != nil:
		d.log.Error("读取设计文档正文", "err", err)
		d.writeDocContentError(w, r, http.StatusInternalServerError, "internal", "读取正文时出现内部错误")
	case doc == nil:
		d.writeDocContentError(w, r, http.StatusNotFound, "doc_not_found", "依据中没有这篇文档")
	default:
		d.writeJSON(w, r, http.StatusOK, docContent{
			DocID: doc.DocID, Title: doc.Title, Revision: doc.Revision, SHA256: doc.SHA256, Content: doc.Content,
		})
	}
}

// getWorkItemDocContent 按需读取工作项冻结快照中一篇文档的正文。
func (d *Dashboard) getWorkItemDocContent(w http.ResponseWriter, r *http.Request, _ *session) {
	id := pathID(r)
	docID, ok := docIDFromQuery(r)
	if id <= 0 || !ok {
		d.writeDocContentError(w, r, http.StatusBadRequest, "invalid_request", "工作项编号或 doc_id 无效")
		return
	}
	if d.ai == nil {
		d.writeDocContentError(w, r, http.StatusNotFound, "not_found", "没有这个对象")
		return
	}
	doc, err := d.ai.WorkItemDocContent(r.Context(), id, docID)
	d.writeDocContent(w, r, doc, err)
}

// getRequestDocContent 按需读取 AI 请求固定依据中一篇文档的正文。
func (d *Dashboard) getRequestDocContent(w http.ResponseWriter, r *http.Request, _ *session) {
	id := pathID(r)
	docID, ok := docIDFromQuery(r)
	if id <= 0 || !ok {
		d.writeDocContentError(w, r, http.StatusBadRequest, "invalid_request", "请求编号或 doc_id 无效")
		return
	}
	if d.ai == nil {
		d.writeDocContentError(w, r, http.StatusNotFound, "not_found", "没有这个对象")
		return
	}
	doc, err := d.ai.RequestDocContent(r.Context(), id, docID)
	d.writeDocContent(w, r, doc, err)
}

// getTaskDocContent 按需读取 AI 任务依据中一篇文档的正文。
func (d *Dashboard) getTaskDocContent(w http.ResponseWriter, r *http.Request, _ *session) {
	id := pathID(r)
	docID, ok := docIDFromQuery(r)
	if id <= 0 || !ok {
		d.writeDocContentError(w, r, http.StatusBadRequest, "invalid_request", "任务编号或 doc_id 无效")
		return
	}
	if d.ai == nil {
		d.writeDocContentError(w, r, http.StatusNotFound, "not_found", "没有这个对象")
		return
	}
	doc, err := d.ai.TaskDocContent(r.Context(), id, docID)
	d.writeDocContent(w, r, doc, err)
}

// ---- 页面数据 ----

// workItemDesignBasis 返回工作项页面的设计依据（没有配置 AI 核心时为空）。
func (d *Dashboard) workItemDesignBasis(ctx context.Context, id int64) (*designBasisView, error) {
	if d.ai == nil {
		return nil, nil
	}
	v, err := d.ai.WorkItemDocsView(ctx, id)
	if err != nil {
		return nil, err
	}
	return newDesignBasisView(v), nil
}

// relatedTasks 返回工作项页面的关联任务与依据关系（没有配置 AI 核心时为空）。
func (d *Dashboard) relatedTasks(ctx context.Context, id int64) ([]relatedTaskView, error) {
	if d.ai == nil {
		return nil, nil
	}
	list, err := d.ai.RelatedTasks(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]relatedTaskView, 0, len(list))
	for _, v := range list {
		out = append(out, newRelatedTaskView(v))
	}
	return out, nil
}

// requestDesignBasis 返回 AI 请求页面的设计依据。
func (d *Dashboard) requestDesignBasis(ctx context.Context, id int64) (*requestDocsPageView, error) {
	if d.ai == nil {
		return nil, nil
	}
	v, err := d.ai.RequestDocsView(ctx, id)
	if err != nil {
		return nil, err
	}
	return newRequestDocsPageView(v), nil
}

// taskDesignBasis 返回 AI 任务页面的设计依据。
func (d *Dashboard) taskDesignBasis(ctx context.Context, id int64) (*taskDocsPageView, error) {
	if d.ai == nil {
		return nil, nil
	}
	v, err := d.ai.TaskDocsView(ctx, id)
	if err != nil {
		return nil, err
	}
	return newTaskDocsPageView(v), nil
}

// ---- 设置 · 仓库 · 外部文档 ----

// docBindingPageView 是只读文档绑定的展示数据（不含 token）。
type docBindingPageView struct {
	Provider       string
	BaseURL        string
	SiteURL        string
	CollectionID   string
	CollectionName string
	// CollectionURL 是验证时观测到的文档集链接（校验后）；没有观测到时为空。
	CollectionURL string
	CreatedBy     string
	// UpdatedBy 为空表示历史行没有记录（显示"未记录"）。
	UpdatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Generation int64
	// CheckStatus 是空（尚未验证）/ ok / empty / failed。
	CheckStatus            string
	CheckStartedAt         *time.Time
	CheckedAt              *time.Time
	CheckedBy              string
	CheckErrorCode         string
	CheckErrorText         string
	CheckDocCount          *int
	CheckSampleDocID       string
	ObservedCollectionName string
}

func newDocBindingPageView(b *store.DocBinding) *docBindingPageView {
	if b == nil {
		return nil
	}
	v := &docBindingPageView{
		Provider: b.Provider, BaseURL: b.BaseURL, SiteURL: safeSiteHref(b.BaseURL),
		CollectionID: b.CollectionID, CollectionName: b.CollectionName,
		CollectionURL: safeDocHref(b.BaseURL, b.CollectionURL),
		CreatedBy:     b.CreatedBy, UpdatedBy: deref(b.UpdatedBy), CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		Generation: b.Generation, CheckStartedAt: b.CheckStartedAt, CheckedAt: b.CheckedAt, CheckedBy: deref(b.CheckedBy),
		CheckErrorCode: b.CheckErrorCode, CheckDocCount: b.CheckDocCount, CheckSampleDocID: b.CheckSampleDocID,
		ObservedCollectionName: b.ObservedCollectionName,
	}
	// 还没验证过时没有观测到的链接：按配置的站点和文档集 ID 生成（配置由管理员写入，仍然经过同样的校验）。
	if v.CollectionURL == "" && b.CollectionID != "" {
		v.CollectionURL = safeDocHref(b.BaseURL, "/collection/"+url.PathEscape(b.CollectionID))
	}
	if b.CheckStatus != nil {
		v.CheckStatus = *b.CheckStatus
	}
	if b.CheckErrorCode != "" {
		v.CheckErrorText = docsync.DocCheckErrorText(b.CheckErrorCode)
	}
	return v
}

// decisionDocPageView 是「待决策清单」配置的展示数据（不含写 token）。
type decisionDocPageView struct {
	Configured    bool
	Provider      string
	BaseURL       string
	SiteURL       string
	DocID         string
	CollectionID  string
	DocURL        string
	ObservedUnits *int64
	WarningCode   string
	CreatedBy     string
	UpdatedBy     string
	UpdatedAt     time.Time
}

func newDecisionDocPageView(d *store.DecisionDoc) *decisionDocPageView {
	if d == nil {
		return &decisionDocPageView{}
	}
	return &decisionDocPageView{
		Configured: true, Provider: d.Provider, BaseURL: d.BaseURL, SiteURL: safeSiteHref(d.BaseURL),
		DocID: d.DocID, CollectionID: d.CollectionID, DocURL: safeDocHref(d.BaseURL, "/doc/"+url.PathEscape(d.DocID)),
		ObservedUnits: d.ObservedUnits, WarningCode: d.WarningCode, CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy,
		UpdatedAt: d.UpdatedAt,
	}
}

// repoDocsPage 是"设置 · 仓库 · 外部文档"页面的数据。
type repoDocsPage struct {
	settingsBase
	Repo     string
	Active   bool
	Binding  *docBindingPageView
	Decision *decisionDocPageView
}

func (d *Dashboard) pageRepoDocs(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	fullName := strings.TrimSpace(r.URL.Query().Get("repo"))
	if fullName == "" {
		d.notFound(w, r, "请指定要查看的仓库（从“设置 · 仓库”的“外部文档”进入）。")
		return
	}
	repo, err := store.FindRepoAny(ctx, d.db, fullName)
	if errors.Is(err, store.ErrRepoNotFound) || (err != nil && !admin.ValidRepoFullName(fullName)) {
		d.notFound(w, r, fmt.Sprintf("Kiln 中没有仓库 %s：名称可能写错了，或者仓库还没有同步。", fullName))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	binding, err := store.GetDocBinding(ctx, d.db, repo.ID)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	decision, err := store.GetDecisionDoc(ctx, d.db, repo.ID)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &repoDocsPage{
		Section: sectionRepos, Repo: repo.FullName(), Active: repo.Active,
		Binding: newDocBindingPageView(binding), Decision: newDecisionDocPageView(decision),
	}
	d.render(w, r, "settings_repo_docs", fmt.Sprintf("设置 · %s · 外部文档", p.Repo), p)
}

// postRepoDocsVerify 显式触发一次只读连接验证，完成后回到仓库的外部文档页。结果说明绝不含 token。
func (d *Dashboard) postRepoDocsVerify(w http.ResponseWriter, r *http.Request, se *session) {
	repo := strings.TrimSpace(r.PostFormValue("repo"))
	back := "/settings/repos/docs?repo=" + url.QueryEscape(repo)
	if repo == "" {
		d.flashBack(w, r, "/settings/repos", "没有验证：请选择仓库")
		return
	}
	res, err := d.admin.VerifyDocBinding(r.Context(), repo, se.Login)
	if errors.Is(err, admin.ErrDocBindingChangedDuringVerify) {
		d.flashBack(w, r, back, "验证期间文档绑定已变更，本次检查结果没有应用；请对新配置重新验证")
		return
	}
	if err != nil {
		d.flashBack(w, r, back, fmt.Sprintf("验证 %s 的文档读取失败：%s", repo, d.failure(r, err)))
		return
	}
	d.flashBack(w, r, back, docVerifyMessage(res))
}

// docVerifyMessage 返回验证结果的一句话说明（不含 token 与远端原始信息）。
func docVerifyMessage(res *admin.DocVerifyResult) string {
	count := 0
	if res.DocCount != nil {
		count = *res.DocCount
	}
	switch res.Status {
	case docsync.DocCheckStatusOK:
		return fmt.Sprintf("验证通过：文档集可访问，共 %d 篇文档，抽查了 %s 的正文", count, res.SampleDocID)
	case docsync.DocCheckStatusEmpty:
		return "验证通过：文档集可访问，但当前没有文档（冻结时会记录一个空集合）"
	default:
		return "验证失败：" + docsync.DocCheckErrorText(res.ErrorCode)
	}
}

// loadRepoDocStatuses 为仓库列表的一页仓库填上外部文档的简要状态（是否绑定、验证状态、是否配了待决策清单）。
// 停用的仓库也查询：外部文档入口对它们仍然可用，只是不能配置。
func (d *Dashboard) loadRepoDocStatuses(ctx context.Context, repos []repoView) error {
	if len(repos) == 0 {
		return nil
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.FullName)
	}
	rows, err := d.db.Query(ctx, `SELECT r.owner||'/'||r.name,
			(b.repo_id IS NOT NULL), coalesce(b.check_status, ''), (dd.repo_id IS NOT NULL)
		FROM repos r
		LEFT JOIN repo_doc_bindings b ON b.repo_id=r.id
		LEFT JOIN repo_decision_docs dd ON dd.repo_id=r.id
		WHERE r.owner||'/'||r.name = ANY($1)`, names)
	if err != nil {
		return err
	}
	type status struct {
		bound, decision bool
		check           string
	}
	byName := make(map[string]status, len(repos))
	if err := func() error {
		defer rows.Close()
		for rows.Next() {
			var name, check string
			var s status
			if err := rows.Scan(&name, &s.bound, &check, &s.decision); err != nil {
				return err
			}
			s.check = check
			byName[name] = s
		}
		return rows.Err()
	}(); err != nil {
		return err
	}
	for i := range repos {
		s := byName[repos[i].FullName]
		repos[i].DocsURL = repoDocsURL(repos[i].FullName)
		repos[i].DocBound = s.bound
		repos[i].DocCheckStatus = s.check
		repos[i].DecisionDoc = s.decision
	}
	return nil
}

// repoDocsURL 返回仓库的外部文档页地址。
func repoDocsURL(fullName string) string {
	return "/settings/repos/docs?repo=" + url.QueryEscape(fullName)
}

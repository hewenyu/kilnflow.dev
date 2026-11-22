package docprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
)

// Outline 的固定参数。
const (
	// outlinePageLimit 是 lists 接口每页请求的条数。
	outlinePageLimit = 100
	// outlineMaxPages 是分页的硬上限，避免服务端一直返回满页导致死循环。
	outlineMaxPages = 1000
	// outlineDefaultTimeout 是未提供 http.Client 时读请求的默认超时。
	outlineDefaultTimeout = 30 * time.Second
	// outlineWriteTimeout 是未提供 http.Client 时写请求的默认超时（写请求可能触发较长的服务端处理）。
	outlineWriteTimeout = 60 * time.Second
	// outlineMaxResponseBytes 是响应体的上限：错误和成功响应都不应超过它，避免被异常的响应拖垮内存。
	outlineMaxResponseBytes = 8 << 20
)

// Config 是构造 Outline 客户端所需的配置。
type Config struct {
	BaseURL string       // Outline 站点地址，例如 https://outline.example.com（结尾的斜杠会被去掉）
	Token   string       // Outline API token
	HTTP    *http.Client // 可选；为 nil 时使用默认超时的客户端
}

// Outline 是 Outline（自托管 wiki）的客户端：实现只读的 Provider 和受限的 DocumentWriter。
type Outline struct {
	baseURL string
	token   string
	http    *http.Client
	// writeHTTP 用于写请求：与读请求分开，禁用了自动跟随重定向（3xx 时 POST 被浏览器 / Go 客户端重放会重复写入）。
	writeHTTP *http.Client
}

// Error 是 Outline 返回的错误：HTTP 状态码和白名单里的错误码。
//
// 远端的 message 可能回显请求头（Authorization）或正文，一律不保留在错误里：错误文本只包含状态码、白名单错误码和本地
// 固定说明。调用方可以把它写进日志和 last_error 而不泄露凭据。
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("Outline API 错误：HTTP %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if m := safeStatusMessage(e.Status); m != "" {
		s += "：" + m
	}
	return s
}

// NewOutline 构造 Outline 客户端。BaseURL、Token 去掉首尾空白后不能为空，BaseURL 还必须通过 ValidateBaseURL。
func NewOutline(cfg Config) (*Outline, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	token := strings.TrimSpace(cfg.Token)
	if baseURL == "" {
		return nil, errors.New("Outline BaseURL 不能为空")
	}
	if err := ValidateBaseURL(baseURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("Outline Token 不能为空")
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: outlineDefaultTimeout}
	}
	return &Outline{baseURL: baseURL, token: token, http: noRedirect(hc), writeHTTP: writeClient(hc)}, nil
}

// ValidateBaseURL 校验文档服务（如 Outline）的 BaseURL：
//   - 必须是 http 或 https，且有主机名（https://:443 这种只有端口的不算）；
//   - 不能包含用户名、密码、查询参数、片段或控制字符（地址会写进日志和页面）；
//   - 非本机地址必须使用 https；本机地址（loopback、localhost）允许 http，方便本地开发和测试。
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("文档系统地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error 会回显整个地址；地址本身不是凭据，但错误文本也不该透传到页面。
		return errors.New("文档系统地址无效")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("文档系统地址必须是 http 或 https")
	}
	if u.Hostname() == "" {
		return errors.New("文档系统地址缺少主机名")
	}
	if u.User != nil {
		return errors.New("文档系统地址不能包含用户名或密码")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.HasSuffix(raw, "#") {
		return errors.New("文档系统地址不能包含查询参数或片段")
	}
	if strings.ContainsFunc(raw, unicode.IsControl) {
		return errors.New("文档系统地址不能包含控制字符")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return errors.New("文档系统地址必须使用 https（本机地址除外）")
	}
	return nil
}

// isLoopbackHost 判断主机名是否指向本机（规则见 endpoint.IsLoopbackHost）。
func isLoopbackHost(host string) bool { return endpoint.IsLoopbackHost(host) }

// noRedirect 返回沿用调用方 Transport 等设置、但禁用自动跟随重定向的客户端副本。
//
// 读写请求都不跟随 3xx：跟随重定向时 Go 会把 Location 拼进错误（远端可以把 token 反射进 Location，
// 例如 302 到 http://127.0.0.1:<token>），而写请求（documents.update）没有幂等键，重放 307/308 的 POST
// 会导致重复写入。3xx 一律作为响应交给调用方按状态处理。
func noRedirect(hc *http.Client) *http.Client {
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// writeClient 返回用于写请求的客户端：在 noRedirect 的基础上保证足够的超时（写请求可能触发较长的服务端处理）。
func writeClient(hc *http.Client) *http.Client {
	c := *noRedirect(hc)
	c.Timeout = max(hc.Timeout, outlineWriteTimeout)
	return &c
}

// Effect 是一次写请求是否可能已经生效。没有幂等键的写操作只能保守判断。
type Effect int

const (
	// EffectNotApplied 表示写请求确定没有生效（本地校验失败、提供方明确拒绝、CAS 冲突、可信的限流）。
	EffectNotApplied Effect = iota
	// EffectMayHaveApplied 表示写请求可能已经生效（5xx、超时、连接中断、响应损坏、重定向、未知错误）。
	// 调用方绝不能重发，只能只读查证。
	EffectMayHaveApplied
)

// String 返回可读的效果说明（用于日志和错误文本，不含 token）。
func (e Effect) String() string {
	if e == EffectMayHaveApplied {
		return "may_have_applied"
	}
	return "not_applied"
}

// safeOutlineCodes 是允许出现在错误文本和 last_error 里的 Outline 错误码。远端的 error 字段本身不可信
// （可能被代理或回显拼接），只保留已知的固定枚举；其余一律丢弃。
var safeOutlineCodes = map[string]bool{
	"document_conflict":       true,
	"rate_limit_exceeded":     true,
	"authentication_required": true,
	"authorization_error":     true,
	"not_found":               true,
	"validation_error":        true,
	"invalid_request":         true,
	"forbidden":               true,
}

// safeCode 只保留白名单里的错误码。
func safeCode(code string) string {
	if safeOutlineCodes[code] {
		return code
	}
	return ""
}

// safeStatusMessage 返回按 HTTP 状态生成的本地说明，绝不使用远端 message。
func safeStatusMessage(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "请求被提供方拒绝"
	case http.StatusUnauthorized:
		return "提供方要求重新认证"
	case http.StatusForbidden:
		return "提供方拒绝访问"
	case http.StatusNotFound:
		return "文档不存在"
	case http.StatusConflict:
		return "文档已被他人修改"
	case http.StatusTooManyRequests:
		return "提供方限流"
	}
	if status >= 500 {
		return "提供方内部错误，结果不明"
	}
	if status >= 300 && status < 400 {
		return "提供方返回重定向，未跟随"
	}
	if status >= 200 && status < 300 {
		return "提供方返回错误"
	}
	return ""
}

// errOutlineTransport 是传输层的固定错误文案：底层错误（*url.Error 等）可能带有完整的 URL、重定向的 Location
// 或响应片段（远端可以把 token 反射进 302 的 Location），一律不向外传播。
var errOutlineTransport = errors.New("与提供方的连接失败")

// errOutlineDecode 表示提供方的响应无法解析（不携带响应体内容）。
var errOutlineDecode = errors.New("提供方的响应无法解析")

// errOutlineTooLarge 表示提供方的响应超过 outlineMaxResponseBytes（不携带响应体内容）。
var errOutlineTooLarge = errors.New("提供方的响应超过大小上限")

// IsDecodeError 判断错误是否来自提供方响应的解析失败：HTTP 成功但 JSON 损坏，或 data 字段不是预期的类型。
// 错误文本只有本地固定文案，不包含响应体或 token；调用方可以据此把它归为"响应无效"，而不是连接失败。
func IsDecodeError(err error) bool { return errors.Is(err, errOutlineDecode) }

// transportError 把传输层错误换成固定文案，保留取消与超时的判别语义（errors.Is 仍然成立）。
func transportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("请求已取消：%w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("请求超时：%w", context.DeadlineExceeded)
	}
	return errOutlineTransport
}

// transportMessage 是 transportError 对应的简短文案（用于 WriteError.Message）。
func transportMessage(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "请求已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时"
	}
	return errOutlineTransport.Error()
}

// WriteError 是 UpdateDocument 的错误：区分是否可能已生效、是否可重试、HTTP 状态与白名单里的错误码。
// Message 只由本地生成；远端 message 可能回显请求头或正文，绝不透传。
type WriteError struct {
	Effect    Effect
	Retryable bool
	Status    int
	Code      string
	Message   string
	// RetryAfter 是提供方建议的等待时间（来自 Retry-After 头，可信限流时才有）。
	RetryAfter time.Duration
	// cause 只保存标准 context 哨兵错误（context.Canceled / context.DeadlineExceeded），让调用方能识别本地取消。
	// 绝不能放底层的 *url.Error：它带有完整 URL、重定向 Location，远端可以把 token 反射进去。
	cause error
}

// Unwrap 暴露 cause，使 errors.Is(err, context.Canceled / context.DeadlineExceeded) 成立。
// cause 永远只是标准哨兵错误（见 contextCause），不会泄露 URL、Location 或 token。
func (e *WriteError) Unwrap() error { return e.cause }

// contextCause 从底层错误里提取标准 context 哨兵错误；其他错误返回 nil。WriteError.cause 只从这里取值，
// 保证它永远不含底层传输错误的细节。
func contextCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return nil
}

func (e *WriteError) Error() string {
	s := fmt.Sprintf("Outline 写文档失败（%s）：", e.Effect)
	if e.Status != 0 {
		s += fmt.Sprintf("HTTP %d ", e.Status)
	}
	if e.Code != "" {
		s += e.Code
	}
	if e.Message != "" {
		s += "：" + e.Message
	}
	return strings.TrimRight(s, " ")
}

// IsDocumentConflict 判断错误是否是 Outline 的 CAS 冲突（HTTP 409 且 error=document_conflict）。只有这一种 409 是
// "确定没有写入、可以换用新修订号重试"；未知的 409 保守归为结果不明。
func IsDocumentConflict(err error) bool {
	we, ok := errors.AsType[*WriteError](err)
	return ok && we.Status == http.StatusConflict && we.Code == documentConflictCode
}

// IsRateLimited 判断错误是否是可信的 Outline 限流（HTTP 429 且 error=rate_limit_exceeded）。
func IsRateLimited(err error) bool {
	we, ok := errors.AsType[*WriteError](err)
	return ok && we.Status == http.StatusTooManyRequests && we.Code == rateLimitCode
}

// IsAuthRejected 判断错误是否是提供方的鉴权拒绝（HTTP 401 / 403）：token 被撤销或无权访问。调用方可以据此判断
// 是否值得换一份凭据重试（同目标轮换 token 时旧凭据会被撤销）。
//
// 写请求只有在确定没有生效时才算：响应损坏的 401 / 403 归为 EffectMayHaveApplied，换凭据重试可能重复写入。
func IsAuthRejected(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	}
	if we, ok := errors.AsType[*WriteError](err); ok {
		return we.Effect == EffectNotApplied &&
			(we.Status == http.StatusUnauthorized || we.Status == http.StatusForbidden)
	}
	return false
}

// PermanentReadError 判断读取错误是否是提供方的永久拒绝（鉴权失败、文档不存在、请求无效）：重试不会成功。
// 网络错误、超时、5xx、限流、响应解码失败都返回 false（临时失败，可以退避后重试）。
func PermanentReadError(err error) bool {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		return false
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

const (
	// documentConflictCode 是 Outline 的 CAS 冲突错误码。
	documentConflictCode = "document_conflict"
	// rateLimitCode 是 Outline 的限流错误码。
	rateLimitCode = "rate_limit_exceeded"
)

// envelope 是 Outline 的响应外壳：成功时 ok=true 且 data 有值，失败时 ok=false 且带 error / message。
type envelope struct {
	OK      bool            `json:"ok"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// collections.list / documents.list 返回的条目。
type collectionItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type documentItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Revision     int64   `json:"revision"`
	URL          string  `json:"url"`
	CollectionID string  `json:"collectionId"`
	PublishedAt  *string `json:"publishedAt"`
	ArchivedAt   *string `json:"archivedAt"`
	DeletedAt    *string `json:"deletedAt"`
}

// documents.info 返回的文档对象（含正文 text）。
type documentInfo struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Text         string  `json:"text"`
	Revision     int64   `json:"revision"`
	URL          string  `json:"url"`
	CollectionID string  `json:"collectionId"`
	PublishedAt  *string `json:"publishedAt"`
	ArchivedAt   *string `json:"archivedAt"`
	DeletedAt    *string `json:"deletedAt"`
}

// published 判断 Outline 的可空时间字段是否有值：null 或缺字段为 false（不把零值当成已发布）。
func published(t *string) bool { return t != nil && strings.TrimSpace(*t) != "" }

// post 向 POST {baseURL}/api/{method} 发起一次调用，返回响应中的 data。
func (o *Outline) post(ctx context.Context, method string, body any) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		// 编码的是本地的请求结构，错误文本只涉及 Go 类型，不含远端内容。
		return nil, fmt.Errorf("编码 Outline 请求 %s 失败", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/"+method, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造 Outline 请求 %s 失败", method)
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Outline %s 失败：%w", method, transportError(err))
	}
	defer resp.Body.Close()

	// 先按上限读取再解析：超大响应在分配内存前就失败，且绝不把响应体（可能回显请求头）放进错误。
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, outlineMaxResponseBytes+1))
	if readErr != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &Error{Status: resp.StatusCode}
		}
		// 读取错误可能来自调用方的取消 / 超时，保留 context 错误链，不吞成普通的传输错误。
		return nil, fmt.Errorf("读取 Outline %s 响应失败：%w", method, transportError(readErr))
	}
	if len(respBody) > outlineMaxResponseBytes {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &Error{Status: resp.StatusCode}
		}
		return nil, fmt.Errorf("读取 Outline %s 响应失败：%w", method, errOutlineTooLarge)
	}

	var env envelope
	decodeErr := json.Unmarshal(respBody, &env)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{Status: resp.StatusCode, Code: safeCode(env.Error)}
	}
	if decodeErr != nil {
		// 解析错误可能夹带响应体片段，不能透传。
		return nil, fmt.Errorf("解析 Outline %s 响应失败：%w", method, errOutlineDecode)
	}
	// Outline 的成功响应不一定带 ok（collections.list / documents.list 就没有），只有失败响应带 error；
	// 以 error 是否为空判断（HTTP 状态已在上面处理）。
	if env.Error != "" {
		return nil, &Error{Status: resp.StatusCode, Code: safeCode(env.Error)}
	}
	return env.Data, nil
}

// ListCollections 列出所有文档集，按 offset 分页直到某页不足一页为止。
func (o *Outline) ListCollections(ctx context.Context) ([]Collection, error) {
	out := make([]Collection, 0)
	offset := 0
	for range outlineMaxPages {
		data, err := o.post(ctx, "collections.list", map[string]any{
			"limit":  outlinePageLimit,
			"offset": offset,
		})
		if err != nil {
			return nil, err
		}
		var items []collectionItem
		if len(data) > 0 {
			if err := json.Unmarshal(data, &items); err != nil {
				return nil, fmt.Errorf("解析 Outline collections.list 数据失败：%w", errOutlineDecode)
			}
		}
		for _, item := range items {
			out = append(out, Collection(item))
		}
		offset += len(items)
		if len(items) < outlinePageLimit {
			return out, nil
		}
	}
	return nil, fmt.Errorf("Outline collections.list 分页超过 %d 页上限", outlineMaxPages)
}

// ListDocuments 列出一个文档集里的文档（只填列表字段，不拉取正文）。
func (o *Outline) ListDocuments(ctx context.Context, collectionID string) ([]Document, error) {
	out := make([]Document, 0)
	offset := 0
	for range outlineMaxPages {
		data, err := o.post(ctx, "documents.list", map[string]any{
			"collectionId": collectionID,
			"limit":        outlinePageLimit,
			"offset":       offset,
		})
		if err != nil {
			return nil, err
		}
		var items []documentItem
		if len(data) > 0 {
			if err := json.Unmarshal(data, &items); err != nil {
				return nil, fmt.Errorf("解析 Outline documents.list 数据失败：%w", errOutlineDecode)
			}
		}
		for _, item := range items {
			out = append(out, Document{
				ID:           item.ID,
				Title:        item.Title,
				Revision:     item.Revision,
				URL:          item.URL,
				CollectionID: item.CollectionID,
				Published:    published(item.PublishedAt),
				Archived:     published(item.ArchivedAt),
				Deleted:      published(item.DeletedAt),
			})
		}
		offset += len(items)
		if len(items) < outlinePageLimit {
			return out, nil
		}
	}
	return nil, fmt.Errorf("Outline documents.list 分页超过 %d 页上限", outlineMaxPages)
}

// GetDocument 读取一篇文档的当前内容（含 Content、Revision、SHA256）。
func (o *Outline) GetDocument(ctx context.Context, docID string) (Document, error) {
	data, err := o.post(ctx, "documents.info", map[string]any{"id": docID})
	if err != nil {
		return Document{}, err
	}
	var info documentInfo
	if len(data) > 0 {
		if err := json.Unmarshal(data, &info); err != nil {
			return Document{}, fmt.Errorf("解析 Outline documents.info 数据失败：%w", errOutlineDecode)
		}
	}
	doc := Document{
		ID:           info.ID,
		Title:        info.Title,
		Revision:     info.Revision,
		URL:          info.URL,
		Content:      info.Text,
		CollectionID: info.CollectionID,
		Published:    published(info.PublishedAt),
		Archived:     published(info.ArchivedAt),
		Deleted:      published(info.DeletedAt),
	}
	doc.SHA256 = Sum(doc.Content)
	return doc, nil
}

// UpdateDocument 修改一篇文档的正文（Outline documents.update）。请求体只含 id、text、lastRevision：
// 不传 title、publish、collectionId，避免改标题、发布状态或移动文档。提供方按 lastRevision 做 CAS。
//
// 返回的错误一律是 *WriteError，调用方据此判断"确定没写"还是"可能已写"；后一种绝不能重发。
func (o *Outline) UpdateDocument(ctx context.Context, req UpdateDocumentRequest) (Document, error) {
	if strings.TrimSpace(req.ID) == "" {
		return Document{}, &WriteError{Effect: EffectNotApplied, Message: "文档 id 为空"}
	}
	if req.LastRevision <= 0 {
		return Document{}, &WriteError{Effect: EffectNotApplied, Message: "缺少有效的 lastRevision"}
	}
	payload, err := json.Marshal(map[string]any{
		"id":           req.ID,
		"text":         req.Text,
		"lastRevision": req.LastRevision,
	})
	if err != nil {
		return Document{}, &WriteError{Effect: EffectNotApplied, Message: "编码请求失败"}
	}
	// 请求发出之前就取消：确定没有写。Retryable 让调用方知道可以安全地再试一次（没有生效），
	// cause 保留取消 / 超时判别，供上层退回 pending 而不是永久取消。
	if err := ctx.Err(); err != nil {
		return Document{}, &WriteError{
			Effect: EffectNotApplied, Retryable: true, Message: "请求在发送前已取消", cause: contextCause(err),
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/documents.update", bytes.NewReader(payload))
	if err != nil {
		return Document{}, &WriteError{Effect: EffectNotApplied, Message: "构造请求失败"}
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := o.writeHTTP.Do(httpReq)
	if err != nil {
		// 请求可能已经发出（连接中断、超时、读完请求后取消），保守归为结果不明。
		// 原始错误可能带有 URL / 重定向 Location（远端可以反射 token），只保留本地固定文案；
		// cause 只保留标准 context 哨兵，让调用方能区分"传输中取消"与普通网络错误。
		return Document{}, &WriteError{
			Effect: EffectMayHaveApplied, Message: "请求失败：" + transportMessage(err), cause: contextCause(err),
		}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, outlineMaxResponseBytes+1))
	if readErr != nil || len(body) > outlineMaxResponseBytes {
		return Document{}, &WriteError{Effect: EffectMayHaveApplied, Status: resp.StatusCode, Message: "读取响应失败"}
	}
	var env envelope
	decodeErr := json.Unmarshal(body, &env)
	status := resp.StatusCode

	switch {
	case status >= 200 && status < 300:
		// 2xx 但响应损坏 / 字段不完整：写可能已经生效，不能当作没写。
		if decodeErr != nil {
			return Document{}, &WriteError{Effect: EffectMayHaveApplied, Status: status, Message: "响应不是有效的 JSON"}
		}
		var info documentInfo
		if err := json.Unmarshal(env.Data, &info); err != nil || info.ID == "" || info.Revision <= 0 ||
			!strings.EqualFold(info.ID, req.ID) {
			// 返回的不是我们写的那篇文档：写可能已经生效，绝不能当作成功。
			return Document{}, &WriteError{Effect: EffectMayHaveApplied, Status: status, Message: "响应缺少有效的文档 id 或 revision"}
		}
		doc := Document{
			ID:           info.ID,
			Title:        info.Title,
			Revision:     info.Revision,
			URL:          info.URL,
			Content:      info.Text,
			CollectionID: info.CollectionID,
			Published:    published(info.PublishedAt),
			Archived:     published(info.ArchivedAt),
			Deleted:      published(info.DeletedAt),
		}
		doc.SHA256 = Sum(doc.Content)
		return doc, nil
	case status >= 300 && status < 400:
		// 重定向：写请求不跟随，结果不明。
		return Document{}, &WriteError{Effect: EffectMayHaveApplied, Status: status, Message: "提供方返回重定向，未跟随"}
	case status == http.StatusConflict && decodeErr == nil && env.Error == documentConflictCode:
		// CAS 冲突：确定没有写入，可以换用新修订号重试。
		return Document{}, &WriteError{
			Effect: EffectNotApplied, Retryable: true, Status: status, Code: documentConflictCode,
			Message: safeStatusMessage(status),
		}
	case status == http.StatusTooManyRequests && decodeErr == nil && env.Error == rateLimitCode:
		return Document{}, &WriteError{
			Effect: EffectNotApplied, Retryable: true, Status: status, Code: rateLimitCode,
			Message: safeStatusMessage(status), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	case status == http.StatusBadRequest, status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusNotFound, status == http.StatusUnprocessableEntity:
		// 只有确实解析出 Outline 的错误说明时才认为是确定拒绝；响应损坏时保守归为结果不明。
		if decodeErr != nil || (env.Error == "" && env.Message == "") {
			return Document{}, &WriteError{Effect: EffectMayHaveApplied, Status: status, Message: "响应不是有效的 Outline 错误"}
		}
		return Document{}, &WriteError{
			Effect: EffectNotApplied, Status: status, Code: safeCode(env.Error),
			Message: safeStatusMessage(status),
		}
	default:
		// 5xx、未知的 409 / 429、其他状态：结果不明。
		return Document{}, &WriteError{
			Effect: EffectMayHaveApplied, Status: status, Code: safeCode(env.Error),
			Message: safeStatusMessage(status),
		}
	}
}

// parseRetryAfter 解析 Retry-After 头（秒数或 HTTP 日期）。无法解析时返回 0。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		// 防止异常大的秒数在换算成 Duration 时溢出（上限是 Duration 能表示的最大值）。
		return time.Duration(min(secs, math.MaxInt64/int64(time.Second))) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

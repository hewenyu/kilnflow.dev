// Package upload 是 HTTP 上传协议 v1 的客户端（release.uploads，PLAN #13.9，协议见 docs/UPLOAD-PROTOCOL.md）：
// 把 release 的文件发布到第三方实现的 HTTPS 服务（例如官网的下载页）。
//
// 三个端点：GET {url}/{tag} 查询、PUT {url}/{tag}/assets/{name} 暂存、POST {url}/{tag}/publish 提交。
// 只允许 https（本机地址可以用 http，供本地测试）；不跟随重定向，token 只发给配置的地址；
// 网络错误、429、5xx 按退避重试，其他 4xx 不重试；错误信息中不含 token。
package upload

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
	"github.com/hewenyu/kilnflow.dev/internal/httpretry"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// Release 的状态。
const (
	StateStaging   = "staging"   // 有暂存的文件，还没有发布，对外不可见
	StatePublished = "published" // 已发布，不可变
)

// 服务端在错误响应中返回的错误码（{"error": "<code>", "message": "..."}）。
const (
	CodeDigestMismatch = "digest_mismatch" // PUT：内容与 Content-Digest 或 Content-Length 不符
	CodeBadName        = "bad_name"        // PUT：服务端不接受这个 tag 或文件名
	CodePublished      = "published"       // PUT / POST：tag 已经发布，且内容（或清单）不同
	CodeTooLarge       = "too_large"       // PUT：文件超过服务端的大小限制
	CodeIncomplete     = "incomplete"      // POST：清单中有没暂存、或大小与摘要不符的文件
)

// Asset 是一个文件的描述：查询结果、提交清单和提交结果中使用。SHA256 是小写十六进制。
type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// URL 是发布后的下载地址，只出现在提交结果中（可选）。
	URL string `json:"url,omitempty"`
}

// Release 是 GET {url}/{tag} 的结果。
type Release struct {
	Tag         string  `json:"tag"`
	State       string  `json:"state"`
	Assets      []Asset `json:"assets"`
	PublishedAt string  `json:"published_at,omitempty"`
}

// PublishRequest 是 POST {url}/{tag}/publish 的请求体：要发布的完整清单。
type PublishRequest struct {
	Commit     string  `json:"commit"`
	RunID      int64   `json:"run_id"`
	Latest     bool    `json:"latest"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// PublishResult 是 POST {url}/{tag}/publish 的成功响应。
type PublishResult struct {
	Tag         string  `json:"tag"`
	PublishedAt string  `json:"published_at"`
	Assets      []Asset `json:"assets"`
}

// Error 是服务端返回的错误（状态码不是 2xx）。错误信息只包含方法、路径、状态码和服务端的错误码与说明，不含请求头。
type Error struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s：HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += "：" + e.Message
	}
	return s
}

// IsCode 判断 err 是否是服务端返回的、错误码为 code 的错误。
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// IsStatus 判断 err 是否是服务端返回的、状态码为 status 的错误。
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// CheckURL 检查上传地址：必须是 https（主机为本机地址时可以用 http），不能带查询参数、fragment 或用户信息。
func CheckURL(raw string) error { return endpoint.Check(raw, "上传地址") }

// checkAssetName 校验 asset 文件名：它是 /{tag}/assets/{name} 中的单个 path segment，
// 不能为空、不能是 . 或 ..、不能含路径分隔符或控制字符。
func checkAssetName(name string) error {
	switch {
	case name == "":
		return errors.New("文件名不能为空")
	case name == "." || name == "..":
		return fmt.Errorf("文件名 %q 无效", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("文件名 %q 不能包含路径分隔符", name)
	}
	return checkControl("文件名", name)
}

// checkTag 校验 tag：整个 tag（含分层 tag 中的 /）按一个 path segment 转义（releasePath 用 url.PathEscape），
// 所以 app/v1.0.0 会发成 app%2Fv1.0.0，仍然是单个 segment；接受哪些字符由服务端决定
// （不接受时返回 400 bad_name，见 docs/UPLOAD-PROTOCOL.md「公共约定」）。
// 空、. 和 .. 必须拒绝：url.PathEscape 不转义点号，单独成段时会改变路径结构（路径穿越）。
func checkTag(tag string) error {
	if tag == "" {
		return errors.New("tag 不能为空")
	}
	for seg := range strings.SplitSeq(tag, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("tag %q 的 %q 段会构成路径穿越", tag, seg)
		}
	}
	return checkControl("tag", tag)
}

// checkControl 检查控制字符：这些值会拼进请求路径，控制字符会破坏请求行和日志。
func checkControl(what, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s %q 含有控制字符", what, s)
		}
	}
	return nil
}

// 读取响应体的上限（重试参数与超时见 httpretry）。
const (
	// maxErrorBody 是读取错误响应体的上限。
	maxErrorBody = 4 << 10
	// maxResponse 是读取成功响应（查询结果、提交结果）的上限。
	maxResponse = 16 << 20
)

// Client 是一个上传地址的客户端。不是并发安全的：一次发布一个客户端，顺序使用。
type Client struct {
	base      string // 不以 / 结尾
	path      string // base 的路径部分，用于错误信息
	token     string
	userAgent string
	hc        *http.Client
	// attempts / backoff 是重试参数，测试中可以调小。
	attempts int
	backoff  time.Duration
}

// Option 调整 Client。
type Option func(*Client)

// WithHTTPClient 指定发送请求的 http.Client 的 Transport（超时和重定向策略仍由 Client 设置）。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil && hc.Transport != nil {
			c.hc.Transport = hc.Transport
		}
	}
}

// WithRetry 设置重试次数和初始退避（测试用）。
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(c *Client) { c.attempts, c.backoff = max(attempts, 1), backoff }
}

// NewClient 返回上传到 rawURL 的客户端。userAgent 形如 kiln/<版本>。
func NewClient(rawURL, token, userAgent string, opts ...Option) (*Client, error) {
	if err := CheckURL(rawURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("token 为空")
	}
	base := strings.TrimRight(rawURL, "/")
	u, _ := url.Parse(base) // CheckURL 已经解析过
	c := &Client{
		base: base, path: u.EscapedPath(), token: token, userAgent: userAgent,
		hc: &http.Client{
			Timeout: httpretry.RequestTimeout,
			// 不跟随重定向：token 和文件只发给配置的地址
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		attempts: httpretry.DefaultAttempts, backoff: httpretry.DefaultBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// releasePath 返回 /{tag} 起的路径：tag 和 name 都按 path segment 转义（tag 中的 / 也转义）。
func releasePath(tag string, rest ...string) string {
	var b strings.Builder
	b.WriteString("/" + url.PathEscape(tag))
	for _, s := range rest {
		b.WriteString("/" + s)
	}
	return b.String()
}

// Get 查询 tag 在服务端的状态：没有记录（404）时返回 nil, nil。
func (c *Client) Get(ctx context.Context, tag string) (*Release, error) {
	if err := checkTag(tag); err != nil {
		return nil, err
	}
	var rel Release
	status, err := c.doJSON(ctx, http.MethodGet, releasePath(tag), nil, &rel, http.StatusOK, http.StatusNotFound)
	if err != nil || status == http.StatusNotFound {
		return nil, err
	}
	return &rel, nil
}

// Put 暂存一个文件（PUT {url}/{tag}/assets/{name}）：open 每次尝试都会调用一次，返回文件内容。
// 返回 true 表示新建（201），false 表示服务端已有完全相同的内容（200）。
func (c *Client) Put(ctx context.Context, tag string, a Asset, open func() (io.ReadCloser, error)) (bool, error) {
	if err := checkTag(tag); err != nil {
		return false, err
	}
	if err := checkAssetName(a.Name); err != nil {
		return false, err
	}
	sum, err := hex.DecodeString(a.SHA256)
	if err != nil || len(sum) != 32 {
		return false, fmt.Errorf("%s 的 sha256 %q 无效", a.Name, a.SHA256)
	}
	r := request{
		method: http.MethodPut, path: releasePath(tag, "assets", url.PathEscape(a.Name)), size: a.Size, body: open,
		header: http.Header{
			"Content-Type":   {"application/octet-stream"},
			"Content-Digest": {"sha-256=:" + base64.StdEncoding.EncodeToString(sum) + ":"},
		},
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusCreated:
		return true, nil
	case http.StatusOK:
		return false, nil
	}
	return false, c.errorOf(r, resp)
}

// Publish 提交（POST {url}/{tag}/publish）：服务端核对清单中的文件都已暂存且一致后原子地发布。
// 同一清单重复提交仍然成功。
func (c *Client) Publish(ctx context.Context, tag string, req *PublishRequest) (*PublishResult, error) {
	if err := checkTag(tag); err != nil {
		return nil, err
	}
	for _, a := range req.Assets {
		if err := checkAssetName(a.Name); err != nil {
			return nil, err
		}
	}
	var res PublishResult
	if _, err := c.doJSON(ctx, http.MethodPost, releasePath(tag, "publish"), req, &res, http.StatusOK); err != nil {
		return nil, err
	}
	return &res, nil
}

// doJSON 发送请求（in 不为 nil 时作为 JSON 请求体），状态码为 ok 中第一个时把响应解析到 out。返回状态码。
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any, ok ...int) (int, error) {
	r := request{method: method, path: path, header: http.Header{"Accept": {"application/json"}}}
	if in != nil {
		body, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		r.size = int64(len(body))
		r.header.Set("Content-Type", "application/json")
		r.body = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	switch {
	case resp.StatusCode == ok[0]:
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
			return 0, fmt.Errorf("%s %s：响应无法解析：%w", method, c.path+path, err)
		}
		return resp.StatusCode, nil
	case len(ok) > 1 && resp.StatusCode == ok[1]:
		return resp.StatusCode, nil
	}
	return 0, c.errorOf(r, resp)
}

// request 描述一次请求。body 每次尝试都会重新调用，为 nil 表示没有请求体。
type request struct {
	method string
	path   string // 相对 base 的路径（已转义）
	header http.Header
	size   int64
	body   func() (io.ReadCloser, error)
}

// do 发送请求：网络错误、429、5xx 按指数退避重试（429 / 503 带 Retry-After 时按它等待，最长 1 分钟）。
// 返回的响应状态码不是 429、5xx（或者重试次数已用完）。
func (c *Client) do(ctx context.Context, r request) (*http.Response, error) {
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		resp, err := c.send(ctx, r)
		delay := wait
		switch {
		case err != nil:
			if ctx.Err() != nil || attempt >= c.attempts {
				return nil, fmt.Errorf("%s %s：%w", r.method, c.path+r.path, httpretry.Transport(err, c.redact))
			}
		case httpretry.Retryable(resp.StatusCode) && attempt < c.attempts:
			if d, ok := httpretry.RetryAfter(resp.Header.Get("Retry-After")); ok {
				delay = d
			}
			drain(resp)
		default:
			return resp, nil
		}
		if err := httpretry.Sleep(ctx, delay); err != nil {
			return nil, err
		}
		wait = min(wait*2, httpretry.MaxBackoff)
	}
}

// send 发送一次请求（不重试）。
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	var body io.ReadCloser
	if r.body != nil {
		b, err := r.body()
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, body)
	if err != nil {
		if body != nil {
			_ = body.Close() // 请求没有发出，关闭失败不影响结果
		}
		return nil, err
	}
	if body != nil {
		req.ContentLength = r.size
		if r.size == 0 {
			// 空文件不发送请求体；换成 NoBody 之前关闭 open 打开的内容，否则会泄漏（例如文件描述符）
			_ = body.Close() // 没有读过的内容，关闭失败不影响结果
			req.Body = http.NoBody
		}
	}
	maps.Copy(req.Header, r.header)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	return c.hc.Do(req)
}

// errorOf 把非预期的响应转换成 *Error（读取服务端的错误码和说明，去掉其中的 token）。
// 先去掉 token 再截断：先截断会把 token 切成两半，剩下的前半段不再与 token 匹配。
func (c *Client) errorOf(r request, resp *http.Response) error {
	e := &Error{Method: r.method, Path: c.path + r.path, Status: resp.StatusCode}
	raw := redact.ReadLimited(resp.Body, maxErrorBody, c.token)
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &body) == nil {
		e.Code, e.Message = body.Error, body.Message
	} else if s := strings.TrimSpace(raw); s != "" && !strings.ContainsAny(s, "<") {
		e.Message = shorten.Text(s, 200) // 已经去掉 token，截断是安全的
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		e.Message = "不跟随重定向（" + shorten.Text(c.redact(resp.Header.Get("Location")), 200) + "），请把 url 改成最终地址"
	}
	// JSON 中的字符串可能以转义的形式带着 token，解码后再去掉一次
	e.Code, e.Message = shorten.Text(c.redact(e.Code), 64), shorten.Text(c.redact(e.Message), 500)
	return e
}

// redact 去掉字符串中的 token。
func (c *Client) redact(s string) string { return redact.String(s, c.token) }

// drain 读完并关闭响应体，让连接可以复用（读取上限见 httpretry.Discard；关闭留在本包中，bodyclose 才能确认）。
func drain(resp *http.Response) {
	httpretry.Discard(resp.Body)
	_ = resp.Body.Close()
}

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError 是 GitHub 返回的错误。
type APIError struct {
	Status  int
	Message string
	Method  string
	URL     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s %s：%d %s", e.Method, e.URL, e.Status, e.Message)
}

// IsNotFound 判断是否是 404。
func IsNotFound(err error) bool { return isStatus(err, http.StatusNotFound) }

// IsUnprocessable 判断是否是 422（请求内容校验失败，例如 review 评论的行不在 PR 的 diff 中）。
func IsUnprocessable(err error) bool { return isStatus(err, http.StatusUnprocessableEntity) }

// NotApplied 判断一次写请求的错误是否说明它肯定没有生效：GitHub 明确拒绝（4xx，包括限流），或者请求根本没有发出
// （仍在限流中、没有交给 HTTP 客户端）。其他错误（网络错误、超时、5xx、响应无法解析）都可能已经生效，没有幂等键的写操作
// 不能据此重试（C19）。
func NotApplied(err error) bool {
	if _, ok := errors.AsType[*RateLimitError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*notSentError](err); ok {
		return true
	}
	ae, ok := errors.AsType[*APIError](err)
	return ok && ae.Status >= 400 && ae.Status < 500
}

// notSentError 是请求交给 HTTP 客户端之前的错误（编码请求体、构造请求、签名 App JWT）：请求肯定没有发出。
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// isStatus 判断错误是否是某个状态码。
func isStatus(err error, status int) bool {
	ae, ok := errors.AsType[*APIError](err)
	return ok && ae.Status == status
}

// rawBody 是不做 JSON 编码、原样发送的请求体（例如上传 asset）。
type rawBody struct {
	r           io.Reader
	size        int64
	contentType string
}

// doFunc 是发送一个请求并把 JSON 响应解码到 out 的函数（Client.App、Inst.Do）。
type doFunc func(ctx context.Context, method, path string, body, out any) error

// call 发送请求并返回解码后的响应；出错时返回 nil。
func call[T any](ctx context.Context, do doFunc, method, path string, body any) (*T, error) {
	var out T
	if err := do(ctx, method, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do 发送请求。u 可以是以 / 开头的 API 路径，也可以是完整地址。
// body 为 rawBody 时原样发送，否则编码为 JSON；out 非 nil 时把响应解码到 out。
// key 是限流键（installation ID，App JWT 为 appLimitKey）：仍在限流中时不发送请求，直接返回 *RateLimitError。
func (c *Client) do(ctx context.Context, key int64, auth, method, u string, body, out any) error {
	if err := c.checkLimit(key); err != nil {
		return err
	}
	var rdr io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case rawBody:
		// 包一层 NopCloser：Transport 会关闭实现了 io.Closer 的请求体，
		// 而调用方（例如上传文件）需要在 401 重试时 Seek 回开头。
		rdr = io.NopCloser(b.r)
		ctype = b.contentType
	default:
		js, err := json.Marshal(body)
		if err != nil {
			return &notSentError{fmt.Errorf("编码请求 %s %s：%w", method, u, err)}
		}
		rdr = bytes.NewReader(js)
		ctype = "application/json"
	}
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		// 完整地址（例如上传 asset 用的 uploads.github.com）
	case strings.HasPrefix(u, "/"):
		u = c.api() + u
	default:
		// 不是 API 路径也不是 http(s) 地址：拼接到 API 地址会得到畸形 URL（https://api.github.comhttpx://…），直接拒绝
		return &notSentError{fmt.Errorf("%s %q 不是以 / 开头的 API 路径，也不是 http(s) 地址", method, u)}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return &notSentError{fmt.Errorf("构造请求 %s %s：%w", method, u, err)}
	}
	if rb, ok := body.(rawBody); ok && rb.size >= 0 {
		req.ContentLength = rb.size
	}
	req.Header.Set("Accept", mediaType)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("读取 GitHub 响应 %s %s：%w", method, u, err)
	}
	if len(data) > maxResponseSize {
		return fmt.Errorf("GitHub 响应 %s %s 超过大小上限 %d 字节", method, u, maxResponseSize)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		ae := newAPIError(resp, data, method, u)
		if rl := c.rateLimitError(key, resp, ae); rl != nil {
			return rl
		}
		return ae
	}
	c.recordQuota(key, resp.Header)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析 GitHub 响应 %s %s：%w", method, u, err)
		}
	}
	if rb, ok := out.(bodyRateLimited); ok {
		if msg, limited := rb.rateLimited(); limited {
			return c.bodyRateLimitError(key, resp, &APIError{Status: resp.StatusCode, Method: method, URL: u, Message: msg})
		}
	}
	return nil
}

// bodyRateLimited 是在 HTTP 200 的响应体中报告限流的响应（GraphQL）：rateLimited 返回限流的说明和是否被限流。
type bodyRateLimited interface {
	rateLimited() (string, bool)
}

// newAPIError 根据错误响应构造 APIError。
func newAPIError(resp *http.Response, data []byte, method, u string) *APIError {
	ae := &APIError{Status: resp.StatusCode, Method: method, URL: u}
	var m struct {
		Message string `json:"message"`
		Errors  []any  `json:"errors"`
	}
	if json.Unmarshal(data, &m) == nil && m.Message != "" {
		ae.Message = m.Message
		if len(m.Errors) > 0 {
			if eb, err := json.Marshal(m.Errors); err == nil {
				ae.Message += " " + string(eb)
			}
		}
		return ae
	}
	ae.Message = strings.TrimSpace(string(data))
	if len(ae.Message) > maxErrorMessage {
		// 按字节截断可能切开多字节字符，去掉不完整的部分
		ae.Message = strings.ToValidUTF8(ae.Message[:maxErrorMessage], "")
	}
	return ae
}

// App 以 App JWT 调用 API。
func (c *Client) App(ctx context.Context, method, path string, body, out any) error {
	jwt, err := c.appJWT()
	if err != nil {
		return &notSentError{err}
	}
	return c.do(ctx, appLimitKey, "Bearer "+jwt, method, path, body, out)
}

func (c *Client) appGet(ctx context.Context, path string, out any) error {
	return c.App(ctx, http.MethodGet, path, nil, out)
}

// Do 以 installation token 调用 API（便于调用未封装的接口）。
// 返回 401 时清掉缓存的 token；请求体可以重放时用新 token 重试一次。用给定 token 的客户端（InstWithToken）不碰缓存、不重试。
func (i *Inst) Do(ctx context.Context, method, path string, body, out any) error {
	return i.doKey(ctx, i.ID, method, path, body, out)
}

// doKey 同 Do，key 是限流键（REST 为 installation ID，GraphQL 见 graphQLLimitKey）。
func (i *Inst) doKey(ctx context.Context, key int64, method, path string, body, out any) error {
	// 限流中时连 token 也不签发
	if err := i.c.checkLimit(key); err != nil {
		return err
	}
	if i.fixed {
		if i.token == "" {
			return &notSentError{fmt.Errorf("%s %s：固定凭据的客户端没有 token，不发送请求", method, path)}
		}
		return i.c.do(ctx, key, "token "+i.token, method, path, body, out)
	}
	tok, err := i.c.installationToken(ctx, i.ID)
	if err != nil {
		return err
	}
	rewind, replayable := rewinder(body)
	err = i.c.do(ctx, key, "token "+tok, method, path, body, out)
	if !isStatus(err, http.StatusUnauthorized) {
		return err
	}
	// token 可能已被吊销：清掉缓存，下次调用会重新签发
	i.c.dropToken(i.ID)
	if !replayable {
		return err
	}
	if rerr := rewind(); rerr != nil {
		return errors.Join(err, fmt.Errorf("重试前重置请求体：%w", rerr))
	}
	tok, err = i.c.installationToken(ctx, i.ID)
	if err != nil {
		return err
	}
	return i.c.do(ctx, key, "token "+tok, method, path, body, out)
}

func (i *Inst) get(ctx context.Context, path string, out any) error {
	return i.Do(ctx, http.MethodGet, path, nil, out)
}

// rewinder 返回把请求体恢复到发送前状态的函数，以及请求体是否可以重放。
// JSON 请求体每次发送时重新编码，总是可以重放；rawBody 只有底层 reader 实现了 io.Seeker 时才可以。
func rewinder(body any) (rewind func() error, ok bool) {
	rb, isRaw := body.(rawBody)
	if !isRaw {
		return func() error { return nil }, true
	}
	s, isSeeker := rb.r.(io.Seeker)
	if !isSeeker {
		return nil, false
	}
	off, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, false
	}
	return func() error {
		_, err := s.Seek(off, io.SeekStart)
		return err
	}, true
}

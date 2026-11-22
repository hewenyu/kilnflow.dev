package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/httpretry"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// registry 客户端：按 OCI Distribution 协议推送 blob、manifest 和 tag。
// 认证使用 registry 的标准流程：401 + WWW-Authenticate 质询，Basic 直接带用户名密码，
// Bearer 先用用户名密码向 realm 换取 token（Docker Hub、GHCR 都是这种）。

const (
	// defaultAttempts 是可重试错误（网络错误、429、5xx）的最大尝试次数。
	defaultAttempts = 5
	// defaultBackoff 是第一次重试前的等待时间，之后每次翻倍，最长 maxBackoff。
	defaultBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
	// maxErrorBody 是错误信息中保留的响应体长度。
	maxErrorBody = 512
	// maxTokenBody 是 token 响应的大小上限。
	maxTokenBody = 1 << 20
)

// manifestAccept 是查询 manifest 时接受的类型。
const manifestAccept = MediaTypeOCIIndex + ", " + MediaTypeOCIManifest + ", " + MediaTypeDockerList + ", " + MediaTypeDockerManifest

// ErrUnauthorized 表示凭据无效或没有推送权限。
var ErrUnauthorized = errors.New("registry 认证失败：用户名或密码（token）无效，或没有该仓库的推送权限")

// Error 是 registry 返回的错误。只包含方法、路径（不含查询参数）、状态码和 registry 的错误码，
// 不包含任何请求头，因此不会带出 Authorization 或密码。
type Error struct {
	Method string
	Path   string
	Status int
	// Detail 是 registry 返回的错误码和说明（已截断、已脱敏）。
	Detail string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s：HTTP %d", e.Method, e.Path, e.Status)
	if e.Detail != "" {
		s += "：" + e.Detail
	}
	return s
}

// Client 是一个仓库（Name）的 registry 客户端。不是并发安全的：一次发布一个客户端，顺序使用。
type Client struct {
	name     Name
	base     string
	username string
	password string
	hc       *http.Client
	// auth 是当前使用的 Authorization 头（认证成功后设置）。
	auth string
	// attempts / backoff 是重试参数，测试中可以调小。
	attempts int
	backoff  time.Duration
}

// NewClient 返回推送到 name 的客户端。hc 为 nil 时使用 http.DefaultClient。
// 客户端使用 hc 的副本，加上安全的重定向策略（见 safeRedirects）。
func NewClient(name Name, username, password string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	hc = safeRedirects(hc)
	scheme := "https"
	if name.Insecure() {
		scheme = "http"
	}
	return &Client{
		name: name, base: scheme + "://" + name.Host, username: username, password: password, hc: hc,
		attempts: defaultAttempts, backoff: defaultBackoff,
	}
}

// maxRedirects 是调用方没有设置重定向策略时最多跟随的重定向次数（与 net/http 的默认值一致）。
const maxRedirects = 10

// errInsecureRedirect 表示 registry 或 token 地址把 https 请求重定向到了 http。
var errInsecureRedirect = errors.New("拒绝从 https 重定向到 http（凭据会以明文传输）")

// safeRedirects 返回 hc 的副本，重定向时：
//   - 拒绝从 https 降级到 http：net/http 对同一主机的重定向会保留 Authorization，不检查协议；
//   - 目标与最初请求的 origin（协议 + 主机 + 端口）不同时去掉 Authorization：net/http 只比较主机名，
//     跨端口、子域名的跳转仍会带上凭据；
//   - 之后再执行调用方原有的重定向策略（没有时最多跟随 maxRedirects 次）。
//
// registry 请求和换取 token 的请求都使用它。
func safeRedirects(hc *http.Client) *http.Client {
	c := *hc
	next := hc.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			first := via[0].URL
			for _, v := range via {
				if v.URL.Scheme == "https" && req.URL.Scheme != "https" {
					return errInsecureRedirect
				}
			}
			if !sameOrigin(first, req.URL) {
				req.Header.Del("Authorization")
			}
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("重定向超过 %d 次", maxRedirects)
		}
		return nil
	}
	return &c
}

// sameOrigin 判断两个地址的协议、主机和端口（省略时按协议的默认端口）是否相同。
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && portOf(a) == portOf(b)
}

// portOf 返回地址的端口，省略时按协议返回默认端口。
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// secrets 返回错误信息中需要去掉的凭据：密码、base64 编码的用户名密码（不依赖当前认证方式，Bearer 时也要去掉），
// 以及当前的 token。
func (c *Client) secrets() []string {
	s := []string{c.password}
	if c.username != "" || c.password != "" {
		s = append(s, base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password)))
	}
	return append(s, strings.TrimPrefix(strings.TrimPrefix(c.auth, "Bearer "), "Basic "))
}

// redact 去掉字符串中的密码和当前 token。
func (c *Client) redact(s string) string { return redact.String(s, c.secrets()...) }

// request 描述一次请求。body 每次尝试都会重新调用，返回 nil 表示没有请求体。
type request struct {
	method      string
	url         string // 完整 URL
	accept      string
	contentType string
	size        int64
	body        func(ctx context.Context) (io.ReadCloser, error)
	// once 为 true 时可重试错误也不在这里重试（由调用方从更早的步骤重来，例如 blob 上传）。
	once bool
}

// pathOf 返回 URL 的路径部分（错误信息只显示路径，不显示查询参数）。
func pathOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Path
	}
	return "?"
}

// do 发送请求：遇到 401 时按质询认证一次后重发；网络错误、429、5xx 按指数退避重试。
// 返回的响应状态码不是 401、429、5xx（或者重试次数已用完）。
func (c *Client) do(ctx context.Context, r request) (*http.Response, error) {
	reauthed := false
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		resp, err := c.send(ctx, r)
		switch {
		case err != nil:
			if ctx.Err() != nil || r.once || attempt >= c.attempts {
				return nil, fmt.Errorf("%s %s：%w", r.method, pathOf(r.url), httpretry.Transport(err, c.redact))
			}
		case resp.StatusCode == http.StatusUnauthorized && !reauthed:
			reauthed = true
			challenge := resp.Header.Get("WWW-Authenticate")
			challengeURL := r.url
			if resp.Request != nil {
				challengeURL = resp.Request.URL.String()
			}
			drain(resp)
			// 上传地址和重定向目标可以是其他 origin，但不能代表 registry 要求认证：
			// 否则它们能通过 Bearer realm 把 registry 的用户名和密码引到自己的 token 服务。
			if !c.isRegistryOrigin(challengeURL) {
				return nil, ErrUnauthorized
			}
			if err := c.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			attempt--
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			drain(resp)
			return nil, ErrUnauthorized
		case httpretry.Retryable(resp.StatusCode) && !r.once && attempt < c.attempts:
			drain(resp)
		default:
			return resp, nil
		}
		if err := httpretry.Sleep(ctx, wait); err != nil {
			return nil, err
		}
		wait = min(wait*2, maxBackoff)
	}
}

// send 发送一次请求（不重试）。
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	var body io.ReadCloser
	if r.body != nil {
		b, err := r.body(ctx)
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		if body != nil {
			_ = body.Close() // 请求没有发出，关闭失败不影响结果
		}
		return nil, err
	}
	if body != nil {
		req.ContentLength = r.size
	}
	if r.accept != "" {
		req.Header.Set("Accept", r.accept)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	// 凭据只发给 registry 本身：上传地址可能指向其他主机（例如对象存储），那里不需要也不应该拿到 token；
	// 同一主机但协议不同（https 的 registry 返回 http 的上传地址）时也不发送，避免凭据以明文传输
	if c.auth != "" && c.isRegistryOrigin(req.URL.String()) {
		req.Header.Set("Authorization", c.auth)
	}
	return c.hc.Do(req)
}

// isRegistryOrigin 统一发送凭据和接受认证质询的 origin 边界（包含协议与有效端口）。
func (c *Client) isRegistryOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	base, err := url.Parse(c.base)
	return err == nil && sameOrigin(base, u)
}

// errorFrom 把非预期的响应转换为 *Error（会读完并关闭响应体）。先去掉凭据再截断：
// 先截断会把凭据切成两半，剩下的前半段不再与完整的值匹配。
func (c *Client) errorFrom(method, u string, resp *http.Response) error {
	defer drain(resp)
	raw := redact.ReadLimited(resp.Body, maxErrorBody, c.secrets()...)
	b := []byte(raw)
	detail := strings.TrimSpace(raw)
	var re struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &re) == nil && len(re.Errors) > 0 {
		var parts []string
		for _, e := range re.Errors {
			parts = append(parts, strings.TrimSpace(e.Code+" "+e.Message))
		}
		detail = strings.Join(parts, "；")
	}
	// JSON 中的字符串可能以转义的形式带着凭据，解码后再去掉一次
	return &Error{Method: method, Path: pathOf(u), Status: resp.StatusCode, Detail: shorten.Text(c.redact(detail), maxErrorBody)}
}

// authenticate 按 WWW-Authenticate 质询取得 Authorization 头。
func (c *Client) authenticate(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if c.username == "" && c.password == "" {
			return ErrUnauthorized
		}
		c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
		return nil
	case "bearer":
		return c.fetchToken(ctx, params)
	}
	return fmt.Errorf("registry %s 返回了不支持的认证方式 %q", c.name.Host, scheme)
}

// fetchToken 向 realm 换取推送本仓库的 Bearer token（Docker 的 token 认证流程）。
func (c *Client) fetchToken(ctx context.Context, params map[string]string) error {
	realm := params["realm"]
	ru, err := url.Parse(realm)
	// 只允许 https；本机测试 registry（Insecure）也可以用 http。地址必须有主机名（https://:443 之类不行）。
	if err != nil || ru.Hostname() == "" || (ru.Scheme != "https" && (ru.Scheme != "http" || !c.name.Insecure())) {
		return fmt.Errorf("registry %s 的 token 地址 %q 无效（必须是 https）", c.name.Host, realm)
	}
	q := ru.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+c.name.Repository+":pull,push")
	ru.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return err
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("获取 registry token（%s）：%w", ru.Host, httpretry.Transport(err, c.redact))
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody+1))
	if err != nil {
		return fmt.Errorf("获取 registry token（%s）：%w", ru.Host, httpretry.Transport(err, c.redact))
	}
	if len(b) > maxTokenBody {
		return fmt.Errorf("获取 registry token（%s）：响应超过大小上限", ru.Host)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("获取 registry token（%s）：HTTP %d：%s", ru.Host, resp.StatusCode, shorten.Text(c.redact(string(b)), maxErrorBody))
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return fmt.Errorf("获取 registry token（%s）：响应不是 JSON", ru.Host)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" {
		return fmt.Errorf("获取 registry token（%s）：响应中没有 token", ru.Host)
	}
	c.auth = "Bearer " + t
	return nil
}

// parseChallenge 解析 WWW-Authenticate，例如 Bearer realm="https://auth.docker.io/token",service="registry.docker.io"。
func parseChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			params[k], rest = strings.TrimSpace(val), after
		}
	}
	return scheme, params
}

func (c *Client) url(kind, ref string) string {
	return c.base + "/v2/" + c.name.Repository + "/" + kind + "/" + ref
}

// Ping 确认 registry 可以访问，并用凭据完成认证（凭据错误时返回 ErrUnauthorized）。
// 凭据只有读权限时，有的 registry（例如 Docker Hub）在这一步仍会成功，到上传 blob 时才返回 ErrUnauthorized。
func (c *Client) Ping(ctx context.Context) error {
	// 查询一个不存在的 tag：需要认证的 registry 会先返回 401 质询，认证后应返回 404
	_, _, err := c.ManifestDigest(ctx, "kiln-auth-check")
	return err
}

// BlobExists 查询仓库中是否已有该 blob。
func (c *Client) BlobExists(ctx context.Context, digest string) (bool, error) {
	u := c.url("blobs", digest)
	resp, err := c.do(ctx, request{method: http.MethodHead, url: u})
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		drain(resp)
		return true, nil
	case http.StatusNotFound:
		drain(resp)
		return false, nil
	}
	return false, c.errorFrom(http.MethodHead, u, resp)
}

// PushBlob 上传 blob（整体上传：POST 开始上传，PUT 提交内容）。失败可重试时从 POST 重新开始。
// open 每次尝试都会调用，返回 blob 的内容。
func (c *Client) PushBlob(ctx context.Context, d Descriptor, open func(ctx context.Context) (io.ReadCloser, error)) error {
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		err := c.pushBlobOnce(ctx, d, open)
		if err == nil {
			return nil
		}
		var re *Error
		transient := !errors.Is(err, ErrUnauthorized) && (!errors.As(err, &re) || httpretry.Retryable(re.Status))
		if !transient || attempt >= c.attempts || ctx.Err() != nil {
			return err
		}
		if err := httpretry.Sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(wait*2, maxBackoff)
	}
}

func (c *Client) pushBlobOnce(ctx context.Context, d Descriptor, open func(ctx context.Context) (io.ReadCloser, error)) error {
	start := c.base + "/v2/" + c.name.Repository + "/blobs/uploads/"
	resp, err := c.do(ctx, request{method: http.MethodPost, url: start, size: 0, body: emptyBody})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return c.errorFrom(http.MethodPost, start, resp)
	}
	loc := resp.Header.Get("Location")
	drain(resp)
	u, err := c.resolve(loc)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("digest", d.Digest)
	u.RawQuery = q.Encode()
	put := u.String()
	resp, err = c.do(ctx, request{method: http.MethodPut, url: put, contentType: "application/octet-stream", size: d.Size, body: open, once: true})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return c.errorFrom(http.MethodPut, put, resp)
	}
	drain(resp)
	return nil
}

func emptyBody(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// resolve 把上传地址（可能是相对路径）解析为绝对 URL。地址可以在其他主机上（例如对象存储），
// send 只把凭据发给 registry 本身。
func (c *Client) resolve(loc string) (*url.URL, error) {
	base, err := url.Parse(c.base + "/")
	if err != nil {
		return nil, err
	}
	u, err := base.Parse(loc)
	if err != nil || loc == "" {
		return nil, fmt.Errorf("registry 返回的上传地址无效：%q", pathOf(loc))
	}
	// Location 可以指向其他主机（例如对象存储），但必须是 https；只有本机 registry 才允许 http，
	// 不能把 blob 经明文发到其他主机。
	switch {
	case u.Hostname() == "":
		return nil, fmt.Errorf("registry 返回的上传地址无效：%q", pathOf(loc))
	case u.Scheme == "https":
	case u.Scheme == "http" && c.name.Insecure():
	default:
		return nil, fmt.Errorf("registry 返回的上传地址必须是 https：%q", pathOf(loc))
	}
	return u, nil
}

// PushManifest 按 ref（digest 或 tag）上传 manifest / index；registry 返回的 Docker-Content-Digest
// 与 m.Digest 不一致时报错。
func (c *Client) PushManifest(ctx context.Context, m Manifest, ref string) error {
	u := c.url("manifests", ref)
	body := func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(m.Body)), nil }
	resp, err := c.do(ctx, request{method: http.MethodPut, url: u, contentType: m.MediaType, size: int64(len(m.Body)), body: body})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return c.errorFrom(http.MethodPut, u, resp)
	}
	got := resp.Header.Get("Docker-Content-Digest")
	drain(resp)
	if got != "" && got != m.Digest {
		return fmt.Errorf("registry 记录的 digest %s 与推送的 %s 不一致", got, m.Digest)
	}
	return nil
}

// ManifestDigest 查询 tag（或 digest）当前指向的 manifest digest；不存在时 found 为 false。
func (c *Client) ManifestDigest(ctx context.Context, ref string) (digest string, found bool, err error) {
	u := c.url("manifests", ref)
	resp, err := c.do(ctx, request{method: http.MethodHead, url: u, accept: manifestAccept})
	if err != nil {
		return "", false, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		drain(resp)
		return "", false, nil
	case http.StatusOK:
	default:
		return "", false, c.errorFrom(http.MethodHead, u, resp)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	drain(resp)
	if d != "" {
		return d, true, nil
	}
	// 没有返回 Docker-Content-Digest 的 registry：取回内容自己计算
	resp, err = c.do(ctx, request{method: http.MethodGet, url: u, accept: manifestAccept})
	if err != nil {
		return "", false, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, c.errorFrom(http.MethodGet, u, resp)
	}
	defer drain(resp)
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > maxManifestSize {
		return "", false, fmt.Errorf("manifest 超过大小上限 %d 字节", maxManifestSize)
	}
	return Digest(b), true, nil
}

// drain 读完并关闭响应体，让连接可以复用（读取上限见 httpretry.Discard；关闭留在本包中，bodyclose 才能确认）。
func drain(resp *http.Response) {
	httpretry.Discard(resp.Body)
	_ = resp.Body.Close()
}

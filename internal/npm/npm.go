// Package npm 是 npm registry 的客户端（release.npm，PLAN #27.3）：Server 直接用 HTTP 发布 npm 包，不安装 npm。
//
// 三个操作：Get 查询包的元数据（packument），Public 不带 token 查询包是否公开，Publish 发布一个版本
// （与 `npm publish` 发出的请求相同：PUT <registry>/@scope%2fname，Authorization: Bearer）。npm 的版本不可覆盖，所以 Publish 不自动重试，
// 结果不确定（Uncertain）时由调用方先 Get 再决定。
//
// registry 必须是 https（本机地址可以用 http，供本地测试）；不跟随重定向，token 只发给配置的地址；
// Get、Public 对网络错误、429、5xx 按退避重试；错误信息中不含 token。
package npm

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // npm 的 dist.shasum 规定用 sha1，只用于兼容，完整性由 sha512 保证
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/endpoint"
	"github.com/hewenyu/kilnflow.dev/internal/httpretry"
	"github.com/hewenyu/kilnflow.dev/internal/redact"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// DefaultRegistry 是 npm 官方 registry。
const DefaultRegistry = "https://registry.npmjs.org/"

// 发布的可见性（npm publish --access）。
const (
	AccessPublic     = "public"
	AccessRestricted = "restricted"
)

// MaxNameLength 是完整包名（含 scope）的长度上限。
const MaxNameLength = 214

// Packument 是 GET <registry>/<name> 返回的包元数据（只解析用到的字段）。
type Packument struct {
	Name     string             `json:"name"`
	DistTags map[string]string  `json:"dist-tags"`
	Versions map[string]Version `json:"versions"`
}

// Version 是 packument 中一个版本的元数据。
type Version struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    Dist   `json:"dist"`
}

// Dist 描述一个版本的 tarball。Integrity 形如 sha512-<base64>，Shasum 是 sha1 的小写十六进制。
type Dist struct {
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
	Tarball   string `json:"tarball"`
}

// PublishRequest 描述要发布的一个版本。
type PublishRequest struct {
	Name    string // 完整包名，如 @acme/kilnflow-runner-darwin-arm64
	Version string
	// Manifest 是 package.json 的内容，原样放进 versions[Version]；其中的 name、version 必须与上面一致（可以不写）。
	Manifest map[string]any
	Tarball  []byte // npm tarball（.tgz）
	DistTag  string // 如 latest、next
	Access   string // AccessPublic 或 AccessRestricted
}

// Error 是 registry 返回的错误（状态码不是 2xx）。错误信息只包含方法、路径、状态码和 registry 的说明，不含请求头。
type Error struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s：HTTP %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += "：" + e.Message
	}
	return s
}

// IsStatus 判断 err 是否是 registry 返回的、状态码为 status 的错误。
func IsStatus(err error, status int) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Status == status
}

// ErrNoResponse 表示请求已经（或可能已经）发出，但没有收到响应：网络错误、超时、取消。
var ErrNoResponse = errors.New("没有收到响应")

// Uncertain 判断 Publish 返回的错误是否意味着"不确定是否已经发布"：没有收到响应（ErrNoResponse）和 5xx
// 都可能发生在 registry 已经保存之后，调用方应先 Get 查询再决定是否重试；参数错误和其他 4xx 表示没有发布。
func Uncertain(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status >= http.StatusInternalServerError
	}
	return errors.Is(err, ErrNoResponse)
}

// ScopeError 是 scope 不合格的错误：Scope 是原样的 scope；Part 是 @ 之后部分的问题，为 nil 表示没有以 @ 开头。
// Dashboard 按 Part 的原因码显示当前语言的说明，CLI 和日志用 Error()。
type ScopeError struct {
	Scope string
	Part  *PartError
}

func (e *ScopeError) Error() string {
	if e.Part == nil {
		return fmt.Sprintf("scope %q 必须以 @ 开头", e.Scope)
	}
	return fmt.Sprintf("scope %q 无效：%s", e.Scope, e.Part)
}

func (e *ScopeError) Unwrap() error {
	if e.Part == nil {
		return nil
	}
	return e.Part
}

// PartProblem 是 scope 或包名的一部分不合格的原因码（不随语言变化）。
type PartProblem string

const (
	// PartEmpty：为空。
	PartEmpty PartProblem = "empty"
	// PartLeadingDot：以 . 或 _ 开头。
	PartLeadingDot PartProblem = "leading_dot"
	// PartUppercase：有大写字母。
	PartUppercase PartProblem = "uppercase"
	// PartBadChar：有不允许的字符（Char）。
	PartBadChar PartProblem = "bad_char"
)

// PartError 是 scope 或包名的一部分（不含 @ 和 /）不合格的错误；Char 是不允许的字符（只用于 PartBadChar）。
type PartError struct {
	Problem PartProblem
	Char    rune
}

func (e *PartError) Error() string {
	switch e.Problem {
	case PartEmpty:
		return "不能为空"
	case PartLeadingDot:
		return "不能以 . 或 _ 开头"
	case PartUppercase:
		return "不能有大写字母"
	}
	return fmt.Sprintf("不能包含字符 %q（只能用小写字母、数字、-、.、_）", e.Char)
}

// ValidScope 检查 scope（形如 @acme）：以 @ 开头，其余部分只含小写字母、数字、-、.、_，不以 . 或 _ 开头。
// 不合格时返回 *ScopeError。
func ValidScope(scope string) error {
	rest, ok := strings.CutPrefix(scope, "@")
	if !ok {
		return &ScopeError{Scope: scope}
	}
	if pe := partProblem(rest); pe != nil {
		return &ScopeError{Scope: scope, Part: pe}
	}
	return nil
}

// NormalizeScope 给不带 @ 的 scope 补上 @，再按 ValidScope 检查（不合格时返回 *ScopeError）。
func NormalizeScope(scope string) (string, error) {
	if !strings.HasPrefix(scope, "@") {
		scope = "@" + scope
	}
	if err := ValidScope(scope); err != nil {
		return "", err
	}
	return scope, nil
}

// ValidName 检查完整包名（@scope/name 或不带 scope 的 name）是否符合 npm 对新包的规则：
// 总长不超过 MaxNameLength；scope 和 name 都只含小写字母、数字、-、.、_（URL 安全），不以 . 或 _ 开头。
// 比 npm 稍严：带 scope 时 name 部分也不能以 . 或 _ 开头。
func ValidName(name string) error {
	if name == "" {
		return errors.New("包名为空")
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("包名 %q 超过 %d 个字符", shorten.Text(name, 64), MaxNameLength)
	}
	if rest, ok := strings.CutPrefix(name, "@"); ok {
		scope, short, ok := strings.Cut(rest, "/")
		if !ok {
			return fmt.Errorf("包名 %q 无效：带 scope 的包名形如 @scope/name", name)
		}
		if err := validPart(scope); err != nil {
			return fmt.Errorf("包名 %q 的 scope 无效：%w", name, err)
		}
		if err := validPart(short); err != nil {
			return fmt.Errorf("包名 %q 无效：%w", name, err)
		}
		return nil
	}
	return validShortName(name)
}

// ValidShortName 检查不含 scope 的包名（release.npm[].name 以及 scope 后的部分）：
// 长度不超过 MaxNameLength，只含小写字母、数字、-、.、_，不以 . 或 _ 开头，不是 npm 保留的名字。
func ValidShortName(name string) error {
	if name == "" {
		return errors.New("包名为空")
	}
	if strings.ContainsAny(name, "/@") {
		return errors.New("不要写 scope（不能包含 @ 或 /）")
	}
	return validShortName(name)
}

// validShortName 是 ValidName 与 ValidShortName 共用的短名检查。
func validShortName(name string) error {
	if len(name) > MaxNameLength {
		return fmt.Errorf("包名 %q 超过 %d 个字符", shorten.Text(name, 64), MaxNameLength)
	}
	if err := validPart(name); err != nil {
		return err
	}
	if name == "node_modules" || name == "favicon.ico" {
		return fmt.Errorf("包名 %q 是 npm 保留的名字", name)
	}
	return nil
}

// validPart 检查 scope 或 name 部分（不含 @ 和 /），不合格时返回 *PartError。
func validPart(s string) error {
	if pe := partProblem(s); pe != nil {
		return pe
	}
	return nil
}

// partProblem 返回 scope 或 name 部分的问题，合格时返回 nil。
func partProblem(s string) *PartError {
	switch {
	case s == "":
		return &PartError{Problem: PartEmpty}
	case s[0] == '.' || s[0] == '_':
		return &PartError{Problem: PartLeadingDot}
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' && r != '_' {
			if r >= 'A' && r <= 'Z' {
				return &PartError{Problem: PartUppercase}
			}
			return &PartError{Problem: PartBadChar, Char: r}
		}
	}
	return nil
}

// FullName 拼出完整包名 @scope/short（scope 可以带或不带 @）。不做校验，用 ValidName 检查结果。
func FullName(scope, short string) string {
	return "@" + strings.TrimPrefix(scope, "@") + "/" + short
}

// ShortName 返回去掉 scope 的包名（@scope/name → name；不带 scope 时原样返回）。
func ShortName(name string) string {
	if strings.HasPrefix(name, "@") {
		if _, short, ok := strings.Cut(name, "/"); ok {
			return short
		}
	}
	return name
}

// escapeName 把包名转成 registry 路径：@scope/name → @scope%2fname（与 npm 相同）。只用于已通过 ValidName 的名字。
func escapeName(name string) string {
	return strings.Replace(name, "/", "%2f", 1)
}

// Integrity 返回 b 的 npm integrity（sha512-<base64>，即 dist.integrity）。
func Integrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// Shasum 返回 b 的 sha1 小写十六进制（即 dist.shasum）。
func Shasum(b []byte) string {
	sum := sha1.Sum(b) //nolint:gosec // npm 的 dist.shasum 规定用 sha1，只用于兼容
	return hex.EncodeToString(sum[:])
}

// CheckRegistry 检查 registry 地址（不合格时返回 *endpoint.Error）：必须是 https（主机为本机地址时可以用 http），不能带查询参数、fragment 或用户信息。
// 可以带路径前缀（如私有 registry 的 https://host/api/npm/repo/）。
func CheckRegistry(raw string) error { return endpoint.Check(raw, "npm registry 地址") }

// 读取响应体的上限（重试参数与超时见 httpretry）。
const (
	// maxPackument 是读取 packument 的上限（版本很多的包可能有几 MB）。
	maxPackument = 64 << 20
	// maxErrorBody 是读取错误响应体的上限。
	maxErrorBody = 4 << 10
)

// Client 是一个 registry 的客户端。可以并发使用。
type Client struct {
	base      string // 以 / 结尾
	path      string // base 的路径部分（以 / 结尾），用于错误信息
	token     string
	userAgent string
	hc        *http.Client
	// attempts / backoff 是 Get 的重试参数，测试中可以调小。
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

// WithRetry 设置 Get 的重试次数和初始退避（测试用）。Publish 不重试。
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(c *Client) { c.attempts, c.backoff = max(attempts, 1), backoff }
}

// NewClient 返回 registry 的客户端。token 为空时不带 Authorization（只能查询公开的包）；userAgent 形如 kiln/<版本>。
func NewClient(registry, token, userAgent string, opts ...Option) (*Client, error) {
	if err := CheckRegistry(registry); err != nil {
		return nil, err
	}
	base := strings.TrimRight(registry, "/") + "/"
	u, _ := url.Parse(base) // CheckRegistry 已经解析过
	c := &Client{
		base: base, path: u.EscapedPath(), token: token, userAgent: userAgent,
		hc: &http.Client{
			Timeout: httpretry.RequestTimeout,
			// 不跟随重定向：token 和 tarball 只发给配置的地址
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		attempts: httpretry.DefaultAttempts, backoff: httpretry.DefaultBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Registry 返回 registry 地址（以 / 结尾）。
func (c *Client) Registry() string { return c.base }

// TarballURL 返回 tarball 的下载地址：<registry><name>/-/<不含 scope 的名字>-<version>.tgz（npmjs 的格式）。
// 记录发布结果时以 Get 返回的 dist.tarball 为准，这里只在查询不到时作为后备。
func (c *Client) TarballURL(name, version string) string {
	return c.base + name + "/-/" + ShortName(name) + "-" + version + ".tgz"
}

// Get 查询包的元数据：包不存在（404）时返回 nil, nil。restricted 的包没有权限时 registry 也返回 404。
func (c *Client) Get(ctx context.Context, name string) (*Packument, error) {
	return c.get(ctx, name, false)
}

// Public 判断包是否已经存在且公开：不带 token 查询（与匿名用户看到的相同），能查到就是公开的；
// 404 和要求认证（401、403）都表示匿名用户看不到。
// 发布时的 access 只决定新包的可见性，已有的公开包不会因为 access: restricted 变成私有，调用方据此判断内容是否会公开。
func (c *Client) Public(ctx context.Context, name string) (bool, error) {
	doc, err := c.get(ctx, name, true)
	switch {
	case IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden):
		return false, nil
	case err != nil:
		return false, err
	}
	return doc != nil, nil
}

func (c *Client) get(ctx context.Context, name string, anonymous bool) (*Packument, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	r := request{method: http.MethodGet, path: escapeName(name), anonymous: anonymous, header: http.Header{
		"Accept": {"application/json"},
		// 发布后立即查询时不要拿到 CDN 缓存的旧结果
		"Cache-Control": {"no-cache"},
	}}
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, c.errorOf(r, resp)
	}
	var p Packument
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPackument)).Decode(&p); err != nil {
		return nil, fmt.Errorf("%s %s：响应无法解析：%w", r.method, c.path+r.path, err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("%s %s：响应中的包名 %q 与请求的 %q 不一致", r.method, c.path+r.path, p.Name, name)
	}
	return &p, nil
}

// ErrPrivate 表示 package.json 中的 private 为真值：npm 不发布这样的包（与 npm publish 的 EPRIVATE 相同），
// 用来防止把内部包误发到 registry。
var ErrPrivate = errors.New(`package.json 中的 private 为真值（如 true），npm 不发布私有包（确认要发布时去掉它，或在 release.npm 的 package 中写 private: false）`)

// IsPrivate 判断 package.json 是否标记为私有。与 npm publish 的 if (manifest.private) 相同，按 JavaScript 的
// 真值判断：true、非空字符串（包括 "false"）、非 0 数字、对象和数组都算私有；false、""、0、null 和没有这个字段不算。
func IsPrivate(manifest map[string]any) bool {
	switch v := manifest["private"].(type) {
	case nil:
		return false
	case bool:
		return v
	case json.Number:
		f, err := v.Float64()
		return err != nil || (f != 0 && !math.IsNaN(f))
	case string:
		return v != ""
	}
	rv := reflect.ValueOf(manifest["private"])
	switch {
	case rv.CanInt():
		return rv.Int() != 0
	case rv.CanUint():
		return rv.Uint() != 0
	case rv.CanFloat():
		return rv.Float() != 0 && !math.IsNaN(rv.Float())
	default:
		return true // 对象、数组等
	}
}

// Publish 发布一个版本（PUT <registry>/<name>，请求体与 npm publish 相同）。不重试：错误满足 Uncertain 时
// 版本可能已经发布，调用方应先 Get 查询；版本已存在时 registry 返回 403。
func (c *Client) Publish(ctx context.Context, req *PublishRequest) error {
	body, err := c.publishBody(req)
	if err != nil {
		return err
	}
	r := request{
		method: http.MethodPut, path: escapeName(req.Name), size: int64(len(body)),
		header: http.Header{"Accept": {"application/json"}, "Content-Type": {"application/json"}},
		body:   func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
	}
	resp, err := c.send(ctx, r)
	if err != nil {
		return c.noResponse(r, err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.errorOf(r, resp)
	}
	return nil
}

// publishBody 生成 npm publish 的文档（与 libnpmpublish 相同）。
func (c *Client) publishBody(req *PublishRequest) ([]byte, error) {
	switch {
	case c.token == "":
		return nil, errors.New("发布 npm 包需要 token")
	case req.Version == "":
		return nil, fmt.Errorf("发布 %s：版本为空", req.Name)
	case req.DistTag == "":
		return nil, fmt.Errorf("发布 %s@%s：dist-tag 为空", req.Name, req.Version)
	case req.Access != AccessPublic && req.Access != AccessRestricted:
		return nil, fmt.Errorf("发布 %s@%s：access 必须是 public 或 restricted，不能是 %q", req.Name, req.Version, req.Access)
	case len(req.Tarball) == 0:
		return nil, fmt.Errorf("发布 %s@%s：tarball 为空", req.Name, req.Version)
	}
	if err := ValidName(req.Name); err != nil {
		return nil, err
	}
	if IsPrivate(req.Manifest) {
		return nil, fmt.Errorf("发布 %s@%s：%w", req.Name, req.Version, ErrPrivate)
	}
	for _, f := range []struct{ key, want string }{{"name", req.Name}, {"version", req.Version}} {
		if v, ok := req.Manifest[f.key]; ok && v != f.want {
			return nil, fmt.Errorf("发布 %s@%s：package.json 中的 %s 是 %v，与要发布的 %q 不一致", req.Name, req.Version, f.key, v, f.want)
		}
	}
	m := maps.Clone(req.Manifest)
	if m == nil {
		m = map[string]any{}
	}
	dist := map[string]any{}
	if old, ok := m["dist"].(map[string]any); ok {
		maps.Copy(dist, old)
	}
	// npm 的附件名和 tarball 路径都用完整包名（registry 会把下载地址改写成 <name>/-/<不含 scope 的名字>-<version>.tgz）
	file := req.Name + "-" + req.Version + ".tgz"
	dist["integrity"], dist["shasum"] = Integrity(req.Tarball), Shasum(req.Tarball)
	dist["tarball"] = c.base + req.Name + "/-/" + file
	m["_id"], m["name"], m["version"], m["dist"] = req.Name+"@"+req.Version, req.Name, req.Version, dist

	doc := map[string]any{
		"_id":       req.Name,
		"name":      req.Name,
		"dist-tags": map[string]string{req.DistTag: req.Version},
		"versions":  map[string]any{req.Version: m},
		"access":    req.Access,
		"_attachments": map[string]any{file: map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(req.Tarball),
			"length":       len(req.Tarball),
		}},
	}
	if d, ok := req.Manifest["description"]; ok {
		doc["description"] = d
	}
	return json.Marshal(doc)
}

// request 描述一次请求。body 每次尝试都会重新调用，为 nil 表示没有请求体。
type request struct {
	method string
	path   string // 相对 base 的路径（已转义，不以 / 开头）
	header http.Header
	size   int64
	body   func() (io.ReadCloser, error)
	// anonymous 为 true 时不带 token（查询匿名用户能否看到）。
	anonymous bool
}

// do 发送请求：网络错误、429、5xx 按指数退避重试（带 Retry-After 时按它等待，最长 1 分钟）。
// 返回的响应状态码不是 429、5xx（或者重试次数已用完）。只用于幂等的请求。
func (c *Client) do(ctx context.Context, r request) (*http.Response, error) {
	wait := c.backoff
	for attempt := 1; ; attempt++ {
		resp, err := c.send(ctx, r)
		delay := wait
		switch {
		case err != nil:
			if ctx.Err() != nil || attempt >= c.attempts {
				return nil, c.noResponse(r, err)
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
	}
	maps.Copy(req.Header, r.header)
	if c.token != "" && !r.anonymous {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", c.userAgent)
	return c.hc.Do(req)
}

// noResponse 把发送请求的错误包装成 ErrNoResponse（去掉其中的 token）。
func (c *Client) noResponse(r request, err error) error {
	return fmt.Errorf("%s %s：%w：%w", r.method, c.path+r.path, ErrNoResponse, httpretry.Transport(err, c.redact))
}

// errorOf 把非预期的响应转换成 *Error（读取 registry 的 error 或 message，去掉其中的 token）。
// 先去掉 token 再截断，避免截断后只剩部分 token 而无法识别。
func (c *Client) errorOf(r request, resp *http.Response) error {
	e := &Error{Method: r.method, Path: c.path + r.path, Status: resp.StatusCode}
	raw := redact.ReadLimited(resp.Body, maxErrorBody, c.token)
	var body struct {
		Error   any `json:"error"`
		Message any `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &body) == nil && (body.Error != nil || body.Message != nil) {
		e.Message = c.redact(joinMessage(body.Error, body.Message))
	} else if s := strings.TrimSpace(raw); s != "" && !strings.ContainsAny(s, "<") {
		e.Message = shorten.Text(s, 200)
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		e.Message = "不跟随重定向（" + shorten.Text(c.redact(resp.Header.Get("Location")), 200) + "），请把 registry 改成最终地址"
	case resp.StatusCode == http.StatusUnauthorized && strings.Contains(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "otp"):
		e.Message += "（账号要求一次性密码，请改用 Automation 或 Granular Access Token）"
	}
	e.Message = shorten.Text(e.Message, 500)
	return e
}

// joinMessage 合并 npm 错误响应中的 error 和 message（可能是字符串或其他 JSON 值，可能相同）。
func joinMessage(vals ...any) string {
	var parts []string
	for _, v := range vals {
		var s string
		switch v := v.(type) {
		case nil:
			continue
		case string:
			s = v
		default:
			b, _ := json.Marshal(v) // 来自 json.Unmarshal，总能重新编码
			s = string(b)
		}
		if s = strings.TrimSpace(s); s != "" && (len(parts) == 0 || parts[0] != s) {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "：")
}

// redact 去掉字符串中的 token。
func (c *Client) redact(s string) string { return redact.String(s, c.token) }

// drain 读完并关闭响应体，让连接可以复用（读取上限见 httpretry.Discard；关闭留在本包中，bodyclose 才能确认）。
func drain(resp *http.Response) {
	httpretry.Discard(resp.Body)
	_ = resp.Body.Close()
}

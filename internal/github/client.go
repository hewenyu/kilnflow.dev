// Package github 是 Kiln 使用的 GitHub REST 客户端：App 认证、installation token、
// webhook 验签，以及 Planner、Publisher 用到的少量接口（PLAN #13）。
package github

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIURL    = "https://api.github.com"
	defaultUploadURL = "https://uploads.github.com"

	// apiVersion 是请求头 X-GitHub-Api-Version 的取值。
	apiVersion = "2022-11-28"
	userAgent  = "kiln-server"
	mediaType  = "application/vnd.github+json"

	// maxResponseSize 是读取单个响应体的上限。
	maxResponseSize = 256 << 20
	// maxErrorMessage 是非 JSON 错误响应保留的最大长度（字节）。
	maxErrorMessage = 300
	// defaultHTTPTimeout 是未配置 HTTP 客户端时的总超时（上传大 asset 需要较长时间）。
	defaultHTTPTimeout = 30 * time.Minute
	// maxHTTPRedirects 是默认客户端跟随重定向的次数上限。
	maxHTTPRedirects = 10
)

// defaultHTTP 是未配置 Client.HTTP 时使用的客户端。只初始化一次，之后只读：带总超时，
// 并在重定向时阻止 https 降级、跨 origin 时去掉 Authorization（防止把 installation token 发给其他主机）。
var defaultHTTP = &http.Client{
	Timeout:       defaultHTTPTimeout,
	CheckRedirect: safeRedirect,
}

// errInsecureRedirect 表示默认客户端拒绝从 https 重定向到 http。
var errInsecureRedirect = errors.New("拒绝从 https 重定向到 http（凭据会以明文传输）")

// safeRedirect 是默认客户端的重定向策略：拒绝 https→http 降级，跨 origin（协议 + 主机 + 端口）时去掉
// Authorization，并限制重定向次数。
func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxHTTPRedirects {
		return fmt.Errorf("重定向超过 %d 次", maxHTTPRedirects)
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errInsecureRedirect
	}
	if !sameOrigin(via[0].URL, req.URL) {
		req.Header.Del("Authorization")
	}
	return nil
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

// Client 以 GitHub App 的身份访问 API。
type Client struct {
	AppID     int64
	Key       *rsa.PrivateKey
	APIURL    string // 默认 https://api.github.com
	UploadURL string // 默认 https://uploads.github.com
	HTTP      *http.Client

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
	tokens map[int64]cachedToken
	// limits 是各 installation（App JWT 为 appLimitKey）的限流状态，见 ratelimit.go。
	limits map[int64]*limitState
	// clock 为 nil 时使用 time.Now（测试中替换），通过 now() 读取。
	clock func() time.Time
}

type cachedToken struct {
	token string
	exp   time.Time
}

func (c *Client) api() string {
	if c.APIURL == "" {
		return defaultAPIURL
	}
	return strings.TrimRight(c.APIURL, "/")
}

func (c *Client) uploads() string {
	if c.UploadURL == "" {
		return defaultUploadURL
	}
	return strings.TrimRight(c.UploadURL, "/")
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func esc(s string) string { return url.PathEscape(s) }

// escPath 对路径的每一段分别转义（保留 /）。
func escPath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

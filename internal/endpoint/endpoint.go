// Package endpoint 校验 Kiln 访问的外部服务地址（npm registry、HTTP 上传、Outline、OCI registry）：
// 必须有主机名、不能带用户信息、查询参数或 fragment，并且必须是 https（本机地址除外）。
// 这些规则只在这里定义一次，各协议包给出自己的称呼（what）以便错误信息说清楚是哪个配置。
package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// IsLoopbackHost 判断主机名是否指向本机：localhost、*.localhost 或 loopback IP。
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Problem 是地址不合格的原因：稳定的原因码，不随语言变化（Dashboard 按它显示当前语言的说明，CLI 和日志用 Error()）。
type Problem string

const (
	// ProblemInvalid：地址无法解析。
	ProblemInvalid Problem = "invalid"
	// ProblemNoHost：缺少主机名。
	ProblemNoHost Problem = "no_host"
	// ProblemUserInfo：带了用户信息（凭据应写在 token 中）。
	ProblemUserInfo Problem = "user_info"
	// ProblemQuery：带了查询参数或 fragment。
	ProblemQuery Problem = "query"
	// ProblemScheme：不是 https（只有本机地址可以用 http）。
	ProblemScheme Problem = "scheme"
)

// Error 是地址不合格的错误：What 是对地址的称呼，Raw 是原样的地址，Problem 是原因码，Err 是解析失败时 url.Parse 的错误。
// 地址带（或疑似带）用户信息时其中可能有凭据：Raw 和 Err 都为空，Error() 不回显地址。
type Error struct {
	What, Raw string
	Problem   Problem
	Err       error
}

func (e *Error) Error() string {
	switch e.Problem {
	case ProblemInvalid:
		return fmt.Sprintf("%s %q 无效：%v", e.What, e.Raw, e.Err)
	case ProblemNoHost:
		return fmt.Sprintf("%s %q 缺少主机名", e.What, e.Raw)
	case ProblemUserInfo:
		return e.What + " 不能包含用户信息（凭据写在 token 中）"
	case ProblemQuery:
		return fmt.Sprintf("%s %q 不能带查询参数或 fragment", e.What, e.Raw)
	}
	return fmt.Sprintf("%s %q 必须是 https（只有本机地址可以用 http）", e.What, e.Raw)
}

func (e *Error) Unwrap() error { return e.Err }

// Check 校验服务地址 raw；what 是错误信息中对它的称呼（例如 "npm registry 地址"）。不合格时返回 *Error。
// 用户信息最先判断：地址无法解析或缺少主机名时也不能把其中的凭据写进错误（url.Parse 的错误同样带着原样的地址）。
// 用 Hostname 而不是 Host 判断主机名：https://:443 的 Host 非空，但没有任何主机。
func Check(raw, what string) error {
	fail := func(p Problem, err error) error { return &Error{What: what, Raw: raw, Problem: p, Err: err} }
	if hasUserInfo(raw) {
		return &Error{What: what, Problem: ProblemUserInfo}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fail(ProblemInvalid, err)
	}
	switch {
	case u.User != nil:
		return &Error{What: what, Problem: ProblemUserInfo}
	case u.Hostname() == "":
		return fail(ProblemNoHost, nil)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#"):
		return fail(ProblemQuery, nil)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && IsLoopbackHost(u.Hostname()):
		return nil
	}
	return fail(ProblemScheme, nil)
}

// hasUserInfo 判断地址的 authority 部分（"://" 或开头的 "//" 之后、第一个 "/"、"?"、"#" 之前）是否带 "@"。
// 不依赖 url.Parse：带用户信息但无法解析的地址（例如密码中有不合法的 % 转义）也要识别出来。
func hasUserInfo(raw string) bool {
	s := raw
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	} else {
		s = strings.TrimPrefix(s, "//")
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Contains(s, "@")
}

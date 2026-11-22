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

// Check 校验服务地址 raw；what 是错误信息中对它的称呼（例如 "npm registry 地址"）。
// 用 Hostname 而不是 Host 判断主机名：https://:443 的 Host 非空，但没有任何主机。
func Check(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q 无效：%w", what, raw, err)
	}
	switch {
	case u.Hostname() == "":
		return fmt.Errorf("%s %q 缺少主机名", what, raw)
	case u.User != nil:
		return fmt.Errorf("%s %q 不能包含用户信息（凭据写在 token 中）", what, raw)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#"):
		return fmt.Errorf("%s %q 不能带查询参数或 fragment", what, raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && IsLoopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("%s %q 必须是 https（只有本机地址可以用 http）", what, raw)
}

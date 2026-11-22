package release

import (
	"net/url"
	"strings"
	"unicode"
)

// SafeWebURL 判断 raw 是否可以作为可点击的 http/https 链接：协议是 http 或 https，且有主机名，
// 不含用户信息、控制字符或反斜杠。外部上传服务返回的下载地址不可信，展示或记录前必须过这一层。
func SafeWebURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "\\") || strings.ContainsFunc(raw, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

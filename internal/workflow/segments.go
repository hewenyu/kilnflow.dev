package workflow

import (
	"regexp"
	"strings"
)

// safeSegmentRe 匹配可以安全用作路径片段、容器名或网络别名的标识符：字母或数字开头，
// 只含字母、数字、点、下划线和连字符，最长 64 个字符。
var safeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validSegment 判断 s 是否是安全的单段标识符：拒绝空值、"."、".."、包含 ".."、路径分隔符、冒号和空白。
func validSegment(s string) bool {
	return safeSegmentRe.MatchString(s) && !strings.Contains(s, "..")
}

// ValidServiceName 判断 services 的服务名：它会成为容器名和网络别名，必须与 toolcache 的 name 一样是安全的单段标识符。
func ValidServiceName(name string) bool {
	return validSegment(name)
}

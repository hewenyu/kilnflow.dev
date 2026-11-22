package workflow

import (
	"regexp"
	"strings"
)

// pinnedDigestRe 匹配 OCI 镜像引用的 digest 部分：sha256 加 64 位小写十六进制。
var pinnedDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// hasPinnedDigest 判断镜像引用是否用 digest 固定。它比 Contains("@sha256:") 严格：
// @sha256:、@sha256:abc、@sha256:../../x 这类写法都不算固定。
func hasPinnedDigest(ref string) bool {
	name, digest, ok := strings.CutLast(ref, "@")
	return ok && name != "" && pinnedDigestRe.MatchString(digest)
}

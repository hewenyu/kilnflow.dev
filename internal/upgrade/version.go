// Package upgrade 是 Runner 自升级（PLAN #27.5–#27.7、#27.11–#27.13）中 Server 和 Runner 共用的部分：版本号的
// 校验和比较、npm 包名和默认 registry。
package upgrade

import (
	"regexp"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/semver"
)

// ProgramCI 是自升级的程序名（按台升级只针对 CI Runner；AI 执行节点的连接器用 npm 升级，PLAN #33.3）。
const ProgramCI = "kiln-runner"

// versionRe 是升级用的严格 semver：X.Y.Z 或 X.Y.Z-<预发布>，不带 v，没有前导零，预发布只含字母、数字、点和连字符
// （每一段都可以含连字符，例如 1.3.0-rc-1）。
// 版本号会用作目录名（versions/<版本>），所以不接受 /、..、空白等（PLAN #27.5 威胁模型）。
var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// MaxVersionLen 是版本号的长度上限：版本号是 Runner 本地的目录名，启动器只认这么长的名字（runner 的 localVersionRe），
// Server 指定的版本、下载器和启动器都按同一个上限校验。
const MaxVersionLen = 64

// Valid 判断 v 是否是升级可以使用的版本号（npm 的版本号，不带 v，不超过 MaxVersionLen）。
func Valid(v string) bool {
	return len(v) <= MaxVersionLen && versionRe.MatchString(v) && !strings.Contains(v, "..")
}

// Normalize 把 Runner 上报的版本（buildinfo，发布构建为 tag 名，如 v0.1.41）转换成 npm 的版本号（0.1.41）；
// 不是合法版本（例如开发构建 dev-abc123）时返回空。
func Normalize(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !Valid(v) {
		return ""
	}
	return v
}

// Prerelease 判断合法版本 v 是否是预发布版本。
func Prerelease(v string) bool { return strings.Contains(v, "-") }

// Compare 比较两个合法版本（semver 规则：预发布低于对应的正式版本）。
func Compare(a, b string) int {
	va, _ := semver.Parse(a)
	vb, _ := semver.Parse(b)
	return va.Compare(vb)
}

// Newer 判断 latest 是否比 Runner 上报的版本 current 高，可以升级（只升不降）。current 不是合法版本（开发构建）时
// 总是可以升级到合法的 latest：管理员点击升级就是明确的意图。
func Newer(latest, current string) bool {
	if !Valid(latest) {
		return false
	}
	cur := Normalize(current)
	return cur == "" || Compare(latest, cur) > 0
}

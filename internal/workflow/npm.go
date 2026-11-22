package workflow

import (
	"fmt"
	"regexp"

	"github.com/hewenyu/kilnflow.dev/internal/npm"
)

// CheckNpmName 检查 release.npm[].name（不含 scope 的包名，PLAN #27.3）。规则由 internal/npm 统一提供：
// 只含小写字母、数字和 - . _，不以 . 或 _ 开头，不是 npm 保留的名字；加 scope 后的长度在发布时检查。
func CheckNpmName(name string) error {
	if err := npm.ValidShortName(name); err != nil {
		return fmt.Errorf("包名 %q 不合法：%w", name, err)
	}
	return nil
}

// npmTagRe 是 dist-tag：URL 安全；不能像版本号或范围（以数字、x、*，或 v 加这些开头，如 1、v1、x、vx、x.1），
// 否则 npm 会把它当成版本范围。
var (
	npmTagRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
	npmVersionRe = regexp.MustCompile(`^[vV]?([0-9]|[xX*](\.|$))`)
)

// CheckNpmDistTag 检查 release.npm[].dist-tag（auto 之外的值）。
func CheckNpmDistTag(tag string) error {
	if !npmTagRe.MatchString(tag) || npmVersionRe.MatchString(tag) {
		return fmt.Errorf("%q 不是合法的 dist-tag（以字母开头，只含字母、数字和 - . _，不能像版本号）", tag)
	}
	return nil
}

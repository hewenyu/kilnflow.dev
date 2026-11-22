package autotag

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
)

// 版本号计算（PLAN #29.4）：纯函数，便于表驱动和属性测试。

// PR 上决定版本号的 label（PLAN #29.3）。
const (
	LabelMinor = "release:minor"
	LabelMajor = "release:major"
	LabelSkip  = "release:skip"
)

// Bump 是版本号递增的位。
type Bump int

const (
	BumpPatch Bump = iota
	BumpMinor
	BumpMajor
)

func (b Bump) String() string {
	switch b {
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	}
	return "patch"
}

// BumpFor 按 PR 的 label 决定递增哪一位（同时有多个时取最大的）；有 release:skip 时 skip 为真（优先于其他 label）。
func BumpFor(labels []string) (b Bump, skip bool) {
	if slices.Contains(labels, LabelSkip) {
		return BumpPatch, true
	}
	switch {
	case slices.Contains(labels, LabelMajor):
		return BumpMajor, false
	case slices.Contains(labels, LabelMinor):
		return BumpMinor, false
	}
	return BumpPatch, false
}

// version 是 X.Y.Z。
type version [3]int

func (v version) String() string {
	return strconv.Itoa(v[0]) + "." + strconv.Itoa(v[1]) + "." + strconv.Itoa(v[2])
}

func (v version) less(o version) bool { return slices.Compare(v[:], o[:]) < 0 }

func (v version) bump(b Bump) version {
	switch b {
	case BumpMajor:
		return version{v[0] + 1, 0, 0}
	case BumpMinor:
		return version{v[0], v[1] + 1, 0}
	}
	return version{v[0], v[1], v[2] + 1}
}

// parseVersion 解析 tag 名中前缀之后的部分，只接受形如 X.Y.Z 的正式版本（预发布和其他格式不参与）。
func parseVersion(prefix, name string) (version, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || !mergecfg.IsVersion(rest) {
		return version{}, false
	}
	var v version
	for i, p := range strings.Split(rest, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

// IsVersionTag 判断 name 是否是带 prefix 的正式版本 tag。
func IsVersionTag(prefix, name string) bool {
	_, ok := parseVersion(prefix, name)
	return ok
}

// Latest 返回 names 中最大的版本 tag（没有时为空）。
func Latest(prefix string, names []string) string {
	var (
		best     version
		bestName string
	)
	for _, n := range names {
		if v, ok := parseVersion(prefix, n); ok && (bestName == "" || best.less(v)) {
			best, bestName = v, n
		}
	}
	return bestName
}

// Next 返回下一个 tag 名：没有版本 tag 时是 prefix+initial，否则是最大的版本按 b 递增。initial 必须是 X.Y.Z
// （mergecfg 已经校验）。
func Next(prefix, initial string, names []string, b Bump) string {
	latest := Latest(prefix, names)
	if latest == "" {
		return prefix + initial
	}
	v, _ := parseVersion(prefix, latest)
	return prefix + v.bump(b).String()
}

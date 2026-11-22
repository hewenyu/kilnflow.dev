// Package semver 实现 Kiln 用到的简化 semver：解析、比较、npm 风格的范围匹配和取最新版本。
package semver

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// Version 是简化的 semver（major.minor.patch[-pre]）。
type Version struct {
	Major, Minor, Patch int
	Pre                 string
	Raw                 string
}

// Parse 解析版本号，允许前缀 v 和缺省的 minor/patch。
func Parse(s string) (Version, bool) {
	raw := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		if s[i] == '-' {
			pre = s[i+1:]
			if j := strings.IndexByte(pre, '+'); j >= 0 {
				pre = pre[:j]
			}
			// 1.2.3- 这样的空预发布段不是合法版本（否则会被当成正式版本）
			if pre == "" {
				return Version{}, false
			}
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Version{}, false
	}
	nums := []int{0, 0, 0}
	for i, p := range parts {
		// 只接受数字：strconv.Atoi 还接受 +1 这样带符号的写法
		if !isNumeric(p) {
			return Version{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre, Raw: raw}, true
}

// isNumeric 判断 s 非空且只含 ASCII 数字。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Compare 比较两个版本，返回 -1、0 或 1。预发布段按 semver 规范比较（见 comparePre），
// 例如 1.0.0-rc.2 < 1.0.0-rc.10；构建元数据不参与比较。
func (a Version) Compare(b Version) int {
	if c := cmp.Or(
		cmp.Compare(a.Major, b.Major),
		cmp.Compare(a.Minor, b.Minor),
		cmp.Compare(a.Patch, b.Patch),
	); c != 0 {
		return c
	}
	// 没有预发布段的版本比带预发布段的大
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre 按 semver 规范（#11）比较两个预发布段：按 . 分成标识符逐个比较，数字标识符按数值比较
// 且小于非数字标识符，非数字标识符按 ASCII 字典序比较；前面都相同时，标识符少的较小。
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		x, y := as[i], bs[i]
		xn, yn := isNumeric(x), isNumeric(y)
		var c int
		switch {
		case xn && yn:
			// 不转换成整数，避免超长的数字溢出：去掉前导 0 后先比长度，再比字典序
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			c = cmp.Or(cmp.Compare(len(x), len(y)), cmp.Compare(x, y))
		case xn:
			c = -1
		case yn:
			c = 1
		default:
			c = cmp.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// IsRelease 判断 tag 是否是不带预发布段的 semver（用于 Release 的 prerelease/latest）。
// ok 为 false 表示不是完整的 major.minor.patch；release 为 true 表示没有预发布段。
func IsRelease(tag string) (v Version, ok, release bool) {
	v, ok = Parse(tag)
	// 只数 major.minor.patch 部分的点：预发布段（-）和构建元数据（+）中也可能有点
	core := tag
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	if !ok || strings.Count(core, ".") != 2 {
		return v, false, false
	}
	return v, true, v.Pre == ""
}

// MatchRange 判断版本是否满足 npm 风格的范围（支持 =、>、>=、<、<=、^、~、x、||、空格表示且、A - B）。
// 与 npm 一致，预发布版本只在同一组条件中有某个条件写了相同 major.minor.patch 的预发布版本时才可能满足，
// 例如 >=1.2.3-rc.1 匹配 1.2.3-rc.2，不匹配 1.2.4-rc.1。
func MatchRange(rng string, v Version) bool {
	rng = strings.TrimSpace(rng)
	if rng == "" || rng == "*" || rng == "latest" {
		return v.Pre == ""
	}
	for alt := range strings.SplitSeq(rng, "||") {
		if matchAll(strings.Fields(alt), v) {
			return true
		}
	}
	return false
}

func matchAll(conds []string, v Version) bool {
	if len(conds) == 0 {
		return false
	}
	// 合并 ">= 1.2" 这种中间有空格的写法；"A - B"（含两端）转换成 ">=A <=B"，
	// 两端只写到 minor 或 major 时与 npm 相同（例如 1.2 - 2.3 即 >=1.2.0 <2.4.0）
	var merged []string
	for i := 0; i < len(conds); i++ {
		c := conds[i]
		switch {
		case i+2 < len(conds) && conds[i+1] == "-":
			merged = append(merged, ">="+c, "<="+conds[i+2])
			i += 2
			continue
		case slices.Contains(rangeOps, c) && i+1 < len(conds):
			c += conds[i+1]
			i++
		}
		merged = append(merged, c)
	}
	for _, c := range merged {
		if !matchOne(c, v) {
			return false
		}
	}
	return v.Pre == "" || slices.ContainsFunc(merged, func(c string) bool { return allowsPre(c, v) })
}

// allowsPre 判断条件 c 是否写了与 v 相同 major.minor.patch 的预发布版本（npm 允许预发布版本匹配的前提）。
func allowsPre(c string, v Version) bool {
	_, base, wild, ok := parseCond(c)
	return ok && wild == 3 && base.Pre != "" &&
		base.Major == v.Major && base.Minor == v.Minor && base.Patch == v.Patch
}

// rangeOps 是支持的比较运算符；较长的写在前面，保证 ">=" 不会被识别成 ">"。
var rangeOps = []string{">=", "<=", ">", "<", "=", "^", "~"}

func matchOne(c string, v Version) bool {
	op, base, wild, ok := parseCond(c)
	switch {
	case !ok:
		return false
	case wild == 0:
		return true
	}
	return matchOp(op, base, wild, v)
}

// parseCond 解析一个条件：运算符、版本和有效的段数 wild（第一个通配段 x、X、* 的位置）。
// wild 为 0 表示匹配任意版本。通配符只在 major.minor.patch 中识别：预发布段（-）和构建元数据（+）
// 中的 x 是普通标识符（例如 1.2.3-rc.x 是一个完整的预发布版本）。
func parseCond(c string) (op string, base Version, wild int, ok bool) {
	for _, p := range rangeOps {
		if rest, found := strings.CutPrefix(c, p); found {
			op, c = p, rest
			break
		}
	}
	core, suffix := strings.TrimPrefix(c, "v"), ""
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core, suffix = core[:i], core[i:]
	}
	parts := strings.Split(core, ".")
	wild = len(parts)
	for i, p := range parts {
		if p == "x" || p == "X" || p == "*" {
			wild = i
			break
		}
	}
	if wild == 0 {
		return op, Version{}, 0, true
	}
	// 与 npm 相同，有通配段时不能再写预发布段（1.x-rc 是无效的范围），构建元数据不参与匹配、忽略（22.x.x+build 即 22.x.x）
	if wild < len(parts) && suffix != "" {
		if suffix[0] == '-' {
			return "", Version{}, 0, false
		}
		suffix = ""
	}
	// Parse 拒绝超过 3 段的版本，所以 wild 不超过 3
	base, ok = Parse(strings.Join(parts[:wild], ".") + suffix)
	if !ok {
		return "", Version{}, 0, false
	}
	return op, base, wild, true
}

// inPrefix 判断 v 的前 wild 段是否与 base 相同；三段都写了时预发布段也要相同。
func inPrefix(base Version, wild int, v Version) bool {
	switch {
	case wild >= 3:
		return v.Compare(base) == 0
	case v.Major != base.Major:
		return false
	case wild == 2 && v.Minor != base.Minor:
		return false
	}
	return true
}

// matchOp 按 npm 的规则展开省略版本段的条件，例如 1.2 即 >=1.2.0 <1.3.0-0、<1.2 即 <1.2.0-0、>1.2 即 >=1.3.0，
// 边界上的预发布版本（1.2.0-rc、1.3.0-rc）不在范围内。
func matchOp(op string, base Version, wild int, v Version) bool {
	switch op {
	case "", "=":
		return inPrefix(base, wild, v) && v.Compare(base) >= 0
	case ">=":
		return v.Compare(base) >= 0
	case ">":
		if wild < 3 {
			return v.Compare(nextPrefix(base, wild)) >= 0
		}
		return v.Compare(base) > 0
	case "<":
		return !inPrefix(base, wild, v) && v.Compare(base) < 0
	case "<=":
		if wild < 3 {
			return inPrefix(base, wild, v) || v.Compare(base) < 0
		}
		return v.Compare(base) <= 0
	case "^":
		return matchCaret(base, wild, v)
	case "~":
		if v.Compare(base) < 0 {
			return false
		}
		if wild == 1 {
			return v.Major == base.Major
		}
		return v.Major == base.Major && v.Minor == base.Minor
	}
	return false
}

// nextPrefix 返回前 wild 段（wild 为 1 或 2）之后的第一个正式版本，例如 1 之后是 2.0.0，1.2 之后是 1.3.0。
func nextPrefix(base Version, wild int) Version {
	if wild == 1 {
		return Version{Major: base.Major + 1}
	}
	return Version{Major: base.Major, Minor: base.Minor + 1}
}

// matchCaret 实现 ^：不改变最左边的非零段。
func matchCaret(base Version, wild int, v Version) bool {
	if v.Compare(base) < 0 {
		return false
	}
	switch {
	case base.Major > 0 || wild == 1:
		return v.Major == base.Major
	case base.Minor > 0 || wild == 2:
		return v.Major == 0 && v.Minor == base.Minor
	default:
		return v.Major == 0 && v.Minor == 0 && v.Patch == base.Patch
	}
}

// Latest 返回 versions 中满足 match 的最大版本。
func Latest(versions []string, match func(Version) bool) (string, bool) {
	var best Version
	found := false
	for _, s := range versions {
		v, ok := Parse(s)
		if !ok || !match(v) {
			continue
		}
		if !found || v.Compare(best) > 0 {
			best = v
			found = true
		}
	}
	return best.Raw, found
}

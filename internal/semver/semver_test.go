package semver

import "testing"

func TestMatchRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rng, v string
		want   bool
	}{
		{">=20", "22.11.0", true},
		{">=20", "18.0.0", false},
		{"^20.1.0", "20.9.0", true},
		{"^20.1.0", "21.0.0", false},
		{"^0.2.3", "0.2.9", true},
		{"^0.2.3", "0.3.0", false},
		{"~20.1.0", "20.1.9", true},
		{"~20.1.0", "20.2.0", false},
		{"20", "20.3.1", true},
		{"20.x", "21.0.0", false},
		{">=18 <21", "20.0.0", true},
		{">=18 <21", "21.0.0", false},
		{">= 18", "18.0.0", true},
		{">20", "20.9.0", false},
		{"<=20", "20.9.0", true},
		{"^18 || ^20", "20.1.0", true},
		{"*", "1.0.0", true},
		{"22", "22.0.0-rc.1", false},
	}
	for _, c := range cases {
		t.Run(c.rng+"@"+c.v, func(t *testing.T) {
			t.Parallel()
			v, ok := Parse(c.v)
			if !ok {
				t.Fatalf("无法解析 %s", c.v)
			}
			if got := MatchRange(c.rng, v); got != c.want {
				t.Errorf("MatchRange(%q, %s) = %v，期望 %v", c.rng, c.v, got, c.want)
			}
		})
	}
}

func TestIsRelease(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tag         string
		ok, release bool
	}{
		{"v1.2.3", true, true},
		{"1.2.3", true, true},
		{"v1.2.3-rc.1", true, false},
		{"v1.2", false, false},
		{"release-1", false, false},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			t.Parallel()
			_, ok, rel := IsRelease(c.tag)
			if ok != c.ok || rel != c.release {
				t.Errorf("IsRelease(%q) = %v, %v，期望 %v, %v", c.tag, ok, rel, c.ok, c.release)
			}
		})
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want Version
		ok   bool
	}{
		{name: "完整版本", in: "1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3, Raw: "1.2.3"}, ok: true},
		{name: "前缀 v", in: "v1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3, Raw: "v1.2.3"}, ok: true},
		{name: "缺省 patch", in: "1.2", want: Version{Major: 1, Minor: 2, Raw: "1.2"}, ok: true},
		{name: "只有 major", in: "20", want: Version{Major: 20, Raw: "20"}, ok: true},
		{name: "首尾空白，Raw 保留原样", in: " v1.0.0 ", want: Version{Major: 1, Raw: " v1.0.0 "}, ok: true},
		{name: "预发布段", in: "1.0.0-rc.1", want: Version{Major: 1, Pre: "rc.1", Raw: "1.0.0-rc.1"}, ok: true},
		{name: "预发布段去掉构建元数据", in: "1.0.0-rc.1+build.5", want: Version{Major: 1, Pre: "rc.1", Raw: "1.0.0-rc.1+build.5"}, ok: true},
		{name: "只有构建元数据", in: "1.0.0+build", want: Version{Major: 1, Raw: "1.0.0+build"}, ok: true},
		{name: "空串", in: "", ok: false},
		{name: "四段", in: "1.2.3.4", ok: false},
		{name: "空段", in: "1..3", ok: false},
		{name: "非数字", in: "1.x", ok: false},
		{name: "负数", in: "1.-2.3", ok: false},
		{name: "只有预发布段", in: "-rc.1", ok: false},
		{name: "前缀不是 v", in: "release-1.0.0", ok: false},
		{name: "空预发布段", in: "1.2.3-", ok: false},  // 以前被当成正式版本
		{name: "带符号的数字", in: "1.+2.3", ok: false}, // strconv.Atoi 接受 +2
		{name: "带空白的段", in: "1. 2.3", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Parse(c.in)
			if ok != c.ok {
				t.Fatalf("Parse(%q) ok = %v，期望 %v", c.in, ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("Parse(%q) = %+v，期望 %+v", c.in, got, c.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2", "1.2.0", 0},
		{"2.0.0", "1.9.9", 1},
		{"1.2.0", "1.10.0", -1},
		{"1.2.10", "1.2.9", 1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-rc.1", "1.0.0-rc.1+build", 0},
		// 预发布段按 semver 规范比较：数字标识符按数值（以前整体按字典序，rc.10 < rc.2）
		{"1.0.0-rc.10", "1.0.0-rc.2", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},      // 标识符少的较小
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1}, // 数字标识符小于非数字
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0-1", "1.0.0-01", 0},                   // 数值相同
		{"1.0.0-99999999999999999999", "1.0.0-2", 1}, // 超长数字不溢出
		// 预发布版本仍然大于更小的正式版本
		{"1.0.1-rc.1", "1.0.0", 1},
	}
	for _, c := range cases {
		t.Run(c.a+"_vs_"+c.b, func(t *testing.T) {
			t.Parallel()
			a, okA := Parse(c.a)
			b, okB := Parse(c.b)
			if !okA || !okB {
				t.Fatalf("无法解析 %q 或 %q", c.a, c.b)
			}
			if got := a.Compare(b); got != c.want {
				t.Errorf("Compare(%s, %s) = %d，期望 %d", c.a, c.b, got, c.want)
			}
			if got := b.Compare(a); got != -c.want {
				t.Errorf("Compare(%s, %s) = %d，期望 %d（应反对称）", c.b, c.a, got, -c.want)
			}
		})
	}
}

func TestIsReleaseEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tag         string
		ok, release bool
	}{
		// 回归：构建元数据中的点曾被算作版本号的段，导致 v1.2.3+build.1 不被视为正式版本
		{"v1.2.3+build.1", true, true},
		{"v1.2.3-rc.1+build.1", true, false},
		{"1.2.3-rc.1.2", true, false},
		{"v1", false, false},
		{"1.2.3.4", false, false},
		{"", false, false},
		{"vX.Y.Z", false, false},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			t.Parallel()
			_, ok, rel := IsRelease(c.tag)
			if ok != c.ok || rel != c.release {
				t.Errorf("IsRelease(%q) = %v, %v，期望 %v, %v", c.tag, ok, rel, c.ok, c.release)
			}
		})
	}
}

// TestMatchRangeOperators 覆盖每个运算符在完整版本和带通配段时的语义，以及无效写法。
func TestMatchRangeOperators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, rng, v string
		want         bool
	}{
		// 空范围、* 和 latest 只匹配正式版本
		{"空范围匹配正式版本", "", "1.0.0", true},
		{"空范围不匹配预发布", "", "1.0.0-rc.1", false},
		{"latest 匹配正式版本", "latest", "3.1.4", true},
		{"latest 不匹配预发布", "latest", "3.1.4-beta", false},
		{"星号不匹配预发布", "*", "1.0.0-rc.1", false},

		// = 与省略运算符：按前缀匹配
		{"= 完整版本相等", "=20.1.0", "20.1.0", true},
		{"= 完整版本不等", "=20.1.0", "20.1.1", false},
		{"省略运算符的 minor 前缀", "20.1", "20.1.7", true},
		{"省略运算符的 minor 前缀不同", "20.1", "20.2.0", false},
		{"带 v 前缀的范围", "v20", "20.0.1", true},

		// 通配段
		{"单独的 x 匹配任意", "x", "99.0.0", true},
		{"大写 X", "1.X", "1.5.0", true},
		{"星号通配 major 不同", "1.*", "2.0.0", false},
		{"多个通配段", "1.x.x", "1.9.9", true},
		{"通配之后的段被忽略", "1.x.3", "1.5.0", true},

		// < 与 <=
		{"< 小于", "<20", "19.9.9", true},
		{"< 等于边界", "<20", "20.0.0", false},
		{"<= 完整版本等于", "<=20.1.0", "20.1.0", true},
		{"<= 完整版本大于", "<=20.1.0", "20.1.1", false},
		{"<= minor 前缀内", "<=20.1", "20.1.9", true},
		{"<= minor 前缀之外", "<=20.1", "20.2.0", false},

		// > 与 >=
		{"> 完整版本", ">20.1.0", "20.1.1", true},
		{"> 完整版本等于", ">20.1.0", "20.1.0", false},
		{"> minor 前缀内不算大于", ">20.1", "20.1.5", false},
		{"> minor 前缀之后", ">20.1", "20.2.0", true},
		{">= 等于", ">=1.2.3", "1.2.3", true},
		{">= 小于", ">=1.2.3", "1.2.2", false},
		{"运算符与版本之间有空格", "< 2", "1.0.0", true},

		// ~：允许 patch 变化；只写 major 时允许 minor 变化
		{"~ 只写 major", "~20", "20.5.0", true},
		{"~ 只写 major，major 不同", "~20", "21.0.0", false},
		{"~ 低于下限", "~20.1.2", "20.1.1", false},
		{"~ minor 前缀", "~1.2", "1.2.9", true},
		{"~ minor 前缀，minor 不同", "~1.2", "1.3.0", false},

		// ^：不改变最左边的非零段
		{"^ 低于下限", "^1.2.3", "1.2.2", false},
		{"^ 只写 major", "^20", "20.99.0", true},
		{"^0 允许 minor 变化", "^0", "0.9.0", true},
		{"^0 不允许 major 变化", "^0", "1.0.0", false},
		{"^0.2 只写到 minor", "^0.2", "0.2.7", true},
		{"^0.2 minor 不同", "^0.2", "0.3.0", false},
		{"^0.0 只写到 minor", "^0.0", "0.0.9", true},
		{"^0.0.3 只允许同一 patch", "^0.0.3", "0.0.3", true},
		{"^0.0.3 patch 不同", "^0.0.3", "0.0.4", false},

		// 组合
		{"且：都满足", ">=1.0.0 <2.0.0", "1.5.0", true},
		{"且：一个不满足", ">=1.0.0 <2.0.0", "2.0.0", false},
		{"且：范围内的预发布不匹配", ">=1.0.0 <2.0.0", "1.5.0-rc.1", false},
		{"或：第二个满足", "^1 || ^3", "3.2.0", true},
		{"或：都不满足", "^1 || ^3", "2.0.0", false},

		// 预发布版本：只在条件写了相同 major.minor.patch 的预发布版本时匹配（与 npm 一致）
		{"完整的预发布版本", "1.2.3-rc.1", "1.2.3-rc.1", true},
		{"完整的预发布版本不匹配其他预发布", "1.2.3-rc.1", "1.2.3-rc.2", false}, // 以前忽略预发布段
		{"完整的预发布版本不匹配正式版本", "1.2.3-rc.1", "1.2.3", false},       // 以前选中正式版本
		{"= 预发布版本", "=1.2.3-rc.1", "1.2.3-rc.1", true},
		{">= 预发布版本匹配同一版本的更新预发布", ">=1.2.3-rc.1", "1.2.3-rc.2", true},
		{">= 预发布版本按数值比较", ">=1.2.3-rc.2", "1.2.3-rc.10", true},
		{">= 预发布版本匹配正式版本", ">=1.2.3-rc.1", "1.3.0", true},
		{">= 预发布版本不匹配其他版本的预发布", ">=1.2.3-rc.1", "1.2.4-rc.1", false},
		{"^ 预发布版本", "^1.2.3-beta.2", "1.2.3-beta.4", true},
		// 预发布段中的 x 是普通标识符，不是通配符
		{"预发布段中的 x 精确匹配", "=1.2.3-rc.x", "1.2.3-rc.x", true},
		{"预发布段中的 x 不截断", "=1.2.3-rc.x", "1.2.3-rc", false}, // 以前截断成 1.2.3-rc
		{"预发布段中的 X 不匹配正式版本", "1.2.3-X", "1.2.3", false},
		{"预发布段中的 x 参与比较", ">=1.2.3-rc.x", "1.2.3-rc.y", true},
		{"预发布段中的 x 参与比较，更小", ">=1.2.3-rc.x", "1.2.3-rc.1", false},
		{"通配段之后不能有预发布段", "1.x-rc", "1.5.0", false},
		{"通配段之后不能有预发布段和构建元数据", "1.x-rc+build", "1.5.0", false},
		{"通配段之后的构建元数据被忽略", "22.x.x+build", "22.11.0", true},
		{"通配段之后的构建元数据，major 不同", "22.x.x+build", "23.1.0", false},
		{"省略段之后的构建元数据被忽略", "^1.x+build.5", "1.9.0", true},
		{"完整版本的构建元数据被忽略", "=1.2.3+build", "1.2.3", true},

		// 省略版本段时的边界与 npm 相同：<1.2 即 <1.2.0-0，>1.2 即 >=1.3.0，1.2 即 >=1.2.0 <1.3.0-0
		{"< 省略段的下边界预发布", ">=1.2.0-alpha <1.2", "1.2.0-alpha", false},
		{"> 省略段的上边界预发布", ">1.2 <1.3.0-beta", "1.3.0-alpha", false},
		{"> 省略 minor 的上边界预发布", ">1 <2.0.0-beta", "2.0.0-alpha", false},
		{"省略段的前缀不含下边界预发布", "1.2 >=1.2.0-alpha", "1.2.0-beta", false},
		{"省略段的前缀含中间的预发布", "1.2 >=1.2.5-alpha", "1.2.5-beta", true},
		{"< 省略段之下的预发布", "<1.3 >=1.2.0-rc", "1.2.0-rc.1", true},
		{"< 省略段的上边界预发布", "<1.3 >=1.3.0-rc", "1.3.0-rc.1", false},
		{"> 省略段之上的正式版本", ">1.2", "1.3.0", true},

		// A - B：含两端
		{"连字符范围内", "1.2.3 - 2.3.4", "2.0.0", true},
		{"连字符范围下限", "1.2.3 - 2.3.4", "1.2.3", true},
		{"连字符范围上限", "1.2.3 - 2.3.4", "2.3.4", true},
		{"连字符范围之外", "1.2.3 - 2.3.4", "2.3.5", false},
		{"连字符范围上限只写到 minor", "1.2 - 2.3", "2.3.9", true},
		{"连字符范围上限只写到 minor 之外", "1.2 - 2.3", "2.4.0", false},
		{"连字符范围与其他条件组合", "1 - 3 <2", "2.0.0", false},

		// 无效写法一律不匹配
		{"版本不是数字", "abc", "1.0.0", false},
		{"运算符后版本无效", ">=abc", "1.0.0", false},
		{"只有运算符", ">=", "1.0.0", false},
		{"不支持的运算符", "!1.0.0", "1.0.0", false},
		{"只有或", "||", "1.0.0", false},
		{"段数过多", "1.2.3.4", "1.2.3", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v, ok := Parse(c.v)
			if !ok {
				t.Fatalf("无法解析 %s", c.v)
			}
			if got := MatchRange(c.rng, v); got != c.want {
				t.Errorf("MatchRange(%q, %s) = %v，期望 %v", c.rng, c.v, got, c.want)
			}
		})
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()
	all := func(Version) bool { return true }
	inRange := func(rng string) func(Version) bool {
		return func(v Version) bool { return MatchRange(rng, v) }
	}
	cases := []struct {
		name     string
		versions []string
		match    func(Version) bool
		want     string
		found    bool
	}{
		{name: "空列表", versions: nil, match: all, want: "", found: false},
		{name: "取最大，返回原始写法", versions: []string{"v1.2.0", "v1.10.0", "v1.9.3"}, match: all, want: "v1.10.0", found: true},
		{name: "跳过无法解析的", versions: []string{"nightly", "1.0.0", "latest"}, match: all, want: "1.0.0", found: true},
		{name: "全部无法解析", versions: []string{"nightly", "beta"}, match: all, want: "", found: false},
		{name: "按范围过滤", versions: []string{"18.20.0", "20.11.1", "22.1.0", "20.9.0"}, match: inRange("^20"), want: "20.11.1", found: true},
		{name: "范围排除预发布", versions: []string{"22.0.0-rc.1", "21.7.0"}, match: inRange(">=21"), want: "21.7.0", found: true},
		{name: "没有满足的", versions: []string{"1.0.0", "2.0.0"}, match: inRange(">=3"), want: "", found: false},
		{name: "相等的版本保留先出现的写法", versions: []string{"1.2", "1.2.0"}, match: all, want: "1.2", found: true},
		{name: "预发布段中的 x 精确选中", versions: []string{"1.2.3-rc", "1.2.3-rc.x"}, match: inRange("=1.2.3-rc.x"), want: "1.2.3-rc.x", found: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, found := Latest(c.versions, c.match)
			if got != c.want || found != c.found {
				t.Errorf("Latest(%v) = %q, %v，期望 %q, %v", c.versions, got, found, c.want, c.found)
			}
		})
	}
}

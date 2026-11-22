package expr

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// evalCtx 在 testCtx 的基础上加入几种不同 Go 类型的值，覆盖求值时的各个类型分支。
func evalCtx() Context {
	ctx := testCtx()
	ctx["i"] = 3 // int 和 float64 都按数字处理
	ctx["zero"] = 0
	ctx["env"] = map[string]string{"A": "1"}
	ctx["vars"] = Lenient{"CHANNEL": "stable", "FOO-BAR": "dash"}
	ctx["nested"] = map[string]any{"Name": "kiln", "list": []any{map[string]any{"k": "v"}}}
	ctx["nan"] = math.NaN()
	return ctx
}

// 真假判断：null、false、0、NaN、空字符串为假，其余（包括对象和空数组）为真。
func TestTruthy(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"!null", true},
		{"!false", true},
		{"!0", true},
		{"!zero", true},
		{"!nan", true},
		{"!''", true},
		{"!'0'", false},
		{"!'false'", false},
		{"!i", false},
		{"!-1", false},
		{"!matrix", false},
		{"!fromJSON('[]')", false},
		{"!fromJSON('{}')", false},
		{"!!'x'", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, evalCtx())
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %v，期望 %v", c.src, v, c.want)
			}
		})
	}
}

// 相等比较：同类型直接比较（字符串不区分大小写），不同类型转换成数字比较。
func TestLooseEqual(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"null == null", true},
		{"null == ''", true}, // 都转换成 0
		{"null == false", true},
		{"true == true", true},
		{"true == false", false},
		{"true == 1", true},
		{"false == 0", true},
		{"'' == 0", true},
		{"' 12 ' == 12", true},
		{"'0x10' == 16", true},
		{"'abc' == 0", false}, // 'abc' 转换成 NaN，与任何数都不相等
		{"'true' == true", false},
		{"i == 3", true},
		{"i == '3'", true},
		{"i == matrix.n", true},
		{"zero == false", true},
		{"nan == nan", false},
		{"'Linux' == matrix.os", true},
		{"'a' != 'A'", false},
		{"1 != 2", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, evalCtx())
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %v，期望 %v", c.src, v, c.want)
			}
		})
	}
}

// 大小比较：两边都是字符串时按不区分大小写的字典序，否则转换成数字；
// 有 NaN 或对象参与时所有比较都为假。
func TestCompare(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 2", true},
		{"2 >= 3", false},
		{"null < 1", true},
		{"true > false", true},
		{"'10' > 9", true},     // 字符串和数字：按数字比较
		{"'10' < '9'", true},   // 两个字符串：按字典序比较
		{"'b' >= 'B'", true},   // 不区分大小写
		{"'b' <= 'B'", true},   // 不区分大小写
		{"'B' > 'a'", true},    // 不区分大小写，B 在 a 后面
		{"'a' < 1", false},     // NaN
		{"'a' >= 1", false},    // NaN
		{"nan <= nan", false},  // NaN
		{"matrix < 1", false},  // 对象
		{"matrix >= 0", false}, // 对象
		{"i > 2", true},        // int
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, evalCtx())
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %v，期望 %v", c.src, v, c.want)
			}
		})
	}
}

// 运算符优先级和 && || 的返回值：返回决定结果的那个操作数，而不是布尔值。
func TestOperatorPrecedenceAndShortCircuit(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"1 == 1 && 2 < 1 || 'x'", "x"},
		{"'a' || 'b' && ''", "a"},
		{"!1 == false", "true"},
		{"1 < 2 == true", "true"},
		{"0 && 'x'", "0"},
		{"'' || 0", "0"},
		{"null || 'd'", "d"},
		{"(1 || 0) && 'y'", "y"},
		// 短路：被跳过的一侧即使求值会出错也不报错
		{"false && github.event", "false"},
		{"true || github.event", "true"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, evalCtx())
			if err != nil {
				t.Fatal(err)
			}
			if got := ToString(v); got != c.want {
				t.Errorf("%s = %q，期望 %q", c.src, got, c.want)
			}
		})
	}
}

// 属性访问和下标：map 不区分大小写，访问不存在的属性或越界下标得到 null。
func TestPropertyAccess(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"matrix.OS", "linux"}, // map[string]any 的键不区分大小写
		{"MATRIX.os", "linux"}, // 上下文名不区分大小写
		{"nested.name", "kiln"},
		{"nested.list[0].k", "v"},
		{"nested['list'][0]['K']", "v"},
		{"matrix.missing", nil},
		{"matrix.os.length", nil},            // 字符串没有属性
		{"fromJSON('null').a", nil},          // null 的属性仍是 null
		{"fromJSON('null').a.b", nil},        // 链式访问 null
		{"env.A", "1"},                       // map[string]string
		{"env.B", nil},                       // map[string]string 中不存在的键
		{"matrix[1]", nil},                   // 对象用数字下标：按属性名 "1" 查找
		{"matrix.list['0']", "a"},            // 数组下标可以是数字字符串
		{"matrix.list[0.9]", "a"},            // 下标截断为整数
		{"matrix.list[2]", nil},              // 越界
		{"matrix.list['x']", nil},            // NaN 下标
		{"vars.FOO-BAR", "dash"},             // 名字中可以有 -
		{"vars['NOPE']", ""},                 // Lenient：不存在的字段是空字符串
		{"fromJSON('[1,[2,3]]')[1][1]", 3.0}, // 嵌套数组
		// 只差大小写的多个键：精确匹配优先，否则取字典序最小的键（不随 map 遍历顺序变化）
		{`fromJSON('{"Ab":1,"aB":2,"AB":3}').ab`, 3.0},
		{`fromJSON('{"Ab":1,"aB":2,"AB":3}').aB`, 2.0},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, evalCtx())
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %#v，期望 %#v", c.src, v, c.want)
			}
		})
	}
}

// 求值错误要传播到最外层，并且统一包装成带原始表达式的 *Error。
func TestEvalErrorPropagation(t *testing.T) {
	cases := []string{
		"!github.event",
		"github.event == 1",
		"1 == github.event",
		"1 < github.event",
		"true && github.event",
		"false || github.event",
		"github.event[0]",
		"matrix[github.event]",
		"contains(github.event, 'x')",
		"format('{0}', unknown)",
		"github.REF", // Strict 的字段区分大小写
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := EvalString(src, evalCtx())
			if err == nil {
				t.Fatal("期望出错")
			}
			e, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("错误应为 *Error：%T %v", err, err)
			}
			if e.Expr != src {
				t.Errorf("Error.Expr = %q，期望 %q", e.Expr, src)
			}
			if !strings.Contains(err.Error(), src) {
				t.Errorf("错误信息 %q 中应包含表达式 %q", err, src)
			}
		})
	}
}

func TestToString(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"null", nil, ""},
		{"true", true, "true"},
		{"false", false, "false"},
		{"整数", 42.0, "42"},
		{"负整数", -7.0, "-7"},
		{"小数", 1.5, "1.5"},
		{"很大的整数", 1e15, "1000000000000000"},
		{"很大的数", 1e21, "1000000000000000000000"},
		{"NaN", math.NaN(), "NaN"},
		{"int", 9, "9"},
		{"字符串", "s", "s"},
		{"数组", []any{"a", 1.0, nil}, `["a",1,null]`},
		{"map", map[string]any{"b": 1.0, "a": "x"}, `{"a":"x","b":1}`},
		{"map[string]string", map[string]string{"k": "v"}, `{"k":"v"}`},
		{"Strict", Strict{Name: "s", Fields: map[string]any{"x": Lenient{"y": "z"}}}, `{"x":{"y":"z"}}`},
		{"Lenient", Lenient{"a": "b"}, `{"a":"b"}`},
		{"没有 JSON 方法的对象", &Placeholder{Prefix: "secrets"}, `{}`},
		{"嵌套数组中的对象", []any{Lenient{"a": "b"}}, `[{"a":"b"}]`},
		{"不转义 HTML 字符", map[string]any{"a": "<b>&"}, `{"a":"<b>&"}`},
		{"无法序列化", make(chan int), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ToString(c.v); got != c.want {
				t.Errorf("ToString(%#v) = %q，期望 %q", c.v, got, c.want)
			}
		})
	}
}

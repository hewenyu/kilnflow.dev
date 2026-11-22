package expr

import (
	"errors"
	"strings"
	"testing"
)

// 字面量的词法：数字的各种写法、字符串转义和关键字。
func TestLiterals(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"1e3", 1000.0},
		{"1E-3", 0.001},
		{"2.5e+1", 25.0},
		{"-0x10", -16.0},
		{"0XfF", 255.0},
		{"-.5", -0.5},
		{"(.5)", 0.5},
		{"1 < .5", false}, // 运算符后面的 . 是数字的开头
		{"contains('x', .5)", false},
		{"''", ""},
		{"'a''''b'", "a''b"},
		{"'  sp  '", "  sp  "},
		{"true", true},
		{"false", false},
		{"null", nil},
		{"\t1\r\n==\n1 ", true}, // 各种空白
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %#v，期望 %#v", c.src, v, c.want)
			}
		})
	}
}

// 语法错误：每种错误都给出可读的原因，并且是带原始表达式的 *Error。
func TestParseErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"", "表达式不完整"},
		{"   ", "表达式不完整"},
		{"!", "表达式不完整"},
		{"1 ==", "表达式不完整"},
		{"a &&", "表达式不完整"},
		{"!(", "表达式不完整"},
		{"a[", "表达式不完整"},
		{"1 2", "表达式末尾有多余内容"},
		{"a b", "表达式末尾有多余内容"},
		{"(1))", "表达式末尾有多余内容"},
		{")", "意外的符号"},
		{"]", "意外的符号"},
		{",", "意外的符号"},
		{"f(1,)", "意外的符号"},
		{"(1 ]", "缺少 ')'"},
		{"a[1", "缺少 ']'"},
		{"a[1 2]", "缺少 ']'"},
		{"contains('a', 'b'", "函数 contains 的参数缺少 ')'"},
		{"contains(1 2)", "函数 contains 的参数缺少 ')'"},
		{"a.", "'.' 后面应为属性名"},
		{"a.1", "'.' 后面应为属性名"},
		{"a.'b'", "'.' 后面应为属性名"},
		{"a = b", "无法识别的字符 \"=\""},
		{"${{ a }}", "无法识别的字符 \"$\""},
		{"a & b", "无法识别的字符 \"&\""},
		{"a | b", "无法识别的字符 \"|\""},
		{"\"x\"", "无法识别的字符 \"\\\"\""},
		{"1.2.3", "无效的数字 \"1.2.3\""},
		{"0xZZ", "无效的数字 \"0xZZ\""},
		{"-0xG", "无效的数字 \"-0xG\""},
		{"1abc", "无效的数字 \"1abc\""},
		{"-", "无效的数字 \"-\""},
		{"'abc''", "字符串没有结束"},
		{"matrix.*", "不支持对象过滤（*）"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := Parse(c.src)
			if err == nil {
				t.Fatal("期望出错")
			}
			e, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("错误应为 *Error：%T", err)
			}
			if e.Expr != c.src {
				t.Errorf("Error.Expr = %q，期望 %q", e.Expr, c.src)
			}
			if !strings.Contains(e.Msg, c.want) {
				t.Errorf("错误 %q 中应包含 %q", e.Msg, c.want)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Expr: "a ==", Msg: "表达式不完整"}
	if got, want := e.Error(), `表达式 "a =="：表达式不完整`; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
}

// 深层嵌套的表达式报错，而不是递归到耗尽栈空间。
func TestParseNestingLimit(t *testing.T) {
	t.Parallel()
	ok := strings.Repeat("(", maxDepth-2) + "1" + strings.Repeat(")", maxDepth-2)
	if _, err := Parse(ok); err != nil {
		t.Errorf("%d 层括号应能解析：%v", maxDepth-2, err)
	}
	// 层数超限而长度不超过 maxLength
	const n = 2000
	for name, src := range map[string]string{
		"括号":   strings.Repeat("(", n) + "1" + strings.Repeat(")", n),
		"!":    strings.Repeat("!", n) + "true",
		"下标":   "a" + strings.Repeat("[a", n) + strings.Repeat("]", n),
		"函数参数": strings.Repeat("join(", n) + "1" + strings.Repeat(")", n),
	} {
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), "嵌套") {
			t.Errorf("%s：err = %v，期望嵌套层数超限", name, err)
		}
	}
}

// 超长的表达式报错。属性访问链、二元运算链不计入嵌套层数，但语法树的深度与长度成正比：
// 修复前几 MB 的 a.b.b… 在求值时耗尽栈空间，进程崩溃。
func TestParseLengthLimit(t *testing.T) {
	t.Parallel()
	ctx := Context{"a": map[string]any{}}
	// 恰好 maxLength 的长链可以解析和求值
	chain := "a" + strings.Repeat(".b", (maxLength-1)/2)
	if len(chain) > maxLength {
		t.Fatalf("测试数据长度 %d 超过上限", len(chain))
	}
	if v, err := EvalString(chain, ctx); err != nil || v != nil {
		t.Errorf("长度 %d 的属性链：v = %v，err = %v", len(chain), v, err)
	}
	or := strings.Repeat("a || ", (maxLength-1)/5) + "a"
	if _, err := StringRefs("${{ " + or + " }}"); err != nil {
		t.Errorf("长度 %d 的 || 链：%v", len(or), err)
	}
	for name, src := range map[string]string{
		"属性链": "a" + strings.Repeat(".b", 5_000_000),
		"||":  strings.Repeat("a||", 3_000_000) + "a",
		"刚超过": strings.Repeat("1", maxLength+1),
	} {
		for what, run := range map[string]func() error{
			"EvalString": func() error { _, err := EvalString(src, ctx); return err },
			"StringRefs": func() error { _, err := StringRefs("${{ " + src + " }}"); return err },
		} {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "超过 21000 字节") {
				t.Errorf("%s %s：err = %v，期望长度超限", name, what, err)
			}
			// 错误信息中不带整个表达式
			if err != nil && len(err.Error()) > 200 {
				t.Errorf("%s %s：错误信息过长（%d 字节）", name, what, len(err.Error()))
			}
		}
	}
}

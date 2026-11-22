package expr

import (
	"reflect"
	"strings"
	"testing"
)

func TestInterpolateCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"没有表达式时原样返回", "plain text }} {{", "plain text }} {{"},
		{"只有表达式", "${{ matrix.os }}", "linux"},
		{"相邻的两个表达式", "${{ matrix.os }}${{ matrix.n }}", "linux3"},
		{"表达式两侧的空白不影响结果", "${{matrix.os}}/${{   matrix.n   }}", "linux/3"},
		{"null 插值为空字符串", "[${{ null }}]", "[]"},
		{"数组插值为 JSON", "${{ matrix.list }}", `["a","b"]`},
		{"字符串字面量中的 }} 不结束表达式", "${{ format('{0}}}', 'x') }}", "x}"},
		{"空表达式插值为空", "a${{ }}b", "ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Interpolate(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Interpolate(%q) = %q，期望 %q", c.src, got, c.want)
			}
		})
	}
}

func TestInterpolateErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"缺少结束符", "a ${{ matrix.os", "缺少 '}}'"},
		{"字符串没有结束导致找不到结束符", "${{ 'abc }}", "缺少 '}}'"},
		{"语法错误", "${{ 1 2 }}", "表达式末尾有多余内容"},
		{"求值错误", "x ${{ github.event }}", "不支持的字段 github.event"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Interpolate(c.src, testCtx())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Interpolate(%q) 的错误 = %v，期望包含 %q", c.src, err, c.want)
			}
		})
	}
}

// 整个字符串只是一个 ${{ }} 时保留原始类型，其他情况与 Interpolate 一样得到字符串。
func TestInterpolateValue(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any
	}{
		{"数组保留类型", "${{ matrix.list }}", []any{"a", "b"}},
		{"数字保留类型", " ${{ matrix.n }} ", 3.0},
		{"fromJSON 的结果保留类型", "${{ fromJSON('[1]') }}", []any{1.0}},
		{"布尔值保留类型", "${{ 1 == 1 }}", true},
		{"混合文本得到字符串", "a${{ matrix.n }}", "a3"},
		{"多个表达式得到字符串", "${{ matrix.n }}${{ matrix.n }}", "33"},
		{"没有表达式时原样返回", "x", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := InterpolateValue(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("InterpolateValue(%q) = %#v，期望 %#v", c.src, got, c.want)
			}
		})
	}

	for _, src := range []string{"${{ 'a }}", "${{ github.event }}", "x${{ github.event }}"} {
		t.Run("出错/"+src, func(t *testing.T) {
			if _, err := InterpolateValue(src, testCtx()); err == nil {
				t.Errorf("InterpolateValue(%q) 应出错", src)
			}
		})
	}
}

// if: 的求值：空条件为真，可以写成 ${{ }} 也可以直接写表达式，文本和表达式混合时按插值结果是否为空判断。
func TestEvalConditionCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"空条件为真", "", true},
		{"只有空白为真", "  \n", true},
		{"直接写表达式", "matrix.n == 3", true},
		{"包在 ${{ }} 中", "${{ matrix.n == 4 }}", false},
		{"两侧有空白", "  ${{ matrix.os == 'linux' }}  ", true},
		{"结果按真假转换", "${{ matrix.os }}", true},
		{"空字符串为假", "${{ '' }}", false},
		{"0 为假", "0", false},
		{"多个表达式拼接后非空为真", "${{ 'a' }}-${{ 'b' }}", true},
		{"多个表达式拼接后为空为假", "${{ '' }}${{ null }}", false},
		{"多个表达式拼接后是 false 字符串仍为真", "${{ false }}${{ '' }}", true},
		{"空的 ${{ }} 为假", "${{ }}", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EvalCondition(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("EvalCondition(%q) = %v，期望 %v", c.src, got, c.want)
			}
		})
	}
}

func TestEvalConditionErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"缺少结束符", "${{ 'a }}", "缺少 '}}'"},
		{"单个表达式求值出错", "${{ github.event }}", "不支持的字段"},
		{"混合时求值出错", "${{ github.event }}-${{ 1 }}", "不支持的字段"},
		{"直接写的表达式语法错误", "a ==", "表达式不完整"},
		{"状态函数", "${{ always() }}", "不支持状态函数"},
		{"文本开头的条件不是表达式", "x ${{ true }}", "无法识别的字符"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := EvalCondition(c.src, testCtx())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("EvalCondition(%q) 的错误 = %v，期望包含 %q", c.src, err, c.want)
			}
		})
	}
}

func TestHasExpr(t *testing.T) {
	for s, want := range map[string]bool{"": false, "a {{ b }}": false, "${{ a }}": true, "x${{": true} {
		if got := HasExpr(s); got != want {
			t.Errorf("HasExpr(%q) = %v，期望 %v", s, got, want)
		}
	}
}

package expr

import (
	"math"
	"strings"
	"testing"
)

// 内置函数：函数名不区分大小写，字符串比较不区分大小写。
func TestFuncs(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		// contains
		{"contains(fromJSON('[1,2]'), '2')", "true"}, // 数组元素按 == 的规则比较
		{"contains(fromJSON('[1,2]'), 3)", "false"},
		{"contains(fromJSON('[]'), '')", "false"},
		{"contains('Hello', '')", "true"},
		{"contains(null, 'a')", "false"},
		{"contains(123, 2)", "true"}, // 非数组按字符串处理
		{"CONTAINS('abc', 'B')", "true"},
		// 对象不按 JSON 文本查找子串（与 Actions 一致）
		{"contains(fromJSON('{\"x\":1}'), 'x')", "false"},
		{"contains('[1]', fromJSON('[1]'))", "false"},
		// startsWith / endsWith
		{"startsWith('Refs/Tags/v1', 'refs/tags/')", "true"},
		{"startsWith('abc', 'b')", "false"},
		{"startsWith('abc', '')", "true"},
		{"endsWith('v1.2.3', '.3')", "true"},
		{"endsWith('v1.2.3', '1')", "false"},
		{"StartsWith(1.5, 1)", "true"}, // 数字按字符串形式比较
		{"startsWith(fromJSON('{\"a\":1}'), '{')", "false"},
		{"endsWith(fromJSON('[1]'), ']')", "false"},
		{"startsWith('{}', fromJSON('{}'))", "false"},
		// format
		{"format('abc')", "abc"},
		{"format('{0}{0}', 'x')", "xx"},
		{"format('{1}/{0}', 'a', 'b')", "b/a"},
		{"format('{{0}}', 'x')", "{0}"},
		{"format('a}b', 'x')", "a}b"},  // 单独的 } 原样输出
		{"format('a}}b', 'x')", "a}b"}, // }} 是转义
		{"format('{0}', null)", ""},
		{"format('{0}', true)", "true"},
		{"format('{0}', fromJSON('[1]'))", "[1]"},
		// join
		{"join(fromJSON('[]'))", ""},
		{"join(fromJSON('[\"a\", 1, true, null]'))", "a,1,true,"},
		{"join(fromJSON('[\"a\",\"b\"]'), '')", "ab"},
		{"join('abc', '-')", "abc"}, // 不是数组时返回它本身
		{"join(null)", ""},
		{"join(fromJSON('[[1,2]]'), ';')", "[1,2]"},
		{"join(fromJSON('{\"a\":1}'))", ""}, // 对象（不是数组）为空字符串
		// toJSON
		{"toJSON('x')", `"x"`},
		{"toJSON(1)", "1"},
		{"toJSON(null)", "null"},
		{"toJSON(fromJSON('{\"b\":1,\"a\":[true]}'))", "{\n  \"a\": [\n    true\n  ],\n  \"b\": 1\n}"},
		{"toJSON(github)", "{\n  \"event_name\": \"push\",\n  \"ref\": \"refs/heads/main\",\n  \"ref_name\": \"main\",\n  \"sha\": \"abc123\"\n}"},
		{"toJSON(vars)", "{\n  \"CHANNEL\": \"stable\"\n}"},
		// 与 Actions 一致，<、>、& 原样输出，不转义成 \u003c 等形式
		{"toJSON('a<b>&c')", `"a<b>&c"`},
		{"toJSON(fromJSON('[\"<x>\"]'))", "[\n  \"<x>\"\n]"},
		// fromJSON
		{"fromJSON('true')", "true"},
		{"fromJSON('1.5') > 1", "true"},
		{"fromJSON('\"s\"')", "s"},
		{"fromJSON(' {\"a\":null} ').a", ""},
		{"fromJSON(toJSON(matrix.list))[0]", "a"}, // toJSON 和 fromJSON 互逆
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if got := ToString(v); got != c.want {
				t.Errorf("%s = %q，期望 %q", c.src, got, c.want)
			}
		})
	}
}

func TestFuncErrors(t *testing.T) {
	ctx := testCtx()
	ctx["nan"] = math.NaN()
	cases := []struct {
		src  string
		want string
	}{
		{"contains('a', 'b', 'c')", "函数 contains 的参数个数不正确"},
		{"startsWith('a')", "函数 startsWith 的参数个数不正确"},
		{"endsWith()", "函数 endsWith 的参数个数不正确"},
		{"format()", "函数 format 的参数个数不正确"},
		{"join()", "函数 join 的参数个数不正确"},
		{"join('a', ',', 'x')", "函数 join 的参数个数不正确"},
		{"toJSON()", "函数 toJSON 的参数个数不正确"},
		{"toJSON(1, 2)", "函数 toJSON 的参数个数不正确"},
		{"fromJSON()", "函数 fromJSON 的参数个数不正确"},
		{"toJSON(nan)", "toJSON"}, // NaN 无法序列化成 JSON
		{"fromJSON('')", "fromJSON"},
		{"fromJSON('[1,')", "fromJSON"},
		{"format('{0', 'a')", "format：缺少 '}'"},
		{"format('{a}', 'x')", "无效的占位符 \"{a}\""},
		{"format('{-1}', 'x')", "无效的占位符"},
		{"format('{}', 'x')", "无效的占位符"},
		{"format('{0}')", "无效的占位符 \"{0}\""},
		{"success()", "不支持状态函数 success()"},
		{"always()", "不支持状态函数 always()"},
		{"cancelled()", "不支持状态函数 cancelled()"},
		{"Failure()", "不支持状态函数 Failure()"},
		{"HASHFILES('**/go.sum')", "不支持 hashFiles"},
		{"lower('A')", "未知函数 lower"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := EvalString(c.src, ctx)
			if err == nil {
				t.Fatal("期望出错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 %q 中应包含 %q", err, c.want)
			}
		})
	}
}

package expr

import (
	"errors"
	"strings"
	"testing"
)

func testCtx() Context {
	return Context{
		"github": Strict{Name: "github", Fields: map[string]any{
			"event_name": "push",
			"ref":        "refs/heads/main",
			"ref_name":   "main",
			"sha":        "abc123",
		}},
		"matrix": map[string]any{"os": "linux", "n": 3.0, "list": []any{"a", "b"}},
		"vars":   Lenient{"CHANNEL": "stable"},
	}
}

func TestEval(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"github.event_name == 'push'", "true"},
		{"github.event_name == 'PUSH'", "true"},
		{"github.ref != 'refs/heads/main'", "false"},
		{"startsWith(github.ref, 'refs/heads/')", "true"},
		{"endsWith(github.ref, '/MAIN')", "true"},
		{"contains(matrix.list, 'b')", "true"},
		{"contains('hello', 'ELL')", "true"},
		{"matrix.n > 2 && matrix.os", "linux"},
		{"matrix.n < 2 || 'fallback'", "fallback"},
		{"!(matrix.n == 3)", "false"},
		{"format('{0}-{1}-{{x}}', matrix.os, matrix.n)", "linux-3-{x}"},
		{"join(matrix.list, '+')", "a+b"},
		{"join(fromJSON('[1,2]'))", "1,2"},
		{"fromJSON('{\"a\":{\"b\":5}}').a.b", "5"},
		{"matrix['os']", "linux"},
		{"matrix.list[1]", "b"},
		{"vars.CHANNEL", "stable"},
		{"vars.MISSING", ""},
		{"1 == '1'", "true"},
		{"null == 0", "true"},
		{"'it''s'", "it's"},
		{"0x10 == 16", "true"},
		{"-1.5 < 0", "true"},
		{".5 == 0.5", "true"},
		{"matrix.list[1e300]", ""},
		{"matrix.list[-1]", ""},
		{"toJSON(matrix.list)", "[\n  \"a\",\n  \"b\"\n]"},
		{"'abc' < 'ABD'", "true"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, testCtx())
			if err != nil {
				t.Fatal(err)
			}
			if got := ToString(v); got != c.want {
				t.Errorf("%s = %q, want %q", c.src, got, c.want)
			}
		})
	}
}

// 对象和数组只有是同一个实例时才相等，内容相同的不同实例不相等。
func TestObjectEquality(t *testing.T) {
	ctx := testCtx()
	ctx["github2"] = Strict{Name: "github", Fields: map[string]any{
		"event_name": "push", "ref": "refs/heads/main", "ref_name": "main", "sha": "abc123",
	}}
	ctx["secrets"] = &Placeholder{Prefix: "secrets"}
	cases := []struct {
		src  string
		want bool
	}{
		{"github == github", true},
		{"github == github2", false},
		{"matrix == matrix", true},
		{"matrix.list == matrix.list", true},
		{"vars == vars", true},
		{"secrets == secrets", true},
		{"fromJSON('{}') == fromJSON('{}')", false},
		{"fromJSON('[1]') == fromJSON('[1]')", false},
		{"matrix == vars", false},
		{"matrix.list == matrix", false},
		{"github != github2", true},
		{"matrix == 'linux'", false},
		{"matrix.list == 0", false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			v, err := EvalString(c.src, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if v != c.want {
				t.Errorf("%s = %v, want %v", c.src, v, c.want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string // 错误信息中应包含的内容
	}{
		{"github.event", "不支持的字段 github.event"},
		{"steps.x.outputs.y", "上下文 steps 在此处不可用"},
		{"failure()", "不支持状态函数"},
		{"hashFiles('x')", "不支持 hashFiles"},
		{"(1", "缺少 ')'"},
		{"'abc", "字符串没有结束"},
		{"fromJSON('{')", "fromJSON"},
		{"contains('a')", "参数个数不正确"},
		{"format('{1}', 'a')", "无效的占位符"},
		{"a.*", "不支持对象过滤"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, err := EvalString(c.src, testCtx())
			if err == nil {
				t.Fatal("期望出错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 %q 中应包含 %q", err, c.want)
			}
			if _, ok := errors.AsType[*Error](err); !ok {
				t.Errorf("错误应为 *Error：%T", err)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"github.event_name == 'push'",
		"!(matrix.n == 3) && contains(matrix.list, 'b')",
		"format('{0}-{{x}}', matrix.os)",
		"fromJSON('[1,2]')[0]",
		"-0x1F < .5e3",
		"a['b'].c[1]",
		"'it''s'",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		n, err := Parse(src)
		if err != nil {
			return
		}
		// 能解析的表达式求值和引用分析都不应 panic
		_, _ = eval(n, testCtx())
		_ = refs(n)
	})
}

func TestInterpolate(t *testing.T) {
	ctx := testCtx()
	sec := &Placeholder{Prefix: "secrets", Used: map[string]bool{}}
	ctx["secrets"] = sec
	got, err := Interpolate("ref=${{ github.ref_name }} os=${{matrix.os}} tok=${{ secrets.NPM }} x='}}'", ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "ref=main os=linux tok=${{ secrets.NPM }} x='}}'"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !sec.Used["NPM"] {
		t.Fatal("secret 没有记录")
	}
	ok, err := EvalCondition("${{ github.event_name == 'push' }}", ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, err = EvalCondition("github.event_name == 'pull_request'", ctx)
	if err != nil || ok {
		t.Fatal(ok, err)
	}
}

func TestRefs(t *testing.T) {
	refs, err := StringRefs("${{ secrets.A }} ${{ format('{0}', matrix['os']) }} ${{ github.sha }}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(refs, ",") != "secrets.A,matrix.os,github.sha" {
		t.Fatal(refs)
	}
}

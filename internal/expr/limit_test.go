package expr

import (
	"errors"
	"strings"
	"testing"
)

// 嵌套 format 每层把结果翻倍：修复前 30 层会分配 GB 级内存，现在在构造过程中就以上限错误失败。
func TestFormatResultLimit(t *testing.T) {
	t.Parallel()
	const depth = 30
	src := strings.Repeat("format('{0}{0}', ", depth) + "'x'" + strings.Repeat(")", depth)
	_, err := EvalString(src, Context{})
	if err == nil {
		t.Fatal("嵌套 format 超过上限应报错")
	}
	if _, ok := errors.AsType[*Error](err); !ok {
		t.Fatalf("期望 *Error，实际 %T：%v", err, err)
	}
	if !strings.Contains(err.Error(), "字节上限") {
		t.Errorf("错误信息 = %q", err)
	}

	// 低于上限的结果仍然正常
	ok := strings.Repeat("format('{0}{0}', ", 5) + "'ab'" + strings.Repeat(")", 5)
	v, err := EvalString(ok, Context{})
	if err != nil {
		t.Fatalf("低于上限：%v", err)
	}
	if len(ToString(v)) != 2<<5 {
		t.Errorf("结果长度 = %d，期望 %d", len(ToString(v)), 2<<5)
	}
}

func TestJoinResultLimit(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", maxResultBytes/2+1)
	_, err := EvalString("join(x)", Context{"x": []any{big, big}})
	if err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("join 超过上限：err = %v", err)
	}
	if _, err := EvalString("join(x)", Context{"x": []any{"a", "b"}}); err != nil {
		t.Fatalf("小数组：%v", err)
	}
}

func TestToJSONResultLimit(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", maxResultBytes+1)
	if _, err := EvalString("toJSON(x)", Context{"x": big}); err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("toJSON 超过上限：err = %v", err)
	}
}

func TestInterpolateResultLimit(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", maxResultBytes)
	_, err := Interpolate("${{ x }}${{ x }}", Context{"x": big})
	if err == nil {
		t.Fatal("插值超过上限应报错")
	}
	if _, ok := errors.AsType[*Error](err); !ok {
		t.Fatalf("期望 *Error，实际 %T", err)
	}
	if !strings.Contains(err.Error(), "字节上限") {
		t.Errorf("错误信息 = %q", err)
	}
}

// Lenient.JSON 与 Strict.JSON 一样递归 normalize：嵌套的 Object 会被展开，而不是序列化成 {}。
func TestLenientJSONNormalizesNestedObject(t *testing.T) {
	t.Parallel()
	l := Lenient{"A": Strict{Name: "inner", Fields: map[string]any{"B": 1.0}}}
	v, err := EvalString("toJSON(vars)", Context{"vars": l})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"A\": {\n    \"B\": 1\n  }\n}"; v != want {
		t.Errorf("toJSON(vars) = %q，期望 %q", v, want)
	}
}

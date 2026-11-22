package expr

import (
	"slices"
	"strings"
	"testing"
)

func TestStrict(t *testing.T) {
	s := Strict{Name: "kiln", Fields: map[string]any{"version": "1.0"}}
	if v, err := s.Get("version"); err != nil || v != "1.0" {
		t.Errorf("Get(version) = %v, %v", v, err)
	}
	_, err := s.Get("missing")
	if err == nil || err.Error() != "不支持的字段 kiln.missing" {
		t.Errorf("Get(missing) 的错误 = %v", err)
	}
}

func TestLenient(t *testing.T) {
	l := Lenient{"A": 1.0}
	if v, _ := l.Get("A"); v != 1.0 {
		t.Errorf("Get(A) = %v", v)
	}
	if v, err := l.Get("B"); err != nil || v != "" {
		t.Errorf("Get(B) = %#v, %v，期望空字符串", v, err)
	}
}

// Placeholder 不展开 secret，只记录引用并返回原样的 ${{ }}，由 Runner 执行时替换。
func TestPlaceholder(t *testing.T) {
	p := &Placeholder{Prefix: "secrets", Used: map[string]bool{}}
	got, err := Interpolate("${{ secrets.B }} ${{ secrets['A'] }} ${{ secrets.B }}", Context{"secrets": p})
	if err != nil {
		t.Fatal(err)
	}
	if want := "${{ secrets.B }} ${{ secrets.A }} ${{ secrets.B }}"; got != want {
		t.Errorf("Interpolate = %q，期望 %q", got, want)
	}
	if names := p.Names(); !slices.Equal(names, []string{"A", "B"}) {
		t.Errorf("Names() = %v，期望已排序并去重的 [A B]", names)
	}

	// Used 为 nil 时只返回占位符，不记录
	q := &Placeholder{Prefix: "secrets"}
	v, err := q.Get("X")
	if err != nil || v != "${{ secrets.X }}" {
		t.Errorf("Get(X) = %v, %v", v, err)
	}
	if names := q.Names(); len(names) != 0 {
		t.Errorf("Names() = %v，期望为空", names)
	}

	// 名字只能包含字母、数字和 _：以前 secrets['A }} B'] 生成 "${{ secrets.A }} B }}"，Runner 无法替换
	for _, src := range []string{"${{ secrets['A }} B'] }}", "${{ secrets['a b'] }}", "${{ secrets[''] }}", "${{ secrets['x.y'] }}"} {
		r := &Placeholder{Prefix: "secrets", Used: map[string]bool{}}
		_, err := Interpolate(src, Context{"secrets": r})
		if err == nil || !strings.Contains(err.Error(), "secrets 的名称") {
			t.Errorf("Interpolate(%q) 的错误 = %v，期望名称无效", src, err)
		}
		if len(r.Used) != 0 {
			t.Errorf("Interpolate(%q) 不应记录引用：%v", src, r.Used)
		}
	}
	for _, name := range []string{"NPM_TOKEN", "_x", "1A"} {
		if _, err := (&Placeholder{Prefix: "secrets"}).Get(name); err != nil {
			t.Errorf("Get(%q)：%v", name, err)
		}
	}
}

func TestStringRefsCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"没有表达式", "plain", nil},
		{"一元和二元运算", "${{ !a.b && c || d['e'] }}", []string{"a.b", "c", "d.e"}},
		{"函数参数", "${{ fromJSON(vars.X).a }}", []string{"vars.X"}},
		{"非字面量下标：先收集下标中的引用，再保留下标之前的部分", "${{ matrix[github.ref].x }}", []string{"github.ref", "matrix"}},
		{"数字下标只保留下标之前的部分", "${{ a[0].b }}", []string{"a"}},
		{"字面量没有引用", "${{ 'x' == 1 }}", nil},
		{"多段", "a ${{ x }} b ${{ y.z }}", []string{"x", "y.z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := StringRefs(c.src)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("StringRefs(%q) = %q，期望 %q", c.src, got, c.want)
			}
		})
	}

	for _, src := range []string{"${{ a", "${{ (1 }}"} {
		t.Run("出错/"+src, func(t *testing.T) {
			if _, err := StringRefs(src); err == nil {
				t.Errorf("StringRefs(%q) 应出错", src)
			}
		})
	}
}

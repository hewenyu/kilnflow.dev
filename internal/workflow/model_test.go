package workflow

import (
	"slices"
	"testing"
)

func TestDiags(t *testing.T) {
	ds := Diags{
		{File: "a.yml", Line: 3, Col: 5, Msg: "坏了"},
		{File: "a.yml", Msg: "全局", Warning: true},
		{File: "a.yml", Line: 1, Col: 1, Msg: "也坏了"},
	}
	if got := ds[0].String(); got != "a.yml:3:5: 错误：坏了" {
		t.Errorf("带位置的错误 = %q", got)
	}
	if got := ds[1].String(); got != "a.yml: 警告：全局" {
		t.Errorf("没有位置的警告 = %q", got)
	}
	if !ds.HasErrors() {
		t.Error("HasErrors 应为 true")
	}
	if e := ds.Errors(); len(e) != 2 || e[0].Msg != "坏了" || e[1].Msg != "也坏了" {
		t.Errorf("Errors() = %v", e)
	}
	if w := ds.Warnings(); len(w) != 1 || w[0].Msg != "全局" {
		t.Errorf("Warnings() = %v", w)
	}
	if got := ds.Error(); got != "a.yml:3:5: 错误：坏了\na.yml: 警告：全局\na.yml:1:1: 错误：也坏了" {
		t.Errorf("Error() = %q", got)
	}

	onlyWarnings := Diags{{File: "a.yml", Msg: "w", Warning: true}}
	if onlyWarnings.HasErrors() || onlyWarnings.Errors() != nil {
		t.Error("只有警告时不算有错误")
	}
	var none Diags
	if none.HasErrors() || none.Error() != "" {
		t.Error("空的 Diags 不应有错误")
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{"ci.yml": "ci", "release.yaml": "release", "x.json": "x.json", "a.yml.yaml": "a.yml"} {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q，期望 %q", in, got, want)
		}
	}
	wf := mustParse(t, "on: push\n"+okJob)
	if wf.Base() != "x" || wf.Name != "x" {
		t.Errorf("Base() = %q，Name = %q", wf.Base(), wf.Name)
	}
	wf = mustParse(t, "name: CI\non: push\n"+okJob)
	if wf.Base() != "x" || wf.Name != "CI" {
		t.Errorf("写了 name 时 Base() = %q，Name = %q", wf.Base(), wf.Name)
	}
}

func TestFetchSplit(t *testing.T) {
	cases := []struct{ from, job, out string }{
		{"build.bin", "build", "bin"},
		{"build", "build", ""},
		{"a.b.c", "a.b", "c"},
	}
	for _, c := range cases {
		job, out := Fetch{From: c.from}.Split()
		if job != c.job || out != c.out {
			t.Errorf("Split(%q) = %q, %q，期望 %q, %q", c.from, job, out, c.job, c.out)
		}
	}
}

// OrderedMap 保留插入顺序，零值和 nil 都可以安全使用。
func TestOrderedMap(t *testing.T) {
	var zero OrderedMap
	zero.Set("b", "1")
	zero.Set("a", "2")
	zero.Set("b", "3") // 已有的键不改变顺序
	var keys []string
	zero.Each(func(k, v string) { keys = append(keys, k+"="+v) })
	if !slices.Equal(keys, []string{"b=3", "a=2"}) || zero.Len() != 2 {
		t.Errorf("遍历结果 = %v，Len = %d", keys, zero.Len())
	}

	var nilMap *OrderedMap
	if nilMap.Len() != 0 {
		t.Error("nil 的 Len 应为 0")
	}
	nilMap.Each(func(string, string) { t.Error("nil 不应遍历到任何键") })
}

package actionkey

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
)

// RFC 8785 附录 B 的数字向量（IEEE 754 位模式 → 期望输出）。
func TestFormatNumberRFC8785(t *testing.T) {
	t.Parallel()
	cases := []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"},
		{0x8000000000000000, "0"},
		{0x0000000000000001, "5e-324"},
		{0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"},
		{0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
		{0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x41b3de4355555553, "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			got, err := formatNumber(math.Float64frombits(c.bits))
			if err != nil || got != c.want {
				t.Errorf("formatNumber(%#016x) = %q, %v，期望 %q", c.bits, got, err, c.want)
			}
		})
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := formatNumber(f); err == nil {
			t.Errorf("formatNumber(%v) 应当报错", f)
		}
	}
}

// RFC 8785 #3.2.3 和 #3.2.4 的示例。
func TestCanonicalRFC8785(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{
			name: "#3.2.3 综合示例",
			in: `{"numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
			      "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
			      "literals": [null, true, false]}`,
			want: `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`,
		},
		{
			name: "#3.2.3 键按 UTF-16 排序",
			in: `{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh",
			      "1": "One", "\ud83d\ude00": "Emoji: Grinning Face", "\u0080": "Control",
			      "\u00f6": "Latin Small Letter O With Diaeresis"}`,
			want: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\"," +
				"\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{name: "嵌套与空值", in: `{"b":[],"a":{"y":{},"x":[1,2.0,-0]}}`, want: `{"a":{"x":[1,2,0],"y":{}},"b":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonical(json.RawMessage(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("Canonical:\n得到 %s\n期望 %s", got, c.want)
			}
		})
	}
}

func TestComputeDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	rts := [][2]string{{"node", "sha256:b"}, {"go", "sha256:a"}}
	orig := slices.Clone(rts)
	in := Input{Platform: "linux/arm64", Runtimes: rts}
	k1, parts, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rts, orig) {
		t.Fatalf("Compute 修改了调用方的 Runtimes：%v", rts)
	}
	// runtime 的声明顺序不影响 action key
	in.Runtimes = [][2]string{{"go", "sha256:a"}, {"node", "sha256:b"}}
	k2, _, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("runtime 顺序不同导致 action key 不同：%s != %s", k1, k2)
	}
	if len(parts) != 14 || parts["runtimes"] == "" {
		t.Fatalf("parts = %v", parts)
	}
}

// 空字段与显式空值的 action key 必须一致（nil 切片和 map 输出 [] / {}）。
func TestComputeNilEqualsEmpty(t *testing.T) {
	t.Parallel()
	k1, _, err := Compute(Input{Salt: "s"})
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := Compute(Input{
		Salt: "s", Runtimes: [][2]string{}, Env: map[string]string{}, Steps: []map[string]any{},
		Outputs: map[string]string{}, Upstream: [][2]string{}, FetchTo: []string{}, Services: []any{}, Repositories: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("%s != %s", k1, k2)
	}
	// 固定值：算法或注入字段变化时必须同时递增 Version
	if want := goldenEmptyKey; k1 != want {
		t.Fatalf("action key = %s，期望 %s（修改算法时请递增 Version）", k1, want)
	}
}

func TestInputDigestOrderIndependent(t *testing.T) {
	t.Parallel()
	a := []TreeEntry{{Path: "b.go", Mode: ModeFile, SHA: "2"}, {Path: "a.go", Mode: ModeExecutable, SHA: "1"}}
	b := []TreeEntry{a[1], a[0]}
	if InputDigest(a) != InputDigest(b) {
		t.Fatal("输入摘要不应依赖条目顺序")
	}
	if a[0].Path != "b.go" {
		t.Fatal("InputDigest 修改了调用方的切片")
	}
}

// goldenEmptyKey 是 Compute(Input{Salt: "s"}) 的结果（与重构前的实现一致）。
const goldenEmptyKey = "369e826af6a742faa7a260ef932bea56d5e5d4d1fb4837e3539ba6d7ce23a6c4"

func TestInputDigestContent(t *testing.T) {
	t.Parallel()
	// 空 tree 的摘要就是空输入的 sha256
	if got, want := InputDigest(nil), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; got != want {
		t.Fatalf("InputDigest(nil) = %s，期望 %s", got, want)
	}
	// 每个条目按 "path \0 mode \0 sha \n" 拼接，按 path 字节序排序（大写字母在小写之前）
	entries := []TreeEntry{
		{Path: "z", Mode: ModeSymlink, SHA: "c"},
		{Path: "B", Mode: ModeSubmodule, SHA: "b"},
		{Path: "a", Mode: ModeFile, SHA: "a"},
	}
	want := Sum([]byte("B\x00160000\x00b\na\x00100644\x00a\nz\x00120000\x00c\n"))
	if got := InputDigest(entries); got != want {
		t.Fatalf("InputDigest = %s，期望 %s", got, want)
	}

	base := []TreeEntry{{Path: "main.go", Mode: ModeFile, SHA: "1"}}
	variants := []struct {
		name    string
		entries []TreeEntry
	}{
		{"文件模式不同", []TreeEntry{{Path: "main.go", Mode: ModeExecutable, SHA: "1"}}},
		{"对象不同", []TreeEntry{{Path: "main.go", Mode: ModeFile, SHA: "2"}}},
		{"路径不同", []TreeEntry{{Path: "main2.go", Mode: ModeFile, SHA: "1"}}},
		{"多一个文件", []TreeEntry{base[0], {Path: "x", Mode: ModeFile, SHA: "1"}}},
		// 分隔符保证字段之间的边界不会混淆
		{"字段边界移动", []TreeEntry{{Path: "main.go\x00100644", Mode: "", SHA: "1"}}},
	}
	for _, v := range variants {
		if InputDigest(v.entries) == InputDigest(base) {
			t.Errorf("%s：输入摘要应当不同", v.name)
		}
	}
}

// fullInput 返回每个字段都有值的 Input。
func fullInput() Input {
	return Input{
		Salt:         "salt",
		Platform:     "linux/arm64",
		Executor:     map[string]any{"kind": "container", "image": "golang:1.25"},
		Inputs:       "digest",
		Runtimes:     [][2]string{{"go", "sha256:a"}},
		Env:          map[string]string{"CGO_ENABLED": "0"},
		Steps:        []map[string]any{{"run": "go build"}},
		Outputs:      map[string]string{"bin": "dist/"},
		Upstream:     [][2]string{{"build.bin", "sha256:m"}},
		FetchTo:      []string{"dist/"},
		Services:     []any{map[string]any{"image": "postgres:17"}},
		Network:      false,
		Repositories: []string{"o/r"},
	}
}

// 每个字段都参与 action key：只改一个字段时，key 和该字段的分项哈希都变化，其他分项不变。
func TestComputeEachFieldAffectsKey(t *testing.T) {
	t.Parallel()
	baseKey, baseParts, err := Compute(fullInput())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		part   string
		mutate func(*Input)
	}{
		{"salt", func(in *Input) { in.Salt = "other" }},
		{"platform", func(in *Input) { in.Platform = "linux/amd64" }},
		{"executor", func(in *Input) { in.Executor["image"] = "golang:1.26" }},
		{"inputs", func(in *Input) { in.Inputs = "digest2" }},
		{"runtimes", func(in *Input) { in.Runtimes = [][2]string{{"go", "sha256:b"}} }},
		{"env", func(in *Input) { in.Env["CGO_ENABLED"] = "1" }},
		{"steps", func(in *Input) { in.Steps = append(in.Steps, map[string]any{"run": "go test"}) }},
		{"outputs", func(in *Input) { in.Outputs = nil }},
		{"upstream", func(in *Input) { in.Upstream = [][2]string{{"build.bin", "sha256:n"}} }},
		{"fetch_to", func(in *Input) { in.FetchTo = []string{"out/"} }},
		{"services", func(in *Input) { in.Services = nil }},
		{"network", func(in *Input) { in.Network = true }},
		{"repositories", func(in *Input) { in.Repositories = append(in.Repositories, "o/other") }},
	}
	for _, c := range cases {
		t.Run(c.part, func(t *testing.T) {
			t.Parallel()
			in := fullInput()
			c.mutate(&in)
			key, parts, err := Compute(in)
			if err != nil {
				t.Fatal(err)
			}
			if key == baseKey {
				t.Errorf("修改 %s 后 action key 没有变化", c.part)
			}
			for k, v := range parts {
				changed := v != baseParts[k]
				if k == c.part && !changed {
					t.Errorf("分项 %s 应当变化", k)
				}
				if k != c.part && changed {
					t.Errorf("只修改了 %s，分项 %s 却变化了", c.part, k)
				}
			}
		})
	}
}

// upstream 按 fetch 声明顺序参与计算（与 runtimes 不同，不排序）。
func TestComputeUpstreamOrderMatters(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Upstream = [][2]string{{"a.x", "1"}, {"b.y", "2"}}
	k1, _, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Upstream = [][2]string{{"b.y", "2"}, {"a.x", "1"}}
	k2, _, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Error("upstream 的顺序应当影响 action key")
	}
}

func TestComputeRejectsUnsupportedValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Input
	}{
		{name: "executor 中有 NaN", in: Input{Executor: map[string]any{"cpu": math.NaN()}}},
		{name: "steps 中有无法序列化的值", in: Input{Steps: []map[string]any{{"f": func() {}}}}},
		{name: "services 中有超出范围的数字", in: Input{Services: []any{json.RawMessage("1e400")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			key, parts, err := Compute(c.in)
			if err == nil {
				t.Fatalf("期望报错，得到 key %s", key)
			}
			if key != "" || parts != nil {
				t.Errorf("出错时不应返回结果：%q %v", key, parts)
			}
		})
	}
}

func TestCanonicalErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
	}{
		{name: "无法序列化的类型", v: make(chan int)},
		{name: "无效的 JSON", v: json.RawMessage("{")},
		{name: "超出 float64 范围的数字", v: json.RawMessage(`{"n":1e400}`)},
		{name: "数组中超出范围的数字", v: json.RawMessage(`[1,-1e999]`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got, err := Canonical(c.v); err == nil {
				t.Fatalf("期望报错，得到 %s", got)
			}
		})
	}
}

func TestCanonicalStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{name: "短转义", in: "\b\f\n\r\t\"\\", want: `"\b\f\n\r\t\"\\"`},
		{name: "其他控制字符用 u 转义", in: "\x00\x01\x1f", want: `"\u0000\u0001\u001f"`},
		{name: "HTML 字符原样输出", in: "<a&b>", want: `"<a&b>"`},
		{name: "DEL 和非 ASCII 原样输出", in: "\x7f€ ", want: "\"\x7f€ \""},
		{name: "斜杠不转义", in: "a/b", want: `"a/b"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonical(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("Canonical(%q) = %s，期望 %s", c.in, got, c.want)
			}
		})
	}
}

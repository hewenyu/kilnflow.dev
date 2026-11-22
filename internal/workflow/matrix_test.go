package workflow

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// expandSrc 解析并展开 src（解析必须成功），返回展开结果。
func expandSrc(t *testing.T, src string, pc PlanContext) *Plan {
	t.Helper()
	return Expand(mustParse(t, src), pc)
}

// jobKeys 返回 job ID 为 id 的全部变体的 Key（按展开顺序）。
func jobKeys(p *Plan, id string) []string {
	var keys []string
	for _, j := range p.Jobs {
		if j.JobID == id {
			keys = append(keys, j.Key)
		}
	}
	return keys
}

// matrixJob 返回只有一个 job a、strategy.matrix 为 matrix（YAML 块，缩进 6 格）的 workflow。
func matrixJob(matrix string) string {
	return "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    strategy:\n      matrix:\n" + matrix + "    steps: [{run: x}]\n"
}

// 矩阵按 Actions 的规则展开：笛卡尔积 → exclude → include。
func TestExpandMatrix(t *testing.T) {
	cases := []struct {
		name   string
		matrix string
		vars   map[string]string
		want   []string
	}{
		{
			name:   "笛卡尔积按变量声明顺序展开",
			matrix: "        os: [linux, darwin]\n        arch: [arm64, amd64]\n",
			want:   []string{"a{arch=arm64,os=linux}", "a{arch=amd64,os=linux}", "a{arch=arm64,os=darwin}", "a{arch=amd64,os=darwin}"},
		},
		{
			name:   "exclude 部分匹配即去掉",
			matrix: "        os: [linux, darwin]\n        arch: [arm64, amd64]\n        exclude:\n          - { os: darwin }\n          - { os: linux, arch: amd64 }\n",
			want:   []string{"a{arch=arm64,os=linux}"},
		},
		{
			name:   "exclude 不匹配任何变体时没有影响",
			matrix: "        os: [linux]\n        exclude: [{ os: windows }]\n",
			want:   []string{"a{os=linux}"},
		},
		{
			// GitHub 文档中的例子：include 合并到所有不改变原有变量取值的原始变体，后面的 include 可以覆盖
			// 前面 include 添加的变量；不能合并时作为新变体追加
			name: "include 的合并规则与 Actions 一致",
			matrix: `        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
          - color: pink
            animal: cat
          - fruit: apple
            shape: circle
          - fruit: banana
          - fruit: banana
            animal: cat
`,
			want: []string{
				"a{animal=cat,color=pink,fruit=apple,shape=circle}",
				"a{animal=dog,color=green,fruit=apple,shape=circle}",
				"a{animal=cat,color=pink,fruit=pear}",
				"a{animal=dog,color=green,fruit=pear}",
				"a{fruit=banana}",
				"a{animal=cat,fruit=banana}",
			},
		},
		{
			name:   "只有 include",
			matrix: "        include:\n          - { os: linux }\n          - { os: darwin, xcode: '16' }\n",
			want:   []string{"a{os=linux}", "a{os=darwin,xcode=16}"},
		},
		{
			name:   "exclude 之后再 include 可以加回被去掉的组合",
			matrix: "        os: [linux, darwin]\n        exclude: [{ os: darwin }]\n        include: [{ os: darwin, extra: x }]\n",
			want:   []string{"a{os=linux}", "a{extra=x,os=darwin}"},
		},
		{
			name:   "YAML 的数字和表达式的数字相等",
			matrix: "        n: [1, 2]\n        exclude: '${{ fromJSON(''[{\"n\": 1}]'') }}'\n",
			want:   []string{"a{n=2}"},
		},
		{
			name:   "取值可以是对象，exclude 按结构比较",
			matrix: "        cfg: [{ os: linux }, { os: darwin }]\n        exclude: [{ cfg: { os: darwin } }]\n",
			want:   []string{`a{cfg={"os":"linux"}}`},
		},
		{
			name:   "变量的取值是表达式",
			matrix: "        os: ${{ fromJSON(vars.OSES) }}\n",
			vars:   map[string]string{"OSES": `["l","d"]`},
			want:   []string{"a{os=l}", "a{os=d}"},
		},
		{
			name:   "include 是表达式",
			matrix: "        os: [l]\n        include: ${{ fromJSON(vars.INC) }}\n",
			vars:   map[string]string{"INC": `[{"os":"l","x":1}]`},
			want:   []string{"a{os=l,x=1}"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := ctx()
			pc.Vars = func(string) map[string]string { return c.vars }
			p := expandSrc(t, matrixJob(c.matrix), pc)
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			if got := jobKeys(p, "a"); !slices.Equal(got, c.want) {
				t.Errorf("变体 = %q\n期望 %q", got, c.want)
			}
		})
	}
}

// 整个矩阵是表达式时，变量按名字排序；显示名按变量顺序列出取值。
func TestExpandMatrixFromExpression(t *testing.T) {
	pc := ctx()
	pc.Vars = func(string) map[string]string {
		return map[string]string{"M": `{"os":["l"],"arch":["x","y"],"exclude":[{"arch":"y"}],"include":[{"os":"l","extra":"e"}]}`}
	}
	p := expandSrc(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, strategy: { matrix: '${{ fromJSON(vars.M) }}' }, steps: [{run: x}] }\n", pc)
	if p.Diags.HasErrors() {
		t.Fatal(p.Diags)
	}
	if len(p.Jobs) != 1 {
		t.Fatalf("应有 1 个变体：%v", jobKeys(p, "a"))
	}
	j := p.Jobs[0]
	if j.Key != "a{arch=x,extra=e,os=l}" || j.DisplayName != "a (x, l, e)" || !slices.Equal(j.MatrixKeys, []string{"arch", "os", "extra"}) {
		t.Errorf("变体 = %q %q %v", j.Key, j.DisplayName, j.MatrixKeys)
	}
}

func TestExpandMatrixErrors(t *testing.T) {
	cases := []struct {
		name   string
		matrix string // strategy.matrix 的值（flow 写法）
		want   string
	}{
		{"矩阵表达式的结果不是对象", "'${{ fromJSON(''[1]'') }}'", "job a 的矩阵：matrix 表达式的结果应为对象"},
		// 以前结果中的字符串会再当成表达式求值（例如来自 inputs 的 ${{ }}）
		// 结果中 os 是字符串 "${{ fromJSON('[1]') }}"：以前被再求值成 [1]，展开出 os=1 的变体
		{"矩阵表达式结果中的值不是列表", `'${{ fromJSON(''{"os": "${{ fromJSON(''''[1]'''') }}"}'') }}'`, "job a 的矩阵：matrix 表达式的结果中 os 应为列表"},
		{"矩阵表达式出错", "'${{ nope }}'", "job a 的矩阵：表达式 \"nope\"：上下文 nope 在此处不可用"},
		{"变量的表达式结果不是列表", "{ os: '${{ vars.X }}' }", "job a 的矩阵：os：应为列表：${{ vars.X }}"},
		{"变量的表达式出错", "{ os: '${{ secrets.X }}' }", "job a 的矩阵：os：表达式 \"secrets.X\"：上下文 secrets 在此处不可用"},
		{"变量为空列表", "{ os: [] }", "job a 的矩阵：os 不能为空列表"},
		{"exclude 的项不是对象", "{ os: [a], exclude: [a] }", "job a 的矩阵：exclude 的每一项应为对象"},
		{"include 的项不是对象", "{ os: [a], include: [a] }", "job a 的矩阵：include 的每一项应为对象"},
		{"exclude 表达式出错", "{ os: [a], exclude: '${{ nope }}' }", "job a 的矩阵：exclude：表达式"},
		{"include 表达式出错", "{ os: [a], include: '${{ nope }}' }", "job a 的矩阵：include：表达式"},
		{"全部被 exclude", "{ os: [a], exclude: [{ os: a }] }", "job a 的矩阵展开后为空"},
		{"重复的变体", "{ os: [a, a] }", "job a{os=a} 的矩阵中有重复的变体"},
		{"矩阵中引用不存在的变量", "{ os: [a] }\n    name: '${{ matrix.arch }}'", "job a：name：表达式 \"matrix.arch\"：不支持的字段 matrix.arch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expandSrc(t, "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps: [{run: x}]\n    strategy:\n      matrix: "+c.matrix+"\n", ctx())
			wantDiag(t, p.Diags, c.want)
		})
	}
}

// 显示名：name 没有引用 matrix 时在后面加上矩阵取值；显示名重复时报错。
func TestDisplayName(t *testing.T) {
	cases := []struct {
		name string
		job  string
		want []string
	}{
		{"没有 name", "strategy: { matrix: { os: [l, d], n: [1] } }", []string{"a (l, 1)", "a (d, 1)"}},
		{"name 没有引用 matrix", "name: Build ${{ github.ref_name }}\n    strategy: { matrix: { os: [l] } }", []string{"Build main (l)"}},
		{"name 引用了 matrix", "name: Build ${{ matrix.os }}\n    strategy: { matrix: { os: [l, d] } }", []string{"Build l", "Build d"}},
		{"name 通过下标引用 MATRIX", "name: Build ${{ MATRIX['os'] }}\n    strategy: { matrix: { os: [l] } }", []string{"Build l"}},
		{"没有矩阵", "name: Plain", []string{"Plain"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expandSrc(t, "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps: [{run: x}]\n    "+c.job+"\n", ctx())
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			var got []string
			for _, j := range p.Jobs {
				got = append(got, j.DisplayName)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("显示名 = %q，期望 %q", got, c.want)
			}
		})
	}

	t.Run("显示名重复", func(t *testing.T) {
		p := expandSrc(t, "on: push\njobs:\n  a: { name: 'x ${{ matrix.os }}', runs-on: linux/arm64, strategy: { matrix: { os: [l], n: [1, 2] } }, steps: [{run: x}] }\n", ctx())
		wantDiag(t, p.Diags, `job a{n=2,os=l} 与 a{n=1,os=l} 的显示名都是 "x l"，Check Run 会重名`)
	})
	t.Run("不同 job 的显示名重复", func(t *testing.T) {
		p := expandSrc(t, "on: push\njobs:\n  a: { name: same, runs-on: linux/arm64, steps: [{run: x}] }\n  b: { name: same, runs-on: linux/arm64, steps: [{run: x}] }\n", ctx())
		wantDiag(t, p.Diags, `x.yml:4:3: 错误：job b 与 a 的显示名都是 "same"`)
	})
}

// 单个 run 的 job 数量上限。
func TestExpandMaxJobs(t *testing.T) {
	vals := make([]string, 17)
	for i := range vals {
		vals[i] = strconv.Itoa(i)
	}
	list := "[" + strings.Join(vals, ", ") + "]"
	// 一个 job 的变体超过上限：在展开各变体之前报错
	p := expandSrc(t, matrixJob("        a: "+list+"\n        b: "+list[:len(list)-5]+"]\n"), ctx())
	// 17 × 16 = 272 > 256
	wantDiag(t, p.Diags, fmt.Sprintf("job a 的矩阵：展开后有 %d 个变体，超过单个 run 的 job 上限 %d", 17*16, MaxJobs))
	if len(p.Jobs) != 0 {
		t.Errorf("不应展开任何变体，实际 %d 个", len(p.Jobs))
	}

	p = expandSrc(t, matrixJob("        a: "+list+"\n        b: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14]\n"), ctx())
	if p.Diags.HasErrors() {
		t.Errorf("17 × 15 = 255 个 job 不应超过上限：%v", p.Diags)
	}

	// include 追加的变体也计入
	p = expandSrc(t, matrixJob("        a: "+list+"\n        b: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14]\n        include: [{a: x}, {a: y}]\n"), ctx())
	wantDiag(t, p.Diags, fmt.Sprintf("展开后有 %d 个变体，超过单个 run 的 job 上限 %d", 257, MaxJobs))

	// 只有 include 的矩阵也按上限早停，不先展开出超大的列表
	var inc strings.Builder
	inc.WriteString("        include:\n")
	for i := range MaxJobs + 1 {
		fmt.Fprintf(&inc, "          - { a: %d }\n", i)
	}
	p = expandSrc(t, matrixJob(inc.String()), ctx())
	wantDiag(t, p.Diags, fmt.Sprintf("展开后有 %d 个变体，超过单个 run 的 job 上限 %d", MaxJobs+1, MaxJobs))

	// 多个 job 合计超过上限
	two := "on: push\njobs:\n" +
		"  a: { runs-on: linux/arm64, strategy: { matrix: { v: " + list + ", w: [0, 1, 2, 3, 4, 5, 6, 7] } }, steps: [{run: x}] }\n" +
		"  b: { runs-on: linux/arm64, strategy: { matrix: { v: " + list + ", w: [0, 1, 2, 3, 4, 5, 6, 7] } }, steps: [{run: x}] }\n"
	p = expandSrc(t, two, ctx())
	wantDiag(t, p.Diags, fmt.Sprintf("矩阵展开后共有 %d 个 job，超过上限 %d", 2*17*8, MaxJobs))
}

// exclude 逐一比较的次数（组合数 × exclude 项数）有上限，不会让规划长时间占满 CPU。
func TestExpandMatrixExcludeWorkLimit(t *testing.T) {
	var b strings.Builder
	for i := range 16 { // 2^16 个组合
		fmt.Fprintf(&b, "        v%d: [0, 1]\n", i)
	}
	b.WriteString("        exclude:\n")
	for i := range maxExcludeWork>>16 + 1 {
		fmt.Fprintf(&b, "          - {v0: %d}\n", i+2) // 都不匹配
	}
	p := expandSrc(t, matrixJob(b.String()), ctx())
	wantDiag(t, p.Diags, fmt.Sprintf("比较次数超过上限 %d", maxExcludeWork))
}

// 标量直接比较，结果与比较 JSON 形式相同；数组和对象按结构或 JSON 形式比较。
func TestValueEqual(t *testing.T) {
	cases := []struct {
		a, b any
		want bool
	}{
		{"a", "a", true},
		{"a", "A", false},
		{1.0, 1, true}, // YAML 的 1 与 Go 代码中的 int
		{1.0, 1.5, false},
		{"1", 1.0, false}, // 字符串与数字
		{1.0, "1", false},
		{true, true, true},
		{true, "true", false},
		{nil, nil, true},
		{nil, "", false},
		{math.NaN(), math.NaN(), false},
		{[]any{1.0, "x"}, []any{1, "x"}, true},
		{map[string]any{"k": 1.0}, map[string]any{"k": 1}, true},
		{map[string]any{"k": 1.0}, map[string]any{"k": "1"}, false},
		{[]any{"x"}, "x", false},
	}
	for _, c := range cases {
		if got := valueEqual(c.a, c.b); got != c.want {
			t.Errorf("valueEqual(%#v, %#v) = %v，期望 %v", c.a, c.b, got, c.want)
		}
		if got := valueEqual(c.b, c.a); got != c.want {
			t.Errorf("valueEqual(%#v, %#v) = %v，期望 %v", c.b, c.a, got, c.want)
		}
	}
}

// 笛卡尔积的组合数在生成之前就限制住，不会因为变量多而耗尽内存。
func TestExpandMatrixCartesianLimit(t *testing.T) {
	list := "[0, 1, 2, 3, 4, 5, 6, 7, 8, 9]"
	var b strings.Builder
	for i := range 8 { // 10^8 个组合
		fmt.Fprintf(&b, "        v%d: %s\n", i, list)
	}
	p := expandSrc(t, matrixJob(b.String()), ctx())
	wantDiag(t, p.Diags, fmt.Sprintf("各变量取值的组合超过 %d 个", maxCartesian))
	if len(p.Jobs) != 0 {
		t.Errorf("不应生成 job，实际 %d 个", len(p.Jobs))
	}
}

// fetch.matrix 从带矩阵的上游中选出恰好一个变体。
func TestFetchSelectsVariant(t *testing.T) {
	const up = `on: push
jobs:
  build:
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, darwin], arch: [arm64] } }
    steps: [{run: x}]
    outputs: { bin: dist/ }
`
	t.Run("按下游的矩阵取值选择", func(t *testing.T) {
		p := expandSrc(t, up+`  test:
    needs: [build]
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, darwin] } }
    fetch: [{ from: build.bin, to: bin/, matrix: { os: "${{ matrix.os }}", arch: arm64 } }]
    steps: [{run: x}]
`, ctx())
		if p.Diags.HasErrors() {
			t.Fatal(p.Diags)
		}
		for _, j := range p.Jobs {
			if j.JobID != "test" {
				continue
			}
			f := j.Spec.Fetch[0]
			want := fmt.Sprintf("build{arch=arm64,os=%s}", j.Matrix["os"])
			if f.JobKey != want || f.Output != "bin" || f.To != "bin/" {
				t.Errorf("%s 的 fetch = %+v，期望来自 %s", j.Key, f, want)
			}
			// needs 包含上游的全部变体
			if len(j.NeedsKeys) != 2 {
				t.Errorf("%s 的 NeedsKeys = %v", j.Key, j.NeedsKeys)
			}
		}
	})

	// 与 job 的其他字段一致，引用不存在的矩阵变量报错（以前静默得到空字符串，再报"选出了 0 个变体"）
	t.Run("引用不存在的矩阵变量", func(t *testing.T) {
		p := expandSrc(t, up+`  test:
    needs: [build]
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, darwin] } }
    fetch: [{ from: build.bin, to: bin/, matrix: { os: "${{ matrix.oss }}", arch: arm64 } }]
    steps: [{run: x}]
`, ctx())
		wantDiag(t, p.Diags, "fetch.matrix.os：表达式 \"matrix.oss\"：不支持的字段 matrix.oss")
	})

	cases := []struct {
		name   string
		matrix string
		want   string
	}{
		{"没有选中", "{ os: windows }", "job test 的 fetch build.bin 选出了 0 个变体，应恰好一个"},
		{"选中多个", "{ arch: arm64 }", "job test 的 fetch build.bin 选出了 2 个变体，应恰好一个"},
		{"选择条件出错", "{ os: '${{ nope }}' }", "fetch.matrix.os：表达式"},
		{"按不存在的变量选择", "{ cpu: x }", "选出了 0 个变体"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expandSrc(t, up+"  test:\n    needs: [build]\n    runs-on: linux/arm64\n    fetch: [{ from: build.bin, to: bin/, matrix: "+c.matrix+" }]\n    steps: [{run: x}]\n", ctx())
			wantDiag(t, p.Diags, c.want)
		})
	}
}

func TestJobKey(t *testing.T) {
	cases := []struct {
		combo map[string]any
		want  string
	}{
		{nil, "j"},
		{map[string]any{}, "j"},
		{map[string]any{"b": 1.0, "a": "x"}, "j{a=x,b=1}"},
		{map[string]any{"ok": true, "n": nil}, "j{n=,ok=true}"},
		{map[string]any{"l": []any{"x", 2.0}}, `j{l=["x",2]}`},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := JobKey("j", c.combo); got != c.want {
				t.Errorf("JobKey = %q，期望 %q", got, c.want)
			}
		})
	}
}

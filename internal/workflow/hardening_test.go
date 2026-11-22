package workflow

import (
	"fmt"
	"strings"
	"testing"
)

// hasPinnedDigest 比 Contains("@sha256:") 严格：必须正好是 64 位小写十六进制。
func TestHasPinnedDigest(t *testing.T) {
	t.Parallel()
	hex := strings.Repeat("ab", 32)
	good := []string{"img@" + "sha256:" + hex, "registry:5000/o/img:tag@sha256:" + hex}
	bad := []string{
		"img", "img:tag", "img@sha256:", "img@sha256:abc", "img@sha256:" + strings.ToUpper(hex),
		"img@sha256:" + hex + "0", "img@sha256:" + strings.Repeat("g", 64), "img@sha512:" + hex,
		"@sha256:" + hex, // @ 之前为空：没有镜像名
		"img@sha256:../../" + strings.Repeat("0", 58),
	}
	for _, ref := range good {
		if !hasPinnedDigest(ref) {
			t.Errorf("hasPinnedDigest(%q) = false，应为 true", ref)
		}
	}
	for _, ref := range bad {
		if hasPinnedDigest(ref) {
			t.Errorf("hasPinnedDigest(%q) = true，应为 false", ref)
		}
	}
}

// parse 阶段：短 digest、缺 hex、表达式之外的写法都不能当成已固定。
func TestParseRejectsLooseDigest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, job string }{
		{"container 短 digest", "{ runs-on: linux/arm64, container: a@sha256:abc, steps: [{run: x}] }"},
		{"container 空 digest", "{ runs-on: linux/arm64, container: a@sha256:, steps: [{run: x}] }"},
		{"services 短 digest", "{ runs-on: linux/arm64, container: a@sha256:" + strings.Repeat("a", 64) + ", services: { pg: { image: p@sha256:abc } }, steps: [{run: x}] }"},
		{"image.base 短 digest", "{ image: { base: a@sha256:abc } }"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n  a: "+c.job+"\n"))
			wantDiag(t, diags, "必须用 digest 固定")
		})
	}
}

// services[].image 用表达式时，展开后的结果同样必须用 digest 固定。
func TestExpandRejectsUnpinnedServiceImage(t *testing.T) {
	t.Parallel()
	src := `on: push
jobs:
  a:
    runs-on: linux/arm64
    container: docker.io/library/debian@sha256:` + strings.Repeat("a", 64) + `
    services:
      pg: { image: "${{ vars.IMG }}" }
    steps: [{run: x}]
`
	pc := ctx()
	pc.Vars = func(string) map[string]string { return map[string]string{"IMG": "postgres:17"} }
	if p := Expand(mustParse(t, src), pc); !strings.Contains(p.Diags.Error(), "services.pg.image 必须用 digest 固定") {
		t.Fatalf("应报告 services 镜像没有 digest：%s", p.Diags.Error())
	}
	pc.Vars = func(string) map[string]string {
		return map[string]string{"IMG": "postgres@sha256:" + strings.Repeat("b", 64)}
	}
	if p := Expand(mustParse(t, src), pc); p.Diags.HasErrors() {
		t.Fatalf("合法 digest 不应报错：%s", p.Diags.Error())
	}
}

// timeout-minutes、max-parallel 必须是合理范围内的正整数；services 的服务名必须是安全的单段标识符。
func TestParseRejectsInvalidBoundsAndNames(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, job, want string }{
		{"timeout 为 0", "{ runs-on: linux/arm64, timeout-minutes: 0, steps: [{run: x}] }", "timeout-minutes 应在 1 到 1440 之间"},
		{"timeout 为负", "{ runs-on: linux/arm64, timeout-minutes: -1, steps: [{run: x}] }", "timeout-minutes 应在 1 到 1440 之间"},
		{"timeout 过大", "{ runs-on: linux/arm64, timeout-minutes: 2000, steps: [{run: x}] }", "timeout-minutes 应在 1 到 1440 之间"},
		{"max-parallel 为 0", "{ runs-on: linux/arm64, strategy: { max-parallel: 0 }, steps: [{run: x}] }", "max-parallel 应在 1 到 256 之间"},
		{"max-parallel 过大", "{ runs-on: linux/arm64, strategy: { max-parallel: 300 }, steps: [{run: x}] }", "max-parallel 应在 1 到 256 之间"},
		{"service 名含 ..", "{ runs-on: linux/arm64, container: a@sha256:" + strings.Repeat("a", 64) + `, services: { "..": { image: p@sha256:` + strings.Repeat("b", 64) + ` } }, steps: [{run: x}] }`, "services 的服务名"},
		{"service 名含 /", "{ runs-on: linux/arm64, container: a@sha256:" + strings.Repeat("a", 64) + `, services: { "a/b": { image: p@sha256:` + strings.Repeat("b", 64) + ` } }, steps: [{run: x}] }`, "services 的服务名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n  a: "+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}
}

// release.uploads.token 里的表达式无法解析时，checkSecrets 不能吞掉错误。
func TestParseReportsSecretRefsError(t *testing.T) {
	t.Parallel()
	const src = `on: push
jobs:
  a:
    release:
      assets: [{ from: build.bin }]
      uploads:
        - url: "https://e.com/r"
          token: "${{ secrets.T"
`
	_, diags := Parse("x.yml", []byte(src))
	wantDiag(t, diags, "release.uploads.token")
}

// 展开后的 release.uploads.url 重复（至少一边来自表达式）要报错。
func TestExpandRejectsDuplicateUploadURL(t *testing.T) {
	t.Parallel()
	src := `on: push
jobs:
  build:
    runs-on: linux/arm64
    steps: [{run: x}]
    outputs: { bin: dist/ }
  publish:
    needs: [build]
    release:
      assets: [{ from: build.bin }]
      uploads:
        - { url: "https://up.example.com/r", token: "${{ secrets.T }}" }
        - { url: "${{ vars.U }}", token: "${{ secrets.T }}" }
`
	pc := ctx()
	pc.Vars = func(string) map[string]string { return map[string]string{"U": "https://up.example.com/r"} }
	if p := Expand(mustParse(t, src), pc); !strings.Contains(p.Diags.Error(), "release.uploads 中 url https://up.example.com/r 重复（展开后）") {
		t.Fatalf("应报告展开后重复的 url：%s", p.Diags.Error())
	}
	pc.Vars = func(string) map[string]string { return map[string]string{"U": "https://mirror.example.com/r"} }
	if p := Expand(mustParse(t, src), pc); p.Diags.HasErrors() {
		t.Fatalf("不重复时不应报错：%s", p.Diags.Error())
	}
}

// include 的比较次数有上限，超限时快速失败而不是长时间占满 CPU。
func TestExpandMatrixIncludeWorkLimit(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("        a: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15]\n")
	b.WriteString("        b: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15]\n")
	b.WriteString("        include:\n")
	// 16 × 16 = 256 个变体（不超过 MaxJobs），× 257 项 > maxIncludeWork
	for i := range 257 {
		fmt.Fprintf(&b, "          - {extra: %d}\n", i)
	}
	// 先缩小组合数，避免在 checkVariantCount 处就失败：用 include 的上限测试只需要超过工作上限。
	p := expandSrc(t, matrixJob(b.String()), ctx())
	if !strings.Contains(p.Diags.Error(), "比较次数超过上限") {
		t.Fatalf("应报告 include 工作量超限：%s", p.Diags.Error())
	}
}

// 过滤模式按 rune 处理（多字节字符不能拆成字节），字符类中的转义 ']' 不能提前结束字符类。
func TestFilterRegexpUnicodeAndClassEscapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		match   []string
		no      []string
	}{
		{"文件?", []string{"文", "文件"}, []string{"文件夹"}},
		{"é+", []string{"é", "éé"}, []string{"e"}},
		{`a[\]]b`, []string{"a]b"}, []string{"ab", "a[b"}},
		{`[a-c]+`, []string{"abc"}, []string{"abd"}},
		{`[\-x]`, []string{"-", "x"}, []string{"y"}},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			t.Parallel()
			re, err := filterRegexp(c.pattern)
			if err != nil {
				t.Fatalf("filterRegexp(%q)：%v", c.pattern, err)
			}
			for _, s := range c.match {
				if !re.MatchString(s) {
					t.Errorf("%q 应匹配 %q", c.pattern, s)
				}
			}
			for _, s := range c.no {
				if re.MatchString(s) {
					t.Errorf("%q 不应匹配 %q", c.pattern, s)
				}
			}
		})
	}
	if _, err := filterRegexp(`a[\`); err == nil {
		t.Error("字符类缺少 ']' 应报错")
	}
}

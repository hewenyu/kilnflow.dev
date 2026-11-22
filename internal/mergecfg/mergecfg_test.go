package mergecfg

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		method  string
		wantErr string
	}{
		{name: "空文件", in: "", method: MethodSquash},
		{name: "只有注释", in: "# 默认\n", method: MethodSquash},
		{name: "merge", in: "method: merge\n", method: MethodMerge},
		{name: "rebase", in: "method: rebase\n", method: MethodRebase},
		{name: "不支持的方式", in: "method: fast-forward\n", wantErr: "method 只能是"},
		{name: "未知字段", in: "methd: squash\n", wantErr: "methd"},
		{name: "不是 YAML 对象", in: "- a\n", wantErr: Path},
		{name: "太大", in: strings.Repeat("#", MaxConfigSize+1), wantErr: "超过"},
		{name: "文档结束标记", in: "method: merge\n...\n", method: MethodMerge},
		// 第二个文档不能被静默忽略（否则实际的合并方式与文件中写的不同）
		{name: "第二个文档", in: "method: squash\n---\nmethod: rebase\n", wantErr: "只能包含一个 YAML 文档"},
		{name: "空的第二个文档", in: "method: merge\n---\n", wantErr: "只能包含一个 YAML 文档"},
		{name: "第一个文档之后的语法错误", in: "method: merge\n---\n[\n", wantErr: Path},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() err = %v，想要包含 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Method != tt.method {
				t.Errorf("Method = %q，想要 %q", c.Method, tt.method)
			}
		})
	}
	if Default().Method != MethodSquash {
		t.Error("默认的合并方式应该是 squash")
	}
}

// required：<workflow> 或 <workflow>/<job>，workflow 不带扩展名，不能重复。
func TestParseRequired(t *testing.T) {
	c, err := Parse([]byte("required: [ci, ci/test, ai-review, release.v2/build_all]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Requirement{{"ci", ""}, {"ci", "test"}, {AIReview, ""}, {"release.v2", "build_all"}}
	if !slices.Equal(c.Requirements, want) {
		t.Fatalf("Requirements = %v，期望 %v", c.Requirements, want)
	}
	if c.Requirements[1].String() != "ci/test" {
		t.Errorf("String() = %q", c.Requirements[1].String())
	}
	for in, wantErr := range map[string]string{
		"required: [ci.yml]\n":    "不带扩展名",
		"required: [ci/test/x]\n": "应为 <workflow> 或 <workflow>/<job>",
		"required: [\"\"]\n":      "应为",
		"required: [ci, ci]\n":    "重复",
		"required: ci\n":          Path,
		"required: [ci/te st]\n":  "应为",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) err = %v，想要包含 %q", in, err, wantErr)
		}
	}
}

// Kiln 仓库自己的 .kiln/merge.yml：合并队列用 squash 合并，要求 CI 的测试和 AI 审计，审计通过后自动入队，合并后自动打 tag。
func TestRepoConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", Path))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Method != MethodSquash {
		t.Errorf("method %q，应为 %s", c.Method, MethodSquash)
	}
	if !slices.Contains(c.Required, "ci/test") || !slices.Contains(c.Required, AIReview) {
		t.Errorf("required %v，应包含 ci/test 和 %s", c.Required, AIReview)
	}
	// AI 审计通过后自动入队（PLAN #29.5）：关掉后需要手动加 kiln:merge
	if !c.Auto {
		t.Error("auto 应为 true：验证和 AI 审计通过后自动入队")
	}
	// Kiln 靠合并后自动打 tag 发版（PLAN #29.4）：删掉 tag 配置后合并不再打 tag、不再发布
	if c.Tag == nil || c.Tag.PrefixOr() != "v" || c.Tag.Initial != "0.1.0" {
		t.Errorf("tag %+v，应开启自动打 tag（prefix v，initial 0.1.0）", c.Tag)
	}
}

// TestParseAutoTag：auto 要求 required 中有 ai-review；tag 的前缀和初始版本的默认值与校验。
func TestParseAutoTag(t *testing.T) {
	c, err := Parse([]byte("auto: true\nrequired: [ci, ai-review]\ntag: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Auto || c.Tag == nil || c.Tag.PrefixOr() != "v" || c.Tag.Initial != "0.1.0" {
		t.Fatalf("Config = %+v（tag %+v）", c, c.Tag)
	}
	c, err = Parse([]byte("tag:\n  prefix: \"\"\n  initial: 1.0.0\n"))
	if err != nil || c.Tag.PrefixOr() != "" || c.Tag.Initial != "1.0.0" {
		t.Fatalf("空前缀：%+v, %v", c, err)
	}
	if c, _ := Parse([]byte("method: squash\n")); c.Tag != nil || c.Auto {
		t.Fatalf("没有 tag: 时不打 tag：%+v", c)
	}
	for in, wantErr := range map[string]string{
		"auto: true\n":                  "ai-review",
		"auto: true\nrequired: [ci]\n":  "ai-review",
		"tag:\n  prefix: ../v\n":        "tag.prefix",
		"tag:\n  prefix: -v\n":          "tag.prefix",
		"tag:\n  prefix: a..b\n":        "tag.prefix",
		"tag:\n  prefix: x.lock/v\n":    "tag.prefix",
		"tag:\n  prefix: \"v \"\n":      "tag.prefix",
		"tag:\n  prefix: \"v~\"\n":      "tag.prefix",
		"tag:\n  initial: 1.0\n":        "tag.initial",
		"tag:\n  initial: v1.0.0\n":     "tag.initial",
		"tag:\n  initial: 01.0.0\n":     "tag.initial",
		"tag:\n  initial: 1.0.0-rc.1\n": "tag.initial",
		// 每个数字最多 9 位（超出的话之后的计算解析不了已经打的 tag，独立审查发现）
		"tag:\n  initial: 9223372036854775808.0.0\n": "tag.initial",
		"tag:\n  initial: 1000000000.0.0\n":          "tag.initial",
		"tag:\n  prefx: v\n":                         "prefx",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) err = %v，想要包含 %q", in, err, wantErr)
		}
	}
	for _, ok := range []string{"cli/v", "release-", "v", "pkg.v"} {
		if _, err := Parse([]byte("tag:\n  prefix: " + ok + "\n")); err != nil {
			t.Errorf("前缀 %q 应合法：%v", ok, err)
		}
	}
}

// TestParseBranches（PLAN #28.5）：branches 省略时只允许默认分支；显式空列表、null、重复、超限、非法分支名都是错误。
func TestParseBranches(t *testing.T) {
	// 省略：只允许默认分支（100% 向后兼容）
	c, err := Parse([]byte("method: merge\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Branches) != 0 || !slices.Equal(c.AllowedBranches("main"), []string{"main"}) || !c.AllowsBranch("main", "main") ||
		c.AllowsBranch("dev", "main") {
		t.Fatalf("省略 branches：Branches %v，允许 %v", c.Branches, c.AllowedBranches("main"))
	}
	if Default().AllowsBranch("dev", "main") || !Default().AllowsBranch("main", "main") {
		t.Error("Default() 只允许默认分支")
	}
	if got := c.AllowedBranches(""); got != nil {
		t.Errorf("没有默认分支时应返回 nil：%v", got)
	}

	// 配置了 multiple
	c, err = Parse([]byte("branches: [master, dev]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.AllowedBranches("main"), []string{"master", "dev"}) || !c.AllowsBranch("dev", "main") ||
		c.AllowsBranch("main", "main") {
		t.Fatalf("branches: [master, dev]：%v", c.AllowedBranches("main"))
	}

	// 显式空列表 / null 必须报错：不能静默退回默认分支
	for in, wantErr := range map[string]string{
		"branches: []\n":                    "branches 不能是空列表",
		"branches: null\n":                  "branches 不能是空列表",
		"branches:\n":                       "branches 不能是空列表",
		"branches: [main, main]\n":          "重复",
		"branches: [\"\"]\n":                "不能为空",
		"branches: [refs/heads/main]\n":     "短分支名",
		"branches: [\"main \"]\n":           "空白",
		"branches: [\"a\\tb\"]\n":           "空白",
		"branches: [\"ma*in\"]\n":           "*",
		"branches: [\"a..b\"]\n":            "..",
		"branches: [\"a//b\"]\n":            "//",
		"branches: [\"a.lock\"]\n":          ".lock",
		"branches: [\"-foo\"]\n":            "开头",
		"branches: [\".foo\"]\n":            "开头",
		"branches: [\"foo.\"]\n":            "结尾",
		"branches: [\"foo/\"]\n":            "结尾",
		"branches: [\"a\"]\n":               "", // 合法
		"branches: [\"a/b\"]\n":             "", // 合法
		"tag:\n  branch: \"\"\n":            "tag.branch 不能是空串",
		"tag:\n  branch: null\n":            "tag.branch 不能是空串",
		"tag:\n  branch:\n":                 "tag.branch 不能是空串",
		"tag:\n  branch: refs/heads/main\n": "短分支名",
		"tag:\n  branch: \"a..b\"\n":        "..",
		"branches: [HEAD]\n":                "HEAD",
		"tag:\n  branch: HEAD\n":            "HEAD",
	} {
		_, err := Parse([]byte(in))
		if wantErr == "" {
			if err != nil {
				t.Errorf("Parse(%q) 应合法：%v", in, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) err = %v，想要包含 %q", in, err, wantErr)
		}
	}

	// 超过 64 项
	var b strings.Builder
	b.WriteString("branches: [")
	for i := range MaxBranches + 1 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "b%d", i)
	}
	b.WriteString("]\n")
	if _, err := Parse([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Errorf("超过 %d 项应报错：%v", MaxBranches, err)
	}
}

// TestParseTagBranch（PLAN #29.4）：tag.branch 只支持单个分支，与 branches 相互独立；BranchOr 的默认行为。
func TestParseTagBranch(t *testing.T) {
	c, err := Parse([]byte("branches: [dev]\ntag:\n  branch: master\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tag.BranchOr("main"); got != "master" {
		t.Errorf("BranchOr = %q，想要 master", got)
	}
	// 发版分支可以不在允许合并的分支中：队列只自动合 dev，master 上的发版由手动 PR 触发
	if !c.AllowsBranch("dev", "main") || c.AllowsBranch("master", "main") {
		t.Errorf("branches 应只允许 dev：%v", c.AllowedBranches("main"))
	}

	c, err = Parse([]byte("tag: {}\n"))
	if err != nil || c.Tag.Branch != nil || c.Tag.BranchOr("main") != "main" {
		t.Fatalf("省略 tag.branch 时按默认分支：%+v，%v", c, err)
	}
	if got := (*TagConfig)(nil).BranchOr("main"); got != "main" {
		t.Errorf("nil TagConfig 的 BranchOr = %q", got)
	}
}

// TestParseMergeKeysAndAliases：YAML 的合并键（`<<:`）和别名（`*alias`）不能绕过显式空值的校验——解码后它们是空列表 /
// 空串 / null，与省略无法区分，只看直接写出的键会静默退回默认分支；同级的显式键仍然优先于合并进来的值。
func TestParseMergeKeysAndAliases(t *testing.T) {
	for in, wantErr := range map[string]string{
		"branches: &e []\n":                             "branches 不能是空列表",
		"required: &e []\nbranches: *e\n":               "branches 不能是空列表",
		"<<: {branches: []}\n":                          "branches 不能是空列表",
		"<<: {branches: null}\n":                        "branches 不能是空列表",
		"tag: {branch: \"\"}\n":                         "tag.branch 不能是空串",
		"tag: {branch: null}\n":                         "tag.branch 不能是空串",
		"tag: {<<: {branch: \"\"}}\n":                   "tag.branch 不能是空串",
		"tag: {<<: {branch: null}}\n":                   "tag.branch 不能是空串",
		"tag: {prefix: &eb \"\", branch: *eb}\n":        "tag.branch 不能是空串",
		"tag: {initial: &eb \"\", <<: {branch: *eb}}\n": "tag.branch 不能是空串",
		"branches: [HEAD]\n":                            "HEAD",
		"tag: {branch: HEAD}\n":                         "HEAD",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) err = %v，想要包含 %q", in, err, wantErr)
		}
	}

	// 同级的显式键优先于合并进来的值：有效值是显式写出的那一个，应通过
	c, err := Parse([]byte("<<: {branches: []}\nbranches: [dev]\n"))
	if err != nil || !slices.Equal(c.Branches, []string{"dev"}) {
		t.Errorf("显式的 branches 应覆盖合并键：%+v，%v", c, err)
	}
	c, err = Parse([]byte("tag: {<<: {branch: \"\"}, branch: master}\n"))
	if err != nil || c.Tag.BranchOr("main") != "master" {
		t.Errorf("显式的 tag.branch 应覆盖合并键：%+v，%v", c, err)
	}
	// 合并键带来的有效值照常生效
	c, err = Parse([]byte("<<: {branches: [dev], tag: {branch: master}}\n"))
	if err != nil || !slices.Equal(c.Branches, []string{"dev"}) || c.Tag.BranchOr("main") != "master" {
		t.Errorf("合并键的有效值：%+v，%v", c, err)
	}
	// 合并键里的 tag.branch 照常解码
	c, err = Parse([]byte("tag: {<<: {branch: master}}\n"))
	if err != nil || c.Tag.BranchOr("main") != "master" {
		t.Errorf("合并键里的 tag.branch：%+v，%v", c, err)
	}
}

// TestParseCyclicAlias：循环别名（`tag: &t {<<: *t}`、`&a {<<: *a}`）必须返回错误，绝不因为合并键的递归展开而栈溢出。
// 结构体解码先做（yaml.v3 报"anchor 包含自己"），explicitKeys 的节点遍历也各自设限，两条路径都不能 panic。
func TestParseCyclicAlias(t *testing.T) {
	for _, in := range []string{
		"tag: &t {<<: *t}\n",
		"&a {<<: *a}\n",
		"tag: &t\n  <<: *t\n",
		"tag: &t {<<: [*t]}\n",
	} {
		t.Run(in, func(t *testing.T) {
			// 深层的循环别名：展开次数或层数超过上限时报错，不会无限递归
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatalf("Parse(%q) 应返回错误（循环别名）", in)
			}
			if _, err := explicitKeys([]byte(in)); err == nil {
				t.Fatalf("explicitKeys(%q) 应返回错误（循环别名/展开超限）", in)
			}
		})
	}
}

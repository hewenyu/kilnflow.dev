// Package mergecfg 解析仓库的合并配置 .kiln/merge.yml（PLAN #28.5）。配置从默认分支读取，PR 不能修改自己的合并方式；
// 文件可以不存在（全部使用默认值）。
package mergecfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// HoldLabel 是 PR 上表示不自动入队的 label（手动入队不受影响，PLAN #29.5）。AI 开发改动了敏感路径时给 PR 加上它。
const HoldLabel = "kiln:hold"

// Path 是配置文件在仓库中的路径。
const Path = ".kiln/merge.yml"

// MaxConfigSize 是配置文件的大小上限。
const MaxConfigSize = 16 << 10

// MaxBranches 是 branches 最多的项数。
const MaxBranches = 64

// maxBranchNameLen 是分支名的长度上限（与 git 的限制相同）。
const maxBranchNameLen = 255

// 合并方式（与 GitHub 合并接口的 merge_method 相同）。
const (
	MethodSquash = "squash"
	MethodMerge  = "merge"
	MethodRebase = "rebase"
)

// AIReview 是 required 中表示 AI 审计的名字（AI 审计的 run 不来自 .kiln/workflows 中的文件）。
const AIReview = "ai-review"

// Config 是 .kiln/merge.yml。
type Config struct {
	// Method 是合并方式，默认 squash。
	Method string `yaml:"method"`
	// Required 是合并前必须存在并成功的验证（PLAN #28.3）：<workflow> 或 <workflow>/<job>。workflow 是
	// .kiln/workflows 中的文件名去掉 .yml / .yaml（AI 审计为 ai-review），job 是 workflow 中的 job ID（矩阵的全部变体）。
	// 从默认分支读取：PR 删掉或改名必需的 workflow、job 时不能合并。
	Required []string `yaml:"required"`
	// Requirements 是解析后的 Required。
	Requirements []Requirement `yaml:"-"`
	// Auto 为真时，PR 的 head 验证全部通过、AI 审计无问题时自动入队（PLAN #29.5）。要求 required 中有 ai-review：
	// 自动入队靠 AI 审计放行，必须保证合并之前审计存在并通过。
	Auto bool `yaml:"auto"`
	// Branches 是允许合并队列 / 自动入队的目标分支（PLAN #28.5）：短分支名，大小写敏感，精确匹配，不支持通配符。
	// 省略时只允许仓库默认分支（完全向后兼容）。显式写空列表或 null 是配置错误：维护者本想关闭队列、却静默退回默认
	// 分支时，PR 会被意外合并。与 tag.branch 相互独立：发版分支可以不在允许合并的分支中。
	Branches []string `yaml:"branches"`
	// Tag 非空时，经过 PR 合入发版分支的 commit 由 Server 打 tag 并发布（PLAN #29.4）。
	Tag *TagConfig `yaml:"tag"`
}

// TagConfig 是自动打 tag 的规则（.kiln/merge.yml 的 tag:）。
type TagConfig struct {
	// Prefix 是 tag 名的前缀，默认 v（显式写 "" 表示没有前缀）。
	Prefix *string `yaml:"prefix"`
	// Initial 是还没有版本 tag 时的第一个版本，默认 0.1.0。
	Initial string `yaml:"initial"`
	// Branch 是发版分支（PLAN #29.4）：只有合入这个分支的 PR 才自动打 tag。省略时是仓库在合并那一刻的默认分支。
	// 只支持单个分支（没有 tag.branches）：多个分支共用同一个版本号序列时，各自的提交可能互不相干，按 T3 的祖先校验会
	// 互相跳过。显式写空串或 null 是配置错误。
	Branch *string `yaml:"branch"`
}

// 自动打 tag 的默认值。
const (
	DefaultTagPrefix  = "v"
	DefaultTagInitial = "0.1.0"
)

// PrefixOr 返回 tag 前缀（没有写时为默认的 v）。
func (t *TagConfig) PrefixOr() string {
	if t.Prefix == nil {
		return DefaultTagPrefix
	}
	return *t.Prefix
}

// BranchOr 返回发版分支：显式配置时是 tag.branch，否则是 defaultBranch（仓库在合并那一刻的默认分支，PLAN #29.4）。
func (t *TagConfig) BranchOr(defaultBranch string) string {
	if t != nil && t.Branch != nil {
		return *t.Branch
	}
	return defaultBranch
}

// AllowedBranches 返回允许合并队列 / 自动入队的目标分支（PLAN #28.5）：配置了 branches 时是它，否则只有仓库默认分支。
// 合并队列和自动入队都只通过它判断，保证两处的规则一致。
func (c *Config) AllowedBranches(defaultBranch string) []string {
	if c != nil && len(c.Branches) > 0 {
		return c.Branches
	}
	if defaultBranch == "" {
		return nil
	}
	return []string{defaultBranch}
}

// AllowsBranch 判断 branch 是否允许合并队列 / 自动入队。
func (c *Config) AllowsBranch(branch, defaultBranch string) bool {
	return slices.Contains(c.AllowedBranches(defaultBranch), branch)
}

// checkBranchName 校验短分支名（PLAN #28.5）：与 git check-ref-format 的规则一致，但不做隐式的 TrimSpace（空白会被
// 明确拒绝，而不是悄悄改成另一个分支名）。prefix 是配置中的字段名，用于报错。
func checkBranchName(prefix, name string) error {
	bad := func(why string) error { return fmt.Errorf("%s：%s 的 %q %s", Path, prefix, name, why) }
	if name == "" {
		return bad("不能为空")
	}
	if name == "HEAD" {
		// 与 git check-ref-format --branch HEAD 一致：HEAD 是保留名，不是普通分支
		return bad("不能是保留名 HEAD")
	}
	if len(name) > maxBranchNameLen {
		return bad(fmt.Sprintf("超过 %d 个字节", maxBranchNameLen))
	}
	if strings.HasPrefix(name, "refs/") {
		return bad("要写短分支名（不带 refs/heads/）")
	}
	if name == "@" {
		return bad("不能是 @")
	}
	if strings.ContainsRune("/.-", rune(name[0])) {
		return bad("不能以 /、. 或 - 开头")
	}
	if strings.ContainsRune("/.", rune(name[len(name)-1])) {
		return bad("不能以 / 或 . 结尾")
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f {
			return bad("不能包含空白或控制字符")
		}
	}
	for _, ch := range []string{"..", "//", "@{", "\\", "~", "^", ":", "?", "*", "["} {
		if strings.Contains(name, ch) {
			return bad("不能包含 " + ch)
		}
	}
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return bad("不能有以 . 开头的路径段")
		}
		if strings.HasSuffix(seg, ".lock") {
			return bad("不能有以 .lock 结尾的路径段")
		}
	}
	return nil
}

// parseBranches 校验 branches：项数、重复、分支名。
func parseBranches(items []string) error {
	if len(items) > MaxBranches {
		return fmt.Errorf("%s：branches 最多 %d 项（实际 %d 项）", Path, MaxBranches, len(items))
	}
	seen := make(map[string]bool, len(items))
	for _, b := range items {
		if err := checkBranchName("branches", b); err != nil {
			return err
		}
		if seen[b] {
			return fmt.Errorf("%s：branches 中 %q 重复", Path, b)
		}
		seen[b] = true
	}
	return nil
}

// versionRE 是形如 X.Y.Z 的正式版本（数字没有多余的前导 0，每个数字最多 9 位：解析成 int 不会溢出，与 Runner 升级用的
// 版本号范围相同）。
var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// IsVersion 判断 s 是否是形如 X.Y.Z 的正式版本（每个数字最多 9 位）。
func IsVersion(s string) bool { return versionRE.MatchString(s) }

// prefixRE 是 tag 前缀允许的字符（拼上版本号后是合法的 tag 名，规则同 git check-ref-format）。
var prefixRE = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

// checkTagPrefix 校验 tag 前缀：拼上 X.Y.Z 之后是合法的 tag 名。
func checkTagPrefix(p string) error {
	bad := len(p) > 64 || !prefixRE.MatchString(p) || strings.Contains(p, "..") || strings.Contains(p, "//") ||
		strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") || strings.HasPrefix(p, "-")
	for seg := range strings.SplitSeq(p, "/") {
		bad = bad || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock")
	}
	if bad {
		return fmt.Errorf("%s：tag.prefix %q 拼上版本号不是合法的 tag 名（只能用字母、数字、. _ / -，不能以 / . - 开头，不能有 .. 或 //）", Path, p)
	}
	return nil
}

// Requirement 是一项必需的验证。Job 为空表示整个 workflow 的 run 成功即可。
type Requirement struct {
	Workflow string
	Job      string
}

func (r Requirement) String() string {
	if r.Job == "" {
		return r.Workflow
	}
	return r.Workflow + "/" + r.Job
}

// requirementRE 是 required 的一项：workflow 文件名（不含扩展名）和可选的 job ID。
var requirementRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+)(?:/([A-Za-z0-9_-]+))?$`)

// parseRequirements 校验 required：格式正确、没有重复、没有写扩展名。
func parseRequirements(items []string) ([]Requirement, error) {
	var out []Requirement
	seen := map[string]bool{}
	for _, it := range items {
		m := requirementRE.FindStringSubmatch(it)
		if m == nil {
			return nil, fmt.Errorf("%s：required 的 %q 应为 <workflow> 或 <workflow>/<job>", Path, it)
		}
		if strings.HasSuffix(m[1], ".yml") || strings.HasSuffix(m[1], ".yaml") {
			return nil, fmt.Errorf("%s：required 的 %q 中 workflow 不带扩展名（写 %s）", Path, it, strings.TrimSuffix(strings.TrimSuffix(it, ".yml"), ".yaml"))
		}
		if seen[it] {
			return nil, fmt.Errorf("%s：required 中 %q 重复", Path, it)
		}
		seen[it] = true
		out = append(out, Requirement{Workflow: m[1], Job: m[2]})
	}
	return out, nil
}

// Default 返回文件不存在时的配置。
func Default() *Config { return &Config{Method: MethodSquash} }

// Parse 解析并校验配置。空文件表示全部使用默认值；未知字段、多个 YAML 文档是错误。
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxConfigSize {
		return nil, fmt.Errorf("%s 超过 %d KiB", Path, MaxConfigSize>>10)
	}
	// 先由 yaml.v3 的结构体解码完成语法、未知字段和循环别名的校验：它会报"anchor 包含自己"，而下面的 explicitKeys 直接
	// 遍历节点树，必须排在它之后，绝不能让恶意配置在更早的阶段进入递归展开。
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s：%w", Path, err)
	}
	// 与 workflow、AI 配置一致：之后的文档（包括空文档）和语法错误不能被静默忽略
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("%s：%w", Path, err)
		}
		return nil, fmt.Errorf("%s 只能包含一个 YAML 文档", Path)
	}
	// 再看原始文档里显式写了哪些键：branches、tag.branch 的 null 与省略解码后都是零值，必须区分（见字段说明）。
	explicit, err := explicitKeys(data)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", Path, err)
	}
	switch c.Method {
	case "":
		c.Method = MethodSquash
	case MethodSquash, MethodMerge, MethodRebase:
	default:
		return nil, fmt.Errorf("%s：method 只能是 %s、%s 或 %s，不能是 %q", Path, MethodSquash, MethodMerge, MethodRebase, c.Method)
	}
	reqs, err := parseRequirements(c.Required)
	if err != nil {
		return nil, err
	}
	c.Requirements = reqs
	if c.Auto && !slices.Contains(c.Required, AIReview) {
		return nil, fmt.Errorf("%s：auto: true 需要在 required 中包含 %s（自动入队靠 AI 审计放行，合并前审计必须存在并通过）", Path, AIReview)
	}
	if explicit.emptyBranches {
		return nil, fmt.Errorf("%s：branches 不能是空列表或 null；只想允许仓库默认分支时不写 branches", Path)
	}
	if err := parseBranches(c.Branches); err != nil {
		return nil, err
	}
	if t := c.Tag; t != nil {
		if explicit.emptyTagBranch {
			return nil, fmt.Errorf("%s：tag.branch 不能是空串或 null；不限制发版分支时不写 tag.branch", Path)
		}
		if err := checkTagPrefix(t.PrefixOr()); err != nil {
			return nil, err
		}
		if t.Branch != nil {
			if err := checkBranchName("tag.branch", *t.Branch); err != nil {
				return nil, err
			}
		}
		if t.Initial == "" {
			t.Initial = DefaultTagInitial
		}
		if !IsVersion(t.Initial) {
			return nil, fmt.Errorf("%s：tag.initial %q 应为 X.Y.Z（每个数字最多 9 位）", Path, t.Initial)
		}
	}
	return &c, nil
}

// explicit 记录原始文档中显式写出的、需要区分"省略"与"空值"的键。
type explicit struct {
	// emptyBranches 表示显式写了 branches: [] 或 branches: null。
	emptyBranches bool
	// emptyTagBranch 表示显式写了 tag.branch: "" 或 tag.branch: null。
	emptyTagBranch bool
}

// YAML 结构的上限：合并键（`<<:`）的展开是递归的，循环别名（例如 `tag: &t {<<: *t}`、`&a {<<: *a}`）会让它永不终止。
// yaml.v3 解码配置结构时会报"anchor 包含自己"，但 explicitKeys 直接遍历节点树，必须自己设限，否则恶意配置能让进程栈溢出。
const (
	// maxYAMLDepth 是合并键展开的嵌套层数上限（正常的 merge.yml 只有两三层）。
	maxYAMLDepth = 32
	// maxYAMLExpansions 是一次解析中展开的合并键总数上限（文档本身有 16 KiB 上限）。
	maxYAMLExpansions = 1024
)

// explicitKeys 读取原始文档，找出 branches、tag.branch 是否显式写了空值（解码后与省略都是零值，无法区分）。
//
// 只看映射里直接写出的键不够：YAML 的合并键（`<<:`）和别名（`*alias`）同样把值带进有效配置，`<<: {branches: []}`、
// `branches: *anchoredEmptyList` 这些写法会被解码成空列表，不能静默绕过"显式空值是配置错误"的检查。这里先按 YAML 的
// 合并语义求出有效的键值（解引用别名、展开合并键，同级的显式键优先），再判断。
func explicitKeys(data []byte) (explicit, error) {
	var out explicit
	var doc yaml.Node
	err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc)
	if errors.Is(err, io.EOF) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return out, nil
		}
		root = root.Content[0]
	}
	// 解析循环别名或过深的合并键时返回错误，绝不在递归里栈溢出
	w := &yamlWalker{visiting: make(map[*yaml.Node]bool)}
	m, err := w.resolveMapping(root, 0)
	if err != nil {
		return out, err
	}
	if m == nil {
		return out, nil
	}
	if v, ok := m["branches"]; ok {
		out.emptyBranches = isEmptyValue(v)
	}
	if tag, ok := m["tag"]; ok {
		tm, err := w.resolveMapping(tag, 0)
		if err != nil {
			return out, err
		}
		if tm != nil {
			if v, ok := tm["branch"]; ok {
				out.emptyTagBranch = isEmptyValue(v)
			}
		}
	}
	return out, nil
}

// deref 返回别名指向的节点（不是别名时返回自身）。别名链有环或过长时返回 nil（调用方按"没有这个值"保守处理）：
// 正常的 YAML 别名指向锚定的节点（不是别名），这里的检查只用于挡住非法输入。
func deref(n *yaml.Node) *yaml.Node {
	var seen map[*yaml.Node]bool
	for n != nil && n.Kind == yaml.AliasNode {
		if seen == nil {
			seen = make(map[*yaml.Node]bool, 1)
		}
		if seen[n] {
			return nil
		}
		seen[n] = true
		n = n.Alias
	}
	return n
}

// yamlWalker 展开合并键时维护的环检测集合和展开计数。
type yamlWalker struct {
	// visiting 是当前递归路径上的 mapping 节点：同一个节点再次出现说明合并键形成了环。
	visiting   map[*yaml.Node]bool
	expansions int
}

// resolveMapping 返回 YAML mapping 的有效键值：解引用别名、展开 `<<` 合并键。同级的显式键优先于合并进来的键；序列形式的
// `<<` 按顺序合并，前面的映射优先（YAML 的合并键语义）。n 解引用后不是 mapping 时返回 nil。递归遇到循环别名、超过
// 层数或展开次数上限时返回错误（不栈溢出）。
func (w *yamlWalker) resolveMapping(n *yaml.Node, depth int) (map[string]*yaml.Node, error) {
	if depth > maxYAMLDepth {
		return nil, fmt.Errorf("YAML 合并键嵌套超过 %d 层（可能是循环别名）", maxYAMLDepth)
	}
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	if w.visiting[n] {
		return nil, errors.New("YAML 存在循环别名")
	}
	w.visiting[n] = true
	defer delete(w.visiting, n)
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "<<" {
			merges = append(merges, v)
			continue
		}
		if _, ok := out[k.Value]; !ok {
			out[k.Value] = v
		}
	}
	// 显式键已经收齐，合并进来的同名键不覆盖它们
	add := func(m map[string]*yaml.Node) {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	for _, mv := range merges {
		w.expansions++
		if w.expansions > maxYAMLExpansions {
			return nil, fmt.Errorf("YAML 合并键展开超过 %d 次（可能是循环别名）", maxYAMLExpansions)
		}
		mv = deref(mv)
		if mv == nil {
			continue
		}
		switch mv.Kind {
		case yaml.MappingNode:
			sub, err := w.resolveMapping(mv, depth+1)
			if err != nil {
				return nil, err
			}
			add(sub)
		case yaml.SequenceNode:
			for _, item := range mv.Content {
				sub, err := w.resolveMapping(item, depth+1)
				if err != nil {
					return nil, err
				}
				add(sub)
			}
		}
	}
	return out, nil
}

// isEmptyValue 判断节点是否是空值：null（包括只写了键、没有值）或者空的字符串、列表。别名先解引用。
func isEmptyValue(n *yaml.Node) bool {
	n = deref(n)
	if n == nil {
		return true
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Tag == "!!null" || (n.Tag == "!!str" && n.Value == "")
	case yaml.SequenceNode:
		return len(n.Content) == 0
	}
	return false
}

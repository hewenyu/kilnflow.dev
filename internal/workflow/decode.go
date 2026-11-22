package workflow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAML 标签。
const (
	tagNull  = "!!null"
	tagBool  = "!!bool"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagMerge = "!!merge"
)

// decoder 把 YAML 节点解码成 Workflow，并收集带位置的诊断。
type decoder struct {
	file  string
	diags Diags
}

// 手动解码 yaml.Node 不会经过 yaml.v3 的别名循环和展开量检查，先限制整个节点图，
// 避免后续 toValue 等递归解码在循环别名、深层嵌套或指数展开时耗尽栈和内存。
const (
	maxYAMLDepth         = 100
	maxExpandedYAMLNodes = 100_000
)

func (d *decoder) validateNodes(root *yaml.Node) bool {
	active := map[*yaml.Node]bool{}
	remaining := maxExpandedYAMLNodes
	var visit func(*yaml.Node, int) bool
	visit = func(n *yaml.Node, depth int) bool {
		if n == nil {
			return true
		}
		if active[n] {
			d.errAt(n, "YAML 别名形成循环")
			return false
		}
		if depth > maxYAMLDepth {
			d.errAt(n, "YAML 嵌套超过 %d 层", maxYAMLDepth)
			return false
		}
		if remaining == 0 {
			d.errAt(n, "YAML 展开后超过 %d 个节点", maxExpandedYAMLNodes)
			return false
		}
		remaining--
		active[n] = true
		defer delete(active, n)
		if n.Kind == yaml.AliasNode {
			return visit(n.Alias, depth+1)
		}
		for _, child := range n.Content {
			if !visit(child, depth+1) {
				return false
			}
		}
		return true
	}
	return visit(root, 0)
}

// handlers 是映射中各字段的解析函数，k 是键节点，v 是（已解开别名的）值节点。
type handlers map[string]func(k, v *yaml.Node)

func (d *decoder) errAt(n *yaml.Node, format string, args ...any) {
	dg := Diag{File: d.file, Msg: fmt.Sprintf(format, args...)}
	if n != nil {
		dg.Line, dg.Col = n.Line, n.Column
	}
	d.diags = append(d.diags, dg)
}

func pos(n *yaml.Node) Pos { return Pos{Line: n.Line, Col: n.Column} }

// resolve 解开别名和文档节点。
func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return resolve(n.Content[0])
	}
	return n
}

func isNull(n *yaml.Node) bool {
	n = resolve(n)
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == tagNull)
}

// nullNode 返回一个 null 节点，用于没有内容的字段（例如 on: push）。
func nullNode() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagNull} }

// fields 遍历映射节点，未知字段报错；hints 中的字段报告为不支持并给出说明。
func (d *decoder) fields(n *yaml.Node, what string, hs handlers, hints map[string]string) {
	n = resolve(n)
	if n == nil || isNull(n) {
		return
	}
	if n.Kind != yaml.MappingNode {
		d.errAt(n, "%s 应为映射", what)
		return
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolve(n.Content[i+1])
		key := k.Value
		if k.Tag == tagMerge {
			d.errAt(k, "不支持 YAML 合并键 <<")
			continue
		}
		if !d.uniqueKey(seen, k, what) {
			continue
		}
		h, ok := hs[key]
		if !ok {
			if hint, ok := hints[key]; ok {
				d.errAt(k, "%s 不支持字段 %s：%s", what, key, hint)
			} else {
				d.errAt(k, "%s 中有未知字段 %s", what, key)
			}
			continue
		}
		h(k, v)
	}
}

// uniqueKey 记录映射的键 k，重复时报错并返回 false。yaml.v3 解码到 yaml.Node 时不检查重复的键，
// 不检查时后写的值会静默覆盖先写的（Actions 对重复的键报错）。
func (d *decoder) uniqueKey(seen map[string]bool, k *yaml.Node, what string) bool {
	if seen[k.Value] {
		d.errAt(k, "%s 中字段 %s 重复", what, k.Value)
		return false
	}
	seen[k.Value] = true
	return true
}

// pairs 遍历映射节点的键值对（重复的键报错并跳过）；不是映射时报错。
func (d *decoder) pairs(n *yaml.Node, what string, f func(k, v *yaml.Node)) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		d.errAt(n, "%s 应为映射", what)
		return
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if d.uniqueKey(seen, n.Content[i], what) {
			f(n.Content[i], n.Content[i+1])
		}
	}
}

// parseSeq 用 item 解析列表的每一项；不是列表时报错并返回 nil。空列表也返回 nil。
func parseSeq[T any](d *decoder, n *yaml.Node, what string, item func(c *yaml.Node) T) []T {
	n = resolve(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		d.errAt(n, "%s 应为列表", what)
		return nil
	}
	var out []T
	for _, c := range n.Content {
		out = append(out, item(c))
	}
	return out
}

func (d *decoder) str(n *yaml.Node) string {
	n = resolve(n)
	if n == nil || isNull(n) {
		return ""
	}
	if n.Kind != yaml.ScalarNode {
		d.errAt(n, "应为字符串")
		return ""
	}
	return n.Value
}

// strList 解析字符串或字符串列表；没有写时返回 nil。
func (d *decoder) strList(n *yaml.Node) []string {
	n = resolve(n)
	if n == nil || isNull(n) {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	if n.Kind != yaml.SequenceNode {
		d.errAt(n, "应为字符串或字符串列表")
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, d.str(c))
	}
	return out
}

func (d *decoder) boolv(n *yaml.Node) bool {
	n = resolve(n)
	if n == nil || isNull(n) {
		return false
	}
	switch strings.ToLower(n.Value) {
	case "true":
		return true
	case "false":
		return false
	}
	d.errAt(n, "应为 true 或 false")
	return false
}

// boolp 解析布尔值并返回指针，用于需要区分"没有写"的字段。
func (d *decoder) boolp(n *yaml.Node) *bool {
	b := d.boolv(n)
	return &b
}

// strMap 解析值都是字符串的映射（保留顺序）。
func (d *decoder) strMap(n *yaml.Node, what string) *OrderedMap {
	n = resolve(n)
	m := NewOrderedMap()
	if n == nil || isNull(n) {
		return m
	}
	if n.Kind != yaml.MappingNode {
		d.errAt(n, "%s 应为映射", what)
		return m
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolve(n.Content[i+1])
		if !d.uniqueKey(seen, k, what) {
			continue
		}
		if v.Kind != yaml.ScalarNode {
			d.errAt(v, "%s.%s 应为字符串", what, k.Value)
			continue
		}
		val := v.Value
		if isNull(v) {
			val = ""
		}
		m.Set(k.Value, val)
	}
	return m
}

// toValue 把 YAML 节点转换成表达式可用的值。
func toValue(n *yaml.Node) any {
	n = resolve(n)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return scalarValue(n)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			out = append(out, toValue(c))
		}
		return out
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = toValue(n.Content[i+1])
		}
		return m
	}
	return nil
}

// scalarValue 按 YAML 标签转换标量：null、布尔、数字（统一为 float64），其余为字符串。
func scalarValue(n *yaml.Node) any {
	switch n.Tag {
	case tagNull:
		return nil
	case tagBool:
		return strings.EqualFold(n.Value, "true")
	case tagInt, tagFloat:
		if f, err := strconv.ParseFloat(n.Value, 64); err == nil {
			return f
		}
		if i, err := strconv.ParseInt(n.Value, 0, 64); err == nil {
			return float64(i)
		}
	}
	return n.Value
}

var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// yamlErrLine 从 yaml.v3 的错误信息中取出行号；没有时返回 0。
func yamlErrLine(err error) int {
	m := yamlLineRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

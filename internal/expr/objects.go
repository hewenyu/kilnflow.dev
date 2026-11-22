package expr

import (
	"fmt"
	"maps"
	"slices"
)

// Strict 是访问不存在的属性时报错的对象（例如 github）。
type Strict struct {
	Name   string
	Fields map[string]any
}

// Get 返回字段的值；字段不存在时报错。
func (s Strict) Get(key string) (any, error) {
	if v, ok := s.Fields[key]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("不支持的字段 %s.%s", s.Name, key)
}

// JSON 返回可以 JSON 序列化的值（用于 toJSON 和插值）。
func (s Strict) JSON() any {
	m := make(map[string]any, len(s.Fields))
	for k, v := range s.Fields {
		m[k] = normalize(v)
	}
	return m
}

// Lenient 是访问不存在的属性时返回空字符串的对象（例如 vars、inputs）。
type Lenient map[string]any

// Get 返回字段的值；字段不存在时返回空字符串。
func (l Lenient) Get(key string) (any, error) {
	if v, ok := l[key]; ok {
		return v, nil
	}
	return "", nil
}

// JSON 返回可以 JSON 序列化的值（用于 toJSON 和插值）。
func (l Lenient) JSON() any {
	m := make(map[string]any, len(l))
	for k, v := range l {
		m[k] = normalize(v)
	}
	return m
}

// Placeholder 对任意属性返回 "${{ <prefix>.<name> }}"，并记录被引用的名字。
// 用于 secrets：规划时不展开，由 Runner 在执行时替换。
type Placeholder struct {
	Prefix string
	Used   map[string]bool
}

// Get 记录 key 被引用，并返回占位符。key 只能包含字母、数字和 _（与 secret 名称的规则相同）：
// 占位符由 Runner 按 "${{ secrets.<名称> }}" 原样替换，secrets['a }} b'] 这样的名字会生成无法替换、
// 甚至改变占位符边界的文本。
func (p *Placeholder) Get(key string) (any, error) {
	if !validPlaceholderName(key) {
		return nil, fmt.Errorf("%s 的名称 %q 无效：只能包含字母、数字和 _", p.Prefix, key)
	}
	if p.Used != nil {
		p.Used[key] = true
	}
	return exprOpen + " " + p.Prefix + "." + key + " " + exprClose, nil
}

// Names 返回已排序的被引用名字。
func (p *Placeholder) Names() []string {
	return slices.Sorted(maps.Keys(p.Used))
}

// validPlaceholderName 判断名字非空且只包含 ASCII 字母、数字和 _。
func validPlaceholderName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

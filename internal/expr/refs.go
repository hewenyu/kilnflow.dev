package expr

import (
	"slices"
	"strings"
)

// refs 返回表达式中以标识符开头的属性访问链，例如 "secrets.NPM_TOKEN"、"matrix.os"、"github"。
func refs(n Node) []string {
	var out []string
	var walk func(n Node)
	walk = func(n Node) {
		switch x := n.(type) {
		case ident, property, index:
			if c, ok := chain(x, walk); ok {
				out = append(out, c)
				return
			}
			switch y := x.(type) {
			case property:
				walk(y.target)
			case index:
				walk(y.target)
				walk(y.key)
			}
		case unary:
			walk(x.x)
		case binary:
			walk(x.l)
			walk(x.r)
		case call:
			for _, a := range x.args {
				walk(a)
			}
		}
	}
	walk(n)
	return out
}

// chain 把 a.b['c'].d 这样的访问链还原成 "a.b.c.d"。下标不是字符串字面量时，
// 先用 walk 分析下标表达式本身，再丢掉已收集的后缀（只保留下标之前的部分）。
func chain(n Node, walk func(Node)) (string, bool) {
	var parts []string
	for {
		switch x := n.(type) {
		case ident:
			parts = append(parts, x.name)
			slices.Reverse(parts)
			return strings.Join(parts, "."), true
		case property:
			parts = append(parts, x.name)
			n = x.target
		case index:
			if l, ok := x.key.(literal); ok {
				if s, ok := l.v.(string); ok {
					parts = append(parts, s)
					n = x.target
					continue
				}
			}
			walk(x.key)
			parts = parts[:0]
			n = x.target
		default:
			return "", false
		}
	}
}

// StringRefs 返回字符串中所有 ${{ }} 的引用。
func StringRefs(s string) ([]string, error) {
	if !HasExpr(s) {
		return nil, nil
	}
	segs, err := split(s)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, seg := range segs {
		if seg.Expr == "" {
			continue
		}
		n, err := Parse(seg.Expr)
		if err != nil {
			return nil, err
		}
		out = append(out, refs(n)...)
	}
	return out, nil
}

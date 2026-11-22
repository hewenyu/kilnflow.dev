package expr

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// Node 是表达式语法树的节点。
type Node interface{ node() }

type (
	literal  struct{ v any }
	ident    struct{ name string }
	index    struct{ target, key Node }
	property struct {
		target Node
		name   string
	}
	unary struct {
		op string
		x  Node
	}
	binary struct {
		op   string
		l, r Node
	}
	call struct {
		name string
		args []Node
	}
)

func (literal) node()  {}
func (ident) node()    {}
func (index) node()    {}
func (property) node() {}
func (unary) node()    {}
func (binary) node()   {}
func (call) node()     {}

// maxDepth 是表达式的嵌套层数上限（括号、!、下标、函数参数）。递归下降解析，
// 不限制时恶意的深层嵌套（例如几十万个 "("）会耗尽栈空间，使进程崩溃。
const maxDepth = 100

// maxLength 是单个表达式的长度上限（字节，与 Actions 相同）。a.b.b… 的属性访问链和 a || a || … 的二元运算链
// 在解析时是循环，不计入 maxDepth，但语法树的深度与长度成正比，求值和引用分析递归遍历语法树；
// 不限制长度时几 MB 的表达式就能耗尽栈空间，使进程崩溃（无法 recover）。
const maxLength = 21000

// errExprLen 是超长表达式在错误信息中保留的字节数。
const errExprLen = 64

type parser struct {
	toks  []token
	pos   int
	depth int
}

// enter 进入一层嵌套，超过上限时报错；返回的函数退出这一层。
func (p *parser) enter() (func(), error) {
	if p.depth >= maxDepth {
		return nil, fmt.Errorf("表达式嵌套超过 %d 层", maxDepth)
	}
	p.depth++
	return func() { p.depth-- }, nil
}

// Parse 解析一个表达式（不含 ${{ }}）。
func Parse(src string) (Node, error) {
	if len(src) > maxLength {
		// 错误信息中不带整个表达式
		return nil, &Error{Expr: shorten.Text(src, errExprLen), Msg: fmt.Sprintf("表达式超过 %d 字节", maxLength)}
	}
	toks, err := lex(src)
	if err != nil {
		return nil, &Error{Expr: src, Msg: err.Error()}
	}
	p := &parser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, &Error{Expr: src, Msg: err.Error()}
	}
	if p.peek().kind != tEOF {
		return nil, &Error{Expr: src, Msg: "表达式末尾有多余内容"}
	}
	return n, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

// peekOp 判断下一个符号是否是 ops 中的某个运算符。
func (p *parser) peekOp(ops ...string) bool {
	t := p.peek()
	return t.kind == tOp && slices.Contains(ops, t.s)
}

// parseBinary 解析左结合的二元运算：operand (op operand)*。
func (p *parser) parseBinary(operand func() (Node, error), ops ...string) (Node, error) {
	l, err := operand()
	if err != nil {
		return nil, err
	}
	for p.peekOp(ops...) {
		op := p.next().s
		r, err := operand()
		if err != nil {
			return nil, err
		}
		l = binary{op: op, l: l, r: r}
	}
	return l, nil
}

// 优先级从低到高：|| → && → == != → < <= > >= → ! → 属性访问。

func (p *parser) parseOr() (Node, error) {
	leave, err := p.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return p.parseBinary(p.parseAnd, opOr)
}
func (p *parser) parseAnd() (Node, error) { return p.parseBinary(p.parseEq, opAnd) }
func (p *parser) parseEq() (Node, error)  { return p.parseBinary(p.parseCmp, opEq, opNe) }
func (p *parser) parseCmp() (Node, error) {
	return p.parseBinary(p.parseUnary, opLt, opLe, opGt, opGe)
}

func (p *parser) parseUnary() (Node, error) {
	if p.peekOp(opNot) {
		p.next()
		leave, err := p.enter()
		if err != nil {
			return nil, err
		}
		defer leave()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return unary{op: opNot, x: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case tDot:
			p.next()
			t := p.next()
			if t.kind != tIdent {
				return nil, errors.New("'.' 后面应为属性名")
			}
			n = property{target: n, name: t.s}
		case tLBrack:
			p.next()
			k, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if p.next().kind != tRBrack {
				return nil, errors.New("缺少 ']'")
			}
			n = index{target: n, key: k}
		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (Node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return literal{v: t.num}, nil
	case tStr:
		return literal{v: t.s}, nil
	case tLParen:
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.next().kind != tRParen {
			return nil, errors.New("缺少 ')'")
		}
		return n, nil
	case tIdent:
		switch t.s {
		case "true":
			return literal{v: true}, nil
		case "false":
			return literal{v: false}, nil
		case "null":
			return literal{v: nil}, nil
		}
		if p.peek().kind == tLParen {
			p.next()
			return p.parseCall(t.s)
		}
		return ident{name: t.s}, nil
	case tEOF:
		return nil, errors.New("表达式不完整")
	default:
		return nil, errors.New("意外的符号")
	}
}

// parseCall 解析函数调用的参数列表（左括号已读过）。
func (p *parser) parseCall(name string) (Node, error) {
	var args []Node
	if p.peek().kind != tRParen {
		for {
			a, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if p.peek().kind != tComma {
				break
			}
			p.next()
		}
	}
	if p.next().kind != tRParen {
		return nil, fmt.Errorf("函数 %s 的参数缺少 ')'", name)
	}
	return call{name: name, args: args}, nil
}

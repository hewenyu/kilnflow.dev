// Package expr 实现 GitHub Actions 表达式的子集（PLAN #4.7）：
// 字面量、运算符 == != < <= > >= && || ! 和括号、属性访问，
// 以及函数 contains、startsWith、endsWith、format、join、toJSON、fromJSON。
// 不支持 hashFiles 和状态函数（success()、failure() 等）。
//
// 代码按阶段分在几个文件中：lex.go（词法）、parse.go（语法）、eval.go（求值）、
// funcs.go（内置函数）、interp.go（${{ }} 插值）、refs.go（引用分析）、objects.go（上下文对象）。
package expr

import (
	"errors"
	"fmt"
	"strings"
)

// Object 是可以按属性名查找的上下文对象。
// 普通的 map[string]any 也可以作为对象使用。
type Object interface {
	Get(key string) (any, error)
}

// Context 是表达式可见的顶层名字，例如 github、matrix。
type Context map[string]any

// Error 是表达式的语法或求值错误。
type Error struct {
	Expr string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("表达式 %q：%s", e.Expr, e.Msg) }

// EvalString 解析并求值一个表达式。
func EvalString(src string, ctx Context) (any, error) {
	n, err := Parse(src)
	if err != nil {
		return nil, err
	}
	v, err := eval(n, ctx)
	if err != nil {
		if _, ok := errors.AsType[*Error](err); ok {
			return nil, err
		}
		return nil, &Error{Expr: src, Msg: err.Error()}
	}
	return v, nil
}

// EvalCondition 对 if: 求值。可以写成 ${{ expr }}，也可以直接写 expr。
func EvalCondition(src string, ctx Context) (bool, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		return true, nil
	}
	if strings.HasPrefix(s, exprOpen) && strings.HasSuffix(s, exprClose) {
		e, ok, err := singleExpr(s)
		if err != nil {
			return false, err
		}
		if !ok {
			// 文本和表达式混合：插值结果非空即为真
			v, err := Interpolate(s, ctx)
			if err != nil {
				return false, err
			}
			return v != "", nil
		}
		s = e
	}
	v, err := EvalString(s, ctx)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tOp
	tLParen
	tRParen
	tLBrack
	tRBrack
	tDot
	tComma
)

// 运算符。
const (
	opOr  = "||"
	opAnd = "&&"
	opEq  = "=="
	opNe  = "!="
	opLt  = "<"
	opLe  = "<="
	opGt  = ">"
	opGe  = ">="
	opNot = "!"
)

// twoCharOps 是两个字符的运算符，词法分析时优先于单字符运算符匹配。
var twoCharOps = []string{opAnd, opOr, opEq, opNe, opLe, opGe}

type token struct {
	kind tokKind
	s    string
	num  float64
}

// punct 是单字符的标点符号。
var punct = map[byte]tokKind{'(': tLParen, ')': tRParen, '[': tLBrack, ']': tRBrack, ',': tComma}

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		if k, ok := punct[c]; ok {
			toks = append(toks, token{kind: k})
			i++
			continue
		}
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '.' && !dotStartsNumber(src, i, toks):
			toks = append(toks, token{kind: tDot})
			i++
		case c == '\'':
			s, n, err := lexString(src[i:])
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tStr, s: s})
			i += n
		case isTwoCharOp(src[i:]):
			toks = append(toks, token{kind: tOp, s: src[i : i+2]})
			i += 2
		case c == '<' || c == '>' || c == '!':
			toks = append(toks, token{kind: tOp, s: string(c)})
			i++
		case isDigit(c) || c == '-' || c == '.':
			j := numberEnd(src, i)
			n, ok := parseNumber(src[i:j])
			if !ok {
				return nil, fmt.Errorf("无效的数字 %q", src[i:j])
			}
			toks = append(toks, token{kind: tNum, num: n})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && (isIdentChar(src[j]) || src[j] == '-') {
				j++
			}
			toks = append(toks, token{kind: tIdent, s: src[i:j]})
			i = j
		case c == '*':
			return nil, errors.New("不支持对象过滤（*）")
		default:
			return nil, fmt.Errorf("无法识别的字符 %q", string(c))
		}
	}
	toks = append(toks, token{kind: tEOF})
	return toks, nil
}

// dotStartsNumber 判断 src[i] 处的 '.' 是否是 .5 这样的数字的开头，而不是属性访问：
// 后面紧跟数字，且前面没有可以被访问属性的值（开头、运算符、左括号或逗号之后）。
func dotStartsNumber(src string, i int, toks []token) bool {
	if i+1 >= len(src) || !isDigit(src[i+1]) {
		return false
	}
	if len(toks) == 0 {
		return true
	}
	switch toks[len(toks)-1].kind {
	case tOp, tLParen, tComma:
		return true
	}
	return false
}

func isTwoCharOp(s string) bool {
	for _, op := range twoCharOps {
		if strings.HasPrefix(s, op) {
			return true
		}
	}
	return false
}

// lexString 读取以 ' 开头的字符串字面量（连续两个单引号表示一个单引号），返回内容和消耗的字节数。
func lexString(s string) (string, int, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i += 2
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, errors.New("字符串没有结束")
}

// numberEnd 返回从 i 开始的数字字面量的结束位置（包含 0x、指数等形式）。
func numberEnd(src string, i int) int {
	j := i + 1
	for j < len(src) && (isIdentChar(src[j]) || src[j] == '.' || ((src[j] == '-' || src[j] == '+') && (src[j-1] == 'e' || src[j-1] == 'E'))) {
		j++
	}
	return j
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigit(c) }

func parseNumber(s string) (float64, bool) {
	ls := strings.ToLower(s)
	if hex, ok := strings.CutPrefix(ls, "0x"); ok {
		n, err := strconv.ParseInt(hex, 16, 64)
		return float64(n), err == nil
	}
	if hex, ok := strings.CutPrefix(ls, "-0x"); ok {
		n, err := strconv.ParseInt(hex, 16, 64)
		return -float64(n), err == nil
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil
}

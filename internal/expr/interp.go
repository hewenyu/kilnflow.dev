package expr

import "strings"

// 插值的定界符。
const (
	exprOpen  = "${{"
	exprClose = "}}"
)

// segment 是插值字符串中的一段：Expr 为空时是字面文本。
type segment struct {
	Text string
	Expr string
}

// split 把含有 ${{ }} 的字符串切成片段。
func split(s string) ([]segment, error) {
	var segs []segment
	for {
		i := strings.Index(s, exprOpen)
		if i < 0 {
			if s != "" {
				segs = append(segs, segment{Text: s})
			}
			return segs, nil
		}
		if i > 0 {
			segs = append(segs, segment{Text: s[:i]})
		}
		rest := s[i+len(exprOpen):]
		j := findClose(rest)
		if j < 0 {
			return nil, &Error{Expr: s, Msg: "缺少 '}}'"}
		}
		segs = append(segs, segment{Expr: strings.TrimSpace(rest[:j])})
		s = rest[j+len(exprClose):]
	}
}

// findClose 找到不在字符串字面量内的 "}}"。
func findClose(s string) int {
	inStr := false
	for i := range len(s) {
		if s[i] == '\'' {
			inStr = !inStr
			continue
		}
		if !inStr && strings.HasPrefix(s[i:], exprClose) {
			return i
		}
	}
	return -1
}

// singleExpr 判断 s（已去掉首尾空白）是否整串就是一个 ${{ }}，是则返回其中的表达式。
func singleExpr(s string) (string, bool, error) {
	if !strings.HasPrefix(s, exprOpen) || !strings.HasSuffix(s, exprClose) {
		return "", false, nil
	}
	segs, err := split(s)
	if err != nil {
		return "", false, err
	}
	if len(segs) == 1 && segs[0].Expr != "" {
		return segs[0].Expr, true, nil
	}
	return "", false, nil
}

// HasExpr 判断字符串中是否有 ${{ }}。
func HasExpr(s string) bool { return strings.Contains(s, exprOpen) }

// Interpolate 替换字符串中所有 ${{ }}，结果按 ToString 转换。
func Interpolate(s string, ctx Context) (string, error) {
	if !HasExpr(s) {
		return s, nil
	}
	segs, err := split(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, seg := range segs {
		if seg.Expr == "" {
			if err := writeStringLimited(&b, seg.Text); err != nil {
				return "", &Error{Expr: s, Msg: err.Error()}
			}
			continue
		}
		v, err := EvalString(seg.Expr, ctx)
		if err != nil {
			return "", err
		}
		if err := writeStringLimited(&b, ToString(v)); err != nil {
			return "", &Error{Expr: s, Msg: err.Error()}
		}
	}
	return b.String(), nil
}

// InterpolateValue 与 Interpolate 类似，但当整个字符串只是一个 ${{ }} 时保留原始类型
// （用于 matrix 等可以是数组的位置）。
func InterpolateValue(s string, ctx Context) (any, error) {
	// 切分出错时交给 Interpolate 报告同样的错误
	if e, ok, err := singleExpr(strings.TrimSpace(s)); err == nil && ok {
		return EvalString(e, ctx)
	}
	return Interpolate(s, ctx)
}

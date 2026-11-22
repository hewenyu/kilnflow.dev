package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// variadic 表示参数个数没有上限。
const variadic = -1

// 表达式结果的上限。format、join、toJSON、Interpolate 等会在构造结果时实时检查，防止嵌套调用
// （例如 format('{0}{0}', format('{0}{0}', …))）指数级放大内存。
const maxResultBytes = 1 << 20

// errResultTooLarge 返回结果超过上限的错误。
func errResultTooLarge() error {
	return fmt.Errorf("表达式结果超过 %d 字节上限", maxResultBytes)
}

// writeStringLimited 向 b 追加 s，超过上限时返回错误而不是继续分配。
func writeStringLimited(b *strings.Builder, s string) error {
	if b.Len()+len(s) > maxResultBytes {
		return errResultTooLarge()
	}
	_, _ = b.WriteString(s)
	return nil
}

// writeByteLimited 向 b 追加一个字节，超过上限时返回错误。
func writeByteLimited(b *strings.Builder, c byte) error {
	if b.Len()+1 > maxResultBytes {
		return errResultTooLarge()
	}
	_ = b.WriteByte(c)
	return nil
}

func callFunc(name string, args []any) (any, error) {
	need := func(lo, hi int) error {
		if len(args) < lo || (hi != variadic && len(args) > hi) {
			return fmt.Errorf("函数 %s 的参数个数不正确", name)
		}
		return nil
	}
	switch strings.ToLower(name) {
	case "contains":
		if err := need(2, 2); err != nil {
			return nil, err
		}
		return contains(args[0], args[1]), nil
	case "startswith":
		if err := need(2, 2); err != nil {
			return nil, err
		}
		return compareStrings(args[0], args[1], strings.HasPrefix), nil
	case "endswith":
		if err := need(2, 2); err != nil {
			return nil, err
		}
		return compareStrings(args[0], args[1], strings.HasSuffix), nil
	case "format":
		if err := need(1, variadic); err != nil {
			return nil, err
		}
		return format(ToString(args[0]), args[1:])
	case "join":
		if err := need(1, 2); err != nil {
			return nil, err
		}
		sep := ","
		if len(args) == 2 {
			sep = ToString(args[1])
		}
		return join(args[0], sep)
	case "tojson":
		if err := need(1, 1); err != nil {
			return nil, err
		}
		s, err := marshalJSON(args[0], "  ")
		if err != nil {
			return nil, fmt.Errorf("toJSON：%w", err)
		}
		return s, nil
	case "fromjson":
		if err := need(1, 1); err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal([]byte(ToString(args[0])), &v); err != nil {
			return nil, fmt.Errorf("fromJSON：%w", err)
		}
		return v, nil
	case "hashfiles":
		return nil, errors.New("不支持 hashFiles（规划时无法求值，见 PLAN #4.7）")
	case "success", "failure", "always", "cancelled":
		return nil, fmt.Errorf("不支持状态函数 %s()（PLAN #4.7）", name)
	}
	return nil, fmt.Errorf("未知函数 %s", name)
}

// contains 对数组判断是否有相等的元素；两个参数都是基本类型时按字符串（不区分大小写）判断是否包含子串；
// 其他情况（对象，或者在字符串中找对象）为 false，与 Actions 一致。
func contains(haystack, needle any) bool {
	if arr, ok := haystack.([]any); ok {
		for _, x := range arr {
			if looseEqual(x, needle) {
				return true
			}
		}
		return false
	}
	return compareStrings(haystack, needle, strings.Contains)
}

// compareStrings 在两个参数都是基本类型时，把它们转换成字符串后不区分大小写地用 f 比较；
// 有一个不是基本类型（对象、数组）时为 false，与 Actions 的 contains、startsWith、endsWith 一致。
func compareStrings(a, b any, f func(s, sub string) bool) bool {
	if !isPrimitive(a) || !isPrimitive(b) {
		return false
	}
	return f(strings.ToLower(ToString(a)), strings.ToLower(ToString(b)))
}

// join 用 sep 连接数组的元素；不是数组时，基本类型返回它本身的字符串形式，对象返回空字符串（与 Actions 一致）。
func join(v any, sep string) (string, error) {
	arr, ok := v.([]any)
	if !ok {
		if !isPrimitive(v) {
			return "", nil
		}
		s := ToString(v)
		if len(s) > maxResultBytes {
			return "", errResultTooLarge()
		}
		return s, nil
	}
	var b strings.Builder
	for i, x := range arr {
		if i > 0 {
			if err := writeStringLimited(&b, sep); err != nil {
				return "", err
			}
		}
		if err := writeStringLimited(&b, ToString(x)); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// format 实现 format('{0} {1}', a, b)：{N} 替换为第 N 个参数，{{ 和 }} 是转义。
func format(f string, args []any) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c == '{' {
			if i+1 < len(f) && f[i+1] == '{' {
				if err := writeByteLimited(&b, '{'); err != nil {
					return "", err
				}
				i++
				continue
			}
			j := strings.IndexByte(f[i:], '}')
			if j < 0 {
				return "", errors.New("format：缺少 '}'")
			}
			n, err := strconv.Atoi(f[i+1 : i+j])
			if err != nil || n < 0 || n >= len(args) {
				return "", fmt.Errorf("format：无效的占位符 %q", f[i:i+j+1])
			}
			if err := writeStringLimited(&b, ToString(args[n])); err != nil {
				return "", err
			}
			i += j
			continue
		}
		if c == '}' {
			if i+1 < len(f) && f[i+1] == '}' {
				i++
			}
			if err := writeByteLimited(&b, '}'); err != nil {
				return "", err
			}
			continue
		}
		if err := writeByteLimited(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

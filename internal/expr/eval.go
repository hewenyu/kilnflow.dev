package expr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// 值的类别（Actions 的比较规则按类别区分）。
const (
	kindNull   = "null"
	kindBool   = "bool"
	kindNumber = "number"
	kindString = "string"
	kindObject = "object"
)

// eval 在 ctx 中对语法树求值。
func eval(n Node, ctx Context) (any, error) {
	switch n := n.(type) {
	case literal:
		return n.v, nil
	case ident:
		v, ok := ctx[strings.ToLower(n.name)]
		if !ok {
			return nil, fmt.Errorf("上下文 %s 在此处不可用", n.name)
		}
		return v, nil
	case property:
		t, err := eval(n.target, ctx)
		if err != nil {
			return nil, err
		}
		return getProp(t, n.name)
	case index:
		return evalIndex(n, ctx)
	case unary:
		x, err := eval(n.x, ctx)
		if err != nil {
			return nil, err
		}
		return !truthy(x), nil
	case binary:
		return evalBinary(n, ctx)
	case call:
		args := make([]any, len(n.args))
		for i, a := range n.args {
			v, err := eval(a, ctx)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		return callFunc(n.name, args)
	}
	return nil, errors.New("未知的语法节点")
}

func evalIndex(n index, ctx Context) (any, error) {
	t, err := eval(n.target, ctx)
	if err != nil {
		return nil, err
	}
	k, err := eval(n.key, ctx)
	if err != nil {
		return nil, err
	}
	if arr, ok := t.([]any); ok {
		f := toNumber(k)
		// 先按浮点数判断范围，避免超大的数转换成 int 时溢出
		if math.IsNaN(f) || f < 0 || f >= float64(len(arr)) {
			return nil, nil
		}
		return arr[int(f)], nil
	}
	return getProp(t, ToString(k))
}

func evalBinary(n binary, ctx Context) (any, error) {
	l, err := eval(n.l, ctx)
	if err != nil {
		return nil, err
	}
	// && 和 || 短路求值，返回决定结果的那个操作数
	switch n.op {
	case opAnd:
		if !truthy(l) {
			return l, nil
		}
		return eval(n.r, ctx)
	case opOr:
		if truthy(l) {
			return l, nil
		}
		return eval(n.r, ctx)
	}
	r, err := eval(n.r, ctx)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case opEq:
		return looseEqual(l, r), nil
	case opNe:
		return !looseEqual(l, r), nil
	default:
		return compare(n.op, l, r), nil
	}
}

func getProp(t any, name string) (any, error) {
	switch o := t.(type) {
	case nil:
		return nil, nil
	case Object:
		return o.Get(name)
	case map[string]any:
		v, _ := lookupFold(o, name)
		return v, nil
	case map[string]string:
		if v, ok := o[name]; ok {
			return v, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// lookupFold 查找对象（例如 fromJSON 的结果）的属性：先精确匹配，再不区分大小写匹配；
// 有多个只差大小写的键时取字典序最小的，结果不受 map 遍历顺序影响。
func lookupFold(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	best, found := "", false
	for k := range m {
		if strings.EqualFold(k, key) && (!found || k < best) {
			best, found = k, true
		}
	}
	if !found {
		return nil, false
	}
	return m[best], true
}

// truthy 按 Actions 的规则判断真假。
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case int:
		return v != 0
	case string:
		return v != ""
	default:
		return true
	}
}

func toNumber(v any) float64 {
	switch v := v.(type) {
	case nil:
		return 0
	case bool:
		if v {
			return 1
		}
		return 0
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if n, ok := parseNumber(s); ok {
			return n
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

func isPrimitive(v any) bool {
	switch v.(type) {
	case nil, bool, float64, int, string:
		return true
	}
	return false
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return kindNull
	case bool:
		return kindBool
	case float64, int:
		return kindNumber
	case string:
		return kindString
	}
	return kindObject
}

// looseEqual 按 Actions 的规则比较：类型不同时转换成数字比较，字符串不区分大小写，
// 对象和数组只有是同一个实例时才相等。
func looseEqual(l, r any) bool {
	if !isPrimitive(l) || !isPrimitive(r) {
		return sameInstance(l, r)
	}
	kl, kr := kindOf(l), kindOf(r)
	if kl == kr {
		switch kl {
		case kindNull:
			return true
		case kindString:
			ls, _ := l.(string)
			rs, _ := r.(string)
			return strings.EqualFold(ls, rs)
		case kindBool:
			lb, _ := l.(bool)
			rb, _ := r.(bool)
			return lb == rb
		}
	}
	return toNumber(l) == toNumber(r)
}

// sameInstance 判断两个对象或数组是否是同一个实例（比较引用，不比较内容）。
// Strict 是按值传递的 map 包装，按它包装的 Fields 判断。
// 长度为 0 的数组可能共用同一个地址，两个不同的空数组也会被当成同一个实例；这种比较没有实际用途，不做区分。
func sameInstance(l, r any) bool {
	if s, ok := l.(Strict); ok {
		l = s.Fields
	}
	if s, ok := r.(Strict); ok {
		r = s.Fields
	}
	lv, rv := reflect.ValueOf(l), reflect.ValueOf(r)
	if !lv.IsValid() || !rv.IsValid() || lv.Type() != rv.Type() {
		return false
	}
	switch lv.Kind() {
	case reflect.Map, reflect.Pointer:
		return lv.Pointer() == rv.Pointer()
	case reflect.Slice:
		return lv.Pointer() == rv.Pointer() && lv.Len() == rv.Len()
	}
	return false
}

func compare(op string, l, r any) bool {
	if !isPrimitive(l) || !isPrimitive(r) {
		return false
	}
	var c int
	ls, lok := l.(string)
	rs, rok := r.(string)
	if lok && rok {
		c = strings.Compare(strings.ToUpper(ls), strings.ToUpper(rs))
	} else {
		a, b := toNumber(l), toNumber(r)
		if math.IsNaN(a) || math.IsNaN(b) {
			return false
		}
		switch {
		case a < b:
			c = -1
		case a > b:
			c = 1
		}
	}
	switch op {
	case opLt:
		return c < 0
	case opLe:
		return c <= 0
	case opGt:
		return c > 0
	case opGe:
		return c >= 0
	}
	return false
}

// ToString 把值转换成插值时使用的字符串。
func ToString(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1e15 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case string:
		return v
	default:
		s, err := marshalJSON(v, "")
		if err != nil {
			return ""
		}
		return s
	}
}

// marshalJSON 把值序列化为 JSON（indent 非空时缩进）。与 Actions 一致，不把 <、>、& 转义成 \u003c 等形式。
// encoding/json 对 map 的键排序，输出是稳定的。
func marshalJSON(v any, indent string) (string, error) {
	b := &limitedBuffer{limit: maxResultBytes}
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(normalize(v)); err != nil {
		return "", err
	}
	// Encode 在末尾加换行
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// limitedBuffer 是写入超过 limit 字节后返回错误的 bytes.Buffer，用于限制序列化结果的大小。
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errResultTooLarge()
	}
	return b.Buffer.Write(p)
}

// normalize 把 Object 等类型转换成可以 JSON 序列化的值。
func normalize(v any) any {
	switch v := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			m[k] = normalize(x)
		}
		return m
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			a[i] = normalize(x)
		}
		return a
	case map[string]string:
		return v
	case Object:
		if j, ok := v.(interface{ JSON() any }); ok {
			return j.JSON()
		}
		return map[string]any{}
	}
	return v
}

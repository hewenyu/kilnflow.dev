// Package actionkey 计算输入摘要和 action key（PLAN #7.4、#11.1）。
package actionkey

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Version 是算法版本。修改算法或 Kiln 注入的固定环境变量时递增，让全部缓存失效。
const Version = 1

// git tree 条目的文件模式（ls-tree 输出的八进制字符串）。
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	ModeSubmodule  = "160000"
)

// TreeEntry 是 git tree 中的一个条目（只包括文件、符号链接和子模块）。
type TreeEntry struct {
	Path string
	Mode string // ModeFile | ModeExecutable | ModeSymlink | ModeSubmodule
	SHA  string
}

// InputDigest 计算输入摘要：
// sha256( 按 path 字节序排序后逐行拼接 "path \0 mode \0 object_sha \n" )
func InputDigest(entries []TreeEntry) string {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b TreeEntry) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, e := range sorted {
		// hash.Hash 的 Write 不会返回错误
		_, _ = h.Write([]byte(e.Path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(e.Mode))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(e.SHA))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Input 是计算 action key 所需的全部字段。
type Input struct {
	Salt         string
	Platform     string
	Executor     map[string]any
	Inputs       string
	Runtimes     [][2]string // [名称, digest]，计算时按名称排序（不修改调用方的切片）
	Env          map[string]string
	Steps        []map[string]any
	Outputs      map[string]string
	Upstream     [][2]string // [job_key.output, manifest digest]，按 fetch 声明顺序
	FetchTo      []string
	Services     []any
	Network      bool
	Repositories []string
}

// orEmpty 把 nil 切片换成空切片，让 JSON 中输出 [] 而不是 null。
func orEmpty[S ~[]E, E any](s S) S {
	if s == nil {
		return S{}
	}
	return s
}

// orEmptyMap 把 nil map 换成空 map，让 JSON 中输出 {} 而不是 null。
func orEmptyMap[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

// Compute 返回 action key（hex）和各组成部分的哈希（供"缓存解释"使用）。
func Compute(in Input) (string, map[string]string, error) {
	// 复制后再排序，避免修改调用方的底层数组
	runtimes := slices.Clone(orEmpty(in.Runtimes))
	slices.SortFunc(runtimes, func(a, b [2]string) int { return cmp.Compare(a[0], b[0]) })
	doc := map[string]any{
		"v":            Version,
		"salt":         in.Salt,
		"platform":     in.Platform,
		"executor":     in.Executor,
		"inputs":       in.Inputs,
		"runtimes":     runtimes,
		"env":          orEmptyMap(in.Env),
		"steps":        orEmpty(in.Steps),
		"outputs":      orEmptyMap(in.Outputs),
		"upstream":     orEmpty(in.Upstream),
		"fetch_to":     orEmpty(in.FetchTo),
		"services":     orEmpty(in.Services),
		"network":      in.Network,
		"repositories": orEmpty(in.Repositories),
	}
	parts := make(map[string]string, len(doc))
	for k, v := range doc {
		b, err := Canonical(v)
		if err != nil {
			return "", nil, err
		}
		parts[k] = Sum(b)
	}
	b, err := Canonical(doc)
	if err != nil {
		return "", nil, err
	}
	return Sum(b), parts, nil
}

// Sum 返回 sha256 的 hex。
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Canonical 按 RFC 8785（JCS）输出规范化 JSON。
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, x); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return err
		}
		s, err := formatNumber(f)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case string:
		writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		// JCS 要求按 UTF-16 码元比较键
		keys := slices.SortedFunc(maps.Keys(x), compareUTF16)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: 不支持的类型 %T", v)
	}
	return nil
}

// formatNumber 按 ECMAScript 的 Number.prototype.toString 输出数字（RFC 8785 #3.2.2.3）。
func formatNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("jcs: 不支持的数字 %v", f)
	}
	if f == 0 {
		return "0", nil // 包括 -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 最短的往返表示：d.ddddde±xx
	mant, expStr, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		return "", err
	}
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits) // 有效数字位数
	n := exp + 1     // 小数点的位置：值 = 0.digits × 10^n
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		e := "e+"
		if n-1 < 0 {
			e = "e-"
		}
		s = digits[:1]
		if k > 1 {
			s += "." + digits[1:]
		}
		s += e + strconv.Itoa(absInt(n-1))
	}
	return sign + s, nil
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// compareUTF16 按 UTF-16 码元序比较两个字符串。
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

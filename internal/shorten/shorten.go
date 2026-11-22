// Package shorten 缩短要显示、保存或写入错误信息的字符串：按字节上限截断文本（不切开多字节字符），缩写 sha 和 digest。
package shorten

import "unicode/utf8"

// HashLen 是 sha、action key、digest 等缩写后的长度。
const HashLen = 12

// Ellipsis 是截断后追加的省略号（3 个字节）。
const Ellipsis = "…"

// Text 把 s 截断到最多 maxBytes 个字节（不含追加的省略号），不切开多字节字符；截断时追加 Ellipsis。
// 只要 s 是合法的 UTF-8，结果就是合法的 UTF-8。
func Text(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return Prefix(s, maxBytes) + Ellipsis
}

// Prefix 返回 s 不超过 maxBytes 个字节、不切开多字节字符的最长前缀（不追加省略号）。
func Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := max(maxBytes, 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Hash 返回 sha 或 digest 的前 HashLen 个字符。
func Hash(s string) string {
	if len(s) > HashLen {
		return s[:HashLen]
	}
	return s
}

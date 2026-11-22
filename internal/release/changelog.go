package release

import (
	"strings"
	"unicode/utf8"
)

// ChangelogSection 取出标题以版本号开头的二级标题段落（tag 的前缀 v 可有可无）。
// 标题可以写成 "## 1.2.0"、"## [1.2.0] - 2026-01-01"、"## v1.2.0 (…)" 等形式。
// 版本号后面紧跟 - 时是另一个版本（例如 1.2.0-rc.1），不算匹配。
func ChangelogSection(doc, tag string) (string, bool) {
	ver := strings.TrimPrefix(tag, "v")
	if ver == "" {
		return "", false
	}
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if !strings.HasPrefix(l, "## ") {
			continue
		}
		if start >= 0 {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n")), true
		}
		h := strings.TrimPrefix(strings.TrimSpace(l[3:]), "[")
		h = strings.TrimPrefix(h, "v")
		if !strings.HasPrefix(h, ver) {
			continue
		}
		rest := h[len(ver):]
		if rest == "" {
			start = i + 1
			continue
		}
		// 按 rune 判断紧跟的字符，– 和 — 是多字节字符。
		if r, _ := utf8.DecodeRuneInString(rest); strings.ContainsRune(" ]–—(:", r) {
			start = i + 1
		}
	}
	if start >= 0 {
		return strings.TrimSpace(strings.Join(lines[start:], "\n")), true
	}
	return "", false
}

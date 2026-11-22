// Package redact 去掉文字（错误信息、错误响应体）中的凭据。发布用的 HTTP 客户端（npm registry、HTTP 上传、
// OCI registry）和发布 worker 共用同一套规则：按长度从长到短替换成 Mask，短于 MinLen 的值不替换；
// 读取有上限的响应体时，在上限处被截断的凭据也不会留下开头的部分。
//
// 截断要在脱敏之后：先截断会把凭据切成两半，剩下的前半段不再与完整的值匹配。
package redact

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"strings"
)

// Mask 是替换凭据的文字。
const Mask = "***"

// MinLen 是需要替换的值的最短长度：更短的值替换掉会误伤普通文字。
const MinLen = 4

// fingerprintLen 是日志中凭据指纹的长度（十六进制字符）。
const fingerprintLen = 12

// Fingerprint 返回凭据在日志中的指纹（SHA-256 的前缀），不能还原凭据。Server 与 Runner 记录同一枚 token 的签发、吊销时
// 用它配对。
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

// String 把 s 中出现的每个 secret 替换成 Mask。先替换较长的值：一个值是另一个值的一部分时，
// 先替换短的会让长的只被替换一部分，剩下的部分留在结果中。
func String(s string, secrets ...string) string {
	vals := slices.SortedFunc(slices.Values(secrets), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, v := range vals {
		if len(v) >= MinLen {
			s = strings.ReplaceAll(s, v, Mask)
		}
	}
	return s
}

// ReadLimited 读取 r 中大约 limit 字节并去掉其中的 secret。多读最长的 secret 的长度，使跨过上限的 secret
// 也能完整识别；仍被截断时，去掉末尾可能是某个 secret 开头的部分。读取失败时使用已经读到的内容。
// 返回的内容可能比 limit 稍长，调用方按需要再截断（截断在脱敏之后，是安全的）。
func ReadLimited(r io.Reader, limit int, secrets ...string) string {
	n := limit
	for _, s := range secrets {
		n = max(n, limit+len(s))
	}
	b, _ := io.ReadAll(io.LimitReader(r, int64(n))) // 只用于错误信息，读取失败时用已读到的部分
	s := String(string(b), secrets...)
	if len(b) == n {
		s = trimPartial(s, secrets)
	}
	return s
}

// trimPartial 去掉 s 末尾与某个 secret 开头相同的最长部分（截断处可能只剩部分 secret）。
func trimPartial(s string, secrets []string) string {
	cut := 0
	for _, sec := range secrets {
		if len(sec) < MinLen {
			continue
		}
		for k := min(len(sec)-1, len(s)); k > cut; k-- {
			if strings.HasSuffix(s, sec[:k]) {
				cut = k
				break
			}
		}
	}
	return s[:len(s)-cut]
}

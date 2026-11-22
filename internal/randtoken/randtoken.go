// Package randtoken 生成密码学安全的随机 token。
package randtoken

import (
	"crypto/rand"
	"encoding/base64"
)

// New 返回 n 个随机字节的 base64url（无填充）编码。
func New(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // Go 1.24 起 crypto/rand.Read 不会返回错误（失败时直接终止程序）
	return base64.RawURLEncoding.EncodeToString(b)
}

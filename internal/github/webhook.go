package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// webhook 请求的头部。
const (
	// HeaderSignature 是 webhook 内容的 HMAC-SHA256 签名（见 VerifySignature）。
	HeaderSignature = "X-Hub-Signature-256"
	// HeaderEvent 是事件名，例如 push、pull_request。
	HeaderEvent = "X-GitHub-Event"
	// HeaderDelivery 是这次投递的唯一 ID，重新投递时不变。
	HeaderDelivery = "X-GitHub-Delivery"
)

// signaturePrefix 是 X-Hub-Signature-256 取值的前缀。
const signaturePrefix = "sha256="

// VerifySignature 校验 X-Hub-Signature-256。secret 为空时一律返回 false（任何人都能算出空密钥的签名）。
func VerifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	hexSig, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok || len(hexSig) != sha256.Size*2 {
		return false
	}
	want, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	return hmac.Equal(mac(secret, body), want)
}

// Sign 计算 X-Hub-Signature-256（模拟 GitHub 发送 webhook 时使用）。
func Sign(secret, body []byte) string {
	return signaturePrefix + hex.EncodeToString(mac(secret, body))
}

func mac(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)
}

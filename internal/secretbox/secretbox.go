// Package secretbox 实现 secret 的信封加密：每个 secret 用随机的数据密钥（DEK）以 AES-256-GCM 加密，
// DEK 再用主密钥加密。两层都绑定附加数据（AAD），密文只能在加密时的作用域和名字下解密，
// 不能被挪到别的 secret 上使用。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize 是主密钥和 DEK 的长度（AES-256）。
const KeySize = 32

// errShortCiphertext 表示密文比 nonce 还短。
var errShortCiphertext = errors.New("密文太短")

// errMasterKeySize 表示主密钥不是 KeySize 字节。aes.NewCipher 也接受 16、24 字节的密钥，
// 不检查时会悄悄退化为 AES-128 / AES-192。
var errMasterKeySize = fmt.Errorf("主密钥应为 %d 字节", KeySize)

// Sealed 是加密后的 secret。
type Sealed struct {
	Ciphertext []byte // DEK 加密的值
	WrappedDEK []byte // 主密钥加密的 DEK
}

// Seal 加密 plain，aad 标识 secret 的位置（见 AAD）。
func Seal(masterKey, aad, plain []byte) (Sealed, error) {
	if len(masterKey) != KeySize {
		return Sealed{}, errMasterKeySize
	}
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("生成数据密钥：%w", err)
	}
	ct, err := seal(dek, aad, plain)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := seal(masterKey, aad, dek)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{Ciphertext: ct, WrappedDEK: wrapped}, nil
}

// Open 解密 Seal 的结果；aad 必须与加密时相同。
func Open(masterKey, aad []byte, s Sealed) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, errMasterKeySize
	}
	dek, err := open(masterKey, aad, s.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("解开数据密钥：%w", err)
	}
	plain, err := open(dek, aad, s.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("解密：%w", err)
	}
	return plain, nil
}

// AAD 由作用域和名字组成。各段以 0 字节分隔，名字中不会出现 0 字节，不同的组合不会得到相同的 AAD。
func AAD(scopeKind string, scopeID int64, name string) []byte {
	return fmt.Appendf(nil, "kiln-secret/v1\x00%s\x00%d\x00%s", scopeKind, scopeID, name)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal 用 key 加密 plain 并绑定 aad，返回 nonce 加密文。参数顺序与 Seal 一致：(key, aad, data)。
func seal(key, aad, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, aad), nil
}

// open 解密 seal 的结果；aad 必须与加密时相同。
func open(key, aad, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errShortCiphertext
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

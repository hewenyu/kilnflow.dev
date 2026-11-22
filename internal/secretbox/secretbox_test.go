package secretbox

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	aad := AAD("repo", 7, "NPM_TOKEN")
	s, err := Seal(key, aad, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, aad, s)
	if err != nil || !bytes.Equal(got, []byte("s3cret")) {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenRejects(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	aad := AAD("repo", 7, "NPM_TOKEN")
	s, err := Seal(key, aad, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := Seal(key, AAD("repo", 7, "OTHER"), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		key  []byte
		aad  []byte
		s    Sealed
	}{
		{name: "其他名字", key: key, aad: AAD("repo", 7, "OTHER"), s: s},
		{name: "其他作用域", key: key, aad: AAD("repo", 8, "NPM_TOKEN"), s: s},
		{name: "其他作用域类型", key: key, aad: AAD("owner", 7, "NPM_TOKEN"), s: s},
		{name: "其他主密钥", key: newKey(t), aad: aad, s: s},
		{name: "换用别的 secret 的 DEK", key: key, aad: aad, s: Sealed{Ciphertext: s.Ciphertext, WrappedDEK: other.WrappedDEK}},
		{name: "密文太短", key: key, aad: aad, s: Sealed{Ciphertext: s.Ciphertext, WrappedDEK: []byte{1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Open(c.key, c.aad, c.s); err == nil {
				t.Fatal("期望解密失败")
			}
		})
	}
}

func TestAADDistinct(t *testing.T) {
	t.Parallel()
	if bytes.Equal(AAD("repo", 1, "2\x00x"), AAD("repo", 12, "x")) {
		t.Fatal("AAD 冲突")
	}
}

func TestRoundTripEdgeValues(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	cases := []struct {
		name  string
		plain []byte
	}{
		{name: "空值", plain: []byte{}},
		{name: "二进制内容", plain: []byte{0, 1, 2, 0xff, 0}},
		{name: "较长的值", plain: bytes.Repeat([]byte("x"), 64<<10)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			aad := AAD("env", 3, c.name)
			s, err := Seal(key, aad, c.plain)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(s.Ciphertext, c.plain) && len(c.plain) > 0 {
				t.Fatal("密文中出现了明文")
			}
			got, err := Open(key, aad, s)
			if err != nil {
				t.Fatalf("Open：%v", err)
			}
			if !bytes.Equal(got, c.plain) {
				t.Fatalf("Open = %x，期望 %x", got, c.plain)
			}
		})
	}
}

// 同一个值加密两次，DEK 和 nonce 都是随机的，两次的密文和包装后的 DEK 都不同。
func TestSealIsRandomized(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	aad := AAD("repo", 1, "TOKEN")
	a, err := Seal(key, aad, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(key, aad, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) || bytes.Equal(a.WrappedDEK, b.WrappedDEK) {
		t.Fatal("两次加密的结果相同，DEK 或 nonce 没有随机化")
	}
}

func TestSealRejectsInvalidMasterKey(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 15, 16, 24, 31, 33} {
		if _, err := Seal(make([]byte, n), AAD("repo", 1, "X"), []byte("v")); err == nil {
			t.Errorf("主密钥长度 %d 时 Seal 应当报错", n)
		}
		if _, err := Open(make([]byte, n), AAD("repo", 1, "X"), Sealed{}); err == nil {
			t.Errorf("主密钥长度 %d 时 Open 应当报错", n)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	aad := AAD("repo", 7, "NPM_TOKEN")
	s, err := Seal(key, aad, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(b []byte) []byte {
		c := bytes.Clone(b)
		c[len(c)-1] ^= 1
		return c
	}
	cases := []struct {
		name  string
		key   []byte
		s     Sealed
		short bool // 是否应为 errShortCiphertext
	}{
		{name: "主密钥长度无效", key: key[:16+1], s: s},
		{name: "包装的 DEK 被篡改", key: key, s: Sealed{Ciphertext: s.Ciphertext, WrappedDEK: flip(s.WrappedDEK)}},
		{name: "密文被篡改", key: key, s: Sealed{Ciphertext: flip(s.Ciphertext), WrappedDEK: s.WrappedDEK}},
		{name: "密文为空", key: key, s: Sealed{WrappedDEK: s.WrappedDEK}, short: true},
		{name: "包装的 DEK 为空", key: key, s: Sealed{Ciphertext: s.Ciphertext}, short: true},
		{name: "密文和 DEK 对调", key: key, s: Sealed{Ciphertext: s.WrappedDEK, WrappedDEK: s.Ciphertext}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Open(c.key, aad, c.s)
			if err == nil {
				t.Fatalf("期望解密失败，得到 %q", got)
			}
			if got != nil {
				t.Errorf("失败时不应返回内容：%q", got)
			}
			if c.short && !errors.Is(err, errShortCiphertext) {
				t.Errorf("err = %v，期望 errShortCiphertext", err)
			}
		})
	}
}

// 篡改后的密文解出来的 DEK 长度不对时也只返回错误，不会 panic。
func TestOpenWrappedNonKey(t *testing.T) {
	t.Parallel()
	key := newKey(t)
	aad := AAD("repo", 1, "X")
	// 用主密钥直接"包装"一个长度不对的 DEK
	wrapped, err := seal(key, aad, []byte("short-dek"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(key, aad, Sealed{Ciphertext: make([]byte, 64), WrappedDEK: wrapped}); err == nil {
		t.Fatal("DEK 长度无效时应当报错")
	}
}

func TestAADFormat(t *testing.T) {
	t.Parallel()
	got := AAD("environment", -1, "名字")
	want := "kiln-secret/v1\x00environment\x00-1\x00名字"
	if string(got) != want {
		t.Fatalf("AAD = %q，期望 %q", got, want)
	}
}

package randtoken

import (
	"encoding/base64"
	"testing"
)

func TestNew(t *testing.T) {
	a, b := New(16), New(16)
	if a == b {
		t.Fatalf("两次生成的 token 相同：%s", a)
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(raw) != 16 {
		t.Fatalf("New(16) = %q，解码得到 %d 字节，err=%v", a, len(raw), err)
	}
}

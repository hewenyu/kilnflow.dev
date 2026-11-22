package shorten

import (
	"testing"
	"unicode/utf8"
)

func TestText(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 3, "abc"},
		{"abcd", 3, "abc…"},
		{"", 0, ""},
		{"abc", 0, "…"},
		{"abc", -1, "…"},
		{"中文", 4, "中…"}, // 第 4 个字节在"文"的中间，退到"中"之后
		{"中文", 3, "中…"},
		{"中文", 2, "…"},
		{"中文", 6, "中文"},
	}
	for _, c := range cases {
		got := Text(c.in, c.n)
		if got != c.want {
			t.Errorf("Text(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Text(%q, %d) = %q 不是合法的 UTF-8", c.in, c.n, got)
		}
	}
}

func TestPrefix(t *testing.T) {
	for n, want := range map[int]string{-1: "", 0: "", 1: "a", 2: "a", 3: "a", 4: "a审", 5: "a审b", 9: "a审b"} {
		if got := Prefix("a审b", n); got != want {
			t.Errorf("Prefix(%d) = %q，期望 %q", n, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	if got := Hash("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("Hash = %q", got)
	}
	if got := Hash("abc"); got != "abc" {
		t.Errorf("Hash = %q", got)
	}
}

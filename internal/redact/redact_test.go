package redact_test

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/hewenyu/kilnflow.dev/internal/redact"
)

func TestString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{"替换全部出现", "a=s3cret b=s3cret", []string{"s3cret"}, "a=*** b=***"},
		{"短值不替换", "abc abc", []string{"abc"}, "abc abc"},
		{"空值不替换", "text", []string{""}, "text"},
		// 一个值是另一个值的一部分：先替换长的，不留下长值的剩余部分（与参数顺序无关）
		{"重叠：短的在前", "token=abcd-1234-tail", []string{"abcd", "abcd-1234-tail"}, "token=***"},
		{"重叠：长的在前", "token=abcd-1234-tail", []string{"abcd-1234-tail", "abcd"}, "token=***"},
		{"没有 secret", "text", nil, "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redact.String(tt.in, tt.secrets...); got != tt.want {
				t.Errorf("String(%q, %q) = %q，期望 %q", tt.in, tt.secrets, got, tt.want)
			}
		})
	}
}

func TestReadLimited(t *testing.T) {
	t.Parallel()
	const tok = "tok_0123456789abcdef"
	const limit = 64
	pad := func(n int) string { return strings.Repeat("x", n) }
	tests := []struct {
		name string
		body string
		want string
	}{
		{"短的响应体原样返回（脱敏后）", "bad token " + tok, "bad token ***"},
		// token 跨过上限：多读 len(token) 字节，仍能完整识别
		{"跨过上限的 token", pad(limit-5) + tok + pad(100), pad(limit-5) + "***" + pad(5)},
		// 多读之后仍被截断（只剩 token 的开头）：去掉末尾的这一部分
		{"截断处只剩 token 开头", pad(limit+3) + tok, pad(limit + 3)},
		{"恰好在上限处结束", pad(limit), pad(limit)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := redact.ReadLimited(strings.NewReader(tt.body), limit, tok)
			if got != tt.want {
				t.Errorf("ReadLimited = %q，期望 %q", got, tt.want)
			}
			for k := 4; k <= len(tok); k++ {
				if strings.Contains(got, tok[:k]) {
					t.Fatalf("结果中留下了 token 的前 %d 个字符：%q", k, got)
				}
			}
		})
	}

	// 截断时留下的是任一 secret 的开头都要去掉（多个 secret 时取最长的匹配）
	got := redact.ReadLimited(strings.NewReader(pad(limit+10)+"pass-word-value"), limit, "short-secret", "pass-word-value")
	if strings.Contains(got, "pass") {
		t.Errorf("多个 secret 时仍留下了开头：%q", got)
	}

	// 读取失败：用已经读到的部分
	if got := redact.ReadLimited(iotest.TimeoutReader(strings.NewReader("first "+tok)), limit, tok); got != "first ***" {
		t.Errorf("读取失败时的结果 %q，期望 %q", got, "first ***")
	}
	if got := redact.ReadLimited(iotest.ErrReader(errors.New("boom")), limit, tok); got != "" {
		t.Errorf("什么也没读到时应为空：%q", got)
	}
}

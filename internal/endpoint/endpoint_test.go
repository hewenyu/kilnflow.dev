package endpoint

import (
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"api.localhost", true},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"example.com", false},
		{"10.0.0.1", false},
		{"localhost.example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopbackHost(c.host); got != c.want {
			t.Errorf("IsLoopbackHost(%q) = %v，期望 %v", c.host, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string // 期望错误信息的片段；空表示通过
	}{
		{"https://example.com/api", ""},
		{"https://example.com", ""},
		{"http://localhost:8080/x", ""},
		{"http://127.0.0.1:5000", ""},
		{"http://[::1]:8080/x", ""},
		{"http://example.com/x", "必须是 https"},
		{"ftp://example.com/x", "必须是 https"},
		{"https://", "缺少主机名"},
		{"https://:443/x", "缺少主机名"},
		{"example.com/x", "缺少主机名"},
		{"https://user:pass@example.com/x", "不能包含用户信息"},
		{"https://example.com/x?y=1", "不能带查询参数或 fragment"},
		{"https://example.com/x#frag", "不能带查询参数或 fragment"},
		{"https://example.com/x?", "不能带查询参数或 fragment"},
		{"https://example.com/x#", "不能带查询参数或 fragment"},
		{"://bad", "无效"},
	}
	for _, c := range cases {
		err := Check(c.raw, "服务地址")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("Check(%q) = %v，期望通过", c.raw, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("Check(%q) = %v，期望包含 %q", c.raw, err, c.want)
		}
	}
}

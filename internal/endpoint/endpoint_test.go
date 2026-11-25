package endpoint

import (
	"errors"
	"fmt"
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
		raw     string
		want    string // 期望错误信息的片段；空表示通过
		problem Problem
	}{
		{"https://example.com/api", "", ""},
		{"https://example.com", "", ""},
		{"http://localhost:8080/x", "", ""},
		{"http://127.0.0.1:5000", "", ""},
		{"http://[::1]:8080/x", "", ""},
		{"http://example.com/x", "必须是 https", ProblemScheme},
		{"ftp://example.com/x", "必须是 https", ProblemScheme},
		{"https://", "缺少主机名", ProblemNoHost},
		{"https://:443/x", "缺少主机名", ProblemNoHost},
		{"example.com/x", "缺少主机名", ProblemNoHost},
		{"https://user:pass@example.com/x", "不能包含用户信息", ProblemUserInfo},
		{"https://example.com/x?y=1", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x#frag", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x?", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x#", "不能带查询参数或 fragment", ProblemQuery},
		{"://bad", "无效", ProblemInvalid},
	}
	for _, c := range cases {
		err := Check(c.raw, "服务地址")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("Check(%q) = %v，期望通过", c.raw, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("Check(%q) = %v，期望包含 %q", c.raw, err, c.want)
		}
		if c.want == "" {
			continue
		}
		// 原因码和原样的地址供 Dashboard 按语言显示；说明文字（CLI、日志）带上称呼和地址
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Problem != c.problem || e.Raw != c.raw || e.What != "服务地址" {
			t.Errorf("Check(%q) = %#v，期望原因码 %q", c.raw, err, c.problem)
		}
		if want := fmt.Sprintf("服务地址 %q ", c.raw); !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Check(%q) = %q，期望以 %q 开头", c.raw, err, want)
		}
	}
}

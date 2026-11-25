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
		{"https://example.com/x?y=1", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x#frag", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x?", "不能带查询参数或 fragment", ProblemQuery},
		{"https://example.com/x#", "不能带查询参数或 fragment", ProblemQuery},
		{"://bad", "无效", ProblemInvalid},
		{"https://example.com/x@y", "", ""},
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

// 带（或疑似带）用户信息的地址：无论能否解析、有没有主机名，都归为 ProblemUserInfo，错误中不保留也不回显地址和凭据。
func TestCheckUserInfoNotEchoed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://user:s3cret@example.com/x",
		"https://u:s3cret@:443/",
		"https://u:s3cret@",
		"https://u:p%ss@npm.example.com/",
		"//u:s3cret@npm.example.com/",
		"u:s3cret@npm.example.com/x",
	} {
		err := Check(raw, "服务地址")
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Problem != ProblemUserInfo {
			t.Errorf("Check(%q) = %#v，期望原因码 %q", raw, err, ProblemUserInfo)
			continue
		}
		if e.Raw != "" || e.Err != nil {
			t.Errorf("Check(%q) 的错误保留了地址：Raw=%q Err=%v", raw, e.Raw, e.Err)
		}
		msg := err.Error()
		if want := "服务地址 不能包含用户信息"; !strings.HasPrefix(msg, want) {
			t.Errorf("Check(%q) = %q，期望以 %q 开头", raw, msg, want)
		}
		for _, secret := range []string{"s3cret", "p%ss", "user"} {
			if strings.Contains(msg, secret) {
				t.Errorf("Check(%q) = %q，不能包含 %q", raw, msg, secret)
			}
		}
	}
}

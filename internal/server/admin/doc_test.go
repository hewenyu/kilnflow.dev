package admin

import "testing"

// TestCheckDocBaseURL 覆盖文档系统地址的校验：必须 http/https 且有主机名；结尾斜杠由调用方去掉。
func TestCheckDocBaseURL(t *testing.T) {
	t.Parallel()
	good := []string{
		"https://outline.example.com",
		"http://127.0.0.1:8080",
		"https://outline.team.example.com",
	}
	for _, u := range good {
		if err := checkDocBaseURL(u); err != nil {
			t.Errorf("checkDocBaseURL(%q) = %v，应为 nil", u, err)
		}
	}
	bad := []string{
		"", "outline.example.com", "ftp://outline.example.com", "https://", "://bad",
		"https://:443",                        // 只有端口，没有主机名
		"https://user:pw@outline.example.com", // 不能带凭据
		"http://outline.example.com",          // 非本机必须 https
		"https://outline.example.com?x=1",     // 不能带查询参数
		"https://outline.example.com#frag",    // 不能带片段
	}
	for _, u := range bad {
		if err := checkDocBaseURL(u); err == nil {
			t.Errorf("checkDocBaseURL(%q) 应报错", u)
		}
	}
}

package workflow

import "testing"

func TestValidToolCacheName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"gomod", true},
		{"pnpm-store", true},
		{"Go1.22_cache", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a..b", false},
		{".hidden", false},
		{"-x", false},
		{"a/b", false},
		{`a\b`, false},
		{"a:b", false},
		{"a b", false},
		{"a\nb", false},
		{string(make([]byte, 65)), false},
	}
	for _, c := range cases {
		if got := ValidToolCacheName(c.name); got != c.want {
			t.Errorf("ValidToolCacheName(%q) = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestValidToolCacheEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		// 本仓库 workflow 中使用的合法配置。
		{"GOCACHE", true},
		{"GOMODCACHE", true},
		{"PNPM_CONFIG_STORE_DIR", true},
		{"_X", true},
		{"", false},
		{"1A", false},
		{"A-B", false},
		{"__X", false},
		// 会被 Runner 用于自身运行或影响后续任务的变量。
		{"PATH", false},
		{"HOME", false},
		{"TMPDIR", false},
		{"LD_PRELOAD", false},
		{"LD_LIBRARY_PATH", false},
		{"LD_AUDIT", false}, // LD_ 前缀整类禁止（与 parse.go 的报错文案一致）
		{"DYLD_INSERT_LIBRARIES", false},
		{"GIT_CONFIG_GLOBAL", false},
		{"KILN_WORKSPACE", false},
	}
	for _, c := range cases {
		if got := ValidToolCacheEnv(c.name); got != c.want {
			t.Errorf("ValidToolCacheEnv(%q) = %v，期望 %v", c.name, got, c.want)
		}
	}
}

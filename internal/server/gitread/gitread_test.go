package gitread

import (
	"errors"
	"strings"
	"testing"
)

func TestBoundedCache(t *testing.T) {
	c := newBoundedCache[int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v", v, ok)
	}
	c.Put("c", 3) // 满了，整体清空后再写入
	if _, ok := c.Get("a"); ok {
		t.Fatal("缓存满后应清空旧条目")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("Get(c) = %d, %v", v, ok)
	}
}

func TestBoundedCacheGetOrLoad(t *testing.T) {
	t.Parallel()
	c := newBoundedCache[string](4)
	calls := 0
	boom := errors.New("boom")
	fail := true
	load := func() (string, error) {
		calls++
		if fail {
			return "", boom
		}
		return "v", nil
	}

	// 读取失败：返回错误，不缓存，下一次重新读取
	if _, err := c.getOrLoad("k", load); !errors.Is(err, boom) {
		t.Fatalf("读取失败时应返回 load 的错误，实际 %v", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("读取失败的结果不应缓存")
	}
	fail = false
	for i := range 3 {
		v, err := c.getOrLoad("k", load)
		if err != nil || v != "v" {
			t.Fatalf("第 %d 次 getOrLoad = %q, %v", i+1, v, err)
		}
	}
	// 一次失败加一次成功；之后都命中缓存
	if calls != 2 {
		t.Fatalf("load 调用了 %d 次，期望 2 次（失败一次、成功一次，之后命中缓存）", calls)
	}
}

// TestIsFullSHA：只有 40 位小写十六进制算完整 sha（分支名、短 sha、大写都不算）。
func TestIsFullSHA(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("0123456789", 4)
	cases := map[string]bool{
		full:                            true,
		strings.Repeat("abcdef0123", 4): true,
		full[:39]:                       false,
		full + "0":                      false,
		strings.ToUpper(strings.Repeat("abcdef0123", 4)): false,
		strings.Repeat("g", 40):                          false,
		"main":                                           false,
		"":                                               false,
	}
	for in, want := range cases {
		if got := IsFullSHA(in); got != want {
			t.Errorf("IsFullSHA(%q) = %v，想要 %v", in, got, want)
		}
	}
}

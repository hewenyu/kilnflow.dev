package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// gcEntry 在 root 下建一个条目：size 字节的文件，最后使用时间为 age 之前。
func gcEntry(t *testing.T, root, name string, size int, age time.Duration, now time.Time) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "f"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := now.Add(-age)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	return p
}

func removedNames(r *GCResult) []string {
	var out []string
	for _, e := range r.Removed {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

// 到期的 runtime（Retention）和安装残留（TempRetention）删除；超过容量时从最久没用的删起；Keep 中的不删（即使到期或
// 最久没用），但计入总量；安装锁不动。
func TestGC(t *testing.T) {
	t.Parallel()
	now := time.Now()
	root := t.TempDir()
	hex := func(c string) string { return strings.Repeat(c, 64) }
	gcEntry(t, root, hex("a"), 100, Retention+time.Hour, now)   // 到期
	gcEntry(t, root, hex("b"), 100, Retention+time.Hour, now)   // 到期但在 Keep 中
	gcEntry(t, root, hex("c"), 400, 10*24*time.Hour, now)       // 最久没用：超过容量时先删
	gcEntry(t, root, hex("d"), 400, 9*24*time.Hour, now)        // 在 Keep 中：跳过
	gcEntry(t, root, hex("e"), 400, time.Hour, now)             // 最近用过
	gcEntry(t, root, ".tmp-x", 10, 13*time.Hour, now)           // 安装残留，过了 12 小时
	gcEntry(t, root, ".tmp-y", 10, time.Hour, now)              // 正在安装的残留，保留
	gcEntry(t, root, LocksDirName, 0, Retention+time.Hour, now) // 安装锁不动

	res, err := GC(root, GCOptions{
		MaxBytes:      1000,
		TempRetention: 12 * time.Hour,
		Keep:          []string{"sha256:" + hex("b"), "sha256:" + hex("d")},
		Now:           now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := removedNames(res), []string{".tmp-x", hex("a"), hex("c")}; !slices.Equal(got, want) {
		t.Errorf("删除了 %v，期望 %v", got, want)
	}
	if res.RemovedBytes != 510 || res.Kept != 2 || len(res.Failures) != 0 {
		t.Errorf("结果 = %+v", res)
	}
	for _, name := range []string{hex("b"), hex("d"), hex("e"), ".tmp-y", LocksDirName} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s 不应删除：%v", name, err)
		}
	}
}

// 删除失败记在结果中，不算释放的容量，继续删下一个最久没用的。
func TestGCRemoveFailure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	root := t.TempDir()
	stuck := gcEntry(t, root, strings.Repeat("a", 64), 400, 3*time.Hour, now)
	gcEntry(t, root, strings.Repeat("b", 64), 400, 2*time.Hour, now)
	gcEntry(t, root, strings.Repeat("c", 64), 400, time.Hour, now)
	res, err := GC(root, GCOptions{MaxBytes: 500, Now: now, Remove: func(p string) error {
		if p == stuck {
			return errors.New("busy")
		}
		return os.RemoveAll(p)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := removedNames(res); !slices.Equal(got, []string{strings.Repeat("b", 64), strings.Repeat("c", 64)}) {
		t.Errorf("删除了 %v", got)
	}
	if len(res.Failures) != 1 || res.Failures[0].Path != stuck || res.Failures[0].Error != "busy" {
		t.Errorf("失败 = %+v", res.Failures)
	}
}

// 安装目录不存在时没有可回收的。
func TestGCMissingRoot(t *testing.T) {
	t.Parallel()
	res, err := GC(filepath.Join(t.TempDir(), "nope"), GCOptions{})
	if err != nil || len(res.Removed) != 0 {
		t.Errorf("GC = %+v, %v", res, err)
	}
}

package toolchain

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// 安装目录的保留与容量规则（PLAN #12.8）：CI Runner 的本地 GC（internal/runner）和 AI 执行节点的回收
// （kiln-runner toolchain gc，PLAN #33.12）都用这里的规则，只定义一次。

// ExpiresAt 返回安装目录下名为 name、最后使用时间为 lastUsed 的条目按保留策略到期的时间：装好的 runtime 是工具，
// 保留 Retention；安装残留（IsTemp）是过程数据，保留 tempRetention（由调用方给出过程数据的保留期）。
func ExpiresAt(name string, lastUsed time.Time, tempRetention time.Duration) time.Time {
	if IsTemp(name) {
		return lastUsed.Add(tempRetention)
	}
	return lastUsed.Add(Retention)
}

// EvictLRU 按容量回收装好的 runtime：总量超过 maxBytes 时从最久没用的删起，直到不超过。items 只包含装好的 runtime；
// info 返回一项的大小和最后使用时间；remove 删除一项，返回 false 时（删除失败或要保留）不计入释放的容量，继续删下一项。
// 返回删掉的项。
func EvictLRU[T any](items []T, info func(T) (size int64, lastUsed time.Time), maxBytes int64, remove func(T) bool) []T {
	var total int64
	for _, it := range items {
		size, _ := info(it)
		total += size
	}
	order := slices.Clone(items)
	slices.SortStableFunc(order, func(a, b T) int {
		_, ta := info(a)
		_, tb := info(b)
		return ta.Compare(tb)
	})
	var removed []T
	for _, it := range order {
		if total <= maxBytes {
			break
		}
		if remove(it) {
			size, _ := info(it)
			total -= size
			removed = append(removed, it)
		}
	}
	return removed
}

// Entry 是安装目录下的一项：装好的 runtime（<digest hex>）或安装残留（以 . 开头）。安装锁（LocksDirName）不在其中。
type Entry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	LastUsed time.Time `json:"last_used"`
}

// Temp 判断是不是安装残留。
func (e *Entry) Temp() bool { return IsTemp(e.Name) }

// ScanRoot 列出安装目录 root 下的条目（不含安装锁），计算大小。root 不存在时为空。
func ScanRoot(root string) ([]*Entry, error) {
	des, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, de := range des {
		if de.Name() == LocksDirName {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue // 列出之后被删除
		}
		p := filepath.Join(root, de.Name())
		size := info.Size()
		if info.IsDir() {
			size = dirSize(p)
		}
		out = append(out, &Entry{Name: de.Name(), Path: p, Size: size, LastUsed: info.ModTime()})
	}
	return out, nil
}

// dirSize 返回目录中普通文件的总大小（读不了的部分忽略）。
func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // 读不了的部分不计入，继续统计其余的
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// GCOptions 控制一次安装目录的回收（GC）。
type GCOptions struct {
	// MaxBytes 是装好的 runtime 的总量上限，0 表示 DefaultMaxSize。
	MaxBytes int64
	// TempRetention 是安装残留的保留期（过程数据）。
	TempRetention time.Duration
	// Keep 中的 digest（sha256:…）正在使用（例如进行中的任务），不删除，但计入总量。
	Keep []string
	// Now 是当前时间，为零时取 time.Now()。
	Now time.Time
	// Remove 删除一个条目（目录整个删除），为 nil 时用 os.RemoveAll。
	Remove func(path string) error
}

// GCFailure 是删除失败的条目。
type GCFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// GCResult 是一次回收的结果。
type GCResult struct {
	Removed      []*Entry    `json:"removed"`
	RemovedBytes int64       `json:"removed_bytes"`
	Kept         int         `json:"kept"`
	Failures     []GCFailure `json:"failures"`
}

// GC 回收安装目录 root（PLAN #12.8、#33.12）：到期的条目删除（ExpiresAt），装好的 runtime 总量超过上限时从最久没用的
// 删起（EvictLRU）；Keep 中的不删，安装锁不动。调用方保证回收期间没有同一个 root 上的安装。只有列出目录失败时返回错误，
// 删除失败记在结果中。
func GC(root string, o GCOptions) (*GCResult, error) {
	entries, err := ScanRoot(root)
	if err != nil {
		return nil, err
	}
	now := cmp.Or(o.Now, time.Now())
	remove := o.Remove
	if remove == nil {
		remove = os.RemoveAll
	}
	keep := map[string]bool{}
	for _, d := range o.Keep {
		keep[strings.TrimPrefix(d, cas.DigestPrefix)] = true
	}
	res := &GCResult{Removed: []*Entry{}, Failures: []GCFailure{}}
	del := func(e *Entry) bool {
		if keep[e.Name] {
			return false
		}
		if err := remove(e.Path); err != nil {
			res.Failures = append(res.Failures, GCFailure{Path: e.Path, Error: err.Error()})
			return false
		}
		res.Removed = append(res.Removed, e)
		res.RemovedBytes += e.Size
		return true
	}
	var installed []*Entry
	for _, e := range entries {
		if keep[e.Name] {
			res.Kept++
		}
		if ExpiresAt(e.Name, e.LastUsed, o.TempRetention).After(now) || !del(e) {
			if !e.Temp() {
				installed = append(installed, e)
			}
		}
	}
	EvictLRU(installed, func(e *Entry) (int64, time.Time) { return e.Size, e.LastUsed }, cmp.Or(o.MaxBytes, DefaultMaxSize), del)
	return res, nil
}

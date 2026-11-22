package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 本地缓存的清单、容量回收和按范围清理（PLAN #8.3）。
//
// 数据目录中的每一项缓存或过程数据都是一个 cacheItem：toolcache 目录、解包的 runtime、git 镜像、CAS 对象、
// buildctl、保留的工作区，以及旧布局之前的 toolcache。镜像不在数据目录中，单独记录在 images.json。
// 清单上报给 Server（CacheReport），在 Dashboard 和 kiln runner cache 中显示；清理请求按清单中的字段筛选。

// 缓存的类别（CacheEntry.kind）。
const (
	kindToolcache = "toolcache"
	kindRuntime   = "runtime"
	kindGit       = "git"
	kindCAS       = "cas"
	kindBuildkit  = "buildkit"
	kindWork      = "work"
	kindImage     = "image"
	kindLegacy    = "legacy" // 旧布局的 toolcache（toolcache/<repo>/<平台>/<名称>），不再使用，清理时直接删除
)

const (
	// gcFileName 是数据目录中记录上一次清理结果的文件。
	gcFileName = "gc.json"
	// attemptMetaName 是工作区中记录 job 所属仓库的文件（work/<attempt>/kiln-job.json）。
	attemptMetaName = "kiln-job.json"
)

// cacheItem 是数据目录中的一项缓存。
type cacheItem struct {
	kind     string
	repoID   int64
	platform string
	domain   string
	name     string
	path     string // 绝对路径
	size     int64
	lastUsed time.Time
	// temp 表示 runtime 目录中安装过程的残留（下载到一半的发行包、解包目录），按过程数据的保留期删除。
	temp bool
}

// installedRuntime 判断是不是装好的 runtime：它是工具，保留期和容量都与缓存分开（PLAN #12.8）。
func (it *cacheItem) installedRuntime() bool { return it.kind == kindRuntime && !it.temp }

// expiresAt 返回按保留策略到期的时间；旧布局的 toolcache 立即到期，装好的 runtime 按 toolchain.Retention。
func (it *cacheItem) expiresAt() time.Time {
	switch it.kind {
	case kindLegacy:
		return it.lastUsed // 不再使用，下次清理时删除
	case kindWork:
		return it.lastUsed.Add(workRetention)
	case kindRuntime:
		// 装好的 runtime 和安装残留的规则只在 toolchain 中定义（AI 执行节点的回收用同一条）
		return toolchain.ExpiresAt(it.name, it.lastUsed, CacheRetention)
	}
	return it.lastUsed.Add(CacheRetention)
}

// entry 返回上报用的清单项；dir 是数据目录。
func (it *cacheItem) entry(dir string) *runnerv1.CacheEntry {
	rel, err := filepath.Rel(dir, it.path)
	if err != nil {
		rel = it.path
	}
	return &runnerv1.CacheEntry{
		Kind: it.kind, RepoId: it.repoID, Platform: it.platform, Domain: it.domain, Name: it.name,
		Path: filepath.ToSlash(rel), SizeBytes: it.size, LastUsedAt: it.lastUsed.Unix(), ExpiresAt: it.expiresAt().Unix(),
	}
}

// scanCache 列出数据目录中的全部缓存项（计算大小，可能较慢）。目录不存在时跳过。
func scanCache(ctx context.Context, cfg *Config) ([]*cacheItem, error) {
	var items []*cacheItem
	var errs []error
	add := func(kind, p string, f func(*cacheItem)) {
		info, err := os.Lstat(p)
		if err != nil {
			return
		}
		it := &cacheItem{kind: kind, path: p, lastUsed: info.ModTime(), name: filepath.Base(p)}
		if info.IsDir() {
			it.size = dirSize(p)
		} else {
			it.size = info.Size()
		}
		if f != nil {
			f(it)
		}
		items = append(items, it)
	}
	// toolcache/<执行身份>/<repo>/<平台>/<名称>；第一层是数字的是旧布局 toolcache/<repo>/<平台>/<名称>
	walkLevels(cfg.path(toolcacheDirName), 1, func(parts []string, p string) {
		if _, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
			walkLevels(p, 2, func(sub []string, q string) {
				repo, _ := strconv.ParseInt(parts[0], 10, 64)
				add(kindLegacy, q, func(it *cacheItem) { it.repoID, it.platform = repo, platformName(sub[0]) })
			})
			return
		}
		walkLevels(p, 3, func(sub []string, q string) {
			repo, err := strconv.ParseInt(sub[0], 10, 64)
			if err != nil {
				return
			}
			add(kindToolcache, q, func(it *cacheItem) { it.domain, it.repoID, it.platform = parts[0], repo, platformName(sub[1]) })
		})
	})
	walkLevels(cfg.path(rtDirName), 1, func(parts []string, p string) {
		if parts[0] == toolchain.LocksDirName {
			return // 安装锁（空文件），不是缓存，也不能删（见 toolchain.LocksDirName）
		}
		add(kindRuntime, p, func(it *cacheItem) { it.temp = toolchain.IsTemp(parts[0]) })
	})
	walkLevels(cfg.path(gitDirName), 1, func(parts []string, p string) {
		add(kindGit, p, func(it *cacheItem) {
			it.repoID, _ = strconv.ParseInt(strings.TrimSuffix(parts[0], ".git"), 10, 64)
		})
	})
	walkLevels(cfg.path(buildkitDirName), 1, func(_ []string, p string) { add(kindBuildkit, p, nil) })
	walkLevels(cfg.path(workDirName), 1, func(_ []string, p string) {
		add(kindWork, p, func(it *cacheItem) { it.repoID = readAttemptRepo(p) })
	})
	err := (&cas.FSStore{Root: cfg.path(casDirName)}).Walk(ctx, func(o cas.Object) error {
		items = append(items, &cacheItem{kind: kindCAS, path: o.Path, name: filepath.Base(o.Path), size: o.Size, lastUsed: o.ModTime})
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return items, errors.Join(errs...)
}

// walkLevels 对 root 下第 depth 层的每个条目调用 fn（parts 是 root 之后的各级名称）；只进入目录，跳过符号链接。
func walkLevels(root string, depth int, fn func(parts []string, p string)) {
	var walk func(dir string, parts []string)
	walk = func(dir string, parts []string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			sub := append(slices.Clone(parts), e.Name())
			if len(sub) == depth {
				fn(sub, p)
				continue
			}
			if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
				walk(p, sub)
			}
		}
	}
	walk(root, nil)
}

// platformName 把目录名（os-arch）转换回平台（os/arch）。
func platformName(dir string) string {
	return strings.Replace(dir, "-", "/", 1)
}

// attemptMeta 是 work/<attempt>/kiln-job.json 的内容。
type attemptMeta struct {
	RepoID int64 `json:"repo_id"`
	JobID  int64 `json:"job_id"`
}

// writeAttemptMeta 记录工作区所属的仓库（按仓库清理时用到）；尽力而为。
func writeAttemptMeta(root string, m attemptMeta) {
	b, err := json.Marshal(m)
	if err == nil {
		_ = os.WriteFile(filepath.Join(root, attemptMetaName), b, 0o600)
	}
}

func readAttemptRepo(root string) int64 {
	b, err := os.ReadFile(filepath.Join(root, attemptMetaName))
	if err != nil {
		return 0
	}
	var m attemptMeta
	if json.Unmarshal(b, &m) != nil {
		return 0
	}
	return m.RepoID
}

// diskUsage 返回 dir 所在文件系统的总量和可用量（非特权用户可用的部分）。
func diskUsage(dir string) (total, free int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	return blockUsage(st.Blocks, st.Bavail, toInt64(st.Bsize))
}

// blockUsage 按块数和块大小算出总量和可用量。块大小不是正数时（文件系统报告的值不合理）按查不到处理，不用它算出的
// 容量做判断。
func blockUsage(blocks, avail uint64, bsize int64) (total, free int64, ok bool) {
	if bsize <= 0 {
		return 0, 0, false
	}
	return int64(blocks) * bsize, int64(avail) * bsize, true //nolint:gosec // 块数远小于 int64 的范围
}

// toInt64 转换 Statfs_t.Bsize：它在 darwin 上是 uint32，在 linux 上是 int64（直接转换在 linux 上会被 unconvert 报告）。
func toInt64[T uint32 | int64](v T) int64 { return int64(v) }

// GCFailure 是一个没有删除成功的对象。
type GCFailure struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

// GCReport 是一次清理（GC 或清理请求）的结果。
type GCReport struct {
	At           time.Time   `json:"at"`
	Removed      int         `json:"removed"`
	RemovedBytes int64       `json:"removed_bytes"`
	Failures     []GCFailure `json:"failures,omitempty"`
	// Notes 是需要告诉用户的说明，例如因为服务正在运行而跳过的容量回收。
	Notes []string `json:"notes,omitempty"`
}

// Err 在有对象没有删除成功时返回错误。
func (r *GCReport) Err() error {
	if len(r.Failures) == 0 {
		return nil
	}
	return fmt.Errorf("%d 个对象没有删除成功（详见上面的列表；下次清理时重试）", len(r.Failures))
}

func (r *GCReport) fail(kind, p string, err error) {
	r.Failures = append(r.Failures, GCFailure{Kind: kind, Path: p, Error: err.Error()})
}

// removeItem 删除一项缓存并记录结果，返回是否删除成功。
func (r *GCReport) removeItem(ctx context.Context, rm *remover, it *cacheItem) bool {
	n, err := rm.remove(ctx, it.path)
	if err != nil {
		r.fail(it.kind, it.path, err)
		return false
	}
	r.Removed++
	r.RemovedBytes += max(n, it.size)
	return true
}

// failures 转换为上报的格式。
func (r *GCReport) failures(dir string) []*runnerv1.CacheFailure {
	out := make([]*runnerv1.CacheFailure, 0, len(r.Failures))
	for _, f := range r.Failures {
		p := f.Path
		if rel, err := filepath.Rel(dir, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.ToSlash(rel)
		}
		out = append(out, &runnerv1.CacheFailure{Kind: f.Kind, Path: p, Error: f.Error})
	}
	return out
}

// saveGCReport 把上一次清理的结果写到 gc.json（尽力而为）。
func saveGCReport(cfg *Config, r *GCReport) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	tmp := cfg.path(gcFileName + ".tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, cfg.path(gcFileName))
	}
}

// LoadGCReport 读取上一次清理的结果；没有时返回 nil。
func LoadGCReport(cfg *Config) *GCReport {
	b, err := os.ReadFile(cfg.path(gcFileName))
	if err != nil {
		return nil
	}
	var r GCReport
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

// evictable 是可以为了容量删除的项（镜像和服务日志不在其中），按删除的先后排列：先是缓存，从最久没用的删起；
// 装好的 runtime 排在最后（同样从最久没用的删起）——缓存删了只是下次慢一点，runtime 删了要重新下载、安装。
func evictable(items []*cacheItem) []*cacheItem {
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(a, b *cacheItem) int {
		if ar, br := a.installedRuntime(), b.installedRuntime(); ar != br {
			if ar {
				return 1
			}
			return -1
		}
		return a.lastUsed.Compare(b.lastUsed)
	})
	return out
}

// evict 按容量回收（只在没有正在执行的 job 时调用）：超过 toolcache_max_size 的 toolcache 目录整个删除；
// 装好的 runtime 总量超过 runtime_max_size 时从最久没用的删起；
// 总量超过 cache_max_size、或磁盘使用率超过高水位时，按 evictable 的顺序删除，直到满足预算和低水位。
// 磁盘使用率由 usage 读取（GCOptions.DiskUsage）。返回剩下的项。
func evict(ctx context.Context, cfg *Config, rm *remover, items []*cacheItem, usage func(string) (int64, int64, bool), rep *GCReport) []*cacheItem {
	removed := map[*cacheItem]bool{}
	limit := cfg.toolcacheMaxBytes()
	for _, it := range items {
		if it.kind == kindToolcache && it.size > limit {
			if rep.removeItem(ctx, rm, it) {
				removed[it] = true
			}
		}
	}
	var runtimes []*cacheItem
	for _, it := range items {
		if it.installedRuntime() {
			runtimes = append(runtimes, it)
		}
	}
	// runtime 总量的上限：从最久没用的删起（规则在 toolchain 中，AI 执行节点的回收用同一条）
	toolchain.EvictLRU(runtimes, func(it *cacheItem) (int64, time.Time) { return it.size, it.lastUsed }, cfg.runtimeMaxBytes(),
		func(it *cacheItem) bool {
			if !rep.removeItem(ctx, rm, it) {
				return false
			}
			removed[it] = true
			return true
		})
	var total int64
	for _, it := range items {
		if !removed[it] {
			total += it.size
		}
	}
	// 需要释放的字节数：总量预算和磁盘水位中要求较多的那个
	var need int64
	if maxBytes := cfg.cacheMaxBytes(); maxBytes > 0 && total > maxBytes {
		need = total - maxBytes
	}
	high, low := cfg.diskWatermarks()
	if dt, free, ok := usage(cfg.Dir); ok && dt > 0 && high < 100 {
		used := dt - free
		if used*100 > dt*int64(high) {
			need = max(need, used-dt*int64(low)/100)
		}
	}
	for _, it := range evictable(items) {
		if need <= 0 {
			break
		}
		if removed[it] {
			continue
		}
		if rep.removeItem(ctx, rm, it) {
			removed[it] = true
			need -= it.size
		}
	}
	if need > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("本地缓存已全部删除，仍需释放约 %d MiB（磁盘上的其他数据，例如 docker 镜像和构建缓存，不由 Kiln 管理）", need>>20))
	}
	return slices.DeleteFunc(items, func(it *cacheItem) bool { return removed[it] })
}

// buildReport 生成上报给 Server 的清单：CAS 对象汇总为一项。
func buildReport(cfg *Config, items []*cacheItem, images imageUsage, imageSizes map[string]int64, last *GCReport) *runnerv1.CacheReport {
	rep := &runnerv1.CacheReport{GeneratedAt: time.Now().Unix()}
	var casItem *cacheItem
	for _, it := range items {
		if it.kind != kindCAS {
			rep.Entries = append(rep.Entries, it.entry(cfg.Dir))
			continue
		}
		if casItem == nil {
			casItem = &cacheItem{kind: kindCAS, name: "objects", path: cfg.path(casDirName)}
		}
		casItem.size += it.size
		if it.lastUsed.After(casItem.lastUsed) {
			casItem.lastUsed = it.lastUsed
		}
	}
	if casItem != nil {
		e := casItem.entry(cfg.Dir)
		e.ExpiresAt = 0 // 每个对象各自到期
		rep.Entries = append(rep.Entries, e)
	}
	for _, img := range slices.Sorted(maps.Keys(images)) {
		at := images[img]
		rep.Entries = append(rep.Entries, &runnerv1.CacheEntry{
			Kind: kindImage, Name: img, SizeBytes: imageSizes[img], LastUsedAt: at.Unix(), ExpiresAt: at.Add(CacheRetention).Unix(),
		})
	}
	slices.SortStableFunc(rep.Entries, func(a, b *runnerv1.CacheEntry) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.RepoId, b.RepoId), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Name, b.Name))
	})
	if total, free, ok := diskUsage(cfg.Dir); ok {
		rep.DiskTotalBytes, rep.DiskFreeBytes = total, free
	}
	if last != nil {
		rep.GcAt = last.At.Unix()
		rep.GcRemoved = int64(last.Removed)
		rep.GcRemovedBytes = last.RemovedBytes
		rep.GcFailures = last.failures(cfg.Dir)
	}
	return rep
}

// matchPurge 判断缓存项是否在清理请求的范围内。
func matchPurge(p *runnerv1.CachePurge, kind string, repoID int64, domain string) bool {
	if len(p.Kinds) > 0 && !slices.Contains(p.Kinds, kind) {
		return false
	}
	if p.RepoId != 0 && repoID != p.RepoId {
		return false
	}
	if p.Domain != "" && domain != p.Domain {
		return false
	}
	return true
}

// Purge 执行一个缓存清理请求（只在没有正在执行的 job 时调用）：删除范围内的缓存项和镜像。
func Purge(ctx context.Context, cfg *Config, rm *remover, p *runnerv1.CachePurge) *GCReport {
	rep := &GCReport{At: time.Now()}
	items, err := scanCache(ctx, cfg)
	if err != nil {
		rep.fail("", cfg.Dir, err)
	}
	for _, it := range items {
		if matchPurge(p, it.kind, it.repoID, it.domain) {
			rep.removeItem(ctx, rm, it)
		}
	}
	if rm.docker != "" && matchPurge(p, kindImage, 0, "") {
		removeImages(ctx, cfg, rm.docker, func(string, time.Time) bool { return true }, rep)
	}
	return rep
}

// removeImages 删除 images.json 中 match 返回 true 的镜像（不加 -f：还有容器在用时失败，记为失败，下次再试）。
func removeImages(ctx context.Context, cfg *Config, docker string, match func(img string, at time.Time) bool, rep *GCReport) []string {
	unlock, err := lockImages(cfg)
	if err != nil {
		if rep != nil {
			rep.fail(kindImage, cfg.path(imagesFileName), err)
		}
		return nil
	}
	defer unlock()
	path := cfg.path(imagesFileName)
	m := loadImages(path)
	var removed []string
	for _, img := range slices.Sorted(maps.Keys(m)) {
		if !match(img, m[img]) {
			continue
		}
		out, err := dockerGC(ctx, docker, "image", "rm", img)
		if err != nil && !strings.Contains(strings.ToLower(out+err.Error()), "no such image") {
			if rep != nil {
				rep.fail(kindImage, img, err)
			}
			continue
		}
		delete(m, img)
		removed = append(removed, img)
		if rep != nil {
			rep.Removed++
		}
	}
	if len(removed) > 0 {
		if err := m.save(path); err != nil && rep != nil {
			rep.fail(kindImage, path, err)
		}
	}
	return removed
}

// imageSizes 返回 images.json 中各镜像的大小（docker 不可用或镜像已不存在时没有）。
func imageSizes(ctx context.Context, docker string, images imageUsage) map[string]int64 {
	out := map[string]int64{}
	if docker == "" {
		return out
	}
	for img := range images {
		s, err := dockerGC(ctx, docker, "image", "inspect", "--format", "{{.Size}}", img)
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			out[img] = n
		}
	}
	return out
}

// Inventory 返回数据目录的缓存清单（含 images.json 中的镜像和上一次清理的结果）。docker 为空时不读取镜像大小。
func Inventory(ctx context.Context, cfg *Config, docker string) (*runnerv1.CacheReport, error) {
	items, err := scanCache(ctx, cfg)
	unlock, lerr := lockImages(cfg)
	if lerr != nil {
		return nil, lerr
	}
	images := loadImages(cfg.path(imagesFileName))
	unlock()
	return buildReport(cfg, items, images, imageSizes(ctx, docker, images), LoadGCReport(cfg)), err
}

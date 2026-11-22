package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/logging"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// Runner 本地的保留策略（PLAN #8.3）：本地 CAS 缓存、git 镜像、toolcache 和 buildctl，
// 超过 CacheRetention 没有用到就删除（使用时刷新 mtime）；装好的 runtime 是工具，不是过程数据，
// 超过 toolchain.Retention 没有用到才删除（PLAN #12.8）；镜像不论是否还在用，拉取（或第一次用到）
// 超过 CacheRetention 就删除，下次用到时重新拉取，保证用的是 tag 最新的镜像（记录在 images.json）；
// 保留的失败现场按 workRetention 删除；Runner 自己的日志按 logging.FileRetention 删除。
// 清理只在 Runner 空闲时进行，不会删掉正在执行的 job 用到的东西。

const (
	// CacheRetention 是本地缓存没有用到时的保留时间，也是镜像从拉取起的保留时间。
	CacheRetention = runnerkit.Retention
	// gcDockerTimeout 是清理时单个 docker 命令的超时。
	gcDockerTimeout = 2 * time.Minute
	// imagesFileName 是数据目录中记录镜像最后使用时间的文件。
	imagesFileName = "images.json"
	// runnerLabel 是 Runner 创建的容器、网络和 volume 的标签名，值为 Runner ID。
	runnerLabel = "dev.kiln.runner"
)

// DockerIfAvailable 返回可用的 docker 命令；关闭了 oci executor 或 docker 不可用时返回空字符串。
func DockerIfAvailable(ctx context.Context, cfg *Config) string {
	if cfg.Docker == dockerDisabled || !dockerOK(ctx, cfg.Docker, cfg.probeTimeoutOrDefault()) {
		return ""
	}
	return cfg.Docker
}

// runnerLabelValue 返回 Runner 创建的 docker 对象的标签（runnerLabel=<Runner ID>）。
func runnerLabelValue(runnerID int64) string {
	return runnerLabel + "=" + strconv.FormatInt(runnerID, 10)
}

// lockImages 锁住 images.json（flock，跨进程）：记录 job 用到的镜像与清理可能同时进行，也可能来自不同的进程
// （服务在运行时手动执行的 kiln-runner gc）。
func lockImages(cfg *Config) (func(), error) {
	return runnerkit.FlockWait(cfg.path(imagesFileName + ".lock"))
}

// imageUsage 是 Runner 用过的镜像和拉取（或第一次用到）的时间，保存在 <数据目录>/images.json。
// 只有这里记录的镜像会被清理，宿主机上没有被 Kiln 用过的镜像不受影响。
type imageUsage map[string]time.Time

// loadImages 读取记录；文件不存在或损坏时返回空记录（损坏只会让这些镜像不被清理）。调用方持有 images.json 的锁。
func loadImages(path string) imageUsage {
	m := imageUsage{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if json.Unmarshal(b, &m) != nil || m == nil {
		return imageUsage{}
	}
	return m
}

// save 原子地写入记录（临时文件用唯一的文件名：并发的进程不会写到同一个 .tmp）。调用方持有 images.json 的锁。
func (m imageUsage) save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return runnerkit.WriteFileAtomic(path, b, 0o644)
}

// recordImages 记录这些镜像被用到：还没有记录的记下现在的时间（镜像在这之前拉取），已有的记录不变——
// 镜像从这个时间起保留 CacheRetention，不因为一直在用而延长。失败只写日志（最多是这些镜像不会被自动清理）。
func (r *Runner) recordImages(images []string) {
	recordImages(r.cfg, r.log, images)
}

// recordImages 见 Runner.recordImages。
func recordImages(cfg *Config, log *slog.Logger, images []string) {
	unlock, err := lockImages(cfg)
	if err != nil {
		log.Warn("锁住 images.json", "err", err)
		return
	}
	defer unlock()
	path := cfg.path(imagesFileName)
	m := loadImages(path)
	now := time.Now().UTC()
	changed := false
	for _, img := range images {
		if _, ok := m[img]; img != "" && !ok {
			m[img] = now
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := m.save(path); err != nil {
		log.Warn("记录用到的镜像", "err", err)
	}
}

// GCOptions 控制一次清理。
type GCOptions struct {
	// Docker 是 docker 命令；为空时不清理镜像和容器，也不能用清理容器删除 root 写入的文件。
	Docker string
	// RunnerID 非 0 时删除带这个 Runner 标签的容器、网络和 volume，以及工作区中的临时 keychain（进程被强行结束时残留的）。
	// 只能在这个 Runner 没有正在执行的 job 时设置。
	RunnerID int64
	// Evict 为 true 时按容量回收（toolcache_max_size、cache_max_size、磁盘水位），可能删除最近还在用的缓存，
	// 只能在这个 Runner 没有正在执行的 job 时设置。
	Evict bool
	// LabelID 非 0 时给清理容器加上这个 Runner 的标签（RunnerID 为 0 时也可以设置）。
	LabelID int64
	// DiskUsage 返回 dir 所在文件系统的总量和可用量（按磁盘水位回收时用），为 nil 时读取真实的文件系统（statfs）。
	DiskUsage func(dir string) (total, free int64, ok bool)
}

// GC 按保留策略（以及 opts.Evict 时按容量）清理数据目录和 docker 中的过程数据，返回结果并写入 gc.json。
// 只读写数据目录和 docker，不需要连接 Server。
func GC(ctx context.Context, cfg *Config, opts GCOptions, log *slog.Logger) *GCReport {
	rep := &GCReport{At: time.Now()}
	if opts.RunnerID != 0 {
		// 这个 Runner 没有正在执行的 job：工作区中的临时 keychain 都是进程被强行结束时残留的
		cleanStaleKeychains(ctx, cfg.path(workDirName), log)
	}
	rm := newRemover(cfg, opts.Docker, cmp.Or(opts.RunnerID, opts.LabelID))
	items, err := scanCache(ctx, cfg)
	if err != nil {
		rep.fail("", cfg.Dir, err)
	}
	now := time.Now()
	kept := items[:0]
	for _, it := range items {
		if it.expiresAt().After(now) {
			kept = append(kept, it)
			continue
		}
		if !rep.removeItem(ctx, rm, it) {
			kept = append(kept, it)
		}
	}
	// 删除后变空的上层目录（toolcache/<执行身份>/<repo>/<平台>）
	removeEmptyDirs(cfg.path(toolcacheDirName), toolcacheParentDepth)
	if opts.Evict {
		usage := opts.DiskUsage
		if usage == nil {
			usage = diskUsage
		}
		evict(ctx, cfg, rm, kept, usage, rep)
		removeEmptyDirs(cfg.path(toolcacheDirName), toolcacheParentDepth)
	}
	// Runner 的日志：按天切分的文件由 logging.DailyFile 自己清理，这里删除其他过期的文件（例如旧版本的 runner.log）；
	// 日志用于排查 Runner 本身，与它们一样保留 logging.FileRetention，不按缓存的保留期
	n, err := pruneEntries(cfg.LogDir(), 1, now.Add(-logging.FileRetention), func(name string) bool { return name == filepath.Base(cfg.home().ConsolePath()) })
	if err != nil {
		rep.fail("log", cfg.LogDir(), err)
	}
	rep.Removed += n
	if opts.Docker != "" {
		gcDocker(ctx, cfg, opts, now.Add(-CacheRetention), rep, log)
	}
	if rm.used {
		// 清理容器的镜像同样按拉取时间超过 CacheRetention 后删除
		recordImages(cfg, log, []string{rm.image})
	}
	if rep.Removed > 0 {
		log.Info("清理本地缓存", "removed", rep.Removed, "removed_mib", rep.RemovedBytes>>20, "retention", CacheRetention)
	}
	for _, f := range rep.Failures {
		log.Warn("清理失败，下次清理时重试", "kind", f.Kind, "path", f.Path, "err", f.Error)
	}
	saveGCReport(cfg, rep)
	return rep
}

// toolcacheParentDepth 是 toolcache 目录之上的层数（<执行身份>/<repo>/<平台>）。
const toolcacheParentDepth = 3

// removeEmptyDirs 删除 root 下（不含 root）前 depth 层中变空的目录，自底向上；不进入更深的层（那是缓存的内容）。
func removeEmptyDirs(root string, depth int) {
	entries, err := os.ReadDir(root)
	if err != nil || depth <= 0 {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		p := filepath.Join(root, e.Name())
		removeEmptyDirs(p, depth-1)
		if rest, err := os.ReadDir(p); err == nil && len(rest) == 0 {
			_ = os.Remove(p)
		}
	}
}

// pruneEntries 删除 root 下第 depth 层中修改时间早于 cutoff 的条目（目录整个删除），以及因此变空的上层目录；
// keep 返回 true 的名称不删除。返回删除的条目数。root 不存在时什么也不做。
func pruneEntries(root string, depth int, cutoff time.Time, keep func(name string) bool) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if depth > 1 {
			if !e.IsDir() {
				continue
			}
			m, err := pruneEntries(p, depth-1, cutoff, keep)
			n += m
			if err != nil {
				errs = append(errs, err)
			}
			// 变空的上层目录一并删除（非空时 Remove 失败，忽略）
			if rest, err := os.ReadDir(p); err == nil && len(rest) == 0 {
				_ = os.Remove(p)
			}
			continue
		}
		if keep != nil && keep(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := runnerkit.RemoveTree(p); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// gcDocker 删除残留的容器、网络和 volume（设置了 RunnerID 时），以及拉取超过保留期的镜像。
func gcDocker(ctx context.Context, cfg *Config, opts GCOptions, cutoff time.Time, rep *GCReport, log *slog.Logger) {
	if opts.RunnerID != 0 {
		filter := "label=" + runnerLabelValue(opts.RunnerID)
		for _, kind := range []struct{ list, rm []string }{
			{[]string{"ps", "-aq", "--filter", filter}, []string{"rm", "-f", "-v"}},
			{[]string{"network", "ls", "-q", "--filter", filter}, []string{"network", "rm"}},
			{[]string{"volume", "ls", "-q", "--filter", filter}, []string{"volume", "rm", "-f"}},
		} {
			out, err := dockerGC(ctx, opts.Docker, kind.list...)
			if err != nil {
				rep.fail("docker", strings.Join(kind.list, " "), err)
				continue
			}
			ids := strings.Fields(out)
			if len(ids) == 0 {
				continue
			}
			if _, err := dockerGC(ctx, opts.Docker, append(kind.rm, ids...)...); err != nil {
				rep.fail("docker", strings.Join(kind.rm, " "), err)
				continue
			}
			log.Info("删除残留的 docker 对象", "cmd", strings.Join(kind.rm, " "), "count", len(ids))
		}
	}
	// 还有容器在用的镜像删除失败是正常的（下次再试），不计为失败
	removed := removeImages(ctx, cfg, opts.Docker, func(_ string, at time.Time) bool { return at.Before(cutoff) }, nil)
	if len(removed) == 0 {
		return
	}
	rep.Removed += len(removed)
	log.Info("删除拉取超过保留期的镜像（下次用到时重新拉取）", "images", removed, "retention", CacheRetention)
}

// dockerGC 执行一个清理用的 docker 命令，返回 stdout（stderr 中的警告不能混进要解析的对象 ID，见 dockerOutput）。
func dockerGC(ctx context.Context, docker string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gcDockerTimeout)
	defer cancel()
	return dockerOutput(ctx, docker, args...)
}

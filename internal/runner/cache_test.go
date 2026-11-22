package runner

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 镜像的默认用户决定 oci job 的执行身份：root（空、root、0）为 oci-root，其他用户各自独立。
func TestOCICacheDomain(t *testing.T) {
	t.Parallel()
	for user, want := range map[string]string{
		"": "oci-root", "root": "oci-root", "0": "oci-root", "0:0": "oci-root", "root:root": "oci-root",
		"node": "oci-node", "1000:1000": "oci-1000-1000", "0:1000": "oci-0-1000", "a/b c": "oci-a-b-c",
	} {
		if got := ociCacheDomain(user); got != want {
			t.Errorf("ociCacheDomain(%q) = %q，期望 %q", user, got, want)
		}
	}
}

// 同一个仓库、同名的 toolcache，oci job（容器中以 root 写入）和 host job 使用不同的目录，
// 互不影响；同类 job 之间仍然复用同一个目录。
func TestToolcacheIsolatedByExecutor(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	docker, calls := fakeDocker(t)
	r := newTestRunner(t, fs, func(c *Config) { c.Docker = docker })
	tcs := []*runnerv1.ToolCache{{Name: "gomod", Env: "GOMODCACHE"}, {Name: "gocache", Env: "GOCACHE"}}

	oci := hostJob(1, bash("build", "go build"))
	oci.Source = &runnerv1.Source{RepoId: 42}
	oci.Executor = &runnerv1.ExecutorSpec{Kind: "oci", Image: "golang@sha256:abc", Platform: runnerkit.NativePlatform()}
	oci.Toolcache = tcs
	got, log := runJob(t, r, fs, oci)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	plat := platformDir(runnerkit.NativePlatform())
	ociDir := r.cfg.path(toolcacheDirName, "oci-root", "42", plat, "gomod")
	if c := dockerCalls(t, calls); !slices.ContainsFunc(c, func(s string) bool { return strings.Contains(s, " -v "+ociDir+":/opt/kiln/toolcache/gomod ") }) {
		t.Fatalf("oci job 应挂载 %s：%q", ociDir, c)
	}

	for i := range int64(2) {
		host := hostJob(2+i, bash("build", `ls "$GOMODCACHE"; echo "DIR=$GOMODCACHE"; echo x >> "$GOMODCACHE/n"`))
		host.Source = &runnerv1.Source{RepoId: 42}
		host.Toolcache = tcs
		got, log = runJob(t, r, fs, host)
		wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
		hostDir := r.cfg.path(toolcacheDirName, "host", "42", plat, "gomod")
		if !strings.Contains(log, "DIR="+hostDir) {
			t.Errorf("host job 的 GOMODCACHE 应为 %s：\n%s", hostDir, log)
		}
	}
	b, err := os.ReadFile(r.cfg.path(toolcacheDirName, "host", "42", plat, "gomod", "n"))
	if err != nil || string(b) != "x\nx\n" {
		t.Errorf("两个 host job 应复用同一个目录：%q，%v", b, err)
	}
	if exists(filepath.Join(ociDir, "n")) {
		t.Error("host job 不应写入 oci 的目录")
	}
	// 非 root 用户的镜像使用自己的执行身份
	nonroot := hostJob(4, bash("build", "true"))
	nonroot.Source = &runnerv1.Source{RepoId: 42}
	nonroot.Executor = &runnerv1.ExecutorSpec{Kind: "oci", Image: "nonroot@sha256:abc", Platform: runnerkit.NativePlatform()}
	nonroot.Toolcache = tcs
	got, log = runJob(t, r, fs, nonroot)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if !exists(r.cfg.path(toolcacheDirName, "oci-1000-1000", "42", plat, "gomod")) {
		t.Error("非 root 镜像应使用 oci-1000-1000")
	}
}

// writeSized 在 p 中写入 n 字节的文件，并把 p 的修改时间设为 age 之前。
func writeSized(t *testing.T, p string, n int, age time.Duration) {
	t.Helper()
	mustWrite(t, filepath.Join(p, "data"), strings.Repeat("x", n))
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// 清单：toolcache 按执行身份/仓库/平台/名称，旧布局为 legacy，CAS 对象汇总为一项，镜像来自 images.json，
// 工作区记录所属仓库；附带上一次清理的结果。
func TestInventory(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir()}
	writeSized(t, cfg.path(toolcacheDirName, "oci-root", "7", "linux-arm64", "gocache"), 100, time.Hour)
	writeSized(t, cfg.path(toolcacheDirName, "7", "linux-arm64", "gocache"), 10, time.Hour)
	writeSized(t, cfg.path(gitDirName, "7.git"), 20, time.Hour)
	writeSized(t, cfg.path(rtDirName, "abcd"), 30, time.Hour)
	writeSized(t, cfg.path(workDirName, "5"), 40, time.Hour)
	writeAttemptMeta(cfg.path(workDirName, "5"), attemptMeta{RepoID: 7, JobID: 9})
	store := &cas.FSStore{Root: cfg.path(casDirName)}
	for _, b := range []string{"aaa", "bbbbb"} {
		p, _ := store.Path(cas.Digest([]byte(b)))
		mustWrite(t, p, b)
	}
	recordImages(cfg, slog.New(slog.DiscardHandler), []string{"golang:1"})
	saveGCReport(cfg, &GCReport{At: time.Unix(1000, 0), Removed: 3, RemovedBytes: 99, Failures: []GCFailure{{Kind: kindWork, Path: cfg.path(workDirName, "1"), Error: "permission denied"}}})

	rep, err := Inventory(t.Context(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		kind, domain, platform, name, path string
		repo, size                         int64
	}
	var got []row
	for _, e := range rep.Entries {
		got = append(got, row{e.Kind, e.Domain, e.Platform, e.Name, e.Path, e.RepoId, e.SizeBytes})
	}
	want := []row{
		{kindCAS, "", "", "objects", "cas", 0, 8},
		{kindGit, "", "", "7.git", "git/7.git", 7, 20},
		{kindImage, "", "", "golang:1", "", 0, 0},
		{kindLegacy, "", "linux/arm64", "gocache", "toolcache/7/linux-arm64/gocache", 7, 10},
		{kindRuntime, "", "", "abcd", "rt/abcd", 0, 30},
		{kindToolcache, "oci-root", "linux/arm64", "gocache", "toolcache/oci-root/7/linux-arm64/gocache", 7, 100},
		{kindWork, "", "", "5", "work/5", 7, 40 + int64(len(`{"repo_id":7,"job_id":9}`))},
	}
	if !slices.Equal(got, want) {
		t.Errorf("清单：\n%v\n期望：\n%v", got, want)
	}
	if rep.GcAt != 1000 || rep.GcRemoved != 3 || len(rep.GcFailures) != 1 || rep.GcFailures[0].Path != "work/1" {
		t.Errorf("上一次清理 = %v", rep)
	}
	for _, e := range rep.Entries {
		if e.Kind == kindLegacy && e.ExpiresAt > time.Now().Unix() {
			t.Error("旧布局的 toolcache 应立即到期")
		}
	}
}

// 容量回收：超过 toolcache_max_size 的 toolcache（即使刚用过）整个删除；总量超过 cache_max_size 时
// 按最后使用时间从旧到新删除，直到满足预算。
func TestEvict(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir(), ToolcacheMaxSize: "1Ki", CacheMaxSize: "3Ki", DiskHighWatermark: 100}
	hot := cfg.path(toolcacheDirName, "host", "1", "linux-arm64", "gocache")
	writeSized(t, hot, 2048, time.Minute) // 刚用过，但超过每个目录的上限
	oldest := cfg.path(gitDirName, "1.git")
	writeSized(t, oldest, 1024, 2*time.Hour)
	// 比 oldest 更久没用，但装好的 runtime 排在缓存之后
	middle := cfg.path(rtDirName, "old")
	writeSized(t, middle, 1024, 3*time.Hour)
	newest := cfg.path(toolcacheDirName, "host", "2", "linux-arm64", "gomod")
	writeSized(t, newest, 1000, time.Hour)
	recent := cfg.path(rtDirName, "new")
	writeSized(t, recent, 1000, time.Minute)

	rep := GC(t.Context(), cfg, GCOptions{Evict: true}, slog.New(slog.DiscardHandler))
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	// 删除 hot（超过上限）后剩 4048 字节，超过预算 3072：再删除最旧的缓存 oldest，剩 3024
	for p, keep := range map[string]bool{hot: false, oldest: false, middle: true, newest: true, recent: true} {
		if exists(p) != keep {
			t.Errorf("%s 存在 = %v，期望 %v", p, exists(p), keep)
		}
	}
	if rep.Removed != 2 || rep.RemovedBytes != 2048+1024 {
		t.Errorf("Removed = %d, %d 字节", rep.Removed, rep.RemovedBytes)
	}
	// 不按容量回收时（服务正在运行）不删除未过期的缓存
	writeSized(t, hot, 2048, time.Minute)
	if rep := GC(t.Context(), cfg, GCOptions{}, slog.New(slog.DiscardHandler)); rep.Removed != 0 || !exists(hot) {
		t.Errorf("Evict 为 false 时删除了 %d 个对象", rep.Removed)
	}
}

// runtime 的总量上限（PLAN #12.8）：超过 runtime_max_size 时从最久没用的 runtime 删起，不动缓存；安装过程的残留和安装锁
// 不计入。缓存删完仍然不够时才为总量预算删除 runtime。
func TestEvictRuntimes(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir(), RuntimeMaxSize: "2Ki", DiskHighWatermark: 100}
	var rts []string
	for i, age := range []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour} {
		p := cfg.path(rtDirName, strconv.Itoa(i))
		writeSized(t, p, 1000, age)
		rts = append(rts, p)
	}
	cache := cfg.path(gitDirName, "1.git")
	writeSized(t, cache, 4096, 5*time.Hour)
	temp := cfg.path(rtDirName, ".extract-1")
	writeSized(t, temp, 4096, time.Hour)
	lock := cfg.path(rtDirName, toolchain.LocksDirName)
	writeSized(t, lock, 4096, 5*time.Hour)

	log := slog.New(slog.DiscardHandler)
	rep := GC(t.Context(), cfg, GCOptions{Evict: true}, log)
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	// 3000 字节的 runtime 超过上限 2048：删除最久没用的一个
	for p, keep := range map[string]bool{rts[0]: false, rts[1]: true, rts[2]: true, cache: true, temp: true, lock: true} {
		if exists(p) != keep {
			t.Errorf("%s 存在 = %v，期望 %v", p, exists(p), keep)
		}
	}
	if rep.Removed != 1 {
		t.Errorf("Removed = %d，期望 1", rep.Removed)
	}

	// 总量预算：先删缓存（cache、temp），还不够才删 runtime；安装锁始终保留
	cfg.CacheMaxSize = "1500"
	rep = GC(t.Context(), cfg, GCOptions{Evict: true}, log)
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	for p, keep := range map[string]bool{rts[1]: false, rts[2]: true, cache: false, temp: false, lock: true} {
		if exists(p) != keep {
			t.Errorf("总量预算：%s 存在 = %v，期望 %v", p, exists(p), keep)
		}
	}
}

// 磁盘水位（PLAN #8.3）：使用率超过高水位时按最后使用时间从旧到新删除（即使没有过期），直到低于低水位；低于高水位时
// 只按保留期清理。磁盘用量由 GCOptions.DiskUsage 注入，与测试机器的磁盘无关。
func TestEvictDiskWatermark(t *testing.T) {
	t.Parallel()
	const total = 100 << 10 // 100KiB 的磁盘，默认水位 80%、65%
	usage := func(used int64) func(string) (int64, int64, bool) {
		return func(string) (int64, int64, bool) { return total, total - used, true }
	}
	cfg := &Config{Dir: t.TempDir()}
	var items []string
	for i, age := range []time.Duration{4 * time.Hour, 3 * time.Hour, 2 * time.Hour, time.Hour} {
		p := cfg.path(rtDirName, strconv.Itoa(i))
		writeSized(t, p, 10<<10, age)
		items = append(items, p)
	}
	log := slog.New(slog.DiscardHandler)

	// 79%：不超过高水位，什么也不删
	if rep := GC(t.Context(), cfg, GCOptions{Evict: true, DiskUsage: usage(79 << 10)}, log); rep.Removed != 0 {
		t.Fatalf("低于高水位时删除了 %d 个对象", rep.Removed)
	}
	// 85%：要释放 20KiB 到低水位 65%，从最旧的删起，删两个 10KiB 的
	rep := GC(t.Context(), cfg, GCOptions{Evict: true, DiskUsage: usage(85 << 10)}, log)
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	for i, keep := range []bool{false, false, true, true} {
		if exists(items[i]) != keep {
			t.Errorf("%s 存在 = %v，期望 %v", items[i], exists(items[i]), keep)
		}
	}
	if rep.Removed != 2 {
		t.Errorf("Removed = %d", rep.Removed)
	}
}

// 按仓库清理：只删除这个仓库的 toolcache（各执行身份）、git 镜像和工作区；runtime、CAS 等共享缓存保留。
// 按类别和执行身份清理。
func TestPurge(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir()}
	paths := map[string]int64{ // 路径 → 所属仓库
		cfg.path(toolcacheDirName, "host", "1", "linux-arm64", "gomod"):     1,
		cfg.path(toolcacheDirName, "oci-root", "1", "linux-arm64", "gomod"): 1,
		cfg.path(toolcacheDirName, "host", "2", "linux-arm64", "gomod"):     2,
		cfg.path(toolcacheDirName, "1", "linux-arm64", "gomod"):             1,
		cfg.path(gitDirName, "1.git"):                                       1,
		cfg.path(gitDirName, "2.git"):                                       2,
		cfg.path(workDirName, "11"):                                         1,
		cfg.path(rtDirName, "go"):                                           0,
	}
	for p := range paths {
		writeSized(t, p, 10, time.Hour)
	}
	writeAttemptMeta(cfg.path(workDirName, "11"), attemptMeta{RepoID: 1})
	rm := newRemover(cfg, "", 0)
	rep := Purge(t.Context(), cfg, rm, &runnerv1.CachePurge{Id: 1, RepoId: 1})
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	for p, repo := range paths {
		if exists(p) != (repo != 1) {
			t.Errorf("按仓库 1 清理后 %s 存在 = %v", p, exists(p))
		}
	}
	if rep.Removed != 5 {
		t.Errorf("Removed = %d，期望 5", rep.Removed)
	}
	// 按类别 + 执行身份
	writeSized(t, cfg.path(toolcacheDirName, "oci-root", "2", "linux-arm64", "gomod"), 10, time.Hour)
	rep = Purge(t.Context(), cfg, rm, &runnerv1.CachePurge{Id: 2, Kinds: []string{kindToolcache}, Domain: "oci-root"})
	if rep.Removed != 1 || !exists(cfg.path(toolcacheDirName, "host", "2", "linux-arm64", "gomod")) || !exists(cfg.path(rtDirName, "go")) {
		t.Errorf("按类别和执行身份清理：Removed = %d", rep.Removed)
	}
}

// Server 下发的清理请求在空闲时执行，结果和新的清单在下一次 Acquire 时上报；同一个请求只执行一次。
func TestRunnerPurges(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, newFakeServer(t))
	tc := r.cfg.path(toolcacheDirName, "host", "3", "linux-arm64", "gomod")
	writeSized(t, tc, 10, time.Hour)
	p := &runnerv1.CachePurge{Id: 8, RepoId: 3}
	r.queuePurges([]*runnerv1.CachePurge{p, p})
	if len(r.purges) != 1 {
		t.Fatalf("重复的请求应合并：%d", len(r.purges))
	}
	r.runPurges(t.Context())
	if exists(tc) || len(r.purgeResults) != 1 || r.purgeResults[0].Id != 8 || r.purgeResults[0].Removed != 1 || r.report == nil {
		t.Fatalf("runPurges：存在 = %v，结果 = %v，清单 = %v", exists(tc), r.purgeResults, r.report != nil)
	}
	r.queuePurges([]*runnerv1.CachePurge{p})
	if len(r.purges) != 0 {
		t.Error("执行过的请求不应再次执行")
	}
}

// 块大小不是正数（文件系统报告的值不合理）时按查不到处理；正常时按块数乘块大小。
func TestBlockUsage(t *testing.T) {
	for _, bs := range []int64{0, -4096} {
		if _, _, ok := blockUsage(100, 50, bs); ok {
			t.Errorf("块大小 %d：ok = true，想要 false", bs)
		}
	}
	total, free, ok := blockUsage(100, 50, 4096)
	if !ok || total != 100*4096 || free != 50*4096 {
		t.Errorf("blockUsage(100, 50, 4096) = %d, %d, %v；想要 %d, %d, true", total, free, ok, 100*4096, 50*4096)
	}
}

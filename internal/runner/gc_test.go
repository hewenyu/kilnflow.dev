package runner

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

func TestRecordImagesRecoversNull(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir()}
	p := cfg.path(imagesFileName)
	mustWrite(t, p, "null")
	recordImages(cfg, slog.New(slog.DiscardHandler), []string{"busybox:1"})
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]time.Time
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved["busybox:1"].IsZero() {
		t.Fatalf("恢复后的镜像记录 = %s", b)
	}
}

// aged 创建文件或目录（以 / 结尾），并把修改时间设为 age 之前。
func aged(t *testing.T, p string, age time.Duration) {
	t.Helper()
	if strings.HasSuffix(p, "/") {
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Clean(p), mt, mt); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// 超过保留期（12 小时）没有用到的本地缓存（CAS、runtime、git 镜像、toolcache、buildctl）被删除，最近用到的保留；
// 保留的失败现场超过 12 小时删除；Runner 自己的日志超过 7 天才删除；服务管理器的输出文件不删除。
func TestGCPrunesLocalCaches(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir(), Docker: dockerDisabled}
	old, recent := 8*24*time.Hour, time.Hour
	oldBlob, newBlob := cas.Digest([]byte("old")), cas.Digest([]byte("new"))
	store := &cas.FSStore{Root: cfg.path(casDirName)}
	oldPath, _ := store.Path(oldBlob)
	newPath, _ := store.Path(newBlob)
	tmpPath := filepath.Join(filepath.Dir(oldPath), ".upload-1")
	paths := map[string]bool{ // 路径 → 是否应保留
		oldPath:                             false,
		newPath:                             true,
		tmpPath:                             false,
		cfg.path(rtDirName, "aaaa") + "/":   false, // 31 天没有用到的 runtime
		cfg.path(rtDirName, "bbbb") + "/":   true,
		cfg.path(gitDirName, "1.git") + "/": false,
		cfg.path(gitDirName, "2.git") + "/": true,
		cfg.path(toolcacheDirName, "host", "1", "linux-arm64", "gomod") + "/":      false,
		cfg.path(toolcacheDirName, "host", "2", "linux-arm64", "gomod") + "/":      true,
		cfg.path(toolcacheDirName, "oci-root", "2", "linux-arm64", "gomod") + "/":  true,
		cfg.path(toolcacheDirName, "host", "2", "linux-arm64", "pnpm-store") + "/": false,
		cfg.path(toolcacheDirName, "3", "linux-arm64", "gomod") + "/":              false, // 旧布局：最近用过也删除
		cfg.path(buildkitDirName, "cccc-native") + "/":                             false,
		cfg.path(logsDirName, "runner.log"):                                        false,
		cfg.home().ConsolePath():                                                   true,
		cfg.path(logsDirName, "runner-"+time.Now().Format("2006-01-02")+".log"):    true,
		cfg.path(workDirName, "41") + "/":                                          false, // 超过 12 小时的失败现场
		cfg.path(rtDirName, "dddd") + "/":                                          true,  // 13 小时没有用到的 runtime：是工具，不按过程数据的保留期
		cfg.path(rtDirName, "eeee") + "/":                                          true,  // 8 天没有用到的 runtime
		cfg.path(rtDirName, ".extract-1") + "/":                                    false, // 安装过程的残留：按过程数据的保留期
		cfg.path(rtDirName, toolchain.LocksDirName) + "/":                          true,  // 安装锁不是缓存，多久都不删
		cfg.path(logsDirName, "old.log"):                                           true,  // 13 小时前的日志：按日志的保留期
	}
	for p, keep := range paths {
		age := old
		if (keep && !strings.Contains(p, "runner.out.log")) || strings.Contains(p, "/3/") {
			age = recent
		}
		if strings.HasSuffix(p, "/41/") || strings.HasSuffix(p, "/dddd/") || strings.HasSuffix(p, "/.extract-1/") || strings.HasSuffix(p, "old.log") {
			age = 13 * time.Hour
		}
		if strings.HasSuffix(p, "/eeee/") {
			age = old
		}
		if strings.HasSuffix(p, "/aaaa/") || strings.HasSuffix(p, "/"+toolchain.LocksDirName+"/") {
			age = toolchain.Retention + 24*time.Hour
		}
		aged(t, p, age)
	}
	rep := GC(t.Context(), cfg, GCOptions{}, slog.New(slog.DiscardHandler))
	if err := rep.Err(); err != nil {
		t.Fatalf("GC：%v（%v）", err, rep.Failures)
	}
	if want := 11; rep.Removed != want { // paths 中不保留的项
		t.Errorf("Removed = %d，期望 %d", rep.Removed, want)
	}
	if last := LoadGCReport(cfg); last == nil || last.Removed != rep.Removed {
		t.Errorf("gc.json = %+v", last)
	}
	for p, keep := range paths {
		if got := exists(filepath.Clean(p)); got != keep {
			t.Errorf("%s 存在 = %v，期望 %v", p, got, keep)
		}
	}
	// toolcache 中因此变空的上层目录一并删除
	if exists(cfg.path(toolcacheDirName, "host", "1")) || exists(cfg.path(toolcacheDirName, "3")) {
		t.Error("变空的 toolcache/host/1、toolcache/3 应删除")
	}
}

// fakeGCDocker 生成一个模拟清理用到的 docker 命令的脚本：列出两个残留容器和一个网络；
// busy 镜像还有容器在用（删除失败），gone 镜像已经不存在。
func fakeGCDocker(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "docker"), filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + log + `'
case "$*" in
"ps -aq --filter "*) printf 'c1\nc2\n' ;;
"network ls -q --filter "*) echo n1 ;;
"image rm busy:1") echo "Error response from daemon: conflict: unable to remove repository reference" >&2; exit 1 ;;
"image rm gone:1") echo "Error response from daemon: No such image: gone:1" >&2; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// 删除带本 Runner 标签的残留容器和网络；删除拉取超过保留期的镜像（不论最近是否用到；还有容器在用的保留记录，
// 下次再试），拉取不到保留期的镜像和没有记录的镜像不动。
func TestGCDocker(t *testing.T) {
	t.Parallel()
	docker, calls := fakeGCDocker(t)
	r := newTestRunner(t, newFakeServer(t))
	r.recordImages([]string{"old:1", "busy:1", "gone:1", "fresh:1", ""})
	unlock, err := lockImages(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := loadImages(r.cfg.path(imagesFileName))
	for _, img := range []string{"old:1", "busy:1", "gone:1"} {
		m[img] = time.Now().Add(-8 * 24 * time.Hour)
	}
	if err := m.save(r.cfg.path(imagesFileName)); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	// 一直在用也不延长：拉取超过保留期就删除，下次用到时重新拉取最新的
	r.recordImages([]string{"old:1"})

	GC(t.Context(), r.cfg, GCOptions{Docker: docker, RunnerID: 7}, slog.New(slog.DiscardHandler))
	wantCalls(t, dockerCalls(t, calls), []string{
		"ps -aq --filter label=dev.kiln.runner=7",
		"rm -f -v c1 c2",
		"network ls -q --filter label=dev.kiln.runner=7",
		"network rm n1",
		"volume ls -q --filter label=dev.kiln.runner=7",
		"image rm busy:1",
		"image rm gone:1",
		"image rm old:1",
	})
	got := dockerCalls(t, calls)
	if slices.Contains(got, "volume rm -f") || slices.Contains(got, "image rm fresh:1") {
		t.Errorf("没有残留的 volume、拉取不到保留期的镜像不应删除：%v", got)
	}
	unlock, err = lockImages(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	left := loadImages(r.cfg.path(imagesFileName))
	unlock()
	if len(left) != 2 || left["busy:1"].IsZero() || left["fresh:1"].IsZero() {
		t.Errorf("剩下的记录 = %v，应为 busy:1 和 fresh:1", left)
	}

	// 没有 RunnerID（手动执行 kiln-runner gc）时不清理容器
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	GC(t.Context(), r.cfg, GCOptions{Docker: docker}, slog.New(slog.DiscardHandler))
	if exists(calls) {
		for _, c := range dockerCalls(t, calls) {
			if strings.HasPrefix(c, "ps ") || strings.HasPrefix(c, "rm ") {
				t.Errorf("没有 RunnerID 时不应清理容器：%s", c)
			}
		}
	}
}

// 只在没有正在执行的 job、并且距上一次清理超过 runnerkit.GCInterval 时清理。
func TestMaybeGC(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, newFakeServer(t))
	old := r.cfg.path(gitDirName, "1.git") + "/"
	aged(t, old, 8*24*time.Hour)

	r.running[1] = &job{}
	r.maybeGC(t.Context())
	if !r.lastGC.IsZero() || !exists(filepath.Clean(old)) {
		t.Fatal("有正在执行的 job 时不应清理")
	}
	delete(r.running, 1)
	r.maybeGC(t.Context())
	if r.lastGC.IsZero() || exists(filepath.Clean(old)) {
		t.Fatal("空闲时应清理")
	}
	aged(t, old, 8*24*time.Hour)
	r.maybeGC(t.Context())
	if !exists(filepath.Clean(old)) {
		t.Error("距上一次清理不到 runnerkit.GCInterval 时不应再清理")
	}
}

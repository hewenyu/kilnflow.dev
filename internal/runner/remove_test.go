package runner

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 只删除数据目录中 Runner 管理的子目录里的对象；路径上有符号链接时拒绝，目标是符号链接时只删除链接。
func TestRemoverBoundaries(t *testing.T) {
	t.Parallel()
	cfg := &Config{Dir: t.TempDir()}
	rm := newRemover(cfg, "", 0)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "x")

	for _, p := range []string{
		cfg.Dir,
		filepath.Join(cfg.Dir, "keys"),
		filepath.Join(cfg.Dir, "keys", "runner.ed25519"),
		filepath.Join(cfg.Dir, "runner.json"),
		filepath.Join(cfg.Dir, toolcacheDirName),
		filepath.Join(cfg.Dir, toolcacheDirName, "..", "..", filepath.Base(outside)),
		outside,
	} {
		if _, err := rm.remove(t.Context(), p); err == nil {
			t.Errorf("remove(%s) 应拒绝", p)
		}
	}

	// toolcache/host -> outside：不能通过符号链接删除数据目录之外的文件
	if err := os.MkdirAll(cfg.path(toolcacheDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cfg.path(toolcacheDirName, "host")); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.remove(t.Context(), cfg.path(toolcacheDirName, "host", "keep")); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Errorf("路径上有符号链接时 remove = %v", err)
	}
	// 目标本身是符号链接：只删除链接
	if _, err := rm.remove(t.Context(), cfg.path(toolcacheDirName, "host")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cfg.path(toolcacheDirName, "host")); !os.IsNotExist(err) {
		t.Errorf("符号链接没有删除：%v", err)
	}
	if !exists(filepath.Join(outside, "keep")) {
		t.Error("符号链接指向的文件被删除")
	}

	// 普通目录（含只读子目录）和不存在的路径
	ws := cfg.path(workDirName, "9")
	mustWrite(t, filepath.Join(ws, "ro", "f"), "12345")
	if err := os.Chmod(filepath.Join(ws, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	n, err := rm.remove(t.Context(), ws)
	if err != nil || n != 5 || exists(ws) {
		t.Errorf("remove(work/9) = %d, %v；存在 = %v", n, err, exists(ws))
	}
	if n, err := rm.remove(t.Context(), ws); err != nil || n != 0 {
		t.Errorf("删除不存在的路径 = %d, %v", n, err)
	}
}

// fakeCleanupDocker 生成一个模拟清理容器的 docker：记录参数，并像容器中的 rm 一样删除挂载目录中的内容。
func fakeCleanupDocker(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "docker"), filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + log + `'
src=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-v" ]; then src="${2%%:*}"; fi
  shift
done
[ -n "$src" ] && rm -rf "$src"/* "$src"/.[!.]* 2>/dev/null
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// 当前用户删不掉时（容器中 root 写入的文件），用清理容器以 root 身份删除目录中的内容，再删除目录本身；
// 没有 docker 时返回权限错误。
func TestRemoverFallsBackToCleanupContainer(t *testing.T) {
	t.Parallel()
	docker, calls := fakeCleanupDocker(t)
	cfg := &Config{Dir: t.TempDir()}
	tc := cfg.path(toolcacheDirName, "oci-root", "1", "linux-arm64", "gocache")
	mustWrite(t, filepath.Join(tc, "f2", "x-d"), "data")
	mustWrite(t, filepath.Join(tc, ".hidden"), "h")

	// 第一次删除模拟"没有权限"（root 所有的目录），清理容器删掉内容后再删除目录本身
	denyOnce := func(rm *remover) {
		first := true
		rm.rmTree = func(p string) error {
			if first {
				first = false
				return &fs.PathError{Op: "unlinkat", Path: filepath.Join(p, "f2", "x-d"), Err: fs.ErrPermission}
			}
			return runnerkit.RemoveTree(p)
		}
	}

	noDocker := newRemover(cfg, "", 0)
	denyOnce(noDocker)
	if _, err := noDocker.remove(t.Context(), tc); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("没有 docker 时 remove = %v，应为权限错误", err)
	}

	rm := newRemover(cfg, docker, 7)
	denyOnce(rm)
	n, err := rm.remove(t.Context(), tc)
	if err != nil || exists(tc) {
		t.Fatalf("remove = %d, %v；存在 = %v", n, err, exists(tc))
	}
	if n != 5 || !rm.used {
		t.Errorf("删除的字节数 = %d，used = %v", n, rm.used)
	}
	got := dockerCalls(t, calls)
	want := "run --rm --network none --user 0:0 --label dev.kiln.runner=7 -v " + tc + ":/x --entrypoint sh " + DefaultCleanupImage +
		" -c rm -rf /x/* /x/.[!.]* /x/..?*"
	if len(got) != 1 || got[0] != want {
		t.Errorf("docker 调用 = %q\n期望 %q", got, want)
	}
}

// gcAsNobodyEnv 非空时 TestGCAsNobodyHelper 在这个数据目录中执行 GC（由 TestGCNonRootCannotHideRootOwned 以普通用户启动）。
const gcAsNobodyEnv = "KILN_TEST_GC_AS_NOBODY_DIR"

// nobodyUID 是测试中使用的普通用户（nobody）。
const nobodyUID = 65534

// TestGCAsNobodyHelper 不是独立的测试：由 TestGCNonRootCannotHideRootOwned 以普通用户的身份启动，
// 执行一次 GC，把结果（JSON）写到 stdout。
func TestGCAsNobodyHelper(t *testing.T) {
	dir := os.Getenv(gcAsNobodyEnv)
	if dir == "" {
		t.Skip("只由 TestGCNonRootCannotHideRootOwned 调用")
	}
	cfg := &Config{Dir: dir, Docker: os.Getenv("KILN_TEST_DOCKER"), DiskHighWatermark: 100}
	rep := GC(t.Context(), cfg, GCOptions{Docker: cfg.Docker}, slog.New(slog.DiscardHandler))
	if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
		t.Fatal(err)
	}
}

// 真实权限：以 root 运行时（例如 CI 的容器），在属于普通用户的数据目录中准备过期的 toolcache 和工作区，
// 其中含有 root 所有、模式 0755 的子目录；以普通用户执行 GC：自己的过期缓存删除，root 所有的删不掉时
// 记为失败（不是悄悄成功），gc.json 中也有记录。不是 root 时跳过（无法创建其他用户的文件）。
func TestGCNonRootCannotHideRootOwned(t *testing.T) {
	if os.Getuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("需要在 Linux 上以 root 运行")
	}
	base := t.TempDir()
	if err := os.Chmod(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// 测试程序复制到普通用户能执行的位置
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(base, "runner.test")
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "kiln")
	owned := filepath.Join(dir, toolcacheDirName, "host", "1", "linux-arm64", "owned")
	rootCache := filepath.Join(dir, toolcacheDirName, "oci-root", "1", "linux-arm64", "root-cache")
	rootWork := filepath.Join(dir, workDirName, "999999")
	mustWrite(t, filepath.Join(owned, "f"), "x")
	mustWrite(t, filepath.Join(rootCache, "root-dir", "file"), "x")
	mustWrite(t, filepath.Join(rootWork, "ws", "root-dir", "file"), "x")
	// 整个数据目录属于普通用户，只有 root-dir 及其中的文件属于 root（0755）
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(p, "root-dir") {
			return os.Chmod(p, map[bool]os.FileMode{true: 0o755, false: 0o644}[d.IsDir()])
		}
		return os.Chown(p, nobodyUID, nobodyUID)
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{owned, rootCache, rootWork} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.CommandContext(t.Context(), bin, "-test.run=^TestGCAsNobodyHelper$")
	cmd.Env = append(os.Environ(), gcAsNobodyEnv+"="+dir, "KILN_TEST_DOCKER=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobodyUID, Gid: nobodyUID}}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("以普通用户执行 GC：%v\n%s", err, out)
	}
	var rep GCReport
	line, _, _ := strings.Cut(string(out), "\n")
	if err := json.Unmarshal([]byte(line), &rep); err != nil {
		t.Fatalf("解析结果 %q：%v", out, err)
	}
	if exists(owned) {
		t.Error("普通用户自己的过期缓存应删除")
	}
	if !exists(rootCache) || !exists(rootWork) {
		t.Fatal("没有清理容器时，普通用户不可能删除 root 所有的目录")
	}
	if rep.Err() == nil || len(rep.Failures) != 2 {
		t.Fatalf("root 所有的对象删除失败应记为失败：%+v", rep)
	}
	for _, f := range rep.Failures {
		if !strings.Contains(f.Error, "permission denied") {
			t.Errorf("失败原因 = %q", f.Error)
		}
	}
	saved := LoadGCReport(&Config{Dir: dir})
	if saved == nil || len(saved.Failures) != 2 {
		t.Errorf("gc.json = %+v", saved)
	}
}

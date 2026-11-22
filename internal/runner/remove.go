package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 删除 Runner 数据目录中的对象（PLAN #8.3）。
//
// oci job 在容器中通常以 root 身份写入工作区和 toolcache，Linux 上以普通用户运行的 Runner 删不掉其中
// root 所有的目录（chmod 也不行：不是属主）。这时用一个一次性的清理容器（--user 0、--network none，
// 只挂载要删除的那个目录）删除其中的内容，再由 Runner 删除目录本身（目录是 Runner 创建的）。
// 只删除数据目录中 Runner 管理的子目录里的对象，路径上不能有符号链接；目标本身是符号链接时只删除链接。

// DefaultCleanupImage 是清理容器默认使用的镜像（固定 digest；在 config.toml 的 cleanup_image 中修改）。
const DefaultCleanupImage = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// cleanupContainerTimeout 是一次清理容器（含拉取镜像）的超时。
const cleanupContainerTimeout = 10 * time.Minute

// managedDirs 是数据目录中由 Runner 管理、可以删除其中对象的子目录。
var managedDirs = []string{workDirName, toolcacheDirName, casDirName, rtDirName, gitDirName, buildkitDirName, logsDirName}

// remover 删除数据目录中的对象。
type remover struct {
	dir    string // 数据目录
	docker string // docker 命令；为空时不用清理容器
	image  string // 清理容器的镜像
	label  string // 清理容器的标签（runnerLabel=<Runner ID>），没有注册时为空
	// used 记录用到的清理镜像（由调用方计入 images.json，按 CacheRetention 清理）。
	used bool
	// rmTree 删除目录树，默认 runnerkit.RemoveTree（测试中替换，模拟没有权限）。
	rmTree func(string) error
}

// newRemover 返回 cfg 的数据目录的 remover；docker 为空时只用当前用户的权限删除。
func newRemover(cfg *Config, docker string, runnerID int64) *remover {
	rm := &remover{dir: cfg.Dir, docker: docker, image: cfg.cleanupImage(), rmTree: runnerkit.RemoveTree}
	if runnerID != 0 {
		rm.label = runnerLabelValue(runnerID)
	}
	return rm
}

// checkManaged 检查 p 是数据目录中 Runner 管理的子目录里的对象，并且从数据目录到 p 的父目录都不是符号链接。
func (rm *remover) checkManaged(p string) error {
	rel, err := filepath.Rel(rm.dir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s 不在数据目录 %s 中", p, rm.dir)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 || !slices.Contains(managedDirs, parts[0]) {
		return fmt.Errorf("%s 不是 Runner 管理的对象", p)
	}
	cur := rm.dir
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s 是符号链接，不删除其中的对象", cur)
		}
	}
	return nil
}

// remove 删除 p（目录整个删除），返回删除前的大小。p 不存在时什么也不做。
func (rm *remover) remove(ctx context.Context, p string) (int64, error) {
	if err := rm.checkManaged(p); err != nil {
		return 0, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		// 文件和符号链接（只删除链接本身）
		return info.Size(), os.Remove(p)
	}
	size := dirSize(p)
	err = rm.rmTree(p)
	if err == nil {
		return size, nil
	}
	if !errors.Is(err, fs.ErrPermission) || rm.docker == "" {
		return 0, err
	}
	// 当前用户删不掉的内容（容器中 root 写入的）：用清理容器删除目录中的内容，再删除目录本身
	if cerr := rm.clearWithContainer(ctx, p); cerr != nil {
		return 0, fmt.Errorf("%w；用清理容器删除也失败：%w", err, cerr)
	}
	if err := runnerkit.RemoveTree(p); err != nil {
		return 0, err
	}
	return size, nil
}

// clearWithContainer 以 root 身份在一次性容器中删除目录 p 中的全部内容（不删除 p 本身，它是挂载点）。
func (rm *remover) clearWithContainer(ctx context.Context, p string) error {
	ctx, cancel := context.WithTimeout(ctx, cleanupContainerTimeout)
	defer cancel()
	const mnt = "/x"
	args := []string{"run", "--rm", "--network", dockerNetworkNone, "--user", "0:0"}
	if rm.label != "" {
		args = append(args, "--label", rm.label)
	}
	args = append(args, "-v", p+":"+mnt, "--entrypoint", "sh", rm.image, "-c", "rm -rf "+mnt+"/* "+mnt+"/.[!.]* "+mnt+"/..?*")
	rm.used = true
	out, err := exec.CommandContext(ctx, rm.docker, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run %s：%w：%s", rm.image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dirSize 返回目录树中普通文件的大小之和（尽力而为：跳过无法访问的条目）。
func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

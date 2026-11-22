package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// placeFetch 把上游输出放到工作区的 to 目录下，返回文件数。
//
// 写入都通过 os.Root 进行，而且根限定在输出目录 to 之内，不只是工作区：manifest 不可信，工作区中可能存在
// 符号链接，允许写入落出输出目录就等于允许覆盖同一工作区的其他源码（cas.FileEntry.Symlink 也要求链接目标
// 落在输出目录内）。to 为空时输出目录就是工作区本身。
func (j *job) placeFetch(ctx context.Context, f *runnerv1.FetchSpec, ws string) (int, error) {
	if err := j.ensureLocal(ctx, []string{f.ManifestDigest}); err != nil {
		return 0, err
	}
	store := j.r.casStore()
	var om cas.OutputManifest
	if err := cas.ReadJSON(ctx, store, f.ManifestDigest, &om); err != nil {
		return 0, err
	}
	var ds []string
	for _, fe := range om.Files {
		if fe.Symlink == "" {
			ds = append(ds, fe.Digest)
		}
	}
	if err := j.ensureLocal(ctx, ds); err != nil {
		return 0, err
	}
	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		return 0, err
	}
	defer wsRoot.Close()
	to := strings.TrimPrefix(path.Clean("/"+f.To), "/")
	toOS := filepath.FromSlash(to)
	// 输出目录 to 的 os.Root：内核层面的写入边界。to 为空时就是工作区。
	destRoot := wsRoot
	if toOS != "" {
		if err := wsRoot.MkdirAll(toOS, 0o755); err != nil {
			return 0, err
		}
		if destRoot, err = wsRoot.OpenRoot(toOS); err != nil {
			return 0, err
		}
		defer destRoot.Close()
	}
	destDir := filepath.Join(ws, toOS)
	made := make([]placedEntry, 0, len(om.Files))
	created := make([]string, 0, len(om.Files))
	for _, fe := range om.Files {
		name, err := placeEntry(destRoot, destDir, store, fe, &created)
		if err != nil {
			cleanupFetch(destRoot, made, created)
			return 0, err
		}
		made = append(made, placedEntry{name: name, path: fe.Path, target: fe.Symlink})
	}
	// 两阶段校验：全部条目落盘之后再统一复查符号链接。manifest 的顺序不可信，靠后的条目可能是靠前条目路径上的
	// 前缀（例如先写 a_escape -> d1/d2/sub/../src，再写 d1/d2/sub -> ../..），写入单条时看不到后面的链接。
	// 复查同样以输出目录 destDir 为根。
	for _, e := range made {
		if e.target == "" {
			continue
		}
		if !symlinkWithinRoot(destDir, filepath.Join(destDir, e.name), e.target) {
			cleanupFetch(destRoot, made, created)
			return 0, fmt.Errorf("fetch 的符号链接 %s 指向输出目录之外：%s", e.path, e.target)
		}
	}
	return len(om.Files), nil
}

// placedEntry 是 placeFetch 已写入的一个条目（target 非空表示符号链接），name 相对于输出目录。
type placedEntry struct {
	name   string
	path   string
	target string
}

// cleanupFetch 在放置失败时尽力清掉这次写入的条目，避免留下半成品：只按逆序删除这次写入的条目和
// mkdirAllTracked 记录的新建目录，不碰输出目录中原本就存在的内容（被覆盖的旧文件无法恢复：
// manifest 已不可信，宁可失败）。
func cleanupFetch(destRoot *os.Root, made []placedEntry, created []string) {
	for _, e := range slices.Backward(made) {
		_ = destRoot.Remove(e.name)
	}
	for _, dir := range slices.Backward(created) {
		_ = destRoot.Remove(dir)
	}
}

// mkdirAllTracked 在 root 中创建 dir（相对路径），把这次真正新建的目录按由浅到深记录到 created：
// 已有的目录保持原样，清理时也就不会误删工作区原有的空目录。调用方失败时只需逆序删除 created。
func mkdirAllTracked(root *os.Root, dir string, created *[]string) error {
	if dir == "." {
		return nil
	}
	var cur string
	for _, comp := range pathComponents(dir) {
		cur = filepath.Join(cur, comp)
		// Stat 会跟随输出目录内已有的目录符号链接（与后续写入的内核解析一致）；指向输出目录之外的
		// 链接由 os.Root 直接报错。
		fi, err := root.Stat(cur)
		switch {
		case err == nil:
			if !fi.IsDir() {
				return fmt.Errorf("fetch 的路径 %s 不是目录", cur)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(cur, 0o755); err != nil {
				if errors.Is(err, fs.ErrExist) {
					continue
				}
				return err
			}
			*created = append(*created, cur)
		default:
			return err
		}
	}
	return nil
}

// entryPath 校验 manifest 中的条目路径并转换成宿主机上的相对路径。
//
// 只接受规范的相对路径。os.Root 与 filepath.Join 对"中间符号链接之后的 .."的解析语义不同
// （d/link/../escape 词法上是 d/escape，内核却先解开 d/link 再走 ..），所以任何 "."、".."、空组件或
// 反斜杠都能让校验和真实写入指向不同位置，一律拒绝。
func entryPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, '\\') || path.Clean(p) != p || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", fmt.Errorf("fetch 的路径不是规范的相对路径：%q", p)
	}
	return filepath.FromSlash(p), nil
}

// placeEntry 把 manifest 中的一个条目写到 destRoot（输出目录的 os.Root）中的 fe.Path，返回相对于输出目录
// 的路径 name。destDir 是输出目录在宿主机上的路径，只用于复核符号链接目标；新建的目录记入 created。
func placeEntry(destRoot *os.Root, destDir string, store *cas.FSStore, fe cas.FileEntry, created *[]string) (string, error) {
	name, err := entryPath(fe.Path)
	if err != nil {
		return "", err
	}
	if err := mkdirAllTracked(destRoot, filepath.Dir(name), created); err != nil {
		return "", err
	}
	if fe.Symlink != "" {
		// 纵深防御：不信任 manifest 中的符号链接目标，按输出目录中真实存在的链接（包括链式链接）解析，
		// 确认它不会落到输出目录之外——否则后续条目就会顺着它覆盖工作区的其他文件。全部条目写完后
		// 还会再复查一次（placeFetch）。
		if !symlinkWithinRoot(destDir, filepath.Join(destDir, name), fe.Symlink) {
			return "", fmt.Errorf("fetch 的符号链接 %s 指向输出目录之外：%s", fe.Path, fe.Symlink)
		}
	}
	// 覆盖输出目录中已有的同名文件或符号链接
	if err := destRoot.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if fe.Symlink != "" {
		return name, destRoot.Symlink(fe.Symlink, name)
	}
	src, err := store.Path(fe.Digest)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if fe.Exec {
		mode = 0o755
	}
	return name, copyFileInto(destRoot, src, name, mode)
}

// copyFileInto 把本地文件 src 复制为 root 中的 name（name 必须不存在）。
func copyFileInto(root *os.Root, src, name string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() // 已经出错，以复制的错误为准
		return err
	}
	return out.Close()
}

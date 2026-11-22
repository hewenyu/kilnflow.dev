package runner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// maxSymlinkHops 是解析一个路径时允许跟随的符号链接数上限，用于打断循环链接。
const maxSymlinkHops = 40

// withinBase 判断 p（绝对路径）是否仍在 base（绝对路径）之内。
func withinBase(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && isWithin(rel)
}

// pathComponents 返回 p 的组件（处理所有平台的分隔符和重复分隔符）。
func pathComponents(p string) []string {
	var out []string
	for p != "" {
		var comp string
		if i := indexSeparator(p); i >= 0 {
			comp, p = p[:i], p[i+1:]
		} else {
			comp, p = p, ""
		}
		if comp != "" {
			out = append(out, comp)
		}
	}
	return out
}

// indexSeparator 返回 p 中第一个路径分隔符的位置。
func indexSeparator(p string) int {
	for i := range len(p) {
		if os.IsPathSeparator(p[i]) {
			return i
		}
	}
	return -1
}

// unsafeLinkTarget 判断符号链接目标是否以绝对路径的形式给出：Windows 卷标、前导分隔符（含 `\`）。
// 这类目标不受链接所在目录约束，一律拒绝。
func unsafeLinkTarget(p string) bool {
	return filepath.VolumeName(p) != "" || (p != "" && (p[0] == '/' || p[0] == '\\'))
}

// resolveSymlinkTarget 模拟内核解析 startDir 下的相对路径 target，返回解析结果以及解析过程中是否始终留在
// rootReal 之内。与 filepath.Clean 的纯字面解析不同，它会读取中间组件上真实存在的符号链接：例如
// `sub -> ../..` 配合 `sub/../../../outside/target.txt` 在字面上仍像在工作区内，实际却会逃出去。
//
// 目标可以悬空（不存在的部分按字面处理）。绝对目标、空目标、循环链接以及任何一步离开 rootReal 都返回 false。
func resolveSymlinkTarget(rootReal, startDir, target string) (string, bool) {
	if target == "" || unsafeLinkTarget(target) || !withinBase(rootReal, startDir) {
		return "", false
	}
	cur := startDir
	pending := pathComponents(target)
	hops := 0
	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		switch comp {
		case ".", "":
			continue
		case "..":
			cur = filepath.Dir(cur)
			if !withinBase(rootReal, cur) {
				return "", false
			}
			continue
		}
		next := filepath.Join(cur, comp)
		if !withinBase(rootReal, next) {
			return "", false
		}
		fi, err := os.Lstat(next)
		switch {
		case err == nil && fi.Mode()&fs.ModeSymlink != 0:
			hops++
			if hops > maxSymlinkHops {
				return "", false
			}
			t, err := os.Readlink(next)
			if err != nil || t == "" || unsafeLinkTarget(t) {
				return "", false
			}
			// 符号链接的目标相对于链接所在目录（cur），先展开它再继续处理剩余组件。
			pending = append(pathComponents(t), pending...)
		case err == nil:
			cur = next
		case errors.Is(err, fs.ErrNotExist):
			// 组件不存在时按字面进入（内核此时会 ENOENT），但不能就此停止检查：后续的 `..` 可能回到已存在的目录，
			// 之后的组件仍要按真实存在的符号链接解析（例如 missing/../d1/d2/sub/../../../outside/target.txt）。
			cur = next
		default:
			return "", false
		}
	}
	return cur, true
}

// symlinkWithinRoot 判断把链接 link（宿主机上的路径，其父目录已存在）指向 target 后，链接是否会解析到
// root（宿主机上的根目录）之外。工作区中已有的符号链接（包括链式链接）都会被解析。
func symlinkWithinRoot(root, link, target string) bool {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	dirReal, err := filepath.EvalSymlinks(filepath.Dir(link))
	if err != nil || !withinBase(rootReal, dirReal) {
		return false
	}
	_, ok := resolveSymlinkTarget(rootReal, dirReal, target)
	return ok
}

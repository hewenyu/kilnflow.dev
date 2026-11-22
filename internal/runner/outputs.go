package runner

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// 输出收集（PLAN #4.6、#10.2）。
//
// 输出路径的三种写法，manifest 中的路径都相对于"输出根目录"：
//   - 目录（以 / 结尾或实际是目录）：根目录就是它，包含其中全部文件；
//   - glob：根目录是 glob 中第一个通配符之前的目录，包含全部匹配的文件；
//   - 单个文件：根目录是它所在的目录，manifest 中只有它的文件名。

type collected struct {
	Manifest cas.OutputManifest
	Local    map[string]string // digest → 本地文件
}

func fileDigest(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return cas.HashReader(f)
}

// isWithin 判断 rel（filepath.Rel 的结果）是否没有跳出基准目录。
func isWithin(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// matchOutput 按输出路径列出文件（相对 ws 的斜杠路径），并返回输出根目录。
func matchOutput(ws, spec string) (root string, files []string, err error) {
	if strings.ContainsAny(spec, "*?[{") {
		base, _ := doublestar.SplitPattern(spec)
		if base == "." {
			base = ""
		}
		matches, err := doublestar.Glob(os.DirFS(ws), spec, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
		if err != nil {
			return "", nil, userErrorf("输出 glob %q 无效：%v", spec, err)
		}
		return base, matches, nil
	}
	full := filepath.Join(ws, filepath.FromSlash(spec))
	st, err := os.Lstat(full)
	if err != nil {
		return "", nil, userErrorf("输出 %q 不存在", spec)
	}
	if !st.IsDir() {
		root = path.Dir(spec)
		if root == "." {
			root = ""
		}
		return root, []string{spec}, nil
	}
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(ws, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return spec, files, err
}

// collectOutput 按输出路径收集文件。ws 是工作区根目录（宿主机上的路径）。
func collectOutput(ws, spec string) (*collected, error) {
	spec = strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(spec)), "/")
	if spec == "" || spec == "." {
		return nil, userErrorf("输出路径为空")
	}
	root, files, err := matchOutput(ws, spec)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, userErrorf("输出 %q 没有匹配任何文件", spec)
	}
	slices.Sort(files)
	out := &collected{Manifest: cas.OutputManifest{Schema: cas.OutputSchema, Files: []cas.FileEntry{}}, Local: map[string]string{}}
	wsReal, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return nil, err
	}
	rootAbs := filepath.Join(ws, filepath.FromSlash(root))
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, err
	}
	sc := &outputScope{spec: spec, ws: ws, wsReal: wsReal, root: root, rootAbs: rootAbs, rootReal: rootReal}
	for _, rel := range files {
		fe, local, err := sc.collectFile(rel)
		if err != nil {
			return nil, err
		}
		if fe == nil {
			continue // 既不是普通文件也不是符号链接
		}
		if local != "" {
			out.Local[fe.Digest] = local
		}
		out.Manifest.Files = append(out.Manifest.Files, *fe)
	}
	return out, nil
}

// outputScope 是收集一个输出时各文件共用的路径。
type outputScope struct {
	spec     string // 输出路径（用于错误信息）
	ws       string // 工作区（宿主机上的路径）
	wsReal   string // ws 解析符号链接后的路径
	root     string // 输出根目录（相对 ws 的斜杠路径）
	rootAbs  string // 输出根目录在宿主机上的路径
	rootReal string // rootAbs 解析符号链接后的路径
}

// collectFile 生成一个文件（相对 ws 的斜杠路径）的 manifest 条目；普通文件同时返回本地路径。
func (sc *outputScope) collectFile(rel string) (*cas.FileEntry, string, error) {
	p := filepath.Join(sc.ws, filepath.FromSlash(rel))
	inRoot := strings.TrimPrefix(strings.TrimPrefix(rel, sc.root), "/")
	// 路径中间的目录也可能是符号链接，解析后必须仍在工作区内
	dirReal, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return nil, "", userErrorf("输出 %q 中的 %s 无法解析：%v", sc.spec, rel, err)
	}
	if r, err := filepath.Rel(sc.wsReal, dirReal); err != nil || !isWithin(r) {
		return nil, "", userErrorf("输出 %q 中的 %s 经由符号链接指向工作区之外", sc.spec, rel)
	}
	st, err := os.Lstat(p)
	if err != nil {
		return nil, "", err
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return nil, "", err
		}
		// 逐组件解析（包括链式符号链接）：字面解析会放过 sub -> ../.. 这类链接拼出的逃逸路径。
		if _, ok := resolveSymlinkTarget(sc.rootReal, dirReal, target); !ok {
			return nil, "", userErrorf("输出 %q 中的符号链接 %s 指向输出目录之外（%s）", sc.spec, rel, target)
		}
		return &cas.FileEntry{Path: inRoot, Symlink: target}, "", nil
	case st.Mode().IsRegular():
		d, n, err := fileDigest(p)
		if err != nil {
			return nil, "", err
		}
		return &cas.FileEntry{Path: inRoot, Digest: d, Size: n, Exec: st.Mode().Perm()&0o111 != 0}, p, nil
	}
	return nil, "", nil
}

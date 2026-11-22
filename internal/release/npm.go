package release

import (
	"archive/tar"
	"bufio"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// npm 包（release.npm，PLAN #27.3）：把上游输出确定性地打成 npm tarball，或读取构建 job 用 pnpm pack 产出的 tarball。

// npmPrefix 是 npm tarball 中所有条目的顶层目录。
const npmPrefix = "package/"

// NpmPackRequest 描述要打成 npm 包的文件。
type NpmPackRequest struct {
	Name    string          // 不含扩展名的文件名
	Files   []cas.FileEntry // 放进包里的文件（不含 package.json），路径相对于包的根目录
	Package []byte          // package.json 的内容
	WorkDir string
	ModTime time.Time
}

// PackNpm 生成 npm tarball：条目加 package/ 前缀，package.json 在最前，其他文件按路径排序，mtime 固定，保留可执行位。
// 返回的 Asset 中 Files、Names 用于源仓库名检查（含 package.json）。
func PackNpm(ctx context.Context, store cas.Store, req NpmPackRequest) (*Asset, error) {
	if err := CheckFileName(req.Name); err != nil {
		return nil, err
	}
	files := make([]cas.FileEntry, 0, len(req.Files))
	names := []string{npmPrefix + "package.json"}
	for _, fe := range req.Files {
		if fe.Path == "package.json" {
			return nil, errors.New("package.json 由 Kiln 生成，不能作为普通文件放进包里")
		}
		fe.Path = npmPrefix + fe.Path
		if fe.Symlink != "" {
			// 符号链接目标相对于链接所在目录，加前缀不影响
			names = append(names, fe.Symlink)
		}
		names = append(names, fe.Path)
		files = append(files, fe)
	}
	slices.SortFunc(files, func(a, b cas.FileEntry) int { return cmp.Compare(a.Path, b.Path) })
	if err := os.MkdirAll(filepath.Join(req.WorkDir, "_src"), 0o755); err != nil {
		return nil, err
	}
	srcDir, err := os.MkdirTemp(filepath.Join(req.WorkDir, "_src"), "npm-")
	if err != nil {
		return nil, err
	}
	pj := filepath.Join(srcDir, "package.json")
	if err := os.WriteFile(pj, req.Package, 0o644); err != nil {
		return nil, err
	}
	p := &packer{store: store, srcDir: srcDir, mtime: req.ModTime, origs: []string{pj}}
	dst := filepath.Join(req.WorkDir, req.Name+".tgz")
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	bw := bufio.NewWriter(io.MultiWriter(f, h))
	err = p.writeNpm(ctx, bw, req.Package, files)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return nil, err
	}
	return &Asset{Name: filepath.Base(dst), Path: dst, Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil)), Files: p.origs, Names: names}, nil
}

// writeNpm 写入 gzip 压缩的 tar：先 package/package.json，再其他文件（父目录条目按需补齐）。
func (p *packer) writeNpm(ctx context.Context, w io.Writer, pkg []byte, files []cas.FileEntry) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{"package": true}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: npmPrefix, Mode: 0o755, ModTime: p.mtime, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: npmPrefix + "package.json", Mode: 0o644, Size: int64(len(pkg)), ModTime: p.mtime, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(pkg); err != nil {
		return err
	}
	for _, fe := range files {
		if err := p.writeTarEntry(ctx, tw, fe, dirs); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// NpmTarball 是读取后的 npm tarball（构建 job 用 pnpm pack 等产出的 .tgz）。
type NpmTarball struct {
	Asset
	// Manifest 是包内的 package.json。
	Manifest map[string]any
}

// npm tarball 的读取上限：单个文件和解压后的总大小。
const (
	maxNpmFile  = 1 << 30
	maxNpmTotal = 4 << 30
	// maxNpmTrailer 是 tar 结束标记之后允许的填充（tar 按 10 KiB 的记录对齐，留足余量）。
	maxNpmTrailer = 1 << 20
	// maxNpmManifest 是包内 package.json 的大小上限。
	maxNpmManifest = 1 << 20
	// maxNpmEntries 是 tar 条目数的上限：条目极多的小文件同样能让解包拖垮内存和磁盘。
	maxNpmEntries = 100_000
)

// npmFileEntry 判断 tar 条目是否带着文件内容：普通文件，以及 npm（node-tar）同样解成普通文件的连续文件（类型 7）。
// 源仓库名检查必须看到包里所有会被解出的内容，只看 TypeReg 会漏掉类型 7 的条目。
func npmFileEntry(typ byte) bool {
	return typ == tar.TypeReg || typ == tar.TypeCont
}

// validNpmEntryName 判断 tar 条目名是包内的相对路径：不是绝对路径，也不能通过 .. 跳出顶层目录。
// "."（顶层目录本身）是允许的。
func validNpmEntryName(name string) bool {
	if name == "" || filepath.IsAbs(filepath.FromSlash(name)) {
		return false
	}
	clean := path.Clean(name)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// ReadNpmTarball 读取 tgz 形式的 npm 包：解出普通文件（只用于源仓库名检查，写到 work 下的独立目录，不按包内路径写，
// 不会越出目录），取出顶层目录中的 package.json。asset 记录原文件的路径、大小和 sha256。
func ReadNpmTarball(tgz, work string) (*NpmTarball, error) {
	if err := os.MkdirAll(filepath.Join(work, "_src"), 0o755); err != nil {
		return nil, err
	}
	srcDir, err := os.MkdirTemp(filepath.Join(work, "_src"), "npm-")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(tgz)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	out := &NpmTarball{}
	out.Name, out.Path, out.Size, out.SHA256 = filepath.Base(tgz), tgz, n, hex.EncodeToString(h.Sum(nil))
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("不是 gzip 压缩的 npm 包：%w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	var entries int
	var pkgJSON []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 npm 包：%w", err)
		}
		entries++
		if entries > maxNpmEntries {
			return nil, fmt.Errorf("npm 包中的条目超过上限 %d", maxNpmEntries)
		}
		// 包内路径必须相对、不能跳出顶层目录："/package.json"、"../package.json" 会被下面的
		// package.json 识别逻辑误当成顶层包清单。
		if !validNpmEntryName(hdr.Name) {
			return nil, fmt.Errorf("npm 包中的路径 %q 无效：必须是包内的相对路径", hdr.Name)
		}
		out.Names = append(out.Names, hdr.Name)
		if hdr.Linkname != "" {
			out.Names = append(out.Names, hdr.Linkname)
		}
		if !npmFileEntry(hdr.Typeflag) {
			continue
		}
		if hdr.Size > maxNpmFile || total+hdr.Size > maxNpmTotal {
			return nil, fmt.Errorf("npm 包中的文件太大：%s", hdr.Name)
		}
		total += hdr.Size
		dst := filepath.Join(srcDir, strconv.Itoa(len(out.Files)))
		b, err := extractTo(dst, tr, hdr.Size)
		if err != nil {
			return nil, fmt.Errorf("解出 %s：%w", hdr.Name, err)
		}
		out.Files = append(out.Files, dst)
		// npm 的 tarball 只有一个顶层目录（通常是 package/），package.json 在它下面；有多个时与解包一样以最后一个为准
		if dir, file := path.Split(strings.TrimPrefix(path.Clean(hdr.Name), "./")); file == "package.json" && strings.Count(dir, "/") == 1 {
			if b == nil {
				return nil, fmt.Errorf("npm 包中的 %s 太大（超过 %d 字节）", hdr.Name, maxNpmManifest)
			}
			pkgJSON = b
		}
	}
	// tar 的结束标记之后只剩填充和 gzip 尾部：读到底，才会校验 gzip 的 CRC 和长度（损坏、截断的包在这里被拒绝）
	if rest, err := io.Copy(io.Discard, io.LimitReader(gz, maxNpmTrailer+1)); err != nil {
		return nil, fmt.Errorf("npm 包已损坏：%w", err)
	} else if rest > maxNpmTrailer {
		return nil, errors.New("npm 包在 tar 结束后还有多余的数据")
	}
	if pkgJSON == nil {
		return nil, errors.New("npm 包中没有 package.json（顶层目录下）")
	}
	if err := json.Unmarshal(pkgJSON, &out.Manifest); err != nil {
		return nil, fmt.Errorf("npm 包中的 package.json 不是合法的 JSON：%w", err)
	}
	if out.Manifest == nil {
		return nil, errors.New("npm 包中的 package.json 不是 JSON 对象")
	}
	return out, nil
}

// extractTo 把 r 中 size 字节写到 dst；文件不超过 maxNpmManifest 时同时返回内容（非 nil），供调用方解析 package.json。
func extractTo(dst string, r io.Reader, size int64) ([]byte, error) {
	w, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	var keep []byte
	if size <= maxNpmManifest {
		b, err := io.ReadAll(io.LimitReader(r, size))
		if err == nil {
			keep = b
			_, err = w.Write(b)
		}
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		return keep, err
	}
	_, err = io.CopyN(w, r, size)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return nil, err
}

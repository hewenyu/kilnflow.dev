// Package release 实现发布 job 中与 Server 状态无关的部分（PLAN #13.4、#13.6）：
// 把上游输出确定性地打包成 asset、生成校验和文件、源仓库名检查和从 CHANGELOG 中取出说明。
package release

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// 打包方式（release.assets[].archive）。
const (
	ArchiveTarGz = "tar.gz" // 默认
	ArchiveZip   = "zip"
	ArchiveNone  = "none" // 不打包，输出必须恰好是一个文件
)

// Asset 是一个准备上传的 Release asset。
type Asset struct {
	Name   string
	Path   string
	Size   int64
	SHA256 string
	// Files 是打包前的原始文件，Names 是包内的路径和符号链接目标（都用于源仓库名检查）。
	Files []string
	Names []string
}

// PackRequest 描述要打包的一个输出。
type PackRequest struct {
	Archive string // ArchiveTarGz（空值同此）、ArchiveZip 或 ArchiveNone
	Name    string // 不含扩展名的 asset 名
	Files   []cas.FileEntry
	// WorkDir 是工作目录：asset 写到这里，原始文件解压到它的 _src 子目录。
	WorkDir string
	// ModTime 是包内所有条目的修改时间（保证打包结果确定）。
	ModTime time.Time
}

// Pack 按 archive 方式把一个输出打成 asset 文件。
func Pack(ctx context.Context, store cas.Store, req PackRequest) (*Asset, error) {
	files := slices.Clone(req.Files)
	slices.SortFunc(files, func(a, b cas.FileEntry) int { return cmp.Compare(a.Path, b.Path) })
	fileName, err := assetFileName(req.Archive, req.Name, files)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, fe := range files {
		names = append(names, fe.Path)
		if fe.Symlink != "" {
			names = append(names, fe.Symlink)
		}
	}
	// 每个 asset 用独立目录存放原始文件，避免多个 asset 之间互相覆盖
	if err := os.MkdirAll(filepath.Join(req.WorkDir, "_src"), 0o755); err != nil {
		return nil, err
	}
	srcDir, err := os.MkdirTemp(filepath.Join(req.WorkDir, "_src"), "asset-")
	if err != nil {
		return nil, err
	}
	p := &packer{store: store, srcDir: srcDir, mtime: req.ModTime}
	dst := filepath.Join(req.WorkDir, fileName)
	sum, err := p.writeFile(ctx, dst, req.Archive, files)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return nil, err
	}
	return &Asset{Name: fileName, Path: dst, Size: st.Size(), SHA256: sum, Files: p.origs, Names: names}, nil
}

// CheckFileName 检查 asset 名可以直接用作工作目录中的文件名：不为空，不是 . 或 ..，不含路径分隔符和 0 字节。
// 名字来自 workflow（含发布时才求值的表达式），含路径分隔符时 filepath.Join 会把文件写到工作目录之外。
func CheckFileName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("asset 名 %q 无效：不能为空，不能含路径分隔符", name)
	}
	return nil
}

// assetFileName 返回带扩展名的 asset 文件名。
func assetFileName(archive, name string, files []cas.FileEntry) (string, error) {
	if err := CheckFileName(name); err != nil {
		return "", err
	}
	switch archive {
	case ArchiveZip:
		return name + ".zip", nil
	case ArchiveNone:
		if len(files) != 1 || files[0].Symlink != "" {
			return "", fmt.Errorf("archive: none 要求输出恰好是一个文件，实际有 %d 个", len(files))
		}
		return name + path.Ext(files[0].Path), nil
	}
	return name + ".tar.gz", nil
}

// packer 从 CAS 取出文件写入包中，并保留一份原始文件供源仓库名检查。
type packer struct {
	store  cas.Store
	srcDir string
	mtime  time.Time
	origs  []string
}

// writeFile 创建 dst 并写入包内容，返回 sha256（hex）。
func (p *packer) writeFile(ctx context.Context, dst, archive string, files []cas.FileEntry) (string, error) {
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	bw := bufio.NewWriter(io.MultiWriter(f, h))
	switch archive {
	case ArchiveNone:
		err = p.copyTo(ctx, bw, files[0])
	case ArchiveZip:
		err = p.writeZip(ctx, bw, files)
	default:
		err = p.writeTarGz(ctx, bw, files)
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (p *packer) writeZip(ctx context.Context, w io.Writer, files []cas.FileEntry) error {
	zw := zip.NewWriter(w)
	for _, fe := range files {
		hdr := &zip.FileHeader{Name: fe.Path, Method: zip.Deflate, Modified: p.mtime}
		mode := os.FileMode(0o644)
		if fe.Exec {
			mode = 0o755
		}
		if fe.Symlink != "" {
			mode = os.ModeSymlink | 0o777
		}
		hdr.SetMode(mode)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if fe.Symlink != "" {
			_, err = io.WriteString(fw, fe.Symlink)
		} else {
			err = p.copyTo(ctx, fw, fe)
		}
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

func (p *packer) writeTarGz(ctx context.Context, w io.Writer, files []cas.FileEntry) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
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

// writeTarEntry 写入一个文件（或符号链接），并在它之前补齐尚未写入的父目录条目。
func (p *packer) writeTarEntry(ctx context.Context, tw *tar.Writer, fe cas.FileEntry, dirs map[string]bool) error {
	var parents []string
	for d := path.Dir(fe.Path); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
		dirs[d] = true
		parents = append(parents, d)
	}
	slices.Reverse(parents)
	for _, d := range parents {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: p.mtime, Format: tar.FormatPAX}); err != nil {
			return err
		}
	}
	hdr := &tar.Header{Name: fe.Path, Mode: 0o644, ModTime: p.mtime, Format: tar.FormatPAX}
	if fe.Exec {
		hdr.Mode = 0o755
	}
	if fe.Symlink != "" {
		hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, fe.Symlink, 0o777
		return tw.WriteHeader(hdr)
	}
	hdr.Typeflag, hdr.Size = tar.TypeReg, fe.Size
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	return p.copyTo(ctx, tw, fe)
}

// copyTo 把文件从 CAS 解压到原始文件目录，再写入 w。
func (p *packer) copyTo(ctx context.Context, w io.Writer, fe cas.FileEntry) error {
	orig, err := p.extract(ctx, fe)
	if err != nil {
		return err
	}
	r, err := os.Open(orig)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

// extract 把 CAS 中的文件保存为原始文件目录中的一个编号文件。
func (p *packer) extract(ctx context.Context, fe cas.FileEntry) (string, error) {
	dst := filepath.Join(p.srcDir, strconv.Itoa(len(p.origs)))
	rc, _, err := p.store.Get(ctx, fe.Digest)
	if err != nil {
		return "", fmt.Errorf("读取 %s：%w", fe.Path, err)
	}
	defer rc.Close()
	w, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(w, rc)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("解压 %s：%w", fe.Path, err)
	}
	p.origs = append(p.origs, dst)
	return dst, nil
}

// WriteChecksums 在 dir 中生成校验和文件（sha256sum 格式），返回对应的 asset。name 不能含路径分隔符。
func WriteChecksums(dir, name string, assets []Asset) (*Asset, error) {
	if err := CheckFileName(name); err != nil {
		return nil, fmt.Errorf("校验和文件名：%w", err)
	}
	var b []byte
	for _, a := range assets {
		b = fmt.Appendf(b, "%s  %s\n", a.SHA256, a.Name)
	}
	p := filepath.Join(dir, "_"+name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &Asset{Name: name, Path: p, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

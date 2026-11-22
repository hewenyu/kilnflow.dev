package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxExtractSize 是一个归档解包后的总大小上限，防止解压炸弹。
	maxExtractSize int64 = 8 << 30
	// maxLinkTarget 是 zip 中符号链接目标的长度上限。
	maxLinkTarget = 4096
)

// 归档格式（artifact.Format）。
const (
	formatTarGz = "tar.gz"
	formatTgz   = "tgz"
	formatZip   = "zip"
)

var errTooLarge = fmt.Errorf("解包后的内容超过 %d GiB", maxExtractSize>>30)

// extract 解包 tar.gz 或 zip 到 dest，去掉 strip 层顶层目录。
//
// 所有写入都通过 os.Root 进行：归档中的路径（包括经由已解出的符号链接）不能落到 dest 之外；
// 符号链接的目标必须是 dest 内的相对路径，硬链接只能指向 dest 中已解出的普通文件。
func extract(file, format string, strip int, dest string) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	x := &extractor{root: root, strip: strip, remain: maxExtractSize}
	switch format {
	case formatTarGz, formatTgz:
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		return x.tar(tar.NewReader(gz))
	case formatZip:
		zr, err := zip.OpenReader(file)
		if err != nil {
			return err
		}
		defer zr.Close()
		return x.zip(&zr.Reader)
	}
	return fmt.Errorf("不支持的归档格式 %s", format)
}

// extractor 把归档条目写入 root。
type extractor struct {
	root   *os.Root
	strip  int
	remain int64 // 还允许写入的字节数
}

func (x *extractor) tar(tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok, err := x.name(hdr.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// tar.Reader 已经把过时的 TypeRegA 规范化为 TypeReg
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = x.root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = x.file(name, hdr.Mode&0o111 != 0, tr)
		case tar.TypeSymlink:
			err = x.symlink(name, hdr.Linkname)
		case tar.TypeLink:
			err = x.link(name, hdr.Linkname)
		}
		if err != nil {
			return err
		}
	}
}

func (x *extractor) zip(zr *zip.Reader) error {
	for _, zf := range zr.File {
		name, ok, err := x.name(zf.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			err = x.root.MkdirAll(name, 0o755)
		case mode&fs.ModeSymlink != 0:
			var target string
			if target, err = readZipLink(zf); err == nil {
				err = x.symlink(name, target)
			}
		case mode.IsRegular():
			err = x.zipFile(name, zf)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) zipFile(name string, zf *zip.File) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return x.file(name, zf.Mode()&0o111 != 0, rc)
}

func readZipLink(zf *zip.File) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLinkTarget+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxLinkTarget {
		return "", fmt.Errorf("归档中的符号链接 %s 目标过长", zf.Name)
	}
	return string(b), nil
}

// name 去掉前 strip 层目录，返回 root 中的相对路径；ok 为 false 表示跳过该条目。
func (x *extractor) name(raw string) (string, bool, error) {
	name, ok := stripPath(raw, x.strip)
	if !ok {
		return "", false, nil
	}
	p := filepath.FromSlash(name)
	if !filepath.IsLocal(p) {
		return "", false, fmt.Errorf("归档中的路径越界：%s", raw)
	}
	return p, true, nil
}

// prepare 创建 name 的父目录，并删除已经存在的同名条目（后出现的条目覆盖先出现的）。
func (x *extractor) prepare(name string) error {
	if dir := filepath.Dir(name); dir != "." {
		if err := x.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := x.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (x *extractor) file(name string, executable bool, r io.Reader) error {
	if err := x.prepare(name); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	out, err := x.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	// 多读一个字节，用来判断是否超过上限
	n, err := io.CopyN(out, r, x.remain+1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	x.remain -= n
	if err == nil && x.remain < 0 {
		err = errTooLarge
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func (x *extractor) symlink(name, target string) error {
	if !linkInside(name, target) {
		return fmt.Errorf("归档中的符号链接 %s 指向解包目录之外：%s", name, target)
	}
	if err := x.prepare(name); err != nil {
		return err
	}
	return x.root.Symlink(target, name)
}

func (x *extractor) link(name, rawTarget string) error {
	target, ok, err := x.name(rawTarget)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("无效的硬链接 %s → %s", name, rawTarget)
	}
	st, err := x.root.Lstat(target)
	if err != nil {
		return fmt.Errorf("硬链接 %s：%w", name, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("硬链接 %s 只能指向普通文件：%s", name, rawTarget)
	}
	if err := x.prepare(name); err != nil {
		return err
	}
	return x.root.Link(target, name)
}

// linkInside 判断符号链接 name → target 按字面解析后是否仍在根目录之内。
func linkInside(name, target string) bool {
	if target == "" || filepath.IsAbs(target) {
		return false
	}
	return filepath.IsLocal(filepath.Join(filepath.Dir(name), target))
}

func stripPath(name string, strip int) (string, bool) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= strip {
		return "", false
	}
	return strings.Join(parts[strip:], "/"), true
}

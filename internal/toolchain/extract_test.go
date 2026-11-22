package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry 描述测试归档中的一个条目。
type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func writeTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestExtractTarGz(t *testing.T) {
	t.Parallel()
	archive := writeTarGz(t, []tarEntry{
		{name: "go/", typ: tar.TypeDir, mode: 0o755},
		{name: "go/bin/go", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		{name: "go/lib/x.txt", typ: tar.TypeReg, body: "x"},
		{name: "go/bin/alias", typ: tar.TypeSymlink, link: "go"},
		{name: "go/lib/y.txt", typ: tar.TypeLink, link: "go/lib/x.txt"},
		{name: "go/lib/x.txt", typ: tar.TypeReg, body: "x2"}, // 后出现的条目覆盖先出现的
	})
	dest := t.TempDir()
	if err := extract(archive, "tar.gz", 1, dest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "bin", "go"))
	if err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/go：%v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "bin", "alias")); err != nil || target != "go" {
		t.Fatalf("bin/alias → %q, %v", target, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "lib", "y.txt")); err != nil || string(b) != "x" {
		t.Fatalf("lib/y.txt = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "lib", "x.txt")); err != nil || string(b) != "x2" {
		t.Fatalf("lib/x.txt = %q, %v", b, err)
	}
}

// 恶意归档：不能在解包目录之外创建或修改任何文件。
func TestExtractRejectsEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries func(outside string) []tarEntry
	}{
		{"../ 路径", func(string) []tarEntry {
			return []tarEntry{{name: "pkg/../../evil", typ: tar.TypeReg, body: "x"}}
		}},
		{"绝对路径的符号链接后跟写入", func(outside string) []tarEntry {
			return []tarEntry{
				{name: "pkg/a", typ: tar.TypeSymlink, link: outside},
				{name: "pkg/a/evil", typ: tar.TypeReg, body: "x"},
			}
		}},
		{"符号链接 a -> /etc 后跟 a/x", func(string) []tarEntry {
			return []tarEntry{
				{name: "pkg/a", typ: tar.TypeSymlink, link: "/etc"},
				{name: "pkg/a/x", typ: tar.TypeReg, body: "x"},
			}
		}},
		{"相对路径的符号链接指向外面", func(string) []tarEntry {
			return []tarEntry{
				{name: "pkg/a", typ: tar.TypeSymlink, link: "../../.."},
				{name: "pkg/a/evil", typ: tar.TypeReg, body: "x"},
			}
		}},
		{"硬链接指向外面", func(outside string) []tarEntry {
			return []tarEntry{{name: "pkg/h", typ: tar.TypeLink, link: "pkg/../../" + filepath.Join(outside, "victim")}}
		}},
		{"硬链接经由符号链接", func(string) []tarEntry {
			return []tarEntry{
				{name: "pkg/s", typ: tar.TypeSymlink, link: "/etc/hosts"},
				{name: "pkg/h", typ: tar.TypeLink, link: "pkg/s"},
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			outside := t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("safe"), 0o644); err != nil {
				t.Fatal(err)
			}
			archive := writeTarGz(t, c.entries(outside))
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := extract(archive, "tar.gz", 1, dest); err == nil {
				t.Fatal("期望解包失败")
			}
			for _, p := range []string{filepath.Join(outside, "evil"), filepath.Join(parent, "evil"), "/etc/x"} {
				if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("在解包目录之外创建了 %s（%v）", p, err)
				}
			}
			if b, err := os.ReadFile(victim); err != nil || string(b) != "safe" {
				t.Errorf("外部文件被修改：%q, %v", b, err)
			}
		})
	}
}

func TestExtractZip(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	add := func(name string, mode fs.FileMode, body string) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("bun-darwin-aarch64/bun", 0o755, "bin")
	add("bun-darwin-aarch64/bunx", fs.ModeSymlink|0o777, "bun")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := extract(p, "zip", 1, dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "bunx")); err != nil || string(b) != "bin" {
		t.Fatalf("bunx = %q, %v", b, err)
	}

	// zip 中指向外面的符号链接
	p2 := filepath.Join(t.TempDir(), "b.zip")
	f2, err := os.Create(p2)
	if err != nil {
		t.Fatal(err)
	}
	zw = zip.NewWriter(f2)
	add("x/evil", fs.ModeSymlink|0o777, "/etc")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extract(p2, "zip", 1, t.TempDir()); err == nil {
		t.Fatal("期望拒绝指向外面的符号链接")
	}
}

// 即使解包目录中已经存在指向外面的符号链接（绕过了 linkInside 的检查），os.Root 也不允许经由它写出去。
func TestExtractorRootConfinement(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "a")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	x := &extractor{root: root, remain: maxExtractSize}
	if err := x.file(filepath.Join("a", "evil"), false, strings.NewReader("x")); err == nil {
		t.Fatal("期望写入失败")
	}
	if err := x.root.MkdirAll(filepath.Join("a", "sub"), 0o755); err == nil {
		t.Fatal("期望创建目录失败")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("外部目录被写入：%v %v", entries, err)
	}
}

func TestExtractorSizeLimit(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	x := &extractor{root: root, remain: 4}
	if err := x.file("a", false, strings.NewReader("1234")); err != nil {
		t.Fatal(err)
	}
	if err := x.file("b", false, strings.NewReader("5")); !errors.Is(err, errTooLarge) {
		t.Fatalf("期望 errTooLarge，实际 %v", err)
	}
}

// 归档本身有问题时返回错误：不支持的格式、损坏的 gzip / zip、zip 中的路径越界、过长的链接目标、无效的硬链接。
func TestExtractInvalidArchives(t *testing.T) {
	t.Parallel()
	goodTar := writeTarGz(t, []tarEntry{{name: "x/a", typ: tar.TypeReg, body: "a"}})
	cases := []struct {
		name    string
		archive func(t *testing.T) string
		format  string
		want    string
	}{
		{name: "不支持的格式", archive: func(*testing.T) string { return goodTar }, format: "tar.xz", want: "不支持的归档格式"},
		{name: "文件不存在", archive: func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "missing") }, format: formatTgz},
		{name: "不是 gzip", archive: func(t *testing.T) string { t.Helper(); return writeTemp(t, "a.tgz", []byte("plain")) }, format: formatTgz},
		{name: "不是 zip", archive: func(t *testing.T) string { t.Helper(); return writeTemp(t, "a.zip", []byte("plain")) }, format: formatZip},
		{name: "zip 中的 ../ 路径", archive: func(t *testing.T) string {
			t.Helper()
			return writeTemp(t, "a.zip", zipBytes(t, []zipEntry{{name: "x/../../evil", mode: 0o644, body: "x"}}))
		}, format: formatZip, want: "路径越界"},
		{name: "zip 中的链接目标过长", archive: func(t *testing.T) string {
			t.Helper()
			return writeTemp(t, "a.zip", zipBytes(t, []zipEntry{{name: "x/l", mode: fs.ModeSymlink | 0o777, body: strings.Repeat("a", maxLinkTarget+1)}}))
		}, format: formatZip, want: "目标过长"},
		{name: "硬链接指向不存在的文件", archive: func(t *testing.T) string {
			t.Helper()
			return writeTarGz(t, []tarEntry{{name: "x/h", typ: tar.TypeLink, link: "x/missing"}})
		}, format: formatTarGz, want: "硬链接 h"},
		{name: "硬链接指向目录", archive: func(t *testing.T) string {
			t.Helper()
			return writeTarGz(t, []tarEntry{{name: "x/d/", typ: tar.TypeDir}, {name: "x/h", typ: tar.TypeLink, link: "x/d"}})
		}, format: formatTarGz, want: "只能指向普通文件"},
		{name: "硬链接目标被 strip 掉", archive: func(t *testing.T) string {
			t.Helper()
			return writeTarGz(t, []tarEntry{{name: "x/h", typ: tar.TypeLink, link: "x"}})
		}, format: formatTarGz, want: "无效的硬链接"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := extract(c.archive(t), c.format, 1, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("extract = %v，期望包含 %q", err, c.want)
			}
		})
	}
}

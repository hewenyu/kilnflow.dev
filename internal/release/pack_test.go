package release

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// sampleFiles 是一个典型的输出：可执行文件、普通文件、多层目录和符号链接，顺序故意打乱。
var sampleFiles = []srcFile{
	{path: "kiln/bin/kiln", content: "#!binary", exec: true},
	{path: "README.md", content: "readme"},
	{path: "kiln/share/doc.txt", content: "doc"},
	{path: "kiln/bin/k", link: "kiln"},
}

func TestPackTarGz(t *testing.T) {
	store, files := putFiles(t, sampleFiles)
	for _, archive := range []string{"", ArchiveTarGz} {
		t.Run("archive="+archive, func(t *testing.T) {
			a := pack(t, store, archive, "kiln-1.0.0-linux-amd64", files)
			if a.Name != "kiln-1.0.0-linux-amd64.tar.gz" {
				t.Errorf("Name = %q", a.Name)
			}
			b := readFile(t, a.Path)
			if a.Size != int64(len(b)) || a.SHA256 != sha256Hex(b) {
				t.Errorf("Size/SHA256 = %d/%s，与文件的 %d/%s 不一致", a.Size, a.SHA256, len(b), sha256Hex(b))
			}

			type want struct {
				name    string
				typ     byte
				mode    int64
				link    string
				content string
			}
			// 文件按路径排序，父目录条目在第一个子条目之前补齐
			wants := []want{
				{name: "README.md", typ: tar.TypeReg, mode: 0o644, content: "readme"},
				{name: "kiln/", typ: tar.TypeDir, mode: 0o755},
				{name: "kiln/bin/", typ: tar.TypeDir, mode: 0o755},
				{name: "kiln/bin/k", typ: tar.TypeSymlink, mode: 0o777, link: "kiln"},
				{name: "kiln/bin/kiln", typ: tar.TypeReg, mode: 0o755, content: "#!binary"},
				{name: "kiln/share/", typ: tar.TypeDir, mode: 0o755},
				{name: "kiln/share/doc.txt", typ: tar.TypeReg, mode: 0o644, content: "doc"},
			}
			items := readTarGz(t, a.Path)
			if len(items) != len(wants) {
				var names []string
				for _, it := range items {
					names = append(names, it.hdr.Name)
				}
				t.Fatalf("条目 = %v，期望 %d 个", names, len(wants))
			}
			for i, w := range wants {
				h := items[i].hdr
				if h.Name != w.name || h.Typeflag != w.typ || h.Mode != w.mode || h.Linkname != w.link || items[i].content != w.content {
					t.Errorf("第 %d 个条目 = {%q %c %o %q %q}，期望 {%q %c %o %q %q}",
						i, h.Name, h.Typeflag, h.Mode, h.Linkname, items[i].content, w.name, w.typ, w.mode, w.link, w.content)
				}
				// 属主为 0、没有用户名，修改时间固定
				if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
					t.Errorf("%s 的属主 = %d:%d（%q:%q），期望 0:0", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
				}
				if !h.ModTime.Equal(testMTime) {
					t.Errorf("%s 的 mtime = %v，期望 %v", h.Name, h.ModTime, testMTime)
				}
			}
		})
	}
}

func TestPackZip(t *testing.T) {
	store, files := putFiles(t, sampleFiles)
	a := pack(t, store, ArchiveZip, "kiln-1.0.0-darwin-arm64", files)
	if a.Name != "kiln-1.0.0-darwin-arm64.zip" {
		t.Errorf("Name = %q", a.Name)
	}
	b := readFile(t, a.Path)
	if a.Size != int64(len(b)) || a.SHA256 != sha256Hex(b) {
		t.Errorf("Size/SHA256 = %d/%s，与文件不一致", a.Size, a.SHA256)
	}
	wants := []zipItem{
		{name: "README.md", mode: 0o644, content: "readme"},
		{name: "kiln/bin/k", mode: os.ModeSymlink | 0o777, content: "kiln"},
		{name: "kiln/bin/kiln", mode: 0o755, content: "#!binary"},
		{name: "kiln/share/doc.txt", mode: 0o644, content: "doc"},
	}
	items := readZip(t, a.Path)
	if len(items) != len(wants) {
		t.Fatalf("条目 = %+v，期望 %d 个", items, len(wants))
	}
	for i, w := range wants {
		got := items[i]
		if got.name != w.name || got.mode != w.mode || got.content != w.content {
			t.Errorf("第 %d 个条目 = {%q %v %q}，期望 {%q %v %q}", i, got.name, got.mode, got.content, w.name, w.mode, w.content)
		}
		if !got.modified.Equal(testMTime) {
			t.Errorf("%s 的修改时间 = %v，期望 %v", got.name, got.modified, testMTime)
		}
	}
}

func TestPackNone(t *testing.T) {
	store, files := putFiles(t, []srcFile{{path: "dist/kiln.exe", content: "MZ", exec: true}})
	a := pack(t, store, ArchiveNone, "kiln-1.0.0-windows-amd64", files)
	// 文件名沿用原始文件的扩展名，内容原样上传
	if a.Name != "kiln-1.0.0-windows-amd64.exe" {
		t.Errorf("Name = %q", a.Name)
	}
	if got := string(readFile(t, a.Path)); got != "MZ" {
		t.Errorf("内容 = %q，期望原始文件内容", got)
	}
	if a.SHA256 != sha256Hex([]byte("MZ")) || a.Size != 2 {
		t.Errorf("Size/SHA256 = %d/%s", a.Size, a.SHA256)
	}
}

func TestPackNoneRejects(t *testing.T) {
	cases := []struct {
		name  string
		files []srcFile
	}{
		{name: "没有文件", files: nil},
		{name: "多个文件", files: []srcFile{{path: "a", content: "a"}, {path: "b", content: "b"}}},
		{name: "符号链接", files: []srcFile{{path: "a", link: "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, files := putFiles(t, tc.files)
			work := t.TempDir()
			_, err := Pack(context.Background(), store, PackRequest{Archive: ArchiveNone, Name: "x", Files: files, WorkDir: work, ModTime: testMTime})
			if err == nil || !strings.Contains(err.Error(), "恰好是一个文件") {
				t.Fatalf("Pack = %v，期望\"恰好是一个文件\"的错误", err)
			}
		})
	}
}

// 文件名：tar.gz / zip 加固定扩展名，none 取原始文件的扩展名（没有扩展名时不加）。
func TestAssetFileName(t *testing.T) {
	cases := []struct {
		archive, file, want string
	}{
		{archive: "", file: "a/b.bin", want: "app.tar.gz"},
		{archive: ArchiveTarGz, file: "a/b.bin", want: "app.tar.gz"},
		{archive: ArchiveZip, file: "a/b.bin", want: "app.zip"},
		{archive: ArchiveNone, file: "a/b.bin", want: "app.bin"},
		{archive: ArchiveNone, file: "bin/app", want: "app"},
		{archive: ArchiveNone, file: "x.tar.gz", want: "app.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.archive+"/"+tc.file, func(t *testing.T) {
			got, err := assetFileName(tc.archive, "app", []cas.FileEntry{{Path: tc.file}})
			if err != nil || got != tc.want {
				t.Errorf("assetFileName(%q, app, %q) = %q, %v，期望 %q", tc.archive, tc.file, got, err, tc.want)
			}
		})
	}
	// 名字含路径分隔符时拒绝（打包会写到工作目录之外）
	for _, name := range []string{"../app", "a/b", "", ".."} {
		if _, err := assetFileName(ArchiveTarGz, name, []cas.FileEntry{{Path: "f"}}); err == nil {
			t.Errorf("assetFileName(%q) 应报错", name)
		}
	}
}

// 同样的输入（不论文件顺序、工作目录）产生逐字节相同的 asset。
func TestPackDeterministic(t *testing.T) {
	store, files := putFiles(t, sampleFiles)
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	for _, archive := range []string{ArchiveTarGz, ArchiveZip} {
		t.Run(archive, func(t *testing.T) {
			a1 := pack(t, store, archive, "app", files)
			a2 := pack(t, store, archive, "app", reversed)
			if !bytes.Equal(readFile(t, a1.Path), readFile(t, a2.Path)) || a1.SHA256 != a2.SHA256 {
				t.Fatalf("两次打包的结果不同：%s / %s", a1.SHA256, a2.SHA256)
			}
		})
	}
	// 修改时间不同时结果不同（证明 mtime 确实写入包中，而不是取当前时间）
	a1 := pack(t, store, ArchiveTarGz, "app", files)
	a3, err := Pack(context.Background(), store, PackRequest{Name: "app", Files: files, WorkDir: t.TempDir(), ModTime: testMTime.AddDate(0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if a1.SHA256 == a3.SHA256 {
		t.Error("修改时间不同，打包结果却相同")
	}
}

// Asset 记录原始文件（供源仓库名检查读取内容）和包内路径、符号链接目标。
func TestPackRecordsOriginals(t *testing.T) {
	store, files := putFiles(t, sampleFiles)
	a := pack(t, store, ArchiveTarGz, "app", files)
	var contents []string
	for _, p := range a.Files {
		contents = append(contents, string(readFile(t, p)))
	}
	// 按路径排序后的普通文件：README.md、kiln/bin/kiln、kiln/share/doc.txt
	if want := []string{"readme", "#!binary", "doc"}; !slices.Equal(contents, want) {
		t.Errorf("原始文件内容 = %q，期望 %q", contents, want)
	}
	if want := []string{"README.md", "kiln/bin/k", "kiln", "kiln/bin/kiln", "kiln/share/doc.txt"}; !slices.Equal(a.Names, want) {
		t.Errorf("Names = %q，期望 %q", a.Names, want)
	}
}

// 同一个工作目录中打多个 asset，原始文件互不覆盖。
func TestPackSharedWorkDir(t *testing.T) {
	store, files := putFiles(t, []srcFile{{path: "a", content: "first"}, {path: "b", content: "second"}})
	work := t.TempDir()
	ctx := context.Background()
	a1, err := Pack(ctx, store, PackRequest{Name: "one", Files: files[:1], WorkDir: work, ModTime: testMTime})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := Pack(ctx, store, PackRequest{Name: "two", Files: files[1:], WorkDir: work, ModTime: testMTime})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, a1.Files[0])); got != "first" {
		t.Errorf("第一个 asset 的原始文件 = %q，被覆盖", got)
	}
	if got := string(readFile(t, a2.Files[0])); got != "second" {
		t.Errorf("第二个 asset 的原始文件 = %q", got)
	}
}

// CAS 中缺少文件时打包失败，错误中带有文件路径。
func TestPackMissingObject(t *testing.T) {
	store := &cas.FSStore{Root: t.TempDir()}
	files := []cas.FileEntry{{Path: "bin/app", Digest: cas.Digest([]byte("absent")), Size: 6}}
	for _, archive := range []string{ArchiveTarGz, ArchiveZip, ArchiveNone} {
		t.Run(archive, func(t *testing.T) {
			_, err := Pack(context.Background(), store, PackRequest{Archive: archive, Name: "app", Files: files, WorkDir: t.TempDir(), ModTime: testMTime})
			if err == nil || !strings.Contains(err.Error(), "bin/app") {
				t.Fatalf("Pack = %v，期望带有文件路径的错误", err)
			}
		})
	}
}

// 工作目录不可用时返回错误。
func TestPackWorkDirUnusable(t *testing.T) {
	store, files := putFiles(t, []srcFile{{path: "a", content: "a"}})
	work := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(work, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), store, PackRequest{Name: "app", Files: files, WorkDir: work, ModTime: testMTime}); err == nil {
		t.Fatal("WorkDir 是文件时 Pack 应失败")
	}
}

func TestWriteChecksums(t *testing.T) {
	dir := t.TempDir()
	assets := []Asset{
		{Name: "app-linux.tar.gz", SHA256: strings.Repeat("a", 64)},
		{Name: "app-darwin.zip", SHA256: strings.Repeat("b", 64)},
	}
	a, err := WriteChecksums(dir, "checksums.txt", assets)
	if err != nil {
		t.Fatal(err)
	}
	// sha256sum 格式：按给定顺序，每行"<hex>  <文件名>"
	want := strings.Repeat("a", 64) + "  app-linux.tar.gz\n" + strings.Repeat("b", 64) + "  app-darwin.zip\n"
	b := readFile(t, a.Path)
	if string(b) != want {
		t.Errorf("内容 = %q，期望 %q", b, want)
	}
	if a.Name != "checksums.txt" || a.Size != int64(len(want)) || a.SHA256 != sha256Hex([]byte(want)) {
		t.Errorf("Asset = %+v", a)
	}
	// 写在带下划线前缀的文件中，不与同名的 asset 冲突
	if a.Path != filepath.Join(dir, "_checksums.txt") {
		t.Errorf("Path = %s", a.Path)
	}
	if _, err := WriteChecksums(filepath.Join(dir, "missing"), "x", assets); err == nil {
		t.Error("目录不存在时应返回错误")
	}
	// 文件名来自 workflow 的 release.checksums：含路径分隔符时不能写到 dir 之外（以前 filepath.Join 会把 .. 解析掉）
	sub := filepath.Join(dir, "work")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x/../../escaped", "../escaped", "a\\b", "..", ".", ""} {
		if _, err := WriteChecksums(sub, name, assets); err == nil || !strings.Contains(err.Error(), "无效") {
			t.Errorf("WriteChecksums(%q) = %v，期望拒绝", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); err == nil {
		t.Error("校验和文件被写到了工作目录之外")
	}
}

// 打包后的 asset 与源仓库名检查配合：包内路径、符号链接目标和文件内容中的仓库名都能被发现。
func TestPackThenSourceNameCheck(t *testing.T) {
	cases := []struct {
		name  string
		files []srcFile
		hit   string
	}{
		{name: "干净", files: []srcFile{{path: "bin/app", content: "clean"}}},
		{name: "文件内容", files: []srcFile{{path: "bin/app", content: "built from Owner/Private"}}, hit: "第 1 个文件"},
		{name: "包内路径", files: []srcFile{{path: "owner/private/app", content: "x"}}, hit: "中的路径 owner/private/app"},
		{name: "符号链接目标", files: []srcFile{{path: "a", link: "../owner/private"}}, hit: "中的路径 ../owner/private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, files := putFiles(t, tc.files)
			a := pack(t, store, ArchiveTarGz, "app", files)
			err := SourceNameCheck("owner/private", []Asset{*a}, "")
			if tc.hit == "" {
				if err != nil {
					t.Fatalf("不应命中：%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.hit) {
				t.Fatalf("SourceNameCheck = %v，期望提到 %q", err, tc.hit)
			}
		})
	}
}

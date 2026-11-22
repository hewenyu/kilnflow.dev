package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPackNpm(t *testing.T) {
	store, files := putFiles(t, []srcFile{
		{path: "bin/kiln-runner", content: "runner", exec: true},
		{path: "README.md", content: "readme"},
	})
	pkg := []byte(`{"name":"@s/a","version":"1.0.0"}` + "\n")
	packOnce := func() *Asset {
		t.Helper()
		// 输入顺序不影响结果
		a, err := PackNpm(t.Context(), store, NpmPackRequest{Name: "a-1.0.0", Files: slices.Clone(files), Package: pkg, WorkDir: t.TempDir(), ModTime: testMTime})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := packOnce()
	slices.Reverse(files)
	if b := packOnce(); b.SHA256 != a.SHA256 || b.Name != "a-1.0.0.tgz" {
		t.Errorf("两次打包不一致：%s %s", a.SHA256, b.SHA256)
	}
	entries := tarEntries(t, a.Path)
	want := []string{"package/ dir 755", "package/package.json file 644", "package/README.md file 644", "package/bin/ dir 755", "package/bin/kiln-runner file 755"}
	if !slices.Equal(entries, want) {
		t.Errorf("条目 %v\n期望 %v", entries, want)
	}
	// 源仓库名检查用到的原始文件包含 package.json
	if len(a.Files) != 3 || !slices.Contains(a.Names, "package/bin/kiln-runner") {
		t.Errorf("Files %v Names %v", a.Files, a.Names)
	}

	_, withPkg := putFiles(t, []srcFile{{path: "package.json", content: "{}"}})
	if _, err := PackNpm(t.Context(), store, NpmPackRequest{Name: "x", Files: withPkg, Package: pkg, WorkDir: t.TempDir(), ModTime: testMTime}); err == nil {
		t.Error("package.json 不能作为普通文件")
	}
}

// tarEntries 列出 tgz 中的条目：名称、类型、权限。
func tarEntries(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		kind := "file"
		if h.Typeflag == tar.TypeDir {
			kind = "dir"
		}
		if !h.ModTime.Equal(testMTime) {
			t.Errorf("%s 的 mtime %v", h.Name, h.ModTime)
		}
		out = append(out, fmt.Sprintf("%s %s %o", h.Name, kind, h.Mode))
	}
	return out
}

// writeTgz 生成测试用的 tgz：entries 依次为名称和内容（内容为 "->x" 时是指向 x 的符号链接，
// 以 "cont:" 开头时是内容为其余部分的连续文件条目，tar 类型 7）。
func writeTgz(t *testing.T, entries ...string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i+1 < len(entries); i += 2 {
		name, body := entries[i], entries[i+1]
		h := &tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(body))}
		if target, ok := strings.CutPrefix(body, "->"); ok {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, target, 0
		}
		if rest, ok := strings.CutPrefix(body, "cont:"); ok {
			body, h.Typeflag, h.Size = rest, tar.TypeCont, int64(len(rest))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeSymlink {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pkg.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadNpmTarball(t *testing.T) {
	work := t.TempDir()
	tgz := writeTgz(t,
		"package/package.json", `{"name":"@s/lib","version":"1.0.0"}`,
		"package/lib/index.js", "x",
		"package/link", "->/outside/file.txt",
	)
	tb, err := ReadNpmTarball(tgz, work)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Manifest["name"] != "@s/lib" || tb.Name != "pkg.tgz" || tb.Path != tgz || tb.Size == 0 || tb.SHA256 == "" {
		t.Errorf("结果 %+v", tb)
	}
	// 符号链接的目标也记下名字（用于源仓库名检查），文件内容写到工作目录下的编号文件中，不会越出目录
	if len(tb.Files) != 2 || !slices.Contains(tb.Names, "/outside/file.txt") {
		t.Errorf("Files %v Names %v", tb.Files, tb.Names)
	}
	for _, f := range tb.Files {
		if !strings.HasPrefix(f, work) {
			t.Errorf("解出的文件 %s 不在工作目录中", f)
		}
	}

	for _, tc := range []struct {
		name    string
		entries []string
		want    string
	}{
		{"没有 package.json", []string{"package/index.js", "x"}, "没有 package.json"},
		// 包内路径必须相对、不能跳出顶层目录：这些包整体拒绝，不只跳过条目
		{"路径跳出顶层目录", []string{"package/package.json", "{}", "../../evil", "x"}, "必须是包内的相对路径"},
		{"绝对路径", []string{"package/package.json", "{}", "/outside/file.txt", "x"}, "必须是包内的相对路径"},
		{"package.json 不在顶层目录下", []string{"package/sub/package.json", "{}"}, "没有 package.json"},
		{"package.json 不是对象", []string{"package/package.json", "[1]"}, "不是合法的 JSON"},
		{"package.json 是 null", []string{"package/package.json", "null"}, "不是 JSON 对象"},
		// 以前超过 1 MiB 的 package.json 被当成没有，报错"没有 package.json"，或者沿用前面同名条目的内容
		{"package.json 太大", []string{
			"package/package.json", `{"name":"@s/lib","version":"1.0.0"}`,
			"package/package.json", `{"pad":"` + strings.Repeat("x", 1<<20) + `"}`,
		}, "太大"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadNpmTarball(writeTgz(t, tc.entries...), t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %v，期望包含 %q", err, tc.want)
			}
		})
	}
	if _, err := ReadNpmTarball(writeTgz(t), t.TempDir()); err == nil {
		t.Error("空包应报错")
	}
}

// 类型 7（连续文件）的条目会被 npm 解成普通文件：它的内容、作为 package.json 时的内容都要读到，
// 否则源仓库名检查看不到这些文件（以前只读 TypeReg）。
func TestReadNpmTarballContiguousFile(t *testing.T) {
	const repo = "owner/private-src"
	tgz := writeTgz(t,
		"package/package.json", `cont:{"name":"@s/lib","version":"1.0.0","repository":"github.com/`+repo+`"}`,
		"package/build-info.txt", "cont:built from "+repo,
	)
	tb, err := ReadNpmTarball(tgz, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if tb.Manifest["name"] != "@s/lib" || len(tb.Files) != 2 {
		t.Fatalf("Manifest %v，Files %v", tb.Manifest, tb.Files)
	}
	err = SourceNameCheck(repo, []Asset{tb.Asset}, "")
	if err == nil || !strings.Contains(err.Error(), "第 2 个文件") {
		t.Fatalf("应发现类型 7 条目中的仓库名：%v", err)
	}
}

// gzip 尾部损坏（CRC、长度）或截断的包要在读取时拒绝：tar 条目本身完好，只读到 tar 结束标记发现不了。
func TestReadNpmTarballCorrupted(t *testing.T) {
	good, err := os.ReadFile(writeTgz(t, "package/package.json", `{"name":"@s/lib","version":"1.0.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
		want   string
	}{
		{"CRC 错误", func(b []byte) []byte { b[len(b)-8] ^= 0xff; return b }, "npm 包已损坏"},
		{"长度错误", func(b []byte) []byte { b[len(b)-1] ^= 0xff; return b }, "npm 包已损坏"},
		{"缺少 gzip 尾部", func(b []byte) []byte { return b[:len(b)-8] }, "npm 包已损坏"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "bad.tgz")
			if err := os.WriteFile(p, tc.mutate(slices.Clone(good)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadNpmTarball(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %v，期望包含 %q", err, tc.want)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "good.tgz")
	if err := os.WriteFile(p, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNpmTarball(p, t.TempDir()); err != nil {
		t.Errorf("完好的包：%v", err)
	}
}

// npm 包内路径必须相对、不能跳出顶层目录；"."（顶层目录本身）允许。
func TestValidNpmEntryName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"package/index.js", true},
		{"package/./index.js", true},
		{".", true},
		{"package/link", true},
		{"", false},
		{"/package.json", false},
		{"../evil", false},
		{"package/../../evil", false},
		{"..", false},
		{"./../evil", false},
	} {
		if got := validNpmEntryName(tc.name); got != tc.ok {
			t.Errorf("validNpmEntryName(%q) = %v，期望 %v", tc.name, got, tc.ok)
		}
	}
}

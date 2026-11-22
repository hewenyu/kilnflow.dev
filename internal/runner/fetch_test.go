package runner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// newFetchJob 创建一个只有本地 CAS 缓存的 job；对象都已在缓存中，不会访问 Server。
func newFetchJob(t *testing.T) *job {
	t.Helper()
	r := &Runner{cfg: &Config{Dir: t.TempDir()}}
	return &job{r: r, spec: &runnerv1.JobSpec{}}
}

func putBlob(t *testing.T, j *job, data []byte) string {
	t.Helper()
	d := cas.Digest(data)
	if _, err := j.r.casStore().Put(context.Background(), d, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return d
}

func putManifest(t *testing.T, j *job, files []cas.FileEntry) string {
	t.Helper()
	b, err := actionkey.Canonical(cas.OutputManifest{Schema: cas.OutputSchema, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return putBlob(t, j, b)
}

func TestPlaceFetch(t *testing.T) {
	t.Parallel()
	j := newFetchJob(t)
	bin := putBlob(t, j, []byte("binary"))
	txt := putBlob(t, j, []byte("text"))
	m := putManifest(t, j, []cas.FileEntry{
		{Path: "bin/tool", Digest: bin, Size: 6, Exec: true},
		{Path: "README", Digest: txt, Size: 4},
		{Path: "bin/alias", Symlink: "tool"},
	})
	ws := t.TempDir()
	// 工作区中已有的同名文件会被覆盖
	mustWrite(t, filepath.Join(ws, "deps", "README"), "old")
	n, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws)
	if err != nil || n != 3 {
		t.Fatalf("placeFetch = %d, %v", n, err)
	}
	st, err := os.Stat(filepath.Join(ws, "deps", "bin", "tool"))
	if err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/tool：%v %v", st, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "deps", "README")); err != nil || string(b) != "text" {
		t.Fatalf("README = %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(ws, "deps", "bin", "alias")); err != nil || target != "tool" {
		t.Fatalf("bin/alias → %q, %v", target, err)
	}
}

// TestPlaceFetchRejectsSymlinkOutsideDestination 复现 AI Review 的 High Finding：fetch.to = "deps" 时写入边界
// 只能到输出目录，不能放宽到整个工作区。否则 deps/escape -> ../src 会通过检查，随后 deps/escape/config.go
// 会经由这条链接覆盖工作区里的 src/config.go。
func TestPlaceFetchRejectsSymlinkOutsideDestination(t *testing.T) {
	t.Parallel()
	j := newFetchJob(t)
	blob := putBlob(t, j, []byte("pwned"))
	m := putManifest(t, j, []cas.FileEntry{
		{Path: "escape", Symlink: "../src"},
		{Path: "escape/config.go", Digest: blob},
	})
	ws := filepath.Join(t.TempDir(), "ws")
	mustWrite(t, filepath.Join(ws, "src", "config.go"), "original-source")
	if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
		t.Fatal("期望报错")
	}
	if b, err := os.ReadFile(filepath.Join(ws, "src", "config.go")); err != nil || string(b) != "original-source" {
		t.Fatalf("src/config.go 被改写为 %q, %v", b, err)
	}
}

// TestPlaceFetchRejectsSymlinkOutOfTo 覆盖链接目标落在工作区内、但在输出目录之外的情况：即使后续没有条目
// 顺着链接写文件，也不能留下逃出 deps 的链接（后续 step 会照常解析它）。
func TestPlaceFetchRejectsSymlinkOutOfTo(t *testing.T) {
	t.Parallel()
	j := newFetchJob(t)
	m := putManifest(t, j, []cas.FileEntry{{Path: "escape", Symlink: "../src"}})
	ws := filepath.Join(t.TempDir(), "ws")
	mustWrite(t, filepath.Join(ws, "src", "config.go"), "original-source")
	if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
		t.Fatal("期望报错")
	}
	if _, err := os.Lstat(filepath.Join(ws, "deps", "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("留下了逃出 deps 的链接：%v", err)
	}
}

// TestPlaceFetchRejectsReversedChainOutOfTo 覆盖反序 manifest 的链式逃逸：a_escape 写入时 d1/d2/sub 还不存在，
// 按字面解析仍在 deps 内；随后 d1/d2/sub -> ../.. 才把它的落点带出 deps（仍在工作区内）。单条检查都通过，
// 必须在全部条目写完后以输出目录为根复查，并把这次写入的条目清理干净。
func TestPlaceFetchRejectsReversedChainOutOfTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{name: "逃出 deps、留在工作区", target: "d1/d2/sub/../src"},
		{name: "连工作区一起逃出", target: "d1/d2/sub/../../../src"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newFetchJob(t)
			m := putManifest(t, j, []cas.FileEntry{
				{Path: "a_escape", Symlink: c.target},
				{Path: "d1/d2/sub", Symlink: "../.."},
			})
			ws := filepath.Join(t.TempDir(), "ws")
			mustWrite(t, filepath.Join(ws, "src", "config.go"), "original-source")
			if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
				t.Fatal("期望报错")
			}
			entries, err := os.ReadDir(filepath.Join(ws, "deps"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("清理后仍有残留：%s", e.Name())
			}
			if b, err := os.ReadFile(filepath.Join(ws, "src", "config.go")); err != nil || string(b) != "original-source" {
				t.Fatalf("src/config.go 被改写为 %q, %v", b, err)
			}
		})
	}
}

// TestPlaceFetchRejectsNonCanonicalPath 覆盖非规范化的 manifest 路径：filepath.Join 会把 d/link/../escape
// 词法化简为 d/escape（于是 ../src 看起来仍在 deps 内），os.Root.Symlink 却先解开 d/link 再走 ..，实际在
// 输出目录根创建了指向 ../src 的越界链接。任何 "."、".."、空组件和反斜杠都必须拒绝。
func TestPlaceFetchRejectsNonCanonicalPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files func(blob string) []cas.FileEntry
	}{
		{name: "中间符号链接之后的 ..", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{
				{Path: "x/keep", Digest: b},
				{Path: "d/link", Symlink: "../x"},
				{Path: "d/link/../escape", Symlink: "../src"},
			}
		}},
		{name: "空组件", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "a//b", Digest: b}}
		}},
		{name: "点组件", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "a/./b", Digest: b}}
		}},
		{name: "结尾分隔符", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "a/", Digest: b}}
		}},
		{name: "反斜杠", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: `a\b`, Digest: b}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newFetchJob(t)
			blob := putBlob(t, j, []byte("pwned"))
			m := putManifest(t, j, c.files(blob))
			ws := filepath.Join(t.TempDir(), "ws")
			mustWrite(t, filepath.Join(ws, "src", "config.go"), "original-source")
			if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
				t.Fatal("期望报错")
			}
			if _, err := os.Lstat(filepath.Join(ws, "deps", "escape")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("留下了 deps/escape：%v", err)
			}
			if b, err := os.ReadFile(filepath.Join(ws, "src", "config.go")); err != nil || string(b) != "original-source" {
				t.Fatalf("src/config.go 被改写为 %q, %v", b, err)
			}
		})
	}
}

// TestPlaceFetchRejectsPathThroughDirSymlink 覆盖路径本身干净、但父目录是符号链接的情况：d/link -> .. 让
// d/link/escape 的父目录解析到 deps 根，target ../src 于是在 deps 之外，必须由 symlinkWithinRoot 拒绝，
// 且不能留下这条链接。
func TestPlaceFetchRejectsPathThroughDirSymlink(t *testing.T) {
	t.Parallel()
	j := newFetchJob(t)
	m := putManifest(t, j, []cas.FileEntry{
		{Path: "d/link", Symlink: ".."},
		{Path: "d/link/escape", Symlink: "../src"},
	})
	ws := filepath.Join(t.TempDir(), "ws")
	mustWrite(t, filepath.Join(ws, "src", "config.go"), "original-source")
	if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
		t.Fatal("期望报错")
	}
	// 经由 d/link 解析，这条链接实际会落在 deps/escape
	if _, err := os.Lstat(filepath.Join(ws, "deps", "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("留下了 deps/escape：%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(ws, "deps"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("清理后仍有残留：%s", e.Name())
	}
}

// TestPlaceFetchCleanupKeepsExistingDirs 覆盖失败清理不能误删工作区原有的空目录：缓存目录等在 fetch 之前
// 就存在，只是这次往里写了文件。
func TestPlaceFetchCleanupKeepsExistingDirs(t *testing.T) {
	t.Parallel()
	j := newFetchJob(t)
	blob := putBlob(t, j, []byte("pwned"))
	m := putManifest(t, j, []cas.FileEntry{
		{Path: "cache/item", Digest: blob},
		{Path: "bad", Symlink: "../outside"},
	})
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m}, ws); err == nil {
		t.Fatal("期望报错")
	}
	if _, err := os.Lstat(filepath.Join(ws, "cache", "item")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache/item 没有被清理：%v", err)
	}
	if fi, err := os.Lstat(filepath.Join(ws, "cache")); err != nil || !fi.IsDir() {
		t.Fatalf("原有的空目录 cache 被删掉了：%v %v", fi, err)
	}
}

func TestPlaceFetchRejectsEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files func(blob string) []cas.FileEntry
		setup func(t *testing.T, ws, outside string)
	}{
		{name: "../ 路径", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "../evil", Digest: b}}
		}},
		{name: "跳出 to 目录", files: func(b string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "x/../../evil", Digest: b}}
		}},
		{name: "绝对路径的符号链接", files: func(string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "a", Symlink: "/etc"}}
		}},
		{name: "相对路径的符号链接指向外面", files: func(string) []cas.FileEntry {
			return []cas.FileEntry{{Path: "a", Symlink: "../../.."}}
		}},
		{name: "链式符号链接逃逸", files: func(string) []cas.FileEntry {
			return []cas.FileEntry{
				{Path: "d1/d2/sub", Symlink: "../.."},
				{Path: "escape", Symlink: "d1/d2/sub/../../../outside/target.txt"},
			}
		}},
		// 反序 manifest：逃逸链接先用不存在的 d1/d2/sub 通过单条检查，随后才创建 d1/d2/sub；
		// 全部写完后必须重新复查每一条符号链接。
		{name: "反序的链式符号链接逃逸", files: func(string) []cas.FileEntry {
			return []cas.FileEntry{
				{Path: "a_escape", Symlink: "d1/d2/sub/../../../outside/target.txt"},
				{Path: "d1/d2/sub", Symlink: "../.."},
			}
		}},
		// 前段不存在时不能跳过后续已有符号链接的解析：missing/.. 会回到 d1/d2/sub 所在的目录。
		{name: "经不存在段的 .. 逃逸", files: func(string) []cas.FileEntry {
			return []cas.FileEntry{
				{Path: "b_escape", Symlink: "missing/../d1/d2/sub/../../../outside/target.txt"},
				{Path: "d1/d2/sub", Symlink: "../.."},
			}
		}},
		{
			name: "工作区中已有指向外面的符号链接",
			files: func(b string) []cas.FileEntry {
				return []cas.FileEntry{{Path: "evil", Digest: b}}
			},
			setup: func(t *testing.T, ws, outside string) {
				t.Helper()
				mustSymlink(t, outside, filepath.Join(ws, "deps"))
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			j := newFetchJob(t)
			blob := putBlob(t, j, []byte("pwned"))
			m := putManifest(t, j, c.files(blob))
			parent := t.TempDir()
			ws := filepath.Join(parent, "ws")
			if err := os.Mkdir(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if c.setup != nil {
				c.setup(t, ws, outside)
			}
			if _, err := j.placeFetch(context.Background(), &runnerv1.FetchSpec{ManifestDigest: m, To: "deps"}, ws); err == nil {
				t.Fatal("期望报错")
			}
			for _, dir := range []string{outside, parent} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if e.Name() != "ws" {
						t.Errorf("在工作区之外创建了 %s", filepath.Join(dir, e.Name()))
					}
				}
			}
		})
	}
}

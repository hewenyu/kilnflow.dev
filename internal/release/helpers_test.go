package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// testMTime 是测试中打包使用的固定修改时间（例如 commit 时间）。
var testMTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// srcFile 描述输出中的一个文件：content 为空且 link 非空时是符号链接。
type srcFile struct {
	path, content, link string
	exec                bool
}

// putFiles 把文件内容写入一个新的 CAS，返回 store 和对应的 FileEntry。
func putFiles(t *testing.T, files []srcFile) (*cas.FSStore, []cas.FileEntry) {
	t.Helper()
	store := &cas.FSStore{Root: t.TempDir()}
	entries := make([]cas.FileEntry, 0, len(files))
	for _, f := range files {
		if f.link != "" {
			entries = append(entries, cas.FileEntry{Path: f.path, Symlink: f.link})
			continue
		}
		d := cas.Digest([]byte(f.content))
		if _, err := store.Put(context.Background(), d, bytes.NewReader([]byte(f.content))); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, cas.FileEntry{Path: f.path, Digest: d, Size: int64(len(f.content)), Exec: f.exec})
	}
	return store, entries
}

// pack 用 testMTime 打包，失败时终止测试。
func pack(t *testing.T, store cas.Store, archive, name string, files []cas.FileEntry) *Asset {
	t.Helper()
	a, err := Pack(context.Background(), store, PackRequest{Archive: archive, Name: name, Files: files, WorkDir: t.TempDir(), ModTime: testMTime})
	if err != nil {
		t.Fatalf("Pack(%s) = %v", archive, err)
	}
	return a
}

// readFile 读取文件，失败时终止测试。
func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sha256Hex 返回内容的 sha256（hex）。
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tarItem 是从 tar.gz 中读出的一个条目。
type tarItem struct {
	hdr     *tar.Header
	content string
}

// readTarGz 按顺序读出 tar.gz 中的所有条目。
func readTarGz(t *testing.T, p string) []tarItem {
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
	if !gz.ModTime.IsZero() || gz.Name != "" {
		t.Errorf("gzip 头应不含时间和文件名：ModTime=%v Name=%q", gz.ModTime, gz.Name)
	}
	tr := tar.NewReader(gz)
	var items []tarItem
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return items
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, tarItem{hdr: hdr, content: string(b)})
	}
}

// zipItem 是从 zip 中读出的一个条目。
type zipItem struct {
	name     string
	mode     os.FileMode
	modified time.Time
	content  string
}

// readZip 按顺序读出 zip 中的所有条目。
func readZip(t *testing.T, p string) []zipItem {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var items []zipItem
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, zipItem{name: zf.Name, mode: zf.Mode(), modified: zf.Modified, content: string(b)})
	}
	return items
}

package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestScanReader(t *testing.T) {
	needle := []byte("owner/secret-repo")
	cases := []struct {
		name string
		data string
		want int64
	}{
		{"没有命中", "hello world", -1},
		{"开头", "Owner/Secret-Repo trailing", 0},
		{"中间", "xxOWNER/SECRET-REPOyy", 2},
		{"跨块", strings.Repeat("a", scanChunk-5) + "owner/secret-repo", scanChunk - 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanReader(strings.NewReader(tc.data), needle)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("scanReader = %d，期望 %d", got, tc.want)
			}
		})
	}
	// 每次只读 1 字节，验证跨块保留的字节
	got, err := scanReader(iotest.OneByteReader(strings.NewReader("abcOWNER/secret-repo")), needle)
	if err != nil || got != 3 {
		t.Errorf("逐字节读取：got %d, err %v", got, err)
	}
}

func TestSourceNameCheckFailsOnUnreadableFile(t *testing.T) {
	assets := []Asset{{Name: "a.tar.gz", Files: []string{filepath.Join(t.TempDir(), "missing")}}}
	if err := SourceNameCheck("owner/repo", assets, ""); err == nil {
		t.Fatal("文件无法读取时应返回错误")
	}
}

func TestSourceNameCheck(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean")
	dirty := filepath.Join(dir, "dirty")
	if err := os.WriteFile(clean, []byte("nothing here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("built from github.com/Owner/Repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SourceNameCheck("owner/repo", []Asset{{Name: "a", Files: []string{clean}}}, "notes"); err != nil {
		t.Errorf("不应命中：%v", err)
	}
	if err := SourceNameCheck("owner/repo", []Asset{{Name: "a", Files: []string{dirty}}}, ""); err == nil {
		t.Error("文件内容包含仓库名时应失败")
	}
	if err := SourceNameCheck("owner/repo", nil, "see OWNER/REPO"); err == nil {
		t.Error("说明包含仓库名时应失败")
	}
}

// 没有原始文件的 asset（例如校验和文件）直接检查 asset 文件本身；asset 名也在检查范围内。
func TestSourceNameCheckAssetWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	sums, err := WriteChecksums(dir, "checksums.txt", []Asset{{Name: "owner-repo.tar.gz", SHA256: "00"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SourceNameCheck("owner/repo", []Asset{*sums}, ""); err != nil {
		t.Errorf("校验和文件不含仓库名，不应命中：%v", err)
	}
	dirty, err := WriteChecksums(dir, "sums", []Asset{{Name: "Owner/Repo.tar.gz", SHA256: "00"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SourceNameCheck("owner/repo", []Asset{*dirty}, ""); err == nil || !strings.Contains(err.Error(), "sums（偏移") {
		t.Errorf("SourceNameCheck = %v，期望命中校验和文件的内容", err)
	}
	missing := Asset{Name: "x", Path: filepath.Join(dir, "missing")}
	if err := SourceNameCheck("owner/repo", []Asset{missing}, ""); err == nil {
		t.Error("asset 文件无法读取时应返回错误")
	}
	named := Asset{Name: "OWNER/REPO-linux", Path: sums.Path}
	if err := SourceNameCheck("owner/repo", []Asset{named}, ""); err == nil || !strings.Contains(err.Error(), "asset 名") {
		t.Errorf("SourceNameCheck = %v，期望命中 asset 名", err)
	}
}

func TestChangelogSection(t *testing.T) {
	doc := "# Changelog\n\n## [1.2.0] - 2026-01-01\n\n- 新功能\n\n## 1.1.0\n\n- 旧\n"
	got, ok := ChangelogSection(doc, "v1.2.0")
	if !ok || got != "- 新功能" {
		t.Errorf("ChangelogSection = %q, %v", got, ok)
	}
	if _, ok := ChangelogSection(doc, "v1.2"); ok {
		t.Error("v1.2 不应匹配 1.2.0")
	}
	if got, ok := ChangelogSection(doc, "1.1.0"); !ok || got != "- 旧" {
		t.Errorf("最后一段 = %q, %v", got, ok)
	}
	// 预发布版本的段落不是正式版本的说明
	pre := "## 2.0.0-rc.1\n\n- 预览\n\n## 2.0.0 (2026-02-01)\n\n- 正式\n"
	if got, ok := ChangelogSection(pre, "v2.0.0"); !ok || got != "- 正式" {
		t.Errorf("v2.0.0 = %q, %v，不应取 2.0.0-rc.1 的段落", got, ok)
	}
	if got, ok := ChangelogSection(pre, "v2.0.0-rc.1"); !ok || got != "- 预览" {
		t.Errorf("v2.0.0-rc.1 = %q, %v", got, ok)
	}
}

// 标题中版本号后面可以是空白、各种括号或多字节的破折号；空 tag 不匹配任何段落。
func TestChangelogSectionHeaders(t *testing.T) {
	t.Parallel()
	doc := "## 3.0.0 — 2026-03-01\n\n- 破折号\n\n## 4.0.0–beta\n\n- 范围\n"
	if got, ok := ChangelogSection(doc, "v3.0.0"); !ok || got != "- 破折号" {
		t.Errorf("多字节破折号 = %q, %v", got, ok)
	}
	if got, ok := ChangelogSection(doc, "v4.0.0"); !ok || got != "- 范围" {
		t.Errorf("多字节 en dash = %q, %v", got, ok)
	}
	for _, tag := range []string{"", "v"} {
		if _, ok := ChangelogSection(doc, tag); ok {
			t.Errorf("tag %q 不应匹配", tag)
		}
	}
}

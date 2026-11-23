package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	Cleanup()
	os.Exit(code)
}

// Init 建出真实的仓库：分支、文件内容、返回的 SHA 与 git 自己读到的一致，工作区是干净的。
func TestInit(t *testing.T) {
	dir := t.TempDir()
	sha := Init(t, dir, "main", map[string]string{"README.md": "demo\n", ".kiln/agent/config.yml": "version: 1\n"})
	if got := revParse(t, dir, "HEAD"); got != sha {
		t.Errorf("Init 返回的 SHA = %s，git rev-parse HEAD = %s", got, sha)
	}
	if got := revParse(t, dir, "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("当前分支 = %s，应为 main", got)
	}
	for name, want := range map[string]string{"README.md": "demo\n", ".kiln/agent/config.yml": "version: 1\n"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q %v，应为 %q", name, b, err, want)
		}
	}
	if out := gitOut(t, dir, "status", "--porcelain"); out != "" {
		t.Errorf("初始仓库的工作区应干净：%q", out)
	}
	if out := gitOut(t, dir, "log", "--format=%s"); out != "init" {
		t.Errorf("提交信息 = %q，应为 init", out)
	}
}

// 拷贝出的仓库各自独立：在其中一个里提交不影响另一个；同样的初始状态复用同一个模板（SHA 相同）。
func TestInitIndependent(t *testing.T) {
	files := map[string]string{"README.md": "demo\n"}
	a, b := t.TempDir(), t.TempDir()
	shaA := Init(t, a, "main", files)
	shaB := Init(t, b, "main", files)
	if shaA != shaB {
		t.Fatalf("同样的初始状态应复用模板：%s != %s", shaA, shaB)
	}
	if err := os.WriteFile(filepath.Join(a, "README.md"), []byte("改了\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, a, "commit", "-q", "-a", "-m", "change")
	if got := revParse(t, b, "HEAD"); got != shaB {
		t.Errorf("另一个仓库的 HEAD 不应改变：%s != %s", got, shaB)
	}
	if got := gitOut(t, b, "show", "HEAD:README.md"); got != "demo" {
		t.Errorf("另一个仓库的内容不应改变：%q", got)
	}
	// 不同的文件内容是不同的模板
	c := t.TempDir()
	if shaC := Init(t, c, "main", map[string]string{"README.md": "other\n"}); shaC == shaA {
		t.Error("不同的文件内容应建不同的模板")
	}
}

// 不在仓库目录内的路径直接拒绝，不会写到模板目录外面。
func TestBuildTemplateRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if err := buildTemplate(t, dir, "main", map[string]string{"../escape.txt": "x"}); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
}

func revParse(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitOut(t, dir, append([]string{"rev-parse"}, args...)...)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v：%v", args, err)
	}
	return strings.TrimSpace(string(out))
}

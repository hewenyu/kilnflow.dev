// Package gittest 给需要真实 git 仓库的测试用。
//
// 模块功能测试经常需要"一个新仓库，初始分支上已经提交了这些文件"。每次都 git init / add / commit 会为每个测试
// 启动三个 git 子进程，包里有上百个测试时占了测试时间的不小一部分。这里按（分支，文件内容）缓存只读模板仓库，
// 每个测试把模板递归拷贝到自己的临时目录：拷贝之后仓库互相独立，改动互不影响，仍然是真实的 git 仓库。
package gittest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	// templateMu 保护 templateRoot 和模板的建立：同一个模板只建一次。
	templateMu sync.Mutex
	// templateRoot 是本进程的模板目录，第一次使用时创建。
	templateRoot string
)

// Init 在 dir 中放好一个已初始化的仓库：分支 branch 上已经提交 files（路径 → 内容，目录自动创建），返回这个 commit 的 SHA。
// dir 必须是空的（测试用自己的 t.TempDir()）。模板只建一次，之后是一次递归拷贝；拷贝出的仓库各自独立。
//
// 模板目录在测试进程结束时删除：TestMain 用 pgtest.MainWith(m, gittest.Cleanup)（或自己调用 Cleanup）。
func Init(tb testing.TB, dir, branch string, files map[string]string) string {
	tb.Helper()
	tpl := template(tb, branch, files)
	if err := os.CopyFS(dir, os.DirFS(tpl)); err != nil {
		tb.Fatalf("gittest：拷贝模板仓库：%v", err)
	}
	return head(tb, dir, branch)
}

// Cleanup 删除本进程的模板目录；在 TestMain 退出前调用。
func Cleanup() {
	templateMu.Lock()
	defer templateMu.Unlock()
	if templateRoot == "" {
		return
	}
	_ = os.RemoveAll(templateRoot)
	templateRoot = ""
}

// template 返回（branch, files）对应的模板仓库目录，没有时建立。
func template(tb testing.TB, branch string, files map[string]string) string {
	tb.Helper()
	key := templateKey(branch, files)
	templateMu.Lock()
	defer templateMu.Unlock()
	if templateRoot == "" {
		root, err := os.MkdirTemp("", "kiln-gittest-") //nolint:usetesting // 整个测试进程共用的模板，TestMain 结束时删除
		if err != nil {
			tb.Fatalf("gittest：建模板目录：%v", err)
		}
		templateRoot = root
	}
	dir := filepath.Join(templateRoot, key)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	// 建在临时名字下、建好再改名：建到一半失败时不会留下被后续调用当成完整的模板。
	tmp, err := os.MkdirTemp(templateRoot, key+"-") //nolint:usetesting // 模板要活到测试进程结束（TestMain 清理）
	if err != nil {
		tb.Fatalf("gittest：建模板目录：%v", err)
	}
	if err := buildTemplate(tb, tmp, branch, files); err != nil {
		_ = os.RemoveAll(tmp)
		tb.Fatalf("gittest：建模板仓库：%v", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		tb.Fatalf("gittest：改名模板目录：%v", err)
	}
	return dir
}

// buildTemplate 在 dir 中初始化仓库并提交 files。
func buildTemplate(tb testing.TB, dir, branch string, files map[string]string) error {
	tb.Helper()
	if _, err := git(tb.Context(), dir, "init", "-q", "-b", branch); err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if !filepath.IsLocal(filepath.FromSlash(p)) {
			return fmt.Errorf("文件 %q 不在仓库目录内", p)
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			return err
		}
	}
	if _, err := git(tb.Context(), dir, "add", "-A"); err != nil {
		return err
	}
	_, err := git(tb.Context(), dir, "commit", "-q", "--allow-empty", "-m", "init")
	return err
}

// templateKey 是（分支，文件内容）的摘要：同一个初始状态复用同一个模板。
func templateKey(branch string, files map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%q\x00", branch)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(h, "%q\x00%d\x00%s\x00", p, len(files[p]), files[p])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// head 读取模板刚提交的分支的 SHA：仓库是我们自己建的，ref 一定是松散的（没有 packed-refs），读文件比再启动一个 git 快。
func head(tb testing.TB, dir, branch string) string {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".git", "refs", "heads", filepath.FromSlash(branch)))
	if err != nil {
		tb.Fatalf("gittest：读分支 %s 的 commit：%v", branch, err)
	}
	return strings.TrimSpace(string(b))
}

// git 在 dir 中执行 git 命令；环境与各包的测试 helper 相同：固定的作者和提交者，不读用户和机器的 git 配置。
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v：%w：%s", args, err, out)
	}
	return string(out), nil
}

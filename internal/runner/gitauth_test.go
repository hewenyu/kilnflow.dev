package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gitTestEnv 返回只有 PATH、HOME 和 extra 的环境（不读系统配置），HOME 是新的临时目录。
func gitTestEnv(t *testing.T, extra ...string) (env []string, home string) {
	t.Helper()
	home = t.TempDir()
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}, extra...), home
}

// 认证冲突的三项检查与查询失败（设计 4.3、V7）：结果只有键和来源，没有值。
func TestGitAuthConflicts(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	const secret = "c2VjcmV0LXZhbHVl"
	cases := []struct {
		name  string
		url   string
		setup func(t *testing.T, home, dir string) []string // 返回额外的环境变量；dir 是执行 git 的目录
		want  string                                        // 冲突的键（为空表示没有冲突）
	}{
		{name: "没有冲突", url: u},
		{name: "地址带用户名和密码", url: "https://user:" + secret + "@github.com/o/r.git", want: "地址中的用户名或密码"},
		{
			name: "全局配置的 extraHeader", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".gitconfig"), "[http]\n\textraHeader = Authorization: Basic "+secret+"\n")
				return nil
			},
			want: gitExtraHeaderKey,
		},
		{
			name: "环境变量配置的 extraHeader（限定地址）", url: u,
			setup: func(*testing.T, string, string) []string {
				return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_0=X: " + secret}
			},
			want: gitExtraHeaderKey,
		},
		{
			name: "其他地址的 extraHeader 不算", url: u,
			setup: func(*testing.T, string, string) []string {
				return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://example.com/.extraHeader", "GIT_CONFIG_VALUE_0=X: " + secret}
			},
		},
		{
			// 传输包装在 git 的工作目录中查询：当前仓库的本地配置同样生效
			name: "当前仓库配置的 extraHeader", url: u,
			setup: func(t *testing.T, _, dir string) []string {
				t.Helper()
				runGit(t, dir, "", "init", "-q")
				runGit(t, dir, "", "config", "http.extraHeader", "X: "+secret)
				return nil
			},
			want: gitExtraHeaderKey,
		},
		{
			name: "空值清空了列表", url: u,
			setup: func(*testing.T, string, string) []string {
				return []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=X: " + secret, "GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1="}
			},
		},
		{
			// GIT_CONFIG 只影响 git config 命令：查询时去掉它，否则读不到传输实际生效的全局配置
			name: "GIT_CONFIG 不遮住全局配置", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".gitconfig"), "[http]\n\textraHeader = X: "+secret+"\n")
				empty := filepath.Join(t.TempDir(), "empty")
				mustWrite(t, empty, "")
				return []string{"GIT_CONFIG=" + empty}
			},
			want: gitExtraHeaderKey,
		},
		{
			name: "配置文件损坏是查询失败", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".gitconfig"), "[http\n")
				return nil
			},
			want: "查询失败",
		},
		{
			name: "netrc 中有这个主机", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".netrc"), "machine GitHub.com login x password "+secret+"\n")
				return nil
			},
			want: "netrc 中的主机 github.com",
		},
		{
			name: "netrc 中的主机带引号", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".netrc"), `machine "github.com" login x password y`+"\n")
				return nil
			},
			want: "netrc 中的主机 github.com",
		},
		{
			name: "netrc 中的 default", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".netrc"), "machine example.com login a password b\ndefault login x password "+secret+"\n")
				return nil
			},
			want: "netrc 中的 default",
		},
		{
			name: "netrc 中只有包含这个主机名的其他主机", url: u,
			setup: func(t *testing.T, home, _ string) []string {
				t.Helper()
				mustWrite(t, filepath.Join(home, ".netrc"), "machine api.github.com.example login a password defaults\nmachine notgithub.com login a password b\n")
				return nil
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, home := gitTestEnv(t)
			dir := t.TempDir()
			if c.setup != nil {
				env = append(env, c.setup(t, home, dir)...)
			}
			got := gitAuthConflicts(c.url, dir, env)
			text := joinConflicts(got)
			if strings.Contains(text, secret) {
				t.Errorf("冲突的说明中有值：%s", text)
			}
			switch {
			case c.want == "" && len(got) > 0:
				t.Errorf("不应有冲突：%s", text)
			case c.want != "" && !strings.Contains(text, c.want):
				t.Errorf("冲突 = %q，期望包含 %q", text, c.want)
			}
		})
	}
}

// git config 的查询卡住（例如配置文件在卡住的网络文件系统上）：到期按查询失败处理（拒绝这次传输），包装不会一直挂着。
// 改 PATH（本进程中找 git 的路径）：不能与别的测试并行。
func TestGitAuthQueryTimeout(t *testing.T) {
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "git"), "#!/bin/sh\nexec sleep 60\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env, _ := gitTestEnv(t)
	start := time.Now()
	got := joinConflicts(gitAuthConflictsWithin("https://github.com/o/r.git", t.TempDir(), env, 200*time.Millisecond))
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("查询卡住时等了 %v", d)
	}
	if !strings.Contains(got, "查询失败") {
		t.Errorf("超时应按查询失败处理：%q", got)
	}
}

// netrc 不是普通文件（没有写端的 FIFO：普通的读取会一直卡住）、超过大小上限：不卡住，按查询失败（拒绝这次传输）。
func TestNetrcNotRegularOrTooLarge(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	env, home := gitTestEnv(t)
	f := filepath.Join(home, ".netrc")
	if err := syscall.Mkfifo(f, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := joinConflicts(gitAuthConflicts(u, t.TempDir(), env))
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("FIFO 的 netrc 等了 %v", d)
	}
	if !strings.Contains(got, "查询失败："+f+" 不是普通文件") {
		t.Errorf("FIFO：%q", got)
	}
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, strings.Repeat("x", maxNetrcBytes+1))
	if got := joinConflicts(gitAuthConflicts(u, t.TempDir(), env)); !strings.Contains(got, "超过") {
		t.Errorf("超过上限：%q", got)
	}
}

// netrc 的路径中有一段不是目录（HOME=/dev/null、HOME 是普通文件、NETRC 在普通文件下）时按不存在处理，不是"无法检查"；
// 有效的临时候选、符号链接循环、目录、没有权限仍然拒绝（与 kiln-orca 的 readNetrc 相同）。
func TestNetrcENOTDIRIsMissing(t *testing.T) {
	t.Parallel()
	const u = "https://github.com/o/r.git"
	root := t.TempDir()
	base := func(home string, extra ...string) []string {
		return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, extra...)
	}
	noNetrc := func(name, got string) {
		t.Helper()
		if strings.Contains(got, "netrc") {
			t.Errorf("%s：不应报告 netrc 冲突：%q", name, got)
		}
	}
	// ENOTDIR：路径中有一段不是目录
	noNetrc("HOME=/dev/null", joinConflicts(gitAuthConflicts(u, root, base("/dev/null"))))
	fileHome := filepath.Join(root, "file-home")
	mustWrite(t, fileHome, "x")
	noNetrc("HOME 是普通文件", joinConflicts(gitAuthConflicts(u, root, base(fileHome))))
	noNetrc("NETRC 在普通文件下", joinConflicts(gitAuthConflicts(u, root, base(t.TempDir(), "NETRC="+filepath.Join(fileHome, ".netrc")))))
	// HOME=/dev/null 时，有效的 NETRC 仍然拒绝
	netrc := filepath.Join(root, "netrc")
	mustWrite(t, netrc, "machine github.com\n")
	if got := joinConflicts(gitAuthConflicts(u, root, base("/dev/null", "NETRC="+netrc))); !strings.Contains(got, "netrc 中的主机 github.com") {
		t.Errorf("有效的 NETRC：%q", got)
	}
	// 符号链接循环（ELOOP）：读不了
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if got := joinConflicts(gitAuthConflicts(u, root, base(t.TempDir(), "NETRC="+loop))); !strings.Contains(got, "查询失败") {
		t.Errorf("ELOOP：%q", got)
	}
	// 目录：不是普通文件
	dir := filepath.Join(root, "dir-netrc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := joinConflicts(gitAuthConflicts(u, root, base(t.TempDir(), "NETRC="+dir))); !strings.Contains(got, "不是普通文件") {
		t.Errorf("目录：%q", got)
	}
	// 没有权限：以 root 运行时权限位不起作用，跳过
	if os.Geteuid() != 0 {
		denied := filepath.Join(root, "denied")
		mustWrite(t, denied, "machine github.com\n")
		if err := os.Chmod(denied, 0); err != nil {
			t.Fatal(err)
		}
		if got := joinConflicts(gitAuthConflicts(u, root, base(t.TempDir(), "NETRC="+denied))); !strings.Contains(got, "查询失败") {
			t.Errorf("没有权限：%q", got)
		}
	}
}

// 查询判冲突 ⇔ git 实际发出请求头（真实 git + 回环服务）。与 kiln-orca/test/gitops.test.ts 中"extraHeader 的单值查询与 git
// 实际发出的请求头等价"是同一组用例（保持同步）。conservative：git 把这个值加进了请求头列表（只有零长度的值才清空），但它
// 不是一个能被服务认出的请求头；检查器按原始值保守地判为冲突。
func TestExtraHeaderQueryEquivalence(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-K"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	host := srv.URL
	u := host + "/a/b.git"
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v：%s", err, out)
	}
	glob := filepath.Join(root, "global")
	extra := filepath.Join(root, "only-git-config")
	mustWrite(t, extra, "[http]\n\textraHeader = X-K: viaGIT_CONFIG\n")
	type entry struct{ src, key, val string }
	cases := []struct {
		name         string
		entries      []entry
		env          []string
		conservative bool
	}{
		{name: "个人头 → 纯空白值", entries: []entry{{"global", "http.extraheader", "X-K: g"}, {"local", "http.extraheader", "  "}}},
		{name: "个人头 → 真正的空值", entries: []entry{{"global", "http.extraheader", "X-K: g"}, {"local", "http.extraheader", ""}}},
		{name: "只有纯空白值", entries: []entry{{"env", "http.extraheader", "  "}}, conservative: true},
		{name: "首尾带空白的值", entries: []entry{{"env", "http.extraheader", " X-K: p "}}, conservative: true},
		{name: "通用 → 按地址", entries: []entry{{"global", "http.extraheader", "X-K: g"}, {"local", "http." + host + ".extraheader", "X-K: u"}}},
		{name: "按地址 → 通用", entries: []entry{{"local", "http." + host + ".extraheader", "X-K: u"}, {"local", "http.extraheader", "X-K: g"}}},
		{name: "按地址 → 通用清空", entries: []entry{{"local", "http." + host + ".extraheader", "X-K: u"}, {"local", "http.extraheader", ""}}},
		{name: "通用 → 按地址清空", entries: []entry{{"global", "http.extraheader", "X-K: g"}, {"local", "http." + host + ".extraheader", ""}}},
		{name: "按地址 → 清空 → 通用", entries: []entry{{"local", "http." + host + ".extraheader", "X-K: u1"}, {"local", "http." + host + ".extraheader", ""}, {"local", "http.extraheader", "X-K: g"}}},
		{name: "清空 → 通用", entries: []entry{{"global", "http.extraheader", ""}, {"local", "http.extraheader", "X-K: g2"}}},
		{name: "按地址两个值", entries: []entry{{"local", "http." + host + ".extraheader", "X-K: u1"}, {"local", "http." + host + ".extraheader", "X-K: u2"}}},
		{name: "按路径 → 按主机清空", entries: []entry{{"local", "http." + u + ".extraheader", "X-K: p"}, {"local", "http." + host + ".extraheader", ""}}},
		{name: "按主机清空 → 按路径", entries: []entry{{"local", "http." + host + ".extraheader", ""}, {"local", "http." + u + ".extraheader", "X-K: p"}}},
		{name: "文件 → 环境变量配置清空", entries: []entry{{"local", "http.extraheader", "X-K: f"}, {"env", "http.extraheader", ""}}},
		{name: "环境变量配置", entries: []entry{{"env", "http.extraheader", "X-K: e"}}},
		{name: "只在 GIT_CONFIG 中（远程助手不读）", env: []string{"GIT_CONFIG=" + extra}},
	}
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v：%v：%s", args, err, out)
		}
	}
	for _, c := range cases {
		mustWrite(t, glob, "")
		mustWrite(t, filepath.Join(repo, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
		env := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(root, "nohome"), "GIT_CONFIG_GLOBAL=" + glob, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}, c.env...)
		n := 0
		for _, e := range c.entries {
			switch e.src {
			case "global":
				git(root, "config", "--file", glob, "--add", e.key, e.val)
			case "local":
				git(repo, "config", "--add", e.key, e.val)
			default:
				env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, e.key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, e.val))
				n++
			}
		}
		if n > 0 {
			env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
		}
		mu.Lock()
		seen = nil
		mu.Unlock()
		cmd := exec.Command("git", "ls-remote", u)
		cmd.Env, cmd.Dir = env, repo
		_ = cmd.Run() // 服务回复 404：只看发出的请求头
		mu.Lock()
		sent := slices.ContainsFunc(seen, func(h string) bool { return h != "" })
		mu.Unlock()
		found := strings.Contains(joinConflicts(gitAuthConflicts(u, repo, env)), gitExtraHeaderKey)
		if c.conservative {
			if !found {
				t.Errorf("%s：应保守地判为冲突", c.name)
			}
		} else if found != sent {
			t.Errorf("%s：检查器判冲突 = %v，git 实际发出请求头 = %v", c.name, found, sent)
		}
	}
}

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

func TestCheckout(t *testing.T) {
	t.Parallel()
	repo, entries := newTestRepo(t)
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	// 子模块条目会被跳过
	entries = append(entries, actionkey.TreeEntry{Path: "sub", Mode: actionkey.ModeSubmodule, SHA: strings.Repeat("0", 40)})
	dest := t.TempDir()
	if err := checkout(context.Background(), repo, entries, dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("a.txt = %q, %v", b, err)
	}
	st, err := os.Stat(filepath.Join(dest, "bin", "run.sh"))
	if err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/run.sh：%v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != "a.txt" {
		t.Fatalf("link → %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "sub")); err == nil {
		t.Fatal("子模块不应检出")
	}
}

// 出错时要结束 git 进程，不能因为 git 阻塞在写满的 stdout 管道上而一直等到 job 超时。
func TestCheckoutErrorDoesNotHang(t *testing.T) {
	t.Parallel()
	repo, _ := newTestRepo(t)
	big := runGit(t, repo, strings.Repeat("x", 4<<20), "hash-object", "-w", "--stdin")
	entries := []actionkey.TreeEntry{{Path: "../evil", Mode: actionkey.ModeFile, SHA: big}}
	for range 16 {
		entries = append(entries, actionkey.TreeEntry{Path: "big", Mode: actionkey.ModeFile, SHA: big})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if err := checkout(ctx, repo, entries, t.TempDir()); err == nil {
		t.Fatal("期望路径越界的错误")
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("出错后用了 %s 才返回", el)
	}
}

// 恶意 tree：符号链接 a → 外部目录，之后再写 a/x，不能写到外面。
func TestCheckoutSymlinkEscape(t *testing.T) {
	t.Parallel()
	repo, _ := newTestRepo(t)
	outside := t.TempDir()
	linkBlob := runGit(t, repo, outside, "hash-object", "-w", "--stdin")
	fileBlob := runGit(t, repo, "pwned", "hash-object", "-w", "--stdin")
	entries := []actionkey.TreeEntry{
		{Path: "a", Mode: actionkey.ModeSymlink, SHA: linkBlob},
		{Path: "a/x", Mode: actionkey.ModeFile, SHA: fileBlob},
	}
	if err := checkout(context.Background(), repo, entries, t.TempDir()); err == nil {
		t.Fatal("期望经由符号链接写出工作区时报错")
	}
	if got, err := os.ReadDir(outside); err != nil || len(got) != 0 {
		t.Fatalf("外部目录被写入：%v %v", got, err)
	}
}

// gitEnvFrom 彻底隔离宿主机：只保留进程环境中的普通系统变量（PATH、代理、CA），按完整变量名去掉用户配置、模板、netrc、
// 配置注入、外层包装、认证 / 协议 / TLS / 调试覆盖与仓库上下文；再固定注入 GIT_CONFIG_GLOBAL=/dev/null、
// GIT_CONFIG_NOSYSTEM=1、HOME=/dev/null、GIT_TERMINAL_PROMPT=0 与固定的 6 项配置（无 token），有 token 时追加第 7 项。
func TestGitEnvFrom(t *testing.T) {
	t.Parallel()
	base := []string{
		"PATH=/usr/bin", "HTTPS_PROXY=http://proxy", "http_proxy=http://proxy2", "NO_PROXY=localhost",
		"SSL_CERT_FILE=/ca.pem", "SSL_CERT_DIR=/cas", "GIT_SSL_CAINFO=/gitca.pem", "GIT_SSL_CAPATH=/gitcapath", "LC_ALL=C",
		// 宿主机用户配置、模板与 netrc
		"HOME=/home/u", "XDG_CONFIG_HOME=/home/u/.config", "NETRC=/home/u/.netrc",
		"GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "GIT_CONFIG_SYSTEM=/etc/gitconfig", "GIT_CONFIG_NOSYSTEM=0", "GIT_TEMPLATE_DIR=/tmpl",
		// 命令行 / 环境变量形式的配置注入
		"GIT_CONFIG=/f", "GIT_CONFIG_PARAMETERS=x", "GIT_CONFIG_COUNT=9", "GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0",
		"GIT_CONFIG_KEY_9=k9", "GIT_CONFIG_VALUE_9=v9",
		// 外层包装与凭据通道的私有变量
		"GIT_EXEC_PATH=/outer", "KILN_ORCA_GIT_OP=op", "KILN_GIT_OP=op2", "KILN_GIT_OP_ERROR=e", "KILN_GIT_SOCKET=/s", "KILN_GIT_EXEC_REAL=/r", "KILN_GIT_CONFIG_N=4",
		"KILN_GIT_CONFIG_KEY_0=k", "KILN_GIT_CONFIG_VALUE_0=v",
		// 认证、协议、TLS 与调试覆盖
		"GIT_TERMINAL_PROMPT=1", "GIT_ASKPASS=/ap", "SSH_ASKPASS=/sap", "GIT_SSH=/ssh", "GIT_SSH_COMMAND=ssh -v", "GIT_SSL_NO_VERIFY=1",
		"GIT_ALLOW_PROTOCOL=file", "GIT_PROTOCOL_FROM_USER=1", "GIT_TRACE=1", "GIT_TRACE_CURL=1", "GIT_TRACE_PACKET=1", "GIT_CURL_VERBOSE=1",
		"GIT_TRACE_REDACT=0", "GIT_TRACE2=1", "GIT_TRACE2_EVENT=/e", "GIT_TRACE2_PERF=/p",
		// 仓库上下文
		"GIT_DIR=/g", "GIT_WORK_TREE=/w", "GIT_COMMON_DIR=/c", "GIT_INDEX_FILE=/i", "GIT_OBJECT_DIRECTORY=/o", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/ao",
	}
	original := slices.Clone(base)
	kept := []string{
		"PATH=/usr/bin", "HTTPS_PROXY=http://proxy", "http_proxy=http://proxy2", "NO_PROXY=localhost",
		"SSL_CERT_FILE=/ca.pem", "SSL_CERT_DIR=/cas", "GIT_SSL_CAINFO=/gitca.pem", "GIT_SSL_CAPATH=/gitcapath", "LC_ALL=C",
	}
	fixed := func(extra ...[2]string) []string {
		env := []string{
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=/dev/null", "GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=" + strconv.Itoa(6+len(extra)),
		}
		config := append([][2]string{
			{"credential.helper", ""},
			{"core.askPass", ""},
			{"protocol.ssh.allow", "never"},
			{"protocol.git.allow", "never"},
			{"protocol.ext.allow", "never"},
			{"http.sslVerify", "true"},
		}, extra...)
		for i, c := range config {
			env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+c[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+c[1])
		}
		return env
	}
	want := append(slices.Clone(kept), fixed()...)
	got := gitEnvFrom(base, "")
	if !slices.Equal(got, want) {
		t.Errorf("gitEnvFrom = %q\n期望 %q", got, want)
	}
	// 不修改入参
	if !slices.Equal(base, original) {
		t.Errorf("gitEnvFrom 修改了入参：%q\n原值 %q", base, original)
	}
	// 重设的键没有重复（POSIX 的 getenv 取第一个匹配）
	seen := make(map[string]bool, len(got))
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		if seen[name] {
			t.Errorf("环境变量 %s 重复", name)
		}
		seen[name] = true
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	withToken := append(slices.Clone(kept), fixed([2]string{"http.extraHeader", "Authorization: Basic " + auth})...)
	if got := gitEnvFrom(base, "tok"); !slices.Equal(got, withToken) {
		t.Errorf("带 token：gitEnvFrom = %q\n期望 %q", got, withToken)
	}
}

// gitSetEnv 返回在 base 上替换（而不是重复）kv 中各个变量的环境：POSIX 的 getenv 取第一个匹配，重复的键不起作用。
func gitSetEnv(base []string, kv ...string) []string {
	names := make(map[string]bool, len(kv))
	for _, e := range kv {
		if n, _, ok := strings.Cut(e, "="); ok {
			names[n] = true
		}
	}
	out := make([]string, 0, len(base)+len(kv))
	for _, e := range base {
		if n, _, _ := strings.Cut(e, "="); !names[n] {
			out = append(out, e)
		}
	}
	return append(out, kv...)
}

// TestGitHelperProcess 是 TestGitEnvDropsOuterWrapper 的子进程：用 Runner 自己的 git 访问远端（见 gitEnv）。
func TestGitHelperProcess(t *testing.T) {
	if os.Getenv("KILN_TEST_GIT_HELPER") != "1" {
		return
	}
	_, err := git(context.Background(), os.Getenv("KILN_TEST_REPO"), os.Getenv("KILN_TEST_TOKEN"), "ls-remote", os.Getenv("KILN_TEST_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestCheckoutHelperProcess 是 TestCheckoutIgnoresInheritedGitEnv 的子进程：在（被污染的）父进程环境中直接调用 checkout。
func TestCheckoutHelperProcess(t *testing.T) {
	repo := os.Getenv("KILN_TEST_CHECKOUT_REPO")
	if repo == "" {
		return
	}
	var entries []actionkey.TreeEntry
	b, err := os.ReadFile(os.Getenv("KILN_TEST_CHECKOUT_ENTRIES"))
	if err == nil {
		err = json.Unmarshal(b, &entries)
	}
	if err == nil {
		err = checkout(context.Background(), repo, entries, os.Getenv("KILN_TEST_CHECKOUT_DEST"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// checkout 的 git cat-file 不能继承外层包装 / 仓库上下文 / 坏的配置注入：checkout 用 gitEnv("") 隔离环境，否则残留的
// GIT_DIR、GIT_OBJECT_DIRECTORY、git 会拒绝的 GIT_CONFIG_COUNT 会让它读不到对象。用子进程注入这些污染（并行测试中不能
// 改父进程的环境），确认 checkout 仍能正确检出 bare 仓库中的文件。
func TestCheckoutIgnoresInheritedGitEnv(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	// 与 fetchSource 的镜像相同：在 bare 仓库中按对象名读取（checkout 的 cmd.Dir）。
	work := t.TempDir()
	runGit(t, work, "", "init", "-q")
	mustWrite(t, filepath.Join(work, "a.txt"), "hello\n")
	mustWrite(t, filepath.Join(work, "sub", "b.txt"), "nested\n")
	runGit(t, work, "", "add", "-A")
	runGit(t, work, "", "commit", "-q", "-m", "init")
	bare := t.TempDir()
	runGit(t, bare, "", "init", "--bare", "-q")
	runGit(t, work, "", "push", "-q", bare, "HEAD:refs/heads/master")
	entries := parseLsTree([]byte(runGit(t, bare, "", "ls-tree", "-r", "-z", "--full-tree", "refs/heads/master")))
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	entriesFile := filepath.Join(t.TempDir(), "entries.json")
	if err := os.WriteFile(entriesFile, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	// 污染：指向不存在的仓库 / 对象目录，以及 git 会拒绝的 GIT_CONFIG_COUNT；再加上外层的配置注入与 askpass。
	cmd := exec.Command(os.Args[0], "-test.run=TestCheckoutHelperProcess")
	cmd.Env = gitSetEnv(os.Environ(),
		"KILN_TEST_CHECKOUT_REPO="+bare, "KILN_TEST_CHECKOUT_DEST="+dest, "KILN_TEST_CHECKOUT_ENTRIES="+entriesFile,
		"GIT_DIR="+filepath.Join(t.TempDir(), "nope"),
		"GIT_OBJECT_DIRECTORY="+filepath.Join(t.TempDir(), "nope", "objects"),
		"GIT_CONFIG_COUNT=bad",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=X-Evil: 1",
		"GIT_ASKPASS=/nonexistent",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("checkout 在污染的环境中失败：%v：%s", err, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("a.txt = %q, %v（git：%s）", b, err, stderr.String())
	}
	if b, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt")); err != nil || string(b) != "nested\n" {
		t.Fatalf("sub/b.txt = %q, %v（git：%s）", b, err, stderr.String())
	}
}

// 外层 kiln-orca task exec 的环境（GIT_EXEC_PATH 指向一个会被记录并退出 99 的传输包装、环境变量配置、操作 id、仓库上下文）
// 不会继承到 Runner 自己的 git 中：请求用 Runner 签发的 token 直接到服务，外层的包装零调用。
func TestGitEnvDropsOuterWrapper(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound) // ls-remote 对 404 失败：只看请求有没有发出、带了什么凭据
	}))
	defer srv.Close()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", "--bare", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init：%v：%s", err, out)
	}
	sentinel := t.TempDir()
	marker := filepath.Join(t.TempDir(), "wrapper-called")
	for _, name := range []string{"git-remote-http", "git-remote-https"} {
		mustWrite(t, filepath.Join(sentinel, name), "#!/bin/sh\ntouch '"+marker+"'\nexit 99\n")
		if err := os.Chmod(filepath.Join(sentinel, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestGitHelperProcess")
	cmd.Env = gitSetEnv(os.Environ(),
		"KILN_TEST_GIT_HELPER=1", "KILN_TEST_REPO="+repo, "KILN_TEST_URL="+srv.URL+"/a/b.git", "KILN_TEST_TOKEN=tok",
		"GIT_EXEC_PATH="+sentinel,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=X-Evil: 1",
		"KILN_ORCA_GIT_OP=outer", "GIT_DIR="+filepath.Join(t.TempDir(), "nope"), "GIT_ASKPASS=/nonexistent",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // 服务回 404，ls-remote 失败是预期的
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("外层的传输包装被调用了：%s", stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	if len(auth) == 0 {
		t.Fatalf("请求没有到达服务（git 的输出：%s）", stderr.String())
	}
	for _, a := range auth {
		if a != want {
			t.Errorf("请求的 Authorization = %q，期望签发的 token（%q）", a, want)
		}
	}
}

// ---- 宿主机隔离的模块测试 ----
//
// 这些测试在子进程中注入模拟的宿主环境（HOME、全局 / 系统配置、模板、netrc、外层包装），绝不影响运行测试的机器上的真实文件与
// 配置；子进程在受控环境中调用 source.go 的 fetchSource / git，验证宿主机的这些东西一律不生效。

// sourceHelper 让测试函数只在作为子进程（runSourceHelper 启动）时运行。
func sourceHelper(t *testing.T, name string) {
	t.Helper()
	if os.Getenv("KILN_TEST_SOURCE_HELPER") != name {
		t.Skip("只作为子进程运行")
	}
}

// sourceHelperEnv 让子进程从受控环境开始：只保留 PATH（找 git）和 TMPDIR（t.TempDir），再按需注入模拟的宿主环境。
func sourceHelperEnv(t *testing.T) {
	t.Helper()
	keep := map[string]string{}
	for _, k := range []string{"PATH", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			keep[k] = v
		}
	}
	os.Clearenv()
	for k, v := range keep {
		t.Setenv(k, v)
	}
}

// runSourceHelper 在子进程中运行 testName（用 KILN_TEST_SOURCE_HELPER 标记），返回它的输出与错误。
func runSourceHelper(t *testing.T, testName string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = gitSetEnv(os.Environ(), "KILN_TEST_SOURCE_HELPER="+testName)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// sourceHelperGit 是子进程中的 git HTTP 服务：每个请求的 Authorization 都记下来，accept 拒绝时回 401 challenge。
type sourceHelperGit struct {
	srv    *httptest.Server
	root   string
	accept func(user, pass string) bool

	mu   sync.Mutex
	auth []string
}

func newSourceHelperGit(t *testing.T, accept func(user, pass string) bool) *sourceHelperGit {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("需要 git")
	}
	g := &sourceHelperGit{root: t.TempDir(), accept: accept}
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + g.root, "GIT_HTTP_EXPORT_ALL=1"}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		user, pass, ok := r.BasicAuth()
		if !ok || g.accept == nil || !g.accept(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// url 返回仓库 repo 的地址。
func (g *sourceHelperGit) url(repo string) string { return g.srv.URL + "/" + repo + ".git" }

// authHeaders 返回收到的全部 Authorization。
func (g *sourceHelperGit) authHeaders() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.auth)
}

// addRepo 在 g.root 下建裸仓库 repo（一个提交），返回 commit sha。
func (g *sourceHelperGit) addRepo(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "", "init", "-q", "-b", "main")
	sha := commitFiles(t, work, files)
	bare := filepath.Join(g.root, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "", "clone", "-q", "--bare", work, bare)
	// fetchSource 按 sha 拉取
	runGit(t, bare, "", "config", "uploadpack.allowAnySHA1InWant", "true")
	return sha
}

// sourceHelperLog 返回 fetchSource 用的、不真正上传的日志。
func sourceHelperLog(t *testing.T) *runnerkit.LogShipper {
	t.Helper()
	log := runnerkit.NewLogShipper(func(_ context.Context, offset int64, data []byte) (int64, error) {
		return offset + int64(len(data)), nil
	})
	t.Cleanup(log.Close)
	return log
}

// TestSourceIsolatesHostGitEnvHelper 是 TestSourceIsolatesHostGitEnv 的子进程：在满是宿主配置、模板、netrc 与外层包装的
// 环境中运行完整的 fetchSource（新建 bare 镜像、缓存缺失的 fetch、检出、第二次缓存命中）。
func TestSourceIsolatesHostGitEnvHelper(t *testing.T) {
	sourceHelper(t, "TestSourceIsolatesHostGitEnvHelper")
	sourceHelperEnv(t)
	const token = "ghs_source_isolation"
	g := newSourceHelperGit(t, func(_, pass string) bool { return pass == token })
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n", "sub/b.txt": "nested\n"})

	// 模拟宿主机：HOME、XDG、GIT_CONFIG_GLOBAL、GIT_CONFIG_SYSTEM、模板目录与外层包装。每一份配置都含
	// insteadOf（会把 fetch 改写到连不上的地址）、个人请求头、写痕迹文件的 credential helper 与 askpass。
	root := t.TempDir()
	home := filepath.Join(root, "home")
	helperTrace := filepath.Join(root, "helper-called")
	askpassTrace := filepath.Join(root, "askpass-called")
	malicious := func() string {
		return fmt.Sprintf("[url \"http://127.0.0.1:1/\"]\n\tinsteadOf = %s/\n[http]\n\textraHeader = X-Personal: leak\n[credential]\n\thelper = \"!f() { echo called >> '%s'; echo username=evil; echo password=evil; }; f\"\n[core]\n\taskPass = \"!f() { echo called >> '%s'; }; f\"\n", g.srv.URL, helperTrace, askpassTrace)
	}
	homeConfig := filepath.Join(home, ".gitconfig")
	xdgConfig := filepath.Join(root, "xdg", "git", "config")
	globalConfig := filepath.Join(root, "global.gitconfig")
	systemConfig := filepath.Join(root, "system.gitconfig")
	netrc := filepath.Join(home, ".netrc")
	tmpl := filepath.Join(root, "template")
	for _, p := range []string{homeConfig, xdgConfig, globalConfig, systemConfig} {
		mustWrite(t, p, malicious())
	}
	mustWrite(t, netrc, "machine 127.0.0.1 login personal password personal-secret\n")
	mustWrite(t, filepath.Join(tmpl, "config"), "[evil]\n\ttemplate = 1\n")
	before := map[string][]byte{}
	for _, p := range []string{homeConfig, xdgConfig, globalConfig, systemConfig, netrc} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = b
	}
	for k, v := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(root, "xdg"), "NETRC": netrc,
		"GIT_CONFIG_GLOBAL": globalConfig, "GIT_CONFIG_SYSTEM": systemConfig, "GIT_CONFIG_NOSYSTEM": "0",
		"GIT_TEMPLATE_DIR": tmpl, "GIT_EXEC_PATH": "/nonexistent/outer-git-exec",
		// git 会拒绝的配置注入与高编号项
		"GIT_CONFIG_COUNT": "bad", "GIT_CONFIG_KEY_9": "img.stale", "GIT_CONFIG_VALUE_9": "9",
	} {
		t.Setenv(k, v)
	}

	r := &Runner{cfg: &Config{Dir: t.TempDir()}}
	log := sourceHelperLog(t)
	issued, revoked := 0, 0
	tokens := gitTokensFunc{
		issue: func(context.Context, string, string) (string, time.Time, error) {
			issued++
			return token, time.Now().Add(time.Hour), nil
		},
		revoke: func(context.Context, string, string) error { revoked++; return nil },
	}
	src := &runnerv1.Source{RepoId: 1, Repository: "o/r", CloneUrl: g.url("o/r"), Sha: sha}
	dest := t.TempDir()
	if err := r.fetchSource(t.Context(), src, tokens, dest, log); err != nil {
		t.Fatalf("fetchSource 在宿主环境的影响下失败：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("a.txt = %q，%v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt")); err != nil || string(b) != "nested\n" {
		t.Fatalf("sub/b.txt = %q，%v", b, err)
	}
	// 请求到达服务，Authorization 恰好且仅为 Server 签发的 token
	auth := g.authHeaders()
	if len(auth) == 0 {
		t.Fatal("请求没有到达服务")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	for _, a := range auth {
		if a != want {
			t.Errorf("请求的 Authorization = %q，期望只是 Server 的 token（%q）", a, want)
		}
	}
	// 宿主配置里的 helper / askpass 与外层包装零调用
	for _, p := range []string{helperTrace, askpassTrace} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("宿主的程序被调用了：%s", p)
		}
	}
	// 模板没有进入新建的 bare 镜像：`init --bare --template=` 既不用环境的模板目录，也不拷默认模板里的 config / hooks
	mirror := r.cfg.path(gitDirName, "1.git")
	cfg, err := os.ReadFile(filepath.Join(mirror, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "template") || strings.Contains(string(cfg), "evil") {
		t.Errorf("模板被拷进了 bare 镜像：\n%s", cfg)
	}
	for _, name := range []string{"hooks", "description", "info"} {
		if _, err := os.Stat(filepath.Join(mirror, name)); err == nil {
			t.Errorf("默认模板的 %s 被拷进了 bare 镜像", name)
		}
	}
	// 第二次命中缓存：零签发
	if err := r.fetchSource(t.Context(), src, tokens, t.TempDir(), log); err != nil {
		t.Fatalf("第二次 fetchSource：%v", err)
	}
	if issued != 1 || revoked != 1 {
		t.Errorf("签发 %d 次、吊销 %d 次，期望各 1 次", issued, revoked)
	}
	// 宿主的配置文件与 netrc 字节没有被修改
	for p, want := range before {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("宿主的文件被修改了：%s（%v）", p, err)
		}
	}
}

func TestSourceIsolatesHostGitEnv(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	_, stderr, err := runSourceHelper(t, "TestSourceIsolatesHostGitEnvHelper")
	if err != nil {
		t.Fatalf("子进程失败：%v\n%s", err, stderr)
	}
}

// TestSourceNetrcIsolationHelper 是 TestSourceNetrcIsolation 的子进程：先用裸 git（继承 HOME / NETRC）证明宿主 netrc 的
// 凭据能通过 401，再用 source.go 的 git 访问同一服务（同一污染环境），确认认证失败且服务没有收到 netrc 中的凭据。
func TestSourceNetrcIsolationHelper(t *testing.T) {
	sourceHelper(t, "TestSourceNetrcIsolationHelper")
	sourceHelperEnv(t)
	g := newSourceHelperGit(t, func(user, pass string) bool { return user == "user" && pass == "netrc-secret" })
	g.addRepo(t, "o/r", map[string]string{"a.txt": "a\n"})
	home := t.TempDir()
	netrc := filepath.Join(home, ".netrc")
	mustWrite(t, netrc, "machine 127.0.0.1 login user password netrc-secret\n")
	t.Setenv("HOME", home)
	t.Setenv("NETRC", netrc)
	url := g.url("o/r")
	// 裸 git 继承 HOME / NETRC：宿主 netrc 的凭据通过 401，ls-remote 成功
	raw := exec.Command("git", "ls-remote", url)
	raw.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := raw.CombinedOutput(); err != nil {
		// 子进程的 t.Skip 不会让父进程看出来（正常输出里没有它），用一行标记告诉父进程跳过
		fmt.Printf("SKIP: 这个平台的 libcurl 不读 netrc：%v：%s\n", err, out)
		return
	}
	personal := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:netrc-secret"))
	auth := g.authHeaders()
	if !slices.Contains(auth, personal) {
		t.Fatalf("裸 git 没有用上宿主 netrc 的凭据：%q", auth)
	}
	// source.go 的 git：无 token，隔离了 HOME / NETRC，认证失败，服务收不到个人凭据
	rawAuth := len(auth)
	if _, err := git(t.Context(), "", "", "ls-remote", url); err == nil {
		t.Fatal("隔离后仍然用宿主 netrc 认证成功")
	}
	for _, a := range g.authHeaders()[rawAuth:] {
		if a == personal {
			t.Fatal("source.go 的 git 把宿主 netrc 的凭据发给了服务")
		}
	}
}

func TestSourceNetrcIsolation(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	out, stderr, err := runSourceHelper(t, "TestSourceNetrcIsolationHelper")
	if err != nil {
		t.Fatalf("子进程失败：%v\n%s", err, stderr)
	}
	if strings.Contains(out, "SKIP:") {
		t.Skip(strings.TrimSpace(out))
	}
}

// TestSourceTLSAndProtocolHelper 是 TestSourceTLSAndProtocol 的子进程。
func TestSourceTLSAndProtocolHelper(t *testing.T) {
	sourceHelper(t, "TestSourceTLSAndProtocolHelper")
	sourceHelperEnv(t)
	// 宿主环境试图跳过证书校验
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	mustWrite(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	url := srv.URL + "/o/r.git"
	// 自签证书 + 宿主试图关闭校验：仍然在证书校验失败（http.sslVerify=true，GIT_SSL_NO_VERIFY 被清掉）
	if _, err := git(t.Context(), "", "", "ls-remote", url); err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("自签证书没有被拒绝：%v", err)
	}
	// 进程级 CA 环境变量保留：证书校验通过，失败变成 HTTP 的 404
	t.Setenv("GIT_SSL_CAINFO", ca)
	if _, err := git(t.Context(), "", "", "ls-remote", url); err == nil || strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("保留的 CA 环境变量没有生效：%v", err)
	}
	// ssh 传输被禁用，伪 GIT_SSH_COMMAND 零调用
	marker := filepath.Join(t.TempDir(), "ssh-called")
	fakeSSH := filepath.Join(t.TempDir(), "fake-ssh")
	mustWrite(t, fakeSSH, "#!/bin/sh\ntouch '"+marker+"'\nexit 0\n")
	if err := os.Chmod(fakeSSH, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)
	_, err := git(t.Context(), "", "", "ls-remote", "ssh://127.0.0.1:1/o/r")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("ssh 传输没有被禁用：%v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("GIT_SSH_COMMAND 被调用了")
	}
}

func TestSourceTLSAndProtocol(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	_, stderr, err := runSourceHelper(t, "TestSourceTLSAndProtocolHelper")
	if err != nil {
		t.Fatalf("子进程失败：%v\n%s", err, stderr)
	}
}

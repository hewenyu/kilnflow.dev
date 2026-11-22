package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 源码获取（PLAN #8.4）：每个仓库一个 bare 镜像，按 sha 浅拉取；只把 inputs 选中的文件写入工作区，没有 .git。Runner 自己
// 执行的 git 与宿主机的 git 配置、系统配置、模板和 netrc 隔离（gitEnvFrom），只使用进程环境中的 PATH、代理与 CA，以及 Server
// 为这次操作签发的 token。

// catFileBufferSize 是读取 git cat-file 输出的缓冲区大小。
const catFileBufferSize = 1 << 20

// gitStopGrace 是取消 git 命令时，从向它的进程组发 SIGTERM 到不再等它的输出管道（强制结束 git 本身）的时间。
const gitStopGrace = 5 * time.Second

// gitEnvDroppedKeys 是 gitEnvFrom 从 base 中按完整变量名去掉的键：宿主机用户配置、模板与 netrc，命令行 / 环境变量形式的
// 配置注入，外层包装与凭据通道的私有变量，认证、协议、TLS 与调试覆盖，以及仓库上下文。
var gitEnvDroppedKeys = []string{
	// 宿主机用户配置、模板与 netrc
	"HOME", "XDG_CONFIG_HOME", "NETRC", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_TEMPLATE_DIR",
	// 命令行 / 环境变量形式的配置注入
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	// 外层包装与凭据通道的私有变量（外层 kiln-orca task exec 设置，继承下来会递归套用外层的传输包装）
	"GIT_EXEC_PATH", "KILN_ORCA_GIT_OP", "KILN_GIT_OP", "KILN_GIT_OP_ERROR", "KILN_GIT_SOCKET", "KILN_GIT_EXEC_REAL", "KILN_GIT_CONFIG_N",
	// 认证、协议、TLS 与调试覆盖
	"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSL_NO_VERIFY", "GIT_ALLOW_PROTOCOL",
	"GIT_PROTOCOL_FROM_USER", "GIT_TRACE", "GIT_TRACE_CURL", "GIT_TRACE_PACKET", "GIT_CURL_VERBOSE", "GIT_TRACE_REDACT",
	"GIT_TRACE2", "GIT_TRACE2_EVENT", "GIT_TRACE2_PERF",
	// 仓库上下文
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
}

// gitEnvDroppedPrefixes 是按前缀去掉的键：编号不定的环境变量配置（GIT_CONFIG_KEY_<n> 等）、Kiln 交给 oci 启动脚本的暂存项。
var gitEnvDroppedPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "KILN_GIT_CONFIG_KEY_", "KILN_GIT_CONFIG_VALUE_"}

// gitEnvDropped 判断环境变量名是否要从 gitEnvFrom 的 base 中去掉。
func gitEnvDropped(name string) bool {
	if slices.Contains(gitEnvDroppedKeys, name) {
		return true
	}
	return slices.ContainsFunc(gitEnvDroppedPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// gitEnvForcedConfig 是固定注入给 git 的配置（GIT_CONFIG_COUNT）：清空凭据 helper 与 askpass，禁用 SSH、git 和 ext 传输
// （file 协议保留，供本地 bare fixture），强制校验证书。
var gitEnvForcedConfig = [][2]string{
	{"credential.helper", ""},
	{"core.askPass", ""},
	{"protocol.ssh.allow", "never"},
	{"protocol.git.allow", "never"},
	{"protocol.ext.allow", "never"},
	{"http.sslVerify", "true"},
}

// gitEnvFrom 返回在 base 上彻底隔离后的 git 环境：只保留进程环境中的普通系统变量（PATH、代理、CA 证书等），按
// gitEnvDropped 去掉宿主机用户配置、模板、netrc、配置注入、外层包装与仓库上下文，再固定注入 GIT_CONFIG_GLOBAL=/dev/null、
// GIT_CONFIG_NOSYSTEM=1、HOME=/dev/null、GIT_TERMINAL_PROMPT=0 与 gitEnvForcedConfig；有 token 时再追加限定在任务仓库
// 地址上的请求头（GIT_CONFIG_COUNT）。不修改入参 base。
//
// 缓存目录中已有 bare 镜像的本地 config 仍会被读取，那是受控缓存的一部分（PLAN #8.4）。
func gitEnvFrom(base []string, token string) []string {
	env := make([]string, 0, len(base)+8+2*len(gitEnvForcedConfig))
	for _, kv := range base {
		if name, _, _ := strings.Cut(kv, "="); gitEnvDropped(name) {
			continue
		}
		env = append(env, kv)
	}
	config := gitEnvForcedConfig
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		config = append(slices.Clone(config), [2]string{"http.extraHeader", "Authorization: Basic " + auth})
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(config)),
	)
	for i, c := range config {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+c[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+c[1])
	}
	return env
}

func gitEnv(token string) []string {
	return gitEnvFrom(os.Environ(), token)
}

// git 在 dir 中执行 git 命令。git 和它启动的远程助手在自己的进程组中：取消（含超时）时向整个进程组发 SIGTERM，远程助手随之
// 结束，不会因为它还拿着输出管道而让 Output 一直等下去；gitStopGrace 后仍没结束的不再等（只强制结束 git 本身）。
//
// 进程组只在 cmd.Cancel 中结束：这时 os/exec 一般还没有回收 git，进程组号仍属于它（git 恰好同时自己退出、已被回收的极小
// 窗口除外）。正常退出之后不再向进程组发信号——git 已被回收，它的进程号可能已经被别的进程用了；因此正常退出时留下的后台
// 进程（git 自己不会留下）不清理。
func git(ctx context.Context, dir, token string, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, dir, token, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s：%w：%s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitCommand 返回在 dir 中执行的 git 命令：自己的进程组，取消时结束整个组。
func gitCommand(ctx context.Context, dir, token string, args ...string) *exec.Cmd {
	cmd := groupCommand(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(token)
	return cmd
}

// groupCommand 构造在自己的进程组中运行、输出等待有界的命令（Runner 自己执行、捕获输出的 git）：ctx 结束时向整个进程组
// 发 SIGTERM；WaitDelay 限定 ctx 结束后、以及直接子进程正常退出而后代仍持有输出管道时的等待（到期关闭管道返回）。
// 进程已经被 Wait 回收之后 os/exec 不再调用 Cancel，所以正常退出后不会按旧的 PID 发信号。
func groupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	//nolint:gosec // G702：调用方给出的程序和参数（git 与它的参数、step 的 PATH 中的 git），不经过 shell
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 进程组可能已经全部退出，ESRCH 可以忽略
	cmd.Cancel = func() error { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); return nil }
	cmd.WaitDelay = gitStopGrace
	return cmd
}

// commitInMirror 判断 bare 镜像中有没有 sha 这个 commit。用 git cat-file --batch-check，只认这个对象那一行：
// "<名字> missing" 是确定没有，"<sha> commit <大小>" 是有；命令失败（例如镜像的配置损坏，git 以 128 退出，对象其实在）、
// 没有读到完整的一行、其他状态都返回错误（本地镜像检查失败），不能当作"没有"去签发凭据。
func commitInMirror(ctx context.Context, mirror, sha string) (bool, error) {
	name := sha + "^{commit}"
	cmd := gitCommand(ctx, mirror, "", "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(name + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git cat-file --batch-check：%w：%s", err, strings.TrimSpace(stderr.String()))
	}
	line, ok := strings.CutSuffix(string(out), "\n")
	switch f := strings.Fields(line); {
	case !ok || strings.Contains(line, "\n"):
		return false, fmt.Errorf("git cat-file --batch-check 的输出不是一行：%q", string(out))
	case line == name+" missing":
		return false, nil
	case len(f) == 3 && f[1] == "commit":
		return true, nil
	default:
		return false, fmt.Errorf("git cat-file --batch-check 的结果无法识别：%q", line)
	}
}

// fetchSource 把 commit 拉到 bare 镜像，并把选中的文件检出到 dest。
// 缓存缺失时（在仓库锁内确认 bare 镜像中没有这个 commit 之后）才为这一次 fetch 签发任务仓库的只读 token，fetch 进程退出后
// 吊销；命中缓存时不签发（PLAN #8.4）。
func (r *Runner) fetchSource(ctx context.Context, src *runnerv1.Source, tokens gitTokens, dest string, log *runnerkit.LogShipper) error {
	mu := r.repoLock(src.RepoId)
	mu.Lock()
	defer mu.Unlock()
	mirror := r.cfg.path(gitDirName, strconv.FormatInt(src.RepoId, 10)+".git")
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		if err := os.MkdirAll(mirror, 0o755); err != nil {
			return err
		}
		if _, err := git(ctx, mirror, "", "init", "--bare", "--template=", "-q"); err != nil {
			return err
		}
	}
	runnerkit.Touch(mirror) // 记录最后使用的时间（gc.go）
	has, err := commitInMirror(ctx, mirror, src.Sha)
	if err != nil {
		// 被取消而失败的不是镜像的问题
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 查不了不是缓存缺失：不签发，job 以本地镜像检查失败结束
		return fmt.Errorf("检查 bare 镜像中有没有 %s 失败（本地镜像的问题，没有申请凭据）：%w", shorten.Hash(src.Sha), err)
	}
	if !has {
		log.Printf("拉取 %s@%s", src.Repository, shorten.Hash(src.Sha))
		if err := fetchCommit(ctx, mirror, src, tokens, log); err != nil {
			return err
		}
	}
	out, err := git(ctx, mirror, "", "ls-tree", "-r", "-z", "--full-tree", src.Sha)
	if err != nil {
		return err
	}
	m := workflow.NewInputMatcher(src.Inputs)
	var sel []actionkey.TreeEntry
	for _, e := range parseLsTree(out) {
		if m.Match(e.Path) {
			sel = append(sel, e)
		}
	}
	if src.InputDigest != "" {
		if d := actionkey.InputDigest(sel); d != src.InputDigest {
			return fmt.Errorf("输入摘要不一致：本地 %s，Server %s", d, src.InputDigest)
		}
	}
	return checkout(ctx, mirror, sel, dest)
}

// fetchCommit 为这一次 fetch 开一个操作、签发任务仓库的只读 token，fetch 进程退出后（成功、失败、取消）关闭操作并吊销。
func fetchCommit(ctx context.Context, mirror string, src *runnerv1.Source, tokens gitTokens, log *runnerkit.LogShipper) error {
	op, l := newGitOpID(), gitOpLog{log: log}
	// 关闭、吊销不随 job 取消
	rctx := context.WithoutCancel(ctx)
	tok, exp, err := tokens.issueGitToken(ctx, op, src.Repository)
	if err != nil {
		l.issueFailed(op, src.Repository, err)
		// 没有收到响应、请求被取消等：Server 上可能有这个操作（甚至已经签出 token），按原 op 关闭
		if !gitOpSettled(err) {
			l.revoke(rctx, tokens, op, src.Repository, "", time.Time{})
		}
		return fmt.Errorf("签发拉取源码的 token：%w", err)
	}
	l.issued(op, src.Repository, tok, exp)
	defer l.revoke(rctx, tokens, op, src.Repository, tok, exp)
	_, err = git(ctx, mirror, tok, "fetch", "-q", "--no-tags", "--depth=1", src.CloneUrl, src.Sha)
	return err
}

// parseLsTree 解析 git ls-tree -r -z 的输出（"mode type sha\tpath\0"）。
func parseLsTree(out []byte) []actionkey.TreeEntry {
	var all []actionkey.TreeEntry
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			continue
		}
		all = append(all, actionkey.TreeEntry{Path: string(path), Mode: f[0], SHA: f[2]})
	}
	return all
}

// checkout 用 git cat-file --batch 把选中的条目写到 dest（子模块在 v0 中不检出）。
//
// 写入都通过 os.Root 进行：即使 tree 中的路径与符号链接组合起来指向 dest 之外，也不会写出去。
func checkout(ctx context.Context, mirror string, entries []actionkey.TreeEntry, dest string) error {
	var blobs []actionkey.TreeEntry
	for _, e := range entries {
		if e.Mode != actionkey.ModeSubmodule {
			blobs = append(blobs, e)
		}
	}
	if len(blobs) == 0 {
		return nil
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	// 出错返回时取消 ctx，结束 git 进程；否则 git 可能阻塞在写满的 stdout 管道上
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = mirror
	// 与其它 git 命令相同的环境边界：不继承宿主机的配置、模板、netrc 与外层包装（gitEnvFrom）
	cmd.Env = gitEnv("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fail := func(err error) error {
		cancel()
		_ = cmd.Wait() // 进程是被我们结束的，退出状态没有意义
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return fmt.Errorf("%w（git：%s）", err, s)
		}
		return err
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- writeObjectNames(stdin, blobs) }()

	br := bufio.NewReaderSize(stdout, catFileBufferSize)
	for _, e := range blobs {
		if err := checkoutEntry(root, br, e); err != nil {
			return fail(err)
		}
	}
	if err := <-writeErr; err != nil {
		return fail(fmt.Errorf("向 git cat-file 写入对象名：%w", err))
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file：%w：%s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// writeObjectNames 把对象名逐行写入 git cat-file 的 stdin，然后关闭它。
func writeObjectNames(stdin io.WriteCloser, blobs []actionkey.TreeEntry) error {
	w := bufio.NewWriter(stdin)
	for _, e := range blobs {
		if _, err := w.WriteString(e.SHA + "\n"); err != nil {
			_ = stdin.Close() // 已经出错，以写入的错误为准
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = stdin.Close() // 同上
		return err
	}
	return stdin.Close()
}

// checkoutEntry 从 cat-file 的输出中读出一个对象（"<sha> <type> <size>\n<内容>\n"），写到 root 中。
func checkoutEntry(root *os.Root, br *bufio.Reader, e actionkey.TreeEntry) error {
	hdr, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取 %s：%w", e.Path, err)
	}
	f := strings.Fields(hdr)
	if len(f) != 3 {
		return fmt.Errorf("git cat-file：%s", strings.TrimSpace(hdr))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("git cat-file：无效的对象大小 %q", f[2])
	}
	name := filepath.FromSlash(e.Path)
	if !filepath.IsLocal(name) {
		return fmt.Errorf("路径越界：%s", e.Path)
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	switch e.Mode {
	case actionkey.ModeSymlink:
		target := make([]byte, size)
		if _, err := io.ReadFull(br, target); err != nil {
			return err
		}
		if err := root.Symlink(string(target), name); err != nil {
			return err
		}
	default:
		if err := writeBlob(root, name, e.Mode == actionkey.ModeExecutable, br, size); err != nil {
			return err
		}
	}
	// 内容之后的换行
	b, err := br.ReadByte()
	if err != nil {
		return fmt.Errorf("读取 %s：%w", e.Path, err)
	}
	if b != '\n' {
		return fmt.Errorf("git cat-file：%s 的内容之后缺少换行", e.Path)
	}
	return nil
}

func writeBlob(root *os.Root, name string, executable bool, r io.Reader, size int64) error {
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.CopyN(out, r, size)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

const (
	// hostStopGrace 是取消 step 时，从 SIGTERM 到 SIGKILL 的等待时间。
	hostStopGrace = 10 * time.Second
	// noNetworkProxy 是关闭网络时注入的代理地址（discard 端口，连接会立即失败）。
	noNetworkProxy = "http://127.0.0.1:9"
)

// hostExec 直接在宿主机上以 Runner 的用户执行 step。
type hostExec struct {
	d         *attemptDirs
	extraPath []string
	network   bool          // job 的 network；为 false 时注入不可用的代理
	proxy     ProxySettings // network: true 时注入的代理（[proxy.host]）
	git       *hostGit
}

func (h *hostExec) workspace() string                       { return h.d.ws }
func (h *hostExec) home() string                            { return h.d.home }
func (h *hostExec) tmp() string                             { return h.d.tmp }
func (h *hostExec) rtMount(_, hostDir string) string        { return hostDir }
func (h *hostExec) toolcacheMount(_, hostDir string) string { return hostDir }
func (h *hostExec) cacheDomain() string                     { return cacheDomainHost }

func (h *hostExec) basePath() string {
	dirs := append(append([]string(nil), h.extraPath...), "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	return strings.Join(dirs, ":")
}

// jobEnv 设置宿主机上的 locale、用户和 shell；开启网络时加入 [proxy.host] 的代理。
func (h *hostExec) jobEnv(env map[string]string) {
	if runtime.GOOS == "darwin" {
		env["LANG"] = "en_US.UTF-8"
	}
	if u, err := user.Current(); err == nil {
		env["USER"], env["LOGNAME"] = u.Username, u.Username
	}
	env["SHELL"] = "/bin/bash"
	if h.network {
		h.proxy.apply(env)
	}
}

// lockEnv 在关闭网络时让常见工具的代理指向不可用的地址：放在 workflow env 之后，workflow 不能恢复外网访问。
func (h *hostExec) lockEnv(env map[string]string) {
	if !h.network {
		blockNetworkEnv(env)
	}
}

// useGit 让 step 中的 git 直接使用宿主机上的 socket 和 kiln-runner；git exec 镜像按每个 step 的 PATH 中的 git 建（hostGit）。
// 通道没能打开时 helper 只说明原因。
func (h *hostExec) useGit(_ context.Context, p gitPlan, log *runnerkit.LogShipper) error {
	base := gitAccess{scope: p.scope, helper: gitHelperCmd(p.exe), socket: p.sock}
	if p.unavailable != "" {
		base = gitAccess{scope: p.scope, helper: gitRefusingHelper("Kiln 的 git 凭据通道不可用（" + p.unavailable + "）")}
		log.Printf("Kiln 的 git 凭据通道不可用（%s）：step 中的 git 访问私有仓库时会失败", p.unavailable)
	}
	h.git = newHostGit(base, h.d.root, p.exe)
	return nil
}

// gitAccess 的 workDir 是 step 的 working-directory（相对工作区）：step 的 shell 在那里按 PATH 找 git，这里同样。不合法时
// 不建镜像（run 随后以同一个原因失败）。
func (h *hostExec) gitAccess(ctx context.Context, env map[string]string, workDir string, log *runnerkit.LogShipper) *gitAccess {
	if h.git == nil {
		return nil
	}
	if checkWorkDir(h.d.ws, workDir) != nil {
		a := h.git.base
		return &a
	}
	return h.git.access(ctx, env, filepath.Join(h.d.ws, filepath.FromSlash(workDir)), log)
}

func (h *hostExec) prepare(context.Context, *runnerkit.LogShipper) error { return nil }
func (h *hostExec) destroy(context.Context)                              {}

// lookPathIn 按 /bin/sh 的规则在 pathEnv（step 设置的 PATH；没有设置 PATH 的不调用它）中查找可执行文件，找不到时原样返回：
// 按顺序逐项查找，第一个是普通文件（跟随符号链接）且本用户可执行的胜出，不是文件、不可执行、查不了的跳过继续找；相对的项
// 和空项（开头、中间、末尾的空项，以及整个 PATH 为空字符串时的单个空项，都表示当前目录）按 base（step 的工作目录，step 的
// shell 在那里解析它们）解析。gitaudit_test.go 用真实的 /bin/sh 核对了这些规则。
func lookPathIn(name, pathEnv, base string) string {
	if strings.Contains(name, "/") {
		return name
	}
	dirs := filepath.SplitList(pathEnv)
	if pathEnv == "" {
		dirs = []string{""} // SplitList("") 没有项，shell 把它当作一个空项
	}
	for _, dir := range dirs {
		// 按原始分量拼接（joinRaw）：missing/../rel 在 missing 不存在时走不通，link/.. 回到链接目标的父目录，与 shell 相同。
		// 空项即 base 本身；base 为空时相对项按本进程的当前目录
		switch {
		case filepath.IsAbs(dir):
		case base != "":
			dir = joinRaw(base, dir)
		case dir == "":
			dir = "."
		}
		p := joinRaw(dir, name)
		//nolint:gosec // G703：只查看 step 的 PATH 中的文件是否存在、可执行，不读写内容
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && syscall.Access(p, accessExecute) == nil {
			return p
		}
	}
	return name
}

// joinRaw 用分隔符拼接 dir 和 rel，不做词法规整（filepath.Join 会 Clean，在文件系统解析符号链接之前就消掉 ..）：用于要与
// 另一个进程（shell、git、libcurl）按同样的字节打开同一个文件的路径——PATH 项、NETRC、HOME、git --exec-path 的输出——由
// 文件系统按原顺序解析符号链接、缺失的组件和 ..。dir 为空时原样返回 rel（相对本进程的当前目录）。
func joinRaw(dir, rel string) string {
	if dir == "" {
		return rel
	}
	return dir + string(filepath.Separator) + rel
}

// accessExecute 是 access(2) 的 X_OK：与 execve 相同按本进程的用户判断能否执行（不是只看有没有任何一个 x 位）。
const accessExecute = 0x1

func (h *hostExec) run(ctx context.Context, s stepSpec, out io.Writer) (int, error) {
	file := filepath.Join(h.d.tmp, stepScriptName(s.index))
	if err := os.WriteFile(file, []byte(s.script), 0o700); err != nil {
		return -1, err
	}
	defer func() { _ = os.Remove(file) }() // 整个 attempt 目录最后会删除
	argv, err := shellCommand(s.shell, file, false)
	if err != nil {
		return -1, err
	}
	if err := checkWorkDir(h.d.ws, s.workDir); err != nil {
		return -1, err
	}
	dir := filepath.Join(h.d.ws, filepath.FromSlash(s.workDir))
	if pathEnv, ok := s.env["PATH"]; ok {
		argv[0] = lookPathIn(argv[0], pathEnv, dir)
	}
	//nolint:noctx // 取消由 waitCmd 处理：先 SIGTERM 整个进程组，宽限期后再 SIGKILL
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = envList(s.env)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// step 退出后，后台进程（或 setsid 脱离进程组的进程）可能还拿着输出管道，最多再等一会儿
	cmd.WaitDelay = pipeWaitDelay
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return -1, userErrorf("启动 %s 失败：%v", argv[0], err)
		}
		return -1, err
	}
	pgid := cmd.Process.Pid
	signalGroup := func(sig syscall.Signal) func() {
		// 进程组可能已经全部退出，ESRCH 可以忽略
		return func() { _ = syscall.Kill(-pgid, sig) }
	}
	werr := waitCmd(ctx, cmd, hostStopGrace, signalGroup(syscall.SIGTERM), signalGroup(syscall.SIGKILL))
	// 清理 step 留下的后台进程
	signalGroup(syscall.SIGKILL)()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	code, err := exitStatus(werr)
	if errors.Is(err, exec.ErrWaitDelay) {
		fmt.Fprintln(out, "（step 已退出，但仍有后台进程占用输出，已结束这些进程）")
		return 0, nil
	}
	return code, err
}

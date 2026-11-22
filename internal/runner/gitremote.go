package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// step 中 git 进程一侧的命令（PLAN #8.4）：都由 kiln-runner 这个二进制提供，
// 不读 Runner 的配置、不连接 Server，只经 KILN_GIT_SOCKET 连 Runner 的凭据通道（gitchannel.go）。
//   - 传输包装：git exec 镜像中的 git-remote-http(s) 等是 kiln-runner，按 argv[0] 分派。先检查认证冲突（gitauth.go），
//     再向通道开一个操作并保持连接，以操作 id 启动真实的远程助手，等它退出后关闭连接（被杀时由内核关闭）。
//   - credential helper（kiln-runner git-credential get）：带着操作 id 向通道要凭据；store、erase 什么都不做（不落盘）。
//   - 连通检查（kiln-runner git-socket-check <socket>）：oci 的 prepare 在容器中执行。

// step 环境中凭据通道用的环境变量。
const (
	// gitSocketEnv 是执行环境中凭据通道的 socket 路径。
	gitSocketEnv = "KILN_GIT_SOCKET"
	// gitRealExecEnv 是真实的 git exec 目录（git exec 镜像的来源，传输包装在其中找真实的远程助手）。
	gitRealExecEnv = "KILN_GIT_EXEC_REAL"
	// gitOpEnv 是传输包装交给真实远程助手（再由它交给 credential helper）的操作 id；gitOpErrorEnv 是没有操作时的原因。
	gitOpEnv      = "KILN_GIT_OP"
	gitOpErrorEnv = "KILN_GIT_OP_ERROR"
)

// kiln-runner 中供 git 调用的子命令。
const (
	gitCredentialCmd  = "git-credential" //nolint:gosec // G101：子命令名，不是凭证
	gitSocketCheckCmd = "git-socket-check"
)

// gitRemoteHelpers 是换成传输包装的远程助手（git 按地址的协议找 git-remote-<协议>）。
var gitRemoteHelpers = []string{"git-remote-http", "git-remote-https", "git-remote-ftp", "git-remote-ftps"}

// GitCommand 处理 git 调用的 kiln-runner：args 是完整的命令行（含 argv[0]）。不是这些调用时 ok 为 false。
func GitCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	if name := filepath.Base(args[0]); slices.Contains(gitRemoteHelpers, name) {
		return gitRemote(name, args[1:], stderr), true
	}
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case gitCredentialCmd:
		return gitCredential(args[2:], stdin, stdout, stderr), true
	case gitSocketCheckCmd:
		return gitSocketCheck(args[2:], stderr), true
	}
	return 0, false
}

// dialGit 连接凭据通道并发送一条请求，返回连接和读取回复用的 reader。连接有 gitDialTimeout，写请求、读第一条回复各有
// wait 的期限（取值见各命令的常量）；读到回复后清除期限：open 的连接保持到传输结束。
func dialGit(sock string, m gitMsg, wait time.Duration) (net.Conn, *bufio.Reader, gitReply, error) {
	if sock == "" {
		return nil, nil, gitReply{}, fmt.Errorf("没有 %s：这个环境不是 Kiln 的 step，或者 Runner 没有为它准备凭据通道", gitSocketEnv)
	}
	// 这些命令由 git 启动，没有可传入的 context
	conn, err := (&net.Dialer{Timeout: gitDialTimeout}).DialContext(context.Background(), "unix", sock)
	if err != nil {
		return nil, nil, gitReply{}, err
	}
	var r gitReply
	br := bufio.NewReaderSize(conn, maxGitMessage)
	if err := conn.SetWriteDeadline(time.Now().Add(wait)); err != nil {
		_ = conn.Close()
		return nil, nil, r, err
	}
	if err := writeGitMsg(conn, m); err != nil {
		_ = conn.Close()
		return nil, nil, r, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		_ = conn.Close()
		return nil, nil, r, err
	}
	if err := readGitMsg(br, &r); err != nil {
		_ = conn.Close()
		return nil, nil, r, fmt.Errorf("读取 kiln-runner 的回复：%w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, r, err
	}
	return conn, br, r, nil
}

// gitRemote 是传输包装：git 以 <remote> [<地址>] 调用，stdin、stdout 是远程助手协议（原样交给真实的远程助手，本进程不写
// stdout）。返回真实远程助手的退出码。
func gitRemote(name string, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "kiln-runner：%s 要由 git 调用：<remote> [<地址>]\n", name)
		return 2
	}
	raw := args[len(args)-1]
	shown := safeGitURL(raw) // 诊断中不回显地址中的用户名、密码、query
	if conflicts := gitAuthConflicts(raw, "", os.Environ()); len(conflicts) > 0 {
		fmt.Fprintf(stderr, "kiln-runner：拒绝这次 git 传输（%s）：执行环境中有会带上别的凭据的配置：%s。Kiln 不使用机器上的个人凭据，去掉这些配置后重试。\n",
			shown, joinConflicts(conflicts))
		return 1
	}
	conn, _, reply, err := dialGit(os.Getenv(gitSocketEnv), gitMsg{Cmd: gitCmdOpen, URL: raw, PID: os.Getpid()}, gitOpenReplyTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "kiln-runner：无法连接 git 凭据通道（%v），这次 git 传输（%s）没有执行\n", err, shown)
		return 1
	}
	defer conn.Close() // 关闭连接即结束操作：Runner 吊销这次传输的凭据
	if reply.Error != "" {
		fmt.Fprintf(stderr, "kiln-runner：%s，这次 git 传输（%s）没有执行\n", reply.Error, shown)
		return 1
	}
	helper, err := realRemoteHelper(name)
	if err != nil {
		fmt.Fprintf(stderr, "kiln-runner：找不到真实的 %s：%v\n", name, err)
		return 1
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, gitOpEnv+"=") || strings.HasPrefix(kv, gitOpErrorEnv+"=")
	})
	if reply.Op != "" {
		env = append(env, gitOpEnv+"="+reply.Op)
	} else {
		env = append(env, gitOpErrorEnv+"="+reply.Denied)
	}
	return runForwarding(helper, args, env, stderr)
}

// realRemoteHelper 返回真实的远程助手：在 KILN_GIT_EXEC_REAL（建 git exec 镜像时的来源）中，没有这个变量时按 PATH 中的
// git 的 exec 目录（不受已经设置的 GIT_EXEC_PATH 影响）。
func realRemoteHelper(name string) (string, error) {
	dir := os.Getenv(gitRealExecEnv)
	if dir == "" {
		var err error
		if dir, err = gitExecPath(context.Background(), "git", "", os.Environ()); err != nil {
			return "", err
		}
	}
	return joinRaw(dir, name), nil // dir 是 git --exec-path 的输出（或建镜像时记下的它）：原样拼接
}

// runForwarding 运行 argv0（stdin、stdout、stderr 原样继承），把 SIGINT、SIGTERM、SIGHUP 转给它，返回它的退出码
// （被信号结束时 128+n）。
func runForwarding(argv0 string, args, env []string, stderr io.Writer) int {
	//nolint:noctx,gosec // 远程助手由 git 控制生命周期，本进程只转发信号、等它退出；G702：真实的远程助手，参数原样来自 git，不经过 shell
	cmd := exec.Command(argv0, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "kiln-runner：无法运行 %s：%v\n", argv0, err)
		return 1
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s) // 进程可能已经退出
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(stderr, "kiln-runner：%s：%v\n", argv0, err)
		return 1
	}
	return 0
}

// gitCredential 是 credential helper：git 在命令后加上 get、store、erase，从 stdin 给出请求。只处理 get：带着传输包装
// 设置的操作 id 向通道要凭据，按 helper 协议写到 stdout。不给凭据时输出 quit=1（git 不再问别的 helper、askpass 或终端，
// 直接失败），原因写到 stderr（进入 step 的日志）。
func gitCredential(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "get" {
		_, _ = io.Copy(io.Discard, stdin) // store、erase：读完请求，什么都不做
		return 0
	}
	q := parseCredentialQuery(stdin)
	refuse := func(reason string) int {
		fmt.Fprintf(stderr, "kiln-runner：%s\n", reason)
		fmt.Fprintln(stdout, "quit=1")
		return 0
	}
	op := os.Getenv(gitOpEnv)
	if op == "" {
		if reason := os.Getenv(gitOpErrorEnv); reason != "" {
			return refuse(reason)
		}
		return refuse("这个 git 请求没有经过 kiln-runner 的传输包装（GIT_EXEC_PATH 被改动，或者用的是另一个 git），不给凭据")
	}
	conn, _, r, err := dialGit(os.Getenv(gitSocketEnv), gitMsg{Cmd: gitCmdGet, Op: op, Protocol: q["protocol"], Host: q["host"], Path: q["path"]}, gitGetReplyTimeout)
	if err != nil {
		return refuse(fmt.Sprintf("无法从 git 凭据通道取得凭据：%v", err))
	}
	_ = conn.Close() // 回复已经读到
	if r.Error != "" {
		return refuse(r.Error)
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", r.Username, r.Password)
	return 0
}

// parseCredentialQuery 解析 credential helper 协议的输入（每行 key=value，空行或结束为止）。
func parseCredentialQuery(r io.Reader) map[string]string {
	q := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			q[k] = v
		}
	}
	return q
}

// gitSocketCheck 检查能否连上凭据通道（args[0] 是 socket 路径）：连上并收到回复时退出码为 0。
func gitSocketCheck(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "用法：kiln-runner %s <socket>\n", gitSocketCheckCmd)
		return 2
	}
	conn, _, r, err := dialGit(args[0], gitMsg{Cmd: gitCmdPing}, gitPingTimeout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_ = conn.Close() // 回复已经读到
	if r.Error != "" {
		fmt.Fprintln(stderr, r.Error)
		return 1
	}
	return 0
}

// gitExecPath 返回 git（按 env 中的 PATH 查找，或者绝对路径）自己的 exec 目录：不受 env 中已经设置的 GIT_EXEC_PATH
// 影响（可能是 git exec 镜像）。cwd 是执行它的工作目录（step 的工作目录；为空时是本进程的）。
func gitExecPath(ctx context.Context, git, cwd string, env []string) (string, error) {
	// 与认证冲突检查的 git 查询相同的期限：git 卡住时这个 step 的凭据设置失败，不一直等；git（或替代它的包装）留下仍持有
	// 输出管道的后代时，等待同样有界（groupCommand）
	ctx, cancel := context.WithTimeout(ctx, gitAuthQueryTimeout)
	defer cancel()
	cmd := groupCommand(ctx, git, "--exec-path")
	cmd.Dir = cwd
	cmd.Env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "GIT_EXEC_PATH=") })
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --exec-path：%w", git, err)
	}
	// 只去掉末尾的那一个换行（记录终止符）：路径本身的字节原样保留（目录名可以以空格、\r 结尾）
	dir := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s --exec-path 给出的不是绝对路径：%s", git, strconv.Quote(dir))
	}
	return dir, nil
}

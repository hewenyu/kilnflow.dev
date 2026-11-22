package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

const (
	// cleanupTimeout 是 destroy 等清理操作的超时（job 的 ctx 可能已经取消）。
	cleanupTimeout = 2 * time.Minute
	// pipeWaitDelay 是 step 退出后，等待仍占用输出管道的后台进程的时间。
	pipeWaitDelay = 5 * time.Second
)

// executor 是一个 job 的执行环境（PLAN #8.5）。
type executor interface {
	// workspace / home / tmp 是执行环境中看到的路径。
	workspace() string
	home() string
	tmp() string
	// rtMount 返回 runtime 在执行环境中的挂载点，并登记需要挂载的宿主机目录。
	rtMount(name, hostDir string) string
	// toolcacheMount 同上，用于 toolcache 目录。
	toolcacheMount(name, hostDir string) string
	// cacheDomain 是 toolcache 的执行身份（PLAN #4.6）：host，或 oci-<镜像的默认用户>。不同执行身份的
	// 同名 toolcache 使用不同的目录，避免容器中 root 写入的文件让宿主机上的 job 无权写入。
	cacheDomain() string
	// basePath 是系统 PATH（runtime 的路径会加在它前面）。
	basePath() string
	// jobEnv 在全部 step 共用的环境变量中加入这种执行环境特有的变量。在加入 runtime 的环境变量之前调用，
	// 只设置 runtime 不会设置的变量。
	jobEnv(env map[string]string)
	// lockEnv 在合并 workflow 的 env 之后调用，覆盖 workflow 不能修改的变量（network: false 时的代理）。
	lockEnv(env map[string]string)
	// useGit 准备 step 中 git 使用的凭据通道（在 prepare 之前调用，gitenv.go）。返回错误时 job 以 infra 失败。
	useGit(ctx context.Context, p gitPlan, log *runnerkit.LogShipper) error
	// gitAccess 返回一个 step 中 git 的设置（prepare 之后、按合并 workflow 的 env 之后的 step 环境 env 调用；没有调用过
	// useGit 时为 nil）。
	gitAccess(ctx context.Context, env map[string]string, workDir string, log *runnerkit.LogShipper) *gitAccess
	prepare(ctx context.Context, log *runnerkit.LogShipper) error
	// run 执行一个 step，返回退出码；用户造成的错误返回 *userError。
	run(ctx context.Context, s stepSpec, out io.Writer) (int, error)
	destroy(ctx context.Context)
}

// gitPlan 是 executor 准备 git 凭据通道用到的信息（宿主机上的路径）。
type gitPlan struct {
	scope         string // credential helper 生效的地址（<协议>://<主机>）
	sockDir, sock string // socket 所在的目录和 socket
	exe           string // kiln-runner（传输包装、credential helper）
	required      bool   // job 声明了 repositories：通道在执行环境中不可用时 job 失败
	// unavailable 非空时通道没能打开（例如 socket 路径太长），是原因：step 中的 git 要凭据时 helper 只说明它。
	unavailable string
}

// rootCleaner 是能删除执行环境中以其他身份（例如容器中的 root）写入工作区的文件的 executor。
// 宿主机上的 Runner 用户删不掉工作区时调用。
type rootCleaner interface {
	cleanupRoot(ctx context.Context)
}

// stepSpec 是执行一个 step 所需的参数（已经替换过占位符和 secret）。
type stepSpec struct {
	index   int    // step 的序号，用于临时文件名
	script  string // 脚本内容
	shell   string // 与 GitHub Actions 的 shell 写法一致
	workDir string // 相对工作区的 working-directory
	env     map[string]string
}

// 宿主机上每个 attempt 的目录：
//
//	work/<attempt>/      0700：其中的 step 脚本可能含有 secret
//	work/<attempt>/ws    工作区（源码根目录）
//	work/<attempt>/home  HOME
//	work/<attempt>/tmp   TMPDIR 和 step 脚本
type attemptDirs struct{ root, ws, home, tmp string }

// attempt 目录下的子目录名。
const (
	attemptWorkspaceDir = "ws"
	attemptHomeDir      = "home"
	attemptTmpDir       = "tmp"
)

// attemptSubdirs 返回 attempt 目录下的全部子目录名。
func attemptSubdirs() []string {
	return []string{attemptWorkspaceDir, attemptHomeDir, attemptTmpDir}
}

func newAttemptDirs(root string) (*attemptDirs, error) {
	d := &attemptDirs{
		root: root,
		ws:   filepath.Join(root, attemptWorkspaceDir),
		home: filepath.Join(root, attemptHomeDir),
		tmp:  filepath.Join(root, attemptTmpDir),
	}
	// 同一个 attempt 的残留目录（例如 Runner 重启前留下的）必须先清掉
	if err := runnerkit.RemoveTree(root); err != nil {
		return nil, fmt.Errorf("清理旧的工作区 %s：%w", root, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	for _, p := range []string{d.ws, d.home, d.tmp} {
		if err := os.Mkdir(p, 0o755); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// stepScriptName 返回第 index 个 step 的脚本文件名（放在 TMPDIR 中）。
func stepScriptName(index int) string { return fmt.Sprintf("kiln-step-%d.sh", index) }

// stepEnvName 返回第 index 个 step 的环境变量文件名（oci executor 使用，放在 TMPDIR 中）。
func stepEnvName(index int) string { return fmt.Sprintf("kiln-env-%d.sh", index) }

// shellCommand 返回执行脚本文件的命令（与 GitHub Actions 的 shell 写法一致）。
func shellCommand(shell, script string, container bool) ([]string, error) {
	switch shell {
	case "":
		if container {
			// 镜像中没有 bash 时退回 sh
			return []string{"sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -eo pipefail "$0"; else exec sh -e "$0"; fi`, script}, nil
		}
		return []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", script}, nil
	case "bash":
		return []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", script}, nil
	case "sh":
		return []string{"sh", "-e", script}, nil
	case "python":
		return []string{"python3", script}, nil
	}
	if !strings.Contains(shell, "{0}") {
		return nil, userErrorf("不支持的 shell %q（可用 bash、sh、python，或包含 {0} 的自定义命令）", shell)
	}
	var out []string
	for f := range strings.FieldsSeq(shell) {
		out = append(out, strings.ReplaceAll(f, "{0}", script))
	}
	return out, nil
}

// checkWorkDir 确认 working-directory 存在、是目录，并且（符号链接解析后）仍在工作区内。
func checkWorkDir(ws, wd string) error {
	if wd == "" {
		return nil
	}
	local := filepath.FromSlash(wd)
	if !filepath.IsLocal(local) {
		return userErrorf("working-directory %q 越出工作区", wd)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Join(ws, local))
	if err != nil {
		return userErrorf("working-directory %q 不存在", wd)
	}
	wsReal, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return userErrorf("working-directory %q 不存在", wd)
	}
	if !withinBase(wsReal, realDir) {
		return userErrorf("working-directory %q 经由符号链接指向工作区之外", wd)
	}
	st, err := os.Stat(realDir)
	if err != nil {
		return userErrorf("working-directory %q 不存在", wd)
	}
	if !st.IsDir() {
		return userErrorf("working-directory %q 不是目录", wd)
	}
	return nil
}

// envList 把环境变量转换为按名称排序的 KEY=VALUE 列表。
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// waitCmd 等待已启动的 cmd 结束。ctx 结束时先调用 stop 请求停止，grace 内仍未结束则调用 kill，
// 再等待其退出，这种情况下返回 ctx.Err()；否则返回 cmd.Wait 的结果。
func waitCmd(ctx context.Context, cmd *exec.Cmd, grace time.Duration, stop, kill func()) error {
	var werr error
	exited := make(chan struct{})
	go func() {
		werr = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		return werr
	case <-ctx.Done():
	}
	// 进程是被取消停下的，退出状态没有意义，以 ctx 的错误为准
	stop()
	select {
	case <-exited:
	case <-time.After(grace):
		kill()
		<-exited
	}
	return ctx.Err()
}

// exitStatus 把 cmd.Wait 的结果转换为退出码：非零退出不是错误。
func exitStatus(err error) (int, error) {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// errPollTimeout 表示 pollUntil 超过了时间上限。
var errPollTimeout = errors.New("超时")

// pollUntil 每隔 interval 调用一次 check，直到 check 返回 done 或错误（返回 check 的错误）、
// ctx 结束（返回 ctx.Err()），或超过 timeout（返回 errPollTimeout）。
func pollUntil(ctx context.Context, timeout, interval time.Duration, check func() (done bool, err error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return errPollTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// cleanupContext 返回清理操作使用的 context：不随 job 取消，但有超时。
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// toolcache 的执行身份（PLAN #4.6）。
const (
	cacheDomainHost   = "host"
	cacheDomainOCIPfx = "oci-"
)

// ociCacheDomain 返回以镜像默认用户 user 执行的 oci job 的执行身份：oci-root（user 为空、root 或 UID 0），
// 或 oci-<user>（只保留字母、数字、点、下划线和连字符）。
func ociCacheDomain(user string) string {
	u := strings.TrimSpace(user)
	name, group, _ := strings.Cut(u, ":")
	if name == "" || name == "root" || name == "0" {
		if group == "" || group == "root" || group == "0" {
			return cacheDomainOCIPfx + "root"
		}
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, u)
	return cacheDomainOCIPfx + clean
}

// platformDir 把平台（os/arch）转换为目录名（os-arch）。
func platformDir(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}

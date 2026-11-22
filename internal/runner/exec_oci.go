package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const (
	// ociStopGrace 是取消 step 时，从 docker kill 到强制结束 docker exec 的等待时间。
	ociStopGrace = 15 * time.Second
	// serviceHealthTimeout 是等待 service 变为 healthy 的时间上限。
	serviceHealthTimeout = 2 * time.Minute
	// serviceHealthPoll 是检查 service 状态的间隔。
	serviceHealthPoll = time.Second
	// defaultContainerPath 是镜像没有设置 PATH 时使用的 PATH。
	defaultContainerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// errorLogLines 是容器意外退出时，错误信息中附带的日志行数。
	errorLogLines = 30
)

// 容器中的固定路径。
const (
	ociWorkspace = "/work"
	ociHome      = "/kiln/home"
	ociTmp       = "/kiln/tmp"
	// ociRuntimeDir、ociToolcacheDir 下按名称挂载 runtime 和 toolcache 目录。
	ociRuntimeDir   = "/opt/kiln/rt"
	ociToolcacheDir = "/opt/kiln/toolcache"
	// ociBuildkitDir 是 job 容器中 buildctl 所在的目录（加在 PATH 最前面，见 exec_buildkit.go）。
	ociBuildkitDir = "/opt/kiln/buildkit"
)

// docker 的网络模式。
const (
	dockerNetworkNone   = "none"
	dockerNetworkBridge = "bridge"
)

// ociExec 在容器中执行 step（docker）。
//
// 容器不指定 --user，以镜像的默认用户运行，并且要求它能读写宿主机上以 Runner 用户身份创建的
// 工作区目录：实际上就是 root（或与 Runner 相同的 uid）。因此 step 脚本和环境变量文件只给属主权限，
// 不影响容器中的读取（root 不受文件权限位限制）。
type ociExec struct {
	docker   string
	d        *attemptDirs
	name     string
	image    string
	platform string
	cpu      float64
	memory   int64
	network  bool
	// proxy 是 network: true 时注入 job 容器和 BuildKit daemon 的代理（[proxy.oci]，容器可达的地址）。
	proxy     ProxySettings
	services  []ociService
	mounts    [][2]string // 宿主机目录 → 容器路径（只读）
	rwMounts  [][2]string
	imagePath string
	imageUser string // 镜像的默认用户（Config.User），step 以它执行
	netName   string
	started   []string // 需要清理的容器
	volumes   []string // 需要清理的 volume
	// buildkitImage 非空时为 job 启动 BuildKit daemon（buildkit: true，见 exec_buildkit.go）。
	buildkitImage string
	// label 是创建的容器、网络和 volume 的标签（runnerLabel=<Runner ID>）：Runner 空闲时据此清理
	// 进程被强行结束时残留的对象（gc.go）。
	label string
	// git 是 step 中 git 的设置（useGit），gitSockDir、gitExe 是挂进容器的 socket 目录和 kiln-runner（不可用时为空），
	// gitRequired 表示 job 声明了 repositories（通道连不上时 job 失败）。
	git         *gitAccess
	gitSockDir  string
	gitExe      string
	gitRequired bool
	// hostOS 是 Runner 所在的操作系统（runtime.GOOS；测试中替换，两种平台的结论在任何平台上都能测到）。
	hostOS string
	// gitCheckTimeout 是 checkGit 两步准备共用的总期限（ociGitPrepareTimeout；测试中缩短）。
	gitCheckTimeout time.Duration
}

type ociService struct {
	Name, Image string
	Env         map[string]string
	Command     []string
}

// newOCIExec 按 JobSpec 创建 oci executor；镜像由 prepareImage 准备。runnerID 用于给创建的对象加标签。
func newOCIExec(docker string, d *attemptDirs, sp *runnerv1.JobSpec, runnerID int64) *ociExec {
	o := &ociExec{
		docker:  docker,
		label:   runnerLabelValue(runnerID),
		d:       d,
		name:    fmt.Sprintf("kiln-%d", sp.Ref.AttemptId),
		image:   sp.Executor.Image,
		network: sp.Network,
		cpu:     sp.GetResources().GetCpu(),
		memory:  sp.GetResources().GetMemoryBytes(),
		hostOS:  runtime.GOOS,

		gitCheckTimeout: ociGitPrepareTimeout,
	}
	if p := sp.Executor.Platform; p != "" && p != runnerkit.NativePlatform() {
		o.platform = p
	}
	for _, s := range sp.Services {
		o.services = append(o.services, ociService{Name: s.Name, Image: s.Image, Env: s.Env, Command: s.Command})
	}
	return o
}

func (o *ociExec) workspace() string { return ociWorkspace }
func (o *ociExec) home() string      { return ociHome }
func (o *ociExec) tmp() string       { return ociTmp }

func (o *ociExec) rtMount(name, hostDir string) string {
	p := ociRuntimeDir + "/" + name
	o.mounts = append(o.mounts, [2]string{hostDir, p})
	return p
}

func (o *ociExec) cacheDomain() string { return ociCacheDomain(o.imageUser) }

func (o *ociExec) toolcacheMount(name, hostDir string) string {
	p := ociToolcacheDir + "/" + name
	o.rwMounts = append(o.rwMounts, [2]string{hostDir, p})
	return p
}

func (o *ociExec) basePath() string {
	p := defaultContainerPath
	if o.imagePath != "" {
		p = o.imagePath
	}
	if o.buildkitImage != "" {
		p = ociBuildkitDir + ":" + p
	}
	return p
}

// jobEnv 在开启了 BuildKit 时设置 BUILDKIT_HOST；开启网络时加入 [proxy.oci] 的代理。
func (o *ociExec) jobEnv(env map[string]string) {
	if o.buildkitImage != "" {
		env["BUILDKIT_HOST"] = buildkitHost
	}
	if o.network {
		o.proxy.apply(env)
	}
}

// lockEnv 不需要做什么：network: false 时容器本身没有外网（none 或内部网络）。
func (o *ociExec) lockEnv(map[string]string) {}

// useGit 登记 socket 目录（读写）和 kiln-runner（只读）的挂载，step 中的 git 用容器中的路径；连通在 prepare 中检查。
// kiln-runner 不能在容器中运行（不是 Linux 上的 Runner，或者不是静态链接的）时通道不可用：声明了 repositories 的 job
// 失败，其余 job 照常运行，容器中的 git 要凭据时得到说明。
func (o *ociExec) useGit(_ context.Context, p gitPlan, log *runnerkit.LogShipper) error {
	o.git = &gitAccess{scope: p.scope, configInScript: true}
	o.gitRequired = p.required
	if p.unavailable != "" {
		return o.gitUnavailable(p.unavailable, log)
	}
	if reason := containerRunnerProblem(o.hostOS, p.exe); reason != "" {
		return o.gitUnavailable(reason, log)
	}
	o.gitSockDir, o.gitExe = p.sockDir, p.exe
	o.git.helper = gitHelperCmd(ociKilnRunner)
	o.git.socket = ociGitDir + "/" + filepath.Base(p.sock)
	return nil
}

// gitUnavailable 处理凭据通道在容器中不可用：声明了 repositories 的 job 返回错误；其余 job 的 credential helper 只说明原因。
func (o *ociExec) gitUnavailable(reason string, log *runnerkit.LogShipper) error {
	if o.gitRequired {
		return fmt.Errorf("%w：%s", errGitUnavailable, reason)
	}
	o.git.socket = ""
	o.git.helper = gitRefusingHelper("Kiln 的 git 凭据通道在容器中不可用（" + reason + "）")
	log.Printf("Kiln 的 git 凭据通道在容器中不可用（%s）：容器中的 git 访问私有仓库时会失败", reason)
	return nil
}

// gitAccess 与 step 的环境无关：git exec 镜像由 step 的启动脚本按容器中的 git 建。
func (o *ociExec) gitAccess(context.Context, map[string]string, string, *runnerkit.LogShipper) *gitAccess {
	return o.git
}

// networkMode 返回 job 的 network 对应的 docker 网络（不使用 service 网络时）。
func (o *ociExec) networkMode() string {
	if o.network {
		return dockerNetworkBridge
	}
	return dockerNetworkNone
}

// limitArgs 返回 docker run 的平台和资源限制参数。
func (o *ociExec) limitArgs() []string {
	var args []string
	if o.platform != "" {
		args = append(args, "--platform", o.platform)
	}
	if o.cpu > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(o.cpu, 'f', 2, 64))
	}
	if o.memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(o.memory, 10))
	}
	return args
}

// output 执行 docker 命令，返回去掉首尾空白的 stdout。
func (o *ociExec) output(ctx context.Context, args ...string) (string, error) {
	out, err := dockerOutput(ctx, o.docker, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// dockerOutput 执行 docker 命令，返回 stdout；失败时错误信息中带上 stderr 和 stdout。
// 不能用 CombinedOutput：docker 会在 stderr 中输出警告（例如同时设置了 DOCKER_HOST 和 docker context 时，
// 每条命令都输出 "Warning: DOCKER_HOST environment variable overrides the active context"），
// 混进要解析的输出后，镜像的默认用户、PATH、容器状态和容器 ID 都会解析错。
func dockerOutput(ctx context.Context, docker string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, docker, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String() + "\n" + string(out))
		return string(out), fmt.Errorf("docker %s：%w：%s", args[0], err, detail)
	}
	return string(out), nil
}

// ensureImage 在本地没有镜像时拉取。
func (o *ociExec) ensureImage(ctx context.Context, image string, log *runnerkit.LogShipper) error {
	if _, err := o.output(ctx, "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return nil
	}
	log.Printf("拉取镜像 %s", image)
	args := []string{"pull", "-q"}
	if o.platform != "" {
		args = append(args, "--platform", o.platform)
	}
	_, err := o.output(ctx, append(args, image)...)
	return err
}

// prepareImage 读取镜像的 PATH（供 basePath 使用）和默认用户（供 cacheDomain 使用），需要在计算环境变量之前调用。
func (o *ociExec) prepareImage(ctx context.Context, log *runnerkit.LogShipper) error {
	if err := o.ensureImage(ctx, o.image, log); err != nil {
		return err
	}
	u, err := o.output(ctx, "image", "inspect", "--format", "{{.Config.User}}", o.image)
	if err != nil {
		return err
	}
	o.imageUser = u
	env, err := o.output(ctx, "image", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", o.image)
	if err != nil {
		return err
	}
	for l := range strings.SplitSeq(env, "\n") {
		if v, ok := strings.CutPrefix(l, "PATH="); ok {
			o.imagePath = v
		}
	}
	return nil
}

func (o *ociExec) prepare(ctx context.Context, log *runnerkit.LogShipper) error {
	net := o.networkMode()
	if len(o.services) > 0 {
		if err := o.startServices(ctx, log); err != nil {
			return err
		}
		net = o.netName
	}
	args := []string{
		"run", "-d", "--name", o.name, "--label", o.label, "--network", net, "-w", ociWorkspace,
		"-v", o.d.ws + ":" + ociWorkspace, "-v", o.d.home + ":" + ociHome, "-v", o.d.tmp + ":" + ociTmp,
	}
	if o.buildkitImage != "" {
		if err := o.startBuildkit(ctx, log); err != nil {
			return err
		}
		_, vol := o.buildkitNames()
		args = append(args, "-v", vol+":"+buildkitSocketDir)
	}
	args = append(args, o.limitArgs()...)
	for _, m := range o.mounts {
		args = append(args, "-v", m[0]+":"+m[1]+":ro")
	}
	for _, m := range o.rwMounts {
		args = append(args, "-v", m[0]+":"+m[1])
	}
	if o.gitSockDir != "" {
		args = append(args, "-v", o.gitSockDir+":"+ociGitDir, "-v", o.gitExe+":"+ociKilnRunner+":ro")
	}
	args = append(args, "--entrypoint", "sleep", o.image, "infinity")
	const marker = ".kiln-mount-check"
	if err := os.WriteFile(filepath.Join(o.d.tmp, marker), nil, 0o644); err != nil {
		return err
	}
	o.started = append(o.started, o.name)
	if _, err := o.output(ctx, args...); err != nil {
		return fmt.Errorf("启动容器：%w", err)
	}
	// 检查宿主机目录确实挂载进了容器（colima / Docker Desktop 只共享部分目录）
	if _, err := o.output(ctx, "exec", o.name, "test", "-f", ociTmp+"/"+marker); err != nil {
		return fmt.Errorf("docker 看不到 Runner 的数据目录 %s（colima 默认只共享 $HOME；可以把 %s 放到 $HOME 下，或在 colima 的 mounts 中加入该目录）",
			filepath.Dir(filepath.Dir(o.d.root)), Program.DirEnv)
	}
	if err := os.Remove(filepath.Join(o.d.tmp, marker)); err != nil {
		return err
	}
	return o.checkGit(ctx, log)
}

// checkGit 在容器中检查 git 凭据通道：连一次 socket（kiln-runner git-socket-check），再试建一次 git exec 镜像（容器中有 git
// 时，缺少要用的命令等会在这里发现）。不行时按 gitUnavailable 处理：不退回到写文件。
func (o *ociExec) checkGit(ctx context.Context, log *runnerkit.LogShipper) error {
	if o.git == nil || o.git.socket == "" {
		return nil
	}
	// 两步准备共用一个总期限（不用整个 job 的）：Runner 一侧迟迟不 accept、镜像中的 git 或文件操作卡住时按通道不可用
	// 处理，不需要 git 的 job 照常执行；job 本身被取消时按取消返回
	cctx, cancel := context.WithTimeout(ctx, o.gitCheckTimeout)
	defer cancel()
	if _, err := o.output(cctx, "exec", o.name, ociKilnRunner, gitSocketCheckCmd, o.git.socket); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return o.gitUnavailable(fmt.Sprintf("容器中连不上 kiln-runner 的 git 凭据通道：%v", err), log)
	}
	script := ociGitSetupScript() + `[ -z "$` + gitOpErrorEnv + `" ] || { printf '%s\n' "$` + gitOpErrorEnv + `" >&2; exit 3; }`
	if _, err := o.output(cctx, "exec", o.name, "sh", "-c", script); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return o.gitUnavailable(fmt.Sprintf("容器中不能建 git exec 镜像：%v", err), log)
	}
	return nil
}

// startServices 创建 job 专用的网络，启动 service 容器并等待它们变为 healthy。
func (o *ociExec) startServices(ctx context.Context, log *runnerkit.LogShipper) error {
	args := []string{"network", "create", "--label", o.label}
	if !o.network {
		args = append(args, "--internal")
	}
	// 先登记再创建：即使中途被取消，destroy 也会清理
	o.netName = o.name
	if _, err := o.output(ctx, append(args, o.netName)...); err != nil {
		return err
	}
	for _, s := range o.services {
		if err := o.ensureImage(ctx, s.Image, log); err != nil {
			return err
		}
		cname := o.name + "-" + s.Name
		args := []string{"run", "-d", "--name", cname, "--label", o.label, "--network", o.netName, "--network-alias", s.Name}
		for _, kv := range envList(s.Env) {
			args = append(args, "-e", kv)
		}
		args = append(args, s.Image)
		args = append(args, s.Command...)
		// 先登记再启动：即使 docker run 中途被取消，destroy 也会清理
		o.started = append(o.started, cname)
		if _, err := o.output(ctx, args...); err != nil {
			return fmt.Errorf("启动 service %s：%w", s.Name, err)
		}
		log.Printf("service %s 已启动（%s）", s.Name, s.Image)
	}
	for _, s := range o.services {
		if err := o.waitHealthy(ctx, o.name+"-"+s.Name); err != nil {
			return fmt.Errorf("service %s：%w", s.Name, err)
		}
	}
	return nil
}

// waitHealthy 等待容器变为 healthy（没有健康检查时只要求在运行）。
func (o *ociExec) waitHealthy(ctx context.Context, cname string) error {
	err := pollUntil(ctx, serviceHealthTimeout, serviceHealthPoll, func() (bool, error) {
		st, err := o.output(ctx, "inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", cname)
		if err != nil {
			return false, err
		}
		f := strings.Fields(st)
		if len(f) != 2 {
			return false, nil
		}
		switch {
		case f[0] != "running":
			return false, fmt.Errorf("容器已退出（%s）：\n%s", f[0], o.tailLogs(ctx, cname))
		case f[1] == "none" || f[1] == "healthy":
			return true, nil
		}
		return false, nil
	})
	if errors.Is(err, errPollTimeout) {
		return fmt.Errorf("%s 内没有变为 healthy", serviceHealthTimeout)
	}
	return err
}

// tailLogs 返回容器最后 errorLogLines 行日志，只用于错误信息（读取失败时为空）。
// docker logs 把容器的 stdout、stderr 分别输出到自己的 stdout、stderr，两者都要。
func (o *ociExec) tailLogs(ctx context.Context, cname string) string {
	out, _ := exec.CommandContext(ctx, o.docker, "logs", "--tail", strconv.Itoa(errorLogLines), cname).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// shQuote 把 s 写成 sh 的单引号字符串（sh、bash 通用）。
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// exportScript 生成设置环境变量的 sh 脚本；变量名不合法时返回用户错误（否则会被 sh 当作命令执行）。
func exportScript(env map[string]string) (string, error) {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if !workflow.EnvNameRe.MatchString(k) {
			return "", userErrorf("环境变量名 %q 无效（只能包含字母、数字和下划线，且不能以数字开头）", k)
		}
		b.WriteString("export " + k + "=" + shQuote(env[k]) + "\n")
	}
	return b.String(), nil
}

func (o *ociExec) run(ctx context.Context, s stepSpec, out io.Writer) (int, error) {
	if err := checkWorkDir(o.d.ws, s.workDir); err != nil {
		return -1, err
	}
	// 环境变量写进文件再 source，避免 secret 出现在 docker 的命令行参数中
	exports, err := exportScript(s.env)
	if err != nil {
		return -1, err
	}
	file := filepath.Join(o.d.tmp, stepScriptName(s.index))
	envFile := filepath.Join(o.d.tmp, stepEnvName(s.index))
	// 脚本中可能替换进了 secret：只给属主权限（容器以 root 运行，见 ociExec 的说明）
	if err := os.WriteFile(file, []byte(s.script), 0o700); err != nil {
		return -1, err
	}
	defer func() { _ = os.Remove(file) }() // 整个 attempt 目录最后会删除
	if err := os.WriteFile(envFile, []byte(exports), 0o600); err != nil {
		return -1, err
	}
	defer func() { _ = os.Remove(envFile) }() // 正常情况下容器中已经删除
	argv, err := shellCommand(s.shell, path.Join(ociTmp, filepath.Base(file)), true)
	if err != nil {
		return -1, err
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shQuote(a)
	}
	inEnv := path.Join(ociTmp, filepath.Base(envFile))
	// 先清掉父 shell 中镜像 ENV 可能带着的外层包装与凭据通道私有变量，再 source 本次 step 的环境变量文件
	// （Go 侧已经清过 workflow / step 的 env，只带入本次合法的 KILN_GIT_SOCKET 和暂存的 git 配置）。镜像的 ENV 中可能有
	// askpass：不给凭据时不能回落到它（gitenv.go），source 之后同样 unset。
	wrapper := ociGitResetScript()
	wrapper += ". " + inEnv + " && rm -f " + inEnv + " || exit 1\nunset GIT_ASKPASS SSH_ASKPASS\n"
	if _, ok := s.env[ociGitConfigN]; ok {
		wrapper += ociGitConfigScript()
	}
	if o.git != nil && o.git.socket != "" {
		wrapper += ociGitSetupScript()
	}
	wrapper += "exec " + strings.Join(quoted, " ")
	//nolint:noctx // 取消由 waitCmd 处理：先 docker kill 结束容器中的全部进程，宽限期后再结束 docker exec
	cmd := exec.Command(o.docker, "exec", "-w", path.Join(ociWorkspace, s.workDir), o.name, "sh", "-c", wrapper)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = pipeWaitDelay
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	stop := func() {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		// 停掉容器即可结束其中的全部进程；失败时由 kill 兜底
		_ = exec.CommandContext(cctx, o.docker, "kill", o.name).Run()
	}
	kill := func() { _ = cmd.Process.Kill() } // 进程可能已经退出
	werr := waitCmd(ctx, cmd, ociStopGrace, stop, kill)
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return exitStatus(werr)
}

func (o *ociExec) destroy(ctx context.Context) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	// 尽力清理：容器或网络可能没有创建成功
	if len(o.started) > 0 {
		_ = exec.CommandContext(ctx, o.docker, append([]string{"rm", "-f", "-v"}, o.started...)...).Run()
	}
	if o.netName != "" {
		_ = exec.CommandContext(ctx, o.docker, "network", "rm", o.netName).Run()
	}
	if len(o.volumes) > 0 {
		_ = exec.CommandContext(ctx, o.docker, append([]string{"volume", "rm", "-f"}, o.volumes...)...).Run()
	}
}

// imagesUsed 返回这个 job 用到的镜像（job、service 和 BuildKit）：Runner 记录最后使用的时间，
// 超过保留期没有用到的镜像在空闲时删除（gc.go）。
func (o *ociExec) imagesUsed() []string {
	images := []string{o.image}
	for _, s := range o.services {
		images = append(images, s.Image)
	}
	if o.buildkitImage != "" {
		images = append(images, o.buildkitImage)
	}
	return images
}

// cleanupRoot 用容器删除容器中以 root 身份写入的文件（Linux 上宿主机用户可能删不掉）。
// attempt 目录挂载到容器中，删除其中的全部子目录（见 attemptDirs）。
func (o *ociExec) cleanupRoot(ctx context.Context) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	const mnt = "/x"
	args := []string{"run", "--rm", "--label", o.label, "--network", dockerNetworkNone, "-v", o.d.root + ":" + mnt, "--entrypoint", "rm", o.image, "-rf"}
	for _, sub := range attemptSubdirs() {
		args = append(args, mnt+"/"+sub)
	}
	// 尽力清理：失败时调用方会记录删除工作区失败
	_ = exec.CommandContext(ctx, o.docker, args...).Run()
}

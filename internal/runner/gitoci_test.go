package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// OCI executor 的 git 凭据通道（设计 6.4）。真实容器的测试需要 Linux 上的 docker（与容器同一内核：socket 挂载、网桥地址；
// 别的平台和没有 docker 时跳过，跳过不算通过）和一个带 git 与 POSIX sh 的镜像：KILN_TEST_GIT_IMAGE，默认 alpine/git:2.47.2
// （本地没有时拉取）。每条 docker 命令有自己的期限（dockerTestTimeout）。不调用真实 docker 的测试在任何平台上运行：Runner
// 所在的操作系统经 ociExec.hostOS 注入，容器中运行的 kiln-runner 总是按 Linux 交叉构建。

// defaultTestGitImage 是真实容器测试默认用的镜像。
const defaultTestGitImage = "alpine/git:2.47.2"

// dockerTestTimeout 是测试中一条真实 docker 命令（含拉取镜像）的期限；ociJobTimeout 是真实容器中一个 job 的期限。
const (
	dockerTestTimeout = 5 * time.Minute
	ociJobTimeout     = 10 * time.Minute
)

// staticRunner 是 CGO_ENABLED=0、GOOS=linux 构建的 kiln-runner（容器中运行的就是它，在任何平台上都是 Linux 的 ELF），整个
// 测试进程构建一次，TestMain 结束时删除。
var staticRunner struct {
	once      sync.Once
	dir, path string
	err       error
}

// staticKilnRunner 返回静态链接的 kiln-runner（按需构建）。
func staticKilnRunner(t *testing.T) string {
	t.Helper()
	staticRunner.once.Do(func() {
		dir, err := os.MkdirTemp("", "kiln-runner-static-") //nolint:usetesting // 整个测试进程共用一份，TestMain 结束时删除
		if err != nil {
			staticRunner.err = err
			return
		}
		staticRunner.dir = dir
		staticRunner.path = filepath.Join(dir, "kiln-runner")
		cmd := exec.Command("go", "build", "-o", staticRunner.path, "../../cmd/kiln-runner")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := cmd.CombinedOutput(); err != nil {
			staticRunner.err = errors.New(string(out))
		}
	})
	if staticRunner.err != nil {
		t.Fatalf("构建静态链接的 kiln-runner：%v", staticRunner.err)
	}
	return staticRunner.path
}

// removeStaticRunner 删除 staticKilnRunner 构建的目录（TestMain 调用）。
func removeStaticRunner() {
	if staticRunner.dir != "" {
		_ = os.RemoveAll(staticRunner.dir)
	}
}

// linuxDocker 返回能用的 docker 命令：只在 Linux 上（容器与 Runner 同一内核：socket 能挂进容器、宿主机能监听网桥的地址；
// macOS 上的 Docker Desktop / colima 中容器在另一个 VM 里，这些前提都不成立，生产代码在那里按不可用处理，见
// TestOCIGitUnavailable 的 darwin 分支）。别的平台、没有 docker 时跳过。
func linuxDocker(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("真实容器的 git 凭据通道只在与容器同一内核的 Linux 上可用，%s 上跳过（不可用时的处理由注入 hostOS 的测试覆盖）", runtime.GOOS)
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("需要 docker")
	}
	if err := dockerCmd(t, bin, "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skipf("docker 不可用：%v", err)
	}
	return bin
}

// dockerCmd 返回一条有期限（dockerTestTimeout）的 docker 命令：docker 卡住时这一条命令失败，不把整个包拖到总超时。
func dockerCmd(t *testing.T, docker string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), dockerTestTimeout)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, docker, args...)
}

// dockerGateway 返回 docker 默认网桥的网关（宿主机在容器中的地址）。
func dockerGateway(t *testing.T, docker string) string {
	t.Helper()
	out, err := dockerCmd(t, docker, "network", "inspect", "bridge", "-f", "{{range .IPAM.Config}}{{.Gateway}}{{end}}").Output()
	gw := strings.TrimSpace(string(out))
	if err != nil || gw == "" {
		t.Skipf("读不到 docker 网桥的网关：%v", err)
	}
	return gw
}

// V2、V6、V7（OCI）：容器中的 git fetch 依赖仓库与本仓库各签一枚只读 token、传输后吊销；step 进行中容器的文件系统、
// 全部进程的环境变量、docker inspect 中都没有 token，结束后 Runner 的数据目录和日志中也没有。
func TestExecuteOCIGitChannel(t *testing.T) {
	t.Parallel()
	docker := linuxDocker(t)
	image := os.Getenv("KILN_TEST_GIT_IMAGE")
	if image == "" {
		image = defaultTestGitImage
	}
	fs := newFakeServer(t)
	g := newGitHTTP(t, fs, dockerGateway(t, docker)+":0")
	sha := g.addRepo(t, "o/r", map[string]string{"a.txt": "hello\n"})
	g.addRepo(t, "o/dep", map[string]string{"dep.txt": "dep\n"})
	r := newTestRunner(t, fs, func(c *Config) { c.Docker = docker })
	r.exe = staticKilnRunner(t)
	// docker export 一个大镜像要几十秒：其间偶尔慢一次的心跳不能让 job 停下（newTestRunner 的上限是 0）
	r.maxDisconnected = runnerMaxDisconnected
	step := &runnerv1.Step{Name: "fetch", Shell: "sh", Run: `git init -q /work/d && cd /work/d
git fetch -q "$DEP" main && test "$(git show FETCH_HEAD:dep.txt)" = dep
git fetch -q "$SELF" main && test "$(git show FETCH_HEAD:a.txt)" = hello
env > /work/env.txt
touch /work/ready
while [ ! -e /work/go ]; do sleep 0.1; done`, Env: map[string]string{"DEP": g.url("o/dep"), "SELF": g.url("o/r")}}
	spec := hostJob(71, step)
	spec.Executor = &runnerv1.ExecutorSpec{Kind: "oci", Image: image, Platform: runnerkit.NativePlatform()}
	spec.Network = true
	spec.Source = g.source(1, "o/r", sha)
	spec.Repositories = []string{"o/dep"}
	fs.register(spec)
	// 检查点一：依赖仓库的传输进行中（带凭据的请求停在 git HTTP 服务上，token 在容器中 git 进程的内存里）
	inFlight := make(chan struct{}, 1)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	var holdOnce sync.Once
	g.setHold(func(_ *http.Request, repo string) {
		if repo == "o/dep" {
			holdOnce.Do(func() {
				inFlight <- struct{}{}
				<-resume
			})
		}
	})
	done := make(chan struct{})
	jobCtx, cancelJob := context.WithTimeout(t.Context(), ociJobTimeout)
	defer cancelJob()
	go func() {
		defer close(done)
		r.newJob(spec).run(jobCtx)
	}()
	name := "kiln-71"
	select {
	case <-inFlight:
	case <-done:
		got, log := completedJob(fs, 71)
		t.Fatalf("job 在传输之前结束：%v；日志：\n%s", got, logTail(log))
	case <-time.After(testWait):
		t.Fatal("等传输进行中的检查点超时")
	}
	live := issuedTokens(fs)
	if len(live) != 2 {
		t.Errorf("传输进行中签发了 %d 枚 token，期望 2 枚（拉取源码、依赖）", len(live))
	}
	checkContainerNoTokens(t, docker, name, live, "o/dep.git")
	if hits := scanContainerFS(t, docker, name, live); len(hits) > 0 {
		t.Errorf("传输进行中容器的文件系统中有 token：%v", hits)
	}
	if hits := scanTreeForTokens(t, r.workDir(71), live); len(hits) > 0 {
		t.Errorf("传输进行中挂进容器的目录中有 token：%v", hits)
	}
	resumeOnce.Do(func() { close(resume) })
	ws := filepath.Join(r.workDir(71), attemptWorkspaceDir)
	err := pollUntil(t.Context(), testWait, 50*time.Millisecond, func() (bool, error) {
		select {
		case <-done:
			return false, errors.New("job 提前结束")
		default:
		}
		_, err := os.Stat(filepath.Join(ws, "ready"))
		return err == nil, nil
	})
	if err != nil {
		got, log := completedJob(fs, 71)
		t.Fatalf("等 step 到达检查点：%v；结果 %v，日志：\n%s", err, got, logTail(log))
	}
	tokens := issuedTokens(fs)
	if len(tokens) != 3 {
		t.Errorf("签发了 %d 枚 token，期望 3 枚（拉取源码、step 中的依赖和本仓库）", len(tokens))
	}
	env, err := os.ReadFile(filepath.Join(ws, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"GIT_EXEC_PATH=" + ociGitExecPrefix, gitSocketEnv + "=" + ociGitDir + "/", gitRealExecEnv + "="} {
		if !strings.Contains(string(env), s) {
			t.Errorf("容器中 step 的环境中没有 %q：\n%s", s, env)
		}
	}
	// 检查点二：两次传输结束之后
	checkContainerNoTokens(t, docker, name, tokens, "")
	select {
	case <-done:
		got, log := completedJob(fs, 71)
		t.Fatalf("检查期间 job 提前结束：%v；日志：\n%s", got, logTail(log))
	default:
	}
	if hits := scanTreeForTokens(t, r.workDir(71), tokens); len(hits) > 0 {
		t.Errorf("挂进容器的工作区、HOME、TMPDIR 中有 token：%v", hits)
	}
	mustWrite(t, filepath.Join(ws, "go"), "")
	waitFor(t, done, "job 结束")
	got, log := completedJob(fs, 71)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	for _, o := range fs.gitOpStates() {
		if !o.revoked {
			t.Errorf("op %s（%s）的 token 没有吊销", o.op, o.repo)
		}
	}
	if n, m := len(opsFor(fs, "o/r")), len(opsFor(fs, "o/dep")); n != 2 || m != 1 {
		t.Errorf("o/r %d 个操作（期望 2），o/dep %d 个（期望 1）", n, m)
	}
	if found := scanTokens([]byte(log), tokens); len(found) > 0 {
		t.Errorf("日志中有 token")
	}
	if hits := scanTreeForTokens(t, r.cfg.Dir, tokens); len(hits) > 0 {
		t.Errorf("Runner 的数据目录中有 token：%v", hits)
	}
}

// checkContainerNoTokens 检查 docker inspect 和容器中全部进程的环境变量、命令行参数中没有 tokens。want 非空时，读到的进程
// 信息中必须有它（例如传输中的远程助手的参数），证明确实读到了正在传输的进程。
func checkContainerNoTokens(t *testing.T, docker, name string, tokens []string, want string) {
	t.Helper()
	inspect, err := dockerCmd(t, docker, "inspect", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	if found := scanTokens(inspect, tokens); len(found) > 0 {
		t.Errorf("docker inspect 中有 token")
	}
	// 读不了的进程（读的时候已经退出）跳过；每个进程的 environ 和 cmdline 都读
	procs, err := dockerCmd(t, docker, "exec", name, "sh", "-c",
		`n=0; for p in /proc/[0-9]*; do cat "$p/environ" "$p/cmdline" 2>/dev/null && n=$((n+1)); echo; done; echo "procs=$n"`).Output()
	if err != nil {
		t.Fatalf("读容器中的进程：%v", err)
	}
	if len(scanTokens(procs, tokens)) > 0 {
		t.Errorf("容器中进程的环境变量或参数中有 token")
	}
	if want != "" && !bytes.Contains(procs, []byte(want)) {
		t.Errorf("容器中进程的参数中没有 %q：没有读到正在传输的进程", want)
	}
	if bytes.Contains(procs, []byte("procs=0\n")) {
		t.Error("一个进程都没有读到")
	}
}

// scanContainerFS 用 docker export 读出容器的文件系统（不含挂载的宿主机目录），返回内容中有 token 的文件。
func scanContainerFS(t *testing.T, docker, name string, tokens []string) []string {
	t.Helper()
	out, err := dockerCmd(t, docker, "export", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if len(scanTokens(b, tokens)) > 0 {
			hits = append(hits, h.Name)
		}
	}
	return hits
}

// 通道在容器中不可用（连通检查失败）：声明了 repositories 的 job 失败；其余 job 照常，helper 只说明原因。挂载 socket 目录
// （读写）和 kiln-runner（只读）。Runner 所在的系统注入为 linux（与运行平台无关，docker 是脚本替身）。
func TestOCIGitUnavailable(t *testing.T) {
	t.Parallel()
	exe := staticKilnRunner(t)
	inner, calls := fakeDocker(t)
	bin := filepath.Join(t.TempDir(), "docker")
	mustWrite(t, bin, "#!/bin/sh\ncase \"$*\" in *"+gitSocketCheckCmd+"*) printf '%s\\n' \"$*\" >> '"+calls+"'; echo 'connect: permission denied' >&2; exit 1 ;; esac\nexec '"+inner+"' \"$@\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sockDir := t.TempDir()
	for _, required := range []bool{true, false} {
		o := newOCIExec(bin, newTestDirs(t), ociSpec(), testRunnerID)
		o.hostOS = "linux"
		p := gitPlan{scope: "https://github.com", sockDir: sockDir, sock: filepath.Join(sockDir, gitSocketName), exe: exe, required: required}
		if err := o.useGit(t.Context(), p, log); err != nil {
			t.Fatal(err)
		}
		err := o.prepare(t.Context(), log)
		if required {
			if !errors.Is(err, errGitUnavailable) || !strings.Contains(err.Error(), "连不上") {
				t.Errorf("声明了 repositories：err = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		a := o.gitAccess(t.Context(), nil, "", log)
		if a.socket != "" || !strings.Contains(a.helper, "连不上") {
			t.Errorf("没有声明 repositories：%+v", a)
		}
		// oci 的 git 配置交给 step 的启动脚本接续在镜像的环境变量配置之后，不直接写 GIT_CONFIG_COUNT（会覆盖镜像的）
		env := map[string]string{}
		a.apply(env)
		if _, ok := env["GIT_CONFIG_COUNT"]; ok || env[ociGitConfigN] != "4" {
			t.Errorf("oci 的 git 配置：%v", env)
		}
	}
	all := strings.Join(dockerCalls(t, calls), "\n")
	for _, s := range []string{"-v " + sockDir + ":" + ociGitDir, "-v " + exe + ":" + ociKilnRunner + ":ro", "exec kiln-5 " + ociKilnRunner + " " + gitSocketCheckCmd + " " + ociGitDir + "/" + gitSocketName} {
		if !strings.Contains(all, s) {
			t.Errorf("docker 调用中没有 %q：\n%s", s, all)
		}
	}
}

// Runner 不在 Linux 上（注入 hostOS=darwin，与运行平台无关）：启动容器之前就判定通道不可用，不挂载 socket 目录和
// kiln-runner、不做连通检查；声明了 repositories 的 job 明确失败，其余 job 照常运行，helper 说明原因。
func TestOCIGitUnavailableNonLinux(t *testing.T) {
	t.Parallel()
	exe := staticKilnRunner(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sockDir := t.TempDir()
	for _, required := range []bool{true, false} {
		bin, calls := fakeDocker(t)
		o := newOCIExec(bin, newTestDirs(t), ociSpec(), testRunnerID)
		o.hostOS = "darwin"
		p := gitPlan{scope: "https://github.com", sockDir: sockDir, sock: filepath.Join(sockDir, gitSocketName), exe: exe, required: required}
		err := o.useGit(t.Context(), p, log)
		if required {
			if !errors.Is(err, errGitUnavailable) || !strings.Contains(err.Error(), "darwin") {
				t.Errorf("声明了 repositories：err = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := o.prepare(t.Context(), log); err != nil {
			t.Fatal(err)
		}
		if a := o.gitAccess(t.Context(), nil, "", log); a.socket != "" || !strings.Contains(a.helper, "darwin") {
			t.Errorf("没有声明 repositories：%+v", a)
		}
		all := strings.Join(dockerCalls(t, calls), "\n")
		for _, s := range []string{sockDir, exe, gitSocketCheckCmd} {
			if strings.Contains(all, s) {
				t.Errorf("通道不可用时 docker 调用中不应有 %q：\n%s", s, all)
			}
		}
	}
}

// kiln-runner 不是静态链接的、不是 ELF 时不能放进容器；Runner 不在 Linux 上时一律不能。hostOS 注入，ELF 由测试写出，
// 两种系统的结论在任何平台上都能测到。
func TestContainerRunnerProblem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	static, dynamic, notELF := filepath.Join(dir, "static"), filepath.Join(dir, "dynamic"), filepath.Join(dir, "x")
	writeTestELF(t, static, false)
	writeTestELF(t, dynamic, true)
	mustWrite(t, notELF, "#!/bin/sh\n")
	built := staticKilnRunner(t)
	for _, c := range []struct {
		goos, exe, want string
	}{
		{"linux", built, ""},
		{"linux", static, ""},
		{"linux", dynamic, "不是静态链接"},
		{"linux", notELF, "读不了"},
		{"darwin", built, "darwin"},
		{"windows", static, "windows"},
	} {
		got := containerRunnerProblem(c.goos, c.exe)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("containerRunnerProblem(%s, %s) = %q，期望含 %q", c.goos, filepath.Base(c.exe), got, c.want)
		}
	}
}

// writeTestELF 写一个只有 ELF 头和一个程序头的文件：interp 为 true 时是 PT_INTERP（动态链接的程序都有），否则是 PT_LOAD。
func writeTestELF(t *testing.T, path string, interp bool) {
	t.Helper()
	typ := elf.PT_LOAD
	if interp {
		typ = elf.PT_INTERP
	}
	h := elf.Header64{
		Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Phoff: 64, Ehsize: 64, Phentsize: 56, Phnum: 1, Shentsize: 64,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var b bytes.Buffer
	if err := binary.Write(&b, binary.LittleEndian, h); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&b, binary.LittleEndian, elf.Prog64{Type: uint32(typ), Flags: uint32(elf.PF_R)}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, b.String())
}

// 通道不可用时的 helper：git 要凭据时直接失败，原因写到 stderr。
func TestGitRefusingHelper(t *testing.T) {
	t.Parallel()
	env, _ := gitTestEnv(t, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0="+gitRefusingHelper("通道不可用 'x'"))
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env, cmd.Dir = env, t.TempDir()
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=o/r.git\n\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil || strings.Contains(stdout.String(), "password=") {
		t.Fatalf("不应给出凭据：%v\n%s", err, stdout.String())
	}
	if !strings.Contains(stderr.String(), "kiln-runner：通道不可用 'x'") {
		t.Errorf("stderr 中没有原因：%s", stderr.String())
	}
}

// docker 启动容器失败按启动失败处理：原样报告 docker 的错误，不推断是凭据通道的挂载问题、不再启动一次。Runner 所在的系统
// 注入为 linux（与运行平台无关，docker 是脚本替身）。
func TestOCIGitStartFailure(t *testing.T) {
	t.Parallel()
	exe := staticKilnRunner(t)
	inner, calls := fakeDocker(t)
	bin := filepath.Join(t.TempDir(), "docker")
	mustWrite(t, bin, "#!/bin/sh\nif [ \"$1\" = run ]; then printf '%s\\n' \"$*\" >> '"+calls+"'; echo 'temporary network setup failure' >&2; exit 125; fi\nexec '"+inner+"' \"$@\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sockDir := t.TempDir()
	o := newOCIExec(bin, newTestDirs(t), ociSpec(), testRunnerID)
	o.hostOS = "linux"
	p := gitPlan{scope: "https://github.com", sockDir: sockDir, sock: filepath.Join(sockDir, gitSocketName), exe: exe}
	if err := o.useGit(t.Context(), p, log); err != nil {
		t.Fatal(err)
	}
	err := o.prepare(t.Context(), log)
	if err == nil || errors.Is(err, errGitUnavailable) || !strings.Contains(err.Error(), "temporary network setup failure") {
		t.Errorf("err = %v", err)
	}
	if a := o.gitAccess(t.Context(), nil, "", log); a.socket == "" {
		t.Errorf("启动失败不应把通道判为不可用：%+v", a)
	}
	runs := 0
	for _, c := range dockerCalls(t, calls) {
		if strings.HasPrefix(c, "run ") {
			runs++
		}
	}
	if runs != 1 {
		t.Errorf("docker run 调用了 %d 次，期望 1 次", runs)
	}
}

// 容器中建 git exec 镜像的脚本（POSIX sh）：完整时导出镜像；没有 git 时什么都不做；缺少 cksum、ln 时不导出（也不发布不完整的
// 目录），而是导出原因。在真实容器中以只有部分命令的 PATH 运行。
func TestOCIGitSetupScriptMinimalPath(t *testing.T) {
	t.Parallel()
	docker := linuxDocker(t)
	image := os.Getenv("KILN_TEST_GIT_IMAGE")
	if image == "" {
		image = defaultTestGitImage
	}
	tools := append([]string{"git", "sh"}, ociGitTools...)
	for _, c := range []struct {
		name, drop, wantErr string
		// stub 是换成"总是失败"的命令（命令在，但执行失败）
		stub     string
		exported bool
	}{
		{name: "完整", exported: true},
		{name: "没有 git", drop: "git"},
		{name: "没有 cksum", drop: "cksum", wantErr: "没有 cksum"},
		{name: "没有 ln", drop: "ln", wantErr: "没有 ln"},
		{name: "ln 失败", stub: "ln", wantErr: "ln -s 失败"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var links strings.Builder
			for _, tool := range tools {
				switch tool {
				case c.drop:
				case c.stub:
					links.WriteString("printf '#!/bin/sh\\nexit 1\\n' > /m/" + tool + " && chmod +x /m/" + tool + "\n")
				default:
					links.WriteString(`ln -s "$(command -v ` + tool + `)" /m/` + tool + "\n")
				}
			}
			script := "set -e\nmkdir -p /m " + ociTmp + "\n" + links.String() + "set +e\n" +
				"(PATH=/m; " + ociGitSetupScript() +
				`printf 'EXEC=%s\nERR=%s\n' "$GIT_EXEC_PATH" "$` + gitOpErrorEnv + `")` + "\n" +
				`echo "DIRS=$(ls -d ` + ociGitExecPrefix + `* 2>/dev/null | wc -l)"` + "\n" +
				`for d in ` + ociGitExecPrefix + `*; do [ -d "$d" ] && echo "HELPER=$(readlink "$d/git-remote-https")"; done; true`
			out, err := dockerCmd(t, docker, "run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := string(out)
			exported := !strings.Contains(got, "EXEC=\n")
			switch {
			case exported != c.exported:
				t.Errorf("GIT_EXEC_PATH 导出 = %t，期望 %t：\n%s", exported, c.exported, got)
			case c.exported && (!strings.Contains(got, "EXEC="+ociGitExecPrefix) || !strings.Contains(got, "HELPER="+ociKilnRunner) || !strings.Contains(got, "DIRS=1")):
				t.Errorf("镜像不完整：\n%s", got)
			case !c.exported && !strings.Contains(got, "DIRS=0"):
				t.Errorf("不应发布镜像目录：\n%s", got)
			case c.wantErr == "" && !strings.Contains(got, "ERR=\n"):
				t.Errorf("不应有原因：\n%s", got)
			case c.wantErr != "" && !strings.Contains(got, c.wantErr):
				t.Errorf("原因中没有 %q：\n%s", c.wantErr, got)
			}
		})
	}
}

// 容器中的探测（git-socket-check）连上一个在监听、但一直不 accept 的 socket（Runner 的 accept 在退避）：在自己的期限内
// 失败返回，不一直等。
func TestGitSocketCheckNoReply(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() // 只监听，不 accept
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- gitSocketCheck([]string{sock}, io.Discard) }()
	limit := gitDialTimeout + 2*gitPingTimeout + 5*time.Second
	select {
	case code := <-done:
		if code == 0 {
			t.Error("没有回复时探测不应成功")
		}
		if took := time.Since(start); took < gitPingTimeout/2 {
			t.Errorf("测试前提：应等到读回复的期限，实际 %v", took)
		}
	case <-time.After(limit):
		t.Fatalf("探测在 %v 内没有返回", limit)
	}
}

// Runner 在容器中执行的探测迟迟不结束（docker exec 卡住）：到探测自己的总期限按通道不可用处理——没有声明 repositories 的
// job 继续，声明了的以明确的准备错误结束；都不等整个 job 的时限。
func TestOCIGitCheckTimeout(t *testing.T) {
	t.Parallel()
	exe := staticKilnRunner(t)
	inner, _ := fakeDocker(t)
	bin := filepath.Join(t.TempDir(), "docker")
	mustWrite(t, bin, "#!/bin/sh\ncase \"$*\" in *"+gitSocketCheckCmd+"*) exec sleep 600 ;; esac\nexec '"+inner+"' \"$@\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sockDir := t.TempDir()
	for _, required := range []bool{true, false} {
		o := newOCIExec(bin, newTestDirs(t), ociSpec(), testRunnerID)
		o.hostOS = "linux"
		o.gitCheckTimeout = 300 * time.Millisecond
		p := gitPlan{scope: "https://github.com", sockDir: sockDir, sock: filepath.Join(sockDir, gitSocketName), exe: exe, required: required}
		if err := o.useGit(t.Context(), p, log); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- o.prepare(t.Context(), log) }()
		var err error
		select {
		case err = <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("required=%v：探测卡住时 prepare 没有在期限内返回", required)
		}
		if required {
			if !errors.Is(err, errGitUnavailable) || !strings.Contains(err.Error(), "连不上") {
				t.Errorf("声明了 repositories：err = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if a := o.gitAccess(t.Context(), nil, "", log); a.socket != "" || !strings.Contains(a.helper, "连不上") {
			t.Errorf("没有声明 repositories：%+v", a)
		}
	}
}

// dialGit 的期限只管连上、发出请求、读到第一条回复：之后清除，open 的连接保持到传输结束（超过期限后仍可读写）。
func TestDialGitClearsDeadline(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		var m gitMsg
		if readGitMsg(br, &m) != nil || writeGitMsg(conn, gitReply{Op: "op"}) != nil {
			return
		}
		line, _ := br.ReadString('\n')
		got <- line
	}()
	const wait = 100 * time.Millisecond
	conn, _, r, err := dialGit(sock, gitMsg{Cmd: gitCmdOpen}, wait)
	if err != nil || r.Op != "op" {
		t.Fatalf("dialGit：%+v %v", r, err)
	}
	defer conn.Close()
	time.Sleep(3 * wait)
	if _, err := conn.Write([]byte("later\n")); err != nil {
		t.Fatalf("期限过后连接应仍可写：%v", err)
	}
	if line := <-got; line != "later\n" {
		t.Errorf("对端读到 %q", line)
	}
}

// checkGit 的第一步（探测）立即成功、第二步（试建 git exec 镜像）卡住：两步共用的总期限到了按通道不可用处理——没有声明
// repositories 的 job 继续，声明了的以明确的准备错误结束；job 本身在准备中被取消时按取消返回，不当成通道不可用。
func TestOCIGitSetupTimeout(t *testing.T) {
	t.Parallel()
	exe := staticKilnRunner(t)
	inner, _ := fakeDocker(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	ready := filepath.Join(dir, "setup-ready")
	mustWrite(t, bin, "#!/bin/sh\ncase \"$*\" in *"+gitSocketCheckCmd+"*) exit 0 ;; *"+ociGitSetupFunc+"*) : > "+shQuote(ready)+"; exec sleep 600 ;; esac\nexec "+shQuote(inner)+" \"$@\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	sockDir := t.TempDir()
	startPrepare := func(ctx context.Context, required bool, timeout time.Duration) (*ociExec, <-chan error) {
		_ = os.Remove(ready)
		o := newOCIExec(bin, newTestDirs(t), ociSpec(), testRunnerID)
		o.hostOS = "linux"
		o.gitCheckTimeout = timeout
		p := gitPlan{scope: "https://github.com", sockDir: sockDir, sock: filepath.Join(sockDir, gitSocketName), exe: exe, required: required}
		if err := o.useGit(t.Context(), p, log); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- o.prepare(ctx, log) }()
		return o, done
	}
	waitPrepare := func(done <-chan error, label string) error {
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatalf("%s：试建镜像卡住时 prepare 没有在期限内返回", label)
			return nil
		}
	}
	for _, required := range []bool{true, false} {
		// 第一步（探测）只是空 shell 立即退出，但全量 -race 负载高时子进程启动可能耗时数百毫秒：
		// 给足余量让两步共用的期限稳定落在第二步（试建镜像），而非第一步。
		o, done := startPrepare(t.Context(), required, 1500*time.Millisecond)
		err := waitPrepare(done, "required="+strconv.FormatBool(required))
		if required {
			if !errors.Is(err, errGitUnavailable) || !strings.Contains(err.Error(), "不能建") {
				t.Errorf("声明了 repositories：err = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if a := o.gitAccess(t.Context(), nil, "", log); a.socket != "" || !strings.Contains(a.helper, "不能建") {
			t.Errorf("没有声明 repositories：%+v", a)
		}
	}
	// job 本身在试建镜像期间被取消（准备的期限远没到）：先等假 docker 确认进入目标探针再取消，
	// 避免把前面的容器启动和挂载检查耗时算进取消时机；按取消返回，通道不降级。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o, done := startPrepare(ctx, false, time.Hour)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("进入试建镜像之前 prepare 已提前结束：%v", err)
		case <-deadline:
			cancel()
			t.Fatal("等待假 docker 进入试建镜像探针超时")
		case <-tick.C:
		}
	}
	cancel()
	err := waitPrepare(done, "job 取消")
	if !errors.Is(err, context.Canceled) || errors.Is(err, errGitUnavailable) {
		t.Errorf("job 被取消：err = %v", err)
	}
	if o.git.socket == "" {
		t.Error("job 被取消时不应把通道当成不可用")
	}
}

// 镜像的 ENV 中已有环境变量配置（用 docker run -e 模拟）：step 启动脚本把 Kiln 的项接续在它之后——镜像的配置仍可读，
// Kiln 清空 helper 的空值排在镜像的 helper 之后、Kiln 的 helper 最后生效；镜像的 GIT_CONFIG_COUNT 是 git 会拒绝的值时
// 什么都不改（git 照样以 bogus count 失败）。在真实容器中运行（镜像中的 sh）。
func TestOCIGitConfigImageEnv(t *testing.T) {
	t.Parallel()
	docker := linuxDocker(t)
	image := os.Getenv("KILN_TEST_GIT_IMAGE")
	if image == "" {
		image = defaultTestGitImage
	}
	env := map[string]string{}
	(&gitAccess{scope: "https://github.com", helper: "!kiln-helper", configInScript: true}).apply(env)
	run := func(count string) string {
		args := []string{
			"run", "--rm", "--entrypoint", "sh", "-e", "GIT_CONFIG_COUNT=" + count,
			"-e", "GIT_CONFIG_KEY_0=credential.https://github.com.helper", "-e", "GIT_CONFIG_VALUE_0=image-helper",
			"-e", "GIT_CONFIG_KEY_1=init.defaultBranch", "-e", "GIT_CONFIG_VALUE_1=trunk",
		}
		for _, kv := range envList(env) {
			if strings.HasPrefix(kv, "KILN_GIT_CONFIG") {
				args = append(args, "-e", kv)
			}
		}
		script := ociGitConfigScript() +
			`printf 'count=%s\n' "$GIT_CONFIG_COUNT"; git config --get-all credential.https://github.com.helper 2>&1; echo ---; git config --get init.defaultBranch 2>&1; true`
		out, err := dockerCmd(t, docker, append(args, image, "-c", script)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker run：%v\n%s", err, out)
		}
		return string(out)
	}
	if got, want := run("2"), "count=6\nimage-helper\n\n!kiln-helper\n---\ntrunk\n"; got != want {
		t.Errorf("镜像已有两项：\n%q\n期望：\n%q", got, want)
	}
	if got := run("2x"); !strings.HasPrefix(got, "count=2x\n") || !strings.Contains(got, "bogus count") {
		t.Errorf("镜像的 GIT_CONFIG_COUNT 无效：不应改动，git 照样失败：\n%s", got)
	}
}

// ociInitAllowed 是 step 启动脚本的初始化可以改动的变量（其余的都不能变）。
func ociInitAllowed() []string {
	names := []string{"GIT_CONFIG_COUNT", "GIT_EXEC_PATH", gitRealExecEnv, gitOpErrorEnv, ociGitConfigN}
	for i := range 16 {
		n := strconv.Itoa(i)
		names = append(names, "GIT_CONFIG_KEY_"+n, "GIT_CONFIG_VALUE_"+n, ociGitConfigKey+n, ociGitConfigValue+n)
	}
	return names
}

// 不变量：step 启动脚本的初始化（接续 git 配置、建 git exec 镜像）除了约定的变量，不改动 shell 中的任何变量（含导出的
// 环境）——workflow 的 env 中与脚本内部同名的变量（kiln_d、d、i、ok、out …）、IFS、带空格、换行、引号、$ 的值都逐字节
// 不变；set -eu 时同样成功。在真实容器中运行（golang 镜像的 /bin/sh 是 dash，alpine/git 是 busybox）。
func TestOCIInitInvariant(t *testing.T) {
	t.Parallel()
	docker := linuxDocker(t)
	image := os.Getenv("KILN_TEST_GIT_IMAGE")
	if image == "" {
		image = defaultTestGitImage
	}
	user := map[string]string{
		"PATH": "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "IFS": ":",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "wf",
		"spaced": "a  b\tc", "newline": "x\ny\n", "quoted": `it's "q" $HOME \n \\`, "empty": "",
	}
	for _, n := range []string{
		"kiln_c", "kiln_d", "kiln_i", "kiln_neg", "kiln_ok", "kiln_t", "kiln_r", "kiln_s", "kiln_f", "kiln_n",
		"c", "d", "i", "n", "e", "f", "o", "q", "r", "s", "t", "v", "ok", "neg", "out", "fail",
	} {
		user[n] = "user-" + n + " value\n"
	}
	env := maps.Clone(user)
	(&gitAccess{scope: "https://github.com", helper: "!kiln-helper", configInScript: true}).apply(env)
	exports, err := exportScript(env)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "env"), exports)
	unset := "unset " + strings.Join(ociInitAllowed(), " ")
	dump := "( " + unset + "; export -p; echo ===; set )"
	for _, opts := range []string{"", "set -eu"} {
		script := ". /u/env\n" + opts + "\n" + dump + " > /tmp/before\n" + ociGitConfigScript() + "mkdir -p " + ociTmp + "\n" + ociGitSetupScript() +
			dump + " > /tmp/after\n" +
			`if cmp -s /tmp/before /tmp/after; then echo SAME; else echo DIFF; diff /tmp/before /tmp/after || true; fi` + "\n" +
			`printf 'count=%s key1=%s exec=%s\n' "$GIT_CONFIG_COUNT" "$GIT_CONFIG_KEY_1" "${GIT_EXEC_PATH-}"; [ -z "${` + ociGitConfigN + `+x}" ] && echo CLEANED`
		out, err := dockerCmd(t, docker, "run", "--rm", "--network", "none", "-v", dir+":/u:ro", "--entrypoint", "sh", image, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%q：%v\n%s", opts, err, out)
		}
		got := string(out)
		if !strings.HasPrefix(got, "SAME\n") {
			t.Errorf("%q：初始化改动了约定之外的变量：\n%s", opts, got)
		}
		if !strings.Contains(got, "count=5 key1=credential.https://github.com.helper exec="+ociGitExecPrefix) || !strings.Contains(got, "CLEANED") {
			t.Errorf("%q：约定的变量不对：\n%s", opts, got)
		}
	}
}

// git --exec-path 给出的目录名以换行、空格结尾、含单引号或是普通的：启动脚本只去掉 git 的那一个换行，KILN_GIT_EXEC_REAL 与目录逐字节
// 相同，镜像中的链接指向其中的文件。在真实容器中运行（dash、busybox）。
func TestOCIGitSetupExecPathBytes(t *testing.T) {
	t.Parallel()
	docker := linuxDocker(t)
	image := os.Getenv("KILN_TEST_GIT_IMAGE")
	if image == "" {
		image = defaultTestGitImage
	}
	for name, suffix := range map[string]string{"普通": "", "以空格结尾": " ", "以换行结尾": `\n`, "含单引号": `\047q`} {
		script := "set -e\nwant=$(printf '/core" + suffix + "x'); want=${want%x}\n" +
			`mkdir -p "$want" /stub ` + ociTmp + "\n" + `printf 'real' > "$want/git-upload-pack"` + "\n" +
			`real=$(command -v git)` + "\n" +
			`{ printf '#!/bin/sh\nif [ "$1" = --exec-path ]; then printf "%%s\\n" "$KILN_TEST_CORE"; exit 0; fi\nexec %s "$@"\n' "$real"; } > /stub/git; chmod +x /stub/git` + "\n" +
			`export KILN_TEST_CORE="$want" PATH="/stub:$PATH"` + "\nset +e\n" + ociGitSetupScript() +
			`[ "$` + gitRealExecEnv + `" = "$want" ] && echo SAME; cat "$GIT_EXEC_PATH/git-upload-pack"; echo; printf 'err=%s\n' "${` + gitOpErrorEnv + `-}"`
		out, err := dockerCmd(t, docker, "run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s：%v\n%s", name, err, out)
		}
		if got := string(out); !strings.Contains(got, "SAME\nreal\nerr=\n") {
			t.Errorf("%s：\n%s", name, got)
		}
	}
}

// step 启动脚本开头的清理（ociGitResetScript）把父 shell 中由镜像 ENV 带来的外层包装 / 凭据通道私有变量清掉，包括任意编号的
// 暂存键；普通用户或镜像自己的 GIT_CONFIG_*、用户变量、多行普通变量与 IFS 都不受影响。
func TestOCIGitResetScript(t *testing.T) {
	t.Parallel()
	script := "export GIT_EXEC_PATH=/outer KILN_GIT_EXEC_REAL=/real KILN_GIT_OP=op KILN_GIT_OP_ERROR=err KILN_GIT_SOCKET=/sock " +
		"KILN_ORCA_GIT_OP=op2 GIT_ASKPASS=/ap SSH_ASKPASS=/sap KILN_GIT_CONFIG_N=4 " +
		"KILN_GIT_CONFIG_KEY_0=k KILN_GIT_CONFIG_VALUE_0=v KILN_GIT_CONFIG_KEY_3=k3 KILN_GIT_CONFIG_VALUE_3=v3 " +
		"KILN_GIT_CONFIG_KEY_7=k7 KILN_GIT_CONFIG_VALUE_7=v7 KILN_GIT_CONFIG_KEY_999=k999 KILN_GIT_CONFIG_VALUE_999=v999 " +
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name GIT_CONFIG_VALUE_0=wf " +
		"USER_VAR=keep KILN_OTHER=keep2 IFS=:\n" +
		// 普通变量的值里有换行，下一行长得像暂存键：只应多 unset 一个不存在的私有前缀名，MULTILINE 本身原样保留。
		"export MULTILINE='line1\nexport KILN_GIT_CONFIG_KEY_5=fake'\n" +
		ociGitResetScript() +
		`printf '%s\n' "${GIT_EXEC_PATH-}|${KILN_GIT_EXEC_REAL-}|${KILN_GIT_OP-}|${KILN_GIT_OP_ERROR-}|${KILN_GIT_SOCKET-}|${KILN_ORCA_GIT_OP-}|${GIT_ASKPASS-}|${SSH_ASKPASS-}|${KILN_GIT_CONFIG_N-}|${KILN_GIT_CONFIG_KEY_0-}|${KILN_GIT_CONFIG_VALUE_3-}|${KILN_GIT_CONFIG_KEY_7-}|${KILN_GIT_CONFIG_VALUE_7-}|${KILN_GIT_CONFIG_KEY_999-}|${KILN_GIT_CONFIG_VALUE_999-}|${GIT_CONFIG_COUNT-}|${GIT_CONFIG_KEY_0-}|${USER_VAR-}|${KILN_OTHER-}"
printf 'ML=%s\n' "$MULTILINE"
printf 'IFS=[%s]\n' "$IFS"`
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh：%v：%s", err, out)
	}
	want := strings.Join([]string{"", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "1", "user.name", "keep", "keep2"}, "|")
	got := string(out)
	if line, _, _ := strings.Cut(got, "\n"); line != want {
		t.Errorf("清理后 = %q\n期望 %q（GIT_CONFIG_*、用户变量应保持原样）", line, want)
	}
	if !strings.Contains(got, "ML=line1\nexport KILN_GIT_CONFIG_KEY_5=fake\n") {
		t.Errorf("多行普通变量被改动：\n%s", got)
	}
	if !strings.Contains(got, "IFS=[:]") {
		t.Errorf("IFS 被改动：\n%s", got)
	}
}

// 单独执行镜像准备（checkGit 的用法）：容器中没有 git、降级时父 shell 中不残留旧的 GIT_EXEC_PATH 和通道操作变量；
// KILN_GIT_SOCKET 是本次 step 合法的输入，保留。
func TestOCIGitSetupScriptClearsStale(t *testing.T) {
	t.Parallel()
	script := "export GIT_EXEC_PATH=/old KILN_GIT_EXEC_REAL=/oldreal KILN_GIT_OP=old KILN_GIT_OP_ERROR=olderr KILN_GIT_SOCKET=/oldsock KILN_ORCA_GIT_OP=oldorca\n" +
		"PATH=/nonexistent\n" + ociGitSetupScript() +
		`printf '%s,%s,%s,%s,%s,%s\n' "${GIT_EXEC_PATH-}" "${KILN_GIT_EXEC_REAL-}" "${KILN_GIT_OP-}" "${KILN_GIT_OP_ERROR-}" "${KILN_GIT_SOCKET-}" "${KILN_ORCA_GIT_OP-}"`
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh：%v：%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), ",,,,/oldsock,"; got != want {
		t.Errorf("降级后 = %q，期望 %q", got, want)
	}
}

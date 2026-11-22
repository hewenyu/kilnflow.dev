package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 这里的 docker 是 fakeDocker 生成的脚本：只记录参数，测试 oci executor 构造的 docker 命令和挂载，
// 不需要真正的 docker。

// testRunnerID 是测试中 Runner 的 ID：创建的容器、网络和 volume 带标签 dev.kiln.runner=7。
const testRunnerID = 7

// otherPlatform 是一定不等于本机平台的容器平台（会带上 --platform）。
const otherPlatform = "linux/s390x"

func ociSpec(services ...*runnerv1.Service) *runnerv1.JobSpec {
	return &runnerv1.JobSpec{
		Ref:       &runnerv1.AttemptRef{AttemptId: 5, Epoch: 1},
		Executor:  &runnerv1.ExecutorSpec{Kind: "oci", Image: "golang@sha256:abc", Platform: otherPlatform},
		Resources: &runnerv1.Resources{Cpu: 2, MemoryBytes: 1 << 30},
		Services:  services,
	}
}

// wantCalls 断言 docker 的调用记录中按顺序包含 want（中间可以有其他调用）。
func wantCalls(t *testing.T, calls, want []string) {
	t.Helper()
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	if i < len(want) {
		t.Fatalf("docker 调用中缺少：\n  %s\n实际调用：\n  %s", want[i], strings.Join(calls, "\n  "))
	}
}

// 有 services 的 job：创建专用网络（network: false 时为 --internal），启动 service 并等待 healthy，
// job 容器加入同一网络，runtime 只读挂载、toolcache 读写挂载；destroy 删除容器和网络。
func TestOCIExecPrepareWithServices(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	d := newTestDirs(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	o := newOCIExec(docker, d, ociSpec(&runnerv1.Service{Name: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "pw", "A": "1"}, Command: []string{"-c", "fsync=off"}}), testRunnerID)
	if err := o.prepareImage(t.Context(), log); err != nil {
		t.Fatal(err)
	}
	if o.basePath() != "/img/bin:/usr/bin" {
		t.Errorf("basePath 应取镜像的 PATH：%q", o.basePath())
	}
	if p := o.rtMount("go", "/host/rt/abc"); p != "/opt/kiln/rt/go" {
		t.Errorf("rtMount = %q", p)
	}
	if p := o.toolcacheMount("gomod", "/host/tc/gomod"); p != "/opt/kiln/toolcache/gomod" {
		t.Errorf("toolcacheMount = %q", p)
	}
	if o.workspace() != "/work" || o.home() != "/kiln/home" || o.tmp() != "/kiln/tmp" {
		t.Errorf("容器中的路径：%s %s %s", o.workspace(), o.home(), o.tmp())
	}
	env := map[string]string{}
	o.jobEnv(env)
	if len(env) != 0 {
		t.Errorf("没有 buildkit 时 jobEnv 不设置变量：%v", env)
	}
	if err := o.prepare(t.Context(), log); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, dockerCalls(t, calls), []string{
		"image inspect --format {{.Id}} golang@sha256:abc",
		"image inspect --format {{range .Config.Env}}{{println .}}{{end}} golang@sha256:abc",
		"network create --label dev.kiln.runner=7 --internal kiln-5",
		"image inspect --format {{.Id}} postgres:16",
		"run -d --name kiln-5-db --label dev.kiln.runner=7 --network kiln-5 --network-alias db -e A=1 -e POSTGRES_PASSWORD=pw postgres:16 -c fsync=off",
		"inspect --format {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} kiln-5-db",
		"run -d --name kiln-5 --label dev.kiln.runner=7 --network kiln-5 -w /work -v " + d.ws + ":/work -v " + d.home + ":/kiln/home -v " + d.tmp + ":/kiln/tmp" +
			" --platform " + otherPlatform + " --cpus 2.00 --memory 1073741824" +
			" -v /host/rt/abc:/opt/kiln/rt/go:ro -v /host/tc/gomod:/opt/kiln/toolcache/gomod --entrypoint sleep golang@sha256:abc infinity",
		"exec kiln-5 test -f /kiln/tmp/.kiln-mount-check",
	})
	if _, err := os.Stat(filepath.Join(d.tmp, ".kiln-mount-check")); !os.IsNotExist(err) {
		t.Errorf("挂载检查的标记文件应删除：%v", err)
	}
	o.destroy(t.Context())
	o.cleanupRoot(t.Context())
	wantCalls(t, dockerCalls(t, calls), []string{
		"rm -f -v kiln-5-db kiln-5",
		"network rm kiln-5",
		"run --rm --label dev.kiln.runner=7 --network none -v " + d.root + ":/x --entrypoint rm golang@sha256:abc -rf /x/ws /x/home /x/tmp",
	})
}

// 没有 services 时按 job 的 network 选择 docker 网络。
func TestOCIExecNetworkMode(t *testing.T) {
	t.Parallel()
	for _, network := range []bool{false, true} {
		docker, calls := fakeDocker(t)
		log := newTestShipper(t, newFakeServer(t))
		spec := ociSpec()
		spec.Network = network
		spec.Executor.Platform = ""
		spec.Resources = nil
		d := newTestDirs(t)
		o := newOCIExec(docker, d, spec, testRunnerID)
		if err := o.prepare(t.Context(), log); err != nil {
			t.Fatal(err)
		}
		log.Close()
		want := "none"
		if network {
			want = "bridge"
		}
		// 原生平台、没有资源限制时不带 --platform、--cpus、--memory
		wantCalls(t, dockerCalls(t, calls), []string{"run -d --name kiln-5 --label dev.kiln.runner=7 --network " + want + " -w /work -v " + d.ws + ":/work -v " + d.home + ":/kiln/home -v " + d.tmp + ":/kiln/tmp --entrypoint sleep golang@sha256:abc infinity"})
	}
}

// 本地没有镜像时拉取（带平台）。
func TestOCIExecEnsureImagePulls(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	o := newOCIExec(docker, newTestDirs(t), ociSpec(), testRunnerID)
	if err := o.ensureImage(t.Context(), "missing-image:1", log); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, dockerCalls(t, calls), []string{"image inspect --format {{.Id}} missing-image:1", "pull -q --platform " + otherPlatform + " missing-image:1"})
}

// service 容器退出时报错，并附上它的日志。
func TestOCIExecServiceExited(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	o := newOCIExec(docker, newTestDirs(t), ociSpec(&runnerv1.Service{Name: "dead", Image: "redis:7"}), testRunnerID)
	err := o.prepare(t.Context(), log)
	if err == nil || !strings.Contains(err.Error(), "service dead：容器已退出（exited）") {
		t.Fatalf("prepare = %v", err)
	}
	wantCalls(t, dockerCalls(t, calls), []string{"logs --tail 30 kiln-5-dead"})
	// 已经启动的容器和网络登记过，destroy 会清理
	if !slices.Contains(o.started, "kiln-5-dead") || o.netName != "kiln-5" {
		t.Errorf("started = %v，netName = %q", o.started, o.netName)
	}
}

// step 在容器中执行：环境变量经文件传入（不出现在命令行），working-directory 相对 /work。
func TestOCIExecRun(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	d := newTestDirs(t)
	if err := os.Mkdir(filepath.Join(d.ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := newOCIExec(docker, d, ociSpec(), testRunnerID)
	var out bytes.Buffer
	code, err := o.run(t.Context(), stepSpec{index: 0, script: "echo hi", workDir: "sub", env: map[string]string{"SECRET": "s3cr3t"}}, &out)
	if err != nil || code != 0 {
		t.Fatalf("run = %d, %v", code, err)
	}
	got := dockerCalls(t, calls)
	// 镜像 ENV 中的 askpass 和外层包装的私有变量去掉（gitenv.go）；没有 git 凭据通道时不建 git exec 镜像
	want := []string{
		`exec -w /work/sub kiln-5 sh -c ` + strings.TrimSpace(ociGitResetScript()),
		`. /kiln/tmp/kiln-env-0.sh && rm -f /kiln/tmp/kiln-env-0.sh || exit 1`,
		`unset GIT_ASKPASS SSH_ASKPASS`,
		`exec 'sh' '-c' 'if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -eo pipefail "$0"; else exec sh -e "$0"; fi' '/kiln/tmp/kiln-step-0.sh'`,
	}
	wantCalls(t, got, want)
	if strings.Contains(strings.Join(got, "\n"), "s3cr3t") {
		t.Error("环境变量的值出现在 docker 的命令行中")
	}
	for _, f := range []string{stepScriptName(0), stepEnvName(0)} {
		if _, err := os.Stat(filepath.Join(d.tmp, f)); !os.IsNotExist(err) {
			t.Errorf("%s 应在执行后删除：%v", f, err)
		}
	}
}

// 变量名不合法、working-directory 不存在是用户错误，不调用 docker。
func TestOCIExecRunUserErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    stepSpec
	}{
		{name: "变量名不合法", s: stepSpec{script: "true", env: map[string]string{"BAD-NAME": "x"}}},
		{name: "working-directory 不存在", s: stepSpec{script: "true", workDir: "missing"}},
		{name: "不支持的 shell", s: stepSpec{script: "true", shell: "fish"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			docker, calls := fakeDocker(t)
			o := newOCIExec(docker, newTestDirs(t), ociSpec(), testRunnerID)
			_, err := o.run(t.Context(), c.s, &bytes.Buffer{})
			if _, ok := errors.AsType[*userError](err); !ok {
				t.Fatalf("期望 userError，实际 %v", err)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Error("用户错误时不应调用 docker")
			}
		})
	}
}

// buildkit: true（PLAN #13.8）：从 daemon 镜像中取出 buildctl 并缓存，daemon 只通过 job 专用 volume 中的 socket 访问。
func TestOCIExecBuildkit(t *testing.T) {
	t.Parallel()
	docker, calls := fakeDocker(t)
	d := newTestDirs(t)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	cache := t.TempDir()
	image := "moby/buildkit@sha256:" + strings.Repeat("c", 64)
	o := newOCIExec(docker, d, ociSpec(), testRunnerID)
	if err := o.prepareBuildkitClient(t.Context(), image, cache, log); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, strings.Repeat("c", shorten.HashLen)+"-linux-s390x")
	st, err := os.Stat(filepath.Join(dir, "buildctl"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("buildctl：%v %v", st, err)
	}
	if !slices.Contains(o.mounts, [2]string{dir, ociBuildkitDir}) {
		t.Errorf("mounts = %v", o.mounts)
	}
	if !strings.HasPrefix(o.basePath(), ociBuildkitDir+":") {
		t.Errorf("basePath = %q", o.basePath())
	}
	env := map[string]string{}
	o.jobEnv(env)
	if env["BUILDKIT_HOST"] != "unix:///run/buildkit/buildkitd.sock" {
		t.Errorf("BUILDKIT_HOST = %q", env["BUILDKIT_HOST"])
	}
	if err := o.prepare(t.Context(), log); err != nil {
		t.Fatal(err)
	}
	o.destroy(t.Context())
	got := dockerCalls(t, calls)
	wantCalls(t, got, []string{
		"image inspect --format {{.Id}} " + image,
		"create --label dev.kiln.runner=7 --platform " + otherPlatform + " " + image,
		"rm cid123",
		"volume create --label dev.kiln.runner=7 kiln-5-buildkit",
		"run -d --name kiln-5-buildkitd --label dev.kiln.runner=7 --privileged --network none -v kiln-5-buildkit:/run/buildkit --platform " + otherPlatform + " --cpus 2.00 --memory 1073741824 " + image + " --addr unix:///run/buildkit/buildkitd.sock",
		"exec kiln-5-buildkitd buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers",
		"rm -f -v kiln-5-buildkitd kiln-5",
		"volume rm -f kiln-5-buildkit",
	})
	jobRun := ""
	for _, c := range got {
		if strings.HasPrefix(c, "run -d --name kiln-5 --label dev.kiln.runner=7 ") {
			jobRun = c
		}
	}
	if !strings.Contains(jobRun, " -v kiln-5-buildkit:/run/buildkit ") || !strings.Contains(jobRun, " -v "+dir+":"+ociBuildkitDir+":ro ") {
		t.Errorf("job 容器应挂载 socket 卷和 buildctl：%s", jobRun)
	}

	// 第二个 job 使用缓存的 buildctl，不再从镜像中取出
	docker2, calls2 := fakeDocker(t)
	o2 := newOCIExec(docker2, newTestDirs(t), ociSpec(), testRunnerID)
	if err := o2.prepareBuildkitClient(t.Context(), image, cache, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls2); !os.IsNotExist(err) {
		t.Errorf("buildctl 已缓存时不应调用 docker：%v", dockerCalls(t, calls2))
	}
}

// BuildKit 镜像必须按 digest 固定。
func TestOCIExecBuildkitRequiresDigest(t *testing.T) {
	t.Parallel()
	docker, _ := fakeDocker(t)
	o := newOCIExec(docker, newTestDirs(t), ociSpec(), testRunnerID)
	if err := o.prepareBuildkitClient(t.Context(), "moby/buildkit:latest", t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("prepareBuildkitClient = %v", err)
	}
}

// 完整的 oci job（假 docker）：准备镜像、启动容器、执行 step、销毁容器。
func TestExecuteOCI(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	docker, calls := fakeDocker(t)
	r := newTestRunner(t, fs, func(c *Config) { c.Docker = docker })
	spec := hostJob(1, bash("build", "make"))
	spec.Executor = &runnerv1.ExecutorSpec{Kind: "oci", Image: "golang@sha256:abc", Platform: otherPlatform}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")
	if !strings.Contains(log, "executor oci · golang@sha256:abc") {
		t.Errorf("日志中没有 executor 的说明：\n%s", log)
	}
	wantCalls(t, dockerCalls(t, calls), []string{
		"image inspect --format {{range .Config.Env}}{{println .}}{{end}} golang@sha256:abc",
		"exec kiln-1 test -f /kiln/tmp/.kiln-mount-check",
		"rm -f -v kiln-1",
	})
}

// dockerWarning 是同时设置了 DOCKER_HOST 和 docker context 时，docker 每条命令都在 stderr 中输出的警告。
const dockerWarning = `Warning: DOCKER_HOST environment variable overrides the active context. To use "colima", either set the global --context flag, or unset DOCKER_HOST environment variable.`

// warnDocker 生成一个先在 stderr 中输出 dockerWarning、再执行 inner 的 docker 命令。
func warnDocker(t *testing.T, inner string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\necho " + shQuote(dockerWarning) + " >&2\nexec " + shQuote(inner) + ` "$@"` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// docker 在 stderr 中输出的警告不能混进要解析的输出：镜像的默认用户和 PATH、service 的状态、
// BuildKit 临时容器的 ID 都只取 stdout（之前 service 一直等不到 healthy，buildctl 也取不出来）。
func TestOCIExecIgnoresDockerWarnings(t *testing.T) {
	t.Parallel()
	inner, calls := fakeDocker(t)
	docker := warnDocker(t, inner)
	log := newTestShipper(t, newFakeServer(t))
	defer log.Close()
	o := newOCIExec(docker, newTestDirs(t), ociSpec(&runnerv1.Service{Name: "db", Image: "postgres:16"}), testRunnerID)
	if err := o.prepareImage(t.Context(), log); err != nil {
		t.Fatal(err)
	}
	if o.imageUser != "" || o.cacheDomain() != "oci-root" {
		t.Errorf("imageUser = %q，cacheDomain = %q", o.imageUser, o.cacheDomain())
	}
	if o.basePath() != "/img/bin:/usr/bin" {
		t.Errorf("basePath = %q", o.basePath())
	}
	cache := t.TempDir()
	if err := o.prepareBuildkitClient(t.Context(), "moby/buildkit@sha256:"+strings.Repeat("d", 64), cache, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, strings.Repeat("d", shorten.HashLen)+"-linux-s390x", "buildctl")); err != nil {
		t.Errorf("buildctl：%v", err)
	}
	// 修复前 service 的状态解析不出来，要等到 serviceHealthTimeout
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := o.prepare(ctx, log); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, dockerCalls(t, calls), []string{
		"rm cid123",
		"inspect --format {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} kiln-5-db",
	})

	// 失败时错误信息中带上 stderr
	_, err := o.output(t.Context(), "image", "inspect", "--format", "{{.Id}}", "missing")
	if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Errorf("错误信息中应带上 stderr：%v", err)
	}
}

// 清理时列出的残留对象只取 stdout：stderr 中的警告不能当作对象 ID 删除。
func TestGCDockerIgnoresWarnings(t *testing.T) {
	t.Parallel()
	inner, calls := fakeGCDocker(t)
	r := newTestRunner(t, newFakeServer(t))
	rep := GC(t.Context(), r.cfg, GCOptions{Docker: warnDocker(t, inner), RunnerID: testRunnerID}, slog.New(slog.DiscardHandler))
	if len(rep.Failures) > 0 {
		t.Errorf("清理失败：%v", rep.Failures)
	}
	wantCalls(t, dockerCalls(t, calls), []string{"rm -f -v c1 c2", "network rm n1"})
}

// 读取 docker 的 CPU 和内存时同样只取 stdout。
func TestDockerLimitsIgnoresWarnings(t *testing.T) {
	t.Parallel()
	inner := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(inner, []byte("#!/bin/sh\necho '8 17179869184'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cpu, mem, ok := dockerLimits(t.Context(), warnDocker(t, inner), testProbeTimeout)
	if !ok || cpu != 8 || mem != 16<<30 {
		t.Errorf("dockerLimits = %v %v %v", cpu, mem, ok)
	}
}

// 探测 docker 的超时由 Config 注入：没有注入时用 runnerkit.ProbeTimeout；注入的超时到期后探测按失败处理，
// 不等默认的超时。
func TestDockerProbeTimeoutInjected(t *testing.T) {
	t.Parallel()
	if got := (&Config{}).probeTimeoutOrDefault(); got != runnerkit.ProbeTimeout {
		t.Errorf("默认超时 = %s，期望 %s", got, runnerkit.ProbeTimeout)
	}
	short := 300 * time.Millisecond
	cfg := &Config{probeTimeout: short}
	if got := cfg.probeTimeoutOrDefault(); got != short {
		t.Errorf("注入的超时 = %s，期望 %s", got, short)
	}
	hang := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Docker = hang
	start := time.Now()
	if docker := DockerIfAvailable(t.Context(), cfg); docker != "" {
		t.Errorf("docker 卡住时 DockerIfAvailable = %q，期望不可用", docker)
	}
	if _, _, ok := dockerLimits(t.Context(), hang, cfg.probeTimeoutOrDefault()); ok {
		t.Error("docker 卡住时 dockerLimits 不应成功")
	}
	if d := time.Since(start); d >= runnerkit.ProbeTimeout {
		t.Errorf("两次探测用了 %s，没有用注入的超时", d)
	}
}

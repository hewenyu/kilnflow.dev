package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// buildkit: true 的 job（PLAN #13.8 方式二）：Runner 为每个 job 启动一个独立的 BuildKit daemon 容器。
//
//   - daemon 镜像由 Server 下发（workflow.BuildkitImage，按 digest 固定，计入 action key）。
//   - daemon 以 --privileged 运行（BuildKit 的 OCI worker 需要创建挂载和 cgroup；rootless 模式在
//     开启了 apparmor_restrict_unprivileged_userns 的发行版上无法启动），但不挂载宿主机的任何目录，
//     也不挂载 docker.sock，不发布端口。
//   - daemon 只监听 unix socket，放在 job 专用的 docker volume 中，同一个 volume 挂进 job 容器，
//     job 中的 buildctl 通过 BUILDKIT_HOST 连接；其他容器和宿主机网络都访问不到它。
//   - daemon 的网络跟随 job 的 network：true 时为 bridge（拉取基础镜像、RUN 联网），false 时为 none。
//   - network: true 时 daemon 的环境中有 [proxy.oci] 的代理（拉取 FROM 的基础镜像）；RUN 中的代理要由
//     buildctl 以 build-arg 传入（HTTP_PROXY 等是 BuildKit 预定义的 build-arg，不写入镜像的 history 和配置）。
//   - daemon 与 job 容器一样受 job 的 resources（--cpus、--memory）限制。
//   - buildctl 从同一个 daemon 镜像中取出（静态链接），只读挂载到 job 容器的 /opt/kiln/buildkit。
//   - job 结束（成功、失败、取消）后，daemon 容器、它的状态卷和 socket 卷随 job 容器一起删除。

const (
	// buildkitSocketDir 是 socket 卷在 daemon 和 job 容器中的挂载点。
	buildkitSocketDir = "/run/buildkit"
	// buildkitHost 是 job 中 BUILDKIT_HOST 的值。
	buildkitHost = "unix://" + buildkitSocketDir + "/buildkitd.sock"
	// buildkitReadyTimeout 是等待 daemon 就绪的时间上限。
	buildkitReadyTimeout = time.Minute
	// buildkitReadyPoll 是检查 daemon 是否就绪的间隔。
	buildkitReadyPoll = 500 * time.Millisecond
)

// buildkitNames 返回 daemon 容器名和 socket 卷名。
func (o *ociExec) buildkitNames() (container, volume string) {
	return o.name + "-buildkitd", o.name + "-buildkit"
}

// prepareBuildkitClient 为 job 开启 BuildKit：拉取 daemon 镜像 image，取出其中的 buildctl
// （按镜像 digest 缓存在 cacheDir 下），并登记为只读挂载。需要在计算环境变量之前调用。
func (o *ociExec) prepareBuildkitClient(ctx context.Context, image, cacheDir string, log *runnerkit.LogShipper) error {
	o.buildkitImage = image
	_, digest, ok := strings.Cut(o.buildkitImage, "@")
	if !ok || !cas.ValidDigest(digest) {
		return fmt.Errorf("BuildKit 镜像必须用 digest 固定（例如 image@sha256:…）：%s", o.buildkitImage)
	}
	dir := filepath.Join(cacheDir, strings.TrimPrefix(digest, cas.DigestPrefix)[:shorten.HashLen]+"-"+strings.ReplaceAll(o.platformOrNative(), "/", "-"))
	bin := filepath.Join(dir, "buildctl")
	if _, err := os.Stat(bin); err == nil {
		runnerkit.Touch(dir) // 记录最后使用的时间（gc.go）
	} else {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := o.ensureImage(ctx, o.buildkitImage, log); err != nil {
			return err
		}
		if err := o.extractBuildctl(ctx, dir); err != nil {
			return fmt.Errorf("从 %s 取出 buildctl：%w", o.buildkitImage, err)
		}
	}
	o.mounts = append(o.mounts, [2]string{dir, ociBuildkitDir})
	return nil
}

// platformOrNative 返回容器的平台（为空时表示 Runner 的原生平台）。
func (o *ociExec) platformOrNative() string {
	if o.platform != "" {
		return o.platform
	}
	return "native"
}

// extractBuildctl 用 docker create + docker cp 取出 /usr/bin/buildctl，原子地放到 dir 下。
func (o *ociExec) extractBuildctl(ctx context.Context, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".buildctl-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // 成功时已经改名，只在失败时有内容
	args := []string{"create", "--label", o.label}
	if o.platform != "" {
		args = append(args, "--platform", o.platform)
	}
	id, err := o.output(ctx, append(args, o.buildkitImage)...)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		_ = exec.CommandContext(cctx, o.docker, "rm", id).Run() // 尽力删除临时容器
	}()
	if _, err := o.output(ctx, "cp", id+":/usr/bin/buildctl", filepath.Join(tmp, "buildctl")); err != nil {
		return err
	}
	//nolint:gosec // G302：buildctl 是可执行文件，需要在容器中以任意用户运行
	if err := os.Chmod(filepath.Join(tmp, "buildctl"), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// 另一个 job 同时取出了同一个版本：沿用它的结果
		if _, serr := os.Stat(filepath.Join(dir, "buildctl")); serr == nil {
			return nil
		}
		return err
	}
	return nil
}

// startBuildkit 创建 socket 卷、启动 daemon 并等待它就绪。
func (o *ociExec) startBuildkit(ctx context.Context, log *runnerkit.LogShipper) error {
	cname, vol := o.buildkitNames()
	// 先登记再创建：即使中途被取消，destroy 也会清理
	o.volumes = append(o.volumes, vol)
	if _, err := o.output(ctx, "volume", "create", "--label", o.label, vol); err != nil {
		return err
	}
	net := o.networkMode()
	args := []string{"run", "-d", "--name", cname, "--label", o.label, "--privileged", "--network", net, "-v", vol + ":" + buildkitSocketDir}
	envFile, err := o.writeBuildkitEnv()
	if err != nil {
		return err
	}
	if envFile != "" {
		// docker 客户端在 run 时读取，之后即可删除；代理凭证不出现在命令行中
		defer func() { _ = os.Remove(envFile) }()
		args = append(args, "--env-file", envFile)
	}
	args = append(args, o.limitArgs()...)
	args = append(args, o.buildkitImage, "--addr", buildkitHost)
	o.started = append(o.started, cname)
	if _, err := o.output(ctx, args...); err != nil {
		return fmt.Errorf("启动 BuildKit：%w", err)
	}
	if err := o.waitBuildkit(ctx, cname); err != nil {
		return fmt.Errorf("BuildKit：%w", err)
	}
	log.Printf("BuildKit 已启动（%s，network %s）", o.buildkitImage, net)
	return nil
}

// writeBuildkitEnv 在 network: true 且配置了 [proxy.oci] 时，把代理写到 attempt 目录（不挂载进容器）
// 下的 env 文件，返回文件路径；不需要时返回空字符串。
func (o *ociExec) writeBuildkitEnv() (string, error) {
	if !o.network || o.proxy.empty() {
		return "", nil
	}
	env := map[string]string{}
	o.proxy.apply(env)
	p := filepath.Join(o.d.root, "buildkitd.env")
	if err := os.WriteFile(p, []byte(strings.Join(envList(env), "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// waitBuildkit 等待 daemon 能够响应 buildctl。
func (o *ociExec) waitBuildkit(ctx context.Context, cname string) error {
	var lastErr error // 最近一次 buildctl 的错误，超时时附在错误信息中
	err := pollUntil(ctx, buildkitReadyTimeout, buildkitReadyPoll, func() (bool, error) {
		if _, lastErr = o.output(ctx, "exec", cname, "buildctl", "--addr", buildkitHost, "debug", "workers"); lastErr == nil {
			return true, nil
		}
		st, ierr := o.output(ctx, "inspect", "--format", "{{.State.Status}}", cname)
		if ierr == nil && st != "running" {
			return false, fmt.Errorf("daemon 已退出（%s）：\n%s", st, o.tailLogs(ctx, cname))
		}
		return false, nil
	})
	if errors.Is(err, errPollTimeout) {
		return fmt.Errorf("%s 内没有就绪：%w", buildkitReadyTimeout, lastErr)
	}
	return err
}

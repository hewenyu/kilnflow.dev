package runner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// 服务安装（v0：以当前用户运行。macOS 为 LaunchAgent，Linux 为 systemd 用户服务；
// 不创建 kiln-job 用户，见 PLAN #25）。安装、卸载的机制见 runnerkit。

const (
	// serviceStopTimeout 是服务管理器停止 Runner 时等待的时间（launchd 的 ExitTimeOut、systemd 的
	// TimeoutStopSec）：Runner 收到信号后会等正在执行的 job 结束（最长为默认的 job 超时），
	// 再留出提交结果和清理的时间。
	serviceStopTimeout = defaultJobTimeout + 100*time.Second
	// minFreeDisk 是 Doctor 要求的数据目录最小可用空间。
	minFreeDisk = 10 << 30
)

// InstallService 安装并启动服务，返回服务文件的路径（及需要用户执行的后续操作）。
// Runner 的日志写到 cfg.LogDir() 中按天切分的文件（保留 7 天）。
func InstallService(ctx context.Context, cfg *Config) (string, error) {
	return runnerkit.InstallService(ctx, cfg.home(), serviceStopTimeout)
}

// UninstallService 停止并删除服务。
func UninstallService(ctx context.Context, cfg *Config) error {
	return runnerkit.UninstallService(ctx, cfg.home())
}

// Doctor 检查运行环境，返回是否全部通过。
func Doctor(ctx context.Context, cfg *Config, opts DoctorOptions, out func(ok bool, item, detail string)) bool {
	all := true
	check := func(ok bool, item, detail string) {
		if !ok {
			all = false
		}
		out(ok, item, detail)
	}
	runnerkit.DoctorIdentity(ctx, cfg.home(), &cfg.Common, check)
	gitVer := runnerkit.CmdOut(ctx, "git", "--version")
	check(gitVer != "", "git", gitVer)
	bash := runnerkit.CmdOut(ctx, "bash", "--version")
	check(bash != "", "bash", runnerkit.FirstLine(bash))
	if cfg.Docker != dockerDisabled {
		v := runnerkit.CmdOutTimeout(ctx, cfg.probeTimeoutOrDefault(), cfg.Docker, "info", "--format", "{{.ServerVersion}} {{.OperatingSystem}} {{.Architecture}}")
		detail := v
		if v == "" {
			detail = "不可用：没有 oci executor"
		}
		out(v != "", "docker（oci executor，可选）", detail)
	}
	if _, free, ok := diskUsage(cfg.Dir); ok {
		check(free > minFreeDisk, "磁盘空间", fmt.Sprintf("%s 可用 %.1f GiB", cfg.Dir, float64(free)/(1<<30)))
	}
	h := detectHost(ctx, cfg)
	out(true, "平台", strings.Join(h.platforms, ", "))
	out(true, "executor", strings.Join(h.executors, ", "))
	out(true, "资源", fmt.Sprintf("%.0f CPU、%.1f GiB 内存（保留 %.0f CPU、%s）", h.totalCPU, float64(h.totalMem)/(1<<30), cfg.ReserveCPU, cfg.ReserveMemory))
	fp := make([]string, 0, len(h.fingerprint))
	for _, k := range slices.Sorted(maps.Keys(h.fingerprint)) {
		fp = append(fp, k+"="+h.fingerprint[k])
	}
	out(true, "宿主机指纹", strings.Join(fp, "; "))
	out(true, "job 代理（host）", cfg.Proxy.Host.String())
	if cfg.Docker != dockerDisabled {
		out(true, "job 代理（oci）", cfg.Proxy.OCI.String())
	}
	if opts.Network {
		doctorHostNetwork(ctx, cfg.Proxy.Host, doctorNetworkTargets, check)
	}
	return all
}

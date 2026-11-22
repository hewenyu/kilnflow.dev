package runner

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 本机信息的探测（PLAN #8.2）：平台、executor、资源和宿主机指纹，Runner 启动时探测一次；
// 各 executor 的固定上限见 capacity.go。

// hostInfo 是探测到的本机信息（PLAN #8.2），Runner 运行期间不变。
type hostInfo struct {
	platforms   []string
	executors   []string
	fingerprint map[string]string
	totalCPU    float64
	totalMem    int64
	// limits 是各 executor 的固定上限（capacity.go）。
	limits map[string]*runnerv1.Resources
}

// detectHost 探测平台、executor、资源和宿主机指纹（PLAN #8.2）。
func detectHost(ctx context.Context, cfg *Config) hostInfo {
	native := runnerkit.NativePlatform()
	h := hostInfo{
		platforms: []string{native},
		executors: []string{string(workflow.ExecutorHost)},
	}
	docker := ""
	if cfg.Docker != dockerDisabled && dockerOK(ctx, cfg.Docker, cfg.probeTimeoutOrDefault()) {
		docker = cfg.Docker
		h.executors = append(h.executors, string(workflow.ExecutorOCI))
		if cfg.Buildkit {
			h.executors = append(h.executors, workflow.CapBuildkit)
		}
		plats := cfg.OCIPlatforms
		if len(plats) == 0 {
			plats = []string{"linux/" + runtime.GOARCH}
		}
		for _, p := range plats {
			if p != native {
				h.platforms = append(h.platforms, p)
			}
		}
	}
	if signingSupported() {
		h.executors = append(h.executors, workflow.CapSigning)
	}
	h.totalCPU = float64(runtime.NumCPU())
	h.totalMem = runnerkit.TotalMemory(ctx)
	h.limits = detectLimits(ctx, docker, cfg.probeTimeoutOrDefault())
	h.fingerprint = runnerkit.HostFingerprint(ctx)
	return h
}

// state 返回上报给 Server 的状态（不含正在执行的 job）；usedCPU、usedMem 是 job 占用的资源。
func (h *hostInfo) state(cfg *Config, usedCPU float64, usedMem int64) *runnerv1.RunnerState {
	freeCPU := h.totalCPU - cfg.ReserveCPU - usedCPU
	freeMem := h.totalMem - cfg.reserveMemoryBytes() - usedMem
	// 0 表示"未上报"，所以用一个极小值表示资源已满
	freeCPU = max(freeCPU, 0.001)
	freeMem = max(freeMem, 1)
	return &runnerv1.RunnerState{
		Version:         buildinfo.Version(),
		Platforms:       h.platforms,
		Executors:       h.executors,
		HostFingerprint: h.fingerprint,
		Free:            &runnerv1.Resources{Cpu: freeCPU, MemoryBytes: freeMem},
		Capacity:        h.capacity(cfg),
	}
}

// dockerOK 判断 docker 是否可用；timeout 是探测的超时（Config.probeTimeoutOrDefault）。
func dockerOK(ctx context.Context, docker string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}").Run() == nil
}

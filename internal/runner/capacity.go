package runner

import (
	"bufio"
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 静态规格与容量（PLAN #8.6）：Runner 启动时探测一次，不采集实时占用。
// 有效容量 = 机器规格 - 预留，host 和 oci 各自另有固定上限（服务的 cgroup 限制、docker 的虚拟机规格）；
// Server 用 job 声明的 resources 记账。

// cgroupRoot 是 cgroup v2 的挂载点。
const cgroupRoot = "/sys/fs/cgroup"

// capacity 返回上报给 Server 的规格和容量。
func (h *hostInfo) capacity(cfg *Config) *runnerv1.Capacity {
	c := &runnerv1.Capacity{
		Machine:        &runnerv1.Resources{Cpu: h.totalCPU, MemoryBytes: h.totalMem},
		Reserved:       &runnerv1.Resources{Cpu: cfg.ReserveCPU, MemoryBytes: cfg.reserveMemoryBytes()},
		MaxJobs:        runnerkit.ClampInt32(cfg.MaxJobs),
		ExecutorLimits: map[string]*runnerv1.Resources{},
	}
	for kind, l := range h.limits {
		if l.GetCpu() > 0 || l.GetMemoryBytes() > 0 {
			c.ExecutorLimits[kind] = l
		}
	}
	return c
}

// detectLimits 探测各 executor 的固定上限：host 为 Runner 所在 cgroup 的限制（Linux），oci 为 docker 的 NCPU / MemTotal。
// timeout 是探测 docker 的超时（Config.probeTimeoutOrDefault）。
func detectLimits(ctx context.Context, docker string, timeout time.Duration) map[string]*runnerv1.Resources {
	m := map[string]*runnerv1.Resources{}
	if cpu, mem := cgroupLimits("/proc/self/cgroup", cgroupRoot); cpu > 0 || mem > 0 {
		m[string(workflow.ExecutorHost)] = &runnerv1.Resources{Cpu: cpu, MemoryBytes: mem}
	}
	if docker != "" {
		if cpu, mem, ok := dockerLimits(ctx, docker, timeout); ok {
			m[string(workflow.ExecutorOCI)] = &runnerv1.Resources{Cpu: cpu, MemoryBytes: mem}
		}
	}
	return m
}

// dockerLimits 读取 docker 能用的 CPU 和内存（Linux 上是整机，colima / Docker Desktop 是虚拟机的规格）。
func dockerLimits(ctx context.Context, docker string, timeout time.Duration) (float64, int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := dockerOutput(ctx, docker, "info", "--format", "{{.NCPU}} {{.MemTotal}}")
	f := strings.Fields(out)
	if err != nil || len(f) != 2 {
		return 0, 0, false
	}
	cpu, err1 := strconv.ParseFloat(f[0], 64)
	mem, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || cpu <= 0 || mem <= 0 {
		return 0, 0, false
	}
	return cpu, mem, true
}

// cgroupLimits 返回进程所在 cgroup（v2）及其上层中最严格的 CPU 配额（核数）和内存上限，没有限制时为 0。
// procCgroup 是 /proc/self/cgroup，root 是 cgroup 的挂载点（测试中替换）。不是 cgroup v2 时返回 0。
func cgroupLimits(procCgroup, root string) (cpu float64, mem int64) {
	f, err := os.Open(procCgroup)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var rel string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			rel = p
		}
	}
	if rel == "" {
		return 0, 0
	}
	for p := path.Clean("/" + rel); ; p = path.Dir(p) {
		dir := filepath.Join(root, filepath.FromSlash(p))
		if c := readCPUMax(filepath.Join(dir, "cpu.max")); c > 0 && (cpu == 0 || c < cpu) {
			cpu = c
		}
		if m := readMemMax(filepath.Join(dir, "memory.max")); m > 0 && (mem == 0 || m < mem) {
			mem = m
		}
		if p == "/" {
			return cpu, mem
		}
	}
}

// readCPUMax 解析 cpu.max（"<quota> <period>" 或 "max <period>"），返回核数；没有限制或无法读取时为 0。
func readCPUMax(p string) float64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quota / period
}

// readMemMax 解析 memory.max（字节数或 "max"）；没有限制或无法读取时为 0。
func readMemMax(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

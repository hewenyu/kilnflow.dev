package runner

import (
	"os"
	"path/filepath"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// cgroup v2：取进程所在 cgroup 及其上层中最严格的 CPU 配额和内存上限（例如 systemd 的 CPUQuota=200%、MemoryMax=6G）。
func TestCgroupLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	svc := "user.slice/user-1001.slice/user@1001.service/app.slice/kiln-runner.service"
	files := map[string]string{
		"cpu.max":                            "max 100000\n",
		"user.slice/memory.max":              "max\n",
		"user.slice/user-1001.slice/cpu.max": "300000 100000\n",
		svc + "/cpu.max":                     "200000 100000\n",
		svc + "/memory.max":                  "6442450944\n",
		"user.slice/user-1001.slice/user@1001.service/memory.max": "8589934592\n",
	}
	for p, c := range files {
		mustWrite(t, filepath.Join(root, p), c)
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	mustWrite(t, proc, "0::/"+svc+"\n")
	cpu, mem := cgroupLimits(proc, root)
	if cpu != 2 || mem != 6<<30 {
		t.Errorf("cgroupLimits = %v CPU, %d", cpu, mem)
	}
	// cgroup v1 或没有限制
	mustWrite(t, proc, "12:cpu,cpuacct:/user.slice\n")
	if cpu, mem := cgroupLimits(proc, root); cpu != 0 || mem != 0 {
		t.Errorf("cgroup v1 = %v, %d，应为 0", cpu, mem)
	}
	if cpu, mem := cgroupLimits(filepath.Join(root, "missing"), root); cpu != 0 || mem != 0 {
		t.Errorf("没有 /proc/self/cgroup = %v, %d", cpu, mem)
	}
}

// 上报的容量：机器规格、预留、各 executor 的固定上限（没有限制的不上报）和 max_jobs。
func TestHostCapacity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\necho 4 8326250496\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := hostInfo{totalCPU: 10, totalMem: 16 << 30, limits: detectLimits(t.Context(), docker, testProbeTimeout)}
	h.limits["host"] = &runnerv1.Resources{} // 没有限制
	cfg := &Config{ReserveCPU: 2, ReserveMemory: "4Gi", MaxJobs: 3}
	c := h.capacity(cfg)
	if c.Machine.Cpu != 10 || c.Machine.MemoryBytes != 16<<30 || c.Reserved.Cpu != 2 || c.Reserved.MemoryBytes != 4<<30 || c.MaxJobs != 3 {
		t.Errorf("capacity = %v", c)
	}
	if l := c.ExecutorLimits["oci"]; l.GetCpu() != 4 || l.GetMemoryBytes() != 8326250496 {
		t.Errorf("oci 上限 = %v", l)
	}
	if _, ok := c.ExecutorLimits["host"]; ok {
		t.Error("没有限制的 executor 不应上报")
	}
	// 显式不预留
	cfg = &Config{ReserveMemory: "0"}
	if cfg.reserveMemoryBytes() != 0 {
		t.Error(`reserve_memory = "0" 应表示不预留`)
	}
}

// 显式写 reserve_cpu = 0 表示不预留；没有写时使用默认值。
func TestLoadConfigReserveZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, runnerkit.ConfigFileName), "reserve_cpu = 0\nreserve_memory = \"0\"\n")
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReserveCPU != 0 || cfg.reserveMemoryBytes() != 0 {
		t.Errorf("reserve = %v, %d", cfg.ReserveCPU, cfg.reserveMemoryBytes())
	}
	cfg, err = LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReserveCPU != defaultReserveCPU || cfg.reserveMemoryBytes() != defaultReserveMemoryBytes {
		t.Errorf("默认 reserve = %v, %d", cfg.ReserveCPU, cfg.reserveMemoryBytes())
	}
	if h, l := cfg.diskWatermarks(); h != 80 || l != 65 {
		t.Errorf("默认水位 = %d, %d", h, l)
	}
	mustWrite(t, filepath.Join(dir, runnerkit.ConfigFileName), "toolcache_max_size = \"lots\"\n")
	if _, err := LoadConfig(dir); err == nil {
		t.Error("无效的 toolcache_max_size 应报错")
	}
}

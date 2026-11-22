package store

import (
	"fmt"
	"math"
)

// Runner 的静态规格与容量（PLAN #8.6）：Runner 上报，保存在 ci_runners.capacity。
// 调度用 job 声明的 resources 记账，不采集实时占用。

// Resources 是 CPU（核）和内存（字节）。
type Resources struct {
	CPU    float64 `json:"cpu"`
	Memory int64   `json:"memory"`
}

// Sub 返回 r - o。
func (r Resources) Sub(o Resources) Resources {
	return Resources{CPU: r.CPU - o.CPU, Memory: r.Memory - o.Memory}
}

// Add 返回 r + o。
func (r Resources) Add(o Resources) Resources {
	return Resources{CPU: r.CPU + o.CPU, Memory: r.Memory + o.Memory}
}

// cpuEpsilon 是比较 CPU 时允许的误差（浮点数累加）。
const cpuEpsilon = 1e-6

// Covers 判断 r 是否不少于 need（CPU 和内存都要满足）。
func (r Resources) Covers(need Resources) bool {
	return need.CPU <= r.CPU+cpuEpsilon && need.Memory <= r.Memory
}

// Less 按"CPU 优先、内存其次"比较空闲容量，用于在多台 Runner 中择优（PLAN #14.2）。
func (r Resources) Less(o Resources) bool {
	if math.Abs(r.CPU-o.CPU) > cpuEpsilon {
		return r.CPU < o.CPU
	}
	return r.Memory < o.Memory
}

// String 返回"2 CPU / 1.9 GiB"这样的写法。
func (r Resources) String() string {
	return fmt.Sprintf("%s CPU / %s", FormatCPU(r.CPU), FormatMemory(r.Memory))
}

// FormatCPU 返回 CPU 核数的写法：整数不带小数，否则保留两位有效的小数。
func FormatCPU(cpu float64) string {
	if cpu == math.Trunc(cpu) {
		return fmt.Sprintf("%.0f", cpu)
	}
	return fmt.Sprintf("%.2g", cpu)
}

// FormatMemory 返回内存大小的写法（GiB 或 MiB）。
func FormatMemory(n int64) string {
	if n >= 1<<30 || n <= -(1<<30) {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

// RunnerCapacity 是 Runner 上报的规格和固定上限（ci_runners.capacity）。
type RunnerCapacity struct {
	// Machine 是机器规格：逻辑 CPU 核数和物理内存。
	Machine Resources `json:"machine"`
	// Reserved 是 config.toml 中为宿主机预留的资源。
	Reserved Resources `json:"reserved"`
	// Limits 是各 executor 的固定上限（host：服务的 cgroup 限制；oci：docker 的 NCPU / MemTotal）；
	// 字段为 0 表示这一项不限制。
	Limits map[string]Resources `json:"limits,omitempty"`
	// MaxJobs 是同时执行的 job 数上限。
	MaxJobs int `json:"max_jobs"`
}

// Allocatable 返回可以分配给 job 的总量：机器规格 - 预留（不小于 0）。
func (c *RunnerCapacity) Allocatable() Resources {
	a := c.Machine.Sub(c.Reserved)
	return Resources{CPU: max(a.CPU, 0), Memory: max(a.Memory, 0)}
}

// ExecutorCapacity 返回某种 executor 的 job 能用的容量：总量与这种 executor 的固定上限中较小的那个。
func (c *RunnerCapacity) ExecutorCapacity(executor string) Resources {
	a := c.Allocatable()
	if l, ok := c.Limits[executor]; ok {
		if l.CPU > 0 {
			a.CPU = min(a.CPU, l.CPU)
		}
		if l.Memory > 0 {
			a.Memory = min(a.Memory, l.Memory)
		}
	}
	return a
}

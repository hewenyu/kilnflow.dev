package runnerapi

import (
	"math"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Native Runner 的领取在调度核心（internal/server/ci/scheduler，PLAN #32）中进行；这里只把 Runner 上报的状态转换成
// 调度核心的形式。

// capacityFromProto 转换 Runner 上报的容量；没有上报（旧版 Runner）时返回 nil。
func capacityFromProto(c *runnerv1.Capacity) *store.RunnerCapacity {
	if c == nil || c.GetMachine() == nil {
		return nil
	}
	res := func(r *runnerv1.Resources) store.Resources {
		cpu := r.GetCpu()
		if !finite(cpu) || cpu < 0 {
			cpu = 0
		}
		return store.Resources{CPU: cpu, Memory: max(r.GetMemoryBytes(), 0)}
	}
	out := &store.RunnerCapacity{Machine: res(c.Machine), Reserved: res(c.Reserved), MaxJobs: int(c.MaxJobs)}
	for k, v := range c.ExecutorLimits {
		if out.Limits == nil {
			out.Limits = map[string]store.Resources{}
		}
		out.Limits[k] = res(v)
	}
	return out
}

// nativeTrigger 返回这次领取的触发方：Runner 的身份和记录的能力（ri），以及这次上报的状态（st 为 nil 时没有上报）。
func nativeTrigger(ri *runnerInfo, req *runnerv1.AcquireRequest) *scheduler.NativeTrigger {
	t := &scheduler.NativeTrigger{
		Runner: scheduler.NativeRunner{
			ID: ri.ID, Name: ri.Name, Pool: ri.Pool, Drain: ri.Drain, Platforms: ri.Platforms, Executors: ri.Executors,
		},
		RequestID: req.RequestId,
		MaxJobs:   int(req.MaxJobs),
	}
	if st := req.State; st != nil {
		rep := &scheduler.NativeReport{
			Platforms: st.Platforms, Executors: st.Executors, Cap: capacityFromProto(st.GetCapacity()), Running: int(st.GetRunningJobs()),
		}
		if rep.Cap == nil {
			if cpu, ok := reportedFreeCPU(st); ok && cpu > 0 {
				rep.LegacyFree = &store.Resources{CPU: cpu, Memory: st.GetFree().GetMemoryBytes()}
			}
		}
		t.Report = rep
	}
	return t
}

// finite 判断 v 是有限值（不是 NaN、±Inf）。
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

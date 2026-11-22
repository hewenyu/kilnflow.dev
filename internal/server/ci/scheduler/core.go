// Package scheduler 是 CI 的调度核心（PLAN #32、PLAN #14）：Native Runner 的领取和 Server 端的托管派发经过同一个原子预留入口，
// 从同一个 pending 队列中按同一套硬约束和固定的择优规则选 job。
//
// 本包分两部分：
//   - 核心（core.go）：与协议、数据库无关的资源模型（Requirements、NativeRunner、ManagedPool、Snapshot）、匹配、择优、
//     等待原因和选择器（Selector）；
//   - 预留（reserve.go）：Reserver 在一个事务中锁触发方的资源、翻页取候选、锁仓库配额、读快照、调用核心、写 attempt。
//
// 本包不依赖任何 Runner 协议（runner.v1、airunner.v2）和云 SDK（internal/archtest 检查）；协议相关的部分（生成 JobSpec）
// 由触发方以 Prepare 回调传入。
package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const (
	// SchedDelay 是为了让更合适的资源领取而跳过一个 job 的最长时间（PLAN #14.3 的延迟调度）。
	SchedDelay = 10 * time.Second
	// maxReasonResources 是等待原因中最多列出的资源数。
	maxReasonResources = 5
)

// 托管池在快照中声明的能力（ManagedPool.Caps）。buildkit、signing 与 Native Runner 上报的能力同名（workflow.CapBuildkit、
// workflow.CapSigning）；services、effects 是 Native Runner 一律具备、托管池需要声明的能力。
const (
	CapServices = "services"
	CapEffects  = "effects"
)

// Requirements 是一个 job 对执行资源的要求：只包含会让某种资源接不了的需求（PLAN #32.5）。调度时由 cistore.Job 计算，不落库；
// 核心判断（mismatch）和候选 SQL 的预过滤参数都由它与资源的 accepts 得出，预过滤不会排除核心会接受的 job（I7）。
type Requirements struct {
	Platform string
	Executor workflow.ExecutorKind
	// Pool 为空表示任何 pool 都可以。
	Pool      string
	Resources store.Resources
	// Caps 是需要的能力（workflow.CapBuildkit、workflow.CapSigning、CapServices、CapEffects），按这个顺序。
	Caps []string
	// Exclusive 表示需要独占 Native Runner（引用了 secret 的 host job，PLAN #8.5）。
	Exclusive bool
}

// RequirementsOf 计算 job 的 Requirements。
func RequirementsOf(j *cistore.Job) Requirements {
	r := Requirements{
		Platform: j.Platform, Executor: j.Executor, Pool: j.Spec.Pool,
		Resources: store.Resources{CPU: j.Spec.CPU, Memory: j.Spec.MemoryBytes},
		Exclusive: j.Executor == workflow.ExecutorHost && len(j.Spec.Secrets) > 0,
	}
	for _, c := range []struct {
		need bool
		cap  string
	}{
		{j.Spec.BuildkitImage != "", workflow.CapBuildkit},
		{j.Spec.Signing != nil, workflow.CapSigning},
		{len(j.Spec.Services) > 0, CapServices},
		{j.Effects, CapEffects},
	} {
		if c.need {
			r.Caps = append(r.Caps, c.cap)
		}
	}
	return r
}

// accepts 描述一种资源能接哪些 job（pool、平台、executor、能力）。核心的 mismatch 和候选 SQL 的预过滤（filterArgs）都只由它得出。
type accepts struct {
	// label 是写在原因中的资源名（"Runner mac"、"托管池 gcp-a"）。
	label     string
	pool      string
	platforms []string
	executors []string
	// native 是 host executor 能执行的平台（Native Runner 的 platforms[0]；托管池没有 host executor，为空）。
	native string
	caps   map[string]bool
	// lacks 返回缺少某种能力时的原因。
	lacks func(capability string) string
}

// mismatch 检查 pool、executor/平台和能力，返回不能执行的原因（空表示可以）。
func (a *accepts) mismatch(req Requirements) string {
	if req.Pool != "" && req.Pool != a.pool {
		return fmt.Sprintf("只允许在 pool %s 上执行", req.Pool)
	}
	if !slices.Contains(a.platforms, req.Platform) || !slices.Contains(a.executors, string(req.Executor)) {
		return fmt.Sprintf("%s 不能用 %s 执行 %s", a.label, req.Executor, req.Platform)
	}
	// host executor 只能执行原生平台的 job；oci 只执行 linux 平台
	if (req.Executor == workflow.ExecutorHost && req.Platform != a.native) || (req.Executor == workflow.ExecutorOCI && !strings.HasPrefix(req.Platform, "linux/")) {
		return fmt.Sprintf("%s 不能用 %s 执行 %s", a.label, req.Executor, req.Platform)
	}
	for _, c := range req.Caps {
		if !a.caps[c] {
			return a.lacks(c)
		}
	}
	return ""
}

// ---------- Native Runner ----------

// NativeRunner 是调度时的一台 Native Runner（来自 runners JOIN ci_runners）：能力、容量和账本（PLAN #14.1、#14.2）。
//
// 账本来自 Server 自己的记录：正在这台 Runner 上执行的 job 声明的 resources 之和（按 executor 分开），加上本次已经选中的。
// job 只有在声明的资源不超过有效容量减去账本时才能领取——不论 Runner 是否空闲。有效容量 = 机器规格 - 预留，再与这种 executor
// 的固定上限（cgroup、docker 虚拟机）取较小值；所有 executor 合计也不超过机器规格 - 预留。没有上报容量的旧版 Runner 按它
// 上报的空闲资源判断。
type NativeRunner struct {
	ID        int64
	Name      string
	Pool      string
	Drain     bool
	Online    bool // 最近 store.RunnerOnlineWindow 内领取过
	Platforms []string
	Executors []string
	// Cap 为 nil 表示旧版 Runner 没有上报容量，这时用 LegacyFree（没有上报时为 nil，不按资源判断）。
	Cap        *store.RunnerCapacity
	LegacyFree *store.Resources
	// Used 是正在执行（包括本次已选中）的 job 声明的资源，按 executor 分开。
	Used map[string]store.Resources
	// Running 是正在执行（包括本次已选中）的 job 数；Exclusive 是其中需要独占 Runner 的数量。
	Running   int
	Exclusive int
}

// native 返回原生平台（platforms[0]）。
func (f *NativeRunner) native() string {
	if len(f.Platforms) == 0 {
		return ""
	}
	return f.Platforms[0]
}

func (f *NativeRunner) accepts() *accepts {
	return &accepts{
		label: "Runner " + f.Name, pool: f.Pool, platforms: f.Platforms, executors: f.Executors, native: f.native(),
		caps: map[string]bool{
			workflow.CapBuildkit: slices.Contains(f.Executors, workflow.CapBuildkit),
			workflow.CapSigning:  slices.Contains(f.Executors, workflow.CapSigning),
			// services、effects：Native Runner 一律具备（PLAN #32.5）
			CapServices: true,
			CapEffects:  true,
		},
		lacks: func(c string) string {
			switch c {
			case workflow.CapBuildkit:
				return fmt.Sprintf("Runner %s 没有开启 buildkit（在 config.toml 中设置 buildkit = true）", f.Name)
			case workflow.CapSigning:
				return fmt.Sprintf("Runner %s 不支持 signing（需要 macOS 上 v0.1.17 及以上的 kiln-runner）", f.Name)
			}
			return fmt.Sprintf("Runner %s 不支持 %s", f.Name, c)
		},
	}
}

// Mismatch 检查 pool、executor/平台和能力，返回不能执行的原因（空表示可以）。
func (f *NativeRunner) Mismatch(req Requirements) string { return f.accepts().mismatch(req) }

// usedTotal 返回所有 executor 的账本之和。
func (f *NativeRunner) usedTotal() store.Resources {
	var t store.Resources
	for _, u := range f.Used {
		t = t.Add(u)
	}
	return t
}

// capacityFor 返回这种 executor 的 job 的有效容量（不扣账本）；known 为 false 表示不知道（旧版 Runner 没有上报）。
func (f *NativeRunner) capacityFor(executor string) (store.Resources, bool) {
	if f.Cap != nil {
		return f.Cap.ExecutorCapacity(executor), true
	}
	return store.Resources{}, false
}

// Free 返回这种 executor 的 job 现在还能用的资源；known 为 false 表示不知道。
func (f *NativeRunner) Free(executor string) (store.Resources, bool) {
	if f.Cap == nil {
		if f.LegacyFree == nil {
			return store.Resources{}, false
		}
		// 旧版 Runner 上报的空闲资源已经扣掉了它正在执行的 job，这里只扣本次已选中的（Used 中记的是全部，
		// 旧版的账本从 0 开始，见 Reserver 的 loadNatives）
		return f.LegacyFree.Sub(f.usedTotal()), true
	}
	total := f.Cap.Allocatable().Sub(f.usedTotal())
	perExec := f.Cap.ExecutorCapacity(executor).Sub(f.Used[executor])
	return store.Resources{CPU: min(total.CPU, perExec.CPU), Memory: min(total.Memory, perExec.Memory)}, true
}

// SlotFree 判断还能再执行一个 job（max_jobs；旧版 Runner 不知道上限，由它自己的 max_jobs 控制）。
func (f *NativeRunner) SlotFree() bool {
	return f.Cap == nil || f.Cap.MaxJobs <= 0 || f.Running < f.Cap.MaxJobs
}

// Fits 判断现在能否放下 req。
func (f *NativeRunner) Fits(req Requirements) bool {
	free, known := f.Free(string(req.Executor))
	return !known || free.Covers(req.Resources)
}

// Add 把 req 计入账本。
func (f *NativeRunner) Add(req Requirements) {
	if f.Used == nil {
		f.Used = map[string]store.Resources{}
	}
	f.Used[string(req.Executor)] = f.Used[string(req.Executor)].Add(req.Resources)
	f.Running++
	if req.Exclusive {
		f.Exclusive++
	}
}

// ---------- 托管池 ----------

// PoolState 是托管池的状态（PLAN #32.5），由托管资源的来源（ManagedSource）计算。
type PoolState string

const (
	PoolReady    PoolState = "ready"
	PoolDegraded PoolState = "degraded"
	PoolDisabled PoolState = "disabled"
)

// ManagedPool 是调度时的一个托管池（由 Server 派发执行的资源）的快照。
type ManagedPool struct {
	// Key 是 attempt 中记录的标识（attempts.managed_pool），也是池的 advisory lock 的键。
	Key  string
	Name string
	// Pool 是池的 pool 名称，与 Native 的 pool 同一个命名空间：job 的 pool 指向托管池即只在托管上执行。
	Pool      string
	Platforms []string
	// Executors 是能用的 executor（不能有 host：托管 job 各自独占执行环境，独占约束只作用于 Native）。
	Executors []string
	// Caps 是具备的能力（workflow.CapBuildkit、workflow.CapSigning、CapServices、CapEffects）。
	Caps []string
	// PerJob 是一个执行槽的资源（例如机型规格）。
	PerJob store.Resources
	// MaxConcurrent 是同时执行（包括已预留、已提交）的上限，必须为正数。
	MaxConcurrent int
	State         PoolState
	// Unavailable 非空时不接任何 job，内容即原因（例如"本月预算已用完"）。
	Unavailable string
	// Running 是账本：当前 attempt 属于这个池、job 处于 running 的数量（不论 phase），加上本次已选中的。由 Reserver 填写。
	Running int
}

func (p *ManagedPool) label() string { return cistore.ManagedPoolLabel + p.Name }

// Unusable 返回池现在不能接任何 job 的原因（空表示可以）：停用、不可用（例如预算用完）、声明了 host executor、没有并发上限。
func (p *ManagedPool) Unusable() string {
	switch {
	case p.State == PoolDisabled:
		return p.label() + " 已停用"
	case p.Unavailable != "":
		return p.label() + " 不可用：" + p.Unavailable
	case slices.Contains(p.Executors, string(workflow.ExecutorHost)):
		return p.label() + " 声明了 host executor（托管 job 不能独占机器，不能用 host）"
	case p.MaxConcurrent <= 0:
		return p.label() + " 没有设置并发上限"
	}
	return ""
}

func (p *ManagedPool) accepts() *accepts {
	caps := map[string]bool{}
	for _, c := range p.Caps {
		caps[c] = true
	}
	return &accepts{
		label: p.label(), pool: p.Pool, platforms: p.Platforms, executors: p.Executors, caps: caps,
		lacks: func(c string) string { return fmt.Sprintf("%s 不支持 %s", p.label(), c) },
	}
}

// Mismatch 返回池不能执行 req 的原因（空表示可以）：先检查池是否可用，再检查 pool、executor/平台和能力。
func (p *ManagedPool) Mismatch(req Requirements) string {
	if why := p.Unusable(); why != "" {
		return why
	}
	return p.accepts().mismatch(req)
}

// SlotFree 判断还有空闲的执行槽。
func (p *ManagedPool) SlotFree() bool { return p.Running < p.MaxConcurrent }

// Fits 判断一个执行槽能否放下 req（每个托管 job 独占一个执行槽，与账本无关）。
func (p *ManagedPool) Fits(req Requirements) bool { return p.PerJob.Covers(req.Resources) }

// freeSlots 返回空闲的执行槽数。
func (p *ManagedPool) freeSlots() int { return p.MaxConcurrent - p.Running }

// ---------- 资源 ----------

// Resource 是调度的一个资源：一台 Native Runner 或一个托管池（恰好一个不为 nil）。
type Resource struct {
	Native  *NativeRunner
	Managed *ManagedPool
}

// owned 判断是自有机器（边际成本为 0，PLAN #15）。
func (r Resource) owned() bool { return r.Native != nil }

// Label 返回写在原因中的资源名。
func (r Resource) Label() string {
	if r.Native != nil {
		return "Runner " + r.Native.Name
	}
	return r.Managed.label()
}

func (r Resource) same(o Resource) bool {
	if r.Native != nil {
		return o.Native != nil && o.Native.ID == r.Native.ID
	}
	return o.Managed != nil && o.Managed.Key == r.Managed.Key
}

// Mismatch 返回资源不能执行 req 的原因（空表示可以）。
func (r Resource) Mismatch(req Requirements) string {
	if r.Native != nil {
		return r.Native.Mismatch(req)
	}
	return r.Managed.Mismatch(req)
}

// SlotFree 判断资源还能再接一个 job。
func (r Resource) SlotFree() bool {
	if r.Native != nil {
		return r.Native.SlotFree()
	}
	return r.Managed.SlotFree()
}

// Fits 判断资源现在能否放下 req。
func (r Resource) Fits(req Requirements) bool {
	if r.Native != nil {
		return r.Native.Fits(req)
	}
	return r.Managed.Fits(req)
}

// Add 把 req 计入资源的账本。
func (r Resource) Add(req Requirements) {
	if r.Native != nil {
		r.Native.Add(req)
		return
	}
	r.Managed.Running++
}

// degraded 判断资源处于降级状态（Native Runner 没有降级状态）。
func (r Resource) degraded() bool { return r.Managed != nil && r.Managed.State == PoolDegraded }

// takesNow 判断资源现在就能接 req（择优时只与这样的资源比较）：Native 在线、未暂停、没有独占 job、槽位和已知的空闲容量够；
// 托管池可用、槽位和执行槽资源够。
func (r Resource) takesNow(req Requirements) bool {
	if r.Mismatch(req) != "" || !r.SlotFree() {
		return false
	}
	if f := r.Native; f != nil {
		if !f.Online || f.Drain || f.Exclusive > 0 || (req.Exclusive && f.Running > 0) {
			return false
		}
		free, known := f.Free(string(req.Executor))
		return known && free.Covers(req.Resources)
	}
	return r.Managed.Fits(req)
}

// outranks 判断 a 按择优规则严格优于 b（PLAN #32.5）：1. 自有机器优先于托管；2. ready 优先于 degraded；
// 3. 空闲更多者优先（Native 按 executor 的空闲容量，CPU 优先、内存其次；托管按空闲的执行槽）。
// 规则 4（并列时按 Runner ID、池的 Key）由 Snapshot.Better 的遍历顺序实现。
func outranks(a, b Resource, req Requirements) bool {
	if a.owned() != b.owned() {
		return a.owned()
	}
	if a.degraded() != b.degraded() {
		return !a.degraded()
	}
	if a.owned() {
		fa, _ := a.Native.Free(string(req.Executor))
		fb, _ := b.Native.Free(string(req.Executor))
		return fb.Less(fa)
	}
	return a.Managed.freeSlots() > b.Managed.freeSlots()
}

// spare 返回写在"等待更合适的资源"原因中的空闲情况。
func (r Resource) spare(req Requirements) string {
	if r.Native != nil {
		free, _ := r.Native.Free(string(req.Executor))
		return "空闲 " + nonNegative(free).String()
	}
	return fmt.Sprintf("空闲 %d 个执行槽", r.Managed.freeSlots())
}

// ---------- 快照 ----------

// Snapshot 是一次预留时全部资源的快照：active 的 Native Runner（只来自 ci_runners，AI 执行节点不会出现）和托管池。
type Snapshot struct {
	Natives map[int64]*NativeRunner
	Pools   map[string]*ManagedPool
}

// resources 按确定的顺序（Native 按 ID，托管池按 Key）返回全部资源。
func (s *Snapshot) resources() []Resource {
	out := make([]Resource, 0, len(s.Natives)+len(s.Pools))
	for _, id := range slices.Sorted(maps.Keys(s.Natives)) {
		out = append(out, Resource{Native: s.Natives[id]})
	}
	for _, k := range slices.Sorted(maps.Keys(s.Pools)) {
		out = append(out, Resource{Managed: s.Pools[k]})
	}
	return out
}

// Better 返回另一个现在就能接 req、并且按择优规则严格优于 self 的资源中最好的一个（没有时 ok 为 false）。
// 与 self 并列时不让：两个同样合适的资源不会互相等待。
func (s *Snapshot) Better(self Resource, req Requirements) (best Resource, ok bool) {
	for _, r := range s.resources() {
		if r.same(self) || !r.takesNow(req) || !outranks(r, self, req) {
			continue
		}
		// 按 resources 的顺序遍历：并列时保留先遇到的（ID、Key 较小者，规则 4）
		if !ok || outranks(r, best, req) {
			best, ok = r, true
		}
	}
	return best, ok
}

// WaitReason 返回没有资源能放下 req 时的等待原因：需要多少，各兼容资源的容量和当前空闲。
func (s *Snapshot) WaitReason(req Requirements) string {
	need := req.Resources
	var lines []string
	everFits := false
	for _, r := range s.resources() {
		if r.Mismatch(req) != "" {
			continue
		}
		if p := r.Managed; p != nil {
			if p.PerJob.Covers(need) {
				everFits = true
			}
			lines = append(lines, fmt.Sprintf("%s 每个执行槽 %s", p.label(), p.PerJob))
			continue
		}
		f := r.Native
		capa, known := f.capacityFor(string(req.Executor))
		free, _ := f.Free(string(req.Executor))
		switch {
		case known:
			if capa.Covers(need) {
				everFits = true
			}
			lines = append(lines, fmt.Sprintf("%s 容量 %s、空闲 %s", f.Name, capa, nonNegative(free)))
		case f.LegacyFree != nil:
			everFits = true // 旧版 Runner 不知道容量
			lines = append(lines, fmt.Sprintf("%s 空闲 %s（旧版 Runner，没有上报容量）", f.Name, nonNegative(free)))
		}
	}
	if len(lines) > maxReasonResources {
		lines = append(lines[:maxReasonResources], fmt.Sprintf("等 %d 台", len(lines)))
	}
	detail := strings.Join(lines, "；")
	if detail == "" {
		detail = "没有兼容的 Runner"
	}
	if !everFits {
		return fmt.Sprintf("没有 Runner 的容量满足 job 声明的 resources（需要 %s）：%s。调小 resources 或增加更大的机器", need, detail)
	}
	return fmt.Sprintf("等待资源释放（需要 %s）：%s", need, detail)
}

func nonNegative(r store.Resources) store.Resources {
	return store.Resources{CPU: max(r.CPU, 0), Memory: max(r.Memory, 0)}
}

// ---------- 选择器 ----------

// QuotaCounts 是一个候选 job 的共用配额现状（在持有仓库配额锁的事务中读取，不含本次已选中的）。
type QuotaCounts struct {
	RepoID int64
	// RepoRunning 是仓库中 running 的 job 数（预留也计入），RepoMax 是仓库的 max_concurrency（0 为不限）。
	RepoRunning, RepoMax int
	// Siblings 是同一个矩阵中 running 的变体数（job 没有 max-parallel 时为 0）。
	Siblings int
}

// Quotas 读取候选 job 的共用配额现状（Reserver 用数据库实现，核心的单元测试用内存实现）。
type Quotas interface {
	Counts(j *cistore.Job) (QuotaCounts, error)
}

// Selector 在一次预留中逐个考察候选 job（按队列顺序），决定哪些交给触发方的资源 self（PLAN #32.5）。
type Selector struct {
	snap   *Snapshot
	self   Resource
	now    time.Time
	max    int
	quotas Quotas
	// busy 表示 self 在本次之前已有正在执行的 job（Native：Runner 上报的或 Server 账本中的），独占的 job 要等它空闲。
	busy bool
	// Chosen 是选中的 job（按选中的顺序）。
	Chosen []*cistore.Job
	// Skip 是没有选中的 job → 原因。
	Skip map[int64]string
	// RetryAt 是为了让更合适的资源领取而跳过的 job 中最早可以不再跳过的时间（零值表示没有）。
	RetryAt time.Time
	perRepo map[int64]int // 本次已选中的 job 按仓库计数
}

// NewSelector 返回为 self 选 job 的选择器，最多选 maxJobs 个。reportedRunning 是触发方自己上报的正在执行的 job 数
// （Native Runner 的上报；托管为 0）。
func NewSelector(snap *Snapshot, self Resource, now time.Time, maxJobs, reportedRunning int, quotas Quotas) *Selector {
	busy := reportedRunning > 0
	if self.Native != nil {
		busy = busy || self.Native.Running > 0
	}
	return &Selector{
		snap: snap, self: self, now: now, max: maxJobs, quotas: quotas, busy: busy,
		Skip: map[int64]string{}, perRepo: map[int64]int{},
	}
}

// Blocked 判断 self 现在不能接任何 job：正在执行独占的 job（host + secret，PLAN #8.5），或者按 Server 的账本没有空闲槽位
// （不只信触发方请求的数量）。
func (p *Selector) Blocked() bool {
	return (p.self.Native != nil && p.self.Native.Exclusive > 0) || !p.self.SlotFree()
}

// Full 判断已经选够（达到请求的数量或者槽位用完）。
func (p *Selector) Full() bool { return len(p.Chosen) >= p.max || !p.self.SlotFree() }

// Consider 考察一个候选 job：可以选时加入 Chosen，否则在 Skip 中记录原因。返回 true 表示不再继续考察（选中了独占的 job）。
// 硬约束的顺序：pool、平台与 executor、能力 → 独占 → 资源 → 仓库 max_concurrency → 矩阵 max-parallel；之后按择优规则让位。
func (p *Selector) Consider(j *cistore.Job) (bool, error) {
	req := RequirementsOf(j)
	if why := p.self.Mismatch(req); why != "" {
		p.Skip[j.ID] = why
		return false, nil
	}
	// 独占只作用于 Native（托管池不能有 host executor，每个托管 job 本来就独占自己的执行环境）
	exclusive := req.Exclusive && p.self.Native != nil
	if exclusive && (len(p.Chosen) > 0 || p.busy) {
		p.Skip[j.ID] = "引用了 secret 的 host job 需要独占 Runner，等待 Runner 空闲"
		return false, nil
	}
	// 每次分配都检查声明的资源，不因为资源空闲而超过容量
	if !p.self.Fits(req) {
		p.Skip[j.ID] = p.snap.WaitReason(req)
		return false, nil
	}
	q, err := p.quotas.Counts(j)
	if err != nil {
		return false, err
	}
	if q.RepoMax > 0 && q.RepoRunning+p.perRepo[q.RepoID] >= q.RepoMax {
		p.Skip[j.ID] = fmt.Sprintf("仓库并发上限 %d", q.RepoMax)
		return false, nil
	}
	if j.Spec.MaxParallel > 0 {
		sib := q.Siblings
		for _, c := range p.Chosen {
			if c.JobName == j.JobName && c.RunID == j.RunID {
				sib++
			}
		}
		if sib >= j.Spec.MaxParallel {
			p.Skip[j.ID] = fmt.Sprintf("矩阵 max-parallel = %d", j.Spec.MaxParallel)
			return false, nil
		}
	}
	// 择优：另一个现在就能接的资源排名更靠前时，先让它领取（排队超过 SchedDelay 后不再让，避免那个资源实际领不到时一直等）
	if j.QueuedAt != nil && p.now.Sub(*j.QueuedAt) < SchedDelay {
		if b, ok := p.snap.Better(p.self, req); ok {
			p.Skip[j.ID] = fmt.Sprintf("等待更合适的 %s 领取（%s）", b.Label(), b.spare(req))
			if at := j.QueuedAt.Add(SchedDelay); p.RetryAt.IsZero() || at.Before(p.RetryAt) {
				p.RetryAt = at
			}
			return false, nil
		}
	}
	p.Chosen = append(p.Chosen, j)
	p.perRepo[q.RepoID]++
	p.self.Add(req)
	return exclusive, nil
}

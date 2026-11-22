package scheduler

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 调度核心的单元测试（PLAN #32.10）：托管池的匹配原因、择优的四级顺序，以及固定种子的随机快照检查选择器的不变量。

func ociJob(sp workflow.Spec) Requirements {
	sp.Executor, sp.Platform = workflow.ExecutorOCI, "linux/arm64"
	return RequirementsOf(&cistore.Job{Executor: sp.Executor, Platform: sp.Platform, Spec: sp})
}

// TestManagedPoolMismatch：托管池不能接 job 的各种原因，按硬约束的顺序（可用 → pool → 平台与 executor → 能力）。
func TestManagedPoolMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(p *ManagedPool)
		req   Requirements
		want  string // 空表示可以执行
	}{
		{"可以执行", nil, ociJob(workflow.Spec{}), ""},
		{"停用", func(p *ManagedPool) { p.State = PoolDisabled }, ociJob(workflow.Spec{}), "已停用"},
		{"降级仍可执行", func(p *ManagedPool) { p.State = PoolDegraded }, ociJob(workflow.Spec{}), ""},
		{"预算用完", func(p *ManagedPool) { p.Unavailable = "本月预算已用完" }, ociJob(workflow.Spec{}), "本月预算已用完"},
		{"不能声明 host", func(p *ManagedPool) { p.Executors = append(p.Executors, "host") }, ociJob(workflow.Spec{}), "host"},
		{"没有并发上限", func(p *ManagedPool) { p.MaxConcurrent = 0 }, ociJob(workflow.Spec{}), "并发上限"},
		{"pool 不匹配", nil, ociJob(workflow.Spec{Pool: "gpu"}), "只允许在 pool gpu 上执行"},
		{"pool 指向托管池", nil, ociJob(workflow.Spec{Pool: testPoolName}), ""},
		{"host job", nil, RequirementsOf(&cistore.Job{Executor: workflow.ExecutorHost, Platform: "linux/arm64"}), "不能用 host 执行"},
		{"没有的平台", nil, RequirementsOf(&cistore.Job{Executor: workflow.ExecutorOCI, Platform: "linux/riscv64"}), "不能用 oci 执行 linux/riscv64"},
		{"缺少 buildkit", nil, ociJob(workflow.Spec{BuildkitImage: "bk"}), "不支持 buildkit"},
		{"缺少 services", func(p *ManagedPool) { p.Caps = nil }, ociJob(workflow.Spec{Services: []workflow.ServiceSpec{{Name: "pg"}}}), "不支持 services"},
		{"缺少 effects", func(p *ManagedPool) { p.Caps = []string{CapServices} }, RequirementsOf(&cistore.Job{Executor: workflow.ExecutorOCI, Platform: "linux/arm64", Effects: true}), "不支持 effects"},
		{"停用优先于其他原因", func(p *ManagedPool) { p.State = PoolDisabled }, ociJob(workflow.Spec{Pool: "gpu"}), "已停用"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := pool(testPoolKey, 2)
			if tc.setup != nil {
				tc.setup(&p)
			}
			got := p.Mismatch(tc.req)
			if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("Mismatch = %q，期望包含 %q", got, tc.want)
			}
		})
	}
	if (&NativeRunner{Name: "n", Pool: "default", Platforms: []string{"linux/arm64"}, Executors: []string{"oci"}}).Mismatch(
		RequirementsOf(&cistore.Job{
			Executor: workflow.ExecutorOCI, Platform: "linux/arm64", Effects: true,
			Spec: workflow.Spec{Services: []workflow.ServiceSpec{{Name: "pg"}}},
		})) != "" {
		t.Fatal("Native Runner 一律具备 services、effects")
	}
}

// TestBetterRanking：择优的四级顺序：自有机器优先、ready 优先于 degraded、空闲更多者优先、并列时按 ID / Key；与 self
// 并列时不让。
func TestBetterRanking(t *testing.T) {
	t.Parallel()
	req := ociJob(workflow.Spec{CPU: 1, MemoryBytes: gib})
	native := func(id int64, cpu float64) *NativeRunner {
		return &NativeRunner{
			ID: id, Name: fmt.Sprintf("n%d", id), Pool: "default", Online: true, Platforms: []string{"linux/arm64"}, Executors: []string{"oci"},
			Cap: &store.RunnerCapacity{Machine: store.Resources{CPU: cpu, Memory: 64 * gib}}, Used: map[string]store.Resources{},
		}
	}
	managed := func(key string, slots int, state PoolState) *ManagedPool {
		p := pool(key, slots, func(p *ManagedPool) { p.State = state; p.Pool = "default" })
		return &p
	}
	snap := func(ns []*NativeRunner, ps ...*ManagedPool) *Snapshot {
		s := &Snapshot{Natives: map[int64]*NativeRunner{}, Pools: map[string]*ManagedPool{}}
		for _, n := range ns {
			s.Natives[n.ID] = n
		}
		for _, p := range ps {
			s.Pools[p.Key] = p
		}
		return s
	}
	label := func(r Resource, ok bool) string {
		if !ok {
			return ""
		}
		return r.Label()
	}
	tests := []struct {
		name string
		snap *Snapshot
		self func(s *Snapshot) Resource
		want string
	}{
		{
			"自有机器优先于托管（托管空闲更多也不让）", snap([]*NativeRunner{native(1, 2)}, managed("a", 100, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["a"]} }, "Runner n1",
		},
		{
			"Native 不让给托管", snap([]*NativeRunner{native(1, 2)}, managed("a", 100, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Native: s.Natives[1]} }, "",
		},
		{
			"ready 优先于 degraded", snap(nil, managed("a", 10, PoolDegraded), managed("b", 1, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["a"]} }, "托管池 b",
		},
		{
			"degraded 不比 ready 优先", snap(nil, managed("a", 1, PoolReady), managed("b", 10, PoolDegraded)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["a"]} }, "",
		},
		{
			"空闲槽位更多者优先", snap(nil, managed("a", 1, PoolReady), managed("b", 3, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["a"]} }, "托管池 b",
		},
		{
			"与 self 并列不让", snap(nil, managed("a", 2, PoolReady), managed("b", 2, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["b"]} }, "",
		},
		{
			"并列时 Key 小者", snap(nil, managed("s", 1, PoolReady), managed("d", 3, PoolReady), managed("c", 3, PoolReady)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["s"]} }, "托管池 c",
		},
		{
			"Native 并列时 ID 小者", snap([]*NativeRunner{native(5, 2), native(9, 8), native(7, 8)}),
			func(s *Snapshot) Resource { return Resource{Native: s.Natives[5]} }, "Runner n7",
		},
		{
			"不可用的池不参与", snap([]*NativeRunner{native(1, 2)}, managed("a", 1, PoolReady), managed("b", 9, PoolDisabled)),
			func(s *Snapshot) Resource { return Resource{Managed: s.Pools["a"]} }, "Runner n1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := label(tc.snap.Better(tc.self(tc.snap), req)); got != tc.want {
				t.Fatalf("Better = %q，期望 %q", got, tc.want)
			}
		})
	}
	// 离线、暂停的 Native Runner 不参与
	s := snap([]*NativeRunner{native(1, 2)}, managed("a", 1, PoolReady))
	s.Natives[1].Online = false
	if b, ok := s.Better(Resource{Managed: s.Pools["a"]}, req); ok {
		t.Fatalf("离线的 Runner 不应让：%s", b.Label())
	}
}

// fakeQuotas 是内存中的配额现状：每个 job 所在的仓库、仓库已运行数和上限。
type fakeQuotas struct {
	repoOf  map[int64]int64
	running map[int64]int
	max     map[int64]int
}

func (q *fakeQuotas) Counts(j *cistore.Job) (QuotaCounts, error) {
	r := q.repoOf[j.ID]
	return QuotaCounts{RepoID: r, RepoRunning: q.running[r], RepoMax: q.max[r]}, nil
}

// cloneSnapshot 深拷贝快照（选择器会修改账本）。
func cloneSnapshot(s *Snapshot) *Snapshot {
	out := &Snapshot{Natives: map[int64]*NativeRunner{}, Pools: map[string]*ManagedPool{}}
	for id, n := range s.Natives {
		c := *n
		c.Used = maps.Clone(n.Used)
		out.Natives[id] = &c
	}
	for k, p := range s.Pools {
		c := *p
		out.Pools[k] = &c
	}
	return out
}

// randomSnapshot 生成随机的资源：Native Runner 有重复的规格（产生并列），部分已在执行；托管池的规格、能力随机。
func randomSnapshot(rng *rand.Rand) *Snapshot {
	s := &Snapshot{Natives: map[int64]*NativeRunner{}, Pools: map[string]*ManagedPool{}}
	execs := [][]string{{"oci"}, {"host", "oci"}, {"oci", workflow.CapBuildkit}}
	for i := range 1 + rng.IntN(4) {
		n := &NativeRunner{
			ID: int64(i + 1), Name: fmt.Sprintf("n%d", i+1), Pool: "default", Online: rng.IntN(5) > 0,
			Platforms: []string{"linux/arm64"}, Executors: execs[rng.IntN(len(execs))], Used: map[string]store.Resources{},
			Cap: &store.RunnerCapacity{Machine: store.Resources{CPU: float64(2 * (1 + rng.IntN(2))), Memory: 8 * gib}, MaxJobs: 1 + rng.IntN(4)},
		}
		if rng.IntN(3) == 0 {
			n.Running, n.Used["oci"] = 1, store.Resources{CPU: 1, Memory: gib}
		}
		if rng.IntN(8) == 0 {
			n.Exclusive, n.Running = 1, n.Running+1
		}
		s.Natives[n.ID] = n
	}
	for i := range rng.IntN(3) {
		p := pool(fmt.Sprintf("p%d", i), 1+rng.IntN(3), func(p *ManagedPool) {
			p.Pool = "default"
			p.PerJob = store.Resources{CPU: float64(1 + rng.IntN(3)), Memory: 4 * gib}
			p.Running = rng.IntN(2)
			if rng.IntN(2) == 0 {
				p.State = PoolDegraded
			}
		})
		s.Pools[p.Key] = &p
	}
	return s
}

// randomJobs 生成随机的候选 job（按队列顺序）和它们所在的仓库。
func randomJobs(rng *rand.Rand, now time.Time, q *fakeQuotas) []*cistore.Job {
	var out []*cistore.Job
	for i := range 12 {
		sp := workflow.Spec{CPU: float64(rng.IntN(3)), MemoryBytes: int64(rng.IntN(3)) * gib}
		exec := workflow.ExecutorOCI
		if rng.IntN(4) == 0 {
			exec = workflow.ExecutorHost
			if rng.IntN(2) == 0 {
				sp.Secrets = []string{"TOKEN"}
			}
		}
		if rng.IntN(6) == 0 {
			sp.BuildkitImage = "bk"
		}
		queued := now.Add(-time.Duration(rng.IntN(20)) * time.Second)
		id := int64(i + 1)
		out = append(out, &cistore.Job{ID: id, RunID: 1, JobName: fmt.Sprintf("j%d", i), Executor: exec, Platform: "linux/arm64", Spec: sp, QueuedAt: &queued})
		q.repoOf[id] = int64(1 + rng.IntN(2))
	}
	return out
}

// selectAll 用快照 s 为 self 运行一次选择，返回选中的 job 和跳过原因。
func selectAll(s *Snapshot, self func(*Snapshot) Resource, jobs []*cistore.Job, now time.Time, maxJobs int, q *fakeQuotas) (*Selector, Resource) {
	r := self(s)
	sel := NewSelector(s, r, now, maxJobs, 0, q)
	if sel.Blocked() {
		return sel, r
	}
	for _, j := range jobs {
		if sel.Full() {
			break
		}
		if stop, _ := sel.Consider(j); stop {
			break
		}
	}
	return sel, r
}

// TestSelectorRandomSnapshots：固定种子的随机快照。对每个资源作为触发方运行选择器，检查：选中的 job 不超过容量、槽位，
// 独占的 job 单独占用一台空闲的 Runner，托管池不超过并发上限、每个 job 放得进一个执行槽，仓库并发不超过上限；
// 相同输入的选择结果和跳过原因相同。
func TestSelectorRandomSnapshots(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(137, 32))
	now := time.Unix(1_700_000_000, 0)
	for iter := range 300 {
		q := &fakeQuotas{repoOf: map[int64]int64{}, running: map[int64]int{1: rng.IntN(2)}, max: map[int64]int{1: 1 + rng.IntN(3), 2: 0}}
		base := randomSnapshot(rng)
		jobs := randomJobs(rng, now, q)
		maxJobs := 1 + rng.IntN(4)
		var selves []func(*Snapshot) Resource
		for _, id := range slices.Sorted(maps.Keys(base.Natives)) {
			selves = append(selves, func(s *Snapshot) Resource { return Resource{Native: s.Natives[id]} })
		}
		for _, k := range slices.Sorted(maps.Keys(base.Pools)) {
			selves = append(selves, func(s *Snapshot) Resource { return Resource{Managed: s.Pools[k]} })
		}
		for si, self := range selves {
			before := self(base)
			sel, r := selectAll(cloneSnapshot(base), self, jobs, now, maxJobs, q)
			again, _ := selectAll(cloneSnapshot(base), self, jobs, now, maxJobs, q)
			ctx := fmt.Sprintf("第 %d 轮、触发方 %d（%s）", iter, si, r.Label())
			if !slices.Equal(ids(sel.Chosen), ids(again.Chosen)) || !maps.Equal(sel.Skip, again.Skip) {
				t.Fatalf("%s：相同输入的结果不同：%v / %v\n%v\n%v", ctx, ids(sel.Chosen), ids(again.Chosen), sel.Skip, again.Skip)
			}
			checkSelection(t, ctx, before, sel.Chosen, maxJobs, q)
		}
	}
}

func ids(jobs []*cistore.Job) []int64 {
	out := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}

// checkSelection 检查选择结果的不变量；before 是选择之前的资源（选择器修改的是它的拷贝）。
func checkSelection(t *testing.T, ctx string, before Resource, chosen []*cistore.Job, maxJobs int, q *fakeQuotas) {
	t.Helper()
	if len(chosen) > maxJobs {
		t.Fatalf("%s：选中 %d 个，超过请求的 %d 个", ctx, len(chosen), maxJobs)
	}
	perRepo := map[int64]int{}
	for _, j := range chosen {
		perRepo[q.repoOf[j.ID]]++
	}
	for repo, n := range perRepo {
		if m := q.max[repo]; m > 0 && q.running[repo]+n > m {
			t.Fatalf("%s：仓库 %d 已运行 %d、选中 %d，超过上限 %d", ctx, repo, q.running[repo], n, m)
		}
	}
	if p := before.Managed; p != nil {
		if len(chosen) > 0 && p.Running+len(chosen) > p.MaxConcurrent {
			t.Fatalf("%s：池已有 %d、选中 %d，超过上限 %d", ctx, p.Running, len(chosen), p.MaxConcurrent)
		}
		for _, j := range chosen {
			if req := RequirementsOf(j); !p.PerJob.Covers(req.Resources) || req.Executor == workflow.ExecutorHost {
				t.Fatalf("%s：池接了放不下或 host 的 job %d（%+v）", ctx, j.ID, req)
			}
		}
		return
	}
	n := before.Native
	if n.Exclusive > 0 && len(chosen) > 0 {
		t.Fatalf("%s：Runner 正在执行独占 job，仍选中 %v", ctx, ids(chosen))
	}
	if n.Cap.MaxJobs > 0 && len(chosen) > 0 && n.Running+len(chosen) > n.Cap.MaxJobs {
		t.Fatalf("%s：Runner 已有 %d、选中 %d，超过 max_jobs %d", ctx, n.Running, len(chosen), n.Cap.MaxJobs)
	}
	used := maps.Clone(n.Used)
	var total store.Resources
	for _, u := range used {
		total = total.Add(u)
	}
	for _, j := range chosen {
		req := RequirementsOf(j)
		if req.Exclusive && (len(chosen) > 1 || n.Running > 0) {
			t.Fatalf("%s：独占的 job %d 没有单独占用空闲的 Runner（选中 %v，已有 %d）", ctx, j.ID, ids(chosen), n.Running)
		}
		used[string(req.Executor)] = used[string(req.Executor)].Add(req.Resources)
		total = total.Add(req.Resources)
		if !n.Cap.ExecutorCapacity(string(req.Executor)).Covers(used[string(req.Executor)]) || !n.Cap.Allocatable().Covers(total) {
			t.Fatalf("%s：选中 %v 后超过容量（%s 用 %s，合计 %s）", ctx, ids(chosen), req.Executor, used[string(req.Executor)], total)
		}
	}
}

package runnerapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 领取即 running（PLAN #14.1）：同一个事务中置为 running、epoch+1、签发租约、创建 attempt，并写入 Check Run 同步记录。
func TestService_Acquire_claim(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id := h.pendingJob(func(j *jobRow) {
		j.spec.Secrets = []string{"TOKEN"}
		j.spec.Env = map[string]string{"A": "1"}
	})
	before := time.Now()
	resp, err := r.h.client.Acquire(t.Context(), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 1, RequestId: "req-claim"}))
	if err != nil {
		t.Fatalf("Acquire：%v", err)
	}
	if len(resp.Msg.Jobs) != 1 {
		t.Fatalf("应领到 1 个 job，实际 %d 个", len(resp.Msg.Jobs))
	}
	spec := resp.Msg.Jobs[0]
	s := h.job(id)
	if s.State != store.JobRunning || s.Epoch != 1 || s.AttemptCount != 1 || s.Attempt == nil {
		t.Fatalf("领取后 job 应为 running、epoch 1、第 1 次尝试：%+v", s)
	}
	if s.Lease == nil || s.Lease.Before(before.Add(testLeaseTTL-5*time.Second)) || s.Lease.After(time.Now().Add(testLeaseTTL+5*time.Second)) {
		t.Fatalf("租约应约为 %v 后到期：%v", testLeaseTTL, s.Lease)
	}
	if s.Reason != "在 Runner "+r.name+" 上执行" {
		t.Fatalf("原因：%q", s.Reason)
	}
	if spec.Ref.AttemptId != *s.Attempt || spec.Ref.Epoch != 1 || spec.JobId != id {
		t.Fatalf("JobSpec 的 Ref %+v 应与 attempt %d、epoch 1 一致", spec.Ref, *s.Attempt)
	}
	if spec.LeaseTtlSeconds != int32(testLeaseTTL/time.Second) || spec.TimeoutSeconds != 600 || spec.Executor.Kind != "oci" ||
		len(spec.Steps) != 1 || spec.Env["A"] != "1" || len(spec.SecretNames) != 1 || !spec.NeedsCredentials {
		t.Fatalf("JobSpec 内容不对：%+v", spec)
	}
	var runner int64
	var requestID string
	if err := h.db.QueryRow(t.Context(), `SELECT runner_id, acquire_request_id FROM attempts WHERE id=$1 AND job_id=$2 AND epoch=1`, *s.Attempt, id).
		Scan(&runner, &requestID); err != nil {
		t.Fatalf("应创建 attempt：%v", err)
	}
	if runner != r.id || requestID != "req-claim" {
		t.Fatalf("attempt 的 Runner %d、request_id %q", runner, requestID)
	}
	var outbox, started int
	if err := h.db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM outbox WHERE dedupe_key=$1),
		(SELECT count(*) FROM run_reruns rr JOIN jobs j ON j.run_id=rr.run_id WHERE j.id=$2 AND rr.started_at IS NOT NULL)`,
		fmt.Sprintf("checkrun:job:%d", id), id).Scan(&outbox, &started); err != nil {
		t.Fatal(err)
	}
	if outbox != 1 || started != 1 {
		t.Fatalf("应写入 Check Run 同步记录（%d）并记录 rerun 开始时间（%d）", outbox, started)
	}
}

// 幂等领取（PLAN #6.1）：同一个 request_id 重试（例如响应丢失）时返回已经分配的那批 job，不再领取新的。
func TestService_Acquire_idempotentRequestID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id := h.pendingJob()
	acquire := func() []*runnerv1.JobSpec {
		t.Helper()
		resp, err := h.client.Acquire(t.Context(), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 2, RequestId: "req-same"}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.Jobs
	}
	first := acquire()
	if len(first) != 1 || first[0].JobId != id {
		t.Fatalf("第一次应领取 job %d：%+v", id, first)
	}
	h.pendingJob()
	again := acquire()
	if len(again) != 1 || again[0].Ref.AttemptId != first[0].Ref.AttemptId || again[0].Ref.Epoch != 1 {
		t.Fatalf("同一个 request_id 应返回同一个 attempt：%+v", again)
	}
	if s := h.job(id); s.Epoch != 1 || s.AttemptCount != 1 {
		t.Fatalf("重试不应再次领取：%+v", s)
	}
	// 这批 job 结束后再用同一个 request_id：不再返回它们，按新的请求领取
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, id)
	if next := acquire(); len(next) != 1 || next[0].JobId == id {
		t.Fatalf("已结束的 job 不应再返回：%+v", next)
	}
}

// 多个 pending job：按优先级领取，最多 max_jobs 个；max_jobs 为 0 时立即返回。
func TestService_Acquire_maxJobsAndPriority(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	run := h.addRun(h.addRepo())
	low := h.addJob(run, func(j *jobRow) { j.priority = 1 })
	high := h.addJob(run, func(j *jobRow) { j.priority = 3 })
	mid := h.addJob(run, func(j *jobRow) { j.priority = 2 })

	r.mustAcquire(0, 0)
	jobs := r.mustAcquire(2, 2)
	if jobs[0].JobId != high || jobs[1].JobId != mid {
		t.Fatalf("应按优先级领取 %d、%d，实际 %d、%d", high, mid, jobs[0].JobId, jobs[1].JobId)
	}
	if s := h.job(low); s.State != store.JobPending {
		t.Fatalf("超过 max_jobs 的 job 应保持 pending：%s", s.State)
	}
}

// 长轮询：没有 job 时等到超时返回空；等待期间有 job 进入队列（notify.Hub 唤醒）时立即领取。
func TestService_Acquire_longPoll(t *testing.T) {
	t.Parallel()
	t.Run("超时返回空", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t) // 长轮询 50ms
		r := h.enrollRunner()
		start := time.Now()
		r.mustAcquire(1, 0)
		if d := time.Since(start); d < 50*time.Millisecond {
			t.Fatalf("没有 job 时应等到长轮询超时，实际只等了 %v", d)
		}
	})
	t.Run("被唤醒后领取", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(o *harnessOpts) { o.longPoll = time.Minute })
		r := h.enrollRunner()
		run := h.addRun(h.addRepo())
		type result struct {
			resp *connect.Response[runnerv1.AcquireResponse]
			err  error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := h.client.Acquire(waitCtx(t, 30*time.Second), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 1, RequestId: "poll"}))
			done <- result{resp, err}
		}()
		// 先入队再唤醒：无论 Acquire 此时在查询还是在等待，都能领到这个 job
		id := h.addJob(run)
		h.hub.All()
		res := <-done
		if res.err != nil {
			t.Fatalf("Acquire：%v", res.err)
		}
		if len(res.resp.Msg.Jobs) != 1 || res.resp.Msg.Jobs[0].JobId != id {
			t.Fatalf("唤醒后应领到 job %d：%+v", id, res.resp.Msg.Jobs)
		}
	})
}

// 按 pool、平台、executor、buildkit 过滤：不匹配的 job 不领取。
func TestService_Acquire_filters(t *testing.T) {
	t.Parallel()
	oci, host := []string{"oci"}, []string{"host", "oci"}
	tests := []struct {
		name   string
		runner []func(*runnerOpts)
		job    []func(*jobRow)
		claim  bool
	}{
		{"oci linux", nil, nil, true},
		{"平台不支持", nil, []func(*jobRow){onPlatform(workflow.ExecutorOCI, "linux/amd64")}, false},
		{"executor 不支持", []func(*runnerOpts){withPlatforms([]string{"host"}, "linux/arm64")}, nil, false},
		{
			"oci 只执行 linux",
			[]func(*runnerOpts){withPlatforms(host, "darwin/arm64", "linux/arm64")},
			[]func(*jobRow){onPlatform(workflow.ExecutorOCI, "darwin/arm64")},
			false,
		},
		{
			"host 原生平台",
			[]func(*runnerOpts){withPlatforms(host, "darwin/arm64", "linux/arm64")},
			[]func(*jobRow){onPlatform(workflow.ExecutorHost, "darwin/arm64")},
			true,
		},
		{
			"host 非原生平台",
			[]func(*runnerOpts){withPlatforms(host, "darwin/arm64", "linux/arm64")},
			[]func(*jobRow){onPlatform(workflow.ExecutorHost, "linux/arm64")},
			false,
		},
		{"job 指定其他 pool", nil, []func(*jobRow){func(j *jobRow) { j.spec.Pool = "gpu" }}, false},
		{"job 指定本 pool", []func(*runnerOpts){withPool("gpu")}, []func(*jobRow){func(j *jobRow) { j.spec.Pool = "gpu" }}, true},
		{"没有指定 pool 的 job 任何 pool 都能领", []func(*runnerOpts){withPool("gpu")}, nil, true},
		{
			"buildkit 需要 Runner 开启",
			[]func(*runnerOpts){withPlatforms(oci, "linux/arm64")},
			[]func(*jobRow){func(j *jobRow) { j.spec.BuildkitImage = "moby/buildkit" }},
			false,
		},
		{
			"buildkit 已开启",
			[]func(*runnerOpts){withPlatforms([]string{"oci", workflow.CapBuildkit}, "linux/arm64")},
			[]func(*jobRow){func(j *jobRow) { j.spec.BuildkitImage = "moby/buildkit" }},
			true,
		},
		{
			"signing 需要 Runner 支持",
			[]func(*runnerOpts){withPlatforms([]string{"host"}, "darwin/arm64")},
			[]func(*jobRow){onPlatform(workflow.ExecutorHost, "darwin/arm64"), withSigning},
			false,
		},
		{
			"signing 已支持",
			[]func(*runnerOpts){withPlatforms([]string{"host", workflow.CapSigning}, "darwin/arm64")},
			[]func(*jobRow){onPlatform(workflow.ExecutorHost, "darwin/arm64"), withSigning},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.enrollRunner(tc.runner...)
			id := h.pendingJob(tc.job...)
			want := 0
			if tc.claim {
				want = 1
			}
			r.mustAcquire(1, want)
			if got := h.job(id).State; (got == store.JobRunning) != tc.claim {
				t.Fatalf("job 状态 %s，期望被领取 = %v", got, tc.claim)
			}
		})
	}
}

// 资源（旧版 Runner，没有上报容量）：按上报的空闲资源严格过滤（记录跳过原因），空闲时也一样；
// 上报的 CPU 不是有限值时当作没有上报，不按资源过滤。
func TestService_Acquire_resources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running int32
		free    *runnerv1.Resources
		claim   bool
	}{
		{"忙且资源不足", 1, &runnerv1.Resources{Cpu: 1, MemoryBytes: 8 << 30}, false},
		{"忙但资源足够", 1, &runnerv1.Resources{Cpu: 8, MemoryBytes: 8 << 30}, true},
		{"忙且内存不足", 1, &runnerv1.Resources{Cpu: 8, MemoryBytes: 1 << 30}, false},
		{"空闲时也按资源过滤", 0, &runnerv1.Resources{Cpu: 1, MemoryBytes: 1 << 30}, false},
		{"空闲且资源足够", 0, &runnerv1.Resources{Cpu: 4, MemoryBytes: 4 << 30}, true},
		{"CPU 为 NaN 时不按资源过滤", 1, &runnerv1.Resources{Cpu: math.NaN(), MemoryBytes: 1 << 30}, true},
		{"CPU 为无穷大时不按资源过滤", 1, &runnerv1.Resources{Cpu: math.Inf(1), MemoryBytes: 1 << 30}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.enrollRunner()
			r.state.RunningJobs, r.state.Free = tc.running, tc.free
			id := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 4, 4<<30 })
			want := 0
			if tc.claim {
				want = 1
			}
			r.mustAcquire(1, want)
			if !tc.claim {
				h.wantSkip(id, "需要 4 CPU / 4.0 GiB")
			}
			// touchRunner 记录上报的资源：非有限的 CPU 不写入（不能编码为 JSON）
			var caps map[string]any
			var raw []byte
			if err := h.db.QueryRow(t.Context(), `SELECT capabilities FROM ci_runners WHERE runner_id=$1`, r.id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &caps); err != nil {
				t.Fatal(err)
			}
			_, hasCPU := caps["free_cpu"]
			finite := !math.IsNaN(tc.free.Cpu) && !math.IsInf(tc.free.Cpu, 0)
			if hasCPU != finite || caps["free_memory"] == nil {
				t.Fatalf("capabilities = %v，期望包含 free_cpu = %v、包含 free_memory", caps, finite)
			}
		})
	}
}

// 仓库并发上限：同一仓库 running 的 job 数（包括本次已选中的）达到上限后不再领取。
func TestService_Acquire_repoConcurrency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	repo := h.addRepo(func(o *repoOpts) { o.maxConcurrency = 1 })
	run := h.addRun(repo)
	first := h.addJob(run, func(j *jobRow) { j.priority = 2 })
	second := h.addJob(run)
	other := h.pendingJob() // 另一个仓库，不受影响

	jobs := r.mustAcquire(3, 2)
	if jobs[0].JobId != first || jobs[1].JobId != other {
		t.Fatalf("应领取 %d 和其他仓库的 %d，实际 %d、%d", first, other, jobs[0].JobId, jobs[1].JobId)
	}
	h.wantSkip(second, "仓库并发上限 1")

	// 下一次领取时第一个仍在运行：仍然跳过
	r.mustAcquire(1, 0)
	h.wantSkip(second, "仓库并发上限 1")
}

// 矩阵的 max-parallel：同一个 job 名的变体同时运行的数量受限。
func TestService_Acquire_maxParallel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	run := h.addRun(h.addRepo())
	variant := func(os string) func(*jobRow) {
		return func(j *jobRow) {
			j.name, j.matrix, j.spec.MaxParallel = "build", map[string]any{"os": os}, 1
		}
	}
	a := h.addJob(run, variant("a"))
	b := h.addJob(run, variant("b"))
	jobs := r.mustAcquire(2, 1)
	if jobs[0].JobId != a {
		t.Fatalf("应领取 %d，实际 %d", a, jobs[0].JobId)
	}
	h.wantSkip(b, "矩阵 max-parallel = 1")
}

// 引用了 secret 的 host job 独占 Runner（PLAN #8.5）：Runner 有其他 job 时不领取；领取后不再领其他 job。
func TestService_Acquire_exclusiveHostJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner(withPlatforms([]string{"host"}, "darwin/arm64"))
	run := h.addRun(h.addRepo())
	hostJob := func(secret bool, priority int) func(*jobRow) {
		return func(j *jobRow) {
			j.spec.Executor, j.spec.Platform, j.priority = workflow.ExecutorHost, "darwin/arm64", priority
			if secret {
				j.spec.Secrets = []string{"TOKEN"}
			}
		}
	}
	exclusive := h.addJob(run, hostJob(true, 3))
	plain := h.addJob(run, hostJob(false, 1))

	// Runner 上报正在执行其他 job：独占 job 跳过，普通 job 可以领
	r.state.RunningJobs = 1
	jobs := r.mustAcquire(2, 1)
	if jobs[0].JobId != plain {
		t.Fatalf("Runner 忙时应只领取普通 job %d，实际 %d", plain, jobs[0].JobId)
	}
	h.wantSkip(exclusive, "需要独占 Runner")

	// 普通 job 还在这台 Runner 上运行：仍然跳过
	r.state.RunningJobs = 0
	r.mustAcquire(2, 0)
	h.wantSkip(exclusive, "需要独占 Runner")

	// 空闲后单独领取独占 job；之后在它结束前不再领取任何 job
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, plain)
	h.addJob(run, hostJob(false, 2))
	jobs = r.mustAcquire(3, 1)
	if jobs[0].JobId != exclusive {
		t.Fatalf("空闲时应单独领取独占 job %d，实际 %d", exclusive, jobs[0].JobId)
	}
	r.mustAcquire(3, 0)
}

// 暂停接单（drain）的 Runner 领不到 job，响应中带 drain 指令；吊销的 Runner 收到 RUNNER_REVOKED。
func TestService_Acquire_drainAndRevoked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	id := h.pendingJob()

	h.exec(`UPDATE runners SET drain=true WHERE id=$1`, r.id)
	resp, err := r.acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Jobs) != 0 || !resp.Directives.GetDrain() {
		t.Fatalf("drain 时不应领取且应下发 drain 指令：%d 个 job，directives %+v", len(resp.Jobs), resp.Directives)
	}

	h.exec(`UPDATE runners SET drain=false, state='revoked' WHERE id=$1`, r.id)
	if _, err := r.acquire(1); !rpcerr.IsRunnerRevoked(err) {
		t.Fatalf("吊销后应返回 RUNNER_REVOKED：%v", err)
	}
	if s := h.job(id); s.State != store.JobPending {
		t.Fatalf("job 不应被领取：%s", s.State)
	}

	h.exec(`UPDATE runners SET state='active' WHERE id=$1`, r.id)
	r.mustAcquire(1, 1) // 恢复后可以领取
}

// 记录在线：每次 Acquire 更新 last_seen_at、上报的平台、版本和正在执行的 job 数。
func TestService_Acquire_touchRunner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	h.exec(`UPDATE runners SET last_seen_at=now()-interval '1 hour' WHERE id=$1`, r.id)
	r.state.Version, r.state.RunningJobs = "v2", 3
	r.state.HostFingerprint = map[string]string{"os": "linux"}
	r.mustAcquire(0, 0)
	var version string
	var running int
	var seen time.Time
	if err := h.db.QueryRow(t.Context(), `SELECT r.version, c.running_jobs, r.last_seen_at FROM runners r JOIN ci_runners c ON c.runner_id=r.id WHERE r.id=$1`, r.id).Scan(&version, &running, &seen); err != nil {
		t.Fatal(err)
	}
	if version != "v2" || running != 3 || time.Since(seen) > time.Minute {
		t.Fatalf("Runner 记录没有更新：version=%s running=%d last_seen=%v", version, running, seen)
	}
}

// withSigning 给 job 加上 signing（PLAN #8.10）。
func withSigning(j *jobRow) {
	j.spec.Signing = &workflow.Signing{
		Certificates:         []workflow.SigningCertificate{{P12: "${{ secrets.P12 }}", Password: "${{ secrets.P12_PASSWORD }}"}},
		ProvisioningProfiles: []string{"${{ secrets.PROFILE }}"},
		Notary:               &workflow.Notary{Profile: "n", KeyID: "K1", Key: "${{ secrets.ASC_KEY }}", Issuer: "i"},
	}
}

// 下发给 Runner 的 JobSpec 中带上 signing，secret 仍是占位符。
func TestService_Acquire_signingSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner(withPlatforms([]string{"host", workflow.CapSigning}, "darwin/arm64"))
	h.pendingJob(onPlatform(workflow.ExecutorHost, "darwin/arm64"), withSigning)
	specs := r.mustAcquire(1, 1)
	got := specs[0].GetSigning()
	want := &runnerv1.Signing{
		Certificates:         []*runnerv1.SigningCertificate{{P12: "${{ secrets.P12 }}", Password: "${{ secrets.P12_PASSWORD }}"}},
		ProvisioningProfiles: []string{"${{ secrets.PROFILE }}"},
		Notary:               &runnerv1.Notary{Profile: "n", KeyId: "K1", Key: "${{ secrets.ASC_KEY }}", Issuer: "i"},
	}
	if !proto.Equal(got, want) {
		t.Fatalf("signing = %v，期望 %v", got, want)
	}
}

// capacity 返回上报的容量（机器规格，不预留，max_jobs 4）。
func capacity(cpu float64, mem int64) *runnerv1.Capacity {
	return &runnerv1.Capacity{Machine: &runnerv1.Resources{Cpu: cpu, MemoryBytes: mem}, Reserved: &runnerv1.Resources{}, MaxJobs: 4}
}

// 上报了容量的 Runner，每次分配都检查 job 声明的资源，空闲时也不超过容量；没有机器满足时保持等待，
// 原因中写明需要多少、各兼容 Runner 的容量和空闲。
func TestService_Acquire_capacityStrict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	r.state.Capacity = capacity(2, 2062835712)
	big := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 2, 4<<30 })
	r.mustAcquire(2, 0)
	h.wantSkip(big, "没有 Runner 的容量满足 job 声明的 resources（需要 2 CPU / 4.0 GiB）："+r.name+" 容量 2 CPU / 1.9 GiB、空闲 2 CPU / 1.9 GiB")
	small := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	if jobs := r.mustAcquire(2, 1); jobs[0].JobId != small {
		t.Fatalf("应领取 1C1G 的 job %d，实际 %d", small, jobs[0].JobId)
	}
	// 账本：已经在执行 1C1G，再来一个 1C1G 放不下内存（剩约 0.9 GiB），等待资源释放
	another := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	r.state.RunningJobs = 1
	r.mustAcquire(2, 0)
	h.wantSkip(another, "等待资源释放（需要 1 CPU / 1.0 GiB）")

	var raw []byte
	if err := h.db.QueryRow(t.Context(), `SELECT capacity FROM ci_runners WHERE runner_id=$1`, r.id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c store.RunnerCapacity
	if err := json.Unmarshal(raw, &c); err != nil || c.Machine.CPU != 2 || c.Machine.Memory != 2062835712 || c.MaxJobs != 4 {
		t.Fatalf("ci_runners.capacity = %s（%v）", raw, err)
	}
}

// 同一批领取的声明资源之和不超过容量（扣到 0 后不会被当作"没有上报"）。
func TestService_Acquire_capacityBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	r.state.Capacity = capacity(4, 8<<30)
	run := h.addRun(h.addRepo())
	for range 3 {
		h.addJob(run, func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 2, 4<<30 })
	}
	r.mustAcquire(4, 2)
	r.state.RunningJobs = 2
	r.mustAcquire(4, 0)
}

// 择优（空闲容量最多者优先）：小机器先来领取时，把 job 让给在线、能放下它且剩余更多的大机器；
// 等待超过 scheduler.SchedDelay 后小机器也可以领取。大机器放不下的 job 不让。
func TestService_Acquire_prefersMostFree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	small := h.enrollRunner()
	small.state.Capacity = capacity(2, 2<<30)
	large := h.enrollRunner()
	large.state.Capacity = capacity(4, 8<<30)
	large.mustAcquire(0, 0) // 上报容量、在线

	id := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	small.mustAcquire(1, 0)
	h.wantSkip(id, "等待更合适的 Runner "+large.name+" 领取")
	if jobs := large.mustAcquire(1, 1); jobs[0].JobId != id {
		t.Fatalf("大机器应领取 job %d", id)
	}

	// 等待超过 scheduler.SchedDelay：小机器直接领取
	late := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	h.exec(`UPDATE jobs SET queued_at = now() - interval '1 minute' WHERE id=$1`, late)
	if jobs := small.mustAcquire(1, 1); jobs[0].JobId != late {
		t.Fatalf("等待超过 %v 后小机器应领取 job %d", scheduler.SchedDelay, late)
	}

	// 大机器离线：不再让给它
	h.exec(`UPDATE runners SET last_seen_at = now() - interval '10 minutes' WHERE id=$1`, large.id)
	again := h.pendingJob(func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	small.state.RunningJobs = 0
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, late)
	if jobs := small.mustAcquire(1, 1); jobs[0].JobId != again {
		t.Fatalf("大机器离线时小机器应领取 job %d", again)
	}
}

// 有待执行的缓存清理请求时 Acquire 只下发请求、不领取新任务；Runner 回报结果后请求完成，不再下发。
// 上报的缓存清单保存在 ci_runners.cache_report，没有上报时保留上一次的。
func TestService_Acquire_cachePurges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	h.pendingJob()
	var pid int64
	if err := h.db.QueryRow(t.Context(), `INSERT INTO runner_cache_purges (runner_id, repo_id, kinds, domain, actor)
		VALUES ($1, 7, '{toolcache}', 'oci-root', 'admin') RETURNING id`, r.id).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	resp, err := r.acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	ps := resp.Directives.GetCachePurges()
	if len(resp.Jobs) != 0 || len(ps) != 1 || ps[0].Id != pid || ps[0].RepoId != 7 || ps[0].Domain != "oci-root" || len(ps[0].Kinds) != 1 {
		t.Fatalf("应只下发清理请求：jobs %d，purges %v", len(resp.Jobs), ps)
	}
	r.state.PurgeResults = []*runnerv1.CachePurgeResult{{Id: pid, Removed: 2, RemovedBytes: 1 << 20}}
	r.state.CacheReport = &runnerv1.CacheReport{GeneratedAt: 100, Entries: []*runnerv1.CacheEntry{{Kind: "toolcache", Name: "gomod", SizeBytes: 5}}}
	resp, err = r.acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Directives.GetCachePurges()) != 0 || len(resp.Jobs) != 1 {
		t.Fatalf("回报结果后不再下发，并恢复领取：%v", resp)
	}
	var state string
	var result []byte
	if err := h.db.QueryRow(t.Context(), `SELECT state, result FROM runner_cache_purges WHERE id=$1`, pid).Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	if state != "done" || !strings.Contains(string(result), `"removedBytes": "1048576"`) {
		t.Fatalf("清理请求 = %s，%s", state, result)
	}
	r.state.PurgeResults, r.state.CacheReport = nil, nil
	r.mustAcquire(0, 0)
	var report []byte
	if err := h.db.QueryRow(t.Context(), `SELECT cache_report FROM ci_runners WHERE runner_id=$1`, r.id).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), `"gomod"`) {
		t.Fatalf("应保留上一次的缓存清单：%s", report)
	}
}

// 不同平台的两台 Runner 同时领取同一个仓库的 job 时，仓库并发上限和矩阵 max-parallel 都不能被突破。
// 先用一个事务占住仓库配额的锁，让两次领取都在检查配额前等待，再一起放行。
func TestService_Acquire_concurrentQuota(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		repo  func(*repoOpts)
		job   func(platform string) func(*jobRow)
		limit string
	}{
		{
			name:  "仓库并发上限",
			repo:  func(o *repoOpts) { o.maxConcurrency = 1 },
			job:   func(p string) func(*jobRow) { return onPlatform(workflow.ExecutorOCI, p) },
			limit: "仓库并发上限 1",
		},
		{
			name: "矩阵 max-parallel",
			repo: func(*repoOpts) {},
			job: func(p string) func(*jobRow) {
				return func(j *jobRow) {
					j.spec.Executor, j.spec.Platform = workflow.ExecutorOCI, p
					j.name, j.matrix, j.spec.MaxParallel = "build", map[string]any{"arch": p}, 1
				}
			},
			limit: "矩阵 max-parallel = 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			arm := h.enrollRunner(withPlatforms([]string{"oci"}, "linux/arm64"))
			amd := h.enrollRunner(withPlatforms([]string{"oci"}, "linux/amd64"))
			repo := h.addRepo(tc.repo)
			run := h.addRun(repo)
			h.addJob(run, tc.job("linux/arm64"))
			h.addJob(run, tc.job("linux/amd64"))

			locked, release := make(chan struct{}), make(chan struct{})
			holder := make(chan error, 1)
			go func() {
				holder <- h.db.Tx(t.Context(), func(tx pgx.Tx) error {
					if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, scheduler.RepoQuotaLockKey(repo)); err != nil {
						return err
					}
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked
			// 测试失败退出时也要放行，否则占着连接的事务一直不结束
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			defer unlock()
			got := make(chan int, 2)
			for _, r := range []*testRunner{arm, amd} {
				go func() {
					resp, err := r.acquire(1)
					if err != nil {
						t.Errorf("Acquire：%v", err)
						got <- 0
						return
					}
					got <- len(resp.Jobs)
				}()
			}
			// 两次领取都在等配额的锁
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting int
				if err := h.db.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
					WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("两次领取没有都等待仓库配额的锁（等待中 %d 个）", waiting)
				}
				time.Sleep(10 * time.Millisecond)
			}
			unlock()
			if err := <-holder; err != nil {
				t.Fatal(err)
			}
			if n := <-got + <-got; n != 1 {
				t.Fatalf("两台 Runner 共领到 %d 个 job，期望只有 1 个（%s）", n, tc.limit)
			}
			var running int
			if err := h.db.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE run_id=$1 AND state='running'`, run).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if running != 1 {
				t.Fatalf("running 的 job 有 %d 个，期望 1 个", running)
			}
		})
	}
}

// 长轮询中的 Runner 被暂停接单或吊销后，再到达的 job 不会分配给它：
// 暂停时立即返回带 drain 指令的空响应，吊销时返回 RUNNER_REVOKED。期间升级请求变了时立即返回最新的升级指令（PLAN #27.13）。
func TestService_Acquire_stateChangedDuringLongPoll(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		version string // Runner 上报的版本（空为默认）
		setup   func(h *harness, r *testRunner)
		update  string
		check   func(t *testing.T, resp *connect.Response[runnerv1.AcquireResponse], err error)
	}{
		{
			name: "暂停接单", update: `UPDATE runners SET drain=true WHERE id=$1`,
			check: func(t *testing.T, resp *connect.Response[runnerv1.AcquireResponse], err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("Acquire：%v", err)
				}
				if len(resp.Msg.Jobs) != 0 || !resp.Msg.Directives.GetDrain() {
					t.Fatalf("暂停后不应领取且应下发 drain 指令：%d 个 job，directives %+v", len(resp.Msg.Jobs), resp.Msg.Directives)
				}
			},
		},
		{
			name: "吊销", update: `UPDATE runners SET state='revoked' WHERE id=$1`,
			check: func(t *testing.T, _ *connect.Response[runnerv1.AcquireResponse], err error) {
				t.Helper()
				if !rpcerr.IsRunnerRevoked(err) {
					t.Fatalf("吊销后应返回 RUNNER_REVOKED：%v", err)
				}
			},
		},
		{
			// 长轮询开始时有升级指令（0.2.0），期间管理员取消了请求：立即返回、不带旧指令
			name: "取消升级请求", version: "v0.1.0",
			setup: func(h *harness, r *testRunner) {
				h.exec(`INSERT INTO runner_upgrade_requests (runner_id, version, actor) VALUES ($1, '0.2.0', 'admin')`, r.id)
				resp, err := h.client.Acquire(waitCtx(h.t, 30*time.Second), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 0}))
				if err != nil || resp.Msg.Directives.GetUpgrade().GetVersion() != "0.2.0" || resp.Msg.Directives.GetUpgrade().GetGeneration() < 1000 {
					h.t.Fatalf("取消之前应下发升级指令（generation 是请求的 ID）：%v %+v", err, resp)
				}
			},
			update: `UPDATE runner_upgrade_requests SET state='cancelled' WHERE runner_id=$1`,
			check: func(t *testing.T, resp *connect.Response[runnerv1.AcquireResponse], err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("Acquire：%v", err)
				}
				if len(resp.Msg.Jobs) != 0 || resp.Msg.Directives.GetUpgrade() != nil {
					t.Fatalf("取消后应立即返回、不带旧的升级指令：%d 个 job，directives %+v", len(resp.Msg.Jobs), resp.Msg.Directives)
				}
			},
		},
		{
			// 长轮询期间管理员请求升级、点了检查更新：立即返回新的指令和检查序号
			name: "请求升级和检查", version: "v0.1.0",
			update: `WITH x AS (INSERT INTO runner_upgrade_requests (runner_id, version, actor) VALUES ($1, '0.2.0', 'admin'))
				UPDATE runners SET upgrade_check=upgrade_check+1 WHERE id=$1`,
			check: func(t *testing.T, resp *connect.Response[runnerv1.AcquireResponse], err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("Acquire：%v", err)
				}
				d := resp.Msg.Directives
				if len(resp.Msg.Jobs) != 0 || d.GetUpgrade().GetVersion() != "0.2.0" || d.GetUpgradeCheck() != 1 {
					t.Fatalf("应立即返回新的升级指令和检查序号：%d 个 job，directives %+v", len(resp.Msg.Jobs), d)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(o *harnessOpts) { o.longPoll = time.Minute })
			r := h.enrollRunner()
			if tc.version != "" {
				r.state.Version = tc.version
			}
			if tc.setup != nil {
				tc.setup(h, r)
			}
			// 只有受仓库并发上限限制的 job：第一轮查询领不到（记下跳过原因），进入等待
			full := h.addRun(h.addRepo(func(o *repoOpts) { o.maxConcurrency = 1 }))
			h.addJob(full, func(j *jobRow) { j.state = store.JobRunning })
			blocked := h.addJob(full)
			type result struct {
				resp *connect.Response[runnerv1.AcquireResponse]
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := h.client.Acquire(waitCtx(t, 30*time.Second), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 1, RequestId: "poll"}))
				done <- result{resp, err}
			}()
			// 等第一轮查询结束（记下了跳过原因）
			deadline := time.Now().Add(10 * time.Second)
			for {
				var n int
				if err := h.db.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE id=$1 AND last_skip_reason IS NOT NULL`, blocked).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Acquire 没有完成第一轮查询")
				}
				time.Sleep(10 * time.Millisecond)
			}
			h.exec(tc.update, r.id)
			id := h.pendingJob()
			h.hub.All()
			res := <-done
			tc.check(t, res.resp, res.err)
			if s := h.job(id); s.State != store.JobPending {
				t.Fatalf("job 不应被领取：%s", s.State)
			}
		})
	}
}

// cloneJob 复制 job id 为 n 个新 job（同一个 run，排在它之后），用于构造很长的队列。
func (h *harness) cloneJob(id int64, n int) {
	h.t.Helper()
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state, priority, queued_at)
		SELECT kind, run_id, job_key || '-' || g, job_name, display_name, matrix, spec, platform, executor_kind, state, priority, queued_at
		FROM jobs, generate_series(1, $2) g WHERE id=$1`, id, n)
}

// 同一个 request_id 的两个长轮询重叠（例如 Runner 网络错误后重试，旧的长轮询还没结束）时，
// 两个响应返回同一批 attempt，不会分配两批。
func TestService_Acquire_concurrentSameRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOpts) { o.longPoll = time.Minute })
	r := h.enrollRunner()
	r.state.Capacity = capacity(8, 8<<30)
	r.state.Capacity.MaxJobs = 1
	run := h.addRun(h.addRepo())
	type result struct {
		jobs []*runnerv1.JobSpec
		err  error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			resp, err := h.client.Acquire(waitCtx(t, 30*time.Second), withAuth(r.token,
				&runnerv1.AcquireRequest{State: r.state, MaxJobs: 1, RequestId: "same-in-flight-request"}))
			if err != nil {
				done <- result{err: err}
				return
			}
			done <- result{jobs: resp.Msg.Jobs}
		}()
	}
	time.Sleep(200 * time.Millisecond) // 两个请求都进入长轮询
	h.addJob(run)
	h.addJob(run)
	h.hub.All()
	var attempts []int64
	for range 2 {
		res := <-done
		if res.err != nil {
			t.Fatalf("Acquire：%v", res.err)
		}
		if len(res.jobs) != 1 {
			t.Fatalf("应领到 1 个 job，实际 %d 个", len(res.jobs))
		}
		attempts = append(attempts, res.jobs[0].Ref.AttemptId)
	}
	if attempts[0] != attempts[1] {
		t.Fatalf("同一个 request_id 返回了不同的 attempt：%v", attempts)
	}
	var n int
	if err := h.db.QueryRow(t.Context(), `SELECT count(*) FROM attempts WHERE runner_id=$1`, r.id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("应只创建 1 个 attempt，实际 %d（%v）", n, err)
	}
}

// 槽位按 Server 的账本检查：Runner 上报的 max_jobs 已经用满时，换一个 request_id、请求的数量再多也不领取。
func TestService_Acquire_serverSlots(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	r.state.Capacity = capacity(8, 8<<30)
	r.state.Capacity.MaxJobs = 1
	run := h.addRun(h.addRepo())
	h.addJob(run)
	h.addJob(run)
	r.mustAcquire(4, 1)
	// Runner 上报自己空闲（例如领取的响应丢了），Server 的账本中仍有 1 个在执行
	r.state.RunningJobs = 0
	r.mustAcquire(4, 0)

	// max_jobs 为 2：一次最多再领 1 个
	r.state.Capacity.MaxJobs = 2
	h.addJob(run)
	r.mustAcquire(4, 1)
}

// 候选窗口（一页）全被这台 Runner 放不下的 job 占满时，继续考察后面的页，领到能执行的 job。
func TestService_Acquire_candidateWindowResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	r.state.Capacity = capacity(2, 2<<30)
	run := h.addRun(h.addRepo())
	big := h.addJob(run, func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 8, 8<<30 })
	h.cloneJob(big, 2*scheduler.CandidateLimit+5)
	small := h.addJob(run, func(j *jobRow) { j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	jobs := r.mustAcquire(1, 1)
	if jobs[0].JobId != small {
		t.Fatalf("应领取能执行的 job %d，实际 %d", small, jobs[0].JobId)
	}
	h.wantSkip(big, "没有 Runner 的容量满足 job 声明的 resources")
	// 正常顺序不变：能执行的 job 排在前面时直接领取
	first := h.addJob(h.addRun(h.addRepo()), func(j *jobRow) { j.priority = 5; j.spec.CPU, j.spec.MemoryBytes = 1, 1<<30 })
	r.state.RunningJobs = 1
	if jobs := r.mustAcquire(1, 1); jobs[0].JobId != first {
		t.Fatalf("应领取优先级最高的 job %d，实际 %d", first, jobs[0].JobId)
	}
}

// 候选窗口全被并发上限已满的仓库的 job 占满时，其他仓库的 job 照常领取；上限释放后再领取这个仓库的。
func TestService_Acquire_candidateWindowRepoQuota(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	busy := h.addRun(h.addRepo(func(o *repoOpts) { o.maxConcurrency = 1 }))
	running := h.addJob(busy, func(j *jobRow) { j.priority = 2 })
	r.mustAcquire(1, 1)
	queued := h.addJob(busy)
	h.cloneJob(queued, scheduler.CandidateLimit+5)
	other := h.pendingJob()
	if jobs := r.mustAcquire(1, 1); jobs[0].JobId != other {
		t.Fatalf("应领取其他仓库的 job %d，实际 %d", other, jobs[0].JobId)
	}
	h.wantSkip(queued, "仓库并发上限 1")
	// 上限释放（运行中的结束）：按顺序领取这个仓库的下一个
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, running)
	if jobs := r.mustAcquire(1, 1); jobs[0].JobId != queued {
		t.Fatalf("上限释放后应领取 %d，实际 %d", queued, jobs[0].JobId)
	}
}

// TestService_Acquire_upgradeRegistry（PLAN #27.13）：每次 Acquire 都下发 registry 设置（没有设置时为空，Runner 用官方
// registry）；暂停接单的 Runner 也照常收到升级指令。
func TestService_Acquire_upgradeRegistry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	acquire := func() *runnerv1.RunnerDirectives {
		t.Helper()
		resp, err := h.client.Acquire(t.Context(), withAuth(r.token, &runnerv1.AcquireRequest{State: r.state, MaxJobs: 1}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.Directives
	}
	if d := acquire(); d.GetUpgradeRegistry() != "" || d.GetUpgrade() != nil {
		t.Fatalf("没有设置时 registry 应为空：%+v", d)
	}
	h.exec(`INSERT INTO kv (key, value) VALUES ('runner_upgrade.registry', 'https://npm.example/')`)
	h.exec(`UPDATE runners SET drain=true WHERE id=$1`, r.id)
	h.exec(`INSERT INTO runner_upgrade_requests (runner_id, version, actor) VALUES ($1, '0.2.0', 'admin')`, r.id)
	if d := acquire(); d.GetUpgradeRegistry() != "https://npm.example/" || d.GetUpgrade().GetVersion() != "0.2.0" || !d.GetDrain() {
		t.Fatalf("应下发设置的 registry，暂停接单时也下发升级指令：%+v", d)
	}
}

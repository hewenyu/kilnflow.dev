package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestReserveManaged：托管预留后 job 是 running（占用配额），但 started_at 为空、租约是预留期限；attempt 属于托管池、
// phase 为 reserved、没有 Runner；Check Run 同步记录已写入；Reserved 列出它。
func TestReserveManaged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 2))
	id := h.pendingJob()
	res := h.reserveManaged(testPoolKey, 1, nil)
	if !slices.Equal(claimedIDs(res), []int64{id}) || res.Claims[0].AttemptID == 0 || res.Claims[0].Epoch != 1 {
		t.Fatalf("预留结果 = %+v", res)
	}
	s := h.job(id)
	if s.State != store.JobRunning || s.StartedAt != nil || s.Lease == nil || s.Pool != testPoolKey || s.Phase != string(cistore.PhaseReserved) || s.Runner != nil {
		t.Fatalf("预留后 job = %+v", s)
	}
	if !strings.Contains(s.Reason, "已预留到托管池 "+testPoolKey) {
		t.Fatalf("原因 = %q", s.Reason)
	}
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE dedupe_key=$1`, fmt.Sprintf("checkrun:job:%d", id)); n != 1 {
		t.Fatal("预留应写入 Check Run 同步记录")
	}
	list, err := Reserved(h.ctx, h.db, testPoolKey)
	if err != nil || len(list) != 1 || list[0].JobID != id || list[0].AttemptID != res.Claims[0].AttemptID {
		t.Fatalf("Reserved = %+v, %v", list, err)
	}
	if res := h.reserveManaged(testPoolKey, 1, nil); len(res.Claims) != 0 {
		t.Fatalf("没有 pending 的 job 时不应预留：%+v", res)
	}
}

// TestNativePreferredOverManaged：自有机器优先（规则 1）。刚排队的 job 在空闲的 Native Runner 能接时，托管派发先让 10 秒；
// 排队超过 SchedDelay 后不再让。Native 领取不会因为托管池而让。
func TestNativePreferredOverManaged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 4, func(p *ManagedPool) { p.MaxConcurrent = 100 }))
	native := h.addNative()
	id := h.pendingJob(fresh)
	res := h.reserveManaged(testPoolKey, 1, nil)
	if len(res.Claims) != 0 || res.RetryAt.IsZero() {
		t.Fatalf("刚排队的 job 应先让给空闲的 Native Runner：%+v", res)
	}
	if s := h.job(id); s.State != store.JobPending || !strings.Contains(s.SkipReason, "等待更合适的 Runner "+native.Runner.Name) {
		t.Fatalf("跳过原因 = %q", s.SkipReason)
	}
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{id}) {
		t.Fatalf("Native 应领到 job %d：%v", id, got)
	}
	// Native 领取时有托管池也不让（托管池永远不比自有机器优先）
	id2 := h.pendingJob(fresh)
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{id2}) {
		t.Fatalf("Native 应领到 job %d：%v", id2, got)
	}
	// 排队超过 SchedDelay：托管派发直接预留
	old := h.pendingJob()
	if got := claimedIDs(h.reserveManaged(testPoolKey, 1, nil)); !slices.Equal(got, []int64{old}) {
		t.Fatalf("排队超过 SchedDelay 后托管应预留 job %d：%v", old, got)
	}
}

// TestPoolPinnedJobs：pool 指向托管池的 job 只在托管上执行（Native 的预过滤排除它）；托管池接不了的 job（host、能力不足）
// Native 照常领取。
func TestPoolPinnedJobs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 4))
	native := h.addNative()
	pinned := h.pendingJob(inPool(testPoolName))
	if got := h.acquireNative(native, 4); len(got.Claims) != 0 {
		t.Fatalf("Native 不应领到只在托管上执行的 job：%v", claimedIDs(got))
	}
	buildkit := h.pendingJob(func(j *jobRow) { j.spec.BuildkitImage = "moby/buildkit" })
	if got := claimedIDs(h.reserveManaged(testPoolKey, 4, nil)); !slices.Equal(got, []int64{pinned}) {
		t.Fatalf("托管应只预留 job %d：%v", pinned, got)
	}
	native.Report.Executors = append(native.Report.Executors, workflow.CapBuildkit)
	if got := claimedIDs(h.acquireNative(native, 4)); !slices.Equal(got, []int64{buildkit}) {
		t.Fatalf("Native 应领到 buildkit 的 job：%v", got)
	}
}

// TestConcurrentNativeAndManaged：2 台 Native Runner 与同一个托管池的 2 个派发者同时竞争一个长队列（多页候选），多轮：
// 每个 job 至多一个 attempt、托管池的并发不超过上限、Native 不超过 max_jobs，没有死锁重试（I1、I2）。
func TestConcurrentNativeAndManaged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const poolMax = 2
	h.pools.set(pool(testPoolKey, poolMax))
	natives := []*NativeTrigger{h.addNative(), h.addNative()}
	run := h.addRun(h.addRepo(0))
	first := h.addJob(run)
	// 4 页候选：每个事务锁住一页，四个触发方各自拿到不同的页，才真正同时竞争各自的槽位。提交时间打乱（与 id、queued_at 的
	// 顺序不同，且有大量相同的），竞争发生在按排队顺序（cistore.QueueOrderSQL）翻页的候选上
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at, created_at)
		SELECT kind, run_id, job_key || '-' || g, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at + make_interval(secs => g),
		created_at - make_interval(secs => (g * 37) % 11)
		FROM jobs, generate_series(1, $2) g WHERE id=$1`, first, 4*CandidateLimit)
	const rounds = 8
	for round := range rounds {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 4)
		trigger := func(t Trigger) {
			defer wg.Done()
			<-start
			if _, err := Reserve(h.ctx, h.r, t, nil, noPrepare); err != nil {
				errs <- err
			}
		}
		for _, n := range natives {
			tr := *n
			tr.RequestID = fmt.Sprintf("req-%d-%d", round, n.Runner.ID)
			wg.Add(1)
			go trigger(Trigger{Native: &tr})
		}
		for range 2 {
			wg.Add(1)
			go trigger(Trigger{Managed: &ManagedTrigger{Pool: testPoolKey, Max: poolMax}})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("第 %d 轮：%v", round, err)
		}
		if n := h.queryInt(`SELECT count(*) FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id WHERE j.state='running' AND a.managed_pool=$1`, testPoolKey); n > poolMax {
			t.Fatalf("第 %d 轮：托管池同时有 %d 个预留，超过上限 %d", round, n, poolMax)
		}
		for _, n := range natives {
			if got := h.queryInt(`SELECT count(*) FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id WHERE j.state='running' AND a.runner_id=$1`, n.Runner.ID); got > int64(n.MaxJobs) {
				t.Fatalf("第 %d 轮：Runner %s 同时执行 %d 个，超过 max_jobs %d", round, n.Runner.Name, got, n.MaxJobs)
			}
		}
		// 结束这一轮选中的 job，下一轮重新竞争
		h.exec(`UPDATE jobs SET state='succeeded', finished_at=now(), lease_expires_at=NULL WHERE run_id=$1 AND state='running'`, run)
	}
	if n := h.queryInt(`SELECT count(*) FROM (SELECT job_id FROM attempts GROUP BY job_id HAVING count(*) > 1) x`); n != 0 {
		t.Fatalf("%d 个 job 有多个 attempt", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM attempts WHERE managed_pool IS NOT NULL`); n == 0 {
		t.Fatal("托管池在竞争中一个也没有预留到")
	}
	if n := h.queryInt(`SELECT count(*) FROM attempts WHERE runner_id IS NOT NULL`); n == 0 {
		t.Fatal("Native Runner 在竞争中一个也没有领到")
	}
	if r := h.db.Retries(); r != 0 {
		t.Fatalf("事务因死锁重试了 %d 次", r)
	}
}

// TestManagedPagesLongQueue：前面一整页都是托管池放不下的 job（资源超过一个执行槽，预过滤不排除）时，托管派发同样翻页，
// 后面的 job 能被预留（不饿死）。
func TestManagedPagesLongQueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 2))
	run := h.addRun(h.addRepo(0))
	big := h.addJob(run, func(j *jobRow) { j.spec.CPU = 64; j.priority = 5 })
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at)
		SELECT kind, run_id, job_key || '-' || g, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at
		FROM jobs, generate_series(1, $2) g WHERE id=$1`, big, CandidateLimit+5)
	small := h.addJob(run)
	var cur *Cursor
	for pages := 0; ; pages++ {
		res := h.reserveManaged(testPoolKey, 1, cur)
		if len(res.Claims) > 0 {
			if got := claimedIDs(res); !slices.Equal(got, []int64{small}) || pages == 0 {
				t.Fatalf("第 %d 页预留了 %v，期望翻页后预留 job %d", pages, got, small)
			}
			break
		}
		if cur = res.Next; cur == nil {
			t.Fatalf("考察了 %d 页仍没有预留到后面的 job %d", pages+1, small)
		}
	}
	if s := h.job(big); !strings.Contains(s.SkipReason, "等待资源释放") && !strings.Contains(s.SkipReason, "容量") {
		t.Fatalf("放不下的 job 的原因 = %q", s.SkipReason)
	}
}

// TestReservationHoldsQuota：托管预留后 job 是 running，仓库 max_concurrency、矩阵 max-parallel 已被占用，Native 领不到同仓库、
// 同矩阵的 job；action key 相同的 job 跟随预留的 job（I3）。
func TestReservationHoldsQuota(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 4))
	native := h.addNative()

	repo := h.addRepo(1)
	run := h.addRun(repo)
	first := h.addJob(run, func(j *jobRow) { j.actionKey = "key-1"; j.spec.Cache = true; j.priority = 9 })
	second := h.addJob(h.addRun(repo))
	if got := claimedIDs(h.reserveManaged(testPoolKey, 1, nil)); !slices.Equal(got, []int64{first}) {
		t.Fatalf("托管应预留 job %d：%v", first, got)
	}
	if got := h.acquireNative(native, 4); len(got.Claims) != 0 {
		t.Fatalf("仓库并发已被预留占用，Native 不应领到：%v", claimedIDs(got))
	}
	if s := h.job(second); !strings.Contains(s.SkipReason, "仓库并发上限 1") {
		t.Fatalf("跳过原因 = %q", s.SkipReason)
	}

	// 同 key 合并：waiting 的 job 与预留的 job action key 相同，就绪判定时跟随它
	follower := h.addJob(run, func(j *jobRow) { j.actionKey = "key-1"; j.spec.Cache = true; j.state = store.JobWaiting })
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error { return h.eng.AdvanceRun(h.ctx, tx, run) }); err != nil {
		t.Fatal(err)
	}
	if s := h.job(follower); s.State != store.JobWaiting || s.Follows == nil || *s.Follows != first {
		t.Fatalf("同 key 的 job 应跟随预留的 job %d：%+v", first, s)
	}

	// 矩阵 max-parallel = 1
	mrun := h.addRun(h.addRepo(8))
	matrix := func(v string) func(*jobRow) {
		return func(j *jobRow) { j.name, j.matrix, j.spec.MaxParallel = "m", map[string]any{"v": v}, 1 }
	}
	a := h.addJob(mrun, matrix("a"), func(j *jobRow) { j.priority = 9 })
	b := h.addJob(mrun, matrix("b"))
	if got := claimedIDs(h.reserveManaged(testPoolKey, 1, nil)); !slices.Equal(got, []int64{a}) {
		t.Fatalf("托管应预留 job %d：%v", a, got)
	}
	if got := h.acquireNative(native, 4); len(got.Claims) != 0 {
		t.Fatalf("矩阵 max-parallel 已被预留占用，Native 不应领到：%v", claimedIDs(got))
	}
	if s := h.job(b); !strings.Contains(s.SkipReason, "矩阵 max-parallel = 1") {
		t.Fatalf("跳过原因 = %q", s.SkipReason)
	}
}

// TestRestartKeepsReservation：Server 重启（新的 Reserver、Engine）后预留仍计入池的并发，Reserved 返回它，不会为同一个 job
// 再预留；启动时顺延预留的期限。
func TestRestartKeepsReservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 1))
	id := h.pendingJob()
	if got := claimedIDs(h.reserveManaged(testPoolKey, 1, nil)); !slices.Equal(got, []int64{id}) {
		t.Fatalf("预留 = %v", got)
	}
	h.exec(`UPDATE jobs SET lease_expires_at=now()+interval '1 second' WHERE id=$1`, id)
	h.restart()
	if err := h.eng.ExtendLeasesOnStartup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.job(id); s.Lease == nil || time.Until(*s.Lease) < testLeaseTTL {
		t.Fatalf("重启后预留的期限应顺延：%v", s.Lease)
	}
	other := h.pendingJob()
	if res := h.reserveManaged(testPoolKey, 1, nil); len(res.Claims) != 0 {
		t.Fatalf("池的唯一槽位已被预留占用，不应再预留：%v", claimedIDs(res))
	}
	list, err := Reserved(h.ctx, h.db, testPoolKey)
	if err != nil || len(list) != 1 || list[0].JobID != id {
		t.Fatalf("重启后 Reserved = %+v, %v", list, err)
	}
	if s := h.job(id); s.Attempts != 1 || s.State != store.JobRunning {
		t.Fatalf("预留的 job = %+v", s)
	}
	if s := h.job(other); s.State != store.JobPending {
		t.Fatalf("另一个 job = %+v", s)
	}
}

// TestUnavailablePools：停用的池、不可用的池（例如预算用完）、不存在的池不考察候选，返回可读的原因；其他池和 Native 照常。
// 池在预留之后被停用时，派发者撤销它的预留，job 回到原来的排队位置。
func TestUnavailablePools(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(
		pool("disabled", 4, func(p *ManagedPool) { p.State = PoolDisabled }),
		pool("budget", 4, func(p *ManagedPool) { p.Unavailable = "本月预算已用完" }),
		pool("hosty", 4, func(p *ManagedPool) { p.Executors = []string{"oci", "host"} }),
		pool("ok", 4),
	)
	native := h.addNative()
	a, b := h.pendingJob(), h.pendingJob()
	for key, want := range map[string]string{"disabled": "已停用", "budget": "本月预算已用完", "hosty": "host", "missing": "不存在"} {
		res := h.reserveManaged(key, 4, nil)
		if len(res.Claims) != 0 || !strings.Contains(res.Unavailable, want) {
			t.Fatalf("池 %s：%+v，期望原因包含 %q", key, res, want)
		}
	}
	if got := claimedIDs(h.reserveManaged("ok", 1, nil)); !slices.Equal(got, []int64{a}) {
		t.Fatalf("可用的池应预留 job %d：%v", a, got)
	}
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{b}) {
		t.Fatalf("Native 应领到 job %d：%v", b, got)
	}

	// 池 ok 被停用：派发者撤销它的预留
	queuedAt := h.job(a).QueuedAt
	h.pools.set(pool("ok", 4, func(p *ManagedPool) { p.State = PoolDisabled }))
	list, err := Reserved(h.ctx, h.db, "ok")
	if err != nil || len(list) != 1 {
		t.Fatalf("Reserved = %+v, %v", list, err)
	}
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		if _, err := cistore.LockRunsOfJob(h.ctx, tx, list[0].JobID); err != nil {
			return err
		}
		j, err := cistore.GetJob(h.ctx, tx, list[0].JobID, true)
		if err != nil {
			return err
		}
		return h.eng.Release(h.ctx, tx, j, "托管池已停用")
	}); err != nil {
		t.Fatal(err)
	}
	if s := h.job(a); s.State != store.JobPending || !s.QueuedAt.Equal(*queuedAt) {
		t.Fatalf("撤销后 job = %+v（原排队时间 %v）", s, queuedAt)
	}
}

// TestPoolSourceError：托管池的来源出错时托管派发返回错误，Native 领取不受影响（托管池只用于等待原因）。
func TestPoolSourceError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.err = errors.New("boom")
	native := h.addNative()
	id := h.pendingJob()
	if _, err := Reserve(h.ctx, h.r, Trigger{Managed: &ManagedTrigger{Pool: testPoolKey, Max: 1}}, nil, noPrepare); err == nil {
		t.Fatal("托管池的来源出错时托管派发应返回错误")
	}
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{id}) {
		t.Fatalf("Native 应照常领到 job %d：%v", id, got)
	}
}

// TestLedgerWithManagedAttempt：存在没有 Runner 的托管 attempt 时 Native 领取不报错，Native 账本不计入它（#32.8）。
func TestLedgerWithManagedAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 4))
	h.pendingJob()
	if got := h.reserveManaged(testPoolKey, 1, nil); len(got.Claims) != 1 {
		t.Fatalf("预留 = %v", claimedIDs(got))
	}
	small := h.addNative(func(o *nativeOpts) {
		o.cap = &store.RunnerCapacity{Machine: store.Resources{CPU: 1, Memory: 2 * gib}, MaxJobs: 1}
	})
	id := h.pendingJob()
	if got := claimedIDs(h.acquireNative(small, 1)); !slices.Equal(got, []int64{id}) {
		t.Fatalf("Native 应领到 job %d（托管的 attempt 不占它的账本）：%v", id, got)
	}
}

// TestAIRunnerNotCandidate：AI 执行节点（没有 ci_runners 行）不会出现在 CI 调度的快照中，也不出现在等待原因中（I8）。
func TestAIRunnerNotCandidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var ai int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runners (name, public_key, kind, last_seen_at) VALUES ('ai-1', '\x00', 'ai', now()) RETURNING id`).Scan(&ai); err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO ai_runners (runner_id, role, platform) VALUES ($1, 'review', 'linux/arm64')`, ai)
	native := h.addNative()
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		natives, err := loadNatives(h.ctx, tx, nil)
		if err != nil {
			return err
		}
		if _, ok := natives[ai]; ok || len(natives) != 1 || natives[native.Runner.ID] == nil {
			t.Errorf("快照 = %v，只应有 CI Runner %d", natives, native.Runner.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	big := h.pendingJob(func(j *jobRow) { j.spec.CPU = 64 })
	h.acquireNative(native, 1)
	if s := h.job(big); s.State != store.JobPending || strings.Contains(s.SkipReason, "ai-1") {
		t.Fatalf("等待原因不应提到 AI 执行节点：%+v", s)
	}
}

// TestPrefilterSupersetOfCore：候选 SQL 的预过滤不会排除核心会接受的 job（I7）：对 Native 和托管的多种资源、各种平台、
// executor、pool、能力的组合逐一比较。
func TestPrefilterSupersetOfCore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	run := h.addRun(h.addRepo(8))
	type combo struct {
		id  int64
		req Requirements
	}
	var jobs []combo
	for _, plat := range []string{"linux/arm64", "linux/amd64", "darwin/arm64"} {
		for _, exec := range []workflow.ExecutorKind{workflow.ExecutorHost, workflow.ExecutorOCI} {
			for _, p := range []string{"", "default", testPoolName} {
				for mask := range 16 {
					id := h.addJob(run, func(j *jobRow) {
						j.spec.Platform, j.spec.Executor, j.spec.Pool = plat, exec, p
						if mask&1 != 0 {
							j.spec.BuildkitImage = "moby/buildkit"
						}
						if mask&2 != 0 {
							j.spec.Signing = &workflow.Signing{}
						}
						if mask&4 != 0 {
							j.spec.Services = []workflow.ServiceSpec{{Name: "pg", Image: "postgres"}}
						}
						j.effects = mask&8 != 0
					})
					j, err := cistore.GetJob(h.ctx, h.db, id, false)
					if err != nil {
						t.Fatal(err)
					}
					jobs = append(jobs, combo{id, RequirementsOf(j)})
				}
			}
		}
	}
	resources := map[string]*accepts{
		"mac":    (&NativeRunner{Name: "mac", Pool: "default", Platforms: []string{"darwin/arm64", "linux/arm64"}, Executors: []string{"host", "oci", workflow.CapSigning}}).accepts(),
		"linux":  (&NativeRunner{Name: "linux", Pool: "default", Platforms: []string{"linux/amd64"}, Executors: []string{"oci", workflow.CapBuildkit}}).accepts(),
		"托管-全能力": (&ManagedPool{Name: "a", Pool: testPoolName, Platforms: []string{"linux/arm64", "linux/amd64"}, Executors: []string{"oci"}, Caps: []string{workflow.CapBuildkit, CapServices, CapEffects}}).accepts(),
		"托管-无能力": (&ManagedPool{Name: "b", Pool: "default", Platforms: []string{"linux/arm64"}, Executors: []string{"oci"}}).accepts(),
	}
	for name, acc := range resources {
		var sqlIDs []int64
		if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
			list, err := cistore.QueryJobs(h.ctx, tx, candidateFilter+` AND run_id=$9`, append(acc.filterArgs(), run)...)
			for _, j := range list {
				sqlIDs = append(sqlIDs, j.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		accepted := 0
		for _, c := range jobs {
			if acc.mismatch(c.req) != "" {
				continue
			}
			accepted++
			if !slices.Contains(sqlIDs, c.id) {
				t.Errorf("%s：核心接受 job %d（%+v），预过滤却排除了它", name, c.id, c.req)
			}
		}
		if accepted == 0 || len(sqlIDs) == len(jobs) {
			t.Errorf("%s：核心接受 %d 个、预过滤留下 %d 个（共 %d 个），组合没有起到检验作用", name, accepted, len(sqlIDs), len(jobs))
		}
	}
}

// TestPoolLockSerializes：同一个托管池的两次预留串行（I2）：先占住池的锁，让两次预留都在读账本之前等待，再一起放行；
// 候选超过一页，两次预留各自锁到不同的候选页，只有池的锁能挡住它们同时用掉唯一的槽位。
func TestPoolLockSerializes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 1))
	run := h.addRun(h.addRepo(0))
	first := h.addJob(run)
	h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at)
		SELECT kind, run_id, job_key || '-' || g, job_name, display_name, spec, platform, executor_kind, state, priority, input_digest, queued_at + make_interval(secs => g)
		FROM jobs, generate_series(1, $2) g WHERE id=$1`, first, CandidateLimit+10)

	locked, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- h.db.Tx(t.Context(), func(tx pgx.Tx) error {
			if err := store.LockXact(t.Context(), tx, poolLockKey(testPoolKey)); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	defer unlock()
	got := make(chan int, 2)
	for range 2 {
		go func() {
			res, err := Reserve(h.ctx, h.r, Trigger{Managed: &ManagedTrigger{Pool: testPoolKey, Max: 1}}, nil, noPrepare)
			if err != nil {
				t.Errorf("预留：%v", err)
			}
			got <- len(res.Claims)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting := h.queryInt(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`)
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("两次预留没有都等待池的锁（等待中 %d 个）", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	unlock()
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if n := <-got + <-got; n != 1 {
		t.Fatalf("两次预留共预留了 %d 个 job，池只有 1 个槽位", n)
	}
}

// TestReenrollKeepsNativeLedger：同名重新注册清空了数据库中记录的容量（runnerapi 的 EnrollHook），而 Runner 的长轮询这次
// 上报了容量：账本必须按上报的容量统计已经在执行的 job，8 CPU 的机器在执行 7 CPU 的 job 时不能再领 2 CPU 的 job。
func TestReenrollKeepsNativeLedger(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	native := h.addNative()
	run := h.addRun(h.addRepo(8))
	busy := h.addJob(run, func(j *jobRow) { j.spec.CPU = 7 })
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{busy}) {
		t.Fatalf("应领到 7 CPU 的 job %d：%v", busy, got)
	}
	h.exec(`UPDATE ci_runners SET capacity=NULL WHERE runner_id=$1`, native.Runner.ID)
	small := h.addJob(run, func(j *jobRow) { j.spec.CPU = 2 })
	if got := claimedIDs(h.acquireNative(native, 1)); len(got) != 0 {
		t.Fatalf("8 CPU 的机器已执行 7 CPU，不应再领到 2 CPU 的 job：%v", got)
	}
	if s := h.job(small); s.State != store.JobPending {
		t.Fatalf("2 CPU 的 job 应保持 pending：%+v", s)
	}
	fits := h.addJob(run, func(j *jobRow) { j.spec.CPU = 1 })
	if got := claimedIDs(h.acquireNative(native, 1)); !slices.Equal(got, []int64{fits}) {
		t.Fatalf("剩余 1 CPU 应领到 1 CPU 的 job %d：%v", fits, got)
	}
}

// failOncePrepare 在第一次调用时返回可重试的数据库错误（序列化失败），让 store.DB.Tx 回滚并重试整个预留事务。
type failOncePrepare struct{ calls atomic.Int32 }

func (f *failOncePrepare) prepare(context.Context, store.Querier, *cistore.Job, int64, int64) (struct{}, error) {
	if f.calls.Add(1) == 1 {
		return struct{}{}, &pgconn.PgError{Code: "40001", Message: "测试注入的序列化失败"}
	}
	return struct{}{}, nil
}

// TestManagedRetryRebuildsLedger：预留事务因可重试的错误回滚后，重试从数据库重新建立托管池的账本，回滚前选中计入的执行槽
// 不会留下：MaxConcurrent=1 的空闲池在重试后照常预留 job。
func TestManagedRetryRebuildsLedger(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pools.set(pool(testPoolKey, 1))
	id := h.pendingJob()
	retries := h.db.Retries()
	fp := &failOncePrepare{}
	res, err := Reserve(h.ctx, h.r, Trigger{Managed: &ManagedTrigger{Pool: testPoolKey, Max: 1}}, nil, fp.prepare)
	if err != nil {
		t.Fatalf("托管预留：%v", err)
	}
	if h.db.Retries() == retries || fp.calls.Load() != 2 {
		t.Fatalf("应重试一次事务：retries %d→%d，prepare 调用 %d 次", retries, h.db.Retries(), fp.calls.Load())
	}
	if !slices.Equal(claimedIDs(res), []int64{id}) {
		t.Fatalf("重试后应预留 job %d：%+v", id, res)
	}
	if s := h.job(id); s.State != store.JobRunning || s.Attempts != 1 || s.Pool != testPoolKey {
		t.Fatalf("重试后 job = %+v", s)
	}
}

// TestReservePrepareFailureCounted：job 本身有问题（*PrepareError）时只把这个 job 置为失败、不回滚整次预留（I10），
// 结果中的 Failed 计入它：Runner 据此立即重新考察队列，而不是等到长轮询到期。
func TestReservePrepareFailureCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bad := h.pendingJob()
	good := h.pendingJob()
	prepare := func(_ context.Context, _ store.Querier, j *cistore.Job, _, _ int64) (struct{}, error) {
		if j.ID == bad {
			return struct{}{}, &PrepareError{Err: errors.New("上游缺少声明的输出")}
		}
		return struct{}{}, nil
	}
	tr := h.addNative()
	res, err := Reserve(h.ctx, h.r, Trigger{Native: tr}, nil, prepare)
	if err != nil {
		t.Fatalf("Native 领取：%v", err)
	}
	if res.Failed != 1 || !slices.Equal(claimedIDs(res), []int64{good}) {
		t.Fatalf("结果 = %+v（Failed=%d），期望失败的只有 %d、领取 %d", res, res.Failed, bad, good)
	}
	if s := h.job(bad); s.State != store.JobFailed || !strings.Contains(s.Reason, "上游缺少声明的输出") || s.Attempts != 0 {
		t.Fatalf("无法准备的 job = %+v", s)
	}
	if s := h.job(good); s.State != store.JobRunning {
		t.Fatalf("其余候选应照常领取：%+v", s)
	}
}

// TestReserveBatchSkipReasons：一次考察的多个候选各自记下自己的跳过原因（批量更新按行写入不同的原因），配额快照同时挡住
// 同一仓库的多个候选——不是只挡第一个（A1）。
func TestReserveBatchSkipReasons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	native := h.addNative()
	// 仓库并发 1：优先级最高的先领，同一仓库的其他候选都被它挡住
	repo := h.addRepo(1)
	run := h.addRun(repo)
	first := h.addJob(run, func(j *jobRow) { j.priority = 9 })
	second := h.addJob(run)
	third := h.addJob(run)
	// 矩阵 max-parallel = 1：另一个仓库里 a 领取后 b 被矩阵挡住
	matrix := func(v string) func(*jobRow) {
		return func(j *jobRow) { j.name, j.matrix, j.spec.MaxParallel = "m", map[string]any{"v": v}, 1 }
	}
	mrun := h.addRun(h.addRepo(0))
	a := h.addJob(mrun, matrix("a"), func(j *jobRow) { j.priority = 9 })
	b := h.addJob(mrun, matrix("b"))

	got := claimedIDs(h.acquireNative(native, 4))
	slices.Sort(got)
	want := []int64{first, a}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("领取 = %v，想要 %v", got, want)
	}
	for _, id := range []int64{second, third} {
		if s := h.job(id); !strings.Contains(s.SkipReason, "仓库并发上限 1") {
			t.Errorf("job %d 的跳过原因 = %q", id, s.SkipReason)
		}
	}
	if s := h.job(b); !strings.Contains(s.SkipReason, "矩阵 max-parallel = 1") {
		t.Errorf("矩阵 job 的跳过原因 = %q", s.SkipReason)
	}
}

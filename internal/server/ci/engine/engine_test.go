package engine

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestAdvanceRun_RootJobs：没有上游的 job 在推进时立即做就绪判定；普通 job 计算 action key 后进入 pending，
// 发布 job 不计算 action key，等待 Server 执行；有上游的 job 保持 waiting。
func TestAdvanceRun_RootJobs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	build := e.addJob(run, jobOpts{name: "build"})
	pub := e.addJob(run, jobOpts{name: "publish", kind: workflow.JobKindPublish})
	test := e.addJob(run, jobOpts{name: "test", needs: []int64{build}})
	e.advance(run)

	for id, reason := range map[int64]string{build: "等待 Runner 领取", pub: "等待 Server 执行发布"} {
		j := e.wantJobState(id, store.JobPending)
		if deref(j.Reason) != reason || j.QueuedAt == nil {
			t.Errorf("job %s：reason=%q queued_at=%v，期望 reason=%q 且已记录排队时间", j.JobKey, deref(j.Reason), j.QueuedAt, reason)
		}
		e.wantOutbox(jobCheckKey(id))
	}
	if e.job(build).ActionKey == nil {
		t.Error("普通 job 就绪时应计算 action key")
	}
	if e.job(pub).ActionKey != nil {
		t.Error("发布 job 不应计算 action key")
	}
	e.wantJobState(test, store.JobWaiting)
	e.wantRunState(run, store.RunActive)
}

// TestAdvanceRun_NotActive：排队中（并发组）或已结束的 run 不推进，job 保持原状。
func TestAdvanceRun_NotActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, state := range []store.RunState{store.RunQueued, store.RunSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			e := e.sub(t)
			run := e.newRun(runOpts{state: state})
			j := e.addJob(run, jobOpts{})
			e.advance(run)
			e.wantJobState(j, store.JobWaiting)
			if got := e.run(run).State; got != state {
				t.Fatalf("run 的状态为 %s，期望保持 %s", got, state)
			}
		})
	}
}

// TestAdvanceRun_NeedsChain：a → b → c 依次成功，下游在上游成功后进入 pending；全部成功后 run 成功，
// run 的汇总 Check Run 写入 outbox。
func TestAdvanceRun_NeedsChain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	a := e.addJob(run, jobOpts{name: "a"})
	b := e.addJob(run, jobOpts{name: "b", needs: []int64{a}})
	c := e.addJob(run, jobOpts{name: "c", needs: []int64{b}})
	e.advance(run)
	e.wantJobState(a, store.JobPending)
	e.wantJobState(b, store.JobWaiting)

	e.succeed(a, "")
	e.wantJobState(a, store.JobSucceeded)
	e.wantJobState(b, store.JobPending)
	e.wantJobState(c, store.JobWaiting)
	e.wantRunState(run, store.RunActive)

	e.succeed(b, "")
	e.wantJobState(c, store.JobPending)
	e.succeed(c, "")
	e.wantRunState(run, store.RunSucceeded)
	e.wantOutbox(runCheckKey(run, 0))
}

// TestAdvanceRun_UpstreamNotSucceeded：上游以 succeeded 以外的终态结束时，下游（包括间接下游）在同一次推进中变为 skipped，
// run 的结论按 PLAN #6.1 计算。
func TestAdvanceRun_UpstreamNotSucceeded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name    string
		res     AttemptResult
		wantUp  store.JobState
		wantRun store.RunState
	}{
		{"失败", AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 1}, store.JobFailed, store.RunFailed},
		{"超时", AttemptResult{Outcome: store.OutcomeTimedOut}, store.JobTimedOut, store.RunFailed},
		{"取消", AttemptResult{Outcome: store.OutcomeCancelled}, store.JobCancelled, store.RunCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			run := e.newRun(runOpts{})
			a := e.addJob(run, jobOpts{name: "a"})
			b := e.addJob(run, jobOpts{name: "b", needs: []int64{a}})
			c := e.addJob(run, jobOpts{name: "c", needs: []int64{b}})
			e.advance(run)
			e.claim(a)
			e.finish(a, tt.res)
			e.wantJobState(a, tt.wantUp)
			for id, reason := range map[int64]string{b: "上游 a 没有成功", c: "上游 b 没有成功"} {
				if j := e.wantJobState(id, store.JobSkipped); deref(j.Reason) != reason || j.FinishedAt == nil {
					t.Errorf("job %s：reason=%q finished_at=%v，期望 %q", j.JobKey, deref(j.Reason), j.FinishedAt, reason)
				}
				e.wantOutbox(jobCheckKey(id))
			}
			e.wantRunState(run, tt.wantRun)
		})
	}
}

// TestAdvanceRun_SkipBeforeAllUpstreamsFinish：多个上游中只要有一个没有成功，下游立即 skipped，不等其他上游结束；
// run 要等全部 job 结束才结束。
func TestAdvanceRun_SkipBeforeAllUpstreamsFinish(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	a := e.addJob(run, jobOpts{name: "a"})
	b := e.addJob(run, jobOpts{name: "b"})
	c := e.addJob(run, jobOpts{name: "c", needs: []int64{a, b}})
	e.advance(run)
	e.claim(a)
	e.claim(b)
	e.finish(a, AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 2})
	e.wantJobState(c, store.JobSkipped)
	e.wantRunState(run, store.RunActive)
	e.finish(b, AttemptResult{Outcome: store.OutcomeSucceeded})
	e.wantRunState(run, store.RunFailed)
}

// TestReadiness_CacheHit：可缓存的 job 成功后写入 action_cache；之后 action key 相同的 job 就绪时直接成功，
// 沿用结果 manifest，cached_from_job_id 指向产出者（succeedWith），不再排队。
func TestReadiness_CacheHit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cached := jobOpts{spec: workflow.Spec{Cache: true}}
	run1 := e.newRun(runOpts{})
	producer := e.addJob(run1, cached)
	e.advance(run1)
	e.succeed(producer, "sha256:m1")
	key := deref(e.job(producer).ActionKey)
	if ent := e.cacheEntry(key); ent == nil || ent.Manifest != "sha256:m1" || ent.Producer != producer {
		t.Fatalf("成功的可缓存 job 应写入 action_cache：%+v", ent)
	}

	run2 := e.newRun(runOpts{})
	hit := e.addJob(run2, cached)
	down := e.addJob(run2, jobOpts{name: "down", needs: []int64{hit}})
	e.advance(run2)
	j := e.wantJobState(hit, store.JobSucceeded)
	wantReason := fmt.Sprintf("命中缓存（来自 run #%d 的 job #%d）", run1, producer)
	if deref(j.ResultManifest) != "sha256:m1" || j.CachedFrom == nil || *j.CachedFrom != producer || deref(j.Reason) != wantReason {
		t.Fatalf("命中缓存：manifest=%q cached_from=%v reason=%q，期望 sha256:m1、%d、%q",
			deref(j.ResultManifest), j.CachedFrom, deref(j.Reason), producer, wantReason)
	}
	if j.FinishedAt == nil || j.AttemptCount != 0 {
		t.Errorf("命中缓存的 job 不执行：finished_at=%v attempt_count=%d", j.FinishedAt, j.AttemptCount)
	}
	if ent := e.cacheEntry(key); !ent.Hit {
		t.Error("命中缓存时应更新 last_hit_at")
	}
	e.wantOutbox(jobCheckKey(hit))
	// 命中缓存的 job 成功后下游照常推进
	e.wantJobState(down, store.JobPending)
}

// TestReadiness_CacheNotUsed：缓存条目可疑、job 不可缓存、有副作用或强制执行时不命中缓存，照常排队。
func TestReadiness_CacheNotUsed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name    string
		opts    jobOpts
		suspect bool
	}{
		{"条目可疑", jobOpts{spec: workflow.Spec{Cache: true}}, true},
		{"cache: false", jobOpts{}, false},
		{"effects", jobOpts{spec: workflow.Spec{Cache: true}, effects: true}, false},
		{"force_execute", jobOpts{spec: workflow.Spec{Cache: true}, force: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			tt.opts.input = tt.name // 每个用例的 action key 互不相同
			run := e.newRun(runOpts{})
			j := e.addJob(run, tt.opts)
			// 先计算出 action key，再放入一个同 key 的缓存条目，然后重新做就绪判定
			e.advance(run)
			key := deref(e.job(j).ActionKey)
			e.exec(`INSERT INTO action_cache (action_key, manifest_digest, producer_job_id, suspect) VALUES ($1, 'sha256:old', 1, $2)`, key, tt.suspect)
			e.exec(`UPDATE jobs SET state='waiting' WHERE id=$1`, j)
			e.advance(run)
			if got := e.wantJobState(j, store.JobPending); got.CachedFrom != nil {
				t.Errorf("不应命中缓存：cached_from=%d", *got.CachedFrom)
			}
		})
	}
}

// TestReadiness_FollowSameKey：已有 action key 相同、正在排队或执行的 job 时，新 job 保持 waiting 并跟随它（同 key 合并）。
// 被跟随的 job 成功时跟随者沿用它的结果，跟随者所在的其他 run 一并推进；被跟随的 job 失败时跟随者不继承失败，重新做就绪判定。
func TestReadiness_FollowSameKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name         string
		res          AttemptResult
		wantFollower store.JobState
		wantRun      store.RunState
	}{
		{"被跟随的 job 成功", AttemptResult{Outcome: store.OutcomeSucceeded, Manifest: "sha256:m"}, store.JobSucceeded, store.RunSucceeded},
		{"被跟随的 job 失败", AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 1}, store.JobPending, store.RunActive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			cached := jobOpts{spec: workflow.Spec{Cache: true}, input: tt.name}
			run1 := e.newRun(runOpts{})
			leader := e.addJob(run1, cached)
			e.advance(run1)
			e.claim(leader)

			run2 := e.newRun(runOpts{})
			follower := e.addJob(run2, cached)
			e.advance(run2)
			f := e.wantJobState(follower, store.JobWaiting)
			if f.Follows == nil || *f.Follows != leader || deref(f.Reason) != fmt.Sprintf("等待 action key 相同的 job #%d", leader) {
				t.Fatalf("应跟随 job %d：follows=%v reason=%q", leader, f.Follows, deref(f.Reason))
			}
			e.wantOutbox(jobCheckKey(follower))

			e.finish(leader, tt.res)
			f = e.wantJobState(follower, tt.wantFollower)
			if f.Follows != nil {
				t.Errorf("被跟随的 job 结束后应清空 follows_job_id：%d", *f.Follows)
			}
			if tt.wantFollower == store.JobSucceeded && (f.CachedFrom == nil || *f.CachedFrom != leader || deref(f.ResultManifest) != "sha256:m") {
				t.Errorf("跟随者应沿用 job %d 的结果：cached_from=%v manifest=%q", leader, f.CachedFrom, deref(f.ResultManifest))
			}
			e.wantRunState(run2, tt.wantRun)
		})
	}
}

// TestReadiness_NoFollowForcedJob：强制执行的 job 不作为同 key 合并的对象，新 job 照常排队。
func TestReadiness_NoFollowForcedJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run1 := e.newRun(runOpts{})
	forced := e.addJob(run1, jobOpts{spec: workflow.Spec{Cache: true}, force: true})
	e.advance(run1)
	e.wantJobState(forced, store.JobPending)
	run2 := e.newRun(runOpts{})
	j := e.addJob(run2, jobOpts{spec: workflow.Spec{Cache: true}})
	e.advance(run2)
	if got := e.wantJobState(j, store.JobPending); got.Follows != nil {
		t.Fatalf("不应跟随强制执行的 job：follows=%d", *got.Follows)
	}
}

// TestReadiness_FollowCachedJob：被跟随的 job 本身沿用了别的 job 的结果时，跟随者的 cached_from_job_id 指向最初的产出者。
func TestReadiness_FollowCachedJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	leader := e.addJob(run, jobOpts{name: "leader"})
	follower := e.addJob(run, jobOpts{name: "follower"})
	// 直接构造：leader 已成功且结果来自 job 42，follower 正在跟随它
	e.exec(`UPDATE jobs SET state='succeeded', result_manifest='sha256:m', cached_from_job_id=42, finished_at=now() WHERE id=$1`, leader)
	e.exec(`UPDATE jobs SET follows_job_id=$2 WHERE id=$1`, follower, leader)
	e.advance(run)
	f := e.wantJobState(follower, store.JobSucceeded)
	if f.CachedFrom == nil || *f.CachedFrom != 42 || deref(f.ResultManifest) != "sha256:m" {
		t.Fatalf("cached_from_job_id = %v manifest=%q，期望最初的产出者 42 和它的结果", f.CachedFrom, deref(f.ResultManifest))
	}
	e.wantRunState(run, store.RunSucceeded)
}

func TestRunConclusion(t *testing.T) {
	failFast := string(store.CancelFailFast)
	byUser := string(store.CancelByUser)
	tests := []struct {
		name string
		jobs []*cistore.Job
		want store.Conclusion
	}{
		{"全部成功", []*cistore.Job{{State: store.JobSucceeded}, {State: store.JobSkipped}}, store.ConclusionSuccess},
		{"有失败", []*cistore.Job{{State: store.JobCancelled, CancelReason: &byUser}, {State: store.JobFailed}}, store.ConclusionFailure},
		{"被取消", []*cistore.Job{{State: store.JobSucceeded}, {State: store.JobCancelled, CancelReason: &byUser}}, store.ConclusionCancelled},
		{"fail-fast 取消不算", []*cistore.Job{{State: store.JobSucceeded}, {State: store.JobCancelled, CancelReason: &failFast}}, store.ConclusionSuccess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runConclusion(tt.jobs); got != tt.want {
				t.Fatalf("runConclusion = %s，期望 %s", got, tt.want)
			}
		})
	}
}

// TestReleaseConcurrency：run 结束后启动并发组中最早排队的 run（PLAN #6.5），它的 job 开始就绪判定；
// 组内还有其他 active 的 run 时不启动；其他组排队的 run 不受影响。
func TestReleaseConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name        string
		otherActive bool
	}{
		{"启动最早排队的 run", false},
		{"组内还有 active 的 run", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			group := "g-" + tt.name
			first := e.newRun(runOpts{concurrency: group})
			a := e.addJob(first, jobOpts{})
			e.advance(first)
			if tt.otherActive {
				e.newRun(runOpts{concurrency: group})
			}
			queued1 := e.newRun(runOpts{state: store.RunQueued, concurrency: group})
			q1 := e.addJob(queued1, jobOpts{})
			queued2 := e.newRun(runOpts{state: store.RunQueued, concurrency: group})
			otherGroup := e.newRun(runOpts{state: store.RunQueued, concurrency: group + "-x"})

			e.succeed(a, "")
			e.wantRunState(first, store.RunSucceeded)
			if tt.otherActive {
				e.wantRunState(queued1, store.RunQueued)
				e.wantJobState(q1, store.JobWaiting)
			} else {
				e.wantRunState(queued1, store.RunActive)
				e.wantJobState(q1, store.JobPending)
			}
			e.wantRunState(queued2, store.RunQueued)
			e.wantRunState(otherGroup, store.RunQueued)
		})
	}
}

// TestComputeActionKey_Fetch：下游的 action key 包含上游输出的 manifest digest（Merkle 链）：上游输出相同则 key 相同，
// 不同则 key 不同；上游没有结果、没有这个输出或结果不在 CAS 中时，下游以"计算 action key 失败"结束。
func TestComputeActionKey_Fetch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// downJob 建一个 up → down 的 run（down 取 up 的输出 bin），up 以 manifest 成功，返回 down。
	downJob := func(e *env, input, manifest string) *cistore.Job {
		e.t.Helper()
		run := e.newRun(runOpts{})
		up := e.addJob(run, jobOpts{name: "up", input: input})
		down := e.addJob(run, jobOpts{
			name: "down", needs: []int64{up}, input: input,
			spec: workflow.Spec{Cache: true, Fetch: []workflow.FetchRef{{JobKey: "up", Output: "bin", To: "dist"}}},
		})
		e.advance(run)
		e.succeed(up, manifest)
		return e.job(down)
	}
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	out1 := e.resultManifest(map[string]string{"bin": digest("1")})
	out2 := e.resultManifest(map[string]string{"bin": digest("2")})

	t.Run("上游输出决定 action key", func(t *testing.T) {
		t.Parallel()
		e := e.sub(t)
		a, b, c := downJob(e, "merkle", out1), downJob(e, "merkle", out1), downJob(e, "merkle", out2)
		if a.ActionKey == nil || a.State != store.JobPending {
			t.Fatalf("下游应计算出 action key 并排队：%s %v", a.State, a.ActionKey)
		}
		var parts []byte
		if err := e.db.QueryRow(e.ctx, `SELECT key_parts FROM jobs WHERE id=$1`, a.ID).Scan(&parts); err != nil || len(parts) < 3 {
			t.Fatalf("应保存 key_parts：%s %v", parts, err)
		}
		// b 与 a 的 key 相同，此时 a 正在排队，b 跟随 a
		if deref(b.ActionKey) != deref(a.ActionKey) || b.Follows == nil || *b.Follows != a.ID {
			t.Errorf("上游输出相同时 action key 应相同并跟随：%v / %v，follows=%v", deref(b.ActionKey), deref(a.ActionKey), b.Follows)
		}
		if deref(c.ActionKey) == deref(a.ActionKey) {
			t.Error("上游输出不同时 action key 应不同")
		}
	})

	failures := []struct {
		name     string
		manifest string
		inReason string
	}{
		{"上游没有结果", "", "上游 up 没有结果"},
		{"没有这个输出", e.resultManifest(map[string]string{"other": digest("3")}), "没有名为 bin 的输出"},
		{"结果不在 CAS 中", digest("9"), "上游 up 的输出 bin"},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			j := downJob(e, tt.name, tt.manifest)
			if j.State != store.JobFailed || !strings.Contains(deref(j.Reason), "计算 action key 失败") || !strings.Contains(deref(j.Reason), tt.inReason) {
				t.Fatalf("下游应失败并说明原因：%s %q，期望包含 %q", j.State, deref(j.Reason), tt.inReason)
			}
			e.wantRunState(j.RunID, store.RunFailed)
		})
	}
}

// TestComputeActionKey_Parts：action key 由 PLAN #11.1 列出的各部分决定：spec 相同则 key 相同；
// 仓库的 cache_salt、输入摘要、step、环境变量、输出、runtime、services、executor 任一变化都得到不同的 key。
func TestComputeActionKey_Parts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := func() jobOpts {
		return jobOpts{
			input: "in",
			spec: workflow.Spec{
				Env:         map[string]string{"A": "1"},
				Steps:       []workflow.StepSpec{{Run: "make", Shell: "bash"}},
				Outputs:     []workflow.NamedPath{{Name: "bin", Path: "dist/"}},
				RuntimeRefs: []workflow.RuntimeRef{{Name: "go", Digest: "sha256:go1"}},
				Services:    []workflow.ServiceSpec{{Name: "pg", Image: "postgres"}},
			},
		}
	}
	// key 建一个只有这个 job 的 run 并推进，返回计算出的 action key。
	key := func(o jobOpts) string {
		t.Helper()
		run := e.newRun(runOpts{})
		id := e.addJob(run, o)
		e.advance(run)
		k := e.job(id).ActionKey
		if k == nil {
			t.Fatalf("job %d 没有 action key", id)
		}
		return *k
	}
	want := key(base())
	if got := key(base()); got != want {
		t.Fatalf("spec 相同时 action key 应相同：%s / %s", got, want)
	}
	changes := []struct {
		name   string
		modify func(o *jobOpts)
	}{
		{"输入摘要", func(o *jobOpts) { o.input = "other" }},
		{"step 命令", func(o *jobOpts) { o.spec.Steps[0].Run = "make test" }},
		{"step continue-on-error", func(o *jobOpts) { o.spec.Steps[0].ContinueOnError = true }},
		{"step 环境变量", func(o *jobOpts) { o.spec.Steps[0].Env = map[string]string{"X": "1"} }},
		{"job 环境变量", func(o *jobOpts) { o.spec.Env["A"] = "2" }},
		{"输出路径", func(o *jobOpts) { o.spec.Outputs[0].Path = "out/" }},
		{"runtime", func(o *jobOpts) { o.spec.RuntimeRefs[0].Digest = "sha256:go2" }},
		{"services", func(o *jobOpts) { o.spec.Services[0].Image = "postgres:17" }},
		{"executor", func(o *jobOpts) { o.spec.Executor, o.spec.Container = workflow.ExecutorOCI, "alpine" }},
		{"网络", func(o *jobOpts) { o.spec.Network = true }},
	}
	for _, c := range changes {
		o := base()
		c.modify(&o)
		if got := key(o); got == want {
			t.Errorf("%s 变化时 action key 应变化", c.name)
		}
	}
	// 仓库的 cache_salt 变化（kiln cache 清空缓存）后，同样的 spec 得到不同的 key
	e.exec(`UPDATE repos SET cache_salt='s2' WHERE id=$1`, testRepoID)
	if got := key(base()); got == want {
		t.Error("cache_salt 变化时 action key 应变化")
	}
}

func TestExecutorKeyPart(t *testing.T) {
	got := executorKeyPart(&workflow.Spec{Executor: workflow.ExecutorHost})
	profile, ok := got["profile"].(map[string]any)
	if !ok || profile["name"] != "default" {
		t.Fatalf("host executor 没有 profile 时应为 default：%v", got)
	}
	got = executorKeyPart(&workflow.Spec{Executor: workflow.ExecutorOCI, Container: "alpine"})
	if got["image"] != "alpine" || got["kind"] != string(workflow.ExecutorOCI) {
		t.Fatalf("oci executor：%v", got)
	}
	if _, ok := got["buildkit"]; ok {
		t.Fatalf("没有 buildkit 时不应改变 action key：%v", got)
	}
	got = executorKeyPart(&workflow.Spec{Executor: workflow.ExecutorOCI, Container: "alpine", BuildkitImage: "bk@sha256:1"})
	if got["buildkit"] != "bk@sha256:1" {
		t.Fatalf("buildkit 镜像应计入 action key：%v", got)
	}
	got = executorKeyPart(&workflow.Spec{Executor: workflow.ExecutorHost})
	if _, ok := got["signing"]; ok {
		t.Fatalf("没有 signing 时不应改变 action key：%v", got)
	}
	sign := func(p12 string) map[string]any {
		return executorKeyPart(&workflow.Spec{Executor: workflow.ExecutorHost, Signing: &workflow.Signing{
			Certificates: []workflow.SigningCertificate{{P12: p12}},
			Notary:       &workflow.Notary{Profile: "n", AppleID: "a", TeamID: "t", Password: "${{ secrets.P }}"},
		}})
	}
	a, b := sign("${{ secrets.A }}"), sign("${{ secrets.B }}")
	if a["signing"] == nil {
		t.Fatalf("signing 应计入 action key：%v", a)
	}
	ka, _, errA := actionkey.Compute(actionkey.Input{Executor: a})
	kb, _, errB := actionkey.Compute(actionkey.Input{Executor: b})
	if errA != nil || errB != nil || ka == kb {
		t.Fatalf("引用不同的 secret 应得到不同的 action key：%v %v", errA, errB)
	}
}

func TestServicesKeyPartSorted(t *testing.T) {
	got := servicesKeyPart([]workflow.ServiceSpec{{Name: "redis", Image: "redis"}, {Name: "pg", Image: "postgres"}})
	if len(got) != 2 {
		t.Fatalf("servicesKeyPart = %v", got)
	}
	first, ok := got[0].([]any)
	if !ok || first[0] != "pg" {
		t.Fatalf("应按名称排序：%v", got)
	}
}

// TestOutputManifest：从结果 manifest 中取出命名输出；没有这个输出、对象内容损坏时报错。
func TestOutputManifest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := e.resultManifest(map[string]string{"bin": "sha256:abc"})
	if got, err := e.eng.OutputManifest(e.ctx, d, "bin"); err != nil || got != "sha256:abc" {
		t.Fatalf("OutputManifest = %q, %v", got, err)
	}
	if _, err := e.eng.OutputManifest(e.ctx, d, "missing"); err == nil || !strings.Contains(err.Error(), "没有名为 missing 的输出") {
		t.Fatalf("没有这个输出时应报错：%v", err)
	}
	// 篡改对象内容：读取时按 digest 校验
	p, err := e.cas.Path(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"outputs":{"bin":"sha256:evil"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.OutputManifest(e.ctx, d, "bin"); err == nil || !strings.Contains(err.Error(), "内容损坏") {
		t.Fatalf("内容损坏时应报错：%v", err)
	}
}

func TestNextState(t *testing.T) {
	e := New(Config{InfraRetryMax: 2}, Deps{})
	tests := []struct {
		name  string
		job   cistore.Job
		res   AttemptResult
		want  store.JobState
		inMsg string
	}{
		{"成功", cistore.Job{}, AttemptResult{Outcome: store.OutcomeSucceeded}, store.JobSucceeded, ""},
		{"失败带消息", cistore.Job{}, AttemptResult{Outcome: store.OutcomeFailed, Message: "boom"}, store.JobFailed, "boom"},
		{"失败只有退出码", cistore.Job{}, AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 3}, store.JobFailed, "退出码 3"},
		{"超时", cistore.Job{}, AttemptResult{Outcome: store.OutcomeTimedOut}, store.JobTimedOut, "超时"},
		{"取消", cistore.Job{}, AttemptResult{Outcome: store.OutcomeCancelled}, store.JobCancelled, "已取消"},
		{"基础设施错误时已请求取消", cistore.Job{CancelRequested: true}, AttemptResult{Outcome: store.OutcomeInfraError}, store.JobCancelled, "已取消"},
		{"有副作用且已开始", cistore.Job{Effects: true, StartedStep: true}, AttemptResult{Outcome: store.OutcomeInfraError}, store.JobFailed, "不自动重试"},
		{"重试次数内重新排队", cistore.Job{AttemptCount: 2}, AttemptResult{Outcome: store.OutcomeInfraError}, store.JobPending, "重新排队"},
		{"超过重试次数", cistore.Job{AttemptCount: 3}, AttemptResult{Outcome: store.OutcomeInfraError}, store.JobFailed, "已重试 2 次"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, msg := e.nextState(&tt.job, tt.res)
			if got != tt.want || !strings.Contains(msg, tt.inMsg) {
				t.Fatalf("nextState = %s, %q；期望 %s，包含 %q", got, msg, tt.want, tt.inMsg)
			}
		})
	}
}

// TestFinishAttempt：attempt 结束后 job 的状态、原因、结果和 attempt 记录（PLAN #6.1 的 running 行）。
func TestFinishAttempt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name string
		opts jobOpts
		// prepare 在领取之后、结束之前修改 job（例如已开始执行 step、已请求取消）。
		prepare     string
		res         AttemptResult
		want        store.JobState
		inReason    string
		wantResult  bool
		wantOutcome store.Outcome
	}{
		{name: "成功", res: AttemptResult{Outcome: store.OutcomeSucceeded, Manifest: "sha256:r"}, want: store.JobSucceeded, wantResult: true, wantOutcome: store.OutcomeSucceeded},
		{name: "失败带消息", res: AttemptResult{Outcome: store.OutcomeFailed, Message: "boom", Manifest: "sha256:r"}, want: store.JobFailed, inReason: "boom", wantOutcome: store.OutcomeFailed},
		{name: "失败只有退出码", res: AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 3, ErrClass: "user"}, want: store.JobFailed, inReason: "step 退出码 3", wantOutcome: store.OutcomeFailed},
		{name: "超时", res: AttemptResult{Outcome: store.OutcomeTimedOut}, want: store.JobTimedOut, inReason: "超时", wantOutcome: store.OutcomeTimedOut},
		{name: "取消", res: AttemptResult{Outcome: store.OutcomeCancelled}, want: store.JobCancelled, inReason: "已取消", wantOutcome: store.OutcomeCancelled},
		{
			name: "基础设施错误重新排队", res: AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.fetch", Message: "下载失败"},
			want: store.JobPending, inReason: "第 1 次尝试遇到基础设施错误（infra.fetch：下载失败），重新排队", wantOutcome: store.OutcomeInfraError,
		},
		{
			name: "已请求取消时的基础设施错误", prepare: `UPDATE jobs SET cancel_requested=true WHERE id=$1`,
			res: AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost"}, want: store.JobCancelled, inReason: "已取消", wantOutcome: store.OutcomeInfraError,
		},
		{
			name: "有副作用且已开始执行 step", opts: jobOpts{effects: true}, prepare: `UPDATE jobs SET started_step=true WHERE id=$1`,
			res: AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost"}, want: store.JobFailed, inReason: "不自动重试", wantOutcome: store.OutcomeInfraError,
		},
		{
			name: "有副作用但还在准备环境", opts: jobOpts{effects: true},
			res: AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.prepare"}, want: store.JobPending, inReason: "重新排队", wantOutcome: store.OutcomeInfraError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			run := e.newRun(runOpts{})
			id := e.addJob(run, tt.opts)
			e.advance(run)
			e.claim(id)
			if tt.prepare != "" {
				e.exec(tt.prepare, id)
			}
			e.finish(id, tt.res)
			j := e.wantJobState(id, tt.want)
			if !strings.Contains(deref(j.Reason), tt.inReason) {
				t.Errorf("reason = %q，期望包含 %q", deref(j.Reason), tt.inReason)
			}
			if got := j.ResultManifest != nil; got != tt.wantResult {
				t.Errorf("result_manifest = %q，只有成功时才保存", deref(j.ResultManifest))
			}
			if j.LeaseExpiresAt != nil || (tt.want == store.JobPending && j.StartedStep) {
				t.Errorf("attempt 结束后应清除租约，重新排队时清除 started_step：lease=%v started_step=%v", j.LeaseExpiresAt, j.StartedStep)
			}
			if tt.want.Terminal() == (j.FinishedAt == nil) {
				t.Errorf("finished_at = %v，终态 = %v", j.FinishedAt, tt.want.Terminal())
			}
			as := e.attempts(id)
			if len(as) != 1 || deref(as[0].Outcome) != string(tt.wantOutcome) || deref(as[0].ErrClass) != tt.res.ErrClass {
				t.Errorf("attempt 记录 = %+v，期望 outcome=%s error_class=%q", as, tt.wantOutcome, tt.res.ErrClass)
			}
			e.wantOutbox(jobCheckKey(id))
		})
	}
}

// TestFinishAttempt_InfraRetryLimit：基础设施错误最多重试 infra_retry_max 次（一个 job 最多 3 个 attempt），
// 每个 attempt 按 epoch 各记录一次结果，之后判为 failed。
func TestFinishAttempt_InfraRetryLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	id := e.addJob(run, jobOpts{})
	e.advance(run)
	infra := AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost", Message: "失联"}
	for range testInfraRetryMax {
		e.claim(id)
		e.finish(id, infra)
		e.wantJobState(id, store.JobPending)
	}
	e.claim(id)
	e.finish(id, infra)
	j := e.wantJobState(id, store.JobFailed)
	if j.Epoch != testInfraRetryMax+1 || j.AttemptCount != testInfraRetryMax+1 {
		t.Errorf("epoch=%d attempt_count=%d，期望都是 %d", j.Epoch, j.AttemptCount, testInfraRetryMax+1)
	}
	if !strings.Contains(deref(j.Reason), fmt.Sprintf("已重试 %d 次", testInfraRetryMax)) {
		t.Errorf("reason = %q", deref(j.Reason))
	}
	as := e.attempts(id)
	if len(as) != testInfraRetryMax+1 {
		t.Fatalf("应有 %d 个 attempt：%+v", testInfraRetryMax+1, as)
	}
	for i, a := range as {
		if a.Epoch != int64(i+1) || deref(a.Outcome) != string(store.OutcomeInfraError) {
			t.Errorf("第 %d 个 attempt：epoch=%d outcome=%q", i+1, a.Epoch, deref(a.Outcome))
		}
	}
	e.wantRunState(run, store.RunFailed)
}

// TestFinishAttempt_ResultRecordedOnce：每个 epoch 的 attempt 只记录一次结果：已经有结果的 attempt 不会被再次覆盖
// （例如 sweeper 判定租约过期后，旧 epoch 的结果晚到）。重试后新 epoch 的结果记录在新的 attempt 上。
// 拒绝旧 epoch 的提交（STALE_EPOCH）由 runnerapi 在调用 FinishAttempt 之前完成。
func TestFinishAttempt_ResultRecordedOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	id := e.addJob(run, jobOpts{})
	e.advance(run)
	e.claim(id)
	stale := e.job(id) // epoch 1 时的 job（current_attempt_id 指向第 1 个 attempt）
	e.finish(id, AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.lost"})
	e.claim(id)
	// 用 epoch 1 的 attempt 再结束一次：epoch 已前进，返回 ErrStale，第 1 个 attempt 的结果不变
	err := e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		return e.eng.FinishAttempt(e.ctx, tx, stale, AttemptResult{Outcome: store.OutcomeInfraError, ErrClass: "infra.upload", Message: "晚到"}, nil)
	})
	if !errors.Is(err, ErrStale) {
		e.t.Fatalf("晚到的旧 epoch 结果应返回 ErrStale：%v", err)
	}
	e.finish(id, AttemptResult{Outcome: store.OutcomeSucceeded})
	as := e.attempts(id)
	if len(as) != 2 || deref(as[0].Outcome) != string(store.OutcomeInfraError) || deref(as[1].Outcome) != string(store.OutcomeSucceeded) {
		t.Fatalf("attempt 记录 = %+v，期望第 1 个为 infra_error（不被覆盖）、第 2 个为 succeeded", as)
	}
}

// TestWriteActionCache：PLAN #6.1 的写缓存规则。key 不存在时插入；已存在且可疑、或本次强制执行时覆盖并清除可疑标记；
// 其他情况（并发重复执行）保留旧条目。失败、不可缓存、有副作用的 job 不写入。
func TestWriteActionCache(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const old, fresh = "sha256:old", "sha256:new"
	const oldProducer = 999
	tests := []struct {
		name string
		opts jobOpts
		// existing 是 job 执行期间出现的缓存条目："" 表示没有，"ok" 正常，"suspect" 可疑。
		existing    string
		outcome     store.Outcome
		wantFresh   bool // 期望条目为本次结果
		wantMissing bool // 期望没有条目
	}{
		{name: "插入新条目", opts: jobOpts{spec: workflow.Spec{Cache: true}}, outcome: store.OutcomeSucceeded, wantFresh: true},
		{name: "并发重复执行保留旧条目", opts: jobOpts{spec: workflow.Spec{Cache: true}}, existing: "ok", outcome: store.OutcomeSucceeded},
		{name: "覆盖可疑条目", opts: jobOpts{spec: workflow.Spec{Cache: true}}, existing: "suspect", outcome: store.OutcomeSucceeded, wantFresh: true},
		{name: "强制执行覆盖", opts: jobOpts{spec: workflow.Spec{Cache: true}, force: true}, existing: "ok", outcome: store.OutcomeSucceeded, wantFresh: true},
		{name: "失败不写入", opts: jobOpts{spec: workflow.Spec{Cache: true}}, outcome: store.OutcomeFailed, wantMissing: true},
		{name: "不可缓存不写入", opts: jobOpts{}, outcome: store.OutcomeSucceeded, wantMissing: true},
		{name: "有副作用不写入", opts: jobOpts{spec: workflow.Spec{Cache: true}, effects: true}, outcome: store.OutcomeSucceeded, wantMissing: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			tt.opts.input = tt.name // 每个用例的 action key 互不相同
			run := e.newRun(runOpts{})
			id := e.addJob(run, tt.opts)
			e.advance(run)
			e.claim(id)
			key := deref(e.job(id).ActionKey)
			if tt.existing != "" {
				e.exec(`INSERT INTO action_cache (action_key, manifest_digest, producer_job_id, suspect) VALUES ($1,$2,$3,$4)`,
					key, old, oldProducer, tt.existing == "suspect")
			}
			e.finish(id, AttemptResult{Outcome: tt.outcome, Manifest: fresh})
			ent := e.cacheEntry(key)
			switch {
			case tt.wantMissing:
				if ent != nil {
					t.Fatalf("不应写入缓存：%+v", ent)
				}
			case tt.wantFresh:
				if ent == nil || ent.Manifest != fresh || ent.Producer != id || ent.Suspect {
					t.Fatalf("缓存条目 = %+v，期望本次结果（producer %d）且不可疑", ent, id)
				}
			default:
				if ent == nil || ent.Manifest != old || ent.Producer != oldProducer {
					t.Fatalf("缓存条目 = %+v，期望保留旧条目", ent)
				}
			}
		})
	}
}

// TestFailFast：矩阵变体以 failed 或 timed_out 结束且 fail-fast 时，同一矩阵中尚未结束的变体被取消
// （排队的直接 cancelled，执行中的请求 Runner 取消），cancel_reason 为 fail_fast；其他 job 不受影响。
// fail-fast 的取消不计入 run 的 cancelled 结论。
func TestFailFast(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tests := []struct {
		name     string
		failFast bool
		res      AttemptResult
	}{
		{"fail-fast 失败", true, AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 1}},
		{"fail-fast 超时", true, AttemptResult{Outcome: store.OutcomeTimedOut}},
		{"关闭 fail-fast", false, AttemptResult{Outcome: store.OutcomeFailed, ExitCode: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := e.sub(t)
			run := e.newRun(runOpts{})
			variant := func(osName string) int64 {
				return e.addJob(run, jobOpts{
					name: "test", key: "test{os=" + osName + "}", matrix: map[string]any{"os": osName},
					spec: workflow.Spec{FailFast: tt.failFast},
				})
			}
			v1, v2, v3 := variant("a"), variant("b"), variant("c")
			lint := e.addJob(run, jobOpts{name: "lint"})
			e.advance(run)
			e.claim(v1)
			e.claim(v2)
			e.finish(v1, tt.res)

			e.wantJobState(lint, store.JobPending)
			running, queued := e.job(v2), e.job(v3)
			if !tt.failFast {
				if running.CancelRequested || queued.State != store.JobPending {
					t.Fatalf("关闭 fail-fast 时其他变体不受影响：v2 cancel_requested=%v，v3 %s", running.CancelRequested, queued.State)
				}
				return
			}
			if running.State != store.JobRunning || !running.CancelRequested || deref(running.CancelReason) != string(store.CancelFailFast) ||
				!strings.Contains(deref(running.Reason), "等待 Runner 确认") {
				t.Fatalf("执行中的变体应请求取消：%s cancel_requested=%v cancel_reason=%q reason=%q",
					running.State, running.CancelRequested, deref(running.CancelReason), deref(running.Reason))
			}
			if queued.State != store.JobCancelled || deref(queued.CancelReason) != string(store.CancelFailFast) {
				t.Fatalf("排队的变体应取消：%s cancel_reason=%q", queued.State, deref(queued.CancelReason))
			}
			e.wantOutbox(jobCheckKey(v3))
			// Runner 确认取消、其余 job 成功后，run 的结论是 failure（不是 cancelled）
			e.finish(v2, AttemptResult{Outcome: store.OutcomeCancelled})
			e.succeed(lint, "")
			e.wantRunState(run, store.RunFailed)
		})
	}
}

// TestCancelRun：取消 run 的当前运行。等待中和排队中的 job 直接 cancelled；执行中的 job 请求 Runner 取消；
// 执行中的发布 job 不取消（PLAN #13.4）；已结束的 job 不变。所有 job 结束后 run 的结论为 cancelled。
func TestCancelRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	done := e.addJob(run, jobOpts{name: "done"})
	running := e.addJob(run, jobOpts{name: "running"})
	pending := e.addJob(run, jobOpts{name: "pending"})
	waiting := e.addJob(run, jobOpts{name: "waiting", needs: []int64{running}})
	pub := e.addJob(run, jobOpts{name: "publish", kind: workflow.JobKindPublish})
	e.advance(run)
	e.succeed(done, "")
	e.claim(running)
	e.claim(pub)

	cancel := func() {
		t.Helper()
		e.tx(func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, run, store.CancelByUser) })
	}
	cancel()
	for _, id := range []int64{pending, waiting} {
		j := e.wantJobState(id, store.JobCancelled)
		if deref(j.CancelReason) != string(store.CancelByUser) || deref(j.Reason) != "已取消" || j.FinishedAt == nil {
			t.Errorf("job %s：cancel_reason=%q reason=%q finished_at=%v", j.JobKey, deref(j.CancelReason), deref(j.Reason), j.FinishedAt)
		}
		e.wantOutbox(jobCheckKey(id))
	}
	if j := e.wantJobState(running, store.JobRunning); !j.CancelRequested || deref(j.Reason) != "已取消（等待 Runner 确认）" {
		t.Errorf("执行中的 job 应请求取消：cancel_requested=%v reason=%q", j.CancelRequested, deref(j.Reason))
	}
	// 运行中的发布 job 也允许取消：设置 cancel_requested，阶段 A 检查、阶段 B 不再中断（PLAN #13.4）
	if j := e.wantJobState(pub, store.JobRunning); !j.CancelRequested || !strings.HasPrefix(deref(j.Reason), "已取消（等待") {
		t.Errorf("执行中的发布 job 应请求取消：cancel_requested=%v reason=%q", j.CancelRequested, deref(j.Reason))
	}
	e.wantJobState(done, store.JobSucceeded)
	e.wantRunState(run, store.RunActive)

	// 再次取消：已请求取消的 job 不重复处理
	e.exec(`UPDATE jobs SET reason='标记' WHERE id=$1`, running)
	cancel()
	if j := e.job(running); deref(j.Reason) != "标记" {
		t.Errorf("已请求取消的 job 不应被再次修改：reason=%q", deref(j.Reason))
	}

	e.finish(running, AttemptResult{Outcome: store.OutcomeCancelled})
	e.wantRunState(run, store.RunActive) // 发布 job 还在执行
	e.finish(pub, AttemptResult{Outcome: store.OutcomeSucceeded})
	e.wantRunState(run, store.RunCancelled)
	e.wantOutbox(runCheckKey(run, 0))

	// 已结束的 run 取消时不做任何事
	cancel()
	e.wantRunState(run, store.RunCancelled)
}

// TestCancelRun_PublishStageB：运行中的发布 job 只在阶段 A 可以取消；已经进入阶段 B（commit_started_at 非空）
// 的发布不可取消，重启续做时继续完成剩余目标（PLAN #13.4）。
func TestCancelRun_PublishStageB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{})
	stageA := e.addJob(run, jobOpts{name: "publish-a", kind: workflow.JobKindPublish})
	stageB := e.addJob(run, jobOpts{name: "publish-b", kind: workflow.JobKindPublish})
	e.advance(run)
	e.claim(stageA)
	e.claim(stageB)
	e.exec(`INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag) VALUES ($1,$2,$3,'tester/app','v1.0.0')`,
		stageA, run, testRepoID)
	e.exec(`INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag, commit_started_at)
		VALUES ($1,$2,$3,'tester/app','v1.0.1', now())`, stageB, run, testRepoID)
	before := deref(e.job(stageB).Reason)

	e.tx(func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, run, store.CancelByUser) })

	if j := e.job(stageA); !j.CancelRequested || deref(j.Reason) != "已取消（等待 Runner 确认）" {
		t.Errorf("阶段 A 的发布 job 应请求取消：cancel_requested=%v reason=%q", j.CancelRequested, deref(j.Reason))
	}
	if j := e.job(stageB); j.CancelRequested || j.State != store.JobRunning || deref(j.Reason) != before {
		t.Errorf("阶段 B 的发布 job 不可取消：cancel_requested=%v state=%s reason=%q（之前 %q）",
			j.CancelRequested, j.State, deref(j.Reason), before)
	}

	e.finish(stageA, AttemptResult{Outcome: store.OutcomeCancelled})
	e.finish(stageB, AttemptResult{Outcome: store.OutcomeSucceeded})
	e.wantRunState(run, store.RunCancelled)
}

// TestCancelRun_QueuedRun：并发组中排队的 run 被取消时直接结束，它的 job 全部 cancelled。
func TestCancelRun_QueuedRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.newRun(runOpts{state: store.RunQueued, concurrency: "g"})
	a := e.addJob(run, jobOpts{name: "a"})
	b := e.addJob(run, jobOpts{name: "b", needs: []int64{a}})
	e.tx(func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, run, store.CancelByConcurrency) })
	for _, id := range []int64{a, b} {
		if j := e.wantJobState(id, store.JobCancelled); deref(j.CancelReason) != string(store.CancelByConcurrency) {
			t.Errorf("cancel_reason = %q", deref(j.CancelReason))
		}
	}
	e.wantRunState(run, store.RunCancelled)
}

// TestCancelRun_NotFound：run 不存在时返回错误。
func TestCancelRun_NotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	err := e.db.Tx(e.ctx, func(tx pgx.Tx) error { return e.eng.CancelRun(e.ctx, tx, 12345, store.CancelByUser) })
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CancelRun 不存在的 run：%v，期望包装 pgx.ErrNoRows", err)
	}
}

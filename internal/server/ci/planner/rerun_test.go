package planner

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// chainWorkflow：a → b → c，d 独立。
const chainWorkflow = `name: chain
on:
  push:
    branches: [main]
jobs:
  a:
    runs-on: linux/arm64
    steps:
      - run: echo a
  b:
    needs: [a]
    runs-on: linux/arm64
    steps:
      - run: echo b
  c:
    needs: [b]
    runs-on: linux/arm64
    steps:
      - run: echo c
  d:
    runs-on: linux/arm64
    steps:
      - run: echo d
`

// chainRun 规划 chainWorkflow 并让它结束：a、d 成功，b 失败，c 跳过。a、d 的结果写入 action cache
// （与真实执行成功后一样），返回 run ID 和第 0 次运行的 job。
func chainRun(e *testEnv) (int64, map[string]*cistore.Job) {
	e.t.Helper()
	sha := e.commit(map[string]string{wf("chain.yml"): chainWorkflow})
	r := e.planOne(e.pushReq("refs/heads/main", sha))
	e.finishRun(r.ID, map[string]store.JobState{"b": store.JobFailed, "c": store.JobSkipped})
	prev := e.jobs(r.ID, 0)
	for _, k := range []string{"a", "d"} {
		e.exec(`INSERT INTO action_cache (action_key, manifest_digest, producer_job_id) VALUES ($1,$2,$3)`, *prev[k].ActionKey, testManifest, prev[k].ID)
	}
	if got := e.run(r.ID); got.State != store.RunFailed {
		e.t.Fatalf("run 应失败：%s", got.State)
	}
	return r.ID, prev
}

// TestRerunModes：四种重跑方式（PLAN #6.6）决定哪些 job 沿用上一次的结果、哪些重新规划、哪些强制执行；
// 重新规划的 job 照常经就绪判定（命中缓存、pending 或等待上游）。
func TestRerunModes(t *testing.T) {
	t.Parallel()
	const (
		reused  = "reused"  // 沿用上一次的结果
		cached  = "cached"  // 重新规划，命中缓存
		pending = "pending" // 重新规划，排队
		waiting = "waiting" // 重新规划，等待上游
	)
	tests := []struct {
		name  string
		mode  store.RerunMode
		job   string // mode 为 job 时重跑的 job
		want  map[string]string
		force []string
		// ended 为 true 表示重跑后 run 立即结束（失败）
		ended bool
	}{
		{
			name: "只重跑未成功的（默认）",
			want: map[string]string{"a": reused, "b": pending, "c": waiting, "d": reused},
		},
		{
			name: "全部重跑", mode: store.RerunAll,
			want: map[string]string{"a": cached, "b": pending, "c": waiting, "d": cached},
		},
		{
			name: "全部重跑并跳过缓存", mode: store.RerunAllNoCache,
			want:  map[string]string{"a": pending, "b": waiting, "c": waiting, "d": pending},
			force: []string{"a", "b", "c", "d"},
		},
		{
			name: "重跑成功的 job：它及下游重新规划，它强制执行", mode: store.RerunJob, job: "a",
			want:  map[string]string{"a": pending, "b": waiting, "c": waiting, "d": reused},
			force: []string{"a"},
		},
		{
			name: "重跑失败的 job：它及下游重新规划", mode: store.RerunJob, job: "b",
			want: map[string]string{"a": reused, "b": pending, "c": waiting, "d": reused},
		},
		{
			name: "重跑叶子 job", mode: store.RerunJob, job: "c",
			// c 的上游 b 沿用失败的结果，c 随即被跳过
			want:  map[string]string{"a": reused, "b": reused, "c": "skipped", "d": reused},
			ended: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			runID, prev := chainRun(e)
			req := RerunRequest{RunID: runID, Mode: tt.mode, Actor: "alice"}
			if tt.job != "" {
				req.JobID = prev[tt.job].ID
			}
			n, err := e.p.Rerun(e.ctx, req)
			if err != nil || n != 1 {
				t.Fatalf("Rerun = %d, %v，期望第 1 次重跑", n, err)
			}
			r := e.run(runID)
			if r.CurrentRerun != 1 || (r.State == store.RunFailed) != tt.ended || r.State.Open() == tt.ended {
				t.Fatalf("run：current_rerun=%d state=%s finished_at=%v", r.CurrentRerun, r.State, r.FinishedAt)
			}
			jobs := e.jobs(runID, 1)
			if got := slices.Sorted(maps.Keys(jobs)); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
				t.Fatalf("第 1 次重跑的 job = %v", got)
			}
			var force []string
			for k, j := range jobs {
				p := prev[k]
				switch want := tt.want[k]; want {
				case reused:
					if j.ReusedFrom == nil || *j.ReusedFrom != p.ID || j.State != p.State || deref(j.ResultManifest) != deref(p.ResultManifest) {
						t.Errorf("%s 应沿用 job #%d：state=%s reused_from=%v", k, p.ID, j.State, j.ReusedFrom)
					}
				case cached:
					if j.State != store.JobSucceeded || j.CachedFrom == nil || *j.CachedFrom != p.ID || j.ReusedFrom != nil {
						t.Errorf("%s 应命中缓存（来自 job #%d）：state=%s cached_from=%v", k, p.ID, j.State, j.CachedFrom)
					}
				default:
					if j.ReusedFrom != nil || string(j.State) != want {
						t.Errorf("%s 应重新规划为 %s：state=%s（%s）reused_from=%v", k, want, j.State, deref(j.Reason), j.ReusedFrom)
					}
				}
				if j.ForceExecute {
					force = append(force, k)
				}
				// 沿用的 job 不新建 Check Run，其他的新建
				var hasCheck bool
				if err := e.db.QueryRow(e.ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE dedupe_key=$1)`, fmt.Sprintf("checkrun:job:%d", j.ID)).Scan(&hasCheck); err != nil {
					t.Fatal(err)
				}
				if hasCheck == (tt.want[k] == reused) {
					t.Errorf("%s 的 Check Run 记录：有=%v，沿用=%v", k, hasCheck, tt.want[k] == reused)
				}
			}
			slices.Sort(force)
			if !slices.Equal(force, tt.force) {
				t.Errorf("force_execute 的 job = %v，期望 %v", force, tt.force)
			}
			e.wantOutbox(fmt.Sprintf("checkrun:run:%d:1", runID))
			var audits int
			if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='run.rerun' AND actor='alice'`).Scan(&audits); err != nil || audits != 1 {
				t.Errorf("应记录 1 条审计日志，实际 %d（%v）", audits, err)
			}
			// 上一次运行的 job 不受影响
			for k, j := range e.jobs(runID, 0) {
				if j.State != prev[k].State {
					t.Errorf("第 0 次运行的 %s 不应改变：%s → %s", k, prev[k].State, j.State)
				}
			}
		})
	}
}

// TestRerunPurged：run 的日志和产物已经按保留策略清理（PLAN #10.3）时，成功的 job 不能沿用结果，重新规划
// （缓存条目仍然有效时命中缓存）；规划之后、创建 rerun 之前 run 被清理时拒绝重跑，不沿用已经不在的产物。
func TestRerunPurged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	runID, prev := chainRun(e)
	e.exec(`UPDATE runs SET purged_at=now() WHERE id=$1`, runID)
	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: runID, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	jobs := e.jobs(runID, 1)
	for _, k := range []string{"a", "d"} {
		j := jobs[k]
		if j.ReusedFrom != nil || j.State != store.JobSucceeded || j.CachedFrom == nil || *j.CachedFrom != prev[k].ID {
			t.Errorf("%s 应重新规划并命中缓存：state=%s reused_from=%v cached_from=%v", k, j.State, j.ReusedFrom, j.CachedFrom)
		}
	}
	if jobs["b"].State != store.JobPending {
		t.Errorf("b 应重新规划：%s", jobs["b"].State)
	}

	// 规划时还没有清理，创建 rerun 时已经清理：拒绝
	e.finishRun(runID, nil)
	run := e.run(runID)
	pr, err := e.p.replan(e.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := rerunPlan(RerunRequest{RunID: runID}, mapsValues(e.jobs(runID, 1)), pr, false)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE runs SET purged_at=now() WHERE id=$1`, runID)
	err = e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		_, _, err := e.p.startRerun(e.ctx, tx, RerunRequest{RunID: runID, Actor: "alice"}, run, pr, opt)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "刚刚按保留策略清理") {
		t.Fatalf("错误 = %v，期望说明刚刚清理", err)
	}
	if r := e.run(runID); r.CurrentRerun != 1 {
		t.Fatalf("被拒绝的重跑不应改变 run：current_rerun=%d", r.CurrentRerun)
	}
}

// mapsValues 返回 map 的全部值。
func mapsValues(m map[string]*cistore.Job) []*cistore.Job { return slices.Collect(maps.Values(m)) }

// TestRerunRejected：run 还没有结束、单个 job 不属于最新一次运行、未知的重跑方式都不能重跑，也不改变 run。
func TestRerunRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	runID, prev := chainRun(e)
	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		req   RerunRequest
		inErr string
	}{
		{"run 还没有结束", RerunRequest{RunID: runID, Mode: store.RerunAll}, "还没有结束"},
		{"run 不存在", RerunRequest{RunID: 99999}, "读取 run 99999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.p.Rerun(e.ctx, tt.req); err == nil || !strings.Contains(err.Error(), tt.inErr) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tt.inErr)
			}
		})
	}
	// 结束后：第 0 次运行的 job 已不属于最新一次运行
	e.finishRun(runID, nil)
	for _, tt := range []struct {
		name  string
		req   RerunRequest
		inErr string
	}{
		{"job 不属于最新一次运行", RerunRequest{RunID: runID, Mode: store.RerunJob, JobID: prev["a"].ID}, "不属于 run"},
		{"未知的重跑方式", RerunRequest{RunID: runID, Mode: "bogus"}, "未知的重跑方式"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.p.Rerun(e.ctx, tt.req); err == nil || !strings.Contains(err.Error(), tt.inErr) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tt.inErr)
			}
		})
	}
	if r := e.run(runID); r.CurrentRerun != 1 {
		t.Fatalf("被拒绝的重跑不应改变 run：current_rerun=%d", r.CurrentRerun)
	}
}

// TestRerunDelivery：同一个 webhook delivery 对同一个 run 最多创建一次 rerun（PLAN #6.6），
// 不论那次 rerun 是否已经结束；新的 delivery 照常重跑。KeepOnFailure 记录在 rerun 和新 job 上。
func TestRerunDelivery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	runID, _ := chainRun(e)
	rerun := func(delivery string) int {
		t.Helper()
		n, err := e.p.Rerun(e.ctx, RerunRequest{RunID: runID, DeliveryID: delivery, KeepOnFailure: true, Actor: "github"})
		if err != nil {
			t.Fatalf("Rerun(%s)：%v", delivery, err)
		}
		return n
	}
	if n := rerun("d1"); n != 1 {
		t.Fatalf("第一次应创建第 1 次重跑：%d", n)
	}
	// run 仍在执行：同一个 delivery 直接返回，不当作"还没有结束"的错误
	if n := rerun("d1"); n != 1 {
		t.Fatalf("同一个 delivery 应返回同一次重跑：%d", n)
	}
	e.finishRun(runID, nil)
	if n := rerun("d1"); n != 1 {
		t.Fatalf("重跑结束后同一个 delivery 仍不再重跑：%d", n)
	}
	if n := rerun("d2"); n != 2 {
		t.Fatalf("新的 delivery 应创建第 2 次重跑：%d", n)
	}
	var keep bool
	var delivery string
	if err := e.db.QueryRow(e.ctx, `SELECT keep_on_failure, delivery_id FROM run_reruns WHERE run_id=$1 AND rerun=2`, runID).Scan(&keep, &delivery); err != nil {
		t.Fatal(err)
	}
	if !keep || delivery != "d2" {
		t.Errorf("run_reruns：keep_on_failure=%v delivery_id=%q", keep, delivery)
	}
	for k, j := range e.jobs(runID, 2) {
		if j.ReusedFrom == nil && !j.KeepOnFailure {
			t.Errorf("重新规划的 job %s 应设置 keep_on_failure", k)
		}
	}
	if r := e.run(runID); r.CurrentRerun != 2 {
		t.Fatalf("current_rerun = %d，期望 2", r.CurrentRerun)
	}
}

// TestRerunConfigError：重跑使用当前的配置（PLAN #6.6）。引用的 environment 已被删除时，
// 新的 rerun 以配置错误直接失败，不插入 job。
func TestRerunConfigError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addEnvironment("staging", envOpts{})
	sha := e.commit(map[string]string{wf("env.yml"): envWorkflow})
	r := e.planOne(e.pushReq("refs/heads/main", sha))
	e.finishRun(r.ID, nil)
	e.exec(`DELETE FROM environments WHERE name='staging'`)

	n, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r.ID, Mode: store.RerunAll})
	if err != nil || n != 1 {
		t.Fatalf("Rerun = %d, %v", n, err)
	}
	got := e.run(r.ID)
	if got.State != store.RunFailed || !strings.Contains(deref(got.ConfigError), "environment staging 不存在") || got.FinishedAt == nil {
		t.Fatalf("rerun 应以配置错误失败：state=%s config_error=%q", got.State, deref(got.ConfigError))
	}
	if s := e.rerunState(r.ID, 1); s != store.RunFailed {
		t.Errorf("run_reruns 第 1 次运行的状态 = %s", s)
	}
	if jobs := e.jobs(r.ID, 1); len(jobs) != 0 {
		t.Errorf("配置错误的 rerun 不应有 job，实际 %d 个", len(jobs))
	}
	e.wantOutbox(fmt.Sprintf("checkrun:run:%d:1", r.ID))
}

// TestRerunConcurrency：重跑同样受并发组约束（PLAN #6.5）：组内有执行中的 run 时重跑排队，那个 run 结束后开始。
func TestRerunConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha1 := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(false), "v.txt": "1"})
	r1 := e.planOne(e.pushReq("refs/heads/main", sha1))
	e.finishRun(r1.ID, map[string]store.JobState{"build": store.JobFailed})
	sha2 := e.commit(map[string]string{"v.txt": "2"})
	r2 := e.planOne(e.pushReq("refs/heads/main", sha2))
	if r2.State != store.RunActive {
		t.Fatalf("第二个 run 应开始：%s", r2.State)
	}

	if n, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r1.ID}); err != nil || n != 1 {
		t.Fatalf("Rerun = %d, %v", n, err)
	}
	if got := e.run(r1.ID); got.State != store.RunQueued || e.rerunState(r1.ID, 1) != store.RunQueued {
		t.Fatalf("组内有执行中的 run，重跑应排队：%s", got.State)
	}
	if j := e.jobs(r1.ID, 1)["build"]; j.State != store.JobWaiting {
		t.Fatalf("排队中的重跑的 job 应保持 waiting：%s", j.State)
	}

	e.finishRun(r2.ID, nil)
	if got := e.run(r1.ID); got.State != store.RunActive {
		t.Fatalf("执行中的 run 结束后，排队的重跑应开始：%s", got.State)
	}
	if j := e.jobs(r1.ID, 1)["build"]; j.State != store.JobPending {
		t.Fatalf("开始后的重跑的 job 应进入 pending：%s", j.State)
	}
}

// TestRerunConcurrencyReplacesQueued：组内已经有排队的 run 时，重跑取代它（取消，cancel_reason=concurrency），
// 组内最多一个 run 排队（PLAN #6.5、#6.6）。
func TestRerunConcurrencyReplacesQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha1 := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(false), "v.txt": "1"})
	r1 := e.planOne(e.pushReq("refs/heads/main", sha1))
	e.finishRun(r1.ID, map[string]store.JobState{"build": store.JobFailed})
	r2 := e.planOne(e.pushReq("refs/heads/main", e.commit(map[string]string{"v.txt": "2"})))
	r3 := e.planOne(e.pushReq("refs/heads/main", e.commit(map[string]string{"v.txt": "3"})))
	if got := e.run(r3.ID); got.State != store.RunQueued {
		t.Fatalf("第三个 run 应排队：%s", got.State)
	}

	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r1.ID}); err != nil {
		t.Fatal(err)
	}
	if got := e.run(r1.ID); got.State != store.RunQueued || e.rerunState(r1.ID, 1) != store.RunQueued {
		t.Fatalf("重跑应排队：%s", got.State)
	}
	if got := e.run(r3.ID); got.State != store.RunCancelled {
		t.Fatalf("排队中的 run 应被重跑取代（取消），实际 %s", got.State)
	}
	if j := e.jobs(r3.ID, 0)["build"]; j.State != store.JobCancelled || deref(j.CancelReason) != string(store.CancelByConcurrency) {
		t.Fatalf("被取代的 run 的 job：%s cancel_reason=%s", j.State, deref(j.CancelReason))
	}
	if got := e.run(r2.ID); got.State != store.RunActive {
		t.Fatalf("执行中的 run 不受影响：%s", got.State)
	}
}

// TestRerunConcurrencyCancelInProgress：cancel-in-progress 为 true 时，重跑与新建 run 一样取消组内未结束的 run 并立即开始。
func TestRerunConcurrencyCancelInProgress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha1 := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(true), "v.txt": "1"})
	r1 := e.planOne(e.pushReq("refs/heads/main", sha1))
	e.finishRun(r1.ID, map[string]store.JobState{"build": store.JobFailed})
	r2 := e.planOne(e.pushReq("refs/heads/main", e.commit(map[string]string{"v.txt": "2"})))

	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r1.ID}); err != nil {
		t.Fatal(err)
	}
	if got := e.run(r2.ID); got.State != store.RunCancelled {
		t.Fatalf("组内执行中的 run 应被取消：%s", got.State)
	}
	if got := e.run(r1.ID); got.State != store.RunActive || e.rerunState(r1.ID, 1) != store.RunActive {
		t.Fatalf("重跑应立即开始：%s", got.State)
	}
	if j := e.jobs(r1.ID, 1)["build"]; j.State != store.JobPending {
		t.Fatalf("重跑的 job 应进入 pending：%s", j.State)
	}
}

// TestRerunUsesPlannedConcurrencyGroup：重跑的并发组来自这次的规划，而不是 run 上的旧值（PLAN #6.6）：
// 旧值缺失（并发组是后来的版本才记录的）时，重跑照样加入规划的组，并按组内的执行情况排队。
func TestRerunUsesPlannedConcurrencyGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha1 := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(false), "v.txt": "1"})
	r1 := e.planOne(e.pushReq("refs/heads/main", sha1))
	e.finishRun(r1.ID, map[string]store.JobState{"build": store.JobFailed})
	// 模拟旧版本创建的 run：行上没有记录并发组
	e.exec(`UPDATE runs SET concurrency_key=NULL, cancel_in_progress=false WHERE id=$1`, r1.ID)
	r2 := e.planOne(e.pushReq("refs/heads/main", e.commit(map[string]string{"v.txt": "2"})))
	if got := e.run(r2.ID); got.State != store.RunActive || got.ConcurrencyKey == nil {
		t.Fatalf("第二个 run 应加入并发组并开始：%s key=%v", got.State, got.ConcurrencyKey)
	}

	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r1.ID}); err != nil {
		t.Fatal(err)
	}
	got := e.run(r1.ID)
	if got.ConcurrencyKey == nil || *got.ConcurrencyKey != "deploy-refs/heads/main" {
		t.Fatalf("重跑应加入这次规划的并发组，实际 key=%v", got.ConcurrencyKey)
	}
	if got.State != store.RunQueued || e.rerunState(r1.ID, 1) != store.RunQueued {
		t.Fatalf("组内有执行中的 run，重跑应排队：%s", got.State)
	}
	e.finishRun(r2.ID, nil)
	if got := e.run(r1.ID); got.State != store.RunActive {
		t.Fatalf("执行中的 run 结束后，排队的重跑应开始：%s", got.State)
	}
}

// TestRerunConcurrencyGroupChangedAfterPlan：规划之后 run 的并发组刚被另一个重跑改过时，新组没有预先锁住，
// 重跑被拒绝（不补锁，否则违反加锁顺序）并保持 run 不变。
func TestRerunConcurrencyGroupChangedAfterPlan(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{wf("conc.yml"): concurrencyWorkflow(false), "v.txt": "1"})
	r := e.planOne(e.pushReq("refs/heads/main", sha))
	e.finishRun(r.ID, nil)
	prev := e.run(r.ID)
	pr, err := e.p.replan(e.ctx, prev)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := rerunPlan(RerunRequest{RunID: r.ID}, mapsValues(e.jobs(r.ID, 0)), pr, false)
	if err != nil {
		t.Fatal(err)
	}
	opt.prevRerun = prev.CurrentRerun
	// 规划之后另一个重跑把 run 换到了别的组
	e.exec(`UPDATE runs SET concurrency_key='another-group' WHERE id=$1`, r.ID)

	err = e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		_, _, err := e.p.startRerun(e.ctx, tx, RerunRequest{RunID: r.ID, Actor: "alice"}, prev, pr, opt)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "并发组刚刚被另一个重跑改变") {
		t.Fatalf("错误 = %v，期望说明并发组刚刚变化", err)
	}
	if got := e.run(r.ID); got.CurrentRerun != 0 || got.State != store.RunSucceeded {
		t.Fatalf("被拒绝的重跑不应改变 run：current_rerun=%d state=%s", got.CurrentRerun, got.State)
	}
}

// TestRerunStalePlan：规划之后 run 又被重跑过并且已经结束时，按旧的一次运行规划的重跑被拒绝，不改变 run。
func TestRerunStalePlan(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	runID, prev := chainRun(e)
	run := e.run(runID)
	pr, err := e.p.replan(e.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := rerunPlan(RerunRequest{RunID: runID}, mapsValues(prev), pr, false)
	if err != nil {
		t.Fatal(err)
	}
	opt.prevRerun = run.CurrentRerun

	// 规划之后有另一个重跑创建并结束
	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	e.finishRun(runID, nil)

	err = e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		_, _, err := e.p.startRerun(e.ctx, tx, RerunRequest{RunID: runID, Actor: "alice"}, run, pr, opt)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "刚刚被重跑过") {
		t.Fatalf("错误 = %v，期望说明刚刚被重跑过", err)
	}
	if r := e.run(runID); r.CurrentRerun != 1 {
		t.Fatalf("被拒绝的重跑不应改变 run：current_rerun=%d", r.CurrentRerun)
	}
}

// TestSubmissionTime：排队顺序的提交时间（jobs.created_at，cistore.QueueOrderSQL）——一次规划的全部 job 相同，一次重跑的
// 全部 job 相同且晚于上一次，job 状态变化不改变它（PLAN #14.2）。
func TestSubmissionTime(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	runID, prev := chainRun(e)
	submitted := func(jobs map[string]*cistore.Job) time.Time {
		t.Helper()
		var at time.Time
		for k, j := range jobs {
			if at.IsZero() {
				at = j.CreatedAt
			}
			if !j.CreatedAt.Equal(at) {
				t.Fatalf("同一次提交的 job 的提交时间不同：%s %v，其他 %v", k, j.CreatedAt, at)
			}
		}
		return at
	}
	first := submitted(prev)
	// 结束 run 之后 job 都改变过状态，提交时间仍是规划时的
	var planned time.Time
	if err := e.db.QueryRow(e.ctx, `SELECT created_at FROM runs WHERE id=$1`, runID).Scan(&planned); err != nil || !planned.Equal(first) {
		t.Fatalf("job 的提交时间 %v 应等于规划时间 %v（%v）", first, planned, err)
	}
	if _, err := e.p.Rerun(e.ctx, RerunRequest{RunID: runID, Mode: store.RerunAll, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	if again := submitted(e.jobs(runID, 1)); !again.After(first) {
		t.Fatalf("重跑的提交时间 %v 应晚于上一次 %v", again, first)
	}
}

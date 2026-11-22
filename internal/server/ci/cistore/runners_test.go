package cistore

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestListCIRunners(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	mustExec(t, db, `INSERT INTO runners (id, name, kind, public_key, version, state, drain, last_seen_at) VALUES
		(1, 'mac-mini', 'ci', '\x00', 'v0.1.0', 'active', false, '2026-10-01T00:00:00Z'),
		(2, 'builder', 'ci', '\x00', 'v0.2.0', 'revoked', true, NULL),
		(3, 'reviewer', 'ai', '\x00', '', 'active', false, NULL)`)
	mustExec(t, db, `INSERT INTO ci_runners (runner_id, pool, platforms, executors, capabilities, capacity) VALUES
		(1, 'default', '{darwin/arm64}', '{host,tart}', '{"host_fingerprint":{"os":"darwin","arch":"arm64"}}', '{"max_jobs":3}'),
		(2, 'linux', '{linux/amd64,linux/arm64}', '{oci}', '{}', NULL)`)

	t.Run("字段完整读出", func(t *testing.T) {
		t.Parallel()
		got, err := ListCIRunners(t.Context(), db, store.RunnerFilter{ID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("ListCIRunners = %d 台，应为 1 台", len(got))
		}
		seen := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got[0].LastSeen = &seen
		want := &CIRunner{
			ID: 1, Name: "mac-mini", Kind: store.RunnerKindCI, Version: "v0.1.0", State: store.RunnerActive, LastSeen: &seen,
			Pool: "default", Platforms: []string{"darwin/arm64"}, Executors: []string{"host", "tart"},
			HostFingerprint: map[string]string{"os": "darwin", "arch": "arm64"},
			Capacity:        &store.RunnerCapacity{MaxJobs: 3},
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Errorf("ListCIRunners = %+v，应为 %+v", got[0], want)
		}
	})

	t.Run("没有指纹和容量", func(t *testing.T) {
		t.Parallel()
		got, err := ListCIRunners(t.Context(), db, store.RunnerFilter{Name: "builder"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostFingerprint != nil || got[0].Capacity != nil || got[0].Pool != "linux" {
			t.Fatalf("ListCIRunners = %+v", got)
		}
	})

	t.Run("只列出 CI Runner，忽略 Kind 条件", func(t *testing.T) {
		t.Parallel()
		got, err := ListCIRunners(t.Context(), db, store.RunnerFilter{Kind: store.RunnerKindAI})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range got {
			names = append(names, r.Name)
		}
		if want := []string{"builder", "mac-mini"}; !slices.Equal(names, want) {
			t.Fatalf("ListCIRunners = %v，应为 %v", names, want)
		}
	})
}

func TestRunningJobs(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	named := insertRun(t, db, runFixture{repoID: 10, workflow: "ci.yml", name: "CI"})
	unnamed := insertRun(t, db, runFixture{repoID: 10, workflow: "release.yml"})
	r1, r2, r3 := insertRunner(t, db, "r1"), insertRunner(t, db, "r2"), insertRunner(t, db, "r3")

	// r1 上两个正在执行的 job：较早开始的排在前面
	late := insertJob(t, db, jobFixture{runID: &named, key: "late", name: "test", state: store.JobRunning, started: new("now() - interval '1 minute'")})
	startAttempt(t, db, late, r1, "1 minute")
	early := insertJob(t, db, jobFixture{runID: &unnamed, key: "early", name: "build", state: store.JobRunning})
	startAttempt(t, db, early, r1, "10 minutes") // jobs.started_at 为空时用 attempt 的开始时间
	// 已经结束的 job 不算
	done := insertJob(t, db, jobFixture{runID: &named, key: "done", state: store.JobSucceeded})
	startAttempt(t, db, done, r1, "20 minutes")
	// 第一次 attempt 在 r1 上、重新领取后在 r2 上执行：只属于 r2
	moved := insertJob(t, db, jobFixture{runID: &named, key: "moved", name: "lint", state: store.JobRunning})
	startAttempt(t, db, moved, r1, "30 minutes")
	startAttempt(t, db, moved, r2, "2 minutes")
	// 排队中的 job 没有当前 attempt
	insertJob(t, db, jobFixture{runID: &named, key: "queued", state: store.JobPending})

	cases := []struct {
		name    string
		runners []int64
		want    []int64
	}{
		{"一台 Runner", []int64{r1}, []int64{early, late}},
		{"重新领取后只属于新的 Runner", []int64{r2}, []int64{moved}},
		{"多台 Runner 按开始时间合并排序", []int64{r1, r2}, []int64{early, moved, late}},
		{"空闲的 Runner", []int64{r3}, nil},
		{"空列表", []int64{}, nil},
		{"nil", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := RunningJobs(t.Context(), db, c.runners)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, j := range got {
				ids = append(ids, j.JobID)
			}
			if !slices.Equal(ids, c.want) {
				t.Fatalf("RunningJobs(%v) = %v，应为 %v", c.runners, ids, c.want)
			}
		})
	}

	t.Run("字段", func(t *testing.T) {
		t.Parallel()
		got, err := RunningJobs(t.Context(), db, []int64{r1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("RunningJobs = %+v", got)
		}
		e, l := got[0], got[1]
		// workflow 没有 name 时用文件名
		if e.RunnerID != r1 || e.Name != "build" || e.RunID != unnamed || e.Workflow != "release.yml" || e.Repo != "alice/app" {
			t.Errorf("early = %+v", e)
		}
		if l.Name != "test" || l.RunID != named || l.Workflow != "CI" {
			t.Errorf("late = %+v", l)
		}
		if d := time.Since(e.Started); d < 9*time.Minute || d > 11*time.Minute {
			t.Errorf("jobs.started_at 为空时应取 attempt 的开始时间（约 10 分钟前），实际 %v 前", d)
		}
	})
}

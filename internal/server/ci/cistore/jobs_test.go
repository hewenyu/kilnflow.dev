package cistore

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestJobReusable(t *testing.T) {
	manifest := "sha256:abc"
	for _, c := range []struct {
		name     string
		job      Job
		purged   bool
		expected bool
	}{
		{"succeeded with result", Job{State: store.JobSucceeded, ResultManifest: &manifest}, false, true},
		{"succeeded publish", Job{State: store.JobSucceeded, Kind: workflow.JobKindPublish}, false, true},
		{"succeeded without result", Job{State: store.JobSucceeded}, false, false},
		{"failed with result", Job{State: store.JobFailed, ResultManifest: &manifest}, false, false},
		{"skipped publish", Job{State: store.JobSkipped, Kind: workflow.JobKindPublish}, false, false},
		// 过程数据已经清理：产物不在了，只有发布 job 沿用（不再次发布）
		{"purged with result", Job{State: store.JobSucceeded, ResultManifest: &manifest}, true, false},
		{"purged publish", Job{State: store.JobSucceeded, Kind: workflow.JobKindPublish}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.job.Reusable(c.purged); got != c.expected {
				t.Errorf("Reusable = %v，应为 %v", got, c.expected)
			}
		})
	}
}

func TestGetJob(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	runID := insertRun(t, db, runFixture{repoID: 10})
	spec := workflow.Spec{
		Workflow: "ci.yml", JobID: "build", Platform: "linux/arm64", Executor: workflow.ExecutorOCI,
		Container: "golang:1.27", Inputs: []string{"**/*.go"}, Env: map[string]string{"CGO_ENABLED": "0"},
		Steps:    []workflow.StepSpec{{Name: "test", Run: "go test ./...", Shell: "bash"}},
		Services: []workflow.ServiceSpec{{Name: "db", Image: "postgres:18"}}, Secrets: []string{"TOKEN"},
		TimeoutMinutes: 30, FailFast: true,
	}
	matrix := map[string]any{"go": "1.27", "os": map[string]any{"name": "linux", "n": float64(2)}}
	full := insertJob(t, db, jobFixture{runID: &runID, key: "build (1.27)", state: store.JobRunning, matrix: store.MustJSON(matrix), spec: store.MustJSON(spec)})
	plain := insertJob(t, db, jobFixture{runID: &runID, key: "lint", name: "lint"})
	// 没有 run 的 job（run_id 为 NULL）
	orphan := insertJob(t, db, jobFixture{key: "gc", name: "gc"})
	badSpec := insertJob(t, db, jobFixture{runID: &runID, key: "bad-spec", spec: []byte(`[1]`)})
	badMatrix := insertJob(t, db, jobFixture{runID: &runID, key: "bad-matrix", matrix: []byte(`"x"`)})

	t.Run("JSON 列往返", func(t *testing.T) {
		t.Parallel()
		j, err := GetJob(t.Context(), db, full, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(j.Spec, spec) {
			t.Errorf("Spec = %+v\n应为 %+v", j.Spec, spec)
		}
		if !reflect.DeepEqual(j.Matrix, matrix) {
			t.Errorf("Matrix = %v，应为 %v", j.Matrix, matrix)
		}
		if j.ID != full || j.RunID != runID || j.JobKey != "build (1.27)" || j.JobName != "build" || j.State != store.JobRunning ||
			j.Kind != workflow.JobKindNormal || j.Executor != workflow.ExecutorHost || j.Needs == nil || len(j.Needs) != 0 {
			t.Errorf("GetJob = %+v", j)
		}
	})

	t.Run("可空列", func(t *testing.T) {
		t.Parallel()
		j, err := GetJob(t.Context(), db, plain, false)
		if err != nil {
			t.Fatal(err)
		}
		if j.Matrix != nil || j.ActionKey != nil || j.Environment != nil || j.CurrentAttemptID != nil || j.StartedAt != nil || j.ResultManifest != nil {
			t.Errorf("NULL 列应读为 nil：%+v", j)
		}
	})

	t.Run("没有 run 的 job", func(t *testing.T) {
		t.Parallel()
		j, err := GetJob(t.Context(), db, orphan, false)
		if err != nil {
			t.Fatal(err)
		}
		if j.RunID != 0 {
			t.Errorf("run_id 为 NULL 时 RunID 应为 0，实际 %d", j.RunID)
		}
	})

	t.Run("不存在", func(t *testing.T) {
		t.Parallel()
		_, err := GetJob(t.Context(), db, 999999, false)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("不存在的 job 应返回 pgx.ErrNoRows：%v", err)
		}
	})

	for _, c := range []struct {
		name string
		id   int64
		want string
	}{
		{"spec 无法解析", badSpec, "spec 无法解析"},
		{"matrix 无法解析", badMatrix, "matrix 无法解析"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := GetJob(t.Context(), db, c.id, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应包含 %q：%v", c.want, err)
			}
		})
	}
}

// lock 为 true 时在事务中锁住这一行，直到事务结束；为 false 时不加锁。
func TestGetJob_Lock(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	id := insertJob(t, db, jobFixture{})
	for _, lock := range []bool{false, true} {
		inTx(t, db, func(tx pgx.Tx) {
			if _, err := GetJob(t.Context(), tx, id, lock); err != nil {
				t.Fatalf("GetJob(lock=%v)：%v", lock, err)
			}
			assertRowLock(t, db, "jobs", id, lock)
		})
	}
	assertRowLock(t, db, "jobs", id, false)
}

func TestQueryJobs(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	a := insertJob(t, db, jobFixture{key: "a", state: store.JobPending})
	insertJob(t, db, jobFixture{key: "b", state: store.JobRunning})
	c := insertJob(t, db, jobFixture{key: "c", state: store.JobPending})

	got, err := QueryJobs(t.Context(), db, `state='pending' AND id >= $1 ORDER BY id DESC`, a)
	if err != nil {
		t.Fatal(err)
	}
	if ids := jobIDs(got); !slices.Equal(ids, []int64{c, a}) {
		t.Errorf("QueryJobs = %v，应为 %v", ids, []int64{c, a})
	}
	if got, err := QueryJobs(t.Context(), db, `false`); err != nil || len(got) != 0 {
		t.Errorf("没有结果时应返回空列表：%v, %v", got, err)
	}
	if _, err := QueryJobs(t.Context(), db, `no_such_column=1`); err == nil {
		t.Error("错误的条件应返回错误")
	}
}

func TestRerunJobsAndJobsNamed(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	run, other := insertRun(t, db, runFixture{repoID: 10}), insertRun(t, db, runFixture{repoID: 10})
	// 第 0 次运行：矩阵 build 的两个变体和 test；第 1 次重跑：build 一个变体
	b1 := insertJob(t, db, jobFixture{runID: &run, key: "build (1)", name: "build"})
	t0 := insertJob(t, db, jobFixture{runID: &run, key: "test", name: "test"})
	b2 := insertJob(t, db, jobFixture{runID: &run, key: "build (2)", name: "build"})
	r1 := insertJob(t, db, jobFixture{runID: &run, rerun: 1, key: "build (1)", name: "build"})
	insertJob(t, db, jobFixture{runID: &other, key: "build (1)", name: "build"})

	t.Run("RerunJobs", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name  string
			rerun int
			want  []int64
		}{
			{"第 0 次运行按 ID 排序", 0, []int64{b1, t0, b2}},
			{"重跑只含这一次的 job", 1, []int64{r1}},
			{"没有这一次运行", 2, nil},
		} {
			got, err := RerunJobs(t.Context(), db, run, c.rerun, false)
			if err != nil {
				t.Fatal(err)
			}
			if ids := jobIDs(got); !slices.Equal(ids, c.want) {
				t.Errorf("%s：RerunJobs = %v，应为 %v", c.name, ids, c.want)
			}
		}
	})

	t.Run("RerunJobs 加锁", func(t *testing.T) {
		t.Parallel()
		inTx(t, db, func(tx pgx.Tx) {
			got, err := RerunJobs(t.Context(), tx, run, 0, true)
			if err != nil {
				t.Fatalf("lock=true 时 SQL 应合法：%v", err)
			}
			if ids := jobIDs(got); !slices.Equal(ids, []int64{b1, t0, b2}) {
				t.Fatalf("RerunJobs = %v", ids)
			}
			for _, id := range []int64{b1, t0, b2} {
				assertRowLock(t, db, "jobs", id, true)
			}
			// 其他运行的 job 不受影响
			assertRowLock(t, db, "jobs", r1, false)
		})
	})

	t.Run("JobsNamed", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			name, job string
			rerun     int
			want      []int64
		}{
			{"矩阵的全部变体", "build", 0, []int64{b1, b2}},
			{"单个 job", "test", 0, []int64{t0}},
			{"重跑中的变体", "build", 1, []int64{r1}},
			{"不存在的名称", "deploy", 0, nil},
			{"名称区分大小写", "Build", 0, nil},
		} {
			got, err := JobsNamed(t.Context(), db, run, c.rerun, c.job)
			if err != nil {
				t.Fatal(err)
			}
			if ids := jobIDs(got); !slices.Equal(ids, c.want) {
				t.Errorf("%s：JobsNamed = %v，应为 %v", c.name, ids, c.want)
			}
		}
	})
}

func jobIDs(jobs []*Job) []int64 {
	var ids []int64
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	return ids
}

package cistore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestGetRun(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	gctx := GitHubCtx{
		EventName: "pull_request", SHA: "abc123", Ref: "refs/pull/7/head", RefName: "7/head", RefType: "branch",
		Repository: "alice/app", RepositoryOwner: "alice", HeadRef: "feature", BaseRef: "main", Actor: "alice",
	}
	inputs := map[string]string{"level": "debug", "dry_run": "true"}
	var full int64
	err := db.QueryRow(t.Context(), `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number,
		sender_login, sender_is_bot, current_rerun, concurrency_key, cancel_in_progress, state, config_error, warnings, github_ctx, inputs, commit_time, priority, finished_at)
		VALUES (10, 'ci.yml', 'CI', 'pull_request', 'refs/pull/7/head', 'abc123', 'webhook', 'd1', 7, 'alice', true, 2, 'ci-7', true, 'failed',
		'配置错误', '警告', $1, $2, 1730000000, 3, now()) RETURNING id`, store.MustJSON(gctx), store.MustJSON(inputs)).Scan(&full)
	if err != nil {
		t.Fatal(err)
	}
	minimal := insertRun(t, db, runFixture{repoID: 10})
	bad := insertRun(t, db, runFixture{repoID: 10})
	mustExec(t, db, `UPDATE runs SET github_ctx='[]' WHERE id=$1`, bad)
	badInputs := insertRun(t, db, runFixture{repoID: 10})
	mustExec(t, db, `UPDATE runs SET inputs='{"n": 1}' WHERE id=$1`, badInputs)

	t.Run("JSON 列和全部字段往返", func(t *testing.T) {
		t.Parallel()
		r, err := GetRun(t.Context(), db, full, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.CreatedAt.IsZero() || r.FinishedAt == nil {
			t.Errorf("CreatedAt = %v，FinishedAt = %v", r.CreatedAt, r.FinishedAt)
		}
		got := *r
		got.CreatedAt, got.FinishedAt = time.Time{}, nil
		want := Run{
			ID: full, RepoID: 10, Workflow: "ci.yml", WorkflowName: "CI", Event: "pull_request", Ref: "refs/pull/7/head", SHA: "abc123",
			Source: "webhook", PRNumber: new(7), SenderLogin: "alice", SenderIsBot: true, CurrentRerun: 2, ConcurrencyKey: new("ci-7"),
			CancelInProgress: true, State: store.RunFailed, ConfigError: new("配置错误"), Warnings: new("警告"), GitHubCtx: gctx, Inputs: inputs,
			CommitTime: 1730000000, Priority: 3,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetRun = %+v\n应为 %+v", got, want)
		}
	})

	t.Run("默认值", func(t *testing.T) {
		t.Parallel()
		r, err := GetRun(t.Context(), db, minimal, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.PRNumber != nil || r.ConcurrencyKey != nil || r.FinishedAt != nil || r.GitHubCtx != (GitHubCtx{}) || r.Inputs == nil || len(r.Inputs) != 0 {
			t.Errorf("GetRun = %+v", r)
		}
	})

	t.Run("不存在", func(t *testing.T) {
		t.Parallel()
		if _, err := GetRun(t.Context(), db, 999999, false); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("不存在的 run 应返回 pgx.ErrNoRows：%v", err)
		}
	})

	for _, c := range []struct {
		name string
		id   int64
		want string
	}{
		{"github_ctx 无法解析", bad, "github_ctx 无法解析"},
		{"inputs 无法解析", badInputs, "inputs 无法解析"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := GetRun(t.Context(), db, c.id, false); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应包含 %q：%v", c.want, err)
			}
		})
	}
}

func TestGetRun_Lock(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	id := insertRun(t, db, runFixture{repoID: 10})
	for _, lock := range []bool{false, true} {
		inTx(t, db, func(tx pgx.Tx) {
			if _, err := GetRun(t.Context(), tx, id, lock); err != nil {
				t.Fatalf("GetRun(lock=%v)：%v", lock, err)
			}
			assertRowLock(t, db, "runs", id, lock)
		})
	}
}

func TestQueryRuns(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRepo(t, db, 20, 1, 100, "alice", "Website")
	insertRepo(t, db, 30, 2, 300, "bob", "tools")
	// 按 ID 递增写入，ID 越大越新
	ids := []int64{
		insertRun(t, db, runFixture{repoID: 10, workflow: "ci.yml", name: "CI", ref: "refs/heads/main", state: store.RunSucceeded}),         // 0
		insertRun(t, db, runFixture{repoID: 10, workflow: "release.yaml", name: "Release", ref: "refs/tags/v1", state: store.RunFailed}),    // 1
		insertRun(t, db, runFixture{repoID: 20, workflow: "ci.yml", ref: "refs/heads/feature", state: store.RunActive}),                     // 2
		insertRun(t, db, runFixture{repoID: 30, workflow: "lint", name: "ci", ref: "refs/pull/3/head", state: store.RunQueued}),             // 3
		insertRun(t, db, runFixture{repoID: 10, workflow: "ci.yml", name: "CI", ref: "refs/heads/main", state: store.RunActive}),            // 4
		insertRun(t, db, runFixture{repoID: 20, workflow: "nightly.yml", name: "Nightly", ref: "refs/heads/main", state: store.RunSkipped}), // 5
	}
	mustExec(t, db, `UPDATE runs SET sender_login='Carol' WHERE id = ANY($1)`, []int64{ids[1], ids[3]})
	// pick 按下标取 run 的 ID，顺序与参数相同
	pick := func(idx ...int) []int64 {
		out := make([]int64, len(idx))
		for i, x := range idx {
			out[i] = ids[x]
		}
		return out
	}
	cases := []struct {
		name string
		f    RunFilter
		want []int64
	}{
		{"全部按 ID 倒序", RunFilter{Limit: 10}, pick(5, 4, 3, 2, 1, 0)},
		{"Limit", RunFilter{Limit: 2}, pick(5, 4)},
		{"按仓库 ID", RunFilter{RepoID: 10, Limit: 10}, pick(4, 1, 0)},
		{"仓库名包含（不区分大小写）", RunFilter{RepoLike: "WEB", Limit: 10}, pick(5, 2)},
		{"仓库名可以匹配 owner/ 前缀", RunFilter{RepoLike: "bob/", Limit: 10}, pick(3)},
		{"按状态", RunFilter{State: store.RunActive, Limit: 10}, pick(4, 2)},
		{"workflow 文件名", RunFilter{Workflow: "ci.yml", Limit: 10}, pick(4, 2, 0)},
		{"workflow 省略 .yml，同时匹配 name", RunFilter{Workflow: "ci", Limit: 10}, pick(4, 3, 2, 0)},
		{"workflow 省略 .yaml", RunFilter{Workflow: "release", Limit: 10}, pick(1)},
		{"workflow 的 name", RunFilter{Workflow: "Nightly", Limit: 10}, pick(5)},
		{"workflow 的 name 区分大小写", RunFilter{Workflow: "nightly", Limit: 10}, pick(5)},
		{"workflow 不匹配部分名称", RunFilter{Workflow: "c", Limit: 10}, nil},
		{"完整 ref", RunFilter{Ref: "refs/heads/main", Limit: 10}, pick(5, 4, 0)},
		{"分支名", RunFilter{Ref: "main", Limit: 10}, pick(5, 4, 0)},
		{"tag 名", RunFilter{Ref: "v1", Limit: 10}, pick(1)},
		{"不匹配 ref 的一部分", RunFilter{Ref: "heads/main", Limit: 10}, nil},
		{"组合条件", RunFilter{RepoID: 10, Workflow: "ci", State: store.RunActive, Ref: "main", Limit: 10}, pick(4)},
		{"发起人（不区分大小写）", RunFilter{Actor: "carol", Limit: 10}, pick(3, 1)},
		{"发起人与仓库组合", RunFilter{Actor: "carol", RepoID: 10, Limit: 10}, pick(1)},
		{"Before：更早的一页", RunFilter{Before: ids[4], Limit: 2}, pick(3, 2)},
		{"Before 最旧的 run 之后没有了", RunFilter{Before: ids[0], Limit: 2}, nil},
		{"After：紧挨着游标的较新的一页，仍按倒序", RunFilter{After: ids[1], Limit: 2}, pick(3, 2)},
		{"After 不足一页", RunFilter{After: ids[3], Limit: 5}, pick(5, 4)},
		{"After 最新的 run 之后没有了", RunFilter{After: ids[5], Limit: 2}, nil},
		{"Before 和 After 同时设置时按 Before", RunFilter{Before: ids[2], After: ids[0], Limit: 10}, pick(1, 0)},
		{"游标与筛选条件组合", RunFilter{RepoID: 10, After: ids[0], Limit: 1}, pick(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runs, names, err := QueryRuns(t.Context(), db, &c.f)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for i, r := range runs {
				got = append(got, r.ID)
				p, err := store.GetRepo(t.Context(), db, r.RepoID)
				if err != nil {
					t.Fatal(err)
				}
				if names[i] != p.FullName() {
					t.Errorf("run %d 的仓库名为 %q，应为 %q", r.ID, names[i], p.FullName())
				}
			}
			if len(names) != len(runs) {
				t.Errorf("仓库名 %d 个，run %d 个", len(names), len(runs))
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("QueryRuns(%+v) = %v，应为 %v", c.f, got, c.want)
			}
		})
	}

	t.Run("ListRuns", func(t *testing.T) {
		t.Parallel()
		runs, names, err := ListRuns(t.Context(), db, 20, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 || runs[0].ID != ids[5] || runs[1].ID != ids[2] || !slices.Equal(names, []string{"alice/Website", "alice/Website"}) {
			t.Fatalf("ListRuns = %v %v", runs, names)
		}
		all, _, err := ListRuns(t.Context(), db, 0, 3)
		if err != nil || len(all) != 3 || all[0].ID != ids[5] {
			t.Fatalf("repoID 为 0 时列出全部仓库：%v %v", all, err)
		}
	})
}

// 用游标从头翻到尾再翻回来，每一页都不重叠、不遗漏。
func TestQueryRuns_PagingRoundTrip(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	var all []int64
	for range 7 {
		all = append(all, insertRun(t, db, runFixture{repoID: 10}))
	}
	slices.Reverse(all)
	page := func(f RunFilter) []int64 {
		f.Limit = 3
		runs, _, err := QueryRuns(t.Context(), db, &f)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		return ids
	}
	var forward [][]int64
	for p := page(RunFilter{}); len(p) > 0; p = page(RunFilter{Before: p[len(p)-1]}) {
		forward = append(forward, p)
	}
	if want := [][]int64{all[0:3], all[3:6], all[6:7]}; !reflect.DeepEqual(forward, want) {
		t.Fatalf("向后翻页 = %v，应为 %v", forward, want)
	}
	// 从最后一页向前翻：After 取紧挨着的较新的 3 个
	if got := page(RunFilter{After: all[6]}); !slices.Equal(got, all[3:6]) {
		t.Fatalf("向前翻页 = %v，应为 %v", got, all[3:6])
	}
	if got := page(RunFilter{After: all[3]}); !slices.Equal(got, all[0:3]) {
		t.Fatalf("向前翻页 = %v，应为 %v", got, all[0:3])
	}
}

func TestRunWorkflows(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRepo(t, db, 20, 1, 100, "alice", "web")
	for _, f := range []runFixture{
		{repoID: 10, workflow: "release.yml"},
		{repoID: 10, workflow: "ci.yml"},
		{repoID: 10, workflow: "ci.yml"},
		{repoID: 20, workflow: "deploy.yml"},
		{repoID: 20, workflow: "ci.yml"},
	} {
		insertRun(t, db, f)
	}
	for _, c := range []struct {
		name  string
		repo  int64
		limit int
		want  []string
	}{
		{"全部仓库，去重并排序", 0, 10, []string{"ci.yml", "deploy.yml", "release.yml"}},
		{"limit", 0, 2, []string{"ci.yml", "deploy.yml"}},
		{"一个仓库", 10, 10, []string{"ci.yml", "release.yml"}},
		{"没有 run 的仓库", 99, 10, []string{}},
	} {
		got, err := RunWorkflows(t.Context(), db, c.repo, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s：RunWorkflows = %v，应为 %v", c.name, got, c.want)
		}
	}
}

func TestPrefixCols(t *testing.T) {
	t.Parallel()
	if got := prefixCols("id, name,\n\tstate", "r."); got != "r.id, r.name, r.state" {
		t.Fatalf("prefixCols = %q", got)
	}
}

// 并发组的锁在事务结束前一直持有：同一组的另一个事务拿不到，其他组、其他仓库不受影响。
func TestLockConcurrencyGroup(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	// tryLock 在另一个事务中以 lock_timeout 尝试锁住并发组，返回是否成功
	tryLock := func(t *testing.T, repoID int64, key string) bool {
		t.Helper()
		err := db.Tx(t.Context(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), `SET LOCAL lock_timeout = '20ms'`); err != nil {
				return err
			}
			return LockConcurrencyGroup(t.Context(), tx, repoID, key)
		})
		if err != nil && pgCode(err) != lockNotAvailable {
			t.Fatal(err)
		}
		return err == nil
	}

	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := LockConcurrencyGroup(t.Context(), tx, 10, "deploy"); err != nil {
		t.Fatal(err)
	}
	// 同一事务中可以重复加锁
	if err := LockConcurrencyGroup(t.Context(), tx, 10, "deploy"); err != nil {
		t.Fatalf("同一事务重复加锁：%v", err)
	}
	for _, c := range []struct {
		name string
		repo int64
		key  string
		want bool
	}{
		{"同一组被锁住", 10, "deploy", false},
		{"同一仓库的其他组", 10, "ci", true},
		{"其他仓库的同名组", 20, "deploy", true},
		{"键的拼接不会混淆", 1, "0:deploy", true},
	} {
		if got := tryLock(t, c.repo, c.key); got != c.want {
			t.Errorf("%s：另一个事务加锁成功 = %v，应为 %v", c.name, got, c.want)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !tryLock(t, 10, "deploy") {
		t.Error("事务结束后锁应释放")
	}
}

func TestCountActiveInGroup(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRepo(t, db, 20, 1, 100, "alice", "web")
	a1 := insertRun(t, db, runFixture{repoID: 10, state: store.RunActive, concurrencyKey: new("g")})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunActive, concurrencyKey: new("g")})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunQueued, concurrencyKey: new("g")})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunSucceeded, concurrencyKey: new("g")})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunActive, concurrencyKey: new("other")})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunActive})
	insertRun(t, db, runFixture{repoID: 20, state: store.RunActive, concurrencyKey: new("g")})
	for _, c := range []struct {
		name    string
		repo    int64
		key     string
		exclude int64
		want    int
	}{
		{"只数 active 的", 10, "g", 0, 2},
		{"排除自己", 10, "g", a1, 1},
		{"其他仓库", 20, "g", 0, 1},
		{"没有 run 的组", 10, "none", 0, 0},
	} {
		got, err := CountActiveInGroup(t.Context(), db, c.repo, c.key, c.exclude)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s：CountActiveInGroup = %d，应为 %d", c.name, got, c.want)
		}
	}
}

func TestRunJobStates(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	a := insertRun(t, db, runFixture{repoID: 10})
	b := insertRun(t, db, runFixture{repoID: 10})
	empty := insertRun(t, db, runFixture{repoID: 10})
	insertJob(t, db, jobFixture{runID: &a, key: "lint", state: store.JobSucceeded})
	insertJob(t, db, jobFixture{runID: &a, key: "test", state: store.JobRunning})
	// b 重跑过一次：只看最新一次运行的 job
	insertJob(t, db, jobFixture{runID: &b, key: "lint", state: store.JobFailed})
	insertJob(t, db, jobFixture{runID: &b, rerun: 1, key: "lint", state: store.JobPending})
	mustExec(t, db, `UPDATE runs SET current_rerun=1 WHERE id=$1`, b)

	got, err := RunJobStates(t.Context(), db, []int64{a, b, empty})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got[a], []string{"succeeded", "running"}) || !slices.Equal(got[b], []string{"pending"}) {
		t.Errorf("RunJobStates = %v", got)
	}
	if _, ok := got[empty]; ok || len(got) != 2 {
		t.Errorf("没有 job 的 run 不应出现：%v", got)
	}
	if got, err := RunJobStates(t.Context(), db, nil); err != nil || len(got) != 0 {
		t.Errorf("空列表：%v %v", got, err)
	}
}

func TestRunCounts(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRun(t, db, runFixture{repoID: 10, state: store.RunSucceeded})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunSucceeded})
	insertRun(t, db, runFixture{repoID: 10, state: store.RunFailed})
	old := insertRun(t, db, runFixture{repoID: 10, state: store.RunFailed})
	mustExec(t, db, `UPDATE runs SET created_at=now() - interval '2 days' WHERE id=$1`, old)

	got, err := RunCounts(t.Context(), db, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[store.RunState]int{store.RunSucceeded: 2, store.RunFailed: 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("RunCounts = %v，期望 %v", got, want)
	}
}

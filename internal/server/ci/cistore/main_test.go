package cistore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 本文件是各测试共用的数据构造辅助：直接用 SQL 写入最少的必填列，被测的是 cistore 的读取和更新。

// newDB 返回一个独立的、已执行迁移的数据库。
func newDB(t *testing.T) *store.DB {
	t.Helper()
	return store.New(pgtest.Pool(t))
}

// mustExec 执行一条写语句，失败时终止测试。
func mustExec(t *testing.T, q store.Execer, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.WithoutCancel(t.Context()), sql, args...); err != nil {
		t.Fatalf("执行 %q：%v", sql, err)
	}
}

// insertRepo 写入一个启用中、信息完整的仓库。
func insertRepo(t *testing.T, db *store.DB, id, instID, ownerID int64, owner, name string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO repos (id, installation_id, owner_id, owner, name, private, default_branch, clone_url)
		VALUES ($1,$2,$3,$4,$5,true,'main','https://example.com/'||$4||'/'||$5||'.git')`, id, instID, ownerID, owner, name)
}

// runFixture 是 insertRun 的参数；零值的字段取默认值。
type runFixture struct {
	repoID         int64
	workflow, name string
	ref            string
	state          store.RunState
	concurrencyKey *string
}

// insertRun 写入一个 run，返回它的 ID。
func insertRun(t *testing.T, db *store.DB, f runFixture) int64 {
	t.Helper()
	if f.workflow == "" {
		f.workflow = "ci.yml"
	}
	if f.ref == "" {
		f.ref = "refs/heads/main"
	}
	if f.state == "" {
		f.state = store.RunActive
	}
	var id int64
	err := db.QueryRow(t.Context(), `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, state, concurrency_key)
		VALUES ($1,$2,$3,'push',$4,'abc123','webhook',gen_random_uuid()::text,$5,$6) RETURNING id`,
		f.repoID, f.workflow, f.name, f.ref, string(f.state), f.concurrencyKey).Scan(&id)
	if err != nil {
		t.Fatalf("写入 run：%v", err)
	}
	return id
}

// jobFixture 是 insertJob 的参数；零值的字段取默认值。
type jobFixture struct {
	runID   *int64
	rerun   int
	key     string
	name    string
	state   store.JobState
	kind    workflow.JobKind
	matrix  []byte
	spec    []byte
	started *string // started_at 的 SQL 表达式，例如 now() - interval '1 minute'
}

// insertJob 写入一个 job，返回它的 ID。
func insertJob(t *testing.T, db *store.DB, f jobFixture) int64 {
	t.Helper()
	if f.name == "" {
		f.name = "build"
	}
	if f.key == "" {
		f.key = f.name
	}
	if f.state == "" {
		f.state = store.JobPending
	}
	if f.kind == "" {
		f.kind = workflow.JobKindNormal
	}
	if f.spec == nil {
		f.spec = []byte(`{"workflow":"ci.yml","job_id":"build"}`)
	}
	started := "NULL"
	if f.started != nil {
		started = *f.started
	}
	var id int64
	err := db.QueryRow(t.Context(), `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state, started_at)
		VALUES ($1,$2,$3,$4,$5,$5,$6,$7,'linux/arm64','host',$8,`+started+`) RETURNING id`,
		string(f.kind), f.runID, f.rerun, f.key, f.name, f.matrix, f.spec, string(f.state)).Scan(&id)
	if err != nil {
		t.Fatalf("写入 job：%v", err)
	}
	return id
}

// insertRunner 写入一台 CI Runner（runners 和 ci_runners），返回它的 ID。
func insertRunner(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(t.Context(), `INSERT INTO runners (name, public_key) VALUES ($1, '\x00') RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("写入 Runner：%v", err)
	}
	mustExec(t, db, `INSERT INTO ci_runners (runner_id) VALUES ($1)`, id)
	return id
}

// startAttempt 为 job 写入一次 attempt 并设为当前 attempt，返回 attempt 的 ID。
// startedAgo 是 attempt 开始距今的时长（SQL interval 字面量，例如 '5 minutes'）。
func startAttempt(t *testing.T, db *store.DB, jobID, runnerID int64, startedAgo string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(t.Context(), `INSERT INTO attempts (job_id, epoch, runner_id, acquire_request_id, started_at)
		VALUES ($1, (SELECT epoch+1 FROM jobs WHERE id=$1), $2, gen_random_uuid()::text, now() - $3::interval) RETURNING id`,
		jobID, runnerID, startedAgo).Scan(&id)
	if err != nil {
		t.Fatalf("写入 attempt：%v", err)
	}
	mustExec(t, db, `UPDATE jobs SET current_attempt_id=$2, epoch=epoch+1 WHERE id=$1`, jobID, id)
	return id
}

// pgCode 返回 err 中 PostgreSQL 错误的 SQLSTATE；不是 PostgreSQL 错误时返回空字符串。
func pgCode(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code
	}
	return ""
}

// lockNotAvailable 是 NOWAIT 或 lock_timeout 拿不到锁时的 SQLSTATE。
const lockNotAvailable = "55P03"

// assertRowLock 用另一个连接以 NOWAIT 方式锁同一行，检查这一行当前是否被别的事务锁住。
func assertRowLock(t *testing.T, db *store.DB, table string, id int64, locked bool) {
	t.Helper()
	_, err := db.Exec(t.Context(), `SELECT 1 FROM `+pgx.Identifier{table}.Sanitize()+` WHERE id=$1 FOR UPDATE NOWAIT`, id)
	switch {
	case locked && pgCode(err) != lockNotAvailable:
		t.Fatalf("%s %d 应被行锁锁住，另一个事务加锁的结果为 %v", table, id, err)
	case !locked && err != nil:
		t.Fatalf("%s %d 不应被锁住，另一个事务加锁失败：%v", table, id, err)
	}
}

// inTx 在事务中执行 f，f 返回后回滚（用于在持有锁时做检查）。
func inTx(t *testing.T, db *store.DB, f func(tx pgx.Tx)) {
	t.Helper()
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	f(tx)
}

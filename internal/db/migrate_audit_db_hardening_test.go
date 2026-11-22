package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/db"
)

// auditDBHardeningVersion 是数据库审计加固的迁移（0042_audit_db_hardening.sql）。
const auditDBHardeningVersion = 42

// 0042：三个及以上仅大小写不同的 environment 合并成一行；变量按合并目标去重后挪到保留行。
// 保留环境本来没有该键、两个落败环境都有同名变量，迁移后只保留一个值（落败行里 id 最小的那个）。
func TestMigrate_AuditDBHardening_MergesCaseVariants(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, auditDBHardeningVersion)
	exec(t, u, `
INSERT INTO environments (id, owner_id, repo_id, name) VALUES
  (1, 7, 9, 'prod'), (2, 7, 9, 'Prod'), (3, 7, 9, 'PROD');
INSERT INTO variables (scope_kind, scope_id, name, value) VALUES
  ('environment', 2, 'TOKEN', 'from-2'),
  ('environment', 3, 'TOKEN', 'from-3');
`)
	pool := open(t, u)

	var envs, vars int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM environments`).Scan(&envs); err != nil {
		t.Fatal(err)
	}
	if envs != 1 {
		t.Fatalf("三个大小写变体应合并成 1 行，实际 %d 行", envs)
	}
	if got := queryString(t, pool, `SELECT id::text || ' ' || name FROM environments`); got != "1 prod" {
		t.Errorf("应保留 id 最小的一行，实际 %q", got)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM variables WHERE scope_kind = 'environment'`).Scan(&vars); err != nil {
		t.Fatal(err)
	}
	if vars != 1 {
		t.Fatalf("重名变量应只保留 1 行，实际 %d 行", vars)
	}
	if got := queryString(t, pool, `SELECT scope_id::text || ' ' || name || ' ' || value FROM variables WHERE scope_kind = 'environment'`); got != "1 TOKEN from-2" {
		t.Errorf("变量应挪到保留环境并保留 id 最小的落败行的值，实际 %q", got)
	}

	// 新的唯一索引不区分大小写：再插入一个变体会被拒绝。显式给一个未占用的 id，避免不推进序列时最后一次插入
	// 拿到 id=1 而先撞上 environments_pkey（那样即使唯一索引不存在测试也会通过）。
	_, err := pool.Exec(t.Context(), `INSERT INTO environments (id, owner_id, repo_id, name) VALUES (100, 7, 9, 'pRoD')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("大小写不敏感的唯一索引应拒绝新的变体，实际错误：%v", err)
	}
	if pgErr.Code != "23505" || pgErr.ConstraintName != "environments_scope_name_ci" {
		t.Fatalf("应触发 environments_scope_name_ci 的唯一冲突，实际 code=%s constraint=%s：%v",
			pgErr.Code, pgErr.ConstraintName, err)
	}
}

// 0042：升级前已经进入阶段 B 的发布回填 commit_started_at（用 updated_at），仍在阶段 A 的保持 NULL：
// stage 已是 commit，或 stage 还是 staging 但已经有目标提交成功，都算进入过阶段 B（崩溃在更新 stage 之前）。
func TestMigrate_AuditDBHardening_BackfillsCommitStartedAt(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, auditDBHardeningVersion)
	exec(t, u, `
INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag, stage, updated_at) VALUES
  (1, 1, 1, 'tester/app', 'v1.0.0', 'commit',  '2026-01-01 00:00:00+00'),
  (2, 2, 1, 'tester/app', 'v1.0.1', 'staging', '2026-01-02 00:00:00+00'),
  (3, 3, 1, 'tester/app', 'v1.0.2', 'staging', '2026-01-03 00:00:00+00');
INSERT INTO release_targets (job_id, seq, kind, name, target, state) VALUES
  (2, 0, 'github', 'tester/app', 'tester/app', 'committed'),
  (3, 0, 'github', 'tester/app', 'tester/app', 'staged');
`)
	pool := open(t, u)
	for _, tc := range []struct {
		job  int64
		want bool
	}{
		{1, true},  // stage 已是 commit
		{2, true},  // stage 还是 staging，但已经有目标提交成功
		{3, false}, // 仍在阶段 A
	} {
		var started *time.Time
		var updated time.Time
		if err := pool.QueryRow(t.Context(), `SELECT commit_started_at, updated_at FROM releases WHERE job_id=$1`, tc.job).
			Scan(&started, &updated); err != nil {
			t.Fatal(err)
		}
		if got := started != nil; got != tc.want {
			t.Errorf("job %d：commit_started_at 非空 = %v，期望 %v", tc.job, got, tc.want)
			continue
		}
		if started != nil && !started.Equal(updated) {
			t.Errorf("job %d：应回填 updated_at %v，实际 %v", tc.job, updated, *started)
		}
	}
}

// 0042：落败 environment 上挂有 secret 时明确中止迁移，不静默删 secret 也不挪动（密文 AAD 绑定 scope_id）。
// 迁移整体回滚：环境、变量、secret 保持原样，版本号不记录。
func TestMigrate_AuditDBHardening_AbortsOnSecret(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, auditDBHardeningVersion)
	exec(t, u, `
INSERT INTO environments (id, owner_id, repo_id, name) VALUES
  (1, 7, 9, 'prod'), (2, 7, 9, 'Prod');
INSERT INTO variables (scope_kind, scope_id, name, value) VALUES
  ('environment', 2, 'TOKEN', 'from-2');
INSERT INTO secrets (scope_kind, scope_id, name, ciphertext, wrapped_dek) VALUES
  ('environment', 2, 'API_KEY', '\x0102'::bytea, '\x0304'::bytea);
`)
	pool, err := db.Open(t.Context(), u)
	if err == nil {
		pool.Close()
		t.Fatal("落败 environment 上挂有 secret 时迁移应中止")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("错误信息应说明是 secret 导致中止，实际：%v", err)
	}

	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(t.Context()))
	var envs, secrets, applied int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM environments`).Scan(&envs); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations WHERE version = $1`, auditDBHardeningVersion).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if envs != 2 || secrets != 1 || applied != 0 {
		t.Fatalf("迁移应整体回滚，实际 environments=%d secrets=%d applied=%d", envs, secrets, applied)
	}
}

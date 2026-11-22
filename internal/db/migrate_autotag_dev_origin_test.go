package db_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/migrations"
)

// autoTagDevOriginVersion 是 AI 开发 PR 永久归属的迁移（0043_autotag_dev_origin.sql，PLAN #29.4）。
const autoTagDevOriginVersion = 43

// TestMigrate_AutoTagDevOrigin：在旧的表结构上直接升级；旧的自动打 tag 记录保留并取新列的默认值；新的归属表有
// (repo_id, pr_number) 主键、(work_item_id, phase, impl_round) 唯一约束，阶段和分支的非空检查生效。
func TestMigrate_AutoTagDevOrigin(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, autoTagDevOriginVersion)
	exec(t, u, `INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, source, state, tag)
		VALUES (42, 'aaaa', 7, 'alice', 'webhook', 'tagged', 'v1.0.0')`)
	body, err := migrations.FS.ReadFile("0043_autotag_dev_origin.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, u, string(body))
	exec(t, u, `INSERT INTO schema_migrations (version, name) VALUES ($1, '0043_autotag_dev_origin.sql')`, autoTagDevOriginVersion)
	pool := open(t, u)

	// 升级前的记录保留，新列取默认值（不回填）
	var workItem *int64
	var head, base, state string
	var bot bool
	if err := pool.QueryRow(t.Context(), `SELECT work_item_id, head_ref, base_ref, sender_is_bot, state FROM auto_tags WHERE repo_id=42 AND sha='aaaa'`).
		Scan(&workItem, &head, &base, &bot, &state); err != nil {
		t.Fatal(err)
	}
	if workItem != nil || head != "" || base != "" || bot || state != "tagged" {
		t.Fatalf("旧记录 = work_item %v head %q base %q bot %v state %q", workItem, head, base, bot, state)
	}

	// 新列的默认值与非空约束（写入时不提供也能插入）
	if got := queryString(t, pool, `SELECT count(*)::text FROM information_schema.columns
		WHERE table_name='auto_tags' AND column_name IN ('work_item_id','head_ref','base_ref','sender_is_bot')`); got != "4" {
		t.Fatalf("auto_tags 应新增 4 列，实际 %s", got)
	}
	if got := queryString(t, pool, `SELECT string_agg(column_name || ':' || is_nullable, ',' ORDER BY column_name)
		FROM information_schema.columns WHERE table_name='auto_tags'
		AND column_name IN ('work_item_id','head_ref','base_ref','sender_is_bot')`); got != "base_ref:NO,head_ref:NO,sender_is_bot:NO,work_item_id:YES" {
		t.Fatalf("新列的可空性 = %s", got)
	}

	idx := queryString(t, pool, `SELECT string_agg(indexdef, ' ' ORDER BY indexname) FROM pg_indexes WHERE tablename='dev_pull_origins'`)
	for _, want := range []string{"(repo_id, pr_number)", "(work_item_id, phase, impl_round)", "(work_item_id)"} {
		if !strings.Contains(idx, want) {
			t.Errorf("dev_pull_origins 缺少索引 %s：%s", want, idx)
		}
	}

	// 阶段和分支的检查先于外键生效（父行不存在，所以只验证违反检查时的错误）
	for _, tt := range []struct {
		name, phase, head, constraint string
	}{
		{"未知阶段", "merged", "kiln/dev-1", "dev_pull_origins_phase_check"},
		{"空 head", "implement", "", "dev_pull_origins_head_ref_check"},
	} {
		_, err := pool.Exec(t.Context(), `INSERT INTO dev_pull_origins (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref)
			VALUES (999, 1, 999, $1, 1, $2, 'main')`, tt.phase, tt.head)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != tt.constraint {
			t.Errorf("%s：期望触发 %s，实际 %v", tt.name, tt.constraint, err)
		}
	}

	// 待核验表（dev_pull_origin_checks）：(repo_id, pr_number) 主键，阶段和分支的检查同样生效
	checkIdx := queryString(t, pool, `SELECT string_agg(indexdef, ' ' ORDER BY indexname) FROM pg_indexes WHERE tablename='dev_pull_origin_checks'`)
	if !strings.Contains(checkIdx, "(repo_id, pr_number)") {
		t.Errorf("dev_pull_origin_checks 缺少主键：%s", checkIdx)
	}
	for _, tt := range []struct {
		name, phase, head, constraint string
	}{
		{"未知阶段", "merged", "kiln/dev-1", "dev_pull_origin_checks_phase_check"},
		{"空 head", "implement", "", "dev_pull_origin_checks_head_ref_check"},
	} {
		_, err := pool.Exec(t.Context(), `INSERT INTO dev_pull_origin_checks (repo_id, pr_number, work_item_id, phase, impl_round, head_ref, base_ref)
			VALUES (999, 1, 999, $1, 1, $2, 'main')`, tt.phase, tt.head)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != tt.constraint {
			t.Errorf("%s：期望触发 %s，实际 %v", tt.name, tt.constraint, err)
		}
	}
}

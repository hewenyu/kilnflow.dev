package db_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/db"
	"github.com/hewenyu/kilnflow.dev/migrations"
)

// perfIndexesVersion 是 Dashboard 高频查询的索引迁移（0045_perf_indexes.sql）。
const perfIndexesVersion = 45

// TestMigrate_PerfIndexes：在旧结构上升级后三个索引都存在、定义与查询匹配（部分索引的谓词、表达式列），
// 记录版本之后重复 Migrate 不会再次执行。
func TestMigrate_PerfIndexes(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, perfIndexesVersion)
	exec(t, u, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`)
	exec(t, u, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, sender_login, state) VALUES
		(42, 'ci.yml', 'push', 'refs/heads/main', 'a', 'push', 'k1', 'Alice', 'failed'),
		(42, 'ci.yml', 'push', 'refs/heads/main', 'b', 'push', 'k2', 'bob', 'succeeded'),
		(42, 'release.yml', 'push', 'refs/heads/main', 'c', 'push', 'k3', 'alice', 'failed')`)
	body, err := migrations.FS.ReadFile("0045_perf_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, u, string(body))
	exec(t, u, `INSERT INTO schema_migrations (version, name) VALUES ($1, '0045_perf_indexes.sql')`, perfIndexesVersion)
	pool := open(t, u) // Open 会再执行一次 Migrate：已记录的版本不会重复执行

	defs := queryString(t, pool, `SELECT string_agg(c.relname || ' ' || pg_get_indexdef(i.indexrelid), E'\n' ORDER BY c.relname)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname IN ('runs_failed', 'runs_actor', 'runs_repo_workflow')`)
	for _, want := range []string{
		// 只包含失败的 run，按 id 倒序：总览的最近失败与 /runs?state=failed 直接走它
		"runs_failed",
		"(id DESC)",
		"state = 'failed'",
		// 按 lower(sender_login) 过滤、id 倒序："我发起的"与 /runs?actor=
		"runs_actor",
		"lower(sender_login)",
		// 按仓库列出去重后的 workflow
		"runs_repo_workflow",
		"(repo_id, workflow)",
	} {
		if !strings.Contains(defs, want) {
			t.Errorf("索引定义中没有 %q：\n%s", want, defs)
		}
	}

	// 关闭顺序扫描后，三个高频查询都能用上对应的索引（不是只在定义上存在）。
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `ANALYZE runs`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct{ sql, index string }{
		{`EXPLAIN SELECT id FROM runs WHERE state = 'failed' ORDER BY id DESC LIMIT 5`, "runs_failed"},
		{`EXPLAIN SELECT id FROM runs WHERE lower(sender_login) = lower('alice') ORDER BY id DESC LIMIT 5`, "runs_actor"},
		{`EXPLAIN SELECT DISTINCT workflow FROM runs WHERE repo_id = 42 ORDER BY 1 LIMIT 10`, "runs_repo_workflow"},
	} {
		rows, err := conn.Query(t.Context(), q.sql)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		planText := strings.Join(plan, "\n")
		// 把实际计划写进 -v 的输出，作为索引被用上的证据（审计时可直接复核）。
		t.Logf("%s\n%s", q.sql, planText)
		if !strings.Contains(planText, q.index) {
			t.Errorf("%s 没有用上 %s：\n%s", q.sql, q.index, planText)
		}
	}

	// 重复执行 Migrate 不会再次执行迁移（记录版本之后的幂等）
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	if got := queryString(t, pool, `SELECT count(*)::text FROM pg_indexes WHERE tablename='runs'
		AND indexname IN ('runs_failed', 'runs_actor', 'runs_repo_workflow')`); got != "3" {
		t.Errorf("重复 Migrate 后索引数 = %s，应为 3", got)
	}
}

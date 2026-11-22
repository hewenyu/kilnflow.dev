package db_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
)

// 0034：按 (repo_id, sha) 读取 run 和 AI 审计请求时用索引 runs_repo_sha、ai_requests_repo_sha，只读这个 commit 的行，
// 不再读出仓库的全部历史再过滤（2026-11-17 代码审计 D1）。仓库中有几千个 run / 请求（结果永久保留）时检查计划：
// 去掉索引（或者索引的列不对）时计划用的是 runs_repo / ai_requests_repo，测试失败。
func TestMigrate_RepoSHAIndexes(t *testing.T) {
	t.Parallel()
	pool := open(t, pgtest.URL(t))
	ctx := t.Context()
	const rows = 4000
	for _, sql := range []string{
		`INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`,
		`INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, pr_number, state)
		 SELECT 42, 'ci.yml', 'pull_request', 'refs/pull/1/head', md5(g::text), 'webhook', 'd' || g, g / 10, 'succeeded'
		 FROM generate_series(1, $1::int) g`,
		`INSERT INTO ai_requests (type, repo_id, event, ref, sha, family_id, state)
		 SELECT 'review', 42, 'pull_request', 'refs/pull/1/head', md5(g::text), g, 'succeeded' FROM generate_series(1, $1::int) g`,
	} {
		args := []any{}
		if strings.Contains(sql, "$1") {
			args = append(args, rows)
		}
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s：%v", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE runs; ANALYZE ai_requests`); err != nil {
		t.Fatal(err)
	}
	sha := queryString(t, pool, `SELECT md5('7')`)
	for _, c := range []struct{ query, index string }{
		// engine.PRSettled、PRVerifications（再加 event、pr_number、base_ref 过滤）、LatestVerification、ingest 重跑 commit
		{`SELECT id, state FROM runs WHERE repo_id=$1 AND sha=$2 AND event='pull_request'`, "runs_repo_sha"},
		// review.PRVerifications、aidev.reviewInputs、沿用审计、RerunHead
		{`SELECT id, state FROM ai_requests WHERE type='review' AND repo_id=$1 AND sha=$2`, "ai_requests_repo_sha"},
	} {
		r, err := pool.Query(ctx, `EXPLAIN `+c.query, 42, sha)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(r, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if plan := strings.Join(lines, "\n"); !strings.Contains(plan, c.index) {
			t.Errorf("%s 应使用索引 %s：\n%s", c.query, c.index, plan)
		}
	}
}

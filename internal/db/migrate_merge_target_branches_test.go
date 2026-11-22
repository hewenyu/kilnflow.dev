package db_test

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/migrations"
)

// mergeTargetBranchesVersion 是多目标分支的迁移（0044_merge_target_branches.sql，PLAN #28.5、#29.4）。
const mergeTargetBranchesVersion = 44

// TestMigrate_MergeTargetBranches：在旧的表结构上直接升级；旧的合并队列项和自动打 tag 记录保留，新列取空串默认值；新的
// 列可以写入并读回，旧行按空串参与兼容判断。
func TestMigrate_MergeTargetBranches(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, mergeTargetBranchesVersion)
	exec(t, u, `INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url) VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`)
	exec(t, u, `INSERT INTO merge_queue (repo_id, pr_number, state, head_sha) VALUES (42, 7, 'queued', 'aaaa')`)
	exec(t, u, `INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, source, state, tag) VALUES (42, 'aaaa', 7, 'alice', 'webhook', 'tagged', 'v1.0.0')`)
	body, err := migrations.FS.ReadFile("0044_merge_target_branches.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, u, string(body))
	exec(t, u, `INSERT INTO schema_migrations (version, name) VALUES ($1, '0044_merge_target_branches.sql')`, mergeTargetBranchesVersion)
	pool := open(t, u)

	// 旧行保留，新列取空串（不回填）
	var base, actionBase string
	if err := pool.QueryRow(t.Context(), `SELECT base_ref, action_base FROM merge_queue WHERE repo_id=42 AND pr_number=7`).
		Scan(&base, &actionBase); err != nil {
		t.Fatal(err)
	}
	var defaultBranch, skip, state, tag string
	if err := pool.QueryRow(t.Context(), `SELECT default_branch_at_merge, skip_code, state, tag FROM auto_tags WHERE repo_id=42 AND sha='aaaa'`).
		Scan(&defaultBranch, &skip, &state, &tag); err != nil {
		t.Fatal(err)
	}
	if base != "" || actionBase != "" || defaultBranch != "" || skip != "" {
		t.Fatalf("旧行 = base %q action_base %q default_branch_at_merge %q skip_code %q", base, actionBase, defaultBranch, skip)
	}
	if state != "tagged" || tag != "v1.0.0" {
		t.Fatalf("旧记录被改动：state %q tag %q", state, tag)
	}

	// 四个新列都是 NOT NULL DEFAULT ''
	got := queryString(t, pool, `SELECT string_agg(table_name || '.' || column_name || ':' || is_nullable, ',' ORDER BY table_name, column_name)
		FROM information_schema.columns WHERE (table_name='merge_queue' AND column_name IN ('base_ref','action_base'))
		OR (table_name='auto_tags' AND column_name IN ('default_branch_at_merge','skip_code'))`)
	if got != "auto_tags.default_branch_at_merge:NO,auto_tags.skip_code:NO,merge_queue.action_base:NO,merge_queue.base_ref:NO" {
		t.Fatalf("新列的可空性 = %s", got)
	}

	// 能写入并读回；同一个 commit 重新记录时可以按 (repo_id, sha) 更新
	exec(t, u, `UPDATE merge_queue SET base_ref='dev' WHERE repo_id=42 AND pr_number=7`)
	exec(t, u, `UPDATE auto_tags SET default_branch_at_merge='main', skip_code='branch_not_release' WHERE repo_id=42 AND sha='aaaa'`)
	if got := queryString(t, pool, `SELECT base_ref FROM merge_queue WHERE repo_id=42 AND pr_number=7`); got != "dev" {
		t.Errorf("base_ref = %q", got)
	}
	if got := queryString(t, pool, `SELECT default_branch_at_merge || ' ' || skip_code FROM auto_tags WHERE repo_id=42 AND sha='aaaa'`); got != "main branch_not_release" {
		t.Errorf("auto_tags 新列 = %q", got)
	}
	if idx := queryString(t, pool, `SELECT string_agg(indexdef, ' ' ORDER BY indexname) FROM pg_indexes WHERE tablename='auto_tags'`); !strings.Contains(idx, "(repo_id, sha)") {
		t.Errorf("auto_tags 的 (repo_id, sha) 唯一约束应保留：%s", idx)
	}

	// 待裁决候选：同一个 (repo_id, sha, base_ref) 只能一行（不同分支各一行），仓库删除时级联删除
	exec(t, u, `INSERT INTO auto_tag_candidates (repo_id, sha, pr_number, base_ref, source) VALUES (42, 'aaaa', 7, 'dev', 'webhook')`)
	if _, err := pool.Exec(t.Context(), `INSERT INTO auto_tag_candidates (repo_id, sha, pr_number, base_ref, source)
		VALUES (42, 'aaaa', 8, 'dev', 'webhook')`); err == nil {
		t.Error("同一个 (repo_id, sha, base_ref) 应有唯一约束")
	}
	exec(t, u, `INSERT INTO auto_tag_candidates (repo_id, sha, pr_number, base_ref, source) VALUES (42, 'aaaa', 8, 'master', 'queue')`)
	if got := queryString(t, pool, `SELECT count(*)::text FROM auto_tag_candidates WHERE repo_id=42 AND sha='aaaa'`); got != "2" {
		t.Errorf("不同目标分支的候选应各保留一行：%s", got)
	}
	if got := queryString(t, pool, `SELECT string_agg(column_name || ':' || is_nullable, ',' ORDER BY column_name)
		FROM information_schema.columns WHERE table_name='auto_tag_candidates' AND column_name IN ('repo_id','sha','base_ref','source')`); got != "base_ref:NO,repo_id:NO,sha:NO,source:NO" {
		t.Errorf("候选表的 NOT NULL 列 = %s", got)
	}
	exec(t, u, `DELETE FROM repos WHERE id=42`)
	if got := queryString(t, pool, `SELECT count(*)::text FROM auto_tag_candidates`); got != "0" {
		t.Errorf("删除仓库后候选应级联删除：%s", got)
	}
}

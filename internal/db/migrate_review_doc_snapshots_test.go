package db_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reviewDocSnapshotsVersion 是独立审计文档快照集的迁移（0039_review_doc_snapshots.sql）。
const reviewDocSnapshotsVersion = 39

// expectExecError 执行 SQL，要求它失败（用于验证约束生效）。
func expectExecError(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err == nil {
		t.Fatalf("期望 %q 失败（约束应生效），实际成功", sql)
	}
}

// TestMigrate_ReviewDocSnapshots：0039 在已有 0038 数据上干净升级：旧审计请求记为 legacy、旧工作项冻结的来源列保持 NULL
// （不回填、不伪造），新列与新表的约束生效。
func TestMigrate_ReviewDocSnapshots(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, reviewDocSnapshotsVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
VALUES (42, 1, 1, 'alice', 'demo', 'main', '');

INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
VALUES (1, 'review', 42, 'pull_request', 'refs/pull/7/head', 'a', 1, 'succeeded'),
       (2, 'develop', 42, 'manual', 'refs/heads/main', 'b', 2, 'succeeded');

INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
VALUES (2, 2, 'develop', 'develop', '{macos-arm64}', 'codex', '开发', '{}', 'succeeded', 60);
INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id)
VALUES (1, 2, 1, 'node', 'acq1');

INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
VALUES (7, 42, 'developing', 'main', 'a', 2, 'codex', 1);

-- 迁移前的审计请求：有工作项关联，但没有记录文档依据（docs_source 列尚不存在）。
INSERT INTO ai_review_requests (request_id, pr_number, head_ref, base_ref, work_item_id)
VALUES (1, 7, 'feat', 'main', 7), (2, NULL, '', 'main', NULL);

-- 迁移前的工作项冻结：来源列尚不存在。
INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
VALUES (7, 'd1', '需求文档', 3, 'sha-content', '正文内容', 'https://outline.example.com/doc/d1');
INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES (7, 1);
`)

	pool := open(t, u)

	// 旧审计请求：docs_source 回落到 legacy，摘要为空，没有快照集，工作项关联保持原样。
	var source, digest string
	var setID *int64
	var workItem *int64
	if err := pool.QueryRow(t.Context(), `SELECT docs_source, docs_manifest_digest, doc_snapshot_set_id, work_item_id
		FROM ai_review_requests WHERE request_id=1`).Scan(&source, &digest, &setID, &workItem); err != nil {
		t.Fatal(err)
	}
	if source != "legacy" || digest != "" || setID != nil {
		t.Fatalf("旧审计请求 = source %q digest %q set %v，期望 legacy / 空 / NULL", source, digest, setID)
	}
	if workItem == nil || *workItem != 7 {
		t.Fatalf("旧审计请求的 work_item_id = %v，期望 7（不被回填或篡改）", workItem)
	}

	// 旧工作项冻结：来源列保持 NULL，不回填 provider / 代次 / 摘要，也没有被误标为已清理。
	var provider, digestCol, purged *string
	var generation *int64
	if err := pool.QueryRow(t.Context(), `SELECT provider, manifest_digest, content_purged_at::text, binding_generation
		FROM work_item_doc_freezes WHERE work_item_id=7`).Scan(&provider, &digestCol, &purged, &generation); err != nil {
		t.Fatal(err)
	}
	if provider != nil || digestCol != nil || purged != nil || generation != nil {
		t.Fatalf("旧冻结的来源列应保持 NULL：provider=%v digest=%v purged=%v generation=%v", provider, digestCol, purged, generation)
	}

	// 新表可用：写入一个快照集后，snapshot_set 来源的审计请求要求快照集非空且工作项为空。
	exec(t, u, `INSERT INTO doc_snapshot_sets (repo_id, provider, base_url, collection_id, binding_generation, doc_count, manifest_digest)
		VALUES (42, 'outline', 'https://outline.example.com', 'c1', 2, 1, repeat('a', 64))`)
	var newSet int64
	if err := pool.QueryRow(t.Context(), `SELECT id FROM doc_snapshot_sets`).Scan(&newSet); err != nil {
		t.Fatal(err)
	}
	exec(t, u, `INSERT INTO doc_snapshot_docs (snapshot_set_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd1', '需求', 1, repeat('b', 64), '正文', 'https://outline.example.com/doc/d1')`, newSet)

	// 约束：snapshot_set 来源必须带快照集，且不能同时带工作项。
	expectExecError(t, pool, `UPDATE ai_review_requests SET docs_source='snapshot_set', docs_manifest_digest=repeat('c',64)
		WHERE request_id=2`)
	expectExecError(t, pool, `UPDATE ai_review_requests SET docs_source='snapshot_set', doc_snapshot_set_id=$1,
		docs_manifest_digest=repeat('c',64) WHERE request_id=1`, newSet)
	// 约束：work_item 来源必须带工作项、不能带快照集。
	expectExecError(t, pool, `UPDATE ai_review_requests SET docs_source='work_item', work_item_id=NULL,
		docs_manifest_digest=repeat('c',64) WHERE request_id=1`)
	// 约束：非 legacy / unplanned 的来源必须有合法的 64 位 hex 摘要。
	expectExecError(t, pool, `UPDATE ai_review_requests SET docs_source='none', docs_manifest_digest='' WHERE request_id=2`)
	// 约束：快照集的摘要必须是 64 位 hex。
	expectExecError(t, pool, `INSERT INTO doc_snapshot_sets (repo_id, provider, base_url, collection_id, binding_generation, doc_count, manifest_digest)
		VALUES (42, 'outline', 'https://outline.example.com', 'c2', 3, 0, 'not-a-digest')`)

	// 合法的组合可以写入。
	exec(t, u, `UPDATE ai_review_requests SET docs_source='snapshot_set', doc_snapshot_set_id=$1,
		work_item_id=NULL, docs_manifest_digest=repeat('c',64) WHERE request_id=1`, newSet)
	// 旧行被引用后仍不能被清理抹掉依据：删除快照集会被外键挡住。
	expectExecError(t, pool, `DELETE FROM doc_snapshot_sets WHERE id=$1`, newSet)
}

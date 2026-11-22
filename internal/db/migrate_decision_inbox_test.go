package db_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// decisionInboxVersion 是「待决策清单」同步的迁移（0040_decision_inbox.sql）。
const decisionInboxVersion = 40

// constraintName 返回表上定义里包含 fragment 的约束名。测试按定义找名字，再断言实际违反的正是这个约束，
// 避免"任意一个约束报错就算通过"的假阳性。
func constraintName(t *testing.T, pool *pgxpool.Pool, table, fragment string) string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid=$1::regclass`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found string
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(def, fragment) {
			if found != "" {
				t.Fatalf("定义 %q 匹配了多个约束：%s、%s", fragment, found, name)
			}
			found = name
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("没有找到定义包含 %q 的约束", fragment)
	}
	return found
}

// expectConstraint 执行 SQL，要求它以指定 SQLSTATE 和约束名失败。
func expectConstraint(t *testing.T, pool *pgxpool.Pool, sqlstate, constraint, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(t.Context(), sql, args...)
	if err == nil {
		t.Fatalf("期望 %q 失败（约束 %s 应生效），实际成功", sql, constraint)
	}
	pe, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		t.Fatalf("期望 *pgconn.PgError，实际 %T：%v", err, err)
	}
	if pe.Code != sqlstate || pe.ConstraintName != constraint {
		t.Fatalf("错误 = %s / %s，期望 %s / %s", pe.Code, pe.ConstraintName, sqlstate, constraint)
	}
}

// TestMigrate_DecisionInbox：0040 在已有 0039 数据上干净升级，新增的表、序列、生成列与约束生效。
// 每个反例只违反目标那一个约束，并断言 SQLSTATE 与精确的约束名。
func TestMigrate_DecisionInbox(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, decisionInboxVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
VALUES (42, 1, 1, 'alice', 'demo', 'main', ''),
       (43, 1, 1, 'alice', 'other', 'main', '');
INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
VALUES (2, 'develop', 42, 'manual', 'refs/heads/main', 'b', 2, 'succeeded');
INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
VALUES (2, 2, 'develop', 'develop', '{macos-arm64}', 'codex', '开发', '{}', 'succeeded', 60);
INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id)
VALUES (1, 2, 1, 'node', 'acq1'), (2, 2, 2, 'node', 'acq2');
INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
VALUES (7, 42, 'developing', 'main', 'a', 2, 'codex', 1),
       (8, 42, 'developing', 'main', 'a', 2, 'codex', 2);
`)

	pool := open(t, u)
	ctx := t.Context()

	// 序列由生成列 / 默认值使用，代次为正。
	var generation int64
	err := pool.QueryRow(ctx, `INSERT INTO repo_decision_docs
		(repo_id, base_url, doc_id, collection_id, token_ciphertext, token_wrapped_dek, created_by, updated_by)
		VALUES (42, 'https://outline.example.com', '11111111-1111-4111-8111-111111111111', 'c1', '\x01', '\x02', 'admin', 'admin')
		RETURNING generation`).Scan(&generation)
	if err != nil {
		t.Fatalf("插入待决策清单配置：%v", err)
	}
	if generation <= 0 {
		t.Fatalf("generation = %d，应由序列生成", generation)
	}
	// 序列的下一个值只由重设推进（普通更新不动代次）。
	var next int64
	if err := pool.QueryRow(ctx, `UPDATE repo_decision_docs SET updated_at=now() RETURNING generation`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != generation {
		t.Fatalf("普通更新不应推进代次：%d → %d", generation, next)
	}
	// provider 只允许 outline（用另一个已存在的仓库，避免主键冲突掩盖 CHECK）。
	expectConstraint(t, pool, "23514",
		constraintName(t, pool, "repo_decision_docs", "provider = 'outline'"),
		`INSERT INTO repo_decision_docs
		(repo_id, provider, base_url, doc_id, collection_id, token_ciphertext, token_wrapped_dek, created_by, updated_by)
		VALUES (43, 'github', 'https://x', '11111111-1111-4111-8111-111111111111', 'c', '\x01', '\x02', 'a', 'a')`)

	const insertSync = `INSERT INTO dev_decision_syncs (repo_id, work_item_id, task_id, entry_idx, target_provider,
		target_base_url, target_doc_id, target_collection_id, target_generation, marker, entry_kind, category,
		report_sha256, payload)
		VALUES (42, $1, 2, $2, 'outline', 'https://outline.example.com', '11111111-1111-4111-8111-111111111111', 'c1', $3,
		$4, $5, $6, repeat('a', 64), '{}')`

	exec(t, u, insertSync, 7, 0, generation, "kiln-dec:42:wi7:t2:0", "decision", "other")
	// confirmed_decision 是 category 的生成列。
	var confirmed bool
	if err := pool.QueryRow(ctx, `SELECT confirmed_decision FROM dev_decision_syncs WHERE entry_idx=0`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Error("category=other 时 confirmed_decision 应为 false")
	}
	exec(t, u, insertSync, 7, 1, generation, "kiln-dec:42:wi7:t2:1", "decision", "confirmed_decision")
	if err := pool.QueryRow(ctx, `SELECT confirmed_decision FROM dev_decision_syncs WHERE entry_idx=1`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Error("category=confirmed_decision 时 confirmed_decision 应为 true")
	}
	// 受阻摘要没有类别，confirmed_decision 为 false。
	exec(t, u, insertSync, 7, 2, generation, "kiln-dec:42:wi7:t2:2", "blocked", nil)

	// marker 必须由这些列拼出来。
	expectConstraint(t, pool, "23514", constraintName(t, pool, "dev_decision_syncs", "'kiln-dec:'"),
		insertSync, 7, 3, generation, "kiln-dec:42:wi7:t2:999", "decision", "other")
	// 决策条目必须有类别，受阻摘要不能有类别。
	decisionKind := constraintName(t, pool, "dev_decision_syncs", "entry_kind = 'decision'::text")
	expectConstraint(t, pool, "23514", decisionKind, insertSync, 7, 4, generation, "kiln-dec:42:wi7:t2:4", "decision", nil)
	expectConstraint(t, pool, "23514", decisionKind, insertSync, 7, 5, generation, "kiln-dec:42:wi7:t2:5", "blocked", "other")
	// (repo_id, task_id, entry_idx) 唯一：换一个工作项，marker 随之不同，只可能撞复合唯一键。
	compositeUnique := constraintName(t, pool, "dev_decision_syncs", "UNIQUE (repo_id, task_id, entry_idx)")
	expectConstraint(t, pool, "23505", compositeUnique, insertSync, 8, 0, generation, "kiln-dec:42:wi8:t2:0", "decision", "other")
	// marker 自己也有唯一约束（由 marker 的 CHECK 与复合唯一键共同保证不会先撞它，这里确认约束存在）。
	constraintName(t, pool, "dev_decision_syncs", "UNIQUE (marker)")
	// 摘要必须是 64 位 hex。
	expectConstraint(t, pool, "23514", constraintName(t, pool, "dev_decision_syncs", "report_sha256 ~"),
		`INSERT INTO dev_decision_syncs (repo_id, work_item_id, task_id, entry_idx, target_provider,
		target_base_url, target_doc_id, target_collection_id, target_generation, marker, entry_kind, category,
		report_sha256, payload) VALUES (42,7,2,7,'outline','https://outline.example.com',
		'11111111-1111-4111-8111-111111111111','c1',1,'kiln-dec:42:wi7:t2:7','decision','other','not-a-digest','{}')`)

	// 状态与辅助列的一致性：每个反例只违反一个约束。
	attemptedCheck := constraintName(t, pool, "dev_decision_syncs", "state <> 'attempted'::text")
	expectConstraint(t, pool, "23514", attemptedCheck, `UPDATE dev_decision_syncs SET state='attempted' WHERE entry_idx=0`)
	expectConstraint(t, pool, "23514", attemptedCheck,
		`UPDATE dev_decision_syncs SET state='attempted', attempts=1, payload_md='块', attempted_at=now() WHERE entry_idx=0`)
	attemptsCheck := constraintName(t, pool, "dev_decision_syncs", "payload_md IS NOT NULL")
	expectConstraint(t, pool, "23514", attemptsCheck, `UPDATE dev_decision_syncs SET attempts=1 WHERE entry_idx=0`)
	retryCheck := constraintName(t, pool, "dev_decision_syncs", "next_retry_at IS NOT NULL")
	// delivered 必须同时有 delivered_at、且没有下次处理时间：拆成两个只违反一个约束的用例。
	deliveredAtCheck := constraintName(t, pool, "dev_decision_syncs", "delivered_at IS NOT NULL")
	expectConstraint(t, pool, "23514", deliveredAtCheck,
		`UPDATE dev_decision_syncs SET state='delivered', next_retry_at=NULL WHERE entry_idx=0`)
	expectConstraint(t, pool, "23514", retryCheck,
		`UPDATE dev_decision_syncs SET state='delivered', delivered_at=now() WHERE entry_idx=0`)
	// pending 必须有下次处理时间；unknown 不能有。
	expectConstraint(t, pool, "23514", retryCheck, `UPDATE dev_decision_syncs SET next_retry_at=NULL WHERE entry_idx=0`)
	expectConstraint(t, pool, "23514", retryCheck, `UPDATE dev_decision_syncs SET state='unknown' WHERE entry_idx=0`)

	// 合法的 attempted 与终态可以写入。
	exec(t, u, `UPDATE dev_decision_syncs SET payload_md='块', attempted_at=now(), attempts=1,
		attempted_revision=3, state='attempted' WHERE entry_idx=0`)
	exec(t, u, `UPDATE dev_decision_syncs SET state='delivered', delivered_revision=4, delivered_at=now(),
		next_retry_at=NULL WHERE entry_idx=0`)

	// 到期的部分索引存在且谓词正确。
	var indexDef string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE tablename='dev_decision_syncs'
		AND indexname='dev_decision_syncs_due'`).Scan(&indexDef); err != nil {
		t.Fatalf("到期索引不存在：%v", err)
	}
	if !strings.Contains(indexDef, "WHERE") || !strings.Contains(indexDef, "'pending'") ||
		!strings.Contains(indexDef, "'attempted'") {
		t.Errorf("到期索引的谓词 = %q，应只包含 pending / attempted", indexDef)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename='dev_decision_syncs'
		AND indexname='dev_decision_syncs_repo_state'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("repo_state 索引数量 = %d，期望 1", n)
	}
	// 序列有归属列。
	var owned string
	if err := pool.QueryRow(ctx, `SELECT d.refobjid::regclass::text FROM pg_depend d
		JOIN pg_class s ON s.oid=d.objid WHERE s.relname='repo_decision_docs_generation_seq' AND d.deptype='a'`).Scan(&owned); err != nil {
		t.Fatalf("序列应有归属列：%v", err)
	}
	if owned != "repo_decision_docs" {
		t.Errorf("序列归属 = %s，期望 repo_decision_docs", owned)
	}
}

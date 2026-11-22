package db_test

import (
	"testing"
)

// docBindingChecksVersion 是文档绑定验证结果的迁移（0041_doc_binding_checks.sql）。
const docBindingChecksVersion = 41

// TestMigrate_DocBindingChecks：0041 在已有绑定上干净升级，新增列的历史行为默认值正确，检查状态与篇数的约束生效。
func TestMigrate_DocBindingChecks(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, docBindingChecksVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
VALUES (42, 1, 1, 'alice', 'demo', 'main', '');
INSERT INTO repo_doc_bindings (repo_id, base_url, collection_id, token_ciphertext, token_wrapped_dek, created_by)
VALUES (42, 'https://outline.example.com', 'c1', '\x01', '\x02', 'alice');
`)
	pool := open(t, u) // 执行 0041
	ctx := t.Context()

	// 历史行不伪造 updated_by（保持 NULL），验证字段为空、篇数为 NULL（不是 0）。
	var (
		updatedBy *string
		status    *string
		count     *int
		code      string
	)
	if err := pool.QueryRow(ctx, `SELECT updated_by, check_status, check_doc_count, check_error_code
		FROM repo_doc_bindings WHERE repo_id=42`).Scan(&updatedBy, &status, &count, &code); err != nil {
		t.Fatal(err)
	}
	if updatedBy != nil || status != nil || count != nil || code != "" {
		t.Fatalf("历史行的新列应为 NULL / 空：updated_by=%v status=%v count=%v code=%q", updatedBy, status, count, code)
	}

	statusConstraint := constraintName(t, pool, "repo_doc_bindings", "check_status")
	countConstraint := constraintName(t, pool, "repo_doc_bindings", "check_doc_count")

	// 只有固定枚举可以作为验证结论。
	expectConstraint(t, pool, "23514", statusConstraint,
		`UPDATE repo_doc_bindings SET check_status='partial' WHERE repo_id=42`)
	// 篇数不能为负。
	expectConstraint(t, pool, "23514", countConstraint,
		`UPDATE repo_doc_bindings SET check_doc_count=-1 WHERE repo_id=42`)
	// 合法值可以写入。
	if _, err := pool.Exec(ctx, `UPDATE repo_doc_bindings SET check_status='empty', check_doc_count=0 WHERE repo_id=42`); err != nil {
		t.Fatalf("合法的验证结果应能写入：%v", err)
	}
}

package db_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// removeDocWebhookVersion 是移除 Outline Webhook 接入与自动差分建单的迁移（0038_remove_doc_webhook.sql）。
const removeDocWebhookVersion = 38

// docTestMasterKey 是测试用的主密钥（32 字节，与信封加密的要求一致）。
const docTestMasterKey = "0123456789abcdef0123456789abcdef"

// bindingRow 是 repo_doc_bindings 中迁移前后都要完整比对的字段。
type bindingRow struct {
	generation     int64
	ciphertext     []byte
	wrappedDEK     []byte
	provider       string
	baseURL        string
	collectionID   string
	collectionName string
	createdBy      string
	createdAt      time.Time
	updatedAt      time.Time
}

// scanBinding 读取一个绑定的全部持久化字段。
func scanBinding(t *testing.T, pool *pgxpool.Pool, repoID int64) bindingRow {
	t.Helper()
	var b bindingRow
	if err := pool.QueryRow(t.Context(), `SELECT generation, token_ciphertext, token_wrapped_dek,
			provider, base_url, collection_id, collection_name, created_by, created_at, updated_at
		FROM repo_doc_bindings WHERE repo_id=$1`, repoID).Scan(
		&b.generation, &b.ciphertext, &b.wrappedDEK,
		&b.provider, &b.baseURL, &b.collectionID, &b.collectionName, &b.createdBy, &b.createdAt, &b.updatedAt); err != nil {
		t.Fatalf("读取仓库 %d 的绑定：%v", repoID, err)
	}
	return b
}

// snapshotRow 是 work_item_doc_snapshots 中迁移前后都要完整比对的字段。
type snapshotRow struct {
	title, sha, content, url string
	revision                 int64
	createdAt                time.Time
}

func scanSnapshot(t *testing.T, pool *pgxpool.Pool, workItemID int64, docID string) snapshotRow {
	t.Helper()
	var s snapshotRow
	if err := pool.QueryRow(t.Context(), `SELECT title, revision, content_sha256, content, url, created_at
		FROM work_item_doc_snapshots WHERE work_item_id=$1 AND doc_id=$2`, workItemID, docID).Scan(
		&s.title, &s.revision, &s.sha, &s.content, &s.url, &s.createdAt); err != nil {
		t.Fatalf("读取工作项 %d 的文档快照：%v", workItemID, err)
	}
	return s
}

// 0038：删除自动差分专用的 doc_watermarks、doc_webhook_deliveries、doc_delta_issues 三张表，以及绑定上的
// hook_enabled 与 Webhook 签名密钥列；repo_doc_bindings.generation 及其序列、既有绑定记录（含可解密的 token 密文）、
// 工作项文档快照与冻结标记必须完整保留，序列继续推进、不复用旧代次。
func TestMigrate_RemoveDocWebhook(t *testing.T) {
	t.Parallel()
	u := databaseAt(t, removeDocWebhookVersion)
	exec(t, u, `
INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
VALUES (42, 1, 1, 'alice', 'demo', 'main', ''), (43, 1, 1, 'alice', 'other', 'main', '');
INSERT INTO doc_watermarks (repo_id, doc_id, seen_rev, triaged_rev) VALUES (42, 'd1', 5, 4);
INSERT INTO doc_webhook_deliveries (repo_id, delivery_id) VALUES (42, 'delivery-1');
INSERT INTO doc_delta_issues (repo_id, doc_id, revision, title) VALUES (42, 'd1', 5, '文档更新');

INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
VALUES (1, 'develop', 42, 'manual', 'refs/heads/main', 'a', 1, 'succeeded');
INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
VALUES (1, 1, 'k1', 'develop', '{macos-arm64}', 'codex', 't', '{}', 'succeeded', 60);
INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id)
VALUES (1, 1, 1, 'node', 'acq1');
INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id)
VALUES (7, 42, 'developing', 'main', 'a', 2, 'codex', 1);
INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url, created_at)
VALUES (7, 'd1', '需求文档', 3, 'sha-content', '正文内容', 'https://outline.example.com/doc/d1', '2026-01-04 05:06:07+00');
INSERT INTO work_item_doc_freezes (work_item_id, doc_count, frozen_at) VALUES (7, 1, '2026-01-04 05:06:08+00');
`)

	// 用真实的信封加密写入 token 密文，迁移后要能经 store.ResolveDocToken 解出原文。
	masterKey := []byte(docTestMasterKey)
	sealed, err := secretbox.Seal(masterKey, secretbox.AAD("doc", 42, "token"), []byte("outline-token-abc"))
	if err != nil {
		t.Fatal(err)
	}
	exec(t, u, `INSERT INTO repo_doc_bindings (repo_id, provider, base_url, collection_id, collection_name,
			token_ciphertext, token_wrapped_dek, hook_enabled, webhook_secret_ciphertext, webhook_secret_wrapped_dek,
			created_by, created_at, updated_at)
		VALUES (42, 'outline', 'https://outline.example.com', 'c1', '设计', $1, $2, true, '\x01', '\x02',
			'alice', '2026-01-02 03:04:05+00', '2026-01-03 04:05:06+00')`, sealed.Ciphertext, sealed.WrappedDEK)
	// 第二个绑定同样走 generation 的默认值（nextval），真正消费序列。
	exec(t, u, `INSERT INTO repo_doc_bindings (repo_id, provider, base_url, collection_id,
			token_ciphertext, token_wrapped_dek) VALUES (43, 'outline', 'https://other.example.com', 'c2', '\x00', '\x00')`)

	beforePool, err := pgxpool.New(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	beforeBinding := scanBinding(t, beforePool, 42)
	beforeSnapshot := scanSnapshot(t, beforePool, 7, "d1")
	var beforeMaxGeneration int64
	if err := beforePool.QueryRow(t.Context(), `SELECT max(generation) FROM repo_doc_bindings`).Scan(&beforeMaxGeneration); err != nil {
		t.Fatal(err)
	}
	if beforeMaxGeneration < 2 {
		t.Fatalf("迁移前序列应已被消费（最大代次 >= 2），实际 %d", beforeMaxGeneration)
	}
	var beforeDocCount int
	var beforeFrozenAt time.Time
	if err := beforePool.QueryRow(t.Context(), `SELECT doc_count, frozen_at FROM work_item_doc_freezes WHERE work_item_id=7`).
		Scan(&beforeDocCount, &beforeFrozenAt); err != nil {
		t.Fatal(err)
	}
	beforePool.Close()

	pool := open(t, u)
	assertDocTablesDropped(t, pool)
	assertHookColumnsDropped(t, pool)

	// 绑定逐字段无损：代次、两段密文、连接信息、创建者与时间戳。
	assertBindingUnchanged(t, beforeBinding, scanBinding(t, pool, 42))

	// 旧绑定的 token 仍能经真实解密路径读出。
	sdb := store.New(pool)
	tk, err := store.ResolveDocToken(t.Context(), sdb, masterKey, 42)
	if err != nil {
		t.Fatalf("升级后解密旧绑定 token：%v", err)
	}
	if tk == nil || tk.Token != "outline-token-abc" || tk.Generation != beforeBinding.generation {
		t.Fatalf("升级后 ResolveDocToken = %+v，期望 token=outline-token-abc、generation=%d", tk, beforeBinding.generation)
	}

	// 升级后继续消费序列：重绑拿到的新代次严格大于迁移前的最大代次，不会重置或复用。
	if err := store.PutDocBinding(t.Context(), sdb, masterKey, store.DocBinding{
		RepoID: 42, Provider: "outline", BaseURL: "https://new.example.com", CollectionID: "c3",
	}, "new-token"); err != nil {
		t.Fatalf("升级后重绑：%v", err)
	}
	var rebindGeneration int64
	if err := pool.QueryRow(t.Context(), `SELECT generation FROM repo_doc_bindings WHERE repo_id=42`).Scan(&rebindGeneration); err != nil {
		t.Fatal(err)
	}
	if rebindGeneration <= beforeMaxGeneration {
		t.Fatalf("重绑后的 generation = %d，应严格大于迁移前最大代次 %d", rebindGeneration, beforeMaxGeneration)
	}
	if tk, err := store.ResolveDocToken(t.Context(), sdb, masterKey, 42); err != nil || tk == nil || tk.Token != "new-token" {
		t.Fatalf("重绑后 ResolveDocToken = %+v, %v，期望 new-token", tk, err)
	}

	// 工作项文档快照与冻结标记逐字段无损。
	assertSnapshotUnchanged(t, beforeSnapshot, scanSnapshot(t, pool, 7, "d1"))
	assertFreezeUnchanged(t, pool, beforeDocCount, beforeFrozenAt)
}

// assertDocTablesDropped 确认自动差分专用的三张表已删除。
func assertDocTablesDropped(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"doc_watermarks", "doc_webhook_deliveries", "doc_delta_issues"} {
		var reg *string
		if err := pool.QueryRow(t.Context(), `SELECT to_regclass($1)::text`, table).Scan(&reg); err != nil {
			t.Fatal(err)
		}
		if reg != nil {
			t.Errorf("表 %s 应已删除，实际存在", table)
		}
	}
}

// assertHookColumnsDropped 确认绑定上的 hook 开关与 Webhook 密钥列已删除。
func assertHookColumnsDropped(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var columns int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns
		WHERE table_name='repo_doc_bindings'
		  AND column_name IN ('hook_enabled','webhook_secret_ciphertext','webhook_secret_wrapped_dek')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Errorf("repo_doc_bindings 上应删掉 hook/密钥列，实际剩 %d 列", columns)
	}
}

// assertBindingUnchanged 逐字段比对迁移前后的绑定。
func assertBindingUnchanged(t *testing.T, before, after bindingRow) {
	t.Helper()
	if after.generation != before.generation ||
		!bytes.Equal(after.ciphertext, before.ciphertext) ||
		!bytes.Equal(after.wrappedDEK, before.wrappedDEK) ||
		after.provider != before.provider ||
		after.baseURL != before.baseURL ||
		after.collectionID != before.collectionID ||
		after.collectionName != before.collectionName ||
		after.createdBy != before.createdBy ||
		!after.createdAt.Equal(before.createdAt) ||
		!after.updatedAt.Equal(before.updatedAt) {
		t.Errorf("绑定在迁移后发生变化：\nbefore %+v\nafter  %+v", before, after)
	}
}

// assertSnapshotUnchanged 逐字段比对迁移前后的工作项文档快照。
func assertSnapshotUnchanged(t *testing.T, before, after snapshotRow) {
	t.Helper()
	if after.title != before.title ||
		after.revision != before.revision ||
		after.sha != before.sha ||
		after.content != before.content ||
		after.url != before.url ||
		!after.createdAt.Equal(before.createdAt) {
		t.Errorf("文档快照在迁移后发生变化：\nbefore %+v\nafter  %+v", before, after)
	}
}

// assertFreezeUnchanged 比对迁移前后的冻结标记。
func assertFreezeUnchanged(t *testing.T, pool *pgxpool.Pool, beforeDocCount int, beforeFrozenAt time.Time) {
	t.Helper()
	var docCount int
	var frozenAt time.Time
	if err := pool.QueryRow(t.Context(), `SELECT doc_count, frozen_at FROM work_item_doc_freezes WHERE work_item_id=7`).
		Scan(&docCount, &frozenAt); err != nil {
		t.Fatal(err)
	}
	if docCount != beforeDocCount || !frozenAt.Equal(beforeFrozenAt) {
		t.Errorf("冻结标记在迁移后发生变化：before %d/%v，after %d/%v", beforeDocCount, beforeFrozenAt, docCount, frozenAt)
	}
}

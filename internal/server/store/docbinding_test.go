package store

import (
	"bytes"
	"testing"
	"time"
)

// 文档绑定的测试：加解密、覆盖时保留创建信息、代次推进、删除。

func TestDocBindingRoundTrip(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")

	b := DocBinding{
		RepoID: 10, Provider: "outline", BaseURL: "https://outline.example.com",
		CollectionID: "col-1", CollectionName: "产品文档", CreatedBy: "admin",
	}
	const token = "outline-token-abc123"
	if err := PutDocBinding(ctx, db, testMasterKey, b, token); err != nil {
		t.Fatal(err)
	}

	got, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("应读到刚写入的绑定")
	}
	if got.RepoID != 10 || got.Provider != "outline" || got.BaseURL != b.BaseURL ||
		got.CollectionID != "col-1" || got.CollectionName != "产品文档" || got.CreatedBy != "admin" {
		t.Errorf("GetDocBinding = %+v", *got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("created_at / updated_at 应已写入：%+v", *got)
	}
	if got.Generation <= 0 {
		t.Errorf("generation 应由序列生成：%+v", *got)
	}

	// 解密后的 token 应和写入的一致
	tk, err := ResolveDocToken(ctx, db, testMasterKey, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tk == nil || tk.Token != token {
		t.Fatalf("ResolveDocToken = %+v，token 应为 %q", tk, token)
	}
	if tk.RepoID != 10 || tk.CollectionID != "col-1" || tk.Generation != got.Generation {
		t.Errorf("ResolveDocToken 的绑定信息 = %+v", tk.DocBinding)
	}

	// 数据库中的密文不应包含明文 token
	var ciphertext []byte
	if err := db.QueryRow(ctx, `SELECT token_ciphertext FROM repo_doc_bindings WHERE repo_id=10`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(token)) {
		t.Error("token_ciphertext 不应包含明文 token")
	}
}

func TestPutDocBindingKeepsCreatedInfo(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")

	first := DocBinding{RepoID: 10, Provider: "outline", BaseURL: "https://a.example.com", CollectionID: "c1", CreatedBy: "alice"}
	if err := PutDocBinding(ctx, db, testMasterKey, first, "token-1"); err != nil {
		t.Fatal(err)
	}
	before, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}

	// 第二次写入换 token 和连接信息、created_by
	second := DocBinding{RepoID: 10, Provider: "outline", BaseURL: "https://b.example.com", CollectionID: "c2", CreatedBy: "bob"}
	if err := PutDocBinding(ctx, db, testMasterKey, second, "token-2"); err != nil {
		t.Fatal(err)
	}
	after, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if after.CreatedBy != "alice" {
		t.Errorf("created_by 应保留首次写入的 alice，实际 %q", after.CreatedBy)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("created_at 应保持不变：%v -> %v", before.CreatedAt, after.CreatedAt)
	}
	if after.BaseURL != second.BaseURL || after.CollectionID != "c2" {
		t.Errorf("连接信息应被覆盖：%+v", *after)
	}
	// 绑定代次在覆盖时推进到序列的下一个值。
	if after.Generation <= before.Generation {
		t.Errorf("generation 应推进：%d -> %d", before.Generation, after.Generation)
	}
	tk, err := ResolveDocToken(ctx, db, testMasterKey, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Token != "token-2" {
		t.Errorf("token 应被覆盖为 token-2，实际 %q", tk.Token)
	}
}

func TestResolveDocTokenWrongKey(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{RepoID: 10, BaseURL: "https://a", CollectionID: "c"}, "token"); err != nil {
		t.Fatal(err)
	}

	wrongKey := bytes.Repeat([]byte{9}, len(testMasterKey))
	if tk, err := ResolveDocToken(ctx, db, wrongKey, 10); err == nil || tk != nil {
		t.Fatalf("用错误的主密钥应解密失败：%+v, %v", tk, err)
	}
}

func TestDeleteDocBinding(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")

	// 没有绑定时返回 false
	if ok, err := DeleteDocBinding(ctx, db, 10); err != nil || ok {
		t.Errorf("没有绑定时 DeleteDocBinding 应返回 false：%v, %v", ok, err)
	}

	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{RepoID: 10, BaseURL: "https://a", CollectionID: "c"}, "token"); err != nil {
		t.Fatal(err)
	}
	if ok, err := DeleteDocBinding(ctx, db, 10); err != nil || !ok {
		t.Fatalf("DeleteDocBinding 应删除一行：%v, %v", ok, err)
	}
	if b, err := GetDocBinding(ctx, db, 10); err != nil || b != nil {
		t.Errorf("删除后应读到 nil：%+v, %v", b, err)
	}
	if ok, err := DeleteDocBinding(ctx, db, 10); err != nil || ok {
		t.Errorf("再次删除应返回 false：%v, %v", ok, err)
	}
}

func TestListDocBindings(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 20, 1, 100, "alice", "b")
	insertRepo(t, db, 10, 1, 100, "alice", "a")
	for _, id := range []int64{20, 10} {
		if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{RepoID: id, BaseURL: "https://a", CollectionID: "c"}, "token"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ListDocBindings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].RepoID != 10 || list[1].RepoID != 20 {
		t.Fatalf("应按 repo_id 排序返回两条：%+v", list)
	}
}

func TestDocBindingMissing(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")

	if b, err := GetDocBinding(ctx, db, 10); err != nil || b != nil {
		t.Errorf("没有绑定时 GetDocBinding 应返回 nil：%+v, %v", b, err)
	}
	if tk, err := ResolveDocToken(ctx, db, testMasterKey, 10); err != nil || tk != nil {
		t.Errorf("没有绑定时 ResolveDocToken 应返回 nil：%+v, %v", tk, err)
	}
	// 不存在的仓库同样返回 nil
	if b, err := GetDocBinding(ctx, db, 99); err != nil || b != nil {
		t.Errorf("不存在的仓库应返回 nil：%+v, %v", b, err)
	}
}

func TestSaveDocBindingCheck(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	actor := "bob"
	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{
		RepoID: 10, BaseURL: "https://a", CollectionID: "c", CreatedBy: "alice", UpdatedBy: &actor,
	}, "token"); err != nil {
		t.Fatal(err)
	}
	before, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if before.UpdatedBy == nil || *before.UpdatedBy != "bob" {
		t.Fatalf("updated_by 应为 bob：%+v", before.UpdatedBy)
	}

	count := 0
	started := before.CreatedAt.Add(time.Hour)
	checked := started.Add(time.Minute)
	ok, err := SaveDocBindingCheck(ctx, db, 10, before.Generation, DocCheckResult{
		StartedAt: started, CheckedAt: checked, CheckedBy: "carol", Status: "empty",
		DocCount: &count, CollectionURL: "https://a/collection/c", ObservedCollectionName: "产品文档",
	})
	if err != nil || !ok {
		t.Fatalf("SaveDocBindingCheck = %v, %v", ok, err)
	}
	after, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Errorf("验证结果不应推进代次：%d -> %d", before.Generation, after.Generation)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.UpdatedBy == nil || *after.UpdatedBy != "bob" {
		t.Errorf("验证结果不应修改 updated_at / updated_by：%+v", after)
	}
	if after.CheckStatus == nil || *after.CheckStatus != "empty" || after.CheckDocCount == nil || *after.CheckDocCount != 0 {
		t.Errorf("验证结果没有写对：status=%v count=%v", after.CheckStatus, after.CheckDocCount)
	}
	if after.CheckedBy == nil || *after.CheckedBy != "carol" || after.CollectionURL != "https://a/collection/c" ||
		after.ObservedCollectionName != "产品文档" {
		t.Errorf("验证结果的观测字段没有写对：%+v", after)
	}

	// 旧代次不覆写：重绑之后用旧代次保存影响 0 行。
	actor2 := "dave"
	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{
		RepoID: 10, BaseURL: "https://b", CollectionID: "c2", CreatedBy: "alice", UpdatedBy: &actor2,
	}, "token2"); err != nil {
		t.Fatal(err)
	}
	if ok, err := SaveDocBindingCheck(ctx, db, 10, before.Generation, DocCheckResult{
		StartedAt: started, CheckedAt: checked, CheckedBy: "carol", Status: "ok",
	}); err != nil || ok {
		t.Fatalf("旧代次的验证结果应被拒绝：%v, %v", ok, err)
	}
	rebound, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.CheckStatus != nil {
		t.Errorf("旧代次的验证结果不应写到新配置上：%+v", rebound.CheckStatus)
	}
}

func TestPutDocBindingClearsCheck(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	actor := "alice"
	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{
		RepoID: 10, BaseURL: "https://a", CollectionID: "c", CreatedBy: "alice", UpdatedBy: &actor,
	}, "token"); err != nil {
		t.Fatal(err)
	}
	b, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 2
	if ok, err := SaveDocBindingCheck(ctx, db, 10, b.Generation, DocCheckResult{
		StartedAt: b.CreatedAt, CheckedAt: b.CreatedAt, CheckedBy: "alice", Status: "ok", DocCount: &count,
		SampleDocID: "d1", CollectionURL: "https://a/collection/c", ObservedCollectionName: "文档",
	}); err != nil || !ok {
		t.Fatalf("SaveDocBindingCheck = %v, %v", ok, err)
	}

	// 只轮换 token 也必须清空旧的验证结果（旧结果对应旧连接）。
	actor2 := "bob"
	if err := PutDocBinding(ctx, db, testMasterKey, DocBinding{
		RepoID: 10, BaseURL: "https://a", CollectionID: "c", CreatedBy: "alice", UpdatedBy: &actor2,
	}, "token2"); err != nil {
		t.Fatal(err)
	}
	after, err := GetDocBinding(ctx, db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if after.CheckStatus != nil || after.CheckedAt != nil || after.CheckedBy != nil || after.CheckDocCount != nil ||
		after.CheckErrorCode != "" || after.CheckSampleDocID != "" || after.CollectionURL != "" || after.ObservedCollectionName != "" {
		t.Errorf("重绑 / 轮换 token 后应清空验证与观测字段：%+v", after)
	}
	if after.UpdatedBy == nil || *after.UpdatedBy != "bob" {
		t.Errorf("updated_by 应更新为 bob：%v", after.UpdatedBy)
	}
}

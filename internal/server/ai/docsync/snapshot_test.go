package docsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestManifestDigest：摘要的确定性编码：列表乱序相同、任一字段或来源域变化不同、none 与绑定空集合不同。
func TestManifestDigest(t *testing.T) {
	t.Parallel()
	docs := []Snapshot{
		{DocID: "b", Title: "乙", Revision: 2, SHA256: strings.Repeat("b", 64), URL: "u-b"},
		{DocID: "a", Title: "甲", Revision: 1, SHA256: strings.Repeat("a", 64), URL: "u-a"},
	}
	src := ManifestSource{Provider: "outline", BaseURL: "https://o.example", CollectionID: "c1", BindingGeneration: 3}
	base := ManifestDigest(SourceSnapshotSet, src, docs)
	if len(base) != sha256HexLen || !validDigest(base) {
		t.Fatalf("摘要应为 64 位小写 hex：%q", base)
	}
	if got := ManifestDigest(SourceSnapshotSet, src, []Snapshot{docs[1], docs[0]}); got != base {
		t.Fatal("文档列表乱序时摘要应相同")
	}
	if got := ManifestDigest(SourceSnapshotSet, ManifestSource{Provider: "outline", BaseURL: "https://o.example", CollectionID: "c1", BindingGeneration: 4}, docs); got == base {
		t.Fatal("绑定代次变化后摘要应不同（重绑后的依据不是同一份）")
	}
	for name, mut := range map[string]func([]Snapshot) []Snapshot{
		"标题": func(d []Snapshot) []Snapshot { d[0].Title = "改"; return d },
		"地址": func(d []Snapshot) []Snapshot { d[0].URL = "改"; return d },
		"修订": func(d []Snapshot) []Snapshot { d[0].Revision++; return d },
		"摘要": func(d []Snapshot) []Snapshot { d[0].SHA256 = strings.Repeat("c", 64); return d },
		"文档": func(d []Snapshot) []Snapshot { return append(d, Snapshot{DocID: "c"}) },
	} {
		cp := append([]Snapshot(nil), docs...)
		if got := ManifestDigest(SourceSnapshotSet, src, mut(cp)); got == base {
			t.Errorf("%s 变化后摘要应不同", name)
		}
	}
	if NoneManifestDigest() == ManifestDigest(SourceSnapshotSet, src, nil) {
		t.Fatal("none 与绑定空集合的摘要必须不同：前者是观察事实，后者是一次空冻结")
	}
	if NoneManifestDigest() == ManifestDigest(SourceNone, ManifestSource{Provider: "outline"}, nil) {
		t.Fatal("none 的摘要不应受来源域影响")
	}
	// 历史工作项的来源未知：即使集合相同，来源未知与已知的摘要也应不同。
	unknown := ManifestDigest(SourceWorkItem, ManifestSource{Unknown: true}, docs)
	if unknown == ManifestDigest(SourceWorkItem, src, docs) {
		t.Fatal("来源未知与已知的摘要应不同")
	}
}

// TestPrepareFreezeCommitSet：独立审计的冻结分两步：事务外拉取与校验，事务内写入快照集；列表按 (title, doc_id) 排序。
func TestPrepareFreezeCommitSet(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{
		docState("d2", "b", 1, "二"),
		docState("d1", "a", 3, "一"),
	}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 30)
	bind(t, db, 30)
	ctx := context.Background()

	prepared, err := svc.PrepareFreeze(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Bound || len(prepared.Docs) != 2 || prepared.ManifestDigest == "" {
		t.Fatalf("PrepareFreeze = %+v", prepared)
	}
	// 事务外不落库。
	var sets int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM doc_snapshot_sets`).Scan(&sets); err != nil || sets != 0 {
		t.Fatalf("PrepareFreeze 不应写库：%d, %v", sets, err)
	}
	var basis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		basis, err = svc.CommitSetTx(ctx, tx, 30, prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if basis.Source != SourceSnapshotSet || basis.SnapshotSetID == 0 || basis.DocCount != 2 || basis.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("CommitSetTx = %+v", basis)
	}
	docs, err := svc.ListBasis(ctx, db, basis)
	if err != nil || len(docs) != 2 {
		t.Fatalf("ListBasis = %+v, %v", docs, err)
	}
	if docs[0].DocID != "d1" || docs[1].DocID != "d2" {
		t.Fatalf("应按标题排序（a < b）：%+v", docs)
	}
	got, err := svc.GetBasis(ctx, db, basis, "d1")
	if err != nil || got == nil || got.Content != "一" || got.SHA256 != docprovider.Sum("一") {
		t.Fatalf("GetBasis = %+v, %v", got, err)
	}
	if missing, err := svc.GetBasis(ctx, db, basis, "nope"); err != nil || missing != nil {
		t.Fatalf("不存在的文档应为 nil,nil：%v %v", missing, err)
	}
	// 可读性核对通过。
	if err := db.Tx(ctx, func(tx pgx.Tx) error { return svc.RequireReadableTx(ctx, tx, 30, basis) }); err != nil {
		t.Fatalf("刚写入的快照集应可读：%v", err)
	}
}

// TestPrepareFreezeUnboundAndEmpty：没有绑定是 none（提交时不加绑定锁、不写快照集）；绑定空集合仍写入 doc_count=0 的快照集，
// 两种摘要不同。
func TestPrepareFreezeUnboundAndEmpty(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 31)
	ctx := context.Background()

	unbound, err := svc.PrepareFreeze(ctx, 31)
	if err != nil {
		t.Fatal(err)
	}
	if unbound.Bound || unbound.ManifestDigest != NoneManifestDigest() {
		t.Fatalf("没有绑定应为 none：%+v", unbound)
	}
	var noneBasis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		noneBasis, err = svc.CommitSetTx(ctx, tx, 31, unbound)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if noneBasis.Source != SourceNone || noneBasis.SnapshotSetID != 0 {
		t.Fatalf("未绑定的提交 = %+v", noneBasis)
	}

	bind(t, db, 31)
	empty, err := svc.PrepareFreeze(ctx, 31)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Bound || len(empty.Docs) != 0 || empty.ManifestDigest == NoneManifestDigest() {
		t.Fatalf("绑定空集合应是一次空冻结（摘要与 none 不同）：%+v", empty)
	}
	var emptyBasis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		emptyBasis, err = svc.CommitSetTx(ctx, tx, 31, empty)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if emptyBasis.Source != SourceSnapshotSet || emptyBasis.DocCount != 0 {
		t.Fatalf("空集合的提交 = %+v", emptyBasis)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT doc_count FROM doc_snapshot_sets WHERE id=$1`, emptyBasis.SnapshotSetID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("空集合的 doc_count = %d, %v", count, err)
	}
}

// TestFreezeRejectsInvalidDocuments：ID 不一致、跨集合、未发布 / 已归档 / 已删除、列表重复时整批失败，零写入。
func TestFreezeRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fp   *fakeProvider
	}{
		{"ID 不一致", &fakeProvider{docs: []docprovider.Document{docState("d1", "a", 1, "a")}, badID: true}},
		{"跨集合", &fakeProvider{docs: []docprovider.Document{{
			ID: "d1", Title: "a", Content: "a", CollectionID: "other", Published: true,
		}}}},
		{"未发布", &fakeProvider{docs: []docprovider.Document{{
			ID: "d1", Title: "a", Content: "a", CollectionID: testCollection,
		}}}},
		{"已归档", &fakeProvider{docs: []docprovider.Document{{
			ID: "d1", Title: "a", Content: "a", CollectionID: testCollection, Published: true, Archived: true,
		}}}},
		{"已删除", &fakeProvider{docs: []docprovider.Document{{
			ID: "d1", Title: "a", Content: "a", CollectionID: testCollection, Published: true, Deleted: true,
		}}}},
		{"列表重复", &fakeProvider{docs: []docprovider.Document{
			docState("d1", "a", 1, "a"), docState("d1", "b", 1, "b"),
		}}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := newService(t, c.fp)
			db := svc.db
			repoID := int64(40 + i)
			insertRepo(t, db, repoID)
			wi := insertWorkItem(t, db, repoID)
			bind(t, db, repoID)
			_, err := svc.Freeze(context.Background(), repoID, wi)
			if !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("应返回 ErrInvalidDocument，实际 %v", err)
			}
			var snaps, freezes int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, wi).Scan(&snaps); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM work_item_doc_freezes WHERE work_item_id=$1`, wi).Scan(&freezes); err != nil {
				t.Fatal(err)
			}
			if snaps != 0 || freezes != 0 {
				t.Fatalf("校验失败时不应写入：snapshots=%d freezes=%d", snaps, freezes)
			}
		})
	}
}

// TestFreezeBindingChangedDuringFetch：拉取期间重绑、解绑或解绑再重绑（ABA）时拒绝写入；绑定不变时正常写入。
func TestFreezeBindingChangedDuringFetch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(t *testing.T, db *store.DB, repoID int64)
	}{
		{"重绑", func(t *testing.T, db *store.DB, repoID int64) {
			t.Helper()
			_ = store.PutDocBinding(context.Background(), db, testKey, store.DocBinding{
				RepoID: repoID, Provider: "outline",
				BaseURL: "https://outline.example.com", CollectionID: "col2",
			}, "tok-2")
		}},
		{"解绑", func(t *testing.T, db *store.DB, repoID int64) {
			t.Helper()
			_, _ = store.DeleteDocBinding(context.Background(), db, repoID)
		}},
		{"解绑再重绑", func(t *testing.T, db *store.DB, repoID int64) {
			t.Helper()
			_, _ = store.DeleteDocBinding(context.Background(), db, repoID)
			_ = store.PutDocBinding(context.Background(), db, testKey, store.DocBinding{
				RepoID: repoID, Provider: "outline",
				BaseURL: "https://outline.example.com", CollectionID: "col1",
			}, "tok-3")
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := store.New(pgtestPool(t))
			repoID := int64(50 + i)
			insertRepo(t, db, repoID)
			bind(t, db, repoID)
			fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "a", 1, "a")}}
			fp.listHook = func() { c.change(t, db, repoID) }
			svc := New(Deps{DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil }})
			wi := insertWorkItem(t, db, repoID)
			_, err := svc.Freeze(context.Background(), repoID, wi)
			if !errors.Is(err, ErrBindingChanged) {
				t.Fatalf("应返回 ErrBindingChanged，实际 %v", err)
			}
			var n int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM work_item_doc_snapshots WHERE work_item_id=$1`, wi).Scan(&n); err != nil || n != 0 {
				t.Fatalf("绑定变化时不应写入：%d, %v", n, err)
			}
		})
	}
}

// TestCommitSetRejectsBindingChanged：独立审计的提交同样核对绑定；空批次（0 篇文档）也要核对，不能在重绑后写下一个空快照集。
func TestCommitSetRejectsBindingChanged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		docs []docprovider.Document
		// docCount 是成功提交时批次里的文档数（空批次也写一行 doc_count=0）。
		docCount int
	}{
		{"有文档", []docprovider.Document{docState("d1", "a", 1, "a")}, 1},
		{"空批次", nil, 0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := store.New(pgtestPool(t))
			repoID := int64(55 + i)
			insertRepo(t, db, repoID)
			bind(t, db, repoID)
			fp := &fakeProvider{docs: c.docs}
			svc := New(Deps{DB: db, MasterKey: testKey, Provider: func(string, string) (docprovider.Provider, error) { return fp, nil }})
			ctx := context.Background()
			prepared, err := svc.PrepareFreeze(ctx, repoID)
			if err != nil {
				t.Fatal(err)
			}
			if !prepared.Bound || len(prepared.Docs) != c.docCount {
				t.Fatalf("准备结果 = %+v", prepared)
			}
			// 拉取之后重绑：提交必须拒绝，且不写快照集。
			if err := store.PutDocBinding(ctx, db, testKey, store.DocBinding{
				RepoID: repoID, Provider: "outline",
				BaseURL: "https://outline.example.com", CollectionID: "col2",
			}, "tok-2"); err != nil {
				t.Fatal(err)
			}
			err = db.Tx(ctx, func(tx pgx.Tx) error {
				_, err := svc.CommitSetTx(ctx, tx, repoID, prepared)
				return err
			})
			if !errors.Is(err, ErrBindingChanged) {
				t.Fatalf("应返回 ErrBindingChanged，实际 %v", err)
			}
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM doc_snapshot_sets`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("拒绝时不应写快照集：%d, %v", n, err)
			}
		})
	}
}

// TestSnapshotOrdering：工作项快照与独立快照集都按 (title, doc_id) 稳定排序。标题顺序与 doc_id 顺序相反，且有两篇同名
// 不同 doc_id 的文档，并以与 doc_id 相反的顺序出现在数据源里：退回 ORDER BY doc_id 或只按标题排序都会失败。
func TestSnapshotOrdering(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{
		docState("d1", "z", 1, "z"),
		docState("d4", "m", 1, "m4"),
		docState("d2", "a", 1, "a"),
		docState("d3", "m", 1, "m3"),
	}}
	want := []string{"d2", "d3", "d4", "d1"} // a(d2)，m(d3)、m(d4)，z(d1)
	check := func(t *testing.T, where string, got []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s 有 %d 篇，应为 %d：%v", where, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s 顺序 = %v，应为 %v", where, got, want)
			}
		}
	}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 62)
	wi := insertWorkItem(t, db, 62)
	bind(t, db, 62)
	ctx := context.Background()

	if _, err := svc.Freeze(ctx, 62, wi); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, wi)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(list))
	for _, d := range list {
		ids = append(ids, d.DocID)
	}
	check(t, "工作项快照", ids)

	prepared, err := svc.PrepareFreeze(ctx, 62)
	if err != nil {
		t.Fatal(err)
	}
	var basis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		basis, err = svc.CommitSetTx(ctx, tx, 62, prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := svc.ListBasis(ctx, db, basis)
	if err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, d := range docs {
		ids = append(ids, d.DocID)
	}
	check(t, "独立快照集", ids)
}

// TestWorkItemBasisAndReadable：工作项未冻结是 none；冻结后是 work_item（历史摘要确定性计算）；正文清理后过期；
// 空正文（摘要为空正文的 sha）是合法的。
func TestWorkItemBasisAndReadable(t *testing.T) {
	t.Parallel()
	db := store.New(pgtestPool(t))
	insertRepo(t, db, 60)
	wi := insertWorkItem(t, db, 60)
	svc := New(Deps{DB: db, MasterKey: testKey})
	ctx := context.Background()

	b, err := svc.WorkItemBasis(ctx, db, 60, wi)
	if err != nil || b.Source != SourceNone || b.ManifestDigest != NoneManifestDigest() {
		t.Fatalf("未冻结的工作项 = %+v, %v", b, err)
	}
	// 历史冻结行（没有 manifest_digest 与来源列）：摘要按历史元数据确定性计算。
	emptySHA := docprovider.Sum("")
	if _, err := db.Exec(ctx, `INSERT INTO work_item_doc_snapshots (work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1, 'd2', 'b', 1, $2, '二', 'u2'), ($1, 'd1', 'a', 2, $3, '', 'u1')`, wi, docprovider.Sum("二"), emptySHA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 2)`, wi); err != nil {
		t.Fatal(err)
	}
	b, err = svc.WorkItemBasis(ctx, db, 60, wi)
	if err != nil || b.Source != SourceWorkItem || b.DocCount != 2 || !validDigest(b.ManifestDigest) {
		t.Fatalf("冻结后的工作项 = %+v, %v", b, err)
	}
	if len(b.Docs) != 2 || b.Docs[0].DocID != "d1" || b.Docs[1].DocID != "d2" {
		t.Fatalf("元数据应按 (title, doc_id) 排序：%+v", b.Docs)
	}
	if err := db.Tx(ctx, func(tx pgx.Tx) error { return svc.RequireReadableTx(ctx, tx, 60, b) }); err != nil {
		t.Fatalf("空正文（摘要为空正文的 sha）应可读：%v", err)
	}
	if got, err := svc.GetBasis(ctx, db, b, "d1"); err != nil || got == nil || got.Content != "" {
		t.Fatalf("空正文文档 = %+v, %v", got, err)
	}
	// 旧清理器留下的空正文（摘要不是空正文的 sha）视为过期，绝不伪造 content_purged_at。
	if _, err := db.Exec(ctx, `UPDATE work_item_doc_snapshots SET content='' WHERE work_item_id=$1 AND doc_id='d2'`, wi); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx pgx.Tx) error { return svc.RequireReadableTx(ctx, tx, 60, b) })
	if !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("正文被清空应返回 ErrSnapshotExpired，实际 %v", err)
	}
	if _, err := svc.GetBasis(ctx, db, b, "d2"); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("GetBasis 应返回 ErrSnapshotExpired，实际 %v", err)
	}
	var purged *string
	if err := db.QueryRow(ctx, `SELECT content_purged_at::text FROM work_item_doc_freezes WHERE work_item_id=$1`, wi).Scan(&purged); err != nil || purged != nil {
		t.Fatalf("不得伪造 content_purged_at：%v, %v", purged, err)
	}
}

// TestSnapshotSetExpiredAndOrdering：快照集正文清理后过期、空集合直接可读、元数据仍可列出。
func TestSnapshotSetExpiredAndOrdering(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{
		docState("d1", "a", 1, "x"),
		docState("d2", "b", 1, "y"),
	}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 61)
	bind(t, db, 61)
	ctx := context.Background()
	prepared, err := svc.PrepareFreeze(ctx, 61)
	if err != nil {
		t.Fatal(err)
	}
	var basis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		basis, err = svc.CommitSetTx(ctx, tx, 61, prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := svc.ListBasis(ctx, db, basis)
	if err != nil || len(docs) != 2 || docs[0].DocID != "d1" || docs[1].DocID != "d2" {
		t.Fatalf("应按标题字典序排序：%+v, %v", docs, err)
	}
	// 模拟保留清理：清空正文并标记。
	if _, err := db.Exec(ctx, `UPDATE doc_snapshot_docs SET content='' WHERE snapshot_set_id=$1`, basis.SnapshotSetID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE doc_snapshot_sets SET content_purged_at=now() WHERE id=$1`, basis.SnapshotSetID); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx pgx.Tx) error { return svc.RequireReadableTx(ctx, tx, 61, basis) })
	if !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("应返回 ErrSnapshotExpired，实际 %v", err)
	}
	if _, err := svc.GetBasis(ctx, db, basis, "d1"); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("GetBasis 应返回 ErrSnapshotExpired，实际 %v", err)
	}
	if list, err := svc.ListBasis(ctx, db, basis); err != nil || len(list) != 2 || list[0].Content != "" {
		t.Fatalf("正文清理后仍可列出元数据：%+v, %v", list, err)
	}
}

// pgtestPool 返回测试数据库的连接池（每次调用是一个独立数据库）。
func pgtestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.Pool(t)
}

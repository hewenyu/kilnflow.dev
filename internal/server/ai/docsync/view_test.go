package docsync

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
)

// 展示视图：只有元数据、不借用当前绑定补造历史来源、清理与合法空正文区分、按父对象读取正文。

func TestWorkItemSnapshotView(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{
		docState("d1", "甲", 3, "正文一"),
		docState("d2", "乙", 1, ""), // 合法的空文档：正文为空、摘要为空正文的摘要，不是清理
	}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 40)
	bind(t, db, 40)
	wi := insertWorkItem(t, db, 40)
	if _, err := svc.Freeze(context.Background(), 40, wi); err != nil {
		t.Fatal(err)
	}

	v, err := svc.WorkItemSnapshotView(context.Background(), db, 40, wi)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Frozen || v.Source != SourceWorkItem || v.SourceKnown != true {
		t.Fatalf("视图 = %+v", v)
	}
	if v.DocCount != 2 || len(v.Docs) != 2 || v.FrozenAt == nil || v.ManifestDigest == "" {
		t.Errorf("元数据不完整：%+v", v)
	}
	if v.CollectionID != testCollection || v.BindingGeneration == 0 || v.Provider != "outline" {
		t.Errorf("来源域 = %+v", v)
	}
	if v.IntegrityError != "" {
		t.Errorf("刚冻结的快照不应有完整性问题：%s", v.IntegrityError)
	}
	for _, d := range v.Docs {
		if d.Purged {
			t.Errorf("合法空文档不应算已清理：%+v", d)
		}
	}
	// 列表只有元数据（DocMeta 没有正文），且按标题排序。
	if v.Docs[0].Title != "乙" || v.Docs[1].Title != "甲" {
		t.Errorf("应按标题排序：%+v", v.Docs)
	}
}

func TestWorkItemSnapshotViewPurged(t *testing.T) {
	t.Parallel()
	svc := newService(t, &fakeProvider{})
	db := svc.db
	insertRepo(t, db, 41)
	wi := insertWorkItem(t, db, 41)
	ctx := context.Background()
	// 没有冻结时 Source=none、不伪造来源。
	v, err := svc.WorkItemSnapshotView(ctx, db, 41, wi)
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != SourceNone || v.Frozen || v.SourceKnown || len(v.Docs) != 0 {
		t.Fatalf("没有冻结时 = %+v", v)
	}
	// 旧的清理器只清正文、没有 content_purged_at：按"空正文 + 非空摘要"识别为已清理。
	if _, err := db.Exec(ctx, `INSERT INTO work_item_doc_snapshots
		(work_item_id, doc_id, title, revision, content_sha256, content, url)
		VALUES ($1,'d1','甲',1,$2,'','https://outline.example.com/doc/d1')`, wi, docprovider.Sum("正文")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO work_item_doc_freezes (work_item_id, doc_count) VALUES ($1, 1)`, wi); err != nil {
		t.Fatal(err)
	}
	v, err = svc.WorkItemSnapshotView(ctx, db, 41, wi)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Frozen || v.DocCount != 1 || len(v.Docs) != 1 || !v.Docs[0].Purged {
		t.Fatalf("旧清理留下的空正文应显示为已清理：%+v", v)
	}
	// 历史行没有来源：SourceKnown 为假，绝不借用当前绑定（这里也根本没有绑定）。
	if v.SourceKnown {
		t.Error("历史快照不应补造来源")
	}
	// 正文读取返回过期。
	if _, err := svc.Get(ctx, wi, "d1"); err == nil || !strings.Contains(err.Error(), ErrSnapshotExpired.Error()) {
		t.Errorf("清理后的正文应返回 ErrSnapshotExpired：%v", err)
	}
}

func TestSnapshotSetViewAndRead(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "甲", 2, "正文")}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 42)
	insertRepo(t, db, 43)
	bind(t, db, 42)
	ctx := context.Background()
	prepared, err := svc.PrepareFreeze(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	var basis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		basis, err = svc.CommitSetTx(ctx, tx, 42, prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v, err := svc.SnapshotSetView(ctx, db, 42, basis.SnapshotSetID)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || !v.Frozen || v.Source != SourceSnapshotSet || v.DocCount != 1 || len(v.Docs) != 1 {
		t.Fatalf("快照集视图 = %+v", v)
	}
	if !v.SourceKnown || v.Docs[0].Purged {
		t.Errorf("来源与清理状态 = %+v", v)
	}
	// 跨仓库不可读。
	if v, err := svc.SnapshotSetView(ctx, db, 43, basis.SnapshotSetID); err == nil || v != nil {
		t.Errorf("跨仓库读取快照集应报错：%+v, %v", v, err)
	}
	if _, err := svc.ReadSetDoc(ctx, db, 43, basis.SnapshotSetID, "d1"); err == nil {
		t.Error("跨仓库读取正文应报错")
	}
	d, err := svc.ReadSetDoc(ctx, db, 42, basis.SnapshotSetID, "d1")
	if err != nil || d == nil || d.Content != "正文" {
		t.Fatalf("ReadSetDoc = %+v, %v", d, err)
	}
	if d, err := svc.ReadSetDoc(ctx, db, 42, basis.SnapshotSetID, "other"); err != nil || d != nil {
		t.Errorf("不存在的文档应返回 nil：%+v, %v", d, err)
	}
	// 不存在的快照集返回 nil。
	if v, err := svc.SnapshotSetView(ctx, db, 42, basis.SnapshotSetID+1000); err != nil || v != nil {
		t.Errorf("不存在的快照集应返回 nil：%+v, %v", v, err)
	}
}

func TestSnapshotSetViewPurged(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "甲", 2, "正文")}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 44)
	bind(t, db, 44)
	ctx := context.Background()
	prepared, err := svc.PrepareFreeze(ctx, 44)
	if err != nil {
		t.Fatal(err)
	}
	var setID int64
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		b, err := svc.CommitSetTx(ctx, tx, 44, prepared)
		setID = b.SnapshotSetID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE doc_snapshot_docs SET content='' WHERE snapshot_set_id=$1`, setID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE doc_snapshot_sets SET content_purged_at=now() WHERE id=$1`, setID); err != nil {
		t.Fatal(err)
	}
	v, err := svc.SnapshotSetView(ctx, db, 44, setID)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || v.ContentPurgedAt == nil || len(v.Docs) != 1 || !v.Docs[0].Purged {
		t.Fatalf("清理后的视图 = %+v", v)
	}
	if _, err := svc.ReadSetDoc(ctx, db, 44, setID, "d1"); err == nil || !strings.Contains(err.Error(), ErrSnapshotExpired.Error()) {
		t.Errorf("清理后的正文应返回 ErrSnapshotExpired：%v", err)
	}
}

func TestWorkItemSnapshotViewIntegrity(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{docs: []docprovider.Document{docState("d1", "甲", 1, "正文")}}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 45)
	bind(t, db, 45)
	wi := insertWorkItem(t, db, 45)
	ctx := context.Background()
	if _, err := svc.Freeze(ctx, 45, wi); err != nil {
		t.Fatal(err)
	}
	// 记录的篇数与实际行数不一致（例如清理器或人工改库）：明确标记，而不是静默当成正常。
	if _, err := db.Exec(ctx, `UPDATE work_item_doc_freezes SET doc_count=5 WHERE work_item_id=$1`, wi); err != nil {
		t.Fatal(err)
	}
	v, err := svc.WorkItemSnapshotView(ctx, db, 45, wi)
	if err != nil {
		t.Fatal(err)
	}
	if i := v.Integrity; i == nil || i.Code != IntegrityCountMismatch || i.Count != 5 || i.Actual != len(v.Docs) {
		t.Fatalf("篇数不一致应标记完整性问题（原因码和两个篇数）：%+v", v.Integrity)
	}
	if want := fmt.Sprintf("记录的篇数（5）与文档行数（%d）不一致", len(v.Docs)); v.IntegrityError != want {
		t.Errorf("中文说明 = %q，应为 %q", v.IntegrityError, want)
	}
}

// TestSnapshotSetViewEmptySetIntegrity：空集合（doc_count=0）的整批摘要同样核对——摘要被改坏时明确标记完整性问题，
// 而不是因为"没有文档"跳过核对；合法的空集合没有完整性问题。
func TestSnapshotSetViewEmptySetIntegrity(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{} // 空集合
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 46)
	bind(t, db, 46)
	ctx := context.Background()
	prepared, err := svc.PrepareFreeze(ctx, 46)
	if err != nil {
		t.Fatal(err)
	}
	var basis Basis
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		basis, err = svc.CommitSetTx(ctx, tx, 46, prepared)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v, err := svc.SnapshotSetView(ctx, db, 46, basis.SnapshotSetID)
	if err != nil {
		t.Fatal(err)
	}
	if v.DocCount != 0 || v.IntegrityError != "" {
		t.Fatalf("合法的空集合不应有完整性问题：%+v", v)
	}
	if v.ManifestDigest != basis.ManifestDigest {
		t.Fatalf("空集合的摘要 = %s，应为冻结时的 %s", v.ManifestDigest, basis.ManifestDigest)
	}
	// 摘要被改坏（清理器或人工改库）：空集合也必须报出来。
	// 改成另一个合法的 64 位摘要（数据库的格式约束仍然满足）：内容与元数据对不上。
	if _, err := db.Exec(ctx, `UPDATE doc_snapshot_sets SET manifest_digest=repeat('0', 64) WHERE id=$1`, basis.SnapshotSetID); err != nil {
		t.Fatal(err)
	}
	v, err = svc.SnapshotSetView(ctx, db, 46, basis.SnapshotSetID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Integrity == nil || v.Integrity.Code != IntegrityManifestMismatch || !strings.Contains(v.IntegrityError, "整批摘要") {
		t.Fatalf("空集合的摘要被改坏时应标记整批摘要的问题：%+v", v)
	}
}

// TestWorkItemSnapshotViewEmptySetIntegrity：工作项冻结的空集合同样核对整批摘要，摘要被改坏时标记完整性问题。
func TestWorkItemSnapshotViewEmptySetIntegrity(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	svc := newService(t, fp)
	db := svc.db
	insertRepo(t, db, 47)
	bind(t, db, 47)
	wi := insertWorkItem(t, db, 47)
	ctx := context.Background()
	if _, err := svc.Freeze(ctx, 47, wi); err != nil {
		t.Fatal(err)
	}
	v, err := svc.WorkItemSnapshotView(ctx, db, 47, wi)
	if err != nil {
		t.Fatal(err)
	}
	if v.DocCount != 0 || v.IntegrityError != "" {
		t.Fatalf("合法的空集合不应有完整性问题：%+v", v)
	}
	if _, err := db.Exec(ctx, `UPDATE work_item_doc_freezes SET manifest_digest=NULL WHERE work_item_id=$1`, wi); err != nil {
		t.Fatal(err)
	}
	v, err = svc.WorkItemSnapshotView(ctx, db, 47, wi)
	if err != nil {
		t.Fatal(err)
	}
	if v.ManifestDigest == "" || v.IntegrityError != "" {
		t.Fatalf("工作项旧记录（没有摘要）按历史元数据计算，不算完整性问题：%+v", v)
	}
}

// TestCheckViewIntegrity：每种完整性问题给出自己的原因码和参数（页面按原因码显示），中文说明（CLI、日志）与原因码一致。
func TestCheckViewIntegrity(t *testing.T) {
	t.Parallel()
	good := &SnapshotView{
		Provider: "outline", BaseURL: "https://outline.example.com", CollectionID: "c", SourceKnown: true,
		Docs: []DocMeta{{DocID: "d1", Title: "甲", Revision: 1, SHA256: docprovider.Sum("正文")}},
	}
	good.ManifestDigest = ManifestDigest(SourceSnapshotSet, good.manifestSource(), viewSnapshots(good.Docs))
	if i := checkViewIntegrity(SourceSnapshotSet, 1, good); i != nil {
		t.Fatalf("一致的快照不应有问题：%+v", i)
	}
	badDoc := *good
	badDoc.Docs = []DocMeta{{DocID: "d1", SHA256: "x"}}
	noManifest := *good
	noManifest.ManifestDigest = ""
	otherManifest := *good
	otherManifest.ManifestDigest = strings.Repeat("0", 64)
	cases := []struct {
		v     *SnapshotView
		count int
		want  IntegrityIssue
		zh    string
	}{
		{good, 3, IntegrityIssue{Code: IntegrityCountMismatch, Count: 3, Actual: 1}, "记录的篇数（3）与文档行数（1）不一致"},
		{&badDoc, 1, IntegrityIssue{Code: IntegrityDocDigestInvalid, DocID: "d1"}, "文档 d1 的内容摘要不合法"},
		{&noManifest, 1, IntegrityIssue{Code: IntegrityManifestInvalid}, "文档快照的整批摘要缺失或不合法"},
		{&otherManifest, 1, IntegrityIssue{Code: IntegrityManifestMismatch}, "文档快照的整批摘要与记录的元数据不一致"},
	}
	for _, c := range cases {
		got := checkViewIntegrity(SourceSnapshotSet, c.count, c.v)
		if got == nil || *got != c.want {
			t.Errorf("%s：问题 = %+v，应为 %+v", c.want.Code, got, c.want)
			continue
		}
		if got.String() != c.zh {
			t.Errorf("%s：中文说明 = %q，应为 %q", c.want.Code, got.String(), c.zh)
		}
	}
}

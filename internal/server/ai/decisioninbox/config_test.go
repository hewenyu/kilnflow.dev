package decisioninbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 配置设置 / 清除与人工对账。

const testDocB = "22222222-2222-4222-8222-222222222222"

func docWith(id string) docprovider.Document {
	d := newDoc()
	d.ID = id
	return d
}

func TestSetDecisionDocTokenRotationKeepsQueue(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	pendingID := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	svc := newService(t, db, &fakeWriter{doc: docWith(testDocID)})

	res, err := svc.SetDecisionDoc(t.Context(), testRepoID, testBaseURL, testDocID, "new-token", "admin")
	if err != nil {
		t.Fatalf("SetDecisionDoc：%v", err)
	}
	if res.Cancelled != 0 || res.Unknown != 0 {
		t.Errorf("只轮换 token 不应取消条目：%+v", res)
	}
	if res.Doc.Generation <= cfg.Generation {
		t.Errorf("代次应推进：%d → %d", cfg.Generation, res.Doc.Generation)
	}
	if v := mustSync(t, db, pendingID); v.State != "pending" {
		t.Errorf("pending 条目应保留：%+v", v)
	}
}

func TestSetDecisionDocRotationEndsOldEntries(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	pendingID := insertSync(t, db, syncSeed{EntryIdx: 0, TargetGen: cfg.Generation})
	attemptedID := insertSync(t, db, syncSeed{EntryIdx: 1, TargetGen: cfg.Generation, State: "attempted", PayloadMD: "块"})
	deliveredID := insertSync(t, db, syncSeed{EntryIdx: 2, TargetGen: cfg.Generation, State: "delivered"})
	svc := newService(t, db, &fakeWriter{doc: docWith(testDocB)})

	res, err := svc.SetDecisionDoc(t.Context(), testRepoID, testBaseURL, testDocB, "tok", "admin")
	if err != nil {
		t.Fatalf("SetDecisionDoc：%v", err)
	}
	if res.Cancelled != 1 || res.Unknown != 1 {
		t.Errorf("轮转计数 = %d / %d，期望 1 / 1", res.Cancelled, res.Unknown)
	}
	if v := mustSync(t, db, pendingID); v.State != "cancelled" || v.LastErrorCode != "target_rotated" {
		t.Errorf("pending = %+v", v)
	}
	if v := mustSync(t, db, attemptedID); v.State != "unknown" || v.LastErrorCode != "target_rotated_while_attempted" {
		t.Errorf("attempted = %+v", v)
	}
	if v := mustSync(t, db, deliveredID); v.State != "delivered" {
		t.Errorf("delivered 不应改变：%+v", v)
	}
}

func TestSetDecisionDocValidation(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	for name, args := range map[string][3]string{
		"非法 UUID": {"", testBaseURL, "tok"},
		"空 token": {"11111111-1111-4111-8111-111111111111", testBaseURL, ""},
		"非法地址":    {"11111111-1111-4111-8111-111111111111", "ftp://x", "tok"},
	} {
		if _, err := svc.SetDecisionDoc(t.Context(), testRepoID, args[1], args[0], args[2], "admin"); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
	// 未给出地址且没有只读绑定时要求显式提供。
	if _, err := svc.SetDecisionDoc(t.Context(), testRepoID, "", testDocID, "tok", "admin"); err == nil {
		t.Error("没有地址也没有只读绑定时应报错")
	}
}

func TestSetDecisionDocRejectsReadonlyCollection(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	// 地址的大小写、默认端口和结尾斜杠不同，规范化后仍是同一个站点 / 集合。
	if err := store.PutDocBinding(t.Context(), db, testMasterKey, store.DocBinding{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://OUTLINE.example.com:443/", CollectionID: testCollection, CreatedBy: "admin",
	}, "read-token"); err != nil {
		t.Fatal(err)
	}
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	_, err := svc.SetDecisionDoc(t.Context(), testRepoID, testBaseURL, testDocID, "tok", "admin")
	if err == nil || !strings.Contains(err.Error(), "只读文档绑定") {
		t.Fatalf("应拒绝与只读绑定落同一集合：%v", err)
	}
	// 没有写入配置。
	if d, _ := store.GetDecisionDoc(t.Context(), db, testRepoID); d != nil {
		t.Errorf("冲突时不应保存配置：%+v", d)
	}
}

func TestClearDecisionDocEndsEntries(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	pendingID := insertSync(t, db, syncSeed{EntryIdx: 0, TargetGen: cfg.Generation})
	attemptedID := insertSync(t, db, syncSeed{EntryIdx: 1, TargetGen: cfg.Generation, State: "attempted", PayloadMD: "块"})
	deliveredID := insertSync(t, db, syncSeed{EntryIdx: 2, TargetGen: cfg.Generation, State: "delivered"})
	svc := newService(t, db, &fakeWriter{doc: newDoc()})

	res, err := svc.ClearDecisionDoc(t.Context(), testRepoID, "admin")
	if err != nil {
		t.Fatalf("ClearDecisionDoc：%v", err)
	}
	if res.Cancelled != 1 || res.Unknown != 1 {
		t.Errorf("清除计数 = %d / %d", res.Cancelled, res.Unknown)
	}
	if v := mustSync(t, db, pendingID); v.State != "cancelled" || v.LastErrorCode != "config_cleared" {
		t.Errorf("pending = %+v", v)
	}
	if v := mustSync(t, db, attemptedID); v.State != "unknown" || v.LastErrorCode != "config_cleared_while_attempted" {
		t.Errorf("attempted = %+v", v)
	}
	if v := mustSync(t, db, deliveredID); v.State != "delivered" {
		t.Errorf("delivered 不应改变：%+v", v)
	}
	if d, _ := store.GetDecisionDoc(t.Context(), db, testRepoID); d != nil {
		t.Error("配置应已删除")
	}
	if _, err := svc.ClearDecisionDoc(t.Context(), testRepoID, "admin"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("重复清除 = %v，期望 ErrNotConfigured", err)
	}
}

func TestReconcileSync(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)

	// 文档里有 marker：转 delivered。
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	doc := newDoc()
	doc.Content += "`" + marker + "`\n"
	w := &fakeWriter{doc: doc}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, State: "unknown"})
	res, err := svc.ReconcileSync(t.Context(), id, "admin")
	if err != nil {
		t.Fatalf("ReconcileSync：%v", err)
	}
	if !res.Found || res.Sync.State != "delivered" {
		t.Errorf("结果 = %+v / %+v", res, res.Sync)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("复查绝不能写文档，实际 %d 次", sets)
	}

	// 文档里没有 marker：保持 unknown。
	w2 := &fakeWriter{doc: newDoc()}
	svc2 := newService(t, db, w2)
	id2 := insertSync(t, db, syncSeed{EntryIdx: 1, TargetGen: cfg.Generation, State: "unknown"})
	res2, err := svc2.ReconcileSync(t.Context(), id2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Found || res2.Sync.State != "unknown" {
		t.Errorf("没有 marker 时应保持 unknown：%+v", res2.Sync)
	}

	// 非 unknown / attempted 不允许复查。
	if _, err := svc2.ReconcileSync(t.Context(), id, "admin"); err == nil {
		t.Error("delivered 不应允许复查")
	}
}

func TestReconcileSyncNeedsMatchingConfig(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	// 条目的目标是另一篇文档，当前配置无法提供它的 token。
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetDocID: testDocB, State: "unknown"})
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	if _, err := svc.ReconcileSync(t.Context(), id, "admin"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v，期望 ErrNoToken", err)
	}
}

func TestResolveSync(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	id := insertSync(t, db, syncSeed{State: "unknown"})

	if _, err := svc.ResolveSync(t.Context(), id, "delivered", "", "admin"); err == nil {
		t.Error("没有原因时不应结案")
	}
	v, err := svc.ResolveSync(t.Context(), id, "delivered", "人工确认已送达", "alice")
	if err != nil {
		t.Fatalf("ResolveSync：%v", err)
	}
	if v.State != "delivered" || v.ResolvedBy != "alice" || v.ResolutionNote != "人工确认已送达" {
		t.Errorf("结果 = %+v", v)
	}
	if v.DeliveredAt == nil {
		t.Error("人工结案为 delivered 应补上 delivered_at")
	}
	// 已结案的条目不能再结案。
	if _, err := svc.ResolveSync(t.Context(), id, "cancelled", "再改一次", "admin"); err == nil {
		t.Error("非 unknown 的条目不应允许结案")
	}
	// 只允许 delivered / cancelled。
	id2 := insertSync(t, db, syncSeed{EntryIdx: 1, State: "unknown"})
	if _, err := svc.ResolveSync(t.Context(), id2, "pending", "无效状态", "admin"); err == nil {
		t.Error("非法目标状态应报错")
	}
	if v, err := svc.ResolveSync(t.Context(), id2, "cancelled", "确认不需要", "admin"); err != nil || v.State != "cancelled" {
		t.Errorf("结案为 cancelled = %+v, %v", v, err)
	}
}

// 同一篇文档用含大写字母的 UUID 重设（只轮换 token）不能误判为换文档：否则 pending 会被取消、attempted 转 unknown。
func TestSetDecisionDocUppercaseUUIDIsTokenRotation(t *testing.T) {
	t.Parallel()
	const mixed = "9F1C2F7E-1A2B-4C3D-8E9F-0A1B2C3D4E5F"
	lower := strings.ToLower(mixed)
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, lower)
	pendingID := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, TargetDocID: lower})
	svc := newService(t, db, &fakeWriter{doc: docWith(lower)})

	res, err := svc.SetDecisionDoc(t.Context(), testRepoID, testBaseURL, mixed, "new-token", "admin")
	if err != nil {
		t.Fatalf("SetDecisionDoc：%v", err)
	}
	if res.Cancelled != 0 || res.Unknown != 0 {
		t.Errorf("大写 UUID 只是换个写法，不应取消条目：%+v", res)
	}
	if res.Doc.DocID != lower {
		t.Errorf("文档 id 应统一存小写：%q", res.Doc.DocID)
	}
	if res.Doc.Generation <= cfg.Generation {
		t.Errorf("只轮换 token 时代次应推进：%d → %d", cfg.Generation, res.Doc.Generation)
	}
	if v := mustSync(t, db, pendingID); v.State != "pending" {
		t.Errorf("pending 条目应保留：%+v", v)
	}
}

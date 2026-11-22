package decisioninbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// C19 状态机与投递：断言 UpdateDocument 的实际调用次数和远端最终正文，不只断言没有报错。

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	return store.New(pgtest.Pool(t))
}

func mustSync(t *testing.T, db *store.DB, id int64) *Sync {
	t.Helper()
	v, err := GetSync(t.Context(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestProcessPendingSuccess(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	gets, sets := w.calls()
	if sets != 1 {
		t.Errorf("UpdateDocument 调用 %d 次，期望 1 次", sets)
	}
	if gets != 1 {
		t.Errorf("GetDocument 调用 %d 次，期望 1 次", gets)
	}
	v := mustSync(t, db, id)
	if v.State != "delivered" || v.Attempts != 1 {
		t.Errorf("状态 = %+v", v)
	}
	if !strings.Contains(w.doc.Content, v.Marker) {
		t.Errorf("远端正文应包含 marker：%q", w.doc.Content)
	}
}

func TestProcessPendingAlreadyHasMarker(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	doc := newDoc()
	doc.Content += "`" + marker + "`\n"
	w := &fakeWriter{doc: doc}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("正文已有 marker 时不应写，实际调用 %d 次", sets)
	}
	if v := mustSync(t, db, id); v.State != "delivered" {
		t.Errorf("状态 = %+v", v)
	}
}

func TestProcessPendingConflictMergesWebEdit(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(f *fakeWriter, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		if f.sets < 3 { // 前两次模拟网页并发编辑导致 CAS 冲突
			f.doc.Content += "\n用户网页编辑"
			f.doc.Revision++
			return docprovider.Document{}, writeErr(docprovider.EffectNotApplied, 409, "document_conflict")
		}
		f.doc.Content = req.Text
		f.doc.Revision++
		return f.doc, nil
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 3 {
		t.Errorf("UpdateDocument 调用 %d 次，期望 3 次（两次冲突后成功）", sets)
	}
	if v := mustSync(t, db, id); v.State != "delivered" {
		t.Fatalf("状态 = %+v", v)
	}
	if !strings.Contains(w.doc.Content, "用户网页编辑") {
		t.Error("冲突重试不应丢掉网页上的修改")
	}
	if !strings.Contains(w.doc.Content, mustSync(t, db, id).Marker) {
		t.Error("最终正文应包含条目 marker")
	}
}

func TestProcessPendingConflictExhausted(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(f *fakeWriter, _ docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		f.doc.Revision++
		return docprovider.Document{}, writeErr(docprovider.EffectNotApplied, 409, "document_conflict")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != conflictRetries {
		t.Errorf("UpdateDocument 调用 %d 次，期望有界重试 %d 次", sets, conflictRetries)
	}
	v := mustSync(t, db, id)
	if v.State != "pending" || v.LastErrorCode != "document_conflict" {
		t.Errorf("状态 = %+v，期望回到 pending 等下一轮", v)
	}
	var future bool
	if err := db.QueryRow(t.Context(), `SELECT next_retry_at > now() FROM dev_decision_syncs WHERE id=$1`, id).Scan(&future); err != nil {
		t.Fatal(err)
	}
	if !future {
		t.Error("冲突用尽后应退避，而不是立即重试")
	}
}

func TestProcessPendingRateLimited(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, &docprovider.WriteError{
			Effect: docprovider.EffectNotApplied, Retryable: true, Status: 429,
			Code: "rate_limit_exceeded", RetryAfter: 90 * time.Second,
		}
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 1 {
		t.Errorf("限流后本轮不应再写，实际 %d 次", sets)
	}
	v := mustSync(t, db, id)
	if v.State != "pending" || v.LastErrorCode != "rate_limited" {
		t.Errorf("状态 = %+v", v)
	}
	var wait float64
	if err := db.QueryRow(t.Context(), `SELECT extract(epoch FROM next_retry_at - now()) FROM dev_decision_syncs WHERE id=$1`, id).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 80 || wait > 100 {
		t.Errorf("应等待约 90 秒，实际 %.1f", wait)
	}
}

func TestProcessPendingPermanentRejection(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, writeErr(docprovider.EffectNotApplied, 403, "forbidden")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 1 {
		t.Errorf("永久拒绝不应重写，实际 %d 次", sets)
	}
	v := mustSync(t, db, id)
	if v.State != "cancelled" || v.LastErrorCode != "write_rejected" {
		t.Errorf("状态 = %+v", v)
	}
}

func TestProcessPendingUncertainButWritten(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(f *fakeWriter, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		// 远端其实写成功了，但响应丢了。
		f.doc.Content = req.Text
		f.doc.Revision++
		return docprovider.Document{}, writeErr(docprovider.EffectMayHaveApplied, 503, "")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 1 {
		t.Errorf("结果不明时绝不能重写，实际 %d 次", sets)
	}
	v := mustSync(t, db, id)
	if v.State != "delivered" {
		t.Errorf("查证到 marker 后应记为 delivered：%+v", v)
	}
}

func TestProcessPendingUncertainDeletedByUser(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.put = func(f *fakeWriter, _ docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		// 远端写了又被人删掉：查证时找不到 marker。
		f.doc.Content = "## 待拍板\n\n"
		f.doc.Revision++
		return docprovider.Document{}, writeErr(docprovider.EffectMayHaveApplied, 502, "")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 1 {
		t.Errorf("查证没有 marker 时也不能再写，实际 %d 次", sets)
	}
	v := mustSync(t, db, id)
	if v.State != "unknown" || v.LastErrorCode != "update_outcome_unknown" {
		t.Errorf("状态 = %+v，期望 unknown", v)
	}
}

func TestProcessPendingVerifyTemporaryFailureKeepsAttempted(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.get = func(f *fakeWriter) (docprovider.Document, error) {
		if f.sets == 0 {
			return f.doc, nil
		}
		return docprovider.Document{}, errors.New("网络中断")
	}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, writeErr(docprovider.EffectMayHaveApplied, 504, "")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	v := mustSync(t, db, id)
	if v.State != "attempted" {
		t.Errorf("查证临时失败应保持 attempted：%+v", v)
	}
	var future bool
	if err := db.QueryRow(t.Context(), `SELECT next_retry_at > now() FROM dev_decision_syncs WHERE id=$1`, id).Scan(&future); err != nil {
		t.Fatal(err)
	}
	if !future {
		t.Error("应退避后只读重试")
	}
}

func TestProcessPendingVerifyPermanentFailureUnknown(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	w.get = func(f *fakeWriter) (docprovider.Document, error) {
		if f.sets == 0 {
			return f.doc, nil
		}
		return docprovider.Document{}, &docprovider.Error{Status: 403}
	}
	w.put = func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
		return docprovider.Document{}, writeErr(docprovider.EffectMayHaveApplied, 500, "")
	}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if v := mustSync(t, db, id); v.State != "unknown" {
		t.Errorf("查证被永久拒绝应转 unknown：%+v", v)
	}
}

func TestProcessAttemptedRecoveryReadOnly(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)

	marker := Marker(testRepoID, testWorkItemID, testTaskID, 0)
	doc := newDoc()
	doc.Content += "`" + marker + "`\n"
	w := &fakeWriter{doc: doc}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, State: "attempted", PayloadMD: "块"})

	// 崩溃恢复：attempted 只读查证，绝不重发。
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("attempted 只读查证不应写，实际 %d 次", sets)
	}
	if v := mustSync(t, db, id); v.State != "delivered" {
		t.Errorf("找到 marker 应转 delivered：%+v", v)
	}
}

func TestProcessAttemptedNotFoundBecomesUnknown(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, State: "attempted", PayloadMD: "块"})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("attempted 不应写，实际 %d 次", sets)
	}
	if v := mustSync(t, db, id); v.State != "unknown" {
		t.Errorf("没有 marker 应转 unknown：%+v", v)
	}
}

func TestProcessAttemptedConfigClearedUnknown(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: 1, State: "attempted", PayloadMD: "块"})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if g, s := w.calls(); g != 0 || s != 0 {
		t.Errorf("配置已清除时不应访问文档，get=%d set=%d", g, s)
	}
	if v := mustSync(t, db, id); v.State != "unknown" {
		t.Errorf("配置清除后 attempted 应转 unknown：%+v", v)
	}
}

func TestDeliveredIsTerminal(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, State: "delivered"})

	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if _, sets := w.calls(); sets != 0 {
		t.Errorf("delivered 是终态，不应再处理，实际 %d 次", sets)
	}
	if v := mustSync(t, db, id); v.State != "delivered" {
		t.Errorf("状态 = %+v", v)
	}
}

func TestPRBackfillOnlyWhenSameRound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)

	// 同一阶段、同一轮：采用当前 PR 号。
	if _, err := db.Exec(t.Context(), `UPDATE dev_work_items SET pr_number=99 WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	w := &fakeWriter{doc: newDoc()}
	svc := newService(t, db, w)
	id := insertSync(t, db, syncSeed{TargetGen: cfg.Generation})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	var md string
	if err := db.QueryRow(t.Context(), `SELECT payload_md FROM dev_decision_syncs WHERE id=$1`, id).Scan(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "PR #99") {
		t.Errorf("冻结的正文应带上当前 PR 号：\n%s", md)
	}

	// 旧条目（已冻结）不会误挂之后的新 PR。
	if _, err := db.Exec(t.Context(), `UPDATE dev_work_items SET pr_number=100, impl_round=2 WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT payload_md FROM dev_decision_syncs WHERE id=$1`, id).Scan(&md); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(md, "PR #100") {
		t.Error("已冻结的旧条目不应改挂新的 PR")
	}

	// 轮次不同：首次冻结也不能采用新的 PR 号。
	id2 := insertSync(t, db, syncSeed{TargetGen: cfg.Generation, EntryIdx: 1})
	if err := svc.ProcessRepo(t.Context(), testRepoID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT payload_md FROM dev_decision_syncs WHERE id=$1`, id2).Scan(&md); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(md, "PR #100") || !strings.Contains(md, "尚未创建") {
		t.Errorf("轮次不同时不应采用新的 PR 号：\n%s", md)
	}
}

// ---- 入队规则 ----

func deliveryWith(decisions []devreport.Decision, status string) *devreport.Delivery {
	return &devreport.Delivery{
		Version: devreport.Version, Status: status, Summary: "概述",
		Design: devreport.Design{Decisions: decisions},
	}
}

func TestEntriesOrderingAndBlockedRules(t *testing.T) {
	t.Parallel()
	d := devreport.Decision{Category: "other", Context: "上下文", Choice: "选择"}
	rep := deliveryWith([]devreport.Decision{d, d}, devreport.StatusDelivered)
	base := ReportInput{
		RepoID: 42, RepoFullName: "alice/demo", WorkItemID: 7, TaskID: 2,
		Phase: "implement", ImplRound: 1, Report: rep, ReportStatus: devreport.StatusDelivered, TaskOutcome: "succeeded",
	}

	got := entries(&base, time.Now())
	if len(got) != 2 || got[0].EntryKind != EntryDecision || got[1].EntryKind != EntryDecision {
		t.Fatalf("delivered 两条决策 = %+v", got)
	}

	// blocked：即使没有决策也有一条摘要，且排在最后。
	rep2 := deliveryWith(nil, devreport.StatusBlocked)
	in := base
	in.Report = rep2
	in.ReportStatus = devreport.StatusBlocked
	got = entries(&in, time.Now())
	if len(got) != 1 || got[0].EntryKind != EntryBlocked {
		t.Fatalf("blocked 无决策 = %+v", got)
	}

	// 初次实现的 no_change 是正常终态：不加受阻摘要。
	rep3 := deliveryWith(nil, devreport.StatusNoChange)
	in = base
	in.Report = rep3
	in.ReportStatus = devreport.StatusNoChange
	if got = entries(&in, time.Now()); len(got) != 0 {
		t.Errorf("初次实现 no_change 不应有受阻摘要：%+v", got)
	}
	// 返修 / 设计阶段的 no_change 实际转 blocked。
	in.Fix = true
	if got = entries(&in, time.Now()); len(got) != 1 || got[0].EntryKind != EntryBlocked {
		t.Errorf("返修 no_change 应有受阻摘要：%+v", got)
	}
	in.Fix = false
	in.Phase = devreport.PhaseDesign
	if got = entries(&in, time.Now()); len(got) != 1 || got[0].EntryKind != EntryBlocked {
		t.Errorf("设计阶段 no_change 应有受阻摘要：%+v", got)
	}
}

// ---- EnqueueReportTx ----

func TestEnqueueWithoutConfigIsNoop(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	insertFixture(t, db)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	in := ReportInput{
		RepoID: 42, RepoFullName: "alice/demo", WorkItemID: 7, TaskID: 2,
		Phase: "implement", ImplRound: 1, ReportStatus: "delivered", TaskOutcome: "succeeded",
		Raw: []byte(`{}`), Report: deliveryWith([]devreport.Decision{{Category: "other"}}, devreport.StatusDelivered),
	}

	var inserted bool
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		var err error
		inserted, err = svc.EnqueueReportTx(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Error("没有配置时不应入队")
	}
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM dev_decision_syncs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("没有配置时不应建意图，实际 %d 条", n)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("没有配置时不应写唤醒记录，实际 %d 条", n)
	}
}

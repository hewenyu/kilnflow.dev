package decisioninbox

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 入队：与交付报告同事务登记意图，没有配置时零写入，已存在的条目不覆盖。

func enqueueTx(t *testing.T, db *store.DB, svc *Service, in ReportInput) bool {
	t.Helper()
	var inserted bool
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		var err error
		inserted, err = svc.EnqueueReportTx(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("EnqueueReportTx：%v", err)
	}
	return inserted
}

func reportInput() ReportInput {
	return ReportInput{
		RepoID: testRepoID, RepoFullName: "alice/demo", WorkItemID: testWorkItemID, TaskID: testTaskID,
		Phase: "implement", ImplRound: 1, Issues: []int{3, 4},
		ReportStatus: devreport.StatusDelivered, TaskOutcome: "succeeded",
		Raw:    []byte(`{"version":2,"status":"delivered"}`),
		Report: deliveryWith([]devreport.Decision{{Category: "other", Context: "一"}, {Category: "policy", Context: "二"}}, devreport.StatusDelivered),
	}
}

func TestEnqueueWithConfig(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	cfg := putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})

	if !enqueueTx(t, db, svc, reportInput()) {
		t.Fatal("应插入条目")
	}
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM dev_decision_syncs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("条目数 = %d，期望 2", n)
	}
	// 顺序稳定：entry_idx 对应原数组下标。
	var idx int
	if err := db.QueryRow(t.Context(), `SELECT entry_idx FROM dev_decision_syncs ORDER BY entry_idx LIMIT 1`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Errorf("第一个条目 entry_idx = %d", idx)
	}
	// payload 记录报告状态、任务结果、目标代次。
	var status, outcome string
	var gen int64
	if err := db.QueryRow(t.Context(), `SELECT payload->>'report_status', payload->>'task_outcome', target_generation
		FROM dev_decision_syncs ORDER BY entry_idx LIMIT 1`).Scan(&status, &outcome, &gen); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || outcome != "succeeded" || gen != cfg.Generation {
		t.Errorf("payload = %s / %s / %d", status, outcome, gen)
	}
	// 同一事务写入唤醒记录。
	var kind string
	if err := db.QueryRow(t.Context(), `SELECT kind FROM outbox WHERE dedupe_key=$1`, "decision-inbox:42").Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != KindWake {
		t.Errorf("唤醒记录类型 = %s", kind)
	}
}

func TestEnqueueIdempotentAndDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})

	in := reportInput()
	if !enqueueTx(t, db, svc, in) {
		t.Fatal("第一次应插入")
	}
	// 模拟其中一个条目已经送达。
	if _, err := db.Exec(t.Context(), `UPDATE dev_decision_syncs SET state='delivered', delivered_revision=3,
		delivered_at=now(), next_retry_at=NULL WHERE entry_idx=0`); err != nil {
		t.Fatal(err)
	}
	if enqueueTx(t, db, svc, in) {
		t.Error("重复入队不应插入新行")
	}
	var state string
	var n int
	if err := db.QueryRow(t.Context(), `SELECT state FROM dev_decision_syncs WHERE entry_idx=0`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" {
		t.Errorf("已存在的条目状态不应被覆盖：%s", state)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM dev_decision_syncs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("条目数 = %d，期望 2", n)
	}
}

func TestEnqueueRejectsStaleTask(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	// 工作项已经不属于这个任务（设计 PR 合并、开始了新轮次）。
	if _, err := db.Exec(t.Context(), `UPDATE dev_work_items SET task_id=NULL WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if enqueueTx(t, db, svc, reportInput()) {
		t.Error("stale task 的报告不应入队")
	}
	var n int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM dev_decision_syncs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("不应有意图，实际 %d 条", n)
	}
}

func TestEnqueueFailedOutcomeWithValidReport(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	in := reportInput()
	in.TaskOutcome = "failed"
	if !enqueueTx(t, db, svc, in) {
		t.Error("执行结果失败但报告有效时仍应入队")
	}
	var outcome string
	if err := db.QueryRow(t.Context(), `SELECT payload->>'task_outcome' FROM dev_decision_syncs LIMIT 1`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" {
		t.Errorf("payload 应记录 task_outcome=failed：%s", outcome)
	}
}

func TestEnqueueBlockedAddsSummary(t *testing.T) {
	t.Parallel()
	db := store.New(pgtest.Pool(t))
	insertFixture(t, db)
	putConfig(t, db, testDocID)
	svc := newService(t, db, &fakeWriter{doc: newDoc()})
	in := reportInput()
	in.ReportStatus = devreport.StatusBlocked
	in.Report = &devreport.Delivery{
		Version: devreport.Version, Status: devreport.StatusBlocked, Summary: "缺条件",
		Handoff: devreport.Handoff{Remaining: "等凭据", Next: "拿到后继续"},
	}
	if !enqueueTx(t, db, svc, in) {
		t.Fatal("应入队")
	}
	var kind, category string
	var confirmed bool
	if err := db.QueryRow(t.Context(), `SELECT entry_kind, coalesce(category,''), confirmed_decision
		FROM dev_decision_syncs ORDER BY entry_idx LIMIT 1`).Scan(&kind, &category, &confirmed); err != nil {
		t.Fatal(err)
	}
	if kind != "blocked" || category != "" || confirmed {
		t.Errorf("受阻摘要 = %s / %q / %t", kind, category, confirmed)
	}
	var summary string
	if err := db.QueryRow(t.Context(), `SELECT payload->>'summary' FROM dev_decision_syncs LIMIT 1`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "缺条件" {
		t.Errorf("summary = %s", summary)
	}
}

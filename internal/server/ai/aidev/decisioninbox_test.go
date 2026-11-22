package aidev

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 「待决策清单」与交付的集成：登记意图与交付报告在同一个结束任务的事务里，没有配置时零副作用，
// 登记失败会让整个 RecordResult 回滚。

var decisionKey = bytes.Repeat([]byte{9}, 32)

const decisionDocID = "33333333-3333-4333-8333-333333333333"

// docWriter 是固定返回一篇可写文档的假写接口。
type docWriter struct{ sets int }

func (w *docWriter) GetDocument(context.Context, string) (docprovider.Document, error) {
	return docprovider.Document{
		ID: decisionDocID, Title: "待拍板", Content: "## 待拍板\n\n", Revision: 4,
		CollectionID: "col-decision", Published: true,
	}, nil
}

func (w *docWriter) UpdateDocument(_ context.Context, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
	w.sets++
	return docprovider.Document{ID: decisionDocID, Content: req.Text, Revision: req.LastRevision + 1, CollectionID: "col-decision", Published: true}, nil
}

// decisionService 组装真实的同步服务（只用到入队，不跑处理循环）。
func (h *harness) decisionService() (*decisioninbox.Service, *docWriter) {
	w := &docWriter{}
	svc := decisioninbox.New(decisioninbox.Deps{
		DB: h.db, MasterKey: decisionKey, Log: slog.New(slog.DiscardHandler),
		Writer: func(string, string) (docprovider.DocumentWriter, error) { return w, nil },
	})
	h.svc.SetDecisionInbox(svc)
	return svc, w
}

// wantDecisionDoc 给仓库配置「待决策清单」。
func (h *harness) wantDecisionDoc() {
	h.t.Helper()
	if err := store.PutDecisionDoc(h.ctx, h.db, decisionKey, store.DecisionDoc{
		RepoID: testRepoID, Provider: "outline", BaseURL: "https://outline.example.com", DocID: decisionDocID,
		CollectionID: "col-decision", CreatedBy: "admin", UpdatedBy: "admin",
	}, "write-token"); err != nil {
		h.t.Fatal(err)
	}
}

// deliverWith 在 delivered 报告上追加决策记录。
func deliverWith(issues []int, decisions ...devreport.Decision) []byte {
	d := devreport.Delivery{Version: devreport.Version, Status: devreport.StatusDelivered, Summary: "实现了", PRTitle: "实现功能"}
	for _, n := range issues {
		d.Issues = append(d.Issues, devreport.IssueResult{
			Number: n, Resolution: devreport.ResolutionFull,
			Acceptance: []devreport.AcceptanceItem{{Item: "能用", Done: true, Evidence: "单元测试"}},
		})
	}
	d.Verification = []devreport.Verification{{Command: "go test ./...", Outcome: devreport.OutcomePassed}}
	d.Design.Decisions = decisions
	return devreport.Marshal(&d)
}

// TestRecordResultEnqueuesDecisions：配置了「待决策清单」时，交付报告中的决策随结束任务的事务登记为同步意图，
// entry_idx 与报告中决策的顺序一致，并唤醒投递 worker。
func TestRecordResultEnqueuesDecisions(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	h.decisionService()
	h.wantDecisionDoc()

	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	res := h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"feature.go": "package demo\n"})
	report := deliverWith([]int{1},
		devreport.Decision{Category: devreport.DecisionPolicy, Context: "一", Choice: "选 A"},
		devreport.Decision{Category: devreport.DecisionConfirmed, Ref: "C20", Context: "二", Choice: "选 B"},
	)
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: report, Branch: devreport.AttemptBranch(aid), SHA: sha})

	w := h.wantState(res.WorkItemID, StateDelivering)
	if n := h.queryInt(`SELECT count(*) FROM dev_decision_syncs WHERE repo_id=$1 AND work_item_id=$2 AND task_id=$3`,
		testRepoID, w.ID, w.TaskID); n != 2 {
		t.Fatalf("同步意图 = %d 条，期望 2", n)
	}
	var first, second string
	if err := h.db.QueryRow(h.ctx, `SELECT marker FROM dev_decision_syncs WHERE entry_idx=0`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(h.ctx, `SELECT marker FROM dev_decision_syncs WHERE entry_idx=1`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first == second || first != decisioninbox.Marker(int64(testRepoID), w.ID, *w.TaskID, 0) {
		t.Errorf("marker = %s / %s", first, second)
	}
	if !h.queryBool(`SELECT confirmed_decision FROM dev_decision_syncs WHERE entry_idx=1`) {
		t.Error("confirmed_decision 应标记需要人拍板的决策")
	}
	if h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1`, decisioninbox.KindWake) != 1 {
		t.Error("应唤醒投递 worker")
	}
}

// TestRecordResultWithoutDecisionDoc：没有配置「待决策清单」时不登记任何意图，原有的交付流程不变。
func TestRecordResultWithoutDecisionDoc(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	h.decisionService()

	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"feature.go": "package demo\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{
		Report: deliverWith([]int{1}, devreport.Decision{Category: devreport.DecisionOther, Context: "一", Choice: "选 A"}),
		Branch: devreport.AttemptBranch(aid), SHA: sha,
	})
	if n := h.queryInt(`SELECT count(*) FROM dev_decision_syncs`); n != 0 {
		t.Errorf("没有配置时不应有意图，实际 %d 条", n)
	}
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1`, decisioninbox.KindWake); n != 0 {
		t.Errorf("没有配置时不应唤醒，实际 %d 条", n)
	}
}

// failingInbox 是总是失败的同步接口。
type failingInbox struct{}

func (failingInbox) EnqueueReportTx(context.Context, pgx.Tx, decisioninbox.ReportInput) (bool, error) {
	return false, errors.New("登记失败")
}

// TestRecordResultDecisionFailureRollsBack：登记意图失败时整个结束任务的事务回滚：交付报告、工作项推进都不生效。
func TestRecordResultDecisionFailureRollsBack(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	h.svc.SetDecisionInbox(failingInbox{})

	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	res := h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"feature.go": "package demo\n"})
	err := h.core.FinishAttempt(h.ctx, aid, ai.Result{
		Outcome: store.OutcomeSucceeded, Message: "结果说明",
		Delivery: &airunnerv2.DevDelivery{Report: deliverWith([]int{1}), Branch: devreport.AttemptBranch(aid), Sha: sha},
	})
	if err == nil {
		t.Fatal("登记失败时应返回错误")
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_reports WHERE kind='delivery'`); n != 0 {
		t.Errorf("事务回滚后不应留下交付报告，实际 %d 条", n)
	}
	if w := h.workItem(res.WorkItemID); w.State != StateDeveloping {
		t.Errorf("事务回滚后工作项应保持 %s：%s", StateDeveloping, w.State)
	}
	if n := h.queryInt(`SELECT count(*) FROM dev_decision_syncs`); n != 0 {
		t.Errorf("不应有意图，实际 %d 条", n)
	}
}

// TestSaveReportDeliveryNotOverwritten：同一任务的交付报告第一次写入后不能被另一份覆盖；内容相同时幂等返回。
func TestSaveReportDeliveryNotOverwritten(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	res := h.mustClaim(aid, []int{1})
	first := deliverWith([]int{1})
	second := deliverWith([]int{1}, devreport.Decision{Category: devreport.DecisionOther, Context: "改主意", Choice: "换"})
	taskID := h.taskOfAttempt(aid).ID
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		if err := saveReport(h.ctx, tx, taskID, aid, reportDelivery, &res.WorkItemID, first, ""); err != nil {
			return err
		}
		// 相同内容重放：幂等。
		return saveReport(h.ctx, tx, taskID, aid, reportDelivery, &res.WorkItemID, first, "")
	}); err != nil {
		t.Fatalf("第一次写入：%v", err)
	}
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		return saveReport(h.ctx, tx, taskID, aid, reportDelivery, &res.WorkItemID, second, "")
	})
	if err == nil {
		t.Fatal("不同的交付报告不应被静默覆盖")
	}
	// 报告存在 jsonb 中会被规范化，按 jsonb 相等比较。
	var storedFirst, storedSecond bool
	if qerr := h.db.QueryRow(h.ctx, `SELECT report = $3::jsonb, report = $4::jsonb FROM dev_reports WHERE task_id=$1 AND kind=$2`,
		taskID, reportDelivery, first, second).Scan(&storedFirst, &storedSecond); qerr != nil {
		t.Fatal(qerr)
	}
	if !storedFirst || storedSecond {
		t.Error("原有交付报告不应被改动")
	}
}

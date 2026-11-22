package decisioninbox

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 测试共用：独立数据库、最小数据构造、可编程的假文档写接口。

var testMasterKey = bytes.Repeat([]byte{7}, 32)

const (
	testRepoID     int64 = 42
	testWorkItemID int64 = 7
	testTaskID     int64 = 2
	testDocID            = "11111111-1111-4111-8111-111111111111"
	testBaseURL          = "https://outline.example.com"
	testCollection       = "col-decision"
)

func newService(t *testing.T, db *store.DB, w docprovider.DocumentWriter) *Service {
	t.Helper()
	return New(Deps{
		DB: db, MasterKey: testMasterKey,
		Log: slog.New(slog.DiscardHandler),
		Writer: func(string, string) (docprovider.DocumentWriter, error) {
			return w, nil
		},
	})
}

// insertFixture 写入一条最小可用的仓库 / AI 请求 / 任务 / attempt / 工作项。
func insertFixture(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	for _, sql := range []string{
		`INSERT INTO repos (id, installation_id, owner_id, owner, name, default_branch, clone_url)
		 VALUES (42, 1, 1, 'alice', 'demo', 'main', '')`,
		`INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
		 VALUES (1, 'develop', 42, 'manual', 'refs/heads/main', 'a', 1, 'succeeded')`,
		`INSERT INTO ai_tasks (id, request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		 VALUES (2, 1, 'develop', 'develop', '{macos-arm64}', 'codex', '开发', '{}', 'succeeded', 60)`,
		`INSERT INTO ai_attempts (id, task_id, epoch, runner_name, acquire_request_id) VALUES (1, 2, 1, 'node', 'acq1')`,
		`INSERT INTO dev_work_items (id, repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id,
			task_id, phase, impl_round) VALUES (7, 42, 'developing', 'main', 'a', 2, 'codex', 1, 2, 'implement', 1)`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("准备数据 %q：%v", sql, err)
		}
	}
}

// putConfig 写入仓库的「待决策清单」配置。
func putConfig(t *testing.T, db *store.DB, docID string) *store.DecisionDoc {
	t.Helper()
	if err := store.PutDecisionDoc(t.Context(), db, testMasterKey, store.DecisionDoc{
		RepoID: testRepoID, Provider: "outline", BaseURL: testBaseURL, DocID: docID,
		CollectionID: testCollection, CreatedBy: "admin", UpdatedBy: "admin",
	}, "write-token"); err != nil {
		t.Fatalf("PutDecisionDoc：%v", err)
	}
	d, err := store.GetDecisionDoc(t.Context(), db, testRepoID)
	if err != nil || d == nil {
		t.Fatalf("GetDecisionDoc = %+v, %v", d, err)
	}
	return d
}

// syncSeed 描述要直接写入的一条同步记录。
type syncSeed struct {
	EntryIdx      int
	EntryKind     EntryKind
	Category      string
	State         string
	PayloadMD     string
	TargetBaseURL string
	TargetDocID   string
	TargetGen     int64
	TargetCollID  string
	Marker        string
}

// insertSync 直接写入一条同步记录（绕过入队，用于构造处理状态）。
func insertSync(t *testing.T, db *store.DB, s syncSeed) int64 {
	t.Helper()
	if s.TargetBaseURL == "" {
		s.TargetBaseURL = testBaseURL
	}
	if s.TargetDocID == "" {
		s.TargetDocID = testDocID
	}
	if s.TargetCollID == "" {
		s.TargetCollID = testCollection
	}
	if s.TargetGen == 0 {
		s.TargetGen = 1
	}
	if s.EntryKind == "" {
		s.EntryKind = EntryDecision
	}
	if s.EntryKind == EntryDecision && s.Category == "" {
		s.Category = "other"
	}
	if s.Marker == "" {
		s.Marker = Marker(testRepoID, testWorkItemID, testTaskID, s.EntryIdx)
	}
	if s.State == "" {
		s.State = "pending"
	}
	if s.PayloadMD == "" && attemptsFor(s.State) > 0 {
		// 尝试过的条目一定有冻结的正文。
		s.PayloadMD = "冻结的正文"
	}
	p := Payload{
		SchemaVersion: 1, AcceptedAt: time.Now(), Repo: "alice/demo", WorkItemID: testWorkItemID,
		TaskID: testTaskID, Phase: "implement", ImplRound: 1, EntryKind: s.EntryKind,
		Category: s.Category, Context: "上下文", Choice: "选择", ReportStatus: "delivered", TaskOutcome: "succeeded",
	}
	payload, _ := json.Marshal(p)
	var category *string
	if s.Category != "" {
		category = &s.Category
	}
	var id int64
	err := db.QueryRow(t.Context(), `INSERT INTO dev_decision_syncs (repo_id, work_item_id, task_id, entry_idx,
			target_provider, target_base_url, target_doc_id, target_collection_id, target_generation, marker,
			entry_kind, category, report_sha256, payload, state, attempts, payload_md, attempted_revision,
			attempted_at, delivered_at, next_retry_at)
		VALUES ($1,$2,$3,$4,'outline',$5,$6::uuid,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id`,
		testRepoID, testWorkItemID, testTaskID, s.EntryIdx, s.TargetBaseURL, s.TargetDocID, s.TargetCollID,
		s.TargetGen, s.Marker, string(s.EntryKind), category, reportSHA(payload), payload, s.State, attemptsFor(s.State),
		nullable(s.PayloadMD), revisionFor(s.State), attemptTime(s.State), deliveredTime(s.State), retryFor(s.State)).Scan(&id)
	if err != nil {
		t.Fatalf("插入同步记录：%v", err)
	}
	return id
}

func reportSHA(payload []byte) string {
	sum, err := ReportSHA256(payload)
	if err != nil {
		panic(err)
	}
	return sum
}

func attemptsFor(state string) int {
	if state == "attempted" || state == "unknown" {
		return 1
	}
	return 0
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func revisionFor(state string) *int64 {
	if state == "pending" {
		return nil
	}
	r := int64(3)
	return &r
}

func attemptTime(state string) *time.Time {
	if state == "pending" {
		return nil
	}
	t := time.Now()
	return &t
}

func deliveredTime(state string) *time.Time {
	if state != "delivered" {
		return nil
	}
	t := time.Now()
	return &t
}

func retryFor(state string) *time.Time {
	if state == "pending" || state == "attempted" {
		t := time.Now()
		return &t
	}
	return nil
}

// fakeWriter 是可编程的文档写接口：记录调用次数，按需模拟远端行为。
type fakeWriter struct {
	mu   sync.Mutex
	doc  docprovider.Document
	get  func(*fakeWriter) (docprovider.Document, error)
	put  func(*fakeWriter, docprovider.UpdateDocumentRequest) (docprovider.Document, error)
	gets int
	sets int
	last docprovider.UpdateDocumentRequest
}

func (f *fakeWriter) GetDocument(context.Context, string) (docprovider.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.get != nil {
		return f.get(f)
	}
	return f.doc, nil
}

func (f *fakeWriter) UpdateDocument(_ context.Context, req docprovider.UpdateDocumentRequest) (docprovider.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.last = req
	if f.put != nil {
		return f.put(f, req)
	}
	f.doc.Content = req.Text
	f.doc.Revision++
	return f.doc, nil
}

func (f *fakeWriter) calls() (gets, sets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.sets
}

// newDoc 返回一篇可写的目标文档。
func newDoc() docprovider.Document {
	return docprovider.Document{
		ID: testDocID, Title: "待拍板", Content: "## 待拍板\n\n", Revision: 5,
		CollectionID: testCollection, Published: true,
	}
}

// writeErr 构造一个 WriteError。
func writeErr(effect docprovider.Effect, status int, code string) error {
	return &docprovider.WriteError{Effect: effect, Status: status, Code: code, Message: "远端说明"}
}

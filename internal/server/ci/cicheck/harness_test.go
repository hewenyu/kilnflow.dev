package cicheck

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// ---------- 测试环境 ----------
//
// harness 由真实的 Postgres（pgtest）和模拟的 GitHub（fakegithub）组成。run 和 job 直接写入数据库，
// 测试同步投递到期的记录（outbox.Worker.DeliverDue），不依赖后台循环和等待。

const (
	// testRepoID 是测试仓库 tester/demo 的 ID（默认 installation）。
	testRepoID = 101
	// testSHA 是 run 的 commit。
	testSHA = "1111111111111111111111111111111111111111"
	// testPublicURL 是 Dashboard 的地址（Check Run 的 details_url）。
	testPublicURL = "https://kiln.example"
)

// testKey 是全部测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

type harness struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	fake   *fakegithub.Server
	logs   *logstore.Store
	hub    *notify.Hub
	writer *outbox.Writer
	worker *outbox.Worker
	syncer *Syncer

	runs atomic.Int64
}

// newHarness 创建测试环境；withGitHub 为 false 时模拟没有配置 GitHub App。
func newHarness(t *testing.T, withGitHub bool) *harness {
	t.Helper()
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), hub: notify.NewHub()}
	var err error
	if h.logs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	h.fake = fakegithub.New("tester")
	t.Cleanup(h.fake.Close)

	var client *gh.Client
	if withGitHub {
		client = &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: h.fake.URL, UploadURL: h.fake.URL}
	}
	h.writer = outbox.NewWriter(withGitHub)
	h.worker = outbox.NewWorker(outbox.Deps{DB: h.db, Hub: h.hub, Log: slog.New(slog.DiscardHandler)})
	h.syncer = New(Config{AppID: h.fake.AppID, PublicURL: testPublicURL}, Deps{DB: h.db, GitHub: client, Logs: h.logs})
	h.syncer.Register(h.worker)
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url) VALUES ($1, $2, 9001, 'tester', 'demo', 'x')`,
		testRepoID, h.fake.InstallationID)
	return h
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// runSpec 是 insertRun 的参数。
type runSpec struct {
	State       store.RunState
	Conclusion  string // run_reruns.conclusion；空表示按 State 计算
	ConfigError string
	Warnings    string
	Event       string
}

// insertRun 插入一个 ci.yml 的 run 和它的 rerun 0，返回 run ID。
func (h *harness) insertRun(s runSpec) int64 {
	h.t.Helper()
	if s.State == "" {
		s.State = store.RunActive
	}
	if s.Event == "" {
		s.Event = "push"
	}
	conclusion := s.State.ConclusionOrNil()
	if s.Conclusion != "" {
		conclusion = &s.Conclusion
	}
	var id int64
	err := h.db.QueryRow(h.ctx, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state, config_error, warnings)
		VALUES ($1, 'ci.yml', $2, 'refs/heads/main', $3, 'webhook', $4, $5, nullif($6,''), nullif($7,'')) RETURNING id`,
		testRepoID, s.Event, testSHA, fmt.Sprintf("run-%d", h.runs.Add(1)), s.State, s.ConfigError, s.Warnings).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO run_reruns (run_id, rerun, state, conclusion, finished_at) VALUES ($1, 0, $2, $3, CASE WHEN $4 THEN now() END)`,
		id, s.State, conclusion, s.State.Terminal())
	return id
}

// jobSpec 是 insertJob 的参数。
type jobSpec struct {
	Key, Display string
	Kind         workflow.JobKind // 默认 normal
	State        store.JobState
	Reason       string
	ActionKey    string
	AttemptID    int64 // current_attempt_id（日志）
	Started      bool
	CachedFrom   int64
	ReusedFrom   int64
}

// insertJob 插入 run 的 rerun 0 中的一个 job，返回 job。
func (h *harness) insertJob(runID int64, s jobSpec) *cistore.Job {
	h.t.Helper()
	if s.Key == "" {
		s.Key = "build"
	}
	if s.Display == "" {
		s.Display = s.Key
	}
	if s.State == "" {
		s.State = store.JobPending
	}
	if s.Kind == "" {
		s.Kind = workflow.JobKindNormal
	}
	null := func(v int64) *int64 {
		if v == 0 {
			return nil
		}
		return &v
	}
	var id int64
	err := h.db.QueryRow(h.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind,
		state, reason, action_key, current_attempt_id, cached_from_job_id, reused_from_job_id, started_at, finished_at)
		VALUES ($12, $1, 0, $2, $2, $3, '{}', 'linux/amd64', 'host', $4, nullif($5,''), nullif($6,''), $7, $8, $9,
		CASE WHEN $10 THEN now() END, CASE WHEN $11 THEN now() END) RETURNING id`,
		runID, s.Key, s.Display, s.State, s.Reason, s.ActionKey, null(s.AttemptID), null(s.CachedFrom), null(s.ReusedFrom),
		s.Started || s.State == store.JobRunning, s.State.Terminal(), s.Kind).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.job(id)
}

func (h *harness) job(id int64) *cistore.Job {
	h.t.Helper()
	j, err := cistore.GetJob(h.ctx, h.db, id, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

// setJobState 修改 job 的状态（模拟状态机推进）。
func (h *harness) setJobState(id int64, state store.JobState) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state=$2, started_at=CASE WHEN $2 <> 'pending' THEN coalesce(started_at, now()) END,
		finished_at=CASE WHEN $3 THEN now() END WHERE id=$1`, id, state, state.Terminal())
}

// tx 在事务中执行 f。
func (h *harness) tx(f func(tx pgx.Tx) error) {
	h.t.Helper()
	if err := h.db.Tx(h.ctx, f); err != nil {
		h.t.Fatal(err)
	}
}

// enqueueJob 写入 job 的 Check Run 同步记录（同时写入 run 的汇总）。
func (h *harness) enqueueJob(id int64) {
	h.t.Helper()
	j := h.job(id)
	h.tx(func(tx pgx.Tx) error { return EnqueueJob(h.ctx, h.writer, tx, j) })
}

// enqueueRun 写入 run 的汇总 Check Run 同步记录。
func (h *harness) enqueueRun(runID int64) {
	h.t.Helper()
	h.tx(func(tx pgx.Tx) error { return EnqueueRun(h.ctx, h.writer, tx, runID, 0) })
}

// deliver 同步投递全部到期的记录（与 Run 中的一轮相同）。
func (h *harness) deliver() {
	h.t.Helper()
	h.worker.DeliverDue(h.ctx)
}

// outboxRow 是 outbox 表中的一条记录。
type outboxRow struct {
	ID          int64
	Kind        string
	Payload     string
	State       string
	Attempts    int
	LastError   string
	NextAttempt time.Time
}

// row 返回 dedupe_key 为 key 的记录；没有时 ok 为 false。
func (h *harness) row(key string) (outboxRow, bool) {
	h.t.Helper()
	var r outboxRow
	err := h.db.QueryRow(h.ctx, `SELECT id, kind, payload::text, state, attempts, coalesce(last_error,''), next_attempt_at FROM outbox WHERE dedupe_key=$1`, key).
		Scan(&r.ID, &r.Kind, &r.Payload, &r.State, &r.Attempts, &r.LastError, &r.NextAttempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return r, true
}

// mustRow 与 row 相同，没有记录时终止测试。
func (h *harness) mustRow(key string) outboxRow {
	h.t.Helper()
	r, ok := h.row(key)
	if !ok {
		h.t.Fatalf("没有 outbox 记录 %s", key)
	}
	return r
}

// count 返回 outbox 的记录数。
func (h *harness) count() int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// checkOutput 是模拟 GitHub 记录的 Check Run 的 output。
type checkOutput struct {
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Annotations []gh.Annotation `json:"annotations"`
}

// check 返回 external_id 为 externalID 的 Check Run 及其 output；没有或有多个时终止测试。
func (h *harness) check(externalID string) (fakegithub.CheckRun, checkOutput) {
	h.t.Helper()
	var found []fakegithub.CheckRun
	for _, c := range h.fake.CheckRuns(testSHA) {
		if c.ExternalID == externalID {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		h.t.Fatalf("external_id %s 的 Check Run 有 %d 个，期望 1 个：%+v", externalID, len(found), h.fake.CheckRuns(testSHA))
	}
	var out checkOutput
	if len(found[0].Output) > 0 {
		if err := json.Unmarshal(found[0].Output, &out); err != nil {
			h.t.Fatal(err)
		}
	}
	return found[0], out
}

// jobKey、runKey 返回 job / run 的 Check Run 同步记录的 dedupe_key。
func jobKey(id int64) string    { return fmt.Sprintf("checkrun:job:%d", id) }
func runKey(runID int64) string { return fmt.Sprintf("checkrun:run:%d:0", runID) }

// ---------- Writer ----------

// TestWriterEnqueue：job 的同步记录同时写入 run 的汇总记录；没有配置 GitHub App、发布等没有 run 的 job、
// 直接沿用的 job 不写入（沿用的 job 不新建 Check Run，PLAN #6.6）。
func TestWriterEnqueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	j := h.insertJob(runID, jobSpec{})
	reused := h.insertJob(runID, jobSpec{Key: "reused", State: store.JobSucceeded, ReusedFrom: j.ID})

	h.tx(func(tx pgx.Tx) error {
		if err := EnqueueJob(h.ctx, outbox.NewWriter(false), tx, j); err != nil {
			return err
		}
		if err := EnqueueRun(h.ctx, outbox.NewWriter(false), tx, runID, 0); err != nil {
			return err
		}
		if err := EnqueueJob(h.ctx, h.writer, tx, &cistore.Job{ID: 999}); err != nil {
			return err
		}
		return EnqueueJob(h.ctx, h.writer, tx, reused)
	})
	if n := h.count(); n != 0 {
		t.Fatalf("不应写入记录，实际 %d 条", n)
	}

	h.enqueueJob(j.ID)
	jr, rr := h.mustRow(jobKey(j.ID)), h.mustRow(runKey(runID))
	if jr.Kind != kindJobCheck || jr.Payload != fmt.Sprintf(`{"job_id": %d}`, j.ID) || jr.State != store.OutboxPending {
		t.Fatalf("job 的记录 = %+v", jr)
	}
	if rr.Kind != kindRunCheck || rr.Payload != fmt.Sprintf(`{"run_id": %d}`, runID) {
		t.Fatalf("run 的记录 = %+v", rr)
	}
}

// TestDeliverWithoutGitHubApp：没有配置 GitHub App 时 Check Run 记录直接视为投递成功。
func TestDeliverWithoutGitHubApp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	runID := h.insertRun(runSpec{})
	h.exec(`INSERT INTO outbox (dedupe_key, kind, payload) VALUES ($1, $2, $3)`, runKey(runID), kindRunCheck, store.MustJSON(checkPayload{RunID: runID}))
	h.deliver()
	if r := h.mustRow(runKey(runID)); r.State != store.OutboxDone {
		t.Fatalf("记录 = %+v，期望 done", r)
	}
	if reqs := h.fake.Requests(); len(reqs) > 0 {
		t.Fatalf("不应请求 GitHub：%v", reqs)
	}
}

// TestDeliverInvalidItems：无法解析的 payload 和不存在的对象按普通失败重试并记录原因。
func TestDeliverInvalidItems(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	h.exec(`INSERT INTO outbox (dedupe_key, kind, payload) VALUES ('k2', $1, '"x"'), ('k3', $2, '{"job_id": 987654}'), ('k4', $1, '{"run_id": 987654}')`,
		kindRunCheck, kindJobCheck)
	h.deliver()
	tests := []struct{ key, wantErr string }{
		{"k2", "payload 无法解析"},
		{"k3", "no rows"},
		{"k4", "no rows"},
	}
	for _, tt := range tests {
		r := h.mustRow(tt.key)
		if r.State != store.OutboxPending || r.Attempts != 1 || !strings.Contains(r.LastError, tt.wantErr) {
			t.Errorf("%s = %+v，期望重试并记录 %q", tt.key, r, tt.wantErr)
		}
	}
}

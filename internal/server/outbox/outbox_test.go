package outbox

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// ---------- 测试环境 ----------
//
// harness 由真实的 Postgres（pgtest）和模拟的 GitHub（fakegithub）组成，注册一个测试用的记录类型（testKind），
// 测试直接调用一次投递批次（deliverBatch），不依赖后台循环和等待。

// testSHA 是测试 Check Run 的 commit。
const testSHA = "1111111111111111111111111111111111111111"

// appKey 是全部测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var appKey = sync.OnceValue(func() *rsa.PrivateKey {
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
	client *gh.Client
	hub    *notify.Hub
	writer *Writer
	worker *Worker

	// intercept 在请求到达模拟 GitHub 之前拦截：返回 true 表示已经写出响应（用于模拟故障）。
	interceptMu sync.Mutex
	intercept   func(w http.ResponseWriter, r *http.Request) bool

	items atomic.Int64
}

// newHarness 创建测试环境。
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), hub: notify.NewHub()}
	h.fake = fakegithub.New("tester")
	t.Cleanup(h.fake.Close)
	target, err := url.Parse(h.fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.interceptMu.Lock()
		f := h.intercept
		h.interceptMu.Unlock()
		if f != nil && f(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)

	h.client = &gh.Client{AppID: h.fake.AppID, Key: appKey(), APIURL: api.URL, UploadURL: api.URL}
	h.writer = NewWriter(true)
	h.worker = NewWorker(Deps{DB: h.db, Hub: h.hub, Log: slog.New(slog.DiscardHandler)})
	h.worker.Handle(testKind, h.handleTest)
	return h
}

// setIntercept 设置请求拦截（nil 表示取消）。
func (h *harness) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.interceptMu.Lock()
	h.intercept = f
	h.interceptMu.Unlock()
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// tx 在事务中执行 f。
func (h *harness) tx(f func(tx pgx.Tx) error) {
	h.t.Helper()
	if err := h.db.Tx(h.ctx, f); err != nil {
		h.t.Fatal(err)
	}
}

// testKind 是测试用的记录类型：投递时为 payload 中的 N 创建一个 Check Run（external_id 为 testExternalID(N)），用来验证
// 投递机制本身（重试、限流、合并、放弃）。
const testKind = "test.check"

type testPayload struct {
	N int64 `json:"n"`
}

func testExternalID(n int64) string { return fmt.Sprintf("test:%d", n) }

// testKey 返回第 n 个测试对象的 dedupe_key。
func testKey(n int64) string { return fmt.Sprintf("test:%d", n) }

// handleTest 是 testKind 的处理函数。
func (h *harness) handleTest(ctx context.Context, payload []byte) error {
	var p testPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("payload 无法解析：%w", err)
	}
	_, err := h.client.Inst(h.fake.InstallationID).CreateCheckRun(ctx, "tester/demo", &gh.CheckRun{
		Name: "test", HeadSHA: testSHA, ExternalID: testExternalID(p.N), Status: gh.CheckStatusQueued,
	})
	return err
}

// newItem 返回一个新的测试对象编号。
func (h *harness) newItem() int64 { return h.items.Add(1) }

// enqueue 写入第 n 个测试对象的记录。
func (h *harness) enqueue(n int64) {
	h.t.Helper()
	h.tx(func(tx pgx.Tx) error { return h.writer.Enqueue(h.ctx, tx, testKey(n), testKind, testPayload{N: n}) })
}

// deliver 同步投递全部到期的记录（与 Run 中的一轮相同）。
func (h *harness) deliver() {
	h.t.Helper()
	for h.worker.deliverBatch(h.ctx) > 0 {
	}
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

// checks 返回 external_id 为 externalID 的 Check Run 数。
func (h *harness) checks(externalID string) int {
	n := 0
	for _, c := range h.fake.CheckRuns(testSHA) {
		if c.ExternalID == externalID {
			n++
		}
	}
	return n
}

// ---------- Writer ----------

// TestWriterEnqueue：没有配置 GitHub App 时不写入；写入的记录是 pending、立即投递。
func TestWriterEnqueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	n := h.newItem()
	h.tx(func(tx pgx.Tx) error {
		if err := NewWriter(false).Enqueue(h.ctx, tx, testKey(n), testKind, testPayload{N: n}); err != nil {
			return err
		}
		return NewWriter(false).EnqueueIf(h.ctx, tx, testKey(n), testKind, testPayload{N: n}, "true")
	})
	if c := h.count(); c != 0 {
		t.Fatalf("没有配置 GitHub App 时不应写入记录，实际 %d 条", c)
	}
	h.enqueue(n)
	r := h.mustRow(testKey(n))
	if r.Kind != testKind || r.Payload != fmt.Sprintf(`{"n": %d}`, n) || r.State != store.OutboxPending || r.NextAttempt.After(time.Now().Add(time.Second)) {
		t.Fatalf("记录 = %+v", r)
	}
}

// TestWriterEnqueueIf：条件不成立时不写入，成立时写入；条件参数从 $4 起。
func TestWriterEnqueueIf(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	n := h.newItem()
	write := func(ok bool) {
		h.tx(func(tx pgx.Tx) error {
			return h.writer.EnqueueIf(h.ctx, tx, testKey(n), testKind, testPayload{N: n}, `$4::bool`, ok)
		})
	}
	write(false)
	if _, ok := h.row(testKey(n)); ok {
		t.Fatal("条件不成立时不应写入")
	}
	write(true)
	h.mustRow(testKey(n))
}

// TestWriterMerge：同一个 dedupe_key 只有一条记录，再次写入时覆盖 payload 并重新排队（清零重试次数、立即投递），
// 不论已有记录是否投递过、是否已经放弃（PLAN #7.8 的可合并类）。
func TestWriterMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	n := h.newItem()
	key := testKey(n)
	for _, state := range []string{store.OutboxDone, store.OutboxDead, store.OutboxPending} {
		t.Run(state, func(t *testing.T) {
			h.enqueue(n)
			h.exec(`UPDATE outbox SET state=$2, attempts=7, last_error='x', next_attempt_at=now()+interval '1 hour', payload='{}' WHERE dedupe_key=$1`, key, state)
			h.enqueue(n)
			r := h.mustRow(key)
			if r.State != store.OutboxPending || r.Attempts != 0 || r.Payload == "{}" || r.NextAttempt.After(time.Now().Add(time.Second)) {
				t.Fatalf("合并后 = %+v，期望 pending、attempts=0、新的 payload、立即投递", r)
			}
			var c int
			if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE dedupe_key=$1`, key).Scan(&c); err != nil {
				t.Fatal(err)
			}
			if c != 1 {
				t.Fatalf("同一个 key 有 %d 条记录", c)
			}
		})
	}
}

// 投递循环退出（ctx 取消）时中断的投递不计入重试次数，结果仍然落盘：下一轮启动按原次数退避重试，而不是被当成一次
// 投递失败（那会让本来就因为退出中断的记录更快被放弃）。
func TestInterruptedDeliveryDoesNotCountAsTry(t *testing.T) {
	h := newHarness(t)
	const kind = "test.interrupted"
	h.worker.Handle(kind, func(context.Context, []byte) error { return context.Canceled })
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		return h.writer.Enqueue(h.ctx, tx, testKey(1), kind, testPayload{N: 1})
	}); err != nil {
		t.Fatal(err)
	}
	var it pendingItem
	if err := h.db.QueryRow(h.ctx, `SELECT id, coalesce(dedupe_key,''), kind, payload, attempts, updated_at FROM outbox
		WHERE dedupe_key=$1`, testKey(1)).Scan(&it.ID, &it.Key, &it.Kind, &it.Payload, &it.Tries, &it.Updated); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	h.worker.deliverOne(ctx, it)

	var state, lastErr string
	var attempts int
	var future bool
	if err := h.db.QueryRow(h.ctx, `SELECT state, attempts, coalesce(last_error,''), next_attempt_at > now() FROM outbox
		WHERE dedupe_key=$1`, testKey(1)).Scan(&state, &attempts, &lastErr, &future); err != nil {
		t.Fatal(err)
	}
	if state != store.OutboxPending || attempts != 0 {
		t.Fatalf("中断的投递不应计入重试次数：state=%s attempts=%d", state, attempts)
	}
	if !strings.Contains(lastErr, "中断") || !future {
		t.Fatalf("last_error = %q，next_attempt_at 在未来 = %t", lastErr, future)
	}
}

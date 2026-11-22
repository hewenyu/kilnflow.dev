package outbox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// failCreate 让创建 Check Run 的请求返回 500。
func failCreate(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/check-runs") {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		return true
	}
	return false
}

// TestDeliverRetryBackoff：投递失败时按 2^n 秒（最长 2^maxBackoffLg 秒）退避重试，失败 maxTries 次后标记为 dead；
// 再次写入同一对象时重新排队（PLAN #7.8）。
func TestDeliverRetryBackoff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setIntercept(failCreate)
	n := h.newItem()
	key := testKey(n)
	h.enqueue(n)

	before := time.Now()
	h.deliver()
	r := h.mustRow(key)
	if r.State != store.OutboxPending || r.Attempts != 1 || !strings.Contains(r.LastError, "500") {
		t.Fatalf("第一次失败后 = %+v，期望 pending、attempts=1、记录错误", r)
	}
	if wait := r.NextAttempt.Sub(before); wait < time.Second || wait > 15*time.Second {
		t.Fatalf("第一次失败后等待 %s，期望约 2 秒", wait)
	}
	// 没到重试时间：不会再投递
	h.deliver()
	if r := h.mustRow(key); r.Attempts != 1 {
		t.Fatalf("没到重试时间就重试了：%+v", r)
	}

	// 退避有上限
	h.exec(`UPDATE outbox SET attempts=15, next_attempt_at=now() WHERE dedupe_key=$1`, key)
	before = time.Now()
	h.deliver()
	if wait := h.mustRow(key).NextAttempt.Sub(before); wait < 1020*time.Second || wait > 1040*time.Second {
		t.Fatalf("第 16 次失败后等待 %s，期望上限 %d 秒", wait, 1<<maxBackoffLg)
	}

	// 第 maxTries 次失败：放弃
	h.exec(`UPDATE outbox SET attempts=$2, next_attempt_at=now() WHERE dedupe_key=$1`, key, maxTries-1)
	h.deliver()
	if r := h.mustRow(key); r.State != store.OutboxDead || r.Attempts != maxTries {
		t.Fatalf("超过次数后 = %+v，期望 dead、attempts=%d", r, maxTries)
	}

	// 对象再次变化：重新排队，GitHub 恢复后投递成功
	h.setIntercept(nil)
	h.enqueue(n)
	h.deliver()
	if r := h.mustRow(key); r.State != store.OutboxDone || r.LastError != "" {
		t.Fatalf("重新排队后 = %+v，期望 done 并清除错误", r)
	}
	if c := h.checks(testExternalID(n)); c != 1 {
		t.Fatalf("Check Run 有 %d 个，期望 1 个", c)
	}
}

// TestDeliverRateLimited：被 GitHub 限流时推迟到恢复时间，不计入重试次数（限流再久也不会变成 dead）；
// 同一 installation 的其他记录不再发出请求，随之推迟（PLAN #7.8）。
func TestDeliverRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	until := time.Now().Add(time.Hour)
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusTooManyRequests, Until: until})
	n1, n2 := h.newItem(), h.newItem()
	h.enqueue(n1)
	h.enqueue(n2)
	h.exec(`UPDATE outbox SET attempts=$1`, maxTries-1)
	h.deliver()
	for _, key := range []string{testKey(n1), testKey(n2)} {
		r := h.mustRow(key)
		if r.State != store.OutboxPending || r.Attempts != maxTries-1 || !strings.Contains(r.LastError, "限流") {
			t.Fatalf("%s 被限流后 = %+v，期望仍为 pending、不计入次数", key, r)
		}
		if r.NextAttempt.Before(until.Add(-2 * time.Second)) {
			t.Fatalf("%s 推迟到 %s，期望不早于恢复时间 %s", key, r.NextAttempt, until)
		}
	}
	if c := h.fake.RequestCount(h.fake.InstallationID); c.Total != 1 || c.Limited != 1 {
		t.Fatalf("请求统计 %+v，期望只发出 1 个被限流的请求", c)
	}
}

// TestDeliverRequeuedDuringDelivery：投递期间同一条记录被重新写入（updated_at 变了）时保留 pending，下一轮再投递，
// 避免较新的状态被当作已投递而丢失。
func TestDeliverRequeuedDuringDelivery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	n := h.newItem()
	h.enqueue(n)
	r := h.mustRow(testKey(n))
	// 读取时的 updated_at 比数据库中的旧：相当于投递期间又写入了一次
	stale := pendingItem{ID: r.ID, Key: testKey(n), Kind: testKind, Payload: []byte(r.Payload), Updated: time.Now().Add(-time.Hour)}
	h.worker.deliverOne(h.ctx, stale)
	if got := h.mustRow(testKey(n)); got.State != store.OutboxPending {
		t.Fatalf("投递期间被重新写入的记录 = %+v，期望保留 pending", got)
	}
	h.deliver()
	if got := h.mustRow(testKey(n)); got.State != store.OutboxDone {
		t.Fatalf("下一轮投递后 = %+v，期望 done", got)
	}
}

// TestDeliverInvalidItems：未知类型、处理函数返回的错误（无法解析的 payload）按普通失败重试并记录原因。
func TestDeliverInvalidItems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`INSERT INTO outbox (dedupe_key, kind, payload) VALUES ('k1', 'unknown.kind', '{}'), ('k2', $1, '"x"')`, testKind)
	h.deliver()
	tests := []struct{ key, wantErr string }{
		{"k1", "未知的 outbox 类型"},
		{"k2", "payload 无法解析"},
	}
	for _, tt := range tests {
		r := h.mustRow(tt.key)
		if r.State != store.OutboxPending || r.Attempts != 1 || !strings.Contains(r.LastError, tt.wantErr) {
			t.Errorf("%s = %+v，期望重试并记录 %q", tt.key, r, tt.wantErr)
		}
	}
}

// TestDeliverSlowStillPersists：投递耗时超过 stateTimeout 时结果仍然落盘（成功与失败）。
// 记录结果的 context 必须在投递返回之后才创建，否则慢请求会把它耗光，状态写不进去、下一轮只能重放。
func TestDeliverSlowStillPersists(t *testing.T) {
	t.Parallel()
	const slow = 200 * time.Millisecond
	t.Run("慢成功", func(t *testing.T) {
		h := newHarness(t)
		h.worker.stateTimeout = 50 * time.Millisecond
		h.worker.Handle(testKind, func(context.Context, []byte) error {
			time.Sleep(slow)
			return nil
		})
		n := h.newItem()
		h.enqueue(n)
		h.deliver()
		if r := h.mustRow(testKey(n)); r.State != store.OutboxDone {
			t.Fatalf("慢成功后 = %+v，期望 done", r)
		}
	})
	t.Run("慢失败", func(t *testing.T) {
		h := newHarness(t)
		h.worker.stateTimeout = 50 * time.Millisecond
		h.worker.Handle(testKind, func(context.Context, []byte) error {
			time.Sleep(slow)
			return errors.New("慢失败")
		})
		n := h.newItem()
		h.enqueue(n)
		h.deliver()
		r := h.mustRow(testKey(n))
		if r.State != store.OutboxPending || r.Attempts != 1 || !strings.Contains(r.LastError, "慢失败") {
			t.Fatalf("慢失败后 = %+v，期望 pending、attempts=1、记录错误", r)
		}
	})
}

// TestWorkerRun：Run 在 Hub 唤醒后投递新记录，ctx 结束后返回。
func TestWorkerRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.worker.Run(ctx)
	}()
	n := h.newItem()
	h.enqueue(n)
	h.hub.All()
	deadline := time.Now().Add(10 * time.Second)
	for h.mustRow(testKey(n)).State != store.OutboxDone {
		if time.Now().After(deadline) {
			t.Fatal("Run 没有在 10 秒内投递")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 结束后 Run 没有返回")
	}
}

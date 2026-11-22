package ingest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// postWebhook 直接调用 HandleWebhook，返回状态码。sig 为空时按 secret 计算正确的签名。
func postWebhook(t *testing.T, i *Ingester, headers map[string]string, body []byte) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	i.HandleWebhook(rec, req)
	return rec.Code
}

// TestHandleWebhook：签名校验失败返回 401，缺少头部返回 400，没有配置 GitHub App 返回 503；
// 通过校验的事件保存后返回 202，同一个 delivery 只保存一次（PLAN #7.2 第 1、2 步）。
func TestHandleWebhook(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(`{"action":"opened","installation":{"id":777}}`)
	good := gh.Sign([]byte(testSecret), body)
	tests := []struct {
		name     string
		headers  map[string]string
		body     []byte
		want     int
		saved    string // 期望保存的 delivery（空表示不保存）
		wantType string
	}{
		{
			name:    "签名错误",
			headers: map[string]string{gh.HeaderSignature: gh.Sign([]byte("other"), body), gh.HeaderDelivery: "bad-sig", gh.HeaderEvent: "pull_request"},
			body:    body, want: http.StatusUnauthorized,
		},
		{
			name:    "没有签名",
			headers: map[string]string{gh.HeaderDelivery: "no-sig", gh.HeaderEvent: "pull_request"},
			body:    body, want: http.StatusUnauthorized,
		},
		{
			name:    "签名不是 hex",
			headers: map[string]string{gh.HeaderSignature: "sha256=zz", gh.HeaderDelivery: "bad-hex", gh.HeaderEvent: "pull_request"},
			body:    body, want: http.StatusUnauthorized,
		},
		{
			name:    "内容被篡改",
			headers: map[string]string{gh.HeaderSignature: good, gh.HeaderDelivery: "tampered", gh.HeaderEvent: "pull_request"},
			body:    []byte(`{"action":"closed","installation":{"id":777}}`), want: http.StatusUnauthorized,
		},
		{
			name:    "缺少 delivery",
			headers: map[string]string{gh.HeaderSignature: good, gh.HeaderEvent: "pull_request"},
			body:    body, want: http.StatusBadRequest,
		},
		{
			name:    "缺少事件名",
			headers: map[string]string{gh.HeaderSignature: good, gh.HeaderDelivery: "no-event"},
			body:    body, want: http.StatusBadRequest,
		},
		{
			name:    "合法",
			headers: map[string]string{gh.HeaderSignature: good, gh.HeaderDelivery: "ok-1", gh.HeaderEvent: "pull_request"},
			body:    body, want: http.StatusAccepted, saved: "ok-1", wantType: "pull_request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postWebhook(t, h.ing, tt.headers, tt.body); got != tt.want {
				t.Fatalf("状态码 %d，期望 %d", got, tt.want)
			}
			var n int
			if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM events WHERE delivery_id=$1`, tt.headers[gh.HeaderDelivery]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 1, false: 0}[tt.saved != ""]; n != want {
				t.Fatalf("保存了 %d 条事件，期望 %d", n, want)
			}
			if tt.saved == "" {
				return
			}
			var typ, action string
			var inst int64
			if err := h.db.QueryRow(h.ctx, `SELECT event_type, action, installation_id FROM events WHERE delivery_id=$1`, tt.saved).
				Scan(&typ, &action, &inst); err != nil {
				t.Fatal(err)
			}
			if typ != tt.wantType || action != "opened" || inst != 777 {
				t.Fatalf("事件的索引列 = (%s, %s, %d)，期望 (%s, opened, 777)", typ, action, inst, tt.wantType)
			}
		})
	}
}

// TestHandleWebhookDuplicateDelivery：GitHub 重复投递同一个 delivery 时只保存第一次的内容，仍然返回 202。
func TestHandleWebhookDuplicateDelivery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := map[string]any{"zen": "first"}
	h.send("ping", "dup", first)
	h.send("ping", "dup", map[string]any{"zen": "second"})
	var n int
	var zen string
	if err := h.db.QueryRow(h.ctx, `SELECT count(*), max(payload->>'zen') FROM events WHERE delivery_id='dup'`).Scan(&n, &zen); err != nil {
		t.Fatal(err)
	}
	if n != 1 || zen != "first" {
		t.Fatalf("重复投递后有 %d 条记录，payload.zen = %q；期望 1 条、保留 first", n, zen)
	}
	h.process()
	if e := h.event("dup"); !e.Processed || e.Result != "ping" {
		t.Fatalf("ping 事件 = %+v，期望已处理、结果为 ping", e)
	}
}

// TestHandleWebhookKicksIngest：保存事件后立即唤醒处理循环。
func TestHandleWebhookKicksIngest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.send("ping", h.newDelivery(), map[string]any{})
	select {
	case <-h.hub.Ingest():
	default:
		t.Fatal("保存 webhook 后没有唤醒事件处理循环")
	}
}

// TestHandleWebhookNoGitHubApp：没有配置 GitHub App 时 webhook 返回 503，不保存事件。
func TestHandleWebhookNoGitHubApp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOpts) { o.noGitHub = true })
	body := []byte(`{}`)
	code := postWebhook(t, h.ing, map[string]string{
		gh.HeaderSignature: gh.Sign([]byte(testSecret), body), gh.HeaderDelivery: "x", gh.HeaderEvent: "ping",
	}, body)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 %d，期望 503", code)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("保存了 %d 条事件，期望 0", n)
	}
}

// TestUnparsablePayload：签名正确但 payload 的结构不对（例如是数组）时照样保存，处理时丢弃并记录原因，不重试。
func TestUnparsablePayload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(`[1, 2]`)
	code := postWebhook(t, h.ing, map[string]string{
		gh.HeaderSignature: gh.Sign([]byte(testSecret), body), gh.HeaderDelivery: "garbage", gh.HeaderEvent: "push",
	}, body)
	if code != http.StatusAccepted {
		t.Fatalf("状态码 %d，期望 202", code)
	}
	h.process()
	if e := h.event("garbage"); !e.Processed || e.Attempts != 0 || !strings.Contains(e.Result, "无法解析") {
		t.Fatalf("事件 = %+v，期望直接丢弃并说明无法解析", e)
	}
}

// TestIngestRetryBackoff：处理失败（非限流）时按 5·2^n 秒退避重试，失败 eventMaxTries 次后放弃并记为 failed。
func TestIngestRetryBackoff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// 指向不存在的 commit：规划时读取 git tree 返回 404，属于普通失败
	missing := strings.Repeat("ab", 20)
	d := h.newDelivery()
	h.send("push", d, h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, missing))
	before := time.Now()
	h.process()
	e := h.event(d)
	if e.Processed || e.Attempts != 1 || !strings.Contains(e.Error, "git tree") {
		t.Fatalf("第一次失败后 = %+v，期望未处理、attempts=1、记录错误", e)
	}
	// 第 1 次失败后等待 5·2^1 = 10 秒
	if wait := e.NextAttempt.Sub(before); wait < 9*time.Second || wait > 25*time.Second {
		t.Fatalf("第一次失败后等待 %s，期望约 10 秒", wait)
	}
	// 还没到重试时间：不会再处理
	h.process()
	if e2 := h.event(d); e2.Attempts != 1 {
		t.Fatalf("没到重试时间就重试了：%+v", e2)
	}
	// 已经失败 eventMaxTries-1 次：再失败一次后放弃
	h.exec(`UPDATE events SET attempts=$2, next_attempt_at=now() WHERE delivery_id=$1`, d, eventMaxTries-1)
	h.process()
	e = h.event(d)
	if !e.Processed || e.Result != "failed" || e.Attempts != eventMaxTries || e.Error == "" {
		t.Fatalf("超过次数后 = %+v，期望已处理、result=failed、attempts=%d", e, eventMaxTries)
	}
}

// TestDeferRateLimited：GitHub 限流时事件推迟到恢复时间，不计入重试次数；同一 installation 的其他事件
// 不再发出请求、随之推迟。恢复后照常处理，同一次推送的两个 delivery 只产生一个 run（dedupe_key）。
func TestDeferRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.head("main")
	until := time.Now().Add(time.Hour)
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: until})

	d1, d2 := h.newDelivery(), h.newDelivery()
	h.send("push", d1, h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
	h.process()
	e := h.event(d1)
	if e.Processed || e.Attempts != 0 || !strings.Contains(e.Error, "等待 GitHub 限流恢复") {
		t.Fatalf("限流后 = %+v，期望未处理、attempts=0、说明等待限流恢复", e)
	}
	if e.NextAttempt.Before(until.Add(-2 * time.Second)) {
		t.Fatalf("推迟到 %s，期望不早于限流恢复时间 %s", e.NextAttempt, until)
	}
	limited := h.fake.RequestCount(h.fake.InstallationID)

	// 同一 installation 的另一个事件：不再向 GitHub 发请求，直接推迟
	h.send("push", d2, h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
	h.process()
	if e := h.event(d2); e.Processed || e.Attempts != 0 || !strings.Contains(e.Error, "限流") {
		t.Fatalf("第二个事件 = %+v，期望推迟", e)
	}
	if got := h.fake.RequestCount(h.fake.InstallationID); got.Total != limited.Total {
		t.Fatalf("限流期间又发出了 %d 个请求", got.Total-limited.Total)
	}

	// 恢复：取消模拟限流，换用没有限流状态的客户端，把事件提前到现在
	h.fake.SetRateLimit(h.fake.InstallationID, nil)
	h.resetGitHubClient()
	h.exec(`UPDATE events SET next_attempt_at=now()`)
	h.process()
	for _, d := range []string{d1, d2} {
		if e := h.event(d); !e.Processed || e.Error != "" {
			t.Fatalf("恢复后事件 %s = %+v，期望处理完成", d, e)
		}
	}
	if r := h.lastRun(demoRepoID, 1); r.SHA != sha {
		t.Fatalf("run 的 sha = %s，期望 %s", r.SHA, sha)
	}
}

// TestIngestSlowStillPersists：处理耗时超过 eventStateTimeout 时结果仍落盘（成功与失败）。
// 记录结果的 context 必须在处理返回之后才创建，否则慢处理会把它耗光，状态写不进去、下一轮只能重放。
func TestIngestSlowStillPersists(t *testing.T) {
	t.Parallel()
	const slow = 200 * time.Millisecond
	t.Run("慢成功", func(t *testing.T) {
		h := newHarness(t)
		h.ing.stateTimeout = 50 * time.Millisecond
		// 模拟 GitHub 的接口延迟响应，让整个处理超过 stateTimeout
		h.setIntercept(func(http.ResponseWriter, *http.Request) bool {
			time.Sleep(slow)
			return false
		})
		sha := h.git.head("main")
		d := h.newDelivery()
		h.send("push", d, h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
		h.process()
		if e := h.event(d); !e.Processed || e.Error != "" {
			t.Fatalf("慢成功后 = %+v，期望处理完成", e)
		}
	})
	t.Run("慢失败", func(t *testing.T) {
		h := newHarness(t)
		h.ing.stateTimeout = 50 * time.Millisecond
		h.setIntercept(func(http.ResponseWriter, *http.Request) bool {
			time.Sleep(slow)
			return false
		})
		missing := strings.Repeat("ab", 20)
		d := h.newDelivery()
		h.send("push", d, h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, missing))
		h.process()
		e := h.event(d)
		if e.Processed || e.Attempts != 1 || e.Error == "" {
			t.Fatalf("慢失败后 = %+v，期望未处理、attempts=1、记录错误", e)
		}
	})
}

// TestRunLoop：Run 在保存 webhook 后被唤醒并处理事件，ctx 结束后返回。
func TestRunLoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ing.Run(ctx)
	}()
	d := h.newDelivery()
	h.send("ping", d, map[string]any{})
	deadline := time.Now().Add(10 * time.Second)
	for !h.event(d).Processed {
		if time.Now().After(deadline) {
			t.Fatal("Run 没有在 10 秒内处理事件")
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

// TestHandleWebhookTooLarge：请求体超过上限时返回 413、不保存。截断的请求体签名也校验不过，
// 如果按截断后的内容处理会报 401，发送方会误以为凭据有问题。
func TestHandleWebhookTooLarge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := make([]byte, maxWebhookBody+1)
	headers := map[string]string{
		gh.HeaderSignature: gh.Sign([]byte(testSecret), body), gh.HeaderDelivery: "too-large", gh.HeaderEvent: "push",
	}
	if got := postWebhook(t, h.ing, headers, body); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 %d，期望 %d", got, http.StatusRequestEntityTooLarge)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM events WHERE delivery_id='too-large'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("超限的请求体不应保存，实际保存了 %d 条", n)
	}
}

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// limitedGitHub 是按 installation 返回限流响应的模拟 GitHub。
// token 为 "t<installation>"；respond 为 nil 的 installation 返回 200。
type limitedGitHub struct {
	mu       sync.Mutex
	respond  map[int64]func(w http.ResponseWriter)
	requests map[int64]int // 每个 installation 收到的 API 请求数（不含签发 token）
	tokens   int
	quota    http.Header // 非 nil 时附加到成功响应上
}

func newLimitedGitHub(t *testing.T) (*limitedGitHub, *Client, *time.Time) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &limitedGitHub{respond: map[int64]func(http.ResponseWriter){}, requests: map[int64]int{}}
	// Client 用 clock 判断 token 是否过期，有效期按模拟时钟的起点计算（测试推进时钟不超过 1 天）
	start := time.Unix(1_800_000_000, 0)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.tokens++
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "t" + r.PathValue("id"), "expires_at": start.Add(24 * time.Hour)})
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		inst, err := strconv.ParseInt(strings.TrimPrefix(r.Header.Get("Authorization"), "token t"), 10, 64)
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		g.requests[inst]++
		f := g.respond[inst]
		quota := g.quota
		g.mu.Unlock()
		if f != nil {
			f(w)
			return
		}
		maps.Copy(w.Header(), quota)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "r"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	now := start
	c := &Client{AppID: 1, Key: key, APIURL: srv.URL}
	c.clock = func() time.Time { return now }
	return g, c, &now
}

func (g *limitedGitHub) set(inst int64, f func(w http.ResponseWriter)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.respond[inst] = f
}

func (g *limitedGitHub) count(inst int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[inst]
}

func (g *limitedGitHub) tokenCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokens
}

func reply(status int, headers map[string]string, msg string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
	}
}

func getRepo(c *Client, inst int64) error {
	var out map[string]any
	return c.Inst(inst).Do(context.Background(), http.MethodGet, "/repos/o/r", nil, &out)
}

func TestRateLimitClassification(t *testing.T) {
	const primaryMsg = "API rate limit exceeded for installation ID 1."
	tests := []struct {
		name     string
		status   int
		headers  map[string]string
		msg      string
		limited  bool
		kind     RateLimitKind
		waitSecs int64 // 相对 now 的恢复时间
	}{
		{"primary 403：Remaining 0 + Reset", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000300"}, primaryMsg, true, RateLimitPrimary, 301},
		{"primary 429：Remaining 0 + Reset", 429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1800000010"}, primaryMsg, true, RateLimitPrimary, 11},
		{"primary：Reset 已经过去", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1700000000"}, primaryMsg, true, RateLimitPrimary, 1},
		{"secondary 403：Retry-After", 403, map[string]string{"Retry-After": "42", "X-RateLimit-Remaining": "4000"}, "You have exceeded a secondary rate limit.", true, RateLimitSecondary, 42},
		{"secondary 429：Retry-After", 429, map[string]string{"Retry-After": "7"}, "Too Many Requests", true, RateLimitSecondary, 7},
		{"secondary 429：Retry-After 过大时取上限", 429, map[string]string{"Retry-After": "99999999999999"}, "Too Many Requests", true, RateLimitSecondary, maxRetryAfterSeconds},
		{"secondary 429：没有头部时等 1 分钟", 429, nil, "Too Many Requests", true, RateLimitSecondary, 60},
		{"secondary 403：只有消息", 403, nil, "You have exceeded a secondary rate limit.", true, RateLimitSecondary, 60},
		{"普通 403 不是限流", 403, nil, "Resource not accessible by integration", false, "", 0},
		{"500 不是限流", 500, map[string]string{"Retry-After": "5"}, "boom", false, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, c, now := newLimitedGitHub(t)
			g.set(1, reply(tt.status, tt.headers, tt.msg))
			err := getRepo(c, 1)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !isStatus(err, tt.status) {
				t.Errorf("错误应保留状态码 %d：%v", tt.status, err)
			}
			rl, ok := RateLimited(err)
			if ok != tt.limited {
				t.Fatalf("RateLimited = %v，期望 %v：%v", ok, tt.limited, err)
			}
			// 再请求一次：限流时不应发出请求
			_ = getRepo(c, 1)
			want := 2
			if tt.limited {
				want = 1
			}
			if n := g.count(1); n != want {
				t.Errorf("请求数 %d，期望 %d", n, want)
			}
			if !tt.limited {
				return
			}
			if rl.Kind != tt.kind || rl.Installation != 1 || rl.API == nil {
				t.Errorf("限流信息不对：%+v", rl)
			}
			if got := rl.Until.Sub(*now); got != time.Duration(tt.waitSecs)*time.Second {
				t.Errorf("恢复时间相差 %s，期望 %ds", got, tt.waitSecs)
			}
			if st := c.RateLimit(1); !st.Until.Equal(rl.Until) || st.Kind != tt.kind {
				t.Errorf("RateLimit(1) = %+v", st)
			}
		})
	}
}

func TestRateLimitPerInstallationAndRecovery(t *testing.T) {
	g, c, now := newLimitedGitHub(t)
	reset := now.Add(5 * time.Minute).Unix()
	g.set(1, reply(http.StatusForbidden, map[string]string{
		"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset, 10),
	}, "API rate limit exceeded"))

	if _, ok := RateLimited(getRepo(c, 1)); !ok {
		t.Fatal("installation 1 应被限流")
	}
	// 限流期间反复调用：不再发出请求，也不再签发 token
	tokens := g.tokenCount()
	for range 50 {
		err := getRepo(c, 1)
		rl, ok := RateLimited(err)
		if !ok || rl.API != nil || rl.Kind != RateLimitPrimary {
			t.Fatalf("限流期间应直接返回本地的限流错误：%v", err)
		}
		if isStatus(err, http.StatusForbidden) {
			t.Fatal("没有发出的请求不应带状态码")
		}
	}
	if n := g.count(1); n != 1 {
		t.Errorf("限流期间 installation 1 的请求数 %d，期望 1", n)
	}
	if g.tokenCount() != tokens {
		t.Errorf("限流期间不应签发 token")
	}
	// 其他 installation 不受影响
	if err := getRepo(c, 2); err != nil {
		t.Fatalf("installation 2 不应被限流：%v", err)
	}
	if low, _ := c.LowBudget(1, 0.2); !low {
		t.Error("限流中的 installation 应视为配额不足")
	}
	if low, _ := c.LowBudget(2, 0.2); low {
		t.Error("installation 2 没有配额信息，不应视为配额不足")
	}

	// Reset 之后恢复
	g.set(1, nil)
	*now = time.Unix(reset, 0)
	if _, ok := RateLimited(getRepo(c, 1)); !ok {
		t.Error("Reset 那一秒还应等待（多等 1 秒容忍时钟偏差）")
	}
	*now = time.Unix(reset+1, 0)
	if err := getRepo(c, 1); err != nil {
		t.Fatalf("恢复后应正常请求：%v", err)
	}
	if n := g.count(1); n != 2 {
		t.Errorf("恢复后请求数 %d，期望 2", n)
	}
}

func TestLowBudget(t *testing.T) {
	g, c, now := newLimitedGitHub(t)
	reset := now.Add(time.Hour).Unix()
	setQuota := func(remaining int) {
		g.mu.Lock()
		g.quota = http.Header{}
		g.quota.Set("X-RateLimit-Limit", "5000")
		g.quota.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		g.quota.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		g.mu.Unlock()
	}
	setQuota(4000)
	if err := getRepo(c, 1); err != nil {
		t.Fatal(err)
	}
	if low, st := c.LowBudget(1, 0.2); low || st.Remaining != 4000 || st.Limit != 5000 {
		t.Errorf("剩余 4000/5000 不应视为不足：%v %+v", low, st)
	}
	setQuota(999)
	if err := getRepo(c, 1); err != nil {
		t.Fatal(err)
	}
	if low, _ := c.LowBudget(1, 0.2); !low {
		t.Error("剩余 999/5000 低于 20%，应视为不足")
	}
	if low, _ := c.LowBudget(2, 0.2); low {
		t.Error("配额按 installation 记录，installation 2 不受影响")
	}
	// 配额重置之后，旧的剩余数不再有效
	*now = time.Unix(reset, 0)
	if low, _ := c.LowBudget(1, 0.2); low {
		t.Error("配额重置后不应视为不足")
	}
}

func TestRateLimitErrorMessage(t *testing.T) {
	until := time.Date(2026, 11, 1, 6, 19, 16, 0, time.UTC)
	local := &RateLimitError{Installation: 7, Kind: RateLimitPrimary, Until: until}
	if s := local.Error(); !strings.Contains(s, "installation 7") || !strings.Contains(s, "2026-11-01T06:19:16Z") {
		t.Errorf("错误信息：%s", s)
	}
	if errors.Unwrap(local) != nil {
		t.Error("没有发出的请求不应包装 APIError")
	}
	app := &RateLimitError{Kind: RateLimitSecondary, Until: until, API: &APIError{Status: 429, Message: "slow down", Method: "GET", URL: "/app"}}
	if s := app.Error(); !strings.Contains(s, "App 被 secondary 限流") || !strings.Contains(s, "slow down") {
		t.Errorf("错误信息：%s", s)
	}
}

// App JWT 的请求单独限流（限流键 0）：App 被限流期间不发出 App 请求，也无法签发新的 installation token。
func TestRateLimitApp(t *testing.T) {
	f, c, now := newFakeAPI(t)
	f.handle("GET /app", jsonReply(http.StatusOK, map[string]any{"id": 42}))
	f.handle("GET /app/installations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeTestJSON(w, http.StatusTooManyRequests, map[string]any{"message": "slow down"})
	})
	f.handle("GET /repos/o/r", jsonReply(http.StatusOK, map[string]any{"id": 1}))

	_, err := c.ListInstallations(t.Context())
	rl, ok := RateLimited(err)
	if !ok || rl.Installation != appLimitKey || rl.Kind != RateLimitSecondary || !rl.Until.Equal(now.Add(30*time.Second)) {
		t.Fatalf("App 应被限流 30 秒：%v", err)
	}
	if !strings.Contains(rl.Error(), "App 被 secondary 限流") {
		t.Errorf("错误信息：%s", rl.Error())
	}
	before := len(f.requests())
	if _, err := c.GetApp(t.Context()); err == nil {
		t.Fatal("App 限流期间 GetApp 应失败")
	} else if rl, ok := RateLimited(err); !ok || rl.API != nil {
		t.Fatalf("应直接返回本地的限流错误：%v", err)
	}
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err == nil {
		t.Fatal("签发 token 需要 App JWT，App 限流期间应失败")
	}
	if len(f.requests()) != before || len(f.tokens()) != 0 {
		t.Error("App 限流期间不应发出请求")
	}
	if st := c.RateLimit(1); !st.Until.IsZero() {
		t.Errorf("installation 1 本身没有被限流：%+v", st)
	}

	*now = now.Add(30 * time.Second)
	if _, err := c.GetApp(t.Context()); err != nil {
		t.Fatalf("恢复后应正常请求：%v", err)
	}
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); err != nil {
		t.Fatal(err)
	}
}

// 错误响应中的配额头部也会记录；头部不完整时不记录。
func TestRecordQuota(t *testing.T) {
	f, c, now := newFakeAPI(t)
	reset := now.Add(time.Hour).Unix()
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "12")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		writeTestJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	})
	f.handle("GET /repos/o/partial", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "1")
		writeTestJSON(w, http.StatusOK, map[string]any{})
	})
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/r"); !IsNotFound(err) {
		t.Fatal(err)
	}
	want := RateLimitStatus{Remaining: 12, Limit: 5000, Reset: time.Unix(reset, 0), Known: true}
	if st := c.RateLimit(1); st != want {
		t.Errorf("RateLimit(1) = %+v，应为 %+v", st, want)
	}
	if _, err := c.Inst(1).GetRepo(t.Context(), "o/partial"); err != nil {
		t.Fatal(err)
	}
	if st := c.RateLimit(1); st != want {
		t.Errorf("头部不完整时不应覆盖已有记录：%+v", st)
	}
	if st := c.RateLimit(2); st.Known {
		t.Errorf("没有请求过的 installation：%+v", st)
	}
	if low, _ := c.LowBudget(1, 0); low {
		t.Error("reserveFraction 为 0 时不应视为不足")
	}
}

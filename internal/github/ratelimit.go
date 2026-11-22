package github

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 限流（PLAN #7.8、#13.5）：GitHub 的配额按 installation 计算（App JWT 的请求另算一份）。
// 收到限流响应后，Client 记下该 installation 的恢复时间，在此之前不再向 GitHub 发送它的请求，
// 直接返回 *RateLimitError；调用方据此延后重试，而不是把它当成普通失败。
// 成功的响应也记录剩余配额，供对账等低优先级的工作判断是否让路。

// appLimitKey 是 App JWT 请求的限流键（installation ID 都是正数）。
const appLimitKey = 0

// graphQLLimitKey 是 installation 的 GraphQL 请求的限流键：GraphQL 的 primary 配额与 REST 分开计算，
// GraphQL 的配额耗尽时不影响 REST 请求（反之亦然）。secondary 限流（并发、CPU 等）是两者共享的，
// 见 markLimited。
func graphQLLimitKey(installationID int64) int64 { return -installationID }

// sharedLimitKey 返回与 key 共享 secondary 限流的另一个键（installation 的 REST 与 GraphQL 互为对方），
// 没有时返回 false（App JWT 的请求）。
func sharedLimitKey(key int64) (int64, bool) {
	if key == appLimitKey {
		return 0, false
	}
	return -key, true
}

// secondaryDefaultWait 是 secondary rate limit 没有给出 Retry-After 时的等待时间（GitHub 文档要求至少 1 分钟）。
const secondaryDefaultWait = time.Minute

// maxRetryAfterSeconds 是采用的 Retry-After 的上限（秒）。GitHub 的限流窗口不超过 1 小时，这里留出余量。
const maxRetryAfterSeconds = 24 * 60 * 60

// 限流相关的响应头部（http.Header.Get 不区分大小写）。
const (
	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
	headerRetryAfter         = "Retry-After"
)

// RateLimitKind 区分 primary（配额耗尽）和 secondary（并发、频率等）限流。
type RateLimitKind string

const (
	// RateLimitPrimary 表示配额耗尽（X-RateLimit-Remaining: 0），到 X-RateLimit-Reset 恢复。
	RateLimitPrimary RateLimitKind = "primary"
	// RateLimitSecondary 表示 secondary rate limit，按 Retry-After 等待，没有时等待 1 分钟。
	RateLimitSecondary RateLimitKind = "secondary"
)

// RateLimitError 表示请求被 GitHub 限流，或者因为该 installation 仍在限流中而没有发出。
type RateLimitError struct {
	// Installation 是被限流的 installation；0 表示 App 本身（JWT 认证的请求）。
	Installation int64
	// GraphQL 表示被限流的是 installation 的 GraphQL 配额（与 REST 分开计算）。
	GraphQL bool
	Kind    RateLimitKind
	// Until 是恢复时间：在此之前不会再为该 installation 发出请求。
	Until time.Time
	// API 是 GitHub 返回的限流响应；为 nil 表示仍在限流中，请求没有发出。
	API *APIError
}

func (e *RateLimitError) Error() string {
	who := "App"
	if e.Installation != appLimitKey {
		who = fmt.Sprintf("installation %d", e.Installation)
	}
	if e.GraphQL {
		who += " 的 GraphQL"
	}
	until := e.Until.UTC().Format(time.RFC3339)
	if e.API == nil {
		return fmt.Sprintf("GitHub %s 正在 %s 限流中，%s 之前不发送请求", who, e.Kind, until)
	}
	return fmt.Sprintf("%v（%s 被 %s 限流，%s 恢复）", e.API, who, e.Kind, until)
}

func (e *RateLimitError) Unwrap() error {
	if e.API == nil {
		return nil
	}
	return e.API
}

// RateLimited 判断错误是否由限流引起，是时返回限流信息。
func RateLimited(err error) (*RateLimitError, bool) {
	return errors.AsType[*RateLimitError](err)
}

// RateLimitStatus 是某个 installation 的限流状态和最近一次看到的配额。
type RateLimitStatus struct {
	// Until 非零且在未来时，该 installation 处于限流中。
	Until time.Time
	Kind  RateLimitKind
	// Remaining、Limit、Reset 来自最近一次响应的 X-RateLimit-* 头部；Known 为 false 表示还没有看到。
	Remaining, Limit int
	Reset            time.Time
	Known            bool
}

// limitState 是一个限流键的状态。
type limitState struct {
	until time.Time
	kind  RateLimitKind
	quota RateLimitStatus
}

func (c *Client) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// RateLimit 返回 installation 的限流状态。
func (c *Client) RateLimit(installationID int64) RateLimitStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.limits[installationID]
	if st == nil {
		return RateLimitStatus{}
	}
	s := st.quota
	if c.now().Before(st.until) {
		s.Until, s.Kind = st.until, st.kind
	}
	return s
}

// LowBudget 判断 installation 是否正在限流，或者剩余配额已低于上限的 reserveFraction（直到配额重置）。
// 对账等可以推迟的工作在这时让路，把剩余配额留给 webhook、规划和发布。
func (c *Client) LowBudget(installationID int64, reserveFraction float64) (bool, RateLimitStatus) {
	s := c.RateLimit(installationID)
	if !s.Until.IsZero() {
		return true, s
	}
	if !s.Known || s.Limit <= 0 || !c.now().Before(s.Reset) {
		return false, s
	}
	return float64(s.Remaining) < float64(s.Limit)*reserveFraction, s
}

// checkLimit 在发出请求前检查限流：仍在限流中时返回 *RateLimitError，不发送请求。
func (c *Client) checkLimit(key int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.limits[key]
	if st == nil || !c.now().Before(st.until) {
		return nil
	}
	return newRateLimitError(key, st.kind, st.until, nil)
}

// newRateLimitError 返回限流键 key 的 *RateLimitError。
func newRateLimitError(key int64, kind RateLimitKind, until time.Time, api *APIError) *RateLimitError {
	e := &RateLimitError{Installation: key, Kind: kind, Until: until, API: api}
	if key < 0 {
		e.Installation, e.GraphQL = -key, true
	}
	return e
}

// state 返回 key 的状态，没有时创建。调用方持有 c.mu。
func (c *Client) state(key int64) *limitState {
	if c.limits == nil {
		c.limits = map[int64]*limitState{}
	}
	st := c.limits[key]
	if st == nil {
		st = &limitState{}
		c.limits[key] = st
	}
	return st
}

// recordQuota 记录响应头部中的剩余配额（没有这些头部时不记录）。
func (c *Client) recordQuota(key int64, h http.Header) {
	quota, ok := parseQuota(h)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state(key).quota = quota
}

// rateLimitError 记录错误响应的配额；是限流时还记下恢复时间并返回 *RateLimitError，否则返回 nil。
func (c *Client) rateLimitError(key int64, resp *http.Response, ae *APIError) *RateLimitError {
	c.recordQuota(key, resp.Header)
	kind, until, limited := rateLimitOf(resp, ae, c.now())
	if !limited {
		return nil
	}
	return c.markLimited(key, kind, until, ae)
}

// bodyRateLimitError 处理在 HTTP 200 的响应体中报告的限流（GraphQL 的 errors.type 为 RATE_LIMITED）：
// 按响应头部（Retry-After、X-RateLimit-*）确定恢复时间，没有时至少等待 1 分钟。
func (c *Client) bodyRateLimitError(key int64, resp *http.Response, ae *APIError) *RateLimitError {
	kind, until, ok := headerRateLimit(resp.Header, c.now())
	if !ok {
		kind, until = RateLimitSecondary, c.now().Add(secondaryDefaultWait)
	}
	return c.markLimited(key, kind, until, ae)
}

// markLimited 记下 key 在 until 之前被限流，返回 *RateLimitError。secondary 限流由 installation 的 REST 和 GraphQL
// 共享（GitHub 文档：并发和 CPU 的限额在两者之间共享），两类请求都要等到 Retry-After 之后；primary 配额分开计算，
// 只限制 key 自己。
func (c *Client) markLimited(key int64, kind RateLimitKind, until time.Time, ae *APIError) *RateLimitError {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extendLimit(key, kind, until)
	if other, ok := sharedLimitKey(key); ok && kind == RateLimitSecondary {
		c.extendLimit(other, kind, until)
	}
	return newRateLimitError(key, kind, until, ae)
}

// extendLimit 把 key 的恢复时间延长到 until。调用方持有 c.mu。
func (c *Client) extendLimit(key int64, kind RateLimitKind, until time.Time) {
	st := c.state(key)
	// 并发的请求可能先后收到限流响应：恢复时间取较晚的一个
	if until.After(st.until) {
		st.until, st.kind = until, kind
	}
}

// parseQuota 读取 X-RateLimit-Limit / Remaining / Reset。
func parseQuota(h http.Header) (RateLimitStatus, bool) {
	limit, err1 := strconv.Atoi(h.Get(headerRateLimitLimit))
	remaining, err2 := strconv.Atoi(h.Get(headerRateLimitRemaining))
	reset, err3 := strconv.ParseInt(h.Get(headerRateLimitReset), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return RateLimitStatus{}, false
	}
	return RateLimitStatus{Remaining: remaining, Limit: limit, Reset: time.Unix(reset, 0), Known: true}, true
}

// rateLimitOf 判断响应是否是限流，以及何时恢复。规则按 GitHub 文档：
//   - 有 Retry-After：等待这么多秒（secondary）；
//   - X-RateLimit-Remaining 为 0：等到 X-RateLimit-Reset（primary）；
//   - 其他 429，或消息中说明是 rate limit 的 403：至少等待 1 分钟（secondary）。
func rateLimitOf(resp *http.Response, ae *APIError, now time.Time) (RateLimitKind, time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return "", time.Time{}, false
	}
	if kind, until, ok := headerRateLimit(resp.Header, now); ok {
		return kind, until, true
	}
	if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(ae.Message), "rate limit") {
		return RateLimitSecondary, now.Add(secondaryDefaultWait), true
	}
	return "", time.Time{}, false
}

// headerRateLimit 按响应头部判断限流和恢复时间：有 Retry-After 时等待这么多秒（secondary）；
// X-RateLimit-Remaining 为 0 时等到 X-RateLimit-Reset（primary）。
func headerRateLimit(h http.Header, now time.Time) (RateLimitKind, time.Time, bool) {
	if s := h.Get(headerRetryAfter); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			// 限制上限，避免异常的大数在乘以 time.Second 时溢出成负数（那样等于没有限流）
			wait := time.Duration(min(n, maxRetryAfterSeconds)) * time.Second
			return RateLimitSecondary, now.Add(wait), true
		}
	}
	if h.Get(headerRateLimitRemaining) == "0" {
		until := now.Add(secondaryDefaultWait)
		if n, err := strconv.ParseInt(h.Get(headerRateLimitReset), 10, 64); err == nil {
			// 多等 1 秒，容忍时钟偏差；Reset 已经过去时也至少等 1 秒
			until = time.Unix(n, 0).Add(time.Second)
			if !until.After(now) {
				until = now.Add(time.Second)
			}
		}
		// 上限：异常的 Reset（例如 99999999999）不能让 Server 停等好几年。
		if maxUntil := now.Add(time.Duration(maxRetryAfterSeconds) * time.Second); until.After(maxUntil) {
			until = maxUntil
		}
		return RateLimitPrimary, until, true
	}
	return "", time.Time{}, false
}

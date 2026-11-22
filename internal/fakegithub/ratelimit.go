package fakegithub

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimit 是模拟的限流：在 Until 之前，某个 installation 的匹配请求返回限流响应，之后自动恢复。
type RateLimit struct {
	// Status 是限流响应的状态码（403 或 429）。
	Status int
	// Primary 为 true 时模拟配额耗尽：返回 X-RateLimit-Remaining: 0 和 X-RateLimit-Reset（Until，向上取整到秒）；
	// 否则模拟 secondary rate limit：返回 Retry-After。
	Primary bool
	Until   time.Time
	// Match 为 nil 时匹配该 installation 的全部请求。
	Match func(r *http.Request) bool
}

// RequestCount 是某个 installation 收到的请求数。
type RequestCount struct {
	// Total 是全部请求数（包括被限流的），Limited 是返回了限流响应的请求数。
	Total, Limited int
}

// SetRateLimit 设置 installation 的模拟限流；rl 为 nil 时取消。installation 0 表示 App JWT 认证的请求。
func (s *Server) SetRateLimit(installation int64, rl *RateLimit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rl == nil {
		delete(s.limits, installation)
		return
	}
	s.limits[installation] = rl
}

// RequestCount 返回 installation 收到的请求数（installation 0 表示 App JWT 认证的请求）。
func (s *Server) RequestCount(installation int64) RequestCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.counts[installation]; c != nil {
		return *c
	}
	return RequestCount{}
}

// installationOf 返回请求所用 token 属于的 installation；App JWT 和未知 token 返回 0。
func (s *Server) installationOf(r *http.Request) int64 {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[tok]
}

// throttle 统计请求；请求处于模拟限流中时写出限流响应并返回 true。
func (s *Server) throttle(w http.ResponseWriter, r *http.Request) bool {
	inst := s.installationOf(r)
	now := time.Now()
	s.mu.Lock()
	c := s.counts[inst]
	if c == nil {
		c = &RequestCount{}
		s.counts[inst] = c
	}
	c.Total++
	rl := s.limits[inst]
	limited := rl != nil && now.Before(rl.Until) && (rl.Match == nil || rl.Match(r))
	if limited {
		c.Limited++
	}
	s.mu.Unlock()
	if !limited {
		return false
	}
	if rl.Primary {
		reset := int64(math.Ceil(float64(rl.Until.UnixMilli()) / 1000))
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		writeMessage(w, rl.Status, "API rate limit exceeded for installation ID "+strconv.FormatInt(inst, 10)+".")
		return true
	}
	wait := int(math.Ceil(rl.Until.Sub(now).Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	writeMessage(w, rl.Status, "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
	return true
}

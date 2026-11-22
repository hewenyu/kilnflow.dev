package github

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GraphQL 的限流以 HTTP 200、errors.type=RATE_LIMITED 返回：返回 *RateLimitError，恢复之前不再发出
// GraphQL 请求；GraphQL 的配额与 REST 分开记录，REST 请求不受影响。
func TestGraphQLRateLimited(t *testing.T) {
	t.Parallel()
	f, c, now := newFakeAPI(t)
	reset := now.Add(time.Hour)
	f.handle("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerRateLimitLimit, "5000")
		w.Header().Set(headerRateLimitRemaining, "0")
		w.Header().Set(headerRateLimitReset, strconv.FormatInt(reset.Unix(), 10))
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation."}]}`))
	})
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":1}`)) })
	inst := c.Inst(7)

	_, err := inst.ListClosingIssues(t.Context(), "o/r", 1)
	rl, ok := RateLimited(err)
	if !ok || !rl.GraphQL || rl.Installation != 7 || rl.Kind != RateLimitPrimary || !rl.Until.Equal(reset.Add(time.Second)) {
		t.Fatalf("应返回 GraphQL 的限流：%v（%+v）", err, rl)
	}
	if !strings.Contains(err.Error(), "GraphQL") {
		t.Errorf("错误信息：%v", err)
	}
	// 恢复之前不再发出 GraphQL 请求
	_, err = inst.ListClosingIssues(t.Context(), "o/r", 1)
	if rl, ok := RateLimited(err); !ok || rl.API != nil {
		t.Fatalf("限流中应直接返回、不发请求：%v", err)
	}
	// REST 不受影响
	if err := inst.Do(t.Context(), http.MethodGet, "/repos/o/r", nil, nil); err != nil {
		t.Fatalf("REST 请求：%v", err)
	}
	var graphql int
	for _, r := range f.requests() {
		if r.key() == "POST /graphql" {
			graphql++
		}
	}
	if graphql != 1 {
		t.Errorf("GraphQL 请求 %d 次，想要 1 次", graphql)
	}
	if st := c.RateLimit(7); !st.Until.IsZero() {
		t.Errorf("REST 不应处于限流中：%+v", st)
	}
	// 恢复后再次请求
	*now = reset.Add(2 * time.Second)
	if _, err := inst.ListClosingIssues(t.Context(), "o/r", 1); err == nil {
		t.Fatal("模拟 GitHub 仍然限流，应返回错误")
	} else if rl, ok := RateLimited(err); !ok || rl.API == nil {
		t.Fatalf("恢复后应重新请求：%v", err)
	}
}

// secondary 限流（并发、CPU 等）由 installation 的 REST 和 GraphQL 共享：任何一方收到后，两类请求都等到 Retry-After 之后，
// 其他 installation 和 App 本身不受影响。
func TestSecondaryRateLimitShared(t *testing.T) {
	t.Parallel()
	secondary := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerRetryAfter, "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":1}`)) }
	for _, tc := range []struct {
		name          string
		restLimited   bool
		first, second func(*Inst) error
	}{
		{
			name: "GraphQL 到 REST",
			first: func(i *Inst) error {
				_, err := i.ListClosingIssues(t.Context(), "o/r", 1)
				return err
			},
			second: func(i *Inst) error { return i.Do(t.Context(), http.MethodGet, "/repos/o/r", nil, nil) },
		},
		{
			name:        "REST 到 GraphQL",
			restLimited: true,
			first:       func(i *Inst) error { return i.Do(t.Context(), http.MethodGet, "/repos/o/r", nil, nil) },
			second: func(i *Inst) error {
				_, err := i.ListClosingIssues(t.Context(), "o/r", 1)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, c, now := newFakeAPI(t)
			if tc.restLimited {
				f.handle("GET /repos/o/r", secondary)
				f.handle("POST /graphql", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{}}`)) })
			} else {
				f.handle("POST /graphql", secondary)
				f.handle("GET /repos/o/r", ok)
			}
			inst := c.Inst(7)
			if rl, limited := RateLimited(tc.first(inst)); !limited || rl.Kind != RateLimitSecondary || rl.API == nil {
				t.Fatalf("第一次请求应被 secondary 限流：%+v", rl)
			}
			// 冷却期间另一类请求不发出
			err := tc.second(inst)
			if rl, limited := RateLimited(err); !limited || rl.API != nil || !rl.Until.Equal(now.Add(time.Minute)) {
				t.Fatalf("冷却期间另一类请求应直接返回限流：%v", err)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("发出 %d 个请求，想要 1 个", n)
			}
			if st := c.RateLimit(7); st.Until.IsZero() {
				t.Errorf("RateLimit 应显示限流中：%+v", st)
			}
			// 其他 installation 不受影响
			if st := c.RateLimit(8); !st.Until.IsZero() {
				t.Errorf("其他 installation 不应限流：%+v", st)
			}
			// 冷却之后两类请求都恢复
			*now = now.Add(61 * time.Second)
			if err := tc.second(inst); err != nil {
				if _, limited := RateLimited(err); limited {
					t.Fatalf("冷却之后应恢复：%v", err)
				}
			}
			if n := len(f.requests()); n != 2 {
				t.Errorf("冷却之后应发出请求，共 %d 个", n)
			}
		})
	}
}

// 没有限流头部的 RATE_LIMITED 至少等待 1 分钟；其他 GraphQL 错误照常返回 *GraphQLError。
func TestGraphQLErrors(t *testing.T) {
	t.Parallel()
	f, c, now := newFakeAPI(t)
	body := `{"errors":[{"type":"RATE_LIMITED","message":"slow down"}]}`
	f.handle("POST /graphql", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	inst := c.Inst(7)
	_, err := inst.ListClosingIssues(t.Context(), "o/r", 1)
	if rl, ok := RateLimited(err); !ok || rl.Kind != RateLimitSecondary || !rl.Until.Equal(now.Add(secondaryDefaultWait)) {
		t.Fatalf("没有头部时按 secondary 等 1 分钟：%v（%+v）", err, rl)
	}
	*now = now.Add(2 * secondaryDefaultWait)
	body = `{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`
	_, err = inst.ListClosingIssues(t.Context(), "o/r", 1)
	ge, ok := errors.AsType[*GraphQLError](err)
	if _, limited := RateLimited(err); limited || !ok || ge.Type != GraphQLForbidden {
		t.Fatalf("FORBIDDEN 应返回 *GraphQLError：%v", err)
	}
}

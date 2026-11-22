package fakegithub

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// 模拟限流：按 installation（0 表示 App JWT）返回 primary 或 secondary 限流响应，并统计请求数。
func TestSetRateLimit(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		primary  bool
		wantKind github.RateLimitKind
	}{
		{"primary 403", http.StatusForbidden, true, github.RateLimitPrimary},
		{"secondary 429", http.StatusTooManyRequests, false, github.RateLimitSecondary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			// 先签发 token，之后的限流只影响 installation 的请求
			if _, err := f.inst.GetRepo(ctx, demo); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Hour)
			f.s.SetRateLimit(f.s.InstallationID, &RateLimit{Status: tt.status, Primary: tt.primary, Until: until})

			_, err := f.inst.GetRepo(ctx, demo)
			rl, ok := github.RateLimited(err)
			if !ok || rl.Kind != tt.wantKind || rl.API == nil || rl.API.Status != tt.status {
				t.Fatalf("应返回 %s 限流：%v", tt.wantKind, err)
			}
			// 恢复时间约为 Until（primary 向上取整到秒再加 1 秒，secondary 按 Retry-After 的整秒数）
			if d := rl.Until.Sub(until); d < -time.Second || d > 2*time.Second {
				t.Errorf("恢复时间 %s，与 Until 相差 %s", rl.Until, d)
			}
			if c := f.s.RequestCount(f.s.InstallationID); c.Total != 2 || c.Limited != 1 {
				t.Errorf("RequestCount = %+v", c)
			}

			// 其他客户端（没有本地限流状态）在取消限流后可以正常请求
			f.s.SetRateLimit(f.s.InstallationID, nil)
			fresh := &github.Client{AppID: f.s.AppID, Key: testKey(), APIURL: f.s.URL}
			if _, err := fresh.Inst(f.s.InstallationID).GetRepo(ctx, demo); err != nil {
				t.Fatalf("取消限流后应正常请求：%v", err)
			}
		})
	}
}

// Match 只限流匹配的请求；App JWT 的请求计在 installation 0 下；过期的限流自动恢复。
func TestRateLimitMatchAndApp(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.s.SetRateLimit(f.s.InstallationID, &RateLimit{
		Status: http.StatusForbidden, Until: time.Now().Add(time.Hour),
		Match: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/branches") },
	})
	if _, err := f.inst.GetRepo(ctx, demo); err != nil {
		t.Fatalf("不匹配的请求不应限流：%v", err)
	}
	if _, err := f.inst.ListBranches(ctx, demo); err == nil {
		t.Fatal("匹配的请求应被限流")
	} else if _, ok := github.RateLimited(err); !ok {
		t.Fatalf("应返回限流错误：%v", err)
	}
	if c := f.s.RequestCount(f.s.InstallationID); c.Total != 2 || c.Limited != 1 {
		t.Errorf("installation 的 RequestCount = %+v", c)
	}
	// App JWT 的请求（签发 token、GetApp）计在 0 下
	if _, err := f.c.GetApp(ctx); err != nil {
		t.Fatal(err)
	}
	if c := f.s.RequestCount(0); c.Total != 2 || c.Limited != 0 {
		t.Errorf("App 的 RequestCount = %+v", c)
	}
	if c := f.s.RequestCount(12345); c != (RequestCount{}) {
		t.Errorf("没有请求的 installation：%+v", c)
	}

	// Until 已经过去的限流不生效
	f.s.SetRateLimit(0, &RateLimit{Status: http.StatusTooManyRequests, Until: time.Now().Add(-time.Second)})
	if _, err := f.c.GetApp(ctx); err != nil {
		t.Fatalf("过期的限流不应生效：%v", err)
	}
	// App 被限流
	f.s.SetRateLimit(0, &RateLimit{Status: http.StatusTooManyRequests, Until: time.Now().Add(time.Hour)})
	_, err := f.c.GetApp(ctx)
	if rl, ok := github.RateLimited(err); !ok || rl.Installation != 0 {
		t.Fatalf("App 应被限流：%v", err)
	}
	if c := f.s.RequestCount(0); c.Limited != 1 {
		t.Errorf("App 的 RequestCount = %+v", c)
	}
}

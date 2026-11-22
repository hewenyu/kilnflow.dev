package checkrun

import (
	"testing"
	"time"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

// TestSetCompleted：completed 的 Check Run 使用记录的结束时间，没有记录时用当前时间。
func TestSetCompleted(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cr := &gh.CheckRun{}
	SetCompleted(cr, "success", &finished)
	if cr.Status != gh.CheckStatusCompleted || cr.Conclusion != "success" || !cr.CompletedAt.Equal(finished) {
		t.Fatalf("setCompleted = %+v", cr)
	}
	before := time.Now()
	SetCompleted(cr, "failure", nil)
	if cr.CompletedAt == nil || cr.CompletedAt.Before(before) {
		t.Fatalf("没有结束时间时 completed_at = %v，期望当前时间", cr.CompletedAt)
	}
}

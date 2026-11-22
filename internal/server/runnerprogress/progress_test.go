package runnerprogress

import (
	"math"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 进度只用于展示：负数、不一致的计数和过长的文本都要修正。
func TestFromProto(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p := FromProto(&runnerv1.Progress{
		Phase: runnerv1.Phase(99), Detail: strings.Repeat("长", 200), PhaseElapsedMs: -5, IdleMs: 3_500,
		FilesTotal: 2, FilesDone: 5, BytesTotal: 100, BytesDone: 80, BytesSent: 50, Retries: -1,
	}, -7, now)
	if p.Phase != "" || len(p.Detail) > MaxProgressText+len(shorten.Ellipsis) || p.Step != -1 || p.PhaseStartedAt != now.Unix() || p.MovedAt != now.Unix()-4 ||
		p.FilesDone != 2 || p.BytesDone != 80 || p.BytesSent != 20 || p.Retries != 0 || p.ReportedAt != now.Unix() {
		t.Fatalf("progressFromProto = %+v", p)
	}
	// 过大的时长不能溢出成将来的时间
	p = FromProto(&runnerv1.Progress{PhaseElapsedMs: math.MaxInt64, IdleMs: math.MaxInt64 / 1000}, 0, now)
	if p.PhaseStartedAt >= now.Unix() || p.MovedAt >= now.Unix() {
		t.Fatalf("过大的时长换算出 PhaseStartedAt=%d、MovedAt=%d，应早于 %d", p.PhaseStartedAt, p.MovedAt, now.Unix())
	}
}

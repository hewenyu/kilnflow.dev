// Package runnerprogress 把 Runner 上报的执行进度（runner.v1 的 Progress，CI Runner 和 AI 执行节点都用）换算成保存的形式
// （store.AttemptProgress）。放在 store 之外：store 是所有领域共用的数据层，不依赖任何 Runner 协议（PLAN #32.9）。
package runnerprogress

import (
	"math"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// MaxProgressText 是保存的进度说明和错误信息的长度上限（字节），与 Runner 的上限一致。
const MaxProgressText = 200

// phaseName 返回上报的阶段对应的保存值（未知的阶段为空）。
func phaseName(p runnerv1.Phase) string {
	switch p {
	case runnerv1.Phase_PHASE_PREPARE:
		return store.PhasePrepare
	case runnerv1.Phase_PHASE_STEP:
		return store.PhaseStep
	case runnerv1.Phase_PHASE_UPLOAD:
		return store.PhaseUpload
	case runnerv1.Phase_PHASE_CLEANUP:
		return store.PhaseCleanup
	case runnerv1.Phase_PHASE_COMPLETE:
		return store.PhaseComplete
	}
	return ""
}

// FromProto 把 Runner 上报的进度换算成保存的形式：相对时长按 now 换算成时间，
// 负数和不一致的计数修正到合理范围，文本截断（这些值只用于展示，不能信任到让页面出错）。
func FromProto(p *runnerv1.Progress, step int32, now time.Time) *store.AttemptProgress {
	at := func(ms int64) int64 {
		// 限制在 time.Duration 能表示的范围内：过大的值乘以 time.Millisecond 会溢出成负数，换算出将来的时间
		ms = min(max(ms, 0), int64(math.MaxInt64/time.Millisecond))
		return now.Add(-time.Duration(ms) * time.Millisecond).Unix()
	}
	nonNeg := func(n int64) int64 { return max(n, 0) }
	out := &store.AttemptProgress{
		Phase:          phaseName(p.Phase),
		Detail:         shorten.Text(p.Detail, MaxProgressText),
		Step:           int(max(step, -1)),
		PhaseStartedAt: at(p.PhaseElapsedMs),
		MovedAt:        at(p.IdleMs),
		ReportedAt:     now.Unix(),
		FilesTotal:     int(max(p.FilesTotal, 0)),
		FilesSkipped:   int(max(p.FilesSkipped, 0)),
		BytesTotal:     nonNeg(p.BytesTotal),
		Retries:        int(max(p.Retries, 0)),
		LastError:      shorten.Text(p.LastError, MaxProgressText),
	}
	out.FilesDone = min(int(max(p.FilesDone, 0)), out.FilesTotal)
	out.BytesDone = min(nonNeg(p.BytesDone), out.BytesTotal)
	out.BytesSent = min(nonNeg(p.BytesSent), out.BytesTotal-out.BytesDone)
	return out
}

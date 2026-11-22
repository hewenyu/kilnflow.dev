package server

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
)

// TestDevOwnerKinds：AI 开发的每一种 PR 归属都换成合并队列中含义相同的一种（PLAN #28.3）；漏掉一种时 Owner 返回错误，
// 合并队列稍后重试，不会把 AI 开发的 PR 当成第三方。
func TestDevOwnerKinds(t *testing.T) {
	want := map[aidev.OwnerKind]mergequeue.DevOwnerKind{
		aidev.OwnerNone: mergequeue.DevOwnerNone, aidev.OwnerActive: mergequeue.DevOwnerActive,
		aidev.OwnerEnded: mergequeue.DevOwnerEnded, aidev.OwnerRecording: mergequeue.DevOwnerRecording,
	}
	if len(devOwnerKinds) != len(want) {
		t.Fatalf("映射了 %d 种归属，想要 %d 种", len(devOwnerKinds), len(want))
	}
	for k, v := range want {
		if got, ok := devOwnerKinds[k]; !ok || got != v {
			t.Errorf("归属 %d → %d（%v），想要 %d", k, got, ok, v)
		}
	}
}

package runnerapi

import (
	"sync"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 同一个 run 的多个 job 同时结束（例如矩阵 build）：Complete 必须先锁 run 再锁 job，与推进 run、取消 run 的顺序一致，
// 否则两个 Complete 各持有自己的 job、互相等待对方的 job（推进 run 时要锁 run 的全部 job），只能靠死锁检测和重试恢复
// （8 个 job 同时结束时事务执行了 39 次，6 个超过重试上限）。
func TestService_Complete_sameRunNoDeadlock(t *testing.T) {
	t.Parallel()
	const n = 8
	h := newHarness(t)
	runID := h.addRun(h.addRepo())
	for range n {
		h.addJob(runID)
	}
	type claimed struct {
		r   *testRunner
		ref *runnerv1.AttemptRef
	}
	var cs []claimed
	for range n {
		r := h.enrollRunner()
		cs = append(cs, claimed{r, r.mustAcquire(1, 1)[0].Ref})
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i, c := range cs {
		wg.Go(func() {
			<-start
			errs[i] = c.r.complete(&runnerv1.CompleteRequest{Ref: c.ref, Outcome: runnerv1.Outcome_OUTCOME_FAILED, ExitCode: 1})
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Complete #%d：%v", i, err)
		}
	}
	if r := h.db.Retries(); r != 0 {
		t.Errorf("同一个 run 的 job 同时结束时事务重试了 %d 次（死锁），应为 0", r)
	}
	var state store.RunState
	if err := h.db.QueryRow(t.Context(), `SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != store.RunFailed {
		t.Fatalf("全部 job 结束后 run 应为 failed，实际 %s", state)
	}
}

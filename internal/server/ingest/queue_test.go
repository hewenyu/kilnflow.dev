package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
)

// recordingQueue 记录 ingest 对合并队列的调用（MergeQueue）。
type recordingQueue struct {
	mu    sync.Mutex
	calls []string
	// queued 是已经入队的 PR（Enqueue 对它们返回 false）。
	queued map[int]bool
	// failNotify 是接下来要失败的 Notify 次数（失败的调用不记录）。
	failNotify int
}

func (q *recordingQueue) Enqueue(_ context.Context, repoID int64, pr int, head, baseRef, actor string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, fmt.Sprintf("enqueue %d#%d %s %s", repoID, pr, head, actor))
	if q.queued == nil {
		q.queued = map[int]bool{}
	}
	added := !q.queued[pr]
	q.queued[pr] = true
	return added, nil
}

func (q *recordingQueue) Notify(_ context.Context, repoID int64, pr int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failNotify > 0 {
		q.failNotify--
		return errors.New("临时的 outbox 写入失败")
	}
	q.calls = append(q.calls, fmt.Sprintf("notify %d#%d", repoID, pr))
	return nil
}

func (q *recordingQueue) Nominate(_ context.Context, repoID int64, pr int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, fmt.Sprintf("nominate %d#%d", repoID, pr))
	return nil
}

// take 返回并清空记录的调用。
func (q *recordingQueue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.calls
	q.calls = nil
	return out
}

// TestMergeQueueEvents：加上 kiln:merge 即入队（本 App 自己加的也一样，幂等）；其他 label、去掉 label、转为 draft、
// 推送、关闭 PR 和默认分支的 push 通知合并队列（PLAN #28.2、#28.3）。
func TestMergeQueueEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.head("main")
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	h.git.git("checkout", "-q", "main")
	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "main"))
	want := []string{fmt.Sprintf("notify %d#7", demoRepoID)}
	if got := h.queue.take(); !slices.Equal(got, want) {
		t.Fatalf("打开 PR：%v，想要 %v", got, want)
	}

	labeled := func(name, sender string) map[string]any {
		p := h.prPayload(h.demo, "labeled", 7, "feature", head, "main")
		p["label"] = map[string]any{"name": name}
		if sender != "" {
			withSender(p, sender, "Bot")
		}
		return p
	}
	if got := h.deliverOK("pull_request", labeled(mergequeue.Label, "")); got != "加入合并队列" {
		t.Fatalf("加 label：%q", got)
	}
	want = []string{fmt.Sprintf("enqueue %d#7 %s github:%s", demoRepoID, head, testAccount)}
	if got := h.queue.take(); !slices.Equal(got, want) {
		t.Fatalf("加 label：%v，想要 %v", got, want)
	}
	if got := h.deliverOK("pull_request", labeled(mergequeue.Label, testBot)); got != "已经在合并队列中" {
		t.Fatalf("本 App 加的 label：%q", got)
	}
	h.queue.take()

	for _, tt := range []struct {
		event   string
		payload map[string]any
		want    string
	}{
		{"pull_request", labeled("bug", ""), fmt.Sprintf("notify %d#7", demoRepoID)},
		{"pull_request", h.prPayload(h.demo, "unlabeled", 7, "feature", head, "main"), fmt.Sprintf("notify %d#7", demoRepoID)},
		{"pull_request", h.prPayload(h.demo, "converted_to_draft", 7, "feature", head, "main"), fmt.Sprintf("notify %d#7", demoRepoID)},
		{"push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha), fmt.Sprintf("notify %d#0", demoRepoID)},
		{"pull_request", h.prPayload(h.demo, "closed", 7, "feature", head, "main"), fmt.Sprintf("notify %d#7", demoRepoID)},
	} {
		h.deliverOK(tt.event, tt.payload)
		if got := h.queue.take(); !slices.Equal(got, []string{tt.want}) {
			t.Fatalf("%s %v：%v，想要 %s", tt.event, tt.payload["action"], got, tt.want)
		}
	}
	// 去掉 kiln:hold、转为 ready 时提名自动入队（PLAN #29.5）
	unhold := h.prPayload(h.demo, "unlabeled", 7, "feature", head, "main")
	unhold["label"] = map[string]any{"name": mergequeue.HoldLabel}
	h.deliverOK("pull_request", unhold)
	if got, want := h.queue.take(), []string{fmt.Sprintf("nominate %d#7", demoRepoID), fmt.Sprintf("notify %d#7", demoRepoID)}; !slices.Equal(got, want) {
		t.Fatalf("去掉 %s：%v，想要 %v", mergequeue.HoldLabel, got, want)
	}
	h.deliverOK("pull_request", h.prPayload(h.demo, "ready_for_review", 7, "feature", head, "main"))
	if got, want := h.queue.take(), []string{fmt.Sprintf("notify %d#7", demoRepoID), fmt.Sprintf("nominate %d#7", demoRepoID)}; !slices.Equal(got, want) {
		t.Fatalf("转为 ready：%v，想要 %v", got, want)
	}
	// 任何分支的 push 都通知队列（PLAN #28.5）：目标是这条分支或者其他允许合并的分支的 PR 都可能落后了；
	// 队列为空时 KickMergeQueue 的 EXISTS 守卫不会写通知（这里队列非空，所以有通知）
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, head))
	if got, want := h.queue.take(), []string{fmt.Sprintf("notify %d#0", demoRepoID)}; !slices.Equal(got, want) {
		t.Fatalf("非默认分支的 push：%v，想要 %v", got, want)
	}
	// 删除分支同样通知
	h.deliverOK("push", h.deletePayload(h.demo, "refs/heads/feature", head))
	if got, want := h.queue.take(), []string{fmt.Sprintf("notify %d#0", demoRepoID)}; !slices.Equal(got, want) {
		t.Fatalf("删除分支：%v，想要 %v", got, want)
	}
}

package review

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// 同步规划的限制：同一去重键的规划在进程内串行，拿锁后二次查库复用已提交的请求；
// 等待锁受总预算约束，服务关闭时立即中断；锁条目用完即回收。

// TestConcurrentPlanFetchesOnce：同一个事件的两个并发规划只有一个新建请求、只拉取一次文档，另一个拿到已提交的请求。
func TestConcurrentPlanFetchesOnce(t *testing.T) {
	h := newHarness(t)
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}}
	h.docsService(fp)
	h.bindDocs("col1")
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")

	ids := make([]int64, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个调用各自构造事件，避免共享可变状态。
			ids[i], errs[i] = h.rev.PlanEvent(h.ctx, h.prEvent("opened", head, base))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发规划 %d：%v", i, err)
		}
	}
	if (ids[0] == 0) == (ids[1] == 0) {
		t.Fatalf("应恰好一个调用新建请求：%d / %d", ids[0], ids[1])
	}
	if fp.lists != 1 {
		t.Fatalf("并发的同键规划只应拉取一次文档，实际 %d", fp.lists)
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_requests WHERE repo_id=$1`, testRepoID); n != 1 {
		t.Fatalf("应只有一个请求，实际 %d", n)
	}
	var sets int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM doc_snapshot_sets`).Scan(&sets); err != nil || sets != 1 {
		t.Fatalf("应只有一个快照集：%d, %v", sets, err)
	}
}

// TestKeyedLocks：按 key 串行、等待可取消、条目在最后一个使用者结束后回收。
func TestKeyedLocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newKeyedLocks()
	unlock, err := l.acquire(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	// 第二个调用要等：ctx 取消后返回错误，不持有锁。
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(waitCtx, "k"); err == nil {
		t.Fatal("锁被持有时等待应受 ctx 约束")
	}
	// 不同的 key 互不影响。
	other, err := l.acquire(ctx, "other")
	if err != nil {
		t.Fatalf("不同的 key 不应互相阻塞：%v", err)
	}
	other()
	unlock()
	l.mu.Lock()
	n := len(l.m)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("锁条目应回收，仍有 %d 个", n)
	}
}

// TestOpContextLifecycleBudget：同步规划的预算在创建时固定为 docsync.FreezeTimeout（等待去重锁也在这份预算里，
// 拿锁后不会重新计时），并随服务生命周期取消；门闩已关（服务正在退出）时拒绝开始新的操作。
func TestOpContextLifecycleBudget(t *testing.T) {
	t.Parallel()
	base, cancelBase := context.WithCancel(context.Background())
	var tracked atomic.Int32
	var mu sync.Mutex
	closed := false
	p := New(Deps{BaseCtx: base, Track: func() (func(), error) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return nil, context.Canceled
		}
		tracked.Add(1)
		return func() { tracked.Add(-1) }, nil
	}})
	ctx, cancel, err := p.opContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("同步规划应有总预算")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > docsync.FreezeTimeout {
		t.Fatalf("预算 %v 应不超过 %v", remaining, docsync.FreezeTimeout)
	}
	if tracked.Load() != 1 {
		t.Fatalf("应登记 1 次在途操作，实际 %d", tracked.Load())
	}
	cancelBase()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("规划 context 没有随服务生命周期取消")
	}
	cancel()
	if tracked.Load() != 0 {
		t.Fatalf("规划结束后应释放登记，实际 %d", tracked.Load())
	}
	mu.Lock()
	closed = true
	mu.Unlock()
	if _, _, err := p.opContext(context.Background()); err == nil {
		t.Fatal("门闩关闭后应拒绝开始新的规划")
	}
}

// TestPlanLockWaitSharesBudget：同一去重键的等待锁使用规划自己的预算，服务生命周期取消时立即中断（不会等满 10 分钟）。
func TestPlanLockWaitSharesBudget(t *testing.T) {
	t.Parallel()
	base, cancelBase := context.WithCancel(context.Background())
	p := New(Deps{BaseCtx: base})
	hold, err := p.locks.acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	ctx, cancel, err := p.opContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// 等待锁用的是规划自己的 deadline：不会在拿锁后重新得到一份完整的 10 分钟。
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > docsync.FreezeTimeout {
		t.Fatalf("等待锁应使用同一份预算：deadline=%v ok=%v", deadline, ok)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelBase()
	}()
	start := time.Now()
	unlock, err := p.locks.acquire(ctx, "k")
	if err == nil {
		unlock()
		t.Fatal("锁被持有时不应拿到锁")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("等待锁 %v 才中断：没有使用同一份预算", waited)
	}
}

// TestPlanInterruptedByLifecycle：外部文档拉取进行中服务生命周期取消失败（进程退出 / Close），规划立即失败、不建请求。
func TestPlanInterruptedByLifecycle(t *testing.T) {
	h := newHarness(t)
	base, cancelBase := context.WithCancel(context.Background())
	h.rev.baseCtx = base
	started := make(chan struct{})
	var once sync.Once
	fp := &docFetch{docs: []docprovider.Document{fetchDoc("d1", "a", 1, "正文")}, delay: 30 * time.Second}
	fp.listHook = func() { once.Do(func() { close(started) }) }
	h.docsService(fp)
	h.bindDocs("col1")
	cfgBase := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	errc := make(chan error, 1)
	go func() {
		_, err := h.rev.PlanEvent(context.Background(), h.prEvent("opened", head, cfgBase))
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("规划没有开始拉取文档")
	}
	cancelBase()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("服务生命周期取消后规划应失败")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("规划没有随服务生命周期中断")
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_requests WHERE repo_id=$1`, testRepoID); n != 0 {
		t.Fatalf("中断的规划不应建请求，实际 %d", n)
	}
}

package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOpGateCloseWaitsForOps：Close 取消在途操作、等它们结束后才返回（关闭数据库之前调用），之后不再接纳新操作。
func TestOpGateCloseWaitsForOps(t *testing.T) {
	t.Parallel()
	g := newOpGate()
	done, err := g.Track()
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		g.Close()
	}()
	select {
	case <-finished:
		t.Fatal("Close 不应在在途操作结束前返回")
	case <-time.After(100 * time.Millisecond):
	}
	done()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 在操作结束后仍未返回")
	}
	if _, err := g.Track(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close 之后 Track 应返回 context.Canceled，实际 %v", err)
	}
}

// TestOpGateStopsAccepting：StopAccepting 立即取消生命周期 context 并拒绝新操作，但不等待在途操作。
func TestOpGateStopsAccepting(t *testing.T) {
	t.Parallel()
	g := newOpGate()
	done, err := g.Track()
	if err != nil {
		t.Fatal(err)
	}
	g.StopAccepting()
	if err := g.Context().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("生命周期 context 应已取消：%v", err)
	}
	if _, err := g.Track(); !errors.Is(err, context.Canceled) {
		t.Fatalf("停止接纳后 Track 应返回 context.Canceled，实际 %v", err)
	}
	// 在途操作仍可结束；Close 等它结束后才返回。
	done()
	g.Close()
	g.StopAccepting() // 可重复调用
	g.Close()
}

// TestOpGateCloseBarrier：Close 与并发进入的新操作之间没有 Add/Wait 竞态：Close 返回后不再有任何成功的登记，
// 且所有成功登记的操作都已在 Close 返回前结束（在 race 检测下运行）。
func TestOpGateCloseBarrier(t *testing.T) {
	t.Parallel()
	g := newOpGate()
	var started, finished atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 200 {
				done, err := g.Track()
				if err != nil {
					continue // 门闩已关：绝不允许再开始操作
				}
				started.Add(1)
				time.Sleep(time.Millisecond) // 让 Close 必须等待在途操作
				finished.Add(1)
				done()
			}
		})
	}
	time.Sleep(5 * time.Millisecond)
	g.Close()
	// Close 返回时：所有成功登记的操作都已结束，之后不会再出现新的成功登记。
	s, f := started.Load(), finished.Load()
	if s != f {
		t.Fatalf("登记 %d 次、结束 %d 次：Close 返回时仍有在途操作", s, f)
	}
	after := make(chan struct{})
	go func() {
		wg.Wait()
		close(after)
	}()
	select {
	case <-after:
	case <-time.After(10 * time.Second):
		t.Fatal("并发登记在 Close 之后仍未结束")
	}
	if later := started.Load(); later != s {
		t.Fatalf("Close 之后又登记了 %d 次操作：门闩没有关闭", later-s)
	}
	if _, err := g.Track(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close 之后 Track 应返回 context.Canceled，实际 %v", err)
	}
}

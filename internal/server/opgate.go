package server

import (
	"context"
	"sync"
)

// opGate 是同步操作（AI 审计的同步规划、文档冻结）的生命周期门闩（审计后的修正）。
//
// 停止接纳与等待在途操作使用同一把锁：新操作在锁内检查 closed 并 Add(1)，Close 在锁内置 closed 并取消，解锁后才 Wait。
// 这样不会出现"新操作读到 baseCtx 仍有效 → Close 观察到计数为 0 并关闭数据库 → 新操作再 Add 并查库"的
// Add/Wait 竞态（WaitGroup 的 Add 与 Wait 并发是不允许的）。
type opGate struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// newOpGate 返回一个打开的门闩。
func newOpGate() *opGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &opGate{ctx: ctx, cancel: cancel}
}

// Context 返回服务生命周期 context：停止接纳时取消。
func (g *opGate) Context() context.Context { return g.ctx }

// Track 登记一次进行中的同步操作，返回的 done 在操作结束时调用。门闩已关闭（服务正在退出）时返回 context.Canceled，
// 调用方不得再开始操作，也不得再查数据库。
func (g *opGate) Track() (done func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, context.Canceled
	}
	g.wg.Add(1)
	return g.wg.Done, nil
}

// StopAccepting 停止接纳新的同步操作并取消在途操作；不等待。可重复调用。
func (g *opGate) StopAccepting() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	g.cancel()
}

// Close 停止接纳、取消在途操作并等待它们结束（关闭数据库之前调用）。可重复调用。
func (g *opGate) Close() {
	g.StopAccepting()
	g.wg.Wait()
}

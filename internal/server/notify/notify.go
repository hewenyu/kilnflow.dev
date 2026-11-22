// Package notify 提供进程内的唤醒信号：长轮询中的 Acquire 和各后台循环（ingest、outbox、发布）
// 在有新工作时被唤醒，而不必等到下一次轮询。
package notify

import "sync"

// broadcaster 唤醒所有等待者（用于唤醒长轮询中的 Acquire）。
type broadcaster struct {
	mu sync.Mutex
	ch chan struct{}
}

// newBroadcaster 返回一个 broadcaster。
func newBroadcaster() *broadcaster { return &broadcaster{ch: make(chan struct{})} }

// Wait 返回一个通道，下一次 Notify 时关闭。
func (b *broadcaster) Wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ch
}

// Notify 唤醒当前所有等待者。
func (b *broadcaster) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.ch)
	b.ch = make(chan struct{})
}

// kick 非阻塞地向信号通道发送一个信号。
func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Hub 汇集 Server 的全部唤醒信号。零值不可用，用 NewHub 创建。
type Hub struct {
	jobs    *broadcaster
	ingest  chan struct{}
	outbox  chan struct{}
	publish chan struct{}
}

// NewHub 返回一个 Hub。
func NewHub() *Hub {
	return &Hub{
		jobs:    newBroadcaster(),
		ingest:  make(chan struct{}, 1),
		outbox:  make(chan struct{}, 1),
		publish: make(chan struct{}, 1),
	}
}

// All 唤醒等待中的 Acquire、outbox 和发布 worker。
func (h *Hub) All() {
	h.jobs.Notify()
	kick(h.outbox)
	kick(h.publish)
}

// KickIngest 唤醒事件处理循环。
func (h *Hub) KickIngest() { kick(h.ingest) }

// KickOutbox 唤醒 outbox 投递循环（写入新的 outbox 记录之后调用）。
func (h *Hub) KickOutbox() { kick(h.outbox) }

// JobsChanged 返回一个通道，下一次 All 时关闭（Acquire 长轮询等待它）。
func (h *Hub) JobsChanged() <-chan struct{} { return h.jobs.Wait() }

// Ingest 是事件处理循环的唤醒通道。
func (h *Hub) Ingest() <-chan struct{} { return h.ingest }

// Outbox 是 outbox 投递循环的唤醒通道。
func (h *Hub) Outbox() <-chan struct{} { return h.outbox }

// Publish 是发布 worker 的唤醒通道。
func (h *Hub) Publish() <-chan struct{} { return h.publish }

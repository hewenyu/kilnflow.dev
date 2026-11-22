package review

import (
	"context"
	"fmt"
	"sync"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// 审计的同步规划（PLAN #30.6）：独立 PR 与手动全面审计要在规划时冻结外部文档，这一步会访问 Outline，
// 可能比一次 webhook 处理的期限更久。因此规划使用一份独立的服务端预算（与工作项冻结相同），并满足：
//   - 同一进程里同一个去重键的规划串行（第二个调用拿到锁后再查一次库，直接复用已提交的请求，不重复拉取）；
//   - 拿锁等待也受总预算约束，不会无限堆积；
//   - 服务生命周期（信号退出、HTTP 异常退出、Close）取消时立即中断，并在关闭数据库前等待进行中的操作结束。

// keyedLocks 按 key 串行化同一进程内的同步规划。条目按使用者计数，最后一个使用者结束后删除。
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

// keyedEntry 是一把可取消的互斥锁：ch 容量为 1，写入即持有。refs 是持有 + 正在等待的调用数。
type keyedEntry struct {
	ch   chan struct{}
	refs int
}

// newKeyedLocks 返回空的 keyedLocks。
func newKeyedLocks() *keyedLocks { return &keyedLocks{m: map[string]*keyedEntry{}} }

// acquire 取得 key 的锁，返回释放函数。等待锁期间 ctx 取消时返回 ctx 的错误（不持有锁）。
func (k *keyedLocks) acquire(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{ch: make(chan struct{}, 1)}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	releaseRef := func() {
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case e.ch <- struct{}{}:
		return func() {
			<-e.ch
			releaseRef()
		}, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// opContext 返回一次同步规划的服务端预算：不受调用方取消影响、总时限 docsync.FreezeTimeout，并随服务生命周期取消。
// 返回的 cancel 停止生命周期挂钩并释放预算；调用方应在规划结束时调用。
func (p *Reviews) opContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if p.baseCtx != nil {
		if err := p.baseCtx.Err(); err != nil {
			return nil, nil, fmt.Errorf("服务正在退出，跳过这次审计规划：%w", err)
		}
	}
	// 先在门闩内登记：门闩已关（服务正在退出）时直接拒绝，避免 Close 观察到计数为 0 后这里再 Add 并查库。
	var done func()
	if p.track != nil {
		d, err := p.track()
		if err != nil {
			return nil, nil, fmt.Errorf("服务正在退出，跳过这次审计规划：%w", err)
		}
		done = d
	}
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), docsync.FreezeTimeout)
	var stop func() bool
	if p.baseCtx != nil {
		stop = context.AfterFunc(p.baseCtx, cancel)
	}
	return octx, func() {
		if stop != nil {
			stop()
		}
		cancel()
		if done != nil {
			done()
		}
	}, nil
}

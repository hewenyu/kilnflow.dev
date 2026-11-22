package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// outbox 投递的重试策略：指数退避，最长约 17 分钟；超过 maxTries 次后放弃（state=dead）。
// GitHub 限流不算失败：推迟到恢复时间，不计入次数（deferRateLimited）。
const (
	maxTries     = 20
	maxBackoffLg = 10 // 退避时间 = 2^min(tries, maxBackoffLg) 秒
	batchSize    = 20
)

const (
	// pollInterval 是兜底轮询的间隔（有新记录时由 Hub 立即唤醒）。
	pollInterval = 2 * time.Second
	// deliverTimeout 是投递一条记录的超时。
	deliverTimeout = 2 * time.Minute
	// stateTimeout 是记录投递结果的时限：已经发出的请求有了结果就必须落盘，不随 worker 退出而中断，但必须有期限。
	stateTimeout = 15 * time.Second
	// maxErrorLen 是 outbox.last_error 保存的错误信息的上限。
	maxErrorLen = 2000
)

// Deps 是 Worker 的依赖。
type Deps struct {
	DB *store.DB
	// Hub 的 Outbox 通道在有新记录时唤醒 worker。
	Hub *notify.Hub
	Log *slog.Logger
}

// Worker 是 outbox 的投递 worker（v0 单实例，串行投递）。
type Worker struct {
	db  *store.DB
	hub *notify.Hub
	log *slog.Logger
	// handlers 是各领域注册的记录类型的处理函数（Handle），在 Run 之前注册。
	handlers map[string]Handler
	// onDead 是记录放弃时在同一事务中调用的处理函数（HandleDead），在 Run 之前注册。
	onDead map[string]DeadHandler
	// stateTimeout 是记录投递结果的时限；测试中可调小，验证慢投递返回后仍能落盘。
	stateTimeout time.Duration
}

// DeadHandler 在一条记录被放弃（state=dead）时与标记放弃在同一事务中调用，msg 是最后一次的错误。
type DeadHandler func(ctx context.Context, tx pgx.Tx, payload []byte, msg string) error

// HandleDead 注册记录类型 kind 被放弃时的处理函数（例如把 PR review 标记为发布失败）。必须在 Run 之前调用。
func (w *Worker) HandleDead(kind string, h DeadHandler) {
	if w.onDead == nil {
		w.onDead = map[string]DeadHandler{}
	}
	w.onDead[kind] = h
}

// Handler 投递一条记录：payload 是写入时的 payload。返回错误时按退避重试。
type Handler func(ctx context.Context, payload []byte) error

// Handle 注册记录类型 kind 的处理函数（例如 CI 的 Check Run、合并队列）。必须在 Run 之前调用。
func (w *Worker) Handle(kind string, h Handler) {
	if w.handlers == nil {
		w.handlers = map[string]Handler{}
	}
	w.handlers[kind] = h
}

// NewWorker 返回投递 worker。
func NewWorker(d Deps) *Worker {
	return &Worker{db: d.DB, hub: d.Hub, log: d.Log, stateTimeout: stateTimeout}
}

// Run 循环投递 outbox 记录，直到 ctx 结束。
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		for w.deliverBatch(ctx) > 0 {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.hub.Outbox():
		}
	}
}

// DeliverDue 投递全部到期的记录（模块测试用：不等投递循环）。
func (w *Worker) DeliverDue(ctx context.Context) {
	for w.deliverBatch(ctx) > 0 {
	}
}

// ErrGiveUp 表示不再自动重试（例如没有幂等键的写操作结果不明，C19）：记录直接标记为 dead，等人手动重试。
var ErrGiveUp = errors.New("不再自动重试，等待手动处理")

// pendingItem 是一条待投递的 outbox 记录（与 Item 不同，包含 payload，用于投递）。
type pendingItem struct {
	ID      int64
	Key     string
	Kind    string
	Payload []byte
	Tries   int
	Updated time.Time
}

func (w *Worker) deliverBatch(ctx context.Context) int {
	rows, err := w.db.Query(ctx, `SELECT id, coalesce(dedupe_key,''), kind, payload, attempts, updated_at FROM outbox
		WHERE state='pending' AND next_attempt_at <= now() ORDER BY id LIMIT $1`, batchSize)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("读取 outbox", "err", err)
		}
		return 0
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pendingItem])
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("读取 outbox", "err", err)
		}
		return 0
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return 0
		}
		w.deliverOne(ctx, it)
	}
	return len(items)
}

// deliverOne 投递一条 outbox 记录并更新它的状态。记录结果的写入用不随 worker 退出取消的 context：请求已经有结果时
// 必须落盘（成功的不重发、失败的不丢），否则下一轮启动时状态与实际不一致。
func (w *Worker) deliverOne(ctx context.Context, it pendingItem) {
	err := w.deliver(ctx, it)
	// 结果写入的 context 在投递返回之后才创建：投递本身最多 deliverTimeout，若提前创建，
	// 慢请求会把 stateTimeout 耗光，结果写不进去，下一轮只能重放。
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.stateTimeout)
	defer cancel()
	if err == nil {
		// 投递期间被重新写入（updated_at 变了）时保留 pending，下一轮再投递
		if _, err := w.db.Exec(sctx, `UPDATE outbox SET state=$3, last_error=NULL WHERE id=$1 AND updated_at=$2`, it.ID, it.Updated, store.OutboxDone); err != nil {
			w.log.Warn("更新 outbox 状态", "key", it.Key, "err", err)
		}
		return
	}
	if ctx.Err() != nil {
		// 服务退出、调用方取消：请求可能已经发出，结果不可信。不计入重试次数（不是投递失败），按当前次数退避后重试。
		w.recordInterrupted(sctx, it, err)
		return
	}
	if rl, ok := gh.RateLimited(err); ok {
		w.deferRateLimited(sctx, it, rl)
		return
	}
	tries := it.Tries + 1
	state := store.OutboxPending
	if tries >= maxTries || errors.Is(err, ErrGiveUp) {
		state = store.OutboxDead
		w.log.Error("outbox 投递失败，已放弃", "key", it.Key, "err", err)
	} else {
		w.log.Warn("outbox 投递失败", "key", it.Key, "tries", tries, "err", err)
	}
	msg := shorten.Text(err.Error(), maxErrorLen)
	if err := w.recordFailure(sctx, it, state, tries, msg, retryBackoff(tries)); err != nil {
		w.log.Warn("更新 outbox 状态", "key", it.Key, "err", err)
	}
}

// retryBackoff 返回第 tries 次失败后的等待时间（指数增长，封顶 2^maxBackoffLg 秒）。
func retryBackoff(tries int) time.Duration {
	return time.Duration(1<<min(tries, maxBackoffLg)) * time.Second
}

// recordInterrupted 记录一次因服务退出 / 取消而中断的投递：不计入重试次数，按当前次数退避后重试。
func (w *Worker) recordInterrupted(ctx context.Context, it pendingItem, err error) {
	msg := shorten.Text("投递被中断："+err.Error(), maxErrorLen)
	if rerr := w.recordFailure(ctx, it, store.OutboxPending, it.Tries, msg, retryBackoff(it.Tries)); rerr != nil {
		w.log.Warn("更新 outbox 状态", "key", it.Key, "err", rerr)
	}
}

// recordFailure 记录一次投递失败。放弃（dead）时在同一事务中调用这类记录注册的 DeadHandler（例如把 AI 审计的 PR review
// 标记为发布失败）。投递期间记录被重新写入（手动重试、新的报告）时 updated_at 不符，什么也不改。
func (w *Worker) recordFailure(ctx context.Context, it pendingItem, state string, tries int, msg string, backoff time.Duration) error {
	return w.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outbox SET state=$2, attempts=$3, last_error=$4, next_attempt_at=now()+make_interval(secs => $5)
			WHERE id=$1 AND updated_at=$6`, it.ID, state, tries, msg, backoff.Seconds(), it.Updated)
		if err != nil || tag.RowsAffected() == 0 || state != store.OutboxDead {
			return err
		}
		if h := w.onDead[it.Kind]; h != nil {
			return h(ctx, tx, it.Payload, msg)
		}
		return nil
	})
}

// deferRateLimited 把被限流的记录推迟到恢复时间：不计入重试次数，所以限流再久也不会被标记为 dead。
// 限流中的 installation 不会再发出请求（gh.Client 直接返回 *gh.RateLimitError），
// 同一 installation 的其他记录也会在第一次尝试时推迟，不会忙循环。
func (w *Worker) deferRateLimited(ctx context.Context, it pendingItem, rl *gh.RateLimitError) {
	if rl.API != nil {
		w.log.Warn("outbox 投递被 GitHub 限流，暂停该 installation 的投递", "key", it.Key, "installation", rl.Installation,
			"kind", rl.Kind, "until", rl.Until, "err", rl.API)
	} else {
		w.log.Debug("outbox 推迟到 GitHub 限流恢复", "key", it.Key, "installation", rl.Installation, "until", rl.Until)
	}
	if _, err := w.db.Exec(ctx, `UPDATE outbox SET last_error=$2, next_attempt_at=greatest($3::timestamptz, now()) WHERE id=$1 AND updated_at=$4`,
		it.ID, shorten.Text(rl.Error(), maxErrorLen), rl.Until, it.Updated); err != nil {
		w.log.Warn("更新 outbox 状态", "key", it.Key, "err", err)
	}
}

func (w *Worker) deliver(ctx context.Context, it pendingItem) error {
	ctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	if h := w.handlers[it.Kind]; h != nil {
		return h(ctx, it.Payload)
	}
	return fmt.Errorf("未知的 outbox 类型 %s", it.Kind)
}

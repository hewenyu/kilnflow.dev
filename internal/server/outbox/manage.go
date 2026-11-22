package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// outbox 记录的查看与手动重试（CLI 和 Dashboard 使用）。

// ErrNotFound 表示要重试的 outbox 记录不存在。
var ErrNotFound = errors.New("outbox 记录不存在")

// Item 是一条 outbox 记录的摘要。
type Item struct {
	ID        int64
	DedupeKey string
	Kind      string
	State     string
	Attempts  int32
	LastError string
	UpdatedAt time.Time
}

// List 返回状态在 states 中的 outbox 记录，按 ID 倒序，最多 limit 条。
func List(ctx context.Context, q store.Querier, states []string, limit int) ([]*Item, error) {
	rows, err := q.Query(ctx, `SELECT id, coalesce(dedupe_key,''), kind, state, attempts, coalesce(last_error,''), updated_at
		FROM outbox WHERE state = ANY($1) ORDER BY id DESC LIMIT $2`, states, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[Item])
}

// RetryHook 在手动重试一条记录时与重新排队在同一事务中调用（例如把发布失败的 PR review 恢复为 pending）。
type RetryHook func(ctx context.Context, tx pgx.Tx, payload []byte) error

// Retry 把 outbox 记录重新排队（清零重试次数，立即投递），并调用这类记录注册的 RetryHook。记录不存在时返回 ErrNotFound。
// 在调用方的事务中执行；投递 worker 由调用方在提交后唤醒。
func Retry(ctx context.Context, tx pgx.Tx, id int64, hooks map[string]RetryHook) error {
	var kind string
	var payload []byte
	err := tx.QueryRow(ctx, `UPDATE outbox SET state=$2, attempts=0, next_attempt_at=now(), updated_at=now() WHERE id=$1 RETURNING kind, payload`,
		id, store.OutboxPending).Scan(&kind, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if h := hooks[kind]; h != nil {
		return h(ctx, tx, payload)
	}
	return nil
}

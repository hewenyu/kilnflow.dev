// Package store 是 kiln-server 的数据层：模型、状态类型、事务，以及各组件共用的查询
// （仓库、run、job、environment、变量、secret、审计）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier 是可以执行查询的连接（*DB、*pgxpool.Pool 或 pgx.Tx）。
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Execer 是可以执行写语句的连接（*DB、*pgxpool.Pool 或 pgx.Tx）。
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DB 是 Server 的数据库连接池。直接执行的语句使用嵌入的 *pgxpool.Pool，事务使用 Tx。
type DB struct {
	*pgxpool.Pool
	// Log 记录事务因死锁或序列化失败而重试（为 nil 时不记录）。
	Log *slog.Logger
	// retries 是 Tx 因死锁或序列化失败重试的累计次数。
	retries atomic.Int64
}

// New 包装连接池。
func New(pool *pgxpool.Pool) *DB { return &DB{Pool: pool} }

const (
	// txMaxAttempts 是事务遇到死锁或序列化失败时的最多尝试次数。
	txMaxAttempts = 5
	// txRetryBackoff 是重试前等待时间的基数：第 n 次失败后等待 n 倍。
	txRetryBackoff = 10 * time.Millisecond
)

// 可以重试整个事务的 PostgreSQL 错误码（SQLSTATE）。
const (
	pgDeadlockDetected     = "40P01"
	pgSerializationFailure = "40001"
)

// Tx 在一个事务中执行 f，遇到死锁或序列化失败时重试。
// 重试次数可以用 Retries 读取：正常情况下应为 0，持续增长说明有加锁顺序相反的路径（见 runs.go 中 LockRuns 前的加锁顺序）。
func (db *DB) Tx(ctx context.Context, f func(tx pgx.Tx) error) error {
	return txOn(ctx, db.Pool, f, func(pe *pgconn.PgError, attempt int) {
		db.retries.Add(1)
		if db.Log != nil {
			db.Log.Warn("事务重试", "sqlstate", pe.Code, "attempt", attempt, "err", pe.Message)
		}
	})
}

// Retries 返回 Tx 因死锁或序列化失败而重试的累计次数。
func (db *DB) Retries() int64 { return db.retries.Load() }

// Beginner 是可以开始事务的连接（*pgxpool.Pool、*pgxpool.Conn 等）。
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// TxOn 在 b 上的一个事务中执行 f，遇到死锁或序列化失败时重试。需要在指定连接上执行时使用
// （例如持有 session 级 advisory lock 的连接，不再从连接池借第二个连接）。
func TxOn(ctx context.Context, b Beginner, f func(tx pgx.Tx) error) error {
	return txOn(ctx, b, f, nil)
}

// txOn 是 TxOn 的实现；onRetry 不为 nil 时在每次重试之前调用（attempt 是失败的第几次尝试，从 1 开始）。
func txOn(ctx context.Context, b Beginner, f func(tx pgx.Tx) error, onRetry func(pe *pgconn.PgError, attempt int)) error {
	var err error
	for i := range txMaxAttempts {
		err = pgx.BeginFunc(ctx, b, f)
		pe, ok := errors.AsType[*pgconn.PgError](err)
		retryable := ok && (pe.Code == pgDeadlockDetected || pe.Code == pgSerializationFailure)
		if !retryable || i == txMaxAttempts-1 {
			return err
		}
		if onRetry != nil {
			onRetry(pe, i+1)
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(i+1) * txRetryBackoff):
		}
	}
	return err
}

// LockXact 在当前事务中获取名为 key 的 advisory lock（事务级），直到事务结束。key 按 hashtextextended 映射为锁号，
// 各处的 key 用不同的前缀区分（例如并发组的 "concurrency:"、仓库配额的 "kiln:repo-quota:"）。
// 一个事务要锁多个 key 时，各方必须按同一顺序加锁，否则会互相等待（死锁）。
func LockXact(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// QueryIDs 执行只返回一列 bigint 的查询。
func QueryIDs(ctx context.Context, q Querier, sql string, args ...any) ([]int64, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// MustJSON 把 v 编码为 JSON（用于写入 jsonb 列）。v 无法编码说明是程序错误，直接 panic。
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// DocBindingLockKey 是按仓库串行化文档绑定变更（bind / unbind）的 advisory lock 键。
func DocBindingLockKey(repoID int64) string { return fmt.Sprintf("kiln:doc-binding:%d", repoID) }

// advisoryUnlockTimeout 是释放会话级 advisory lock 的时限：解锁也要有期限，不能因为连接卡死而永远不返回。
const advisoryUnlockTimeout = 5 * time.Second

// TryAdvisoryLock 在 conn 上尝试获取会话级 advisory lock（key），与 WithAdvisoryLock 是同一把键（hashtextextended）。
// 拿到时返回释放函数 ok 为真；没拿到时释放函数为 nil。释放函数有期限（advisoryUnlockTimeout），释放失败时关闭连接
// （可能仍持有会话锁的连接不能回到连接池）并返回错误。调用方必须在拿到锁之后调用释放函数。
func TryAdvisoryLock(ctx context.Context, conn *pgxpool.Conn, key string) (unlock func(context.Context) error, ok bool, err error) {
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&locked); err != nil {
		return nil, false, err
	}
	if !locked {
		return nil, false, nil
	}
	return func(ctx context.Context) error {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), advisoryUnlockTimeout)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			return errors.Join(fmt.Errorf("释放会话锁：%w", err), conn.Conn().Close(uctx))
		}
		return nil
	}, true, nil
}

// WithAdvisoryLock 在一条专用连接上持有会话级 advisory lock（key）执行 fn，结束后释放。fn 必须用传入的 conn 执行锁内事务
// （store.TxOn(ctx, conn, ...)），不要在持锁时再向同一连接池借连接——否则连接池较小时会与等待同一把锁的请求互相等待。
func (db *DB) WithAdvisoryLock(ctx context.Context, key string, fn func(context.Context, *pgxpool.Conn) error) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), advisoryUnlockTimeout)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			// 解锁结果不明：这条连接可能仍持有会话锁，绝不能放回池中给别人复用。关闭底层连接，pgxpool 会丢弃它。
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	return fn(ctx, conn)
}

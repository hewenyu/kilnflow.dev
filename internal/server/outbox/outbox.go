// Package outbox 实现 kiln-server 的 outbox 投递机制（PLAN #7.8、#30.3）：领域在业务事务中写入"要同步哪个对象"，
// 投递 worker 异步调用该类记录注册的处理函数（各领域在组装时注册，例如 CI 的 Check Run、AI 的 PR review、合并队列）。
//
// 本包只有机制，不含领域的表和判断。
package outbox

import (
	"context"
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 记录只保存"要同步哪个对象"，投递时读取最新状态，所以同一对象的多次更新天然合并。

// outbox 记录的类型。
const (
	// KindPRVerified 是 PR 的一个验证（CI 的 PR run、AI 审计请求）结束的事件（PLAN #30.3）：由组装层注册的处理函数通知合并队列、
	// 提名自动入队、通知 AI 开发的执行者。领域在结束的事务中写入，不直接访问其他领域的表。
	KindPRVerified = "pr.verified"
)

// Writer 在调用方的事务中写入 outbox 记录。
type Writer struct {
	enabled bool
}

// NewWriter 返回 Writer。enabled 为 false（没有配置 GitHub App）时不写入任何记录。
func NewWriter(enabled bool) *Writer { return &Writer{enabled: enabled} }

// Enabled 表示是否写入记录（配置了 GitHub App）。
func (w *Writer) Enabled() bool { return w.enabled }

// enqueue 写入一条 outbox 记录。同一 key 已有记录时合并：换成新的 payload 并重新排队（清零重试次数，立即投递）。
// cond 不为空时只在条件成立时写入（SQL 条件，参数从 $4 起依次为 condArgs）。
func enqueue(ctx context.Context, q store.Execer, key, kind string, payload any, cond string, condArgs ...any) error {
	if cond == "" {
		cond = "true"
	}
	_, err := q.Exec(ctx, `INSERT INTO outbox (dedupe_key, kind, installation_id, payload) SELECT $1, $2, 0, $3 WHERE `+cond+`
		ON CONFLICT (dedupe_key) DO UPDATE SET payload=EXCLUDED.payload, state='pending', next_attempt_at=now(), attempts=0, updated_at=now()`,
		append([]any{key, kind, store.MustJSON(payload)}, condArgs...)...)
	return err
}

// PRVerifiedPayload 是 KindPRVerified 的 payload。
type PRVerifiedPayload struct {
	RepoID int64 `json:"repo_id"`
	PR     int   `json:"pr"`
	// Passed 表示这个验证没有失败（成功或跳过）：可以提名自动入队。
	Passed bool `json:"passed"`
}

// Enqueue 写入一条 outbox 记录（kind 由注册了处理函数的领域定义，key 要带领域前缀，PLAN #30.4）。同一 key 已有记录时合并。
// 没有配置 GitHub App 时不写入。
func (w *Writer) Enqueue(ctx context.Context, q store.Execer, key, kind string, payload any) error {
	if !w.enabled {
		return nil
	}
	return enqueue(ctx, q, key, kind, payload, "")
}

// EnqueueIf 与 Enqueue 相同，但只在 SQL 条件 cond 成立时写入（与写入是同一条语句）；cond 的参数从 $4 起依次为 args。
func (w *Writer) EnqueueIf(ctx context.Context, q store.Execer, key, kind string, payload any, cond string, args ...any) error {
	if !w.enabled {
		return nil
	}
	return enqueue(ctx, q, key, kind, payload, cond, args...)
}

// PRVerified 写入 PR pr 的一个验证结束的事件（KindPRVerified）。通过和没有通过的分开合并：没有通过的不会覆盖通过的提名。
func (w *Writer) PRVerified(ctx context.Context, q store.Execer, repoID int64, pr int, passed bool) error {
	return w.Enqueue(ctx, q, fmt.Sprintf("pr-verified:%d:%d:%t", repoID, pr, passed), KindPRVerified,
		PRVerifiedPayload{RepoID: repoID, PR: pr, Passed: passed})
}

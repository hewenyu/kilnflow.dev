// Package mqevents 是合并领域（合并队列、自动打 tag）的 outbox 记录（PLAN #28.3、#29.4、#30.3）：在调用方的事务中通知合并队列和
// 自动打 tag 的执行者。CI、AI 不直接调用本包，PR 验证结束时写 outbox.KindPRVerified，由组装层转交。
package mqevents

import (
	"context"
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// outbox 记录的类型（已写入的记录按它投递，不能修改）。
const (
	// KindMergeQueue 通知合并队列的执行者检查一个仓库的队列（PLAN #28.3）。
	KindMergeQueue = "mergequeue.repo"
	// KindAutoTag 通知自动打 tag 的执行者处理一个仓库的 tag 记录（PLAN #29.4）。
	KindAutoTag = "autotag.repo"
)

// RepoPayload 是按仓库投递的记录（KindMergeQueue、KindAutoTag）的 payload。
type RepoPayload struct {
	RepoID int64 `json:"repo_id"`
}

// mergeQueueKey 返回仓库的合并队列通知的 dedupe_key（同一仓库合并为一条）。
func mergeQueueKey(repoID int64) string { return fmt.Sprintf("mergequeue:repo:%d", repoID) }

// EnqueueAutoTag 通知自动打 tag 的执行者处理仓库 repoID（同一仓库合并为一条，PLAN #29.4）。
func EnqueueAutoTag(ctx context.Context, w *outbox.Writer, q store.Execer, repoID int64) error {
	return w.Enqueue(ctx, q, fmt.Sprintf("autotag:repo:%d", repoID), KindAutoTag, RepoPayload{RepoID: repoID})
}

// EnqueueMergeQueue 通知合并队列的执行者检查仓库 repoID 的队列（PLAN #28.3）。q 可以是事务或连接池。
func EnqueueMergeQueue(ctx context.Context, w *outbox.Writer, q store.Execer, repoID int64) error {
	return w.Enqueue(ctx, q, mergeQueueKey(repoID), KindMergeQueue, RepoPayload{RepoID: repoID})
}

// NominateAutoMerge 记下 PR pr 可能可以自动入队（PLAN #29.5），并通知合并队列的执行者检查。PR 已经在队列中时什么也不写。
// 已经有候选时刷新它的时间：执行者只删除它读到的那一版，检查期间新到的提名不会被删掉。
func NominateAutoMerge(ctx context.Context, w *outbox.Writer, q store.Execer, repoID int64, pr int) error {
	if !w.Enabled() {
		return nil
	}
	tag, err := q.Exec(ctx, `INSERT INTO merge_auto_candidates (repo_id, pr_number)
		SELECT $1, $2 WHERE NOT EXISTS (SELECT 1 FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 AND state='queued')
		ON CONFLICT (repo_id, pr_number) DO UPDATE SET created_at=clock_timestamp()`, repoID, pr)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return EnqueueMergeQueue(ctx, w, q, repoID)
}

// KickMergeQueue 在仓库 repoID 有排队中的 PR 时通知合并队列的执行者（pr 非 0 时只看这个 PR 是否在队列中）；
// 队列为空时什么也不写。用于 PR 验证结束、PR 事件、默认分支 push 等只可能影响队列中的 PR 的变化。
func KickMergeQueue(ctx context.Context, w *outbox.Writer, q store.Execer, repoID int64, pr int) error {
	return w.EnqueueIf(ctx, q, mergeQueueKey(repoID), KindMergeQueue, RepoPayload{RepoID: repoID},
		`EXISTS (SELECT 1 FROM merge_queue WHERE repo_id=$4 AND state='queued' AND ($5=0 OR pr_number=$5))`, repoID, pr)
}

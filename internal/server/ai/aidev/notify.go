package aidev

import (
	"context"
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// KindRepo 是通知 AI 开发的执行者处理一个仓库的 outbox 记录（payload 为 repoPayload，由 HandleOutbox 投递，PLAN #26.11）。
// 同一仓库合并为一条。类型、去重键和 payload 与拆分前相同：升级时未投递的记录照常投递。
const KindRepo = "aidev.repo"

// repoPayload 是 KindRepo 记录的 payload。
type repoPayload struct {
	RepoID int64 `json:"repo_id"`
}

// repoKey 返回仓库的通知的去重键。
func repoKey(repoID int64) string { return fmt.Sprintf("aidev:repo:%d", repoID) }

// Enqueue 通知 AI 开发的执行者处理仓库 repoID。q 可以是事务或连接池。
func (s *Service) Enqueue(ctx context.Context, q store.Execer, repoID int64) error {
	return s.outbox.Enqueue(ctx, q, repoKey(repoID), KindRepo, repoPayload{RepoID: repoID})
}

// Conn 是既能查询又能写入的连接（*store.DB、pgx.Tx、*pgxpool.Conn）。
type Conn interface {
	store.Querier
	store.Execer
}

// Kick 在 PR pr 属于仓库 repoID 未结束的工作项时通知执行者（PR 的一个验证结束后判断是否返修）；不是工作项的 PR 时什么也不写。
// 组装层处理 PR 验证结束的事件（outbox.KindPRVerified）时调用。
func (s *Service) Kick(ctx context.Context, q Conn, repoID int64, pr int) error {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dev_work_items WHERE repo_id=$1 AND pr_number=$2 AND active)`,
		repoID, pr).Scan(&ok); err != nil || !ok {
		return err
	}
	return s.Enqueue(ctx, q, repoID)
}

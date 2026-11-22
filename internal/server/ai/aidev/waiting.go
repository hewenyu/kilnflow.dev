package aidev

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 依赖等待（PLAN #31.6）：选题时未选、写了 wait_for 的 Issue 的跳过记录带 waiting_on（还没关闭的依赖）。先登记、后复查，
// 两者不会互相错过：
//
//  1. 写跳过记录的事务中登记 waiting_on，标记 waiting_checked=false，同一事务通知仓库执行者（writeSkips）；
//  2. 任何 Issue 的 issues.closed 事件把它从所有 waiting_on 中去掉（releaseWaiters）；
//  3. 执行者对 waiting_checked=false 的记录读一次平台上依赖的状态，去掉已经关闭的，再标记已复查（recheckWaits）。
//
// waiting_on 空了就删除这条跳过记录，任务池随之变化，触发选题。依赖关闭之后又重新打开，不再加回等待（Issue 回到候选，
// Agent 重新判断）。手动同步任务池时把有依赖的记录重新标记为未复查，补上丢失的关闭事件。

// releaseWaiters 在 Issue number 关闭时把它从仓库的所有 waiting_on 中去掉（第 2 步），等待清空的跳过记录删除。返回删除的数量。
func (s *Service) releaseWaiters(ctx context.Context, repoID int64, number int) (int, error) {
	var released int
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE dev_issue_skips SET waiting_on=array_remove(waiting_on, $2)
			WHERE repo_id=$1 AND $2 = ANY(waiting_on) RETURNING issue_number`, repoID, number)
		if err != nil {
			return err
		}
		touched, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil || len(touched) == 0 {
			return err
		}
		if released, err = dropReleased(ctx, tx, repoID, touched); err != nil {
			return err
		}
		return s.Enqueue(ctx, tx, repoID)
	})
	return released, err
}

// dropReleased 删除 issues 中等待已经清空的跳过记录（Issue 回到候选），有删除时记下任务池变化。返回删除的数量。
func dropReleased(ctx context.Context, tx pgx.Tx, repoID int64, issues []int) (int, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM dev_issue_skips WHERE repo_id=$1 AND issue_number = ANY($2) AND cardinality(waiting_on)=0`,
		repoID, issues)
	if err != nil || tag.RowsAffected() == 0 {
		return 0, err
	}
	return int(tag.RowsAffected()), poolChanged(ctx, tx, repoID)
}

// waitingSkip 是一条还没复查的等待记录。
type waitingSkip struct {
	Issue     int
	WaitingOn []int
}

// recheckWaits 复查仓库还没复查的等待记录（第 3 步，持有仓库锁）：逐个读依赖的状态，去掉已经关闭（或者已经不存在）的。
// 读取失败时记录不变，下一次处理重试；期间关闭事件改过这条记录时（waiting_on 不同）也不改，下一次按最新的内容复查。
func (p *pass) recheckWaits(ctx context.Context) error {
	rows, err := p.conn.Query(ctx, `SELECT issue_number, waiting_on FROM dev_issue_skips WHERE repo_id=$1 AND NOT waiting_checked
		ORDER BY issue_number`, p.repo.ID)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[waitingSkip])
	if err != nil {
		return err
	}
	for _, k := range list {
		open := make([]int, 0, len(k.WaitingOn))
		for _, dep := range k.WaitingOn {
			is, err := p.f.GetIssue(ctx, dep)
			switch {
			case forge.IsRejected(err):
				// 依赖不存在或者没有权限读：不能一直等下去，回到候选由 Agent 重新判断
				p.s.log.Warn("AI 开发：等待的依赖读不到，不再等待", "repo", p.repo.FullName(), "issue", k.Issue, "dep", dep, "err", err)
				continue
			case err != nil:
				return fmt.Errorf("读取 Issue #%d（#%d 等待它）：%w", dep, k.Issue, err)
			}
			if is.Open {
				open = append(open, dep)
			}
		}
		if err := p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE dev_issue_skips SET waiting_on=$3, waiting_checked=true
				WHERE repo_id=$1 AND issue_number=$2 AND NOT waiting_checked AND waiting_on=$4`, p.repo.ID, k.Issue, open, k.WaitingOn)
			if err != nil || tag.RowsAffected() == 0 || len(open) > 0 {
				return err
			}
			n, err := dropReleased(ctx, tx, p.repo.ID, []int{k.Issue})
			if err != nil || n == 0 {
				return err
			}
			return p.s.Enqueue(ctx, tx, p.repo.ID)
		}); err != nil {
			return err
		}
	}
	return nil
}

// markWaitsUnchecked 让仓库所有等待记录重新复查（手动同步任务池时，补上丢失的关闭事件）。
func markWaitsUnchecked(ctx context.Context, q store.Execer, repoID int64) error {
	_, err := q.Exec(ctx, `UPDATE dev_issue_skips SET waiting_checked=false WHERE repo_id=$1 AND cardinality(waiting_on) > 0`, repoID)
	return err
}

// waitingNote 返回等待依赖的说明（Dashboard 的任务池），没有依赖时为空。
func waitingNote(deps []int) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("等待")
	for _, d := range slices.Sorted(slices.Values(deps)) {
		fmt.Fprintf(&b, " #%d", d)
	}
	b.WriteString(" 关闭")
	return b.String()
}

package aidev

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 评论没有幂等键（C19，PLAN #26.12）：先在事务中写下意图（pending），执行者发送前改为 attempted 再调用，不重试；
// 发送时崩溃、或者结果不明（响应丢失、超时）的记为 unknown，在 Dashboard 显示，不自动重发。平台明确拒绝的记为 failed；
// 平台明确没有执行的（限流、4xx 之外的明确回答）回到 pending，下一次再发。

// 评论的状态（dev_comments.state）。
const (
	CommentPending   = "pending"
	CommentAttempted = "attempted"
	CommentDone      = "done"
	CommentUnknown   = "unknown"
	CommentFailed    = "failed"
)

// 评论的类别（dev_comments.kind）。
const (
	commentClaimed      = "claimed"
	commentBlocked      = "blocked"
	commentDispositions = "dispositions"
)

// maxCommentLen 是评论正文的上限（GitHub 限制 65536 字符，留出余量）。
const maxCommentLen = 60000

// commentStateTimeout 是记录评论结果的时限：已经发出的请求有了结果就必须落盘，不随执行者退出而中断，但必须有期限。
const commentStateTimeout = 15 * time.Second

// addComment 写下一条评论意图；同一个 key 已经写过时什么也不做。
func addComment(ctx context.Context, tx pgx.Tx, repoID, workItem int64, number int, kind, key, body string) error {
	_, err := tx.Exec(ctx, `INSERT INTO dev_comments (work_item_id, repo_id, number, kind, dedupe_key, body) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (dedupe_key) DO NOTHING`, workItem, repoID, number, kind, key, shorten.Text(body, maxCommentLen))
	return err
}

// claimComments 在每个领取的 Issue 上评论领取和计划（Agent 自报）。
func claimComments(ctx context.Context, tx pgx.Tx, repoID, workItem int64, sel *devreport.Selection) error {
	var refs []string
	for _, n := range sel.Selected {
		refs = append(refs, fmt.Sprintf("#%d", n))
	}
	var b strings.Builder
	phase := sel.FirstPhase()
	fmt.Fprintf(&b, "Kiln AI 开发领取了这个 Issue（%s，同一批：%s），分支 `%s`。\n\n", workItemRef(workItem), strings.Join(refs, " "),
		devreport.WorkItemBranch(workItem, phase, 1))
	if phase == PhaseDesign {
		b.WriteString("**方式**：先交付设计 PR（只改文档，不关闭 Issue），合并后自动开始实现。")
	} else {
		b.WriteString("**方式**：先设计再开发，一个 PR 完成。")
	}
	b.WriteString("理由（Agent 自报）：" + defuse(sel.DesignReason) + "\n\n")
	if sel.BatchReason != "" {
		b.WriteString("**合批理由**（Agent 自报）：" + defuse(sel.BatchReason) + "\n\n")
	}
	b.WriteString("**计划**（Agent 自报）：\n\n" + defuse(sel.Plan) + "\n\n")
	b.WriteString("去掉 `" + LabelReady + "` 或加上 `" + LabelSkip + "` 会终止这个工作项。")
	for _, n := range sel.Selected {
		if err := addComment(ctx, tx, repoID, workItem, n, commentClaimed, fmt.Sprintf("claimed:%d:%d", workItem, n), b.String()); err != nil {
			return err
		}
	}
	return nil
}

// blockedComment 写下受阻的评论：有 PR 时评论在 PR 上，否则在每个 Issue 上。每次进入受阻各写一次。
func (s *Service) blockedComment(ctx context.Context, tx pgx.Tx, w *WorkItem) error {
	retry := "再返修一轮"
	if !w.RoundRan {
		retry = "从最新的默认分支开始下一轮实现"
	}
	body := fmt.Sprintf("Kiln AI 开发的%s受阻：%s\n\n处理方式：`kiln ai dev retry %d`（%s）、`kiln ai dev stop %d`（终止），"+
		"或者直接在分支 `%s` 上修改。", workItemRef(w.ID), defuse(w.Reason), w.ID, retry, w.ID, w.Branch())
	var targets []int
	if w.PRNumber != nil {
		targets = []int{*w.PRNumber}
	} else {
		is, err := workIssues(ctx, tx, w.ID)
		if err != nil {
			return err
		}
		for _, i := range is {
			if !i.Closed {
				targets = append(targets, i.Number)
			}
		}
	}
	for _, n := range targets {
		var seq int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dev_comments WHERE work_item_id=$1 AND kind=$2 AND number=$3`,
			w.ID, commentBlocked, n).Scan(&seq); err != nil {
			return err
		}
		if err := addComment(ctx, tx, w.RepoID, w.ID, n, commentBlocked, fmt.Sprintf("blocked:%d:%d:%d", w.ID, n, seq+1), body); err != nil {
			return err
		}
	}
	return nil
}

// sendComments 发送仓库待发的评论（持有仓库锁）。上一次处理留下的 attempted（发送时崩溃）先记为 unknown。
func (s *Service) sendComments(ctx context.Context, conn *pgxpool.Conn, f forge.Repo, repoID int64) error {
	if _, err := conn.Exec(ctx, `UPDATE dev_comments SET state=$2, error='发送时中断，结果不明（不自动重发）', updated_at=now()
		WHERE repo_id=$1 AND state='attempted'`, repoID, CommentUnknown); err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT id, number, body FROM dev_comments WHERE repo_id=$1 AND state='pending' ORDER BY id`, repoID)
	if err != nil {
		return err
	}
	type pending struct {
		ID     int64
		Number int
		Body   string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return err
	}
	for _, c := range list {
		tag, err := conn.Exec(ctx, `UPDATE dev_comments SET state=$2, updated_at=now() WHERE id=$1 AND state=$3`, c.ID, CommentAttempted, CommentPending)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		cerr := f.Comment(ctx, c.Number, c.Body)
		state, msg := CommentDone, ""
		switch {
		case cerr == nil:
		case ctx.Err() != nil:
			// 服务退出 / 取消：请求可能已经发出，结果不明。不自动重发（C19），与发送时崩溃的处理相同。
			state, msg = CommentUnknown, "发送时中断，结果不明（不自动重发）"
		case forge.IsRejected(cerr):
			state, msg = CommentFailed, cerr.Error()
		case forge.Uncertain(cerr):
			state, msg = CommentUnknown, "结果不明（不自动重发）："+cerr.Error()
		default:
			// 平台明确没有执行（限流等）：回到 pending，下一次再发
			state, msg = CommentPending, cerr.Error()
		}
		// 结果的落盘不随调用方取消而中断：否则下一轮会把刚才发出去的评论当成没发过。
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commentStateTimeout)
		_, err = conn.Exec(sctx, `UPDATE dev_comments SET state=$2, error=$3, updated_at=now() WHERE id=$1`,
			c.ID, state, shorten.Text(msg, maxReasonLen))
		cancel()
		if err != nil {
			return err
		}
		if state == CommentPending {
			return fmt.Errorf("发送评论（#%d）：%w", c.Number, cerr)
		}
	}
	return nil
}

package mergequeue

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 关闭合并队列合并的 PR 关联的 issue（PLAN #28.3）：平台（GitHub）只在用户合并时按 PR 描述中的关键字
// （Closes #N 等）关闭 issue，App 合并时不会，所以由队列关闭。只处理队列自己合并的 PR；用户手动合并时平台自己关闭。
//
// 留言和关闭都没有幂等键，按"先写意图、最多尝试一次"处理（PLAN C19、#29.8）：
//   - 合并时与合并结果同一事务把队列项标记为 issues_state=pending（finishMerged），Server 重启也不会漏掉。
//   - 第一次处理时读一次关联的 issue，把当时打开的冻结到 issues（issue 号 → 状态）中。之后不再重新读取关联、不再按
//     "现在是否打开"决定要不要关闭：合并时已经关闭、之后被人重新打开的 issue 不会被关闭；读到滞后的数据也不会重复留言。
//   - 每个 issue：pending → 先记 commenting 再留言 → 先记 closing 再关闭 → closed。调用前就记下"已尝试"：
//     结果不明（响应丢失、记录之前重启）时不会再调用一次。重试时 commenting 不再留言、直接关闭；closing 读 issue：
//     已关闭 → closed，仍然打开 → unknown（结果不明、不再关闭，状态中写明交给人处理）。
//   - 平台明确拒绝（例如 App 没有 Issues 权限）时退回上一个状态，不影响合并结果：原因写在 status 中（Dashboard 显示），
//     下一次处理重试（兜底检查也会检查有 pending 项的仓库）；合并超过 closeIssuesGiveUp 仍失败时放弃（failed）。
//   - 限流时这次处理不再关闭，不计失败次数，等之后的处理（限流中的请求不会发出）；不阻塞队列。

// 队列项的 issues_state。
const (
	issuesPending = "pending"
	issuesDone    = "done"
	issuesFailed  = "failed"
)

// 冻结的每个 issue 的状态（merge_queue.issues 的值）。
const (
	issuePending    = "pending"
	issueCommenting = "commenting"
	issueClosing    = "closing"
	issueClosed     = "closed"
	issueUnknown    = "unknown"
)

// statusClosingIssues 是队列合并、还没关闭关联 issue 时的状态。
var statusClosingIssues = Note{Code: StatusClosingIssues}

// closeIssuesGiveUp 是合并之后仍然关闭失败时放弃的时间。
const closeIssuesGiveUp = 24 * time.Hour

// mergedEntry 是等待关闭关联 issue 的队列项。
type mergedEntry struct {
	ID       int64
	PR       int
	MergeSHA string
	Closed   []int32 // issues_closed：升级前已经关闭的（冻结时不放进去）
	// Issues 是冻结的关联 issue（issue 号 → 状态）；nil 表示还没有冻结。
	Issues   map[string]string
	Tries    int
	Finished time.Time
	// Uncertain 是升级前旧版本已经请求过关闭、结果不明的 issue（issues_uncertain，migration 0021）：冻结时按 closing 处理。
	Uncertain []int32
}

// closeIssues 关闭仓库中队列合并的 PR 关联的 issue（issues_state=pending 的项，按合并顺序）。只返回数据库的错误：
// 平台的失败记在队列项中，下一次处理重试。
func (p *pass) closeIssues(ctx context.Context) error {
	rows, err := p.conn.Query(ctx, `SELECT id, pr_number, merge_sha, issues_closed, issues, issues_tries, coalesce(finished_at, now()),
		issues_uncertain FROM merge_queue WHERE repo_id=$1 AND issues_state='pending' ORDER BY id`, p.repo.ID)
	if err != nil {
		return err
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[mergedEntry])
	if err != nil {
		return err
	}
	for i := range entries {
		e := &entries[i]
		err := p.closeIssuesOf(ctx, e)
		if err == nil {
			if err := p.finishIssues(ctx, e); err != nil {
				return err
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if forge.RateLimited(err) {
			// 同一平台的其他项也会被限流：这次不再关闭，等之后的处理
			p.s.log.Info("合并队列：关闭关联的 issue 被限流，稍后重试", "repo", p.repo.FullName(), "pr", e.PR, "err", err)
			return nil
		}
		if err := p.recordIssuesFailure(ctx, e, err); err != nil {
			return err
		}
	}
	return nil
}

// issueNumbers 返回冻结的 issue 号（升序）。
func (e *mergedEntry) issueNumbers() []int {
	out := make([]int, 0, len(e.Issues))
	for k := range e.Issues {
		if n, err := strconv.Atoi(k); err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// closeIssuesOf 处理 e 冻结的每个 issue，直到全部结束（closed / unknown）。返回平台的错误时由调用方记录、稍后重试。
func (p *pass) closeIssuesOf(ctx context.Context, e *mergedEntry) error {
	if e.Issues == nil {
		if err := p.freezeIssues(ctx, e); err != nil {
			return err
		}
	}
	for _, n := range e.issueNumbers() {
		if err := p.closeIssue(ctx, e, n); err != nil {
			return err
		}
	}
	return nil
}

// freezeIssues 读一次 PR 关联的、同一仓库中的 issue，把当时打开的（并且不是升级前已经关闭过的）冻结到队列项上；
// 升级前旧版本请求过关闭、结果不明的冻结为 closing。
func (p *pass) freezeIssues(ctx context.Context, e *mergedEntry) error {
	issues, err := p.f.ClosingIssues(ctx, e.PR)
	if err != nil {
		return fmt.Errorf("读取关联的 issue：%w", err)
	}
	frozen := map[string]string{}
	for _, is := range issues {
		n := int32(is.Number) //nolint:gosec // issue 号不会超过 int32
		switch {
		case !is.Open || slices.Contains(e.Closed, n):
		case slices.Contains(e.Uncertain, n):
			// 升级前旧版本请求过关闭、结果不明：不再留言、不再关闭，按 closing 读一次确认（仍然打开 → unknown）
			frozen[strconv.Itoa(is.Number)] = issueClosing
		default:
			frozen[strconv.Itoa(is.Number)] = issuePending
		}
	}
	tag, err := p.conn.Exec(ctx, `UPDATE merge_queue SET issues=$2, updated_at=now() WHERE id=$1 AND issues IS NULL`, e.ID, frozen)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// 不会发生（持有仓库锁）：保险起见按已有的冻结结果继续
		return p.conn.QueryRow(ctx, `SELECT issues FROM merge_queue WHERE id=$1`, e.ID).Scan(&e.Issues)
	}
	e.Issues = frozen
	return nil
}

// setIssue 记下 issue n 的状态（调用平台之前记下"已尝试"）。
func (p *pass) setIssue(ctx context.Context, e *mergedEntry, n int, state string) error {
	key := strconv.Itoa(n)
	if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET issues = issues || jsonb_build_object($2::text, $3::text), updated_at=now()
		WHERE id=$1`, e.ID, key, state); err != nil {
		return err
	}
	e.Issues[key] = state
	return nil
}

// closeIssue 推进一个 issue：留言（最多尝试一次）、关闭（最多尝试一次）。
func (p *pass) closeIssue(ctx context.Context, e *mergedEntry, n int) error {
	switch e.Issues[strconv.Itoa(n)] {
	case issuePending:
		if err := p.setIssue(ctx, e, n, issueCommenting); err != nil {
			return err
		}
		if err := p.f.Comment(ctx, n, closeComment(e)); err != nil {
			if !forge.Uncertain(err) {
				// 平台明确没有接受：退回，稍后重试
				return joinDB(fmt.Errorf("在 issue #%d 上留言：%w", n, err), p.setIssue(ctx, e, n, issuePending))
			}
			// 结果不明：可能已经留言，不再留言，继续关闭
			p.s.log.Warn("合并队列：留言的结果不明，不再重复留言", "repo", p.repo.FullName(), "pr", e.PR, "issue", n, "err", err)
		}
		return p.closeIssue(ctx, e, n)
	case issueCommenting:
		if err := p.setIssue(ctx, e, n, issueClosing); err != nil {
			return err
		}
		err := p.f.CloseIssue(ctx, n)
		switch {
		case err == nil:
			return p.recordClosed(ctx, e, n)
		case !forge.Uncertain(err):
			return joinDB(fmt.Errorf("关闭 issue #%d：%w", n, err), p.setIssue(ctx, e, n, issueCommenting))
		}
		// 结果不明：下一次处理读 issue 的状态判断（现在读可能读到旧数据）
		return fmt.Errorf("关闭 issue #%d 的结果不明，稍后确认：%w", n, err)
	case issueClosing:
		open, err := p.f.IssueOpen(ctx, n)
		if err != nil {
			return fmt.Errorf("读取 issue #%d：%w", n, err)
		}
		if !open {
			return p.recordClosed(ctx, e, n)
		}
		p.s.log.Warn("合并队列：关闭 issue 的结果不明，之后读到仍然打开，不再关闭", "repo", p.repo.FullName(), "pr", e.PR, "issue", n)
		return p.setIssue(ctx, e, n, issueUnknown)
	}
	return nil // closed、unknown
}

// joinDB 在平台错误之外加上记录状态的数据库错误（没有时原样返回平台错误）。
func joinDB(err, dbErr error) error {
	if dbErr != nil {
		return fmt.Errorf("%w（记录状态也失败：%w）", err, dbErr)
	}
	return err
}

// recordClosed 记下 issue 已经由 Kiln 关闭。
func (p *pass) recordClosed(ctx context.Context, e *mergedEntry, issue int) error {
	p.s.log.Info("合并队列：关闭 PR 关联的 issue", "repo", p.repo.FullName(), "pr", e.PR, "issue", issue, "merge_sha", e.MergeSHA)
	e.Issues[strconv.Itoa(issue)] = issueClosed
	return p.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE merge_queue SET issues = issues || jsonb_build_object($2::text, $3::text),
			issues_closed = array_append(issues_closed, $4::int), updated_at=now() WHERE id=$1`,
			e.ID, strconv.Itoa(issue), issueClosed, issue); err != nil {
			return err
		}
		return store.Audit(ctx, tx, auditActor, "mergequeue.issue_closed", map[string]any{
			"repo_id": p.repo.ID, "pr": e.PR, "issue": issue, "merge_sha": e.MergeSHA,
		})
	})
}

// finishIssues 记录关联的 issue 已经处理完（状态中列出关闭的和结果不明的）。
func (p *pass) finishIssues(ctx context.Context, e *mergedEntry) error {
	var closed, unknown []int
	for _, n := range e.issueNumbers() {
		switch e.Issues[strconv.Itoa(n)] {
		case issueClosed:
			closed = append(closed, n)
		case issueUnknown:
			unknown = append(unknown, n)
		}
	}
	// 没有关闭任何 issue（没有关联的、都已经关闭了）时没有状态
	st := Note{}
	if len(closed) > 0 || len(unknown) > 0 {
		st = Note{Code: StatusIssuesDone, Params: NoteParams{Closed: closed, Unknown: unknown}}
	}
	_, err := p.conn.Exec(ctx, `UPDATE merge_queue SET issues_state=$2, status=$3, status_code=$5, status_params=$6, updated_at=now()
		WHERE id=$1 AND issues_state=$4`, e.ID, issuesDone, st.Text(), issuesPending, st.Code, st.Params)
	return err
}

// recordIssuesFailure 记下关闭失败的原因（显示在 status 中）；合并超过 closeIssuesGiveUp 时放弃。
func (p *pass) recordIssuesFailure(ctx context.Context, e *mergedEntry, cause error) error {
	tries := e.Tries + 1
	params := NoteParams{Tries: tries, Detail: cause.Error(), Rejected: forge.IsRejected(cause)}
	state, code := issuesPending, StatusIssuesRetry
	if p.s.now().Sub(e.Finished) >= closeIssuesGiveUp {
		state, code = issuesFailed, StatusIssuesGaveUp
	}
	st := note(code, params)
	p.s.log.Warn("合并队列：关闭关联的 issue", "repo", p.repo.FullName(), "pr", e.PR, "tries", tries, "err", cause)
	_, err := p.conn.Exec(ctx, `UPDATE merge_queue SET issues_state=$2, issues_tries=$3, status=$4, status_code=$6, status_params=$7,
		updated_at=now() WHERE id=$1 AND issues_state=$5`,
		e.ID, state, tries, shorten.Text(st.Text(), maxReasonLen), issuesPending, st.Code, st.Params)
	return err
}

// closeComment 返回关闭 issue 时的留言。
func closeComment(e *mergedEntry) string {
	return fmt.Sprintf("**Kiln 合并队列**：PR #%d 已由合并队列合并（合并提交 %s），按 PR 中的关联关闭这个 issue。", e.PR, e.MergeSHA)
}

package aidev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// maxReasonLen 是工作项原因保存的上限。
const maxReasonLen = 2000

// auditActor 是审计日志中 AI 开发执行者的操作者。
const auditActor = "aidev"

// WorkItem 是 dev_work_items 的一行。
type WorkItem struct {
	ID             int64  `json:"id"`
	RepoID         int64  `json:"repo_id"`
	State          string `json:"state"`
	BaseRef        string `json:"base_ref"`
	BaseSHA        string `json:"base_sha"`
	PRNumber       *int   `json:"pr_number"`
	HeadSHA        string `json:"head_sha"`
	FixRounds      int    `json:"fix_rounds"`
	MaxFixRounds   int    `json:"max_fix_rounds"`
	Agent          string `json:"agent"`
	ClaimAttemptID int64  `json:"claim_attempt_id"`
	// DevAgents 是开发过这个工作项的 Agent（不能审计它的 PR，PLAN #33.7）。
	DevAgents           []string `json:"-"`
	Plan                string   `json:"plan"`
	TaskID              *int64   `json:"task_id"`
	DeliveryBranch      string   `json:"delivery_branch"`
	DeliverySHA         string   `json:"delivery_sha"`
	DeliveryTaskID      *int64   `json:"delivery_task_id"`
	FixedHead           string   `json:"fixed_head"`
	HoldLabel           bool     `json:"hold_label"`
	RequirementsChanged bool     `json:"requirements_changed"`
	LabelsState         string   `json:"-"`
	Reason              string   `json:"reason"`
	// Phase、DesignPR、DesignSHA、ImplRound、RoundBase、RoundRan 是阶段与轮次（PLAN #31.3）。
	Phase     string `json:"phase"`
	DesignPR  *int   `json:"design_pr"`
	DesignSHA string `json:"design_sha"`
	ImplRound int    `json:"impl_round"`
	RoundBase int    `json:"round_base"`
	RoundRan  bool   `json:"round_ran"`
	// HeadTaskID 是交付在正式分支上的任务（合并后是实际合并的那次交付）；DesignDocs 是已合并的设计文档。
	HeadTaskID *int64   `json:"head_task_id"`
	DesignDocs []string `json:"design_docs"`
	// ConflictHead、ConflictBase 是待处理的合并冲突（合并队列交回，HandBack）：PR head 与目标分支的哪个 commit 冲突；
	// FixedConflict 是已经为哪个冲突（conflictKey）开过返修；RoundConflicts 是本阶段、本轮做过的冲突返修数（与审计、
	// CI 返修分开计预算，PLAN #26.11）。
	ConflictHead   string     `json:"conflict_head"`
	ConflictBase   string     `json:"conflict_base"`
	FixedConflict  string     `json:"fixed_conflict"`
	RoundConflicts int        `json:"round_conflicts"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

// Branch 返回工作项本阶段、本轮的正式分支（PLAN #31.3）。
func (w *WorkItem) Branch() string { return devreport.WorkItemBranch(w.ID, w.Phase, w.ImplRound) }

// Pos 返回工作项在状态机中的位置。
func (w *WorkItem) Pos() Pos { return Pos{Phase: w.Phase, State: w.State} }

// RoundFixes 返回本阶段、本轮已经返修的轮数（fix_rounds 单调递增，PLAN #31.3），包括冲突返修。
func (w *WorkItem) RoundFixes() int { return w.FixRounds - w.RoundBase }

// RoundCheckFixes 返回本阶段、本轮因为审计、CI 没有通过（以及人工 retry）返修的轮数：冲突返修另计（RoundConflicts），
// 两者的上限都是 max_fix_rounds（PLAN #26.11）。
func (w *WorkItem) RoundCheckFixes() int { return w.RoundFixes() - w.RoundConflicts }

// DesignFirst 判断工作项是否先设计（有设计阶段）。
func (w *WorkItem) DesignFirst() bool { return w.Phase == PhaseDesign || w.DesignPR != nil }

const workItemCols = `id, repo_id, state, base_ref, base_sha, pr_number, head_sha, fix_rounds, max_fix_rounds, agent, claim_attempt_id,
	dev_agents, plan, task_id, delivery_branch, delivery_sha, delivery_task_id, fixed_head, hold_label, requirements_changed, labels_state,
	reason, phase, design_pr, design_sha, impl_round, round_base, round_ran, head_task_id, design_docs, conflict_head, conflict_base, fixed_conflict,
	round_conflicts, created_at, updated_at, finished_at`

func scanWorkItems(rows pgx.Rows) ([]*WorkItem, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*WorkItem, error) {
		var w WorkItem
		return &w, r.Scan(workItemDests(&w)...)
	})
}

// workItemDests 返回按 workItemCols 顺序扫描到 w 的目标指针；需要额外列时在其后追加。
func workItemDests(w *WorkItem) []any {
	return []any{
		&w.ID, &w.RepoID, &w.State, &w.BaseRef, &w.BaseSHA, &w.PRNumber, &w.HeadSHA, &w.FixRounds, &w.MaxFixRounds, &w.Agent,
		&w.ClaimAttemptID, &w.DevAgents, &w.Plan, &w.TaskID, &w.DeliveryBranch, &w.DeliverySHA, &w.DeliveryTaskID, &w.FixedHead,
		&w.HoldLabel, &w.RequirementsChanged, &w.LabelsState, &w.Reason, &w.Phase, &w.DesignPR, &w.DesignSHA, &w.ImplRound, &w.RoundBase,
		&w.RoundRan, &w.HeadTaskID, &w.DesignDocs, &w.ConflictHead, &w.ConflictBase, &w.FixedConflict, &w.RoundConflicts,
		&w.CreatedAt, &w.UpdatedAt, &w.FinishedAt,
	}
}

// forUpdate 是 where 条件的后缀：锁住读到的工作项。
const forUpdate = " FOR UPDATE"

// queryWorkItems 返回满足条件 where（参数 args，以 forUpdate 结尾时加锁）的工作项，按 id 升序。
func queryWorkItems(ctx context.Context, q store.Querier, where string, args ...any) ([]*WorkItem, error) {
	where, lock := strings.CutSuffix(where, forUpdate)
	sql := `SELECT ` + workItemCols + ` FROM dev_work_items WHERE ` + where + ` ORDER BY id`
	if lock {
		sql += forUpdate
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanWorkItems(rows)
}

// queryWorkItem 返回满足条件的第一个工作项，没有时为 nil。
func queryWorkItem(ctx context.Context, q store.Querier, where string, args ...any) (*WorkItem, error) {
	list, err := queryWorkItems(ctx, q, where, args...)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// GetWorkItem 返回工作项；不存在时返回 ErrNotFound。
func GetWorkItem(ctx context.Context, q store.Querier, id int64) (*WorkItem, error) {
	w, err := queryWorkItem(ctx, q, `id=$1`, id)
	if err == nil && w == nil {
		err = ErrNotFound
	}
	return w, err
}

// ErrNotFound 表示工作项不存在。
var ErrNotFound = errors.New("工作项不存在")

// IssueDigest 返回 Issue 内容（标题 + 正文）的摘要：领取时核对，跳过记录按它判断内容是否变了。
func IssueDigest(title, body string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte{0})
	h.Write([]byte(body))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// WorkIssue 是工作项包含的一个 Issue。
type WorkIssue struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Digest     string `json:"digest"`
	Active     bool   `json:"active"`
	Closed     bool   `json:"closed"`
	Resolution string `json:"resolution"`
}

// workIssues 返回工作项的 Issue（按编号）。
func workIssues(ctx context.Context, q store.Querier, workItem int64) ([]WorkIssue, error) {
	rows, err := q.Query(ctx, `SELECT issue_number, title, digest, active, closed, resolution FROM dev_work_item_issues
		WHERE work_item_id=$1 ORDER BY issue_number`, workItem)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[WorkIssue])
}

// issueNumbers 返回 Issue 的编号。
func issueNumbers(is []WorkIssue) []int {
	out := make([]int, 0, len(is))
	for _, i := range is {
		out = append(out, i.Number)
	}
	return out
}

// skipReasons 是写入 dev_issue_skips 的原因。
const (
	skipNotSelected = "选题时未选"
	skipNoChange    = "Agent 判断不需要改代码"
	skipStopped     = "工作项被终止"
)

// newRoundSet 是开始新的轮次（设计合并、部分完成的实现合并，PLAN #31.3）时清空上一个 PR 的状态：上一个 PR 得到的人工
// 确认（去掉 kiln:hold）不延续到下一个 PR，上一个 PR 的冲突也不带过来；fix_rounds 不归零（它是返修、实现任务去重键的
// 一部分），返修上限从这里重新算（冲突返修数归零）。
const newRoundSet = `pr_number=NULL, head_sha='', fixed_head='', task_id=NULL, delivery_branch='', delivery_sha='', hold_label=false,
	hold_pending=false, round_base=fix_rounds, round_ran=false, ` + clearConflict + `, fixed_conflict='', round_conflicts=0`

// clearConflict 清空待处理的冲突。
const clearConflict = `conflict_head='', conflict_base=''`

// transition 在事务 tx 中让工作项 w 收到事件 ev（PLAN #26.9、#31.3）：位置不变（Transition 返回 false）或者期间被别人
// 改过（状态、阶段、轮次任何一个不同）时返回 false，什么也不写。结束时（终态）释放 Issue（I1），无需改代码、终止的 Issue
// 记入跳过记录（内容不变就不再选），并通知执行者同步 label、触发下一次选题。设计合并、部分完成的实现合并在同一条
// UPDATE 中开始新的轮次。set 是同一条 UPDATE 中额外设置的列（"col=$n"，参数从 $9 开始：$1–$8 由 transition 使用）。
func (s *Service) transition(ctx context.Context, tx pgx.Tx, w *WorkItem, ev Event, reason string, set string, args ...any) (bool, error) {
	next, ok := Transition(w.Pos(), w.RoundRan, ev)
	if !ok {
		return false, nil
	}
	var sets []string
	switch {
	case w.Phase == PhaseDesign && next.Phase == PhaseImplement:
		sets = append(sets, "design_pr=pr_number", newRoundSet)
	case ev == EvMergedPartial && next.State == StateBlocked:
		sets = append(sets, "impl_round=impl_round+1", newRoundSet)
	}
	newRound := len(sets) > 0
	if set != "" {
		sets = append(sets, set)
	}
	extra := ""
	if len(sets) > 0 {
		extra = ", " + strings.Join(sets, ", ")
	}
	tag, err := tx.Exec(ctx, `UPDATE dev_work_items SET state=$2, phase=$6, active=$5, reason=$4, updated_at=now(),
		finished_at=CASE WHEN $5 THEN NULL ELSE now() END`+extra+` WHERE id=$1 AND state=$3 AND phase=$7 AND impl_round=$8`,
		append([]any{w.ID, next.State, w.State, shorten.Text(reason, maxReasonLen), Active(next.State), next.Phase, w.Phase, w.ImplRound}, args...)...)
	if err != nil {
		return false, fmt.Errorf("工作项 #%d %s/%s → %s/%s：%w", w.ID, w.Phase, w.State, next.Phase, next.State, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	prev := w.Pos()
	if newRound {
		if err := reload(ctx, tx, w); err != nil {
			return false, err
		}
	}
	w.State, w.Phase, w.Reason = next.State, next.Phase, reason
	if !Active(next.State) {
		if _, err := tx.Exec(ctx, `UPDATE dev_work_item_issues SET active=false WHERE work_item_id=$1`, w.ID); err != nil {
			return false, err
		}
		skip := map[string]string{StateNoChange: skipNoChange, StateStopped: skipStopped}[next.State]
		if skip != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO dev_issue_skips (repo_id, issue_number, digest, reason)
				SELECT repo_id, issue_number, digest, $2 FROM dev_work_item_issues WHERE work_item_id=$1
				ON CONFLICT (repo_id, issue_number) DO UPDATE SET digest=EXCLUDED.digest, reason=EXCLUDED.reason, reconsider='', waiting_on='{}',
				waiting_checked=true, created_at=now()`,
				w.ID, skip+"（工作项 #"+strconv.FormatInt(w.ID, 10)+"）"); err != nil {
				return false, err
			}
		}
	}
	if next.State == StateBlocked {
		if err := s.blockedComment(ctx, tx, w); err != nil {
			return false, err
		}
	}
	if err := store.Audit(ctx, tx, auditActor, "aidev.transition", map[string]any{
		"work_item": w.ID, "repo_id": w.RepoID, "from": prev.State, "to": next.State, "phase": prev.Phase, "to_phase": next.Phase,
		"event": string(ev), "reason": shorten.Text(reason, maxReasonLen),
	}); err != nil {
		return false, err
	}
	return true, s.Enqueue(ctx, tx, w.RepoID)
}

// reload 重新读取工作项 w（同一事务中刚刚更新过）。
func reload(ctx context.Context, tx pgx.Tx, w *WorkItem) error {
	cur, err := GetWorkItem(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	*w = *cur
	return nil
}

// branchMatch 是按正式分支关联工作项的条件（PLAN #31.3：分支的编号、阶段和轮次都要与工作项当前的相同）；参数依次是
// 编号、阶段、轮次。
func branchMatch(id, phase, round string) string {
	return "(id=" + id + " AND phase=" + phase + " AND impl_round=" + round + ")"
}

// HoldPending 判断仓库 repoID 的 PR pr（head 分支 headRef）是否是工作项的 PR、而工作项决定要加 kiln:hold 但还没有确认加上
// （PLAN #26.12）。按 PR 编号或本轮的正式分支关联：Server 建 PR 之后才记下编号。合并队列的自动入队据此跳过
// （组装层注入 mergequeue.Deps.HoldPending）。
func HoldPending(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (bool, error) {
	b, _ := devreport.ParseWorkItemBranch(headRef)
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dev_work_items WHERE repo_id=$1 AND hold_pending AND (pr_number=$2 OR `+
		branchMatch("$3", "$4", "$5")+`))`, repoID, pr, b.ID, b.Phase, b.Round).Scan(&held)
	return held, err
}

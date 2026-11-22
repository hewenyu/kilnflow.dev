package aidev

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 合并冲突的返修（PLAN #26.11、#28.3、C23）：合并队列更新 PR 分支时平台确认冲突，PR 由 Server 记录的工作项负责时，合并队列
// 在出队的同一个事务中把冲突交回工作项（HandBack，组装层注入 mergequeue.Deps.DevConflicts）；仓库的执行者随后开始一轮冲突
// 返修（startConflictFix）：开发机把最新的目标分支合并进正式分支、解决冲突，Server 只快进正式分支，新 head 重新经过 CI 和
// 独立审计，通过后按既有的合并策略重新入队。第三方 PR（没有工作项负责）由提交者处理，不建开发任务。
//
// 归属只按 Server 自己写下的记录：工作项的 pr_number 与 PR 编号相同，并且 PR 的 head 分支是工作项本阶段、本轮的正式
// 分支（I18）。作者是不是机器人、分支名像不像正式分支、PR 描述中的标记都不单独作为依据。

// OwnerKind 是 PR 的归属。
type OwnerKind int

const (
	// OwnerNone：没有工作项负责（第三方 PR，或者分支名像正式分支、编号却对不上）。
	OwnerNone OwnerKind = iota
	// OwnerActive：未结束的工作项负责，冲突交回它返修。
	OwnerActive
	// OwnerEnded：负责它的工作项已经结束（终止等），不自动返修、不复活工作项，需要人处理。
	OwnerEnded
	// OwnerRecording：工作项已经建了 PR、还没记下编号（推进中）：等它记下之后再判断。
	OwnerRecording
)

// PullOwner 是 PR 的归属：Kind 不是 OwnerNone 时 WorkItem 是负责的工作项，State 是它的状态；OwnerActive 时 BaseRef 是
// 工作项本阶段的目标分支（冲突返修只把它合并进来，PR 改投了别的目标分支时合并队列不交回）。
type PullOwner struct {
	Kind     OwnerKind
	WorkItem int64
	State    string
	BaseRef  string
}

// Owner 返回仓库 repoID 的 PR pr（head 分支 headRef）由哪个工作项负责（只读）。
func Owner(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (PullOwner, error) {
	b, ok := devreport.ParseWorkItemBranch(headRef)
	if !ok {
		return PullOwner{}, nil
	}
	var o PullOwner
	var active bool
	var baseRef string
	err := q.QueryRow(ctx, `SELECT id, state, active, base_ref FROM dev_work_items WHERE repo_id=$1 AND pr_number=$2 AND `+
		branchMatch("$3", "$4", "$5"), repoID, pr, b.ID, b.Phase, b.Round).Scan(&o.WorkItem, &o.State, &active, &baseRef)
	switch {
	case err == nil && active:
		o.Kind, o.BaseRef = OwnerActive, baseRef
		return o, nil
	case err == nil:
		o.Kind = OwnerEnded
		return o, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return PullOwner{}, err
	}
	err = q.QueryRow(ctx, `SELECT id, state FROM dev_work_items WHERE repo_id=$1 AND active AND state=$2 AND pr_number IS NULL AND `+
		branchMatch("$3", "$4", "$5"), repoID, StateDelivering, b.ID, b.Phase, b.Round).Scan(&o.WorkItem, &o.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return PullOwner{}, nil
	}
	if err != nil {
		return PullOwner{}, err
	}
	o.Kind = OwnerRecording
	return o, nil
}

// Conflict 是合并队列交回的一个冲突：仓库的 PR（head 分支 HeadRef）在 Head 上与目标分支 BaseRef 的 Base 冲突。
type Conflict struct {
	RepoID  int64
	PR      int
	HeadRef string
	Head    string
	BaseRef string
	Base    string
}

// conflictKey 返回一个冲突的去重键：同一个 head 面对不同的 base 是新的冲突。
func conflictKey(head, base string) string { return head + ":" + base }

// HandBack 在调用方（合并队列）的事务 tx 中把冲突 c 交给负责 PR 的工作项：记下冲突的 head 和 base，通知仓库的执行者
// （它在自己的处理中开始返修，交回本身不推进状态机）。返回 false 表示工作项期间已经结束或换了 PR，或者冲突的目标分支不是
// 工作项的目标分支（PR 改投了别的分支：返修的输入只记 base commit，分支名按工作项的目标分支，两者必须是同一个分支），
// 调用方改为等待人处理。只写工作项和 outbox（加锁顺序：合并队列的仓库锁 → merge_queue → dev_work_items，aidev 的路径不锁 merge_queue，
// 不会成环）。
//
// 同一个冲突（head:base）已经开过返修时什么也不做（重复的事件、合并队列重试）。只记冲突，不改工作项记下的 head：冲突的
// head 可能比它新（PR 的 synchronize 事件还没处理），也可能比它旧（合并队列重试时交回的是队列项记下的 head，期间人已经
// 推送、新 head 已经记下），交回时分不出来；由执行者按正式分支现在的 head 判断（handleConflict）。
func (s *Service) HandBack(ctx context.Context, tx pgx.Tx, c Conflict) (bool, error) {
	b, ok := devreport.ParseWorkItemBranch(c.HeadRef)
	if !ok {
		return false, nil
	}
	w, err := queryWorkItem(ctx, tx, `repo_id=$1 AND active AND pr_number=$2 AND `+branchMatch("$3", "$4", "$5")+forUpdate,
		c.RepoID, c.PR, b.ID, b.Phase, b.Round)
	if err != nil || w == nil || w.BaseRef != c.BaseRef {
		return false, err
	}
	key := conflictKey(c.Head, c.Base)
	if w.FixedConflict == key || (w.ConflictHead == c.Head && w.ConflictBase == c.Base) {
		return true, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE dev_work_items SET conflict_head=$2, conflict_base=$3, updated_at=now() WHERE id=$1`,
		w.ID, c.Head, c.Base); err != nil {
		return false, err
	}
	if err := store.Audit(ctx, tx, auditActor, "aidev.conflict", map[string]any{
		"work_item": w.ID, "repo_id": c.RepoID, "pr": c.PR, "head": c.Head, "base": c.Base, "state": w.State,
	}); err != nil {
		return false, err
	}
	return true, s.Enqueue(ctx, tx, c.RepoID)
}

// conflictNote 是受阻的工作项有待处理的冲突时追加在原因后面的说明。
const conflictNote = "PR 与目标分支冲突（retry 时一起解决）"

// handleConflict 处理工作项 w 待处理的冲突（执行者，持有仓库锁，在判断审计返修之前）：
//
//   - pr_open、blocked：开始返修、以冲突为由受阻或追加说明之前，先按正式分支现在的 head 判断（currentConflict）：就是冲突的
//     head 时有效（synchronize 事件还没处理时记下它、按它处理），否则冲突过期（期间推进了新的交付、人推送过而 synchronize
//     事件可能还没处理，或者合并队列重试时交回的是旧 head），清空；
//   - pr_open：这个冲突已经返修过时不再返修，清空（新 head 入队时仍冲突会被重新发现）；本轮的冲突返修用完时受阻；否则开始
//     一轮冲突返修（EvConflict）。同一条 UPDATE 中记下 fixed_conflict（同一个冲突只返修一次，I16）、fixed_head（这个
//     head 不再按审计判断返修）、fix_rounds 和 round_conflicts 加一；
//   - fixing、delivering（返修进行中）：保留冲突，不另开一轮（一个工作项同一时刻只有一轮返修）；还没规划的返修会带上它，
//     新的交付推进后 head 变了，冲突过期；
//   - blocked：保留冲突，原因追加说明；人 retry 时这一轮返修带上它（不绕过人的裁决）。
func (p *pass) handleConflict(ctx context.Context, w *WorkItem) error {
	if w.ConflictHead == "" || (w.State != StatePROpen && w.State != StateBlocked) {
		return nil
	}
	if w.State == StateBlocked && strings.Contains(w.Reason, conflictNote) {
		// 已经说明过，不再读取分支；head 变了时 synchronize 事件记下新 head，返修的规划不再带上这个冲突（pendingConflict）
		return nil
	}
	if current, err := p.currentConflict(ctx, w); err != nil || !current {
		return err
	}
	switch w.State {
	case StatePROpen:
	case StateBlocked:
		reason := shorten.Text(strings.TrimSuffix(w.Reason, "。")+"；"+conflictNote, maxReasonLen)
		_, err := p.conn.Exec(ctx, `UPDATE dev_work_items SET reason=$3, updated_at=now() WHERE id=$1 AND state=$2`, w.ID, StateBlocked, reason)
		if err == nil {
			w.Reason = reason
		}
		return err
	}
	key := conflictKey(w.ConflictHead, w.ConflictBase)
	if key == w.FixedConflict {
		// 这个冲突已经返修过（这一轮已经交付、推进）：清空
		return p.dropConflict(ctx, w)
	}
	what := fmt.Sprintf("PR head %s 与目标分支 %s（%s）冲突", shorten.Hash(w.ConflictHead), w.BaseRef, shorten.Hash(w.ConflictBase))
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		if w.RoundConflicts >= w.MaxFixRounds {
			_, err := p.s.transition(ctx, tx, w, EvRoundsExhausted, fmt.Sprintf("%s冲突返修 %d 轮后仍冲突（%s），需要人处理；%s",
				phaseName(w), w.RoundConflicts, what, conflictNote), `fixed_conflict=$9`, key)
			return err
		}
		moved, err := p.s.transition(ctx, tx, w, EvConflict, fmt.Sprintf("%s第 %d 轮冲突返修：%s，等待开发机领取",
			phaseName(w), w.RoundConflicts+1, what),
			`fixed_conflict=$9, fixed_head=$10, fix_rounds=fix_rounds+1, round_conflicts=round_conflicts+1, task_id=NULL`, key, w.ConflictHead)
		if err != nil || !moved {
			return err
		}
		w.FixedConflict, w.FixedHead = key, w.ConflictHead
		return nil
	})
}

// currentConflict 核对冲突的 head 是不是 PR 现在的 head，返回冲突是否仍然有效（过期的已经清空）。总是读取正式分支（PR 的
// head 分支，I18）现在指向的 commit，不只比较记下的 head：记下的 head 与冲突的 head 相同时，人也可能已经推送了解决冲突的
// 新 head、synchronize 事件还没处理，按记录判断会把已经解决的冲突当成有效，消耗返修预算或者受阻。分支上不是冲突的 head
// 时过期，清空；是冲突的 head 而记下的不是时 synchronize 事件还没处理，记下这个 head（只在记下的 head 期间没有变化时
// 写入）。读取失败时返回错误（稍后重试），不按冲突处理、也不清空。
func (p *pass) currentConflict(ctx context.Context, w *WorkItem) (bool, error) {
	head, err := p.f.BranchHead(ctx, w.Branch())
	if err != nil {
		return false, fmt.Errorf("读取正式分支 %s：%w", w.Branch(), err)
	}
	if head != w.ConflictHead {
		return false, p.dropConflict(ctx, w)
	}
	if w.ConflictHead == w.HeadSHA {
		return true, nil
	}
	tag, err := p.conn.Exec(ctx, `UPDATE dev_work_items SET head_sha=$3, updated_at=now() WHERE id=$1 AND head_sha=$2 AND conflict_head=$3`,
		w.ID, w.HeadSHA, w.ConflictHead)
	if err != nil || tag.RowsAffected() == 0 {
		// 期间记下了别的 head（或者冲突变了）：下一次处理按最新的记录判断
		return false, err
	}
	w.HeadSHA = w.ConflictHead
	return true, nil
}

// dropConflict 清空工作项 w 记下的冲突（只在冲突期间没有变化时）。
func (p *pass) dropConflict(ctx context.Context, w *WorkItem) error {
	_, err := p.conn.Exec(ctx, `UPDATE dev_work_items SET `+clearConflict+` WHERE id=$1 AND conflict_head=$2 AND conflict_base=$3`,
		w.ID, w.ConflictHead, w.ConflictBase)
	if err == nil {
		w.ConflictHead, w.ConflictBase = "", ""
	}
	return err
}

// pendingConflict 返回返修要一起解决的冲突（没有时为 nil）：冲突就在返修的起点所在的 PR head 上。
func pendingConflict(w *WorkItem) *conflictInput {
	if w.ConflictHead == "" || w.ConflictHead != w.HeadSHA {
		return nil
	}
	return &conflictInput{head: w.ConflictHead, base: w.ConflictBase}
}

// conflictInput 是一轮返修要解决的冲突。
type conflictInput struct {
	head, base string
}

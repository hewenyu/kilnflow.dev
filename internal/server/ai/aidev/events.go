package aidev

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 事件（PLAN #26.11、PLAN #31）：issues 事件更新任务池（选题的候选从任务池中取）；去掉 ready、加上 skip、Issue 全部关闭、PR
// 关闭时终止工作项，PR 合并时按阶段推进；Issue 关闭时唤醒等待它的跳过记录；Issue 的内容变了记 requirements_changed；
// PR 的 head 变了（人推送）以新 head 为准。

// Issue 事件的 action（与 GitHub 相同）。
const (
	IssueLabeled   = "labeled"
	IssueUnlabeled = "unlabeled"
	IssueEdited    = "edited"
	IssueClosed    = "closed"
	IssueReopened  = "reopened"
)

// IssueEvent 是一个 issues webhook 事件。
type IssueEvent struct {
	RepoID int64
	Action string
	Number int
	// Title、Body、Open、Labels、UpdatedAt 是事件中 Issue 的当前内容（任务池按它更新）。
	Title, Body string
	Open        bool
	Labels      []string
	UpdatedAt   time.Time
	URL         string
	// Label 是 labeled、unlabeled 变化的 label。
	Label  string
	Sender string
	// FromSelf 表示事件由本 App 触发（执行者同步状态 label）。
	FromSelf bool
}

// HandleIssueEvent 处理 issues 事件，返回处理结果的说明（记入 events.result）。比任务池中已记录的内容更旧的事件（webhook
// 乱序到达）整个忽略：任务池和工作项用同一个新旧判断，旧的"去掉 ready"不能终止按较新的授权领取的工作项。
//
// 工作项按事件中 Issue 的当前内容处理（reconcileIssue），不按事件的 action：较新的事件（包括本 App 同步状态 label
// 产生的）已经反映了人之前的操作，那个操作的事件晚到时被当作旧事件忽略，不能因此漏掉。
func (s *Service) HandleIssueEvent(ctx context.Context, ev IssueEvent) (string, error) {
	pooled, stale, err := s.poolEvent(ctx, ev)
	if err != nil {
		return "", err
	}
	if stale {
		return fmt.Sprintf("AI 开发：Issue #%d 的事件比已记录的内容旧（乱序到达），忽略", ev.Number), nil
	}
	note := ""
	if pooled != "" {
		note = "AI 开发：" + pooled
	}
	if ev.FromSelf {
		note = joinNote(note, "本 App 的操作")
	}
	if !ev.Open {
		// 等待这个 Issue 的跳过记录（PLAN #31.6）：任何 Issue 关闭都要处理，不只是任务池中的
		n, err := s.releaseWaiters(ctx, ev.RepoID, ev.Number)
		if err != nil {
			return "", err
		}
		if n > 0 {
			note = joinNote(note, fmt.Sprintf("AI 开发：等待 #%d 的 %d 个 Issue 回到候选", ev.Number, n))
		}
	}
	res, err := s.reconcileIssue(ctx, ev)
	return joinNote(note, res), err
}

// reconcileIssue 按事件中 Issue 的当前内容处理包含它的未结束的工作项：有 kiln:skip 或没有 kiln:ready 时终止（人撤回了
// 授权；Kiln 自己从不去掉 ready）；记下 Issue 关闭或重新打开（没有 PR 的工作项的 Issue 全部关闭时终止）；内容变了记
// requirements_changed。每一步都按当前内容判断，重复执行没有影响。
func (s *Service) reconcileIssue(ctx context.Context, ev IssueEvent) (string, error) {
	switch {
	case slices.Contains(ev.Labels, LabelSkip):
		return s.stopForIssue(ctx, ev, fmt.Sprintf("Issue #%d 有 %s", ev.Number, LabelSkip))
	case !slices.Contains(ev.Labels, LabelReady):
		return s.stopForIssue(ctx, ev, fmt.Sprintf("Issue #%d 没有 %s 了（授权已撤回）", ev.Number, LabelReady))
	}
	if !ev.Open {
		if res, err := s.issueClosed(ctx, ev); err != nil || res != "" {
			return res, err
		}
	} else if _, err := s.db.Exec(ctx, `UPDATE dev_work_item_issues SET closed=false WHERE repo_id=$1 AND issue_number=$2 AND active AND closed`,
		ev.RepoID, ev.Number); err != nil {
		return "", err
	}
	return s.issueEdited(ctx, ev)
}

// joinNote 用"；"连接非空的说明。
func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "；" + b
}

// poolEvent 按事件中 Issue 的当前内容更新任务池；人加上 kiln:ready 时清除跳过记录（人重新授权，内容不变也重新进入
// 候选）。任务池有变化时通知执行者（有空闲的开发机就开始选题），返回说明（没有变化时为空）；事件比已记录的内容旧时
// stale 为真，什么也不改。
func (s *Service) poolEvent(ctx context.Context, ev IssueEvent) (note string, stale bool, err error) {
	is := &forge.IssueContent{Number: ev.Number, Title: ev.Title, Body: ev.Body, Open: ev.Open, Labels: ev.Labels, URL: ev.URL, UpdatedAt: ev.UpdatedAt}
	in := is.Open && is.HasLabel(LabelReady)
	var changed bool
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		// 先用任务池的独立锁串行化更新 dev_pool 的入口，再按 issue_number 升序锁行；ai_repo_state 放在全部 dev_pool
		// 更新之后统一写，保持 dev_pool -> ai_repo_state 的锁序（与 claimTx / reoffer 一致）。
		if err := store.LockXact(ctx, tx, poolLockKey(ev.RepoID)); err != nil {
			return err
		}
		moved, old, err := mirror(ctx, tx, ev.RepoID, is)
		if err != nil || old {
			stale = old
			return err
		}
		changed = moved
		if !ev.FromSelf && ev.Action == IssueLabeled && ev.Label == LabelReady {
			tag, err := tx.Exec(ctx, `DELETE FROM dev_issue_skips WHERE repo_id=$1 AND issue_number=$2`, ev.RepoID, ev.Number)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		// 全部 dev_pool 更新完成之后才写 ai_repo_state。
		if err := poolChanged(ctx, tx, ev.RepoID); err != nil {
			return err
		}
		return s.Enqueue(ctx, tx, ev.RepoID)
	})
	switch {
	case err != nil || !changed:
		return "", stale, err
	case in:
		return fmt.Sprintf("Issue #%d 在任务池中（已更新）", ev.Number), false, nil
	}
	return fmt.Sprintf("Issue #%d 离开任务池", ev.Number), false, nil
}

// activeWorkItemOf 返回包含 Issue 的未结束的工作项（没有时为 nil）。
func activeWorkItemOf(ctx context.Context, q pgx.Tx, repoID int64, issue int) (*WorkItem, error) {
	return queryWorkItem(ctx, q, `id=(SELECT work_item_id FROM dev_work_item_issues WHERE repo_id=$1 AND issue_number=$2 AND active)`, repoID, issue)
}

func (s *Service) workItemOf(ctx context.Context, repoID int64, issue int) (*WorkItem, error) {
	var w *WorkItem
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		w, err = activeWorkItemOf(ctx, tx, repoID, issue)
		return err
	})
	return w, err
}

// stopForIssue 终止包含 Issue 的未结束的工作项（人撤回了授权）。
func (s *Service) stopForIssue(ctx context.Context, ev IssueEvent, reason string) (string, error) {
	w, err := s.workItemOf(ctx, ev.RepoID, ev.Number)
	if err != nil || w == nil {
		return "", err
	}
	if err := s.Stop(ctx, w.ID, reason, ev.Sender); err != nil && !errors.Is(err, ErrFinished) {
		return "", err
	}
	return fmt.Sprintf("AI 开发：终止工作项 #%d（%s）", w.ID, reason), nil
}

// reasonIssuesClosed 是工作项的 Issue 全部关闭（还没有 PR）时终止的原因。
const reasonIssuesClosed = "工作项的 Issue 全部关闭"

// issueClosed 记下 Issue 已关闭；还没有 PR 的工作项的 Issue 全部关闭时终止它。有 PR 的工作项由 PR 的合并或关闭决定：
// PR 合并会按 Fixes 关闭 Issue，issues.closed 可能比 pull_request.closed 先到，不能据此终止。delivering 的工作项可能
// 已经有没记下编号的 PR（建 PR 的响应丢失），由执行者看过正式分支上的 PR 之后判断（settleUnrecorded）。
func (s *Service) issueClosed(ctx context.Context, ev IssueEvent) (string, error) {
	var w *WorkItem
	var open int
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if w, err = activeWorkItemOf(ctx, tx, ev.RepoID, ev.Number); err != nil || w == nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dev_work_item_issues SET closed=true WHERE work_item_id=$1 AND issue_number=$2`, w.ID, ev.Number); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM dev_work_item_issues WHERE work_item_id=$1 AND NOT closed`, w.ID).Scan(&open)
	})
	if err != nil || w == nil || open > 0 || w.PRNumber != nil {
		return "", err
	}
	if w.State == StateDelivering {
		return "", s.Enqueue(ctx, s.db, ev.RepoID)
	}
	if err := s.Stop(ctx, w.ID, reasonIssuesClosed, ev.Sender); err != nil && !errors.Is(err, ErrFinished) {
		return "", err
	}
	return fmt.Sprintf("AI 开发：终止工作项 #%d（%s）", w.ID, reasonIssuesClosed), nil
}

// issueEdited 在未结束的工作项的 Issue 内容变了时记 requirements_changed（下一轮执行的 prompt 写明，不自动取消）。
func (s *Service) issueEdited(ctx context.Context, ev IssueEvent) (string, error) {
	tag, err := s.db.Exec(ctx, `UPDATE dev_work_items SET requirements_changed=true, updated_at=now() WHERE id=(
		SELECT work_item_id FROM dev_work_item_issues WHERE repo_id=$1 AND issue_number=$2 AND active AND digest<>$3) AND NOT requirements_changed`,
		ev.RepoID, ev.Number, IssueDigest(ev.Title, ev.Body))
	if err != nil || tag.RowsAffected() == 0 {
		return "", err
	}
	s.notify()
	return fmt.Sprintf("AI 开发：Issue #%d 的内容在领取之后变了", ev.Number), nil
}

// ClosedPull 是一个被关闭的 PR。
type ClosedPull struct {
	Number int
	// HeadRef 是 PR 的来源分支（fork 来的 PR 为空）；HeadSHA 是合并时 PR 的 head（拿不到时为空）。
	HeadRef string
	HeadSHA string
	Merged  bool
	// MergeSHA 是合并得到的 commit（合并时；拿不到时为空）。
	MergeSHA string
}

// HandlePullClosed 处理工作项的 PR 被关闭（PLAN #26.9、#31.3）：合并时按阶段推进（设计 → 开始实现；实现 → 全部完整
// 解决时 merged，否则受阻、下一轮用新的分支），未合并 → stopped。不是工作项本阶段、本轮的 PR 时什么也不做：设计 PR 或
// 上一轮实现 PR 的合并事件重复到达、晚到时，工作项已经换了阶段或轮次、pr_number 已经清空，按分支也只匹配本轮。
// Server 建 PR 之后才记下编号，记下之前（建 PR 的响应丢失、加 label 还没返回）人关闭了 PR 时按本轮的正式分支找到工作项。
// 在仓库锁下进行，等正在进行的推进做完。
func (s *Service) HandlePullClosed(ctx context.Context, repoID int64, cp ClosedPull, sender string) (string, error) {
	b, _ := devreport.ParseWorkItemBranch(cp.HeadRef)
	var id int64
	err := s.db.QueryRow(ctx, `SELECT id FROM dev_work_items WHERE repo_id=$1 AND active AND (pr_number=$2 OR (pr_number IS NULL AND `+
		branchMatch("$3", "$4", "$5")+`))`, repoID, cp.Number, b.ID, b.Phase, b.Round).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	w0, err := GetWorkItem(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	var reason string
	var cancelReq int64
	err = s.withRepoLock(ctx, repoID, lockWait, func(conn *pgxpool.Conn) error {
		return s.tx(ctx, conn, func(tx pgx.Tx) error {
			var err error
			reason, cancelReq, err = s.pullClosedTx(ctx, tx, w0.ID, cp, b, sender, true)
			return err
		})
	})
	if err != nil {
		if errors.Is(err, ErrFinished) {
			return "", nil
		}
		return "", err
	}
	// 等待工作项锁期间关联上来的新任务不在已经取消的请求里：事务提交后再取消（见 pullClosedTx 的加锁顺序说明）。
	s.cancelAfterCommit(ctx, id, cancelReq)
	s.notify()
	if reason == "" {
		return "", nil
	}
	return fmt.Sprintf("AI 开发：工作项 #%d %s", id, reason), nil
}

// pullClosedOurs 判断被关闭的 PR 是否属于工作项本阶段、本轮：PR 是工作项记下的 PR，或者工作项还没记下 PR、PR 来自本轮的
// 正式分支 b。加锁前后各判断一次（锁内以锁住的为准）。
func pullClosedOurs(w *WorkItem, cp ClosedPull, b devreport.BranchRef) bool {
	if w.PRNumber != nil {
		return *w.PRNumber == cp.Number
	}
	return b.ID == w.ID && b.Phase == w.Phase && b.Round == w.ImplRound
}

// pullClosedTx 在事务 tx 中（调用方持有仓库锁）让工作项 id 收到它本阶段、本轮的 PR 被关闭：PR 是工作项记下的 PR，或者
// 工作项还没记下 PR、PR 来自本轮的正式分支 b。不是时什么也不做（返回空）。先取消执行中的任务，再按结果转移。
// 返回值 cancelReq 是需要在事务提交后取消的请求（等待工作项锁期间关联上来的新任务；0 表示没有）。
//
// recordCheck 为真时（webhook 没有平台返回的 PR 作者等核验依据）：工作项还没记下 PR（创建 PR 的响应丢失）、PR 已合并时，
// 在推进工作项（可能换阶段、换轮次、清空交付引用）之前冻结一条待核验记录，由执行者经平台核验后补记永久归属，并通知
// 执行者。绝不在这里按正式分支直接写归属。
func (s *Service) pullClosedTx(ctx context.Context, tx pgx.Tx, id int64, cp ClosedPull, b devreport.BranchRef, actor string, recordCheck bool,
) (reason string, cancelReq int64, err error) {
	w, err := GetWorkItem(ctx, tx, id)
	if err != nil {
		return "", 0, err
	}
	if !Active(w.State) {
		return "", 0, ErrFinished
	}
	// 先取消执行中的任务，再加锁工作项：结束任务的事务持有请求、任务锁并等待工作项锁，反过来做会死锁（加锁顺序：
	// 并发组 → 请求 → 任务 → 工作项）。
	var cancelledTaskID int64
	if pullClosedOurs(w, cp, b) && w.TaskID != nil {
		t, err := ai.GetTask(ctx, tx, *w.TaskID, false)
		if err != nil {
			return "", 0, err
		}
		if err := s.core.CancelTx(ctx, tx, t.RequestID, ai.CancelWorkItemDone, ""); err != nil {
			return "", 0, err
		}
		cancelledTaskID = *w.TaskID
	}
	// 锁住之后复查：等待工作项锁期间工作项可能已经换了阶段、轮次或者 PR（例如设计 PR 已经合并、开始了新一轮），这时
	// 被关闭的 PR 不再属于它。
	w, err = queryWorkItem(ctx, tx, `id=$1`+forUpdate, id)
	if err != nil {
		return "", 0, err
	}
	if w == nil || !Active(w.State) {
		return "", 0, ErrFinished
	}
	if !pullClosedOurs(w, cp, b) {
		return "", 0, nil
	}
	// 期间关联上来的新任务（结束任务的事务把另一轮的任务记到工作项上）不在刚才取消的请求里，提交后取消。
	if w.TaskID != nil && *w.TaskID != cancelledTaskID {
		t, err := ai.GetTask(ctx, tx, *w.TaskID, false)
		if err != nil {
			return "", 0, err
		}
		cancelReq = t.RequestID
	}
	// 按分支找到的（编号还没记下）：在锁内记下 PR 编号（Dashboard 上能看到是哪个 PR；设计合并时记为 design_pr）。
	unrecorded := w.PRNumber == nil
	if unrecorded {
		if _, err := tx.Exec(ctx, `UPDATE dev_work_items SET pr_number=$2 WHERE id=$1`, id, cp.Number); err != nil {
			return "", 0, err
		}
		w.PRNumber = &cp.Number
	}
	// 编号还没记下、PR 已经合并：趁推进工作项换阶段、换轮次、清空交付引用之前冻结待核验的归属。
	if unrecorded && recordCheck && cp.Merged && w.State == StateDelivering {
		if err := recordPullOriginCheck(ctx, tx, w, cp.Number); err != nil {
			return "", 0, err
		}
		if err := s.Enqueue(ctx, tx, w.RepoID); err != nil {
			return "", 0, err
		}
	}
	ev, why, set, args := EvStop, fmt.Sprintf("PR #%d 被关闭（没有合并）", cp.Number), "", []any(nil)
	if cp.Merged {
		rep, err := recordMerged(ctx, tx, w, cp.HeadSHA)
		if err != nil {
			return "", 0, err
		}
		if ev, why, err = mergeEvent(ctx, tx, w, cp.Number, rep); err != nil {
			return "", 0, err
		}
		if w.Phase == PhaseDesign {
			set, args = `design_sha=$9`, []any{cp.MergeSHA}
		}
	}
	moved, err := s.transition(ctx, tx, w, ev, why, set, args...)
	if err != nil || !moved {
		return "", 0, err
	}
	if err := store.Audit(ctx, tx, actor, "aidev."+string(ev), map[string]any{
		"work_item": id, "repo_id": w.RepoID, "pr": cp.Number, "phase": w.Phase, "reason": why,
	}); err != nil {
		return "", 0, err
	}
	return why, cancelReq, nil
}

// recordMerged 记下 PR 合并的那次交付（head 为 head）并返回它的报告（没有时为 nil）：合并的 head 就是待推进的交付
// （正式分支已经推进、PR 还没记下）时是这次交付，否则是推进到正式分支上的交付；还没推进的交付（例如返修已经交付、
// 正式分支还是上一次的）不算（PLAN #31.3）。按它记下完成度、交付在正式分支上的任务和已合并的设计文档（报告没有设计
// 文档时保留之前的：之后的轮次继续按已合并的设计实现），工作项 w 随之更新。
func recordMerged(ctx context.Context, tx pgx.Tx, w *WorkItem, head string) (*devreport.Delivery, error) {
	task := w.HeadTaskID
	if w.DeliverySHA != "" && head == w.DeliverySHA {
		task = w.DeliveryTaskID
	}
	rep, err := taskDelivery(ctx, tx, w.ID, task)
	if err != nil || rep == nil {
		return nil, err
	}
	if err := saveResolutions(ctx, tx, w.ID, rep); err != nil {
		return nil, err
	}
	if docs := rep.Design.Documents; len(docs) > 0 {
		w.DesignDocs = docs
	}
	w.HeadTaskID = task
	_, err = tx.Exec(ctx, `UPDATE dev_work_items SET head_task_id=$2, design_docs=$3 WHERE id=$1`, w.ID, task, w.DesignDocs)
	return rep, err
}

// mergeEvent 返回工作项本阶段的 PR 合并时的事件和原因（PLAN #31.3）：设计 PR 合并进入实现；实现 PR 合并时每个未关闭的
// Issue 都完整解决（full）才结束，否则受阻（原因写剩余事项和外部输入），完整解决的 Issue 照常由合并关闭。
func mergeEvent(ctx context.Context, q store.Querier, w *WorkItem, pr int, rep *devreport.Delivery) (Event, string, error) {
	if w.Phase == PhaseDesign {
		return EvMerged, fmt.Sprintf("设计 PR #%d 已合并，开始实现", pr), nil
	}
	is, err := workIssues(ctx, q, w.ID)
	if err != nil {
		return "", "", err
	}
	var open []string
	for _, i := range is {
		if !i.Closed && i.Resolution != devreport.ResolutionFull {
			open = append(open, fmt.Sprintf("#%d（%s）", i.Number, cmp.Or(i.Resolution, devreport.ResolutionNone)))
		}
	}
	if len(open) == 0 {
		return EvMerged, fmt.Sprintf("PR #%d 已合并", pr), nil
	}
	parts := []string{fmt.Sprintf("PR #%d 已合并，还有没有完整解决的 Issue：%s", pr, strings.Join(open, " "))}
	if rep != nil {
		if r := strings.TrimSpace(rep.Handoff.Remaining); r != "" {
			parts = append(parts, "剩余（Agent 自报）："+r)
		}
		for _, x := range rep.Design.ExternalInputs {
			parts = append(parts, fmt.Sprintf("需要人提供：%s（阻塞：%s；恢复条件：%s）", x.What, x.Blocks, x.Resume))
		}
	}
	parts = append(parts, "条件具备后 retry 从最新的默认分支开始下一轮实现")
	return EvMergedPartial, strings.Join(parts, "；"), nil
}

// HandlePullHead 记下工作项的 PR 的新 head（人推送了新 commit，或者 Kiln 推进了分支）：之后以它为准判断返修。
//
// 只在 pr_open 记录（那是唯一按 PR head 判断返修的状态）：返修、受阻、交付进行中时判断依据已经由那次转移定下（返修任务
// 的 spec 已经带着它），乱序到达的旧 head 事件不能改掉它——否则会按旧 head 重复返修，或者让返修从错的 head 开始。
func (s *Service) HandlePullHead(ctx context.Context, repoID int64, pr int, head string) error {
	tag, err := s.db.Exec(ctx, `UPDATE dev_work_items SET head_sha=$3, updated_at=now()
		WHERE repo_id=$1 AND pr_number=$2 AND active AND state=$4 AND head_sha<>$3`, repoID, pr, head, StatePROpen)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return s.Enqueue(ctx, s.db, repoID)
}

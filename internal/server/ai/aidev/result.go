package aidev

import (
	"cmp"
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 记录一次执行的结果（PLAN #26.10 第 5–6 步、#26.11 推进）：在结束任务的事务中保存交付报告、attempt 分支，
// 按结果推进工作项。交付只是"待推进"（delivering）：执行者核对过 commit 才动正式分支（I5、I9）。

// errClassAIDelivery 是交付报告无效时 attempt 的错误类别（与 Runner 相同）。
const errClassAIDelivery = "ai.delivery"

// Delivery 是 Runner 随 Complete 提交的 AI 开发结果。
type Delivery struct {
	// Report 是交付报告（JSON），可能为空。
	Report []byte
	// Branch、SHA 是推到远端的 attempt 分支和 commit；Checkpoint 为真时是中断时保存的未完成改动。
	Branch, SHA string
	Checkpoint  bool
	// RunnerVersion、AgentVersion、Model 记入报告的适用范围（#26.14，拿不到时为空）。
	RunnerVersion, AgentVersion, Model string
	// Agent 是执行这次开发的 Agent：记入工作项开发过的 Agent（PLAN #33.7：它不能审计这个工作项的 PR）。
	Agent string
}

// RecordResult 在结束任务的事务 tx 中记录 AI 开发任务 t 的结果，返回任务最终的结果（交付报告无效时改为失败）。
// 调用方已经锁住请求和任务（加锁顺序：并发组 → 请求 → 任务 → 工作项）。
func (s *Service) RecordResult(ctx context.Context, tx pgx.Tx, t *ai.Task, attemptID int64, del *Delivery, res ai.Result) (ai.Result, error) {
	d, err := developSpec(t)
	if err != nil {
		return res, err
	}
	var w *WorkItem
	switch d.Mode {
	case aispec.DevSelect:
		// 只认工作项仍关联着的选题任务：设计 PR 合并后开始了新的轮次（task_id 已清空），被取消的设计任务晚到的结果不能
		// 推进实现阶段
		w, err = queryWorkItem(ctx, tx, `claim_attempt_id=$1 AND task_id=$2`+forUpdate, attemptID, t.ID)
	case aispec.DevImplement:
		// 与返修相同：规划实现时请求先提交、之后才关联到工作项，两步之间失败时在这里关联这一轮的任务
		w, err = queryWorkItem(ctx, tx, `id=$1 AND (task_id=$2 OR (`+implStill("$3", "$4")+`))`+forUpdate,
			d.WorkItemID, t.ID, d.ImplRound, d.Round)
		if err == nil && w != nil && w.TaskID == nil {
			if _, err = linkImpl(ctx, tx, w.ID, t.ID, d.BaseRef, d.StartSHA, d.ImplRound, d.Round, false); err == nil {
				err = reload(ctx, tx, w)
			}
		}
	default:
		// 规划返修时请求先提交、之后才在另一个事务中记到工作项上：两步之间失败（进程退出、数据库错误）时任务已经可以
		// 领取。这一轮（同一个工作项、返修中、轮次相同，去重键保证一轮只有一个请求）还没有关联任务时在这里关联，
		// 不丢掉有效的交付
		w, err = queryWorkItem(ctx, tx, `id=$1 AND (task_id=$2 OR (task_id IS NULL AND state=$3 AND fix_rounds=$4))`+forUpdate,
			d.WorkItemID, t.ID, StateFixing, d.Round)
		if err == nil && w != nil && w.TaskID == nil {
			if _, err = tx.Exec(ctx, `UPDATE dev_work_items SET task_id=$2 WHERE id=$1`, w.ID, t.ID); err == nil {
				w.TaskID = &t.ID
			}
		}
	}
	if del == nil {
		del = &Delivery{}
	}
	if err != nil {
		return res, err
	}
	if w == nil {
		// 选题没有领取（没有选中、dry_run、冲突）：选题报告已经在领取时保存。任务已经不属于工作项（被取消的旧任务，
		// 例如设计 PR 合并、开始了新的轮次）：不推进工作项，但它推到远端的分支照样登记（按任务所属的阶段和轮次，
		// 不会被本轮的 retry 使用），保留期满后删除
		return res, recordStaleBranch(ctx, tx, attemptID, d, del)
	}
	if err := recordExecution(ctx, tx, w, del); err != nil {
		return res, err
	}
	rep, res, err := deliveryReport(ctx, tx, t, d, attemptID, w, del, res)
	if err != nil {
		return res, err
	}
	// 决策疑难点同步：与交付报告在同一个事务里登记，任何失败都回滚整个结果。
	if rep != nil && s.decisionInbox != nil {
		if err := s.enqueueDecisions(ctx, tx, t, d, w, rep, res, del); err != nil {
			return res, err
		}
	}
	return res, s.advanceWorkItem(ctx, tx, w, d, del, rep, res, t.ID)
}

// advanceWorkItem 按交付报告和任务结果把工作项推进到下一个状态。
func (s *Service) advanceWorkItem(ctx context.Context, tx pgx.Tx, w *WorkItem, d *aispec.DevelopSpec,
	del *Delivery, rep *devreport.Delivery, res ai.Result, taskID int64,
) error {
	ok := res.Outcome == store.OutcomeSucceeded && rep != nil
	switch {
	case ok && rep.Status == devreport.StatusDelivered && del.SHA != "" && !del.Checkpoint:
		// 完成度在推进到正式分支时才记下（saveResolutions）：推进之前合并的是正式分支上之前的交付，合并时按它判断
		_, err := s.transition(ctx, tx, w, EvDelivered, "等待核对交付、推进分支", `delivery_branch=$9, delivery_sha=$10, delivery_task_id=$11`,
			del.Branch, del.SHA, taskID)
		return err
	case ok && rep.Status == devreport.StatusNoChange:
		why := "Agent 判断不需要改代码：" + rep.Summary
		switch {
		case d.Mode == aispec.DevFix:
			why = "返修没有新的提交（全部不采纳或无法完成），需要人裁决：" + rep.Summary
		case w.Phase == PhaseDesign:
			why = "设计阶段没有交付设计文档（Agent 报告不需要改动），需要人裁决：" + rep.Summary
		}
		_, err := s.transition(ctx, tx, w, EvNoChange, why, "")
		return err
	case ok && rep.Status == devreport.StatusBlocked:
		_, err := s.transition(ctx, tx, w, EvBlocked, "Agent 报告受阻："+rep.Handoff.Remaining+s.checkpointNote(del), "")
		return err
	default:
		why := res.Message
		if why == "" {
			why = string(res.Outcome)
		}
		_, err := s.transition(ctx, tx, w, EvExecFailed, "执行没有交付："+why+s.checkpointNote(del), "")
		return err
	}
}

// enqueueDecisions 在结束任务的事务里登记交付报告要同步到「待拍板」清单的条目。只在仓库配置了
// 「待决策清单」时生效；登记失败会让整个 RecordResult 回滚。
func (s *Service) enqueueDecisions(ctx context.Context, tx pgx.Tx, t *ai.Task, d *aispec.DevelopSpec, w *WorkItem,
	rep *devreport.Delivery, res ai.Result, del *Delivery,
) error {
	repo, err := store.GetRepo(ctx, tx, w.RepoID)
	if err != nil {
		return err
	}
	issues, err := workIssues(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	_, err = s.decisionInbox.EnqueueReportTx(ctx, tx, decisioninbox.ReportInput{
		RepoID:       w.RepoID,
		RepoFullName: repo.FullName(),
		WorkItemID:   w.ID,
		TaskID:       t.ID,
		Phase:        w.Phase,
		ImplRound:    max(w.ImplRound, 1),
		Issues:       issueNumbers(issues),
		PRNumber:     w.PRNumber,
		DesignPR:     w.DesignPR,
		Fix:          d.Mode == aispec.DevFix,
		ReportStatus: rep.Status,
		TaskOutcome:  string(res.Outcome),
		Raw:          del.Report,
		Report:       rep,
	})
	return err
}

// recordExecution 记下执行的 Agent（PLAN #33.7）和推到远端的 attempt 分支（交付或 checkpoint，过期删除）。
func recordExecution(ctx context.Context, tx pgx.Tx, w *WorkItem, del *Delivery) error {
	if err := addDevAgent(ctx, tx, w.ID, del.Agent); err != nil {
		return err
	}
	return recordBranch(ctx, tx, w.RepoID, w.ID, w.Phase, w.ImplRound, del)
}

// recordStaleBranch 登记已经不属于工作项的任务推到远端的分支：阶段和轮次取自任务（选题任务取领取时的起始阶段）。
// 选题没有领取时没有工作项，什么也不做。
func recordStaleBranch(ctx context.Context, tx pgx.Tx, attemptID int64, d *aispec.DevelopSpec, del *Delivery) error {
	if del.Branch == "" || del.SHA == "" {
		return nil
	}
	if d.Mode != aispec.DevSelect {
		w, err := queryWorkItem(ctx, tx, `id=$1`, d.WorkItemID)
		if err != nil || w == nil {
			return err
		}
		return recordBranch(ctx, tx, w.RepoID, w.ID, cmp.Or(d.Phase, PhaseImplement), max(d.ImplRound, 1), del)
	}
	w, err := queryWorkItem(ctx, tx, `claim_attempt_id=$1`, attemptID)
	if err != nil || w == nil {
		return err
	}
	phase := PhaseImplement
	if w.DesignFirst() {
		phase = PhaseDesign
	}
	return recordBranch(ctx, tx, w.RepoID, w.ID, phase, 1, del)
}

// recordBranch 登记推到远端的 attempt 分支（交付或 checkpoint）和它所属的阶段、轮次：retry 只用本阶段、本轮的
// checkpoint（PLAN #31.3），保留期满后删除。
func recordBranch(ctx context.Context, tx pgx.Tx, repoID, workItem int64, phase string, round int, del *Delivery) error {
	if del.Branch == "" || del.SHA == "" {
		return nil
	}
	kind := "delivery"
	if del.Checkpoint {
		kind = "checkpoint"
	}
	_, err := tx.Exec(ctx, `INSERT INTO dev_branches (repo_id, branch, sha, work_item_id, kind, phase, impl_round) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (repo_id, branch) DO UPDATE SET sha=EXCLUDED.sha, kind=EXCLUDED.kind, phase=EXCLUDED.phase, impl_round=EXCLUDED.impl_round,
		created_at=now(), deleted_at=NULL`,
		repoID, del.Branch, del.SHA, workItem, kind, phase, round)
	return err
}

// deliveryReport 解析并保存交付报告（没有报告时为 nil）。报告无效时不保存，成功的执行改为失败（ai.delivery）。
func deliveryReport(ctx context.Context, tx pgx.Tx, t *ai.Task, d *aispec.DevelopSpec, attemptID int64, w *WorkItem, del *Delivery,
	res ai.Result,
) (*devreport.Delivery, ai.Result, error) {
	if len(del.Report) == 0 {
		return nil, res, nil
	}
	exp, err := deliveryExpect(ctx, tx, d, w)
	if err != nil {
		return nil, res, err
	}
	rep, perr := devreport.ParseDelivery(del.Report, exp)
	if perr != nil {
		if res.Outcome == store.OutcomeSucceeded {
			res = res.Failed(errClassAIDelivery, "交付报告无效："+perr.Error())
		}
		return nil, res, nil
	}
	if err := saveReport(ctx, tx, t.ID, attemptID, reportDelivery, &w.ID, del.Report, del.RunnerVersion); err != nil {
		return nil, res, err
	}
	_, err = tx.Exec(ctx, `UPDATE dev_reports SET agent_version=$3, model=$4 WHERE task_id=$1 AND kind=$2`, t.ID, reportDelivery, del.AgentVersion, del.Model)
	return rep, res, err
}

// checkpointNote 返回 checkpoint 的说明（没有时为空）。
func (s *Service) checkpointNote(del *Delivery) string {
	if !del.Checkpoint || del.Branch == "" {
		return ""
	}
	return fmt.Sprintf("。未完成的改动保存在分支 %s（%s），%g 小时后删除", del.Branch, shorten.Hash(del.SHA), s.branchTTL.Hours())
}

// saveResolutions 记下交付报告给出的每个 Issue 的完成度：交付推进到正式分支（成为 PR 的 head）时调用，合并时按它
// 判断是否完整解决（PLAN #31.3）。
func saveResolutions(ctx context.Context, tx pgx.Tx, workItem int64, rep *devreport.Delivery) error {
	for _, r := range rep.Issues {
		if _, err := tx.Exec(ctx, `UPDATE dev_work_item_issues SET resolution=$3 WHERE work_item_id=$1 AND issue_number=$2`,
			workItem, r.Number, r.Resolution); err != nil {
			return err
		}
	}
	return nil
}

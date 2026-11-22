package aidev

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 实现任务（PLAN #31.3）：新的轮次开始后（设计 PR 合并、部分完成的实现 PR 合并后 retry），工作项是 implement /
// developing、本轮还没有执行过任务。执行者在释放仓库锁之后规划一个实现任务：起点是规划时默认分支的 head（包含已合并
// 的设计和之前轮次的改动），分支是本轮的正式分支。写法与返修相同：先提交请求，再关联到工作项；两步之间失败时，
// 结束任务时关联（RecordResult）。

// implKey 返回工作项第 implRound 轮实现的去重键：带 fix_rounds（单调递增，retry 加一），每次规划之前要么进入了新的阶段
// 或轮次，要么 retry 把 fix_rounds 加了一，不会命中已经结束的旧任务。
func implKey(repoID, workItem int64, implRound, fixRounds int) string {
	return fmt.Sprintf("%s:%d:impl:%d:%d:%d", dedupePrefix, repoID, workItem, implRound, fixRounds)
}

// implPending 是还没有实现任务的工作项的条件（参数 $1 是仓库）。
const implPending = `repo_id=$1 AND state='` + StateDeveloping + `' AND phase='` + PhaseImplement + `' AND task_id IS NULL AND NOT round_ran`

// planImpls 为还没有任务的实现规划请求（不持有仓库锁）。只为仓库的占用者规划（I14）：开始新的阶段或轮次（设计合并后的
// 实现、部分完成后 retry 开始的下一轮）只有这一处，其余的保持 developing、没有任务，原因写明在等谁；占用者结束时
// transition 通知执行者，下一次处理时下一个工作项成为占用者、在这里被规划。
func (s *Service) planImpls(ctx context.Context, repo *store.Repo) error {
	list, err := queryWorkItems(ctx, s.db, implPending, repo.ID)
	if err != nil || len(list) == 0 {
		return err
	}
	occ, err := occupancy(ctx, s.db, repo.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range list {
		if occ.Occupant != nil && w.ID != occ.Occupant.ID {
			if err := deferImpl(ctx, s.db, w, occ.waitingReason()); err != nil {
				errs = append(errs, fmt.Errorf("工作项 #%d：%w", w.ID, err))
			}
			continue
		}
		if err := s.planImpl(ctx, repo, w); err != nil {
			errs = append(errs, fmt.Errorf("规划工作项 #%d 的实现：%w", w.ID, err))
		}
	}
	return errors.Join(errs...)
}

// deferImpl 记下工作项 w 本轮的实现被推迟的原因（不是占用者）。只在它仍在等这一轮的实现、原因不同时写。
func deferImpl(ctx context.Context, q store.Execer, w *WorkItem, reason string) error {
	_, err := q.Exec(ctx, `UPDATE dev_work_items SET reason=$4, updated_at=now() WHERE id=$1 AND `+implStill("$2", "$3")+` AND reason<>$4`,
		w.ID, w.ImplRound, w.FixRounds, shorten.Text(reason, maxReasonLen))
	return err
}

// planImpl 规划工作项本轮的实现：配置和说明从当时的默认分支读取，带上工作项的 Issue（当前内容）、已合并的设计、
// 上一轮的交接。关联时把任务的起点记为工作项的 base_sha（本轮的起点，retry 没有正式分支和 checkpoint 时从它继续）。
func (s *Service) planImpl(ctx context.Context, repo *store.Repo, w *WorkItem) error {
	f := s.forge(repo)
	dc, why, err := loadDevConfig(ctx, f, repo)
	if err != nil {
		return err
	}
	if why != "" {
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := s.transition(ctx, tx, w, EvExecFailed, "无法开始实现："+why, "")
			return err
		})
	}
	if why, err := designMissing(ctx, f, w, dc.head); err != nil || why != "" {
		if err != nil {
			return err
		}
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := s.transition(ctx, tx, w, EvExecFailed, why, "")
			return err
		})
	}
	tools, why, err := devTools(ctx, f, dc.head, dc.cfg)
	if err != nil {
		return err
	}
	if why != "" {
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := s.transition(ctx, tx, w, EvExecFailed, "无法开始实现："+why, "")
			return err
		})
	}
	issues, changed, err := currentIssues(ctx, s.db, f, w)
	if err != nil {
		return err
	}
	docs, err := s.docsFor(ctx, repo.ID, w.ID)
	if err != nil {
		if !docsync.IsPermanentError(err) {
			return err // 临时失败：下一轮重试，不能把工作项打成受阻
		}
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, e := s.transition(ctx, tx, w, EvExecFailed, "无法拉取设计文档："+err.Error(), "")
			return e
		})
	}
	// 交接取已合并的那次交付（上一轮实现，或者设计）：之后交付、还没推进的返修报告不算
	merged, err := taskDelivery(ctx, s.db, w.ID, w.HeadTaskID)
	if err != nil {
		return err
	}
	cfg := dc.cfg
	r, t, err := s.plan(ctx, &devRequest{
		repo: repo, ref: gitref.BranchPrefix + repo.DefaultBranch, sha: dc.head, platforms: cfg.DevPlatforms(),
		dedupeKey: implKey(repo.ID, w.ID, w.ImplRound, w.FixRounds), sender: auditActor, timeout: int(cfg.DevTimeout().Minutes()),
		spec: &aispec.DevelopSpec{
			Mode: aispec.DevImplement, Agent: cfg.Development.Agent, Instructions: dc.instructions, ConfigSHA: dc.head,
			WorkItemID: w.ID, Branch: w.Branch(), BaseRef: repo.DefaultBranch, Issues: issues, Handoff: handoff(merged, w), Round: w.FixRounds,
			RequirementsChanged: changed || w.RequirementsChanged, Services: cfg.Services(), PlatformTools: tools, Repositories: cfg.Repositories(),
			MaxFixRounds: w.MaxFixRounds, Phase: w.Phase, ImplRound: w.ImplRound, DesignPR: derefInt(w.DesignPR), DesignSHA: w.DesignSHA,
			DesignDocs: w.DesignDocs, StartSHA: dc.head, Docs: docs,
		},
	})
	if err != nil {
		return err
	}
	// 去重键命中了之前提交、还没关联上的任务时，它的起点和目标分支是当时的默认分支（期间可能已经推进或者改了）：
	// 本轮的起点和目标分支以任务为准
	planned, err := developSpec(t)
	if err != nil {
		return err
	}
	unneeded := false
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		unneeded = false
		linked, err := linkImpl(ctx, tx, w.ID, t.ID, planned.BaseRef, planned.StartSHA, w.ImplRound, w.FixRounds, changed)
		if err != nil || linked {
			if linked {
				s.log.Info("AI 开发：已规划实现", "repo", repo.FullName(), "work_item", w.ID, "impl_round", w.ImplRound, "request", r.ID)
			}
			return err
		}
		var cur *int64
		if err := tx.QueryRow(ctx, `SELECT task_id FROM dev_work_items WHERE id=$1`, w.ID).Scan(&cur); err != nil {
			return err
		}
		// 期间结束时已经关联了这个任务（见 RecordResult）时什么也不做；工作项被终止、换了轮次或已经有了别的任务时
		// 这个请求不再需要
		unneeded = cur == nil || *cur != t.ID
		return nil
	})
	if err != nil || !unneeded {
		return err
	}
	// 取消在单独的事务中进行（加锁顺序：并发组 → 请求 → 任务 → 工作项）
	return s.core.Cancel(ctx, r.ID, ai.CancelWorkItemDone, "")
}

// designMissing 确认已合并的设计在实现的起点 head（默认分支）中：设计 PR 合并到了别的分支（人改了目标分支）时返回
// 原因，不从缺少设计的起点开始实现。没有记下合并 commit 时不检查。
func designMissing(ctx context.Context, f forge.Repo, w *WorkItem, head string) (string, error) {
	if w.DesignSHA == "" {
		return "", nil
	}
	behind, err := f.Behind(ctx, w.DesignSHA, head)
	switch {
	case forge.IsRejected(err):
		behind = -1 // 平台上找不到这个 commit：同样不能确认
	case err != nil:
		return "", fmt.Errorf("确认设计 %s 在默认分支中：%w", shorten.Hash(w.DesignSHA), err)
	}
	if behind == 0 {
		return "", nil
	}
	return fmt.Sprintf("已合并的设计 PR #%d（%s）不在默认分支（%s）中，可能合并到了别的分支：设计进入默认分支后 retry 开始实现",
		derefInt(w.DesignPR), shorten.Hash(w.DesignSHA), shorten.Hash(head)), nil
}

// linkImpl 把实现任务 taskID 关联到工作项（只关联到规划时的那一轮：同一个实现轮次、同一个 fix_rounds、还没有任务），
// 记下本轮的目标分支 baseRef 和起点 start（规划时的默认分支和它的 head：仓库改了默认分支之后，新的一轮从新的默认
// 分支开始，PR 也开到它上面）。返回是否关联了。
func linkImpl(ctx context.Context, tx pgx.Tx, workItem, taskID int64, baseRef, start string, implRound, fixRounds int, changed bool) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE dev_work_items SET task_id=$2, base_ref=coalesce(nullif($7, ''), base_ref), base_sha=$3, round_ran=true,
		requirements_changed=requirements_changed OR $4, updated_at=now() WHERE id=$1 AND `+implStill("$5", "$6"),
		workItem, taskID, start, changed, implRound, fixRounds, baseRef)
	return tag.RowsAffected() > 0, err
}

// implStill 返回"工作项仍在等这一轮的实现任务"的条件（参数 round 是实现轮次，fix 是 fix_rounds）。
func implStill(round, fix string) string {
	return `state='` + StateDeveloping + `' AND phase='` + PhaseImplement + `' AND task_id IS NULL AND NOT round_ran
	AND impl_round=` + round + ` AND fix_rounds=` + fix
}

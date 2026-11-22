package mergequeue

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自动入队（PLAN #29.5）：开启了 auto 的仓库，PR 的验证全部通过（包括 AI 审计无问题）时由执行者给 PR 加上 kiln:merge 并入队。
//
// 候选（merge_auto_candidates）在 PR 的 run 结束、PR 去掉 kiln:hold、转为 ready 时记下（Nominate、outbox.NominateAutoMerge），
// 由仓库的执行者在同一次处理中检查：条件每次按最新状态重新判断，不满足就丢掉候选，之后的事件会再记下。

// HoldLabel 是不自动入队的 label（手动入队不受影响），值见 mergecfg.HoldLabel。
const HoldLabel = mergecfg.HoldLabel

// AutoActor 是自动入队的操作者（merge_queue.enqueued_by 和审计日志）。
const AutoActor = "auto"

// Nominate 记下 PR pr 可能可以自动入队，并通知执行者检查（PR 已经在队列中时什么也不做）。
func (s *Service) Nominate(ctx context.Context, repoID int64, pr int) error {
	if err := mqevents.NominateAutoMerge(ctx, s.outbox, s.db, repoID, pr); err != nil {
		return err
	}
	s.wake()
	return nil
}

// autoEnqueue 检查仓库的候选，满足条件的加 label 并入队。返回是否有新入队的。暂时失败时返回错误，候选保留，稍后重试。
func (p *pass) autoEnqueue(ctx context.Context) (bool, error) {
	rows, err := p.conn.Query(ctx, `SELECT pr_number, created_at FROM merge_auto_candidates WHERE repo_id=$1 ORDER BY created_at, pr_number`,
		p.repo.ID)
	if err != nil {
		return false, err
	}
	type candidate struct {
		PR int
		At time.Time
	}
	cands, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
	if err != nil || len(cands) == 0 {
		return false, err
	}
	if err := p.loadConfig(ctx); err != nil {
		return false, err
	}
	if p.cfgErr != "" || !p.cfg.Auto {
		// 没有开启（或配置错误）：丢掉读到的候选
		for _, c := range cands {
			if err := p.dropCandidate(ctx, c.PR, c.At); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	added := false
	for _, c := range cands {
		ok, err := p.autoEnqueueOne(ctx, c.PR)
		if err != nil {
			return added, err
		}
		added = added || ok
		if err := p.dropCandidate(ctx, c.PR, c.At); err != nil {
			return added, err
		}
	}
	return added, nil
}

// dropCandidate 删除检查过的候选：只删读到的那一版（检查期间重新提名的保留，下一次处理再检查）。
func (p *pass) dropCandidate(ctx context.Context, pr int, at time.Time) error {
	_, err := p.conn.Exec(ctx, `DELETE FROM merge_auto_candidates WHERE repo_id=$1 AND pr_number=$2 AND created_at=$3`, p.repo.ID, pr, at)
	return err
}

// autoEnqueueOne 检查一个候选，满足条件时加 label 并入队，返回是否入队。
func (p *pass) autoEnqueueOne(ctx context.Context, pr int) (bool, error) {
	pull, err := p.f.GetPull(ctx, pr)
	if err != nil {
		return false, fmt.Errorf("读取 PR #%d：%w", pr, err)
	}
	if !autoPullEligible(pull, p.cfg, p.repo.DefaultBranch) {
		return false, nil
	}
	if held, err := p.holdPending(ctx, pull); err != nil || held {
		return false, err
	}
	var queued bool
	var removed, notHandedBack []string
	if err := p.conn.QueryRow(ctx, `SELECT
			EXISTS(SELECT 1 FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 AND state='queued'),
			ARRAY(SELECT DISTINCT head_sha FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 AND state='removed'),
			ARRAY(SELECT DISTINCT head_sha FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 AND state='removed' AND removal<>$3)`,
		p.repo.ID, pr, RemovalConflictDev).Scan(&queued, &removed, &notHandedBack); err != nil {
		return false, err
	}
	if queued {
		return false, nil
	}
	if again, err := p.removedBefore(ctx, pull.HeadSHA, removed, notHandedBack); err != nil || again {
		return false, err
	}
	s := &snapshot{Pull: pull, DefaultBranch: p.repo.DefaultBranch, Required: p.cfg.Requirements, Now: p.s.now()}
	if err := p.conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pr_plans WHERE repo_id=$1 AND pr_number=$2 AND sha=$3 AND base_ref=$4)`,
		p.repo.ID, pr, pull.HeadSHA, pull.BaseRef).Scan(&s.Processed); err != nil {
		return false, err
	}
	if err := p.loadRuns(ctx, s, pr, pull.HeadSHA); err != nil {
		return false, err
	}
	if !autoVerified(s) {
		return false, nil
	}
	// 先加 label 再入队：入队后执行者看到没有 label 会当成被去掉而出队。label 产生的 webhook 也会入队（幂等）。
	// 已经有 label 但不在队列中（上一次加 label 的响应丢失、label 的 webhook 也没有送到，或者 webhook 还没处理）：
	// 不再加，直接入队（独立审查发现：以前把"有 label"当成已经入队，丢掉候选，PR 永远停在队列之外）
	if !pull.HasLabel(Label) {
		if err := p.f.AddLabel(ctx, pr, Label); err != nil {
			return false, fmt.Errorf("给 PR #%d 加 %s：%w", pr, Label, err)
		}
	}
	added := false
	err = p.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO merge_queue (repo_id, pr_number, head_sha, base_ref, enqueued_by) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (repo_id, pr_number) WHERE state='queued' DO NOTHING`, p.repo.ID, pr, pull.HeadSHA, pull.BaseRef, AutoActor)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		added = true
		return store.Audit(ctx, tx, AutoActor, "mergequeue.enqueue", map[string]any{
			"repo_id": p.repo.ID, "pr": pr, "head": pull.HeadSHA, "base_ref": pull.BaseRef,
		})
	})
	if added {
		p.s.log.Info("合并队列：自动入队", "repo", p.repo.FullName(), "pr", pr, "head", pull.HeadSHA)
	}
	return added, err
}

// removedBefore 判断 head 是否被移出过队列（用户手动移除、验证没有通过等）：被移出过的不再自动入队，推送新的 commit
// 或手动入队。removed 是这个 PR 被移出时记录的 head。队列更新分支产生的 merge commit（两个父提交，其中一个是被移出的
// head）同样算被移出过：用户在更新分支期间移出时记录的是旧 head，新 head 由 Kiln 推送，不是用户的新 commit（独立审查发现）。
// 用户自己把默认分支合并进来的 commit 也会被这样算进去，只是不自动入队（手动入队不受影响）。
//
// 父提交一条只看 parentRule（因为冲突交回 AI 开发而移出的 head 除外，PLAN #29.5）：AI 解决冲突时把默认分支合并进被移出的
// head，产生的正是这种 merge commit，它是返修的结果，验证通过后要重新入队；同一个 head 被移出过（removed）仍然适用，
// 冲突的 head 不会被反复加入再移出。
func (p *pass) removedBefore(ctx context.Context, head string, removed, parentRule []string) (bool, error) {
	if slices.Contains(removed, head) {
		return true, nil
	}
	if len(parentRule) == 0 {
		return false, nil
	}
	parents, err := p.f.CommitParents(ctx, head)
	if err != nil {
		return false, fmt.Errorf("读取 %s 的父提交：%w", head, err)
	}
	return len(parents) == 2 && slices.ContainsFunc(parents, func(s string) bool { return slices.Contains(parentRule, s) }), nil
}

// holdPending 判断 PR 是否有待加的 kiln:hold（Deps.HoldPending）。
func (p *pass) holdPending(ctx context.Context, pull *forge.Pull) (bool, error) {
	if p.s.holdPending == nil {
		return false, nil
	}
	return p.s.holdPending(ctx, p.conn, p.repo.ID, pull.Number, pull.HeadRef)
}

// autoPullEligible 判断 PR 本身是否可以自动入队：打开、不是 draft、不是 fork、目标是允许合并的分支、没有 kiln:hold。
// 已经有 kiln:merge 不算不满足：是否已在队列中按队列的记录判断，不按 label。
func autoPullEligible(p *forge.Pull, cfg *mergecfg.Config, defaultBranch string) bool {
	return p.State == forge.PullOpen && !p.Draft && !p.Fork && cfg.AllowsBranch(p.BaseRef, defaultBranch) && !p.HasLabel(HoldLabel)
}

// autoVerified 判断 head 的验证是否全部通过（与合并前的判断相同，PLAN #28.3）：按默认分支规划完成，run 全部结束、
// 没有失败、至少一个没被跳过，必需的验证（auto 要求其中有 ai-review，只有结论 no_issues 的审计 run 成功）都存在且成功。
func autoVerified(s *snapshot) bool {
	if !s.Processed || len(s.Runs) == 0 {
		return false
	}
	for _, r := range s.Runs {
		if !r.State.Terminal() || (r.State != store.RunSucceeded && r.State != store.RunSkipped) {
			return false
		}
	}
	if !slices.ContainsFunc(s.Runs, func(r runInfo) bool { return r.State != store.RunSkipped }) {
		return false
	}
	return len(missingRequired(s)) == 0 && len(unsuccessfulRequired(s)) == 0
}

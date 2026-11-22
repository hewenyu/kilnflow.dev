package ingest

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 引用对账（PLAN #7.3）：列出仓库的分支、tag 和打开的 PR，与 ref_heads 比较，补上丢失的 webhook。
//
// 默认关闭：Server 按 webhook 事件工作，没有事件或显式操作时不向 GitHub 发请求，
// 所以不会自动发现没有送达的事件。恢复方式：
//   - 显式补对账：Dashboard 仓库页的"补对账"或 kiln repo reconcile，只对账一个仓库（ReconcileRepo）；
//   - 可选的周期对账：KILN_RECONCILE_INTERVAL 和 KILN_RECONCILE_REPOS 都设置时，按间隔对账指定范围内的仓库（RunReconcile）。
//
// 仓库第一次对账时，ref_heads 中还没有的 ref 只记录为基线、不触发构建（避免把历史 tag 全部发布一遍）；
// 已经由事件记录过的 ref 照常比较，变化了就补上。

// reconcileAllRepos 是 ReconcileRepos 中表示全部启用中仓库的写法。
const reconcileAllRepos = "*"

// RunReconcile 按 ReconcileInterval 周期对账 ReconcileRepos 范围内的仓库，直到 ctx 结束。
// 没有配置 GitHub App、间隔为 0（默认）或范围为空时不对账，直接返回。
func (i *Ingester) RunReconcile(ctx context.Context) {
	if i.gh == nil || i.cfg.ReconcileInterval <= 0 || len(i.cfg.ReconcileRepos) == 0 {
		i.log.Info("周期对账已关闭（默认按 webhook 事件工作；漏掉的事件用\"补对账\"恢复，PLAN #7.3）")
		return
	}
	i.log.Info("周期对账已开启", "interval", i.cfg.ReconcileInterval, "repos", strings.Join(i.cfg.ReconcileRepos, ","))
	select {
	case <-ctx.Done():
		return
	case <-time.After(i.cfg.ReconcileDelay):
	}
	t := time.NewTicker(i.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		i.reconcileAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcileReserve 是对账为 webhook、规划和发布保留的配额比例：installation 剩余配额低于上限的这个比例
// （或者正在限流）时，本轮跳过它的仓库，等配额恢复后的下一轮再对账。其他 installation 照常对账。
const reconcileReserve = 0.2

// pausedInstallation 记录本轮对账中被跳过的 installation。
type pausedInstallation struct {
	status gh.RateLimitStatus
	reason string
	repos  int
}

func (i *Ingester) reconcileAll(ctx context.Context) {
	type repoRef struct{ ID, Installation int64 }
	all := slices.Contains(i.cfg.ReconcileRepos, reconcileAllRepos)
	rows, err := i.db.Query(ctx, `SELECT id, installation_id FROM repos WHERE active AND ($1 OR lower(owner||'/'||name) = ANY($2))
		ORDER BY installation_id, id`, all, i.cfg.ReconcileRepos)
	if err != nil {
		i.log.Error("对账：列出仓库", "err", err)
		return
	}
	repos, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repoRef])
	if err != nil {
		i.log.Error("对账：列出仓库", "err", err)
		return
	}
	paused := map[int64]*pausedInstallation{}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if p := paused[r.Installation]; p != nil {
			p.repos++
			continue
		}
		if low, st := i.gh.LowBudget(r.Installation, reconcileReserve); low {
			reason := "剩余配额不足，留给 webhook、规划和发布"
			if !st.Until.IsZero() {
				reason = "被 GitHub 限流"
			}
			paused[r.Installation] = &pausedInstallation{status: st, reason: reason, repos: 1}
			continue
		}
		_, err := i.ReconcileRepo(ctx, r.ID)
		if rl, ok := gh.RateLimited(err); ok {
			// 这个仓库没有对账完，下一轮重新比较；同一 installation 的其余仓库本轮不再请求
			paused[r.Installation] = &pausedInstallation{
				status: gh.RateLimitStatus{Until: rl.Until, Kind: rl.Kind}, reason: "被 GitHub 限流", repos: 1,
			}
			continue
		}
		if err != nil {
			i.log.Warn("对账失败", "repo", r.ID, "err", err)
		}
	}
	for inst, p := range paused {
		args := []any{"installation", inst, "reason", p.reason, "skipped_repos", p.repos}
		if p.status.Until.IsZero() {
			args = append(args, "remaining", p.status.Remaining, "limit", p.status.Limit, "reset", p.status.Reset)
		} else {
			args = append(args, "kind", p.status.Kind, "until", p.status.Until)
		}
		i.log.Warn("对账：暂停 installation", args...)
	}
}

// isRateLimited 判断错误是否由 GitHub 限流引起。
func isRateLimited(err error) bool {
	_, ok := gh.RateLimited(err)
	return ok
}

// refHead 是 ref_heads 中记录的一个 ref 的位置。
type refHead struct {
	// SHA 是 ref 指向的 commit。
	SHA string
	// Object 是 ref 直接指向的对象：附注 tag 为 tag 对象，其他与 SHA 相同。
	Object string
	// Base 是 PR 的 head 按哪个目标分支规划过（只用于 PR ref；基线和以前的记录为空）。
	Base string
}

// refHeads 返回仓库已记录的 ref：ref → 位置。
func (i *Ingester) refHeads(ctx context.Context, repoID int64) (map[string]refHead, error) {
	rows, err := i.db.Query(ctx, `SELECT ref, sha, object_sha, base_ref FROM ref_heads WHERE repo_id=$1`, repoID)
	if err != nil {
		return nil, err
	}
	known := map[string]refHead{}
	var ref string
	var h refHead
	if _, err := pgx.ForEachRow(rows, []any{&ref, &h.SHA, &h.Object, &h.Base}, func() error {
		known[ref] = h
		return nil
	}); err != nil {
		return nil, err
	}
	return known, nil
}

// planMissed 为对账发现的变化规划 run 和 AI 审计，成功后把 ref 记入 ref_heads，返回新建的 run 和审计请求数。
// 只有 GitHub 限流时返回错误（这个仓库本轮停止对账）；其他错误只记录，不更新 ref_heads，
// 下一轮对账重试（PlanEvent 按 dedupe_key 去重）。
func (i *Ingester) planMissed(ctx context.Context, req *planner.Request, object string) (int, error) {
	n, err := i.planEvent(ctx, req)
	if isRateLimited(err) {
		return n.total(), err
	}
	if err != nil {
		i.log.Warn("对账：规划", "repo", req.Repo.FullName(), "event", req.Event, "ref", req.Ref, "err", err)
		return n.total(), nil
	}
	i.setRefHead(ctx, req.Repo.ID, req.Ref, req.SHA, object, req.BaseRef)
	return n.total(), nil
}

// ReconcileResult 是一个仓库的对账结果。
type ReconcileResult struct {
	// Repo 是仓库全名。
	Repo string
	// Baseline 为 true 表示这是仓库第一次对账，没有记录过的 ref 只记录为基线。
	Baseline bool
	// Branches、Tags、Pulls 是 GitHub 上的分支、tag 和打开的 PR 数；Runs 是补上的 run 数。
	Branches, Tags, Pulls, Runs int
}

// ReconcileRepo 对一个仓库对账：列出它的分支、tag 和打开的 PR，与 ref_heads 比较，为漏掉的变化规划 run。
// 只请求这一个仓库；被 GitHub 限流时返回限流错误（gh.RateLimited），已经补上的部分不会重复（dedupe_key）。
func (i *Ingester) ReconcileRepo(ctx context.Context, repoID int64) (ReconcileResult, error) {
	res, err := i.reconcileRepo(ctx, repoID)
	if res.Runs > 0 {
		i.hub.All()
	}
	return res, err
}

// repoReconcile 是一个仓库一次对账的状态。
type repoReconcile struct {
	i    *Ingester
	repo *store.Repo
	inst *gh.Inst
	// known 是 ref_heads 中已记录的 ref；baseline 为 true 时，其中没有的 ref 只记录为基线。
	known    map[string]refHead
	baseline bool
	res      *ReconcileResult
}

// changed 判断 ref 是否需要处理：与记录相同时返回 false；第一次对账且没有记录过时记录基线并返回 false。
func (c *repoReconcile) changed(ctx context.Context, ref, sha, object string, same bool) bool {
	_, ok := c.known[ref]
	if ok && same {
		return false
	}
	if c.baseline && !ok {
		c.i.setRefHead(ctx, c.repo.ID, ref, sha, object, "")
		return false
	}
	return true
}

func (c *repoReconcile) plan(ctx context.Context, req *planner.Request, object string) error {
	n, err := c.i.planMissed(ctx, req, object)
	c.res.Runs += n
	return err
}

func (c *repoReconcile) branches(ctx context.Context) error {
	branches, err := c.inst.ListBranches(ctx, c.repo.FullName())
	if err != nil {
		return err
	}
	c.res.Branches = len(branches)
	for _, b := range branches {
		ref := workflow.RefPrefixBranch + b.Name
		old, ok := c.known[ref]
		if !c.changed(ctx, ref, b.Commit.SHA, b.Commit.SHA, old.SHA == b.Commit.SHA) {
			continue
		}
		before := gitref.ZeroSHA
		if ok {
			before = old.SHA
		}
		req := &planner.Request{Repo: c.repo, Event: workflow.EventPush, Ref: ref, SHA: b.Commit.SHA, Before: before, Source: planner.SourceReconcile}
		if err := c.plan(ctx, req, b.Commit.SHA); err != nil {
			return err
		}
	}
	return nil
}

func (c *repoReconcile) tags(ctx context.Context) error {
	tags, err := c.inst.MatchingRefs(ctx, c.repo.FullName(), "tags/")
	if err != nil {
		return err
	}
	c.res.Tags = len(tags)
	for _, t := range tags {
		if old, ok := c.known[t.Ref]; ok && old.Object == t.Object.SHA {
			continue
		}
		commit, obj, err := c.inst.PeelObject(ctx, c.repo.FullName(), t.Object)
		if isRateLimited(err) {
			return err
		}
		if err != nil {
			c.i.log.Warn("对账：解析 tag", "repo", c.repo.FullName(), "ref", t.Ref, "err", err)
			continue
		}
		if !c.changed(ctx, t.Ref, commit, obj, false) {
			continue
		}
		if err := c.plan(ctx, &planner.Request{Repo: c.repo, Event: workflow.EventPush, Ref: t.Ref, SHA: commit, Source: planner.SourceReconcile}, obj); err != nil {
			return err
		}
	}
	return nil
}

func (c *repoReconcile) pulls(ctx context.Context) error {
	pulls, err := c.inst.ListOpenPulls(ctx, c.repo.FullName())
	if err != nil {
		return err
	}
	c.res.Pulls = len(pulls)
	for _, p := range pulls {
		if gh.IsFork(p.Head, p.Base) {
			continue // fork 来的 PR 一律丢弃
		}
		ref := workflow.PullRef(p.Number)
		old, ok := c.known[ref]
		// 规划过的 head 改了目标分支（漏掉了 edited 事件）也要重新规划；没有规划过的（基线）不补
		same := old.SHA == p.Head.SHA && (old.Base == "" || old.Base == p.Base.Ref)
		if !c.changed(ctx, ref, p.Head.SHA, p.Head.SHA, same) {
			continue
		}
		action := actionSynchronize
		if !ok {
			action = actionOpened
		}
		req := &planner.Request{
			Repo: c.repo, Event: workflow.EventPullRequest, Ref: ref, SHA: p.Head.SHA, Action: action,
			PRNumber: p.Number, BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA, HeadRef: p.Head.Ref, SenderLogin: p.User.Login,
			SenderIsBot: p.User.Type == senderTypeBot, Source: planner.SourceReconcile,
		}
		if err := c.plan(ctx, req, p.Head.SHA); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) reconcileRepo(ctx context.Context, repoID int64) (ReconcileResult, error) {
	var res ReconcileResult
	if i.gh == nil {
		return res, errNoGitHubApp
	}
	repo, err := store.GetRepo(ctx, i.db, repoID)
	if err != nil {
		return res, err
	}
	res.Repo = repo.FullName()
	known, err := i.refHeads(ctx, repo.ID)
	if err != nil {
		return res, err
	}
	// 第一次对账：ref_heads 中没有的 ref 只记录基线；已经由事件记录过的照常比较
	c := &repoReconcile{i: i, repo: repo, inst: i.gh.Inst(repo.InstallationID), known: known, baseline: !repo.BaselineDone, res: &res}
	res.Baseline = c.baseline
	for _, step := range []func(context.Context) error{c.branches, c.tags, c.pulls} {
		if err := step(ctx); err != nil {
			return res, err
		}
	}
	if c.baseline {
		if _, err := i.db.Exec(ctx, `UPDATE repos SET baseline_done=true WHERE id=$1`, repo.ID); err != nil {
			return res, err
		}
		i.log.Info("对账：记录基线", "repo", res.Repo, "branches", res.Branches, "tags", res.Tags, "pulls", res.Pulls, "runs", res.Runs)
	} else if res.Runs > 0 {
		i.log.Info("对账：补上丢失的事件", "repo", res.Repo, "runs", res.Runs)
	}
	return res, nil
}

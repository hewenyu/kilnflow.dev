package planner

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 重跑（PLAN #6.6）。

type rerunOptions struct {
	reuse         map[string]*cistore.Job
	force         map[string]bool
	keepOnFailure bool
	// purged 是规划时 run 的过程数据是否已经清理；创建 rerun 时在 run 的行锁下复查。
	purged bool
	// prevRerun 是规划时 run 的 current_rerun（reuse 中的 job 来自这一次运行）；创建 rerun 时在 run 的行锁下复查。
	prevRerun int
	// reuseReason 是沿用的 job 的原因（%d 是被沿用的 job ID）；为空时是"沿用上一次运行的结果"。
	reuseReason string
}

// RerunRequest 是一次重跑请求。
type RerunRequest struct {
	RunID int64
	Mode  store.RerunMode // 空值等同于 failed
	// JobID 是 Mode 为 job 时要重跑的 job。
	JobID         int64
	Actor         string
	KeepOnFailure bool
	// DeliveryID 是触发重跑的 webhook delivery（GitHub 上的 Re-run），用作幂等键：
	// 同一个 delivery 对同一个 run 最多创建一次 rerun（run_reruns.delivery_id），
	// 事件中途被限流、恢复后重试时，已经处理过的 run 不再重跑。为空时（Dashboard、CLI）不去重。
	DeliveryID string
}

// Rerun 为一个已结束的 run 创建新的 rerun，返回新的 rerun 序号。
// req.DeliveryID 已经为该 run 创建过 rerun 时不再创建，直接返回那次的序号（不论它现在是否已经结束）。
func (p *Planner) Rerun(ctx context.Context, req RerunRequest) (int, error) {
	if p.gh == nil {
		return 0, errNoGitHubApp
	}
	// 先于读取 GitHub 和"还没有结束"的检查：同一个 delivery 重试时既不消耗配额，也不把已处理的 run 当作失败
	if n, err := deliveryRerun(ctx, p.db, req); err != nil || n > 0 {
		return n, err
	}
	run, err := cistore.GetRun(ctx, p.db, req.RunID, false)
	if err != nil {
		return 0, err
	}
	if run.State.Open() {
		return 0, fmt.Errorf("run #%d 还没有结束，不能重跑", req.RunID)
	}
	pr, err := p.replan(ctx, run)
	if err != nil {
		return 0, err
	}
	prev, err := cistore.RerunJobs(ctx, p.db, run.ID, run.CurrentRerun, false)
	if err != nil {
		return 0, err
	}
	opt, err := rerunPlan(req, prev, pr, run.Purged())
	if err != nil {
		return 0, err
	}
	opt.prevRerun = run.CurrentRerun
	var rerun int
	created := false
	err = p.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		rerun, created, err = p.startRerun(ctx, tx, req, run, pr, opt)
		return err
	})
	if err == nil && created {
		p.hub.All()
	}
	return rerun, err
}

// deliveryRerun 返回 req.DeliveryID 已经为 req.RunID 创建的 rerun 序号；没有 delivery 或还没有创建时返回 0。
func deliveryRerun(ctx context.Context, q store.Querier, req RerunRequest) (int, error) {
	if req.DeliveryID == "" {
		return 0, nil
	}
	var n int
	err := q.QueryRow(ctx, `SELECT rerun FROM run_reruns WHERE run_id=$1 AND delivery_id=$2`, req.RunID, req.DeliveryID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// replan 按 run 的 commit 和当前配置重新规划 workflow。
func (p *Planner) replan(ctx context.Context, run *cistore.Run) (*plannedRun, error) {
	repo, err := store.GetRepo(ctx, p.db, run.RepoID)
	if err != nil {
		return nil, err
	}
	inst := p.gh.Inst(repo.InstallationID)
	tree, err := p.tree(ctx, inst, repo, run.SHA)
	if err != nil {
		return nil, err
	}
	blobSHA, ok := workflowFiles(tree)[run.Workflow]
	if !ok {
		return nil, fmt.Errorf("commit %s 中已经没有 %s", run.SHA, run.Workflow)
	}
	data, err := p.blob(ctx, inst, repo, blobSHA)
	if err != nil {
		return nil, err
	}
	pr := &plannedRun{File: run.Workflow, Name: run.WorkflowName, BlobSHA: blobSHA, State: store.RunActive}
	wf, diags := workflow.Parse(run.Workflow, data)
	if diags.HasErrors() {
		pr.ConfigError = diags.Errors().Error()
		return pr, nil
	}
	in := &expandInput{
		Inst: inst, Repo: repo, Tree: tree, Workflow: wf, GitHub: run.GitHubCtx, Inputs: run.Inputs, SenderIsBot: run.SenderIsBot,
		TrustedAutoTag: isTrustedAutoTag(run.Source, run.Event, run.Ref),
	}
	if err := p.expandRun(ctx, in, pr); err != nil {
		return nil, err
	}
	return pr, nil
}

// rerunPlan 按重跑方式决定哪些 job 沿用上一次的结果、哪些强制执行。
// purged 表示 run 的过程数据已经按保留策略清理：产物已经不在了，有产物的 job 不能沿用，重新规划（可能命中缓存）。
func rerunPlan(req RerunRequest, prev []*cistore.Job, pr *plannedRun, purged bool) (*rerunOptions, error) {
	prevByKey := map[string]*cistore.Job{}
	var target *cistore.Job
	for _, j := range prev {
		prevByKey[j.JobKey] = j
		if j.ID == req.JobID {
			target = j
		}
	}
	opt := &rerunOptions{reuse: map[string]*cistore.Job{}, force: map[string]bool{}, keepOnFailure: req.KeepOnFailure, purged: purged}
	switch req.Mode {
	case "", store.RerunFailed:
		for k, j := range prevByKey {
			if j.Reusable(purged) {
				opt.reuse[k] = j
			}
		}
	case store.RerunAll:
	case store.RerunAllNoCache:
		for _, j := range pr.Jobs {
			opt.force[j.Key] = true
		}
	case store.RerunJob:
		if target == nil {
			return nil, fmt.Errorf("job #%d 不属于 run #%d 的最新一次运行", req.JobID, req.RunID)
		}
		// 该 job 及其所有下游重新规划，其他 job 沿用
		down := downstream(pr.Jobs, target.JobKey)
		for k, j := range prevByKey {
			if !down[k] {
				opt.reuse[k] = j
			}
		}
		if target.State == store.JobSucceeded {
			opt.force[target.JobKey] = true
		}
		if purged {
			for k, j := range opt.reuse {
				if j.ResultManifest != nil && j.Kind != workflow.JobKindPublish {
					delete(opt.reuse, k)
				}
			}
		}
	default:
		return nil, fmt.Errorf("未知的重跑方式 %s", req.Mode)
	}
	return opt, nil
}

// downstream 返回 key 及其所有（直接或间接）下游 job 的 key。
func downstream(jobs []*workflow.PlannedJob, key string) map[string]bool {
	down := map[string]bool{key: true}
	for changed := true; changed; {
		changed = false
		for _, j := range jobs {
			if down[j.Key] {
				continue
			}
			for _, n := range j.NeedsKeys {
				if down[n] {
					down[j.Key] = true
					changed = true
					break
				}
			}
		}
	}
	return down
}

// startRerun 在事务中创建新的 rerun 并插入 job，返回 rerun 序号以及是否新建。重跑同样受并发组约束（PLAN #6.5）。
// 锁住 run 之后再按 delivery 去重一次：同一个 delivery 并发处理时只有一个能创建 rerun（唯一索引兜底）。
// prev 是规划之前读到的 run（未加锁），用于确定要预先锁住的并发组。
func (p *Planner) startRerun(ctx context.Context, tx pgx.Tx, req RerunRequest, prev *cistore.Run, pr *plannedRun, opt *rerunOptions) (int, bool, error) {
	// 加锁顺序是并发组 → run（PLAN #6.1）：重跑可能换组（工作流改了并发组），旧的组和新的组都要在锁 run 之前锁住。
	// 持有 run 的行锁之后再取另一个组的锁，会与"持有那个组、等待这个 run"的事务互相等待（死锁）。
	groups := rerunGroups(prev, pr)
	for _, key := range groups {
		if err := cistore.LockConcurrencyGroup(ctx, tx, prev.RepoID, key); err != nil {
			return 0, false, err
		}
	}
	if err := cistore.LockRuns(ctx, tx, []int64{req.RunID}); err != nil {
		return 0, false, err
	}
	run, err := cistore.GetRun(ctx, tx, req.RunID, true)
	if err != nil {
		return 0, false, err
	}
	// 规划之后 run 的并发组可能刚被另一个重跑改过：那个组不在预先锁住的集合里，这时不能再补锁（会违反加锁顺序），让调用方重试
	if run.ConcurrencyKey != nil && !slices.Contains(groups, *run.ConcurrencyKey) {
		return 0, false, fmt.Errorf("run #%d 的并发组刚刚被另一个重跑改变，请重新发起重跑", req.RunID)
	}
	if n, err := deliveryRerun(ctx, tx, req); err != nil || n > 0 {
		return n, false, err
	}
	rerun, err := p.insertRerun(ctx, tx, req, run, pr, opt)
	return rerun, err == nil, err
}

// rerunGroups 返回重跑要预先锁住的并发组：run 当前的组（要离开）和这次规划给出的组（要加入），排序、去重。
func rerunGroups(prev *cistore.Run, pr *plannedRun) []string {
	var keys []string
	if prev.ConcurrencyKey != nil {
		keys = append(keys, *prev.ConcurrencyKey)
	}
	if key, _ := runConcurrency(pr, rerunState(pr)); key != nil && !slices.Contains(keys, *key) {
		keys = append(keys, *key)
	}
	slices.Sort(keys)
	return keys
}

// rerunState 返回重跑这次运行的状态：配置有错误时直接失败，否则重新开始执行。
func rerunState(pr *plannedRun) store.RunState {
	if pr.ConfigError != "" {
		return store.RunFailed
	}
	return store.RunActive
}

// insertRerun 为已锁住的 run 创建第 current_rerun+1 次运行。
func (p *Planner) insertRerun(ctx context.Context, tx pgx.Tx, req RerunRequest, run *cistore.Run, pr *plannedRun, opt *rerunOptions) (int, error) {
	if run.State.Open() {
		return 0, fmt.Errorf("run #%d 还没有结束，不能重跑", req.RunID)
	}
	if run.Purged() != opt.purged {
		// 规划之后 run 的过程数据刚刚被清理：沿用的 job 的产物已经不在了（保留策略的清理在同一行锁下进行）
		return 0, fmt.Errorf("run #%d 的日志和产物刚刚按保留策略清理，请重新发起重跑", req.RunID)
	}
	if run.CurrentRerun != opt.prevRerun {
		// 规划之后 run 又被重跑过（而且已经结束）：沿用的 job 来自更早的一次运行，不是最新的结果
		return 0, fmt.Errorf("run #%d 刚刚被重跑过，请重新发起重跑", req.RunID)
	}
	rerun := run.CurrentRerun + 1
	state := rerunState(pr)
	// 这次规划的工作流决定新的并发组（工作流可能改了并发组）：run 上的并发组随之更新，重跑按新的组加入
	concKey, cancelInProgress := runConcurrency(pr, state)
	if _, err := tx.Exec(ctx, `UPDATE runs SET current_rerun=$2, state=$3, config_error=nullif($4,''), warnings=nullif($5,''),
		concurrency_key=$6, cancel_in_progress=$7,
		finished_at=CASE WHEN $8 THEN now() END, updated_at=now() WHERE id=$1`,
		run.ID, rerun, state, pr.ConfigError, pr.Warnings, concKey, cancelInProgress, state.Terminal()); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_reruns (run_id, rerun, state, conclusion, keep_on_failure, finished_at, delivery_id)
		VALUES ($1,$2,$3,$4,$5, CASE WHEN $6 THEN now() END, nullif($7,''))`,
		run.ID, rerun, state, state.ConclusionOrNil(), req.KeepOnFailure, state.Terminal(), req.DeliveryID); err != nil {
		return 0, err
	}
	detail := map[string]any{"run_id": run.ID, "rerun": rerun, "mode": req.Mode, "job_id": req.JobID}
	if req.DeliveryID != "" {
		detail["delivery"] = req.DeliveryID
	}
	if err := store.Audit(ctx, tx, req.Actor, "run.rerun", detail); err != nil {
		return 0, err
	}
	if state == store.RunFailed {
		return rerun, cicheck.EnqueueRun(ctx, p.outbox, tx, run.ID, rerun)
	}
	if err := p.insertJobs(ctx, tx, run.ID, rerun, pr, opt); err != nil {
		return 0, err
	}
	if concKey != nil {
		// 与新建 run 相同（PLAN #6.6）：cancel-in-progress 时取消组内其他 run；否则取代组内排队的 run，组内有 active 的 run 时排队
		if err := p.joinConcurrencyGroup(ctx, tx, run.RepoID, run.ID, rerun, *concKey, cancelInProgress); err != nil {
			return 0, err
		}
	}
	if err := cicheck.EnqueueRun(ctx, p.outbox, tx, run.ID, rerun); err != nil {
		return 0, err
	}
	return rerun, p.engine.AdvanceRun(ctx, tx, run.ID)
}

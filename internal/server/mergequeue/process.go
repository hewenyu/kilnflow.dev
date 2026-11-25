package mergequeue

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 执行者（PLAN #28.3）：持有仓库级的 advisory lock，读取快照、决定一个动作并执行，直到需要等待或队列为空。

// maxSteps 是一次处理最多执行的动作数（防止意外的循环）；用完时返回错误，由 outbox 稍后再处理。
const maxSteps = 20

// maxReasonLen 是出队原因和状态保存的上限。
const maxReasonLen = 1000

// ErrBusy 表示另一次处理正在进行（拿不到仓库的锁），稍后重试。
var ErrBusy = errors.New("合并队列正在处理这个仓库，稍后重试")

func unmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("合并队列通知的 payload 无法解析：%w", err)
	}
	return nil
}

// lockKey 是仓库合并队列的 advisory lock 键。
func lockKey(repoID int64) string { return fmt.Sprintf("kiln:mergequeue:%d", repoID) }

// Process 处理仓库 repoID 的合并队列。返回错误表示需要稍后重试（平台暂时不可用、另一次处理正在进行等）。可以重复调用。
func (s *Service) Process(ctx context.Context, repoID int64) (err error) {
	if s.forge == nil {
		return nil
	}
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	unlock, locked, err := store.TryAdvisoryLock(ctx, conn, lockKey(repoID))
	if err != nil {
		return err
	}
	if !locked {
		return ErrBusy
	}
	defer func() {
		// 释放失败时 store.TryAdvisoryLock 已关闭连接（session 级的锁随连接释放），不会把带着锁的连接放回连接池
		if uerr := unlock(ctx); uerr != nil {
			err = errors.Join(err, fmt.Errorf("释放合并队列锁：%w", uerr))
		}
	}()
	// 之后的查询和事务都用持有锁的这个连接（连接池上限为 1 时再借一个连接会一直等待）
	repo, err := store.GetRepo(ctx, conn, repoID)
	if err != nil {
		return err
	}
	p := &pass{s: s, conn: conn, repo: repo}
	if !repo.Active {
		return p.removeInactive(ctx)
	}
	p.f = s.forge(repo)
	// 先检查自动入队的候选（新入队的在这次处理中接着处理），再处理队列，最后关闭已合并的 PR 关联的 issue：
	// 前面的失败（稍后重试）不影响后面的
	_, aerr := p.autoEnqueue(ctx)
	return errors.Join(aerr, p.run(ctx), p.closeIssues(ctx))
}

// inactiveReason 是仓库停用时队列项的出队原因。
var inactiveReason = Note{Code: ReasonRepoInactive}

// removeInactive 把停用仓库（从 installation 中移除、App 被卸载等）排队中的项全部出队。平台已经不能访问，
// 不去 label、不评论；仓库恢复后重新加上 label 即可再次入队。
func (p *pass) removeInactive(ctx context.Context) error {
	repo := p.repo
	return p.tx(ctx, func(tx pgx.Tx) error {
		reason := inactiveReason.Text()
		prs, err := store.QueryIDs(ctx, tx, `UPDATE merge_queue SET state='removed', reason=$2, reason_code=$3, reason_params=$4,
			status='', status_code='', status_params='{}', action='none', finished_at=now(), updated_at=now()
			WHERE repo_id=$1 AND state='queued' RETURNING pr_number`, repo.ID, reason, inactiveReason.Code, inactiveReason.Params)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if err := store.Audit(ctx, tx, auditActor, "mergequeue.removed",
				map[string]any{"repo_id": repo.ID, "pr": pr, "reason": reason}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM merge_auto_candidates WHERE repo_id=$1`, repo.ID); err != nil {
			return err
		}
		// 已合并、还没关闭关联 issue 的项：平台已经不能访问，放弃
		st := Note{Code: StatusIssuesInactive}
		_, err = tx.Exec(ctx, `UPDATE merge_queue SET issues_state='failed', status=$2, status_code=$3, status_params=$4, updated_at=now()
			WHERE repo_id=$1 AND issues_state='pending'`, repo.ID, st.Text(), st.Code, st.Params)
		return err
	})
}

// tx 在持有锁的连接上执行事务。
func (p *pass) tx(ctx context.Context, f func(tx pgx.Tx) error) error {
	return store.TxOn(ctx, p.conn, f)
}

// pass 是一次处理。
type pass struct {
	s *Service
	// conn 是持有仓库锁的连接，这次处理的查询和事务都用它。
	conn *pgxpool.Conn
	repo *store.Repo
	f    forge.Repo
	// cfg、cfgErr 是 .kiln/merge.yml（第一次需要时读取）。
	cfg    *mergecfg.Config
	cfgErr string
	loaded bool
}

// shouldAbortPass 判断错误是否要中止本轮处理（ctx 结束、仓库级的限流、401/403 鉴权失败）：这些错误下继续请求平台没有意义，
// 整轮交给 outbox 稍后重试。单个 PR 的临时错误（读取失败、5xx）只影响这个 PR，其他目标分支照常推进。
func shouldAbortPass(ctx context.Context, err error) bool {
	return ctx.Err() != nil || forge.RateLimited(err) || forge.IsAuthError(err)
}

func (p *pass) run(ctx context.Context) error {
	entries, err := Queued(ctx, p.conn, p.repo.ID)
	if err != nil || len(entries) == 0 {
		return err
	}
	groups, perr := p.prepare(ctx, entries)
	if groups == nil {
		// 没有可以推进的目标分支（全部失败或者本轮中止）：错误交给 outbox 稍后重试
		return perr
	}
	// 逐条目标分支公平推进（PLAN #28.5）：一条分支的队首在等 CI、等更新完成或者配置错误时，其他分支照常推进。普通的
	// 错误记下后继续推进健康的分支；ctx 结束或者仓库级的限流、401/403 时中止本轮，交给 outbox 稍后重试。
	var errs []error
	// scheduled 记录本轮已排入（或已经处理过）的目标分支；推进期间改投到新分支时，只有还没排入的才在本轮接着处理，
	// 已经处理过的留给下一次通知。新分支按自己的 id 顺序排队（queuedByBase），绝不越过它已有的队首。
	scheduled := make(map[string]bool, len(groups))
	for _, b := range groups {
		scheduled[b] = true
	}
	for i := 0; i < len(groups); i++ {
		base := groups[i]
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		to, err := p.advanceGroup(ctx, base)
		if err != nil {
			if shouldAbortPass(ctx, err) {
				return errors.Join(append(errs, err)...)
			}
			errs = append(errs, err)
		}
		for _, b := range to {
			if scheduled[b] {
				// 新分支本轮已经处理过（或已经排入后面的位置）：写一条通知让下一次处理接着排它，避免等兜底检查
				if err := mqevents.KickMergeQueue(ctx, p.s.outbox, p.conn, p.repo.ID, 0); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			scheduled[b] = true
			groups = append(groups, b)
		}
	}
	return errors.Join(append(errs, perr)...)
}

// queuedByBase 返回一条目标分支上排队的项，按入队顺序（第一个是这条分支的队首）。
func queuedByBase(ctx context.Context, q store.Querier, repoID int64, base string) ([]*Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryCols+` FROM merge_queue WHERE repo_id=$1 AND state='queued' AND base_ref=$2 ORDER BY id`,
		repoID, base)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// prepare 处理每一项（PLAN #28.5）：先做与配置无关的收尾和清理（已合并、已关闭、去掉 label、推送了新 commit 等），
// 同步观测到的目标分支，处理 action=updating 期间改投目标分支的项；再读一次配置，配置错误时全部等待（绝不按分支
// 出队、也不发新的请求），不属于允许的目标分支的立即出队。返回要逐条推进的目标分支（按各组最早的入队顺序）和需要稍后
// 重试的单 PR 错误。
//
// 单个 PR 的临时错误（读取 PR、读取 commit 失败）不拖垮其他目标分支：记进返回的错误里，健康的分支照常推进；已知目标
// 分支的失败项还会让这条分支本轮不推进，排在它后面的 PR 不能越过队首。目标分支未知的旧行（base_ref 为空，升级前的
// 数据）失败时无法归属到哪条分支，保守地结束本轮准备（后续项绝不越过它）。ctx 结束、仓库级限流或者 401/403 时中止
// 本轮（返回 nil 分组和错误）。不属于允许目标分支的项要出队，单个 PR 的出队失败同样只影响它所在的分支。
func (p *pass) prepare(ctx context.Context, entries []*Entry) ([]string, error) {
	type aliveEntry struct {
		e    *Entry
		pull *forge.Pull
	}
	alive := make([]aliveEntry, 0, len(entries))
	var errs []error
	blocked := map[string]bool{}
	// unknownBlocked 表示遇到了目标分支未知（升级前的旧行，base_ref 为空）的失败项：无法判断它属于哪条目标分支，
	// 后面入队的项绝不能越过它推进（否则旧行永远拿不到合并，后来的 PR 却能直接合入同一条分支）。
	unknownBlocked := false
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, errors.Join(append(errs, ctx.Err())...)
		}
		if unknownBlocked {
			break
		}
		a, err := p.prepareEntry(ctx, e)
		if err != nil {
			if shouldAbortPass(ctx, err) {
				return nil, errors.Join(append(errs, err)...)
			}
			errs = append(errs, err)
			if e.BaseRef != "" {
				blocked[e.BaseRef] = true
			} else {
				unknownBlocked = true
			}
			continue
		}
		if a != nil {
			alive = append(alive, aliveEntry{e: a.e, pull: a.pull})
		}
	}
	if unknownBlocked {
		// 保守处理：目标分支未知的旧行失败时直接结束本轮准备并返回汇总错误，绝不让后续项越过它。
		return nil, errors.Join(errs...)
	}
	if len(alive) == 0 {
		return nil, errors.Join(errs...)
	}
	if err := p.loadConfig(ctx); err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	if p.cfgErr != "" {
		// 配置损坏：保持队列不动，只更新显示的状态（不检查允许的分支，也不因为正在更新就继续发请求）
		for _, a := range alive {
			if err := p.setStatus(ctx, a.e, configErrorStatus(p.cfgErr)); err != nil {
				return nil, errors.Join(append(errs, err)...)
			}
		}
		return nil, errors.Join(errs...)
	}
	allowed := p.cfg.AllowedBranches(p.repo.DefaultBranch)
	var groups []string
	seen := make(map[string]bool, len(allowed))
	for _, a := range alive {
		if !p.cfg.AllowsBranch(a.pull.BaseRef, p.repo.DefaultBranch) {
			reason := Note{Code: ReasonBranchNotAllowed, Params: NoteParams{Branch: a.pull.BaseRef, Allowed: allowed}}
			if _, err := p.apply(ctx, a.e, a.pull, removeWithComment(reason)); err != nil {
				// 单个 PR 的出队失败（例如去掉 label 时平台返回 500）只影响它所在的目标分支，不能拖垮其他健康的分支；
				// 仓库级的限流、401/403 仍中止整轮。
				if shouldAbortPass(ctx, err) {
					return nil, errors.Join(append(errs, err)...)
				}
				errs = append(errs, err)
				if a.pull.BaseRef != "" {
					blocked[a.pull.BaseRef] = true
				}
				continue
			}
			continue
		}
		if !seen[a.pull.BaseRef] {
			seen[a.pull.BaseRef] = true
			groups = append(groups, a.pull.BaseRef)
		}
	}
	// 失败的项所在的已知目标分支本轮不推进：后面的 PR 不能越过它（目标分支未知的旧行已在前面结束本轮准备）
	groups = slices.DeleteFunc(groups, func(base string) bool { return blocked[base] })
	for _, base := range groups {
		if err := p.markWaiting(ctx, base); err != nil {
			return nil, errors.Join(append(errs, err)...)
		}
	}
	return groups, errors.Join(errs...)
}

// prepared 是 prepareEntry 读到的项：需要继续按配置判断的 PR。
type prepared struct {
	e    *Entry
	pull *forge.Pull
}

// prepareEntry 处理一项中与配置无关的部分：decidePull 的决定照常执行（已合并、已关闭、去掉 label 等）；通过的项回填
// 观测到的目标分支，真实改投时原子清掉更新意图。返回要继续按配置判断的项；已经出队或结束时返回 nil。
func (p *pass) prepareEntry(ctx context.Context, e *Entry) (*prepared, error) {
	pull, err := p.f.GetPull(ctx, e.PR)
	if err != nil {
		return nil, fmt.Errorf("读取 PR #%d：%w", e.PR, err)
	}
	s := &snapshot{Entry: e, Pull: pull, DefaultBranch: p.repo.DefaultBranch, Now: p.s.now()}
	if pull.State == forge.PullOpen && e.HeadSHA != "" && pull.HeadSHA != e.HeadSHA && e.Action == actionUpdating {
		if s.Parents, err = p.f.CommitParents(ctx, pull.HeadSHA); err != nil {
			return nil, fmt.Errorf("读取 commit %s：%w", pull.HeadSHA, err)
		}
	}
	if d, ok := decidePull(s); ok {
		if d.Kind == actAcceptHead {
			// 入队时不知道 head，或者队列更新产生了新的 head：记录下来，这一次处理中接着按新的 head 判断
			if _, err := p.observeBase(ctx, e, pull.BaseRef); err != nil {
				return nil, err
			}
			if _, err := p.apply(ctx, e, pull, d); err != nil {
				return nil, err
			}
			return &prepared{e: e, pull: pull}, nil
		}
		if _, err := p.apply(ctx, e, pull, d); err != nil {
			return nil, err
		}
		return nil, nil
	}
	// 与配置无关的检查通过（开放的 PR）：回填目标分支；真实改投时原子清掉更新意图
	if _, err := p.observeBase(ctx, e, pull.BaseRef); err != nil {
		return nil, err
	}
	return &prepared{e: e, pull: pull}, nil
}

// markWaiting 更新一条目标分支上不在队首的项的显示状态（目标分支与排位）。
func (p *pass) markWaiting(ctx context.Context, base string) error {
	entries, err := queuedByBase(ctx, p.conn, p.repo.ID, base)
	if err != nil || len(entries) < 2 {
		return err
	}
	return p.markWaitingList(ctx, base, entries)
}

// markWaitingList 与 markWaiting 相同，用已经读到的排队项（entries 的第一个是队首）。
func (p *pass) markWaitingList(ctx context.Context, base string, entries []*Entry) error {
	for i, e := range entries[1:] {
		if err := p.setStatus(ctx, e, p.waitingStatus(base, i+2)); err != nil {
			return err
		}
	}
	return nil
}

// waitingStatus 是一条目标分支上非队首项的显示状态：配置了多个分支或者不是默认分支时显示目标分支，避免多分支时看不出来。
func (p *pass) waitingStatus(base string, pos int) Note {
	if len(p.cfg.AllowedBranches(p.repo.DefaultBranch)) > 1 || base != p.repo.DefaultBranch {
		return Note{Code: StatusPositionBranch, Params: NoteParams{Branch: base, Pos: pos}}
	}
	return Note{Code: StatusPosition, Params: NoteParams{Pos: pos}}
}

// observeBase 回填观测到的目标分支（PLAN #28.5），返回是否发生了真实的改投（不是升级前旧行的回填）。
//
// 升级前的旧行（base_ref 为空）只回填 base_ref，绝不动 action=updating 和 action_head。真实的改投在一条 SQL 中原子更新
// base_ref 并清掉 action_head / action_base；action=updating 时同时重置更新意图（清空 action_at、次数和状态）。这样无论是
// 更新期间改投还是 acceptHead 之后改投，都不会留下"旧分支的 action_head + 新分支的 base_ref"这种会让审计沿用跨分支的组合，
// 也没有两条 SQL 之间的中间窗口。
func (p *pass) observeBase(ctx context.Context, e *Entry, base string) (bool, error) {
	if base == "" || base == e.BaseRef {
		return false, nil
	}
	if e.BaseRef == "" {
		if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET base_ref=$2, updated_at=now() WHERE id=$1 AND state='queued'`, e.ID, base); err != nil {
			return false, err
		}
		e.BaseRef = base
		return false, nil
	}
	if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET base_ref=$2, action_head='', action_base='',
		action=CASE WHEN action=$3 THEN 'none' ELSE action END,
		action_at=CASE WHEN action=$3 THEN NULL ELSE action_at END,
		action_tries=CASE WHEN action=$3 THEN 0 ELSE action_tries END,
		status=CASE WHEN action=$3 THEN '' ELSE status END,
		status_code=CASE WHEN action=$3 THEN '' ELSE status_code END,
		status_params=CASE WHEN action=$3 THEN '{}' ELSE status_params END,
		updated_at=now() WHERE id=$1 AND state='queued'`, e.ID, base, actionUpdating); err != nil {
		return false, err
	}
	e.BaseRef, e.ActionHead, e.ActionBase = base, "", ""
	if e.Action == actionUpdating {
		e.Action, e.ActionAt, e.ActionTries, e.Status, e.StatusNote = actionNone, nil, 0, "", Note{}
	}
	return true, nil
}

// advanceGroup 推进一条目标分支的队首，直到需要等待或者这条分支的队列为空。每条分支有自己的 maxSteps 预算。返回推进期间
// 改投到的其他目标分支：这一项立即中止在当前组的评估，交给新分支按自己的入队顺序排队，绝不越过新分支已有的队首。
func (p *pass) advanceGroup(ctx context.Context, base string) ([]string, error) {
	var retargeted []string
	for range maxSteps {
		entries, err := queuedByBase(ctx, p.conn, p.repo.ID, base)
		if err != nil {
			return retargeted, err
		}
		if len(entries) == 0 {
			return retargeted, nil
		}
		if err := p.markWaitingList(ctx, base, entries); err != nil {
			return retargeted, err
		}
		to, wait, err := p.step(ctx, entries[0], base)
		if err != nil || wait {
			return retargeted, err
		}
		if to != "" && to != base && !slices.Contains(retargeted, to) {
			retargeted = append(retargeted, to)
		}
	}
	return retargeted, fmt.Errorf("合并队列中目标分支 %s 一次处理超过 %d 步", base, maxSteps)
}

// step 处理一条目标分支的队首：返回这一项改投到的目标分支（非空时已中止在当前组的评估）、是否需要等待。
func (p *pass) step(ctx context.Context, e *Entry, expectedBase string) (retargeted string, wait bool, err error) {
	s, to, err := p.snapshot(ctx, e, expectedBase)
	if err != nil || to != "" {
		return to, false, err
	}
	if s.BranchRemoval {
		return "", false, p.remove(ctx, e, s.Pull, removeWithComment(s.BranchReason))
	}
	wait, err = p.apply(ctx, e, s.Pull, decide(s))
	return "", wait, err
}

// snapshot 读取队首项的快照。只在需要时读取：PR 不能合并时不读验证结果和落后的 commit 数。
//
// expectedBase 是正在推进的目标分支。处理期间 PR 改投了别的分支时，先回填（原子清掉更新意图），返回改投到的分支并中止
// 这一项在当前组的评估：否则会按新分支的验证结果直接合并，越过新分支已有的队首（PLAN #28.5）。
func (p *pass) snapshot(ctx context.Context, e *Entry, expectedBase string) (*snapshot, string, error) {
	pull, err := p.f.GetPull(ctx, e.PR)
	if err != nil {
		return nil, "", fmt.Errorf("读取 PR #%d：%w", e.PR, err)
	}
	s := &snapshot{Entry: e, Pull: pull, DefaultBranch: p.repo.DefaultBranch, Now: p.s.now()}
	if pull.State == forge.PullOpen && e.HeadSHA != "" && pull.HeadSHA != e.HeadSHA && e.Action == actionUpdating {
		if s.Parents, err = p.f.CommitParents(ctx, pull.HeadSHA); err != nil {
			return nil, "", fmt.Errorf("读取 commit %s：%w", pull.HeadSHA, err)
		}
	}
	// decidePull 严格在回填目标分支之前：接受队列更新产生的新 head 必须按记录里的目标分支判断，先回填会把旧分支更新出来的
	// 合并提交误当成新分支的更新接受（PLAN #28.5）
	if _, ok := decidePull(s); ok {
		return s, "", nil
	}
	if pull.State == forge.PullOpen {
		retargeted, err := p.observeBase(ctx, e, pull.BaseRef)
		if err != nil {
			return nil, "", err
		}
		if retargeted && expectedBase != "" && pull.BaseRef != expectedBase {
			return nil, pull.BaseRef, nil
		}
	}
	if err := p.loadConfig(ctx); err != nil {
		return nil, "", err
	}
	s.ConfigErr = p.cfgErr
	if s.ConfigErr != "" {
		return s, "", nil
	}
	if !p.cfg.AllowsBranch(pull.BaseRef, p.repo.DefaultBranch) {
		s.BranchRemoval = true
		s.BranchReason = Note{Code: ReasonBranchNotAllowed, Params: NoteParams{
			Branch: pull.BaseRef, Allowed: p.cfg.AllowedBranches(p.repo.DefaultBranch),
		}}
		return s, "", nil
	}
	if e.Action == actionUpdating {
		// 配置有效、目标分支允许：只需按更新进度处理，不读验证结果
		return s, "", nil
	}
	// 这个 head 按目标分支规划完成才看验证结果（只有基线、只按其他目标分支规划过都不算，PLAN #28.3）
	if err := p.conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pr_plans WHERE repo_id=$1 AND pr_number=$2 AND sha=$3 AND base_ref=$4)`,
		p.repo.ID, e.PR, pull.HeadSHA, pull.BaseRef).Scan(&s.Processed); err != nil {
		return nil, "", err
	}
	s.Required = p.cfg.Requirements
	if err := p.loadRuns(ctx, s, e.PR, pull.HeadSHA); err != nil {
		return nil, "", err
	}
	if s.Processed {
		if s.Behind, err = p.f.Behind(ctx, pull.BaseRef, pull.HeadSHA); err != nil {
			return nil, "", fmt.Errorf("比较 %s 与 PR #%d：%w", pull.BaseRef, e.PR, err)
		}
	}
	return s, "", nil
}

// loadRuns 读取 head 针对目标分支的验证（CI 的 pull_request run、AI 审计请求，PLAN #30.3）；配置了必需的 job 时同时读取
// job。全部来源在同一个 REPEATABLE READ 事务中读取（同一个快照）：分开读时，中间提交的重跑会让旧一轮已结束的 run 配上
// 新一轮还没执行的 job，把正在重跑的 PR 当成验证没有成功而出队。
func (p *pass) loadRuns(ctx context.Context, s *snapshot, pr int, head string) error {
	withJobs := slices.ContainsFunc(s.Required, func(r mergecfg.Requirement) bool { return r.Job != "" })
	tx, err := p.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 只读事务，提交后回滚是空操作
	var all []store.Verification
	for _, src := range p.s.verifications {
		list, err := src(ctx, tx, p.repo.ID, pr, head, s.Pull.BaseRef, withJobs)
		if err != nil {
			return err
		}
		all = append(all, list...)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, v := range all {
		info := runInfo{ID: v.ID, Name: v.Name, State: store.RunState(v.State), Source: v.Source, Builtin: v.Builtin}
		for _, j := range v.Jobs {
			info.Jobs = append(info.Jobs, jobInfo{Name: j.Name, State: store.JobState(j.State)})
		}
		s.Runs = append(s.Runs, info)
	}
	return nil
}

// loadConfig 读取默认分支上的 .kiln/merge.yml（一次处理只读一次）。读取失败返回错误，配置有错误记在 cfgErr 中
// （cfg 保持默认值，避免在配置错误的分支上解引用）。
func (p *pass) loadConfig(ctx context.Context) error {
	if p.loaded {
		return nil
	}
	data, err := p.f.ReadFile(ctx, p.repo.DefaultBranch, mergecfg.Path)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", mergecfg.Path, err)
	}
	p.cfg, p.loaded = mergecfg.Default(), true
	if data != nil {
		cfg, err := mergecfg.Parse(data)
		if err != nil {
			p.cfgErr = err.Error()
			return nil //nolint:nilerr // 配置错误记在 cfgErr 中，由 prepare / decide 处理（不是读取失败，不需要重试）
		}
		p.cfg = cfg
	}
	return nil
}

// apply 执行决定，返回是否需要等待（本次处理结束）。
func (p *pass) apply(ctx context.Context, e *Entry, pull *forge.Pull, d decision) (bool, error) {
	switch d.Kind {
	case actWait:
		return true, p.setStatus(ctx, e, d.StatusNote)
	case actMerged:
		if e.Action == actionMerging && !mergedByHuman(pull) {
			// 上一次合并请求的结果未知（响应丢失、记录结果之前 Server 重启），PR 已经合并：按队列自己合并的处理。
			// 平台记录的合并者是人时不是：期间用户自己合并了，按在队列之外合并处理，自动打 tag
			// 由 webhook 按合并那一刻的 label 记录
			return false, p.finishMerged(ctx, e, pull, pull.MergeSHA, Note{})
		}
		return false, p.finishMerged(ctx, e, pull, pull.MergeSHA, Note{Code: ReasonMergedOutside})
	case actRemove:
		return false, p.remove(ctx, e, pull, d)
	case actAcceptHead:
		return false, p.acceptHead(ctx, e, d.Head)
	case actUpdate:
		return p.update(ctx, e, pull)
	case actMerge:
		return p.merge(ctx, e, pull)
	}
	return false, fmt.Errorf("未知的动作 %d", d.Kind)
}

// setStatus 更新显示的状态和原因码（没有变化时不写）。
func (p *pass) setStatus(ctx context.Context, e *Entry, n Note) error {
	status := shorten.Text(n.Text(), maxReasonLen)
	if status == e.Status && n.Code == e.StatusNote.Code {
		return nil
	}
	_, err := p.conn.Exec(ctx, `UPDATE merge_queue SET status=$2, status_code=$3, status_params=$4, updated_at=now()
		WHERE id=$1 AND state='queued'`, e.ID, status, n.Code, n.Params)
	return err
}

// acceptHead 记录新的 head。action_head 保留（上一次从哪个 head 更新），规划新 head 的审计时据此沿用旧 head 的审计
// （synchronize 事件可能在这之后才处理）。
func (p *pass) acceptHead(ctx context.Context, e *Entry, head string) error {
	_, err := p.conn.Exec(ctx, `UPDATE merge_queue SET head_sha=$2, action='none', action_at=NULL, action_tries=0,
		status='', status_code='', status_params='{}', updated_at=now() WHERE id=$1 AND state='queued'`, e.ID, head)
	return err
}

// remove 出队：按决定去掉 label、在 PR 上评论，然后记录原因。平台操作失败时返回错误（稍后重试，还没有出队）。
// 自动出队先记下原因（action=removing）：去掉 label 后评论失败时，重试按记下的原因继续，不会当成手动去掉了 label。
func (p *pass) remove(ctx context.Context, e *Entry, pull *forge.Pull, d decision) error {
	reason := shorten.Text(d.Reason, maxReasonLen)
	if d.Comment && (e.Action != actionRemoving || e.Reason != reason || e.ReasonNote.Code != d.ReasonNote.Code) {
		st := Note{Code: StatusRemoving}
		if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET action='removing', reason=$2, reason_code=$5, reason_params=$6, removal=$3,
			conflict_base=$4, status=$7, status_code=$8, status_params=$9, updated_at=now() WHERE id=$1 AND state='queued'`,
			e.ID, reason, d.Removal, d.ConflictBase, d.ReasonNote.Code, d.ReasonNote.Params, st.Text(), st.Code, st.Params); err != nil {
			return err
		}
	}
	if d.DropLabel {
		if err := p.f.RemoveLabel(ctx, e.PR, Label); err != nil {
			return fmt.Errorf("去掉 PR #%d 的 %s：%w", e.PR, Label, err)
		}
	}
	if d.Comment && pull.State == forge.PullOpen && e.CommentState != commentAttempted {
		body := p.removalComment(d.Reason)
		if d.Removal == RemovalConflictDev {
			body = p.handBackComment(d.Reason)
		}
		if err := p.comment(ctx, e, body); err != nil {
			return err
		}
	}
	return p.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE merge_queue SET state='removed', reason=$2, reason_code=$5, reason_params=$6, removal=$3,
			conflict_base=$4, status='', status_code='', status_params='{}', action='none', finished_at=now(), updated_at=now()
			WHERE id=$1 AND state='queued'`, e.ID, reason, d.Removal, d.ConflictBase, d.ReasonNote.Code, d.ReasonNote.Params)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		// 交回 AI 开发与出队在同一个事务中（PLAN #28.3，I15）：要么都生效，要么都不生效
		if d.Removal == RemovalConflictDev {
			if err := p.handBack(ctx, tx, e, pull, d); err != nil {
				return err
			}
		}
		// 因为推送了新的 commit 出队：新 head 的提名可能在旧 head 还排队时到达、被丢掉了（验证先于这次处理完成），
		// 这里重新提名，由自动入队检查新 head（PLAN #29.5）。没有开启 auto 时检查只读配置、丢掉候选
		if pull != nil && pull.State == forge.PullOpen && pull.HeadSHA != e.HeadSHA {
			if err := mqevents.NominateAutoMerge(ctx, p.s.outbox, tx, e.RepoID, e.PR); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, auditActor, "mergequeue.removed", map[string]any{"repo_id": e.RepoID, "pr": e.PR, "reason": reason})
	})
}

// commentAttempted 是 merge_queue.comment_state 的取值：出队评论已经尝试过。
const commentAttempted = "attempted"

// comment 在 PR 上发表出队评论，最多尝试一次（PLAN C19、#29.8）：调用前记下 comment_state=attempted；平台明确没有接受
// （4xx、限流）时清掉标记、返回错误（稍后重试）；结果不明（响应丢失：网络错误、5xx）时不返回错误也不再评论——
// 评论可能已经发出，重试会重复。
func (p *pass) comment(ctx context.Context, e *Entry, body string) error {
	if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET comment_state=$2, updated_at=now() WHERE id=$1`, e.ID, commentAttempted); err != nil {
		return err
	}
	e.CommentState = commentAttempted
	err := p.f.Comment(ctx, e.PR, body)
	switch {
	case err == nil:
		return nil
	case forge.Uncertain(err):
		p.s.log.Warn("合并队列：出队评论的结果不明，不再重复评论", "repo", p.repo.FullName(), "pr", e.PR, "err", err)
		return nil
	}
	if _, uerr := p.conn.Exec(ctx, `UPDATE merge_queue SET comment_state='', updated_at=now() WHERE id=$1`, e.ID); uerr != nil {
		err = errors.Join(err, uerr)
	}
	e.CommentState = ""
	return fmt.Errorf("在 PR #%d 上评论：%w", e.PR, err)
}

// removalComment 返回出队时在 PR 上的评论。
func (p *pass) removalComment(reason string) string {
	body := fmt.Sprintf("**Kiln 合并队列**：已将这个 PR 移出队列。\n\n原因：%s\n\n处理后重新加上 `%s` label 即可再次入队。", reason, Label)
	if p.s.publicURL != "" {
		body += fmt.Sprintf("（[合并队列](%s/merge-queue?repo=%s)）", strings.TrimSuffix(p.s.publicURL, "/"), url.QueryEscape(p.repo.FullName()))
	}
	return body
}

// handBackComment 返回冲突交回 AI 开发时在 PR 上的评论：不需要人重新加 label，新的 commit 验证通过后按合并策略重新入队。
func (p *pass) handBackComment(reason string) string {
	body := fmt.Sprintf("**Kiln 合并队列**：已将这个 PR 移出队列。\n\n原因：%s\n\n"+
		"AI 开发会把目标分支合并进来、解决冲突并更新这个 PR；新的 commit 经过 CI 和 AI 审计后按合并策略重新入队，不需要手动操作。", reason)
	if p.s.publicURL != "" {
		body += fmt.Sprintf("（[合并队列](%s/merge-queue?repo=%s)）", strings.TrimSuffix(p.s.publicURL, "/"), url.QueryEscape(p.repo.FullName()))
	}
	return body
}

// handBack 在出队的事务 tx 中把冲突交回 AI 开发的工作项（Deps.DevConflicts）。工作项期间已经结束、换了 PR，或者 PR 的
// 目标分支不是工作项的目标分支（记下意图之后改投的）时不交回：出队改记为等待人处理（评论已经发出，说明以队列记录和
// Dashboard 为准）。
func (p *pass) handBack(ctx context.Context, tx pgx.Tx, e *Entry, pull *forge.Pull, d decision) error {
	ok := false
	if p.s.devConflicts != nil && pull != nil {
		var err error
		if ok, err = p.s.devConflicts.HandBack(ctx, tx, DevConflict{
			RepoID: e.RepoID, PR: e.PR, HeadRef: pull.HeadRef, Head: e.HeadSHA, BaseRef: pull.BaseRef, Base: d.ConflictBase,
		}); err != nil {
			return err
		}
	}
	if ok {
		return nil
	}
	p.s.log.Warn("合并队列：负责冲突 PR 的 AI 开发工作项已经结束或目标分支不同，改为等待人处理", "repo", p.repo.FullName(), "pr", e.PR)
	// 原因码沿用冲突的参数；升级前记下的旧原因没有原因码，只追加说明
	n := Note{}
	if d.ReasonNote.Code == ReasonConflictDev {
		n = Note{Code: ReasonConflictDevUnavailable, Params: d.ReasonNote.Params}
	}
	_, err := tx.Exec(ctx, `UPDATE merge_queue SET removal=$2, reason=$3, reason_code=$4, reason_params=$5 WHERE id=$1`, e.ID,
		RemovalConflictAuthor, shorten.Text(d.Reason+devUnavailableSuffix, maxReasonLen), n.Code, n.Params)
	return err
}

// finishMerged 记录 PR 已经合并。reason 说明不是由队列合并的（由队列合并时为零值）。由队列合并时同时标记为要关闭关联的
// issue（issues_state=pending，平台只在用户合并时关闭它们，见 closeissues.go）；在队列之外合并的由平台自己关闭。
//
// 由队列合并时同一事务中记下要自动打 tag 的 commit（PLAN #29.4，T4）：pull_request.closed 的发送者是 App 自己，
// webhook 那边不会记录。在队列之外合并的不在这里记：由 webhook 按合并那一刻的快照（label）记录，这里读到的是现在的
// label，合并后被整理过（例如去掉 release:skip）时会抢先写入错误的记录（(repo_id, sha) 唯一，webhook 的正确记录
// 被忽略）。pull 为 nil 时（只在测试中）不记录。
func (p *pass) finishMerged(ctx context.Context, e *Entry, pull *forge.Pull, mergeSHA string, why Note) error {
	reason := why.Text()
	issues, status := "", Note{}
	if reason == "" {
		issues, status = issuesPending, statusClosingIssues
	}
	return p.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE merge_queue SET state='merged', merge_sha=$2, reason=$3, reason_code=$6, reason_params=$7,
			status=$4, status_code=$8, status_params=$9, issues_state=$5, action='none', finished_at=now(), updated_at=now()
			WHERE id=$1 AND state='queued'`, e.ID, mergeSHA, reason, status.Text(), issues, why.Code, why.Params, status.Code, status.Params)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := p.recordAutoTag(ctx, tx, e, pull, mergeSHA, reason); err != nil {
			return err
		}
		if reason != "" {
			return nil
		}
		// 合并请求的结果未知、之后才看到 PR 已合并时这次处理没有读取配置
		method := ""
		if p.cfg != nil {
			method = p.cfg.Method
		}
		return store.Audit(ctx, tx, auditActor, "mergequeue.merged", map[string]any{
			"repo_id": e.RepoID, "pr": e.PR, "head": e.HeadSHA, "merge_sha": mergeSHA, "method": method,
		})
	})
}

// recordAutoTag 记下合并提交，由自动打 tag 的执行者按 .kiln/merge.yml 的 tag: 处理（没有配置时跳过）。任何目标分支的
// 合并都记下，是否发版由自动打 tag 按发版分支裁决（PLAN #29.4）：合入开发分支只跳过，不发布版本。发布的发送者是
// 把 PR 加入队列的人（自动入队时是 PR 作者）：由 App 合并时平台记录的合并者是 App 自己，用它规划会被当成机器人发起的
// 事件而跳过。由机器人（自动入队、Bot 加 label）入队时记下真实发起者和 sender_is_bot，交给自动打 tag 统一裁决：
// Kiln 自己的 Bot 合并 AI 开发的实现 PR 可以发布，第三方 Bot 的 PR 仍然不发布（PLAN #29.4）。
func (p *pass) recordAutoTag(ctx context.Context, tx pgx.Tx, e *Entry, pull *forge.Pull, mergeSHA, reason string) error {
	// 在队列之外合并时，PR 可能刚改了目标分支（执行者还没处理 edited 事件），以合并请求读到的 PR 为准记下它的目标分支
	if reason != "" || pull == nil || mergeSHA == "" || pull.Fork {
		return nil
	}
	sender, senderIsBot := releaseSender(e.EnqueuedBy, pull)
	// 用请求合并时记下的 label（响应丢失后重新读到的可能已经被整理过）
	labels := pull.Labels
	if e.MergeLabels != nil {
		labels = e.MergeLabels
	}
	added, err := mqevents.RecordAutoTag(ctx, tx, mqevents.AutoTagRecord{
		RepoID: e.RepoID, SHA: mergeSHA, PR: e.PR, Labels: labels, MergedBy: sender, SenderIsBot: senderIsBot,
		HeadRef: pull.HeadRef, BaseRef: pull.BaseRef, DefaultBranchAtMerge: p.repo.DefaultBranch, Source: mqevents.AutoTagFromQueue,
	})
	if err != nil || !added {
		return err
	}
	return mqevents.EnqueueAutoTag(ctx, p.s.outbox, tx, e.RepoID)
}

// releaseSender 返回发布的真实发起者和它是否是机器人：人给 PR 加了 kiln:merge（github:<login>）时是那个人；自动入队、
// 由 App 自己加 label 入队时是 PR 的作者，作者是机器人或未知时照实记下，不静默跳过（判定的规则在自动打 tag 中）。
func releaseSender(enqueuedBy string, pull *forge.Pull) (sender string, isBot bool) {
	if by, ok := strings.CutPrefix(enqueuedBy, "github:"); ok && by != "" && !strings.HasSuffix(by, "[bot]") {
		return by, false
	}
	return pull.Author, pull.Author == "" || pull.AuthorBot || strings.HasSuffix(pull.Author, "[bot]")
}

// OwnsMerge 判断仓库 repoID 的 PR pr 的合并（合并 commit 为 sha）是否正由合并队列处理、或者已经由合并队列完成。事件接入
// 据此避免用未冻结的 webhook label 抢先写入自动打 tag 的记录（PLAN #29.4）：由队列合并的在同一事务中已经按请求合并时的
// label 记下。
func OwnsMerge(ctx context.Context, q store.Querier, repoID int64, pr int, sha string) (bool, error) {
	var own bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merge_queue WHERE repo_id=$1 AND pr_number=$2
		AND ((state='queued' AND action='merging') OR (state='merged' AND merge_sha=$3 AND reason='')))`, repoID, pr, sha).Scan(&own)
	return own, err
}

// update 请求更新 PR 分支：先记录请求（中途崩溃后下一次处理会等待而不是重复请求），再调用平台。记录更新意图时一并
// 写下这次更新的目标分支（action_base 和展示用的 base_ref）：期间 PR 改投别的分支时，接受新 head 和审计沿用
// （UpdatedFrom）都不能跨分支。
func (p *pass) update(ctx context.Context, e *Entry, pull *forge.Pull) (bool, error) {
	// action_at 与判断超时用同一个时钟（s.now）
	st := Note{Code: StatusUpdating}
	if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET action='updating', action_head=$2, action_base=$4, action_at=$3,
		action_tries=action_tries+1, base_ref=$4, status=$5, status_code=$6, status_params=$7, updated_at=now()
		WHERE id=$1 AND state='queued'`,
		e.ID, e.HeadSHA, p.s.now(), pull.BaseRef, st.Text(), st.Code, st.Params); err != nil {
		return false, err
	}
	e.BaseRef, e.ActionBase = pull.BaseRef, pull.BaseRef
	err := p.f.UpdateBranch(ctx, e.PR, e.HeadSHA)
	switch {
	case err == nil:
		p.s.log.Info("合并队列：更新 PR 分支", "repo", p.repo.FullName(), "pr", e.PR, "head", e.HeadSHA)
		return true, nil
	case errors.Is(err, forge.ErrHeadChanged):
		// head 已经变了（可能是上一次请求已经完成）：下一轮按新的 head 判断
		return false, nil
	case errors.Is(err, forge.ErrMergeConflict):
		return p.conflict(ctx, e, pull.BaseRef, err)
	case forge.IsRejected(err):
		// 没有权限、其他原因被拒：出队并评论原因，不交给 AI 开发（不是冲突）
		return false, p.remove(ctx, e, pull, removeWithComment(note(ReasonUpdateRejected, NoteParams{Detail: err.Error()})))
	}
	// 暂时失败：结果未知（平台可能已经接受了请求，只是响应丢失），保留更新意图和 action_head，清空请求时间并且不计这次请求。
	// 下一次处理时新 head 到了就按队列更新接受，没到就立即重新请求（decide）。
	return false, p.retryUpdate(ctx, e, note(StatusUpdateRetry, NoteParams{Detail: err.Error()}), false, err)
}

// retryUpdate 保留更新意图、清空请求时间（下一次处理立即重新请求，decide），显示状态 status，返回 cause（稍后重试）。
// counted 为 false 时不计这次请求（结果不明）；为 true 时计入，用完 maxUpdateTries 次出队（decide）。
func (p *pass) retryUpdate(ctx context.Context, e *Entry, status Note, counted bool, cause error) error {
	tries := `greatest(action_tries-1, 0)`
	if counted {
		tries = `action_tries`
	}
	err := cause
	if _, uerr := p.conn.Exec(ctx, `UPDATE merge_queue SET action_at=NULL, action_tries=`+tries+`, status=$2, status_code=$3,
		status_params=$4, updated_at=now() WHERE id=$1 AND state='queued'`,
		e.ID, shorten.Text(status.Text(), maxReasonLen), status.Code, status.Params); uerr != nil {
		err = errors.Join(err, uerr)
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("更新 PR #%d 的分支：%w", e.PR, err)
}

// conflict 处理平台对更新分支回答合并冲突（PLAN #28.3）：再读一次 PR，平台的 mergeability 也确认冲突（mergeable=false、
// mergeable_state=dirty）才算冲突（I17），按 PR 的归属处理——AI 开发的工作项负责、PR 的目标分支仍是工作项的目标分支的
// 出队并交回返修，其余的出队、请提交者（或人）解决。mergeability 还没有计算完成（或者与 422 矛盾：期间目标分支又前进了）时不算冲突：保留更新意图稍后重新请求，计入
// 次数，用完按超时出队（不交给 AI 开发）。
//
// expectedBase 是请求更新时的目标分支（pull.BaseRef）。期间 PR 改投了别的分支时，这次冲突不再属于这条分支：不归因、
// 不交回，直接重走调度（PLAN #28.5）。
func (p *pass) conflict(ctx context.Context, e *Entry, expectedBase string, cause error) (bool, error) {
	cur, err := p.f.GetPull(ctx, e.PR)
	if err != nil {
		return false, p.retryUpdate(ctx, e, note(StatusConflictReadFailed, NoteParams{Detail: err.Error()}), false, err)
	}
	if cur.State != forge.PullOpen || cur.HeadSHA != e.HeadSHA || cur.BaseRef != expectedBase ||
		!p.cfg.AllowsBranch(cur.BaseRef, p.repo.DefaultBranch) {
		// 期间 PR 关闭、合并、推送了新的 commit、改了目标分支或者目标分支不再允许合并：下一轮按最新的状态判断
		return false, nil
	}
	if !cur.Conflicted() {
		state := cmp.Or(cur.MergeableState, "unknown")
		return false, p.retryUpdate(ctx, e, Note{Code: StatusMergeability, Params: NoteParams{State: state}},
			true, fmt.Errorf("%w（mergeable_state=%s）", cause, state))
	}
	base, err := p.f.BranchHead(ctx, cur.BaseRef)
	if err != nil {
		return false, p.retryUpdate(ctx, e, note(StatusBaseReadFailed, NoteParams{Detail: err.Error()}), false, err)
	}
	owner := DevOwner{}
	if p.s.devConflicts != nil {
		if owner, err = p.s.devConflicts.Owner(ctx, p.conn, p.repo.ID, e.PR, cur.HeadRef); err != nil {
			return false, err
		}
	}
	params := NoteParams{Branch: cur.BaseRef, SHA: base}
	code, removal := ReasonConflictAuthor, RemovalConflictAuthor
	switch owner.Kind {
	case DevOwnerRecording:
		// AI 开发刚建了这个 PR、还没记下编号：等它记下（记下时通知合并队列），不出队、不评论
		params.WorkItem = owner.WorkItem
		return true, p.retryUpdate(ctx, e, Note{Code: StatusConflictRecording, Params: params}, false, nil)
	case DevOwnerActive:
		params.WorkItem = owner.WorkItem
		if owner.BaseRef != cur.BaseRef {
			// PR 改投了别的目标分支（例如默认分支改了名）：工作项的目标分支、交付核对和返修输入都按它开始时的目标分支，
			// 交回会让 AI 开发合并旧的目标分支，解决不了这个冲突。不自动返修，请人处理
			code, params.From = ReasonConflictRetargeted, owner.BaseRef
			break
		}
		code, removal = ReasonConflictDev, RemovalConflictDev
	case DevOwnerEnded:
		code, params.WorkItem, params.State = ReasonConflictEnded, owner.WorkItem, owner.State
	}
	d := removeWithComment(Note{Code: code, Params: params})
	d.Removal, d.ConflictBase = removal, base
	p.s.log.Info("合并队列：PR 与目标分支冲突", "repo", p.repo.FullName(), "pr", e.PR, "head", e.HeadSHA, "base", base, "removal", d.Removal)
	return false, p.remove(ctx, e, cur, d)
}

// merge 合并 PR（带上期望的 head，不会合入没有验证过的 commit）。先记下合并意图（action=merging）：平台已经合并、
// 但响应丢失或记录结果之前 Server 重启时，下一次处理看到 PR 已合并仍按队列自己合并的处理（关闭关联的 issue）。
//
// 同时冻结合并时应关闭的 issue（当时打开的关联 issue，PLAN #29.8）：之后被人重新打开的不会被关闭。读取失败（例如 App
// 没有 Issues 权限）不阻止合并，合并后第一次处理时再冻结。
func (p *pass) merge(ctx context.Context, e *Entry, pull *forge.Pull) (bool, error) {
	// 读取失败时写入 SQL NULL（不能是 JSON 的 null：那样会被当成已经冻结、没有 issue）
	var frozen any
	if issues, err := p.f.ClosingIssues(ctx, e.PR); err == nil {
		m := map[string]string{}
		for _, is := range issues {
			if is.Open {
				m[strconv.Itoa(is.Number)] = issuePending
			}
		}
		frozen = m
	} else {
		p.s.log.Info("合并队列：合并前读取关联的 issue 失败，合并后再读取", "repo", p.repo.FullName(), "pr", e.PR, "err", err)
	}
	// 同时记下合并这一刻的 label（自动打 tag 用，PLAN #29.4）：响应丢失后重新读到的 label 可能已经被人整理过
	labels := []string{}
	if pull != nil { // 只在测试中为 nil
		labels = append(labels, pull.Labels...)
	}
	st := Note{Code: StatusMerging}
	if _, err := p.conn.Exec(ctx, `UPDATE merge_queue SET action=$2, issues=$3, merge_labels=$4, status=$5, status_code=$6,
		status_params=$7, updated_at=now() WHERE id=$1 AND state='queued'`,
		e.ID, actionMerging, frozen, labels, st.Text(), st.Code, st.Params); err != nil {
		return false, err
	}
	e.MergeLabels = labels
	sha, err := p.f.Merge(ctx, e.PR, p.cfg.Method, e.HeadSHA)
	switch {
	case err == nil:
		p.s.log.Info("合并队列：合并 PR", "repo", p.repo.FullName(), "pr", e.PR, "head", e.HeadSHA, "merge_sha", sha)
		return false, p.finishMerged(ctx, e, pull, sha, Note{})
	case errors.Is(err, forge.ErrHeadChanged):
		// 平台确认没有合并：清掉合并意图，之后在队列之外合并的不会被当成队列合并的
		_, err := p.conn.Exec(ctx, `UPDATE merge_queue SET action='none', merge_labels=NULL, updated_at=now()
			WHERE id=$1 AND state='queued' AND action=$2`, e.ID, actionMerging)
		return false, err
	case forge.IsRejected(err):
		return false, p.remove(ctx, e, pull, removeWithComment(note(ReasonMergeRejected, NoteParams{Detail: err.Error()})))
	case !forge.Uncertain(err):
		// 平台明确没有执行（限流、其他 4xx）：清掉合并意图和记下的 label，之后用户自己合并的不会被当成队列合并的；稍后重试
		if _, uerr := p.conn.Exec(ctx, `UPDATE merge_queue SET action='none', merge_labels=NULL, updated_at=now()
			WHERE id=$1 AND state='queued' AND action=$2`, e.ID, actionMerging); uerr != nil {
			err = errors.Join(err, uerr)
		}
	}
	return false, fmt.Errorf("合并 PR #%d：%w", e.PR, err)
}

// mergedByHuman 判断平台记录的合并者是人（不是机器人）：队列自己合并时是 App 的 <slug>[bot]。平台给出的账号类型
// （MergedByBot）和 login 的 [bot] 后缀任一表示机器人：外部机器人的 login 不一定带后缀，只看后缀会在队列处于
// action=merging 时把外部机器人的合并当成人工合并，队列和 webhook 两边都不写 auto_tags。
func mergedByHuman(pull *forge.Pull) bool {
	return pull != nil && pull.MergedBy != "" && !pull.MergedByBot && !strings.HasSuffix(pull.MergedBy, "[bot]")
}

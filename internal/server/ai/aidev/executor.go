package aidev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 仓库执行者（C19，PLAN #26.11）：每个仓库一个（advisory lock），状态全部在 Postgres，每次按最新状态重新判断，
// 只做幂等或带条件的写操作。一次处理：
//
//  1. 核对执行：开发、返修中的工作项，它的任务已经结束却没有推进它时转为受阻；
//  2. 推进交付（delivering）：核对 attempt 分支、相对目标分支有改动（设计阶段只有 Markdown）、正式分支只快进，按本轮的
//     head 分支找或建 PR，转为 pr_open；
//  3. 判断返修（pr_open）：合并队列交回了冲突就开始冲突返修（conflict.go）；否则 PR head 的 CI 和 AI 审计全部结束后，有审计
//     问题或非基础设施的 CI 失败就返修（轮数用完转为受阻）；
//  4. 同步 Issue 的状态 label、发送评论（至多一次）、删除过期的 attempt 分支、复查等待的依赖（PLAN #31.6）；
//
// 释放锁之后规划返修和实现、在有空闲的开发机时规划选题（规划从连接池借连接，持有锁的连接不归还时连接池上限为 1 会一直
// 等待）。
// 仓库按工作项串行（I14，serial.go）：正常时至多一个未结束的工作项；升级前留下的多个工作项逐个推进（已有 PR 的验证、
// 返修、合并不受限制，只有占用者开始新的阶段或轮次）。

// sensitivePaths 返回改动后 PR 要加 kiln:hold（不自动入队，#29.5）的路径（以 / 结尾的是目录）：CI / Kiln 的配置和给 AI 的说明
// （PLAN #26.11）。
func sensitivePaths() []string {
	return []string{".kiln/", ".github/", "AGENTS.md", "CLAUDE.md", ".claude/", ".codex/"}
}

// Process 处理仓库 repoID 的 AI 开发。返回错误表示需要稍后重试。可以重复调用。
func (s *Service) Process(ctx context.Context, repoID int64) error {
	if s.forge == nil {
		return nil
	}
	var repo *store.Repo
	err := s.withRepoLock(ctx, repoID, 0, func(conn *pgxpool.Conn) error {
		var err error
		if repo, err = store.GetRepo(ctx, conn, repoID); err != nil || !repo.Active {
			return err
		}
		p := &pass{s: s, conn: conn, repo: repo, f: s.forge(repo)}
		return p.run(ctx)
	})
	if err != nil || repo == nil || !repo.Active {
		return err
	}
	s.notify()
	return errors.Join(s.planFixes(ctx, repo), s.planImpls(ctx, repo), s.planNext(ctx, repo))
}

// planNext 在条件满足时规划下一轮选题（PLAN #26.11 调度）；不满足（没有空闲的开发机、正在选题、任务池没有变化等）时什么也不做。
func (s *Service) planNext(ctx context.Context, repo *store.Repo) error {
	res, err := s.PlanSelect(ctx, repo.ID, auditActor)
	if err != nil {
		return fmt.Errorf("规划选题：%w", err)
	}
	if res.RequestID == 0 && res.Reason != whyNoCandidates && res.Reason != whyExamined {
		s.log.Debug("AI 开发：没有开始选题", "repo", repo.FullName(), "reason", res.Reason)
	}
	return nil
}

// pass 是持有仓库锁的一次处理。
type pass struct {
	s    *Service
	conn *pgxpool.Conn
	repo *store.Repo
	f    forge.Repo
}

func (p *pass) run(ctx context.Context) error {
	// 先核验待核验的 PR 创建归属：即使当前没有未结束的工作项也要处理（PR 已经合并、工作项可能已经结束），
	// 处理时同样在仓库锁下、用持有锁的连接。
	checkErr := p.processOriginChecks(ctx)
	list, err := queryWorkItems(ctx, p.conn, `repo_id=$1 AND active`, p.repo.ID)
	if err != nil {
		return errors.Join(checkErr, err)
	}
	var errs []error
	for _, w := range list {
		if err := p.step(ctx, w); err != nil {
			errs = append(errs, fmt.Errorf("工作项 #%d：%w", w.ID, err))
		}
	}
	errs = append(errs, checkErr, p.syncLabels(ctx), p.s.sendComments(ctx, p.conn, p.f, p.repo.ID), p.gcBranches(ctx), p.recheckWaits(ctx))
	return errors.Join(errs...)
}

// processOriginChecks 核验 PR 创建归属还没有确认的 PR：创建 PR 的响应丢失、合并事件先到时，合并事件按正式
// 分支找到工作项并冻结一条待核验记录；这里经平台读取这个 PR 核验，通过后补记永久归属。网络、暂时失败时保留记录、返回
// 错误稍后重试；平台明确没有这个 PR 或者核验不通过时删除记录、不写永久归属（绝不按分支直接授权）。
func (p *pass) processOriginChecks(ctx context.Context) error {
	checks, err := queryPullOriginChecks(ctx, p.conn, p.repo.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range checks {
		if err := p.checkOrigin(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("核验 PR #%d 的 AI 开发归属：%w", c.PR, err))
		}
	}
	return errors.Join(errs...)
}

// checkOrigin 经平台核验一条待核验的 PR 创建归属：通过时在同一事务中补记 dev_pull_origins 并删除记录；核验不通过时只删除
// 记录。归属的身份列取冻结时的阶段、轮次、分支（工作项可能已经换了阶段、轮次）。
func (p *pass) checkOrigin(ctx context.Context, c pullOriginCheck) error {
	pull, err := p.f.GetPull(ctx, c.PR)
	if err != nil {
		if gh.IsNotFound(err) {
			return p.dropOriginCheck(ctx, c, "平台上没有这个 PR")
		}
		return err
	}
	verified, err := verifiedPullRefs(pull, c.HeadRef, c.BaseRef, p.s.checkSelf)
	if err != nil {
		// App 身份还没加载出来时保留待核验记录，稍后重试：绝不能把“还不知道作者”当成核验不通过、删掉合法的证据
		return err
	}
	if !verified {
		return p.dropOriginCheck(ctx, c, fmt.Sprintf("PR 的作者是 %q，来源分支 %q、目标分支 %q 与记录不符或者是 fork",
			pull.Author, pull.HeadRef, pull.BaseRef))
	}
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		if err := recordPullOrigin(ctx, tx, pullOrigin{
			RepoID: c.RepoID, PR: c.PR, WorkItemID: c.WorkItemID, Phase: c.Phase, ImplRound: c.ImplRound, HeadRef: c.HeadRef,
			BaseRef: c.BaseRef, CreatorLogin: pull.Author, DeliveryTaskID: c.DeliveryTaskID, DeliverySHA: c.DeliverySHA,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, c.RepoID, c.PR); err != nil {
			return err
		}
		p.s.log.Info("AI 开发：已核验并补记 PR 的归属", "repo", p.repo.FullName(), "work_item", c.WorkItemID, "pr", c.PR, "creator", pull.Author)
		return store.Audit(ctx, tx, auditActor, "aidev.origin_recorded", map[string]any{
			"repo_id": c.RepoID, "work_item": c.WorkItemID, "pr": c.PR, "phase": c.Phase, "impl_round": c.ImplRound,
			"head_ref": c.HeadRef, "base_ref": c.BaseRef, "creator": pull.Author,
		})
	})
}

// dropOriginCheck 删除一条核验不通过的待核验记录并写审计日志（绝不写 dev_pull_origins）。
func (p *pass) dropOriginCheck(ctx context.Context, c pullOriginCheck, why string) error {
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM dev_pull_origin_checks WHERE repo_id=$1 AND pr_number=$2`, c.RepoID, c.PR); err != nil {
			return err
		}
		p.s.log.Warn("AI 开发：PR 的创建归属核验不通过，不记为可信来源", "repo", p.repo.FullName(), "work_item", c.WorkItemID, "pr", c.PR, "reason", why)
		return store.Audit(ctx, tx, auditActor, "aidev.origin_rejected", map[string]any{
			"repo_id": c.RepoID, "work_item": c.WorkItemID, "pr": c.PR, "phase": c.Phase, "impl_round": c.ImplRound, "reason": why,
		})
	})
}

// step 推进未结束的工作项（最多走到不能再前进的状态）。
func (p *pass) step(ctx context.Context, w *WorkItem) error {
	if err := p.reconcileTask(ctx, w); err != nil {
		return err
	}
	if w.State == StateDelivering {
		if err := p.promote(ctx, w); err != nil {
			return err
		}
	}
	if err := p.handleConflict(ctx, w); err != nil {
		return err
	}
	if w.State == StatePROpen {
		return p.evaluate(ctx, w)
	}
	return nil
}

// reconcileTask：开发、返修中的工作项，它的任务已经结束、结果却没有推进它（例如还没领取就被取消）时转为受阻。
func (p *pass) reconcileTask(ctx context.Context, w *WorkItem) error {
	if (w.State != StateDeveloping && w.State != StateFixing) || w.TaskID == nil {
		return nil
	}
	t, err := ai.GetTask(ctx, p.conn, *w.TaskID, false)
	if err != nil || !t.State.Terminal() {
		return err
	}
	why := string(t.State)
	if t.Reason != nil {
		why = *t.Reason
	}
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		_, err := p.s.transition(ctx, tx, w, EvExecFailed, "执行没有交付："+why, "")
		return err
	})
}

// reject 让交付核对不通过：工作项转为受阻（原因说明给人看）。
func (p *pass) reject(ctx context.Context, w *WorkItem, why string) error {
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		_, err := p.s.transition(ctx, tx, w, EvPromoteRejected, why, `delivery_branch='', delivery_sha=''`)
		return err
	})
}

// promote 推进交付（PLAN #26.11 推进、#26.12、#31.5）。每一步都可以重复：
//
//   - attempt 分支必须指向交付的 commit（Runner 推送、ls-remote 确认过）；分支已经不在时，正式分支已经是这个 commit
//     （上一次推进到一半）就继续，否则不通过；
//   - 交付相对目标分支要有改动；改动了敏感路径、声明要改已确认的决定时 PR 要加 kiln:hold；设计阶段只能改 Markdown
//     （I12），设计文档要在改动中，PR 加 release:skip；
//   - 正式分支不存在就创建，存在就只快进（I5）；不能快进说明有人推送过（分叉），不通过，交给人；
//   - PR：pr_number 为空时按 head 分支找打开的 PR，没有就创建（创建得到"已存在"时再找）；只在为空时写入（I6）；
//   - PR 描述按最新的交付报告更新；转为 pr_open，删除 attempt 分支。
func (p *pass) promote(ctx context.Context, w *WorkItem) error {
	if w.PRNumber == nil {
		if settled, err := p.settleUnrecorded(ctx, w); err != nil || settled {
			return err
		}
	}
	branch := w.Branch()
	formal, err := p.f.BranchHead(ctx, branch)
	if err != nil {
		return fmt.Errorf("读取正式分支 %s：%w", branch, err)
	}
	attempt, err := p.f.BranchHead(ctx, w.DeliveryBranch)
	if err != nil {
		return fmt.Errorf("读取 attempt 分支 %s：%w", w.DeliveryBranch, err)
	}
	if attempt != w.DeliverySHA && formal != w.DeliverySHA {
		got := attempt
		if got == "" {
			got = "不存在"
		}
		return p.reject(ctx, w, fmt.Sprintf("交付核对不通过：attempt 分支 %s 是 %s，应为 %s", w.DeliveryBranch, got, shorten.Hash(w.DeliverySHA)))
	}
	diff, err := p.f.Diff(ctx, w.BaseRef, w.DeliverySHA)
	if err != nil {
		return fmt.Errorf("比较 %s 与交付 %s：%w", w.BaseRef, shorten.Hash(w.DeliverySHA), err)
	}
	if diff.Ahead == 0 {
		return p.reject(ctx, w, fmt.Sprintf("交付核对不通过：交付 %s 相对目标分支 %s 没有新的提交", shorten.Hash(w.DeliverySHA), w.BaseRef))
	}
	rep, issues, err := p.latestDelivery(ctx, w)
	if err != nil {
		return err
	}
	if why := designViolation(w, diff, rep); why != "" {
		return p.reject(ctx, w, "交付核对不通过："+why)
	}
	holds := holdReasons(w, diff, rep, issues)
	hold := len(holds) > 0
	if hold {
		// 移动正式分支、建 PR 之前先记下：之后 PR 的验证可能在 kiln:hold 加上之前通过（加 label 失败、响应丢失时更久），
		// 合并队列按它不自动入队
		if _, err := p.conn.Exec(ctx, `UPDATE dev_work_items SET hold_pending=true WHERE id=$1 AND state=$2`, w.ID, w.State); err != nil {
			return err
		}
	}
	if why, err := p.advance(ctx, w, formal); err != nil || why != "" {
		if why != "" {
			return p.reject(ctx, w, why)
		}
		return err
	}
	pr, creator, verified, err := p.writePull(ctx, w, prTitle(w, rep, issues), prBody(w, rep, issues, p.s.holdLabel, holds), hold)
	if closed, ok := errors.AsType[*pullClosedError](err); ok {
		return p.closedBeforeRecorded(ctx, w, closed.pull)
	}
	if errors.Is(err, errPullNotOurs) {
		// 正式分支上有一个不是 Kiln 创建的 PR：不接管，避免把第三方或伪造的 PR 变成可信发布来源
		return p.reject(ctx, w, "交付核对不通过："+err.Error()+"，需要人处理")
	}
	if err != nil {
		return err
	}
	if err := p.recordPromoted(ctx, w, pr, creator, verified, hold, rep); err != nil {
		return err
	}
	if w.PRNumber == nil {
		w.PRNumber = &pr
	}
	// 与 recordPromoted 写入的相同：这次处理接着判断冲突和返修时按新的 head
	w.HeadSHA = w.DeliverySHA
	p.s.log.Info("AI 开发：已推进交付", "repo", p.repo.FullName(), "work_item", w.ID, "pr", pr, "sha", w.DeliverySHA, "hold", hold)
	return nil
}

// recordPromoted 在推进完成后记下：转为 pr_open（PR 编号、新的 head、hold），记下这次交付的完成度（合并时按它判断，
// PLAN #31.3），有处置时在 PR 上发处置汇总。verified 为真时在同一事务中记下 PR 的永久归属（PLAN #29.4）：新建的和
// Server 自己记下编号的 PR 是可信发布来源；平台上发现但核验不通过的 PR 不记。
func (p *pass) recordPromoted(ctx context.Context, w *WorkItem, pr int, creator string, verified, hold bool, rep *devreport.Delivery) error {
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		moved, err := p.s.transition(ctx, tx, w, EvPromoted, "PR 已开，等待 CI、AI 审计和合并",
			`pr_number=coalesce(pr_number, $9), head_sha=$10, hold_label=hold_label OR $11, hold_pending=false, delivery_branch='', delivery_sha='',
			head_task_id=delivery_task_id`,
			pr, w.DeliverySHA, hold)
		if err != nil || !moved {
			return err
		}
		if verified {
			if err := recordPullOrigin(ctx, tx, pullOrigin{
				RepoID: w.RepoID, PR: pr, WorkItemID: w.ID, Phase: w.Phase, ImplRound: w.ImplRound, HeadRef: w.Branch(),
				BaseRef: w.BaseRef, CreatorLogin: creator, DeliveryTaskID: w.DeliveryTaskID, DeliverySHA: w.DeliverySHA,
			}); err != nil {
				return err
			}
		}
		if f := p.s.pullRecorded; f != nil {
			if err := f(ctx, tx, w.RepoID, pr); err != nil {
				return err
			}
		}
		if rep != nil {
			if err := saveResolutions(ctx, tx, w.ID, rep); err != nil {
				return err
			}
		}
		if rep != nil && len(rep.Dispositions) > 0 {
			if err := addComment(ctx, tx, w.RepoID, w.ID, pr, commentDispositions, fmt.Sprintf("dispositions:%d:%d", w.ID, w.FixRounds),
				dispositionsComment(w, rep)); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, auditActor, "aidev.promoted", map[string]any{
			"repo_id": w.RepoID, "work_item": w.ID, "pr": pr, "sha": w.DeliverySHA, "hold": hold,
		})
	})
}

// writePull 写这一轮的 PR：已经记下编号的更新描述（返修只改原 PR），否则找到或创建（ensurePull），再加 label。这组写操作
// 共用一枚只有 pull_requests: write 的 token（不附带 contents），第一次写时签发、返回时吊销；只有已经关闭的 PR 时
// （pullClosedError）没有写操作，不签发。
//
// 返回值 verified 表示这个 PR 是可信的发布来源（新建的，或者核验通过的已有 PR）；creator 是核验时平台返回的作者
// （新建时为空）。
func (p *pass) writePull(ctx context.Context, w *WorkItem, title, body string, hold bool) (pr int, creator string, verified bool, err error) {
	pw := p.writer(permPullRequests)
	defer pw.release(ctx)
	if w.PRNumber == nil {
		pr, creator, verified, err = p.ensurePull(ctx, pw, w, title, body)
		if err != nil {
			return 0, "", false, err
		}
		return pr, creator, verified, p.labelPull(ctx, pw, w, pr, hold)
	}
	pr = *w.PRNumber
	creator, verified, err = p.existingPullVerified(ctx, w, pr)
	if err != nil {
		return 0, "", false, err
	}
	if err := pw.do(ctx, func(f forge.Repo) error { return f.EditPullBody(ctx, pr, body) }); err != nil {
		return 0, "", false, fmt.Errorf("更新 PR #%d 的描述：%w", pr, err)
	}
	return pr, creator, verified, p.labelPull(ctx, pw, w, pr, hold)
}

// existingPullVerified 判断已经记下编号的 PR（返修推进同一个 PR）是否可信的发布来源（PLAN #29.4、I19）：归属记录
// 存在且身份列（工作项、阶段、轮次、来源分支、目标分支）与工作项当前的一致时直接复用；归属缺失（例如升级前记下的 PR）时
// 按平台返回的 PR 重新核验（不是 fork、head/base 与正式分支和目标分支一致、作者是本 App），通过才可信、并返回真实的作者。
// 核验不通过时返修照常推进代码，但绝不记为可信来源：升级前收养的第三方 Bot PR 不会因为一次返修被提升为可信的发布来源。
func (p *pass) existingPullVerified(ctx context.Context, w *WorkItem, pr int) (creator string, verified bool, err error) {
	matched, err := pullOriginMatches(ctx, p.conn, w, pr)
	if err != nil || matched {
		return "", matched, err
	}
	pull, err := p.f.GetPull(ctx, pr)
	if err != nil {
		return "", false, fmt.Errorf("读取 PR #%d：%w", pr, err)
	}
	verified, err = verifiedPull(w, pull, p.s.checkSelf)
	if err != nil {
		// 身份未就绪：返回错误稍后重试，不要以“未核验”静默跳过归属记录
		return "", false, err
	}
	if !verified {
		return "", false, nil
	}
	return pull.Author, true, nil
}

// pullOriginMatches 判断 PR pr 的永久归属存在，且身份列（工作项、阶段、轮次、来源分支、目标分支）与工作项 w 当前的一致。
func pullOriginMatches(ctx context.Context, q store.Querier, w *WorkItem, pr int) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dev_pull_origins WHERE repo_id=$1 AND pr_number=$2 AND work_item_id=$3
		AND phase=$4 AND impl_round=$5 AND head_ref=$6 AND base_ref=$7)`,
		w.RepoID, pr, w.ID, w.Phase, w.ImplRound, w.Branch(), w.BaseRef).Scan(&ok)
	return ok, err
}

// labelPull 给推进的 PR 加 label（用 PR 写操作的 token pw）：要人确认时加 kiln:hold；设计 PR 加 release:skip（合并后不打
// 版本 tag，PLAN #31.5）。失败时下一次推进重试。
func (p *pass) labelPull(ctx context.Context, pw *writer, w *WorkItem, pr int, hold bool) error {
	var labels []string
	if hold && p.s.holdLabel != "" {
		labels = append(labels, p.s.holdLabel)
	}
	if w.Phase == PhaseDesign && p.s.skipLabel != "" {
		labels = append(labels, p.s.skipLabel)
	}
	for _, l := range labels {
		if err := pw.do(ctx, func(f forge.Repo) error { return f.AddLabel(ctx, pr, l) }); err != nil {
			return fmt.Errorf("给 PR #%d 加 %s：%w", pr, l, err)
		}
	}
	return nil
}

// advance 把正式分支（当前是 formal，不存在时为空）移到交付的 commit：不存在就创建，存在就只快进（I5）。不能快进
// （有人推送过，分叉）时返回不通过的原因。确需写 ref 时签一枚只有 contents: write 的 token 执行这一个写操作，返回后吊销；
// 已经是交付的 commit 时不签发。
func (p *pass) advance(ctx context.Context, w *WorkItem, formal string) (rejected string, err error) {
	branch := w.Branch()
	if formal == w.DeliverySHA {
		return "", nil
	}
	cw := p.writer(permContents)
	defer cw.release(ctx)
	if formal == "" {
		err := cw.do(ctx, func(f forge.Repo) error { return f.CreateBranch(ctx, branch, w.DeliverySHA) })
		if errors.Is(err, forge.ErrBranchExists) {
			return "", fmt.Errorf("正式分支 %s 刚刚被创建，下一次重新核对", branch)
		}
		if err != nil {
			return "", fmt.Errorf("创建正式分支 %s：%w", branch, err)
		}
		return "", nil
	}
	err = cw.do(ctx, func(f forge.Repo) error { return f.FastForward(ctx, branch, w.DeliverySHA) })
	if errors.Is(err, forge.ErrNotFastForward) {
		return fmt.Sprintf("正式分支 %s（%s）不是交付 %s 的祖先：有人推送过这个分支（分叉），需要人处理",
			branch, shorten.Hash(formal), shorten.Hash(w.DeliverySHA)), nil
	}
	if err != nil {
		return "", fmt.Errorf("快进正式分支 %s：%w", branch, err)
	}
	return "", nil
}

// errPullNotOurs 表示正式分支上有一个打开着的 PR，但它不是本 App 在同一个分支、目标分支上创建的（第三方或伪造）。
var errPullNotOurs = errors.New("正式分支上的 PR 不是 Kiln 创建的")

// ensurePull 返回工作项的 PR：head 分支已经有打开的 PR 就用它（更新描述），否则创建。读用共用客户端，写用 PR 写操作的
// token pw。
//
// 已有 PR 只有核验通过（同一仓库、分支和目标分支都对得上、作者是本 App）才接管并记为可信的发布来源；核验不通过时返回
// errPullNotOurs，由调用方把工作项转为受阻：绝不把第三方或伪造的 PR 提升为可信发布来源。新建的 PR 由本 App 用 token
// 创建，本身就是可信来源。
func (p *pass) ensurePull(ctx context.Context, pw *writer, w *WorkItem, title, body string) (pr int, creator string, verified bool, err error) {
	for range 2 {
		list, err := p.f.PullsByHead(ctx, w.Branch())
		if err != nil {
			return 0, "", false, fmt.Errorf("按分支找 PR：%w", err)
		}
		for _, pull := range list {
			if pull.State == forge.PullOpen {
				verified, err := verifiedPull(w, &pull, p.s.checkSelf)
				if err != nil {
					// 身份未就绪：稍后重试，绝不据此把工作项转为受阻
					return 0, "", false, fmt.Errorf("核验 PR #%d 的作者：%w", pull.Number, err)
				}
				if !verified {
					return 0, "", false, fmt.Errorf("%w：PR #%d 的作者是 %q", errPullNotOurs, pull.Number, pull.Author)
				}
				if err := pw.do(ctx, func(f forge.Repo) error { return f.EditPullBody(ctx, pull.Number, body) }); err != nil {
					return 0, "", false, fmt.Errorf("更新 PR #%d 的描述：%w", pull.Number, err)
				}
				return pull.Number, pull.Author, true, nil
			}
		}
		if len(list) > 0 {
			// 正式分支只属于这个工作项，上面只有已经关闭的 PR：是上一次建的（响应丢失，编号没有记下）被人关闭了，
			// 或者别人用这个分支开过又关了。不再建新的，违背人的关闭
			return 0, "", false, &pullClosedError{pull: list[len(list)-1]}
		}
		var n int
		err = pw.do(ctx, func(f forge.Repo) (err error) {
			n, err = f.CreatePull(ctx, title, w.Branch(), w.BaseRef, body)
			return err
		})
		if err == nil {
			// 本 App 刚刚创建：创建本身就是可信来源，不需要再读作者
			return n, "", true, nil
		}
		if !errors.Is(err, forge.ErrPullExists) {
			return 0, "", false, fmt.Errorf("创建 PR：%w", err)
		}
		// 刚好被建了（可能是上一次的请求，响应丢失）：再找一次
	}
	return 0, "", false, errors.New("创建 PR：平台说已经存在，但按分支找不到")
}

// settleUnrecorded 在推进 PR 编号还没记下的工作项之前，按平台上的事实结束它。返回是否已经结束：
//   - 正式分支上已经有 PR、都已经关闭（建 PR 的响应丢失之后人关闭或合并了它）：按 PR 的真实状态结束，合并了的转
//     merged。要在核对交付之前判断：普通合并之后交付已经在目标分支上，"没有新的提交"会把它误判为不通过；
//   - 没有 PR、工作项的 Issue 却已经全部关闭：终止，不再建 PR。Issue 关闭事件不在 delivering 时直接终止（那时可能
//     已经有没记下的 PR，合并时 GitHub 关闭 Issue 的事件可能先到），由这里在看过 PR 之后判断。
func (p *pass) settleUnrecorded(ctx context.Context, w *WorkItem) (bool, error) {
	list, err := p.f.PullsByHead(ctx, w.Branch())
	if err != nil {
		return false, fmt.Errorf("按分支找 PR：%w", err)
	}
	if len(list) > 0 {
		for _, pr := range list {
			if pr.State == forge.PullOpen {
				return false, nil
			}
		}
		return true, p.closedBeforeRecorded(ctx, w, list[len(list)-1])
	}
	var open int
	if err := p.conn.QueryRow(ctx, `SELECT count(*) FROM dev_work_item_issues WHERE work_item_id=$1 AND NOT closed`, w.ID).Scan(&open); err != nil {
		return false, err
	}
	if open > 0 {
		return false, nil
	}
	var cancelReq int64
	err = p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		var err error
		cancelReq, err = p.s.stopTx(ctx, tx, w.ID, EvStop, reasonIssuesClosed, auditActor)
		return err
	})
	if err != nil && !errors.Is(err, ErrFinished) {
		return true, err
	}
	p.s.cancelAfterCommit(ctx, w.ID, cancelReq)
	w.State = StateStopped
	return true, nil
}

// pullClosedError 表示工作项本轮的正式分支上只有已经关闭的 PR（pull 是最新的一个）。
type pullClosedError struct {
	pull forge.Pull
}

func (e *pullClosedError) Error() string { return fmt.Sprintf("PR #%d 已经关闭", e.pull.Number) }

// closedBeforeRecorded 处理编号还没有记下、PR 就被关闭了的工作项：与 PR 关闭事件的处理相同（事件可能丢失、已经被忽略或者
// 还没到；晚到时工作项已经换了阶段、轮次或者结束，不再改变）。合并得到的 commit 从平台读到的 PR 中取。之后不再推进这个
// 工作项（w 已经过时），下一次处理按最新的状态继续。
//
// 合并的 PR 核验通过（由本 App 在正式分支上创建）时在同一事务中记下它的永久归属：合并事件可能先到、按未知来源跳过了
// 自动打 tag，人可以在补齐归属之后重试。
func (p *pass) closedBeforeRecorded(ctx context.Context, w *WorkItem, pr forge.Pull) error {
	merged := pr.State == forge.PullMerged
	var verified bool
	if merged {
		var err error
		// 身份未就绪时返回错误稍后重试：不能丢掉这个已经合并的 PR 的归属记录
		if verified, err = verifiedPull(w, &pr, p.s.checkSelf); err != nil {
			return err
		}
	}
	var cancelReq int64
	err := p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		if merged && verified {
			if err := recordPullOrigin(ctx, tx, pullOrigin{
				RepoID: w.RepoID, PR: pr.Number, WorkItemID: w.ID, Phase: w.Phase, ImplRound: w.ImplRound, HeadRef: w.Branch(),
				BaseRef: w.BaseRef, CreatorLogin: pr.Author, DeliveryTaskID: w.DeliveryTaskID, DeliverySHA: w.DeliverySHA,
			}); err != nil {
				return err
			}
		}
		var err error
		_, cancelReq, err = p.s.pullClosedTx(ctx, tx, w.ID, ClosedPull{Number: pr.Number, HeadRef: w.Branch(), HeadSHA: pr.HeadSHA, Merged: merged, MergeSHA: pr.MergeSHA},
			devreport.BranchRef{ID: w.ID, Phase: w.Phase, Round: w.ImplRound}, auditActor, false)
		return err
	})
	if err != nil && !errors.Is(err, ErrFinished) {
		return err
	}
	p.s.cancelAfterCommit(ctx, w.ID, cancelReq)
	cur, err := GetWorkItem(ctx, p.conn, w.ID)
	if err != nil {
		return err
	}
	*w = *cur
	p.s.log.Info("AI 开发：正式分支上的 PR 已经关闭", "repo", p.repo.FullName(), "work_item", w.ID, "pr", pr.Number, "merged", merged)
	return nil
}

// designViolation 返回设计阶段的交付不符合要求的原因（PLAN #31.3 I12、#31.4；不是设计阶段或符合时为空）：只能改 Markdown
// （按平台返回的文件列表；列表不完整时由 holdReasons 加 hold），交付报告列出的设计文档要在改动中。一个 PR 完成的交付
// （没有已合并的设计、第一轮实现）列出的设计文档同样要在改动中；之后的实现可以引用已经合并的设计文档。
func designViolation(w *WorkItem, d *forge.Diff, rep *devreport.Delivery) string {
	if w.Phase == PhaseDesign {
		var code []string
		for _, f := range d.Files {
			if !devreport.IsMarkdown(f) {
				code = append(code, f)
			}
		}
		if len(code) > 0 {
			return "设计 PR 只能改 Markdown，改了 " + shorten.Text(strings.Join(code, "、"), maxReasonLen/2)
		}
	}
	ownsDesign := w.Phase == PhaseDesign || (w.DesignPR == nil && w.ImplRound == 1)
	if rep == nil || !d.FilesComplete || !ownsDesign {
		return ""
	}
	var missing []string
	for _, doc := range rep.Design.Documents {
		if !slices.Contains(d.Files, doc) {
			missing = append(missing, doc)
		}
	}
	if len(missing) > 0 {
		return "交付报告列出的设计文档不在改动中：" + defuse(strings.Join(missing, "、"))
	}
	return ""
}

// holdReasons 返回交付要加 kiln:hold 的原因（PLAN #26.12、#31.5）：改动了敏感路径；文件或提交太多，无法逐个核对；提交说明中
// 有关闭关键字引用了工作项中完整解决的 Issue 之外的 Issue（合并时 GitHub 会按它关闭，Server 改不了提交说明；设计阶段
// 没有完整解决的 Issue，有关闭关键字就加）；交付声明要改已确认的决定（confirmed_decision）。本轮之前的交付加过 hold 时
// 保留说明（label 不会自动去掉；新的轮次重新判断）。
func holdReasons(w *WorkItem, d *forge.Diff, rep *devreport.Delivery, issues []WorkIssue) []string {
	var out []string
	if rep != nil {
		for _, x := range rep.Design.Decisions {
			if x.Category == devreport.DecisionConfirmed {
				out = append(out, "交付声明要改已确认的决定 "+defuse(x.Ref)+"（见下面的决策记录）：需要人确认后去掉 hold 或手动入队")
			}
		}
	}
	if touchesSensitive(d.Files) {
		out = append(out, "改动涉及 CI / Kiln 配置或给 AI 的说明（`"+strings.Join(sensitivePaths(), "`、`")+"`）")
	}
	if !d.FilesComplete {
		out = append(out, "改动的文件太多，无法逐个核对是否涉及敏感路径")
	}
	var full []int
	for _, is := range issues {
		res := is.Resolution
		if rep != nil {
			res = rep.Resolution(is.Number)
		}
		if res == devreport.ResolutionFull {
			full = append(full, is.Number)
		}
	}
	if refs := closingRefs(d.Messages, full); len(refs) > 0 {
		out = append(out, "提交说明中的关闭关键字引用了 "+defuse(strings.Join(refs, " "))+"：合并时 GitHub 可能按它关闭这些 Issue，请先改写提交说明")
	}
	if !d.MessagesComplete {
		out = append(out, "提交太多，无法逐个核对提交说明中的关闭关键字")
	}
	if len(out) == 0 && w.HoldLabel {
		out = append(out, "之前的交付加了 kiln:hold")
	}
	return out
}

// touchesSensitive 判断改动是否涉及敏感路径。
func touchesSensitive(files []string) bool {
	return slices.ContainsFunc(files, func(f string) bool {
		return slices.ContainsFunc(sensitivePaths(), func(p string) bool {
			return f == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(f, p))
		})
	})
}

// latestDelivery 返回工作项最近一次有效的交付报告和工作项的 Issue。
func (p *pass) latestDelivery(ctx context.Context, w *WorkItem) (*devreport.Delivery, []WorkIssue, error) {
	issues, err := workIssues(ctx, p.conn, w.ID)
	if err != nil {
		return nil, nil, err
	}
	rep, err := lastDelivery(ctx, p.conn, w.ID)
	return rep, issues, err
}

// lastDelivery 返回工作项最近一次有效的交付报告（没有时为 nil）。
func lastDelivery(ctx context.Context, q store.Querier, workItem int64) (*devreport.Delivery, error) {
	return queryDelivery(ctx, q, workItem, `work_item_id=$1 AND kind=$2 ORDER BY created_at DESC LIMIT 1`, workItem, reportDelivery)
}

// taskDelivery 返回任务 task 的交付报告（task 为 nil 或者没有报告时为 nil）。
func taskDelivery(ctx context.Context, q store.Querier, workItem int64, task *int64) (*devreport.Delivery, error) {
	if task == nil {
		return nil, nil
	}
	return queryDelivery(ctx, q, workItem, `task_id=$1 AND kind=$2`, *task, reportDelivery)
}

// queryDelivery 返回满足条件 where 的第一份交付报告（没有时为 nil）。
func queryDelivery(ctx context.Context, q store.Querier, workItem int64, where string, args ...any) (*devreport.Delivery, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT report FROM dev_reports WHERE `+where, args...).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 保存的报告在 Complete 时已经严格校验过
	var rep devreport.Delivery
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("工作项 #%d 的交付报告无法解析：%w", workItem, err)
	}
	return &rep, nil
}

// syncLabels 让 Issue 上的状态 label 与工作项的状态一致（只是展示，领取以 Postgres 为准）：label 已经按某个状态同步过
// （labels_state）就不再调用。结束的工作项去掉全部状态 label，保留人加的 kiln:ready。
func (p *pass) syncLabels(ctx context.Context) error {
	list, err := queryWorkItems(ctx, p.conn, `repo_id=$1 AND labels_state<>state`, p.repo.ID)
	if err != nil {
		return err
	}
	labels := stateLabels()
	for _, w := range list {
		issues, err := workIssues(ctx, p.conn, w.ID)
		if err != nil {
			return err
		}
		want := LabelFor(w.State)
		for _, is := range issues {
			if is.Closed {
				continue
			}
			for _, l := range labels {
				var err error
				if l == want {
					err = p.f.AddLabel(ctx, is.Number, l)
				} else {
					err = p.f.RemoveLabel(ctx, is.Number, l)
				}
				if forge.IsRejected(err) {
					// Issue 被删除、没有权限：label 只是展示，不阻塞
					p.s.log.Warn("AI 开发：同步 label 被拒", "repo", p.repo.FullName(), "issue", is.Number, "label", l, "err", err)
					continue
				}
				if err != nil {
					return fmt.Errorf("同步 Issue #%d 的 label %s：%w", is.Number, l, err)
				}
			}
		}
		if _, err := p.conn.Exec(ctx, `UPDATE dev_work_items SET labels_state=$2 WHERE id=$1 AND state=$2`, w.ID, w.State); err != nil {
			return err
		}
	}
	return nil
}

// gcBranches 删除已经推进过的交付分支和超过保留时间的 attempt 分支（PLAN #26.13：checkpoint 和没有通过核对的交付
// 留给人查看，不 pin，过期丢弃）。
func (p *pass) gcBranches(ctx context.Context) error {
	rows, err := p.conn.Query(ctx, `SELECT b.branch FROM dev_branches b LEFT JOIN dev_work_items w ON w.id=b.work_item_id
		WHERE b.repo_id=$1 AND b.deleted_at IS NULL AND (b.created_at < now() - make_interval(secs => $2)
		  OR (b.kind='delivery' AND w.head_sha=b.sha AND w.delivery_branch<>b.branch))`, p.repo.ID, p.s.branchTTL.Seconds())
	if err != nil {
		return err
	}
	branches, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, b := range branches {
		if !strings.HasPrefix(b, devreport.AttemptBranchPrefix) {
			continue // 只删除 attempt 分支（防御：记录被改坏时不删除别的分支）
		}
		if err := p.f.DeleteBranch(ctx, b); err != nil && !forge.IsRejected(err) {
			return fmt.Errorf("删除 attempt 分支 %s：%w", b, err)
		}
		if _, err := p.conn.Exec(ctx, `UPDATE dev_branches SET deleted_at=now() WHERE repo_id=$1 AND branch=$2`, p.repo.ID, b); err != nil {
			return err
		}
	}
	return nil
}

package aidev

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 返修（PLAN #26.11）：PR head 的 CI 和 AI 审计全部结束后，AI 审计发现问题（ai.findings）或 CI 有非基础设施的失败时返修；
// 基础设施失败（infra.*）、审计未完成、超时、取消不返修，由重跑解决。同一个 head 只返修一次（fixed_head）。合并队列交回的
// 冲突另见 conflict.go，规划时一起交给这一轮返修。

// infraClassPrefix 是基础设施错误分类的前缀。
const infraClassPrefix = "infra."

// maxCIFailureLen 是一条 CI 失败说明的上限。
const maxCIFailureLen = 1000

// CIResults 是 PR head 上 CI 的结果（PLAN #30.3）：由组装层用 CI 领域的数据实现（aidev 不读 CI 的表）。
type CIResults interface {
	// Settled 返回仓库 repoID 的 PR pr 在 head 上的 CI（event=pull_request 的 run）：runs 是 run 的数量，done 表示都已结束，
	// failures 是已结束的 run 最近一次运行中失败（failed、timed_out，与 run 的结论用同一条规则）的 job。
	Settled(ctx context.Context, q store.Querier, repoID int64, pr int, head string) (runs int, done bool, failures []CIFailure, err error)
}

// CIFailure 是一个失败的 CI job，或者一个因配置错误直接失败（没有 job）的 run。
type CIFailure struct {
	// JobID 是失败的 job；run 自身因配置错误失败时为 0，Name 是 workflow 名。
	JobID int64
	Name  string
	// Reason 是 job 的结束说明（配置错误时是 run 的配置错误）；ErrClass 是它当前 attempt 的错误分类（infra.* 不返修）。
	Reason   string
	ErrClass string
}

// fixInputs 返回 PR head 上要返修的问题：审计 finding（稳定 ID F<审计任务>-<序号>）和失败的 CI；done 为 false 表示
// head 上还有 CI run 或审计请求没有结束（或者两者都还没有），先不判断。
func (s *Service) fixInputs(ctx context.Context, q store.Querier, repoID int64, pr int, head string) (findings []aispec.DevFinding, ci []string, done bool, err error) {
	var runs int
	ciDone := true
	var failures []CIFailure
	if s.ci != nil {
		if runs, ciDone, failures, err = s.ci.Settled(ctx, q, repoID, pr, head); err != nil {
			return nil, nil, false, err
		}
	}
	reviews, aiDone, findings, err := reviewInputs(ctx, q, repoID, pr, head)
	if err != nil || !ciDone || !aiDone || runs+reviews == 0 {
		return nil, nil, false, err
	}
	for _, f := range failures {
		if strings.HasPrefix(f.ErrClass, infraClassPrefix) {
			continue
		}
		ci = append(ci, shorten.Text(ciFailureText(f), maxCIFailureLen))
	}
	return findings, ci, true, nil
}

// ciFailureText 是一条 CI 失败说明：有 job 时带 job 编号，run 因配置错误失败时只有 workflow 名和原因。
func ciFailureText(f CIFailure) string {
	if f.JobID == 0 {
		return fmt.Sprintf("%s：%s", f.Name, f.Reason)
	}
	return fmt.Sprintf("job #%d %s：%s", f.JobID, f.Name, f.Reason)
}

// reviewInputs 返回 PR head 上的审计：请求族的数量、是否都已结束、发现的问题。每个请求族看最近的一次（重跑之后看重跑的）；
// 沿用的任务按结果的来源取报告。
func reviewInputs(ctx context.Context, q store.Querier, repoID int64, pr int, head string) (families int, done bool, findings []aispec.DevFinding, err error) {
	reqs, err := ai.QueryRequests(ctx, q, `id IN (SELECT DISTINCT ON (r.family_id) r.id FROM ai_requests r
		JOIN ai_review_requests d ON d.request_id=r.id
		WHERE r.type=$1 AND r.repo_id=$2 AND r.event=$3 AND r.sha=$4 AND d.pr_number=$5 ORDER BY r.family_id, r.id DESC) ORDER BY id`,
		agentcfg.TaskReview, repoID, gitref.EventPullRequest, head, pr)
	if err != nil {
		return 0, false, nil, err
	}
	ids := make([]int64, 0, len(reqs))
	for _, r := range reqs {
		if !r.State.Terminal() {
			return len(reqs), false, nil, nil
		}
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return 0, true, nil, nil
	}
	rows, err := q.Query(ctx, `SELECT s.id, coalesce(a.error_class, '') FROM ai_tasks t
		JOIN ai_tasks s ON s.id=coalesce(t.reused_from, t.id) LEFT JOIN ai_attempts a ON a.id=s.current_attempt_id
		WHERE t.request_id = ANY($1) AND t.state='failed' ORDER BY t.id`, ids)
	if err != nil {
		return 0, false, nil, err
	}
	type failed struct {
		Src      int64
		ErrClass string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[failed])
	if err != nil {
		return 0, false, nil, err
	}
	for _, f := range list {
		if f.ErrClass != ai.ErrClassFindings {
			continue // 审计未完成、报告无效、基础设施错误：重跑审计，不返修
		}
		fs, err := reviewFindings(ctx, q, f.Src)
		if err != nil {
			return 0, false, nil, err
		}
		findings = append(findings, fs...)
	}
	return len(reqs), true, findings, nil
}

// FindingID 返回审计任务的第 i 个（从 0 开始）finding 的稳定 ID。
func FindingID(reviewTask int64, i int) string { return fmt.Sprintf("F%d-%d", reviewTask, i+1) }

// reviewFindings 返回审计任务报告中的 finding。
func reviewFindings(ctx context.Context, q store.Querier, taskID int64) ([]aispec.DevFinding, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT report FROM ai_reviews WHERE task_id=$1`, taskID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rep, err := aireport.Parse(data)
	if err != nil {
		// 保存时校验过；解析不了（报告格式升级等）按审计未完成处理，不返修，也不让工作项一直卡在出错上
		return nil, nil //nolint:nilerr // 见上
	}
	out := make([]aispec.DevFinding, 0, len(rep.Findings))
	for i, f := range rep.Findings {
		detail := f.Body
		if f.Scenario != "" {
			detail = strings.TrimSpace(strings.Join([]string{
				"场景：" + f.Scenario, "实际：" + f.Actual, "预期：" + f.Expected, "影响：" + f.Impact, "证据：" + f.Proof, f.Body,
			}, "\n"))
		}
		out = append(out, aispec.DevFinding{ID: FindingID(taskID, i), Title: f.Title, Severity: f.Severity, Path: f.Path, Line: f.Line, Detail: detail})
	}
	return out, nil
}

// evaluate 判断 PR 是否需要返修（pr_open）。
func (p *pass) evaluate(ctx context.Context, w *WorkItem) error {
	if w.PRNumber == nil || w.HeadSHA == "" || w.FixedHead == w.HeadSHA {
		return nil
	}
	findings, ci, done, err := p.s.fixInputs(ctx, p.conn, w.RepoID, *w.PRNumber, w.HeadSHA)
	if err != nil || !done || (len(findings) == 0 && len(ci) == 0) {
		return err
	}
	what := fmt.Sprintf("PR head %s：AI 审计 %d 个问题、CI 失败 %d 个", shorten.Hash(w.HeadSHA), len(findings), len(ci))
	return p.s.tx(ctx, p.conn, func(tx pgx.Tx) error {
		// 返修上限按本阶段、本轮计算（PLAN #31.3），冲突返修另计（conflict.go）
		if w.RoundCheckFixes() >= w.MaxFixRounds {
			_, err := p.s.transition(ctx, tx, w, EvRoundsExhausted, fmt.Sprintf("%s返修 %d 轮后仍未通过（%s），需要人处理",
				phaseName(w), w.RoundCheckFixes(), what), `fixed_head=$9`, w.HeadSHA)
			return err
		}
		_, err := p.s.transition(ctx, tx, w, EvChecksFailed, fmt.Sprintf("%s第 %d 轮返修：%s", phaseName(w), w.RoundCheckFixes()+1, what),
			`fixed_head=$9, fix_rounds=fix_rounds+1, task_id=NULL`, w.HeadSHA)
		return err
	})
}

// planFixes 为还没有任务的返修（fixing 且 task_id 为空）规划请求（不持有仓库锁）。
func (s *Service) planFixes(ctx context.Context, repo *store.Repo) error {
	list, err := queryWorkItems(ctx, s.db, `repo_id=$1 AND state=$2 AND task_id IS NULL`, repo.ID, StateFixing)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range list {
		if err := s.planFix(ctx, repo, w); err != nil {
			errs = append(errs, fmt.Errorf("规划工作项 #%d 的返修：%w", w.ID, err))
		}
	}
	return errors.Join(errs...)
}

// planFix 规划一轮返修：配置和说明从当时的默认分支读取，起点是正式分支的 head（有更新的 checkpoint 时从 checkpoint 继续），
// 带上工作项的 Issue（当前内容）、要处理的问题、失败的 CI、上一轮的交接。去重键带轮次，重复规划只建一个请求。
func (s *Service) planFix(ctx context.Context, repo *store.Repo, w *WorkItem) error {
	f := s.forge(repo)
	dc, why, err := loadDevConfig(ctx, f, repo)
	if err != nil {
		return err
	}
	if why != "" {
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := s.transition(ctx, tx, w, EvExecFailed, "无法返修："+why, "")
			return err
		})
	}
	start, startNote, err := fixStart(ctx, s.db, f, w)
	if err != nil {
		return err
	}
	// 工具链按返修的起点（正式分支或 checkpoint）上的 lockfile：返修可能正是在升级它们
	tools, why, err := devTools(ctx, f, start, dc.cfg)
	if err != nil {
		return err
	}
	if why != "" {
		return s.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := s.transition(ctx, tx, w, EvExecFailed, "无法返修："+why, "")
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
	var findings []aispec.DevFinding
	var ci []string
	if w.PRNumber != nil {
		head := cmp.Or(w.FixedHead, w.HeadSHA)
		if findings, ci, _, err = s.fixInputs(ctx, s.db, w.RepoID, *w.PRNumber, head); err != nil {
			return err
		}
	}
	var conflict *aispec.DevConflict
	if c := pendingConflict(w); c != nil {
		// 交回时已经核对过冲突的目标分支就是工作项的目标分支（HandBack），base commit 与分支名来自同一个分支
		conflict = &aispec.DevConflict{Head: c.head, BaseRef: w.BaseRef, BaseSHA: c.base}
	}
	handoff, err := lastHandoff(ctx, s.db, w)
	if err != nil {
		return err
	}
	if startNote != "" {
		handoff = strings.TrimSpace(handoff + "\n" + startNote)
	}
	pr := 0
	if w.PRNumber != nil {
		pr = *w.PRNumber
	}
	cfg := dc.cfg
	r, t, err := s.plan(ctx, &devRequest{
		repo: repo, ref: gitref.BranchPrefix + w.Branch(), sha: start, platforms: cfg.DevPlatforms(),
		dedupeKey: fixKey(repo.ID, w.ID, w.FixRounds), sender: auditActor, timeout: int(cfg.DevTimeout().Minutes()),
		spec: &aispec.DevelopSpec{
			Mode: aispec.DevFix, Agent: cfg.Development.Agent, Instructions: dc.instructions, ConfigSHA: dc.head,
			WorkItemID: w.ID, Branch: w.Branch(), BaseRef: w.BaseRef, PRNumber: pr, Issues: issues, Findings: findings, CIFailures: ci,
			Handoff: handoff, Round: w.FixRounds, RequirementsChanged: changed || w.RequirementsChanged, Services: cfg.Services(),
			PlatformTools: tools, Repositories: cfg.Repositories(), MaxFixRounds: w.MaxFixRounds,
			Phase: w.Phase, ImplRound: w.ImplRound, DesignPR: derefInt(w.DesignPR), DesignSHA: w.DesignSHA, Conflict: conflict, Docs: docs,
		},
	})
	if err != nil {
		return err
	}
	unneeded := false
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		unneeded = false
		// 只关联到规划时的那一轮：期间这一轮的任务已经由结束时关联并结束、人又重试开了新的一轮时，新的一轮要规划自己
		// 的任务
		tag, err := tx.Exec(ctx, `UPDATE dev_work_items SET task_id=$2, requirements_changed=requirements_changed OR $3, updated_at=now()
			WHERE id=$1 AND state=$4 AND task_id IS NULL AND fix_rounds=$5`, w.ID, t.ID, changed, StateFixing, w.FixRounds)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var cur *int64
			if err := tx.QueryRow(ctx, `SELECT task_id FROM dev_work_items WHERE id=$1`, w.ID).Scan(&cur); err != nil {
				return err
			}
			// 期间结束时已经关联了这个任务（见 RecordResult）时什么也不做；工作项被终止或已经有了别的任务时这个请求不再需要
			unneeded = cur == nil || *cur != t.ID
			return nil
		}
		s.log.Info("AI 开发：已规划返修", "repo", repo.FullName(), "work_item", w.ID, "round", w.FixRounds, "request", r.ID,
			"findings", len(findings), "ci_failures", len(ci), "conflict", conflict != nil)
		return nil
	})
	if err != nil || !unneeded {
		return err
	}
	// 取消在单独的事务中进行（加锁顺序：并发组 → 请求 → 任务 → 工作项，不能在锁住工作项之后再锁请求）
	return s.core.Cancel(ctx, r.ID, ai.CancelWorkItemDone, "")
}

// fixStart 返回返修的起点：本轮正式分支的 head；没有正式分支（还没推进过）时是本轮最近的 checkpoint 或本轮的起点；
// 有包含正式分支 head 的更新的 checkpoint 时从 checkpoint 继续（上一次中断时保存的改动）。checkpoint 只用本阶段、本轮
// 保存的（PLAN #31.3），不把上一阶段或上一轮的改动带进来。从 checkpoint 继续时 note 写明（交给 Agent：起点上已经有
// 上一次未完成的改动）。
func fixStart(ctx context.Context, q store.Querier, f forge.Repo, w *WorkItem) (start, note string, err error) {
	formal, err := f.BranchHead(ctx, w.Branch())
	if err != nil {
		return "", "", fmt.Errorf("读取正式分支：%w", err)
	}
	var cpBranch, cpSHA string
	err = q.QueryRow(ctx, `SELECT branch, sha FROM dev_branches WHERE work_item_id=$1 AND kind='checkpoint' AND deleted_at IS NULL
		AND phase=$2 AND impl_round=$3 ORDER BY created_at DESC LIMIT 1`, w.ID, w.Phase, w.ImplRound).Scan(&cpBranch, &cpSHA)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	if cpSHA != "" {
		head, err := f.BranchHead(ctx, cpBranch)
		switch {
		case err == nil && head == cpSHA:
		case err == nil, forge.IsRejected(err):
			cpSHA = "" // 已经被删除、被改动，或者平台拒绝读取：不用
		default:
			return "", "", fmt.Errorf("读取 checkpoint 分支 %s：%w", cpBranch, err)
		}
	}
	fromCheckpoint := fmt.Sprintf("起点是上一轮中断时保存的未完成改动（分支 %s，%s），先检查这些改动再继续", cpBranch, shorten.Hash(cpSHA))
	switch {
	case formal == "" && cpSHA != "":
		return cpSHA, fromCheckpoint, nil
	case formal == "":
		return w.BaseSHA, "", nil
	case cpSHA != "" && cpSHA != formal:
		behind, err := f.Behind(ctx, formal, cpSHA)
		switch {
		case err == nil && behind == 0:
			return cpSHA, fromCheckpoint, nil
		case err != nil && !forge.IsRejected(err):
			// 临时失败不能悄悄退回正式分支：那会丢掉 checkpoint 里未完成的改动。
			return "", "", fmt.Errorf("比较 checkpoint 与正式分支：%w", err)
		}
	}
	return formal, "", nil
}

// currentIssues 读取工作项的 Issue 的当前内容；changed 表示有 Issue 的标题或正文与领取时不同。读不到的 Issue
// 用领取时的标题。
func currentIssues(ctx context.Context, q store.Querier, f forge.Repo, w *WorkItem) (out []aispec.DevIssue, changed bool, err error) {
	is, err := workIssues(ctx, q, w.ID)
	if err != nil {
		return nil, false, err
	}
	for _, i := range is {
		cur, err := f.GetIssue(ctx, i.Number)
		switch {
		case forge.IsRejected(err):
			out = append(out, aispec.DevIssue{Number: i.Number, Title: i.Title, Digest: i.Digest})
			continue
		case err != nil:
			return nil, false, fmt.Errorf("读取 Issue #%d：%w", i.Number, err)
		}
		d := devIssue(cur)
		changed = changed || d.Digest != i.Digest
		out = append(out, d)
	}
	return out, changed, nil
}

// phaseName 返回写在原因中的阶段名（先设计的工作项才写）。
func phaseName(w *WorkItem) string {
	switch {
	case w.Phase == PhaseDesign:
		return "设计 PR "
	case w.DesignFirst() || w.ImplRound > 1:
		return fmt.Sprintf("第 %d 轮实现 PR ", w.ImplRound)
	}
	return ""
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// lastHandoff 返回返修的交接（最近一次交付报告的 handoff）和受阻的原因。
func lastHandoff(ctx context.Context, q store.Querier, w *WorkItem) (string, error) {
	rep, err := lastDelivery(ctx, q, w.ID)
	if err != nil {
		return "", err
	}
	return handoff(rep, w), nil
}

// handoff 返回交付报告 rep（可能为 nil）的交接和工作项 w 受阻的原因。
func handoff(rep *devreport.Delivery, w *WorkItem) string {
	var parts []string
	if rep != nil {
		h := rep.Handoff
		for _, kv := range [][2]string{{"已完成", h.Done}, {"剩余", h.Remaining}, {"决定", h.Decisions}, {"下一步", h.Next}} {
			if strings.TrimSpace(kv[1]) != "" {
				parts = append(parts, kv[0]+"："+kv[1])
			}
		}
	}
	if w.Reason != "" {
		parts = append(parts, "工作项的状态："+w.Reason)
	}
	return strings.Join(parts, "\n")
}

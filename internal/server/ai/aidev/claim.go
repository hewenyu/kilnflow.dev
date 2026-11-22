package aidev

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 领取（PLAN #26.11）：先逐个读选中的 Issue 核对（打开、仍有 ready、没有 skip、内容摘要与交给 Agent 的快照相同），
// 再在一个事务中校验 attempt 和 epoch、写入工作项和 Issue。I1（一个 Issue 一个未结束的工作项）、I3（一次执行至多领取一次）
// 由唯一索引保证：插入用 ON CONFLICT DO NOTHING，没有插入就是冲突；I14（仓库中有未结束的工作项时不建新的）在仓库的
// 领取锁下判断。领取成功或没有领取都结束这一轮选题。

// ErrInvalidClaim 表示领取请求不合法（不是选题任务、选题报告无效等），重试没有意义。
var ErrInvalidClaim = errors.New("领取请求无效")

// ClaimRequest 是一次领取。
type ClaimRequest struct {
	AttemptID int64
	// Issues 是选中的 Issue（与选题报告的 selected 相同）。
	Issues    []int
	Selection []byte
	// Agent 是执行这次开发的 Agent（记入工作项开发过的 Agent，PLAN #33.7）；RunnerVersion 记入报告的适用范围。
	Agent         string
	RunnerVersion string
}

// ClaimResult 是领取的结果。
type ClaimResult struct {
	Claimed    bool
	WorkItemID int64
	Branch     string
	// Phase 是领取后的阶段（选题报告的 design_first，PLAN #31.4）。
	Phase string
	// Reason 是没有领取的原因；Candidates、InProgress 是最新的候选和进行中的工作项（候选为空时节点结束这次执行）。
	Reason     string
	Candidates []aispec.DevIssue
	InProgress []InProgress
	// Conflict 表示这次领取因为与别的工作项冲突而没有成功（候选已经更新，可以让 Agent 重新选题）。
	Conflict bool
}

// lockFunc 在事务 tx 中按加锁顺序锁住并复查 attempt（当前 epoch、仍在运行，I4）。
type lockFunc func(ctx context.Context, tx pgx.Tx) error

// attemptLock 返回任务 t 的当前 attempt attemptID 的 lockFunc。
func attemptLock(t *ai.Task, attemptID int64) lockFunc {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := ai.LockCurrentAttempt(ctx, tx, t.ID, attemptID)
		return err
	}
}

// selectSpec 返回选题任务的内容；t 不是 AI 开发的选题时返回 ErrInvalidClaim。
func selectSpec(t *ai.Task, attemptID int64) (*aispec.DevelopSpec, error) {
	d, err := developSpec(t)
	if err != nil || d.Mode != aispec.DevSelect {
		return nil, fmt.Errorf("%w：attempt #%d 不是 AI 开发的选题", ErrInvalidClaim, attemptID)
	}
	return d, nil
}

// errClaimConflict 让领取的事务回滚（Issue 已被另一个工作项领取，I1）。
var errClaimConflict = errors.New("领取冲突")

// ClaimIssues 领取选中的 Issue（t 是 attempt 所属的任务，调用方已经校验 attempt 属于调用的 Runner、是当前 epoch）。
// dry_run、没有选中时只保存选题报告（非 dry_run 时写入未选的跳过记录）。同一个 attempt 重复请求返回同一个结果（I3）。
func (s *Service) ClaimIssues(ctx context.Context, t *ai.Task, req *ClaimRequest) (*ClaimResult, error) {
	return s.claimIssues(ctx, t, req, attemptLock(t, req.AttemptID))
}

// claimIssues 是 ClaimIssues，写入前用 lock 锁住并复查 attempt。
func (s *Service) claimIssues(ctx context.Context, t *ai.Task, req *ClaimRequest, lock lockFunc) (*ClaimResult, error) {
	d, err := selectSpec(t, req.AttemptID)
	if err != nil {
		return nil, err
	}
	if w, err := queryWorkItem(ctx, s.db, `claim_attempt_id=$1`, req.AttemptID); err != nil || w != nil {
		return claimedResult(w), err
	}
	offers, err := s.offers(ctx, s.db, req.AttemptID)
	if err != nil {
		return nil, err
	}
	sel, err := parseClaim(req, offers, d.MaxIssues)
	if err != nil {
		return nil, err
	}
	r, err := ai.GetRequest(ctx, s.db, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	in := &claimInput{task: t, spec: d, req: req, request: r, sel: sel, offers: offers}
	if len(sel.Selected) == 0 || d.DryRun {
		return s.claimNothing(ctx, in, lock)
	}
	repo, err := store.GetRepo(ctx, s.db, r.RepoID)
	if err != nil {
		return nil, err
	}
	if s.forge == nil {
		return &ClaimResult{Reason: whyNoForge}, nil
	}
	f := s.forge(repo)
	conflict, fresh, err := verifySelected(ctx, f, sel.Selected, offers)
	if err != nil {
		return nil, err
	}
	if conflict != "" {
		return s.reoffer(ctx, in, conflict, fresh, lock)
	}
	var res *ClaimResult
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
			return err
		}
		var err error
		res, err = s.claimTx(ctx, tx, in)
		return err
	})
	if errors.Is(err, errClaimConflict) {
		return s.reoffer(ctx, in, strings.TrimPrefix(err.Error(), errClaimConflict.Error()+"："), nil, lock)
	}
	if err != nil {
		return nil, err
	}
	if res.Claimed {
		s.log.Info("AI 开发：已领取", "repo", repo.FullName(), "work_item", res.WorkItemID, "issues", sel.Selected)
		s.notify()
	}
	return res, nil
}

// claimInput 是一次领取请求核对过的内容。
type claimInput struct {
	task    *ai.Task
	spec    *aispec.DevelopSpec
	req     *ClaimRequest
	request *ai.Request
	sel     *devreport.Selection
	offers  map[int]aispec.DevIssue
}

// parseClaim 解析并核对选题报告：只能选候选中的 Issue，请求领取的 Issue 与报告一致。
func parseClaim(req *ClaimRequest, offers map[int]aispec.DevIssue, maxIssues int) (*devreport.Selection, error) {
	sel, err := parseSelection(req.Selection, offers, maxIssues)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", ErrInvalidClaim, err)
	}
	want := slices.Clone(sel.Selected)
	slices.Sort(want)
	got := slices.Clone(req.Issues)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return nil, fmt.Errorf("%w：选中的 Issue %v 与选题报告 %v 不一致", ErrInvalidClaim, req.Issues, sel.Selected)
	}
	return sel, nil
}

// parseSelection 校验选题报告（选中的都是交给这次执行的候选、没选的理由、数量上限）：领取和 ValidateReport 共用。
func parseSelection(selection []byte, offers map[int]aispec.DevIssue, maxIssues int) (*devreport.Selection, error) {
	return devreport.ParseSelection(selection, offerNumbers(offers), maxIssues)
}

// claimNothing 处理没有选中或 dry_run 的选题：只保存选题报告；不是 dry_run 时写入不适合做的 Issue 的跳过记录。
// 这一轮选题结束（任务池在这之后没有变化就不再选）。
func (s *Service) claimNothing(ctx context.Context, in *claimInput, lock lockFunc) (*ClaimResult, error) {
	dry := in.spec.DryRun
	reason := "没有选中 Issue"
	if dry {
		reason = "dry_run：只保存选题报告，没有领取"
	}
	return &ClaimResult{Reason: reason}, s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
			return err
		}
		if err := saveReport(ctx, tx, in.task.ID, in.req.AttemptID, reportSelection, nil, in.req.Selection, in.req.RunnerVersion); err != nil {
			return err
		}
		if err := endTurn(ctx, tx, in.request.RepoID, in.spec.SelectSeq); err != nil {
			return err
		}
		if !dry {
			if err := writeSkips(ctx, tx, in.request.RepoID, in.sel.Skipped, in.offers); err != nil {
				return err
			}
		}
		// 这一批看过了，什么也没领：写完跳过记录之后的候选都交给过这次选题（内容没变）时记下任务池状态，没有新变化就
		// 不再选（Agent 留到之后的 Issue 要等任务池变化——新的 Issue、工作项结束、内容变了——或者手动同步）。有没交给它
		// 的候选时（超过候选上限、跳过之后才露出来的，或者选题期间新加入的）不算看过，接着选
		st, err := readPool(ctx, tx, in.request.RepoID)
		if err != nil {
			return err
		}
		examined := st.fingerprint()
		if hasUnoffered(st.candidates, in.offers) {
			examined = ""
		}
		if err := setExamined(ctx, tx, in.request.RepoID, examined); err != nil {
			return err
		}
		return s.Enqueue(ctx, tx, in.request.RepoID)
	})
}

// hasUnoffered 判断候选中是否有没交给这次选题的（不在 offers 中，或者内容在交给它之后变了）。
func hasUnoffered(cands []aispec.DevIssue, offers map[int]aispec.DevIssue) bool {
	return slices.ContainsFunc(cands, func(c aispec.DevIssue) bool {
		o, ok := offers[c.Number]
		return !ok || o.Digest != c.Digest
	})
}

// claimTx 在已经锁住 attempt 的事务 tx 中领取：同一个 attempt 已经领取过时返回那个工作项（I3），否则保存选题报告，确认仓库没有被占用（I14），建工作项（I3 由唯一约束保证），占用 Issue（I1），
// 写入跳过记录、领取评论和审计日志，结束这一轮选题。Issue 已被占用时返回 errClaimConflict（事务回滚）。
func (s *Service) claimTx(ctx context.Context, tx pgx.Tx, in *claimInput) (*ClaimResult, error) {
	t, req, sel, d, repoID := in.task, in.req, in.sel, in.spec, in.request.RepoID
	// 同一个 attempt 并发的重复请求（I3）：事务外的预查可能都没看到工作项，锁住 attempt 后先复查，已经领取过的返回那个
	// 工作项；放在占用判断之前，否则会被自己刚建的工作项当成占用者挡住
	if w, err := queryWorkItem(ctx, tx, `claim_attempt_id=$1`, req.AttemptID); err != nil || w != nil {
		return claimedResult(w), err
	}
	if err := saveReport(ctx, tx, t.ID, req.AttemptID, reportSelection, nil, req.Selection, req.RunnerVersion); err != nil {
		return nil, err
	}
	// 仓库级串行（I14）：在仓库的领取锁下判断仓库是否被占用，并发的领取只有一个能建出工作项；被占用时不领取、结束
	// 这一轮选题（与暂停相同，不让 Agent 重新选）
	occ, err := lockClaim(ctx, tx, repoID)
	if err != nil {
		return nil, err
	}
	if why := occ.Why(); why != "" {
		return &ClaimResult{Reason: why}, endTurn(ctx, tx, repoID, d.SelectSeq)
	}
	rs, err := readRepoState(ctx, tx, repoID)
	if err != nil {
		return nil, err
	}
	if rs.paused {
		return &ClaimResult{Reason: whyPaused}, endTurn(ctx, tx, repoID, d.SelectSeq)
	}
	if err := checkPool(ctx, tx, repoID, sel.Selected, in.offers); err != nil {
		return nil, err
	}
	var agents []string
	if req.Agent != "" {
		agents = []string{req.Agent}
	}
	var id int64
	phase := sel.FirstPhase()
	err = tx.QueryRow(ctx, `INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id,
		dev_agents, plan, task_id, phase) VALUES ($1,$2,$3,$4,$5,$6,$7,coalesce($8::text[], '{}'),$9,$10,$11) ON CONFLICT DO NOTHING RETURNING id`,
		repoID, StateDeveloping, d.BaseRef, in.request.SHA, d.MaxFixRounds, d.Agent, req.AttemptID, agents, sel.Plan, t.ID, phase).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimLost(ctx, tx, req.AttemptID)
	}
	if err != nil {
		return nil, err
	}
	for _, n := range sel.Selected {
		o := in.offers[n]
		tag, err := tx.Exec(ctx, `INSERT INTO dev_work_item_issues (work_item_id, repo_id, issue_number, title, digest)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id, repoID, n, o.Title, o.Digest)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, fmt.Errorf("%w：Issue #%d 已被另一个工作项领取", errClaimConflict, n)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE dev_reports SET work_item_id=$3 WHERE task_id=$1 AND kind=$2`, t.ID, reportSelection, id); err != nil {
		return nil, err
	}
	if err := writeSkips(ctx, tx, repoID, sel.Skipped, in.offers); err != nil {
		return nil, err
	}
	if err := claimComments(ctx, tx, repoID, id, sel); err != nil {
		return nil, err
	}
	if err := endTurn(ctx, tx, repoID, d.SelectSeq); err != nil {
		return nil, err
	}
	if err := store.Audit(ctx, tx, auditActor, "aidev.claim", map[string]any{
		"repo_id": repoID, "work_item": id, "issues": sel.Selected, "attempt": req.AttemptID, "agent": req.Agent, "phase": phase,
	}); err != nil {
		return nil, err
	}
	return &ClaimResult{Claimed: true, WorkItemID: id, Branch: devreport.WorkItemBranch(id, phase, 1), Phase: phase}, s.Enqueue(ctx, tx, repoID)
}

// claimLost 处理建工作项被唯一约束挡住（I3：同一个 attempt 并发的重复请求已经领取过）：返回那个工作项。
func claimLost(ctx context.Context, tx pgx.Tx, attemptID int64) (*ClaimResult, error) {
	w, err := queryWorkItem(ctx, tx, `claim_attempt_id=$1`, attemptID)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, fmt.Errorf("%w：同一次执行的领取刚刚变化", errClaimConflict)
	}
	return claimedResult(w), nil
}

func claimedResult(w *WorkItem) *ClaimResult {
	if w == nil {
		return nil
	}
	// 重复的领取请求（I3）：领取后的阶段和分支（之后阶段变了也返回领取时的，Runner 按它开发）
	phase := PhaseImplement
	if w.DesignFirst() {
		phase = PhaseDesign
	}
	return &ClaimResult{Claimed: true, WorkItemID: w.ID, Branch: devreport.WorkItemBranch(w.ID, phase, 1), Phase: phase}
}

// offers 返回交给 attempt 的候选（编号 → 快照）：Offer 给出的，或者领取冲突后给出的新候选。
func (s *Service) offers(ctx context.Context, q store.Querier, attemptID int64) (map[int]aispec.DevIssue, error) {
	var list []aispec.DevIssue
	err := q.QueryRow(ctx, `SELECT candidates FROM dev_offers WHERE attempt_id=$1`, attemptID).Scan(&list)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w：attempt #%d 还没有要过候选", ErrInvalidClaim, attemptID)
	}
	if err != nil {
		return nil, err
	}
	out := make(map[int]aispec.DevIssue, len(list))
	for _, o := range list {
		out[o.Number] = o
	}
	return out, nil
}

func offerNumbers(offers map[int]aispec.DevIssue) []int {
	out := make([]int, 0, len(offers))
	for n := range offers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// verifySelected 逐个读选中的 Issue，返回第一个冲突的原因（没有冲突时为空）和读到的内容（冲突时用来更新任务池）。
// 读取失败（暂时的）返回错误，Runner 重试。
// checkPool 在领取的事务中锁住选中的 Issue 在任务池中的行（按编号），核对它们仍在任务池中、没有 kiln:skip、内容与给出的
// 相同：Issue 事件更新任务池时锁同一行，人撤回授权（去掉 ready、加 skip、关闭）与领取串行——事件先到时这里看到它，
// 领取先提交时事件处理随后找到工作项并终止它。不满足时返回 errClaimConflict。
func checkPool(ctx context.Context, tx pgx.Tx, repoID int64, selected []int, offers map[int]aispec.DevIssue) error {
	rows, err := tx.Query(ctx, `SELECT issue_number, in_pool, digest, $3 = ANY(labels) FROM dev_pool
		WHERE repo_id=$1 AND issue_number = ANY($2) ORDER BY issue_number FOR UPDATE`, repoID, selected, LabelSkip)
	if err != nil {
		return err
	}
	type row struct {
		in, skip bool
		digest   string
	}
	got := map[int]row{}
	var n int
	var r row
	if _, err := pgx.ForEachRow(rows, []any{&n, &r.in, &r.digest, &r.skip}, func() error {
		got[n] = r
		return nil
	}); err != nil {
		return err
	}
	for _, n := range selected {
		r, ok := got[n]
		switch {
		case !ok || !r.in:
			return fmt.Errorf("%w：Issue #%d 已经不在任务池中（关闭了或去掉了 %s）", errClaimConflict, n, LabelReady)
		case r.skip:
			return fmt.Errorf("%w：Issue #%d 有 %s", errClaimConflict, n, LabelSkip)
		case r.digest != offers[n].Digest:
			return fmt.Errorf("%w：Issue #%d 的标题或正文在选题之后变了", errClaimConflict, n)
		}
	}
	return nil
}

func verifySelected(ctx context.Context, f forge.Repo, selected []int, offers map[int]aispec.DevIssue) (string, []forge.IssueContent, error) {
	var fresh []forge.IssueContent
	for _, n := range selected {
		is, err := f.GetIssue(ctx, n)
		if forge.IsRejected(err) {
			return fmt.Sprintf("Issue #%d 无法读取（%v）", n, err), fresh, nil
		}
		if err != nil {
			return "", nil, fmt.Errorf("读取 Issue #%d：%w", n, err)
		}
		fresh = append(fresh, *is)
		switch {
		case !is.Open:
			return fmt.Sprintf("Issue #%d 已关闭", n), fresh, nil
		case !is.HasLabel(LabelReady):
			return fmt.Sprintf("Issue #%d 不再有 %s", n, LabelReady), fresh, nil
		case is.HasLabel(LabelSkip):
			return fmt.Sprintf("Issue #%d 有 %s", n, LabelSkip), fresh, nil
		case IssueDigest(is.Title, is.Body) != offers[n].Digest:
			return fmt.Sprintf("Issue #%d 的标题或正文在选题之后变了", n), fresh, nil
		}
	}
	return "", fresh, nil
}

// reoffer 处理领取冲突：保存选题报告，按读到的最新内容更新任务池（webhook 可能还没到或者丢了），从任务池取最新的候选
// 交给 attempt（覆盖之前的），返回原因、新的候选和进行中的工作项。这一轮选题没有结束。
func (s *Service) reoffer(ctx context.Context, in *claimInput, reason string, fresh []forge.IssueContent, lock lockFunc) (*ClaimResult, error) {
	req, repoID := in.req, in.request.RepoID
	res := &ClaimResult{Reason: reason, Conflict: true}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
			return err
		}
		if err := saveReport(ctx, tx, in.task.ID, req.AttemptID, reportSelection, nil, req.Selection, req.RunnerVersion); err != nil {
			return err
		}
		// 按编号依次锁任务池的行（与 checkPool、SyncPool 相同的加锁顺序）
		slices.SortFunc(fresh, func(a, b forge.IssueContent) int { return a.Number - b.Number })
		for i := range fresh {
			if _, _, err := mirror(ctx, tx, repoID, &fresh[i]); err != nil {
				return err
			}
		}
		var err error
		res.Candidates, res.InProgress, err = s.offerTx(ctx, tx, repoID, req.AttemptID)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("AI 开发：领取冲突", "task", in.task.ID, "attempt", req.AttemptID, "reason", reason, "candidates", len(res.Candidates))
	return res, nil
}

// offerTx 在事务 tx 中从任务池取候选交给 attempt：写入（覆盖）dev_offers，记下交给选题的任务池状态（没有变化就不再选），
// 返回候选和进行中的工作项。
func (s *Service) offerTx(ctx context.Context, tx pgx.Tx, repoID, attemptID int64) ([]aispec.DevIssue, []InProgress, error) {
	st, err := readPool(ctx, tx, repoID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dev_offers (attempt_id, candidates) VALUES ($1,$2)
		ON CONFLICT (attempt_id) DO UPDATE SET candidates=EXCLUDED.candidates, updated_at=now()`, attemptID, store.MustJSON(st.candidates)); err != nil {
		return nil, nil, err
	}
	if err := setExamined(ctx, tx, repoID, st.fingerprint()); err != nil {
		return nil, nil, err
	}
	ip, err := inProgress(ctx, tx, repoID)
	return st.candidates, ip, err
}

// writeSkips 记下选题时认为现在不适合做的候选（内容摘要不变就不再进入候选，重新打上 kiln:ready 清除）。只是不放进
// 这一批的（later）留在任务池中，之后的选题还会看到。
func writeSkips(ctx context.Context, tx pgx.Tx, repoID int64, skipped []devreport.Skip, offers map[int]aispec.DevIssue) error {
	for _, sk := range skipped {
		o, ok := offers[sk.Number]
		if !ok || sk.Later {
			continue
		}
		// 等待依赖（PLAN #31.6）：先登记、标记未复查，执行者在这个事务提交之后读一次依赖的状态；之间关闭的依赖由
		// issues.closed 事件去掉，两者不会互相错过
		wait := slices.Compact(slices.Sorted(slices.Values(sk.WaitFor)))
		if wait == nil {
			wait = []int{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dev_issue_skips (repo_id, issue_number, digest, reason, reconsider, waiting_on, waiting_checked)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (repo_id, issue_number) DO UPDATE SET digest=EXCLUDED.digest, reason=EXCLUDED.reason, reconsider=EXCLUDED.reconsider,
			waiting_on=EXCLUDED.waiting_on, waiting_checked=EXCLUDED.waiting_checked, created_at=now()`,
			repoID, sk.Number, o.Digest, skipNotSelected+"："+sk.Reason, sk.Reconsider, wait, len(wait) == 0); err != nil {
			return err
		}
	}
	return nil
}

// 报告的类别（dev_reports.kind）。
const (
	reportSelection = "selection"
	reportDelivery  = "delivery"
)

// saveReport 保存（或替换）任务的选题、交付报告。选题报告按既有行为覆盖（重试、重新选题会换掉）；
// 交付报告是历史结果，已经存在时只校验内容一致，绝不静默覆盖（审计）。
func saveReport(ctx context.Context, tx pgx.Tx, taskID, attemptID int64, kind string, workItem *int64, report []byte, runnerVersion string) error {
	if kind == reportDelivery {
		// 报告存在 jsonb 中会被规范化，按 jsonb 相等比较（同一份报告重放是幂等的）。
		var same bool
		err := tx.QueryRow(ctx, `SELECT report = $3::jsonb FROM dev_reports WHERE task_id=$1 AND kind=$2`,
			taskID, kind, report).Scan(&same)
		switch {
		case err == nil:
			if !same {
				return fmt.Errorf("任务 %d 已经有不同的交付报告，拒绝覆盖", taskID)
			}
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			// 第一次写入，继续走插入。
		default:
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO dev_reports (task_id, attempt_id, kind, work_item_id, report, runner_version) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (task_id, kind) DO UPDATE SET attempt_id=EXCLUDED.attempt_id, work_item_id=coalesce(EXCLUDED.work_item_id, dev_reports.work_item_id),
		report=EXCLUDED.report, runner_version=EXCLUDED.runner_version, created_at=now()`,
		taskID, attemptID, kind, workItem, report, runnerVersion)
	return err
}

// OfferResult 是 Offer 的结果：Wait 非 0 时等这么久再要；否则 Candidates 是候选（为空时没有可做的），InProgress 是
// 同一个仓库中未结束的工作项。
type OfferResult struct {
	Wait       time.Duration
	Candidates []aispec.DevIssue
	InProgress []InProgress
}

// OfferCandidates 给选题任务的 attempt 候选（PLAN #26.10 第 2 步）：Runner 准备好 clone 和测试服务、启动 Agent 之前来要，候选从
// 任务池中取，越晚确定越完整。任务池在 poolSettle 内变化过（批量打 label 时 webhook 还在陆续到达）时让 Runner 等一会儿
// 再要，从 attempt 开始算最多等 maxSettleWait。同一个 attempt 已经要过时返回同一批（Runner 重试）。
func (s *Service) OfferCandidates(ctx context.Context, t *ai.Task, attemptID int64) (*OfferResult, error) {
	if _, err := selectSpec(t, attemptID); err != nil {
		return nil, err
	}
	r, err := ai.GetRequest(ctx, s.db, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	lock := attemptLock(t, attemptID)
	res := &OfferResult{}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
			return err
		}
		var stored []aispec.DevIssue
		err := tx.QueryRow(ctx, `SELECT candidates FROM dev_offers WHERE attempt_id=$1`, attemptID).Scan(&stored)
		if err == nil {
			res.Candidates = stored
			res.InProgress, err = inProgress(ctx, tx, r.RepoID)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var quietSec, waitedSec float64
		if err := tx.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - (SELECT dev_pool_changed_at FROM ai_repo_state WHERE repo_id=$1))::float8, $3),
			extract(epoch FROM now() - a.started_at)::float8 FROM ai_attempts a WHERE a.id=$2`,
			r.RepoID, attemptID, poolSettle.Seconds()).Scan(&quietSec, &waitedSec); err != nil {
			return err
		}
		quiet, waited := seconds(quietSec), seconds(waitedSec)
		if quiet < poolSettle && waited < maxSettleWait {
			res.Wait = min(poolSettle-quiet, maxSettleWait-waited)
			return nil
		}
		res.Candidates, res.InProgress, err = s.offerTx(ctx, tx, r.RepoID, attemptID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if res.Wait == 0 {
		s.log.Info("AI 开发：给出候选", "task", t.ID, "attempt", attemptID, "candidates", len(res.Candidates), "in_progress", len(res.InProgress))
	}
	return res, nil
}

// seconds 把秒数转换为 time.Duration。
func seconds(sec float64) time.Duration { return time.Duration(sec * float64(time.Second)) }

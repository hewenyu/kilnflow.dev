package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 开发（PLAN #26.15）：/ai-dev 列出工作项和暂停了新领取的仓库（只看一个仓库时还有它的任务池），管理员可以从 GitHub
// 全量同步任务池、暂停 / 恢复仓库的新领取、终止工作项、让受阻的工作项再返修一轮；/ai-dev/{id} 是一个工作项的全部记录（Issue、选题和交付报告及其适用范围、
// 评论意图、attempt 分支）。操作由 admin.Service 交给 internal/server/aidev 执行。

// aiDevPath 是 AI 开发页的地址。
const aiDevPath = "/ai-dev"

// aiDevURL 返回 AI 开发页的地址；repo 为空时是全部仓库。
func aiDevURL(repo string) string {
	if repo == "" {
		return aiDevPath
	}
	return aiDevPath + "?" + url.Values{"repo": {repo}}.Encode()
}

// aiDevItemURL 返回工作项详情页的地址。
func aiDevItemURL(id int64) string {
	return aiDevPath + "/" + strconv.FormatInt(id, 10)
}

// aiDevIssue 是工作项中的一个 Issue。
type aiDevIssue struct {
	Number int
	Title  string
	// Resolution 是交付报告给出的完成度（full、partial、none），还没有交付时为空。
	Resolution string
	// Active 表示 Issue 仍属于这个工作项；Closed 表示 Issue 已经关闭。
	Active, Closed bool
}

// aiDevItem 是一个工作项（aidev.View 中页面用到的字段）。
type aiDevItem struct {
	ID    int64
	Repo  string
	State string
	// Active 表示工作项还没有结束（终止按钮）；Retryable 表示受阻，可以再返修一轮。
	Active, Retryable bool
	Issues            []aiDevIssue
	// PR 是工作项的 PR 号，还没有开 PR 时为 0。
	PR int
	// Branch 是本阶段、本轮的正式分支；HeadSHA 是已知的正式分支 / PR head。
	Branch, HeadSHA string
	// FixRounds 是本阶段、本轮因为审计、CI 没有通过返修的轮数，ConflictRounds 是冲突返修的轮数（各自的上限都是 MaxFixRounds）。
	FixRounds, ConflictRounds, MaxFixRounds int
	Agent                                   string
	// Phase 是阶段（design | implement，PLAN #31.3）；DesignFirst 表示先设计（有设计阶段），否则一个 PR 完成；DesignPR 是
	// 已合并的设计 PR（没有时为 0）；ImplRound 是实现的轮次（部分完成之后加一）。
	Phase       string
	DesignFirst bool
	DesignPR    int
	ImplRound   int
	// NextRound 表示受阻在新一轮开始之前（部分完成的实现 PR 已合并）：重试开始下一轮实现，而不是再返修一轮。
	NextRound bool
	// Hold 表示交付需要人确认（敏感路径、提交说明里的关闭关键字等，PR 加了 kiln:hold，原因在 PR 描述中）；RequirementsChanged 表示领取后 Issue 内容变了。
	Hold, RequirementsChanged bool
	// Reason 是受阻、终止的原因，或者当前在等什么。
	Reason           string
	Created, Updated time.Time
	Finished         *time.Time
	// URL 是详情页的地址。
	URL string
}

func newAIDevItem(v *aidev.View) aiDevItem {
	it := aiDevItem{
		ID: v.ID, Repo: v.Repo, State: v.State, Active: aidev.Active(v.State), Retryable: v.State == aidev.StateBlocked,
		Branch: v.Branch(), HeadSHA: v.HeadSHA, FixRounds: v.RoundCheckFixes(), ConflictRounds: v.RoundConflicts, MaxFixRounds: v.MaxFixRounds,
		Agent: v.Agent,
		Phase: v.Phase, DesignFirst: v.DesignFirst(), ImplRound: v.ImplRound, NextRound: v.State == aidev.StateBlocked && !v.RoundRan,
		Hold: v.HoldLabel, RequirementsChanged: v.RequirementsChanged, Reason: v.Reason,
		Created: v.CreatedAt, Updated: v.UpdatedAt, Finished: v.FinishedAt, URL: aiDevItemURL(v.ID),
		Issues: make([]aiDevIssue, 0, len(v.Issues)),
	}
	if v.PRNumber != nil {
		it.PR = *v.PRNumber
	}
	if v.DesignPR != nil {
		it.DesignPR = *v.DesignPR
	}
	for _, is := range v.Issues {
		it.Issues = append(it.Issues, aiDevIssue{Number: is.Number, Title: is.Title, Resolution: is.Resolution, Active: is.Active, Closed: is.Closed})
	}
	return it
}

// aiDevOccupancy 是仓库的 AI 开发占用（规则见 aidev.Occupancy）：Occupant 是占用者（没有被占用时为 0），Phase、State 是它的
// 阶段和状态（名称由前端按语言显示），Reason 是它记下的原因或在等什么（原样显示）；Waiting 是排在它之后的未结束的
// 工作项（升级前留下的）。
type aiDevOccupancy struct {
	Occupant     int64
	Phase, State string
	Reason       string
	Waiting      []int64
}

func newAIDevOccupancy(o *aidev.Occupancy) *aiDevOccupancy {
	out := &aiDevOccupancy{Waiting: o.Waiting}
	if w := o.Occupant; w != nil {
		out.Occupant, out.Phase, out.State, out.Reason = w.ID, w.Phase, w.State, w.Reason
	}
	if out.Waiting == nil {
		out.Waiting = []int64{}
	}
	return out
}

// aiDevPage 是 AI 开发页的数据。
type aiDevPage struct {
	// Repo 是只看的仓库（owner/name）；为空表示全部仓库。
	Repo  string
	Items []aiDevItem
	// Paused 是暂停了新领取的仓库（只看一个仓库时最多是它自己）。
	Paused []string
	// Pool 是只看的仓库的任务池（有 kiln:ready 的打开的 Issue 及其情况）；看全部仓库时为 nil。
	Pool *aidev.PoolView
	// Occupancy 是只看的仓库的占用（PLAN #26.9 I14：同一个仓库按工作项串行）；看全部仓库时为 nil。
	Occupancy *aiDevOccupancy
	// Repos 是"同步任务池"表单的仓库选项（Kiln 中启用的仓库）；只有管理员有。
	Repos []string
	// Ready 是授权 Issue 进入开发候选的 label；HoldLabel 是交付需要人确认时 PR 加的 label。
	Ready, HoldLabel string
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
}

func (d *Dashboard) pageAIDev(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	p := &aiDevPage{
		Repo: strings.TrimSpace(r.URL.Query().Get("repo")), Ready: aidev.LabelReady, HoldLabel: mergequeue.HoldLabel, GitHub: d.githubWeb(),
	}
	if p.Repo != "" {
		repo, err := store.FindRepo(ctx, d.db, p.Repo)
		// 写的不是 owner/name 时 store.FindRepo 返回的不是 ErrRepoNotFound，同样不可能是任何仓库
		if errors.Is(err, store.ErrRepoNotFound) || (err != nil && !admin.ValidRepoFullName(p.Repo)) {
			d.notFoundText(w, r, message(msgAIDevRepoNotFound, strArg(argRepo, p.Repo)))
			return
		}
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		if err := d.loadAIDevRepo(ctx, p, repo); err != nil {
			d.serverError(w, r, err)
			return
		}
	} else {
		var err error
		if p.Paused, err = d.pausedRepos(ctx); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	list, err := d.admin.ListWorkItems(ctx, p.Repo, 0)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.Items = make([]aiDevItem, 0, len(list))
	for _, v := range list {
		p.Items = append(p.Items, newAIDevItem(v))
	}
	if se.Admin() {
		if p.Repos, err = d.activeRepos(ctx); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	title := message(msgAIDevTitle)
	if p.Repo != "" {
		title = message(msgAIDevTitleRepo, strArg(argRepo, p.Repo))
	}
	d.renderPage(w, r, "ai_dev", title, p, http.StatusOK)
}

// loadAIDevRepo 填写只看一个仓库时的数据：暂停状态（只有它自己的）、任务池和仓库占用。
func (d *Dashboard) loadAIDevRepo(ctx context.Context, p *aiDevPage, repo *store.Repo) error {
	p.Repo = repo.FullName()
	paused, err := aidev.Paused(ctx, d.db, repo.ID)
	if err != nil {
		return err
	}
	if paused {
		p.Paused = []string{p.Repo}
	}
	if p.Pool, err = aidev.GetPool(ctx, d.db, repo.ID); err != nil {
		return err
	}
	occ, err := aidev.RepoOccupancy(ctx, d.db, repo.ID)
	if err != nil {
		return err
	}
	p.Occupancy = newAIDevOccupancy(occ)
	return nil
}

// pausedRepos 返回暂停了 AI 开发新领取的仓库（按名称）。
func (d *Dashboard) pausedRepos(ctx context.Context) ([]string, error) {
	rows, err := d.db.Query(ctx, `SELECT p.owner || '/' || p.name FROM repos p JOIN ai_repo_state s ON s.repo_id=p.id WHERE s.dev_paused ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// aiDevReport 是一份选题或交付报告。报告中的文字都是 Agent 自报的（页面上标明），适用范围是 Runner / Agent 版本和模型
// （PLAN #26.14，拿不到时为空，页面显示"未知"）。
type aiDevReport struct {
	// Kind 是 selection 或 delivery。
	Kind   string
	TaskID int64
	// TaskURL 是产生报告的 AI 任务的详情页。
	TaskURL string
	// Selection、Delivery 是解析后的报告（按 Kind 二选一）；解析不了时两者都为 nil，Raw 是原文。
	Selection *devreport.Selection
	Delivery  *devreport.Delivery
	Raw       string
	// RunnerVersion、AgentVersion、Model 是报告的适用范围。
	RunnerVersion, AgentVersion, Model string
	Created                            time.Time
}

// reportKindSelection、reportKindDelivery 是 dev_reports.kind。
const (
	reportKindSelection = "selection"
	reportKindDelivery  = "delivery"
)

func newAIDevReport(rep *aidev.Report) aiDevReport {
	v := aiDevReport{
		Kind: rep.Kind, TaskID: rep.TaskID, TaskURL: aiTaskURL(rep.TaskID),
		RunnerVersion: rep.RunnerVersion, AgentVersion: rep.AgentVersion, Model: rep.Model, Created: rep.CreatedAt,
	}
	// 报告在领取、交付时已经校验过；这里只为显示解析，旧版本或解析不了的报告显示原文
	switch rep.Kind {
	case reportKindSelection:
		var s devreport.Selection
		if json.Unmarshal(rep.Report, &s) == nil {
			v.Selection = &s
		}
	case reportKindDelivery:
		var dl devreport.Delivery
		if json.Unmarshal(rep.Report, &dl) == nil {
			v.Delivery = &dl
		}
	}
	if v.Selection == nil && v.Delivery == nil {
		v.Raw = string(rep.Report)
	}
	return v
}

// aiDevComment 是一条评论意图（C19：没有幂等键，至多发一次；unknown 表示结果不明，不自动重发）。
type aiDevComment struct {
	// Number 是 Issue 或 PR 编号。
	Number int
	// Kind 是 claimed、blocked、dispositions、stopped 或 no_change。
	Kind string
	// State 是 pending、attempted、done、unknown 或 failed。
	State   string
	Error   string
	Updated time.Time
}

// aiDevBranch 是一个 attempt 分支（交付或 checkpoint）。
type aiDevBranch struct {
	Branch, SHA string
	// Kind 是 delivery 或 checkpoint。
	Kind    string
	Created time.Time
	// Deleted 是删除的时间；还没有删除时为 nil，Expires 是保留期满（届时删除）的时间。
	Deleted *time.Time
	Expires *time.Time
}

// aiDevItemPage 是工作项详情页的数据。
type aiDevItemPage struct {
	Item aiDevItem
	// BaseRef、BaseSHA 是目标分支和领取时它的 head（开发的起点）。
	BaseRef, BaseSHA string
	// Plan 是选题时 Agent 给出的计划（Agent 自报）。
	Plan string
	// TaskURL 是当前（或最近一次）为它执行的 AI 任务的详情页，没有时为空。
	TaskURL string
	// DeliveryBranch、DeliverySHA 是待推进的交付（等待推进时非空）；FixedHead 是已经为哪个 head 建过返修。
	DeliveryBranch, DeliverySHA, FixedHead string
	Reports                                []aiDevReport
	Comments                               []aiDevComment
	Branches                               []aiDevBranch
	// RepoURL 是这个仓库的 AI 开发页。
	RepoURL string
	// Ready、HoldLabel 同 aiDevPage。
	Ready, HoldLabel string
	// DesignBasis 是这个工作项冻结的设计依据（只有元数据）；RelatedTasks 是关联任务与依据关系。
	DesignBasis  *designBasisView
	RelatedTasks []relatedTaskView
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
}

func (d *Dashboard) pageAIDevItem(w http.ResponseWriter, r *http.Request, _ *session) {
	id := pathID(r)
	ctx := r.Context()
	detail, err := d.admin.WorkItem(ctx, id)
	if errors.Is(err, aidev.ErrNotFound) {
		d.notFoundText(w, r, message(msgAIDevItemNotFound, intArg(argID, id)))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &aiDevItemPage{
		Item: newAIDevItem(&detail.View), BaseRef: detail.BaseRef, BaseSHA: detail.BaseSHA, Plan: detail.Plan,
		DeliveryBranch: detail.DeliveryBranch, DeliverySHA: detail.DeliverySHA, FixedHead: detail.FixedHead,
		Reports:  make([]aiDevReport, 0, len(detail.Reports)),
		Comments: make([]aiDevComment, 0, len(detail.Comments)),
		Branches: make([]aiDevBranch, 0, len(detail.Branches)),
		RepoURL:  aiDevURL(detail.Repo), Ready: aidev.LabelReady, HoldLabel: mergequeue.HoldLabel, GitHub: d.githubWeb(),
	}
	if detail.TaskID != nil {
		p.TaskURL = aiTaskURL(*detail.TaskID)
	}
	for i := range detail.Reports {
		p.Reports = append(p.Reports, newAIDevReport(&detail.Reports[i]))
	}
	for _, c := range detail.Comments {
		p.Comments = append(p.Comments, aiDevComment{Number: c.Number, Kind: c.Kind, State: c.State, Error: c.Error, Updated: c.UpdatedAt})
	}
	for _, b := range detail.Branches {
		v := aiDevBranch{Branch: b.Branch, SHA: b.SHA, Kind: b.Kind, Created: b.CreatedAt, Deleted: b.DeletedAt}
		if b.DeletedAt == nil {
			exp := b.CreatedAt.Add(d.admin.AIDevBranchTTL())
			v.Expires = &exp
		}
		p.Branches = append(p.Branches, v)
	}
	if p.DesignBasis, err = d.workItemDesignBasis(ctx, id); err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.RelatedTasks, err = d.relatedTasks(ctx, id); err != nil {
		d.serverError(w, r, err)
		return
	}
	d.renderPage(w, r, "ai_dev_item", message(msgAIDevItemTitle, intArg(argID, id), strArg(argRepo, detail.Repo)), p, http.StatusOK)
}

// postAIDevSync 从 GitHub 全量同步仓库的任务池（webhook 可能丢失），回到这个仓库的 AI 开发页。
func (d *Dashboard) postAIDevSync(w http.ResponseWriter, r *http.Request, se *session) {
	repo := strings.TrimSpace(r.PostFormValue("repo"))
	back := aiDevURL(repo)
	if repo == "" {
		d.flashMsg(w, r, back, message(msgAIDevSyncNoRepo))
		return
	}
	res, err := d.admin.SyncAIDev(r.Context(), repo, se.Login)
	if err != nil {
		d.flashMsg(w, r, back, d.aiDevFailure(r, err, aiDevTarget{repo: repo}, msgAIDevSyncFailed, msgAIDevSyncFailedRef))
		return
	}
	id := msgAIDevSynced
	if !res.Complete {
		id = msgAIDevSyncedPartial
	}
	d.flashMsg(w, r, back, message(id, strArg(argRepo, repo), intArg(argCount, int64(res.Listed)), strArg(argLabel, aidev.LabelReady),
		intArg(argChanged, int64(res.Changed)), intArg(argCandidates, int64(res.Candidates))))
}

// postAIDevPause、postAIDevResume 暂停、恢复仓库的新领取，回到这个仓库的 AI 开发页。
func (d *Dashboard) postAIDevPause(w http.ResponseWriter, r *http.Request, se *session) {
	d.setAIDevPaused(w, r, se, true)
}

func (d *Dashboard) postAIDevResume(w http.ResponseWriter, r *http.Request, se *session) {
	d.setAIDevPaused(w, r, se, false)
}

func (d *Dashboard) setAIDevPaused(w http.ResponseWriter, r *http.Request, se *session, paused bool) {
	repo := strings.TrimSpace(r.PostFormValue("repo"))
	back := aiDevURL(repo)
	// 暂停、恢复的提示各一组：没有选仓库、失败（原样的说明 / 错误编号）、成功
	noRepo, failed, failedRef, done := msgAIDevResumeNoRepo, msgAIDevResumeFailed, msgAIDevResumeFailRef, msgAIDevResumed
	if paused {
		noRepo, failed, failedRef, done = msgAIDevPauseNoRepo, msgAIDevPauseFailed, msgAIDevPauseFailedRef, msgAIDevPaused
	}
	if repo == "" {
		d.flashMsg(w, r, back, message(noRepo))
		return
	}
	if err := d.admin.PauseAIDev(r.Context(), repo, paused, se.Login); err != nil {
		d.flashMsg(w, r, back, d.aiDevFailure(r, err, aiDevTarget{repo: repo}, failed, failedRef))
		return
	}
	d.flashMsg(w, r, back, message(done, strArg(argRepo, repo)))
}

// aiDevFormID 读取终止、重试表单的工作项 ID；无效时返回 0。
func aiDevFormID(r *http.Request) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("id")), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// postAIDevStop 终止工作项（取消执行中的 job，释放 Issue；PR 保持原样），回到工作项详情页。
func (d *Dashboard) postAIDevStop(w http.ResponseWriter, r *http.Request, se *session) {
	id := aiDevFormID(r)
	if id == 0 {
		d.flashMsg(w, r, aiDevPath, message(msgAIDevStopBadID))
		return
	}
	back := aiDevItemURL(id)
	if err := d.admin.StopWorkItem(r.Context(), id, se.Login); err != nil {
		d.flashMsg(w, r, back, d.aiDevFailure(r, err, aiDevTarget{id: id}, msgAIDevStopFailed, msgAIDevStopFailedRef))
		return
	}
	d.flashMsg(w, r, back, message(msgAIDevStopped, intArg(argID, id)))
}

// postAIDevRetry 让受阻的工作项再返修一轮（不受轮数限制），回到工作项详情页。
func (d *Dashboard) postAIDevRetry(w http.ResponseWriter, r *http.Request, se *session) {
	id := aiDevFormID(r)
	if id == 0 {
		d.flashMsg(w, r, aiDevPath, message(msgAIDevRetryBadID))
		return
	}
	back := aiDevItemURL(id)
	if err := d.admin.RetryWorkItem(r.Context(), id, se.Login); err != nil {
		d.flashMsg(w, r, back, d.aiDevFailure(r, err, aiDevTarget{id: id}, msgAIDevRetryFailed, msgAIDevRetryFailedRef))
		return
	}
	d.flashMsg(w, r, back, message(msgAIDevRetried, intArg(argID, id)))
}

// aiDevTarget 是 AI 开发操作的对象：工作项（终止、重试）或仓库（同步、暂停、恢复）。
type aiDevTarget struct {
	id   int64
	repo string
}

// args 返回操作的提示中共同的参数（工作项编号或仓库名）。
func (t aiDevTarget) args() []msgArg {
	if t.repo != "" {
		return []msgArg{strArg(argRepo, t.repo)}
	}
	return []msgArg{intArg(argID, t.id)}
}

// aiDevFailure 返回 AI 开发操作失败后的提示：工作项、仓库不存在和状态不允许时给出可以怎么做；其余按 failureFlash
// （withReason 带原样的说明，withRef 带错误编号）。
func (d *Dashboard) aiDevFailure(r *http.Request, err error, t aiDevTarget, withReason, withRef msgID) uiText {
	switch {
	case errors.Is(err, aidev.ErrNotFound):
		return message(msgAIDevItemMissing, intArg(argID, t.id))
	case errors.Is(err, store.ErrRepoNotFound), errors.Is(err, aidev.ErrRepoNotFound):
		return message(msgAIDevRepoMissing, strArg(argRepo, t.repo))
	case errors.Is(err, aidev.ErrNotRetryable):
		return message(msgAIDevNotRetryable, intArg(argID, t.id))
	case errors.Is(err, aidev.ErrFinished):
		return message(msgAIDevFinished, intArg(argID, t.id))
	}
	return d.failureFlash(r, err, withReason, withRef, t.args()...)
}

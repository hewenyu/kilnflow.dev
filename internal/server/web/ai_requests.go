package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 请求和 AI 任务的页面（PLAN #30.9）：AI 的数据只在 AI 的表中，与 CI 的 run、job 分开显示。
//   - /ai/requests：AI 请求列表（审计、开发），按仓库、类型、PR、状态筛选；
//   - /ai/requests/{id}：一个请求的各平台任务、重跑关系，取消和重跑（只有审计可以重跑）；
//   - /ai/tasks/{id}：一个任务的每次 attempt、实时日志、审计报告或开发报告。
//
// 迁移来的 AI 记录保留了旧的外部标识（PLAN #30.5）：旧链接 /runs/<id>、/jobs/<id> 找不到 CI 记录时跳转到对应的 AI 页面。

// aiRequestsPath 是 AI 请求列表的地址。
const aiRequestsPath = "/ai/requests"

// aiRequestURL 返回 AI 请求详情页的地址。
func aiRequestURL(id int64) string { return aiRequestsPath + "/" + strconv.FormatInt(id, 10) }

// aiTaskURL 返回 AI 任务详情页的地址。
func aiTaskURL(id int64) string { return "/ai/tasks/" + strconv.FormatInt(id, 10) }

// aiRequestView 是页面上的一个 AI 请求。
type aiRequestView struct {
	ID int64
	// Type 是 review 或 develop，名称由前端按语言显示。
	Type            string
	Repo            string
	Event, Ref, SHA string
	Trigger, Sender string
	State           string
	// PRNumber 是审计的 PR（手动审计、开发请求为 0）。
	PRNumber int
	// WorkItemURL 是关联的 AI 开发工作项的详情页（没有时为空）。
	WorkItemID  int64
	WorkItemURL string
	// DevMode、DevRound 是开发请求的方式（select 选题、fix 返修）和返修轮次。
	DevMode  string
	DevRound int
	// RerunOf 是被重跑的请求（不是重跑时为 0）。
	RerunOf     int64
	ConfigError string
	Warnings    string
	Created     time.Time
	Finished    *time.Time
	// Purged 是日志按保留策略清理的时间；没有清理时为 nil。
	Purged *time.Time
}

func newAIRequestView(v *admin.AIRequestView) aiRequestView {
	out := aiRequestView{
		ID: v.ID, Type: v.Type, Repo: v.Repo, Event: v.Event, Ref: v.Ref, SHA: v.SHA,
		Trigger: v.Trigger, Sender: v.Sender, State: string(v.State), PRNumber: v.PRNumber, WorkItemID: v.WorkItemID,
		DevMode: v.DevMode, DevRound: v.DevRound, ConfigError: deref(v.ConfigError), Warnings: deref(v.Warnings),
		Created: v.CreatedAt, Finished: v.FinishedAt, Purged: v.PurgedAt,
	}
	if v.WorkItemID != 0 {
		out.WorkItemURL = aiDevItemURL(v.WorkItemID)
	}
	if v.RerunOf != nil {
		out.RerunOf = *v.RerunOf
	}
	return out
}

// aiTaskView 是页面上的一个 AI 任务。
type aiTaskView struct {
	ID, RequestID int64
	Key           string
	DisplayName   string
	// Platforms 是可以执行这个任务的平台（一组平台时多个）；Platform 是当前 attempt 实际执行的平台（还没有领取时为空）。
	Platforms []string
	Platform  string
	Agent     string
	// State 是任务的状态，名称由前端按语言显示；Reason 是调度和执行记下的原因（原样显示）。
	State  string
	Reason string
	// ReusedFrom 是沿用结果的来源任务（没有沿用时为 0）。
	ReusedFrom      int64
	AttemptCount    int
	Runner          string
	CancelRequested bool
	Created         time.Time
	Started         *time.Time
	Finished        *time.Time
}

// newAITaskView 返回任务的页面数据；at 是当前 attempt 的执行信息（还没有领取时为 nil）。
func newAITaskView(t *ai.Task, at *admin.AITaskAttempt) aiTaskView {
	v := aiTaskView{
		ID: t.ID, RequestID: t.RequestID, Key: t.Key, DisplayName: t.DisplayName, Platforms: t.Platforms, Agent: t.Agent,
		State: string(t.State), Reason: deref(t.Reason), AttemptCount: t.AttemptCount,
		CancelRequested: t.CancelRequested, Created: t.CreatedAt, Started: t.StartedAt, Finished: t.FinishedAt,
	}
	if at != nil {
		v.Runner, v.Platform = at.Runner, at.Platform
	}
	if t.ReusedFrom != nil {
		v.ReusedFrom = *t.ReusedFrom
	}
	return v
}

// ---- 列表 ----

// aiRequestFilter 是 AI 请求列表的筛选条件（已校验，无效的取值视为不筛选）。
type aiRequestFilter struct {
	Repo, Type, State string
	PR                int
}

// aiRequestsPage 是 AI 请求列表的数据。
type aiRequestsPage struct {
	Requests []aiRequestView
	Filter   aiRequestFilter
	// Repos 是可以筛选的仓库；Types、States 是类型和状态的选项（Label 是消息）。
	Repos  []string
	Types  []textOption
	States []textOption
	// Limit 是列出的最大条数（达到时页面提示还有更早的请求）。
	Limit int
}

// aiRequestsLimit 是 AI 请求列表一次列出的条数。
const aiRequestsLimit = 100

func (d *Dashboard) pageAIRequests(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	q := r.URL.Query()
	p := &aiRequestsPage{Types: aiTypeOptions(), States: aiRequestStateOptions(), Limit: aiRequestsLimit}
	rows, err := d.db.Query(ctx, `SELECT p.owner || '/' || p.name FROM repos p WHERE EXISTS (SELECT 1 FROM ai_requests q WHERE q.repo_id=p.id) ORDER BY 1`)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.Repos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		d.serverError(w, r, err)
		return
	}
	if v := strings.TrimSpace(q.Get("repo")); v != "" {
		for _, repo := range p.Repos {
			if repo == v {
				p.Filter.Repo = v
			}
		}
	}
	if v := q.Get("type"); textOptionValid(p.Types, v) {
		p.Filter.Type = v
	}
	if v := q.Get("state"); textOptionValid(p.States, v) {
		p.Filter.State = v
	}
	if n, err := strconv.Atoi(q.Get("pr")); err == nil && n > 0 {
		p.Filter.PR = n
	}
	list, err := d.admin.ListAIRequests(ctx, admin.AIRequestFilter{
		Repo: p.Filter.Repo, Type: p.Filter.Type, State: p.Filter.State, PR: p.Filter.PR, Limit: aiRequestsLimit,
	})
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.Requests = make([]aiRequestView, 0, len(list))
	for _, v := range list {
		p.Requests = append(p.Requests, newAIRequestView(v))
	}
	d.renderPage(w, r, "ai_requests", message(msgAIRequestsTitle), p, http.StatusOK)
}

// ---- 请求详情 ----

// aiFamilyItem 是同一个请求族（首次请求和它的重跑）中的一个请求。
type aiFamilyItem struct {
	ID      int64
	State   string
	Created time.Time
}

// aiRequestPage 是 AI 请求详情页的数据。
type aiRequestPage struct {
	Request aiRequestView
	Tasks   []aiTaskView
	// Family 是同一个请求族中的全部请求（按 ID 排序），只有一个时为空。
	Family []aiFamilyItem
	// Terminal 表示请求已经结束；CanRerun 表示可以重跑（已结束的审计请求）；HasFailed 表示有没有成功的任务（可以"只重跑失败的"）。
	Terminal, CanRerun, HasFailed bool
	// PRURL 是 PR 在 GitHub 上的地址（不是 PR 审计或不是 github.com 时为空）。
	PRURL string
	// DesignBasis 是这次请求固定的设计依据（只有元数据）。
	DesignBasis *requestDocsPageView
}

func (d *Dashboard) pageAIRequest(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	det, err := d.admin.AIRequest(ctx, pathID(r))
	if err != nil {
		d.adminPageError(w, r, err)
		return
	}
	p := &aiRequestPage{Request: newAIRequestView(det.AIRequestView), Terminal: det.State.Terminal(), Tasks: make([]aiTaskView, 0, len(det.Tasks))}
	p.CanRerun = p.Terminal && det.Type == agentcfg.TaskReview
	for _, t := range det.Tasks {
		p.Tasks = append(p.Tasks, newAITaskView(t, det.Attempts[t.ID]))
		if t.State != ai.TaskSucceeded {
			p.HasFailed = true
		}
	}
	if web := d.githubWeb(); web != "" && det.PRNumber != 0 {
		p.PRURL = fmt.Sprintf("%s/%s/pull/%d", web, det.Repo, det.PRNumber)
	}
	family, err := ai.QueryRequests(ctx, d.db, `family_id=$1 ORDER BY id`, det.FamilyID)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if len(family) > 1 {
		for _, f := range family {
			p.Family = append(p.Family, aiFamilyItem{ID: f.ID, State: string(f.State), Created: f.CreatedAt})
		}
	}
	if p.DesignBasis, err = d.requestDesignBasis(ctx, det.ID); err != nil {
		d.serverError(w, r, err)
		return
	}
	d.renderPage(w, r, "ai_request", aiRequestTitle(det.Type, det.ID), p, http.StatusOK)
}

// ---- 任务详情 ----

// aiAttemptView 是 AI 任务的一次 attempt。
type aiAttemptView struct {
	ID, Epoch int64
	// N 是序号：第 N 次 attempt。
	N int
	// Runner、Platform 是领取的节点和它上报的平台（实际执行的平台）。
	Runner, Platform    string
	Outcome, ErrorClass string
	Message             string
	// RunnerVersion、AgentVersion、AgentModel 是执行时的连接器、Agent 版本和模型（拿不到时为空）。
	RunnerVersion, AgentVersion, AgentModel string
	// Orca 是这次执行在 Orca 中的关联（PLAN #33.4）；Stage、Text 是心跳上报的进度。
	Orca              ai.OrcaBinding
	Stage             string
	Text              string
	Started, Finished int64
	// Current 表示这是任务当前的 attempt。
	Current bool
}

func (d *Dashboard) aiAttemptViews(ctx context.Context, t *ai.Task) ([]aiAttemptView, error) {
	rows, err := d.db.Query(ctx, `SELECT a.id, a.epoch, coalesce(r.name, a.runner_name, ''), a.platform, coalesce(a.outcome, ''), coalesce(a.error_class, ''),
		coalesce(a.message, ''), a.runner_version, a.agent_version, a.agent_model, a.orca, a.progress,
		extract(epoch from a.started_at)::bigint, coalesce(extract(epoch from a.finished_at)::bigint, 0)
		FROM ai_attempts a LEFT JOIN runners r ON r.id=a.runner_id WHERE a.task_id=$1 ORDER BY a.epoch`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []aiAttemptView
	for rows.Next() {
		var v aiAttemptView
		var orca, progress []byte
		if err := rows.Scan(&v.ID, &v.Epoch, &v.Runner, &v.Platform, &v.Outcome, &v.ErrorClass, &v.Message, &v.RunnerVersion, &v.AgentVersion,
			&v.AgentModel, &orca, &progress, &v.Started, &v.Finished); err != nil {
			return nil, err
		}
		a := &ai.Attempt{ID: v.ID, Orca: orca, Progress: progress}
		if v.Orca, err = a.DecodeOrca(); err != nil {
			return nil, err
		}
		pr, err := a.DecodeProgress()
		if err != nil {
			return nil, err
		}
		v.Stage, v.Text = pr.Stage, pr.Text
		v.N = len(list) + 1
		v.Current = t.CurrentAttemptID != nil && v.ID == *t.CurrentAttemptID
		list = append(list, v)
	}
	return list, rows.Err()
}

// aiReviewView 是 AI 审计任务的结果。
type aiReviewView struct {
	Agent   string
	Verdict string
	// Report 是保存的完整报告（结论、摘要、问题、验证命令、未验证的部分）。
	Report *aireport.Report
	// PRNumber 是被审计的 PR（手动审计为 0）。
	PRNumber int
	HeadSHA  string
	BaseSHA  string
	// ReviewState 是 PR review 的发布状态：none（不发布）、pending、publishing、published、failed、unknown（review.State*）；
	// ReviewURL 是已发布的 review。
	ReviewState string
	ReviewURL   string
	ReviewError string
	Created     time.Time
}

// aiReviewViewFor 返回审计任务的结果；没有时为 nil。
func (d *Dashboard) aiReviewViewFor(ctx context.Context, taskID int64, repo string) (*aiReviewView, error) {
	r, err := review.GetReport(ctx, d.db, taskID)
	if err != nil || r == nil {
		return nil, err
	}
	v := &aiReviewView{Agent: r.Agent, Verdict: r.Verdict, HeadSHA: r.HeadSHA, BaseSHA: r.BaseSHA, ReviewState: r.ReviewState, Created: r.CreatedAt}
	if v.Report, err = aireport.Parse(r.Report); err != nil {
		return nil, fmt.Errorf("AI 任务 #%d 保存的审计报告无效：%w", taskID, err)
	}
	if r.PRNumber != nil {
		v.PRNumber = *r.PRNumber
		if r.GitHubReviewID != nil {
			v.ReviewURL = prReviewURL(d.githubWeb(), repo, *r.PRNumber, *r.GitHubReviewID)
		}
	}
	v.ReviewError = deref(r.ReviewError)
	return v, nil
}

// prReviewURL 返回 PR review 在 GitHub 网页上的地址；web 为空（不是 github.com，见 githubWeb）时返回空，页面不显示链接。
func prReviewURL(web, repo string, pr int, reviewID int64) string {
	if web == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/pull/%d#pullrequestreview-%d", web, repo, pr, reviewID)
}

// aiDevelopView 是 AI 开发任务保存的报告（PLAN #26.15）：没有领取的选题（没有选中、dry_run、领取失败）不属于任何工作项，
// 选题的计划只能在这里查看。
type aiDevelopView struct {
	Reports []aiDevReport
	// WorkItemURL 是这次执行领取或返修的工作项的详情页；没有工作项时为空。
	WorkItemURL string
}

func (d *Dashboard) aiDevelopViewFor(ctx context.Context, taskID int64) (*aiDevelopView, error) {
	reports, item, err := aidev.TaskReports(ctx, d.db, taskID)
	if err != nil {
		return nil, err
	}
	v := &aiDevelopView{Reports: make([]aiDevReport, 0, len(reports))}
	for i := range reports {
		v.Reports = append(v.Reports, newAIDevReport(&reports[i]))
	}
	if item != nil {
		v.WorkItemURL = aiDevItemURL(*item)
	}
	return v, nil
}

// aiTaskPage 是 AI 任务详情页的数据。
type aiTaskPage struct {
	Task    aiTaskView
	Request aiRequestView
	// Tasks 是同一个请求中的全部任务（页面左侧的列表）。
	Tasks    []aiTaskView
	Attempts []aiAttemptView
	// Attempt 是日志读取的 attempt：地址中指定的，或当前 attempt；任务还没有 attempt 时为 0。
	Attempt int64
	// Explicit 表示地址中指定了 attempt（日志固定在这次 attempt）；Viewing 是地址中指定的、不是当前 attempt 的那次。
	Explicit bool
	Viewing  *aiAttemptView
	// AttemptErr 非空表示地址中的 attempt 无效（页面返回 404，不加载日志）。
	AttemptErr uiText
	Terminal   bool
	// RequestTerminal 表示请求已经结束；CanRerun 表示可以重跑这个任务（已结束的审计请求中的任务）。
	RequestTerminal, CanRerun bool
	// NoLog 表示任务没有执行过、也不会再有日志；LogEmpty 是没有日志时的说明。
	NoLog    bool
	LogEmpty uiText
	// AttemptKey 是日志阅读位置在浏览器 sessionStorage 中的键的一部分。
	AttemptKey string
	// Spec 是任务冻结的执行输入（JSON，缩进过）。
	Spec    string
	Review  *aiReviewView
	Develop *aiDevelopView
	// DesignBasis 是这个任务实际可读的设计依据（只有元数据）。
	DesignBasis *taskDocsPageView
}

func (d *Dashboard) pageAITask(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	t, err := ai.GetTask(ctx, d.db, pathID(r), false)
	if errors.Is(err, ai.ErrNotFound) {
		d.notFoundText(w, r, message(msgAITaskNotFound, strArg(argValue, r.PathValue("id"))))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p, err := d.aiTaskPageData(ctx, t)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.selectAttempt(r.URL.Query().Get("attempt"))
	status := http.StatusOK
	if !p.AttemptErr.IsZero() {
		status = http.StatusNotFound
	}
	d.renderPage(w, r, "ai_task", aiTaskTitle(t), p, status)
}

// aiTaskPageData 准备 AI 任务页面的数据（不含地址中的 attempt 参数）。
func (d *Dashboard) aiTaskPageData(ctx context.Context, t *ai.Task) (*aiTaskPage, error) {
	det, err := d.admin.AIRequest(ctx, t.RequestID)
	if err != nil {
		return nil, err
	}
	p := &aiTaskPage{
		Task: newAITaskView(t, det.Attempts[t.ID]), Request: newAIRequestView(det.AIRequestView), Terminal: t.State.Terminal(),
		RequestTerminal: det.State.Terminal(), AttemptKey: "current", Tasks: make([]aiTaskView, 0, len(det.Tasks)),
	}
	p.CanRerun = p.RequestTerminal && det.Type == agentcfg.TaskReview
	for _, s := range det.Tasks {
		p.Tasks = append(p.Tasks, newAITaskView(s, det.Attempts[s.ID]))
	}
	if p.Attempts, err = d.aiAttemptViews(ctx, t); err != nil {
		return nil, err
	}
	if t.CurrentAttemptID != nil {
		p.Attempt = *t.CurrentAttemptID
	}
	p.NoLog = len(p.Attempts) == 0 && p.Terminal
	p.LogEmpty = aiLogEmptyText(t, len(p.Attempts) > 0, det.PurgedAt != nil)
	var spec any
	if err := json.Unmarshal(t.Spec, &spec); err == nil {
		if b, err := json.MarshalIndent(spec, "", "  "); err == nil {
			p.Spec = string(b)
		}
	}
	switch t.Type {
	case agentcfg.TaskReview:
		p.Review, err = d.aiReviewViewFor(ctx, t.ID, det.Repo)
	case agentcfg.TaskDevelop:
		p.Develop, err = d.aiDevelopViewFor(ctx, t.ID)
	}
	if err != nil {
		return nil, err
	}
	p.DesignBasis, err = d.taskDesignBasis(ctx, t.ID)
	return p, err
}

// selectAttempt 按地址中的 attempt 参数选择要查看的 attempt。参数无效或不属于这个任务时设置 AttemptErr。
func (p *aiTaskPage) selectAttempt(v string) {
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		p.AttemptErr = message(msgAITaskAttemptInvalid, strArg(argValue, v))
		return
	}
	for i := range p.Attempts {
		a := &p.Attempts[i]
		if a.ID != n {
			continue
		}
		p.Attempt, p.Explicit, p.AttemptKey = n, true, strconv.FormatInt(n, 10)
		if !a.Current {
			p.Viewing = a
		}
		return
	}
	p.AttemptErr = message(msgAITaskAttemptMissing, intArg(argAttempt, n))
}

// aiLogEmptyText 返回 AI 任务没有日志时的说明；purged 表示日志已经按保留策略清理。
func aiLogEmptyText(t *ai.Task, hasAttempts, purged bool) uiText {
	switch {
	case purged && hasAttempts:
		return message(msgAITaskLogPurged)
	case hasAttempts:
		return message(msgAITaskLogNoOutput)
	case t.ReusedFrom != nil:
		return message(msgAITaskLogReused, intArg(argTask, *t.ReusedFrom))
	case t.State == ai.TaskSkipped:
		return message(msgAITaskLogSkipped)
	case t.State.Queued():
		return message(msgJobLogQueued)
	case t.State.Terminal():
		return message(msgAITaskLogNotRun)
	}
	return message(msgJobLogNone)
}

// ---- 日志 ----

// getAITaskLog 返回 AI 任务的日志片段，响应头与 job 的日志接口（getLog）相同，页面共用同一个日志组件。
func (d *Dashboard) getAITaskLog(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	attempt, err := parseOptionalInt(q.Get("attempt"))
	if err != nil || attempt < 0 {
		apiError(w, http.StatusBadRequest, message(msgLogBadAttempt))
		return
	}
	offset, err := parseOptionalInt(q.Get("offset"))
	if err != nil || offset < 0 {
		apiError(w, http.StatusBadRequest, message(msgLogBadOffset))
		return
	}
	id := pathID(r)
	l, err := d.admin.AITaskLog(r.Context(), id, attempt, offset)
	if err != nil {
		d.logError(w, r, logOfAITask, id, attempt, err)
		return
	}
	writeLogChunk(w, r, d, l)
}

// writeLogChunk 写出日志片段：正文是日志，响应头带上下一次的 offset、日志是否结束、读取的 attempt、当前 attempt 和状态。
func writeLogChunk(w http.ResponseWriter, r *http.Request, d *Dashboard, l *admin.LogChunk) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Next-Offset", strconv.FormatInt(l.Next, 10))
	h.Set("X-Finished", strconv.FormatBool(l.Finished))
	h.Set("X-Job-State", l.State)
	h.Set("X-Job-Terminal", strconv.FormatBool(l.Terminal))
	h.Set("X-Attempt", strconv.FormatInt(l.Attempt, 10))
	h.Set("X-Current-Attempt", strconv.FormatInt(l.CurrentAttempt, 10))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	//nolint:gosec // 以 text/plain + nosniff 返回，浏览器不会当作 HTML 解析
	if _, err := w.Write(l.Data); err != nil {
		d.log.Debug("发送日志", "path", r.URL.Path, "err", err)
	}
}

// getAITaskLogDownload 下载 AI 任务的完整日志（attempt 为空时取当前 attempt）。
func (d *Dashboard) getAITaskLogDownload(w http.ResponseWriter, r *http.Request, _ *session) {
	attempt, err := parseOptionalInt(r.URL.Query().Get("attempt"))
	if err != nil || attempt < 0 {
		d.badRequestText(w, r, message(msgAITaskAttemptInvalid, strArg(argValue, r.URL.Query().Get("attempt"))))
		return
	}
	id := pathID(r)
	f, err := d.admin.OpenAITaskLog(r.Context(), id, attempt)
	if err != nil {
		d.adminPageError(w, r, err)
		return
	}
	defer f.Close()
	name := fmt.Sprintf("kiln-ai-task-%d.log", id)
	if f.Attempt != 0 {
		name = fmt.Sprintf("kiln-ai-task-%d-attempt-%d.log", id, f.Attempt)
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if _, err := io.Copy(w, f); err != nil {
		d.log.Debug("发送日志", "path", r.URL.Path, "err", err)
	}
}

// ---- 操作 ----

func (d *Dashboard) postCancelAIRequest(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	if err := d.admin.CancelAIRequest(r.Context(), id, se.Login); err != nil {
		d.flashMsg(w, r, aiRequestURL(id), d.failureFlash(r, err, msgRunCancelFailed, msgRunCancelFailedRef))
		return
	}
	d.flashMsg(w, r, aiRequestURL(id), message(msgAIRequestCancelled))
}

// postRerunAIRequest 重跑 AI 审计请求：mode 为 all（全部任务）或 failed（只重跑没有成功的任务，默认）。
func (d *Dashboard) postRerunAIRequest(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	d.rerunAI(w, r, se, id, review.RerunMode(r.PostFormValue("mode")), 0, aiRequestURL(id))
}

// postRerunAITask 只重跑一个 AI 审计任务（其余任务沿用上一次的结果）。
func (d *Dashboard) postRerunAITask(w http.ResponseWriter, r *http.Request, se *session) {
	id := pathID(r)
	t, err := ai.GetTask(r.Context(), d.db, id, false)
	if errors.Is(err, ai.ErrNotFound) {
		d.notFoundText(w, r, message(msgAITaskRerunNotFound, intArg(argID, id)))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.rerunAI(w, r, se, t.RequestID, review.RerunTask, t.ID, aiTaskURL(id))
}

func (d *Dashboard) rerunAI(w http.ResponseWriter, r *http.Request, se *session, id int64, mode review.RerunMode, taskID int64, back string) {
	nr, err := d.admin.RerunAIRequest(r.Context(), id, mode, taskID, se.Login, store.SourceDashboard)
	if err != nil {
		d.flashMsg(w, r, back, d.failureFlash(r, err, msgRunRerunFailed, msgRunRerunFailedRef))
		return
	}
	d.flashMsg(w, r, aiRequestURL(nr.ID), message(msgAIRequestRerun, intArg(argID, nr.ID), intArg(argFrom, id)))
}

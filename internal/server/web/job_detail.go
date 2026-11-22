package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Job 详情页：
//   - 明确正在查看的是哪一次尝试（attempt），地址中的值无效时给出提示（404）；
//   - 重跑这个 job 前说明会一起重新规划哪些下游 job、是否强制执行；
//   - 日志接口区分未登录（401）和不存在（404），提供完整日志下载；
//   - 产物列表和下载：下载先校验登录和产物归属，再跳转到短期有效的 CAS 预签名地址（与 Runner 下载同一套授权）。

// outputURLTTL 是产物下载地址（CAS 预签名 URL）的有效期。
const outputURLTTL = 5 * time.Minute

// jobView 是 run 详情页和 job 详情页上的一个 job。
type jobView struct {
	*cistore.Job
	Runner   string
	Needs    []string
	Started  int64
	Finished int64
	// Progress 是进行中的 job 当前 attempt 最近上报的阶段与进度；
	// 不在执行中或 Runner 没有上报（旧版）时为 nil。
	Progress *store.AttemptProgress
	// WaitingOn 是等待中的 job 还没有完成的上游 job（名称）；上游不在 jobs 中时不列出。
	WaitingOn []string
}

func (d *Dashboard) jobViews(ctx context.Context, jobs []*cistore.Job) ([]jobView, error) {
	runners, err := d.admin.RunnerNames(ctx, jobs)
	if err != nil {
		return nil, err
	}
	var running []int64
	byID := map[int64]*cistore.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
		if j.State == store.JobRunning && j.CurrentAttemptID != nil {
			running = append(running, *j.CurrentAttemptID)
		}
	}
	progress, err := cistore.AttemptProgresses(ctx, d.db, running)
	if err != nil {
		return nil, err
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		v := jobView{Job: j, Runner: runners[j.ID]}
		for _, n := range j.Needs {
			up, ok := byID[n]
			if !ok {
				v.Needs = append(v.Needs, "")
				continue
			}
			v.Needs = append(v.Needs, up.DisplayName)
			if j.State == store.JobWaiting && !up.State.Terminal() {
				v.WaitingOn = append(v.WaitingOn, up.DisplayName)
			}
		}
		if j.State == store.JobRunning && j.CurrentAttemptID != nil {
			v.Progress = progress[*j.CurrentAttemptID]
		}
		if j.StartedAt != nil {
			v.Started = j.StartedAt.Unix()
		}
		if j.FinishedAt != nil {
			v.Finished = j.FinishedAt.Unix()
		}
		out = append(out, v)
	}
	return out, nil
}

type attemptView struct {
	ID, Epoch           int64
	Runner              string
	Outcome, ErrorClass string
	Message             string
	ExitCode            *int
	Started, Finished   int64
	// N 是序号：第 N 次尝试。
	N int
	// Current 表示这是 job 当前的 attempt。
	Current bool
}

func (d *Dashboard) attemptViews(ctx context.Context, j *cistore.Job) ([]attemptView, error) {
	rows, err := d.db.Query(ctx, `SELECT a.id, a.epoch, `+cistore.AttemptExecutorSQL+`, coalesce(a.outcome,''), coalesce(a.error_class,''), coalesce(a.message,''), a.exit_code,
		extract(epoch from a.started_at)::bigint, coalesce(extract(epoch from a.finished_at)::bigint, 0), 0, false
		FROM attempts a LEFT JOIN runners r ON r.id=a.runner_id WHERE a.job_id=$1 ORDER BY a.started_at, a.id`, j.ID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[attemptView])
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].N = i + 1
		list[i].Current = j.CurrentAttemptID != nil && list[i].ID == *j.CurrentAttemptID
	}
	return list, nil
}

// releaseView 是发布 job 页面上的发布进度。
type releaseView struct {
	Target, Tag, State, Stage string
	Progress                  map[string]any
	Error                     string
	// Targets 是各发布目标的结果（按提交顺序，release_targets）；v0.25 之前的发布没有，只显示 Progress。
	Targets []releaseTargetView
}

// releaseTargetView 是一个发布目标（镜像、HTTP 上传、GitHub Release、npm 包）的结果。
type releaseTargetView struct {
	Kind, Name, Target, State string
	Detail                    map[string]any
	Error                     string
}

// URL 返回发布结果的页面（页面数据中的 URL，见 json.go）：目前只有发布到 npm 官方 registry 的包（www.npmjs.com 上的版本页），
// 其他目标的地址在 Detail 中，没有时为空。
func (t releaseTargetView) URL() string {
	if t.Kind == "npm" && t.State == "committed" {
		return npmPackageURL(t.Detail)
	}
	return ""
}

// releaseViewFor 返回发布 job 的发布进度，还没有开始发布时返回 nil。
func (d *Dashboard) releaseViewFor(ctx context.Context, jobID int64) (*releaseView, error) {
	var v releaseView
	var progress []byte
	var errMsg *string
	err := d.db.QueryRow(ctx, `SELECT target_repo, tag, state, stage, progress, error FROM releases WHERE job_id=$1`, jobID).
		Scan(&v.Target, &v.Tag, &v.State, &v.Stage, &progress, &errMsg)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(progress, &v.Progress); err != nil {
		return nil, fmt.Errorf("发布进度无法解析：%w", err)
	}
	v.Error = deref(errMsg)
	rows, err := d.db.Query(ctx, `SELECT kind, name, target, state, detail, coalesce(error, '') FROM release_targets WHERE job_id=$1 ORDER BY seq`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t releaseTargetView
		var detail []byte
		if err := rows.Scan(&t.Kind, &t.Name, &t.Target, &t.State, &detail, &t.Error); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &t.Detail); err != nil {
			return nil, fmt.Errorf("发布目标的结果无法解析：%w", err)
		}
		sanitizeReleaseDetailURLs(t.Detail)
		v.Targets = append(v.Targets, t)
	}
	return &v, rows.Err()
}

// sanitizeReleaseDetailURLs 去掉发布目标 detail 中外部上传服务返回的不可信链接：assets[].url 只允许
// http/https 且有主机名，其他值（javascript:、data:、mailto: 等）从数据中删除，模板就不会把它渲染成可点击的链接。
func sanitizeReleaseDetailURLs(detail any) {
	m, ok := detail.(map[string]any)
	if !ok {
		return
	}
	assets, ok := m["assets"].([]any)
	if !ok {
		return
	}
	kept := assets[:0]
	for _, a := range assets {
		am, ok := a.(map[string]any)
		if !ok {
			kept = append(kept, a)
			continue
		}
		if u, ok := am["url"].(string); ok && !release.SafeWebURL(u) {
			delete(am, "url")
		}
		kept = append(kept, am)
	}
	m["assets"] = kept
}

// jobPage 是 job 详情页的数据。
type jobPage struct {
	Job  jobView
	Run  *cistore.Run
	Repo string
	// Attempts 按时间排列，N 是序号（第 N 次尝试）。
	Attempts []attemptView
	// Attempt 是日志读取的 attempt：地址中指定的，或当前 attempt；job 还没有 attempt 时为 0。
	Attempt int64
	// Explicit 表示地址中指定了 attempt（日志固定在这次尝试，不随新的尝试切换）。
	Explicit bool
	// Viewing 是地址中指定的、不是当前 attempt 的那次尝试（查看历史尝试时非 nil）。
	Viewing *attemptView
	// AttemptErr 非空表示地址中的 attempt 无效（页面返回 404，不加载日志）。
	AttemptErr string
	Spec       string
	Terminal   bool
	RunActive  bool
	Release    *releaseView
	// Outputs 是产物；Declared 是 job 声明的输出名称。
	Outputs  *admin.JobOutputs
	Declared []string
	// Producer 是生成产物的那次尝试（命中缓存、沿用上一次结果时为 nil）。
	Producer *attemptView
	// Rerun 是"重跑这个 job"的说明；run 没有结束时为 nil。
	Rerun *jobRerun
	// LogEmpty 是没有日志时显示的说明。
	LogEmpty string
	// Purged 是这个 job 的日志和产物按保留策略清理的时间（PLAN #10.3）；没有清理时为 nil。
	Purged *time.Time
	// Jobs 是同一次运行（rerun）中的全部 job（页面左侧的 job 列表，按 ID 排序）。
	Jobs []jobView
	// Queue 是排队中、调度器没有记下跳过原因的 job 前面还有多少个待领取的 job（调度解释，PLAN #14.2）；其他情况为 nil。
	Queue *cistore.QueuePosition
}

// Publish 表示这是发布 job（由 Server 执行，没有 attempt）。
func (p *jobPage) Publish() bool { return p.Job.Kind == workflow.JobKindPublish }

// NoLog 表示这个 job 没有执行过、也不会再有日志（命中缓存、沿用结果、跳过等），页面只显示原因。
func (p *jobPage) NoLog() bool { return !p.Publish() && len(p.Attempts) == 0 && p.Terminal }

// AttemptKey 是日志阅读位置在浏览器 sessionStorage 中的键的一部分。
func (p *jobPage) AttemptKey() string {
	if p.Explicit {
		return strconv.FormatInt(p.Attempt, 10)
	}
	return "current"
}

func (d *Dashboard) pageJob(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	j, err := cistore.GetJob(ctx, d.db, pathID(r), false)
	if isNotFound(err) || (err == nil && j.RunID == 0) {
		d.notFound(w, r, fmt.Sprintf("没有编号为 %s 的 job：编号可能写错了，或者这个 job 已经被清理。", r.PathValue("id")))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p, err := d.jobPageData(ctx, j)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.selectAttempt(r.URL.Query().Get("attempt"))
	status := http.StatusOK
	if p.AttemptErr != "" {
		status = http.StatusNotFound
	}
	d.renderStatus(w, r, "job", j.DisplayName, p, status)
}

// selectAttempt 按地址中的 attempt 参数选择要查看的尝试。参数无效或不属于这个 job 时设置 AttemptErr。
func (p *jobPage) selectAttempt(v string) {
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	switch {
	case p.Publish():
		p.AttemptErr = "发布 job 由 Server 执行，没有尝试记录，日志按 job 保存。"
		return
	case err != nil || n <= 0:
		p.AttemptErr = fmt.Sprintf("地址中的 attempt 参数无效（%q），应为这个 job 的某次尝试的编号。", v)
		return
	}
	for i := range p.Attempts {
		a := &p.Attempts[i]
		if a.ID != n {
			continue
		}
		p.Attempt, p.Explicit = n, true
		if !a.Current {
			p.Viewing = a
		}
		return
	}
	p.AttemptErr = fmt.Sprintf("这个 job 没有尝试 #%d（可能属于别的 job，或编号写错了）。", n)
}

// jobPageData 准备 job 页面的数据（不含地址中的 attempt 参数）。
func (d *Dashboard) jobPageData(ctx context.Context, j *cistore.Job) (*jobPage, error) {
	run, err := cistore.GetRun(ctx, d.db, j.RunID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, d.db, run.RepoID)
	if err != nil {
		return nil, err
	}
	p := &jobPage{Run: run, Repo: repo.FullName(), Terminal: j.State.Terminal(), RunActive: run.State.Open()}
	if p.Attempts, err = d.attemptViews(ctx, j); err != nil {
		return nil, err
	}
	if j.CurrentAttemptID != nil {
		p.Attempt = *j.CurrentAttemptID
	}
	switch j.Kind {
	case workflow.JobKindPublish:
		if p.Release, err = d.releaseViewFor(ctx, j.ID); err != nil {
			return nil, err
		}
	default:
		if p.Outputs, err = d.admin.JobOutputs(ctx, j); err != nil {
			return nil, err
		}
		for _, o := range j.Spec.Outputs {
			p.Declared = append(p.Declared, o.Name)
		}
		if j.State == store.JobSucceeded && j.ReusedFrom == nil && j.CachedFrom == nil {
			for i := range p.Attempts {
				if p.Attempts[i].Current {
					p.Producer = &p.Attempts[i]
				}
			}
		}
	}
	if !p.RunActive {
		if p.Rerun, err = d.jobRerunFor(ctx, j, run); err != nil {
			return nil, err
		}
	}
	spec, err := json.MarshalIndent(j.Spec, "", "  ")
	if err != nil {
		return nil, err
	}
	p.Spec = string(spec)
	siblings, err := cistore.RerunJobs(ctx, d.db, run.ID, j.Rerun, false)
	if err != nil {
		return nil, err
	}
	if p.Jobs, err = d.jobViews(ctx, siblings); err != nil {
		return nil, err
	}
	views, err := d.jobViews(ctx, []*cistore.Job{j})
	if err != nil {
		return nil, err
	}
	p.Job = views[0]
	// 上游 job 的状态只在同一次运行的 job 列表中才有
	for _, s := range p.Jobs {
		if s.ID == j.ID {
			p.Job.WaitingOn = s.WaitingOn
		}
	}
	if run.JobPurged(j) {
		p.Purged = run.PurgedAt
	}
	// 有跳过原因（资源、配额、择优等）时页面显示它；没有时说明它排在哪些 job 之后
	if j.WaitReason == nil {
		if p.Queue, err = cistore.QueueAhead(ctx, d.db, j); err != nil {
			return nil, err
		}
	}
	p.LogEmpty = logEmptyText(j, len(p.Attempts) > 0, p.Purged != nil)
	return p, nil
}

// jobRerun 说明重跑这个 job 的影响（规则同 planner.rerunPlan 的 job 方式）。
type jobRerun struct {
	// Latest 为 false 表示 job 不属于最新一次运行，不能直接重跑；LatestID 是最新一次运行中同一个 job（没有时为 0）。
	Latest   bool
	LatestID int64
	// Force 表示这个 job 上次成功，重跑时强制重新执行（不使用缓存）。
	Force bool
	// Downstream 是会一起重新规划的下游 job；Kept 是沿用结果的其他 job 数。
	Downstream []string
	Kept       int
	// Publish 表示重新规划的 job 中有发布 job（会再次执行发布）。
	Publish bool
	// Replan 是产物已经按保留策略清理、不能沿用而重新规划的其他 job 数（不计入 Kept）。
	Replan int
}

// jobRerunFor 计算重跑 job 的说明。job 不在最新一次运行中时不能直接重跑，给出最新一次运行中同一个 job。
func (d *Dashboard) jobRerunFor(ctx context.Context, j *cistore.Job, run *cistore.Run) (*jobRerun, error) {
	if j.Rerun != run.CurrentRerun {
		r := &jobRerun{}
		err := d.db.QueryRow(ctx, `SELECT id FROM jobs WHERE run_id=$1 AND rerun=$2 AND job_key=$3`, run.ID, run.CurrentRerun, j.JobKey).Scan(&r.LatestID)
		if err != nil && !isNotFound(err) {
			return nil, err
		}
		return r, nil
	}
	jobs, err := cistore.RerunJobs(ctx, d.db, run.ID, run.CurrentRerun, false)
	if err != nil {
		return nil, err
	}
	return newJobRerun(j, jobs, run.Purged()), nil
}

// newJobRerun 按 job 所在运行的全部 job 计算重跑说明；purged 表示这次运行的产物已经清理（cistore.Run.Purged）。
func newJobRerun(j *cistore.Job, jobs []*cistore.Job, purged bool) *jobRerun {
	down := map[int64]bool{j.ID: true}
	for changed := true; changed; {
		changed = false
		for _, o := range jobs {
			if down[o.ID] {
				continue
			}
			for _, n := range o.Needs {
				if down[n] {
					down[o.ID], changed = true, true
					break
				}
			}
		}
	}
	r := &jobRerun{Latest: true, Force: j.State == store.JobSucceeded, Publish: j.Kind == workflow.JobKindPublish}
	for _, o := range jobs {
		switch {
		case o.ID == j.ID:
		case down[o.ID]:
			r.Downstream = append(r.Downstream, o.DisplayName)
			r.Publish = r.Publish || o.Kind == workflow.JobKindPublish
		case purged && o.ResultManifest != nil && o.Kind != workflow.JobKindPublish:
			r.Replan++ // 规则同 planner.rerunPlan：产物已经清理，不能沿用
		default:
			r.Kept++
		}
	}
	return r
}

// logEmptyText 返回 job 没有日志时的说明；purged 表示日志已经按保留策略清理。
func logEmptyText(j *cistore.Job, hasAttempts, purged bool) string {
	switch {
	case purged && (hasAttempts || j.Kind == workflow.JobKindPublish):
		return "日志已按保留策略清理：run 结束超过保留期后只保留结果（状态、耗时、原因、退出码）。"
	case hasAttempts:
		return "这次尝试没有输出日志。"
	case j.Kind == workflow.JobKindPublish && !j.State.Terminal():
		return "发布还没有开始，开始后这里会实时显示发布日志。"
	case j.ReusedFrom != nil:
		return fmt.Sprintf("这个 job 沿用上一次运行的结果，没有执行；日志见 job #%d。", *j.ReusedFrom)
	case j.CachedFrom != nil:
		return fmt.Sprintf("这个 job 命中缓存，没有执行；日志见 job #%d。", *j.CachedFrom)
	case j.State == store.JobSkipped:
		return "这个 job 被跳过，没有执行。"
	case j.State.Queued():
		return "还没有开始执行，开始后这里会实时显示日志。"
	case j.State.Terminal():
		return "这个 job 没有执行，没有日志（原因见上方）。"
	}
	return "没有日志。"
}

// getLog 返回日志片段。响应头带上下一次的 offset、日志是否结束、读取的 attempt、job 当前的 attempt 和状态
// （页面据此切换到新的尝试、局部刷新）。job 或 attempt 不存在时返回 404，未登录时返回 401（见 webAPI）。
func (d *Dashboard) getLog(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	attempt, err := parseOptionalInt(q.Get("attempt"))
	if err != nil || attempt < 0 {
		http.Error(w, "attempt 无效", http.StatusBadRequest)
		return
	}
	offset, err := parseOptionalInt(q.Get("offset"))
	if err != nil || offset < 0 {
		http.Error(w, "offset 无效", http.StatusBadRequest)
		return
	}
	l, err := d.admin.JobLog(r.Context(), pathID(r), attempt, offset)
	if err != nil {
		d.adminError(w, r, err)
		return
	}
	writeLogChunk(w, r, d, l)
}

// parseOptionalInt 解析可选的整数参数（空字符串为 0）。
func parseOptionalInt(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// getLogDownload 下载完整日志（attempt 为空时取当前 attempt）。
func (d *Dashboard) getLogDownload(w http.ResponseWriter, r *http.Request, _ *session) {
	attempt, err := parseOptionalInt(r.URL.Query().Get("attempt"))
	if err != nil || attempt < 0 {
		d.badRequest(w, r, fmt.Sprintf("attempt 参数无效（%q），应为这个 job 的某次尝试的编号。", r.URL.Query().Get("attempt")))
		return
	}
	id := pathID(r)
	f, err := d.admin.OpenJobLog(r.Context(), id, attempt)
	if err != nil {
		d.adminPageError(w, r, err)
		return
	}
	defer f.Close()
	name := fmt.Sprintf("kiln-job-%d.log", id)
	if f.Attempt != 0 {
		name = fmt.Sprintf("kiln-job-%d-attempt-%d.log", id, f.Attempt)
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

// getOutput 下载 job 的一个产物文件：确认文件属于这个 job 的结果后，跳转到短期有效的 CAS 预签名地址。
func (d *Dashboard) getOutput(w http.ResponseWriter, r *http.Request, _ *session) {
	id := pathID(r)
	q := r.URL.Query()
	output, file := q.Get("output"), q.Get("path")
	f, err := d.admin.JobOutputFile(r.Context(), id, output, file)
	if errors.Is(err, admin.ErrOutputGone) {
		d.flashBack(w, r, jobURL(id), fmt.Sprintf("没有下载：产物 %s/%s 已不在存储中。重跑这个 job（不用缓存）可以重新生成。", output, file))
		return
	}
	if err != nil {
		d.adminPageError(w, r, err)
		return
	}
	u := d.keys.CASURL(auth.CASOpGet, f.Digest, outputURLTTL) + "&filename=" + url.QueryEscape(path.Base(f.Path))
	w.Header().Set("Cache-Control", "no-store")
	d.redirect(w, r, u)
}

// isNotFound 判断错误是否表示查询的对象不存在。
func isNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

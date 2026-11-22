package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// Run 详情页：明确正在查看的是哪一次运行（rerun），地址中的值无效时给出提示（404）；
// 重跑前说明会重新执行哪些 job、哪些沿用结果、是否用缓存、发布 job 是否会再次执行。
// job 按 needs 分成阶段（没有依赖的在第一阶段，其余在所依赖 job 的最后一个阶段之后），从左到右排列；
// 页面下方是这个 run 的活动：谁触发、手动启动、取消和重跑了它，多人使用时能看出状态变化是谁造成的。

// rerunView 是 run 的一次运行。
type rerunView struct {
	N int
	// State 是这次运行的状态；没有记录时为空。
	State store.RunState
	// created、finished 是这次运行的创建（开始排队）和结束时间（run_reruns）；没有记录、还没结束时为 nil。
	created, finished *time.Time
}

// runPage 是 run 详情页的数据。
type runPage struct {
	Run  *cistore.Run
	Repo string
	// Back 是"返回运行列表"的地址（可以为空）。
	Back   string
	Reruns []rerunView
	// Viewed 是正在查看的运行；地址中的 rerun 无效时为 nil，Invalid 是原值。
	Viewed  *rerunView
	Invalid string
	Jobs    []jobView
	// Stages 是按 needs 分好的阶段。
	Stages []jobStage
	// States 是正在查看的这次运行中各 job 的状态（烧制条）。
	States []string
	// Activity 是这个 run 的活动（启动、取消、重跑），按时间排序。
	Activity []activityEntry
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
	// Active 表示 run 还没有结束（排队中或执行中）：可以取消，不能重跑。
	Active bool
	// Form 是重跑的说明和默认方式；run 没有结束时为 nil。
	Form *rerunForm
	// Purged 是正在查看的这次运行的日志和产物按保留策略清理的时间（PLAN #10.3）；没有清理时为 nil。
	Purged *time.Time
	// MergeQueue 是 PR 的合并队列状态（PLAN #28）；不是 PR 的 run 为 nil。
	MergeQueue *runMergeQueue
}

// Start、End 返回正在查看的这次运行的开始和结束时间（Unix 秒），用于显示耗时。
// 耗时是这次运行从创建（开始排队）到结束的总时长，含排队；重跑从重跑时算起，不含上一次结束后的空闲时间。
// 还没结束时 End 为 0（耗时算到现在）；查看的是历史运行而时间没有记录（早期数据）时 Start 为 0，不显示耗时。
func (p *runPage) Start() int64 {
	v := p.Viewed
	switch {
	case v == nil:
		return p.Run.CreatedAt.Unix()
	case v.N == p.Run.CurrentRerun:
		if v.created == nil {
			return p.Run.CreatedAt.Unix()
		}
		return v.created.Unix()
	case v.created == nil || v.finished == nil:
		return 0 // 历史运行一定已经结束，没有记录时不显示耗时，免得一直增长
	}
	return v.created.Unix()
}

func (p *runPage) End() int64 {
	v := p.Viewed
	if v == nil || v.N == p.Run.CurrentRerun {
		// 最新一次以 run 为准：重跑开始执行时会清空 runs.finished_at
		if p.Run.FinishedAt == nil {
			return 0
		}
		return p.Run.FinishedAt.Unix()
	}
	if v.finished == nil {
		return 0
	}
	return v.finished.Unix()
}

// Ref 返回 run 的 ref（页面数据中的 Ref，见 json.go）。
func (p *runPage) Ref() string { return p.Run.Ref }

// jobStage 是 run 详情页上的一个阶段：N 从 1 开始，Jobs 按 job ID 排序。
type jobStage struct {
	N    int
	Jobs []jobView
}

// History 表示正在查看的不是最新一次运行。
func (p *runPage) History() bool { return p.Viewed != nil && p.Viewed.N != p.Run.CurrentRerun }

// rerunForm 说明各种重跑方式的影响（按最新一次运行的 job 计算，规则同 planner.rerunPlan）。
type rerunForm struct {
	// Default 是默认选中的方式：有未成功的 job 时为 failed，否则为 all（成功的 run 不默认"重跑未成功的 job"）。
	Default store.RerunMode
	Total   int
	// Retry 是按"重跑未成功的 job"会重新规划的 job 数，其余沿用上一次的结果。
	Retry int
	// Publish、Effects 是发布 job 和有副作用（effects: true）的 job 数：它们不使用缓存，全部重跑时会再次执行。
	Publish, Effects int
	// Purged 表示最新一次运行的产物已经按保留策略清理：成功的 job 也要重新规划（只有发布 job 沿用）。
	Purged bool
}

// Kept 返回按"重跑未成功的 job"会沿用结果的 job 数。
func (f *rerunForm) Kept() int { return f.Total - f.Retry }

// newRerunForm 按最新一次运行的 job 计算重跑说明；purged 表示这次运行的产物已经清理（cistore.Run.Purged）。
func newRerunForm(jobs []*cistore.Job, purged bool) *rerunForm {
	f := &rerunForm{Total: len(jobs), Purged: purged}
	for _, j := range jobs {
		if !j.Reusable(purged) {
			f.Retry++
		}
		if j.Kind == workflow.JobKindPublish {
			f.Publish++
		}
		if j.Effects {
			f.Effects++
		}
	}
	f.Default = store.RerunAll
	if f.Retry > 0 {
		f.Default = store.RerunFailed
	}
	return f
}

// rerunViews 返回 run 的每一次运行（0..CurrentRerun）。最新一次的状态以 run 为准；
// 没有 run_reruns 记录的（例如早期数据）状态为空。
func (d *Dashboard) rerunViews(ctx context.Context, run *cistore.Run) ([]rerunView, error) {
	rows, err := d.db.Query(ctx, `SELECT rerun, state, created_at, finished_at FROM run_reruns WHERE run_id=$1 AND rerun <= $2 ORDER BY rerun`, run.ID, run.CurrentRerun)
	if err != nil {
		return nil, err
	}
	recorded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rerunView, error) {
		var v rerunView
		err := row.Scan(&v.N, &v.State, &v.created, &v.finished)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]rerunView, run.CurrentRerun+1)
	for i := range out {
		out[i].N = i
	}
	for _, v := range recorded {
		out[v.N] = v
	}
	out[run.CurrentRerun].State = run.State
	return out, nil
}

// parseRerun 解析地址中的 rerun 参数：空值为最新一次；不是 0..current 的整数时返回 false。
func parseRerun(v string, current int) (int, bool) {
	if v == "" {
		return current, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > current {
		return 0, false
	}
	return n, true
}

func (d *Dashboard) pageRun(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	run, err := cistore.GetRun(ctx, d.db, pathID(r), false)
	if isNotFound(err) {
		d.notFoundText(w, r, message(msgRunNotFound, strArg(argValue, r.PathValue("id"))))
		return
	}
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	repo, err := store.GetRepo(ctx, d.db, run.RepoID)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	reruns, err := d.rerunViews(ctx, run)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &runPage{
		Run: run, Repo: repo.FullName(), Back: runBackURL(r.URL.Query().Get("back")), Reruns: reruns, Active: run.State.Open(),
		GitHub: d.githubWeb(),
	}
	if p.Activity, err = d.runActivity(ctx, run.ID); err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.MergeQueue, err = d.runMergeQueueState(ctx, run, p.Repo); err != nil {
		d.serverError(w, r, err)
		return
	}
	n, ok := parseRerun(r.URL.Query().Get("rerun"), run.CurrentRerun)
	if !ok {
		// 地址中的 rerun 无效：不显示任何一次运行的 job，避免误以为在看最新结果
		p.Invalid = r.URL.Query().Get("rerun")
		d.renderPage(w, r, "run", runTitle(run.ID, run.CurrentRerun, run.CurrentRerun), p, http.StatusNotFound)
		return
	}
	p.Viewed = &reruns[n]
	jobs, err := cistore.RerunJobs(ctx, d.db, run.ID, n, false)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if p.Jobs, err = d.jobViews(ctx, jobs); err != nil {
		d.serverError(w, r, err)
		return
	}
	p.Stages = jobStages(p.Jobs)
	if len(jobs) > 0 && run.JobPurged(jobs[0]) {
		p.Purged = run.PurgedAt
	}
	for _, j := range p.Jobs {
		p.States = append(p.States, string(j.State))
	}
	if !p.Active {
		// 重跑总是基于最新一次运行
		latest := jobs
		if n != run.CurrentRerun {
			if latest, err = cistore.RerunJobs(ctx, d.db, run.ID, run.CurrentRerun, false); err != nil {
				d.serverError(w, r, err)
				return
			}
		}
		p.Form = newRerunForm(latest, run.Purged())
	}
	d.renderPage(w, r, "run", runTitle(run.ID, n, run.CurrentRerun), p, http.StatusOK)
}

// jobStages 按 needs 把 job 分成阶段：没有依赖（或依赖不在这次运行中）的在第一阶段，其余在所依赖 job 中最晚的阶段之后。
func jobStages(jobs []jobView) []jobStage {
	byID := make(map[int64]*cistore.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j.Job
	}
	depth := make(map[int64]int, len(jobs))
	var stageOf func(id int64, seen map[int64]bool) int
	stageOf = func(id int64, seen map[int64]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		j := byID[id]
		if j == nil || seen[id] { // 不在这次运行中，或者有环（规划时已经拒绝，这里只防御）
			return -1
		}
		seen[id] = true
		d := 0
		for _, n := range j.Needs {
			d = max(d, stageOf(n, seen)+1)
		}
		depth[id] = d
		return d
	}
	var stages []jobStage
	for _, j := range jobs {
		d := stageOf(j.ID, map[int64]bool{})
		for len(stages) <= d {
			stages = append(stages, jobStage{N: len(stages) + 1})
		}
		stages[d].Jobs = append(stages[d].Jobs, j)
	}
	for _, s := range stages {
		slices.SortStableFunc(s.Jobs, func(a, b jobView) int { return cmp.Compare(a.ID, b.ID) })
	}
	return stages
}

// runActivityLimit 是 run 详情页列出的活动数上限。
const runActivityLimit = 50

// runActivity 返回审计日志中关于这个 run 的活动（手动启动、取消、重跑），按时间排序。
func (d *Dashboard) runActivity(ctx context.Context, runID int64) ([]activityEntry, error) {
	rows, err := d.db.Query(ctx, `SELECT `+activityCols+` FROM audit_log
		WHERE detail ? 'run_id' AND (detail->>'run_id')::bigint = $1 ORDER BY id LIMIT $2`, runID, runActivityLimit)
	if err != nil {
		return nil, fmt.Errorf("读取 run 的活动：%w", err)
	}
	return pgx.CollectRows(rows, scanActivity)
}

// runTitle 返回 run 详情页的标题：查看的不是最新一次运行（n 不是 current）时写明是哪一次。
func runTitle(id int64, n, current int) uiText {
	switch n {
	case current:
		return message(msgRunTitle, intArg(argID, id))
	case 0:
		return message(msgRunTitleFirst, intArg(argID, id))
	}
	return message(msgRunTitleRerun, intArg(argID, id), intArg(argN, int64(n)))
}

// RunText 返回 run 详情页上这条活动的说明（不含操作者和 run 编号）：run 的操作是消息，
// 其他活动（不会出现在 run 详情页上）原样返回审计日志的说明。
func (e activityEntry) RunText() uiText {
	num := func(k string) int64 {
		v, _ := e.Detail[k].(float64)
		return int64(v)
	}
	switch e.Action {
	case "run.dispatch":
		return message(msgRunActDispatch)
	case "run.cancel":
		return message(msgRunActCancel)
	case "run.rerun":
		n := intArg(argN, num("rerun"))
		if j := num("job_id"); j != 0 {
			return message(msgRunActRerunJob, intArg(argJob, j), n)
		}
		switch store.RerunMode(fmt.Sprint(e.Detail["mode"])) {
		case store.RerunFailed:
			return message(msgRunActRerunFailed, n)
		case store.RerunAll:
			return message(msgRunActRerunAll, n)
		case store.RerunAllNoCache:
			return message(msgRunActRerunNoCache, n)
		}
		return message(msgRunActRerun, n)
	}
	return rawText(e.Text())
}

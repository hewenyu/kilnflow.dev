package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 运行列表（查构建，/runs）和启动构建（手动触发 workflow_dispatch，/runs/new 与 POST /dispatch）。

const (
	// runsPageSize 是运行列表每页的 run 数。
	runsPageSize = 50
	// runWorkflowsLimit 是筛选栏中 workflow 候选项的数量上限。
	runWorkflowsLimit = 200
)

// option 是下拉框的一个选项。
type option struct{ Value, Label string }

// runStateOptions 是运行列表按状态筛选的选项（与 store.RunState 一致）。
var runStateOptions = []option{
	{string(store.RunQueued), "排队中"},
	{string(store.RunActive), "运行中"},
	{string(store.RunSucceeded), "成功"},
	{string(store.RunFailed), "失败"},
	{string(store.RunCancelled), "已取消"},
	{string(store.RunSkipped), "已跳过"},
}

// runView 是运行列表中的一个 run。
type runView struct {
	*cistore.Run
	Repo string
	// States 是最新一次运行中各 job 的状态（烧制条），没有 job 时为空。
	States []string
	// URL 是详情页的地址（从筛选后的列表进入时带上 back）。
	URL string
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
	// since 是最新一次运行的创建时间（run_reruns）；没有记录时为 nil，用 run 的创建时间。
	since *time.Time
}

// Start 返回最新一次运行的创建时间（Unix 秒），与 End 一起显示耗时：重跑从重跑时算起。
func (v runView) Start() int64 {
	if v.since != nil {
		return v.since.Unix()
	}
	return v.CreatedAt.Unix()
}

// End 返回 run 的结束时间（Unix 秒），还没结束时为 0（耗时算到现在）。
func (v runView) End() int64 {
	if v.FinishedAt == nil {
		return 0
	}
	return v.FinishedAt.Unix()
}

// runFilter 是运行列表的筛选条件。条件都写在 URL 中，可以分享，也能从详情页带回。
type runFilter struct {
	// Repo 是仓库的全名，或者全名的一部分（不区分大小写）。
	Repo, State, Workflow, Ref string
	// Actor 是发起人的 GitHub 登录名（"我发起的"即当前用户）。
	Actor string
}

// Active 表示设置了任一筛选条件（页面数据中的 Active，见 json.go）。
func (f runFilter) Active() bool { return f != runFilter{} }

// readRunFilter 从查询参数中读取筛选条件；未知的状态被忽略。
func readRunFilter(q url.Values) runFilter {
	f := runFilter{
		Repo:     strings.TrimSpace(q.Get("repo")),
		State:    strings.TrimSpace(q.Get("state")),
		Workflow: strings.TrimSpace(q.Get("workflow")),
		Ref:      strings.TrimSpace(q.Get("ref")),
		Actor:    strings.TrimSpace(q.Get("actor")),
	}
	if !slices.ContainsFunc(runStateOptions, func(o option) bool { return o.Value == f.State }) {
		f.State = ""
	}
	return f
}

// url 返回带筛选条件的运行列表地址；cursor 是游标参数（before 或 after，为空表示最新一页）。
func (f runFilter) url(cursor string, id int64) string {
	q := url.Values{}
	for k, v := range map[string]string{"repo": f.Repo, "state": f.State, "workflow": f.Workflow, "ref": f.Ref, "actor": f.Actor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if cursor != "" {
		q.Set(cursor, strconv.FormatInt(id, 10))
	}
	if len(q) == 0 {
		return "/runs"
	}
	return "/runs?" + q.Encode()
}

// runsPage 是运行列表的数据。
type runsPage struct {
	Filter runFilter
	// RepoExact 是与筛选中的仓库全名完全相同的仓库（此时"启动构建"预选它）；为空表示按子串匹配或没有按仓库筛选。
	RepoExact string
	Runs      []runView
	States    []option
	Workflows []string
	// Paged 表示当前不是最新一页（用游标翻过页）；NewestID / OldestID 是本页第一个和最后一个 run。
	Paged              bool
	NewestID, OldestID int64
	// NewerURL / OlderURL 是较新和较早一页的地址（没有时为空）；LatestURL 是带当前筛选的最新一页。
	NewerURL, OlderURL, LatestURL string
	// Self 是当前页的地址，详情页的"返回运行列表"回到这里；为 /runs 时不需要传递。
	Self string
	// NewURL 是"启动构建"的地址。
	NewURL string
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
	// Me 是当前用户的 GitHub 登录名（"我发起的"），还没有绑定 GitHub 账号的 admin token 登录为空。
	Me string
	// AllURL、MineURL 是"全部"和"我发起的"的地址（保留其他筛选条件）。
	AllURL, MineURL string
}

// Mine 表示正在看"我发起的"。
func (p *runsPage) Mine() bool { return p.Me != "" && strings.EqualFold(p.Filter.Actor, p.Me) }

func (d *Dashboard) pageRuns(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	q := r.URL.Query()
	p := &runsPage{Filter: readRunFilter(q), States: runStateOptions, NewURL: "/runs/new", GitHub: d.githubWeb()}
	p.Me = se.githubLogin()
	all, mine := p.Filter, p.Filter
	all.Actor, mine.Actor = "", p.Me
	p.AllURL, p.MineURL = all.url("", 0), mine.url("", 0)
	// 筛选表单会提交空字段：去掉它们，让地址只包含实际的条件
	for k, v := range q {
		if len(v) == 1 && strings.TrimSpace(v[0]) == "" && k != "flash" {
			d.redirect(w, r, p.Filter.url("", 0))
			return
		}
	}
	sf := &cistore.RunFilter{
		RepoLike: p.Filter.Repo, State: store.RunState(p.Filter.State), Workflow: p.Filter.Workflow, Ref: p.Filter.Ref,
		Actor: p.Filter.Actor, Limit: runsPageSize + 1,
	}
	if strings.Contains(p.Filter.Repo, "/") {
		repo, err := store.FindRepo(ctx, d.db, p.Filter.Repo)
		switch {
		case err == nil:
			sf.RepoID, sf.RepoLike = repo.ID, ""
			p.RepoExact = repo.FullName()
			p.NewURL = "/runs/new?" + url.Values{"repo": {p.RepoExact}, "from": {"filter"}}.Encode()
		case !errors.Is(err, store.ErrRepoNotFound):
			d.serverError(w, r, err)
			return
		}
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	switch {
	case before > 0:
		sf.Before = before
	case after > 0:
		sf.After = after
	}
	runs, names, err := cistore.QueryRuns(ctx, d.db, sf)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	// 多取的一条只用来判断这个方向上还有没有更多
	more := len(runs) > runsPageSize
	if more {
		if sf.After > 0 {
			runs, names = runs[1:], names[1:]
		} else {
			runs, names = runs[:runsPageSize], names[:runsPageSize]
		}
	}
	if p.Runs, err = d.runViews(ctx, runs, names); err != nil {
		d.serverError(w, r, err)
		return
	}
	p.Paged = sf.Before > 0 || sf.After > 0
	p.LatestURL = p.Filter.url("", 0)
	switch {
	case sf.Before > 0:
		p.Self = p.Filter.url("before", sf.Before)
	case sf.After > 0:
		p.Self = p.Filter.url("after", sf.After)
	default:
		p.Self = p.LatestURL
	}
	if len(p.Runs) > 0 {
		newest, oldest := p.Runs[0].ID, p.Runs[len(p.Runs)-1].ID
		p.NewestID, p.OldestID = newest, oldest
		if sf.Before > 0 || (sf.After > 0 && more) {
			p.NewerURL = p.Filter.url("after", newest)
		}
		if sf.After > 0 || (sf.After == 0 && more) {
			p.OlderURL = p.Filter.url("before", oldest)
		}
	}
	for i := range p.Runs {
		p.Runs[i].GitHub = p.GitHub
		if p.Self != "/runs" {
			p.Runs[i].URL += "?back=" + url.QueryEscape(p.Self)
		}
	}
	if p.Workflows, err = cistore.RunWorkflows(ctx, d.db, sf.RepoID, runWorkflowsLimit); err != nil {
		d.serverError(w, r, err)
		return
	}
	d.render(w, r, "runs", "Runs", p)
}

// runViews 给 run 加上仓库名和最新一次运行中各 job 的状态（烧制条）。
func (d *Dashboard) runViews(ctx context.Context, runs []*cistore.Run, names []string) ([]runView, error) {
	ids := make([]int64, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	states, err := cistore.RunJobStates(ctx, d.db, ids)
	if err != nil {
		return nil, err
	}
	since, err := currentRerunStarts(ctx, d.db, ids)
	if err != nil {
		return nil, err
	}
	out := make([]runView, len(runs))
	for i, run := range runs {
		out[i] = runView{Run: run, Repo: names[i], States: states[run.ID], URL: runURL(run.ID)}
		if t, ok := since[run.ID]; ok {
			out[i].since = &t
		}
	}
	return out, nil
}

// currentRerunStarts 返回各 run 最新一次运行的创建时间（没有 run_reruns 记录的不在结果中）。
func currentRerunStarts(ctx context.Context, q store.Querier, runIDs []int64) (map[int64]time.Time, error) {
	rows, err := q.Query(ctx, `SELECT r.id, rr.created_at FROM runs r JOIN run_reruns rr ON rr.run_id=r.id AND rr.rerun=r.current_rerun
		WHERE r.id = ANY($1)`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// githubWeb 返回 GitHub 网页的地址。只在使用 github.com 时返回，其他 API 地址（例如测试用的模拟服务）不生成链接。
func (d *Dashboard) githubWeb() string {
	switch strings.TrimRight(d.cfg.GitHubAPIURL, "/") {
	case "", githubAPIURL:
		return githubWebURL
	}
	return ""
}

// runBackURL 返回详情页"返回运行列表"的地址：只接受站内的运行列表地址，其他值返回空。
func runBackURL(back string) string {
	if back != "/runs" && !strings.HasPrefix(back, "/runs?") {
		return ""
	}
	return safeNext(back)
}

// runNewPage 是"启动构建"页的数据。没有选定仓库时（Picker 非 nil）显示仓库选择器。
type runNewPage struct {
	Picker *repoSearch
	// Repo 是已选定的仓库；FromFilter 表示它是从运行列表的筛选条件带过来的。
	Repo       string
	FromFilter bool
	// DefaultBranch 是仓库的默认分支（ref 留空时使用）。
	DefaultBranch string
	// Target 是按表单中的 ref 读取到的 workflow 列表；为 nil 时表单退回为手填文件名。
	Target *planner.DispatchTarget
	// Dispatchable 是 Target 中可以手动触发的 workflow 数。
	Dispatchable int
	// TargetErr 是读取 workflow 列表失败的原因（ref 找不到时放在 Form 的 ref 字段上）。
	TargetErr string
	Form      *formState
	// Attempted 表示这是一次启动构建的提交（错误摘要据此写明"没有启动构建"）。
	Attempted bool
	// RequestID 是这次表单的幂等键：重复提交同一个表单只会创建一个 run（PLAN #5.2 的 dispatch:<请求 ID>）。
	RequestID string
	// ListURL 是"取消"和面包屑回到的运行列表。
	ListURL string
}

// newRequestID 生成手动触发的请求 ID。
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read 不会返回错误
	return hex.EncodeToString(b)
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (d *Dashboard) pageRunNew(w http.ResponseWriter, r *http.Request, _ *session) {
	q := r.URL.Query()
	f := newForm()
	f.Values["repo"] = strings.TrimSpace(q.Get("repo"))
	f.Values["ref"] = strings.TrimSpace(q.Get("ref"))
	f.Values["workflow"] = strings.TrimSpace(q.Get("workflow"))
	p := &runNewPage{Form: f, RequestID: newRequestID(), FromFilter: q.Get("from") == "filter"}
	status := http.StatusOK
	if f.Val("repo") == "" {
		page, _ := strconv.Atoi(q.Get("page"))
		if !d.fillPicker(w, r, p, strings.TrimSpace(q.Get("q")), page) {
			return
		}
	} else if !d.fillTarget(w, r, p) {
		return
	}
	if p.Picker != nil && f.Failed() {
		status = http.StatusNotFound
	}
	d.renderRunNew(w, r, p, status)
}

// postRunNewLoad 按表单中（可能改过）的 ref 重新读取 workflow 列表，保留已填写的内容，不创建 run。
func (d *Dashboard) postRunNewLoad(w http.ResponseWriter, r *http.Request, _ *session) {
	p := &runNewPage{Form: dispatchForm(r), RequestID: formRequestID(r)}
	if p.Form.Val("repo") == "" {
		p.Form.fail("repo", "请先选择要构建的仓库")
		if d.fillPicker(w, r, p, "", 1) {
			d.renderRunNew(w, r, p, http.StatusUnprocessableEntity)
		}
		return
	}
	if d.fillTarget(w, r, p) {
		d.renderRunNew(w, r, p, http.StatusOK)
	}
}

// dispatchForm 读取启动构建表单中的非敏感字段（inputs 会保存在 run 中并显示在 Dashboard 上，不是敏感数据）。
func dispatchForm(r *http.Request) *formState {
	f := newForm()
	for _, k := range []string{"repo", "ref", "workflow"} {
		f.Values[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	f.Values["inputs"] = r.PostFormValue("inputs")
	return f
}

// formRequestID 返回表单中的请求 ID，格式不对时生成新的。
func formRequestID(r *http.Request) string {
	if id := r.PostFormValue("request_id"); requestIDPattern.MatchString(id) {
		return id
	}
	return newRequestID()
}

// fillPicker 填充仓库选择器（只列出启用的仓库）。出错时已经写好响应，返回 false。
func (d *Dashboard) fillPicker(w http.ResponseWriter, r *http.Request, p *runNewPage, query string, page int) bool {
	var err error
	p.Picker, err = d.searchRepos(r.Context(), query, page, true, func(n int) string {
		v := url.Values{}
		if query != "" {
			v.Set("q", query)
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return "/runs/new"
		}
		return "/runs/new?" + v.Encode()
	})
	if err != nil {
		d.serverError(w, r, err)
		return false
	}
	return true
}

// fillTarget 检查选定的仓库并读取 ref 下的 workflow。仓库不存在时退回仓库选择器并在 repo 字段上报错。
// 出错时已经写好响应，返回 false。
func (d *Dashboard) fillTarget(w http.ResponseWriter, r *http.Request, p *runNewPage) bool {
	ctx := r.Context()
	f := p.Form
	name := f.Val("repo")
	repo, err := store.FindRepo(ctx, d.db, name)
	switch {
	case errors.Is(err, store.ErrRepoNotFound) || (err != nil && !strings.Contains(name, "/")):
		// 仓库不存在，或者写的不是 owner/name（不可能是任何仓库）
		f.fail("repo", fmt.Sprintf("Kiln 中没有启用的仓库 %s，请重新选择", name))
		return d.fillPicker(w, r, p, "", 1)
	case err != nil:
		d.serverError(w, r, err)
		return false
	}
	p.Repo = repo.FullName()
	f.Values["repo"] = p.Repo
	d.loadTarget(r, p)
	if repo.Incomplete() {
		// 记录来自 installation 事件、还没有默认分支时，读取 workflow 时已经补齐
		if r2, err := store.FindRepo(ctx, d.db, p.Repo); err == nil {
			repo = r2
		}
	}
	p.DefaultBranch = repo.DefaultBranch
	return true
}

// loadTarget 读取 ref 下的 workflow 列表；失败时把原因放在 ref 字段或 TargetErr 上。
func (d *Dashboard) loadTarget(r *http.Request, p *runNewPage) {
	t, err := d.admin.DispatchWorkflows(r.Context(), p.Repo, p.Form.Val("ref"))
	de, isDispatchErr := errors.AsType[*planner.DispatchError](err)
	switch {
	case isDispatchErr && de.Field == "ref":
		p.Form.fail("ref", de.Error())
	case err != nil:
		p.TargetErr = d.failure(r, err)
	default:
		p.Target = t
		for _, w := range t.Workflows {
			if w.Dispatchable {
				p.Dispatchable++
			}
		}
	}
}

func (d *Dashboard) renderRunNew(w http.ResponseWriter, r *http.Request, p *runNewPage, status int) {
	if p.ListURL == "" {
		p.ListURL = "/runs"
		if p.Repo != "" {
			p.ListURL = "/runs?" + url.Values{"repo": {p.Repo}}.Encode()
		}
	}
	// 表单里带着一次性的请求 ID：不让浏览器缓存，返回时重新生成
	w.Header().Set("Cache-Control", "no-store")
	d.renderStatus(w, r, "run_new", "启动构建", p, status)
}

func (d *Dashboard) postDispatch(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	f := dispatchForm(r)
	p := &runNewPage{Form: f, RequestID: formRequestID(r), Attempted: true}
	if f.Val("repo") == "" {
		// 必须明确选定仓库，不替用户挑一个
		f.fail("repo", "请先选择要构建的仓库")
		if d.fillPicker(w, r, p, "", 1) {
			d.renderRunNew(w, r, p, http.StatusUnprocessableEntity)
		}
		return
	}
	if !d.fillTarget(w, r, p) {
		return
	}
	if p.Picker != nil {
		d.renderRunNew(w, r, p, http.StatusUnprocessableEntity)
		return
	}
	inputs, problems := parseInputs(f.Val("inputs"))
	wf := f.Val("workflow")
	if wf == "" {
		f.fail("workflow", "请选择要运行的 workflow")
	} else if p.Target != nil {
		problems = append(problems, checkWorkflow(p.Target, wf, inputs, f)...)
	}
	if len(problems) > 0 {
		f.fail("inputs", strings.Join(problems, "\n"))
	}
	if f.Failed() {
		d.renderRunNew(w, r, p, http.StatusUnprocessableEntity)
		return
	}
	id, err := d.admin.Dispatch(ctx, &admin.DispatchRequest{
		Repo: p.Repo, Workflow: wf, Ref: f.Val("ref"), Inputs: inputs, RequestID: p.RequestID,
		Actor: se.Login, Source: planner.SourceDashboard,
	})
	if err != nil {
		if de, ok := errors.AsType[*planner.DispatchError](err); ok {
			f.fail(de.Field, de.Error())
		} else {
			f.fail(formGeneral, "触发失败："+d.failure(r, err))
		}
		d.renderRunNew(w, r, p, http.StatusUnprocessableEntity)
		return
	}
	d.flashBack(w, r, runURL(id), "已启动构建")
}

// checkWorkflow 检查选定的 workflow 在 ref 下存在、可以手动触发，并按它声明的 inputs 检查填写的 inputs。
// workflow 的问题直接记在表单上，inputs 的问题作为返回值。
func checkWorkflow(t *planner.DispatchTarget, file string, inputs map[string]string, f *formState) []string {
	i := slices.IndexFunc(t.Workflows, func(w planner.DispatchWorkflow) bool {
		return w.File == file || w.File == file+".yml" || w.File == file+".yaml"
	})
	if i < 0 {
		f.fail("workflow", fmt.Sprintf("%s 中没有 .kiln/workflows/%s", t.Ref, file))
		return nil
	}
	w := t.Workflows[i]
	if !w.Dispatchable {
		f.fail("workflow", w.File+" 没有声明 on.workflow_dispatch，不能手动触发")
		return nil
	}
	f.Values["workflow"] = w.File
	return workflow.CheckDispatchInputs(w.Inputs, inputs)
}

var inputNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// parseInputs 解析"每行一个 key=value"的 inputs：值可以包含 =，空行忽略。
// 没有 =、名称为空或不合法、名称重复的行都作为问题返回（行号从 1 开始），不会被静默丢弃。
func parseInputs(text string) (map[string]string, []string) {
	inputs := map[string]string{}
	seen := map[string]int{}
	var problems []string
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("第 %d 行缺少 =，应写成 key=value：%s", n, line))
		case k == "":
			problems = append(problems, fmt.Sprintf("第 %d 行缺少名称（= 前面为空）", n))
		case !inputNamePattern.MatchString(k):
			problems = append(problems, fmt.Sprintf("第 %d 行的名称 %s 不合法：只能包含字母、数字、_ 和 -，且不能以数字或 - 开头", n, k))
		case seen[k] > 0:
			problems = append(problems, fmt.Sprintf("第 %d 行的 %s 与第 %d 行重复", n, k, seen[k]))
		default:
			seen[k] = n
			inputs[k] = strings.TrimSpace(v)
		}
	}
	return inputs, problems
}

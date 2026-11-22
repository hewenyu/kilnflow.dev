package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func testRepoSearch() *repoSearch {
	return &repoSearch{
		Query: "de", Page: 2, Pages: 3, Total: 120,
		Repos:   []repoView{{FullName: "tester/demo", DefaultBranch: "main", Active: true}, {FullName: "tester/old"}},
		PrevURL: "/settings/repos?page=1", NextURL: "/settings/repos?page=3",
	}
}

// testTarget 是 tester/demo 的 main 下的 workflow：ci.yml 可以手动触发并声明了 inputs，push.yml 不能。
func testTarget() *planner.DispatchTarget {
	return &planner.DispatchTarget{
		Ref: "refs/heads/main", SHA: "0123456789abcdef0123",
		Workflows: []planner.DispatchWorkflow{
			{File: "ci.yml", Name: "CI", Dispatchable: true, ConfigError: "bad", Inputs: []workflow.DispatchInput{
				{Name: "version", Type: "string", Required: true, Description: "要发布的版本"},
				{Name: "mode", Type: "choice", Default: "fast", Options: []string{"fast", "full"}},
				{Name: "dry_run", Type: "boolean", Default: "false"},
			}},
			{File: "push.yml"},
		},
	}
}

// testRunNew 是校验失败后重新渲染的启动构建表单。
func testRunNew() *runNewPage {
	f := newForm()
	maps.Copy(f.Values, map[string]string{"repo": "tester/demo", "ref": "main", "workflow": "ci.yml", "inputs": "version=1.2.3\nbroken"})
	f.fail("inputs", "第 2 行缺少 =，应写成 key=value：broken")
	return &runNewPage{
		Repo: "tester/demo", FromFilter: true, DefaultBranch: "main", Target: testTarget(), Dispatchable: 1,
		Form: f, Attempted: true, RequestID: newRequestID(), ListURL: "/runs?repo=tester%2Fdemo",
	}
}

// renderData 把 data 编码为页面 page 的数据（JSON），返回 JSON 文本。
func renderData(t *testing.T, page string, data any) string {
	t.Helper()
	return renderJSON(t, page, &session{Login: "alice", Role: "user", Exp: time.Now().Add(time.Hour).Unix()}, data)
}

// renderJSON 以 se 的身份把 data 编码为页面 page 的数据（与 renderStatus 发送给前端的相同），返回 JSON 文本。
func renderJSON(t *testing.T, page string, se *session, data any) string {
	t.Helper()
	d := &Dashboard{log: slog.New(slog.NewTextHandler(t.Output(), nil))}
	w := httptest.NewRecorder()
	d.writeJSON(w, http.StatusOK, &uiPage{Page: page, Title: page, Session: newUISession(se), CSRF: "csrf", Nav: page, Status: http.StatusOK, Data: data})
	dumpUI("render "+page, w)
	if w.Code != http.StatusOK {
		t.Fatalf("编码 %s：HTTP %d：%s", page, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func mustHave(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, s := range wants {
		if !strings.Contains(body, s) {
			t.Errorf("%s 中没有 %q", what, s)
		}
	}
}

// runNewData 是启动构建页的页面数据（runNewPage），另带表单的 Failed。
type runNewData struct {
	runNewPage
	Form struct {
		Values, Errors map[string]string
		Failed         bool
	}
}

func TestRunNewRender(t *testing.T) {
	// 校验失败：页面数据保留输入、带着字段的错误（Form.Failed）、各 workflow 能否手动触发和声明的 inputs
	p := pageData[runNewData](t, renderData(t, "run_new", testRunNew()))
	want := map[string]string{"repo": "tester/demo", "ref": "main", "workflow": "ci.yml", "inputs": "version=1.2.3\nbroken"}
	expect(t, "校验失败的表单", p.Form, maps.Equal(p.Form.Values, want), p.Form.Failed, strings.Contains(p.Form.Errors["inputs"], "第 2 行缺少 ="))
	expect(t, "校验失败的启动构建页", p.runNewPage, p.Attempted, p.FromFilter, p.Dispatchable == 1, p.RequestID != "", p.ListURL == "/runs?repo=tester%2Fdemo", p.Picker == nil)
	if wf := p.Target.Workflows; expect(t, "workflow 列表", p.Target, len(wf) == 2 && len(wf[0].Inputs) == 3) {
		expect(t, "workflow 列表", wf, wf[0].Dispatchable, wf[0].ConfigError == "bad", !wf[1].Dispatchable, wf[0].Inputs[0].Required,
			slices.Equal(wf[0].Inputs[1].Options, []string{"fast", "full"}))
	}

	// 没有选定仓库：只有仓库选择器（Picker），没有 Target
	f := newForm()
	f.fail("repo", "请先选择要构建的仓库")
	p = pageData[runNewData](t, renderData(t, "run_new", &runNewPage{Picker: testRepoSearch(), Form: f, Attempted: true, ListURL: "/runs"}))
	if expect(t, "仓库选择器", p, p.Picker != nil, p.Target == nil, p.Form.Errors["repo"] == "请先选择要构建的仓库") {
		s := p.Picker
		expect(t, "仓库选择器", s, s.Page == 2, s.Pages == 3, len(s.Repos) == 2 && s.Repos[0].FullName == "tester/demo")
	}

	// ref 找不到：没有 Target（退回手填文件名），保留 workflow
	f = newForm()
	maps.Copy(f.Values, map[string]string{"repo": "tester/demo", "ref": "nope", "workflow": "ci.yml"})
	f.fail("ref", "找不到 ref nope")
	p = pageData[runNewData](t, renderData(t, "run_new", &runNewPage{Repo: "tester/demo", DefaultBranch: "main", Form: f, RequestID: "x", ListURL: "/runs"}))
	if p.Target != nil || p.Form.Errors["ref"] != "找不到 ref nope" || p.Form.Values["workflow"] != "ci.yml" || p.DefaultBranch != "main" {
		t.Errorf("ref 错误：%+v", p)
	}

	// 没有可以手动触发的 workflow：Dispatchable 为 0（前端说明原因，不提供启动按钮）
	tg := testTarget()
	tg.Workflows = tg.Workflows[1:]
	p = pageData[runNewData](t, renderData(t, "run_new", &runNewPage{Repo: "tester/demo", Target: tg, Form: newForm(), RequestID: "x", ListURL: "/runs"}))
	if p.Dispatchable != 0 || len(p.Target.Workflows) != 1 || p.Form.Failed {
		t.Errorf("没有可手动触发的 workflow：%+v", p)
	}
}

// runsData 是运行列表的页面数据（runsPage），另带计算的 Filter.Active、Mine 和各 run 的 Start、End。
type runsData struct {
	runsPage
	Filter struct {
		Repo, State, Workflow, Ref, Actor string
		Active                            bool
	}
	Runs []struct {
		ID         int64
		State      store.RunState
		PRNumber   *int
		Ref, SHA   string
		Repo, URL  string
		GitHub     string
		Start, End int64
	}
	Mine bool
}

func TestRunsRender(t *testing.T) {
	// 区分"还没有构建"和"筛选后没有结果"：Filter.Active
	p := pageData[runsData](t, renderData(t, "runs", &runsPage{States: runStateOptions, LatestURL: "/runs", Self: "/runs", NewURL: "/runs/new"}))
	if p.Filter.Active || len(p.Runs) != 0 || p.NewURL != "/runs/new" {
		t.Errorf("没有构建：%+v", p)
	}
	p = pageData[runsData](t, renderData(t, "runs", &runsPage{Filter: runFilter{State: "failed"}, States: runStateOptions, LatestURL: "/runs?state=failed", Self: "/runs?state=failed", NewURL: "/runs/new"}))
	if !p.Filter.Active || p.Filter.State != "failed" || len(p.Runs) != 0 || !slices.Contains(p.States, option{Value: "failed", Label: "失败"}) {
		t.Errorf("筛选无结果：%+v", p)
	}

	now := time.Now()
	run := &cistore.Run{ID: 9, Workflow: "ci.yml", Event: "pull_request", Ref: "refs/heads/feat", SHA: "abcdef0123456789", State: store.RunFailed, PRNumber: new(3), CreatedAt: now}
	p = pageData[runsData](t, renderData(t, "runs", &runsPage{
		Filter: runFilter{Repo: "demo"}, States: runStateOptions, Runs: []runView{{Run: run, Repo: "tester/demo", URL: "/runs/9?back=%2Fruns%3Frepo%3Ddemo", GitHub: "https://github.com"}},
		LatestURL: "/runs?repo=demo", Self: "/runs?repo=demo", NewURL: "/runs/new", GitHub: "https://github.com",
	}))
	if len(p.Runs) != 1 {
		t.Fatalf("列表：%+v", p.Runs)
	}
	// run 的字段（cistore.Run 内嵌）和计算的 Start、End（没有结束时为 0）
	if r := p.Runs[0]; r.ID != 9 || r.URL != "/runs/9?back=%2Fruns%3Frepo%3Ddemo" || r.State != store.RunFailed || r.PRNumber == nil || *r.PRNumber != 3 ||
		r.Ref != "refs/heads/feat" || r.SHA != "abcdef0123456789" || r.Repo != "tester/demo" || r.GitHub != "https://github.com" || r.Start != now.Unix() || r.End != 0 {
		t.Errorf("列表中的 run：%+v", r)
	}
	if p.Filter.Repo != "demo" || p.RepoExact != "" || p.Mine {
		t.Errorf("按仓库名的一部分筛选：%+v", p)
	}
}

func TestParseInputs(t *testing.T) {
	got, problems := parseInputs("a=1\r\n\n  b = x=y  \nnoeq\n=v\n9x=1\na=2\nc=\n")
	if want := map[string]string{"a": "1", "b": "x=y", "c": ""}; !maps.Equal(got, want) {
		t.Errorf("inputs = %v，应为 %v", got, want)
	}
	want := []string{
		"第 4 行缺少 =，应写成 key=value：noeq",
		"第 5 行缺少名称（= 前面为空）",
		"第 6 行的名称 9x 不合法：只能包含字母、数字、_ 和 -，且不能以数字或 - 开头",
		"第 7 行的 a 与第 1 行重复",
	}
	if !slices.Equal(problems, want) {
		t.Errorf("问题 = %q\n应为 %q", problems, want)
	}
	if got, problems := parseInputs(" \n"); len(got) != 0 || len(problems) != 0 {
		t.Errorf("空白 inputs：%v %v", got, problems)
	}
}

func TestCheckWorkflow(t *testing.T) {
	tg := testTarget()
	f := newForm()
	got := checkWorkflow(tg, "ci", map[string]string{"mode": "slow", "dry_run": "yes", "extra": "1"}, f)
	want := []string{"缺少必填的 version", "mode 只能是 fast、full 之一", "dry_run 只能是 true 或 false", "workflow 没有声明 extra"}
	if !slices.Equal(got, want) || f.Failed() || f.Val("workflow") != "ci.yml" {
		t.Errorf("问题 = %q（应为 %q），表单 %v", got, want, f.Errors)
	}
	if got := checkWorkflow(tg, "ci.yml", map[string]string{"version": "1", "dry_run": "TRUE"}, newForm()); len(got) != 0 {
		t.Errorf("合法的 inputs 报了问题：%q", got)
	}
	f = newForm()
	checkWorkflow(tg, "push.yml", nil, f)
	if !strings.Contains(f.Err("workflow"), "没有声明 on.workflow_dispatch") {
		t.Errorf("不能手动触发的 workflow：%v", f.Errors)
	}
	f = newForm()
	checkWorkflow(tg, "nope.yml", nil, f)
	if !strings.Contains(f.Err("workflow"), "refs/heads/main 中没有 .kiln/workflows/nope.yml") {
		t.Errorf("不存在的 workflow：%v", f.Errors)
	}
}

func TestRunFilterURL(t *testing.T) {
	f := readRunFilter(url.Values{"repo": {" tester/demo "}, "state": {"bogus"}, "workflow": {"ci"}, "ref": {"main"}})
	if f != (runFilter{Repo: "tester/demo", Workflow: "ci", Ref: "main"}) {
		t.Errorf("筛选条件 = %+v（未知状态应被忽略）", f)
	}
	if got, want := f.url("before", 42), "/runs?before=42&ref=main&repo=tester%2Fdemo&workflow=ci"; got != want {
		t.Errorf("url = %q，应为 %q", got, want)
	}
	if got := (runFilter{}).url("", 0); got != "/runs" {
		t.Errorf("没有筛选时 url = %q", got)
	}
	for in, want := range map[string]string{
		"/runs": "/runs", "/runs?state=failed": "/runs?state=failed", "/runsx": "", "/settings": "", "//evil.com/runs": "", "https://evil.com/runs?": "",
	} {
		if got := runBackURL(in); got != want {
			t.Errorf("runBackURL(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// ---- 连接数据库的功能测试 ----

// listedRuns 返回运行列表页面数据中的 run ID（按列表顺序）。
func listedRuns(t *testing.T, body string) []int64 {
	t.Helper()
	var out []int64
	for _, r := range pageData[runsData](t, body).Runs {
		out = append(out, r.ID)
	}
	return out
}

// newestFirst 返回 ids[from:to] 倒序排列的副本。
func newestFirst(ids []int64, from, to int) []int64 {
	out := slices.Clone(ids[from:to])
	slices.Reverse(out)
	return out
}

// seedRuns 写入运行列表的测试数据，返回 tester/demo 的 55 个 run 和 tester/other 的 3 个 run 的 ID（按创建顺序）：
// tester/demo 中每 5 个有一个是 dev 分支上失败的 push.yml，其余是 main 上成功的 build.yml（name 为 Build）；
// tester/other 的都是 tag v1 上成功的 build.yml。
func (e *webEnv) seedRuns() (demo, other []int64) {
	e.t.Helper()
	e.addRepo(502, "other")
	for i := range 55 {
		o := runOpts{workflow: "build.yml", name: "Build"}
		if i%5 == 0 {
			o = runOpts{workflow: "push.yml", ref: "refs/heads/dev", state: store.RunFailed}
		}
		demo = append(demo, e.addRun(o))
	}
	for range 3 {
		other = append(other, e.addRun(runOpts{repoID: 502, workflow: "build.yml", name: "Build", ref: "refs/tags/v1"}))
	}
	return demo, other
}

func TestPageRuns_Paging(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	demo, other := e.seedRuns()
	all := append(slices.Clone(demo), other...)

	for _, c := range []struct {
		name         string
		target       string
		want         []int64
		newer, older string
	}{
		{"最新一页", "/runs", newestFirst(all, 8, 58), "", fmt.Sprintf("before=%d", all[8])},
		{"较早一页（到底）", fmt.Sprintf("/runs?before=%d", all[8]), newestFirst(all, 0, 8), fmt.Sprintf("after=%d", all[7]), ""},
		// 紧挨着游标、较新的 50 个
		{"较新一页（还有更新的）", fmt.Sprintf("/runs?after=%d", all[2]), newestFirst(all, 3, 53), fmt.Sprintf("after=%d", all[52]), fmt.Sprintf("before=%d", all[3])},
		{"较新一页（到最新）", fmt.Sprintf("/runs?after=%d", all[7]), newestFirst(all, 8, 58), "", fmt.Sprintf("before=%d", all[8])},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.get(e.bob, c.target)
			wantStatus(t, c.name, w, http.StatusOK)
			body := w.Body.String()
			if got := listedRuns(t, body); !slices.Equal(got, c.want) {
				t.Errorf("列出的 run = %v，应为 %v", got, c.want)
			}
			p := pageData[runsData](t, body)
			for name, c := range map[string][2]string{"较新": {p.NewerURL, c.newer}, "较早": {p.OlderURL, c.older}} {
				got, q := c[0], c[1]
				if (q == "" && got != "") || (q != "" && got != "/runs?"+q) {
					t.Errorf("%s一页的地址 %q，应为 %q", name, got, q)
				}
			}
		})
	}
	// 翻页后详情页的"返回运行列表"带着当前页
	p := pageData[runsData](t, e.get(e.bob, fmt.Sprintf("/runs?before=%d", all[8])).Body.String())
	if want := fmt.Sprintf("/runs/%d?back=%%2Fruns%%3Fbefore%%3D%d", all[7], all[8]); len(p.Runs) == 0 || p.Runs[0].URL != want || !p.Paged {
		t.Errorf("翻页后的详情页地址应为 %q：%+v", want, p.Runs)
	}
}

func TestPageRuns_Filters(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	demo, other := e.seedRuns()
	var failed, built []int64
	for i, id := range demo {
		if i%5 == 0 {
			failed = append(failed, id)
		} else {
			built = append(built, id)
		}
	}
	rev := func(ids ...[]int64) []int64 {
		out := slices.Concat(ids...)
		slices.Sort(out)
		slices.Reverse(out)
		return out
	}

	for _, c := range []struct {
		name, query string
		want        []int64
		// newURL 是"启动构建"的地址（仓库全名精确匹配时预选这个仓库），为空时不检查
		newURL string
	}{
		{"仓库全名（精确匹配，预选启动构建的仓库）", "repo=Tester/Demo", rev(demo)[:50], "/runs/new?from=filter&repo=tester%2Fdemo"},
		{"仓库名的一部分", "repo=othe", rev(other), "/runs/new"},
		{"状态", "state=failed", rev(failed), ""},
		{"workflow 文件名省略 .yml", "workflow=push", rev(failed), ""},
		{"workflow 的 name", "workflow=Build", rev(built, other), ""},
		{"分支名", "ref=dev", rev(failed), ""},
		{"tag 名", "ref=v1", rev(other), ""},
		{"完整的 ref", "ref=refs%2Ftags%2Fv1", rev(other), ""},
		{"组合条件", "repo=tester%2Fdemo&state=succeeded&ref=main", rev(built), ""},
		{"未知的状态被忽略", "state=bogus", rev(demo, other)[:50], ""},
		{"不存在的仓库", "repo=tester%2Fnope", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.get(e.bob, "/runs?"+c.query)
			wantStatus(t, c.name, w, http.StatusOK)
			body := w.Body.String()
			if got := listedRuns(t, body); !slices.Equal(got, c.want) {
				t.Errorf("列出的 run = %v，应为 %v", got, c.want)
			}
			p := pageData[runsData](t, body)
			if c.newURL != "" && p.NewURL != c.newURL {
				t.Errorf("启动构建的地址 %q，应为 %q", p.NewURL, c.newURL)
			}
			// 有筛选条件（未知的状态被忽略）时，没有结果显示为"没有符合筛选条件的构建"
			if p.Filter.Active != (c.query != "state=bogus") {
				t.Errorf("Filter.Active = %v", p.Filter.Active)
			}
		})
	}

	// 筛选表单提交的空字段被去掉
	if got := redirectTarget(t, "去掉空字段", e.get(e.bob, "/runs?repo=&state=failed&workflow=+")); got != "/runs?state=failed" {
		t.Errorf("跳转到 %q", got)
	}
}

func TestPageRunNew(t *testing.T) {
	t.Parallel()
	e := newGitHubWebEnv(t)
	e.addRepo(502, "other")
	e.exec(`UPDATE repos SET active=false WHERE id=502`)

	// 没有选定仓库：仓库选择器只列出启用的仓库
	w := e.get(e.bob, "/runs/new")
	wantStatus(t, "仓库选择器", w, http.StatusOK)
	body := w.Body.String()
	if p := pageData[runNewData](t, body); p.Picker == nil || len(p.Picker.Repos) != 1 || p.Picker.Repos[0].FullName != "tester/demo" {
		t.Errorf("仓库选择器：%+v", p.Picker)
	}
	if strings.Contains(body, "tester/other") {
		t.Errorf("仓库选择器不应列出已停用的仓库")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("启动构建页带着一次性的请求 ID，不应缓存")
	}

	// 仓库不存在：404，退回仓库选择器并说明
	w = e.get(e.bob, "/runs/new?repo=tester%2Fnope")
	wantStatus(t, "仓库不存在", w, http.StatusNotFound)
	mustHave(t, "仓库不存在", w.Body.String(), "Kiln 中没有启用的仓库 tester/nope")

	// 选定仓库：列出 workflow，只有声明了 workflow_dispatch 的可以选择
	w = e.get(e.bob, "/runs/new?repo=tester%2Fdemo&from=filter")
	wantStatus(t, "选定仓库", w, http.StatusOK)
	p := pageData[runNewData](t, w.Body.String())
	dispatchable := map[string]bool{}
	if p.Target != nil {
		for _, wf := range p.Target.Workflows {
			dispatchable[wf.File] = wf.Dispatchable
		}
	}
	if p.Repo != testRepo || !p.FromFilter || !dispatchable["build.yml"] || dispatchable["push.yml"] || len(dispatchable) < 2 {
		t.Errorf("选定仓库：Repo %q FromFilter %v，workflow %v", p.Repo, p.FromFilter, dispatchable)
	}
	if !requestIDPattern.MatchString(p.RequestID) {
		t.Errorf("表单应带着请求 ID：%q", p.RequestID)
	}

	// ref 找不到：在 ref 字段旁说明
	w = e.get(e.bob, "/runs/new?repo=tester%2Fdemo&ref=nope")
	wantStatus(t, "ref 找不到", w, http.StatusOK)
	if p := pageData[runNewData](t, w.Body.String()); !strings.Contains(p.Form.Errors["ref"], "找不到 ref nope") {
		t.Errorf("ref 找不到：%+v", p.Form)
	}

	// 按表单中改过的 ref 重新读取：保留已填写的内容，不创建 run
	gitRun(t, e.repoDir, "tag", "v1.0.0")
	w = e.post(e.bob, "/runs/new", url.Values{"repo": {testRepo}, "ref": {"v1.0.0"}, "inputs": {"channel=beta"}})
	wantStatus(t, "重新读取", w, http.StatusOK)
	if p := pageData[runNewData](t, w.Body.String()); p.Form.Values["ref"] != "v1.0.0" || p.Form.Values["inputs"] != "channel=beta" || p.Form.Failed {
		t.Errorf("重新读取应保留已填写的内容：%+v", p.Form)
	}
	w = e.post(e.bob, "/runs/new", url.Values{"ref": {"main"}})
	wantStatus(t, "重新读取时没有仓库", w, http.StatusUnprocessableEntity)
	mustHave(t, "重新读取时没有仓库", w.Body.String(), "请先选择要构建的仓库")
	if n := e.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("读取 workflow 不应创建 run")
	}
}

func TestPostDispatch(t *testing.T) {
	t.Parallel()
	e := newGitHubWebEnv(t)
	const reqID = "0123456789abcdef0123456789abcdef"
	form := func(kv ...string) url.Values {
		f := url.Values{"repo": {testRepo}, "workflow": {"build.yml"}, "request_id": {reqID}}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}

	// 校验失败：422，在字段旁说明并保留输入，不创建 run
	for _, c := range []struct {
		name  string
		form  url.Values
		wants []string
		// field、value 是保留的输入（为空时不检查）
		field, value string
	}{
		{"没有选择仓库", form("repo", ""), []string{"请先选择要构建的仓库"}, "", ""},
		{"仓库不存在", form("repo", "tester/nope"), []string{"Kiln 中没有启用的仓库 tester/nope"}, "", ""},
		{"没有选择 workflow", form("workflow", ""), []string{"请选择要运行的 workflow"}, "", ""},
		{"workflow 不存在", form("workflow", "nope.yml"), []string{".kiln/workflows/nope.yml"}, "workflow", "nope.yml"},
		{"workflow 不能手动触发", form("workflow", "push.yml"), []string{"push.yml 没有声明 on.workflow_dispatch"}, "workflow", "push.yml"},
		{"inputs 缺少 =", form("inputs", "channel=beta\nbroken"), []string{"第 2 行缺少 ="}, "inputs", "channel=beta\nbroken"},
		{"inputs 取值不对", form("inputs", "channel=nightly"), []string{"channel 只能是 stable、beta 之一"}, "inputs", "channel=nightly"},
		{"ref 找不到", form("ref", "nope"), []string{"找不到 ref nope"}, "ref", "nope"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.post(e.bob, "/dispatch", c.form)
			wantStatus(t, c.name, w, http.StatusUnprocessableEntity)
			mustHave(t, c.name, w.Body.String(), c.wants...)
			// Attempted：错误摘要说明没有启动构建
			p := pageData[runNewData](t, w.Body.String())
			if !p.Form.Failed || !p.Attempted {
				t.Errorf("校验失败的表单：Failed %v Attempted %v", p.Form.Failed, p.Attempted)
			}
			if c.field != "" && p.Form.Values[c.field] != c.value {
				t.Errorf("应保留 %s 的输入 %q：%q", c.field, c.value, p.Form.Values[c.field])
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("校验失败不应创建 run（%d 个）", n)
	}

	// 成功：进入新的 run；成员也可以启动构建，操作者为登录用户
	w := e.post(e.bob, "/dispatch", form("inputs", "channel=beta\n\n"))
	var id int64
	if err := e.db.QueryRow(e.ctx, `SELECT id FROM runs`).Scan(&id); err != nil {
		t.Fatalf("应创建 run：%v", err)
	}
	if flash := wantRedirect(t, "启动构建", w, runURL(id)); flash != "已启动构建" {
		t.Errorf("提示 = %q", flash)
	}
	run, err := cistore.GetRun(e.ctx, e.db, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.SenderLogin != "bob" || run.Source != planner.SourceDashboard || run.Inputs["channel"] != "beta" || run.Ref != "refs/heads/main" {
		t.Errorf("run = %+v", run)
	}

	// 重复提交同一个表单（同一个请求 ID）：回到同一个 run，不再创建
	w = e.post(e.bob, "/dispatch", form("inputs", "channel=beta"))
	wantRedirect(t, "重复提交", w, runURL(id))
	if n := e.count(`SELECT count(*) FROM runs`); n != 1 {
		t.Errorf("重复提交创建了 %d 个 run", n)
	}
	// 请求 ID 格式不对时生成新的：再创建一个
	w = e.post(e.bob, "/dispatch", form("request_id", "bad"))
	if got, _ := url.Parse(redirectTarget(t, "新的请求 ID", w)); got == nil || !strings.HasPrefix(got.Path, "/runs/") || got.Path == runURL(id) {
		t.Errorf("新的请求 ID 应进入新的 run：%q", got)
	}
	if n := e.count(`SELECT count(*) FROM runs`); n != 2 {
		t.Errorf("新的请求 ID 应创建新的 run（共 %d 个）", n)
	}

	// CSRF 不对：403，不创建 run
	w = e.post(e.bob, "/dispatch", form("csrf", "bad", "request_id", "ffffffffffffffffffffffffffffffff"))
	wantStatus(t, "CSRF 失败", w, http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM runs`); n != 2 {
		t.Errorf("CSRF 失败时不应创建 run")
	}
}

// jsonTime 返回时间在页面数据（JSON）中的写法（不含引号）。
func jsonTime(t time.Time) string {
	b, _ := json.Marshal(t)
	return strings.Trim(string(b), `"`)
}

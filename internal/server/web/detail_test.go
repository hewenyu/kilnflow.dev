package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// testRunPage 是正在查看历史（首次运行）的 run 详情页，最新一次（第 1 次重跑）还在执行。
func testRunPage(run *cistore.Run, jv jobView) *runPage {
	reruns := []rerunView{{N: 0, State: store.RunFailed}, {N: 1, State: run.State}}
	return &runPage{
		Run: run, Repo: "tester/demo", Back: "/runs?state=failed", Reruns: reruns, Viewed: &reruns[0],
		Jobs: []jobView{jv}, Stages: jobStages([]jobView{jv}), States: []string{string(jv.State)}, Active: run.State.Open(),
		Activity: []activityEntry{
			{ID: 1, Actor: "bob", Action: "run.rerun", Detail: map[string]any{"run_id": float64(run.ID), "rerun": float64(1), "mode": "failed"}, At: run.CreatedAt},
		},
	}
}

// testJobPage 是排队中的发布 job 页面。
func testJobPage(run *cistore.Run, jv jobView) *jobPage {
	return &jobPage{
		Job: jv, Run: run, Repo: "tester/demo", Spec: "{}", RunActive: run.State.Open(),
		Attempts: []attemptView{{ID: 5, N: 1, Epoch: 1, Runner: "mini", Outcome: "failed", ExitCode: new(3), Started: time.Now().Unix()}},
		Release: &releaseView{
			Target: "tester/demo-dist", Tag: "v1.2.0", State: "done", Stage: "assets",
			Progress: map[string]any{"html_url": "https://example.com/r"}, Error: "x",
		},
		LogEmpty: message(msgJobLogPublishPending),
	}
}

func testJobs() []*cistore.Job {
	man := "sha256:" + strings.Repeat("a", 64)
	return []*cistore.Job{
		{ID: 1, DisplayName: "build", State: store.JobSucceeded, ResultManifest: &man},
		{ID: 2, DisplayName: "lint", State: store.JobSucceeded, ResultManifest: &man},
		{ID: 3, DisplayName: "test", State: store.JobFailed, Needs: []int64{1}},
		{ID: 4, DisplayName: "package", State: store.JobSkipped, Needs: []int64{3}},
		{ID: 5, DisplayName: "publish", Kind: workflow.JobKindPublish, State: store.JobSkipped, Needs: []int64{4, 2}},
		{ID: 6, DisplayName: "deploy", State: store.JobSucceeded, ResultManifest: &man, Effects: true},
	}
}

func TestNewRerunForm(t *testing.T) {
	f := newRerunForm(testJobs(), false)
	if f.Default != store.RerunFailed || f.Total != 6 || f.Retry != 3 || f.Kept() != 3 || f.Publish != 1 || f.Effects != 1 {
		t.Errorf("有失败的 job：%+v", f)
	}
	// 全部成功：不默认"只重跑未成功的 job"
	man := "sha256:" + strings.Repeat("b", 64)
	ok := []*cistore.Job{
		{ID: 1, State: store.JobSucceeded, ResultManifest: &man},
		{ID: 2, Kind: workflow.JobKindPublish, State: store.JobSucceeded},
	}
	f = newRerunForm(ok, false)
	if f.Default != store.RerunAll || f.Retry != 0 || f.Kept() != 2 {
		t.Errorf("全部成功：%+v", f)
	}
	// 产物已经清理：成功的 job 也要重新规划，只有成功的发布 job 沿用
	f = newRerunForm(ok, true)
	if f.Default != store.RerunFailed || f.Retry != 1 || f.Kept() != 1 || !f.Purged {
		t.Errorf("已清理：%+v", f)
	}
}

func TestNewJobRerun(t *testing.T) {
	jobs := testJobs()
	r := newJobRerun(jobs[0], jobs, false)
	if !r.Latest || !r.Force || !slices.Equal(r.Downstream, []string{"test", "package", "publish"}) || r.Kept != 2 || !r.Publish {
		t.Errorf("重跑 build：%+v", r)
	}
	r = newJobRerun(jobs[1], jobs, false)
	if !slices.Equal(r.Downstream, []string{"publish"}) || r.Kept != 4 || !r.Publish {
		t.Errorf("重跑 lint：%+v", r)
	}
	r = newJobRerun(jobs[2], jobs, false)
	if r.Force || len(r.Downstream) != 2 {
		t.Errorf("重跑失败的 test 不应强制执行：%+v", r)
	}
	r = newJobRerun(jobs[5], jobs, false)
	if len(r.Downstream) != 0 || r.Kept != 5 || r.Publish {
		t.Errorf("没有下游：%+v", r)
	}
	// 产物已经清理：有产物的其他 job（build、lint）不能沿用，重新规划
	r = newJobRerun(jobs[5], jobs, true)
	if len(r.Downstream) != 0 || r.Kept != 3 || r.Replan != 2 {
		t.Errorf("已清理：%+v", r)
	}
}

func TestParseRerun(t *testing.T) {
	for in, want := range map[string]int{"": 2, "0": 0, "2": 2} {
		if n, ok := parseRerun(in, 2); !ok || n != want {
			t.Errorf("parseRerun(%q) = %d %v", in, n, ok)
		}
	}
	for _, in := range []string{"3", "-1", "x", "1.0", "999999999999999999999"} {
		if _, ok := parseRerun(in, 2); ok {
			t.Errorf("parseRerun(%q) 应无效", in)
		}
	}
}

func TestSelectAttempt(t *testing.T) {
	newPage := func() *jobPage {
		return &jobPage{
			Job:      jobView{Job: &cistore.Job{ID: 9}},
			Attempts: []attemptView{{ID: 10, N: 1}, {ID: 12, N: 2, Current: true}},
			Attempt:  12,
		}
	}
	p := newPage()
	p.selectAttempt("")
	if p.Attempt != 12 || p.Explicit || p.Viewing != nil || !p.AttemptErr.IsZero() || p.AttemptKey() != "current" {
		t.Errorf("没有指定 attempt：%+v", p)
	}
	p = newPage()
	p.selectAttempt("10")
	if p.Attempt != 10 || !p.Explicit || p.Viewing == nil || p.Viewing.N != 1 || p.AttemptKey() != "10" {
		t.Errorf("历史尝试：%+v", p)
	}
	p = newPage()
	p.selectAttempt("12")
	if p.Attempt != 12 || !p.Explicit || p.Viewing != nil {
		t.Errorf("指定当前尝试：%+v", p)
	}
	for _, in := range []string{"999999999", "11", "0", "-3", "abc"} {
		p = newPage()
		p.selectAttempt(in)
		if p.AttemptErr.IsZero() || p.Attempt != 12 || p.Explicit {
			t.Errorf("attempt=%s 应无效：%+v", in, p)
		}
	}
	p = newPage()
	p.Job.Kind = workflow.JobKindPublish
	p.selectAttempt("12")
	if got := textZH(t, p.AttemptErr); !strings.Contains(got, "发布 job") {
		t.Errorf("发布 job 没有尝试：%q", got)
	}
}

func TestLogEmptyText(t *testing.T) {
	prev := int64(7)
	cases := []struct {
		j      *cistore.Job
		has    bool
		purged bool
		want   string
	}{
		{&cistore.Job{State: store.JobSucceeded}, true, false, "这次尝试没有输出日志"},
		{&cistore.Job{State: store.JobSucceeded, ReusedFrom: &prev, CachedFrom: &prev}, false, false, "沿用上一次运行的结果"},
		{&cistore.Job{State: store.JobSucceeded, CachedFrom: &prev}, false, false, "命中缓存"},
		{&cistore.Job{State: store.JobSkipped}, false, false, "被跳过"},
		{&cistore.Job{State: store.JobPending}, false, false, "还没有开始执行"},
		{&cistore.Job{State: store.JobCancelled}, false, false, "没有执行"},
		{&cistore.Job{Kind: workflow.JobKindPublish, State: store.JobPending}, false, false, "发布还没有开始"},
		// 日志已经按保留策略清理：执行过的 job 和发布 job 说明清理；没有执行过的仍然说明原因
		{&cistore.Job{State: store.JobFailed}, true, true, "日志已按保留策略清理"},
		{&cistore.Job{Kind: workflow.JobKindPublish, State: store.JobSucceeded}, false, true, "日志已按保留策略清理"},
		{&cistore.Job{State: store.JobSucceeded, CachedFrom: &prev}, false, true, "命中缓存"},
	}
	for _, c := range cases {
		if got := textZH(t, logEmptyText(c.j, c.has, c.purged)); !strings.Contains(got, c.want) {
			t.Errorf("logEmptyText(%s) = %q，应包含 %q", c.j.State, got, c.want)
		}
	}
}

// runData 是 run 详情页的页面数据（runPage），另带计算的 History、Ref、Start、End，重跑表单的 Kept 和活动的说明。
type runData struct {
	runPage
	Form *struct {
		rerunForm
		Kept int
	}
	Activity []struct {
		Actor, Action, Text, Link string
		RunText                   uiText
	}
	Start, End int64
	Ref        string
	History    bool
}

func TestRunDetailRender(t *testing.T) {
	now := time.Now()
	run := &cistore.Run{ID: 7, Workflow: "ci.yml", Event: "push", Ref: "refs/heads/main", SHA: "0123456789abcdef0123", State: store.RunActive, CurrentRerun: 1, CreatedAt: now}
	jv := jobView{Job: &cistore.Job{ID: 11, DisplayName: "build", State: store.JobRunning, CachedFrom: new(int64(3))}}

	// 正在执行：可以取消（Active），没有重跑表单
	p := testRunPage(run, jv)
	p.Viewed = &p.Reruns[1]
	body := renderData(t, "run", p)
	d := pageData[runData](t, body)
	expect(t, "执行中", d, d.Active, d.Form == nil, !d.History, d.Ref == "refs/heads/main", d.Start == now.Unix(), d.End == 0,
		d.Back == "/runs?state=failed", len(d.Reruns) == 2, d.Viewed != nil && d.Viewed.N == 1)
	expect(t, "job 列表", d.Jobs, len(d.Jobs) == 1 && d.Jobs[0].CachedFrom != nil && *d.Jobs[0].CachedFrom == 3, len(d.Stages) == 1)
	if expect(t, "活动", d.Activity, len(d.Activity) == 1) {
		a := d.Activity[0]
		expect(t, "活动", a, a.Actor == "bob", a.Link == "/runs/7", textZH(t, a.RunText) == "重跑了这个 run（只重跑未成功的 job），开始第 1 次重跑", a.Text == "重跑了 run #7（第 1 次重跑）")
	}
	// job 列表不带 spec（只在 job 详情页）
	if strings.Contains(body, `"Spec"`) {
		t.Error("run 详情页的 job 不应带 spec")
	}

	// 已结束、有失败：默认"只重跑未成功的 job"，说明沿用和不使用缓存的 job 数
	run.State = store.RunFailed
	p = testRunPage(run, jv)
	p.Viewed = &p.Reruns[1]
	p.Active, p.Form = false, newRerunForm(testJobs(), false)
	d = pageData[runData](t, renderData(t, "run", p))
	if expect(t, "失败的 run", d, !d.Active, d.Form != nil) {
		f := d.Form
		expect(t, "失败的 run", f, f.Default == store.RerunFailed, f.Retry == 3, f.Kept == 3, f.Publish == 1, f.Effects == 1)
	}

	// 全部成功：默认全部重跑（没有未成功的 job）
	man := "sha256:" + strings.Repeat("a", 64)
	run.State = store.RunSucceeded
	p.Form = newRerunForm([]*cistore.Job{{ID: 1, State: store.JobSucceeded, ResultManifest: &man}}, false)
	d = pageData[runData](t, renderData(t, "run", p))
	if expect(t, "成功的 run", d, d.Form != nil, d.Purged == nil) {
		f := d.Form
		expect(t, "成功的 run", f, f.Default == store.RerunAll, f.Retry == 0, f.Kept == 1, !f.Purged)
	}

	// 日志和产物已经清理：带着清理时间；成功的 job 也要重新规划
	purgedAt := now.Add(-time.Hour)
	p.Purged = &purgedAt
	p.Form = newRerunForm([]*cistore.Job{{ID: 1, State: store.JobSucceeded, ResultManifest: &man}}, true)
	body = renderData(t, "run", p)
	d = pageData[runData](t, body)
	mustHave(t, "已清理的 run", body, `"Purged":"`+jsonTime(purgedAt)+`"`)
	if expect(t, "已清理的 run", d, d.Form != nil) {
		f := d.Form
		expect(t, "已清理的 run", f, f.Purged, f.Default == store.RerunFailed, f.Retry == 1, f.Kept == 0)
	}
	p.Purged = nil

	// 查看历史：History，Viewed 是那一次（状态为那一次的）
	p.Viewed = &p.Reruns[0]
	d = pageData[runData](t, renderData(t, "run", p))
	expect(t, "历史", d, d.History, d.Viewed != nil && d.Viewed.N == 0 && d.Viewed.State == store.RunFailed, d.Run.CurrentRerun == 1)

	// rerun 无效：Viewed 为 nil，Invalid 是地址中的原值
	p.Viewed, p.Invalid = nil, "999"
	d = pageData[runData](t, renderData(t, "run", p))
	expect(t, "无效的 rerun", d, d.Viewed == nil, d.Invalid == "999", !d.History, len(d.Reruns) == 2)
}

func testOutputs() *admin.JobOutputs {
	d := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	return &admin.JobOutputs{Manifest: d("0"), Outputs: []*admin.JobOutput{
		{Name: "bin", Digest: d("1"), FileCount: 3, Size: 3 << 20, Files: []admin.OutputFile{
			{Path: "kiln demo", Digest: d("2"), Size: 3 << 20, Exec: true},
			{Path: "docs/old.txt", Digest: d("3"), Size: 1, Missing: true},
		}},
		{Name: "link", Digest: d("4"), FileCount: 1, Files: []admin.OutputFile{{Path: "latest", Symlink: "bin/x"}}},
		{Name: "gone", Digest: d("5"), Missing: true},
		{Name: "readme", Digest: d("6"), FileCount: 1, Size: 12, Files: []admin.OutputFile{{Path: "README.txt", Digest: d("7"), Size: 12}}},
	}}
}

// jobData 是 job 详情页的页面数据（jobPage），另带计算的 Publish、NoLog、AttemptKey。
type jobData struct {
	jobPage
	Publish, NoLog bool
	AttemptKey     string
}

// testJobDetail 是 job 详情页的测试数据：成功的 build 有两次尝试（第一次基础设施错误），有产物，下游有 test。
func testJobDetail(now time.Time) *jobPage {
	run := &cistore.Run{ID: 7, Workflow: "ci.yml", SHA: "0123456789abcdef0123", State: store.RunSucceeded, CurrentRerun: 1, CreatedAt: now}
	j := &cistore.Job{ID: 21, RunID: 7, Rerun: 1, DisplayName: "build", State: store.JobSucceeded}
	attempts := []attemptView{
		{ID: 30, N: 1, Outcome: string(store.OutcomeInfraError), ErrorClass: "infra.lost", Runner: "mini"},
		{ID: 31, N: 2, Outcome: string(store.OutcomeSucceeded), Runner: "mini", Current: true},
	}
	return &jobPage{
		Job: jobView{Job: j}, Run: run, Repo: "tester/demo", Attempts: attempts, Attempt: 31, Spec: "{}", Terminal: true,
		Outputs: testOutputs(), Declared: []string{"bin"}, Producer: &attempts[1], LogEmpty: message(msgJobLogNoOutput),
		Rerun: newJobRerun(j, []*cistore.Job{j, {ID: 22, DisplayName: "test", Needs: []int64{21}}}, false),
	}
}

func TestJobDetailRender(t *testing.T) {
	p := pageData[jobData](t, renderData(t, "job", testJobDetail(time.Now())))
	// 产物：生成它的尝试、各输出的文件（cas.FileEntry 的 JSON 字段名是小写的 path、digest 等）
	if expect(t, "产物", p, p.Producer != nil && p.Producer.ID == 31 && p.Producer.N == 2, p.Outputs != nil && len(p.Outputs.Outputs) == 4,
		slices.Equal(p.Declared, []string{"bin"})) {
		o := p.Outputs.Outputs
		expect(t, "输出", o, o[0].FileCount == 3, len(o[0].Files) == 2 && o[0].Files[0].Path == "kiln demo" && o[0].Files[0].Exec && o[0].Files[1].Missing,
			len(o[1].Files) == 1 && o[1].Files[0].Symlink == "bin/x", o[2].Missing)
	}
	// 尝试、日志读取的位置、重跑的说明
	expect(t, "尝试", p, len(p.Attempts) == 2 && p.Attempts[0].ErrorClass == "infra.lost" && p.Attempts[0].Outcome == "infra_error" && p.Attempts[1].Current,
		p.Attempt == 31, !p.Explicit, p.Viewing == nil, p.AttemptKey == "current", !p.NoLog, !p.Publish, textZH(t, p.LogEmpty) == "这次尝试没有输出日志。")
	if r := p.Rerun; expect(t, "重跑", r, r != nil) {
		expect(t, "重跑", r, r.Latest, r.Force, slices.Equal(r.Downstream, []string{"test"}), r.Kept == 0)
	}
	expect(t, "job 详情页应带着 job 的 spec", p.Job, p.Spec == "{}", p.Job.ID == 21)

	// 查看历史尝试：日志固定在这次尝试（Explicit、AttemptKey）
	jp := testJobDetail(time.Now())
	jp.selectAttempt("30")
	p = pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "历史尝试", p, p.Attempt == 30, p.Explicit, p.Viewing != nil && p.Viewing.ID == 30, p.AttemptKey == "30")

	// 无效的 attempt：AttemptErr（前端提示，不加载日志）
	jp = testJobDetail(time.Now())
	jp.selectAttempt("999999999")
	p = pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "无效的 attempt", p, textZH(t, p.AttemptErr) == "这个 job 没有尝试 #999999999（可能属于别的 job，或编号写错了）。", !p.Explicit, p.AttemptKey == "current")

	// 不是最新一次运行中的 job：不能直接重跑，指向最新一次
	jp = testJobDetail(time.Now())
	jp.Rerun = &jobRerun{LatestID: 40}
	p = pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "历史 job", p.Rerun, p.Rerun != nil && !p.Rerun.Latest && p.Rerun.LatestID == 40)

	// run 还在执行：取消整个 run，没有重跑
	jp = testJobDetail(time.Now())
	jp.RunActive, jp.Rerun = true, nil
	p = pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "run 还在执行", p, p.RunActive, p.Rerun == nil, p.Run.ID == 7)
}

// TestJobDetailRender_NoLog：没有执行过的 job（命中缓存）和日志、产物已经清理的 job。
func TestJobDetailRender_NoLog(t *testing.T) {
	now := time.Now()
	// 命中缓存：没有执行过，NoLog（不加载日志），LogEmpty 说明原因
	jp := testJobDetail(now)
	jp.Job.Job = &cistore.Job{ID: 21, RunID: 7, Rerun: 1, DisplayName: "build", State: store.JobSucceeded, CachedFrom: new(int64(5))}
	jp.Producer, jp.Attempts, jp.LogEmpty = nil, nil, logEmptyText(jp.Job.Job, false, false)
	p := pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "命中缓存", p, p.NoLog, p.Producer == nil, p.Job.CachedFrom != nil && *p.Job.CachedFrom == 5, textZH(t, p.LogEmpty) == "这个 job 命中缓存，没有执行；日志见 job #5。")
	jp.Outputs = &admin.JobOutputs{Manifest: "sha256:x", Missing: true}
	p = pageData[jobData](t, renderData(t, "job", jp))
	expect(t, "清单丢失", p.Outputs, p.Outputs != nil && p.Outputs.Missing)

	// 日志和产物已经按保留策略清理：带着清理时间，重跑时有产物的 job 重新规划
	jp = testJobDetail(now)
	purgedAt := now.Add(-time.Hour)
	jp.Purged, jp.Outputs = &purgedAt, &admin.JobOutputs{Manifest: "sha256:x", Missing: true}
	jp.LogEmpty = logEmptyText(jp.Job.Job, true, true)
	jp.Rerun = &jobRerun{Latest: true, Force: true, Replan: 2}
	body := renderData(t, "job", jp)
	p = pageData[jobData](t, body)
	mustHave(t, "已清理", body, `"Purged":"`+jsonTime(purgedAt)+`"`)
	expect(t, "已清理", p, strings.Contains(textZH(t, p.LogEmpty), "日志已按保留策略清理"), p.Rerun != nil && p.Rerun.Replan == 2)
}

// ---- 连接数据库的功能测试 ----

// runJobs 返回 run 详情页的页面数据中列出的 job ID（排序后）。
func runJobs(t *testing.T, body string) []int64 {
	t.Helper()
	var out []int64
	for _, j := range pageData[runData](t, body).Jobs {
		out = append(out, j.ID)
	}
	slices.Sort(out)
	return out
}

func TestPageRun(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	// 首次运行失败，第 1 次重跑成功
	run := e.addRun(runOpts{state: store.RunSucceeded, rerun: 1})
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, 'failed'), ($1, 1, 'succeeded')`, run)
	first := e.addJob(jobOpts{runID: run, rerun: 0, state: store.JobFailed})
	latest := e.addJob(jobOpts{runID: run, rerun: 1, state: store.JobSucceeded})
	id := strconv.FormatInt(run, 10)

	// 最新一次：显示最新的 job 和重跑表单
	w := e.get(e.bob, "/runs/"+id+"?back=%2Fruns%3Fstate%3Dfailed")
	wantStatus(t, "最新一次", w, http.StatusOK)
	body := w.Body.String()
	if got := runJobs(t, body); !slices.Equal(got, []int64{latest}) {
		t.Errorf("最新一次的 job = %v，应为 [%d]", got, latest)
	}
	// 已结束：可以重跑（Form），不能取消（Active 为 false）
	if p := pageData[runData](t, body); p.Form == nil || p.Active || p.Back != "/runs?state=failed" || p.History {
		t.Errorf("最新一次：Form %+v Active %v Back %q", p.Form, p.Active, p.Back)
	}

	// 查看历史：显示那一次的 job
	w = e.get(e.bob, "/runs/"+id+"?rerun=0")
	wantStatus(t, "首次运行", w, http.StatusOK)
	if got := runJobs(t, w.Body.String()); !slices.Equal(got, []int64{first}) {
		t.Errorf("首次运行的 job = %v，应为 [%d]", got, first)
	}
	if p := pageData[runData](t, w.Body.String()); !p.History {
		t.Error("查看首次运行应是查看历史")
	}

	// rerun 无效：404，提示，不显示任何一次运行的 job
	for _, v := range []string{"2", "-1", "x"} {
		w := e.get(e.bob, "/runs/"+id+"?rerun="+v)
		wantStatus(t, "rerun="+v, w, http.StatusNotFound)
		p := pageData[runData](t, w.Body.String())
		if p.Invalid != v || p.Viewed != nil || len(p.Jobs) != 0 || len(p.Stages) != 0 {
			t.Errorf("rerun=%s：不应显示任何一次运行的 job：Invalid %q Viewed %v Jobs %v", v, p.Invalid, p.Viewed, p.Jobs)
		}
	}

	// 不安全的 back 不显示
	if body := e.get(e.bob, "/runs/"+id+"?back=%2F%2Fevil.example%2Fruns").Body.String(); strings.Contains(body, "evil.example") {
		t.Errorf("不安全的 back 不应出现在页面数据中")
	}

	// 运行中：可以取消，不能重跑
	active := e.addRun(runOpts{state: store.RunActive})
	if p := pageData[runData](t, e.get(e.bob, runURL(active)).Body.String()); !p.Active || p.Form != nil {
		t.Errorf("运行中的 run 应能取消、不能重跑：Active %v Form %+v", p.Active, p.Form)
	}

	// run 不存在
	w = e.get(e.bob, "/runs/99999")
	wantErrorPage(t, "run 不存在", w, http.StatusNotFound, []string{"没有编号为 99999 的 run"}, []string{"/runs"})
}

func TestPageJob(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{state: store.RunSucceeded})
	build := e.addJob(jobOpts{runID: run, state: store.JobSucceeded})
	first := e.addAttempt(build, mini, "infra_error")
	second := e.addAttempt(build, mini, "succeeded")
	test := e.addJob(jobOpts{runID: run, key: "test", needs: []int64{build}})
	other := e.addAttempt(test, mini, "succeeded")
	publish := e.addJob(jobOpts{runID: run, key: "publish", kind: "publish", needs: []int64{test}})
	e.exec(`INSERT INTO releases (job_id, run_id, source_repo_id, target_repo, tag, state, stage, progress)
		VALUES ($1,$2,$3,'tester/pub','v1.0.0','succeeded','done','{"html_url":"https://github.example/tester/pub/releases/v1.0.0"}')`, publish, run, testRepoID)
	jb := strconv.FormatInt(build, 10)

	w := e.get(e.bob, "/jobs/"+jb)
	wantStatus(t, "job 详情", w, http.StatusOK)
	p := pageData[jobData](t, w.Body.String())
	if expect(t, "尝试", p.Attempts, len(p.Attempts) == 2, p.Attempt == second) {
		a := p.Attempts[1]
		expect(t, "尝试", a, a.N == 2, a.Current, a.Runner == "mini")
	}
	expect(t, "重跑", p.Rerun, p.Rerun != nil && p.Rerun.Latest && slices.Contains(p.Rerun.Downstream, "test"))
	// 同一次运行中的全部 job（页面左侧的 job 列表）
	var ids []int64
	for _, j := range p.Jobs {
		ids = append(ids, j.ID)
	}
	if !slices.Equal(ids, []int64{build, test, publish}) {
		t.Errorf("同一次运行的 job = %v，应为 %v", ids, []int64{build, test, publish})
	}

	// 指定这个 job 的历史尝试
	w = e.get(e.bob, fmt.Sprintf("/jobs/%s?attempt=%d", jb, first))
	wantStatus(t, "历史尝试", w, http.StatusOK)
	p = pageData[jobData](t, w.Body.String())
	expect(t, "历史尝试", p, p.Attempt == first, p.Explicit, p.Viewing != nil, p.AttemptKey == strconv.FormatInt(first, 10))
	if w := e.get(e.bob, fmt.Sprintf("/jobs/%s?attempt=%d", jb, second)); w.Code != http.StatusOK {
		t.Errorf("当前尝试：HTTP %d", w.Code)
	}

	// attempt 无效：404 并提示，不加载日志
	for _, c := range []struct{ name, q, want string }{
		{"属于别的 job", strconv.FormatInt(other, 10), fmt.Sprintf("这个 job 没有尝试 #%d", other)},
		{"不是数字", "abc", "地址中的 attempt 参数无效"},
		{"不是正数", "0", "地址中的 attempt 参数无效"},
	} {
		w := e.get(e.bob, "/jobs/"+jb+"?attempt="+c.q)
		wantStatus(t, c.name, w, http.StatusNotFound)
		// AttemptErr 非空：前端提示，不加载日志
		if p := pageData[jobData](t, w.Body.String()); !strings.Contains(textZH(t, p.AttemptErr), c.want) || p.Explicit {
			t.Errorf("%s：AttemptErr %+v", c.name, p.AttemptErr)
		}
	}

	// 发布 job：显示发布进度；没有尝试记录
	w = e.get(e.bob, fmt.Sprintf("/jobs/%d", publish))
	wantStatus(t, "发布 job", w, http.StatusOK)
	p = pageData[jobData](t, w.Body.String())
	if expect(t, "发布 job", p, p.Publish, p.Release != nil, len(p.Attempts) == 0) {
		r := p.Release
		expect(t, "发布 job", r, r.Target == "tester/pub", r.Tag == "v1.0.0", r.Progress["html_url"] == "https://github.example/tester/pub/releases/v1.0.0")
	}
	// 有目标记录时按目标列出（release_targets）
	e.exec(`INSERT INTO release_targets (job_id, seq, kind, name, target, state, detail, error) VALUES
		($1, 0, 'upload', 'site', 'https://dl.example.com/api', 'failed', '{}', 'HTTP 401 unauthorized'),
		($1, 1, 'github', 'tester/pub', 'tester/pub', 'staged', '{"html_url":"https://github.example/draft"}', NULL)`, publish)
	w = e.get(e.bob, fmt.Sprintf("/jobs/%d", publish))
	if r := pageData[jobData](t, w.Body.String()).Release; expect(t, "发布目标", r, r != nil && len(r.Targets) == 2) {
		up, gh := r.Targets[0], r.Targets[1]
		expect(t, "发布目标", r.Targets, up.Kind == "upload", up.State == "failed", up.Error == "HTTP 401 unauthorized",
			gh.Kind == "github", gh.State == "staged", gh.Detail["html_url"] == "https://github.example/draft")
	}
	w = e.get(e.bob, fmt.Sprintf("/jobs/%d?attempt=%d", publish, first))
	wantStatus(t, "发布 job 的 attempt", w, http.StatusNotFound)
	mustHave(t, "发布 job 的 attempt", textZH(t, pageData[jobData](t, w.Body.String()).AttemptErr), "发布 job 由 Server 执行，没有尝试记录")

	// job 不存在；不属于任何 run 的内部 job 也不显示
	wantErrorPage(t, "job 不存在", e.get(e.bob, "/jobs/99999"), http.StatusNotFound, []string{"没有编号为 99999 的 job"}, []string{"/runs"})
	var internal int64
	e.must(e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, job_key, job_name, display_name, spec, platform, executor_kind, state)
		VALUES ('internal','x','x','x','{}','linux/amd64','host','pending') RETURNING id`).Scan(&internal))
	wantErrorPage(t, "内部 job", e.get(e.bob, fmt.Sprintf("/jobs/%d", internal)), http.StatusNotFound, []string{"没有编号为"}, []string{"/runs"})
}

// TestGetLog：页面脚本读取日志的接口。
func TestGetLog(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{state: store.RunActive})
	job := e.addJob(jobOpts{runID: run, state: store.JobRunning})
	first := e.addAttempt(job, mini, "infra_error")
	e.appendLog(first, "old\n")
	current := e.addAttempt(job, mini, "")
	e.appendLog(current, "hello world\n")
	other := e.addAttempt(e.addJob(jobOpts{runID: run, key: "other"}), mini, "succeeded")
	jid := strconv.FormatInt(job, 10)

	for _, c := range []struct {
		name, q                     string
		body, next, finished, state string
		attempt, current            int64
	}{
		{"当前尝试", "", "hello world\n", "12", "false", "running", current, current},
		{"从 offset 继续", "?offset=6", "world\n", "12", "false", "running", current, current},
		{"指定的历史尝试读到末尾", fmt.Sprintf("?attempt=%d&offset=4", first), "", "4", "true", "running", first, current},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.get(e.bob, "/jobs/"+jid+"/log"+c.q)
			wantStatus(t, c.name, w, http.StatusOK)
			h := w.Header()
			if w.Body.String() != c.body {
				t.Errorf("日志 = %q，应为 %q", w.Body.String(), c.body)
			}
			for k, v := range map[string]string{
				"X-Next-Offset": c.next, "X-Finished": c.finished, "X-Job-State": c.state, "X-Job-Terminal": "false",
				"X-Attempt": strconv.FormatInt(c.attempt, 10), "X-Current-Attempt": strconv.FormatInt(c.current, 10),
				"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
			} {
				if h.Get(k) != v {
					t.Errorf("%s = %q，应为 %q", k, h.Get(k), v)
				}
			}
		})
	}

	// job 结束后读到末尾：X-Finished 和 X-Job-Terminal 为 true
	e.exec(`UPDATE attempts SET outcome='succeeded' WHERE id=$1`, current)
	e.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, job)
	w := e.get(e.bob, "/jobs/"+jid+"/log?offset=12")
	if w.Header().Get("X-Finished") != "true" || w.Header().Get("X-Job-Terminal") != "true" || w.Header().Get("X-Job-State") != "succeeded" {
		t.Errorf("结束后的头部 = %v", w.Header())
	}

	// 错误：不是错误页，正文是消息的 JSON（前端按当前语言显示；按请求的参数说明找不到的是什么，不回显内部错误）
	for _, c := range []struct {
		name   string
		c      *client
		target string
		status int
		want   string
	}{
		{"未登录", nil, "/jobs/" + jid + "/log", http.StatusUnauthorized, "登录已过期，请重新登录。"},
		{"job 不存在", e.bob, "/jobs/99999/log", http.StatusNotFound, "没有 job #99999"},
		{"尝试属于别的 job", e.bob, fmt.Sprintf("/jobs/%s/log?attempt=%d", jid, other), http.StatusNotFound, fmt.Sprintf("job #%s 没有尝试 #%d", jid, other)},
		{"offset 无效", e.bob, "/jobs/" + jid + "/log?offset=x", http.StatusBadRequest, "offset 参数无效"},
		{"offset 为负", e.bob, "/jobs/" + jid + "/log?offset=-1", http.StatusBadRequest, "offset 参数无效"},
		{"attempt 无效", e.bob, "/jobs/" + jid + "/log?attempt=-2", http.StatusBadRequest, "attempt 参数无效"},
	} {
		w := e.get(c.c, c.target)
		var msg uiText
		if w.Code != c.status || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || json.Unmarshal(w.Body.Bytes(), &msg) != nil ||
			msg.id == "" || textZH(t, msg) != c.want {
			t.Errorf("%s：HTTP %d（应为 %d 的消息 %q）：%s", c.name, w.Code, c.status, c.want, w.Body.String())
		}
	}
}

func TestGetLogDownload(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{})
	job := e.addJob(jobOpts{runID: run})
	first := e.addAttempt(job, mini, "failed")
	e.appendLog(first, "one\n")
	second := e.addAttempt(job, mini, "succeeded")
	e.appendLog(second, "two two\n")
	skipped := e.addJob(jobOpts{runID: run, key: "skipped", state: store.JobSkipped})

	for _, c := range []struct {
		name, target, file, body string
	}{
		{"当前尝试", fmt.Sprintf("/jobs/%d/log/download", job), fmt.Sprintf("kiln-job-%d-attempt-%d.log", job, second), "two two\n"},
		{"指定尝试", fmt.Sprintf("/jobs/%d/log/download?attempt=%d", job, first), fmt.Sprintf("kiln-job-%d-attempt-%d.log", job, first), "one\n"},
		{"没有尝试", fmt.Sprintf("/jobs/%d/log/download", skipped), fmt.Sprintf("kiln-job-%d.log", skipped), ""},
	} {
		w := e.get(e.bob, c.target)
		wantStatus(t, c.name, w, http.StatusOK)
		h := w.Header()
		if w.Body.String() != c.body || h.Get("Content-Disposition") != `attachment; filename="`+c.file+`"` ||
			h.Get("Content-Length") != strconv.Itoa(len(c.body)) || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s：%q %v", c.name, w.Body.String(), h)
		}
	}
	// 未登录（浏览器直接打开下载地址）：跳转到登录页；不存在：错误页
	w := e.browse(nil, fmt.Sprintf("/jobs/%d/log/download", job))
	wantStatus(t, "未登录", w, http.StatusSeeOther)
	if !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Errorf("未登录应跳转到登录页：%q", w.Header().Get("Location"))
	}
	wantErrorPage(t, "job 不存在", e.get(e.bob, "/jobs/99999/log/download"), http.StatusNotFound, []string{"没有 job #99999"}, []string{"/runs"})
	wantErrorPage(t, "尝试不属于 job", e.get(e.bob, fmt.Sprintf("/jobs/%d/log/download?attempt=%d", skipped, first)), http.StatusNotFound,
		[]string{fmt.Sprintf("job #%d 没有尝试 #%d", skipped, first)}, []string{"/runs"})
}

// putBlob 把 data 写入 CAS，返回 digest。
func (e *webEnv) putBlob(data []byte) string {
	e.t.Helper()
	d := cas.Digest(data)
	if _, err := e.cas.Put(e.ctx, d, bytes.NewReader(data)); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// putJSON 把 v 的 JSON 写入 CAS，返回 digest。
func (e *webEnv) putJSON(v any) string {
	e.t.Helper()
	return e.putBlob(mustJSON(v))
}

// TestGetOutput：产物下载先校验登录和产物归属，再跳转到短期有效的 CAS 预签名地址。
func TestGetOutput(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	app := []byte("#!/bin/sh\necho app\n")
	appFile := cas.FileEntry{Path: "bin/app", Digest: e.putBlob(app), Size: int64(len(app))}
	lostFile := cas.FileEntry{Path: "lost.txt", Digest: cas.Digest([]byte("lost")), Size: 4}
	out := e.putJSON(cas.OutputManifest{Schema: cas.OutputSchema, Files: []cas.FileEntry{appFile, lostFile}})
	result := e.putJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{"bin": out}})
	run := e.addRun(runOpts{})
	job := e.addJob(jobOpts{runID: run, manifest: result})
	otherJob := e.addJob(jobOpts{runID: run, key: "other"})

	// 下载地址由浏览器直接打开
	target := fmt.Sprintf("/jobs/%d/download?output=bin&path=bin%%2Fapp", job)
	w := e.browse(e.bob, target)
	wantStatus(t, "下载", w, http.StatusSeeOther)
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/cas/"+appFile.Digest || q.Get("filename") != "app" || q.Get("op") != auth.CASOpGet ||
		!e.d.keys.CheckCASSig(auth.CASOpGet, appFile.Digest, q.Get("exp"), q.Get("sig")) {
		t.Errorf("应跳转到这个文件的预签名地址：%s", u)
	}
	if exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64); time.Until(time.Unix(exp, 0)) > outputURLTTL {
		t.Errorf("预签名地址的有效期超过 %s", outputURLTTL)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("跳转不应缓存")
	}

	// 未登录：跳转到登录页，不给出预签名地址
	w = e.browse(nil, target)
	if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("未登录：HTTP %d → %q", w.Code, loc)
	}
	// 文件不属于这个 job、输出或文件不存在：404
	for name, target := range map[string]string{
		"文件属于别的 job": fmt.Sprintf("/jobs/%d/download?output=bin&path=bin%%2Fapp", otherJob),
		"job 不存在":    "/jobs/99999/download?output=bin&path=bin%2Fapp",
		"输出不存在":      fmt.Sprintf("/jobs/%d/download?output=nope&path=bin%%2Fapp", job),
		"文件不存在":      fmt.Sprintf("/jobs/%d/download?output=bin&path=..%%2F..%%2Fetc%%2Fpasswd", job),
	} {
		wantErrorPage(t, name, e.browse(e.bob, target), http.StatusNotFound, nil, []string{"/runs"}, "/cas/")
	}
	// 文件内容已不在存储中：回到 job 页面并说明
	w = e.browse(e.bob, fmt.Sprintf("/jobs/%d/download?output=bin&path=lost.txt", job))
	if flash := flashZH(t, wantRedirect(t, "产物已丢失", w, jobURL(job))); !strings.Contains(flash, "没有下载：产物 bin/lost.txt 已不在存储中") {
		t.Errorf("提示 = %q", flash)
	}
}

func TestJobStages(t *testing.T) {
	jv := func(id int64, needs ...int64) jobView {
		return jobView{Job: &cistore.Job{ID: id, DisplayName: fmt.Sprint("j", id), Needs: needs}}
	}
	// 1、2 没有依赖；3 依赖 1；4 依赖 2、3；5 依赖不在这次运行中的 job（沿用结果的 job 不在列表中时）
	stages := jobStages([]jobView{jv(4, 2, 3), jv(3, 1), jv(1), jv(2), jv(5, 99)})
	var got [][]int64
	for _, s := range stages {
		var ids []int64
		for _, j := range s.Jobs {
			ids = append(ids, j.ID)
		}
		got = append(got, ids)
	}
	want := [][]int64{{1, 2, 5}, {3}, {4}}
	if !reflect.DeepEqual(got, want) || stages[2].N != 3 {
		t.Errorf("jobStages = %v，应为 %v", got, want)
	}
	if len(jobStages(nil)) != 0 {
		t.Error("没有 job 时不应有阶段")
	}
}

// AI 审计发布的 PR review 的链接与页面上其他指向 GitHub 的链接规则相同：只在使用 github.com 时生成
// （原来固定指向 github.com，GitHub API 是其他地址时链接是错的）。
func TestPRReviewURL(t *testing.T) {
	t.Parallel()
	d, _, _ := testDashboard(t, "")
	if got := prReviewURL(d.githubWeb(), "acme/kiln", 74, 3123456789); got != "https://github.com/acme/kiln/pull/74#pullrequestreview-3123456789" {
		t.Errorf("github.com：%q", got)
	}
	d.cfg.GitHubAPIURL = "https://ghe.example.com/api/v3"
	if got := prReviewURL(d.githubWeb(), "acme/kiln", 74, 1); got != "" {
		t.Errorf("其他 GitHub API 地址时不应生成链接：%q", got)
	}
}

func TestOverviewStatsRate(t *testing.T) {
	for _, c := range []struct {
		s    overviewStats
		want string
	}{
		{overviewStats{}, ""},
		{overviewStats{Active: 3}, ""},
		{overviewStats{Succeeded: 2, Failed: 1, Cancelled: 1}, "50%"},
		{overviewStats{Succeeded: 2}, "100%"},
	} {
		if got := c.s.Rate(); got != c.want {
			t.Errorf("%+v.Rate() = %q，应为 %q", c.s, got, c.want)
		}
	}
}

// 发布 job 页面按目标列出各自的状态与结果（release_targets）：GitHub Release 的地址、HTTP 上传的下载地址、失败原因。
func TestJobPageReleaseTargets(t *testing.T) {
	now := time.Now()
	run := &cistore.Run{ID: 7, Workflow: "release.yml", Event: "push", Ref: "refs/tags/v1.2.0", SHA: "0123456789abcdef0123", State: store.RunFailed, CreatedAt: now}
	job := &cistore.Job{ID: 11, Kind: workflow.JobKindPublish, RunID: 7, DisplayName: "publish", Platform: "server", State: store.JobFailed, StartedAt: &now}
	p := testJobPage(run, jobView{Job: job})
	p.Release = &releaseView{
		Target: "tester/demo, site", Tag: "v1.2.0", State: "failed", Stage: "commit", Error: "发布失败",
		Progress: map[string]any{"html_url": "https://example.com/old"},
		Targets: []releaseTargetView{
			{Kind: "upload", Name: "site", Target: "https://dl.example.com/api/releases", State: "committed", Detail: map[string]any{
				"latest": true, "assets": []any{map[string]any{"name": "app.dmg", "url": "https://dl.example.com/download/v1.2.0/app.dmg"}},
			}},
			{Kind: "github", Name: "tester/demo", Target: "tester/demo", State: "failed", Error: "发布 Release：HTTP 500", Detail: map[string]any{}},
			{Kind: "image", Name: "ghcr.io/t/app", Target: "ghcr.io/t/app", State: "staged", Detail: map[string]any{}},
			{Kind: "npm", Name: "@t/app", Target: "@t/app", State: "committed", Detail: map[string]any{
				"package": "@t/app", "version": "1.2.0", "dist_tag": "latest", "access": "public", "previous": true,
				"tarball": "https://registry.npmjs.org/@t/app/-/app-1.2.0.tgz",
			}},
		},
	}
	body := renderJSON(t, "job", &session{Login: "alice", Role: roleAdmin}, p)
	r := pageData[jobData](t, body).Release
	if r == nil || len(r.Targets) != 4 {
		t.Fatalf("发布目标：%+v", r)
	}
	// npm 包：发布到 npm 官方 registry 时带上 npmjs.com 的版本页（页面数据中的 URL），其他目标没有
	urls := pageData[struct {
		Release struct{ Targets []struct{ URL string } }
	}](t, body)
	if got := urls.Release.Targets; got[3].URL != "https://www.npmjs.com/package/@t/app/v/1.2.0" || got[0].URL != "" || got[1].URL != "" {
		t.Errorf("发布目标的 URL：%+v", got)
	}
	if n := r.Targets[3]; n.Kind != "npm" || n.Detail["previous"] != true || n.Detail["dist_tag"] != "latest" {
		t.Errorf("npm 包：%+v", n)
	}
	// 各目标的种类、状态、结果（HTTP 上传的 latest 和 assets 的下载地址）和失败原因
	up, gh, img := r.Targets[0], r.Targets[1], r.Targets[2]
	var asset map[string]any
	if assets, _ := up.Detail["assets"].([]any); len(assets) == 1 {
		asset, _ = assets[0].(map[string]any)
	}
	if up.Kind != "upload" || up.Target != "https://dl.example.com/api/releases" || up.State != "committed" || up.Detail["latest"] != true ||
		asset["name"] != "app.dmg" || asset["url"] != "https://dl.example.com/download/v1.2.0/app.dmg" {
		t.Errorf("HTTP 上传：%+v", up)
	}
	if gh.Kind != "github" || gh.State != "failed" || gh.Error != "发布 Release：HTTP 500" || img.Kind != "image" || img.State != "staged" {
		t.Errorf("GitHub Release、容器镜像：%+v %+v", gh, img)
	}
	if r.State != "failed" || r.Error != "发布失败" || r.Tag != "v1.2.0" {
		t.Errorf("发布：%+v", r)
	}
}

// 进行中的 job 带上 Runner 上报的进度（旧版 Runner 没有上报时为空）；等待中的 job 列出还没完成的上游。
func TestPageRunProgress(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{state: store.RunActive})
	lint := e.addJob(jobOpts{runID: run, key: "lint", state: store.JobSucceeded})
	dmg := e.addJob(jobOpts{runID: run, key: "dmg", state: store.JobRunning})
	e.exec(`UPDATE attempts SET progress=$2 WHERE id=$1`, e.addAttempt(dmg, mini, ""),
		`{"Phase":"upload","Detail":"上传产物到 Kiln Server","FilesTotal":1,"BytesTotal":135682737,"BytesSent":67841368}`)
	old := e.addJob(jobOpts{runID: run, key: "old", state: store.JobRunning})
	e.addAttempt(old, mini, "")
	publish := e.addJob(jobOpts{runID: run, key: "publish", kind: "publish", state: store.JobWaiting, needs: []int64{lint, dmg, old}})

	p := pageData[runData](t, e.get(e.bob, runURL(run)).Body.String())
	byID := map[int64]jobView{}
	for _, j := range p.Jobs {
		byID[j.ID] = j
	}
	if pr := byID[dmg].Progress; pr == nil || pr.Phase != store.PhaseUpload || pr.BytesSent != 67841368 || pr.BytesTotal != 135682737 {
		t.Errorf("dmg 的进度 = %+v", pr)
	}
	if pr := byID[old].Progress; pr != nil {
		t.Errorf("没有上报进度的 job 应为空：%+v", pr)
	}
	if got := byID[publish].WaitingOn; !slices.Equal(got, []string{"dmg", "old"}) {
		t.Errorf("publish 等待的上游 = %v，应为 [dmg old]", got)
	}

	// job 详情页同样带上进度和等待的上游
	jp := pageData[jobData](t, e.get(e.bob, fmt.Sprintf("/jobs/%d", dmg)).Body.String())
	if jp.Job.Progress == nil || jp.Job.Progress.Detail != "上传产物到 Kiln Server" {
		t.Errorf("job 页的进度 = %+v", jp.Job.Progress)
	}
	jp = pageData[jobData](t, e.get(e.bob, fmt.Sprintf("/jobs/%d", publish)).Body.String())
	if !slices.Equal(jp.Job.WaitingOn, []string{"dmg", "old"}) {
		t.Errorf("job 页等待的上游 = %v", jp.Job.WaitingOn)
	}
}

// 耗时按正在查看的这次运行计算：历史运行已经结束，耗时不会增长；隔天重跑的耗时不含中间的空闲时间；
// 运行列表显示最新一次运行的耗时；早期没有记录的历史运行不显示耗时。
func TestRunDuration(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	run := e.addRun(runOpts{state: store.RunActive, rerun: 1})
	// 首次运行前天创建、运行 60 秒后失败；昨天重跑，正在执行
	e.exec(`UPDATE runs SET created_at=now()-interval '2 days', finished_at=NULL WHERE id=$1`, run)
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state, created_at, finished_at) VALUES
		($1, 0, 'failed', now()-interval '2 days', now()-interval '2 days'+interval '60 seconds'),
		($1, 1, 'active', now()-interval '1 day', NULL)`, run)
	var first, rerun time.Time
	if err := e.db.QueryRow(e.ctx, `SELECT (SELECT created_at FROM run_reruns WHERE run_id=$1 AND rerun=0),
		(SELECT created_at FROM run_reruns WHERE run_id=$1 AND rerun=1)`, run).Scan(&first, &rerun); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(run, 10)

	p := pageData[runData](t, e.get(e.bob, "/runs/"+id+"?rerun=0").Body.String())
	if !p.History || p.Start != first.Unix() || p.End-p.Start != 60 {
		t.Errorf("历史运行：History %v Start %d End %d，期望从 %d 起 60 秒", p.History, p.Start, p.End, first.Unix())
	}
	p = pageData[runData](t, e.get(e.bob, "/runs/"+id).Body.String())
	if p.History || p.Start != rerun.Unix() || p.End != 0 {
		t.Errorf("最新一次：Start %d End %d，期望从重跑时 %d 起、还没结束", p.Start, p.End, rerun.Unix())
	}

	list := pageData[runsData](t, e.get(e.bob, "/runs").Body.String())
	found := false
	for _, r := range list.Runs {
		if r.ID == run {
			found = true
			if r.Start != rerun.Unix() || r.End != 0 {
				t.Errorf("运行列表：Start %d End %d，期望从重跑时 %d 起", r.Start, r.End, rerun.Unix())
			}
		}
	}
	if !found {
		t.Fatalf("运行列表中没有 run %d", run)
	}

	// 早期数据：历史运行没有 run_reruns 记录时不显示耗时，最新一次用 run 的时间
	old := e.addRun(runOpts{state: store.RunSucceeded, rerun: 1})
	e.exec(`UPDATE runs SET finished_at=now() WHERE id=$1`, old)
	oid := strconv.FormatInt(old, 10)
	if p := pageData[runData](t, e.get(e.bob, "/runs/"+oid+"?rerun=0").Body.String()); p.Start != 0 {
		t.Errorf("没有记录的历史运行不应显示耗时：Start %d End %d", p.Start, p.End)
	}
	if p := pageData[runData](t, e.get(e.bob, "/runs/"+oid).Body.String()); p.Start == 0 || p.End == 0 {
		t.Errorf("没有记录的最新一次应使用 run 的时间：Start %d End %d", p.Start, p.End)
	}
}

// TestPageJobManagedAttempt：托管池的 attempt 没有 Runner，页面上显示"托管池 <Key>"（attempt 列表和 job 列表），
// 不显示为已删除的 Runner（PLAN #32.9）。
func TestPageJobManagedAttempt(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	run := e.addRun(runOpts{state: store.RunActive})
	job := e.addJob(jobOpts{runID: run, state: store.JobRunning})
	var aid int64
	e.must(e.db.QueryRow(e.ctx, `INSERT INTO attempts (job_id, epoch, managed_pool, phase, acquire_request_id) VALUES ($1, 1, 'gcp-a', 'reserved', '')
		RETURNING id`, job).Scan(&aid))
	e.exec(`UPDATE jobs SET current_attempt_id=$2, epoch=1, attempt_count=1 WHERE id=$1`, job, aid)
	p := pageData[jobData](t, e.get(e.bob, "/jobs/"+strconv.FormatInt(job, 10)).Body.String())
	if expect(t, "尝试", p.Attempts, len(p.Attempts) == 1) {
		expect(t, "托管的尝试", p.Attempts[0], p.Attempts[0].Runner == "托管池 gcp-a")
	}
	if expect(t, "job 列表", p.Jobs, len(p.Jobs) == 1) {
		expect(t, "托管的 job", p.Jobs[0], p.Jobs[0].Runner == "托管池 gcp-a")
	}
}

// TestPageJobQueuePosition：排队中、调度器没有记下跳过原因的 job，页面数据给出前面还有几个待领取的 job 和排在最前面的 run
// （调度解释，PLAN #14.2）；有跳过原因时只显示原因，不在排队的 job 没有位置。
func TestPageJobQueuePosition(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	earlier := e.addRun(runOpts{state: store.RunActive})
	later := e.addRun(runOpts{state: store.RunActive})
	e.addJob(jobOpts{runID: earlier, state: store.JobPending, key: "test"})
	job := e.addJob(jobOpts{runID: later, state: store.JobPending})
	// 先提交的 run 的 job 排在前面（即使它的 queued_at 更晚）
	e.exec(`UPDATE jobs SET queued_at=CASE WHEN run_id=$1 THEN now() ELSE now() - interval '1 minute' END,
		created_at=CASE WHEN run_id=$1 THEN now() - interval '1 hour' ELSE now() END WHERE run_id IN ($1, $2)`, earlier, later)
	url := "/jobs/" + strconv.FormatInt(job, 10)
	p := pageData[jobData](t, e.get(e.bob, url).Body.String())
	expect(t, "排队位置", p.Queue, p.Queue != nil && p.Queue.Ahead == 1 && p.Queue.FirstRunID == earlier)

	e.exec(`UPDATE jobs SET last_skip_reason='仓库并发上限 1' WHERE id=$1`, job)
	p = pageData[jobData](t, e.get(e.bob, url).Body.String())
	expect(t, "有跳过原因时没有排队位置", p.Queue, p.Queue == nil)

	e.exec(`UPDATE jobs SET state='running', last_skip_reason=NULL WHERE id=$1`, job)
	p = pageData[jobData](t, e.get(e.bob, url).Body.String())
	expect(t, "不在排队时没有排队位置", p.Queue, p.Queue == nil)
}

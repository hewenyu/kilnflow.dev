package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ---- 页面数据（TestPagesRender） ----

func testAIRequestView(now time.Time) aiRequestView {
	return aiRequestView{
		ID: 12, Type: agentcfg.TaskReview, Repo: testRepo, Event: "pull_request", Ref: "refs/pull/5/head", SHA: "abc",
		Trigger: "pull_request", Sender: "alice", State: string(ai.RequestFailed), PRNumber: 5, RerunOf: 11, Created: now, Finished: &now,
	}
}

func testAITaskView(now time.Time) aiTaskView {
	return aiTaskView{
		ID: 30, RequestID: 12, Key: "review-macos-arm64", DisplayName: "codex · macos-arm64", Platforms: []string{"darwin/arm64"}, Platform: "darwin/arm64", Agent: "codex",
		State: string(ai.TaskFailed), Reason: "AI 审计发现 1 个问题", AttemptCount: 1, Runner: "mini", Created: now, Started: &now, Finished: &now,
	}
}

func testAIRequestsPage(now time.Time) *aiRequestsPage {
	return &aiRequestsPage{
		Requests: []aiRequestView{testAIRequestView(now)}, Filter: aiRequestFilter{Repo: testRepo, PR: 5}, Repos: []string{testRepo},
		Types: aiTypeOptions(), States: aiRequestStateOptions(), Limit: aiRequestsLimit,
	}
}

func testAIRequestPage(now time.Time) *aiRequestPage {
	return &aiRequestPage{
		Request: testAIRequestView(now), Tasks: []aiTaskView{testAITaskView(now)}, Terminal: true, CanRerun: true, HasFailed: true,
		Family: []aiFamilyItem{{ID: 11, State: "failed", Created: now}, {ID: 12, State: "failed", Created: now}}, PRURL: "https://github.com/tester/demo/pull/5",
	}
}

func testAITaskPage(now time.Time) *aiTaskPage {
	a := aiAttemptView{ID: 40, Epoch: 1, N: 1, Runner: "mini", Platform: "darwin/arm64", Outcome: "failed", ErrorClass: "ai.findings", Started: now.Unix(), Finished: now.Unix(), Current: true}
	return &aiTaskPage{
		Task: testAITaskView(now), Request: testAIRequestView(now), Tasks: []aiTaskView{testAITaskView(now)}, Attempts: []aiAttemptView{a},
		Attempt: 40, Terminal: true, RequestTerminal: true, CanRerun: true, LogEmpty: message(msgAITaskLogNoOutput), AttemptKey: "current", Spec: "{}",
		Review: &aiReviewView{
			Agent: "codex", Verdict: aireport.VerdictIssuesFound, PRNumber: 5, HeadSHA: "abc", BaseSHA: "def", ReviewState: "published", Created: now,
			Report: &aireport.Report{Verdict: aireport.VerdictIssuesFound, Summary: "有问题", Findings: []aireport.Finding{{Title: "空指针", Body: "x"}}},
		},
	}
}

func testAIRunner(now time.Time) *aiRunnerView {
	return &aiRunnerView{
		ID: 3, Name: "reviewer", Role: store.RunnerRoleReview, Version: "v0.3.0", Platform: "darwin/arm64",
		Protocol: agentcfg.AIProtocol, HostID: "host-1", Online: true, LastSeen: &now, Capacity: 2, OrcaState: "ready",
		OrcaVersion: "1.4.245", OrcaRuntimeID: "runtime-1", DockerState: "unavailable", DockerReason: "没有 Docker",
		Agents: []aiAgentView{{Agent: "codex", Name: "Codex", Version: "0.50.0", State: "available", CheckedAt: &now}},
		Tasks: []aiRunningTask{{
			RunnerID: 3, TaskID: 30, RequestID: 12, Type: agentcfg.TaskReview, Name: "codex · macos-arm64", Repo: testRepo, Started: now,
			Orca:  ai.OrcaBinding{RuntimeID: "runtime-1", RunID: "run-1", DispatchID: "dispatch-1", WorktreeName: "kiln-a30-e1"},
			Stage: "running", Text: "审计中", TaskURL: "https://kiln.example/ai/tasks/30",
		}},
	}
}

func testAIRunnersPage(now time.Time) *aiRunnersPage {
	return &aiRunnersPage{
		Runners: []*aiRunnerView{testAIRunner(now)}, Counts: aiRunnerCounts{Total: 1, Online: 1, Accepting: 1, Tasks: 1}, Self: aiRunnersPath,
		OfflineAfter: 120, LeaseTTL: 60, LongPoll: 25,
	}
}

// ---- 测试数据：AI 请求、任务、attempt、AI 执行节点 ----

// aiRequestOpts 是 addAIRequest 的选项。
type aiRequestOpts struct {
	repoID int64
	typ    string // 默认 review
	state  ai.RequestState
	// pr 不为 0 时是 PR 审计；devMode 不为空时写开发请求的明细。
	pr      int
	devMode string
	// family 不为 0 时是这个请求族的重跑（rerun_of 指向 family）。
	family int64
}

// addAIRequest 直接写入一个 AI 请求，返回 ID。
func (e *webEnv) addAIRequest(o aiRequestOpts) int64 {
	e.t.Helper()
	if o.repoID == 0 {
		o.repoID = testRepoID
	}
	if o.typ == "" {
		o.typ = agentcfg.TaskReview
	}
	if o.state == "" {
		o.state = ai.RequestActive
	}
	event := "workflow_dispatch"
	if o.pr != 0 {
		event = "pull_request"
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, family_id, rerun_of, type, repo_id, event, ref, sha, trigger, sender, state, finished_at)
		SELECT n.id, coalesce(nullif($5::bigint, 0), n.id), nullif($5::bigint, 0), $1, $2, $3, 'refs/heads/main', 'abc123', $3, 'alice', $4,
		  CASE WHEN $4 IN ('queued', 'active') THEN NULL ELSE now() END
		FROM n RETURNING id`, o.typ, o.repoID, event, string(o.state), o.family).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	if o.typ == agentcfg.TaskReview {
		var pr *int
		if o.pr != 0 {
			pr = &o.pr
		}
		e.exec(`INSERT INTO ai_review_requests (request_id, pr_number, head_ref, base_ref) VALUES ($1,$2,'feature','main')`, id, pr)
	}
	if o.devMode != "" {
		e.exec(`INSERT INTO ai_dev_requests (request_id, mode) VALUES ($1,$2)`, id, o.devMode)
	}
	return id
}

// aiTaskOpts 是 addAITask 的选项。
type aiTaskOpts struct {
	key   string // 默认 macos-arm64
	state ai.TaskState
	spec  string
	// platforms 是可以执行的平台（默认 darwin/arm64）；attempt 记下的平台是最后一个。
	platforms []string
}

// addAITask 直接写入请求的一个任务，返回 ID。
func (e *webEnv) addAITask(requestID int64, o aiTaskOpts) int64 {
	e.t.Helper()
	if o.key == "" {
		o.key = "macos-arm64"
	}
	if o.state == "" {
		o.state = ai.TaskPending
	}
	if o.spec == "" {
		o.spec = `{"agent":"codex"}`
	}
	if o.platforms == nil {
		o.platforms = []string{"darwin/arm64"}
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes,
		  queued_at, finished_at)
		SELECT $1, $2, q.type, $5, 'codex', 'codex · '||$2, $4,
		  $3, 60, now(), CASE WHEN $3 IN ('waiting', 'pending', 'running') THEN NULL ELSE now() END
		FROM ai_requests q WHERE q.id=$1 RETURNING id`, requestID, o.key, string(o.state), o.spec, o.platforms).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// addAIAttempt 为任务写入一个 attempt（成为任务的当前 attempt），返回 ID。runnerID 为 0 时只记下 Runner 的名称 runnerName；
// outcome 为空表示还在执行。
func (e *webEnv) addAIAttempt(taskID, runnerID int64, runnerName string, outcome store.Outcome) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, runner_name, platform, acquire_request_id, outcome, finished_at,
		  runner_version, agent_version)
		SELECT $1, t.epoch+1, nullif($2::bigint, 0), nullif($3, ''), t.platforms[cardinality(t.platforms)], gen_random_uuid()::text, nullif($4, ''),
		  CASE WHEN $4 = '' THEN NULL ELSE now() END, 'v0.3.0', '1.2.3'
		FROM ai_tasks t WHERE t.id=$1 RETURNING id`, taskID, runnerID, runnerName, string(outcome)).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE ai_tasks SET current_attempt_id=$2, epoch=epoch+1, attempt_count=attempt_count+1 WHERE id=$1`, taskID, id)
	return id
}

// addAIRunner 直接写入一台 AI 执行节点（角色 role），返回 ID。
func (e *webEnv) addAIRunner(name, role string, lastSeen *time.Time) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO runners (name, kind, public_key, version, state, last_seen_at)
		VALUES ($1,'ai','\x00','v1.0.0','active',$2) RETURNING id`, name, lastSeen).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO ai_runners (runner_id, role, platform, protocol, host_id, capacity, orca_state, orca_reason, docker_state)
		VALUES ($1,$2,'darwin/arm64',$3,'host-1',2,'unavailable','Orca Server 没有启动','ready')`, id, role, agentcfg.AIProtocol)
	e.exec(`INSERT INTO ai_runner_agents (runner_id, agent, installed, version, state, checked_at) VALUES ($1,'codex',true,'0.50.0','available',now())`, id)
	return id
}

// appendAILog 把 data 追加到 AI attempt 的日志末尾。
func (e *webEnv) appendAILog(attempt int64, data string) {
	e.t.Helper()
	size, err := e.aiLogs.Size(attempt)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.aiLogs.Append(attempt, size, []byte(data)); err != nil {
		e.t.Fatal(err)
	}
}

func aiRequestIDs(list []aiRequestView) []int64 {
	out := make([]int64, 0, len(list))
	for _, v := range list {
		out = append(out, v.ID)
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AI 请求列表：只列出 AI 的请求（不含 CI 的 run），按 ID 倒序；按类型、PR、状态筛选，无效的条件视为不筛选；导航高亮 AI 请求。
func TestPageAIRequests(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRun(runOpts{})
	pr7 := e.addAIRequest(aiRequestOpts{pr: 7, state: ai.RequestFailed})
	manual := e.addAIRequest(aiRequestOpts{state: ai.RequestSucceeded})
	dev := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select"})

	w := e.get(e.bob, aiRequestsPath)
	wantStatus(t, "AI 请求列表", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "ai_requests" || pg.Nav != "ai_requests" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[aiRequestsPage](t, w.Body.String())
	if got := aiRequestIDs(p.Requests); !sameIDs(got, []int64{dev, manual, pr7}) {
		t.Errorf("列表 = %v", got)
	}
	if v := p.Requests[2]; v.PRNumber != 7 || v.Repo != testRepo || v.Type != agentcfg.TaskReview || v.State != string(ai.RequestFailed) {
		t.Errorf("PR 审计 = %+v", v)
	}
	if v := p.Requests[0]; v.Type != agentcfg.TaskDevelop || v.DevMode != "select" {
		t.Errorf("开发请求 = %+v", v)
	}
	wantPageZH(t, w.Body.String(), "ai_requests", "ai_requests", "AI 请求")
	// 筛选项的名称是消息（前端按语言显示），取值与 ai.RequestState 一致
	if got := textsZH(t, []uiText{p.Types[0].Label, p.Types[1].Label}); got[0] != "AI 审计" || got[1] != "AI 开发" {
		t.Errorf("类型选项 = %v", got)
	}
	if len(p.States) != 6 || p.States[1].Value != string(ai.RequestActive) || textZH(t, p.States[1].Label) != "进行中" {
		t.Errorf("状态选项 = %+v", p.States)
	}
	if len(p.Repos) != 1 || p.Repos[0] != testRepo {
		t.Errorf("仓库选项 = %v", p.Repos)
	}

	for target, want := range map[string][]int64{
		"?type=develop":            {dev},
		"?pr=7":                    {pr7},
		"?state=succeeded":         {manual},
		"?repo=tester%2Fdemo&pr=7": {pr7},
		"?type=bogus&state=nope":   {dev, manual, pr7},
		"?repo=tester%2Fghost":     {dev, manual, pr7},
	} {
		p := pageData[aiRequestsPage](t, e.get(e.bob, aiRequestsPath+target).Body.String())
		if got := aiRequestIDs(p.Requests); !sameIDs(got, want) {
			t.Errorf("%s：列表 = %v，应为 %v", target, got, want)
		}
	}
	if p := pageData[aiRequestsPage](t, e.get(e.bob, aiRequestsPath+"?type=bogus").Body.String()); p.Filter.Type != "" {
		t.Errorf("无效的类型应视为不筛选：%+v", p.Filter)
	}
}

// AI 请求详情：任务、Runner、请求族；已结束的审计可以重跑，开发请求不能；不存在的请求 404。
func TestPageAIRequest(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	first := e.addAIRequest(aiRequestOpts{pr: 5, state: ai.RequestFailed})
	id := e.addAIRequest(aiRequestOpts{pr: 5, state: ai.RequestFailed, family: first})
	ok := e.addAITask(id, aiTaskOpts{state: ai.TaskSucceeded})
	bad := e.addAITask(id, aiTaskOpts{key: "any", state: ai.TaskFailed, platforms: []string{"darwin/arm64", "linux/amd64"}})
	e.addAIAttempt(bad, e.addAIRunner("mini", store.RunnerRoleReview, nil), "", store.OutcomeFailed)

	w := e.get(e.bob, aiRequestURL(id))
	wantStatus(t, "AI 请求详情", w, http.StatusOK)
	wantPageZH(t, w.Body.String(), "ai_request", "ai_requests", "AI 审计请求 #"+strconv.FormatInt(id, 10))
	p := pageData[aiRequestPage](t, w.Body.String())
	if p.Request.ID != id || p.Request.RerunOf != first || !p.Terminal || !p.CanRerun || !p.HasFailed || len(p.Tasks) != 2 {
		t.Fatalf("详情 = %+v", p)
	}
	if p.Tasks[0].ID != ok || p.Tasks[1].ID != bad || p.Tasks[1].Runner != "mini" || p.Tasks[1].State != string(ai.TaskFailed) {
		t.Errorf("任务 = %+v", p.Tasks)
	}
	// 一组平台的任务：可以执行的平台和实际执行的平台（attempt 记下的）；还没有领取的任务没有执行的平台
	if g := p.Tasks[1]; !slices.Equal(g.Platforms, []string{"darwin/arm64", "linux/amd64"}) || g.Platform != "linux/amd64" {
		t.Errorf("一组平台的任务 = %v、%q", g.Platforms, g.Platform)
	}
	if o := p.Tasks[0]; !slices.Equal(o.Platforms, []string{"darwin/arm64"}) || o.Platform != "" {
		t.Errorf("没有领取的任务 = %v、%q", o.Platforms, o.Platform)
	}
	if len(p.Family) != 2 || p.Family[0].ID != first || p.Family[1].ID != id {
		t.Errorf("请求族 = %+v", p.Family)
	}

	dev := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "fix", state: ai.RequestFailed})
	if p := pageData[aiRequestPage](t, e.get(e.bob, aiRequestURL(dev)).Body.String()); p.CanRerun || p.Family != nil {
		t.Errorf("开发请求不能重跑：%+v", p)
	}
	active := e.addAIRequest(aiRequestOpts{pr: 6})
	if p := pageData[aiRequestPage](t, e.get(e.bob, aiRequestURL(active)).Body.String()); p.CanRerun || p.Terminal {
		t.Errorf("进行中的请求不能重跑：%+v", p)
	}
	wantStatus(t, "不存在的请求", e.get(e.bob, aiRequestURL(id+100)), http.StatusNotFound)
}

// AI 任务详情：attempt、日志的 attempt 选择、审计报告；地址中无效的 attempt 返回 404；不存在的任务 404。
func TestPageAITask(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	req := e.addAIRequest(aiRequestOpts{pr: 5, state: ai.RequestFailed})
	task := e.addAITask(req, aiTaskOpts{state: ai.TaskFailed})
	first := e.addAIAttempt(task, 0, "old", store.OutcomeFailed)
	second := e.addAIAttempt(task, 0, "mini", store.OutcomeFailed)
	// v1 的报告（问题只有 body）
	rep := &aireport.Report{
		Verdict: aireport.VerdictIssuesFound, Summary: "有问题",
		Findings: []aireport.Finding{{Title: "空指针", Severity: "high", Evidence: "code_evidence", Path: "a.go", Line: 3, Body: "a.go:3 解引用了 nil"}},
	}
	e.exec(`INSERT INTO ai_reviews (task_id, repo_id, attempt_id, pr_number, head_sha, base_sha, agent, verdict, summary, findings, report,
		review_state, github_review_id) VALUES ($1,$2,$3,5,'abc','def','codex','issues_found','有问题',1,$4,'published',77)`,
		task, testRepoID, second, rep.Marshal())

	w := e.get(e.bob, aiTaskURL(task))
	wantStatus(t, "AI 任务详情", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "ai_task" || pg.Nav != "ai_requests" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[aiTaskPage](t, w.Body.String())
	if p.Task.ID != task || p.Request.ID != req || p.Attempt != second || p.Explicit || len(p.Attempts) != 2 || !p.CanRerun || p.NoLog {
		t.Fatalf("详情 = %+v", p)
	}
	if a := p.Attempts[0]; a.N != 1 || a.Runner != "old" || a.Current || a.RunnerVersion != "v0.3.0" || a.AgentVersion != "1.2.3" {
		t.Errorf("第一次 attempt = %+v", a)
	}
	if p.Review == nil || p.Review.Verdict != aireport.VerdictIssuesFound || len(p.Review.Report.Findings) != 1 || p.Review.PRNumber != 5 ||
		p.Review.ReviewURL != "https://github.com/tester/demo/pull/5#pullrequestreview-77" || p.Develop != nil {
		t.Errorf("审计结果 = %+v", p.Review)
	}
	if !strings.Contains(p.Spec, `"agent": "codex"`) {
		t.Errorf("Spec = %q", p.Spec)
	}

	p = pageData[aiTaskPage](t, e.get(e.bob, aiTaskURL(task)+"?attempt="+strconv.FormatInt(first, 10)).Body.String())
	if p.Attempt != first || !p.Explicit || p.Viewing == nil || p.Viewing.ID != first || p.AttemptKey != strconv.FormatInt(first, 10) {
		t.Errorf("指定 attempt = %+v", p)
	}
	// 跳过的任务没有日志
	skipped := e.addAITask(req, aiTaskOpts{key: "linux-amd64", state: ai.TaskSkipped})
	if p := pageData[aiTaskPage](t, e.get(e.bob, aiTaskURL(skipped)).Body.String()); !p.NoLog || textZH(t, p.LogEmpty) != "这个任务被跳过，没有执行。" || p.Review != nil {
		t.Errorf("跳过的任务 = %+v", p)
	}
}

// AI 任务详情页的错误：别的任务的 attempt、无效的 attempt 是页面中的说明（404），不存在的任务、下载日志的参数无效是错误页；
// 说明都是消息。
func TestPageAITaskErrors(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	req := e.addAIRequest(aiRequestOpts{pr: 5, state: ai.RequestFailed})
	task := e.addAITask(req, aiTaskOpts{state: ai.TaskFailed})
	e.addAIAttempt(task, 0, "mini", store.OutcomeFailed)

	w := e.get(e.bob, aiTaskURL(task)+"?attempt=999999")
	wantStatus(t, "别的 attempt", w, http.StatusNotFound)
	if got := textZH(t, pageData[aiTaskPage](t, w.Body.String()).AttemptErr); got != "这个任务没有 attempt #999999（可能属于别的任务，或编号写错了）。" {
		t.Errorf("别的 attempt 的说明 = %q", got)
	}
	w = e.get(e.bob, aiTaskURL(task)+"?attempt=x")
	wantStatus(t, "无效的 attempt", w, http.StatusNotFound)
	if got := textZH(t, pageData[aiTaskPage](t, w.Body.String()).AttemptErr); got != `地址中的 attempt 参数无效（"x"），应为这个任务的某次 attempt 的编号。` {
		t.Errorf("无效的 attempt 的说明 = %q", got)
	}
	w = e.get(e.bob, aiTaskURL(task+1000))
	wantStatus(t, "不存在的任务", w, http.StatusNotFound)
	if got := pageTextZH(t, w.Body.String()); !strings.Contains(got, "没有编号为 "+strconv.FormatInt(task+1000, 10)+" 的 AI 任务") {
		t.Errorf("不存在的任务的说明 = %q", got)
	}
	w = e.get(e.bob, aiTaskURL(task)+"/log/download?attempt=x")
	wantStatus(t, "下载日志的 attempt 无效", w, http.StatusBadRequest)
	if got := pageTextZH(t, w.Body.String()); !strings.Contains(got, `attempt 参数无效（"x"）`) {
		t.Errorf("下载日志的 attempt 无效的说明 = %q", got)
	}
}

// AI 开发任务详情页显示这个任务保存的报告：没有领取的选题不属于任何工作项，属于工作项的带工作项的链接。
func TestPageAITaskDevelopReports(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	req := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestSucceeded})
	dry := e.addAITask(req, aiTaskOpts{state: ai.TaskSucceeded})
	att := e.addAIAttempt(dry, 0, "dev1", store.OutcomeSucceeded)
	sel := devreport.Selection{Version: devreport.Version, Selected: []int{3}, Plan: "dry_run 的计划", Skipped: []devreport.Skip{}}
	e.exec(`INSERT INTO dev_reports (task_id, attempt_id, kind, work_item_id, report) VALUES ($1,$2,'selection',NULL,$3)`,
		dry, att, string(devreport.Marshal(&sel)))
	p := pageData[aiTaskPage](t, e.get(e.bob, aiTaskURL(dry)).Body.String())
	if p.Develop == nil || len(p.Develop.Reports) != 1 || p.Develop.Reports[0].Selection == nil ||
		p.Develop.Reports[0].Selection.Plan != "dry_run 的计划" || p.Develop.WorkItemURL != "" || p.Develop.Reports[0].TaskURL != aiTaskURL(dry) || p.CanRerun {
		t.Fatalf("dry_run 选题的任务页 = %+v", p.Develop)
	}

	id, task, _ := e.seedAIDevItem()
	p = pageData[aiTaskPage](t, e.get(e.bob, aiTaskURL(task)).Body.String())
	if p.Develop == nil || len(p.Develop.Reports) != 2 || p.Develop.WorkItemURL != aiDevItemURL(id) {
		t.Fatalf("工作项的任务页 = %+v", p.Develop)
	}
}

// AI 任务的日志接口：响应头与 job 的日志接口相同；不存在的任务 404；未登录 401；下载完整日志。
func TestAITaskLog(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	req := e.addAIRequest(aiRequestOpts{pr: 5})
	task := e.addAITask(req, aiTaskOpts{state: ai.TaskRunning})
	att := e.addAIAttempt(task, 0, "mini", "")
	e.appendAILog(att, "hello\n")

	w := e.get(e.bob, aiTaskURL(task)+"/log?offset=0")
	wantStatus(t, "日志", w, http.StatusOK)
	h := w.Header()
	if w.Body.String() != "hello\n" || h.Get("X-Next-Offset") != "6" || h.Get("X-Finished") != "false" || h.Get("X-Job-State") != "running" ||
		h.Get("X-Job-Terminal") != "false" || h.Get("X-Attempt") != strconv.FormatInt(att, 10) || h.Get("X-Current-Attempt") != strconv.FormatInt(att, 10) {
		t.Errorf("日志 = %q，响应头 %v", w.Body.String(), h)
	}
	wantStatus(t, "不存在的任务", e.get(e.bob, aiTaskURL(task+1000)+"/log"), http.StatusNotFound)
	wantStatus(t, "offset 无效", e.get(e.bob, aiTaskURL(task)+"/log?offset=-1"), http.StatusBadRequest)
	wantStatus(t, "未登录", e.get(nil, aiTaskURL(task)+"/log"), http.StatusUnauthorized)

	d := e.browse(e.bob, aiTaskURL(task)+"/log/download")
	wantStatus(t, "下载", d, http.StatusOK)
	if d.Body.String() != "hello\n" || !strings.Contains(d.Header().Get("Content-Disposition"), "kiln-ai-task-"+strconv.FormatInt(task, 10)+"-attempt-") {
		t.Errorf("下载 = %q，%v", d.Body.String(), d.Header())
	}
}

// 取消和重跑：取消写审计日志并结束没有开始的任务；进行中的请求不能重跑；开发请求不能重跑；不存在的任务 404。
func TestPostAIRequestActions(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id := e.addAIRequest(aiRequestOpts{pr: 5})
	task := e.addAITask(id, aiTaskOpts{})

	wantStatus(t, "CSRF 不对", e.post(e.bob, aiRequestURL(id)+"/cancel", url.Values{"csrf": {"bad"}}), http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM ai_tasks WHERE id=$1 AND state='cancelled'`, task); n != 0 {
		t.Fatal("CSRF 不对时不应取消")
	}
	flash := flashZH(t, wantRedirect(t, "进行中的请求不能重跑", e.post(e.bob, aiRequestURL(id)+"/rerun", url.Values{"mode": {"all"}}), aiRequestURL(id)))
	if !strings.Contains(flash, "重跑失败：") {
		t.Errorf("提示 = %q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "取消", e.post(e.bob, aiRequestURL(id)+"/cancel", nil), aiRequestURL(id)))
	if flash != "已请求取消：没有开始的任务立即结束，执行中的任务由 AI 执行节点确认后结束。" {
		t.Errorf("提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM ai_tasks WHERE id=$1 AND state='cancelled'`, task); n != 1 {
		t.Error("取消后任务应结束")
	}
	if a := e.audits("ai_request.cancel"); len(a) != 1 || a[0].Actor != "bob" {
		t.Errorf("取消的审计日志 = %+v", a)
	}
	flash = flashZH(t, wantRedirect(t, "取消不存在的请求", e.post(e.bob, aiRequestURL(id+100)+"/cancel", nil), aiRequestURL(id+100)))
	if !strings.Contains(flash, "取消失败：") {
		t.Errorf("提示 = %q", flash)
	}

	dev := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestFailed})
	devTask := e.addAITask(dev, aiTaskOpts{state: ai.TaskFailed})
	flash = flashZH(t, wantRedirect(t, "开发任务不能重跑", e.post(e.bob, aiTaskURL(devTask)+"/rerun", nil), aiTaskURL(devTask)))
	if !strings.Contains(flash, "重跑失败：") {
		t.Errorf("提示 = %q", flash)
	}
	w := e.post(e.bob, aiTaskURL(devTask+1000)+"/rerun", nil)
	wantStatus(t, "重跑不存在的任务", w, http.StatusNotFound)
	if got := pageTextZH(t, w.Body.String()); !strings.Contains(got, "没有编号为 "+strconv.FormatInt(devTask+1000, 10)+" 的 AI 任务，没有执行任何操作。") {
		t.Errorf("重跑不存在的任务的说明 = %q", got)
	}
}

// AI 执行节点页面：只列出 AI 执行节点（CI Runner 在 CI 的页面），带角色、Orca Server、Agent、并发上限和正在执行的任务
// （包括在 Orca 中的关联）；CI 的 Runner 页面不含 AI 执行节点；两边的详情页互相跳转。
func TestPageAIRunners(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	recent := time.Now()
	e.addRunner("ci-box", runnerOpts{lastSeen: &recent})
	rev := e.addAIRunner("reviewer", store.RunnerRoleReview, &recent)
	e.addAIRunner("developer", store.RunnerRoleDevelopment, nil)
	req := e.addAIRequest(aiRequestOpts{pr: 5})
	task := e.addAITask(req, aiTaskOpts{state: ai.TaskRunning})
	e.addAIAttempt(task, rev, "", "")

	w := e.get(e.bob, aiRunnersPath)
	wantStatus(t, "AI 执行节点", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "ai_runners" || pg.Nav != "ai_runners" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[aiRunnersPage](t, w.Body.String())
	if len(p.Runners) != 2 || p.Runners[0].Name != "developer" || p.Runners[1].Name != "reviewer" {
		t.Fatalf("AI 执行节点 = %+v", p.Runners)
	}
	r := p.Runners[1]
	if r.Role != store.RunnerRoleReview || !r.Online || r.OrcaState != "unavailable" ||
		r.OrcaReason != "Orca Server 没有启动" || r.Capacity != 2 || len(r.Agents) != 1 || r.Agents[0].Agent != "codex" ||
		len(r.Tasks) != 1 || r.Tasks[0].TaskID != task || r.Tasks[0].RequestID != req || r.Tasks[0].Orca.DispatchID != "" {
		t.Errorf("reviewer = %+v", r)
	}
	if c := p.Counts; c.Total != 2 || c.Online != 1 || c.Offline != 1 || c.Accepting != 1 || c.Tasks != 1 {
		t.Errorf("统计 = %+v", c)
	}
	ci := pageData[runnersPage](t, e.get(e.bob, "/runners").Body.String())
	if len(ci.Runners) != 1 || ci.Runners[0].Name != "ci-box" || ci.Counts.Total != 1 {
		t.Errorf("CI Runner 页面 = %+v", ci.Runners)
	}

	if pg := parsePage(t, e.get(e.bob, aiRunnerURL("reviewer")).Body.String()); pg.Page != "ai_runner" || pg.Nav != "ai_runners" {
		t.Errorf("AI 执行节点详情 = %q %q", pg.Page, pg.Nav)
	}
	wantRedirect(t, "CI 详情页打开 AI 执行节点", e.get(e.bob, runnerURL("reviewer")), aiRunnerURL("reviewer"))
	wantRedirect(t, "AI 详情页打开 CI Runner", e.get(e.bob, aiRunnerURL("ci-box")), runnerURL("ci-box"))
	wantStatus(t, "不存在的 AI 执行节点", e.get(e.bob, aiRunnerURL("ghost")), http.StatusNotFound)
}

// AI 执行节点的操作与 CI Runner 共用表单，完成后回到 AI 执行节点页面；吊销时说明正在执行的 AI 任务。
func TestPostAIRunnerOps(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	recent := time.Now()
	rev := e.addAIRunner("reviewer", store.RunnerRoleReview, &recent)
	task := e.addAITask(e.addAIRequest(aiRequestOpts{pr: 5}), aiTaskOpts{state: ai.TaskRunning})
	e.addAIAttempt(task, rev, "", "")

	flash := flashZH(t, wantRedirect(t, "暂停", e.post(e.alice, "/runners/reviewer/drain", nil), aiRunnersPath))
	if !strings.Contains(flash, "正在执行的 1 个任务会继续执行到结束") {
		t.Errorf("提示 = %q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "吊销", e.post(e.alice, "/runners/reviewer/revoke", nil), aiRunnersPath))
	if !strings.Contains(flash, "1 个 AI 任务会被中断") {
		t.Errorf("提示 = %q", flash)
	}
	flash = flashZH(t, wantRedirect(t, "检查更新", e.post(e.alice, "/runners/upgrade/reviewer/check", nil), aiRunnerURL("reviewer")))
	if flash == "" {
		t.Error("检查更新应给出结果")
	}
}

// 注册 AI 执行节点的令牌被使用后，展示页显示注册出的 AI 执行节点。
func TestRunnerEnrollAIRunner(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	recent := time.Now()
	id := e.addAIRunner("reviewer", store.RunnerRoleReview, &recent)
	v, err := e.d.enrolledRunner(e.ctx, store.RunnerFilter{ID: id})
	if err != nil || v == nil || v.Name != "reviewer" || !v.Online || len(v.Platforms) != 1 || v.Platforms[0] != "darwin/arm64" {
		t.Fatalf("按 ID = %+v, %v", v, err)
	}
	if v, err := e.d.enrolledRunner(e.ctx, store.RunnerFilter{Name: "reviewer"}); err != nil || v == nil || v.ID != id {
		t.Errorf("按名称 = %+v, %v", v, err)
	}
	if v, err := e.d.enrolledRunner(e.ctx, store.RunnerFilter{Name: "ghost"}); err != nil || v != nil {
		t.Errorf("不存在 = %+v, %v", v, err)
	}
}

// AI 请求的活动：说明和链接（重跑指向新的请求）。
func TestAIActivity(t *testing.T) {
	for _, c := range []struct {
		e          activityEntry
		text, link string
	}{
		{
			activityEntry{Action: "ai_review.trigger", Detail: map[string]any{"ai_request_id": float64(5), "repo": testRepo, "ref": "main"}},
			"手动启动了 AI 审计请求 #5（tester/demo main）", aiRequestURL(5),
		},
		{activityEntry{Action: "ai_request.cancel", Detail: map[string]any{"ai_request_id": float64(5)}}, "取消了 AI 请求 #5", aiRequestURL(5)},
		{
			activityEntry{Action: "ai_request.rerun", Detail: map[string]any{"ai_request_id": float64(5), "rerun": float64(6)}},
			"重跑了 AI 审计请求 #5，新的请求 #6", aiRequestURL(6),
		},
	} {
		if got := textZH(t, c.e.Text()); got != c.text {
			t.Errorf("%s：Text = %q，应为 %q", c.e.Action, got, c.text)
		}
		if got := c.e.Link(); got != c.link {
			t.Errorf("%s：Link = %q，应为 %q", c.e.Action, got, c.link)
		}
	}
}

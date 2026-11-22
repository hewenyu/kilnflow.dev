package admin

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// aiRequestOpts 是 addAIRequest 的选项。
type aiRequestOpts struct {
	repoID int64
	typ    string // 默认 review
	state  ai.RequestState
	// pr 不为 0 时是 PR 审计；devMode 不为空时写开发请求的明细。
	pr      int
	devMode string
	round   int
	rerunOf int64
}

// addAIRequest 直接写入一个 AI 请求（首次请求，family_id 指向自己），返回 ID。
func (e *testEnv) addAIRequest(o aiRequestOpts) int64 {
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
		INSERT INTO ai_requests (id, family_id, type, repo_id, event, ref, sha, trigger, sender, state, rerun_of, finished_at)
		SELECT n.id, n.id, $1, $2, $3, 'refs/heads/main', 'abc123', $3, 'alice', $4, nullif($5::bigint, 0),
		  CASE WHEN $4 IN ('queued', 'active') THEN NULL ELSE now() END
		FROM n RETURNING id`, o.typ, o.repoID, event, string(o.state), o.rerunOf).Scan(&id)
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
		e.exec(`INSERT INTO ai_dev_requests (request_id, mode, round) VALUES ($1,$2,$3)`, id, o.devMode, o.round)
	}
	return id
}

// aiTaskOpts 是 addAITask 的选项。
type aiTaskOpts struct {
	key   string // 默认 macos-arm64
	state ai.TaskState
}

// addAITask 直接写入请求的一个任务，返回 ID。
func (e *testEnv) addAITask(requestID int64, o aiTaskOpts) int64 {
	e.t.Helper()
	if o.key == "" {
		o.key = "macos-arm64"
	}
	if o.state == "" {
		o.state = ai.TaskPending
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes,
		  queued_at, finished_at)
		SELECT $1, $2, q.type, '{darwin/arm64}', 'codex', 'AI 审计（'||$2||'）', '{}',
		  $3, 60, now(), CASE WHEN $3 IN ('waiting', 'pending', 'running') THEN NULL ELSE now() END
		FROM ai_requests q WHERE q.id=$1 RETURNING id`, requestID, o.key, string(o.state)).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// addAIAttempt 为任务写入一个 attempt（成为任务的当前 attempt），返回 ID。runnerID 为 0 时只记下 Runner 的名称 runnerName。
// outcome 为空表示还在执行。
func (e *testEnv) addAIAttempt(taskID, runnerID int64, runnerName string, outcome store.Outcome) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, runner_name, platform, acquire_request_id, outcome, finished_at)
		SELECT $1, t.epoch+1, nullif($2::bigint, 0), nullif($3, ''), t.platforms[1], 'acq', nullif($4, ''), CASE WHEN $4 = '' THEN NULL ELSE now() END
		FROM ai_tasks t WHERE t.id=$1 RETURNING id`, taskID, runnerID, runnerName, string(outcome)).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE ai_tasks SET current_attempt_id=$2, epoch=epoch+1, attempt_count=attempt_count+1 WHERE id=$1`, taskID, id)
	return id
}

// addAIRunner 直接写入一个 AI 执行节点（角色 role），返回 ID。
func (e *testEnv) addAIRunner(name, role string, lastSeen *time.Time) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO runners (name, kind, public_key, version, state, last_seen_at)
		VALUES ($1,'ai','\x00','v1.0.0','active',$2) RETURNING id`, name, lastSeen).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO ai_runners (runner_id, role, platform, protocol, host_id, capacity, orca_state, orca_reason, running)
		VALUES ($1,$2,'macos/arm64',$3,'host-1',2,'unavailable','Orca Server 没有启动',1)`, id, role, agentcfg.AIProtocol)
	return id
}

func requestIDs(list []*AIRequestView) []int64 {
	out := make([]int64, len(list))
	for i, v := range list {
		out[i] = v.ID
	}
	return out
}

func equalIDs(a, b []int64) bool {
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

// TestService_ListAIRequests：按仓库、类型、PR、状态过滤，按 ID 倒序，限制条数；补上仓库名、PR、开发方式；
// 未知的类型、状态是参数错误，不存在的仓库是 NotFound。
func TestService_ListAIRequests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addRepo(301, "other")
	pr7 := e.addAIRequest(aiRequestOpts{pr: 7, state: ai.RequestSucceeded})
	pr8 := e.addAIRequest(aiRequestOpts{pr: 8, state: ai.RequestFailed})
	manual := e.addAIRequest(aiRequestOpts{})
	dev := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "fix", round: 2})
	other := e.addAIRequest(aiRequestOpts{repoID: 301, pr: 7})

	cases := []struct {
		name string
		f    AIRequestFilter
		want []int64
	}{
		{"全部", AIRequestFilter{}, []int64{other, dev, manual, pr8, pr7}},
		{"仓库", AIRequestFilter{Repo: testAccount + "/demo"}, []int64{dev, manual, pr8, pr7}},
		{"类型", AIRequestFilter{Type: agentcfg.TaskDevelop}, []int64{dev}},
		{"PR", AIRequestFilter{PR: 7}, []int64{other, pr7}},
		{"仓库和 PR", AIRequestFilter{RepoID: testRepoID, PR: 7}, []int64{pr7}},
		{"状态", AIRequestFilter{State: string(ai.RequestFailed)}, []int64{pr8}},
		{"条数", AIRequestFilter{Limit: 2}, []int64{other, dev}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list, err := e.svc.ListAIRequests(e.ctx, c.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := requestIDs(list); !equalIDs(got, c.want) {
				t.Errorf("ListAIRequests(%+v) = %v，应为 %v", c.f, got, c.want)
			}
		})
	}

	list, err := e.svc.ListAIRequests(e.ctx, AIRequestFilter{RepoID: testRepoID})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*AIRequestView{}
	for _, v := range list {
		byID[v.ID] = v
	}
	if v := byID[pr7]; v.Repo != testAccount+"/demo" || v.PRNumber != 7 || v.Type != agentcfg.TaskReview || v.State != ai.RequestSucceeded {
		t.Errorf("PR 审计 = %+v", v)
	}
	if v := byID[dev]; v.DevMode != "fix" || v.DevRound != 2 || v.PRNumber != 0 {
		t.Errorf("开发请求 = %+v", v)
	}

	for _, f := range []AIRequestFilter{{Type: "build"}, {State: "running"}} {
		if _, err := e.svc.ListAIRequests(e.ctx, f); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("ListAIRequests(%+v) = %v，应为参数错误", f, err)
		}
	}
	if _, err := e.svc.ListAIRequests(e.ctx, AIRequestFilter{Repo: "nobody/x"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("不存在的仓库：%v", err)
	}
}

// TestService_AIRequest：详情包括各平台任务和当前 attempt 的 Runner（已删除的 Runner 用 attempt 记下的名称）。
func TestService_AIRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addAIRequest(aiRequestOpts{pr: 5})
	mac := e.addAITask(id, aiTaskOpts{state: ai.TaskRunning})
	linux := e.addAITask(id, aiTaskOpts{key: "linux-amd64", state: ai.TaskFailed})
	waiting := e.addAITask(id, aiTaskOpts{key: "windows-amd64"})
	runner := e.addAIRunner("mini", store.RunnerRoleReview, nil)
	e.addAIAttempt(mac, runner, "", "")
	e.addAIAttempt(linux, 0, "gone", store.OutcomeFailed)

	d, err := e.svc.AIRequest(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != id || d.PRNumber != 5 || d.Repo != testAccount+"/demo" || len(d.Tasks) != 3 {
		t.Fatalf("详情 = %+v，任务 %d 个", d.AIRequestView, len(d.Tasks))
	}
	want := map[int64]string{mac: "mini", linux: "gone"}
	if len(d.Attempts) != len(want) || d.Attempts[mac].Runner != want[mac] || d.Attempts[linux].Runner != want[linux] {
		t.Errorf("Attempts = %v，节点应为 %v（%d 没有 attempt）", d.Attempts, want, waiting)
	}
	if _, err := e.svc.AIRequest(e.ctx, id+100); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("不存在的请求：%v", err)
	}
}

// TestService_CancelAIRequest：取消后没有开始的任务立即结束、写审计日志；不存在的请求是 NotFound。
func TestService_CancelAIRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addAIRequest(aiRequestOpts{pr: 5})
	task := e.addAITask(id, aiTaskOpts{})
	if err := e.svc.CancelAIRequest(e.ctx, id, testActor); err != nil {
		t.Fatal(err)
	}
	tk, err := ai.GetTask(e.ctx, e.db, task, false)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != ai.TaskCancelled {
		t.Errorf("任务状态 = %s，应为 cancelled", tk.State)
	}
	e.wantAudit("ai_request.cancel", testActor, map[string]any{"ai_request_id": float64(id)})
	if err := e.svc.CancelAIRequest(e.ctx, id+100, testActor); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("不存在的请求：%v", err)
	}
}

// TestService_RerunAIRequestArgs：重跑方式与任务的组合不对是参数错误；不存在的请求是 NotFound；开发请求不能重跑；
// 没有结束的请求不能重跑。
func TestService_RerunAIRequestArgs(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	done := e.addAIRequest(aiRequestOpts{pr: 5, state: ai.RequestFailed})
	task := e.addAITask(done, aiTaskOpts{state: ai.TaskFailed})
	active := e.addAIRequest(aiRequestOpts{pr: 6})
	dev := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestFailed})

	cases := []struct {
		name   string
		id     int64
		mode   review.RerunMode
		taskID int64
		code   connect.Code
	}{
		{"未知的方式", done, "some", 0, connect.CodeInvalidArgument},
		{"task 没有任务", done, review.RerunTask, 0, connect.CodeInvalidArgument},
		{"all 指定任务", done, review.RerunAll, task, connect.CodeInvalidArgument},
		{"默认方式指定任务", done, "", task, connect.CodeInvalidArgument},
		{"不存在", done + 1000, review.RerunAll, 0, connect.CodeNotFound},
		{"开发请求", dev, review.RerunAll, 0, connect.CodeFailedPrecondition},
		{"没有结束", active, review.RerunAll, 0, connect.CodeFailedPrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.RerunAIRequest(e.ctx, c.id, c.mode, c.taskID, testActor, store.SourceDashboard)
			if connect.CodeOf(err) != c.code {
				t.Errorf("RerunAIRequest = %v，应为 %s", err, c.code)
			}
		})
	}
	// 开发请求不能重跑的原因来自 AI 核心
	if _, err := e.svc.RerunAIRequest(e.ctx, dev, review.RerunAll, 0, testActor, store.SourceDashboard); !errors.Is(err, ai.ErrNotRerunnable) {
		t.Errorf("开发请求：%v，应为 ErrNotRerunnable", err)
	}
	if _, err := e.svc.RerunAIRequest(e.ctx, active, review.RerunAll, 0, testActor, store.SourceDashboard); !errors.Is(err, review.ErrNotFinished) {
		t.Errorf("没有结束的请求：%v，应为 ErrNotFinished", err)
	}
}

// testAIConfig 是启用审计（macOS arm64，Codex）的 .kiln/agent/config.yml。
const testAIConfig = "version: 1\nreview:\n  agent: codex\n  required_platforms:\n    - {os: macos, arch: arm64}\n"

// newAIEnv 返回仓库中启用了 AI 审计的环境。
func newAIEnv(t *testing.T) *testEnv {
	t.Helper()
	files := map[string]string{agentcfg.ConfigPath: testAIConfig}
	maps.Copy(files, testRepoFiles)
	return newEnvWith(t, envOpts{github: true, files: files})
}

// TestService_TriggerAndRerunAIReview：手动审计建一个审计请求（每个平台一个任务）；结束后重跑建新请求，rerun_of 指向它，
// 只重跑没有成功的任务；没有启用 AI 审计的 Server 返回 FailedPrecondition。
func TestService_TriggerAndRerunAIReview(t *testing.T) {
	t.Parallel()
	e := newAIEnv(t)
	repo := testAccount + "/demo"
	id, sha, err := e.svc.TriggerAIReview(e.ctx, repo, "main", "req-1", testActor, store.SourceDashboard)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.svc.AIRequest(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.SHA != sha || d.Type != agentcfg.TaskReview || len(d.Tasks) != 1 || d.Tasks[0].Key != "review-macos-arm64" {
		t.Fatalf("审计请求 = %+v，任务 %+v", d.Request, d.Tasks[0])
	}
	// 同一个 request ID 不重复建请求
	if again, _, err := e.svc.TriggerAIReview(e.ctx, repo, "main", "req-1", testActor, store.SourceDashboard); err != nil || again != id {
		t.Errorf("重复触发 = %d, %v，应为 %d", again, err, id)
	}

	e.exec(`UPDATE ai_tasks SET state='failed', finished_at=now() WHERE request_id=$1`, id)
	e.exec(`UPDATE ai_requests SET state='failed', finished_at=now() WHERE id=$1`, id)
	r, err := e.svc.RerunAIRequest(e.ctx, id, "", 0, testActor, store.SourceDashboard)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == id || r.RerunOf == nil || *r.RerunOf != id {
		t.Fatalf("重跑的请求 = %+v，应指向 #%d", r, id)
	}
	e.wantAudit("ai_request.rerun", testActor, map[string]any{"ai_request_id": float64(id), "rerun": float64(r.ID), "mode": string(review.RerunFailed)})

	bare := newEnv(t)
	bare.svc.reviews = nil
	if _, _, err := bare.svc.TriggerAIReview(bare.ctx, repo, "main", "", testActor, store.SourceDashboard); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("没有 AI 审计：%v", err)
	}
}

// TestService_AITaskLog：读取当前 attempt 或指定 attempt 的日志；当前 attempt 结束并读到末尾才算结束；
// 没有 attempt 的任务没有日志（任务结束即结束）；不属于这个任务的 attempt、不存在的任务是 NotFound；offset 不能为负。
func TestService_AITaskLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	req := e.addAIRequest(aiRequestOpts{pr: 5})
	task := e.addAITask(req, aiTaskOpts{state: ai.TaskRunning})
	first := e.addAIAttempt(task, 0, "old", store.OutcomeFailed)
	second := e.addAIAttempt(task, 0, "mini", "")
	if _, err := e.aiLogs.Append(first, 0, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.aiLogs.Append(second, 0, []byte("second\n")); err != nil {
		t.Fatal(err)
	}

	l, err := e.svc.AITaskLog(e.ctx, task, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(l.Data) != "second\n" || l.Next != int64(len("second\n")) || l.Finished || l.Attempt != second || l.CurrentAttempt != second ||
		l.State != string(ai.TaskRunning) || l.Terminal {
		t.Errorf("当前 attempt 的日志 = %+v", l)
	}
	if l, err := e.svc.AITaskLog(e.ctx, task, first, 0); err != nil || string(l.Data) != "first\n" || l.Attempt != first || l.CurrentAttempt != second {
		t.Errorf("指定 attempt 的日志 = %+v, %v", l, err)
	}
	// 已结束的 attempt：读到末尾才结束
	if l, err := e.svc.AITaskLog(e.ctx, task, first, 0); err != nil || l.Finished {
		t.Errorf("还有没读的内容时不应结束：%+v %v", l, err)
	}
	if l, err := e.svc.AITaskLog(e.ctx, task, first, int64(len("first\n"))); err != nil || !l.Finished {
		t.Errorf("读到末尾后应结束：%+v %v", l, err)
	}
	// 执行中的 attempt 读到末尾也不结束
	if l, err := e.svc.AITaskLog(e.ctx, task, 0, int64(len("second\n"))); err != nil || l.Finished {
		t.Errorf("执行中的 attempt 不应结束：%+v %v", l, err)
	}

	wantAILogDownload(t, e, task, first)

	// 没有 attempt 的任务
	idle := e.addAITask(req, aiTaskOpts{key: "linux-amd64", state: ai.TaskSkipped})
	if l, err := e.svc.AITaskLog(e.ctx, idle, 0, 0); err != nil || !l.Finished || l.Attempt != 0 || len(l.Data) != 0 || !l.Terminal {
		t.Errorf("没有 attempt 的已结束任务 = %+v, %v", l, err)
	}
	pending := e.addAITask(req, aiTaskOpts{key: "windows-amd64"})
	if l, err := e.svc.AITaskLog(e.ctx, pending, 0, 0); err != nil || l.Finished {
		t.Errorf("没有 attempt 的排队任务不应结束：%+v, %v", l, err)
	}
	wantAILogErrors(t, e, task, idle, first, second)
}

// wantAILogDownload 检查下载 AI 任务 task 的 attempt 的完整日志（内容为 "first\n"）。
func wantAILogDownload(t *testing.T, e *testEnv, task, attempt int64) {
	t.Helper()
	f, err := e.svc.OpenAITaskLog(e.ctx, task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	buf := make([]byte, 64)
	for {
		n, rerr := f.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	_ = f.Close()
	if b.String() != "first\n" || f.Attempt != attempt || f.Size != int64(len("first\n")) {
		t.Errorf("下载 = %q attempt %d size %d", b.String(), f.Attempt, f.Size)
	}
}

// wantAILogErrors 检查读取日志的错误：不属于这个任务的 attempt、不存在的任务和 attempt 是 NotFound，offset 不能为负。
func wantAILogErrors(t *testing.T, e *testEnv, task, idle, first, second int64) {
	t.Helper()
	for name, c := range map[string]struct{ task, attempt, offset int64 }{
		"别的任务的 attempt": {idle, first, 0},
		"不存在的任务":        {task + 1000, 0, 0},
		"不存在的 attempt":  {task, second + 1000, 0},
	} {
		if _, err := e.svc.AITaskLog(e.ctx, c.task, c.attempt, c.offset); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s：%v，应为 NotFound", name, err)
		}
	}
	if _, err := e.svc.AITaskLog(e.ctx, task, 0, -1); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("负的 offset：%v", err)
	}
}

// TestService_ListAIRunners：只列出 AI 执行节点（不含 CI Runner），带显示状态。
func TestService_ListAIRunners(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	recent := time.Now()
	e.addRunner("ci-box", runnerOpts{lastSeen: &recent})
	rev := e.addAIRunner("reviewer", store.RunnerRoleReview, &recent)
	e.addAIRunner("developer", store.RunnerRoleDevelopment, nil)
	list, err := e.svc.ListAIRunners(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "developer" || list[1].Name != "reviewer" {
		t.Fatalf("AI 执行节点 = %v", list)
	}
	r := list[1]
	if r.ID != rev || r.Role != store.RunnerRoleReview || r.OrcaState != "unavailable" || AIRunnerState(r.RunnerView) != store.RunnerActive {
		t.Errorf("reviewer = %+v，状态 %s", r.RunnerView, AIRunnerState(r.RunnerView))
	}
	if st := AIRunnerState(list[0].RunnerView); st != store.RunnerOffline {
		t.Errorf("没有上线过的 developer 状态 = %s，应为 offline", st)
	}
	one, err := e.svc.ListAIRunners(e.ctx, rev)
	if err != nil || len(one) != 1 || one[0].ID != rev {
		t.Errorf("按 ID 列出 = %v, %v", one, err)
	}
	// CI 的列表不含 AI 执行节点
	ci, err := e.svc.ListRunners(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ci) != 1 || ci[0].Name != "ci-box" {
		t.Errorf("CI Runner = %v", ci)
	}
}

// TestRPC_AI：AI 请求、任务、日志和 AI 执行节点的 RPC 与 Service 一致，错误码供 CLI 显示。
func TestRPC_AI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.rpcClient(testAdminToken)
	runner := e.addAIRunner("reviewer", store.RunnerRoleReview, nil)
	id := e.addAIRequest(aiRequestOpts{pr: 9, state: ai.RequestFailed})
	task := e.addAITask(id, aiTaskOpts{state: ai.TaskFailed})
	att := e.addAIAttempt(task, runner, "", store.OutcomeFailed)
	e.exec(`UPDATE ai_tasks SET reason='超时' WHERE id=$1`, task)
	if _, err := e.aiLogs.Append(att, 0, []byte("log\n")); err != nil {
		t.Fatal(err)
	}

	list, err := c.ListAIRequests(e.ctx, req(&apiv1.ListAIRequestsRequest{Repo: testAccount + "/demo", PrNumber: 9}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.Requests) != 1 {
		t.Fatalf("列出 = %v", list.Msg.Requests)
	}
	if r := list.Msg.Requests[0]; r.Id != id || r.PrNumber != 9 || r.State != string(ai.RequestFailed) || r.Type != agentcfg.TaskReview ||
		r.Repo != testAccount+"/demo" || r.FinishedAt == 0 {
		t.Errorf("请求 = %v", r)
	}
	if _, err := c.ListAIRequests(e.ctx, req(&apiv1.ListAIRequestsRequest{State: "nope"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("未知的状态：%v", err)
	}

	got, err := c.GetAIRequest(e.ctx, req(&apiv1.GetAIRequestRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Msg.Tasks) != 1 {
		t.Fatalf("任务 = %v", got.Msg.Tasks)
	}
	if tk := got.Msg.Tasks[0]; tk.Id != task || tk.RequestId != id || tk.Runner != "reviewer" || tk.Reason != "超时" || tk.AttemptCount != 1 ||
		tk.State != string(ai.TaskFailed) || tk.Platform != "darwin/arm64" || !slices.Equal(tk.Platforms, []string{"darwin/arm64"}) {
		t.Errorf("任务 = %v", tk)
	}
	if _, err := c.GetAIRequest(e.ctx, req(&apiv1.GetAIRequestRequest{Id: id + 100})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("不存在的请求：%v", err)
	}

	l, err := c.GetAITaskLog(e.ctx, req(&apiv1.GetAITaskLogRequest{TaskId: task}))
	if err != nil || string(l.Msg.Data) != "log\n" || l.Msg.NextOffset != 4 || l.Msg.Finished {
		t.Errorf("日志 = %v, %v", l, err)
	}
	if _, err := c.CancelAIRequest(e.ctx, req(&apiv1.CancelAIRequestRequest{Id: id + 100})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("取消不存在的请求：%v", err)
	}
	if _, err := c.RerunAIRequest(e.ctx, req(&apiv1.RerunAIRequestRequest{Id: id, Mode: "bad"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("未知的重跑方式：%v", err)
	}
}

// TestRPC_ListAIRunners：AI 执行节点的 RPC 带角色、状态、平台、Orca Server、并发上限和运行中的任务数。
func TestRPC_ListAIRunners(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.rpcClient(testAdminToken)
	recent := time.Now()
	runner := e.addAIRunner("reviewer", store.RunnerRoleReview, &recent)
	runners, err := c.ListAIRunners(e.ctx, req(&apiv1.ListAIRunnersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(runners.Msg.Runners) != 1 {
		t.Fatalf("AI 执行节点 = %v", runners.Msg.Runners)
	}
	if r := runners.Msg.Runners[0]; r.Id != runner || r.Role != store.RunnerRoleReview || r.State != store.RunnerActive || r.OrcaState != "unavailable" ||
		r.OrcaReason != "Orca Server 没有启动" || r.Capacity != 2 || r.Running != 1 || r.Platform != "macos/arm64" || r.HostId != "host-1" ||
		r.LastSeenAt != recent.Unix() {
		t.Errorf("AI 执行节点 = %v", r)
	}
}

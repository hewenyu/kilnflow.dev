package review

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// prReviewed 规划 PR #7 的审计（main 上启用 config，feature 分支上有一个 commit），返回请求和它唯一的任务。
func (h *harness) prReviewed(config string) (reqID int64, task *ai.Task) {
	h.t.Helper()
	base := h.enableReview(config)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	reqID = h.planPR(h.prEvent("opened", head, base))
	ts := h.tasks(reqID)
	if len(ts) != 1 {
		h.t.Fatalf("任务 %d 个，应为 1 个", len(ts))
	}
	return reqID, ts[0]
}

// attemptResult 返回 attempt 提交后的结果和错误分类、说明。
func (h *harness) attemptResult(aid int64) (outcome store.Outcome, class, msg string) {
	h.t.Helper()
	a, err := ai.GetAttempt(h.ctx, h.db, aid)
	if err != nil {
		h.t.Fatal(err)
	}
	if a.Outcome != nil {
		outcome = *a.Outcome
	}
	if a.ErrorClass != nil {
		class = *a.ErrorClass
	}
	if a.Message != nil {
		msg = *a.Message
	}
	return outcome, class, msg
}

// TestFinish_verdicts：审计的结果规则（PLAN #30.6）：Runner 报告成功时按报告的结论决定任务的结果——没有发现问题才算通过；
// 发现问题、审计未完成、没有报告、报告无效都不通过（各有错误分类）；Runner 报告的其他结果原样保留，不保存报告。
func TestFinish_verdicts(t *testing.T) {
	incomplete := &aireport.Report{Version: aireport.Version, Verdict: aireport.VerdictIncomplete, Summary: strings.Repeat("长", maxVerdictMessage*2), Scope: "x"}
	tests := []struct {
		name      string
		res       ai.Result
		outcome   store.Outcome
		class     string
		msg       string
		task      ai.TaskState
		saved     string // 保存的结论，空表示不保存
		published bool
	}{
		{"没有发现问题", reportResult(cleanReport()), store.OutcomeSucceeded, "", "", ai.TaskSucceeded, aireport.VerdictNoIssues, true},
		{"发现问题", reportResult(testReport()), store.OutcomeFailed, ai.ErrClassFindings, "AI 审计发现 3 个问题", ai.TaskFailed, aireport.VerdictIssuesFound, true},
		{"审计未完成", reportResult(incomplete), store.OutcomeFailed, ai.ErrClassIncomplete, "AI 审计未完成：", ai.TaskFailed, aireport.VerdictIncomplete, true},
		{"没有报告", ai.Result{Outcome: store.OutcomeSucceeded}, store.OutcomeFailed, ai.ErrClassReport, "AI 执行节点没有提交审计报告", ai.TaskFailed, "", false},
		{"报告无效", ai.Result{Outcome: store.OutcomeSucceeded, ReviewReport: []byte(`{"verdict":"ok"}`)}, store.OutcomeFailed, ai.ErrClassReport, "审计报告无效：", ai.TaskFailed, "", false},
		{"超时原样保留", ai.Result{Outcome: store.OutcomeTimedOut, ErrClass: "timeout", Message: "超时", ReviewReport: cleanReport().Marshal()}, store.OutcomeTimedOut, "timeout", "超时", ai.TaskTimedOut, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			runner := h.addRunner("review1", "host-review")
			reqID, task := h.prReviewed(reviewConfig)
			aid := h.finish(task.ID, runner, tt.res)

			outcome, class, msg := h.attemptResult(aid)
			if outcome != tt.outcome || class != tt.class || !strings.HasPrefix(msg, tt.msg) {
				t.Fatalf("attempt = %s / %q / %q，应为 %s / %q / %q…", outcome, class, msg, tt.outcome, tt.class, tt.msg)
			}
			if tt.class == ai.ErrClassIncomplete && len([]rune(msg)) > len([]rune(tt.msg))+maxVerdictMessage+1 {
				t.Errorf("审计未完成的说明没有截断摘要：%d 个字符", len([]rune(msg)))
			}
			if got := h.tasks(reqID)[0].State; got != tt.task {
				t.Errorf("任务状态 = %s，应为 %s", got, tt.task)
			}
			rep, err := GetReport(h.ctx, h.db, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.saved == "" {
				if rep != nil {
					t.Fatalf("不应保存报告：%+v", rep)
				}
				if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1`, KindPRReview); n != 0 {
					t.Fatalf("没有报告时不应写发布意图：%d", n)
				}
				return
			}
			if rep == nil || rep.Verdict != tt.saved || rep.PRNumber == nil || *rep.PRNumber != aiPR || rep.Agent != "codex" {
				t.Fatalf("保存的报告 = %+v，结论应为 %s", rep, tt.saved)
			}
			var runnerVersion, agentVersion, model string
			var attemptID int64
			if err := h.db.QueryRow(h.ctx, `SELECT runner_version, agent_version, model, attempt_id FROM ai_reviews WHERE task_id=$1`, task.ID).
				Scan(&runnerVersion, &agentVersion, &model, &attemptID); err != nil {
				t.Fatal(err)
			}
			if runnerVersion != testRunnerVersion || agentVersion != "codex 1.2.3" || model != "gpt-test" || attemptID != aid {
				t.Errorf("适用范围 = %s / %s / %s / attempt %d", runnerVersion, agentVersion, model, attemptID)
			}
			if a, _ := ai.GetAttempt(h.ctx, h.db, aid); a.AgentVersion != "codex 1.2.3" || a.AgentModel != "gpt-test" {
				t.Errorf("attempt 的 Agent 版本、模型 = %s / %s（不通过时也要保留）", a.AgentVersion, a.AgentModel)
			}
			if rep.ReviewState != StatePending {
				t.Errorf("review_state = %s，PR 审计默认发布，应为 pending", rep.ReviewState)
			}
			if n := h.queryInt(`SELECT count(*) FROM outbox WHERE dedupe_key=$1 AND kind=$2`, prReviewKey(task.ID), KindPRReview); n != 1 {
				t.Errorf("发布意图 %d 条，应为 1", n)
			}
		})
	}
}

// TestFinish_scrubsLocalPaths：保存前去掉报告中残留的本机路径。
func TestFinish_scrubsLocalPaths(t *testing.T) {
	h := newHarness(t)
	runner := h.addRunner("review1", "host-review")
	_, task := h.prReviewed(reviewConfig)
	rep := cleanReport()
	rep.Summary = "检查了 /Users/alice/work/demo/f.go"
	h.finish(task.ID, runner, reportResult(rep))
	saved, err := GetReport(h.ctx, h.db, task.ID)
	if err != nil || saved == nil {
		t.Fatalf("报告：%v（%v）", saved, err)
	}
	if strings.Contains(saved.Summary, "/Users/alice") || strings.Contains(string(saved.Report), "/Users/alice") {
		t.Fatalf("保存的报告残留本机路径：%q", saved.Summary)
	}
}

// TestFinish_publishOnlyForPRWithComments：只有 PR 审计并且配置了 review 评论时才写发布意图；关闭评论、手动审计的报告
// review_state 为 none。
func TestFinish_publishOnlyForPRWithComments(t *testing.T) {
	t.Run("关闭评论", func(t *testing.T) {
		h := newHarness(t)
		runner := h.addRunner("review1", "host-review")
		_, task := h.prReviewed(reviewConfig + "  publish:\n    review_comments: false\n")
		h.finish(task.ID, runner, reportResult(testReport()))
		h.assertNotPublished(task.ID)
	})
	t.Run("手动审计", func(t *testing.T) {
		h := newHarness(t)
		runner := h.addRunner("review1", "host-review")
		h.enableReview(reviewConfig)
		id, _, err := h.rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", Actor: "bob", Source: store.SourceCLI})
		if err != nil {
			t.Fatal(err)
		}
		ts := h.tasks(id)
		if len(ts) != 1 {
			t.Fatalf("任务 %d 个", len(ts))
		}
		h.finish(ts[0].ID, runner, reportResult(testReport()))
		h.assertNotPublished(ts[0].ID)
		if got := h.events.list(); len(got) != 0 {
			t.Fatalf("手动审计不应写 PR 验证事件：%+v", got)
		}
	})
}

func (h *harness) assertNotPublished(taskID int64) {
	h.t.Helper()
	rep, err := GetReport(h.ctx, h.db, taskID)
	if err != nil || rep == nil {
		h.t.Fatalf("报告：%v（%v）", rep, err)
	}
	if rep.ReviewState != StateNone {
		h.t.Errorf("review_state = %s，应为 none", rep.ReviewState)
	}
	if n := h.queryInt(`SELECT count(*) FROM outbox WHERE kind=$1`, KindPRReview); n != 0 {
		h.t.Errorf("不应写发布意图：%d", n)
	}
}

// TestFinish_resubmitOverwrites：同一个任务再次保存（重新执行后的报告，例如基础设施错误后重新领取）覆盖旧的结论和适用范围
// （attempt、Runner / Agent 版本、模型），清掉发布的错误。
func TestFinish_resubmitOverwrites(t *testing.T) {
	h := newHarness(t)
	runner := h.addRunner("review1", "host-review")
	_, task := h.prReviewed(reviewConfig)
	h.finish(task.ID, runner, reportResult(testReport()))
	h.exec(`UPDATE ai_reviews SET review_state='failed', review_error='旧的错误' WHERE task_id=$1`, task.ID)

	const newVersion = "0.4.0"
	other := h.addRunner("review2", "host-review-2")
	h.exec(`UPDATE runners SET version=$2 WHERE id=$1`, other, newVersion)
	h.exec(`UPDATE ai_tasks SET state='pending', finished_at=NULL WHERE id=$1`, task.ID)
	res := reportResult(cleanReport())
	res.AgentVersion, res.AgentModel = "codex 2.0.0", "gpt-next"
	aid := h.finish(task.ID, other, res)

	rep, _ := GetReport(h.ctx, h.db, task.ID)
	if rep.Verdict != aireport.VerdictNoIssues || rep.Findings != 0 || rep.ReviewState != StatePending || rep.ReviewError != nil {
		t.Fatalf("覆盖后的报告 = %+v", rep)
	}
	var runnerVersion, agentVersion, model string
	var attemptID int64
	if err := h.db.QueryRow(h.ctx, `SELECT runner_version, agent_version, model, attempt_id FROM ai_reviews WHERE task_id=$1`, task.ID).
		Scan(&runnerVersion, &agentVersion, &model, &attemptID); err != nil {
		t.Fatal(err)
	}
	if runnerVersion != newVersion || agentVersion != "codex 2.0.0" || model != "gpt-next" || attemptID != aid {
		t.Fatalf("覆盖后的适用范围 = %s / %s / %s / attempt %d，应为新的 attempt %d", runnerVersion, agentVersion, model, attemptID, aid)
	}
}

// TestAfterFinish_prVerified：PR 审计请求结束时写 PR 验证事件，通过 = succeeded 或 skipped；手动审计、没有明细的请求不写。
func TestAfterFinish_prVerified(t *testing.T) {
	h := newHarness(t)
	runner := h.addRunner("review1", "host-review")
	_, task := h.prReviewed(reviewConfig)
	h.finish(task.ID, runner, reportResult(cleanReport()))
	if got := h.events.list(); !slices.Equal(got, []prVerified{{testRepoID, aiPR, true}}) {
		t.Fatalf("事件 = %+v，应为 PR #%d 通过", got, aiPR)
	}

	tests := []struct {
		state  ai.RequestState
		passed bool
	}{
		{ai.RequestSucceeded, true}, {ai.RequestSkipped, true}, {ai.RequestFailed, false}, {ai.RequestCancelled, false},
	}
	for _, tt := range tests {
		id, _ := h.insertRequest(fixture{sha: "abc", pr: aiPR, baseRef: "main", state: tt.state})
		h.events.events = nil
		h.tx(func(tx pgx.Tx) error { return h.typ.AfterFinish(h.ctx, tx, h.request(id)) })
		if got := h.events.list(); !slices.Equal(got, []prVerified{{testRepoID, aiPR, tt.passed}}) {
			t.Errorf("%s：事件 = %+v，通过应为 %v", tt.state, got, tt.passed)
		}
	}

	// 手动审计（event 不是 pull_request）、没有 PR 编号的请求：不写
	manual, _ := h.insertRequest(fixture{event: eventManual, sha: "abc", pr: aiPR, state: ai.RequestSucceeded})
	noPR, _ := h.insertRequest(fixture{sha: "abc", state: ai.RequestSucceeded})
	h.events.events = nil
	for _, id := range []int64{manual, noPR} {
		h.tx(func(tx pgx.Tx) error { return h.typ.AfterFinish(h.ctx, tx, h.request(id)) })
	}
	if got := h.events.list(); len(got) != 0 {
		t.Fatalf("手动审计、没有 PR 的请求不应写事件：%+v", got)
	}
}

// TestAfterFinish_cancelledBySupersede：被新 head 取代的审计也结束（不通过），合并队列据此不再等它。
func TestAfterFinish_cancelledBySupersede(t *testing.T) {
	h := newHarness(t)
	reqID, _ := h.prReviewed(reviewConfig)
	head2 := h.commitOn(featureRef, map[string]string{"g.go": "package f\n"}, "第二个 commit")
	h.planPR(h.prEvent("synchronize", head2, h.branchHead("main")))
	if r := h.request(reqID); r.State != ai.RequestCancelled {
		t.Fatalf("旧 head 的请求 = %s，应被取代", r.State)
	}
	if got := h.events.list(); !slices.Equal(got, []prVerified{{testRepoID, aiPR, false}}) {
		t.Fatalf("事件 = %+v", got)
	}
}

// devWorkItem 写入一个开发过 PR pr（pr 为 0 时还没有记下编号）的工作项，开发机为 hosts，返回它的 ID。
// devWorkItem 建一个开发过 agents 的工作项（PR pr，0 表示还没有记下编号）。
func (h *harness) devWorkItem(pr int, agents ...string) int64 {
	h.t.Helper()
	runner := h.addRunner("dev-"+strings.Join(agents, "-"), "host-dev")
	_, tasks := h.insertRequest(fixture{typ: agentcfg.TaskDevelop, event: eventManual, sha: "x", state: ai.RequestSucceeded, tasks: []ai.TaskState{ai.TaskSucceeded}})
	aid := h.insertAttempt(tasks[0], runner)
	var prNum *int
	if pr != 0 {
		prNum = &pr
	}
	var id int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, pr_number, max_fix_rounds, agent, claim_attempt_id, dev_agents)
		VALUES ($1, 'pr_open', 'main', 'base', $2, 2, 'claude-code', $3, $4) RETURNING id`, testRepoID, prNum, aid, agents).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// mismatch 返回不能领取的任务和原因（开发过这个工作项的 Agent 不能审计它的 PR，与节点无关）。
func (h *harness) mismatch(cands []*ai.Task) map[int64]string {
	h.t.Helper()
	var out map[int64]string
	h.tx(func(tx pgx.Tx) error {
		var err error
		out, err = h.typ.Mismatch(h.ctx, tx, &ai.Runner{ID: 1, Role: store.RunnerRoleReview}, cands)
		return err
	})
	return out
}

// TestMismatch_agentIsolation：开发过工作项的 Agent 不审计它的 PR（PLAN #33.7）：按 PR 编号关联，PR 来自正式分支
// kiln/dev-<id> 时也按分支关联；审计用的是另一个 Agent 时可以；不是工作项的 PR 不限制。
func TestMismatch_agentIsolation(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	reqID := h.planPR(h.prEvent("opened", head, base))
	task := h.tasks(reqID)[0]

	// 不是工作项的 PR：谁都可以审计
	if got := h.mismatch([]*ai.Task{task}); len(got) != 0 {
		t.Fatalf("不是工作项的 PR 不应限制：%v", got)
	}
	if task.Agent != agentcfg.AgentCodex {
		t.Fatalf("审计 Agent = %s，应为配置中的 codex", task.Agent)
	}

	// 这个工作项是 Codex 开发的：同一个 Agent 不能审计（审计任务配置的是 codex）
	item := h.devWorkItem(aiPR, agentcfg.AgentCodex)
	if got := h.mismatch([]*ai.Task{task}); !strings.Contains(got[task.ID], "与开发 Agent 相同") ||
		!strings.Contains(got[task.ID], "#"+strconv.FormatInt(item, 10)) {
		t.Fatalf("Codex 开发过的工作项：%v，应说明原因和工作项编号", got)
	}

	// 换一个没有开发过它的审计 Agent：重新规划后可以审计
	h2 := newHarness(t)
	base2 := h2.enableReview(reviewConfig)
	head2 := h2.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	req2 := h2.planPR(h2.prEvent("opened", head2, base2))
	task2 := h2.tasks(req2)[0]
	h2.devWorkItem(aiPR, agentcfg.AgentClaudeCode)
	if got := h2.mismatch([]*ai.Task{task2}); len(got) != 0 {
		t.Fatalf("开发用的是另一个 Agent，应可以审计：%v", got)
	}

	// 都被用过时规划仍然选第一个配置的 Agent（任务等待另一个审计 Agent，见 reviewAgent）
	h2.exec(`UPDATE dev_work_items SET dev_agents=ARRAY['codex','claude-code']`)
	if got := h2.mismatch([]*ai.Task{task2}); !strings.Contains(got[task2.ID], "与开发 Agent 相同") {
		t.Fatalf("都被用过：%v", got)
	}

	// 按正式分支关联：Server 还没有记下 PR 编号时，来源分支 kiln/dev-<id> 就是这个工作项的 PR
	h3 := newHarness(t)
	base3 := h3.enableReview(reviewConfig)
	branchItem := h3.devWorkItem(0, agentcfg.AgentCodex)
	branch := devreport.WorkItemBranch(branchItem, devreport.PhaseImplement, 1)
	head3 := h3.commitOn(branch, map[string]string{"f.go": "package f\n"}, "AI 开发")
	ev := h3.prEvent("opened", head3, base3)
	ev.HeadRef = branch
	task3 := h3.tasks(h3.planPR(ev))[0]
	if got := h3.mismatch([]*ai.Task{task3}); !strings.Contains(got[task3.ID], "与开发 Agent 相同") {
		t.Fatalf("按分支关联：%v", got)
	}
	// 手动审计（没有 PR）不限制
	id, _, err := h3.rev.Trigger(h3.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", Actor: "bob", Source: store.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	if got := h3.mismatch(h3.tasks(id)); len(got) != 0 {
		t.Fatalf("手动审计不应限制：%v", got)
	}
}

// TestTypeSpec：任务的审计内容按冻结的 spec 下发；服务、并发上限、协议取自 spec 和 Agent。
func TestTypeSpec(t *testing.T) {
	h := newHarness(t)
	_, task := h.prReviewed(reviewConfig + "  max_concurrency: 3\n")
	if got := h.typ.Limit(task); got != 3 {
		t.Errorf("Limit = %d，应为 3", got)
	}
	if got := h.typ.Name(); got != agentcfg.TaskReview {
		t.Errorf("Name = %s", got)
	}
	if got := h.typ.Role(); got != store.RunnerRoleReview {
		t.Errorf("Role = %s", got)
	}
	if p := h.typ.Policy(); !p.CheckRuns || !p.Rerunnable || p.GroupMode != ai.GroupSupersede {
		t.Errorf("Policy = %+v", p)
	}
	bad := *task
	bad.Spec = []byte("{")
	if h.typ.Limit(&bad) != 0 || h.typ.Services(&bad) != nil {
		t.Error("spec 无法解析时不应有并发上限和服务")
	}
	if err := h.typ.Spec(context.Background(), h.db, ai.SpecInput{Task: &bad}, nil); err == nil {
		t.Error("spec 无法解析时 Spec 应返回错误")
	}

	// 工具链按平台冻结（PLAN #33.12）：下发给哪个平台，prompt 就写哪个平台；任务中没有这个平台的工具链时不下发
	var s aispec.ReviewSpec
	if err := task.DecodeSpec(&s); err != nil {
		t.Fatal(err)
	}
	s.PlatformTools = map[string][]aispec.ToolRef{"darwin/arm64": {{Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: "sha256:aa"}}}
	frozen := *task
	frozen.Spec = mustJSON(t, &s)
	in := ai.SpecInput{Task: &frozen, Request: &ai.Request{SHA: "abc"}, Repo: &store.Repo{}, Platform: "darwin/arm64"}
	var out airunnerv2.AITask
	if err := h.typ.Spec(context.Background(), h.db, in, &out); err != nil || !strings.Contains(out.Prompt, "执行平台：darwin/arm64") {
		t.Errorf("Spec = %v，prompt 中应写明执行平台", err)
	}
	if got := h.typ.Tools(&frozen, "darwin/arm64"); len(got) != 1 || got[0].Platform != "darwin/arm64" {
		t.Errorf("Tools(darwin/arm64) = %v", got)
	}
	in.Platform = "linux/amd64"
	var se *ai.SpecError
	if err := h.typ.Spec(context.Background(), h.db, in, &out); !errors.As(err, &se) || !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("没有 linux/amd64 的工具链时 Spec = %v，应为写明平台的 SpecError", err)
	}
}

// mustJSON 把 v 编码为 JSON。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// validNull 是把空数组写成 null 的报告（旧的 kiln-ai-runner 接受的写法，现在算缺少必填字段）。
const validNull = `{"verdict":"no_issues","summary":"改动正确","scope":"1 个变更文件","findings":null,"checks":null,"unverified":null,` +
	`"env_notes":null,"preexisting":null,"commands_run":null}`

// TestValidateReport：审计报告的校验（PLAN #33.5）不改变状态：缺必填字段、字段取值不合法都给出原因，合法时没有问题；
// 其他种类的报告不属于审计任务。
func TestValidateReport(t *testing.T) {
	h := newHarness(t)
	req, task := h.prReviewed(reviewConfig)

	problems, err := h.typ.ValidateReport(h.ctx, task, 0, ai.ReportReview, []byte(`{"verdict": "no_issues"}`))
	if err != nil || len(problems) == 0 {
		t.Fatalf("缺必填字段：%v，%v", problems, err)
	}
	problems, err = h.typ.ValidateReport(h.ctx, task, 0, ai.ReportReview, []byte(`{"verdict": "unknown"}`))
	if err != nil || len(problems) == 0 {
		t.Fatalf("不合法的取值：%v，%v", problems, err)
	}
	if _, err := h.typ.ValidateReport(h.ctx, task, 0, ai.ReportSelection, []byte("{}")); !errors.Is(err, ai.ErrUnknownReportKind) {
		t.Fatalf("选题报告不属于审计任务：%v", err)
	}
	// Agent 按 Schema 写：没有内容时写空数组（不是 null），没有 version（由 Kiln 写入）
	valid := `{"verdict":"no_issues","summary":"改动正确","scope":"1 个变更文件","findings":[],"checks":[],"unverified":[],` +
		`"env_notes":[],"preexisting":[],"commands_run":[]}`
	if problems, err := h.typ.ValidateReport(h.ctx, task, 0, ai.ReportReview, []byte(valid)); err != nil || len(problems) != 0 {
		t.Fatalf("合法的报告：%v，%v", problems, err)
	}
	// 空数组写成 null 也算缺少必填字段
	if problems, err := h.typ.ValidateReport(h.ctx, task, 0, ai.ReportReview, []byte(validNull)); err != nil || len(problems) == 0 {
		t.Fatalf("null 数组：%v，%v", problems, err)
	}
	// 只读：请求还在排队、任务没有结果
	if r := h.request(req); r.State != ai.RequestActive {
		t.Errorf("校验报告不应改变状态：%s", r.State)
	}
	if cur, err := ai.GetTask(h.ctx, h.db, task.ID, false); err != nil || cur.State != ai.TaskPending || cur.CurrentAttemptID != nil {
		t.Errorf("校验报告不应改变任务：%+v，%v", cur, err)
	}
}

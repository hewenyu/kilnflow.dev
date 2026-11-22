package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
)

// Agent 用 kiln-orca task ci 查询任务所在 PR（或指定 commit）最新的 CI 验证（PLAN #33.9）：状态、失败的 job、错误分类、
// 日志尾部和 Dashboard 地址；只读，不改 CI 的状态。
func TestAIPort_getVerification(t *testing.T) {
	p := newSplitPort(t, portConfig)
	req, _ := planned(t, p, 7)
	// PR 的当前 head：任务没有指定 sha 时查它（ref_heads 中记下的 pull ref）
	head := p.reqSHA(req)
	p.exec(`INSERT INTO ref_heads (repo_id, ref, sha) VALUES ($1, $2, $3)`, portRepoID, fmt.Sprintf("%s%d/head", gitref.PullPrefix, 7), head)
	// head 上有一个失败的 CI run：失败的 job 带错误分类和日志尾部
	var runID int64
	mustNil(t, p.s.db.QueryRow(p.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state)
		VALUES ($1,'ci.yml','CI','pull_request',$2,$3,'webhook','verify-1',7,'failed') RETURNING id`,
		portRepoID, fmt.Sprintf("%s%d/head", gitref.PullPrefix, 7), head).Scan(&runID))
	var jobID int64
	mustNil(t, p.s.db.QueryRow(p.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state, reason)
		VALUES ('normal',$1,0,'test','test','test','{}','linux/amd64','host','failed','断言失败') RETURNING id`, runID).Scan(&jobID))
	var attemptID int64
	mustNil(t, p.s.db.QueryRow(p.ctx, `INSERT INTO attempts (job_id, epoch, runner_name, acquire_request_id, error_class)
		VALUES ($1,1,'ci-box','verify','step') RETURNING id`, jobID).Scan(&attemptID))
	p.exec(`UPDATE jobs SET current_attempt_id=$2 WHERE id=$1`, jobID, attemptID)
	if _, err := p.s.logs.Append(attemptID, 0, []byte("line 1\n\x1b[31mboom\x1b[0m\n")); err != nil {
		t.Fatal(err)
	}

	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	// 默认查任务的 PR 当前的 head
	resp, err := p.ai.GetVerification(p.ctx, withToken(r.token, &airunnerv2.GetVerificationRequest{Ref: ref(task)}))
	if err != nil {
		t.Fatalf("查询验证：%v", err)
	}
	if resp.Msg.GetSha() != head || resp.Msg.GetState() != "failed" || len(resp.Msg.GetFailedJobs()) != 1 {
		t.Fatalf("验证 = %+v", resp.Msg)
	}
	f := resp.Msg.GetFailedJobs()[0]
	want := fmt.Sprintf("%s/jobs/%d", p.s.cfg.PublicURL, jobID)
	if f.GetName() != "test" || f.GetReason() != "断言失败" || f.GetErrorClass() != "step" || f.GetLogExpired() ||
		!strings.Contains(string(f.GetLogTail()), "boom") || strings.Contains(string(f.GetLogTail()), "\x1b") || f.GetUrl() != want {
		t.Fatalf("失败的 job = %+v", f)
	}
	// 只读：CI 的 run 和 job 不变
	if st := p.count(`SELECT count(*) FROM runs WHERE repo_id=$1 AND state='failed'`, portRepoID); st != 1 {
		t.Fatalf("run 的状态被改了（%d）", st)
	}

	// 指定的 sha 没有验证过：none
	resp, err = p.ai.GetVerification(p.ctx, withToken(r.token, &airunnerv2.GetVerificationRequest{Ref: ref(task), Sha: "nope"}))
	if err != nil || resp.Msg.GetState() != "none" || resp.Msg.GetSha() != "nope" {
		t.Fatalf("没有验证过的 commit：%+v，%v", resp.Msg, err)
	}

	// 过期的 attempt 不能查询（任务已经结束）
	if _, err := p.complete(r, task, succeeded(okReport)); err != nil {
		t.Fatalf("提交：%v", err)
	}
	_, err = p.ai.GetVerification(p.ctx, withToken(r.token, &airunnerv2.GetVerificationRequest{Ref: ref(task)}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("任务结束后查询：%v，应为 FAILED_PRECONDITION", err)
	}
}

// Agent 校验报告（ValidateReport）：审计报告按 Schema 校验，只有任务的种类能校验，且不改变状态（PLAN #33.5）。
func TestAIPort_validateReport(t *testing.T) {
	p := newSplitPort(t, portConfig)
	req, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	valid := []byte(`{"verdict":"no_issues","summary":"改动正确","scope":"1 个变更文件","findings":[],"checks":[],"unverified":[],` +
		`"env_notes":[],"preexisting":[],"commands_run":[]}`)

	resp, err := p.ai.ValidateReport(p.ctx, withToken(r.token, &airunnerv2.ValidateReportRequest{
		Ref: ref(task), Kind: airunnerv2.ReportKind_REPORT_KIND_REVIEW, Report: valid,
	}))
	if err != nil || !resp.Msg.GetValid() || len(resp.Msg.GetProblems()) != 0 {
		t.Fatalf("合法的报告：%+v，%v", resp.Msg, err)
	}
	resp, err = p.ai.ValidateReport(p.ctx, withToken(r.token, &airunnerv2.ValidateReportRequest{
		Ref: ref(task), Kind: airunnerv2.ReportKind_REPORT_KIND_REVIEW, Report: []byte(`{"verdict":"no_issues"}`),
	}))
	if err != nil || resp.Msg.GetValid() || len(resp.Msg.GetProblems()) == 0 {
		t.Fatalf("缺字段的报告：%+v，%v", resp.Msg, err)
	}
	// 交付报告不属于审计任务
	_, err = p.ai.ValidateReport(p.ctx, withToken(r.token, &airunnerv2.ValidateReportRequest{
		Ref: ref(task), Kind: airunnerv2.ReportKind_REPORT_KIND_DELIVERY, Report: valid,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("交付报告：%v，应为 InvalidArgument", err)
	}
	// 只读：任务还在执行中，没有结果
	if v := p.task(id); v.state != string(ai.TaskRunning) {
		t.Fatalf("任务 = %s", v.state)
	}
	if st := p.reqState(req); st != "active" {
		t.Fatalf("请求 = %s", st)
	}
	if n := p.count(`SELECT count(*) FROM ai_attempts WHERE task_id=$1 AND outcome IS NOT NULL`, id); n != 0 {
		t.Fatalf("校验报告不应写结果（%d 个 attempt 有结果）", n)
	}
}

// 下发的任务带 Server 生成的 prompt 和它的摘要、Agent 和需要的测试服务（PLAN #33.5、#33.6）：连接器只替换占位符。
func TestAIPort_taskPrompt(t *testing.T) {
	// 仓库声明需要标准测试服务 pg
	p := newSplitPort(t, strings.Replace(portConfig, "version: 1\n", "version: 1\nrequirements:\n  services: [pg]\n", 1))
	_, _ = planned(t, p, 7)
	r := p.runner(runnerSpec{})
	resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: p.state(r), RequestId: "prompt"}))
	if err != nil {
		t.Fatalf("领取：%v", err)
	}
	at := resp.Msg.GetTask()
	if at == nil {
		t.Fatal("没有领到任务")
	}
	if at.GetAgent() != agentcfg.AgentCodex {
		t.Errorf("Agent = %q", at.GetAgent())
	}
	if !slices.Equal(at.GetServices(), []string{"pg"}) {
		t.Errorf("任务的服务 = %v，应为 pg", at.GetServices())
	}
	prompt := at.GetPrompt()
	if !strings.Contains(prompt, "{{KILN_TASK_DIR}}/report.json") || !strings.Contains(prompt, "{{KILN_TOOL}} check-report") ||
		!strings.Contains(prompt, "{{KILN_SERVICES}}") {
		t.Errorf("prompt 中没有占位符：%q", prompt)
	}
	sum := sha256.Sum256([]byte(prompt))
	if at.GetPromptDigest() != hex.EncodeToString(sum[:]) {
		t.Errorf("prompt_digest = %q", at.GetPromptDigest())
	}
}

// 节点的并发上限（PLAN #33.8）：running 达到 capacity 就不再派任务；capacity 为 2 时可以同时执行两个任务。节点上报的
// 执行中数量为 0，只有 Server 的账本到了上限：队列里还有能领的任务，它照样留在队列中。
func TestAIPort_capacity(t *testing.T) {
	p := newSplitPort(t, portConfig)
	ids := make([]int64, 0, 3)
	for pr := 7; pr <= 9; pr++ {
		_, id := planned(t, p, pr)
		ids = append(ids, id)
	}
	r := p.runner(runnerSpec{})
	r.capacity = 2
	first := mustAcquire(t, p, r)
	second := mustAcquire(t, p, r)
	if first.id == second.id {
		t.Fatalf("两个任务相同：#%d", first.id)
	}
	// 账本里已经有两个执行中的任务：还有能领的第三个，也领不到
	wantNone(t, p, r)
	for _, id := range ids {
		if id != first.id && id != second.id {
			wantTask(t, p, id, "pending")
		}
	}
	var attempts int
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT count(*) FROM ai_attempts WHERE task_id = ANY($1)`, ids).Scan(&attempts))
	if attempts != 2 {
		t.Fatalf("attempt 有 %d 个，应为 2（达到上限后不再建 attempt）", attempts)
	}
	// 一个结束后槽位空出来：领到第三个
	if _, err := p.complete(r, first, succeeded(okReport)); err != nil {
		t.Fatal(err)
	}
	if third := mustAcquire(t, p, r); third.id == first.id || third.id == second.id {
		t.Fatalf("领到 #%d，应为第三个任务", third.id)
	}
}

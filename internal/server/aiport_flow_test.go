package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 生命周期的端口测试（PLAN #30.10 中要在 AI 侧重新保证、现在没有 AI 专用测试的条目）。

// report 返回 Agent 写出的审计报告（PLAN #33.5）：必填字段都写出，没有内容的数组写 []，version 由 Kiln 写入。
func report(verdict string, findings ...aireport.Finding) string {
	r := &aireport.Report{
		Verdict: verdict, Summary: "审计了改动", Scope: "1 个变更文件", Findings: findings,
		Checks: []aireport.Check{}, Unverified: []string{}, EnvNotes: []string{}, Preexisting: []aireport.Preexisting{},
		CommandsRun: []aireport.Command{},
	}
	if r.Findings == nil {
		r.Findings = []aireport.Finding{}
	}
	// 每个 finding 的必填字段都要写出（Agent 报告的规则，PLAN #33.5）：测试里没有写的用占位内容补齐
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Scenario, f.Actual = orText(f.Scenario, "输入 1 → 触发"), orText(f.Actual, "实际行为")
		f.Expected, f.Impact = orText(f.Expected, "预期行为"), orText(f.Impact, "影响")
		f.Proof = orText(f.Proof, "调用链 a → b")
	}
	return string(r.Marshal())
}

var okReport = report(aireport.VerdictNoIssues)

// orText 返回 s，为空时返回 def。
func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func succeeded(rep string) portResult {
	return portResult{outcome: string(store.OutcomeSucceeded), report: rep}
}

// mustAcquire 领取一个任务（没有领到时失败）。
func mustAcquire(t *testing.T, p aiPort, r *portRunner) *portTask {
	t.Helper()
	task, err := p.acquire(r, fmt.Sprintf("req-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("领取：%v", err)
	}
	if task == nil {
		t.Fatal("没有领到任务")
	}
	return task
}

// wantNone 确认领不到任务。
func wantNone(t *testing.T, p aiPort, r *portRunner) {
	t.Helper()
	task, err := p.acquire(r, fmt.Sprintf("req-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("领取：%v", err)
	}
	if task != nil {
		t.Fatalf("不应领到任务，领到了 #%d", task.id)
	}
}

func wantTask(t *testing.T, p aiPort, id int64, state string) taskView {
	t.Helper()
	v := p.task(id)
	if v.state != state {
		t.Fatalf("任务 #%d 的状态 = %s（%s），应为 %s", id, v.state, v.reason, state)
	}
	return v
}

// planned 规划 PR pr 的一次审计，返回请求和它唯一的任务。
func planned(t *testing.T, p aiPort, pr int) (aiReq, int64) {
	t.Helper()
	req := p.planReview(pr, envOf(t, p).prHead(pr))
	ids := p.tasks(req)
	if len(ids) != 1 {
		t.Fatalf("请求有 %d 个任务，应为 1", len(ids))
	}
	return req, ids[0]
}

// 领取即 running：同一事务中 epoch+1、签发租约、建 attempt；同一个 request_id 重放返回同一个任务；
// 一台 AI 执行节点一次只执行一个（Server 的账本）。
func TestAIPort_acquireClaims(t *testing.T) {
	p := newPort(t, portConfig)
	_, id := planned(t, p, 7)
	wantTask(t, p, id, "pending")
	r := p.runner(runnerSpec{})
	got, err := p.acquire(r, "same")
	if err != nil || got == nil {
		t.Fatalf("领取：%v %v", got, err)
	}
	if got.id != id || got.epoch != 1 || got.agent != agentcfg.AgentCodex || got.prNumber != 7 {
		t.Fatalf("领到 %+v，应为任务 #%d、epoch 1、codex、PR #7", got, id)
	}
	v := wantTask(t, p, id, "running")
	if v.epoch != 1 || v.attempts != 1 {
		t.Fatalf("epoch=%d attempts=%d，应都为 1", v.epoch, v.attempts)
	}
	again, err := p.acquire(r, "same")
	if err != nil || again == nil || again.attempt != got.attempt || again.epoch != got.epoch {
		t.Fatalf("同一个 request_id 重放：%+v %v，应返回同一个 attempt %d", again, err, got.attempt)
	}
	planned(t, p, 8)
	wantNone(t, p, r)
}

// Runner 上报还有执行中的任务时不领取，即使 Server 的账本为 0。
func TestAIPort_runnerReportedRunning(t *testing.T) {
	p := newPort(t, portConfig)
	planned(t, p, 7)
	r := p.runner(runnerSpec{})
	r.running = 1
	wantNone(t, p, r)
	r.running = 0
	mustAcquire(t, p, r)
}

// 领取时持 Runner 行锁复查：暂停接单的不领取，吊销的被拒绝。
func TestAIPort_acquireDrainRevoke(t *testing.T) {
	p := newPort(t, portConfig)
	e := envOf(t, p)
	planned(t, p, 7)
	r := p.runner(runnerSpec{})
	e.exec(`UPDATE runners SET drain=true WHERE id=$1`, r.id)
	wantNone(t, p, r)
	e.exec(`UPDATE runners SET drain=false, state='revoked' WHERE id=$1`, r.id)
	if _, err := p.acquire(r, "x"); !rpcerr.IsRunnerRevoked(err) {
		t.Fatalf("吊销后领取：%v，应为 RUNNER_REVOKED", err)
	}
}

// spec 生成失败只让这个任务失败，不回滚整次领取：后面的任务照常领到。
func TestAIPort_badSpecFailsOnlyThatTask(t *testing.T) {
	p := newPort(t, portConfig)
	_, bad := planned(t, p, 7)
	_, good := planned(t, p, 8)
	p.breakSpec(bad)
	r := p.runner(runnerSpec{})
	got, err := p.acquire(r, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil { // 这一次领取只让坏的任务失败（没有回滚），下一次领到后面的
		got = mustAcquire(t, p, r)
	}
	if got.id != good {
		t.Fatalf("领到 #%d，应为 #%d", got.id, good)
	}
	wantTask(t, p, bad, "failed")
}

// attempt 级 RPC 校验 epoch；结果至多提交一次：相同内容的重试返回 replayed，内容不同返回 ALREADY_EXISTS（PLAN #33.6）。
func TestAIPort_staleEpoch(t *testing.T) {
	p := newPort(t, portConfig)
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	stale := *task
	stale.epoch++
	if _, err := p.heartbeat(r, &stale); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("错误的 epoch 心跳：%v", err)
	}
	if _, err := p.complete(r, &stale, succeeded(okReport)); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("错误的 epoch 提交：%v", err)
	}
	// 别的 Runner 不能替它提交
	if _, err := p.complete(p.runner(runnerSpec{}), task, succeeded(okReport)); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的 Runner 提交：%v", err)
	}
	if _, err := p.complete(r, task, succeeded(okReport)); err != nil {
		t.Fatalf("提交：%v", err)
	}
	wantTask(t, p, id, "succeeded")
	// 相同内容的重试：Server 返回回执，不再处理（不是 STALE_EPOCH，调用方知道它已经被接受）
	replayed, err := p.complete(r, task, succeeded(okReport))
	if err != nil || !replayed {
		t.Fatalf("相同内容的重试：replayed = %v，err = %v", replayed, err)
	}
	// 内容不同：这次执行已经有结果了
	if _, err := p.complete(r, task, succeeded(report(aireport.VerdictIssuesFound, aireport.Finding{
		Title: "x", Severity: "high", Evidence: "reproduced", Path: "a.go", Line: 1, Body: "x",
	}))); !rpcerr.IsAlreadyExists(err) {
		t.Fatalf("不同的结果：%v，应为 ALREADY_EXISTS", err)
	}
	wantTask(t, p, id, "succeeded")
}

// 任务类型按业务结论把成功的提交改判为失败（审计发现问题、未完成、报告无效，开发的交付报告无效）时，提交本身的摘要和
// 最终的 Orca 关联照样保存：相同内容的重试返回 replayed，内容不同仍是 ALREADY_EXISTS（PLAN #33.6）。
func TestAIPort_completeReplayAfterVerdict(t *testing.T) {
	finding := aireport.Finding{Title: "空指针", Severity: "high", Evidence: "reproduced", Path: "a.go", Line: 3, Body: "x"}
	review := func(rep string) func(aiPort) (*portRunner, portResult) {
		return func(p aiPort) (*portRunner, portResult) {
			planned(t, p, 7)
			return p.runner(runnerSpec{}), succeeded(rep)
		}
	}
	cases := []struct {
		name  string
		class string // 改判失败的错误分类
		setup func(aiPort) (*portRunner, portResult)
	}{
		{"审计发现问题", "ai.findings", review(report(aireport.VerdictIssuesFound, finding))},
		{"审计未完成", "ai.incomplete", review(report(aireport.VerdictIncomplete))},
		{"审计报告无效", "ai.report", review(`{"verdict":"maybe"}`)},
		{"开发交付报告无效", "ai.delivery", func(p aiPort) (*portRunner, portResult) {
			p.planFix(p.addWorkItem(9, "kiln/dev-x"), 1)
			return p.runner(runnerSpec{role: store.RunnerRoleDevelopment}),
				portResult{outcome: string(store.OutcomeSucceeded), delivery: `{"status":"delivered"}`}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPort(t, portConfig)
			r, res := c.setup(p)
			task := mustAcquire(t, p, r)
			if replayed, err := p.complete(r, task, res); err != nil || replayed {
				t.Fatalf("提交：replayed = %v，err = %v", replayed, err)
			}
			wantTask(t, p, task.id, "failed")
			e := envOf(t, p)
			var class, digest, dispatch *string
			mustNil(t, e.s.db.QueryRow(e.ctx, `SELECT error_class, result_digest, orca->>'dispatch_id' FROM ai_attempts WHERE id=$1`,
				task.attempt).Scan(&class, &digest, &dispatch))
			if class == nil || *class != c.class {
				t.Fatalf("错误分类 = %v，应为 %s", class, c.class)
			}
			if digest == nil || dispatch == nil || *dispatch != portDispatchID {
				t.Fatalf("attempt 的摘要 = %v，dispatch = %v：没有保存提交的摘要和最终的 Orca 关联", digest, dispatch)
			}
			if replayed, err := p.complete(r, task, res); err != nil || !replayed {
				t.Fatalf("相同内容的重试：replayed = %v，err = %v", replayed, err)
			}
			res.message = "不同的说明"
			if _, err := p.complete(r, task, res); !rpcerr.IsAlreadyExists(err) {
				t.Fatalf("不同的结果：%v，应为 ALREADY_EXISTS", err)
			}
			wantTask(t, p, task.id, "failed")
		})
	}
}

// 心跳续租并下发取消；Runner 确认取消后任务和请求都为 cancelled。
func TestAIPort_heartbeatCancel(t *testing.T) {
	p := newPort(t, portConfig)
	req, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	if c, err := p.heartbeat(r, task); err != nil || c {
		t.Fatalf("心跳：cancel=%v err=%v", c, err)
	}
	p.cancel(req)
	wantTask(t, p, id, "running")
	if c, err := p.heartbeat(r, task); err != nil || !c {
		t.Fatalf("取消后心跳：cancel=%v err=%v，应下发取消", c, err)
	}
	if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeCancelled)}); err != nil {
		t.Fatal(err)
	}
	wantTask(t, p, id, "cancelled")
	if st := p.reqState(req); st != "cancelled" {
		t.Fatalf("请求 = %s，应为 cancelled", st)
	}
}

// 排队中的任务取消时立即结束。
func TestAIPort_cancelPending(t *testing.T) {
	p := newPort(t, portConfig)
	req, id := planned(t, p, 7)
	p.cancel(req)
	wantTask(t, p, id, "cancelled")
	if st := p.reqState(req); st != "cancelled" {
		t.Fatalf("请求 = %s，应为 cancelled", st)
	}
}

// 已请求取消时 Runner 照常完成：保留结果并照常发布。
func TestAIPort_cancelledButSucceeded(t *testing.T) {
	p := newPort(t, portConfig)
	req, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	p.cancel(req)
	if _, err := p.complete(r, task, succeeded(okReport)); err != nil {
		t.Fatal(err)
	}
	wantTask(t, p, id, "succeeded")
	e := envOf(t, p)
	if n := e.outboxCount(prReviewKind(p)); n != 1 {
		t.Fatalf("PR review 的发布记录 = %d，应为 1", n)
	}
}

// 租约过期记 infra.lost；失联的 attempt 再传日志返回 STALE_EPOCH；审计不再自动重试（PLAN #30.13）。
func TestAIPort_leaseExpiry(t *testing.T) {
	p := newPort(t, portConfig)
	req, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	if err := p.appendLog(r, task, 0, "hello\n"); err != nil {
		t.Fatalf("日志：%v", err)
	}
	p.expireLease(task)
	v := p.task(id)
	if v.errClass != "infra.lost" {
		t.Fatalf("error_class = %q，应为 infra.lost", v.errClass)
	}
	if err := p.appendLog(r, task, 6, "more\n"); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("失联后传日志：%v，应为 STALE_EPOCH", err)
	}
	if !p.newRules() {
		return
	}
	wantTask(t, p, id, "failed")
	if st := p.reqState(req); st != "failed" {
		t.Fatalf("请求 = %s，应为 failed", st)
	}
	// 失联后这次执行已经有结果（infra.lost）：再提交也不被接受，调用方知道不用重试
	if _, err := p.complete(r, task, succeeded(okReport)); !rpcerr.IsAlreadyExists(err) {
		t.Fatalf("失联后提交：%v，应为 ALREADY_EXISTS", err)
	}
}

// 审计的基础设施错误不重试（PLAN #30.13）；已请求取消时记为 cancelled。
func TestAIPort_reviewInfraError(t *testing.T) {
	p := newPort(t, portConfig)
	if !p.newRules() {
		t.Skip("旧规则：审计按 CI 的规则重试")
	}
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	if _, err := p.complete(r, mustAcquire(t, p, r), portResult{outcome: string(store.OutcomeInfraError), errClass: "infra.agent", message: "codex 退出"}); err != nil {
		t.Fatal(err)
	}
	v := wantTask(t, p, id, "failed")
	if !strings.Contains(v.reason, "infra.agent") {
		t.Errorf("原因 = %q，应说明错误类别", v.reason)
	}
	req2, id2 := planned(t, p, 8)
	task := mustAcquire(t, p, r)
	p.cancel(req2)
	if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeInfraError), errClass: "infra.agent"}); err != nil {
		t.Fatal(err)
	}
	wantTask(t, p, id2, "cancelled")
}

// Server 判超时 = started_at + timeout + cancel_grace + lease_ttl；重启后 2 × lease_ttl 内不判。
func TestAIPort_timeout(t *testing.T) {
	p := newPort(t, portConfig)
	_, id := planned(t, p, 7)
	task := mustAcquire(t, p, p.runner(runnerSpec{}))
	limit := agentcfg.DefaultTimeout + portGrace + portLease
	p.age(task, limit-time.Minute)
	p.sweep()
	wantTask(t, p, id, "running")
	p.age(task, 2*time.Minute)
	p.restart()
	p.sweep()
	wantTask(t, p, id, "running")
}

func TestAIPort_timeoutAfterLimit(t *testing.T) {
	p := newPort(t, portConfig)
	_, id := planned(t, p, 7)
	task := mustAcquire(t, p, p.runner(runnerSpec{}))
	p.age(task, agentcfg.DefaultTimeout+portGrace+portLease+time.Minute)
	p.sweep()
	wantTask(t, p, id, "timed_out")
}

// AI 开发的 12 小时上限在 Server 端只靠超时判定。
func TestAIPort_developTimeout(t *testing.T) {
	p := newPort(t, portConfig)
	wi := p.addWorkItem(9, "kiln/dev-x")
	req := p.planFix(wi, 1)
	ids := p.tasks(req)
	task := mustAcquire(t, p, p.runner(runnerSpec{role: store.RunnerRoleDevelopment}))
	if task.id != ids[0] {
		t.Fatalf("领到 #%d，应为 #%d", task.id, ids[0])
	}
	p.age(task, 11*time.Hour)
	p.sweep()
	wantTask(t, p, task.id, "running")
	p.age(task, time.Hour+portGrace+portLease+time.Minute)
	p.sweep()
	wantTask(t, p, task.id, "timed_out")
}

// AI 开发遇到基础设施错误或失联不重新排队，直接失败；失联后执行者把工作项转为受阻。
func TestAIPort_developInfraNoRetry(t *testing.T) {
	p := newPort(t, portConfig)
	wi := p.addWorkItem(9, "kiln/dev-x")
	p.planFix(wi, 1)
	dev := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
	task := mustAcquire(t, p, dev)
	if _, err := p.complete(dev, task, portResult{outcome: string(store.OutcomeInfraError), errClass: "infra.agent", message: "x"}); err != nil {
		t.Fatal(err)
	}
	wantTask(t, p, task.id, "failed")

	wi2 := p.addWorkItem(10, "kiln/dev-y")
	p.planFix(wi2, 1)
	task = mustAcquire(t, p, dev)
	p.expireLease(task)
	wantTask(t, p, task.id, "failed")
	p.settle()
	p.processAIDev()
	e := envOf(t, p)
	var state string
	mustNil(t, e.s.db.QueryRow(e.ctx, `SELECT state FROM dev_work_items WHERE id=$1`, wi2).Scan(&state))
	if state != "blocked" {
		t.Fatalf("失联后工作项 = %s，应为 blocked", state)
	}
}

// 审计结论到任务状态的映射；报告保存后写入发布 PR review 的记录。
func TestAIPort_reviewVerdicts(t *testing.T) {
	finding := aireport.Finding{Title: "空指针", Severity: "high", Evidence: "reproduced", Path: "a.go", Line: 3, Body: "x"}
	cases := []struct {
		name, report, state, class string
		published                  bool
	}{
		{"未发现问题", okReport, "succeeded", "", true},
		{"发现问题", report(aireport.VerdictIssuesFound, finding), "failed", "ai.findings", true},
		{"未完成", report(aireport.VerdictIncomplete), "failed", "ai.incomplete", true},
		{"报告无效", `{"verdict":"ok"}`, "failed", "ai.report", false},
	}
	p := newPort(t, portConfig)
	e := envOf(t, p)
	r := p.runner(runnerSpec{})
	for i, c := range cases {
		_, id := planned(t, p, 20+i)
		before := e.outboxCount(prReviewKind(p))
		if _, err := p.complete(r, mustAcquire(t, p, r), succeeded(c.report)); err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		v := p.task(id)
		if v.state != c.state || v.errClass != c.class {
			t.Errorf("%s：%s / %q，应为 %s / %q", c.name, v.state, v.errClass, c.state, c.class)
		}
		if got := e.outboxCount(prReviewKind(p)) - before; (got == 1) != c.published {
			t.Errorf("%s：新增 PR review 记录 %d", c.name, got)
		}
	}
}

// 去重：同一个 head 只建一个请求；同一个 PR 的新 head 取代旧的审计（运行中的请求取消，排队中的直接结束）。
func TestAIPort_dedupeAndSupersede(t *testing.T) {
	p := newPort(t, portConfig)
	e := envOf(t, p)
	head := e.prHead(7)
	first := p.planReview(7, head)
	if again := p.planReview(7, head); again != first {
		t.Fatalf("同一个 head 规划了两次：%v %v", first, again)
	}
	r := p.runner(runnerSpec{})
	task := mustAcquire(t, p, r)
	second := p.planReview(7, e.prHead(7))
	if c, err := p.heartbeat(r, task); err != nil || !c {
		t.Fatalf("被取代的审计心跳：cancel=%v %v", c, err)
	}
	third := p.planReview(7, e.prHead(7))
	wantTask(t, p, p.tasks(second)[0], "cancelled")
	wantTask(t, p, p.tasks(third)[0], "pending")
}

// 重跑旧 head 不取消当前 head 的审计（PLAN #30.6）。
func TestAIPort_rerunOldHeadKeepsCurrent(t *testing.T) {
	p := newPort(t, portConfig)
	if !p.newRules() {
		t.Skip("旧规则：重跑加入并发组时取消组内其他运行")
	}
	e := envOf(t, p)
	r := p.runner(runnerSpec{})
	old := p.planReview(7, e.prHead(7))
	if _, err := p.complete(r, mustAcquire(t, p, r), succeeded(okReport)); err != nil {
		t.Fatal(err)
	}
	cur := p.planReview(7, e.prHead(7))
	if _, err := p.rerunReview(old, ""); err != nil {
		t.Fatal(err)
	}
	wantTask(t, p, p.tasks(cur)[0], "pending")
}

// GitHub 触发的重跑按 delivery 至多一次：重放同一个 delivery（部分成功后整体重试、重跑结束之后再重放）都返回那次的请求，
// 新的 delivery 可以再次重跑。
func TestAIPort_rerunDelivery(t *testing.T) {
	p := newPort(t, portConfig)
	r := p.runner(runnerSpec{})
	req, _ := planned(t, p, 7)
	if _, err := p.complete(r, mustAcquire(t, p, r), succeeded(report(aireport.VerdictIncomplete))); err != nil {
		t.Fatal(err)
	}
	first, err := p.rerunReview(req, "d1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.rerunReview(req, "d1")
	if err != nil || again != first {
		t.Fatalf("重放同一个 delivery：%v %v，应返回 %v", again, err, first)
	}
	if _, err := p.complete(r, mustAcquire(t, p, r), succeeded(okReport)); err != nil {
		t.Fatal(err)
	}
	if st := p.reqState(first); st != "succeeded" {
		t.Fatalf("重跑 = %s", st)
	}
	if again, err = p.rerunReview(req, "d1"); err != nil || again != first {
		t.Fatalf("重跑结束后重放同一个 delivery：%v %v，应返回 %v", again, err, first)
	}
	wantNone(t, p, r)
}

// AI 开发的请求不能重跑。
func TestAIPort_developNotRerunnable(t *testing.T) {
	p := newPort(t, portConfig)
	wi := p.addWorkItem(9, "kiln/dev-x")
	req := p.planFix(wi, 1)
	p.cancel(req)
	if _, err := p.rerunReview(req, ""); err == nil {
		t.Fatal("AI 开发的请求重跑成功了")
	}
}

// AI 开发不建 Check Run（任务级和请求级），审计建。
func TestAIPort_checkRuns(t *testing.T) {
	p := newPort(t, portConfig)
	e := envOf(t, p)
	req, _ := planned(t, p, 7)
	if got := e.githubChecks(p.reqSHA(req)); len(got) != 2 {
		t.Fatalf("审计的 Check Run = %v，应为请求级和任务级各一个", got)
	}
	wi := p.addWorkItem(9, "kiln/dev-x")
	fix := p.planFix(wi, 1)
	dev := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
	task := mustAcquire(t, p, dev)
	if _, err := p.complete(dev, task, portResult{outcome: string(store.OutcomeFailed), message: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := e.githubChecks(p.reqSHA(fix)); len(got) != 0 {
		t.Fatalf("AI 开发建了 Check Run：%v", got)
	}
}

// 任务被领取后，请求级的汇总 Check Run 和任务级的一起更新为执行中（不停留在排队）。
func TestAIPort_requestCheckFollowsClaim(t *testing.T) {
	p := newPort(t, portConfig)
	e := envOf(t, p)
	req, _ := planned(t, p, 7)
	sha := p.reqSHA(req)
	e.githubChecks(sha)
	mustAcquire(t, p, p.runner(runnerSpec{}))
	e.githubChecks(sha)
	got := map[string]string{}
	for _, c := range e.fake.CheckRuns(sha) {
		got[c.Name] = c.Status
	}
	if len(got) != 2 {
		t.Fatalf("Check Run = %v，应为请求级和任务级各一个", got)
	}
	for name, status := range got {
		if status != gh.CheckStatusInProgress {
			t.Errorf("%s = %s，领取后应为 %s", name, status, gh.CheckStatusInProgress)
		}
	}
}

// 结束钩子：PR 审计请求结束时通知合并队列、AI 开发的执行者，成功时提名自动入队；每条结束路径都有。
func TestAIPort_endHooks(t *testing.T) {
	type path struct {
		name    string
		finish  func(p aiPort, req aiReq, r *portRunner, task *portTask)
		success bool
	}
	paths := []path{
		{"Complete 成功", func(p aiPort, _ aiReq, r *portRunner, task *portTask) {
			mustComplete(t, p, r, task, succeeded(okReport))
		}, true},
		{"Complete 失败", func(p aiPort, _ aiReq, r *portRunner, task *portTask) {
			mustComplete(t, p, r, task, succeeded(report(aireport.VerdictIncomplete)))
		}, false},
		{"超时", func(p aiPort, _ aiReq, _ *portRunner, task *portTask) {
			p.age(task, agentcfg.DefaultTimeout+portGrace+portLease+time.Minute)
			p.sweep()
		}, false},
		{"取消", func(p aiPort, req aiReq, r *portRunner, task *portTask) {
			p.cancel(req)
			mustComplete(t, p, r, task, portResult{outcome: string(store.OutcomeCancelled)})
		}, false},
	}
	if p := newPort(t, portConfig); p.newRules() {
		paths = append(paths, path{"失联", func(p aiPort, _ aiReq, _ *portRunner, task *portTask) { p.expireLease(task) }, false})
	}
	for _, c := range paths {
		t.Run(c.name, func(t *testing.T) {
			p := newPort(t, portConfig)
			e := envOf(t, p)
			r := p.runner(runnerSpec{})
			// PR #7 是工作项的 PR、在合并队列中：通知合并队列和 AI 开发的执行者
			p.addWorkItem(7, prBranch(7))
			e.exec(`INSERT INTO merge_queue (repo_id, pr_number, enqueued_by) VALUES ($1, 7, 'a')`, portRepoID)
			req, _ := planned(t, p, 7)
			e.resetHooks()
			c.finish(p, req, r, mustAcquire(t, p, r))
			p.settle()
			if e.outboxCount("mergequeue.repo") == 0 {
				t.Error("没有通知合并队列")
			}
			if e.outboxCount("aidev.repo") == 0 {
				t.Error("没有通知 AI 开发的执行者")
			}
			// PR #8 不在队列中：成功时提名自动入队
			req, _ = planned(t, p, 8)
			e.resetHooks()
			c.finish(p, req, r, mustAcquire(t, p, r))
			p.settle()
			if e.nominated(8) != c.success {
				t.Errorf("提名自动入队 = %v，应为 %v", e.nominated(8), c.success)
			}
		})
	}
}

// review.max_concurrency 跨所有 AI 执行节点生效。
func TestAIPort_reviewQuota(t *testing.T) {
	p := newPort(t, strings.Replace(portConfig, "  agent: codex\n", "  agent: codex\n  max_concurrency: 1\n", 1))
	planned(t, p, 7)
	_, second := planned(t, p, 8)
	mustAcquire(t, p, p.runner(runnerSpec{}))
	wantNone(t, p, p.runner(runnerSpec{}))
	if v := p.task(second); !strings.Contains(v.skipReason, "并发上限 1") {
		t.Fatalf("等待原因 = %q", v.skipReason)
	}
}

// 指定的 Agent 不可用时不领取，不换另一个。
func TestAIPort_agentRequired(t *testing.T) {
	p := newPort(t, portConfig)
	_, id := planned(t, p, 7)
	wantNone(t, p, p.runner(runnerSpec{agents: []string{agentcfg.AgentClaudeCode}}))
	if v := p.task(id); !strings.Contains(v.skipReason, agentcfg.AgentCodex) {
		t.Fatalf("等待原因 = %q，应写明需要 codex", v.skipReason)
	}
}

package admin

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestRunnerState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	for _, c := range []struct {
		name     string
		runner   store.Runner
		online   bool
		expected string
	}{
		{"online", store.Runner{State: store.RunnerActive, LastSeen: ago(time.Second)}, true, store.RunnerActive},
		{"刚好在离线阈值上", store.Runner{State: store.RunnerActive, LastSeen: ago(RunnerOfflineAfter)}, true, store.RunnerActive},
		{"never seen", store.Runner{State: store.RunnerActive}, false, store.RunnerOffline},
		{"offline", store.Runner{State: store.RunnerActive, LastSeen: ago(RunnerOfflineAfter + time.Second)}, false, store.RunnerOffline},
		{"draining wins over offline", store.Runner{State: store.RunnerActive, Drain: true, LastSeen: ago(time.Hour)}, false, store.RunnerDraining},
		{"draining online", store.Runner{State: store.RunnerActive, Drain: true, LastSeen: ago(time.Second)}, true, store.RunnerDraining},
		{"revoked", store.Runner{State: store.RunnerRevoked, Drain: true, LastSeen: ago(time.Second)}, true, store.RunnerRevoked},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RunnerOnline(&c.runner, now); got != c.online {
				t.Errorf("RunnerOnline = %v，应为 %v", got, c.online)
			}
			if got := RunnerState(&c.runner, now); got != c.expected {
				t.Errorf("RunnerState = %q，应为 %q", got, c.expected)
			}
		})
	}
}

func TestService_ListRunners(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if list, err := e.svc.ListRunners(e.ctx); err != nil || len(list) != 0 {
		t.Fatalf("没有 Runner 时应返回空列表：%v %v", list, err)
	}
	now := time.Now()
	recent, stale := now.Add(-10*time.Second), now.Add(-time.Hour)
	busy := e.addRunner("busy", runnerOpts{lastSeen: &recent, pool: "mac"})
	e.addRunner("never", runnerOpts{})
	e.addRunner("stale", runnerOpts{lastSeen: &stale})
	e.addRunner("paused", runnerOpts{lastSeen: &recent, drain: true})
	e.addRunner("revoked", runnerOpts{lastSeen: &recent, state: store.RunnerRevoked})

	// busy 正在执行两个 job；另一个 job 的当前尝试已经结束（不计入）
	run := e.addRun(runOpts{state: store.RunActive})
	for _, key := range []string{"a", "b"} {
		j := e.addJob(jobOpts{runID: run, key: key, state: store.JobRunning})
		e.addAttempt(j, busy, "")
	}
	done := e.addJob(jobOpts{runID: run, key: "c", state: store.JobSucceeded})
	e.addAttempt(done, busy, "succeeded")

	list, err := e.svc.ListRunners(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range list {
		got[r.Name] = r.State
	}
	want := map[string]string{
		"busy": store.RunnerActive, "never": store.RunnerOffline, "stale": store.RunnerOffline,
		"paused": store.RunnerDraining, "revoked": store.RunnerRevoked,
	}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s 的状态 = %q，应为 %q", name, got[name], state)
		}
	}
	// 按名称排序；字段完整
	b := list[0]
	if b.Name != "busy" || b.Pool != "mac" || b.RunningJobs != 2 || b.Version != "v1.0.0" || b.HostFingerprint["os"] != "linux" ||
		b.LastSeenAt != recent.Round(time.Second).Unix() || len(b.Platforms) != 1 || len(b.Executors) != 1 {
		t.Errorf("busy = %+v", b)
	}
	if list[1].Name != "never" || list[1].LastSeenAt != 0 || list[1].RunningJobs != 0 {
		t.Errorf("never = %+v", list[1])
	}
}

func TestService_RunnerOps(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addRunner("mini", runnerOpts{})

	runner := func() (state string, drain bool) {
		t.Helper()
		if err := e.db.QueryRow(e.ctx, `SELECT state, drain FROM runners WHERE name='mini'`).Scan(&state, &drain); err != nil {
			t.Fatal(err)
		}
		return state, drain
	}
	steps := []struct {
		name      string
		op        func() error
		state     string
		drain     bool
		auditName string
	}{
		{"暂停接单", func() error { return e.svc.SetRunnerDrain(e.ctx, "mini", true, testActor) }, store.RunnerActive, true, "runner.drain"},
		{"恢复接单", func() error { return e.svc.SetRunnerDrain(e.ctx, "mini", false, testActor) }, store.RunnerActive, false, "runner.undrain"},
		{"吊销", func() error { return e.svc.RevokeRunner(e.ctx, "mini", testActor) }, store.RunnerRevoked, false, "runner.revoke"},
	}
	for _, s := range steps {
		if err := s.op(); err != nil {
			t.Fatalf("%s：%v", s.name, err)
		}
		state, drain := runner()
		if state != s.state || drain != s.drain {
			t.Errorf("%s 后 state=%s drain=%v，应为 %s %v", s.name, state, drain, s.state, s.drain)
		}
		e.wantAudit(s.auditName, testActor, map[string]any{"name": "mini"})
	}

	// 不存在的 Runner：NotFound，不写审计日志
	before := len(e.audits(""))
	for name, op := range map[string]func() error{
		"drain":  func() error { return e.svc.SetRunnerDrain(e.ctx, "ghost", true, testActor) },
		"revoke": func() error { return e.svc.RevokeRunner(e.ctx, "ghost", testActor) },
	} {
		err := op()
		wantCode(t, name+" 不存在的 Runner", err, connect.CodeNotFound)
		if !strings.Contains(err.Error(), "没有 Runner ghost") {
			t.Errorf("%s：错误应说明 Runner 不存在：%v", name, err)
		}
	}
	if len(e.audits("")) != before {
		t.Errorf("操作不存在的 Runner 不应写审计日志")
	}
}

func TestService_DeleteRunner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	e.addRunner("other", runnerOpts{})
	run := e.addRun(runOpts{state: store.RunActive})
	running := e.addJob(jobOpts{runID: run, key: "a", state: store.JobRunning})
	e.addAttempt(running, mini, "")
	done := e.addJob(jobOpts{runID: run, key: "b", state: store.JobSucceeded})
	e.addAttempt(done, mini, "succeeded")
	e.exec(`INSERT INTO auth_nonces (runner_id, nonce) VALUES ($1, 'n')`, mini)
	e.exec(`INSERT INTO runner_cache_purges (runner_id) VALUES ($1)`, mini)
	e.exec(`INSERT INTO enrollment_tokens (token_hash, expires_at, used_at, runner_id) VALUES ('h', now(), now(), $1)`, mini)

	// 没有吊销的不能删除
	wantCode(t, "没有吊销", e.svc.DeleteRunner(e.ctx, "mini", testActor), connect.CodeFailedPrecondition)
	if err := e.svc.RevokeRunner(e.ctx, "mini", testActor); err != nil {
		t.Fatal(err)
	}
	// 吊销时正在执行的任务还没结束（租约没过期）时不能删除
	err := e.svc.DeleteRunner(e.ctx, "mini", testActor)
	wantCode(t, "还有未结束的任务", err, connect.CodeFailedPrecondition)
	if !strings.Contains(err.Error(), "1 个任务没有结束") {
		t.Errorf("错误应说明未结束的任务数：%v", err)
	}
	if n := len(e.audits("runner.delete")); n != 0 {
		t.Fatalf("拒绝删除时不应写审计日志，有 %d 条", n)
	}

	e.exec(`UPDATE jobs SET state=$2 WHERE id=$1`, running, store.JobFailed)
	if err := e.svc.DeleteRunner(e.ctx, "mini", testActor); err != nil {
		t.Fatal(err)
	}
	e.wantAudit("runner.delete", testActor, map[string]any{"name": "mini", "runner_id": mini, "attempts": 2})
	for what, sql := range map[string]string{
		"runners":             `SELECT count(*) FROM runners WHERE id=$1`,
		"auth_nonces":         `SELECT count(*) FROM auth_nonces WHERE runner_id=$1`,
		"runner_cache_purges": `SELECT count(*) FROM runner_cache_purges WHERE runner_id=$1`,
		"enrollment_tokens":   `SELECT count(*) FROM enrollment_tokens WHERE runner_id=$1`,
		"attempts.runner_id":  `SELECT count(*) FROM attempts WHERE runner_id=$1`,
	} {
		if n := e.count(sql, mini); n != 0 {
			t.Errorf("删除后 %s 中还有 %d 行", what, n)
		}
	}
	if n := e.count(`SELECT count(*) FROM runners WHERE name='other'`); n != 1 {
		t.Errorf("其他 Runner 不应受影响")
	}
	// 尝试保留，仍显示 Runner 的名称
	jobs := make([]*cistore.Job, 0, 2)
	for _, id := range []int64{running, done} {
		j, err := cistore.GetJob(e.ctx, e.db, id, false)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	names, err := e.svc.RunnerNames(e.ctx, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if names[running] != "mini" || names[done] != "mini" {
		t.Errorf("删除后尝试的 Runner 名称 = %v，应为 mini", names)
	}

	// 同名可以重新注册为新的 Runner；已删除的不能再删除
	if again := e.addRunner("mini", runnerOpts{}); again == mini {
		t.Errorf("重新注册应得到新的 Runner ID")
	}
	wantCode(t, "删除不存在的 Runner", e.svc.DeleteRunner(e.ctx, "ghost", testActor), connect.CodeNotFound)
}

func TestService_CreateEnrollToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, c := range []struct {
		name     string
		pool     string
		ttl      time.Duration
		wantPool string
		wantTTL  time.Duration
	}{
		{"默认 pool 和有效期", "", 0, store.DefaultPool, defaultEnrollTokenTTL},
		{"负的有效期取默认值", "mac", -time.Minute, "mac", defaultEnrollTokenTTL},
		{"指定有效期", "linux", 10 * time.Minute, "linux", 10 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			tok, exp, err := e.svc.CreateEnrollToken(e.ctx, c.pool, "", "", c.ttl, testActor)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(tok, "kiln-enroll-") {
				t.Errorf("令牌格式 = %q", tok)
			}
			if d := exp.Sub(start); d < c.wantTTL-time.Second || d > c.wantTTL+5*time.Second {
				t.Errorf("有效期 = %s，应约为 %s", d, c.wantTTL)
			}
			// 数据库中只保存哈希
			var pool string
			var used bool
			err = e.db.QueryRow(e.ctx, `SELECT pool, used_at IS NOT NULL FROM enrollment_tokens WHERE token_hash=$1`, auth.HashEnrollToken(tok)).Scan(&pool, &used)
			if err != nil {
				t.Fatalf("按哈希找不到令牌：%v", err)
			}
			if pool != c.wantPool || used {
				t.Errorf("令牌 pool=%q used=%v，应为 %q、未使用", pool, used, c.wantPool)
			}
			if n := e.count(`SELECT count(*) FROM enrollment_tokens WHERE token_hash=$1`, tok); n != 0 {
				t.Errorf("数据库中保存了令牌原文")
			}
		})
	}
	audits := e.audits("runner.token_create")
	// 没有指定 pool 时审计日志记录实际使用的 default
	if len(audits) != 3 || audits[0].Detail["pool"] != store.DefaultPool || audits[1].Detail["pool"] != "mac" || audits[0].Actor != testActor {
		t.Errorf("审计日志 = %+v", audits)
	}
	for _, a := range audits {
		if strings.Contains(string(mustJSON(t, a.Detail)), "kiln-enroll-") {
			t.Errorf("审计日志中出现了令牌")
		}
	}
}

// TestService_CreateEnrollTokenAuditFailure：审计日志写入失败时不生成令牌（令牌和审计在同一个事务中），
// 修复前令牌已经写入并返回，只记录一条告警。
func TestService_CreateEnrollTokenAuditFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.failOn("audit_log", "INSERT", "NEW.action = 'runner.token_create'")
	tok, _, err := e.svc.CreateEnrollToken(e.ctx, "", "", "", 0, testActor)
	if err == nil || tok != "" {
		t.Fatalf("审计失败时应返回错误、不返回令牌：tok=%q err=%v", tok, err)
	}
	if n := e.count(`SELECT count(*) FROM enrollment_tokens`); n != 0 {
		t.Errorf("审计失败时写入了 %d 个令牌", n)
	}
	_, _, err = e.svc.CreateEnrollToken(e.ctx, "", "gpu", "", 0, testActor)
	wantCode(t, "无效的 Runner 类型", err, connect.CodeInvalidArgument)
	_, _, err = e.svc.CreateEnrollToken(e.ctx, "", store.RunnerKindCI, store.RunnerRoleDevelopment, 0, testActor)
	wantCode(t, "CI Runner 不能有角色", err, connect.CodeInvalidArgument)
}

// TestService_DeleteRunnerAI：AI 执行节点还有执行中的 AI 任务时不能删除；删除后 AI 的 attempt 保留并记下 Runner 的名称。
func TestService_DeleteRunnerAI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rev := e.addAIRunner("reviewer", store.RunnerRoleReview, nil)
	req := e.addAIRequest(aiRequestOpts{pr: 3})
	running := e.addAITask(req, aiTaskOpts{state: ai.TaskRunning})
	att := e.addAIAttempt(running, rev, "", "")
	done := e.addAITask(req, aiTaskOpts{key: "linux-amd64", state: ai.TaskSucceeded})
	e.addAIAttempt(done, rev, "", store.OutcomeSucceeded)
	if err := e.svc.RevokeRunner(e.ctx, "reviewer", testActor); err != nil {
		t.Fatal(err)
	}
	err := e.svc.DeleteRunner(e.ctx, "reviewer", testActor)
	wantCode(t, "还有执行中的 AI 任务", err, connect.CodeFailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "1 个任务没有结束") {
		t.Errorf("错误应说明未结束的任务数：%v", err)
	}

	e.exec(`UPDATE ai_tasks SET state='failed' WHERE id=$1`, running)
	if err := e.svc.DeleteRunner(e.ctx, "reviewer", testActor); err != nil {
		t.Fatal(err)
	}
	e.wantAudit("runner.delete", testActor, map[string]any{"name": "reviewer", "runner_id": rev, "attempts": 2})
	if n := e.count(`SELECT count(*) FROM ai_attempts WHERE runner_id IS NULL AND runner_name='reviewer'`); n != 2 {
		t.Errorf("删除后记下名称的 AI attempt 有 %d 个，应为 2", n)
	}
	if n := e.count(`SELECT count(*) FROM ai_runners WHERE runner_id=$1`, rev); n != 0 {
		t.Errorf("删除后 ai_runners 中还有 %d 行", n)
	}
	d, err := e.svc.AIRequest(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a := d.Attempts[running]; a == nil || a.Runner != "reviewer" {
		t.Errorf("删除后任务的 Runner = %+v（attempt %d），应为 reviewer", a, att)
	}
}

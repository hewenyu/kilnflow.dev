package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// prRun 插入 PR pr 在 head 上的一个 pull_request run（base 为 base，当前运行为 rerun），返回 run ID。
func (e *env) prRun(pr int, head, base, file, name string, state store.RunState, rerun int) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state,
		github_ctx, current_rerun) VALUES ($1,$2,$3,'pull_request',$4,$5,'webhook',$6,$7,$8,jsonb_build_object('base_ref', $9::text),$10) RETURNING id`,
		testRepoID, file, name, fmt.Sprintf("refs/pull/%d/head", pr), head, fmt.Sprintf("test:%d", e.seq.Add(1)), pr, state, base, rerun).Scan(&id); err != nil {
		e.t.Fatalf("插入 run：%v", err)
	}
	return id
}

// prJob 插入 run 第 rerun 次运行中的一个 job，attemptClass 不为空时带一个错误分类为它的 attempt，返回 job ID。
func (e *env) prJob(runID int64, rerun int, name string, state store.JobState, reason, attemptClass string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state, reason)
		VALUES ('normal',$1,$2,$3,$3,$3,'{}','linux/amd64','host',$4,nullif($5,'')) RETURNING id`, runID, rerun, name, state, reason).Scan(&id); err != nil {
		e.t.Fatalf("插入 job：%v", err)
	}
	if attemptClass != "" {
		var aid int64
		if err := e.db.QueryRow(e.ctx, `INSERT INTO attempts (job_id, epoch, runner_id, acquire_request_id, error_class) VALUES ($1,1,$2,'req',$3) RETURNING id`,
			id, e.runnerID, attemptClass).Scan(&aid); err != nil {
			e.t.Fatalf("插入 attempt：%v", err)
		}
		e.exec(`UPDATE jobs SET current_attempt_id=$2 WHERE id=$1`, id, aid)
	}
	return id
}

// TestPRVerifications：只返回 PR head 针对 baseRef 的 pull_request run；来源是 workflow 文件名去掉扩展名，不是内建验证；
// withJobs 时带上当前这次运行的 job。
func TestPRVerifications(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const head = "h1"
	ci := e.prRun(7, head, "main", "ci.yml", "CI", store.RunSucceeded, 1)
	e.prJob(ci, 0, "old", store.JobFailed, "", "")
	e.prJob(ci, 1, "build", store.JobSucceeded, "", "")
	e.prJob(ci, 1, "test", store.JobSkipped, "", "")
	review := e.prRun(7, head, "main", "ai-review.yaml", "fake", store.RunActive, 0)
	e.prRun(7, head, "release", "ci.yml", "CI", store.RunSucceeded, 0) // 其他 base
	e.prRun(7, "h0", "main", "ci.yml", "CI", store.RunSucceeded, 0)    // 其他 head
	e.prRun(8, head, "main", "ci.yml", "CI", store.RunSucceeded, 0)    // 其他 PR
	e.exec(`INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, pr_number, state)
		VALUES ($1,'ci.yml','push','refs/heads/x',$2,'webhook','push-run',7,'succeeded')`, testRepoID, head)

	got, err := PRVerifications(e.ctx, e.db, testRepoID, 7, head, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Verification{
		{ID: ci, Name: "CI", Source: "ci", State: string(store.RunSucceeded), Jobs: []store.VerificationJob{
			{Name: "build", State: string(store.JobSucceeded)}, {Name: "test", State: string(store.JobSkipped)},
		}},
		{ID: review, Name: "fake", Source: "ai-review", State: string(store.RunActive)},
	}
	if !slices.EqualFunc(got, want, func(a, b store.Verification) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Source == b.Source && a.State == b.State && !a.Builtin && slices.Equal(a.Jobs, b.Jobs)
	}) {
		t.Fatalf("PRVerifications = %+v，期望 %+v", got, want)
	}
	got, err = PRVerifications(e.ctx, e.db, testRepoID, 7, head, "main", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Jobs != nil {
		t.Fatalf("不要求 job 时不应读取 job：%+v", got)
	}
}

// TestPRSettled：没有 run 或有 run 没结束时 done 为 false；都结束后返回各 run 当前这次运行中失败的 job 和 attempt 的错误分类。
func TestPRSettled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	settled := func(head string) (int, bool, []PRFailure) {
		t.Helper()
		runs, done, failures, err := PRSettled(e.ctx, e.db, testRepoID, 7, head)
		if err != nil {
			t.Fatal(err)
		}
		return runs, done, failures
	}
	if runs, done, _ := settled("none"); runs != 0 || done {
		t.Fatalf("没有 run：runs=%d done=%v", runs, done)
	}

	r1 := e.prRun(7, "h1", "main", "ci.yml", "CI", store.RunFailed, 1)
	e.prJob(r1, 0, "flaky", store.JobFailed, "上一次", "")
	lint := e.prJob(r1, 1, "lint", store.JobFailed, "lint 失败", "step")
	e.prJob(r1, 1, "build", store.JobSucceeded, "", "")
	r2 := e.prRun(7, "h1", "main", "e2e.yml", "E2E", store.RunActive, 0)
	if runs, done, failures := settled("h1"); runs != 2 || done || failures != nil {
		t.Fatalf("有 run 没结束：runs=%d done=%v failures=%v", runs, done, failures)
	}

	e.exec(`UPDATE runs SET state='failed' WHERE id=$1`, r2)
	infra := e.prJob(r2, 0, "infra", store.JobFailed, "", "infra.lost")
	runs, done, failures := settled("h1")
	want := []PRFailure{{JobID: lint, Name: "lint", Reason: "lint 失败", ErrClass: "step"}, {JobID: infra, Name: "infra", ErrClass: "infra.lost"}}
	if runs != 2 || !done || !slices.Equal(failures, want) {
		t.Fatalf("都结束后：runs=%d done=%v failures=%+v，期望 %+v", runs, done, failures, want)
	}
}

// TestPRSettledTimedOut：只有超时的 job 时它也是失败的 job（与 run 的结论同一条规则，store.FailedJobStates），AI 开发据此返修；
// 被连带取消、跳过的 job 不列。
func TestPRSettledTimedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.prRun(7, "h1", "main", "ci.yml", "CI", store.RunFailed, 0)
	slow := e.prJob(run, 0, "test", store.JobTimedOut, "超时", errClassTimeout)
	e.prJob(run, 0, "deploy", store.JobSkipped, "上游没有成功", "")
	e.prJob(run, 0, "matrix", store.JobCancelled, "fail-fast", "")
	runs, done, failures, err := PRSettled(e.ctx, e.db, testRepoID, 7, "h1")
	if err != nil {
		t.Fatal(err)
	}
	want := []PRFailure{{JobID: slow, Name: "test", Reason: "超时", ErrClass: errClassTimeout}}
	if runs != 1 || !done || !slices.Equal(failures, want) {
		t.Fatalf("只有超时的 job：runs=%d done=%v failures=%+v，期望 %+v", runs, done, failures, want)
	}
}

// TestPRSettledConfigError：run 因配置错误直接失败、没有 job 时也算失败并带上原因；否则 done 为真、failures 为空，
// AI 开发会把这次验证当成通过。
func TestPRSettledConfigError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.prRun(7, "h1", "main", "ci.yml", "CI", store.RunFailed, 0)
	e.exec(`UPDATE runs SET config_error=$2 WHERE id=$1`, run, "ci.yml:3:1: 错误：未知的字段 foo")
	runs, done, failures, err := PRSettled(e.ctx, e.db, testRepoID, 7, "h1")
	if err != nil {
		t.Fatal(err)
	}
	want := []PRFailure{{Name: "CI", Reason: configFailurePrefix + "ci.yml:3:1: 错误：未知的字段 foo"}}
	if runs != 1 || !done || !slices.Equal(failures, want) {
		t.Fatalf("配置错误：runs=%d done=%v failures=%+v，期望 %+v", runs, done, failures, want)
	}
	// 成功的 run 即使带着 warnings 也不算失败
	ok := e.prRun(7, "h1", "main", "e2e.yml", "E2E", store.RunSucceeded, 0)
	e.exec(`UPDATE runs SET warnings='有告警' WHERE id=$1`, ok)
	if _, done, failures, err := PRSettled(e.ctx, e.db, testRepoID, 7, "h1"); err != nil || !done || !slices.Equal(failures, want) {
		t.Fatalf("成功的 run 带来了失败：done=%v failures=%+v, %v", done, failures, err)
	}
}

// TestLatestVerification：commit 上的验证按各个验证最新的 run 汇总状态；失败时带上失败的 job、错误分类、日志尾部和 Dashboard 地址；
// 过程数据按保留策略清理后返回 log_expired（PLAN #33.9）。只读：没有验证时返回 none，不建 run。
func TestLatestVerification(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const head = "h1"
	latest := func(sha string) *Verification {
		t.Helper()
		v, err := LatestVerification(e.ctx, e.db, logs, "https://kiln.example", testRepoID, sha, 1024)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	if v := latest("none"); v.State != VerifyNone || v.SHA != "none" || len(v.FailedJobs) != 0 {
		t.Fatalf("没有验证：%+v", v)
	}

	run := e.prRun(7, head, "main", "ci.yml", "CI", store.RunFailed, 1)
	e.prJob(run, 1, "build", store.JobSucceeded, "", "")
	job := e.prJob(run, 1, "test", store.JobFailed, "断言失败", "step")
	attempt := e.queryInt(`SELECT current_attempt_id FROM jobs WHERE id=$1`, job)
	// 日志尾部：只返回最后一段，去掉终端控制字符
	if _, err := logs.Append(attempt, 0, []byte("line 1\n\x1b[31mline 2\x1b[0m\n")); err != nil {
		t.Fatal(err)
	}

	v := latest(head)
	if v.State != VerifyFailed || v.SHA != head || len(v.FailedJobs) != 1 {
		t.Fatalf("失败的验证：%+v", v)
	}
	f := v.FailedJobs[0]
	if f.Name != "test" || f.Reason != "断言失败" || f.ErrClass != "step" || f.LogExpired ||
		!strings.Contains(string(f.LogTail), "line 2") || strings.Contains(string(f.LogTail), "\x1b") ||
		f.URL != fmt.Sprintf("https://kiln.example/jobs/%d", job) {
		t.Fatalf("失败的 job：%+v", f)
	}

	// 有 run 没结束：pending（不返回 job）
	e.exec(`UPDATE runs SET state='running' WHERE id=$1`, run)
	if v := latest(head); v.State != VerifyPending || len(v.FailedJobs) != 0 {
		t.Fatalf("有 run 没结束：%+v", v)
	}

	// 全部成功：succeeded
	e.exec(`UPDATE runs SET state='succeeded' WHERE id=$1`, run)
	if v := latest(head); v.State != VerifySucceeded {
		t.Fatalf("成功：%+v", v)
	}
}

// TestLatestVerificationLogExpired：run 的过程数据按保留策略清理之后，失败 job 的日志不再返回（log_expired）。
func TestLatestVerificationLogExpired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := e.prRun(7, "h1", "main", "ci.yml", "CI", store.RunFailed, 0)
	job := e.prJob(run, 0, "test", store.JobFailed, "", "")
	e.exec(`INSERT INTO attempts (id, job_id, epoch, runner_id, acquire_request_id) VALUES (1,$1,1,$2,'req')`, job, e.runnerID)
	e.exec(`UPDATE jobs SET current_attempt_id=1 WHERE id=$1`, job)
	if _, err := logs.Append(1, 0, []byte("boom\n")); err != nil {
		t.Fatal(err)
	}
	// 清理发生在 job 创建之后（store.Run.JobPurged）
	e.exec(`UPDATE runs SET purged_at=now() + interval '1 second' WHERE id=$1`, run)
	v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, "h1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.FailedJobs) != 1 || !v.FailedJobs[0].LogExpired || len(v.FailedJobs[0].LogTail) != 0 || v.FailedJobs[0].URL != "" {
		t.Fatalf("日志过期：%+v", v.FailedJobs)
	}
}

// TestLatestVerificationTimedOut：超时的 job 与失败的 job 一样使 run 失败（store.JobState.Failed），也作为失败的 job 返回，
// 带名称、原因、错误分类和日志尾部；被连带取消（cancelled）或因上游没有成功而跳过（skipped）的 job 不列。
func TestLatestVerificationTimedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	latest := func(sha string) map[string]VerificationJob {
		t.Helper()
		v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, sha, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != VerifyFailed {
			t.Fatalf("验证 = %+v，应为 failed", v)
		}
		out := map[string]VerificationJob{}
		for _, j := range v.FailedJobs {
			out[j.Name] = j
		}
		return out
	}

	// 只有一个超时的 job
	run := e.verifyRun("only-timeout", "ci.yml", "push", "refs/heads/main", "", store.RunFailed, 0)
	e.prJob(run, 0, "build", store.JobSucceeded, "", "")
	test := e.prJob(run, 0, "test", store.JobTimedOut, "超时", errClassTimeout)
	if _, err := logs.Append(e.queryInt(`SELECT current_attempt_id FROM jobs WHERE id=$1`, test), 0, []byte("test deadlocked\n")); err != nil {
		t.Fatal(err)
	}
	got := latest("only-timeout")
	if j, ok := got["test"]; len(got) != 1 || !ok || j.Reason != "超时" || j.ErrClass != errClassTimeout || j.LogExpired ||
		string(j.LogTail) != "test deadlocked" {
		t.Fatalf("超时的 job = %+v", got)
	}

	// 失败和超时同时存在：都返回；取消、跳过的不列
	run = e.verifyRun("mixed", "ci.yml", "push", "refs/heads/main", "", store.RunFailed, 0)
	e.prJob(run, 0, "lint", store.JobFailed, "退出码 1", "step")
	e.prJob(run, 0, "test", store.JobTimedOut, "超时", errClassTimeout)
	e.prJob(run, 0, "matrix", store.JobCancelled, "同一矩阵中有变体失败（fail-fast）", "")
	e.prJob(run, 0, "deploy", store.JobSkipped, "上游 test 没有成功", "")
	got = latest("mixed")
	if len(got) != 2 || got["lint"].ErrClass != "step" || got["test"].ErrClass != errClassTimeout {
		t.Fatalf("失败和超时 = %+v，期望只有 lint、test", got)
	}
}

// verifyRun 插入 commit sha 上的一个 run：workflow 文件 file、事件 event、ref、目标分支 base（pull_request 之外为空），
// 当前运行为 rerun，返回 run ID。
func (e *env) verifyRun(sha, file, event, ref, base string, state store.RunState, rerun int) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, state,
		github_ctx, current_rerun) VALUES ($1,$2,$2,$3,$4,$5,'webhook',$6,$7,jsonb_build_object('base_ref', $8::text),$9) RETURNING id`,
		testRepoID, file, event, ref, sha, fmt.Sprintf("test:%d", e.seq.Add(1)), state, base, rerun).Scan(&id); err != nil {
		e.t.Fatalf("插入 run：%v", err)
	}
	return id
}

// failedNames 返回验证中失败的 job 的名称。
func failedNames(v *Verification) []string {
	var out []string
	for _, j := range v.FailedJobs {
		out = append(out, j.Name)
	}
	return out
}

// TestLatestVerificationLatestRun：每个验证（workflow、事件、ref、目标分支）只看最新的 run 和它当前这次运行的 job：
// 被后来的 run 取代的失败不算；不同的验证一起汇总；被 paths 排除的 run 不算验证。
func TestLatestVerificationLatestRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		dispatch = "workflow_dispatch"
		push     = "push"
		pull     = "pull_request"
		main     = "refs/heads/main"
		prRef    = "refs/pull/7/head"
	)
	type run struct {
		file, event, ref, base string
		state                  store.RunState
		failed                 []string // 当前这次运行中失败的 job
	}
	for _, c := range []struct {
		name       string
		runs       []run // 按创建顺序
		want       string
		wantFailed []string
	}{
		{"先失败后成功", []run{
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"old"}},
			{"ci.yml", dispatch, main, "", store.RunSucceeded, nil},
		}, VerifySucceeded, nil},
		{"先成功后失败：只带新 run 的 job", []run{
			{"ci.yml", dispatch, main, "", store.RunSucceeded, []string{"old"}},
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"new"}},
		}, VerifyFailed, []string{"new"}},
		{"先失败后进行中", []run{
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"old"}},
			{"ci.yml", dispatch, main, "", store.RunActive, nil},
		}, VerifyPending, nil},
		{"先失败后取消", []run{
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"old"}},
			{"ci.yml", dispatch, main, "", store.RunCancelled, nil},
		}, VerifyCancelled, nil},
		{"不同 workflow 一成一败", []run{
			{"lint.yml", push, main, "", store.RunFailed, []string{"lint"}},
			{"ci.yml", push, main, "", store.RunSucceeded, nil},
		}, VerifyFailed, []string{"lint"}},
		{"同一个 ref 上不同事件各取最新", []run{
			{"ci.yml", push, main, "", store.RunFailed, []string{"push"}},
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"dispatch-old"}},
			{"ci.yml", dispatch, main, "", store.RunSucceeded, nil},
		}, VerifyFailed, []string{"push"}},
		{"push 和 pull_request 各取最新", []run{
			{"ci.yml", push, "refs/heads/feature", "", store.RunSucceeded, nil},
			{"ci.yml", pull, prRef, "main", store.RunFailed, []string{"pr-old"}},
			{"ci.yml", pull, prRef, "main", store.RunSucceeded, nil},
		}, VerifySucceeded, nil},
		{"不同 ref 各取最新", []run{
			{"ci.yml", push, "refs/heads/a", "", store.RunFailed, []string{"a"}},
			{"ci.yml", push, "refs/heads/b", "", store.RunSucceeded, nil},
		}, VerifyFailed, []string{"a"}},
		{"不同目标分支各取最新", []run{
			{"ci.yml", pull, prRef, "release", store.RunFailed, []string{"release"}},
			{"ci.yml", pull, prRef, "main", store.RunSucceeded, nil},
		}, VerifyFailed, []string{"release"}},
		{"失败和取消", []run{
			{"ci.yml", push, main, "", store.RunCancelled, nil},
			{"lint.yml", push, main, "", store.RunFailed, []string{"lint"}},
		}, VerifyFailed, []string{"lint"}},
		{"失败和取消（取消的 run 中也有失败的 job）", []run{
			{"ci.yml", push, main, "", store.RunFailed, []string{"test"}},
			{"lint.yml", push, main, "", store.RunCancelled, []string{"lint"}},
		}, VerifyFailed, []string{"test", "lint"}},
		{"失败和进行中", []run{
			{"ci.yml", push, main, "", store.RunFailed, []string{"test"}},
			{"lint.yml", push, main, "", store.RunQueued, nil},
		}, VerifyPending, nil},
		{"跳过的 run 不算验证", []run{
			{"ci.yml", push, main, "", store.RunSkipped, nil},
			{"lint.yml", push, main, "", store.RunSkipped, nil},
		}, VerifyNone, nil},
		{"跳过和成功", []run{
			{"ci.yml", push, main, "", store.RunSkipped, nil},
			{"lint.yml", push, main, "", store.RunSucceeded, nil},
		}, VerifySucceeded, nil},
		{"失败后被跳过", []run{
			{"ci.yml", dispatch, main, "", store.RunFailed, []string{"old"}},
			{"ci.yml", dispatch, main, "", store.RunSkipped, nil},
		}, VerifyNone, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			sha := fmt.Sprintf("sha-%d", e.seq.Add(1))
			for _, r := range c.runs {
				id := e.verifyRun(sha, r.file, r.event, r.ref, r.base, r.state, 0)
				for _, name := range r.failed {
					e.prJob(id, 0, name, store.JobFailed, "", "")
				}
			}
			v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, sha, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if v.State != c.want || !slices.Equal(failedNames(v), c.wantFailed) {
				t.Fatalf("验证 = %s %v，期望 %s %v", v.State, failedNames(v), c.want, c.wantFailed)
			}
		})
	}

	// 重跑之后只看当前这次运行的 job
	t.Run("重跑", func(t *testing.T) {
		const sha = "rerun"
		id := e.verifyRun(sha, "ci.yml", push, main, "", store.RunFailed, 1)
		e.prJob(id, 0, "first", store.JobFailed, "", "")
		e.prJob(id, 1, "second", store.JobFailed, "", "")
		e.prJob(id, 1, "ok", store.JobSucceeded, "", "")
		v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, sha, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != VerifyFailed || !slices.Equal(failedNames(v), []string{"second"}) {
			t.Fatalf("重跑后 = %s %v，期望只有当前这次运行的 second", v.State, failedNames(v))
		}
		// 重跑成功：当前这次运行没有失败的 job
		e.exec(`UPDATE runs SET state='succeeded', current_rerun=2 WHERE id=$1`, id)
		if v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, sha, 1024); err != nil || v.State != VerifySucceeded || len(v.FailedJobs) != 0 {
			t.Fatalf("重跑成功后 = %+v，%v", v, err)
		}
	})

	// 其他仓库的同一个 commit 不算
	t.Run("其他仓库", func(t *testing.T) {
		v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID+1, "rerun", 1024)
		if err != nil || v.State != VerifyNone {
			t.Fatalf("其他仓库 = %+v，%v", v, err)
		}
	})
}

// TestLatestVerificationPublishLog：失败的发布 job 没有 attempt，日志按 job 保存（cistore.JobLogID），同样返回日志尾部；
// 普通 job 仍读当前 attempt 的日志；过程数据清理之后两者都是 log_expired。
func TestLatestVerificationPublishLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sha = "tag"
	run := e.verifyRun(sha, "release.yml", "push", "refs/tags/v1.0.0", "", store.RunFailed, 0)
	build := e.prJob(run, 0, "build", store.JobFailed, "退出码 1", "step")
	var publish int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state, reason)
		VALUES ('publish',$1,0,'release','release','release','{}','','server','failed','发布失败') RETURNING id`, run).Scan(&publish); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Append(e.queryInt(`SELECT current_attempt_id FROM jobs WHERE id=$1`, build), 0, []byte("compile error\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Append(logstore.PublishLogID(publish), 0, []byte("npm publish: registry returned 403\n")); err != nil {
		t.Fatal(err)
	}
	latest := func() map[string]VerificationJob {
		t.Helper()
		v, err := LatestVerification(e.ctx, e.db, logs, "", testRepoID, sha, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != VerifyFailed {
			t.Fatalf("验证 = %+v", v)
		}
		out := map[string]VerificationJob{}
		for _, j := range v.FailedJobs {
			out[j.Name] = j
		}
		return out
	}
	got := latest()
	if j := got["release"]; j.LogExpired || string(j.LogTail) != "npm publish: registry returned 403" || j.Reason != "发布失败" {
		t.Fatalf("发布 job = %+v", j)
	}
	if j := got["build"]; j.LogExpired || string(j.LogTail) != "compile error" || j.ErrClass != "step" {
		t.Fatalf("普通 job = %+v", j)
	}

	e.exec(`UPDATE runs SET purged_at=now() + interval '1 second' WHERE id=$1`, run)
	for name, j := range latest() {
		if !j.LogExpired || len(j.LogTail) != 0 {
			t.Fatalf("清理之后 %s = %+v，应为 log_expired", name, j)
		}
	}
}

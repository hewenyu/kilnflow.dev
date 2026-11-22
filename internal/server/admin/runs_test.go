package admin

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// run 返回 run 的当前状态。
func (e *testEnv) run(id int64) *cistore.Run {
	e.t.Helper()
	r, err := cistore.GetRun(e.ctx, e.db, id, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// jobStates 返回 run 某一次运行中各 job 的状态（job_key → 状态）。
func (e *testEnv) jobStates(runID int64, rerun int) map[string]store.JobState {
	e.t.Helper()
	jobs, err := cistore.RerunJobs(e.ctx, e.db, runID, rerun, false)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]store.JobState{}
	for _, j := range jobs {
		out[j.JobKey] = j.State
	}
	return out
}

// jobID 返回 run 某一次运行中 job_key 为 key 的 job。
func (e *testEnv) jobID(runID int64, rerun int, key string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `SELECT id FROM jobs WHERE run_id=$1 AND rerun=$2 AND job_key=$3`, runID, rerun, key).Scan(&id); err != nil {
		e.t.Fatalf("run %d 的第 %d 次运行中没有 job %s：%v", runID, rerun, key, err)
	}
	return id
}

// dispatch 手动触发 build.yml，返回 run ID。
func (e *testEnv) dispatch(requestID string) int64 {
	e.t.Helper()
	id, err := e.svc.Dispatch(e.ctx, &DispatchRequest{
		Repo: testRepo, Workflow: "build", Inputs: map[string]string{"channel": "beta"}, RequestID: requestID,
		Actor: testActor, Source: planner.SourceDashboard,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestService_ListRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addRepo(502, "other")
	r1 := e.addRun(runOpts{})
	r2 := e.addRun(runOpts{repoID: 502})
	r3 := e.addRun(runOpts{})

	runs, repos, err := e.svc.ListRuns(e.ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != r3 || runs[1].ID != r2 || repos[1] != "tester/other" || repos[0] != testRepo {
		t.Errorf("全部仓库的 run 应按 ID 倒序：%v %v", ids(runs), repos)
	}
	runs, _, err = e.svc.ListRuns(e.ctx, "Tester/Demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != r3 {
		t.Errorf("按仓库筛选并限制条数：%v（r1=%d）", ids(runs), r1)
	}
	_, _, err = e.svc.ListRuns(e.ctx, "tester/nope", 10)
	wantCode(t, "仓库不存在", err, connect.CodeInvalidArgument)
}

func ids(runs []*cistore.Run) []int64 {
	out := make([]int64, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func TestService_RunnerNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mini, linux := e.addRunner("mini", runnerOpts{}), e.addRunner("linux", runnerOpts{})
	run := e.addRun(runOpts{})
	a := e.addJob(jobOpts{runID: run, key: "a"})
	b := e.addJob(jobOpts{runID: run, key: "b"})
	c := e.addJob(jobOpts{runID: run, key: "c"}) // 没有尝试
	e.addAttempt(a, linux, "failed")
	e.addAttempt(a, mini, "succeeded") // 当前尝试换了 Runner
	e.addAttempt(b, linux, "succeeded")

	jobs, err := cistore.RerunJobs(e.ctx, e.db, run, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	names, err := e.svc.RunnerNames(e.ctx, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if names[a] != "mini" || names[b] != "linux" || names[c] != "" || len(names) != 2 {
		t.Errorf("RunnerNames = %v", names)
	}
	if names, err := e.svc.RunnerNames(e.ctx, nil); err != nil || len(names) != 0 {
		t.Errorf("没有 job 时应返回空：%v %v", names, err)
	}
}

func TestService_CancelRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.addRun(runOpts{state: store.RunActive})
	e.addJob(jobOpts{runID: run, key: "a", state: store.JobPending})
	e.addJob(jobOpts{runID: run, key: "b", state: store.JobWaiting})
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, 'active')`, run)

	if err := e.svc.CancelRun(e.ctx, run, testActor); err != nil {
		t.Fatal(err)
	}
	if got := e.run(run).State; got != store.RunCancelled {
		t.Errorf("run 的状态 = %s，应为 cancelled", got)
	}
	for key, state := range e.jobStates(run, 0) {
		if state != store.JobCancelled {
			t.Errorf("job %s 的状态 = %s，应为 cancelled", key, state)
		}
	}
	e.wantAudit("run.cancel", testActor, map[string]any{"run_id": run})

	// 已经结束的 run：什么也不改变
	done := e.addRun(runOpts{state: store.RunSucceeded})
	if err := e.svc.CancelRun(e.ctx, done, testActor); err != nil {
		t.Fatal(err)
	}
	if got := e.run(done).State; got != store.RunSucceeded {
		t.Errorf("已经结束的 run 不应被取消：%s", got)
	}

	// 不存在的 run：NotFound
	err := e.svc.CancelRun(e.ctx, done+100, testActor)
	wantCode(t, "取消不存在的 run", err, connect.CodeNotFound)
	if len(e.audits("run.cancel")) != 2 {
		t.Errorf("取消不存在的 run 不应写审计日志")
	}
}

func TestService_Dispatch(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	const reqID = "0123456789abcdef0123456789abcdef"

	id := e.dispatch(reqID)
	run := e.run(id)
	if run.Event != "workflow_dispatch" || run.Ref != "refs/heads/main" || run.Source != planner.SourceDashboard ||
		run.SenderLogin != testActor || run.Inputs["channel"] != "beta" || run.Workflow != "build.yml" || run.State.Terminal() {
		t.Errorf("手动触发的 run = %+v", run)
	}
	if got := e.jobStates(id, 0); len(got) != 2 || got["build"] != store.JobPending || got["test"] != store.JobWaiting {
		t.Errorf("job 的状态 = %v，应为 build 等待领取、test 等待上游", got)
	}
	e.wantAudit("run.dispatch", testActor, map[string]any{"run_id": id, "repo": testRepo, "workflow": "build.yml", "ref": "refs/heads/main"})

	// 同一个请求 ID 重复提交只创建一个 run
	if again := e.dispatch(reqID); again != id {
		t.Errorf("重复提交返回 run %d，应为 %d", again, id)
	}
	if n := e.count(`SELECT count(*) FROM runs`); n != 1 {
		t.Errorf("重复提交创建了 %d 个 run", n)
	}
	// 没有请求 ID 时每次都创建新的；ref 可以写 tag
	gitRun(t, e.repoDir, "tag", "v1.0.0")
	id2, err := e.svc.Dispatch(e.ctx, &DispatchRequest{Repo: testRepo, Workflow: "build.yml", Ref: "v1.0.0", Actor: TokenActor, Source: planner.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.run(id2); id2 == id || r.Ref != "refs/tags/v1.0.0" || r.Source != planner.SourceCLI {
		t.Errorf("第二个 run = %+v", r)
	}
}

func TestService_DispatchErrors(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	for _, c := range []struct {
		name      string
		req       DispatchRequest
		field     string
		wantError string
	}{
		{"ref 不存在", DispatchRequest{Repo: testRepo, Workflow: "build", Ref: "nope"}, "ref", "找不到 ref nope"},
		{"workflow 不存在", DispatchRequest{Repo: testRepo, Workflow: "nope"}, "workflow", "nope.yml"},
		{"workflow 不能手动触发", DispatchRequest{Repo: testRepo, Workflow: "push"}, "workflow", "没有声明 on.workflow_dispatch"},
		{"仓库不存在", DispatchRequest{Repo: "tester/nope", Workflow: "build"}, "", "仓库不在 Kiln 中"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.req.Actor, c.req.Source = testActor, planner.SourceCLI
			_, err := e.svc.Dispatch(e.ctx, &c.req)
			if err == nil || !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("应失败并说明 %q：%v", c.wantError, err)
			}
			de, ok := errors.AsType[*planner.DispatchError](err)
			if c.field != "" && (!ok || de.Field != c.field) {
				t.Errorf("应是字段 %s 的 DispatchError：%v", c.field, err)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE state <> 'failed'`); n != 0 {
		t.Errorf("失败的手动触发不应创建可执行的 run（%d 个）", n)
	}
}

func TestService_DispatchWorkflows(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	target, err := e.svc.DispatchWorkflows(e.ctx, testRepo, "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Ref != "refs/heads/main" || len(target.SHA) != 40 || len(target.Workflows) != 2 {
		t.Fatalf("DispatchWorkflows = %+v", target)
	}
	build, push := target.Workflows[0], target.Workflows[1]
	if build.File != "build.yml" || build.Name != "build" || !build.Dispatchable || len(build.Inputs) != 1 || build.ConfigError != "" {
		t.Errorf("build.yml = %+v", build)
	}
	if push.File != "push.yml" || push.Dispatchable {
		t.Errorf("push.yml = %+v", push)
	}
	if n := e.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("列出 workflow 不应创建 run")
	}
}

func TestService_Rerun(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	id := e.dispatch("")

	// 还没有结束：不能重跑
	_, err := e.svc.RerunRun(e.ctx, id, store.RerunAll, testActor, false)
	if err == nil || !strings.Contains(err.Error(), "还没有结束") {
		t.Fatalf("运行中的 run 不应能重跑：%v", err)
	}

	e.must(e.svc.CancelRun(e.ctx, id, testActor))
	n, err := e.svc.RerunRun(e.ctx, id, "", testActor, true) // 空的方式等同于 failed
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("新的 rerun 序号 = %d，应为 1", n)
	}
	run := e.run(id)
	if run.CurrentRerun != 1 || run.State.Terminal() {
		t.Errorf("重跑后 run = rerun %d、%s，应为第 1 次重跑且未结束", run.CurrentRerun, run.State)
	}
	if got := e.jobStates(id, 1); len(got) != 2 || got["build"] != store.JobPending {
		t.Errorf("重跑的 job = %v", got)
	}
	if got := e.jobStates(id, 0); got["build"] != store.JobCancelled {
		t.Errorf("上一次运行的 job 应保持不变：%v", got)
	}
	var keep bool
	if err := e.db.QueryRow(e.ctx, `SELECT keep_on_failure FROM run_reruns WHERE run_id=$1 AND rerun=1`, id).Scan(&keep); err != nil || !keep {
		t.Errorf("run_reruns 应记录失败时保留现场：%v %v", keep, err)
	}

	// 重跑一个 job：返回所属的 run 和新的序号
	e.must(e.svc.CancelRun(e.ctx, id, testActor))
	build := e.jobID(id, 1, "build")
	runID, n, err := e.svc.RerunJob(e.ctx, build, testActor, false)
	if err != nil {
		t.Fatal(err)
	}
	if runID != id || n != 2 {
		t.Errorf("RerunJob = run %d、rerun %d，应为 %d、2", runID, n, id)
	}
	if got := e.jobStates(id, 2); got["build"] != store.JobPending || got["test"] != store.JobWaiting {
		t.Errorf("重跑 build 应一起重新规划下游：%v", got)
	}

	// 运行中的 run 不能重跑 job：返回 run，错误为 FailedPrecondition
	runID, _, err = e.svc.RerunJob(e.ctx, e.jobID(id, 2, "build"), testActor, false)
	wantCode(t, "运行中的 run 重跑 job", err, connect.CodeFailedPrecondition)
	if runID != id {
		t.Errorf("失败时也应返回 job 所属的 run：%d", runID)
	}
	// 不存在的 job、不存在的 run
	runID, _, err = e.svc.RerunJob(e.ctx, build+1000, testActor, false)
	wantCode(t, "重跑不存在的 job", err, connect.CodeNotFound)
	if runID != 0 {
		t.Errorf("job 不存在时 run 应为 0：%d", runID)
	}
	_, err = e.svc.RerunRun(e.ctx, id+1000, store.RerunAll, testActor, false)
	wantCode(t, "重跑不存在的 run", err, connect.CodeNotFound)
}

func TestService_RerunWithoutGitHub(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.addRun(runOpts{state: store.RunFailed})
	if _, err := e.svc.RerunRun(e.ctx, run, store.RerunAll, testActor, false); err == nil {
		t.Error("没有配置 GitHub App 时重跑应失败")
	}
	if _, err := e.svc.Dispatch(e.ctx, &DispatchRequest{Repo: testRepo, Workflow: "build"}); err == nil {
		t.Error("没有配置 GitHub App 时手动触发应失败")
	}
}

// appendLog 把 data 追加到日志 id（attempt ID，或发布 job 的 logstore.PublishLogID）的末尾。
func (e *testEnv) appendLog(id int64, data string) {
	e.t.Helper()
	size, err := e.logs.Size(id)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.logs.Append(id, size, []byte(data)); err != nil {
		e.t.Fatal(err)
	}
}

func TestService_JobLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{state: store.RunActive})

	// build：第一次尝试失败（已结束），自动重试后的第二次尝试还在执行
	build := e.addJob(jobOpts{runID: run, key: "build", state: store.JobRunning})
	first := e.addAttempt(build, mini, "failed")
	e.appendLog(first, "first attempt\n")
	second := e.addAttempt(build, mini, "")
	e.appendLog(second, "hello ")
	// queued：还没有尝试；skipped：没有尝试且已到终态
	queued := e.addJob(jobOpts{runID: run, key: "queued", state: store.JobPending})
	skipped := e.addJob(jobOpts{runID: run, key: "skipped", state: store.JobSkipped})
	// publish：发布 job 的日志按 job 保存
	publish := e.addJob(jobOpts{runID: run, key: "publish", kind: "publish", state: store.JobSucceeded})
	e.appendLog(logstore.PublishLogID(publish), "published\n")
	other := e.addJob(jobOpts{runID: run, key: "other"})
	otherAttempt := e.addAttempt(other, mini, "succeeded")

	for _, c := range []struct {
		name                   string
		job, attempt, offset   int64
		data                   string
		next                   int64
		finished               bool
		gotAttempt, gotCurrent int64
	}{
		{"当前尝试", build, 0, 0, "hello ", 6, false, second, second},
		{"从 offset 继续读", build, 0, 3, "lo ", 6, false, second, second},
		{"读到末尾、还在执行", build, 0, 6, "", 6, false, second, second},
		{"指定历史尝试", build, first, 0, "first attempt\n", 14, false, first, second},
		{"历史尝试读到末尾", build, first, 14, "", 14, true, first, second},
		{"还没有尝试", queued, 0, 0, "", 0, false, 0, 0},
		{"没有尝试且已结束", skipped, 0, 0, "", 0, true, 0, 0},
		{"发布 job", publish, 0, 0, "published\n", 10, false, 0, 0},
		{"发布 job 读到末尾", publish, 0, 10, "", 10, true, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, err := e.svc.JobLog(e.ctx, c.job, c.attempt, c.offset)
			if err != nil {
				t.Fatal(err)
			}
			if string(l.Data) != c.data || l.Next != c.next || l.Finished != c.finished || l.Attempt != c.gotAttempt || l.CurrentAttempt != c.gotCurrent {
				t.Errorf("JobLog = data %q next %d finished %v attempt %d current %d，应为 %q %d %v %d %d",
					l.Data, l.Next, l.Finished, l.Attempt, l.CurrentAttempt, c.data, c.next, c.finished, c.gotAttempt, c.gotCurrent)
			}
		})
	}
	// 当前尝试结束后读到末尾才算结束
	e.exec(`UPDATE attempts SET outcome='succeeded' WHERE id=$1`, second)
	e.exec(`UPDATE jobs SET state='succeeded' WHERE id=$1`, build)
	if l, err := e.svc.JobLog(e.ctx, build, 0, 0); err != nil || l.Finished || l.State != string(store.JobSucceeded) || !l.Terminal {
		t.Errorf("还有没读的内容时不应结束：%+v %v", l, err)
	}
	if l, err := e.svc.JobLog(e.ctx, build, 0, 6); err != nil || !l.Finished {
		t.Errorf("读到末尾后应结束：%+v %v", l, err)
	}

	// 不存在的 job、不属于这个 job 的尝试、发布 job 的尝试：NotFound
	for name, args := range map[string][2]int64{
		"job 不存在":     {publish + 100, 0},
		"尝试属于别的 job":  {build, otherAttempt},
		"尝试不存在":       {build, otherAttempt + 100},
		"发布 job 没有尝试": {publish, first},
	} {
		_, err := e.svc.JobLog(e.ctx, args[0], args[1], 0)
		wantCode(t, name, err, connect.CodeNotFound)
		_, err = e.svc.OpenJobLog(e.ctx, args[0], args[1])
		wantCode(t, name+"（下载）", err, connect.CodeNotFound)
	}
	_, err := e.svc.JobLog(e.ctx, build, 0, -1)
	wantCode(t, "offset 为负数", err, connect.CodeInvalidArgument)
}

// TestService_JobLogChunk：一次最多读取 logChunk 字节，下一次从返回的 offset 继续。
func TestService_JobLogChunk(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	run := e.addRun(runOpts{})
	job := e.addJob(jobOpts{runID: run})
	a := e.addAttempt(job, e.addRunner("mini", runnerOpts{}), "succeeded")
	e.appendLog(a, strings.Repeat("x", logChunk+10))

	l, err := e.svc.JobLog(e.ctx, job, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Data) != logChunk || l.Next != logChunk || l.Finished {
		t.Fatalf("第一次读取 %d 字节、next %d、finished %v", len(l.Data), l.Next, l.Finished)
	}
	if l, err = e.svc.JobLog(e.ctx, job, 0, l.Next); err != nil || len(l.Data) != 10 || l.Finished {
		t.Fatalf("第二次读取：%d 字节 %v", len(l.Data), err)
	}
}

func TestService_OpenJobLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mini := e.addRunner("mini", runnerOpts{})
	run := e.addRun(runOpts{})
	job := e.addJob(jobOpts{runID: run})
	first := e.addAttempt(job, mini, "failed")
	e.appendLog(first, "one\n")
	second := e.addAttempt(job, mini, "succeeded")
	e.appendLog(second, "two two\n")
	empty := e.addJob(jobOpts{runID: run, key: "empty", state: store.JobSkipped})

	for _, c := range []struct {
		name          string
		job, attempt  int64
		want          string
		wantAttemptID int64
	}{
		{"当前尝试", job, 0, "two two\n", second},
		{"指定尝试", job, first, "one\n", first},
		{"没有尝试", empty, 0, "", 0},
	} {
		f, err := e.svc.OpenJobLog(e.ctx, c.job, c.attempt)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || !bytes.Equal(b, []byte(c.want)) || f.Size != int64(len(c.want)) || f.Attempt != c.wantAttemptID {
			t.Errorf("%s：内容 %q 大小 %d attempt %d，应为 %q %d %d", c.name, b, f.Size, f.Attempt, c.want, len(c.want), c.wantAttemptID)
		}
	}
}

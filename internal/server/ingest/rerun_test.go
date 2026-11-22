package ingest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// checkRunPayload 构造 check_run.rerequested（GitHub 上单个 Check Run 的 Re-run）。
func (h *harness) checkRunPayload(externalID string) map[string]any {
	p := h.fake.RepositoryPayload(h.demo, "rerequested")
	p["check_run"] = map[string]any{"external_id": externalID}
	return p
}

// finishedRun 推送 main 并取消产生的 run，返回 run ID（重跑要求 run 已结束）。
func (h *harness) finishedRun() int64 {
	h.t.Helper()
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main")))
	r := h.lastRun(demoRepoID, 1)
	h.cancelRun(r.ID)
	return r.ID
}

// jobOf 返回 run 某次 rerun 中的唯一一个 job。
func (h *harness) jobOf(runID int64, rerun int) int64 {
	h.t.Helper()
	var id int64
	if err := h.db.QueryRow(h.ctx, `SELECT id FROM jobs WHERE run_id=$1 AND rerun=$2`, runID, rerun).Scan(&id); err != nil {
		h.t.Fatalf("读取 run %d rerun %d 的 job：%v", runID, rerun, err)
	}
	return id
}

// rerunDeliveries 返回 run 的各次 rerun 记录的 delivery（rerun → delivery，没有时为空）。
func (h *harness) rerunDeliveries(runID int64) map[int]string {
	h.t.Helper()
	rows, err := h.db.Query(h.ctx, `SELECT rerun, coalesce(delivery_id,'') FROM run_reruns WHERE run_id=$1`, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var n int
		var d string
		if err := rows.Scan(&n, &d); err != nil {
			h.t.Fatal(err)
		}
		out[n] = d
	}
	return out
}

// TestCheckRunRerequestRun：汇总 Check Run 的 Re-run 按"重跑失败的 job"处理，按 delivery 幂等：
// 同一个 delivery 重试时不再重跑（即使 run 又结束了），新的 delivery 在 run 未结束时只记录原因（PLAN #6.6）。
func TestCheckRunRerequestRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID := h.finishedRun()
	d := h.newDelivery()
	h.send("check_run", d, h.checkRunPayload(fmt.Sprintf("run:%d:0", runID)))
	h.process()
	if e := h.event(d); e.Result != fmt.Sprintf("重跑 run #%d 失败的 job（rerun 1）", runID) {
		t.Fatalf("结果 %q", e.Result)
	}
	if got := h.rerunDeliveries(runID); got[1] != d {
		t.Fatalf("rerun 记录 = %v，期望 rerun 1 记录 delivery %s", got, d)
	}

	// 同一个 delivery 重试（例如中途被限流后恢复）：run 已经再次结束，也不再重跑
	h.cancelRun(runID)
	h.exec(`UPDATE events SET processed_at=NULL WHERE delivery_id=$1`, d)
	h.process()
	if r := h.lastRun(demoRepoID, 1); r.CurrentRerun != 1 {
		t.Fatalf("同一个 delivery 重试后 current_rerun = %d，期望仍为 1", r.CurrentRerun)
	}

	// 新的 delivery：照常重跑
	if got := h.deliverOK("check_run", h.checkRunPayload(fmt.Sprintf("run:%d:1", runID))); got != fmt.Sprintf("重跑 run #%d 失败的 job（rerun 2）", runID) {
		t.Fatalf("新 delivery 的结果 %q", got)
	}
	// run 还没有结束：重跑失败只记录原因，不重试事件
	e := h.deliver("check_run", h.checkRunPayload(fmt.Sprintf("run:%d:2", runID)))
	if !e.Processed || e.Attempts != 0 || !strings.HasPrefix(e.Result, "重跑失败：") || !strings.Contains(e.Result, "还没有结束") {
		t.Fatalf("run 未结束时的事件 = %+v", e)
	}
}

// TestCheckRunRerequestJob：job 的 Check Run 的 Re-run 只重跑这个 job（PLAN #6.6）。
func TestCheckRunRerequestJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID := h.finishedRun()
	jobID := h.jobOf(runID, 0)
	got := h.deliverOK("check_run", h.checkRunPayload(fmt.Sprintf("job:%d", jobID)))
	if got != fmt.Sprintf("重跑 run #%d 的 job #%d（rerun 1）", runID, jobID) {
		t.Fatalf("结果 %q", got)
	}
	if r := h.lastRun(demoRepoID, 1); r.CurrentRerun != 1 || r.State != "active" {
		t.Fatalf("run = %+v，期望进入 rerun 1", r)
	}
}

// TestCheckRunRerequestExternalID：不是 Kiln 的 external_id 忽略；格式错误、找不到 job 或 run、
// 或者指向其他仓库的 run 时丢弃并说明原因。
func TestCheckRunRerequestExternalID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID := h.finishedRun()
	jobID := h.jobOf(runID, 0)
	// 另一个已在 Kiln 中的仓库：它的事件不能重跑 demo 的 run
	other, otherGit := h.addRepo(202, "other", 0, true)
	h.deliverOK("push", h.pushPayload(other, "refs/heads/feature", gitref.ZeroSHA, otherGit.head("main")))
	otherPayload := func(externalID string) map[string]any {
		p := h.fake.RepositoryPayload(other, "rerequested")
		p["check_run"] = map[string]any{"external_id": externalID}
		return p
	}
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"其他 App 的 Check Run", h.checkRunPayload("other-app:1"), ""},
		{"job ID 无效", h.checkRunPayload("job:abc"), "external_id 无效：job:abc"},
		{"找不到 job", h.checkRunPayload("job:987654"), "找不到 job"},
		{"找不到 run", h.checkRunPayload("run:987654:0"), "找不到 run"},
		{"其他仓库的 job", otherPayload(fmt.Sprintf("job:%d", jobID)), "找不到 job"},
		{"其他仓库的 run", otherPayload(fmt.Sprintf("run:%d:0", runID)), "找不到 run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.deliverOK("check_run", tt.payload); got != tt.want {
				t.Fatalf("结果 %q，期望 %q", got, tt.want)
			}
		})
	}
	if r := h.lastRun(demoRepoID, 1); r.CurrentRerun != 0 {
		t.Fatalf("不应重跑：current_rerun = %d", r.CurrentRerun)
	}
}

// TestCheckSuiteRerequest："Re-run all checks" 重跑该 commit 上已结束的全部 run（按 ID 顺序）。中途被限流时整个事件稍后重试，
// 已经由这个 delivery 重跑过的 run 不再重跑，即使它在冷却期间已经结束。
func TestCheckSuiteRerequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.commit(map[string]string{".kiln/workflows/lint.yml": lintWorkflow}, "add lint")
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
	runs := h.runs(demoRepoID)
	if len(runs) != 2 || runs[0].Workflow != "ci.yml" || runs[1].Workflow != "lint.yml" {
		t.Fatalf("runs = %+v，期望 ci.yml、lint.yml 两个 run", runs)
	}
	for _, r := range runs {
		h.cancelRun(r.ID)
	}

	// 重新规划第二个 run 时读取 lint.yml 被限流：换用没有缓存的 planner，只限流这个文件的 blob
	h.resetGitHubClient()
	lintBlob := h.git.git("rev-parse", sha+":.kiln/workflows/lint.yml")
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{
		Status: http.StatusTooManyRequests, Until: time.Now().Add(time.Hour),
		Match: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/git/blobs/"+lintBlob) },
	})
	d := h.newDelivery()
	h.send("check_suite", d, h.fake.CheckSuitePayload(h.demo, "rerequested", sha))
	h.process()
	if e := h.event(d); e.Processed || e.Attempts != 0 || !strings.Contains(e.Error, "限流") {
		t.Fatalf("中途被限流后的事件 = %+v，期望推迟", e)
	}
	if got := h.runs(demoRepoID); got[0].CurrentRerun != 1 || got[1].CurrentRerun != 0 {
		t.Fatalf("限流前应只重跑了第一个 run：%+v", got)
	}

	// 冷却期间第一个 run 的重跑已经结束；恢复后重试同一个 delivery：第一个 run 不再重跑，第二个照常重跑
	h.cancelRun(runs[0].ID)
	h.fake.SetRateLimit(h.fake.InstallationID, nil)
	h.resetGitHubClient()
	h.exec(`UPDATE events SET next_attempt_at=now() WHERE delivery_id=$1`, d)
	h.process()
	if e := h.event(d); !e.Processed || e.Result != "重跑 2 个 run" {
		t.Fatalf("恢复后的事件 = %+v", e)
	}
	for _, r := range h.runs(demoRepoID) {
		if r.CurrentRerun != 1 {
			t.Fatalf("run %d（%s）的 current_rerun = %d，期望 1", r.ID, r.Workflow, r.CurrentRerun)
		}
		if got := h.rerunDeliveries(r.ID); got[1] != d {
			t.Fatalf("run %d 的 rerun 1 记录的 delivery = %q，期望 %s", r.ID, got[1], d)
		}
	}

	// 新的 delivery：只重跑已结束的 run（第一个），执行中的第二个不动
	if got := h.deliverOK("check_suite", h.fake.CheckSuitePayload(h.demo, "rerequested", sha)); got != "重跑 1 个 run" {
		t.Fatalf("新 delivery 的结果 %q", got)
	}
	if got := h.runs(demoRepoID); got[0].CurrentRerun != 2 || got[1].CurrentRerun != 1 {
		t.Fatalf("新 delivery 后 = %+v，期望第一个 run 进入 rerun 2，第二个不变", got)
	}
}

// TestCheckRunRerequestRateLimited：重跑请求被 GitHub 限流时事件稍后重试，不丢弃（PLAN #7.8）。
func TestCheckRunRerequestRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID := h.finishedRun()
	h.resetGitHubClient() // 清空 planner 的缓存，重跑时需要重新读取 workflow
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: time.Now().Add(time.Hour)})
	e := h.deliver("check_run", h.checkRunPayload(fmt.Sprintf("run:%d:0", runID)))
	if e.Processed || e.Attempts != 0 || !strings.Contains(e.Error, "限流") {
		t.Fatalf("被限流的重跑请求 = %+v，期望推迟", e)
	}
	if r := h.lastRun(demoRepoID, 1); r.CurrentRerun != 0 {
		t.Fatalf("限流时不应重跑：current_rerun = %d", r.CurrentRerun)
	}
}

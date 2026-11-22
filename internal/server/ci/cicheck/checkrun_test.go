package cicheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/checkrun"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestCheckName(t *testing.T) {
	run := &cistore.Run{Workflow: "ci.yml", Event: "push"}
	if got := checkName(run, ""); got != "ci (push)" {
		t.Fatalf("checkName = %q", got)
	}
	if got := checkName(run, "test (linux)"); got != "ci / test (linux) (push)" {
		t.Fatalf("checkName = %q", got)
	}
}

// TestExternalID：external_id 的格式不能变（已经创建的 Check Run 按它查找），解析与生成一致。
func TestExternalID(t *testing.T) {
	if got := jobExternalID(7); got != "job:7" {
		t.Fatalf("jobExternalID = %q", got)
	}
	if got := runExternalID(3, 2); got != "run:3:2" {
		t.Fatalf("runExternalID = %q", got)
	}
	tests := []struct {
		id      string
		want    CheckTarget
		wantErr bool
	}{
		{id: jobExternalID(7), want: CheckTarget{JobID: 7}},
		{id: runExternalID(3, 2), want: CheckTarget{RunID: 3}},
		{id: "run:3", want: CheckTarget{RunID: 3}},
		{id: "job:x", wantErr: true},
		{id: "run::1", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseExternalID(tt.id)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseExternalID(%q) = %+v, %v；期望 %+v，出错 %v", tt.id, got, err, tt.want, tt.wantErr)
		}
		if errors.Is(err, ErrUnknownExternalID) {
			t.Errorf("ParseExternalID(%q) 不应返回 ErrUnknownExternalID", tt.id)
		}
	}
	if _, err := ParseExternalID("other-app:1"); !errors.Is(err, ErrUnknownExternalID) {
		t.Errorf("其他格式应返回 ErrUnknownExternalID，得到 %v", err)
	}
}

func TestDetailsURL(t *testing.T) {
	w := New(Config{PublicURL: "https://kiln.example"}, Deps{})
	if got := w.detailsURL(3, 0); got != "https://kiln.example/runs/3" {
		t.Fatalf("detailsURL = %q", got)
	}
	if got := w.detailsURL(3, 7); got != "https://kiln.example/jobs/7" {
		t.Fatalf("detailsURL = %q", got)
	}
	// 没有配置 PublicURL：返回空串，不能拼出 "/jobs/7" 这样的相对地址
	noURL := New(Config{}, Deps{})
	if got := noURL.detailsURL(3, 0); got != "" {
		t.Errorf("没有 PublicURL 时 run 的 detailsURL = %q", got)
	}
	if got := noURL.detailsURL(3, 7); got != "" {
		t.Errorf("没有 PublicURL 时 job 的 detailsURL = %q", got)
	}
	if got := JobURL("", 7); got != "" {
		t.Errorf("JobURL(\"\", 7) = %q", got)
	}
	if got := RunURL("", 3); got != "" {
		t.Errorf("RunURL(\"\", 3) = %q", got)
	}
}

func TestConfigAnnotations(t *testing.T) {
	msg := "ci.yml:12:3: 错误：未知的字段 foo\n不带行号的错误\nci.yml:x:1: 错误：坏行号"
	run := &cistore.Run{ConfigError: &msg}
	got := configAnnotations(run)
	if len(got) != 1 {
		t.Fatalf("configAnnotations = %+v", got)
	}
	a := got[0]
	if a.Path != ".kiln/workflows/ci.yml" || a.StartLine != 12 || a.Message != "错误：未知的字段 foo" {
		t.Fatalf("annotation = %+v", a)
	}
}

// TestConfigAnnotationsLimit：一次更新最多携带 maxAnnotations 个 annotation（GitHub 的限制）。
func TestConfigAnnotationsLimit(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 1; i <= maxAnnotations+10; i++ {
		fmt.Fprintf(&b, "ci.yml:%d:1: 错误：第 %d 行\n", i, i)
	}
	msg := b.String()
	if got := configAnnotations(&cistore.Run{ConfigError: &msg}); len(got) != maxAnnotations {
		t.Fatalf("annotation %d 个，期望 %d", len(got), maxAnnotations)
	}
}

// TestParseExternalIDRoundTrip：生成的 external_id 都能解析回原来的对象（GitHub 上的 Re-run 据此找到要重跑的对象）。
func TestParseExternalIDRoundTrip(t *testing.T) {
	t.Parallel()
	for _, id := range []int64{1, 42, 1 << 40} {
		if got, err := ParseExternalID(jobExternalID(id)); err != nil || got != (CheckTarget{JobID: id}) {
			t.Errorf("job %d：ParseExternalID = %+v, %v", id, got, err)
		}
		for _, rerun := range []int{0, 3} {
			if got, err := ParseExternalID(runExternalID(id, rerun)); err != nil || got != (CheckTarget{RunID: id}) {
				t.Errorf("run %d rerun %d：ParseExternalID = %+v, %v", id, rerun, got, err)
			}
		}
	}
}

// TestJobCheckLifecycle：job 的 Check Run 第一次投递时创建并记下 ID，之后的状态变化更新同一个 Check Run；
// 名称、external_id、details_url 和状态与 job 一致（PLAN #13.3）。run 的汇总 Check Run 同时创建。
func TestJobCheckLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	j := h.insertJob(runID, jobSpec{Display: "build (linux)"})
	ext := jobExternalID(j.ID)

	steps := []struct {
		state      store.JobState
		status     string
		conclusion string
		title      string
		updates    int
	}{
		{store.JobPending, gh.CheckStatusQueued, "", "排队中", 0},
		{store.JobRunning, gh.CheckStatusInProgress, "", "执行中", 1},
		{store.JobSucceeded, gh.CheckStatusCompleted, "success", "成功", 2},
	}
	var checkID int64
	for _, s := range steps {
		h.setJobState(j.ID, s.state)
		h.enqueueJob(j.ID)
		h.deliver()
		c, out := h.check(ext)
		if c.Status != s.status || c.Conclusion != s.conclusion || out.Title != s.title || c.UpdateCount != s.updates {
			t.Fatalf("%s：Check Run = %+v（标题 %q），期望 status=%s conclusion=%q 标题 %q 更新 %d 次",
				s.state, c, out.Title, s.status, s.conclusion, s.title, s.updates)
		}
		if checkID == 0 {
			checkID = c.ID
		} else if c.ID != checkID {
			t.Fatalf("%s：创建了新的 Check Run %d，期望更新 %d", s.state, c.ID, checkID)
		}
		if r := h.mustRow(jobKey(j.ID)); r.State != store.OutboxDone {
			t.Fatalf("%s：投递后记录 = %+v，期望 done", s.state, r)
		}
	}
	c, _ := h.check(ext)
	if c.Name != "ci / build (linux) (push)" || c.HeadSHA != testSHA || c.DetailsURL != fmt.Sprintf("%s/jobs/%d", testPublicURL, j.ID) {
		t.Fatalf("Check Run = %+v", c)
	}
	if got := h.job(j.ID).CheckRunID; got == nil || *got != checkID {
		t.Fatalf("jobs.check_run_id = %v，期望 %d", got, checkID)
	}
	rc, _ := h.check(runExternalID(runID, 0))
	if rc.Name != "ci (push)" || rc.DetailsURL != fmt.Sprintf("%s/runs/%d", testPublicURL, runID) {
		t.Fatalf("汇总 Check Run = %+v", rc)
	}
	var runCheckID *int64
	if err := h.db.QueryRow(h.ctx, `SELECT check_run_id FROM run_reruns WHERE run_id=$1 AND rerun=0`, runID).Scan(&runCheckID); err != nil {
		t.Fatal(err)
	}
	if runCheckID == nil || *runCheckID != rc.ID {
		t.Fatalf("run_reruns.check_run_id = %v，期望 %d", runCheckID, rc.ID)
	}
}

// TestJobCheckConclusion：job 的终态映射为 Check Run 的 conclusion，标题为中文状态名。
func TestJobCheckConclusion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	tests := []struct {
		state      store.JobState
		conclusion string
		title      string
	}{
		{store.JobSucceeded, "success", "成功"},
		{store.JobFailed, "failure", "失败"},
		{store.JobTimedOut, "timed_out", "超时"},
		{store.JobCancelled, "cancelled", "已取消"},
		{store.JobSkipped, "skipped", "已跳过"},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			runID := h.insertRun(runSpec{})
			j := h.insertJob(runID, jobSpec{State: tt.state})
			h.enqueueJob(j.ID)
			h.deliver()
			c, out := h.check(jobExternalID(j.ID))
			if c.Status != gh.CheckStatusCompleted || c.Conclusion != tt.conclusion || out.Title != tt.title {
				t.Fatalf("Check Run = (%s, %s, %q)，期望 (completed, %s, %q)", c.Status, c.Conclusion, out.Title, tt.conclusion, tt.title)
			}
		})
	}
}

// job（或 run）结束后才第一次创建 Check Run 时（例如命中缓存、上游失败而跳过、规划时就结束），GitHub 默认把
// started_at 设为创建时间，会晚于 completed_at（master 上 67 个 Check Run 如此）。创建时应带上开始时间：
// 执行过的 job 用它的 started_at，run 用这次运行的开始时间，没有执行过的 job 用结束时间。更新已有的 Check Run 时不改 run 的开始时间。
func TestCheckStartedAtNotAfterCompleted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	ran := h.insertJob(runID, jobSpec{Key: "test", State: store.JobFailed})
	h.exec(`UPDATE jobs SET started_at=now()-interval '10 minutes', finished_at=now()-interval '5 minutes' WHERE id=$1`, ran.ID)
	skipped := h.insertJob(runID, jobSpec{Key: "build", State: store.JobSkipped})
	h.exec(`UPDATE jobs SET started_at=NULL, finished_at=now()-interval '5 minutes' WHERE id=$1`, skipped.ID)
	h.exec(`UPDATE run_reruns SET state='failed', conclusion='failure', started_at=now()-interval '10 minutes', finished_at=now()-interval '5 minutes' WHERE run_id=$1`, runID)
	h.enqueueJob(ran.ID)
	h.enqueueJob(skipped.ID)
	h.enqueueRun(runID)
	h.deliver()

	for name, ext := range map[string]string{"执行过的 job": jobExternalID(ran.ID), "跳过的 job": jobExternalID(skipped.ID), "run": runExternalID(runID, 0)} {
		c, _ := h.check(ext)
		if c.StartedAt == nil || c.CompletedAt == nil || c.StartedAt.After(*c.CompletedAt) {
			t.Errorf("%s：started_at = %v，completed_at = %v，开始时间不能晚于结束时间", name, c.StartedAt, c.CompletedAt)
		}
	}
	for name, ext := range map[string]string{"执行过的 job": jobExternalID(ran.ID), "run": runExternalID(runID, 0)} {
		c, _ := h.check(ext)
		if got := c.CompletedAt.Sub(*c.StartedAt); got < 4*time.Minute {
			t.Errorf("%s 应使用自己的开始时间：耗时 %s，期望约 5 分钟", name, got)
		}
	}
}

// TestJobCheckSummary：摘要包含原因、job 信息和 action key 前缀；失败的 job 附上最后 50 行日志（代码块标记被替换）；
// 命中缓存的成功 job 标题说明命中缓存；摘要不超过 GitHub 的上限。
func TestJobCheckSummary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)

	t.Run("失败附上日志末尾", func(t *testing.T) {
		const attempt = 77
		var log strings.Builder
		for i := 1; i <= 60; i++ {
			fmt.Fprintf(&log, "line %02d\n", i)
		}
		log.WriteString("```evil```\n")
		if _, err := h.logs.Append(attempt, 0, []byte(log.String())); err != nil {
			t.Fatal(err)
		}
		runID := h.insertRun(runSpec{})
		j := h.insertJob(runID, jobSpec{State: store.JobFailed, Reason: "退出码 1", ActionKey: "abcdef0123456789abcdef", AttemptID: attempt})
		h.enqueueJob(j.ID)
		h.deliver()
		_, out := h.check(jobExternalID(j.ID))
		for _, want := range []string{"退出码 1", "- job：`build`", "`linux/amd64`", "executor：`host`", "action key：`abcdef012345`", "最后 50 行日志", "line 60", "`‵`evil`‵`"} {
			if !strings.Contains(out.Summary, want) {
				t.Errorf("摘要中没有 %q：\n%s", want, out.Summary)
			}
		}
		if strings.Contains(out.Summary, "line 10\n") || strings.Contains(out.Summary, "abcdef0123456789") {
			t.Errorf("摘要应只包含最后 50 行日志和 action key 的前 12 个字符：\n%s", out.Summary)
		}
		if !strings.HasPrefix(out.Summary, "退出码 1\n\n") {
			t.Errorf("原因应在摘要开头：\n%s", out.Summary)
		}
	})
	t.Run("失败的发布 job 附上按 job 保存的日志", func(t *testing.T) {
		runID := h.insertRun(runSpec{})
		j := h.insertJob(runID, jobSpec{Key: "release", Kind: workflow.JobKindPublish, State: store.JobFailed, Reason: "发布失败"})
		if _, err := h.logs.Append(logstore.PublishLogID(j.ID), 0, []byte("npm publish: registry returned 403\n")); err != nil {
			t.Fatal(err)
		}
		h.enqueueJob(j.ID)
		h.deliver()
		if _, out := h.check(jobExternalID(j.ID)); !strings.Contains(out.Summary, "registry returned 403") {
			t.Errorf("摘要中没有发布日志：\n%s", out.Summary)
		}
	})
	t.Run("命中缓存", func(t *testing.T) {
		runID := h.insertRun(runSpec{})
		src := h.insertJob(runID, jobSpec{Key: "src", State: store.JobSucceeded})
		j := h.insertJob(runID, jobSpec{State: store.JobSucceeded, CachedFrom: src.ID})
		h.enqueueJob(j.ID)
		h.deliver()
		if _, out := h.check(jobExternalID(j.ID)); out.Title != "成功（命中缓存）" {
			t.Fatalf("标题 %q", out.Title)
		}
	})
	t.Run("截断", func(t *testing.T) {
		runID := h.insertRun(runSpec{})
		j := h.insertJob(runID, jobSpec{State: store.JobFailed, Reason: strings.Repeat("长", 30000)})
		h.enqueueJob(j.ID)
		h.deliver()
		_, out := h.check(jobExternalID(j.ID))
		if len(out.Summary) > checkrun.MaxSummaryBytes+len("…") || !strings.HasSuffix(out.Summary, "…") {
			t.Fatalf("摘要 %d 字节，期望截断到 %d 字节以内并以 … 结尾", len(out.Summary), checkrun.MaxSummaryBytes)
		}
		if !utf8.ValidString(out.Summary) {
			t.Fatal("截断后的摘要不是合法的 UTF-8")
		}
	})
}

// TestRunCheck：run 的汇总 Check Run 的状态（排队、执行中、结束）、结论（优先使用 run_reruns.conclusion）、
// 标题和摘要（每个 job 一行，配置错误、警告、并发组排队、paths 过滤）。
func TestRunCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	tests := []struct {
		name        string
		run         runSpec
		jobs        []jobSpec
		status      string
		conclusion  string
		title       string
		summary     []string
		annotations int
	}{
		{
			name: "job 都在排队", run: runSpec{State: store.RunActive},
			jobs:   []jobSpec{{Key: "a", State: store.JobPending}, {Key: "b", State: store.JobWaiting}},
			status: gh.CheckStatusQueued, title: "2 个 job：pending 1，waiting 1",
			summary: []string{"| job | 状态 | 说明 |", "| pending |"},
		},
		{
			name: "有 job 开始执行", run: runSpec{State: store.RunActive},
			jobs:   []jobSpec{{Key: "a", State: store.JobRunning}, {Key: "b", State: store.JobWaiting}},
			status: gh.CheckStatusInProgress, title: "2 个 job：running 1，waiting 1",
		},
		{
			name: "结束：按 run_reruns 的结论", run: runSpec{State: store.RunFailed, Conclusion: "cancelled"},
			jobs:   []jobSpec{{Key: "a", State: store.JobSucceeded, Started: true}, {Key: "b", State: store.JobFailed, Reason: "a|b\nc"}},
			status: gh.CheckStatusCompleted, conclusion: "cancelled", title: "2 个 job：succeeded 1，failed 1",
			summary: []string{`| a\|b c |`},
		},
		{
			name: "结束：没有记录结论时按状态", run: runSpec{State: store.RunSucceeded},
			jobs:   []jobSpec{{Key: "a", State: store.JobSucceeded, Started: true}},
			status: gh.CheckStatusCompleted, conclusion: "success", title: "1 个 job：succeeded 1",
		},
		{
			name: "并发组排队", run: runSpec{State: store.RunQueued},
			jobs:   []jobSpec{{Key: "a", State: store.JobWaiting}},
			status: gh.CheckStatusQueued, title: "1 个 job：waiting 1", summary: []string{"并发组中已有正在执行的 run，排队等待。"},
		},
		{
			name: "被 paths 过滤", run: runSpec{State: store.RunSkipped},
			status: gh.CheckStatusCompleted, conclusion: "skipped", title: "被 paths 过滤，没有 job 执行",
		},
		{
			name: "配置错误", run: runSpec{State: store.RunFailed, ConfigError: "ci.yml:3:5: 错误：未知字段 foo\nci.yml:9:1: 错误：缺少 steps"},
			status: gh.CheckStatusCompleted, conclusion: "failure", title: "workflow 配置错误",
			summary: []string{"workflow 配置错误：", "未知字段 foo"}, annotations: 2,
		},
		{
			name: "警告", run: runSpec{State: store.RunActive, Warnings: "ci.yml: 警告：x 已废弃"},
			jobs:   []jobSpec{{Key: "a", State: store.JobPending}},
			status: gh.CheckStatusQueued, title: "1 个 job：pending 1", summary: []string{"警告：", "x 已废弃"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := h.insertRun(tt.run)
			for _, j := range tt.jobs {
				h.insertJob(runID, j)
			}
			h.enqueueRun(runID)
			h.deliver()
			c, out := h.check(runExternalID(runID, 0))
			if c.Status != tt.status || c.Conclusion != tt.conclusion || out.Title != tt.title {
				t.Fatalf("汇总 Check Run = (%s, %q, %q)，期望 (%s, %q, %q)", c.Status, c.Conclusion, out.Title, tt.status, tt.conclusion, tt.title)
			}
			for _, want := range tt.summary {
				if !strings.Contains(out.Summary, want) {
					t.Errorf("摘要中没有 %q：\n%s", want, out.Summary)
				}
			}
			if len(out.Annotations) != tt.annotations {
				t.Errorf("annotation %d 个，期望 %d：%+v", len(out.Annotations), tt.annotations, out.Annotations)
			}
		})
	}
}

// TestRunCheckJobNoteTruncated：汇总表格中每个 job 的说明截断到 maxJobNoteBytes。
func TestRunCheckJobNoteTruncated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	h.insertJob(runID, jobSpec{State: store.JobFailed, Reason: strings.Repeat("x", 500)})
	h.enqueueRun(runID)
	h.deliver()
	_, out := h.check(runExternalID(runID, 0))
	if !strings.Contains(out.Summary, "| "+strings.Repeat("x", maxJobNoteBytes)+"… |") {
		t.Fatalf("说明没有截断到 %d 字节：\n%s", maxJobNoteBytes, out.Summary)
	}
}

// TestUpsertCheckReusesExisting：Check Run 已经创建、但 ID 没来得及写回（崩溃）时，按 external_id 找到并复用，
// 不重复创建（PLAN #7.8 的幂等）。
func TestUpsertCheckReusesExisting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	j := h.insertJob(runID, jobSpec{})
	inst := h.syncer.gh.Inst(h.fake.InstallationID)
	existing, err := inst.CreateCheckRun(h.ctx, "tester/demo", &gh.CheckRun{
		Name: "ci / build (push)", HeadSHA: testSHA, ExternalID: jobExternalID(j.ID), Status: gh.CheckStatusQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.setJobState(j.ID, store.JobRunning)
	h.enqueueJob(j.ID)
	h.deliver()
	c, _ := h.check(jobExternalID(j.ID))
	if c.ID != existing.ID || c.Status != gh.CheckStatusInProgress {
		t.Fatalf("Check Run = %+v，期望复用并更新 %d", c, existing.ID)
	}
	if got := h.job(j.ID).CheckRunID; got == nil || *got != existing.ID {
		t.Fatalf("jobs.check_run_id = %v，期望 %d", got, existing.ID)
	}
}

// TestUpsertCheckRecreatesMissing：记下的 Check Run 在 GitHub 上已经不存在（404）时重新创建，并更新记下的 ID。
func TestUpsertCheckRecreatesMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	j := h.insertJob(runID, jobSpec{})
	h.exec(`UPDATE jobs SET check_run_id=424242 WHERE id=$1`, j.ID)
	h.enqueueJob(j.ID)
	h.deliver()
	c, _ := h.check(jobExternalID(j.ID))
	if got := h.job(j.ID).CheckRunID; got == nil || *got != c.ID {
		t.Fatalf("jobs.check_run_id = %v，期望新建的 %d", got, c.ID)
	}
}

// TestJobCheckNotStarted：running 但还没有开始执行（托管执行已预留，jobs.started_at 为空）的 job 显示为 queued，
// 汇总 Check Run 也不算已开始；写入 started_at 后才是 in_progress（PLAN #32.6，I4）。
func TestJobCheckNotStarted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true)
	runID := h.insertRun(runSpec{})
	j := h.insertJob(runID, jobSpec{Display: "build (linux)"})
	h.exec(`UPDATE jobs SET state='running', started_at=NULL, reason='已预留到托管池 gcp-a，等待开始执行' WHERE id=$1`, j.ID)
	h.enqueueJob(j.ID)
	h.deliver()
	c, out := h.check(jobExternalID(j.ID))
	if c.Status != gh.CheckStatusQueued || out.Title != titleNotStarted || !strings.Contains(out.Summary, "已预留到托管池 gcp-a") {
		t.Fatalf("预留后 Check Run = %+v（标题 %q，摘要 %q），期望 queued、%q", c, out.Title, out.Summary, titleNotStarted)
	}
	if rc, _ := h.check(runExternalID(runID, 0)); rc.Status != gh.CheckStatusQueued {
		t.Fatalf("只有预留的 job 时汇总 Check Run = %+v，期望 queued", rc)
	}
	h.exec(`UPDATE jobs SET started_at=now() WHERE id=$1`, j.ID)
	h.enqueueJob(j.ID)
	h.deliver()
	if c, out := h.check(jobExternalID(j.ID)); c.Status != gh.CheckStatusInProgress || out.Title != "执行中" {
		t.Fatalf("开始执行后 Check Run = %+v（标题 %q），期望 in_progress", c, out.Title)
	}
	if rc, _ := h.check(runExternalID(runID, 0)); rc.Status != gh.CheckStatusInProgress {
		t.Fatalf("开始执行后汇总 Check Run = %+v，期望 in_progress", rc)
	}
}

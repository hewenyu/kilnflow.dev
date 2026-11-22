package store

import (
	"slices"
	"testing"
)

func TestTerminalJobStateArgs(t *testing.T) {
	if len(TerminalJobStateArgs()) != len(TerminalJobStates()) {
		t.Fatalf("TerminalJobStateArgs() = %v", TerminalJobStateArgs())
	}
	for _, s := range TerminalJobStates() {
		if !s.Terminal() || !slices.Contains(TerminalJobStateArgs(), string(s)) {
			t.Fatalf("%s 应为终态", s)
		}
	}
	if JobRunning.Terminal() || !JobPending.Queued() || !JobTimedOut.Failed() {
		t.Fatal("状态判断错误")
	}
}

// TestFailedJobStateArgs：SQL 用的失败状态集合与 Failed 是同一条规则——遍历全部状态（JobStateOrder，完整性由
// TestJobStateTitleAndConclusion 约束），Failed 为 true 的都在集合中，集合中的都是 Failed。以后新增失败状态时不会只改一处。
func TestFailedJobStateArgs(t *testing.T) {
	t.Parallel()
	args := FailedJobStateArgs()
	if len(args) != len(FailedJobStates()) {
		t.Fatalf("FailedJobStateArgs() = %v", args)
	}
	for _, s := range JobStateOrder() {
		if s.Failed() != slices.Contains(args, string(s)) {
			t.Errorf("%s：Failed=%v，但 SQL 的失败集合 %v", s, s.Failed(), args)
		}
		if s.Failed() && !s.Terminal() {
			t.Errorf("失败状态 %s 应为终态", s)
		}
	}
	for _, s := range args {
		if !slices.Contains(JobStateOrder(), JobState(s)) {
			t.Errorf("失败集合中的 %s 不是已知状态", s)
		}
	}
	// 被连带取消、因上游没有成功而跳过的不算失败
	if JobCancelled.Failed() || JobSkipped.Failed() {
		t.Error("cancelled、skipped 不应算失败")
	}
	failed := FailedJobStates()
	failed[0] = JobSucceeded
	if JobSucceeded.Failed() || !JobFailed.Failed() {
		t.Fatal("调用方修改切片不应改变失败判断")
	}
}

func TestJobStateListsAreIndependent(t *testing.T) {
	t.Parallel()
	states := TerminalJobStates()
	args := TerminalJobStateArgs()
	order := JobStateOrder()
	states[0] = JobRunning
	args[0] = string(JobRunning)
	order[0] = JobRunning
	if !JobSucceeded.Terminal() || JobRunning.Terminal() {
		t.Fatal("调用方修改切片不应改变状态机的终态判断")
	}
	if !slices.Contains(TerminalJobStateArgs(), string(JobSucceeded)) || slices.Contains(TerminalJobStateArgs(), string(JobRunning)) {
		t.Fatal("调用方修改切片不应改变 SQL 的终态筛选")
	}
	if JobStateOrder()[0] != JobSucceeded {
		t.Fatal("调用方修改切片不应改变 Check Run 的展示顺序")
	}
}

func TestConclusionRoundTrip(t *testing.T) {
	for _, s := range []RunState{RunSucceeded, RunFailed, RunCancelled, RunSkipped} {
		if got := s.Conclusion().RunState(); got != s {
			t.Fatalf("%s → %s", s, got)
		}
	}
	if RunActive.ConclusionOrNil() != nil {
		t.Fatal("非终态的结论应为 NULL")
	}
}

func TestEnvironmentPolicy(t *testing.T) {
	e := &Environment{RefPolicy: []string{"refs/tags/v*"}}
	if !e.RefAllowed("refs/tags/v1.0.0") || e.RefAllowed("refs/heads/main") {
		t.Fatal("ref 策略判断错误")
	}
	// Kiln 镜像的隔离验收 tag 以 v 开头，不用为它修改 refs/tags/v* 策略（DEPLOY #7.2）
	if !e.RefAllowed("refs/tags/v0.0.0-docker-test.1") || e.RefAllowed("refs/tags/docker-test-1") {
		t.Fatal("ref 策略对镜像验收 tag 的判断错误")
	}
	if !(&Environment{}).RefAllowed("refs/heads/x") {
		t.Fatal("空策略应不限制")
	}
}

// 每个 job 状态都有中文名、在汇总顺序中出现一次；终态都有对应的 Check Run 结论，非终态没有。
func TestJobStateTitleAndConclusion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state      JobState
		title      string
		conclusion Conclusion
		failed     bool
		queued     bool
	}{
		{JobWaiting, "等待上游", "", false, true},
		{JobPending, "排队中", "", false, true},
		{JobRunning, "执行中", "", false, false},
		{JobSucceeded, "成功", ConclusionSuccess, false, false},
		{JobFailed, "失败", ConclusionFailure, true, false},
		{JobTimedOut, "超时", ConclusionTimedOut, true, false},
		{JobCancelled, "已取消", ConclusionCancelled, false, false},
		{JobSkipped, "已跳过", ConclusionSkipped, false, false},
	}
	if len(JobStateOrder()) != len(cases) {
		t.Errorf("JobStateOrder() 有 %d 个状态，应为 %d 个", len(JobStateOrder()), len(cases))
	}
	for _, c := range cases {
		if got := c.state.Title(); got != c.title {
			t.Errorf("%s.Title() = %q，应为 %q", c.state, got, c.title)
		}
		if got := c.state.Conclusion(); got != c.conclusion {
			t.Errorf("%s.Conclusion() = %q，应为 %q", c.state, got, c.conclusion)
		}
		if c.state.Terminal() != (c.conclusion != "") {
			t.Errorf("%s.Terminal() = %v", c.state, c.state.Terminal())
		}
		if c.state.Failed() != c.failed || c.state.Queued() != c.queued {
			t.Errorf("%s：Failed=%v Queued=%v", c.state, c.state.Failed(), c.state.Queued())
		}
		if !slices.Contains(JobStateOrder(), c.state) {
			t.Errorf("JobStateOrder() 中没有 %s", c.state)
		}
	}
	if got := JobState("unknown").Title(); got != "unknown" {
		t.Errorf("未知状态的 Title 应为原值，实际 %q", got)
	}
}

func TestRunState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		state      RunState
		terminal   bool
		open       bool
		conclusion Conclusion
	}{
		{RunQueued, false, true, ""},
		{RunActive, false, true, ""},
		{RunSucceeded, true, false, ConclusionSuccess},
		{RunFailed, true, false, ConclusionFailure},
		{RunCancelled, true, false, ConclusionCancelled},
		{RunSkipped, true, false, ConclusionSkipped},
	} {
		if c.state.Terminal() != c.terminal || c.state.Open() != c.open || c.state.Conclusion() != c.conclusion {
			t.Errorf("%s：Terminal=%v Open=%v Conclusion=%q", c.state, c.state.Terminal(), c.state.Open(), c.state.Conclusion())
		}
		got := c.state.ConclusionOrNil()
		if c.terminal != (got != nil) || got != nil && *got != string(c.conclusion) {
			t.Errorf("%s.ConclusionOrNil() = %v", c.state, got)
		}
	}
	// timed_out 只用于 job，run 的结论中记为失败
	if got := ConclusionTimedOut.RunState(); got != RunFailed {
		t.Errorf("timed_out → %s，应为 failed", got)
	}
}

func TestOutcomeReleaseState(t *testing.T) {
	t.Parallel()
	for o, want := range map[Outcome]string{
		OutcomeSucceeded: "succeeded", OutcomeFailed: "failed", OutcomeInfraError: "failed",
		OutcomeCancelled: "failed", OutcomeTimedOut: "failed",
	} {
		if got := o.ReleaseState(); got != want {
			t.Errorf("%s.ReleaseState() = %q，应为 %q", o, got, want)
		}
	}
}

func TestCancelReasonText(t *testing.T) {
	t.Parallel()
	seen := map[string]CancelReason{}
	for _, r := range []CancelReason{CancelFailFast, CancelByConcurrency, CancelPRClosed, CancelRefDeleted} {
		text := r.Text()
		if text == "已取消" {
			t.Errorf("%s 应有具体的说明", r)
		}
		if prev, ok := seen[text]; ok {
			t.Errorf("%s 与 %s 的说明相同：%s", r, prev, text)
		}
		seen[text] = r
	}
	for _, r := range []CancelReason{CancelByUser, "", "other"} {
		if got := r.Text(); got != "已取消" {
			t.Errorf("%q.Text() = %q，应为 已取消", r, got)
		}
	}
}

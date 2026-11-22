package review

import (
	"reflect"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestPRVerifications：PR head 针对目标分支的 AI 审计，每个请求族只取最新的一次（重跑取代首次），按请求 ID 排列；
// 来源与 .kiln/merge.yml 中 required 的 ai-review 对应；带任务时列出各任务的状态（名称为 review）；其他 head、其他目标分支、其他 PR、手动审计、开发请求都不算。
func TestPRVerifications(t *testing.T) {
	h := newHarness(t)
	const head = "headsha"
	first, _ := h.insertRequest(fixture{sha: head, pr: aiPR, baseRef: "main", state: ai.RequestFailed, tasks: []ai.TaskState{ai.TaskFailed}})
	rerun, _ := h.insertRequest(fixture{
		sha: head, pr: aiPR, baseRef: "main", family: first, state: ai.RequestSucceeded,
		tasks: []ai.TaskState{ai.TaskSucceeded, ai.TaskSkipped},
	})
	other, _ := h.insertRequest(fixture{sha: head, pr: aiPR, baseRef: "main", state: ai.RequestActive, tasks: []ai.TaskState{ai.TaskRunning}})
	noTasks, _ := h.insertRequest(fixture{sha: head, pr: aiPR, baseRef: "main", state: ai.RequestFailed})
	// 不算的
	h.insertRequest(fixture{sha: "oldhead", pr: aiPR, baseRef: "main", state: ai.RequestSucceeded})
	h.insertRequest(fixture{sha: head, pr: aiPR, baseRef: "release", state: ai.RequestSucceeded})
	h.insertRequest(fixture{sha: head, pr: aiPR + 1, baseRef: "main", state: ai.RequestSucceeded})
	h.insertRequest(fixture{event: eventManual, sha: head, pr: aiPR, baseRef: "main", state: ai.RequestSucceeded})
	h.insertRequest(fixture{typ: agentcfg.TaskDevelop, sha: head, pr: aiPR, baseRef: "main", state: ai.RequestSucceeded})

	verification := func(id int64, state ai.RequestState, jobs ...store.VerificationJob) store.Verification {
		return store.Verification{ID: id, Name: verificationName, Source: mergecfg.AIReview, Builtin: true, State: string(state), Jobs: jobs}
	}
	job := func(s ai.TaskState) store.VerificationJob {
		return store.VerificationJob{Name: agentcfg.TaskReview, State: string(s)}
	}

	got, err := PRVerifications(h.ctx, h.db, testRepoID, aiPR, head, "main", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Verification{
		verification(rerun, ai.RequestSucceeded), verification(other, ai.RequestActive), verification(noTasks, ai.RequestFailed),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("不带任务：\n%+v\n应为\n%+v", got, want)
	}

	got, err = PRVerifications(h.ctx, h.db, testRepoID, aiPR, head, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	want = []store.Verification{
		verification(rerun, ai.RequestSucceeded, job(ai.TaskSucceeded), job(ai.TaskSkipped)),
		verification(other, ai.RequestActive, job(ai.TaskRunning)),
		verification(noTasks, ai.RequestFailed),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("带任务：\n%+v\n应为\n%+v", got, want)
	}

	if got, err := PRVerifications(h.ctx, h.db, testRepoID+1, aiPR, head, "main", true); err != nil || len(got) != 0 {
		t.Fatalf("其他仓库：%+v（%v）", got, err)
	}
}

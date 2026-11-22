package aidev

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
)

// TestHoldPending：工作项待加 kiln:hold 时，按 PR 编号或正式分支（PR 编号还没记下）判断为待加；已确认、其他仓库、
// 其他 PR 不算。
func TestHoldPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	task := h.taskOf(h.startSelect())
	attempt := func(epoch int) int64 {
		return h.queryInt(`INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id) VALUES ($1,$2,$3,'req') RETURNING id`,
			task.ID, epoch, h.runnerID)
	}
	insert := func(epoch int, pr any, hold bool) int64 {
		return h.queryInt(`INSERT INTO dev_work_items (repo_id, state, active, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, pr_number, hold_pending)
			VALUES ($1,'delivering',true,'main','x',2,'codex',$2,$3,$4) RETURNING id`, testRepoID, attempt(epoch), pr, hold)
	}
	byPR := insert(1, 7, true)
	byBranch := insert(2, nil, true)
	settled := insert(3, 9, false)
	cases := []struct {
		name    string
		repo    int64
		pr      int
		headRef string
		want    bool
	}{
		{"按 PR 编号", testRepoID, 7, "feature", true},
		{"按正式分支", testRepoID, 8, devreport.WorkItemBranch(byBranch, devreport.PhaseImplement, 1), true},
		{"已确认加上", testRepoID, 9, devreport.WorkItemBranch(settled, devreport.PhaseImplement, 1), false},
		{"其他 PR", testRepoID, 10, "feature", false},
		{"其他仓库", testRepoID + 1, 7, devreport.WorkItemBranch(byPR, devreport.PhaseImplement, 1), false},
	}
	for _, c := range cases {
		got, err := HoldPending(h.ctx, h.db, c.repo, c.pr, c.headRef)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s：HoldPending = %v，期望 %v", c.name, got, c.want)
		}
	}
}

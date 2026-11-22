package aidev

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// useDevAgent 把默认分支上的 development.agent 换成 agent（之后规划的开发任务用它）。
func (h *harness) useDevAgent(agent string) {
	h.t.Helper()
	cfg := strings.Replace(devConfigYML, "agent: "+devConfigAgent, "agent: "+agent, 1)
	h.commitOn("main", map[string]string{".kiln/agent/config.yml": cfg}, "换开发 Agent")
}

// wantDevAgents 断言工作项开发过的 Agent（按记入的顺序）。
func (h *harness) wantDevAgents(id int64, want ...string) {
	h.t.Helper()
	if got := h.workItem(id).DevAgents; !slices.Equal(got, want) {
		h.t.Fatalf("工作项 #%d 开发过的 Agent = %v，想要 %v", id, got, want)
	}
}

// claimAgain 对同一个任务再调用一次 Claimed（同一个 Agent 不重复记入）。
func (h *harness) claimAgain(taskID int64) {
	h.t.Helper()
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		return h.svc.TaskType().Claimed(h.ctx, tx, h.task(taskID), nil)
	}); err != nil {
		h.t.Fatal(err)
	}
}

// TestDevAgents_switchedAgent：工作项开发过的 Agent（PLAN #33.7）：选题在领取 Issue 时就记下首次领取的 Agent（不等执行
// 结束）；中途换了 development.agent 后，返修、实现任务只规划时不记，领取执行时记下执行它的 Agent，同一个 Agent 只记一次。
func TestDevAgents_switchedAgent(t *testing.T) {
	t.Run("选题", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "加功能", "要做 a")
		aid := h.acquire(h.taskOf(h.startSelect()).ID)
		res := h.mustClaim(aid, []int{1})
		h.wantDevAgents(res.WorkItemID, devConfigAgent) // 领取 Issue 时就记下，不等执行结束
	})

	t.Run("返修", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		h.issue(1, "加功能", "要做 a")
		w, pr := h.developed(1)
		h.wantDevAgents(w.ID, devConfigAgent)

		h.useDevAgent(agentcfg.AgentCodex)
		h.failingRun(pr, w.HeadSHA)
		h.mustProcess()
		w = h.wantState(w.ID, StateFixing)
		fix := h.fixTask(w)
		if fix.Agent != agentcfg.AgentCodex || h.specOf(fix.ID).Mode != aispec.DevFix {
			t.Fatalf("返修任务的 Agent = %s、mode = %s，想要 codex 的返修", fix.Agent, h.specOf(fix.ID).Mode)
		}
		h.wantDevAgents(w.ID, devConfigAgent) // 只规划：还没有执行

		h.acquire(fix.ID)
		h.wantDevAgents(w.ID, devConfigAgent, agentcfg.AgentCodex)
		h.claimAgain(fix.ID)
		h.wantDevAgents(w.ID, devConfigAgent, agentcfg.AgentCodex)
	})

	t.Run("实现", func(t *testing.T) {
		h := newHarness(t, devConfigYML)
		w := h.partialRound()
		h.wantDevAgents(w.ID, devConfigAgent)

		h.useDevAgent(agentcfg.AgentCodex)
		if err := h.svc.Retry(h.ctx, w.ID, "admin"); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		w = h.workItem(w.ID)
		impl := h.task(*w.TaskID)
		if impl.Agent != agentcfg.AgentCodex || h.specOf(impl.ID).Mode != aispec.DevImplement {
			t.Fatalf("实现任务的 Agent = %s、mode = %s，想要 codex 的实现", impl.Agent, h.specOf(impl.ID).Mode)
		}
		h.wantDevAgents(w.ID, devConfigAgent)

		h.acquire(impl.ID)
		h.wantDevAgents(w.ID, devConfigAgent, agentcfg.AgentCodex)
	})
}

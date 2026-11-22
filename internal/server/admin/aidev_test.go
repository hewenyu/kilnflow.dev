package admin

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestRPC_AIDevSync：从 GitHub 全量同步任务池（暂停中也可以同步）：带 kiln:ready 的进入，有 kiln:skip 的不是候选；
// 不存在的仓库 NotFound。
func TestRPC_AIDevSync(t *testing.T) {
	e := newGitHubEnv(t)
	c := e.rpcClient(testAdminToken)
	repo := testAccount + "/demo"
	if _, err := c.PauseAIDev(e.ctx, req(&apiv1.PauseAIDevRequest{Repo: repo, Paused: true})); err != nil {
		t.Fatal(err)
	}
	e.gh.SetIssue(repo, fakegithub.Issue{Number: 3, Title: "A", Labels: []string{aidev.LabelReady}, UpdatedAt: time.Now()})
	e.gh.SetIssue(repo, fakegithub.Issue{Number: 4, Title: "B", Labels: []string{aidev.LabelReady, aidev.LabelSkip}, UpdatedAt: time.Now()})
	e.gh.SetIssue(repo, fakegithub.Issue{Number: 5, Title: "C", UpdatedAt: time.Now()})
	res, err := c.SyncAIDev(e.ctx, req(&apiv1.SyncAIDevRequest{Repo: repo}))
	if err != nil || res.Msg.Listed != 2 || !res.Msg.Complete || res.Msg.Changed != 2 || res.Msg.Candidates != 1 {
		t.Fatalf("同步任务池：%v, %v", res, err)
	}
	if _, err := c.SyncAIDev(e.ctx, req(&apiv1.SyncAIDevRequest{Repo: "nobody/x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("同步不存在的仓库：%v", err)
	}
}

// TestRPC_AIDev：暂停 / 恢复、列出、重试（只对受阻的）、终止（已结束的不能再终止），
// 错误码供 CLI 显示。
func TestRPC_AIDev(t *testing.T) {
	e := newGitHubEnv(t)
	c := e.rpcClient(testAdminToken)
	repo := testAccount + "/demo"
	if _, err := c.PauseAIDev(e.ctx, req(&apiv1.PauseAIDevRequest{Repo: "nobody/x", Paused: true})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("不存在的仓库：%v", err)
	}
	if _, err := c.PauseAIDev(e.ctx, req(&apiv1.PauseAIDevRequest{Repo: repo, Paused: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseAIDev(e.ctx, req(&apiv1.PauseAIDevRequest{Repo: repo})); err != nil {
		t.Fatal(err)
	}
	if paused, err := aidev.Paused(e.ctx, e.db, testRepoID); err != nil || paused {
		t.Fatalf("恢复后 paused = %v, %v", paused, err)
	}

	claim := e.addAIAttempt(e.addAITask(e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestSucceeded}),
		aiTaskOpts{state: ai.TaskSucceeded}), 0, "dev1", store.OutcomeSucceeded)
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO dev_work_items (repo_id, state, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id, pr_number, reason, hold_label)
		VALUES ($1,'blocked','main','x',2,'claude-code',$2,9,'等人',true) RETURNING id`, testRepoID, claim).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO dev_work_item_issues (work_item_id, repo_id, issue_number, digest) VALUES ($1,$2,3,'d'),($1,$2,4,'d')`, id, testRepoID)
	list, err := c.ListWorkItems(e.ctx, req(&apiv1.ListWorkItemsRequest{Repo: repo}))
	if err != nil || len(list.Msg.Items) != 1 {
		t.Fatalf("列出：%v, %v", list, err)
	}
	w := list.Msg.Items[0]
	if w.Id != id || w.Repo != repo || w.State != aidev.StateBlocked || w.PrNumber != 9 || len(w.Issues) != 2 || w.Branch != devreport.WorkItemBranch(id, devreport.PhaseImplement, 1) ||
		!w.Hold || w.Reason != "等人" || w.MaxFixRounds != 2 {
		t.Fatalf("工作项 = %v", w)
	}
	wantOccupancy(t, e, repo, id)
	if _, err := c.RetryWorkItem(e.ctx, req(&apiv1.RetryWorkItemRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RetryWorkItem(e.ctx, req(&apiv1.RetryWorkItemRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("重试不是受阻的工作项：%v", err)
	}
	if _, err := c.StopWorkItem(e.ctx, req(&apiv1.StopWorkItemRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StopWorkItem(e.ctx, req(&apiv1.StopWorkItemRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("终止已结束的工作项：%v", err)
	}
	if _, err := c.StopWorkItem(e.ctx, req(&apiv1.StopWorkItemRequest{Id: id + 100})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("终止不存在的工作项：%v", err)
	}
	d, err := e.svc.WorkItem(e.ctx, id)
	if err != nil || d.State != aidev.StateStopped || len(d.Issues) != 2 {
		t.Fatalf("详情：%+v, %v", d, err)
	}
	wantOccupancy(t, e, repo, 0)
}

// wantOccupancy 检查只列一个仓库时带上的仓库占用（同一个仓库按工作项串行）：占用者是 occupant（0 表示没有被占用）；
// 列全部仓库时没有。
func wantOccupancy(t *testing.T, e *testEnv, repo string, occupant int64) {
	t.Helper()
	c := e.rpcClient(testAdminToken)
	list, err := c.ListWorkItems(e.ctx, req(&apiv1.ListWorkItemsRequest{Repo: repo}))
	if err != nil {
		t.Fatal(err)
	}
	o := list.Msg.Occupancy
	if o == nil || o.WorkItem != occupant || (occupant != 0) != strings.Contains(o.Reason, "按工作项串行") || len(o.Waiting) != 0 {
		t.Fatalf("仓库占用 = %v，想要占用者 %d", o, occupant)
	}
	if all, err := c.ListWorkItems(e.ctx, req(&apiv1.ListWorkItemsRequest{})); err != nil || all.Msg.Occupancy != nil {
		t.Fatalf("列全部仓库：%v, %v", all, err)
	}
}

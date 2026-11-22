package ingest

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
)

// sendDeferred 投递一个事件但不立即处理：模拟第一次处理失败、按退避等待重试（之后用 retryNow 让它到期）。
func (h *harness) sendDeferred(event string, payload any) string {
	h.t.Helper()
	d := h.newDelivery()
	h.send(event, d, payload)
	h.exec(`UPDATE events SET attempts=1, error='模拟失败', next_attempt_at=now()+interval '1 hour' WHERE delivery_id=$1`, d)
	return d
}

// retryNow 让等待重试的事件到期并处理，返回事件记录。
func (h *harness) retryNow(delivery string) eventRow {
	h.t.Helper()
	h.exec(`UPDATE events SET next_attempt_at=now() WHERE delivery_id=$1`, delivery)
	h.process()
	return h.event(delivery)
}

// aiReviewKey 是 runStates 中 AI 审计请求的键。
const aiReviewKey = "ai-review"

// runStates 返回 sha 上各 workflow 的 run 状态，以及 AI 审计请求的状态（键为 aiReviewKey）。
func (h *harness) runStates(sha string) map[string]string {
	h.t.Helper()
	m := map[string]string{}
	for _, r := range h.runs(demoRepoID) {
		if r.SHA == sha {
			m[r.Workflow] = r.State
		}
	}
	for _, r := range h.reviewRequests(sha) {
		m[aiReviewKey] = r.state
	}
	return m
}

// reviewRequest 是一个 AI 审计请求的状态和它的任务。
type reviewRequest struct {
	id     int64
	state  string
	sender string
	// reused 表示全部任务都沿用了之前的结果。
	reused bool
}

// reviewRequests 返回 demo 仓库 sha 上的 AI 审计请求（按 ID 排序）。
func (h *harness) reviewRequests(sha string) []reviewRequest {
	h.t.Helper()
	rows, err := h.db.Query(h.ctx, `SELECT q.id, q.state, q.sender, coalesce(bool_and(t.reused_from IS NOT NULL), false)
		FROM ai_requests q LEFT JOIN ai_tasks t ON t.request_id=q.id WHERE q.repo_id=$1 AND q.sha=$2 AND q.type='review'
		GROUP BY q.id ORDER BY q.id`, demoRepoID, sha)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []reviewRequest
	for rows.Next() {
		var r reviewRequest
		if err := rows.Scan(&r.id, &r.state, &r.sender, &r.reused); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// TestPullRequestSupersededRetry：H1 的事件第一次处理失败，H2 的事件先规划并开始审计；H1 重试时已经过时，
// 不新建 run、不按 cancel-in-progress 取消 H2 的审计，ref_heads 仍是 H2。
func TestPullRequestSupersededRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.commit(map[string]string{agentcfg.ConfigPath: reviewConfig}, "开启 AI 审计")
	h.git.git("checkout", "-q", "-b", "feature")
	h1 := h.git.commit(map[string]string{"f.txt": "1\n"}, "h1")
	h2 := h.git.commit(map[string]string{"f.txt": "2\n"}, "h2")

	d1 := h.sendDeferred("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", h1, "main"))
	h.deliverOK("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", h2, "main"))
	before := h.runStates(h2)
	if len(before) != 2 {
		t.Fatalf("H2 的 run 和审计请求 = %v，期望 CI 和 AI 审计各一个", before)
	}

	e := h.retryNow(d1)
	if !e.Processed || e.Error != "" || e.Result != supersededResult {
		t.Fatalf("H1 的事件 = %+v，期望按过时处理", e)
	}
	if got := h.runStates(h1); len(got) != 0 {
		t.Fatalf("过时的事件新建了 run 或审计请求：%v", got)
	}
	got := h.runStates(h2)
	for wf, st := range before {
		if got[wf] != st {
			t.Fatalf("H2 的 run %s 从 %s 变成了 %s", wf, st, got[wf])
		}
	}
	if sha, _, _ := h.refHead(demoRepoID, "refs/pull/7/head"); sha != h2 {
		t.Fatalf("ref_heads = %s，应仍是 H2 %s", sha, h2)
	}
}

// TestPullRequestNotSupersededByOlderCommit：新旧按收到事件的先后判断，不按 commit 的时间：
// force push 回到更早的 commit 时，这个更晚收到的事件照常规划。
func TestPullRequestNotSupersededByOlderCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	old := h.git.commit(map[string]string{"f.txt": "1\n"}, "old")
	h.git.git("checkout", "-q", "main")
	h.git.git("checkout", "-q", "-b", "other")
	newer := h.git.commit(map[string]string{"f.txt": "2\n"}, "newer")

	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", newer, "main"))
	if r := h.deliverOK("pull_request", h.prPayload(h.demo, "synchronize", 7, "feature", old, "main")); r == supersededResult {
		t.Fatal("force push 到更早的 commit 不应按过时处理")
	}
	if sha, _, _ := h.refHead(demoRepoID, "refs/pull/7/head"); sha != old {
		t.Fatalf("ref_heads = %s，期望 %s", sha, old)
	}
	if len(h.runStates(old)) != 1 {
		t.Fatalf("应为 %s 新建 run：%+v", old, h.runs(demoRepoID))
	}
}

// TestPullRequestSupersededBase：目标分支改了之后，旧目标分支的事件重试时过时（不按旧的目标分支规划）。
func TestPullRequestSupersededBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "h")

	d := h.sendDeferred("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "release"))
	edited := h.prPayload(h.demo, "edited", 7, "feature", head, "main")
	edited["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": "release"}}}
	h.deliverOK("pull_request", edited)
	if e := h.retryNow(d); e.Result != supersededResult {
		t.Fatalf("目标分支已经改了，旧事件 = %+v", e)
	}
	if base := h.refBase(demoRepoID, "refs/pull/7/head"); base != "main" {
		t.Fatalf("ref_heads 的目标分支 = %s，应为 main", base)
	}
}

// TestPullRequestClosedSuperseded：关闭事件第一次处理失败，PR 重新打开并规划后再重试：不取消新的 run。
func TestPullRequestClosedSuperseded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "h")

	d := h.sendDeferred("pull_request", h.prPayload(h.demo, "closed", 7, "feature", head, "main"))
	h.deliverOK("pull_request", h.prPayload(h.demo, "reopened", 7, "feature", head, "main"))
	run := h.lastRun(demoRepoID, 1)
	if e := h.retryNow(d); e.Result != supersededResult {
		t.Fatalf("PR 已经重新打开，旧的关闭事件 = %+v", e)
	}
	if got := h.lastRun(demoRepoID, 1); got.State != run.State {
		t.Fatalf("run 从 %s 变成了 %s", run.State, got.State)
	}
	if _, _, ok := h.refHead(demoRepoID, "refs/pull/7/head"); !ok {
		t.Fatal("ref_heads 被过时的关闭事件删除")
	}
}

// TestPushSupersededRetry：分支 push 的事件第一次处理失败，同一分支更新的 push 先处理；旧事件重试时过时，
// 不新建 run、不回退 ref_heads。
func TestPushSupersededRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := h.git.head("HEAD")
	c1 := h.git.commit(map[string]string{"f.txt": "1\n"}, "c1")
	c2 := h.git.commit(map[string]string{"f.txt": "2\n"}, "c2")

	d := h.sendDeferred("push", h.pushPayload(h.demo, "refs/heads/main", base, c1))
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", c1, c2))
	if e := h.retryNow(d); e.Result != supersededResult {
		t.Fatalf("旧的 push 事件 = %+v", e)
	}
	if len(h.runStates(c1)) != 0 {
		t.Fatalf("过时的 push 新建了 run：%+v", h.runs(demoRepoID))
	}
	if sha, _, _ := h.refHead(demoRepoID, "refs/heads/main"); sha != c2 {
		t.Fatalf("ref_heads = %s，应仍是 %s", sha, c2)
	}
}

// pathsWorkflow 只在 src/ 下的文件变化时由 main 的 push 触发。
const pathsWorkflow = `name: ci
on:
  push:
    branches: [main]
    paths: ["src/**"]
jobs:
  build:
    runs-on: ` + testPlatform + `
    steps:
      - run: echo build
`

// pushPaths 准备 paths 过滤的场景：c1 修改 src/，c2 只修改 README。返回 c0（paths workflow 所在的 commit）、c1、c2。
func (h *harness) pushPaths() (c0, c1, c2 string) {
	h.t.Helper()
	c0 = h.git.commit(map[string]string{".kiln/workflows/ci.yml": pathsWorkflow}, "paths")
	c1 = h.git.commit(map[string]string{"src/a.go": "package src\n"}, "源码")
	c2 = h.git.commit(map[string]string{"README.md": "文档\n"}, "文档")
	return c0, c1, c2
}

// TestPushSupersededPaths：c0→c1（改 src/）和 c1→c2（只改 README）两个 push 一起积压时，c1 的事件过时，
// c2 匹配 paths 时要包含 c1 带来的改动（从 c0 比较），源码改动照常触发构建。
func TestPushSupersededPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c0, c1, c2 := h.pushPaths()
	d1 := h.newDelivery()
	h.send("push", d1, h.pushPayload(h.demo, "refs/heads/main", c0, c1))
	d2 := h.newDelivery()
	h.send("push", d2, h.pushPayload(h.demo, "refs/heads/main", c1, c2))
	h.process()
	if e := h.event(d1); e.Result != supersededResult {
		t.Fatalf("c1 的事件 = %+v，期望过时", e)
	}
	if got := h.runStates(c2); got["ci.yml"] != "active" {
		t.Fatalf("c2 的 run = %v，期望包含 c1 的源码改动而执行；全部 run：%+v", got, h.runs(demoRepoID))
	}
}

// TestPushSupersededRetryPaths：c1 的事件处理失败等待重试时，c2 先规划，同样从 c0 比较；之后 c1 重试时过时。
func TestPushSupersededRetryPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c0, c1, c2 := h.pushPaths()
	d1 := h.sendDeferred("push", h.pushPayload(h.demo, "refs/heads/main", c0, c1))
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", c1, c2))
	if got := h.runStates(c2); got["ci.yml"] != "active" {
		t.Fatalf("c2 的 run = %v，期望包含等待重试的 c1 的源码改动而执行", got)
	}
	if e := h.retryNow(d1); e.Result != supersededResult {
		t.Fatalf("c1 的事件 = %+v，期望过时", e)
	}
	if n := len(h.runs(demoRepoID)); n != 1 {
		t.Fatalf("run 数 = %d，期望只有 c2 的一个：%+v", n, h.runs(demoRepoID))
	}
}

// TestPushPathsAfterPlanned：c1 已经正常规划过时，c2 只比较 c1...c2（只改 README 时跳过）。
func TestPushPathsAfterPlanned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c0, c1, c2 := h.pushPaths()
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", c0, c1))
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", c1, c2))
	if got := h.runStates(c1); got["ci.yml"] != "active" {
		t.Fatalf("c1 的 run = %v", got)
	}
	if got := h.runStates(c2); got["ci.yml"] != "skipped" {
		t.Fatalf("c2 的 run = %v，期望只比较 c1...c2 而跳过", got)
	}
}

// TestPushChainLimit：积压的过时 push 超过追溯上限时，改动的范围不完整，paths 按匹配处理（不漏掉源码改动）。
func TestPushChainLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ing.pushChainLimit = 2
	c0 := h.git.commit(map[string]string{".kiln/workflows/ci.yml": pathsWorkflow}, "paths")
	commits := []string{c0, h.git.commit(map[string]string{"src/a.go": "package src\n"}, "源码")}
	for i := range 3 { // 之后只改文档：c1→c2→c3→c4，追溯到上限时还没有到 c0
		commits = append(commits, h.git.commit(map[string]string{"README.md": strings.Repeat("x", i+1) + "\n"}, "文档"))
	}
	for i := 1; i < len(commits); i++ {
		h.send("push", h.newDelivery(), h.pushPayload(h.demo, "refs/heads/main", commits[i-1], commits[i]))
	}
	h.process()
	last := commits[len(commits)-1]
	if got := h.runStates(last); got["ci.yml"] != "active" {
		t.Fatalf("最后一个 push 的 run = %v，期望按匹配处理；全部 run：%+v", got, h.runs(demoRepoID))
	}
	// 没有超过上限时照常精确比较：只改文档的积压从 c1 之后开始，跳过
	h2 := newHarness(t)
	h2.ing.pushChainLimit = 2
	d0 := h2.git.commit(map[string]string{".kiln/workflows/ci.yml": pathsWorkflow}, "paths")
	d1 := h2.git.commit(map[string]string{"src/a.go": "package src\n"}, "源码")
	h2.deliverOK("push", h2.pushPayload(h2.demo, "refs/heads/main", d0, d1))
	d2 := h2.git.commit(map[string]string{"README.md": "1\n"}, "文档")
	d3 := h2.git.commit(map[string]string{"README.md": "2\n"}, "文档")
	h2.send("push", h2.newDelivery(), h2.pushPayload(h2.demo, "refs/heads/main", d1, d2))
	h2.send("push", h2.newDelivery(), h2.pushPayload(h2.demo, "refs/heads/main", d2, d3))
	h2.process()
	if got := h2.runStates(d3); got["ci.yml"] != "skipped" {
		t.Fatalf("只改文档的积压 = %v，期望跳过", got)
	}
}

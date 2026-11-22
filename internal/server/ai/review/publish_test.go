package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 审计结果发布为 PR review：按 ai_reviews.task_id 发布，没有幂等键，先写意图、最多尝试一次（C19）。

// aiRepo 让 PR #7 的 head 在 a.go（10 行）上只改第 5 行：main 上写入 a.go，featureRef 上修改第 5 行。返回 head commit。
func (h *harness) aiRepo() string {
	h.t.Helper()
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "// line"
	}
	h.commitOn("main", map[string]string{"a.go": strings.Join(lines, "\n") + "\n"}, "a.go")
	lines[4] = "// changed"
	head := h.commitOn(featureRef, map[string]string{"a.go": strings.Join(lines, "\n") + "\n"}, "改第 5 行")
	h.fake.PullRequestPayload(h.repo, "synchronize", aiPR, featureRef, head, "main", "")
	return head
}

// insertAIReview 写入 PR #7 的一个已结束的审计任务和它的报告（review_state=pending），并写入发布 review 的 outbox 记录。
// 返回任务 ID。
func (h *harness) insertAIReview(head string, rep *aireport.Report) int64 {
	h.t.Helper()
	runner := h.addRunner("review1", "host-review")
	_, tasks := h.insertRequest(fixture{sha: head, pr: aiPR, baseRef: "main", state: ai.RequestFailed, tasks: []ai.TaskState{ai.TaskFailed}})
	taskID := tasks[0]
	aid := h.insertAttempt(taskID, runner)
	h.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(h.ctx, `INSERT INTO ai_reviews (task_id, repo_id, attempt_id, pr_number, head_sha, base_sha, agent, verdict, summary,
			findings, report, review_state) VALUES ($1, $2, $3, $4, $5, 'basesha', 'codex', $6, $7, $8, $9, 'pending')`,
			taskID, testRepoID, aid, aiPR, head, rep.Verdict, rep.Summary, len(rep.Findings), rep.Marshal()); err != nil {
			return err
		}
		return h.writer.Enqueue(h.ctx, tx, prReviewKey(taskID), KindPRReview, prReviewPayload{TaskID: taskID})
	})
	return taskID
}

// deliver 投递全部到期的 outbox 记录。
func (h *harness) deliver() { h.worker.DeliverDue(h.ctx) }

// publisher 返回一个新的 Publisher（直接调用 publish，不经 outbox）。
func (h *harness) publisher() *Publisher { return NewPublisher(h.db, h.client, testPublicURL) }

// outboxRow 是一条 outbox 记录。
type outboxRow struct {
	ID       int64
	State    string
	Attempts int
}

func (h *harness) mustRow(key string) outboxRow {
	h.t.Helper()
	var r outboxRow
	if err := h.db.QueryRow(h.ctx, `SELECT id, state, attempts FROM outbox WHERE dedupe_key=$1`, key).Scan(&r.ID, &r.State, &r.Attempts); err != nil {
		h.t.Fatalf("outbox %s：%v", key, err)
	}
	return r
}

// retry 像 CLI / Dashboard 一样手动重试 outbox 记录（带 PR review 的重试钩子）。
func (h *harness) retry(id int64) {
	h.t.Helper()
	h.tx(func(tx pgx.Tx) error {
		return outbox.Retry(h.ctx, tx, id, map[string]outbox.RetryHook{KindPRReview: OnRetry})
	})
}

// aiReviewState 返回任务的 review_state 和 github_review_id。
func (h *harness) aiReviewState(taskID int64) (string, int64) {
	h.t.Helper()
	r, err := GetReport(h.ctx, h.db, taskID)
	if err != nil || r == nil {
		h.t.Fatalf("任务 %d 的审计报告：%v（%v）", taskID, r, err)
	}
	if r.GitHubReviewID == nil {
		return r.ReviewState, 0
	}
	return r.ReviewState, *r.GitHubReviewID
}

// aiReviewError 返回任务的 review_error。
func (h *harness) aiReviewError(taskID int64) string {
	h.t.Helper()
	r, err := GetReport(h.ctx, h.db, taskID)
	if err != nil || r == nil {
		h.t.Fatalf("任务 %d 的审计报告：%v（%v）", taskID, r, err)
	}
	if r.ReviewError == nil {
		return ""
	}
	return *r.ReviewError
}

// v2Finding 返回一个 v2 格式的问题。
func v2Finding(title, severity, evidence, path string, line int) aireport.Finding {
	return aireport.Finding{
		Title: title, Severity: severity, Evidence: evidence, Path: path, Line: line,
		Scenario: title + "的触发条件", Actual: title + "的实际结果", Expected: title + "的预期结果",
		Impact: title + "的影响", Proof: title + "的证据", Fix: title + "的修复方向",
	}
}

// testReport 返回一个发现 3 个问题的报告：第 1 个落在 diff 中（行内），第 2 个在 diff 之外，第 3 个没有位置。
func testReport() *aireport.Report {
	return &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "审计了 a.go 的改动", Scope: "1 个变更文件",
		CommandsRun: []aireport.Command{
			{Command: "go test ./...", ExitCode: 1, Outcome: aireport.OutcomeFailed, Note: "失败"},
			{Command: "rg -- '--- SKIP'", ExitCode: 1, Outcome: aireport.OutcomeNoMatch},
		},
		Findings: []aireport.Finding{
			v2Finding("改动行有问题", aireport.SeverityHigh, aireport.EvidenceReproduced, "a.go", 5),
			v2Finding("未改动的行", aireport.SeverityLow, aireport.EvidenceCode, "a.go", 1),
			v2Finding("整体问题", aireport.SeverityMedium, aireport.EvidenceSuspicion, "", 0),
		},
		Checks:     []aireport.Check{{Kind: aireport.CheckSuite, Title: "make test", Result: aireport.CheckFailed, Detail: "a.go 的测试失败"}},
		Unverified: []string{"没有运行集成测试"},
	}
}

func TestPublishAIReview(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	h.deliver()

	reviews := h.fake.Reviews(testRepo, aiPR)
	if len(reviews) != 1 {
		t.Fatalf("应创建 1 个 review，实际 %d", len(reviews))
	}
	r := reviews[0]
	if r.Event != "COMMENT" || r.CommitID != head {
		t.Errorf("event = %s，commit = %s", r.Event, r.CommitID)
	}
	// diff 中的问题另作为行内评论，编号与正文一致
	if len(r.Comments) != 1 || r.Comments[0].Path != "a.go" || r.Comments[0].Line != 5 ||
		!strings.Contains(r.Comments[0].Body, "> [!CAUTION]\n> **Kiln AI 审计 · 问题 1 / 3 · 🔴 高 · 改动行有问题**") || !strings.Contains(r.Comments[0].Body, "改动行有问题的触发条件") {
		t.Fatalf("行内评论 = %+v", r.Comments)
	}
	// 正文完整列出全部问题（包括行内的），并给出指向被审 commit 的代码链接和 Dashboard 中任务的地址
	assertNumberedFindings(t, r.Body, testReport().Findings)
	for _, want := range []string{
		"❌ 审计未通过：发现 3 个问题（高 1、中 1、低 1）",
		"| 1 | 🔴 高 | 改动行有问题 | [`a.go:5`](https://github.com/tester/demo/blob/" + head + "/a.go#L5) 💬 | 已复现 |",
		"| 2 | 🟡 低 | 未改动的行 | [`a.go:1`](https://github.com/tester/demo/blob/" + head + "/a.go#L1) | 代码证据 |",
		"| 3 | 🟠 中 | 整体问题 | 不针对具体文件 | 推测（未验证） |", "未改动的行的实际结果",
		"| ❌ 失败 | 整体验证 | make test | a.go 的测试失败 |", "没有运行集成测试",
		"完整执行记录与审计证据（2 条命令，1 条最终失败）", fmt.Sprintf("(%s/ai/tasks/%d)", testPublicURL, taskID),
		aiReviewMarker(taskID),
	} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("正文缺少 %q：\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "go test ./...") {
		t.Error("v2 报告的命令记录不应写进正文（只在 Kiln 中展示）")
	}
	if state, id := h.aiReviewState(taskID); state != StatePublished || id != r.ID {
		t.Errorf("review_state = %s，github_review_id = %d，应为 published、%d", state, id, r.ID)
	}
	if rep, _ := GetReport(h.ctx, h.db, taskID); rep.PublishedAt == nil {
		t.Error("发布后应记下 published_at")
	}
	if row := h.mustRow(prReviewKey(taskID)); row.State != store.OutboxDone {
		t.Errorf("outbox 状态 = %s", row.State)
	}

	// 远端已经有带标记的 review、本地没有记下（例如发布后崩溃）：不重复发布
	h.exec(`UPDATE ai_reviews SET review_state='pending', github_review_id=NULL WHERE task_id=$1`, taskID)
	if err := h.publisher().publish(h.ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 1 {
		t.Fatalf("重复发布了 review：%d", n)
	}
	if state, id := h.aiReviewState(taskID); state != StatePublished || id != r.ID {
		t.Errorf("找到已有的 review 后 review_state = %s，id = %d", state, id)
	}
}

func TestPublishAIReview_unprocessableFallback(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	// 第一次（带行内评论）返回 422，例如审计期间 PR 推送了新的 commit
	var posts []map[string]any
	h.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews") {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		posts = append(posts, m)
		if len(posts) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
			return true
		}
		return false
	})
	h.deliver()
	if len(posts) != 2 {
		t.Fatalf("应请求 2 次，实际 %d", len(posts))
	}
	if posts[0]["commit_id"] != head || posts[0]["comments"] == nil {
		t.Errorf("第一次请求应带 commit_id 和行内评论：%v", posts[0])
	}
	if posts[1]["commit_id"] != nil || posts[1]["comments"] != nil {
		t.Errorf("回退的请求不应带 commit_id 和行内评论：%v", posts[1])
	}
	reviews := h.fake.Reviews(testRepo, aiPR)
	if len(reviews) != 1 || len(reviews[0].Comments) != 0 || strings.Contains(reviews[0].Body, "💬") {
		t.Fatalf("回退后的 review = %+v", reviews)
	}
	assertNumberedFindings(t, reviews[0].Body, testReport().Findings)
	if state, _ := h.aiReviewState(taskID); state != StatePublished {
		t.Errorf("review_state = %s", state)
	}
}

func TestPublishAIReview_skip(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	// 不需要发布的（none）不请求 GitHub
	h.exec(`UPDATE ai_reviews SET review_state='none' WHERE task_id=$1`, taskID)
	h.deliver()
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 0 {
		t.Fatalf("review_state=none 时不应发布，实际 %d", n)
	}
	// 没有 PR 的（手动审计）不发布
	h.exec(`UPDATE ai_reviews SET review_state='pending', pr_number=NULL WHERE task_id=$1`, taskID)
	if err := h.publisher().publish(h.ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 0 {
		t.Fatalf("没有 PR 时不应发布，实际 %d", n)
	}
	if state, _ := h.aiReviewState(taskID); state != StatePending {
		t.Errorf("没有 PR 时 review_state = %s，应不变", state)
	}
	// 没有报告的任务：什么也不做
	if err := h.publisher().publish(h.ctx, taskID+1000); err != nil {
		t.Fatalf("没有报告的任务：%v", err)
	}
	// 保存的报告无效：标记为 failed，不重试
	h.exec(`UPDATE ai_reviews SET pr_number=$2, report='{"verdict":"ok"}' WHERE task_id=$1`, taskID, aiPR)
	if err := h.publisher().publish(h.ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if state, _ := h.aiReviewState(taskID); state != StateFailed || !strings.Contains(h.aiReviewError(taskID), "保存的报告无效") {
		t.Errorf("review_state = %s（%q），应为 failed", state, h.aiReviewError(taskID))
	}
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 0 {
		t.Fatalf("报告无效时不应发布，实际 %d", n)
	}
}

// TestPublishAIReview_foreignMarker：其他用户或其他 bot 的 review 中带同样的标记时不算已发布，照常发布真正的报告。
func TestPublishAIReview_foreignMarker(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	marker := aiReviewMarker(taskID)
	h.fake.AddReview(testRepo, aiPR, fakegithub.Review{
		Body: "ordinary user review " + marker, CommitID: head,
		User: &fakegithub.ReviewUser{ID: 12345, Login: "unrelated-human", Type: "User"},
	})
	h.fake.AddReview(testRepo, aiPR, fakegithub.Review{
		Body: "other app " + marker, CommitID: head,
		User: &fakegithub.ReviewUser{ID: 23456, Login: "other-app[bot]", Type: "Bot"},
	})
	// 与 bot 同名、类型却是 User 的账号也不算
	h.fake.AddReview(testRepo, aiPR, fakegithub.Review{
		Body: "spoof " + marker, CommitID: head,
		User: &fakegithub.ReviewUser{ID: 34567, Login: h.fake.AppSlug + "[bot]", Type: "User"},
	})
	h.deliver()

	var ours []fakegithub.Review
	for _, r := range h.fake.Reviews(testRepo, aiPR) {
		if r.User != nil && r.User.Type == "Bot" && r.User.Login == h.fake.AppSlug+"[bot]" {
			ours = append(ours, r)
		}
	}
	if len(ours) != 1 || !strings.Contains(ours[0].Body, "## Kiln AI 审计") {
		t.Fatalf("应由本 App 发布 1 个 review，实际 %+v", ours)
	}
	if state, id := h.aiReviewState(taskID); state != StatePublished || id != ours[0].ID {
		t.Errorf("review_state = %s，github_review_id = %d，应为 published、%d", state, id, ours[0].ID)
	}
}

// forbidReviews 让创建 review 的请求返回 403。
func (h *harness) forbidReviews() {
	h.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews") {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		return true
	})
}

// TestPublishAIReview_deadMarksFailed：重试次数用完、outbox 放弃时，ai_reviews 同步为发布失败并记录原因；
// 手动重试恢复为 pending，成功后为 published。
func TestPublishAIReview_deadMarksFailed(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	key := prReviewKey(taskID)
	h.forbidReviews()

	// 还没到上限：GitHub 明确拒绝（肯定没有创建），退回 pending
	h.deliver()
	if state, _ := h.aiReviewState(taskID); state != StatePending || h.aiReviewError(taskID) != "" {
		t.Fatalf("第一次失败后 review_state = %s，review_error = %q，应仍为 pending", state, h.aiReviewError(taskID))
	}
	if r := h.mustRow(key); r.State != store.OutboxPending || r.Attempts != 1 {
		t.Fatalf("outbox = %+v，应为 pending、1 次", r)
	}

	// 次数用完的那一次失败：放弃，ai_reviews 为 failed 并带上错误
	h.exec(`UPDATE outbox SET attempts=$2, next_attempt_at=now() WHERE dedupe_key=$1`, key, exhaustedTries)
	h.deliver()
	if r := h.mustRow(key); r.State != store.OutboxDead {
		t.Fatalf("outbox = %+v，应为 dead", r)
	}
	if state, _ := h.aiReviewState(taskID); state != StateFailed {
		t.Fatalf("review_state = %s，应为 failed", state)
	}
	if msg := h.aiReviewError(taskID); !strings.Contains(msg, "Resource not accessible by integration") || !strings.HasPrefix(msg, "多次重试后放弃") {
		t.Fatalf("review_error = %q", msg)
	}

	// 手动重试：恢复为 pending，GitHub 恢复后发布成功
	h.retry(h.mustRow(key).ID)
	if state, _ := h.aiReviewState(taskID); state != StatePending || h.aiReviewError(taskID) != "" {
		t.Fatalf("重试后 review_state = %s，review_error = %q，应为 pending、清除错误", state, h.aiReviewError(taskID))
	}
	h.setIntercept(nil)
	h.deliver()
	if state, id := h.aiReviewState(taskID); state != StatePublished || id == 0 {
		t.Fatalf("重试后 review_state = %s，id = %d，应为 published", state, id)
	}
	if r := h.mustRow(key); r.State != store.OutboxDone {
		t.Fatalf("outbox = %+v，应为 done", r)
	}
}

// TestPublishAIReview_deadAndRetryHooks：放弃和手动重试的钩子只改该改的状态——还没尝试过的（pending）记为失败，结果不明的
// （publishing）记为 unknown（可能已经发布，不能说失败）；已经发布、不需要发布的不改。手动重试只恢复 failed 和 unknown。
// payload 无法解析时什么也不改。
func TestPublishAIReview_deadAndRetryHooks(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	payload, err := json.Marshal(prReviewPayload{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	set := func(state string) {
		h.exec(`UPDATE ai_reviews SET review_state=$2, review_error=NULL, github_review_id=NULL WHERE task_id=$1`, taskID, state)
	}
	tests := []struct {
		from, dead, retried string
	}{
		{StatePending, StateFailed, StatePending},
		{StatePublishing, StateUnknown, StatePending},
		{StatePublished, StatePublished, StatePublished},
		{StateNone, StateNone, StateNone},
	}
	for _, tt := range tests {
		set(tt.from)
		h.tx(func(tx pgx.Tx) error { return OnDead(h.ctx, tx, payload, "x") })
		if state, _ := h.aiReviewState(taskID); state != tt.dead {
			t.Errorf("%s 放弃后 = %s，应为 %s", tt.from, state, tt.dead)
		}
		if tt.dead != tt.from && h.aiReviewError(taskID) != "多次重试后放弃：x" {
			t.Errorf("%s 放弃后 review_error = %q", tt.from, h.aiReviewError(taskID))
		}
		if tt.dead == tt.from && h.aiReviewError(taskID) != "" {
			t.Errorf("%s 放弃时不应写 review_error：%q", tt.from, h.aiReviewError(taskID))
		}
		h.tx(func(tx pgx.Tx) error { return OnRetry(h.ctx, tx, payload) })
		if state, _ := h.aiReviewState(taskID); state != tt.retried || (tt.retried == StatePending && h.aiReviewError(taskID) != "") {
			t.Errorf("%s 放弃再重试后 = %s（%q），应为 %s", tt.from, state, h.aiReviewError(taskID), tt.retried)
		}
	}
	set(StatePending)
	h.tx(func(tx pgx.Tx) error {
		if err := OnDead(h.ctx, tx, []byte("{"), "x"); err != nil {
			return err
		}
		return OnRetry(h.ctx, tx, []byte("{"))
	})
	if state, _ := h.aiReviewState(taskID); state != StatePending {
		t.Errorf("payload 无法解析时 review_state = %s，应不变", state)
	}
}

// TestPublishAIReview_rateLimited：被 GitHub 限流时推迟，不计入次数，ai_reviews 保持 pending。
func TestPublishAIReview_rateLimited(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	key := prReviewKey(taskID)
	h.exec(`UPDATE outbox SET attempts=$2 WHERE dedupe_key=$1`, key, exhaustedTries)
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusTooManyRequests, Until: time.Now().Add(time.Hour)})
	h.deliver()
	if r := h.mustRow(key); r.State != store.OutboxPending || r.Attempts != exhaustedTries {
		t.Fatalf("outbox = %+v，应仍为 pending、不计入次数", r)
	}
	if state, _ := h.aiReviewState(taskID); state != StatePending || h.aiReviewError(taskID) != "" {
		t.Fatalf("review_state = %s，应仍为 pending", state)
	}
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 0 {
		t.Fatalf("限流时不应发布，实际 %d", n)
	}
}

// TestPublishAIReview_lostResponse：创建 review 的请求生效但响应丢失（C19、#26.12）：调用前已记下 publishing，
// 下一次投递只查找、不再创建；找到即 published，GitHub 上只有一个 review。
func TestPublishAIReview_lostResponse(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	h.fake.SetFault(fmt.Sprintf("POST /repos/%s/pulls/%d/reviews", testRepo, aiPR), fakegithub.Fault{Lost: true})
	h.deliver()
	if state, _ := h.aiReviewState(taskID); state != StatePublishing {
		t.Fatalf("响应丢失后 review_state = %s，应为 publishing", state)
	}
	if r := h.mustRow(prReviewKey(taskID)); r.State != store.OutboxPending {
		t.Fatalf("outbox = %+v，响应丢失后应稍后再查", r)
	}
	h.exec(`UPDATE outbox SET next_attempt_at=now()`)
	h.deliver()
	reviews := h.fake.Reviews(testRepo, aiPR)
	if len(reviews) != 1 {
		t.Fatalf("应只有 1 个 review，实际 %d", len(reviews))
	}
	if state, id := h.aiReviewState(taskID); state != StatePublished || id != reviews[0].ID {
		t.Fatalf("review_state = %s，id = %d，应为 published、%d", state, id, reviews[0].ID)
	}
}

// TestPublishAIReview_lostResponseStaleRead：响应丢失之后又读到滞后的 review 列表（看不到刚创建的）：不重发，记为 unknown，
// outbox 直接放弃（等人处理）；人手动重试后恢复为 pending，读到最新列表时找到已有的 review，仍然只有一个。
func TestPublishAIReview_lostResponseStaleRead(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	key := prReviewKey(taskID)
	h.fake.SetFault(fmt.Sprintf("POST /repos/%s/pulls/%d/reviews", testRepo, aiPR), fakegithub.Fault{Lost: true})
	h.deliver()
	h.fake.HideReviews(true)
	h.exec(`UPDATE outbox SET next_attempt_at=now()`)
	h.deliver()
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 1 {
		t.Fatalf("读到滞后的列表后重复发布了 review：%d", n)
	}
	if state, _ := h.aiReviewState(taskID); state != StateUnknown || !strings.Contains(h.aiReviewError(taskID), "结果不明") {
		t.Fatalf("review_state = %s（%q），应为 unknown", state, h.aiReviewError(taskID))
	}
	if r := h.mustRow(key); r.State != store.OutboxDead {
		t.Fatalf("outbox = %+v，应直接放弃（dead），等人手动重试", r)
	}
	// 放弃的钩子不把 unknown 改成别的（结果仍然不明）
	if state, _ := h.aiReviewState(taskID); state != StateUnknown {
		t.Fatalf("放弃后 review_state = %s，应仍为 unknown", state)
	}
	h.retry(h.mustRow(key).ID)
	if state, _ := h.aiReviewState(taskID); state != StatePending {
		t.Fatalf("手动重试后 review_state = %s，应为 pending", state)
	}
	h.fake.HideReviews(false)
	h.deliver()
	if n := len(h.fake.Reviews(testRepo, aiPR)); n != 1 {
		t.Fatalf("手动重试后应找到已有的 review，实际 %d 个", n)
	}
	if state, _ := h.aiReviewState(taskID); state != StatePublished {
		t.Fatalf("review_state = %s，应为 published", state)
	}
}

// TestPublishAIReview_noGitHub：没有配置 GitHub App 时投递什么也不做（记录完成，不改 ai_reviews）。
func TestPublishAIReview_noGitHub(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	payload, err := json.Marshal(prReviewPayload{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPublisher(h.db, nil, testPublicURL).Deliver(h.ctx, payload); err != nil {
		t.Fatal(err)
	}
	if state, _ := h.aiReviewState(taskID); state != StatePending {
		t.Fatalf("review_state = %s，应不变", state)
	}
	if err := h.publisher().Deliver(h.ctx, []byte("{")); err == nil {
		t.Fatal("payload 无法解析时应返回错误")
	}
}

// TestPublishAIReview_notSentBackToPending：已经记为 publishing，但创建 review 的 POST 根本没有发出（客户端在读完变更后
// 得知这个 installation 正在限流，请求在本地被拦下）：肯定没有创建，退回 pending，之后照常重试；GitHub 没有收到 POST。
func TestPublishAIReview_notSentBackToPending(t *testing.T) {
	h := newHarness(t)
	head := h.aiRepo()
	taskID := h.insertAIReview(head, testReport())
	const limitPath = "/kiln-test/rate-limit"
	var posts atomic.Int32
	h.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == limitPath:
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return true
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files"):
			// 读变更的同时，同一 installation 的另一个请求被限流：客户端记下限流，之后的 POST 不发出
			_ = h.client.Inst(h.fake.InstallationID).Do(r.Context(), http.MethodGet, limitPath, nil, nil)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			posts.Add(1)
		}
		return false
	})
	h.deliver()
	if state, _ := h.aiReviewState(taskID); state != StatePending {
		t.Fatalf("POST 没有发出时 review_state = %s，应退回 pending", state)
	}
	h.setIntercept(nil)
	if n := posts.Load(); n != 0 {
		t.Fatalf("限流中不应发出 POST，实际 %d 次", n)
	}
	if r := h.mustRow(prReviewKey(taskID)); r.State != store.OutboxPending {
		t.Errorf("outbox = %+v，应等待重试", r)
	}
}

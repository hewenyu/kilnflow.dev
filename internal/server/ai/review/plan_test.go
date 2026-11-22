package review

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// requestCount 返回仓库的审计请求数。
func (h *harness) requestCount() int64 {
	return h.queryInt(`SELECT count(*) FROM ai_requests WHERE repo_id=$1 AND type='review'`, testRepoID)
}

// TestPlanEvent：PR 事件按配置建审计请求（每个平台一个待领取的任务，spec 冻结 PR、base、发布方式和审计说明的来源）；
// 同一个事件重投去重；不是 PR 事件不规划。
func TestPlanEvent(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(reviewConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	ev := h.prEvent("opened", head, base)
	id := h.planPR(ev)

	r := h.request(id)
	if r.State != ai.RequestActive || r.SHA != head || r.GroupKey == nil || *r.GroupKey != GroupKey(aiPR) || r.FamilyID != id {
		t.Fatalf("请求 = %+v", r)
	}
	ts := h.tasks(id)
	if len(ts) != 1 || ts[0].State != ai.TaskPending || ts[0].Key != "review-macos-arm64" || !slices.Equal(ts[0].Platforms, []string{"darwin/arm64"}) {
		t.Fatalf("任务 = %+v", ts)
	}
	var s aispec.ReviewSpec
	if err := ts[0].DecodeSpec(&s); err != nil {
		t.Fatal(err)
	}
	if s.PRNumber != aiPR || s.BaseSHA != base || s.HeadRef != featureRef || s.BaseRef != "main" || !s.ReviewComments ||
		s.InstructionsFrom != base || s.ConfigSHA != head || s.PolicyDigest == "" || s.Trigger != aispec.TriggerPullRequest {
		t.Fatalf("spec = %+v", s)
	}
	var pr int
	var baseRef string
	if err := h.db.QueryRow(h.ctx, `SELECT pr_number, base_ref FROM ai_review_requests WHERE request_id=$1`, id).Scan(&pr, &baseRef); err != nil {
		t.Fatal(err)
	}
	if pr != aiPR || baseRef != "main" {
		t.Fatalf("明细 = PR %d、%s", pr, baseRef)
	}

	// 同一个事件重投：不建第二个请求
	again, err := h.rev.PlanEvent(h.ctx, h.prEvent("synchronize", head, base))
	if err != nil || again != 0 {
		t.Fatalf("重投 = %d（%v），应为 0", again, err)
	}
	// 不是 PR 事件：不规划
	push := *ev
	push.Event = "push"
	if got, err := h.rev.PlanEvent(h.ctx, &push); err != nil || got != 0 {
		t.Fatalf("push 事件 = %d（%v）", got, err)
	}
	if n := h.requestCount(); n != 1 {
		t.Fatalf("审计请求 %d 个，应为 1", n)
	}
}

// TestPlanEvent_supersede：同一个 PR 的新 head 取代旧 head 未结束的审计（取消，原因是被取代）；同一 head 换目标分支不取代。
func TestPlanEvent_supersede(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(reviewConfig)
	head1 := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	first := h.planPR(h.prEvent("opened", head1, base))
	head2 := h.commitOn(featureRef, map[string]string{"g.go": "package f\n"}, "第二个 commit")
	second := h.planPR(h.prEvent("synchronize", head2, base))

	if r := h.request(first); r.State != ai.RequestCancelled {
		t.Fatalf("旧 head 的请求 = %s，应已取消", r.State)
	}
	old := h.tasks(first)[0]
	if old.State != ai.TaskCancelled || old.CancelReason == nil || *old.CancelReason != string(ai.CancelSuperseded) {
		t.Fatalf("旧任务 = %s（%v）", old.State, old.CancelReason)
	}
	if r := h.request(second); r.State != ai.RequestActive {
		t.Fatalf("新 head 的请求 = %s", r.State)
	}

	// 同一 head、另一个目标分支：是另一个请求，但不取代同 head 的审计
	ev := h.prEvent("edited", head2, base)
	ev.BaseRef = "release"
	third := h.planPR(ev)
	if r := h.request(second); r.State != ai.RequestActive {
		t.Fatalf("同 head 的请求被取代了：%s", r.State)
	}
	if r := h.request(third); r.State != ai.RequestActive {
		t.Fatalf("新目标分支的请求 = %s", r.State)
	}
}

// TestPlanEvent_configError：配置有错误时建一个失败的请求（没有任务，说明错误），PR 验证不通过。
func TestPlanEvent_configError(t *testing.T) {
	h := newHarness(t)
	h.enableReview(reviewConfig)
	base := h.branchHead("main")
	head := h.commitOn(featureRef, map[string]string{".kiln/agent/config.yml": "version: 1\nreview: [\n"}, "改坏配置")
	id := h.planPR(h.prEvent("opened", head, base))
	r := h.request(id)
	if r.State != ai.RequestFailed || r.ConfigError == nil || *r.ConfigError == "" {
		t.Fatalf("请求 = %s（%v），应为配置错误", r.State, r.ConfigError)
	}
	if ts := h.tasks(id); len(ts) != 0 {
		t.Fatalf("配置错误时不应有任务：%d 个", len(ts))
	}
	if got := h.events.list(); !slices.Equal(got, []prVerified{{testRepoID, aiPR, false}}) {
		t.Fatalf("事件 = %+v，应为不通过", got)
	}
	if n := h.queryInt(`SELECT count(*) FROM ai_review_requests WHERE request_id=$1 AND pr_number=$2`, id, aiPR); n != 1 {
		t.Fatal("配置错误的请求也要有明细（PR 编号）")
	}
}

// TestPlanEvent_notEnabled：没有 AI 配置、关闭了审计、关闭了 PR 触发时不建请求。
func TestPlanEvent_notEnabled(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"没有配置", ""},
		{"关闭审计", "version: 1\nreview:\n  enabled: false\n"},
		{"关闭 PR 触发", reviewConfig + "  triggers:\n    pull_request: false\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			base := h.branchHead("main")
			if tt.config != "" {
				base = h.enableReview(tt.config)
			}
			head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
			id, err := h.rev.PlanEvent(h.ctx, h.prEvent("opened", head, base))
			if err != nil || id != 0 {
				t.Fatalf("PlanEvent = %d（%v），应不规划", id, err)
			}
			if n := h.requestCount(); n != 0 {
				t.Fatalf("审计请求 %d 个", n)
			}
		})
	}
}

// TestPlanEvent_noGitHub：没有配置 GitHub App 时 PR 事件、手动审计、重跑都返回 ErrNoGitHubApp。
func TestPlanEvent_noGitHub(t *testing.T) {
	h := newHarness(t)
	rev := New(Deps{DB: h.db, Core: h.core, Git: h.rev.git, Log: h.rev.log})
	if _, err := rev.PlanEvent(h.ctx, h.prEvent("opened", "abc", "def")); !errors.Is(err, ErrNoGitHubApp) {
		t.Fatalf("PlanEvent：%v", err)
	}
	if _, _, err := rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo}); !errors.Is(err, ErrNoGitHubApp) {
		t.Fatalf("Trigger：%v", err)
	}
	if _, err := rev.Rerun(h.ctx, RerunRequest{RequestID: 1, Mode: RerunAll}); !errors.Is(err, ErrNoGitHubApp) {
		t.Fatalf("Rerun：%v", err)
	}
}

// TestTrigger：手动审计按 RequestID 幂等；没有配置时是参数错误。
func TestTrigger(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", RequestID: "r0", Actor: "bob"})
	if te, ok := errors.AsType[*TriggerError](err); !ok || te.Field != "ref" {
		t.Fatalf("没有配置时应为 ref 的参数错误：%v", err)
	}
	head := h.enableReview(reviewConfig)
	id, sha, err := h.rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", RequestID: "r1", Actor: "bob", Source: store.SourceCLI})
	if err != nil || sha != head {
		t.Fatalf("Trigger = %d、%s（%v）", id, sha, err)
	}
	again, sha2, err := h.rev.Trigger(h.ctx, &TriggerRequest{Repo: testRepo, Ref: "main", RequestID: "r1", Actor: "bob"})
	if err != nil || again != id || sha2 != head {
		t.Fatalf("同一个 RequestID 应返回同一个请求：%d、%s（%v）", again, sha2, err)
	}
	r := h.request(id)
	if r.Event != eventManual || r.Trigger != aispec.TriggerManual || r.GroupKey != nil {
		t.Fatalf("手动审计的请求 = %+v", r)
	}
	var s aispec.ReviewSpec
	if err := h.tasks(id)[0].DecodeSpec(&s); err != nil {
		t.Fatal(err)
	}
	if s.PRNumber != 0 || s.ReviewComments || s.Trigger != aispec.TriggerManual || s.InstructionsFrom != head {
		t.Fatalf("手动审计的 spec = %+v，没有 PR、不发布评论、按被审的 commit 读取说明", s)
	}
	if n := h.queryInt(`SELECT count(*) FROM audit_log WHERE action='ai_review.trigger'`); n != 1 {
		t.Fatalf("审计日志 %d 条", n)
	}
}

// TestCancelPR：PR 关闭时取消它未结束的审计请求，已结束的不动；其他 PR 不受影响。
func TestCancelPR(t *testing.T) {
	h := newHarness(t)
	reqID, _ := h.prReviewed(reviewConfig)
	done, _ := h.insertRequest(fixture{sha: "old", pr: aiPR, baseRef: "main", state: ai.RequestSucceeded})
	other, _ := h.insertRequest(fixture{sha: "x", pr: aiPR + 1, baseRef: "main", state: ai.RequestActive, tasks: []ai.TaskState{ai.TaskPending}})

	n, err := h.rev.CancelPR(h.ctx, testRepoID, aiPR)
	if err != nil || n != 1 {
		t.Fatalf("CancelPR = %d（%v），应取消 1 个", n, err)
	}
	if r := h.request(reqID); r.State != ai.RequestCancelled {
		t.Fatalf("请求 = %s", r.State)
	}
	if ts := h.tasks(reqID); ts[0].CancelReason == nil || *ts[0].CancelReason != string(ai.CancelPRClosed) {
		t.Fatalf("取消原因 = %v", ts[0].CancelReason)
	}
	if r := h.request(done); r.State != ai.RequestSucceeded {
		t.Fatalf("已结束的请求被改了：%s", r.State)
	}
	if r := h.request(other); r.State != ai.RequestActive {
		t.Fatalf("其他 PR 的请求被取消了：%s", r.State)
	}
	if n, err := h.rev.CancelPR(h.ctx, testRepoID, aiPR); err != nil || n != 0 {
		t.Fatalf("再次取消 = %d（%v）", n, err)
	}
}

// queueUpdate 模拟合并队列把默认分支合并进 PR 分支：main 上先提交 files，再合并进 featureRef，返回新的 head。
func (h *harness) queueUpdate(files map[string]string) string {
	h.t.Helper()
	h.commitOn("main", files, "默认分支前进")
	h.git("checkout", "-q", featureRef)
	h.git("merge", "-q", "--no-ff", "-m", "Merge main", "main")
	head := h.git("rev-parse", "HEAD")
	h.git("checkout", "-q", "main")
	return head
}

// TestQueueReuse：合并队列更新 PR 分支后，新 head 沿用旧 head 已经通过的审计（PLAN #28.4）：任务直接为 succeeded、
// 记下来源，请求随即通过并写 PR 验证事件；明细记下来源请求。条件不满足时照常审计。
func TestQueueReuse(t *testing.T) {
	mainMoved := map[string]string{"main.txt": "新的默认分支\n"}
	// setup 审计 PR 的旧 head（通过），返回旧 head 的请求、任务和 head。
	setup := func(t *testing.T) (*harness, int64, *ai.Task, string) {
		t.Helper()
		h := newHarness(t)
		runner := h.addRunner("review1", "host-review")
		reqID, task := h.prReviewed(reviewConfig)
		h.finish(task.ID, runner, reportResult(cleanReport()))
		return h, reqID, task, h.request(reqID).SHA
	}
	queueEvent := func(h *harness, head string) *Event {
		ev := h.prEvent("synchronize", head, h.branchHead("main"))
		ev.FromSelf = true
		return ev
	}

	t.Run("沿用", func(t *testing.T) {
		h, reqID, task, old := setup(t)
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		h.events.events = nil
		id := h.planPR(queueEvent(h, head))
		ts := h.tasks(id)
		if len(ts) != 1 || ts[0].State != ai.TaskSucceeded || ts[0].ReusedFrom == nil || *ts[0].ReusedFrom != task.ID {
			t.Fatalf("任务 = %+v，应沿用任务 #%d", ts, task.ID)
		}
		if ts[0].Reason == nil || !strings.Contains(*ts[0].Reason, "合并队列只把目标分支 main") {
			t.Errorf("沿用的说明 = %v", ts[0].Reason)
		}
		if r := h.request(id); r.State != ai.RequestSucceeded {
			t.Fatalf("请求 = %s，应随即通过", r.State)
		}
		var reused *int64
		if err := h.db.QueryRow(h.ctx, `SELECT reused_from FROM ai_review_requests WHERE request_id=$1`, id).Scan(&reused); err != nil {
			t.Fatal(err)
		}
		if reused == nil || *reused != reqID {
			t.Fatalf("明细的 reused_from = %v，应为 %d", reused, reqID)
		}
		if got := h.events.list(); !slices.Equal(got, []prVerified{{testRepoID, aiPR, true}}) {
			t.Fatalf("事件 = %+v", got)
		}
	})

	notReused := func(t *testing.T, h *harness, ev *Event) {
		t.Helper()
		id := h.planPR(ev)
		ts := h.tasks(id)
		if len(ts) != 1 || ts[0].State != ai.TaskPending || ts[0].ReusedFrom != nil {
			t.Fatalf("任务 = %s（沿用 %v），应照常审计", ts[0].State, ts[0].ReusedFrom)
		}
	}
	t.Run("更新期间改投了目标分支", func(t *testing.T) {
		// 合并队列记录的更新目标是 dev，PR 改投 master 后的新 head 不能沿用 dev 那次更新的旧审计（PLAN #28.5）
		h, _, _, old := setup(t)
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		h.queueFromBase[head] = "dev"
		notReused(t, h, queueEvent(h, head))
	})
	t.Run("不是本 App 触发", func(t *testing.T) {
		h, _, _, old := setup(t)
		head := h.queueUpdate(mainMoved)
		h.queueFrom[head] = old
		ev := queueEvent(h, head)
		ev.FromSelf = false
		notReused(t, h, ev)
	})
	t.Run("合并队列没有记录", func(t *testing.T) {
		h, _, _, _ := setup(t)
		head := h.queueUpdate(mainMoved)
		notReused(t, h, queueEvent(h, head))
	})
	t.Run("不是合并提交", func(t *testing.T) {
		h, _, _, old := setup(t)
		head := h.commitOn(featureRef, map[string]string{"g.go": "package f\n"}, "普通 commit")
		h.queueFrom[head] = old
		notReused(t, h, queueEvent(h, head))
	})
	t.Run("旧审计没有通过", func(t *testing.T) {
		h, reqID, _, old := setup(t)
		head := h.queueUpdate(mainMoved)
		h.exec(`UPDATE ai_requests SET state='failed' WHERE id=$1`, reqID)
		h.queueFrom[head] = old
		notReused(t, h, queueEvent(h, head))
	})
	t.Run("审计规则变了", func(t *testing.T) {
		h, _, _, old := setup(t)
		// 默认分支改了审计说明：policy_digest 不同，旧结论不再适用（PLAN #29.6）
		head := h.queueUpdate(map[string]string{".kiln/agent/review.md": "新的审计说明\n"})
		h.queueFrom[head] = old
		notReused(t, h, queueEvent(h, head))
	})
	t.Run("平台变了", func(t *testing.T) {
		h, _, _, old := setup(t)
		// 同一个 id（任务 key 不变）改成一组平台：旧结论只在 macOS 上审计过，不能沿用到可以在 Linux 上执行的任务
		head := h.queueUpdate(map[string]string{agentcfg.ConfigPath: "version: 1\nreview:\n  agent: codex\n  required_platforms:\n" +
			"    - {id: macos-arm64, any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]}\n"})
		h.queueFrom[head] = old
		notReused(t, h, queueEvent(h, head))
	})
}

// TestPlanReviewAgents：review.agent 可以写一个列表，规划时为这个工作项的 PR 选第一个没有开发过它的 Agent（PLAN #33.7）；
// 都被开发用过时仍然选第一个配置的 Agent（任务等待另一个审计 Agent）。
func TestPlanReviewAgents(t *testing.T) {
	const twoAgents = "version: 1\nreview:\n  agent: [claude-code, codex]\n  required_platforms:\n    - {os: macos, arch: arm64}\n"
	cases := []struct {
		name string
		devs []string
		want string
	}{
		{"开发用的是 Claude Code", []string{agentcfg.AgentClaudeCode}, agentcfg.AgentCodex},
		{"开发用的是 Codex", []string{agentcfg.AgentCodex}, agentcfg.AgentClaudeCode},
		{"两个都用过", []string{agentcfg.AgentCodex, agentcfg.AgentClaudeCode}, agentcfg.AgentClaudeCode},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			base := h.enableReview(twoAgents)
			// 工作项在规划之前就有开发过的 Agent（Server 建 PR 之后才记下编号时按分支关联，这里直接写 PR 编号）
			h.devWorkItem(aiPR, c.devs...)
			head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
			id := h.planPR(h.prEvent("opened", head, base))
			task := h.tasks(id)[0]
			if task.Agent != c.want {
				t.Errorf("审计 Agent = %s，应为 %s", task.Agent, c.want)
			}
			var s aispec.ReviewSpec
			if err := task.DecodeSpec(&s); err != nil {
				t.Fatal(err)
			}
			// Agent 也是审计规则摘要的一部分：换了 Agent 的审计结论不能互相沿用
			if s.Agent != c.want || s.PolicyDigest == "" {
				t.Errorf("spec 的 Agent = %s，摘要 = %q", s.Agent, s.PolicyDigest)
			}
		})
	}
}

// toolsConfig 启用审计并声明受管理的工具链（PLAN #33.12）。
const toolsConfig = "version: 1\nrequirements: {tools: [pnpm, go]}\nreview:\n  agent: codex\n  required_platforms:\n    - {os: macos, arch: arm64}\n"

// TestPlanEvent_tools：requirements.tools 的版本和摘要来自被审 commit 上的 .kiln/runtimes.lock（按声明的顺序、按平台）；
// 没有 lockfile 或缺少平台的条目是配置错误（请求失败并说明原因），不换版本。
func TestPlanEvent_tools(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(toolsConfig)
	goDigest, pnpmDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	lock := "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + goDigest + "\"\n\"linux/arm64\" = \"sha256:" + strings.Repeat("c", 64) + "\"\n" +
		"[pnpm]\nversion = \"12.14.0\"\nany = \"" + pnpmDigest + "\"\n"
	head := h.commitOn(featureRef, map[string]string{".kiln/runtimes.lock": lock}, "锁定工具链")
	id := h.planPR(h.prEvent("opened", head, base))
	ts := h.tasks(id)
	if len(ts) != 1 {
		t.Fatalf("任务 = %+v（请求 %+v）", ts, h.request(id))
	}
	var s aispec.ReviewSpec
	if err := ts[0].DecodeSpec(&s); err != nil {
		t.Fatal(err)
	}
	want := []aispec.ToolRef{
		{Name: "pnpm", Version: "12.14.0", Platform: "any", Digest: pnpmDigest},
		{Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: goDigest},
	}
	if len(s.PlatformTools) != 1 || !slices.Equal(s.PlatformTools["darwin/arm64"], want) {
		t.Fatalf("工具链 = %+v，期望 darwin/arm64 → %+v", s.PlatformTools, want)
	}

	// 被审 commit 的 lockfile 缺少本平台的 go：配置错误，没有任务
	head2 := h.commitOn(featureRef, map[string]string{".kiln/runtimes.lock": strings.Replace(lock, "\"darwin/arm64\" = \""+goDigest+"\"\n", "", 1)}, "缺少平台")
	id2 := h.planPR(h.prEvent("synchronize", head2, base))
	r := h.request(id2)
	if r.State != ai.RequestFailed || r.ConfigError == nil || !strings.Contains(*r.ConfigError, "没有平台 darwin/arm64 的条目") || len(h.tasks(id2)) != 0 {
		t.Fatalf("缺少平台：请求 = %s（%v）", r.State, r.ConfigError)
	}
}

// TestPlanEvent_toolsGroup：一组平台（any_of）的审计任务可以在组内任一平台上执行：工具链为组内每个平台各解析一份
// （领取时按节点的平台下发）；任何一个平台在 lockfile 中缺条目都是配置错误，说明是哪个平台。
func TestPlanEvent_toolsGroup(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview("version: 1\nrequirements: {tools: [pnpm, go]}\nreview:\n  agent: codex\n  required_platforms:\n" +
		"    - id: any\n      any_of: [{os: macos, arch: arm64}, {os: linux, arch: arm64}]\n")
	macGo, linuxGo, pnpmDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("b", 64)
	lock := "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + macGo + "\"\n\"linux/arm64\" = \"" + linuxGo + "\"\n" +
		"[pnpm]\nversion = \"12.14.0\"\nany = \"" + pnpmDigest + "\"\n"
	head := h.commitOn(featureRef, map[string]string{".kiln/runtimes.lock": lock}, "锁定工具链")
	id := h.planPR(h.prEvent("opened", head, base))
	ts := h.tasks(id)
	if len(ts) != 1 || ts[0].Key != "review-any" || ts[0].DisplayName != "codex · any" || !slices.Equal(ts[0].Platforms, []string{"darwin/arm64", "linux/arm64"}) {
		t.Fatalf("任务 = %+v（请求 %+v）", ts, h.request(id))
	}
	if r := ts[0].Reason; r == nil || *r != "等待 AI 审计节点领取（平台 darwin/arm64 或 linux/arm64）" {
		t.Errorf("等待原因 = %v", r)
	}
	var s aispec.ReviewSpec
	if err := ts[0].DecodeSpec(&s); err != nil {
		t.Fatal(err)
	}
	pnpm := aispec.ToolRef{Name: "pnpm", Version: "12.14.0", Platform: "any", Digest: pnpmDigest}
	want := map[string][]aispec.ToolRef{
		"darwin/arm64": {pnpm, {Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: macGo}},
		"linux/arm64":  {pnpm, {Name: "go", Version: "1.27.3", Platform: "linux/arm64", Digest: linuxGo}},
	}
	if !reflect.DeepEqual(s.PlatformTools, want) || s.PlatformID != "any" {
		t.Fatalf("工具链 = %+v，期望 %+v（平台 id %q）", s.PlatformTools, want, s.PlatformID)
	}

	// 组内的 linux/arm64 在 lockfile 中缺 go：配置错误，没有任务
	head2 := h.commitOn(featureRef, map[string]string{".kiln/runtimes.lock": strings.Replace(lock, "\"linux/arm64\" = \""+linuxGo+"\"\n", "", 1)}, "缺少平台")
	id2 := h.planPR(h.prEvent("synchronize", head2, base))
	r := h.request(id2)
	if r.State != ai.RequestFailed || r.ConfigError == nil || !strings.Contains(*r.ConfigError, "没有平台 linux/arm64 的条目") || len(h.tasks(id2)) != 0 {
		t.Fatalf("缺少平台：请求 = %s（%v）", r.State, r.ConfigError)
	}
}

// TestPlanEvent_toolsNoLock：声明了工具链但仓库中没有 lockfile 是配置错误。
func TestPlanEvent_toolsNoLock(t *testing.T) {
	h := newHarness(t)
	base := h.enableReview(toolsConfig)
	head := h.commitOn(featureRef, map[string]string{"f.go": "package f\n"}, "feature")
	r := h.request(h.planPR(h.prEvent("opened", head, base)))
	if r.State != ai.RequestFailed || r.ConfigError == nil || !strings.Contains(*r.ConfigError, "requirements.tools：仓库中没有 .kiln/runtimes.lock") {
		t.Fatalf("请求 = %s（%v）", r.State, r.ConfigError)
	}
}

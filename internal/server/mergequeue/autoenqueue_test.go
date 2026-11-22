package mergequeue

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const autoConfig = "auto: true\nrequired: [ci, ai-review]\n"

// openAutoPR 打开一个没有 kiln:merge 的 PR（等待自动入队），labels 是 PR 上的其他 label。
func (h *harness) openAutoPR(n int, branch, head string, labels ...string) {
	h.t.Helper()
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: n, Title: "PR " + branch, HeadRef: branch, HeadSHA: head, Labels: labels})
}

// autoRuns 记下 head 按默认分支规划完成，CI 和 AI 审计的 run 分别处于 ci、ai。
func (h *harness) autoRuns(n int, head string, ci, ai store.RunState) {
	h.t.Helper()
	h.planned(n, head, "main")
	for _, r := range []struct {
		wf    string
		state store.RunState
	}{{"ci.yml", ci}, {mergecfg.AIReview, ai}} {
		h.exec(`INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, pr_number, state, github_ctx)
			VALUES ($1,$2,$2,'pull_request',$3,$4,'webhook',gen_random_uuid()::text,$5,$6,'{"base_ref":"main"}')`,
			repoID, r.wf, workflow.PullRef(n), head, n, string(r.state))
	}
}

func (h *harness) nominate(n int) {
	h.t.Helper()
	if err := h.svc.Nominate(h.ctx, repoID, n); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) candidates() []int {
	h.t.Helper()
	ids, err := store.QueryIDs(h.ctx, h.db, `SELECT pr_number FROM merge_auto_candidates WHERE repo_id=$1 ORDER BY pr_number`, repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		out = append(out, int(id))
	}
	return out
}

// TestAutoEnqueue（PLAN #29.5）：开启 auto 的仓库，CI 和 AI 审计都通过时自动加 label、入队并合并；操作者记为 auto，
// 自动打 tag 的发送者是 PR 作者。
func TestAutoEnqueue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	head := h.branch("feature")
	h.openAutoPR(7, "feature", head)

	// AI 审计还没结束：不入队，候选丢掉（审计结束时会再提名）
	h.autoRuns(7, head, store.RunSucceeded, store.RunActive)
	h.nominate(7)
	h.process()
	if e, _ := Latest(h.ctx, h.db, repoID, 7); e != nil || len(h.candidates()) != 0 {
		t.Fatalf("验证没有结束时不应入队：%+v，候选 %v", e, h.candidates())
	}
	h.setRuns(7, head, store.RunSucceeded)
	h.nominate(7)
	h.process()
	e := h.wantState(7, StateMerged, "")
	if e.EnqueuedBy != AutoActor || !h.pull(7).Merged || !slices.Contains(h.pull(7).Labels, Label) {
		t.Fatalf("应自动入队并合并：%+v，PR %+v", e, h.pull(7))
	}
	var actor string
	if err := h.db.QueryRow(h.ctx, `SELECT actor FROM audit_log WHERE action='mergequeue.enqueue'`).Scan(&actor); err != nil || actor != AutoActor {
		t.Fatalf("审计日志的操作者：%q（%v）", actor, err)
	}
	h.wantAutoTag(7, e.MergeSHA, testAccount, mqevents.AutoTagFromQueue, Label)
	if len(h.candidates()) != 0 {
		t.Errorf("候选应已处理：%v", h.candidates())
	}
}

// TestAutoEnqueueNewHeadWhileOldQueued：旧 head 还在队列中时用户推送了新 head，新 head 的规划和验证
// 先于执行者处理 head 变化完成（提名因为 PR 还在队列中被丢掉）；执行者移出旧 head 时重新提名，新 head 自动入队并合并。
func TestAutoEnqueueNewHeadWhileOldQueued(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	h1 := h.branch("feature")
	h.openPR(7, "feature", h1)
	h.git("checkout", "-q", "feature")
	h2 := h.commit(map[string]string{"more.txt": "2\n"}, "新的 commit")
	h.git("checkout", "-q", "main")
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.HeadSHA = h2 })
	h.autoRuns(7, h2, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7) // 旧 head 还在队列中：不记候选
	h.process()   // 移出旧 head，重新提名
	h.process()   // 自动入队新 head 并合并
	e := h.wantState(7, StateMerged, "")
	if e.HeadSHA != h2 || e.EnqueuedBy != AutoActor {
		t.Fatalf("新 head 应自动入队并合并：%+v", e)
	}
}

// TestAutoEnqueueBotAuthorNoAutoTag：机器人（Renovate）的 PR 验证通过后自动入队、合并，
// 自动打 tag 的记录照实写下机器人发起者，最终由发布策略跳过：不建 tag、不规划发布，Retry 之后仍然拒绝。
func TestAutoEnqueueBotAuthorNoAutoTag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig + "tag: {}\n"}, "开启 auto 和 tag")
	head := h.branch("deps")
	h.openAutoPR(7, "deps", head)
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Author = "renovate[bot]" })
	h.autoRuns(7, head, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)
	h.process()
	e := h.wantState(7, StateMerged, "")

	var (
		by  string
		bot bool
	)
	if err := h.db.QueryRow(h.ctx, `SELECT merged_by, sender_is_bot FROM auto_tags WHERE repo_id=$1 AND pr_number=7`, repoID).
		Scan(&by, &bot); err != nil {
		t.Fatal(err)
	}
	if by != "renovate[bot]" || !bot {
		t.Fatalf("应记下机器人发起者：merged_by=%q sender_is_bot=%v", by, bot)
	}

	svc, pl := h.autoTagService()
	tagState := func() (string, string) {
		h.t.Helper()
		var state, reason string
		if err := h.db.QueryRow(h.ctx, `SELECT state, reason FROM auto_tags WHERE repo_id=$1 AND pr_number=7`, repoID).
			Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		return state, reason
	}
	if err := svc.Process(h.ctx, repoID); err != nil {
		t.Fatalf("自动打 tag：%v", err)
	}
	state, reason := tagState()
	if state != autotag.StateSkipped || !strings.Contains(reason, "机器人") {
		t.Fatalf("第三方机器人的 PR 应跳过并写明原因：state=%q reason=%q", state, reason)
	}
	if n := h.countRequests("POST /repos/" + h.repo.Owner + "/" + h.repo.Name + "/git/refs"); n != 0 {
		t.Errorf("不应创建 tag：%d 个请求", n)
	}
	if reqs := pl.requests(); len(reqs) != 0 {
		t.Errorf("不应规划发布：%+v", reqs)
	}

	// Retry 之后仍然拒绝：授权判断在认出已有 tag 之前
	if err := svc.Retry(h.ctx, repoID, e.MergeSHA, testAccount); err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(h.ctx, repoID); err != nil {
		t.Fatalf("重试后自动打 tag：%v", err)
	}
	if state, reason := tagState(); state != autotag.StateSkipped || !strings.Contains(reason, "机器人") {
		t.Fatalf("重试后仍应跳过：state=%q reason=%q", state, reason)
	}
	if reqs := pl.requests(); len(reqs) != 0 {
		t.Errorf("重试后仍不应规划发布：%+v", reqs)
	}
}

// tagPlannerStub 记录自动打 tag 的规划请求。
type tagPlannerStub struct {
	mu   sync.Mutex
	reqs []autotag.TagPush
}

func (p *tagPlannerStub) PlanTagPush(_ context.Context, ev autotag.TagPush) ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, ev)
	return []int64{int64(len(p.reqs))}, nil
}

func (p *tagPlannerStub) requests() []autotag.TagPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]autotag.TagPush(nil), p.reqs...)
}

// unknownOrigin 是 DevOrigin 的替身：没有可信的 AI 开发工作项归属（第三方 PR）。
type unknownOrigin struct{}

func (unknownOrigin) Resolve(context.Context, store.Querier, int64, int, string, string) (autotag.DevOrigin, error) {
	return autotag.DevOrigin{Kind: autotag.OriginUnknown}, nil
}

// autoTagService 返回用这个测试的数据库和模拟 GitHub 的自动打 tag 服务（来源判定是第三方 PR）。
func (h *harness) autoTagService() (*autotag.Service, *tagPlannerStub) {
	h.t.Helper()
	client := &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: h.fake.URL, UploadURL: h.fake.URL}
	pl := &tagPlannerStub{}
	svc := autotag.New(autotag.Deps{
		DB: h.db,
		Forge: func(r *store.Repo) forge.Repo {
			return forge.GitHub(client, r.InstallationID, r.FullName())
		},
		Planner: pl, Outbox: outbox.NewWriter(true), Log: slog.New(slog.DiscardHandler), DevOrigin: unknownOrigin{},
	})
	return svc, pl
}

// countRequests 返回 fakegithub 收到的形如 prefix 的请求数。
func (h *harness) countRequests(prefix string) int {
	n := 0
	for _, r := range h.fake.Requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// TestAutoEnqueueLostLabelResponse（独立审查发现）：加 kiln:merge 的响应丢失（label 已经加上）、label 的 webhook 也没有
// 送到：重试时不再加 label，直接入队（不把"有 label"当成已经入队而丢掉候选）。
func TestAutoEnqueueLostLabelResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	head := h.branch("feature")
	h.openAutoPR(7, "feature", head)
	h.autoRuns(7, head, store.RunSucceeded, store.RunSucceeded)
	h.fake.SetFault("POST /repos/alice/demo/issues/7/labels", fakegithub.Fault{Lost: true})
	h.nominate(7)
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("加 label 的响应丢失时应返回错误（稍后重试）")
	}
	if !slices.Contains(h.pull(7).Labels, Label) || !slices.Equal(h.candidates(), []int{7}) {
		t.Fatalf("label 应已加上、候选保留：labels %v，候选 %v", h.pull(7).Labels, h.candidates())
	}
	h.process()
	e := h.wantState(7, StateMerged, "")
	if e.EnqueuedBy != AutoActor || h.countAddLabel(7) != 1 || len(h.candidates()) != 0 {
		t.Fatalf("重试应直接入队并合并：%+v，加 label %d 次，候选 %v", e, h.countAddLabel(7), h.candidates())
	}
}

// TestAutoEnqueueConditions：不满足条件时不自动入队。
func TestAutoEnqueueConditions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		yml    string
		labels []string
		ci, ai store.RunState
		setup  func(h *harness, n int, head string)
	}{
		{name: "没有开启 auto", yml: "required: [ci, ai-review]\n", ci: store.RunSucceeded, ai: store.RunSucceeded},
		{name: "配置错误", yml: "auto: true\nrequired: [ci]\n", ci: store.RunSucceeded, ai: store.RunSucceeded},
		{name: "kiln:hold", yml: autoConfig, labels: []string{HoldLabel}, ci: store.RunSucceeded, ai: store.RunSucceeded},
		{name: "AI 审计没有通过", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunFailed},
		{name: "没有 AI 审计 run", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded, setup: func(h *harness, n int, _ string) {
			h.exec(`DELETE FROM runs WHERE pr_number=$1 AND workflow=$2`, n, mergecfg.AIReview)
		}},
		{name: "全部跳过", yml: "auto: true\nrequired: [ai-review]\n", ci: store.RunSkipped, ai: store.RunSkipped},
		{
			name: "必需的 job 被跳过", yml: "auto: true\nrequired: [ci/test, ai-review]\n", ci: store.RunSucceeded, ai: store.RunSucceeded,
			setup: func(h *harness, n int, _ string) {
				h.exec(`INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, spec, platform, executor_kind, state)
					SELECT 'normal', id, 'test', 'test', 'test', '{}', 'linux/arm64', 'oci', 'skipped' FROM runs WHERE pr_number=$1 AND workflow='ci.yml'`, n)
			},
		},
		{name: "CI 失败", yml: autoConfig, ci: store.RunFailed, ai: store.RunSucceeded},
		{name: "draft", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded, setup: func(h *harness, n int, _ string) {
			h.fake.UpdatePull(h.repo, n, func(p *fakegithub.Pull) { p.Draft = true })
		}},
		{name: "没有按默认分支规划", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded, setup: func(h *harness, n int, head string) {
			h.exec(`DELETE FROM pr_plans WHERE pr_number=$1`, n)
		}},
		{name: "这个 head 被移出过队列", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded, setup: func(h *harness, n int, head string) {
			h.exec(`INSERT INTO merge_queue (repo_id, pr_number, head_sha, enqueued_by, state, reason, finished_at)
				VALUES ($1,$2,$3,'github:alice','removed','去掉了 kiln:merge label',now())`, repoID, n, head)
		}},
		{
			name: "待加 kiln:hold（按 head 分支）", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded,
			setup: func(h *harness, n int, _ string) {
				h.fake.UpdatePull(h.repo, n, func(p *fakegithub.Pull) { p.HeadRef = "kiln/dev-3" })
				h.hold("kiln/dev-3")
			},
		},
		{
			name: "待加 kiln:hold（按 PR 编号）", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded,
			setup: func(h *harness, n int, _ string) { h.hold(n) },
		},
		{name: "目标不是默认分支", yml: autoConfig, ci: store.RunSucceeded, ai: store.RunSucceeded, setup: func(h *harness, n int, _ string) {
			h.git("branch", "release")
			h.fake.UpdatePull(h.repo, n, func(p *fakegithub.Pull) { p.BaseRef = "release" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.commit(map[string]string{".kiln/merge.yml": tc.yml}, "配置")
			head := h.branch("feature")
			h.openAutoPR(7, "feature", head, tc.labels...)
			h.autoRuns(7, head, tc.ci, tc.ai)
			if tc.setup != nil {
				tc.setup(h, 7, head)
			}
			h.nominate(7)
			h.process()
			var queued bool
			if err := h.db.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM merge_queue WHERE pr_number=7 AND enqueued_by=$1)`, AutoActor).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued || slices.Contains(h.pull(7).Labels, Label) || h.pull(7).Merged || h.countAddLabel(7) != 0 {
				t.Fatalf("不应自动入队：PR %+v", h.pull(7))
			}
			if len(h.candidates()) != 0 {
				t.Errorf("候选应已丢掉：%v", h.candidates())
			}
		})
	}
}

// hold 记下 PR（编号或 head 分支）有待加的 kiln:hold。
func (h *harness) hold(key any) {
	h.holdsMu.Lock()
	defer h.holdsMu.Unlock()
	if h.holds == nil {
		h.holds = map[any]bool{}
	}
	h.holds[key] = true
}

// TestAutoEnqueueHoldSettled（PLAN #26.12）：只在 kiln:hold 待加时不自动入队；其他 PR 照常入队。
func TestAutoEnqueueHoldSettled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	head := h.branch("feature")
	n := 7
	h.hold(8)
	h.hold("other")
	h.openAutoPR(n, "feature", head)
	h.autoRuns(n, head, store.RunSucceeded, store.RunSucceeded)
	h.nominate(n)
	h.process()
	if e := h.wantState(n, StateMerged, ""); e.EnqueuedBy != AutoActor {
		t.Fatalf("应自动入队：%+v", e)
	}
}

// countAddLabel 返回给 PR n 加 label 的请求数。
func (h *harness) countAddLabel(n int) int {
	c := 0
	for _, r := range h.fake.Requests() {
		if strings.HasPrefix(r, fmt.Sprintf("POST /repos/alice/demo/issues/%d/labels", n)) {
			c++
		}
	}
	return c
}

// TestEnqueuedByBot：由 App 自己加 label 入队（例如自动入队加的 label 的 webhook 先到）时，自动打 tag 的发送者是 PR 作者，
// 不是 App（否则发布被当成机器人发起的事件）。
func TestEnqueuedByBot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.fake.SetPull(h.repo, fakegithub.Pull{Number: 7, Title: "PR", HeadRef: "feature", HeadSHA: head, Labels: []string{Label}})
	if _, err := h.svc.Enqueue(h.ctx, repoID, 7, head, "main", "github:kiln-test[bot]"); err != nil {
		t.Fatal(err)
	}
	h.verified(7, head, store.RunSucceeded)
	h.process()
	e := h.wantState(7, StateMerged, "")
	h.wantAutoTag(7, e.MergeSHA, testAccount, mqevents.AutoTagFromQueue, Label)
}

// TestAutoEnqueueLabelFailure：加 label 失败（暂时）时不入队、保留候选，下一次处理重试。
func TestAutoEnqueueLabelFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	head := h.branch("feature")
	h.openAutoPR(7, "feature", head)
	h.autoRuns(7, head, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)
	h.fake.SetFault("POST /repos/alice/demo/issues/7/labels", fakegithub.Fault{Status: 502})
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("加 label 失败时应返回错误")
	}
	if e, _ := Latest(h.ctx, h.db, repoID, 7); e != nil {
		t.Fatalf("加 label 失败时不应入队：%+v", e)
	}
	if got := h.candidates(); !slices.Equal(got, []int{7}) {
		t.Fatalf("候选应保留：%v", got)
	}
	h.process()
	h.wantState(7, StateMerged, "")
}

// TestNominateDuringCheck：检查候选期间同一个 PR 又被提名（另一个 run 刚结束）：只删读到的那一版，新的提名保留。
func TestNominateDuringCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	head := h.branch("feature")
	h.openAutoPR(7, "feature", head)
	h.autoRuns(7, head, store.RunSucceeded, store.RunActive)
	h.nominate(7)
	// 读 PR 时（已经读完候选）AI 审计结束并提名
	h.svc.forge = wrapForge(h.svc.forge, func(f forge.Repo) forge.Repo {
		return &nominateOnGetPull{Repo: f, fn: func() {
			h.setRuns(7, head, store.RunSucceeded)
			h.nominate(7)
		}}
	})
	h.process()
	if got := h.candidates(); !slices.Equal(got, []int{7}) {
		t.Fatalf("检查期间的新提名应保留：%v", got)
	}
	h.process()
	h.wantState(7, StateMerged, "")
}

// TestNominateQueued：PR 已经在队列中时不记候选。
func TestNominateQueued(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.branch("feature")
	h.openPR(7, "feature", head)
	h.nominate(7)
	if got := h.candidates(); len(got) != 0 {
		t.Fatalf("已在队列中的 PR 不应记候选：%v", got)
	}
}

// TestAutoEnqueueSweep：兜底检查也会处理留下的候选。
func TestAutoEnqueueSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(`INSERT INTO merge_auto_candidates (repo_id, pr_number, created_at) VALUES ($1, 9, $2)`, repoID, time.Now().Add(-time.Hour))
	h.exec(`DELETE FROM outbox`)
	if err := h.svc.sweep(h.ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.QueryRow(h.ctx, `SELECT count(*) FROM outbox WHERE kind='mergequeue.repo'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("应通知执行者：%d（%v）", n, err)
	}
}

// nominateOnGetPull 在第一次读取 PR 时执行 fn。
type nominateOnGetPull struct {
	forge.Repo
	fn func()
}

func (n *nominateOnGetPull) GetPull(ctx context.Context, number int) (*forge.Pull, error) {
	if fn := n.fn; fn != nil {
		n.fn = nil
		fn()
	}
	return n.Repo.GetPull(ctx, number)
}

// wrapForge 用 wrap 包装 f 返回的平台接口（同一个仓库多次调用返回同一个包装，保留状态）。
func wrapForge(f ForgeFunc, wrap func(forge.Repo) forge.Repo) ForgeFunc {
	var cached forge.Repo
	return func(r *store.Repo) forge.Repo {
		if cached == nil {
			cached = wrap(f(r))
		}
		return cached
	}
}

// TestAutoEnqueueAfterRemovedDuringUpdate（独立审查发现）：用户在队列更新分支期间把 PR 移出队列（记录的是旧 head），
// 之后队列推送的 merge commit 验证通过时不自动入队；用户自己推送的新 commit 照常自动入队。
func TestAutoEnqueueAfterRemovedDuringUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	old := h.branch("feature")
	h.exec(`INSERT INTO merge_queue (repo_id, pr_number, head_sha, enqueued_by, state, reason, finished_at)
		VALUES ($1,7,$2,'github:alice','removed','去掉了 kiln:merge label',now())`, repoID, old)
	// 队列更新分支：把 main 合并进 feature（两个父提交：旧 head 和 main）
	h.commit(map[string]string{"main.txt": "x\n"}, "main 前进")
	h.git("checkout", "-q", "feature")
	h.git("merge", "-q", "--no-edit", "main")
	merged := h.git("rev-parse", "HEAD")
	h.git("checkout", "-q", "main")
	h.openAutoPR(7, "feature", merged)
	h.autoRuns(7, merged, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)
	h.process()
	if e, _ := Latest(h.ctx, h.db, repoID, 7); e.State != StateRemoved || h.countAddLabel(7) != 0 {
		t.Fatalf("队列推送的 merge commit 不应自动入队：%+v", e)
	}
	// 用户在被移出的 head 上推送了新的 commit（一个父提交就是被移出的 head）：照常自动入队
	h.git("checkout", "-q", "-B", "feature", old)
	fix := h.commit(map[string]string{"fix.txt": "fix\n"}, "修复")
	h.git("checkout", "-q", "main")
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.HeadSHA = fix })
	h.autoRuns(7, fix, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)
	h.process()
	// 落后于 main：入队后由队列更新分支
	if e, _ := Latest(h.ctx, h.db, repoID, 7); e == nil || e.State != StateQueued || e.EnqueuedBy != AutoActor {
		t.Fatalf("用户推送新 commit 后应自动入队：%+v", e)
	}
}

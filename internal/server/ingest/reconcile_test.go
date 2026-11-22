package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
)

// openPull 是模拟的打开的 PR（模拟 GitHub 的 PR 列表总是空的，由测试拦截返回）。
type openPull struct {
	Number      int
	HeadRef     string
	HeadSHA     string
	BaseRef     string
	HeadRepoID  int64
	AuthorLogin string
}

// pullsServer 拦截 GET /repos/{repo}/pulls，返回 pulls 中的 PR。
type pullsServer struct {
	mu    sync.Mutex
	repo  string
	pulls []openPull
}

func (p *pullsServer) set(pulls ...openPull) {
	p.mu.Lock()
	p.pulls = pulls
	p.mu.Unlock()
}

func (p *pullsServer) intercept(baseRepoID int64) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/"+p.repo+"/pulls" {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		list := []any{}
		if r.URL.Query().Get("page") == "1" {
			for _, pr := range p.pulls {
				list = append(list, map[string]any{
					"number": pr.Number, "state": "open",
					"head": map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA, "repo": map[string]any{"id": pr.HeadRepoID}},
					"base": map[string]any{"ref": pr.BaseRef, "sha": gitref.ZeroSHA, "repo": map[string]any{"id": baseRepoID}},
					"user": map[string]any{"login": pr.AuthorLogin, "type": "User"},
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
		return true
	}
}

// knownRepos 让 Kiln 通过 installation.created 事件记录默认 installation 的仓库（不产生 ref_heads）。
func (h *harness) knownRepos() {
	h.t.Helper()
	h.deliverOK("installation", h.fake.InstallationPayload("created", h.fake.InstallationID))
}

// baselineDone 返回仓库是否已经完成第一次对账。
func (h *harness) baselineDone(id int64) bool {
	h.t.Helper()
	return h.repo(id).BaselineDone
}

// TestReconcileRepo：第一次对账只记录基线、不构建；之后补上漏掉的分支推送、新 tag 和 PR 推送，
// fork 来的 PR 丢弃；没有变化时不做任何事（PLAN #7.3）。
func TestReconcileRepo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pulls := &pullsServer{repo: "tester/demo"}
	h.setIntercept(pulls.intercept(demoRepoID))
	h.knownRepos()

	main1 := h.git.head("main")
	h.git.git("tag", "-a", "v1", "-m", "v1")
	h.git.git("checkout", "-q", "-b", "feature")
	pr1 := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature 1")
	h.git.git("checkout", "-q", "main")
	v1Obj := h.git.git("rev-parse", "v1")
	pulls.set(
		openPull{Number: 4, HeadRef: "feature", HeadSHA: pr1, BaseRef: "main", HeadRepoID: demoRepoID, AuthorLogin: "alice"},
		openPull{Number: 5, HeadRef: "evil", HeadSHA: pr1, BaseRef: "main", HeadRepoID: 999, AuthorLogin: "mallory"},
	)

	res, err := h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if err != nil {
		t.Fatal(err)
	}
	want := ReconcileResult{Repo: "tester/demo", Baseline: true, Branches: 2, Tags: 1, Pulls: 2}
	if res != want {
		t.Fatalf("第一次对账 = %+v，期望 %+v", res, want)
	}
	if !h.baselineDone(demoRepoID) || len(h.runs(demoRepoID)) != 0 {
		t.Fatal("第一次对账应记录基线、不构建")
	}
	for ref, sha := range map[string]string{"refs/heads/main": main1, "refs/heads/feature": pr1, "refs/tags/v1": main1, "refs/pull/4/head": pr1} {
		if got, _, ok := h.refHead(demoRepoID, ref); !ok || got != sha {
			t.Errorf("基线 %s = %q，期望 %s", ref, got, sha)
		}
	}
	if _, obj, _ := h.refHead(demoRepoID, "refs/tags/v1"); obj != v1Obj {
		t.Errorf("附注 tag 的 object_sha = %s，期望 tag 对象 %s", obj, v1Obj)
	}
	if _, _, ok := h.refHead(demoRepoID, "refs/pull/5/head"); ok {
		t.Error("fork 来的 PR 不应记录")
	}
	if base := h.refBase(demoRepoID, "refs/pull/4/head"); base != "" {
		t.Errorf("基线没有规划 PR，base_ref = %q，期望为空", base)
	}

	// 漏掉的事件：main 有新 commit、新的 tag v2、v1 被强制移动、PR #4 有新的推送
	main2 := h.git.commit(map[string]string{"a.txt": "a\n"}, "main 2")
	h.git.git("tag", "v2")
	h.git.git("tag", "-f", "-a", "v1", "-m", "moved") // tag 被强制移动
	h.git.git("checkout", "-q", "feature")
	pr2 := h.git.commit(map[string]string{"f.txt": "2\n"}, "feature 2")
	h.git.git("checkout", "-q", "main")
	pulls.set(openPull{Number: 4, HeadRef: "feature", HeadSHA: pr2, BaseRef: "main", HeadRepoID: demoRepoID, AuthorLogin: "alice"})

	res, err = h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if err != nil {
		t.Fatal(err)
	}
	// feature 分支的推送不匹配 ci.yml（只有 main），不产生 run，但 ref_heads 照常更新
	want = ReconcileResult{Repo: "tester/demo", Branches: 2, Tags: 2, Pulls: 1, Runs: 4}
	if res != want {
		t.Fatalf("第二次对账 = %+v，期望 %+v", res, want)
	}
	got := map[string]string{}
	for _, r := range h.runs(demoRepoID) {
		if r.Source != planner.SourceReconcile {
			t.Errorf("run %d 的来源 %s，期望 reconcile", r.ID, r.Source)
		}
		got[r.Ref] = r.SHA
	}
	wantRuns := map[string]string{"refs/heads/main": main2, "refs/tags/v1": main2, "refs/tags/v2": main2, "refs/pull/4/head": pr2}
	for ref, sha := range wantRuns {
		if got[ref] != sha {
			t.Errorf("对账补上的 run：%s = %q，期望 %s（全部 %v）", ref, got[ref], sha, got)
		}
	}
	if sha, _, _ := h.refHead(demoRepoID, "refs/heads/feature"); sha != pr2 {
		t.Errorf("feature 分支的 ref_heads = %s，期望 %s", sha, pr2)
	}
	if base := h.refBase(demoRepoID, "refs/pull/4/head"); base != "main" {
		t.Errorf("补上规划后 PR 的 base_ref = %q，期望 main", base)
	}

	// 漏掉的 edited 事件：PR #4 的 head 没变，目标分支改成了 develop（不匹配 ci.yml，不产生 run），照样重新规划
	pulls.set(openPull{Number: 4, HeadRef: "feature", HeadSHA: pr2, BaseRef: "develop", HeadRepoID: demoRepoID, AuthorLogin: "alice"})
	if _, err := h.ing.ReconcileRepo(h.ctx, demoRepoID); err != nil {
		t.Fatal(err)
	}
	if base := h.refBase(demoRepoID, "refs/pull/4/head"); base != "develop" {
		t.Errorf("改了目标分支后 PR 的 base_ref = %q，期望 develop", base)
	}

	// 再次对账：没有变化
	res, err = h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 0 || res.Baseline {
		t.Fatalf("没有变化时的对账 = %+v", res)
	}
	if n := len(h.runs(demoRepoID)); n != 4 {
		t.Fatalf("没有变化时又产生了 run：共 %d 个", n)
	}
}

// TestReconcileBaselineComparesKnownRefs：第一次对账时，已经由事件记录过的 ref 照常比较，变化了就补上。
func TestReconcileBaselineComparesKnownRefs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	main1 := h.git.head("main")
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, main1))
	main2 := h.git.commit(map[string]string{"a.txt": "a\n"}, "missed")
	h.git.git("tag", "v0.1") // 历史 tag：只记录基线，不发布

	res, err := h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Baseline || res.Runs != 1 {
		t.Fatalf("对账 = %+v，期望第一次对账、补上 1 个 run", res)
	}
	if r := h.lastRun(demoRepoID, 2); r.Ref != "refs/heads/main" || r.SHA != main2 {
		t.Fatalf("补上的 run = %+v", r)
	}
	if sha, _, ok := h.refHead(demoRepoID, "refs/tags/v0.1"); !ok || sha != main2 {
		t.Fatal("历史 tag 应记录为基线")
	}
}

// TestReconcilePlanFailure：规划失败（非限流）只记录，不更新 ref_heads，下一轮重试；对账本身不失败。
func TestReconcilePlanFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	main1 := h.git.head("main")
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, main1))
	main2 := h.git.commit(map[string]string{"a.txt": "a\n"}, "missed")
	h.resetGitHubClient() // 清空 planner 的缓存
	h.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/git/trees/"+main2) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return true
		}
		return false
	})
	res, err := h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if err != nil {
		t.Fatalf("规划失败不应让对账失败：%v", err)
	}
	if res.Runs != 0 {
		t.Fatalf("对账 = %+v，期望没有补上 run", res)
	}
	if sha, _, _ := h.refHead(demoRepoID, "refs/heads/main"); sha != main1 {
		t.Fatalf("规划失败时 ref_heads 被更新为 %s", sha)
	}
	// 恢复后下一轮补上
	h.setIntercept(nil)
	if res, err := h.ing.ReconcileRepo(h.ctx, demoRepoID); err != nil || res.Runs != 1 {
		t.Fatalf("恢复后的对账 = %+v, %v，期望补上 1 个 run", res, err)
	}
}

// TestReconcileRateLimited：对账被限流时返回限流错误，已经补上的部分不会重复。
func TestReconcileRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.knownRepos()
	h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: time.Now().Add(time.Hour)})
	_, err := h.ing.ReconcileRepo(h.ctx, demoRepoID)
	if _, ok := gh.RateLimited(err); !ok {
		t.Fatalf("错误 %v，期望限流错误", err)
	}
	if h.baselineDone(demoRepoID) {
		t.Fatal("没有对账完时不应记录基线完成")
	}
}

// TestReconcileAll：周期对账只处理 ReconcileRepos 范围内的仓库；某个 installation 被限流或剩余配额不足时，
// 本轮跳过它的其余仓库，其他 installation 照常（PLAN #7.3）。
func TestReconcileAll(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, repos ...string) *harness {
		t.Helper()
		h := newHarness(t, func(o *harnessOpts) { o.cfg.ReconcileRepos = repos })
		h.addRepo(202, "other", 0, true)
		h.fake.AddInstallation(778)
		h.addRepo(303, "b1", 778, true)
		h.knownRepos()
		h.deliverOK("installation", h.fake.InstallationPayload("created", 778))
		return h
	}
	baselines := func(h *harness) []bool {
		return []bool{h.baselineDone(demoRepoID), h.baselineDone(202), h.baselineDone(303)}
	}

	t.Run("指定仓库", func(t *testing.T) {
		t.Parallel()
		h := setup(t, "tester/other")
		h.ing.reconcileAll(h.ctx)
		if got := baselines(h); !slices.Equal(got, []bool{false, true, false}) {
			t.Fatalf("完成基线的仓库 %v，期望只有 tester/other", got)
		}
	})
	t.Run("全部仓库", func(t *testing.T) {
		t.Parallel()
		h := setup(t, reconcileAllRepos)
		h.ing.reconcileAll(h.ctx)
		if got := baselines(h); !slices.Equal(got, []bool{true, true, true}) {
			t.Fatalf("完成基线的仓库 %v，期望全部", got)
		}
	})
	t.Run("被限流的 installation 跳过其余仓库", func(t *testing.T) {
		t.Parallel()
		h := setup(t, reconcileAllRepos)
		h.fake.SetRateLimit(h.fake.InstallationID, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: time.Now().Add(time.Hour)})
		h.ing.reconcileAll(h.ctx)
		if got := baselines(h); !slices.Equal(got, []bool{false, false, true}) {
			t.Fatalf("完成基线的仓库 %v，期望只有另一个 installation 的仓库", got)
		}
		if c := h.fake.RequestCount(h.fake.InstallationID); c.Limited != 1 {
			t.Fatalf("被限流后又发出了请求：%+v", c)
		}
		// 下一轮：仍在限流中，不发请求
		h.ing.reconcileAll(h.ctx)
		if c := h.fake.RequestCount(h.fake.InstallationID); c.Total != 1 {
			t.Fatalf("限流中的 installation 不应再发请求：%+v", c)
		}
	})
	t.Run("剩余配额不足时让路", func(t *testing.T) {
		t.Parallel()
		h := setup(t, reconcileAllRepos)
		// 每个响应都报告只剩 1% 的配额：第一个仓库对账完后，同一 installation 的其余仓库本轮跳过
		reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		h.setResponseHeaders(http.Header{"X-Ratelimit-Limit": {"5000"}, "X-Ratelimit-Remaining": {"50"}, "X-Ratelimit-Reset": {reset}})
		h.ing.reconcileAll(h.ctx)
		got := baselines(h)
		if !got[0] || got[1] {
			t.Fatalf("完成基线的仓库 %v，期望默认 installation 只对账第一个仓库", got)
		}
	})
}

// TestRunReconcile：周期对账默认关闭（直接返回）；开启后按间隔对账范围内的仓库，ctx 结束后返回。
func TestRunReconcile(t *testing.T) {
	t.Parallel()
	t.Run("默认关闭", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ing.RunReconcile(h.ctx)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("周期对账关闭时 RunReconcile 应直接返回")
		}
	})
	t.Run("开启", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(o *harnessOpts) {
			o.cfg.ReconcileInterval, o.cfg.ReconcileRepos = time.Hour, []string{"tester/demo"}
		})
		h.knownRepos()
		ctx, cancel := context.WithCancel(h.ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ing.RunReconcile(ctx)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for !h.baselineDone(demoRepoID) {
			if time.Now().After(deadline) {
				t.Fatal("开启周期对账后 10 秒内没有对账")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ctx 结束后 RunReconcile 没有返回")
		}
	})
}

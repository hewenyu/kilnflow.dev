package aidev

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// TestSelectTools：开发配置声明了 requirements.tools 时，选题任务带着按默认分支上 .kiln/runtimes.lock 解析出的工具链
// （开发平台，PLAN #33.12）；没有 lockfile 时不规划，原因说明配置错误（与开发配置的其它错误相同：修好后手动同步任务池再选）。
func TestSelectTools(t *testing.T) {
	h := newHarness(t, strings.Replace(devConfigYML, "version: 1\n", "version: 1\nrequirements: {tools: [node, pnpm]}\n", 1))
	h.issue(1, "需求", "x")
	res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob")
	if err != nil || res.RequestID != 0 || !strings.Contains(res.Reason, "工具链配置有错误") || !strings.Contains(res.Reason, "仓库中没有 .kiln/runtimes.lock") {
		t.Fatalf("没有 lockfile：%+v, %v", res, err)
	}
	nodeDigest, pnpmDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	h.commitOn("main", map[string]string{toolchain.LockPath: "[node]\nversion = \"24.11.0\"\n\"darwin/arm64\" = \"" + nodeDigest + "\"\n" +
		"[pnpm]\nversion = \"12.14.0\"\n\"darwin/arm64\" = \"" + pnpmDigest + "\"\n"}, "锁定工具链")
	if _, err := h.svc.SyncPool(h.ctx, testRepoID, "bob"); err != nil {
		t.Fatal(err)
	}
	spec := h.specOf(h.taskOf(h.startSelect()).ID)
	want := []aispec.ToolRef{
		{Name: "node", Version: "24.11.0", Platform: "darwin/arm64", Digest: nodeDigest},
		{Name: "pnpm", Version: "12.14.0", Platform: "darwin/arm64", Digest: pnpmDigest},
	}
	if got := spec.PlatformTools[testDevPlatform]; len(spec.PlatformTools) != 1 || !slices.Equal(got, want) {
		t.Fatalf("工具链 = %+v，期望只有 %s 的 %+v", spec.PlatformTools, testDevPlatform, want)
	}
}

// TestDevRepositories：开发任务的依赖仓库（requirements.repositories，PLAN #33.13）与开发配置一样从默认分支读取：选题带着
// 规划时默认分支上的声明，之后默认分支改了声明，下一轮返修按新的声明。
func TestDevRepositories(t *testing.T) {
	h := newHarness(t, strings.Replace(devConfigYML, "version: 1\n", "version: 1\nrequirements: {repositories: [tester/lib]}\n", 1))
	h.issue(1, "加功能", "要做 a")
	sel := h.taskOf(h.startSelect())
	if spec := h.specOf(sel.ID); !slices.Equal(spec.Repositories, []string{"tester/lib"}) {
		t.Fatalf("选题的依赖仓库 = %v", spec.Repositories)
	}
	aid := h.acquire(sel.ID)
	res := h.mustClaim(aid, []int{1})
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"feature.go": "package demo\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.mustProcess()
	w := h.wantState(res.WorkItemID, StatePROpen)
	pr := *w.PRNumber
	h.commitOn("main", map[string]string{".kiln/agent/config.yml": strings.Replace(devConfigYML, "version: 1\n",
		"version: 1\nrequirements: {repositories: [tester/lib, tester/proto]}\n", 1)}, "多一个依赖仓库")
	h.finishPR(pr)
	h.failingRun(pr, w.HeadSHA)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	if spec := h.specOf(h.fixTask(w).ID); !slices.Equal(spec.Repositories, []string{"tester/lib", "tester/proto"}) {
		t.Fatalf("返修的依赖仓库 = %v，期望默认分支上的新声明", spec.Repositories)
	}
}

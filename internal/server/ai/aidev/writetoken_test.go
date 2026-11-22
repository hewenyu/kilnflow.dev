package aidev

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 交付的写凭据（PLAN #26.10）：推进正式分支只签 contents: write，写 PR
// 只签 pull_requests: write（不含 contents），都只覆盖任务仓库，返回或出错后都已吊销；没有写操作时不签发。fakegithub 按
// token 的仓库和权限校验写请求（用错 token 的写会被 403 拒绝），断言落在真实的校验上。

const (
	routeCreateRef  = "POST /repos/alice/demo/git/refs"
	routeCreatePull = "POST /repos/alice/demo/pulls"
)

var (
	contentsWrite = map[string]string{permContents: permWrite}
	pullsWrite    = map[string]string{permPullRequests: permWrite}
)

// writeTokens 返回签发过的限定了权限的 token（共用客户端缓存的完整权限 token 不算）。
func (h *harness) writeTokens() []fakegithub.Token {
	var out []fakegithub.Token
	for _, tk := range h.fake.IssuedTokens() {
		if tk.Permissions != nil {
			out = append(out, tk)
		}
	}
	return out
}

// wantWriteTokens 检查限定权限的 token 依次是 perms（只覆盖任务仓库），且都已吊销。
func (h *harness) wantWriteTokens(perms ...map[string]string) {
	h.t.Helper()
	toks := h.writeTokens()
	if len(toks) != len(perms) {
		h.t.Fatalf("签发了 %d 枚写 token（%+v），期望 %d 枚", len(toks), toks, len(perms))
	}
	for i, tk := range toks {
		if !maps.Equal(tk.Permissions, perms[i]) || !slices.Equal(tk.RepoIDs, []int64{testRepoID}) {
			h.t.Fatalf("第 %d 枚 token 的权限 %v、仓库 %v，期望 %v、[%d]", i+1, tk.Permissions, tk.RepoIDs, perms[i], testRepoID)
		}
		if !tk.Revoked {
			h.t.Fatalf("第 %d 枚 token（%v）没有吊销", i+1, tk.Permissions)
		}
	}
}

// TestPromoteWriteTokens：推进时先签 contents: write 创建正式分支、返回后吊销，再签 pull_requests: write 建 PR、加 label，
// 结束后吊销；建 PR 时写分支的 token 已经吊销。之后的处理没有写操作，不再签发。
func TestPromoteWriteTokens(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	// 改动 AGENTS.md：PR 要加 kiln:hold，label 用同一枚 PR token
	w, _, sha := h.deliver(map[string]string{"AGENTS.md": "x\n"}, 1)
	var atCreate []fakegithub.Token
	h.fake.SetFault(routeCreatePull, fakegithub.Fault{Pause: func() { atCreate = h.writeTokens() }})
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if h.branchHead(w.Branch()) != sha {
		t.Fatalf("正式分支 = %s", h.branchHead(w.Branch()))
	}
	if pr, _ := h.fake.GetPull(h.repo, *w.PRNumber); !slices.Contains(pr.Labels, h.svc.holdLabel) {
		t.Fatalf("PR 的 label = %v", pr.Labels)
	}
	h.wantWriteTokens(contentsWrite, pullsWrite)
	if len(atCreate) != 2 || !atCreate[0].Revoked || atCreate[1].Revoked {
		t.Fatalf("建 PR 时 token = %+v：写分支的应已吊销、PR 的应仍有效", atCreate)
	}
	h.mustProcess()
	h.wantWriteTokens(contentsWrite, pullsWrite)
}

// TestPromoteWriteTokenErrors：平台出错（没有生效）时这组写操作的 token 照样吊销，本轮推进返回错误，下一轮重新签发；
// 写分支失败时不签 PR 的 token。签发本身失败时本轮不重试（只请求一次），下一轮照常推进。
func TestPromoteWriteTokenErrors(t *testing.T) {
	tests := []struct {
		name  string
		fault func(h *harness) string
		// first 是出错的那一轮签发的 token，then 是下一轮签发的
		first, then []map[string]string
	}{
		{"建分支失败", func(*harness) string { return routeCreateRef }, []map[string]string{contentsWrite}, []map[string]string{contentsWrite, pullsWrite}},
		{"建 PR 失败", func(*harness) string { return routeCreatePull }, []map[string]string{contentsWrite, pullsWrite}, []map[string]string{pullsWrite}},
		{"签发失败", func(h *harness) string { return fakegithub.CreateTokenRoute(h.fake.InstallationID) }, nil, []map[string]string{contentsWrite, pullsWrite}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			// 共用客户端的完整权限 token 先签好（读分支时），注入的签发失败只落在交付的写 token 上
			if _, err := h.svc.forge(h.repoRow()).BranchHead(h.ctx, "main"); err != nil {
				t.Fatal(err)
			}
			route := tc.fault(h)
			before := h.countRequests(fakegithub.CreateTokenRoute(h.fake.InstallationID))
			h.fake.SetFault(route, fakegithub.Fault{Status: http.StatusBadGateway})
			if err := h.process(); err == nil {
				t.Fatal("出错时没有返回错误")
			}
			h.wantState(w.ID, StateDelivering)
			h.wantWriteTokens(tc.first...)
			if n := h.countRequests(fakegithub.CreateTokenRoute(h.fake.InstallationID)) - before; n != max(len(tc.first), 1) {
				t.Fatalf("出错的一轮请求签发 %d 次", n)
			}
			h.mustProcess()
			h.wantState(w.ID, StatePROpen)
			h.wantWriteTokens(append(slices.Clone(tc.first), tc.then...)...)
		})
	}
}

// TestPromoteWriteTokenRejected：写 PR 时平台返回 401 / 403（token 被拒绝），本轮返回错误，不换一枚 token 重发这个写操作，
// 这枚 token 照样吊销；下一轮按最新状态重新签发。
func TestPromoteWriteTokenRejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			h.fake.SetFault(routeCreatePull, fakegithub.Fault{Status: status})
			if err := h.process(); err == nil {
				t.Fatal("PR 的写操作被拒绝时没有返回错误")
			}
			h.wantState(w.ID, StateDelivering)
			h.wantWriteTokens(contentsWrite, pullsWrite)
			if n := h.countRequests(routeCreatePull); n != 1 {
				t.Fatalf("建 PR 请求了 %d 次，期望 1 次（不换 token 重发）", n)
			}
			h.mustProcess()
			h.wantState(w.ID, StatePROpen)
			h.wantWriteTokens(contentsWrite, pullsWrite, pullsWrite)
		})
	}
}

// TestPromoteWriteTokenCanceled：写分支的请求在途时本轮的 context 被取消，这枚 token 仍然吊销（吊销不随本轮处理取消）。
func TestPromoteWriteTokenCanceled(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.fake.SetFault(routeCreateRef, fakegithub.Fault{Pause: cancel})
	if err := h.svc.Process(ctx, testRepoID); err == nil {
		t.Fatal("取消后没有返回错误")
	}
	if ctx.Err() == nil {
		t.Fatal("写分支的请求没有发出")
	}
	h.wantWriteTokens(contentsWrite)
}

// TestPromoteWriteTokenPanic：签发之后构造平台接口时 panic，这枚 token 仍然吊销。
func TestPromoteWriteTokenPanic(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "一", "a")
	h.deliver(map[string]string{"a.go": "package a\n"}, 1)
	h.svc.tokenForge = func(*store.Repo, string) forge.Repo { panic("构造平台接口失败") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("没有 panic")
			}
		}()
		_ = h.process()
	}()
	h.wantWriteTokens(contentsWrite)
}

// TestPromoteLostPullWriteTokens：建 PR 的响应丢失后下一轮按分支找到它、只更新描述，只有一个 PR；响应丢失后 PR 被人关闭时
// 下一轮不重建，没有写操作、不签发。每一轮的 token 都已吊销。
func TestPromoteLostPullWriteTokens(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "下一轮找到它"
		if closed {
			name = "被人关闭后不重建"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, devConfigYML)
			h.issue(1, "一", "a")
			w, _, _ := h.deliver(map[string]string{"a.go": "package a\n"}, 1)
			h.fake.SetFault(routeCreatePull, fakegithub.Fault{Lost: true})
			if err := h.process(); err == nil {
				t.Fatal("响应丢失时没有返回错误")
			}
			h.wantWriteTokens(contentsWrite, pullsWrite)
			want, then := StatePROpen, []map[string]string{contentsWrite, pullsWrite, pullsWrite}
			if closed {
				// Issue #1 之后的第一个编号是 PR #2
				h.fake.UpdatePull(h.repo, 2, func(p *fakegithub.Pull) { p.State = "closed" })
				want, then = StateStopped, []map[string]string{contentsWrite, pullsWrite}
			}
			h.mustProcess()
			h.wantState(w.ID, want)
			h.wantWriteTokens(then...)
			if n := h.countRequests(routeCreatePull); n != 1 {
				t.Fatalf("建 PR 请求了 %d 次，期望 1 次", n)
			}
			pulls, err := h.svc.forge(h.repoRow()).PullsByHead(h.ctx, w.Branch())
			if err != nil || len(pulls) != 1 || pulls[0].Number != 2 {
				t.Fatalf("正式分支上的 PR = %+v（%v），期望只有 #2", pulls, err)
			}
		})
	}
}

// TestFixWriteTokens：返修快进正式分支（contents: write）、只更新原 PR 的描述（pull_requests: write），不建新的 PR。
func TestFixWriteTokens(t *testing.T) {
	h := newHarness(t, devConfigYML)
	h.issue(1, "加功能", "要做 a")
	w, pr := h.developed(1)
	h.wantWriteTokens(contentsWrite, pullsWrite)
	head := w.HeadSHA
	h.failingRun(pr, head)
	h.mustProcess()
	w = h.wantState(w.ID, StateFixing)
	aid := h.acquire(h.fixTask(w).ID)
	fix := h.push(devreport.AttemptBranch(aid), head, map[string]string{"feature.go": "package demo\n\n// 修了\n"})
	fid := h.specOf(h.fixTask(w).ID).Findings[0].ID
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered([]int{1}, fix, fid), Branch: devreport.AttemptBranch(aid), SHA: fix})
	editPull := "PATCH /repos/alice/demo/pulls/" + strconv.Itoa(pr)
	edits := h.countRequests(editPull)
	h.mustProcess()
	w = h.wantState(w.ID, StatePROpen)
	if *w.PRNumber != pr || h.branchHead(w.Branch()) != fix {
		t.Fatalf("返修后 PR #%d、正式分支 %s", *w.PRNumber, h.branchHead(w.Branch()))
	}
	h.wantWriteTokens(contentsWrite, pullsWrite, contentsWrite, pullsWrite)
	if n := h.countRequests(routeCreatePull); n != 1 {
		t.Fatalf("建 PR 请求了 %d 次，返修不应再建", n)
	}
	if n := h.countRequests(editPull) - edits; n != 1 {
		t.Fatalf("返修更新原 PR 的描述 %d 次，期望 1 次", n)
	}
}

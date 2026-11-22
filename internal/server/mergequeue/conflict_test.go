package mergequeue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 合并冲突按 PR 的归属处理（PLAN #28.3、C23）：平台回答冲突且 mergeability 确认冲突时，AI 开发的工作项负责的 PR 在出队的
// 同一个事务中交回返修，其余的出队、请提交者解决；mergeability 还没有计算完成、权限不足等不交给 AI 开发。

// conflictPR 制造与 main 冲突的 PR n（feature 与 main 改同一行），验证通过、排在队列中，返回 PR 的 head。
func (h *harness) conflictPR(n int, branch string) string {
	h.t.Helper()
	h.git("checkout", "-q", "-b", branch, "main")
	head := h.commit(map[string]string{"README.md": branch + "\n"}, branch)
	h.git("checkout", "-q", "main")
	h.commit(map[string]string{"README.md": "main 的改动 " + branch + "\n"}, "别的 PR 合并："+branch)
	h.openPR(n, branch, head)
	h.verified(n, head, store.RunSucceeded)
	return head
}

// gitErr 执行 git，返回输出和错误（不因失败结束测试）。
func (h *harness) gitErr(args ...string) (string, error) {
	cmd := exec.CommandContext(context.WithoutCancel(h.ctx), "git", args...)
	cmd.Dir = h.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commitResolved 写入解决冲突后的文件并完成正在进行的合并（merge commit）。
func (h *harness) commitResolved(files map[string]string) {
	h.t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(h.dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.git("add", "-A")
	h.git("commit", "-q", "--no-edit")
}

// TestConflictHandBack：AI 开发的工作项负责的 PR 冲突：去掉 label、评论一次（说明已交给 AI 开发、不需要手动操作），出队
// 记为 conflict_dev 并在同一个事务中交回（冲突的 head、默认分支的 head、PR 的分支）；重复处理不再交回。
func TestConflictHandBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	head := h.conflictPR(7, "kiln/dev-3")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 3, State: "pr_open"})

	h.process()
	e := h.wantState(7, StateRemoved, "已交给 AI 开发返修（工作项 #3）")
	base := h.git("rev-parse", "main")
	if e.Removal != RemovalConflictDev || e.ConflictBase != base {
		t.Fatalf("出队记录：removal=%q base=%q", e.Removal, e.ConflictBase)
	}
	want := []DevConflict{{RepoID: repoID, PR: 7, HeadRef: "kiln/dev-3", Head: head, BaseRef: "main", Base: base}}
	if got := h.dev.backs(); !slices.Equal(got, want) {
		t.Fatalf("交回 = %+v，想要 %+v", got, want)
	}
	p := h.pull(7)
	if len(p.Comments) != 1 || slices.Contains(p.Labels, Label) || p.Updates != 0 ||
		!strings.Contains(p.Comments[0], "AI 开发会把目标分支合并进来") || strings.Contains(p.Comments[0], "重新加上") {
		t.Fatalf("PR = %+v", p)
	}
	h.process()
	if len(h.dev.backs()) != 1 || len(h.pull(7).Comments) != 1 {
		t.Fatalf("重复处理后交回 %d 次、评论 %d 条", len(h.dev.backs()), len(h.pull(7).Comments))
	}
}

// TestConflictHandBackAtomic：交回失败时出队一起回滚（记下的意图保留，评论不重发）；重试时按记下的意图完成出队和交回。
// 工作项期间结束（HandBack 返回 false）时出队改记为等待人处理。
func TestConflictHandBackAtomic(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.conflictPR(7, "kiln/dev-3")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 3})
	h.dev.fail = errors.New("数据库断开")
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("交回失败时应返回错误（稍后重试）")
	}
	e := h.entry(7)
	if e.State != StateQueued || e.Action != actionRemoving || e.Removal != RemovalConflictDev || len(h.dev.backs()) != 0 {
		t.Fatalf("交回失败后：%+v", e)
	}
	// 重试：PR 已经没有 label、评论已经发过，按记下的意图完成
	h.process()
	if e := h.wantState(7, StateRemoved, "已交给 AI 开发"); e.Removal != RemovalConflictDev || len(h.dev.backs()) != 1 ||
		len(h.pull(7).Comments) != 1 {
		t.Fatalf("重试后：%+v，交回 %v，评论 %d", e, h.dev.backs(), len(h.pull(7).Comments))
	}

	h2 := newHarness(t)
	h2.conflictPR(8, "kiln/dev-4")
	h2.dev.setOwner(8, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 4})
	h2.dev.refuse = true
	h2.process()
	if e := h2.wantState(8, StateRemoved, "不自动返修，需要人处理"); e.Removal != RemovalConflictAuthor {
		t.Fatalf("工作项期间结束：%+v", e)
	}
}

// TestConflictOwners：负责的工作项已经结束、或者 PR 的目标分支不是工作项的目标分支时出队、说明需要人处理（不复活工作项、
// 不交回）；工作项还没记下 PR 编号时等待（不出队、不评论），记下之后再次处理时交回。
func TestConflictOwners(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.conflictPR(7, "kiln/dev-3")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerEnded, WorkItem: 3, State: "stopped"})
	h.process()
	if e := h.wantState(7, StateRemoved, "工作项 #3 已经结束（stopped）"); e.Removal != RemovalConflictAuthor || len(h.dev.backs()) != 0 {
		t.Fatalf("结束的工作项：%+v，交回 %v", e, h.dev.backs())
	}

	// 工作项未结束，但 PR 改投了别的目标分支（工作项按 release 开始）：交回会让 AI 开发合并旧的目标分支，不交回、请人处理
	h3 := newHarness(t)
	h3.conflictPR(9, "kiln/dev-5")
	h3.dev.setOwner(9, DevOwner{Kind: DevOwnerActive, BaseRef: "release", WorkItem: 5})
	h3.process()
	if e := h3.wantState(9, StateRemoved, "PR 的目标分支已从 AI 开发工作项 #5 的目标分支 release 改为 main，不自动返修，需要人处理"); e.Removal != RemovalConflictAuthor ||
		len(h3.dev.backs()) != 0 {
		t.Fatalf("目标分支不同的工作项：%+v，交回 %v", e, h3.dev.backs())
	}
	if p := h3.pull(9); len(p.Comments) != 1 || strings.Contains(p.Comments[0], "AI 开发会把目标分支合并进来") {
		t.Fatalf("目标分支不同时的评论：%+v", p.Comments)
	}

	h2 := newHarness(t)
	h2.conflictPR(8, "kiln/dev-4")
	h2.dev.setOwner(8, DevOwner{Kind: DevOwnerRecording, WorkItem: 4})
	h2.process()
	h2.wantStatus(8, "等待 AI 开发记下 PR 归属")
	if p := h2.pull(8); len(p.Comments) != 0 || !slices.Contains(p.Labels, Label) {
		t.Fatalf("等待归属时：%+v", p)
	}
	h2.dev.setOwner(8, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 4})
	h2.process()
	if e := h2.wantState(8, StateRemoved, "已交给 AI 开发返修"); e.Removal != RemovalConflictDev || len(h2.dev.backs()) != 1 {
		t.Fatalf("记下归属后：%+v", e)
	}
}

// TestConflictMergeabilityUnknown：平台回答冲突、PR 的 mergeability 却还没有计算完成时不算冲突：不出队、不交回，保留更新
// 意图稍后重新请求（计入次数）；一直没有确认时按更新超时出队（不交给 AI 开发）。
func TestConflictMergeabilityUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.conflictPR(7, "kiln/dev-3")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 3})
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Mergeability = fakegithub.MergeabilityUnknown })
	for i := 1; i <= maxUpdateTries; i++ {
		if err := h.svc.Process(h.ctx, repoID); err == nil {
			t.Fatalf("第 %d 次：等待计算可合并状态时应稍后重试", i)
		}
		e := h.entry(7)
		if e.State != StateQueued || e.ActionTries != i || e.ActionAt != nil || !strings.Contains(e.Status, "可合并状态是 unknown") {
			t.Fatalf("第 %d 次：%+v", i, e)
		}
	}
	h.process()
	e := h.wantState(7, StateRemoved, "更新 PR 分支 3 次都没有完成")
	if e.Removal != "" || len(h.dev.backs()) != 0 {
		t.Fatalf("可合并状态一直不确定：removal=%q，交回 %v", e.Removal, h.dev.backs())
	}
}

// TestConflictMergeabilityResolves：可合并状态确认之后（第二次请求时 GitHub 已经算完）按归属交回。
func TestConflictMergeabilityResolves(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.conflictPR(7, "kiln/dev-3")
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 3})
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Mergeability = fakegithub.MergeabilityUnknown })
	if err := h.svc.Process(h.ctx, repoID); err == nil {
		t.Fatal("等待计算可合并状态时应稍后重试")
	}
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.Mergeability = nil }) // 按 git 计算：dirty
	h.process()
	if e := h.wantState(7, StateRemoved, "已交给 AI 开发"); e.Removal != RemovalConflictDev || len(h.dev.backs()) != 1 {
		t.Fatalf("确认冲突后：%+v", e)
	}
}

// TestAutoEnqueueAfterConflictFix：冲突交回 AI 开发后，AI 把 main 合并进被移出的 head 得到的 merge commit 验证通过时自动
// 入队（removedBefore 的父提交一条不适用于 conflict_dev）；被移出的冲突 head 本身不再自动入队。
func TestAutoEnqueueAfterConflictFix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.commit(map[string]string{".kiln/merge.yml": autoConfig}, "开启 auto")
	h.git("checkout", "-q", "-b", "kiln/dev-3", "main")
	head := h.commit(map[string]string{"README.md": "feature\n"}, "AI 开发")
	h.git("checkout", "-q", "main")
	h.commit(map[string]string{"README.md": "main\n"}, "别的 PR 改了同一行")
	h.openAutoPR(7, "kiln/dev-3", head)
	h.autoRuns(7, head, store.RunSucceeded, store.RunSucceeded)
	h.dev.setOwner(7, DevOwner{Kind: DevOwnerActive, BaseRef: "main", WorkItem: 3})
	h.nominate(7)
	h.process()
	h.wantState(7, StateRemoved, "已交给 AI 开发")
	// 同一个 head 再次提名：不再入队（不会被反复加入再移出）
	h.nominate(7)
	h.process()
	if e := h.entry(7); e.State != StateRemoved || len(h.dev.backs()) != 1 {
		t.Fatalf("冲突的 head 又被入队：%+v", e)
	}
	// AI 解决冲突：把 main 合并进 head（merge commit，父提交是被移出的 head 和 main）
	h.git("checkout", "-q", "kiln/dev-3")
	if out, err := h.gitErr("merge", "--no-edit", "main"); err == nil {
		t.Fatalf("应该冲突：%s", out)
	}
	h.commitResolved(map[string]string{"README.md": "feature + main\n"})
	fix := h.git("rev-parse", "HEAD")
	h.git("checkout", "-q", "main")
	h.fake.UpdatePull(h.repo, 7, func(p *fakegithub.Pull) { p.HeadSHA = fix; p.Labels = nil })
	h.autoRuns(7, fix, store.RunSucceeded, store.RunSucceeded)
	h.nominate(7)
	h.process()
	if e := h.entry(7); e.State != StateMerged || e.HeadSHA != fix || e.EnqueuedBy != AutoActor {
		t.Fatalf("解决冲突后的新 head 应自动入队并合并：%+v", e)
	}
}

package admin

import (
	"slices"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
)

// 合并队列与自动打 tag 的多目标分支支持（PLAN #28.5、#29.4）：列表按仓库汇总、排位按目标分支计算。

// TestListMergeQueues_SkipsBranchNotReleaseOnly：只有因目标分支不是发版分支而跳过的自动打 tag 记录的仓库不出现在合并队列页；
// 有合并队列记录或有其他自动打 tag 记录的仓库照常出现，按仓库全名排序。
func TestListMergeQueues_SkipsBranchNotReleaseOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addRepo(502, "other")
	e.addRepo(503, "quiet")
	// tester/demo 只有 branch_not_release 的自动打 tag 记录：高频的开发分支合并，不应让仓库出现在合并队列页
	e.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, source, state, skip_code)
		VALUES ($1,'aaaa',7,'webhook','skipped',$2)`, testRepoID, "branch_not_release")
	// tester/other 有合并队列记录（排队中和最近结束各一项）
	e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref) VALUES (502, 3, 'queued', 'main')`)
	e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref, finished_at) VALUES (502, 4, 'merged', 'dev', now())`)
	// tester/quiet 有还没处理的自动打 tag 记录
	e.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, source, state) VALUES (503,'bbbb',5,'webhook','pending')`)

	qs, err := e.svc.ListMergeQueues(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var repos []string
	for _, q := range qs {
		repos = append(repos, q.Repo.FullName())
	}
	if !slices.Equal(repos, []string{"tester/other", "tester/quiet"}) {
		t.Fatalf("列出的仓库 = %v，想要 tester/other、tester/quiet（只有 branch_not_release 的 tester/demo 不列出）", repos)
	}
	if q := qs[0]; len(q.Queued) != 1 || q.Queued[0].PR != 3 || q.Queued[0].BaseRef != "main" ||
		len(q.Recent) != 1 || q.Recent[0].PR != 4 || q.Recent[0].BaseRef != "dev" {
		t.Errorf("tester/other 的队列 = %+v", q)
	}
}

// TestPullMergeQueue_BaseRefPosition：排位按同一个目标分支计算；升级前 base_ref 为空的旧行与任何分支一起计数（兼容）。
func TestPullMergeQueue_BaseRefPosition(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, x := range []struct {
		pr   int
		base string
		// state 为空时是排队中
		state string
	}{
		{1, "main", ""},
		{2, "dev", ""},
		{3, "dev", ""},
		{4, "", ""}, // 升级前入队的旧行
		{5, "main", ""},
		{9, "main", mergequeue.StateMerged},
	} {
		state := x.state
		if state == "" {
			state = mergequeue.StateQueued
		}
		e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref, finished_at)
			VALUES ($1,$2,$3,$4, CASE WHEN $3='queued' THEN NULL ELSE now() END)`, testRepoID, x.pr, state, x.base)
	}
	for _, x := range []struct {
		pr   int
		want int
	}{
		{1, 1}, // main 的第 1 个
		{2, 1}, // dev 的第 1 个
		{3, 2}, // dev 的第 2 个
		{4, 4}, // 旧行：与所有分支一起计数
		{5, 3}, // main 的第 3 个（main 的 #1 + 旧行 #4 + 自己）
		{9, 0}, // 已经合并：不在排队中
	} {
		entry, pos, err := e.svc.PullMergeQueue(e.ctx, testRepoID, x.pr)
		if err != nil {
			t.Fatal(err)
		}
		if entry == nil || pos != x.want {
			t.Errorf("PR #%d：entry=%+v pos=%d，想要 pos=%d", x.pr, entry, pos, x.want)
		}
	}
	if entry, pos, err := e.svc.PullMergeQueue(e.ctx, testRepoID, 99); err != nil || entry != nil || pos != 0 {
		t.Errorf("不在队列中的 PR：entry=%+v pos=%d err=%v", entry, pos, err)
	}
}

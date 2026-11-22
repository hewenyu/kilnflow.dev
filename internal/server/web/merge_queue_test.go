package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
)

// ---- 合并队列（PLAN #28） ----

// testMergeQueuePage 是有一个排队中的 PR 和一个已合并的 PR 的合并队列页（TestPagesRender）。
func testMergeQueuePage(now time.Time) *mergeQueuePage {
	return &mergeQueuePage{
		Label: mergequeue.Label, GitHub: "https://github.com", Repos: []string{"tester/demo"},
		Queues: []mergeQueueView{{
			Repo: "tester/demo", URL: mergeQueueURL("tester/demo"),
			Queued: []mergeQueueEntry{{PR: 7, State: mergequeue.StateQueued, HeadSHA: "abc", EnqueuedBy: "alice", Status: "等待验证", EnqueuedAt: now}},
			Recent: []mergeQueueEntry{{PR: 5, State: mergequeue.StateMerged, MergeSHA: "def", EnqueuedAt: now, FinishedAt: &now}},
			AutoTags: []autoTagView{
				{SHA: "def", PR: 5, Labels: []string{"minor"}, MergedBy: "alice", Source: "queue", State: autotag.StatePlanned, Tag: "v0.2.0", Created: now, Updated: now},
				{SHA: "abc", PR: 4, Source: "webhook", State: autotag.StateFailed, Reason: "创建 tag 被拒绝", Tries: 3, Created: now, Updated: now, Retryable: true},
			},
		}},
	}
}

// addQueueEntry 直接写入合并队列的一项，返回 ID。state 不是 queued 时记录结束时间。
func (e *webEnv) addQueueEntry(repoID int64, pr int, state, status, reason, mergeSHA string) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO merge_queue (repo_id, pr_number, state, head_sha, enqueued_by, status, reason, merge_sha, finished_at)
		VALUES ($1,$2,$3,'0123456789abcdef0123456789abcdef01234567','github:alice',$4,$5,$6, CASE WHEN $3='queued' THEN NULL ELSE now() END)
		RETURNING id`, repoID, pr, state, status, reason, mergeSHA).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func queuedPRs(v mergeQueueView) []int {
	var out []int
	for _, x := range v.Queued {
		out = append(out, x.PR)
	}
	return out
}

func recentPRs(v mergeQueueView) []int {
	var out []int
	for _, x := range v.Recent {
		out = append(out, x.PR)
	}
	return out
}

// 合并队列页显示目标分支（PLAN #28.5）：排队中和最近结束的项都带 BaseRef，多分支时按目标分支分组排队。
func TestPageMergeQueue_BaseRef(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref) VALUES ($1, 12, 'queued', 'main')`, testRepoID)
	e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref) VALUES ($1, 13, 'queued', 'dev')`, testRepoID)
	e.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, base_ref, merge_sha, finished_at)
		VALUES ($1, 11, 'merged', 'dev', 'abcdef', now())`, testRepoID)

	p := pageData[mergeQueuePage](t, e.get(e.bob, mergeQueuePath).Body.String())
	if len(p.Queues) != 1 || len(p.Queues[0].Queued) != 2 {
		t.Fatalf("合并队列页 = %+v", p)
	}
	if q := p.Queues[0]; q.Queued[0].BaseRef != "main" || q.Queued[1].BaseRef != "dev" || q.Recent[0].BaseRef != "dev" {
		t.Errorf("目标分支 = %q、%q，最近结束 %q", q.Queued[0].BaseRef, q.Queued[1].BaseRef, q.Recent[0].BaseRef)
	}
	body := e.get(e.bob, mergeQueuePath).Body.String()
	for _, want := range []string{`"BaseRef":"main"`, `"BaseRef":"dev"`} {
		if !strings.Contains(body, want) {
			t.Errorf("页面数据中没有目标分支 %q", want)
		}
	}
}

// 合并队列页：按仓库分组，只列有记录的仓库；排队中的按入队顺序，最近结束的新的在前；只有管理员有入队表单的仓库选项。
func TestPageMergeQueue(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRepo(502, "other")
	e.addRepo(503, "quiet") // 没有队列记录，不列出

	// 没有任何记录
	w := e.get(e.bob, mergeQueuePath)
	wantStatus(t, "空的合并队列页", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "merge_queue" || pg.Nav != "merge_queue" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	if p := pageData[mergeQueuePage](t, w.Body.String()); len(p.Queues) != 0 || p.Label != mergequeue.Label {
		t.Fatalf("空的合并队列页 = %+v", p)
	}

	e.addQueueEntry(testRepoID, 9, mergequeue.StateMerged, "", "", "fedcba9876543210")
	e.addQueueEntry(testRepoID, 4, mergequeue.StateRemoved, "", "验证失败：ci.yml", "")
	e.addQueueEntry(testRepoID, 12, mergequeue.StateQueued, "等待验证：1/2 个 run 已结束", "", "")
	e.addQueueEntry(testRepoID, 3, mergequeue.StateQueued, "排在第 2 个", "", "")
	e.addQueueEntry(502, 1, mergequeue.StateRemoved, "", "PR 已关闭", "")

	w = e.get(e.bob, mergeQueuePath)
	wantStatus(t, "成员查看合并队列", w, http.StatusOK)
	p := pageData[mergeQueuePage](t, w.Body.String())
	if len(p.Queues) != 2 || p.Queues[0].Repo != testRepo || p.Queues[1].Repo != "tester/other" || p.Repos != nil || p.Repo != "" {
		t.Fatalf("合并队列页 = %+v", p)
	}
	demo := p.Queues[0]
	if !slices.Equal(queuedPRs(demo), []int{12, 3}) || !slices.Equal(recentPRs(demo), []int{4, 9}) || demo.URL != "/merge-queue?repo=tester%2Fdemo" {
		t.Errorf("tester/demo：排队 %v，最近 %v，地址 %q", queuedPRs(demo), recentPRs(demo), demo.URL)
	}
	if q := demo.Queued[0]; q.Status != "等待验证：1/2 个 run 已结束" || q.EnqueuedBy != "github:alice" || q.HeadSHA == "" || q.FinishedAt != nil {
		t.Errorf("队首 = %+v", q)
	}
	if m := demo.Recent[1]; m.State != mergequeue.StateMerged || m.MergeSHA != "fedcba9876543210" || m.FinishedAt == nil {
		t.Errorf("已合并的一项 = %+v", m)
	}
	if r := demo.Recent[0]; r.State != mergequeue.StateRemoved || r.Reason != "验证失败：ci.yml" {
		t.Errorf("已移出的一项 = %+v", r)
	}

	// 管理员有入队表单的仓库选项（Kiln 中启用的全部仓库）
	p = pageData[mergeQueuePage](t, e.get(e.alice, mergeQueuePath).Body.String())
	if !slices.Equal(p.Repos, []string{"tester/demo", "tester/other", "tester/quiet"}) {
		t.Errorf("管理员的仓库选项 = %v", p.Repos)
	}
}

// 合并队列页只看一个仓库：没有记录的仓库也显示（空的队列）；不存在的仓库 404；未登录跳转到登录页。
func TestPageMergeQueue_Repo(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRepo(502, "other")
	e.addRepo(503, "quiet")
	e.addQueueEntry(testRepoID, 12, mergequeue.StateQueued, "", "", "")
	e.addQueueEntry(502, 1, mergequeue.StateRemoved, "", "PR 已关闭", "")

	p := pageData[mergeQueuePage](t, e.get(e.bob, mergeQueuePath+"?repo=tester/other").Body.String())
	if p.Repo != "tester/other" || len(p.Queues) != 1 || len(p.Queues[0].Queued) != 0 || !slices.Equal(recentPRs(p.Queues[0]), []int{1}) {
		t.Errorf("只看 tester/other = %+v", p)
	}
	p = pageData[mergeQueuePage](t, e.get(e.bob, mergeQueuePath+"?repo=tester/quiet").Body.String())
	if p.Repo != "tester/quiet" || len(p.Queues) != 1 || len(p.Queues[0].Queued)+len(p.Queues[0].Recent) != 0 {
		t.Errorf("只看 tester/quiet = %+v", p)
	}
	wantStatus(t, "不存在的仓库", e.get(e.bob, mergeQueuePath+"?repo=tester/ghost"), http.StatusNotFound)
	// 不是 owner/name 的仓库名同样是 404，不是服务器错误
	wantStatus(t, "仓库名没有 /", e.get(e.bob, mergeQueuePath+"?repo=ghost"), http.StatusNotFound)
	if strings.Contains(e.log.String(), "页面出错") {
		t.Errorf("无效的仓库名不应记录为服务器错误：%s", e.log.String())
	}
	if loc := redirectTarget(t, "未登录", e.get(nil, mergeQueuePath)); !strings.HasPrefix(loc, "/login") {
		t.Errorf("未登录时跳转到 %q", loc)
	}
}

// 入队和移除只允许管理员；没有配置 GitHub App 时给出原因；表单无效时不调用接口。
func TestPostMergeQueue_Errors(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	// e.post 会在表单中写入 csrf，每次提交用新的表单
	form := func() url.Values { return url.Values{"repo": {testRepo}, "pr": {"7"}} }

	wantStatus(t, "成员入队", e.post(e.bob, mergeQueuePath+"/add", form()), http.StatusForbidden)
	wantStatus(t, "成员移除", e.post(e.bob, mergeQueuePath+"/remove", form()), http.StatusForbidden)
	wantStatus(t, "CSRF 不对", e.post(e.alice, mergeQueuePath+"/add", url.Values{"repo": {testRepo}, "pr": {"7"}, "csrf": {"bad"}}), http.StatusForbidden)

	flash := wantRedirect(t, "没有 GitHub 时入队", e.post(e.alice, mergeQueuePath+"/add", form()), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "加入合并队列失败") || !strings.Contains(flash, mergequeue.ErrNoForge.Error()) {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "没有 GitHub 时移除", e.post(e.alice, mergeQueuePath+"/remove", form()), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "移出合并队列失败") || !strings.Contains(flash, mergequeue.ErrNoForge.Error()) {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "PR 号无效", e.post(e.alice, mergeQueuePath+"/add", url.Values{"repo": {testRepo}, "pr": {"abc"}}), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "PR 号") {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "没有选仓库", e.post(e.alice, mergeQueuePath+"/add", url.Values{"pr": {"7"}}), mergeQueuePath)
	if !strings.Contains(flash, "请选择仓库") {
		t.Errorf("提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM merge_queue`); n != 0 {
		t.Errorf("失败的操作写入了 %d 项", n)
	}
}

// 管理员入队（给 PR 加上 label 并记录一项）、重复入队、移除（去掉 label）；关闭的 PR 不能入队。
func TestPostMergeQueue_GitHub(t *testing.T) {
	t.Parallel()
	e := newGitHubWebEnv(t)
	head := "1111111111111111111111111111111111111111"
	e.gh.SetPull(e.ghRepo, fakegithub.Pull{Number: 7, HeadRef: "feature", HeadSHA: head})
	e.gh.SetPull(e.ghRepo, fakegithub.Pull{Number: 8, HeadRef: "old", HeadSHA: head, State: "closed"})
	form := func() url.Values { return url.Values{"repo": {testRepo}, "pr": {"#7"}} }

	flash := wantRedirect(t, "入队", e.post(e.alice, mergeQueuePath+"/add", form()), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "已把 tester/demo#7 加入合并队列") {
		t.Errorf("提示 = %q", flash)
	}
	if p, _ := e.gh.GetPull(e.ghRepo, 7); !slices.Contains(p.Labels, mergequeue.Label) {
		t.Errorf("PR #7 的 label = %v", p.Labels)
	}
	if n := e.count(`SELECT count(*) FROM merge_queue WHERE repo_id=$1 AND pr_number=7 AND state='queued' AND enqueued_by='alice' AND head_sha=$2`, testRepoID, head); n != 1 {
		t.Errorf("排队中的 PR #7 有 %d 项", n)
	}
	flash = wantRedirect(t, "重复入队", e.post(e.alice, mergeQueuePath+"/add", form()), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "已经在合并队列中") {
		t.Errorf("提示 = %q", flash)
	}

	// 入队后的合并队列页
	p := pageData[mergeQueuePage](t, e.get(e.bob, mergeQueuePath+"?repo=tester/demo").Body.String())
	if len(p.Queues) != 1 || !slices.Equal(queuedPRs(p.Queues[0]), []int{7}) {
		t.Errorf("入队后的合并队列页 = %+v", p)
	}

	flash = wantRedirect(t, "移除", e.post(e.alice, mergeQueuePath+"/remove", url.Values{"repo": {testRepo}, "pr": {"7"}}), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "已请求把 tester/demo#7 移出合并队列") {
		t.Errorf("提示 = %q", flash)
	}
	if p, _ := e.gh.GetPull(e.ghRepo, 7); slices.Contains(p.Labels, mergequeue.Label) {
		t.Errorf("移除后 PR #7 的 label = %v", p.Labels)
	}
	if a := e.audits("mergequeue.remove"); len(a) != 1 || a[0].Actor != "alice" {
		t.Errorf("移除的审计日志 = %+v", a)
	}

	flash = wantRedirect(t, "关闭的 PR 入队", e.post(e.alice, mergeQueuePath+"/add", url.Values{"repo": {testRepo}, "pr": {"8"}}), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "加入合并队列失败") || !strings.Contains(flash, "不是打开的") {
		t.Errorf("提示 = %q", flash)
	}
}

// PR 的 run 详情页显示这个 PR 在合并队列中的状态；不是 PR 的 run 没有。
func TestPageRun_MergeQueue(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	push := e.addRun(runOpts{})
	pr := e.addRun(runOpts{ref: "refs/heads/feature"})
	e.exec(`UPDATE runs SET event='pull_request', pr_number=7 WHERE id=$1`, pr)
	runPath := func(id int64) string { return "/runs/" + strconv.FormatInt(id, 10) }

	if p := pageData[runData](t, e.get(e.bob, runPath(push)).Body.String()); p.MergeQueue != nil {
		t.Errorf("push 的 run 有合并队列状态：%+v", p.MergeQueue)
	}

	// 没有记录：只有表单字段和队列页地址
	mq := pageData[runData](t, e.get(e.bob, runPath(pr)).Body.String()).MergeQueue
	if mq == nil || mq.Entry != nil || mq.Repo != testRepo || mq.PR != 7 || mq.Pos != 0 || mq.URL != "/merge-queue?repo=tester%2Fdemo" {
		t.Fatalf("没有记录时 = %+v", mq)
	}

	// 已移出，之后重新入队排在第 2 个：排队中的优先
	e.addQueueEntry(testRepoID, 7, mergequeue.StateRemoved, "", "验证失败", "")
	e.addQueueEntry(testRepoID, 3, mergequeue.StateQueued, "等待验证：0/1 个 run 已结束", "", "")
	e.addQueueEntry(testRepoID, 7, mergequeue.StateQueued, "排在第 2 个", "", "")
	mq = pageData[runData](t, e.get(e.bob, runPath(pr)).Body.String()).MergeQueue
	if mq == nil || mq.Entry == nil || mq.Entry.State != mergequeue.StateQueued || mq.Pos != 2 || mq.Entry.Status != "排在第 2 个" {
		t.Fatalf("排队中 = %+v", mq)
	}

	// 合并后
	e.exec(`UPDATE merge_queue SET state='merged', merge_sha='abcdef', finished_at=now() WHERE pr_number=7 AND state='queued'`)
	mq = pageData[runData](t, e.get(e.bob, runPath(pr)).Body.String()).MergeQueue
	if mq == nil || mq.Entry == nil || mq.Entry.State != mergequeue.StateMerged || mq.Entry.MergeSHA != "abcdef" || mq.Pos != 0 {
		t.Fatalf("合并后 = %+v", mq)
	}
}

// addAutoTag 直接写入一条自动打 tag 记录；workItem 为 0 表示没有认定的 AI 开发工作项。
func (e *webEnv) addAutoTag(repoID int64, sha string, pr int, state, tag, reason string, workItem int64, bot bool) {
	e.t.Helper()
	var item *int64
	if workItem > 0 {
		item = &workItem
	}
	e.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, labels, merged_by, sender_is_bot, work_item_id, source, state, tag, reason)
		VALUES ($1,$2,$3,ARRAY['minor'],'alice',$7,$8,'webhook',$4,$5,$6)`, repoID, sha, pr, state, tag, reason, bot, item)
}

// 合并队列页列出自动打 tag 的记录（PLAN #29）：只有自动打 tag 记录（在队列之外合入）的仓库也列出；失败和跳过的可以重试，
// 重试只允许管理员，已完成的不能重试。
func TestMergeQueueAutoTags(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	const failedSHA, plannedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.addAutoTag(testRepoID, plannedSHA, 3, autotag.StatePlanned, "v0.2.0", "", 12, true)
	e.addAutoTag(testRepoID, failedSHA, 4, autotag.StateFailed, "v0.3.0", "创建 tag 被拒绝：App 需要 Contents 写权限", 0, false)

	p := pageData[mergeQueuePage](t, e.get(e.bob, mergeQueuePath).Body.String())
	if len(p.Queues) != 1 || p.Queues[0].Repo != testRepo || len(p.Queues[0].AutoTags) != 2 {
		t.Fatalf("只有自动打 tag 记录的仓库也应列出：%+v", p.Queues)
	}
	f, pl := p.Queues[0].AutoTags[0], p.Queues[0].AutoTags[1]
	expect(t, "失败的记录（新的在前）", f, f.SHA == failedSHA, f.PR == 4, f.State == autotag.StateFailed, f.Tag == "v0.3.0",
		strings.Contains(f.Reason, "Contents"), f.Retryable, f.MergedBy == "alice", f.Source == "webhook", len(f.Labels) == 1,
		!f.SenderIsBot, f.WorkItemID == 0)
	// 机器人合并的可信 AI 开发实现 PR：列出真实的发送者和认定的工作项
	expect(t, "已规划的记录", pl, pl.State == autotag.StatePlanned, !pl.Retryable, pl.SenderIsBot, pl.WorkItemID == 12)

	form := func(sha string) url.Values { return url.Values{"repo": {testRepo}, "sha": {sha}} }
	wantStatus(t, "成员重试", e.post(e.bob, mergeQueuePath+"/autotag/retry", form(failedSHA)), http.StatusForbidden)
	flash := wantRedirect(t, "重试已完成的记录", e.post(e.alice, mergeQueuePath+"/autotag/retry", form(plannedSHA)), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, autotag.ErrNotRetryable.Error()) {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "重试失败的记录", e.post(e.alice, mergeQueuePath+"/autotag/retry", form(failedSHA)), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "已重新处理 aaaaaaa") {
		t.Errorf("提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM auto_tags WHERE sha=$1 AND state='pending' AND reason=''`, failedSHA); n != 1 {
		t.Errorf("重试后应回到 pending：%d", n)
	}
	if a := e.audits("autotag.retry"); len(a) != 1 || a[0].Actor != "alice" {
		t.Errorf("重试的审计：%+v", a)
	}
	flash = wantRedirect(t, "缺少 commit", e.post(e.alice, mergeQueuePath+"/autotag/retry", url.Values{"repo": {testRepo}}), "/merge-queue?repo=tester%2Fdemo")
	if !strings.Contains(flash, "缺少") {
		t.Errorf("提示 = %q", flash)
	}
}

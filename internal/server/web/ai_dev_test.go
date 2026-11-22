package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ---- AI 开发（PLAN #26.15） ----

// testAIDevPage 是有一个受阻的工作项、一个暂停的仓库的 AI 开发页（TestPagesRender）。
func testAIDevPage(now time.Time) *aiDevPage {
	return &aiDevPage{
		GitHub: "https://github.com", Repos: []string{"tester/demo"}, Paused: []string{"tester/demo"}, Ready: aidev.LabelReady, HoldLabel: "kiln:hold",
		Items:     []aiDevItem{testAIDevItem(now)},
		Occupancy: &aiDevOccupancy{Occupant: 3, Reason: "工作项 #3 进行中（implement / blocked）", Waiting: []int64{}},
	}
}

func testAIDevItem(now time.Time) aiDevItem {
	return aiDevItem{
		ID: 3, Repo: "tester/demo", State: aidev.StateBlocked, Active: true, Retryable: true,
		Issues: []aiDevIssue{{Number: 7, Title: "加登录", Resolution: devreport.ResolutionPartial, Active: true}},
		PR:     12, Branch: devreport.WorkItemBranch(3, devreport.PhaseImplement, 1), HeadSHA: "abc", FixRounds: 2, MaxFixRounds: 2, Agent: "claude-code",
		Hold: true, RequirementsChanged: true, Reason: "返修 2 轮后仍未通过", Created: now, Updated: now, URL: aiDevItemURL(3),
	}
}

// testAIDevItemPage 是有选题、交付报告、评论和分支的工作项详情页（TestPagesRender）。
func testAIDevItemPage(now time.Time) *aiDevItemPage {
	return &aiDevItemPage{
		Item: testAIDevItem(now), BaseRef: "main", BaseSHA: "def", Plan: "改 a.go", TaskURL: aiTaskURL(9), FixedHead: "abc",
		Reports: []aiDevReport{
			{Kind: reportKindSelection, TaskID: 9, TaskURL: aiTaskURL(9), Selection: &devreport.Selection{Selected: []int{7}, Plan: "改 a.go"}, Created: now},
			{Kind: reportKindDelivery, TaskID: 9, TaskURL: aiTaskURL(9), Delivery: &devreport.Delivery{Status: devreport.StatusDelivered}, RunnerVersion: "v0.2.0", Created: now},
		},
		Comments: []aiDevComment{{Number: 7, Kind: "claimed", State: aidev.CommentUnknown, Updated: now}},
		Branches: []aiDevBranch{{Branch: devreport.AttemptBranch(5), SHA: "abc", Kind: "delivery", Created: now, Expires: &now}},
		RepoURL:  aiDevURL("tester/demo"), Ready: aidev.LabelReady, HoldLabel: "kiln:hold", GitHub: "https://github.com",
	}
}

// addWorkItem 直接写入一个工作项和它的 Issue，返回 ID。pr 为 0 表示还没有 PR。
func (e *webEnv) addWorkItem(repoID int64, state string, pr int, reason string, issues ...int) int64 {
	e.t.Helper()
	var prNumber *int
	if pr != 0 {
		prNumber = &pr
	}
	// 每个工作项由一次选题的领取（AI 开发任务的 attempt）创建
	claim := e.addAIAttempt(e.addAITask(e.addAIRequest(aiRequestOpts{repoID: repoID, typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestSucceeded}),
		aiTaskOpts{state: ai.TaskSucceeded}), 0, "dev1", store.OutcomeSucceeded)
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO dev_work_items (repo_id, state, active, base_ref, base_sha, max_fix_rounds, agent, claim_attempt_id,
		pr_number, head_sha, reason, hold_label, finished_at)
		VALUES ($1,$2,$3,'main','0123456789abcdef',2,'claude-code',$6,
		$4,'fedcba9876543210',$5,true, CASE WHEN $3 THEN NULL ELSE now() END) RETURNING id`,
		repoID, state, aidev.Active(state), prNumber, reason, claim).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, n := range issues {
		e.exec(`INSERT INTO dev_work_item_issues (work_item_id, repo_id, issue_number, title, digest, active, resolution)
			VALUES ($1,$2,$3,$4,'d',$5,'partial')`, id, repoID, n, "Issue "+strconv.Itoa(n), aidev.Active(state))
	}
	return id
}

func itemIDs(items []aiDevItem) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// AI 开发页：新的工作项在前，带 Issue、PR、受阻可重试；暂停的仓库单独列出；只有管理员有表单的仓库选项。
func TestPageAIDev(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRepo(502, "other")

	w := e.get(e.bob, aiDevPath)
	wantStatus(t, "空的 AI 开发页", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "ai_dev" || pg.Nav != "ai_dev" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	if p := pageData[aiDevPage](t, w.Body.String()); len(p.Items) != 0 || len(p.Paused) != 0 || p.Repos != nil || p.Ready != aidev.LabelReady {
		t.Fatalf("空的 AI 开发页 = %+v", p)
	}

	merged := e.addWorkItem(502, aidev.StateMerged, 4, "", 1)
	blocked := e.addWorkItem(testRepoID, aidev.StateBlocked, 9, "返修 2 轮后仍未通过", 3, 4)
	e.exec(`INSERT INTO ai_repo_state (repo_id, dev_paused) VALUES (502, true)`)

	p := pageData[aiDevPage](t, e.get(e.bob, aiDevPath).Body.String())
	if !slices.Equal(itemIDs(p.Items), []int64{blocked, merged}) || !slices.Equal(p.Paused, []string{"tester/other"}) || p.Repos != nil {
		t.Fatalf("成员看到的 AI 开发页 = %+v", p)
	}
	b := p.Items[0]
	if b.Repo != testRepo || b.State != aidev.StateBlocked || !b.Active || !b.Retryable || b.PR != 9 || !b.Hold ||
		b.Reason != "返修 2 轮后仍未通过" || b.Branch != devreport.WorkItemBranch(blocked, devreport.PhaseImplement, 1) || b.URL != aiDevItemURL(blocked) || b.Finished != nil {
		t.Errorf("受阻的工作项 = %+v", b)
	}
	if len(b.Issues) != 2 || b.Issues[0].Number != 3 || b.Issues[0].Title != "Issue 3" || b.Issues[0].Resolution != devreport.ResolutionPartial {
		t.Errorf("受阻的工作项的 Issue = %+v", b.Issues)
	}
	if m := p.Items[1]; m.Active || m.Retryable || m.Finished == nil || m.Repo != "tester/other" {
		t.Errorf("已合并的工作项 = %+v", m)
	}

	p = pageData[aiDevPage](t, e.get(e.alice, aiDevPath).Body.String())
	if !slices.Equal(p.Repos, []string{"tester/demo", "tester/other"}) {
		t.Errorf("管理员的仓库选项 = %v", p.Repos)
	}
	if loc := redirectTarget(t, "未登录", e.get(nil, aiDevPath)); !strings.HasPrefix(loc, "/login") {
		t.Errorf("未登录时跳转到 %q", loc)
	}
}

// AI 开发页只看一个仓库：它的工作项和任务池，暂停状态只有它自己的；仓库名不区分大小写；不存在的仓库 404。
func TestPageAIDev_Repo(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRepo(502, "other")
	merged := e.addWorkItem(502, aidev.StateMerged, 4, "", 1)
	blocked := e.addWorkItem(testRepoID, aidev.StateBlocked, 9, "", 3)
	e.exec(`INSERT INTO ai_repo_state (repo_id, dev_paused) VALUES (502, true)`)
	e.exec(`INSERT INTO dev_pool (repo_id, issue_number, in_pool, title, issue_updated_at, ready_at) VALUES
		($1, 3, true, 'Issue 3', now(), now() - interval '2 minutes'), ($1, 6, true, '新需求', now(), now() - interval '1 minute'),
		($1, 7, false, '已移出', now(), now())`, testRepoID)

	p := pageData[aiDevPage](t, e.get(e.bob, aiDevPath+"?repo=tester/demo").Body.String())
	if p.Repo != testRepo || !slices.Equal(itemIDs(p.Items), []int64{blocked}) || len(p.Paused) != 0 {
		t.Errorf("只看 tester/demo = %+v", p)
	}
	if p.Pool == nil || len(p.Pool.Issues) != 2 || p.Pool.Issues[0].Status != aidev.PoolClaimed || *p.Pool.Issues[0].WorkItem != blocked ||
		p.Pool.Issues[1].Number != 6 || p.Pool.Issues[1].Status != aidev.PoolCandidate {
		t.Errorf("tester/demo 的任务池 = %+v", p.Pool)
	}
	// 受阻的工作项仍占用仓库（同一个仓库按工作项串行）
	if o := p.Occupancy; o == nil || o.Occupant != blocked || !strings.Contains(o.Reason, "按工作项串行") || len(o.Waiting) != 0 {
		t.Errorf("tester/demo 的占用 = %+v", o)
	}
	if p := pageData[aiDevPage](t, e.get(e.bob, aiDevPath).Body.String()); p.Pool != nil || p.Occupancy != nil {
		t.Errorf("看全部仓库时不应有任务池和占用：%+v %+v", p.Pool, p.Occupancy)
	}
	p = pageData[aiDevPage](t, e.get(e.bob, aiDevPath+"?repo=TESTER/other").Body.String())
	if p.Repo != "tester/other" || !slices.Equal(itemIDs(p.Items), []int64{merged}) || !slices.Equal(p.Paused, []string{"tester/other"}) ||
		p.Occupancy == nil || p.Occupancy.Occupant != 0 || p.Occupancy.Reason != "" {
		t.Errorf("只看 tester/other = %+v", p)
	}
	wantStatus(t, "不存在的仓库", e.get(e.bob, aiDevPath+"?repo=tester/ghost"), http.StatusNotFound)
	wantStatus(t, "仓库名没有 /", e.get(e.bob, aiDevPath+"?repo=ghost"), http.StatusNotFound)
	if strings.Contains(e.log.String(), "页面出错") {
		t.Errorf("无效的仓库名不应记录为服务器错误：%s", e.log.String())
	}
}

// seedAIDevItem 写入一个 PR 已开的工作项和它的记录：选题、交付报告和一份解析不了的报告，结果不明、失败的评论，
// 已删除、保留中的 attempt 分支。返回工作项、选题的 AI 任务和分支的创建时间。
func (e *webEnv) seedAIDevItem() (id, task int64, created time.Time) {
	e.t.Helper()
	id = e.addWorkItem(testRepoID, aidev.StatePROpen, 9, "", 3)
	req := e.addAIRequest(aiRequestOpts{typ: agentcfg.TaskDevelop, devMode: "select", state: ai.RequestSucceeded})
	task = e.addAITask(req, aiTaskOpts{state: ai.TaskSucceeded})
	att := e.addAIAttempt(task, 0, "dev1", store.OutcomeSucceeded)
	e.exec(`UPDATE dev_work_items SET task_id=$2, plan='先改 a.go' WHERE id=$1`, id, task)
	sel := devreport.Selection{Version: devreport.Version, Selected: []int{3}, Plan: "先改 a.go", Skipped: []devreport.Skip{{Number: 5, Reason: "需求不清"}}}
	del := devreport.Delivery{
		Version: devreport.Version, Status: devreport.StatusDelivered, Summary: "实现了 a", PRTitle: "实现 a",
		Issues: []devreport.IssueResult{{Number: 3, Resolution: devreport.ResolutionFull}},
	}
	e.exec(`INSERT INTO dev_reports (task_id, attempt_id, kind, work_item_id, report, runner_version, agent_version, model, created_at)
		VALUES ($1,$5,'selection',$2,$3,'v0.2.0','2.1.0','',now()-interval '1 minute'),($1,$5,'delivery',$2,$4,'v0.2.0','','opus',now())`,
		task, id, string(devreport.Marshal(&sel)), string(devreport.Marshal(&del)), att)
	other := e.addAITask(req, aiTaskOpts{key: "fix", state: ai.TaskSucceeded})
	otherAtt := e.addAIAttempt(other, 0, "dev1", store.OutcomeSucceeded)
	e.exec(`INSERT INTO dev_reports (task_id, attempt_id, kind, work_item_id, report) VALUES ($1,$3,'delivery',$2,'["不是对象"]')`, other, id, otherAtt)
	e.exec(`INSERT INTO dev_comments (work_item_id, repo_id, number, kind, dedupe_key, body, state, error)
		VALUES ($1,$2,3,'claimed','k1','b','unknown',''),($1,$2,9,'dispositions','k2','b','failed','被拒')`, id, testRepoID)
	created = time.Now().Add(-time.Hour).Truncate(time.Second)
	e.exec(`INSERT INTO dev_branches (repo_id, branch, sha, work_item_id, kind, created_at, deleted_at)
		VALUES ($1,'kiln/attempt-1','aaa',$2,'delivery',$3,now()),($1,'kiln/attempt-2','bbb',$2,'checkpoint',$3,NULL)`, testRepoID, id, created)
	return id, task, created
}

// 工作项详情页：概要、计划和当前 job；评论意图的状态；不存在的工作项 404。
func TestPageAIDevItem(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id, task, _ := e.seedAIDevItem()

	w := e.get(e.bob, aiDevItemURL(id))
	wantStatus(t, "工作项详情", w, http.StatusOK)
	if pg := parsePage(t, w.Body.String()); pg.Page != "ai_dev_item" || pg.Nav != "ai_dev" {
		t.Fatalf("页面 = %q，导航 = %q", pg.Page, pg.Nav)
	}
	p := pageData[aiDevItemPage](t, w.Body.String())
	if p.Item.ID != id || p.Item.State != aidev.StatePROpen || p.Item.Retryable || p.Plan != "先改 a.go" || p.BaseRef != "main" ||
		p.TaskURL != aiTaskURL(task) || p.RepoURL != "/ai-dev?repo=tester%2Fdemo" {
		t.Fatalf("工作项详情 = %+v", p)
	}
	if len(p.Comments) != 2 || p.Comments[0].State != aidev.CommentUnknown || p.Comments[1].Error != "被拒" || p.Comments[1].Number != 9 {
		t.Errorf("评论 = %+v", p.Comments)
	}
	wantStatus(t, "不存在的工作项", e.get(e.bob, aiDevItemURL(id+100)), http.StatusNotFound)
	wantStatus(t, "不是数字", e.get(e.bob, aiDevPath+"/abc"), http.StatusNotFound)
}

// 工作项详情页的报告：解析为选题、交付（解析不了的显示原文），带适用范围（拿不到的为空）。
func TestPageAIDevItem_Reports(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id, task, _ := e.seedAIDevItem()
	p := pageData[aiDevItemPage](t, e.get(e.bob, aiDevItemURL(id)).Body.String())
	if len(p.Reports) != 3 {
		t.Fatalf("报告 = %+v", p.Reports)
	}
	if r := p.Reports[0]; r.Kind != reportKindSelection || r.Selection == nil || r.Selection.Skipped[0].Reason != "需求不清" ||
		r.Delivery != nil || r.Raw != "" || r.RunnerVersion != "v0.2.0" || r.AgentVersion != "2.1.0" || r.Model != "" {
		t.Errorf("选题报告 = %+v", r)
	}
	if r := p.Reports[1]; r.Kind != reportKindDelivery || r.Delivery == nil || r.Delivery.PRTitle != "实现 a" || r.Selection != nil ||
		r.Model != "opus" || r.TaskURL != aiTaskURL(task) {
		t.Errorf("交付报告 = %+v", r)
	}
	if r := p.Reports[2]; r.Delivery != nil || r.Selection != nil || r.Raw != `["不是对象"]` {
		t.Errorf("解析不了的报告 = %+v", r)
	}
}

// 工作项详情页的 attempt 分支：已删除的有删除时间，保留中的按配置的保留时间（默认与 aidev 相同）算出删除时间。
func TestPageAIDevItem_Branches(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id, _, created := e.seedAIDevItem()
	p := pageData[aiDevItemPage](t, e.get(e.bob, aiDevItemURL(id)).Body.String())
	if len(p.Branches) != 2 {
		t.Fatalf("分支 = %+v", p.Branches)
	}
	if b := p.Branches[0]; b.Deleted == nil || b.Expires != nil || b.Kind != "delivery" {
		t.Errorf("已删除的分支 = %+v", b)
	}
	// 删除时间按 aidev 实际使用的保留时间（测试环境配置的不是默认值）
	if b := p.Branches[1]; b.Deleted != nil || b.Expires == nil || !b.Expires.Equal(created.Add(testBranchTTL)) {
		t.Errorf("保留中的分支 = %+v（应在 %s 删除）", b, created.Add(testBranchTTL))
	}
}

// testBranchTTL 是测试环境中 attempt 分支的保留时间（不同于 aidev.DefaultBranchTTL）。
const testBranchTTL = 5 * time.Hour

// 操作只允许管理员；同步任务池失败时说明原因；暂停、恢复；重试只对受阻的；终止已结束的给出说明；都写审计日志。
func TestPostAIDev(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id := e.addWorkItem(testRepoID, aidev.StateBlocked, 9, "等人", 3)
	sid := strconv.FormatInt(id, 10)
	repoForm := func() url.Values { return url.Values{"repo": {testRepo}} }
	idForm := func(id string) url.Values { return url.Values{"id": {id}} }
	back, itemBack := "/ai-dev?repo=tester%2Fdemo", aiDevItemURL(id)

	for _, op := range []string{"sync", "pause", "resume"} {
		wantStatus(t, "成员 "+op, e.post(e.bob, aiDevPath+"/"+op, repoForm()), http.StatusForbidden)
	}
	for _, op := range []string{"stop", "retry"} {
		wantStatus(t, "成员 "+op, e.post(e.bob, aiDevPath+"/"+op, idForm(sid)), http.StatusForbidden)
	}
	wantStatus(t, "CSRF 不对", e.post(e.alice, aiDevPath+"/pause", url.Values{"repo": {testRepo}, "csrf": {"bad"}}), http.StatusForbidden)
	if n := e.count(`SELECT count(*) FROM ai_repo_state WHERE dev_paused`); n != 0 {
		t.Fatalf("被拒绝的操作暂停了 %d 个仓库", n)
	}

	// 没有配置 GitHub App：不能同步，说明原因
	flash := wantRedirect(t, "同步", e.post(e.alice, aiDevPath+"/sync", repoForm()), back)
	if !strings.Contains(flash, "同步 tester/demo 的任务池失败：") {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "同步没有选仓库", e.post(e.alice, aiDevPath+"/sync", nil), aiDevPath)
	if !strings.Contains(flash, "请选择仓库") {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "暂停不存在的仓库", e.post(e.alice, aiDevPath+"/pause", url.Values{"repo": {"tester/ghost"}}), "/ai-dev?repo=tester%2Fghost")
	if !strings.Contains(flash, "Kiln 中没有这个仓库") {
		t.Errorf("提示 = %q", flash)
	}

	flash = wantRedirect(t, "暂停", e.post(e.alice, aiDevPath+"/pause", repoForm()), back)
	if !strings.Contains(flash, "已暂停 tester/demo") {
		t.Errorf("提示 = %q", flash)
	}
	if p := pageData[aiDevPage](t, e.get(e.bob, aiDevPath).Body.String()); !slices.Equal(p.Paused, []string{testRepo}) {
		t.Errorf("暂停后 Paused = %v", p.Paused)
	}
	flash = wantRedirect(t, "恢复", e.post(e.alice, aiDevPath+"/resume", repoForm()), back)
	if !strings.Contains(flash, "已恢复 tester/demo") {
		t.Errorf("提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM ai_repo_state WHERE dev_paused`); n != 0 {
		t.Errorf("恢复后仍有 %d 个仓库暂停", n)
	}
	if a, r := e.audits("aidev.pause"), e.audits("aidev.resume"); len(a) != 1 || a[0].Actor != "alice" || len(r) != 1 {
		t.Errorf("暂停、恢复的审计日志 = %+v %+v", a, r)
	}

	flash = wantRedirect(t, "重试", e.post(e.alice, aiDevPath+"/retry", idForm(sid)), itemBack)
	if !strings.Contains(flash, "已让工作项 #"+sid+" 再返修一轮") {
		t.Errorf("提示 = %q", flash)
	}
	if a := e.audits("aidev.retry"); len(a) != 1 || a[0].Actor != "alice" {
		t.Errorf("重试的审计日志 = %+v", a)
	}
	flash = wantRedirect(t, "重试不是受阻的", e.post(e.alice, aiDevPath+"/retry", idForm(sid)), itemBack)
	if !strings.Contains(flash, "重试工作项 #"+sid+" 失败") || !strings.Contains(flash, "请刷新页面") {
		t.Errorf("提示 = %q", flash)
	}

	flash = wantRedirect(t, "终止", e.post(e.alice, aiDevPath+"/stop", idForm(sid)), itemBack)
	if !strings.Contains(flash, "已终止工作项 #"+sid) {
		t.Errorf("提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM dev_work_items WHERE id=$1 AND state='stopped'`, id); n != 1 {
		t.Errorf("终止后工作项不是 stopped")
	}
	if a := e.audits("aidev.stop"); len(a) != 1 || a[0].Actor != "alice" {
		t.Errorf("终止的审计日志 = %+v", a)
	}
	flash = wantRedirect(t, "再次终止", e.post(e.alice, aiDevPath+"/stop", idForm(sid)), itemBack)
	if !strings.Contains(flash, "工作项已经结束") {
		t.Errorf("提示 = %q", flash)
	}
	missing := strconv.FormatInt(id+100, 10)
	flash = wantRedirect(t, "终止不存在的", e.post(e.alice, aiDevPath+"/stop", idForm(missing)), aiDevPath+"/"+missing)
	if !strings.Contains(flash, "工作项不存在") {
		t.Errorf("提示 = %q", flash)
	}
	flash = wantRedirect(t, "编号无效", e.post(e.alice, aiDevPath+"/retry", idForm("x")), aiDevPath)
	if !strings.Contains(flash, "工作项编号无效") {
		t.Errorf("提示 = %q", flash)
	}
	if strings.Contains(e.log.String(), "操作出错") {
		t.Errorf("预期中的失败不应记录为服务器错误：%s", e.log.String())
	}
}

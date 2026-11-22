package ingest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// installationRepos 构造 installation_repositories 事件。
func (h *harness) installationRepos(action string, inst int64, added, removed []*fakegithub.Repo) map[string]any {
	return h.fake.InstallationRepositoriesPayload(action, inst, added, removed)
}

// activeRepos 返回 Kiln 中启用的仓库 ID（按 ID）。
func (h *harness) activeRepos() []int64 {
	h.t.Helper()
	ids, err := store.QueryIDs(h.ctx, h.db, `SELECT id FROM repos WHERE active ORDER BY id`)
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

// TestInstallationEvents：installation 和 installation_repositories 事件按 payload 增量更新仓库目录，
// 不列出仓库、不请求 GitHub。
func TestInstallationEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other, _ := h.addRepo(202, "other", 0, true)
	inst := h.fake.InstallationID

	steps := []struct {
		name       string
		event      string
		payload    map[string]any
		wantResult string
		wantActive []int64
		wantState  store.InstallationState
	}{
		{
			name: "installation.created 记录 payload 中的仓库", event: "installation",
			payload:    h.fake.InstallationPayload("created", inst),
			wantResult: "已更新 installation：新增 2 个仓库，移除 0 个", wantActive: []int64{demoRepoID, 202}, wantState: store.InstallationActive,
		},
		{
			name: "移除仓库", event: "installation_repositories",
			payload:    h.installationRepos("removed", inst, nil, []*fakegithub.Repo{other}),
			wantResult: "已更新 installation：新增 0 个仓库，移除 1 个", wantActive: []int64{demoRepoID}, wantState: store.InstallationActive,
		},
		{
			name: "installation 的其他 action 只更新 installation", event: "installation",
			payload:    h.fake.InstallationPayload("new_permissions_accepted", inst),
			wantResult: "已更新 installation：新增 0 个仓库，移除 0 个", wantActive: []int64{demoRepoID}, wantState: store.InstallationActive,
		},
		{
			name: "installation.deleted 标记卸载并停用仓库", event: "installation",
			payload:    h.fake.InstallationPayload("deleted", inst),
			wantResult: "installation 已删除", wantActive: nil, wantState: store.InstallationDeleted,
		},
	}
	for _, s := range steps {
		mark := len(h.fake.Requests())
		if got := h.deliverOK(s.event, s.payload); got != s.wantResult {
			t.Fatalf("%s：结果 %q，期望 %q", s.name, got, s.wantResult)
		}
		if got := h.activeRepos(); !slices.Equal(got, s.wantActive) {
			t.Fatalf("%s：启用的仓库 %v，期望 %v", s.name, got, s.wantActive)
		}
		if got := h.installationState(inst); got != s.wantState {
			t.Fatalf("%s：installation 状态 %d，期望 %d", s.name, got, s.wantState)
		}
		if reqs := h.requestsSince(mark); len(reqs) > 0 {
			t.Fatalf("%s：不应请求 GitHub：%v", s.name, reqs)
		}
	}
	// installation 事件中的仓库没有默认分支和 clone 地址，留到第一次用到前补齐
	if r := h.repo(demoRepoID); !r.Incomplete() || r.FullName() != "tester/demo" || r.OwnerID != h.fake.AccountID {
		t.Fatalf("仓库记录 = %+v", r)
	}
}

// TestInstallationSuspended：installation 的 suspend 等 action 按 payload 更新 installation 的状态。
func TestInstallationSuspended(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := h.fake.InstallationPayload("suspend", h.fake.InstallationID)
	field(t, p, "installation")["suspended_at"] = "2026-01-02T03:04:05Z"
	h.deliverOK("installation", p)
	var suspended bool
	if err := h.db.QueryRow(h.ctx, `SELECT suspended FROM installations WHERE id=$1`, h.fake.InstallationID).Scan(&suspended); err != nil {
		t.Fatal(err)
	}
	if !suspended {
		t.Fatal("suspend 后 installation 应标记 suspended")
	}
}

// TestInstallationLateEvents：生命周期事件可能乱序或晚到。停用和卸载直接执行，恢复不相信 payload，
// 定向确认 GitHub 的当前状态。
func TestInstallationLateEvents(t *testing.T) {
	t.Parallel()
	t.Run("removed 先于 added 到达：仓库已不在授权范围内，不恢复", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		inst := h.fake.InstallationID
		h.deliverOK("installation", h.fake.InstallationPayload("created", inst))
		// 本地还没有记录的仓库被移除：也记下一条停用的记录
		gone, _ := h.addRepo(303, "gone", 0, true)
		h.deliverOK("installation_repositories", h.installationRepos("removed", inst, nil, []*fakegithub.Repo{gone}))
		if r := h.repo(303); r == nil || r.Active {
			t.Fatalf("移除的仓库应记为停用：%+v", r)
		}
		h.fake.MoveRepo(gone, -1)
		got := h.deliverOK("installation_repositories", h.installationRepos("added", inst, []*fakegithub.Repo{gone}, nil))
		if !strings.Contains(got, "1 个已不在授权范围内，不恢复") {
			t.Fatalf("晚到的 added 的结果 %q", got)
		}
		if h.repo(303).Active {
			t.Fatal("晚到的 added 不应恢复仓库")
		}
		// 真正的重新授权：定向确认后恢复，并用 GitHub 的信息补齐
		h.fake.MoveRepo(gone, inst)
		if got := h.deliverOK("installation_repositories", h.installationRepos("added", inst, []*fakegithub.Repo{gone}, nil)); got != "已更新 installation：新增 1 个仓库，移除 0 个" {
			t.Fatalf("重新授权的结果 %q", got)
		}
		if r := h.repo(303); !r.Active || r.Incomplete() {
			t.Fatalf("重新授权的仓库 = %+v，期望启用且信息完整", r)
		}
	})
	t.Run("installation 已卸载：晚到的 created 不恢复", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		const inst = 778
		h.fake.AddInstallation(inst)
		h.addRepo(404, "b1", inst, true)
		created := h.fake.InstallationPayload("created", inst)
		h.deliverOK("installation", created)
		h.deliverOK("installation", h.fake.InstallationPayload("deleted", inst))
		h.fake.RemoveInstallation(inst)
		if got := h.deliverOK("installation", created); got != "installation 不存在或已卸载，丢弃" {
			t.Fatalf("晚到的 created 的结果 %q", got)
		}
		if h.installationState(inst) != store.InstallationDeleted || h.repo(404).Active {
			t.Fatal("晚到的 created 不应恢复 installation 或仓库")
		}
	})
	t.Run("installation 重新安装：定向确认后恢复", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		inst := h.fake.InstallationID
		h.deliverOK("installation", h.fake.InstallationPayload("created", inst))
		h.deliverOK("installation", h.fake.InstallationPayload("deleted", inst))
		// 只移除仓库的事件不恢复已卸载的 installation，也不请求 GitHub
		mark := len(h.fake.Requests())
		h.deliverOK("installation_repositories", h.installationRepos("removed", inst, nil, []*fakegithub.Repo{h.demo}))
		if h.installationState(inst) != store.InstallationDeleted {
			t.Fatal("只移除仓库的事件不应恢复 installation")
		}
		if reqs := h.requestsSince(mark); len(reqs) > 0 {
			t.Fatalf("不应请求 GitHub：%v", reqs)
		}
		got := h.deliverOK("installation", h.fake.InstallationPayload("created", inst))
		if got != "已更新 installation：新增 1 个仓库，移除 0 个" {
			t.Fatalf("重新安装的结果 %q", got)
		}
		if h.installationState(inst) != store.InstallationActive || !h.repo(demoRepoID).Active {
			t.Fatal("重新安装后 installation 和仓库应恢复")
		}
	})
}

// TestInstallationEventRejected：不在白名单中的账号安装时卸载（PLAN #16.1）；payload 没有 installation 时丢弃。
func TestInstallationEventRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOpts) { o.cfg.AllowedAccounts = map[string]bool{"9001000": true} })
	if got := h.deliverOK("installation", h.fake.InstallationPayload("created", h.fake.InstallationID)); got != "不在白名单中，已卸载" {
		t.Fatalf("结果 %q", got)
	}
	if !slices.Contains(h.fake.UnhandledRequests(), "DELETE /app/installations/777") {
		t.Fatal("没有卸载不在白名单中的 installation")
	}
	if h.installationState(h.fake.InstallationID) != store.InstallationUnknown || len(h.activeRepos()) != 0 {
		t.Fatal("不在白名单中的 installation 不应记录")
	}
	if got := h.deliverOK("installation", map[string]any{"action": "created"}); got != "没有 installation，忽略" {
		t.Fatalf("没有 installation 的结果 %q", got)
	}
}

// TestRepositoryDefaultBranchChanged：修改默认分支的事件只是信号，定向读取这一个仓库，按 GitHub 的当前值记录默认分支，
// 并通知合并队列按当前的默认分支重新判断；迟到的旧事件不会把之后改回的值覆盖掉；通知失败后重试补发通知。
func TestRepositoryDefaultBranchChanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, h.git.head("main")))
	if r := h.repo(demoRepoID); r.DefaultBranch != "main" {
		t.Fatalf("默认分支 = %q", r.DefaultBranch)
	}
	h.queue.take()
	notified := []string{fmt.Sprintf("notify %d#0", demoRepoID)}
	branchChanged := func(branch string) map[string]any {
		p := h.fake.RepositoryPayload(h.demo, "edited")
		field(t, p, "repository")["default_branch"] = branch
		p["changes"] = map[string]any{"default_branch": map[string]any{"from": "main"}}
		return p
	}
	readRepo := fmt.Sprintf("GET /repositories/%d", demoRepoID)
	wantRead := func(mark int) {
		t.Helper()
		if reqs := h.requestsSince(mark); !slices.Equal(reqs, []string{readRepo}) {
			t.Fatalf("请求 = %v，期望只定向读取这个仓库", reqs)
		}
	}

	// 修改默认分支：按 GitHub 的当前值记录
	h.fake.SetDefaultBranch(h.demo, "trunk")
	mark := len(h.fake.Requests())
	if got := h.deliverOK("repository", branchChanged("trunk")); got != "默认分支改为 trunk" {
		t.Fatalf("修改默认分支的结果 %q", got)
	}
	wantRead(mark)
	if r := h.repo(demoRepoID); r.DefaultBranch != "trunk" {
		t.Fatalf("修改后默认分支 = %q，期望 trunk", r.DefaultBranch)
	}
	if got := h.queue.take(); !slices.Equal(got, notified) {
		t.Fatalf("合并队列：%v，想要 %v", got, notified)
	}

	// 迟到的旧事件（默认分支已经改回 main，旧的"改为 dev"事件才到）：按 GitHub 的当前值记录，不按 payload
	h.fake.SetDefaultBranch(h.demo, "main")
	mark = len(h.fake.Requests())
	if got := h.deliverOK("repository", branchChanged("dev")); got != "默认分支改为 main" {
		t.Fatalf("迟到的旧事件的结果 %q", got)
	}
	wantRead(mark)
	if r := h.repo(demoRepoID); r.DefaultBranch != "main" {
		t.Fatalf("迟到的旧事件后默认分支 = %q，期望 GitHub 的当前值 main", r.DefaultBranch)
	}
	if got := h.queue.take(); !slices.Equal(got, notified) {
		t.Fatalf("迟到的旧事件的合并队列：%v，想要 %v", got, notified)
	}

	// 记录之后通知失败：重试时默认分支已经是当前值，仍然补发通知
	h.fake.SetDefaultBranch(h.demo, "release")
	h.queue.failNotify = 1
	d := h.newDelivery()
	h.send("repository", d, branchChanged("release"))
	h.process()
	if e := h.event(d); e.Processed || e.Error == "" {
		t.Fatalf("通知失败后的事件 = %+v，期望等待重试", e)
	}
	if r := h.repo(demoRepoID); r.DefaultBranch != "release" {
		t.Fatalf("通知失败前默认分支已记录为 %q，期望 release", r.DefaultBranch)
	}
	h.exec(`UPDATE events SET next_attempt_at=now() WHERE delivery_id=$1`, d)
	h.process()
	if e := h.event(d); !e.Processed || e.Error != "" || e.Result != "默认分支已是 release" {
		t.Fatalf("重试后的事件 = %+v", e)
	}
	if got := h.queue.take(); !slices.Equal(got, notified) {
		t.Fatalf("重试后的合并队列：%v，想要 %v", got, notified)
	}

	// 仓库已经不在 GitHub 上：忽略，不改记录（之后的删除事件停用它）
	h.fake.RemoveRepo(h.demo)
	if got := h.deliverOK("repository", branchChanged("x")); got != "仓库已不在授权范围内，忽略" {
		t.Fatalf("仓库不在时的结果 %q", got)
	}
	if r := h.repo(demoRepoID); r.DefaultBranch != "release" || !r.Active {
		t.Fatalf("仓库不在时记录 = %+v，期望不变", r)
	}
	if got := h.queue.take(); len(got) != 0 {
		t.Fatalf("仓库不在时不应通知合并队列：%v", got)
	}
}

// TestRepositoryEvents：repository 的改名和转移按仓库 ID 更新名称（修改默认分支见 TestRepositoryDefaultBranchChanged）；删除时停用（不请求 GitHub），
// 还不在 Kiln 中的仓库也记下一条停用的记录。
func TestRepositoryEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, h.git.head("main")))

	renamed := h.fake.RepositoryPayload(h.demo, "renamed")
	field(t, renamed, "repository")["name"] = "demo2"
	if got := h.deliverOK("repository", renamed); got != "仓库改名" {
		t.Fatalf("改名的结果 %q", got)
	}
	if r := h.repo(demoRepoID); r.Name != "demo2" || r.Owner != testAccount {
		t.Fatalf("改名后 = %s", r.FullName())
	}

	transferred := h.fake.RepositoryPayload(h.demo, "transferred")
	repo := field(t, transferred, "repository")
	repo["owner"] = map[string]any{"login": "new-owner", "id": 4242}
	if got := h.deliverOK("repository", transferred); got != "仓库改名" {
		t.Fatalf("转移的结果 %q", got)
	}
	if r := h.repo(demoRepoID); r.FullName() != "new-owner/demo" || r.OwnerID != 4242 {
		t.Fatalf("转移后 = %s（owner_id %d）", r.FullName(), r.OwnerID)
	}

	if got := h.deliverOK("repository", h.fake.RepositoryPayload(h.demo, "edited")); got != "" {
		t.Fatalf("编辑仓库的结果 %q，期望忽略", got)
	}

	mark := len(h.fake.Requests())
	if got := h.deliverOK("repository", h.fake.RepositoryPayload(h.demo, "deleted")); got != "仓库已删除" {
		t.Fatalf("删除的结果 %q", got)
	}
	if h.repo(demoRepoID).Active {
		t.Fatal("删除的仓库应停用")
	}
	// 还不在 Kiln 中的仓库被删除：记下一条停用的记录
	unknown, _ := h.addRepo(505, "unknown", 0, true)
	h.deliverOK("repository", h.fake.RepositoryPayload(unknown, "deleted"))
	if r := h.repo(505); r == nil || r.Active {
		t.Fatalf("删除的未知仓库 = %+v，期望记为停用", r)
	}
	if reqs := h.requestsSince(mark); len(reqs) > 0 {
		t.Fatalf("删除仓库不应请求 GitHub：%v", reqs)
	}

	// 不属于白名单账号的仓库、没有仓库的事件：忽略
	foreign := h.fake.RepositoryPayload(unknown, "deleted")
	field(t, foreign, "repository")["id"] = 606
	field(t, foreign, "repository")["owner"] = map[string]any{"login": "mallory", "id": 66}
	if got := h.deliverOK("repository", foreign); got != "仓库不属于白名单账号，忽略" {
		t.Fatalf("其他账号的仓库的结果 %q", got)
	}
	if h.repo(606) != nil {
		t.Fatal("其他账号的仓库不应记录")
	}
	if got := h.deliverOK("repository", map[string]any{"action": "deleted", "installation": map[string]any{"id": 777}}); got != "没有仓库，忽略" {
		t.Fatalf("没有仓库的结果 %q", got)
	}
}

// TestSyncInstallations：显式"刷新仓库"列出 App 的全部安装和仓库：记录白名单中的，卸载不在白名单中的，
// 停用 GitHub 上已经没有的 installation 和仓库（本地配置保留），不读取分支、tag、PR。
func TestSyncInstallations(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.AddInstallation(778)
	h.addRepo(202, "b1", 778, true)
	// 本地有一个 GitHub 上已经没有的 installation 和它的仓库，以及默认 installation 中已删除的仓库
	h.exec(`INSERT INTO installations (id, account_id, account_login) VALUES (999, 9001, 'tester')`)
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url) VALUES (901, 999, 9001, 'tester', 'old', 'x'),
		(902, 777, 9001, 'tester', 'removed', 'x')`)
	h.exec(`UPDATE repos SET cache_salt='keep' WHERE id=901`)

	mark := len(h.fake.Requests())
	res, err := h.ing.SyncInstallations(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Installations != 2 || res.Repos != 2 {
		t.Fatalf("结果 %+v，期望 2 个 installation、2 个仓库", res)
	}
	if got := h.activeRepos(); !slices.Equal(got, []int64{demoRepoID, 202}) {
		t.Fatalf("启用的仓库 %v", got)
	}
	if h.installationState(999) != store.InstallationDeleted {
		t.Fatal("GitHub 上已经没有的 installation 应标记卸载")
	}
	if r := h.repo(901); r.Active || r.CacheSalt != "keep" {
		t.Fatalf("停用的仓库 = %+v，期望停用并保留本地配置", r)
	}
	if r := h.repo(demoRepoID); r.Incomplete() {
		t.Fatalf("刷新得到的仓库信息应完整：%+v", r)
	}
	for _, r := range h.requestsSince(mark) {
		if strings.HasSuffix(r, "/branches") || strings.Contains(r, "/git/") || strings.HasSuffix(r, "/pulls") {
			t.Errorf("刷新仓库不应读取分支、tag 或 PR：%s", r)
		}
	}
}

// TestSyncInstallationsRejects：不在白名单中的 installation 被卸载，不计入结果。
func TestSyncInstallationsRejects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOpts) { o.cfg.AllowedAccounts = map[string]bool{"someone-else": true} })
	res, err := h.ing.SyncInstallations(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Installations != 0 || res.Repos != 0 {
		t.Fatalf("结果 %+v，期望都为 0", res)
	}
	if !slices.Contains(h.fake.UnhandledRequests(), "DELETE /app/installations/777") {
		t.Fatal("没有卸载不在白名单中的 installation")
	}
}

// TestSyncInstallationsNoGitHubApp：没有配置 GitHub App 时刷新仓库和补对账都返回错误。
func TestSyncInstallationsNoGitHubApp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *harnessOpts) { o.noGitHub = true })
	if _, err := h.ing.SyncInstallations(h.ctx); !errors.Is(err, errNoGitHubApp) {
		t.Fatalf("SyncInstallations 错误 %v，期望 errNoGitHubApp", err)
	}
	if _, err := h.ing.ReconcileRepo(h.ctx, demoRepoID); !errors.Is(err, errNoGitHubApp) {
		t.Fatalf("ReconcileRepo 错误 %v，期望 errNoGitHubApp", err)
	}
}

// TestBriefRepoMeta：installation 事件中的仓库按 full_name 拆分 owner 和 name；没有 full_name 时用 installation 的账号。
func TestBriefRepoMeta(t *testing.T) {
	t.Parallel()
	inst := &gh.Installation{ID: 7, Account: gh.Account{ID: 9, Login: "acct"}}
	tests := []struct {
		name        string
		in          ghRepoBrief
		owner, repo string
	}{
		{"有 full_name", ghRepoBrief{ID: 1, Name: "x", FullName: "org/x"}, "org", "x"},
		{"没有 full_name", ghRepoBrief{ID: 2, Name: "y"}, "acct", "y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := briefRepoMeta(inst, tt.in)
			if m.Owner != tt.owner || m.Name != tt.repo || m.InstallationID != 7 || m.OwnerID != 9 || m.ID != tt.in.ID {
				t.Fatalf("briefRepoMeta = %+v，期望 %s/%s", m, tt.owner, tt.repo)
			}
		})
	}
}

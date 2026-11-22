package ingest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// TestRepoNeedOf：与构建无关的事件不读取 GitHub，只针对已有仓库的事件不接入新仓库。
func TestRepoNeedOf(t *testing.T) {
	t.Parallel()
	base := json.RawMessage(`{}`)
	tests := []struct {
		name, typ, action string
		head              eventHead
		want              repoNeed
	}{
		{"push", "push", "", eventHead{}, needAny},
		{"删除分支", "push", "", eventHead{Deleted: true}, needKnown},
		{"打开 PR", "pull_request", "opened", eventHead{}, needAny},
		{"修改 PR 标题", "pull_request", "edited", eventHead{}, needNone},
		{"修改 PR 目标分支", "pull_request", "edited", eventHead{Changes: eventChanges{Base: &base}}, needAny},
		{"关闭 PR", "pull_request", "closed", eventHead{}, needKnown},
		{"PR 加标签", "pull_request", "labeled", eventHead{}, needKnown},
		{"PR 去掉标签", "pull_request", "unlabeled", eventHead{}, needKnown},
		{"PR 转为 draft", "pull_request", "converted_to_draft", eventHead{}, needKnown},
		{"PR 指定 reviewer", "pull_request", "review_requested", eventHead{}, needNone},
		{"Check Run 重跑", "check_run", "rerequested", eventHead{}, needKnown},
		{"本 App 的 Check Run", "check_run", "completed", eventHead{}, needNone},
		{"Check Suite 重跑", "check_suite", "rerequested", eventHead{}, needKnown},
		{"Check Suite 请求", "check_suite", "requested", eventHead{}, needNone},
		{"仓库改名", "repository", "renamed", eventHead{}, needKnown},
		{"仓库转移", "repository", "transferred", eventHead{}, needKnown},
		{"仓库编辑", "repository", "edited", eventHead{}, needNone},
		{"修改仓库的默认分支", "repository", "edited", eventHead{Changes: eventChanges{DefaultBranch: &base}}, needKnown},
		{"其他事件", "issues", "assigned", eventHead{}, needNone},
		{"新建 Issue（可能已带 kiln:ready）", "issues", "opened", eventHead{}, needKnown},
		{"Issue 加标签（AI 开发）", "issues", "labeled", eventHead{}, needKnown},
		{"Issue 去掉标签", "issues", "unlabeled", eventHead{}, needKnown},
		{"Issue 编辑", "issues", "edited", eventHead{}, needKnown},
		{"Issue 关闭", "issues", "closed", eventHead{}, needKnown},
		{"Issue 重新打开", "issues", "reopened", eventHead{}, needKnown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoNeedOf(tt.typ, tt.action, &tt.head); got != tt.want {
				t.Fatalf("repoNeedOf(%s, %s) = %d，期望 %d", tt.typ, tt.action, got, tt.want)
			}
		})
	}
}

// TestProcessEventIgnored：与构建无关、缺少 installation 或仓库、仓库不在 Kiln 中的"只针对已有仓库"的事件，
// 记录原因后忽略，不向 GitHub 发任何请求。
func TestProcessEventIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	repoJSON := h.fake.RepositoryPayload(h.demo, "x")["repository"]
	inst := map[string]any{"id": h.fake.InstallationID}
	sha := h.git.head("main")
	tests := []struct {
		name    string
		event   string
		payload map[string]any
		want    string
	}{
		{"ping", "ping", map[string]any{"zen": "hi"}, "ping"},
		{"本 App 的 check_run.completed", "check_run", map[string]any{"action": "completed", "installation": inst, "repository": repoJSON}, ""},
		{"check_suite.requested", "check_suite", map[string]any{"action": "requested", "installation": inst, "repository": repoJSON}, ""},
		{"PR 指定 reviewer", "pull_request", h.prPayload(h.demo, "review_requested", 1, "feature", sha, "main"), ""},
		{"PR 加标签（仓库不在 Kiln 中）", "pull_request", h.prPayload(h.demo, "labeled", 1, "feature", sha, "main"), "仓库不在 Kiln 中，忽略"},
		{"其他事件", "issues", map[string]any{"action": "assigned", "installation": inst, "repository": repoJSON}, ""},
		{"Issue 加标签（仓库不在 Kiln 中）", "issues", map[string]any{"action": "labeled", "installation": inst, "repository": repoJSON}, "仓库不在 Kiln 中，忽略"},
		{"没有 installation", "push", map[string]any{"ref": "refs/heads/main", "after": sha, "repository": repoJSON}, "没有 installation，忽略"},
		{"没有仓库", "push", map[string]any{"ref": "refs/heads/main", "after": sha, "installation": inst}, "没有仓库，忽略"},
		{"删除分支（仓库不在 Kiln 中）", "push", h.deletePayload(h.demo, "refs/heads/old", sha), "仓库不在 Kiln 中，忽略"},
		{"关闭 PR（仓库不在 Kiln 中）", "pull_request", h.prPayload(h.demo, "closed", 1, "feature", sha, "main"), "仓库不在 Kiln 中，忽略"},
		{"重跑（仓库不在 Kiln 中）", "check_run", map[string]any{
			"action": "rerequested", "installation": inst, "repository": repoJSON, "check_run": map[string]any{"external_id": "job:1"},
		}, "仓库不在 Kiln 中，忽略"},
		{"改名（仓库不在 Kiln 中）", "repository", h.fake.RepositoryPayload(h.demo, "renamed"), "仓库不在 Kiln 中，忽略"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mark := len(h.fake.Requests())
			if got := h.deliverOK(tt.event, tt.payload); got != tt.want {
				t.Fatalf("结果 %q，期望 %q", got, tt.want)
			}
			if reqs := h.requestsSince(mark); len(reqs) > 0 {
				t.Fatalf("不应请求 GitHub：%v", reqs)
			}
		})
	}
	if h.repo(demoRepoID) != nil {
		t.Fatal("被忽略的事件不应把仓库记录到 Kiln")
	}
}

// TestFirstEventTargetedVerification：installation 和仓库都还不在 Kiln 中时，只定向校验这一个 installation
// （GET /app/installations/{id}）和这一个仓库（GET /repositories/{id}），不列出 installation 或仓库，然后记录并规划。
func TestFirstEventTargetedVerification(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sha := h.git.head("main")
	mark := len(h.fake.Requests())
	if got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha)); got != "新建 1 个 run" {
		t.Fatalf("结果 %q", got)
	}
	reqs := h.requestsSince(mark)
	for _, want := range []string{"GET /app/installations/777", "GET /repositories/101"} {
		if !slices.Contains(reqs, want) {
			t.Errorf("没有定向校验：缺少 %s，请求 %v", want, reqs)
		}
	}
	for _, r := range reqs {
		if r == "GET /app/installations" || r == "GET /installation/repositories" {
			t.Errorf("不应列出：%s", r)
		}
	}
	if st := h.installationState(h.fake.InstallationID); st != store.InstallationActive {
		t.Fatalf("installation 状态 %d，期望已记录", st)
	}
	repo := h.repo(demoRepoID)
	if repo == nil || !repo.Active || repo.FullName() != "tester/demo" || repo.CloneURL == "" || repo.DefaultBranch != "main" {
		t.Fatalf("仓库记录 = %+v", repo)
	}

	// 第二个事件：installation 和仓库都已在 Kiln 中，不再校验
	mark = len(h.fake.Requests())
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, sha))
	for _, r := range h.requestsSince(mark) {
		if strings.HasPrefix(r, "GET /app/installations/") || strings.HasPrefix(r, "GET /repositories/") {
			t.Errorf("已记录的 installation 和仓库不应再次校验：%s", r)
		}
	}
}

// TestInstallationAllowlist：installation 所属账号必须在白名单中（PLAN #16.1）。
func TestInstallationAllowlist(t *testing.T) {
	t.Parallel()
	t.Run("GitHub 上的账号不在白名单中：卸载并丢弃", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(o *harnessOpts) { o.cfg.AllowedAccounts = map[string]bool{"someone-else": true} })
		got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main")))
		if got != "installation 不在白名单中，已卸载" {
			t.Fatalf("结果 %q", got)
		}
		if !slices.Contains(h.fake.UnhandledRequests(), "DELETE /app/installations/777") {
			t.Fatalf("没有卸载 installation：%v", h.fake.Requests())
		}
		if h.repo(demoRepoID) != nil || len(h.runs(demoRepoID)) != 0 {
			t.Fatal("不在白名单中的事件不应记录仓库或产生 run")
		}
	})
	t.Run("本地记录的账号不在白名单中：丢弃，不请求 GitHub", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.exec(`INSERT INTO installations (id, account_id, account_login) VALUES (777, 5, 'mallory')`)
		mark := len(h.fake.Requests())
		got := h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main")))
		if got != "installation 不在白名单中，丢弃" {
			t.Fatalf("结果 %q", got)
		}
		if reqs := h.requestsSince(mark); len(reqs) > 0 {
			t.Fatalf("不应请求 GitHub：%v", reqs)
		}
	})
	t.Run("installation 在 GitHub 上不存在：丢弃", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		p := h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main"))
		p["installation"] = map[string]any{"id": 9999}
		if got := h.deliverOK("push", p); got != "installation 不存在或已卸载，丢弃" {
			t.Fatalf("结果 %q", got)
		}
		if h.installationState(9999) != store.InstallationUnknown {
			t.Fatal("不存在的 installation 不应记录")
		}
	})
}

// TestRepoScope：第一次出现的仓库必须在 installation 的授权范围内（repodir.Verify），否则丢弃，不记录。
func TestRepoScope(t *testing.T) {
	t.Parallel()
	t.Run("私有仓库已移出 installation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fake.MoveRepo(h.demo, -1)
		p := h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main"))
		p["installation"] = map[string]any{"id": h.fake.InstallationID}
		if got := h.deliverOK("push", p); got != "仓库不在 installation 的授权范围内（或已删除），丢弃" {
			t.Fatalf("结果 %q", got)
		}
		if h.repo(demoRepoID) != nil {
			t.Fatal("不在授权范围内的仓库不应记录")
		}
	})
	t.Run("公开仓库属于别的 installation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fake.AddInstallation(778)
		pub, g := h.addRepo(202, "pub", 778, false)
		p := h.pushPayload(pub, "refs/heads/main", gitref.ZeroSHA, g.head("main"))
		// 事件声称来自默认 installation：公开仓库谁都能读取，必须再确认它所属的 installation
		p["installation"] = map[string]any{"id": h.fake.InstallationID}
		if got := h.deliverOK("push", p); got != "仓库不在 installation 的授权范围内（或已删除），丢弃" {
			t.Fatalf("结果 %q", got)
		}
		if !slices.Contains(h.fake.Requests(), "GET /repos/tester/pub/installation") {
			t.Fatal("公开仓库没有定向确认所属的 installation")
		}
		if h.repo(202) != nil {
			t.Fatal("属于别的 installation 的公开仓库不应记录")
		}
	})
	t.Run("已停用的仓库：晚到的事件按 GitHub 的当前状态决定", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		sha := h.git.head("main")
		h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, sha))
		h.exec(`UPDATE repos SET active=false WHERE id=$1`, demoRepoID)
		h.fake.MoveRepo(h.demo, -1)
		late := h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, sha)
		late["installation"] = map[string]any{"id": h.fake.InstallationID}
		if got := h.deliverOK("push", late); !strings.Contains(got, "授权范围") {
			t.Fatalf("结果 %q，期望丢弃", got)
		}
		if h.repo(demoRepoID).Active {
			t.Fatal("晚到的事件不应恢复已停用的仓库")
		}
		// 仓库重新加入 installation：下一个事件定向校验后恢复
		h.fake.MoveRepo(h.demo, h.fake.InstallationID)
		h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, sha))
		if !h.repo(demoRepoID).Active {
			t.Fatal("重新授权的仓库应恢复")
		}
	})
}

// TestResolveRepoCompletesFromPayload：来自 installation 事件的仓库记录没有默认分支和 clone 地址，
// 下一个事件用 payload 中的值补齐，不额外请求 GitHub。
func TestResolveRepoCompletesFromPayload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverOK("installation", h.fake.InstallationPayload("created", h.fake.InstallationID))
	if r := h.repo(demoRepoID); r == nil || !r.Incomplete() {
		t.Fatalf("installation 事件记录的仓库 = %+v，期望缺少默认分支和 clone 地址", r)
	}
	mark := len(h.fake.Requests())
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/feature", gitref.ZeroSHA, h.git.head("main")))
	r := h.repo(demoRepoID)
	if r.Incomplete() || r.CloneURL != "file://"+h.git.dir || r.DefaultBranch != "main" {
		t.Fatalf("补齐后的仓库 = %+v", r)
	}
	if slices.Contains(h.requestsSince(mark), "GET /repositories/101") {
		t.Fatal("payload 中已有的信息不应再读取 GitHub")
	}
}

// TestBotSenderFlag：sender 是 Bot（但不是本 App）时照常规划，run 标记 sender_is_bot。
func TestBotSenderFlag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := withSender(h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main")), "dependabot[bot]", "Bot")
	if got := h.deliverOK("push", p); got != "新建 1 个 run" {
		t.Fatalf("结果 %q", got)
	}
	if r := h.lastRun(demoRepoID, 1); !r.SenderIsBot {
		t.Fatal("Bot 触发的 run 应标记 sender_is_bot")
	}
}

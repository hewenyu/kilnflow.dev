package aidev

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 开发的模块功能测试（PLAN #26.8–#26.13）：数据库用 pgtest（真实的 Postgres，唯一索引就是不变量），GitHub 用
// fakegithub（本地 git 仓库，分支和 PR 真实移动），规划、取消和结束任务用真实的 AI 核心（ai.Core）。Runner 的领取、
// 推送由测试直接模拟（Runner 端由 internal/runner 的测试覆盖）；PR head 上 CI 的结果由 fakeCI 提供（组装层实现）。

func TestMain(m *testing.M) { pgtest.Main(m) }

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

const (
	testAccount = "alice"
	testRepoID  = 42
	repoName    = testAccount + "/demo"
	// devConfigAgent 是 devConfigYML 中开发的 Agent。
	devConfigAgent = agentcfg.AgentClaudeCode
	// devConfigYML 是启用开发的 .kiln/agent/config.yml。
	devConfigYML = "version: 1\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\ndevelopment:\n  agent: claude-code\n  max_fix_rounds: 2\n  max_issues: 2\n"
	testLease    = time.Minute
	// testSkipLabel 是不打版本 tag 的 label（组装层传 autotag.LabelSkip）。
	testSkipLabel = "release:skip"
)

type harness struct {
	t        *testing.T
	ctx      context.Context
	db       *store.DB
	fake     *fakegithub.Server
	repo     *fakegithub.Repo
	dir      string
	svc      *Service
	core     *ai.Core
	ci       *fakeCI
	runnerID int64
	host     string
	clock    time.Time
	// queued 是 queueElsewhere 排过的任务数（每个任务的去重键不同）
	queued int
	// selfMu 保护 selfReady：App 身份是否已经加载（模拟启动时读取 App 信息失败、后台仍在重试）。
	selfMu    sync.Mutex
	selfReady bool
}

// checkSelf 是 Deps.CheckSelf：默认 App 身份已经加载、认得出本 App 的 bot；测试可以用 setSelfReady 模拟身份未就绪。
func (h *harness) checkSelf(login string) (self, ready bool) {
	h.selfMu.Lock()
	defer h.selfMu.Unlock()
	if !h.selfReady {
		return false, false
	}
	return strings.EqualFold(login, h.fake.AppSlug+"[bot]"), true
}

// setSelfReady 设置 App 身份是否已经加载。
func (h *harness) setSelfReady(ready bool) {
	h.selfMu.Lock()
	defer h.selfMu.Unlock()
	h.selfReady = ready
}

func newHarness(t *testing.T, config string) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), dir: t.TempDir(), host: "host-dev"}
	h.git("init", "-q", "-b", "main")
	files := map[string]string{"README.md": "demo\n"}
	if config != "" {
		files[".kiln/agent/config.yml"] = config
		files[".kiln/agent/development.md"] = "开发说明\n"
	}
	h.commitOn("main", files, "init")
	h.fake = fakegithub.New(testAccount)
	t.Cleanup(h.fake.Close)
	h.repo = &fakegithub.Repo{ID: testRepoID, Owner: testAccount, Name: "demo", Dir: h.dir, Private: true}
	h.fake.AddRepo(h.repo)
	if err := store.UpsertRepo(h.ctx, h.db, store.RepoMeta{
		ID: testRepoID, InstallationID: h.fake.InstallationID, OwnerID: 1, Owner: testAccount, Name: "demo", Private: true,
		DefaultBranch: "main", CloneURL: "https://example.invalid/alice/demo.git",
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	hub := notify.NewHub()
	ob := outbox.NewWriter(true)
	h.ci = &fakeCI{}
	h.selfReady = true
	h.svc = New(Deps{
		DB: h.db, Forge: h.newForge(), TokenForge: h.newTokenForge(), GitHub: h.client(), CIResults: h.ci, Outbox: ob, Hub: hub, Log: log, HoldLabel: mergecfg.HoldLabel,
		ReleaseSkipLabel: testSkipLabel,
		CheckSelf:        h.checkSelf,
	})
	// 与组装层相同：Core 的 Registry 要有开发的任务类型，所以 Core 在 Service 之后创建
	h.core = ai.New(ai.Config{LeaseTTL: testLease, CancelGrace: time.Minute, AcquireLongPoll: time.Second, SweepInterval: time.Hour},
		ai.Deps{DB: h.db, Outbox: ob, Hub: hub, Log: log, Types: ai.NewRegistry(h.svc.TaskType())})
	h.svc.SetCore(h.core)
	h.core.SetDevHandler(h.svc)
	h.runnerID = h.addRunner("dev1", h.host)
	return h
}

// addRunner 登记一台在线的开发机（开发角色的 AI 执行节点），返回它的 ID。
func (h *harness) addRunner(name, host string) int64 {
	return h.addRunnerWith(name, host, agentcfg.Agents()...)
}

// testDevPlatform 是没有写 development.platform 时开发的平台（macOS arm64），也是测试中开发机默认上报的平台。
const testDevPlatform = "darwin/arm64"

// addRunnerWith 登记一台在线的开发机（平台 testDevPlatform），只有 agents 可用（其余上报为不可用）。
func (h *harness) addRunnerWith(name, host string, agents ...string) int64 {
	h.t.Helper()
	return h.addRunnerOn(name, host, testDevPlatform, agents...)
}

// addRunnerOn 登记一台上报 platform 的在线开发机，只有 agents 可用（其余上报为不可用）。
func (h *harness) addRunnerOn(name, host, platform string, agents ...string) int64 {
	h.t.Helper()
	var id int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runners (name, public_key, kind, version, last_seen_at)
		VALUES ($1, '\x00', 'ai', $2, now()) RETURNING id`, name, testRunnerVersion).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO ai_runners (runner_id, role, platform, protocol, host_id, orca_state, reported_at)
		VALUES ($1, $2, $3, $4, $5, 'ready', now())`, id, store.RunnerRoleDevelopment, platform, agentcfg.AIProtocol, host)
	for _, a := range agentcfg.Agents() {
		st := agentcfg.AgentUnavailable
		if slices.Contains(agents, a) {
			st = agentcfg.AgentAvailable
		}
		h.exec(`INSERT INTO ai_runner_agents (runner_id, agent, installed, state, checked_at) VALUES ($1, $2, true, $3, now())`, id, a, st)
	}
	return id
}

// queueElsewhere 在另一个仓库（testRepoID+1）排一个 agent 的、可以在 platforms 上执行的开发任务（选题），返回任务 ID。
// 这些任务与本仓库竞争开发机的空闲槽位（freeDevSlots）。
func (h *harness) queueElsewhere(agent string, platforms ...string) int64 {
	h.t.Helper()
	other := int64(testRepoID + 1)
	if err := store.UpsertRepo(h.ctx, h.db, store.RepoMeta{
		ID: other, InstallationID: h.fake.InstallationID, OwnerID: 1, Owner: testAccount, Name: "other", Private: true,
		DefaultBranch: "main", CloneURL: "https://example.invalid/alice/other.git",
	}); err != nil {
		h.t.Fatal(err)
	}
	otherRepo, err := store.GetRepo(h.ctx, h.db, other)
	if err != nil {
		h.t.Fatal(err)
	}
	h.queued++
	_, task, err := h.svc.plan(h.ctx, &devRequest{
		platforms: platforms, repo: otherRepo, ref: "refs/heads/main", sha: "x", dedupeKey: "elsewhere-" + strconv.Itoa(h.queued),
		timeout: testTimeout, spec: &aispec.DevelopSpec{Mode: aispec.DevSelect, Agent: agent},
	})
	if err != nil || task.State != ai.TaskPending {
		h.t.Fatalf("另一个仓库的排队任务：%v, %v", task, err)
	}
	return task.ID
}

// client 返回连接 fakegithub 的新客户端（限流等状态是每个客户端各自记住的）。
func (h *harness) client() *gh.Client {
	return &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: h.fake.URL, UploadURL: h.fake.URL}
}

// newForge 返回用新客户端的 ForgeFunc。
func (h *harness) newForge() ForgeFunc {
	client := h.client()
	return func(r *store.Repo) forge.Repo { return forge.GitHub(client, r.InstallationID, r.FullName()) }
}

// newTokenForge 返回用新客户端的 TokenForgeFunc（交付的写操作）。
func (h *harness) newTokenForge() TokenForgeFunc {
	client := h.client()
	return func(r *store.Repo, token string) forge.Repo {
		return forge.GitHubWithToken(client, r.InstallationID, token, r.FullName())
	}
}

func (h *harness) fakeRepo() *fakegithub.Repo { return h.repo }

func (h *harness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(h.ctx), "git", args...)
	cmd.Dir = h.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitOn 在分支 branch（不存在时从当前 HEAD 创建）上写入 files 并提交，返回新的 commit；之后回到 main。
func (h *harness) commitOn(branch string, files map[string]string, msg string) string {
	h.t.Helper()
	if h.branchHead("main") != "" {
		if h.branchHead(branch) == "" {
			h.git("branch", branch)
		}
		h.git("checkout", "-q", branch)
	}
	defer func() {
		if branch != "main" {
			h.git("checkout", "-q", "main")
		}
	}()
	for name, content := range files {
		p := filepath.Join(h.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.git("add", "-A")
	h.git("commit", "-q", "--allow-empty", "-m", msg)
	return h.git("rev-parse", "HEAD")
}

// push 模拟 Runner 推送交付：从 from 开始提交 files，推到 attempt 分支 branch，返回 commit。
func (h *harness) push(branch, from string, files map[string]string) string {
	h.t.Helper()
	h.git("branch", "-f", branch, from)
	return h.commitOn(branch, files, "AI 开发："+branch)
}

// branchHead 返回分支的 commit（不存在时为空）。
func (h *harness) branchHead(branch string) string {
	h.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	cmd.Dir = h.dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

func (h *harness) queryInt(sql string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.db.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
	return n
}

func (h *harness) queryBool(sql string, args ...any) bool {
	h.t.Helper()
	var v bool
	if err := h.db.QueryRow(h.ctx, sql, args...).Scan(&v); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
	return v
}

func (h *harness) queryString(sql string, args ...any) string {
	h.t.Helper()
	var v string
	if err := h.db.QueryRow(h.ctx, sql, args...).Scan(&v); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
	return v
}

// issue 在 GitHub 上登记一个 Issue（默认有 kiln:ready），并像人打 label 一样投递 issues 事件（任务池随之更新）。
func (h *harness) issue(n int, title, body string, labels ...string) {
	h.t.Helper()
	if labels == nil {
		labels = []string{LabelReady}
	}
	h.fake.SetIssue(repoName, fakegithub.Issue{Number: n, Title: title, Body: body, Labels: labels, UpdatedAt: h.tick()})
	if slices.Contains(labels, LabelReady) {
		h.event(n, IssueLabeled, LabelReady)
	} else {
		h.event(n, IssueEdited, "")
	}
}

// tick 返回严格递增的时间（Issue 的 updated_at：任务池丢弃比已记录的更旧的内容）。
func (h *harness) tick() time.Time {
	h.clock = h.clock.Add(time.Second)
	if h.clock.Before(time.Now()) {
		h.clock = time.Now().Truncate(time.Second)
	}
	return h.clock
}

// event 按 GitHub 上 Issue 的当前内容投递一个 issues 事件（人的操作）。
func (h *harness) event(n int, action, label string) string {
	h.t.Helper()
	is, ok := h.fake.GetIssue(repoName, n)
	if !ok {
		h.t.Fatalf("Issue #%d 不存在", n)
	}
	res, err := h.svc.HandleIssueEvent(h.ctx, IssueEvent{
		RepoID: testRepoID, Action: action, Number: n, Title: is.Title, Body: is.Body, Open: is.State == "open", Labels: is.Labels,
		UpdatedAt: is.UpdatedAt, Label: label, Sender: "bob",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// editIssue 在 GitHub 上修改 Issue（updated_at 前进），不投递事件。
func (h *harness) editIssue(n int, f func(is *fakegithub.Issue)) {
	h.fake.UpdateIssue(repoName, n, func(is *fakegithub.Issue) {
		f(is)
		is.UpdatedAt = h.tick()
	})
}

// settle 让任务池看起来已经静默很久（给出候选时不用等）。
func (h *harness) settle() {
	h.exec(`INSERT INTO ai_repo_state (repo_id, dev_pool_changed_at) VALUES ($1, now() - interval '1 hour')
		ON CONFLICT (repo_id) DO UPDATE SET dev_pool_changed_at=EXCLUDED.dev_pool_changed_at`, testRepoID)
}

// offer 模拟 Runner 准备好后要候选（任务池已经静默）。
func (h *harness) offer(attemptID int64) *OfferResult {
	h.t.Helper()
	h.settle()
	res, err := h.svc.OfferCandidates(h.ctx, h.taskOfAttempt(attemptID), attemptID)
	if err != nil {
		h.t.Fatal(err)
	}
	if res.Wait != 0 {
		h.t.Fatalf("静默之后还要等 %s", res.Wait)
	}
	return res
}

// candidates 返回候选的编号。
func candidates(list []aispec.DevIssue) []int {
	out := make([]int, 0, len(list))
	for _, c := range list {
		out = append(out, c.Number)
	}
	return out
}

// startSelect 规划选题，返回请求（没有规划时 t.Fatal）。
func (h *harness) startSelect() int64 {
	h.t.Helper()
	res, err := h.svc.PlanSelect(h.ctx, testRepoID, "bob")
	if err != nil {
		h.t.Fatal(err)
	}
	if res.RequestID == 0 {
		h.t.Fatalf("没有规划选题：%s", res.Reason)
	}
	return res.RequestID
}

// taskOf 返回请求中唯一的任务。
func (h *harness) taskOf(requestID int64) *ai.Task {
	h.t.Helper()
	tasks, err := ai.RequestTasks(h.ctx, h.db, requestID, false)
	if err != nil || len(tasks) != 1 {
		h.t.Fatalf("请求 %d 的任务：%v（%d 个）", requestID, err, len(tasks))
	}
	return tasks[0]
}

// task 返回任务的当前内容。
func (h *harness) task(id int64) *ai.Task {
	h.t.Helper()
	t, err := ai.GetTask(h.ctx, h.db, id, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// taskOfAttempt 返回 attempt 所属任务的当前内容。
func (h *harness) taskOfAttempt(attemptID int64) *ai.Task {
	h.t.Helper()
	a, err := ai.GetAttempt(h.ctx, h.db, attemptID)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.task(a.TaskID)
}

// testRunnerVersion 是测试中 AI 执行节点的版本（记入 attempt，领取时写入选题报告的适用范围）。
const testRunnerVersion = "0.2.0"

// acquire 模拟开发机 dev1 领取任务，返回 attempt。
func (h *harness) acquire(taskID int64) int64 {
	h.t.Helper()
	return h.acquireBy(taskID, h.runnerID)
}

// acquireBy 模拟开发机 runnerID 领取任务：与 ai.Core 的领取相同，同一事务中 running、epoch+1、签发租约、写 attempt
// （记下 Runner 的名字、机器标识和版本），再调用任务类型的 Claimed。
func (h *harness) acquireBy(taskID, runnerID int64) int64 {
	h.t.Helper()
	var aid int64
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		r := &ai.Runner{ID: runnerID, Role: store.RunnerRoleDevelopment}
		if err := tx.QueryRow(h.ctx, `SELECT r.name, r.version, ar.host_id FROM runners r JOIN ai_runners ar ON ar.runner_id=r.id WHERE r.id=$1`,
			runnerID).Scan(&r.Name, &r.Version, &r.HostID); err != nil {
			return err
		}
		var epoch int64
		if err := tx.QueryRow(h.ctx, `UPDATE ai_tasks SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
			lease_expires_at=now()+make_interval(secs => $2), started_at=now() WHERE id=$1 AND state='pending' RETURNING epoch`,
			taskID, testLease.Seconds()).Scan(&epoch); err != nil {
			return fmt.Errorf("任务 %d 不是 pending：%w", taskID, err)
		}
		if err := tx.QueryRow(h.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, runner_name, host_id, acquire_request_id, runner_version)
			VALUES ($1,$2,$3,$4,$5,'req',$6) RETURNING id`, taskID, epoch, runnerID, r.Name, r.HostID, r.Version).Scan(&aid); err != nil {
			return err
		}
		if _, err := tx.Exec(h.ctx, `UPDATE ai_tasks SET current_attempt_id=$2 WHERE id=$1`, taskID, aid); err != nil {
			return err
		}
		t, err := ai.GetTask(h.ctx, tx, taskID, false)
		if err != nil {
			return err
		}
		return h.svc.TaskType().Claimed(h.ctx, tx, t, r)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return aid
}

// isStale 判断 err 是否是 attempt 过期（STALE_EPOCH）。
func isStale(err error) bool { return rpcerr.IsStaleEpoch(err) }

// selection 返回选题报告：选中 selected（一个 PR 完成），skipped 写"超出授权"（现在不适合做），later 写"不放进这一批"
// （留在任务池）。
func selection(selected, skipped, later []int) []byte {
	return selectionAs(false, selected, skipped, later)
}

// selectionAs 返回选题报告，designFirst 是交付方式（PLAN #31.4）。
func selectionAs(designFirst bool, selected, skipped, later []int) []byte {
	s := devreport.Selection{
		Version: devreport.Version, Selected: selected, Plan: "先改 a，再测", BatchReason: "同一个模块", Skipped: []devreport.Skip{},
		DesignFirst: &designFirst, DesignReason: "按规模判断",
	}
	for _, n := range skipped {
		s.Skipped = append(s.Skipped, devreport.Skip{Number: n, Reason: "需求不清", Reconsider: "补齐验收标准后"})
	}
	for _, n := range later {
		s.Skipped = append(s.Skipped, devreport.Skip{Number: n, Reason: "与这一批无关", Later: true})
	}
	return devreport.Marshal(&s)
}

// claim 领取（还没有要过候选时先要）：未选的候选是 skipped（现在不适合做）。
func (h *harness) claim(attemptID int64, selected []int, skipped ...int) (*ClaimResult, error) {
	return h.claimSel(attemptID, selected, selection(selected, skipped, nil))
}

// claimSel 用选题报告 sel 领取（还没有要过候选时先要）。
func (h *harness) claimSel(attemptID int64, selected []int, sel []byte) (*ClaimResult, error) {
	h.t.Helper()
	if h.queryInt(`SELECT count(*) FROM dev_offers WHERE attempt_id=$1`, attemptID) == 0 {
		h.offer(attemptID)
	}
	a, err := ai.GetAttempt(h.ctx, h.db, attemptID)
	if err != nil {
		h.t.Fatal(err)
	}
	// 与 DevHandler 的 Claim 相同：执行的 Agent 和连接器版本取自任务和 attempt
	t := h.taskOfAttempt(attemptID)
	return h.svc.ClaimIssues(h.ctx, t, &ClaimRequest{
		AttemptID: attemptID, Issues: selected, Selection: sel, Agent: t.Agent, RunnerVersion: a.RunnerVersion,
	})
}

func (h *harness) mustClaim(attemptID int64, selected []int, skipped ...int) *ClaimResult {
	h.t.Helper()
	res, err := h.claim(attemptID, selected, skipped...)
	if err != nil {
		h.t.Fatal(err)
	}
	if !res.Claimed {
		h.t.Fatalf("没有领取：%s", res.Reason)
	}
	return res
}

// delivered 返回交付报告：issues 都是 full；dispositions 是对 findings 的处置（fixed）。
func delivered(issues []int, fixCommit string, findings ...string) []byte {
	d := devreport.Delivery{Version: devreport.Version, Status: devreport.StatusDelivered, Summary: "实现了", PRTitle: "实现功能"}
	for _, n := range issues {
		d.Issues = append(d.Issues, devreport.IssueResult{
			Number: n, Resolution: devreport.ResolutionFull,
			Acceptance: []devreport.AcceptanceItem{{Item: "能用", Done: true, Evidence: "单元测试"}},
		})
	}
	d.Verification = []devreport.Verification{{Command: "go test ./...", Outcome: devreport.OutcomePassed}}
	for _, f := range findings {
		d.Dispositions = append(d.Dispositions, devreport.Disposition{Finding: f, Action: devreport.ActionFixed, Commit: fixCommit, Reason: "改了"})
	}
	return devreport.Marshal(&d)
}

// complete 模拟 Complete：经 AI 核心提交 attempt 的结果（任务类型的 Finish 记录交付、推进工作项），返回 attempt 最终的结果。
// del 的 HostID、RunnerVersion 不用：执行的机器和版本取自 attempt（领取时记下）。
func (h *harness) complete(attemptID int64, outcome store.Outcome, del *Delivery) ai.Result {
	h.t.Helper()
	res := ai.Result{Outcome: outcome, Message: "结果说明"}
	if del != nil {
		res.AgentVersion, res.AgentModel = del.AgentVersion, del.Model
		res.Delivery = &airunnerv2.DevDelivery{Report: del.Report, Branch: del.Branch, Sha: del.SHA, Checkpoint: del.Checkpoint}
	}
	if err := h.core.FinishAttempt(h.ctx, attemptID, res); err != nil {
		h.t.Fatal(err)
	}
	var out ai.Result
	var class *string
	if err := h.db.QueryRow(h.ctx, `SELECT outcome, error_class FROM ai_attempts WHERE id=$1`, attemptID).Scan(&out.Outcome, &class); err != nil {
		h.t.Fatal(err)
	}
	if class != nil {
		out.ErrClass = *class
	}
	return out
}

func (h *harness) process() error { return h.svc.Process(h.ctx, testRepoID) }

func (h *harness) mustProcess() {
	h.t.Helper()
	if err := h.process(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) workItem(id int64) *WorkItem {
	h.t.Helper()
	w, err := GetWorkItem(h.ctx, h.db, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *harness) wantState(id int64, state string) *WorkItem {
	h.t.Helper()
	w := h.workItem(id)
	if w.State != state {
		h.t.Fatalf("工作项 #%d 的状态 = %s（%s），想要 %s", id, w.State, w.Reason, state)
	}
	return w
}

// comments 返回仓库的评论意图（编号、类别、状态）。
func (h *harness) comments() []string {
	h.t.Helper()
	rows, err := h.db.Query(h.ctx, `SELECT number || ':' || kind || ':' || state FROM dev_comments WHERE repo_id=$1 ORDER BY id`, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// countRequests 返回 fakegithub 收到的形如 route 的请求数。
func (h *harness) countRequests(route string) int {
	n := 0
	for _, r := range h.fake.Requests() {
		if r == route {
			n++
		}
	}
	return n
}

// labels 返回 Issue 的 label。
func (h *harness) labels(n int) []string {
	h.t.Helper()
	is, ok := h.fake.GetIssue(repoName, n)
	if !ok {
		h.t.Fatalf("Issue #%d 不存在", n)
	}
	slices.Sort(is.Labels)
	return is.Labels
}

// developed 走完选题、领取、交付、推进，返回处于 pr_open 的工作项和它的 PR。issues 是要领取的 Issue（已登记）。
func (h *harness) developed(issues ...int) (*WorkItem, int) {
	h.t.Helper()
	run := h.startSelect()
	aid := h.acquire(h.taskOf(run).ID)
	res := h.mustClaim(aid, issues)
	sha := h.push(devreport.AttemptBranch(aid), h.branchHead("main"), map[string]string{"feature.go": "package demo\n"})
	h.complete(aid, store.OutcomeSucceeded, &Delivery{Report: delivered(issues, ""), Branch: devreport.AttemptBranch(aid), SHA: sha})
	h.mustProcess()
	w := h.wantState(res.WorkItemID, StatePROpen)
	return w, *w.PRNumber
}

// prJob 是 PR head 上的一个 CI job 或 AI 审计任务（prRun 用）。
type prJob struct {
	// review 为真时是 AI 审计任务，否则是 CI job。
	review   bool
	name     string
	state    ai.TaskState
	errClass string
	// report 是 AI 审计的报告（review 时）。
	report string
}

// prRun 在 PR pr 的 head 上建一次 CI 运行（CI job，经 fakeCI 提供）和一个 AI 审计请求（审计任务），finished 表示都已结束；
// 返回各个 job / 任务的 id（与 jobs 的顺序相同）。没有 CI job 时不建 CI 运行，没有审计任务时不建审计请求。
func (h *harness) prRun(pr int, head string, finished bool, jobs ...prJob) []int64 {
	h.t.Helper()
	ids := make([]int64, len(jobs))
	var ci []CIFailure
	hasCI := false
	for i, j := range jobs {
		if j.review {
			continue
		}
		hasCI = true
		ids[i] = h.ci.nextJobID()
		if j.state == ai.TaskFailed {
			ci = append(ci, CIFailure{JobID: ids[i], Name: j.name, Reason: "失败的原因", ErrClass: j.errClass})
		}
	}
	if hasCI || len(jobs) == 0 {
		h.ci.add(pr, head, ciRun{done: finished, failures: ci})
	}
	var reviews []int
	for i, j := range jobs {
		if j.review {
			reviews = append(reviews, i)
		}
	}
	if len(reviews) == 0 {
		return ids
	}
	state := ai.RequestActive
	if finished {
		state = ai.RequestSucceeded
		for _, i := range reviews {
			if jobs[i].state != ai.TaskSucceeded {
				state = ai.RequestFailed
			}
		}
	}
	err := h.db.Tx(h.ctx, func(tx pgx.Tx) error {
		var reqID int64
		if err := tx.QueryRow(h.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
			INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, source, family_id, state)
			SELECT id, $1, $2, $3, $4, $5, 'webhook', id, $6 FROM n RETURNING id`,
			agentcfg.TaskReview, testRepoID, gitref.EventPullRequest, fmt.Sprintf("%s%d/head", gitref.PullPrefix, pr), head, state).Scan(&reqID); err != nil {
			return err
		}
		if _, err := tx.Exec(h.ctx, `INSERT INTO ai_review_requests (request_id, pr_number) VALUES ($1, $2)`, reqID, pr); err != nil {
			return err
		}
		for _, i := range reviews {
			j := jobs[i]
			var id int64
			if err := tx.QueryRow(h.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, reason, timeout_minutes)
				VALUES ($1,$2,$3,'{darwin/arm64}','codex',$2,'{}',$4,'失败的原因',60) RETURNING id`, reqID, j.name, agentcfg.TaskReview, j.state).Scan(&id); err != nil {
				return err
			}
			var aid int64
			if err := tx.QueryRow(h.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id, outcome, error_class)
				VALUES ($1, 1, $2, 'req', 'failed', nullif($3,'')) RETURNING id`, id, h.runnerID, j.errClass).Scan(&aid); err != nil {
				return err
			}
			if _, err := tx.Exec(h.ctx, `UPDATE ai_tasks SET current_attempt_id=$2 WHERE id=$1`, id, aid); err != nil {
				return err
			}
			if j.report != "" {
				if _, err := tx.Exec(h.ctx, `INSERT INTO ai_reviews (task_id, repo_id, attempt_id, pr_number, head_sha, agent, verdict, findings, report)
					VALUES ($1,$2,$3,$4,$5,'codex','issues_found',1,$6)`, id, testRepoID, aid, pr, head, j.report); err != nil {
					return err
				}
			}
			ids[i] = id
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

// finishPR 让 PR pr 上进行中的 CI 运行和 AI 审计请求都结束（成功）。
func (h *harness) finishPR(pr int) {
	h.ci.finish(pr)
	h.exec(`UPDATE ai_requests SET state='succeeded', finished_at=now() WHERE state='active'
		AND id IN (SELECT request_id FROM ai_review_requests WHERE pr_number=$1)`, pr)
}

// ciRun 是 PR head 上的一次 CI 运行。
type ciRun struct {
	pr       int
	head     string
	done     bool
	failures []CIFailure
}

// fakeCI 实现 CIResults（组装层用 CI 领域的数据实现）：测试登记 PR head 上的 CI 运行。
type fakeCI struct {
	mu     sync.Mutex
	runs   []ciRun
	lastID int64
}

var _ CIResults = (*fakeCI)(nil)

func (f *fakeCI) Settled(_ context.Context, _ store.Querier, repoID int64, pr int, head string) (runs int, done bool, failures []CIFailure, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done = true
	if repoID != testRepoID {
		return 0, true, nil, nil
	}
	for _, r := range f.runs {
		if r.pr != pr || r.head != head {
			continue
		}
		runs++
		done = done && r.done
		if r.done {
			failures = append(failures, r.failures...)
		}
	}
	return runs, done, failures, nil
}

func (f *fakeCI) add(pr int, head string, r ciRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.pr, r.head = pr, head
	f.runs = append(f.runs, r)
}

func (f *fakeCI) finish(pr int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.runs {
		if f.runs[i].pr == pr {
			f.runs[i].done = true
		}
	}
}

// fakeCIJobBase 是 fakeCI 中 job 编号的起点（与 AI 任务的编号区分开，便于看出说明来自哪里）。
const fakeCIJobBase = 9000

func (f *fakeCI) nextJobID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID++
	return fakeCIJobBase + f.lastID
}

// findingsReport 是有一个问题的 AI 审计报告（v1）。
const findingsReport = `{"verdict":"issues_found","summary":"有问题","findings":[{"title":"空指针","severity":"high","evidence":"code_evidence","body":"x 为 nil 时崩溃","path":"feature.go","line":1}]}`

// specOf 返回任务的 AI 开发内容。
func (h *harness) specOf(taskID int64) *aispec.DevelopSpec {
	h.t.Helper()
	d, err := developSpec(h.task(taskID))
	if err != nil {
		h.t.Fatalf("任务 %d 不是 AI 开发：%v", taskID, err)
	}
	return d
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

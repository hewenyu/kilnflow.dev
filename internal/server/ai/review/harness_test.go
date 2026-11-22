package review

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 审计的模块功能测试（PLAN #30.6、#30.7）：数据库用 pgtest（真实的 Postgres），GitHub 用 fakegithub（内容来自本地 git
// 仓库），规划、取消、结束任务用真实的 AI 核心（ai.Core，注册本包的任务类型），PR review 由真实的 outbox worker 投递。
// Runner 的领取由测试直接模拟（与 ai.Core 的领取相同：同一事务中 running、epoch+1、写 attempt）。

func TestMain(m *testing.M) { pgtest.Main(m) }

// testKey 是全部测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

const (
	testAccount   = "tester"
	testRepoID    = 101
	testRepo      = testAccount + "/demo"
	testPublicURL = "https://kiln.example"
	// aiPR 是测试中被审计的 PR。
	aiPR = 7
	// featureRef 是 PR 的来源分支。
	featureRef = "feat"
	// testRunnerVersion 是测试中 AI 执行节点的版本（领取时记入 attempt，保存报告时写入 ai_reviews）。
	testRunnerVersion = "0.3.1"
	testLease         = time.Minute
	// exhaustedTries 远超 outbox 的重试上限：下一次失败即放弃（dead）。
	exhaustedTries = 1 << 20
)

// reviewConfig 启用审计（一个平台，默认发布 PR review）。
const reviewConfig = "version: 1\nreview:\n  agent: codex\n  required_platforms:\n    - {os: macos, arch: arm64}\n"

// twoPlatformConfig 启用审计，两个平台（两个任务，用于区分重跑方式）。
const twoPlatformConfig = "version: 1\nreview:\n  agent: codex\n  required_platforms:\n    - {id: m1, os: macos, arch: arm64}\n    - {id: m2, os: macos, arch: arm64}\n"

// recordingEvents 记录审计请求结束时写出的 PR 验证事件（Events，组装层经 outbox 转交合并队列和 aidev）。
type recordingEvents struct {
	mu     sync.Mutex
	events []prVerified
}

type prVerified struct {
	repoID int64
	pr     int
	passed bool
}

func (e *recordingEvents) PRVerified(_ context.Context, _ store.Execer, repoID int64, pr int, passed bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, prVerified{repoID: repoID, pr: pr, passed: passed})
	return nil
}

func (e *recordingEvents) list() []prVerified {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]prVerified(nil), e.events...)
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	fake   *fakegithub.Server
	repo   *fakegithub.Repo
	dir    string
	client *gh.Client
	writer *outbox.Writer
	worker *outbox.Worker
	events *recordingEvents
	typ    *Type
	core   *ai.Core
	rev    *Reviews
	// queueFrom 是合并队列记录的"新 head ← 旧 head"（UpdatedFrom 的替身）；queueFromBase 是记录的目标分支（为空表示
	// 不区分，模拟升级前的旧行）。更新期间 PR 改投目标分支时 baseRef 不同，不能沿用。
	queueFrom     map[string]string
	queueFromBase map[string]string

	// intercept 在请求到达模拟 GitHub 之前拦截：返回 true 表示已经写出响应。
	interceptMu sync.Mutex
	intercept   func(w http.ResponseWriter, r *http.Request) bool
}

// newHarness 创建测试环境：tester/demo 的 main 分支上有 README.md（还没有 AI 配置），Kiln 中已登记这个仓库。
func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	h := &harness{
		t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), dir: t.TempDir(), events: &recordingEvents{},
		queueFrom: map[string]string{}, queueFromBase: map[string]string{},
	}
	h.git("init", "-q", "-b", "main")
	h.commitOn("main", map[string]string{"README.md": "demo\n"}, "init")
	h.fake = fakegithub.New(testAccount)
	t.Cleanup(h.fake.Close)
	h.repo = &fakegithub.Repo{ID: testRepoID, Owner: testAccount, Name: "demo", Dir: h.dir, Private: true}
	h.fake.AddRepo(h.repo)
	if err := store.UpsertRepo(h.ctx, h.db, store.RepoMeta{
		ID: testRepoID, InstallationID: h.fake.InstallationID, OwnerID: 1, Owner: testAccount, Name: "demo", Private: true,
		DefaultBranch: "main", CloneURL: "https://example.invalid/tester/demo.git",
	}); err != nil {
		t.Fatal(err)
	}

	target, err := url.Parse(h.fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.interceptMu.Lock()
		f := h.intercept
		h.interceptMu.Unlock()
		if f != nil && f(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	h.client = &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: api.URL, UploadURL: api.URL}

	log := slog.New(slog.DiscardHandler)
	hub := notify.NewHub()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.writer = outbox.NewWriter(true)
	h.worker = outbox.NewWorker(outbox.Deps{DB: h.db, Hub: hub, Log: log})
	h.worker.Handle(KindPRReview, NewPublisher(h.db, h.client, testPublicURL).Deliver)
	h.worker.HandleDead(KindPRReview, OnDead)
	h.typ = NewType(h.writer, h.events)
	h.core = ai.New(ai.Config{LeaseTTL: testLease, CancelGrace: time.Minute, AcquireLongPoll: time.Second, SweepInterval: time.Hour},
		ai.Deps{DB: h.db, Logs: logs, Outbox: h.writer, Hub: hub, Log: log, Types: ai.NewRegistry(h.typ)})
	h.rev = New(Deps{DB: h.db, GitHub: h.client, Core: h.core, Git: gitread.New(), UpdatedFrom: h.updatedFrom, Log: log})
	return h
}

// updatedFrom 是 UpdatedFromFunc 的替身：按 queueFrom 返回合并队列更新前的 head；记录了目标分支且与 baseRef 不同时
// 不返回（合并队列按目标分支隔离，PLAN #28.5）。
func (h *harness) updatedFrom(_ context.Context, _ store.Querier, repoID int64, pr int, head, baseRef string) (string, error) {
	if repoID != testRepoID || pr != aiPR {
		return "", nil
	}
	if b := h.queueFromBase[head]; b != "" && b != baseRef {
		return "", nil
	}
	return h.queueFrom[head], nil
}

// setIntercept 设置请求拦截（nil 表示取消）。
func (h *harness) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.interceptMu.Lock()
	h.intercept = f
	h.interceptMu.Unlock()
}

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

// commitOn 在分支 branch（不存在时从 main 创建）上写入 files 并提交，返回新的 commit；之后回到 main。
func (h *harness) commitOn(branch string, files map[string]string, msg string) string {
	h.t.Helper()
	if branch != "main" {
		if h.branchHead(branch) == "" {
			h.git("branch", branch, "main")
		}
		h.git("checkout", "-q", branch)
		defer h.git("checkout", "-q", "main")
	}
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

// branchHead 返回分支的 commit（不存在时为空）。
func (h *harness) branchHead(branch string) string {
	h.t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(h.ctx), "git", "rev-parse", "--verify", "-q", "refs/heads/"+branch)
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

// tx 在事务中执行 f。
func (h *harness) tx(f func(tx pgx.Tx) error) {
	h.t.Helper()
	if err := h.db.Tx(h.ctx, f); err != nil {
		h.t.Fatal(err)
	}
}

// ---------- 规划与执行 ----------

// enableReview 在 main 上写入 AI 配置 config，返回 main 的新 head（PR 的 base）。
func (h *harness) enableReview(config string) string {
	h.t.Helper()
	return h.commitOn("main", map[string]string{agentcfg.ConfigPath: config}, "开启 AI 审计")
}

// prEvent 返回 PR aiPR（来源分支 featureRef，目标 main）的 head 为 head、base 为 base 的事件。
func (h *harness) prEvent(action, head, base string) *Event {
	h.t.Helper()
	repo, err := store.GetRepo(h.ctx, h.db, testRepoID)
	if err != nil {
		h.t.Fatal(err)
	}
	h.fake.PullRequestPayload(h.repo, action, aiPR, featureRef, head, "main", base)
	return &Event{
		Repo: repo, Event: eventPullRequest, Ref: fmt.Sprintf("%s%d/head", gitref.PullPrefix, aiPR), SHA: head, Action: action,
		PRNumber: aiPR, BaseRef: "main", BaseSHA: base, HeadRef: featureRef, Sender: "bob", Source: store.SourceWebhook,
	}
}

// planPR 规划 ev 的审计，要求新建了请求，返回请求 ID。
func (h *harness) planPR(ev *Event) int64 {
	h.t.Helper()
	id, err := h.rev.PlanEvent(h.ctx, ev)
	if err != nil {
		h.t.Fatal(err)
	}
	if id == 0 {
		h.t.Fatal("没有新建审计请求")
	}
	return id
}

// tasks 返回请求的任务（按 ID）。
func (h *harness) tasks(requestID int64) []*ai.Task {
	h.t.Helper()
	ts, err := ai.RequestTasks(h.ctx, h.db, requestID, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return ts
}

func (h *harness) request(id int64) *ai.Request {
	h.t.Helper()
	r, err := ai.GetRequest(h.ctx, h.db, id, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// addRunner 登记一台审计角色的 AI 执行节点，返回它的 ID。
func (h *harness) addRunner(name, host string) int64 {
	h.t.Helper()
	var id int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO runners (name, public_key, kind, version, last_seen_at)
		VALUES ($1, '\x00', 'ai', $2, now()) RETURNING id`, name, testRunnerVersion).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO ai_runners (runner_id, role, platform, protocol, host_id, orca_state, reported_at)
		VALUES ($1, $2, 'darwin/arm64', $3, $4, 'ready', now())`, id, store.RunnerRoleReview, agentcfg.AIProtocol, host)
	return id
}

// acquire 模拟 Runner runnerID 领取任务（与 ai.Core 的领取相同：同一事务中 running、epoch+1、写 attempt），返回 attempt。
func (h *harness) acquire(taskID, runnerID int64) int64 {
	h.t.Helper()
	var aid int64
	h.tx(func(tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(h.ctx, `UPDATE ai_tasks SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
			lease_expires_at=now()+make_interval(secs => $2), started_at=now() WHERE id=$1 AND state='pending' RETURNING epoch`,
			taskID, testLease.Seconds()).Scan(&epoch); err != nil {
			return fmt.Errorf("任务 %d 不是 pending：%w", taskID, err)
		}
		if err := tx.QueryRow(h.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, runner_name, host_id, acquire_request_id, runner_version)
			SELECT $1, $2, r.id, r.name, a.host_id, 'req', r.version FROM runners r JOIN ai_runners a ON a.runner_id=r.id WHERE r.id=$3
			RETURNING id`, taskID, epoch, runnerID).Scan(&aid); err != nil {
			return err
		}
		_, err := tx.Exec(h.ctx, `UPDATE ai_tasks SET current_attempt_id=$2 WHERE id=$1`, taskID, aid)
		return err
	})
	return aid
}

// finish 模拟 Runner 领取任务并提交结果（经 AI 核心，任务类型的 Finish 处理审计报告），返回 attempt。
func (h *harness) finish(taskID, runnerID int64, res ai.Result) int64 {
	h.t.Helper()
	aid := h.acquire(taskID, runnerID)
	if err := h.core.FinishAttempt(h.ctx, aid, res); err != nil {
		h.t.Fatal(err)
	}
	return aid
}

// reportResult 返回 Agent 写出报告 rep 时连接器提交的结果（报告要有全部必填字段，见 agentReport）。
func reportResult(rep *aireport.Report) ai.Result {
	return ai.Result{Outcome: store.OutcomeSucceeded, AgentVersion: "codex 1.2.3", AgentModel: "gpt-test", ReviewReport: agentReport(rep)}
}

// agentReport 把测试用的报告整理成 Agent 写出的形式：没有内容的数组写成 []（不是 null），version 由 Kiln 写入（这里保留，
// 解析时会被覆盖）。
func agentReport(rep *aireport.Report) []byte {
	if rep.Findings == nil {
		rep.Findings = []aireport.Finding{}
	}
	if rep.Checks == nil {
		rep.Checks = []aireport.Check{}
	}
	if rep.Unverified == nil {
		rep.Unverified = []string{}
	}
	if rep.EnvNotes == nil {
		rep.EnvNotes = []string{}
	}
	if rep.Preexisting == nil {
		rep.Preexisting = []aireport.Preexisting{}
	}
	if rep.CommandsRun == nil {
		rep.CommandsRun = []aireport.Command{}
	}
	return rep.Marshal()
}

// cleanReport 是没有发现问题的报告。
func cleanReport() *aireport.Report {
	return &aireport.Report{Version: aireport.Version, Verdict: aireport.VerdictNoIssues, Summary: "改动正确", Scope: "1 个变更文件"}
}

// ---------- 直接写入的请求（读取类接口的测试） ----------

// fixture 是直接写入数据库的审计请求。
type fixture struct {
	typ     string // 默认 review
	event   string // 默认 pull_request
	sha     string
	pr      int // 0 表示没有 PR
	baseRef string
	family  int64 // 0 表示自成一族
	state   ai.RequestState
	tasks   []ai.TaskState
}

// insertRequest 写入请求、明细和任务，返回请求 ID 和任务 ID。
func (h *harness) insertRequest(f fixture) (int64, []int64) {
	h.t.Helper()
	if f.typ == "" {
		f.typ = agentcfg.TaskReview
	}
	if f.event == "" {
		f.event = eventPullRequest
	}
	var family *int64
	if f.family != 0 {
		family = &f.family
	}
	var pr *int
	if f.pr != 0 {
		pr = &f.pr
	}
	var id int64
	var tasks []int64
	h.tx(func(tx pgx.Tx) error {
		if err := tx.QueryRow(h.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
			INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, source, family_id, state)
			SELECT id, $1, $2, $3, 'refs/heads/x', $4, 'webhook', coalesce($5, id), $6 FROM n RETURNING id`,
			f.typ, testRepoID, f.event, f.sha, family, f.state).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(h.ctx, `INSERT INTO ai_review_requests (request_id, pr_number, head_ref, base_ref) VALUES ($1, $2, $3, $4)`,
			id, pr, featureRef, f.baseRef); err != nil {
			return err
		}
		for i, s := range f.tasks {
			var tid int64
			if err := tx.QueryRow(h.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
				VALUES ($1, $2, $3, '{darwin/arm64}', 'codex', $2, '{}', $4, 60) RETURNING id`, id, fmt.Sprintf("review-%d", i), f.typ, s).Scan(&tid); err != nil {
				return err
			}
			tasks = append(tasks, tid)
		}
		return nil
	})
	return id, tasks
}

// insertAttempt 为任务写一个已结束的 attempt（由 runnerID 执行），返回它的 ID。
func (h *harness) insertAttempt(taskID, runnerID int64) int64 {
	h.t.Helper()
	var aid int64
	if err := h.db.QueryRow(h.ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, acquire_request_id, outcome, finished_at)
		VALUES ($1, 1, $2, 'req', 'failed', now()) RETURNING id`, taskID, runnerID).Scan(&aid); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`UPDATE ai_tasks SET current_attempt_id=$2, epoch=1, attempt_count=1 WHERE id=$1`, taskID, aid)
	return aid
}

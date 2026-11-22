package ingest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestAccountAllowed(t *testing.T) {
	t.Parallel()
	i := New(Config{AllowedAccounts: map[string]bool{"alice": true, "42": true}}, Deps{})
	tests := []struct {
		name  string
		id    int64
		login string
		want  bool
	}{
		{"登录名（不区分大小写）", 1, "Alice", true},
		{"账号 ID", 42, "someone", true},
		{"不在白名单中", 7, "mallory", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := i.accountAllowed(tt.id, tt.login); got != tt.want {
				t.Fatalf("accountAllowed(%d, %q) = %v，期望 %v", tt.id, tt.login, got, tt.want)
			}
		})
	}
}

// ---------- 测试环境 ----------
//
// harness 由真实的 Postgres（pgtest）、模拟的 GitHub（fakegithub，数据来自本地 git 仓库）和真实的
// planner、engine 组成。webhook 经 HTTP 投递给 HandleWebhook，再由测试直接调用一次处理批次（ingestBatch），
// 不依赖后台循环和等待。

const (
	// testSecret 是 webhook 密钥。
	testSecret = "whsecret"
	// testAccount 是模拟 GitHub 的账号（默认在白名单中）。
	testAccount = "tester"
	// testBot 是本 App 的 bot 账号（IsSelf 为 true）。
	testBot = "kiln-test[bot]"
	// demoRepoID 是默认仓库 tester/demo 的 ID。
	demoRepoID = 101
	// testPlatform 是 workflow 中 runs-on 的平台；规划不要求有 Runner。
	testPlatform = "linux/amd64"
)

// ciWorkflow 由 main 分支、v* tag 的 push 和指向 main 的 PR 触发，只有一个 job。
const ciWorkflow = `name: ci
on:
  push:
    branches: [main]
    tags: ["v*"]
  pull_request:
    branches: [main]
jobs:
  build:
    runs-on: ` + testPlatform + `
    steps:
      - run: echo build
`

// lintWorkflow 是另一个由 main 分支 push 触发的 workflow（同一个 commit 上产生第二个 run）。
const lintWorkflow = `name: lint
on:
  push:
    branches: [main]
jobs:
  lint:
    runs-on: ` + testPlatform + `
    steps:
      - run: echo lint
`

// testKey 是全部测试共用的 App 私钥（生成 RSA 密钥较慢，只生成一次）。
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// gitRepo 是作为模拟 GitHub 仓库内容的本地 git 仓库。
type gitRepo struct {
	t   *testing.T
	dir string
}

// newGitRepo 创建一个 main 分支上有一个 commit 的仓库，内容为 files。
func newGitRepo(t *testing.T, files map[string]string) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	g := &gitRepo{t: t, dir: t.TempDir()}
	g.git("init", "-q", "-b", "main")
	g.commit(files, "init")
	return g
}

func (g *gitRepo) git(args ...string) string {
	g.t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(g.t.Context()), "git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit 在当前分支上写入 files 并提交，返回新的 commit。
func (g *gitRepo) commit(files map[string]string, msg string) string {
	g.t.Helper()
	for name, content := range files {
		p := filepath.Join(g.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			g.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	g.git("add", "-A")
	g.git("commit", "-q", "--allow-empty", "-m", msg)
	return g.head("HEAD")
}

// head 返回 rev 指向的 commit。
func (g *gitRepo) head(rev string) string {
	g.t.Helper()
	return g.git("rev-parse", rev+"^{commit}")
}

// harnessOpts 是 newHarness 的选项。
type harnessOpts struct {
	cfg Config
	// noGitHub 为 true 时模拟没有配置 GitHub App（Deps.GitHub 为 nil）。
	noGitHub bool
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	fake   *fakegithub.Server
	hub    *notify.Hub
	engine *engine.Engine
	ing    *Ingester
	// webhookURL 是 HandleWebhook 的地址。
	webhookURL string
	// demo 是默认仓库 tester/demo（私有，属于默认 installation），git 是它的内容。
	demo *fakegithub.Repo
	git  *gitRepo

	// intercept 在请求到达模拟 GitHub 之前拦截：返回 true 表示已经写出响应（用于模拟 fakegithub 没有的接口或故障）。
	interceptMu sync.Mutex
	intercept   func(w http.ResponseWriter, r *http.Request) bool
	// respHeaders 是附加到模拟 GitHub 每个响应上的头部（例如模拟剩余配额）。
	respHeaders http.Header

	deliveries atomic.Int64
	// queue 记录对合并队列的调用。
	queue *recordingQueue
	// aiDev 记录交给 AI 开发的 issues 事件。
	aiDev *recordingAIDev
}

// newHarness 创建测试环境：白名单为 testAccount，仓库 tester/demo 带 ciWorkflow，Kiln 中还没有任何 installation 和仓库。
func newHarness(t *testing.T, opts ...func(*harnessOpts)) *harness {
	t.Helper()
	o := harnessOpts{cfg: Config{WebhookSecret: testSecret, AllowedAccounts: map[string]bool{testAccount: true}}}
	for _, f := range opts {
		f(&o)
	}
	h := &harness{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), hub: notify.NewHub()}
	h.fake = fakegithub.New(testAccount)
	t.Cleanup(h.fake.Close)
	h.git = newGitRepo(t, map[string]string{".kiln/workflows/ci.yml": ciWorkflow, "README.md": "demo\n"})
	h.demo = &fakegithub.Repo{ID: demoRepoID, Owner: testAccount, Name: "demo", Dir: h.git.dir, Private: true}
	h.fake.AddRepo(h.demo)

	target, err := url.Parse(h.fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		h.interceptMu.Lock()
		defer h.interceptMu.Unlock()
		maps.Copy(resp.Header, h.respHeaders)
		return nil
	}
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

	var client *gh.Client
	if !o.noGitHub {
		client = &gh.Client{AppID: h.fake.AppID, Key: testKey(), APIURL: api.URL, UploadURL: api.URL}
	}
	h.build(o.cfg, client)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ing.HandleWebhook(w, r) }))
	t.Cleanup(srv.Close)
	h.webhookURL = srv.URL
	return h
}

// build 组装 engine、planner、AI 审计和 Ingester（与 server.init 相同的接线）。
func (h *harness) build(cfg Config, client *gh.Client) {
	log := slog.New(slog.DiscardHandler)
	w := outbox.NewWriter(client != nil)
	h.engine = engine.New(engine.Config{InfraRetryMax: 2, LeaseTTL: time.Minute}, engine.Deps{
		DB: h.db, CAS: &cas.FSStore{Root: h.t.TempDir()}, Outbox: w, Hub: h.hub, Log: log,
	})
	git := gitread.New()
	p := planner.New(planner.Deps{DB: h.db, GitHub: client, Git: git, Engine: h.engine, Outbox: w, Hub: h.hub, Log: log})
	aiLogs, err := logstore.New(h.t.TempDir())
	if err != nil {
		h.t.Fatal(err)
	}
	core := ai.New(ai.Config{LeaseTTL: time.Minute, CancelGrace: time.Minute, SweepInterval: time.Hour}, ai.Deps{
		DB: h.db, GitHub: client, Logs: aiLogs, Outbox: w, Hub: h.hub, Log: log,
		Types: ai.NewRegistry(review.NewType(w, w)),
	})
	reviews := review.New(review.Deps{DB: h.db, GitHub: client, Core: core, Git: git, UpdatedFrom: mergequeue.UpdatedFrom, Log: log})
	if h.queue == nil {
		h.queue = &recordingQueue{}
	}
	if h.aiDev == nil {
		h.aiDev = &recordingAIDev{}
	}
	h.ing = New(cfg, Deps{
		DB: h.db, GitHub: client, Planner: p, Reviews: reviews, Outbox: w, Engine: h.engine, Hub: h.hub, Log: log,
		IsSelf:     func(login string) bool { return strings.EqualFold(login, testBot) },
		MergeQueue: h.queue, AIDev: h.aiDev,
	})
}

// resetGitHubClient 换用新的 GitHub 客户端和 planner（没有限流状态和缓存），模拟限流恢复后的进程状态。
func (h *harness) resetGitHubClient() {
	h.t.Helper()
	old := h.ing.gh
	client := &gh.Client{AppID: old.AppID, Key: old.Key, APIURL: old.APIURL, UploadURL: old.UploadURL}
	cfg := h.ing.cfg
	h.build(cfg, client)
}

// setIntercept 设置请求拦截（nil 表示取消）。
func (h *harness) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.interceptMu.Lock()
	h.intercept = f
	h.interceptMu.Unlock()
}

// setResponseHeaders 设置附加到模拟 GitHub 每个响应上的头部（nil 表示取消）。
func (h *harness) setResponseHeaders(hdr http.Header) {
	h.interceptMu.Lock()
	h.respHeaders = hdr
	h.interceptMu.Unlock()
}

// addRepo 在模拟 GitHub 中增加一个带 ciWorkflow 的仓库（不通知 Kiln）；inst 为 0 表示默认 installation。
func (h *harness) addRepo(id int64, name string, inst int64, private bool) (*fakegithub.Repo, *gitRepo) {
	h.t.Helper()
	g := newGitRepo(h.t, map[string]string{".kiln/workflows/ci.yml": ciWorkflow, "README.md": name + "\n"})
	r := &fakegithub.Repo{ID: id, Owner: testAccount, Name: name, Dir: g.dir, Private: private, Installation: inst}
	h.fake.AddRepo(r)
	return r, g
}

// newDelivery 返回一个新的 delivery ID。
func (h *harness) newDelivery() string {
	return fmt.Sprintf("delivery-%d", h.deliveries.Add(1))
}

// send 以 GitHub 的格式（签名、头部）投递 webhook，要求 HandleWebhook 接受它。
func (h *harness) send(event, delivery string, payload any) {
	h.t.Helper()
	if err := fakegithub.SendWebhookDelivery(h.ctx, h.webhookURL, testSecret, event, delivery, payload); err != nil {
		h.t.Fatalf("投递 %s：%v", event, err)
	}
}

// process 同步处理全部到期的事件（与 Run 中的一轮相同）。
func (h *harness) process() {
	h.t.Helper()
	for h.ing.ingestBatch(h.ctx) > 0 {
	}
}

// deliver 投递一个新的 webhook 并处理，返回事件记录。
func (h *harness) deliver(event string, payload any) eventRow {
	h.t.Helper()
	d := h.newDelivery()
	h.send(event, d, payload)
	h.process()
	return h.event(d)
}

// deliverOK 与 deliver 相同，并要求事件已处理完成（没有失败或推迟）。
func (h *harness) deliverOK(event string, payload any) string {
	h.t.Helper()
	e := h.deliver(event, payload)
	if !e.Processed || e.Error != "" {
		h.t.Fatalf("%s 事件没有处理完成：%+v", event, e)
	}
	return e.Result
}

// eventRow 是 events 表中的一条记录。
type eventRow struct {
	Processed   bool
	Result      string
	Error       string
	Attempts    int
	NextAttempt time.Time
}

func (h *harness) event(delivery string) eventRow {
	h.t.Helper()
	var e eventRow
	err := h.db.QueryRow(h.ctx, `SELECT processed_at IS NOT NULL, coalesce(result,''), coalesce(error,''), attempts, next_attempt_at
		FROM events WHERE delivery_id=$1`, delivery).Scan(&e.Processed, &e.Result, &e.Error, &e.Attempts, &e.NextAttempt)
	if err != nil {
		h.t.Fatalf("读取事件 %s：%v", delivery, err)
	}
	return e
}

// runRow 是 runs 表中与事件接入有关的字段。
type runRow struct {
	ID           int64
	Workflow     string
	Event        string
	Ref          string
	SHA          string
	Source       string
	State        string
	PRNumber     *int
	SenderIsBot  bool
	CurrentRerun int
}

// runs 返回仓库的全部 run（按 ID）。
func (h *harness) runs(repoID int64) []runRow {
	h.t.Helper()
	rows, err := h.db.Query(h.ctx, `SELECT id, workflow, event, ref, sha, source, state, pr_number, sender_is_bot, current_rerun
		FROM runs WHERE repo_id=$1 ORDER BY id`, repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[runRow])
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

// lastRun 要求仓库正好有 n 个 run，返回最后一个。
func (h *harness) lastRun(repoID int64, n int) runRow {
	h.t.Helper()
	runs := h.runs(repoID)
	if len(runs) != n {
		h.t.Fatalf("仓库 %d 有 %d 个 run，期望 %d：%+v", repoID, len(runs), n, runs)
	}
	return runs[n-1]
}

// refHead 返回 ref_heads 中的记录；没有时 ok 为 false。
func (h *harness) refHead(repoID int64, ref string) (sha, object string, ok bool) {
	h.t.Helper()
	err := h.db.QueryRow(h.ctx, `SELECT sha, object_sha FROM ref_heads WHERE repo_id=$1 AND ref=$2`, repoID, ref).Scan(&sha, &object)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return sha, object, true
}

// refBase 返回 ref_heads 中 PR ref 的 head 是按哪个目标分支规划的。
func (h *harness) refBase(repoID int64, ref string) string {
	h.t.Helper()
	var base string
	if err := h.db.QueryRow(h.ctx, `SELECT base_ref FROM ref_heads WHERE repo_id=$1 AND ref=$2`, repoID, ref).Scan(&base); err != nil {
		h.t.Fatal(err)
	}
	return base
}

// repo 返回 Kiln 中的仓库记录；没有时返回 nil。
func (h *harness) repo(id int64) *store.Repo {
	h.t.Helper()
	r, err := store.GetRepo(h.ctx, h.db, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// installationState 返回 installation 在本地的状态。
func (h *harness) installationState(id int64) store.InstallationState {
	h.t.Helper()
	s, err := store.GetInstallationState(h.ctx, h.db, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// exec 执行一条 SQL（构造测试数据）。
func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// cancelRun 用状态机取消 run（让它进入终态，以便重跑）。
func (h *harness) cancelRun(id int64) {
	h.t.Helper()
	if err := h.db.Tx(h.ctx, func(tx pgx.Tx) error { return h.engine.CancelRun(h.ctx, tx, id, store.CancelByUser) }); err != nil {
		h.t.Fatalf("取消 run %d：%v", id, err)
	}
}

// requestsSince 返回模拟 GitHub 在第 n 个请求之后收到的请求。
func (h *harness) requestsSince(n int) []string { return h.fake.Requests()[n:] }

// ---------- payload ----------

// pushPayload 构造仓库 r 的 push 事件。
func (h *harness) pushPayload(r *fakegithub.Repo, ref, before, after string) map[string]any {
	return h.fake.PushPayload(r, ref, before, after)
}

// deletePayload 构造删除 ref 的 push 事件。
func (h *harness) deletePayload(r *fakegithub.Repo, ref, before string) map[string]any {
	p := h.fake.PushPayload(r, ref, before, gitref.ZeroSHA)
	p["deleted"] = true
	return p
}

// prPayload 构造同仓库分支的 pull_request 事件。
func (h *harness) prPayload(r *fakegithub.Repo, action string, number int, headRef, headSHA, baseRef string) map[string]any {
	return h.fake.PullRequestPayload(r, action, number, headRef, headSHA, baseRef, gitref.ZeroSHA)
}

// withSender 把事件的 sender 换成 login（typ 为 User 或 Bot）。
func withSender(p map[string]any, login, typ string) map[string]any {
	p["sender"] = map[string]any{"login": login, "id": 1, "type": typ}
	return p
}

// field 返回 payload 中的对象字段 key（用于修改 payload）；不是对象时终止测试。
func field(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("payload 中的 %s 不是对象：%v", key, m)
	}
	return v
}

package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/gittest"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/ingest"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestPagesRender 用与页面处理器相同类型的数据编码每个页面的数据（JSON），确认都能编码为 JSON 对象（MarshalJSON 没有出错）。
// 数据库查询部分由各页面处理器的测试（pgtest）覆盖。
func TestPagesRender(t *testing.T) {
	now := time.Now()
	run := &cistore.Run{
		ID: 7, Workflow: "ci.yml", WorkflowName: "CI", Event: "push", Ref: "refs/heads/main", SHA: "0123456789abcdef0123",
		Source: "webhook", SenderLogin: "alice", CurrentRerun: 1, State: store.RunActive,
		ConfigError: new("bad"), Warnings: new("warn"), CreatedAt: now,
	}
	job := &cistore.Job{
		ID: 11, Kind: workflow.JobKindPublish, RunID: 7, Rerun: 1, DisplayName: "build (a)", Platform: "linux/amd64",
		State: store.JobPending, Reason: new("等待 Server 执行发布"), ActionKey: new("abcdef0123456789"), CachedFrom: new(int64(3)),
		StartedAt: &now,
	}
	jv := jobView{Job: job, Runner: "mini", Needs: []string{"lint"}, Started: now.Unix()}
	se := &session{Login: "alice", Role: roleAdmin, Exp: now.Add(time.Hour).Unix()}

	cases := map[string]any{
		"login": &loginPage{Next: "/runs", GitHubURL: "/auth/github?next=%2Fruns", Notice: message(msgLoginNoticeExpired), Form: failedLoginForm()},
		"error": &errorPage{Status: 500, Heading: message(msgErrServerHeading), Reason: message(msgErrServerReasonGet), Ref: "abc", Links: []errorLink{listLink()}},
		"overview": &overviewPage{
			Me: "alice", MineURL: "/runs?actor=alice",
			Running: []runView{{Run: run, Repo: "tester/demo", States: []string{"succeeded", "running", "pending"}}},
			Queued:  []runView{{Run: run, Repo: "tester/demo"}}, Failures: []runView{{Run: run, Repo: "tester/demo"}},
			Stats:        overviewStats{Total: 4, Succeeded: 2, Failed: 1, Active: 1},
			Runners:      []*runnerView{{ID: 1, Name: "mini", Pool: "default", Online: true, Jobs: []cistore.RunningJob{{JobID: 11, Name: "build", Repo: "tester/demo"}}}},
			RunnerCounts: runnerCounts{Total: 1, Online: 1, Accepting: 1, Jobs: 1}, MoreRunners: 2,
			Setup: []setupItem{
				{Key: "repos", Title: message(msgSetupReposTitle), URL: "/settings/repos", Action: message(msgSetupReposAction), Done: true},
				{Key: "bind", Title: message(msgSetupBindTitle), URL: accountURL, Action: message(msgSetupBindAction)},
			}, SetupDone: 1,
		},
		"runs": &runsPage{
			Filter: runFilter{Repo: "tester/demo", State: "failed", Workflow: "ci.yml", Ref: "main"}, RepoExact: "tester/demo",
			Runs: []runView{{Run: run, Repo: "tester/demo"}}, States: runStateOptions(), Workflows: []string{"ci.yml"},
			Paged: true, NewestID: 7, OldestID: 7, NewerURL: "/runs?after=7", OlderURL: "/runs?before=7", LatestURL: "/runs",
			Self: "/runs?before=8", NewURL: "/runs/new?repo=tester%2Fdemo", GitHub: "https://github.com",
		},
		"run_new":    testRunNew(),
		"run":        testRunPage(run, jv),
		"job":        testJobPage(run, jv),
		"runners":    testRunnersPage(now),
		"runner_new": testEnrollPage(now, enrollUnused, "kiln-enroll-x"),
		"settings": &connPage{Section: sectionConn, Docker: []*dockerView{
			{Account: "empty", Status: dockerUnconfigured, Missing: []string{"访问令牌"}},
			testDocker(now),
		}, Npm: []*npmView{{Account: "empty"}, testNpm(now)}},
		"settings_docker": &dockerPage{Section: sectionConn, Docker: testDocker(now), Form: failedForm(), DefaultRef: dockerDefaultRef},
		"settings_npm":    &npmPage{Section: sectionConn, Npm: testNpm(now), Form: failedForm(), DefaultRegistry: "https://registry.npmjs.org/"},
		"settings_vars": &varsIndexPage{
			Section:  sectionVars,
			Accounts: []scopeCount{{Name: "tester", URL: "/settings/scope?account=tester", Vars: 1}},
			Repos:    []scopeCount{{Name: "tester/demo", URL: "/settings/scope?repo=tester%2Fdemo", Secrets: 2}},
			Envs:     []scopeCount{{Name: "tester / docker-release", URL: "/x", Docker: true}},
		},
		"settings_scope": testScope(now),
		"settings_item": &itemPage{
			scopePage: testScope(now), Kind: kindSecrets, Name: "DOCKERHUB_TOKEN",
			Secret: &store.SecretMeta{Name: "DOCKERHUB_TOKEN", UpdatedBy: "alice", UpdatedAt: now}, Form: failedForm(),
		},
		"settings_envs": &envsPage{Section: sectionEnvs, Envs: envViews(testEnvs)},
		"settings_env_new": &envNewPage{
			Section: sectionEnvs, Accounts: []string{"tester"}, Repo: "tester/demo", Form: failedForm(),
		},
		"settings_repos": &reposPage{Section: sectionRepos, Pick: "env", repoSearch: testRepoSearch()},
		"settings_account": &accountPage{
			Section: sectionAccount, OAuth: true, GitHubLogin: "/auth/github?next=%2Fsettings%2Faccount",
			Binding: &store.AdminBinding{GitHubID: 42, GitHubLogin: "alice", TokenLogin: true, BoundBy: "admin", BoundAt: now},
		},
		"settings_members": &membersPage{Section: sectionMembers, Admins: 1, Users: 1, Bound: true, Members: []member{
			{Login: "alice", Role: roleAdmin, Bound: true, Source: "绑定的 GitHub 账号", LastLogin: now, Me: true},
			{Login: "bob", Role: roleUser, Source: "KILN_USERS"},
		}},
		"settings_activity": &activityPage{
			Section: sectionActivity, Actor: "alice", Actors: []string{"alice", "bob"}, OlderURL: "/settings/activity?before=1",
			Entries: []activityEntry{{ID: 2, Actor: "alice", Action: "run.cancel", Detail: map[string]any{"run_id": float64(7)}, At: now}},
		},
		"merge_queue":      testMergeQueuePage(now),
		"ai_dev":           testAIDevPage(now),
		"ai_dev_item":      testAIDevItemPage(now),
		"settings_upgrade": testRunnerUpgradePage(now),
		"runner":           testRunnerPage(now),
		"ai_requests":      testAIRequestsPage(now),
		"ai_request":       testAIRequestPage(now),
		"ai_task":          testAITaskPage(now),
		"ai_runners":       testAIRunnersPage(now),
		"ai_runner":        &aiRunnerPage{Runner: testAIRunner(now), OfflineAfter: 120, LongPoll: 25},
		"settings_ops": &opsPage{
			Section: sectionOps, App: "kiln", PublicURL: "https://kiln.example.com", Version: "dev",
			Outbox: []*outbox.Item{{ID: 1, DedupeKey: "checkrun:job:1", Kind: "checkrun.job", State: "dead", Attempts: 3, LastError: "boom", UpdatedAt: now}},
		},
	}
	// 全部 32 个页面（前端 web/src/pages 中每个页面组件对应一个）
	if len(cases) != 32 {
		t.Fatalf("应有 32 个页面的测试数据，实际 %d 个", len(cases))
	}
	for page, data := range cases {
		t.Run(page, func(t *testing.T) {
			body := renderJSON(t, page, se, data)
			var p struct {
				Page string
				Data map[string]any
			}
			if err := json.Unmarshal([]byte(body), &p); err != nil || p.Page != page || p.Data == nil {
				t.Fatalf("页面 %s 的数据不是有效的 JSON 对象（%v）：%.300s", page, err, body)
			}
		})
	}
}

// testDocker 是一个已验证的 Docker Hub 连接，docker-release 在 tester/demo 中被覆盖。
func testDocker(now time.Time) *dockerView {
	env := envViews(testEnvs)[0]
	return &dockerView{
		Account: "tester", Env: &env, Username: "bob", Namespace: "team", Extra: 1, Status: dockerVerified,
		Token:    &store.SecretMeta{Name: admin.DockerTokenSecret, UpdatedBy: "alice", UpdatedAt: now},
		Verified: &dockerPublish{Image: "docker.io/team/app", Version: "v1.0.0", At: now},
	}
}

// testNpm 是一个已保存的 npm 配置。
func testNpm(now time.Time) *npmView {
	return &npmView{Account: "tester", Config: &npmConfig{Scope: "@tester", Registry: "https://registry.npmjs.org/", UpdatedBy: "alice", UpdatedAt: now}}
}

func testScope(now time.Time) *scopePage {
	env := envViews(testEnvs)[0]
	return &scopePage{
		Section: sectionEnvs,
		Account: "tester", Env: "docker-release", Label: "tester/docker-release", Kind: store.ScopeEnvironment, KindLabel: "账号级 environment",
		Environment: &env, Docker: true, DockerURL: dockerURL("tester"), EnvForm: failedForm(),
		Vars:       []store.Variable{{Name: "DOCKERHUB_USERNAME", Value: "bob"}},
		Secrets:    []store.SecretMeta{{Name: "DOCKERHUB_TOKEN", UpdatedBy: "alice", UpdatedAt: now}},
		CrumbLabel: "环境", CrumbURL: "/settings/environments",
	}
}

// failedForm 是每个字段都有错误的表单（页面数据中带着全部字段的错误）。
func failedForm() *formState {
	f := newForm()
	for _, k := range []string{formGeneral, "name", "value", "username", "namespace", "token", "scope", "registry", "allowed_publish_repos"} {
		f.fail(k, "错误："+k)
		f.Values[k] = "v-" + k
	}
	return f
}

// testEnvs 是一个账号级 environment 和覆盖它的同名仓库级 environment，外加另一个账号的同名 environment。
var testEnvs = []*apiv1.Environment{
	{Name: "docker-release", Account: "tester", RefPolicy: []string{"refs/tags/v*"}},
	{Name: "Docker-Release", Account: "tester", Repo: "tester/demo"},
	{Name: "docker-release", Account: "other", Repo: "other/app"},
	{Name: "release", Account: "tester", Repo: "tester/demo"},
}

func TestEnvViews(t *testing.T) {
	v := envViews(testEnvs)
	if got := v[0].OverriddenIn; len(got) != 1 || got[0] != "tester/demo" || v[0].Overrides {
		t.Errorf("账号级 docker-release 应只在 tester/demo 中被覆盖：%+v", v[0])
	}
	if !v[1].Overrides || len(v[1].OverriddenIn) != 0 {
		t.Errorf("tester/demo 的 docker-release 应覆盖账号级：%+v", v[1])
	}
	if v[2].Overrides || v[3].Overrides || len(v[3].OverriddenIn) != 0 {
		t.Errorf("其他账号或不同名的 environment 不应有覆盖关系：%+v %+v", v[2], v[3])
	}
	if want := "/settings/scope?account=tester&env=docker-release"; v[0].URL != want {
		t.Errorf("URL = %q，应为 %q", v[0].URL, want)
	}
	if want := "/settings/scope?env=Docker-Release&repo=tester%2Fdemo"; v[1].URL != want {
		t.Errorf("URL = %q，应为 %q", v[1].URL, want)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" refs/tags/v*,refs/heads/main\r\n\n alice  bob,")
	want := []string{"refs/tags/v*", "refs/heads/main", "alice", "bob"}
	if !slices.Equal(got, want) {
		t.Errorf("splitList = %q，应为 %q", got, want)
	}
	if got := splitList("  "); len(got) != 0 {
		t.Errorf("splitList(空白) = %q", got)
	}
}

func TestRoleOf(t *testing.T) {
	d := &Dashboard{cfg: Config{Admins: map[string]bool{"alice": true}, Users: map[string]bool{"42": true}}}
	if got := d.roleOf(nil, "Alice", 1); got != roleAdmin {
		t.Errorf("roleOf(Alice) = %q", got)
	}
	if got := d.roleOf(nil, "bob", 42); got != roleUser {
		t.Errorf("roleOf(bob, 42) = %q", got)
	}
	if got := d.roleOf(nil, "eve", 2); got != "" {
		t.Errorf("roleOf(eve) = %q", got)
	}
}

// failedLoginForm 是 admin token 错误时的登录表单。
func failedLoginForm() *formState {
	f := newForm()
	f.failText("token", message(msgLoginErrAdminWrong))
	return f
}

// ---- 连接真实数据库的 Dashboard（功能测试）----
//
// 页面处理器的功能测试用真实的 Dashboard（New）、admin.Service 和 Postgres（internal/pgtest），通过 httptest 发送请求，
// 断言状态码、页面数据（JSON）和数据库副作用。需要 GitHub 的操作（启动构建、重跑、刷新仓库、补对账）使用 internal/fakegithub。

func TestMain(m *testing.M) { pgtest.MainWith(m, gittest.Cleanup) }

// 测试数据中的账号、installation 和仓库，与 fakegithub 的默认值一致。
const (
	testAccount      = "tester"
	testAccountID    = 9001
	testInstallation = 777
	testRepo         = "tester/demo"
	testRepoID       = 501
)

// testDispatchWorkflow 是可以手动触发的 workflow，声明了一个 choice 类型的 input。
const testDispatchWorkflow = `name: build
on:
  workflow_dispatch:
    inputs:
      channel:
        type: choice
        options: [stable, beta]
        default: stable
jobs:
  build:
    runs-on: linux/amd64
    steps:
      - run: echo build
  test:
    needs: [build]
    runs-on: linux/amd64
    steps:
      - run: echo test
`

// testRepoFiles 是 fakegithub 仓库的内容：build.yml 可以手动触发，push.yml 不能。
var testRepoFiles = map[string]string{
	".kiln/workflows/build.yml": testDispatchWorkflow,
	".kiln/workflows/push.yml":  "on: push\njobs:\n  lint:\n    runs-on: linux/amd64\n    steps:\n      - run: echo lint\n",
	"README.md":                 "demo\n",
}

var (
	ghKeyOnce sync.Once
	ghKey     *rsa.PrivateKey
	errGHKey  error
)

// githubKey 返回签 App JWT 的私钥（整个测试进程生成一次；fakegithub 不校验签名）。
func githubKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	ghKeyOnce.Do(func() { ghKey, errGHKey = rsa.GenerateKey(rand.Reader, 2048) })
	if errGHKey != nil {
		t.Fatal(errGHKey)
	}
	return ghKey
}

// syncBuffer 是并发安全的日志缓冲区。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// webEnv 是连接真实数据库的 Dashboard：已经记录了账号 tester 的 installation 和仓库 tester/demo。
type webEnv struct {
	t    *testing.T
	ctx  context.Context
	db   *store.DB
	svc  *admin.Service
	d    *Dashboard
	mux  *http.ServeMux
	logs *logstore.Store
	// aiLogs 是 AI attempt 的日志。
	aiLogs *logstore.Store
	cas    *cas.FSStore
	log    *syncBuffer
	// core / docs 是 AI 领域的核心与文档快照服务（页面展示设计依据用）。
	core *ai.Core
	docs *docsync.Service
	// docProviderCalls 记录外部文档客户端被构造的次数（普通页面 GET 与正文读取不应触发）。
	docProviderCalls int32
	// docProvider 是显式验证使用的外部文档客户端（默认在构造时就失败，证明没有外部调用）。
	docProvider docsync.Factory
	// gh 是模拟的 GitHub（newGitHubWebEnv 才有），repoDir 是它的仓库 tester/demo 的本地 git 仓库。
	gh      *fakegithub.Server
	ghRepo  *fakegithub.Repo
	repoDir string
	// alice 是管理员，bob 是成员。
	alice, bob *client
}

// client 是一个登录用户：会话 cookie 和 CSRF token。
type client struct {
	login  string
	cookie *http.Cookie
	csrf   string
}

// newWebEnv 返回不连接 GitHub 的 Dashboard。
func newWebEnv(t *testing.T) *webEnv {
	t.Helper()
	return newWebEnvWith(t, false)
}

// newGitHubWebEnv 返回连接 fakegithub 的 Dashboard，仓库 tester/demo 的内容为 testRepoFiles。
func newGitHubWebEnv(t *testing.T) *webEnv {
	t.Helper()
	return newWebEnvWith(t, true)
}

func newWebEnvWith(t *testing.T, github bool) *webEnv {
	t.Helper()
	e := &webEnv{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t)), log: &syncBuffer{}}
	var err error
	if e.logs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	e.cas = &cas.FSStore{Root: t.TempDir()}
	cfg := Config{
		PublicURL: "https://kiln.example.com", Admins: map[string]bool{"alice": true, "carol": true}, Users: map[string]bool{"bob": true},
		LeaseTTL: 90 * time.Second, AcquireLongPoll: 20 * time.Second,
	}
	var client *gh.Client
	if github {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("没有 git")
		}
		e.repoDir = gitRepo(t, testRepoFiles)
		e.gh = fakegithub.New(testAccount)
		t.Cleanup(e.gh.Close)
		e.ghRepo = &fakegithub.Repo{ID: testRepoID, Owner: testAccount, Name: "demo", Dir: e.repoDir, Private: true}
		e.gh.AddRepo(e.ghRepo)
		client = &gh.Client{AppID: e.gh.AppID, Key: githubKey(t), APIURL: e.gh.URL, UploadURL: e.gh.URL, HTTP: &http.Client{Timeout: 10 * time.Second}}
		cfg.GitHubAPIURL = e.gh.URL
	}
	log := slog.New(slog.NewTextHandler(e.log, nil))
	hub := notify.NewHub()
	ob := outbox.NewWriter(client != nil)
	eng := engine.New(engine.Config{InfraRetryMax: 2, LeaseTTL: time.Minute}, engine.Deps{DB: e.db, CAS: e.cas, Outbox: ob, Hub: hub, Log: log})
	pl := planner.New(planner.Deps{DB: e.db, GitHub: client, Engine: eng, Outbox: ob, Hub: hub, Log: log})
	// 合并队列：连接 fakegithub 时通过它操作 PR；不连接 GitHub 时入队和移除返回 mergequeue.ErrNoForge
	var forgeFor mergequeue.ForgeFunc
	if client != nil {
		forgeFor = func(r *store.Repo) forge.Repo { return forge.GitHub(client, r.InstallationID, r.FullName()) }
	}
	mq := mergequeue.New(mergequeue.Deps{DB: e.db, Forge: forgeFor, Outbox: ob, Hub: hub, Log: log})
	ing := ingest.New(ingest.Config{AllowedAccounts: map[string]bool{testAccount: true}}, ingest.Deps{
		DB: e.db, GitHub: client, Planner: pl, Engine: eng, Hub: hub, Log: log, IsSelf: func(string) bool { return false },
	})
	dev := aidev.New(aidev.Deps{DB: e.db, Forge: aidev.ForgeFunc(forgeFor), GitHub: client, Outbox: ob, Hub: hub, Log: log, BranchTTL: testBranchTTL, HoldLabel: mergecfg.HoldLabel})
	if e.aiLogs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	core := ai.New(ai.Config{LeaseTTL: time.Minute, CancelGrace: time.Minute, AcquireLongPoll: time.Second, SweepInterval: time.Hour},
		ai.Deps{DB: e.db, GitHub: client, Logs: e.aiLogs, Outbox: ob, Hub: hub, Log: log, Types: ai.NewRegistry(review.NewType(ob, noEvents{}), dev.TaskType())})
	e.core = core
	failProvider := func(string, string) (docprovider.Provider, error) {
		atomic.AddInt32(&e.docProviderCalls, 1)
		return nil, errors.New("测试不应访问外部文档")
	}
	e.docProvider = failProvider
	e.docs = docsync.New(docsync.Deps{
		DB: e.db, MasterKey: []byte("0123456789abcdef0123456789abcdef"), Provider: failProvider,
	})
	core.SetDocs(e.docs)
	dev.SetCore(core)
	e.svc = admin.New(admin.Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, admin.Deps{
		DB: e.db, Engine: eng, Planner: pl, Ingest: ing, Logs: e.logs, CAS: e.cas, MergeQueue: mq, Hub: hub, Log: log,
		AutoTag: autotag.New(autotag.Deps{DB: e.db, Planner: tagPlanner{pl}, Outbox: ob, Hub: hub, Log: log}),
		AI:      core, Reviews: review.New(review.Deps{DB: e.db, GitHub: client, Core: core, Git: gitread.New(), Log: log}), AIDev: dev,
		DocProvider: func(baseURL, token string) (docprovider.Provider, error) { return e.docProvider(baseURL, token) },
	})
	keys := auth.NewKeys([]byte("token-key"), []byte("cas-key"), testAdminToken)
	e.d, err = New(cfg, Deps{DB: e.db, Admin: e.svc, AI: core, Keys: keys, Log: log, AppSlug: func() string { return "kiln-test" }})
	if err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	e.d.Register(e.mux)
	e.alice, e.bob = e.login("alice"), e.login("bob")
	e.must(store.UpsertInstallation(e.ctx, e.db, store.InstallationMeta{ID: testInstallation, AccountID: testAccountID, AccountLogin: testAccount}))
	e.addRepo(testRepoID, "demo")
	return e
}

// tagPlanner 用 CI 的规划器实现 autotag.Planner（与组装层相同：tag 按 push 事件规划，发送者是合并者）。
type tagPlanner struct{ p *planner.Planner }

func (t tagPlanner) PlanTagPush(ctx context.Context, ev autotag.TagPush) ([]int64, error) {
	return t.p.PlanEvent(ctx, &planner.Request{
		Repo: ev.Repo, Event: gitref.EventPush, Ref: gitref.TagPrefix + ev.Tag, SHA: ev.SHA,
		SenderLogin: ev.Sender, Source: store.SourceAutoTag,
	})
}

// noEvents 是测试中审计请求结束的事件（不转交其他领域）。
type noEvents struct{}

func (noEvents) PRVerified(context.Context, store.Execer, int64, int, bool) error { return nil }

// login 返回 login 用 GitHub 登录的会话（有效期一小时），角色按配置确定。
func (e *webEnv) login(login string) *client {
	exp := time.Now().Add(time.Hour)
	return &client{login: login, cookie: cookieFor(e.d, login, exp), csrf: e.d.csrfToken(&session{Login: login, Exp: exp.Unix()})}
}

// gitRepo 在临时目录中创建只有一个提交（分支 main）的 git 仓库。
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Init(t, dir, "main", files)
	return dir
}

// gitRun 在 dir 中执行 git 命令，不受用户全局配置（签名、hook）影响。
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
}

func (e *webEnv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// addRepo 在账号 tester 下记录一个启用中的仓库。
func (e *webEnv) addRepo(id int64, name string) {
	e.t.Helper()
	e.must(store.UpsertRepo(e.ctx, e.db, store.RepoMeta{
		ID: id, InstallationID: testInstallation, OwnerID: testAccountID, Owner: testAccount, Name: name, Private: true,
		DefaultBranch: "main", CloneURL: "https://github.example/tester/" + name + ".git",
	}))
}

// exec 执行 SQL，失败时终止测试。
func (e *webEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

// count 返回 SQL（SELECT count(*) ...）的结果。
func (e *webEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
	return n
}

// get 以 c 的身份（nil 表示未登录）请求 target 的页面数据（前端请求，带 X-Kiln-UI 请求头）。
func (e *webEnv) get(c *client, target string) *httptest.ResponseRecorder {
	q := testReq{target: target}
	if c != nil {
		q.cookie = c.cookie
	}
	return serve(e.mux, q)
}

// browse 以 c 的身份在浏览器中直接打开 target（不带 X-Kiln-UI 请求头，例如下载地址）；c 为 nil 时不登录。
func (e *webEnv) browse(c *client, target string) *httptest.ResponseRecorder {
	q := testReq{target: target, browser: true}
	if c != nil {
		q.cookie = c.cookie
	}
	return serve(e.mux, q)
}

// post 以 c 的身份提交表单（自动带上 c 的 CSRF token；form 中已有 csrf 时不覆盖），Referer 为站内的 /runs。
func (e *webEnv) post(c *client, target string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	q := testReq{method: http.MethodPost, target: target, form: form, referer: "https://kiln.example.com/runs"}
	if c != nil {
		q.cookie = c.cookie
		if !form.Has("csrf") {
			form.Set("csrf", c.csrf)
		}
	}
	return serve(e.mux, q)
}

// wantStatus 检查响应的状态码，失败时输出响应正文的开头。
func wantStatus(t *testing.T, what string, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		body := w.Body.String()
		if len(body) > 3000 {
			body = body[:3000]
		}
		t.Fatalf("%s：HTTP %d，应为 %d（Location %q）\n%s", what, w.Code, status, w.Header().Get("Location"), body)
	}
}

// wantRedirect 检查响应是跳转到 path（前端请求为 {"Redirect": 地址}，浏览器请求为 303；比较时去掉查询参数中的 flash），返回 flash 提示。
func wantRedirect(t *testing.T, what string, w *httptest.ResponseRecorder, path string) string {
	t.Helper()
	loc := redirectTarget(t, what, w)
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	flash := q.Get("flash")
	q.Del("flash")
	got := u.Path
	if len(q) > 0 {
		got += "?" + q.Encode()
	}
	if got != path {
		t.Fatalf("%s：跳转到 %q，应为 %q", what, loc, path)
	}
	return flash
}

// redirectTarget 返回响应跳转到的地址：前端请求的 {"Redirect": 地址}，或浏览器请求的 303 Location。
func redirectTarget(t *testing.T, what string, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code == http.StatusSeeOther {
		return w.Header().Get("Location")
	}
	wantStatus(t, what, w, http.StatusOK)
	var v uiRedirect
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Redirect == "" {
		t.Fatalf("%s：不是跳转：%s", what, w.Body.String())
	}
	return v.Redirect
}

// auditEntry 是一条审计日志。
type auditEntry struct {
	Actor  string
	Detail map[string]any
}

// audits 返回 action 的全部审计日志（按写入顺序）。
func (e *webEnv) audits(action string) []auditEntry {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT actor, detail FROM audit_log WHERE action=$1 ORDER BY id`, action)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditEntry, error) {
		var a auditEntry
		var detail []byte
		if err := row.Scan(&a.Actor, &detail); err != nil {
			return a, err
		}
		return a, json.Unmarshal(detail, &a.Detail)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

// ---- 测试数据：run、job、attempt、Runner ----

// runOpts 是 addRun 的选项。
type runOpts struct {
	repoID         int64
	workflow, name string
	ref            string
	state          store.RunState
	rerun          int
}

// addRun 直接写入一个 run（不经过规划），返回 run ID。
func (e *webEnv) addRun(o runOpts) int64 {
	e.t.Helper()
	if o.repoID == 0 {
		o.repoID = testRepoID
	}
	if o.workflow == "" {
		o.workflow = "build.yml"
	}
	if o.ref == "" {
		o.ref = "refs/heads/main"
	}
	if o.state == "" {
		o.state = store.RunSucceeded
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO runs (repo_id, workflow, workflow_name, event, ref, sha, source, dedupe_key, state, current_rerun)
		VALUES ($1,$2,$3,'push',$4,'0123456789abcdef0123456789abcdef01234567','webhook',gen_random_uuid()::text,$5,$6) RETURNING id`,
		o.repoID, o.workflow, o.name, o.ref, o.state, o.rerun).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// jobOpts 是 addJob 的选项。
type jobOpts struct {
	runID    int64
	rerun    int
	key      string
	kind     string
	state    store.JobState
	manifest string
	needs    []int64
}

// addJob 直接写入一个 job，返回 job ID。
func (e *webEnv) addJob(o jobOpts) int64 {
	e.t.Helper()
	if o.kind == "" {
		o.kind = "normal"
	}
	if o.state == "" {
		o.state = store.JobSucceeded
	}
	if o.key == "" {
		o.key = "build"
	}
	if o.needs == nil {
		o.needs = []int64{}
	}
	var manifest *string
	if o.manifest != "" {
		manifest = &o.manifest
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest, needs)
		VALUES ($1,$2,$3,$4,$4,$4,'{}','linux/amd64','host',$5,$6,$7) RETURNING id`, o.kind, o.runID, o.rerun, o.key, o.state, manifest, o.needs).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// addAttempt 为 job 写入一次尝试并设为 job 的当前尝试；outcome 为空表示还在执行。
func (e *webEnv) addAttempt(jobID, runnerID int64, outcome string) int64 {
	e.t.Helper()
	var out *string
	if outcome != "" {
		out = &outcome
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO attempts (job_id, epoch, runner_id, acquire_request_id, outcome)
		VALUES ($1, (SELECT coalesce(max(epoch),0)+1 FROM attempts WHERE job_id=$1), $2, gen_random_uuid()::text, $3) RETURNING id`,
		jobID, runnerID, out).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE jobs SET current_attempt_id=$2, attempt_count=attempt_count+1 WHERE id=$1`, jobID, id)
	return id
}

// runnerOpts 是 addRunner 的选项。
type runnerOpts struct {
	pool     string
	state    string
	drain    bool
	lastSeen *time.Time
}

// addRunner 直接写入一台 Runner，返回 ID。
func (e *webEnv) addRunner(name string, o runnerOpts) int64 {
	e.t.Helper()
	if o.pool == "" {
		o.pool = store.DefaultPool
	}
	if o.state == "" {
		o.state = store.RunnerActive
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO runners (name, kind, public_key, version, state, drain, last_seen_at)
		VALUES ($1,'ci','\x00','v1.0.0',$2,$3,$4) RETURNING id`, name, o.state, o.drain, o.lastSeen).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO ci_runners (runner_id, pool, platforms, executors) VALUES ($1,$2,ARRAY['linux/amd64'],ARRAY['host'])`, id, o.pool)
	return id
}

// appendLog 把 data 追加到日志 id（attempt ID，或发布 job 的 logstore.PublishLogID）的末尾。
func (e *webEnv) appendLog(id int64, data string) {
	e.t.Helper()
	size, err := e.logs.Size(id)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.logs.Append(id, size, []byte(data)); err != nil {
		e.t.Fatal(err)
	}
}

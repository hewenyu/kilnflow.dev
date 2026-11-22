package admin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/review"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitread"
	"github.com/hewenyu/kilnflow.dev/internal/server/ingest"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// admin 包的功能测试：Service 的每个操作都连接真实的 Postgres（internal/pgtest），
// 需要 GitHub 的操作（手动触发、重跑、刷新仓库、补对账）使用 internal/fakegithub 和本地 git 仓库。

func TestMain(m *testing.M) { pgtest.Main(m) }

// 测试数据中的账号、installation 和仓库，与 fakegithub 的默认值一致。
const (
	testAccount      = "tester"
	testAccountID    = 9001
	testInstallation = 777
	testRepo         = "tester/demo"
	testRepoID       = 501
	// testActor 是测试中执行操作的登录用户。
	testActor = "alice"
)

// testMasterKey 是加密 secret 的 master key（32 字节）。
var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

// testDispatchWorkflow 是可以手动触发的 workflow：一个 job，声明了 inputs。
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

// testPushWorkflow 是不能手动触发的 workflow。
const testPushWorkflow = `on: push
jobs:
  lint:
    runs-on: linux/amd64
    steps:
      - run: echo lint
`

// testRepoFiles 是 fakegithub 仓库的默认内容。
var testRepoFiles = map[string]string{
	".kiln/workflows/build.yml": testDispatchWorkflow,
	".kiln/workflows/push.yml":  testPushWorkflow,
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

// testEnv 是一个测试用的 Service 及其依赖：独立的数据库，已经记录了账号 tester 的 installation 和仓库 tester/demo。
type testEnv struct {
	t    *testing.T
	ctx  context.Context
	db   *store.DB
	svc  *Service
	logs *logstore.Store
	// aiLogs 是 AI attempt 的日志。
	aiLogs *logstore.Store
	cas    *cas.FSStore
	// gh 是模拟的 GitHub（newGitHubEnv 才有），repoDir 是它的仓库 tester/demo 的本地 git 仓库。
	gh      *fakegithub.Server
	repoDir string
}

// envOpts 是 newEnvWith 的选项。
type envOpts struct {
	// github 为 true 时启动 fakegithub，仓库内容为 files（nil 时为 testRepoFiles）。
	github bool
	files  map[string]string
	// masterKey 为 nil 时使用 testMasterKey。
	masterKey []byte
}

// newEnv 返回不连接 GitHub 的测试环境（手动触发、重跑、刷新仓库等操作会因为没有 GitHub App 而失败）。
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, envOpts{})
}

// newGitHubEnv 返回连接 fakegithub 的测试环境，仓库 tester/demo 的内容为 testRepoFiles。
func newGitHubEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, envOpts{github: true})
}

func newEnvWith(t *testing.T, o envOpts) *testEnv {
	t.Helper()
	e := &testEnv{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t))}
	var err error
	if e.logs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	e.cas = &cas.FSStore{Root: t.TempDir()}
	var client *gh.Client
	if o.github {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("没有 git")
		}
		files := o.files
		if files == nil {
			files = testRepoFiles
		}
		e.repoDir = gitRepo(t, files)
		e.gh = fakegithub.New(testAccount)
		t.Cleanup(e.gh.Close)
		e.gh.AddRepo(&fakegithub.Repo{ID: testRepoID, Owner: testAccount, Name: "demo", Dir: e.repoDir, Private: true})
		client = &gh.Client{AppID: e.gh.AppID, Key: githubKey(t), APIURL: e.gh.URL, UploadURL: e.gh.URL, HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	mk := o.masterKey
	if mk == nil {
		mk = testMasterKey
	}
	log := slog.New(slog.DiscardHandler)
	hub := notify.NewHub()
	ob := outbox.NewWriter(client != nil)
	eng := engine.New(engine.Config{InfraRetryMax: 2, LeaseTTL: time.Minute}, engine.Deps{DB: e.db, CAS: e.cas, Outbox: ob, Hub: hub, Log: log})
	pl := planner.New(planner.Deps{DB: e.db, GitHub: client, Engine: eng, Outbox: ob, Hub: hub, Log: log})
	ing := ingest.New(ingest.Config{AllowedAccounts: map[string]bool{testAccount: true}}, ingest.Deps{
		DB: e.db, GitHub: client, Planner: pl, Engine: eng, Hub: hub, Log: log, IsSelf: func(string) bool { return false },
	})
	var forgeFor aidev.ForgeFunc
	if client != nil {
		forgeFor = func(r *store.Repo) forge.Repo { return forge.GitHub(client, r.InstallationID, r.FullName()) }
	}
	dev := aidev.New(aidev.Deps{DB: e.db, Forge: forgeFor, GitHub: client, Outbox: ob, Hub: hub, Log: log, HoldLabel: mergecfg.HoldLabel})
	if e.aiLogs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	core := ai.New(ai.Config{LeaseTTL: time.Minute, CancelGrace: time.Minute, AcquireLongPoll: time.Second, SweepInterval: time.Hour},
		ai.Deps{DB: e.db, GitHub: client, Logs: e.aiLogs, Outbox: ob, Hub: hub, Log: log, Types: ai.NewRegistry(review.NewType(ob, noEvents{}), dev.TaskType())})
	dev.SetCore(core)
	reviews := review.New(review.Deps{DB: e.db, GitHub: client, Core: core, Git: gitread.New(), Log: log})
	e.svc = New(Config{MasterKey: mk}, Deps{
		DB: e.db, Engine: eng, Planner: pl, Ingest: ing, Logs: e.logs, CAS: e.cas, AI: core, Reviews: reviews, AIDev: dev, Hub: hub, Log: log,
	})
	e.seedRepo()
	return e
}

// noEvents 是测试中审计请求结束的事件（不转交其他领域）。
type noEvents struct{}

func (noEvents) PRVerified(context.Context, store.Execer, int64, int, bool) error { return nil }

// gitRepo 在临时目录中创建只有一个提交（分支 main）的 git 仓库。
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// gitRun 在 dir 中执行 git 命令，不受用户全局配置（签名、hook）影响。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedRepo 记录 installation 和仓库 tester/demo（与 fakegithub 的 ID 一致）。
func (e *testEnv) seedRepo() {
	e.t.Helper()
	e.must(store.UpsertInstallation(e.ctx, e.db, store.InstallationMeta{ID: testInstallation, AccountID: testAccountID, AccountLogin: testAccount}))
	e.addRepo(testRepoID, "demo")
}

// addRepo 在账号 tester 下记录一个启用中的仓库。
func (e *testEnv) addRepo(id int64, name string) {
	e.t.Helper()
	e.must(store.UpsertRepo(e.ctx, e.db, store.RepoMeta{
		ID: id, InstallationID: testInstallation, OwnerID: testAccountID, Owner: testAccount, Name: name, Private: true,
		DefaultBranch: "main", CloneURL: "https://github.example/tester/" + name + ".git",
	}))
}

func (e *testEnv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// exec 执行 SQL，失败时终止测试。
func (e *testEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

// count 返回 SQL（SELECT count(*) ...）的结果。
func (e *testEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
	return n
}

// auditEntry 是一条审计日志。
type auditEntry struct {
	Actor, Action string
	Detail        map[string]any
}

// audits 返回 action 的全部审计日志（按写入顺序）；action 为空时返回全部。
func (e *testEnv) audits(action string) []auditEntry {
	e.t.Helper()
	rows, err := e.db.Query(e.ctx, `SELECT actor, action, detail FROM audit_log WHERE $1='' OR action=$1 ORDER BY id`, action)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditEntry, error) {
		var a auditEntry
		var detail []byte
		if err := row.Scan(&a.Actor, &a.Action, &detail); err != nil {
			return a, err
		}
		return a, json.Unmarshal(detail, &a.Detail)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

// wantAudit 检查 action 恰好有一条审计日志，操作者为 actor，detail 包含 want 中的全部键值（数字按 JSON 解析后的 float64 比较）。
func (e *testEnv) wantAudit(action, actor string, want map[string]any) auditEntry {
	e.t.Helper()
	list := e.audits(action)
	if len(list) != 1 {
		e.t.Fatalf("审计日志 %s 有 %d 条，应为 1 条：%+v", action, len(list), list)
	}
	a := list[0]
	if a.Actor != actor {
		e.t.Errorf("审计日志 %s 的操作者为 %q，应为 %q", action, a.Actor, actor)
	}
	for k, v := range want {
		if got, err := json.Marshal(a.Detail[k]); err != nil || string(got) != string(mustJSON(e.t, v)) {
			e.t.Errorf("审计日志 %s 的 %s = %v，应为 %v（全部：%v）", action, k, a.Detail[k], v, a.Detail)
		}
	}
	return a
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantCode 检查 err 是 code 的 connect 错误。
func wantCode(t *testing.T, what string, err error, code connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：应返回 %v 错误，实际成功", what, code)
	}
	if got := connect.CodeOf(err); got != code {
		t.Fatalf("%s：错误码为 %v，应为 %v：%v", what, got, code, err)
	}
}

// ---- 测试数据：run、job、attempt、Runner、outbox ----

// runOpts 是 addRun 的选项。
type runOpts struct {
	repoID   int64
	workflow string
	ref      string
	state    store.RunState
	rerun    int
}

// addRun 直接写入一个 run（不经过规划），返回 run ID。
func (e *testEnv) addRun(o runOpts) int64 {
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
	err := e.db.QueryRow(e.ctx, `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state, current_rerun)
		VALUES ($1,$2,'push',$3,'0123456789abcdef0123456789abcdef01234567','webhook',gen_random_uuid()::text,$4,$5) RETURNING id`,
		o.repoID, o.workflow, o.ref, o.state, o.rerun).Scan(&id)
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
}

// addJob 直接写入一个 job，返回 job ID。
func (e *testEnv) addJob(o jobOpts) int64 {
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
	var manifest *string
	if o.manifest != "" {
		manifest = &o.manifest
	}
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, spec, platform, executor_kind, state, result_manifest)
		VALUES ($1,$2,$3,$4,$4,$4,'{}','linux/amd64','host',$5,$6) RETURNING id`, o.kind, o.runID, o.rerun, o.key, o.state, manifest).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// addAttempt 为 job 写入一次尝试（epoch 取下一个）并设为 job 的当前尝试；outcome 为空表示还在执行。
func (e *testEnv) addAttempt(jobID, runnerID int64, outcome string) int64 {
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
func (e *testEnv) addRunner(name string, o runnerOpts) int64 {
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
	e.exec(`INSERT INTO ci_runners (runner_id, pool, platforms, executors, capabilities)
		VALUES ($1,$2,ARRAY['linux/amd64'],ARRAY['host'],'{"host_fingerprint":{"os":"linux"}}')`, id, o.pool)
	return id
}

// addOutbox 写入一条 outbox 记录，返回 ID。
func (e *testEnv) addOutbox(key, state string, attempts int) int64 {
	e.t.Helper()
	var id int64
	err := e.db.QueryRow(e.ctx, `INSERT INTO outbox (dedupe_key, kind, state, attempts, last_error, next_attempt_at)
		VALUES ($1,'checkrun.job',$2,$3,'boom',now()+interval '1 hour') RETURNING id`, key, state, attempts).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// failOn 让表 table 上的 op（INSERT、UPDATE、DELETE）在满足 when（SQL 条件，可以引用 NEW / OLD）时失败，
// 用来构造事务中途失败的情况。
func (e *testEnv) failOn(table, op, when string) {
	e.t.Helper()
	e.exec(`CREATE FUNCTION kiln_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'kiln_test_fail'; END $$`)
	e.exec(`CREATE TRIGGER kiln_test_fail BEFORE ` + op + ` ON ` + table + ` FOR EACH ROW WHEN (` + when + `) EXECUTE FUNCTION kiln_test_fail()`)
}

// ---- admin.go ----

func TestService_SyncInstallations(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	// 本地还有一个 GitHub 上已经没有的仓库：刷新后停用
	e.addRepo(599, "gone")

	res, err := e.svc.SyncInstallations(e.ctx, testActor)
	if err != nil {
		t.Fatal(err)
	}
	if res.Installations != 1 || res.Repos != 1 {
		t.Errorf("刷新结果 = %+v，应为 1 个 installation、1 个仓库", res)
	}
	if n := e.count(`SELECT count(*) FROM repos WHERE id=599 AND active`); n != 0 {
		t.Errorf("GitHub 上没有的仓库应被停用")
	}
	st, err := store.LoadSyncStatus(e.ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Actor != testActor || st.Error != "" || st.Installations != 1 || st.Repos != 1 {
		t.Errorf("最近刷新的记录 = %+v", st)
	}
	e.wantAudit("repos.sync", testActor, map[string]any{"installations": 1, "repos": 1})
}

func TestService_SyncInstallationsFailure(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.gh.Close() // GitHub 不可用

	if _, err := e.svc.SyncInstallations(e.ctx, testActor); err == nil {
		t.Fatal("GitHub 不可用时刷新应失败")
	}
	// 失败也记录在"最近刷新"和审计日志中
	st, err := store.LoadSyncStatus(e.ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Error == "" || st.Actor != testActor {
		t.Errorf("最近刷新应记录失败原因：%+v", st)
	}
	if a := e.wantAudit("repos.sync", testActor, nil); a.Detail["error"] == nil {
		t.Errorf("审计日志应记录失败原因：%v", a.Detail)
	}
}

func TestService_SyncInstallationsWithoutGitHub(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.SyncInstallations(e.ctx, testActor); err == nil {
		t.Fatal("没有配置 GitHub App 时刷新应失败")
	}
}

func TestService_ReconcileRepo(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)

	// 第一次补对账：没有记录过的 ref 只记录为基线，不创建 run
	res, err := e.svc.ReconcileRepo(e.ctx, "Tester/Demo", testActor)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Baseline || res.Repo != testRepo || res.Branches != 1 || res.Runs != 0 {
		t.Errorf("第一次补对账 = %+v，应为基线、1 个分支、0 个 run", res)
	}
	if n := e.count(`SELECT count(*) FROM ref_heads WHERE repo_id=$1 AND ref='refs/heads/main'`, testRepoID); n != 1 {
		t.Errorf("基线应记录 refs/heads/main")
	}
	e.wantAudit("repo.reconcile", testActor, map[string]any{"repo": testRepo, "baseline": true, "runs": 0})

	// 推送了新的提交而 webhook 丢失：补对账补上 push 的 run
	if err := os.WriteFile(filepath.Join(e.repoDir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, e.repoDir, "commit", "-q", "-am", "change")
	res, err = e.svc.ReconcileRepo(e.ctx, testRepo, testActor)
	if err != nil {
		t.Fatal(err)
	}
	if res.Baseline || res.Runs != 1 {
		t.Errorf("第二次补对账 = %+v，应补上 1 个 run", res)
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE repo_id=$1 AND workflow='push.yml' AND source='reconcile'`, testRepoID); n != 1 {
		t.Errorf("应为 push.yml 补上 1 个 run，实际 %d 个", n)
	}
}

func TestService_ReconcileRepoErrors(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)

	_, err := e.svc.ReconcileRepo(e.ctx, "tester/nope", testActor)
	wantCode(t, "仓库不存在", err, connect.CodeInvalidArgument)
	if len(e.audits("repo.reconcile")) != 0 {
		t.Errorf("仓库不存在时不应写审计日志")
	}

	// 被 GitHub 限流：ResourceExhausted，说明稍后再试；审计日志记录失败
	e.gh.SetRateLimit(testInstallation, &fakegithub.RateLimit{Status: http.StatusForbidden, Primary: true, Until: time.Now().Add(time.Hour)})
	_, err = e.svc.ReconcileRepo(e.ctx, testRepo, testActor)
	wantCode(t, "限流", err, connect.CodeResourceExhausted)
	if !strings.Contains(err.Error(), "被 GitHub 限流") {
		t.Errorf("限流的错误应说明原因：%v", err)
	}
	if a := e.wantAudit("repo.reconcile", testActor, map[string]any{"repo": testRepo}); a.Detail["error"] == nil {
		t.Errorf("审计日志应记录失败原因：%v", a.Detail)
	}
}

func TestService_ListOutbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pending := e.addOutbox("a", store.OutboxPending, 1)
	dead := e.addOutbox("b", store.OutboxDead, 5)
	e.addOutbox("c", store.OutboxDone, 1)

	list, err := e.svc.ListOutbox(e.ctx, []string{store.OutboxPending, store.OutboxDead}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != dead || list[1].ID != pending {
		t.Fatalf("应按 ID 倒序列出 pending 和 dead 的记录：%+v", list)
	}
	if list[0].Attempts != 5 || list[0].LastError != "boom" || list[0].DedupeKey != "b" {
		t.Errorf("记录内容 = %+v", list[0])
	}
	if list, err := e.svc.ListOutbox(e.ctx, []string{store.OutboxDead}, 1); err != nil || len(list) != 1 {
		t.Errorf("只列出 dead：%v %+v", err, list)
	}
}

func TestService_RetryOutbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.addOutbox("checkrun:job:1", store.OutboxDead, 5)

	if err := e.svc.RetryOutbox(e.ctx, id, testActor); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	var due bool
	if err := e.db.QueryRow(e.ctx, `SELECT state, attempts, next_attempt_at <= now() FROM outbox WHERE id=$1`, id).Scan(&state, &attempts, &due); err != nil {
		t.Fatal(err)
	}
	if state != store.OutboxPending || attempts != 0 || !due {
		t.Errorf("重试后 state=%s attempts=%d 立即投递=%v，应为 pending、0、是", state, attempts, due)
	}
	e.wantAudit("outbox.retry", testActor, map[string]any{"id": id})

	wantCode(t, "记录不存在", e.svc.RetryOutbox(e.ctx, id+100, testActor), connect.CodeNotFound)
	if len(e.audits("outbox.retry")) != 1 {
		t.Errorf("记录不存在时不应写审计日志")
	}
}

func TestClampInt32(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   int64
		want int32
	}{{0, 0}, {-5, -5}, {1 << 40, 1<<31 - 1}, {-(1 << 40), -1 << 31}} {
		if got := clampInt32(c.in); got != c.want {
			t.Errorf("clampInt32(%d) = %d，应为 %d", c.in, got, c.want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()
	if !isNotFound(errors.Join(errors.New("x"), pgx.ErrNoRows)) || isNotFound(errors.New("x")) {
		t.Error("isNotFound 判断错误")
	}
}

package planner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 测试仓库的固定信息（与 fakegithub 的默认 installation、账号一致）。
const (
	testOwner  = "tester"
	testRepoID = 100
)

// appKey 是测试用 GitHub App 的私钥，整个测试进程只生成一次（生成 RSA 密钥较慢）。
var appKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// testEnv 是一个测试的完整环境：独立的数据库、模拟的 GitHub、一个本地 git 仓库和 Planner。
type testEnv struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	gh     *fakegithub.Server
	client *gh.Client
	eng    *engine.Engine
	p      *Planner
	// fake 是默认仓库 tester/demo 在模拟 GitHub 中的记录，repo 是它在 Kiln 中的记录。
	fake *fakegithub.Repo
	repo *store.Repo
}

// newEnv 准备测试环境：默认仓库 tester/demo（ID testRepoID，私有，默认分支 main）还没有任何 commit。
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	db := store.New(pgtest.Pool(t))
	s := fakegithub.New(testOwner)
	t.Cleanup(s.Close)
	client := &gh.Client{AppID: s.AppID, Key: appKey(), APIURL: s.URL, UploadURL: s.URL}
	log := slog.New(slog.DiscardHandler)
	ob := outbox.NewWriter(true)
	hub := notify.NewHub()
	eng := engine.New(engine.Config{
		InfraRetryMax: 2, LeaseTTL: time.Minute, CancelGrace: 30 * time.Second,
		SweepInterval: 5 * time.Second, NonceRetention: 10 * time.Minute,
	}, engine.Deps{DB: db, CAS: &cas.FSStore{Root: t.TempDir()}, Outbox: ob, Hub: hub, Log: log})
	e := &testEnv{
		t: t, ctx: t.Context(), db: db, gh: s, client: client, eng: eng,
		p: New(Deps{DB: db, GitHub: client, Engine: eng, Outbox: ob, Hub: hub, Log: log}),
	}
	if err := store.UpsertInstallation(e.ctx, db, store.InstallationMeta{
		ID: s.InstallationID, AccountID: s.AccountID, AccountLogin: testOwner,
	}); err != nil {
		t.Fatal(err)
	}
	e.fake, e.repo = e.addRepo(testRepoID, "demo", s.InstallationID, true)
	return e
}

// addRepo 创建一个空的本地 git 仓库，登记到模拟 GitHub；inst 不为 -1 时也记录到 Kiln（启用中）。
func (e *testEnv) addRepo(id int64, name string, inst int64, private bool) (*fakegithub.Repo, *store.Repo) {
	e.t.Helper()
	dir := e.t.TempDir()
	e.gitIn(dir, "init", "-q", "-b", "main")
	fr := &fakegithub.Repo{ID: id, Owner: testOwner, Name: name, Dir: dir, Private: private, Installation: inst}
	e.gh.AddRepo(fr)
	if inst == -1 {
		return fr, nil
	}
	if err := store.UpsertRepo(e.ctx, e.db, store.RepoMeta{
		ID: id, InstallationID: inst, OwnerID: e.gh.AccountID, Owner: testOwner, Name: name, Private: private,
		DefaultBranch: "main", CloneURL: "file://" + dir,
	}); err != nil {
		e.t.Fatal(err)
	}
	r, err := store.GetRepo(e.ctx, e.db, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return fr, r
}

// gitIn 在 dir 中执行 git，返回去掉首尾空白的输出。
func (e *testEnv) gitIn(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.CommandContext(e.ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// git 在默认仓库中执行 git。
func (e *testEnv) git(args ...string) string {
	e.t.Helper()
	return e.gitIn(e.fake.Dir, args...)
}

// commitIn 在仓库 dir 中写入文件（值为空字符串表示删除）并提交，返回 commit sha。
func (e *testEnv) commitIn(dir string, files map[string]string) string {
	e.t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(p); err != nil {
				e.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	e.gitIn(dir, "add", "-A")
	e.gitIn(dir, "commit", "-q", "--allow-empty", "-m", "c")
	return e.gitIn(dir, "rev-parse", "HEAD")
}

// commit 在默认仓库中提交。
func (e *testEnv) commit(files map[string]string) string {
	e.t.Helper()
	return e.commitIn(e.fake.Dir, files)
}

// wf 返回 workflow 文件在仓库中的路径。
func wf(name string) string { return ".kiln/workflows/" + name }

// pushReq 返回默认仓库的分支或 tag push（before 为空表示新建的 ref）。
func (e *testEnv) pushReq(ref, sha string) *Request {
	return &Request{Repo: e.repo, Event: "push", Ref: ref, SHA: sha, Before: gitref.ZeroSHA, SenderLogin: testOwner, Source: SourceWebhook}
}

// prReq 返回默认仓库的 pull_request（head 分支 feat → base 分支 main），并在模拟 GitHub 中登记 PR 的 head。
func (e *testEnv) prReq(number int, action, headSHA string) *Request {
	e.gh.PullRequestPayload(e.fake, action, number, "feat", headSHA, "main", "")
	return &Request{
		Repo: e.repo, Event: "pull_request", Ref: fmt.Sprintf("refs/pull/%d/head", number), SHA: headSHA, Action: action,
		PRNumber: number, BaseRef: "main", HeadRef: "feat", SenderLogin: testOwner, Source: SourceWebhook,
	}
}

// plan 规划事件，出错时终止测试。
func (e *testEnv) plan(req *Request) []int64 {
	e.t.Helper()
	ids, err := e.p.PlanEvent(e.ctx, req)
	if err != nil {
		e.t.Fatalf("PlanEvent：%v", err)
	}
	return ids
}

// planOne 规划事件，要求正好新建一个 run，返回它。
func (e *testEnv) planOne(req *Request) *cistore.Run {
	e.t.Helper()
	ids := e.plan(req)
	if len(ids) != 1 {
		e.t.Fatalf("应新建 1 个 run，实际 %v", ids)
	}
	return e.run(ids[0])
}

// run 读取 run。
func (e *testEnv) run(id int64) *cistore.Run {
	e.t.Helper()
	r, err := cistore.GetRun(e.ctx, e.db, id, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// jobs 返回 run 某一次运行的全部 job（job_key → job）。
func (e *testEnv) jobs(runID int64, rerun int) map[string]*cistore.Job {
	e.t.Helper()
	list, err := cistore.RerunJobs(e.ctx, e.db, runID, rerun, false)
	if err != nil {
		e.t.Fatal(err)
	}
	m := map[string]*cistore.Job{}
	for _, j := range list {
		m[j.JobKey] = j
	}
	return m
}

// rerunState 返回 run_reruns 中某一次运行的状态。
func (e *testEnv) rerunState(runID int64, rerun int) store.RunState {
	e.t.Helper()
	var s store.RunState
	if err := e.db.QueryRow(e.ctx, `SELECT state FROM run_reruns WHERE run_id=$1 AND rerun=$2`, runID, rerun).Scan(&s); err != nil {
		e.t.Fatalf("读取 run %d 第 %d 次运行：%v", runID, rerun, err)
	}
	return s
}

// countRuns 返回默认仓库的 run 数。
func (e *testEnv) countRuns() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE repo_id=$1`, e.repo.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// wantOutbox 断言 outbox 中有 dedupe_key 为 key 的记录。
func (e *testEnv) wantOutbox(key string) {
	e.t.Helper()
	var ok bool
	if err := e.db.QueryRow(e.ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE dedupe_key=$1)`, key).Scan(&ok); err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		e.t.Errorf("outbox 中应有 %s", key)
	}
}

// wantRunOutbox 断言 run 的汇总 Check Run 和全部 job 的 Check Run 都写入了 outbox。
func (e *testEnv) wantRunOutbox(runID int64, rerun int) {
	e.t.Helper()
	e.wantOutbox(fmt.Sprintf("checkrun:run:%d:%d", runID, rerun))
	for _, j := range e.jobs(runID, rerun) {
		if j.ReusedFrom == nil {
			e.wantOutbox(fmt.Sprintf("checkrun:job:%d", j.ID))
		}
	}
}

// envOpts 是 addEnvironment 的参数。
type envOpts struct {
	// repoLevel 为 true 时是仓库级 environment，否则是账号级。
	repoLevel bool
	refPolicy []string
	publishTo []string
}

// addEnvironment 在 Kiln 中创建默认仓库可以使用的 environment。
func (e *testEnv) addEnvironment(name string, o envOpts) {
	e.t.Helper()
	var repoID *int64
	if o.repoLevel {
		repoID = &e.repo.ID
	}
	if o.refPolicy == nil {
		o.refPolicy = []string{}
	}
	if o.publishTo == nil {
		o.publishTo = []string{}
	}
	if _, err := e.db.Exec(e.ctx, `INSERT INTO environments (owner_id, repo_id, name, ref_policy, allowed_publish_repos) VALUES ($1,$2,$3,$4,$5)`,
		e.repo.OwnerID, repoID, name, store.MustJSON(o.refPolicy), o.publishTo); err != nil {
		e.t.Fatal(err)
	}
}

// exec 执行一条 SQL，出错时终止测试。
func (e *testEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

// deref 返回指针指向的字符串，nil 时为空字符串。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// testManifest 是测试中充当 job 结果的 manifest digest（不需要真的存在于 CAS 中）。
const testManifest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

// finishRun 把 run 当前运行中的 job 置为给定的终态（没有列出的成功），然后由 engine 推进，使 run 结束。
// 成功的 job 带有结果 manifest，可以被"只重跑未成功的 job"沿用。
func (e *testEnv) finishRun(runID int64, states map[string]store.JobState) {
	e.t.Helper()
	r := e.run(runID)
	for key, j := range e.jobs(runID, r.CurrentRerun) {
		if j.State.Terminal() {
			continue
		}
		st, ok := states[key]
		if !ok {
			st = store.JobSucceeded
		}
		var manifest *string
		if st == store.JobSucceeded {
			m := testManifest
			manifest = &m
		}
		e.exec(`UPDATE jobs SET state=$2, result_manifest=$3, finished_at=now(), follows_job_id=NULL WHERE id=$1`, j.ID, st, manifest)
	}
	if err := e.db.Tx(e.ctx, func(tx pgx.Tx) error { return e.eng.AdvanceRun(e.ctx, tx, runID) }); err != nil {
		e.t.Fatal(err)
	}
	if got := e.run(runID); !got.State.Terminal() {
		e.t.Fatalf("run %d 应已结束，实际 %s", runID, got.State)
	}
}

// ---------- 常用的 workflow ----------

// matrixWorkflow：矩阵 build（两个 os）→ 汇总的 pack。
const matrixWorkflow = `name: matrix
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux/arm64
    strategy:
      matrix:
        os: [linux, darwin]
    steps:
      - run: echo ${{ matrix.os }}
  pack:
    needs: [build]
    runs-on: linux/arm64
    steps:
      - run: echo pack
`

// dispatchWorkflow：只能手动触发，有一个 input。
const dispatchWorkflow = `name: manual
on:
  workflow_dispatch:
    inputs:
      level:
        type: string
        default: low
jobs:
  go:
    runs-on: linux/arm64
    steps:
      - run: echo ${{ inputs.level }}
`

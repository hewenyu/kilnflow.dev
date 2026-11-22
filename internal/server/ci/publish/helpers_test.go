package publish

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// 测试中的源仓库（私有）和跨仓库发布的目标仓库（公开），都属于账号 tester、installation 777。
const (
	srcRepoID    = 101
	srcRepo      = "tester/myapp-src"
	targetRepoID = 102
	targetRepo   = "tester/myapp"
	ownerID      = 9001
)

// changelog 是源仓库中默认的 CHANGELOG.md。
const changelog = "# Changelog\n\n## v1.2.0\n\n- 新功能\n\n## v1.1.0\n\n- 旧的修复\n"

// pubEnv 是发布 worker 的测试环境：独立的数据库、本地 CAS 和日志、模拟的 GitHub（源仓库是本地 git 仓库）。
// 测试直接调用 Worker.publishOnce，不启动发布循环。
type pubEnv struct {
	t      *testing.T
	db     *store.DB
	fake   *fakegithub.Server
	cas    *cas.FSStore
	logs   *logstore.Store
	hub    *notify.Hub
	w      *Worker
	eng    *engine.Engine
	dir    string // 源仓库的本地 git 目录
	master []byte
	// faults 拦截发往 GitHub 的请求（模拟失败、崩溃、在请求之间改变状态）。
	faults *faultTransport
	seq    atomic.Int64
}

// envOpts 调整测试环境。
type envOpts struct {
	files    map[string]string // 源仓库的文件，默认只有 CHANGELOG.md
	noGitHub bool              // 不配置 GitHub App
	// targetPrivate 为 true 时跨仓库发布的目标仓库是私有的。
	targetPrivate bool
	// noTargetInKiln 为 true 时目标仓库不在 Kiln 中（不在 App 的安装范围内）。
	noTargetInKiln bool
	// targetOtherInstallation 为 true 时目标仓库在 Kiln 中属于另一个 installation（规划后被移走）。
	targetOtherInstallation bool
}

func newPubEnv(t *testing.T, opts ...func(*envOpts)) *pubEnv {
	t.Helper()
	o := envOpts{files: map[string]string{"CHANGELOG.md": changelog}}
	for _, f := range opts {
		f(&o)
	}
	e := &pubEnv{
		t: t, db: store.New(pgtest.Pool(t)), fake: fakegithub.New("tester"), cas: &cas.FSStore{Root: t.TempDir()},
		hub: notify.NewHub(), dir: gitRepo(t, o.files), master: bytes.Repeat([]byte{9}, secretbox.KeySize),
		faults: &faultTransport{next: http.DefaultTransport},
	}
	t.Cleanup(e.fake.Close)
	var err error
	if e.logs, err = logstore.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	e.fake.AddRepo(&fakegithub.Repo{ID: srcRepoID, Owner: "tester", Name: "myapp-src", Dir: e.dir, Private: true})
	e.fake.AddRepo(&fakegithub.Repo{ID: targetRepoID, Owner: "tester", Name: "myapp", Dir: e.dir, Private: o.targetPrivate})
	e.addRepo(srcRepoID, "myapp-src", true)
	if !o.noTargetInKiln {
		e.addRepo(targetRepoID, "myapp", o.targetPrivate)
	}
	if o.targetOtherInstallation {
		e.exec(`UPDATE repos SET installation_id=installation_id+1 WHERE id=$1`, targetRepoID)
	}
	var client *gh.Client
	if !o.noGitHub {
		client = &gh.Client{AppID: e.fake.AppID, Key: testRSAKey(t), APIURL: e.fake.URL, UploadURL: e.fake.URL, HTTP: &http.Client{Transport: e.faults}}
	}
	log := slog.New(slog.DiscardHandler)
	ob := outbox.NewWriter(true)
	eng := engine.New(engine.Config{InfraRetryMax: 2}, engine.Deps{DB: e.db, CAS: e.cas, Outbox: ob, Hub: e.hub, Log: log})
	e.eng = eng
	e.w = New(Config{WorkDir: t.TempDir(), MasterKey: e.master}, Deps{
		DB: e.db, GitHub: client, CAS: e.cas, Engine: eng, Outbox: ob, Logs: e.logs, Hub: e.hub, Log: log,
	})
	return e
}

// rsaKey 是 GitHub App 的测试私钥（生成较慢，整个包共用一把）。
var rsaKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsaKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// gitRepo 创建一个有一个 commit 的本地 git 仓库。
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *pubEnv) next() int64 { return e.seq.Add(1) }

func (e *pubEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.t.Context(), sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

func (e *pubEnv) addRepo(id int64, name string, private bool) {
	e.t.Helper()
	e.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, private, clone_url) VALUES ($1, $2, $3, 'tester', $4, $5, $6)`,
		id, e.fake.InstallationID, ownerID, name, private, "https://github.com/tester/"+name+".git")
}

// ---------- run 和 job ----------

// testRun 是一个由 tag push 触发的 run。
type testRun struct {
	id    int64
	tag   string
	sha   string
	rerun int
}

// tagRun 在源仓库 HEAD 上打 tag（已存在时沿用），创建由它触发的 active run。
func (e *pubEnv) tagRun(tag string) *testRun {
	e.t.Helper()
	if git(e.t, e.dir, "tag", "-l", tag) == "" {
		git(e.t, e.dir, "tag", tag)
	}
	sha := git(e.t, e.dir, "rev-list", "-n", "1", tag)
	ct, err := strconv.ParseInt(git(e.t, e.dir, "show", "-s", "--format=%ct", sha), 10, 64)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.addRun("refs/tags/"+tag, sha, ct)
}

// addRun 创建一个 active 的 run（rerun 0）。
func (e *pubEnv) addRun(ref, sha string, commitTime int64) *testRun {
	e.t.Helper()
	r := &testRun{sha: sha}
	r.tag, _ = strings.CutPrefix(ref, "refs/tags/")
	refName := ref[strings.LastIndex(ref, "/")+1:]
	ghCtx := store.MustJSON(map[string]any{"ref": ref, "ref_name": refName, "sha": sha, "repository": srcRepo})
	err := e.db.QueryRow(e.t.Context(), `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state, github_ctx, commit_time)
		VALUES ($1, 'release.yml', 'push', $2, $3, 'webhook', $4, 'active', $5, $6) RETURNING id`,
		srcRepoID, ref, sha, fmt.Sprintf("run-%d", e.next()), ghCtx, commitTime).Scan(&r.id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, 'active')`, r.id)
	return r
}

// upstreamFile 是上游输出中的一个文件。
type upstreamFile struct {
	content string
	exec    bool
}

// upstream 在 run 中插入一个已成功的上游 job：输出 output 包含 files，内容写入 CAS。返回 job ID。
func (e *pubEnv) upstream(r *testRun, name, output string, matrix map[string]any, files map[string]upstreamFile) int64 {
	e.t.Helper()
	om := cas.OutputManifest{Schema: cas.OutputSchema}
	for p, f := range files {
		om.Files = append(om.Files, cas.FileEntry{Path: p, Digest: e.putBlob([]byte(f.content)), Size: int64(len(f.content)), Exec: f.exec})
	}
	od := e.putBlob(store.MustJSON(om))
	rm := e.putBlob(store.MustJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{output: od}}))
	return e.insertJob(r, workflow.JobKindNormal, name, matrix, workflow.Spec{Platform: "linux/arm64", Executor: workflow.ExecutorOCI}, store.JobSucceeded, &rm)
}

// putBlob 把内容写入 CAS，返回 digest。
func (e *pubEnv) putBlob(data []byte) string {
	e.t.Helper()
	d := cas.Digest(data)
	if _, err := e.cas.Put(e.t.Context(), d, bytes.NewReader(data)); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// publishJob 在 run 中插入一个 pending 的发布 job，返回 job ID。
func (e *pubEnv) publishJob(r *testRun, rel workflow.Release, opts ...func(*workflow.Spec)) int64 {
	e.t.Helper()
	spec := workflow.Spec{Platform: workflow.PlatformServer, Executor: workflow.ExecutorServer, Release: &rel}
	for _, f := range opts {
		f(&spec)
	}
	return e.insertJob(r, workflow.JobKindPublish, "publish", nil, spec, store.JobPending, nil)
}

func (e *pubEnv) insertJob(r *testRun, kind workflow.JobKind, name string, matrix map[string]any, spec workflow.Spec, state store.JobState, result *string) int64 {
	e.t.Helper()
	var m []byte
	display := name
	if matrix != nil {
		m = store.MustJSON(matrix)
		display = fmt.Sprintf("%s %v", name, matrix)
	}
	var id int64
	err := e.db.QueryRow(e.t.Context(), `INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state, result_manifest, queued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now()) RETURNING id`,
		kind, r.id, r.rerun, fmt.Sprintf("%s-%d", name, e.next()), name, display, m, store.MustJSON(spec), spec.Platform, spec.Executor, state, result).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// rerun 模拟重跑整个 run（PLAN #6.6）：创建下一次 rerun，成功的上游 job 沿用结果，发布 job 重新排队。
// 返回新的发布 job ID（新的 job 行，releases 中还没有它的进度）。
func (e *pubEnv) rerun(r *testRun) int64 {
	e.t.Helper()
	prev := r.rerun
	r.rerun++
	e.exec(`UPDATE runs SET current_rerun=$2, state='active', finished_at=NULL WHERE id=$1`, r.id, r.rerun)
	e.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, $2, 'active')`, r.id, r.rerun)
	e.exec(`INSERT INTO jobs (kind, run_id, rerun, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state, result_manifest, queued_at)
		SELECT kind, run_id, $2, job_key, job_name, display_name, matrix, spec, platform, executor_kind,
			CASE WHEN kind='publish' THEN 'pending' ELSE state END, CASE WHEN kind='publish' THEN NULL ELSE result_manifest END, now()
		FROM jobs WHERE run_id=$1 AND rerun=$3`, r.id, r.rerun, prev)
	var id int64
	if err := e.db.QueryRow(e.t.Context(), `SELECT id FROM jobs WHERE run_id=$1 AND rerun=$2 AND kind='publish'`, r.id, r.rerun).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// publish 执行一次 publishOnce，期望它处理了一个发布 job。
func (e *pubEnv) publish() {
	e.t.Helper()
	if !e.w.publishOnce(e.t.Context()) {
		e.t.Fatal("publishOnce 应处理一个发布 job")
	}
}

// ---------- 检查 ----------

// jobResult 是发布 job 的状态和原因。
func (e *pubEnv) jobResult(id int64) (store.JobState, string) {
	e.t.Helper()
	var state store.JobState
	var reason string
	if err := e.db.QueryRow(e.t.Context(), `SELECT state, coalesce(reason, '') FROM jobs WHERE id=$1`, id).Scan(&state, &reason); err != nil {
		e.t.Fatal(err)
	}
	return state, reason
}

// wantJob 检查发布 job 的状态，want 为失败时原因应包含 reason。
func (e *pubEnv) wantJob(id int64, want store.JobState, reason string) {
	e.t.Helper()
	state, got := e.jobResult(id)
	if state != want || !strings.Contains(got, reason) {
		e.t.Fatalf("发布 job 状态 %s（%s），期望 %s 且原因包含 %q\n日志：\n%s", state, got, want, reason, e.log(id))
	}
}

// releaseRow 是 releases 表中的发布进度。
type releaseRow struct {
	State, Stage, Error, Target string
	ReleaseID                   *int64
	Progress                    map[string]any
}

func (e *pubEnv) releaseRow(jobID int64) releaseRow {
	e.t.Helper()
	var r releaseRow
	var progress []byte
	if err := e.db.QueryRow(e.t.Context(), `SELECT state, stage, coalesce(error, ''), target_repo, github_release_id, progress FROM releases WHERE job_id=$1`, jobID).
		Scan(&r.State, &r.Stage, &r.Error, &r.Target, &r.ReleaseID, &progress); err != nil {
		e.t.Fatalf("读取 job %d 的发布进度：%v", jobID, err)
	}
	if err := json.Unmarshal(progress, &r.Progress); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// runState 返回 run 的状态。
func (e *pubEnv) runState(r *testRun) string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRow(e.t.Context(), `SELECT state FROM runs WHERE id=$1`, r.id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// log 返回发布 job 的日志。
func (e *pubEnv) log(jobID int64) string {
	e.t.Helper()
	b, _, err := e.logs.Read(logstore.PublishLogID(jobID), 0, 1<<20)
	if err != nil {
		return "（读取日志失败：" + err.Error() + "）"
	}
	return string(b)
}

// ---------- 注入故障 ----------

// faultTransport 在请求发往模拟 GitHub 之前调用 hook；hook 返回非 nil 的响应或错误时直接返回它，不再发送。
type faultTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	hook func(*http.Request) (*http.Response, error)
}

func (f *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if resp, err := hook(req); resp != nil || err != nil {
			return resp, err
		}
	}
	return f.next.RoundTrip(req)
}

func (f *faultTransport) set(hook func(*http.Request) (*http.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// failOnce 让第一个满足 match 的请求得到 500。
func (f *faultTransport) failOnce(match func(*http.Request) bool) {
	var done atomic.Bool
	f.set(func(req *http.Request) (*http.Response, error) {
		if !match(req) || !done.CompareAndSwap(false, true) {
			return nil, nil
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError, Request: req,
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   io.NopCloser(strings.NewReader(`{"message":"injected failure"}`)),
		}, nil
	})
}

// isUpload 匹配上传名为 name 的 asset 的请求。
func isUpload(name string) func(*http.Request) bool {
	return func(req *http.Request) bool {
		return req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/assets") && req.URL.Query().Get("name") == name
	}
}

// waitFor 等待 cond 成立：每次 notify.Hub 唤醒后检查一次，最多等 d。
func waitFor(t *testing.T, hub *notify.Hub, d time.Duration, what string, cond func() bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		ch := hub.JobsChanged()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-timeout:
			t.Fatalf("等待%s超时", what)
		}
	}
}

// ctxCanceledOn 返回一个 context：第一个满足 match 的请求发出时取消它（模拟发布过程中 Server 退出）。
func (e *pubEnv) ctxCanceledOn(match func(*http.Request) bool) context.Context {
	ctx, cancel := context.WithCancel(e.t.Context())
	e.t.Cleanup(cancel)
	e.faults.set(func(req *http.Request) (*http.Response, error) {
		if match(req) {
			cancel()
			return nil, context.Canceled
		}
		return nil, nil
	})
	return ctx
}

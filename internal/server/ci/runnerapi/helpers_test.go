package runnerapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1/runnerv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/scheduler"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// testLeaseTTL 是测试中 Runner 租约的有效期。
const testLeaseTTL = 2 * time.Minute

// harness 是一个连着独立数据库的 RunnerService，经 httptest 用 Connect 协议访问（与 Runner 相同的路径，
// 包括 ErrorInterceptor）。
type harness struct {
	t      *testing.T
	db     *store.DB
	svc    *Service
	eng    *engine.Engine
	sched  *scheduler.Reserver
	keys   *auth.Keys
	cas    *cas.FSStore
	hub    *notify.Hub
	client runnerv1connect.RunnerServiceClient
	// logs 是 Server 日志（ErrorInterceptor 记录内部错误）。
	logs *syncBuffer
	// logDir 是 attempt 日志的目录（logstore 把日志保存为 <logDir>/<attempt>.log）。
	logDir string
	// master 是解密 secret 用的 master key。
	master []byte
	// seq 用于生成唯一的名称（仓库 ID、run 的 dedupe_key、Runner 名称）。
	seq atomic.Int64
}

// harnessOpts 调整 harness 的配置。
type harnessOpts struct {
	// longPoll 是 Acquire 长轮询的最长等待，默认 50ms（没有 job 时的 Acquire 很快返回）。
	longPoll time.Duration
	// github 是 GitHub App 客户端；nil 表示没有配置 GitHub App。
	github *gh.Client
	// managed 是托管池的来源（调度核心的测试替身）；nil 表示没有托管池。
	managed scheduler.ManagedSource
	// gitcred 是签发登记的配置（配置了 GitHub App 时）。
	gitcred gitcred.Config
}

func newHarness(t *testing.T, opts ...func(*harnessOpts)) *harness {
	t.Helper()
	o := harnessOpts{longPoll: 50 * time.Millisecond}
	for _, f := range opts {
		f(&o)
	}
	db := store.New(pgtest.Pool(t))
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	casStore := &cas.FSStore{Root: t.TempDir()}
	logDir := t.TempDir()
	ls, err := logstore.New(logDir)
	if err != nil {
		t.Fatal(err)
	}
	hub := notify.NewHub()
	ob := outbox.NewWriter(true)
	// 与组装层相同：配置了 GitHub App 时有签发登记，attempt 结束的事务提交后使它失效
	var reg *gitcred.Registry
	if o.github != nil {
		reg = gitcred.New(o.gitcred, gitcred.Deps{GitHub: o.github, Log: log})
	}
	ended := func(ctx context.Context, attemptID int64) {
		if reg != nil {
			reg.Invalidate(ctx, GitCredAttempt(attemptID))
		}
	}
	reconcile := func(ctx context.Context, running func(ctx context.Context, ids []int64) ([]int64, error)) error {
		if reg == nil {
			return nil
		}
		_, err := reg.Reconcile(ctx, gitcred.DomainCI, running)
		return err
	}
	eng := engine.New(engine.Config{InfraRetryMax: 2, LeaseTTL: testLeaseTTL},
		engine.Deps{DB: db, CAS: casStore, Outbox: ob, Hub: hub, Log: log, AttemptEnded: ended, ReconcileGitCred: reconcile})
	master := bytes.Repeat([]byte{7}, secretbox.KeySize)
	keys := auth.NewKeys([]byte("token-key"), []byte("cas-key"), "admin")
	ra := runnerauth.New(db, keys, map[string]runnerauth.EnrollHook{store.RunnerKindCI: EnrollHook})
	sched := scheduler.New(scheduler.Config{LeaseTTL: testLeaseTTL}, scheduler.Deps{DB: db, Engine: eng, Outbox: ob, Managed: o.managed, Log: log})
	svc := New(Config{LeaseTTL: testLeaseTTL, AcquireLongPoll: o.longPoll, MasterKey: master}, Deps{
		DB: db, GitHub: o.github, GitCred: reg, CAS: casStore, Keys: keys, Logs: ls, Engine: eng, Scheduler: sched, Hub: hub, Log: log, Auth: ra,
	})
	mux := http.NewServeMux()
	mux.Handle(runnerv1connect.NewRunnerServiceHandler(svc, connect.WithInterceptors(ErrorInterceptor(log))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{
		t: t, db: db, svc: svc, eng: eng, sched: sched, keys: keys, cas: casStore, hub: hub, logs: logs, logDir: logDir, master: master,
		client: runnerv1connect.NewRunnerServiceClient(srv.Client(), srv.URL),
	}
}

// syncBuffer 是可以并发写入的 bytes.Buffer（slog 在请求的 goroutine 中写日志）。
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

// next 返回一个本 harness 内唯一的序号。
func (h *harness) next() int64 { return h.seq.Add(1) }

// exec 执行 SQL，失败时终止测试。
func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.t.Context(), sql, args...); err != nil {
		h.t.Fatalf("%s：%v", sql, err)
	}
}

// ---------- Runner ----------

// testRunner 是一个已注册、已认证的 Runner。
type testRunner struct {
	h     *harness
	id    int64
	name  string
	pool  string
	priv  ed25519.PrivateKey
	token string
	// role 是注册时 Server 返回的角色（AI 执行节点）。
	role string
	// state 是 Acquire 时上报的状态。
	state *runnerv1.RunnerState
}

// runnerOpts 描述要注册的 Runner。
type runnerOpts struct {
	pool      string
	kind      string
	role      string
	platforms []string
	executors []string
}

// enrollRunner 用新的 enroll token 注册一个 Runner 并认证，得到 access token。
// 默认是 default pool 中的 linux/arm64 Runner，executor 为 oci。
func (h *harness) enrollRunner(opts ...func(*runnerOpts)) *testRunner {
	h.t.Helper()
	o := runnerOpts{platforms: []string{"linux/arm64"}, executors: []string{"oci"}}
	for _, f := range opts {
		f(&o)
	}
	tok, _, err := auth.CreateEnrollToken(h.t.Context(), h.db, o.pool, o.kind, o.role, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	r := &testRunner{
		h: h, name: fmt.Sprintf("runner-%d", h.next()), priv: priv,
		state: &runnerv1.RunnerState{Version: "test", Platforms: o.platforms, Executors: o.executors},
	}
	resp, err := h.client.Enroll(h.t.Context(), connect.NewRequest(&runnerv1.EnrollRequest{
		Token: tok, Name: r.name, PublicKey: pub, State: r.state,
	}))
	if err != nil {
		h.t.Fatalf("Enroll：%v", err)
	}
	r.id, r.pool, r.role = resp.Msg.RunnerId, resp.Msg.Pool, resp.Msg.Role
	ar, err := h.client.Authenticate(h.t.Context(), connect.NewRequest(r.authRequest(time.Now(), randNonce())))
	if err != nil {
		h.t.Fatalf("Authenticate：%v", err)
	}
	r.token = ar.Msg.AccessToken
	return r
}

func withPool(p string) func(*runnerOpts) { return func(o *runnerOpts) { o.pool = p } }

func withPlatforms(executors []string, platforms ...string) func(*runnerOpts) {
	return func(o *runnerOpts) { o.executors, o.platforms = executors, platforms }
}

// authRequest 返回用 Runner 私钥签名的认证请求（格式与 internal/runner/client.go 一致）。
func (r *testRunner) authRequest(ts time.Time, nonce string) *runnerv1.AuthenticateRequest {
	msg := fmt.Sprintf("kiln-runner-auth:%d:%d:%s", r.id, ts.Unix(), nonce)
	return &runnerv1.AuthenticateRequest{RunnerId: r.id, Timestamp: ts.Unix(), Nonce: nonce, Signature: ed25519.Sign(r.priv, []byte(msg))}
}

func randNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// withAuth 给请求加上 Runner 的 access token。
func withAuth[T any](tok string, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+tok)
	return req
}

// acquire 领取 job（不等待：每次用新的 request_id，max_jobs 默认 1）。
func (r *testRunner) acquire(maxJobs int32) (*runnerv1.AcquireResponse, error) {
	resp, err := r.h.client.Acquire(r.h.t.Context(), withAuth(r.token, &runnerv1.AcquireRequest{
		State: r.state, MaxJobs: maxJobs, RequestId: fmt.Sprintf("req-%d", r.h.next()),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// mustAcquire 领取 job，期望得到 want 个。
func (r *testRunner) mustAcquire(maxJobs int32, want int) []*runnerv1.JobSpec {
	r.h.t.Helper()
	resp, err := r.acquire(maxJobs)
	if err != nil {
		r.h.t.Fatalf("Acquire：%v", err)
	}
	if len(resp.Jobs) != want {
		r.h.t.Fatalf("Acquire 领到 %d 个 job，期望 %d 个", len(resp.Jobs), want)
	}
	return resp.Jobs
}

func (r *testRunner) heartbeat(ref *runnerv1.AttemptRef, step int32) (*runnerv1.HeartbeatResponse, error) {
	resp, err := r.h.client.Heartbeat(r.h.t.Context(), withAuth(r.token, &runnerv1.HeartbeatRequest{Ref: ref, CurrentStep: step}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// progress 返回 attempt 保存的进度（没有时为 nil）。
func (h *harness) progress(attempt int64) *store.AttemptProgress {
	h.t.Helper()
	m, err := cistore.AttemptProgresses(h.t.Context(), h.db, []int64{attempt})
	if err != nil {
		h.t.Fatal(err)
	}
	return m[attempt]
}

func (r *testRunner) complete(req *runnerv1.CompleteRequest) error {
	_, err := r.h.client.Complete(r.h.t.Context(), withAuth(r.token, req))
	return err
}

// ---------- 仓库、run、job ----------

// repoOpts 描述测试仓库。
type repoOpts struct {
	name           string // 默认 repo<id>
	installation   int64  // 默认 777
	maxConcurrency int
}

// addRepo 登记一个仓库，返回仓库 ID。
func (h *harness) addRepo(opts ...func(*repoOpts)) int64 {
	h.t.Helper()
	id := 1000 + h.next()
	o := repoOpts{name: fmt.Sprintf("repo%d", id), installation: 777, maxConcurrency: 8}
	for _, f := range opts {
		f(&o)
	}
	h.exec(`INSERT INTO repos (id, installation_id, owner_id, owner, name, clone_url, max_concurrency) VALUES ($1, $2, 9001, 'tester', $3, $4, $5)`,
		id, o.installation, o.name, "https://github.com/tester/"+o.name+".git", o.maxConcurrency)
	return id
}

// addGitHubRepo 登记一个仓库（addRepo），同时在 fakegithub 中登记同一个仓库（ID、名称、installation 相同）：签发 token 时
// 替身按真实的规则校验仓库属于这个 installation。
func (h *harness) addGitHubRepo(fake *fakegithub.Server, opts ...func(*repoOpts)) int64 {
	h.t.Helper()
	id := h.addRepo(opts...)
	r, err := store.GetRepo(h.t.Context(), h.db, id)
	if err != nil {
		h.t.Fatal(err)
	}
	fake.AddRepo(&fakegithub.Repo{ID: r.ID, Owner: r.Owner, Name: r.Name, Private: true, Installation: r.InstallationID})
	return id
}

// addRun 在仓库中创建一个 active 的 run（rerun 0），返回 run ID。
func (h *harness) addRun(repoID int64) int64 {
	h.t.Helper()
	var id int64
	err := h.db.QueryRow(h.t.Context(), `INSERT INTO runs (repo_id, workflow, event, ref, sha, source, dedupe_key, state)
		VALUES ($1, 'ci.yml', 'push', 'refs/heads/main', 'abc123', 'webhook', $2, 'active') RETURNING id`, repoID, fmt.Sprintf("run-%d", h.next())).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO run_reruns (run_id, rerun, state) VALUES ($1, 0, 'active')`, id)
	return id
}

// jobRow 描述要插入的 job。
type jobRow struct {
	name     string
	matrix   map[string]any
	spec     workflow.Spec
	state    store.JobState
	priority int
}

// addJob 在 run 中插入一个 job（默认 pending、linux/arm64、oci），返回 job ID。
func (h *harness) addJob(runID int64, opts ...func(*jobRow)) int64 {
	h.t.Helper()
	n := h.next()
	j := jobRow{
		name: fmt.Sprintf("job%d", n), state: store.JobPending, priority: 1,
		spec: workflow.Spec{
			Platform: "linux/arm64", Executor: workflow.ExecutorOCI, Container: "alpine",
			Steps: []workflow.StepSpec{{Name: "build", Run: "make", Shell: "sh"}}, TimeoutMinutes: 10,
		},
	}
	for _, f := range opts {
		f(&j)
	}
	var matrix []byte
	if j.matrix != nil {
		matrix = store.MustJSON(j.matrix)
	}
	key := fmt.Sprintf("%s-%d", j.name, n)
	var id int64
	err := h.db.QueryRow(h.t.Context(), `INSERT INTO jobs (kind, run_id, job_key, job_name, display_name, matrix, spec, platform, executor_kind, state, priority, queued_at)
		VALUES ('normal', $1, $2, $3, $2, $4, $5, $6, $7, $8, $9, now()) RETURNING id`,
		runID, key, j.name, matrix, store.MustJSON(j.spec), j.spec.Platform, j.spec.Executor, j.state, j.priority).Scan(&id)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// pendingJob 是"一个仓库、一个 run、一个 pending job"的快捷方式，返回 job ID。
func (h *harness) pendingJob(opts ...func(*jobRow)) int64 {
	h.t.Helper()
	return h.addJob(h.addRun(h.addRepo()), opts...)
}

func onPlatform(executor workflow.ExecutorKind, platform string) func(*jobRow) {
	return func(j *jobRow) { j.spec.Executor, j.spec.Platform = executor, platform }
}

// jobState 是测试关心的 job 字段。
type jobState struct {
	State        store.JobState
	Epoch        int64
	AttemptCount int
	Attempt      *int64
	Reason       string
	SkipReason   string
	Lease        *time.Time
	Result       *string
	Cancel       bool
}

func (h *harness) job(id int64) jobState {
	h.t.Helper()
	var s jobState
	err := h.db.QueryRow(h.t.Context(), `SELECT state, epoch, attempt_count, current_attempt_id, coalesce(reason, ''), coalesce(last_skip_reason, ''),
		lease_expires_at, result_manifest, cancel_requested FROM jobs WHERE id=$1`, id).
		Scan(&s.State, &s.Epoch, &s.AttemptCount, &s.Attempt, &s.Reason, &s.SkipReason, &s.Lease, &s.Result, &s.Cancel)
	if err != nil {
		h.t.Fatalf("读取 job %d：%v", id, err)
	}
	return s
}

// wantSkip 检查 job 仍为 pending，且记录的跳过原因包含 reason（reason 为空时只检查 pending）。
func (h *harness) wantSkip(id int64, reason string) {
	h.t.Helper()
	s := h.job(id)
	if s.State != store.JobPending {
		h.t.Fatalf("job %d 不应被领取，状态 %s", id, s.State)
	}
	if !strings.Contains(s.SkipReason, reason) {
		h.t.Fatalf("job %d 的跳过原因 %q，期望包含 %q", id, s.SkipReason, reason)
	}
}

// ---------- CAS ----------

// putBlob 把内容写入 CAS，返回 digest。
func (h *harness) putBlob(data []byte) string {
	h.t.Helper()
	d := cas.Digest(data)
	if _, err := h.cas.Put(h.t.Context(), d, bytes.NewReader(data)); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// putResult 写入一个结果 manifest：一个名为 output 的输出，包含 files（路径 → 内容）。
// uploaded 为 false 时文件内容不写入 CAS（模拟 Runner 没有上传完）。返回结果 manifest 的 digest。
func (h *harness) putResult(output string, files map[string]string, uploaded bool) string {
	h.t.Helper()
	om := cas.OutputManifest{Schema: cas.OutputSchema}
	for p, c := range files {
		d := cas.Digest([]byte(c))
		if uploaded {
			h.putBlob([]byte(c))
		}
		om.Files = append(om.Files, cas.FileEntry{Path: p, Digest: d, Size: int64(len(c))})
	}
	od := h.putBlob(store.MustJSON(om))
	return h.putBlob(store.MustJSON(cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{output: od}}))
}

// setSecret 在作用域中保存一个 secret（与 admin 相同的信封加密）。
func (h *harness) setSecret(kind store.ScopeKind, scopeID int64, name, value string) {
	h.t.Helper()
	sealed, err := secretbox.Seal(h.master, secretbox.AAD(string(kind), scopeID, name), []byte(value))
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO secrets (scope_kind, scope_id, name, ciphertext, wrapped_dek) VALUES ($1,$2,$3,$4,$5)`,
		kind, scopeID, name, sealed.Ciphertext, sealed.WrappedDEK)
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

// waitCtx 返回一个有上限的 context，避免长轮询的用例在出错时挂住。
func waitCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

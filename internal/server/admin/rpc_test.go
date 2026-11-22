package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AdminService 的 RPC 适配层：admin token 拦截器、请求和响应的转换、错误码映射（CLI 据此显示错误）。

const testAdminToken = "admin-token-0123456789abcdef"

// bearer 给每个请求加上 Authorization 头。
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b != "" {
			req.Header().Set("Authorization", "Bearer "+string(b))
		}
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(n connect.StreamingClientFunc) connect.StreamingClientFunc {
	return n
}

func (b bearer) WrapStreamingHandler(n connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return n
}

// rpcClient 启动 AdminService 的 HTTP 服务，返回用 token 认证的客户端（token 为空时不带认证头）。
func (e *testEnv) rpcClient(token string) apiv1connect.AdminServiceClient {
	e.t.Helper()
	keys := auth.NewKeys([]byte("token-key"), []byte("cas-key"), testAdminToken)
	route, h := NewHandler(e.svc, keys)
	mux := http.NewServeMux()
	mux.Handle(route, h)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return apiv1connect.NewAdminServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer(token)))
}

func req[T any](m *T) *connect.Request[T] { return connect.NewRequest(m) }

func TestRPC_AuthInterceptor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, c := range []struct {
		name  string
		token string
		code  connect.Code
	}{
		{"没有 token", "", connect.CodeUnauthenticated},
		{"错误的 token", "wrong-token", connect.CodeUnauthenticated},
		{"正确的 token", testAdminToken, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := e.rpcClient(c.token)
			_, err := cl.ListRepos(e.ctx, req(&apiv1.ListReposRequest{}))
			if c.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantCode(t, c.name, err, c.code)
			// 写操作同样被拦截，什么也不执行
			_, err = cl.SetVariable(e.ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "X", Value: "1"}))
			wantCode(t, c.name+"（写操作）", err, c.code)
		})
	}
	if n := e.count(`SELECT count(*) FROM variables`); n != 0 {
		t.Errorf("没有认证的写操作不应执行")
	}
}

// TestRPC_ErrorCodes：各 RPC 的错误码映射。
func TestRPC_ErrorCodes(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx
	active := e.dispatch("")
	// 没有结束的 run 中的 job
	build := e.jobID(active, 0, "build")

	for _, c := range []struct {
		name string
		call func() error
		code connect.Code
	}{
		{"GetRun 不存在", func() error { _, err := cl.GetRun(ctx, req(&apiv1.GetRunRequest{Id: 9999})); return err }, connect.CodeNotFound},
		{"CancelRun 不存在", func() error { _, err := cl.CancelRun(ctx, req(&apiv1.CancelRunRequest{Id: 9999})); return err }, connect.CodeNotFound},
		{"RerunRun 不存在", func() error { _, err := cl.RerunRun(ctx, req(&apiv1.RerunRunRequest{Id: 9999})); return err }, connect.CodeNotFound},
		{"RerunRun 没有结束", func() error { _, err := cl.RerunRun(ctx, req(&apiv1.RerunRunRequest{Id: active})); return err }, connect.CodeFailedPrecondition},
		{"RerunJob 不存在", func() error { _, err := cl.RerunJob(ctx, req(&apiv1.RerunJobRequest{JobId: 9999})); return err }, connect.CodeNotFound},
		{"RerunJob 没有结束", func() error { _, err := cl.RerunJob(ctx, req(&apiv1.RerunJobRequest{JobId: build})); return err }, connect.CodeFailedPrecondition},
		{"Dispatch workflow 不存在", func() error {
			_, err := cl.Dispatch(ctx, req(&apiv1.DispatchRequest{Repo: testRepo, Workflow: "nope"}))
			return err
		}, connect.CodeFailedPrecondition},
		{"Dispatch 仓库不存在", func() error {
			_, err := cl.Dispatch(ctx, req(&apiv1.DispatchRequest{Repo: "tester/nope", Workflow: "build"}))
			return err
		}, connect.CodeFailedPrecondition},
		{"GetJobLog 不存在", func() error { _, err := cl.GetJobLog(ctx, req(&apiv1.GetJobLogRequest{JobId: 9999})); return err }, connect.CodeNotFound},
		{"GetJobLog 尝试不属于 job", func() error {
			_, err := cl.GetJobLog(ctx, req(&apiv1.GetJobLogRequest{JobId: build, AttemptId: 9999}))
			return err
		}, connect.CodeNotFound},
		{"ListRuns 仓库不存在", func() error { _, err := cl.ListRuns(ctx, req(&apiv1.ListRunsRequest{Repo: "tester/nope"})); return err }, connect.CodeInvalidArgument},
		{"RevokeRunner 不存在", func() error {
			_, err := cl.RevokeRunner(ctx, req(&apiv1.RevokeRunnerRequest{Name: "ghost"}))
			return err
		}, connect.CodeNotFound},
		{"DrainRunner 不存在", func() error {
			_, err := cl.DrainRunner(ctx, req(&apiv1.DrainRunnerRequest{Name: "ghost", Drain: true}))
			return err
		}, connect.CodeNotFound},
		{"RetryOutbox 不存在", func() error { _, err := cl.RetryOutbox(ctx, req(&apiv1.RetryOutboxRequest{Id: 9999})); return err }, connect.CodeNotFound},
		{"SetVariable 名称无效", func() error {
			_, err := cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: repoScope, Name: "A-B", Value: "1"}))
			return err
		}, connect.CodeInvalidArgument},
		{"SetVariable 作用域无效", func() error {
			_, err := cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: &apiv1.Scope{Repo: "tester/nope"}, Name: "A", Value: "1"}))
			return err
		}, connect.CodeInvalidArgument},
		{"SetSecret 没有作用域", func() error {
			_, err := cl.SetSecret(ctx, req(&apiv1.SetSecretRequest{Name: "A", Value: "1"}))
			return err
		}, connect.CodeInvalidArgument},
		{"DeleteSecret 作用域无效", func() error {
			_, err := cl.DeleteSecret(ctx, req(&apiv1.DeleteSecretRequest{Scope: &apiv1.Scope{Account: "nobody"}, Name: "A"}))
			return err
		}, connect.CodeInvalidArgument},
		{"DeleteVariable environment 不存在", func() error {
			_, err := cl.DeleteVariable(ctx, req(&apiv1.DeleteVariableRequest{Scope: envScope(testRepo, "nope"), Name: "A"}))
			return err
		}, connect.CodeInvalidArgument},
		{"ListVariables 作用域无效", func() error {
			_, err := cl.ListVariables(ctx, req(&apiv1.ListVariablesRequest{Scope: &apiv1.Scope{Repo: "x"}}))
			return err
		}, connect.CodeInvalidArgument},
		{"ListSecrets 作用域无效", func() error {
			_, err := cl.ListSecrets(ctx, req(&apiv1.ListSecretsRequest{Scope: &apiv1.Scope{Repo: "x"}}))
			return err
		}, connect.CodeInvalidArgument},
		{"UpsertEnvironment 没有名称", func() error {
			_, err := cl.UpsertEnvironment(ctx, req(&apiv1.UpsertEnvironmentRequest{Environment: &apiv1.Environment{Repo: testRepo}}))
			return err
		}, connect.CodeInvalidArgument},
		{"DeleteEnvironment 不是 environment", func() error {
			_, err := cl.DeleteEnvironment(ctx, req(&apiv1.DeleteEnvironmentRequest{Scope: repoScope}))
			return err
		}, connect.CodeInvalidArgument},
		{"ReconcileRepo 仓库不存在", func() error {
			_, err := cl.ReconcileRepo(ctx, req(&apiv1.ReconcileRepoRequest{Repo: "tester/nope"}))
			return err
		}, connect.CodeInvalidArgument},
		{"BumpCacheSalt 仓库不存在", func() error {
			_, err := cl.BumpCacheSalt(ctx, req(&apiv1.BumpCacheSaltRequest{Repo: "tester/nope"}))
			return err
		}, connect.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) { wantCode(t, c.name, c.call(), c.code) })
	}
}

// TestRPC_Settings：变量、secret、environment 的 RPC，操作者记为 admin。
func TestRPC_Settings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx

	_, err := cl.UpsertEnvironment(ctx, req(&apiv1.UpsertEnvironmentRequest{Environment: &apiv1.Environment{
		Name: "release", Repo: testRepo, RefPolicy: []string{"refs/tags/v*"}, AllowedPublishRepos: []string{"tester/pub"},
	}}))
	e.must(err)
	_, err = cl.SetVariable(ctx, req(&apiv1.SetVariableRequest{Scope: envScope(testRepo, "release"), Name: "CHANNEL", Value: "stable"}))
	e.must(err)
	_, err = cl.SetSecret(ctx, req(&apiv1.SetSecretRequest{Scope: envScope(testRepo, "release"), Name: "TOKEN", Value: "s3cr3t"}))
	e.must(err)

	envs, err := cl.ListEnvironments(ctx, req(&apiv1.ListEnvironmentsRequest{Account: testAccount}))
	e.must(err)
	if len(envs.Msg.Environments) != 1 || envs.Msg.Environments[0].Repo != testRepo || envs.Msg.Environments[0].AllowedPublishRepos[0] != "tester/pub" {
		t.Errorf("ListEnvironments = %v", envs.Msg.Environments)
	}
	vars, err := cl.ListVariables(ctx, req(&apiv1.ListVariablesRequest{Scope: envScope(testRepo, "release")}))
	e.must(err)
	if len(vars.Msg.Variables) != 1 || vars.Msg.Variables[0].Value != "stable" {
		t.Errorf("ListVariables = %v", vars.Msg.Variables)
	}
	secrets, err := cl.ListSecrets(ctx, req(&apiv1.ListSecretsRequest{Scope: envScope(testRepo, "release")}))
	e.must(err)
	if len(secrets.Msg.Secrets) != 1 || secrets.Msg.Secrets[0].Name != "TOKEN" || secrets.Msg.Secrets[0].UpdatedAt == 0 {
		t.Errorf("ListSecrets = %v", secrets.Msg.Secrets)
	}
	if strings.Contains(secrets.Msg.String(), "s3cr3t") {
		t.Errorf("ListSecrets 返回了 secret 的值")
	}
	for _, a := range e.audits("") {
		if a.Actor != TokenActor {
			t.Errorf("通过 admin token 的操作应记为 %q：%+v", TokenActor, a)
		}
	}

	_, err = cl.DeleteVariable(ctx, req(&apiv1.DeleteVariableRequest{Scope: envScope(testRepo, "release"), Name: "CHANNEL"}))
	e.must(err)
	_, err = cl.DeleteSecret(ctx, req(&apiv1.DeleteSecretRequest{Scope: envScope(testRepo, "release"), Name: "TOKEN"}))
	e.must(err)
	_, err = cl.DeleteEnvironment(ctx, req(&apiv1.DeleteEnvironmentRequest{Scope: envScope(testRepo, "release")}))
	e.must(err)
	if n := e.count(`SELECT count(*) FROM environments`) + e.count(`SELECT count(*) FROM variables`) + e.count(`SELECT count(*) FROM secrets`); n != 0 {
		t.Errorf("删除后还剩 %d 行", n)
	}
}

// TestRPC_ListRunsLimit：limit 不大于 0 时取默认值，超过上限时按上限截断（而不是退回默认值）。
func TestRPC_ListRunsLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	for range defaultRunListLimit + 1 {
		e.addRun(runOpts{})
	}
	for _, c := range []struct {
		limit int32
		want  int
	}{{0, defaultRunListLimit}, {maxRunListLimit + 1, defaultRunListLimit + 1}} {
		runs, err := cl.ListRuns(e.ctx, req(&apiv1.ListRunsRequest{Limit: c.limit}))
		e.must(err)
		if len(runs.Msg.Runs) != c.want {
			t.Errorf("limit %d：返回 %d 个 run，应为 %d 个", c.limit, len(runs.Msg.Runs), c.want)
		}
	}
}

// TestRPC_RunsAndRunners：run、job、日志、Runner、注册令牌、outbox、仓库和 cache salt 的 RPC。
func TestRPC_RunsAndRunners(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx
	now := time.Now()
	mini := e.addRunner("mini", runnerOpts{lastSeen: &now})
	run := e.addRun(runOpts{state: store.RunActive})
	job := e.addJob(jobOpts{runID: run, state: store.JobRunning})
	attempt := e.addAttempt(job, mini, "")
	e.appendLog(attempt, "line\n")

	// GetRun：job 带上当前尝试所在的 Runner
	got, err := cl.GetRun(ctx, req(&apiv1.GetRunRequest{Id: run}))
	e.must(err)
	if got.Msg.Run.Repo != testRepo || got.Msg.Run.State != "active" || len(got.Msg.Jobs) != 1 || got.Msg.Jobs[0].Runner != "mini" || got.Msg.Jobs[0].Attempts != 1 {
		t.Errorf("GetRun = %v", got.Msg)
	}
	// ListRuns：limit 不合法时取默认值
	runs, err := cl.ListRuns(ctx, req(&apiv1.ListRunsRequest{Repo: testRepo, Limit: -1}))
	e.must(err)
	if len(runs.Msg.Runs) != 1 || runs.Msg.Runs[0].Id != run {
		t.Errorf("ListRuns = %v", runs.Msg.Runs)
	}
	// GetJobLog
	l, err := cl.GetJobLog(ctx, req(&apiv1.GetJobLogRequest{JobId: job, Offset: 0}))
	e.must(err)
	if string(l.Msg.Data) != "line\n" || l.Msg.NextOffset != 5 || l.Msg.Finished {
		t.Errorf("GetJobLog = %v", l.Msg)
	}
	// ListRunners、DrainRunner、RevokeRunner
	runners, err := cl.ListRunners(ctx, req(&apiv1.ListRunnersRequest{}))
	e.must(err)
	if len(runners.Msg.Runners) != 1 || runners.Msg.Runners[0].RunningJobs != 1 || runners.Msg.Runners[0].State != store.RunnerActive {
		t.Errorf("ListRunners = %v", runners.Msg.Runners)
	}
	_, err = cl.DrainRunner(ctx, req(&apiv1.DrainRunnerRequest{Name: "mini", Drain: true}))
	e.must(err)
	e.wantAudit("runner.drain", TokenActor, map[string]any{"name": "mini"})
	_, err = cl.RevokeRunner(ctx, req(&apiv1.RevokeRunnerRequest{Name: "mini"}))
	e.must(err)
	e.wantAudit("runner.revoke", TokenActor, map[string]any{"name": "mini"})
	// CreateEnrollToken：有效期按分钟
	tok, err := cl.CreateEnrollToken(ctx, req(&apiv1.CreateEnrollTokenRequest{Pool: "mac", TtlMinutes: 5}))
	e.must(err)
	if d := time.Until(time.Unix(tok.Msg.ExpiresAt, 0)); d < 4*time.Minute || d > 6*time.Minute || !strings.HasPrefix(tok.Msg.Token, "kiln-enroll-") {
		t.Errorf("CreateEnrollToken = %v（%s 后过期）", tok.Msg, d)
	}
	// CancelRun
	_, err = cl.CancelRun(ctx, req(&apiv1.CancelRunRequest{Id: run}))
	e.must(err)
	e.wantAudit("run.cancel", TokenActor, map[string]any{"run_id": run})

	// ListOutbox：默认列出 pending 和 dead；RetryOutbox
	e.addOutbox("a", store.OutboxPending, 1)
	dead := e.addOutbox("b", store.OutboxDead, 5)
	e.addOutbox("c", store.OutboxDone, 1)
	ob, err := cl.ListOutbox(ctx, req(&apiv1.ListOutboxRequest{}))
	e.must(err)
	if len(ob.Msg.Items) != 2 || ob.Msg.Items[0].Id != dead || ob.Msg.Items[0].UpdatedAt == 0 {
		t.Errorf("ListOutbox = %v", ob.Msg.Items)
	}
	ob, err = cl.ListOutbox(ctx, req(&apiv1.ListOutboxRequest{State: store.OutboxDone}))
	e.must(err)
	if len(ob.Msg.Items) != 1 || ob.Msg.Items[0].DedupeKey != "c" {
		t.Errorf("ListOutbox(done) = %v", ob.Msg.Items)
	}
	_, err = cl.RetryOutbox(ctx, req(&apiv1.RetryOutboxRequest{Id: dead}))
	e.must(err)
	e.wantAudit("outbox.retry", TokenActor, map[string]any{"id": dead})

	// ListRepos、BumpCacheSalt
	repos, err := cl.ListRepos(ctx, req(&apiv1.ListReposRequest{}))
	e.must(err)
	if len(repos.Msg.Repos) != 1 || repos.Msg.Repos[0].FullName != testRepo || repos.Msg.Repos[0].InstallationId != testInstallation ||
		!repos.Msg.Repos[0].Active || repos.Msg.Repos[0].DefaultBranch != "main" {
		t.Errorf("ListRepos = %v", repos.Msg.Repos)
	}
	salt := func() string {
		var s string
		e.must(e.db.QueryRow(ctx, `SELECT cache_salt FROM repos WHERE id=$1`, testRepoID).Scan(&s))
		return s
	}
	before := salt()
	_, err = cl.BumpCacheSalt(ctx, req(&apiv1.BumpCacheSaltRequest{Repo: testRepo}))
	e.must(err)
	if after := salt(); after == before || after == "" {
		t.Errorf("cache salt 应变化：%q → %q", before, after)
	}
	e.wantAudit("cache.bump_salt", TokenActor, map[string]any{"repo": testRepo})
}

// TestRPC_GitHubOperations：需要 GitHub 的 RPC（手动触发、重跑、刷新仓库、补对账）。
func TestRPC_GitHubOperations(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	cl := e.rpcClient(testAdminToken)
	ctx := e.ctx

	d, err := cl.Dispatch(ctx, req(&apiv1.DispatchRequest{Repo: testRepo, Workflow: "build", RequestId: "r1", Inputs: map[string]string{"channel": "beta"}}))
	e.must(err)
	run := e.run(d.Msg.RunId)
	if run.Source != "cli" || run.SenderLogin != TokenActor {
		t.Errorf("CLI 触发的 run：source=%q sender=%q", run.Source, run.SenderLogin)
	}
	again, err := cl.Dispatch(ctx, req(&apiv1.DispatchRequest{Repo: testRepo, Workflow: "build", RequestId: "r1"}))
	e.must(err)
	if again.Msg.RunId != d.Msg.RunId {
		t.Errorf("同一个请求 ID 应返回同一个 run")
	}

	_, err = cl.CancelRun(ctx, req(&apiv1.CancelRunRequest{Id: d.Msg.RunId}))
	e.must(err)
	rr, err := cl.RerunRun(ctx, req(&apiv1.RerunRunRequest{Id: d.Msg.RunId, Mode: string(store.RerunAll)}))
	e.must(err)
	if rr.Msg.Rerun != 1 {
		t.Errorf("RerunRun = %d", rr.Msg.Rerun)
	}
	_, err = cl.CancelRun(ctx, req(&apiv1.CancelRunRequest{Id: d.Msg.RunId}))
	e.must(err)
	rj, err := cl.RerunJob(ctx, req(&apiv1.RerunJobRequest{JobId: e.jobID(d.Msg.RunId, 1, "test")}))
	e.must(err)
	if rj.Msg.Rerun != 2 {
		t.Errorf("RerunJob = %d", rj.Msg.Rerun)
	}

	sync, err := cl.SyncInstallations(ctx, req(&apiv1.SyncInstallationsRequest{}))
	e.must(err)
	if sync.Msg.Installations != 1 || sync.Msg.Repos != 1 {
		t.Errorf("SyncInstallations = %v", sync.Msg)
	}
	e.wantAudit("repos.sync", TokenActor, nil)
	rec, err := cl.ReconcileRepo(ctx, req(&apiv1.ReconcileRepoRequest{Repo: testRepo}))
	e.must(err)
	if rec.Msg.Repo != testRepo || !rec.Msg.Baseline || rec.Msg.Branches != 1 {
		t.Errorf("ReconcileRepo = %v", rec.Msg)
	}
}

// 等待中的 job 带上调度器记录的等待原因；不在等待时不返回（领取后旧的原因已无意义）。
func TestRPC_JobWaitReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cl := e.rpcClient(testAdminToken)
	run := e.addRun(runOpts{state: store.RunActive})
	running := e.addJob(jobOpts{runID: run, key: "running", state: store.JobRunning})
	waiting := e.addJob(jobOpts{runID: run, key: "waiting", state: store.JobPending})
	const why = "等待资源释放（需要 1 CPU / 1.0 GiB）"
	e.exec(`UPDATE jobs SET last_skip_reason=$3 WHERE id IN ($1, $2)`, waiting, running, why)
	got, err := cl.GetRun(e.ctx, req(&apiv1.GetRunRequest{Id: run}))
	e.must(err)
	want := map[int64]string{waiting: why, running: ""}
	if len(got.Msg.Jobs) != 2 {
		t.Fatalf("GetRun 返回 %d 个 job", len(got.Msg.Jobs))
	}
	for _, j := range got.Msg.Jobs {
		if j.WaitReason != want[j.Id] {
			t.Errorf("job %d（%s）的 WaitReason = %q，期望 %q", j.Id, j.State, j.WaitReason, want[j.Id])
		}
	}
}

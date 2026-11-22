package runnerapi

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 测试仓库的 installation（addRepo 的默认值）。
const testInstallation = 777

// gitHarness 是配置了 GitHub App（fakegithub）的 harness：任务仓库 tester/app 中有一个声明了 repositories（tester/lib）的
// job 已被 runner 领取。
type gitHarness struct {
	*harness
	fake   *fakegithub.Server
	runner *testRunner
	ref    *runnerv1.AttemptRef
	jobID  int64
	app    int64
	lib    int64
}

func newGitHarness(t *testing.T, opts ...func(*harnessOpts)) *gitHarness {
	t.Helper()
	fake := fakegithub.New("tester")
	t.Cleanup(fake.Close)
	h := newHarness(t, append([]func(*harnessOpts){func(o *harnessOpts) {
		o.github = &gh.Client{AppID: fake.AppID, Key: testRSAKey(t), APIURL: fake.URL, UploadURL: fake.URL}
	}}, opts...)...)
	g := &gitHarness{harness: h, fake: fake, runner: h.enrollRunner()}
	// 都在 fakegithub 中登记：签发时替身按真实的规则校验仓库属于 installation，未声明的 other 由 Server 拒绝而不是替身
	g.app = h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "app" })
	g.lib = h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "lib" })
	h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "other" })
	g.jobID = h.addJob(h.addRun(g.app), func(j *jobRow) { j.spec.Repositories = []string{"tester/lib"} })
	g.ref = g.runner.mustAcquire(1, 1)[0].Ref
	return g
}

func (g *gitHarness) issue(op, repo string) (*runnerv1.IssueGitTokenResponse, error) {
	resp, err := g.client.IssueGitToken(g.t.Context(), withAuth(g.runner.token, &runnerv1.IssueGitTokenRequest{
		Ref: g.ref, OpId: op, Repository: repo,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (g *gitHarness) mustIssue(op, repo string) *runnerv1.IssueGitTokenResponse {
	g.t.Helper()
	resp, err := g.issue(op, repo)
	if err != nil {
		g.t.Fatalf("IssueGitToken(%s, %s)：%v", op, repo, err)
	}
	return resp
}

func (g *gitHarness) revoke(op, token string) error {
	_, err := g.client.RevokeGitToken(g.t.Context(), withAuth(g.runner.token, &runnerv1.RevokeGitTokenRequest{
		Ref: g.ref, OpId: op, Token: token,
	}))
	return err
}

// waitRevocations 等签发登记在后台的吊销（attempt 失效时发起）全部结束。
func (g *gitHarness) waitRevocations() {
	g.t.Helper()
	if err := g.svc.gitcred.Wait(g.t.Context()); err != nil {
		g.t.Fatalf("等待后台吊销：%v", err)
	}
}

// tokens 返回 fakegithub 签发过的 token。
func (g *gitHarness) tokens() []fakegithub.Token { return g.fake.IssuedTokens() }

// wantRevoked 检查 fakegithub 签发过的 token 都已吊销，返回它们的个数。
func (g *gitHarness) wantRevoked() int {
	g.t.Helper()
	toks := g.tokens()
	for _, tok := range toks {
		if !tok.Revoked {
			g.t.Fatalf("token %s（仓库 %v）没有吊销", tok.Token, tok.RepoIDs)
		}
	}
	return len(toks)
}

// pauseCreate 让下一次签发 token 的请求先执行 f（在 GitHub 签发之前、Server 等待响应时），模拟与签发在途交错的操作。
func (g *gitHarness) pauseCreate(f func()) {
	g.fake.SetFault(fakegithub.CreateTokenRoute(testInstallation), fakegithub.Fault{Pause: f})
}

// finishCommitted 在一个真实提交的事务中结束 attempt（与 Complete 的事务相同），但不调用 AttemptEnded：模拟结束已经提交、
// 失效处理还没执行的时刻。
func (g *gitHarness) finishCommitted(res engine.AttemptResult) {
	g.t.Helper()
	err := g.db.Tx(g.t.Context(), func(tx pgx.Tx) error {
		if _, err := lockAttemptRuns(g.t.Context(), tx, g.ref); err != nil {
			return err
		}
		j, err := g.svc.attemptJob(g.t.Context(), tx, g.runner.id, g.ref, true)
		if err != nil {
			return err
		}
		return g.eng.FinishAttempt(g.t.Context(), tx, j, res, nil)
	})
	if err != nil {
		g.t.Fatalf("结束 attempt：%v", err)
	}
}

// FetchSecrets 只下发 secret，不签发任何 token；校验与 FetchCredentials 相同（attempt 结束后、environment 策略不允许时拒绝）。
func TestService_FetchSecrets(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	h := g.harness
	var envID int64
	if err := h.db.QueryRow(t.Context(), `INSERT INTO environments (owner_id, repo_id, name, ref_policy)
		VALUES (9001, $1, 'prod', '["refs/heads/main"]') RETURNING id`, g.app).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	h.setSecret(store.ScopeRepo, g.app, "A", "repo-a")
	h.setSecret(store.ScopeEnvironment, envID, "A", "env-a")
	h.setSecret(store.ScopeRepo, g.app, "UNUSED", "x")
	jobID := h.addJob(h.addRun(g.app), func(j *jobRow) {
		j.spec.Environment, j.spec.Secrets, j.spec.Repositories = "prod", []string{"A"}, []string{"tester/lib"}
	})
	r := h.enrollRunner()
	ref := r.mustAcquire(1, 1)[0].Ref
	fetch := func() (*runnerv1.FetchSecretsResponse, error) {
		resp, err := h.client.FetchSecrets(t.Context(), withAuth(r.token, &runnerv1.FetchSecretsRequest{Ref: ref}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	resp, err := fetch()
	if err != nil || !maps.Equal(resp.Secrets, map[string]string{"A": "env-a"}) {
		t.Fatalf("FetchSecrets = %+v, %v，期望只有 environment 级的 A", resp, err)
	}
	if reqs := g.fake.Requests(); len(reqs) != 0 {
		t.Fatalf("FetchSecrets 不应请求 GitHub：%v", reqs)
	}
	h.exec(`UPDATE environments SET ref_policy='["refs/tags/v*"]' WHERE id=$1`, envID)
	if _, err := fetch(); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "ref 策略不允许") {
		t.Fatalf("ref 策略收紧后应拒绝：%v", err)
	}
	h.exec(`UPDATE jobs SET state='cancelled' WHERE id=$1`, jobID)
	if _, err := fetch(); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("job 结束后不应再下发 secret：%v", err)
	}
	if n := len(g.tokens()); n != 0 {
		t.Fatalf("FetchSecrets 签发了 %d 枚 token", n)
	}
}

// IssueGitToken 对任务仓库和声明的依赖各签一枚只读 token，范围只有那一个仓库；未声明的仓库拒绝且不签发；同一 op_id 重试拿回
// 同一枚，换了仓库的重试拒绝。
func TestService_IssueGitToken_scope(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	app := g.mustIssue("op-app", "tester/app")
	lib := g.mustIssue("op-lib", "Tester/LIB") // 仓库名不区分大小写
	toks := g.tokens()
	if len(toks) != 2 {
		t.Fatalf("签发了 %d 枚 token，期望 2 枚", len(toks))
	}
	for i, want := range []struct {
		resp *runnerv1.IssueGitTokenResponse
		repo int64
	}{{app, g.app}, {lib, g.lib}} {
		tok := toks[i]
		if tok.Token != want.resp.Token || want.resp.ExpiresAt != tok.ExpiresAt.Unix() || !slices.Equal(tok.RepoIDs, []int64{want.repo}) ||
			!maps.Equal(tok.Permissions, map[string]string{"contents": "read"}) || tok.Installation != testInstallation || tok.Revoked {
			t.Fatalf("token %d = %+v，期望只覆盖仓库 %d 的 contents: read（响应 %+v）", i, tok, want.repo, want.resp)
		}
	}
	if again := g.mustIssue("op-app", "tester/app"); again.Token != app.Token {
		t.Fatalf("同一 op_id 重试应拿回同一枚 token：%s，第一次 %s", again.Token, app.Token)
	}
	if _, err := g.issue("op-app", "tester/lib"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("同一 op_id 换仓库应拒绝：%v", err)
	}
	for _, repo := range []string{"tester/other", "tester/missing", ""} {
		if _, err := g.issue("op-"+repo, repo); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("未声明的仓库 %q 应拒绝（PermissionDenied）：%v", repo, err)
		}
	}
	if _, err := g.issue("", "tester/app"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("没有 op_id 应拒绝：%v", err)
	}
	if n := len(g.tokens()); n != 2 {
		t.Fatalf("重试和被拒绝的请求不应签发新 token，共签发 %d 枚", n)
	}
	var audits, leaked int
	if err := g.db.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE detail->>'token_fingerprint' <> ''),
		count(*) FILTER (WHERE detail::text LIKE '%ghs_%') FROM audit_log WHERE action=$1`, auditGitIssue).Scan(&audits, &leaked); err != nil {
		t.Fatal(err)
	}
	if audits != 3 || leaked != 0 {
		t.Fatalf("每次交出 token 应写一条带指纹、不含 token 的审计日志：%d 条，含 token 的 %d 条", audits, leaked)
	}
}

// 租约已过期（sweeper 还没处理）时拒绝签发，不请求 GitHub。
func TestService_IssueGitToken_leaseExpired(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	g.exec(`UPDATE jobs SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, g.jobID)
	_, err := g.issue("op", "tester/app")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "租约已过期") {
		t.Fatalf("租约过期应拒绝：%v", err)
	}
	if reqs := g.fake.Requests(); len(reqs) != 0 {
		t.Fatalf("租约过期时不应请求 GitHub：%v", reqs)
	}
	if g.job(g.jobID).State != store.JobRunning {
		t.Fatal("测试前提：sweeper 没有运行，job 仍是 running")
	}
}

// 交出前的最终检查：GitHub 签发期间 Runner 被吊销，或租约过期，token 不交出并立即吊销。
func TestService_IssueGitToken_finalCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change string
		check  func(error) bool
	}{
		{"Runner 被吊销", `UPDATE runners SET state='revoked' WHERE id=$1`, rpcerr.IsRunnerRevoked},
		{"租约过期", `UPDATE jobs SET lease_expires_at=now() - interval '1 second' WHERE current_attempt_id IN
			(SELECT id FROM attempts WHERE runner_id=$1)`, func(err error) bool { return connect.CodeOf(err) == connect.CodeFailedPrecondition }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGitHarness(t)
			g.pauseCreate(func() { g.exec(tc.change, g.runner.id) })
			_, err := g.issue("op", "tester/app")
			if !tc.check(err) {
				t.Fatalf("最终检查不通过时应拒绝：%v", err)
			}
			if n := g.wantRevoked(); n != 1 {
				t.Fatalf("GitHub 应签发过 1 枚 token（随即吊销），实际 %d 枚", n)
			}
		})
	}
}

// RevokeGitToken 按 op_id 关闭操作并吊销；只带 op_id 也能关闭（客户端没收到 token），关闭后同一 op_id 不再签发；不要求租约，
// job 结束后仍可关闭；别的 Runner 不能关闭。
func TestService_RevokeGitToken(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	g.mustIssue("op-1", "tester/app")
	if err := g.revoke("op-1", ""); err != nil {
		t.Fatalf("RevokeGitToken：%v", err)
	}
	if n := g.wantRevoked(); n != 1 {
		t.Fatalf("签发了 %d 枚 token，期望 1 枚", n)
	}
	if _, err := g.issue("op-1", "tester/app"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("关闭后同一 op_id 不应再签发：%v", err)
	}
	// 先关闭、后签发（关闭先到达）：之后的签发同样被拒绝
	if err := g.revoke("op-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.issue("op-2", "tester/app"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("先关闭后签发应拒绝：%v", err)
	}
	if n := len(g.tokens()); n != 1 {
		t.Fatalf("关闭后的签发不应请求 GitHub，共签发 %d 枚", n)
	}
	other := g.enrollRunner()
	if _, err := g.client.RevokeGitToken(t.Context(), withAuth(other.token, &runnerv1.RevokeGitTokenRequest{Ref: g.ref, OpId: "op-3"})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的 Runner 不能关闭这个 attempt 的操作：%v", err)
	}
	stale := &runnerv1.AttemptRef{AttemptId: g.ref.AttemptId, Epoch: g.ref.Epoch + 1}
	if _, err := g.client.RevokeGitToken(t.Context(), withAuth(g.runner.token, &runnerv1.RevokeGitTokenRequest{Ref: stale, OpId: "op-3"})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("epoch 不一致应拒绝：%v", err)
	}
	// 租约过期、job 结束后仍可关闭（只要求归属和 epoch）
	g.exec(`UPDATE jobs SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, g.jobID)
	if err := g.revoke("op-4", ""); err != nil {
		t.Fatalf("租约过期后关闭应成功：%v", err)
	}
	g.finishCommitted(engine.AttemptResult{Outcome: store.OutcomeSucceeded})
	if err := g.revoke("op-5", ""); err != nil {
		t.Fatalf("job 结束后关闭应成功：%v", err)
	}
}

// 关闭时客户端带上 token：登记中没有这枚 token（Server 重启丢了登记）时用它补吊销。
func TestService_RevokeGitToken_clientToken(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	tok := g.mustIssue("op", "tester/app").Token
	// 同一个数据库、密钥和 GitHub，换一个新的登记：模拟 Server 重启
	svc := *g.svc
	svc.gitcred = gitcred.New(gitcred.Config{}, gitcred.Deps{GitHub: g.svc.gh, Log: g.svc.log})
	if _, err := svc.RevokeGitToken(t.Context(), withAuth(g.runner.token, &runnerv1.RevokeGitTokenRequest{
		Ref: g.ref, OpId: "op", Token: tok,
	})); err != nil {
		t.Fatalf("RevokeGitToken：%v", err)
	}
	if n := g.wantRevoked(); n != 1 {
		t.Fatalf("签发了 %d 枚 token，期望 1 枚", n)
	}
}

// attempt 已经结束后带 token 的 RevokeGitToken（Server 重启丢了登记）：照样吊销这枚 token，且不在登记中留下关闭记录
// （gitcred.RevokeEnded，不能用 Close）。用同一个键直接向新的登记签发来确认没有记录：有关闭记录时会被拒绝。
func TestService_RevokeGitToken_afterAttemptEnded(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	tok := g.mustIssue("op", "tester/app").Token
	g.finishCommitted(engine.AttemptResult{Outcome: store.OutcomeSucceeded})
	svc := *g.svc
	svc.gitcred = gitcred.New(gitcred.Config{}, gitcred.Deps{GitHub: g.svc.gh, Log: g.svc.log})
	if _, err := svc.RevokeGitToken(t.Context(), withAuth(g.runner.token, &runnerv1.RevokeGitTokenRequest{
		Ref: g.ref, OpId: "op", Token: tok,
	})); err != nil {
		t.Fatalf("attempt 结束后 RevokeGitToken：%v", err)
	}
	if got, ok := g.fake.IssuedToken(tok); !ok || !got.Revoked {
		t.Fatalf("attempt 结束后带来的 token 应被吊销：%+v", got)
	}
	scope := gitcred.Scope{Installation: testInstallation, RepoID: g.app, Repository: "tester/app", Permissions: map[string]string{"contents": "read"}}
	probe, err := svc.gitcred.Issue(t.Context(), gitKey(g.ref, "op"), scope, func(context.Context) error { return nil })
	if err != nil || probe == nil {
		t.Fatalf("登记中不应留下这个操作的记录（直接签发应成功）：%v", err)
	}
}

// 发起签发的客户端在 GitHub 签发期间断开：这次调用失败，但操作不关闭，同一 op_id 重放拿到同一枚 token（没有被吊销）。
func TestService_IssueGitToken_clientCancelled(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	g.pauseCreate(cancel)
	if _, err := g.client.IssueGitToken(ctx, withAuth(g.runner.token, &runnerv1.IssueGitTokenRequest{
		Ref: g.ref, OpId: "op", Repository: "tester/app",
	})); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("客户端取消后这次调用应失败：%v", err)
	}
	again := g.mustIssue("op", "tester/app")
	toks := g.tokens()
	if len(toks) != 1 || toks[0].Token != again.Token || toks[0].Revoked {
		t.Fatalf("重放应拿到第一次签发、没有吊销的那一枚：%+v，重放得到 %s", toks, again.Token)
	}
}

// job 声明的依赖属于别的 installation：拒绝且不请求 GitHub 签发。
func TestService_IssueGitToken_foreignDependency(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	g.addGitHubRepo(g.fake, func(o *repoOpts) { o.name, o.installation = "foreign", 778 })
	g.addJob(g.addRun(g.app), func(j *jobRow) { j.spec.Repositories = []string{"tester/foreign"} })
	r := g.enrollRunner()
	ref := r.mustAcquire(1, 1)[0].Ref
	_, err := g.client.IssueGitToken(t.Context(), withAuth(r.token, &runnerv1.IssueGitTokenRequest{Ref: ref, OpId: "op", Repository: "tester/foreign"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "不在同一个 installation 下") {
		t.Fatalf("别的 installation 的依赖应拒绝：%v", err)
	}
	if n := len(g.tokens()); n != 0 {
		t.Fatalf("不应签发 token，签发了 %d 枚", n)
	}
}

// attempt 结束与签发的交错（真实提交的事务）：
//   - 签发前 attempt 已经结束（结束先提交，失效处理还没执行）：最终检查不通过，token 不交出并吊销；
//   - 交出之后才结束（Complete、sweeper 判定的租约过期、Server 判定的超时）：失效处理吊销已交出的 token，之后不再签发。
func TestService_IssueGitToken_attemptEnds(t *testing.T) {
	t.Parallel()
	t.Run("结束先于最终检查提交", func(t *testing.T) {
		t.Parallel()
		g := newGitHarness(t)
		g.pauseCreate(func() { g.finishCommitted(engine.AttemptResult{Outcome: store.OutcomeSucceeded}) })
		if _, err := g.issue("op", "tester/app"); !rpcerr.IsStaleEpoch(err) {
			t.Fatalf("attempt 已结束时不应交出 token：%v", err)
		}
		if n := g.wantRevoked(); n != 1 {
			t.Fatalf("应签发过 1 枚 token（随即吊销），实际 %d 枚", n)
		}
	})
	tests := []struct {
		name string
		end  func(g *gitHarness)
	}{
		{"Complete", func(g *gitHarness) {
			if err := g.runner.complete(&runnerv1.CompleteRequest{Ref: g.ref, Outcome: runnerv1.Outcome_OUTCOME_CANCELLED}); err != nil {
				g.t.Fatalf("Complete：%v", err)
			}
		}},
		{"sweeper 判定租约过期", func(g *gitHarness) {
			g.exec(`UPDATE jobs SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, g.jobID)
			g.eng.SweepOnce(g.t.Context())
		}},
		{"sweeper 判定超时", func(g *gitHarness) {
			g.exec(`UPDATE jobs SET started_at=now() - interval '1 day' WHERE id=$1`, g.jobID)
			g.eng.SweepOnce(g.t.Context())
		}},
	}
	for _, tc := range tests {
		t.Run("交出后由"+tc.name+"结束", func(t *testing.T) {
			t.Parallel()
			g := newGitHarness(t)
			g.mustIssue("op-app", "tester/app")
			g.mustIssue("op-lib", "tester/lib")
			tc.end(g)
			g.waitRevocations()
			if s := g.job(g.jobID); s.State == store.JobRunning && s.Epoch == g.ref.Epoch {
				t.Fatalf("测试前提：attempt 应已结束，job = %+v", s)
			}
			if n := g.wantRevoked(); n != 2 {
				t.Fatalf("attempt 结束后已交出的 2 枚 token 都应吊销，签发了 %d 枚", n)
			}
			if _, err := g.issue("op-new", "tester/app"); err == nil {
				t.Fatal("attempt 结束后不应再签发")
			}
			if n := len(g.tokens()); n != 2 {
				t.Fatalf("attempt 结束后的签发不应请求 GitHub，共签发 %d 枚", n)
			}
		})
	}
}

// 结束 attempt 的事务已经提交、失效通知却没有发出（例如提交确认丢失，调用方收到错误、跳过了 AttemptEnded）：下一个
// sweeper 周期的对账补上——已交出的 token 被吊销、之后不再签发；还在执行的 attempt 不受对账影响。
func TestService_GitCredReconcile(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	g.mustIssue("op-app", "tester/app")
	// attempt 还在执行：对账不动它
	g.eng.SweepOnce(g.t.Context())
	g.waitRevocations()
	if toks := g.tokens(); len(toks) != 1 || toks[0].Revoked {
		t.Fatalf("执行中的 attempt 的 token 不应被吊销：%+v", toks)
	}
	g.mustIssue("op-lib", "tester/lib")
	// 结束已提交，但没有通知
	g.finishCommitted(engine.AttemptResult{Outcome: store.OutcomeSucceeded})
	if toks := g.tokens(); toks[0].Revoked || toks[1].Revoked {
		t.Fatal("测试前提：没有通知时 token 还没有吊销")
	}
	g.eng.SweepOnce(g.t.Context())
	g.waitRevocations()
	if n := g.wantRevoked(); n != 2 {
		t.Fatalf("对账后 2 枚 token 都应吊销，签发了 %d 枚", n)
	}
	if _, err := g.issue("op-new", "tester/app"); err == nil {
		t.Fatal("对账使 attempt 失效后不应再签发")
	}
	if n := len(g.tokens()); n != 2 {
		t.Fatalf("失效后的签发不应请求 GitHub，共签发 %d 枚", n)
	}
}

// job 结束（Complete，含失效处理）与在途的签发同时发生：GitHub 迟到的 token 被吊销、不交出。
func TestService_IssueGitToken_jobEndsWhileIssuing(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	g.pauseCreate(func() {
		if err := g.runner.complete(&runnerv1.CompleteRequest{Ref: g.ref, Outcome: runnerv1.Outcome_OUTCOME_CANCELLED}); err != nil {
			t.Errorf("Complete：%v", err)
		}
	})
	_, err := g.issue("op", "tester/app")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("签发在途时 job 结束，不应交出 token：%v", err)
	}
	if n := g.wantRevoked(); n != 1 {
		t.Fatalf("迟到的 token 应吊销，签发了 %d 枚", n)
	}
}

// 没有配置 GitHub App 时不签发；关闭照常成功（不会有 token）。
func TestService_IssueGitToken_noGitHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.enrollRunner()
	h.pendingJob()
	ref := r.mustAcquire(1, 1)[0].Ref
	_, err := h.client.IssueGitToken(t.Context(), withAuth(r.token, &runnerv1.IssueGitTokenRequest{Ref: ref, OpId: "op", Repository: "tester/x"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("没有 GitHub App 时应拒绝：%v", err)
	}
	if _, err := h.client.RevokeGitToken(t.Context(), withAuth(r.token, &runnerv1.RevokeGitTokenRequest{Ref: ref, OpId: "op"})); err != nil {
		t.Fatalf("没有 GitHub App 时关闭应成功：%v", err)
	}
}

// 依赖仓库的解析结果必须仍是声明的仓库，否则拒绝且不签发：
//   - 改名：Kiln 目录中没有声明的名称，定向查询按旧名称读到的是改名后的仓库；
//   - 陈旧目录项：目录中这个名称的仓库已经转到别的 installation；
//   - 已删除：目录项已停用，GitHub 上也读不到。
func TestService_IssueGitToken_dependencyResolution(t *testing.T) {
	t.Parallel()
	t.Run("改名", func(t *testing.T) {
		t.Parallel()
		fake := fakegithub.New("tester")
		t.Cleanup(fake.Close)
		const libID = 424242
		fake.AddRepo(&fakegithub.Repo{ID: libID, Owner: "tester", Name: "lib2", Private: true, Installation: testInstallation})
		// GitHub 对改名前的名称返回改名后的仓库（重定向）：替身没有这个行为，在它前面拦下这一个请求
		upstream, err := url.Parse(fake.URL)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.EqualFold(r.URL.Path, "/repos/tester/lib") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(gh.Repository{
					ID: libID, Name: "lib2", FullName: "tester/lib2", Owner: gh.Account{ID: 9001, Login: "tester"}, Private: true,
					DefaultBranch: "main", CloneURL: "https://github.com/tester/lib2.git",
				})
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(api.Close)
		h := newHarness(t, func(o *harnessOpts) {
			o.github = &gh.Client{AppID: fake.AppID, Key: testRSAKey(t), APIURL: api.URL, UploadURL: api.URL}
		})
		app := h.addGitHubRepo(fake, func(o *repoOpts) { o.name = "app" })
		h.addJob(h.addRun(app), func(j *jobRow) { j.spec.Repositories = []string{"tester/lib"} })
		r := h.enrollRunner()
		ref := r.mustAcquire(1, 1)[0].Ref
		_, err = h.client.IssueGitToken(t.Context(), withAuth(r.token, &runnerv1.IssueGitTokenRequest{Ref: ref, OpId: "op", Repository: "tester/lib"}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "tester/lib2") {
			t.Fatalf("改名后的依赖应拒绝：%v", err)
		}
		if toks := sourceTokens(fake.IssuedTokens()); len(toks) != 0 {
			t.Fatalf("不应签发源码 token：%+v", toks)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(g *gitHarness)
	}{
		{"陈旧目录项", func(g *gitHarness) {
			g.exec(`UPDATE repos SET installation_id=778 WHERE id=$1`, g.lib)
		}},
		{"已删除", func(g *gitHarness) {
			g.exec(`UPDATE repos SET active=false WHERE id=$1`, g.lib)
			g.fake.RemoveRepo(&fakegithub.Repo{Owner: "tester", Name: "lib"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGitHarness(t)
			tc.change(g)
			if _, err := g.issue("op", "tester/lib"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("应拒绝：%v", err)
			}
			if toks := sourceTokens(g.tokens()); len(toks) != 0 {
				t.Fatalf("不应签发源码 token：%+v", toks)
			}
		})
	}
}

// sourceTokens 返回签发记录中的源码 token（限定了仓库的 contents: read）；GitHub 客户端定向查询仓库时自己的
// installation token 不限定仓库，不算。
func sourceTokens(toks []fakegithub.Token) []fakegithub.Token {
	return slices.DeleteFunc(toks, func(t fakegithub.Token) bool { return len(t.RepoIDs) == 0 })
}

// 越权与校验：别的 Runner 的 attempt、错误的 epoch、environment 策略不允许时都拒绝，且不请求 GitHub。
func TestService_IssueGitToken_rejectsBeforeGitHub(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	other := g.enrollRunner()
	if _, err := g.client.IssueGitToken(t.Context(), withAuth(other.token, &runnerv1.IssueGitTokenRequest{
		Ref: g.ref, OpId: "op", Repository: "tester/app",
	})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的 Runner 的 attempt 应拒绝：%v", err)
	}
	if _, err := g.client.IssueGitToken(t.Context(), withAuth(g.runner.token, &runnerv1.IssueGitTokenRequest{
		Ref: &runnerv1.AttemptRef{AttemptId: g.ref.AttemptId, Epoch: g.ref.Epoch + 1}, OpId: "op", Repository: "tester/app",
	})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("错误的 epoch 应拒绝：%v", err)
	}
	if _, err := g.client.RevokeGitToken(t.Context(), withAuth(other.token, &runnerv1.RevokeGitTokenRequest{Ref: g.ref, OpId: "op"})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的 Runner 不能关闭这个 attempt 的操作：%v", err)
	}
	var envID int64
	if err := g.db.QueryRow(t.Context(), `INSERT INTO environments (owner_id, repo_id, name, ref_policy)
		VALUES (9001, $1, 'prod', '["refs/tags/v*"]') RETURNING id`, g.app).Scan(&envID); err != nil {
		t.Fatal(err)
	}
	g.addJob(g.addRun(g.app), func(j *jobRow) { j.spec.Environment = "prod" })
	r := g.enrollRunner()
	ref := r.mustAcquire(1, 1)[0].Ref
	if _, err := g.client.IssueGitToken(t.Context(), withAuth(r.token, &runnerv1.IssueGitTokenRequest{
		Ref: ref, OpId: "op", Repository: "tester/app",
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "ref 策略不允许") {
		t.Fatalf("environment 策略不允许时应拒绝：%v", err)
	}
	if reqs := g.fake.Requests(); len(reqs) != 0 {
		t.Fatalf("被拒绝的签发不应请求 GitHub：%v", reqs)
	}
}

// 没有配置 GitHub App 时，RevokeGitToken 的空操作也要先通过归属检查。
func TestService_RevokeGitToken_noGitHubChecksOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r, other := h.enrollRunner(), h.enrollRunner()
	h.pendingJob()
	ref := r.mustAcquire(1, 1)[0].Ref
	if _, err := h.client.RevokeGitToken(t.Context(), withAuth(other.token, &runnerv1.RevokeGitTokenRequest{Ref: ref, OpId: "op"})); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的 Runner 应被拒绝：%v", err)
	}
}

// RPC 层的错误码：GitHub 确定没有签发是 Unavailable，同一 op_id 重试拿到 token；结果不明是 Aborted、记一条不含 token
// 的审计事件（到期时间是估计值），之后同一 op_id 被拒绝；Server 侧的最终检查超时是 Internal，之后同一 op_id 被拒绝。
func TestService_IssueGitToken_errorCodes(t *testing.T) {
	t.Parallel()
	t.Run("确定没有签发", func(t *testing.T) {
		t.Parallel()
		g := newGitHarness(t)
		g.fake.SetFault(fakegithub.CreateTokenRoute(testInstallation), fakegithub.Fault{Status: http.StatusForbidden})
		if _, err := g.issue("op", "tester/app"); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("确定没有签发应为 Unavailable：%v", err)
		}
		tok := g.mustIssue("op", "tester/app")
		if toks := g.tokens(); len(toks) != 1 || toks[0].Token != tok.Token {
			t.Fatalf("同一 op_id 重试应签发且只签发一枚：%+v", toks)
		}
	})
	t.Run("结果不明", func(t *testing.T) {
		t.Parallel()
		g := newGitHarness(t)
		g.fake.SetFault(fakegithub.CreateTokenRoute(testInstallation), fakegithub.Fault{Lost: true})
		if _, err := g.issue("op", "tester/app"); connect.CodeOf(err) != connect.CodeAborted {
			t.Fatalf("结果不明应为 Aborted：%v", err)
		}
		var n int
		var estimated bool
		if err := g.db.QueryRow(t.Context(), `SELECT count(*), bool_and((detail->>'expires_at_estimated')::bool AND detail->>'op'='op'
			AND detail->>'repository'='tester/app' AND detail::text NOT LIKE '%ghs_%') FROM audit_log WHERE action=$1`,
			auditGitIssueUnknown).Scan(&n, &estimated); err != nil {
			t.Fatal(err)
		}
		if n != 1 || !estimated {
			t.Fatalf("应有一条标明估计到期时间、不含 token 的审计事件：%d 条，%v", n, estimated)
		}
		if _, err := g.issue("op", "tester/app"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("结果不明之后同一 op_id 应被拒绝：%v", err)
		}
		g.wantNoIssueAudit()
	})
	t.Run("最终检查超时", func(t *testing.T) {
		t.Parallel()
		g := newGitHarness(t, func(o *harnessOpts) { o.gitcred.CheckTimeout = 100 * time.Millisecond })
		// 签发期间锁住 runners 表，交出前的最终检查读不到 Runner 状态、等到超时
		var lock pgx.Tx
		g.pauseCreate(func() {
			var err error
			if lock, err = g.db.Begin(t.Context()); err == nil {
				_, err = lock.Exec(t.Context(), `LOCK TABLE runners IN ACCESS EXCLUSIVE MODE`)
			}
			if err != nil {
				t.Errorf("锁表：%v", err)
			}
		})
		_, err := g.issue("op", "tester/app")
		if lock != nil {
			_ = lock.Rollback(t.Context())
		}
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("最终检查超时应为 Internal：%v", err)
		}
		if n := g.wantRevoked(); n != 1 {
			t.Fatalf("签发过的 token 应被吊销：%d 枚", n)
		}
		if _, err := g.issue("op", "tester/app"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("最终检查超时之后同一 op_id 应被拒绝：%v", err)
		}
	})
}

// wantNoIssueAudit 检查没有交出 token 的审计事件。
func (g *gitHarness) wantNoIssueAudit() {
	g.t.Helper()
	var n int
	if err := g.db.QueryRow(g.t.Context(), `SELECT count(*) FROM audit_log WHERE action=$1`, auditGitIssue).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	if n != 0 {
		g.t.Fatalf("没有交出 token 时不应有 %s 事件：%d 条", auditGitIssue, n)
	}
}

// 写审计日志时客户端断开：审计不随请求取消，写成后才算交出；操作保持打开，同一 op_id 重放拿到同一枚。用表锁让审计停在
// 写入上，确认停住之后再取消。
func TestService_IssueGitToken_cancelDuringAudit(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	lock, err := g.db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := lock.Exec(t.Context(), `LOCK TABLE audit_log IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := g.svc.IssueGitToken(ctx, withAuth(g.runner.token, &runnerv1.IssueGitTokenRequest{Ref: g.ref, OpId: "op", Repository: "tester/app"}))
		done <- err
	}()
	// 等到审计的 INSERT 在表锁上等待
	for {
		var waiting bool
		if err := g.db.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
			WHERE c.relname='audit_log' AND NOT l.granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("签发没有停在审计上就结束了：%v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := lock.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("审计写成后应交出 token（客户端已经断开，这次的响应没人收到）：%v", err)
	}
	again := g.mustIssue("op", "tester/app")
	toks := g.tokens()
	if len(toks) != 1 || toks[0].Token != again.Token || toks[0].Revoked {
		t.Fatalf("重放应拿到同一枚、没有吊销的 token：%+v，重放得到 %s", toks, again.Token)
	}
}

// attempt 结束时对 GitHub 的吊销在后台：吊销被挡住时 Complete 已经返回、之后的签发被拒绝；放行后 token 被吊销。
func TestService_Complete_revokesInBackground(t *testing.T) {
	t.Parallel()
	g := newGitHarness(t)
	tok := g.mustIssue("op", "tester/app").Token
	entered, release := make(chan struct{}), make(chan struct{})
	g.fake.SetFault(fakegithub.RevokeTokenRoute, fakegithub.Fault{Pause: func() {
		close(entered)
		<-release
	}})
	if err := g.runner.complete(&runnerv1.CompleteRequest{Ref: g.ref, Outcome: runnerv1.Outcome_OUTCOME_CANCELLED}); err != nil {
		t.Fatalf("吊销被挡住时 Complete 应照常返回：%v", err)
	}
	<-entered
	if _, err := g.issue("op-new", "tester/app"); err == nil {
		t.Fatal("attempt 结束后不应再签发")
	}
	if got, _ := g.fake.IssuedToken(tok); got.Revoked {
		t.Fatal("吊销被挡住时 token 不应已经吊销")
	}
	close(release)
	g.waitRevocations()
	if got, _ := g.fake.IssuedToken(tok); !got.Revoked {
		t.Fatal("放行后 token 应被吊销")
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 执行节点的 git 凭据（PLAN #33.13）：Server 按
// 操作签发只覆盖一个仓库的 GitHub App installation token，经 AI、CI 共用的签发登记。

const (
	credLibID    = 101
	credSecretID = 102
	credOtherID  = 103
	credOtherIns = 888
	credKnownID  = 104
)

// 访问的仓库与权限。
const (
	credDemo = portOwner + "/demo"
	credLib  = portOwner + "/lib"
	read     = airunnerv2.CredentialAccess_CREDENTIAL_ACCESS_READ
	write    = airunnerv2.CredentialAccess_CREDENTIAL_ACCESS_WRITE
)

// credPort 是声明了依赖仓库的环境：tester/lib、tester/secret 与任务仓库在同一个 installation，other/x 在另一个。都在
// fakegithub 中登记：签发时替身按真实的规则校验仓库属于 installation，未声明的 tester/secret 由 Server 拒绝而不是替身。
func credPort(t *testing.T) *splitPort {
	t.Helper()
	p := newSplitPort(t, portConfig)
	p.fake.AddRepo(&fakegithub.Repo{ID: credLibID, Owner: portOwner, Name: "lib", Dir: p.dir, Private: true})
	p.fake.AddRepo(&fakegithub.Repo{ID: credSecretID, Owner: portOwner, Name: "secret", Dir: p.dir, Private: true})
	p.fake.AddInstallation(credOtherIns)
	p.fake.AddRepo(&fakegithub.Repo{ID: credOtherID, Owner: "other", Name: "x", Dir: p.dir, Private: true, Installation: credOtherIns})
	return p
}

// withRepos 返回声明了依赖仓库 repos 的配置。
func withRepos(repos string) string {
	return strings.Replace(portConfig, "version: 1\n", "version: 1\nrequirements: {repositories: ["+repos+"]}\n", 1)
}

// issue 为操作 op 签发 repo 的凭据。
func (p *splitPort) issue(r *portRunner, t *portTask, access airunnerv2.CredentialAccess, op, repo string) (*airunnerv2.Credential, error) {
	resp, err := p.ai.IssueCredential(p.ctx, withToken(r.token, &airunnerv2.IssueCredentialRequest{
		Ref: ref(t), Kind: airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT, Access: access, OpId: op, Repository: repo,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCredential(), nil
}

func (p *splitPort) mustIssue(r *portRunner, t *portTask, access airunnerv2.CredentialAccess, op, repo string) *airunnerv2.Credential {
	p.t.Helper()
	c, err := p.issue(r, t, access, op, repo)
	if err != nil {
		p.t.Fatalf("签发 %s（%s）：%v", op, repo, err)
	}
	return c
}

// revokeCred 关闭操作 op（secret 可以为空）。
func (p *splitPort) revokeCred(r *portRunner, t *portTask, op, secret string) error {
	_, err := p.ai.RevokeCredential(p.ctx, withToken(r.token, &airunnerv2.RevokeCredentialRequest{
		Ref: ref(t), Kind: airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT, OpId: op, Secret: secret,
	}))
	return err
}

// issuedToken 返回 fake GitHub 中签发的这个 token（仓库范围、权限、是否已经吊销）。
func (p *splitPort) issuedToken(secret string) fakegithub.Token {
	p.t.Helper()
	tok, ok := p.fake.IssuedToken(secret)
	if !ok {
		p.t.Fatalf("fake GitHub 没有签发过 %q", secret)
	}
	return tok
}

// scopedTokens 返回 fake GitHub 签发的限定了权限的 token（Server 查找仓库时用的完整权限 token 不算）。
func (p *splitPort) scopedTokens() []fakegithub.Token {
	var out []fakegithub.Token
	for _, tk := range p.fake.IssuedTokens() {
		if tk.Permissions != nil {
			out = append(out, tk)
		}
	}
	return out
}

// wantAllRevoked 检查签发的限定权限的 token 共 n 枚、都已吊销。
func (p *splitPort) wantAllRevoked(n int) {
	p.t.Helper()
	toks := p.scopedTokens()
	if len(toks) != n {
		p.t.Fatalf("签发了 %d 枚 token，期望 %d 枚", len(toks), n)
	}
	for _, tk := range toks {
		if !tk.Revoked {
			p.t.Fatalf("token %s（仓库 %v）没有吊销", tk.Token, tk.RepoIDs)
		}
	}
}

// waitRevocations 等签发登记在后台的吊销（attempt 失效时发起）全部结束。
func (p *splitPort) waitRevocations() {
	p.t.Helper()
	if err := p.s.gitCred.Wait(p.ctx); err != nil {
		p.t.Fatalf("等待后台吊销：%v", err)
	}
}

// pauseCreate 让下一次签发 token 的请求先执行 f（在 GitHub 签发之前、Server 等待响应时），模拟与签发在途交错的操作。
func (p *splitPort) pauseCreate(f func()) {
	p.fake.SetFault(fakegithub.CreateTokenRoute(p.fake.InstallationID), fakegithub.Fault{Pause: f})
}

// endCommitted 在一个真实提交的事务中结束 attempt（任务取消、attempt 有结果），但不经过 Core（不调用失效处理）：模拟结束
// 已经提交、失效处理还没执行（或 Server 在两者之间重启）的时刻。
func (p *splitPort) endCommitted(t *portTask) {
	p.t.Helper()
	mustNil(p.t, p.s.db.Tx(p.ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(p.ctx, `UPDATE ai_attempts SET outcome='cancelled', finished_at=now() WHERE id=$1`, t.attempt); err != nil {
			return err
		}
		_, err := tx.Exec(p.ctx, `UPDATE ai_tasks SET state='cancelled', finished_at=now(), lease_expires_at=NULL WHERE id=$1`, t.id)
		return err
	}))
}

// credAudits 返回签发 git 凭据的审计日志的 detail。
func (p *splitPort) credAudits() []map[string]any {
	p.t.Helper()
	rows, err := p.s.db.Query(p.ctx, `SELECT actor, detail FROM audit_log WHERE action='ai.credential.issue' ORDER BY id`)
	mustNil(p.t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var actor string
		var raw []byte
		mustNil(p.t, rows.Scan(&actor, &raw))
		var d map[string]any
		mustNil(p.t, json.Unmarshal(raw, &d))
		d["actor"] = actor
		out = append(out, d)
	}
	mustNil(p.t, rows.Err())
	return out
}

func wantCode(t *testing.T, what string, err error, code connect.Code, msg string) {
	t.Helper()
	if connect.CodeOf(err) != code || !strings.Contains(err.Error(), msg) {
		t.Fatalf("%s：%v，期望 %v（包含 %q）", what, err, code, msg)
	}
}

// reviewTask 规划并领取一个审计任务：依赖仓库从 PR 的 base 读取（tester/lib），head 中多声明的 tester/secret 不算。
func reviewTask(t *testing.T, p *splitPort) (*portRunner, *portTask) {
	t.Helper()
	p.base = p.commit("main", map[string]string{agentcfg.ConfigPath: withRepos(credLib)})
	head := p.commit(prBranch(7), map[string]string{agentcfg.ConfigPath: withRepos(credLib + ", tester/secret")})
	p.planReview(7, head)
	r := p.runner(runnerSpec{})
	resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: p.state(r), RequestId: "req-1"}))
	mustNil(t, err)
	at := resp.Msg.GetTask()
	if at == nil || !slices.Equal(at.Repositories, []string{credLib}) {
		t.Fatalf("下发的任务 = %v，期望依赖仓库只有 base 声明的 %s", at, credLib)
	}
	return r, &portTask{id: at.TaskId, attempt: at.Ref.AttemptId, epoch: at.Ref.Epoch}
}

// 审计任务：每个操作一枚只读 token，只覆盖请求的那一个仓库（任务仓库或 base 声明的依赖，不区分大小写）；同一操作重试拿回
// 同一枚，换仓库被拒绝；未声明的仓库、写凭据、缺 op_id 被拒绝且不签发；每次签发一条审计日志（有指纹、没有 token）。
func TestAICredentialReview(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)

	app := p.mustIssue(r, task, read, "op-app", credDemo)
	if app.Username != ai.GitUsername || app.Secret == "" || app.ExpiresAt == 0 || !slices.Equal(app.Scopes, []string{credDemo}) ||
		app.Kind != airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT || app.Access != read {
		t.Fatalf("任务仓库的读凭据 = %v", app)
	}
	if tok := p.issuedToken(app.Secret); !slices.Equal(tok.RepoIDs, []int64{portRepoID}) || len(tok.Permissions) != 1 ||
		tok.Permissions["contents"] != "read" || tok.Installation != p.fake.InstallationID || tok.ExpiresAt.Unix() != app.ExpiresAt {
		t.Fatalf("任务仓库的 token = %+v", tok)
	}
	lib := p.mustIssue(r, task, read, "op-lib", "TESTER/Lib")
	if tok := p.issuedToken(lib.Secret); !slices.Equal(tok.RepoIDs, []int64{credLibID}) || tok.Permissions["contents"] != "read" ||
		!slices.Equal(lib.Scopes, []string{credLib}) {
		t.Fatalf("依赖仓库的凭据 = %v，token = %+v", lib, tok)
	}

	// 同一操作重试（连接器没收到响应）：同一枚，不再请求 GitHub
	again := p.mustIssue(r, task, read, "op-app", credDemo)
	if again.Secret != app.Secret || len(p.scopedTokens()) != 2 {
		t.Fatalf("同一 op_id 重试应拿回同一枚：%v，共签发 %d 枚", again, len(p.scopedTokens()))
	}
	_, err := p.issue(r, task, read, "op-app", credLib)
	wantCode(t, "同一操作换仓库", err, connect.CodeInvalidArgument, "")

	_, err = p.issue(r, task, read, "op-secret", "tester/secret")
	wantCode(t, "未声明的仓库", err, connect.CodePermissionDenied, "requirements.repositories")
	_, err = p.issue(r, task, write, "op-w", credDemo)
	wantCode(t, "审计任务要写凭据", err, connect.CodePermissionDenied, "只读")
	_, err = p.issue(r, task, read, "", credDemo)
	wantCode(t, "缺 op_id", err, connect.CodeInvalidArgument, "op_id")
	if n := len(p.scopedTokens()); n != 2 {
		t.Fatalf("拒绝的请求不应签发，共签发 %d 枚", n)
	}

	// 每次交出一条（重试拿回同一枚时照样记），拒绝的不记
	p.wantCredAudits(r, task, app, lib)
}

// wantCredAudits 检查 TestAICredentialReview 的审计日志：app（op-app）、lib（op-lib）、app 的重试各一条，带 op 和指纹，没有 token。
func (p *splitPort) wantCredAudits(r *portRunner, task *portTask, app, lib *airunnerv2.Credential) {
	t := p.t
	t.Helper()
	audits := p.credAudits()
	if len(audits) != 3 || audits[1]["op"] != "op-lib" || audits[2]["op"] != "op-app" || audits[2]["token_fingerprint"] != gitcred.Fingerprint(app.Secret) {
		t.Fatalf("审计日志 = %v", audits)
	}
	a := audits[0]
	if a["access"] != "read" || a["kind"] != "git" || a["attempt_id"] != float64(task.attempt) || a["runner_id"] != float64(r.id) ||
		a["op"] != "op-app" || a["token_fingerprint"] != gitcred.Fingerprint(app.Secret) ||
		a["actor"] != "runner:"+strconv.FormatInt(r.id, 10) || a["expires_at"] == nil || !slices.Equal(anyStrings(a["repositories"]), []string{credDemo}) {
		t.Fatalf("审计日志 = %v", a)
	}
	for _, a := range audits {
		if raw, _ := json.Marshal(a); strings.Contains(string(raw), app.Secret) || strings.Contains(string(raw), lib.Secret) {
			t.Fatalf("审计日志中有 token：%s", raw)
		}
	}
}

// 开发任务：写凭据只覆盖任务仓库、只有 contents: write；依赖仓库只能要读凭据。
func TestAICredentialDevelop(t *testing.T) {
	p := credPort(t)
	wi := p.addWorkItem(5, "")
	req := p.planFix(wi, 1)
	tid := p.tasks(req)[0]
	p.exec(`UPDATE ai_tasks SET spec=jsonb_set(spec, '{repositories}', '["tester/lib"]') WHERE id=$1`, tid)
	r := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
	task, err := p.acquire(r, "dev-1")
	if err != nil || task == nil || task.id != tid {
		t.Fatalf("领取：%v, %v", task, err)
	}
	w := p.mustIssue(r, task, write, "op-push", credDemo)
	if wt := p.issuedToken(w.Secret); !slices.Equal(wt.RepoIDs, []int64{portRepoID}) || wt.Permissions["contents"] != "write" ||
		len(wt.Permissions) != 1 || !slices.Equal(w.Scopes, []string{credDemo}) || w.Access != write {
		t.Fatalf("写凭据 = %v，token = %+v", w, wt)
	}
	_, err = p.issue(r, task, write, "op-push-lib", credLib)
	wantCode(t, "依赖仓库的写凭据", err, connect.CodePermissionDenied, "只覆盖任务仓库")
	rd := p.mustIssue(r, task, read, "op-fetch-lib", credLib)
	if rt := p.issuedToken(rd.Secret); !slices.Equal(rt.RepoIDs, []int64{credLibID}) || rt.Permissions["contents"] != "read" {
		t.Fatalf("依赖仓库的读凭据 = %+v", rt)
	}
	if n := len(p.scopedTokens()); n != 2 {
		t.Fatalf("共签发 %d 枚，期望 2 枚（被拒绝的写凭据不签发）", n)
	}
	if audits := p.credAudits(); len(audits) != 2 || audits[0]["access"] != "write" || audits[1]["access"] != "read" {
		t.Fatalf("审计日志 = %v", audits)
	}
}

// 依赖仓库不在任务仓库的 installation 中（属于别的 installation、不存在）时拒绝，说明是哪个仓库；不签发 token。
func TestAICredentialForeignRepository(t *testing.T) {
	// other/known 已经在 Kiln 中、属于别的 installation（repodir.Lookup 照样返回它，由签发方判断）
	for _, repo := range []string{"other/x", "tester/missing", "other/known"} {
		t.Run(repo, func(t *testing.T) {
			p := credPort(t)
			p.fake.AddRepo(&fakegithub.Repo{ID: credKnownID, Owner: "other", Name: "known", Dir: p.dir, Private: true, Installation: credOtherIns})
			mustNil(t, store.UpsertRepo(p.ctx, p.s.db, store.RepoMeta{
				ID: credKnownID, InstallationID: credOtherIns, Owner: "other", Name: "known", Private: true, DefaultBranch: "main",
				CloneURL: "https://github.com/other/known.git",
			}))
			p.base = p.commit("main", map[string]string{agentcfg.ConfigPath: withRepos("tester/lib, " + repo)})
			planned(t, p, 7)
			r := p.runner(runnerSpec{})
			task := mustAcquire(t, p, r)
			_, err := p.issue(r, task, read, "op", repo)
			wantCode(t, "依赖仓库在别处", err, connect.CodeFailedPrecondition, "requirements.repositories 中的 "+repo+" 不在")
			if toks := p.scopedTokens(); len(toks) != 0 {
				t.Fatalf("拒绝时仍签发了限定权限的 token：%+v", toks)
			}
			if a := p.credAudits(); len(a) != 0 {
				t.Fatalf("拒绝时写了审计日志：%v", a)
			}
		})
	}
}

// 失效的 attempt 拿不到凭据：epoch 不对、别的节点、租约过期（sweeper 未运行）、attempt 已经结束、节点被吊销。
func TestAICredentialRejectsStale(t *testing.T) {
	setup := func(t *testing.T) (*splitPort, *portRunner, *portTask) {
		t.Helper()
		p := credPort(t)
		planned(t, p, 7)
		r := p.runner(runnerSpec{})
		return p, r, mustAcquire(t, p, r)
	}
	t.Run("epoch 与别的节点", func(t *testing.T) {
		p, r, task := setup(t)
		bad := *task
		bad.epoch++
		if _, err := p.issue(r, &bad, read, "op", credDemo); !rpcerr.IsStaleEpoch(err) {
			t.Fatalf("epoch 不对：%v", err)
		}
		other := p.runner(runnerSpec{})
		if _, err := p.issue(other, task, read, "op", credDemo); !rpcerr.IsStaleEpoch(err) {
			t.Fatalf("别的节点：%v", err)
		}
	})
	t.Run("租约过期", func(t *testing.T) {
		p, r, task := setup(t)
		// 只让租约过期、不跑 sweeper：签发自己按租约判断，不等 sweeper
		p.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, task.id)
		if _, err := p.issue(r, task, read, "op", credDemo); !rpcerr.IsStaleEpoch(err) {
			t.Fatalf("租约过期：%v", err)
		}
	})
	t.Run("已经结束", func(t *testing.T) {
		p, r, task := setup(t)
		if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeCancelled), message: "取消"}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.issue(r, task, read, "op", credDemo); !rpcerr.IsStaleEpoch(err) {
			t.Fatalf("结束之后：%v", err)
		}
	})
	t.Run("节点被吊销", func(t *testing.T) {
		p, r, task := setup(t)
		p.exec(`UPDATE runners SET state='revoked' WHERE id=$1`, r.id)
		if _, err := p.issue(r, task, read, "op", credDemo); !rpcerr.IsRunnerRevoked(err) {
			t.Fatalf("吊销之后：%v", err)
		}
	})
}

// 最终检查（交出之前在数据库中复查）：GitHub 签发期间节点被吊销、租约过期、attempt 结束（已提交，失效处理还没执行）时
// 不交出，这枚 token 随即吊销。
func TestAICredentialFinalCheck(t *testing.T) {
	tests := []struct {
		name   string
		change func(p *splitPort, r *portRunner, task *portTask)
		check  func(error) bool
	}{
		{"节点被吊销", func(p *splitPort, r *portRunner, _ *portTask) {
			p.exec(`UPDATE runners SET state='revoked' WHERE id=$1`, r.id)
		}, rpcerr.IsRunnerRevoked},
		{"租约过期", func(p *splitPort, _ *portRunner, task *portTask) {
			p.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, task.id)
		}, rpcerr.IsStaleEpoch},
		{"attempt 结束先于最终检查", func(p *splitPort, _ *portRunner, task *portTask) { p.endCommitted(task) }, rpcerr.IsStaleEpoch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := credPort(t)
			r, task := reviewTask(t, p)
			p.pauseCreate(func() { tc.change(p, r, task) })
			if _, err := p.issue(r, task, read, "op", credDemo); !tc.check(err) {
				t.Fatalf("最终检查不通过时应拒绝：%v", err)
			}
			p.wantAllRevoked(1)
			if a := p.credAudits(); len(a) != 0 {
				t.Fatalf("没有交出的 token 不应记审计日志：%v", a)
			}
		})
	}
}

// attempt 结束（Complete、sweeper 判定租约过期或超时）的事务提交后，已交出的 token 全部吊销（最终检查之后才结束），之后
// 的签发不再请求 GitHub；签发在途时 attempt 结束，迟到的 token 吊销、不交出。
func TestAICredentialAttemptEnds(t *testing.T) {
	tests := []struct {
		name string
		end  func(p *splitPort, r *portRunner, task *portTask)
	}{
		{"Complete", func(p *splitPort, r *portRunner, task *portTask) {
			if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeCancelled), message: "取消"}); err != nil {
				p.t.Fatalf("Complete：%v", err)
			}
		}},
		{"sweeper 判定租约过期", func(p *splitPort, _ *portRunner, task *portTask) {
			p.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, task.id)
			p.sweep()
		}},
		{"sweeper 判定超时", func(p *splitPort, _ *portRunner, task *portTask) {
			p.exec(`UPDATE ai_tasks SET started_at=now() - interval '1 day' WHERE id=$1`, task.id)
			p.sweep()
		}},
	}
	for _, tc := range tests {
		t.Run("交出后由"+tc.name+"结束", func(t *testing.T) {
			p := credPort(t)
			r, task := reviewTask(t, p)
			p.mustIssue(r, task, read, "op-app", credDemo)
			p.mustIssue(r, task, read, "op-lib", credLib)
			tc.end(p, r, task)
			var state string
			mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT state FROM ai_tasks WHERE id=$1`, task.id).Scan(&state))
			if state == string(ai.TaskRunning) {
				t.Fatal("测试前提：attempt 应已结束")
			}
			p.waitRevocations()
			p.wantAllRevoked(2)
			if _, err := p.issue(r, task, read, "op-new", credDemo); err == nil {
				t.Fatal("attempt 结束后不应再签发")
			}
			if n := len(p.scopedTokens()); n != 2 {
				t.Fatalf("attempt 结束后的签发不应请求 GitHub，共签发 %d 枚", n)
			}
		})
	}
	t.Run("签发在途时 Complete", func(t *testing.T) {
		p := credPort(t)
		r, task := reviewTask(t, p)
		p.pauseCreate(func() {
			if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeCancelled), message: "取消"}); err != nil {
				t.Errorf("Complete：%v", err)
			}
		})
		_, err := p.issue(r, task, read, "op", credDemo)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("签发在途时 attempt 结束，不应交出 token：%v", err)
		}
		p.waitRevocations()
		p.wantAllRevoked(1)
	})
}

// 结束 attempt 的事务已经提交、失效通知却没有发出（提交确认丢失等）：下一个 sweeper 周期的对账补上，已交出的 token 被
// 吊销、之后不再签发；还在执行的 attempt 不受对账影响。
func TestAICredentialReconcile(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	c := p.mustIssue(r, task, read, "op-app", credDemo)
	p.sweep()
	p.waitRevocations()
	if p.issuedToken(c.Secret).Revoked {
		t.Fatal("执行中的 attempt 的 token 不应被吊销")
	}
	p.endCommitted(task)
	if p.issuedToken(c.Secret).Revoked {
		t.Fatal("测试前提：没有通知时 token 还没有吊销")
	}
	p.sweep()
	p.waitRevocations()
	p.wantAllRevoked(1)
	if _, err := p.issue(r, task, read, "op-new", credDemo); err == nil {
		t.Fatal("对账使 attempt 失效后不应再签发")
	}
	if n := len(p.scopedTokens()); n != 1 {
		t.Fatalf("失效后的签发不应请求 GitHub，共签发 %d 枚", n)
	}
}

// RevokeCredential 按 op_id 关闭操作：只带 op_id 也能吊销 Server 已经签发的（连接器没收到 token）；关闭后同一 op_id 不再
// 签发，先关闭后签发同样被拒绝且不请求 GitHub；只要求归属和 epoch（租约过期后仍可关闭）。
func TestAICredentialRevoke(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	p.mustIssue(r, task, read, "op-1", credDemo)
	mustNil(t, p.revokeCred(r, task, "op-1", ""))
	p.wantAllRevoked(1)
	_, err := p.issue(r, task, read, "op-1", credDemo)
	wantCode(t, "关闭后同一 op_id", err, connect.CodeFailedPrecondition, "")
	mustNil(t, p.revokeCred(r, task, "op-2", ""))
	_, err = p.issue(r, task, read, "op-2", credDemo)
	wantCode(t, "先关闭后签发", err, connect.CodeFailedPrecondition, "")
	if n := len(p.scopedTokens()); n != 1 {
		t.Fatalf("关闭后的签发不应请求 GitHub，共签发 %d 枚", n)
	}
	err = p.revokeCred(r, task, "", "")
	wantCode(t, "关闭缺 op_id", err, connect.CodeInvalidArgument, "op_id")

	bad := *task
	bad.epoch++
	if err := p.revokeCred(r, &bad, "op-3", ""); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("epoch 不对时关闭：%v", err)
	}
	other := p.runner(runnerSpec{})
	if err := p.revokeCred(other, task, "op-3", ""); !rpcerr.IsStaleEpoch(err) {
		t.Fatalf("别的节点关闭：%v", err)
	}
	// 租约过期之后照样能关闭（只会收回权限）
	c := p.mustIssue(r, task, read, "op-4", credDemo)
	p.exec(`UPDATE ai_tasks SET lease_expires_at=now() - interval '1 second' WHERE id=$1`, task.id)
	mustNil(t, p.revokeCred(r, task, "op-4", c.Secret))
	if !p.issuedToken(c.Secret).Revoked {
		t.Fatal("租约过期后关闭没有吊销")
	}
}

// attempt 已经结束（Server 在结束提交之后重启，登记中没有它）之后带 token 的 RevokeCredential：照样吊销这枚 token，且不在
// 登记中留下关闭记录（gitcred.RevokeEnded，不能用 Close）。用同一个键直接向登记签发来确认没有记录：有关闭记录时会被拒绝。
func TestAICredentialRevokeAfterEnded(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	// 上一个 Server 进程签发的 token：这个进程的登记中没有它
	prev, err := p.s.gh.CreateToken(p.ctx, p.fake.InstallationID, []int64{portRepoID}, map[string]string{"contents": "read"})
	mustNil(t, err)
	p.endCommitted(task)
	mustNil(t, p.revokeCred(r, task, "op", prev.Token))
	if !p.issuedToken(prev.Token).Revoked {
		t.Fatal("attempt 结束后带来的 token 应被吊销")
	}
	key := gitcred.Key{Attempt: ai.GitCredAttempt(task.attempt), Epoch: task.epoch, Op: "op"}
	scope := gitcred.Scope{Installation: p.fake.InstallationID, RepoID: portRepoID, Repository: credDemo, Permissions: map[string]string{"contents": "read"}}
	probe, err := p.s.gitCred.Issue(p.ctx, key, scope, func(context.Context) error { return nil })
	if err != nil || probe == nil {
		t.Fatalf("登记中不应留下这个操作的记录（直接签发应成功）：%v", err)
	}
}

func anyStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// attempt 结束时对 GitHub 的吊销在后台：吊销被挡住时 Complete 已经返回、之后的签发被拒绝；放行后 token 被吊销。
func TestAICredentialCompleteRevokesInBackground(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	tok := p.mustIssue(r, task, read, "op", credDemo).Secret
	entered, release := make(chan struct{}), make(chan struct{})
	p.fake.SetFault(fakegithub.RevokeTokenRoute, fakegithub.Fault{Pause: func() {
		close(entered)
		<-release
	}})
	if _, err := p.complete(r, task, portResult{outcome: string(store.OutcomeCancelled), message: "取消"}); err != nil {
		t.Fatalf("吊销被挡住时 Complete 应照常返回：%v", err)
	}
	<-entered
	if _, err := p.issue(r, task, read, "op-new", credDemo); err == nil {
		t.Fatal("attempt 结束后不应再签发")
	}
	if p.issuedToken(tok).Revoked {
		t.Fatal("吊销被挡住时 token 不应已经吊销")
	}
	close(release)
	p.waitRevocations()
	if !p.issuedToken(tok).Revoked {
		t.Fatal("放行后 token 应被吊销")
	}
}

// 签发 RPC 的错误码：GitHub 确定没有签发是 Unavailable，同一 op_id 重试拿到 token；结果不明是 Aborted、记一条不含 token
// 的审计事件（到期时间是估计值），之后同一 op_id 被拒绝、没有交出的审计事件。
func TestAICredentialErrorCodes(t *testing.T) {
	t.Run("确定没有签发", func(t *testing.T) {
		p := credPort(t)
		r, task := reviewTask(t, p)
		p.fake.SetFault(fakegithub.CreateTokenRoute(p.fake.InstallationID), fakegithub.Fault{Status: http.StatusForbidden})
		_, err := p.issue(r, task, read, "op", credDemo)
		wantCode(t, "确定没有签发", err, connect.CodeUnavailable, "")
		c := p.mustIssue(r, task, read, "op", credDemo)
		if toks := p.scopedTokens(); len(toks) != 1 || toks[0].Token != c.Secret {
			t.Fatalf("同一 op_id 重试应签发且只签发一枚：%+v", toks)
		}
	})
	t.Run("结果不明", func(t *testing.T) {
		p := credPort(t)
		r, task := reviewTask(t, p)
		p.fake.SetFault(fakegithub.CreateTokenRoute(p.fake.InstallationID), fakegithub.Fault{Lost: true})
		_, err := p.issue(r, task, read, "op", credDemo)
		wantCode(t, "结果不明", err, connect.CodeAborted, "")
		var n int
		var ok bool
		mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT count(*), coalesce(bool_and((detail->>'expires_at_estimated')::bool AND detail->>'op'='op'
			AND detail->'repositories'->>0=$2 AND detail::text NOT LIKE '%ghs_%'), false) FROM audit_log WHERE action=$1`,
			"ai.credential.issue_unknown", credDemo).Scan(&n, &ok))
		if n != 1 || !ok {
			t.Fatalf("应有一条标明估计到期时间、不含 token 的审计事件：%d 条，%v", n, ok)
		}
		_, err = p.issue(r, task, read, "op", credDemo)
		wantCode(t, "结果不明之后同一 op_id", err, connect.CodeFailedPrecondition, "")
		if a := p.credAudits(); len(a) != 0 {
			t.Fatalf("没有交出 token 时不应有签发事件：%v", a)
		}
	})
}

// 写审计日志时连接器断开：审计不随请求取消，写成后才算交出；操作保持打开，同一 op_id 重放拿到同一枚。用表锁让审计停在
// 写入上，确认停住之后再取消。
func TestAICredentialCancelDuringAudit(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	lock, err := p.s.db.Begin(p.ctx)
	mustNil(t, err)
	defer func() { _ = lock.Rollback(context.WithoutCancel(p.ctx)) }()
	_, err = lock.Exec(p.ctx, `LOCK TABLE audit_log IN EXCLUSIVE MODE`)
	mustNil(t, err)
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := p.ai.IssueCredential(ctx, withToken(r.token, &airunnerv2.IssueCredentialRequest{
			Ref: ref(task), Kind: airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT, Access: read, OpId: "op", Repository: credDemo,
		}))
		done <- err
	}()
	// 等到审计的 INSERT 在表锁上等待
	for {
		var waiting bool
		mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
			WHERE c.relname='audit_log' AND NOT l.granted)`).Scan(&waiting))
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
	// 连接器这边的调用随取消返回（Canceled）；Server 端的审计仍在等锁
	if err := <-done; connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("取消后连接器这边的调用应返回 Canceled：%v", err)
	}
	mustNil(t, lock.Commit(p.ctx))
	again := p.mustIssue(r, task, read, "op", credDemo)
	toks := p.scopedTokens()
	if len(toks) != 1 || toks[0].Token != again.Secret || toks[0].Revoked {
		t.Fatalf("重放应拿到同一枚、没有吊销的 token：%+v，重放得到 %s", toks, again.Secret)
	}
}

// 交出前的审计日志写失败（用触发器让这条 INSERT 确定地失败）：连接器收到 INTERNAL，操作已经关闭、已签发的 token 被收回；
// 同一 op_id 的重放被终态拒绝（FAILED_PRECONDITION），不再向 GitHub 签发。
func TestAICredentialAuditWriteFails(t *testing.T) {
	p := credPort(t)
	r, task := reviewTask(t, p)
	_, err := p.s.db.Exec(p.ctx, `CREATE FUNCTION fail_cred_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION '注入的审计写失败'; END $$;
		CREATE TRIGGER fail_cred_audit BEFORE INSERT ON audit_log FOR EACH ROW
		WHEN (NEW.action = 'ai.credential.issue') EXECUTE FUNCTION fail_cred_audit()`)
	mustNil(t, err)
	_, err = p.issue(r, task, read, "op", credDemo)
	wantCode(t, "审计写失败", err, connect.CodeInternal, "")
	p.wantAllRevoked(1)
	_, err = p.issue(r, task, read, "op", credDemo)
	wantCode(t, "审计写失败之后同一 op_id", err, connect.CodeFailedPrecondition, "")
	p.wantAllRevoked(1)
}

// 依赖仓库的解析结果必须仍是声明的仓库，否则拒绝且不签发：改名（Kiln 目录中没有声明的名称，定向查询按旧名称读到改名后的
// 仓库）、陈旧目录项（目录中这个名称的仓库已经转到别的 installation）。
func TestAICredentialDependencyResolution(t *testing.T) {
	t.Run("改名", func(t *testing.T) {
		const lib2ID = 424242
		// GitHub 对改名前的名称返回改名后的仓库（重定向）：替身没有这个行为，在它前面拦下这一个请求
		var upstream atomic.Pointer[url.URL]
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.EqualFold(r.URL.Path, "/repos/"+credLib) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(gh.Repository{
					ID: lib2ID, Name: "lib2", FullName: portOwner + "/lib2", Owner: gh.Account{ID: 9001, Login: portOwner}, Private: true,
					DefaultBranch: "main", CloneURL: "https://github.com/tester/lib2.git",
				})
				return
			}
			httputil.NewSingleHostReverseProxy(upstream.Load()).ServeHTTP(w, r)
		}))
		t.Cleanup(api.Close)
		p := newSplitPort(t, portConfig, func(c *Config) { c.GitHubAPIURL, c.GitHubUploadURL = api.URL, api.URL })
		u, err := url.Parse(p.fake.URL)
		mustNil(t, err)
		upstream.Store(u)
		p.fake.AddRepo(&fakegithub.Repo{ID: lib2ID, Owner: portOwner, Name: "lib2", Dir: p.dir, Private: true})
		r, task := reviewTask(t, p)
		_, err = p.issue(r, task, read, "op", credLib)
		wantCode(t, "改名后的依赖", err, connect.CodeFailedPrecondition, portOwner+"/lib2")
		if toks := p.scopedTokens(); len(toks) != 0 {
			t.Fatalf("不应签发：%+v", toks)
		}
	})
	t.Run("陈旧目录项", func(t *testing.T) {
		p := credPort(t)
		mustNil(t, store.UpsertRepo(p.ctx, p.s.db, store.RepoMeta{
			ID: credLibID, InstallationID: credOtherIns, Owner: portOwner, Name: "lib", Private: true, DefaultBranch: "main",
			CloneURL: "https://github.com/tester/lib.git",
		}))
		r, task := reviewTask(t, p)
		_, err := p.issue(r, task, read, "op", credLib)
		wantCode(t, "陈旧目录项", err, connect.CodeFailedPrecondition, "不在")
		if toks := p.scopedTokens(); len(toks) != 0 {
			t.Fatalf("不应签发：%+v", toks)
		}
	})
}

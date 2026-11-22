package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/repodir"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 执行节点的凭据（PLAN #33.13）：连接器按操作向 Server 要短期凭据，不用执行机器上的凭据。现在只有 git：GitHub App
// installation token，按操作签发、用后吊销（PLAN #33.13）。
//
// 每个操作（连接器准备仓库、推 attempt 分支，Agent 经 kiln-orca task exec 发起的一次 git 传输）由连接器随机生成 op_id，
// 只访问一个仓库：读凭据（contents: read）覆盖任务仓库或任务声明的一个依赖仓库；写凭据（contents: write）只覆盖任务仓库，
// 只签给开发任务。签发与回收的规则在 AI、CI 共用的登记（gitcred）中：同一 op_id 的重试拿回同一枚，关闭是终态，交出前
// 在数据库中做最终检查，attempt 结束的事务提交之后（attemptEnded）吊销它的全部操作。每次签发写一条审计日志（不记 token）。

// GitUsername 是 installation token 在 HTTPS git 中的用户名（GitHub 约定）。
const GitUsername = "x-access-token"

// 凭据的权限对应的 installation token 权限（contents）。
const (
	gitReadPermission  = "read"
	gitWritePermission = "write"
)

// 审计日志的动作：交出一枚 git 凭据（同一 op_id 的重放每次交出各一条），GitHub 签发结果不明（可能有一枚谁也没拿到的
// token）。
const (
	auditGitIssue        = "ai.credential.issue"
	auditGitIssueUnknown = "ai.credential.issue_unknown"
)

// auditTimeout 是写 git 凭据审计日志的时限。审计不随发起请求的客户端取消：客户端断开不是审计失败，不能因此关闭操作
// （同一 op_id 的重放要拿到同一枚）。
const auditTimeout = 10 * time.Second

// GitCredAttempt 返回 AI attempt 在签发登记中的标识。
func GitCredAttempt(attemptID int64) gitcred.Attempt {
	return gitcred.Attempt{Domain: gitcred.DomainAI, ID: attemptID}
}

// gitKey 返回操作在签发登记中的键。
func gitKey(ref *airunnerv2.TaskRef, op string) gitcred.Key {
	return gitcred.Key{Attempt: GitCredAttempt(ref.GetAttemptId()), Epoch: ref.GetEpoch(), Op: op}
}

// validCredentialOp 校验凭据请求的种类、TaskRef 和 op_id。
func validCredentialOp(kind airunnerv2.CredentialKind, ref *airunnerv2.TaskRef, op string) error {
	switch {
	case kind != airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT:
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("不支持的凭据种类 %v", kind))
	case ref == nil:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 TaskRef"))
	}
	return gitcred.ValidOp(op)
}

// IssueCredential 为执行中的 attempt 的一个操作签发短期凭据（PLAN #33.13）。
func (s *Service) IssueCredential(ctx context.Context, req *connect.Request[airunnerv2.IssueCredentialRequest]) (*connect.Response[airunnerv2.IssueCredentialResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validCredentialOp(m.GetKind(), m.GetRef(), m.GetOpId()); err != nil {
		return nil, err
	}
	perm, err := gitPermission(m.GetAccess())
	if err != nil {
		return nil, err
	}
	cred, err := s.c.issueGitCredential(ctx, id, m, perm)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&airunnerv2.IssueCredentialResponse{Credential: cred}), nil
}

// gitPermission 返回凭据权限对应的 contents 权限。
func gitPermission(a airunnerv2.CredentialAccess) (string, error) {
	switch a {
	case airunnerv2.CredentialAccess_CREDENTIAL_ACCESS_READ:
		return gitReadPermission, nil
	case airunnerv2.CredentialAccess_CREDENTIAL_ACCESS_WRITE:
		return gitWritePermission, nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("未知的凭据权限 %v", a))
}

// issueGitCredential 校验 attempt（归属、epoch、状态、租约）和仓库后，经签发登记为操作 m.OpId 签发 m.Repository 一个仓库的
// git 凭据：读凭据的仓库是任务仓库或声明的依赖仓库，写凭据只能是任务仓库、只签给可以推送的任务类型（Policy.GitWrite）。
// 交出前在数据库中复查（liveAttempt）。
func (c *Core) issueGitCredential(ctx context.Context, runnerID int64, m *airunnerv2.IssueCredentialRequest, perm string) (*airunnerv2.Credential, error) {
	if c.gitCred == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有配置 GitHub App，不能签发 git 凭据"))
	}
	ref := m.GetRef()
	_, t, err := c.attemptTask(ctx, c.db, runnerID, ref, false)
	if err != nil {
		return nil, err
	}
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return nil, err
	}
	if perm == gitWritePermission && !typ.Policy().GitWrite {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s 任务只能使用只读的 git 凭据", typeLabel(t.Type)))
	}
	r, err := GetRequest(ctx, c.db, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, c.db, r.RepoID)
	if err != nil {
		return nil, err
	}
	target, err := c.gitTarget(ctx, typ, t, repo, m.GetRepository(), perm)
	if err != nil {
		return nil, err
	}
	key := gitKey(ref, m.GetOpId())
	scope := gitcred.Scope{
		Installation: repo.InstallationID, RepoID: target.ID, Repository: target.FullName(),
		Permissions: map[string]string{"contents": perm},
	}
	tok, err := c.gitCred.Issue(ctx, key, scope, func(ctx context.Context) error { return liveAttempt(ctx, c.db, runnerID, ref) })
	actor := fmt.Sprintf("runner:%d", runnerID)
	detail := map[string]any{
		"runner_id": runnerID, "attempt_id": key.ID, "epoch": key.Epoch, "op": key.Op, "task_id": t.ID, "kind": "git", "access": perm,
		"repositories": []string{scope.Repository},
	}
	if errors.Is(err, gitcred.ErrUnknown) {
		// 可能有一枚谁也没拿到的 token：持久记下（到期时间是估计值），这条审计失败不改变返回码
		detail["expires_at"], detail["expires_at_estimated"] = time.Now().Add(gh.TokenLifetime), true
		if aerr := c.audit(ctx, actor, auditGitIssueUnknown, detail); aerr != nil {
			c.log.Warn("记录 git 凭据签发结果不明的审计日志失败", "attempt", key.ID, "op", key.Op, "err", aerr)
		}
	}
	if err != nil {
		return nil, gitcred.RPCError(err)
	}
	detail["expires_at"], detail["token_fingerprint"] = tok.ExpiresAt, gitcred.Fingerprint(tok.Token)
	if err := c.audit(ctx, actor, auditGitIssue, detail); err != nil {
		// 没有记下的凭据不交出：关闭登记中的这个操作（吊销，不新建记录），连接器这次 git 访问失败，重新执行即是新的操作
		_ = c.gitCred.Discard(ctx, key)
		return nil, fmt.Errorf("记录签发 git 凭据的审计日志：%w", err)
	}
	return &airunnerv2.Credential{
		Kind: airunnerv2.CredentialKind_CREDENTIAL_KIND_GIT, Access: m.GetAccess(), Username: GitUsername, Secret: tok.Token,
		ExpiresAt: tok.ExpiresAt.Unix(), Scopes: []string{scope.Repository},
	}, nil
}

// audit 写一条 git 凭据的审计日志，不随 ctx 取消，以 auditTimeout 为限。
func (c *Core) audit(ctx context.Context, actor, action string, detail map[string]any) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	return store.Audit(actx, c.db, actor, action, detail)
}

// gitTarget 返回操作可以访问的仓库 name：任务仓库，或（只读时）任务声明的依赖仓库之一。仓库名按 GitHub 的规则不区分
// 大小写。不在其中时拒绝（PERMISSION_DENIED）。
//
// 依赖仓库经 dependency（repodir.Lookup）解析：必须与任务仓库同一个 installation，且解析结果现在的全名仍是声明的名称
// （例如定向查询按旧名称读到了改名后的仓库时拒绝），签发用解析出的 ID。与 CI 的 repositories 相同，不为每次签发多发
// GitHub 请求：Kiln 目录中有这个名称时直接用目录项（由已验签的事件和显式的刷新维护），没有时才定向查询 GitHub。
func (c *Core) gitTarget(ctx context.Context, typ TaskType, t *Task, repo *store.Repo, name, perm string) (*store.Repo, error) {
	if strings.EqualFold(name, repo.FullName()) {
		return repo, nil
	}
	if perm == gitWritePermission {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("写凭据只覆盖任务仓库 %s，不签发 %q 的写凭据", repo.FullName(), name))
	}
	for _, dep := range typ.Repositories(t) {
		if !strings.EqualFold(name, dep) {
			continue
		}
		r, err := c.dependency(ctx, repo, dep)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(r.FullName(), dep) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("仓库 %s 现在是 %s（改名或转移），与任务的 requirements.repositories 不一致，不签发 git 凭据", dep, r.FullName()))
		}
		return r, nil
	}
	return nil, connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("仓库 %q 不是任务仓库 %s，也不在任务的 requirements.repositories 中，不签发 git 凭据", name, repo.FullName()))
}

// dependency 查找任务声明的依赖仓库 name：必须与任务仓库 repo 在同一个 installation 下（与 CI 的 repositories 相同，
// repodir.Lookup 只定向读取这一个仓库）。暂时的故障让节点重试（UNAVAILABLE），不在授权范围内时拒绝并说明。
func (c *Core) dependency(ctx context.Context, repo *store.Repo, name string) (*store.Repo, error) {
	dep, err := repodir.Lookup(ctx, c.db, c.gh, repo.InstallationID, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("查找依赖仓库 %s：%w", name, err))
	}
	if dep == nil || dep.InstallationID != repo.InstallationID {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"requirements.repositories 中的 %s 不在 %s 所在的 GitHub App installation 中（不存在、没有安装本 App 或属于别的账号），不签发 git 凭据", name, repo.FullName()))
	}
	return dep, nil
}

// liveAttempt 是交出凭据前的最终检查（gitcred.Check）：attempt 仍属于 runnerID、epoch 是这次领取的、任务仍在执行它且
// 租约未过期（leaseLiveSQL，按数据库时钟），节点未被吊销。
func liveAttempt(ctx context.Context, q store.Querier, runnerID int64, ref *airunnerv2.TaskRef) error {
	var live bool
	var state string
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT `+leaseLiveSQL+` FROM ai_tasks WHERE id=a.task_id), false), r.state
		FROM ai_attempts a JOIN runners r ON r.id=a.runner_id
		WHERE a.runner_id=$1 AND a.id=$2 AND a.epoch=$3`,
		runnerID, ref.GetAttemptId(), ref.GetEpoch()).Scan(&live, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return rpcerr.StaleEpoch()
	case err != nil:
		return err
	case state != store.RunnerActive:
		return rpcerr.RunnerRevoked()
	case !live:
		return rpcerr.StaleEpoch()
	}
	return nil
}

// RevokeCredential 按 op_id 关闭操作并吊销它的凭据（终态，PLAN #33.13）。只校验 attempt 属于这个节点的这次领取（epoch），
// 不要求租约和任务状态：吊销只会收回权限，attempt 结束后连接器仍可关闭剩下的操作。secret 可选：连接器收到过凭据时带上，
// 只用于 Server 丢了登记（重启）之后补吊销。
func (s *Service) RevokeCredential(ctx context.Context, req *connect.Request[airunnerv2.RevokeCredentialRequest]) (*connect.Response[airunnerv2.RevokeCredentialResponse], error) {
	id, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validCredentialOp(m.GetKind(), m.GetRef(), m.GetOpId()); err != nil {
		return nil, err
	}
	c := s.c
	if c.gitCred == nil {
		// 没有配置 GitHub App 时不会签发过凭据
		return connect.NewResponse(&airunnerv2.RevokeCredentialResponse{}), nil
	}
	ref := m.GetRef()
	a, err := attemptOf(ctx, c.db, id, ref)
	if err != nil {
		return nil, err
	}
	var inst int64
	var running bool
	err = c.db.QueryRow(ctx, `SELECT coalesce(repo.installation_id, 0), t.state=$2 AND t.current_attempt_id=$3
		FROM ai_tasks t JOIN ai_requests r ON r.id=t.request_id LEFT JOIN repos repo ON repo.id=r.repo_id WHERE t.id=$1`,
		a.TaskID, TaskRunning, a.ID).Scan(&inst, &running)
	if err != nil {
		return nil, err
	}
	running = running && a.Outcome == nil
	key := gitKey(ref, m.GetOpId())
	// attempt 还在执行时关闭操作（终态，之后同一 op_id 不再签发）；已经结束时只补吊销、不在登记中新建记录：结束的 attempt
	// 不会再签发（签发前的数据库校验），新建的关闭记录在 Server 重启或失效记录清除后不会再被清除（gitcred.Close 的调用约定）
	revoke := c.gitCred.Close
	if !running {
		revoke = c.gitCred.RevokeEnded
	}
	if err := revoke(ctx, key, inst, m.GetSecret()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&airunnerv2.RevokeCredentialResponse{}), nil
}

// attemptEnded 在 attempt 结束的事务提交之后使它在签发登记中失效：吊销它的全部 git 凭据，之后不再签发（完成、取消、超时、
// sweeper 判定租约过期都经过 finishTask，由各自的调用方在提交后调用）。
func (c *Core) attemptEnded(ctx context.Context, attemptID int64) {
	if c.gitCred != nil {
		c.gitCred.Invalidate(ctx, GitCredAttempt(attemptID))
	}
}

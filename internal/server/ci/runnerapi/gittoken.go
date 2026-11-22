package runnerapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 按操作签发的源码凭据（PLAN #8.4）：Runner 为每次网络 git 操作（缓存缺失时的 fetch、
// step 中的一次 git 传输）随机生成 op_id，经 IssueGitToken 拿到这次操作访问的那一个仓库的只读 token，操作结束时
// RevokeGitToken。签发与回收的规则在共用的登记（gitcred）中；attempt 结束的事务提交后由 engine 通知登记失效
// （engine.Deps.AttemptEnded，组装层接线）。

// gitReadPermission 是源码 token 的 contents 权限。
const gitReadPermission = "read"

// 源码 token 的审计日志动作：交出一枚 token（同一 op_id 的重放每次交出各一条），GitHub 签发结果不明（可能有一枚谁也
// 没拿到的 token）。
const (
	auditGitIssue        = "ci.git_token.issue"
	auditGitIssueUnknown = "ci.git_token.issue_unknown"
)

// auditTimeout 是写源码 token 审计日志的时限。审计不随发起请求的客户端取消：客户端断开不是审计失败，不能因此关闭操作
// （同一 op_id 的重放要拿到同一枚）。
const auditTimeout = 10 * time.Second

// GitCredAttempt 返回 CI attempt 在签发登记中的标识（组装层据此在 attempt 结束后调用 gitcred.Registry.Invalidate）。
func GitCredAttempt(attemptID int64) gitcred.Attempt {
	return gitcred.Attempt{Domain: gitcred.DomainCI, ID: attemptID}
}

// gitKey 返回操作在签发登记中的键。
func gitKey(ref *runnerv1.AttemptRef, op string) gitcred.Key {
	return gitcred.Key{Attempt: GitCredAttempt(ref.GetAttemptId()), Epoch: ref.GetEpoch(), Op: op}
}

// validOp 校验请求中的 AttemptRef 和 op_id。
func validOp(ref *runnerv1.AttemptRef, op string) error {
	if ref == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 AttemptRef"))
	}
	return gitcred.ValidOp(op)
}

// leasedAttempt 在数据库中确认 attempt 仍可签发源码 token：属于 runnerID、epoch 是当前值、job 仍在运行、租约未过期（按数据库
// 时钟比较，sweeper 还没处理的过期租约同样拒绝），且 Runner 未被吊销。签发前和交出前（gitcred.Check）各检查一次。
func leasedAttempt(ctx context.Context, q store.Querier, runnerID int64, ref *runnerv1.AttemptRef) error {
	var running, leased, active bool
	err := q.QueryRow(ctx, `SELECT j.state=$4 AND j.epoch=a.epoch, coalesce(j.lease_expires_at > now(), false), r.state=$5
		FROM attempts a JOIN jobs j ON j.id=a.job_id JOIN runners r ON r.id=a.runner_id
		WHERE a.id=$1 AND a.epoch=$2 AND a.runner_id=$3`,
		ref.GetAttemptId(), ref.GetEpoch(), runnerID, store.JobRunning, store.RunnerActive).Scan(&running, &leased, &active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return rpcerr.StaleEpoch()
	case err != nil:
		return err
	case !running:
		return rpcerr.StaleEpoch()
	case !active:
		return rpcerr.RunnerRevoked()
	case !leased:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("attempt 的租约已过期，不签发源码 token"))
	}
	return nil
}

// IssueGitToken 为一次网络 git 操作签发 repository 的只读 token：校验 attempt（归属、epoch、running）、environment 的授权、
// 租约，以及仓库是任务仓库或 job 的 repositories 之一；交出前在数据库中复查 attempt 与 Runner。每次交出写审计日志（不记 token）。
func (s *Service) IssueGitToken(ctx context.Context, req *connect.Request[runnerv1.IssueGitTokenRequest]) (*connect.Response[runnerv1.IssueGitTokenResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validOp(m.Ref, m.OpId); err != nil {
		return nil, err
	}
	if s.gitcred == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有配置 GitHub App，不签发源码 token"))
	}
	c, err := s.credentialJob(ctx, id, m.Ref)
	if err != nil {
		return nil, err
	}
	if err := leasedAttempt(ctx, s.db, id, m.Ref); err != nil {
		return nil, err
	}
	target, err := s.gitTarget(ctx, c, m.Repository)
	if err != nil {
		return nil, err
	}
	key := gitKey(m.Ref, m.OpId)
	scope := gitcred.Scope{
		Installation: c.repo.InstallationID, RepoID: target.ID, Repository: target.FullName(),
		Permissions: map[string]string{"contents": gitReadPermission},
	}
	tok, err := s.gitcred.Issue(ctx, key, scope, func(ctx context.Context) error { return leasedAttempt(ctx, s.db, id, m.Ref) })
	actor := fmt.Sprintf("runner:%d", id)
	detail := map[string]any{
		"runner_id": id, "attempt_id": key.ID, "epoch": key.Epoch, "op": key.Op, "job_id": c.job.ID,
		"repository": scope.Repository, "permissions": scope.Permissions,
	}
	if errors.Is(err, gitcred.ErrUnknown) {
		// 可能有一枚谁也没拿到的 token：持久记下（到期时间是估计值），这条审计失败不改变返回码
		detail["expires_at"], detail["expires_at_estimated"] = time.Now().Add(gh.TokenLifetime), true
		if aerr := s.audit(ctx, actor, auditGitIssueUnknown, detail); aerr != nil {
			s.log.Warn("记录源码 token 签发结果不明的审计日志失败", "attempt", key.ID, "op", key.Op, "err", aerr)
		}
	}
	if err != nil {
		return nil, gitcred.RPCError(err)
	}
	detail["expires_at"], detail["token_fingerprint"] = tok.ExpiresAt, gitcred.Fingerprint(tok.Token)
	if err := s.audit(ctx, actor, auditGitIssue, detail); err != nil {
		// 没有记下的 token 不交出：关闭登记中的这个操作（吊销，不新建记录），Runner 用新的操作重试
		_ = s.gitcred.Discard(ctx, key)
		return nil, fmt.Errorf("记录签发源码 token 的审计日志：%w", err)
	}
	return connect.NewResponse(&runnerv1.IssueGitTokenResponse{Token: tok.Token, ExpiresAt: tok.ExpiresAt.Unix()}), nil
}

// audit 写一条源码 token 的审计日志，不随 ctx 取消，以 auditTimeout 为限。
func (s *Service) audit(ctx context.Context, actor, action string, detail map[string]any) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	return store.Audit(actx, s.db, actor, action, detail)
}

// gitTarget 返回 job 可以访问的仓库 name：任务仓库（run 绑定的仓库 ID），或 job 的 repositories 中声明的仓库。仓库名按
// GitHub 的规则不区分大小写。
//
// 依赖仓库经 repodir.Lookup 解析：必须与任务仓库同一个 installation，且解析结果现在的全名仍是声明的名称（例如定向查询
// 按旧名称读到了改名后的仓库时拒绝），签发用解析出的 ID。不为每次签发多发 GitHub 请求：Kiln 目录中有这个名称时直接用
// 目录项，目录由已验签的事件（仓库改名、删除、转移，installation 的仓库增减）和显式的"刷新仓库"维护，没有时才定向查询
// GitHub。事件丢失时目录项可能陈旧（例如改名后又建了同名的新仓库，而改名事件没有处理），这时按陈旧的 ID 签发，直到下一个
// 事件或刷新——这是按需目录的边界。
func (s *Service) gitTarget(ctx context.Context, c *credentialJob, name string) (*store.Repo, error) {
	if strings.EqualFold(name, c.repo.FullName()) {
		return c.repo, nil
	}
	for _, dep := range c.job.Spec.Repositories {
		if !strings.EqualFold(name, dep) {
			continue
		}
		r, err := s.dependency(ctx, c.repo, dep)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(r.FullName(), dep) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("仓库 %s 现在是 %s（改名或转移），与 job 的 repositories 不一致，不签发源码 token", dep, r.FullName()))
		}
		return r, nil
	}
	return nil, connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("仓库 %q 不是任务仓库 %s，也不在 job 的 repositories 中，不签发源码 token", name, c.repo.FullName()))
}

// RevokeGitToken 按 op_id 关闭操作并吊销它的 token（终态）。只要求 attempt 属于这个 Runner 且 epoch 一致，不要求租约和 job
// 状态：吊销只会收回权限，job 结束后 Runner 仍可吊销剩下的操作（客户端带来的 token 和登记中的 token）。
func (s *Service) RevokeGitToken(ctx context.Context, req *connect.Request[runnerv1.RevokeGitTokenRequest]) (*connect.Response[runnerv1.RevokeGitTokenResponse], error) {
	id, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validOp(m.Ref, m.OpId); err != nil {
		return nil, err
	}
	var owner, inst int64
	var running bool
	err = s.db.QueryRow(ctx, `SELECT coalesce(a.runner_id, 0), coalesce(r.installation_id, 0),
			a.outcome IS NULL AND j.state=$3 AND j.epoch=a.epoch
		FROM attempts a JOIN jobs j ON j.id=a.job_id LEFT JOIN runs ON runs.id=j.run_id LEFT JOIN repos r ON r.id=runs.repo_id
		WHERE a.id=$1 AND a.epoch=$2`, m.Ref.GetAttemptId(), m.Ref.GetEpoch(), store.JobRunning).Scan(&owner, &inst, &running)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != id) {
		return nil, rpcerr.StaleEpoch()
	}
	if err != nil {
		return nil, err
	}
	if s.gitcred == nil {
		// 没有配置 GitHub App 时不会签发过 token
		return connect.NewResponse(&runnerv1.RevokeGitTokenResponse{}), nil
	}
	key := gitKey(m.Ref, m.OpId)
	// attempt 还在执行时关闭操作（终态，之后同一 op_id 不再签发）；已经结束时只补吊销、不在登记中新建记录：结束的 attempt
	// 不会再签发（签发前的数据库校验），新建的关闭记录在 Server 重启或失效记录清除后不会再被清除（gitcred.Close 的调用约定）。
	// 判定为执行中与 Close 之间没有锁：这里假定 RPC 的期限远短于登记的保留期（gitcred.DefaultRetention），判定之后才结束的
	// attempt 由 Invalidate 收尾，不会出现保留期清除之后才 Close、留下关闭记录的情况。
	revoke := s.gitcred.Close
	if !running {
		revoke = s.gitcred.RevokeEnded
	}
	if err := revoke(ctx, key, inst, m.Token); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&runnerv1.RevokeGitTokenResponse{}), nil
}

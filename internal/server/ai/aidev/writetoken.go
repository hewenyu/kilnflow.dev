package aidev

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/gitcred"
)

// 交付的写凭据（PLAN #26.10）：推进正式分支、写 PR 各自签发一枚只覆盖
// 任务仓库的窄 token，用它构造一次性的平台接口执行这组写操作，结束后吊销；推进中的读（分支、diff、按分支找 PR）继续用
// 共用客户端（pass.f）。token 不离开 Server、没有客户端重试，所以不经过签发登记（gitcred）。签发失败或结果不明时本轮推进
// 返回错误，下一轮按最新状态重新判断（新的 token），不在同一组写操作中重试。

// 交付的写操作所需的权限（installation token 的权限名）。
const (
	permContents     = "contents"
	permPullRequests = "pull_requests"
	permWrite        = "write"
)

// writeRevokeTimeout 是吊销交付写 token 的时限：吊销不随本轮处理取消。
const writeRevokeTimeout = 30 * time.Second

// writer 是一组交付写操作共用的 token：第一次写时签发（没有写操作就不签），release 时吊销。
type writer struct {
	p    *pass
	perm string
	// f 是用这枚 token 的平台接口（签发之后），tok 是这枚 token。
	f   forge.Repo
	tok *gh.AccessToken
}

// writer 返回一组需要 perm 写权限（只覆盖任务仓库）的写操作。调用方在这组写操作结束后 release。
func (p *pass) writer(perm string) *writer { return &writer{p: p, perm: perm} }

// do 用这组写操作的 token 执行 f；第一次调用时签发。
func (w *writer) do(ctx context.Context, f func(forge.Repo) error) error {
	if w.f == nil {
		if err := w.issue(ctx); err != nil {
			return err
		}
	}
	return f(w.f)
}

// issue 签发任务仓库 w.perm 写权限的 token。
func (w *writer) issue(ctx context.Context) error {
	s, repo := w.p.s, w.p.repo
	if s.gh == nil || s.tokenForge == nil {
		return errors.New("没有配置 GitHub App，不能签发交付的写凭据")
	}
	perms := map[string]string{w.perm: permWrite}
	tok, err := s.gh.CreateToken(ctx, repo.InstallationID, []int64{repo.ID}, perms)
	if err != nil {
		if !gh.NotApplied(err) {
			s.log.Warn("AI 开发：签发交付的写凭据结果不明，本轮不重试；可能有一枚谁也没拿到的 token，到期自然失效",
				"repo", repo.FullName(), "permissions", perms, "expires_at", time.Now().Add(gh.TokenLifetime), "expires_at_estimated", true, "err", err)
		}
		return fmt.Errorf("签发 %s 的 %s 写凭据：%w", repo.FullName(), w.perm, err)
	}
	// 先交给 w，之后（日志、构造平台接口）即使 panic，release 也能吊销它。
	w.tok = tok
	s.log.Info("AI 开发：签发交付的写凭据", "repo", repo.FullName(), "permissions", perms,
		"token_fingerprint", gitcred.Fingerprint(tok.Token), "expires_at", tok.ExpiresAt)
	w.f = s.tokenForge(repo, tok.Token)
	return nil
}

// release 吊销这组写操作的 token（没有签发时什么也不做）。不随 ctx 取消，以 writeRevokeTimeout 为限；失败只记 warn，
// token 到期自然失效。
func (w *writer) release(ctx context.Context) {
	if w.tok == nil {
		return
	}
	s, repo := w.p.s, w.p.repo
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeRevokeTimeout)
	defer cancel()
	attrs := []any{
		"repo", repo.FullName(), "permissions",
		map[string]string{w.perm: permWrite},
		"token_fingerprint", gitcred.Fingerprint(w.tok.Token), "expires_at", w.tok.ExpiresAt,
	}
	if err := s.gh.RevokeToken(rctx, repo.InstallationID, w.tok.Token); err != nil {
		s.log.Warn("AI 开发：吊销交付的写凭据失败，不重试，到期自然失效", append(attrs, "err", err)...)
	} else {
		s.log.Info("AI 开发：吊销交付的写凭据", attrs...)
	}
	w.tok, w.f = nil, nil
}

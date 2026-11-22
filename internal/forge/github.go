package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// gitHubRepo 用 GitHub App 的 installation token 操作一个仓库。
type gitHubRepo struct {
	inst *gh.Inst
	name string
}

// GitHub 返回 GitHub 上仓库 fullName（owner/name，属于 installation）的 Repo。
func GitHub(c *gh.Client, installation int64, fullName string) Repo {
	return &gitHubRepo{inst: c.Inst(installation), name: fullName}
}

// GitHubWithToken 同 GitHub，但全部请求用给定的 installation token（例如按操作签发、只覆盖这个仓库和所需权限的 token），
// 不用共用客户端缓存的完整权限 token。
func GitHubWithToken(c *gh.Client, installation int64, token, fullName string) Repo {
	return &gitHubRepo{inst: c.InstWithToken(installation, token), name: fullName}
}

func (r *gitHubRepo) GetPull(ctx context.Context, number int) (*Pull, error) {
	p, err := r.inst.GetPull(ctx, r.name, number)
	if err != nil {
		return nil, err
	}
	out := &Pull{
		Number: p.Number, Title: p.Title, State: p.State, Draft: p.Draft, HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref,
		Fork: gh.IsFork(p.Head, p.Base), Author: p.User.Login, AuthorBot: p.User.Type == "Bot",
		Mergeable: p.Mergeable, MergeableState: p.MergeableState,
	}
	if p.Merged {
		out.State, out.MergeSHA = PullMerged, p.MergeCommitSHA
		if p.MergedBy != nil {
			out.MergedBy, out.MergedByBot = p.MergedBy.Login, p.MergedBy.Type == "Bot"
		}
	}
	for _, l := range p.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out, nil
}

func (r *gitHubRepo) Behind(ctx context.Context, base, head string) (int, error) {
	c, err := r.inst.Compare(ctx, r.name, base, head)
	if err != nil {
		return 0, err
	}
	return c.BehindBy, nil
}

func (r *gitHubRepo) CommitParents(ctx context.Context, sha string) ([]string, error) {
	c, err := r.inst.GetCommit(ctx, r.name, sha)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		out = append(out, p.SHA)
	}
	return out, nil
}

// platformError 返回 GitHub 对请求的明确响应（据此区分拒绝和暂时失败）；没有出错、网络错误、限流时返回 nil。
// 限流（*gh.RateLimitError 可以展开成 403 的 *APIError）是暂时失败，不能当成没有权限。
func platformError(err error) *gh.APIError {
	if _, limited := gh.RateLimited(err); limited {
		return nil
	}
	ae, _ := errors.AsType[*gh.APIError](err)
	return ae
}

func (r *gitHubRepo) UpdateBranch(ctx context.Context, number int, expectedHead string) error {
	err := r.inst.UpdatePullBranch(ctx, r.name, number, expectedHead)
	ae := platformError(err)
	if ae == nil {
		return err
	}
	switch ae.Status {
	case http.StatusUnprocessableEntity:
		// 422：expected_head_sha 不符，或者有冲突等原因不能更新
		msg := strings.ToLower(ae.Message)
		switch {
		case strings.Contains(msg, "expected head sha"):
			return ErrHeadChanged
		case strings.Contains(msg, "merge conflict"):
			return fmt.Errorf("%w：%s", ErrMergeConflict, ae.Message)
		}
		return &RejectedError{Message: ae.Message}
	case http.StatusForbidden:
		// 没有权限（例如不能写 PR 分支）
		return &RejectedError{Message: ae.Message}
	}
	return err
}

func (r *gitHubRepo) Merge(ctx context.Context, number int, method, expectedHead string) (string, error) {
	res, err := r.inst.MergePull(ctx, r.name, number, expectedHead, method)
	if err == nil {
		return res.SHA, nil
	}
	ae := platformError(err)
	if ae == nil {
		return "", err
	}
	switch {
	case ae.Status == http.StatusConflict:
		// 409：head 已经不是 sha
		return "", ErrHeadChanged
	case ae.Status == http.StatusMethodNotAllowed && strings.Contains(strings.ToLower(ae.Message), "base branch was modified"):
		// 目标分支刚好在合并时前进了：暂时失败，下一轮重新判断
		return "", err
	case ae.Status == http.StatusMethodNotAllowed, ae.Status == http.StatusUnprocessableEntity, ae.Status == http.StatusForbidden:
		// 不能合并（冲突、方式不允许、没有权限等）
		return "", &RejectedError{Message: ae.Message}
	}
	return "", err
}

func (r *gitHubRepo) AddLabel(ctx context.Context, number int, label string) error {
	return r.inst.AddLabels(ctx, r.name, number, label)
}

func (r *gitHubRepo) RemoveLabel(ctx context.Context, number int, label string) error {
	if err := r.inst.RemoveLabel(ctx, r.name, number, label); err != nil && !gh.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *gitHubRepo) Comment(ctx context.Context, number int, body string) error {
	return rejectForbidden(r.inst.CreateIssueComment(ctx, r.name, number, body))
}

func (r *gitHubRepo) ClosingIssues(ctx context.Context, number int) ([]Issue, error) {
	refs, err := r.inst.ListClosingIssues(ctx, r.name, number)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	var out []Issue
	for _, ref := range refs {
		// 只处理同一仓库的 issue：其他仓库（包括公开的发布仓库）的 issue 不由这个 PR 关闭
		if !strings.EqualFold(ref.Repo, r.name) {
			continue
		}
		out = append(out, Issue{Number: ref.Number, Open: ref.Open})
	}
	return out, nil
}

func (r *gitHubRepo) CloseIssue(ctx context.Context, number int) error {
	return rejectForbidden(r.inst.CloseIssue(ctx, r.name, number, gh.IssueStateReasonCompleted))
}

func (r *gitHubRepo) IssueOpen(ctx context.Context, number int) (bool, error) {
	open, err := r.inst.IssueOpen(ctx, r.name, number)
	return open, rejectForbidden(err)
}

func (r *gitHubRepo) Tags(ctx context.Context, prefix string, keep func(name string) bool) ([]Tag, error) {
	refs, err := r.inst.MatchingRefs(ctx, r.name, "tags/"+prefix)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	var out []Tag
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref.Ref, gitref.TagPrefix)
		if !ok || !strings.HasPrefix(name, prefix) || !keep(name) {
			continue
		}
		commit, _, err := r.inst.PeelObject(ctx, r.name, ref.Object)
		if err != nil {
			return nil, fmt.Errorf("解开 tag %s：%w", name, err)
		}
		out = append(out, Tag{Name: name, Commit: commit})
	}
	return out, nil
}

func (r *gitHubRepo) TagCommit(ctx context.Context, name string) (string, error) {
	commit, _, err := r.inst.ResolveTag(ctx, r.name, name)
	if gh.IsNotFound(err) {
		return "", nil
	}
	return commit, rejectForbidden(err)
}

func (r *gitHubRepo) CreateTag(ctx context.Context, name, sha string) error {
	err := r.inst.CreateRef(ctx, r.name, gitref.TagPrefix+name, sha)
	if ae := platformError(err); ae != nil && ae.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(ae.Message), "already exists") {
		return ErrTagExists
	}
	return rejectForbidden(err)
}

// Uncertain 判断写操作的错误是否表示"结果不明"：请求可能已经生效（网络错误、超时、5xx、ctx 取消）。
// 平台明确的回答（4xx，包括 ErrHeadChanged、ErrTagExists、ErrMergeConflict、*RejectedError）和限流（请求没有发出，或者被拒绝）
// 都不算：这些请求没有生效，可以照常重试（PLAN #29.8）。
func Uncertain(err error) bool {
	if err == nil || IsRejected(err) || errors.Is(err, ErrTagExists) || errors.Is(err, ErrMergeConflict) || RateLimited(err) {
		return false
	}
	if ge, ok := errors.AsType[*gh.GraphQLError](err); ok && ge != nil {
		return false
	}
	if ae := platformError(err); ae != nil && ae.Status < http.StatusInternalServerError {
		return false
	}
	return true
}

// RateLimited 判断错误是否是平台的限流：暂时失败，等恢复后重试，不算失败次数。
func RateLimited(err error) bool {
	_, ok := gh.RateLimited(err)
	return ok
}

// IsAuthError 判断错误是否表示仓库级的鉴权/权限失败（401、非限流的 403）：installation 被撤销、App 的权限被收回、
// token 失效等。这种情况下继续请求平台没有意义，整轮处理交给 outbox 稍后重试。单个 PR 的 422/404（PR 不存在、不能合并）
// 不算：它们只影响那一个 PR，绝不能当成整个仓库不可访问。
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	// 限流也可能是 403，但它是暂时失败、不是鉴权失败
	if _, limited := gh.RateLimited(err); limited {
		return false
	}
	ae, ok := errors.AsType[*gh.APIError](err)
	if !ok {
		return false
	}
	return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden
}

// rejectForbidden 把没有权限（非限流的 403、GraphQL 的 FORBIDDEN）、issue 不存在（404）和 issue 被禁用（410）
// 转成 *RejectedError，其他错误原样返回。
func rejectForbidden(err error) error {
	if ge, ok := errors.AsType[*gh.GraphQLError](err); ok && ge.Type == gh.GraphQLForbidden {
		return &RejectedError{Message: ge.Message, Cause: err}
	}
	ae := platformError(err)
	if ae == nil {
		return err
	}
	switch ae.Status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		// 保留原始错误：非限流的 403 是仓库级的鉴权失败，IsAuthError 要能从 RejectedError 里认出来（Comment 等写操作）
		return &RejectedError{Message: ae.Message, Cause: err}
	}
	return err
}

func (r *gitHubRepo) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	data, err := r.inst.GetFileContent(ctx, r.name, path, ref)
	if gh.IsNotFound(err) {
		return nil, nil
	}
	return data, err
}

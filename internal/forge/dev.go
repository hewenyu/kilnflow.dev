package forge

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// AI 开发（PLAN #26.11）用到的平台操作：读取 Issue、移动和删除分支、按 head 分支找 PR、创建 PR。
// 写操作都是幂等或带条件的（PLAN #26.12）：创建分支已存在、快进不是快进、同一个 head 已有打开的 PR 都返回明确的错误，
// 调用方据此重读状态，不会建出第二个。

// IssueContent 是一个 Issue 的当前内容。
type IssueContent struct {
	Number int
	Title  string
	Body   string
	Open   bool
	Labels []string
	URL    string
	// UpdatedAt 是平台上 Issue 最近一次变化的时间（任务池据此丢弃乱序到达的旧内容）。
	UpdatedAt time.Time
}

// HasLabel 判断 Issue 是否有某个 label。
func (i *IssueContent) HasLabel(name string) bool { return slices.Contains(i.Labels, name) }

// ErrBranchExists 表示要创建的分支已经存在（不会移动已有的分支）。
var ErrBranchExists = errors.New("分支已经存在")

// ErrNotFastForward 表示分支不能快进到目标 commit（分支上有目标 commit 之外的提交，分支不变）。
var ErrNotFastForward = errors.New("不是快进")

// ErrPullExists 表示同一个 head 分支已经有打开的 PR。
var ErrPullExists = errors.New("同一个分支已经有打开的 PR")

func (r *gitHubRepo) ListIssues(ctx context.Context, label string, limit int) ([]IssueContent, bool, error) {
	list, complete, err := r.inst.ListOpenIssuesByLabel(ctx, r.name, label, limit)
	if err != nil {
		return nil, false, rejectForbidden(err)
	}
	out := make([]IssueContent, 0, len(list))
	for i := range list {
		out = append(out, issueContent(&list[i]))
	}
	return out, complete, nil
}

func issueContent(is *gh.Issue) IssueContent {
	return IssueContent{
		Number: is.Number, Title: is.Title, Body: is.Body, Open: is.State == "open", Labels: is.LabelNames(), URL: is.HTMLURL,
		UpdatedAt: is.UpdatedAt,
	}
}

func (r *gitHubRepo) GetIssue(ctx context.Context, number int) (*IssueContent, error) {
	is, err := r.inst.GetIssue(ctx, r.name, number)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	if is.PullRequest != nil {
		return nil, &RejectedError{Message: "这是 PR，不是 Issue"}
	}
	out := issueContent(is)
	return &out, nil
}

func (r *gitHubRepo) BranchHead(ctx context.Context, branch string) (string, error) {
	ref, err := r.inst.GetRef(ctx, r.name, "heads/"+branch)
	if gh.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", rejectForbidden(err)
	}
	return ref.Object.SHA, nil
}

// unprocessable 判断错误是否是 422 并且信息包含 text（不区分大小写）。
func unprocessable(err error, text string) bool {
	ae := platformError(err)
	return ae != nil && ae.Status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(ae.Message), text)
}

func (r *gitHubRepo) CreateBranch(ctx context.Context, branch, sha string) error {
	err := r.inst.CreateRef(ctx, r.name, gitref.BranchPrefix+branch, sha)
	if unprocessable(err, "already exists") {
		return ErrBranchExists
	}
	return rejectForbidden(err)
}

func (r *gitHubRepo) FastForward(ctx context.Context, branch, sha string) error {
	err := r.inst.UpdateRef(ctx, r.name, "heads/"+branch, sha, false)
	if unprocessable(err, "not a fast forward") {
		return ErrNotFastForward
	}
	return rejectForbidden(err)
}

func (r *gitHubRepo) DeleteBranch(ctx context.Context, branch string) error {
	err := r.inst.DeleteRef(ctx, r.name, "heads/"+branch)
	if gh.IsNotFound(err) || unprocessable(err, "does not exist") {
		return nil
	}
	return rejectForbidden(err)
}

// Diff 是 head 相对 base 的改动。
type Diff struct {
	Ahead int
	// Files 是改动的文件，FilesComplete 为 false 表示列表被截断。
	Files         []string
	FilesComplete bool
	// Messages 是多出的提交的说明，MessagesComplete 为 false 表示只有一部分。
	Messages         []string
	MessagesComplete bool
}

func (r *gitHubRepo) Diff(ctx context.Context, base, head string) (*Diff, error) {
	c, err := r.inst.Compare(ctx, r.name, base, head)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	if c.AheadBy == 0 {
		return &Diff{FilesComplete: true, MessagesComplete: true}, nil
	}
	d, err := r.inst.CompareDetail(ctx, r.name, base, head)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	return &Diff{Ahead: c.AheadBy, Files: d.Files, FilesComplete: d.FilesComplete, Messages: d.Messages, MessagesComplete: d.MessagesComplete}, nil
}

func (r *gitHubRepo) PullsByHead(ctx context.Context, branch string) ([]Pull, error) {
	owner, _, _ := strings.Cut(r.name, "/")
	list, err := r.inst.PullsByHead(ctx, r.name, owner, branch)
	if err != nil {
		return nil, rejectForbidden(err)
	}
	out := make([]Pull, 0, len(list))
	for _, p := range list {
		// head 参数按 owner:branch 过滤，再核对一次（fork 的同名分支不算）
		if p.Head.Ref != branch || gh.IsFork(p.Head, p.Base) {
			continue
		}
		state, merge := p.State, ""
		if p.MergedAt != nil {
			state, merge = PullMerged, p.MergeCommitSHA
		}
		out = append(out, Pull{
			Number: p.Number, State: state, Draft: p.Draft, HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Author: p.User.Login,
			AuthorBot: p.User.Type == "Bot", MergeSHA: merge,
		})
	}
	// GitHub 默认按创建时间倒序返回；接口约定按编号升序（调用方把最后一个当作最新的）
	slices.SortFunc(out, func(a, b Pull) int { return cmp.Compare(a.Number, b.Number) })
	return out, nil
}

func (r *gitHubRepo) CreatePull(ctx context.Context, title, head, base, body string) (int, error) {
	p, err := r.inst.CreatePull(ctx, r.name, title, head, base, body)
	if unprocessable(err, "already exists") {
		return 0, ErrPullExists
	}
	if err != nil {
		if ae := platformError(err); ae != nil && ae.Status == http.StatusUnprocessableEntity {
			return 0, &RejectedError{Message: ae.Message}
		}
		return 0, rejectForbidden(err)
	}
	return p.Number, nil
}

func (r *gitHubRepo) EditPullBody(ctx context.Context, number int, body string) error {
	return rejectForbidden(r.inst.EditPullBody(ctx, r.name, number, body))
}

// CreateIssue 创建一个 issue，返回编号；被平台拒绝（没有 Issues 权限等）时返回 *RejectedError。
func (r *gitHubRepo) CreateIssue(ctx context.Context, title, body string, labels []string) (int, error) {
	is, err := r.inst.CreateIssue(ctx, r.name, title, body, labels)
	if err != nil {
		return 0, rejectForbidden(err)
	}
	return is.Number, nil
}

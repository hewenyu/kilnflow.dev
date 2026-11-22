// Package forge 是代码托管平台的接口（PLAN #28.6）：合并队列等需要读写 PR 的功能只依赖这里的接口，
// 不直接使用某个平台的客户端。现在只有 GitHub 一个实现（github.go）；以后接其他平台（如 Gitea）时实现同样的接口。
package forge

import (
	"context"
	"errors"
	"slices"
)

// Repo 是一个仓库上的 PR 操作。返回的错误中，ErrHeadChanged、*RejectedError 表示平台明确拒绝了这次操作（重试没有意义）；
// ErrMergeConflict 是平台对更新分支的明确拒绝中单独区分的一种（调用方必须显式处理）；其他错误按暂时失败处理（稍后重试）。
type Repo interface {
	// GetPull 读取一个 PR。
	GetPull(ctx context.Context, number int) (*Pull, error)
	// Behind 返回 head 落后于 base 的 commit 数（base 上有、head 中没有的）。
	Behind(ctx context.Context, base, head string) (int, error)
	// CommitParents 返回 commit 的父提交（按顺序）。
	CommitParents(ctx context.Context, sha string) ([]string, error)
	// UpdateBranch 把目标分支合并进 PR 分支。expectedHead 是期望的 PR head，不同时返回 ErrHeadChanged；
	// 平台可能异步完成，新的 head 由之后的事件带来。平台回答合并冲突时返回 ErrMergeConflict（还要读 PR 的 mergeability
	// 确认，PLAN #28.3）；其他原因被拒时返回 *RejectedError。
	UpdateBranch(ctx context.Context, number int, expectedHead string) error
	// Merge 合并 PR，返回合并提交。expectedHead 与 PR 当前的 head 不同时返回 ErrHeadChanged；被拒时返回 *RejectedError。
	Merge(ctx context.Context, number int, method, expectedHead string) (string, error)
	// AddLabel 给 PR 加 label。
	AddLabel(ctx context.Context, number int, label string) error
	// RemoveLabel 去掉 PR 的 label；PR 没有这个 label 时不算错误。
	RemoveLabel(ctx context.Context, number int, label string) error
	// Comment 在 PR 或 issue 上发表评论。没有权限、issue 被锁定等原因被拒时返回 *RejectedError。
	Comment(ctx context.Context, number int, body string) error
	// ClosingIssues 返回 PR 合并时应关闭的 issue（PR 描述中用关键字关联的），只含同一仓库的；其他仓库的 issue 不返回。
	// 没有权限读取时返回 *RejectedError。
	ClosingIssues(ctx context.Context, number int) ([]Issue, error)
	// CloseIssue 以"已完成"关闭 issue；issue 已经关闭时不算错误。没有权限等原因被拒时返回 *RejectedError。
	CloseIssue(ctx context.Context, number int) error
	// IssueOpen 读取 issue 当前是否打开。issue 不存在、没有权限时返回 *RejectedError。
	IssueOpen(ctx context.Context, number int) (bool, error)
	// ReadFile 读取 ref 下的文件；文件不存在时返回 nil, nil。
	ReadFile(ctx context.Context, ref, path string) ([]byte, error)
	// Tags 返回名字以 prefix 开头、并且 keep 返回真的 tag 和它们指向的 commit（annotated tag 解开到 commit）。
	// keep 在解开之前调用，用来少发请求。
	Tags(ctx context.Context, prefix string, keep func(name string) bool) ([]Tag, error)
	// TagCommit 返回 tag 指向的 commit；tag 不存在时返回空。
	TagCommit(ctx context.Context, name string) (string, error)
	// CreateTag 创建指向 commit sha 的轻量 tag。tag 已经存在时返回 ErrTagExists（不会移动已有的 tag，所以可以放心重试）；
	// 没有权限等原因被拒时返回 *RejectedError。
	CreateTag(ctx context.Context, name, sha string) error

	// 以下是 AI 开发（PLAN #26.11，dev.go）用到的操作。

	// ListIssues 返回带 label 的打开的 Issue（不含 PR），按创建时间从早到晚，逐页读取，最多读 limit 条（含 PR）；
	// complete 为假表示还有没有读到的。
	ListIssues(ctx context.Context, label string, limit int) (list []IssueContent, complete bool, err error)
	// GetIssue 读取一个 Issue；不存在、是 PR、没有权限时返回 *RejectedError。
	GetIssue(ctx context.Context, number int) (*IssueContent, error)
	// BranchHead 返回分支指向的 commit；分支不存在时返回空。
	BranchHead(ctx context.Context, branch string) (string, error)
	// CreateBranch 创建指向 sha 的分支；已经存在时返回 ErrBranchExists（不移动）。
	CreateBranch(ctx context.Context, branch, sha string) error
	// FastForward 把分支快进到 sha；不是快进时返回 ErrNotFastForward（分支不变）。
	FastForward(ctx context.Context, branch, sha string) error
	// DeleteBranch 删除分支；不存在时不算错误。
	DeleteBranch(ctx context.Context, branch string) error
	// Diff 返回 head 相对 base 多出的提交数、改动的文件和提交说明（没有多出的提交时只有 Ahead）。
	Diff(ctx context.Context, base, head string) (*Diff, error)
	// PullsByHead 返回 head 分支为 branch（同一仓库）的 PR，包括已关闭、已合并的，按编号升序。
	PullsByHead(ctx context.Context, branch string) ([]Pull, error)
	// CreatePull 创建 PR，返回编号。同一个 head 已经有打开的 PR 时返回 ErrPullExists；被拒时返回 *RejectedError。
	CreatePull(ctx context.Context, title, head, base, body string) (int, error)
	// EditPullBody 修改 PR 的描述。
	EditPullBody(ctx context.Context, number int, body string) error
	// CreateIssue 创建一个 issue，返回编号；被平台拒绝（没有 Issues 权限等）时返回 *RejectedError。
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, error)
}

// Tag 是一个 tag 和它指向的 commit。
type Tag struct {
	Name   string
	Commit string
}

// ErrTagExists 表示要创建的 tag 已经存在。
var ErrTagExists = errors.New("tag 已经存在")

// PR 的状态（Pull.State）。
const (
	PullOpen   = "open"
	PullClosed = "closed"
	PullMerged = "merged"
)

// Pull 是 PR 的当前状态。
type Pull struct {
	Number int
	Title  string
	// State 是 PullOpen、PullClosed 或 PullMerged。
	State   string
	Draft   bool
	HeadSHA string
	// HeadRef 是来源分支（同一仓库时）。
	HeadRef string
	BaseRef string
	Labels  []string
	// Fork 表示 PR 来自另一个仓库。
	Fork bool
	// MergeSHA 是合并提交（State 为 PullMerged 时）。
	MergeSHA string
	// Author 是 PR 作者的 login；MergedBy 是合并者的 login（State 为 PullMerged 时，可能为空）。
	Author, MergedBy string
	// AuthorBot 表示 PR 作者是机器人（GitHub 账号类型为 Bot，例如 Renovate）。
	AuthorBot bool
	// MergedByBot 表示合并者是机器人（GitHub 账号类型为 Bot）。合并队列据此判断这次合并是自己（App 的 bot 账号）做的
	// 还是别人做的：不能只看 login 的 [bot] 后缀，外部机器人的 login 不一定带后缀。
	MergedByBot bool
	// Mergeable 是平台计算的 PR 能否合并（GitHub 的 mergeable）：还没有计算完成时为 nil。MergeableState 是平台给出的
	// 状态（GitHub 的 mergeable_state，例如 dirty 表示冲突、unknown 表示还没有计算）。
	Mergeable      *bool
	MergeableState string
}

// MergeableDirty 是 Pull.MergeableState 表示与目标分支冲突的取值。
const MergeableDirty = "dirty"

// Conflicted 判断平台确认 PR 与目标分支冲突（mergeable=false 且 mergeable_state=dirty）。还没有计算、可以合并、
// 因为别的原因不能合并（例如 blocked）都不算。
func (p *Pull) Conflicted() bool {
	return p.Mergeable != nil && !*p.Mergeable && p.MergeableState == MergeableDirty
}

// HasLabel 判断 PR 是否有某个 label。
func (p *Pull) HasLabel(name string) bool {
	return slices.Contains(p.Labels, name)
}

// Issue 是 PR 关联的一个 issue 的当前状态。
type Issue struct {
	Number int
	// Open 表示 issue 还没有关闭。
	Open bool
}

// ErrHeadChanged 表示 PR 的 head 已经不是期望的 commit。
var ErrHeadChanged = errors.New("PR 的 head 已经变化")

// ErrMergeConflict 表示平台因为合并冲突拒绝更新 PR 分支。它不满足 IsRejected：冲突要按 PR 的归属处理（PLAN #28.3），
// 调用方必须显式判断，不能和其他拒绝一起出队。
var ErrMergeConflict = errors.New("PR 与目标分支有合并冲突")

// RejectedError 表示平台拒绝了操作（冲突、不能合并、没有权限等），Message 是平台返回的原因。Cause 是平台的原始错误
// （例如 403 的 *gh.APIError）：IsAuthError 据此认出仓库级的鉴权失败，不能因为转成了 RejectedError 就当成单 PR 的拒绝。
type RejectedError struct {
	Message string
	Cause   error
}

func (e *RejectedError) Error() string { return "平台拒绝：" + e.Message }

// Unwrap 返回平台的原始错误，让 errors.Is / errors.As 能看到它。
func (e *RejectedError) Unwrap() error { return e.Cause }

// IsRejected 判断错误是否表示平台明确拒绝了操作（*RejectedError 或 ErrHeadChanged）。
func IsRejected(err error) bool {
	_, ok := errors.AsType[*RejectedError](err)
	return ok || errors.Is(err, ErrHeadChanged)
}

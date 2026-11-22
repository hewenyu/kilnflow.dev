package github

import (
	"context"
	"fmt"
	"net/http"
)

// 合并队列（PLAN #28）用到的 PR 接口：读取 PR、比较分支、更新 PR 分支、合并，以及 PR 的 label 和评论。

// Label 是 issue / PR 的 label。
type Label struct {
	Name string `json:"name"`
}

// Pull 是单个 PR 的详细信息。
type Pull struct {
	PullRequest
	Title          string   `json:"title"`
	Merged         bool     `json:"merged"`
	MergeCommitSHA string   `json:"merge_commit_sha"`
	Labels         []Label  `json:"labels"`
	MergedBy       *Account `json:"merged_by"`
	// Mergeable 在 GitHub 还没有计算完成时为 null；MergeableState 为 dirty 表示冲突、unknown 表示还没有计算。
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
}

// GetPull 读取一个 PR。
func (i *Inst) GetPull(ctx context.Context, repo string, number int) (*Pull, error) {
	return call[Pull](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil)
}

// CompareStatus 是 base...head 的比较结果。
type CompareStatus struct {
	// Status 是 ahead、behind、diverged 或 identical。
	Status   string `json:"status"`
	AheadBy  int    `json:"ahead_by"`
	BehindBy int    `json:"behind_by"`
}

// Compare 比较 base...head（只取提交数，不需要文件列表）。
func (i *Inst) Compare(ctx context.Context, repo, base, head string) (*CompareStatus, error) {
	return call[CompareStatus](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=1", repo, esc(base), esc(head)), nil)
}

// UpdatePullBranch 把目标分支合并进 PR 分支（异步，GitHub 返回 202，新的 head 由之后的 synchronize 事件带来）。
// expectedHead 与 PR 当前的 head 不同时 GitHub 返回 422。
func (i *Inst) UpdatePullBranch(ctx context.Context, repo string, number int, expectedHead string) error {
	return i.Do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/update-branch", repo, number),
		map[string]string{"expected_head_sha": expectedHead}, nil)
}

// MergeResult 是合并 PR 的结果。
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// MergePull 合并 PR。sha 是期望的 head（不同时 GitHub 返回 409），method 是 merge、squash 或 rebase。
func (i *Inst) MergePull(ctx context.Context, repo string, number int, sha, method string) (*MergeResult, error) {
	return call[MergeResult](ctx, i.Do, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number),
		map[string]string{"sha": sha, "merge_method": method})
}

// AddLabels 给 issue / PR 加 label（label 不存在时 GitHub 自动创建）。
func (i *Inst) AddLabels(ctx context.Context, repo string, number int, labels ...string) error {
	return i.Do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), map[string][]string{"labels": labels}, nil)
}

// RemoveLabel 去掉 issue / PR 的一个 label；没有这个 label 时 GitHub 返回 404。
func (i *Inst) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	return i.Do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, esc(label)), nil, nil)
}

// CreateIssueComment 在 issue / PR 上发表评论。
func (i *Inst) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	return i.Do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, nil)
}

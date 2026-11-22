package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AI 开发（PLAN #26.11）用到的接口：按 label 列出 issue、读取 issue、更新和删除分支、按 head 分支找 PR、创建 PR。
// 需要 App 的 Issues、Pull requests、Contents 读写权限。

// Issue 是一个 issue 的当前内容。
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []Label   `json:"labels"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	// PullRequest 不为 nil 时这是一个 PR（GitHub 的 issues 接口也返回 PR）。
	PullRequest *struct{} `json:"pull_request"`
}

// LabelNames 返回 issue 的 label 名称。
func (is *Issue) LabelNames() []string {
	out := make([]string, 0, len(is.Labels))
	for _, l := range is.Labels {
		out = append(out, l.Name)
	}
	return out
}

// ListOpenIssuesByLabel 返回带 label 的打开的 issue（不含 PR），按创建时间从早到晚，逐页读取，最多读 maxItems 条
// （GitHub 的结果中含 PR，按含 PR 的条数计）。complete 为假表示还有没有读到的。
func (i *Inst) ListOpenIssuesByLabel(ctx context.Context, repo, label string, maxItems int) (list []Issue, complete bool, err error) {
	q := url.Values{"state": {"open"}, "labels": {label}, "sort": {"created"}, "direction": {"asc"}, "per_page": {strconv.Itoa(perPage)}}
	for page, read := 1, 0; read < maxItems; page++ {
		q.Set("page", strconv.Itoa(page))
		items, err := call[[]Issue](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/issues?%s", repo, q.Encode()), nil)
		if err != nil {
			return nil, false, err
		}
		read += len(*items)
		for _, is := range *items {
			if is.PullRequest == nil {
				list = append(list, is)
			}
		}
		if len(*items) < perPage {
			return list, true, nil
		}
	}
	return list, false, nil
}

// GetIssue 读取一个 issue。
func (i *Inst) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	return call[Issue](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil)
}

// CreateIssue 创建一个 issue。labels 为空时不带 labels 字段。返回创建出来的 issue。
func (i *Inst) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (*Issue, error) {
	req := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		req["labels"] = labels
	}
	return call[Issue](ctx, i.Do, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repo), req)
}

// UpdateRef 把 ref（如 heads/kiln/dev-1）移到 sha。force 为 false 时只允许快进：不是快进时 GitHub 返回 422
// （"Update is not a fast forward"），ref 不变。
func (i *Inst) UpdateRef(ctx context.Context, repo, ref, sha string, force bool) error {
	return i.Do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/git/refs/%s", repo, escPath(ref)), map[string]any{"sha": sha, "force": force}, nil)
}

// DeleteRef 删除 ref（如 heads/kiln/attempt-9）；不存在时 GitHub 返回 422。
func (i *Inst) DeleteRef(ctx context.Context, repo, ref string) error {
	return i.Do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/git/refs/%s", repo, escPath(ref)), nil, nil)
}

// HeadPull 是按 head 分支列出的 PR。
type HeadPull struct {
	PullRequest
	// MergedAt 非空表示已合并（列表接口没有 merged 字段）；MergeCommitSHA 是合并得到的 commit。
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
}

// headPullsLimit 是按 head 分支列出 PR 时取的数量（同一个分支的 PR 很少，只取一页）。
const headPullsLimit = 30

// PullsByHead 返回 head 分支为 branch（同一仓库）的 PR，包括已关闭的，按 GitHub 的默认顺序（创建时间倒序，即最新的
// headPullsLimit 个）。
func (i *Inst) PullsByHead(ctx context.Context, repo, owner, branch string) ([]HeadPull, error) {
	q := url.Values{"state": {"all"}, "head": {owner + ":" + branch}, "per_page": {strconv.Itoa(headPullsLimit)}}
	list, err := call[[]HeadPull](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?%s", repo, q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	return *list, nil
}

// EditPullBody 修改 PR 的描述。
func (i *Inst) EditPullBody(ctx context.Context, repo string, number int, body string) error {
	return i.Do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]string{"body": body}, nil)
}

// CreatePull 创建 PR。同一个 head 分支已经有打开的 PR 时 GitHub 返回 422（"A pull request already exists"）。
func (i *Inst) CreatePull(ctx context.Context, repo, title, head, base, body string) (*PullRequest, error) {
	return call[PullRequest](ctx, i.Do, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repo),
		map[string]any{"title": title, "head": head, "base": base, "body": body, "maintainer_can_modify": false})
}

// Comparison 是 base...head 的比较：改动的文件和 head 多出的提交的说明（AI 开发推进前核对，PLAN #26.12）。
type Comparison struct {
	// Files 是改动的文件（重命名同时包含新旧路径）；FilesComplete 为 false 表示列表被截断。
	Files         []string
	FilesComplete bool
	// Messages 是多出的提交的说明；MessagesComplete 为 false 表示提交太多，只返回了一部分。
	Messages         []string
	MessagesComplete bool
}

// CompareDetail 返回 base...head 改动的文件和提交说明（只读第一页：最多 perPage 个提交、compareMaxFiles 个文件）。
func (i *Inst) CompareDetail(ctx context.Context, repo, base, head string) (*Comparison, error) {
	var cmp struct {
		Files        []changedFile `json:"files"`
		TotalCommits int           `json:"total_commits"`
		Commits      []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	p := fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=%d", repo, esc(base), esc(head), perPage)
	if err := i.get(ctx, p, &cmp); err != nil {
		return nil, err
	}
	out := &Comparison{
		Files: fileNames(cmp.Files), FilesComplete: len(cmp.Files) < compareMaxFiles,
		MessagesComplete: len(cmp.Commits) >= cmp.TotalCommits,
	}
	for _, c := range cmp.Commits {
		out.Messages = append(out.Messages, c.Commit.Message)
	}
	return out, nil
}

package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 各列表接口最多请求的页数（每页 100 条）。
const (
	maxInstallationPages = 50
	maxRepoPages         = 100
	maxBranchPages       = 50
	maxPullPages         = 20
)

// Account 是用户或组织账号。
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // User | Organization | Bot
}

// Installation 是 App 在某个账号下的安装。
type Installation struct {
	ID          int64      `json:"id"`
	Account     Account    `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

// Repository 是仓库的基本信息。
type Repository struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         Account `json:"owner"`
	Private       bool    `json:"private"`
	DefaultBranch string  `json:"default_branch"`
	CloneURL      string  `json:"clone_url"`
}

// AppInfo 是 App 自身的信息。
type AppInfo struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// GetApp 返回 App 自身的信息（用于识别 bot 账号 <slug>[bot]）。
func (c *Client) GetApp(ctx context.Context) (*AppInfo, error) {
	return call[AppInfo](ctx, c.App, http.MethodGet, "/app", nil)
}

// ListInstallations 列出 App 的全部安装。
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	return paginate(ctx, c.appGet, "/app/installations", maxInstallationPages, unwrapList[Installation])
}

// GetInstallation 读取一个 installation（App JWT，GET /app/installations/{id}）。处理单个事件时用它定向校验本地还没有记录的
// installation，不必列出全部安装。不存在（已卸载）时返回 404 错误。
func (c *Client) GetInstallation(ctx context.Context, id int64) (*Installation, error) {
	return call[Installation](ctx, c.App, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), nil)
}

// GetRepoInstallation 读取仓库所属的本 App 的 installation（App JWT，GET /repos/{owner}/{name}/installation）。
// App 没有安装在这个仓库上（包括 installation 只选择了其他仓库）时返回 404 错误。公开仓库任何 token 都能读取，
// 读到仓库信息不能说明它在某个 installation 的授权范围内，要用它定向确认。
func (c *Client) GetRepoInstallation(ctx context.Context, fullName string) (*Installation, error) {
	owner, name, _ := strings.Cut(fullName, "/")
	return call[Installation](ctx, c.App, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(name)+"/installation", nil)
}

// DeleteInstallation 卸载一个 installation（PLAN #16.1）。
func (c *Client) DeleteInstallation(ctx context.Context, id int64) error {
	return c.App(ctx, http.MethodDelete, fmt.Sprintf("/app/installations/%d", id), nil, nil)
}

// ListRepos 列出 installation 可访问的仓库。
func (i *Inst) ListRepos(ctx context.Context) ([]Repository, error) {
	return paginate(ctx, i.get, "/installation/repositories", maxRepoPages, unwrapField[Repository]("repositories"))
}

// GetRepo 读取仓库信息。
func (i *Inst) GetRepo(ctx context.Context, fullName string) (*Repository, error) {
	owner, name, _ := strings.Cut(fullName, "/")
	return call[Repository](ctx, i.Do, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(name), nil)
}

// GetRepoByID 按仓库 ID 读取仓库信息（GET /repositories/{id}）。用 installation token 请求：私有仓库不在这个 installation 的
// 授权范围内（或已删除）时返回 404 错误；公开仓库任何 token 都能读取，授权范围要另外确认（repodir.Verify）。
func (i *Inst) GetRepoByID(ctx context.Context, id int64) (*Repository, error) {
	return call[Repository](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repositories/%d", id), nil)
}

// encodingNone 是 contents 接口对超过 1 MB 的文件返回的 encoding（content 为空）。
const encodingNone = "none"

// encodedContent 是 blob、contents 接口返回的文件内容。
type encodedContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// decode 返回解码后的内容（base64 内容中带换行）。
// contents 接口对 1 MB 到 100 MB 的文件返回 encoding "none" 和空的 content，不能当成空文件。
func (c *encodedContent) decode() ([]byte, error) {
	if c.Encoding == encodingNone {
		return nil, errors.New("文件超过 1 MB，GitHub 没有返回内容")
	}
	if c.Encoding != "base64" {
		return []byte(c.Content), nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("解码 base64 内容：%w", err)
	}
	return b, nil
}

// GetFileContent 读取某个 ref 下的文件内容。
func (i *Inst) GetFileContent(ctx context.Context, repo, path, ref string) ([]byte, error) {
	f, err := call[encodedContent](ctx, i.Do, http.MethodGet,
		fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escPath(path), url.QueryEscape(ref)), nil)
	if err != nil {
		return nil, err
	}
	return f.decode()
}

// Branch 是一个分支及其最新 commit。
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// ListBranches 列出所有分支。
func (i *Inst) ListBranches(ctx context.Context, repo string) ([]Branch, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/branches", repo), maxBranchPages, unwrapList[Branch])
}

// PRRef 是 PR 的 head 或 base。
type PRRef struct {
	Ref  string     `json:"ref"`
	SHA  string     `json:"sha"`
	Repo Repository `json:"repo"`
}

// IsFork 判断 head、base 所属的 PR 是否来自另一个仓库（fork，PLAN #16.1 一律丢弃）。
// 不知道 head 或 base 属于哪个仓库（fork 已删除时 head.repo 为 null、payload 不完整）时也当作 fork：宁可丢弃，不放过。
func IsFork(head, base PRRef) bool {
	return head.Repo.ID == 0 || base.Repo.ID == 0 || head.Repo.ID != base.Repo.ID
}

// PullRequest 是 PR 的基本信息。
type PullRequest struct {
	Number int     `json:"number"`
	State  string  `json:"state"`
	Draft  bool    `json:"draft"`
	Head   PRRef   `json:"head"`
	Base   PRRef   `json:"base"`
	User   Account `json:"user"`
}

// ListOpenPulls 列出打开的 PR。
func (i *Inst) ListOpenPulls(ctx context.Context, repo string) ([]PullRequest, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/pulls?state=open", repo), maxPullPages, unwrapList[PullRequest])
}

package fakegithub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// 模拟 internal/github/pulls.go 用到的接口（合并队列，PLAN #28）：读取 PR、更新 PR 分支、合并、label；评论见 issues.go。
// 更新分支和合并在仓库的 git 目录中真实地生成提交、移动分支；不发送 webhook（由测试按需投递）。

// Pull 是模拟的 PR（SetPull 登记）。
type Pull struct {
	Number  int
	Title   string
	Body    string
	State   string // open | closed，默认 open
	Draft   bool
	HeadRef string
	HeadSHA string
	BaseRef string
	// ForkRepoID 非 0 时 PR 来自这个仓库（fork）。
	ForkRepoID     int64
	Labels         []string
	Merged         bool
	MergeCommitSHA string
	MergeMethod    string
	// MergedBy 是合并者的 login：通过 API 合并的是 App 的 bot 账号，MergePull 合并的是 s.Account（已设置时不变）。
	MergedBy string
	// MergedByBot 为真时合并者的类型是 Bot，即使 login 不带 [bot] 后缀（例如外部机器人）。
	MergedByBot bool
	// Author 是 PR 作者的 login（空为 s.Account）；以 [bot] 结尾时类型是 Bot。
	Author string
	// Comments 是 PR 上的评论（按时间顺序）。
	Comments []string
	// ClosingIssues 是 PR 关联的、合并时会被关闭的 issue（GraphQL closingIssuesReferences，见 issues.go）。
	ClosingIssues []IssueRef
	// ClosingIssuesForbidden 为真时读取 ClosingIssues 返回 GraphQL 的 FORBIDDEN 错误（模拟 App 没有 Issues 权限）。
	ClosingIssuesForbidden bool
	// Updates 是 update-branch 的次数。
	Updates int
	// UpdateErr、MergeErr 非空时对应的接口返回这个错误（状态码和信息），用于模拟冲突、被拒等。
	UpdateErr *APIErr
	MergeErr  *APIErr
	// Mergeability 非 nil 时读取单个 PR 返回它（模拟 GitHub 还没有计算完成等）；为 nil 时按仓库中的 git 实际计算
	// （与 update-branch 用同一个 merge-tree 判断冲突，打开的 PR 才计算）。
	Mergeability *Mergeability
}

// Mergeability 是读取单个 PR 时返回的 mergeable、mergeable_state。Mergeable 为 nil 表示还没有计算完成（JSON 的 null）。
type Mergeability struct {
	Mergeable *bool
	State     string
}

// 常用的 mergeability。
var (
	// MergeabilityUnknown：GitHub 还没有计算完成。
	MergeabilityUnknown = &Mergeability{State: "unknown"}
	// MergeabilityClean：可以合并。
	MergeabilityClean = &Mergeability{Mergeable: new(true), State: "clean"}
	// MergeabilityDirty：与目标分支冲突。
	MergeabilityDirty = &Mergeability{Mergeable: new(false), State: "dirty"}
)

// APIErr 是模拟接口返回的错误。
type APIErr struct {
	Status  int
	Message string
}

// SetPull 登记（或替换）仓库 r 的一个 PR。
func (s *Server) SetPull(r *Repo, p Pull) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.State == "" {
		p.State = "open"
	}
	if p.BaseRef == "" {
		p.BaseRef = r.DefaultBranch
	}
	if s.pulls == nil {
		s.pulls = map[string]*Pull{}
	}
	s.pulls[pullKey(r.FullName(), p.Number)] = &p
	s.prHeads[strconv.Itoa(p.Number)] = p.HeadSHA
}

// UpdatePull 在锁内修改已登记的 PR（例如推送新 commit、加 label、关闭）。
func (s *Server) UpdatePull(r *Repo, number int, f func(p *Pull)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[pullKey(r.FullName(), number)]; p != nil {
		f(p)
		s.prHeads[strconv.Itoa(number)] = p.HeadSHA
	}
}

// GetPull 返回 PR 的当前状态（副本）。
func (s *Server) GetPull(r *Repo, number int) (Pull, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pulls[pullKey(r.FullName(), number)]
	if p == nil {
		return Pull{}, false
	}
	out := *p
	out.Labels = slices.Clone(p.Labels)
	out.Comments = slices.Clone(p.Comments)
	return out, true
}

func pullKey(repo string, n int) string { return strings.ToLower(repo) + "#" + strconv.Itoa(n) }

func (s *Server) pullJSON(rp *Repo, p *Pull) map[string]any {
	repo := s.repoJSON(rp)
	headRepo := repo
	if p.ForkRepoID != 0 {
		headRepo = map[string]any{"id": p.ForkRepoID, "full_name": "fork/" + rp.Name}
	}
	m := map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State, "draft": p.Draft, "merged": p.Merged,
		"labels": labelsJSON(p.Labels), "user": s.authorJSON(p.Author),
		"head": map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": headRepo},
		"base": map[string]any{"ref": p.BaseRef, "repo": repo},
	}
	if p.Merged {
		m["merged_at"] = "2026-01-01T00:00:00Z"
		m["merge_commit_sha"] = p.MergeCommitSHA
		typ := "User"
		if p.MergedByBot || strings.HasSuffix(p.MergedBy, "[bot]") {
			typ = "Bot"
		}
		m["merged_by"] = map[string]any{"login": p.MergedBy, "id": 1, "type": typ}
	}
	return m
}

// findPull 查找路径中的 PR（调用方持有 s.mu 之外）；不存在时写出 404。
func (s *Server) findPull(w http.ResponseWriter, r *http.Request) (*Repo, *Pull) {
	rp := s.repo(w, r)
	if rp == nil {
		return nil, nil
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	s.mu.Lock()
	p := s.pulls[pullKey(rp.FullName(), n)]
	s.mu.Unlock()
	if p == nil {
		notFound(w)
		return nil, nil
	}
	return rp, p
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request) {
	rp, p := s.findPull(w, r)
	if p == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.pullJSON(rp, p)
	mb := p.Mergeability
	if mb == nil {
		mb = computeMergeability(r.Context(), rp, p)
	}
	m["mergeable"], m["mergeable_state"] = nil, mb.State
	if mb.Mergeable != nil {
		m["mergeable"] = *mb.Mergeable
	}
	writeJSON(w, http.StatusOK, m)
}

// computeMergeability 按仓库中的 git 计算打开的 PR 能否合并到目标分支：用 update-branch 同一个 merge-tree 判断冲突；
// 已经关闭、合并的 PR 或者算不出来（没有 git 仓库、commit 不存在）时是还没有计算完成。
func computeMergeability(ctx context.Context, rp *Repo, p *Pull) *Mergeability {
	if p.State != "open" || p.Merged || rp.Dir == "" {
		return MergeabilityUnknown
	}
	base, err := gitPlain(ctx, rp.Dir, "rev-parse", "--verify", "-q", "refs/heads/"+p.BaseRef)
	if err != nil {
		return MergeabilityUnknown
	}
	_, conflict, err := mergeTree(ctx, rp.Dir, p.HeadSHA, base)
	switch {
	case err != nil:
		return MergeabilityUnknown
	case conflict:
		return MergeabilityDirty
	}
	return MergeabilityClean
}

// mergeTree 在 dir 中合并 base 和 head，返回合并结果的 tree；有冲突时 conflict 为 true。
func mergeTree(ctx context.Context, dir, base, head string) (tree string, conflict bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", base, head)
	cmd.Dir = dir
	out, err := cmd.Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return "", true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("git merge-tree：%w", err)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), false, nil
}

// commitTree 在 dir 中用 tree 和父提交生成 commit。
func commitTree(ctx context.Context, dir, tree, msg string, parents ...string) (string, error) {
	args := []string{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit-tree"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return gitPlain(ctx, dir, append(args, "-m", msg, tree)...)
}

// gitPlain 执行不支持 --end-of-options 的 git 命令（rev-parse、commit-tree 等）。参数都来自已登记的 PR 和已解析的 sha，
// 不是请求中的任意值；返回去掉首尾空白的输出。
func gitPlain(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v：%w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// updateBranch 模拟 PUT /pulls/{n}/update-branch：把目标分支合并进 PR 分支（GitHub 是异步的，这里立即完成），
// 移动 PR 的分支，返回 202。expected_head_sha 不符或有冲突时返回 422。
func (s *Server) updateBranch(w http.ResponseWriter, r *http.Request) {
	rp, p := s.findPull(w, r)
	if p == nil {
		return
	}
	var body struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case p.UpdateErr != nil:
		writeMessage(w, p.UpdateErr.Status, p.UpdateErr.Message)
		return
	case body.ExpectedHeadSHA != "" && body.ExpectedHeadSHA != p.HeadSHA:
		writeMessage(w, http.StatusUnprocessableEntity, "expected head sha didn't match current head ref.")
		return
	}
	ctx := r.Context()
	base, err := gitPlain(ctx, rp.Dir, "rev-parse", "--verify", "refs/heads/"+p.BaseRef)
	if err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	tree, conflict, err := mergeTree(ctx, rp.Dir, p.HeadSHA, base)
	switch {
	case err != nil:
		writeMessage(w, http.StatusInternalServerError, err.Error())
		return
	case conflict:
		writeMessage(w, http.StatusUnprocessableEntity, "merge conflict between base and head")
		return
	}
	sha, err := commitTree(ctx, rp.Dir, tree, fmt.Sprintf("Merge branch '%s' into %s", p.BaseRef, p.HeadRef), p.HeadSHA, base)
	if err == nil && p.HeadRef != "" {
		_, err = gitPlain(ctx, rp.Dir, "update-ref", "refs/heads/"+p.HeadRef, sha)
	}
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.HeadSHA = sha
	p.Updates++
	s.prHeads[strconv.Itoa(p.Number)] = sha
	writeJSON(w, http.StatusAccepted, map[string]any{"message": "Updating pull request branch."})
}

// mergePull 模拟 PUT /pulls/{n}/merge：生成合并提交（merge 方式两个父提交，其余一个）并移动目标分支。
// sha 与 head 不符时 409，PR 不能合并时 405。
func (s *Server) mergePull(w http.ResponseWriter, r *http.Request) {
	rp, p := s.findPull(w, r)
	if p == nil {
		return
	}
	var body struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case p.MergeErr != nil:
		writeMessage(w, p.MergeErr.Status, p.MergeErr.Message)
		return
	case p.Merged || p.State != "open":
		writeMessage(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	case body.SHA != "" && body.SHA != p.HeadSHA:
		writeMessage(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
		return
	}
	sha, err := s.mergeCommit(r.Context(), rp, p, body.MergeMethod)
	if err != nil {
		writeMessage(w, http.StatusMethodNotAllowed, err.Error())
		return
	}
	p.MergedBy = s.AppSlug + "[bot]"
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

// MergePull 在平台上直接合并 PR（模拟在合并队列之外手动合并），返回合并提交。
func (s *Server) MergePull(r *Repo, number int, method string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pulls[pullKey(r.FullName(), number)]
	if p == nil || p.Merged || p.State != "open" {
		return "", fmt.Errorf("PR #%d 不能合并", number)
	}
	if p.MergedBy == "" {
		p.MergedBy = s.Account
	}
	return s.mergeCommit(context.Background(), r, p, method)
}

// mergeCommit 生成合并提交、移动目标分支并把 PR 标记为已合并（调用方持有 s.mu）。
func (s *Server) mergeCommit(ctx context.Context, rp *Repo, p *Pull, method string) (string, error) {
	base, err := gitPlain(ctx, rp.Dir, "rev-parse", "--verify", "refs/heads/"+p.BaseRef)
	if err != nil {
		return "", err
	}
	tree, conflict, err := mergeTree(ctx, rp.Dir, base, p.HeadSHA)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("merge conflict")
	}
	parents := []string{base}
	if method == "merge" {
		parents = append(parents, p.HeadSHA)
	}
	sha, err := commitTree(ctx, rp.Dir, tree, fmt.Sprintf("%s (#%d)", p.Title, p.Number), parents...)
	if err != nil {
		return "", err
	}
	if _, err := gitPlain(ctx, rp.Dir, "update-ref", "refs/heads/"+p.BaseRef, sha, base); err != nil {
		return "", err
	}
	p.Merged, p.State, p.MergeCommitSHA, p.MergeMethod = true, "closed", sha, method
	return sha, nil
}

// addLabels 模拟 POST /issues/{n}/labels。
func (s *Server) addLabels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Labels []string `json:"labels"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	labels := s.labelsOf(w, r)
	if labels == nil {
		return
	}
	for _, l := range body.Labels {
		if !slices.Contains(*labels, l) {
			*labels = append(*labels, l)
		}
	}
	writeJSON(w, http.StatusOK, []any{})
}

// labelsOf 返回路径中的 PR 或 issue 的 label（调用方持有 s.mu）；都不存在时写出 404 并返回 nil。
func (s *Server) labelsOf(w http.ResponseWriter, r *http.Request) *[]string {
	rp := s.repos[strings.ToLower(repoName(r))]
	n, _ := strconv.Atoi(r.PathValue("n"))
	switch {
	case rp == nil:
	case s.pulls[pullKey(rp.FullName(), n)] != nil:
		return &s.pulls[pullKey(rp.FullName(), n)].Labels
	case s.issues[issueKey(rp.FullName(), n)] != nil:
		return &s.issues[issueKey(rp.FullName(), n)].Labels
	}
	notFound(w)
	return nil
}

// removeLabel 模拟 DELETE /issues/{n}/labels/{name}：PR 或 issue 没有这个 label 时 404。
func (s *Server) removeLabel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	labels := s.labelsOf(w, r)
	if labels == nil {
		return
	}
	i := slices.Index(*labels, name)
	if i < 0 {
		notFound(w)
		return
	}
	*labels = slices.Delete(*labels, i, i+1)
	writeJSON(w, http.StatusOK, []any{})
}

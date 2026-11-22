package fakegithub

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 模拟 internal/github/issues.go 用到的接口（合并队列合并后关闭关联的 issue）：在 issue / PR 上评论、
// 关闭 issue，以及 GraphQL 中读取 PR 的 closingIssuesReferences。GraphQL 只支持这一个查询。

// Issue 是模拟的 issue（SetIssue 登记）。PR 上的评论记在 Pull.Comments 中。
type Issue struct {
	Number int
	// Title、Body、Labels 是 issue 的内容（AI 开发按 label 列出、读取，PLAN #26.11）。
	Title  string
	Body   string
	Labels []string
	// State 是 open 或 closed，默认 open。
	State string
	// UpdatedAt 是 GitHub 上的 updated_at（零值时不输出）。
	UpdatedAt   time.Time
	StateReason string
	// Comments 是本 App 发表的评论（GraphQL 中 viewerDidAuthor 为真）；UserComments 是其他用户的评论。
	Comments     []string
	UserComments []string
	// Closes 是通过 API 关闭的次数。
	Closes int
	// CommentErr、CloseErr 非空时评论、关闭返回这个错误（例如没有权限）。
	CommentErr *APIErr
	CloseErr   *APIErr
}

// IssueRef 是 PR 关联的 issue。Repo（owner/name）为空表示与 PR 在同一仓库。
type IssueRef struct {
	Repo   string
	Number int
}

func issueKey(repo string, n int) string { return pullKey(repo, n) }

// SetIssue 登记（或替换）仓库 repo（owner/name，可以是没有用 AddRepo 登记的仓库）的一个 issue。
func (s *Server) SetIssue(repo string, is Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is.State == "" {
		is.State = "open"
	}
	s.issues[issueKey(repo, is.Number)] = &is
}

// UpdateIssue 在锁内修改已登记的 issue（例如手动关闭、重新打开）。
func (s *Server) UpdateIssue(repo string, number int, f func(is *Issue)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.issues[issueKey(repo, number)]; is != nil {
		f(is)
	}
}

// GetIssue 返回 issue 的当前状态（副本）。
func (s *Server) GetIssue(repo string, number int) (Issue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := s.issues[issueKey(repo, number)]
	if is == nil {
		return Issue{}, false
	}
	out := *is
	out.Labels = slices.Clone(is.Labels)
	out.Comments = slices.Clone(is.Comments)
	out.UserComments = slices.Clone(is.UserComments)
	return out, true
}

// createComment 模拟 POST /issues/{n}/comments：n 可以是 PR 或 issue。
func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	var body struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[pullKey(rp.FullName(), n)]; p != nil {
		p.Comments = append(p.Comments, body.Body)
	} else {
		is := s.issues[issueKey(rp.FullName(), n)]
		switch {
		case is == nil:
			notFound(w)
			return
		case is.CommentErr != nil:
			writeMessage(w, is.CommentErr.Status, is.CommentErr.Message)
			return
		}
		is.Comments = append(is.Comments, body.Body)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": s.id(), "body": body.Body})
}

// updateIssue 模拟 PATCH /issues/{n}（只支持修改 state 和 state_reason）。
func (s *Server) updateIssue(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	var body struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	is := s.issues[issueKey(rp.FullName(), n)]
	switch {
	case is == nil:
		notFound(w)
		return
	case is.CloseErr != nil && body.State == "closed":
		writeMessage(w, is.CloseErr.Status, is.CloseErr.Message)
		return
	}
	if body.State != "" {
		is.State = body.State
	}
	is.StateReason = body.StateReason
	if body.State == "closed" {
		is.Closes++
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": is.Number, "state": is.State, "state_reason": is.StateReason})
}

// getIssue 模拟 GET /issues/{n}：n 可以是 PR（只返回 number、state 和 pull_request）或 issue。
func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[pullKey(rp.FullName(), n)]; p != nil {
		writeJSON(w, http.StatusOK, map[string]any{"number": n, "state": p.State, "pull_request": map[string]any{}})
		return
	}
	is := s.issues[issueKey(rp.FullName(), n)]
	if is == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.issueJSON(rp, is))
}

// issueJSON 返回 issue 的 JSON（调用方持有 s.mu）。
func (s *Server) issueJSON(rp *Repo, is *Issue) map[string]any {
	return map[string]any{
		"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State, "labels": labelsJSON(is.Labels),
		"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", rp.FullName(), is.Number), "updated_at": issueTime(is.UpdatedAt),
	}
}

// issueTime 返回 JSON 中的时间（零值为 null）。
func issueTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func labelsJSON(names []string) []any {
	out := []any{}
	for _, l := range names {
		out = append(out, map[string]any{"name": l})
	}
	return out
}

// graphql 模拟 POST /graphql，只支持读取 PR 的 closingIssuesReferences（internal/github ListClosingIssues 的查询）。
func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string `json:"query"`
		Variables struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			Number int    `json:"number"`
		} `json:"variables"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	gqlErr := func(typ, msg string) map[string]any { return map[string]any{"type": typ, "message": msg} }
	if !strings.Contains(body.Query, "closingIssuesReferences") {
		writeJSON(w, http.StatusOK, map[string]any{"errors": []any{gqlErr("", "fakegithub 不支持这个 GraphQL 查询")}})
		return
	}
	v := body.Variables
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repos[strings.ToLower(v.Owner+"/"+v.Name)]
	if rp == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data":   map[string]any{"repository": nil},
			"errors": []any{gqlErr("NOT_FOUND", "Could not resolve to a Repository")},
		})
		return
	}
	p := s.pulls[pullKey(rp.FullName(), v.Number)]
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data":   map[string]any{"repository": map[string]any{"pullRequest": nil}},
			"errors": []any{gqlErr("NOT_FOUND", "Could not resolve to a PullRequest")},
		})
		return
	}
	nodes := []any{}
	for _, ref := range p.ClosingIssues {
		if p.ClosingIssuesForbidden {
			nodes = append(nodes, nil)
			continue
		}
		repo := cmp.Or(ref.Repo, rp.FullName())
		state, comments := "OPEN", []any{}
		if is := s.issues[issueKey(repo, ref.Number)]; is != nil {
			state = strings.ToUpper(is.State)
			for _, c := range is.UserComments {
				comments = append(comments, map[string]any{"body": c, "viewerDidAuthor": false})
			}
			for _, c := range is.Comments {
				comments = append(comments, map[string]any{"body": c, "viewerDidAuthor": true})
			}
		}
		nodes = append(nodes, map[string]any{
			"number": ref.Number, "state": state, "repository": map[string]any{"nameWithOwner": repo},
			"comments": map[string]any{"nodes": comments},
		})
	}
	// 与真实的 GraphQL 一样只返回查询中选择了的字段：查询漏掉字段时调用方读到零值，测试能发现
	data := project(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"closingIssuesReferences": map[string]any{"nodes": nodes},
	}}}, parseSelection(body.Query))
	resp := map[string]any{"data": data}
	if p.ClosingIssuesForbidden {
		resp["errors"] = []any{gqlErr("FORBIDDEN", "Resource not accessible by integration")}
	}
	writeJSON(w, http.StatusOK, resp)
}

// selection 是 GraphQL 查询的选择集：字段名 → 子选择集（叶子字段为 nil）。
type selection map[string]selection

// parseSelection 解析查询的选择集（只支持本包用到的写法：没有片段、别名和指令）。参数（括号中的内容，包括
// 变量 $number 和 pullRequest(number: $number)）不算字段。
func parseSelection(q string) selection {
	var toks []string
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '(':
			// 跳过参数和变量声明
			depth := 0
			for ; i < len(q); i++ {
				if q[i] == '(' {
					depth++
				} else if q[i] == ')' {
					depth--
					if depth == 0 {
						i++
						break
					}
				}
			}
		case c == '{' || c == '}':
			toks = append(toks, string(c))
			i++
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(q) && (q[j] == '_' || q[j] >= 'a' && q[j] <= 'z' || q[j] >= 'A' && q[j] <= 'Z' || q[j] >= '0' && q[j] <= '9') {
				j++
			}
			toks = append(toks, q[i:j])
			i = j
		default:
			i++
		}
	}
	// 跳过 query 关键字和操作名，从第一个 { 开始
	start := slices.Index(toks, "{")
	if start < 0 {
		return selection{}
	}
	sel, _ := parseSet(toks, start+1)
	return sel
}

// parseSet 从 toks[i]（{ 之后）解析到对应的 }，返回选择集和 } 之后的位置。
func parseSet(toks []string, i int) (selection, int) {
	sel := selection{}
	for i < len(toks) && toks[i] != "}" {
		name := toks[i]
		i++
		if i < len(toks) && toks[i] == "{" {
			sel[name], i = parseSet(toks, i+1)
		} else {
			sel[name] = nil
		}
	}
	return sel, i + 1
}

// project 只保留 v 中 sel 选择了的字段（数组逐个处理）。
func project(v any, sel selection) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range sel {
			if fv, ok := x[k]; ok {
				if child == nil {
					out[k] = fv
				} else {
					out[k] = project(fv, child)
				}
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = project(e, sel)
		}
		return out
	}
	return v
}

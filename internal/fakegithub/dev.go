package fakegithub

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// 模拟 internal/github/dev.go 用到的接口（AI 开发，PLAN #26.11）：按 label 列出 issue、按 head 分支列出 PR、创建和修改 PR、
// 移动和删除分支。分支操作在仓库的 git 目录中真实执行（快进检查用 git merge-base）。

// listIssues 模拟 GET /issues：按 state（默认 open）和 labels（逗号分隔，全部都要有）过滤，按编号升序（创建顺序），
// 按 per_page、page 分页。
// 与 GitHub 一样，PR 也会出现在结果中（带 pull_request 字段）。
func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	q := r.URL.Query()
	state := cmp.Or(q.Get("state"), "open")
	var want []string
	if l := q.Get("labels"); l != "" {
		want = strings.Split(l, ",")
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	match := func(st string, labels []string) bool {
		if state != "all" && st != state {
			return false
		}
		for _, l := range want {
			if !slices.Contains(labels, l) {
				return false
			}
		}
		return true
	}
	type item struct {
		n int
		j map[string]any
	}
	var items []item
	s.mu.Lock()
	prefix := strings.ToLower(rp.FullName()) + "#"
	for k, is := range s.issues {
		if strings.HasPrefix(k, prefix) && match(is.State, is.Labels) {
			items = append(items, item{is.Number, s.issueJSON(rp, is)})
		}
	}
	for k, p := range s.pulls {
		if strings.HasPrefix(k, prefix) && match(p.State, p.Labels) {
			items = append(items, item{p.Number, map[string]any{
				"number": p.Number, "title": p.Title, "state": p.State, "labels": labelsJSON(p.Labels), "pull_request": map[string]any{},
			}})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(items, func(a, b item) int { return cmp.Compare(a.n, b.n) })
	out := []any{}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	if perPage <= 0 {
		perPage = len(items)
	}
	for i := (page - 1) * perPage; i < len(items) && i < page*perPage; i++ {
		out = append(out, items[i].j)
	}
	writeJSON(w, http.StatusOK, out)
}

// createIssue 模拟 POST /issues：读 title/body/labels，分配仓库内下一个未被占用的编号（s.issues 中已登记的最大编号
// +1，起始至少 1），写入 s.issues，返回 201 和 issue JSON。
func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := strings.ToLower(rp.FullName()) + "#"
	next := 1
	for k, is := range s.issues {
		if strings.HasPrefix(k, prefix) {
			next = max(next, is.Number+1)
		}
	}
	is := &Issue{Number: next, Title: body.Title, Body: body.Body, State: "open", Labels: body.Labels}
	s.issues[issueKey(rp.FullName(), next)] = is
	writeJSON(w, http.StatusCreated, s.issueJSON(rp, is))
}

// listPulls 模拟 GET /pulls：按 state（open、closed、all，默认 open）和 head（owner:branch）过滤。与 GitHub 相同，默认
// 按创建时间（编号）倒序，direction=asc 时升序。
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	q := r.URL.Query()
	state := cmp.Or(q.Get("state"), "open")
	head := q.Get("head")
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []*Pull
	prefix := strings.ToLower(rp.FullName()) + "#"
	for k, p := range s.pulls {
		if !strings.HasPrefix(k, prefix) || (state != "all" && p.State != state) {
			continue
		}
		if head != "" && !strings.EqualFold(head, rp.Owner+":"+p.HeadRef) {
			continue
		}
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b *Pull) int { return cmp.Compare(b.Number, a.Number) })
	if q.Get("direction") == "asc" {
		slices.Reverse(list)
	}
	out := []any{}
	if firstPage(r) {
		for _, p := range list {
			out = append(out, s.pullJSON(rp, p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// createPull 模拟 POST /pulls（同一仓库的分支）：head 分支不存在、与 base 没有差异时返回 422；同一个 head 已经有打开的 PR
// 时返回 422 "A pull request already exists for owner:branch."。PR 的作者是本 App 的 bot 账号。
func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	var body struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	headSHA, err := s.branchHead(r, rp, body.Head)
	if err != nil || headSHA == "" {
		writeMessage(w, http.StatusUnprocessableEntity, "Validation Failed: head "+body.Head+" does not exist")
		return
	}
	baseSHA, err := s.branchHead(r, rp, body.Base)
	if err != nil || baseSHA == "" {
		writeMessage(w, http.StatusUnprocessableEntity, "Validation Failed: base "+body.Base+" does not exist")
		return
	}
	if _, err := git(r.Context(), rp.Dir, []string{"merge-base", "--is-ancestor"}, headSHA, baseSHA); err == nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Validation Failed: No commits between "+body.Base+" and "+body.Head)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := strings.ToLower(rp.FullName()) + "#"
	next := 1
	for k, p := range s.pulls {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if p.HeadRef == body.Head && p.State == "open" {
			writeMessage(w, http.StatusUnprocessableEntity, fmt.Sprintf("Validation Failed: A pull request already exists for %s:%s.", rp.Owner, body.Head))
			return
		}
		next = max(next, p.Number+1)
	}
	for k, is := range s.issues {
		if strings.HasPrefix(k, prefix) {
			next = max(next, is.Number+1)
		}
	}
	p := &Pull{
		Number: next, Title: body.Title, Body: body.Body, State: "open", HeadRef: body.Head, HeadSHA: headSHA, BaseRef: body.Base,
		Author: s.AppSlug + "[bot]",
	}
	if s.pulls == nil {
		s.pulls = map[string]*Pull{}
	}
	s.pulls[pullKey(rp.FullName(), next)] = p
	s.prHeads[strconv.Itoa(next)] = headSHA
	writeJSON(w, http.StatusCreated, s.pullJSON(rp, p))
}

// editPull 模拟 PATCH /pulls/{n}（只支持修改 title 和 body）。
func (s *Server) editPull(w http.ResponseWriter, r *http.Request) {
	rp, p := s.findPull(w, r)
	if p == nil {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if body.Title != nil {
		p.Title = *body.Title
	}
	if body.Body != nil {
		p.Body = *body.Body
	}
	writeJSON(w, http.StatusOK, s.pullJSON(rp, p))
}

// branchHead 返回分支的 commit，不存在时返回空。
func (s *Server) branchHead(r *http.Request, rp *Repo, branch string) (string, error) {
	out, err := git(r.Context(), rp.Dir, []string{"for-each-ref", "--format=%(refname) %(objectname)"}, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, l := range lines(out) {
		if name, sha, ok := strings.Cut(l, " "); ok && name == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", nil
}

// refValue 返回 ref（完整名字）当前指向的对象，不存在时返回空。
func refValue(r *http.Request, rp *Repo, ref string) string {
	out, err := git(r.Context(), rp.Dir, []string{"for-each-ref", refFormat}, ref)
	if err != nil {
		return ""
	}
	for _, l := range lines(out) {
		if j, ok := refJSON(l); ok && j["ref"] == ref {
			obj, _ := j["object"].(map[string]any)
			sha, _ := obj["sha"].(string)
			return sha
		}
	}
	return ""
}

// updateRef 模拟 PATCH /git/refs/{ref}：ref 不存在、sha 不是已有的 commit 时返回 422；force 为 false 且不是快进时返回 422
// "Update is not a fast forward"（ref 不变）。
func (s *Server) updateRef(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	var body struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ref := "refs/" + r.PathValue("ref")
	old := refValue(r, rp, ref)
	if old == "" {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}
	if _, err := git(r.Context(), rp.Dir, []string{"cat-file", "-e"}, body.SHA+"^{commit}"); err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	if !body.Force {
		if _, err := git(r.Context(), rp.Dir, []string{"merge-base", "--is-ancestor"}, old, body.SHA); err != nil {
			writeMessage(w, http.StatusUnprocessableEntity, "Update is not a fast forward")
			return
		}
	}
	// 旧值作为条件：期间被别人移动时失败
	if _, err := git(r.Context(), rp.Dir, []string{"update-ref"}, ref, body.SHA, old); err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference cannot be updated")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "object": map[string]any{"type": "commit", "sha": body.SHA}})
}

// deleteRef 模拟 DELETE /git/refs/{ref}：不存在时返回 422 "Reference does not exist"。
func (s *Server) deleteRef(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	ref := "refs/" + r.PathValue("ref")
	old := refValue(r, rp, ref)
	if old == "" {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference does not exist")
		return
	}
	if _, err := git(r.Context(), rp.Dir, []string{"update-ref", "-d"}, ref, old); err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference cannot be deleted")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// IssuesPayload 构造 issues 事件（action 为 labeled、unlabeled 时 label 是变化的 label；sender 为空时是 s.Account）。
// issue 的内容取自 SetIssue 登记的状态。
func (s *Server) IssuesPayload(r *Repo, action string, number int, label, sender string) map[string]any {
	s.mu.Lock()
	is := s.issues[issueKey(r.FullName(), number)]
	var issue map[string]any
	if is != nil {
		issue = s.issueJSON(r, is)
	} else {
		issue = map[string]any{"number": number, "state": "open", "labels": []any{}}
	}
	s.mu.Unlock()
	p := map[string]any{
		"action": action, "issue": issue,
		"repository": s.repoJSON(r), "installation": map[string]any{"id": r.Installation}, "sender": s.authorJSON(sender),
	}
	if label != "" {
		p["label"] = map[string]any{"name": label}
	}
	return p
}

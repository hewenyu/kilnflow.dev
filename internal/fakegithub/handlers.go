package fakegithub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// routes 注册全部模拟的接口。每个请求都记录到 requests 中。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// perms 非空的是写请求：token 限定了仓库或权限时，要覆盖路径中的仓库、有其中任一权限的 write（checkToken）
	handle := func(pattern string, f http.HandlerFunc, perms ...string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.record(r.Method + " " + r.URL.Path)
			if s.throttle(w, r) || !s.checkToken(w, r, perms) || s.serveWithFault(w, r, f) {
				return
			}
			f(w, r)
		})
	}
	h := func(pattern string, f http.HandlerFunc) { handle(pattern, f) }
	const (
		contents     = "contents"
		pullRequests = "pull_requests"
		issues       = "issues"
		checks       = "checks"
	)
	h("GET /app", s.getApp)
	h("GET /app/installations", s.listInstallations)
	h("GET /app/installations/{id}", s.getInstallation)
	h("POST /app/installations/{id}/access_tokens", s.createToken)
	h(RevokeTokenRoute, s.revokeToken)
	h("GET /installation/repositories", s.listRepos)
	h("GET /repos/{owner}/{repo}", s.getRepo)
	h("GET /repos/{owner}/{repo}/installation", s.getRepoInstallation)
	h("GET /repositories/{id}", s.getRepoByID)
	h("GET /repos/{owner}/{repo}/git/trees/{sha}", s.getTree)
	h("GET /repos/{owner}/{repo}/git/blobs/{sha}", s.getBlob)
	h("GET /repos/{owner}/{repo}/git/commits/{sha}", s.getCommit)
	h("GET /repos/{owner}/{repo}/git/ref/{ref...}", s.getRef)
	h("GET /repos/{owner}/{repo}/git/matching-refs/{prefix...}", s.matchingRefs)
	h("GET /repos/{owner}/{repo}/git/tags/{sha}", s.getTag)
	handle("POST /repos/{owner}/{repo}/git/refs", s.createRef, contents)
	h("GET /repos/{owner}/{repo}/branches", s.listBranches)
	h("GET /repos/{owner}/{repo}/pulls", s.listPulls)
	handle("POST /repos/{owner}/{repo}/pulls", s.createPull, pullRequests)
	handle("PATCH /repos/{owner}/{repo}/pulls/{n}", s.editPull, pullRequests)
	h("GET /repos/{owner}/{repo}/issues", s.listIssues)
	// 创建 issue：Issues 或 Pull requests 的写权限都可以（与下面 issue 与 PR 共用的接口一致）
	handle("POST /repos/{owner}/{repo}/issues", s.createIssue, issues, pullRequests)
	handle("PATCH /repos/{owner}/{repo}/git/refs/{ref...}", s.updateRef, contents)
	handle("DELETE /repos/{owner}/{repo}/git/refs/{ref...}", s.deleteRef, contents)
	h("GET /repos/{owner}/{repo}/pulls/{n}", s.getPull)
	handle("PUT /repos/{owner}/{repo}/pulls/{n}/update-branch", s.updateBranch, pullRequests)
	handle("PUT /repos/{owner}/{repo}/pulls/{n}/merge", s.mergePull, contents)
	// issue 与 PR 共用的接口（label、评论、修改）：Issues 或 Pull requests 的写权限都可以
	handle("POST /repos/{owner}/{repo}/issues/{n}/labels", s.addLabels, issues, pullRequests)
	handle("DELETE /repos/{owner}/{repo}/issues/{n}/labels/{name}", s.removeLabel, issues, pullRequests)
	handle("POST /repos/{owner}/{repo}/issues/{n}/comments", s.createComment, issues, pullRequests)
	handle("PATCH /repos/{owner}/{repo}/issues/{n}", s.updateIssue, issues, pullRequests)
	h("GET /repos/{owner}/{repo}/issues/{n}", s.getIssue)
	h("POST /graphql", s.graphql)
	h("GET /repos/{owner}/{repo}/pulls/{n}/files", s.pullFiles)
	h("GET /repos/{owner}/{repo}/pulls/{n}/reviews", s.listReviews)
	handle("POST /repos/{owner}/{repo}/pulls/{n}/reviews", s.createReview, pullRequests)
	h("GET /repos/{owner}/{repo}/compare/{basehead}", s.compare)
	h("GET /repos/{owner}/{repo}/contents/{path...}", s.getContents)

	handle("POST /repos/{owner}/{repo}/check-runs", s.createCheckRun, checks)
	handle("PATCH /repos/{owner}/{repo}/check-runs/{id}", s.updateCheckRun, checks)
	h("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", s.listCheckRuns)

	h("GET /repos/{owner}/{repo}/releases", s.listReleases)
	h("GET /repos/{owner}/{repo}/releases/{id}", s.getRelease)
	handle("POST /repos/{owner}/{repo}/releases", s.createRelease, contents)
	handle("PATCH /repos/{owner}/{repo}/releases/{id}", s.updateRelease, contents)
	h("GET /repos/{owner}/{repo}/releases/{id}/assets", s.listAssets)
	handle("POST /repos/{owner}/{repo}/releases/{id}/assets", s.uploadAsset, contents)
	handle("DELETE /repos/{owner}/{repo}/releases/assets/{id}", s.deleteAsset, contents)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(unhandledPrefix + r.Method + " " + r.URL.String())
		notFound(w)
	})
	return mux
}

// ---------- 通用辅助 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// 状态码已经写出，编码失败也无法再告诉客户端
	_ = json.NewEncoder(w).Encode(v)
}

func writeMessage(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"message": msg})
}

func notFound(w http.ResponseWriter) { writeMessage(w, http.StatusNotFound, "Not Found") }

// decodeBody 解码 JSON 请求体，失败时返回 400。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeMessage(w, http.StatusBadRequest, "Problems parsing JSON: "+err.Error())
		return false
	}
	return true
}

// pathID 读取路径中的数字 ID，失败时返回 400。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// firstPage 判断请求的是否是第一页（模拟的列表接口只有一页数据）。
func firstPage(r *http.Request) bool {
	p := r.URL.Query().Get("page")
	return p == "" || p == "1"
}

func repoName(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }

// repo 查找路径中的仓库；不存在时写出 404 并返回 nil。
func (s *Server) repo(w http.ResponseWriter, r *http.Request) *Repo {
	s.mu.Lock()
	rp := s.repos[strings.ToLower(repoName(r))]
	s.mu.Unlock()
	if rp == nil {
		notFound(w)
	}
	return rp
}

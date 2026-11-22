package fakegithub

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// 模拟 internal/github/reviews.go 用到的接口：PR review（AI 审计的结果）。
// 与 GitHub 一样，行内评论的行不在 PR 的 diff 中时整个请求返回 422。

// ReviewComment 是 review 中的一条行内评论。
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side,omitempty"`
	Body string `json:"body"`
}

// Review 是记录下来的 PR review。
type Review struct {
	ID       int64           `json:"id"`
	Body     string          `json:"body"`
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event,omitempty"`
	Comments []ReviewComment `json:"comments,omitempty"`
	// User 是发布 review 的账号：通过 API 创建的是 App 的 bot 账号（<AppSlug>[bot]），AddReview 可以指定其他账号。
	User *ReviewUser `json:"user,omitempty"`
	Repo string      `json:"-"`
	PR   int         `json:"-"`
}

// ReviewUser 是发布 review 的账号。
type ReviewUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // User | Bot
}

// AddReview 直接在 PR repo#number 上记录一个 review（模拟其他账号发布的 review），返回它的 ID。
func (s *Server) AddReview(repo string, number int, r Review) int64 {
	r.ID, r.Repo, r.PR = s.id(), repo, number
	s.mu.Lock()
	s.reviews = append(s.reviews, &r)
	s.mu.Unlock()
	return r.ID
}

// Reviews 返回 PR repo#number 上创建的 review。
func (s *Server) Reviews(repo string, number int) []Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Review
	for _, r := range s.reviews {
		if r.Repo == repo && r.PR == number {
			out = append(out, *r)
		}
	}
	return out
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		notFound(w)
		return
	}
	var rv Review
	if !decodeBody(w, r, &rv) {
		return
	}
	if len(rv.Comments) > 0 {
		lines := s.diffLines(r, rp, r.PathValue("n"))
		for _, c := range rv.Comments {
			if !lines[c.Path][c.Line] {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": fmt.Sprintf("Line %d of %s is not part of the diff", c.Line, c.Path)})
				return
			}
		}
	}
	rv.ID, rv.Repo, rv.PR = s.id(), repoName(r), n
	rv.User = &ReviewUser{ID: s.AppID + 100000, Login: s.AppSlug + "[bot]", Type: "Bot"}
	stored := rv
	s.mu.Lock()
	s.reviews = append(s.reviews, &stored)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, rv)
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	if s.repo(w, r) == nil {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	out := []Review{}
	s.mu.Lock()
	stale := s.staleReviews
	s.mu.Unlock()
	if firstPage(r) && !stale {
		out = append(out, s.Reviews(repoName(r), n)...)
	}
	writeJSON(w, http.StatusOK, out)
}

// pullPatches 返回 PR（head 与它的父 commit 之间）每个变更文件的 patch。
func (s *Server) pullPatches(r *http.Request, rp *Repo, n string) map[string]string {
	s.mu.Lock()
	head := s.prHeads[n]
	s.mu.Unlock()
	out := map[string]string{}
	if head == "" {
		return out
	}
	diff, err := git(r.Context(), rp.Dir, []string{"diff", "-U3", "--no-color", "--no-renames"}, head+"~1", head)
	if err != nil {
		return out
	}
	var file string
	var patch []string
	flush := func() {
		if file != "" {
			out[file] = strings.Join(patch, "\n")
		}
	}
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			file, patch = "", nil
		case strings.HasPrefix(l, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
		case strings.HasPrefix(l, "@@") || (len(patch) > 0 && !strings.HasPrefix(l, "--- ")):
			patch = append(patch, l)
		}
	}
	flush()
	return out
}

// hunkRE 匹配 hunk 头中新文件一侧的起始行。
var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

// diffLines 返回 PR 每个文件中可以写行内评论的行（新增行和上下文行）。
func (s *Server) diffLines(r *http.Request, rp *Repo, n string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for file, patch := range s.pullPatches(r, rp, n) {
		lines := map[int]bool{}
		cur := 0
		for l := range strings.SplitSeq(patch, "\n") {
			if m := hunkRE.FindStringSubmatch(l); m != nil {
				cur, _ = strconv.Atoi(m[1])
				continue
			}
			if strings.HasPrefix(l, "+") || strings.HasPrefix(l, " ") {
				lines[cur] = true
				cur++
			}
		}
		out[file] = lines
	}
	return out
}

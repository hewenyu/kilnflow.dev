package fakegithub

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
)

// 模拟 internal/github/repos.go 用到的接口：App、installation 和仓库。

func (s *Server) getApp(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"id": s.AppID, "slug": s.AppSlug, "name": "Kiln Test"})
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) {
	list := []any{}
	if firstPage(r) {
		for _, id := range s.installations() {
			list = append(list, map[string]any{"id": id, "account": s.userJSON()})
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// installations 返回全部 installation 的 ID。
func (s *Server) installations() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64{s.InstallationID}, s.extraInstallations...)
}

func (s *Server) getInstallation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !slices.Contains(s.installations(), id) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "account": s.userJSON()})
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	var list []any
	if firstPage(r) {
		inst := s.installationOf(r)
		s.mu.Lock()
		for _, rp := range s.repos {
			if rp.Installation == inst {
				list = append(list, s.repoJSON(rp))
			}
		}
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "repositories": list})
}

// visible 判断请求所用的 installation token 能否读取仓库信息（模拟 GitHub 的授权范围：不在范围内的私有仓库返回 404；
// 公开仓库与 GitHub 一样，任何 token 都能读取）。
func (s *Server) visible(r *http.Request, rp *Repo) bool {
	inst := s.installationOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return !rp.Private || inst != 0 && rp.Installation == inst
}

// getRepoInstallation 模拟 GET /repos/{owner}/{repo}/installation（App JWT）：返回仓库所属的 installation，
// 仓库不在任何 installation 中时返回 404。
func (s *Server) getRepoInstallation(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	s.mu.Lock()
	inst := rp.Installation
	s.mu.Unlock()
	if !slices.Contains(s.installations(), inst) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inst, "account": s.userJSON()})
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	if !s.visible(r, rp) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.repoJSON(rp))
}

func (s *Server) getRepoByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var found *Repo
	s.mu.Lock()
	for _, rp := range s.repos {
		if rp.ID == id {
			found = rp
		}
	}
	s.mu.Unlock()
	if found == nil || !s.visible(r, found) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.repoJSON(found))
}

func (s *Server) getContents(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	out, err := git(r.Context(), rp.Dir, []string{"show"}, r.URL.Query().Get("ref")+":"+r.PathValue("path"))
	if err != nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(out))})
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	list := []any{}
	if firstPage(r) {
		out, _ := git(r.Context(), rp.Dir, []string{"for-each-ref", "--format=%(refname:short) %(objectname)"}, "refs/heads")
		for _, l := range lines(out) {
			if f := strings.Fields(l); len(f) == 2 {
				list = append(list, map[string]any{"name": f[0], "commit": map[string]any{"sha": f[1]}})
			}
		}
	}
	writeJSON(w, http.StatusOK, list)
}

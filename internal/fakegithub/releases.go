package fakegithub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// 模拟 internal/github/releases.go 用到的接口：Releases 和 asset。

// withRelease 持锁按 ID 查找 Release，找到时调用 f 并返回 f 的结果。
// f 应返回可以在锁外使用的副本（写响应在锁外进行）。
func (s *Server) withRelease(id int64, f func(rel *Release) any) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := s.releases[id]
	if rel == nil {
		return nil, false
	}
	return f(rel), true
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	list := []Release{}
	if firstPage(r) {
		list = append(list, s.Releases(repoName(r))...)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, ok := s.withRelease(id, func(rel *Release) any { return rel.clone() })
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Release
		Draft      *bool `json:"draft"`
		Prerelease *bool `json:"prerelease"`
		Generate   bool  `json:"generate_release_notes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	rel := body.Release
	rel.Draft = body.Draft != nil && *body.Draft
	rel.Prerelease = body.Prerelease != nil && *body.Prerelease
	if body.Generate {
		rel.Body = body.Body + "\n\n**Full Changelog**: generated"
	}
	rel.ID = s.id()
	rel.Repo = repoName(r)
	rel.HTMLURL = fmt.Sprintf("https://github.com/%s/releases/tag/%s", rel.Repo, rel.TagName)
	rel.Assets = []*Asset{}
	stored := rel
	s.mu.Lock()
	s.releases[rel.ID] = &stored
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, rel)
}

func (s *Server) updateRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Draft      *bool  `json:"draft"`
		MakeLatest string `json:"make_latest"`
		Target     string `json:"target_commitish"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	out, ok := s.withRelease(id, func(rel *Release) any {
		if body.Draft != nil {
			rel.Draft = *body.Draft
		}
		if body.MakeLatest != "" {
			rel.MakeLatest = body.MakeLatest
		}
		if body.Target != "" {
			rel.TargetCommitish = body.Target
		}
		return rel.clone()
	})
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, ok := s.withRelease(id, func(rel *Release) any {
		if !firstPage(r) {
			return []*Asset{}
		}
		return rel.clone().Assets
	})
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) uploadAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	sum := sha256.Sum256(data)
	a := &Asset{
		ID: s.id(), Name: r.URL.Query().Get("name"), Size: int64(len(data)),
		Digest: "sha256:" + hex.EncodeToString(sum[:]), State: "uploaded", Data: data,
	}
	out, ok := s.withRelease(id, func(rel *Release) any {
		rel.Assets = append(rel.Assets, a)
		ac := *a
		return ac
	})
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !s.removeAsset(id) {
		notFound(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeAsset 删除某个 asset，返回是否找到。
func (s *Server) removeAsset(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.releases {
		if i := slices.IndexFunc(rel.Assets, func(a *Asset) bool { return a.ID == id }); i >= 0 {
			rel.Assets = slices.Delete(rel.Assets, i, i+1)
			return true
		}
	}
	return false
}

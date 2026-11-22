package fakegithub

import (
	"net/http"
	"time"
)

// 模拟 internal/github/checks.go 用到的接口：Check Runs。

func (s *Server) createCheckRun(w http.ResponseWriter, r *http.Request) {
	var c CheckRun
	if !decodeBody(w, r, &c) {
		return
	}
	c.ID = s.id()
	c.Repo = repoName(r)
	if c.Status == "" {
		c.Status = "queued"
	}
	if c.StartedAt == nil {
		// 与 GitHub 相同：没有给 started_at 时用创建时间（创建一个已经结束的 Check Run 时会晚于 completed_at）
		now := time.Now()
		c.StartedAt = &now
	}
	stored := c
	s.mu.Lock()
	s.checkRuns[c.ID] = &stored
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateCheckRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var u CheckRun
	if !decodeBody(w, r, &u) {
		return
	}
	s.mu.Lock()
	c := s.checkRuns[id]
	if c == nil {
		s.mu.Unlock()
		notFound(w)
		return
	}
	if u.Status != "" {
		c.Status = u.Status
	}
	if u.Conclusion != "" {
		c.Conclusion = u.Conclusion
	}
	if u.Name != "" {
		c.Name = u.Name
	}
	if u.Output != nil {
		c.Output = u.Output
	}
	if u.StartedAt != nil {
		c.StartedAt = u.StartedAt
	}
	if u.CompletedAt != nil {
		c.CompletedAt = u.CompletedAt
	}
	c.UpdateCount++
	out := *c
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listCheckRuns(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("check_name")
	var list []any
	for _, c := range s.CheckRuns(r.PathValue("sha")) {
		if name == "" || c.Name == name {
			list = append(list, map[string]any{"id": c.ID, "name": c.Name, "external_id": c.ExternalID, "head_sha": c.HeadSHA, "app": map[string]any{"id": s.AppID}})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "check_runs": list})
}

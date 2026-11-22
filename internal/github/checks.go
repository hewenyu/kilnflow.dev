package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CheckRunOutput 是 Check Run 页面上显示的内容。
type CheckRunOutput struct {
	Title       string       `json:"title"`
	Summary     string       `json:"summary"`
	Text        string       `json:"text,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Annotation 是 Check Run 中针对某个文件位置的标注。
type Annotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"` // notice | warning | failure
	Message         string `json:"message"`
}

// Check Run 的 status。
const (
	CheckStatusQueued     = "queued"
	CheckStatusInProgress = "in_progress"
	CheckStatusCompleted  = "completed"
)

// CheckRun 是 Check Run 的创建、更新参数和返回值。
type CheckRun struct {
	ID          int64           `json:"id,omitempty"`
	Name        string          `json:"name,omitempty"`
	HeadSHA     string          `json:"head_sha,omitempty"`
	ExternalID  string          `json:"external_id,omitempty"`
	DetailsURL  string          `json:"details_url,omitempty"`
	Status      string          `json:"status,omitempty"`
	Conclusion  string          `json:"conclusion,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Output      *CheckRunOutput `json:"output,omitempty"`
}

// CreateCheckRun 创建 Check Run。
func (i *Inst) CreateCheckRun(ctx context.Context, repo string, cr *CheckRun) (*CheckRun, error) {
	return call[CheckRun](ctx, i.Do, http.MethodPost, fmt.Sprintf("/repos/%s/check-runs", repo), cr)
}

// UpdateCheckRun 更新 Check Run（忽略 cr 中的 ID 和 HeadSHA）。
func (i *Inst) UpdateCheckRun(ctx context.Context, repo string, id int64, cr *CheckRun) error {
	body := *cr
	body.ID = 0
	body.HeadSHA = ""
	return i.Do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", repo, id), &body, nil)
}

// FindCheckRun 按名称和 external_id 查找 sha 上已有的 Check Run（崩溃后避免重复创建，PLAN #7.8）。
// 没有找到时返回 nil, nil。
func (i *Inst) FindCheckRun(ctx context.Context, repo, sha, name, externalID string, appID int64) (*CheckRun, error) {
	var res struct {
		CheckRuns []struct {
			CheckRun
			App struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	p := fmt.Sprintf("/repos/%s/commits/%s/check-runs?check_name=%s&filter=all&per_page=%d", repo, esc(sha), url.QueryEscape(name), perPage)
	if appID != 0 {
		p += fmt.Sprintf("&app_id=%d", appID)
	}
	if err := i.get(ctx, p, &res); err != nil {
		return nil, err
	}
	for _, cr := range res.CheckRuns {
		if cr.ExternalID == externalID {
			c := cr.CheckRun
			return &c, nil
		}
	}
	return nil, nil
}

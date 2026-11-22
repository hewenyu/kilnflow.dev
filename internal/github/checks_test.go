package github

import (
	"net/http"
	"testing"
	"time"
)

func TestCreateCheckRun(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("POST /repos/o/r/check-runs", jsonReply(http.StatusCreated, map[string]any{"id": 11, "name": "ci / build", "status": "queued"}))
	started := time.Date(2026, 11, 1, 1, 2, 3, 0, time.UTC)
	cr, err := c.Inst(1).CreateCheckRun(t.Context(), "o/r", &CheckRun{
		Name: "ci / build", HeadSHA: "abc", ExternalID: "job-1", DetailsURL: "https://kiln/runs/1",
		Status: CheckStatusInProgress, StartedAt: &started,
		Output: &CheckRunOutput{Title: "t", Summary: "s", Annotations: []Annotation{{Path: "a.go", StartLine: 1, EndLine: 2, AnnotationLevel: "failure", Message: "m"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cr.ID != 11 || cr.Status != CheckStatusQueued {
		t.Errorf("返回值：%+v", cr)
	}
	r := f.only(t)
	body := r.jsonBody(t)
	for k, want := range map[string]any{
		"name": "ci / build", "head_sha": "abc", "external_id": "job-1", "details_url": "https://kiln/runs/1",
		"status": "in_progress", "started_at": "2026-11-01T01:02:03Z",
	} {
		if body[k] != want {
			t.Errorf("请求体 %s = %v，应为 %v", k, body[k], want)
		}
	}
	for _, k := range []string{"id", "conclusion", "completed_at"} {
		if _, ok := body[k]; ok {
			t.Errorf("零值字段 %s 不应发送", k)
		}
	}
	out, _ := body["output"].(map[string]any)
	anns, _ := out["annotations"].([]any)
	if out["title"] != "t" || len(anns) != 1 {
		t.Errorf("output：%v", out)
	}
}

// UpdateCheckRun 不发送 ID 和 head_sha，也不修改调用方的参数。
func TestUpdateCheckRun(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("PATCH /repos/o/r/check-runs/11", jsonReply(http.StatusOK, map[string]any{"id": 11}))
	in := &CheckRun{ID: 99, HeadSHA: "abc", Status: CheckStatusCompleted, Conclusion: "success"}
	if err := c.Inst(1).UpdateCheckRun(t.Context(), "o/r", 11, in); err != nil {
		t.Fatal(err)
	}
	body := f.only(t).jsonBody(t)
	if _, ok := body["id"]; ok {
		t.Error("不应发送 id")
	}
	if _, ok := body["head_sha"]; ok {
		t.Error("不应发送 head_sha")
	}
	if body["status"] != "completed" || body["conclusion"] != "success" {
		t.Errorf("请求体：%v", body)
	}
	if in.ID != 99 || in.HeadSHA != "abc" {
		t.Errorf("调用方的参数被修改：%+v", in)
	}

	// 不存在的 Check Run 返回 404
	if err := c.Inst(1).UpdateCheckRun(t.Context(), "o/r", 12, in); !IsNotFound(err) {
		t.Errorf("应返回 404：%v", err)
	}
}

func TestFindCheckRun(t *testing.T) {
	list := map[string]any{"total_count": 3, "check_runs": []any{
		map[string]any{"id": 1, "name": "ci", "external_id": "other", "app": map[string]any{"id": 42}},
		map[string]any{"id": 2, "name": "ci", "external_id": "want", "status": "completed", "app": map[string]any{"id": 42}},
		map[string]any{"id": 3, "name": "ci", "external_id": "want", "app": map[string]any{"id": 42}},
	}}
	tests := []struct {
		name       string
		externalID string
		appID      int64
		wantID     int64 // 0 表示没有找到
		wantAppID  string
	}{
		{"按 external_id 取第一个", "want", 42, 2, "42"},
		{"没有匹配的 external_id", "missing", 42, 0, "42"},
		{"appID 为 0 时不过滤 App", "want", 0, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c, _ := newFakeAPI(t)
			f.handle("GET /repos/o/r/commits/abc/check-runs", jsonReply(http.StatusOK, list))
			cr, err := c.Inst(1).FindCheckRun(t.Context(), "o/r", "abc", "ci / build & test", tt.externalID, tt.appID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.wantID == 0 && cr != nil:
				t.Errorf("应返回 nil，实际 %+v", cr)
			case tt.wantID != 0 && (cr == nil || cr.ID != tt.wantID || cr.ExternalID != tt.externalID):
				t.Errorf("应返回 ID %d，实际 %+v", tt.wantID, cr)
			}
			q := f.only(t).Query
			if q.Get("check_name") != "ci / build & test" || q.Get("filter") != "all" || q.Get("per_page") != "100" {
				t.Errorf("查询参数：%v", q)
			}
			if got := q.Get("app_id"); got != tt.wantAppID {
				t.Errorf("app_id = %q，应为 %q", got, tt.wantAppID)
			}
		})
	}

	_, c, _ := newFakeAPI(t)
	if _, err := c.Inst(1).FindCheckRun(t.Context(), "o/r", "abc", "ci", "x", 0); !IsNotFound(err) {
		t.Errorf("接口出错时应返回错误：%v", err)
	}
}

package publish

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// TestWorker_publishUploads_unsafeAssetURLs：上传服务返回的 asset URL 全部不安全（被 release.SafeWebURL 拒绝）时，
// detail 中的 assets 必须是空数组而不是 null：null 会让前端 uploadDetailOf 整体解析失败，连带丢掉 latest。
func TestWorker_publishUploads_unsafeAssetURLs(t *testing.T) {
	t.Parallel()
	e := newUploadEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			http.NotFound(w, r)
		case http.MethodPut:
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodPost:
			var req upload.PublishRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			res := upload.PublishResult{Tag: "v1.2.0", PublishedAt: "2026-01-01T00:00:00Z"}
			for _, a := range req.Assets {
				a.URL = "javascript:void(0)"
				res.Assets = append(res.Assets, a)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(res); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	r := e.tagRun("v1.2.0")
	e.builds(r)
	rel := e.uploadRelease(func(rel *workflow.Release) { rel.Uploads[0].URL = srv.URL })
	id := e.uploadJob(r, rel)
	e.publish()

	e.wantJob(id, store.JobSucceeded, "")
	rows := e.wantTargetStates(id, "upload:committed", "github:committed")
	assets, ok := rows[0].Detail["assets"].([]any)
	if !ok || len(assets) != 0 {
		t.Fatalf("没有安全的 asset URL 时 assets 应是空数组而不是 null：%#v", rows[0].Detail["assets"])
	}
	if rows[0].Detail["latest"] != true {
		t.Fatalf("latest 应保留：%#v", rows[0].Detail)
	}
}

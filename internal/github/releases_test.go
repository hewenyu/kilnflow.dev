package github

import (
	"net/http"
	"strings"
	"testing"
)

func TestGetRelease(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/releases/5", jsonReply(http.StatusOK, map[string]any{
		"id": 5, "tag_name": "v1.0.0", "draft": true, "prerelease": false, "html_url": "https://github.com/o/r/releases/tag/v1.0.0",
		"published_at": nil,
		"assets":       []any{map[string]any{"id": 9, "name": "a.tgz", "size": 3, "digest": "sha256:ab", "state": "uploaded"}},
	}))
	rel, err := c.Inst(1).GetRelease(t.Context(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := ReleaseAsset{ID: 9, Name: "a.tgz", Size: 3, Digest: "sha256:ab", State: "uploaded"}
	if rel.ID != 5 || rel.TagName != "v1.0.0" || rel.Draft == nil || !*rel.Draft || rel.Prerelease == nil || *rel.Prerelease ||
		rel.PublishedAt != nil || len(rel.Assets) != 1 || rel.Assets[0] != want {
		t.Errorf("release = %+v", rel)
	}
	if _, err := c.Inst(1).GetRelease(t.Context(), "o/r", 6); !IsNotFound(err) {
		t.Errorf("不存在的 Release 应返回 404：%v", err)
	}
}

// CreateRelease、UpdateRelease 的请求体：指针字段 false 也发送，零值字段不发送。
func TestCreateUpdateRelease(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("POST /repos/o/r/releases", jsonReply(http.StatusCreated, map[string]any{"id": 5, "tag_name": "v1"}))
	f.handle("PATCH /repos/o/r/releases/5", jsonReply(http.StatusOK, map[string]any{"id": 5, "draft": false}))
	no, yes := false, true
	inst := c.Inst(1)

	created, err := inst.CreateRelease(t.Context(), "o/r", &Release{
		TagName: "v1", TargetCommitish: "abc", Name: "v1", Body: "notes", Draft: &yes, Prerelease: &no, GenerateNotes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 5 {
		t.Errorf("CreateRelease = %+v", created)
	}
	updated, err := inst.UpdateRelease(t.Context(), "o/r", 5, &Release{Draft: &no, MakeLatest: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Draft == nil || *updated.Draft {
		t.Errorf("UpdateRelease = %+v", updated)
	}

	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].key() != "POST /repos/o/r/releases" || reqs[1].key() != "PATCH /repos/o/r/releases/5" {
		t.Fatalf("请求：%v", reqs)
	}
	cb := reqs[0].jsonBody(t)
	for k, want := range map[string]any{
		"tag_name": "v1", "target_commitish": "abc", "name": "v1", "body": "notes",
		"draft": true, "prerelease": false, "generate_release_notes": true,
	} {
		if cb[k] != want {
			t.Errorf("创建请求体 %s = %v，应为 %v", k, cb[k], want)
		}
	}
	ub := reqs[1].jsonBody(t)
	if len(ub) != 2 || ub["draft"] != false || ub["make_latest"] != "true" {
		t.Errorf("更新请求体应只有 draft 和 make_latest：%v", ub)
	}
}

func TestListReleaseAssets(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("GET /repos/o/r/releases/5/assets", pagedReply(2, "", func(i int) any {
		return map[string]any{"id": i + 1, "name": []string{"a", "b"}[i]}
	}))
	assets, err := c.Inst(1).ListReleaseAssets(t.Context(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[1].Name != "b" {
		t.Errorf("assets = %+v", assets)
	}
}

func TestDeleteReleaseAsset(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	f.handle("DELETE /repos/o/r/releases/assets/9", rawReply(http.StatusNoContent, ""))
	if err := c.Inst(1).DeleteReleaseAsset(t.Context(), "o/r", 9); err != nil {
		t.Fatal(err)
	}
	if err := c.Inst(1).DeleteReleaseAsset(t.Context(), "o/r", 10); !IsNotFound(err) {
		t.Errorf("不存在的 asset 应返回 404：%v", err)
	}
}

// 上传发到 UploadURL，name 转义，原样发送内容。
func TestUploadReleaseAsset(t *testing.T) {
	f, c, _ := newFakeAPI(t)
	c.UploadURL = f.srv.URL + "/uploads/"
	f.handle("POST /uploads/repos/o/r/releases/5/assets", jsonReply(http.StatusCreated, map[string]any{"id": 9, "name": "kiln linux&amd64.tar.gz", "size": 4, "state": "uploaded"}))
	a, err := c.Inst(1).UploadReleaseAsset(t.Context(), "o/r", 5, "kiln linux&amd64.tar.gz", strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 9 || a.Size != 4 {
		t.Errorf("asset = %+v", a)
	}
	r := f.only(t)
	if r.Query.Get("name") != "kiln linux&amd64.tar.gz" {
		t.Errorf("name = %q", r.Query.Get("name"))
	}
	if string(r.Body) != "data" || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Length") != "4" {
		t.Errorf("上传请求：body=%q Content-Type=%q Content-Length=%q", r.Body, r.Header.Get("Content-Type"), r.Header.Get("Content-Length"))
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "token ") {
		t.Errorf("上传应使用 installation token：%q", r.Header.Get("Authorization"))
	}
}

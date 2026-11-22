package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxReleasePages 是列出 Release、asset 时最多请求的页数。
const maxReleasePages = 20

// Release 是 Release 的创建、更新参数和返回值。
type Release struct {
	ID              int64          `json:"id,omitempty"`
	TagName         string         `json:"tag_name,omitempty"`
	TargetCommitish string         `json:"target_commitish,omitempty"`
	Name            string         `json:"name,omitempty"`
	Body            string         `json:"body,omitempty"`
	Draft           *bool          `json:"draft,omitempty"`
	Prerelease      *bool          `json:"prerelease,omitempty"`
	MakeLatest      string         `json:"make_latest,omitempty"`
	GenerateNotes   bool           `json:"generate_release_notes,omitempty"`
	HTMLURL         string         `json:"html_url,omitempty"`
	Assets          []ReleaseAsset `json:"assets,omitempty"`
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
}

// ReleaseAsset 是 Release 的一个 asset。
type ReleaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // GitHub 为新上传的 asset 提供 "sha256:…"
	State  string `json:"state"`
}

// ListReleases 列出全部 Release（包含 draft）。
func (i *Inst) ListReleases(ctx context.Context, repo string) ([]Release, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/releases", repo), maxReleasePages, unwrapList[Release])
}

// GetRelease 按 ID 读取 Release。
func (i *Inst) GetRelease(ctx context.Context, repo string, id int64) (*Release, error) {
	return call[Release](ctx, i.Do, http.MethodGet, fmt.Sprintf("/repos/%s/releases/%d", repo, id), nil)
}

// CreateRelease 创建 Release。
func (i *Inst) CreateRelease(ctx context.Context, repo string, r *Release) (*Release, error) {
	return call[Release](ctx, i.Do, http.MethodPost, fmt.Sprintf("/repos/%s/releases", repo), r)
}

// UpdateRelease 更新 Release。
func (i *Inst) UpdateRelease(ctx context.Context, repo string, id int64, r *Release) (*Release, error) {
	return call[Release](ctx, i.Do, http.MethodPatch, fmt.Sprintf("/repos/%s/releases/%d", repo, id), r)
}

// ListReleaseAssets 列出 Release 的全部 asset。
func (i *Inst) ListReleaseAssets(ctx context.Context, repo string, id int64) ([]ReleaseAsset, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/releases/%d/assets", repo, id), maxReleasePages, unwrapList[ReleaseAsset])
}

// DeleteReleaseAsset 删除一个 asset。
func (i *Inst) DeleteReleaseAsset(ctx context.Context, repo string, assetID int64) error {
	return i.Do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/releases/assets/%d", repo, assetID), nil, nil)
}

// UploadReleaseAsset 上传 asset。r 实现了 io.Seeker（例如 *os.File）时，
// 遇到 401 可以重置后用新 token 重试；否则不重试。
func (i *Inst) UploadReleaseAsset(ctx context.Context, repo string, releaseID int64, name string, r io.Reader, size int64) (*ReleaseAsset, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s", i.c.uploads(), repo, releaseID, url.QueryEscape(name))
	return call[ReleaseAsset](ctx, i.Do, http.MethodPost, u, rawBody{r: r, size: size, contentType: "application/octet-stream"})
}

package publish

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/semver"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 发布到 HTTP 上传目标（release.uploads，PLAN #13.9，协议见 docs/UPLOAD-PROTOCOL.md）：
// 阶段 A 查询服务端的状态，暂存缺少的文件（PUT，带 Content-Digest）；阶段 B 提交清单（POST publish），服务端原子地对外发布。

// uploadTarget 是 release.uploads 的一项。
type uploadTarget struct {
	w  *Worker
	pj *publishJob
	t  workflow.UploadTarget
	n  int
	c  *upload.Client
	// published 表示服务端已经以完全相同的内容发布过这个 tag（之前的发布 job 已经提交），提交时跳过。
	published bool
}

func (u *uploadTarget) seq() int { return u.n }

// errorf 给错误加上目标名。
func (u *uploadTarget) errorf(format string, args ...any) error {
	return fmt.Errorf("上传目标 %s：%w", u.t.Name, fmt.Errorf(format, args...))
}

func (u *uploadTarget) stage(ctx context.Context, m *manifest) error {
	if len(m.assets) == 0 {
		return u.errorf("没有要上传的文件")
	}
	token, err := substSecrets(u.t.Token, u.pj.secrets)
	if err != nil {
		return u.errorf("token：%w", err)
	}
	if token == "" {
		return u.errorf("token 为空（检查引用的 secret 是否已在 Kiln 中配置）")
	}
	if u.c, err = upload.NewClient(u.t.URL, token, "kiln/"+buildinfo.Version(), u.w.uploadOpts...); err != nil {
		return u.errorf("%w", err)
	}
	rel, err := u.c.Get(ctx, m.tag)
	switch {
	case upload.IsStatus(err, http.StatusMethodNotAllowed) || upload.IsStatus(err, http.StatusNotImplemented):
		u.pj.plog.printf("上传目标 %s 不支持查询，上传全部文件", u.t.Name)
		rel = nil
	case err != nil:
		return u.errorf("查询 %s：%w", m.tag, err)
	}
	if rel != nil && rel.State == upload.StatePublished {
		if d := uploadedDiff(rel.Assets, m.assets); !d.same() {
			return publishedConflict("上传目标 "+u.t.Name, m.tag, d.String())
		}
		u.published = true
		u.pj.plog.printf("上传目标 %s 已经发布过 %s，内容一致，跳过", u.t.Name, m.tag)
		return nil
	}
	have := map[string]upload.Asset{}
	if rel != nil {
		for _, a := range rel.Assets {
			have[a.Name] = a
		}
	}
	for _, a := range m.assets {
		if h, ok := have[a.Name]; ok && h.Size == a.Size && h.SHA256 == a.SHA256 {
			u.pj.plog.printf("上传目标 %s：%s 已暂存且一致，跳过", u.t.Name, a.Name)
			continue
		}
		if err := u.w.checkCancel(ctx, u.pj.job.ID); err != nil {
			return err
		}
		u.pj.plog.printf("上传 %s 到 %s（%s）", a.Name, u.t.Name, bytesize.Format(a.Size))
		path := a.Path
		if _, err := u.c.Put(ctx, m.tag, upload.Asset{Name: a.Name, Size: a.Size, SHA256: a.SHA256}, func() (io.ReadCloser, error) {
			return os.Open(path)
		}); err != nil {
			return u.errorf("上传 %s：%w", a.Name, err)
		}
	}
	return nil
}

func (u *uploadTarget) commit(ctx context.Context, m *manifest) (map[string]any, error) {
	if u.published {
		return map[string]any{"url": u.t.URL, "previous": true}, nil
	}
	pre, err := choice(u.t.Prerelease, func() (bool, error) { return autoPrerelease(m.tag), nil })
	if err != nil {
		return nil, err
	}
	latest, err := choice(u.t.Latest, func() (bool, error) { return u.w.uploadLatest(ctx, u.t.URL, m.tag, pre) })
	if err != nil {
		return nil, err
	}
	req := &upload.PublishRequest{Commit: m.commit, RunID: m.runID, Latest: latest, Prerelease: pre}
	for _, a := range m.assets {
		req.Assets = append(req.Assets, upload.Asset{Name: a.Name, Size: a.Size, SHA256: a.SHA256})
	}
	res, err := u.c.Publish(ctx, m.tag, req)
	if err != nil {
		return nil, u.errorf("提交 %s：%w", m.tag, err)
	}
	if err := auditUpload(ctx, u, m, latest); err != nil {
		return nil, err
	}
	u.pj.plog.printf("已发布到 %s（%d 个文件，latest=%v，prerelease=%v）", u.t.Name, len(req.Assets), latest, pre)
	links := make([]map[string]any, 0, len(res.Assets))
	for _, a := range res.Assets {
		// 下载地址来自外部服务，不可信：只接受 http/https，并且必须有主机名；其他值不作为可点击链接下发。
		if a.URL != "" && release.SafeWebURL(a.URL) {
			links = append(links, map[string]any{"name": a.Name, "url": a.URL})
		}
	}
	return map[string]any{"url": u.t.URL, "latest": latest, "prerelease": pre, "published_at": res.PublishedAt, "assets": links}, nil
}

func auditUpload(ctx context.Context, u *uploadTarget, m *manifest, latest bool) error {
	return store.Audit(ctx, u.w.db, auditActor, "publish.upload", map[string]any{
		"job_id": u.pj.job.ID, "name": u.t.Name, "url": u.t.URL, "tag": m.tag, "assets": len(m.assets), "latest": latest,
	})
}

// choice 按 auto / true / false 取值（GitHub Release 和 HTTP 上传的 prerelease、latest）：auto（或不写）时调用 auto。
func choice(v string, auto func() (bool, error)) (bool, error) {
	switch v {
	case workflow.ChoiceTrue:
		return true, nil
	case workflow.ChoiceFalse:
		return false, nil
	}
	return auto()
}

// autoPrerelease 是 prerelease: auto 的规则：tag 是带预发布段的 semver 版本。
func autoPrerelease(tag string) bool {
	v, ok, _ := semver.IsRelease(tag)
	return ok && v.Pre != ""
}

// uploadLatest 是 latest: auto 的规则（与 GitHub Release 相同）：本版本是正式版本（semver 且没有预发布段），
// 并且不低于 Kiln 向同一个上传地址发布过的所有正式版本。
func (w *Worker) uploadLatest(ctx context.Context, url, tag string, prerelease bool) (bool, error) {
	v, ok, rel := semver.IsRelease(tag)
	if !ok || !rel || prerelease {
		return false, nil
	}
	rows, err := w.db.Query(ctx, `SELECT r.tag FROM release_targets t JOIN releases r ON r.job_id = t.job_id
		WHERE t.kind = $1 AND t.target = $2 AND t.state = 'committed'`, kindUpload, url)
	if err != nil {
		return false, err
	}
	tags, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, other := range tags {
		if o, ok, rel := semver.IsRelease(other); ok && rel && o.Compare(v) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// uploadedDiff 比较服务端已发布的文件与本次要发布的文件（名称、大小、sha256，不看顺序）。上传协议要求服务端返回 sha256，
// 没有返回时按不同处理（不像 GitHub 那样退回只比较大小）。服务端的清单中有重复的名字时，重复项算作多出。
func uploadedDiff(published []upload.Asset, assets []release.Asset) assetDiff {
	ps := make([]publishedAsset, 0, len(published))
	for _, p := range published {
		sum := p.SHA256
		if sum == "" {
			sum = "（没有返回 sha256）"
		}
		ps = append(ps, publishedAsset{Name: p.Name, Size: p.Size, SHA256: sum})
	}
	return diffAssets(ps, assets)
}

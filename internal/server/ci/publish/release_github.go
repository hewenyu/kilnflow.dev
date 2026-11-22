package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/semver"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 发布到 GitHub Release（PLAN #13.4、#13.6）。

// maxAssetSize 是 GitHub Release 单个 asset 的大小上限（2GiB）。
const maxAssetSize = 2 << 30

// githubPublish 是一次发布到 GitHub Release 的上下文。
type githubPublish struct {
	job     *cistore.Job
	run     *cistore.Run
	g       *workflow.ReleaseGitHub
	tag     string
	src     *store.Repo // 源码仓库
	target  *store.Repo // 发布的目标仓库（可以与 src 相同）
	srcInst *gh.Inst
	inst    *gh.Inst // 目标仓库的 installation
	plog    *publishLog
}

// cross 判断是否发布到其他仓库。
func (p *githubPublish) cross() bool { return p.target.ID != p.src.ID }

// newGitHubPublish 解析 release.github 的目标仓库。
func (w *Worker) newGitHubPublish(ctx context.Context, pj *publishJob) (*githubPublish, error) {
	g := pj.job.Spec.Release.GitHub
	target := pj.src
	if g.Repository != "" && !strings.EqualFold(g.Repository, pj.src.FullName()) {
		var err error
		if target, err = store.FindRepo(ctx, w.db, g.Repository); err != nil {
			return nil, fmt.Errorf("目标仓库 %s 不在 App 的安装范围内：%w", g.Repository, err)
		}
		// 规划时已确认目标仓库与源仓库属于同一个 installation（PLAN #13.6）；之后仓库可能被移到别的 installation，
		// 这里再确认一次，不用别的 installation 的 token 发布
		if target.InstallationID != pj.src.InstallationID {
			return nil, fmt.Errorf("目标仓库 %s 与源仓库不在同一个 installation 下", g.Repository)
		}
	}
	return &githubPublish{
		job: pj.job, run: pj.run, g: g, tag: pj.tag, src: pj.src, target: target,
		srcInst: pj.srcInst, inst: w.gh.Inst(target.InstallationID), plog: pj.plog,
	}, nil
}

// githubTarget 是文件类发布目标 GitHub Release（release.github）。
type githubTarget struct {
	w      *Worker
	p      *githubPublish
	n      int
	notes  *releaseNotes
	staged *stagedRelease
}

func (t *githubTarget) seq() int { return t.n }

func (t *githubTarget) stage(ctx context.Context, m *manifest) error {
	rel, err := t.w.stageRelease(ctx, t.p, m.assets, t.notes)
	t.staged = rel
	return err
}

func (t *githubTarget) commit(ctx context.Context, _ *manifest) (map[string]any, error) {
	switch {
	case t.staged.Published:
		return map[string]any{"html_url": t.staged.HTMLURL}, nil
	case t.notes.keepDraft:
		t.p.plog.printf("notes.from 为 draft：Release 保持 draft，请在 GitHub 上编辑说明后手动发布")
		return map[string]any{"html_url": t.staged.HTMLURL, "draft": true}, nil
	}
	return t.w.commitRelease(ctx, t.p, t.staged)
}

// stageRelease 是阶段 A 中 GitHub 的部分：检查 asset，找到或创建 draft Release 并上传 asset。
// 源仓库的 tag 已经在 checkTag 中确认过。本 run 已经正式发布过时返回 Published 为 true，不需要再提交。
func (w *Worker) stageRelease(ctx context.Context, p *githubPublish, assets []release.Asset, notes *releaseNotes) (*stagedRelease, error) {
	for _, a := range assets {
		if a.Size > maxAssetSize {
			return nil, fmt.Errorf("asset %s 超过 2GiB", a.Name)
		}
	}
	if err := w.checkSourceName(ctx, p, assets, notes.body); err != nil {
		return nil, err
	}
	marker := fmt.Sprintf("<!-- kiln-run:%d -->", p.run.ID)
	body := strings.TrimSpace(notes.body + "\n\n" + marker)
	if notes.generate {
		body = marker
	}
	prerelease, err := choice(p.g.Prerelease, func() (bool, error) { return autoPrerelease(p.tag), nil })
	if err != nil {
		return nil, err
	}
	rel, err := w.findOrCreateDraft(ctx, p, &draftSpec{Marker: marker, Body: body, Generate: notes.generate, Prerelease: prerelease})
	if err != nil {
		return nil, err
	}
	if rel.Draft != nil && !*rel.Draft {
		// 正式发布的 Release 不覆盖：内容一致时幂等成功，不一致时失败
		if err := w.checkPublishedAssets(ctx, p, rel.ID, assets); err != nil {
			return nil, err
		}
		p.plog.printf("Release %s 已经由本 run 正式发布，asset 与本次一致：%s", p.tag, rel.HTMLURL)
		return &stagedRelease{ID: rel.ID, HTMLURL: rel.HTMLURL, Published: true}, nil
	}
	if err := w.uploadAssets(ctx, p, rel.ID, assets); err != nil {
		return nil, err
	}
	return &stagedRelease{ID: rel.ID, HTMLURL: rel.HTMLURL, Prerelease: prerelease}, nil
}

// assetUploaded 判断 GitHub 的 asset 是否已经上传完成（state 为 uploaded）；其他状态（starter、uploading 等）都算没有完成。
// 已发布 Release 的比对和跳过已存在的 asset 用同一条规则。
func assetUploaded(state string) bool { return state == "uploaded" }

// checkPublishedAssets 按名字比较已正式发布的 Release 上的 asset 与本次的 asset（大小和 GitHub 返回的 digest）。
// 没有上传完成的 asset（state 不是 uploaded）算作不同；GitHub 没有返回 sha256 digest 的（较早上传的 asset）只比较大小。
func (w *Worker) checkPublishedAssets(ctx context.Context, p *githubPublish, releaseID int64, assets []release.Asset) error {
	existing, err := p.inst.ListReleaseAssets(ctx, p.target.FullName(), releaseID)
	if err != nil {
		return err
	}
	published := make([]publishedAsset, 0, len(existing))
	for _, a := range existing {
		pa := publishedAsset{Name: a.Name, Size: a.Size}
		if sum, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
			pa.SHA256 = sum // 其他算法的 digest 无法与本次的 sha256 比较，按没有 digest 处理（只比较大小）
		}
		if !assetUploaded(a.State) {
			pa.Size = -1 // 没有上传完成：不可能与本次一致
		}
		published = append(published, pa)
	}
	d := diffAssets(published, assets)
	if !d.same() {
		return publishedConflict("GitHub Release "+p.target.FullName(), p.tag, d.String())
	}
	if len(d.Unverified) > 0 {
		p.plog.printf("GitHub 没有返回 %s 的 digest，只比较了大小", strings.Join(d.Unverified, "、"))
	}
	return nil
}

// stagedRelease 是阶段 A 完成后的 draft Release（Published 为 true 时是本 run 已经正式发布过的 Release）。
type stagedRelease struct {
	ID         int64
	HTMLURL    string
	Prerelease bool
	Published  bool
}

// checkSourceName 在从私有仓库发布到公开仓库时做源仓库名检查（PLAN #13.6）。
func (w *Worker) checkSourceName(ctx context.Context, p *githubPublish, assets []release.Asset, notes string) error {
	if !p.src.Private || p.g.AllowSourceName {
		return nil
	}
	tr, err := p.inst.GetRepo(ctx, p.target.FullName())
	if err != nil {
		return fmt.Errorf("读取目标仓库 %s（判断是否公开）：%w", p.target.FullName(), err)
	}
	if tr.Private {
		return nil
	}
	if err := release.SourceNameCheck(p.src.FullName(), assets, notes); err != nil {
		return err
	}
	p.plog.printf("源仓库名检查通过（目标 %s 是公开仓库）", p.target.FullName())
	return nil
}

// uploadAssets 上传 asset：已存在且一致的跳过，不一致的先删除再上传。
func (w *Worker) uploadAssets(ctx context.Context, p *githubPublish, releaseID int64, assets []release.Asset) error {
	existing, err := p.inst.ListReleaseAssets(ctx, p.target.FullName(), releaseID)
	if err != nil {
		return err
	}
	byName := map[string]gh.ReleaseAsset{}
	for _, a := range existing {
		byName[a.Name] = a
	}
	for _, a := range assets {
		if e, ok := byName[a.Name]; ok {
			if e.Size == a.Size && (e.Digest == "" || e.Digest == "sha256:"+a.SHA256) && assetUploaded(e.State) {
				p.plog.printf("asset %s 已存在且一致，跳过", a.Name)
				continue
			}
			if err := p.inst.DeleteReleaseAsset(ctx, p.target.FullName(), e.ID); err != nil {
				return fmt.Errorf("删除旧 asset %s：%w", a.Name, err)
			}
		}
		if err := w.checkCancel(ctx, p.job.ID); err != nil {
			return err
		}
		p.plog.printf("上传 %s（%s）", a.Name, bytesize.Format(a.Size))
		if err := uploadAsset(ctx, p, releaseID, a); err != nil {
			return fmt.Errorf("上传 %s：%w", a.Name, err)
		}
	}
	return nil
}

func uploadAsset(ctx context.Context, p *githubPublish, releaseID int64, a release.Asset) error {
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = p.inst.UploadReleaseAsset(ctx, p.target.FullName(), releaseID, a.Name, f, a.Size)
	return err
}

// commitRelease 是阶段 B：把 draft 正式发布，并按配置设置 latest。
func (w *Worker) commitRelease(ctx context.Context, p *githubPublish, rel *stagedRelease) (map[string]any, error) {
	latest, err := choice(p.g.Latest, func() (bool, error) { return w.isLatest(ctx, p, rel.Prerelease) })
	if err != nil {
		return nil, err
	}
	draft := false
	upd := &gh.Release{Draft: &draft, MakeLatest: strconv.FormatBool(latest)}
	if p.cross() {
		upd.TargetCommitish = cmp.Or(p.g.Target, p.target.DefaultBranch)
	}
	out, err := p.inst.UpdateRelease(ctx, p.target.FullName(), rel.ID, upd)
	if err != nil {
		return nil, fmt.Errorf("发布 Release：%w", err)
	}
	w.releaseStage(ctx, p.job.ID, stageCommit, map[string]any{"html_url": out.HTMLURL})
	p.plog.printf("已发布 %s", out.HTMLURL)
	return map[string]any{"html_url": out.HTMLURL, "latest": latest}, nil
}

// releaseNotes 是 Release 说明的来源。
type releaseNotes struct {
	body      string
	generate  bool // 由 GitHub 生成
	keepDraft bool // 保持 draft，由人工编辑后发布
}

// releaseNotes 按 notes.from 取得 Release 说明。
func (w *Worker) releaseNotes(ctx context.Context, p *githubPublish) (*releaseNotes, error) {
	switch p.g.NotesFrom {
	case "", "github":
		if p.cross() && p.g.NotesFrom == "" {
			return nil, errors.New("跨仓库发布必须显式填写 notes.from")
		}
		return &releaseNotes{generate: true}, nil
	case "draft":
		return &releaseNotes{keepDraft: true}, nil
	}
	data, err := p.srcInst.GetFileContent(ctx, p.src.FullName(), p.g.NotesFrom, p.run.SHA)
	if err != nil {
		return nil, fmt.Errorf("读取说明文件 %s：%w", p.g.NotesFrom, err)
	}
	sec, ok := release.ChangelogSection(string(data), p.tag)
	if !ok {
		return nil, fmt.Errorf("%s 中没有以 %s 开头的二级标题", p.g.NotesFrom, p.tag)
	}
	return &releaseNotes{body: sec}, nil
}

// draftSpec 描述要创建的 draft Release。
type draftSpec struct {
	Marker     string // 写在说明中的 run 标记，用于找回本 run 创建的 Release
	Body       string
	Generate   bool
	Prerelease bool
}

// findOrCreateDraft 找回本 run 已经创建的 Release，或者创建新的 draft Release。
func (w *Worker) findOrCreateDraft(ctx context.Context, p *githubPublish, spec *draftSpec) (*gh.Release, error) {
	repo := p.target.FullName()
	var id *int64
	if err := w.db.QueryRow(ctx, `SELECT github_release_id FROM releases WHERE job_id=$1`, p.job.ID).Scan(&id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if id != nil {
		r, err := p.inst.GetRelease(ctx, repo, *id)
		if err == nil {
			return r, nil
		}
		if !gh.IsNotFound(err) {
			return nil, err
		}
	}
	list, err := p.inst.ListReleases(ctx, repo)
	if err != nil {
		return nil, err
	}
	for i := range list {
		r := &list[i]
		if r.TagName != p.tag {
			continue
		}
		if strings.Contains(r.Body, spec.Marker) {
			if err := w.saveReleaseID(ctx, p.job.ID, r.ID); err != nil {
				return nil, err
			}
			p.plog.printf("找回本 run 的 Release #%d", r.ID)
			return r, nil
		}
		if r.Draft == nil || !*r.Draft {
			return nil, fmt.Errorf("tag %s 在 %s 已有正式发布的 Release（%s），且不是本 run 创建的；请先手动删除", p.tag, repo, r.HTMLURL)
		}
	}
	draft := true
	req := &gh.Release{TagName: p.tag, Name: p.tag, Body: spec.Body, Draft: &draft, Prerelease: &spec.Prerelease, GenerateNotes: spec.Generate}
	if p.cross() {
		req.TargetCommitish = cmp.Or(p.g.Target, p.target.DefaultBranch)
	} else {
		req.TargetCommitish = p.run.SHA
	}
	r, err := p.inst.CreateRelease(ctx, repo, req)
	if err != nil {
		return nil, fmt.Errorf("创建 draft Release：%w", err)
	}
	if err := w.saveReleaseID(ctx, p.job.ID, r.ID); err != nil {
		return nil, err
	}
	p.plog.printf("创建 draft Release #%d（%s）", r.ID, repo)
	return r, nil
}

func (w *Worker) saveReleaseID(ctx context.Context, jobID, releaseID int64) error {
	_, err := w.db.Exec(ctx, `UPDATE releases SET github_release_id=$2 WHERE job_id=$1`, jobID, releaseID)
	return err
}

// isLatest：本版本不是预发布、并且是目标仓库所有正式 Release 中 semver 最大的版本。
func (w *Worker) isLatest(ctx context.Context, p *githubPublish, prerelease bool) (bool, error) {
	v, ok, rel := semver.IsRelease(p.tag)
	if !ok || !rel || prerelease {
		return false, nil
	}
	list, err := p.inst.ListReleases(ctx, p.target.FullName())
	if err != nil {
		return false, err
	}
	for _, r := range list {
		if (r.Draft != nil && *r.Draft) || (r.Prerelease != nil && *r.Prerelease) || r.TagName == p.tag {
			continue
		}
		if o, ok, rel := semver.IsRelease(r.TagName); ok && rel && o.Compare(v) > 0 {
			return false, nil
		}
	}
	return true, nil
}

package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/oci"
	"github.com/hewenyu/kilnflow.dev/internal/semver"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 发布容器镜像（PLAN #13.8）：上游 job 的输出是 OCI image layout（buildctl --output type=oci,tar=false），
// Server 校验后按 OCI Distribution 协议推送：阶段 A 检查不可变 tag、按 digest 推送 blob 和 manifest，
// 阶段 B 设置不可变 tag，再按规则推进浮动 tag。

// registryTimeout 是单个 registry 请求的超时（上传大的 layer 需要较长时间）。
const registryTimeout = 30 * time.Minute

// imagePublish 是 release.images 的一项在本次发布中的状态。
type imagePublish struct {
	target   workflow.ImageTarget
	name     oci.Name
	img      *oci.Image
	client   *oci.Client
	pending  []string // 需要设置的不可变 tag（阶段 A 检查后得到）
	floating []string
	// 实际使用的凭据，记录到 image_publishes（阶段 A 认证时得到）：登录用户名（引用了 secret 时为 nil，不记录）
	// 和 password 所用 secret 的版本（不是恰好一个 secret 引用时为 nil）。Dashboard 据此判断 Docker Hub 连接是否已验证。
	loginUser     *string
	secretVersion *int64
}

// prepareImages 是阶段 0 中镜像的部分：校验配置，读取并校验每个上游输出中的 OCI layout，
// 上游有矩阵时把各变体合并成一个多平台 index。
func (w *Worker) prepareImages(ctx context.Context, pj *publishJob) ([]*imagePublish, error) {
	var out []*imagePublish
	for _, t := range pj.job.Spec.Release.Images {
		ip, err := w.prepareImage(ctx, pj, t)
		if err != nil {
			return nil, fmt.Errorf("镜像 %s：%w", t.Name, err)
		}
		out = append(out, ip)
	}
	return out, nil
}

func (w *Worker) prepareImage(ctx context.Context, pj *publishJob, t workflow.ImageTarget) (*imagePublish, error) {
	name, err := oci.ParseName(t.Name)
	if err != nil {
		return nil, err
	}
	if t.Sign != "" && t.Sign != workflow.SignNone {
		return nil, fmt.Errorf("sign: %s 尚未实现", t.Sign)
	}
	for _, tag := range append(slices.Clone(t.Tags), t.FloatingTags...) {
		if err := oci.CheckTag(tag); err != nil {
			return nil, err
		}
	}
	for _, tag := range t.FloatingTags {
		if slices.Contains(t.Tags, tag) {
			return nil, fmt.Errorf("tag %s 不能同时是不可变 tag 和浮动 tag", tag)
		}
	}
	ups, output, err := w.upstreamJobs(ctx, pj.job, t.From)
	if err != nil {
		return nil, err
	}
	var imgs []*oci.Image
	for _, up := range ups {
		img, err := w.loadImage(ctx, up, output)
		if err != nil {
			return nil, fmt.Errorf("%s.%s：%w", up.DisplayName, output, err)
		}
		imgs = append(imgs, img)
	}
	img, err := oci.Merge(imgs)
	if err != nil {
		return nil, err
	}
	pj.plog.printf("镜像 %s ← %s（%s，%d 个 blob，%s）", name, t.From, imageKind(img), len(img.Blobs), bytesize.Format(img.Size()))
	return &imagePublish{target: t, name: name, img: img, floating: t.FloatingTags}, nil
}

func imageKind(img *oci.Image) string {
	if img.Platform != nil {
		return img.Platform.String()
	}
	return "多平台 index"
}

// loadImage 读取上游 job 的一个输出，作为 OCI image layout 校验。错误信息不含上游的名称，由调用方补充。
func (w *Worker) loadImage(ctx context.Context, up *cistore.Job, output string) (*oci.Image, error) {
	om, err := w.upstreamOutput(ctx, up, output)
	if err != nil {
		return nil, err
	}
	files := map[string]oci.File{}
	for _, f := range om.Files {
		if f.Symlink == "" {
			files[f.Path] = oci.File{Digest: f.Digest, Size: f.Size}
		}
	}
	return oci.LoadLayout(ctx, files, w.openCAS)
}

// openCAS 按 digest 读取 CAS 中的对象（layout 中的 blob 文件名就是内容的 sha256，与 CAS 的 digest 相同）。
func (w *Worker) openCAS(ctx context.Context, digest string) (io.ReadCloser, error) {
	rc, _, err := w.cas.Get(ctx, digest)
	return rc, err
}

// loginUser 返回要记录的登录用户名：username 模板中没有 secret 引用时（值在规划时已经求出，不是机密）才记录。
func loginUser(tmpl, username string) *string {
	if strings.Contains(tmpl, exprOpen) {
		return nil
	}
	return &username
}

// passwordSecretVersion 返回 password 所用 secret 的版本：只有模板恰好是一个 ${{ secrets.NAME }} 时，
// 才能确定登录用的就是这一版 secret；其他写法（拼接、多个引用）不记录。
func passwordSecretVersion(tmpl string, creds map[string]store.ResolvedSecret) *int64 {
	name, ok := strings.CutPrefix(tmpl, secretRefPrefix)
	if !ok {
		return nil
	}
	if name, ok = strings.CutSuffix(name, secretRefSuffix); !ok || strings.ContainsAny(name, "${} ") {
		return nil
	}
	s, ok := creds[name]
	if !ok {
		return nil
	}
	return &s.Version
}

// stageImages 是阶段 A 中镜像的部分：先对全部镜像完成认证和不可变 tag 检查，再按 digest 推送（不打 tag）。
// 失败时返回出错的镜像在 images 中的下标（与某个镜像无关时为 -1）。
func (w *Worker) stageImages(ctx context.Context, pj *publishJob, images []*imagePublish, resolved map[string]store.ResolvedSecret) (int, error) {
	hc := &http.Client{Timeout: registryTimeout}
	creds := secretValues(resolved)
	for i, ip := range images {
		username, err := substSecrets(ip.target.Username, creds)
		if err != nil {
			return i, fmt.Errorf("镜像 %s 的 username：%w", ip.name, err)
		}
		if username == "" {
			return i, fmt.Errorf("镜像 %s 的 username 为空（检查引用的变量是否已在 Kiln 中配置）", ip.name)
		}
		password, err := substSecrets(ip.target.Password, creds)
		if err != nil {
			return i, fmt.Errorf("镜像 %s 的 password：%w", ip.name, err)
		}
		ip.loginUser, ip.secretVersion = loginUser(ip.target.Username, username), passwordSecretVersion(ip.target.Password, resolved)
		ip.client = oci.NewClient(ip.name, username, password, hc)
		if err := ip.client.Ping(ctx); err != nil {
			return i, fmt.Errorf("镜像 %s：%w", ip.name, err)
		}
		pending, err := oci.PendingTags(ctx, ip.client, ip.target.Tags, ip.img.Root.Digest)
		if tc, ok := errors.AsType[*oci.TagConflictError](err); ok {
			return i, publishedConflict("镜像 "+ip.name.String(), tc.Tag, fmt.Sprintf("tag 已指向 %s，本次构建是 %s", tc.Have, tc.Want))
		}
		if err != nil {
			return i, fmt.Errorf("镜像 %s：%w", ip.name, err)
		}
		ip.pending = pending
		for _, t := range ip.target.Tags {
			if !slices.Contains(pending, t) {
				pj.plog.printf("%s:%s 已指向 %s，跳过", ip.name, t, ip.img.Root.Digest)
			}
		}
	}
	for i, ip := range images {
		if err := w.checkCancel(ctx, pj.job.ID); err != nil {
			return -1, err
		}
		st, err := oci.Push(ctx, ip.client, ip.img, w.openCAS)
		if err != nil {
			return i, fmt.Errorf("推送镜像 %s：%w", ip.name, err)
		}
		pj.plog.printf("推送 %s@%s：上传 %d 个 blob（%s），%d 个已存在", ip.name, ip.img.Root.Digest, st.Uploaded, bytesize.Format(st.Bytes), st.Skipped)
	}
	return -1, nil
}

// commitImages 是阶段 B 中镜像的部分：设置不可变 tag，记录版本，再按规则设置浮动 tag。每一步都是幂等的。
// 失败时返回出错的镜像在 images 中的下标。
func (w *Worker) commitImages(ctx context.Context, pj *publishJob, images []*imagePublish) (int, error) {
	var progress []map[string]any
	for i, ip := range images {
		for _, t := range ip.pending {
			if err := oci.SetTag(ctx, ip.client, ip.img, t); err != nil {
				return i, fmt.Errorf("镜像 %s：%w", ip.name, err)
			}
			pj.plog.printf("%s:%s → %s", ip.name, t, ip.img.Root.Digest)
		}
		if _, err := w.db.Exec(ctx, `INSERT INTO image_publishes (name, version, digest, job_id, username, password_secret_version) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (name, version) DO UPDATE SET digest=excluded.digest, job_id=excluded.job_id, updated_at=now(),
				username=excluded.username, password_secret_version=excluded.password_secret_version`,
			ip.name.String(), pj.tag, ip.img.Root.Digest, pj.job.ID, ip.loginUser, ip.secretVersion); err != nil {
			return i, err
		}
		set := slices.Clone(ip.target.Tags)
		if len(ip.floating) > 0 {
			ok, why, err := w.floatingAllowed(ctx, ip.name.String(), pj.tag)
			if err != nil {
				return i, err
			}
			if !ok {
				pj.plog.printf("跳过浮动 tag %s：%s", strings.Join(ip.floating, "、"), why)
			}
			for _, t := range ip.floating {
				if !ok {
					break
				}
				if err := oci.SetTag(ctx, ip.client, ip.img, t); err != nil {
					return i, fmt.Errorf("镜像 %s：%w", ip.name, err)
				}
				set = append(set, t)
				pj.plog.printf("%s:%s → %s", ip.name, t, ip.img.Root.Digest)
			}
		}
		if err := store.Audit(ctx, w.db, auditActor, "publish.image", map[string]any{
			"job_id": pj.job.ID, "name": ip.name.String(), "digest": ip.img.Root.Digest, "tags": set,
		}); err != nil {
			return i, err
		}
		detail := map[string]any{"name": ip.name.String(), "digest": ip.img.Root.Digest, "tags": set}
		progress = append(progress, detail)
		w.setTarget(ctx, pj.job.ID, pj.imageSeq(i), targetCommitted, detail)
	}
	if len(progress) > 0 {
		w.releaseStage(ctx, pj.job.ID, stageCommit, map[string]any{"images": progress})
	}
	return -1, nil
}

// floatingAllowed 判断浮动 tag 是否应该指向本版本（PLAN #13.8）：本版本是正式版本（semver 且没有预发布段），
// 并且是 Kiln 向该镜像名发布过的所有正式版本中最大的。
func (w *Worker) floatingAllowed(ctx context.Context, name, tag string) (bool, string, error) {
	v, ok, rel := semver.IsRelease(tag)
	switch {
	case !ok:
		return false, tag + " 不是 semver 版本", nil
	case !rel:
		return false, tag + " 是预发布版本", nil
	}
	rows, err := w.db.Query(ctx, `SELECT version FROM image_publishes WHERE name=$1`, name)
	if err != nil {
		return false, "", err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, "", err
	}
	for _, other := range versions {
		if o, ok, rel := semver.IsRelease(other); ok && rel && o.Compare(v) > 0 {
			return false, "已经发布过更高的正式版本 " + other, nil
		}
	}
	return true, "", nil
}

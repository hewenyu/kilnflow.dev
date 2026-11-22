package publish

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/expr"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/semver"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 发布到 npm（release.npm，PLAN #27.3）：registry、scope、token 来自账号级 npm 配置（kiln npm set），
// 每个 npm 目标的每个上游变体发布一个包 @<scope>/<name>。
// 阶段 0 打包（或读取构建 job 产出的 .tgz）；阶段 A 查询 registry：版本不存在 → 待发布，已存在且 integrity 一致 → 已完成，
// 不一致 → 失败；阶段 B 最先提交（npm 的版本不可删除重来），结果不确定时先查询再决定。

// npmPublish 是一个发布 job 中的全部 npm 包。
type npmPublish struct {
	acct *store.NpmAccount
	pkgs []*npmPackage
	c    *npm.Client
}

// npmPackage 是要发布的一个 npm 包（一个 npm 目标的一个上游变体）。
type npmPackage struct {
	t      workflow.NpmTarget
	up     *cistore.Job
	output string
	seq    int
	name   string // 完整包名 @scope/name
	// version 是 npm 版本（tag 去掉 v）。
	version string
	// distTag 是求值后的 dist-tag（可能是 auto，在阶段 A 确定）。
	distTag string
	// 阶段 0 之后
	asset    *release.Asset
	manifest map[string]any
	tarball  []byte
	// 阶段 A 之后
	tag       string // 实际使用的 dist-tag
	published bool   // registry 中已经有内容相同的这个版本
	// tarballURL 是 registry 为这个版本返回的 dist.tarball（registry 中已经有这个版本时），记录在发布结果中。
	tarballURL string
}

// prepareNpm 是阶段 0 之前的准备：读取账号级 npm 配置（不解密 token），按上游变体求出每个包的完整名称。
// 没有 release.npm 时返回 nil。
func (w *Worker) prepareNpm(ctx context.Context, pj *publishJob) (*npmPublish, error) {
	targets := pj.job.Spec.Release.Npm
	if len(targets) == 0 {
		return nil, nil
	}
	acct, err := store.GetNpmAccount(ctx, w.db, pj.src.OwnerID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, fmt.Errorf("账号 %s 没有配置 npm（release.npm 需要先执行 kiln npm set --account %s --scope @<scope>）", pj.src.Owner, pj.src.Owner)
	}
	np := &npmPublish{acct: acct}
	seen := map[string]bool{}
	for _, t := range targets {
		ups, output, err := w.upstreamJobs(ctx, pj.job, t.From)
		if err != nil {
			return nil, err
		}
		for _, up := range ups {
			ectx := upstreamContext(up, pj.run)
			short, err := expr.Interpolate(t.Name, ectx)
			if err != nil {
				return nil, fmt.Errorf("release.npm 的 name：%w", err)
			}
			if err := workflow.CheckNpmName(short); err != nil {
				return nil, fmt.Errorf("release.npm（%s）：%w", up.DisplayName, err)
			}
			name := npm.FullName(acct.Scope, short)
			if err := npm.ValidName(name); err != nil {
				return nil, fmt.Errorf("release.npm（%s）：%w", up.DisplayName, err)
			}
			if seen[name] {
				return nil, fmt.Errorf("npm 包名重复：%s（用 name 引用 matrix 区分各变体）", name)
			}
			seen[name] = true
			tag := cmp.Or(t.DistTag, workflow.NpmDistTagAuto)
			if tag, err = expr.Interpolate(tag, ectx); err != nil {
				return nil, fmt.Errorf("release.npm 的 dist-tag：%w", err)
			}
			if tag != workflow.NpmDistTagAuto {
				if err := workflow.CheckNpmDistTag(tag); err != nil {
					return nil, fmt.Errorf("release.npm（%s）：%w", name, err)
				}
			}
			np.pkgs = append(np.pkgs, &npmPackage{t: t, up: up, output: output, seq: len(np.pkgs), name: name, distTag: tag})
		}
	}
	return np, nil
}

// upstreamContext 是按上游变体求值的表达式上下文（asset 的 name、npm 包的 name、dist-tag 和 package 共用）：matrix 和 github。
func upstreamContext(up *cistore.Job, run *cistore.Run) expr.Context {
	m := maps.Clone(up.Matrix)
	if m == nil {
		m = map[string]any{}
	}
	return expr.Context{"matrix": expr.Lenient(m), "github": expr.Lenient(run.GitHubCtx.Map())}
}

// npmVersion 返回 tag 对应的 npm 版本（去掉 v）；tag 不是完整的语义化版本时报错。
func npmVersion(tag string) (string, error) {
	if _, ok, _ := semver.IsRelease(tag); !ok {
		return "", fmt.Errorf("tag %s 不是语义化版本（vX.Y.Z），不能发布到 npm", tag)
	}
	if strings.Contains(tag, "+") {
		// npm 会去掉构建元数据，registry 中的版本号与 tag 对不上（重跑时无法确认是否已发布）
		return "", fmt.Errorf("tag %s 带构建元数据（+…），npm 版本不支持，请用不带 + 的 tag", tag)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// packNpm 是阶段 0：生成或读取每个包的 tarball，检查包名和版本。
func (w *Worker) packNpm(ctx context.Context, pj *publishJob, np *npmPublish, work string, mtime time.Time) error {
	version, err := npmVersion(pj.tag)
	if err != nil {
		return err
	}
	for _, p := range np.pkgs {
		p.version = version
		if err := w.packNpmPackage(ctx, pj, p, version, work, mtime); err != nil {
			return fmt.Errorf("npm 包 %s：%w", p.name, err)
		}
		b, err := os.ReadFile(p.asset.Path)
		if err != nil {
			return err
		}
		p.tarball = b
		pj.plog.printf("打包 npm 包 %s@%s ← %s.%s（%s）", p.name, version, p.up.DisplayName, p.output, bytesize.Format(p.asset.Size))
	}
	return nil
}

func (w *Worker) packNpmPackage(ctx context.Context, pj *publishJob, p *npmPackage, version, work string, mtime time.Time) error {
	om, err := w.upstreamOutput(ctx, p.up, p.output)
	if err != nil {
		return fmt.Errorf("%s.%s：%w", p.up.DisplayName, p.output, err)
	}
	dir, err := os.MkdirTemp(work, "npm-")
	if err != nil {
		return err
	}
	if len(om.Files) == 1 && om.Files[0].Symlink == "" && strings.HasSuffix(om.Files[0].Path, ".tgz") {
		// 构建 job 用 pnpm pack 等产出的包：原样发布
		if len(p.t.Files) > 0 || len(p.t.Package) > 0 {
			return errors.New("输出是 .tgz（构建 job 打好的包）时不能写 files、package")
		}
		tgz, err := w.extractFile(ctx, om.Files[0], dir)
		if err != nil {
			return err
		}
		tb, err := release.ReadNpmTarball(tgz, dir)
		if err != nil {
			return err
		}
		if got, _ := tb.Manifest["name"].(string); got != p.name {
			return fmt.Errorf("包内 package.json 的 name 是 %q，应为 %q", got, p.name)
		}
		if got, _ := tb.Manifest["version"].(string); got != version {
			return fmt.Errorf("包内 package.json 的 version 是 %q，应等于 tag 去掉 v：%s", got, version)
		}
		if npm.IsPrivate(tb.Manifest) {
			return npm.ErrPrivate
		}
		p.asset, p.manifest = &tb.Asset, tb.Manifest
		return nil
	}
	files, base, err := w.selectNpmFiles(ctx, pj, p, om.Files)
	if err != nil {
		return err
	}
	manifest, err := npmManifest(base, p, version, upstreamContext(p.up, pj.run))
	if err != nil {
		return err
	}
	if err := checkNotPrivate(manifest); err != nil {
		return err
	}
	pkg, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	pkg = append(pkg, '\n')
	name := strings.ReplaceAll(strings.TrimPrefix(p.name, "@"), "/", "-") + "-" + version
	a, err := release.PackNpm(ctx, w.cas, release.NpmPackRequest{Name: name, Files: files, Package: pkg, WorkDir: dir, ModTime: mtime})
	if err != nil {
		return err
	}
	p.asset, p.manifest = a, manifest
	return nil
}

// checkNotPrivate 在打包前拒绝 "private": true 的 package.json（合并 release.npm 的 package 之后）。
func checkNotPrivate(manifest map[string]any) error {
	if npm.IsPrivate(manifest) {
		return npm.ErrPrivate
	}
	return nil
}

// selectNpmFiles 按 files 从输出中选出放进包里的文件；输出根目录的 package.json 被选中时作为基础，不作为普通文件。
// 某个模式没有匹配任何文件只在日志中提示；一个文件都没选中则失败。
func (w *Worker) selectNpmFiles(ctx context.Context, pj *publishJob, p *npmPackage, all []cas.FileEntry) ([]cas.FileEntry, map[string]any, error) {
	var out []cas.FileEntry
	var base map[string]any
	matched := make([]bool, len(p.t.Files))
	for _, fe := range all {
		if len(p.t.Files) > 0 {
			hit := false
			for i, pat := range p.t.Files {
				if ok, _ := doublestar.Match(pat, fe.Path); ok {
					matched[i], hit = true, true
				}
			}
			if !hit {
				continue
			}
		}
		if fe.Path == "package.json" && fe.Symlink == "" {
			b, err := w.readSmallFile(ctx, fe)
			if err != nil {
				return nil, nil, err
			}
			if err := json.Unmarshal(b, &base); err != nil {
				return nil, nil, fmt.Errorf("输出中的 package.json 不是合法的 JSON：%w", err)
			}
			if base == nil {
				return nil, nil, errors.New("输出中的 package.json 不是 JSON 对象")
			}
			continue
		}
		out = append(out, fe)
	}
	for i, ok := range matched {
		if !ok {
			pj.plog.printf("npm 包 %s：files 中的 %s 在 %s.%s 中没有匹配的文件", p.name, p.t.Files[i], p.up.DisplayName, p.output)
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%s.%s 中没有要放进包里的文件（检查 files）", p.up.DisplayName, p.output)
	}
	return out, base, nil
}

// npmManifest 生成 package.json：输出中的 package.json 为基础，合并 release.npm[].package（字符串按上游变体求值），
// 最后写入 name 和 version。
func npmManifest(base map[string]any, p *npmPackage, version string, ectx expr.Context) (map[string]any, error) {
	out := map[string]any{}
	maps.Copy(out, base)
	extra, err := interpolateJSON(p.t.Package, ectx)
	if err != nil {
		return nil, fmt.Errorf("release.npm 的 package：%w", err)
	}
	if m, ok := extra.(map[string]any); ok {
		maps.Copy(out, m)
	}
	out["name"], out["version"] = p.name, version
	return out, nil
}

// interpolateJSON 对 YAML 解析出的值中的每个字符串求值。
func interpolateJSON(v any, ectx expr.Context) (any, error) {
	switch x := v.(type) {
	case string:
		return expr.Interpolate(x, ectx)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := interpolateJSON(e, ectx)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := interpolateJSON(e, ectx)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// maxPackageJSON 是输出中 package.json 的大小上限。
const maxPackageJSON = 1 << 20

// readSmallFile 读取 CAS 中的一个小文件（输出中的 package.json）。
func (w *Worker) readSmallFile(ctx context.Context, fe cas.FileEntry) ([]byte, error) {
	if fe.Size > maxPackageJSON {
		return nil, fmt.Errorf("%s 太大（%s）", fe.Path, bytesize.Format(fe.Size))
	}
	rc, err := w.openCAS(ctx, fe.Digest)
	if err != nil {
		return nil, fmt.Errorf("读取 %s：%w", fe.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxPackageJSON+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s：%w", fe.Path, err)
	}
	if len(b) > maxPackageJSON {
		return nil, fmt.Errorf("%s 太大", fe.Path)
	}
	return b, nil
}

// extractFile 把 CAS 中的一个文件写到 dir 下（文件名取包内路径的最后一段），返回路径。
func (w *Worker) extractFile(ctx context.Context, fe cas.FileEntry, dir string) (string, error) {
	rc, err := w.openCAS(ctx, fe.Digest)
	if err != nil {
		return "", fmt.Errorf("读取 %s：%w", fe.Path, err)
	}
	defer rc.Close()
	dst := filepath.Join(dir, path.Base(fe.Path))
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	_, err = f.ReadFrom(rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return dst, err
}

// npmClient 创建 registry 客户端（token 在阶段 A 解密后才有）。registry、scope 和 token 必须来自准备时读取的
// 同一版配置：包名按准备时的 scope 求出，期间配置被修改（例如换了 registry 和 token）时停止，
// 不能把新的 token 发给旧的 registry。
func (w *Worker) npmClient(np *npmPublish, nc *store.NpmCredentials) error {
	if nc.Version != np.acct.Version || nc.Registry != np.acct.Registry || nc.Scope != np.acct.Scope {
		return fmt.Errorf("账号 %s 的 npm 配置在发布过程中被修改（%s %s → %s %s），停止发布；重跑该 job 按新的配置发布",
			nc.Account, np.acct.Scope, np.acct.Registry, nc.Scope, nc.Registry)
	}
	c, err := npm.NewClient(nc.Registry, nc.Token, "kiln/"+buildinfo.Version(), w.npmOpts...)
	if err != nil {
		return err
	}
	np.c = c
	return nil
}

// stageNpm 是阶段 A：查询每个包的这个版本是否已经发布，确定 dist-tag，做源仓库名检查。返回出错的包的 seq。
func (w *Worker) stageNpm(ctx context.Context, pj *publishJob, np *npmPublish) (int, error) {
	for _, p := range np.pkgs {
		if err := w.stageNpmPackage(ctx, pj, np, p); err != nil {
			return p.seq, fmt.Errorf("npm 包 %s：%w", p.name, err)
		}
	}
	return -1, nil
}

func (w *Worker) stageNpmPackage(ctx context.Context, pj *publishJob, np *npmPublish, p *npmPackage) error {
	version := p.version
	doc, err := np.c.Get(ctx, p.name)
	if err != nil {
		return fmt.Errorf("查询 registry：%w", err)
	}
	integrity := npm.Integrity(p.tarball)
	if doc != nil {
		if v, ok := doc.Versions[version]; ok {
			if v.Dist.Integrity != integrity {
				return publishedConflict("", version, fmt.Sprintf("registry 中 integrity %s，本次 %s", v.Dist.Integrity, integrity))
			}
			p.published, p.tarballURL = true, v.Dist.Tarball
			pj.plog.printf("npm 包 %s@%s 已经发布过，内容一致，跳过", p.name, version)
		}
	}
	p.tag = p.distTag
	if p.tag == workflow.NpmDistTagAuto {
		p.tag = autoDistTag(version, doc)
	}
	if !pj.src.Private || p.t.AllowSourceName {
		return nil
	}
	public, err := npmPublic(ctx, np, p, doc != nil)
	if err != nil || !public {
		return err
	}
	if err := release.SourceNameCheck(pj.src.FullName(), []release.Asset{*p.asset}, ""); err != nil {
		return err
	}
	pj.plog.printf("源仓库名检查通过（%s 是公开包）", p.name)
	return nil
}

// npmPublic 判断这次发布的内容是否公开（决定是否做源仓库名检查）。access 只决定新包的可见性（与 npm publish --access 相同）：
// 包已经存在时，registry 沿用它原来的可见性，已经公开的包写 access: restricted 发布的新版本同样公开，
// 所以 restricted 的已有包要按匿名用户能否看到判断。
func npmPublic(ctx context.Context, np *npmPublish, p *npmPackage, exists bool) (bool, error) {
	if p.t.Access == workflow.NpmAccessPublic {
		return true, nil
	}
	if !exists {
		return false, nil
	}
	public, err := np.c.Public(ctx, p.name)
	if err != nil {
		return false, fmt.Errorf("查询 %s 是否公开（access: restricted 不改变已有包的可见性）：%w", p.name, err)
	}
	return public, nil
}

// autoDistTag 是 dist-tag: auto 的规则（PLAN #13.7）：预发布 → next；正式版本且不低于该包已发布的所有正式版本 → latest；
// 其他正式版本（如 2.x 之后发布的 1.x 补丁）→ latest-<主版本号>。
func autoDistTag(version string, doc *npm.Packument) string {
	v, ok := semver.Parse(version)
	if !ok || v.Pre != "" {
		return "next"
	}
	if doc != nil {
		for other := range doc.Versions {
			if o, ok, rel := semver.IsRelease(other); ok && rel && o.Compare(v) > 0 {
				return "latest-" + strconv.Itoa(v.Major)
			}
		}
	}
	return "latest"
}

// npmTarballURL 返回发布结果中记录的下载地址：以 registry 返回的 dist.tarball 为准（下载路径不一定是 npmjs 的格式）。
// 刚发布的版本再查询一次；查询失败或还查不到（registry 的缓存可能稍有延迟）时退回 npmjs 格式的地址并在日志中说明，
// 不影响发布结果。
func (w *Worker) npmTarballURL(ctx context.Context, pj *publishJob, np *npmPublish, p *npmPackage) string {
	if httpURL(p.tarballURL) {
		return p.tarballURL
	}
	fallback := np.c.TarballURL(p.name, p.version)
	doc, err := np.c.Get(ctx, p.name)
	var v npm.Version
	ok := false
	if err == nil && doc != nil {
		v, ok = doc.Versions[p.version]
	}
	switch {
	case err != nil:
		pj.plog.printf("查询 npm 包 %s 的下载地址失败（%v），记录为 %s", p.name, err, fallback)
	case !ok || !httpURL(v.Dist.Tarball) || v.Dist.Integrity != npm.Integrity(p.tarball):
		pj.plog.printf("registry 中暂时查不到 %s@%s 的下载地址，记录为 %s", p.name, p.version, fallback)
	default:
		return v.Dist.Tarball
	}
	return fallback
}

// npmVersionExists 判断 Publish 的错误是否可能是"版本已经存在"：npmjs 返回 403，Verdaccio 等返回 409。
// 403 也可能是没有权限，所以只作为"先查询再决定"的依据，查询不到这个版本时仍按原错误失败。
func npmVersionExists(err error) bool {
	return npm.IsStatus(err, http.StatusForbidden) || npm.IsStatus(err, http.StatusConflict)
}

// httpURL 判断 s 是不是 http(s) 的绝对地址（registry 返回的 dist.tarball 只按这种地址记录）。
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// commitNpm 是阶段 B 的第一步：发布每个包。返回出错的包的 seq。
func (w *Worker) commitNpm(ctx context.Context, pj *publishJob, np *npmPublish) (int, error) {
	for _, p := range np.pkgs {
		if err := w.commitNpmPackage(ctx, pj, np, p); err != nil {
			return p.seq, fmt.Errorf("npm 包 %s：%w", p.name, err)
		}
		version := p.version
		w.setTarget(ctx, pj.job.ID, p.seq, targetCommitted, map[string]any{
			"package": p.name, "version": version, "dist_tag": p.tag, "integrity": npm.Integrity(p.tarball),
			"tarball": w.npmTarballURL(ctx, pj, np, p), "access": p.t.Access, "previous": p.published,
		})
	}
	return -1, nil
}

func (w *Worker) commitNpmPackage(ctx context.Context, pj *publishJob, np *npmPublish, p *npmPackage) error {
	version := p.version
	if p.published {
		return nil
	}
	// 阶段 B 不再中断（PLAN #13.4）：取消在阶段 A 检查；进入提交阶段后必须按顺序完成不可回滚的发布步骤。
	err := np.c.Publish(ctx, &npm.PublishRequest{
		Name: p.name, Version: version, Manifest: p.manifest, Tarball: p.tarball, DistTag: p.tag, Access: p.t.Access,
	})
	if err != nil {
		uncertain := npm.Uncertain(err)
		if !uncertain && !npmVersionExists(err) {
			return fmt.Errorf("发布：%w", err)
		}
		// 请求可能已经生效（超时、5xx），或者 registry 说这个版本已经存在（阶段 A 的查询可能读到 registry 稍旧的数据，
		// 例如上一次发布之后 Server 中断、马上重跑）：查询后再决定
		doc, gerr := np.c.Get(ctx, p.name)
		if gerr != nil {
			if !uncertain {
				return fmt.Errorf("发布：%w", err)
			}
			return fmt.Errorf("发布结果不确定（%w），查询也失败：%w", err, gerr)
		}
		var v npm.Version
		ok := false
		if doc != nil {
			v, ok = doc.Versions[version]
		}
		switch {
		case !ok:
			return fmt.Errorf("发布：%w", err)
		case v.Dist.Integrity != npm.Integrity(p.tarball):
			return fmt.Errorf("发布结果不确定（%w），registry 中的 %s 内容不同", err, version)
		}
		p.tarballURL = v.Dist.Tarball
		pj.plog.printf("npm 包 %s@%s 的发布请求返回错误，但 registry 中已经是本次的内容", p.name, version)
	}
	pj.plog.printf("已发布 npm 包 %s@%s（dist-tag %s，%s）", p.name, version, p.tag, p.t.Access)
	return store.Audit(ctx, w.db, auditActor, "publish.npm", map[string]any{
		"job_id": pj.job.ID, "package": p.name, "version": version, "dist_tag": p.tag, "access": p.t.Access,
		"integrity": npm.Integrity(p.tarball), "registry": np.acct.Registry,
	})
}

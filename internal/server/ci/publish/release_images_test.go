package publish

import (
	"errors"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/oci/ocitest"
	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// ---------- 发布容器镜像（用 internal/oci/ocitest 的内存 registry） ----------

const (
	registryUser  = "kiln-bot"
	registryToken = "dckr_pat_s3cr3t-token-value"
	imageRepo     = "team/kiln"
	dockerEnv     = "docker-release"
)

// imageEnv 是镜像发布的测试环境：Bearer 认证的 registry，账号级 environment docker-release
// （ref 策略 refs/tags/v*，secret DOCKERHUB_TOKEN 是 registry 的 token）。
type imageEnv struct {
	*pubEnv
	reg   *ocitest.Registry
	envID int64
	last  *testRun // release 最近创建的 run
}

func newImageEnv(t *testing.T) *imageEnv {
	t.Helper()
	e := &imageEnv{pubEnv: newPubEnv(t), reg: ocitest.New(ocitest.AuthBearer, registryUser, registryToken)}
	t.Cleanup(e.reg.Close)
	e.envID = e.addEnvironment(nil, `["refs/tags/v*"]`)
	e.setSecret(store.ScopeEnvironment, e.envID, "DOCKERHUB_TOKEN", registryToken)
	return e
}

// addEnvironment 创建 environment docker-release：repoID 为 nil 时是账号级。返回 environment ID。
func (e *imageEnv) addEnvironment(repoID *int64, refPolicy string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.t.Context(), `INSERT INTO environments (owner_id, repo_id, name, ref_policy) VALUES ($1, $2, $3, $4) RETURNING id`,
		ownerID, repoID, dockerEnv, refPolicy).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// setSecret 在作用域中保存（或替换）一个 secret（与 admin 相同的信封加密）。
func (e *pubEnv) setSecret(kind store.ScopeKind, scopeID int64, name, value string) {
	e.t.Helper()
	sealed, err := secretbox.Seal(e.master, secretbox.AAD(string(kind), scopeID, name), []byte(value))
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO secrets (scope_kind, scope_id, name, ciphertext, wrapped_dek) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (scope_kind, scope_id, name) DO UPDATE SET ciphertext=EXCLUDED.ciphertext, wrapped_dek=EXCLUDED.wrapped_dek, version=nextval('secret_versions')`,
		kind, scopeID, name, sealed.Ciphertext, sealed.WrappedDEK)
}

// imageBuild 在 run 中插入构建 job image：输出 image 是 OCI image layout。layouts 有多个时是矩阵的各变体（键是 arch）。
func (e *imageEnv) imageBuild(r *testRun, layouts map[string]*ocitest.Layout) {
	e.t.Helper()
	for arch, l := range layouts {
		files := map[string]upstreamFile{}
		for p, b := range l.Files {
			files[p] = upstreamFile{content: string(b)}
		}
		var matrix map[string]any
		if len(layouts) > 1 {
			matrix = map[string]any{"arch": arch}
		}
		e.upstream(r, "image", "image", matrix, files)
	}
}

// imageJob 插入发布 image.image 到 <registry>/team/kiln 的发布 job：不可变 tag 是 run 的 tag，浮动 tag 是 latest，
// 用户名是字面量，密码来自 secret。
func (e *imageEnv) imageJob(r *testRun, opts ...func(*workflow.ImageTarget)) int64 {
	e.t.Helper()
	target := workflow.ImageTarget{
		From: "image.image", Name: e.reg.Host + "/" + imageRepo, Tags: []string{r.tag}, FloatingTags: []string{"latest"},
		Username: registryUser, Password: "${{ secrets.DOCKERHUB_TOKEN }}", Sign: workflow.SignNone,
	}
	for _, f := range opts {
		f(&target)
	}
	return e.publishJob(r, workflow.Release{Images: []workflow.ImageTarget{target}}, func(s *workflow.Spec) {
		s.Environment, s.Secrets = dockerEnv, []string{"DOCKERHUB_TOKEN"}
	})
}

// release 打 tag、构建一个单平台镜像（内容为 tag）并发布，返回发布 job ID 和镜像的 digest。
// 最近一次的 run 记在 e.last 中。
func (e *imageEnv) release(tag string, opts ...func(*workflow.ImageTarget)) (int64, string) {
	e.t.Helper()
	r := e.tagRun(tag)
	e.last = r
	l := ocitest.NewLayout("linux", "arm64", tag)
	e.imageBuild(r, map[string]*ocitest.Layout{"arm64": l})
	id := e.imageJob(r, opts...)
	e.publish()
	return id, l.Root
}

// wantTags 检查 registry 中 tag 指向的 digest（空字符串表示 tag 不存在）。
func (e *imageEnv) wantTags(want map[string]string) {
	e.t.Helper()
	for tag, d := range want {
		if got := e.reg.Tag(imageRepo, tag); got != d {
			e.t.Errorf("%s:%s 指向 %q，期望 %q", imageRepo, tag, got, d)
		}
	}
}

// publishedImage 是 image_publishes 中的一条记录。
type publishedImage struct {
	Digest        string
	Username      *string
	SecretVersion *int64
}

func (e *imageEnv) imagePublish(version string) (publishedImage, bool) {
	e.t.Helper()
	var p publishedImage
	err := e.db.QueryRow(e.t.Context(), `SELECT digest, username, password_secret_version FROM image_publishes WHERE name=$1 AND version=$2`,
		e.reg.Host+"/"+imageRepo, version).Scan(&p.Digest, &p.Username, &p.SecretVersion)
	if err != nil {
		return p, false
	}
	return p, true
}

// secretVersion 返回 environment 中 DOCKERHUB_TOKEN 当前的版本。
func (e *imageEnv) secretVersion() int64 {
	e.t.Helper()
	var v int64
	if err := e.db.QueryRow(e.t.Context(), `SELECT version FROM secrets WHERE scope_kind='environment' AND scope_id=$1 AND name='DOCKERHUB_TOKEN'`, e.envID).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// 镜像发布的完整过程（PLAN #13.8）：凭据来自 environment 的 secret，按 digest 推送、设置不可变 tag，
// 浮动 tag 只指向已发布的最高正式版本；重跑幂等；不可变 tag 已指向其他 digest 时失败且不改动任何 tag。
func TestWorker_publishImages(t *testing.T) {
	t.Parallel()
	e := newImageEnv(t)

	// 1. 第一个正式版本：不可变 tag 和 latest 都指向它，记录实际使用的凭据
	id, v1 := e.release("v1.0.0")
	e.wantJob(id, store.JobSucceeded, "")
	e.wantTags(map[string]string{"v1.0.0": v1, "latest": v1})
	p, ok := e.imagePublish("v1.0.0")
	if !ok || p.Digest != v1 || p.Username == nil || *p.Username != registryUser || p.SecretVersion == nil || *p.SecretVersion != e.secretVersion() {
		t.Fatalf("image_publishes 应记录 digest、登录用户名和 secret 版本：%+v %v", p, ok)
	}
	row := e.releaseRow(id)
	if row.State != "succeeded" || row.Stage != stageDone || row.Target != e.reg.Host+"/"+imageRepo || row.Progress["images"] == nil {
		t.Fatalf("发布进度：%+v", row)
	}
	var audits int
	if err := e.db.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor=$1 AND action IN ('publish.credentials', 'publish.image')`, auditActor).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("应审计凭据的使用和镜像的发布：%d %v", audits, err)
	}
	if n := len(e.fake.Requests()); n == 0 || len(e.releases(srcRepo)) != 0 {
		t.Fatal("只发布镜像时应确认 tag，但不创建 GitHub Release")
	}
	for _, h := range e.reg.AuthorizationHeaders() {
		if strings.Contains(h, registryToken) {
			t.Fatal("token 只应用于换取 Bearer token，不应直接出现在 registry 请求中")
		}
	}

	// 2. 只重跑发布 job：tag 已指向同一个 digest，幂等成功
	again := e.rerun(e.last)
	e.publish()
	e.wantJob(again, store.JobSucceeded, "")
	if l := e.log(again); !strings.Contains(l, "已指向 "+v1+"，跳过") {
		t.Fatalf("重跑应跳过已设置的 tag：\n%s", l)
	}

	// 3. 旧版本的补丁：不可变 tag 正常设置，latest 不回退
	id, v09 := e.release("v0.9.0")
	e.wantJob(id, store.JobSucceeded, "")
	e.wantTags(map[string]string{"v0.9.0": v09, "latest": v1})
	if l := e.log(id); !strings.Contains(l, "已经发布过更高的正式版本 v1.0.0") {
		t.Fatalf("应说明为什么不更新 latest：\n%s", l)
	}

	// 4. 预发布版本：latest 不变
	id, rc := e.release("v1.1.0-rc.1")
	e.wantJob(id, store.JobSucceeded, "")
	e.wantTags(map[string]string{"v1.1.0-rc.1": rc, "latest": v1})
	if l := e.log(id); !strings.Contains(l, "是预发布版本") {
		t.Fatalf("应说明预发布版本不更新 latest：\n%s", l)
	}

	// 5. 不可变 tag 已指向其他 digest：失败，什么都不改
	other := e.reg.SetTag(imageRepo, "v2.0.0", []byte(`{"schemaVersion":2,"other":true}`))
	id, _ = e.release("v2.0.0")
	e.wantJob(id, store.JobFailed, "已指向 "+other)
	e.wantTags(map[string]string{"v2.0.0": other, "latest": v1})
	if _, ok := e.imagePublish("v2.0.0"); ok {
		t.Fatal("失败的发布不应留下记录")
	}

	// 6. 更高的正式版本：latest 推进
	id, v12 := e.release("v1.2.0")
	e.wantJob(id, store.JobSucceeded, "")
	e.wantTags(map[string]string{"v1.2.0": v12, "latest": v12})
}

// 发布凭据：只在 environment 存在、ref 策略允许时解密；缺少 secret 时失败；错误信息中不出现密码。
// 这些检查都在访问 registry 之前完成。
func TestWorker_publishImages_credentials(t *testing.T) {
	t.Parallel()
	const wrong = "dckr_pat_wr0ng-value-zzz"
	tests := []struct {
		name       string
		setup      func(e *imageEnv)
		target     func(*workflow.ImageTarget)
		wantErr    string // 空表示应成功
		noRegistry bool   // 失败时不应访问 registry
		check      func(t *testing.T, e *imageEnv)
	}{
		{
			name: "密码错误", setup: func(e *imageEnv) { e.setSecret(store.ScopeEnvironment, e.envID, "DOCKERHUB_TOKEN", wrong) },
			wantErr: "认证失败",
			check: func(t *testing.T, e *imageEnv) {
				t.Helper()
				if _, ok := e.imagePublish("v1.0.0"); ok {
					t.Fatal("认证失败的发布不应留下记录")
				}
			},
		},
		{
			name: "没有配置 secret", setup: func(e *imageEnv) { e.exec(`DELETE FROM secrets`) },
			wantErr: "secret DOCKERHUB_TOKEN 没有配置", noRegistry: true,
		},
		{
			name: "environment 不存在", setup: func(e *imageEnv) { e.exec(`DELETE FROM environments`) },
			wantErr: "environment docker-release 不存在", noRegistry: true,
		},
		{
			name: "ref 策略不允许", setup: func(e *imageEnv) { e.exec(`UPDATE environments SET ref_policy='["refs/tags/release-*"]'`) },
			wantErr: "ref 策略不允许 refs/tags/v1.0.0", noRegistry: true,
		},
		{
			name: "仓库级同名 environment 整体取代账号级", setup: func(e *imageEnv) {
				repo := int64(srcRepoID)
				e.addEnvironment(&repo, `["refs/tags/release-*"]`)
			},
			wantErr: "ref 策略不允许", noRegistry: true,
		},
		{
			name: "用户名引用 secret 时不记录用户名",
			setup: func(e *imageEnv) {
				e.setSecret(store.ScopeEnvironment, e.envID, "DOCKERHUB_USERNAME", registryUser)
			},
			target: func(t *workflow.ImageTarget) { t.Username = "${{ secrets.DOCKERHUB_USERNAME }}" },
			check: func(t *testing.T, e *imageEnv) {
				t.Helper()
				p, ok := e.imagePublish("v1.0.0")
				if !ok || p.Username != nil || p.SecretVersion == nil {
					t.Fatalf("应记录 secret 版本但不记录用户名：%+v %v", p, ok)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newImageEnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			r := e.tagRun("v1.0.0")
			e.imageBuild(r, map[string]*ocitest.Layout{"arm64": ocitest.NewLayout("linux", "arm64", "v1")})
			var opts []func(*workflow.ImageTarget)
			if tc.target != nil {
				opts = append(opts, tc.target)
			}
			id := e.imageJob(r, opts...)
			if tc.target != nil {
				// 用户名也引用了 secret：job 需要声明它
				e.exec(`UPDATE jobs SET spec=jsonb_set(spec, '{secrets}', '["DOCKERHUB_TOKEN","DOCKERHUB_USERNAME"]') WHERE id=$1`, id)
			}
			e.publish()
			if tc.wantErr == "" {
				e.wantJob(id, store.JobSucceeded, "")
			} else {
				e.wantJob(id, store.JobFailed, tc.wantErr)
				if _, reason := e.jobResult(id); strings.Contains(reason+e.log(id)+e.releaseRow(id).Error, wrong) {
					t.Fatalf("错误信息或日志中出现了密码：%s", reason)
				}
				e.wantTags(map[string]string{"v1.0.0": "", "latest": ""})
			}
			if n := len(e.reg.Requests()); tc.noRegistry && n != 0 {
				t.Fatalf("凭据检查失败时不应访问 registry，实际 %d 个请求", n)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

// 镜像的内容与配置：矩阵的各变体合并为多平台 index；损坏的 layout 和错误的配置在推送前失败。
func TestWorker_publishImages_layout(t *testing.T) {
	t.Parallel()
	corrupt := ocitest.NewLayout("linux", "arm64", "v1")
	corrupt.Files["blobs/sha256/"+strings.TrimPrefix(corrupt.Layer, "sha256:")] = []byte("tampered")
	tests := []struct {
		name    string
		layouts map[string]*ocitest.Layout
		target  func(*workflow.ImageTarget)
		wantErr string // 空表示应成功
	}{
		{name: "矩阵合并为多平台 index", layouts: map[string]*ocitest.Layout{
			"arm64": ocitest.NewLayout("linux", "arm64", "a"), "amd64": ocitest.NewLayout("linux", "amd64", "b"),
		}},
		{name: "layout 损坏", layouts: map[string]*ocitest.Layout{"arm64": corrupt}, wantErr: "OCI image layout 无效"},
		{name: "签名尚未实现", target: func(t *workflow.ImageTarget) { t.Sign = workflow.SignCosign }, wantErr: "尚未实现"},
		{name: "tag 同时是不可变和浮动的", target: func(t *workflow.ImageTarget) { t.FloatingTags = t.Tags }, wantErr: "不能同时是不可变 tag 和浮动 tag"},
		{name: "镜像名没有 registry", target: func(t *workflow.ImageTarget) { t.Name = imageRepo }, wantErr: "必须以 registry 开头"},
		{name: "找不到上游", target: func(t *workflow.ImageTarget) { t.From = "missing.image" }, wantErr: "找不到上游 job missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newImageEnv(t)
			r := e.tagRun("v1.0.0")
			layouts := tc.layouts
			if layouts == nil {
				layouts = map[string]*ocitest.Layout{"arm64": ocitest.NewLayout("linux", "arm64", "v1")}
			}
			e.imageBuild(r, layouts)
			var opts []func(*workflow.ImageTarget)
			if tc.target != nil {
				opts = append(opts, tc.target)
			}
			id := e.imageJob(r, opts...)
			e.publish()
			if tc.wantErr != "" {
				e.wantJob(id, store.JobFailed, tc.wantErr)
				if n := len(e.reg.Requests()); n != 0 {
					t.Fatalf("准备阶段失败时不应访问 registry，实际 %d 个请求", n)
				}
				return
			}
			e.wantJob(id, store.JobSucceeded, "")
			mt, body, ok := e.reg.Manifest(imageRepo, "v1.0.0")
			if !ok || mt != "application/vnd.oci.image.index.v1+json" {
				t.Fatalf("v1.0.0 应指向多平台 index：%s %v", mt, ok)
			}
			for _, l := range layouts {
				if !strings.Contains(string(body), l.Root) {
					t.Fatalf("index 应包含各平台的 manifest %s：%s", l.Root, body)
				}
			}
		})
	}
}

func TestSubstSecrets(t *testing.T) {
	got, err := substSecrets("${{ secrets.TOKEN }}", map[string]string{"TOKEN": "abc"})
	if err != nil || got != "abc" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := substSecrets("${{ secrets.MISSING }}", map[string]string{"TOKEN": "abc"}); err == nil {
		t.Fatal("没有配置的 secret 应报错")
	}
	creds := map[string]string{"A": "pa${{ss", "B": "${{ secrets.C }}", "C": "other-secret"}
	for _, tc := range []struct {
		tmpl, want string
		ok         bool
	}{
		{"user-${{ secrets.A }}-x", "user-pa${{ss-x", true},
		// 值中含有 ${{ 不是没有替换掉的占位符（以前替换后再检查整个结果，会误报"没有配置"）
		{"${{ secrets.A }}${{ secrets.A }}", "pa${{sspa${{ss", true},
		// 值中的占位符原样保留，不会再被替换成别的 secret 的值
		{"${{ secrets.B }}", "${{ secrets.C }}", true},
		{"literal", "literal", true},
		{"", "", true},
		{"${{ vars.X }}", "", false},
		{"${{ secrets.A }", "", false},
		{"${{ secrets. }}", "", false},
	} {
		got, err := substSecrets(tc.tmpl, creds)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("substSecrets(%q) = %q, %v，期望 %q（成功 %v）", tc.tmpl, got, err, tc.want, tc.ok)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	base := errors.Join(errPublishCancelled, errors.New("login failed for p4ssw0rd-value"))
	err := redactSecrets(base, map[string]string{"T": "p4ssw0rd-value", "SHORT": "ab"})
	if strings.Contains(err.Error(), "p4ssw0rd-value") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("没有脱敏：%v", err)
	}
	if !errors.Is(err, errPublishCancelled) {
		t.Fatal("脱敏后应保留错误链")
	}
	// 一个值是另一个值的一部分：先替换长的，不留下长值的剩余部分
	overlap := redactSecrets(errors.New("token=abcd-1234-tail"), map[string]string{"SHORT": "abcd", "LONG": "abcd-1234-tail"})
	if got := overlap.Error(); got != "token=***" {
		t.Fatalf("重叠的 secret 脱敏结果 = %q，期望 %q", got, "token=***")
	}
	plain := errors.New("nothing secret")
	if got := redactSecrets(plain, map[string]string{"T": "p4ssw0rd-value"}); got != plain { //nolint:errorlint // 比较的是同一个实例，确认没有被包装
		t.Fatal("没有 secret 时应原样返回")
	}
	if redactSecrets(nil, nil) != nil {
		t.Fatal("nil 应返回 nil")
	}
}

// 记录到 image_publishes 的凭据：只有能确定时才记录，否则为 nil（Dashboard 不会把这次发布算作验证）。
func TestRecordedCredentials(t *testing.T) {
	creds := map[string]store.ResolvedSecret{"DOCKERHUB_TOKEN": {Value: "pat-1", Version: 7}, "OTHER": {Value: "x", Version: 9}}
	for _, c := range []struct {
		tmpl string
		want int64 // 0 表示不记录
	}{
		{"${{ secrets.DOCKERHUB_TOKEN }}", 7},
		{"${{ secrets.OTHER }}", 9},
		{"${{ secrets.MISSING }}", 0},
		{"prefix-${{ secrets.DOCKERHUB_TOKEN }}", 0},
		{"${{ secrets.DOCKERHUB_TOKEN }}${{ secrets.OTHER }}", 0},
		{" ${{ secrets.DOCKERHUB_TOKEN }}", 0},
		{"literal-password", 0},
	} {
		got := passwordSecretVersion(c.tmpl, creds)
		switch {
		case c.want == 0 && got != nil:
			t.Errorf("%q 不应记录版本，得到 %d", c.tmpl, *got)
		case c.want != 0 && (got == nil || *got != c.want):
			t.Errorf("%q 应记录版本 %d，得到 %v", c.tmpl, c.want, got)
		}
	}
	if u := loginUser("dockeruser", "dockeruser"); u == nil || *u != "dockeruser" {
		t.Errorf("规划时求值的用户名应记录：%v", u)
	}
	if u := loginUser("${{ secrets.DOCKERHUB_USERNAME }}", "secret-user"); u != nil {
		t.Errorf("引用了 secret 的用户名不应记录：%q", *u)
	}
}

// 同一个发布 job 同时发布镜像和 GitHub Release：阶段 B 先设置镜像 tag，再正式发布 Release；进度中记录两个目标。
func TestWorker_publishImagesAndGitHub(t *testing.T) {
	t.Parallel()
	e := newImageEnv(t)
	r := e.tagRun("v1.0.0")
	l := ocitest.NewLayout("linux", "arm64", "v1")
	e.imageBuild(r, map[string]*ocitest.Layout{"arm64": l})
	e.builds(r)
	id := e.imageJob(r)
	e.exec(`UPDATE jobs SET spec=jsonb_set(spec, '{release,GitHub}', $2::jsonb) WHERE id=$1`, id, store.MustJSON(ghRelease().GitHub))
	e.publish()

	e.wantJob(id, store.JobSucceeded, "")
	e.wantTags(map[string]string{"v1.0.0": l.Root, "latest": l.Root})
	if rel := e.oneRelease(srcRepo); rel.Draft || len(rel.Assets) != 3 {
		t.Fatalf("Release 应正式发布并有 3 个 asset：draft=%v %v", rel.Draft, assetNames(rel))
	}
	row := e.releaseRow(id)
	if want := srcRepo + ", " + e.reg.Host + "/" + imageRepo; row.Target != want || row.Stage != stageDone || row.Progress["images"] == nil || row.Progress["html_url"] == nil {
		t.Fatalf("发布进度应记录两个目标：%+v，期望目标 %q", row, want)
	}
}

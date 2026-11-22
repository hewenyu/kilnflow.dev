package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/fakenpm"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/oci/ocitest"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// ---------- 发布到 npm（用 internal/fakenpm，PLAN #27.3） ----------

const (
	npmToken = "npm_s3cr3t-registry-token"
	npmScope = "@acme"
)

// npmEnv 是 npm 发布的测试环境：账号 tester 配置了 npm（scope @acme、registry 为 fakenpm）。
type npmEnv struct {
	*pubEnv
	reg *fakenpm.Server
}

func newNpmEnv(t *testing.T) *npmEnv {
	t.Helper()
	e := &npmEnv{pubEnv: newPubEnv(t), reg: fakenpm.New(npmToken)}
	t.Cleanup(e.reg.Close)
	e.w.npmOpts = []npm.Option{npm.WithRetry(3, 0)}
	if err := store.PutNpmAccount(t.Context(), e.db, e.master, ownerID, "tester", npmScope, e.reg.URL, npmToken, "test"); err != nil {
		t.Fatal(err)
	}
	return e
}

// binaries 插入构建 job build 的两个变体（darwin、linux），输出 bin 中是各平台的二进制。
func (e *npmEnv) binaries(r *testRun) {
	e.t.Helper()
	e.upstream(r, "build", "bin", map[string]any{"os": "darwin", "arch": "arm64"}, map[string]upstreamFile{
		"kiln-runner":    {content: "runner darwin", exec: true},
		"kiln-ai-runner": {content: "ai runner darwin", exec: true},
		"kiln-server":    {content: "server darwin", exec: true},
		"kiln":           {content: "cli darwin", exec: true},
	})
	e.upstream(r, "build", "bin", map[string]any{"os": "linux", "arch": "arm64"}, map[string]upstreamFile{
		"kiln-runner": {content: "runner linux", exec: true},
		"kiln-server": {content: "server linux", exec: true},
		"kiln":        {content: "cli linux", exec: true},
	})
}

// runnerNpm 是 Kiln 自己的 npm 目标：每个平台一个包，只放 Runner。
func runnerNpm(opts ...func(*workflow.NpmTarget)) workflow.Release {
	t := workflow.NpmTarget{
		From: "build.bin", Name: "kilnflow-${{ matrix.os }}-${{ matrix.arch }}", Files: []string{"kiln-runner", "kiln-ai-runner"},
		Package: map[string]any{"description": "Kiln runner", "os": []any{"${{ matrix.os }}"}},
		Access:  workflow.NpmAccessPublic, AllowSourceName: true,
	}
	for _, f := range opts {
		f(&t)
	}
	return workflow.Release{Npm: []workflow.NpmTarget{t}}
}

// untarNpm 解开 npm tarball，返回路径到内容和权限的映射。
func untarNpm(t *testing.T, tgz []byte) (map[string]string, map[string]int64) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files, modes := map[string]string{}, map[string]int64{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name], modes[h.Name] = string(b), h.Mode
	}
	return files, modes
}

func TestPublishNpmFromFiles(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	rows := e.wantTargetStates(job, "npm:committed", "npm:committed")
	if rows[0].Name != npmScope+"/kilnflow-darwin-arm64" || rows[1].Name != npmScope+"/kilnflow-linux-arm64" {
		t.Fatalf("发布目标 %+v", rows)
	}
	if rows[0].Detail["dist_tag"] != "latest" || rows[0].Detail["version"] != "1.2.0" || rows[0].Detail["integrity"] == "" {
		t.Errorf("发布结果 %+v", rows[0].Detail)
	}

	// darwin 包：Runner 和 AI 执行节点，不含 kiln-server、CLI；保留可执行位
	files, modes := untarNpm(t, e.reg.Tarball(npmScope+"/kilnflow-darwin-arm64", "1.2.0"))
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	if want := []string{"package/kiln-ai-runner", "package/kiln-runner", "package/package.json"}; !slices.Equal(names, want) {
		t.Fatalf("darwin 包的文件 %v，期望 %v", names, want)
	}
	if files["package/kiln-runner"] != "runner darwin" || modes["package/kiln-runner"] != 0o755 || modes["package/package.json"] != 0o644 {
		t.Errorf("kiln-runner = %q（%o）", files["package/kiln-runner"], modes["package/kiln-runner"])
	}
	var pkg map[string]any
	if err := json.Unmarshal([]byte(files["package/package.json"]), &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg["name"] != npmScope+"/kilnflow-darwin-arm64" || pkg["version"] != "1.2.0" || pkg["description"] != "Kiln runner" ||
		!slices.Equal(anyStrings(pkg["os"]), []string{"darwin"}) {
		t.Errorf("package.json = %v", pkg)
	}
	// linux 包没有 kiln-ai-runner：只在日志中提示
	files, _ = untarNpm(t, e.reg.Tarball(npmScope+"/kilnflow-linux-arm64", "1.2.0"))
	if len(files) != 2 || files["package/kiln-runner"] != "runner linux" {
		t.Errorf("linux 包的文件 %v", files)
	}
	log := e.log(job)
	if !strings.Contains(log, "files 中的 kiln-ai-runner 在 build") {
		t.Errorf("日志中应提示 linux 没有 kiln-ai-runner：\n%s", log)
	}
	if strings.Contains(log, npmToken) {
		t.Errorf("日志中出现了 npm token：\n%s", log)
	}
	if e.reg.Access(npmScope+"/kilnflow-darwin-arm64") != "public" || e.reg.DistTags(npmScope + "/kilnflow-linux-arm64")["latest"] != "1.2.0" {
		t.Errorf("access %q，dist-tags %v", e.reg.Access(npmScope+"/kilnflow-darwin-arm64"), e.reg.DistTags(npmScope+"/kilnflow-linux-arm64"))
	}
	for _, h := range e.reg.AuthorizationHeaders() {
		if h != "Bearer "+npmToken {
			t.Errorf("Authorization = %q", h)
		}
	}

	// 重跑：重新打包的结果与已发布的相同（打包是确定的），不再提交
	puts := countMethod(e.reg.Requests(), http.MethodPut)
	job2 := e.rerun(r)
	e.publish()
	e.wantJob(job2, store.JobSucceeded, "")
	rows = e.wantTargetStates(job2, "npm:committed", "npm:committed")
	if rows[0].Detail["previous"] != true || countMethod(e.reg.Requests(), http.MethodPut) != puts {
		t.Errorf("重跑应跳过已发布的包：%+v，PUT %d → %d", rows[0].Detail, puts, countMethod(e.reg.Requests(), http.MethodPut))
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func countMethod(reqs []string, method string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

func TestPublishNpmConflictStopsOtherTargets(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	e.reg.Preset(npmScope+"/kilnflow-linux-arm64", "1.2.0", []byte("not the same"), nil)
	rel := runnerNpm()
	rel.Assets = []workflow.Asset{{From: "build.bin", Archive: "tar.gz", Name: "kiln-${{ matrix.os }}"}}
	rel.GitHub = &workflow.ReleaseGitHub{}
	job := e.publishJob(r, rel)
	e.publish()
	e.wantJob(job, store.JobFailed, "已经以不同的内容发布")
	rows := e.wantTargetStates(job, "npm:pending", "npm:failed", "github:pending")
	if !strings.Contains(rows[1].Error, "npm 包 "+npmScope+"/kilnflow-linux-arm64：1.2.0 已经以不同的内容发布") || !strings.Contains(rows[1].Error, "已发布的版本不能覆盖") {
		t.Errorf("错误 %q", rows[1].Error)
	}
	if v := e.reg.Versions(npmScope + "/kilnflow-darwin-arm64"); len(v) != 0 {
		t.Errorf("阶段 A 失败时不应发布任何包：%v", v)
	}
	if rels := e.fake.Releases(srcRepo); len(rels) != 0 {
		t.Errorf("阶段 A 失败时 GitHub Release 不应发布：%+v", rels)
	}
}

func TestPublishNpmWithoutAccount(t *testing.T) {
	e := newNpmEnv(t)
	e.exec(`DELETE FROM npm_accounts`)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobFailed, "账号 tester 没有配置 npm")
}

func TestPublishNpmUncertainResult(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	// 第一个 PUT 已经保存但返回 502：查询后确认是本次的内容，算作成功
	e.reg.FailAfterApply(http.MethodPut, http.StatusBadGateway, 1)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	e.wantTargetStates(job, "npm:committed", "npm:committed")
	if !strings.Contains(e.log(job), "registry 中已经是本次的内容") {
		t.Errorf("日志：\n%s", e.log(job))
	}
}

func TestPublishNpmRejected(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	// 4xx：没有发布，job 失败，错误中不含 token
	e.reg.FailNext(http.MethodPut, http.StatusForbidden, 1)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobFailed, "HTTP 403")
	if _, reason := e.jobResult(job); strings.Contains(reason, npmToken) {
		t.Errorf("错误中出现了 token：%s", reason)
	}
}

func TestPublishNpmSourceNameCheck(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.upstream(r, "build", "bin", nil, map[string]upstreamFile{"kiln-runner": {content: "built from github.com/" + srcRepo, exec: true}})
	job := e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) { t.Name, t.AllowSourceName = "kiln", false }))
	e.publish()
	e.wantJob(job, store.JobFailed, "源仓库名检查失败")

	// 新建的 restricted 包不公开，不检查（已有的公开包见 TestPublishNpmSourceNameCheckExistingPublicPackage）
	r2 := e.tagRun("v1.2.1")
	e.upstream(r2, "build", "bin", nil, map[string]upstreamFile{"kiln-runner": {content: "built from github.com/" + srcRepo, exec: true}})
	job = e.publishJob(r2, runnerNpm(func(t *workflow.NpmTarget) {
		t.Name, t.AllowSourceName, t.Access = "kiln", false, workflow.NpmAccessRestricted
	}))
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
}

// access 只决定新包的可见性：已经公开的包写 access: restricted 发布的新版本同样公开，要做源仓库名检查
// （以前 restricted 一律跳过检查，私有仓库名会随新版本公开）。已有的包本身是 restricted 时不检查。
func TestPublishNpmSourceNameCheckExistingPublicPackage(t *testing.T) {
	e := newNpmEnv(t)
	const name = npmScope + "/kiln"
	restricted := runnerNpm(func(t *workflow.NpmTarget) {
		t.Name, t.AllowSourceName, t.Access = "kiln", false, workflow.NpmAccessRestricted
	})
	leaky := map[string]upstreamFile{"kiln-runner": {content: "built from github.com/" + srcRepo, exec: true}}

	e.reg.Preset(name, "1.0.0", []byte("old"), nil) // Preset 的包是公开的
	r := e.tagRun("v1.1.0")
	e.upstream(r, "build", "bin", nil, leaky)
	job := e.publishJob(r, restricted)
	e.publish()
	e.wantJob(job, store.JobFailed, "源仓库名检查失败")
	if v := e.reg.Versions(name); slices.Contains(v, "1.1.0") {
		t.Fatalf("源仓库名检查失败时不应发布：%v", v)
	}
	// 查询是否公开的请求不带 token
	auth := e.reg.AuthorizationHeaders()
	if !slices.Contains(auth, "") {
		t.Errorf("判断是否公开应匿名查询：%v", auth)
	}

	// 已有的包是 restricted：发布的内容不公开，不检查
	e.reg.SetAccess(name, npm.AccessRestricted)
	r = e.tagRun("v1.2.0")
	e.upstream(r, "build", "bin", nil, leaky)
	job = e.publishJob(r, restricted)
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
}

// staleGets 让前 n 个查询包元数据的 GET 返回 404（模拟 registry 的查询读到稍旧的数据，还看不到刚发布的版本）。
type staleGets struct {
	mu sync.Mutex
	n  int
}

func (s *staleGets) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	stale := s.n > 0 && r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/-/")
	if stale {
		s.n--
	}
	s.mu.Unlock()
	if stale {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"error":"Not found"}`)), Header: http.Header{}, Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

// 上一次发布已经生效（例如发布后 Server 中断），重跑时阶段 A 的查询读到旧数据、以为没有发布：registry 拒绝覆盖（403）后
// 再查询一次，内容相同就算作成功（以前直接失败，要等 registry 的数据更新后再重跑）。
func TestPublishNpmVersionExistsAfterStaleRead(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")

	e.w.npmOpts = []npm.Option{npm.WithRetry(1, 0), npm.WithHTTPClient(&http.Client{Transport: &staleGets{n: 2}})}
	puts := countMethod(e.reg.Requests(), http.MethodPut)
	job = e.rerun(r)
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	e.wantTargetStates(job, "npm:committed", "npm:committed")
	if got := countMethod(e.reg.Requests(), http.MethodPut) - puts; got != 2 {
		t.Errorf("两个包都应尝试发布一次（registry 返回 403），实际 %d 次", got)
	}
	if !strings.Contains(e.log(job), "registry 中已经是本次的内容") {
		t.Errorf("日志：\n%s", e.log(job))
	}

	// 版本已存在但内容不同：仍然失败
	e.reg.Preset(npmScope+"/kilnflow-darwin-arm64", "1.3.0", []byte("different"), nil)
	e.w.npmOpts = []npm.Option{npm.WithRetry(1, 0), npm.WithHTTPClient(&http.Client{Transport: &staleGets{n: 1}})}
	r = e.tagRun("v1.3.0")
	e.binaries(r)
	job = e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobFailed, "内容不同")
}

func TestPublishNpmDistTags(t *testing.T) {
	e := newNpmEnv(t)
	name := npmScope + "/kiln"
	single := func(tag string) {
		t.Helper()
		r := e.tagRun(tag)
		e.upstream(r, "build", "bin", nil, map[string]upstreamFile{"kiln-runner": {content: "runner " + tag, exec: true}})
		job := e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) { t.Name = "kiln" }))
		e.publish()
		e.wantJob(job, store.JobSucceeded, "")
	}
	single("v2.0.0")
	single("v1.3.0") // 低于已发布的 2.0.0
	single("v2.1.0-rc.1")
	tags := e.reg.DistTags(name)
	if tags["latest"] != "2.0.0" || tags["latest-1"] != "1.3.0" || tags["next"] != "2.1.0-rc.1" {
		t.Errorf("dist-tags = %v", tags)
	}
}

func TestPublishNpmTarball(t *testing.T) {
	e := newNpmEnv(t)
	pkg := func(name, version string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := `{"name":"` + name + `","version":"` + version + `","main":"index.js"}`
		for _, f := range [][2]string{{"package/package.json", body}, {"package/index.js", "module.exports = 1\n"}} {
			if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(f[1])); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	rel := workflow.Release{Npm: []workflow.NpmTarget{{From: "pack.pkg", Name: "lib", Access: workflow.NpmAccessPublic, AllowSourceName: true}}}

	r := e.tagRun("v1.0.0")
	tgz := pkg(npmScope+"/lib", "1.0.0")
	e.upstream(r, "pack", "pkg", nil, map[string]upstreamFile{"lib-1.0.0.tgz": {content: tgz}})
	job := e.publishJob(r, rel)
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	if got := e.reg.Tarball(npmScope+"/lib", "1.0.0"); string(got) != tgz {
		t.Error("构建 job 打好的包应原样发布")
	}

	// 包内的名字不是 @scope/<name>
	r = e.tagRun("v1.0.1")
	e.upstream(r, "pack", "pkg", nil, map[string]upstreamFile{"lib.tgz": {content: pkg("lib", "1.0.1")}})
	job = e.publishJob(r, rel)
	e.publish()
	e.wantJob(job, store.JobFailed, `package.json 的 name 是 "lib"，应为 "@acme/lib"`)

	// 版本与 tag 不一致
	r = e.tagRun("v1.0.2")
	e.upstream(r, "pack", "pkg", nil, map[string]upstreamFile{"lib.tgz": {content: pkg(npmScope+"/lib", "1.0.0")}})
	job = e.publishJob(r, rel)
	e.publish()
	e.wantJob(job, store.JobFailed, "应等于 tag 去掉 v")
}

func TestPublishNpmPackageJSONFromOutput(t *testing.T) {
	e := newNpmEnv(t)
	r := e.tagRun("v1.0.0")
	e.upstream(r, "build", "bin", nil, map[string]upstreamFile{
		"package.json": {content: `{"name":"x","version":"0.0.0","license":"MIT","bin":{"kiln":"kiln"}}`},
		"kiln":         {content: "cli", exec: true},
	})
	job := e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) {
		t.Name, t.Files, t.Package = "kiln", nil, map[string]any{"license": "Apache-2.0"}
	}))
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	files, _ := untarNpm(t, e.reg.Tarball(npmScope+"/kiln", "1.0.0"))
	var pkg map[string]any
	if err := json.Unmarshal([]byte(files["package/package.json"]), &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg["name"] != npmScope+"/kiln" || pkg["version"] != "1.0.0" || pkg["license"] != "Apache-2.0" || pkg["bin"] == nil {
		t.Errorf("package.json = %v", pkg)
	}
}

func TestPublishNpmErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tag    string
		target func(*workflow.NpmTarget)
		want   string
	}{
		{"tag 不是语义化版本", "release-1", nil, "不是语义化版本"},
		{"tag 带构建元数据", "v1.0.0+build.1", nil, "带构建元数据"},
		{"一个文件都没选中", "v1.0.0", func(t *workflow.NpmTarget) { t.Files = []string{"nope"} }, "没有要放进包里的文件"},
		{"包名重复", "v1.0.0", func(t *workflow.NpmTarget) { t.Name = "kiln" }, "npm 包名重复"},
		{"求值后的包名不合法", "v1.0.0", func(t *workflow.NpmTarget) { t.Name = "Kiln-${{ matrix.os }}" }, "不合法"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNpmEnv(t)
			r := e.tagRun(tc.tag)
			e.binaries(r)
			var opts []func(*workflow.NpmTarget)
			if tc.target != nil {
				opts = append(opts, tc.target)
			}
			job := e.publishJob(r, runnerNpm(opts...))
			e.publish()
			e.wantJob(job, store.JobFailed, tc.want)
			if v := e.reg.Versions(npmScope + "/kilnflow-darwin-arm64"); len(v) != 0 {
				t.Errorf("不应发布：%v", v)
			}
		})
	}
}

// release.yml 中 package 的写法：amd64 换成 npm 的 x64，其他原样。
func TestNpmManifestInterpolation(t *testing.T) {
	p := &npmPackage{name: npmScope + "/kiln", t: workflow.NpmTarget{Package: map[string]any{
		"cpu": []any{"${{ matrix.arch == 'amd64' && 'x64' || matrix.arch }}"},
		"os":  []any{"${{ matrix.os }}"},
		"n":   1.0,
	}}}
	for arch, want := range map[string]string{"amd64": "x64", "arm64": "arm64"} {
		run := &cistore.Run{}
		up := &cistore.Job{Matrix: map[string]any{"os": "linux", "arch": arch}}
		m, err := npmManifest(map[string]any{"name": "x", "license": "MIT"}, p, "1.0.0", upstreamContext(up, run))
		if err != nil {
			t.Fatal(err)
		}
		if got := anyStrings(m["cpu"]); !slices.Equal(got, []string{want}) || m["name"] != npmScope+"/kiln" || m["version"] != "1.0.0" ||
			m["license"] != "MIT" || m["n"] != 1.0 || !slices.Equal(anyStrings(m["os"]), []string{"linux"}) {
			t.Errorf("%s：%v", arch, m)
		}
	}
}

// 准备之后（阶段 A 解密凭据之前）账号的 npm 配置被整体替换：不能把新的 token 发给准备时读到的旧 registry，
// 也不能按旧的 scope 发到新 registry；停止发布，重跑时按新配置。
func TestPublishNpmConfigChangedDuringPublish(t *testing.T) {
	e := newNpmEnv(t)
	const newToken = "npm_new-registry-token"
	other := fakenpm.New(newToken)
	t.Cleanup(other.Close)
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	var once sync.Once
	e.faults.set(func(*http.Request) (*http.Response, error) {
		// 第一个 GitHub 请求在阶段 A 的 tag 检查中，此时 npm 包已经按旧配置准备好
		once.Do(func() {
			if err := store.PutNpmAccount(t.Context(), e.db, e.master, ownerID, "tester", "@other", other.URL, newToken, "admin"); err != nil {
				t.Error(err)
			}
		})
		return nil, nil
	})
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobFailed, "npm 配置在发布过程中被修改")
	if n := len(e.reg.Requests()) + len(other.Requests()); n != 0 {
		t.Errorf("不应向任何 registry 发请求：旧 %v，新 %v", e.reg.Requests(), other.Requests())
	}
	if _, reason := e.jobResult(job); strings.Contains(reason, newToken) {
		t.Errorf("错误中出现了 token：%s", reason)
	}

	// 重跑：按新的配置发布
	e.faults.set(nil)
	job = e.rerun(r)
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	if v := other.Versions("@other/kilnflow-darwin-arm64"); !slices.Equal(v, []string{"1.2.0"}) {
		t.Errorf("新 registry 中的版本 %v", v)
	}
}

// package.json 中有 "private": true 的包不发布（与 npm 的 EPRIVATE 相同），两种来源都在阶段 0 拒绝，不向 registry 发请求；
// release.npm 的 package 中写 private: false 可以明确覆盖。
func TestPublishNpmPrivate(t *testing.T) {
	e := newNpmEnv(t)
	files := map[string]upstreamFile{
		"package.json": {content: `{"name":"internal","private":true}`},
		"index.js":     {content: "module.exports = 1\n"},
	}
	r := e.tagRun("v1.0.0")
	e.upstream(r, "build", "bin", nil, files)
	job := e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) { t.Name, t.Files, t.Package = "internal", nil, nil }))
	e.publish()
	e.wantJob(job, store.JobFailed, "private 为真值")

	// package 中写 private: true 同样拒绝
	r = e.tagRun("v1.0.1")
	e.upstream(r, "build", "bin", nil, map[string]upstreamFile{"index.js": {content: "x"}})
	job = e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) {
		t.Name, t.Files, t.Package = "internal", nil, map[string]any{"private": true}
	}))
	e.publish()
	e.wantJob(job, store.JobFailed, "private 为真值")

	// 构建 job 打好的 .tgz
	r = e.tagRun("v1.0.2")
	e.upstream(r, "pack", "pkg", nil, map[string]upstreamFile{"lib.tgz": {content: string(npmTgz(t, map[string]string{
		"package/package.json": `{"name":"` + npmScope + `/lib","version":"1.0.2","private":true}`,
	}))}})
	job = e.publishJob(r, workflow.Release{Npm: []workflow.NpmTarget{{From: "pack.pkg", Name: "lib", Access: workflow.NpmAccessPublic, AllowSourceName: true}}})
	e.publish()
	e.wantJob(job, store.JobFailed, "private 为真值")
	if n := len(e.reg.Requests()); n != 0 {
		t.Fatalf("不应向 registry 发请求：%v", e.reg.Requests())
	}

	// 明确写 private: false 覆盖输出中的 private: true
	r = e.tagRun("v1.0.3")
	e.upstream(r, "build", "bin", nil, files)
	job = e.publishJob(r, runnerNpm(func(t *workflow.NpmTarget) {
		t.Name, t.Files, t.Package = "internal", nil, map[string]any{"private": false}
	}))
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
}

// npmTgz 生成 npm tarball（files：路径到内容）。
func npmTgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 同一个发布 job 同时发布 npm 包和镜像：npm 包的 seq 在前，镜像的结果（成功或失败）记在镜像自己的行上。
func TestPublishNpmAndImage(t *testing.T) {
	t.Parallel()
	e := newImageEnv(t)
	reg := fakenpm.New(npmToken)
	t.Cleanup(reg.Close)
	e.w.npmOpts = []npm.Option{npm.WithRetry(3, 0)}
	if err := store.PutNpmAccount(t.Context(), e.db, e.master, ownerID, "tester", npmScope, reg.URL, npmToken, "test"); err != nil {
		t.Fatal(err)
	}
	release := func(tag string) int64 {
		r := e.tagRun(tag)
		(&npmEnv{pubEnv: e.pubEnv, reg: reg}).binaries(r)
		e.imageBuild(r, map[string]*ocitest.Layout{"arm64": ocitest.NewLayout("linux", "arm64", tag)})
		id := e.imageJob(r)
		e.exec(`UPDATE jobs SET spec=jsonb_set(spec, '{release,Npm}', $2::jsonb) WHERE id=$1`, id, store.MustJSON(runnerNpm().Npm))
		e.publish()
		return id
	}

	id := release("v1.0.0")
	e.wantJob(id, store.JobSucceeded, "")
	rows := e.wantTargetStates(id, "npm:committed", "npm:committed", "image:committed")
	if rows[0].Detail["package"] != npmScope+"/kilnflow-darwin-arm64" || rows[1].Detail["package"] != npmScope+"/kilnflow-linux-arm64" {
		t.Errorf("npm 包的结果应记在 npm 的行上：%+v", rows[:2])
	}
	if rows[2].Detail["name"] != e.reg.Host+"/"+imageRepo || rows[2].Detail["digest"] == nil {
		t.Errorf("镜像的结果应记在镜像的行上：%+v", rows[2])
	}

	// 镜像的不可变 tag 已指向其他 digest：阶段 A 失败，错误记在镜像的行上，npm 包不发布
	e.reg.SetTag(imageRepo, "v2.0.0", []byte(`{"schemaVersion":2,"other":true}`))
	id = release("v2.0.0")
	e.wantJob(id, store.JobFailed, "已指向")
	rows = e.wantTargetStates(id, "npm:staged", "npm:staged", "image:failed")
	if !strings.Contains(rows[2].Error, "已指向") {
		t.Errorf("镜像的错误 %q", rows[2].Error)
	}
	if v := reg.Versions(npmScope + "/kilnflow-darwin-arm64"); slices.Contains(v, "2.0.0") {
		t.Errorf("阶段 A 失败时不应发布 npm 包：%v", v)
	}
}

// afterPutFails 让 PUT 之后的 GET 都返回 500（模拟发布成功后 registry 暂时查询失败）。
type afterPutFails struct {
	mu  sync.Mutex
	put bool
}

func (a *afterPutFails) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	failing := a.put && r.Method == http.MethodGet
	a.put = a.put || r.Method == http.MethodPut
	a.mu.Unlock()
	if failing {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

// 发布结果中的下载地址以 registry 返回的 dist.tarball 为准（registry 的下载路径不一定是 npmjs 的格式）：
// 新发布、重跑（已发布过）都记录真实地址；发布后查询失败时退回 npmjs 格式的地址，不影响发布结果。
func TestPublishNpmTarballURL(t *testing.T) {
	e := newNpmEnv(t)
	e.reg.SetTarballPrefix("downloads/")
	r := e.tagRun("v1.2.0")
	e.binaries(r)
	job := e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	rows := e.wantTargetStates(job, "npm:committed", "npm:committed")
	want := e.reg.URL + "downloads/" + npmScope + "/kilnflow-darwin-arm64/-/kilnflow-darwin-arm64-1.2.0.tgz"
	if rows[0].Detail["tarball"] != want {
		t.Fatalf("下载地址 %v，期望 %s", rows[0].Detail["tarball"], want)
	}
	resp, err := http.Get(want)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, e.reg.Tarball(npmScope+"/kilnflow-darwin-arm64", "1.2.0")) {
		t.Fatalf("记录的下载地址应能下载到发布的包：HTTP %d", resp.StatusCode)
	}

	// 重跑：已发布过，记录 registry 中的地址
	job = e.rerun(r)
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	rows = e.wantTargetStates(job, "npm:committed", "npm:committed")
	if rows[0].Detail["previous"] != true || rows[0].Detail["tarball"] != want {
		t.Fatalf("重跑的发布结果 %+v", rows[0].Detail)
	}

	// 发布后查询失败：发布照常成功，记录 npmjs 格式的地址并在日志中说明
	e.w.npmOpts = []npm.Option{npm.WithRetry(1, 0), npm.WithHTTPClient(&http.Client{Transport: &afterPutFails{}})}
	r = e.tagRun("v1.3.0")
	e.binaries(r)
	job = e.publishJob(r, runnerNpm())
	e.publish()
	e.wantJob(job, store.JobSucceeded, "")
	rows = e.wantTargetStates(job, "npm:committed", "npm:committed")
	if want := e.reg.URL + npmScope + "/kilnflow-darwin-arm64/-/kilnflow-darwin-arm64-1.3.0.tgz"; rows[0].Detail["tarball"] != want {
		t.Errorf("查询失败时的下载地址 %v，期望 %s", rows[0].Detail["tarball"], want)
	}
	if log := e.log(job); !strings.Contains(log, "下载地址失败") {
		t.Errorf("日志中应说明查询下载地址失败：\n%s", log)
	}
}

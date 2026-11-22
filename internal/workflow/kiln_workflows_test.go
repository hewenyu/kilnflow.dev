package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
	"github.com/hewenyu/kilnflow.dev/internal/oci"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 本仓库自己的 .kiln/workflows：确认 tag push 时哪些 workflow 被触发、镜像发布到哪里、带不带浮动 tag
// （docs/DEPLOY.md #7.2），以及二进制发布在哪里构建、产出哪些 asset（docs/DEPLOY.md #7.3）。

// loadKilnWorkflow 解析本仓库的 .kiln/workflows/<name>。
func loadKilnWorkflow(t *testing.T, name string) *Workflow {
	t.Helper()
	rel := ".kiln/workflows/" + name
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	w, d := Parse(rel, data)
	if d.HasErrors() {
		t.Fatalf("%s：%v", rel, d)
	}
	return w
}

// tagPushContext 是推送 tag 时的 github 上下文。
func tagPushContext(tag string) map[string]any {
	return map[string]any{
		"event_name": "push", "ref": "refs/tags/" + tag, "ref_name": tag, "ref_type": "tag",
		"sha": strings.Repeat("0", 40), "repository": "acme/kilnflow.dev", "repository_owner": "acme", "actor": "octocat",
	}
}

func TestKilnWorkflowsTagTriggers(t *testing.T) {
	t.Parallel()
	ci, release, docker := loadKilnWorkflow(t, "ci.yml"), loadKilnWorkflow(t, "release.yml"), loadKilnWorkflow(t, "docker.yml")
	tests := []struct {
		name                string
		tag                 string
		ci, release, docker bool
	}{
		{name: "正式版本", tag: "v1.2.3", release: true, docker: true},
		{name: "预发布版本", tag: "v1.2.3-rc.1", release: true, docker: true},
		{name: "下一个补丁版本", tag: "v0.1.2", release: true, docker: true},
		// 镜像隔离验收 tag 仍以 v 开头（满足 docker-release 的 refs/tags/v* 策略），release.yml 用 !v*-docker-test* 排除
		{name: "镜像验收 tag", tag: "v0.0.0-docker-test.1", docker: true},
		{name: "镜像验收 tag（没有序号）", tag: "v1.2.3-docker-test", docker: true},
		{name: "旧的 docker-test- 前缀", tag: "docker-test-1"},
		{name: "其他 tag", tag: "test-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := Event{Name: "push", Ref: "refs/tags/" + tt.tag}
			for _, c := range []struct {
				w    *Workflow
				want bool
			}{{ci, tt.ci}, {release, tt.release}, {docker, tt.docker}} {
				got, err := c.w.Match(ev)
				if err != nil {
					t.Fatal(err)
				}
				if (got == Matched) != c.want {
					t.Errorf("%s 对 tag %s：匹配=%v，应为 %v", c.w.File, tt.tag, got == Matched, c.want)
				}
			}
		})
	}
}

// PR 在真实 Runner 上验证产物，但不访问发布凭据或创建 Release / registry tag。
func TestKilnWorkflowsPRBuildOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"release.yml", "docker.yml"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := loadKilnWorkflow(t, name)
			match, err := w.Match(Event{Name: "pull_request", Action: "opened", Ref: "refs/pull/123/head", BaseRef: "master"})
			if err != nil || match != Matched {
				t.Fatalf("PR 应触发 %s：%v %v", name, match, err)
			}
			github := tagPushContext("feature")
			github["event_name"], github["ref"], github["ref_type"] = "pull_request", "refs/pull/123/head", "branch"
			p := Expand(w, PlanContext{GitHub: github, Inputs: map[string]string{}})
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			builds, skipped := 0, 0
			for _, j := range p.Jobs {
				if j.FailReason != "" {
					t.Fatalf("job %s 规划失败：%s", j.JobID, j.FailReason)
				}
				if strings.HasPrefix(j.JobID, "publish") {
					if j.SkipReason == "" {
						t.Errorf("PR 必须跳过发布 job %s", j.JobID)
					}
					skipped++
				} else {
					if j.SkipReason != "" {
						t.Errorf("PR 应执行构建 job %s：%s", j.JobID, j.SkipReason)
					}
					builds++
				}
			}
			wantBuilds, wantSkipped := 5, 1 // web + 三平台 build + kiln-orca
			if name == "docker.yml" {
				wantBuilds, wantSkipped = 2, 2
			}
			if builds != wantBuilds || skipped != wantSkipped {
				t.Errorf("构建/跳过发布 job 数 = %d/%d，应为 %d/%d", builds, skipped, wantBuilds, wantSkipped)
			}
		})
	}
}

func TestKilnDockerImagePlan(t *testing.T) {
	t.Parallel()
	w := loadKilnWorkflow(t, "docker.yml")
	tests := []struct {
		name         string
		tag          string
		publishJob   string // 应执行的发布 job；另一个应为 skipped
		floatingTags []string
	}{
		{name: "正式版本", tag: "v1.2.3", publishJob: "publish", floatingTags: []string{"latest"}},
		// 预发布版本在规划时带 latest，发布时按 PLAN #13.8 的规则跳过（只有正式版本才移动浮动 tag）
		{name: "预发布版本", tag: "v1.2.3-rc.1", publishJob: "publish", floatingTags: []string{"latest"}},
		{name: "镜像验收 tag", tag: "v0.0.0-docker-test.1", publishJob: "publish-test"},
		{name: "镜像验收 tag（没有序号）", tag: "v1.2.3-docker-test", publishJob: "publish-test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := Expand(w, PlanContext{
				GitHub: tagPushContext(tt.tag),
				Inputs: map[string]string{},
				Vars: func(env string) map[string]string {
					if env != "docker-release" {
						return map[string]string{}
					}
					return map[string]string{"DOCKERHUB_NAMESPACE": "example", "DOCKERHUB_USERNAME": "example"}
				},
			})
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			var platforms, imageKeys []string
			for _, j := range p.Jobs {
				if j.JobID != "image" {
					continue
				}
				sp := j.Spec
				platforms = append(platforms, sp.Env["TARGETPLATFORM"])
				imageKeys = append(imageKeys, j.Key)
				if sp.Platform != sp.Env["TARGETPLATFORM"] || sp.Executor != ExecutorOCI || sp.BuildkitImage == "" {
					t.Errorf("image 应在目标平台的 OCI Runner 上用 BuildKit 构建：%+v", sp)
				}
				if sp.Env["VERSION"] != tt.tag {
					t.Errorf("image 版本 %q，应为 %s", sp.Env["VERSION"], tt.tag)
				}
				if len(sp.Outputs) != 1 || sp.Outputs[0].Name != "image" || sp.Outputs[0].Path != "out/image/" {
					t.Errorf("image 应输出 OCI layout：%+v", sp.Outputs)
				}
			}
			slices.Sort(platforms)
			if want := []string{"linux/amd64", "linux/arm64"}; !slices.Equal(platforms, want) {
				t.Errorf("镜像平台 %v，应为 %v", platforms, want)
			}
			var active []string
			for _, j := range p.Jobs {
				if j.FailReason != "" {
					t.Fatalf("job %s 失败：%s", j.JobID, j.FailReason)
				}
				if j.Spec.Release == nil || j.SkipReason != "" {
					continue
				}
				active = append(active, j.JobID)
				if !slices.Equal(j.NeedsKeys, imageKeys) {
					t.Errorf("发布应等待全部镜像变体：needs=%v，image=%v", j.NeedsKeys, imageKeys)
				}
				if len(j.Spec.Release.Images) != 1 {
					t.Fatalf("job %s 应发布一个镜像：%+v", j.JobID, j.Spec.Release.Images)
				}
				img := j.Spec.Release.Images[0]
				if img.From != "image.image" {
					t.Errorf("发布应合并 image.image 的全部平台，实际 %s", img.From)
				}
				if img.Name != "docker.io/example/kilnflow" {
					t.Errorf("镜像名 %s，应为 docker.io/example/kilnflow", img.Name)
				}
				if !slices.Equal(img.Tags, []string{tt.tag}) {
					t.Errorf("tags %v，应为 [%s]", img.Tags, tt.tag)
				}
				if !slices.Equal(img.FloatingTags, tt.floatingTags) {
					t.Errorf("floating-tags %v，应为 %v", img.FloatingTags, tt.floatingTags)
				}
			}
			if !slices.Equal(active, []string{tt.publishJob}) {
				t.Errorf("执行的发布 job %v，应为 [%s]", active, tt.publishJob)
			}
		})
	}
}

// release.yml 中每个二进制的构建 job 和输出（一个二进制一个文件）。
var releaseBinaries = []struct {
	job, output, program string
	platforms            []string
}{
	{"build", "server", "kiln-server", []string{"darwin/arm64", "linux/amd64", "linux/arm64"}},
	{"build", "runner", "kiln-runner", []string{"darwin/arm64", "linux/amd64", "linux/arm64"}},
	{"build", "cli", "kiln", []string{"darwin/arm64", "linux/amd64", "linux/arm64"}},
}

// release.yml：build 在各平台的 host Runner 上使用锁定的 Go runtime 编译，版本取 tag；kiln-orca 在 Node 镜像中
// pnpm pack 连接器（版本取 tag）；publish 引用 environment release，每个二进制不打包、单独作为一个 asset
// （<程序名>-<os>-<arch>）发布到本仓库，CI Runner 每个平台发一个只含该二进制的 npm 包，连接器原样发布打好的包。
func TestKilnReleasePlan(t *testing.T) {
	t.Parallel()
	_, ciWebContainer := ciContainers(t)
	w := loadKilnWorkflow(t, "release.yml")
	for _, tag := range []string{"v1.2.3", "v1.2.3-rc.1"} {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			gctx := tagPushContext(tag)
			p := Expand(w, PlanContext{GitHub: gctx, Inputs: map[string]string{}})
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			variants := map[string][]*PlannedJob{}
			var publish *PlannedJob
			for _, j := range p.Jobs {
				if j.FailReason != "" || j.SkipReason != "" {
					t.Fatalf("job %s：fail=%q skip=%q", j.JobID, j.FailReason, j.SkipReason)
				}
				sp := j.Spec
				switch j.JobID {
				case "web":
					checkReleaseWeb(t, sp, ciWebContainer)
				case "build":
					checkReleaseBuild(t, j, tag)
					variants[j.JobID] = append(variants[j.JobID], j)
				case "kiln-orca":
					checkReleaseOrca(t, j, ciWebContainer, tag)
					variants[j.JobID] = append(variants[j.JobID], j)
				case "publish":
					if publish != nil {
						t.Fatal("应只有 1 个 publish job")
					}
					publish = j
					if j.Kind != JobKindPublish || sp.Environment != "release" || sp.Release == nil || sp.Release.GitHub == nil {
						t.Fatalf("publish 应是引用 environment release 的 GitHub Release 发布：%+v", sp)
					}
					if g := sp.Release.GitHub; g.Repository != "" {
						t.Errorf("publish 应发布到本仓库：%+v", g)
					}
				default:
					t.Errorf("多出的 job %s", j.JobID)
				}
			}
			if publish == nil {
				t.Fatal("没有 publish job")
			}
			for _, b := range releaseBinaries {
				var got []string
				for _, j := range variants[b.job] {
					got = append(got, j.Spec.Env["TARGETOS"]+"/"+j.Spec.Env["TARGETARCH"])
				}
				slices.Sort(got)
				if !slices.Equal(got, b.platforms) {
					t.Errorf("%s 的平台 %v，应为 %v", b.program, got, b.platforms)
				}
			}
			checkReleaseAssets(t, publish, variants, gctx)
			checkReleaseNpm(t, publish, variants, gctx)
		})
	}
}

// checkReleaseAssets 检查每个二进制在每个平台单独一个 asset（archive: none），名字为 <程序名>-<os>-<arch>。
func checkReleaseAssets(t *testing.T, publish *PlannedJob, variants map[string][]*PlannedJob, gctx map[string]any) {
	t.Helper()
	fa, sums := publish.Spec.Release.FileAssets()
	if sums != "" && sums != "SHA256SUMS" {
		t.Errorf("校验和文件 %q，应为默认的 SHA256SUMS", sums)
	}
	var got, want []string
	for _, a := range fa {
		if a.Archive != "none" {
			t.Errorf("asset %s 应不打包（archive: none），实际 %s", a.Name, a.Archive)
		}
		job, _, _ := strings.Cut(a.From, ".")
		for _, j := range variants[job] {
			got = append(got, a.From+" "+interpolateFor(t, a.Name, j, gctx))
		}
	}
	for _, b := range releaseBinaries {
		for _, pl := range b.platforms {
			os, arch, _ := strings.Cut(pl, "/")
			want = append(want, b.job+"."+b.output+" "+b.program+"-"+os+"-"+arch)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("asset %v，应为 %v", got, want)
	}
}

// checkReleaseNpm 检查 CI Runner 每个平台一个 npm 包（"<短名> os=<os> cpu=<cpu>"），包中只有该 Runner 的二进制
// （上游输出就是这一个文件，不写 files）；公开发布，允许包中出现源仓库名（Go 二进制带模块路径）。连接器 kiln-orca
// 是一个打好的 .tgz，原样公开发布（不写 files、package），不放开源仓库名检查。
func checkReleaseNpm(t *testing.T, publish *PlannedJob, variants map[string][]*PlannedJob, gctx map[string]any) {
	t.Helper()
	var got, want []string
	orca := 0
	for _, n := range publish.Spec.Release.Npm {
		if n.From == "kiln-orca.package" {
			orca++
			if n.Name != "kilnflow-orca" || n.Access != NpmAccessPublic || n.AllowSourceName || len(n.Files) != 0 || len(n.Package) != 0 {
				t.Errorf("kiln-orca 应原样公开发布为 kilnflow-orca，不放开源仓库名检查：%+v", n)
			}
			continue
		}
		if n.Access != NpmAccessPublic || !n.AllowSourceName || len(n.Files) != 0 {
			t.Errorf("npm 目标应公开发布上游输出中的唯一文件：%+v", n)
		}
		job, _, _ := strings.Cut(n.From, ".")
		for _, j := range variants[job] {
			eval := func(v any) string {
				t.Helper()
				list, ok := v.([]any)
				if !ok || len(list) != 1 {
					t.Fatalf("npm package 字段应是一个元素的列表：%#v", v)
				}
				str, ok := list[0].(string)
				if !ok {
					t.Fatalf("npm package 字段的元素应是字符串：%#v", list[0])
				}
				return interpolateFor(t, str, j, gctx)
			}
			got = append(got, n.From+" "+interpolateFor(t, n.Name, j, gctx)+" os="+eval(n.Package["os"])+" cpu="+eval(n.Package["cpu"]))
		}
	}
	if orca != 1 {
		t.Errorf("应有 1 个 kiln-orca 的 npm 包，实际 %d 个", orca)
	}
	for _, b := range releaseBinaries {
		if b.program != "kiln-runner" {
			continue // kiln-server 和 CLI 不发到 npm
		}
		for _, pl := range b.platforms {
			os, arch, _ := strings.Cut(pl, "/")
			cpu := arch
			if arch == "amd64" {
				cpu = "x64"
			}
			want = append(want, b.job+"."+b.output+" "+"kilnflow-runner-"+os+"-"+arch+" os="+os+" cpu="+cpu)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("npm 包 %v，应为 %v", got, want)
	}
}

// interpolateFor 按上游 job 的一个变体求值发布字段中的表达式（同 Server 发布时的求值方式）。
func interpolateFor(t *testing.T, s string, j *PlannedJob, gctx map[string]any) string {
	t.Helper()
	v, err := expr.Interpolate(s, expr.Context{"matrix": expr.Lenient(j.Matrix), "github": expr.Lenient(gctx)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ciContainers 返回 ci.yml 中 Go（test）和 Dashboard 前端（web）job 的镜像，都必须固定 digest。
func ciContainers(t *testing.T) (goImage, webImage string) {
	t.Helper()
	for _, j := range Expand(loadKilnWorkflow(t, "ci.yml"), PlanContext{GitHub: tagPushContext("v1.2.3"), Inputs: map[string]string{}}).Jobs {
		switch j.JobID {
		case "test":
			goImage = j.Spec.Container
		case "web":
			webImage = j.Spec.Container
		}
	}
	if !strings.Contains(goImage, "@sha256:") || !strings.Contains(webImage, "@sha256:") {
		t.Fatalf("ci.yml 的镜像应固定 digest：test %q，web %q", goImage, webImage)
	}
	return goImage, webImage
}

// TestKilnCIOrcaContainer 检查 ci.yml 的 kiln-orca job：连接器的测试用真实的 git，必须用带 git 的完整 Node 镜像（不是 web 用的
// slim 镜像），固定 digest，只取 kiln-orca/。
func TestKilnCIOrcaContainer(t *testing.T) {
	_, webImage := ciContainers(t)
	var sp *Spec
	for _, j := range Expand(loadKilnWorkflow(t, "ci.yml"), PlanContext{GitHub: tagPushContext("v1.2.3"), Inputs: map[string]string{}}).Jobs {
		if j.JobID == "kiln-orca" {
			sp = j.Spec
		}
	}
	if sp == nil {
		t.Fatal("ci.yml 应有 kiln-orca job")
	}
	if !strings.HasPrefix(sp.Container, "docker.io/library/node@sha256:") || sp.Container == webImage {
		t.Errorf("kiln-orca 应使用固定 digest 的官方 Node 完整镜像（带 git），不是 web 的 slim 镜像：%q", sp.Container)
	}
	if !slices.Equal(sp.Inputs, []string{"kiln-orca/"}) {
		t.Errorf("kiln-orca 应只取 kiln-orca/：%v", sp.Inputs)
	}
}

// checkReleaseWeb 检查 release.yml 的 web job：与 ci.yml 相同的 Node 镜像，构建产物交给 build 内嵌进 kiln-server。
func checkReleaseWeb(t *testing.T, sp *Spec, ciWebContainer string) {
	t.Helper()
	if sp.Container != ciWebContainer || len(sp.Outputs) != 1 || sp.Outputs[0].Name != "dist" ||
		sp.Outputs[0].Path != "internal/server/web/ui/dist/" {
		t.Errorf("web 应在 %s 中构建并输出 dist：%s %+v", ciWebContainer, sp.Container, sp.Outputs)
	}
}

// checkReleaseOrca 检查 release.yml 的 kiln-orca job：与 ci.yml 的 web 相同的 Node 镜像，只取 kiln-orca/，版本取 tag，
// 输出打好的 dist/kiln-orca.tgz。
func checkReleaseOrca(t *testing.T, j *PlannedJob, ciWebContainer, tag string) {
	t.Helper()
	sp := j.Spec
	if sp.Container != ciWebContainer || !slices.Equal(sp.Inputs, []string{"kiln-orca/"}) || sp.Env["VERSION"] != tag {
		t.Errorf("kiln-orca 应在 %s 中只用 kiln-orca/ 打包、版本取 tag %s：%s %v %q", ciWebContainer, tag, sp.Container, sp.Inputs, sp.Env["VERSION"])
	}
	if len(sp.Outputs) != 1 || sp.Outputs[0].Name != "package" || sp.Outputs[0].Path != "dist/kiln-orca.tgz" {
		t.Errorf("kiln-orca 应输出 package: dist/kiln-orca.tgz：%+v", sp.Outputs)
	}
}

// checkReleaseBuild 检查 release.yml 中一个 build 变体的执行环境和构建参数。
func checkReleaseBuild(t *testing.T, j *PlannedJob, tag string) {
	t.Helper()
	sp := j.Spec
	expected := sp.Env["TARGETOS"] + "/" + sp.Env["TARGETARCH"]
	if sp.Platform != expected || sp.Executor != ExecutorHost || sp.Container != "" {
		t.Errorf("%s 应在 %s 的 host Runner 执行，实际 %s %s %s", j.JobID, expected, sp.Platform, sp.Executor, sp.Container)
	}
	if !slices.Equal(sp.Runtimes, []string{"go"}) {
		t.Errorf("%s 应使用锁定的 Go runtime：%v", j.JobID, sp.Runtimes)
	}
	for _, key := range []string{"GOOS", "GOARCH"} {
		if _, ok := sp.Env[key]; ok {
			t.Errorf("%s 不应设置 %s 交叉编译", j.JobID, key)
		}
	}
	if sp.CPU != 1 || sp.MemoryBytes != 1536<<20 || !sp.Network || sp.TimeoutMinutes != 30 {
		t.Errorf("%s 资源限制不对：cpu=%v memory=%d network=%v timeout=%d", j.JobID, sp.CPU, sp.MemoryBytes, sp.Network, sp.TimeoutMinutes)
	}
	if sp.MaxParallel != 1 {
		t.Errorf("%s 的变体应逐个构建（max-parallel: 1），实际 %d", j.JobID, sp.MaxParallel)
	}
	for k, v := range map[string]string{"VERSION": tag, "CGO_ENABLED": "0", "GOTOOLCHAIN": "local"} {
		if sp.Env[k] != v {
			t.Errorf("%s 的 env %s=%q，应为 %q", j.JobID, k, sp.Env[k], v)
		}
	}
	// 每个二进制一个输出，输出就是 dist/<程序名> 这一个文件；构建脚本编译的正是这些程序。
	var wantOut []NamedPath
	var programs []string
	for _, b := range releaseBinaries {
		if b.job == j.JobID {
			wantOut = append(wantOut, NamedPath{Name: b.output, Path: "dist/" + b.program})
			programs = append(programs, b.program)
		}
	}
	if !slices.Equal(sp.Outputs, wantOut) {
		t.Errorf("%s 的 outputs %+v，应为 %+v", j.JobID, sp.Outputs, wantOut)
	}
	wantRun := "bash .kiln/scripts/release-build.sh " + strings.Join(programs, " ")
	if len(sp.Steps) != 1 || strings.TrimSpace(sp.Steps[0].Run) != wantRun {
		t.Errorf("%s 的 steps %+v，应只有 %q", j.JobID, sp.Steps, wantRun)
	}
	// 只有 kiln-server 内嵌 Dashboard 前端
	embedsWeb := slices.Contains(programs, "kiln-server")
	switch {
	case embedsWeb && (len(sp.Fetch) != 1 || sp.Fetch[0].Output != "dist" || sp.Fetch[0].To != "internal/server/web/ui/dist/"):
		t.Errorf("%s 应取 web.dist 放到 internal/server/web/ui/dist/（内嵌 Dashboard 前端）：%+v", j.JobID, sp.Fetch)
	case !embedsWeb && len(sp.Fetch) != 0:
		t.Errorf("%s 不需要 fetch：%+v", j.JobID, sp.Fetch)
	}
}

// 没有配置 DOCKERHUB_NAMESPACE 时镜像名无效，发布失败，不会退回到某个默认仓库。
func TestKilnDockerImageNeedsNamespace(t *testing.T) {
	t.Parallel()
	p := Expand(loadKilnWorkflow(t, "docker.yml"), PlanContext{
		GitHub: tagPushContext("v1.2.3"),
		Inputs: map[string]string{},
		Vars:   func(string) map[string]string { return map[string]string{} },
	})
	for _, j := range p.Jobs {
		if j.JobID != "publish" {
			continue
		}
		name := j.Spec.Release.Images[0].Name
		if _, err := oci.ParseName(name); err == nil {
			t.Fatalf("没有命名空间时镜像名 %s 不应有效", name)
		}
		return
	}
	t.Fatal("没有 publish job")
}

// TestKilnNodeJobsUsePnpmRuntime 检查 ci.yml、release.yml 中 Node 容器的 job（web、kiln-orca）用 Runner 管理的 pnpm
// （PLAN #12.8）：声明 runtimes: [pnpm]，lockfile 中有这个平台的原生包条目（不是 any），版本与 package.json 的
// packageManager 相同；包存储放在 toolcache（PNPM_CONFIG_STORE_DIR）；step 中不再安装 pnpm。
func TestKilnNodeJobsUsePnpmRuntime(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(toolchain.LockPath)))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.ParseLock(data)
	if err != nil {
		t.Fatal(err)
	}
	// packageManager 中的版本（pnpm@<版本>）
	pmVersion := func(dir string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", dir, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct {
			PackageManager string `json:"packageManager"`
		}
		if err := json.Unmarshal(b, &pkg); err != nil {
			t.Fatal(err)
		}
		v, ok := strings.CutPrefix(pkg.PackageManager, "pnpm@")
		if !ok {
			t.Fatalf("%s/package.json 的 packageManager 应为 pnpm：%q", dir, pkg.PackageManager)
		}
		v, _, _ = strings.Cut(v, "+")
		return v
	}
	wantDir := map[string]string{"web": "web", "kiln-orca": "kiln-orca"}
	for _, name := range []string{"ci.yml", "release.yml"} {
		p := Expand(loadKilnWorkflow(t, name), PlanContext{GitHub: tagPushContext("v1.2.3"), Inputs: map[string]string{}})
		seen := 0
		for _, j := range p.Jobs {
			dir, ok := wantDir[j.JobID]
			if !ok {
				continue
			}
			seen++
			sp := j.Spec
			where := name + " 的 " + j.JobID
			if !slices.Contains(sp.Runtimes, "pnpm") {
				t.Errorf("%s 应声明 runtimes: [pnpm]：%v", where, sp.Runtimes)
			}
			version, plat, _, err := lock.Resolve("pnpm", sp.Platform)
			if err != nil {
				t.Errorf("%s：%v", where, err)
			} else if plat != sp.Platform || version != pmVersion(dir) {
				t.Errorf("%s：lockfile 中 pnpm 为 %s（%s），应为 %s 的 %s 原生包条目", where, version, plat, sp.Platform, pmVersion(dir))
			}
			if !slices.Contains(sp.Toolcache, ToolCache{Name: "pnpm-store", Env: "PNPM_CONFIG_STORE_DIR"}) {
				t.Errorf("%s 应把 pnpm 的包存储放在 toolcache（PNPM_CONFIG_STORE_DIR）：%+v", where, sp.Toolcache)
			}
			for _, st := range sp.Steps {
				if strings.Contains(st.Run, "install --global pnpm") || strings.Contains(st.Run, "install -g pnpm") {
					t.Errorf("%s 的 step %q 不应再安装 pnpm", where, st.Name)
				}
			}
		}
		if seen != len(wantDir) {
			t.Errorf("%s 中找到 %d 个 Node job，应为 %d 个", name, seen, len(wantDir))
		}
	}
}

package workflow

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// buildJob 是发布 job 的上游：声明了 bin、pkg、rt 三个输出。
const buildJob = `  build:
    runs-on: linux/arm64
    steps: [{run: x}]
    outputs: { bin: dist/, pkg: pkg.tgz, rt: rt/ }
`

func TestParseReleaseFields(t *testing.T) {
	wf := mustParse(t, `on: push
jobs:
`+buildJob+`  publish:
    needs: [build]
    release:
      verify-reproducible: true
      github:
        repository: o/public
        target: main
        allow-source-name: true
        checksums: none
        sign: none
        provenance: none
        sbom: false
        prerelease: auto
        latest: "false"
        notes: { from: CHANGELOG.md }
        assets:
          - from: build.bin
          - { from: build.bin, archive: zip, name: "app.zip" }
          - { from: build.bin, archive: none }
      npm:
        - { from: build.pkg, name: "app-${{ matrix.os }}", files: [app, "lib/**"], package: { os: ["${{ matrix.os }}"], description: d }, access: public, dist-tag: next, allow-source-name: true }
      runtimes:
        - { from: build.rt, name: tool, version: 1.0.0, platform: linux/arm64, env: { PATH: bin, N: 1 } }
`)
	r := wf.Job("publish").Release
	if !r.VerifyReproducible {
		t.Error("verify-reproducible 应为 true")
	}
	g := r.GitHub
	sbom := false
	// 旧写法 release.github.assets / checksums 移到 release.assets / checksums
	wantG := ReleaseGitHub{
		Repository: "o/public", Target: "main", AllowSourceName: true, Sign: SignNone,
		Provenance: ProvenanceNone, SBOM: &sbom, NotesFrom: "CHANGELOG.md", Prerelease: "auto", Latest: "false",
	}
	if !reflect.DeepEqual(*g, wantG) {
		t.Errorf("release.github = %+v\n期望 %+v", *g, wantG)
	}
	if r.Checksums != ChecksumsNone {
		t.Errorf("release.checksums = %q", r.Checksums)
	}
	// archive 默认是 tar.gz
	wantArchives := []string{"tar.gz", "zip", "none"}
	if len(r.Assets) != len(wantArchives) {
		t.Fatalf("release.assets = %+v", r.Assets)
	}
	for i, a := range r.Assets {
		if a.From != "build.bin" || a.Archive != wantArchives[i] || a.Pos.Line == 0 {
			t.Errorf("assets[%d] = %+v", i, a)
		}
	}
	if r.Assets[1].Name != "app.zip" {
		t.Errorf("assets[1].name = %q", r.Assets[1].Name)
	}
	wantNpm := NpmTarget{
		From: "build.pkg", Name: "app-${{ matrix.os }}", Files: []string{"app", "lib/**"},
		Package: map[string]any{"os": []any{"${{ matrix.os }}"}, "description": "d"},
		Access:  NpmAccessPublic, DistTag: "next", AllowSourceName: true,
	}
	if len(r.Npm) == 1 {
		wantNpm.Pos = r.Npm[0].Pos
	}
	if !reflect.DeepEqual(r.Npm, []NpmTarget{wantNpm}) || wantNpm.Pos.Line == 0 {
		t.Errorf("release.npm = %+v", r.Npm)
	}
	wantRT := RuntimeTarget{From: "build.rt", Name: "tool", Version: "1.0.0", Platform: "linux/arm64", Env: map[string]any{"PATH": "bin", "N": 1.0}}
	if !reflect.DeepEqual(r.Runtimes, []RuntimeTarget{wantRT}) {
		t.Errorf("release.runtimes = %+v", r.Runtimes)
	}
}

// release.assets（新写法）与 release.github.assets（旧写法）等价：解析结果相同，展开后的 Spec JSON 也相同。
func TestParseReleaseAssetsEquivalent(t *testing.T) {
	const assets = `
        - { from: build.bin, name: "app-${{ github.ref_name }}", archive: none }
        - { from: build.bin, archive: zip }`
	oldStyle := mustParse(t, `on: push
jobs:
`+buildJob+`  publish:
    needs: [build]
    release:
      github:
        checksums: SUMS
        assets:`+assets+`
`)
	newStyle := mustParse(t, `on: push
jobs:
`+buildJob+`  publish:
    needs: [build]
    release:
      checksums: SUMS
      assets:`+assets+`
      github: {}
`)
	clearPos := func(r *Release) Release {
		c := *r
		c.Assets = slices.Clone(r.Assets)
		for i := range c.Assets {
			c.Assets[i].Pos = Pos{}
		}
		return c
	}
	a, b := clearPos(oldStyle.Job("publish").Release), clearPos(newStyle.Job("publish").Release)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("两种写法的解析结果不同：\n旧 %+v\n新 %+v", a, b)
	}
	ja, jb := mustJSON(t, a), mustJSON(t, b)
	if ja != jb {
		t.Fatalf("两种写法的 Spec JSON 不同：\n旧 %s\n新 %s", ja, jb)
	}
	const golden = `{"VerifyReproducible":false,"GitHub":{"Repository":"","Target":"","AllowSourceName":false,"Assets":null,"Checksums":"",` +
		`"Sign":"","Provenance":"","SBOM":null,"NotesFrom":"","Prerelease":"","Latest":""},"Npm":null,"Images":null,"Runtimes":null,` +
		`"Assets":[{"From":"build.bin","Archive":"none","Name":"app-${{ github.ref_name }}"},{"From":"build.bin","Archive":"zip","Name":""}],"Checksums":"SUMS"}`
	if ja != golden {
		t.Fatalf("Spec JSON：\n got  %s\n want %s", ja, golden)
	}
	// jobs.spec 中的旧数据（assets 在 GitHub 下）经 FileAssets 读出同样的结果
	legacy := Release{GitHub: &ReleaseGitHub{Assets: a.Assets, Checksums: "SUMS"}}
	gotAssets, gotSums := legacy.FileAssets()
	wantAssets, wantSums := b.FileAssets()
	if !reflect.DeepEqual(gotAssets, wantAssets) || gotSums != wantSums {
		t.Fatalf("FileAssets：旧数据 %+v %q，新写法 %+v %q", gotAssets, gotSums, wantAssets, wantSums)
	}
}

func TestParseReleaseUploads(t *testing.T) {
	wf := mustParse(t, `on: push
jobs:
`+buildJob+`  publish:
    needs: [build]
    environment: release
    release:
      assets: [{ from: build.bin, archive: none }]
      uploads:
        - name: site
          url: https://example.com/api/releases
          token: ${{ secrets.SITE_TOKEN }}
          latest: auto
          prerelease: "false"
        - { url: "http://127.0.0.1:8080/api", token: "${{ secrets.LOCAL }}", latest: true }
`)
	got := wf.Job("publish").Release.Uploads
	want := []UploadTarget{
		{Name: "site", URL: "https://example.com/api/releases", Token: "${{ secrets.SITE_TOKEN }}", Latest: ChoiceAuto, Prerelease: ChoiceFalse},
		{URL: "http://127.0.0.1:8080/api", Token: "${{ secrets.LOCAL }}", Latest: ChoiceTrue},
	}
	for i := range got {
		if got[i].Pos.Line == 0 {
			t.Errorf("uploads[%d] 没有位置", i)
		}
		got[i].Pos = Pos{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("release.uploads = %+v\n期望 %+v", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseImageJob(t *testing.T) {
	wf := mustParse(t, `on: push
jobs:
`+buildJob+`  img:
    needs: [build]
    image:
      base: docker.io/library/alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      platforms: [linux/arm64, linux/amd64]
      entrypoint: [/app]
      cmd: serve
      env: { A: "1" }
      workdir: /w
      user: "1000"
      labels: { org.opencontainers.image.source: "x" }
      files:
        - { from: build.bin, to: /usr/bin/, select: { goarch: arm64 } }
`)
	im := wf.Job("img").Image
	want := &ImageJob{
		Base:       "docker.io/library/alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Platforms:  []string{"linux/arm64", "linux/amd64"},
		Files:      []ImageFile{{From: "build.bin", To: "/usr/bin/", Select: map[string]string{"goarch": "arm64"}}},
		Entrypoint: []string{"/app"},
		Cmd:        []string{"serve"},
		Env:        &OrderedMap{Keys: []string{"A"}, Values: map[string]string{"A": "1"}},
		Workdir:    "/w",
		User:       "1000",
		Labels:     &OrderedMap{Keys: []string{"org.opencontainers.image.source"}, Values: map[string]string{"org.opencontainers.image.source": "x"}},
	}
	if !reflect.DeepEqual(im, want) {
		t.Errorf("image = %+v\n期望 %+v", im, want)
	}
}

func TestParseReleaseDiags(t *testing.T) {
	cases := []struct {
		name    string
		release string // publish 的 release 内容（flow 写法）
		want    string
	}{
		{"release 未知字段", "{ homebrew: {} }", "release 中有未知字段 homebrew"},
		{"release 不是映射", "[github]", "release 应为映射"},
		{"github 未知字段", "{ github: { draft: true } }", "release.github 中有未知字段 draft"},
		{"notes 未知字段", "{ github: { notes: { template: x } } }", "release.github.notes 中有未知字段 template"},
		{"assets 不是列表", "{ github: { assets: build.bin } }", "release.github.assets 应为列表"},
		{"asset 缺少 from", "{ github: { assets: [{ name: x }] } }", "asset 缺少 from"},
		{"asset 的 archive 无效", "{ github: { assets: [{ from: build.bin, archive: rar }] } }", "archive 只能是 tar.gz、zip、none"},
		{"asset 未知字段", "{ github: { assets: [{ from: build.bin, label: x }] } }", "assets[] 中有未知字段 label"},
		{"sbom 不是布尔值", "{ github: { sbom: maybe } }", "应为 true 或 false"},
		{"npm 未知字段", "{ npm: [{ from: build.pkg, name: a, access: public, tag: x }] }", "npm[] 中有未知字段 tag"},
		{"npm 缺少 name", "{ npm: [{ from: build.pkg, access: public }] }", "npm[] 缺少 name"},
		{"npm 缺少 access", "{ npm: [{ from: build.pkg, name: a }] }", "npm[] 缺少 access"},
		{"npm 写了 token", `{ npm: [{ from: build.pkg, name: a, access: public, token: "${{ secrets.NPM }}" }] }`, "npm[] 不能写 token：npm 的 registry、scope 和 token 在 Kiln 中按账号配置"},
		{"npm 写了 registry", "{ npm: [{ from: build.pkg, name: a, access: public, registry: https://r }] }", "npm[] 不能写 registry"},
		{"npm 包名带 scope", "{ npm: [{ from: build.pkg, name: \"@o/a\", access: public }] }", "不要写 scope"},
		{"npm 包名大写", "{ npm: [{ from: build.pkg, name: App, access: public }] }", "包名 \"App\" 不合法"},
		{"npm 包名含 ~", "{ npm: [{ from: build.pkg, name: \"a~b\", access: public }] }", "包名 \"a~b\" 不合法"},
		{"npm 保留名", "{ npm: [{ from: build.pkg, name: node_modules, access: public }] }", "npm 保留的名字"},
		{"npm access 无效", "{ npm: [{ from: build.pkg, name: a, access: private }] }", "npm[].access 只能是 public 或 restricted"},
		{"npm dist-tag 像版本号", "{ npm: [{ from: build.pkg, name: a, access: public, dist-tag: v1 }] }", "不是合法的 dist-tag"},
		{"npm dist-tag 是范围 x", "{ npm: [{ from: build.pkg, name: a, access: public, dist-tag: x }] }", "不是合法的 dist-tag"},
		{"npm dist-tag 是范围 vX", "{ npm: [{ from: build.pkg, name: a, access: public, dist-tag: vX }] }", "不是合法的 dist-tag"},
		{"npm dist-tag 是范围 x.1", "{ npm: [{ from: build.pkg, name: a, access: public, dist-tag: x.1 }] }", "不是合法的 dist-tag"},
		{"npm files 无效", "{ npm: [{ from: build.pkg, name: a, access: public, files: [\"a[\"] }] }", "不是合法的 glob"},
		{"npm package 写 version", "{ npm: [{ from: build.pkg, name: a, access: public, package: { version: 1.0.0 } }] }", "npm[].package 不能写 version"},
		{"npm package 不是映射", "{ npm: [{ from: build.pkg, name: a, access: public, package: [a] }] }", "npm[].package 应为映射"},
		{"npm 不是列表", "{ npm: { from: build.pkg } }", "release.npm 应为列表"},
		{"runtimes 未知字段", "{ runtimes: [{ from: build.rt, os: linux }] }", "runtimes[] 中有未知字段 os"},
		{"images 缺少 from", `{ images: [{ name: docker.io/o/a, tags: [v1], username: u, password: "${{ secrets.P }}" }] }`, "images[] 缺少 from"},
		{"images 缺少 username", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], password: "${{ secrets.P }}" }] }`, "images[] 缺少 username"},
		{"images 缺少 password", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], username: u }] }`, "images[] 缺少 password"},
		{"images 缺少 name", `{ images: [{ from: build.bin, tags: [v1], username: u, password: "${{ secrets.P }}" }] }`, "images[] 缺少 name"},
		{"images 无效的 floating tag", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], floating-tags: ["a b"], username: u, password: "${{ secrets.P }}" }] }`, `tag "a b" 无效`},
		{"images 未知字段", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], username: u, password: "${{ secrets.P }}", platform: x }] }`, "images[] 中有未知字段 platform"},
		{"images 的 floating tag 引用 secrets", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], floating-tags: ["${{ secrets.T }}"], username: u, password: "${{ secrets.P }}" }] }`, "release.images.tags 不能引用 secrets"},
		{"assets 两处都写", "{ assets: [{ from: build.bin }], github: { assets: [{ from: build.bin }] } }", "release.assets 和 release.github.assets 只能写一处"},
		{"checksums 两处都写", "{ checksums: A, assets: [{ from: build.bin }], github: { checksums: B } }", "release.checksums 和 release.github.checksums 只能写一处"},
		{"assets 没有使用者", "{ assets: [{ from: build.bin }] }", "release.assets 没有使用者"},
		{"uploads 没有 assets", `{ uploads: [{ url: "https://e.com/r", token: "${{ secrets.T }}" }] }`, "release.uploads 需要 release.assets"},
		{"uploads 缺少 url", `{ assets: [{ from: build.bin }], uploads: [{ token: "${{ secrets.T }}" }] }`, "uploads[] 缺少 url"},
		{"uploads 缺少 token", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r" }] }`, "uploads[] 缺少 token"},
		{"uploads 不是 https", `{ assets: [{ from: build.bin }], uploads: [{ url: "http://e.com/r", token: "${{ secrets.T }}" }] }`, "必须是 https"},
		{"uploads 带查询参数", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r?k=v", token: "${{ secrets.T }}" }] }`, "不能带查询参数"},
		{"uploads 的 latest 无效", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r", token: "${{ secrets.T }}", latest: always }] }`, "uploads[].latest 只能是 auto、true、false"},
		{"uploads 的 prerelease 无效", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r", token: "${{ secrets.T }}", prerelease: maybe }] }`, "uploads[].prerelease 只能是 auto、true、false"},
		{"uploads 未知字段", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r", token: "${{ secrets.T }}", method: PUT }] }`, "uploads[] 中有未知字段 method"},
		{"uploads 不是列表", `{ assets: [{ from: build.bin }], uploads: { url: "https://e.com/r" } }`, "release.uploads 应为列表"},
		{"uploads 的 url 重复", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r", token: "${{ secrets.T }}" }, { url: "https://e.com/r", token: "${{ secrets.U }}" }] }`, "url https://e.com/r 重复"},
		{"uploads 的 token 不是 secret", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/r", token: "plain-token" }] }`, "release.uploads.token 必须引用 secret"},
		{"uploads 的 url 引用 secret", `{ assets: [{ from: build.bin }], uploads: [{ url: "https://e.com/${{ secrets.P }}", token: "${{ secrets.T }}" }] }`, "release.uploads.url 不能引用 secrets"},
		{"release.assets 引用不存在的输出", `{ assets: [{ from: build.nope }], github: {} }`, "没有名为 nope 的输出"},
		{"images 的密码引用 vars", `{ images: [{ from: build.bin, name: docker.io/o/a, tags: [v1], username: u, password: "${{ vars.P }}" }] }`, "release.images.password 必须引用 secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n"+buildJob+"  publish:\n    needs: [build]\n    release: "+c.release+"\n"))
			wantDiag(t, diags, c.want)
		})
	}
}

func TestParseImageDiags(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{"image 未知字段", "{ base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, ports: [80] }", "image 中有未知字段 ports"},
		{"image.base 没有 digest", "{ base: alpine:3 }", "job img 的 image.base 必须用 digest 固定"},
		{"files 不是列表", "{ base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, files: { from: build.bin } }", "image.files 应为列表"},
		{"files 未知字段", "{ base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, files: [{ from: build.bin, mode: 755 }] }", "files[] 中有未知字段 mode"},
		{"labels 不是映射", "{ base: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, labels: [a] }", "labels 应为映射"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n"+buildJob+"  img:\n    needs: [build]\n    image: "+c.image+"\n"))
			wantDiag(t, diags, c.want)
		})
	}
}

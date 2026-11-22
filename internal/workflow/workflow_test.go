package workflow

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

const ciYAML = `
name: ci
on:
  push:
    branches: [main, "release/**"]
  pull_request:
    branches: [main]
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
env:
  CGO_ENABLED: "0"
jobs:
  go-deps:
    runs-on: linux/arm64
    container: docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    runtimes: [go]
    inputs: [go.mod, go.sum]
    network: true
    env:
      GOMODCACHE: ${{ kiln.workspace }}/.gomodcache
    steps:
      - run: go mod download
    outputs:
      modcache: .gomodcache/
  build:
    needs: [go-deps]
    strategy:
      fail-fast: false
      matrix:
        include:
          - { target: darwin-arm64, goos: darwin, goarch: arm64 }
          - { target: linux-amd64, goos: linux, goarch: amd64 }
    runs-on: linux/arm64
    runtimes: [go]
    inputs: [go.mod, go.sum, cmd/, internal/]
    fetch:
      - from: go-deps.modcache
        to: .gomodcache/
    env:
      GOOS: ${{ matrix.goos }}
      TOKEN: ${{ secrets.TOKEN }}
    steps:
      - run: go build -o dist/${{ matrix.target }}/ ./cmd/...
      - if: github.event_name == 'pull_request'
        run: echo pr only
    outputs:
      bin: dist/${{ matrix.target }}/
  test:
    needs: [build]
    if: github.ref == 'refs/heads/never'
    runs-on: darwin/arm64
    strategy:
      matrix:
        os: [a, b]
        n: [1, 2]
        exclude:
          - { os: a, n: 2 }
        include:
          - { os: b, extra: x }
          - { os: c }
    steps:
      - run: echo ${{ matrix.os }}
  publish:
    needs: [build]
    environment: release
    release:
      github:
        assets:
          - from: build.bin
            archive: tar.gz
            name: app-${{ github.ref_name }}-${{ matrix.target }}
`

func ctx() PlanContext {
	return PlanContext{
		GitHub: map[string]any{
			"event_name": "push", "sha": "abc", "ref": "refs/heads/main", "ref_name": "main",
			"ref_type": "branch", "repository": "o/r", "repository_owner": "o", "head_ref": "", "base_ref": "",
		},
		Vars: func(string) map[string]string { return map[string]string{"X": "1"} },
	}
}

func TestParseExpand(t *testing.T) {
	wf, diags := Parse("ci.yml", []byte(ciYAML))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	p := Expand(wf, ctx())
	if p.Diags.HasErrors() {
		t.Fatal(p.Diags)
	}
	if p.ConcurrencyGroup != "ci-refs/heads/main" || !p.CancelInProgress {
		t.Fatal(p.ConcurrencyGroup, p.CancelInProgress)
	}
	byKey := map[string]*PlannedJob{}
	var keys []string
	for _, j := range p.Jobs {
		byKey[j.Key] = j
		keys = append(keys, j.Key+"|"+j.DisplayName)
	}
	t.Log(strings.Join(keys, "\n"))
	b := byKey["build{goarch=amd64,goos=linux,target=linux-amd64}"]
	if b == nil {
		t.Fatal("缺少 build 变体")
	}
	if b.DisplayName != "build (linux-amd64, linux, amd64)" {
		t.Fatal(b.DisplayName)
	}
	if b.Spec.Env["GOOS"] != "linux" || b.Spec.Env["CGO_ENABLED"] != "0" || b.Spec.Env["TOKEN"] != "${{ secrets.TOKEN }}" {
		t.Fatal(b.Spec.Env)
	}
	if len(b.Spec.Steps) != 1 || b.Spec.Steps[0].Run != "go build -o dist/linux-amd64/ ./cmd/..." {
		t.Fatal(b.Spec.Steps)
	}
	if b.Spec.Fetch[0].JobKey != "go-deps" || b.Spec.Fetch[0].Output != "modcache" {
		t.Fatal(b.Spec.Fetch)
	}
	if b.Spec.Executor != "host" || b.Spec.Secrets[0] != "TOKEN" {
		t.Fatal(b.Spec.Executor, b.Spec.Secrets)
	}
	if byKey["go-deps"].Spec.Env["GOMODCACHE"] != WorkspacePlaceholder+"/.gomodcache" {
		t.Fatal(byKey["go-deps"].Spec.Env)
	}
	// test 的矩阵：(a,1) (b,1,x) (b,2,x) (c)
	var testKeys []string
	for _, j := range p.Jobs {
		if j.JobID == "test" {
			testKeys = append(testKeys, j.Key)
			if j.SkipReason == "" {
				t.Fatal("test 应被 if 跳过")
			}
		}
	}
	want := "test{n=1,os=a} test{extra=x,n=1,os=b} test{extra=x,n=2,os=b} test{os=c}"
	if strings.Join(testKeys, " ") != want {
		t.Fatalf("got %s", strings.Join(testKeys, " "))
	}
	pub := byKey["publish"]
	if pub.Kind != "publish" || len(pub.NeedsKeys) != 2 || pub.FailReason != "" {
		t.Fatal(pub.Kind, pub.NeedsKeys, pub.FailReason)
	}
	if !p.Diags.HasErrors() && len(p.Diags.Warnings()) != 1 {
		t.Fatal(p.Diags)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // 诊断中应包含的内容
	}{
		{"uses", `on: push
jobs:
  a:
    runs-on: linux/arm64
    steps:
      - uses: actions/checkout@v4`, "x.yml:6:9: 错误：step 不支持字段 uses：Kiln 不运行 marketplace action"},
		{"cycle", `on: push
jobs:
  a: { needs: [b], runs-on: linux/arm64, steps: [{run: x}] }
  b: { needs: [a], runs-on: linux/arm64, steps: [{run: x}] }`, "needs 构成了环：a → b → a"},
		{"unknown", `on: push
jobs:
  a: { runs-on: linux/arm64, foo: 1, steps: [{run: x}] }`, "x.yml:3:30: 错误：job a 中有未知字段 foo"},
		{"digest", `on: push
jobs:
  a: { runs-on: linux/arm64, container: debian:12, steps: [{run: x}] }`, "job a 的 container 必须用 digest 固定"},
		{"secret-in-run", `on: push
jobs:
  a: { runs-on: linux/arm64, steps: [{run: "echo ${{ secrets.X }}"}] }`, "job a 的 steps.run 不能引用 secrets"},
		{"secret-uppercase", `on: push
jobs:
  a: { runs-on: linux/arm64, steps: [{run: "echo ${{ SECRETS.X }}"}] }`, "job a 的 steps.run 不能引用 secrets"},
		{"fetch-not-needed", `on: push
jobs:
  a: { runs-on: linux/arm64, steps: [{run: x}], outputs: {o: out/} }
  b: { runs-on: linux/arm64, fetch: [{from: a.o, to: x/}], steps: [{run: x}] }`, "job b 的 fetch 引用了 a，但它不在 needs 中"},
		{"bad-input-pattern", `on: push
jobs:
  a: { runs-on: linux/arm64, inputs: ["src/[a"], steps: [{run: x}] }`, `inputs 中有无效的匹配模式 "src/[a"`},
		{"bad-filter-pattern", `on: { push: { branches: ["release/[x"] } }
jobs:
  a: { runs-on: linux/arm64, steps: [{run: x}] }`, `无效的匹配模式 "release/[x"`},
		{"memory-overflow", `on: push
jobs:
  a: { runs-on: linux/arm64, resources: { memory: 9999999999Ti }, steps: [{run: x}] }`, "超出范围"},
		{"seq", `on: push
jobs:
  a: { runs-on: linux/arm64, toolcache: x, steps: [{run: x}] }`, "toolcache 应为列表"},
		{"buildkit-without-container", `on: push
jobs:
  a: { runs-on: linux/arm64, buildkit: true, steps: [{run: x}] }`, "buildkit 只能用于带 container 的 job"},
		{"image-plain-password", imageJob(`password: hunter2`), "release.images.password 必须引用 secret"},
		{"image-missing-tags", imageJob(`password: "${{ secrets.T }}"`, "tags"), "缺少 tags"},
		{"image-bad-name", imageJob(`password: "${{ secrets.T }}"`, "name"), "必须以 registry 开头"},
		{"image-bad-tag", imageJob(`password: "${{ secrets.T }}"`, "badtag"), "tag \"-x\" 无效"},
		{"image-secret-in-name", imageJob(`password: "${{ secrets.T }}"`, "secretname"), "release.images.name 不能引用 secrets"},
		{"image-bad-sign", imageJob(`password: "${{ secrets.T }}"`, "sign"), "sign 只能是 cosign 或 none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte(c.src))
			if !diags.HasErrors() {
				t.Fatal("期望报错")
			}
			if !strings.Contains(diags.Error(), c.want) {
				t.Errorf("诊断中应包含 %q，实际：\n%s", c.want, diags.Error())
			}
		})
	}
}

// imageJob 返回带 release.images 的 workflow；variant 改坏其中一个字段。
func imageJob(password string, variant ...string) string {
	name, tags, sign := "docker.io/${{ vars.NS }}/app", `tags: ["${{ github.ref_name }}"]`, "none"
	for _, v := range variant {
		switch v {
		case "tags":
			tags = ""
		case "name":
			name = "app"
		case "badtag":
			tags = `tags: ["-x"]`
		case "secretname":
			name = "docker.io/${{ secrets.NS }}/app"
		case "sign":
			sign = "gpg"
		}
	}
	return `on: { push: { tags: ["v*"] } }
jobs:
  build:
    runs-on: linux/arm64
    container: docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    buildkit: true
    steps: [{run: x}]
    outputs: { image: out/image/ }
  publish:
    needs: [build]
    release:
      images:
        - from: build.image
          name: ` + name + `
          ` + tags + `
          username: ${{ vars.USER }}
          ` + password + `
          sign: ` + sign + "\n"
}

func TestExpandImageRelease(t *testing.T) {
	wf, diags := Parse("x.yml", []byte(imageJob(`password: "${{ secrets.TOKEN }}"`)))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	pc := ctx()
	pc.GitHub["ref"], pc.GitHub["ref_name"] = "refs/tags/v1.2.3", "v1.2.3"
	pc.Vars = func(string) map[string]string { return map[string]string{"NS": "team", "USER": "bot"} }
	p := Expand(wf, pc)
	if p.Diags.HasErrors() {
		t.Fatal(p.Diags)
	}
	var build, pub *PlannedJob
	for _, j := range p.Jobs {
		switch j.JobID {
		case "build":
			build = j
		case "publish":
			pub = j
		}
	}
	if build.FailReason != "" || build.Spec.BuildkitImage != BuildkitImage || build.Spec.Executor != ExecutorOCI {
		t.Fatalf("build：%q %q %s", build.FailReason, build.Spec.BuildkitImage, build.Spec.Executor)
	}
	if pub.FailReason != "" {
		t.Fatal(pub.FailReason)
	}
	im := pub.Spec.Release.Images[0]
	if im.Name != "docker.io/team/app" || im.Tags[0] != "v1.2.3" || im.Username != "bot" || im.Password != "${{ secrets.TOKEN }}" {
		t.Fatalf("images 求值结果不对：%+v", im)
	}
	if len(pub.Spec.Secrets) != 1 || pub.Spec.Secrets[0] != "TOKEN" {
		t.Fatalf("publish 应引用 secret TOKEN：%v", pub.Spec.Secrets)
	}
	if len(build.Spec.Secrets) != 0 || len(build.Spec.Env) != 0 {
		t.Fatalf("构建 job 不应带任何 secret：%v %v", build.Spec.Secrets, build.Spec.Env)
	}
	// 原始 workflow 不应被展开结果修改
	if wf.Job("publish").Release.Images[0].Name != "docker.io/${{ vars.NS }}/app" {
		t.Fatal("展开修改了解析结果")
	}

	src := strings.Replace(imageJob(`password: "${{ secrets.TOKEN }}"`), "sign: none", "sign: cosign", 1)
	wf, diags = Parse("x.yml", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	for _, j := range Expand(wf, pc).Jobs {
		if j.JobID == "publish" && !strings.Contains(j.FailReason, "release.images.sign: cosign 尚未实现") {
			t.Fatalf("sign: cosign 应明确失败：%q", j.FailReason)
		}
	}
}

func TestExpandCollectsErrors(t *testing.T) {
	wf, diags := Parse("x.yml", []byte(`on: push
jobs:
  a:
    runs-on: linux/arm64
    env: { A: "${{ nope }}", B: "${{ nope2 }}" }
    steps: [{run: x}]
`))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	p := Expand(wf, ctx())
	got := p.Diags.Error()
	for _, want := range []string{"env.A：", "env.B："} {
		if !strings.Contains(got, want) {
			t.Errorf("应报告 %s，实际：\n%s", want, got)
		}
	}
	if n := len(p.Diags.Errors()); n != 2 {
		t.Errorf("应有 2 条错误，实际 %d：\n%s", n, got)
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"echo hi\nsecond", "echo hi"},
		{"  \n  x", "x"},
		{strings.Repeat("a", 80), strings.Repeat("a", 80)},
		{strings.Repeat("a", 81), strings.Repeat("a", 80) + "…"},
		{strings.Repeat("中", 81), strings.Repeat("中", 80) + "…"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := firstLine(c.in); got != c.want {
				t.Errorf("firstLine(%q) = %q，应为 %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"4Gi", 4 << 30, false},
		{"512Mi", 512 << 20, false},
		{"2G", 2e9, false},
		{"1.5Ki", 1536, false},
		{"1024", 1024, false},
		{"0", 0, true},
		{"-1Gi", 0, true},
		{"NaNGi", 0, true},
		{"9999999999Ti", 0, true},
		{"0.0001Ki", 0, true}, // 不到 1 字节：以前截断为 0 而不报错
		{"lots", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseMemory(c.in)
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("ParseMemory(%q) = %d, %v", c.in, got, err)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	wf, diags := Parse("ci.yml", []byte(`on:
  push:
    branches: [main]
    paths: ["src/**", "!src/docs/**"]
  pull_request:
    paths-ignore: ["**.md"]
`+"jobs:\n  a: { runs-on: linux/arm64, steps: [{run: x}] }\n"))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	files := func(f ...string) func() ([]string, bool, error) {
		return func() ([]string, bool, error) { return f, true, nil }
	}
	check := func(ev Event, want MatchResult) {
		t.Helper()
		got, err := wf.Match(ev)
		if err != nil || got != want {
			t.Fatalf("%+v: got %v want %v (%v)", ev, got, want, err)
		}
	}
	check(Event{Name: "push", Ref: "refs/heads/main", ChangedFiles: files("src/a.go")}, Matched)
	check(Event{Name: "push", Ref: "refs/heads/main", ChangedFiles: files("src/docs/a.md")}, PathsExcluded)
	check(Event{Name: "push", Ref: "refs/heads/dev", ChangedFiles: files("src/a.go")}, NoMatch)
	check(Event{Name: "push", Ref: "refs/tags/v1"}, NoMatch)
	check(Event{Name: "pull_request", Action: "opened", BaseRef: "x", ChangedFiles: files("README.md")}, PathsExcluded)
	check(Event{Name: "pull_request", Action: "synchronize", BaseRef: "x", ChangedFiles: files("README.md", "a.go")}, Matched)
	check(Event{Name: "pull_request", Action: "closed", BaseRef: "x"}, NoMatch)
}

func TestInputMatcher(t *testing.T) {
	m := NewInputMatcher([]string{"go.mod", "cmd/", "**/*.go", "!internal/gen/**"})
	for path, want := range map[string]bool{
		"go.mod": true, "go.sum": false, "cmd/a/main.go": true, "x/y.go": true,
		"internal/gen/a.go": false, "cmdx/a": false,
	} {
		t.Run(path, func(t *testing.T) {
			if m.Match(path) != want {
				t.Errorf("%s: want %v", path, want)
			}
		})
	}
	d := NewInputMatcher(nil)
	if d.Match(".kiln/workflows/ci.yml") || !d.Match(".kiln/runtimes.lock") {
		t.Fatal("默认 inputs 不正确")
	}
}

func TestEmptyPatterns(t *testing.T) {
	m := NewInputMatcher([]string{"./", "cmd/", "docs/**", "go.mod", "!x"})
	got := m.EmptyPatterns([]string{"cmd/a.go", "README.md"})
	// "./" 表示整个仓库，与 Match 一致，不应报告为空
	if strings.Join(got, ",") != "docs/**,go.mod" {
		t.Fatalf("got %v", got)
	}
	if !m.Match("README.md") {
		t.Fatal("./ 应匹配整个仓库")
	}
}

// release.uploads 在规划时求值：url、name 可以引用 vars 和 github，name 不写时为 url 的主机名；token 保留 secret 占位符。
func TestExpandUploadRelease(t *testing.T) {
	const src = `on: { push: { tags: ["v*"] } }
jobs:
  build:
    runs-on: linux/arm64
    steps: [{run: x}]
    outputs: { bin: dist/ }
  publish:
    needs: [build]
    environment: release
    release:
      assets: [{ from: build.bin, name: "app-${{ github.ref_name }}", archive: none }]
      github: {}
      uploads:
        - url: https://${{ vars.SITE }}/api/releases
          token: ${{ secrets.SITE_TOKEN }}
        - name: mirror-${{ github.ref_name }}
          url: ${{ vars.MIRROR }}
          token: ${{ secrets.MIRROR_TOKEN }}
          latest: "false"
`
	wf, diags := Parse("x.yml", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	pc := ctx()
	pc.GitHub["ref"], pc.GitHub["ref_name"] = "refs/tags/v1.2.3", "v1.2.3"
	pc.Vars = func(string) map[string]string {
		return map[string]string{"SITE": "dl.example.com", "MIRROR": "https://mirror.example.com/r"}
	}
	p := Expand(wf, pc)
	if p.Diags.HasErrors() {
		t.Fatal(p.Diags)
	}
	pub := planJob(t, p, "publish")
	if pub.FailReason != "" {
		t.Fatal(pub.FailReason)
	}
	want := []UploadTarget{
		{Name: "dl.example.com", URL: "https://dl.example.com/api/releases", Token: "${{ secrets.SITE_TOKEN }}"},
		{Name: "mirror-v1.2.3", URL: "https://mirror.example.com/r", Token: "${{ secrets.MIRROR_TOKEN }}", Latest: ChoiceFalse},
	}
	got := slices.Clone(pub.Spec.Release.Uploads)
	for i := range got {
		got[i].Pos = Pos{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uploads 求值结果：%+v\n期望 %+v", got, want)
	}
	if !slices.Equal(pub.Spec.Secrets, []string{"MIRROR_TOKEN", "SITE_TOKEN"}) {
		t.Fatalf("publish 应引用两个 secret：%v", pub.Spec.Secrets)
	}
	if a, _ := pub.Spec.Release.FileAssets(); len(a) != 1 || a[0].Name != "app-${{ github.ref_name }}" {
		t.Fatalf("asset 名在发布时求值（可以引用上游的 matrix）：%+v", a)
	}

	// 求值后的 url 不是 https：规划报错
	pc.Vars = func(string) map[string]string {
		return map[string]string{"SITE": "dl.example.com", "MIRROR": "http://mirror.example.com/r"}
	}
	if p := Expand(wf, pc); !strings.Contains(p.Diags.Error(), "release.uploads.url：上传地址 \"http://mirror.example.com/r\" 必须是 https") {
		t.Fatalf("应报告 url 不是 https：%s", p.Diags.Error())
	}
}

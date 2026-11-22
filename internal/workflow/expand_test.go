package workflow

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// planJob 返回 job ID 为 id 的第一个变体。
func planJob(t *testing.T, p *Plan, id string) *PlannedJob {
	t.Helper()
	for _, j := range p.Jobs {
		if j.JobID == id {
			return j
		}
	}
	t.Fatalf("没有 job %s；诊断：\n%s", id, p.Diags.Error())
	return nil
}

// mustExpand 解析并展开 src，有错误时让测试失败。
func mustExpand(t *testing.T, src string, pc PlanContext) *Plan {
	t.Helper()
	p := expandSrc(t, src, pc)
	if p.Diags.HasErrors() {
		t.Fatalf("展开出错：\n%s", p.Diags.Error())
	}
	return p
}

// 表达式中只能使用规划时可以求值的上下文：if、name、environment 只能用 github、inputs、vars、matrix；
// runs-on、steps 等还可以用 kiln；只有 env 可以用 secrets（保留为占位符）。
func TestExpandContexts(t *testing.T) {
	cases := []struct {
		name  string
		field string // 写在 job a 中的字段
		want  string // 错误信息中应包含的内容
	}{
		{"if 中不能用 kiln", `if: "kiln.workspace != ''"`, "if：表达式 \"kiln.workspace != ''\"：上下文 kiln 在此处不可用"},
		{"if 中不能用裸写的 secrets", `if: "secrets.X == 'a'"`, "上下文 secrets 在此处不可用"},
		{"if 中不能用 env", `if: "env.A == 'a'"`, "上下文 env 在此处不可用"},
		{"if 中不能用 needs", `if: "needs.b.result == 'success'"`, "上下文 needs 在此处不可用"},
		{"if 中不能用状态函数", `if: "${{ success() }}"`, "不支持状态函数 success()"},
		{"name 中不能用 kiln", `name: "${{ kiln.workspace }}"`, "name：表达式"},
		{"environment 中不能用 kiln", `environment: "${{ kiln.workspace }}"`, "environment：表达式"},
		{"environment.url 中不能用 kiln", `environment: { name: p, url: "${{ kiln.workspace }}" }`, "environment.url：表达式"},
		{"steps.run 中不能用 steps", `env: {}` + "\n    steps: [{ run: \"${{ steps.a.outputs.x }}\" }]", "steps.run：表达式 \"steps.a.outputs.x\"：上下文 steps 在此处不可用"},
		{"steps.run 中不能用 runner", "steps: [{ run: \"${{ runner.os }}\" }]", "上下文 runner 在此处不可用"},
		{"steps.run 中不能用 job", "steps: [{ run: \"${{ job.status }}\" }]", "上下文 job 在此处不可用"},
		{"steps.run 中不能用 hashFiles", "steps: [{ run: \"${{ hashFiles('go.sum') }}\" }]", "不支持 hashFiles"},
		{"github 中不支持的字段", "steps: [{ run: \"${{ github.event.head_commit.message }}\" }]", "不支持的字段 github.event"},
		{"kiln 中不支持的字段", "steps: [{ run: \"${{ kiln.version }}\" }]", "不支持的字段 kiln.version"},
		{"steps.if 中不能用 kiln", "steps: [{ run: x, if: \"kiln.workspace\" }]", "steps[0].if：表达式"},
		{"steps.env 中的表达式错误", "steps: [{ run: x, env: { A: \"${{ nope }}\" } }]", "steps.env.A：表达式"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    " + c.field + "\n"
			if !strings.Contains(c.field, "steps:") {
				src += "    steps: [{run: x}]\n"
			}
			p := expandSrc(t, src, ctx())
			wantDiag(t, p.Diags, c.want)
		})
	}

	t.Run("各字段可用的上下文", func(t *testing.T) {
		pc := ctx()
		pc.CommitTime = 1700000000
		pc.Vars = func(env string) map[string]string { return map[string]string{"WHERE": "env=" + env} }
		p := mustExpand(t, `on: push
env: { W: "${{ vars.WHERE }}" }
jobs:
  a:
    if: github.ref_name == 'main' && vars.WHERE == 'env=prod' && matrix.nope == ''
    environment: prod
    runs-on: linux/arm64
    env: { S: "${{ secrets.TOKEN }}", K: "${{ kiln.workspace }}/x" }
    outputs: { o: "${{ kiln.workspace }}/out" }
    steps:
      - run: echo ${{ kiln.commit_time }} ${{ github.sha }} ${{ inputs.none }}
        env: { T: "${{ secrets.STEP }}" }
`, pc)
		j := planJob(t, p, "a")
		if j.SkipReason != "" {
			t.Errorf("if 应为 true（没有矩阵时 matrix 的字段为空字符串）：%q", j.SkipReason)
		}
		wantEnv := map[string]string{"W": "env=prod", "S": "${{ secrets.TOKEN }}", "K": WorkspacePlaceholder + "/x"}
		if !reflect.DeepEqual(j.Spec.Env, wantEnv) {
			t.Errorf("env = %v，期望 %v（vars 按 job 的 environment 取）", j.Spec.Env, wantEnv)
		}
		if got := j.Spec.Steps[0].Run; got != "echo 1700000000 abc " {
			t.Errorf("steps.run = %q", got)
		}
		if got := j.Spec.Steps[0].Env["T"]; got != "${{ secrets.STEP }}" {
			t.Errorf("steps.env.T = %q", got)
		}
		if !slices.Equal(j.Spec.Secrets, []string{"STEP", "TOKEN"}) {
			t.Errorf("Secrets = %v，期望已排序的 [STEP TOKEN]", j.Spec.Secrets)
		}
		if j.Spec.Outputs[0] != (NamedPath{Name: "o", Path: WorkspacePlaceholder + "/out"}) {
			t.Errorf("outputs = %+v", j.Spec.Outputs)
		}
	})
}

// workflow_dispatch 的 inputs：没有传入的取默认值，boolean 转换成布尔值；其他事件中为空字符串。
func TestExpandDispatchInputs(t *testing.T) {
	const src = `on:
  push:
  workflow_dispatch:
    inputs:
      level: { default: low }
      dry: { type: boolean, default: "true" }
      name: {}
jobs:
  a:
    runs-on: linux/arm64
    env:
      LEVEL: "${{ inputs.level }}"
      DRY: "${{ inputs.dry == true }}"
      NAME: "${{ inputs.name }}"
      EXTRA: "${{ inputs.extra }}"
    steps: [{run: x}]
`
	cases := []struct {
		name   string
		event  string
		inputs map[string]string
		want   map[string]string
	}{
		{"手动触发时取默认值", EventWorkflowDispatch, nil, map[string]string{"LEVEL": "low", "DRY": "true", "NAME": "", "EXTRA": ""}},
		{"手动触发时使用传入的值", EventWorkflowDispatch, map[string]string{"level": "high", "dry": "FALSE", "name": "n"}, map[string]string{"LEVEL": "high", "DRY": "false", "NAME": "n", "EXTRA": ""}},
		{"boolean 不区分大小写", EventWorkflowDispatch, map[string]string{"dry": "True"}, map[string]string{"LEVEL": "low", "DRY": "true", "NAME": "", "EXTRA": ""}},
		{"其他事件中为空字符串", EventPush, nil, map[string]string{"LEVEL": "", "DRY": "false", "NAME": "", "EXTRA": ""}},
		{"没有声明的输入也可以读到", EventWorkflowDispatch, map[string]string{"extra": "e"}, map[string]string{"LEVEL": "low", "DRY": "true", "NAME": "", "EXTRA": "e"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := ctx()
			pc.GitHub["event_name"] = c.event
			pc.Inputs = c.inputs
			j := planJob(t, mustExpand(t, src, pc), "a")
			if !reflect.DeepEqual(j.Spec.Env, c.want) {
				t.Errorf("env = %v，期望 %v", j.Spec.Env, c.want)
			}
		})
	}
}

func TestExpandConcurrency(t *testing.T) {
	cases := []struct {
		name       string
		conc       string
		wantGroup  string
		wantCancel bool
		wantErr    string
	}{
		{"简写", "g-${{ github.ref_name }}", "g-main", false, ""},
		{"cancel-in-progress 是表达式", "{ group: g, cancel-in-progress: \"${{ github.ref_name != 'main' }}\" }", "g", false, ""},
		{"cancel-in-progress 直接写表达式", "{ group: g, cancel-in-progress: \"github.ref_name == 'main'\" }", "g", true, ""},
		{"group 中的表达式出错", "{ group: \"${{ nope }}\" }", "", false, "x.yml:2:14: 错误：concurrency.group：表达式"},
		{"cancel-in-progress 中的表达式出错", "{ group: g, cancel-in-progress: \"${{ secrets.X }}\" }", "g", false, "concurrency.cancel-in-progress：表达式"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expandSrc(t, "on: push\nconcurrency: "+c.conc+"\n"+okJob, ctx())
			if c.wantErr != "" {
				wantDiag(t, p.Diags, c.wantErr)
				return
			}
			if p.Diags.HasErrors() {
				t.Fatal(p.Diags)
			}
			if p.ConcurrencyGroup != c.wantGroup || p.CancelInProgress != c.wantCancel {
				t.Errorf("concurrency = %q %v，期望 %q %v", p.ConcurrencyGroup, p.CancelInProgress, c.wantGroup, c.wantCancel)
			}
		})
	}
}

// 没有写的字段取默认值。
func TestExpandSpecDefaults(t *testing.T) {
	j := planJob(t, mustExpand(t, "on: push\n"+okJob, ctx()), "a")
	s := j.Spec
	if s.Workflow != "x.yml" || s.JobID != "a" || s.Platform != "linux/arm64" || s.Executor != ExecutorHost {
		t.Errorf("基本字段 = %+v", s)
	}
	if !s.Cache || s.CPU != defaultCPU || s.MemoryBytes != defaultMemoryBytes || s.TimeoutMinutes != DefaultTimeoutMinutes || !s.FailFast || !s.InputsDefault {
		t.Errorf("默认值不对：%+v", s)
	}
	if s.Steps[0].Shell != "bash" || s.Steps[0].Name != "x" {
		t.Errorf("step 默认值 = %+v", s.Steps[0])
	}
	if j.Kind != JobKindNormal || j.FailReason != "" || j.SkipReason != "" || len(j.NeedsKeys) != 0 {
		t.Errorf("PlannedJob = %+v", j)
	}
}

func TestExpandSpecFields(t *testing.T) {
	pc := ctx()
	pc.Vars = func(string) map[string]string {
		return map[string]string{"IMG": "docker.io/library/postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	}
	p := mustExpand(t, `on: push
defaults: { run: { shell: sh, working-directory: wf-dir } }
jobs:
  a:
    runs-on: "${{ 'linux' }}/amd64"
    container: docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    inputs: [go.mod, "${{ github.ref_name }}/"]
    cache: false
    network: true
    reproducible: true
    resources: { cpu: 0.5, memory: 512Mi }
    timeout-minutes: 5
    repositories: [o/z, o/a]
    pool: p
    host: h
    strategy: { fail-fast: false, max-parallel: 3 }
    toolcache: [{ name: n, env: E }]
    services:
      zz: { image: "${{ vars.IMG }}", env: { A: "${{ github.sha }}" }, command: [x] }
      aa: { image: "docker.io/library/redis@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }
    defaults: { run: { shell: zsh } }
    steps:
      - run: one
      - run: two
        shell: fish
        working-directory: "${{ github.ref_name }}"
      - run: "\n  three\nfour"
        if: github.ref_name == 'dev'
      - name: named
        run: x
        continue-on-error: true
  b:
    runs-on: linux/arm64
    effects: true
    steps: [{run: x}]
`, pc)
	a := planJob(t, p, "a").Spec
	if a.Platform != "linux/amd64" || a.Executor != ExecutorOCI || a.Container != "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("执行方式 = %s %s %s", a.Platform, a.Executor, a.Container)
	}
	if a.InputsDefault || !slices.Equal(a.Inputs, []string{"go.mod", "main/"}) {
		t.Errorf("inputs = %v（default=%v）", a.Inputs, a.InputsDefault)
	}
	if a.Cache || !a.Network || !a.Reproducible || a.CPU != 0.5 || a.MemoryBytes != 512<<20 || a.TimeoutMinutes != 5 {
		t.Errorf("资源和开关 = %+v", a)
	}
	if !slices.Equal(a.Repositories, []string{"o/a", "o/z"}) || a.Pool != "p" || a.Host != "h" || a.FailFast || a.MaxParallel != 3 {
		t.Errorf("repositories/pool/host/strategy = %+v", a)
	}
	if !reflect.DeepEqual(a.Toolcache, []ToolCache{{Name: "n", Env: "E"}}) {
		t.Errorf("toolcache = %+v", a.Toolcache)
	}
	wantSvcs := []ServiceSpec{
		{Name: "aa", Image: "docker.io/library/redis@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Env: map[string]string{}},
		{Name: "zz", Image: "docker.io/library/postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Env: map[string]string{"A": "abc"}, Command: []string{"x"}},
	}
	if !reflect.DeepEqual(a.Services, wantSvcs) {
		t.Errorf("services 应按名字排序并求值：%+v", a.Services)
	}
	// 第三个 step 的 if 为 false，被去掉
	wantSteps := []StepSpec{
		{Name: "one", Run: "one", Shell: "zsh", WorkingDirectory: "wf-dir"},
		{Name: "two", Run: "two", Shell: "fish", WorkingDirectory: "main"},
		{Name: "named", Run: "x", Shell: "zsh", WorkingDirectory: "wf-dir", ContinueOnError: true},
	}
	if !reflect.DeepEqual(a.Steps, wantSteps) {
		t.Errorf("steps = %+v\n期望 %+v", a.Steps, wantSteps)
	}
	if b := planJob(t, p, "b").Spec; b.Cache || !b.Effects {
		t.Errorf("effects: true 时不缓存：cache=%v effects=%v", b.Cache, b.Effects)
	}
}

func TestExpandStepNameFromRun(t *testing.T) {
	j := planJob(t, mustExpand(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, steps: [{ run: \"\\n  make build\\nmake test\" }] }\n", ctx()), "a")
	if got := j.Spec.Steps[0].Name; got != "make build" {
		t.Errorf("step 名字应取 run 的第一个非空行：%q", got)
	}
}

func TestExpandExecutorErrors(t *testing.T) {
	cases := []struct {
		name string
		job  string
		want string
	}{
		{"未知平台", "{ runs-on: windows/amd64, steps: [{run: x}] }", `job a：runs-on "windows/amd64" 不是已知平台`},
		{"runs-on 表达式出错", "{ runs-on: '${{ nope }}', steps: [{run: x}] }", "job a：runs-on：表达式"},
		{"表达式展开后的 container 没有 digest", "{ runs-on: linux/arm64, container: '${{ github.ref_name }}', steps: [{run: x}] }", "job a：container 必须用 digest 固定：main"},
		{"多个错误都报告", "{ runs-on: linux/arm64, env: { A: '${{ x }}' }, steps: [{ run: '${{ y }}' }] }", "job a：steps.run：表达式 \"y\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := expandSrc(t, "on: push\njobs:\n  a: "+c.job+"\n", ctx())
			wantDiag(t, p.Diags, c.want)
		})
	}
}

func TestValidPlatform(t *testing.T) {
	for p, want := range map[string]bool{
		"linux/arm64": true, "linux/amd64": true, "darwin/arm64": true, "darwin/amd64": true,
		"linux/arm64/musl": true, "linux/amd64/gnu": true,
		"windows/amd64": false, "linux/386": false, "linux/arm64/uclibc": false, "linux": false, "": false, PlatformServer: false,
	} {
		if got := ValidPlatform(p); got != want {
			t.Errorf("ValidPlatform(%q) = %v，期望 %v", p, got, want)
		}
	}
}

// CheckRuntime 按 job 的平台校验每个 runtime；平台无效时不校验。
func TestExpandCheckRuntime(t *testing.T) {
	var calls []string
	pc := ctx()
	pc.CheckRuntime = func(name, platform string) error {
		calls = append(calls, name+"@"+platform)
		if name == "bad" {
			return errors.New("runtime bad 在 lockfile 中没有 " + platform + " 的条目")
		}
		return nil
	}
	p := expandSrc(t, `on: push
jobs:
  a: { runs-on: darwin/arm64, runtimes: [go, bad], steps: [{run: x}] }
  b: { runs-on: plan9/arm, runtimes: [go], steps: [{run: x}] }
`, pc)
	wantDiag(t, p.Diags, "job a：runtime bad 在 lockfile 中没有 darwin/arm64 的条目")
	if !slices.Equal(calls, []string{"go@darwin/arm64", "bad@darwin/arm64"}) {
		t.Errorf("CheckRuntime 的调用 = %v", calls)
	}
}

// if、environment 和 skipped / failed 的判断。
func TestExpandJobStatus(t *testing.T) {
	cases := []struct {
		name     string
		job      string
		bot      bool
		wantSkip string
		wantFail string
	}{
		{"if 为 false", "{ if: \"github.ref_name == 'dev'\", runs-on: linux/arm64, steps: [{run: x}] }", false, "if 条件为 false", ""},
		{"if 写成 ${{ }}", "{ if: \"${{ github.ref_name == 'main' }}\", runs-on: linux/arm64, steps: [{run: x}] }", false, "", ""},
		{"skipped 的 job 不检查尚未实现的功能", "{ if: 'false', runs-on: darwin/arm64, vm: 'v@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', steps: [{run: x}] }", false, "if 条件为 false", ""},
		{"vm 尚未实现", "{ runs-on: darwin/arm64, vm: 'v@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', steps: [{run: x}] }", false, "", "vm（tart executor） 尚未实现（Kiln v0）"},
		{"job 级 concurrency 尚未实现", "{ runs-on: linux/arm64, concurrency: g, steps: [{run: x}] }", false, "", "job 级 concurrency 尚未实现"},
		{"Bot 触发时引用 secret 的 job 跳过", "{ runs-on: linux/arm64, env: { T: '${{ secrets.T }}' }, steps: [{run: x}] }", true, "Bot 触发的 run 不下发 secret（PLAN #16.1）", ""},
		{"Bot 触发时发布 job 跳过", "{ release: { github: {} } }", true, "Bot 触发的 run 不执行发布（PLAN #16.1）", ""},
		{"Bot 触发时普通 job 照常执行", "{ runs-on: linux/arm64, steps: [{run: x}] }", true, "", ""},
		{"Bot 触发时已被 if 跳过的保留原因", "{ if: 'false', release: { github: {} } }", true, "if 条件为 false", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := ctx()
			pc.SenderIsBot = c.bot
			j := planJob(t, mustExpand(t, "on: push\njobs:\n  a: "+c.job+"\n", pc), "a")
			if j.SkipReason != c.wantSkip || !strings.Contains(j.FailReason, c.wantFail) || (c.wantFail == "" && j.FailReason != "") {
				t.Errorf("SkipReason = %q，FailReason = %q；期望 %q、%q", j.SkipReason, j.FailReason, c.wantSkip, c.wantFail)
			}
		})
	}
}

func TestExpandEnvironment(t *testing.T) {
	pc := ctx()
	pc.Vars = func(env string) map[string]string { return map[string]string{"HOST": env + ".example.com"} }
	p := mustExpand(t, `on: push
jobs:
  a:
    runs-on: linux/arm64
    environment: { name: "prod-${{ github.ref_name }}", url: "https://${{ github.sha }}" }
    steps: [{run: x}]
  b:
    runs-on: linux/arm64
    environment: { name: staging, url: "https://${{ vars.HOST }}" }
    steps: [{run: x}]
`, pc)
	if a := planJob(t, p, "a").Spec; a.Environment != "prod-main" || a.EnvironmentURL != "https://abc" {
		t.Errorf("environment = %q %q", a.Environment, a.EnvironmentURL)
	}
	// vars 取自 job 的 environment
	if b := planJob(t, p, "b").Spec; b.Environment != "staging" || b.EnvironmentURL != "https://staging.example.com" {
		t.Errorf("environment = %q %q", b.Environment, b.EnvironmentURL)
	}
}

// 引用 secret 却没有关闭缓存时给出警告，矩阵的多个变体只警告一次。
func TestExpandSecretCacheWarning(t *testing.T) {
	p := mustExpand(t, `on: push
jobs:
  a:
    runs-on: linux/arm64
    strategy: { matrix: { n: [1, 2] } }
    env: { T: "${{ secrets.T }}" }
    steps: [{run: x}]
  b:
    runs-on: linux/arm64
    cache: false
    env: { T: "${{ secrets.T }}" }
    steps: [{run: x}]
`, ctx())
	w := p.Diags.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].String(), "x.yml:3:3: 警告：job a 引用了 secrets 却没有设置 cache: false") {
		t.Errorf("警告 = %v", w)
	}
}

func TestExpandPublishAndImageJobs(t *testing.T) {
	p := mustExpand(t, `on: push
jobs:
  build:
    runs-on: linux/arm64
    steps: [{run: x}]
    outputs: { bin: dist/, pkg: p.tgz }
  publish:
    needs: [build]
    release:
      github: { assets: [{ from: build.bin }] }
  npm:
    needs: [build]
    release:
      npm: [{ from: build.pkg, name: "app-${{ matrix.os }}", access: public }]
  img:
    needs: [build]
    image: { base: "a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", files: [{ from: build.bin, to: / }] }
`, ctx())
	pub := planJob(t, p, "publish")
	if pub.Kind != JobKindPublish || pub.Spec.Executor != ExecutorServer || pub.Spec.Platform != PlatformServer || pub.Spec.Cache {
		t.Errorf("发布 job = %+v %+v", pub, pub.Spec)
	}
	if pub.FailReason != "" || !slices.Equal(pub.NeedsKeys, []string{"build"}) {
		t.Errorf("发布 job 的 FailReason = %q，NeedsKeys = %v", pub.FailReason, pub.NeedsKeys)
	}
	npm := planJob(t, p, "npm")
	if npm.Kind != JobKindPublish || len(npm.Spec.Secrets) != 0 || npm.FailReason != "" {
		t.Errorf("npm 发布 job：Kind = %s，Secrets = %v，FailReason = %q", npm.Kind, npm.Spec.Secrets, npm.FailReason)
	}
	if npm.Spec.Release.Npm[0].Name != "app-${{ matrix.os }}" {
		t.Errorf("npm 包名应原样保留，由 Server 发布时按上游变体求值：%q", npm.Spec.Release.Npm[0].Name)
	}
	img := planJob(t, p, "img")
	if img.Kind != JobKindImage || img.Spec.Executor != ExecutorImage || img.Spec.Platform != PlatformLinuxAny || img.Spec.Image == nil {
		t.Errorf("镜像组装 job = %+v %+v", img, img.Spec)
	}
	if !strings.Contains(img.FailReason, "镜像组装 job（image） 尚未实现") {
		t.Errorf("镜像组装 job 的 FailReason = %q", img.FailReason)
	}
}

// v0 尚未实现的发布功能：job 判为 failed，并说明原因。
func TestNotImplementedRelease(t *testing.T) {
	cases := []struct {
		release string
		want    string
	}{
		{"{ verify-reproducible: true, github: {} }", "release.verify-reproducible 尚未实现"},
		{"{ runtimes: [{ from: build.bin }] }", "release.runtimes 尚未实现"},
		{"{}", "release 中没有任何发布目标"},
		{"{ github: { sign: cosign } }", "release.github.sign: cosign 尚未实现"},
		{"{ github: { provenance: slsa } }", "release.github.provenance: slsa 尚未实现"},
		{"{ github: { sbom: true } }", "release.github.sbom 尚未实现"},
		{"{ github: { notes: { from: commits } } }", "release.github.notes.from: commits 尚未实现"},
		{"{ github: { sign: none, provenance: none, sbom: false, notes: { from: CHANGELOG.md } } }", ""},
	}
	for _, c := range cases {
		t.Run(c.release, func(t *testing.T) {
			p := mustExpand(t, "on: push\njobs:\n"+buildJob+"  publish:\n    needs: [build]\n    release: "+c.release+"\n", ctx())
			j := planJob(t, p, "publish")
			if c.want == "" && j.FailReason != "" || !strings.Contains(j.FailReason, c.want) {
				t.Errorf("FailReason = %q，期望包含 %q", j.FailReason, c.want)
			}
		})
	}
}

// environment 名称中有表达式时，vars 按求值后的名称读取：environment 级 > 仓库级 > 账号级。
// 名称本身只能用账号级、仓库级的 vars（这时还不知道读哪个 environment）。
func TestExpandDynamicEnvironmentVars(t *testing.T) {
	// 模拟三级合并：账号级 ACCOUNT_ONLY、TARGET；仓库级覆盖 TARGET、PREFIX；各 environment 覆盖 TARGET
	envVars := map[string]map[string]string{
		"prod-main":   {"TARGET": "production"},
		"stage-linux": {"TARGET": "staging-linux"},
		"stage-mac":   {"TARGET": "staging-mac"},
		"qa":          {"TARGET": "qa", "PREFIX": "env-level"},
	}
	var asked []string
	pc := ctx()
	pc.GitHub["event_name"] = EventWorkflowDispatch
	pc.Inputs = map[string]string{"target": "qa"}
	pc.Vars = func(env string) map[string]string {
		asked = append(asked, env)
		out := map[string]string{"ACCOUNT_ONLY": "account", "TARGET": "repo-fallback", "PREFIX": "prod"}
		maps.Copy(out, envVars[env])
		return out
	}
	p := mustExpand(t, `on: [push, workflow_dispatch]
jobs:
  gh:
    runs-on: linux/arm64
    environment: "${{ vars.PREFIX }}-${{ github.ref_name }}"
    env: { TARGET: "${{ vars.TARGET }}", ACCOUNT: "${{ vars.ACCOUNT_ONLY }}" }
    steps: [{run: 'echo "$TARGET"'}]
  in:
    runs-on: linux/arm64
    environment: { name: "${{ inputs.target }}", url: "https://${{ vars.TARGET }}.example.com" }
    env: { TARGET: "${{ vars.TARGET }}" }
    steps: [{run: x}]
  mx:
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, mac] } }
    environment: "stage-${{ matrix.os }}"
    env: { TARGET: "${{ vars.TARGET }}" }
    steps: [{run: x}]
`, pc)
	for _, asked := range asked {
		if strings.Contains(asked, "${{") {
			t.Errorf("Vars 收到了未求值的 environment 名称 %q", asked)
		}
	}
	want := map[string]struct{ env, target string }{
		"gh":           {"prod-main", "production"},
		"in":           {"qa", "qa"},
		"mx{os=linux}": {"stage-linux", "staging-linux"},
		"mx{os=mac}":   {"stage-mac", "staging-mac"},
	}
	for _, j := range p.Jobs {
		w, ok := want[j.Key]
		if !ok {
			t.Fatalf("多出的 job %s", j.Key)
		}
		if j.Spec.Environment != w.env || j.Spec.Env["TARGET"] != w.target {
			t.Errorf("%s：environment %q、TARGET %q，期望 %q、%q", j.Key, j.Spec.Environment, j.Spec.Env["TARGET"], w.env, w.target)
		}
	}
	if gh := planJob(t, p, "gh"); gh.Spec.Env["ACCOUNT"] != "account" {
		t.Errorf("account 级的变量 = %q", gh.Spec.Env["ACCOUNT"])
	}
	if in := planJob(t, p, "in"); in.Spec.EnvironmentURL != "https://qa.example.com" {
		t.Errorf("environment.url = %q，应使用 environment 级的变量", in.Spec.EnvironmentURL)
	}
}

// fetch.matrix 中的 vars 同样按求值后的 environment 名称读取：environment 名称中有表达式时，
// 不能用原始的名称（例如 "stage-${{ matrix.os }}"）去读 environment 级的 vars。
func TestExpandFetchMatrixDynamicEnvironment(t *testing.T) {
	var asked []string
	pc := ctx()
	pc.Vars = func(env string) map[string]string {
		asked = append(asked, env)
		out := map[string]string{"ARCH": "amd64"}
		if env == "stage-linux" || env == "stage-mac" {
			out["ARCH"] = "arm64" // environment 级覆盖仓库级
		}
		return out
	}
	p := mustExpand(t, `on: push
jobs:
  build:
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, mac], arch: [amd64, arm64] } }
    steps: [{run: x}]
    outputs: { bin: dist/ }
  test:
    needs: [build]
    runs-on: linux/arm64
    strategy: { matrix: { os: [linux, mac] } }
    environment: "stage-${{ matrix.os }}"
    fetch: [{ from: build.bin, to: bin/, matrix: { os: "${{ matrix.os }}", arch: "${{ vars.ARCH }}" } }]
    steps: [{run: x}]
`, pc)
	for _, a := range asked {
		if strings.Contains(a, "${{") {
			t.Errorf("Vars 收到了未求值的 environment 名称 %q", a)
		}
	}
	n := 0
	for _, j := range p.Jobs {
		if j.JobID != "test" {
			continue
		}
		n++
		want := fmt.Sprintf("build{arch=arm64,os=%v}", j.Matrix["os"])
		if len(j.Spec.Fetch) != 1 || j.Spec.Fetch[0].JobKey != want {
			t.Errorf("%s 的 fetch = %+v，期望来自 %s", j.Key, j.Spec.Fetch, want)
		}
	}
	if n != 2 {
		t.Errorf("test 有 %d 个变体", n)
	}
}

// TestExpandTrustedAutoTag：可信的自动 tag push 保留 Bot 发送者，但允许执行引用 secret 和发布 job（PLAN #29.4）；
// 不可信的 Bot（手动 tag push、其他事件）仍然按 #16.1 跳过。
func TestExpandTrustedAutoTag(t *testing.T) {
	cases := []struct {
		name    string
		trusted bool
	}{{"可信的自动 tag", true}, {"不可信的 Bot", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := ctx()
			pc.SenderIsBot, pc.TrustedAutoTag = true, c.trusted
			p := mustExpand(t, `on: push
jobs:
  secret: { runs-on: linux/arm64, env: { T: '${{ secrets.T }}' }, steps: [{run: x}] }
  publish: { release: { github: {} } }
`, pc)
			for _, key := range []string{"secret", "publish"} {
				j := planJob(t, p, key)
				if c.trusted && j.SkipReason != "" {
					t.Errorf("%s 不应跳过：%s", key, j.SkipReason)
				}
				if !c.trusted && !strings.Contains(j.SkipReason, "Bot") {
					t.Errorf("%s 的 SkipReason = %q", key, j.SkipReason)
				}
			}
		})
	}
}

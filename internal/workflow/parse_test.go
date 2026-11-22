package workflow

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// okJob 是一个合法的最小 job，用于只关心其他部分的用例。
const okJob = "jobs:\n  a: { runs-on: linux/arm64, steps: [{run: x}] }\n"

// mustParse 解析 src，有错误时让测试失败。
func mustParse(t *testing.T, src string) *Workflow {
	t.Helper()
	wf, diags := Parse("x.yml", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("解析出错：\n%s", diags.Error())
	}
	return wf
}

// wantDiag 检查 diags 中有一条包含 want 的错误。
func wantDiag(t *testing.T, diags Diags, want string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatalf("期望有错误（应包含 %q），实际没有", want)
	}
	if !strings.Contains(diags.Error(), want) {
		t.Errorf("诊断中应包含 %q，实际：\n%s", want, diags.Error())
	}
}

// 文件级别的错误：YAML 语法、空文件、顶层结构。
func TestParseFileLevelDiags(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"YAML 语法错误带行号", "on: push\njobs:\n\ta: 1\n", []string{"x.yml:3:0: 错误：YAML 无法解析：yaml: line 3"}},
		{"空文件", "", []string{"x.yml: 错误：文件为空"}},
		{"只有注释", "# 注释\n", []string{"x.yml: 错误：文件为空"}},
		{"顶层不是映射", "- a\n", []string{"x.yml:1:1: 错误：workflow 应为映射"}},
		{"缺少 on", okJob, []string{"x.yml:1:1: 错误：缺少 on"}},
		{"缺少 jobs", "on: push\n", []string{"x.yml:1:1: 错误：缺少 jobs"}},
		{"顶层字段重复", "on: push\non: push\n" + okJob, []string{"x.yml:2:1: 错误：workflow 中字段 on 重复"}},
		// yaml.v3 解码到 yaml.Node 时不检查重复的键：以前后写的值静默覆盖先写的
		{"env 的键重复", "on: push\nenv: { A: 1, A: 2 }\n" + okJob, []string{"x.yml:2:14: 错误：env 中字段 A 重复"}},
		{"on 的事件重复", "on:\n  push: { branches: [main] }\n  push: { branches: [dev] }\n" + okJob, []string{"x.yml:3:3: 错误：on 中字段 push 重复"}},
		{"matrix 的变量重复", "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    strategy: { matrix: { os: [a], os: [b] } }\n    steps: [{run: x}]\n", []string{"x.yml:5:36: 错误：strategy.matrix 中字段 os 重复"}},
		{"services 重复", "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    container: i@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    services: { db: { image: p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa }, db: { image: q@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa } }\n    steps: [{run: x}]\n", []string{"x.yml:6:107: 错误：services 中字段 db 重复"}},
		{"顶层未知字段", "on: push\nfoo: 1\n" + okJob, []string{"x.yml:2:1: 错误：workflow 中有未知字段 foo"}},
		{"permissions 不支持", "on: push\npermissions: read-all\n" + okJob, []string{"x.yml:2:1: 错误：workflow 不支持字段 permissions：job 不直接访问 GitHub"}},
		{"run-name 不支持", "on: push\nrun-name: x\n" + okJob, []string{"workflow 不支持字段 run-name：Kiln 不支持 run-name"}},
		{"YAML 合并键", "on: push\njobs:\n  a:\n    <<: { runs-on: linux/arm64 }\n    steps: [{run: x}]\n", []string{"x.yml:4:5: 错误：不支持 YAML 合并键 <<"}},
		{"defaults 未知字段", "on: push\ndefaults: { shell: bash }\n" + okJob, []string{"defaults 中有未知字段 shell"}},
		{"defaults.run 未知字段", "on: push\ndefaults: { run: { env: x } }\n" + okJob, []string{"defaults.run 中有未知字段 env"}},
		{"concurrency 缺少 group", "on: push\nconcurrency: { cancel-in-progress: true }\n" + okJob, []string{"x.yml:2:14: 错误：concurrency.group 不能为空"}},
		{"concurrency 写成空字符串", "on: push\nconcurrency: ''\n" + okJob, []string{"x.yml:2:14: 错误：concurrency.group 不能为空"}}, // 以前不报错
		{"concurrency 未知字段", "on: push\nconcurrency: { group: g, queue: x }\n" + okJob, []string{"concurrency 中有未知字段 queue"}},
		{"env 不是映射", "on: push\nenv: [a]\n" + okJob, []string{"x.yml:2:6: 错误：env 应为映射"}},
		{"env 的值不是字符串", "on: push\nenv: { A: [1] }\n" + okJob, []string{"env.A 应为字符串"}},
		{"name 不是字符串", "on: push\nname: [a]\n" + okJob, []string{"x.yml:2:7: 错误：应为字符串"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte(c.src))
			for _, w := range c.want {
				wantDiag(t, diags, w)
			}
		})
	}
}

// ai-review 保留给内建的 AI 审计：同名的普通 workflow 会被当成 AI 审计（合并队列的必需验证、Check Run 名称）。
func TestParseReservedName(t *testing.T) {
	for _, f := range []string{"ai-review.yml", "ai-review.yaml", ".kiln/workflows/ai-review.yml"} {
		_, diags := Parse(f, []byte("on: pull_request\n"+okJob))
		wantDiag(t, diags, "保留给 Kiln 的 AI 审计")
	}
	if _, diags := Parse("ai-review-extra.yml", []byte("on: pull_request\n"+okJob)); diags.HasErrors() {
		t.Fatalf("ai-review-extra.yml 不应报错：%v", diags)
	}
}

// on 的各种写法和其中的错误。
func TestParseOn(t *testing.T) {
	t.Run("单个事件名", func(t *testing.T) {
		wf := mustParse(t, "on: push\n"+okJob)
		if wf.On.Push == nil || wf.On.PullRequest != nil {
			t.Errorf("On = %+v，期望只有 push", wf.On)
		}
	})
	t.Run("事件名列表", func(t *testing.T) {
		wf := mustParse(t, "on: [push, pull_request, workflow_dispatch]\n"+okJob)
		if wf.On.Push == nil || wf.On.PullRequest == nil || wf.On.WorkflowDispatch == nil {
			t.Errorf("On = %+v，期望有 push、pull_request、workflow_dispatch", wf.On)
		}
	})
	t.Run("完整的过滤条件", func(t *testing.T) {
		wf := mustParse(t, `on:
  push:
    branches: [main, "release/**"]
    tags-ignore: v0*
    paths-ignore: ["docs/**"]
  pull_request:
    branches-ignore: [wip]
    paths: ["src/**", "!src/gen/**"]
    types: [opened, ready_for_review]
  workflow_dispatch:
    inputs:
      level:
        description: 级别
        type: choice
        options: [low, high]
        default: low
        required: true
      dry:
        type: boolean
  schedule:
    - cron: "0 1 * * *"
    - cron: "0 2 * * 1"
`+okJob)
		p := wf.On.Push
		if !reflect.DeepEqual(p.Branches, []string{"main", "release/**"}) || !reflect.DeepEqual(p.TagsIgnore, []string{"v0*"}) ||
			!reflect.DeepEqual(p.PathsIgnore, []string{"docs/**"}) || p.Tags != nil || p.Paths != nil {
			t.Errorf("push = %+v", p)
		}
		pr := wf.On.PullRequest
		if !reflect.DeepEqual(pr.BranchesIgnore, []string{"wip"}) || !reflect.DeepEqual(pr.Paths, []string{"src/**", "!src/gen/**"}) ||
			!reflect.DeepEqual(pr.Types, []string{"opened", "ready_for_review"}) {
			t.Errorf("pull_request = %+v", pr)
		}
		want := []DispatchInput{
			{Name: "level", Description: "级别", Type: InputChoice, Default: "low", Required: true, Options: []string{"low", "high"}},
			{Name: "dry", Type: InputBoolean},
		}
		if !reflect.DeepEqual(wf.On.WorkflowDispatch.Inputs, want) {
			t.Errorf("inputs = %+v\n期望 %+v", wf.On.WorkflowDispatch.Inputs, want)
		}
		if !reflect.DeepEqual(wf.On.Schedule, []string{"0 1 * * *", "0 2 * * 1"}) {
			t.Errorf("schedule = %v", wf.On.Schedule)
		}
	})
	t.Run("input 默认类型是 string", func(t *testing.T) {
		wf := mustParse(t, "on: { workflow_dispatch: { inputs: { v: { default: x } } } }\n"+okJob)
		if in := wf.On.WorkflowDispatch.Inputs[0]; in.Type != InputString || in.Default != "x" {
			t.Errorf("input = %+v", in)
		}
	})

	cases := []struct {
		name string
		on   string
		want string
	}{
		{"不支持的事件", "on: issues", "x.yml:1:5: 错误：不支持的触发事件 issues（支持 push、pull_request、workflow_dispatch、schedule）"},
		{"列表中不支持的事件", "on: [push, release]", "x.yml:1:12: 错误：不支持的触发事件 release"},
		{"映射中不支持的事件", "on: { pull_request_target: {} }", "不支持的触发事件 pull_request_target"},
		{"push 不是映射", "on: { push: [main] }", "on.push 应为映射"},
		{"push 未知字段", "on: { push: { types: [x] } }", "on.push 中有未知字段 types"},
		{"push 同时写 branches 和 branches-ignore", "on: { push: { branches: [a], branches-ignore: [b] } }", "on.push 不能同时使用 branches 和 branches-ignore"},
		{"push 同时写 tags 和 tags-ignore", "on: { push: { tags: [a], tags-ignore: [b] } }", "on.push 不能同时使用 tags 和 tags-ignore"},
		{"push 同时写 paths 和 paths-ignore", "on: { push: { paths: [a], paths-ignore: [b] } }", "on.push 不能同时使用 paths 和 paths-ignore"},
		{"push 的过滤模式不是字符串", "on: { push: { branches: { a: b } } }", "应为字符串或字符串列表"},
		{"push 的无效 tag 模式", "on: { push: { tags: ['v[1'] } }", `无效的匹配模式 "v[1"`},
		{"否定的无效模式", "on: { push: { paths: ['!a/[x'] } }", `无效的匹配模式 "!a/[x"`},
		{"pull_request 不支持 tags", "on: { pull_request: { tags: [v1] } }", "on.pull_request 中有未知字段 tags"},
		{"pull_request 不支持的 types", "on: { pull_request: { types: [opened, closed] } }", "on.pull_request.types 只支持 opened、synchronize、reopened、ready_for_review，不支持 closed"},
		{"pull_request 同时写 branches 和 branches-ignore", "on: { pull_request: { branches: [a], branches-ignore: [b] } }", "on.pull_request 不能同时使用 branches 和 branches-ignore"},
		{"pull_request 同时写 paths 和 paths-ignore", "on: { pull_request: { paths: [a], paths-ignore: [b] } }", "on.pull_request 不能同时使用 paths 和 paths-ignore"},
		{"workflow_dispatch 未知字段", "on: { workflow_dispatch: { secrets: {} } }", "on.workflow_dispatch 中有未知字段 secrets"},
		{"workflow_dispatch.inputs 不是映射", "on: { workflow_dispatch: { inputs: [a] } }", "on.workflow_dispatch.inputs 应为映射"},
		{"input 类型不支持", "on: { workflow_dispatch: { inputs: { n: { type: number } } } }", "input n 的类型只能是 string、boolean、choice"},
		{"choice 没有 options", "on: { workflow_dispatch: { inputs: { n: { type: choice } } } }", "input n 的类型是 choice，需要 options"},
		{"inputs 重复", "on: { workflow_dispatch: { inputs: { n: {}, n: {} } } }", "on.workflow_dispatch.inputs 中字段 n 重复"},
		{"input 未知字段", "on: { workflow_dispatch: { inputs: { n: { min: 1 } } } }", "input n 中有未知字段 min"},
		{"input required 不是布尔值", "on: { workflow_dispatch: { inputs: { n: { required: yes } } } }", "应为 true 或 false"},
		{"schedule 不是列表", "on: { schedule: { cron: x } }", "on.schedule 应为列表"},
		{"schedule 项未知字段", "on: { schedule: [{ cron: x, tz: UTC }] }", "on.schedule[] 中有未知字段 tz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte(c.on+"\n"+okJob))
			wantDiag(t, diags, c.want)
		})
	}
}

// job 中各字段的解析结果。
func TestParseJobFields(t *testing.T) {
	wf := mustParse(t, jobFieldsYAML)
	checkWorkflowFields(t, wf)
	checkJobAFields(t, wf.Job("a"))
	checkJobANested(t, wf.Job("a"))
	checkJobBFields(t, wf.Job("b"))
	if wf.Job("nope") != nil {
		t.Error("不存在的 job 应返回 nil")
	}
}

const jobFieldsYAML = `on: push
defaults:
  run: { shell: sh, working-directory: w }
concurrency: ci-${{ github.ref }}
jobs:
  a:
    name: A job
    runs-on: [linux/arm64]
    pool: fast
    host: mini
    container: { image: "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }
    runtimes: go
    inputs: []
    outputs: { bin: dist/ }
    network: TRUE
    reproducible: false
    cache: false
    resources: { cpu: 1.5, memory: 2Gi }
    toolcache: [{ name: go-build, env: GOCACHE }]
    services:
      pg: { image: "docker.io/library/postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", env: { POSTGRES_PASSWORD: x, EMPTY: ~ }, command: [postgres, -c, fsync=off] }
    repositories: [o/b, o/a]
    effects: true
    buildkit: true
    timeout-minutes: 15
    environment: { name: prod, url: "https://x" }
    concurrency: { group: g, cancel-in-progress: "${{ true }}" }
    strategy: { fail-fast: false, max-parallel: 2, matrix: { os: [a] } }
    env: { B: "2", A: "1" }
    defaults: { run: { shell: zsh } }
    steps:
      - id: s1
        name: build
        if: always
        run: make
        shell: bash
        working-directory: sub
        env: { X: y }
        continue-on-error: true
  b:
    needs: a
    runs-on: linux/arm64
    environment: staging
    container: "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    fetch: [{ from: a.bin, to: in/, matrix: { os: a } }]
    steps: [{ run: x }]
`

func checkWorkflowFields(t *testing.T, wf *Workflow) {
	t.Helper()
	if wf.Name != "x" {
		t.Errorf("没有 name 时用文件名：%q", wf.Name)
	}
	if wf.Defaults != (Defaults{Shell: "sh", WorkingDirectory: "w"}) {
		t.Errorf("workflow defaults = %+v", wf.Defaults)
	}
	if c := wf.Concurrency; c.Group != "ci-${{ github.ref }}" || c.CancelInProgress != "false" || c.Pos != (Pos{4, 14}) {
		t.Errorf("concurrency 简写 = %+v", c)
	}
}

func checkJobAFields(t *testing.T, a *Job) {
	t.Helper()
	if a.Pos != (Pos{6, 3}) || a.Name != "A job" || a.RunsOn != "linux/arm64" || a.Pool != "fast" || a.Host != "mini" ||
		a.Container != "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || !reflect.DeepEqual(a.Runtimes, []string{"go"}) {
		t.Errorf("job a 的基本字段 = %+v", a)
	}
	if a.Inputs == nil || len(a.Inputs) != 0 {
		t.Errorf("inputs: [] 应得到非 nil 的空切片：%#v", a.Inputs)
	}
	if !a.Network || a.Reproducible || a.Cache == nil || *a.Cache || !a.Effects || !a.Buildkit || a.TimeoutMinutes != 15 {
		t.Errorf("job a 的布尔和整数字段 = %+v", a)
	}
	if a.Resources != (Resources{CPU: 1.5, Memory: "2Gi"}) || !reflect.DeepEqual(a.Toolcache, []ToolCache{{Name: "go-build", Env: "GOCACHE"}}) {
		t.Errorf("resources/toolcache = %+v %+v", a.Resources, a.Toolcache)
	}
}

func checkJobANested(t *testing.T, a *Job) {
	t.Helper()
	svc := a.Services[0]
	if svc.Name != "pg" || !reflect.DeepEqual(svc.Env.Keys, []string{"POSTGRES_PASSWORD", "EMPTY"}) || svc.Env.Values["EMPTY"] != "" ||
		!reflect.DeepEqual(svc.Command, []string{"postgres", "-c", "fsync=off"}) {
		t.Errorf("services = %+v", svc)
	}
	if a.Environment != "prod" || a.EnvironmentURL != "https://x" {
		t.Errorf("environment = %q %q", a.Environment, a.EnvironmentURL)
	}
	if c := a.Concurrency; c == nil || c.Group != "g" || c.CancelInProgress != "${{ true }}" {
		t.Errorf("job concurrency = %+v", c)
	}
	if s := a.Strategy; s.FailFast == nil || *s.FailFast || s.MaxParallel != 2 || !reflect.DeepEqual(s.MatrixOrder, []string{"os"}) {
		t.Errorf("strategy = %+v", s)
	}
	if !reflect.DeepEqual(a.Env.Keys, []string{"B", "A"}) {
		t.Errorf("env 应保留声明顺序：%v", a.Env.Keys)
	}
	if a.Defaults.Shell != "zsh" {
		t.Errorf("job defaults = %+v", a.Defaults)
	}
	st := a.Steps[0]
	if st.ID != "s1" || st.Name != "build" || st.If != "always" || st.Run != "make" || st.Shell != "bash" ||
		st.WorkingDirectory != "sub" || st.Env.Values["X"] != "y" || !st.ContinueOnError || st.Pos != (Pos{32, 9}) {
		t.Errorf("step = %+v", st)
	}
}

func checkJobBFields(t *testing.T, b *Job) {
	t.Helper()
	if !reflect.DeepEqual(b.Needs, []string{"a"}) || b.Environment != "staging" || b.Container != "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("job b = %+v", b)
	}
	if f := b.Fetch[0]; f.From != "a.bin" || f.To != "in/" || f.Matrix["os"] != "a" {
		t.Errorf("fetch = %+v", f)
	}
	if j, out := b.Fetch[0].Split(); j != "a" || out != "bin" {
		t.Errorf("Split() = %q %q", j, out)
	}
}

// job 中各种不支持的字段和写法都给出带位置的诊断。
func TestParseJobDiags(t *testing.T) {
	cases := []struct {
		name string
		job  string // jobs.a 的内容（flow 写法，第 3 行开始）
		want string
	}{
		{"job 级 uses", "{ uses: o/r/.github/workflows/x.yml@v1 }", "x.yml:3:8: 错误：job a 不支持字段 uses：Kiln 不运行 marketplace action"},
		{"job 级 permissions", "{ runs-on: linux/arm64, permissions: {}, steps: [{run: x}] }", "job a 不支持字段 permissions：job 不直接访问 GitHub，发布由 Server 完成"},
		{"job 级 continue-on-error", "{ runs-on: linux/arm64, continue-on-error: true, steps: [{run: x}] }", "job a 不支持字段 continue-on-error：job 级 continue-on-error 不支持"},
		{"job 级 secrets", "{ runs-on: linux/arm64, secrets: inherit, steps: [{run: x}] }", "job a 不支持字段 secrets：不支持"},
		{"job 级 with", "{ runs-on: linux/arm64, with: {}, steps: [{run: x}] }", "job a 不支持字段 with：不支持（没有 uses）"},
		{"step 级 uses", "{ runs-on: linux/arm64, steps: [{ uses: actions/checkout@v4 }] }", "x.yml:3:40: 错误：step 不支持字段 uses"},
		{"step 级 with", "{ runs-on: linux/arm64, steps: [{ run: x, with: {} }] }", "step 不支持字段 with：不支持（没有 uses）"},
		{"step 级 timeout-minutes", "{ runs-on: linux/arm64, steps: [{ run: x, timeout-minutes: 1 }] }", "step 级超时不支持，请使用 job 级 timeout-minutes"},
		{"step 缺少 run", "{ runs-on: linux/arm64, steps: [{ name: x }] }", "x.yml:3:38: 错误：step 缺少 run"},
		{"step 未知字段", "{ runs-on: linux/arm64, steps: [{ run: x, foo: 1 }] }", "step 中有未知字段 foo"},
		{"steps 不是列表", "{ runs-on: linux/arm64, steps: x }", "steps 应为列表"},
		{"steps 为空", "{ runs-on: linux/arm64, steps: [] }", "job a 的 steps 不能为空"},
		{"没有 steps、release、image", "{ runs-on: linux/arm64 }", "x.yml:3:3: 错误：job a 需要 steps、release 或 image 之一"},
		{"steps 和 image 同时存在", "{ image: { base: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa }, steps: [{run: x}] }", "job a 的 steps、release、image 只能有一个"},
		{"job 不是映射", "[a]", "job a 应为映射"},
		{"runs-on 写了多个平台", "{ runs-on: [linux/arm64, self-hosted], steps: [{run: x}] }", "runs-on 应为一个平台，例如 linux/arm64、darwin/arm64"},
		{"缺少 runs-on", "{ steps: [{run: x}] }", "job a 缺少 runs-on"},
		{"container.env", "{ runs-on: linux/arm64, container: { image: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, env: {} }, steps: [{run: x}] }", "container 不支持字段 env：请把 env 写在 job 级"},
		{"container.credentials", "{ runs-on: linux/arm64, container: { image: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, credentials: {} }, steps: [{run: x}] }", "container 不支持字段 credentials：v1 只支持不需要登录的镜像"},
		{"container.options", "{ runs-on: linux/arm64, container: { image: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, options: --privileged }, steps: [{run: x}] }", "container 不支持字段 options：不支持"},
		{"container.ports", "{ runs-on: linux/arm64, container: { image: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, ports: [80] }, steps: [{run: x}] }", "container 不支持字段 ports：不需要"},
		{"services 不是映射", "{ runs-on: linux/arm64, container: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: [pg], steps: [{run: x}] }", "services 应为映射"},
		{"services 缺少 image", "{ runs-on: linux/arm64, container: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: { pg: { env: {} } }, steps: [{run: x}] }", "services.pg 缺少 image"},
		{"services.ports", "{ runs-on: linux/arm64, container: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: { pg: { image: p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, ports: [5432] } }, steps: [{run: x}] }", "services.pg 不支持字段 ports：不需要，job 通过服务名访问"},
		{"services.credentials", "{ runs-on: linux/arm64, container: x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, services: { pg: { image: p@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, credentials: {} } }, steps: [{run: x}] }", "services.pg 不支持字段 credentials"},
		{"environment 未知字段", "{ runs-on: linux/arm64, environment: { name: p, reviewers: [] }, steps: [{run: x}] }", "environment 中有未知字段 reviewers"},
		{"resources.cpu 为 0", "{ runs-on: linux/arm64, resources: { cpu: 0 }, steps: [{run: x}] }", "resources.cpu 应为正数"},
		{"resources.cpu 为负数", "{ runs-on: linux/arm64, resources: { cpu: -1 }, steps: [{run: x}] }", "resources.cpu 应为正数"},
		{"resources.cpu 不是数字", "{ runs-on: linux/arm64, resources: { cpu: many }, steps: [{run: x}] }", "resources.cpu 应为正数"},
		{"resources.cpu 为 NaN", "{ runs-on: linux/arm64, resources: { cpu: .nan }, steps: [{run: x}] }", "resources.cpu 应为正数"},
		{"resources.cpu 为无穷大", "{ runs-on: linux/arm64, resources: { cpu: .inf }, steps: [{run: x}] }", "resources.cpu 应为正数"},
		{"resources.memory 无效", "{ runs-on: linux/arm64, resources: { memory: 4GB }, steps: [{run: x}] }", `resources.memory：无效的大小 "4GB"`},
		{"resources 未知字段", "{ runs-on: linux/arm64, resources: { gpu: 1 }, steps: [{run: x}] }", "resources 中有未知字段 gpu"},
		{"toolcache 缺少 env", "{ runs-on: linux/arm64, toolcache: [{ name: x }], steps: [{run: x}] }", "toolcache 的每一项都需要 name 和 env"},
		{"toolcache name 含路径", "{ runs-on: linux/arm64, toolcache: [{ name: ../x, env: GOCACHE }], steps: [{run: x}] }", "toolcache 的 name"},
		{"toolcache name 含分隔符", "{ runs-on: linux/arm64, toolcache: [{ name: a/b, env: GOCACHE }], steps: [{run: x}] }", "toolcache 的 name"},
		{"toolcache env 覆盖 PATH", "{ runs-on: linux/arm64, toolcache: [{ name: x, env: PATH }], steps: [{run: x}] }", "toolcache 的 env"},
		{"toolcache env 覆盖 KILN", "{ runs-on: linux/arm64, toolcache: [{ name: x, env: KILN_WORKSPACE }], steps: [{run: x}] }", "toolcache 的 env"},
		{"toolcache env 覆盖 GIT", "{ runs-on: linux/arm64, toolcache: [{ name: x, env: GIT_CONFIG_GLOBAL }], steps: [{run: x}] }", "toolcache 的 env"},
		{"toolcache env 名字非法", "{ runs-on: linux/arm64, toolcache: [{ name: x, env: 1A }], steps: [{run: x}] }", "toolcache 的 env"},
		{"fetch 缺少 to", "{ runs-on: linux/arm64, fetch: [{ from: b.o }], steps: [{run: x}] }", "fetch 的每一项都需要 from 和 to"},
		{"fetch 不是列表", "{ runs-on: linux/arm64, fetch: { from: b.o }, steps: [{run: x}] }", "fetch 应为列表"},
		{"strategy 未知字段", "{ runs-on: linux/arm64, strategy: { matrix: { a: [1] }, retries: 1 }, steps: [{run: x}] }", "strategy 中有未知字段 retries"},
		{"matrix 是列表", "{ runs-on: linux/arm64, strategy: { matrix: [1] }, steps: [{run: x}] }", "strategy.matrix 应为映射"},
		{"matrix 变量是映射", "{ runs-on: linux/arm64, strategy: { matrix: { os: { a: 1 } } }, steps: [{run: x}] }", "matrix.os 应为列表"},
		{"max-parallel 不是整数", "{ runs-on: linux/arm64, strategy: { max-parallel: two }, steps: [{run: x}] }", "应为整数"},
		{"timeout-minutes 不是整数", "{ runs-on: linux/arm64, timeout-minutes: 1.5, steps: [{run: x}] }", "应为整数"},
		{"布尔字段写成 yes", "{ runs-on: linux/arm64, network: yes, steps: [{run: x}] }", "应为 true 或 false"},
		{"needs 是映射", "{ runs-on: linux/arm64, needs: { b: 1 }, steps: [{run: x}] }", "应为字符串或字符串列表"},
		{"outputs 不是映射", "{ runs-on: linux/arm64, outputs: [a], steps: [{run: x}] }", "outputs 应为映射"},
		{"step env 的值不是字符串", "{ runs-on: linux/arm64, steps: [{ run: x, env: { A: { b: 1 } } }] }", "env.A 应为字符串"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\njobs:\n  a: "+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}
}

func TestParseJobsDiags(t *testing.T) {
	cases := []struct {
		name string
		jobs string
		want string
	}{
		{"jobs 不是映射", "jobs: [a]", "x.yml:2:7: 错误：jobs 应为映射"},
		{"jobs 为空", "jobs: {}", "x.yml:2:7: 错误：jobs 不能为空"},
		{"job ID 以数字开头", "jobs:\n  1a: { runs-on: linux/arm64, steps: [{run: x}] }", `x.yml:3:3: 错误：job ID "1a" 只能包含字母、数字、_ 和 -，且以字母或 _ 开头`},
		{"job ID 有点号", "jobs:\n  a.b: { runs-on: linux/arm64, steps: [{run: x}] }", `job ID "a.b" 只能包含`},
		{"job ID 重复", "jobs:\n  a: { runs-on: linux/arm64, steps: [{run: x}] }\n  a: { runs-on: linux/arm64, steps: [{run: y}] }", "x.yml:4:3: 错误：job a 重复"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte("on: push\n"+c.jobs+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("合法的 job ID", func(t *testing.T) {
		wf := mustParse(t, "on: push\njobs:\n  _a-1_B: { runs-on: linux/arm64, steps: [{run: x}] }\n")
		if wf.Job("_a-1_B") == nil {
			t.Error("找不到 job _a-1_B")
		}
	})
}

// 别名展开后与直接写相同。
func TestParseAlias(t *testing.T) {
	wf := mustParse(t, `on: push
env: &e { A: "1" }
jobs:
  a:
    runs-on: linux/arm64
    env: *e
    steps: [{run: x}]
`)
	if wf.Job("a").Env.Values["A"] != "1" {
		t.Errorf("别名没有展开：%+v", wf.Job("a").Env)
	}
}

func TestParseSingleDocument(t *testing.T) {
	const workflow = "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps: [{run: x}]\n"
	for _, suffix := range []string{"---\non: release\n", "---\n", "---\ninvalid: [\n"} {
		t.Run(suffix, func(t *testing.T) {
			_, diags := Parse("ci.yml", []byte(workflow+suffix))
			if !diags.HasErrors() {
				t.Fatal("尾随文档或语法错误应被拒绝")
			}
		})
	}
	for _, suffix := range []string{"\n# trailing comment\n", "...\n"} {
		mustParse(t, workflow+suffix)
	}
}

func TestParseYAMLResourceLimits(t *testing.T) {
	const prefix = "on: push\njobs:\n  a:\n    runs-on: linux/arm64\n    steps: [{run: x}]\n    strategy:\n      matrix:\n"
	var expanded strings.Builder
	expanded.WriteString("        a0: &a0 [x]\n")
	for i := 1; i <= 16; i++ {
		fmt.Fprintf(&expanded, "        a%d: &a%d [*a%d, *a%d]\n", i, i, i-1, i-1)
	}
	for _, tc := range []struct{ name, matrix, want string }{
		{"循环序列", "        x: &x [*x]\n", "别名形成循环"},
		{"循环映射", "        x: &x [{nested: *x}]\n", "别名形成循环"},
		{"深层嵌套", "        x: " + strings.Repeat("[", maxYAMLDepth) + "0" + strings.Repeat("]", maxYAMLDepth) + "\n", "嵌套超过"},
		{"指数展开", expanded.String(), "展开后超过"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := Parse("ci.yml", []byte(prefix+tc.matrix))
			wantDiag(t, diags, tc.want)
			if diags[0].Line == 0 {
				t.Fatalf("诊断缺少行号：%+v", diags)
			}
		})
	}
}

func TestParseSharedMatrixAliases(t *testing.T) {
	wf := mustParse(t, `on: push
jobs:
  a:
    runs-on: linux/arm64
    steps: [{run: x}]
    strategy:
      matrix:
        first: &values [1, 2]
        second: *values
`)
	matrix, ok := wf.Job("a").Strategy.Matrix.(map[string]any)
	if !ok || !reflect.DeepEqual(matrix["first"], matrix["second"]) {
		t.Fatalf("共享别名没有正确展开：%#v", matrix)
	}
}

// matrix 的值和 include 中的变量名顺序。
func TestParseMatrix(t *testing.T) {
	wf := mustParse(t, `on: push
jobs:
  a:
    runs-on: linux/arm64
    strategy:
      matrix:
        os: [linux, darwin]
        n: [1, 2.5, true, null]
        dyn: ${{ fromJSON(vars.LIST) }}
        include:
          - { extra: x, os: linux }
          - { zz: 1, extra: y }
          - notamap
        exclude: [{ os: darwin, n: 1 }]
    steps: [{run: x}]
`)
	s := wf.Job("a").Strategy
	if !reflect.DeepEqual(s.MatrixOrder, []string{"os", "n", "dyn"}) {
		t.Errorf("MatrixOrder = %v", s.MatrixOrder)
	}
	if !reflect.DeepEqual(s.IncludeOrder, []string{"extra", "os", "zz"}) {
		t.Errorf("IncludeOrder = %v", s.IncludeOrder)
	}
	m, ok := s.Matrix.(map[string]any)
	if !ok {
		t.Fatalf("Matrix 应为映射：%T", s.Matrix)
	}
	if !reflect.DeepEqual(m["n"], []any{1.0, 2.5, true, nil}) {
		t.Errorf("n 的值应按 YAML 类型转换：%#v", m["n"])
	}
	if m["dyn"] != "${{ fromJSON(vars.LIST) }}" {
		t.Errorf("表达式应原样保留：%#v", m["dyn"])
	}

	wf = mustParse(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, strategy: { matrix: '${{ fromJSON(vars.M) }}' }, steps: [{run: x}] }\n")
	if wf.Job("a").Strategy.Matrix != "${{ fromJSON(vars.M) }}" {
		t.Errorf("整个矩阵是表达式时应原样保留：%#v", wf.Job("a").Strategy.Matrix)
	}
}

func TestScalarValue(t *testing.T) {
	wf := mustParse(t, "on: push\njobs:\n  a: { runs-on: linux/arm64, strategy: { matrix: { v: [0x1F, 1e3, 0o17, TRUE, False, ~, '1', abc] } }, steps: [{run: x}] }\n")
	m, ok := wf.Job("a").Strategy.Matrix.(map[string]any)
	if !ok {
		t.Fatalf("Matrix 应为映射：%T", wf.Job("a").Strategy.Matrix)
	}
	got := m["v"]
	want := []any{31.0, 1000.0, 15.0, true, false, nil, "1", "abc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("标量转换 = %#v\n期望 %#v", got, want)
	}
}

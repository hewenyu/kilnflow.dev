package planner

import (
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// envWorkflow：deploy 引用 environment staging。
const envWorkflow = `name: env
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: linux/arm64
    environment: staging
    steps:
      - run: echo deploy
`

// TestExpandEnvironment：job 引用的 environment 必须先在 Kiln 中创建，否则 run 以配置错误失败；
// ref 策略不允许时 job 在规划时直接失败（PLAN #6.1、#16.4）。仓库级 environment 整体取代同名的账号级 environment。
func TestExpandEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		envs map[string]envOpts // environment 名 → 设置；键带 "repo:" 前缀的是仓库级
		// wantErr 非空时 run 应以配置错误失败，错误中包含它
		wantErr   string
		wantState store.JobState
		inReason  string
	}{
		{name: "environment 不存在", wantErr: "environment staging 不存在"},
		{name: "账号级、不限制 ref", envs: map[string]envOpts{"staging": {}}, wantState: store.JobPending},
		{name: "名字不区分大小写", envs: map[string]envOpts{"Staging": {}}, wantState: store.JobPending},
		{name: "ref 策略允许", envs: map[string]envOpts{"staging": {refPolicy: []string{"refs/heads/*"}}}, wantState: store.JobPending},
		{
			name: "ref 策略不允许", envs: map[string]envOpts{"staging": {refPolicy: []string{"refs/tags/*"}}},
			wantState: store.JobFailed, inReason: "environment staging 的 ref 策略不允许 refs/heads/main",
		},
		{
			name:      "仓库级取代账号级（不允许）",
			envs:      map[string]envOpts{"staging": {refPolicy: []string{"refs/heads/*"}}, "repo:staging": {repoLevel: true, refPolicy: []string{"refs/tags/*"}}},
			wantState: store.JobFailed, inReason: "ref 策略不允许",
		},
		{
			name:      "仓库级取代账号级（允许）",
			envs:      map[string]envOpts{"staging": {refPolicy: []string{"refs/tags/*"}}, "repo:staging": {repoLevel: true, refPolicy: []string{"refs/heads/main"}}},
			wantState: store.JobPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			for name, o := range tt.envs {
				e.addEnvironment(strings.TrimPrefix(name, "repo:"), o)
			}
			sha := e.commit(map[string]string{wf("env.yml"): envWorkflow})
			r := e.planOne(e.pushReq("refs/heads/main", sha))
			if tt.wantErr != "" {
				if r.State != store.RunFailed || !strings.Contains(deref(r.ConfigError), tt.wantErr) {
					t.Fatalf("run 应以配置错误失败（包含 %q）：state=%s config_error=%q", tt.wantErr, r.State, deref(r.ConfigError))
				}
				return
			}
			j := e.jobs(r.ID, 0)["deploy"]
			if j.State != tt.wantState || !strings.Contains(deref(j.Reason), tt.inReason) {
				t.Fatalf("deploy：state=%s reason=%q，期望 %s、包含 %q", j.State, deref(j.Reason), tt.wantState, tt.inReason)
			}
			if deref(j.Environment) != "staging" {
				t.Errorf("job 应记录 environment：%q", deref(j.Environment))
			}
			if tt.wantState == store.JobFailed {
				// 唯一的 job 在规划时失败，run 随即结束
				if got := e.run(r.ID); got.State != store.RunFailed {
					t.Errorf("run 应失败：%s", got.State)
				}
			}
		})
	}
}

// varsWorkflow：job 的 if 引用变量；b 在 environment staging 中求值。
const varsWorkflow = `name: vars
on:
  push:
    branches: [main]
jobs:
  a:
    if: ${{ vars.ENABLE == 'yes' }}
    runs-on: linux/arm64
    steps:
      - run: echo a
  b:
    if: ${{ vars.LEVEL == 'env' }}
    environment: staging
    runs-on: linux/arm64
    steps:
      - run: echo b
`

// TestExpandVariables：规划时用 Kiln 中的变量求值 if（environment > 仓库 > 账号）；为 false 的 job 直接 skipped。
func TestExpandVariables(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		vars         [][3]string // 作用域、名称、值
		wantA, wantB store.JobState
	}{
		{"没有变量", nil, store.JobSkipped, store.JobSkipped},
		{"账号级", [][3]string{{"owner", "ENABLE", "yes"}}, store.JobPending, store.JobSkipped},
		{"仓库级覆盖账号级", [][3]string{{"owner", "ENABLE", "yes"}, {"repo", "ENABLE", "no"}}, store.JobSkipped, store.JobSkipped},
		{"environment 级只对引用它的 job 可见", [][3]string{{"environment", "LEVEL", "env"}, {"environment", "ENABLE", "yes"}}, store.JobSkipped, store.JobPending},
		{"environment 级覆盖仓库级", [][3]string{{"repo", "LEVEL", "repo"}, {"environment", "LEVEL", "env"}}, store.JobSkipped, store.JobPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.addEnvironment("staging", envOpts{})
			var envID int64
			if err := e.db.QueryRow(e.ctx, `SELECT id FROM environments WHERE name='staging'`).Scan(&envID); err != nil {
				t.Fatal(err)
			}
			scopeID := map[string]int64{"owner": e.repo.OwnerID, "repo": e.repo.ID, "environment": envID}
			for _, v := range tt.vars {
				e.exec(`INSERT INTO variables (scope_kind, scope_id, name, value) VALUES ($1,$2,$3,$4)`, v[0], scopeID[v[0]], v[1], v[2])
			}
			sha := e.commit(map[string]string{wf("vars.yml"): varsWorkflow})
			r := e.planOne(e.pushReq("refs/heads/main", sha))
			jobs := e.jobs(r.ID, 0)
			for key, want := range map[string]store.JobState{"a": tt.wantA, "b": tt.wantB} {
				j := jobs[key]
				if j.State != want {
					t.Errorf("job %s 的状态 = %s（%s），期望 %s", key, j.State, deref(j.Reason), want)
				}
				if want == store.JobSkipped && !strings.Contains(deref(j.Reason), "if 条件为 false") {
					t.Errorf("job %s 跳过的原因 = %q", key, deref(j.Reason))
				}
			}
		})
	}
}

// TestExpandBotSender：Bot 触发的 run 中引用 secret 的 job 被跳过（PLAN #16.1），其他 job 照常。
func TestExpandBotSender(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{wf("bot.yml"): `name: bot
on:
  push:
    branches: [main]
jobs:
  plain:
    runs-on: linux/arm64
    steps:
      - run: echo plain
  secret:
    runs-on: linux/arm64
    env:
      TOKEN: ${{ secrets.TOKEN }}
    steps:
      - run: echo secret
`})
	req := e.pushReq("refs/heads/main", sha)
	req.SenderLogin, req.SenderIsBot = "dependabot[bot]", true
	r := e.planOne(req)
	jobs := e.jobs(r.ID, 0)
	if j := jobs["plain"]; j.State != store.JobPending {
		t.Errorf("不引用 secret 的 job 应照常执行：%s", j.State)
	}
	if j := jobs["secret"]; j.State != store.JobSkipped || !strings.Contains(deref(j.Reason), "Bot") {
		t.Errorf("引用 secret 的 job 应被跳过：%s %q", j.State, deref(j.Reason))
	}
}

// publishWorkflow 返回 build → publish 的发布 workflow；release 是 publish 的 release.github 中除 assets 外的内容。
func publishWorkflow(environment, release string) string {
	env := ""
	if environment != "" {
		env = "    environment: " + environment + "\n"
	}
	return `name: rel
on:
  push:
    branches: [main]
    tags: ["v*"]
jobs:
  build:
    runs-on: linux/arm64
    steps:
      - run: mkdir -p dist && echo x > dist/app
    outputs:
      bin: dist/
  publish:
    needs: [build]
` + env + `    release:
      github:
` + release + `        assets:
          - from: build.bin
            name: app
`
}

// TestExpandPublishOnlyOnTag：发布 job 只能由 tag push 触发（PLAN #13.4），其他事件中在规划时失败。
func TestExpandPublishOnlyOnTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       string
		wantState store.JobState
	}{
		{"分支 push", "refs/heads/main", store.JobFailed},
		{"tag push", "refs/tags/v1.0.0", store.JobWaiting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("rel.yml"): publishWorkflow("", "")})
			r := e.planOne(e.pushReq(tt.ref, sha))
			if r.ConfigError != nil {
				t.Fatalf("不应有配置错误：%s", *r.ConfigError)
			}
			j := e.jobs(r.ID, 0)["publish"]
			if j.Kind != workflow.JobKindPublish || j.State != tt.wantState {
				t.Fatalf("publish：kind=%s state=%s reason=%q，期望 %s", j.Kind, j.State, deref(j.Reason), tt.wantState)
			}
			if tt.wantState == store.JobFailed && !strings.Contains(deref(j.Reason), "发布只能由 tag push 触发") {
				t.Errorf("失败原因 = %q", deref(j.Reason))
			}
			if j.InputDigest != nil {
				t.Error("发布 job 不计算输入摘要")
			}
		})
	}
}

// TestExpandCrossRepoPublish：发布到其他仓库（PLAN #13.6）必须声明 environment、目标在允许列表中、
// App 安装在目标仓库上（与源仓库同一个 installation）、显式填写 notes.from。
func TestExpandCrossRepoPublish(t *testing.T) {
	t.Parallel()
	const notes = "        repository: tester/site\n        notes:\n          from: CHANGELOG.md\n"
	const noNotes = "        repository: tester/site\n"
	// site 的状态：不在 Kiln 中但在同一个 installation（私有）、没有安装 App（公开）、在 Kiln 中但属于别的 installation
	const (
		siteSameInst = iota
		siteNotInstalled
		siteOtherInst
	)
	tests := []struct {
		name        string
		environment string
		release     string
		publishTo   []string
		site        int
		wantErr     string
	}{
		{"没有声明 environment", "", notes, nil, siteSameInst, "必须声明 environment"},
		{"不在允许发布的仓库中", "public", notes, []string{"tester/other"}, siteSameInst, "不在 environment public 的允许发布仓库列表中"},
		{"目标没有安装 App", "public", notes, []string{"tester/site"}, siteNotInstalled, "App 没有安装在目标仓库 tester/site 上"},
		{"目标属于别的 installation", "public", notes, []string{"tester/site"}, siteOtherInst, "App 没有安装在目标仓库 tester/site 上"},
		{"缺少 notes.from", "public", noNotes, []string{"tester/site"}, siteSameInst, "notes.from"},
		{"全部满足", "public", notes, []string{"TESTER/Site"}, siteSameInst, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			switch tt.site {
			case siteSameInst:
				e.addSite(e.gh.InstallationID, false)
			case siteNotInstalled:
				e.addSite(-1, false)
			case siteOtherInst:
				e.gh.AddInstallation(778)
				e.addSite(778, true)
			}
			if tt.environment != "" {
				e.addEnvironment(tt.environment, envOpts{refPolicy: []string{"refs/tags/v*"}, publishTo: tt.publishTo})
			}
			sha := e.commit(map[string]string{wf("rel.yml"): publishWorkflow(tt.environment, tt.release), "CHANGELOG.md": "## v1\n"})
			r := e.planOne(e.pushReq("refs/tags/v1.0.0", sha))
			if tt.wantErr != "" {
				if r.State != store.RunFailed || !strings.Contains(deref(r.ConfigError), tt.wantErr) {
					t.Fatalf("应以配置错误失败（包含 %q）：state=%s config_error=%q", tt.wantErr, r.State, deref(r.ConfigError))
				}
				return
			}
			if r.State != store.RunActive || r.ConfigError != nil {
				t.Fatalf("应正常规划：state=%s config_error=%q", r.State, deref(r.ConfigError))
			}
			if j := e.jobs(r.ID, 0)["publish"]; j.State != store.JobWaiting {
				t.Fatalf("publish 应等待 build：%s %q", j.State, deref(j.Reason))
			}
			// 目标仓库经定向确认后记录到 Kiln（同一个 installation）
			site, err := store.FindRepo(e.ctx, e.db, "tester/site")
			if err != nil || site.InstallationID != e.gh.InstallationID {
				t.Fatalf("目标仓库应已记录到源仓库的 installation 下：%+v %v", site, err)
			}
		})
	}
}

// addSite 登记跨仓库发布的目标仓库 tester/site（ID 200）：inst 为 -1 表示没有安装 App；inKiln 为 true 时同时记录到 Kiln。
func (e *testEnv) addSite(inst int64, inKiln bool) {
	e.t.Helper()
	if inKiln {
		e.addRepo(200, "site", inst, false)
		return
	}
	fr, _ := e.addRepo(200, "site", -1, false)
	e.gh.MoveRepo(fr, inst)
}

// runtimeWorkflow：tool 使用 runtime go。
const runtimeWorkflow = `name: rt
on:
  push:
    branches: [main]
jobs:
  tool:
    runs-on: linux/arm64
    runtimes: [go]
    steps:
      - run: go version
`

// TestExpandRuntimeLock：job 声明的 runtime 按 .kiln/runtimes.lock 解析（PLAN #4.8）；
// 没有 lockfile、lockfile 无效、缺少 runtime 或平台都是配置错误，run 直接失败。
func TestExpandRuntimeLock(t *testing.T) {
	t.Parallel()
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct {
		name     string
		lock     string // 空表示没有 lockfile
		inReason string // 空表示应成功解析
	}{
		{"没有 lockfile", "", "仓库中没有 .kiln/runtimes.lock"},
		{"lockfile 无效", "[go\nversion = ", "解析 .kiln/runtimes.lock"},
		{"digest 格式不对", "[go]\nversion = \"1.23.0\"\n\"linux/arm64\" = \"md5:x\"\n", "应为 sha256"},
		{"没有这个 runtime", "[node]\nversion = \"22.0.0\"\n\"linux/arm64\" = \"" + digest + "\"\n", "runtime go 不在"},
		{"没有这个平台", "[go]\nversion = \"1.23.0\"\n\"darwin/arm64\" = \"" + digest + "\"\n", "没有平台 linux/arm64 的条目"},
		{"解析成功", "[go]\nversion = \"1.23.0\"\n\"linux/arm64\" = \"" + digest + "\"\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			files := map[string]string{wf("rt.yml"): runtimeWorkflow}
			if tt.lock != "" {
				files[".kiln/runtimes.lock"] = tt.lock
			}
			sha := e.commit(files)
			r := e.planOne(e.pushReq("refs/heads/main", sha))
			jobs := e.jobs(r.ID, 0)
			if tt.inReason != "" {
				if r.State != store.RunFailed || !strings.Contains(deref(r.ConfigError), tt.inReason) || len(jobs) != 0 {
					t.Fatalf("run 应以配置错误失败（包含 %q）：state=%s config_error=%q，%d 个 job", tt.inReason, r.State, deref(r.ConfigError), len(jobs))
				}
				return
			}
			j := jobs["tool"]
			want := []workflow.RuntimeRef{{Name: "go", Version: "1.23.0", Platform: "linux/arm64", Digest: digest}}
			if j.State != store.JobPending || !slices.Equal(j.Spec.RuntimeRefs, want) {
				t.Fatalf("tool：state=%s reason=%q runtime_refs=%+v，期望 %+v", j.State, deref(j.Reason), j.Spec.RuntimeRefs, want)
			}
		})
	}
}

// inputsWorkflow：api、web 只依赖各自的目录，all 使用默认 inputs（整个仓库，不含 .kiln/workflows），
// ghost 的 inputs 匹配不到任何文件。
const inputsWorkflow = `name: inputs
on:
  push:
    branches: [main]
jobs:
  api:
    runs-on: linux/arm64
    inputs: ["api/"]
    steps:
      - run: echo api
  web:
    runs-on: linux/arm64
    inputs: ["web/**", "!web/**/*.md"]
    steps:
      - run: echo web
  all:
    runs-on: linux/arm64
    steps:
      - run: echo all
  ghost:
    runs-on: linux/arm64
    inputs: ["missing/"]
    steps:
      - run: echo ghost
`

// TestExpandInputDigest：输入摘要只取决于 inputs 匹配到的文件（PLAN #7.4 第 7 步）；匹配为空的模式给出警告。
func TestExpandInputDigest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	digests := func(sha string) map[string]string {
		t.Helper()
		r := e.planOne(e.pushReq("refs/heads/main", sha))
		out := map[string]string{}
		for k, j := range e.jobs(r.ID, 0) {
			out[k] = deref(j.InputDigest)
		}
		if !strings.Contains(deref(r.Warnings), `"missing/**" 没有匹配任何文件`) {
			t.Errorf("应警告 ghost 的 inputs 匹配为空：%q", deref(r.Warnings))
		}
		return out
	}
	base := digests(e.commit(map[string]string{wf("inputs.yml"): inputsWorkflow, "api/a.go": "a", "web/w.ts": "w", "web/doc.md": "d"}))

	tests := []struct {
		name    string
		files   map[string]string
		changed []string // 输入摘要应改变的 job
	}{
		{"改 api", map[string]string{"api/a.go": "a2"}, []string{"api", "all"}},
		{"改 web 的文档（被排除）", map[string]string{"web/doc.md": "d2"}, []string{"all"}},
		{"改 web 的代码", map[string]string{"web/w.ts": "w2"}, []string{"web", "all"}},
		{"只改 workflow 文件", map[string]string{wf("inputs.yml"): inputsWorkflow + "# 注释\n"}, nil},
	}
	// 共用一个仓库，按顺序提交，每次与上一次比较
	prev := base
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := digests(e.commit(tt.files))
			for _, k := range []string{"api", "web", "all", "ghost"} {
				changed := cur[k] != prev[k]
				if changed != slices.Contains(tt.changed, k) {
					t.Errorf("job %s 的输入摘要是否改变 = %v，期望 %v", k, changed, !changed)
				}
				if cur[k] == "" {
					t.Errorf("job %s 应有输入摘要", k)
				}
			}
			prev = cur
		})
	}
}

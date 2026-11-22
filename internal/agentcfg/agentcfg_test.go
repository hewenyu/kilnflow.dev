package agentcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimal = "version: 1\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n"

func TestParse_defaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.ReviewAgents(), []string{AgentCodex}) || c.DevelopmentEnabled() || !c.ReviewEnabled() || !c.PullRequestTrigger() || !c.ManualTrigger() || !c.PublishReviewComments() {
		t.Fatalf("默认值不对：%+v", c)
	}
	if c.ReviewTimeout() != DefaultTimeout || c.InstructionsPath() != DefaultReviewInstructions || len(c.Services()) != 0 {
		t.Fatalf("timeout %s，instructions %s，services %v", c.ReviewTimeout(), c.InstructionsPath(), c.Services())
	}
	p := c.Review.RequiredPlatforms[0]
	if p.ID != "macos-arm64" || p.RunnerPlatform() != "darwin/arm64" {
		t.Fatalf("平台 = %+v（%s）", p, p.RunnerPlatform())
	}
}

func TestParseSingleDocument(t *testing.T) {
	for _, suffix := range []string{"---\nenabled: false\n", "---\n", "---\ninvalid: [\n"} {
		t.Run(suffix, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + suffix)); err == nil {
				t.Fatal("尾随文档或语法错误应被拒绝")
			}
		})
	}
	for _, suffix := range []string{"\n# trailing comment\n", "...\n"} {
		if _, err := Parse([]byte(minimal + suffix)); err != nil {
			t.Fatalf("应允许尾随注释和文档结束标记：%v", err)
		}
	}
}

func TestParse_full(t *testing.T) {
	c, err := Parse([]byte(`version: 1
agent: codex
requirements:
  services: [redis, pg, redis]
review:
  instructions: ./docs/review.md
  max_concurrency: 2
  timeout: 1h
  required_platforms:
    - {id: mac, os: Darwin, arch: aarch64}
  triggers: {pull_request: false, manual: true}
  publish: {review_comments: false}
development:
  agent: claude-code
  instructions: ./docs/dev.md
  timeout: 12h
  max_fix_rounds: 5
  max_issues: 1
  dry_run: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.PullRequestTrigger() || !c.ManualTrigger() || c.PublishReviewComments() || c.ReviewTimeout() != time.Hour {
		t.Fatalf("配置没有生效：%+v", c.Review)
	}
	if got := strings.Join(c.Services(), ","); got != "pg,redis" {
		t.Errorf("services = %s", got)
	}
	if c.InstructionsPath() != "docs/review.md" {
		t.Errorf("instructions = %s", c.InstructionsPath())
	}
	if p := c.Review.RequiredPlatforms[0]; p.ID != "mac" || p.OS != OSMacOS || p.Arch != ArchARM64 {
		t.Errorf("平台没有归一：%+v", p)
	}
	// 顶层 agent 是一期写法，只表示审计的 Agent；开发的 Agent 单独选
	if !slices.Equal(c.ReviewAgents(), []string{AgentCodex}) || !c.DevelopmentEnabled() || c.Development.Agent != AgentClaudeCode {
		t.Errorf("Agent：审计 %q，开发 %v", c.Review.Agent, c.Development)
	}
	if c.DevTimeout() != 12*time.Hour || c.MaxFixRounds() != 5 || c.MaxIssues() != 1 || !c.Development.DryRun || c.DevInstructionsPath() != "docs/dev.md" {
		t.Errorf("开发配置没有生效：%+v", c.Development)
	}
}

// TestParse_development：开发与审计分别启用、各选一个 Agent（PLAN #26.7）。
func TestParse_development(t *testing.T) {
	// 不写、写空或 null：不开发
	for _, dev := range []string{"", "development:\n", "development: null\n"} {
		c, err := Parse([]byte(minimal + dev))
		if err != nil || c.DevelopmentEnabled() {
			t.Errorf("%q：开发 = %v，%v", dev, c != nil && c.DevelopmentEnabled(), err)
		}
	}
	c, err := Parse([]byte(minimal + "development: {agent: codex}\n"))
	if err != nil || !c.DevelopmentEnabled() || c.DevTimeout() != DefaultDevTimeout || c.MaxFixRounds() != DefaultMaxFixRounds ||
		c.MaxIssues() != DefaultMaxIssues || c.DevInstructionsPath() != DefaultDevelopmentInstructions {
		t.Fatalf("默认值：%+v，%v", c, err)
	}
	// 关闭开发时不要求 agent；顶层关闭时开发也关闭
	for _, y := range []string{minimal + "development: {enabled: false}\n", "version: 1\nenabled: false\ndevelopment: {agent: codex}\n"} {
		c, err := Parse([]byte(y))
		if err != nil || c.DevelopmentEnabled() {
			t.Errorf("%q：开发 = %v，%v", y, c != nil && c.DevelopmentEnabled(), err)
		}
	}
	// 只开发、不审计（review.enabled: false）时不要求审计平台
	if c, err := Parse([]byte("version: 1\nreview: {enabled: false, agent: codex}\ndevelopment: {agent: claude-code}\n")); err != nil || c.ReviewEnabled() || !c.DevelopmentEnabled() {
		t.Errorf("只开发：%+v，%v", c, err)
	}
	if c, err := Parse([]byte(strings.Replace(minimal, "review:\n", "review:\n  agent: claude-code\n", 1))); err != nil ||
		!slices.Equal(c.ReviewAgents(), []string{AgentClaudeCode}) {
		t.Errorf("review.agent：%+v，%v", c, err)
	}
}

// requirements.tools（PLAN #33.12）：按声明的顺序去重；lockfile 需要的平台是启用的审计平台和开发平台。
func TestParseTools(t *testing.T) {
	c, err := Parse([]byte(minimal + "requirements: {tools: [pnpm, go, pnpm]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Tools(), []string{"pnpm", "go"}) {
		t.Errorf("Tools = %v", c.Tools())
	}
	want := map[string][]string{"pnpm": {"darwin/arm64"}, "go": {"darwin/arm64"}}
	if got := c.ToolPlatforms(); !reflect.DeepEqual(got, want) {
		t.Errorf("ToolPlatforms = %v", got)
	}
	// 没有声明工具、或者审计和开发都关闭时不需要平台
	if c, err := Parse([]byte(minimal)); err != nil || c.ToolPlatforms() != nil || c.Tools() != nil {
		t.Errorf("没有工具：%v, %v", c.ToolPlatforms(), err)
	}
	if c, err := Parse([]byte("version: 1\nenabled: false\nrequirements: {tools: [go]}\n")); err != nil || c.ToolPlatforms() != nil {
		t.Errorf("关闭时：%v, %v", c.ToolPlatforms(), err)
	}
	// 只开发：开发平台
	c, err = Parse([]byte("version: 1\nreview: {enabled: false}\nrequirements: {tools: [node]}\ndevelopment: {agent: codex}\n"))
	if err != nil || !reflect.DeepEqual(c.ToolPlatforms(), map[string][]string{"node": {"darwin/arm64"}}) {
		t.Errorf("只开发：%v, %v", c.ToolPlatforms(), err)
	}
}

// requirements.repositories（PLAN #33.13）：owner/name，按声明的顺序去重（不区分大小写，与 GitHub 相同）。
func TestParseRepositories(t *testing.T) {
	c, err := Parse([]byte(minimal + "requirements: {repositories: [acme/lib, Acme/Lib, acme/proto.v2, a-b/c_d]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/lib", "acme/proto.v2", "a-b/c_d"}; !slices.Equal(c.Repositories(), want) {
		t.Errorf("Repositories = %v，期望 %v", c.Repositories(), want)
	}
	if c, err := Parse([]byte(minimal)); err != nil || c.Repositories() != nil {
		t.Errorf("没有声明：%v, %v", c.Repositories(), err)
	}
	for _, bad := range []string{"lib", "acme/lib/x", "https://github.com/acme/lib", "acme/lib.git", "-acme/lib", "acme/..", "acme/", "/lib", "acme/lib,acme/x"} {
		if _, err := Parse([]byte(minimal + "requirements: {repositories: [\"" + bad + "\"]}\n")); err == nil ||
			!strings.Contains(err.Error(), "requirements.repositories 中的") {
			t.Errorf("%q：err = %v", bad, err)
		}
	}
}

// TestParseReviewAgents：review.agent 可以写一个列表（PLAN #33.7）：按写的顺序去重，每个都校验。
func TestParseReviewAgents(t *testing.T) {
	t.Parallel()
	list := strings.Replace(minimal, "review:\n", "review:\n  agent: [claude-code, codex, claude-code]\n", 1)
	c, err := Parse([]byte(list))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{AgentClaudeCode, AgentCodex}; !slices.Equal(c.ReviewAgents(), want) {
		t.Errorf("review.agent = %v，期望 %v", c.ReviewAgents(), want)
	}
	single := strings.Replace(minimal, "review:\n", "review:\n  agent: [claude-code]\n", 1)
	if c, err := Parse([]byte(single)); err != nil || !slices.Equal(c.ReviewAgents(), []string{AgentClaudeCode}) {
		t.Errorf("单个元素的列表：%v，%v", c, err)
	}
	bad := strings.Replace(minimal, "review:\n", "review:\n  agent: [codex, opencode]\n", 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Error("列表中有不支持的 Agent 时应报错")
	}
	top := strings.Replace(minimal, "review:\n", "review:\n  agent: [codex]\nagent: codex\n", 1)
	if _, err := Parse([]byte(top)); err == nil {
		t.Error("顶层 agent 与 review.agent 同时写时应报错")
	}
}

func TestParse_disabled(t *testing.T) {
	// 关闭审计时不要求平台
	c, err := Parse([]byte("version: 1\nenabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewEnabled() || c.PullRequestTrigger() || c.ManualTrigger() {
		t.Fatal("关闭后不应触发")
	}
}

func TestParse_errors(t *testing.T) {
	plat := "  required_platforms:\n    - {os: macos, arch: arm64}\n"
	cases := []struct{ name, yaml, want string }{
		{"空文件", "", "空文件"},
		{"版本", "version: 2\n", "version 必须是 1"},
		{"未知字段", "version: 1\nfoo: 1\n", "foo"},
		{"claude", "version: 1\nagent: claude\n", `agent：不支持的 agent "claude"（只支持 codex、claude-code）`},
		{"未知 agent", "version: 1\nagent: x\n", "不支持的 agent"},
		{"opencode 审计", minimal + "  agent: opencode\n", `review.agent：不支持的 agent "opencode"`},
		{"opencode 开发", minimal + "development: {agent: opencode}\n", `development.agent：不支持的 agent "opencode"`},
		{"大小写", minimal + "development: {agent: Codex}\n", "不支持的 agent"},
		{"新旧写法同时", "version: 1\nagent: codex\n" + minimal[len("version: 1\n"):] + "  agent: codex\n", "不能同时写"},
		{"开发缺 agent", minimal + "development: {}\n", "必须填写 development.agent"},
		{"开发超时太长", minimal + "development: {agent: codex, timeout: 13h}\n", "10m0s 到 12h0m0s 之间"},
		{"开发超时太短", minimal + "development: {agent: codex, timeout: 5m}\n", "之间"},
		{"开发超时无效", minimal + "development: {agent: codex, timeout: soon}\n", "development.timeout"},
		{"返修轮数", minimal + "development: {agent: codex, max_fix_rounds: 11}\n", "development.max_fix_rounds 必须在 1 到 10 之间"},
		{"领取数为负", minimal + "development: {agent: codex, max_issues: -1}\n", "development.max_issues"},
		{"开发说明越界", minimal + "development: {agent: codex, instructions: ../x.md}\n", "development.instructions"},
		{"开发未知字段", minimal + "development: {agent: codex, x: 1}\n", "x"},
		{"服务", "version: 1\nrequirements: {services: [mysql]}\n", "不支持的服务"},
		{"工具链", minimal + "requirements: {tools: [go, python]}\n", `requirements.tools 中的 "python" 不是内置 runtime（只支持 bun、go、node、pnpm）`},
		{"缺平台", "version: 1\n", "required_platforms"},
		{"windows", "version: 1\nreview:\n  required_platforms:\n    - {os: windows, arch: amd64}\n", "平台 windows/amd64 暂不支持（支持 macos/arm64、linux/amd64、linux/arm64）"},
		{"linux riscv64", "version: 1\nreview:\n  required_platforms:\n    - {os: linux, arch: riscv64}\n", "平台 linux/riscv64 暂不支持"},
		{"macos amd64", "version: 1\nreview:\n  required_platforms:\n    - {os: macos, arch: x86_64}\n", "平台 macos/amd64 暂不支持"},
		{"组为空", "version: 1\nreview:\n  required_platforms:\n    - {id: any, any_of: []}\n", "required_platforms[0] 的 any_of 不能为空"},
		{"组没有 id", "version: 1\nreview:\n  required_platforms:\n    - any_of: [{os: macos, arch: arm64}]\n", "必须写 id"},
		{"组与平台混写", "version: 1\nreview:\n  required_platforms:\n    - {id: any, os: macos, arch: arm64, any_of: [{os: linux, arch: amd64}]}\n", "any_of 与 os、arch 不能同时写"},
		{"组与 arch 混写", "version: 1\nreview:\n  required_platforms:\n    - {id: any, arch: arm64, any_of: [{os: linux, arch: amd64}]}\n", "不能同时写"},
		{"组内重复", "version: 1\nreview:\n  required_platforms:\n    - {id: any, any_of: [{os: linux, arch: amd64}, {os: Linux, arch: x86_64}]}\n", "any_of 中的平台 linux/amd64 重复"},
		{"组内不支持", "version: 1\nreview:\n  required_platforms:\n    - {id: any, any_of: [{os: linux, arch: amd64}, {os: windows, arch: amd64}]}\n", "required_platforms[0].any_of[1]：平台 windows/amd64 暂不支持"},
		{"组内缺 arch", "version: 1\nreview:\n  required_platforms:\n    - {id: any, any_of: [{os: linux}]}\n", "any_of[0] 必须同时填写 os 和 arch"},
		{"组内未知字段", "version: 1\nreview:\n  required_platforms:\n    - {id: any, any_of: [{id: x, os: linux, arch: amd64}]}\n", "id"},
		{"组 id 无效", "version: 1\nreview:\n  required_platforms:\n    - {id: \"a b\", any_of: [{os: linux, arch: amd64}]}\n", "id \"a b\" 无效"},
		{"组与单个平台 id 重复", "version: 1\nreview:\n  required_platforms:\n    - {os: linux, arch: amd64}\n    - {id: linux-amd64, any_of: [{os: linux, arch: arm64}]}\n", "id \"linux-amd64\" 重复"},
		{"缺 arch", "version: 1\nreview:\n  required_platforms:\n    - {os: macos}\n", "同时填写 os 和 arch"},
		{"重复 id", "version: 1\nreview:\n  required_platforms:\n    - {os: macos, arch: arm64}\n    - {os: macos, arch: arm64}\n", "重复"},
		{"超时太短", "version: 1\nreview:\n  timeout: 1m\n" + plat, "之间"},
		{"超时无效", "version: 1\nreview:\n  timeout: soon\n" + plat, "无效"},
		{"并发为负", "version: 1\nreview:\n  max_concurrency: -1\n" + plat, "负数"},
		{"周期审计", "version: 1\nreview:\n  triggers: {schedule: daily}\n" + plat, "schedule"},
		{"越界说明", "version: 1\nreview:\n  instructions: ../x.md\n" + plat, "相对路径"},
		{"绝对说明", "version: 1\nreview:\n  instructions: /etc/x\n" + plat, "相对路径"},
		{"平台 id", "version: 1\nreview:\n  required_platforms:\n    - {id: \"mac arm\", os: macos, arch: arm64}\n", "id \"mac arm\" 无效"},
		{"平台 id 太长", "version: 1\nreview:\n  required_platforms:\n    - {id: " + strings.Repeat("a", 65) + ", os: macos, arch: arm64}\n", "无效"},
		{"开发平台不支持", minimal + "development: {agent: codex, platform: {os: windows, arch: amd64}}\n", "development.platform：平台 windows/amd64 暂不支持（支持 macos/arm64、linux/amd64、linux/arm64）"},
		{"开发平台缺 arch", minimal + "development: {agent: codex, platform: {os: linux}}\n", "development.platform 必须同时填写 os 和 arch"},
		{"开发平台为空对象", minimal + "development: {agent: codex, platform: {}}\n", "development.platform 必须同时填写 os 和 arch"},
		{"开发组为空", minimal + "development: {agent: codex, platform: {any_of: []}}\n", "development.platform 的 any_of 不能为空"},
		{"开发组内重复", minimal + "development: {agent: codex, platform: {any_of: [{os: linux, arch: amd64}, {os: linux, arch: x64}]}}\n", "development.platform.any_of 中的平台 linux/amd64 重复"},
		{"开发组与平台混写", minimal + "development: {agent: codex, platform: {os: macos, arch: arm64, any_of: [{os: linux, arch: amd64}]}}\n", "development.platform：any_of 与 os、arch 不能同时写"},
		{"开发组内不支持", minimal + "development: {agent: codex, platform: {any_of: [{os: macos, arch: arm64}, {os: linux, arch: riscv64}]}}\n", "development.platform.any_of[1]：平台 linux/riscv64 暂不支持"},
		{"开发平台不能写 id", minimal + "development: {agent: codex, platform: {id: any, any_of: [{os: linux, arch: amd64}]}}\n", "id"},
		{"development 不是对象", minimal + "development: 5\n", "agentcfg.Development"},
		{"development 是列表", minimal + "development: [a]\n", "agentcfg.Development"},
		{"太大", "version: 1\n#" + strings.Repeat("x", MaxConfigSize), "KiB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
		})
	}
}

// TestParsePlatforms：审计支持 macos/arm64、linux/amd64、linux/arm64，每一项审计一次：单个平台，或者一组可以互相
// 替代的平台（any_of，组内任一平台的节点领取，只审计一次）。
func TestParsePlatforms(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte(`version: 1
requirements: {tools: [go]}
review:
  required_platforms:
    - {os: macos, arch: arm64}
    - {os: Linux, arch: x86_64}
    - {os: linux, arch: aarch64}
    - id: any
      any_of:
        - {os: macos, arch: arm64}
        - {os: linux, arch: amd64}
`))
	if err != nil {
		t.Fatal(err)
	}
	ps := c.Review.RequiredPlatforms
	got := make([]string, 0, len(ps))
	for _, p := range ps {
		got = append(got, p.ID+"="+strings.Join(p.RunnerPlatforms(), "|"))
	}
	want := []string{"macos-arm64=darwin/arm64", "linux-amd64=linux/amd64", "linux-arm64=linux/arm64", "any=darwin/arm64|linux/amd64"}
	if !slices.Equal(got, want) {
		t.Fatalf("平台 = %v，期望 %v", got, want)
	}
	if g := ps[3].Targets(); !slices.Equal(g, []Target{{OSMacOS, ArchARM64}, {OSLinux, ArchAMD64}}) {
		t.Errorf("组的 Targets = %v", g)
	}
	// lockfile 需要的平台：所有项（含组内）的平台，去重
	if got := c.ToolPlatforms()["go"]; !slices.Equal(got, []string{"darwin/arm64", "linux/amd64", "linux/arm64"}) {
		t.Errorf("ToolPlatforms = %v", got)
	}
	// 支持的平台只定义一次：每一个都能单独写
	for _, tg := range Targets() {
		y := fmt.Sprintf("version: 1\nreview:\n  required_platforms:\n    - {os: %s, arch: %s}\n", tg.OS, tg.Arch)
		if _, err := Parse([]byte(y)); err != nil {
			t.Errorf("%s：%v", tg, err)
		}
	}
}

// TestParseDevPlatform：开发的平台可以是单个平台或一组平台（any_of，规则与审计相同），不写时是 macOS arm64（PLAN #26.7）；
// lockfile 需要的平台包含开发的平台。
func TestParseDevPlatform(t *testing.T) {
	t.Parallel()
	const head = "version: 1\nreview: {enabled: false}\nrequirements: {tools: [go]}\n"
	for _, c := range []struct {
		name, yaml string
		want       []string
	}{
		{"不写", "development: {agent: codex}\n", []string{"darwin/arm64"}},
		{"单个平台", "development: {agent: codex, platform: {os: Linux, arch: x86_64}}\n", []string{"linux/amd64"}},
		{
			"一组平台", "development:\n  agent: codex\n  platform:\n    any_of: [{os: linux, arch: amd64}, {os: macos, arch: arm64}]\n",
			[]string{"linux/amd64", "darwin/arm64"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse([]byte(head + c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.DevPlatforms(); !slices.Equal(got, c.want) {
				t.Errorf("DevPlatforms = %v，应为 %v", got, c.want)
			}
			want := slices.Sorted(slices.Values(c.want))
			if got := cfg.ToolPlatforms()["go"]; !slices.Equal(got, want) {
				t.Errorf("ToolPlatforms = %v，应为 %v", got, want)
			}
		})
	}
	// 没有 development 段时也有默认值（调用方不必判断）
	if cfg, err := Parse([]byte("version: 1\nreview: {enabled: false}\n")); err != nil || !slices.Equal(cfg.DevPlatforms(), []string{"darwin/arm64"}) {
		t.Errorf("没有 development：%v, %v", cfg, err)
	}
	// 支持的平台与审计是同一份：每一个都能写成开发的平台
	for _, tg := range Targets() {
		y := fmt.Sprintf("%sdevelopment: {agent: codex, platform: {os: %s, arch: %s}}\n", head, tg.OS, tg.Arch)
		if cfg, err := Parse([]byte(y)); err != nil || !slices.Equal(cfg.DevPlatforms(), []string{tg.RunnerPlatform()}) {
			t.Errorf("%s：%v", tg, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"Darwin": "macos", "osx": "macos", "Linux": "linux"} {
		if got := NormalizeOS(in); got != want {
			t.Errorf("NormalizeOS(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"x86_64": "amd64", "X64": "amd64", "aarch64": "arm64", "arm": "arm"} {
		if got := NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q", in, got)
		}
	}
}

// TestRepoConfig 校验 Kiln 仓库自己的 AI 审计配置，避免改坏后 PR 上的审计 run 才报配置错误。
func TestRepoConfig(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, ConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !c.PullRequestTrigger() {
		t.Error("Kiln 仓库应在 PR 上启用 AI 审计")
	}
	ins, err := os.ReadFile(filepath.Join(root, c.InstructionsPath()))
	if err != nil {
		t.Fatalf("审计说明：%v", err)
	}
	if len(ins) > MaxInstructionsSize {
		t.Fatalf("审计说明 %d 字节，超过 %d KiB", len(ins), MaxInstructionsSize>>10)
	}
}

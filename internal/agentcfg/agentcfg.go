// Package agentcfg 解析仓库的 AI 执行节点配置 .kiln/agent/config.yml。
//
// 仓库配置只描述"这个项目需要什么"：开发和审计各用哪个 Agent、平台、标准测试服务、触发方式和超时；服务的镜像、地址、
// 凭据和清理方式由 AI 执行节点所在机器的配置提供，不进入仓库。审计（review，PLAN #26.1）和开发（development，#26.7）
// 分别启用、各选一个 Agent；审计和开发的平台都从 targets（支持的平台）中选，服务只有 pg、redis。
package agentcfg

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 仓库中的路径。
const (
	// Dir 是 AI 执行节点配置所在的目录。
	Dir = ".kiln/agent"
	// ConfigPath 是结构化配置文件。
	ConfigPath = Dir + "/config.yml"
	// DefaultReviewInstructions 是审计说明的默认路径（可选）。
	DefaultReviewInstructions = Dir + "/review.md"
	// DefaultDevelopmentInstructions 是开发说明的默认路径（可选，PLAN #26.7）。
	DefaultDevelopmentInstructions = Dir + "/development.md"
)

// Agent 是开发或审计使用的 CLI（本机默认登录，不做账号池）。只有这两个合法值，其他值（包括 opencode、claude）
// 在所有配置入口都报错，不忽略、不映射到某个默认值（PLAN #26.7）。
const (
	AgentCodex      = "codex"
	AgentClaudeCode = "claude-code"
)

// AI 执行节点上报的 Agent 状态（AgentStatus.state，PLAN #30.7）。
const (
	AgentAvailable   = "available"
	AgentUnavailable = "unavailable"
)

// ValidAgent 判断 a 是否是 Catalog 中的 Agent。
func ValidAgent(a string) bool { _, ok := LookupAgent(a); return ok }

// agentError 返回不支持的 Agent 的错误，指出字段和支持的值。
func agentError(field, a string) error {
	return fmt.Errorf("%s：不支持的 agent %q（只支持 %s）", field, a, strings.Join(Agents(), "、"))
}

// AgentList 是 Agent 列表：YAML 中可以写一个字符串或一个列表（PLAN #33.7）。
type AgentList []string

// UnmarshalYAML 接受 "codex" 或 ["codex", "claude-code"]（也接受单个元素的列表）。
func (l *AgentList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*l = AgentList{s}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return err
		}
		*l = list
		return nil
	}
	return errors.New("应为 Agent 名字或名字的列表")
}

// 标准测试服务（由 AI 执行节点所在机器提供）。
const (
	ServicePostgres = "pg"
	ServiceRedis    = "redis"
)

// supportedServices 是一期支持的标准测试服务，由配置解析器管理。
var supportedServices = []string{ServicePostgres, ServiceRedis}

// 平台：os 和 arch 是独立的调度字段，按明确的组合列出（不做笛卡尔积）。
const (
	OSMacOS   = "macos"
	OSLinux   = "linux"
	ArchARM64 = "arm64"
	ArchAMD64 = "amd64"
)

// targets 是 AI 任务支持的平台（review.required_platforms、development.platform 中可以写的 os/arch 组合，PLAN #26.1、
// #26.7）。审计和开发共用，校验、错误信息都从它来。
var targets = []Target{{OS: OSMacOS, Arch: ArchARM64}, {OS: OSLinux, Arch: ArchAMD64}, {OS: OSLinux, Arch: ArchARM64}}

// defaultDevTarget 是没有写 development.platform 时开发的平台。
var defaultDevTarget = Target{OS: OSMacOS, Arch: ArchARM64}

// Targets 返回 AI 任务支持的平台。
func Targets() []Target { return slices.Clone(targets) }

// targetsText 是支持的平台的说明（错误信息用）：macos/arm64、linux/amd64、linux/arm64。
func targetsText() string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.String())
	}
	return strings.Join(names, "、")
}

// platformIDRE 是平台 id 的格式：id 是 job key（review-<id>）和显示名的一部分。
var platformIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Target 是一个具体的平台：系统和架构（归一后的写法，例如 macos/arm64）。
type Target struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

// String 返回配置中的写法 <os>/<arch>（例如 macos/arm64）。
func (t Target) String() string { return t.OS + "/" + t.Arch }

// RunnerPlatform 返回平台对应的 Runner 平台字符串（与节点上报的 AIRunnerState.platform 的格式一致，Go 的写法，例如
// darwin/arm64、linux/amd64）。
func (t Target) RunnerPlatform() string {
	goos := t.OS
	if goos == OSMacOS {
		goos = "darwin"
	}
	return goos + "/" + t.Arch
}

// TargetSet 是一个任务可以执行的平台：单个平台（os、arch），或者一组可以互相替代的平台（any_of）：组内任一平台的节点
// 都可以领取，哪台先领到就在哪台执行，只执行一次（PLAN #26.1、#26.7）。审计的每一项和开发的平台都是它。
type TargetSet struct {
	// Target 是单个平台；写了 any_of 时为空。
	Target `yaml:",inline"`
	// AnyOf 是一组可以互相替代的平台（不为 nil 表示写了 any_of）。
	AnyOf []Target `yaml:"any_of"`
}

// Targets 返回可以执行的平台：单个平台时是它自己，一组时是组内的平台（写的顺序）。
func (s TargetSet) Targets() []Target {
	if s.AnyOf != nil {
		return slices.Clone(s.AnyOf)
	}
	return []Target{s.Target}
}

// RunnerPlatforms 返回可以执行的平台的 Runner 平台字符串（写的顺序，例如 [darwin/arm64 linux/amd64]）。
func (s TargetSet) RunnerPlatforms() []string {
	ts := s.Targets()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.RunnerPlatform())
	}
	return out
}

// Platform 是 review.required_platforms 的一项：每一项审计一次（一个任务）。一项可以是单个平台，也可以是一组平台
// （TargetSet）；一组时必须写 id（任务的名称是 review-<id>）。
type Platform struct {
	ID        string `yaml:"id"`
	TargetSet `yaml:",inline"`
	Labels    []string `yaml:"labels"`
}

// Triggers 是审计的触发方式。
type Triggers struct {
	// PullRequest：PR 打开、推送、重新打开、转为 ready 时审计（默认开启）。
	PullRequest *bool `yaml:"pull_request"`
	// Manual：允许用 CLI / Dashboard 手动触发（默认开启）。
	Manual *bool `yaml:"manual"`
	// Schedule 为后续的周期审计保留，一期设置了会报错。
	Schedule yaml.Node `yaml:"schedule"`
}

// Publish 是审计结果发布到 GitHub 的方式。Check Run 总是显示（审计结论的门禁投影）。
type Publish struct {
	// ReviewComments：PR 审计的结果以 PR review 评论发布（默认开启）。
	ReviewComments *bool `yaml:"review_comments"`
}

// Review 是审计的配置。
type Review struct {
	Enabled *bool `yaml:"enabled"`
	// Agent 是审计可以使用的 Agent（按优先级，PLAN #33.7）：写一个或一个列表。审计这个工作项的 PR 时选第一个
	// 没有开发过它的 Agent（dev_agents）；不写时沿用顶层的 agent（一期写法），都不写为 codex。解析后总是填好、去重。
	Agent             AgentList  `yaml:"agent"`
	Instructions      string     `yaml:"instructions"`
	MaxConcurrency    int        `yaml:"max_concurrency"`
	RequiredPlatforms []Platform `yaml:"required_platforms"`
	Triggers          Triggers   `yaml:"triggers"`
	Timeout           string     `yaml:"timeout"`
	Publish           Publish    `yaml:"publish"`
}

// Requirements 是开发与审计共用的需求。
type Requirements struct {
	Services []string `yaml:"services"`
	// Tools 是任务使用的受管理的工具链（PLAN #33.12）：内置 runtime 的名称（go、node、pnpm、bun），版本和摘要来自任务
	// commit 上的 .kiln/runtimes.lock，由 AI 执行节点安装在自己的目录中；Agent 经 kiln-orca task exec 使用，不用本机的。
	// 顺序就是 PATH 中的先后。
	Tools []string `yaml:"tools"`
	// Repositories 是任务的构建命令要读取的同账号私有仓库（owner/name，例如 Go module 依赖，PLAN #33.13）：写法与 CI
	// workflow 的 repositories 相同。必须与任务仓库在同一个 installation 下（签发读凭据时校验）。审计从 PR 的 base 读取
	// （PR 不能给自己加读权限），开发从默认分支读取。
	Repositories []string `yaml:"repositories"`
	Labels       []string `yaml:"labels"`
}

// Development 是 AI 开发的配置（PLAN #26.7）。只从默认分支读取，PR 改不了它。
type Development struct {
	// Enabled 为 false 时关闭；写了 development 段即启用。
	Enabled *bool `yaml:"enabled"`
	// Agent 是开发（选题、开发、返修）使用的 Agent，启用时必填。
	Agent        string `yaml:"agent"`
	Instructions string `yaml:"instructions"`
	// Platform 是开发（选题、实现、返修）可以执行的平台：单个平台或一组平台（any_of），不写时是 macos/arm64（PLAN #26.7）。
	// 每次执行由平台在其中的、有空闲槽位的开发机领取。
	Platform *TargetSet `yaml:"platform"`
	// Timeout 是单次执行（选题并开发，或者一轮返修）的超时。
	Timeout      string `yaml:"timeout"`
	MaxFixRounds int    `yaml:"max_fix_rounds"`
	MaxIssues    int    `yaml:"max_issues"`
	// DryRun 为 true 时只选题、写计划，不领取、不写代码（试运行）。
	DryRun bool `yaml:"dry_run"`
}

// Config 是 .kiln/agent/config.yml。
type Config struct {
	Version int   `yaml:"version"`
	Enabled *bool `yaml:"enabled"`
	// Agent 是一期的写法，只表示审计的 Agent（与 review.agent 同时写时报错）；不会启用开发。
	Agent        string       `yaml:"agent"`
	Requirements Requirements `yaml:"requirements"`
	Review       Review       `yaml:"review"`
	// Development 为 nil 表示没有写 development 段（不开发）。
	Development *Development `yaml:"development"`
}

// 审计超时的默认值和上限。
const (
	DefaultTimeout = 45 * time.Minute
	MaxTimeout     = 3 * time.Hour
	minTimeout     = 5 * time.Minute
)

// 开发的默认值和上限（PLAN #26.7、#26.13）：单次执行最长 12 小时（C15），超过的任务本身就不合理。
const (
	DefaultDevTimeout   = 3 * time.Hour
	MaxDevTimeout       = 12 * time.Hour
	minDevTimeout       = 10 * time.Minute
	DefaultMaxFixRounds = 3
	DefaultMaxIssues    = 5
	// maxDevLimit 是 max_fix_rounds、max_issues 的上限。
	maxDevLimit = 10
)

// 文件的大小上限。
const (
	// MaxConfigSize 是配置文件的大小上限。
	MaxConfigSize = 64 << 10
	// MaxInstructionsSize 是审计说明（review.instructions）的大小上限，超出部分不交给 Agent。
	MaxInstructionsSize = 64 << 10
)

// Parse 解析并校验 .kiln/agent/config.yml。未知字段、不支持的平台或服务都返回错误，不静默忽略。
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxConfigSize {
		return nil, fmt.Errorf("%s 超过 %d KiB", ConfigPath, MaxConfigSize>>10)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s 是空文件", ConfigPath)
		}
		return nil, fmt.Errorf("%s：%w", ConfigPath, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("%s：%w", ConfigPath, err)
		}
		return nil, fmt.Errorf("%s 只能包含一个 YAML 文档", ConfigPath)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s：%w", ConfigPath, err)
	}
	return &c, nil
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// ReviewEnabled 判断是否启用审计（enabled 和 review.enabled 都没有关闭）。
func (c *Config) ReviewEnabled() bool {
	return boolOr(c.Enabled, true) && boolOr(c.Review.Enabled, true)
}

// PullRequestTrigger 判断 PR 事件是否触发审计。
func (c *Config) PullRequestTrigger() bool {
	return c.ReviewEnabled() && boolOr(c.Review.Triggers.PullRequest, true)
}

// ManualTrigger 判断是否允许手动触发审计。
func (c *Config) ManualTrigger() bool {
	return c.ReviewEnabled() && boolOr(c.Review.Triggers.Manual, true)
}

// PublishReviewComments 判断是否把 PR 审计的结果发布为 PR review 评论。
func (c *Config) PublishReviewComments() bool { return boolOr(c.Review.Publish.ReviewComments, true) }

// ReviewAgents 返回审计可以使用的 Agent（按优先级；至少一个）。
func (c *Config) ReviewAgents() []string { return c.Review.Agent }

// ReviewTimeout 返回审计的超时。
func (c *Config) ReviewTimeout() time.Duration {
	if c.Review.Timeout == "" {
		return DefaultTimeout
	}
	d, _ := time.ParseDuration(c.Review.Timeout) // validate 已经校验
	return d
}

// DevelopmentEnabled 判断是否启用开发（写了 development 段、没有关闭，顶层也没有关闭）。
func (c *Config) DevelopmentEnabled() bool {
	return boolOr(c.Enabled, true) && c.Development != nil && boolOr(c.Development.Enabled, true)
}

// DevPlatforms 返回开发可以执行的平台的 Runner 平台字符串（写的顺序；没有写 development.platform 时是 darwin/arm64）。
func (c *Config) DevPlatforms() []string {
	if c.Development == nil || c.Development.Platform == nil {
		return []string{defaultDevTarget.RunnerPlatform()}
	}
	return c.Development.Platform.RunnerPlatforms()
}

// DevTimeout 返回开发单次执行的超时。
func (c *Config) DevTimeout() time.Duration {
	if c.Development == nil || c.Development.Timeout == "" {
		return DefaultDevTimeout
	}
	d, _ := time.ParseDuration(c.Development.Timeout) // validate 已经校验
	return d
}

// MaxFixRounds 返回自动返修的轮数上限。
func (c *Config) MaxFixRounds() int {
	if c.Development == nil || c.Development.MaxFixRounds == 0 {
		return DefaultMaxFixRounds
	}
	return c.Development.MaxFixRounds
}

// MaxIssues 返回一批最多领取的 Issue 数。
func (c *Config) MaxIssues() int {
	if c.Development == nil || c.Development.MaxIssues == 0 {
		return DefaultMaxIssues
	}
	return c.Development.MaxIssues
}

// DevInstructionsPath 返回开发说明的路径（仓库相对路径）。
func (c *Config) DevInstructionsPath() string {
	if c.Development != nil && c.Development.Instructions != "" {
		return strings.TrimPrefix(c.Development.Instructions, "./")
	}
	return DefaultDevelopmentInstructions
}

// InstructionsPath 返回审计说明的路径（仓库相对路径）。
func (c *Config) InstructionsPath() string {
	if c.Review.Instructions != "" {
		return strings.TrimPrefix(c.Review.Instructions, "./")
	}
	return DefaultReviewInstructions
}

// Services 返回需要的标准测试服务（已排序、去重）。
func (c *Config) Services() []string {
	s := slices.Clone(c.Requirements.Services)
	slices.Sort(s)
	return slices.Compact(s)
}

// Tools 返回任务使用的受管理的工具链（声明的顺序，去重）。
func (c *Config) Tools() []string {
	var out []string
	for _, t := range c.Requirements.Tools {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// Repositories 返回声明的依赖仓库（声明的顺序，去重；名字不区分大小写，与 GitHub 相同）。
func (c *Config) Repositories() []string {
	var out []string
	for _, r := range c.Requirements.Repositories {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, r) }) {
			out = append(out, r)
		}
	}
	return out
}

// repoNameRE 是 requirements.repositories 中仓库的写法：owner/name（GitHub 的账号名和仓库名允许的字符）。
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

// ValidRepoName 判断 s 是否是 owner/name 形式的仓库名（不带 .git、协议和主机）。
func ValidRepoName(s string) bool {
	_, name, _ := strings.Cut(s, "/")
	return repoNameRE.MatchString(s) && name != "." && name != ".." && !strings.HasSuffix(strings.ToLower(name), ".git")
}

// ToolPlatforms 返回 lockfile 中工具链需要的平台（工具 → Runner 平台，例如 darwin/arm64）：启用的审计的
// required_platforms 和启用的开发的平台。kiln runtime lock 据此为 AI 任务锁定各平台的 digest，kiln lint 据此检查。
func (c *Config) ToolPlatforms() map[string][]string {
	tools := c.Tools()
	if len(tools) == 0 {
		return nil
	}
	var plats []string
	if c.ReviewEnabled() {
		for _, p := range c.Review.RequiredPlatforms {
			plats = append(plats, p.RunnerPlatforms()...)
		}
	}
	if c.DevelopmentEnabled() {
		plats = append(plats, c.DevPlatforms()...)
	}
	slices.Sort(plats)
	plats = slices.Compact(plats)
	if len(plats) == 0 {
		return nil
	}
	out := make(map[string][]string, len(tools))
	for _, t := range tools {
		out[t] = slices.Clone(plats)
	}
	return out
}

func (c *Config) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version 必须是 1（当前为 %d）", c.Version)
	}
	if err := c.validateAgents(); err != nil {
		return err
	}
	for _, s := range c.Requirements.Services {
		if !slices.Contains(supportedServices, s) {
			return fmt.Errorf("不支持的服务 %q（一期只提供标准的 %s）", s, strings.Join(supportedServices, "、"))
		}
	}
	for _, t := range c.Requirements.Tools {
		if !toolchain.Builtin(t) {
			return fmt.Errorf("requirements.tools 中的 %q 不是内置 runtime（只支持 %s）", t, strings.Join(toolchain.BuiltinNames(), "、"))
		}
	}
	for _, name := range c.Requirements.Repositories {
		if !ValidRepoName(name) {
			return fmt.Errorf("requirements.repositories 中的 %q 不是 owner/name 形式的仓库名（不带 https://、.git）", name)
		}
	}
	r := &c.Review
	if r.Timeout != "" {
		d, err := time.ParseDuration(r.Timeout)
		if err != nil {
			return fmt.Errorf("review.timeout %q 无效：%w", r.Timeout, err)
		}
		if d < minTimeout || d > MaxTimeout {
			return fmt.Errorf("review.timeout 必须在 %s 到 %s 之间", minTimeout, MaxTimeout)
		}
	}
	if r.MaxConcurrency < 0 {
		return errors.New("review.max_concurrency 不能为负数")
	}
	if !r.Triggers.Schedule.IsZero() {
		return errors.New("review.triggers.schedule（周期审计）暂不支持")
	}
	if err := checkRelPath("review.instructions", r.Instructions); err != nil {
		return err
	}
	if err := c.validateDevelopment(); err != nil {
		return err
	}
	if !c.ReviewEnabled() {
		return nil
	}
	// 启用审计时必须显式填写平台集合，不把缺失解释为"任意平台都行"
	if len(r.RequiredPlatforms) == 0 {
		return fmt.Errorf("启用审计时必须填写 review.required_platforms（支持 %s）", targetsText())
	}
	seen := map[string]bool{}
	for i := range r.RequiredPlatforms {
		if err := r.RequiredPlatforms[i].validate(fmt.Sprintf("review.required_platforms[%d]", i)); err != nil {
			return err
		}
		id := r.RequiredPlatforms[i].ID
		if seen[id] {
			return fmt.Errorf("review.required_platforms 中的 id %q 重复", id)
		}
		seen[id] = true
	}
	return nil
}

// validate 归一并校验 required_platforms 的一项（field 是它在配置中的位置），没有写 id 的单个平台补上 <os>-<arch>；
// 一组平台（any_of）必须写 id。平台本身的规则见 TargetSet.validate。
func (p *Platform) validate(field string) error {
	if err := p.TargetSet.validate(field); err != nil {
		return err
	}
	switch {
	case p.AnyOf != nil && p.ID == "":
		return fmt.Errorf("%s：一组平台（any_of）必须写 id（任务的名称是 review-<id>）", field)
	case p.ID == "":
		p.ID = p.OS + "-" + p.Arch
	}
	if !platformIDRE.MatchString(p.ID) {
		return fmt.Errorf("%s 的 id %q 无效（字母、数字、_ . -，最长 64 个字符）", field, p.ID)
	}
	return nil
}

// validate 归一并校验可以执行的平台（审计的一项、开发的平台共用这一条规则，field 是它在配置中的位置）：单个平台时
// os、arch 都要写；一组平台（any_of）不能为空、不能与 os / arch 同时写，组内的平台不能重复；每个平台都必须是支持的。
func (s *TargetSet) validate(field string) error {
	if s.AnyOf == nil {
		return s.Target.validate(field)
	}
	switch {
	case s.OS != "" || s.Arch != "":
		return fmt.Errorf("%s：any_of 与 os、arch 不能同时写（一组平台写在 any_of 中）", field)
	case len(s.AnyOf) == 0:
		return fmt.Errorf("%s 的 any_of 不能为空", field)
	}
	seen := map[Target]bool{}
	for j := range s.AnyOf {
		t := &s.AnyOf[j]
		if err := t.validate(fmt.Sprintf("%s.any_of[%d]", field, j)); err != nil {
			return err
		}
		if seen[*t] {
			return fmt.Errorf("%s.any_of 中的平台 %s 重复", field, t)
		}
		seen[*t] = true
	}
	return nil
}

// validate 归一并校验一个平台：os、arch 都要写，组合必须是支持的平台。
func (t *Target) validate(field string) error {
	t.OS, t.Arch = NormalizeOS(t.OS), NormalizeArch(t.Arch)
	if t.OS == "" || t.Arch == "" {
		return fmt.Errorf("%s 必须同时填写 os 和 arch", field)
	}
	if !slices.Contains(targets, *t) {
		return fmt.Errorf("%s：平台 %s 暂不支持（支持 %s）", field, t, targetsText())
	}
	return nil
}

// validateAgents 校验并填好审计的 Agent：review.agent 优先，不写时沿用顶层的 agent（一期写法），都不写为 codex；
// 两处同时写时报错（不猜优先级）。列表中的每个 Agent 都校验，并按写的顺序去重（顺序就是审计 Agent 的优先级，
// PLAN #33.7）。开发的 Agent 在 validateDevelopment 中校验。
func (c *Config) validateAgents() error {
	if c.Agent != "" && !ValidAgent(c.Agent) {
		return agentError("agent", c.Agent)
	}
	for _, a := range c.Review.Agent {
		if !ValidAgent(a) {
			return agentError("review.agent", a)
		}
	}
	switch {
	case c.Agent != "" && len(c.Review.Agent) > 0:
		return errors.New("顶层的 agent（一期写法，只表示审计的 Agent）与 review.agent 不能同时写，只保留 review.agent")
	case len(c.Review.Agent) == 0:
		c.Review.Agent = AgentList{cmp.Or(c.Agent, AgentCodex)}
	}
	out := make(AgentList, 0, len(c.Review.Agent))
	for _, a := range c.Review.Agent {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	c.Review.Agent = out
	return nil
}

// validateDevelopment 校验 development 段。
func (c *Config) validateDevelopment() error {
	d := c.Development
	if d == nil {
		return nil
	}
	if d.Agent != "" && !ValidAgent(d.Agent) {
		return agentError("development.agent", d.Agent)
	}
	if err := checkRelPath("development.instructions", d.Instructions); err != nil {
		return err
	}
	if d.Platform != nil {
		if err := d.Platform.validate("development.platform"); err != nil {
			return err
		}
	}
	if d.Timeout != "" {
		t, err := time.ParseDuration(d.Timeout)
		if err != nil {
			return fmt.Errorf("development.timeout %q 无效：%w", d.Timeout, err)
		}
		if t < minDevTimeout || t > MaxDevTimeout {
			return fmt.Errorf("development.timeout 必须在 %s 到 %s 之间", minDevTimeout, MaxDevTimeout)
		}
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"development.max_fix_rounds", d.MaxFixRounds}, {"development.max_issues", d.MaxIssues}} {
		if f.v < 0 || f.v > maxDevLimit {
			return fmt.Errorf("%s 必须在 1 到 %d 之间（不写为默认值）", f.name, maxDevLimit)
		}
	}
	if c.DevelopmentEnabled() && d.Agent == "" {
		// 开发必须显式选择 Agent，不沿用审计的、不给默认值
		return fmt.Errorf("启用开发时必须填写 development.agent（%s）", strings.Join(Agents(), "、"))
	}
	return nil
}

// checkRelPath 校验仓库内的相对路径（说明文件）。
func checkRelPath(field, p string) error {
	if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%s %q 必须是仓库内的相对路径", field, p)
	}
	return nil
}

// NormalizeOS 把系统名称归一为 linux / windows / macos（Darwin 显示与匹配为 macos）。
func NormalizeOS(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case "darwin", "mac", "osx":
		return OSMacOS
	}
	return s
}

// NormalizeArch 把架构名称归一为标准值：x86_64、x64 → amd64，aarch64 → arm64。
// 含糊的 arm 不归一为 arm64（未来支持 32 位 ARM 时另加明确的字段）。
func NormalizeArch(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case "x86_64", "x64":
		return ArchAMD64
	case "aarch64":
		return ArchARM64
	}
	return s
}

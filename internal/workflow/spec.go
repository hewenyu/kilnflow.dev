package workflow

import (
	"maps"
	"slices"
)

// JobKind 是展开后 job 的类别。
type JobKind string

// job 的类别。
const (
	// JobKindNormal 是在 Runner 上执行 steps 的普通 job。
	JobKindNormal JobKind = "normal"
	// JobKindPublish 是由 Server 执行的发布 job（release:）。
	JobKindPublish JobKind = "publish"
	// JobKindImage 是镜像组装 job（image:）。
	JobKindImage JobKind = "image"
)

// ExecutorKind 是执行 job 的方式。
type ExecutorKind string

// 执行方式。
const (
	// ExecutorHost 直接在 Runner 所在的机器上执行。
	ExecutorHost ExecutorKind = "host"
	// ExecutorOCI 在容器中执行（container:）。
	ExecutorOCI ExecutorKind = "oci"
	// ExecutorTart 在 tart 虚拟机中执行（vm:）。
	ExecutorTart ExecutorKind = "tart"
	// ExecutorImage 是镜像组装 job 的执行方式。
	ExecutorImage ExecutorKind = "image"
	// ExecutorServer 表示由 Server 执行（发布 job）。
	ExecutorServer ExecutorKind = "server"
)

// CapBuildkit 是 Runner 在 RunnerState.executors 中额外上报的能力：可以为 buildkit: true 的 job
// 启动 BuildKit daemon。它不是一种 executor，只用于调度过滤；属于线上协议，不能修改。
const CapBuildkit = "buildkit"

// CapSigning 是 Runner 在 RunnerState.executors 中额外上报的能力：可以为 signing 的 job 建立临时 keychain
// （macOS 上的 kiln-runner）。只用于调度过滤；属于线上协议，不能修改。
const CapSigning = "signing"

// BuildkitImage 是 buildkit: true 时 Runner 为 job 启动的 BuildKit daemon 镜像（PLAN #13.8），
// 由 Kiln 按 digest 固定（多架构 index，包含 linux/arm64 和 linux/amd64），计入 action key。
// 升级时同时更新 docs/PLAN.md #25。
const BuildkitImage = "docker.io/moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c"

// 发布 job 和镜像组装 job 的平台。
const (
	// PlatformServer 是发布 job 的平台：由 Server 执行，不需要 Runner。
	PlatformServer = "server"
	// PlatformLinuxAny 是镜像组装 job 的平台。
	PlatformLinuxAny = "linux/any"
)

// PlanContext 是展开 workflow 时可用的上下文。
type PlanContext struct {
	// GitHub 是 github 上下文的字段（PLAN #4.7）。
	GitHub map[string]any
	// Inputs 是 workflow_dispatch 的输入；其他事件为空。
	Inputs map[string]string
	// Vars 返回某个 environment（空字符串表示没有 environment）下合并后的变量。
	Vars func(environment string) map[string]string
	// CommitTime 是 github.sha 的 committer 时间（Unix 秒）。
	CommitTime int64
	// CheckRuntime 校验 runtime 在 lockfile 中有该平台的条目，nil 表示不校验。
	CheckRuntime func(name, platform string) error
	// SenderIsBot 为 true 时引用 secret 的 job 和发布 job 被 skipped（PLAN #16.1）。
	SenderIsBot bool
	// TrustedAutoTag 表示这是一次可信的自动 tag push（Server 按 .kiln/merge.yml 打的版本 tag，PLAN #29.4）。只由内部的
	// 规划器派生，不从 YAML / HTTP / CLI 读取：为真且 SenderIsBot 时仍保留真实的 SenderIsBot，但允许执行引用 secret 的
	// 签名 job 和发布 job。
	TrustedAutoTag bool
}

// Plan 是展开后的 workflow。
type Plan struct {
	Workflow         *Workflow
	ConcurrencyGroup string
	CancelInProgress bool
	Jobs             []*PlannedJob
	Diags            Diags
}

// RuntimePlatforms 汇总 plans 中各 job 用到的 runtime 及其平台（按字典序排列），用于生成 runtime lockfile。
// 只在平台不合法的 job 中用到的 runtime 也会出现，平台列表为空。
func RuntimePlatforms(plans []*Plan) map[string][]string {
	needs := map[string]map[string]bool{}
	for _, p := range plans {
		for _, j := range p.Jobs {
			if j.Spec == nil {
				continue
			}
			for _, rt := range j.Spec.Runtimes {
				if needs[rt] == nil {
					needs[rt] = map[string]bool{}
				}
				if ValidPlatform(j.Spec.Platform) {
					needs[rt][j.Spec.Platform] = true
				}
			}
		}
	}
	out := make(map[string][]string, len(needs))
	for name, plats := range needs {
		out[name] = slices.Sorted(maps.Keys(plats))
	}
	return out
}

// PlannedJob 是矩阵展开后的一个 job。
type PlannedJob struct {
	JobID       string
	Key         string
	DisplayName string
	Matrix      map[string]any
	MatrixKeys  []string
	Kind        JobKind
	NeedsKeys   []string
	SkipReason  string
	FailReason  string
	Spec        *Spec
}

// Spec 是 job 展开后的完整定义，保存在 jobs.spec 中。
type Spec struct {
	Workflow string       `json:"workflow"`
	JobID    string       `json:"job_id"`
	Platform string       `json:"platform"`
	Executor ExecutorKind `json:"executor"`

	Container string `json:"container,omitempty"`
	// BuildkitImage 非空表示 buildkit: true：Runner 为 job 启动这个 BuildKit daemon。
	BuildkitImage  string            `json:"buildkit_image,omitempty"`
	VM             string            `json:"vm,omitempty"`
	Host           string            `json:"host,omitempty"`
	Pool           string            `json:"pool,omitempty"`
	Runtimes       []string          `json:"runtimes,omitempty"`
	Inputs         []string          `json:"inputs"`
	InputsDefault  bool              `json:"inputs_default"`
	Outputs        []NamedPath       `json:"outputs,omitempty"`
	Fetch          []FetchRef        `json:"fetch,omitempty"`
	Network        bool              `json:"network"`
	Reproducible   bool              `json:"reproducible"`
	Cache          bool              `json:"cache"`
	Effects        bool              `json:"effects"`
	CPU            float64           `json:"cpu"`
	MemoryBytes    int64             `json:"memory_bytes"`
	Toolcache      []ToolCache       `json:"toolcache,omitempty"`
	Services       []ServiceSpec     `json:"services,omitempty"`
	Repositories   []string          `json:"repositories,omitempty"`
	Signing        *Signing          `json:"signing,omitempty"`
	Env            map[string]string `json:"env"`
	Steps          []StepSpec        `json:"steps,omitempty"`
	TimeoutMinutes int               `json:"timeout_minutes"`
	Environment    string            `json:"environment,omitempty"`
	EnvironmentURL string            `json:"environment_url,omitempty"`
	Secrets        []string          `json:"secrets,omitempty"`
	FailFast       bool              `json:"fail_fast"`
	MaxParallel    int               `json:"max_parallel,omitempty"`
	Release        *Release          `json:"release,omitempty"`
	Image          *ImageJob         `json:"image,omitempty"`
	// RuntimeRefs 由 Planner 根据 lockfile 填写。
	RuntimeRefs []RuntimeRef `json:"runtime_refs,omitempty"`
}

// RuntimeRef 是 lockfile 解析出的 runtime。
type RuntimeRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
}

// NamedPath 是 job 的一个输出：名字和工作区中的路径。
type NamedPath struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FetchRef 是解析到上游变体后的 fetch：从 JobKey 的 Output 取产物放到 To。
type FetchRef struct {
	JobKey string `json:"job_key"`
	Output string `json:"output"`
	To     string `json:"to"`
}

// ServiceSpec 是展开后的 service 容器。
type ServiceSpec struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	Command []string          `json:"command,omitempty"`
}

// StepSpec 是展开后的 step。
type StepSpec struct {
	Name             string            `json:"name"`
	Run              string            `json:"run"`
	Shell            string            `json:"shell"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	ContinueOnError  bool              `json:"continue_on_error,omitempty"`
}

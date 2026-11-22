// Package workflow 解析和展开 .kiln/workflows/*.yml（PLAN #4）。
// 解析（Parse）只检查结构，展开（Expand）在规划时对表达式求值、展开矩阵。
// Server 的 Planner 和 `kiln lint` 使用同一份代码。
//
// 文件划分：model.go（解析结果）、spec.go（展开结果）、decode.go（YAML 辅助）、
// parse.go / parse_release.go（解析）、validate.go（结构校验）、
// expand.go（展开 workflow）/ expand_job.go（展开 job 变体）/ expand_spec.go（填写 Spec）/ matrix.go（矩阵）、
// match.go（事件和 inputs 匹配）、github.go（github 上下文和 ref）。
package workflow

import (
	"fmt"
	"strings"
)

// Diag 是带位置的诊断信息。
type Diag struct {
	File    string
	Line    int
	Col     int
	Msg     string
	Warning bool
}

func (d Diag) String() string {
	kind := "错误"
	if d.Warning {
		kind = "警告"
	}
	if d.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s：%s", d.File, d.Line, d.Col, kind, d.Msg)
	}
	return fmt.Sprintf("%s: %s：%s", d.File, kind, d.Msg)
}

// Diags 是诊断列表。
type Diags []Diag

// HasErrors 判断是否有错误（警告不算）。
func (ds Diags) HasErrors() bool {
	for _, d := range ds {
		if !d.Warning {
			return true
		}
	}
	return false
}

// Errors 返回其中的错误。
func (ds Diags) Errors() Diags { return ds.filter(false) }

// Warnings 返回其中的警告。
func (ds Diags) Warnings() Diags { return ds.filter(true) }

func (ds Diags) filter(warning bool) Diags {
	var out Diags
	for _, d := range ds {
		if d.Warning == warning {
			out = append(out, d)
		}
	}
	return out
}

// Error 把全部诊断逐行连接起来，使 Diags 可以作为 error 使用。
func (ds Diags) Error() string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(d.String())
	}
	return b.String()
}

// Pos 是源文件中的位置。
type Pos struct{ Line, Col int }

// Workflow 是解析后的 workflow 文件。字符串字段保留原始的 ${{ }}。
type Workflow struct {
	File        string // 文件名，例如 ci.yml
	Name        string
	On          On
	Env         *OrderedMap
	Defaults    Defaults
	Concurrency *Concurrency
	Jobs        []*Job
	jobIndex    map[string]*Job
}

// Job 按 ID 查找 job。
func (w *Workflow) Job(id string) *Job { return w.jobIndex[id] }

// Base 返回 workflow 文件名去掉 .yml / .yaml。
func (w *Workflow) Base() string { return BaseName(w.File) }

// ReservedName 是 Kiln 内建的 AI 审计占用的 workflow 名字（runs.workflow、Check Run 名称和 .kiln/merge.yml 的
// required 中都用它）：.kiln/workflows 中不能有 ai-review.yml / ai-review.yaml，否则普通 workflow 会与 AI 审计混淆。
const ReservedName = "ai-review"

// BaseName 返回文件名去掉 .yml / .yaml。
func BaseName(file string) string {
	return strings.TrimSuffix(strings.TrimSuffix(file, ".yml"), ".yaml")
}

// On 是 workflow 的触发条件；没有配置的事件为 nil。
type On struct {
	Push             *PushTrigger
	PullRequest      *PRTrigger
	WorkflowDispatch *DispatchTrigger
	Schedule         []string // cron 表达式
}

// PushTrigger 是 on.push 的过滤条件。nil 表示没有写该字段。
type PushTrigger struct {
	Branches, BranchesIgnore []string
	Tags, TagsIgnore         []string
	Paths, PathsIgnore       []string
}

// PRTrigger 是 on.pull_request 的过滤条件。nil 表示没有写该字段。
type PRTrigger struct {
	Branches, BranchesIgnore []string
	Paths, PathsIgnore       []string
	Types                    []string
}

// workflow_dispatch 输入的类型（DispatchInput.Type）。
const (
	InputString  = "string" // 默认
	InputBoolean = "boolean"
	InputChoice  = "choice"
)

// DispatchInput 是 workflow_dispatch 的一个输入。
type DispatchInput struct {
	Name        string
	Description string
	Type        string // InputString、InputBoolean 或 InputChoice
	Default     string
	Required    bool
	Options     []string
}

// DispatchTrigger 是 on.workflow_dispatch。
type DispatchTrigger struct {
	Inputs []DispatchInput
}

// Defaults 是 defaults.run。
type Defaults struct {
	Shell            string
	WorkingDirectory string
}

// Concurrency 是 run 级或 job 级的 concurrency。
type Concurrency struct {
	Group            string
	CancelInProgress string // "true" | "false" | 表达式
	Pos              Pos
}

// Job 是解析后的 job，字段与 YAML 中的同名字段对应。
type Job struct {
	ID             string
	Pos            Pos
	Name           string
	Needs          []string
	If             string
	RunsOn         string
	Pool           string
	Host           string
	Container      string
	VM             string
	Runtimes       []string
	Inputs         []string // nil 表示默认值
	Outputs        *OrderedMap
	Fetch          []Fetch
	Network        bool
	Reproducible   bool
	Cache          *bool
	Resources      Resources
	Toolcache      []ToolCache
	Services       []Service
	Repositories   []string
	Effects        bool
	Release        *Release
	Image          *ImageJob
	Buildkit       bool
	Signing        *Signing
	TimeoutMinutes int
	Environment    string
	EnvironmentURL string
	Concurrency    *Concurrency
	Strategy       Strategy
	Env            *OrderedMap
	Defaults       Defaults
	Steps          []Step
}

// Fetch 是从上游 job 的输出取产物。
type Fetch struct {
	From   string // <job>.<output>
	To     string
	Matrix map[string]string
	Pos    Pos
}

// Split 把 From 拆成 job ID 和输出名。
func (f Fetch) Split() (job, output string) { return splitRef(f.From) }

// splitRef 把 <job>.<output> 拆开；没有 . 时 output 为空。
func splitRef(s string) (job, output string) {
	job, output, ok := strings.CutLast(s, ".")
	if !ok {
		return s, ""
	}
	return job, output
}

// Resources 是 job 申请的资源。
type Resources struct {
	CPU    float64
	Memory string // 例如 4Gi，见 ParseMemory
}

// ToolCache 是跨 run 保留的工具缓存目录，路径通过环境变量 Env 传给 job。
type ToolCache struct {
	Name string `json:"name"`
	Env  string `json:"env"`
}

// Service 是 job 的 service 容器。
type Service struct {
	Name    string
	Image   string
	Env     *OrderedMap
	Command []string
}

// Strategy 是 strategy。
type Strategy struct {
	Matrix      any // 原始值：map[string]any 或表达式字符串；nil 表示没有矩阵
	MatrixOrder []string
	// IncludeOrder 是 include 中出现的变量名（按首次出现的顺序），用于显示名。
	IncludeOrder []string
	FailFast     *bool
	MaxParallel  int
}

// Step 是解析后的 step。
type Step struct {
	ID               string
	Name             string
	If               string
	Run              string
	Shell            string
	WorkingDirectory string
	Env              *OrderedMap
	ContinueOnError  bool
	Pos              Pos
}

// Release 是发布 job 的发布目标。
//
// Release 及其引用的类型（ReleaseGitHub、Asset、NpmTarget、ImageTarget、RuntimeTarget、
// ImageJob、ImageFile、OrderedMap）经 Spec 保存到 jobs.spec。JSON 键名就是最初没有 tag 时
// 使用的 Go 字段名，已入库的数据依赖这些键名，改字段名时 tag 不能跟着改；Pos 只用于诊断，不保存。
//
// Assets、Checksums、Uploads 是 v0.25 新增的（PLAN #13.4、#13.9），用 omitempty，旧数据的 JSON 不变。
type Release struct {
	VerifyReproducible bool            `json:"VerifyReproducible"`
	GitHub             *ReleaseGitHub  `json:"GitHub"`
	Npm                []NpmTarget     `json:"Npm"`
	Images             []ImageTarget   `json:"Images"`
	Runtimes           []RuntimeTarget `json:"Runtimes"`
	// Assets 是文件类发布目标（github、uploads）共用的 asset（release.assets）。旧写法 release.github.assets
	// 在解析时移到这里，所以新规划的 job 中 GitHub.Assets 总是空的；jobs.spec 中的旧数据由 FileAssets 兼容。
	Assets []Asset `json:"Assets,omitempty"`
	// Checksums 是校验和文件名（release.checksums，旧写法 release.github.checksums），不写时为 SHA256SUMS。
	Checksums string         `json:"Checksums,omitempty"`
	Uploads   []UploadTarget `json:"Uploads,omitempty"`
}

// FileAssets 返回文件类发布目标共用的 asset 和校验和文件名：release.assets，
// 或 jobs.spec 中旧数据的 release.github.assets / checksums。
func (r *Release) FileAssets() ([]Asset, string) {
	if len(r.Assets) == 0 && r.GitHub != nil && len(r.GitHub.Assets) > 0 {
		return r.GitHub.Assets, r.GitHub.Checksums
	}
	checksums := r.Checksums
	if checksums == "" && r.GitHub != nil {
		checksums = r.GitHub.Checksums
	}
	return r.Assets, checksums
}

// HasFileTarget 判断是否有文件类发布目标（github、uploads）。
func (r *Release) HasFileTarget() bool { return r.GitHub != nil || len(r.Uploads) > 0 }

// HasTarget 判断是否有 Server 能执行的发布目标（github、uploads、images、npm）。
func (r *Release) HasTarget() bool { return r.HasFileTarget() || len(r.Images) > 0 || len(r.Npm) > 0 }

// 签名方式（release.github.sign、release.images.sign）；不写视为 SignNone。
const (
	SignNone   = "none"
	SignCosign = "cosign"
)

// ChecksumsNone 表示不生成校验和文件（release.checksums: none）；不写时生成 SHA256SUMS。
const ChecksumsNone = "none"

// ProvenanceNone 表示不生成 provenance（release.github.provenance）；不写同此。
const ProvenanceNone = "none"

// ReleaseGitHub 是 release.github：发布到 GitHub Release。
type ReleaseGitHub struct {
	Repository      string  `json:"Repository"`
	Target          string  `json:"Target"`
	AllowSourceName bool    `json:"AllowSourceName"`
	Assets          []Asset `json:"Assets"`
	Checksums       string  `json:"Checksums"`
	Sign            string  `json:"Sign"`
	Provenance      string  `json:"Provenance"`
	SBOM            *bool   `json:"SBOM"`
	NotesFrom       string  `json:"NotesFrom"`
	Prerelease      string  `json:"Prerelease"`
	Latest          string  `json:"Latest"`
}

// Asset 是文件类发布目标（GitHub Release、HTTP 上传）的一个 asset。
type Asset struct {
	From    string `json:"From"`
	Archive string `json:"Archive"` // release.ArchiveTarGz（默认）、release.ArchiveZip 或 release.ArchiveNone
	Name    string `json:"Name"`
	Pos     Pos    `json:"-"`
}

// NpmTarget 是 release.npm 的一项：发布为 npm 包 @<账号级 scope>/<Name>（PLAN #27.3）。
// registry、scope 和 token 在 Kiln 中按账号配置（kiln npm set），workflow 中不写凭据。
// Name 和 Package 中的字符串在发布时按上游变体求值（与 asset 的 name 相同，可以引用 matrix、github）。
type NpmTarget struct {
	From string `json:"From"`
	// Name 是不含 scope 的包名（别名）。
	Name string `json:"Name,omitempty"`
	// Files 是要放进包里的文件（glob，匹配输出中的相对路径），为空表示全部。输出是一个 .tgz 时不能写。
	Files []string `json:"Files,omitempty"`
	// Package 合并进生成的 package.json（同名字段以它为准）；不能包含 name、version。
	Package         map[string]any `json:"Package,omitempty"`
	Access          string         `json:"Access"`
	DistTag         string         `json:"DistTag"`
	AllowSourceName bool           `json:"AllowSourceName"`
	Pos             Pos            `json:"-"`
}

// npm 包的访问级别（release.npm[].access）。
const (
	NpmAccessPublic     = "public"
	NpmAccessRestricted = "restricted"
)

// NpmDistTagAuto 是 release.npm[].dist-tag 的默认值：按版本自动选择（PLAN #13.7）。
const NpmDistTagAuto = "auto"

// ImageTarget 是 release.images 的一项：推送容器镜像（PLAN #13.8）。
// 展开后（Spec 中）各字段已求值，Username、Password 中引用的 secret 保留为占位符。
type ImageTarget struct {
	From         string   `json:"From"`
	Name         string   `json:"Name"`
	Username     string   `json:"Username"`
	Password     string   `json:"Password"`
	Sign         string   `json:"Sign"`
	Tags         []string `json:"Tags"`
	FloatingTags []string `json:"FloatingTags"`
	Pos          Pos      `json:"-"`
}

// 取值 auto、true、false 的字段（release.github.latest / prerelease、release.uploads[].latest / prerelease）。
const (
	ChoiceAuto  = "auto"
	ChoiceTrue  = "true"
	ChoiceFalse = "false"
)

// UploadTarget 是 release.uploads 的一项：按 HTTP 上传协议 v1 把 asset 发布到第三方服务（PLAN #13.9）。
// 展开后（Spec 中）Name、URL 已求值（Name 不写时为 URL 的主机名），Token 中引用的 secret 保留为占位符。
type UploadTarget struct {
	Name       string `json:"Name"`
	URL        string `json:"URL"`
	Token      string `json:"Token"`
	Latest     string `json:"Latest"`     // auto（默认）、true、false
	Prerelease string `json:"Prerelease"` // auto（默认）、true、false
	Pos        Pos    `json:"-"`
}

// RuntimeTarget 是 release.runtimes 的一项：发布为 Kiln runtime。
type RuntimeTarget struct {
	From     string         `json:"From"`
	Name     string         `json:"Name"`
	Version  string         `json:"Version"`
	Platform string         `json:"Platform"`
	Env      map[string]any `json:"Env"`
}

// ImageJob 是镜像组装 job（image:）。
type ImageJob struct {
	Base       string      `json:"Base"`
	Platforms  []string    `json:"Platforms"`
	Files      []ImageFile `json:"Files"`
	Entrypoint []string    `json:"Entrypoint"`
	Cmd        []string    `json:"Cmd"`
	Env        *OrderedMap `json:"Env"`
	Workdir    string      `json:"Workdir"`
	User       string      `json:"User"`
	Labels     *OrderedMap `json:"Labels"`
}

// ImageFile 是放进镜像的上游输出。
type ImageFile struct {
	From   string            `json:"From"`
	Select map[string]string `json:"Select"`
	To     string            `json:"To"`
}

// OrderedMap 是保留声明顺序的字符串映射。
// 字段导出是为了 JSON 序列化（Spec 中的 ImageJob 会保存到数据库）。
type OrderedMap struct {
	Keys   []string          `json:"Keys"`
	Values map[string]string `json:"Values"`
}

// NewOrderedMap 返回空的 OrderedMap。
func NewOrderedMap() *OrderedMap { return &OrderedMap{Values: map[string]string{}} }

// Set 设置 k 的值；k 是新键时追加到末尾。
func (m *OrderedMap) Set(k, v string) {
	if m.Values == nil {
		m.Values = map[string]string{}
	}
	if _, ok := m.Values[k]; !ok {
		m.Keys = append(m.Keys, k)
	}
	m.Values[k] = v
}

// Len 返回键的个数（nil 安全）。
func (m *OrderedMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.Keys)
}

// Each 按声明顺序遍历（nil 安全）。
func (m *OrderedMap) Each(f func(k, v string)) {
	if m == nil {
		return
	}
	for _, k := range m.Keys {
		f(k, m.Values[k])
	}
}

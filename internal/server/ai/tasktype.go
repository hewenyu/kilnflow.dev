package ai

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 任务类型（PLAN #30.7）：每种 AI 任务（审计、开发）实现一个处理器，在组装时注册。调度、租约、取消、超时、并发组、日志
// 由生命周期代码处理，不随任务类型改变；新增一种任务类型只要一个处理器、明细表（如需要）、airunner.v2 的 oneof 分支、
// 连接器的执行流程和 Dashboard 的详情组件。

// Policy 是任务类型的固定规则。
type Policy struct {
	// InfraRetries 是领取后基础设施错误（含失联）自动重试的次数。AI 任务的基础设施由 AI 执行节点在领取前准备好（就绪检查），
	// 审计和开发都不重试（PLAN #30.13）。
	InfraRetries int
	// CheckRuns 表示在 GitHub 上建 Check Run（开发不是对某个 commit 的检查，不建）。
	CheckRuns bool
	// Rerunnable 表示请求可以重跑。
	Rerunnable bool
	// GroupMode 是并发组的规则：取代（审计）或排队（返修）。
	GroupMode GroupMode
	// GitWrite 表示可以给这类任务签发 git 的写凭据（IssueCredential，PLAN #33.13）：开发任务由连接器推 attempt 分支；
	// 审计只读。
	GitWrite bool
}

// GroupMode 是并发组的规则（PLAN #30.6）。
type GroupMode int

const (
	// GroupSupersede：同组中规划更早、head 不同的未结束请求取消（审计）。重跑不取代。
	GroupSupersede GroupMode = iota
	// GroupQueue：同组有执行中的请求时新请求排队，只留最新的一个（返修）。
	GroupQueue
)

// Result 是一次 attempt 的结果（Complete，或者 Server 判定的失联、超时）。
type Result struct {
	Outcome  store.Outcome
	ErrClass string
	Message  string
	// AgentVersion、AgentModel 是 Agent 的 CLI 版本和模型（拿不到时为空）。
	AgentVersion, AgentModel string
	// ReviewReport、Delivery 是随 Complete 提交的结果（对应 airunner.v2 CompleteRequest 的 oneof result），只交给任务类型解析。
	ReviewReport []byte
	Delivery     *airunnerv2.DevDelivery
	// Digest 是这次结果的内容摘要（PLAN #33.6）：同一个 attempt 用相同摘要重试时返回原来的回执，不同摘要返回
	// ALREADY_EXISTS。Orca 是这次提交的 Orca 关联（覆盖心跳上报的）。
	Digest string
	Orca   *OrcaBinding
}

// Failed 返回把这次结果改判为失败的结果：只改 outcome、错误分类和说明，提交本身的内容（报告、交付、Agent 版本、
// 模型、摘要、Orca 关联）保持不变。任务类型的 Finish 按业务结论改判时使用。
func (r Result) Failed(class, msg string) Result {
	r.Outcome, r.ErrClass, r.Message = store.OutcomeFailed, class, msg
	return r
}

// keepSubmission 返回 fin，其中属于提交本身的字段（摘要、Orca 关联、Agent 版本和模型）取自原始结果 sub：这些是
// "这次 Complete 请求"的事实，不是任务类型的业务结论，Finish 返回什么都不改变它们（结果重放按摘要判断，PLAN #33.6）。
func keepSubmission(fin, sub Result) Result {
	fin.Digest, fin.Orca, fin.AgentVersion, fin.AgentModel = sub.Digest, sub.Orca, sub.AgentVersion, sub.AgentModel
	return fin
}

// Runner 是一个 AI 执行节点在领取时的信息（来自 runners、ai_runners 和这次上报的状态）。
type Runner struct {
	ID       int64
	Name     string
	Role     string
	Version  string
	Platform string
	HostID   string
	// Capacity 是同时执行的任务数上限（连接器配置 max_tasks）。
	Capacity int
	// Agents 是这个节点提供、此刻可用的 Agent。
	Agents map[string]bool
	// AgentReasons 是提供但不可用的 Agent 的原因。
	AgentReasons map[string]string
	// OrcaReady 表示节点上的 Orca Server 就绪；不就绪时不派任务。
	OrcaReady   bool
	OrcaReason  string
	DockerReady bool
	// DockerReason 是 Docker / Compose 不就绪的原因（任务需要标准测试服务时要求就绪）。
	DockerReason string
	// ToolchainReady 表示节点能准备受管理的工具链（任务声明了 requirements.tools 时要求就绪，PLAN #33.12）；
	// ToolchainReason 是不就绪的原因，没有上报（不支持的旧连接器）时为空。
	ToolchainReady  bool
	ToolchainReason string
}

// SpecInput 是生成下发内容时用到的上下文。
type SpecInput struct {
	Task    *Task
	Request *Request
	Repo    *store.Repo
	// Platform 是 attempt 执行的平台（领取时节点上报的）。
	Platform string
}

// TaskType 是一种 AI 任务的处理器。
type TaskType interface {
	// Name 是任务类型（ai_tasks.type、ai_requests.type）：review | develop。
	Name() string
	// Role 是领取它的 AI 执行节点的角色。
	Role() string
	Policy() Policy
	// Services 是任务需要的标准测试服务（需要时 Docker / Compose 必须就绪），也是下发给节点的 AITask.services。
	Services(t *Task) []string
	// Tools 是任务在 platform（节点上报的平台）上使用的受管理的工具链（PLAN #33.12，需要时节点的工具链必须就绪），也是
	// 下发给节点的 AITask.tools。同一个工具在不同平台上版本相同、发行包（digest）不同。
	Tools(t *Task, platform string) []aispec.ToolRef
	// Repositories 是任务声明的依赖仓库（requirements.repositories，PLAN #33.13）：读凭据覆盖它们，也是下发给节点的
	// AITask.repositories。
	Repositories(t *Task) []string
	// Limit 是任务所在仓库同时执行的这类任务数上限（0 表示不限制，跨所有 AI 执行节点）。
	Limit(t *Task) int
	// Mismatch 返回这个节点不能领取各候选的类型特有原因（例如审计 Agent 开发过这个工作项的 PR）；能领取的不在结果中。
	Mismatch(ctx context.Context, tx pgx.Tx, r *Runner, cands []*Task) (map[int64]string, error)
	// Spec 生成下发给节点的任务（填写 oneof kind 和 prompt）。服务、Agent 和 prompt 的摘要由生命周期代码统一填写。
	// 任务本身的问题返回 *SpecError（只让这个任务失败）。
	Spec(ctx context.Context, q store.Querier, in SpecInput, out *airunnerv2.AITask) error
	// Claimed 在领取的事务中调用（例如记下开发过工作项的 Agent）。
	Claimed(ctx context.Context, tx pgx.Tx, t *Task, r *Runner) error
	// Finish 按结果规则（PLAN #30.6）处理 Runner 提交或 Server 判定的结果，返回最终的结果：例如审计的结论决定成功还是失败、
	// 保存报告和 PR review 意图；开发的交付由 aidev 推进。在结束任务的事务中调用，attempt 是当前的那次领取。返回值中的
	// 摘要、Orca 关联、Agent 版本和模型不起作用（finishTask 总是用提交的值，见 keepSubmission）；改判失败用 Result.Failed。
	Finish(ctx context.Context, tx pgx.Tx, t *Task, a *Attempt, res Result) (Result, error)
	// AfterFinish 是请求进入终态时类型特有的结束钩子（通知合并队列、aidev 等），在同一事务中调用。
	AfterFinish(ctx context.Context, tx pgx.Tx, r *Request) error
	// WaitReason 是任务排队时的说明（例如"等待 AI 审计节点领取"）。
	WaitReason() string
	// ValidateReport 校验 Agent 写出的报告（PLAN #33.5），不改变任何状态：返回不合格的原因（为空表示通过）。
	// 不属于这个任务类型的报告种类返回 ErrUnknownReportKind。
	ValidateReport(ctx context.Context, t *Task, attemptID int64, kind ReportKind, report []byte) ([]string, error)
}

// ReportKind 是 Agent 写出的报告的种类（ValidateReport）。
type ReportKind string

const (
	// ReportReview 是审计报告（aireport）。
	ReportReview ReportKind = "review"
	// ReportSelection 是选题报告（devreport.Selection）。
	ReportSelection ReportKind = "selection"
	// ReportDelivery 是交付报告（devreport.Delivery）。
	ReportDelivery ReportKind = "delivery"
)

// ErrUnknownReportKind 表示任务类型不认识这种报告（调用方指定的种类与任务不符）。
var ErrUnknownReportKind = errors.New("这种任务没有这种报告")

// SpecError 表示任务本身的问题导致无法生成下发内容（重试也不会好）。
type SpecError struct{ Err error }

func (e *SpecError) Error() string { return e.Err.Error() }
func (e *SpecError) Unwrap() error { return e.Err }

// Registry 是注册的任务类型。组装时创建，之后只读。
type Registry struct {
	types map[string]TaskType
	order []string
}

// NewRegistry 返回注册了 types 的 Registry；名称重复时 panic（组装错误）。
func NewRegistry(types ...TaskType) *Registry {
	r := &Registry{types: map[string]TaskType{}}
	for _, t := range types {
		if _, dup := r.types[t.Name()]; dup {
			panic(fmt.Sprintf("AI 任务类型 %s 重复注册", t.Name()))
		}
		r.types[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	return r
}

// Get 返回任务类型；没有注册时返回错误。
func (r *Registry) Get(name string) (TaskType, error) {
	if t, ok := r.types[name]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("没有注册的 AI 任务类型 %q", name)
}

// ForRole 返回角色 role 的 Runner 领取的任务类型。
func (r *Registry) ForRole(role string) []string {
	var out []string
	for _, n := range r.order {
		if r.types[n].Role() == role {
			out = append(out, n)
		}
	}
	return out
}

// Names 返回全部任务类型（注册顺序）。
func (r *Registry) Names() []string { return slices.Clone(r.order) }

// Config 是 AI 核心的配置（由 server.Config 映射）。
type Config struct {
	// LeaseTTL 是租约的有效期；CancelGrace 是取消后等待 Runner 确认的宽限。
	LeaseTTL, CancelGrace time.Duration
	// AcquireLongPoll 是 Acquire 长轮询的最长等待时间。
	AcquireLongPoll time.Duration
	// SweepInterval 是 sweeper 的周期。
	SweepInterval time.Duration
	// PublicURL 是 Dashboard 的地址（Check Run 的 details_url、失败 job 的地址）。
	PublicURL string
	// AppID 是 GitHub App 的 ID（按 external_id 查找本 App 创建的 Check Run）。
	AppID int64
}

package store

import (
	"fmt"
	"slices"
	"time"
)

// 状态值（PLAN #6.1）。数据库中保存的就是这些字符串，类型只用于在 Go 代码中区分含义。

// JobState 是 job 的状态。
type JobState string

const (
	JobWaiting   JobState = "waiting"
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobTimedOut  JobState = "timed_out"
	JobCancelled JobState = "cancelled"
	JobSkipped   JobState = "skipped"
)

// TerminalJobStates 返回 job 的全部终态；调用方拥有返回的切片，不会改动状态机规则。
func TerminalJobStates() []JobState {
	return []JobState{JobSucceeded, JobFailed, JobTimedOut, JobCancelled, JobSkipped}
}

// TerminalJobStateArgs 是 TerminalJobStates 的 []string 形式，作为 SQL 参数传入（state = ANY($n)），
// 避免在多条 SQL 中重复书写终态列表。
func TerminalJobStateArgs() []string { return jobStateArgs(TerminalJobStates()) }

// FailedJobStates 返回失败类终态（failed、timed_out）：计入 run 失败，并触发矩阵 fail-fast（Failed 按它判断）。
// 被连带取消（cancelled）或因上游没有成功而跳过（skipped）的 job 不算失败。调用方拥有返回的切片。
func FailedJobStates() []JobState { return []JobState{JobFailed, JobTimedOut} }

// FailedJobStateArgs 是 FailedJobStates 的 []string 形式，作为 SQL 参数传入（state = ANY($n)），使 SQL 中的失败
// 筛选与 Failed 是同一条规则。
func FailedJobStateArgs() []string { return jobStateArgs(FailedJobStates()) }

// jobStateArgs 把状态列表转换为 SQL 参数用的 []string。
func jobStateArgs(states []JobState) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

// JobStateOrder 返回汇总展示时各状态的顺序；每次返回独立的切片。
func JobStateOrder() []JobState {
	return []JobState{
		JobSucceeded, JobFailed, JobTimedOut, JobCancelled, JobSkipped,
		JobRunning, JobPending, JobWaiting,
	}
}

// Terminal 判断是否为终态。
func (s JobState) Terminal() bool { return slices.Contains(TerminalJobStates(), s) }

// Failed 判断是否为失败类终态（FailedJobStates）：计入 run 失败，并触发矩阵 fail-fast。
func (s JobState) Failed() bool { return slices.Contains(FailedJobStates(), s) }

// Queued 判断 job 是否还没有开始执行（等待上游或排队中）。
func (s JobState) Queued() bool {
	return s == JobWaiting || s == JobPending
}

// Title 是 Check Run 标题中的中文状态名。
func (s JobState) Title() string {
	switch s {
	case JobWaiting:
		return "等待上游"
	case JobPending:
		return "排队中"
	case JobRunning:
		return "执行中"
	case JobSucceeded:
		return "成功"
	case JobFailed:
		return "失败"
	case JobTimedOut:
		return "超时"
	case JobCancelled:
		return "已取消"
	case JobSkipped:
		return "已跳过"
	}
	return string(s)
}

// Conclusion 返回终态对应的 Check Run 结论。
func (s JobState) Conclusion() Conclusion {
	switch s {
	case JobSucceeded:
		return ConclusionSuccess
	case JobFailed:
		return ConclusionFailure
	case JobTimedOut:
		return ConclusionTimedOut
	case JobCancelled:
		return ConclusionCancelled
	case JobSkipped:
		return ConclusionSkipped
	}
	return ""
}

// RunState 是 run（以及 run_reruns）的状态。
type RunState string

const (
	RunQueued    RunState = "queued" // 并发组排队中（PLAN #6.5）
	RunActive    RunState = "active"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
	RunSkipped   RunState = "skipped" // 被 paths 排除，没有 job（PLAN #13.3）
)

// Terminal 判断是否为终态。
func (s RunState) Terminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled, RunSkipped:
		return true
	}
	return false
}

// Open 判断 run 是否还没有结束（排队中或执行中）。
func (s RunState) Open() bool { return s == RunQueued || s == RunActive }

// Conclusion 返回终态对应的 Check Run 结论。
func (s RunState) Conclusion() Conclusion {
	switch s {
	case RunSucceeded:
		return ConclusionSuccess
	case RunFailed:
		return ConclusionFailure
	case RunCancelled:
		return ConclusionCancelled
	case RunSkipped:
		return ConclusionSkipped
	}
	return ""
}

// ConclusionOrNil 返回写入 run_reruns.conclusion 的值：终态时为结论，否则为 NULL。
func (s RunState) ConclusionOrNil() *string {
	if !s.Terminal() {
		return nil
	}
	c := string(s.Conclusion())
	return &c
}

// Conclusion 是 GitHub Check Run 的结论，也保存在 run_reruns.conclusion 中。
type Conclusion string

const (
	ConclusionSuccess   Conclusion = "success"
	ConclusionFailure   Conclusion = "failure"
	ConclusionCancelled Conclusion = "cancelled"
	ConclusionSkipped   Conclusion = "skipped"
	ConclusionTimedOut  Conclusion = "timed_out"
)

// RunState 返回 run 结论对应的 run 终态。
func (c Conclusion) RunState() RunState {
	switch c {
	case ConclusionSuccess:
		return RunSucceeded
	case ConclusionCancelled:
		return RunCancelled
	case ConclusionSkipped:
		return RunSkipped
	}
	return RunFailed
}

// Outcome 是一次 attempt（或一次发布执行）的结果，保存在 attempts.outcome 中。
type Outcome string

const (
	OutcomeSucceeded  Outcome = "succeeded"
	OutcomeFailed     Outcome = "failed"
	OutcomeInfraError Outcome = "infra_error"
	OutcomeCancelled  Outcome = "cancelled"
	OutcomeTimedOut   Outcome = "timed_out"
)

// ReleaseState 返回发布结束后 releases.state 的值（取消也记为 failed）。
func (o Outcome) ReleaseState() string {
	if o == OutcomeSucceeded {
		return "succeeded"
	}
	return "failed"
}

// CancelReason 是 jobs.cancel_reason 的取值（用于 run 结论，PLAN #6.1）。
type CancelReason string

const (
	CancelByUser        CancelReason = "run_cancelled"
	CancelFailFast      CancelReason = "fail_fast"
	CancelByConcurrency CancelReason = "concurrency"
	CancelPRClosed      CancelReason = "pr_closed"
	CancelRefDeleted    CancelReason = "ref_deleted"
)

// Text 是展示给用户的取消原因。
func (r CancelReason) Text() string {
	switch r {
	case CancelFailFast:
		return "同一矩阵中有变体失败（fail-fast）"
	case CancelByConcurrency:
		return "被并发组中更新的 run 取代"
	case CancelPRClosed:
		return "PR 已关闭"
	case CancelRefDeleted:
		return "分支已删除"
	}
	return "已取消"
}

// RerunMode 是重跑方式（PLAN #6.6）。
type RerunMode string

const (
	RerunFailed     RerunMode = "failed"
	RerunAll        RerunMode = "all"
	RerunAllNoCache RerunMode = "all_no_cache"
	RerunJob        RerunMode = "job"
)

// ScopeKind 是变量、secret 的作用域类型（PLAN #16.4）。
type ScopeKind string

const (
	ScopeOwner       ScopeKind = "owner"
	ScopeRepo        ScopeKind = "repo"
	ScopeEnvironment ScopeKind = "environment"
)

// runners.state 的取值；draining、offline 只用于展示。
const (
	RunnerActive   = "active"
	RunnerRevoked  = "revoked"
	RunnerDraining = "draining"
	RunnerOffline  = "offline"
)

// DefaultPool 是没有指定 pool 时 Runner 所在的 pool。
const DefaultPool = "default"

// runners.kind 与 enrollment_tokens.kind 的取值：Runner 的类型由注册令牌决定。
const (
	// RunnerKindCI 是 kiln-runner（CI Runner），执行 workflow 的普通 job。
	RunnerKindCI = "ci"
	// RunnerKindAI 是 AI 执行节点（执行机器上的 kiln-orca 连接器，PLAN #33），只领取 AI 任务。
	RunnerKindAI = "ai"
)

// RunnerOnlineWindow 是 Runner 算作在线的最近一次领取的时间范围（长轮询约 25 秒一次）：调度择优、AI 开发判断有没有空闲的
// 开发机都按它。
const RunnerOnlineWindow = time.Minute

// ValidRunnerKind 判断 k 是否是合法的 Runner 类型。
func ValidRunnerKind(k string) bool { return k == RunnerKindCI || k == RunnerKindAI }

// AI 执行节点的角色（runners.role，PLAN #26.7）：由注册令牌决定，Runner 继承且不能修改。
const (
	// RunnerRoleReview 只领取 AI 审计 job。
	RunnerRoleReview = "review"
	// RunnerRoleDevelopment 只领取 AI 开发 job。
	RunnerRoleDevelopment = "development"
)

// RunnerRole 校验并返回 kind 类型的 Runner 的角色：CI Runner 没有角色（必须为空）；AI 执行节点为空时是审计。
func RunnerRole(kind, role string) (string, error) {
	switch {
	case kind != RunnerKindAI && role != "":
		return "", fmt.Errorf("只有 AI 执行节点有角色（%s 或 %s），CI Runner 不能设置", RunnerRoleReview, RunnerRoleDevelopment)
	case kind != RunnerKindAI:
		return "", nil
	case role == "":
		return RunnerRoleReview, nil
	case role == RunnerRoleReview || role == RunnerRoleDevelopment:
		return role, nil
	}
	return "", fmt.Errorf("无效的 AI 执行节点角色 %q（%s 或 %s）", role, RunnerRoleReview, RunnerRoleDevelopment)
}

// outbox.state 的取值。
const (
	OutboxPending = "pending"
	OutboxDone    = "done"
	OutboxDead    = "dead"
)

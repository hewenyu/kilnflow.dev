// Package aidev 是 AI 开发（PLAN #26.7–#26.16、PLAN #31）的 Server 端：工作项状态机、领取、交付的记录与推进、返修、
// 仓库级单一执行者（C19）。平台的读写经 forge.Repo，开发请求的规划、取消和执行经 AI 核心（internal/server/ai）。
package aidev

import "github.com/hewenyu/kilnflow.dev/internal/devreport"

// 工作项的阶段（dev_work_items.phase，PLAN #31.3）：每个阶段走一遍下面的状态。
const (
	PhaseDesign    = devreport.PhaseDesign
	PhaseImplement = devreport.PhaseImplement
)

// 工作项的状态（dev_work_items.state，PLAN #26.9）。
const (
	StateDeveloping = "developing" // 已领取，选题任务正在开发
	StateDelivering = "delivering" // 已交付，等执行者核对、推进分支、开 PR
	StatePROpen     = "pr_open"    // PR 已开，等 CI、审计、合并
	StateFixing     = "fixing"     // 返修任务已规划或正在执行
	StateBlocked    = "blocked"    // 受阻，等人处理（retry、stop、合并或关闭 PR）
	StateMerged     = "merged"     // PR 已合并（终态）
	StateStopped    = "stopped"    // 终止：PR 关闭未合并、去掉 ready、加 skip、Issue 全部关闭、人工终止（终态）
	StateNoChange   = "no_change"  // Agent 判断不需要改代码（终态）
)

// Active 判断状态是否未结束（占住仓库，PLAN #26.8 I2）。
func Active(state string) bool {
	switch state {
	case StateDeveloping, StateDelivering, StatePROpen, StateFixing, StateBlocked:
		return true
	}
	return false
}

// Event 是驱动工作项状态转移的事件。
type Event string

// 事件。
const (
	// EvDelivered：执行交付了有改动的 commit（交付报告 delivered，Runner 已推到 attempt 分支）。
	EvDelivered Event = "delivered"
	// EvNoChange：交付报告 no_change（不需要改代码）。返修时没有新提交按 EvNoProgress 处理。
	EvNoChange Event = "no_change"
	// EvNoProgress：返修没有新的提交（全部不采纳、无法完成），交给人裁决。
	EvNoProgress Event = "no_progress"
	// EvBlocked：交付报告 blocked。
	EvBlocked Event = "blocked"
	// EvExecFailed：执行失败、超时、取消，没有有效的交付。
	EvExecFailed Event = "exec_failed"
	// EvPromoted：执行者核对了交付、推进了正式分支、PR 已存在。
	EvPromoted Event = "promoted"
	// EvPromoteRejected：交付核对不通过（commit 不在远端、没有改动、正式分支分叉等）。
	EvPromoteRejected Event = "promote_rejected"
	// EvChecksFailed：PR head 的 CI 或 AI 审计没有通过，轮数未满。
	EvChecksFailed Event = "checks_failed"
	// EvRoundsExhausted：PR head 没有通过（或者冲突），返修轮数已用完。
	EvRoundsExhausted Event = "rounds_exhausted"
	// EvConflict：合并队列交回的冲突（PR head 与目标分支冲突，PLAN #26.11），本轮的冲突返修轮数未满。
	EvConflict Event = "conflict"
	// EvMerged：PR 已合并；实现阶段要求每个未关闭的 Issue 都完整解决（否则是 EvMergedPartial）。设计阶段进入实现。
	EvMerged Event = "merged"
	// EvMergedPartial：实现 PR 已合并，还有没有完整解决的 Issue（PLAN #31.3）：受阻，下一轮用新的分支。
	EvMergedPartial Event = "merged_partial"
	// EvStop：PR 关闭未合并、Issue 去掉 ready 或加了 skip、Issue 全部关闭、人工终止、仓库关闭开发。
	EvStop Event = "stop"
	// EvRetry：人对受阻的工作项要求继续（kiln ai dev retry）：本轮执行过任务时再返修一轮，没有执行过（部分完成后开始了
	// 新的轮次）时开始本轮的实现。
	EvRetry Event = "retry"
)

// Pos 是工作项在状态机中的位置：阶段和状态。
type Pos struct {
	Phase, State string
}

// Transition 返回位置 p 收到 ev 之后的位置；ran 表示本轮是否执行过任务（区分 retry 的两种起点）。ok 为 false 表示这个
// 组合不改变位置（例如终态收到任何事件、晚到的旧交付），调用方不做任何事。纯函数，全部组合见 TestTransition。
func Transition(p Pos, ran bool, ev Event) (next Pos, ok bool) {
	if !Active(p.State) {
		return p, false
	}
	to := func(state string) (Pos, bool) { return Pos{Phase: p.Phase, State: state}, true }
	switch ev {
	case EvMerged, EvMergedPartial:
		switch {
		case p.Phase == PhaseDesign:
			return Pos{Phase: PhaseImplement, State: StateDeveloping}, true
		case ev == EvMergedPartial:
			return to(StateBlocked)
		}
		return to(StateMerged)
	case EvStop:
		return to(StateStopped)
	}
	switch p.State {
	case StateDeveloping, StateFixing:
		switch ev {
		case EvDelivered:
			return to(StateDelivering)
		case EvNoChange:
			if p.State == StateDeveloping && p.Phase == PhaseImplement {
				return to(StateNoChange)
			}
			return to(StateBlocked) // 返修没有新提交交给人；设计阶段必须有设计文档
		case EvNoProgress, EvBlocked, EvExecFailed:
			return to(StateBlocked)
		}
	case StateDelivering:
		switch ev {
		case EvPromoted:
			return to(StatePROpen)
		case EvPromoteRejected:
			return to(StateBlocked)
		}
	case StatePROpen:
		switch ev {
		case EvChecksFailed, EvConflict:
			return to(StateFixing)
		case EvRoundsExhausted:
			return to(StateBlocked)
		}
	case StateBlocked:
		if ev == EvRetry {
			if !ran && p.Phase == PhaseImplement {
				return to(StateDeveloping)
			}
			return to(StateFixing)
		}
	}
	return p, false
}

// 状态 label（PLAN #26.11）：由执行者按工作项的状态同步到 Issue 上，只是展示，领取以 Postgres 为准。
const (
	LabelReady   = "kiln:ready"   // 人加：授权进入开发候选
	LabelSkip    = "kiln:skip"    // 人加：排除（优先于 ready）
	LabelClaimed = "kiln:claimed" // 已领取，正在开发
	LabelPROpen  = "kiln:pr-open" // 已有 PR，等待审计、返修或合并
	LabelBlocked = "kiln:blocked" // 受阻，原因在评论和 Dashboard 中
)

// stateLabels 返回执行者维护的全部状态 label（每次返回新的切片：调用方改不到共享状态）。
func stateLabels() []string { return []string{LabelClaimed, LabelPROpen, LabelBlocked} }

// LabelFor 返回状态对应的 Issue 状态 label；终态没有。
func LabelFor(state string) string {
	switch state {
	case StateDeveloping, StateDelivering:
		return LabelClaimed
	case StatePROpen, StateFixing:
		return LabelPROpen
	case StateBlocked:
		return LabelBlocked
	}
	return ""
}

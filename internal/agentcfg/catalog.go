package agentcfg

import (
	"slices"
	"time"
)

// AIProtocol 是当前的 AI 执行协议版本（kiln.airunner.v2，PLAN #33.6）。Server 只支持这个版本：上报其他版本的
// AI 执行节点（包括已经退役的 kiln-ai-runner）不派任务。
//
// 协议 10 起连接器随包分发 kiln-task skill，任务说明只保留任务本身的规则并指向它。
const AIProtocol = 10

// TaskDocMinProtocol 是外部设计文档工具（kiln-orca task doc，协议 9）所需的最低 AI 协议版本。它不能随 AIProtocol
// 漂移：Server 升级到协议 10 后，协议 9 的在途选题任务仍要能冻结并读取文档（协议 8 的节点没有这个命令）。
const TaskDocMinProtocol = 9

// AgentProbeInterval 是 AI 执行节点检测本机 Agent（--version）的周期：Runner 按它重新检测，Server 把检测时间超过它两倍的
// 结果显示为"未知"（PLAN #30.7）。
const AgentProbeInterval = 10 * time.Minute

// AI 任务类型（PLAN #30.7）：审计和开发。
const (
	TaskReview  = "review"
	TaskDevelop = "develop"
)

// AI 开发的模式（airunner.v2 DevTask.mode，PLAN #26.10），Server 与 Runner 共用。
const (
	// DevModeSelect 在默认分支的 head 上从候选 Issue 中选题，经 Server 领取后设计或开发。
	DevModeSelect = "select"
	// DevModeImplement 在默认分支的 head 上开始新一轮的实现（先设计的设计合并后、部分完成后重试，PLAN #31.3）。
	DevModeImplement = "implement"
	// DevModeFix 在正式分支的期望 head 上返修同一个工作项。
	DevModeFix = "fix"
)

// AgentInfo 是 Agent Catalog 的一项（PLAN #30.7 Agent 四层的第 2 层，Server 与 Runner 共用）。
type AgentInfo struct {
	// ID 是配置和协议中的名称（codex | claude-code）。
	ID string
	// Name 是显示名。
	Name string
	// Tasks 是这个 Agent 支持的任务类型。
	Tasks []string
	// MinProtocol 是使用它所需的最低 AI 协议版本。
	MinProtocol int
}

// Supports 判断 Agent 是否支持任务类型 task。
func (a AgentInfo) Supports(task string) bool { return slices.Contains(a.Tasks, task) }

// Catalog 返回支持的 Agent（按显示顺序）。只有 Catalog 中的 Agent 会被调度；Runner 上报的其他 Agent 忽略。
func Catalog() []AgentInfo {
	return []AgentInfo{
		{ID: AgentCodex, Name: "Codex", Tasks: []string{TaskReview, TaskDevelop}, MinProtocol: AIProtocol},
		{ID: AgentClaudeCode, Name: "Claude Code", Tasks: []string{TaskReview, TaskDevelop}, MinProtocol: AIProtocol},
	}
}

// LookupAgent 按 ID 查找 Catalog 中的 Agent。
func LookupAgent(id string) (AgentInfo, bool) {
	for _, a := range Catalog() {
		if a.ID == id {
			return a, true
		}
	}
	return AgentInfo{}, false
}

// Agents 返回 Catalog 中全部 Agent 的 ID（按显示顺序）。
func Agents() []string {
	var ids []string
	for _, a := range Catalog() {
		ids = append(ids, a.ID)
	}
	return ids
}

// AgentName 返回 Agent 的显示名；不在 Catalog 中时返回 ID。
func AgentName(id string) string {
	if a, ok := LookupAgent(id); ok {
		return a.Name
	}
	return id
}

// Package aispec 是 AI 任务冻结的执行输入（ai_tasks.spec，PLAN #30.4）：审计和开发的内容、开发的 Issue 与审计问题。
// 规划时生成，领取时据此生成下发给 AI 执行节点的 airunner.v2 消息；JSON 格式与迁移前 jobs.spec 中的 ai_review、ai_develop 相同。
package aispec

import (
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// AI 开发的模式（DevelopSpec.Mode，与下发给 Runner 的相同）。
const (
	DevSelect    = agentcfg.DevModeSelect
	DevImplement = agentcfg.DevModeImplement
	DevFix       = agentcfg.DevModeFix
)

// DocRef 是一次任务冻结的外部设计文档引用：标题、修订号和摘要。正文由 Agent 经 kiln-orca task doc get
// 按需读取（执行节点不持有外部文档凭据），所以这里只带指针，不把整棵树塞进 spec。
type DocRef struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Revision int64  `json:"revision"`
	SHA256   string `json:"sha256,omitempty"`
	URL      string `json:"url,omitempty"`
}

// DevelopSpec 是一次 AI 开发（PLAN #26.10）：由 aidev 执行者根据 .kiln/agent/config.yml 的 development 生成。
// 候选、Issue、问题都是生成时的快照：Runner 执行期间的变化由 Server 在领取和推进时核对。
type DevelopSpec struct {
	Mode  string `json:"mode"`
	Agent string `json:"agent"`
	// Instructions 是 .kiln/agent/development.md 的内容（可选，从默认分支读取）；ConfigSHA 是读取配置和说明的 commit。
	Instructions string `json:"instructions,omitempty"`
	ConfigSHA    string `json:"config_sha"`
	// select：选题轮次（领取或没有领取时结束这一轮）、一次最多领取的数量；DryRun 时只选题、不领取。候选不在 spec 中：
	// Runner 准备好后向 Server 要（越晚确定越完整，PLAN #26.10）。
	SelectSeq int  `json:"select_seq,omitempty"`
	MaxIssues int  `json:"max_issues,omitempty"`
	DryRun    bool `json:"dry_run,omitempty"`
	// MaxFixRounds 是自动返修的轮数上限（领取时写入工作项）。
	MaxFixRounds int `json:"max_fix_rounds,omitempty"`
	// implement、fix：工作项、正式分支、目标分支、PR（implement 没有）、工作项的 Issue、要处理的问题、失败的 CI、上一轮的
	// 交接、返修轮次（fix_rounds）。
	WorkItemID          int64        `json:"work_item_id,omitempty"`
	Branch              string       `json:"branch,omitempty"`
	BaseRef             string       `json:"base_ref,omitempty"`
	PRNumber            int          `json:"pr_number,omitempty"`
	Issues              []DevIssue   `json:"issues,omitempty"`
	Findings            []DevFinding `json:"findings,omitempty"`
	CIFailures          []string     `json:"ci_failures,omitempty"`
	Handoff             string       `json:"handoff,omitempty"`
	Round               int          `json:"round,omitempty"`
	RequirementsChanged bool         `json:"requirements_changed,omitempty"`
	// Services 是需要的标准测试服务（pg、redis）。
	Services []string `json:"services,omitempty"`
	// PlatformTools 是受管理的工具链（PLAN #33.12）：规划时按任务起点 commit 上的 .kiln/runtimes.lock 为开发可以执行的
	// 每个平台（development.platform）各解析一份（Runner 平台 → 工具），领取时按节点的平台下发对应的那一份。没有声明
	// 工具链时为空。
	PlatformTools map[string][]ToolRef `json:"platform_tools,omitempty"`
	// Repositories 是声明的依赖仓库（requirements.repositories，PLAN #33.13），与配置一样从默认分支读取。
	Repositories []string `json:"repositories,omitempty"`
	// implement：本轮的起点（规划时默认分支的 head，关联时记为工作项的 base_sha）。
	StartSHA string `json:"start_sha,omitempty"`
	// implement、fix：工作项的阶段、实现的轮次（PLAN #31.3）；已合并的设计 PR、合并得到的 commit 和设计文档。
	Phase      string   `json:"phase,omitempty"`
	ImplRound  int      `json:"impl_round,omitempty"`
	DesignPR   int      `json:"design_pr,omitempty"`
	DesignSHA  string   `json:"design_sha,omitempty"`
	DesignDocs []string `json:"design_docs,omitempty"`
	// Docs 是仓库绑定的外部设计文档快照，实现 / 返修任务附带，Agent 经 kiln-orca task doc 读取正文。
	Docs []DocRef `json:"docs,omitempty"`
	// fix：要一起解决的合并冲突（合并队列交回的，没有时为 nil）。
	Conflict *DevConflict `json:"conflict,omitempty"`
}

// ToolRef 是任务使用的一个受管理的工具（内置 runtime）：名称、版本、发行包的平台和 digest（PLAN #33.12）。
type ToolRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
}

// ResolveTools 按 lockfile 解析任务在 platform（Runner 平台，例如 darwin/arm64）上使用的工具链；names 为空时为 nil。
// 仓库中没有 lockfile（lock 为 nil）、缺少条目或平台时返回错误（配置错误：不换版本、不取最新）。
func ResolveTools(lock *toolchain.Lock, names []string, platform string) ([]ToolRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	refs, err := toolchain.ResolveRefs(lock, names, platform)
	if err != nil {
		return nil, fmt.Errorf("requirements.tools：%w", err)
	}
	out := make([]ToolRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, ToolRef{Name: r.Name, Version: r.Version, Platform: r.Platform, Digest: r.Digest})
	}
	return out, nil
}

// ResolvePlatformTools 为任务可以执行的每个平台（Runner 平台）各解析一份工具链（审计和开发共用）：Runner 平台 → 工具。
// names 为空时为 nil；任何一个平台缺少条目都返回错误（写明是哪个平台），不跳过这个平台。
func ResolvePlatformTools(lock *toolchain.Lock, names, platforms []string) (map[string][]ToolRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string][]ToolRef, len(platforms))
	for _, p := range platforms {
		refs, err := ResolveTools(lock, names, p)
		if err != nil {
			return nil, err
		}
		out[p] = refs
	}
	return out, nil
}

// DevIssue 是 AI 开发的一个候选或已领取的 Issue（快照）。Body 是需求数据，不是给 Agent 的指令（PLAN #26.15）。
type DevIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
	// Digest 是标题 + 正文的 sha256：领取时核对，内容变了的不领取。
	Digest string `json:"digest"`
	URL    string `json:"url,omitempty"`
}

// DevConflict 是返修要解决的合并冲突（合并队列更新 PR 分支时平台确认的，PLAN #26.11）：PR head Head 与目标分支 BaseRef
// 的 commit BaseSHA 冲突。
type DevConflict struct {
	Head    string `json:"head"`
	BaseRef string `json:"base_ref"`
	BaseSHA string `json:"base_sha"`
}

// DevFinding 是返修要处理的一个审计问题（稳定 ID F<审计任务>-<序号>）。
type DevFinding struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Detail   string `json:"detail"`
}

// ReviewSpec 是一次 AI 审计：由 Planner 根据 .kiln/agent/config.yml 生成。
type ReviewSpec struct {
	// Agent 是审计使用的 CLI（codex）。
	Agent string `json:"agent"`
	// Trigger 是触发来源：pull_request | manual。
	Trigger string `json:"trigger"`
	// PR 审计的 PR 编号、目标分支与其 commit、来源分支；手动审计时 PRNumber 为 0。
	PRNumber int    `json:"pr_number,omitempty"`
	BaseRef  string `json:"base_ref,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	HeadRef  string `json:"head_ref,omitempty"`
	// Instructions 是 .kiln/agent/review.md 的内容（可选）。
	Instructions string `json:"instructions,omitempty"`
	// Services 是需要的标准测试服务（pg、redis）。
	Services []string `json:"services,omitempty"`
	// PlatformTools 是受管理的工具链（PLAN #33.12）：规划时按被审 commit 上的 .kiln/runtimes.lock 为任务可以执行的每个平台
	// 各解析一份（Runner 平台 → 工具；同一个工具版本相同、发行包不同），领取时按节点的平台下发对应的那一份。没有声明
	// 工具链时为空。
	PlatformTools map[string][]ToolRef `json:"platform_tools,omitempty"`
	// Repositories 是声明的依赖仓库（requirements.repositories，PLAN #33.13）：PR 审计从 PR 的 base 读取（与审计说明相同，
	// PR 不能靠改配置给自己加读权限），手动审计从被审的 commit 读取。base 中没有配置或配置有错误时为空。
	Repositories []string `json:"repositories,omitempty"`
	// PlatformID 是 review.required_platforms 中这一项的 id（例如 macos-arm64，一组平台时是组的 id）。
	PlatformID string `json:"platform_id"`
	// ReviewComments 为 true 时 PR 审计的结果以 PR review 评论发布。
	ReviewComments bool `json:"review_comments"`
	// MaxConcurrency 是这个仓库同时执行的 AI 审计数上限（跨所有 AI 执行节点），0 表示不限制。
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// ConfigSHA 是读取 .kiln/agent/config.yml 的 commit（配置来源）。
	ConfigSHA string `json:"config_sha"`
	// InstructionsFrom 是读取审计说明的 commit：PR 审计是 PR 的 base（PLAN #29.6），手动审计是被审的 commit。
	InstructionsFrom string `json:"instructions_from,omitempty"`
	// PolicyDigest 是审计规则（agent、审计说明、报告版本）的摘要；沿用旧审计时要求相同（PLAN #29.6）。
	PolicyDigest string `json:"policy_digest,omitempty"`
	// WorkItemID 是 PR 所属的 AI 开发工作项（不是 AI 开发的 PR 时为 0）；PriorFindings 是上一轮审计的问题和开发给出的处置，
	// 审计据此判断处置是否成立（PLAN #26.11）。
	WorkItemID    int64          `json:"work_item_id,omitempty"`
	PriorFindings []PriorFinding `json:"prior_findings,omitempty"`
	// Phase 是审计方式（PLAN #31.5）：design 按设计审计（被审 head 的完整改动只有 Markdown 的设计 PR），implement 按实现
	// 审计；不是工作项的 PR 时为空。DesignPR、DesignSHA 是已合并的设计；PhaseNote 是审计方式的说明。
	Phase     string `json:"phase,omitempty"`
	DesignPR  int    `json:"design_pr,omitempty"`
	DesignSHA string `json:"design_sha,omitempty"`
	PhaseNote string `json:"phase_note,omitempty"`
	// Docs 是工作项冻结的外部设计文档快照：审计与开发读到同一份设计依据。
	Docs []DocRef `json:"docs,omitempty"`
	// DocsSource 是审计的文档依据来源（snapshot_set | work_item | none | legacy | unplanned，见 docsync 的常量）：
	// 独立审计冻结自己的快照集，工作项 PR 复用工作项的快照。#213 之后规划的任务一律显式记录。
	DocsSource string `json:"docs_source,omitempty"`
	// DocSnapshotSetID 是独立审计冻结的快照集（DocsSource=snapshot_set 时非零）。提交时回填，与数据库关系一致。
	DocSnapshotSetID int64 `json:"doc_snapshot_set_id,omitempty"`
	// DocsManifestDigest 是文档依据的确定性摘要（含来源域与每篇文档的元数据）：重跑沿用与审计规则摘要都要求相同。
	DocsManifestDigest string `json:"docs_manifest_digest,omitempty"`
}

// PriorFinding 是上一轮审计的一个问题和开发给出的处置（fixed | rejected | duplicate | unable）。
type PriorFinding struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Disposition string `json:"disposition"`
	Note        string `json:"note,omitempty"`
}

// AI 审计的触发来源（ReviewSpec.Trigger）。
const (
	TriggerPullRequest = "pull_request"
	TriggerManual      = "manual"
)

// EventDevelop 是开发请求的 event（ai_requests.event）：开发不是对某个 commit 的检查，没有 Check Run。
const EventDevelop = "ai-develop"

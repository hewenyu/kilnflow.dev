import { z } from 'zod'
import { defaultLocale, msg, translate, type Locale } from '../../i18n/messages'
import { list } from '../../lib/envelope'
import { reportSchema } from './report'

// AI 页面的数据（internal/server/web 的 ai_requests.go、ai_runners.go；PLAN #30.9、PLAN #33）：AI 请求、AI 任务和 AI 执行节点
// 与 CI 的 run、job、Runner 分开。

/** aiRequestView：一个 AI 请求（审计或开发）。 */
export const aiRequestSchema = z.object({
  ID: z.number(),
  /** review 或 develop（名称按语言显示，见 parts.tsx 的 requestTitle）。 */
  Type: z.string(),
  Repo: z.string(),
  Event: z.string(),
  Ref: z.string(),
  SHA: z.string(),
  Trigger: z.string(),
  Sender: z.string(),
  /** queued、active、succeeded、failed、cancelled、skipped */
  State: z.string(),
  /** 审计的 PR（手动审计、开发请求为 0）。 */
  PRNumber: z.number(),
  /** 关联的 AI 开发工作项（没有时为 0 和空）。 */
  WorkItemID: z.number(),
  WorkItemURL: z.string(),
  /** 开发请求的方式（select 选题、fix 返修）和返修轮次。 */
  DevMode: z.string(),
  DevRound: z.number(),
  /** 被重跑的请求（不是重跑时为 0）。 */
  RerunOf: z.number(),
  ConfigError: z.string(),
  Warnings: z.string(),
  Created: z.string(),
  Finished: z.string().nullable(),
  /** 迁移前的 run（迁移之后新建的请求为 0）。 */
  /** 日志按保留策略清理的时间。 */
  Purged: z.string().nullable(),
})
export type AIRequest = z.infer<typeof aiRequestSchema>

/** aiTaskView：一个 AI 任务（review.required_platforms 的每一项一个，一项可以是一组可以互相替代的平台）。 */
export const aiTaskSchema = z.object({
  ID: z.number(),
  RequestID: z.number(),
  Key: z.string(),
  DisplayName: z.string(),
  /** 可以执行这个任务的平台（一组平台时多个）。 */
  Platforms: list(z.string()),
  /** 当前 attempt 实际执行的平台（还没有领取时为空）。 */
  Platform: z.string(),
  Agent: z.string(),
  /** waiting、pending、running、succeeded、failed、timed_out、cancelled、skipped（名称按语言显示，见 parts.tsx 的 taskState）。 */
  State: z.string(),
  /** 失败、跳过的原因（服务端记下的原文，原样显示）。 */
  Reason: z.string(),
  /** 沿用结果的来源任务（没有沿用时为 0）。 */
  ReusedFrom: z.number(),
  AttemptCount: z.number(),
  Runner: z.string(),
  CancelRequested: z.boolean(),
  Created: z.string(),
  Started: z.string().nullable(),
  Finished: z.string().nullable(),
})
export type AITask = z.infer<typeof aiTaskSchema>

/** taskPlatform 是任务的平台：已经领取的是实际执行的平台，还没有领取的是可以执行的平台（一组平台时用"或"连接，locale 不传时为 zh-CN）。 */
export function taskPlatform(t: Pick<AITask, 'Platform' | 'Platforms'>, locale: Locale = defaultLocale): string {
  return t.Platform || t.Platforms.join(translate(locale, msg('aiTask.platformOr')))
}

/** ai.OrcaBinding：一次执行在 Orca 中的关联（PLAN #33.4），连接器上报，没有上报的字段不出现。 */
export const orcaBindingSchema = z.object({
  runtime_id: z.string().optional(),
  /** 启动请求的幂等键（Orca 的 --retry-request）。 */
  start_request_id: z.string().optional(),
  run_id: z.string().optional(),
  task_id: z.string().optional(),
  dispatch_id: z.string().optional(),
  worktree_path: z.string().optional(),
  worktree_name: z.string().optional(),
})
export type OrcaBinding = z.infer<typeof orcaBindingSchema>

/** aiAttemptView：AI 任务的一次 attempt。Started、Finished 是 Unix 秒（没有结束时为 0）。 */
export const aiAttemptSchema = z.object({
  ID: z.number(),
  Epoch: z.number(),
  /** 第 N 次 attempt。 */
  N: z.number(),
  Runner: z.string(),
  /** 领取时节点上报的平台（实际执行的平台）。 */
  Platform: z.string(),
  /** 空串表示还在执行。 */
  Outcome: z.string(),
  ErrorClass: z.string(),
  Message: z.string(),
  /** 执行时的连接器（kiln-orca）、Agent 版本和模型（拿不到时为空）。 */
  RunnerVersion: z.string(),
  AgentVersion: z.string(),
  AgentModel: z.string(),
  /** 在 Orca 中的关联；Stage（prepare、start、running、finishing）、Text 是心跳上报的进度。 */
  Orca: orcaBindingSchema,
  Stage: z.string(),
  Text: z.string(),
  Started: z.number(),
  Finished: z.number(),
  Current: z.boolean(),
})
export type AIAttempt = z.infer<typeof aiAttemptSchema>

/** aireport.Report：AI 审计报告（v2 的 finding 有 scenario、actual、expected、impact、proof，v1 的报告中为空）。 */
export const aiReportSchema = z.object({
  /** 报告格式的版本；0 表示 v1。 */
  version: z.number(),
  /** no_issues、issues_found、incomplete */
  verdict: z.string(),
  summary: z.string(),
  /** 审计范围。 */
  scope: z.string(),
  findings: list(
    z.object({
      title: z.string(),
      /** high、medium、low */
      severity: z.string(),
      /** reproduced、code_evidence、suspicion */
      evidence: z.string(),
      /** 仓库相对路径；line 为 0 表示不针对具体行。 */
      path: z.string(),
      line: z.number(),
      /** 触发条件、实际与预期行为、影响、证据、修复方向（v2）。 */
      scenario: z.string(),
      actual: z.string(),
      expected: z.string(),
      impact: z.string(),
      proof: z.string(),
      fix: z.string(),
      /** v1 中是问题的全部说明；v2 中是补充说明。 */
      body: z.string(),
    }),
  ),
  /** 精简的验证结果。kind：change、suite、mutation；result：passed、failed、not_run。 */
  checks: list(z.object({ kind: z.string(), title: z.string(), result: z.string(), detail: z.string() })),
  unverified: list(z.string()),
  /** 环境备注（不影响结论）。 */
  env_notes: list(z.string()),
  /** 顺带发现、与本次改动无关的已有问题。 */
  preexisting: list(z.object({ title: z.string(), path: z.string(), line: z.number(), detail: z.string() })),
  /** 完整的命令记录。outcome：passed、failed、env_resolved、expected_failure、no_match、explore；v1 为空。 */
  commands_run: list(z.object({ command: z.string(), exit_code: z.number(), outcome: z.string(), note: z.string() })),
})
export type AIReport = z.infer<typeof aiReportSchema>

/** aiReviewView：AI 审计任务的结果（review.GetReport）。 */
export const aiReviewSchema = z.object({
  Agent: z.string(),
  Verdict: z.string(),
  Report: aiReportSchema,
  /** 被审计的 PR；手动审计为 0。 */
  PRNumber: z.number(),
  HeadSHA: z.string(),
  BaseSHA: z.string(),
  /** PR review 的发布状态：none（不发布）、pending、published、failed。 */
  ReviewState: z.string(),
  ReviewURL: z.string(),
  ReviewError: z.string(),
  Created: z.string(),
})
export type AIReview = z.infer<typeof aiReviewSchema>

// AI 开发任务保存的报告（PLAN #26.15）：没有领取的选题（没有选中、dry_run、领取失败）不属于任何工作项，计划只能在这里查看。
export const aiDevelopSchema = z.object({
  Reports: list(reportSchema),
  /** 这次执行领取或返修的工作项；没有时为空。 */
  WorkItemURL: z.string(),
})
export type AIDevelop = z.infer<typeof aiDevelopSchema>

/** aiRunningTask：AI 执行节点正在执行的一个 AI 任务。 */
export const aiRunningTaskSchema = z.object({
  RunnerID: z.number(),
  TaskID: z.number(),
  RequestID: z.number(),
  /** review 或 develop。 */
  Type: z.string(),
  Name: z.string(),
  Repo: z.string(),
  /** 在 Orca 中的关联；Stage、Text 是心跳上报的进度。 */
  Orca: orcaBindingSchema,
  Stage: z.string(),
  Text: z.string(),
  TaskURL: z.string(),
  Started: z.string(),
})
export type AIRunningTask = z.infer<typeof aiRunningTaskSchema>

/** aiAgentView：AI 执行节点本机的一个 Agent。 */
export const aiAgentSchema = z.object({
  /** codex 或 claude；Name 是显示名称。 */
  Agent: z.string(),
  Name: z.string(),
  Version: z.string(),
  /** available、unavailable、backoff 或 unknown；Reason 是不可用、退避或未知的原因。 */
  State: z.string(),
  Reason: z.string(),
  CheckedAt: z.string().nullable(),
  BackoffUntil: z.string().nullable(),
})
export type AIAgent = z.infer<typeof aiAgentSchema>

/** aiRunnerView：一个 AI 执行节点（Orca Server + kiln-orca 连接器）。 */
export const aiRunnerSchema = z.object({
  ID: z.number(),
  Name: z.string(),
  /** review（审计）或 development（开发）；名称见 RunnerSchema 的 runnerKindLabel。 */
  Role: z.string(),
  /** 连接器（kiln-orca）的版本。 */
  Version: z.string(),
  Platform: z.string(),
  /** 上报的 AI 协议版本；NeedsUpgrade 表示不是 Server 支持的版本（不派任务，需要用 npm 升级连接器）。 */
  Protocol: z.number(),
  NeedsUpgrade: z.boolean(),
  HostID: z.string(),
  Revoked: z.boolean(),
  Drain: z.boolean(),
  Online: z.boolean(),
  /** 同时执行的任务数上限（连接器配置的 max_tasks）。 */
  Capacity: z.number(),
  LastSeen: z.string().nullable(),
  /** ready、unavailable 或 unknown；*Reason 是不可用的原因。OrcaVersion、OrcaRuntimeID 是 Orca Server 的版本和运行实例。 */
  OrcaState: z.string(),
  OrcaReason: z.string(),
  OrcaVersion: z.string(),
  OrcaRuntimeID: z.string(),
  DockerState: z.string(),
  DockerReason: z.string(),
  /** 受管理的工具链（requirements.tools，PLAN #33.12）能否准备；旧连接器没有上报时为 unknown。 */
  ToolchainState: z.string(),
  ToolchainReason: z.string(),
  /** 最近一次上报状态的时间。 */
  ReportedAt: z.string().nullable(),
  Agents: list(aiAgentSchema),
  Tasks: list(aiRunningTaskSchema),
  Accepting: z.boolean(),
  /** 连接状态的样式（ok、warn、bad）；文字见 RunnerSchema 的 connLabel。 */
  ConnClass: z.string(),
})
export type AIRunner = z.infer<typeof aiRunnerSchema>

/** aiRunnerCounts：全部 AI 执行节点的统计。 */
export const aiRunnerCountsSchema = z.object({
  Total: z.number(),
  Online: z.number(),
  Accepting: z.number(),
  Paused: z.number(),
  Offline: z.number(),
  Revoked: z.number(),
  Tasks: z.number(),
})
export type AIRunnerCounts = z.infer<typeof aiRunnerCountsSchema>

/** aiRequestURL、aiTaskURL、aiRunnerURL 返回详情页的地址（与服务端相同）。 */
export const aiRequestURL = (id: number) => `/ai/requests/${id}`
export const aiTaskURL = (id: number) => `/ai/tasks/${id}`
export const aiRunnerURL = (name: string) => `/ai/runners/${encodeURIComponent(name)}`


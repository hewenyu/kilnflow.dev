import { z } from 'zod'
import { list } from './envelope'
import { lookup } from './lookup'

// AI 开发（PLAN #26.15）的工作项：列表页和详情页共用的数据格式和中文说明。

/** aiDevIssueSchema 是工作项中的一个 Issue（internal/server/web 的 aiDevIssue）。 */
const aiDevIssueSchema = z.object({
  Number: z.number(),
  Title: z.string(),
  /** 交付报告给出的完成度：full、partial、none；还没有交付时为空。 */
  Resolution: z.string(),
  /** 仍属于这个工作项。 */
  Active: z.boolean(),
  /** Issue 已经关闭。 */
  Closed: z.boolean(),
})
export type AIDevIssue = z.infer<typeof aiDevIssueSchema>

/** aiDevItemSchema 是一个工作项（internal/server/web 的 aiDevItem）。 */
export const aiDevItemSchema = z.object({
  ID: z.number(),
  Repo: z.string(),
  State: z.string(),
  /** 还没有结束（可以终止）。 */
  Active: z.boolean(),
  /** 受阻，可以重试（再返修一轮，或者开始下一轮实现）。 */
  Retryable: z.boolean(),
  Issues: list(aiDevIssueSchema),
  /** PR 号，还没有开 PR 时为 0。 */
  PR: z.number(),
  /** 正式分支。 */
  Branch: z.string(),
  HeadSHA: z.string(),
  /** 本阶段、本轮因为审计、CI 没有通过返修的轮数。 */
  FixRounds: z.number(),
  /** 本阶段、本轮的冲突返修轮数（合并队列交回的冲突，与审计返修分开计，上限相同）。 */
  ConflictRounds: z.number(),
  MaxFixRounds: z.number(),
  Agent: z.string(),
  /** 阶段（PLAN #31.3）：design | implement。 */
  Phase: z.string(),
  /** 先设计（有设计阶段）；否则一个 PR 完成。 */
  DesignFirst: z.boolean(),
  /** 已合并的设计 PR，没有时为 0。 */
  DesignPR: z.number(),
  /** 实现的轮次（部分完成之后加一）。 */
  ImplRound: z.number(),
  /** 部分完成的实现 PR 已合并、下一轮还没开始：重试开始下一轮实现。 */
  NextRound: z.boolean(),
  /** 交付需要人确认（原因在 PR 描述中），PR 加了 kiln:hold。 */
  Hold: z.boolean(),
  /** 领取后 Issue 的内容变了。 */
  RequirementsChanged: z.boolean(),
  /** 受阻、终止的原因，或者当前在等什么。 */
  Reason: z.string(),
  Created: z.string(),
  Updated: z.string(),
  Finished: z.string().nullable(),
  /** 详情页的地址。 */
  URL: z.string(),
})
export type AIDevItem = z.infer<typeof aiDevItemSchema>

/** poolIssueSchema 是任务池中的一个 Issue（internal/server/aidev 的 PoolIssue）。 */
const poolIssueSchema = z.object({
  Number: z.number(),
  Title: z.string(),
  /** GitHub 上 Issue 的地址（可能为空）。 */
  URL: z.string(),
  /** candidate、claimed、skip_label、waiting、skipped。 */
  Status: z.string(),
  /** 领取了它的工作项（claimed 时）。 */
  WorkItem: z.number().nullable(),
  /** 跳过或等待的原因（skipped、waiting 时）。 */
  Reason: z.string(),
  /** 还没关闭的依赖（waiting 时）。 */
  WaitingOn: list(z.number()),
  ReadyAt: z.string(),
})
export type PoolIssue = z.infer<typeof poolIssueSchema>

/** poolSchema 是一个仓库的任务池（internal/server/aidev 的 PoolView）：带 kiln:ready 的打开的 Issue。 */
export const poolSchema = z.object({
  Issues: list(poolIssueSchema),
  /** 上次全量同步的时间；没有同步过时为 null。 */
  SyncedAt: z.string().nullable(),
})

type LabelVariant = 'secondary' | 'accent' | 'success' | 'attention' | 'danger' | 'done'

/** 工作项状态的中文和颜色：进行中琥珀、等人处理红、已开 PR 蓝、已合并紫、其余灰（DESIGN.md Labels）。 */
const states: Record<string, { text: string; variant: LabelVariant }> = {
  developing: { text: '开发中', variant: 'attention' },
  delivering: { text: '等待推进', variant: 'attention' },
  pr_open: { text: 'PR 已开', variant: 'accent' },
  fixing: { text: '返修中', variant: 'attention' },
  blocked: { text: '受阻', variant: 'danger' },
  merged: { text: '已合并', variant: 'done' },
  stopped: { text: '已终止', variant: 'secondary' },
  no_change: { text: '无需改代码', variant: 'secondary' },
}

/** workItemState 返回工作项状态的中文和 Label 颜色；未知的状态原样显示。 */
export function workItemState(state: string): { text: string; variant: LabelVariant } {
  return lookup(states, state) ?? { text: state, variant: 'secondary' }
}

/** 任务池中 Issue 情况的中文和颜色：可选蓝、已领取琥珀、跳过的灰（DESIGN.md Labels）。 */
const poolStatuses: Record<string, { text: string; variant: LabelVariant }> = {
  candidate: { text: '可选', variant: 'accent' },
  claimed: { text: '已领取', variant: 'attention' },
  skip_label: { text: 'kiln:skip', variant: 'secondary' },
  waiting: { text: '等待依赖', variant: 'secondary' },
  skipped: { text: '已跳过', variant: 'secondary' },
}

/** poolStatus 返回任务池中 Issue 情况的中文和 Label 颜色；未知的情况原样显示。 */
export function poolStatus(status: string): { text: string; variant: LabelVariant } {
  return lookup(poolStatuses, status) ?? { text: status, variant: 'secondary' }
}

const resolutions: Record<string, string> = {
  full: '完整解决',
  partial: '部分解决',
  none: '未解决',
}

/** resolution 返回 Issue 完成度的中文；还没有交付（空）时返回空串。 */
export function resolution(r: string): string {
  return lookup(resolutions, r) ?? r
}

/** workItemPhase 返回先设计的工作项所处的阶段（"设计"、"实现"，部分完成之后带轮次）；一个 PR 完成的第一轮返回空串。 */
export function workItemPhase(it: Pick<AIDevItem, 'Phase' | 'DesignFirst' | 'ImplRound'>): string {
  let p = ''
  if (it.Phase === 'design') p = '设计'
  else if (it.DesignFirst) p = '实现'
  if (it.ImplRound > 1) p = `${p || '实现'}第 ${it.ImplRound} 轮`
  return p
}

/** workItemMode 返回工作项的交付方式。 */
export function workItemMode(it: Pick<AIDevItem, 'DesignFirst'>): string {
  return it.DesignFirst ? '先设计：设计 PR 合并后再实现' : '一个 PR 完成（先设计再开发）'
}

/** retryText 返回重试按钮的文字。 */
export function retryText(it: Pick<AIDevItem, 'NextRound'>): string {
  return it.NextRound ? '开始下一轮实现' : '再返修一轮'
}

/** fixRounds 返回返修轮次的说明，例如"返修 1 / 2 轮 · 冲突返修 1 / 2 轮"；还没有返修时返回空串。 */
export function fixRounds(it: Pick<AIDevItem, 'FixRounds' | 'ConflictRounds' | 'MaxFixRounds'>): string {
  const parts: string[] = []
  if (it.FixRounds > 0) parts.push(`返修 ${it.FixRounds} / ${it.MaxFixRounds} 轮`)
  if (it.ConflictRounds > 0) parts.push(`冲突返修 ${it.ConflictRounds} / ${it.MaxFixRounds} 轮`)
  return parts.join(' · ')
}

/** occupancySchema 是仓库的 AI 开发占用（internal/server/web 的 aiDevOccupancy，PLAN #26.9 I14）。 */
export const occupancySchema = z.object({
  /** 占用者（没有被占用时为 0）。 */
  Occupant: z.number(),
  /** 不能开始新的工作项的原因。 */
  Reason: z.string(),
  /** 排在占用者之后的未结束的工作项（升级前留下的）。 */
  Waiting: list(z.number()),
})
export type Occupancy = z.infer<typeof occupancySchema>

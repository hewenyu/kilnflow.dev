import { z } from 'zod'
import { defaultLocale, type Locale } from '../i18n/locale-id'
import { msg, translate, type MsgKey, type Params } from '../i18n/messages'
import { list } from './envelope'
import { enumLabel } from './format'
import { lookup } from './lookup'

// AI 开发（PLAN #26.15）的工作项：列表页和详情页共用的数据格式和按语言显示的说明（locale 不传时为 zh-CN）。

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

/** 工作项状态的颜色：进行中琥珀、等人处理红、已开 PR 蓝、已合并紫、其余灰（DESIGN.md Labels）；名称在词典 workItemState.* 中。 */
const stateVariants: Record<string, LabelVariant> = {
  developing: 'attention',
  delivering: 'attention',
  pr_open: 'accent',
  fixing: 'attention',
  blocked: 'danger',
  merged: 'done',
  stopped: 'secondary',
  no_change: 'secondary',
}

/** workItemState 返回工作项状态的名称和 Label 颜色；未知的状态原样显示（locale 不传时为 zh-CN）。 */
export function workItemState(state: string, locale: Locale = defaultLocale): { text: string; variant: LabelVariant } {
  return { text: enumLabel(locale, 'workItemState', state), variant: lookup(stateVariants, state) ?? 'secondary' }
}

/** 任务池中 Issue 情况的颜色：可选蓝、已领取琥珀、跳过的灰（DESIGN.md Labels）；名称在词典 poolStatus.* 中。 */
const poolVariants: Record<string, LabelVariant> = {
  candidate: 'accent',
  claimed: 'attention',
  skip_label: 'secondary',
  waiting: 'secondary',
  skipped: 'secondary',
}

/** poolStatus 返回任务池中 Issue 情况的名称和 Label 颜色；未知的情况原样显示。 */
export function poolStatus(status: string, locale: Locale = defaultLocale): { text: string; variant: LabelVariant } {
  return { text: enumLabel(locale, 'poolStatus', status), variant: lookup(poolVariants, status) ?? 'secondary' }
}

/** resolution 返回 Issue 完成度（full、partial、none）的名称；还没有交付（空）时返回空串，未知的原样返回。 */
export function resolution(r: string, locale: Locale = defaultLocale): string {
  return r === '' ? '' : enumLabel(locale, 'resolution', r)
}

const tr = (locale: Locale, id: MsgKey, params?: Params) => translate(locale, msg(id, params))

/** phaseText 返回工作项阶段（design、implement）的名称；未知的原样返回。 */
export function phaseText(phase: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'workItemPhase', phase)
}

/** workItemPhase 返回先设计的工作项所处的阶段（"设计"、"实现"，部分完成之后带轮次）；一个 PR 完成的第一轮返回空串。 */
export function workItemPhase(it: Pick<AIDevItem, 'Phase' | 'DesignFirst' | 'ImplRound'>, locale: Locale = defaultLocale): string {
  let p = ''
  if (it.Phase === 'design') p = tr(locale, 'workItemPhase.design')
  else if (it.DesignFirst) p = tr(locale, 'workItemPhase.implement')
  if (it.ImplRound > 1) p = tr(locale, 'workItemPhase.round', { phase: p || tr(locale, 'workItemPhase.implement'), round: it.ImplRound })
  return p
}

/** workItemMode 返回工作项的交付方式。 */
export function workItemMode(it: Pick<AIDevItem, 'DesignFirst'>, locale: Locale = defaultLocale): string {
  return tr(locale, it.DesignFirst ? 'workItemMode.designFirst' : 'workItemMode.onePR')
}

/** retryText 返回重试按钮的文字。 */
export function retryText(it: Pick<AIDevItem, 'NextRound'>, locale: Locale = defaultLocale): string {
  return tr(locale, it.NextRound ? 'workItem.retryNextRound' : 'workItem.retryFix')
}

/** fixRounds 返回返修轮次的说明，例如"返修 1 / 2 轮 · 冲突返修 1 / 2 轮"；还没有返修时返回空串。 */
export function fixRounds(it: Pick<AIDevItem, 'FixRounds' | 'ConflictRounds' | 'MaxFixRounds'>, locale: Locale = defaultLocale): string {
  const parts: string[] = []
  if (it.FixRounds > 0) parts.push(tr(locale, 'workItem.fixRounds', { n: it.FixRounds, max: it.MaxFixRounds }))
  if (it.ConflictRounds > 0) parts.push(tr(locale, 'workItem.conflictRounds', { n: it.ConflictRounds, max: it.MaxFixRounds }))
  return parts.join(' · ')
}

/** occupancySchema 是仓库的 AI 开发占用（internal/server/web 的 aiDevOccupancy，PLAN #26.9 I14）。 */
export const occupancySchema = z.object({
  /** 占用者（没有被占用时为 0）。 */
  Occupant: z.number(),
  /** 占用者的阶段（design、implement）和状态（名称按语言显示）。 */
  Phase: z.string(),
  State: z.string(),
  /** 占用者记下的原因或在等什么（原样显示）。 */
  Reason: z.string(),
  /** 排在占用者之后的未结束的工作项（升级前留下的）。 */
  Waiting: list(z.number()),
})
export type Occupancy = z.infer<typeof occupancySchema>

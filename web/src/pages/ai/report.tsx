import { Label } from '@primer/react'
import { z } from 'zod'
import { Box, BoxHeader, BoxRow } from '../../components/Box'
import { Ago } from '../../components/Time'
import { Trans, useLocale, useT } from '../../i18n/locale'
import { resolution } from '../../lib/aidev'
import { list } from '../../lib/envelope'
import { enumLabel, short } from '../../lib/format'
import css from '../ai_dev_item.module.css'

// AI 开发报告（PLAN #26.15）：选题报告和交付报告的数据格式与展示。工作项详情页和 AI 任务详情页共用，
// 放在 pages/ai/ 下，避免 ai/ 反过来依赖 ai_dev_item.tsx。界面文字按语言显示；报告中 Agent 写的内容（计划、理由、摘要、
// 验收项、命令、决定等）是原文，原样显示，不翻译。

/** 选题报告（devreport.Selection，字段名与报告 JSON 相同）。 */
export const selectionSchema = z.object({
  selected: list(z.number()),
  batch_reason: z.string(),
  plan: z.string(),
  /** 交付方式（PLAN #31.2）：true 先交付设计 PR，false 一个 PR 完成；没有选或旧报告时没有。 */
  design_first: z.boolean().nullable().optional(),
  design_reason: z.string().optional(),
  /** later：只是不放进这一批（留在任务池给之后的选题）；wait_for：等这些 Issue 关闭（PLAN #31.6）。旧报告没有这两个字段。 */
  skipped: list(
    z.object({
      number: z.number(),
      reason: z.string(),
      reconsider: z.string(),
      later: z.boolean().optional(),
      wait_for: list(z.number()).optional(),
    }),
  ),
})
type Selection = z.infer<typeof selectionSchema>

/** 交付报告（devreport.Delivery）。 */
export const deliverySchema = z.object({
  /** delivered、no_change 或 blocked。 */
  status: z.string(),
  summary: z.string(),
  pr_title: z.string(),
  issues: list(
    z.object({
      number: z.number(),
      resolution: z.string(),
      acceptance: list(z.object({ item: z.string(), done: z.boolean(), evidence: z.string() })),
      notes: z.string(),
    }),
  ),
  verification: list(z.object({ command: z.string(), outcome: z.string(), note: z.string() })),
  dispositions: list(
    z.object({ finding: z.string(), action: z.string(), commit: z.string(), duplicate_of: z.string(), reason: z.string() }),
  ),
  handoff: z.object({ done: z.string(), remaining: z.string(), decisions: z.string(), next: z.string() }),
  /** 设计段（PLAN #31.4）：设计文档、决策记录、需要人提供的条件；旧报告没有。 */
  design: z
    .object({
      documents: list(z.string()),
      decisions: list(
        z.object({
          category: z.string(),
          ref: z.string(),
          context: z.string(),
          options: z.string(),
          choice: z.string(),
          impact: z.string(),
          review: z.string(),
        }),
      ),
      external_inputs: list(z.object({ what: z.string(), blocks: z.string(), resume: z.string() })),
    })
    .optional(),
  /** 与已合并设计的出入（实现阶段）；旧报告没有。 */
  design_deviations: list(z.object({ item: z.string(), reason: z.string() })).optional(),
})
type Delivery = z.infer<typeof deliverySchema>

export const reportSchema = z.object({
  /** selection 或 delivery。 */
  Kind: z.string(),
  /** 产生报告的 AI 任务和它的详情页。 */
  TaskID: z.number(),
  TaskURL: z.string(),
  /** 解析后的报告（按 Kind 二选一）；解析不了时两者都为 null，Raw 是原文。 */
  Selection: selectionSchema.nullable(),
  Delivery: deliverySchema.nullable(),
  Raw: z.string(),
  /** 适用范围（PLAN #26.14）：拿不到时为空，显示"未知"。 */
  RunnerVersion: z.string(),
  AgentVersion: z.string(),
  Model: z.string(),
  Created: z.string(),
})
type Report = z.infer<typeof reportSchema>

/** AgentSays 标明 Agent 自己写的内容（没有经过 Kiln 核实）。 */
export function AgentSays() {
  const t = useT()
  return (
    <Label variant="attention" title={t('devReport.agentSaysTitle')}>
      {t('devReport.agentSays')}
    </Label>
  )
}

/** Scope 显示报告的适用范围：Runner / Agent 版本和模型，拿不到的显示"未知"。 */
function Scope({ r }: { r: Report }) {
  const t = useT()
  const unknown = t('devReport.unknown')
  return (
    <p className={css.meta}>
      <Trans
        id="devReport.scope"
        params={{ runner: r.RunnerVersion || unknown, agent: r.AgentVersion || unknown, model: r.Model || unknown, task: r.TaskID }}
        tags={{ link: (c) => <a href={r.TaskURL}>{c}</a>, ago: () => <Ago t={r.Created} /> }}
      />
    </p>
  )
}

/** issueRefs 返回 Issue 编号的列表，例如"#1、#2"。 */
function issueRefs(ns: readonly number[], sep: string): string {
  return ns.map((n) => `#${n}`).join(sep)
}

/** Text 显示 Agent 写的一段文字（保留换行）；为空时不显示。 */
function Text({ k, v }: { k: string; v: string }) {
  if (!v) return null
  return (
    <div className={css.field}>
      <h4 className={css.h}>{k}</h4>
      <p className={css.pre}>{v}</p>
    </div>
  )
}

function SelectionBody({ s }: { s: Selection }) {
  const t = useT()
  const sep = t('list.enum')
  const colon = t('list.colon')
  return (
    <>
      <p className={css.p}>{t('devReport.selected', { list: s.selected.length > 0 ? issueRefs(s.selected, sep) : t('devReport.selectedNone') })}</p>
      {s.design_first != null && (
        <p className={css.p}>{t('devReport.mode', { mode: t(s.design_first ? 'devReport.modeDesignFirst' : 'workItemMode.onePR') })}</p>
      )}
      {s.design_reason && <Text k={t('devReport.modeReason')} v={s.design_reason} />}
      <Text k={t('devReport.batchReason')} v={s.batch_reason} />
      <Text k={t('devReport.plan')} v={s.plan} />
      {s.skipped.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.skipped')}</h4>
          <ul className={css.list}>
            {s.skipped.map((sk) => (
              <li key={sk.number}>
                #{sk.number}
                {colon}
                {sk.reason}
                {sk.later && <span className={css.muted}>{t('devReport.later')}</span>}
                {sk.wait_for && sk.wait_for.length > 0 && <span className={css.muted}>{t('devReport.waitFor', { list: issueRefs(sk.wait_for, sep) })}</span>}
                {sk.reconsider && <span className={css.muted}>{t('devReport.reconsider', { text: sk.reconsider })}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

function DeliveryBody({ d }: { d: Delivery }) {
  const t = useT()
  const { locale } = useLocale()
  const colon = t('list.colon')
  const h = d.handoff
  return (
    <>
      <p className={css.p}>
        {t('devReport.outcomeLine', { status: enumLabel(locale, 'devReport.status', d.status) })}
        {d.pr_title && t('devReport.prTitle', { title: d.pr_title })}
      </p>
      <Text k={t('devReport.summary')} v={d.summary} />
      {d.issues.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.issues')}</h4>
          <ul className={css.list}>
            {d.issues.map((is) => (
              <li key={is.number}>
                #{is.number}
                {colon}
                {resolution(is.resolution, locale)}
                {is.acceptance.length > 0 && (
                  <ul className={css.list}>
                    {is.acceptance.map((a, i) => (
                      <li key={i}>
                        {t(a.done ? 'devReport.done' : 'devReport.notDone')}
                        {colon}
                        {a.item}
                        {a.evidence && <span className={css.muted}>{t('devReport.evidence', { text: a.evidence })}</span>}
                      </li>
                    ))}
                  </ul>
                )}
                {is.notes && <p className={css.pre}>{is.notes}</p>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {d.verification.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.verification')}</h4>
          <ul className={css.list}>
            {d.verification.map((v, i) => (
              <li key={i}>
                <code>{v.command}</code>
                {colon}
                {enumLabel(locale, 'devReport.outcome', v.outcome)}
                {v.note && <span className={css.muted}>{t('devReport.paren', { text: v.note })}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {d.dispositions.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.dispositions')}</h4>
          <ul className={css.list}>
            {d.dispositions.map((x) => (
              <li key={x.finding}>
                <code>{x.finding}</code>
                {colon}
                {enumLabel(locale, 'devReport.action', x.action)}
                {x.commit && (
                  <>
                    {' '}
                    <Trans id="devReport.commit" params={{ sha: short(x.commit) }} tags={{ code: (c) => <code>{c}</code> }} />
                  </>
                )}
                {x.duplicate_of && t('devReport.duplicateOf', { ref: x.duplicate_of })}
                {x.reason && <span className={css.muted}> · {x.reason}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {d.design && <DesignBody g={d.design} />}
      {d.design_deviations && d.design_deviations.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.deviations')}</h4>
          <ul className={css.list}>
            {d.design_deviations.map((x, i) => (
              <li key={i}>
                {x.item}
                {x.reason && <span className={css.muted}>{t('devReport.reason', { text: x.reason })}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      <Text k={t('devReport.handoffDone')} v={h.done} />
      <Text k={t('devReport.handoffRemaining')} v={h.remaining} />
      <Text k={t('devReport.handoffDecisions')} v={h.decisions} />
      <Text k={t('devReport.handoffNext')} v={h.next} />
    </>
  )
}

// 决定的类别（devreport.Decision.Category，与 PR 描述的说法相同）在词典 devReport.category.* 中。

function DesignBody({ g }: { g: NonNullable<Delivery['design']> }) {
  const t = useT()
  const { locale } = useLocale()
  const colon = t('list.colon')
  return (
    <>
      {g.documents.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.documents')}</h4>
          <ul className={css.list}>
            {g.documents.map((doc) => (
              <li key={doc}>
                <code>{doc}</code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {g.decisions.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.decisions')}</h4>
          <ul className={css.list}>
            {g.decisions.map((x, i) => (
              <li key={i}>
                {enumLabel(locale, 'devReport.category', x.category)}
                {x.ref && t('devReport.paren', { text: x.ref })}
                {colon}
                {x.choice}
                {x.context && <p className={css.pre}>{t('devReport.context', { text: x.context })}</p>}
                {x.options && <p className={css.pre}>{t('devReport.options', { text: x.options })}</p>}
                {x.impact && <p className={css.pre}>{t('devReport.impact', { text: x.impact })}</p>}
                {x.review && <p className={css.pre}>{t('devReport.review', { text: x.review })}</p>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {g.external_inputs.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>{t('devReport.externalInputs')}</h4>
          <ul className={css.list}>
            {g.external_inputs.map((x, i) => (
              <li key={i}>
                {x.what}
                {x.blocks && <span className={css.muted}>{t('devReport.blocks', { text: x.blocks })}</span>}
                {x.resume && <span className={css.muted}>{t('devReport.resume', { text: x.resume })}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

export function ReportBox({ r, n }: { r: Report; n: number }) {
  const t = useT()
  const { locale } = useLocale()
  const id = `ai-dev-report-${n}`
  return (
    <Box as="section" aria-labelledby={id}>
      <BoxHeader
        id={id}
        title={
          <>
            {enumLabel(locale, 'devReport.kind', r.Kind)} <AgentSays />
          </>
        }
      />
      <BoxRow className={css.block}>
        <Scope r={r} />
        {r.Selection && <SelectionBody s={r.Selection} />}
        {r.Delivery && <DeliveryBody d={r.Delivery} />}
        {r.Raw && (
          <>
            <p className={css.p}>{t('devReport.raw')}</p>
            <pre className={css.raw}>{r.Raw}</pre>
          </>
        )}
      </BoxRow>
    </Box>
  )
}

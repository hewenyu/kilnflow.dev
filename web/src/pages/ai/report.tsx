import { Label } from '@primer/react'
import { z } from 'zod'
import { Box, BoxHeader, BoxRow } from '../../components/Box'
import { Ago } from '../../components/Time'
import { resolution } from '../../lib/aidev'
import { list } from '../../lib/envelope'
import { short } from '../../lib/format'
import { lookup } from '../../lib/lookup'
import css from '../ai_dev_item.module.css'

// AI 开发报告（PLAN #26.15）：选题报告和交付报告的数据格式与展示。工作项详情页和 AI 任务详情页共用，
// 放在 pages/ai/ 下，避免 ai/ 反过来依赖 ai_dev_item.tsx。

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

/** unknownText 是适用范围拿不到时显示的文字。 */
export const unknownText = '未知'

/** AgentSays 标明 Agent 自己写的内容（没有经过 Kiln 核实）。 */
export function AgentSays() {
  return (
    <Label variant="attention" title="这部分内容由 Agent 写在报告中，Kiln 没有核实">
      Agent 自报
    </Label>
  )
}

/** Scope 显示报告的适用范围：Runner / Agent 版本和模型，拿不到的显示"未知"。 */
function Scope({ r }: { r: Report }) {
  return (
    <p className={css.meta}>
      Runner {r.RunnerVersion || unknownText} · Agent 版本 {r.AgentVersion || unknownText} · 模型 {r.Model || unknownText} ·{' '}
      <a href={r.TaskURL}>AI 任务 #{r.TaskID}</a> · <Ago t={r.Created} />
    </p>
  )
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
  return (
    <>
      <p className={css.p}>选中：{s.selected.length > 0 ? s.selected.map((n) => `#${n}`).join('、') : '没有选'}</p>
      {s.design_first != null && (
        <p className={css.p}>方式：{s.design_first ? '先交付设计 PR，合并后再实现' : '一个 PR 完成（先设计再开发）'}</p>
      )}
      {s.design_reason && <Text k="方式的理由" v={s.design_reason} />}
      <Text k="合批理由" v={s.batch_reason} />
      <Text k="计划" v={s.plan} />
      {s.skipped.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>没有选的 Issue</h4>
          <ul className={css.list}>
            {s.skipped.map((sk) => (
              <li key={sk.number}>
                #{sk.number}：{sk.reason}
                {sk.later && <span className={css.muted}>（留到之后的选题）</span>}
                {sk.wait_for && sk.wait_for.length > 0 && (
                  <span className={css.muted}>（等 {sk.wait_for.map((n) => `#${n}`).join('、')} 关闭后再选）</span>
                )}
                {sk.reconsider && <span className={css.muted}>（什么情况下再考虑：{sk.reconsider}）</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

const deliveryStatus: Record<string, string> = { delivered: '已交付', no_change: '不需要改代码', blocked: '受阻' }
const outcomes: Record<string, string> = { passed: '通过', failed: '失败', not_run: '没有运行' }
const actions: Record<string, string> = { fixed: '已修复', rejected: '不采纳', duplicate: '重复', unable: '无法处理' }

function DeliveryBody({ d }: { d: Delivery }) {
  const h = d.handoff
  return (
    <>
      <p className={css.p}>
        结论：{lookup(deliveryStatus, d.status) ?? d.status}
        {d.pr_title && ` · PR 标题：${d.pr_title}`}
      </p>
      <Text k="摘要" v={d.summary} />
      {d.issues.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>Issue 完成情况</h4>
          <ul className={css.list}>
            {d.issues.map((is) => (
              <li key={is.number}>
                #{is.number}：{resolution(is.resolution)}
                {is.acceptance.length > 0 && (
                  <ul className={css.list}>
                    {is.acceptance.map((a, i) => (
                      <li key={i}>
                        {a.done ? '已完成' : '未完成'}：{a.item}
                        {a.evidence && <span className={css.muted}>（依据：{a.evidence}）</span>}
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
          <h4 className={css.h}>验证</h4>
          <ul className={css.list}>
            {d.verification.map((v, i) => (
              <li key={i}>
                <code>{v.command}</code>：{lookup(outcomes, v.outcome) ?? v.outcome}
                {v.note && <span className={css.muted}>（{v.note}）</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {d.dispositions.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>审计问题的处置</h4>
          <ul className={css.list}>
            {d.dispositions.map((x) => (
              <li key={x.finding}>
                <code>{x.finding}</code>：{lookup(actions, x.action) ?? x.action}
                {x.commit && (
                  <>
                    {' '}
                    （<code>{short(x.commit)}</code>）
                  </>
                )}
                {x.duplicate_of && `，与 ${x.duplicate_of} 重复`}
                {x.reason && <span className={css.muted}> · {x.reason}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {d.design && <DesignBody g={d.design} />}
      {d.design_deviations && d.design_deviations.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>与已合并设计的出入</h4>
          <ul className={css.list}>
            {d.design_deviations.map((x, i) => (
              <li key={i}>
                {x.item}
                {x.reason && <span className={css.muted}>（理由：{x.reason}）</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      <Text k="已完成" v={h.done} />
      <Text k="还剩什么" v={h.remaining} />
      <Text k="做出的决定" v={h.decisions} />
      <Text k="下一步" v={h.next} />
    </>
  )
}

/** 决定的类别（devreport.Decision.Category，与 PR 描述的说法相同）。 */
const decisionCategories: Record<string, string> = {
  confirmed_decision: '改已确认的决定（需要人确认）',
  acceptance: '删减或改变验收项',
  policy: '预算、范围等取舍',
  other: '其他',
}

function DesignBody({ g }: { g: NonNullable<Delivery['design']> }) {
  return (
    <>
      {g.documents.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>设计文档</h4>
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
          <h4 className={css.h}>决策记录</h4>
          <ul className={css.list}>
            {g.decisions.map((x, i) => (
              <li key={i}>
                {lookup(decisionCategories, x.category) ?? x.category}
                {x.ref && `（${x.ref}）`}：{x.choice}
                {x.context && <p className={css.pre}>依据：{x.context}</p>}
                {x.options && <p className={css.pre}>考虑过的方案：{x.options}</p>}
                {x.impact && <p className={css.pre}>影响：{x.impact}</p>}
                {x.review && <p className={css.pre}>复盘时看：{x.review}</p>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {g.external_inputs.length > 0 && (
        <div className={css.field}>
          <h4 className={css.h}>需要人提供的条件</h4>
          <ul className={css.list}>
            {g.external_inputs.map((x, i) => (
              <li key={i}>
                {x.what}
                {x.blocks && <span className={css.muted}>（阻塞：{x.blocks}）</span>}
                {x.resume && <span className={css.muted}>（提供后：{x.resume}）</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  )
}

const reportTitles: Record<string, string> = { selection: '选题报告', delivery: '交付报告' }

export function ReportBox({ r, n }: { r: Report; n: number }) {
  const id = `ai-dev-report-${n}`
  return (
    <Box as="section" aria-labelledby={id}>
      <BoxHeader
        id={id}
        title={
          <>
            {lookup(reportTitles, r.Kind) ?? r.Kind} <AgentSays />
          </>
        }
      />
      <BoxRow className={css.block}>
        <Scope r={r} />
        {r.Selection && <SelectionBody s={r.Selection} />}
        {r.Delivery && <DeliveryBody d={r.Delivery} />}
        {r.Raw && (
          <>
            <p className={css.p}>报告的格式无法识别，下面是原文。</p>
            <pre className={css.raw}>{r.Raw}</pre>
          </>
        )}
      </BoxRow>
    </Box>
  )
}

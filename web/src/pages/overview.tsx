import { CheckCircleFillIcon, CheckCircleIcon, CircleIcon, PersonIcon, PlayIcon } from '@primer/octicons-react'
import { LinkButton, ProgressBar } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { RunRow } from '../components/RunRow'
import { runnerCountsSchema, runnerViewSchema, type RunnerCounts, type RunnerView } from '../components/RunnerSchema'
import { RunnerIcon } from '../components/RunnerStatus'
import { aiRunnerCountsSchema, type AIRunnerCounts } from './ai/schema'
import { Ago } from '../components/Time'
import { list } from '../lib/envelope'
import { runViewSchema, type RunView } from '../lib/schemas'
import css from './overview.module.css'

// 总览（首页）：现在在跑什么、最近有没有坏、Runner 够不够用；管理员的初始设置清单在最上方。列表每 5 秒自动更新。

const statsSchema = z.object({
  Total: z.number(),
  Succeeded: z.number(),
  Failed: z.number(),
  Cancelled: z.number(),
  Active: z.number(),
  /** 成功率（已结束的 run 中成功的比例），没有已结束的 run 时为空。 */
  Rate: z.string(),
})
type Stats = z.infer<typeof statsSchema>

const setupSchema = z.object({
  Title: z.string(),
  Text: z.string(),
  URL: z.string(),
  Action: z.string(),
  Done: z.boolean(),
})
type SetupItem = z.infer<typeof setupSchema>

const schema = z.object({
  /** 当前用户的 GitHub 登录名；没有绑定 GitHub 账号的 admin token 登录为空。 */
  Me: z.string(),
  Running: list(runViewSchema),
  Queued: list(runViewSchema),
  Failures: list(runViewSchema),
  Mine: list(runViewSchema),
  Stats: statsSchema,
  Runners: list(runnerViewSchema),
  RunnerCounts: runnerCountsSchema,
  /** AI 执行节点的统计（AI 执行节点不在 Runners 中，见 AI 执行节点页面）。 */
  AIRunnerCounts: aiRunnerCountsSchema,
  /** 没有列出的 Runner 数。 */
  MoreRunners: z.number(),
  /** 还没完成的初始设置（只给管理员看，全部完成后为空）。 */
  Setup: list(setupSchema),
  SetupDone: z.number(),
  MineURL: z.string(),
})
type Data = z.infer<typeof schema>

function Setup({ items, done }: { items: SetupItem[]; done: number }) {
  return (
    <Box as="section" aria-labelledby="ov-setup-h" className={css.setup}>
      <BoxHeader title="完成初始设置" id="ov-setup-h">
        <span className={css.muted}>
          已完成 {done} / {items.length}
        </span>
      </BoxHeader>
      <ProgressBar progress={(done * 100) / items.length} barSize="small" aria-label={`已完成 ${done} / ${items.length}`} className={css.setupBar} />
      <ol className={css.steps}>
        {items.map((it) => (
          <li key={it.Title} className={it.Done ? css.stepDone : undefined}>
            {it.Done ? (
              <CheckCircleFillIcon className={css.okIcon} aria-label="已完成" />
            ) : (
              <CircleIcon className={css.todoIcon} aria-label="未完成" />
            )}
            <div className={css.stepText}>
              <b>{it.Title}</b>
              {!it.Done && <p>{it.Text}</p>}
            </div>
            {!it.Done && (
              <LinkButton size="small" href={it.URL}>
                {it.Action}
              </LinkButton>
            )}
          </li>
        ))}
      </ol>
    </Box>
  )
}

/** RunSection 是主栏中的一组 run：标题行右侧是"查看全部"链接，没有 run 时显示 empty。 */
function RunSection({
  id,
  title,
  more,
  runs,
  empty,
}: {
  id: string
  title: string
  more: { label: string; href: string }
  runs: RunView[]
  empty: ReactNode
}) {
  return (
    <Box as="section" aria-labelledby={id}>
      <BoxHeader title={title} id={id}>
        <a href={more.href} className={css.more}>
          {more.label}
        </a>
      </BoxHeader>
      {runs.length > 0 ? (
        <div role="list" aria-label={title}>
          {runs.map((r) => (
            <RunRow key={r.ID} run={r} />
          ))}
        </div>
      ) : (
        empty
      )}
    </Box>
  )
}

const segments = [
  { key: 'Succeeded', label: '成功', state: 'succeeded', bg: 'success' },
  { key: 'Failed', label: '失败', state: 'failed', bg: 'danger' },
  { key: 'Cancelled', label: '取消', state: 'cancelled', bg: 'neutral' },
  { key: 'Active', label: '进行中', state: 'active', bg: 'attention' },
] as const

/** Day 是右侧栏的"过去 24 小时"：成功率和各结果的比例（像 GitHub 仓库页的 Languages）。 */
function Day({ s }: { s: Stats }) {
  return (
    <section className={css.side} aria-labelledby="ov-day-h">
      <h2 id="ov-day-h" className={css.sideTitle}>
        过去 24 小时
      </h2>
      {s.Total > 0 ? (
        <>
          <p className={css.rate}>
            成功率 <b className="num">{s.Rate || '—'}</b> · 共 <span className="num">{s.Total}</span> 个构建
          </p>
          <ProgressBar
            className={css.bar}
            aria-label={`成功 ${s.Succeeded}，失败 ${s.Failed}，取消 ${s.Cancelled}，进行中 ${s.Active}`}
          >
            {segments
              .filter((g) => s[g.key] > 0)
              .map((g) => (
                <ProgressBar.Item key={g.key} progress={(s[g.key] * 100) / s.Total} bg={`${g.bg}.emphasis`} aria-label={`${g.label} ${s[g.key]}`} />
              ))}
          </ProgressBar>
          <ul className={css.legend}>
            {segments.map((g) => (
              <li key={g.key}>
                <a href={`/runs?state=${g.state}`}>
                  <span className={css.dot} style={{ backgroundColor: `var(--bgColor-${g.bg}-emphasis)` }} aria-hidden />
                  <span className={css.legendLabel}>{g.label}</span> <span className="num">{s[g.key]}</span>
                </a>
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p className={css.muted}>过去 24 小时没有构建。</p>
      )}
    </section>
  )
}

function runnerLine(r: RunnerView): ReactNode {
  if (!r.Online) {
    return (
      <>
        离线
        {r.LastSeen && (
          <>
            ，<Ago t={r.LastSeen} />
            联系过
          </>
        )}
      </>
    )
  }
  if (r.Jobs.length > 0) {
    return r.Jobs.map((j, i) => (
      <span key={j.JobID}>
        {i > 0 && '、'}
        <a href={`/jobs/${j.JobID}`}>
          {j.Repo}：{j.Name}
        </a>
      </span>
    ))
  }
  return r.Drain ? '已暂停接单' : '空闲，等待任务'
}

/** AIRunners 是 AI 执行节点的一行统计（没有 AI 执行节点时不显示）。 */
function AIRunners({ counts: c }: { counts: AIRunnerCounts }) {
  if (c.Total === 0) return null
  const parts = [`${c.Online} 在线`, c.Offline > 0 && `${c.Offline} 离线`, c.Tasks > 0 && `${c.Tasks} 个 AI 任务`].filter(Boolean).join('，')
  return (
    <p className={css.muted}>
      <a href="/ai/runners">AI 执行节点</a>：{parts}
    </p>
  )
}

function Runners({
  runners,
  counts,
  aiCounts,
  more,
  admin,
}: {
  runners: RunnerView[]
  counts: RunnerCounts
  aiCounts: AIRunnerCounts
  more: number
  admin: boolean
}) {
  const summary = [`${counts.Online} 在线`, counts.Offline > 0 && `${counts.Offline} 离线`, counts.Jobs > 0 && `${counts.Jobs} 个任务`]
    .filter(Boolean)
    .join('，')
  return (
    <section className={css.side} aria-labelledby="ov-runners-h">
      <h2 id="ov-runners-h" className={css.sideTitle}>
        <a href="/runners">Runner</a>
        <span className={css.sideCount}>{summary}</span>
      </h2>
      {runners.length > 0 ? (
        <ul className={css.runners}>
          {runners.map((r) => (
            <li key={r.ID}>
              <span className={css.runnerIcon}>
                <RunnerIcon runner={r} running={r.Jobs.length} />
              </span>
              <div className={css.runnerText}>
                <a href={`/runners#r-${r.ID}`} className={css.runnerName}>
                  {r.Name}
                </a>{' '}
                <span className={css.muted}>{r.Pool}</span>
                <p>{runnerLine(r)}</p>
              </div>
            </li>
          ))}
          {more > 0 && (
            <li>
              <a href="/runners">还有 {more} 台</a>
            </li>
          )}
        </ul>
      ) : (
        <p className={css.muted}>
          还没有 Runner。{admin ? <><a href="/runners/new">注册一台</a>后才能开始构建。</> : '请管理员注册一台。'}
        </p>
      )}
      <AIRunners counts={aiCounts} />
    </section>
  )
}

function Overview({ data, env }: PageProps<Data>) {
  useLive(true)
  const admin = env.Session?.Admin ?? false
  const active = [...data.Running, ...data.Queued]
  const lead =
    active.length > 0
      ? `现在有 ${data.Running.length} 个构建在运行${data.Queued.length > 0 ? `，${data.Queued.length} 个在排队` : ''}。`
      : '现在没有正在运行的构建。'
  return (
    <div className={css.page}>
      <PageHeader
        title="总览"
        description={`${data.Me ? `${data.Me}，` : ''}${lead}`}
        actions={
          <>
            <LinkButton href="/runs">全部运行</LinkButton>
            <LinkButton href="/runs/new" variant="primary" leadingVisual={PlayIcon}>
              启动构建
            </LinkButton>
          </>
        }
      />

      {data.Setup.length > 0 && <Setup items={data.Setup} done={data.SetupDone} />}

      <div className={css.grid}>
        <div className={css.main}>
          <RunSection
            id="ov-active-h"
            title="正在进行"
            more={{ label: '查看全部', href: '/runs?state=active' }}
            runs={active}
            empty={
              <EmptyState size="small" icon={PlayIcon} title="现在没有构建在跑">
                推送代码、开 PR 或推送 tag 后，构建会出现在这里。
              </EmptyState>
            }
          />
          {data.Me && (
            <RunSection
              id="ov-mine-h"
              title="我最近发起的"
              more={{ label: '全部', href: data.MineURL }}
              runs={data.Mine}
              empty={
                <EmptyState size="small" icon={PersonIcon} title="你最近没有发起构建">
                  你推送、开 PR 或手动启动的构建会出现在这里。
                </EmptyState>
              }
            />
          )}
          <RunSection
            id="ov-failed-h"
            title="最近失败"
            more={{ label: '全部失败', href: '/runs?state=failed' }}
            runs={data.Failures}
            empty={<EmptyState size="small" icon={CheckCircleIcon} title="最近没有失败的构建" />}
          />
        </div>
        <aside className={css.aside} aria-label="统计和 Runner">
          <Day s={data.Stats} />
          <Runners runners={data.Runners} counts={data.RunnerCounts} aiCounts={data.AIRunnerCounts} more={data.MoreRunners} admin={admin} />
        </aside>
      </div>
    </div>
  )
}

export default definePage(schema, Overview)

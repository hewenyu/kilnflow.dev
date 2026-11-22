import { CheckCircleFillIcon, CheckCircleIcon, CircleIcon, PersonIcon, PlayIcon } from '@primer/octicons-react'
import { LinkButton, ProgressBar } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Msg, Trans, useT } from '../i18n/locale'
import type { MsgKey, Params } from '../i18n/messages'
import { useLive } from '../app/live'
import { Box, BoxHeader } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { RunRow } from '../components/RunRow'
import { runnerCountsSchema, runnerViewSchema, type RunnerCounts, type RunnerView } from '../components/RunnerSchema'
import { RunnerIcon } from '../components/RunnerStatus'
import { aiRunnerCountsSchema, type AIRunnerCounts } from './ai/schema'
import { Ago } from '../components/Time'
import { list, textSchema } from '../lib/envelope'
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
  /** 稳定的标识（列表的 key，不随语言变化）。 */
  Key: z.string(),
  Title: textSchema,
  Text: textSchema,
  URL: z.string(),
  Action: textSchema,
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
  const t = useT()
  const progress = t('overview.setupProgress', { done, total: items.length })
  return (
    <Box as="section" aria-labelledby="ov-setup-h" className={css.setup}>
      <BoxHeader title={t('overview.setupTitle')} id="ov-setup-h">
        <span className={css.muted}>{progress}</span>
      </BoxHeader>
      <ProgressBar progress={(done * 100) / items.length} barSize="small" aria-label={progress} className={css.setupBar} />
      <ol className={css.steps}>
        {items.map((it) => (
          <li key={it.Key} className={it.Done ? css.stepDone : undefined}>
            {it.Done ? (
              <CheckCircleFillIcon className={css.okIcon} aria-label={t('overview.stepDone')} />
            ) : (
              <CircleIcon className={css.todoIcon} aria-label={t('overview.stepTodo')} />
            )}
            <div className={css.stepText}>
              <b>
                <Msg text={it.Title} />
              </b>
              {!it.Done && (
                <p>
                  <Msg text={it.Text} />
                </p>
              )}
            </div>
            {!it.Done && (
              <LinkButton size="small" href={it.URL}>
                <Msg text={it.Action} />
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
  { key: 'Succeeded', label: 'overview.statSucceeded', state: 'succeeded', bg: 'success' },
  { key: 'Failed', label: 'overview.statFailed', state: 'failed', bg: 'danger' },
  { key: 'Cancelled', label: 'overview.statCancelled', state: 'cancelled', bg: 'neutral' },
  { key: 'Active', label: 'overview.statActive', state: 'active', bg: 'attention' },
] as const satisfies readonly { key: keyof Stats; label: MsgKey; state: string; bg: string }[]

/** Day 是右侧栏的"过去 24 小时"：成功率和各结果的比例（像 GitHub 仓库页的 Languages）。 */
function Day({ s }: { s: Stats }) {
  const t = useT()
  return (
    <section className={css.side} aria-labelledby="ov-day-h">
      <h2 id="ov-day-h" className={css.sideTitle}>
        {t('overview.day')}
      </h2>
      {s.Total > 0 ? (
        <>
          <p className={css.rate}>
            <Trans
              id="overview.rate"
              params={{ rate: s.Rate || '—', count: s.Total }}
              tags={{ b: (c) => <b className="num">{c}</b>, num: (c) => <span className="num">{c}</span> }}
            />
          </p>
          <ProgressBar
            className={css.bar}
            aria-label={t('overview.dayBar', { succeeded: s.Succeeded, failed: s.Failed, cancelled: s.Cancelled, active: s.Active })}
          >
            {segments
              .filter((g) => s[g.key] > 0)
              .map((g) => (
                <ProgressBar.Item
                  key={g.key}
                  progress={(s[g.key] * 100) / s.Total}
                  bg={`${g.bg}.emphasis`}
                  aria-label={t('overview.statItem', { label: t(g.label), count: s[g.key] })}
                />
              ))}
          </ProgressBar>
          <ul className={css.legend}>
            {segments.map((g) => (
              <li key={g.key}>
                <a href={`/runs?state=${g.state}`}>
                  <span className={css.dot} style={{ backgroundColor: `var(--bgColor-${g.bg}-emphasis)` }} aria-hidden />
                  <span className={css.legendLabel}>{t(g.label)}</span> <span className="num">{s[g.key]}</span>
                </a>
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p className={css.muted}>{t('overview.dayEmpty')}</p>
      )}
    </section>
  )
}

function RunnerLine({ runner: r }: { runner: RunnerView }) {
  const t = useT()
  if (!r.Online) {
    if (!r.LastSeen) return <>{t('overview.runnerOffline')}</>
    return <Trans id="overview.runnerOfflineSeen" tags={{ ago: () => <Ago t={r.LastSeen} /> }} />
  }
  if (r.Jobs.length > 0) {
    return r.Jobs.map((j, i) => (
      <span key={j.JobID}>
        {i > 0 && t('list.enum')}
        <a href={`/jobs/${j.JobID}`}>{t('overview.runnerJob', { repo: j.Repo, name: j.Name })}</a>
      </span>
    ))
  }
  return <>{r.Drain ? t('overview.runnerPaused') : t('overview.runnerIdle')}</>
}

/** counts 返回在线、离线和任务数的说明（为 0 的离线数和任务数不写），例如"2 在线，1 离线，3 个任务"。 */
function counts(t: (id: MsgKey, params?: Params) => string, online: number, offline: number, tasks: number, tasksKey: MsgKey): string {
  return [
    t('overview.countOnline', { count: online }),
    offline > 0 && t('overview.countOffline', { count: offline }),
    tasks > 0 && t(tasksKey, { count: tasks }),
  ]
    .filter(Boolean)
    .join(t('list.separator'))
}

/** AIRunners 是 AI 执行节点的一行统计（没有 AI 执行节点时不显示）。 */
function AIRunners({ counts: c }: { counts: AIRunnerCounts }) {
  const t = useT()
  if (c.Total === 0) return null
  return (
    <p className={css.muted}>
      <a href="/ai/runners">{t('nav.aiRunners')}</a>
      {t('list.colon')}
      {counts(t, c.Online, c.Offline, c.Tasks, 'overview.countAITasks')}
    </p>
  )
}

function Runners({
  runners,
  counts: c,
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
  const t = useT()
  return (
    <section className={css.side} aria-labelledby="ov-runners-h">
      <h2 id="ov-runners-h" className={css.sideTitle}>
        <a href="/runners">{t('nav.runners')}</a>
        <span className={css.sideCount}>{counts(t, c.Online, c.Offline, c.Jobs, 'overview.countJobs')}</span>
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
                <p>
                  <RunnerLine runner={r} />
                </p>
              </div>
            </li>
          ))}
          {more > 0 && (
            <li>
              <a href="/runners">{t('overview.moreRunners', { count: more })}</a>
            </li>
          )}
        </ul>
      ) : (
        <p className={css.muted}>
          {admin ? (
            <Trans id="overview.noRunnersAdmin" tags={{ link: (x) => <a href="/runners/new">{x}</a> }} />
          ) : (
            t('overview.noRunners')
          )}
        </p>
      )}
      <AIRunners counts={aiCounts} />
    </section>
  )
}

/** lead 返回页面说明：现在有多少构建在运行、在排队。 */
function lead(t: (id: MsgKey, params?: Params) => string, data: Data): string {
  const running = data.Running.length
  const queued = data.Queued.length
  const s =
    running + queued === 0
      ? t('overview.leadIdle')
      : queued > 0
        ? t('overview.leadQueued', { count: running, queued })
        : t('overview.leadRunning', { count: running })
  return data.Me ? t('overview.greeting', { login: data.Me, lead: s }) : s
}

function Overview({ data, env }: PageProps<Data>) {
  useLive(true)
  const t = useT()
  const admin = env.Session?.Admin ?? false
  const active = [...data.Running, ...data.Queued]
  return (
    <div className={css.page}>
      <PageHeader
        title={t('nav.overview')}
        description={lead(t, data)}
        actions={
          <>
            <LinkButton href="/runs">{t('overview.allRuns')}</LinkButton>
            <LinkButton href="/runs/new" variant="primary" leadingVisual={PlayIcon}>
              {t('runNew.pageTitle')}
            </LinkButton>
          </>
        }
      />

      {data.Setup.length > 0 && <Setup items={data.Setup} done={data.SetupDone} />}

      <div className={css.grid}>
        <div className={css.main}>
          <RunSection
            id="ov-active-h"
            title={t('overview.active')}
            more={{ label: t('overview.viewAll'), href: '/runs?state=active' }}
            runs={active}
            empty={
              <EmptyState size="small" icon={PlayIcon} title={t('overview.activeEmpty')}>
                {t('overview.activeEmptyText')}
              </EmptyState>
            }
          />
          {data.Me && (
            <RunSection
              id="ov-mine-h"
              title={t('overview.mine')}
              more={{ label: t('overview.mineAll'), href: data.MineURL }}
              runs={data.Mine}
              empty={
                <EmptyState size="small" icon={PersonIcon} title={t('overview.mineEmpty')}>
                  {t('overview.mineEmptyText')}
                </EmptyState>
              }
            />
          )}
          <RunSection
            id="ov-failed-h"
            title={t('overview.failures')}
            more={{ label: t('overview.failuresAll'), href: '/runs?state=failed' }}
            runs={data.Failures}
            empty={<EmptyState size="small" icon={CheckCircleIcon} title={t('overview.failuresEmpty')} />}
          />
        </div>
        <aside className={css.aside} aria-label={t('overview.aside')}>
          <Day s={data.Stats} />
          <Runners runners={data.Runners} counts={data.RunnerCounts} aiCounts={data.AIRunnerCounts} more={data.MoreRunners} admin={admin} />
        </aside>
      </div>
    </div>
  )
}

export default definePage(schema, Overview)

import { CpuIcon, PlusIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { RunnerIcon, RunnerLabels } from '../components/RunnerStatus'
import { Trans, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import { AIRunnerFacts, AIRunnerOps, AIWork, RoleLabel } from './ai/runner'
import { aiRunnerCountsSchema, aiRunnerSchema, aiRunnerURL, type AIRunner } from './ai/schema'
import css from './ai/ai.module.css'
import runnersCss from './runners.module.css'

// AI 执行节点列表（PLAN #30.9、PLAN #33）：AI 执行节点（执行机器上的 Orca Server + kiln-orca 连接器）与 CI Runner 分开列出。
// 连接和接单状态与 CI Runner 相同；另外显示角色、协议、并发、Orca / Docker、本机 Agent 和正在执行的 AI 任务及其在 Orca 中的
// 关联。列表每 5 秒自动更新。

const schema = z.object({
  Runners: list(aiRunnerSchema),
  Counts: aiRunnerCountsSchema,
  Self: z.string(),
  /** 页面说明中用到的时长（秒）。 */
  OfflineAfter: z.number(),
  LeaseTTL: z.number(),
  LongPoll: z.number(),
})
type Data = z.infer<typeof schema>

function Row({ runner: r, data, admin }: { runner: AIRunner; data: Data; admin: boolean }) {
  return (
    <BoxRow as="li" id={`r-${r.ID}`} className={`${runnersCss.row} ${r.Revoked ? runnersCss.revoked : ''}`}>
      <span className={runnersCss.icon}>
        <RunnerIcon runner={r} running={r.Tasks.length} />
      </span>
      <div className={runnersCss.main}>
        <div className={runnersCss.head}>
          <h3 className={runnersCss.name}>
            <a href={aiRunnerURL(r.Name)}>{r.Name}</a>
          </h3>
          <RoleLabel runner={r} />
          <RunnerLabels runner={r} />
        </div>
        <AIRunnerFacts runner={r} />
        <AIWork runner={r} />
      </div>
      {admin && (
        <div className={runnersCss.ops}>
          <AIRunnerOps runner={r} longPoll={data.LongPoll} leaseTTL={data.LeaseTTL} />
        </div>
      )}
    </BoxRow>
  )
}

function AIRunners({ data, env }: PageProps<Data>) {
  useLive(true)
  const t = useT()
  const admin = env.Session?.Admin ?? false
  const c = data.Counts
  return (
    <div className={css.page}>
      <PageHeader
        title={t('aiRunners.title')}
        description={
          <Trans id="aiRunners.description" params={{ offline: data.OfflineAfter }} tags={{ link: (s) => <a href="/runners">{s}</a> }} />
        }
        actions={
          admin && (
            <LinkButton href="/runners/new" variant="primary" leadingVisual={PlusIcon}>
              {t('runners.register')}
            </LinkButton>
          )
        }
      />
      {c.Total > 0 && c.Accepting === 0 && (
        <Callout tone="warning" title={t('aiRunners.noAcceptingTitle')}>
          {t(c.Paused > 0 ? 'aiRunners.noAcceptingPaused' : 'aiRunners.noAccepting')}
        </Callout>
      )}
      <Box>
        <h2 className="visually-hidden">{t('aiRunners.listTitle')}</h2>
        {c.Total > 0 && (
          <BoxRow className={css.muted}>
            {t('aiRunners.counts', {
              count: c.Total,
              online: c.Online,
              accepting: c.Accepting,
              paused: c.Paused,
              offline: c.Offline,
              revoked: c.Revoked,
              tasks: c.Tasks,
            })}
          </BoxRow>
        )}
        {data.Runners.length > 0 ? (
          <ul className={runnersCss.rows} aria-label={t('aiRunners.title')}>
            {data.Runners.map((r) => (
              <Row key={r.ID} runner={r} data={data} admin={admin} />
            ))}
          </ul>
        ) : (
          <EmptyState icon={CpuIcon} title={t('aiRunners.emptyTitle')}>
            {t('aiRunners.emptyText')}
            {admin ? '' : t('runners.needAdmin')}
          </EmptyState>
        )}
      </Box>
    </div>
  )
}

export default definePage(schema, AIRunners)

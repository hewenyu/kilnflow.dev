import { ArrowLeftIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { PageHeader } from '../components/PageHeader'
import { runnerKindLabel } from '../components/RunnerSchema'
import { RunnerLabels } from '../components/RunnerStatus'
import { Trans, useT } from '../i18n/locale'
import { AIRunnerFacts, AIWork, RoleLabel } from './ai/runner'
import { aiRunnerSchema } from './ai/schema'
import css from './ai/ai.module.css'

// AI 执行节点详情页（PLAN #30.9、PLAN #33）：一个节点的状态、Orca 和本机 Agent、并发、正在执行的 AI 任务及其在 Orca 中的关联。
// AI 执行节点没有按台升级：连接器在节点上用 npm 升级（PLAN #33.3）。

const schema = z.object({
  Runner: aiRunnerSchema,
  /** 页面说明中用到的时长（秒）。 */
  OfflineAfter: z.number(),
  LongPoll: z.number(),
})
type Data = z.infer<typeof schema>

function AIRunnerPage({ data }: PageProps<Data>) {
  const t = useT()
  const r = data.Runner
  useLive(r.Tasks.length > 0)
  return (
    <div className={css.page}>
      <PageHeader
        title={r.Name}
        description={runnerKindLabel('ai', r.Role, t)}
        actions={
          <LinkButton href="/ai/runners" leadingVisual={ArrowLeftIcon}>
            {t('aiRunners.backToList')}
          </LinkButton>
        }
      />
      <Box as="section" aria-labelledby="summary-title">
        <BoxHeader title={t('runner.summary')} id="summary-title">
          <span className={css.labels}>
            <RoleLabel runner={r} />
            <RunnerLabels runner={r} />
          </span>
        </BoxHeader>
        <BoxRow className={css.block}>
          <AIRunnerFacts runner={r} />
          <AIWork runner={r} />
          <p className={`${css.p} ${css.muted}`}>
            <Trans
              id="aiRunner.opsHint"
              params={{ offline: data.OfflineAfter, command: 'npm install -g @yuebanlaosiji/kilnflow-orca@latest && kiln-orca service restart' }}
              tags={{ link: (c) => <a href="/ai/runners">{c}</a>, code: (c) => <code>{c}</code> }}
            />
          </p>
        </BoxRow>
      </Box>
    </div>
  )
}

export default definePage(schema, AIRunnerPage)

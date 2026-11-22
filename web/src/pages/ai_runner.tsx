import { ArrowLeftIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { PageHeader } from '../components/PageHeader'
import { RunnerLabels } from '../components/RunnerStatus'
import { AIRunnerFacts, AIWork, RoleLabel } from './ai/runner'
import { aiRunnerSchema } from './ai/schema'
import css from './ai/ai.module.css'

// AI 执行节点详情页（PLAN #30.9、PLAN #33）：一个节点的状态、Orca 和本机 Agent、并发、正在执行的 AI 任务及其在 Orca 中的关联。
// AI 执行节点没有按台升级：连接器在节点上用 npm 升级（PLAN #33.3）。

const schema = z.object({
  Runner: aiRunnerSchema,
  OfflineAfter: z.string(),
  LongPoll: z.string(),
})
type Data = z.infer<typeof schema>

function AIRunnerPage({ data }: PageProps<Data>) {
  const r = data.Runner
  useLive(r.Tasks.length > 0)
  return (
    <div className={css.page}>
      <PageHeader
        title={r.Name}
        description={r.RoleLabel}
        actions={
          <LinkButton href="/ai/runners" leadingVisual={ArrowLeftIcon}>
            AI 执行节点列表
          </LinkButton>
        }
      />
      <Box as="section" aria-labelledby="summary-title">
        <BoxHeader title="概况" id="summary-title">
          <span className={css.labels}>
            <RoleLabel runner={r} />
            <RunnerLabels runner={r} />
          </span>
        </BoxHeader>
        <BoxRow className={css.block}>
          <AIRunnerFacts runner={r} />
          <AIWork runner={r} />
          <p className={`${css.p} ${css.muted}`}>
            暂停接单、吊销、删除在 <a href="/ai/runners">AI 执行节点列表</a> 中操作；{data.OfflineAfter}没有联系 Server 时显示为离线。
            升级连接器在节点上执行 <code>npm install -g @yuebanlaosiji/kilnflow-orca@latest &amp;&amp; kiln-orca service restart</code>
            ，Orca 中正在执行的任务不受影响。
          </p>
        </BoxRow>
      </Box>
    </div>
  )
}

export default definePage(schema, AIRunnerPage)

import { ArrowLeftIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { PageHeader } from '../components/PageHeader'
import { StatusIcon } from '../components/StatusIcon'
import { Ago } from '../components/Time'
import { Trans, useLocale, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import { CancelRequest, RequestFacts, requestState, requestTitle, RerunRequest, TasksTable } from './ai/parts'
import { RequestDesignBasis } from './ai/DesignBasis'
import { requestDocsSchema } from './ai/docs'
import { aiRequestSchema, aiRequestURL, aiTaskSchema } from './ai/schema'
import css from './ai/ai.module.css'

// AI 请求详情页（PLAN #30.9）：请求的信息、各平台的 AI 任务、同一个请求族（首次请求和它的重跑）；管理员可以取消进行中的请求，
// 重跑已结束的审计请求（重跑新建请求）。请求没有结束时每 5 秒刷新。

const schema = z.object({
  Request: aiRequestSchema,
  Tasks: list(aiTaskSchema),
  /** 同一个请求族中的全部请求（按 ID 排序），只有一个时为空。 */
  Family: list(z.object({ ID: z.number(), State: z.string(), Created: z.string() })),
  Terminal: z.boolean(),
  /** 可以重跑（已结束的审计请求）；HasFailed 表示有没有成功的任务。 */
  CanRerun: z.boolean(),
  HasFailed: z.boolean(),
  /** PR 在 GitHub 上的地址（没有时为空）。 */
  PRURL: z.string(),
  /** 这个请求固定的设计依据；历史页面的数据没有这一项时为 null。 */
  DesignBasis: requestDocsSchema.nullable().default(null),
})
type Data = z.infer<typeof schema>

function Family({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  if (data.Family.length === 0) return null
  return (
    <Box as="section" aria-labelledby="family-h">
      <BoxHeader title={t('aiRequest.family')} id="family-h" />
      <BoxRow className={css.block}>
        <ul className={css.nav}>
          {data.Family.map((f) => (
            <li key={f.ID} aria-current={f.ID === data.Request.ID ? 'page' : undefined}>
              <StatusIcon state={f.State} size={14} label={requestState(f.State, locale)} />
              {f.ID === data.Request.ID ? <span>#{f.ID}</span> : <a href={aiRequestURL(f.ID)}>#{f.ID}</a>}
              <span className={css.muted}>
                <Trans id="aiRequest.familyMeta" params={{ state: requestState(f.State, locale) }} tags={{ ago: () => <Ago t={f.Created} /> }} />
              </span>
            </li>
          ))}
        </ul>
      </BoxRow>
    </Box>
  )
}

function AIRequestPage({ data }: PageProps<Data>) {
  const t = useT()
  const { locale } = useLocale()
  const r = data.Request
  useLive(!data.Terminal)
  return (
    <div className={css.page}>
      <PageHeader
        title={requestTitle(r, locale)}
        description={t(r.Type === 'develop' ? 'aiRequest.descDevelop' : 'aiRequest.descReview', { repo: r.Repo })}
        actions={
          <>
            <LinkButton href="/ai/requests" leadingVisual={ArrowLeftIcon}>
              {t('aiRequests.pageTitle')}
            </LinkButton>
            {!data.Terminal && <CancelRequest r={r} />}
            {data.CanRerun && <RerunRequest r={r} hasFailed={data.HasFailed} />}
          </>
        }
      />
      {r.ConfigError && (
        <Callout tone="critical" title={t('aiRequest.configError')}>
          <pre className={css.pre}>{r.ConfigError}</pre>
        </Callout>
      )}
      {r.Warnings && (
        <Callout tone="warning" title={t('aiRequest.configWarnings')}>
          <pre className={css.pre}>{r.Warnings}</pre>
        </Callout>
      )}
      <Box as="section" aria-labelledby="summary-h">
        <BoxHeader title={t('run.summary')} id="summary-h" />
        <BoxRow className={css.block}>
          <RequestFacts r={r} prURL={data.PRURL} />
        </BoxRow>
      </Box>
      {data.DesignBasis && <RequestDesignBasis view={data.DesignBasis} />}
      <Box as="section" aria-labelledby="tasks-h">
        <BoxHeader title={t('aiTasks.label')} id="tasks-h" />
        {data.Tasks.length > 0 ? (
          <TasksTable tasks={data.Tasks} />
        ) : (
          <BoxRow className={css.muted}>{t('aiRequest.noTasks')}</BoxRow>
        )}
      </Box>
      <Family data={data} />
    </div>
  )
}

export default definePage(schema, AIRequestPage)

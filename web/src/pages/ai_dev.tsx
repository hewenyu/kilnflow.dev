import { ArrowLeftIcon, CopilotIcon, PauseIcon, PlayIcon, SyncIcon } from '@primer/octicons-react'
import { CounterLabel, FormControl, Label, LinkButton, Select } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import { Ago } from '../components/Time'
import { Trans, useLocale, useT } from '../i18n/locale'
import {
  aiDevItemSchema,
  fixRounds,
  occupancySchema,
  phaseText,
  poolSchema,
  poolStatus,
  resolution,
  retryText,
  workItemPhase,
  workItemState,
  type AIDevItem,
  type Occupancy,
  type PoolIssue,
} from '../lib/aidev'
import { list } from '../lib/envelope'
import css from './ai_dev.module.css'

// AI 开发（PLAN #26.15）：工作项列表（新的在前）、暂停了新领取的仓库，只看一个仓库时还有它的任务池。管理员可以
// 从 GitHub 全量同步任务池、暂停 / 恢复仓库的新领取、让受阻的工作项再返修一轮、终止工作项。

const schema = z.object({
  /** 只看的仓库；为空表示全部仓库。 */
  Repo: z.string(),
  Items: list(aiDevItemSchema),
  /** 暂停了新领取的仓库。 */
  Paused: list(z.string()),
  /** 只看的仓库的任务池；看全部仓库时为 null。 */
  Pool: poolSchema.nullable(),
  /** 只看的仓库的占用（同一个仓库按工作项串行）；看全部仓库时为 null。 */
  Occupancy: occupancySchema.nullable(),
  /** 表单的仓库选项（只有管理员有）。 */
  Repos: list(z.string()),
  /** 授权 Issue 进入开发候选的 label。 */
  Ready: z.string(),
  /** 交付需要人确认时 PR 加的 label。 */
  HoldLabel: z.string(),
  GitHub: z.string(),
})
type Data = z.infer<typeof schema>

/** repoURL 返回只看仓库 repo 的地址。 */
const repoURL = (repo: string) => `/ai-dev?${new URLSearchParams({ repo }).toString()}`

export function PRLink({ github, repo, pr }: { github: string; repo: string; pr: number }) {
  if (pr === 0) return null
  if (!github) return <b>PR #{pr}</b>
  return (
    <a href={`${github}/${repo}/pull/${pr}`} rel="noreferrer" className={css.strong}>
      PR #{pr}
    </a>
  )
}

/** StopForm 是终止工作项的确认表单。 */
export function StopForm({ item, ready }: { item: AIDevItem; ready: string }) {
  const t = useT()
  return (
    <ConfirmForm
      action="/ai-dev/stop"
      fields={{ id: String(item.ID) }}
      button={t('aiDev.stopButton')}
      buttonProps={{ size: 'small' }}
      title={t('aiDev.stopTitle', { id: item.ID })}
      confirmLabel={t('aiDev.stopConfirm')}
      cancelLabel={t('run.cancelBack')}
    >
      <ul className={css.list}>
        <li>{t('aiDev.stopNote1')}</li>
        <li>{t('aiDev.stopNote2', { ready })}</li>
        <li>{t('aiDev.stopNote3')}</li>
      </ul>
    </ConfirmForm>
  )
}

/** RetryForm 是让受阻的工作项继续的表单（再返修一轮，或者部分完成后开始下一轮实现；不受返修轮数限制）。 */
export function RetryForm({ item }: { item: AIDevItem }) {
  const t = useT()
  const { locale } = useLocale()
  return (
    <Form action="/ai-dev/retry" aria-label={t('aiDev.retryLabel', { id: item.ID })}>
      <input type="hidden" name="id" value={item.ID} />
      <SubmitButton size="small">{retryText(item, locale)}</SubmitButton>
    </Form>
  )
}

function ItemRow({ data, it, admin }: { data: Data; it: AIDevItem; admin: boolean }) {
  const t = useT()
  const { locale } = useLocale()
  const st = workItemState(it.State, locale)
  const rounds = fixRounds(it, locale)
  const phase = workItemPhase(it, locale)
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <a href={it.URL} className={css.strong}>
            {t('workItem.ref', { id: it.ID })}
          </a>
          {phase && <Label variant="secondary">{phase}</Label>}
          <Label variant={st.variant}>{st.text}</Label>
          {it.Hold && <Label variant="attention">{data.HoldLabel}</Label>}
          {it.RequirementsChanged && <Label variant="attention">{t('aiDev.requirementsChanged')}</Label>}
          <PRLink github={data.GitHub} repo={it.Repo} pr={it.PR} />
        </div>
        {it.Issues.length > 0 && (
          <ul className={css.issues} aria-label={t('aiDev.issuesLabel', { id: it.ID })}>
            {it.Issues.map((is) => (
              <li key={is.Number}>
                #{is.Number} {is.Title}
                {is.Resolution && <span className={css.muted}>{t('devReport.paren', { text: resolution(is.Resolution, locale) })}</span>}
                {is.Closed && <span className={css.muted}>{t('aiDev.closed')}</span>}
              </li>
            ))}
          </ul>
        )}
        <p className={css.meta}>
          {it.Reason && <span className={it.State === 'blocked' ? css.reason : undefined}>{it.Reason} · </span>}
          {!data.Repo && (
            <>
              <a href={repoURL(it.Repo)}>{it.Repo}</a> ·{' '}
            </>
          )}
          {rounds && `${rounds} · `}
          {it.Agent && `${it.Agent} · `}
          <Trans id="aiDev.updated" tags={{ ago: () => <Ago t={it.Updated} /> }} />
        </p>
      </div>
      {admin && (it.Retryable || it.Active) && (
        <div className={css.actions}>
          {it.Retryable && <RetryForm item={it} />}
          {it.Active && <StopForm item={it} ready={data.Ready} />}
        </div>
      )}
    </BoxRow>
  )
}

/** PauseForm 是暂停仓库新领取的确认表单（可以恢复）。 */
function PauseForm({ repo }: { repo: string }) {
  const t = useT()
  return (
    <ConfirmForm
      action="/ai-dev/pause"
      fields={{ repo }}
      button={t('aiDev.pauseButton')}
      buttonProps={{ leadingVisual: PauseIcon }}
      danger={false}
      title={t('aiDev.pauseTitle', { repo })}
      confirmLabel={t('aiDev.pauseButton')}
      cancelLabel={t('run.cancelBack')}
    >
      <ul className={css.list}>
        <li>{t('aiDev.pauseNote1')}</li>
        <li>{t('aiDev.pauseNote2')}</li>
      </ul>
    </ConfirmForm>
  )
}

function ResumeForm({ repo }: { repo: string }) {
  const t = useT()
  return (
    <Form action="/ai-dev/resume" aria-label={t('aiDev.resumeLabel', { repo })}>
      <input type="hidden" name="repo" value={repo} />
      <SubmitButton size="small" leadingVisual={PlayIcon}>
        {t('aiDev.resumeButton')}
      </SubmitButton>
    </Form>
  )
}

/** SyncForm 是管理员从 GitHub 全量同步任务池的表单（webhook 可能丢失）：只看一个仓库时就是这个仓库，否则选择仓库。 */
function SyncForm({ data }: { data: Data }) {
  const t = useT()
  if (data.Repo) {
    return (
      <Form action="/ai-dev/sync" aria-label={t('aiDev.syncLabelRepo', { repo: data.Repo })}>
        <input type="hidden" name="repo" value={data.Repo} />
        <SubmitButton leadingVisual={SyncIcon}>{t('aiDev.syncButton')}</SubmitButton>
      </Form>
    )
  }
  return (
    <Form action="/ai-dev/sync" className={css.start} aria-label={t('aiDev.syncButton')}>
      <FormControl required>
        <FormControl.Label>{t('aiReq.repo')}</FormControl.Label>
        <Select name="repo" defaultValue={data.Repos[0]}>
          {data.Repos.map((r) => (
            <Select.Option key={r} value={r}>
              {r}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <SubmitButton leadingVisual={SyncIcon}>{t('aiDev.syncButton')}</SubmitButton>
    </Form>
  )
}

function PoolRow({ is }: { is: PoolIssue }) {
  const t = useT()
  const { locale } = useLocale()
  const st = poolStatus(is.Status, locale)
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          {is.URL ? (
            <a href={is.URL} rel="noreferrer" className={css.strong}>
              #{is.Number}
            </a>
          ) : (
            <b>#{is.Number}</b>
          )}
          <span>{is.Title}</span>
          <Label variant={st.variant}>{st.text}</Label>
          {is.WorkItem !== null && <a href={`/ai-dev/${is.WorkItem}`}>{t('workItem.ref', { id: is.WorkItem })}</a>}
        </div>
        <p className={css.meta}>
          {is.Reason && `${is.Reason} · `}
          <Trans id="aiDev.poolAdded" tags={{ ago: () => <Ago t={is.ReadyAt} /> }} />
        </p>
      </div>
    </BoxRow>
  )
}

/** PoolBox 是仓库的任务池：带 kiln:ready 的打开的 Issue 和各自的情况。 */
function PoolBox({ pool, ready }: { pool: NonNullable<Data['Pool']>; ready: string }) {
  const t = useT()
  const syncedAt = pool.SyncedAt
  return (
    <Box as="section" aria-labelledby="ai-dev-pool">
      <BoxHeader
        id="ai-dev-pool"
        title={
          <>
            {t('aiDev.pool')} <CounterLabel>{pool.Issues.length}</CounterLabel>
          </>
        }
      >
        <span className={css.synced}>
          {syncedAt ? <Trans id="aiDev.lastSync" tags={{ ago: () => <Ago t={syncedAt} /> }} /> : t('aiDev.neverSynced')}
        </span>
      </BoxHeader>
      {pool.Issues.length > 0 ? (
        <ul className={css.rows} aria-label={t('aiDev.pool')}>
          {pool.Issues.map((is) => (
            <PoolRow key={is.Number} is={is} />
          ))}
        </ul>
      ) : (
        <EmptyState icon={CopilotIcon} title={t('aiDev.poolEmpty')}>
          <Trans id="aiDev.poolEmptyBody" params={{ ready }} tags={{ code: (c) => <code>{c}</code> }} />
        </EmptyState>
      )}
    </Box>
  )
}

/** OccupancyBox 是仓库的占用（PLAN #26.9 I14）：同一个仓库按工作项串行，占用者合并或终止之后才开始下一项。 */
function OccupancyBox({ occ }: { occ: Occupancy }) {
  const t = useT()
  const { locale } = useLocale()
  const state = { phase: phaseText(occ.Phase, locale), state: workItemState(occ.State, locale).text }
  return (
    <Box as="section" aria-labelledby="ai-dev-occupancy">
      <BoxHeader id="ai-dev-occupancy" title={t('aiDev.occupancy')} />
      <BoxRow className={css.row}>
        <div className={css.text}>
          {occ.Occupant === 0 ? (
            <span>{t('aiDev.occupancyNone')}</span>
          ) : (
            <>
              <span>
                <Trans
                  id="aiDev.occupant"
                  params={{ id: occ.Occupant }}
                  tags={{
                    link: (c) => (
                      <a href={`/ai-dev/${occ.Occupant}`} className={css.strong}>
                        {c}
                      </a>
                    ),
                  }}
                />
                {occ.Waiting.length > 0 && t('aiDev.occupantWaiting', { list: occ.Waiting.map((id) => `#${id}`).join(' ') })}
              </span>
              <p className={css.meta}>
                {occ.Reason ? t('aiDev.occupancyStateReason', { ...state, reason: occ.Reason }) : t('aiDev.occupancyState', state)}
              </p>
            </>
          )}
        </div>
      </BoxRow>
    </Box>
  )
}

function PausedBox({ data, admin }: { data: Data; admin: boolean }) {
  const t = useT()
  return (
    <Box as="section" aria-labelledby="ai-dev-paused">
      <BoxHeader id="ai-dev-paused" title={t('aiDev.pausedRepos')} />
      <ul className={css.rows}>
        {data.Paused.map((repo) => (
          <BoxRow as="li" key={repo} className={css.row}>
            <div className={css.text}>
              {data.Repo ? repo : <a href={repoURL(repo)}>{repo}</a>}
              <p className={css.meta}>{t('aiDev.pausedNote')}</p>
            </div>
            {admin && <ResumeForm repo={repo} />}
          </BoxRow>
        ))}
      </ul>
    </Box>
  )
}

function AIDevPage({ data, env }: PageProps<Data>) {
  const t = useT()
  const admin = env.Session?.Admin ?? false
  useLive(data.Items.some((it) => it.Active))
  const paused = data.Repo !== '' && data.Paused.includes(data.Repo)
  return (
    <div className={css.page}>
      <PageHeader
        title={data.Repo ? t('aiDev.titleRepo', { repo: data.Repo }) : t('aiDev.pageTitle')}
        description={<Trans id="aiDev.description" params={{ ready: data.Ready }} tags={{ code: (c) => <code>{c}</code> }} />}
        actions={
          <>
            {admin && data.Repo && !paused && <PauseForm repo={data.Repo} />}
            {data.Repo && (
              <LinkButton href="/ai-dev" leadingVisual={ArrowLeftIcon}>
                {t('aiRequests.allRepos')}
              </LinkButton>
            )}
          </>
        }
      />

      {admin && data.Repos.length > 0 && <SyncForm data={data} />}

      {data.Paused.length > 0 && <PausedBox data={data} admin={admin} />}

      {data.Occupancy && <OccupancyBox occ={data.Occupancy} />}

      {data.Pool && <PoolBox pool={data.Pool} ready={data.Ready} />}

      {data.Items.length > 0 ? (
        <Box as="section" aria-labelledby="ai-dev-items">
          <BoxHeader
            id="ai-dev-items"
            title={
              <>
                {t('aiReq.workItem')} <CounterLabel>{data.Items.length}</CounterLabel>
              </>
            }
          />
          <ul className={css.rows} aria-label={t('aiReq.workItem')}>
            {data.Items.map((it) => (
              <ItemRow key={it.ID} data={data} it={it} admin={admin} />
            ))}
          </ul>
        </Box>
      ) : (
        <Box>
          <EmptyState icon={CopilotIcon} title={t('aiDev.itemsEmpty')}>
            <Trans
              id={admin ? 'aiDev.itemsEmptyBodyAdmin' : 'aiDev.itemsEmptyBody'}
              params={{ ready: data.Ready }}
              tags={{ code: (c) => <code>{c}</code> }}
            />
          </EmptyState>
        </Box>
      )}
    </div>
  )
}

export default definePage(schema, AIDevPage)

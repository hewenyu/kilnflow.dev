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
import {
  aiDevItemSchema,
  fixRounds,
  occupancySchema,
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
  return (
    <ConfirmForm
      action="/ai-dev/stop"
      fields={{ id: String(item.ID) }}
      button="终止"
      buttonProps={{ size: 'small' }}
      title={`终止工作项 #${item.ID}？`}
      confirmLabel="终止工作项"
      cancelLabel="返回"
    >
      <ul className={css.list}>
        <li>正在执行的开发或返修会被取消（Runner 保存 checkpoint）。</li>
        <li>Issue 被释放，内容不变就不再自动选它；重新打上 {ready} 后可以再选。</li>
        <li>PR（如果有）保持原样，由你决定关闭还是接手。终止后不能恢复。</li>
      </ul>
    </ConfirmForm>
  )
}

/** RetryForm 是让受阻的工作项继续的表单（再返修一轮，或者部分完成后开始下一轮实现；不受返修轮数限制）。 */
export function RetryForm({ item }: { item: AIDevItem }) {
  return (
    <Form action="/ai-dev/retry" aria-label={`重试工作项 #${item.ID}`}>
      <input type="hidden" name="id" value={item.ID} />
      <SubmitButton size="small">{retryText(item)}</SubmitButton>
    </Form>
  )
}

function ItemRow({ data, it, admin }: { data: Data; it: AIDevItem; admin: boolean }) {
  const st = workItemState(it.State)
  const rounds = fixRounds(it)
  const phase = workItemPhase(it)
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <a href={it.URL} className={css.strong}>
            工作项 #{it.ID}
          </a>
          {phase && <Label variant="secondary">{phase}</Label>}
          <Label variant={st.variant}>{st.text}</Label>
          {it.Hold && <Label variant="attention">{data.HoldLabel}</Label>}
          {it.RequirementsChanged && <Label variant="attention">需求已变</Label>}
          <PRLink github={data.GitHub} repo={it.Repo} pr={it.PR} />
        </div>
        {it.Issues.length > 0 && (
          <ul className={css.issues} aria-label={`工作项 #${it.ID} 的 Issue`}>
            {it.Issues.map((is) => (
              <li key={is.Number}>
                #{is.Number} {is.Title}
                {is.Resolution && <span className={css.muted}>（{resolution(is.Resolution)}）</span>}
                {is.Closed && <span className={css.muted}>（已关闭）</span>}
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
          更新于 <Ago t={it.Updated} />
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
  return (
    <ConfirmForm
      action="/ai-dev/pause"
      fields={{ repo }}
      button="暂停领取"
      buttonProps={{ leadingVisual: PauseIcon }}
      danger={false}
      title={`暂停 ${repo} 的 AI 开发？`}
      confirmLabel="暂停领取"
      cancelLabel="返回"
    >
      <ul className={css.list}>
        <li>不再领取新的 Issue；已有的工作项照常开发、返修直到合并或终止。</li>
        <li>随时可以在这里恢复。</li>
      </ul>
    </ConfirmForm>
  )
}

function ResumeForm({ repo }: { repo: string }) {
  return (
    <Form action="/ai-dev/resume" aria-label={`恢复 ${repo} 的 AI 开发`}>
      <input type="hidden" name="repo" value={repo} />
      <SubmitButton size="small" leadingVisual={PlayIcon}>
        恢复领取
      </SubmitButton>
    </Form>
  )
}

/** SyncForm 是管理员从 GitHub 全量同步任务池的表单（webhook 可能丢失）：只看一个仓库时就是这个仓库，否则选择仓库。 */
function SyncForm({ data }: { data: Data }) {
  if (data.Repo) {
    return (
      <Form action="/ai-dev/sync" aria-label={`同步 ${data.Repo} 的任务池`}>
        <input type="hidden" name="repo" value={data.Repo} />
        <SubmitButton leadingVisual={SyncIcon}>同步任务池</SubmitButton>
      </Form>
    )
  }
  return (
    <Form action="/ai-dev/sync" className={css.start} aria-label="同步任务池">
      <FormControl required>
        <FormControl.Label>仓库</FormControl.Label>
        <Select name="repo" defaultValue={data.Repos[0]}>
          {data.Repos.map((r) => (
            <Select.Option key={r} value={r}>
              {r}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <SubmitButton leadingVisual={SyncIcon}>同步任务池</SubmitButton>
    </Form>
  )
}

function PoolRow({ is }: { is: PoolIssue }) {
  const st = poolStatus(is.Status)
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
          {is.WorkItem !== null && <a href={`/ai-dev/${is.WorkItem}`}>工作项 #{is.WorkItem}</a>}
        </div>
        <p className={css.meta}>
          {is.Reason && `${is.Reason} · `}
          进入任务池 <Ago t={is.ReadyAt} />
        </p>
      </div>
    </BoxRow>
  )
}

/** PoolBox 是仓库的任务池：带 kiln:ready 的打开的 Issue 和各自的情况。 */
function PoolBox({ pool, ready }: { pool: NonNullable<Data['Pool']>; ready: string }) {
  return (
    <Box as="section" aria-labelledby="ai-dev-pool">
      <BoxHeader
        id="ai-dev-pool"
        title={
          <>
            任务池 <CounterLabel>{pool.Issues.length}</CounterLabel>
          </>
        }
      >
        <span className={css.synced}>
          {pool.SyncedAt ? (
            <>
              上次全量同步 <Ago t={pool.SyncedAt} />
            </>
          ) : (
            '还没有全量同步过，只有 webhook 送来的 Issue'
          )}
        </span>
      </BoxHeader>
      {pool.Issues.length > 0 ? (
        <ul className={css.rows} aria-label="任务池">
          {pool.Issues.map((is) => (
            <PoolRow key={is.Number} is={is} />
          ))}
        </ul>
      ) : (
        <EmptyState icon={CopilotIcon} title="任务池是空的">
          给 Issue 加上 label <code>{ready}</code> 后它会进入任务池。
        </EmptyState>
      )}
    </Box>
  )
}

/** OccupancyBox 是仓库的占用（PLAN #26.9 I14）：同一个仓库按工作项串行，占用者合并或终止之后才开始下一项。 */
function OccupancyBox({ occ }: { occ: Occupancy }) {
  return (
    <Box as="section" aria-labelledby="ai-dev-occupancy">
      <BoxHeader id="ai-dev-occupancy" title="仓库占用" />
      <BoxRow className={css.row}>
        <div className={css.text}>
          {occ.Occupant === 0 ? (
            <span>没有进行中的工作项：有空闲的 AI 开发节点时从任务池开始下一项。</span>
          ) : (
            <>
              <span>
                <a href={`/ai-dev/${occ.Occupant}`} className={css.strong}>
                  工作项 #{occ.Occupant}
                </a>{' '}
                占用这个仓库
                {occ.Waiting.length > 0 && <>，之后依次是 {occ.Waiting.map((id) => `#${id}`).join(' ')}</>}
              </span>
              <p className={css.meta}>{occ.Reason}</p>
            </>
          )}
        </div>
      </BoxRow>
    </Box>
  )
}

function PausedBox({ data, admin }: { data: Data; admin: boolean }) {
  return (
    <Box as="section" aria-labelledby="ai-dev-paused">
      <BoxHeader id="ai-dev-paused" title="暂停了新领取的仓库" />
      <ul className={css.rows}>
        {data.Paused.map((repo) => (
          <BoxRow as="li" key={repo} className={css.row}>
            <div className={css.text}>
              {data.Repo ? repo : <a href={repoURL(repo)}>{repo}</a>}
              <p className={css.meta}>不再领取新的 Issue，已有的工作项照常收尾。</p>
            </div>
            {admin && <ResumeForm repo={repo} />}
          </BoxRow>
        ))}
      </ul>
    </Box>
  )
}

function AIDevPage({ data, env }: PageProps<Data>) {
  const admin = env.Session?.Admin ?? false
  useLive(data.Items.some((it) => it.Active))
  const paused = data.Repo !== '' && data.Paused.includes(data.Repo)
  return (
    <div className={css.page}>
      <PageHeader
        title={data.Repo ? `${data.Repo} 的 AI 开发` : 'AI 开发'}
        description={
          <>
            带 <code>{data.Ready}</code> 的 Issue 进入任务池；有空闲的 AI 开发节点时从中选一批（一个 PR）开发，CI 或审计不通过、与目标分支冲突时返修。同一个仓库按工作项串行（前一个合并或终止后才开始下一个），不同仓库并行。
          </>
        }
        actions={
          <>
            {admin && data.Repo && !paused && <PauseForm repo={data.Repo} />}
            {data.Repo && (
              <LinkButton href="/ai-dev" leadingVisual={ArrowLeftIcon}>
                全部仓库
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
                工作项 <CounterLabel>{data.Items.length}</CounterLabel>
              </>
            }
          />
          <ul className={css.rows} aria-label="工作项">
            {data.Items.map((it) => (
              <ItemRow key={it.ID} data={data} it={it} admin={admin} />
            ))}
          </ul>
        </Box>
      ) : (
        <Box>
          <EmptyState icon={CopilotIcon} title="还没有工作项">
            给 Issue 加上 label <code>{data.Ready}</code>，它进入任务池，有空闲的 AI 开发节点时开始选题{admin ? '；webhook 丢失时可以在上面全量同步任务池' : ''}。
          </EmptyState>
        </Box>
      )}
    </div>
  )
}

export default definePage(schema, AIDevPage)

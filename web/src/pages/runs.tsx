import { InboxIcon, PlayIcon, SearchIcon } from '@primer/octicons-react'
import { Button, FormControl, LinkButton, Select, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Box, BoxHeader } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { Form } from '../components/Form'
import { RunRow } from '../components/RunRow'
import { useLocale, useT, useText } from '../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey, type Params } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { runState } from '../lib/format'
import { runViewSchema } from '../lib/schemas'
import css from './runs.module.css'

// 运行列表（/runs）：按仓库、状态、workflow、ref、发起人筛选；条件在地址中，可以分享。每 5 秒自动更新。

const filterSchema = z.object({
  Repo: z.string(),
  State: z.string(),
  Workflow: z.string(),
  Ref: z.string(),
  Actor: z.string(),
  /** 设置了任一筛选条件。 */
  Active: z.boolean(),
})
type Filter = z.infer<typeof filterSchema>

const schema = z.object({
  Filter: filterSchema,
  /** 与筛选中的仓库全名完全相同的仓库；为空表示按子串匹配或没有按仓库筛选。 */
  RepoExact: z.string(),
  Runs: list(runViewSchema),
  /** 状态筛选的选项：Label 是服务端给的消息。 */
  States: list(z.object({ Value: z.string(), Label: textSchema })),
  Workflows: list(z.string()),
  /** 不是最新一页；NewestID、OldestID 是本页第一个和最后一个 run。 */
  Paged: z.boolean(),
  NewestID: z.number(),
  OldestID: z.number(),
  NewerURL: z.string(),
  OlderURL: z.string(),
  LatestURL: z.string(),
  Self: z.string(),
  /** 启动构建的地址（按仓库筛选时预选仓库）。 */
  NewURL: z.string(),
  GitHub: z.string(),
  /** 当前用户的 GitHub 登录名；还没有绑定 GitHub 账号的 admin token 登录为空。 */
  Me: z.string(),
  AllURL: z.string(),
  MineURL: z.string(),
  /** 正在看"我发起的"。 */
  Mine: z.boolean(),
})
type Data = z.infer<typeof schema>

/** filterSummary 返回筛选条件的说明，例如"已筛选：仓库 tester/demo，状态 失败。"（locale 不传时为 zh-CN）。 */
export function filterSummary(f: Filter, repoExact: string, locale: Locale = defaultLocale): string {
  if (!f.Active) return ''
  const t = (id: MsgKey, params?: Params) => translate(locale, msg(id, params))
  const parts: string[] = []
  if (repoExact) parts.push(t('runs.fRepo', { repo: repoExact }))
  else if (f.Repo) parts.push(t('runs.fRepoLike', { repo: f.Repo }))
  if (f.State) parts.push(t('runs.fState', { state: runState(f.State, locale) }))
  if (f.Workflow) parts.push(t('runs.fWorkflow', { workflow: f.Workflow }))
  if (f.Ref) parts.push(t('runs.fRef', { ref: f.Ref }))
  if (f.Actor) parts.push(t('runs.fActor', { actor: f.Actor }))
  return parts.length > 0 ? t('runs.filteredBy', { parts: parts.join(t('list.separator')) }) : t('runs.filtered')
}

function FilterForm({ data }: { data: Data }) {
  const t = useT()
  const text = useText()
  const f = data.Filter
  const repoHint = data.RepoExact ? t('runs.repoOnly', { repo: data.RepoExact }) : f.Repo ? t('runs.fRepoLike', { repo: f.Repo }) : t('runs.repoHint')
  return (
    <Form action="/runs" method="get" className={css.filter} aria-label={t('runs.filterForm')}>
      <FormControl>
        <FormControl.Label>{t('runs.repo')}</FormControl.Label>
        <TextInput type="search" name="repo" defaultValue={f.Repo} placeholder={t('runs.repoPlaceholder')} spellCheck={false} autoCapitalize="off" size="small" block />
        <FormControl.Caption className="visually-hidden">{repoHint}</FormControl.Caption>
      </FormControl>
      <FormControl>
        <FormControl.Label>{t('runs.state')}</FormControl.Label>
        <Select name="state" defaultValue={f.State} size="small" block>
          <Select.Option value="">{t('runs.allStates')}</Select.Option>
          {data.States.map((s) => (
            <Select.Option key={s.Value} value={s.Value}>
              {text(s.Label)}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <FormControl>
        <FormControl.Label>{t('runs.workflow')}</FormControl.Label>
        <TextInput type="search" name="workflow" defaultValue={f.Workflow} list="runs-workflows" placeholder={t('runs.workflowPlaceholder')} spellCheck={false} autoCapitalize="off" size="small" block />
        <datalist id="runs-workflows">
          {data.Workflows.map((w) => (
            <option key={w} value={w} />
          ))}
        </datalist>
      </FormControl>
      <FormControl>
        <FormControl.Label>{t('runs.ref')}</FormControl.Label>
        <TextInput type="search" name="ref" defaultValue={f.Ref} placeholder={t('runs.refPlaceholder')} spellCheck={false} autoCapitalize="off" size="small" block />
      </FormControl>
      <FormControl>
        <FormControl.Label>{t('runs.actor')}</FormControl.Label>
        <TextInput type="search" name="actor" defaultValue={f.Actor} placeholder={t('runs.actorPlaceholder')} spellCheck={false} autoCapitalize="off" size="small" block />
      </FormControl>
      <div className={css.filterActions}>
        <Button type="submit" size="small" leadingVisual={SearchIcon}>
          {t('runs.filter')}
        </Button>
        {f.Active && (
          <LinkButton href="/runs" size="small" variant="invisible">
            {t('runs.clearFilter')}
          </LinkButton>
        )}
      </div>
    </Form>
  )
}

function Empty({ data }: { data: Data }) {
  const t = useT()
  if (data.Paged) {
    return <EmptyState icon={InboxIcon} title={t('runs.emptyPaged')} action={{ label: t('runs.backToLatest'), href: data.LatestURL }} />
  }
  if (data.Filter.Active) {
    return (
      <EmptyState
        icon={SearchIcon}
        title={t('runs.emptyFiltered')}
        action={data.RepoExact ? { label: t('runs.startFor', { repo: data.RepoExact }), href: data.NewURL } : undefined}
        secondary={{ label: t('runs.clearFilter'), href: '/runs' }}
      >
        {t('runs.emptyFilteredText')}
      </EmptyState>
    )
  }
  return (
    <EmptyState icon={PlayIcon} title={t('runs.empty')}>
      {t('runs.emptyText')}
    </EmptyState>
  )
}

function Runs({ data }: PageProps<Data>) {
  useLive(true)
  const t = useT()
  const { locale } = useLocale()
  const f = data.Filter
  const summary = filterSummary(f, data.RepoExact, locale)
  const count =
    data.Runs.length === 0
      ? ''
      : data.Paged
        ? t('runs.showing', { newest: data.NewestID, oldest: data.OldestID })
        : t('runs.latestCount', { count: data.Runs.length })
  // 按仓库筛选（或只有一个仓库）时不在每行重复仓库名
  const showRepo = data.RepoExact === ''
  return (
    <div className={css.page}>
      <PageHeader
        title={t('nav.runs')}
        description={t('runs.description')}
        actions={
          <LinkButton href={data.NewURL} variant="primary" leadingVisual={PlayIcon}>
            {t('runNew.pageTitle')}
          </LinkButton>
        }
      />

      {/* 从来没有构建时筛选没有意义 */}
      {(data.Runs.length > 0 || f.Active || data.Paged) && <FilterForm data={data} />}

      <Box>
        <BoxHeader
          title={
            data.Me ? (
              <nav className={css.scope} aria-label={t('runs.scope')}>
                <a href={data.AllURL} aria-current={f.Actor === '' ? 'page' : undefined}>
                  {t('runs.scopeAll')}
                </a>
                <a href={data.MineURL} aria-current={data.Mine ? 'page' : undefined}>
                  {t('runs.scopeMine')}
                </a>
              </nav>
            ) : (
              t('runs.builds')
            )
          }
        >
          <span className={css.count}>
            {summary}
            {count && t('runs.live', { what: count, seconds: liveSeconds })}
          </span>
        </BoxHeader>
        {data.Runs.length > 0 ? (
          <div role="list" aria-label={t('runs.builds')}>
            {data.Runs.map((r) => (
              <RunRow key={r.ID} run={r} showRepo={showRepo} />
            ))}
          </div>
        ) : (
          <Empty data={data} />
        )}
      </Box>

      {(data.NewerURL || data.OlderURL || data.Paged) && (
        <nav className={css.pager} aria-label={t('pager.label')}>
          {data.NewerURL && (
            <LinkButton href={data.NewerURL} rel="prev" size="small">
              {t('runs.newer')}
            </LinkButton>
          )}
          {data.Paged && (
            <LinkButton href={data.LatestURL} size="small" variant="invisible">
              {t('runs.latest')}
            </LinkButton>
          )}
          {data.OlderURL && (
            <LinkButton href={data.OlderURL} rel="next" size="small">
              {t('runs.older')}
            </LinkButton>
          )}
        </nav>
      )}
    </div>
  )
}

export default definePage(schema, Runs)

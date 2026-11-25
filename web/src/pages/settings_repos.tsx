import { LockIcon, RepoIcon } from '@primer/octicons-react'
import { Button, Label, LinkButton, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Box, BoxRow } from '../components/Box'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Trans, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import { Crumbs, settingsTags, withAgo } from './settings/common'
import { docStatus, emptyDocCountLabel } from './settings/docStatus'
import css from './settings_repos.module.css'

// 设置 · 仓库：可搜索、分页的仓库列表，每 5 秒自动更新；pick=env 时是新建仓库级 environment 的仓库选择器。

const repoSchema = z.object({
  FullName: z.string(),
  DefaultBranch: z.string(),
  Private: z.boolean(),
  Active: z.boolean(),
  InstallationID: z.number(),
  /** 外部文档的简要状态；停用的仓库也保留入口。旧页面的数据没有这些字段。 */
  DocBound: z.boolean().default(false),
  /** 空（尚未验证）/ ok / empty / failed。 */
  DocCheckStatus: z.string().default(''),
  DecisionDoc: z.boolean().default(false),
  DocsURL: z.string().default(''),
})
type Repo = z.infer<typeof repoSchema>

/** store.SyncStatus：最近一次"刷新仓库"的结果（字段带 json tag）。 */
const syncSchema = z.object({
  at: z.string(),
  actor: z.string(),
  error: z.string().default(''),
  installations: z.number(),
  repos: z.number(),
})

const schema = z.object({
  Section: z.string(),
  Repos: list(repoSchema),
  Query: z.string(),
  Page: z.number(),
  Pages: z.number(),
  Total: z.number(),
  PrevURL: z.string(),
  NextURL: z.string(),
  /** 非空时是选择器（"env"：为新建仓库级 environment 选择仓库）。 */
  Pick: z.string(),
  Sync: syncSchema.nullable(),
})
type Data = z.infer<typeof schema>

function SyncNote({ sync }: { sync: Data['Sync'] }) {
  const t = useT()
  return (
    <div className={css.sync} id="r-sync">
      <p>
        <Trans id="repos.syncHelp" tags={settingsTags} />
        {sync ? (
          <>
            <Trans id="repos.lastSync" params={{ actor: sync.actor }} tags={withAgo(sync.at)} />
            {sync.error ? (
              <span className={css.warn}>{t('repos.syncFailed', { error: sync.error })}</span>
            ) : (
              t('repos.syncResult', { installations: sync.installations, repos: sync.repos })
            )}
            {t('repos.end')}
          </>
        ) : (
          t('repos.neverSynced')
        )}
      </p>
      <p>
        <Trans id="repos.reconcileHelp" tags={settingsTags} />
      </p>
    </div>
  )
}

function RepoOps({ r, data }: { r: Repo; data: Data }) {
  const t = useT()
  const repo = { repo: r.FullName }
  const docs = (
    <LinkButton
      href={r.DocsURL || `/settings/repos/docs?repo=${encodeURIComponent(r.FullName)}`}
      size="small"
      aria-label={t('repos.docsLabel', repo)}
    >
      {t('repos.docs')}
    </LinkButton>
  )
  if (!r.Active) {
    return (
      <>
        {!data.Pick && docs}
        <span className={css.disabled}>{t('repos.inactiveNote')}</span>
      </>
    )
  }
  if (data.Pick) {
    return (
      <LinkButton
        href={`/settings/environments/new?repo=${encodeURIComponent(r.FullName)}`}
        variant="primary"
        size="small"
        aria-label={t('repos.pickLabel', repo)}
      >
        {t('repos.pick')}
      </LinkButton>
    )
  }
  return (
    <>
      {docs}
      <LinkButton href={`/settings/scope?repo=${encodeURIComponent(r.FullName)}`} size="small" aria-label={t('repos.varsLabel', repo)}>
        {t('repos.vars')}
      </LinkButton>
      <LinkButton href={`/runs?repo=${encodeURIComponent(r.FullName)}`} size="small" aria-label={t('repos.runsLabel', repo)}>
        {t('repos.runs')}
      </LinkButton>
      <Form action="/settings/repos/reconcile">
        <input type="hidden" name="repo" value={r.FullName} />
        <input type="hidden" name="q" value={data.Query} />
        <input type="hidden" name="page" value={data.Page} />
        <SubmitButton size="small" aria-label={t('repos.reconcileLabel', repo)}>
          {t('repos.reconcile')}
        </SubmitButton>
      </Form>
    </>
  )
}

function RepoRow({ r, data }: { r: Repo; data: Data }) {
  const t = useT()
  return (
    <BoxRow className={css.row}>
      {r.Private ? <LockIcon className={css.icon} aria-hidden /> : <RepoIcon className={css.icon} aria-hidden />}
      <div>
        <span className={css.name}>{r.FullName}</span>
        <div className={css.meta}>
          {r.DefaultBranch && <span>{r.DefaultBranch} ·</span>}
          <span>{t(r.Private ? 'repoVisibility.private' : 'repoVisibility.public')}</span>
          {!r.Active && <Label variant="attention">{t('repos.inactive')}</Label>}
          {r.DocBound && (
            <>
              <Label variant={docStatus(r.DocCheckStatus).variant}>{t(docStatus(r.DocCheckStatus).text)}</Label>
              {r.DocCheckStatus === 'empty' && <Label variant={emptyDocCountLabel.variant}>{t(emptyDocCountLabel.text)}</Label>}
            </>
          )}
          {r.DecisionDoc && <Label variant="secondary">{t('repos.decisionDoc')}</Label>}
        </div>
      </div>
      <div className={css.ops}>
        <RepoOps r={r} data={data} />
      </div>
    </BoxRow>
  )
}

function SettingsRepos({ data }: PageProps<Data>) {
  const t = useT()
  useLive(true)
  const pick = data.Pick !== ''
  const clearURL = pick ? `/settings/repos?pick=${encodeURIComponent(data.Pick)}` : '/settings/repos'
  return (
    <SettingsLayout section={data.Section}>
      {pick ? (
        <Subhead description={t('repos.pickDesc')}>
          <Crumbs
            items={[
              { label: t('settingsNav.envs'), href: '/settings/environments' },
              { label: t('repos.crumbNew'), href: '/settings/environments/new' },
            ]}
            current={t('repos.pickCurrent')}
          />
        </Subhead>
      ) : (
        <>
          <Subhead
            actions={
              <Form action="/sync">
                <input type="hidden" name="q" value={data.Query} />
                <input type="hidden" name="page" value={data.Page} />
                <SubmitButton>{t('repos.refresh')}</SubmitButton>
              </Form>
            }
            description={t('repos.description', { seconds: liveSeconds })}
          >
            {t('settingsNav.repos')}
          </Subhead>
          <SyncNote sync={data.Sync} />
        </>
      )}
      <Form action="/settings/repos" method="get" className={css.search} aria-label={t('repos.searchLabel')}>
        {pick && <input type="hidden" name="pick" value={data.Pick} />}
        <TextInput
          className={css.searchInput}
          type="search"
          name="q"
          defaultValue={data.Query}
          aria-label={t('repos.searchLabel')}
          placeholder={t('repos.searchPlaceholder')}
          spellCheck={false}
          autoCapitalize="off"
        />
        <Button type="submit">{t('settingsForm.search')}</Button>
        {data.Query && <LinkButton href={clearURL}>{t('repos.clear')}</LinkButton>}
      </Form>
      <p className={css.count}>
        {data.Query ? t('repos.countMatch', { count: data.Total, query: data.Query }) : t('repos.countAll', { count: data.Total })}
        {data.Pages > 1 && t('repos.pageSuffix', { page: data.Page, pages: data.Pages })}
      </p>
      <Box>
        {data.Repos.map((r) => (
          <RepoRow key={r.FullName} r={r} data={data} />
        ))}
        {data.Repos.length === 0 && (
          <BoxRow className={css.empty}>
            {data.Query ? t('repos.noMatch') : t('repos.empty')}
          </BoxRow>
        )}
      </Box>
      {data.Pages > 1 && (
        <nav className={css.pager} aria-label={t('pager.label')}>
          {data.PrevURL && (
            <LinkButton href={data.PrevURL} rel="prev">
              {t('pager.prev')}
            </LinkButton>
          )}
          <span>{t('pager.page', { page: data.Page, pages: data.Pages })}</span>
          {data.NextURL && (
            <LinkButton href={data.NextURL} rel="next">
              {t('pager.next')}
            </LinkButton>
          )}
        </nav>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsRepos)

import { ArrowLeftIcon, DatabaseIcon, SearchIcon } from '@primer/octicons-react'
import { Checkbox, CheckboxGroup, FormControl, Label, LinkButton, Select } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import { runnerViewSchema, type TFn } from '../components/RunnerSchema'
import { Ago } from '../components/Time'
import { Msg, Trans, useLocale, useT, type Tags } from '../i18n/locale'
import { isEmpty, isMsgKey } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { clock, size } from '../lib/format'
import css from './runner_cache.module.css'

// Runner 的本地缓存（PLAN #8.3）：Runner 最近一次上报的缓存清单、上一次自动清理的结果和清理请求。
// 管理员可以按仓库、类别和执行身份预览清理范围（GET 参数，服务端算好），确认后发起清理（Runner 空闲时执行）。

const entrySchema = z.object({
  /** 缓存类别；名称见 cacheKindText。 */
  Kind: z.string(),
  /** 所属仓库（owner/name）；仓库不在 Kiln 中时为 #ID，不属于单个仓库时为空。 */
  Repo: z.string(),
  Platform: z.string(),
  /** 执行身份（host、oci-root 等），只有 toolcache 有。 */
  Domain: z.string(),
  Name: z.string(),
  /** 数据目录中的相对路径。 */
  Path: z.string(),
  Size: z.number(),
  LastUsed: z.string().nullable(),
  Expires: z.string().nullable(),
})
type Entry = z.infer<typeof entrySchema>

const failureSchema = z.object({ Kind: z.string(), Path: z.string(), Error: z.string() })
type Failure = z.infer<typeof failureSchema>

const purgeSchema = z.object({
  ID: z.number(),
  Repo: z.string(),
  Kinds: list(z.string()),
  Domain: z.string(),
  /** pending：等待 Runner 空闲后执行；done：已执行。 */
  State: z.string(),
  Actor: z.string(),
  Created: z.string(),
  Done: z.string().nullable(),
  Removed: z.number(),
  RemovedBytes: z.number(),
  Failures: list(failureSchema),
})
type Purge = z.infer<typeof purgeSchema>

const schema = z.object({
  Runner: runnerViewSchema,
  /** 清单生成的时间；Runner 还没有上报过时为 null。 */
  Generated: z.string().nullable(),
  Entries: list(entrySchema),
  TotalBytes: z.number(),
  DiskTotal: z.number(),
  DiskFree: z.number(),
  GC: z
    .object({
      At: z.string(),
      Removed: z.number(),
      RemovedBytes: z.number(),
      Failures: list(failureSchema),
    })
    .nullable(),
  Purges: list(purgeSchema),
  /** 清理表单的选项（只有管理员有 Repos）和当前取值；Kinds 是缓存类别。 */
  Kinds: list(z.string()),
  Repos: list(z.string()),
  Domains: list(z.string()),
  Repo: z.string(),
  KindsPicked: list(z.string()),
  Domain: z.string(),
  /** 按当前取值预览的结果（管理员带 preview=1 打开时）。 */
  Preview: z
    .object({
      Matched: list(entrySchema),
      MatchedBytes: z.number(),
      Shared: list(entrySchema),
      /** 预览的说明和失败原因：服务端按原因码给出的消息。 */
      Notes: list(textSchema),
      Error: textSchema,
    })
    .nullable(),
  Self: z.string(),
})
type Data = z.infer<typeof schema>

/** cacheKindText 返回缓存类别的名称（不认识的类别原样显示）。 */
export function cacheKindText(kind: string, t: TFn): string {
  const key = `cacheKind.${kind}`
  return isMsgKey(key) ? t(key) : kind
}

/** scopeText 返回清理范围的一句话说明，例如"仓库 a/b，类别 toolcache、git 镜像，执行身份 host"。 */
export function scopeText(repo: string, kinds: readonly string[], domain: string, t: TFn): string {
  const sep = t('list.separator')
  return [
    repo ? t('runnerCache.scopeRepo', { repo }) : t('runnerCache.scopeAllRepos'),
    kinds.length > 0 ? t('runnerCache.scopeKinds', { kinds: kinds.map((k) => cacheKindText(k, t)).join(t('runner.enumSeparator')) }) : t('runnerCache.scopeAllKinds'),
    domain ? t('runnerCache.scopeDomain', { domain }) : t('runnerCache.scopeAllDomains'),
  ].join(sep)
}

function Failures({ list: items, label }: { list: Failure[]; label: string }) {
  const t = useT()
  if (items.length === 0) return null
  return (
    <ul className={css.failures} aria-label={label}>
      {items.map((f) => (
        <li key={`${f.Kind}:${f.Path}`}>
          <span className="mono">{f.Path || f.Kind}</span>
          {t('runnerCache.colon')}
          <span className={css.err}>{f.Error}</span>
        </li>
      ))}
    </ul>
  )
}

function EntriesTable({ entries, label }: { entries: Entry[]; label: string }) {
  const t = useT()
  const { locale } = useLocale()
  const cols = {
    kind: t('runnerCache.colKind'),
    repo: t('runnerCache.colRepo'),
    platform: t('runnerCache.colPlatform'),
    domain: t('runnerCache.colDomain'),
    name: t('runnerCache.colName'),
    size: t('runnerCache.colSize'),
    lastUsed: t('runnerCache.colLastUsed'),
    expires: t('runnerCache.colExpires'),
  }
  return (
    <div className={css.scroll}>
      <table className={css.table} aria-label={label}>
        <thead>
          <tr>
            <th scope="col">{cols.kind}</th>
            <th scope="col">{cols.repo}</th>
            <th scope="col">{cols.platform}</th>
            <th scope="col">{cols.domain}</th>
            <th scope="col">{cols.name}</th>
            <th scope="col" className={css.right}>
              {cols.size}
            </th>
            <th scope="col">{cols.lastUsed}</th>
            <th scope="col">{cols.expires}</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={`${e.Kind}:${e.Path}:${e.Name}`}>
              <td data-label={cols.kind}>{cacheKindText(e.Kind, t)}</td>
              <td data-label={cols.repo}>{e.Repo || <span className={css.muted}>{t('runnerCache.shared')}</span>}</td>
              <td data-label={cols.platform} className="mono">
                {e.Platform || '—'}
              </td>
              <td data-label={cols.domain}>{e.Domain || '—'}</td>
              <td data-label={cols.name} className={css.name} title={e.Path}>
                {e.Name || e.Path}
              </td>
              <td data-label={cols.size} className={`num ${css.right ?? ''}`}>
                {size(e.Size)}
              </td>
              <td data-label={cols.lastUsed} className={css.nowrap}>
                <Ago t={e.LastUsed} fallback="—" />
              </td>
              <td data-label={cols.expires} className={css.nowrap}>
                {e.Expires ? <time dateTime={e.Expires}>{clock(e.Expires, locale)}</time> : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Summary({ data }: { data: Data }) {
  const t = useT()
  return (
    <dl className={css.summary}>
      <div>
        <dt>{t('runnerCache.inventory')}</dt>
        <dd>
          {data.Generated ? (
            <Trans id="runnerCache.generated" tags={{ ago: () => <Ago t={data.Generated} /> }} />
          ) : (
            t('runnerCache.notReported')
          )}
        </dd>
      </div>
      <div>
        <dt>{t('runnerCache.total')}</dt>
        <dd>{t('runnerCache.totalValue', { size: size(data.TotalBytes), count: data.Entries.length })}</dd>
      </div>
      {data.DiskTotal > 0 && (
        <div>
          <dt>{t('runnerCache.disk')}</dt>
          <dd>{t('runnerCache.diskValue', { free: size(data.DiskFree), total: size(data.DiskTotal) })}</dd>
        </div>
      )}
    </dl>
  )
}

/** Removed 是一次清理的结果：删除了几项、多大，有删除失败时标红。 */
function Removed({ removed, bytes, failures }: { removed: number; bytes: number; failures: number }) {
  const t = useT()
  return (
    <>
      {t('runnerCache.removed', { count: removed, size: size(bytes) })}
      {failures > 0 && <span className={css.err}>{t('runnerCache.removeFailed', { count: failures })}</span>}
    </>
  )
}

function GC({ data }: { data: Data }) {
  const t = useT()
  const gc = data.GC
  return (
    <Box as="section" aria-labelledby="gc-title">
      <BoxHeader title={t('runnerCache.gcTitle')} id="gc-title" />
      <BoxRow className={css.block}>
        {gc ? (
          <>
            <p className={css.line}>
              <Trans id="runnerCache.gcAt" tags={{ ago: () => <Ago t={gc.At} /> }} />
              <Removed removed={gc.Removed} bytes={gc.RemovedBytes} failures={gc.Failures.length} />
            </p>
            <Failures list={gc.Failures} label={t('runnerCache.gcFailures')} />
          </>
        ) : (
          <p className={css.muted}>{t('runnerCache.gcEmpty')}</p>
        )}
      </BoxRow>
    </Box>
  )
}

function PurgeRow({ p }: { p: Purge }) {
  const t = useT()
  return (
    <BoxRow as="li" className={css.block}>
      <div className={css.purgeHead}>
        <b>#{p.ID}</b>
        <span>{scopeText(p.Repo, p.Kinds, p.Domain, t)}</span>
        {p.State === 'done' ? (
          <Label variant="success">{t('runnerCache.purgeDone')}</Label>
        ) : (
          <Label variant="attention">{t('runnerCache.purgePending')}</Label>
        )}
      </div>
      <p className={css.meta}>
        <Trans id="runnerCache.purgeBy" params={{ actor: p.Actor || '—' }} tags={{ ago: () => <Ago t={p.Created} /> }} />
        {p.State === 'done' && (
          <>
            <Trans id="runnerCache.purgeRan" tags={{ ago: () => <Ago t={p.Done} /> }} />
            <Removed removed={p.Removed} bytes={p.RemovedBytes} failures={p.Failures.length} />
          </>
        )}
      </p>
      <Failures list={p.Failures} label={t('runnerCache.purgeFailures', { id: p.ID })} />
    </BoxRow>
  )
}

function Purges({ data }: { data: Data }) {
  const t = useT()
  return (
    <Box as="section" aria-labelledby="purges-title">
      <BoxHeader title={t('runnerCache.purgesTitle')} id="purges-title" />
      {data.Purges.length > 0 ? (
        <ul className={css.rows} aria-label={t('runnerCache.purgesTitle')}>
          {data.Purges.map((p) => (
            <PurgeRow key={p.ID} p={p} />
          ))}
        </ul>
      ) : (
        <BoxRow className={css.muted}>{t('runnerCache.purgesEmpty')}</BoxRow>
      )}
    </Box>
  )
}

/** PurgeForm 是清理范围的表单：预览（GET，带 preview=1）在本页显示将删除的内容。 */
function PurgeForm({ data }: { data: Data }) {
  const t = useT()
  // 换了取值后重建表单，使默认值与页面数据一致
  const key = [data.Repo, data.KindsPicked.join(','), data.Domain].join('|')
  return (
    <Box as="section" aria-labelledby="purge-title">
      <BoxHeader title={t('runnerCache.purgeTitle')} id="purge-title" />
      <BoxRow className={css.block}>
        <Form key={key} action={data.Self} method="get" className={css.form} aria-label={t('runnerCache.formLabel')}>
          <input type="hidden" name="preview" value="1" />
          <FormControl>
            <FormControl.Label>{t('runnerCache.colRepo')}</FormControl.Label>
            <Select name="repo" defaultValue={data.Repo}>
              <Select.Option value="">{t('runnerCache.optAllRepos')}</Select.Option>
              {data.Repos.map((r) => (
                <Select.Option key={r} value={r}>
                  {r}
                </Select.Option>
              ))}
            </Select>
            <FormControl.Caption>{t('runnerCache.repoCaption')}</FormControl.Caption>
          </FormControl>
          <CheckboxGroup>
            <CheckboxGroup.Label>{t('runnerCache.colKind')}</CheckboxGroup.Label>
            <CheckboxGroup.Caption>{t('runnerCache.kindCaption')}</CheckboxGroup.Caption>
            <div className={css.kinds}>
              {data.Kinds.map((k) => (
                <FormControl key={k}>
                  <Checkbox name="kind" value={k} defaultChecked={data.KindsPicked.includes(k)} />
                  <FormControl.Label>{cacheKindText(k, t)}</FormControl.Label>
                </FormControl>
              ))}
            </div>
          </CheckboxGroup>
          <FormControl>
            <FormControl.Label>{t('runnerCache.colDomain')}</FormControl.Label>
            <Select name="domain" defaultValue={data.Domain}>
              <Select.Option value="">{t('runnerCache.optAllDomains')}</Select.Option>
              {data.Domains.map((d) => (
                <Select.Option key={d} value={d}>
                  {d}
                </Select.Option>
              ))}
            </Select>
            <FormControl.Caption>{t('runnerCache.domainCaption')}</FormControl.Caption>
          </FormControl>
          <div>
            <SubmitButton leadingVisual={SearchIcon}>{t('runnerCache.preview')}</SubmitButton>
          </div>
        </Form>
      </BoxRow>
    </Box>
  )
}

function Preview({ data }: { data: Data }) {
  const t = useT()
  const p = data.Preview
  if (!p) return null
  const name = data.Runner.Name
  const scope = scopeText(data.Repo, data.KindsPicked, data.Domain, t)
  if (!isEmpty(p.Error)) {
    return (
      <Callout tone="critical" title={t('runnerCache.previewFailed')}>
        <Msg text={p.Error} />
      </Callout>
    )
  }
  const strong: Tags = { b: (c) => <b>{c}</b> }
  return (
    <Box as="section" aria-labelledby="preview-title">
      <BoxHeader title={t('runnerCache.preview')} id="preview-title">
        <ConfirmForm
          action={`/runners/cache/${encodeURIComponent(name)}/purge`}
          fields={{ repo: data.Repo, kind: data.KindsPicked.join(','), domain: data.Domain }}
          button={t('runnerCache.purge')}
          buttonProps={{ size: 'small' }}
          title={t('runnerCache.purgeConfirmTitle', { name })}
          confirmLabel={t('runnerCache.purgeConfirm')}
        >
          <ul className={css.confirmList}>
            <li>{t('runnerCache.confirmScope', { scope })}</li>
            <li>{t('runnerCache.confirmMatched', { count: p.Matched.length, size: size(p.MatchedBytes) })}</li>
            <li>{t('runnerCache.confirmWhen', { name })}</li>
          </ul>
        </ConfirmForm>
      </BoxHeader>
      <BoxRow className={css.block}>
        <p className={css.line}>
          <Trans id="runnerCache.previewLine" params={{ scope, count: p.Matched.length, size: size(p.MatchedBytes) }} tags={strong} />
        </p>
        {p.Notes.length > 0 && (
          <ul className={css.notes} aria-label={t('runnerCache.notesLabel')}>
            {p.Notes.map((n, i) => (
              <li key={i}>
                <Msg text={n} />
              </li>
            ))}
          </ul>
        )}
      </BoxRow>
      {p.Matched.length > 0 && <EntriesTable entries={p.Matched} label={t('runnerCache.matchedLabel')} />}
      {p.Shared.length > 0 && (
        <>
          <BoxRow className={css.subhead}>{t('runnerCache.sharedTitle', { count: p.Shared.length })}</BoxRow>
          <EntriesTable entries={p.Shared} label={t('runnerCache.sharedLabel')} />
        </>
      )}
    </Box>
  )
}

function RunnerCache({ data, env }: PageProps<Data>) {
  const t = useT()
  const admin = env.Session?.Admin ?? false
  const r = data.Runner
  // 清理请求还没执行（等待 Runner 空闲）时自动刷新
  useLive(data.Purges.some((p) => p.State === 'pending'))
  return (
    <div className={css.page}>
      <PageHeader
        title={t('runnerCache.title', { name: r.Name })}
        description={t('runnerCache.description')}
        actions={
          <LinkButton href="/runners" leadingVisual={ArrowLeftIcon}>
            {t('runner.backToList')}
          </LinkButton>
        }
      />

      {r.Revoked && (
        <Callout tone="warning" title={t('runnerConn.revoked')}>
          {t('runnerCache.revoked', { name: r.Name })}
        </Callout>
      )}

      <Summary data={data} />

      <Box as="section" aria-labelledby="entries-title">
        <BoxHeader title={t('runnerCache.entriesTitle')} id="entries-title" />
        {data.Entries.length > 0 ? (
          <EntriesTable entries={data.Entries} label={t('runnerCache.entriesTitle')} />
        ) : (
          <EmptyState icon={DatabaseIcon} title={t(data.Generated ? 'runnerCache.emptyTitle' : 'runnerCache.noReportTitle')}>
            {t(data.Generated ? 'runnerCache.emptyText' : 'runnerCache.noReportText')}
          </EmptyState>
        )}
      </Box>

      <GC data={data} />
      <Purges data={data} />

      {admin && !r.Revoked && (
        <>
          <PurgeForm data={data} />
          <Preview data={data} />
        </>
      )}
      {!admin && <p className={css.muted}>{t('runnerCache.needAdmin')}</p>}
    </div>
  )
}

export default definePage(schema, RunnerCache)

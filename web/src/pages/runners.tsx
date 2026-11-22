import { FilterIcon, PlayIcon, PlusIcon, SearchIcon, ServerIcon } from '@primer/octicons-react'
import { ActionList, ActionMenu, Label, LinkButton } from '@primer/react'
import { Fragment } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Box, BoxFooter, BoxRow } from '../components/Box'
import boxCss from '../components/Box.module.css'
import { Callout } from '../components/Callout'
import { CopyButton } from '../components/CopyButton'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import {
  connLabel,
  limitsText,
  resourcesText,
  runnerCountsSchema,
  runnerIdle,
  runnerViewSchema,
  upgradeStateText,
  upgradeTone,
  type RunnerView,
  type TFn,
} from '../components/RunnerSchema'
import { RunnerIcon, RunnerLabels } from '../components/RunnerStatus'
import { StatusIcon } from '../components/StatusIcon'
import { Ago } from '../components/Time'
import { Msg, Trans, useT, useText, type Tags } from '../i18n/locale'
import { isEmpty, type MsgKey } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { shortVersion, size, timeOfDay } from '../lib/format'
import css from './runners.module.css'

// CI Runner 列表（AI 执行节点在 ai_runners.tsx）：连接状态（在线 / 离线 / 已吊销）和接单状态（接单中 / 已暂停接单）分开显示，
// 列出每台 Runner 正在执行的 job；管理员可以暂停或恢复接单、吊销，删除已吊销的。列表每 5 秒自动更新。

const schema = z.object({
  Runners: list(runnerViewSchema),
  Counts: runnerCountsSchema,
  /** 筛选条件（已校验）。 */
  State: z.string(),
  Pool: z.string(),
  /** 状态筛选的选项：Label 是服务端给的消息。 */
  States: list(z.object({ Value: z.string(), Label: textSchema })),
  Pools: list(z.string()),
  Self: z.string(),
  Now: z.string(),
  /** 页面说明中用到的时长（秒）。 */
  OfflineAfter: z.number(),
  LeaseTTL: z.number(),
  LongPoll: z.number(),
  Filtered: z.boolean(),
  StateLabel: textSchema,
})
type Data = z.infer<typeof schema>

/** runnerURL 返回 Runner 详情页的地址（与服务端 runnerURL 相同）。 */
export function runnerURL(name: string): string {
  return `/runners/detail/${encodeURIComponent(name)}`
}

/** runnersURL 返回带筛选条件的 Runner 列表地址（与服务端 runnersURL 相同）。 */
export function runnersURL(state: string, pool: string): string {
  const q = new URLSearchParams()
  if (state) q.set('state', state)
  if (pool) q.set('pool', pool)
  const s = q.toString()
  return s ? `/runners?${s}` : '/runners'
}

/** rich 是说明文字中的格式标签。 */
const rich: Tags = { b: (c) => <b>{c}</b>, code: (c) => <code>{c}</code> }

function Help({ data }: { data: Data }) {
  const t = useT()
  const help: [MsgKey, MsgKey][] = [
    ['runners.helpConnTerm', 'runners.helpConn'],
    ['runners.helpAcceptTerm', 'runners.helpAccept'],
    ['runners.helpPoolTerm', 'runners.helpPool'],
    ['runners.helpPlatformTerm', 'runners.helpPlatform'],
    ['runners.helpExecutorTerm', 'runners.helpExecutor'],
    ['runners.helpRunningTerm', 'runners.helpRunning'],
    ['runners.helpVersionTerm', 'runners.helpVersion'],
    ['runners.helpUpgradeTerm', 'runners.helpUpgrade'],
    ['runners.helpHeartbeatTerm', 'runners.helpHeartbeat'],
    ['runners.helpSpecTerm', 'runners.helpSpec'],
    ['runners.helpAllocTerm', 'runners.helpAlloc'],
    ['runners.helpCacheTerm', 'runners.helpCache'],
  ]
  const params = { offline: data.OfflineAfter, lease: data.LeaseTTL, poll: data.LongPoll }
  return (
    <details className={css.help}>
      <summary>{t('runners.helpSummary')}</summary>
      <dl className={css.kv}>
        {help.map(([term, text]) => (
          <Fragment key={term}>
            <dt>{t(term)}</dt>
            <dd>
              <Trans id={text} params={params} tags={rich} />
            </dd>
          </Fragment>
        ))}
      </dl>
    </details>
  )
}

/** Counts 是列表标题行中的统计，每项都是按状态筛选的链接（GitHub 议题列表的 Open / Closed）。 */
function Counts({ data }: { data: Data }) {
  const t = useT()
  const c = data.Counts
  const items: { state: string; n: number; label: MsgKey }[] = [
    { state: 'online', n: c.Online, label: 'runners.countOnline' },
    { state: 'accepting', n: c.Accepting, label: 'runners.countAccepting' },
    { state: 'paused', n: c.Paused, label: 'runners.countPaused' },
    { state: 'offline', n: c.Offline, label: 'runners.countOffline' },
    { state: 'revoked', n: c.Revoked, label: 'runners.countRevoked' },
    { state: 'busy', n: c.Jobs, label: 'runners.countJobs' },
  ]
  return (
    <ul className={css.counts} aria-label={t('runners.countsLabel')}>
      {items.map((i) => (
        <li key={i.state}>
          <a href={runnersURL(i.state, data.Pool)} aria-current={data.State === i.state ? 'true' : undefined}>
            <Trans id={i.label} params={{ count: i.n }} tags={{ b: (n) => <b className="num">{n}</b> }} />
          </a>
        </li>
      ))}
    </ul>
  )
}

function Filters({ data }: { data: Data }) {
  const t = useT()
  const text = useText()
  const stateLabel = isEmpty(data.StateLabel) ? t('runners.allStates') : text(data.StateLabel)
  return (
    <div className={css.filters}>
      <ActionMenu>
        <ActionMenu.Button size="small" leadingVisual={FilterIcon}>
          {t('runners.filterState', { state: stateLabel })}
        </ActionMenu.Button>
        <ActionMenu.Overlay width="small">
          <ActionList selectionVariant="single">
            <ActionList.LinkItem href={runnersURL('', data.Pool)} active={data.State === ''} aria-checked={data.State === ''}>
              {t('runners.allStates')}
            </ActionList.LinkItem>
            {data.States.map((s) => (
              <ActionList.LinkItem key={s.Value} href={runnersURL(s.Value, data.Pool)} active={data.State === s.Value} aria-checked={data.State === s.Value}>
                {text(s.Label)}
              </ActionList.LinkItem>
            ))}
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
      {data.Pools.length > 0 && (
        <ActionMenu>
          <ActionMenu.Button size="small">{t('runners.filterPool', { pool: data.Pool || t('runners.allShort') })}</ActionMenu.Button>
          <ActionMenu.Overlay width="small">
            <ActionList>
              <ActionList.LinkItem href={runnersURL(data.State, '')} active={data.Pool === ''} aria-checked={data.Pool === ''}>
                {t('runners.allPools')}
              </ActionList.LinkItem>
              {data.Pools.map((p) => (
                <ActionList.LinkItem key={p} href={runnersURL(data.State, p)} active={data.Pool === p} aria-checked={data.Pool === p}>
                  {p}
                </ActionList.LinkItem>
              ))}
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      )}
    </div>
  )
}

/** Ops 是一台 Runner 的管理员操作：暂停或恢复接单、吊销；已吊销的可以重新注册或删除。 */
function Ops({ runner: r, data }: { runner: RunnerView; data: Data }) {
  const t = useT()
  const back = { state: data.State, pool: data.Pool }
  const n = r.Jobs.length
  const name = r.Name
  if (r.Revoked) {
    return (
      <>
        <LinkButton size="small" href={`/runners/new?name=${encodeURIComponent(r.Name)}&pool=${encodeURIComponent(r.Pool)}`}>
          {t('runners.reenroll')}
        </LinkButton>
        {n > 0 ? (
          <span className={css.jobMeta}>{t('runners.deleteAfterJobs', { count: n })}</span>
        ) : (
          <ConfirmForm
            action={`/runners/${encodeURIComponent(r.Name)}/delete`}
            fields={back}
            button={t('runners.delete')}
            buttonProps={{ size: 'small' }}
            title={t('runners.deleteTitle', { name })}
            confirmLabel={t('runners.deleteConfirm', { name })}
          >
            <ul className={css.confirmList}>
              <li>{t('runners.deleteNote1', { name })}</li>
              <li>{t('runners.deleteNote2', { name })}</li>
              <li>{t('runners.deleteNote3', { name })}</li>
            </ul>
          </ConfirmForm>
        )}
      </>
    )
  }
  return (
    <>
      {r.Drain ? (
        <Form action={`/runners/${encodeURIComponent(r.Name)}/undrain`} aria-label={t('runners.undrainLabel', { name })}>
          <input type="hidden" name="state" value={data.State} />
          <input type="hidden" name="pool" value={data.Pool} />
          <SubmitButton size="small" leadingVisual={PlayIcon} title={t('runners.undrainTitle')}>
            {t('runners.undrain')}
          </SubmitButton>
        </Form>
      ) : (
        <ConfirmForm
          action={`/runners/${encodeURIComponent(r.Name)}/drain`}
          fields={back}
          danger={false}
          button={t('runners.drain')}
          buttonProps={{ size: 'small' }}
          title={t('runners.drainTitle', { name })}
          confirmLabel={t('runners.drainConfirm')}
        >
          <ul className={css.confirmList}>
            <li>{t('runners.drainNote1', { name, poll: data.LongPoll })}</li>
            <li>{n > 0 ? t('runners.drainNoteBusy', { count: n }) : t('runners.drainNoteIdle')}</li>
            <li>{t('runners.drainNote3')}</li>
          </ul>
        </ConfirmForm>
      )}
      <ConfirmForm
        action={`/runners/${encodeURIComponent(r.Name)}/revoke`}
        fields={back}
        button={t('runners.revoke')}
        buttonProps={{ size: 'small' }}
        title={t('runners.revokeTitle', { name })}
        confirmLabel={t('runners.revokeConfirm', { name })}
      >
        <p className={css.confirmLead}>{t('runners.revokeLead', { pool: r.Pool, conn: connLabel(r, t), count: n })}</p>
        <ul className={css.confirmList}>
          <li>{t('runners.revokeNote1', { name })}</li>
          {n > 0 ? (
            <li>
              <Trans
                id="runners.revokeNoteBusy"
                params={{ count: n, jobs: r.Jobs.map((j) => j.Name).join(t('runner.enumSeparator')), lease: data.LeaseTTL }}
                tags={rich}
              />
            </li>
          ) : (
            <li>{t('runners.revokeNoteIdle')}</li>
          )}
          <li>{t('runners.revokeNote3')}</li>
          <li>
            <Trans id="runners.revokeNote4" params={{ name }} tags={rich} />
          </li>
        </ul>
      </ConfirmForm>
    </>
  )
}

/** Facts 是 Runner 的平台、版本、心跳、容量和缓存；upgrade 为 false 时不显示升级状态（详情页单独显示）。 */
export function Facts({ runner: r, upgrade = true }: { runner: RunnerView; upgrade?: boolean }) {
  const t = useT()
  const ver = shortVersion(r.Version)
  const c = r.Capacity
  return (
    <dl className={css.facts}>
      <div>
        <dt>{t('runners.factPlatform')}</dt>
        <dd className="mono">{r.Platforms.length > 0 ? r.Platforms.join(', ') : '—'}</dd>
      </div>
      <div>
        <dt>executor</dt>
        <dd>{r.Executors.length > 0 ? r.Executors.join(', ') : '—'}</dd>
      </div>
      <div>
        <dt>{t('runners.factVersion')}</dt>
        <dd>
          {r.Version ? (
            <>
              <span className="mono" title={r.Version}>
                {ver}
              </span>
              {ver !== r.Version && <CopyButton value={r.Version} label={t('runners.copyVersion', { name: r.Name })} />}
            </>
          ) : (
            '—'
          )}
        </dd>
      </div>
      {upgrade && <UpgradeFact runner={r} />}
      <div>
        <dt>{t('runners.factHeartbeat')}</dt>
        <dd>
          <Ago t={r.LastSeen} fallback={t('runners.never')} />
        </dd>
      </div>
      <div>
        <dt>{t('runners.factSpec')}</dt>
        <dd>{c ? resourcesText(c.Machine, t) : t('runners.specNotReported')}</dd>
      </div>
      {c && (
        <div>
          <dt>{t('runners.factAlloc')}</dt>
          <dd title={t('runners.allocTitle', { reserved: resourcesText(c.Reserved, t), max: c.MaxJobs })}>
            {resourcesText(c.Allocated, t)} / {resourcesText(c.Allocatable, t)}
            {c.Limits.length > 0 && t('runners.allocLimits', { limits: limitsText(c, t).join(t('list.separator')) })}
          </dd>
        </div>
      )}
      <div>
        <dt>{t('runners.factCache')}</dt>
        <dd>
          <a href={`/runners/cache/${encodeURIComponent(r.Name)}`} aria-label={t('runners.cacheLabel', { name: r.Name })}>
            {r.CacheReportedAt ? size(r.CacheBytes) : t('runners.notReported')}
          </a>
          {r.CacheReportedAt && (
            <span className={css.factNote}>
              <Trans id="runners.cacheReported" tags={{ ago: () => <Ago t={r.CacheReportedAt} /> }} />
            </span>
          )}
        </dd>
      </div>
    </dl>
  )
}

/** UpgradeFact 是 Runner 的按台升级状态（PLAN #27.14）：进行中的请求、上一次失败、可以升级或已是最新；详情页中操作。 */
function UpgradeFact({ runner: r }: { runner: RunnerView }) {
  const t = useT()
  const u = r.Upgrade
  if (!u || r.Revoked) return null
  const tone = upgradeTone(r, u)
  const text = upgradeStateText(r, u, t)
  return (
    <div>
      <dt>{t('runners.factUpgrade')}</dt>
      <dd>
        <a
          href={runnerURL(r.Name)}
          className={tone === 'bad' ? css.upBad : tone === 'warn' ? css.upWarn : undefined}
          aria-label={t('runners.upgradeLabel', { name: r.Name, state: text })}
        >
          {text}
        </a>
      </dd>
    </div>
  )
}

/** UpgradeNotes 是需要注意的升级情况：上一次升级失败的原因，或者不能按台升级的原因。 */
function UpgradeNotes({ runner: r }: { runner: RunnerView }) {
  const t = useT()
  const u = r.Upgrade
  if (!u || r.Revoked) return null
  const tone = upgradeTone(r, u)
  if (tone === 'bad' && u.Request?.Error) {
    return (
      <p className={`${css.upgradeNote} ${css.upBad}`} title={t('runners.upgradeErrorTitle')}>
        {u.Request.Error}
      </p>
    )
  }
  if (tone === 'muted' && !isEmpty(u.Reason)) {
    return (
      <p className={`${css.upgradeNote} ${css.factNote}`}>
        <Msg text={u.Reason} />
      </p>
    )
  }
  return null
}

/** Work 是 Runner 正在执行的 job（没有时说明它在做什么）。 */
export function Work({ runner: r }: { runner: RunnerView }) {
  const t = useT()
  if (r.Jobs.length === 0) return <p className={css.idle}>{runnerIdle(r, t)}</p>
  return (
    <>
      {r.Drain && <p className={css.idle}>{t('runners.pausedWaiting', { count: r.Jobs.length })}</p>}
      <ul className={css.jobs} aria-label={t('runners.jobsLabel', { name: r.Name })}>
        {r.Jobs.map((j) => (
          <li key={j.JobID}>
            <StatusIcon state="running" size={14} />
            <a href={`/jobs/${j.JobID}`}>{j.Name}</a>
            <span className={css.jobMeta}>
              <Trans
                id="runners.jobMeta"
                params={{ repo: j.Repo, workflow: j.Workflow, run: j.RunID }}
                tags={{ ago: () => <Ago t={j.Started} /> }}
              />
            </span>
          </li>
        ))}
      </ul>
    </>
  )
}

function RunnerRow({ runner: r, data, admin }: { runner: RunnerView; data: Data; admin: boolean }) {
  return (
    <BoxRow as="li" id={`r-${r.ID}`} className={`${css.row} ${r.Revoked ? css.revoked : ''}`}>
      <span className={css.icon}>
        <RunnerIcon runner={r} running={r.Jobs.length} />
      </span>
      <div className={css.main}>
        <div className={css.head}>
          <h3 className={css.name}>
            <a href={runnerURL(r.Name)}>{r.Name}</a>
          </h3>
          <Label title="pool">{r.Pool}</Label>
          <RunnerLabels runner={r} />
        </div>
        <Facts runner={r} />
        <UpgradeNotes runner={r} />
        <Work runner={r} />
      </div>
      {admin && (
        <div className={css.ops}>
          <Ops runner={r} data={data} />
        </div>
      )}
    </BoxRow>
  )
}

function Runners({ data, env }: PageProps<Data>) {
  useLive(true)
  const t = useT()
  const text = useText()
  const admin = env.Session?.Admin ?? false
  const c = data.Counts
  return (
    <div className={css.page}>
      <PageHeader
        title={t('runners.title')}
        description={<Trans id="runners.description" tags={{ link: (s) => <a href="/ai/runners">{s}</a> }} />}
        actions={
          admin && (
            <>
              <LinkButton href="/settings/runner-upgrade">{t('runners.upgradeSettings')}</LinkButton>
              <LinkButton href="/runners/new" variant="primary" leadingVisual={PlusIcon}>
                {t('runners.register')}
              </LinkButton>
            </>
          )
        }
      />

      <Help data={data} />

      {c.Total > 0 && c.Accepting === 0 && (
        <Callout tone="warning" title={t('runners.noAcceptingTitle')}>
          {t(c.Paused > 0 ? 'runners.noAcceptingPaused' : 'runners.noAccepting')}
        </Callout>
      )}

      <Box className={css.list}>
        <h2 className="visually-hidden">{t('runners.listTitle')}</h2>
        {(c.Total > 0 || data.Filtered) && (
          <div className={`${boxCss.header ?? ''} ${css.listHead ?? ''}`}>
            {c.Total > 0 && <Counts data={data} />}
            <Filters data={data} />
          </div>
        )}
        {data.Filtered && (
          <BoxRow className={css.filtered}>
            <span>{filteredText(data, t, text(data.StateLabel))}</span>
            <a href="/runners">{t('runners.clearFilter')}</a>
          </BoxRow>
        )}
        {data.Runners.length > 0 ? (
          <ul className={css.rows} aria-label={t('runners.rowsLabel')}>
            {data.Runners.map((r) => (
              <RunnerRow key={r.ID} runner={r} data={data} admin={admin} />
            ))}
          </ul>
        ) : data.Filtered ? (
          <EmptyState icon={SearchIcon} title={t('runners.emptyFiltered')} secondary={{ label: t('runners.clearFilter'), href: '/runners' }} />
        ) : (
          <EmptyState icon={ServerIcon} title={t('runners.emptyTitle')}>
            {t('runners.emptyText')}
            {admin ? '' : t('runners.needAdmin')}
          </EmptyState>
        )}
        <BoxFooter className={css.footer}>
          {t('runners.footer', { count: c.Total, time: timeOfDay(data.Now), seconds: liveSeconds })}
        </BoxFooter>
      </Box>
    </div>
  )
}

/** filteredText 返回筛选条件的说明，例如"已筛选，状态：离线，pool：linux；显示 2 台。" */
export function filteredText(data: Pick<Data, 'Pool' | 'Runners'>, t: TFn, state: string): string {
  const parts = [state && t('runners.filteredState', { state }), data.Pool && t('runners.filteredPool', { pool: data.Pool })].filter(Boolean)
  return t('runners.filtered', { conditions: parts.join(''), count: data.Runners.length })
}

export default definePage(schema, Runners)

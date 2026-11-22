import { ArrowLeftIcon, DatabaseIcon, SyncIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import {
  requestStateText,
  runnerKindLabel,
  runnerViewSchema,
  upgradeRequestSchema,
  upgradeStateText,
  upgradeTone,
  upgrading,
  type RunnerUpgrade,
  type RunnerView,
  type UpgradeRequest,
} from '../components/RunnerSchema'
import { RunnerLabels } from '../components/RunnerStatus'
import { Ago } from '../components/Time'
import { Msg, Trans, useT, type Tags } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { list } from '../lib/envelope'
import { Facts, Work } from './runners'
import css from './runner.module.css'
import table from './settings_people.module.css'

// Runner 详情页（PLAN #27.14）：一台 Runner 的连接和接单状态、平台、版本、容量、正在执行的任务，以及按台升级：
// 当前版本、npm 包、Runner 查到的最新正式版本，管理员可以"升级到 vX"、取消、检查更新；下面是最近的升级请求。

const schema = z.object({
  Runner: runnerViewSchema,
  /** 最近的升级请求（新的在前）。 */
  History: list(upgradeRequestSchema),
  /** Runner 查询和下载升级包用的 registry；RegistryDefault 表示是官方 registry（没有设置）。 */
  Registry: z.string(),
  RegistryDefault: z.boolean(),
  /** 页面说明中用到的时长（秒）。 */
  OfflineAfter: z.number(),
  LongPoll: z.number(),
})
type Data = z.infer<typeof schema>

const upgradeAction = (name: string, op: 'start' | 'cancel' | 'check') => `/runners/upgrade/${encodeURIComponent(name)}/${op}`

/** UpgradeRunner 是升级部分用到的 Runner 字段。 */
type UpgradeRunner = Pick<RunnerView, 'Name' | 'Version' | 'Online' | 'Revoked' | 'Upgrade'>

/** UpgradeProps 是升级部分的数据：running 是正在执行的 job 数，其余来自页面数据。 */
interface UpgradeProps {
  runner: UpgradeRunner
  running: number
  registry: string
  registryDefault: boolean
  longPoll: number
  admin: boolean
}

const rich: Tags = { code: (c) => <code>{c}</code> }

function UpgradeFacts({ p, u }: { p: UpgradeProps; u: RunnerUpgrade }) {
  const t = useT()
  const r = p.runner
  return (
    <dl className={css.kv}>
      <dt>{t('runner.current')}</dt>
      <dd className="mono">{r.Version || '—'}</dd>
      <dt>{t('runner.package')}</dt>
      <dd className={u.Package ? 'mono' : css.muted}>{u.Package || t('runner.packageUnknown')}</dd>
      <dt>{t('runner.latest')}</dt>
      <dd>
        {u.Latest ? <span className="mono">{u.Latest}</span> : <span className={css.muted}>{t('runner.latestUnknown')}</span>}
        {u.CheckPending ? (
          <span className={css.muted}>{t('runner.checkPending', { poll: p.longPoll })}</span>
        ) : (
          u.CheckedAt && (
            <span className={css.muted}>
              <Trans id="runner.checkedAt" tags={{ ago: () => <Ago t={u.CheckedAt} /> }} />
            </span>
          )
        )}
        {u.CheckError && <div className={css.err}>{t('runner.checkError', { error: u.CheckError })}</div>}
      </dd>
      <dt>registry</dt>
      <dd>
        <span className="mono">{p.registry}</span>
        {p.registryDefault && <span className={css.muted}>{t('runner.registryDefault')}</span>}
      </dd>
      <dt>{t('runner.launcher')}</dt>
      <dd>
        {u.Launcher ? t('runner.launcherYes') : <span className={css.muted}>{t('runner.launcherNo')}</span>}
        {u.Disabled && (
          <span className={css.muted}>
            <Trans id="runner.disabled" tags={rich} />
          </span>
        )}
      </dd>
      {u.Previous && (
        <>
          <dt>{t('runner.previous')}</dt>
          <dd className="mono">{u.Previous}</dd>
        </>
      )}
    </dl>
  )
}

/** UpgradeStatus 是升级的当前情况和管理员的操作：进行中可以取消，可以升级时"升级到 vX"，否则说明原因。 */
function UpgradeStatus({ p, u }: { p: UpgradeProps; u: RunnerUpgrade }) {
  const t = useT()
  const { runner: r, running, admin } = p
  const req = upgrading(u)
  const tone = upgradeTone(r, u)
  const text = upgradeStateText(r, u, t)
  if (req) {
    return (
      <Callout tone="info" title={text}>
        <p className={css.p}>
          <Trans
            id={r.Online ? 'runner.pendingText' : 'runner.pendingTextOffline'}
            params={{ actor: req.Actor, from: req.From || '—', version: req.Version }}
            tags={{ ago: () => <Ago t={req.Created} /> }}
          />
        </p>
        {admin && (
          <div className={css.actions}>
            <ConfirmForm
              action={upgradeAction(r.Name, 'cancel')}
              button={t('runner.cancel')}
              buttonProps={{ size: 'small' }}
              danger={false}
              title={t('runner.cancelTitle', { name: r.Name, version: req.Version })}
              confirmLabel={t('runner.cancelConfirm')}
              cancelLabel={t('runner.cancelBack')}
            >
              {t('runner.cancelText')}
            </ConfirmForm>
          </div>
        )}
      </Callout>
    )
  }
  return (
    <div className={css.status}>
      {tone === 'bad' && u.Request && (
        <Callout tone="critical" title={text}>
          {u.Request.Error || t('runner.noReason')}
        </Callout>
      )}
      {u.CanUpgrade ? (
        <p className={`${css.p} ${css.upWarn}`}>
          <Trans id="runner.canUpgrade" params={{ version: u.Latest }} tags={{ v: (v) => <span className="mono">{v}</span> }} />
        </p>
      ) : (
        <p className={`${css.p} ${tone === 'ok' ? '' : css.muted}`}>{isEmpty(u.Reason) ? text : <Msg text={u.Reason} />}</p>
      )}
      {admin && !r.Revoked && (
        <div className={css.actions}>
          {u.CanUpgrade && (
            <ConfirmForm
              action={upgradeAction(r.Name, 'start')}
              fields={{ version: u.Latest }}
              button={t('runner.upgradeButton', { version: u.Latest })}
              buttonProps={{ size: 'small', variant: 'primary' }}
              danger={false}
              title={t('runner.upgradeTitle', { name: r.Name, version: u.Latest })}
              confirmLabel={t('runner.upgradeConfirm')}
            >
              <ul className={css.list}>
                <li>
                  <Trans
                    id="runner.upgradeNote1"
                    params={{ name: r.Name, registry: p.registry, pkg: u.Package, version: u.Latest }}
                    tags={{ pkg: (c) => <span className="mono">{c}</span> }}
                  />
                </li>
                <li>{running > 0 ? t('runner.upgradeNoteBusy', { count: running }) : t('runner.upgradeNoteIdle')}</li>
                <li>{t('runner.upgradeNoteRollback', { version: r.Version || t('runner.originalVersion') })}</li>
                {!r.Online && <li>{t('runner.upgradeNoteOffline')}</li>}
              </ul>
            </ConfirmForm>
          )}
          <Form action={upgradeAction(r.Name, 'check')} aria-label={t('runner.checkLabel', { name: r.Name })}>
            <SubmitButton size="small" leadingVisual={SyncIcon} disabled={u.CheckPending}>
              {t('runner.check')}
            </SubmitButton>
          </Form>
        </div>
      )}
    </div>
  )
}

/** UpgradeHistory 是最近的升级请求（新的在前）。 */
function UpgradeHistory({ items }: { items: UpgradeRequest[] }) {
  const t = useT()
  const cols = {
    version: t('runner.colVersion'),
    state: t('runner.colState'),
    actor: t('runner.colActor'),
    created: t('runner.colCreated'),
    finished: t('runner.colFinished'),
  }
  return (
    <table className={table.table} aria-labelledby="history-title">
      <thead>
        <tr>
          <th scope="col">{cols.version}</th>
          <th scope="col">{cols.state}</th>
          <th scope="col">{cols.actor}</th>
          <th scope="col">{cols.created}</th>
          <th scope="col">{cols.finished}</th>
        </tr>
      </thead>
      <tbody>
        {items.map((q) => (
          <tr key={q.ID}>
            <td data-label={cols.version} className={table.mono}>
              {q.From || '—'} → {q.Version}
            </td>
            <td data-label={cols.state}>
              <Label variant={q.State === 'failed' ? 'danger' : q.State === 'succeeded' ? 'success' : q.State === 'pending' ? 'accent' : 'secondary'}>
                {requestStateText(q.State, t)}
              </Label>
              {q.Error && <div className={q.State === 'failed' ? table.err : css.muted}>{q.Error}</div>}
            </td>
            <td data-label={cols.actor}>{q.Actor}</td>
            <td data-label={cols.created} className={table.nowrap}>
              <Ago t={q.Created} />
            </td>
            <td data-label={cols.finished} className={table.nowrap}>
              <Ago t={q.Finished} fallback="—" />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/** UpgradeSection 是按台升级（PLAN #27.14）：版本、npm 包、最新版本，进行中的请求和管理员的操作。 */
function UpgradeSection(p: UpgradeProps) {
  const t = useT()
  const u = p.runner.Upgrade
  return (
    <Box as="section" aria-labelledby="upgrade-title">
      <BoxHeader title={t('runner.upgradeSection')} id="upgrade-title" />
      {u ? (
        <>
          <BoxRow className={css.block}>
            <UpgradeFacts p={p} u={u} />
          </BoxRow>
          <BoxRow className={css.block}>
            <UpgradeStatus p={p} u={u} />
            {!p.admin && !p.runner.Revoked && <p className={`${css.p} ${css.muted}`}>{t('runner.needAdmin')}</p>}
          </BoxRow>
        </>
      ) : (
        <BoxRow className={css.muted}>{t('runner.noUpgrade')}</BoxRow>
      )}
    </Box>
  )
}

function Runner({ data, env }: PageProps<Data>) {
  const t = useT()
  const admin = env.Session?.Admin ?? false
  const r: RunnerView = data.Runner
  const u = r.Upgrade
  // 正在升级、等待检查或有正在执行的 job 时自动刷新
  useLive((!!u && (upgrading(u) !== null || u.CheckPending)) || r.Jobs.length > 0)
  return (
    <div className={css.page}>
      <PageHeader
        title={r.Name}
        description={t('runner.description', { kind: runnerKindLabel(r.Kind, r.Role, t), pool: r.Pool })}
        actions={
          <>
            <LinkButton href="/runners" leadingVisual={ArrowLeftIcon}>
              {t('runner.backToList')}
            </LinkButton>
            <LinkButton href={`/runners/cache/${encodeURIComponent(r.Name)}`} leadingVisual={DatabaseIcon}>
              {t('runner.cache')}
            </LinkButton>
          </>
        }
      />

      <Box as="section" aria-labelledby="summary-title">
        <BoxHeader title={t('runner.summary')} id="summary-title">
          <span className={css.labels}>
            <RunnerLabels runner={r} />
          </span>
        </BoxHeader>
        <BoxRow className={css.block}>
          <Facts runner={r} upgrade={false} />
          <Work runner={r} />
          <p className={`${css.p} ${css.muted}`}>
            <Trans id="runner.opsHint" params={{ offline: data.OfflineAfter }} tags={{ link: (c) => <a href="/runners">{c}</a> }} />
          </p>
        </BoxRow>
      </Box>

      <UpgradeSection
        runner={r}
        running={r.Jobs.length}
        registry={data.Registry}
        registryDefault={data.RegistryDefault}
        longPoll={data.LongPoll}
        admin={admin}
      />

      <Box as="section" aria-labelledby="history-title">
        <BoxHeader title={t('runner.history')} id="history-title" />
        {data.History.length > 0 ? <UpgradeHistory items={data.History} /> : <BoxRow className={css.muted}>{t('runner.historyEmpty')}</BoxRow>}
      </Box>
    </div>
  )
}

export default definePage(schema, Runner)

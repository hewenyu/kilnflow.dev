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
  OfflineAfter: z.string(),
  LongPoll: z.string(),
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
  longPoll: string
  admin: boolean
}

function UpgradeFacts({ p, u }: { p: UpgradeProps; u: RunnerUpgrade }) {
  const r = p.runner
  return (
    <dl className={css.kv}>
      <dt>当前版本</dt>
      <dd className="mono">{r.Version || '—'}</dd>
      <dt>npm 包</dt>
      <dd className={u.Package ? 'mono' : css.muted}>{u.Package || 'Runner 还没有上报'}</dd>
      <dt>最新版本</dt>
      <dd>
        {u.Latest ? <span className="mono">{u.Latest}</span> : <span className={css.muted}>还没有查到</span>}
        {u.CheckPending ? (
          <span className={css.muted}>，等待 Runner 检查（在线时最长约 {p.longPoll}）</span>
        ) : (
          u.CheckedAt && (
            <span className={css.muted}>
              ，<Ago t={u.CheckedAt} />
              查询
            </span>
          )
        )}
        {u.CheckError && <div className={css.err}>最近一次查询失败：{u.CheckError}</div>}
      </dd>
      <dt>registry</dt>
      <dd>
        <span className="mono">{p.registry}</span>
        {p.registryDefault && <span className={css.muted}>（官方）</span>}
      </dd>
      <dt>启动器</dt>
      <dd>
        {u.Launcher ? '经过启动器运行，可以切换版本' : <span className={css.muted}>没有经过启动器运行</span>}
        {u.Disabled && <span className={css.muted}>；config.toml 中 upgrade = false</span>}
      </dd>
      {u.Previous && (
        <>
          <dt>可回滚到</dt>
          <dd className="mono">{u.Previous}</dd>
        </>
      )}
    </dl>
  )
}

/** UpgradeStatus 是升级的当前情况和管理员的操作：进行中可以取消，可以升级时"升级到 vX"，否则说明原因。 */
function UpgradeStatus({ p, u }: { p: UpgradeProps; u: RunnerUpgrade }) {
  const { runner: r, running, admin } = p
  const req = upgrading(u)
  const tone = upgradeTone(r, u)
  const text = upgradeStateText(r, u)
  if (req) {
    return (
      <Callout tone="info" title={text}>
        <p className={css.p}>
          {req.Actor} <Ago t={req.Created} />
          请求从 {req.From || '—'} 升级到 {req.Version}。Runner 下载、核对后等正在执行的任务结束再切换，新版本启动并连上 Server 后完成；
          {r.Online ? '' : `它目前离线，重新连接后执行；`}新版本连续启动失败会回滚到原来的版本。
        </p>
        {admin && (
          <div className={css.actions}>
            <ConfirmForm
              action={upgradeAction(r.Name, 'cancel')}
              button="取消升级…"
              buttonProps={{ size: 'small' }}
              danger={false}
              title={`取消 ${r.Name} 升级到 ${req.Version}？`}
              confirmLabel="取消升级"
              cancelLabel="返回"
            >
              已经下载的包保留，之后再升级时直接使用。正在切换版本时不能取消。
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
          {u.Request.Error || '没有给出原因。'}
        </Callout>
      )}
      {u.CanUpgrade ? (
        <p className={`${css.p} ${css.upWarn}`}>
          可以升级到 <span className="mono">{u.Latest}</span>。
        </p>
      ) : (
        <p className={`${css.p} ${tone === 'ok' ? '' : css.muted}`}>{u.Reason || text}</p>
      )}
      {admin && !r.Revoked && (
        <div className={css.actions}>
          {u.CanUpgrade && (
            <ConfirmForm
              action={upgradeAction(r.Name, 'start')}
              fields={{ version: u.Latest }}
              button={`升级到 ${u.Latest}…`}
              buttonProps={{ size: 'small', variant: 'primary' }}
              danger={false}
              title={`把 ${r.Name} 升级到 ${u.Latest}？`}
              confirmLabel="升级"
            >
              <ul className={css.list}>
                <li>
                  {r.Name} 从 {p.registry} 下载 <span className="mono">{u.Package}</span> 的 {u.Latest}，按 registry 记录的 integrity 核对。
                </li>
                <li>
                  {running > 0 ? `等正在执行的 ${running} 个任务结束后切换，` : '它现在没有正在执行的任务，下载后立即切换，'}
                  切换期间不领取新任务。
                </li>
                <li>新版本连续启动失败会自动回滚到 {r.Version || '原来的版本'}。</li>
                {!r.Online && <li>它目前离线，重新连接后执行。</li>}
              </ul>
            </ConfirmForm>
          )}
          <Form action={upgradeAction(r.Name, 'check')} aria-label={`让 ${r.Name} 检查更新`}>
            <SubmitButton size="small" leadingVisual={SyncIcon} disabled={u.CheckPending}>
              检查更新
            </SubmitButton>
          </Form>
        </div>
      )}
    </div>
  )
}

/** UpgradeHistory 是最近的升级请求（新的在前）。 */
function UpgradeHistory({ items }: { items: UpgradeRequest[] }) {
  return (
    <table className={table.table} aria-labelledby="history-title">
      <thead>
        <tr>
          <th scope="col">版本</th>
          <th scope="col">状态</th>
          <th scope="col">请求人</th>
          <th scope="col">请求时间</th>
          <th scope="col">结束</th>
        </tr>
      </thead>
      <tbody>
        {items.map((q) => (
          <tr key={q.ID}>
            <td data-label="版本" className={table.mono}>
              {q.From || '—'} → {q.Version}
            </td>
            <td data-label="状态">
              <Label variant={q.State === 'failed' ? 'danger' : q.State === 'succeeded' ? 'success' : q.State === 'pending' ? 'accent' : 'secondary'}>
                {requestStateText(q.State)}
              </Label>
              {q.Error && <div className={q.State === 'failed' ? table.err : css.muted}>{q.Error}</div>}
            </td>
            <td data-label="请求人">{q.Actor}</td>
            <td data-label="请求时间" className={table.nowrap}>
              <Ago t={q.Created} />
            </td>
            <td data-label="结束" className={table.nowrap}>
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
  const u = p.runner.Upgrade
  return (
    <Box as="section" aria-labelledby="upgrade-title">
      <BoxHeader title="升级" id="upgrade-title" />
      {u ? (
        <>
          <BoxRow className={css.block}>
            <UpgradeFacts p={p} u={u} />
          </BoxRow>
          <BoxRow className={css.block}>
            <UpgradeStatus p={p} u={u} />
            {!p.admin && !p.runner.Revoked && <p className={`${css.p} ${css.muted}`}>升级需要管理员。</p>}
          </BoxRow>
        </>
      ) : (
        <BoxRow className={css.muted}>没有升级状态。</BoxRow>
      )}
    </Box>
  )
}

function Runner({ data, env }: PageProps<Data>) {
  const admin = env.Session?.Admin ?? false
  const r: RunnerView = data.Runner
  const u = r.Upgrade
  // 正在升级、等待检查或有正在执行的 job 时自动刷新
  useLive((!!u && (upgrading(u) !== null || u.CheckPending)) || r.Jobs.length > 0)
  return (
    <div className={css.page}>
      <PageHeader
        title={r.Name}
        description={`${r.KindLabel}，pool ${r.Pool}`}
        actions={
          <>
            <LinkButton href="/runners" leadingVisual={ArrowLeftIcon}>
              Runner 列表
            </LinkButton>
            <LinkButton href={`/runners/cache/${encodeURIComponent(r.Name)}`} leadingVisual={DatabaseIcon}>
              本地缓存
            </LinkButton>
          </>
        }
      />

      <Box as="section" aria-labelledby="summary-title">
        <BoxHeader title="概况" id="summary-title">
          <span className={css.labels}>
            <RunnerLabels runner={r} />
          </span>
        </BoxHeader>
        <BoxRow className={css.block}>
          <Facts runner={r} upgrade={false} />
          <Work runner={r} />
          <p className={`${css.p} ${css.muted}`}>
            暂停接单、吊销、删除在 <a href="/runners">Runner 列表</a> 中操作；{data.OfflineAfter}
            没有联系 Server 时显示为离线。
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
        <BoxHeader title="升级记录" id="history-title" />
        {data.History.length > 0 ? <UpgradeHistory items={data.History} /> : <BoxRow className={css.muted}>还没有升级过。</BoxRow>}
      </Box>
    </div>
  )
}

export default definePage(schema, Runner)

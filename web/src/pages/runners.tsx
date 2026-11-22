import { FilterIcon, PlayIcon, PlusIcon, SearchIcon, ServerIcon } from '@primer/octicons-react'
import { ActionList, ActionMenu, Label, LinkButton } from '@primer/react'
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
import { runnerCountsSchema, runnerIdle, runnerViewSchema, upgradeStateText, upgradeTone, type RunnerView } from '../components/RunnerSchema'
import { RunnerIcon, RunnerLabels } from '../components/RunnerStatus'
import { StatusIcon } from '../components/StatusIcon'
import { Ago } from '../components/Time'
import { list } from '../lib/envelope'
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
  States: list(z.object({ Value: z.string(), Label: z.string() })),
  Pools: list(z.string()),
  Self: z.string(),
  Now: z.string(),
  /** 页面说明中用到的时长，例如"2 分钟"。 */
  OfflineAfter: z.string(),
  LeaseTTL: z.string(),
  LongPoll: z.string(),
  Filtered: z.boolean(),
  StateLabel: z.string(),
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

function Help({ data }: { data: Data }) {
  return (
    <details className={css.help}>
      <summary>状态、各列和操作是什么意思</summary>
      <dl className={css.kv}>
        <dt>连接</dt>
        <dd>
          <b>在线</b>：{data.OfflineAfter}内联系过 Server（领取任务或上报心跳）。<b>离线</b>：超过 {data.OfflineAfter}
          没有联系，可能是机器关机、网络断开或服务没有运行；离线时不会领取任务，它名下正在执行的任务在租约过期（约 {data.LeaseTTL}
          ）后按"Runner 失联"处理。<b>已吊销</b>：身份已作废，不能再连接，需要重新注册。
        </dd>
        <dt>接单</dt>
        <dd>
          <b>接单中</b>：有空闲时领取匹配的任务。<b>已暂停接单</b>：不再领取新任务（在 Runner 正在进行的一轮领取结束后生效，最长约 {data.LongPoll}
          ），正在执行的任务继续执行到结束，不会被中断；Runner 保持连接，恢复接单后立即可以领取。暂停状态保存在 Server 上，Runner
          重启后仍然暂停；重新注册会恢复接单。在机器上执行 <code>kiln-runner drain</code> 是本地暂停，不显示在这里。
        </dd>
        <dt>pool</dt>
        <dd>
          机器组，注册时由令牌决定。写了 <code>pool: X</code> 的 job 只在 pool X 的 Runner 上执行；没有写 pool 的 job 可以在任何 pool 上执行。
        </dd>
        <dt>平台</dt>
        <dd>
          Runner 能执行的 <code>runs-on</code> 取值（os/arch），由 Runner 启动时探测。
        </dd>
        <dt>executor</dt>
        <dd>
          <code>host</code>：直接在机器上执行；<code>oci</code>：在容器中执行（机器上的 docker 可用时才有）；<code>buildkit</code>
          ：可以构建镜像（在 Runner 的 config.toml 中开启）。
        </dd>
        <dt>运行中</dt>
        <dd>当前分配给这台 Runner、正在执行的 job，点击进入 job 页面。</dd>
        <dt>版本</dt>
        <dd>kiln-runner 的版本；较长的只显示前 12 位，点复制按钮得到完整值。</dd>
        <dt>升级</dt>
        <dd>
          Runner 每小时从 npm 查询自己的最新正式版本。<b>可以升级</b>时，管理员在 Runner 详情页点"升级"：Runner
          下载核对后，等正在执行的任务结束再切换，新版本连续启动失败会回滚到原来的版本。不经过启动器运行的 Runner（前台 <code>run</code>
          或旧的服务）不能按台升级，在机器上执行一次 <code>service install</code> 即可；config.toml 中 <code>upgrade = false</code>
          表示本地关闭了自升级。
        </dd>
        <dt>最后心跳</dt>
        <dd>最后一次联系 Server 的时间。</dd>
        <dt>规格</dt>
        <dd>
          机器的逻辑 CPU 核数和物理内存，由 Runner 每次领取任务时上报。这是<b>静态规格</b>，不是实时占用；旧版 kiln-runner
          不上报，显示"未上报"。
        </dd>
        <dt>已分配 / 可分配</dt>
        <dd>
          <b>已分配</b>是正在执行的 job 声明的 <code>resources</code>（没有声明时按默认值）之和；<b>可分配</b>是机器规格减去
          config.toml 中为宿主机预留的部分。某种 executor 有固定上限时（host 服务的 cgroup 限制、docker 的 CPU 和内存）再取两者中较小的。
          调度按声明的资源记账，已分配加上新 job 的声明不会超过这个容量，放不下的 job 留在队列中等待，不会因为机器当时比较空闲而多领。
        </dd>
        <dt>本地缓存</dt>
        <dd>
          Runner 上报的本地缓存（toolcache、runtime、git 镜像、镜像等）总大小和上报时间；点击查看清单、上一次清理的结果，
          管理员可以在那里按仓库或类别清理。
        </dd>
      </dl>
    </details>
  )
}

/** Counts 是列表标题行中的统计，每项都是按状态筛选的链接（GitHub 议题列表的 Open / Closed）。 */
function Counts({ data }: { data: Data }) {
  const c = data.Counts
  const items = [
    { state: 'online', n: c.Online, label: '在线' },
    { state: 'accepting', n: c.Accepting, label: '接单中' },
    { state: 'paused', n: c.Paused, label: '已暂停' },
    { state: 'offline', n: c.Offline, label: '离线' },
    { state: 'revoked', n: c.Revoked, label: '已吊销' },
    { state: 'busy', n: c.Jobs, label: '个任务在执行' },
  ]
  return (
    <ul className={css.counts} aria-label="统计（点击按状态筛选）">
      {items.map((i) => (
        <li key={i.state}>
          <a href={runnersURL(i.state, data.Pool)} aria-current={data.State === i.state ? 'true' : undefined}>
            <b className="num">{i.n}</b> {i.label}
          </a>
        </li>
      ))}
    </ul>
  )
}

function Filters({ data }: { data: Data }) {
  const stateLabel = data.StateLabel || '全部状态'
  return (
    <div className={css.filters}>
      <ActionMenu>
        <ActionMenu.Button size="small" leadingVisual={FilterIcon}>
          状态：{stateLabel}
        </ActionMenu.Button>
        <ActionMenu.Overlay width="small">
          <ActionList selectionVariant="single">
            <ActionList.LinkItem href={runnersURL('', data.Pool)} active={data.State === ''} aria-checked={data.State === ''}>
              全部状态
            </ActionList.LinkItem>
            {data.States.map((s) => (
              <ActionList.LinkItem key={s.Value} href={runnersURL(s.Value, data.Pool)} active={data.State === s.Value} aria-checked={data.State === s.Value}>
                {s.Label}
              </ActionList.LinkItem>
            ))}
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
      {data.Pools.length > 0 && (
        <ActionMenu>
          <ActionMenu.Button size="small">pool：{data.Pool || '全部'}</ActionMenu.Button>
          <ActionMenu.Overlay width="small">
            <ActionList>
              <ActionList.LinkItem href={runnersURL(data.State, '')} active={data.Pool === ''} aria-checked={data.Pool === ''}>
                全部 pool
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
  const back = { state: data.State, pool: data.Pool }
  const n = r.Jobs.length
  if (r.Revoked) {
    return (
      <>
        <LinkButton size="small" href={`/runners/new?name=${encodeURIComponent(r.Name)}&pool=${encodeURIComponent(r.Pool)}`}>
          重新注册…
        </LinkButton>
        {n > 0 ? (
          <span className={css.jobMeta}>{n} 个任务结束后可以删除</span>
        ) : (
          <ConfirmForm
            action={`/runners/${encodeURIComponent(r.Name)}/delete`}
            fields={back}
            button="删除…"
            buttonProps={{ size: 'small' }}
            title={`删除 ${r.Name}？`}
            confirmLabel={`确认删除 ${r.Name}`}
          >
            <ul className={css.confirmList}>
              <li>{r.Name} 从 Runner 列表中移除，它的缓存清单和清理记录一并删除。删除不能撤销。</li>
              <li>它执行过的尝试和日志按保留策略保留，job 页面仍显示 Runner 名称 {r.Name}。</li>
              <li>
                名称 {r.Name} 可以重新注册，注册后是一台新的 Runner。要继续使用这台机器，也可以不删除，直接"重新注册"。
              </li>
            </ul>
          </ConfirmForm>
        )}
      </>
    )
  }
  return (
    <>
      {r.Drain ? (
        <Form action={`/runners/${encodeURIComponent(r.Name)}/undrain`} aria-label={`恢复 ${r.Name} 接单`}>
          <input type="hidden" name="state" value={data.State} />
          <input type="hidden" name="pool" value={data.Pool} />
          <SubmitButton size="small" leadingVisual={PlayIcon} title="恢复后有空闲时会领取新任务">
            恢复接单
          </SubmitButton>
        </Form>
      ) : (
        <ConfirmForm
          action={`/runners/${encodeURIComponent(r.Name)}/drain`}
          fields={back}
          danger={false}
          button="暂停接单…"
          buttonProps={{ size: 'small' }}
          title={`暂停 ${r.Name} 接收新任务？`}
          confirmLabel="确认暂停接单"
        >
          <ul className={css.confirmList}>
            <li>
              {r.Name} 不再领取新任务。在它正在进行的一轮领取结束后生效（最长约 {data.LongPoll}），这期间仍可能领到新任务。
            </li>
            <li>{n > 0 ? `正在执行的 ${n} 个任务继续执行到结束，不会被中断。` : '它现在没有正在执行的任务。'}</li>
            <li>Runner 保持连接、显示为在线；随时可以"恢复接单"。</li>
          </ul>
        </ConfirmForm>
      )}
      <ConfirmForm
        action={`/runners/${encodeURIComponent(r.Name)}/revoke`}
        fields={back}
        button="吊销…"
        buttonProps={{ size: 'small' }}
        title={`吊销 ${r.Name}？`}
        confirmLabel={`确认吊销 ${r.Name}`}
      >
        <p className={css.confirmLead}>
          pool {r.Pool}，{r.ConnLabel}，正在执行 {n} 个任务
        </p>
        <ul className={css.confirmList}>
          <li>立即生效：{r.Name} 之后的所有请求都会被拒绝，kiln-runner 会中断正在执行的任务并退出，服务不会自动重启它。</li>
          {n > 0 ? (
            <li>
              正在执行的 {n} 个任务（{r.Jobs.map((j) => j.Name).join('、')}）会被中断，约 {data.LeaseTTL}
              后（租约过期）判为"Runner 失联"，按基础设施错误自动重新排队，由其他 Runner 执行；有副作用且已开始执行的 job
              不会自动重试，会标为失败。<b>只想让它停下来时，先"暂停接单"，等任务结束后再吊销。</b>
            </li>
          ) : (
            <li>它现在没有正在执行的任务。</li>
          )}
          <li>历史记录（尝试、日志）保留，列表中显示为"已吊销"，之后可以删除。吊销不能撤销。</li>
          <li>
            恢复方式：生成新的注册令牌，在这台机器上重新执行 <code>kiln-runner enroll --name {r.Name}</code>
            ，再重启 Runner 服务。同名注册会替换原来的身份，恢复为在线并接单。
          </li>
        </ul>
      </ConfirmForm>
    </>
  )
}

/** Facts 是 Runner 的平台、版本、心跳、容量和缓存；upgrade 为 false 时不显示升级状态（详情页单独显示）。 */
export function Facts({ runner: r, upgrade = true }: { runner: RunnerView; upgrade?: boolean }) {
  const ver = shortVersion(r.Version)
  return (
    <dl className={css.facts}>
      <div>
        <dt>平台</dt>
        <dd className="mono">{r.Platforms.length > 0 ? r.Platforms.join(', ') : '—'}</dd>
      </div>
      <div>
        <dt>executor</dt>
        <dd>{r.Executors.length > 0 ? r.Executors.join(', ') : '—'}</dd>
      </div>
      <div>
        <dt>版本</dt>
        <dd>
          {r.Version ? (
            <>
              <span className="mono" title={r.Version}>
                {ver}
              </span>
              {ver !== r.Version && <CopyButton value={r.Version} label={`复制 ${r.Name} 的完整版本号`} />}
            </>
          ) : (
            '—'
          )}
        </dd>
      </div>
      {upgrade && <UpgradeFact runner={r} />}
      <div>
        <dt>最后心跳</dt>
        <dd>
          <Ago t={r.LastSeen} fallback="从未" />
        </dd>
      </div>
      <div>
        <dt>规格</dt>
        <dd>{r.Capacity ? r.Capacity.Machine : '未上报（旧版 kiln-runner）'}</dd>
      </div>
      {r.Capacity && (
        <div>
          <dt>已分配 / 可分配</dt>
          <dd title={`预留 ${r.Capacity.Reserved}，最多同时执行 ${r.Capacity.MaxJobs} 个 job`}>
            {r.Capacity.Allocated} / {r.Capacity.Allocatable}
            {r.Capacity.Limits.length > 0 && `（${r.Capacity.Limits.join('，')}）`}
          </dd>
        </div>
      )}
      <div>
        <dt>本地缓存</dt>
        <dd>
          <a href={`/runners/cache/${encodeURIComponent(r.Name)}`} aria-label={`${r.Name} 的本地缓存`}>
            {r.CacheReportedAt ? size(r.CacheBytes) : '未上报'}
          </a>
          {r.CacheReportedAt && (
            <span className={css.factNote}>
              ，<Ago t={r.CacheReportedAt} />上报
            </span>
          )}
        </dd>
      </div>
    </dl>
  )
}

/** UpgradeFact 是 Runner 的按台升级状态（PLAN #27.14）：进行中的请求、上一次失败、可以升级或已是最新；详情页中操作。 */
function UpgradeFact({ runner: r }: { runner: RunnerView }) {
  const u = r.Upgrade
  if (!u || r.Revoked) return null
  const tone = upgradeTone(r, u)
  return (
    <div>
      <dt>升级</dt>
      <dd>
        <a href={runnerURL(r.Name)} className={tone === 'bad' ? css.upBad : tone === 'warn' ? css.upWarn : undefined} aria-label={`${r.Name} 的升级：${upgradeStateText(r, u)}`}>
          {upgradeStateText(r, u)}
        </a>
      </dd>
    </div>
  )
}

/** UpgradeNotes 是需要注意的升级情况：上一次升级失败的原因，或者不能按台升级的原因。 */
function UpgradeNotes({ runner: r }: { runner: RunnerView }) {
  const u = r.Upgrade
  if (!u || r.Revoked) return null
  const tone = upgradeTone(r, u)
  if (tone === 'bad' && u.Request?.Error) {
    return (
      <p className={`${css.upgradeNote} ${css.upBad}`} title="上一次升级失败的原因">
        {u.Request.Error}
      </p>
    )
  }
  if (tone === 'muted' && u.Reason) return <p className={`${css.upgradeNote} ${css.factNote}`}>{u.Reason}</p>
  return null
}

/** Work 是 Runner 正在执行的 job（没有时说明它在做什么）。 */
export function Work({ runner: r }: { runner: RunnerView }) {
  if (r.Jobs.length === 0) return <p className={css.idle}>{runnerIdle(r)}</p>
  return (
    <>
      {r.Drain && <p className={css.idle}>已暂停接单，等 {r.Jobs.length} 个任务执行完。</p>}
      <ul className={css.jobs} aria-label={`${r.Name} 正在执行的任务`}>
        {r.Jobs.map((j) => (
          <li key={j.JobID}>
            <StatusIcon state="running" size={14} />
            <a href={`/jobs/${j.JobID}`}>{j.Name}</a>
            <span className={css.jobMeta}>
              {j.Repo}，{j.Workflow} #{j.RunID}，<Ago t={j.Started} />开始
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
  const admin = env.Session?.Admin ?? false
  const c = data.Counts
  return (
    <div className={css.page}>
      <PageHeader
        title="Runner"
        description={
          <>
            执行 job 的机器（kiln-runner），只向 Server 发出站连接。AI 审计和 AI 开发由 Orca + kiln-orca 执行，见 <a href="/ai/runners">AI 执行节点</a>。
          </>
        }
        actions={
          admin && (
            <>
              <LinkButton href="/settings/runner-upgrade">Runner 升级</LinkButton>
              <LinkButton href="/runners/new" variant="primary" leadingVisual={PlusIcon}>
                注册新的 Runner
              </LinkButton>
            </>
          )
        }
      />

      <Help data={data} />

      {c.Total > 0 && c.Accepting === 0 && (
        <Callout tone="warning" title="没有在线并接单的 Runner">
          新的任务会一直排队，直到有 Runner 上线{c.Paused > 0 ? '或恢复接单' : ''}。
        </Callout>
      )}

      <Box className={css.list}>
        <h2 className="visually-hidden">Runner 列表</h2>
        {(c.Total > 0 || data.Filtered) && (
          <div className={`${boxCss.header ?? ''} ${css.listHead ?? ''}`}>
            {c.Total > 0 && <Counts data={data} />}
            <Filters data={data} />
          </div>
        )}
        {data.Filtered && (
          <BoxRow className={css.filtered}>
            <span>
              已筛选{data.StateLabel && `，状态：${data.StateLabel}`}
              {data.Pool && `，pool：${data.Pool}`}；显示 {data.Runners.length} 台。
            </span>
            <a href="/runners">清空筛选</a>
          </BoxRow>
        )}
        {data.Runners.length > 0 ? (
          <ul className={css.rows} aria-label="Runner">
            {data.Runners.map((r) => (
              <RunnerRow key={r.ID} runner={r} data={data} admin={admin} />
            ))}
          </ul>
        ) : data.Filtered ? (
          <EmptyState icon={SearchIcon} title="没有符合筛选条件的 Runner" secondary={{ label: '清空筛选', href: '/runners' }} />
        ) : (
          <EmptyState icon={ServerIcon} title="还没有 Runner">
            注册至少一台 Runner 后，job 才能执行。{admin ? '' : '注册 Runner 需要管理员。'}
          </EmptyState>
        )}
        <BoxFooter className={css.footer}>
          共 {c.Total} 台，更新于 <time dateTime={data.Now}>{timeOfDay(data.Now)}</time>，每 {liveSeconds} 秒自动更新。
        </BoxFooter>
      </Box>
    </div>
  )
}

export default definePage(schema, Runners)

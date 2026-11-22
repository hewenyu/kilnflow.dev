import { SyncIcon, VersionsIcon } from '@primer/octicons-react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { Form, SubmitButton } from '../components/Form'
import { runnerViewSchema, upgradeStateText, upgradeTone, type RunnerView } from '../components/RunnerSchema'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { Msg, useT } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { formStateSchema, list } from '../lib/envelope'
import { shortVersion } from '../lib/format'
import { FormAlert, TextField, settingsCss } from './settings/common'
import table from './settings_people.module.css'
import css from './settings_upgrade.module.css'

// 设置 · Runner 升级（PLAN #27.14）：Runner 查询和下载升级包用的 npm registry（留空用官方），每台 Runner 的版本、
// 查到的最新版本和升级状态；升级在 Runner 详情页按台操作。修改经服务端 admin.Service（与 CLI kiln runner upgrade 相同的校验和审计）。

const schema = z.object({
  Section: z.string(),
  /** 设置的 registry；空表示官方 registry。 */
  Registry: z.string(),
  DefaultRegistry: z.string(),
  /** 没有吊销的 Runner。 */
  Runners: list(runnerViewSchema),
  /** registry 表单的值和错误。 */
  Form: formStateSchema,
})
type Data = z.infer<typeof schema>

function RegistrySection({ data }: { data: Data }) {
  return (
    <section aria-labelledby="up-registry">
      <Subhead
        level={2}
        id="up-registry"
        description="Runner 从这里查询自己的 npm 包的最新正式版本、下载升级包，并按 registry 记录的 integrity 核对。Server 不访问 npm。"
      >
        npm registry
      </Subhead>
      <Form action="/settings/runner-upgrade/registry" className={settingsCss.form} aria-label="npm registry">
        <FormAlert form={data.Form} />
        <TextField
          form={data.Form}
          name="registry"
          mono
          label="registry 地址"
          caption={
            <>
              留空使用官方 registry（<code>{data.DefaultRegistry}</code>）。必须是 https（本机地址除外），不能带用户信息；不需要 token。
            </>
          }
        />
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">保存</SubmitButton>
        </div>
      </Form>
      <p className={settingsCss.muted}>
        当前使用：<span className="mono">{data.Registry || data.DefaultRegistry}</span>
        {data.Registry ? '' : '（官方）'}
      </p>
    </section>
  )
}

function LatestCell({ r }: { r: RunnerView }) {
  const u = r.Upgrade
  if (!u) return <span className={settingsCss.muted}>—</span>
  return (
    <>
      {u.Latest ? <span className="mono">{u.Latest}</span> : <span className={settingsCss.muted}>还没有查到</span>}
      {u.CheckPending ? (
        <div className={settingsCss.muted}>等待 Runner 检查</div>
      ) : (
        u.CheckedAt && (
          <div className={settingsCss.muted}>
            <Ago t={u.CheckedAt} />
            查询
          </div>
        )
      )}
      {u.CheckError && <div className={table.err}>{u.CheckError}</div>}
    </>
  )
}

function StateCell({ r }: { r: RunnerView }) {
  const t = useT()
  const u = r.Upgrade
  if (!u) return <span className={settingsCss.muted}>—</span>
  const tone = upgradeTone(r, u)
  return (
    <>
      <span className={tone === 'bad' ? table.err : tone === 'warn' ? css.warn : undefined}>{upgradeStateText(r, u, t)}</span>
      {tone === 'muted' && !isEmpty(u.Reason) && (
        <div className={settingsCss.muted}>
          <Msg text={u.Reason} />
        </div>
      )}
    </>
  )
}

function RunnersSection({ data }: { data: Data }) {
  return (
    <section aria-labelledby="up-runners" className={settingsCss.section}>
      <Subhead level={2} id="up-runners" description="在 Runner 详情页按台升级到它查到的最新版本；Runner 每小时查询一次，也可以让全部 Runner 立即检查。AI 执行节点不在这里，连接器 kiln-orca 在节点上用 npm 升级。">
        各 Runner
      </Subhead>
      <Box>
        {data.Runners.length > 0 ? (
          <table className={table.table} aria-labelledby="up-runners">
            <thead>
              <tr>
                <th scope="col">Runner</th>
                <th scope="col">版本</th>
                <th scope="col">最新版本</th>
                <th scope="col">升级</th>
              </tr>
            </thead>
            <tbody>
              {data.Runners.map((r) => (
                <tr key={r.ID}>
                  <td data-label="Runner">
                    <a href={r.URL}>
                      <b>{r.Name}</b>
                    </a>
                  </td>
                  <td data-label="版本" className={table.mono} title={r.Version}>
                    {r.Version ? shortVersion(r.Version) : '—'}
                  </td>
                  <td data-label="最新版本">
                    <LatestCell r={r} />
                  </td>
                  <td data-label="升级">
                    <StateCell r={r} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <EmptyState icon={VersionsIcon} title="还没有 Runner" />
        )}
      </Box>
      {data.Runners.length > 0 && (
        <Form action="/settings/runner-upgrade/check" className={css.check} aria-label="全部检查更新">
          <SubmitButton leadingVisual={SyncIcon}>全部检查更新</SubmitButton>
          <span className={settingsCss.muted}>在线的 Runner 下一次连接 Server 时查询最新版本，离线的重新连接后查询。</span>
        </Form>
      )}
    </section>
  )
}

function SettingsUpgrade({ data }: PageProps<Data>) {
  // 有 Runner 正在升级或等待检查时自动刷新
  useLive(data.Runners.some((r) => r.Upgrade && (upgradeTone(r, r.Upgrade) === 'run' || r.Upgrade.CheckPending)))
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="Runner 自己查询 npm 上的最新正式版本；在 Runner 详情页点“升级”后，Runner 下载核对，等正在执行的任务结束再切换，新版本连续启动失败会回滚。Server 的升级是手动更换镜像。">
        Runner 升级
      </Subhead>
      <RegistrySection data={data} />
      <RunnersSection data={data} />
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsUpgrade)

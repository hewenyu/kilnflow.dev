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
import { Msg, Trans, useT } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { formStateSchema, list } from '../lib/envelope'
import { shortVersion } from '../lib/format'
import { FormAlert, TextField, settingsCss, settingsTags, withAgo } from './settings/common'
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
  const t = useT()
  return (
    <section aria-labelledby="up-registry">
      <Subhead level={2} id="up-registry" description={t('upgradeSettings.registryDesc')}>
        npm registry
      </Subhead>
      <Form action="/settings/runner-upgrade/registry" className={settingsCss.form} aria-label="npm registry">
        <FormAlert form={data.Form} />
        <TextField
          form={data.Form}
          name="registry"
          mono
          label={t('upgradeSettings.registryLabel')}
          caption={<Trans id="upgradeSettings.registryCaption" params={{ registry: data.DefaultRegistry }} tags={settingsTags} />}
        />
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{t('settingsForm.save')}</SubmitButton>
        </div>
      </Form>
      <p className={settingsCss.muted}>
        {t('upgradeSettings.current')}
        <span className="mono">{data.Registry || data.DefaultRegistry}</span>
        {data.Registry ? '' : t('upgradeSettings.official')}
      </p>
    </section>
  )
}

function LatestCell({ r }: { r: RunnerView }) {
  const t = useT()
  const u = r.Upgrade
  if (!u) return <span className={settingsCss.muted}>—</span>
  return (
    <>
      {u.Latest ? <span className="mono">{u.Latest}</span> : <span className={settingsCss.muted}>{t('upgradeSettings.notFound')}</span>}
      {u.CheckPending ? (
        <div className={settingsCss.muted}>{t('upgradeSettings.checkPending')}</div>
      ) : (
        u.CheckedAt && (
          <div className={settingsCss.muted}>
            <Trans id="upgradeSettings.checkedAt" tags={withAgo(u.CheckedAt)} />
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
  const t = useT()
  return (
    <section aria-labelledby="up-runners" className={settingsCss.section}>
      <Subhead level={2} id="up-runners" description={t('upgradeSettings.runnersDesc')}>
        {t('upgradeSettings.runners')}
      </Subhead>
      <Box>
        {data.Runners.length > 0 ? (
          <table className={table.table} aria-labelledby="up-runners">
            <thead>
              <tr>
                <th scope="col">Runner</th>
                <th scope="col">{t('upgradeSettings.version')}</th>
                <th scope="col">{t('upgradeSettings.latest')}</th>
                <th scope="col">{t('upgradeSettings.upgrade')}</th>
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
                  <td data-label={t('upgradeSettings.version')} className={table.mono} title={r.Version}>
                    {r.Version ? shortVersion(r.Version) : '—'}
                  </td>
                  <td data-label={t('upgradeSettings.latest')}>
                    <LatestCell r={r} />
                  </td>
                  <td data-label={t('upgradeSettings.upgrade')}>
                    <StateCell r={r} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <EmptyState icon={VersionsIcon} title={t('upgradeSettings.empty')} />
        )}
      </Box>
      {data.Runners.length > 0 && (
        <Form action="/settings/runner-upgrade/check" className={css.check} aria-label={t('upgradeSettings.checkAll')}>
          <SubmitButton leadingVisual={SyncIcon}>{t('upgradeSettings.checkAll')}</SubmitButton>
          <span className={settingsCss.muted}>{t('upgradeSettings.checkAllHint')}</span>
        </Form>
      )}
    </section>
  )
}

function SettingsUpgrade({ data }: PageProps<Data>) {
  const t = useT()
  // 有 Runner 正在升级或等待检查时自动刷新
  useLive(data.Runners.some((r) => r.Upgrade && (upgradeTone(r, r.Upgrade) === 'run' || r.Upgrade.CheckPending)))
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('upgradeSettings.description')}>{t('settingsNav.upgrade')}</Subhead>
      <RegistrySection data={data} />
      <RunnersSection data={data} />
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsUpgrade)

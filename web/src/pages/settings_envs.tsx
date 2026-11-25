import { AlertIcon, StackIcon } from '@primer/octicons-react'
import { Label, Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxRow } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Trans, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import { settingsTags } from './settings/common'
import { envViewSchema, type EnvView } from './settings/schemas'
import css from './settings_envs.module.css'

// 设置 · 环境：environment 列表（GitHub Environments 的样式），标出同名覆盖关系。

const schema = z.object({
  Section: z.string(),
  Envs: list(envViewSchema),
})
type Data = z.infer<typeof schema>

function EnvRow({ e }: { e: EnvView }) {
  const t = useT()
  return (
    <BoxRow className={css.row}>
      <StackIcon className={css.icon} aria-hidden />
      <div>
        <div className={css.title}>
          <Link href={e.URL}>{e.name}</Link>
          <Label variant="secondary">{e.repo ? t('settingsCommon.repoLevel') : t('settingsCommon.accountLevel')}</Label>
          {e.Docker && <Label variant="accent">Docker Hub</Label>}
        </div>
        <p className={css.meta}>
          {e.repo || e.account} · {e.ref_policy.length > 0 ? <code>{e.ref_policy.join(', ')}</code> : t('settingsCommon.refPolicyAny')}
          {e.allowed_publish_repos.length > 0 && ` · ${t('envs.publishTo', { repos: e.allowed_publish_repos.join(', ') })}`}
        </p>
        {e.OverriddenIn.length > 0 && (
          <p className={css.warn}>
            <AlertIcon size={12} aria-hidden />
            <span>{t('envs.overridden', { repos: e.OverriddenIn.join(', ') })}</span>
          </p>
        )}
        {e.Overrides && (
          <p className={css.warn}>
            <AlertIcon size={12} aria-hidden />
            <span>{t('envs.overrides')}</span>
          </p>
        )}
      </div>
    </BoxRow>
  )
}

function SettingsEnvs({ data }: PageProps<Data>) {
  const t = useT()
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={
          <LinkButton href="/settings/environments/new" variant="primary">
            {t('envs.new')}
          </LinkButton>
        }
        description={t('envs.description')}
      >
        {t('settingsNav.envs')}
      </Subhead>
      {data.Envs.length > 0 ? (
        <Box>
          {data.Envs.map((e) => (
            <EnvRow key={e.URL} e={e} />
          ))}
        </Box>
      ) : (
        <Box>
          <EmptyState icon={StackIcon} title={t('envs.emptyTitle')} action={{ label: t('envs.new'), href: '/settings/environments/new' }}>
            <Trans id="envs.emptyText" params={{ name: t('settingsCommon.namePlaceholder') }} tags={settingsTags} />
          </EmptyState>
        </Box>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsEnvs)

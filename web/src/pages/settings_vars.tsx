import { OrganizationIcon, RepoIcon, StackIcon, type Icon } from '@primer/octicons-react'
import { Button, Label, Link, LinkButton, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxRow } from '../components/Box'
import { Form } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Trans, useT } from '../i18n/locale'
import type { MsgKey } from '../i18n/messages'
import { list } from '../lib/envelope'
import { withLink } from './settings/common'
import css from './settings_vars.module.css'

// 设置 · 变量与密钥：先选择作用域（账号级、仓库级、environment 级），再进入作用域配置页。

const scopeCountSchema = z.object({
  Name: z.string(),
  URL: z.string(),
  Vars: z.number(),
  Secrets: z.number(),
  /** Docker Hub 连接使用的账号级 environment。 */
  Docker: z.boolean(),
})
type ScopeCount = z.infer<typeof scopeCountSchema>

const schema = z.object({
  Section: z.string(),
  Accounts: list(scopeCountSchema),
  Repos: list(scopeCountSchema),
  Envs: list(scopeCountSchema),
})
type Data = z.infer<typeof schema>

function CountRow({ c, icon: I, manage }: { c: ScopeCount; icon: Icon; manage: MsgKey }) {
  const t = useT()
  return (
    <BoxRow className={css.row}>
      <span className={css.name}>
        <I />
        <Link href={c.URL}>{c.Name}</Link>
      </span>
      <span className={css.counts}>
        {t('vars.varCount', { count: c.Vars })} · {t('vars.secretCount', { count: c.Secrets })}
      </span>
      <LinkButton href={c.URL} size="small" aria-label={t(manage, { name: c.Name })}>
        {t('vars.manage')}
      </LinkButton>
    </BoxRow>
  )
}

function SettingsVars({ data }: PageProps<Data>) {
  const t = useT()
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('vars.description')}>{t('settingsNav.vars')}</Subhead>

      <section aria-labelledby="acc-title">
        <Subhead level={2} id="acc-title" description={t('vars.accountDesc')}>
          {t('vars.accountTitle')}
        </Subhead>
        <Box>
          {data.Accounts.map((c) => (
            <CountRow key={c.URL} c={c} icon={OrganizationIcon} manage="vars.manageAccount" />
          ))}
          {data.Accounts.length === 0 && <BoxRow className={css.empty}>{t('vars.noAccounts')}</BoxRow>}
        </Box>
      </section>

      <section aria-labelledby="repo-title">
        <Subhead level={2} id="repo-title" spaced>
          {t('vars.repoTitle')}
        </Subhead>
        <Form action="/settings/repos" method="get" className={css.search} aria-label={t('vars.searchRepos')}>
          <TextInput
            type="search"
            name="q"
            aria-label={t('vars.searchRepos')}
            placeholder={t('vars.searchPlaceholder')}
            spellCheck={false}
            autoCapitalize="off"
          />
          <Button type="submit">{t('settingsForm.search')}</Button>
        </Form>
        <p className={css.hint}>{t('vars.repoHint')}</p>
        <Box>
          {data.Repos.map((c) => (
            <CountRow key={c.URL} c={c} icon={RepoIcon} manage="vars.manageRepo" />
          ))}
          {data.Repos.length === 0 && <BoxRow className={css.empty}>{t('vars.noRepos')}</BoxRow>}
        </Box>
      </section>

      <section aria-labelledby="env-title">
        <Subhead
          level={2}
          id="env-title"
          spaced
          actions={
            <LinkButton href="/settings/environments/new" size="small">
              {t('envs.new')}
            </LinkButton>
          }
          description={
            <Trans id="vars.envDesc" params={{ name: t('settingsCommon.namePlaceholder') }} tags={withLink('/settings/environments')} />
          }
        >
          {t('vars.envTitle')}
        </Subhead>
        <Box>
          {data.Envs.map((c) => (
            <BoxRow key={c.URL} className={css.row}>
              <span className={css.name}>
                <StackIcon />
                <Link href={c.URL}>{c.Name}</Link>
                {c.Docker && <Label variant="accent">Docker Hub</Label>}
              </span>
              {c.Docker && (
                <span className={css.note}>
                  <Trans id="vars.dockerNote" tags={withLink('/settings')} />
                </span>
              )}
            </BoxRow>
          ))}
          {data.Envs.length === 0 && <BoxRow className={css.empty}>{t('vars.noEnvs')}</BoxRow>}
        </Box>
      </section>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsVars)

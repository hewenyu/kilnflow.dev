import { PencilIcon, TrashIcon } from '@primer/octicons-react'
import { IconButton, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxBody, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { sectionLabel } from '../components/SettingsLayout'
import { Trans, useT } from '../i18n/locale'
import { formStateSchema, type FormState } from '../lib/envelope'
import {
  Crumbs,
  DangerItem,
  DangerZone,
  EnvRuleFields,
  FormAlert,
  ScopeFields,
  exprRef,
  scopeKindLabel,
  settingsCss,
  settingsTags,
  withLink,
} from './settings/common'
import { itemURL, scopePageSchema, type EnvView, type ItemKind, type ScopePage } from './settings/schemas'
import css from './settings_scope.module.css'

// 作用域配置页（账号级、仓库级、environment 级）：environment 的保护规则、变量和 secret 列表。
// secret 只列出更新信息，从不显示值。（账号级 npm 配置在“连接与发布凭据”中，见 settings_npm.tsx。）

const schema = scopePageSchema.extend({
  /** environment 保护规则表单（校验失败时带着输入和错误）。 */
  EnvForm: formStateSchema,
})
type Data = z.infer<typeof schema>

function EnvNotes({ env }: { env: EnvView }) {
  const t = useT()
  return (
    <>
      {env.OverriddenIn.length > 0 && (
        <Callout tone="warning" title={t('scope.overriddenTitle')}>
          <Trans id="scope.overridden" params={{ env: env.name, repos: env.OverriddenIn.join(', ') }} tags={settingsTags} />
        </Callout>
      )}
      {env.Overrides && (
        <Callout tone="warning" title={t('scope.overridesTitle')}>
          <Trans id="scope.overrides" params={{ env: env.name }} tags={settingsTags} />
        </Callout>
      )}
    </>
  )
}

function Rules({ p, env, form }: { p: ScopePage; env: EnvView; form: FormState }) {
  const t = useT()
  return (
    <Box as="section" aria-labelledby="rules-title">
      <BoxHeader id="rules-title" title={t('scope.rules')} />
      <BoxBody className={css.rules}>
        <dl className={settingsCss.kv}>
          <dt>{t('envRules.refs')}</dt>
          <dd>{env.ref_policy.length > 0 ? <code>{env.ref_policy.join(', ')}</code> : t('scope.refsAny')}</dd>
          <dt>{t('scope.publishTo')}</dt>
          <dd>{env.allowed_publish_repos.length > 0 ? env.allowed_publish_repos.join(', ') : t('scope.publishSource')}</dd>
        </dl>
        <details className={settingsCss.details} open={form?.Failed}>
          <summary>{t('scope.editRules')}</summary>
          <div className={settingsCss.detailsBody}>
            <Form action="/settings/environments" className={settingsCss.form} aria-label={t('scope.editRules')}>
              <ScopeFields account={p.Account} repo={p.Repo} env={p.Env} />
              <input type="hidden" name="name" value={env.name} />
              <FormAlert form={form} />
              <EnvRuleFields form={form} />
              <div className={settingsCss.actions}>
                <SubmitButton variant="primary">{t('scope.saveRules')}</SubmitButton>
                <LinkButton href={p.URL}>{t('form.cancel')}</LinkButton>
              </div>
            </Form>
          </div>
        </details>
      </BoxBody>
    </Box>
  )
}

/** DeleteItem 是列表中的删除按钮（确认后提交）。 */
function DeleteItem({ p, kind, name }: { p: ScopePage; kind: ItemKind; name: string }) {
  const t = useT()
  const secret = kind === 'secrets'
  return (
    <ConfirmForm
      action={`/settings/${kind}/delete`}
      fields={{ account: p.Account, repo: p.Repo, env: p.Env, name, confirm: '1' }}
      icon={TrashIcon}
      button={t(secret ? 'scope.deleteSecret' : 'scope.deleteVar', { name })}
      buttonProps={{ className: settingsCss.iconDanger }}
      title={t(secret ? 'scope.deleteSecretConfirm' : 'scope.deleteVarConfirm', { name })}
      confirmLabel={t(secret ? 'scope.deleteSecretLabel' : 'scope.deleteVarLabel')}
    >
      <Trans
        id={secret ? 'scope.deleteSecretBody' : 'scope.deleteVarBody'}
        params={{ scope: p.Label, ref: `${secret ? 'secrets' : 'vars'}.${name}` }}
        tags={settingsTags}
      />
    </ConfirmForm>
  )
}

function Variables({ p }: { p: ScopePage }) {
  const t = useT()
  return (
    <section aria-labelledby="vars-title">
      <Subhead
        level={2}
        id="vars-title"
        spaced
        actions={
          <LinkButton href={itemURL({ query: p.Query, kind: 'variables' })} size="small">
            {t('scope.newVar')}
          </LinkButton>
        }
        description={
          <Trans id="scope.varsDesc" params={{ ref: exprRef('vars', t('settingsCommon.namePlaceholder')) }} tags={settingsTags} />
        }
      >
        {t('scope.vars')}
      </Subhead>
      <Box>
        {p.Vars.map((v) => (
          <BoxRow key={v.Name} className={css.item}>
            <span className={css.itemName}>{v.Name}</span>
            <span className={css.itemValue} title={v.Value}>
              {v.Value}
            </span>
            <span className={css.itemActions}>
              <IconButton
                as="a"
                href={itemURL({ query: p.Query, kind: 'variables', name: v.Name })}
                icon={PencilIcon}
                variant="invisible"
                size="small"
                aria-label={t('scope.editVar', { name: v.Name })}
              />
              <DeleteItem p={p} kind="variables" name={v.Name} />
            </span>
          </BoxRow>
        ))}
        {p.Vars.length === 0 && <BoxRow className={css.empty}>{t('scope.noVars')}</BoxRow>}
      </Box>
    </section>
  )
}

function Secrets({ p }: { p: ScopePage }) {
  const t = useT()
  return (
    <section aria-labelledby="secrets-title">
      <Subhead
        level={2}
        id="secrets-title"
        spaced
        actions={
          <LinkButton href={itemURL({ query: p.Query, kind: 'secrets' })} size="small">
            {t('scope.newSecret')}
          </LinkButton>
        }
        description={
          <Trans id="scope.secretsDesc" params={{ ref: exprRef('secrets', t('settingsCommon.namePlaceholder')) }} tags={settingsTags} />
        }
      >
        {t('scope.secrets')}
      </Subhead>
      <Box>
        {p.Secrets.map((s) => (
          <BoxRow key={s.Name} className={css.item}>
            <span className={css.itemName}>{s.Name}</span>
            <span className={css.itemMeta}>
              <Ago t={s.UpdatedAt} />
              {s.UpdatedBy && t('settingsCommon.updatedBy', { login: s.UpdatedBy })}
            </span>
            <span className={css.itemActions}>
              <IconButton
                as="a"
                href={itemURL({ query: p.Query, kind: 'secrets', name: s.Name })}
                icon={PencilIcon}
                variant="invisible"
                size="small"
                aria-label={t('scope.replaceSecret', { name: s.Name })}
              />
              <DeleteItem p={p} kind="secrets" name={s.Name} />
            </span>
          </BoxRow>
        ))}
        {p.Secrets.length === 0 && <BoxRow className={css.empty}>{t('scope.noSecrets')}</BoxRow>}
      </Box>
    </section>
  )
}

function SettingsScope({ data }: PageProps<Data>) {
  const t = useT()
  const env = data.Environment
  const crumb = sectionLabel(data.Section)
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t(scopeKindLabel(data.Kind, data.Repo))}>
        <Crumbs items={crumb ? [{ label: t(crumb), href: data.CrumbURL }] : []} current={data.Label} />
      </Subhead>
      <div className={css.notes}>
        {data.Docker && (
          <Callout title={t('scope.dockerTitle')} hideTitle>
            <Trans id="scope.dockerNote" tags={withLink(data.DockerURL)} />
          </Callout>
        )}
        {env && <EnvNotes env={env} />}
      </div>
      {env && <Rules p={data} env={env} form={data.EnvForm} />}
      <Variables p={data} />
      <Secrets p={data} />
      <details className={`${settingsCss.details} ${css.help}`}>
        <summary>{t('scope.help')}</summary>
        <div className={settingsCss.detailsBody}>
          <p>
            <Trans id="scope.help1" params={{ name: t('settingsCommon.namePlaceholder') }} tags={settingsTags} />
          </p>
          <p>{t('scope.help2')}</p>
          <p>
            <Trans id="scope.help3" tags={settingsTags} />
          </p>
        </div>
      </details>
      {data.Env && (
        <DangerZone>
          <DangerItem
            title={t('scope.deleteEnv', { env: data.Env })}
            action="/settings/environments/delete"
            fields={{ account: data.Account, repo: data.Repo, env: data.Env, confirm: '1' }}
            button={t('scope.deleteEnvButton')}
            confirmTitle={t('scope.deleteEnvConfirm', { env: data.Env })}
            confirmLabel={t('scope.deleteEnvButton')}
            confirmBody={t('scope.deleteEnvBody', { vars: data.Vars.length, secrets: data.Secrets.length })}
          >
            {t('scope.deleteEnvText', { vars: data.Vars.length, secrets: data.Secrets.length })}
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsScope)

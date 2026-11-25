import { FormControl, Link, LinkButton, Select } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { fieldError, fieldValue, formStateSchema, list } from '../lib/envelope'
import { Crumbs, EnvRuleFields, FormAlert, TextField, settingsCss, settingsTags } from './settings/common'
import css from './settings_env_new.module.css'
import { Msg, Trans, useT } from '../i18n/locale'

// 新建 environment：作用域（账号级，或从仓库选择器选中的仓库）、名称和可选的保护规则。

const schema = z.object({
  Section: z.string(),
  Accounts: list(z.string()),
  /** 从仓库选择器选中的仓库（新建仓库级 environment），为空时只能选账号级。 */
  Repo: z.string(),
  Form: formStateSchema,
})
type Data = z.infer<typeof schema>

function SettingsEnvNew({ data }: PageProps<Data>) {
  const t = useT()
  const form = data.Form
  const scopeErr = fieldError(form, 'scope')
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('envNew.description')}>
        <Crumbs items={[{ label: t('settingsNav.envs'), href: '/settings/environments' }]} current={t('envs.new')} />
      </Subhead>
      <Form action="/settings/environments" className={settingsCss.form} aria-label={t('envs.new')}>
        <input type="hidden" name="create" value="1" />
        <FormAlert form={form} />
        <FormControl>
          <FormControl.Label>{t('envNew.scope')}</FormControl.Label>
          <Select
            name="scope"
            block
            defaultValue={fieldValue(form, 'scope')}
            validationStatus={scopeErr ? 'error' : undefined}
            aria-invalid={scopeErr ? true : undefined}
          >
            {data.Accounts.map((a) => (
              <Select.Option key={a} value={`account:${a}`}>
                {t('envNew.accountOption', { account: a })}
              </Select.Option>
            ))}
            {data.Repo && <Select.Option value={`repo:${data.Repo}`}>{t('envNew.repoOption', { repo: data.Repo })}</Select.Option>}
          </Select>
          <FormControl.Caption>
            {t('envNew.repoHint')} <Link href="/settings/repos?pick=env">{data.Repo ? t('envNew.changeRepo') : t('envNew.pickRepo')}</Link>
          </FormControl.Caption>
          {scopeErr && <FormControl.Validation variant="error"><Msg text={scopeErr} /></FormControl.Validation>}
        </FormControl>
        <TextField
          form={form}
          name="name"
          required
          label={t('settingsForm.name')}
          caption={<Trans id="envNew.nameCaption" params={{ name: t('settingsCommon.namePlaceholder') }} tags={settingsTags} />}
        />
        <fieldset className={css.fieldset}>
          <legend className={css.legend}>{t('envNew.rules')}</legend>
          <EnvRuleFields form={form} />
        </fieldset>
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{t('envNew.submit')}</SubmitButton>
          <LinkButton href="/settings/environments">{t('form.cancel')}</LinkButton>
        </div>
      </Form>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsEnvNew)

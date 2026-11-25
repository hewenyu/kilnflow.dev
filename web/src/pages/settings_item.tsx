import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Callout } from '../components/Callout'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout, sectionLabel } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { Trans, useT } from '../i18n/locale'
import type { MsgKey } from '../i18n/messages'
import { formStateSchema } from '../lib/envelope'
import {
  Crumbs,
  DangerItem,
  DangerZone,
  FormAlert,
  ScopeFields,
  TextField,
  exprRef,
  scopeKindLabel,
  settingsCss,
  settingsTags,
  withLink,
} from './settings/common'
import { dockerItems, scopePageSchema, secretMetaSchema } from './settings/schemas'

// 变量或 secret 的新建、编辑页。secret 的值只写不读：页面从不显示或回填它，替换时留空则保持不变。

const schema = scopePageSchema.extend({
  Kind: z.enum(['variables', 'secrets']),
  /** 作用域的类型（scopePage 的 Kind 被条目的种类占用）。 */
  ScopeKind: scopePageSchema.shape.Kind,
  /** 为空表示新建。 */
  Name: z.string(),
  /** 被替换的 secret 的元数据（只用于编辑 secret）。 */
  Secret: secretMetaSchema.nullable(),
  Form: formStateSchema,
  IsSecret: z.boolean(),
})
type Data = z.infer<typeof schema>

/** itemTitle 返回页面标题的键：新建变量、编辑变量、新建 secret、替换 secret。 */
function itemTitle(d: Pick<Data, 'Name' | 'IsSecret'>): MsgKey {
  if (d.Name === '') return d.IsSecret ? 'item.newSecret' : 'item.newVar'
  return d.IsSecret ? 'item.replaceSecret' : 'item.editVar'
}

/** submitLabel 返回提交按钮的键：创建变量、创建 secret、替换、保存修改。 */
function submitLabel(d: Pick<Data, 'Name' | 'IsSecret'>): MsgKey {
  if (d.Name === '') return d.IsSecret ? 'item.createSecret' : 'item.createVar'
  return d.IsSecret ? 'item.replace' : 'item.saveChanges'
}

function SettingsItem({ data }: PageProps<Data>) {
  const t = useT()
  const secret = data.IsSecret
  const ref = secret ? 'secrets' : 'vars'
  const editing = data.Name !== ''
  const form = data.Form
  const title = t(itemTitle(data))
  const crumb = sectionLabel(data.Section)
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        description={
          <Trans id="item.scopeDesc" params={{ scope: data.Label, kind: t(scopeKindLabel(data.ScopeKind, data.Repo)) }} tags={withLink(data.URL)} />
        }
      >
        <Crumbs
          items={[...(crumb ? [{ label: t(crumb), href: data.CrumbURL }] : []), { label: data.Label, href: data.URL }]}
          current={
            editing ? (
              <>
                {title} <code>{data.Name}</code>
              </>
            ) : (
              title
            )
          }
        />
      </Subhead>
      {data.Docker && dockerItems.includes(data.Name) && (
        <Callout title={t('scope.dockerTitle')} hideTitle>
          <Trans id="item.dockerNote" tags={withLink(data.DockerURL)} />
        </Callout>
      )}

      <Form action={`/settings/${data.Kind}`} className={settingsCss.form} aria-label={title}>
        <ScopeFields account={data.Account} repo={data.Repo} env={data.Env} />
        <input type="hidden" name="mode" value={editing ? 'edit' : 'new'} />
        <FormAlert form={form} />
        {editing ? (
          <TextField form={form} name="name" label={t('settingsForm.name')} value={data.Name} readOnly mono caption={t('item.nameFixed')} />
        ) : (
          <TextField
            form={form}
            name="name"
            label={t('settingsForm.name')}
            required
            mono
            caption={<Trans id="item.nameCaption" params={{ ref: exprRef(ref, t('settingsCommon.namePlaceholder')) }} tags={settingsTags} />}
          />
        )}
        {secret ? (
          <div>
            {data.Secret && (
              <p className={settingsCss.secretMeta}>
                {t('item.secretCurrent')} · <Ago t={data.Secret.UpdatedAt} />
                {data.Secret.UpdatedBy && t('settingsCommon.updatedBy', { login: data.Secret.UpdatedBy })}
                {t('item.secretUnreadable')}
              </p>
            )}
            <TextField
              form={form}
              name="value"
              type="password"
              required={!editing}
              label={editing ? t('item.newValue') : t('item.value')}
              caption={editing ? t('item.replaceCaption') : t('item.newCaption')}
            />
          </div>
        ) : (
          <TextField form={form} name="value" label={t('item.value')} multiline mono caption={t('item.varValueCaption')} />
        )}
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{t(submitLabel(data))}</SubmitButton>
          <LinkButton href={data.URL}>{t('form.cancel')}</LinkButton>
        </div>
      </Form>

      {editing && (
        <DangerZone>
          <DangerItem
            title={t(secret ? 'scope.deleteSecret' : 'scope.deleteVar', { name: data.Name })}
            action={`/settings/${data.Kind}/delete`}
            fields={{ account: data.Account, repo: data.Repo, env: data.Env, name: data.Name, confirm: '1' }}
            button={t(secret ? 'scope.deleteSecretLabel' : 'scope.deleteVarLabel')}
            confirmTitle={t(secret ? 'scope.deleteSecretConfirm' : 'scope.deleteVarConfirm', { name: data.Name })}
            confirmLabel={t(secret ? 'scope.deleteSecretLabel' : 'scope.deleteVarLabel')}
            confirmBody={t('scope.deleteShort', { scope: data.Label })}
          >
            <Trans
              id={secret ? 'scope.deleteSecretBody' : 'scope.deleteVarBody'}
              params={{ scope: data.Label, ref: `${ref}.${data.Name}` }}
              tags={settingsTags}
            />
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsItem)

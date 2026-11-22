import { Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Callout } from '../components/Callout'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { formStateSchema } from '../lib/envelope'
import { Crumbs, DangerItem, DangerZone, FormAlert, ScopeFields, TextField, settingsCss } from './settings/common'
import { dockerItems, scopePageSchema, secretMetaSchema } from './settings/schemas'

// 变量或 secret 的新建、编辑页。secret 的值只写不读：页面从不显示或回填它，替换时留空则保持不变。

const schema = scopePageSchema.extend({
  Kind: z.enum(['variables', 'secrets']),
  /** 为空表示新建。 */
  Name: z.string(),
  /** 被替换的 secret 的元数据（只用于编辑 secret）。 */
  Secret: secretMetaSchema.nullable(),
  Form: formStateSchema,
  IsSecret: z.boolean(),
})
type Data = z.infer<typeof schema>

/** itemTitle 返回页面标题：新建变量、编辑变量、新建 secret、替换 secret。 */
function itemTitle(d: Pick<Data, 'Name' | 'IsSecret'>): string {
  if (d.Name === '') return d.IsSecret ? '新建 secret' : '新建变量'
  return d.IsSecret ? '替换 secret' : '编辑变量'
}

function SettingsItem({ data }: PageProps<Data>) {
  // 中文和英文词之间留空格：删除变量、删除 secret
  const what = data.IsSecret ? ' secret' : '变量'
  const ref = data.IsSecret ? 'secrets' : 'vars'
  const editing = data.Name !== ''
  const form = data.Form
  const submitLabel = !editing ? `创建${what}` : data.IsSecret ? '替换' : '保存修改'
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        description={
          <>
            作用域：<Link href={data.URL}>{data.Label}</Link>（{data.KindLabel}）
          </>
        }
      >
        <Crumbs
          items={[
            { label: data.CrumbLabel, href: data.CrumbURL },
            { label: data.Label, href: data.URL },
          ]}
          current={
            editing ? (
              <>
                {itemTitle(data)} <code>{data.Name}</code>
              </>
            ) : (
              itemTitle(data)
            )
          }
        />
      </Subhead>
      {data.Docker && dockerItems.includes(data.Name) && (
        <Callout title="Docker Hub 连接" hideTitle>
          这一项属于 Docker Hub 连接，更推荐在 <Link href={data.DockerURL}>Docker Hub 连接</Link> 中修改。
        </Callout>
      )}

      <Form action={`/settings/${data.Kind}`} className={settingsCss.form} aria-label={itemTitle(data)}>
        <ScopeFields account={data.Account} repo={data.Repo} env={data.Env} />
        <input type="hidden" name="mode" value={editing ? 'edit' : 'new'} />
        <FormAlert form={form} />
        {editing ? (
          <TextField form={form} name="name" label="名称" value={data.Name} readOnly mono caption="名称不能修改；要改名请新建一个，再删除这个。" />
        ) : (
          <TextField
            form={form}
            name="name"
            label="名称"
            required
            mono
            caption={
              <>
                字母、数字和 _，例如 <code>DEPLOY_HOST</code>。workflow 中用 <code>{`\${{ ${ref}.名称 }}`}</code> 引用。
              </>
            }
          />
        )}
        {data.IsSecret ? (
          <div>
            {data.Secret && (
              <p className={settingsCss.secretMeta}>
                当前值已设置 · <Ago t={data.Secret.UpdatedAt} />
                {data.Secret.UpdatedBy && ` 由 ${data.Secret.UpdatedBy} 更新`}。值保存后不能再读出。
              </p>
            )}
            <TextField
              form={form}
              name="value"
              type="password"
              required={!editing}
              label={editing ? '新值' : '值'}
              caption={editing ? '保存后替换当前值；留空则保持不变。' : '保存后不会再显示。'}
            />
          </div>
        ) : (
          <TextField form={form} name="value" label="值" multiline mono caption="值不保密，会显示在设置页中。" />
        )}
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{submitLabel}</SubmitButton>
          <LinkButton href={data.URL}>取消</LinkButton>
        </div>
      </Form>

      {editing && (
        <DangerZone>
          <DangerItem
            title={`删除${what} ${data.Name}`}
            action={`/settings/${data.Kind}/delete`}
            fields={{ account: data.Account, repo: data.Repo, env: data.Env, name: data.Name, confirm: '1' }}
            button={`删除${what}`}
            confirmTitle={`删除${what} ${data.Name}？`}
            confirmLabel={`删除${what}`}
            confirmBody={`从 ${data.Label} 中删除，无法恢复。`}
          >
            从 {data.Label} 中删除，无法恢复。引用 <code>{`${ref}.${data.Name}`}</code> 的 job 将得到空值（如果更宽的作用域中有同名的{what.trim()}，会改用那一个）。
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsItem)

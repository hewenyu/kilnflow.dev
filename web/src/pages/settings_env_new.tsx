import { FormControl, Link, LinkButton, Select } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { fieldError, fieldValue, formStateSchema, list } from '../lib/envelope'
import { Crumbs, FormAlert, TextField, settingsCss } from './settings/common'
import css from './settings_env_new.module.css'

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
  const form = data.Form
  const scopeErr = fieldError(form, 'scope')
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="账号级 environment 由账号下的所有仓库共用；只有某个仓库需要不同的规则或凭据时才建仓库级的。">
        <Crumbs items={[{ label: '环境', href: '/settings/environments' }]} current="新建 environment" />
      </Subhead>
      <Form action="/settings/environments" className={settingsCss.form} aria-label="新建 environment">
        <input type="hidden" name="create" value="1" />
        <FormAlert form={form} />
        <FormControl>
          <FormControl.Label>作用域</FormControl.Label>
          <Select
            name="scope"
            block
            defaultValue={fieldValue(form, 'scope')}
            validationStatus={scopeErr ? 'error' : undefined}
            aria-invalid={scopeErr ? true : undefined}
          >
            {data.Accounts.map((a) => (
              <Select.Option key={a} value={`account:${a}`}>
                {a}（账号级，账号下所有仓库共用）
              </Select.Option>
            ))}
            {data.Repo && <Select.Option value={`repo:${data.Repo}`}>{data.Repo}（仓库级）</Select.Option>}
          </Select>
          <FormControl.Caption>
            要建在某个仓库上？<Link href="/settings/repos?pick=env">{data.Repo ? '换一个仓库' : '搜索并选择仓库'}</Link>
          </FormControl.Caption>
          {scopeErr && <FormControl.Validation variant="error">{scopeErr}</FormControl.Validation>}
        </FormControl>
        <TextField
          form={form}
          name="name"
          required
          label="名称"
          caption={
            <>
              job 中写 <code>environment: 名称</code> 引用，例如 <code>release</code>。
            </>
          }
        />
        <fieldset className={css.fieldset}>
          <legend className={css.legend}>保护规则（可选，创建后也可以修改）</legend>
          <TextField
            form={form}
            name="ref_policy"
            multiline
            rows={2}
            mono
            label="允许的 ref"
            caption={
              <>
                每行或逗号分隔一个，例如 <code>refs/tags/v*</code>；留空表示不限制。
              </>
            }
          />
          <TextField form={form} name="allowed_publish_repos" label="允许发布到的仓库" caption="owner/name，逗号分隔；留空表示只能发布到源仓库。" />
        </fieldset>
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">创建 environment</SubmitButton>
          <LinkButton href="/settings/environments">取消</LinkButton>
        </div>
      </Form>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsEnvNew)

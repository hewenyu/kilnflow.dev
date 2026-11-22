import { PencilIcon, TrashIcon } from '@primer/octicons-react'
import { IconButton, Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxBody, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { formStateSchema, type FormState } from '../lib/envelope'
import { Crumbs, DangerItem, DangerZone, FormAlert, ScopeFields, TextField, settingsCss } from './settings/common'
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
  return (
    <>
      {env.OverriddenIn.length > 0 && (
        <Callout tone="warning" title="被仓库级 environment 整体覆盖">
          以下仓库定义了同名的仓库级 environment <code>{env.name}</code>，会<strong>整体覆盖</strong>这里的配置：{env.OverriddenIn.join(', ')}
          。这些仓库的 job 不会使用这里的 ref 策略、变量和 secret；需要的值必须在仓库级 environment 中单独配置。
        </Callout>
      )}
      {env.Overrides && (
        <Callout tone="warning" title="整体覆盖账号级同名 environment">
          这个仓库级 environment <strong>整体覆盖</strong>账号级同名的 <code>{env.name}</code>：该仓库的 job 不会继承账号级{' '}
          <code>{env.name}</code> 中的任何配置、变量或 secret，需要的值必须在这里重新配置。
        </Callout>
      )}
    </>
  )
}

function Rules({ p, env, form }: { p: ScopePage; env: EnvView; form: FormState }) {
  return (
    <Box as="section" aria-labelledby="rules-title">
      <BoxHeader id="rules-title" title="保护规则" />
      <BoxBody className={css.rules}>
        <dl className={settingsCss.kv}>
          <dt>允许的 ref</dt>
          <dd>{env.ref_policy.length > 0 ? <code>{env.ref_policy.join(', ')}</code> : '不限制'}</dd>
          <dt>允许发布到</dt>
          <dd>{env.allowed_publish_repos.length > 0 ? env.allowed_publish_repos.join(', ') : '只发布到源仓库'}</dd>
        </dl>
        <details className={settingsCss.details} open={form?.Failed}>
          <summary>编辑保护规则</summary>
          <div className={settingsCss.detailsBody}>
            <Form action="/settings/environments" className={settingsCss.form} aria-label="编辑保护规则">
              <ScopeFields account={p.Account} repo={p.Repo} env={p.Env} />
              <input type="hidden" name="name" value={env.name} />
              <FormAlert form={form} />
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
              <div className={settingsCss.actions}>
                <SubmitButton variant="primary">保存保护规则</SubmitButton>
                <LinkButton href={p.URL}>取消</LinkButton>
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
  // 中文和英文词之间留空格：删除变量、删除 secret
  const what = kind === 'secrets' ? ' secret' : '变量'
  return (
    <ConfirmForm
      action={`/settings/${kind}/delete`}
      fields={{ account: p.Account, repo: p.Repo, env: p.Env, name, confirm: '1' }}
      icon={TrashIcon}
      button={`删除${what} ${name}`}
      buttonProps={{ className: settingsCss.iconDanger }}
      title={`删除${what} ${name}？`}
      confirmLabel={`删除${what}`}
    >
      从 {p.Label} 中删除，无法恢复。引用{' '}
      <code>
        {kind === 'secrets' ? 'secrets' : 'vars'}.{name}
      </code>{' '}
      的 job 将得到空值（如果更宽的作用域中有同名的{what.trim()}，会改用那一个）。
    </ConfirmForm>
  )
}

function Variables({ p }: { p: ScopePage }) {
  return (
    <section aria-labelledby="vars-title">
      <Subhead
        level={2}
        id="vars-title"
        spaced
        actions={
          <LinkButton href={itemURL({ query: p.Query, kind: 'variables' })} size="small">
            新建变量
          </LinkButton>
        }
        description={
          <>
            workflow 中用 <code>{'${{ vars.名称 }}'}</code> 引用。变量的值不保密，会显示在这里并计入 action key。
          </>
        }
      >
        变量
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
                aria-label={`编辑变量 ${v.Name}`}
              />
              <DeleteItem p={p} kind="variables" name={v.Name} />
            </span>
          </BoxRow>
        ))}
        {p.Vars.length === 0 && <BoxRow className={css.empty}>没有变量</BoxRow>}
      </Box>
    </section>
  )
}

function Secrets({ p }: { p: ScopePage }) {
  return (
    <section aria-labelledby="secrets-title">
      <Subhead
        level={2}
        id="secrets-title"
        spaced
        actions={
          <LinkButton href={itemURL({ query: p.Query, kind: 'secrets' })} size="small">
            新建 secret
          </LinkButton>
        }
        description={
          <>
            workflow 中用 <code>{'${{ secrets.名称 }}'}</code> 引用。值保存后不能再读出，这里只显示名称和更新记录。
          </>
        }
      >
        Secret
      </Subhead>
      <Box>
        {p.Secrets.map((s) => (
          <BoxRow key={s.Name} className={css.item}>
            <span className={css.itemName}>{s.Name}</span>
            <span className={css.itemMeta}>
              <Ago t={s.UpdatedAt} />
              {s.UpdatedBy && ` 由 ${s.UpdatedBy} 更新`}
            </span>
            <span className={css.itemActions}>
              <IconButton
                as="a"
                href={itemURL({ query: p.Query, kind: 'secrets', name: s.Name })}
                icon={PencilIcon}
                variant="invisible"
                size="small"
                aria-label={`替换 secret ${s.Name}`}
              />
              <DeleteItem p={p} kind="secrets" name={s.Name} />
            </span>
          </BoxRow>
        ))}
        {p.Secrets.length === 0 && <BoxRow className={css.empty}>没有 secret</BoxRow>}
      </Box>
    </section>
  )
}

function SettingsScope({ data }: PageProps<Data>) {
  const env = data.Environment
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={data.KindLabel}>
        <Crumbs items={[{ label: data.CrumbLabel, href: data.CrumbURL }]} current={data.Label} />
      </Subhead>
      <div className={css.notes}>
        {data.Docker && (
          <Callout title="Docker Hub 连接" hideTitle>
            这是 Docker Hub 连接使用的 environment。修改 Docker Hub 用户名、命名空间或访问令牌，请到{' '}
            <Link href={data.DockerURL}>Docker Hub 连接</Link>，不用在下面的列表中操作。
          </Callout>
        )}
        {env && <EnvNotes env={env} />}
      </div>
      {env && <Rules p={data} env={env} form={data.EnvForm} />}
      <Variables p={data} />
      <Secrets p={data} />
      <details className={`${settingsCss.details} ${css.help}`}>
        <summary>作用域与覆盖规则</summary>
        <div className={settingsCss.detailsBody}>
          <p>
            变量和 secret 按 environment 级 &gt; 仓库级 &gt; 账号级合并，同名时更具体的作用域生效。账号级 environment 由账号下的所有仓库共用，job 写{' '}
            <code>environment: 名称</code> 即可引用。
          </p>
          <p>environment 本身不合并：某个仓库定义了与账号级同名的 environment 时，这个仓库的 job 只使用仓库级的那一份，不会继承账号级同名 environment 中的任何配置、变量或 secret。</p>
          <p>
            也可以用 CLI 管理：<code>kiln env set</code>、<code>kiln var set</code>、<code>kiln secret set</code>
            。
          </p>
        </div>
      </details>
      {data.Env && (
        <DangerZone>
          <DangerItem
            title={`删除 environment ${data.Env}`}
            action="/settings/environments/delete"
            fields={{ account: data.Account, repo: data.Repo, env: data.Env, confirm: '1' }}
            button="删除 environment"
            confirmTitle={`删除 environment ${data.Env}？`}
            confirmLabel="删除 environment"
            confirmBody={`同时删除其中的 ${data.Vars.length} 个变量和 ${data.Secrets.length} 个 secret，无法恢复。`}
          >
            同时删除其中的 {data.Vars.length} 个变量和 {data.Secrets.length} 个 secret，无法恢复。引用它的 job 将无法运行，直到重新创建。
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsScope)

import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { formStateSchema } from '../lib/envelope'
import { Crumbs, DangerItem, DangerZone, FormAlert, NpmRows, NpmStatusLabel, TextField, settingsCss } from './settings/common'
import { npmViewSchema } from './settings/schemas'
import css from './settings_npm.module.css'

// 设置 · npm：账号级的 scope、registry 和 token（与 Docker Hub 连接平级）。token 只写不读，每次保存整体替换，要重新填写。

const schema = z.object({
  Section: z.string(),
  Npm: npmViewSchema,
  /** 校验失败时带着 scope、registry 和错误，token 从不回填。 */
  Form: formStateSchema,
  /** registry 留空时使用的 npm 官方 registry。 */
  DefaultRegistry: z.string(),
})
type Data = z.infer<typeof schema>

const workflowExample = `  publish:
    needs: [build]
    environment: release
    release:
      npm:
        - from: build.bin
          name: mytool          # 发布为 @<scope>/mytool
          access: public`

function SettingsNpm({ data }: PageProps<Data>) {
  const v = data.Npm
  const form = data.Form
  const configured = v.Config !== null
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={<NpmStatusLabel v={v} />}
        description={
          <>
            workflow 中 <code>release.npm</code> 的包用这里的 scope、registry 和 token 发布。{v.Account} 下的所有仓库共用这一份。
          </>
        }
      >
        <Crumbs items={[{ label: '连接与发布凭据', href: '/settings' }]} current={`npm · ${v.Account}`} />
      </Subhead>

      {configured ? (
        <dl className={`${settingsCss.kv} ${css.summary}`}>
          <NpmRows v={v} />
        </dl>
      ) : (
        <ol className={css.steps}>
          <li>
            在 npmjs.com 创建一个对这个 scope 有读写权限、能免 OTP 发布的 token（granular access token 勾选 bypass 2FA，或 Automation token）。
          </li>
          <li>填写 scope 和 token 并保存。保存只把配置存进 Kiln，不会访问 registry。</li>
          <li>
            发布 job 中 <code>release.npm</code> 列出的包发布为 <code>@&lt;scope&gt;/&lt;name&gt;</code>：name 写在 workflow 中，凭据只在这里设置。
          </li>
        </ol>
      )}

      <Form action="/settings/npm" className={settingsCss.form} aria-label="npm 配置">
        <input type="hidden" name="account" value={v.Account} />
        <FormAlert form={form} />
        <TextField
          form={form}
          name="scope"
          required
          mono
          label="scope"
          caption={
            <>
              npm 的用户名或组织名，可以不写 @。包发布为 <code>@scope/&lt;name&gt;</code>。
            </>
          }
        />
        <TextField
          form={form}
          name="registry"
          mono
          label="registry"
          caption={
            <>
              留空使用 npm 官方 registry <code>{data.DefaultRegistry}</code>。私有 registry 可以带路径前缀，必须是 https。
            </>
          }
        />
        <TextField
          form={form}
          name="token"
          type="password"
          required
          label="token"
          caption={
            configured
              ? '已保存的 token 不能读出：每次保存都整体替换 scope、registry 和 token，需要重新填写 token。保存后不会再显示。'
              : '需要有这个 scope 的发布权限，并能免 OTP 发布。保存后不会再显示。'
          }
        />
        <details className={settingsCss.details}>
          <summary>workflow 写法与 CLI</summary>
          <div className={settingsCss.detailsBody}>
            <p>发布 job 中这样写（账号下的任意仓库），不写 registry 和 token：</p>
            <pre className={settingsCss.pre}>{workflowExample}</pre>
            <p>
              也可以用 CLI 设置（token 从标准输入读取）：<code>kiln npm set --account {v.Account} --scope @&lt;scope&gt;</code>
            </p>
          </div>
        </details>
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">保存</SubmitButton>
          <LinkButton href="/settings">取消</LinkButton>
        </div>
      </Form>

      {configured && (
        <DangerZone>
          <DangerItem
            title="删除 npm 配置"
            action="/settings/npm/delete"
            fields={{ account: v.Account, confirm: '1' }}
            button="删除 npm 配置"
            confirmTitle={`删除 ${v.Account} 的 npm 配置？`}
            confirmLabel="删除 npm 配置"
            confirmBody="删除 scope、registry 和 token，无法恢复。重新配置之前，这个账号下的仓库无法发布 release.npm。"
          >
            删除 scope、registry 和 token，无法恢复。重新配置之前，这个账号下的仓库无法发布 <code>release.npm</code>，已发布到 npm 的包不受影响。
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsNpm)

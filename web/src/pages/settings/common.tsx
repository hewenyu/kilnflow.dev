import { Label, Link, FormControl, TextInput, Textarea } from '@primer/react'
import { Fragment, type ReactNode } from 'react'
import { Box, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { ConfirmForm } from '../../components/Form'
import { Ago } from '../../components/Time'
import { fieldError, fieldValue, type FormState } from '../../lib/envelope'
import type { DockerStatus, DockerView, EnvView, NpmView } from './schemas'
import css from './common.module.css'
import { Msg } from '../../i18n/locale'

export { css as settingsCss }

/** ScopeFields 是表单中回传的作用域（account、repo、env）。 */
export function ScopeFields({ account, repo, env }: { account: string; repo: string; env: string }) {
  return (
    <>
      <input type="hidden" name="account" value={account} />
      <input type="hidden" name="repo" value={repo} />
      <input type="hidden" name="env" value={env} />
    </>
  )
}

/** FormAlert 是表单顶部的错误摘要（表单没有保存时）。 */
export function FormAlert({ form }: { form: FormState }) {
  if (!form?.Failed) return null
  const general = fieldError(form, '_')
  return (
    <Callout tone="critical" title="没有保存。">
      {general ? <Msg text={general} /> : '请按字段旁的提示修改后再保存。'}
    </Callout>
  )
}

/** Crumbs 是 Subhead 标题中的上级链接（GitHub 设置子页的"环境 / release"写法）。 */
export function Crumbs({ items, current }: { items: readonly { label: string; href: string }[]; current: ReactNode }) {
  return (
    <>
      {items.map((i) => (
        <Fragment key={i.href}>
          <Link className={css.crumb} href={i.href}>
            {i.label}
          </Link>
          <span className={css.crumbSep} aria-hidden>
            /
          </span>
        </Fragment>
      ))}
      {current}
    </>
  )
}

/**
 * TextField 是单行或多行的文本字段：label、caption 和服务端回填的值与错误。
 * 敏感字段（type="password"）不回填。
 */
export function TextField({
  form,
  name,
  label,
  caption,
  required,
  multiline,
  rows = 3,
  type = 'text',
  mono,
  readOnly,
  value,
  autoComplete = 'off',
}: {
  form: FormState
  name: string
  label: ReactNode
  caption?: ReactNode
  required?: boolean
  multiline?: boolean
  rows?: number
  type?: 'text' | 'password' | 'search'
  mono?: boolean
  readOnly?: boolean
  /** 固定的值（例如编辑时不能修改的名称）；不给时用服务端回填的值。 */
  value?: string
  autoComplete?: string
}) {
  const err = fieldError(form, name)
  const initial = type === 'password' ? '' : (value ?? fieldValue(form, name))
  const status = err ? ('error' as const) : undefined
  const common = {
    name,
    block: true,
    spellCheck: false,
    autoCapitalize: 'off',
    'aria-invalid': err ? true : undefined,
    validationStatus: status,
  }
  return (
    <FormControl required={required}>
      <FormControl.Label>{label}</FormControl.Label>
      {multiline ? (
        <Textarea {...common} rows={rows} defaultValue={initial} className={mono ? css.mono : undefined} resize="vertical" />
      ) : (
        <TextInput
          {...common}
          type={type}
          defaultValue={initial}
          readOnly={readOnly}
          monospace={mono}
          autoComplete={type === 'password' ? 'new-password' : autoComplete}
        />
      )}
      {caption && <FormControl.Caption>{caption}</FormControl.Caption>}
      {err && <FormControl.Validation variant="error"><Msg text={err} /></FormControl.Validation>}
    </FormControl>
  )
}

const dockerLabels = {
  verified: { text: '已验证', variant: 'success' },
  saved: { text: '已保存 · 未验证', variant: 'accent' },
  incomplete: { text: '不完整', variant: 'attention' },
  unconfigured: { text: '未配置', variant: 'secondary' },
} as const satisfies Record<DockerStatus, { text: string; variant: string }>

/** DockerStatusLabel 是 Docker Hub 连接的状态标签。 */
export function DockerStatusLabel({ status }: { status: DockerStatus }) {
  const l = dockerLabels[status]
  return <Label variant={l.variant}>{l.text}</Label>
}

/** RefPolicy 显示 environment 允许的 ref。 */
export function RefPolicy({ env }: { env: EnvView }) {
  return env.ref_policy.length > 0 ? (
    <>
      只允许 <code>{env.ref_policy.join(', ')}</code>
    </>
  ) : (
    <>不限制 ref</>
  )
}

/** DockerMoreRows 是 Docker Hub 连接的验证状态和发布规则（键值列表中的行）。 */
export function DockerMoreRows({ v }: { v: DockerView }) {
  return (
    <>
      {v.Token && (
        <>
          <dt>连接验证</dt>
          <dd>
            {v.Verified ? (
              <>
                <span className={css.ok}>已用当前用户名、命名空间和令牌推送成功</span>{' '}
                <span className={css.muted}>
                  · <code>
                    {v.Verified.Image}:{v.Verified.Version}
                  </code>
                  ，<Ago t={v.Verified.At} />
                </span>
              </>
            ) : (
              <span className={css.muted}>
                尚未验证：保存不会登录 Docker Hub。推送 v* tag 后，发布 job 用当前这套用户名、命名空间和令牌推送成功即为已验证；修改其中任何一项（包括在变量页或 CLI 中修改、替换令牌）后需要重新验证
              </span>
            )}
          </dd>
        </>
      )}
      <dt>发布规则</dt>
      <dd>{v.Env ? <RefPolicy env={v.Env} /> : <span className={css.muted}>首次保存时创建：只允许 v* tag</span>}</dd>
    </>
  )
}

/** DockerSummary 是 Docker Hub 连接的摘要。 */
export function DockerSummary({ v }: { v: DockerView }) {
  return (
    <dl className={css.kv}>
      <dt>GitHub 账号</dt>
      <dd>{v.Account}（账号下的所有仓库共用）</dd>
      <dt>Docker Hub 用户名</dt>
      <dd>{v.Username ? <code>{v.Username}</code> : <span className={css.muted}>未设置</span>}</dd>
      <dt>镜像命名空间</dt>
      <dd>{v.Namespace ? <code>docker.io/{v.Namespace}/…</code> : <span className={css.muted}>未设置</span>}</dd>
      <dt>访问令牌</dt>
      <dd>
        {v.Token ? (
          <>
            <span className={css.ok}>已设置</span>{' '}
            <span className={css.muted}>
              · <Ago t={v.Token.UpdatedAt} />
              {v.Token.UpdatedBy && ` 由 ${v.Token.UpdatedBy} 更新`}
            </span>
          </>
        ) : (
          <span className={css.muted}>未设置</span>
        )}
      </dd>
      <DockerMoreRows v={v} />
    </dl>
  )
}

/** NpmStatusLabel 是 npm 配置的状态标签。保存不访问 registry，所以只有"已配置"和"未配置"。 */
export function NpmStatusLabel({ v }: { v: NpmView }) {
  return v.Config ? <Label variant="accent">已配置</Label> : <Label variant="secondary">未配置</Label>
}

/** NpmRows 是 npm 配置的 scope、registry 和 token（键值列表中的行），从不显示 token 本身。 */
export function NpmRows({ v }: { v: NpmView }) {
  const c = v.Config
  if (!c) {
    return (
      <>
        <dt>scope</dt>
        <dd>
          <span className={css.muted}>未设置</span>
        </dd>
        <dt>token</dt>
        <dd>
          <span className={css.muted}>未设置</span>
        </dd>
      </>
    )
  }
  return (
    <>
      <dt>scope</dt>
      <dd>
        <code>{c.Scope}</code>
        <span className={css.muted}>
          {' '}
          · 包发布为 <code>{c.Scope}/&lt;name&gt;</code>
        </span>
      </dd>
      <dt>registry</dt>
      <dd>
        <code>{c.Registry}</code>
      </dd>
      <dt>token</dt>
      <dd>
        <span className={css.ok}>已设置</span>{' '}
        <span className={css.muted}>
          · <Ago t={c.UpdatedAt} />
          {c.UpdatedBy && ` 由 ${c.UpdatedBy} 更新`}
        </span>
      </dd>
    </>
  )
}

/** NpmSummary 是 npm 配置的摘要。 */
export function NpmSummary({ v }: { v: NpmView }) {
  return (
    <dl className={css.kv}>
      <dt>GitHub 账号</dt>
      <dd>{v.Account}（账号下的所有仓库共用）</dd>
      <NpmRows v={v} />
    </dl>
  )
}

/** DockerOverride 提醒：定义了同名仓库级 environment 的仓库不使用这里的凭据。 */
export function DockerOverride({ v }: { v: DockerView }) {
  if (!v.Env || v.Env.OverriddenIn.length === 0) return null
  return (
    <Callout tone="warning" title="注意">
      {v.Env.OverriddenIn.join(', ')} 定义了自己的 environment <code>{v.Env.name}</code>，这些仓库<strong>不会</strong>使用这里的凭据。
    </Callout>
  )
}

/** DangerZone 是页面底部的危险操作区（GitHub 设置页的 Danger Zone）。 */
export function DangerZone({ children }: { children: ReactNode }) {
  return (
    <section className={css.section} aria-labelledby="danger-zone">
      <h2 id="danger-zone" className={css.dangerTitle}>
        危险操作
      </h2>
      <Box danger>{children}</Box>
    </section>
  )
}

/** DangerItem 是危险操作区中的一项：说明和确认后提交的按钮。 */
export function DangerItem({
  title,
  children,
  action,
  fields,
  button,
  confirmTitle,
  confirmLabel,
  confirmBody,
}: {
  title: string
  children: ReactNode
  action: string
  fields: Record<string, string>
  button: string
  confirmTitle: string
  confirmLabel: string
  confirmBody: ReactNode
}) {
  return (
    <BoxRow className={css.dangerRow}>
      <div className={css.dangerText}>
        <h3>{title}</h3>
        <p>{children}</p>
      </div>
      <ConfirmForm action={action} fields={fields} button={button} title={confirmTitle} confirmLabel={confirmLabel}>
        {confirmBody}
      </ConfirmForm>
    </BoxRow>
  )
}

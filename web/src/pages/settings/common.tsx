import { Label, Link, FormControl, TextInput, Textarea } from '@primer/react'
import { Fragment, type ReactNode } from 'react'
import { Box, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { ConfirmForm } from '../../components/Form'
import { Ago } from '../../components/Time'
import { fieldError, fieldValue, type FormState } from '../../lib/envelope'
import type { DockerField, DockerStatus, DockerView, EnvView, NpmView } from './schemas'
import css from './common.module.css'
import { Msg, Trans, useT, type Tags } from '../../i18n/locale'
import type { MsgKey } from '../../i18n/messages'

export { css as settingsCss }

/** settingsTags 是设置页文案中的富文本标签：代码、强调。 */
export const settingsTags: Tags = {
  code: (c) => <code>{c}</code>,
  strong: (c) => <strong>{c}</strong>,
  b: (c) => <b>{c}</b>,
}

/** withLink 返回带 <link>…</link> 标签的富文本标签（链接到 href）。 */
export function withLink(href: string, more: Tags = settingsTags): Tags {
  return { ...more, link: (c) => <Link href={href}>{c}</Link> }
}

/** withAgo 返回带 <ago></ago> 标签（时间 t 的"多久之前"，词序由文案决定）的富文本标签。 */
export function withAgo(t: string | null | undefined, more: Tags = settingsTags): Tags {
  return { ...more, ago: () => <Ago t={t ?? ''} /> }
}

/** scopeKindLabel 返回作用域类型说明的键：environment 属于账号还是仓库看 repo。 */
export function scopeKindLabel(kind: 'owner' | 'repo' | 'environment', repo: string): MsgKey {
  switch (kind) {
    case 'owner':
      return 'scope.kindOwner'
    case 'repo':
      return 'scope.kindRepo'
  }
  return repo ? 'scope.kindEnvRepo' : 'scope.kindEnvOwner'
}

/** exprRef 返回 workflow 中引用变量或 secret 的表达式，例如 ${{ vars.FOO }}（原样显示，作为文案的参数）。 */
export function exprRef(ctx: 'vars' | 'secrets', name: string): string {
  return '${{ ' + ctx + '.' + name + ' }}'
}

/** dockerFieldLabels 是 Docker Hub 连接各字段的显示名（dockerView.Missing 中的键）。 */
export const dockerFieldLabels: Readonly<Record<DockerField, MsgKey>> = {
  username: 'dockerField.username',
  namespace: 'dockerField.namespace',
  token: 'dockerField.token',
}

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
  const t = useT()
  if (!form?.Failed) return null
  const general = fieldError(form, '_')
  return (
    <Callout tone="critical" title={t('settingsForm.notSaved')}>
      {general ? <Msg text={general} /> : t('settingsForm.fixFields')}
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
  verified: { text: 'dockerStatus.verified', variant: 'success' },
  saved: { text: 'dockerStatus.saved', variant: 'accent' },
  incomplete: { text: 'dockerStatus.incomplete', variant: 'attention' },
  unconfigured: { text: 'dockerStatus.unconfigured', variant: 'secondary' },
} as const satisfies Record<DockerStatus, { text: MsgKey; variant: string }>

/** DockerStatusLabel 是 Docker Hub 连接的状态标签。 */
export function DockerStatusLabel({ status }: { status: DockerStatus }) {
  const t = useT()
  const l = dockerLabels[status]
  return <Label variant={l.variant}>{t(l.text)}</Label>
}

/** RefPolicy 显示 environment 允许的 ref（ref 原样显示）。 */
export function RefPolicy({ env }: { env: EnvView }) {
  const t = useT()
  return env.ref_policy.length > 0 ? (
    <Trans id="settingsCommon.refPolicyOnly" params={{ refs: env.ref_policy.join(', ') }} tags={settingsTags} />
  ) : (
    <>{t('settingsCommon.refPolicyAny')}</>
  )
}

/** UpdatedBy 是"<时间> 由 <登录名> 更新"中登录名的部分（没有登录名时为空）。 */
function UpdatedBy({ login }: { login: string }) {
  const t = useT()
  return login ? <>{t('settingsCommon.updatedBy', { login })}</> : null
}

/** DockerMoreRows 是 Docker Hub 连接的验证状态和发布规则（键值列表中的行）。 */
export function DockerMoreRows({ v }: { v: DockerView }) {
  const t = useT()
  return (
    <>
      {v.Token && (
        <>
          <dt>{t('docker.verifyLabel')}</dt>
          <dd>
            {v.Verified ? (
              <>
                <span className={css.ok}>{t('docker.verifiedOK')}</span>{' '}
                <span className={css.muted}>
                  · <code>
                    {v.Verified.Image}:{v.Verified.Version}
                  </code>
                  {t('settingsCommon.comma')}
                  <Ago t={v.Verified.At} />
                </span>
              </>
            ) : (
              <span className={css.muted}>{t('docker.notVerified')}</span>
            )}
          </dd>
        </>
      )}
      <dt>{t('docker.publishRule')}</dt>
      <dd>{v.Env ? <RefPolicy env={v.Env} /> : <span className={css.muted}>{t('docker.ruleOnFirstSave')}</span>}</dd>
    </>
  )
}

/** DockerSummary 是 Docker Hub 连接的摘要。 */
export function DockerSummary({ v }: { v: DockerView }) {
  const t = useT()
  const notSet = <span className={css.muted}>{t('settingsCommon.notSet')}</span>
  return (
    <dl className={css.kv}>
      <dt>{t('settingsCommon.githubAccount')}</dt>
      <dd>{t('settingsCommon.sharedAccount', { account: v.Account })}</dd>
      <dt>{t(dockerFieldLabels.username)}</dt>
      <dd>{v.Username ? <code>{v.Username}</code> : notSet}</dd>
      <dt>{t(dockerFieldLabels.namespace)}</dt>
      <dd>{v.Namespace ? <code>docker.io/{v.Namespace}/…</code> : notSet}</dd>
      <dt>{t(dockerFieldLabels.token)}</dt>
      <dd>
        {v.Token ? (
          <>
            <span className={css.ok}>{t('settingsCommon.set')}</span>{' '}
            <span className={css.muted}>
              · <Ago t={v.Token.UpdatedAt} />
              <UpdatedBy login={v.Token.UpdatedBy} />
            </span>
          </>
        ) : (
          notSet
        )}
      </dd>
      <DockerMoreRows v={v} />
    </dl>
  )
}

/** NpmStatusLabel 是 npm 配置的状态标签。保存不访问 registry，所以只有"已配置"和"未配置"。 */
export function NpmStatusLabel({ v }: { v: NpmView }) {
  const t = useT()
  return v.Config ? <Label variant="accent">{t('settingsCommon.configured')}</Label> : <Label variant="secondary">{t('settingsCommon.notConfigured')}</Label>
}

/** NpmRows 是 npm 配置的 scope、registry 和 token（键值列表中的行），从不显示 token 本身。 */
export function NpmRows({ v }: { v: NpmView }) {
  const t = useT()
  const c = v.Config
  if (!c) {
    return (
      <>
        <dt>scope</dt>
        <dd>
          <span className={css.muted}>{t('settingsCommon.notSet')}</span>
        </dd>
        <dt>token</dt>
        <dd>
          <span className={css.muted}>{t('settingsCommon.notSet')}</span>
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
          · <Trans id="npm.publishedAs" params={{ scope: c.Scope, pkg: '<name>' }} tags={settingsTags} />
        </span>
      </dd>
      <dt>registry</dt>
      <dd>
        <code>{c.Registry}</code>
      </dd>
      <dt>token</dt>
      <dd>
        <span className={css.ok}>{t('settingsCommon.set')}</span>{' '}
        <span className={css.muted}>
          · <Ago t={c.UpdatedAt} />
          <UpdatedBy login={c.UpdatedBy} />
        </span>
      </dd>
    </>
  )
}

/** NpmSummary 是 npm 配置的摘要。 */
export function NpmSummary({ v }: { v: NpmView }) {
  const t = useT()
  return (
    <dl className={css.kv}>
      <dt>{t('settingsCommon.githubAccount')}</dt>
      <dd>{t('settingsCommon.sharedAccount', { account: v.Account })}</dd>
      <NpmRows v={v} />
    </dl>
  )
}

/** DockerOverride 提醒：定义了同名仓库级 environment 的仓库不使用这里的凭据。 */
export function DockerOverride({ v }: { v: DockerView }) {
  const t = useT()
  if (!v.Env || v.Env.OverriddenIn.length === 0) return null
  return (
    <Callout tone="warning" title={t('docker.overrideTitle')}>
      <Trans id="docker.override" params={{ repos: v.Env.OverriddenIn.join(', '), env: v.Env.name }} tags={settingsTags} />
    </Callout>
  )
}

/** EnvRuleFields 是 environment 保护规则的两个字段（允许的 ref、允许发布到的仓库），新建页和配置页共用。 */
export function EnvRuleFields({ form }: { form: FormState }) {
  const t = useT()
  return (
    <>
      <TextField
        form={form}
        name="ref_policy"
        multiline
        rows={2}
        mono
        label={t('envRules.refs')}
        caption={<Trans id="envRules.refsCaption" tags={settingsTags} />}
      />
      <TextField form={form} name="allowed_publish_repos" label={t('envRules.publish')} caption={t('envRules.publishCaption')} />
    </>
  )
}

/** DangerZone 是页面底部的危险操作区（GitHub 设置页的 Danger Zone）。 */
export function DangerZone({ children }: { children: ReactNode }) {
  const t = useT()
  return (
    <section className={css.section} aria-labelledby="danger-zone">
      <h2 id="danger-zone" className={css.dangerTitle}>
        {t('settingsCommon.dangerZone')}
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

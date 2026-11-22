import { CheckCircleFillIcon, MarkGithubIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Avatar } from '../components/Avatar'
import { Box, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import type { Session } from '../lib/envelope'
import css from './settings_people.module.css'

// 登录与身份（PLAN #16.5）：管理员绑定 GitHub 账号 → 用 GitHub 登录 → 关闭 admin token 登录。

const bindingSchema = z.object({
  GitHubID: z.number(),
  GitHubLogin: z.string(),
  /** Dashboard 是否还允许用 admin token 登录。 */
  TokenLogin: z.boolean(),
  BoundBy: z.string(),
  BoundAt: z.string(),
  UpdatedAt: z.string(),
})
type Binding = z.infer<typeof bindingSchema>

const schema = z.object({
  Section: z.string(),
  /** 管理员绑定的 GitHub 账号；还没有绑定时为 null。 */
  Binding: bindingSchema.nullable(),
  /** 配置了 GitHub 登录（绑定需要它）。 */
  OAuth: z.boolean(),
  /** 当前会话是用绑定的 GitHub 账号通过 GitHub 登录的。 */
  SignedInAsBound: z.boolean(),
  /** 重新用 GitHub 登录后回到这个页面的地址。 */
  GitHubLogin: z.string(),
  /** 当前要做的一步：1 绑定、2 用 GitHub 登录、3 关闭 token 登录，全部完成时为 4。 */
  Step: z.number(),
})
type Data = z.infer<typeof schema>

function Who({ session }: { session: Session }) {
  return (
    <Box as="section" aria-labelledby="who-h" className={css.block}>
      <BoxRow className={css.who}>
        <Avatar login={session.Login} size={48} />
        <div>
          <h2 id="who-h" className={css.whoName}>
            <span className="visually-hidden">当前登录：</span>
            {session.Login}
            {session.ViaToken ? <Label>admin token</Label> : <Label variant="accent">GitHub</Label>}
            {session.Bound && <Label variant="accent">绑定的管理员账号</Label>}
          </h2>
          <p className={css.muted}>
            {session.ViaToken
              ? '用 admin token 登录，拥有全部管理员权限。'
              : session.Bound
                ? '用绑定的 GitHub 账号登录。'
                : '用 GitHub 登录（KILN_ADMINS 中的管理员）。'}
          </p>
        </div>
      </BoxRow>
    </Box>
  )
}

/** Step 是三步中的一步：完成的显示对勾，当前的突出显示，之后的变灰。 */
function Step({ n, step, done, title, children }: { n: number; step: number; done: boolean; title: string; children: ReactNode }) {
  const state = done ? 'done' : step === n ? 'current' : 'todo'
  return (
    <BoxRow as="li" className={`${css.step} ${css[state] ?? ''}`} aria-current={state === 'current' ? 'step' : undefined}>
      <span className={css.mark} aria-hidden>
        {done ? <CheckCircleFillIcon size={24} /> : n}
      </span>
      <div className={css.stepBody}>
        <h3 className={css.stepTitle}>
          {title}
          {done && <span className="visually-hidden">（已完成）</span>}
        </h3>
        {children}
      </div>
    </BoxRow>
  )
}

function BindStep({ data, b }: { data: Data; b: Binding | null }) {
  if (b) {
    return (
      <>
        <div className={css.bound}>
          <Avatar login={b.GitHubLogin} size={32} />
          <div>
            <p className={css.p0}>
              <b>{b.GitHubLogin}</b> <span className={css.muted}>GitHub ID {b.GitHubID}</span>
            </p>
            <p className={css.muted}>
              <Ago t={b.BoundAt} /> 由 {b.BoundBy} 绑定。用这个账号通过 GitHub 登录即为管理员，不需要写进 KILN_ADMINS。
            </p>
          </div>
        </div>
        <p className={css.p}>更换：用另一个 GitHub 账号完成授权，绑定随之换成那个账号，admin token 登录的开关不变。</p>
        <Form action="/settings/account/bind">
          <SubmitButton size="small" leadingVisual={MarkGithubIcon}>
            用另一个 GitHub 账号重新绑定
          </SubmitButton>
        </Form>
      </>
    )
  }
  return (
    <>
      <p className={css.p}>
        在 GitHub 上授权一次，Kiln 记下你的账号。之后你用 GitHub 登录就是管理员，admin token 登录和 CLI 的身份也都是这个账号。
      </p>
      {data.OAuth ? (
        <>
          <Form action="/settings/account/bind">
            <SubmitButton variant="primary" leadingVisual={MarkGithubIcon}>
              绑定 GitHub 账号
            </SubmitButton>
          </Form>
          <p className={css.hint}>会跳转到 GitHub 授权页，完成后回到这里。只读取账号的登录名和 ID。</p>
        </>
      ) : (
        <Callout tone="warning" title="需要先配置 GitHub 登录">
          在 Server 配置中设置 <code>KILN_GITHUB_CLIENT_ID</code> 和 <code>KILN_GITHUB_CLIENT_SECRET</code>（GitHub App 的用户授权凭据，回调地址{' '}
          <code>/auth/github/callback</code>），重启后再回到这里绑定。
        </Callout>
      )}
    </>
  )
}

function LoginStep({ data, b, session }: { data: Data; b: Binding | null; session: Session }) {
  const step = data.Step
  if (step > 2 && b) return <p className={css.p}>当前就是用 {b.GitHubLogin} 通过 GitHub 登录的，GitHub 登录可用。</p>
  if (step === 2 && b && session.ViaToken) {
    return (
      <>
        <p className={css.p}>用 {b.GitHubLogin} 通过 GitHub 登录一次，确认 GitHub 登录可用。会替换当前的登录，完成后回到这里。</p>
        <LinkButton href={data.GitHubLogin} variant="primary" leadingVisual={MarkGithubIcon}>
          用 GitHub 登录
        </LinkButton>
      </>
    )
  }
  if (step === 2 && b) {
    return (
      <p className={css.p}>
        当前登录的 {session.Login} 不是绑定的账号。后面的步骤需要 {b.GitHubLogin} 本人用 GitHub 登录后完成。
      </p>
    )
  }
  return <p className={css.muted}>绑定后，用绑定的账号通过 GitHub 登录一次。</p>
}

function TokenStep({ data, b }: { data: Data; b: Binding | null }) {
  if (data.Step === 4) {
    return (
      <>
        <p className={css.p}>已关闭：Dashboard 只能用 GitHub 登录。admin token 仍然可以用于 CLI。</p>
        <p className={css.hint}>
          GitHub 登录出问题时，在能访问 Server 的机器上执行 <code>kiln admin token-login on</code> 恢复。
        </p>
        <Form action="/settings/account/token-login" className={css.gapTop}>
          <input type="hidden" name="enabled" value="1" />
          <SubmitButton size="small">重新开启 admin token 登录</SubmitButton>
        </Form>
      </>
    )
  }
  if (data.Step === 3) {
    return (
      <>
        <p className={css.p}>关闭后，登录页不再提供 admin token 入口，已经用 admin token 登录的会话随即失效。CLI 仍用 admin token，不受影响。</p>
        <Form action="/settings/account/token-login">
          <input type="hidden" name="enabled" value="0" />
          <SubmitButton variant="primary">关闭 admin token 登录</SubmitButton>
        </Form>
        <p className={css.hint}>
          之后需要恢复时执行 <code>kiln admin token-login on</code>。
        </p>
      </>
    )
  }
  return (
    <p className={css.muted}>
      {b && !b.TokenLogin ? 'admin token 登录已经关闭。' : '先完成前两步：只有用绑定的账号通过 GitHub 登录后才能关闭，避免把自己锁在外面。'}
    </p>
  )
}

function Account({ data, env }: PageProps<Data>) {
  const b = data.Binding
  const step = data.Step
  const session = env.Session
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="Kiln 的管理员就是你的 GitHub 账号。绑定后用 GitHub 登录即为管理员，CLI 的操作也记在这个账号名下；确认 GitHub 登录可用后，可以关闭 admin token 登录。">
        登录与身份
      </Subhead>
      {session && <Who session={session} />}
      <Subhead level={2} spaced>
        三步完成
      </Subhead>
      <Box as="ul" className={css.steps} aria-label="绑定 GitHub 账号的三个步骤">
        <Step n={1} step={step} done={step > 1} title="绑定你的 GitHub 账号">
          <BindStep data={data} b={b} />
        </Step>
        <Step n={2} step={step} done={step > 2} title="用 GitHub 登录">
          {session && <LoginStep data={data} b={b} session={session} />}
        </Step>
        <Step n={3} step={step} done={step === 4} title="关闭 admin token 登录">
          <TokenStep data={data} b={b} />
        </Step>
      </Box>
      {b && (
        <>
          <Subhead level={2} spaced danger>
            危险操作
          </Subhead>
          <Box danger>
            <BoxRow className={css.danger}>
              <div>
                <h3 className={css.stepTitle}>解除绑定</h3>
                <p className={css.muted}>
                  {b.GitHubLogin} 不再因为绑定而是管理员（在 KILN_ADMINS / KILN_USERS 中时按名单）；admin token 登录随即恢复。CLI 之后的操作记为{' '}
                  <code>admin</code>。
                </p>
              </div>
              <ConfirmForm
                action="/settings/account/unbind"
                fields={{ confirm: '1' }}
                button="解除绑定"
                title={`解除 ${b.GitHubLogin} 的绑定？`}
                confirmLabel="解除绑定"
              >
                <p className={css.p0}>
                  我确认解除 {b.GitHubLogin} 的绑定。admin token 登录随即恢复，CLI 之后的操作记为 <code>admin</code>。
                </p>
              </ConfirmForm>
            </BoxRow>
          </Box>
        </>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, Account)

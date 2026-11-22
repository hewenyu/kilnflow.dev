import { MarkGithubIcon } from '@primer/octicons-react'
import { FormControl, Link, LinkButton, TextInput } from '@primer/react'
import { Callout } from '../components/Callout'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { KilnMark } from '../app/Shell'
import { Form, SubmitButton } from '../components/Form'
import { fieldError, formStateSchema, type Session } from '../lib/envelope'
import css from './login.module.css'

const schema = z.object({
  /** 登录后回到的站内地址。 */
  Next: z.string(),
  GitHubURL: z.string(),
  /** 会话过期、已退出等说明。 */
  Notice: z.string(),
  TokenLogin: z.boolean(),
  /** 管理员绑定的 GitHub 账号。 */
  Bound: z.string(),
  Form: formStateSchema,
})
type Data = z.infer<typeof schema>

/** TokenForm 是 admin token 登录表单：不回显输入的 token。 */
function TokenForm({ data, primary }: { data: Data; primary: boolean }) {
  const err = fieldError(data.Form, 'token')
  return (
    <Form action="/login" className={css.tokenForm} aria-label="用 admin token 登录">
      <input type="hidden" name="next" value={data.Next} />
      <FormControl required>
        <FormControl.Label>admin token</FormControl.Label>
        <TextInput
          type="password"
          name="token"
          block
          autoComplete="current-password"
          spellCheck={false}
          autoCapitalize="off"
          validationStatus={err ? 'error' : undefined}
          aria-invalid={err ? true : undefined}
        />
        <FormControl.Caption>Server 配置的 KILN_ADMIN_TOKEN，拥有全部管理员权限。不要在共享的电脑上保存。</FormControl.Caption>
        {err && <FormControl.Validation variant="error">{err}</FormControl.Validation>}
      </FormControl>
      <SubmitButton variant={primary ? 'primary' : 'default'} block>
        用 admin token 登录
      </SubmitButton>
    </Form>
  )
}

function SignedIn({ session, next }: { session: Session; next: string }) {
  return (
    <Callout title="已登录" hideTitle>
        当前已登录为 <strong>{session.Login}</strong>（{session.Admin ? '管理员' : '成员'}）。可以直接<Link href={next}>继续</Link>，或在下面换一个身份登录。
    </Callout>
  )
}

function Login({ data, env }: PageProps<Data>) {
  const formErr = fieldError(data.Form, '_')
  return (
    <div className={css.page}>
      <div className={css.brand}>
        <KilnMark size={48} />
      </div>
      <h1 className={css.title}>登录 Kiln</h1>
      {data.Notice && (
        <Callout title="登录状态" hideTitle className={css.notice}>
          {data.Notice}
        </Callout>
      )}
      {formErr && (
        <Callout tone="critical" title="登录失败" hideTitle className={css.notice}>
          {formErr}
        </Callout>
      )}
      <div className={css.box}>
        {env.Session && <SignedIn session={env.Session} next={data.Next} />}
        {env.OAuth ? (
          <>
            <LinkButton href={data.GitHubURL} variant="primary" size="large" block leadingVisual={MarkGithubIcon}>
              使用 GitHub 登录
            </LinkButton>
            {data.TokenLogin && (
              <details open={data.Form?.Failed}>
                <summary className={css.summary}>管理员备用入口：用 admin token 登录</summary>
                <TokenForm data={data} primary={false} />
              </details>
            )}
          </>
        ) : data.TokenLogin ? (
          <>
            <p className={css.lead}>这个 Server 没有配置 GitHub 登录，只能由管理员用 admin token 登录。</p>
            <TokenForm data={data} primary />
          </>
        ) : (
          <Callout tone="critical" title="现在无法登录">
              admin token 登录已关闭，而这个 Server 没有配置 GitHub 登录。管理员可以在服务器上执行 <code>kiln admin token-login on</code> 恢复。
          </Callout>
        )}
      </div>
      {(env.OAuth || data.TokenLogin) && (
      <div className={css.callout}>
        {env.OAuth && data.TokenLogin && (
          <p>
            {data.Bound
              ? `admin token 登录后的身份是绑定的 GitHub 账号 ${data.Bound}，只在 GitHub 登录不可用时使用。`
              : '第一次使用时用 admin token 登录，然后在 设置 → 登录与身份 中绑定你的 GitHub 账号。'}
          </p>
        )}
        {env.OAuth && !data.TokenLogin && (
          <p>
            admin token 登录已关闭。GitHub 登录不可用时，管理员可以在服务器上执行 <code>kiln admin token-login on</code> 恢复。
          </p>
        )}
        {!env.OAuth && data.TokenLogin && <p>配置 GitHub 登录（KILN_GITHUB_CLIENT_ID）后，你和其他成员可以用 GitHub 账号登录。</p>}
      </div>
      )}
    </div>
  )
}

export default definePage(schema, Login)

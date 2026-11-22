import { MarkGithubIcon } from '@primer/octicons-react'
import { FormControl, Link, LinkButton, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { KilnMark } from '../app/Shell'
import { Callout } from '../components/Callout'
import { Form, SubmitButton } from '../components/Form'
import { Msg, Trans, useT, type Tags } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { fieldError, formStateSchema, textSchema, type Session } from '../lib/envelope'
import css from './login.module.css'

const schema = z.object({
  /** 登录后回到的站内地址。 */
  Next: z.string(),
  GitHubURL: z.string(),
  /** 会话过期、已退出等说明（消息）。 */
  Notice: textSchema,
  TokenLogin: z.boolean(),
  /** 管理员绑定的 GitHub 账号。 */
  Bound: z.string(),
  Form: formStateSchema,
})
type Data = z.infer<typeof schema>

/** 文案中的命令（<code>…</code>）。 */
const codeTags: Tags = { code: (c) => <code>{c}</code> }

/** TokenForm 是 admin token 登录表单：不回显输入的 token。 */
function TokenForm({ data, primary }: { data: Data; primary: boolean }) {
  const t = useT()
  const err = fieldError(data.Form, 'token')
  return (
    <Form action="/login" className={css.tokenForm} aria-label={t('login.tokenForm')}>
      <input type="hidden" name="next" value={data.Next} />
      <FormControl required>
        <FormControl.Label>{t('login.tokenLabel')}</FormControl.Label>
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
        <FormControl.Caption>{t('login.tokenCaption')}</FormControl.Caption>
        {err && (
          <FormControl.Validation variant="error">
            <Msg text={err} />
          </FormControl.Validation>
        )}
      </FormControl>
      <SubmitButton variant={primary ? 'primary' : 'default'} block>
        {t('login.tokenForm')}
      </SubmitButton>
    </Form>
  )
}

function SignedIn({ session, next }: { session: Session; next: string }) {
  const t = useT()
  return (
    <Callout title={t('login.signedInTitle')} hideTitle>
      <Trans
        id="login.signedIn"
        params={{ login: session.Login, role: t(session.Admin ? 'role.admin' : 'role.member') }}
        tags={{ strong: (c) => <strong>{c}</strong>, link: (c) => <Link href={next}>{c}</Link> }}
      />
    </Callout>
  )
}

function Login({ data, env }: PageProps<Data>) {
  const t = useT()
  const formErr = fieldError(data.Form, '_')
  return (
    <div className={css.page}>
      <div className={css.brand}>
        <KilnMark size={48} />
      </div>
      <h1 className={css.title}>{t('login.title')}</h1>
      {!isEmpty(data.Notice) && (
        <Callout title={t('login.statusTitle')} hideTitle className={css.notice}>
          <Msg text={data.Notice} />
        </Callout>
      )}
      {formErr && (
        <Callout tone="critical" title={t('login.failedTitle')} hideTitle className={css.notice}>
          <Msg text={formErr} />
        </Callout>
      )}
      <div className={css.box}>
        {env.Session && <SignedIn session={env.Session} next={data.Next} />}
        {env.OAuth ? (
          <>
            <LinkButton href={data.GitHubURL} variant="primary" size="large" block leadingVisual={MarkGithubIcon}>
              {t('login.github')}
            </LinkButton>
            {data.TokenLogin && (
              <details open={data.Form?.Failed}>
                <summary className={css.summary}>{t('login.tokenFallback')}</summary>
                <TokenForm data={data} primary={false} />
              </details>
            )}
          </>
        ) : data.TokenLogin ? (
          <>
            <p className={css.lead}>{t('login.noOAuth')}</p>
            <TokenForm data={data} primary />
          </>
        ) : (
          <Callout tone="critical" title={t('login.unavailableTitle')}>
            <Trans id="login.unavailable" tags={codeTags} />
          </Callout>
        )}
      </div>
      {(env.OAuth || data.TokenLogin) && (
        <div className={css.callout}>
          {env.OAuth && data.TokenLogin && (
            <p>{data.Bound ? t('login.hintBound', { login: data.Bound }) : t('login.hintUnbound')}</p>
          )}
          {env.OAuth && !data.TokenLogin && (
            <p>
              <Trans id="login.hintTokenOff" tags={codeTags} />
            </p>
          )}
          {!env.OAuth && data.TokenLogin && <p>{t('login.hintConfigureOAuth')}</p>}
        </div>
      )}
    </div>
  )
}

export default definePage(schema, Login)

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
import { Trans, useT } from '../i18n/locale'
import type { Session } from '../lib/envelope'
import { settingsTags, withAgo } from './settings/common'
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
  const t = useT()
  return (
    <Box as="section" aria-labelledby="who-h" className={css.block}>
      <BoxRow className={css.who}>
        <Avatar login={session.Login} size={48} />
        <div>
          <h2 id="who-h" className={css.whoName}>
            <span className="visually-hidden">{t('accountPage.currentLogin')}</span>
            {session.Login}
            {session.ViaToken ? <Label>admin token</Label> : <Label variant="accent">GitHub</Label>}
            {session.Bound && <Label variant="accent">{t('accountPage.boundLabel')}</Label>}
          </h2>
          <p className={css.muted}>
            {session.ViaToken
              ? t('accountPage.viaToken')
              : session.Bound
                ? t('accountPage.viaBound')
                : t('accountPage.viaGitHub')}
          </p>
        </div>
      </BoxRow>
    </Box>
  )
}

/** Step 是三步中的一步：完成的显示对勾，当前的突出显示，之后的变灰。 */
function Step({ n, step, done, title, children }: { n: number; step: number; done: boolean; title: string; children: ReactNode }) {
  const t = useT()
  const state = done ? 'done' : step === n ? 'current' : 'todo'
  return (
    <BoxRow as="li" className={`${css.step} ${css[state] ?? ''}`} aria-current={state === 'current' ? 'step' : undefined}>
      <span className={css.mark} aria-hidden>
        {done ? <CheckCircleFillIcon size={24} /> : n}
      </span>
      <div className={css.stepBody}>
        <h3 className={css.stepTitle}>
          {title}
          {done && <span className="visually-hidden">{t('accountPage.done')}</span>}
        </h3>
        {children}
      </div>
    </BoxRow>
  )
}

function BindStep({ data, b }: { data: Data; b: Binding | null }) {
  const t = useT()
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
              <Trans id="accountPage.boundAt" params={{ by: b.BoundBy }} tags={withAgo(b.BoundAt)} />
            </p>
          </div>
        </div>
        <p className={css.p}>{t('accountPage.changeHint')}</p>
        <Form action="/settings/account/bind">
          <SubmitButton size="small" leadingVisual={MarkGithubIcon}>
            {t('accountPage.rebind')}
          </SubmitButton>
        </Form>
      </>
    )
  }
  return (
    <>
      <p className={css.p}>{t('accountPage.bindIntro')}</p>
      {data.OAuth ? (
        <>
          <Form action="/settings/account/bind">
            <SubmitButton variant="primary" leadingVisual={MarkGithubIcon}>
              {t('accountPage.bind')}
            </SubmitButton>
          </Form>
          <p className={css.hint}>{t('accountPage.bindHint')}</p>
        </>
      ) : (
        <Callout tone="warning" title={t('accountPage.needOAuthTitle')}>
          <Trans id="accountPage.needOAuth" tags={settingsTags} />
        </Callout>
      )}
    </>
  )
}

function LoginStep({ data, b, session }: { data: Data; b: Binding | null; session: Session }) {
  const t = useT()
  const step = data.Step
  if (step > 2 && b) return <p className={css.p}>{t('accountPage.loginDone', { login: b.GitHubLogin })}</p>
  if (step === 2 && b && session.ViaToken) {
    return (
      <>
        <p className={css.p}>{t('accountPage.loginNeeded', { login: b.GitHubLogin })}</p>
        <LinkButton href={data.GitHubLogin} variant="primary" leadingVisual={MarkGithubIcon}>
          {t('accountPage.loginButton')}
        </LinkButton>
      </>
    )
  }
  if (step === 2 && b) {
    return (
      <p className={css.p}>{t('accountPage.notBoundUser', { current: session.Login, login: b.GitHubLogin })}</p>
    )
  }
  return <p className={css.muted}>{t('accountPage.loginAfterBind')}</p>
}

function TokenStep({ data, b }: { data: Data; b: Binding | null }) {
  const t = useT()
  if (data.Step === 4) {
    return (
      <>
        <p className={css.p}>{t('accountPage.tokenOff')}</p>
        <p className={css.hint}>
          <Trans id="accountPage.tokenRecover" tags={settingsTags} />
        </p>
        <Form action="/settings/account/token-login" className={css.gapTop}>
          <input type="hidden" name="enabled" value="1" />
          <SubmitButton size="small">{t('accountPage.tokenReenable')}</SubmitButton>
        </Form>
      </>
    )
  }
  if (data.Step === 3) {
    return (
      <>
        <p className={css.p}>{t('accountPage.tokenOffIntro')}</p>
        <Form action="/settings/account/token-login">
          <input type="hidden" name="enabled" value="0" />
          <SubmitButton variant="primary">{t('accountPage.tokenDisable')}</SubmitButton>
        </Form>
        <p className={css.hint}>
          <Trans id="accountPage.tokenRecoverLater" tags={settingsTags} />
        </p>
      </>
    )
  }
  return (
    <p className={css.muted}>
      {b && !b.TokenLogin ? t('accountPage.tokenAlreadyOff') : t('accountPage.tokenFirst')}
    </p>
  )
}

function Account({ data, env }: PageProps<Data>) {
  const t = useT()
  const b = data.Binding
  const step = data.Step
  const session = env.Session
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('accountPage.description')}>{t('settingsNav.account')}</Subhead>
      {session && <Who session={session} />}
      <Subhead level={2} spaced>
        {t('accountPage.threeSteps')}
      </Subhead>
      <Box as="ul" className={css.steps} aria-label={t('accountPage.stepsLabel')}>
        <Step n={1} step={step} done={step > 1} title={t('accountPage.step1')}>
          <BindStep data={data} b={b} />
        </Step>
        <Step n={2} step={step} done={step > 2} title={t('accountPage.step2')}>
          {session && <LoginStep data={data} b={b} session={session} />}
        </Step>
        <Step n={3} step={step} done={step === 4} title={t('accountPage.step3')}>
          <TokenStep data={data} b={b} />
        </Step>
      </Box>
      {b && (
        <>
          <Subhead level={2} spaced danger>
            {t('settingsCommon.dangerZone')}
          </Subhead>
          <Box danger>
            <BoxRow className={css.danger}>
              <div>
                <h3 className={css.stepTitle}>{t('accountPage.unbindTitle')}</h3>
                <p className={css.muted}>
                  <Trans id="accountPage.unbindText" params={{ login: b.GitHubLogin }} tags={settingsTags} />
                </p>
              </div>
              <ConfirmForm
                action="/settings/account/unbind"
                fields={{ confirm: '1' }}
                button={t('accountPage.unbindButton')}
                title={t('accountPage.unbindConfirmTitle', { login: b.GitHubLogin })}
                confirmLabel={t('accountPage.unbindButton')}
              >
                <p className={css.p0}>
                  <Trans id="accountPage.unbindConfirm" params={{ login: b.GitHubLogin }} tags={settingsTags} />
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

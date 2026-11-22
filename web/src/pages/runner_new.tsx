import { CheckCircleFillIcon } from '@primer/octicons-react'
import { Breadcrumbs, FormControl, Link, Radio, RadioGroup, TextInput } from '@primer/react'
import { useState, type ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Callout } from '../components/Callout'
import { CopyButton } from '../components/CopyButton'
import { Form, SubmitButton } from '../components/Form'
import { connLabel, runnerKindLabel, runnerViewSchema, type RunnerView, type TFn } from '../components/RunnerSchema'
import { RunnerLabels } from '../components/RunnerStatus'
import { Ago, secondMs, useNow } from '../components/Time'
import { Msg, Trans, useLocale, useT, type Tags } from '../i18n/locale'
import { fieldError, fieldValue, formStateSchema, list, type FormState } from '../lib/envelope'
import { clock, shortVersion, toDate } from '../lib/format'
import css from './runner_new.module.css'

// 注册新的 Runner（/runners/new）和生成令牌后的一次性展示页（/runners/enroll/{id}）：
// 按顺序完成四步：准备机器，生成一次性注册令牌，在机器上执行命令，确认上线。令牌只在第一次打开展示页时返回。

const enrollSchema = z.object({
  ID: z.string(),
  /** 只在第一次打开展示页时有值。 */
  Token: z.string(),
  Pool: z.string(),
  Name: z.string(),
  /** ci（kiln-runner）或 ai（AI 执行节点的连接器 kiln-orca）。 */
  Kind: z.string().default('ci'),
  /** AI 执行节点的角色：review（审计）或 development（开发）；CI Runner 为空。 */
  Role: z.string().default(''),
  Created: z.string(),
  Expires: z.string(),
  /** unused、used、expired */
  Status: z.string(),
  UsedAt: z.string().nullable(),
  /** 使用了这个令牌的 Runner。 */
  Runner: runnerViewSchema.nullable(),
  /** 已经存在的同名 Runner：注册会替换它的身份。 */
  Existing: runnerViewSchema.nullable(),
})
type Enroll = z.infer<typeof enrollSchema>

const schema = z.object({
  Form: formStateSchema,
  RequestID: z.string(),
  PublicURL: z.string(),
  ServerVersion: z.string(),
  Pools: list(z.string()),
  /** 生成后的一次性展示页；为 null 时显示生成令牌的表单。 */
  Reveal: enrollSchema.nullable(),
  /** 地址中的令牌找不到。 */
  Missing: z.boolean(),
  RegenID: z.string(),
  /** 在 Runner 机器上执行的注册命令（令牌已经显示过时为空）。 */
  Command: z.string(),
})
type Data = z.infer<typeof schema>

/** formKind 返回生成令牌表单中的 Runner 类型取值：ci、ai（AI 审计）或 ai-dev（AI 开发）。 */
export function formKind(kind: string, role: string): string {
  if (kind !== 'ai') return 'ci'
  return role === 'development' ? 'ai-dev' : 'ai'
}

/** TokenLeft 是令牌的剩余有效期：expired 表示已过期（样式按它决定，不看文字），minutes 是剩余的整分钟数。 */
export interface TokenLeft {
  expired: boolean
  minutes: number
}

/** tokenLeft 返回令牌在 now 时的剩余有效期，精度是分钟；时间无效时返回 null。 */
export function tokenLeft(expires: string, now: number): TokenLeft | null {
  const d = toDate(expires)
  if (!d) return null
  const left = (d.getTime() - now) / 1000
  return { expired: left <= 0, minutes: left <= 0 ? 0 : Math.floor(left / 60) }
}

/** expiresText 返回令牌的剩余有效期的文字，例如"剩余 12 分钟"。 */
export function expiresText(left: TokenLeft, t: TFn): string {
  if (left.expired) return t('runnerNew.expired')
  if (left.minutes < 1) return t('runnerNew.leftUnderMinute')
  return t('runnerNew.leftMinutes', { count: left.minutes })
}

/** Countdown 是令牌的有效期倒计时，每 10 秒更新（精度是分钟）。 */
function Countdown({ expires }: { expires: string }) {
  const t = useT()
  const now = useNow(10 * secondMs)
  const left = tokenLeft(expires, now)
  if (!left) return null
  return <span className={left.expired ? css.expired : css.left}>{expiresText(left, t)}</span>
}

function Step({ n, title, done, todo, children }: { n: number; title: string; done?: boolean; todo?: boolean; children: ReactNode }) {
  return (
    <section className={`${css.step} ${todo ? css.todo : ''}`} aria-labelledby={`s${n}`}>
      <span className={`${css.num} ${done ? css.numDone : ''}`} aria-hidden>
        {done ? <CheckCircleFillIcon size={24} /> : n}
      </span>
      <div className={css.stepBody}>
        <h2 id={`s${n}`} className={css.stepTitle}>
          {title}
        </h2>
        {children}
      </div>
    </section>
  )
}

/** Code 是一段可以复制的命令（GitHub 新建 runner 页的代码块）。 */
function Code({ code, label }: { code: string; label: string }) {
  return (
    <div className={css.code}>
      <pre>{code}</pre>
      <span className={css.copy}>
        <CopyButton value={code} label={label} />
      </span>
    </div>
  )
}

const rich: Tags = { b: (c) => <b>{c}</b>, code: (c) => <code>{c}</code> }

function Prepare({ data }: { data: Data }) {
  const t = useT()
  const ver = shortVersion(data.ServerVersion)
  return (
    <Step n={1} title={t('runnerNew.step1')}>
      <ul className={css.list}>
        <li>
          <Trans
            id="runnerNew.prepInstall"
            params={{ archive: `kiln-<${t('runnerNew.archiveVersion')}>-<os>-<arch>.tar.gz` }}
            tags={{
              ...rich,
              ver: () => (
                <span className="mono" title={data.ServerVersion}>
                  {ver}
                </span>
              ),
            }}
          />
        </li>
        <li>
          <Trans id="runnerNew.prepHealth" tags={rich} />
          <Code code={`curl -fsS ${data.PublicURL}/healthz`} label={t('runnerNew.copyCheck')} />
        </li>
        <li>
          <Trans id="runnerNew.prepUser" tags={rich} />
        </li>
        <li>
          <Trans id="runnerNew.prepDocker" tags={rich} />
        </li>
        <li>{t('runnerNew.prepSecrets')}</li>
        <li>
          <Trans id="runnerNew.prepAI" tags={rich} />
          <Code code="npm install -g @yuebanlaosiji/kilnflow-orca" label={t('runnerNew.copyInstall')} />
          <Trans id="runnerNew.prepAIAgents" params={{ home: `--home <${t('runnerNew.dataDir')}>` }} tags={rich} />
        </li>
      </ul>
    </Step>
  )
}

function Field({ form, name, label, caption, children }: { form: FormState; name: string; label: string; caption: ReactNode; children: (p: { value: string; invalid: boolean }) => ReactNode }) {
  const err = fieldError(form, name)
  return (
    <FormControl>
      <FormControl.Label>{label}</FormControl.Label>
      {children({ value: fieldValue(form, name), invalid: Boolean(err) })}
      <FormControl.Caption>{caption}</FormControl.Caption>
      {err && (
        <FormControl.Validation variant="error">
          <Msg text={err} />
        </FormControl.Validation>
      )}
    </FormControl>
  )
}

function TokenForm({ data }: { data: Data }) {
  const t = useT()
  const f = data.Form
  const general = fieldError(f, '_')
  const kindErr = fieldError(f, 'kind')
  return (
    <Form action="/runners/token" aria-labelledby="s2">
      <input type="hidden" name="request_id" value={data.RequestID} />
      <Step n={2} title={t('runnerNew.step2')}>
        {f?.Failed && (
          <Callout tone="critical" title={t('runnerNew.failedTitle')} className={css.gap}>
            {general ? <Msg text={general} /> : t('runnerNew.failedFields')}
          </Callout>
        )}
        <div className={css.fields}>
          <RadioGroup name="kind">
            <RadioGroup.Label>{t('runnerNew.kind')}</RadioGroup.Label>
            <FormControl>
              <Radio value="ci" defaultChecked={(fieldValue(f, 'kind') || 'ci') === 'ci'} />
              <FormControl.Label>{t('runnerKind.ci')}</FormControl.Label>
              <FormControl.Caption>
                <Trans id="runnerNew.kindCI" tags={rich} />
              </FormControl.Caption>
            </FormControl>
            <FormControl>
              <Radio value="ai" defaultChecked={fieldValue(f, 'kind') === 'ai'} />
              <FormControl.Label>{t('runnerKind.aiReview')}</FormControl.Label>
              <FormControl.Caption>
                <Trans id="runnerNew.kindAIReview" tags={rich} />
              </FormControl.Caption>
            </FormControl>
            <FormControl>
              <Radio value="ai-dev" defaultChecked={fieldValue(f, 'kind') === 'ai-dev'} />
              <FormControl.Label>{t('runnerKind.aiDev')}</FormControl.Label>
              <FormControl.Caption>
                <Trans id="runnerNew.kindAIDev" tags={rich} />
              </FormControl.Caption>
            </FormControl>
            {kindErr && (
              <RadioGroup.Validation variant="error">
                <Msg text={kindErr} />
              </RadioGroup.Validation>
            )}
          </RadioGroup>
          <Field
            form={f}
            name="pool"
            label="pool"
            caption={
              <>
                <Trans id="runnerNew.poolCaption" tags={rich} />
                {data.Pools.length > 0 && t('runnerNew.poolExisting', { pools: data.Pools.join(t('runner.enumSeparator')) })}
              </>
            }
          >
            {({ value, invalid }) => (
              <TextInput
                name="pool"
                defaultValue={value}
                list="e-pools"
                placeholder="default"
                spellCheck={false}
                autoCapitalize="off"
                validationStatus={invalid ? 'error' : undefined}
                aria-invalid={invalid || undefined}
              />
            )}
          </Field>
          <datalist id="e-pools">
            {data.Pools.map((p) => (
              <option key={p} value={p} />
            ))}
          </datalist>
          <Field form={f} name="name" label={t('runnerNew.name')} caption={t('runnerNew.nameCaption')}>
            {({ value, invalid }) => (
              <TextInput
                name="name"
                defaultValue={value}
                placeholder={t('runnerNew.namePlaceholder')}
                spellCheck={false}
                autoCapitalize="off"
                validationStatus={invalid ? 'error' : undefined}
                aria-invalid={invalid || undefined}
              />
            )}
          </Field>
        </div>
        <div className={css.actions}>
          <SubmitButton variant="primary">{t('runnerNew.generate')}</SubmitButton>
        </div>
        <p className={css.hint}>{t('runnerNew.tokenHint')}</p>
      </Step>
    </Form>
  )
}

/** Regen 是重新生成令牌的表单（沿用这次的 pool 和名称）。 */
function Regen({ data }: { data: Data }) {
  const t = useT()
  return (
    <Form action="/runners/token" className={css.gapTop}>
      <input type="hidden" name="request_id" value={data.RegenID} />
      <input type="hidden" name="pool" value={fieldValue(data.Form, 'pool')} />
      <input type="hidden" name="name" value={fieldValue(data.Form, 'name')} />
      <input
        type="hidden"
        name="kind"
        value={data.Reveal ? formKind(data.Reveal.Kind, data.Reveal.Role) : fieldValue(data.Form, 'kind')}
      />
      <SubmitButton>{t('runnerNew.regenerate')}</SubmitButton>
    </Form>
  )
}

function TokenStatus({ data, v, token }: { data: Data; v: Enroll; token: string }) {
  const t = useT()
  const { locale } = useLocale()
  const mono: Tags = { m: (c) => <b className="mono">{c}</b> }
  return (
    <Step n={2} title={t('runnerNew.tokenTitle')} done={v.Status === 'used'}>
      <p className={css.facts}>
        {runnerKindLabel(v.Kind, v.Role, t)}
        {v.Pool && (
          <>
            {' · '}
            <Trans id="runnerNew.factPool" params={{ pool: v.Pool }} tags={mono} />
          </>
        )}
        {v.Name && (
          <>
            {' · '}
            <Trans id="runnerNew.factName" params={{ name: v.Name }} tags={mono} />
          </>
        )}
        {' · '}
        {t('runnerNew.factTimes', { created: clock(v.Created, locale), expires: clock(v.Expires, locale) })}
        {v.Status === 'unused' && (
          <>
            {t('runnerNew.leftOpen')}
            <Countdown expires={v.Expires} />
            {t('runnerNew.leftClose')}
          </>
        )}
        {' · '}
        {t('runnerNew.factOnce')}
      </p>
      {token ? (
        <Callout tone="warning" title={t('runnerNew.onceTitle')}>
          {t('runnerNew.onceText')}
        </Callout>
      ) : v.Status === 'unused' ? (
        <Callout tone="info" title={t('runnerNew.shownTitle')}>
          {t('runnerNew.shownText')}
          <Regen data={data} />
        </Callout>
      ) : v.Status === 'expired' ? (
        <Callout tone="critical" title={t('runnerNew.expiredTitle')}>
          {t('runnerNew.expiredText')}
          <Regen data={data} />
        </Callout>
      ) : (
        <p className={css.muted}>{t('runnerNew.usedText')}</p>
      )}
    </Step>
  )
}

function Existing({ r }: { r: RunnerView }) {
  const t = useT()
  const params = { kind: runnerKindLabel(r.Kind, r.Role, t), conn: connLabel(r, t), name: r.Name }
  return (
    <Callout tone="warning" title={t('runnerNew.existingTitle', { name: r.Name })} className={css.gapTop}>
      {t(r.Pool ? 'runnerNew.existingFactsPool' : 'runnerNew.existingFacts', { ...params, pool: r.Pool })}
      {t(r.Revoked ? 'runnerNew.existingRevoked' : 'runnerNew.existingActive', params)}
    </Callout>
  )
}

function Commands({ v, command }: { v: Enroll; command: string }) {
  const t = useT()
  const ai = v.Kind === 'ai'
  const tags: Tags = { ...rich, m: (c) => <span className="mono">{c}</span> }
  return (
    <Step n={3} title={t('runnerNew.step3')} todo={!command}>
      {command ? (
        <>
          <Code code={command} label={t('runnerNew.copyCommand')} />
          <ul className={css.list}>
            <li>
              {v.Name ? (
                <Trans id="runnerNew.cmdEnrollNamed" params={{ name: v.Name }} tags={tags} />
              ) : (
                <Trans id="runnerNew.cmdEnroll" tags={tags} />
              )}
            </li>
            {ai ? (
              <>
                <li>
                  <Trans id="runnerNew.cmdDoctorAI" tags={tags} />
                </li>
                <li>
                  <Trans id="runnerNew.cmdServiceAI" tags={tags} />
                </li>
                <li>
                  <Trans id="runnerNew.cmdHomeAI" params={{ home: `--home <${t('runnerNew.dataDir')}>`, example: '--home ~/kiln-orca-dev' }} tags={tags} />
                </li>
              </>
            ) : (
              <>
                <li>
                  <Trans id="runnerNew.cmdDoctor" tags={tags} />
                </li>
                <li>
                  <Trans id="runnerNew.cmdService" tags={tags} />
                </li>
              </>
            )}
          </ul>
          {v.Existing && <Existing r={v.Existing} />}
        </>
      ) : (
        <p className={css.muted}>
          {t(v.Status === 'used' ? 'runnerNew.noCmdUsed' : v.Status === 'expired' ? 'runnerNew.noCmdExpired' : 'runnerNew.noCmdShown')}
        </p>
      )}
    </Step>
  )
}

function Online({ v, live }: { v: Enroll; live: boolean }) {
  const t = useT()
  const r = v.Runner
  const bin = v.Kind === 'ai' ? 'kiln-orca' : 'kiln-runner'
  const online = v.Status === 'used' && r?.Accepting === true
  return (
    <Step n={4} title={t('runnerNew.step4')} done={online}>
      <div aria-live="polite">
        {v.Status === 'used' ? (
          r ? (
            <>
              <p className={css.p}>
                <Trans
                  id={r.Pool ? 'runnerNew.usedByPool' : 'runnerNew.usedBy'}
                  params={{ name: r.Name, pool: r.Pool }}
                  tags={{ link: (c) => <a href={r.URL}>{c}</a>, ago: () => <Ago t={v.UsedAt} /> }}
                />
              </p>
              <p className={css.labels}>
                <RunnerLabels runner={r} />
                {r.Platforms.length > 0 && <span className="mono">{r.Platforms.join(', ')}</span>}
                {r.Executors.length > 0 && <span>executor {r.Executors.join(', ')}</span>}
              </p>
              {r.Accepting ? (
                <p className={css.ok}>
                  <b>{t('runnerNew.online')}</b>
                </p>
              ) : !r.Revoked && !r.Online ? (
                <p className={css.p}>
                  <Trans id="runnerNew.notConnected" params={{ install: `${bin} service install`, run: `${bin} run` }} tags={rich} />
                </p>
              ) : null}
            </>
          ) : (
            <p className={css.p}>{t('runnerNew.usedGone')}</p>
          )
        ) : v.Status === 'expired' ? (
          <p className={css.p}>{t('runnerNew.expiredUnused')}</p>
        ) : (
          <p className={css.p}>{t('runnerNew.waiting')}</p>
        )}
      </div>
      <p className={css.hint}>
        <Link href={v.Kind === 'ai' ? '/ai/runners' : '/runners'}>{t(v.Kind === 'ai' ? 'runnerNew.viewAIList' : 'runnerNew.viewList')}</Link>
        {live && t('runnerNew.autoRefresh', { seconds: liveSeconds })}
      </p>
    </Step>
  )
}

function RunnerNew({ data }: PageProps<Data>) {
  const t = useT()
  const v = data.Reveal
  // 令牌只在第一次取数据时返回：定时刷新拿到的数据中没有令牌，保留第一次的令牌和命令
  const [first] = useState(() => ({ token: v?.Token ?? '', command: data.Command }))
  const unused = v?.Status === 'unused'
  const token = unused ? first.token : ''
  const command = unused ? first.command : ''
  const live = v !== null && v.Status !== 'expired' && !(v.Status === 'used' && v.Runner?.Accepting === true)
  useLive(live)
  return (
    <div className={css.page}>
      <Breadcrumbs>
        <Breadcrumbs.Item href="/runners">{t('runners.title')}</Breadcrumbs.Item>
        <Breadcrumbs.Item href={v ? `/runners/enroll/${v.ID}` : '/runners/new'} selected>
          {t('runnerNew.title')}
        </Breadcrumbs.Item>
      </Breadcrumbs>
      <h1 className={css.title}>{t('runnerNew.title')}</h1>
      <p className={css.lead}>{t('runnerNew.lead')}</p>
      {data.Missing && (
        <Callout tone="critical" title={t('runnerNew.missingTitle')}>
          {t('runnerNew.missingText')}
        </Callout>
      )}
      <Prepare data={data} />
      {v ? (
        <>
          <TokenStatus data={data} v={v} token={token} />
          <Commands v={v} command={command} />
          <Online v={v} live={live} />
        </>
      ) : (
        <>
          <TokenForm data={data} />
          <Step n={3} title={t('runnerNew.step3')} todo>
            <p className={css.muted}>{t('runnerNew.step3Todo')}</p>
          </Step>
          <Step n={4} title={t('runnerNew.step4')} todo>
            <p className={css.muted}>
              <Trans id="runnerNew.step4Todo" tags={{ link: (c) => <Link href="/runners">{c}</Link> }} />
            </p>
          </Step>
        </>
      )}
    </div>
  )
}

export default definePage(schema, RunnerNew)

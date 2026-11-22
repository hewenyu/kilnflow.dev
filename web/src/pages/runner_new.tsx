import { CheckCircleFillIcon } from '@primer/octicons-react'
import { Breadcrumbs, FormControl, Link, Radio, RadioGroup, TextInput } from '@primer/react'
import { useState, type ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Callout } from '../components/Callout'
import { CopyButton } from '../components/CopyButton'
import { Form, SubmitButton } from '../components/Form'
import { runnerViewSchema, type RunnerView } from '../components/RunnerSchema'
import { RunnerLabels } from '../components/RunnerStatus'
import { Ago, secondMs, useNow } from '../components/Time'
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

/** enrollKindLabel 返回令牌对应的 Runner 类型的显示名称。 */
export function enrollKindLabel(kind: string, role: string): string {
  if (kind !== 'ai') return 'CI Runner'
  return role === 'development' ? 'AI 开发节点' : 'AI 审计节点'
}

/** expiresText 返回令牌的剩余有效期，精度是分钟。 */
export function expiresText(expires: string, now: number): string {
  const d = toDate(expires)
  if (!d) return ''
  const left = (d.getTime() - now) / 1000
  if (left <= 0) return '已过期'
  if (left < 60) return '剩余不到 1 分钟'
  return `剩余 ${Math.floor(left / 60)} 分钟`
}

/** Countdown 是令牌的有效期倒计时，每 10 秒更新（精度是分钟）。 */
function Countdown({ expires }: { expires: string }) {
  const now = useNow(10 * secondMs)
  const text = expiresText(expires, now)
  return <span className={text === '已过期' ? css.expired : css.left}>{text}</span>
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

function Prepare({ data }: { data: Data }) {
  const ver = shortVersion(data.ServerVersion)
  return (
    <Step n={1} title="准备机器">
      <ul className={css.list}>
        <li>
          安装 <code>kiln-runner</code>：从 Kiln 的 Release 下载对应平台的 <code>{'kiln-<版本>-<os>-<arch>.tar.gz'}</code>，把其中的{' '}
          <code>kiln-runner</code> 放进 PATH（例如 <code>/usr/local/bin</code>）。建议与 Server 版本一致（Server 当前为{' '}
          <span className="mono" title={data.ServerVersion}>
            {ver}
          </span>
          ）。
        </li>
        <li>
          确认这台机器能访问 Server：下面的命令输出 <code>ok</code>。Runner 只发出站连接，不需要开放端口。
          <Code code={`curl -fsS ${data.PublicURL}/healthz`} label="复制检查命令" />
        </li>
        <li>
          用将来运行 Runner 的普通用户执行后面的命令，不要用 root。Linux 上先执行 <code>sudo loginctl enable-linger "$USER"</code>
          ，让用户服务在退出登录后继续运行。
        </li>
        <li>
          需要容器 job 时装好 docker（OrbStack、colima 或 Docker Engine），<code>docker info</code> 能用即可；没有 docker 时只有 host executor。
        </li>
        <li>不要把 Server 的 admin token 或 GitHub App 私钥复制到这台机器，只用一次性注册令牌。</li>
        <li>
          <b>AI 执行节点</b>（AI 审计、AI 开发）不用 <code>kiln-runner</code>，而是执行机器上常驻的 Orca 加连接器 <code>kiln-orca</code>：
          需要 Node.js 24+，Orca 已运行（macOS 已验证；Linux 可以注册为审计、开发节点，实机验证进行中），Codex 和 / 或 Claude Code
          已安装并登录；git 凭据由 Server 按任务签发，机器上不用配置；可选 Docker Compose，为需要 pg、redis 的仓库拉起测试服务。用 npm 安装连接器：
          <Code code="npm install -g @yuebanlaosiji/kilnflow-orca" label="复制安装命令" />
          审计 AI 开发的 PR 时用的 Agent 不能是开发时用过的（例如 Claude Code 开发、Codex 审计），所以一台装了两种 Agent 的机器可以同时注册一个审计节点和一个开发节点：
          第二个节点的每条命令都加 <code>--home &lt;另一个数据目录&gt;</code>。AI 执行节点可以与 kiln-runner 装在同一台机器上。
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
      {err && <FormControl.Validation variant="error">{err}</FormControl.Validation>}
    </FormControl>
  )
}

function TokenForm({ data }: { data: Data }) {
  const f = data.Form
  const general = fieldError(f, '_')
  return (
    <Form action="/runners/token" aria-labelledby="s2">
      <input type="hidden" name="request_id" value={data.RequestID} />
      <Step n={2} title="生成一次性注册令牌">
        {f?.Failed && (
          <Callout tone="critical" title="没有生成令牌" className={css.gap}>
            {general ?? '请按字段旁的提示修改后再生成。'}
          </Callout>
        )}
        <div className={css.fields}>
          <RadioGroup name="kind">
            <RadioGroup.Label>Runner 类型</RadioGroup.Label>
            <FormControl>
              <Radio value="ci" defaultChecked={(fieldValue(f, 'kind') || 'ci') === 'ci'} />
              <FormControl.Label>CI Runner</FormControl.Label>
              <FormControl.Caption>
                <code>kiln-runner</code>：执行 <code>.kiln/workflows</code> 中的 job。
              </FormControl.Caption>
            </FormControl>
            <FormControl>
              <Radio value="ai" defaultChecked={fieldValue(f, 'kind') === 'ai'} />
              <FormControl.Label>AI 审计节点</FormControl.Label>
              <FormControl.Caption>
                <code>kiln-orca</code>：只执行 PR 的 AI 审计（仓库的 <code>.kiln/agent/config.yml</code>），在 Orca 中运行 Codex 或 Claude Code，可以同时执行多个任务。
              </FormControl.Caption>
            </FormControl>
            <FormControl>
              <Radio value="ai-dev" defaultChecked={fieldValue(f, 'kind') === 'ai-dev'} />
              <FormControl.Label>AI 开发节点</FormControl.Label>
              <FormControl.Caption>
                <code>kiln-orca</code>：从标了 <code>kiln:ready</code> 的 Issue 中选题、设计、开发并交付为 PR（仓库开启 <code>development</code> 后）。
                它开发的 PR 由审计节点用另一个 Agent 审计；角色注册后不能修改。
              </FormControl.Caption>
            </FormControl>
            {fieldError(f, 'kind') && <RadioGroup.Validation variant="error">{fieldError(f, 'kind')}</RadioGroup.Validation>}
          </RadioGroup>
          <Field
            form={f}
            name="pool"
            label="pool"
            caption={
              <>
                机器组，留空为 default。写了 <code>pool: X</code> 的 job 只在 pool X 的 Runner 上执行；没有写 pool 的 job 可以在任何 pool 上执行。
                {data.Pools.length > 0 && `已有：${data.Pools.join('、')}。`}
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
          <Field
            form={f}
            name="name"
            label="Runner 名称（可选）"
            caption="只用于生成第 3 步的命令，留空时取机器的主机名。名称不能重复：与已有 Runner 同名时，注册会替换那台 Runner 的身份（用于重新注册同一台机器）。"
          >
            {({ value, invalid }) => (
              <TextInput
                name="name"
                defaultValue={value}
                placeholder="例如 mini-01"
                spellCheck={false}
                autoCapitalize="off"
                validationStatus={invalid ? 'error' : undefined}
                aria-invalid={invalid || undefined}
              />
            )}
          </Field>
        </div>
        <div className={css.actions}>
          <SubmitButton variant="primary">生成注册令牌</SubmitButton>
        </div>
        <p className={css.hint}>令牌 1 小时内有效、只能使用一次，生成后只显示一次；刷新或返回不会重新生成，也不会再次显示。</p>
      </Step>
    </Form>
  )
}

/** Regen 是重新生成令牌的表单（沿用这次的 pool 和名称）。 */
function Regen({ data }: { data: Data }) {
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
      <SubmitButton>重新生成令牌</SubmitButton>
    </Form>
  )
}

function TokenStatus({ data, v, token }: { data: Data; v: Enroll; token: string }) {
  return (
    <Step n={2} title="一次性注册令牌" done={v.Status === 'used'}>
      <p className={css.facts}>
        {enrollKindLabel(v.Kind, v.Role)}
        {v.Pool && (
          <>
            {' '}
            · pool <b className="mono">{v.Pool}</b>
          </>
        )}
        {v.Name && (
          <>
            {' '}
            · 名称 <b className="mono">{v.Name}</b>
          </>
        )}{' '}
        · 生成于 {clock(v.Created)} · 有效期到 {clock(v.Expires)}
        {v.Status === 'unused' && (
          <>
            （<Countdown expires={v.Expires} />）
          </>
        )}{' '}
        · 只能使用一次。
      </p>
      {token ? (
        <Callout tone="warning" title="令牌只显示这一次">
          离开或刷新本页后不会再显示，也不会重新生成。请现在复制第 3 步的命令。
        </Callout>
      ) : v.Status === 'unused' ? (
        <Callout tone="info" title="令牌已经显示过，不会再次显示">
          已经复制的话，直接在机器上执行即可；没有保存下来时，重新生成一个。之前的令牌没有被使用时，到期后自动失效。
          <Regen data={data} />
        </Callout>
      ) : v.Status === 'expired' ? (
        <Callout tone="critical" title="令牌已过期，没有被使用">
          过期的令牌不能注册，请重新生成。
          <Regen data={data} />
        </Callout>
      ) : (
        <p className={css.muted}>令牌已被使用，不能再次使用，见第 4 步。</p>
      )}
    </Step>
  )
}

function Existing({ r }: { r: RunnerView }) {
  return (
    <Callout tone="warning" title={`已经有名为 ${r.Name} 的 Runner`} className={css.gapTop}>
      （{r.KindLabel}，{r.ConnLabel}
      {r.Pool && `，pool ${r.Pool}`}）用这个名称注册会替换它的身份：
      {r.Revoked
        ? `被吊销的 ${r.Name} 会恢复为在线并接单。`
        : '原来那台机器之后无法再连接。如果是同一台机器重新注册，可以继续；否则请换一个名称后重新生成。'}
    </Callout>
  )
}

function Commands({ v, command }: { v: Enroll; command: string }) {
  const ai = v.Kind === 'ai'
  return (
    <Step n={3} title="在机器上执行命令" todo={!command}>
      {command ? (
        <>
          <Code code={command} label="复制命令" />
          <ul className={css.list}>
            <li>
              <code>enroll</code>：用令牌注册
              {v.Name ? (
                <>
                  为 <span className="mono">{v.Name}</span>
                </>
              ) : (
                <>
                  ，名称默认取机器的主机名（去掉域名部分），需要别的名称时加 <code>--name</code>
                </>
              )}
              。
            </li>
            {ai ? (
              <>
                <li>
                  <code>doctor</code>：检查注册、与 Server 的连接、Orca、Agent、git 和 Docker（可选）。
                </li>
                <li>
                  <code>service install</code>：安装并启动服务。macOS 为 LaunchAgent，Linux 为 systemd 用户服务；日志在数据目录的{' '}
                  <code>logs/</code>。也可以先用 <code>kiln-orca run</code> 在前台运行，直接看输出。
                </li>
                <li>
                  同一台机器上已经有一个 AI 执行节点时，每条命令都加 <code>--home &lt;另一个数据目录&gt;</code>，例如{' '}
                  <code>--home ~/kiln-orca-dev</code>。
                </li>
              </>
            ) : (
              <>
                <li>
                  <code>doctor</code>：检查平台、executor 和与 Server 的连接。
                </li>
                <li>
                  <code>service install</code>：安装并启动服务。macOS 为 LaunchAgent（开机自启，日志在 <code>~/kiln/logs/runner.log</code>）；Linux 为
                  systemd 用户服务（日志用 <code>journalctl --user -u kiln-runner -f</code> 查看）。也可以先用 <code>kiln-runner run</code>{' '}
                  在前台运行，直接看输出。
                </li>
              </>
            )}
          </ul>
          {v.Existing && <Existing r={v.Existing} />}
        </>
      ) : (
        <p className={css.muted}>
          {v.Status === 'used' ? '令牌已使用' : v.Status === 'expired' ? '令牌已过期' : '令牌已经显示过'}，这里不再显示命令。
        </p>
      )}
    </Step>
  )
}

function Online({ v, live }: { v: Enroll; live: boolean }) {
  const r = v.Runner
  const bin = v.Kind === 'ai' ? 'kiln-orca' : 'kiln-runner'
  const online = v.Status === 'used' && r?.Accepting === true
  return (
    <Step n={4} title="确认上线" done={online}>
      <div aria-live="polite">
        {v.Status === 'used' ? (
          r ? (
            <>
              <p className={css.p}>
                令牌已被 <a href={r.URL}>{r.Name}</a> 使用（<Ago t={v.UsedAt} />
                {r.Pool && `，pool ${r.Pool}`}）。
              </p>
              <p className={css.labels}>
                <RunnerLabels runner={r} />
                {r.Platforms.length > 0 && <span className="mono">{r.Platforms.join(', ')}</span>}
                {r.Executors.length > 0 && <span>executor {r.Executors.join(', ')}</span>}
              </p>
              {r.Accepting ? (
                <p className={css.ok}>
                  <b>已上线，可以接收任务。</b>
                </p>
              ) : !r.Revoked && !r.Online ? (
                <p className={css.p}>
                  已注册，但还没有连接：在机器上执行 <code>{bin} service install</code> 启动服务，或用 <code>{bin} run</code>{' '}
                  在前台运行查看输出。
                </p>
              ) : null}
            </>
          ) : (
            <p className={css.p}>令牌已被使用，但对应的 Runner 已不存在。</p>
          )
        ) : v.Status === 'expired' ? (
          <p className={css.p}>令牌已过期，没有 Runner 用它注册。</p>
        ) : (
          <p className={css.p}>等待注册：令牌还没有被使用。在机器上执行第 3 步的命令后，这里会自动更新。</p>
        )}
      </div>
      <p className={css.hint}>
        <Link href={v.Kind === 'ai' ? '/ai/runners' : '/runners'}>在 {v.Kind === 'ai' ? 'AI 执行节点列表' : 'Runner 列表'}中查看</Link>
        {live && ` · 每 ${liveSeconds} 秒自动更新`}
      </p>
    </Step>
  )
}

function RunnerNew({ data }: PageProps<Data>) {
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
        <Breadcrumbs.Item href="/runners">Runner</Breadcrumbs.Item>
        <Breadcrumbs.Item href={v ? `/runners/enroll/${v.ID}` : '/runners/new'} selected>
          注册新的 Runner
        </Breadcrumbs.Item>
      </Breadcrumbs>
      <h1 className={css.title}>注册新的 Runner</h1>
      <p className={css.lead}>按顺序完成四步：准备机器，生成一次性注册令牌，在机器上执行命令，确认上线。</p>
      {data.Missing && (
        <Callout tone="critical" title="找不到这次生成的令牌">
          这个地址只对生成令牌的用户有效，令牌过期一小时后或 Server 重启后不再保存。令牌不会再次显示；需要时在下面重新生成。
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
          <Step n={3} title="在机器上执行命令" todo>
            <p className={css.muted}>生成令牌后显示要执行的命令。</p>
          </Step>
          <Step n={4} title="确认上线" todo>
            <p className={css.muted}>
              Runner 注册并启动服务后，在下一页或 <Link href="/runners">Runner 列表</Link>中确认它在线并接单。
            </p>
          </Step>
        </>
      )}
    </div>
  )
}

export default definePage(schema, RunnerNew)

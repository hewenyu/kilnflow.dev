import { RepoIcon, SearchIcon } from '@primer/octicons-react'
import { Button, FormControl, LinkButton, Radio, RadioGroup, Textarea, TextInput } from '@primer/react'
import { useState, type MouseEvent } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useRefreshLock } from '../app/live'
import { useRouter } from '../app/router'
import { Box, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { Form, formBody, SubmitButton } from '../components/Form'
import { fieldError, fieldValue, formStateSchema, list } from '../lib/envelope'
import { short } from '../lib/format'
import css from './run_new.module.css'
import { Msg } from '../i18n/locale'

// 启动构建（/runs/new、POST /dispatch）：手动运行声明了 on.workflow_dispatch 的 workflow。
// 没有选定仓库时显示仓库选择器；选定后按 ref 读取 workflow 列表（"读取 workflow" 提交到 POST /runs/new，不创建 run）。

const inputSchema = z.object({
  Name: z.string(),
  Description: z.string(),
  /** string、boolean 或 choice */
  Type: z.string(),
  Default: z.string(),
  Required: z.boolean(),
  Options: list(z.string()),
})

const workflowSchema = z.object({
  File: z.string(),
  Name: z.string(),
  /** 声明了 on.workflow_dispatch。 */
  Dispatchable: z.boolean(),
  /** 配置错误的摘要：触发后 run 会因配置错误直接失败。 */
  ConfigError: z.string(),
  Inputs: list(inputSchema),
})
type Workflow = z.infer<typeof workflowSchema>

const pickerSchema = z.object({
  Repos: list(z.object({ FullName: z.string(), DefaultBranch: z.string(), Private: z.boolean(), Active: z.boolean() })),
  Query: z.string(),
  Page: z.number(),
  Pages: z.number(),
  Total: z.number(),
  PrevURL: z.string(),
  NextURL: z.string(),
})
type Picker = z.infer<typeof pickerSchema>

const schema = z.object({
  /** 没有选定仓库时的仓库选择器。 */
  Picker: pickerSchema.nullable(),
  Repo: z.string(),
  /** 仓库是从运行列表的筛选条件带过来的。 */
  FromFilter: z.boolean(),
  DefaultBranch: z.string(),
  /** 按表单中的 ref 读取到的 workflow 列表；为 null 时退回为手填文件名。 */
  Target: z.object({ Ref: z.string(), SHA: z.string(), Workflows: list(workflowSchema) }).nullable(),
  /** Target 中可以手动触发的 workflow 数。 */
  Dispatchable: z.number(),
  /** 读取 workflow 列表失败的原因。 */
  TargetErr: z.string(),
  Form: formStateSchema,
  /** 这是一次启动构建的提交（错误摘要写明"没有启动构建"）。 */
  Attempted: z.boolean(),
  /** 这次表单的幂等键：重复提交同一个表单只会创建一个 run。 */
  RequestID: z.string(),
  /** "取消"和面包屑回到的运行列表。 */
  ListURL: z.string(),
})
type Data = z.infer<typeof schema>

/** FormAlert 是表单顶部的错误摘要。 */
function FormAlert({ data }: { data: Data }) {
  if (!data.Form?.Failed) return null
  const general = fieldError(data.Form, '_')
  return (
    <Callout tone="critical" title={data.Attempted ? '没有启动构建。' : '请修改后再提交'} hideTitle={!data.Attempted}>
      {general ? <Msg text={general} /> : '请按标出的提示修改后再提交。'}
    </Callout>
  )
}

/** inputsText 说明 workflow 声明的 inputs，例如"inputs：version（string，必填）；mode（choice，默认 fast，可选 fast、full）"。 */
export function inputsText(w: Workflow): string {
  if (!w.Dispatchable) return '不能手动触发：没有声明 on.workflow_dispatch。'
  if (w.Inputs.length === 0) return '没有声明 inputs。'
  const items = w.Inputs.map((i) => {
    const attrs = [i.Type]
    if (i.Required) attrs.push('必填')
    if (i.Default) attrs.push(`默认 ${i.Default}`)
    if (i.Options.length > 0) attrs.push(`可选 ${i.Options.join('、')}`)
    return `${i.Name}（${attrs.join('，')}）${i.Description ? ' ' + i.Description : ''}`
  })
  return `inputs：${items.join('；')}`
}

function RepoPicker({ data, picker: s }: { data: Data; picker: Picker }) {
  const repoErr = fieldError(data.Form, 'repo')
  return (
    <section className={css.step} aria-labelledby="pick-h">
      <h2 className={css.stepTitle} id="pick-h">
        1. 选择仓库
      </h2>
      <Form action="/runs/new" method="get" className={css.search} aria-label="搜索仓库">
        <FormControl>
          <FormControl.Label visuallyHidden>搜索仓库</FormControl.Label>
          <TextInput
            type="search"
            name="q"
            defaultValue={s.Query}
            placeholder="仓库名的一部分，例如 api"
            spellCheck={false}
            autoCapitalize="off"
            leadingVisual={SearchIcon}
            block
            validationStatus={repoErr ? 'error' : undefined}
            aria-invalid={repoErr ? true : undefined}
          />
          {repoErr && <FormControl.Validation variant="error"><Msg text={repoErr} /></FormControl.Validation>}
        </FormControl>
        <Button type="submit">搜索</Button>
        {s.Query && (
          <LinkButton href="/runs/new" variant="invisible">
            清除
          </LinkButton>
        )}
      </Form>
      <Box>
        <div className={css.boxHead} role="status">
          {s.Query ? `与"${s.Query}"匹配的` : '共'} {s.Total} 个可构建的仓库
          {s.Pages > 1 && `，第 ${s.Page} / ${s.Pages} 页`}
        </div>
        {s.Repos.length > 0 ? (
          <ul className={css.repos} aria-label="仓库">
            {s.Repos.map((r) => (
              <BoxRow as="li" key={r.FullName} hoverable>
                <RepoIcon className={css.repoIcon} aria-hidden />
                <span className={css.repoText}>
                  <span className={css.repoName}>{r.FullName}</span>
                  <span className={css.muted}>
                    {r.DefaultBranch && `默认分支 ${r.DefaultBranch} · `}
                    {r.Private ? 'private' : 'public'}
                  </span>
                </span>
                <LinkButton
                  href={`/runs/new?${new URLSearchParams({ repo: r.FullName }).toString()}`}
                  size="small"
                  aria-label={`选择 ${r.FullName}`}
                >
                  选择
                </LinkButton>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <p className={css.none}>{s.Query ? '没有匹配的仓库。' : '没有可以构建的仓库：确认 App 已安装、账号在 KILN_ALLOWED_ACCOUNTS 中。'}</p>
        )}
      </Box>
      {s.Pages > 1 && (
        <nav className={css.pager} aria-label="分页">
          {s.PrevURL && (
            <LinkButton href={s.PrevURL} rel="prev" size="small">
              上一页
            </LinkButton>
          )}
          <span className={css.muted}>
            第 {s.Page} / {s.Pages} 页
          </span>
          {s.NextURL && (
            <LinkButton href={s.NextURL} rel="next" size="small">
              下一页
            </LinkButton>
          )}
        </nav>
      )}
    </section>
  )
}

/** LoadButton 按表单中的 ref 重新读取 workflow 列表（POST /runs/new），保留已填写的内容，不创建 run。 */
function LoadButton() {
  const { submit } = useRouter()
  const [busy, setBusy] = useState(false)
  useRefreshLock(busy)
  const load = (e: MouseEvent<HTMLButtonElement>) => {
    const form = e.currentTarget.form
    if (!form || busy) return
    setBusy(true)
    void submit({ action: '/runs/new', body: formBody(form, null) }).finally(() => setBusy(false))
  }
  return (
    <Button type="button" onClick={load} loading={busy}>
      读取 workflow
    </Button>
  )
}

function WorkflowChoice({ data }: { data: Data }) {
  const f = data.Form
  const t = data.Target
  const wfErr = fieldError(f, 'workflow')
  if (!t) {
    return (
      <>
        {data.TargetErr && (
          <Callout tone="warning" title="无法读取 workflow 列表">
            {data.TargetErr}。可以直接填写 workflow 文件名。
          </Callout>
        )}
        <FormControl>
          <FormControl.Label>workflow 文件名</FormControl.Label>
          <TextInput
            name="workflow"
            defaultValue={fieldValue(f, 'workflow')}
            placeholder="ci.yml"
            spellCheck={false}
            autoCapitalize="off"
            className={css.mono}
            validationStatus={wfErr ? 'error' : undefined}
            aria-invalid={wfErr ? true : undefined}
          />
          <FormControl.Caption>
            .kiln/workflows/ 下的文件名，可以省略 .yml。{fieldError(f, 'ref') && '修正 ref 后点"读取 workflow"可以从列表中选择。'}
          </FormControl.Caption>
          {wfErr && <FormControl.Validation variant="error"><Msg text={wfErr} /></FormControl.Validation>}
        </FormControl>
      </>
    )
  }
  return (
    <>
      {t.Workflows.length > 0 ? (
        <RadioGroup name="workflow" aria-invalid={wfErr ? true : undefined}>
          <RadioGroup.Label>选择 workflow</RadioGroup.Label>
          {t.Workflows.map((w) => (
            <FormControl key={w.File} disabled={!w.Dispatchable}>
              <Radio value={w.File} defaultChecked={w.File === fieldValue(f, 'workflow')} aria-invalid={wfErr ? true : undefined} />
              <FormControl.Label>
                {w.Name || w.File} <span className={css.file}>{w.File}</span>
              </FormControl.Label>
              <FormControl.Caption>
                {inputsText(w)}
                {w.ConfigError && <span className={css.warn}> 配置有错误，触发后 run 会直接失败并显示错误。</span>}
              </FormControl.Caption>
            </FormControl>
          ))}
          {wfErr && <RadioGroup.Validation variant="error"><Msg text={wfErr} /></RadioGroup.Validation>}
        </RadioGroup>
      ) : (
        <Callout title="没有 workflow" hideTitle>
          <code>{t.Ref}</code> 中没有 <code>.kiln/workflows/*.yml</code>。
        </Callout>
      )}
      {data.Dispatchable === 0 && (
        <Callout tone="warning" title="这个 ref 下没有可以手动触发的 workflow。">
          要手动运行，先在 workflow 的 <code>on</code> 中加上 <code>workflow_dispatch</code> 并推送；或者换一个 ref 再读取。
        </Callout>
      )}
    </>
  )
}

function DispatchForm({ data }: { data: Data }) {
  const f = data.Form
  const refErr = fieldError(f, 'ref')
  const inErr = fieldError(f, 'inputs')
  const canSubmit = !data.Target || data.Dispatchable > 0
  return (
    <Form action="/dispatch" className={css.form} aria-label="启动构建">
      <input type="hidden" name="request_id" value={data.RequestID} />
      <input type="hidden" name="repo" value={data.Repo} />
      <FormAlert data={data} />

      <section className={css.step} aria-labelledby="step-repo">
        <h2 className={css.stepTitle} id="step-repo">
          1. 仓库
        </h2>
        <div className={css.repoPicked}>
          <RepoIcon aria-hidden />
          <span className={css.repoName}>{data.Repo}</span>
          <a href="/runs/new">更换仓库</a>
        </div>
        {data.FromFilter && <p className={css.muted}>按运行列表的筛选预选，请确认。</p>}
      </section>

      <section className={css.step} aria-labelledby="step-ref">
        <h2 className={css.stepTitle} id="step-ref">
          2. ref
        </h2>
        <FormControl>
          <FormControl.Label>分支或 tag</FormControl.Label>
          <TextInput
            name="ref"
            defaultValue={fieldValue(f, 'ref')}
            placeholder={data.DefaultBranch}
            spellCheck={false}
            autoCapitalize="off"
            className={css.mono}
            validationStatus={refErr ? 'error' : undefined}
            aria-invalid={refErr ? true : undefined}
          />
          <FormControl.Caption>
            留空表示默认分支 {data.DefaultBranch}；可以写分支名、tag 名（如 v1.2.0）或完整 ref（如 refs/tags/v1.2.0）。改了 ref 后点"读取 workflow"刷新下面的列表。
          </FormControl.Caption>
          {refErr && <FormControl.Validation variant="error"><Msg text={refErr} /></FormControl.Validation>}
        </FormControl>
        <div>
          <LoadButton />
        </div>
        {data.Target && (
          <p className={css.muted} role="status">
            已读取 <code>{data.Target.Ref}</code>（commit <code>{short(data.Target.SHA)}</code>）中的 workflow。
          </p>
        )}
      </section>

      <section className={css.step} aria-labelledby="step-wf">
        <h2 className={css.stepTitle} id="step-wf">
          3. workflow
        </h2>
        <WorkflowChoice data={data} />
        <FormControl>
          <FormControl.Label>inputs（可选）</FormControl.Label>
          <Textarea
            name="inputs"
            rows={4}
            defaultValue={fieldValue(f, 'inputs')}
            spellCheck={false}
            autoCapitalize="off"
            placeholder={'version=1.2.3\ndry_run=true'}
            className={css.mono}
            block
            validationStatus={inErr ? 'error' : undefined}
            aria-invalid={inErr ? true : undefined}
          />
          <FormControl.Caption>
            每行一个 <code>key=value</code>，值可以包含 =，空行忽略。没有填写的 input 使用 workflow 中的默认值。inputs 会保存在 run 中、显示在 Dashboard 上，不要填写密钥。
          </FormControl.Caption>
          {inErr && (
            <FormControl.Validation variant="error">
              <span className={css.lines}><Msg text={inErr} /></span>
            </FormControl.Validation>
          )}
        </FormControl>
      </section>

      <p className={css.muted}>手动触发的 run 中，发布 job 会在规划时判为失败：发布只能由推送 tag 触发。</p>
      <div className={css.actions}>
        {canSubmit && <SubmitButton variant="primary">启动构建</SubmitButton>}
        <LinkButton href={data.ListURL}>取消</LinkButton>
      </div>
    </Form>
  )
}

function RunNew({ data }: PageProps<Data>) {
  return (
    <div className={css.page}>
      <nav className={css.crumbs} aria-label="位置">
        <a href={data.ListURL}>运行</a>
        <span aria-hidden>/</span>
        <span>启动构建</span>
      </nav>
      <div className={css.head}>
        <h1 className={css.title}>启动构建</h1>
        <p className={css.lead}>
          手动运行声明了 <code>on.workflow_dispatch</code> 的 workflow：先选定仓库，再选 ref 和 workflow。
        </p>
      </div>
      {data.Picker ? (
        <>
          <FormAlert data={data} />
          <RepoPicker data={data} picker={data.Picker} />
        </>
      ) : (
        <DispatchForm data={data} />
      )}
    </div>
  )
}

export default definePage(schema, RunNew)

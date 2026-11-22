import { RepoIcon, SearchIcon } from '@primer/octicons-react'
import { Button, FormControl, LinkButton, Radio, RadioGroup, Textarea, TextInput } from '@primer/react'
import { Fragment, useState, type MouseEvent } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useRefreshLock } from '../app/live'
import { useRouter } from '../app/router'
import { Box, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { Form, formBody, SubmitButton } from '../components/Form'
import { fieldError, fieldValue, formStateSchema, list, textSchema } from '../lib/envelope'
import { short } from '../lib/format'
import { Msg, Trans, useLocale, useT } from '../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey, type Params } from '../i18n/messages'
import css from './run_new.module.css'

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
  /** 读取 workflow 列表失败的原因（消息或原样的说明）。 */
  TargetErr: textSchema,
  Form: formStateSchema,
  /** inputs 的各个问题（服务端给的消息）；有问题时 Form 的 inputs 字段上是汇总说明。 */
  InputProblems: list(textSchema),
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
  const t = useT()
  if (!data.Form?.Failed) return null
  const general = fieldError(data.Form, '_')
  return (
    <Callout tone="critical" title={data.Attempted ? t('runNew.notStarted') : t('runNew.fixTitle')} hideTitle={!data.Attempted}>
      {general ? <Msg text={general} /> : t('runNew.fixHint')}
    </Callout>
  )
}

/** inputsText 说明 workflow 声明的 inputs，例如"inputs：version（string，必填）；mode（choice，默认 fast，可选 fast、full）"。 */
export function inputsText(w: Workflow, locale: Locale = defaultLocale): string {
  const t = (id: MsgKey, params?: Params) => translate(locale, msg(id, params))
  if (!w.Dispatchable) return t('runNew.notDispatchable')
  if (w.Inputs.length === 0) return t('runNew.noInputs')
  const items = w.Inputs.map((i) => {
    const attrs = [i.Type]
    if (i.Required) attrs.push(t('runNew.inputRequired'))
    if (i.Default) attrs.push(t('runNew.inputDefault', { value: i.Default }))
    if (i.Options.length > 0) attrs.push(t('runNew.inputOptions', { options: i.Options.join(t('list.enum')) }))
    const item = t('runNew.inputItem', { name: i.Name, attrs: attrs.join(t('list.separator')) })
    // 描述是 workflow 中写的原文，不翻译
    return i.Description ? `${item} ${i.Description}` : item
  })
  return t('runNew.inputs', { items: items.join(t('runNew.inputsSep')) })
}

function RepoPicker({ data, picker: s }: { data: Data; picker: Picker }) {
  const t = useT()
  const repoErr = fieldError(data.Form, 'repo')
  return (
    <section className={css.step} aria-labelledby="pick-h">
      <h2 className={css.stepTitle} id="pick-h">
        {t('runNew.pickRepo')}
      </h2>
      <Form action="/runs/new" method="get" className={css.search} aria-label={t('runNew.searchRepos')}>
        <FormControl>
          <FormControl.Label visuallyHidden>{t('runNew.searchRepos')}</FormControl.Label>
          <TextInput
            type="search"
            name="q"
            defaultValue={s.Query}
            placeholder={t('runNew.searchPlaceholder')}
            spellCheck={false}
            autoCapitalize="off"
            leadingVisual={SearchIcon}
            block
            validationStatus={repoErr ? 'error' : undefined}
            aria-invalid={repoErr ? true : undefined}
          />
          {repoErr && <FormControl.Validation variant="error"><Msg text={repoErr} /></FormControl.Validation>}
        </FormControl>
        <Button type="submit">{t('runNew.search')}</Button>
        {s.Query && (
          <LinkButton href="/runs/new" variant="invisible">
            {t('runNew.clear')}
          </LinkButton>
        )}
      </Form>
      <Box>
        <div className={css.boxHead} role="status">
          {s.Query ? t('runNew.matchCount', { query: s.Query, count: s.Total }) : t('runNew.repoCount', { count: s.Total })}
          {s.Pages > 1 && t('runNew.pageOf', { page: s.Page, pages: s.Pages })}
        </div>
        {s.Repos.length > 0 ? (
          <ul className={css.repos} aria-label={t('runNew.repos')}>
            {s.Repos.map((r) => (
              <BoxRow as="li" key={r.FullName} hoverable>
                <RepoIcon className={css.repoIcon} aria-hidden />
                <span className={css.repoText}>
                  <span className={css.repoName}>{r.FullName}</span>
                  <span className={css.muted}>
                    {r.DefaultBranch && t('runNew.defaultBranch', { branch: r.DefaultBranch })}
                    {r.Private ? 'private' : 'public'}
                  </span>
                </span>
                <LinkButton
                  href={`/runs/new?${new URLSearchParams({ repo: r.FullName }).toString()}`}
                  size="small"
                  aria-label={t('runNew.chooseRepo', { repo: r.FullName })}
                >
                  {t('runNew.choose')}
                </LinkButton>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <p className={css.none}>{s.Query ? t('runNew.noMatch') : t('runNew.noRepos')}</p>
        )}
      </Box>
      {s.Pages > 1 && (
        <nav className={css.pager} aria-label={t('pager.label')}>
          {s.PrevURL && (
            <LinkButton href={s.PrevURL} rel="prev" size="small">
              {t('pager.prev')}
            </LinkButton>
          )}
          <span className={css.muted}>{t('pager.page', { page: s.Page, pages: s.Pages })}</span>
          {s.NextURL && (
            <LinkButton href={s.NextURL} rel="next" size="small">
              {t('pager.next')}
            </LinkButton>
          )}
        </nav>
      )}
    </section>
  )
}

/** LoadButton 按表单中的 ref 重新读取 workflow 列表（POST /runs/new），保留已填写的内容，不创建 run。 */
function LoadButton() {
  const t = useT()
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
      {t('runNew.load')}
    </Button>
  )
}

function WorkflowChoice({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const f = data.Form
  const tg = data.Target
  const wfErr = fieldError(f, 'workflow')
  if (!tg) {
    return (
      <>
        {data.TargetErr && (
          <Callout tone="warning" title={t('runNew.targetErrTitle')}>
            <Msg text={data.TargetErr} /> {t('runNew.targetErrHint')}
          </Callout>
        )}
        <FormControl>
          <FormControl.Label>{t('runNew.workflowFile')}</FormControl.Label>
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
            {t('runNew.workflowFileHint')}
            {fieldError(f, 'ref') && t('runNew.workflowFileRefHint')}
          </FormControl.Caption>
          {wfErr && <FormControl.Validation variant="error"><Msg text={wfErr} /></FormControl.Validation>}
        </FormControl>
      </>
    )
  }
  return (
    <>
      {tg.Workflows.length > 0 ? (
        <RadioGroup name="workflow" aria-invalid={wfErr ? true : undefined}>
          <RadioGroup.Label>{t('runNew.pickWorkflow')}</RadioGroup.Label>
          {tg.Workflows.map((w) => (
            <FormControl key={w.File} disabled={!w.Dispatchable}>
              <Radio value={w.File} defaultChecked={w.File === fieldValue(f, 'workflow')} aria-invalid={wfErr ? true : undefined} />
              <FormControl.Label>
                {w.Name || w.File} <span className={css.file}>{w.File}</span>
              </FormControl.Label>
              <FormControl.Caption>
                {inputsText(w, locale)}
                {w.ConfigError && <span className={css.warn}> {t('runNew.workflowConfigError')}</span>}
              </FormControl.Caption>
            </FormControl>
          ))}
          {wfErr && <RadioGroup.Validation variant="error"><Msg text={wfErr} /></RadioGroup.Validation>}
        </RadioGroup>
      ) : (
        <Callout title={t('runNew.noWorkflowsTitle')} hideTitle>
          <Trans id="runNew.noWorkflows" params={{ ref: tg.Ref }} tags={{ code: (c) => <code>{c}</code> }} />
        </Callout>
      )}
      {data.Dispatchable === 0 && (
        <Callout tone="warning" title={t('runNew.noDispatchableTitle')}>
          <Trans id="runNew.noDispatchable" tags={{ code: (c) => <code>{c}</code> }} />
        </Callout>
      )}
    </>
  )
}

function DispatchForm({ data }: { data: Data }) {
  const t = useT()
  const f = data.Form
  const refErr = fieldError(f, 'ref')
  const inErr = fieldError(f, 'inputs')
  const canSubmit = !data.Target || data.Dispatchable > 0
  return (
    <Form action="/dispatch" className={css.form} aria-label={t('runNew.pageTitle')}>
      <input type="hidden" name="request_id" value={data.RequestID} />
      <input type="hidden" name="repo" value={data.Repo} />
      <FormAlert data={data} />

      <section className={css.step} aria-labelledby="step-repo">
        <h2 className={css.stepTitle} id="step-repo">
          {t('runNew.stepRepo')}
        </h2>
        <div className={css.repoPicked}>
          <RepoIcon aria-hidden />
          <span className={css.repoName}>{data.Repo}</span>
          <a href="/runs/new">{t('runNew.changeRepo')}</a>
        </div>
        {data.FromFilter && <p className={css.muted}>{t('runNew.fromFilter')}</p>}
      </section>

      <section className={css.step} aria-labelledby="step-ref">
        <h2 className={css.stepTitle} id="step-ref">
          {t('runNew.stepRef')}
        </h2>
        <FormControl>
          <FormControl.Label>{t('runNew.refLabel')}</FormControl.Label>
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
          <FormControl.Caption>{t('runNew.refHint', { branch: data.DefaultBranch })}</FormControl.Caption>
          {refErr && <FormControl.Validation variant="error"><Msg text={refErr} /></FormControl.Validation>}
        </FormControl>
        <div>
          <LoadButton />
        </div>
        {data.Target && (
          <p className={css.muted} role="status">
            <Trans id="runNew.loaded" params={{ ref: data.Target.Ref, sha: short(data.Target.SHA) }} tags={{ code: (c) => <code>{c}</code> }} />
          </p>
        )}
      </section>

      <section className={css.step} aria-labelledby="step-wf">
        <h2 className={css.stepTitle} id="step-wf">
          {t('runNew.stepWorkflow')}
        </h2>
        <WorkflowChoice data={data} />
        <FormControl>
          <FormControl.Label>{t('runNew.inputsLabel')}</FormControl.Label>
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
            <Trans id="runNew.inputsHint" tags={{ code: (c) => <code>{c}</code> }} />
          </FormControl.Caption>
          {inErr && (
            <FormControl.Validation variant="error">
              <span className={css.lines}>
                <Msg text={inErr} />
                {data.InputProblems.map((p, i) => (
                  <Fragment key={i}>
                    {'\n'}
                    <Msg text={p} />
                  </Fragment>
                ))}
              </span>
            </FormControl.Validation>
          )}
        </FormControl>
      </section>

      <p className={css.muted}>{t('runNew.publishNote')}</p>
      <div className={css.actions}>
        {canSubmit && <SubmitButton variant="primary">{t('runNew.pageTitle')}</SubmitButton>}
        <LinkButton href={data.ListURL}>{t('form.cancel')}</LinkButton>
      </div>
    </Form>
  )
}

function RunNew({ data }: PageProps<Data>) {
  const t = useT()
  return (
    <div className={css.page}>
      <nav className={css.crumbs} aria-label={t('nav.breadcrumb')}>
        <a href={data.ListURL}>{t('nav.runs')}</a>
        <span aria-hidden>/</span>
        <span>{t('runNew.pageTitle')}</span>
      </nav>
      <div className={css.head}>
        <h1 className={css.title}>{t('runNew.pageTitle')}</h1>
        <p className={css.lead}>
          <Trans id="runNew.lead" tags={{ code: (c) => <code>{c}</code> }} />
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

import { FileIcon, LinkExternalIcon } from '@primer/octicons-react'
import { Button, Dialog, Label, Link } from '@primer/react'
import { useEffect, useRef, useState } from 'react'
import { z } from 'zod'
import { useRefreshLock } from '../../app/live'
import { Box, BoxHeader, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { Ago } from '../../components/Time'
import { Msg, Trans, useLocale, useT } from '../../i18n/locale'
import { isEmpty, msg, translate, type Locale, type MsgKey, type Params, type Text } from '../../i18n/messages'
import { loginURL } from '../../lib/api'
import { enumLabel, short } from '../../lib/format'
import { lookup } from '../../lib/lookup'
import table from '../settings_people.module.css'
import { sourceText, type DesignBasisData, type DocMeta, type RelatedTask } from './docs'
import { devModeText } from './parts'
import css from './DesignBasis.module.css'

// 设计依据：展示冻结时保存的文档元数据与来源，正文只在点击后单独读取。
// 页面数据每 5 秒刷新，但不含正文；刷新不会重新扫描 Outline，也不会重新读已经看过的正文。
// 历史快照没有记录来源时如实标注，绝不用当前仓库绑定补造。界面文字按语言显示；文档标题、文档集名称和正文是外部文档的
// 原文，原样显示。

/** here 是当前页面地址：登录后回到这里。 */
const here = () => window.location.pathname + window.location.search

/** docContentSchema 是正文接口的响应（internal/server/web/docs.go 的 docContent）。 */
const docContentSchema = z.object({
  doc_id: z.string(),
  title: z.string(),
  revision: z.number(),
  sha256: z.string(),
  content: z.string(),
})
type DocContent = z.infer<typeof docContentSchema>

/**
 * ContentError 是读取正文失败的原因。只保存结构化的信息，显示时按当前语言翻译（contentErrorText）：切换语言时打开的
 * 对话框立即换成新语言的说明，不关闭、不重新读取。
 */
type ContentError = {
  /** 服务端给的错误码（docs.go 的 docContentError.code）；没有时为空。 */
  code: string
  /** 服务端给的说明（docContentError.message）：只在错误码不认识或缺失时显示。 */
  fallback: string
  /** 登录已过期：提示重新登录，不提供重试。 */
  relogin: boolean
  /** 前端自己发现的错误：badResponse（响应格式不对）、network（连接不上）；服务端给了错误响应时为 null。 */
  client: 'badResponse' | 'network' | null
  /** HTTP 状态码（没有响应时为 0）。 */
  status: number
}

type View =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'ready'; doc: DocContent }
  | { kind: 'purged' }
  | { kind: 'error'; error: ContentError }

/** contentErrorCodes 是正文接口的错误码（docs.go），每个都有词典中的说明 docContentError.<code>。 */
const contentErrorCodes = new Set(['unauthorized', 'content_purged', 'basis_conflict', 'no_basis', 'not_found', 'doc_not_found', 'invalid_request', 'internal'])

/** contentErrorText 返回读取正文失败的说明：认识的错误码按词典翻译，不认识或没有错误码时显示服务端的说明，都没有时说明 HTTP 状态。 */
export function contentErrorText(e: ContentError, locale: Locale): string {
  const t = (id: MsgKey, params?: Params) => translate(locale, msg(id, params))
  if (e.client === 'network') return t('docContentError.network')
  if (e.client === 'badResponse') return t('docContentError.badResponse')
  switch (contentErrorCodes.has(e.code) ? e.code : '') {
    case 'unauthorized':
      return t('docContentError.unauthorized')
    case 'content_purged':
      return t('docContentError.content_purged')
    case 'basis_conflict':
      return t('docContentError.basis_conflict')
    case 'no_basis':
      return t('docContentError.no_basis')
    case 'not_found':
      return t('docContentError.not_found')
    case 'doc_not_found':
      return t('docContentError.doc_not_found')
    case 'invalid_request':
      return t('docContentError.invalid_request')
    case 'internal':
      return t('docContentError.internal')
  }
  return e.fallback || t('docContentError.http', { status: e.status })
}

/** field 返回响应体中字符串字段 name 的值；没有或不是字符串时为空。 */
function field(body: unknown, name: string): string {
  if (body === null || typeof body !== 'object' || !(name in body)) return ''
  const v: unknown = Reflect.get(body, name)
  return typeof v === 'string' ? v : ''
}

/** contentKey 标识一篇文档的一次冻结版本（正文缓存的键）。 */
function contentKey(base: string, d: DocMeta): string {
  return `${base}|${d.DocID}|${d.SHA256}`
}

/** ContentDialog 按需读取一篇冻结文档的正文。打开期间暂停定时刷新；关闭后保留已读的正文。 */
function ContentDialog({
  base,
  doc,
  purged,
  initial,
  cache,
  onPurged,
  onClose,
}: {
  base: string
  doc: DocMeta
  purged: boolean
  /** 之前已经读过的正文；不为空时直接显示，不再访问服务器。 */
  initial: DocContent | null
  /** 已读过的正文（按文档与指纹缓存，关闭再打开、页面刷新都不用重新读）。 */
  cache: { current: Map<string, DocContent> }
  onPurged: () => void
  onClose: () => void
}) {
  const t = useT()
  const { locale } = useLocale()
  const key = contentKey(base, doc)
  const [attempt, setAttempt] = useState(0)
  const [view, setView] = useState<View>(() =>
    initial ? { kind: 'ready', doc: initial } : purged ? { kind: 'purged' } : { kind: 'loading' },
  )
  const purgedCb = useRef(onPurged)
  useRefreshLock(true)

  useEffect(() => {
    purgedCb.current = onPurged
  }, [onPurged])

  // 每次打开、重试时读取一次；已经缓存或已清理时不再访问服务器。关闭对话框时中止请求。
  useEffect(() => {
    if (initial || purged) return
    const ctrl = new AbortController()
    void (async () => {
      try {
        const res = await fetch(`${base}?doc_id=${encodeURIComponent(doc.DocID)}`, {
          headers: { Accept: 'application/json' },
          credentials: 'same-origin',
          signal: ctrl.signal,
        })
        const body: unknown = await res.json().catch(() => null)
        if (ctrl.signal.aborted) return
        if (res.status === 401) {
          const code = field(body, 'code') || 'unauthorized'
          setView({ kind: 'error', error: { code, fallback: field(body, 'message'), relogin: true, client: null, status: res.status } })
          return
        }
        if (res.status === 410) {
          purgedCb.current()
          setView({ kind: 'purged' })
          return
        }
        if (!res.ok) {
          setView({ kind: 'error', error: { code: field(body, 'code'), fallback: field(body, 'message'), relogin: false, client: null, status: res.status } })
          return
        }
        const parsed = docContentSchema.safeParse(body)
        if (!parsed.success) {
          setView({ kind: 'error', error: { code: '', fallback: '', relogin: false, client: 'badResponse', status: res.status } })
          return
        }
        cache.current.set(key, parsed.data)
        setView({ kind: 'ready', doc: parsed.data })
      } catch (err) {
        if (ctrl.signal.aborted || (err instanceof DOMException && err.name === 'AbortError')) return
        setView({ kind: 'error', error: { code: '', fallback: '', relogin: false, client: 'network', status: 0 } })
      }
    })()
    return () => ctrl.abort()
  }, [base, doc.DocID, key, purged, initial, cache, attempt])

  return (
    <Dialog title={doc.Title} subtitle={t('designBasis.dialogSubtitle', { revision: doc.Revision })} onClose={onClose} width="large">
      <div className={css.dialog}>
        {view.kind === 'loading' && <p className={css.muted}>{t('designBasis.loading')}</p>}
        {view.kind === 'purged' && (
          <Callout tone="warning" title={t('designBasis.purgedTitle')}>
            {t('designBasis.purgedBody')}
          </Callout>
        )}
        {view.kind === 'error' && (
          <Callout tone="critical" title={t('designBasis.failedTitle')}>
            {contentErrorText(view.error, locale)}
            {view.error.relogin && (
              <>
                {' '}
                <Link href={loginURL(here())}>{t('designBasis.relogin')}</Link>
              </>
            )}
          </Callout>
        )}
        {view.kind === 'ready' && (
          <>
            <p className={css.muted}>
              <Trans
                id="designBasis.plain"
                params={{ digest: short(view.doc.sha256) }}
                tags={{ code: (c) => <code title={view.doc.sha256}>{c}</code> }}
              />
            </p>
            <pre className={css.content}>{view.doc.content}</pre>
          </>
        )}
        <div className={css.dialogActions}>
          {view.kind === 'error' && !view.error.relogin && (
            <Button
              onClick={() => {
                setView({ kind: 'loading' })
                setAttempt((n) => n + 1)
              }}
            >
              {t('designBasis.retry')}
            </Button>
          )}
          <Button variant="primary" onClick={onClose}>
            {t('designBasis.close')}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}

/** DocsTable 是冻结依据中的文档：标题、修订、指纹和按需查看正文（清理后只显示元数据）。 */
function DocsTable({ docs, base }: { docs: DocMeta[]; base: string }) {
  const [open, setOpen] = useState<{ doc: DocMeta; initial: DocContent | null } | null>(null)
  const [cleaned, setCleaned] = useState<ReadonlySet<string>>(() => new Set())
  const cache = useRef(new Map<string, DocContent>())
  const t = useT()
  const col = { doc: t('designBasis.colDoc'), revision: t('designBasis.colRevision'), digest: t('designBasis.colDigest'), content: t('designBasis.colContent') }
  return (
    <>
      <table className={table.table} aria-label={t('designBasis.docsLabel')}>
        <thead>
          <tr>
            <th scope="col">{col.doc}</th>
            <th scope="col">{col.revision}</th>
            <th scope="col">{col.digest}</th>
            <th scope="col">{col.content}</th>
          </tr>
        </thead>
        <tbody>
          {docs.map((d) => {
            const purgedDoc = d.Purged || cleaned.has(contentKey(base, d))
            return (
              <tr key={d.DocID}>
                <td data-label={col.doc}>
                  <FileIcon className={css.fileIcon} aria-hidden /> {d.Title || d.DocID}
                  {d.URL && (
                    <a className={css.ext} href={d.URL} target="_blank" rel="noreferrer" title={t('designBasis.openTitle')}>
                      <LinkExternalIcon aria-label={t('designBasis.open')} />
                    </a>
                  )}
                  <div className={css.docID}>
                    <code>{d.DocID}</code>
                  </div>
                </td>
                <td data-label={col.revision} className={table.nowrap}>
                  r{d.Revision}
                </td>
                <td data-label={col.digest} className={table.mono}>
                  <code title={d.SHA256}>{short(d.SHA256)}</code>
                </td>
                <td data-label={col.content} className={table.nowrap}>
                  {purgedDoc ? (
                    <span className={css.muted}>{t('designBasis.docPurged')}</span>
                  ) : (
                    <Button size="small" onClick={() => setOpen({ doc: d, initial: cache.current.get(contentKey(base, d)) ?? null })}>
                      {t('designBasis.view')}
                    </Button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      {open && (
        <ContentDialog
          key={contentKey(base, open.doc)}
          base={base}
          doc={open.doc}
          initial={open.initial}
          cache={cache}
          purged={open.doc.Purged || cleaned.has(contentKey(base, open.doc))}
          onPurged={() =>
            setCleaned((prev) => {
              const next = new Set(prev)
              next.add(contentKey(base, open.doc))
              return next
            })
          }
          onClose={() => setOpen(null)}
        />
      )}
    </>
  )
}

/**
 * DesignBasis 是"设计依据"分区。declaredSource / basisSource 只在 AI 请求、AI 任务页面上给出：
 * 前者是数据库记录的来源，后者是实际解析出的依据，两者不同时如实分别展示。
 */
export function DesignBasis({
  basis,
  contentBase,
  emptyText,
  declaredSource,
  declared = true,
  basisSource,
  workItemURL,
  snapshotSetID,
  error,
  integrityHint,
}: {
  basis: DesignBasisData | null
  contentBase: string
  emptyText: string
  declaredSource?: string
  declared?: boolean
  basisSource?: string
  workItemURL?: string
  snapshotSetID?: number
  /** 记录不一致的原因（服务端给的消息）。 */
  error?: Text
  integrityHint?: string
}) {
  const t = useT()
  const { locale } = useLocale()
  const show = basis !== null && basis.Frozen && basis.Source !== 'none'
  const collection = basis && basis.CollectionName ? basis.CollectionName : basis?.CollectionID
  const unknown = t('designBasis.unknown')
  return (
    <Box as="section" aria-labelledby="design-basis">
      <BoxHeader id="design-basis" title={t('designBasis.title')}>
        {basis && <Label variant={show ? 'accent' : 'secondary'}>{show ? sourceText(basis.Source, locale) : t('designBasis.none')}</Label>}
      </BoxHeader>
      <BoxRow className={css.block}>
        {!isEmpty(error) && (
          <Callout tone="warning" title={t('designBasis.errorTitle')}>
            <Msg text={error} /> {t('designBasis.errorNote')}
          </Callout>
        )}
        {declaredSource !== undefined && (
          <p className={css.p}>
            {t('designBasis.declared', { source: declared ? sourceText(declaredSource, locale) : t('designBasis.notDeclared') })}
            {workItemURL && (
              <>
                {' '}
                · <Link href={workItemURL}>{t('designBasis.workItem')}</Link>
              </>
            )}
            {snapshotSetID !== undefined && snapshotSetID > 0 && (
              <>
                {' '}
                · {t('designBasis.snapshotSet')} <code>#{snapshotSetID}</code>
              </>
            )}
          </p>
        )}
        {basisSource !== undefined && basisSource !== declaredSource && (
          <p className={css.p}>
            {t('designBasis.actual', { source: sourceText(basisSource, locale) })}
            <span className={css.muted}>{t('designBasis.actualNote')}</span>
          </p>
        )}
        {!show ? (
          <p className={css.muted}>{emptyText}</p>
        ) : (
          <>
            <dl className={css.kv}>
              <dt>{t('designBasis.frozenAt')}</dt>
              <dd>{basis.FrozenAt ? <Ago t={basis.FrozenAt} /> : unknown}</dd>
              <dt>{t('designBasis.source')}</dt>
              <dd>
                {basis.SourceKnown ? (
                  <>
                    {basis.Provider || t('designBasis.unknownProvider')}
                    {basis.SiteURL ? (
                      <>
                        {' '}
                        ·{' '}
                        <a href={basis.SiteURL} target="_blank" rel="noreferrer">
                          {basis.SiteURL.replace(/^https?:\/\//, '')}
                        </a>
                      </>
                    ) : null}
                    {collection && (
                      <>
                        {' '}
                        ·{' '}
                        {basis.CollectionURL ? (
                          <a href={basis.CollectionURL} target="_blank" rel="noreferrer">
                            {collection}
                          </a>
                        ) : (
                          <span>{collection}</span>
                        )}
                      </>
                    )}
                    {basis.BindingGeneration > 0 && <span className={css.muted}>{t('designBasis.generation', { n: basis.BindingGeneration })}</span>}
                  </>
                ) : (
                  <span className={css.muted}>{t('designBasis.sourceUnknown')}</span>
                )}
              </dd>
              <dt>{t('designBasis.docCount')}</dt>
              <dd>
                {basis.DocCount}
                {basis.DocCount === 0 && <span className={css.muted}>{t('designBasis.emptySet')}</span>}
              </dd>
              <dt>{t('designBasis.digest')}</dt>
              <dd>
                <code className={css.digest} title={basis.ManifestDigest}>
                  {basis.ManifestDigest || t('designBasis.notRecorded')}
                </code>
              </dd>
            </dl>
            {integrityHint && <p className={css.muted}>{integrityHint}</p>}
            {!isEmpty(basis.IntegrityError) && (
              <Callout tone="warning" title={t('designBasis.integrityTitle')}>
                <Msg text={basis.IntegrityError} /> {t('designBasis.integrityNote')}
              </Callout>
            )}
            {basis.ContentPurgedAt && (basis.DocCount > 0 || basis.Docs.length > 0) && (
              <p className={css.muted}>
                <Trans id="designBasis.purged" tags={{ ago: () => <Ago t={basis.ContentPurgedAt} /> }} />
              </p>
            )}
            {basis.Docs.length > 0 ? (
              <DocsTable docs={basis.Docs} base={contentBase} />
            ) : (
              <p className={css.muted}>{t('designBasis.noDocs')}</p>
            )}
          </>
        )}
      </BoxRow>
    </Box>
  )
}

/** RequestDesignBasis 是 AI 请求页面的设计依据。 */
export function RequestDesignBasis({ view }: { view: RequestDocsViewData }) {
  const t = useT()
  return (
    <DesignBasis
      basis={view.Basis}
      contentBase={`/ai/requests/${view.RequestID}/docs/content`}
      declaredSource={view.DeclaredSource}
      declared={view.Declared}
      workItemURL={view.WorkItemURL}
      snapshotSetID={view.SnapshotSetID}
      error={view.Error}
      emptyText={t(requestEmptyText(view))}
    />
  )
}

/** TaskDesignBasis 是 AI 任务页面的设计依据。 */
export function TaskDesignBasis({ view }: { view: TaskDocsViewData }) {
  const t = useT()
  return (
    <DesignBasis
      basis={view.Basis}
      contentBase={`/ai/tasks/${view.TaskID}/docs/content`}
      declaredSource={view.DeclaredSource}
      basisSource={view.BasisSource}
      error={view.Error}
      emptyText={t('designBasis.taskEmpty')}
    />
  )
}

/** requestEmptyText 按请求记录的来源解释"没有依据"，不把正常状态当成系统错误。 */
function requestEmptyText(view: RequestDocsViewData) {
  switch (view.DeclaredSource) {
    case 'snapshot_set':
      return 'designBasis.requestEmpty.snapshot_set'
    case 'work_item':
      return 'designBasis.requestEmpty.work_item'
    case 'legacy':
      return 'designBasis.requestEmpty.legacy'
    case 'unplanned':
      return 'designBasis.requestEmpty.unplanned'
    default:
      return 'designBasis.requestEmpty.none'
  }
}

/** 由 schema 推断的视图类型在这里单独命名，避免页面文件互相依赖。 */
type RequestDocsViewData = {
  RequestID: number
  DeclaredSource: string
  Declared: boolean
  WorkItemID: number
  WorkItemURL: string
  SnapshotSetID: number
  Basis: DesignBasisData | null
  Error: Text
}
type TaskDocsViewData = {
  TaskID: number
  DeclaredSource: string
  BasisSource: string
  Basis: DesignBasisData | null
  Error: Text
}

/** 依据关系的颜色；名称在词典 docsRelation.* 中，未知的关系原样显示。 */
const relationVariants: Record<string, 'success' | 'accent' | 'attention' | 'danger' | 'secondary'> = {
  same_basis: 'success',
  refs_match: 'success',
  claimed_dynamic: 'accent',
  legacy_pinned: 'secondary',
  no_basis: 'secondary',
  mismatch: 'danger',
  unverifiable: 'attention',
}

/** RelatedTasksTable 是工作项关联的开发 / 审计任务，以及它们与工作项冻结依据的关系。 */
export function RelatedTasksTable({ tasks }: { tasks: RelatedTask[] }) {
  const t = useT()
  const { locale } = useLocale()
  const title = t('relatedTasks.title')
  if (tasks.length === 0) {
    return (
      <Box as="section" aria-labelledby="related-tasks">
        <BoxHeader id="related-tasks" title={title} />
        <BoxRow>
          <p className={css.muted}>{t('relatedTasks.empty')}</p>
        </BoxRow>
      </Box>
    )
  }
  const col = { task: t('relatedTasks.colTask'), type: t('relatedTasks.colType'), mode: t('relatedTasks.colMode'), relation: t('relatedTasks.colRelation') }
  return (
    <Box as="section" aria-labelledby="related-tasks">
      <BoxHeader id="related-tasks" title={title} />
      <table className={table.table} aria-label={title}>
        <thead>
          <tr>
            <th scope="col">{col.task}</th>
            <th scope="col">{col.type}</th>
            <th scope="col">{col.mode}</th>
            <th scope="col">{col.relation}</th>
          </tr>
        </thead>
        <tbody>
          {tasks.map((task) => (
            <tr key={task.TaskID}>
              <td data-label={col.task}>
                <Link href={task.TaskURL}>#{task.TaskID}</Link>
                <div className={css.docID}>
                  <Link href={task.RequestURL}>{t('relatedTasks.request', { id: task.RequestID })}</Link>
                </div>
              </td>
              <td data-label={col.type}>{enumLabel(locale, 'relatedTaskType', task.Type)}</td>
              <td data-label={col.mode}>{task.Mode ? devModeText(task.Mode, locale) : '—'}</td>
              <td data-label={col.relation}>
                <Label variant={lookup(relationVariants, task.Relation) ?? 'secondary'}>{enumLabel(locale, 'docsRelation', task.Relation)}</Label>
                {!isEmpty(task.Note) && (
                  <div className={css.muted}>
                    <Msg text={task.Note} />
                    {!isEmpty(task.NoteDetail) && (
                      <>
                        {t('list.colon')}
                        <Msg text={task.NoteDetail} />
                      </>
                    )}
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Box>
  )
}

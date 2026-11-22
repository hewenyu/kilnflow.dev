import { FileIcon, LinkExternalIcon } from '@primer/octicons-react'
import { Button, Dialog, Label, Link } from '@primer/react'
import { useEffect, useRef, useState } from 'react'
import { z } from 'zod'
import { useRefreshLock } from '../../app/live'
import { Box, BoxHeader, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { Ago } from '../../components/Time'
import { loginURL } from '../../lib/api'
import { short } from '../../lib/format'
import { lookup } from '../../lib/lookup'
import table from '../settings_people.module.css'
import { sourceText, type DesignBasisData, type DocMeta, type RelatedTask } from './docs'
import css from './DesignBasis.module.css'

// 设计依据：展示冻结时保存的文档元数据与来源，正文只在点击后单独读取。
// 页面数据每 5 秒刷新，但不含正文；刷新不会重新扫描 Outline，也不会重新读已经看过的正文。
// 历史快照没有记录来源时如实标注，绝不用当前仓库绑定补造。

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

type View =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'ready'; doc: DocContent }
  | { kind: 'purged' }
  | { kind: 'error'; message: string; relogin: boolean }

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
          setView({ kind: 'error', message: '登录已过期，请重新登录后再查看。', relogin: true })
          return
        }
        if (res.status === 410) {
          purgedCb.current()
          setView({ kind: 'purged' })
          return
        }
        if (!res.ok) {
          const message =
            body !== null && typeof body === 'object' && 'message' in body && typeof body.message === 'string'
              ? body.message
              : `读取失败（HTTP ${res.status}）。`
          setView({ kind: 'error', message, relogin: false })
          return
        }
        const parsed = docContentSchema.safeParse(body)
        if (!parsed.success) {
          setView({ kind: 'error', message: '服务器返回的正文格式不对，请刷新页面重试。', relogin: false })
          return
        }
        cache.current.set(key, parsed.data)
        setView({ kind: 'ready', doc: parsed.data })
      } catch (err) {
        if (ctrl.signal.aborted || (err instanceof DOMException && err.name === 'AbortError')) return
        setView({ kind: 'error', message: '连接不上 Kiln Server。检查网络后重试。', relogin: false })
      }
    })()
    return () => ctrl.abort()
  }, [base, doc.DocID, key, purged, initial, cache, attempt])

  return (
    <Dialog title={doc.Title} subtitle={`修订 r${doc.Revision} · 冻结时保存的正文`} onClose={onClose} width="large">
      <div className={css.dialog}>
        {view.kind === 'loading' && <p className={css.muted}>正在读取冻结的正文…</p>}
        {view.kind === 'purged' && (
          <Callout tone="warning" title="正文已清理">
            这篇文档的正文已按保留策略清理，只保留了标题、修订号和指纹。页面不会为它恢复旧正文。
          </Callout>
        )}
        {view.kind === 'error' && (
          <Callout tone="critical" title="没有读到正文">
            {view.message}
            {view.relogin && (
              <>
                {' '}
                <Link href={loginURL(here())}>重新登录</Link>
              </>
            )}
          </Callout>
        )}
        {view.kind === 'ready' && (
          <>
            <p className={css.muted}>
              指纹 <code title={view.doc.sha256}>{short(view.doc.sha256)}</code> · 按纯文本显示，文档中的 Markdown
              由你自行判断，页面不执行其中的任何标记。
            </p>
            <pre className={css.content}>{view.doc.content}</pre>
          </>
        )}
        <div className={css.dialogActions}>
          {view.kind === 'error' && !view.relogin && (
            <Button
              onClick={() => {
                setView({ kind: 'loading' })
                setAttempt((n) => n + 1)
              }}
            >
              重试
            </Button>
          )}
          <Button variant="primary" onClick={onClose}>
            关闭
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
  return (
    <>
      <table className={table.table} aria-label="冻结的设计文档">
        <thead>
          <tr>
            <th scope="col">文档</th>
            <th scope="col">修订</th>
            <th scope="col">指纹</th>
            <th scope="col">正文</th>
          </tr>
        </thead>
        <tbody>
          {docs.map((d) => {
            const purgedDoc = d.Purged || cleaned.has(contentKey(base, d))
            return (
              <tr key={d.DocID}>
                <td data-label="文档">
                  <FileIcon className={css.fileIcon} aria-hidden /> {d.Title || d.DocID}
                  {d.URL && (
                    <a className={css.ext} href={d.URL} target="_blank" rel="noreferrer" title="在 Outline 中打开这篇文档">
                      <LinkExternalIcon aria-label="在 Outline 中打开" />
                    </a>
                  )}
                  <div className={css.docID}>
                    <code>{d.DocID}</code>
                  </div>
                </td>
                <td data-label="修订" className={table.nowrap}>
                  r{d.Revision}
                </td>
                <td data-label="指纹" className={table.mono}>
                  <code title={d.SHA256}>{short(d.SHA256)}</code>
                </td>
                <td data-label="正文" className={table.nowrap}>
                  {purgedDoc ? (
                    <span className={css.muted}>已按保留策略清理</span>
                  ) : (
                    <Button size="small" onClick={() => setOpen({ doc: d, initial: cache.current.get(contentKey(base, d)) ?? null })}>
                      查看正文
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
  error?: string
  integrityHint?: string
}) {
  const show = basis !== null && basis.Frozen && basis.Source !== 'none'
  const collection = basis && basis.CollectionName ? basis.CollectionName : basis?.CollectionID
  return (
    <Box as="section" aria-labelledby="design-basis">
      <BoxHeader id="design-basis" title="设计依据">
        {basis && <Label variant={show ? 'accent' : 'secondary'}>{show ? sourceText(basis.Source) : '没有依据'}</Label>}
      </BoxHeader>
      <BoxRow className={css.block}>
        {error && (
          <Callout tone="warning" title="设计依据的记录与实际快照不一致">
            {error} 页面不会把它当作正常依据，也不会用当前绑定补造。
          </Callout>
        )}
        {declaredSource !== undefined && (
          <p className={css.p}>
            请求记录的来源：{declared ? sourceText(declaredSource) : '迁移前，没有记录'}
            {workItemURL && (
              <>
                {' '}
                · <Link href={workItemURL}>关联的工作项</Link>
              </>
            )}
            {snapshotSetID !== undefined && snapshotSetID > 0 && (
              <>
                {' '}
                · 快照集 <code>#{snapshotSetID}</code>
              </>
            )}
          </p>
        )}
        {basisSource !== undefined && basisSource !== declaredSource && (
          <p className={css.p}>
            实际可读的依据：{sourceText(basisSource)}
            <span className={css.muted}>（请求记录与实际解析不同，下面展示实际保存的快照）</span>
          </p>
        )}
        {!show ? (
          <p className={css.muted}>{emptyText}</p>
        ) : (
          <>
            <dl className={css.kv}>
              <dt>冻结时间</dt>
              <dd>{basis.FrozenAt ? <Ago t={basis.FrozenAt} /> : '未知'}</dd>
              <dt>来源</dt>
              <dd>
                {basis.SourceKnown ? (
                  <>
                    {basis.Provider || '未知提供方'}
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
                    {basis.BindingGeneration > 0 && <span className={css.muted}> · 绑定第 {basis.BindingGeneration} 代</span>}
                  </>
                ) : (
                  <span className={css.muted}>历史快照没有记录来源；下面是当时保存的文档，不按当前仓库绑定解释</span>
                )}
              </dd>
              <dt>文档数</dt>
              <dd>
                {basis.DocCount}
                {basis.DocCount === 0 && <span className={css.muted}>（冻结的是一个空集合，不是读取失败）</span>}
              </dd>
              <dt>摘要</dt>
              <dd>
                <code className={css.digest} title={basis.ManifestDigest}>
                  {basis.ManifestDigest || '未记录'}
                </code>
              </dd>
            </dl>
            {integrityHint && <p className={css.muted}>{integrityHint}</p>}
            {basis.IntegrityError && (
              <Callout tone="warning" title="快照的元数据不一致">
                {basis.IntegrityError} 这可能来自清理或人工改库，请以数据库记录为准排查。
              </Callout>
            )}
            {basis.ContentPurgedAt && (basis.DocCount > 0 || basis.Docs.length > 0) && (
              <p className={css.muted}>
                正文已于 <Ago t={basis.ContentPurgedAt} /> 按保留策略清理；标题、修订号和指纹仍然保留。
              </p>
            )}
            {basis.Docs.length > 0 ? (
              <DocsTable docs={basis.Docs} base={contentBase} />
            ) : (
              <p className={css.muted}>这个快照是空集合，没有文档。</p>
            )}
          </>
        )}
      </BoxRow>
    </Box>
  )
}

/** RequestDesignBasis 是 AI 请求页面的设计依据。 */
export function RequestDesignBasis({ view }: { view: RequestDocsViewData }) {
  return (
    <DesignBasis
      basis={view.Basis}
      contentBase={`/ai/requests/${view.RequestID}/docs/content`}
      declaredSource={view.DeclaredSource}
      declared={view.Declared}
      workItemURL={view.WorkItemURL}
      snapshotSetID={view.SnapshotSetID}
      error={view.Error}
      emptyText={requestEmptyText(view)}
    />
  )
}

/** TaskDesignBasis 是 AI 任务页面的设计依据。 */
export function TaskDesignBasis({ view }: { view: TaskDocsViewData }) {
  return (
    <DesignBasis
      basis={view.Basis}
      contentBase={`/ai/tasks/${view.TaskID}/docs/content`}
      declaredSource={view.DeclaredSource}
      basisSource={view.BasisSource}
      error={view.Error}
      emptyText="这个任务没有可读的设计文档依据：它没有在 spec 中固定文档，所属请求也没有冻结快照。"
    />
  )
}

/** requestEmptyText 按请求记录的来源解释"没有依据"，不把正常状态当成系统错误。 */
function requestEmptyText(view: RequestDocsViewData): string {
  switch (view.DeclaredSource) {
    case 'snapshot_set':
      return '请求记录的独立快照集不存在或读不到（见上）。'
    case 'work_item':
      return '请求引用的工作项没有冻结设计文档。'
    case 'legacy':
      return '迁移前的请求没有记录设计依据；这里不按当前仓库绑定补造。'
    case 'unplanned':
      return '请求时没有计划设计文档依据（不表示系统错误）。'
    default:
      return '这个请求没有设计文档依据（不表示系统错误）。'
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
  Error: string
}
type TaskDocsViewData = {
  TaskID: number
  DeclaredSource: string
  BasisSource: string
  Basis: DesignBasisData | null
  Error: string
}

const relationLabels: Record<string, { text: string; variant: 'success' | 'accent' | 'attention' | 'danger' | 'secondary' }> = {
  same_basis: { text: '同一份依据', variant: 'success' },
  refs_match: { text: '文档引用一致', variant: 'success' },
  claimed_dynamic: { text: '领取后关联', variant: 'accent' },
  legacy_pinned: { text: '迁移前核对一致', variant: 'secondary' },
  no_basis: { text: '没有依据', variant: 'secondary' },
  mismatch: { text: '依据不一致', variant: 'danger' },
  unverifiable: { text: '无法核对', variant: 'attention' },
}

const taskTypes: Record<string, string> = { develop: '开发', review: '审计' }
const taskModes: Record<string, string> = { select: '选题', implement: '实现', fix: '返修' }

/** RelatedTasksTable 是工作项关联的开发 / 审计任务，以及它们与工作项冻结依据的关系。 */
export function RelatedTasksTable({ tasks }: { tasks: RelatedTask[] }) {
  if (tasks.length === 0) {
    return (
      <Box as="section" aria-labelledby="related-tasks">
        <BoxHeader id="related-tasks" title="关联任务与依据" />
        <BoxRow>
          <p className={css.muted}>还没有关联任务。</p>
        </BoxRow>
      </Box>
    )
  }
  return (
    <Box as="section" aria-labelledby="related-tasks">
      <BoxHeader id="related-tasks" title="关联任务与依据" />
      <table className={table.table} aria-label="关联任务与依据">
        <thead>
          <tr>
            <th scope="col">任务</th>
            <th scope="col">类型</th>
            <th scope="col">方式</th>
            <th scope="col">与冻结依据的关系</th>
          </tr>
        </thead>
        <tbody>
          {tasks.map((t) => {
            const rel = lookup(relationLabels, t.Relation) ?? { text: t.Relation, variant: 'secondary' as const }
            return (
              <tr key={t.TaskID}>
                <td data-label="任务">
                  <Link href={t.TaskURL}>#{t.TaskID}</Link>
                  <div className={css.docID}>
                    <Link href={t.RequestURL}>请求 #{t.RequestID}</Link>
                  </div>
                </td>
                <td data-label="类型">{lookup(taskTypes, t.Type) ?? t.Type}</td>
                <td data-label="方式">{lookup(taskModes, t.Mode) ?? (t.Mode || '—')}</td>
                <td data-label="与冻结依据的关系">
                  <Label variant={rel.variant}>{rel.text}</Label>
                  {t.Note && <div className={css.muted}>{t.Note}</div>}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </Box>
  )
}

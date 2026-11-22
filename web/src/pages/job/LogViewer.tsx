import { ArrowDownIcon, ChevronDownIcon, ChevronUpIcon, DownloadIcon, SearchIcon } from '@primer/octicons-react'
import { Button, Checkbox, FormControl, IconButton, Link, TextInput } from '@primer/react'
import { useEffect, useRef, useState, type KeyboardEvent } from 'react'
import { useRouter } from '../../app/router'
import { loginURL } from '../../lib/api'
import { LogBuffer } from './buffer'
import { findMatches, logDownloadURL, parsePosition, searchLimit, stepIndex, storageKey, type Match } from './log'
import { LogPoller, type LogSink, type LogStatus } from './poller'
import css from './LogViewer.module.css'

// 日志面板：深色（浅色和深色模式下都是），头部是状态、搜索、跟随开关和下载；读取逻辑在 poller.ts，DOM 在 buffer.ts。

const hitName = 'kiln-log-hit'
const curName = 'kiln-log-cur'

/** highlights 返回 CSS Custom Highlight 的注册表；浏览器不支持时返回 null（改为选中当前结果）。 */
function highlights(): HighlightRegistry | null {
  return typeof Highlight === 'function' && typeof CSS !== 'undefined' && 'highlights' in CSS ? CSS.highlights : null
}

/** readPosition 读取保存的阅读位置；存储不可用时返回 null。 */
function readPosition(key: string) {
  try {
    return parsePosition(sessionStorage.getItem(key))
  } catch {
    return null
  }
}

/** statusText 返回日志状态的说明（不含"立即重试"等操作）。 */
export function statusText(s: LogStatus, counts: { shown: number; lines: number }, emptyText: string): string {
  switch (s.kind) {
    case 'loading':
      return counts.shown > 0 ? `正在加载…（已显示 ${counts.shown.toLocaleString('zh-CN')} 个字符）` : '正在加载日志…'
    case 'live':
      return counts.shown > 0 ? '实时更新中…' : '等待日志输出…'
    case 'next-step':
      return '这次尝试已结束，等待 job 的下一步…'
    case 'complete':
      return `日志已完整加载（${counts.lines.toLocaleString('zh-CN')} 行）。`
    case 'empty':
      return emptyText
    case 'retry':
      return `${s.why}，${Math.round(s.wait / 1000)} 秒后自动重试（第 ${s.failures} 次）；已显示的日志保留。`
    case 'expired':
      return s.why === 'unauthorized' ? '登录已过期，日志已停止更新。' : '收到的不是日志内容（可能登录已过期），已停止更新。'
    case 'not-found':
      return `日志不存在${s.message ? '：' + s.message : ''}。`
    case 'failed':
      return `读取日志失败（HTTP ${s.status}），已停止。`
    default: {
      const _exhaustive: never = s
      return _exhaustive
    }
  }
}

const tone = (s: LogStatus): 'run' | 'ok' | 'warn' | 'bad' | 'muted' => {
  switch (s.kind) {
    case 'loading':
    case 'live':
    case 'next-step':
      return 'run'
    case 'complete':
      return 'ok'
    case 'retry':
      return 'warn'
    case 'expired':
    case 'not-found':
    case 'failed':
      return 'bad'
    case 'empty':
      return 'muted'
    default: {
      const _exhaustive: never = s
      return _exhaustive
    }
  }
}

export interface LogViewerProps {
  /** job 或 AI 任务的页面地址（/jobs/{id}、/ai/tasks/{id}）：日志接口、下载和尝试的链接都在它下面。 */
  base: string
  /** 读取的 attempt（0 表示 job 当前的尝试，读到后确定）。 */
  attempt: number
  /** 地址中指定了 attempt，日志固定在这次尝试。 */
  explicit: boolean
  /** 阅读位置在 sessionStorage 中的键的一部分（jobPage.AttemptKey）。 */
  attemptKey: string
  /** 页面上 job 的状态（日志接口返回的状态与它不同时刷新页面数据）。 */
  state: string
  /** 没有日志时的说明（jobPage.LogEmpty，随页面数据更新）。 */
  emptyText: string
  /** 标题旁的说明，例如"第 1 次尝试"或"发布"。 */
  note?: string
}

export function LogViewer({ base, attempt: initialAttempt, explicit, attemptKey, state, emptyText, note }: LogViewerProps) {
  const { refresh } = useRouter()
  const scroller = useRef<HTMLDivElement>(null)
  const linesRoot = useRef<HTMLDivElement>(null)
  const buffer = useRef<LogBuffer | null>(null)
  const poller = useRef<LogPoller | null>(null)
  const followRef = useRef(true)
  const refreshRef = useRef(refresh)
  // 开始读取时的 attempt 和状态：之后由日志接口的响应头决定（页面数据刷新后 Attempt 变了也不重新开始）
  const initial = useRef({ attempt: initialAttempt, explicit, state })
  const [follow, setFollowState] = useState(true)
  const [hasNew, setHasNew] = useState(false)
  const [status, setStatus] = useState<LogStatus>({ kind: 'loading' })
  const [counts, setCounts] = useState({ shown: 0, lines: 0 })
  const [attempt, setAttempt] = useState(initialAttempt)
  const [switchedFrom, setSwitchedFrom] = useState(0)
  const [truncated, setTruncated] = useState(false)

  // 搜索
  const [term, setTerm] = useState('')
  const [hits, setHits] = useState('')
  const search = useRef<{ term: string; matches: Match[]; idx: number; dirty: boolean }>({ term: '', matches: [], idx: -1, dirty: false })

  useEffect(() => {
    refreshRef.current = refresh
  }, [refresh])

  const setFollow = (on: boolean) => {
    followRef.current = on
    setFollowState(on)
    const sc = scroller.current
    if (on && sc) sc.scrollTop = sc.scrollHeight
    if (on) setHasNew(false)
  }

  // 清除搜索结果（setHits 和 search 都不随渲染变化，所以可以只创建一次）
  const clearSearchRef = useRef(() => {
    search.current = { term: '', matches: [], idx: -1, dirty: false }
    highlights()?.delete(hitName)
    highlights()?.delete(curName)
    setHits('')
  })

  useEffect(() => {
    const sc = scroller.current
    const root = linesRoot.current
    if (!sc || !root) return
    const buf = new LogBuffer(root)
    buffer.current = buf
    const key = storageKey(base, attemptKey)
    const sink: LogSink = {
      append(text) {
        if (!buf.append(text)) return
        search.current.dirty = true
        // 追加在底部，不影响正在阅读的位置；丢弃最早的行后内容整体上移，按丢弃部分的高度往回滚
        const appended = sc.scrollHeight
        if (buf.trim() > 0) {
          // 已找到的位置不再对应
          setTruncated(true)
          clearSearchRef.current()
          if (!followRef.current) sc.scrollTop -= appended - sc.scrollHeight
        }
        if (followRef.current) sc.scrollTop = sc.scrollHeight
        else setHasNew(true)
      },
      reset() {
        buf.clear()
        setTruncated(false)
        clearSearchRef.current()
      },
      shown: () => buf.shown,
      status(s) {
        setStatus(s)
        setCounts({ shown: buf.shown, lines: buf.lineCount })
      },
      attempt(next, prev) {
        setAttempt(next)
        if (prev) setSwitchedFrom(prev)
      },
      jobState() {
        void refreshRef.current()
      },
      caughtUp() {
        const saved = readPosition(key)
        if (saved && !saved.follow) {
          followRef.current = false
          setFollowState(false)
          sc.scrollTop = saved.top
        }
      },
    }
    const p = new LogPoller({ base, ...initial.current, sink })
    poller.current = p
    p.start()
    const save = () => {
      try {
        sessionStorage.setItem(key, JSON.stringify({ follow: followRef.current, top: sc.scrollTop }))
      } catch {
        // 存储不可用时不保存
      }
    }
    window.addEventListener('pagehide', save)
    return () => {
      p.stop()
      save()
      window.removeEventListener('pagehide', save)
      highlights()?.delete(hitName)
      highlights()?.delete(curName)
    }
  }, [base, attemptKey])

  const onScroll = () => {
    const sc = scroller.current
    if (!sc) return
    const bottom = sc.scrollTop + sc.clientHeight >= sc.scrollHeight - 20
    if (bottom !== followRef.current) {
      followRef.current = bottom
      setFollowState(bottom)
    }
    if (bottom) setHasNew(false)
  }

  const go = (dir: 1 | -1) => {
    const buf = buffer.current
    const sc = scroller.current
    if (!buf || !sc) return
    if (term === '') {
      clearSearchRef.current()
      return
    }
    const s = search.current
    const reg = highlights()
    if (term !== s.term || s.dirty) {
      const keep = s.idx
      const matches = findMatches(buf.lines, term)
      const idx = term === s.term ? Math.min(keep, matches.length - 1) : -1
      search.current = { term, matches, idx, dirty: false }
      reg?.delete(curName)
      if (reg) {
        const ranges = matches.map((m) => buf.range(m)).filter((r): r is Range => r !== null)
        if (ranges.length > 0) reg.set(hitName, new Highlight(...ranges))
        else reg.delete(hitName)
      }
    }
    const cur = search.current
    if (cur.matches.length === 0) {
      setHits('没有找到')
      return
    }
    cur.idx = stepIndex(cur.idx, dir, cur.matches.length)
    const m = cur.matches[cur.idx]
    const r = m ? buf.range(m) : null
    if (!r) return
    if (reg) reg.set(curName, new Highlight(r))
    else {
      const sel = window.getSelection()
      sel?.removeAllRanges()
      sel?.addRange(r)
    }
    followRef.current = false
    setFollowState(false)
    // 滚到这一行（行元素的 offsetParent 是日志区）
    const line = r.startContainer.parentElement
    if (line) sc.scrollTop = line.offsetTop - sc.clientHeight / 3
    setHits(`第 ${cur.idx + 1} / ${cur.matches.length} 处${cur.matches.length >= searchLimit ? `（最多 ${searchLimit} 处）` : ''}`)
  }

  const onSearchKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    go(e.shiftKey ? -1 : 1)
  }

  const download = logDownloadURL(base, attempt)
  const empty = status.kind === 'empty'
  // 没有执行过的 job（命中缓存、跳过、取消等）：不显示搜索、跟随和下载
  const toolsHidden = empty && attempt === 0
  const t = tone(status)
  // 只有日志还会增加时才需要"跟随最新输出"
  const streaming = t === 'run' || t === 'warn'
  const here = typeof window === 'undefined' ? '' : window.location.pathname + window.location.search

  return (
    <section className={css.panel} aria-labelledby="log-h" data-color-mode="dark" data-dark-theme="dark" data-light-theme="dark">
      <div className={css.head}>
        <h2 className={css.title} id="log-h">
          日志{note && <span className={css.note}>{note}</span>}
        </h2>
        {!toolsHidden && (
          <div className={css.tools}>
            <div className={css.search} role="search" aria-label="在日志中搜索">
              <TextInput
                size="small"
                type="search"
                leadingVisual={SearchIcon}
                placeholder="搜索已加载的日志"
                aria-label="在日志中搜索"
                aria-describedby="log-scope"
                spellCheck={false}
                autoCapitalize="off"
                value={term}
                onChange={(e) => {
                  setTerm(e.target.value)
                  if (e.target.value === '') clearSearchRef.current()
                }}
                onKeyDown={onSearchKey}
                className={css.searchInput}
              />
              <IconButton icon={ChevronUpIcon} aria-label="上一个" size="small" variant="invisible" onClick={() => go(-1)} />
              <IconButton icon={ChevronDownIcon} aria-label="下一个" size="small" variant="invisible" onClick={() => go(1)} />
              <span className={css.hits} aria-live="polite">
                {hits}
              </span>
            </div>
            {streaming && (
              <FormControl layout="horizontal" className={css.follow}>
                <Checkbox checked={follow} onChange={(e) => setFollow(e.target.checked)} />
                <FormControl.Label>跟随最新输出</FormControl.Label>
              </FormControl>
            )}
            <IconButton as="a" href={download} download icon={DownloadIcon} aria-label="下载完整日志" size="small" variant="invisible" />
          </div>
        )}
      </div>
      <p className="visually-hidden" id="log-scope">
        只搜索页面上已加载的日志，不区分大小写（回车下一个，Shift+回车上一个）；页面最多保留最后约 200 万个字符，完整内容请下载。
      </p>
      <div className={`${css.status ?? ''} ${css[t] ?? ''}`} role="status" aria-live="polite">
        <span>{statusText(status, counts, emptyText)}</span>
        {(status.kind === 'retry' || status.kind === 'failed') && (
          <Button size="small" onClick={() => poller.current?.retryNow()}>
            {status.kind === 'retry' ? '立即重试' : '重试'}
          </Button>
        )}
        {status.kind === 'expired' && <Link href={loginURL(here)}>重新登录</Link>}
        {status.kind === 'not-found' && <Link href={base}>查看当前尝试</Link>}
      </div>
      {switchedFrom > 0 && (
        <p className={css.notice}>
          尝试 #{switchedFrom} 已结束，下面是新的尝试 #{attempt} 的日志。<Link href={`${base}?attempt=${switchedFrom}`}>查看尝试 #{switchedFrom} 的日志</Link>
        </p>
      )}
      {truncated && (
        <p className={css.notice}>
          日志太长，页面只保留最后约 200 万个字符，更早的部分已丢弃。<a href={download} download>下载完整日志</a>
        </p>
      )}
      <div className={css.bodyWrap}>
        <div ref={scroller} className={css.body} tabIndex={0} aria-labelledby="log-h" onScroll={onScroll} hidden={empty}>
          <div ref={linesRoot} className={css.lines} />
        </div>
        {empty && <p className={css.empty}>{emptyText}</p>}
        {!follow && !empty && (
          <Button size="small" className={css.bottom} leadingVisual={ArrowDownIcon} onClick={() => setFollow(true)}>
            {hasNew ? '有新输出 · 回到底部' : '回到底部'}
          </Button>
        )}
      </div>
    </section>
  )
}

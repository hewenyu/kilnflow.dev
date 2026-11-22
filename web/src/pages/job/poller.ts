import { msg, parseText, type Text } from '../../i18n/messages'
import { backoff, maxBackoffMs, nextStepMs, parseLogMeta, pollMs } from './log'

// 日志的分段读取：GET {base}/log?attempt=&offset= 一段一段读（base 是 /jobs/{id} 或 /ai/tasks/{id}），读完已有内容后每 1.5 秒取新输出。
// 区分加载中、实时更新、已完成、没有日志、暂时断线（退避重试，可立即重试）、未登录（401）、不存在（404）
// 和收到的不是日志（例如被跳转到登录页），不可恢复的错误停止轮询。同一时间只有一个请求。
// 状态中的说明是消息（Text），显示时按当前语言翻译：切换语言后已经显示的状态立即更新。错误响应的正文是服务端的消息（JSON），
// 不是 JSON 时（例如旧版服务端的纯文本）原样显示。

/** LogStatus 是日志区的状态。 */
export type LogStatus =
  | { kind: 'loading' }
  | { kind: 'live' }
  | { kind: 'next-step' }
  | { kind: 'complete' }
  | { kind: 'empty' }
  | { kind: 'retry'; why: Text; wait: number; failures: number }
  | { kind: 'expired'; why: 'unauthorized' | 'not-log' }
  | { kind: 'not-found'; message: Text }
  | { kind: 'failed'; status: number }

/** LogSink 接收读到的日志和状态变化。 */
export interface LogSink {
  /** 追加一段文字（已经解码，还带 ANSI 转义）。 */
  append: (text: string) => void
  /** 清空已显示的日志（切换到新的尝试）。 */
  reset: () => void
  /** 页面上已显示的字符数。 */
  shown: () => number
  status: (s: LogStatus) => void
  /** 读取的 attempt 变了：第一次得到当前 attempt，或切换到新的尝试（prev 是上一次，首次为 0）。 */
  attempt: (next: number, prev: number) => void
  /** job 的状态变了（页面据此刷新其他部分）。 */
  jobState: (state: string) => void
  /** 第一次读完已有内容（恢复阅读位置的时机）。 */
  caughtUp: () => void
}

export interface PollerOptions {
  /** job 或 AI 任务的页面地址（/jobs/{id}、/ai/tasks/{id}），日志接口在它下面。 */
  base: string
  /** 要读的 attempt；0 表示当前尝试。 */
  attempt: number
  /** 地址中指定了 attempt：固定在这次尝试，不随新的尝试切换。 */
  explicit: boolean
  /** 页面上 job 的状态。 */
  state: string
  sink: LogSink
  fetchFn?: typeof fetch
}

export class LogPoller {
  private attempt: number
  private offset = 0
  private decoder = new TextDecoder()
  private failures = 0
  private timer: ReturnType<typeof setTimeout> | undefined
  private stopped = false
  private inflight = false
  private caughtUp = false
  private lastState: string
  private readonly fetchFn: typeof fetch
  /** 进行中的请求：stop 时中止它，免得关掉页面后请求还在继续。 */
  private ctl: AbortController | undefined

  constructor(private readonly opts: PollerOptions) {
    this.attempt = opts.attempt
    this.lastState = opts.state
    this.fetchFn = opts.fetchFn ?? ((...args) => fetch(...args))
  }

  start(): void {
    this.stopped = false
    void this.poll()
  }

  /** stop 停止轮询（组件卸载时），并中止进行中的请求。 */
  stop(): void {
    this.stopped = true
    clearTimeout(this.timer)
    this.ctl?.abort()
  }

  /** retryNow 立即重试（断线等待中或因错误停止后）；已有请求在进行时什么也不做。 */
  retryNow(): void {
    if (this.inflight) return
    clearTimeout(this.timer)
    this.stopped = false
    void this.poll()
  }

  /** isStopped 在 await 之后检查是否已停止（期间可能调用了 stop）。 */
  private isStopped(): boolean {
    return this.stopped
  }

  private later(ms: number): void {
    clearTimeout(this.timer)
    this.timer = setTimeout(() => void this.poll(), ms)
  }

  private halt(s: LogStatus): void {
    this.stopped = true
    this.opts.sink.status(s)
  }

  private retryLater(why: Text): void {
    this.failures++
    const wait = backoff(pollMs, this.failures - 1, maxBackoffMs)
    this.opts.sink.status({ kind: 'retry', why, wait, failures: this.failures })
    this.later(wait)
  }

  private switchTo(next: number): void {
    const prev = this.attempt
    this.attempt = next
    this.offset = 0
    this.decoder = new TextDecoder()
    this.opts.sink.reset()
    this.opts.sink.attempt(next, prev)
    void this.poll()
  }

  private async poll(): Promise<void> {
    this.timer = undefined
    if (this.stopped || this.inflight) return
    const { base, explicit, sink } = this.opts
    let res: Response
    let buf: ArrayBuffer | null = null
    let notFound: Text = ''
    const ctl = new AbortController()
    this.ctl = ctl
    this.inflight = true
    try {
      res = await this.fetchFn(`${base}/log?attempt=${this.attempt}&offset=${this.offset}`, {
        cache: 'no-store',
        credentials: 'same-origin',
        signal: ctl.signal,
      })
      // 响应体也在请求进行中读完：期间调用的 stop、retryNow 都能看到
      if (res.ok) buf = await res.arrayBuffer()
      else if (res.status === 404) notFound = await errorText(res)
    } catch {
      this.inflight = false
      // stop 中止的请求不是断线，不进入退避重试
      if (ctl.signal.aborted) return
      if (!this.isStopped()) this.retryLater(msg('logView.disconnected'))
      return
    }
    this.inflight = false
    if (this.isStopped()) return
    if (res.status === 401) {
      this.halt({ kind: 'expired', why: 'unauthorized' })
      return
    }
    if (res.status === 404) {
      this.halt({ kind: 'not-found', message: notFound })
      return
    }
    if (res.status >= 400 && res.status < 500) {
      this.halt({ kind: 'failed', status: res.status })
      return
    }
    if (!res.ok || buf === null) {
      this.retryLater(msg('logView.serverError', { status: res.status }))
      return
    }
    const meta = res.redirected ? null : parseLogMeta(res.headers)
    if (!meta) {
      // 例如会话过期后被跳转到登录页：不能把这样的内容当作日志显示
      this.halt({ kind: 'expired', why: 'not-log' })
      return
    }
    this.failures = 0
    if (!this.attempt && meta.attempt) {
      this.attempt = meta.attempt
      sink.attempt(meta.attempt, 0)
    }
    if (meta.next >= this.offset) this.offset = meta.next
    const text = this.decoder.decode(buf, { stream: !meta.finished })
    if (text) sink.append(text)
    if (meta.state && meta.state !== this.lastState) {
      this.lastState = meta.state
      sink.jobState(meta.state)
    }
    if (buf.byteLength > 0) {
      sink.status({ kind: 'loading' })
      void this.poll()
      return
    }
    if (!this.caughtUp) {
      this.caughtUp = true
      sink.caughtUp()
    }
    if (!meta.finished) {
      sink.status({ kind: 'live' })
      this.later(pollMs)
      return
    }
    const tail = this.decoder.decode()
    if (tail) sink.append(tail)
    if (!explicit && meta.current && meta.current !== this.attempt) {
      this.switchTo(meta.current)
      return
    }
    if (!explicit && !meta.terminal) {
      sink.status({ kind: 'next-step' })
      this.later(nextStepMs)
      return
    }
    this.stopped = true
    sink.status(sink.shown() === 0 ? { kind: 'empty' } : { kind: 'complete' })
  }
}

/** errorText 读取错误响应的说明：JSON 是服务端的消息（见 internal/server/web 的 apiError），其他按纯文本原样显示；读不到时为空。 */
async function errorText(res: Response): Promise<Text> {
  const body = (await res.text().catch(() => '')).trim()
  if (!(res.headers.get('Content-Type') ?? '').startsWith('application/json')) return body
  try {
    return parseText(JSON.parse(body))
  } catch {
    return body
  }
}

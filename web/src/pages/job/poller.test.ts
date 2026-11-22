import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { deferred } from '../../test/render'
import { LogPoller, type LogSink, type LogStatus } from './poller'

// 日志轮询：用假的 fetch 和假的计时器。

interface Meta {
  next: number
  finished?: boolean
  state?: string
  terminal?: boolean
  attempt?: number
  current?: number
}

const logRes = (body: string, m: Meta) =>
  new Response(body, {
    status: 200,
    headers: {
      'Content-Type': 'text/plain; charset=utf-8',
      'X-Next-Offset': String(m.next),
      'X-Finished': String(m.finished ?? false),
      'X-Job-State': m.state ?? 'running',
      'X-Job-Terminal': String(m.terminal ?? false),
      'X-Attempt': String(m.attempt ?? 31),
      'X-Current-Attempt': String(m.current ?? 31),
    },
  })

function setup(responses: (Response | Error)[], opts: { base?: string; attempt?: number; explicit?: boolean; state?: string; fetchFn?: typeof fetch } = {}) {
  const urls: string[] = []
  const record = (input: RequestInfo | URL) => {
    urls.push(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
  }
  const fetchFn = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    record(input)
    if (opts.fetchFn) return opts.fetchFn(input, init)
    const r = responses.shift()
    if (!r) return new Promise<Response>(() => undefined) // 之后的请求不返回
    return r instanceof Error ? Promise.reject(r) : Promise.resolve(r)
  })
  let text = ''
  const statuses: LogStatus[] = []
  const attempts: [number, number][] = []
  const states: string[] = []
  let caught = 0
  let resets = 0
  const sink: LogSink = {
    append: (t) => {
      text += t
    },
    reset: () => {
      resets++
      text = ''
    },
    shown: () => text.length,
    status: (s) => statuses.push(s),
    attempt: (next, prev) => attempts.push([next, prev]),
    jobState: (s) => states.push(s),
    caughtUp: () => {
      caught++
    },
  }
  const p = new LogPoller({ base: opts.base ?? '/jobs/412', attempt: opts.attempt ?? 0, explicit: opts.explicit ?? false, state: opts.state ?? 'running', sink, fetchFn })
  return { p, urls, fetchFn, statuses, attempts, states, text: () => text, caught: () => caught, resets: () => resets }
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

const last = (s: LogStatus[]) => s[s.length - 1]

describe('LogPoller', () => {
  it('AI 任务的日志从 /ai/tasks/{id}/log 读取', async () => {
    const t = setup([logRes('x\n', { next: 2 })], { base: '/ai/tasks/9' })
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.urls[0]).toBe('/ai/tasks/9/log?attempt=0&offset=0')
  })

  it('一段一段读完已有内容，再每 1.5 秒取新输出', async () => {
    const t = setup([logRes('line 1\n', { next: 7 }), logRes('line 2\n', { next: 14 }), logRes('', { next: 14 }), logRes('line 3\n', { next: 21 })])
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.urls).toEqual(['/jobs/412/log?attempt=0&offset=0', '/jobs/412/log?attempt=31&offset=7', '/jobs/412/log?attempt=31&offset=14'])
    expect(t.attempts).toEqual([[31, 0]])
    expect(t.text()).toBe('line 1\nline 2\n')
    expect(t.caught()).toBe(1)
    expect(last(t.statuses)).toEqual({ kind: 'live' })
    await vi.advanceTimersByTimeAsync(1499)
    expect(t.urls).toHaveLength(3)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.urls[3]).toBe('/jobs/412/log?attempt=31&offset=14')
    expect(t.text()).toBe('line 1\nline 2\nline 3\n')
  })

  it('job 结束、日志读完：有内容时显示已完成，没有内容时显示没有日志', async () => {
    const done = setup([logRes('x\n', { next: 2 }), logRes('', { next: 2, finished: true, terminal: true, state: 'succeeded' })])
    done.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(last(done.statuses)).toEqual({ kind: 'complete' })
    expect(done.states).toEqual(['succeeded'])

    const empty = setup([logRes('', { next: 0, finished: true, terminal: true, state: 'running' })])
    empty.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(last(empty.statuses)).toEqual({ kind: 'empty' })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(empty.urls).toHaveLength(1)
  })

  it('尝试结束、job 开始新的尝试：清空并切换到新的尝试', async () => {
    const t = setup([logRes('old\n', { next: 4 }), logRes('', { next: 4, finished: true, current: 32 }), logRes('new\n', { next: 4, attempt: 32, current: 32 })], {
      attempt: 31,
    })
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.resets()).toBe(1)
    expect(t.attempts).toEqual([[32, 31]])
    expect(t.urls[2]).toBe('/jobs/412/log?attempt=32&offset=0')
    expect(t.text()).toBe('new\n')
  })

  it('指定了 attempt 时不切换；尝试结束但 job 没结束时等 3 秒看下一步', async () => {
    const pinned = setup([logRes('', { next: 0, finished: true, current: 32 })], { attempt: 31, explicit: true })
    pinned.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(pinned.attempts).toEqual([])
    expect(last(pinned.statuses)).toEqual({ kind: 'empty' })

    const next = setup([logRes('a\n', { next: 2 }), logRes('', { next: 2, finished: true })], { attempt: 31 })
    next.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(last(next.statuses)).toEqual({ kind: 'next-step' })
    await vi.advanceTimersByTimeAsync(2999)
    expect(next.urls).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1)
    expect(next.urls).toHaveLength(3)
  })

  it('断线和服务器错误时退避重试（1.5、3 秒……），成功后恢复', async () => {
    const t = setup([new TypeError('offline'), new Response('oops', { status: 502 }), logRes('ok\n', { next: 3 })])
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    // 状态中存消息（显示时按当前语言翻译），不存翻译好的文字
    expect(last(t.statuses)).toEqual({ kind: 'retry', why: { id: 'logView.disconnected' }, wait: 1500, failures: 1 })
    await vi.advanceTimersByTimeAsync(1500)
    expect(last(t.statuses)).toEqual({ kind: 'retry', why: { id: 'logView.serverError', params: { status: 502 } }, wait: 3000, failures: 2 })
    await vi.advanceTimersByTimeAsync(3000)
    expect(t.text()).toBe('ok\n')
  })

  it('立即重试：不等退避；已有请求在进行时不重复请求', async () => {
    const t = setup([new TypeError('offline')])
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    t.p.retryNow()
    t.p.retryNow()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.fetchFn).toHaveBeenCalledTimes(2)
  })

  it('未登录、不存在、其他 4xx、收到的不是日志：停止轮询', async () => {
    const cases: [Response, LogStatus][] = [
      [new Response('', { status: 401 }), { kind: 'expired', why: 'unauthorized' }],
      [new Response('没有这次尝试\n', { status: 404 }), { kind: 'not-found', message: '没有这次尝试' }],
      // 服务端的错误是消息的 JSON（apiError）：按消息保存
      [
        new Response('{"id":"log.errJobAttemptNotFound","params":{"id":7,"attempt":9}}\n', {
          status: 404,
          headers: { 'Content-Type': 'application/json; charset=utf-8' },
        }),
        { kind: 'not-found', message: { id: 'log.errJobAttemptNotFound', params: { id: 7, attempt: 9 } } },
      ],
      // 声明是 JSON 却解析不了时原样显示
      [new Response('{oops', { status: 404, headers: { 'Content-Type': 'application/json' } }), { kind: 'not-found', message: '{oops' }],
      [new Response('', { status: 400 }), { kind: 'failed', status: 400 }],
      [new Response('<html>', { status: 200, headers: { 'Content-Type': 'text/html' } }), { kind: 'expired', why: 'not-log' }],
    ]
    for (const [res, want] of cases) {
      const t = setup([res])
      t.p.start()
      await vi.advanceTimersByTimeAsync(60_000)
      expect(t.statuses).toEqual([want])
      expect(t.fetchFn).toHaveBeenCalledTimes(1)
    }
  })

  it('读 404 的响应体期间停止：读完后不再报告状态', async () => {
    const res = new Response('', { status: 404 })
    const body = deferred<string>()
    vi.spyOn(res, 'text').mockReturnValue(body.promise)
    const t = setup([res])
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    t.p.stop()
    body.resolve('没有这次尝试')
    await vi.advanceTimersByTimeAsync(0)
    expect(t.statuses).toEqual([])
  })

  it('停止后不再请求', async () => {
    const t = setup([logRes('', { next: 0 })])
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    t.p.stop()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(t.fetchFn).toHaveBeenCalledTimes(1)
  })

  it('stop 时中止进行中的请求，中止不算断线、不进入退避重试', async () => {
    let signal: AbortSignal | undefined
    const t = setup([], {
      fetchFn: (_input, init) => {
        signal = init?.signal ?? undefined
        return new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => reject(new DOMException('已中止', 'AbortError')))
        })
      },
    })
    t.p.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.fetchFn).toHaveBeenCalledTimes(1)
    expect(signal?.aborted).toBe(false)
    t.p.stop()
    expect(signal?.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.statuses).toEqual([])
    expect(t.fetchFn).toHaveBeenCalledTimes(1)
  })
})

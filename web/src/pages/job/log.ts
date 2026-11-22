// 日志查看器的纯函数：ANSI 转义处理、日志接口响应的解析、退避和搜索。没有 DOM 和网络，便于单测。

/** 页面上最多保留的字符数，超出后丢弃最早的行。 */
export const maxChars = 2_000_000
/** 实时更新的间隔、断线后退避的上限、尝试结束后等待 job 下一步的间隔（毫秒）。 */
export const pollMs = 1500
export const maxBackoffMs = 30_000
export const nextStepMs = 3000
/** 搜索结果最多标出的处数。 */
export const searchLimit = 5000

// 片段末尾不完整的 ANSI 转义序列（留到下一个片段再处理）和完整的 CSI 序列；ESC 是控制字符，正是要匹配的
/* eslint-disable no-control-regex */
const partialEscape = /\x1b(\[[0-9;?]*[ -/]*)?$/
const ansi = /\x1b\[[0-9;?]*[ -/]*[@-~]/g
/* eslint-enable no-control-regex */

/**
 * stripAnsi 去掉 ANSI 转义序列（颜色等）。pending 是上一个片段末尾不完整的转义，与 chunk 拼起来处理；
 * 返回去掉转义后的文字和这次末尾不完整、留到下一次的部分。
 */
export function stripAnsi(pending: string, chunk: string): { text: string; pending: string } {
  const all = pending + chunk
  const rest = partialEscape.exec(all)?.[0] ?? ''
  return { text: all.slice(0, all.length - rest.length).replace(ansi, ''), pending: rest }
}

/** backoff 返回第 failures 次失败后的等待时间：base·2^failures，最多 max。 */
export function backoff(base: number, failures: number, max: number): number {
  return Math.min(base * 2 ** failures, max)
}

/** LogMeta 是日志接口的响应头（internal/server/web 的 getLog）。 */
export interface LogMeta {
  /** 下一次请求的 offset。 */
  next: number
  /** 这次尝试的日志已经结束。 */
  finished: boolean
  /** job 的状态和是否已结束。 */
  state: string
  terminal: boolean
  /** 读取的 attempt 和 job 当前的 attempt（0 表示没有）。 */
  attempt: number
  current: number
}

/** parseLogMeta 解析日志接口的响应头；不是日志（没有 X-Next-Offset、不是 text/plain）时返回 null。 */
export function parseLogMeta(h: Headers): LogMeta | null {
  const type = h.get('Content-Type') ?? ''
  const next = h.get('X-Next-Offset')
  if (!type.startsWith('text/plain') || next === null) return null
  const n = Number(next)
  if (!Number.isFinite(n) || n < 0) return null
  const int = (name: string) => {
    const v = Number(h.get(name))
    return Number.isFinite(v) ? v : 0
  }
  return {
    next: n,
    finished: h.get('X-Finished') === 'true',
    state: h.get('X-Job-State') ?? '',
    terminal: h.get('X-Job-Terminal') === 'true',
    attempt: int('X-Attempt'),
    current: int('X-Current-Attempt'),
  }
}

/** Match 是一处搜索结果：第 line 行（从 0 开始，相对于页面上保留的行）中从 start 开始的 length 个字符。 */
export interface Match {
  line: number
  start: number
  length: number
}

/**
 * findMatches 在各行中查找 term（不区分大小写），最多 limit 处。
 * 小写后长度变化的文字（少数 Unicode 字符）改为区分大小写查找，保证位置正确。
 */
export function findMatches(lines: readonly string[], term: string, limit = searchLimit): Match[] {
  if (term === '') return []
  const out: Match[] = []
  for (let line = 0; line < lines.length && out.length < limit; line++) {
    const text = lines[line] ?? ''
    let hay = text.toLowerCase()
    let needle = term.toLowerCase()
    if (hay.length !== text.length || needle.length !== term.length) {
      hay = text
      needle = term
    }
    for (let i = hay.indexOf(needle); i >= 0 && out.length < limit; i = hay.indexOf(needle, i + needle.length)) {
      out.push({ line, start: i, length: needle.length })
    }
  }
  return out
}

/** stepIndex 返回从 idx 前进 dir（1 或 -1）后的位置（循环）；idx 为 -1 表示还没有选中。 */
export function stepIndex(idx: number, dir: 1 | -1, count: number): number {
  if (count === 0) return -1
  if (idx < 0) return dir > 0 ? 0 : count - 1
  return (idx + dir + count) % count
}

/** logDownloadURL 返回下载完整日志的地址（base 是 job 或 AI 任务的页面地址；attempt 为 0 时取当前尝试）。 */
export function logDownloadURL(base: string, attempt: number): string {
  return `${base}/log/download${attempt ? `?attempt=${attempt}` : ''}`
}

/** 阅读位置（刷新页面后恢复）。 */
export interface SavedPosition {
  follow: boolean
  top: number
}

/** storageKey 返回阅读位置在 sessionStorage 中的键（base 是 job 或 AI 任务的页面地址）。 */
export function storageKey(base: string, attemptKey: string): string {
  return `kiln-log:${base}:${attemptKey}`
}

/** parsePosition 解析保存的阅读位置；格式不对时返回 null。 */
export function parsePosition(raw: string | null): SavedPosition | null {
  if (!raw) return null
  let v: unknown
  try {
    v = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof v !== 'object' || v === null || !('follow' in v) || !('top' in v)) return null
  const { follow, top } = v
  if (typeof follow !== 'boolean' || typeof top !== 'number' || !Number.isFinite(top)) return null
  return { follow, top }
}

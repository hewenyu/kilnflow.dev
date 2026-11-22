import { useEffect, useState } from 'react'
import { ago, clock, dur, toDate, type TimeLike } from '../lib/format'

/** 进行中的耗时每秒更新；"多久之前"精度是秒到分钟，每 30 秒更新就够了。 */
export const secondMs = 1000
const agoTickMs = 30 * secondMs

/** useNow 返回当前时间（毫秒），每 ms 毫秒更新一次；on 为 false 时不更新。 */
export function useNow(ms: number, on = true): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!on) return
    const t = window.setInterval(() => setNow(Date.now()), ms)
    return () => window.clearInterval(t)
  }, [ms, on])
  return now
}

/** Ago 显示"多久之前"，鼠标悬停显示具体时间；没有时间时显示 fallback。 */
export function Ago({ t, fallback = '' }: { t: TimeLike; fallback?: string }) {
  const now = useNow(agoTickMs)
  const d = toDate(t)
  if (!d) return fallback ? <span>{fallback}</span> : null
  return (
    <time dateTime={d.toISOString()} title={clock(t)}>
      {ago(t, now)}
    </time>
  )
}

/** Duration 显示从 start 到 end（Unix 秒）的时长；end 为 0 表示还在进行，每秒更新。 */
export function Duration({ start, end }: { start: number; end: number }) {
  const now = useNow(secondMs, start !== 0 && end === 0)
  if (start === 0) return null
  return <span className="num duration">{dur(start, end, now)}</span>
}

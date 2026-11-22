import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useRouter } from './router'

// 定时刷新：列表和进行中的 run、job 每 5 秒重新取一次页面数据。出错时间隔加倍（最长 60 秒）；
// 页面在后台、正在输入、选中了文字、打开了确认面板或正在提交表单时跳过这一次，不打断正在做的事。

/** 定时刷新的间隔（毫秒）；页面上"每 N 秒自动更新"的说明用 liveSeconds。 */
const interval = 5000
export const liveSeconds = interval / 1000
const maxInterval = 60000

interface Live {
  /** 页面声明自己需要定时刷新（例如 run 还在进行）。 */
  setActive: (on: boolean) => void
  /** 打开确认面板、正在提交表单等时加锁，期间不刷新。 */
  lock: () => () => void
}

const LiveContext = createContext<Live | null>(null)

function useLiveContext(): Live {
  const l = useContext(LiveContext)
  if (!l) throw new Error('需要在 LiveProvider 内使用')
  return l
}

/** useLive 让当前页面在 active 时定时刷新。 */
export function useLive(active: boolean): void {
  const { setActive } = useLiveContext()
  useEffect(() => {
    setActive(active)
    return () => setActive(false)
  }, [active, setActive])
}

/** useRefreshLock 在 locked 时暂停定时刷新（例如确认面板打开时）。 */
export function useRefreshLock(locked: boolean): void {
  const { lock } = useLiveContext()
  useEffect(() => (locked ? lock() : undefined), [locked, lock])
}

/** busyNow 表示用户正在页面上操作：在输入框中、选中了文字。 */
function busyNow(): boolean {
  const el = document.activeElement
  if (el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) return true
  if (el instanceof HTMLInputElement && !['checkbox', 'radio', 'button', 'submit'].includes(el.type)) return true
  const sel = window.getSelection()
  return sel !== null && !sel.isCollapsed && sel.toString().trim() !== ''
}

export function LiveProvider({ children }: { children: ReactNode }) {
  const { view, expired, refresh } = useRouter()
  const [active, setActiveState] = useState(false)
  const locks = useRef(0)
  const setActive = useCallback((on: boolean) => setActiveState(on), [])
  const lock = useCallback(() => {
    locks.current++
    let released = false
    return () => {
      if (!released) locks.current--
      released = true
    }
  }, [])

  const seq = view?.seq
  const fromPost = view?.fromPost ?? false
  useEffect(() => {
    if (!active || fromPost || expired || seq === undefined) return
    let delay = interval
    let timer = 0
    let stopped = false
    // 正在刷新：期间回到页面不再另起一次，免得同时有两条定时链（刷新频率翻倍，旧的计时器也清不掉）
    let running = false
    const tick = async () => {
      if (running) return
      if (document.hidden || locks.current > 0 || busyNow()) {
        schedule(interval)
        return
      }
      running = true
      const ok = await refresh().finally(() => {
        running = false
      })
      if (stopped) return
      delay = ok ? interval : Math.min(delay * 2, maxInterval)
      schedule(delay)
    }
    const schedule = (ms: number) => {
      window.clearTimeout(timer)
      timer = window.setTimeout(() => void tick(), ms)
    }
    // 回到页面时立即刷新一次
    const onVisible = () => {
      if (document.hidden || running) return
      window.clearTimeout(timer)
      void tick()
    }
    schedule(delay)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      stopped = true
      window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [active, fromPost, expired, seq, refresh])

  const value = useMemo(() => ({ setActive, lock }), [setActive, lock])
  return <LiveContext.Provider value={value}>{children}</LiveContext.Provider>
}

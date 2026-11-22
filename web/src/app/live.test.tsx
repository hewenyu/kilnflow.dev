import { act, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { z } from 'zod'
import errorFixture from '../../dev/fixtures/error.json'
import { pages } from '../pages'
import { json, stubFetch } from '../test/fetch'
import { deferred, envelope, renderPage, withData } from '../test/render'
import { definePage } from './definePage'
import { useLive } from './live'

const page = (heading: string) => withData(envelope(errorFixture, { Title: heading }), { Heading: heading, Links: [] })

function LivePage({ data }: { data: { Heading: string } }) {
  useLive(true)
  return <h1>{data.Heading}</h1>
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('定时刷新', () => {
  it('刷新进行中回到页面时不另起一次刷新，之后仍然每 5 秒一次', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const original = pages.error
    pages.error = definePage(z.object({ Heading: z.string() }), LivePage)
    try {
      const pending = deferred<Response>()
      let n = 0
      const calls = stubFetch(() => {
        n++
        return n === 1 ? pending.promise : json(page(`第 ${n} 次`))
      })
      renderPage(page('开始'), '/runs')
      await act(() => vi.advanceTimersByTimeAsync(5000))
      expect(calls).toHaveLength(1)
      // 第一次刷新还没有返回时回到页面
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'))
      })
      expect(calls).toHaveLength(1)
      await act(async () => {
        pending.resolve(json(page('第 1 次')))
        await vi.advanceTimersByTimeAsync(0)
      })
      expect(await screen.findByRole('heading', { name: '第 1 次' })).toBeInTheDocument()
      await act(() => vi.advanceTimersByTimeAsync(5000))
      expect(calls).toHaveLength(2)
      await act(() => vi.advanceTimersByTimeAsync(5000))
      expect(calls).toHaveLength(3)
    } finally {
      pages.error = original
    }
  })
})

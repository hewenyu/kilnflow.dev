import { act, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_ops.json'
import emptyFixture from '../../dev/fixtures/settings_ops.empty.json'
import { envelope, renderPage, withData } from '../test/render'
import { json, stubFetch } from '../test/fetch'

let posts: string[] = []

beforeEach(() => {
  posts = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'POST') posts.push(input instanceof Request ? input.url : String(input))
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/settings/ops' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

const row = (id: number) => {
  const cell = screen.getByText(String(id), { selector: 'td' })
  const tr = cell.closest('tr')
  if (!tr) throw new Error(`没有 #${id} 的行`)
  return within(tr)
}

describe('定时刷新（审计）', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('有还没发送的请求时每 5 秒刷新；都发送完之后不再轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const calls = stubFetch(() => json(envelope(fixture)))
    renderPage(envelope(fixture), '/settings/ops')
    await act(() => vi.advanceTimersByTimeAsync(5000))
    expect(calls.map((c) => c.url)).toEqual(['/settings/ops'])
  })

  it('没有待发送的请求时不轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const done = withData(envelope(fixture), { Outbox: fixture.Data.Outbox.filter((o) => o.State !== 'pending') })
    const calls = stubFetch(() => json(done))
    renderPage(done, '/settings/ops')
    await act(() => vi.advanceTimersByTimeAsync(15_000))
    expect(calls).toHaveLength(0)
  })
})

describe('运维', () => {
  it('显示版本、GitHub App 和公网地址', () => {
    renderPage(envelope(fixture), '/settings/ops')
    expect(screen.getByText('v0.4.2')).toBeInTheDocument()
    expect(screen.getByText('kiln-demo')).toBeInTheDocument()
    expect(screen.getByText('https://kiln.example.com')).toBeInTheDocument()
  })

  it('只有放弃发送的请求可以重试', async () => {
    renderPage(envelope(fixture), '/settings/ops')
    expect(row(5821).queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
    expect(row(5821).getByText(/502 Bad Gateway/)).toBeInTheDocument()
    await userEvent.click(row(5790).getByRole('button', { name: '重试' }))
    expect(posts).toEqual(['/outbox/5790/retry'])
  })

  it('没有待发送的请求时显示全部已送达，GitHub App 未连接', () => {
    renderPage(envelope(emptyFixture), '/settings/ops')
    expect(screen.getByText('全部已送达')).toBeInTheDocument()
    expect(screen.getByText('未连接')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
  })
})

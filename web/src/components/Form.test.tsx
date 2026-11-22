import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import errorFixture from '../../dev/fixtures/error.json'
import { RouterProvider } from '../app/router'
import { LiveProvider } from '../app/live'
import { deferred, envelope } from '../test/render'
import { ConfirmForm } from './Form'

// 确认表单：提交期间忽略关闭和重复确认，只提交一次。

let posts = 0
let pending: Promise<Response>

beforeEach(() => {
  posts = 0
  pending = new Promise<Response>(() => undefined)
  vi.stubGlobal(
    'fetch',
    vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'POST') {
        posts++
        return pending
      }
      // 提交成功后按服务端的重定向取新页面
      return Promise.resolve(new Response(JSON.stringify(envelope(errorFixture)), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderConfirm() {
  return render(
    <RouterProvider initial={envelope(errorFixture)}>
      <LiveProvider>
        <ConfirmForm action="/runs/7/cancel" fields={{ reason: '手动取消' }} button="取消运行" title="取消这个 run？" confirmLabel="确认取消">
          取消后不能恢复。
        </ConfirmForm>
      </LiveProvider>
    </RouterProvider>,
  )
}

describe('ConfirmForm', () => {
  it('提交期间不关闭、不重复提交；完成后关闭', async () => {
    const gate = deferred<Response>()
    pending = gate.promise
    const user = userEvent.setup()
    renderConfirm()
    await user.click(screen.getByRole('button', { name: '取消运行' }))
    const dialog = await screen.findByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: '确认取消' }))
    await waitFor(() => expect(posts).toBe(1))

    // 提交还没返回：Esc、取消、再次点确认都不生效
    await user.keyboard('{Escape}')
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    await user.click(within(dialog).getByRole('button', { name: '确认取消' }))
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    expect(posts).toBe(1)

    gate.resolve(new Response(JSON.stringify({ Redirect: '/runs/7?flash=ok' }), { headers: { 'Content-Type': 'application/json' } }))
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(posts).toBe(1)
  })

  it('没有提交时可以关闭，不发请求', async () => {
    const user = userEvent.setup()
    renderConfirm()
    await user.click(screen.getByRole('button', { name: '取消运行' }))
    const dialog = await screen.findByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(posts).toBe(0)
  })
})

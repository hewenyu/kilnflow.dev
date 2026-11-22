import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_account.json'
import doneFixture from '../../dev/fixtures/settings_account.done.json'
import tokenFixture from '../../dev/fixtures/settings_account.token.json'
import unboundFixture from '../../dev/fixtures/settings_account.unbound.json'
import { envelope, renderPage, withData } from '../test/render'

let posts: { url: string; body: string }[] = []

beforeEach(() => {
  posts = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      if (init?.method === 'POST') posts.push({ url, body: init.body instanceof URLSearchParams ? init.body.toString() : '' })
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/settings/account' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('管理员账号', () => {
  it('没有绑定时第一步是绑定 GitHub 账号，后两步还不能做', () => {
    renderPage(envelope(unboundFixture), '/settings/account')
    expect(screen.getByText('用 admin token 登录，拥有全部管理员权限。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '绑定 GitHub 账号' })).toBeInTheDocument()
    expect(screen.getByText('绑定后，用绑定的账号通过 GitHub 登录一次。')).toBeInTheDocument()
    expect(screen.getByText(/先完成前两步/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '关闭 admin token 登录' })).not.toBeInTheDocument()
    expect(screen.queryByText('危险操作')).not.toBeInTheDocument()
  })

  it('没有配置 GitHub 登录时说明要设置的配置项', () => {
    renderPage(withData(envelope(unboundFixture), { OAuth: false }), '/settings/account')
    expect(screen.getByText('需要先配置 GitHub 登录')).toBeInTheDocument()
    expect(screen.getByText('KILN_GITHUB_CLIENT_ID')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '绑定 GitHub 账号' })).not.toBeInTheDocument()
  })

  it('绑定后用 admin token 登录时，第二步是用 GitHub 登录', () => {
    renderPage(envelope(tokenFixture), '/settings/account')
    expect(screen.getByText('用 octocat 通过 GitHub 登录一次，确认 GitHub 登录可用。会替换当前的登录，完成后回到这里。')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '用 GitHub 登录' })).toHaveAttribute('href', '/auth/github?next=%2Fsettings%2Faccount')
    expect(screen.getByRole('button', { name: '用另一个 GitHub 账号重新绑定' })).toBeInTheDocument()
  })

  it('用绑定的账号登录后可以关闭 admin token 登录', async () => {
    renderPage(envelope(fixture), '/settings/account')
    expect(screen.getByText('当前就是用 octocat 通过 GitHub 登录的，GitHub 登录可用。')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '关闭 admin token 登录' }))
    expect(posts).toHaveLength(1)
    expect(posts[0]?.url).toBe('/settings/account/token-login')
    expect(new URLSearchParams(posts[0]?.body).get('enabled')).toBe('0')
  })

  it('关闭后可以重新开启，并提示 CLI 的恢复命令', () => {
    renderPage(envelope(doneFixture), '/settings/account')
    expect(screen.getByText('已关闭：Dashboard 只能用 GitHub 登录。admin token 仍然可以用于 CLI。')).toBeInTheDocument()
    expect(screen.getByText('kiln admin token-login on')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新开启 admin token 登录' })).toBeInTheDocument()
  })

  it('解除绑定先弹出确认，确认后才提交', async () => {
    renderPage(envelope(fixture), '/settings/account')
    await userEvent.click(screen.getByRole('button', { name: '解除绑定' }))
    const dialog = await screen.findByRole('alertdialog', { name: '解除 octocat 的绑定？' })
    expect(dialog).toHaveTextContent('我确认解除 octocat 的绑定。')
    expect(posts).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: '解除绑定' }))
    expect(posts).toHaveLength(1)
    expect(posts[0]?.url).toBe('/settings/account/unbind')
    expect(new URLSearchParams(posts[0]?.body).get('confirm')).toBe('1')
  })
})

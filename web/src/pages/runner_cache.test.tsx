import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/runner_cache.json'
import noneFixture from '../../dev/fixtures/runner_cache.none.json'
import previewFixture from '../../dev/fixtures/runner_cache.preview.json'
import { chineseLeft, envelope, renderPage, switchLocale, withData } from '../test/render'
import { json, stubFetch } from '../test/fetch'

let requests: { url: string; method: string; body: string }[] = []

beforeEach(() => {
  requests = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      requests.push({ url, method: init?.method ?? 'GET', body: init?.body instanceof URLSearchParams ? init.body.toString() : '' })
      const res = init?.method === 'POST' ? { Redirect: '/runners/cache/mini-01?flash=ok' } : fixture
      return Promise.resolve(new Response(JSON.stringify(res), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

const member = { Session: { Login: 'lin-design', Admin: false, ViaToken: false, Bound: false } }

describe('定时刷新（审计）', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('有还没完成的清理时每 5 秒刷新；都完成之后不再轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const calls = stubFetch(() => json(envelope(fixture)))
    renderPage(envelope(fixture), '/runners/cache/mini-01')
    await act(() => vi.advanceTimersByTimeAsync(5000))
    expect(calls.map((c) => c.url)).toEqual(['/runners/cache/mini-01'])
  })

  it('没有待完成的清理时不轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const done = withData(envelope(fixture), { Purges: fixture.Data.Purges.filter((p) => p.State !== 'pending') })
    const calls = stubFetch(() => json(done))
    renderPage(done, '/runners/cache/mini-01')
    await act(() => vi.advanceTimersByTimeAsync(15_000))
    expect(calls).toHaveLength(0)
  })
})

describe('Runner 的本地缓存', () => {
  it('显示清单、磁盘、上一次自动清理和清理请求', () => {
    renderPage(envelope(fixture), '/runners/cache/mini-01')
    expect(screen.getByRole('heading', { level: 1, name: 'mini-01 的本地缓存' })).toBeInTheDocument()
    expect(screen.getByText('3.1 GiB，共 10 项')).toBeInTheDocument()
    expect(screen.getByText('可用 122.1 GiB / 共 460.4 GiB')).toBeInTheDocument()

    const table = screen.getByRole('table', { name: '缓存清单' })
    const rows = within(table).getAllByRole('row')
    expect(rows).toHaveLength(11)
    const gocache = within(table).getByText('gocache')
    expect(gocache).toHaveAttribute('title', 'toolcache/host/88123/darwin-arm64/gocache')
    expect(within(table).getAllByText('共享').length).toBeGreaterThan(0)

    const gc = screen.getByRole('region', { name: '上一次自动清理' })
    expect(within(gc).getByText(/删除 4 项（1\.7 GiB）/)).toBeInTheDocument()
    expect(within(gc).getByRole('list', { name: '自动清理失败的对象' })).toHaveTextContent('permission denied')

    const purges = screen.getByRole('list', { name: '清理请求' })
    expect(within(purges).getByText('仓库 acme/notes，类别 toolcache，全部执行身份')).toBeInTheDocument()
    expect(within(purges).getByText('等待 Runner 空闲后执行')).toBeInTheDocument()
    expect(within(purges).getByText('已完成')).toBeInTheDocument()
    expect(within(purges).getByText(/删除 5 项（1\.3 GiB）/)).toBeInTheDocument()
  })

  it('预览按所选范围跳转（GET，带 preview=1），不提交清理', async () => {
    renderPage(envelope(fixture), '/runners/cache/mini-01')
    const form = screen.getByRole('form', { name: '清理范围' })
    await userEvent.selectOptions(within(form).getByLabelText('仓库'), 'acme/kiln')
    await userEvent.click(within(form).getByLabelText('toolcache'))
    await userEvent.click(within(form).getByLabelText('git 镜像'))
    await userEvent.click(within(form).getByRole('button', { name: '预览' }))
    await waitFor(() => expect(requests.length).toBeGreaterThan(0))
    expect(requests.every((r) => r.method === 'GET')).toBe(true)
    const u = new URL(requests[0]?.url ?? '', 'http://x')
    expect(u.pathname).toBe('/runners/cache/mini-01')
    expect(u.searchParams.get('preview')).toBe('1')
    expect(u.searchParams.get('repo')).toBe('acme/kiln')
    expect(u.searchParams.getAll('kind')).toEqual(['toolcache', 'git'])
  })

  it('预览列出将删除和继续保留的缓存，确认后提交清理', async () => {
    renderPage(envelope(previewFixture), '/runners/cache/mini-01?repo=acme/kiln&kind=toolcache&kind=git&preview=1')
    const preview = screen.getByRole('region', { name: '预览' })
    expect(within(preview).getByText(/仓库 acme\/kiln，类别 toolcache、git 镜像，全部执行身份：将删除/)).toHaveTextContent('将删除 4 项，共 2.3 GiB')
    expect(within(within(preview).getByRole('table', { name: '将删除的缓存' })).getAllByRole('row')).toHaveLength(5)
    expect(within(preview).getByRole('table', { name: '继续保留的共享缓存' })).toBeInTheDocument()
    expect(within(preview).getByRole('list', { name: '预览说明' })).toHaveTextContent('执行时以 Runner 上的实际内容为准')
    // 表单保留所选的范围
    const form = screen.getByRole('form', { name: '清理范围' })
    expect(within(form).getByLabelText('仓库')).toHaveValue('acme/kiln')
    expect(within(form).getByLabelText('git 镜像')).toBeChecked()
    expect(within(form).getByLabelText('runtime')).not.toBeChecked()

    await userEvent.click(within(preview).getByRole('button', { name: '清理…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/会删除 4 项（2\.3 GiB）/)).toBeInTheDocument()
    expect(within(dialog).getByText(/在没有正在执行的 job 时执行清理/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '确认清理' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
    const post = requests.find((r) => r.method === 'POST')
    expect(post?.url).toBe('/runners/cache/mini-01/purge')
    const body = new URLSearchParams(post?.body)
    expect(body.get('repo')).toBe('acme/kiln')
    expect(body.get('kind')).toBe('toolcache,git')
    expect(body.get('domain')).toBe('')
    expect(body.get('csrf')).toBe('preview')
  })

  it('预览失败时说明原因', () => {
    const env = withData(envelope(previewFixture), {
      Preview: { Matched: null, MatchedBytes: 0, Shared: null, Notes: null, Error: '没有仓库 acme/ghost' },
    })
    renderPage(env, '/runners/cache/mini-01?repo=acme/ghost&preview=1')
    expect(screen.getByText('无法预览')).toBeInTheDocument()
    expect(screen.getByText('没有仓库 acme/ghost')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '清理…' })).not.toBeInTheDocument()
  })

  it('成员只能查看，没有清理表单', () => {
    renderPage(envelope(fixture, member), '/runners/cache/mini-01')
    expect(screen.getByRole('table', { name: '缓存清单' })).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: '清理范围' })).not.toBeInTheDocument()
    expect(screen.getByText('清理缓存需要管理员。')).toBeInTheDocument()
  })

  it('没有上报过清单的 Runner', () => {
    renderPage(envelope(noneFixture), '/runners/cache/mac-02')
    expect(screen.getByText('还没有上报缓存清单')).toBeInTheDocument()
    expect(screen.getByText('已吊销')).toBeInTheDocument()
    expect(screen.getByRole('region', { name: '上一次自动清理' })).toHaveTextContent('还没有记录')
    expect(screen.getByRole('region', { name: '清理请求' })).toHaveTextContent('还没有清理请求。')
  })
})

describe('Runner 本地缓存（英文与切换语言）', () => {
  const rawZH = ['unlinkat gocache/00/0a1b-d: permission denied；用清理容器删除也失败：docker run busybox:1.37：exit status 125：Cannot connect to the Docker daemon']

  it('英文：清单、类别、预览说明（服务端的原因码）和清理记录都按英文显示，没有残留的中文界面文字', () => {
    renderPage(envelope(previewFixture), '/runners/cache/mini-01?preview=1', { locale: 'en' })
    expect(screen.getAllByText('git mirror').length).toBeGreaterThan(0)
    const notes = within(screen.getByRole('list', { name: 'Preview notes' }))
    expect(notes.getByText(/^The preview uses the inventory reported at .+; the purge acts on what is actually on the runner$/)).toBeInTheDocument()
    expect(screen.getByRole('table', { name: 'Caches to be removed' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Purge…' })).toBeInTheDocument()
    expect(chineseLeft(document.body, rawZH)).toEqual([])
  })

  it('预览失败的原因是服务端的消息，切换语言后立即更新', async () => {
    const p = previewFixture.Data.Preview
    const error = { id: 'runnerCache.errRepoNotFound', params: { repo: 'acme/ghost' } }
    renderPage(withData(envelope(previewFixture), { Preview: { ...p, Error: error } }), '/runners/cache/mini-01?preview=1')
    expect(screen.getByText('仓库 acme/ghost 不在 Kiln 中，不能预览。')).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByText("Can't preview")).toBeInTheDocument()
    expect(screen.getByText('Repository acme/ghost is not in Kiln, so there is nothing to preview.')).toBeInTheDocument()
  })

  it('清理确认对话框打开时切换语言：对话框保持打开，范围的文字立即切换', async () => {
    renderPage(envelope(previewFixture), '/runners/cache/mini-01?preview=1')
    await userEvent.click(screen.getByRole('button', { name: '清理…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/^范围：/)).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByRole('alertdialog')).toBe(dialog)
    expect(within(dialog).getByText(/^Scope: /)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Purge' })).toBeInTheDocument()
  })
})


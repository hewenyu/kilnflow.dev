import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import pickerFixture from '../../dev/fixtures/run_new.json'
import formFixture from '../../dev/fixtures/run_new.form.json'
import manualFixture from '../../dev/fixtures/run_new.manual.json'
import noneFixture from '../../dev/fixtures/run_new.none.json'
import { envelope, renderPage, withData } from '../test/render'
import { json, stubFetch } from '../test/fetch'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('启动构建', () => {
  it('没有选定仓库时列出可构建的仓库', () => {
    renderPage(envelope(pickerFixture), '/runs/new')
    expect(screen.getByText('共 5 个可构建的仓库')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '选择 acme/kiln' })).toHaveAttribute('href', '/runs/new?repo=acme%2Fkiln')
    expect(screen.getByText('默认分支 master · public')).toBeInTheDocument()
  })

  it('仓库选择器：搜索结果、分页和仓库错误', () => {
    const env = withData(envelope(pickerFixture), {
      Picker: { Repos: null, Query: 'de', Page: 2, Pages: 3, Total: 120, PrevURL: '/runs/new?q=de', NextURL: '/runs/new?page=3&q=de' },
      Form: { Values: {}, Errors: { repo: '请先选择要构建的仓库' }, Failed: true },
      Attempted: true,
    })
    renderPage(env, '/runs/new?q=de&page=2')
    expect(screen.getByText('没有启动构建。')).toBeInTheDocument()
    expect(screen.getByText('与"de"匹配的 120 个可构建的仓库，第 2 / 3 页')).toBeInTheDocument()
    expect(screen.getByText('没有匹配的仓库。')).toBeInTheDocument()
    expect(screen.getByLabelText('搜索仓库', { selector: 'input' })).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText('请先选择要构建的仓库')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '上一页' })).toHaveAttribute('href', '/runs/new?q=de')
    expect(screen.getByRole('link', { name: '下一页' })).toHaveAttribute('href', '/runs/new?page=3&q=de')
  })

  it('没有任何仓库时说明原因', () => {
    renderPage(withData(envelope(pickerFixture), { Picker: { Repos: null, Query: '', Page: 1, Pages: 1, Total: 0, PrevURL: '', NextURL: '' } }), '/runs/new')
    expect(screen.getByText(/没有可以构建的仓库：确认 App 已安装、账号在 KILN_ALLOWED_ACCOUNTS 中。/)).toBeInTheDocument()
  })

  it('选定仓库后列出 workflow：说明 inputs，不能手动触发的不可选，回填选中项和错误', () => {
    renderPage(envelope(formFixture), '/runs/new?repo=acme%2Fkiln&from=filter')
    expect(screen.getByText('没有启动构建。')).toBeInTheDocument()
    expect(screen.getByText('按运行列表的筛选预选，请确认。')).toBeInTheDocument()
    expect(screen.getByRole('radio', { name: /Release/ })).toBeChecked()
    expect(screen.getByRole('radio', { name: /pr\.yml/ })).toBeDisabled()
    expect(screen.getByText('不能手动触发：没有声明 on.workflow_dispatch。')).toBeInTheDocument()
    expect(screen.getByText(/version（string，必填） 要发布的版本/)).toBeInTheDocument()
    expect(screen.getByText(/mode（choice，默认 fast，可选 fast、full）/)).toBeInTheDocument()
    expect(screen.getByText(/配置有错误，触发后 run 会直接失败并显示错误。/)).toBeInTheDocument()
    expect(screen.getByLabelText('inputs（可选）')).toHaveValue('version=1.2.3\nbroken')
    expect(screen.getByLabelText('inputs（可选）')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText(/第 2 行缺少 =，应写成 key=value：broken/)).toBeInTheDocument()
    expect(screen.getByText(/发布只能由推送 tag 触发/)).toBeInTheDocument()
    expect(screen.getByText(/已读取/)).toHaveTextContent('已读取 refs/heads/main（commit 9baff2b3145c）中的 workflow。')
  })

  it('这个 ref 下没有可以手动触发的 workflow 时说明怎么办，不显示提交按钮', () => {
    renderPage(envelope(noneFixture), '/runs/new?repo=acme%2Fkiln')
    expect(screen.getByText('这个 ref 下没有可以手动触发的 workflow。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '启动构建' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: '取消' })).toHaveAttribute('href', '/runs?repo=acme%2Fkiln')
  })

  it('读取不到 workflow 列表时可以直接填写文件名', () => {
    renderPage(envelope(manualFixture), '/runs/new?repo=acme%2Fkiln')
    expect(screen.getByText(/GitHub 暂时不可用（502 Bad Gateway），稍后重试。可以直接填写 workflow 文件名。/)).toBeInTheDocument()
    expect(screen.getByLabelText('workflow 文件名')).toBeInTheDocument()
    expect(screen.getByLabelText('分支或 tag')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText('找不到 ref nope')).toBeInTheDocument()
    expect(screen.getByText(/修正 ref 后点"读取 workflow"可以从列表中选择。/)).toBeInTheDocument()
  })

  it('提交到 POST /dispatch，带请求 ID、仓库、ref、workflow 和 inputs；提交中不能重复提交', async () => {
    let release: () => void = () => undefined
    const pending = new Promise<Response>((resolve) => {
      release = () => resolve(json({ Redirect: '/runs/140?flash=x' }))
    })
    const calls = stubFetch((c) => (c.method === 'POST' ? pending : json(envelope(formFixture))))
    renderPage(envelope(formFixture), '/runs/new?repo=acme%2Fkiln')
    const button = screen.getByRole('button', { name: '启动构建' })
    await userEvent.click(button)
    await userEvent.click(button)
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
    const post = calls[0]
    expect(post?.url).toBe('/dispatch')
    expect(post?.body.get('request_id')).toBe('7a5255feb6ff1a08d419baa338ab018b')
    expect(post?.body.get('repo')).toBe('acme/kiln')
    expect(post?.body.get('ref')).toBe('main')
    expect(post?.body.get('workflow')).toBe('release.yml')
    expect(post?.body.get('inputs')).toBe('version=1.2.3\nbroken')
    expect(post?.body.get('csrf')).toBe('csrf')
    release()
    await waitFor(() => expect(calls).toHaveLength(2))
    expect(calls[1]?.url).toBe('/runs/140?flash=x')
  })

  it('"读取 workflow" 按表单中的 ref 重新读取（POST /runs/new），不创建 run', async () => {
    const calls = stubFetch(() => json(envelope(noneFixture)))
    renderPage(envelope(formFixture), '/runs/new?repo=acme%2Fkiln')
    const ref = screen.getByLabelText('分支或 tag')
    await userEvent.clear(ref)
    await userEvent.type(ref, 'v0.3.2')
    await userEvent.click(screen.getByRole('button', { name: '读取 workflow' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]?.url).toBe('/runs/new')
    expect(calls[0]?.method).toBe('POST')
    expect(calls[0]?.body.get('ref')).toBe('v0.3.2')
    expect(calls[0]?.body.get('repo')).toBe('acme/kiln')
    expect(await screen.findByText('这个 ref 下没有可以手动触发的 workflow。')).toBeInTheDocument()
  })
})

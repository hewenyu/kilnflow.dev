import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import cachedFixture from '../../dev/fixtures/job.cached.json'
import fixture from '../../dev/fixtures/job.json'
import releaseFixture from '../../dev/fixtures/job.release.json'
import runningFixture from '../../dev/fixtures/job.running.json'
import successFixture from '../../dev/fixtures/job.success.json'
import type { Envelope } from '../lib/envelope'
import { envelope, renderPage, switchLocale, withData } from '../test/render'
import { statusText } from './job/LogViewer'
import { jobPageSchema, type JobPage } from './job/schema'

// job 详情页：页面数据用 dev/fixtures/job*.json，日志接口和表单提交用假的 fetch。

interface Call {
  url: string
  method: string
  body: string
}

let calls: Call[] = []

beforeEach(() => {
  calls = []
  sessionStorage.clear()
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      const body = init?.body instanceof URLSearchParams ? init.body.toString() : ''
      calls.push({ url, method: init?.method ?? 'GET', body })
      const u = new URL(url, 'http://kiln.test')
      if (/^\/jobs\/\d+\/log$/.test(u.pathname)) {
        const first = u.searchParams.get('offset') === '0'
        return Promise.resolve(
          new Response(first ? '\x1b[32mok\x1b[0m  pkg\n--- FAIL: TestRerun\n' : '', {
            headers: {
              'Content-Type': 'text/plain; charset=utf-8',
              'X-Next-Offset': first ? '40' : '40',
              'X-Finished': 'true',
              'X-Job-State': 'failed',
              'X-Job-Terminal': 'true',
              'X-Attempt': '31',
              'X-Current-Attempt': '31',
            },
          }),
        )
      }
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/runs/128?rerun=1&flash=ok' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/** page 返回页面数据，changes 修改 Data（经过 schema 校验，保证测试数据和服务端格式一致）。 */
function page(json: unknown, changes: Partial<JobPage> = {}): Envelope {
  const env = envelope(json)
  const data = jobPageSchema.parse(env.Data)
  return withData(env, { ...data, ...changes })
}

describe('job 详情页', () => {
  it('标题、所在的 run、左侧 job 列表和失败原因', () => {
    renderPage(page(fixture), '/jobs/412')
    // 标题区与 run 详情页相同（run 的标题），job 名称在主栏
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('CI #128')
    expect(screen.getByRole('heading', { level: 2, name: /test/ })).toBeInTheDocument()
    const crumbs = screen.getByRole('navigation', { name: '位置' })
    expect(within(crumbs).getByRole('link', { name: '#128' })).toHaveAttribute('href', '/runs/128?rerun=0')
    const side = screen.getByRole('navigation', { name: 'run 的概要和 job' })
    expect(within(side).getByRole('link', { name: '概要' })).toHaveAttribute('href', '/runs/128?rerun=0')
    expect(within(side).getByRole('link', { name: /test/ })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByText(/internal\/server\/store 中 1 个测试没有通过/)).toBeInTheDocument()
  })

  it('读取日志：去掉 ANSI，读完后显示行数，下载地址带上读到的 attempt', async () => {
    renderPage(page(fixture), '/jobs/412')
    expect(await screen.findByText('日志已完整加载（2 行）。')).toBeInTheDocument()
    expect(Array.from(document.querySelectorAll('.kiln-log-line'), (l) => l.textContent)).toEqual(['ok  pkg', '--- FAIL: TestRerun'])
    expect(calls[0]?.url).toBe('/jobs/412/log?attempt=31&offset=0')
    expect(screen.getByRole('link', { name: '下载完整日志' })).toHaveAttribute('href', '/jobs/412/log/download?attempt=31')
    // download 属性让浏览器自己下载，前端路由也不接管这次点击
    expect(screen.getByRole('link', { name: '下载完整日志' })).toHaveAttribute('download')
  })

  it('在日志中搜索：显示第几处', async () => {
    renderPage(page(fixture), '/jobs/412')
    await screen.findByText('日志已完整加载（2 行）。')
    await userEvent.type(screen.getByRole('searchbox', { name: '在日志中搜索' }), 'fail{Enter}')
    expect(screen.getByText('第 1 / 1 处')).toBeInTheDocument()
    await userEvent.clear(screen.getByRole('searchbox', { name: '在日志中搜索' }))
    await userEvent.type(screen.getByRole('searchbox', { name: '在日志中搜索' }), 'nothing{Enter}')
    expect(screen.getByText('没有找到')).toBeInTheDocument()
  })

  it('尝试：结果、当前、正在查看和其他尝试的日志链接', () => {
    renderPage(page(fixture), '/jobs/412')
    const box = screen.getByRole('region', { name: /尝试/ })
    expect(within(box).getByText('基础设施错误')).toBeInTheDocument()
    expect(within(box).getByText('当前')).toBeInTheDocument()
    expect(within(box).getByText(/Runner mac-mini 失联超过 30 秒/)).toBeInTheDocument()
    expect(within(box).getByRole('link', { name: '查看日志' })).toHaveAttribute('href', '/jobs/412?attempt=30')
    expect(within(box).getByText('正在查看日志')).toBeInTheDocument()
    expect(within(box).getByText(/退出码 1/)).toBeInTheDocument()
  })

  it('没有产物时说明原因', () => {
    renderPage(page(fixture), '/jobs/412')
    expect(screen.getByText('声明了输出 coverage。job 没有成功，没有产物。')).toBeInTheDocument()
  })

  it('没有声明 outputs', () => {
    renderPage(page(fixture, { Declared: [] }), '/jobs/412')
    expect(screen.getByText('这个 job 没有声明 outputs，没有产物。')).toBeInTheDocument()
  })

  it('重跑 job：说明影响，提交到 /jobs/{id}/rerun 并带上 keep 和 csrf', async () => {
    renderPage(page(fixture), '/jobs/412')
    await userEvent.click(screen.getByRole('button', { name: '重跑这个 job' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('这个 job 重新规划，输入没变时仍可能命中缓存。')).toBeInTheDocument()
    expect(within(dialog).getByText('下游 2 个 job 也会重新规划（可命中缓存）：build (linux, amd64)、build (darwin, arm64)。')).toBeInTheDocument()
    expect(within(dialog).getByText('其他 2 个 job 沿用这一次的结果，不重新执行。')).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('checkbox', { name: '失败时保留工作区' }))
    await userEvent.click(within(dialog).getByRole('button', { name: '重跑这个 job 及其下游' }))
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST')).toBe(true)
    })
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/jobs/412/rerun')
    expect(new URLSearchParams(post?.body).get('keep')).toBe('on')
    expect(new URLSearchParams(post?.body).get('csrf')).toBe('csrf-preview')
  })

  it('不在最新一次运行中的 job 不能直接重跑，给出最新的那个 job', () => {
    renderPage(page(fixture, { Rerun: { Latest: false, LatestID: 530, Force: false, Downstream: [], Kept: 0, Publish: false, Replan: 0 } }), '/jobs/412')
    expect(screen.queryByRole('button', { name: '重跑这个 job' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: '打开最新一次运行中的这个 job' })).toHaveAttribute('href', '/jobs/530')
  })

  it('run 还在进行时可以取消整个 run，说明影响', async () => {
    renderPage(page(runningFixture), '/jobs/412')
    expect(screen.queryByRole('button', { name: '重跑这个 job' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '取消整个 run' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/已经发布的 Release 或镜像不会撤回/)).toBeInTheDocument()
  })

  it('产物：文件、大小、可执行和符号链接、下载地址、生成它的尝试', () => {
    renderPage(page(successFixture), '/jobs/413')
    const box = screen.getByRole('region', { name: '产物' })
    expect(within(box).getByText('由第 1 次尝试（#40）生成。下载需要登录；下载地址 5 分钟内有效，不要转发。')).toBeInTheDocument()
    expect(within(box).getByText('3 个文件，46.0 MiB')).toBeInTheDocument()
    expect(within(box).getByText('15.4 MiB')).toBeInTheDocument()
    expect(within(box).getAllByText('可执行')).toHaveLength(3)
    expect(within(box).getByRole('link', { name: '下载 bin/kiln' })).toHaveAttribute('href', '/jobs/413/download?output=bin&path=kiln')
    expect(within(box).getByText('符号链接')).toBeInTheDocument()
    expect(within(box).getByText('→ SHA256SUMS')).toBeInTheDocument()
  })

  it('产物：没有列出的文件数、已不在存储中的文件、已清理', () => {
    const data = jobPageSchema.parse(envelope(successFixture).Data)
    const outputs = data.Outputs
    if (!outputs) throw new Error('fixture 应有产物')
    const [bin, sums] = outputs.Outputs
    if (!bin || !sums) throw new Error('fixture 应有两个输出')
    const missing = { ...bin, FileCount: 5, Files: bin.Files.map((f, i) => (i === 0 ? { ...f, Missing: true } : f)) }
    renderPage(page(successFixture, { Outputs: { ...outputs, Outputs: [missing, sums] } }), '/jobs/413')
    expect(screen.getByText('还有 2 个文件没有列出（只列出按路径排序的前 3 个）。')).toBeInTheDocument()
    expect(screen.getByText('已不在存储中')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '下载 bin/kiln' })).not.toBeInTheDocument()
  })

  it('产物已按保留策略清理', () => {
    renderPage(page(cachedFixture, { Purged: '2026-11-01T08:00:00Z' }), '/jobs/411')
    expect(screen.getByText(/按保留策略清理（run 结束超过保留期后只保留结果），无法下载/)).toBeInTheDocument()
  })

  it('命中缓存、没有执行的 job：只显示原因，不读取日志', () => {
    renderPage(page(cachedFixture), '/jobs/411')
    expect(screen.getByText('这个 job 命中缓存，没有执行；日志见 job #377。')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: 'job #377' })[0]).toHaveAttribute('href', '/jobs/377')
    expect(screen.queryByRole('searchbox')).not.toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('发布 job：各目标的种类、状态、结果和 asset 链接；有目标时不显示旧的发布进度', () => {
    renderPage(page(releaseFixture), '/jobs/520')
    const box = screen.getByRole('region', { name: '发布' })
    expect(within(box).getByText('发布失败')).toBeInTheDocument()
    expect(within(box).getByText('容器镜像')).toBeInTheDocument()
    expect(within(box).getByText('HTTP 上传')).toBeInTheDocument()
    expect(within(box).getByText('GitHub Release')).toBeInTheDocument()
    expect(within(box).getAllByText('已发布')).toHaveLength(4)
    expect(within(box).getByText('已设为最新版。')).toBeInTheDocument()
    expect(within(box).getByRole('link', { name: 'kiln_0.26.0_linux_amd64.tar.gz' })).toHaveAttribute(
      'href',
      'https://dl.example.com/download/v0.26.0/kiln_0.26.0_linux_amd64.tar.gz',
    )
    expect(within(box).getByText('创建 Release：HTTP 502 Bad Gateway')).toBeInTheDocument()
    expect(within(box).queryByRole('link', { name: /releases\/tag/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('region', { name: /尝试/ })).not.toBeInTheDocument()
  })

  it('发布 job：npm 包显示包名@版本、dist-tag 和 access；npm 官方 registry 的包链接到 npmjs.com，已发布过的注明内容一致', () => {
    renderPage(page(releaseFixture), '/jobs/520')
    const box = screen.getByRole('region', { name: '发布' })
    expect(within(box).getAllByText('npm 包')).toHaveLength(2)
    expect(within(box).getByRole('link', { name: '@acme/kiln@0.26.0' })).toHaveAttribute(
      'href',
      'https://www.npmjs.com/package/@acme/kiln/v/0.26.0',
    )
    expect(within(box).getByText('latest')).toBeInTheDocument()
    expect(within(box).getByText(/access public/)).toBeInTheDocument()
    // 其他 registry 不加链接
    expect(within(box).getByText('@acme/kiln-sdk@0.26.0')).toBeInTheDocument()
    expect(within(box).queryByRole('link', { name: '@acme/kiln-sdk@0.26.0' })).not.toBeInTheDocument()
    expect(within(box).getByText(/access restricted/)).toBeInTheDocument()
    expect(within(box).getAllByText('已发布过，内容一致，本次没有重新发布。')).toHaveLength(1)
  })

  it('发布目标中的地址只把 http(s) 渲染成链接：javascript: 地址降级为纯文本', () => {
    const data = jobPageSchema.parse(envelope(releaseFixture).Data)
    const release = data.Release
    if (!release) throw new Error('fixture 应有发布')
    const bad = 'javascript:void(0)'
    // 发布结果来自平台返回的内容：三个目标分别带非法的 html_url、asset 地址和包页面地址
    const targets = [
      { Kind: 'github', Name: 'acme/kiln', Target: 'acme/kiln', State: 'committed', Detail: { html_url: bad }, Error: '', URL: '' },
      {
        Kind: 'upload',
        Name: 'downloads',
        Target: 'https://dl.example.com/api/releases',
        State: 'committed',
        Detail: { latest: true, assets: [{ name: 'kiln.tar.gz', url: bad }] },
        Error: '',
        URL: '',
      },
      {
        Kind: 'npm',
        Name: '@x/y',
        Target: '@x/y',
        State: 'committed',
        Detail: { package: '@x/y', version: '1.0.0' },
        Error: '',
        URL: bad,
      },
    ]
    const view = renderPage(page(releaseFixture, { Release: { ...release, Targets: targets } }), '/jobs/520')
    const box = screen.getByRole('region', { name: '发布' })
    expect(within(box).getByText(bad)).toBeInTheDocument()
    expect(within(box).getByText('kiln.tar.gz')).toBeInTheDocument()
    expect(within(box).getByText('@x/y@1.0.0').closest('a')).toBeNull()
    expect(within(box).queryAllByRole('link').filter((a) => a.getAttribute('href') === bad)).toHaveLength(0)
    view.unmount()

    // 旧版发布进度中的 html_url 同样处理
    const legacy = renderPage(page(releaseFixture, { Release: { ...release, Targets: [], Progress: { html_url: bad } } }), '/jobs/520')
    expect(screen.getByText(bad).closest('a')).toBeNull()
    legacy.unmount()

    // 合法的 https 地址是带 rel="noreferrer" 的链接
    const ok = 'https://github.com/acme/kiln/releases/tag/v0.26.0'
    renderPage(
      page(releaseFixture, {
        Release: {
          ...release,
          Targets: [{ Kind: 'github', Name: 'acme/kiln', Target: 'acme/kiln', State: 'committed', Detail: { html_url: ok }, Error: '', URL: '' }],
        },
      }),
      '/jobs/520',
    )
    expect(screen.getByRole('link', { name: ok })).toHaveAttribute('href', ok)
    expect(screen.getByRole('link', { name: ok })).toHaveAttribute('rel', 'noreferrer')
  })

  it('发布 job：v0.25 之前的发布只有发布进度；暂存的目标', () => {
    const data = jobPageSchema.parse(envelope(releaseFixture).Data)
    const release = data.Release
    if (!release) throw new Error('fixture 应有发布')
    renderPage(page(releaseFixture, { Release: { ...release, Targets: [] } }), '/jobs/520')
    expect(screen.getByRole('link', { name: 'https://github.com/acme/kiln-releases/releases/tag/v0.26.0' })).toBeInTheDocument()
    const staged = { Kind: 'image', Name: 'ghcr.io/x/y', Target: 'ghcr.io/x/y', State: 'staged', Detail: {}, Error: '', URL: '' }
    renderPage(page(releaseFixture, { Release: { ...release, Targets: [staged] } }), '/jobs/520')
    expect(screen.getAllByText('已暂存，未发布').length).toBeGreaterThan(0)
  })

  it('等待中的 job 显示等待原因', () => {
    const data = jobPageSchema.parse(envelope(runningFixture).Data)
    const why = '没有 Runner 的容量满足 job 声明的 resources（需要 4 CPU / 8.0 GiB）：mini-1 容量 2 CPU / 1.9 GiB'
    renderPage(page(runningFixture, { Job: { ...data.Job, State: 'pending', WaitReason: why } }), '/jobs/412')
    expect(screen.getByText(why)).toBeInTheDocument()
    expect(screen.getByText('等待 Runner')).toBeInTheDocument()
    // 不在等待时不显示（领取后旧的原因不再有意义）
    renderPage(page(runningFixture, { Job: { ...data.Job, WaitReason: why } }), '/jobs/412')
    expect(screen.getAllByText(why)).toHaveLength(1)
  })

  it('排队中、没有跳过原因的 job 说明前面还有几个待领取的 job；有跳过原因时只显示原因', () => {
    const data = jobPageSchema.parse(envelope(runningFixture).Data)
    const pending = { ...data.Job, State: 'pending', WaitReason: null }
    let r = renderPage(page(runningFixture, { Job: pending, Queue: { Ahead: 3, FirstRunID: 1432 } }), '/jobs/412')
    expect(screen.getByText('排队位置')).toBeInTheDocument()
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Run #1432' })).toHaveAttribute('href', '/runs/1432')
    expect(screen.getByText(/只计平台 linux\/arm64、executor docker/)).toBeInTheDocument()
    r.unmount()

    r = renderPage(page(runningFixture, { Job: pending, Queue: { Ahead: 0, FirstRunID: 0 } }), '/jobs/412')
    expect(screen.getByText(/前面没有待领取的 job/)).toBeInTheDocument()
    r.unmount()

    // 有跳过原因（资源、配额等）时显示原因，不显示排队位置
    const why = '仓库并发上限 1'
    renderPage(page(runningFixture, { Job: { ...pending, WaitReason: why }, Queue: { Ahead: 3, FirstRunID: 1432 } }), '/jobs/412')
    expect(screen.getByText(why)).toBeInTheDocument()
    expect(screen.queryByText('排队位置')).not.toBeInTheDocument()
  })

  it('执行中的 job 显示当前阶段；上传时显示文件、字节和百分比，已全部发送时说明在等 Server 确认', () => {
    const data = jobPageSchema.parse(envelope(runningFixture).Data)
    let r = renderPage(page(runningFixture), '/jobs/412')
    expect(screen.getByText('当前阶段')).toBeInTheDocument()
    expect(screen.getByText('[2/3] go test ./...', { exact: false })).toBeInTheDocument()
    r.unmount()

    const now = Math.floor(Date.now() / 1000)
    const upload = {
      Phase: 'upload',
      Detail: '上传产物到 Kiln Server',
      Step: 2,
      PhaseStartedAt: now - 90,
      MovedAt: now - 1,
      ReportedAt: now - 1,
      FilesTotal: 1,
      FilesDone: 0,
      FilesSkipped: 0,
      BytesTotal: 135682737,
      BytesDone: 0,
      BytesSent: 67841368,
      Retries: 1,
      LastError: '上传失败：HTTP 502',
    }
    r = renderPage(page(runningFixture, { Job: { ...data.Job, Progress: upload } }), '/jobs/412')
    expect(screen.getByText('已确认 0/1 个文件，已发送 64.7 MiB / 129.4 MiB（49%）')).toBeInTheDocument()
    expect(screen.getByRole('progressbar', { name: '上传产物 49%' })).toBeInTheDocument()
    expect(screen.getByText('第 1 次重试，上次失败：上传失败：HTTP 502')).toBeInTheDocument()
    expect(screen.queryByText(/等待 Server 确认校验/)).not.toBeInTheDocument()
    r.unmount()

    r = renderPage(page(runningFixture, { Job: { ...data.Job, Progress: { ...upload, BytesSent: upload.BytesTotal, Retries: 0 } } }), '/jobs/412')
    expect(screen.getByText(/已全部发送，等待 Server 确认校验/)).toBeInTheDocument()
    expect(screen.queryByText(/没有新的进展/)).not.toBeInTheDocument()
    r.unmount()

    // 长时间没有进展、Runner 没有上报：提示，但不说失败
    renderPage(page(runningFixture, { Job: { ...data.Job, Progress: { ...upload, MovedAt: now - 120, ReportedAt: now - 60 } } }), '/jobs/412')
    expect(screen.getByText(/没有新的进展（仍在等待，不代表失败）/)).toBeInTheDocument()
    expect(screen.getByText(/Runner 已 .*没有上报/)).toBeInTheDocument()
  })

  it('执行中的 job 没有上报进度（旧版 Runner）时说明进度暂不可用', () => {
    const data = jobPageSchema.parse(envelope(runningFixture).Data)
    renderPage(page(runningFixture, { Job: { ...data.Job, Progress: null } }), '/jobs/412')
    expect(screen.getByText(/进度暂不可用/)).toBeInTheDocument()
  })

  it('等待上游的 job 说明在等哪些 job', () => {
    const data = jobPageSchema.parse(envelope(runningFixture).Data)
    renderPage(page(runningFixture, { Job: { ...data.Job, State: 'waiting', Progress: null, WaitingOn: ['dmg', 'macOS 二进制'] } }), '/jobs/412')
    expect(screen.getByText('等 dmg、macOS 二进制 完成后开始。')).toBeInTheDocument()
    expect(screen.queryByText('当前阶段')).not.toBeInTheDocument()
  })

  it('查看历史尝试和地址中的 attempt 无效', () => {
    const data = jobPageSchema.parse(envelope(fixture).Data)
    const first = data.Attempts[0]
    if (!first) throw new Error('fixture 应有尝试')
    renderPage(page(fixture, { Viewing: first, Attempt: first.ID, Explicit: true, AttemptKey: String(first.ID) }), '/jobs/412?attempt=30')
    expect(screen.getByText('正在查看第 1 次尝试')).toBeInTheDocument()
    expect(screen.getAllByText('第 1 次尝试').length).toBeGreaterThan(0)
  })

  it('地址中的 attempt 无效：说明原因，不读取日志', () => {
    renderPage(page(fixture, { AttemptErr: '这个 job 没有尝试 #99（可能属于别的 job，或编号写错了）。' }), '/jobs/412?attempt=99')
    expect(screen.getByText('无法显示这次尝试')).toBeInTheDocument()
    expect(screen.getByText('选择一次有效的尝试后显示日志。')).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })
})

describe('日志状态的说明', () => {
  const counts = { shown: 1234, lines: 56 }
  it('各状态', () => {
    expect(statusText({ kind: 'loading' }, { shown: 0, lines: 0 }, '')).toBe('正在加载日志…')
    expect(statusText({ kind: 'loading' }, counts, '')).toBe('正在加载…（已显示 1,234 个字符）')
    expect(statusText({ kind: 'live' }, { shown: 0, lines: 0 }, '')).toBe('等待日志输出…')
    expect(statusText({ kind: 'live' }, counts, '')).toBe('实时更新中…')
    expect(statusText({ kind: 'complete' }, counts, '')).toBe('日志已完整加载（56 行）。')
    expect(statusText({ kind: 'empty' }, counts, '这次尝试没有输出日志。')).toBe('这次尝试没有输出日志。')
    expect(statusText({ kind: 'retry', why: { id: 'logView.disconnected' }, wait: 3000, failures: 2 }, counts, '')).toBe('连接中断，3 秒后自动重试（第 2 次）；已显示的日志保留。')
    expect(statusText({ kind: 'not-found', message: '没有这次尝试' }, counts, '')).toBe('日志不存在：没有这次尝试。')
    expect(statusText({ kind: 'expired', why: 'unauthorized' }, counts, '')).toBe('登录已过期，日志已停止更新。')
    expect(statusText({ kind: 'failed', status: 400 }, counts, '')).toBe('读取日志失败（HTTP 400），已停止。')
  })
})

describe('job 详情页的语言', () => {
  it('英文：标题区、日志状态、尝试和产物按英文显示，日志正文不翻译', async () => {
    renderPage(page(fixture), '/jobs/412', { locale: 'en' })
    expect(await screen.findByText('Log fully loaded (2 lines).')).toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: 'Breadcrumb' })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Run summary and jobs' })).getByRole('link', { name: 'Summary' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Download full log' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /Attempts/ })).toBeInTheDocument()
    expect(Array.from(document.querySelectorAll('.kiln-log-line'), (l) => l.textContent)).toEqual(['ok  pkg', '--- FAIL: TestRerun'])
  })

  it('读日志、搜索中途切换语言：搜索词、结果位置、阅读位置都保留，日志不重新读取，文字立即切换', async () => {
    renderPage(page(fixture), '/jobs/412')
    await screen.findByText('日志已完整加载（2 行）。')
    const box = screen.getByRole('searchbox', { name: '在日志中搜索' })
    await userEvent.type(box, 'fail{Enter}')
    expect(screen.getByText('第 1 / 1 处')).toBeInTheDocument()
    const body = document.querySelector<HTMLElement>('[tabindex="0"][aria-labelledby="log-h"]')
    if (!body) throw new Error('没有日志区')
    body.scrollTop = 17
    const reads = calls.filter((c) => c.url.includes('/log?')).length

    await switchLocale('English')
    expect(screen.getByText('Log fully loaded (2 lines).')).toBeInTheDocument()
    expect(screen.getByText('1 of 1')).toBeInTheDocument()
    // 同一个输入框和日志区（没有重新挂载），搜索词和阅读位置都在
    expect(screen.getByRole('searchbox', { name: 'Search the log' })).toBe(box)
    expect(box).toHaveValue('fail')
    expect(document.querySelector('[tabindex="0"][aria-labelledby="log-h"]')).toBe(body)
    expect(body.scrollTop).toBe(17)
    expect(screen.getByRole('button', { name: 'Jump to bottom' })).toBeInTheDocument()
    expect(calls.filter((c) => c.url.includes('/log?')).length).toBe(reads)

    await switchLocale('简体中文')
    expect(screen.getByText('第 1 / 1 处')).toBeInTheDocument()
  })

  it('重跑 job 的对话框打开时切换语言：对话框不关闭，勾选保留，说明立即切换', async () => {
    renderPage(page(fixture), '/jobs/412')
    await userEvent.click(screen.getByRole('button', { name: '重跑这个 job' }))
    const dialog = screen.getByRole('dialog')
    await userEvent.click(within(dialog).getByRole('checkbox', { name: '失败时保留工作区' }))
    await switchLocale('English')
    expect(screen.getByRole('dialog')).toBe(dialog)
    expect(within(dialog).getByRole('heading', { name: 'Re-run this job and its downstream jobs' })).toBeInTheDocument()
    expect(within(dialog).getByRole('checkbox', { name: 'Keep the workspace on failure' })).toBeChecked()
  })

  it('服务端的消息（无效的 attempt、没有日志的说明）按当前语言显示', () => {
    renderPage(page(cachedFixture), '/jobs/388', { locale: 'en' })
    expect(screen.getByText(/This job hit the cache and didn't execute; see job #377 for the log\./)).toBeInTheDocument()
  })
})

describe('日志状态的说明（英文）', () => {
  it('状态中的消息按语言显示', () => {
    const counts = { shown: 1234, lines: 1 }
    expect(statusText({ kind: 'loading' }, counts, '', 'en')).toBe('Loading… (1,234 characters shown)')
    expect(statusText({ kind: 'complete' }, counts, '', 'en')).toBe('Log fully loaded (1 line).')
    expect(statusText({ kind: 'empty' }, counts, { id: 'job.logPurged' }, 'en')).toMatch(/^The log was cleaned up under the retention policy/)
    expect(statusText({ kind: 'retry', why: { id: 'logView.serverError', params: { status: 502 } }, wait: 3000, failures: 2 }, counts, '', 'en')).toBe(
      'Server error (HTTP 502). Retrying automatically in 3s (attempt 2); the log shown so far is kept.',
    )
    expect(statusText({ kind: 'not-found', message: { id: 'log.errJobAttemptNotFound', params: { id: 7, attempt: 9 } } }, counts, '', 'en')).toBe(
      "The log doesn't exist: Job #7 has no attempt #9.",
    )
    expect(statusText({ kind: 'not-found', message: '' }, counts, '', 'en')).toBe("The log doesn't exist.")
    expect(statusText({ kind: 'not-found', message: { id: 'log.errJobAttemptNotFound', params: { id: 7, attempt: 9 } } }, counts, '')).toBe(
      '日志不存在：job #7 没有尝试 #9。',
    )
  })
})

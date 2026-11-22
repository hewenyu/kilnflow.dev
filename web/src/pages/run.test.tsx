import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/run.json'
import configFixture from '../../dev/fixtures/run.config.json'
import failedFixture from '../../dev/fixtures/run.failed.json'
import historyFixture from '../../dev/fixtures/run.history.json'
import invalidFixture from '../../dev/fixtures/run.invalid.json'
import purgedFixture from '../../dev/fixtures/run.purged.json'
import { envelope, renderPage, switchLocale, withData } from '../test/render'
import { runPageURL } from './run'
import { json, stubFetch } from '../test/fetch'
import { modeHints } from './run/RerunDialog'

afterEach(() => {
  vi.unstubAllGlobals()
})

const failedForm = { Default: 'failed', Total: 6, Retry: 4, Kept: 2, Publish: 0, Effects: 0, Purged: false }

describe('run 详情页', () => {
  it('按阶段列出 job：名称链接到 job 页，第二行说明沿用、缓存或耗时', () => {
    renderPage(envelope(fixture), '/runs/128')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('CI #128')
    const stages = screen.getAllByRole('list', { name: /^阶段 \d$/ })
    expect(stages).toHaveLength(3)
    const first = stages[0]
    if (!first) throw new Error('没有阶段 1')
    expect(within(first).getByRole('link', { name: /^test \(go\)/ })).toHaveAttribute('href', '/jobs/402')
    expect(within(first).getByText('命中缓存')).toBeInTheDocument()
    expect(within(first).getByRole('link', { name: '#377' })).toHaveAttribute('href', '/jobs/377')
    expect(within(first).getByRole('link', { name: '#388' })).toHaveAttribute('href', '/jobs/388')
    expect(screen.getByText(/按依赖分成 3 个阶段，从左到右执行/)).toBeInTheDocument()
  })

  it('等待中的 job 在第二行说明等待原因', () => {
    const env = envelope(fixture)
    const why = '等待资源释放（需要 1 CPU / 1.0 GiB）：mini-2 空闲 0 CPU / 0.5 GiB'
    const stages = fixture.Data.Stages.map((st) => ({
      ...st,
      Jobs: st.Jobs.map((j) => (j.ID === 406 ? { ...j, State: 'pending', WaitReason: why } : j)),
    }))
    renderPage(withData(env, { Stages: stages }), '/runs/128')
    const last = screen.getAllByRole('list', { name: /^阶段 \d$/ })[2]
    if (!last) throw new Error('没有阶段 3')
    expect(within(last).getByText(`等待：${why}`)).toBeInTheDocument()
  })

  it('执行中的 job 在第二行显示当前阶段，等待中的 job 说明在等哪些上游', () => {
    renderPage(envelope(fixture), '/runs/128')
    const [, second, last] = screen.getAllByRole('list', { name: /^阶段 \d$/ })
    if (!second || !last) throw new Error('没有阶段 2、3')
    expect(within(second).getByText(/上传产物 50%/)).toBeInTheDocument()
    expect(within(second).getByText(/\[3\/4\] go build/)).toBeInTheDocument()
    expect(within(last).getByText('等待 build (darwin/arm64)、build (linux/amd64)')).toBeInTheDocument()
  })

  it('左侧列出概要和这次运行的全部 job', () => {
    renderPage(envelope(fixture), '/runs/128')
    const nav = screen.getByRole('navigation', { name: 'run 的概要和 job' })
    expect(within(nav).getByRole('link', { name: '概要' })).toHaveAttribute('aria-current', 'page')
    expect(within(nav).getByRole('link', { name: /package/ })).toHaveAttribute('href', '/jobs/406')
  })

  it('进行中的 run 可以取消：确认后提交 POST /runs/{id}/cancel', async () => {
    const calls = stubFetch(() => json({ Redirect: '/runs/128?flash=x' }))
    renderPage(envelope(fixture), '/runs/128')
    expect(screen.queryByRole('button', { name: '重跑' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '取消运行' }))
    const dialog = screen.getByRole('alertdialog')
    expect(within(dialog).getByText(/取消这个 run 的第 1 次重跑：正在执行的 job 会被终止/)).toBeInTheDocument()
    expect(within(dialog).getByText(/已经发布的 Release 或镜像不会撤回/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '取消这个 run' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/runs/128/cancel')
    expect(post?.body.get('csrf')).toBe('csrf')
  })

  it('结束的 run 可以重跑：默认只重跑未成功的 job，说明沿用和重新规划的数量，可以保留工作区', async () => {
    const calls = stubFetch(() => json({ Redirect: '/runs/128?rerun=2&flash=x' }))
    renderPage(envelope(failedFixture), '/runs/128')
    expect(screen.queryByRole('button', { name: '取消运行' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '重跑' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByRole('radio', { name: '只重跑未成功的 job' })).toBeChecked()
    expect(within(dialog).getByText(/4 个未成功（失败、取消、跳过）的 job 重新规划/)).toBeInTheDocument()
    expect(within(dialog).getByText(/2 个成功的 job 沿用上一次的结果/)).toBeInTheDocument()
    expect(within(dialog).getByText(/基于最新一次（第 1 次重跑，失败）的结果/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('checkbox', { name: '失败时保留工作区' }))
    await userEvent.click(within(dialog).getByRole('button', { name: '重跑' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/runs/128/rerun')
    expect(post?.body.get('mode')).toBe('failed')
    expect(post?.body.get('keep')).toBe('on')
    expect(post?.body.get('csrf')).toBe('csrf')
  })

  it('全部成功时不能只重跑未成功的 job，默认全部重跑', async () => {
    const env = withData(envelope(failedFixture), { Form: { ...failedForm, Default: 'all', Retry: 0, Kept: 6 } })
    renderPage(env, '/runs/128')
    await userEvent.click(screen.getByRole('button', { name: '重跑' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByRole('radio', { name: '只重跑未成功的 job' })).toBeDisabled()
    expect(within(dialog).getByRole('radio', { name: '全部重跑（可命中缓存）' })).toBeChecked()
    expect(within(dialog).getByText('最新一次运行中所有 job 都成功了，没有需要重跑的 job。')).toBeInTheDocument()
  })

  it('重跑说明：发布和有副作用的 job 不使用缓存；产物已清理时只有发布 job 沿用', () => {
    const h = modeHints({ ...failedForm, Publish: 1, Effects: 2 })
    expect(h.failed).toContain('，已成功的发布不会再次执行。')
    expect(h.all).toContain('1 个发布 job 和 2 个有副作用（effects）的 job 不使用缓存，会再次执行（发布按 tag 幂等')
    expect(h.noCache).toBe('全部 6 个 job 强制重新执行，不读取缓存，用于怀疑缓存结果有问题时；耗时最长。发布 job 会再次执行。')
    const purged = modeHints({ ...failedForm, Publish: 1, Purged: true })
    expect(purged.failed).toContain('2 个已成功的发布 job 沿用上一次的结果，不重新执行。')
  })

  it('产物已清理时说明清理时间，重跑对话框也说明无法沿用', async () => {
    renderPage(envelope(purgedFixture), '/runs/123')
    expect(screen.getByText('日志和产物已清理。')).toBeInTheDocument()
    expect(screen.getByText(/按保留策略清理了日志和产物，只保留结果（状态、耗时、原因）/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '重跑' }))
    expect(screen.getByText(/最新一次运行的产物已经按保留策略清理/)).toBeInTheDocument()
  })

  it('查看历史运行时提示最新一次的状态，并链接到最新一次', () => {
    renderPage(envelope(historyFixture), '/runs/128?rerun=0')
    expect(screen.getByText('正在查看历史：首次运行（失败）')).toBeInTheDocument()
    expect(screen.getByText(/这个 run 最新的是第 1 次重跑，失败。/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '查看最新一次' })).toHaveAttribute('href', '/runs/128')
  })

  it('可以切换查看每一次运行', async () => {
    renderPage(envelope(failedFixture), '/runs/128')
    await userEvent.click(screen.getByRole('button', { name: /正在查看第 1 次重跑/ }))
    expect(await screen.findByRole('menuitem', { name: /首次运行/ })).toHaveAttribute('href', '/runs/128?rerun=0')
  })

  it('地址中的 rerun 无效时说明共运行了几次，不显示 job', () => {
    renderPage(envelope(invalidFixture), '/runs/128?rerun=7')
    expect(screen.getByText('没有这一次运行（rerun=7）。')).toBeInTheDocument()
    expect(screen.getByText(/这个 run 共运行了 2 次：首次运行到第 1 次重跑。/)).toBeInTheDocument()
    expect(screen.queryByRole('list', { name: /^阶段/ })).not.toBeInTheDocument()
  })

  it('配置错误和警告原样显示；没有 job 时说明原因', () => {
    renderPage(envelope(configFixture), '/runs/128')
    expect(screen.getByText('配置错误')).toBeInTheDocument()
    expect(screen.getByText(/jobs\.test\.services\.postgres\.image 不能为空/)).toBeInTheDocument()
    expect(screen.getByText(/timeout-minutes 超过 Runner 的上限/)).toBeInTheDocument()
    expect(screen.getByText('这次运行没有 job（配置错误）。')).toBeInTheDocument()
  })

  it('活动：触发、重跑、取消和运行结束', () => {
    renderPage(envelope(failedFixture), '/runs/128')
    expect(screen.getByText(/触发了这个 run（推送）/)).toBeInTheDocument()
    expect(screen.getByText(/重跑了这个 run（只重跑未成功的 job），开始第 1 次重跑/)).toBeInTheDocument()
    expect(screen.getByText('取消了这个 run', { exact: false })).toBeInTheDocument()
    expect(screen.getByText(/运行结束：失败/)).toBeInTheDocument()
  })

  describe('PR 的合并队列状态', () => {
    const mq = (entry: Record<string, unknown> | null, pos = 0) => ({
      MergeQueue: {
        Repo: 'acme/kiln',
        PR: 104,
        Pos: pos,
        URL: '/merge-queue?repo=acme%2Fkiln',
        Entry: entry && {
          PR: 104,
          State: 'queued',
          HeadSHA: 'b4182bff4b3cf75f9e54f4990f9bd153c0c2973c',
          BaseRef: 'main',
          EnqueuedBy: 'github:octocat',
          Status: '',
          Reason: '',
          Removal: '',
          MergeSHA: '',
          EnqueuedAt: '2026-11-05T04:38:00-08:00',
          FinishedAt: null,
          ...entry,
        },
      },
    })

    it('排队中：显示目标分支、位置和在等什么，链接到合并队列页，没有加入按钮', () => {
      const { unmount } = renderPage(withData(envelope(failedFixture), mq({ Status: '等待验证：1/2 个 run 已结束' }, 2)), '/runs/128')
      expect(screen.getByText('合并队列：目标 main · 第 2 个 · 等待验证：1/2 个 run 已结束')).toBeInTheDocument()
      expect(screen.getByRole('link', { name: '查看合并队列' })).toHaveAttribute('href', '/merge-queue?repo=acme%2Fkiln')
      expect(screen.queryByRole('button', { name: '加入合并队列' })).not.toBeInTheDocument()
      unmount()
      // 多目标分支（PLAN #28.5）：队列位置与目标分支一起显示
      renderPage(withData(envelope(failedFixture), mq({ BaseRef: 'dev', Status: '' }, 1)), '/runs/128')
      expect(screen.getByText('合并队列：目标 dev · 第 1 个')).toBeInTheDocument()
    })

    it('已合并：显示合并提交，或者在队列之外合并的说明', () => {
      const merged = { State: 'merged', MergeSHA: '5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f', FinishedAt: '2026-11-05T05:00:00-08:00' }
      const { unmount } = renderPage(withData(envelope(failedFixture), mq(merged)), '/runs/128')
      expect(screen.getByText('已由合并队列合并（5e6f7a8b9c0d）')).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '加入合并队列' })).not.toBeInTheDocument()
      unmount()
      renderPage(withData(envelope(failedFixture), mq({ ...merged, MergeSHA: '', Reason: 'PR 在合并队列之外被合并' })), '/runs/128')
      expect(screen.getByText('PR 在合并队列之外被合并')).toBeInTheDocument()
    })

    it('已移出：显示原因，管理员可以重新加入', async () => {
      const calls = stubFetch(() => json({ Redirect: '/merge-queue?repo=acme%2Fkiln&flash=ok' }))
      renderPage(withData(envelope(failedFixture), mq({ State: 'removed', Reason: '验证失败：ci.yml' })), '/runs/128')
      expect(screen.getByText('已移出合并队列：验证失败：ci.yml')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: '加入合并队列' }))
      const dialog = await screen.findByRole('alertdialog')
      expect(within(dialog).getByText(/加上 label kiln:merge/)).toBeInTheDocument()
      await userEvent.click(within(dialog).getByRole('button', { name: '加入合并队列' }))
      await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
      const post = calls.find((c) => c.method === 'POST')
      expect(post?.url).toBe('/merge-queue/add')
      expect(post?.body.get('repo')).toBe('acme/kiln')
      expect(post?.body.get('pr')).toBe('104')
    })

    it('没有记录时不显示状态；管理员可以加入，成员没有按钮', () => {
      const { unmount } = renderPage(withData(envelope(failedFixture), mq(null)), '/runs/128')
      expect(screen.queryByText(/合并队列：/)).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: '查看合并队列' })).not.toBeInTheDocument()
      expect(screen.getByRole('button', { name: '加入合并队列' })).toBeInTheDocument()
      unmount()
      const member = { Session: { Login: 'bob', Admin: false, ViaToken: false, Bound: false } }
      renderPage(withData(envelope(failedFixture, member), mq(null)), '/runs/128')
      expect(screen.queryByRole('button', { name: '加入合并队列' })).not.toBeInTheDocument()
    })

    it('不是 PR 的 run 没有合并队列', () => {
      renderPage(envelope(failedFixture), '/runs/128')
      expect(screen.queryByRole('button', { name: '加入合并队列' })).not.toBeInTheDocument()
      expect(screen.queryByText(/合并队列：|由合并队列合并|移出合并队列/)).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: '查看合并队列' })).not.toBeInTheDocument()
    })
  })

  it('runPageURL 带上 rerun 和返回的列表地址', () => {
    expect(runPageURL(7, null, '')).toBe('/runs/7')
    expect(runPageURL(7, 0, '/runs?state=failed')).toBe('/runs/7?rerun=0&back=%2Fruns%3Fstate%3Dfailed')
  })
})

describe('run 详情页的语言', () => {
  it('英文：标题（服务端的消息）、阶段、活动（服务端的消息）和提示', () => {
    renderPage(envelope(failedFixture), '/runs/128', { locale: 'en' })
    expect(document.title).toBe('Run #128 · Kiln')
    expect(screen.getAllByRole('list', { name: /^Stage \d$/ }).length).toBeGreaterThan(0)
    expect(screen.getByRole('heading', { name: 'Activity' })).toBeInTheDocument()
    expect(screen.getByText(/re-ran this run \(only jobs that didn't succeed\), starting re-run 1/)).toBeInTheDocument()
    expect(screen.getByText(/cancelled this run/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Re-run' })).toBeInTheDocument()
  })

  it('重跑对话框打开时切换语言：对话框不关闭，选中的方式和勾选保留，说明立即切换', async () => {
    renderPage(envelope(failedFixture), '/runs/128')
    await userEvent.click(screen.getByRole('button', { name: '重跑' }))
    const dialog = screen.getByRole('dialog')
    await userEvent.click(within(dialog).getByRole('radio', { name: '全部重跑（不用缓存）' }))
    await userEvent.click(within(dialog).getByRole('checkbox', { name: '失败时保留工作区' }))
    await switchLocale('English')
    expect(screen.getByRole('dialog')).toBe(dialog)
    expect(within(dialog).getByRole('heading', { name: 'Re-run this run' })).toBeInTheDocument()
    expect(within(dialog).getByRole('radio', { name: 'All jobs (without cache)' })).toBeChecked()
    expect(within(dialog).getByRole('checkbox', { name: 'Keep the workspace on failure' })).toBeChecked()
    expect(within(dialog).getByText(/^4 jobs that didn't succeed \(failed, cancelled, skipped\) are replanned/)).toBeInTheDocument()
    expect(document.title).toBe('Run #128 · Kiln')
  })

  it('modeHints 按语言说明三种重跑方式', () => {
    const h = modeHints({ ...failedForm, Publish: 1, Effects: 1 }, 'en')
    expect(h.failed).toBe(
      "4 jobs that didn't succeed (failed, cancelled, skipped) are replanned; jobs with unchanged inputs still hit the cache; 2 successful jobs reuse the previous result and don't run again, and successful publishes don't run again.",
    )
    expect(h.all).toMatch(/^All 6 jobs are replanned;.* 1 publish job and 1 job with side effects \(effects\) don't use the cache and run again \(publishing is idempotent per tag/)
    expect(h.noCache).toMatch(/^All 6 jobs are forced to run again.* Publish jobs run again\.$/)
  })
})

import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ReleaseSection } from './sections'
import { uploadDetailOf, type Release, type ReleaseTarget } from './schema'

// 上传目标的 detail 是 Server 清洗后的形态：不安全的 asset URL 被删除，只保留 name；
// 前端必须能解析这种形态，不能因为一个 asset 没有 url 就丢掉同批其他下载链接和 latest 标记。

const detail = {
  latest: true,
  assets: [{ name: 'ok.zip', url: 'https://example.com/ok.zip' }, { name: 'bad.zip' }],
}

const target: ReleaseTarget = {
  Kind: 'upload',
  Name: '上传',
  Target: 'https://upload.example.com',
  State: 'committed',
  Detail: detail,
  Error: '',
  URL: '',
}

const release: Release = {
  Target: 'tester/app',
  Tag: 'v1.2.0',
  State: 'succeeded',
  Stage: 'done',
  Progress: {},
  Error: '',
  Targets: [target],
}

const releaseOf = (d: Record<string, unknown>): Release => ({ ...release, Targets: [{ ...target, Detail: d }] })

describe('ReleaseSection 上传目标', () => {
  it('asset 缺少 url 时仍保留同批下载链接和最新版标记', () => {
    // 与 Server 清洗后的同一份 JSON 形态：两个 asset 都要解析出来，latest 也要保留。
    const d = uploadDetailOf(detail)
    expect(d.assets).toEqual([
      { name: 'ok.zip', url: 'https://example.com/ok.zip' },
      { name: 'bad.zip' },
    ])
    expect(d.latest).toBe(true)

    render(<ReleaseSection release={release} />)
    const ok = screen.getByRole('link', { name: 'ok.zip' })
    expect(ok).toHaveAttribute('href', 'https://example.com/ok.zip')
    expect(ok).toHaveAttribute('rel', 'noreferrer')
    // 没有 url 的 asset 渲染成纯文本，不可点击。
    expect(screen.getByText('bad.zip').closest('a')).toBeNull()
    expect(screen.getByText('已设为最新版。')).toBeInTheDocument()
  })

  it('有 url 和没有 url 的 asset 混排时 React key 不碰撞', () => {
    // 旧 key（name:url 与 name:idx 拼接）下第一项的 "a:1" 会和第二项的 "a:1" 碰撞。
    const error = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      render(<ReleaseSection release={releaseOf({ latest: true, assets: [{ name: 'a', url: '1' }, { name: 'a' }] })} />)
      expect(screen.getAllByText('a')).toHaveLength(2)
      expect(error.mock.calls.some((c) => String(c[0]).includes('same key'))).toBe(false)
    } finally {
      error.mockRestore()
    }
  })

  it.each([{ assets: null }, { assets: [] }])('assets 为 %o 时仍显示最新版', ({ assets }) => {
    const d = uploadDetailOf({ latest: true, assets })
    expect(d.latest).toBe(true)
    expect(d.assets ?? []).toEqual([])

    render(<ReleaseSection release={releaseOf({ latest: true, assets })} />)
    expect(screen.getByText('已设为最新版。')).toBeInTheDocument()
  })
})

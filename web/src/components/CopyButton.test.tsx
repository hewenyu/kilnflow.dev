import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CopyButton } from './CopyButton'

describe('CopyButton', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('复制成功后说明已复制', async () => {
    const writeText = vi.fn(() => Promise.resolve())
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    render(<CopyButton value="v1.2.3" label="复制版本号" />)
    await userEvent.click(screen.getByRole('button', { name: '复制版本号' }))
    expect(writeText).toHaveBeenCalledWith('v1.2.3')
    expect(await screen.findByRole('button', { name: '已复制' })).toBeInTheDocument()
  })

  it('没有剪贴板接口（不是 https）时说明复制失败，不抛出错误', async () => {
    vi.stubGlobal('navigator', {})
    render(<CopyButton value="v1.2.3" label="复制版本号" />)
    await userEvent.click(screen.getByRole('button', { name: '复制版本号' }))
    expect(await screen.findByRole('button', { name: '复制失败，请手动复制' })).toBeInTheDocument()
  })
})

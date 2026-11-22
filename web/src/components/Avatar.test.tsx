import { fireEvent, render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Avatar } from './Avatar'

describe('头像', () => {
  it('加载失败后显示首字母；换了账号（地址变了）时重新尝试（审计）', () => {
    const { container, rerender } = render(<Avatar login="lin-design" />)
    const img = () => container.querySelector('img')
    expect(img()).not.toBeNull()
    fireEvent.error(img() ?? container)
    expect(img()).toBeNull()
    // 同一个账号再次渲染：不再反复请求加载不了的地址
    rerender(<Avatar login="lin-design" />)
    expect(img()).toBeNull()
    // 换了账号：新的地址照样尝试
    rerender(<Avatar login="octocat" />)
    expect(img()).not.toBeNull()
  })
})

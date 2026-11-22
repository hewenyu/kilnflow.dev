import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LocaleProvider, storageKey, useLocale, useT } from './locale'

// 当前语言：手动选择（浏览器存储）> 浏览器语言 > zh-CN；切换后同步 <html lang> 并保存；存储不可用时退化为内存状态。

function Probe() {
  const t = useT()
  const { locale, setLocale } = useLocale()
  return (
    <>
      <p data-testid="text">{t('shell.logout')}</p>
      <p data-testid="locale">{locale}</p>
      <button type="button" onClick={() => setLocale(locale === 'en' ? 'zh-CN' : 'en')}>
        switch
      </button>
    </>
  )
}

function languages(langs: string[]) {
  vi.spyOn(window.navigator, 'languages', 'get').mockReturnValue(langs)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('LocaleProvider', () => {
  it('没有手动选择时按浏览器语言，并同步 <html lang>', () => {
    languages(['en-GB', 'zh-CN'])
    render(<LocaleProvider><Probe /></LocaleProvider>)
    expect(screen.getByTestId('text')).toHaveTextContent('Sign out')
    expect(document.documentElement.lang).toBe('en')
  })

  it('浏览器语言都不支持时用 zh-CN', () => {
    languages(['fr-FR', 'de'])
    render(<LocaleProvider><Probe /></LocaleProvider>)
    expect(screen.getByTestId('locale')).toHaveTextContent('zh-CN')
    expect(document.documentElement.lang).toBe('zh-CN')
  })

  it('手动选择优先于浏览器语言；不认识的值忽略', () => {
    languages(['en-US'])
    window.localStorage.setItem(storageKey, 'zh-CN')
    const { unmount } = render(<LocaleProvider><Probe /></LocaleProvider>)
    expect(screen.getByTestId('text')).toHaveTextContent('退出登录')
    unmount()
    window.localStorage.setItem(storageKey, 'klingon')
    render(<LocaleProvider><Probe /></LocaleProvider>)
    expect(screen.getByTestId('locale')).toHaveTextContent('en')
  })

  it('切换后立即生效、保存选择（刷新、OAuth 往返、退出登录后保持）', () => {
    languages(['zh-CN'])
    render(<LocaleProvider><Probe /></LocaleProvider>)
    act(() => screen.getByRole('button').click())
    expect(screen.getByTestId('text')).toHaveTextContent('Sign out')
    expect(document.documentElement.lang).toBe('en')
    expect(window.localStorage.getItem(storageKey)).toBe('en')
  })

  it('浏览器存储不可用时按浏览器语言打开，切换只在内存中生效', () => {
    languages(['zh-CN'])
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('denied', 'SecurityError')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('denied', 'SecurityError')
    })
    render(<LocaleProvider><Probe /></LocaleProvider>)
    expect(screen.getByTestId('text')).toHaveTextContent('退出登录')
    act(() => screen.getByRole('button').click())
    expect(screen.getByTestId('text')).toHaveTextContent('Sign out')
  })
})

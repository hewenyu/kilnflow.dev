import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// jsdom 没有的浏览器接口（Primer 的组件用到）：matchMedia 总是不匹配，ResizeObserver、IntersectionObserver 不回调。
{
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: (query: string): MediaQueryList => {
      const list = new EventTarget()
      return Object.assign(list, {
        matches: false,
        media: query,
        onchange: null,
        addListener: () => undefined,
        removeListener: () => undefined,
      })
    },
  })
}

class NoopObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
  takeRecords(): [] {
    return []
  }
}

Object.defineProperty(window, 'ResizeObserver', { configurable: true, value: NoopObserver })
Object.defineProperty(window, 'IntersectionObserver', { configurable: true, value: NoopObserver })
// 前端内跳转后回到页面顶部；jsdom 没有实现，只会打印"Not implemented"
Object.defineProperty(window, 'scrollTo', { configurable: true, value: () => undefined })

// Popover API（Primer 的 Tooltip 用到；没有时它会加载需要 adoptedStyleSheets 的 polyfill）
Object.defineProperty(HTMLElement.prototype, 'popover', { configurable: true, writable: true, value: null })
Object.defineProperty(HTMLElement.prototype, 'showPopover', { configurable: true, value: () => undefined })
Object.defineProperty(HTMLElement.prototype, 'hidePopover', { configurable: true, value: () => undefined })
Object.defineProperty(HTMLElement.prototype, 'togglePopover', { configurable: true, value: () => false })

afterEach(() => {
  cleanup()
  // 语言选择保存在浏览器存储中：用例之间不互相影响
  window.localStorage.clear()
  document.documentElement.lang = ''
})

import { fireEvent, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { stubFetch } from '../test/fetch'
import { envelope, chineseLeft, rawStrings, renderPage, switchLocale } from '../test/render'

// 全站的 fixtures（dev/fixtures/*.json，每个页面的各种状态）都在英文界面中渲染：界面文字全部来自词典，
// 剩下的中文只能是服务端原样给出的内容（报告、原因、Issue 标题等，见 rawStrings）。
// 按语言切换（不重新取数据）后，标题和正文都随之切换，切回中文后与切换前相同。

const fixtures = import.meta.glob<{ default: unknown }>('../../dev/fixtures/*.json', { eager: true })

const cases = Object.entries(fixtures).map(([path, mod]) => ({ name: path.replace(/^.*\//, ''), json: mod.default }))

const han = /[一-鿿]/

/**
 * formErrorStrings 返回页面数据中表单（Form、EnvForm 等带 Errors 的对象）字段错误里的字符串：字段错误都是服务端给的界面文字，
 * 应是消息（{id, params}）。rawStrings 把含中文的字符串都当作原样内容从检查中去掉，没迁移的中文错误会因此漏检，所以单独检查。
 */
function formErrorStrings(data: unknown): string[] {
  const out: string[] = []
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) {
      v.forEach(walk)
    } else if (typeof v === 'object' && v !== null) {
      const entries: [string, unknown][] = Object.entries(v)
      for (const [k, x] of entries) {
        if (k === 'Errors' && typeof x === 'object' && x !== null) {
          for (const e of Object.values(x)) if (typeof e === 'string') out.push(e)
        }
        walk(x)
      }
    }
  }
  walk(data)
  return out
}

/**
 * chineseInterface 返回页面数据的外层（Title、Flash）中写成中文字符串的界面文字：标题和提示都应是消息，
 * 只有对象名本身作标题时（作用域页的 acme/release、job 页的 job 名称）才是原样的字符串。
 */
function chineseInterface(json: unknown): string[] {
  if (typeof json !== 'object' || json === null) return []
  const out: string[] = []
  for (const [k, v] of Object.entries(json)) if ((k === 'Title' || k === 'Flash') && typeof v === 'string' && han.test(v)) out.push(`${k}: ${v}`)
  return out
}

/** toLocale 在当前页面上切换语言：已登录时在身份菜单中，未登录的页面（登录页、错误页）用右上角的语言按钮。 */
async function toLocale(name: 'English' | '简体中文', loggedIn: boolean): Promise<void> {
  if (loggedIn) {
    await switchLocale(name)
    return
  }
  fireEvent.click(screen.getByRole('button', { name: /^切换语言|^Change language/ }))
  fireEvent.click(await screen.findByRole('menuitemradio', { name }))
}

beforeEach(() => {
  // 页面自己发起的请求（日志、定时刷新）一直不返回：只检查页面数据渲染出的界面
  stubFetch(() => new Promise<Response>(() => undefined))
})

describe('全站 fixtures 的英文界面', () => {
  it('覆盖全部页面的 fixtures', () => {
    const pages = new Set(cases.map((c) => envelope(c.json).Page))
    expect(cases.length).toBeGreaterThanOrEqual(90)
    for (const p of ['overview', 'runs', 'run', 'run_new', 'job', 'merge_queue', 'ai_dev', 'ai_dev_item', 'ai_requests', 'ai_request', 'ai_task']) {
      expect(pages).toContain(p)
    }
    for (const p of ['runners', 'runner', 'runner_new', 'runner_cache', 'ai_runners', 'ai_runner', 'settings', 'settings_scope', 'login', 'error']) {
      expect(pages).toContain(p)
    }
  })

  it('formErrorStrings、chineseInterface 找出写成字符串的界面文字', () => {
    expect(formErrorStrings({ Form: { Errors: { a: '中文', b: { id: 'x' } } }, List: [{ EnvForm: { Errors: { c: 'raw' } } }] })).toEqual(['中文', 'raw'])
    expect(chineseInterface({ Title: '运行列表', Flash: '已保存', Data: { Title: '原样' } })).toEqual(['Title: 运行列表', 'Flash: 已保存'])
    expect(chineseInterface({ Title: 'acme/release', Flash: { id: 'account.unbound' } })).toEqual([])
  })

  it.each(cases)('$name：标题、提示和表单的字段错误都是消息，不是写死的中文', ({ json }) => {
    expect(chineseInterface(json)).toEqual([])
    expect(formErrorStrings(envelope(json).Data)).toEqual([])
  })

  it.each(cases)('$name：英文界面没有残留的中文', ({ json }) => {
    const env = envelope(json)
    const { container } = renderPage(env, '/', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1 })).not.toHaveTextContent(han)
    expect(chineseLeft(container, rawStrings(env.Data))).toEqual([])
    expect(document.title).not.toMatch(han)
    expect(document.documentElement.lang).toBe('en')
  })

  it.each(cases)('$name：中文界面切换到英文后不残留中文，再切回中文与切换前相同', async ({ json }) => {
    const env = envelope(json)
    const { container } = renderPage(env, '/')
    const zhHeading = screen.getByRole('heading', { level: 1 }).textContent
    const zhTitle = document.title
    await toLocale('English', env.Session !== null)
    const enHeading = screen.getByRole('heading', { level: 1 }).textContent
    // 标题含界面文字时随语言变化；作用域页、job 页的标题是对象名（例如 acme/release、build），原样显示
    if (han.test(zhHeading)) expect(enHeading).not.toBe(zhHeading)
    expect(chineseLeft(container, rawStrings(env.Data))).toEqual([])
    expect(document.title).not.toMatch(han)
    await toLocale('简体中文', env.Session !== null)
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(zhHeading)
    expect(document.title).toBe(zhTitle)
  })
})

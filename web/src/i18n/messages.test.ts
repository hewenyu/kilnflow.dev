import { describe, expect, it } from 'vitest'
import { matchLocale } from './locale-id'
import { dictionary, isMsgKey, locales, missingParam, msg, parseText, placeholders, segments, translate } from './messages'

// 词典与消息契约（docs/I18N.md）：两种语言的键、参数一致；未知的消息、缺参、类型不对、格式不对都有可读的兜底；
// 参数永远是普通文字，不会被当作标签。

describe('词典完整性', () => {
  const [base, ...others] = locales
  const baseDict = dictionary(base)

  it.each(others)('%s 与 zh-CN 的键集合相同', (l) => {
    expect(Object.keys(dictionary(l)).sort()).toEqual(Object.keys(baseDict).sort())
  })

  it.each(locales)('%s 中每条文案的参数名和格式与 zh-CN 相同，复数文案都有 other', (l) => {
    const d = dictionary(l)
    for (const [key, entry] of Object.entries(baseDict)) {
      const other = d[key]
      expect(other, key).toBeDefined()
      if (other === undefined || entry === undefined) continue
      if (typeof other !== 'string') expect(other.other, key).toBeTruthy()
      expect([...placeholders(other)].sort(), key).toEqual([...placeholders(entry)].sort())
    }
  })

  it.each(locales)('%s 中富文本标签成对出现', (l) => {
    for (const [key, entry] of Object.entries(dictionary(l))) {
      const forms = typeof entry === 'string' ? [entry] : Object.values(entry ?? {}).filter((v) => typeof v === 'string')
      for (const f of forms) {
        const opens = [...f.matchAll(/<([a-z]+)>/g)].map((m) => m[1])
        const closes = [...f.matchAll(/<\/([a-z]+)>/g)].map((m) => m[1])
        expect(closes, key).toEqual(opens)
      }
    }
  })
})

describe('translate', () => {
  it('按语言取文案并代入参数', () => {
    expect(translate('zh-CN', msg('shell.userMenu', { login: 'bob', role: '成员' }))).toBe('bob（成员）的菜单')
    expect(translate('en', msg('shell.userMenu', { login: 'bob', role: 'Member' }))).toBe('Menu for bob (Member)')
  })

  it('原样的文字不翻译', () => {
    expect(translate('en', '已请求取消')).toBe('已请求取消')
  })

  it.each([
    [0, '0 seconds ago', '0 秒前'],
    [1, '1 second ago', '1 秒前'],
    [2, '2 seconds ago', '2 秒前'],
  ])('复数：count=%i', (count, en, zh) => {
    expect(translate('en', msg('time.secondsAgo', { count }))).toBe(en)
    expect(translate('zh-CN', msg('time.secondsAgo', { count }))).toBe(zh)
  })

  it('数字按语言格式化', () => {
    expect(translate('en', msg('time.hoursAgo', { count: 1234 }))).toBe('1,234 hours ago')
  })

  it('不认识的消息显示兜底说明（带消息键），不白屏', () => {
    const t = translate('en', { id: 'nope.missing' })
    expect(t).toContain('nope.missing')
    expect(t).toContain("can't be displayed")
    expect(translate('zh-CN', { id: 'nope.missing' })).toContain('无法显示')
  })

  it('缺少参数、参数类型不对时插入占位符', () => {
    expect(translate('zh-CN', { id: 'shell.userMenu', params: { login: 'bob' } })).toBe(`bob（${missingParam}）的菜单`)
    expect(translate('en', { id: 'time.secondsAgo', params: { count: 'many' } })).toBe(`${missingParam} seconds ago`)
    expect(translate('en', { id: 'time.secondsAgo' })).toBe(`${missingParam} seconds ago`)
  })

  it('纯文本中去掉富文本标签，只保留文字', () => {
    expect(translate('en', msg('shell.expired'))).toBe('Your session has expired and auto-refresh has stopped. Sign in again')
  })
})

describe('富文本', () => {
  // 先按词典切分标签、后代入参数：参数中的标签（包括闭合词典外层标签、再新开标签的写法）
  // 只能出现在它所在段的文字中，不能改变段的结构。
  it.each([
    '<link>evil</link><img src=x onerror=alert(1)>',
    '</strong><link>evil</link><strong>',
    '</strong>evil<strong>',
    '<strong>evil',
  ])('参数中的标签是普通文字，不会被当作元素：%s', (login) => {
    expect(segments('zh-CN', msg('login.signedIn', { login, role: '成员' }))).toEqual([
      { tag: null, text: '当前已登录为 ' },
      { tag: 'strong', text: login },
      { tag: null, text: '（成员）。可以直接' },
      { tag: 'link', text: '继续' },
      { tag: null, text: '，或在下面换一个身份登录。' },
    ])
  })

  it('参数在英文文案中同样不会关闭词典标签', () => {
    const login = '</strong><link>evil</link><strong>'
    expect(segments('en', msg('login.signedIn', { login, role: 'member' }))).toEqual([
      { tag: null, text: "You're signed in as " },
      { tag: 'strong', text: login },
      { tag: null, text: ' (member). You can ' },
      { tag: 'link', text: 'continue' },
      { tag: null, text: ', or sign in as someone else below.' },
    ])
  })
})

describe('parseText', () => {
  it('字符串原样保留，合格的消息保留参数', () => {
    expect(parseText('已保存')).toBe('已保存')
    expect(parseText({ id: 'account.bound', params: { login: 'octocat' } })).toEqual({ id: 'account.bound', params: { login: 'octocat' } })
    expect(parseText({ id: 'account.unbound' })).toEqual({ id: 'account.unbound' })
  })

  it.each([null, 42, [], { id: 7 }, { id: 'no dots' }, { id: 'x'.repeat(200) + '.y' }, { params: {} }])('格式不对的消息转换为兜底说明：%j', (v) => {
    expect(parseText(v)).toEqual({ id: 'message.invalid' })
  })

  it('不合格的参数被丢掉（显示时按缺参处理）', () => {
    expect(parseText({ id: 'shell.userMenu', params: { login: 'bob', role: { x: 1 }, n: Number.NaN } })).toEqual({
      id: 'shell.userMenu',
      params: { login: 'bob' },
    })
  })
})

describe('语言', () => {
  it.each([
    [['en-US', 'zh-CN'], 'en'],
    [['zh-TW'], 'zh-CN'],
    [['fr-FR', 'en-GB'], 'en'],
    [['fr-FR'], null],
    [[], null],
  ])('浏览器语言 %j → %s', (langs, want) => {
    expect(matchLocale(langs)).toBe(want)
  })

  it('isMsgKey 只认词典中的键', () => {
    expect(isMsgKey('runState.queued')).toBe(true)
    expect(isMsgKey('runState.bogus')).toBe(false)
    expect(isMsgKey('toString')).toBe(false)
  })
})

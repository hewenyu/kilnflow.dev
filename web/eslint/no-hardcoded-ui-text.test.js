import path from 'node:path'
import { ESLint, Linter } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, expect, it } from 'vitest'
import kiln, { uiTextKind } from './no-hardcoded-ui-text.js'

// kiln/no-hardcoded-ui-text 的单元测试：写死的中文、英文界面文字都报错；注释、词典调用、符号、白名单的术语和技术标识不误报。

const linter = new Linter({ configType: 'flat' })
const config = [
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: { parser: tseslint.parser, parserOptions: { ecmaFeatures: { jsx: true } } },
    plugins: { kiln },
    rules: { 'kiln/no-hardcoded-ui-text': 'error' },
  },
]

/** lint 返回 code（一个 .tsx 文件）中这条规则的报错：[messageId, 报错的文字]。 */
function lint(code) {
  return linter.verify(code, config, { filename: 'src/pages/x.tsx' }).map((m) => {
    if (!m.ruleId) throw new Error(`解析失败：${m.message}`)
    return [m.messageId, m.message.match(/「(.*)」/)?.[1]]
  })
}

describe('uiTextKind', () => {
  it.each([
    ['加载中', 'zh'],
    ['，', 'zh'],
    ['Loading…', 'en'],
    ['Delete item', 'en'],
    ['on: push and more', 'en'],
    ["Don't", 'en'],
    ['Re-run', 'en'],
    ['Docker Hub settings', 'en'],
  ])('%s 是写死的界面文字（%s）', (s, kind) => {
    expect(uiTextKind(s)).toBe(kind)
  })

  it.each([
    '',
    '   ',
    '·',
    ' — ',
    '/',
    ': ',
    '#',
    '→',
    '…',
    '× 100%',
    '()[]<>',
    '42',
    'r',
    'v1',
    'Kiln',
    'Docker Hub · ',
    'PR #',
    'PR review',
    '· head',
    '· epoch',
    'GitHub App',
    'admin token',
    'kiln-orca',
    'ci.yml',
    'rerun=0',
    'docker.io/',
    'KILN_ADMINS',
    'dryRun',
    'on:',
  ])('%j 不是界面文字', (s) => {
    expect(uiTextKind(s)).toBeNull()
  })

  it('代码元素中只检查中文', () => {
    expect(uiTextKind('kiln doc bind acme/kiln --name docs', true)).toBeNull()
    expect(uiTextKind('名称', true)).toBe('zh')
  })

  it('白名单按整词匹配，区分大小写', () => {
    expect(uiTextKind('Kilns')).toBe('en')
    expect(uiTextKind('kiln')).toBe('en')
    expect(uiTextKind('tagline')).toBe('en')
  })
})

describe('kiln/no-hardcoded-ui-text 报错', () => {
  it.each([
    ['中文 JSXText', 'const A = () => <p>加载中</p>', [['jsxText', '加载中']]],
    ['英文 JSXText', 'const A = () => <button>Delete item</button>', [['jsxText', 'Delete item']]],
    ['夹在符号中的英文', 'const A = ({ n }: { n: number }) => <p>{n} · items</p>', [['jsxText', '· items']]],
    ['中文 aria-label', 'const A = () => <button aria-label="删除" />', [['attribute', '删除']]],
    ['英文 aria-label', 'const A = () => <button aria-label="Delete" />', [['attribute', 'Delete']]],
    ['英文 title', 'const A = () => <span title="Delete item" />', [['attribute', 'Delete item']]],
    ['中文 placeholder', 'const A = () => <input placeholder="搜索" />', [['attribute', '搜索']]],
    ['英文 placeholder', "const A = () => <input placeholder={'Search logs'} />", [['attribute', 'Search logs']]],
    ['英文 data-label', 'const A = () => <td data-label="Status" />', [['attribute', 'Status']]],
    ['中文 alt', 'const A = () => <img alt="头像" />', [['attribute', '头像']]],
    ['组件的文案属性', 'const A = () => <ConfirmForm confirmLabel="Remove" cancelLabel="取消" />', [['attribute', 'Remove'], ['attribute', '取消']]],
    ['含中文的模板字面量属性', 'const A = ({ n }: { n: number }) => <span title={`第 ${n} 次`} />', [['attribute', '第 0 次']]],
    ['含英文的模板字面量属性', 'const A = ({ n }: { n: number }) => <span title={`Attempt ${n} of many`} />', [['attribute', 'Attempt 0 of many']]],
    ['属性中的三元表达式', 'const A = ({ ok }: { ok: boolean }) => <span title={ok ? t("a.b") : "Failed"} />', [['attribute', 'Failed']]],
    ['表达式中的中文字面量', "const A = () => <p>{'加载中'}</p>", [['expression', '加载中']]],
    ['表达式中的英文模板字面量', 'const A = () => <p>{`Loading...`}</p>', [['expression', 'Loading...']]],
    ['三元表达式的两个分支', "const A = ({ ok }: { ok: boolean }) => <p>{ok ? 'Yes' : 'No'}</p>", [['expression', 'Yes'], ['expression', 'No']]],
    ['&& 的右边', "const A = ({ ok }: { ok: boolean }) => <p>{ok && 'Saved'}</p>", [['expression', 'Saved']]],
    ['?? 的默认值', "const A = ({ s }: { s?: string }) => <p>{s ?? 'unknown'}</p>", [['expression', 'unknown']]],
    ['字符串拼接', "const A = ({ s }: { s: string }) => <p>{'Hello ' + s}</p>", [['expression', 'Hello']]],
    ['模板插值中三元表达式的英文分支', "const A = ({ ok }: { ok: boolean }) => <p>{` · ${ok ? 'Active' : 'Idle'}`}</p>", [['expression', 'Active'], ['expression', 'Idle']]],
    ['模板插值中三元表达式的中文分支', "const A = ({ ok }: { ok: boolean }) => <p>{` · ${ok ? '启用' : '停用'}`}</p>", [['expression', '启用'], ['expression', '停用']]],
    ['模板插值中 && 的右边', "const A = ({ a }: { a: boolean }) => <p>{`${a && 'Done'}`}</p>", [['expression', 'Done']]],
    ['模板插值中嵌套的模板字面量', "const A = ({ n }: { n: number }) => <p>{`#${n} ${n > 1 ? `${n} 次` : 'once'}`}</p>", [['expression', '0 次'], ['expression', 'once']]],
    ['属性的模板插值中的英文', "const A = ({ ok }: { ok: boolean }) => <span title={`${ok ? 'Active' : t('x.idle')}`} />", [['attribute', 'Active']]],
    ['属性的模板插值中的中文', "const A = ({ ok }: { ok: boolean }) => <span aria-label={`${ok ? '启用' : t('x.idle')}`} />", [['attribute', '启用']]],
    ['Dialog 的 subtitle', 'const A = () => <Dialog title={t("x.t")} subtitle="Latest revision" />', [['attribute', 'Latest revision']]],
    ['Ago 的 fallback', 'const A = ({ at }: { at: number }) => <Ago t={at} fallback="从未" />', [['attribute', '从未']]],
    ['DesignBasis 的 integrityHint', 'const A = () => <DesignBasis integrityHint="Snapshot was frozen" />', [['attribute', 'Snapshot was frozen']]],
    ['ConfirmationDialog 的按钮文字', 'const A = () => <ConfirmationDialog confirmButtonContent="Remove" cancelButtonContent="取消" />', [['attribute', 'Remove'], ['attribute', '取消']]],
    ['代码元素中的中文', 'const A = () => <code>名称</code>', [['jsxText', '名称']]],
    ['变量中的中文', "const label = '已过期'", [['cjkLiteral', '已过期']]],
    ['用译文做判断', "const bad = (s: string) => s.includes('失败')", [['cjkLiteral', '失败']]],
    ['对象中的中文', "const labels = { failed: `失败 ${1}` }", [['cjkLiteral', '失败 0']]],
    ['非文案属性中的中文', 'const A = () => <div className="中文" />', [['cjkLiteral', '中文']]],
  ])('%s', (_name, code, want) => {
    expect(lint(code)).toEqual(want)
  })
})

describe('kiln/no-hardcoded-ui-text 不误报', () => {
  it.each([
    ['中文注释', '// 加载中的提示\n/* 多行\n   注释 */\nconst A = () => <p>{/* 中文 JSX 注释 */}</p>'],
    ['词典', "const A = () => <p title={t('x.title')} aria-label={t('x.label', { n: 1 })}>{t('x.body')}</p>"],
    ['<Msg> 和 <Trans>', "const A = ({ text }: { text: Text }) => <><Msg text={text} /><Trans id=\"shell.expired\" tags={{ link: (c) => <a href=\"/login\">{c}</a> }} /></>"],
    ['符号和分隔符', "const A = ({ a, b }: { a: string; b: string }) => <p>{a} · {b} — # / : , → … × % () [] {' '}{'—'}{`${a} / ${b}`}</p>"],
    ['品牌和术语', 'const A = ({ id }: { id: number }) => <p>Kiln · GitHub · Docker Hub · npm · PR #{id} · epoch {id} · kiln-orca<span title="Runner" data-label="commit" /></p>'],
    ['固定前缀和编号', 'const A = ({ r, id }: { r: number; id: number }) => <p>r{r} #{id} v1 {`#${id}`} {`rerun=${r}`}</p>'],
    ['技术属性', 'const A = () => <a className="mono num" href="/runs?state=failed" id="main" type="button" target="_blank" rel="noreferrer" role="status" data-color-mode="dark"><Button variant="primary" size="small" /></a>'],
    ['技术属性中的模板字面量', 'const A = ({ id }: { id: number }) => <a href={`/runs/${id}/cancel`} className={`${id} active`} />'],
    ['状态值的比较和路由', "const f = (s: string) => (s === 'failed' ? '/runs?state=failed' : s.startsWith('refs/heads/') ? 'branch' : 'unknown')"],
    ['代码元素中的命令', 'const A = ({ repo }: { repo: string }) => <><pre>{`kiln doc bind ${repo} --name docs`}</pre><code>npm install -g kiln</code><kbd>Enter key</kbd></>'],
    ['编程错误的说明', "function f() { throw new Error('useRouter 必须在 RouterProvider 内使用') }\nconst e = new TypeError(`参数 ${1} 不对`)"],
    ['类型中的英文', "type Kind = 'Delete item' | 'Loading'\nconst k: Kind = 'Loading'"],
    ['&& 左边的条件', "const A = ({ s }: { s: string }) => <p>{s === 'failed' && t('x.failed')}</p>"],
    ['模板插值中的白名单、技术标识和词典', "const A = ({ ok, id }: { ok: boolean; id: number }) => <p title={`${ok ? 'Runner' : 'v1'} ${t('x.n', { id })}`}>{` · ${ok ? 'GitHub' : 'ci.yml'}`}{`${ok && t('x.done')} #${id}`}{`${ok ? t('x.a') : t('x.b')}`}</p>"],
    ['模板插值中比较的状态值', "const A = ({ s }: { s: string }) => <p>{`${s === 'failed' ? t('x.f') : t('x.ok')}`}</p>"],
    ['符号 fallback 和非文案属性的模板插值', "const A = ({ at, ok }: { at: number; ok: boolean }) => <><Ago t={at} fallback=\"—\" /><a className={`row ${ok ? 'active' : 'idle'}`} /></>"],
    ['新增文案属性中的词典', "const A = () => <><Dialog subtitle={t('x.s')} /><Ago fallback={t('x.never')} /><DesignBasis integrityHint={t('x.h')} /><ConfirmationDialog confirmButtonContent={t('x.ok')} cancelButtonContent={t('x.cancel')} /></>"],
  ])('%s', (_name, code) => {
    expect(lint(code)).toEqual([])
  })
})

describe('eslint.config.js 中的范围', () => {
  const eslint = new ESLint({ cwd: path.resolve(import.meta.dirname, '..') })
  const rule = async (file) => (await eslint.calculateConfigForFile(file)).rules?.['kiln/no-hardcoded-ui-text']

  it.each(['src/pages/run.tsx', 'src/components/Form.tsx', 'src/lib/format.ts', 'src/app/Shell.tsx'])('%s：检查', async (file) => {
    expect(await rule(file)).toEqual([2])
  })

  it.each(['src/i18n/messages.ts', 'src/i18n/locale.tsx', 'src/test/render.tsx', 'src/pages/run.test.tsx', 'src/lib/format.test.ts', 'dev/preview.ts'])(
    '%s：不检查',
    async (file) => {
      expect(await rule(file)).toBeUndefined()
    },
  )
})

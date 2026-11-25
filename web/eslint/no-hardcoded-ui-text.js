// kiln/no-hardcoded-ui-text：业务源码中不写死界面文字，界面文字都来自词典（t、<Msg>、<Trans>，见 docs/I18N.md）。
//
// 按语法树检查，不按正则扫文件：注释、类型、CSS 类名、路由路径、状态值的比较都不是检查对象，天然不会误报。检查的是：
//   1. JSX 的文字（JSXText）：含中文，或含不在白名单中的英文单词；
//   2. 用户可见和读屏软件读出的属性（TEXT_ATTRS）的字符串或模板字面量；
//   3. JSX 子节点中的表达式（{'…'}、{`…`}、{ok ? '…' : '…'}、{x && '…'}、字符串拼接、模板字面量的 ${…}）里的字面量；
//   4. 其余位置（普通变量、对象、比较）含中文的字符串和模板字面量：中文只能在词典中，
//      也不能用译文做判断（.includes('失败')）。例外是 Error 的说明（编程错误，开发者看，不显示在界面上）。
//
// 英文的判断按"词"：去掉白名单中的词组后，按空白切分，每段去掉首尾的标点；剩下的只由字母组成（可含 ' 和 -）、
// 不少于两个字母、不是驼峰写法的，就是界面文字。带数字、点、斜杠、下划线、等号、@ 等的（ci.yml、v1、rerun=2、
// KILN_ADMINS）是技术标识，单个字母（r{revision}）是固定前缀，都不算。
// <code>、<pre>、<kbd>、<samp> 中是原样的命令和代码，不检查英文（仍检查中文）。
//
// 白名单（ALLOWED）只放两种语言中都按原样显示的品牌、产品名、Actions / Git 术语和配置字段名，越窄越好：
// 真正的界面文字一律迁进词典，不要靠扩大白名单绕过。

/** 用户可见或读屏软件读出的属性：HTML / ARIA 属性和项目组件的文案属性（Field、ConfirmForm、BoxHeader、KV 等）。 */
const TEXT_ATTRS = new Set([
  'aria-label',
  'aria-description',
  'aria-placeholder',
  'aria-roledescription',
  'aria-valuetext',
  'title',
  'placeholder',
  'alt',
  'data-label',
  'label',
  'caption',
  'description',
  'subtitle',
  'heading',
  'button',
  'confirmLabel',
  'cancelLabel',
  'confirmTitle',
  'confirmBody',
  'confirmButtonContent',
  'cancelButtonContent',
  'integrityHint',
  'fallback',
  'emptyText',
  'empty',
  'note',
  'text',
  'k',
])

/** 其中的文字是原样的命令、代码，不检查英文。 */
const CODE_TAGS = new Set(['code', 'pre', 'kbd', 'samp'])

/** 两种语言中都原样显示的词和词组（区分大小写）。 */
export const ALLOWED = [
  // 品牌和产品
  'Kiln',
  'GitHub',
  'GitHub App',
  'Release',
  'Docker',
  'Docker Hub',
  'npm',
  'Orca',
  'Outline',
  'OAuth',
  'kiln-orca',
  'kiln-runner',
  'kiln-server',
  // 概念和术语（与 GitHub Actions、Git 一致）
  'AI',
  'CI',
  'PR',
  'PR review',
  'Issue',
  'Issues',
  'Agent',
  'Runner',
  'job',
  'run',
  'workflow',
  'commit',
  'ref',
  'head',
  'tag',
  'dist-tag',
  'SHA',
  'ID',
  'URL',
  'HTTP',
  'HTTPS',
  'JSON',
  'YAML',
  'epoch',
  'attempt',
  'checkpoint',
  'admin token',
  // workflow 和设置中的字段名
  'on:',
  'executor',
  'pool',
  'label',
  'registry',
  'scope',
  'token',
]

/** 中文：CJK 标点、汉字（含扩展 A、兼容汉字）和全角字符。 */
const CJK = /[\u3000-\u303f\u3400-\u9fff\uf900-\ufaff\uff00-\uffef]/

const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** 白名单的词组（长的在前，先去掉 Docker Hub 再去掉 Docker），前后不能紧挨字母、数字、- 或 _。 */
const allowedRe = new RegExp(
  [...ALLOWED]
    .sort((a, b) => b.length - a.length)
    .map((w) => `(?<![A-Za-z0-9_-])${escape(w)}(?![A-Za-z0-9_-])`)
    .join('|'),
  'g',
)

/** isWord：s（已去掉首尾标点）是普通的英文单词，不是技术标识。 */
function isWord(s) {
  return /^[A-Za-z][A-Za-z'’-]*[A-Za-z]$/.test(s) && !/[a-z][A-Z]/.test(s)
}

/**
 * uiTextKind 返回 s 是哪种写死的界面文字：'zh'（含中文）、'en'（含不在白名单中的英文单词）或 null。
 * code 为 true 时在代码元素中，只检查中文。
 */
export function uiTextKind(s, code = false) {
  if (CJK.test(s)) return 'zh'
  if (code) return null
  const rest = s.replace(allowedRe, ' ')
  for (const chunk of rest.split(/\s+/)) {
    const core = chunk.replace(/^[^A-Za-z0-9]+|[^A-Za-z0-9]+$/g, '')
    if (core && isWord(core)) return 'en'
  }
  return null
}

/**
 * templateText 返回模板字面量中写死的部分，${…} 处换成 0：插值与前后的文字连在一起（rerun=${n}、#${id}）时按技术标识处理，
 * 与前后隔着空格时（${n} items）不影响单词的判断。
 */
function templateText(node) {
  return node.quasis.map((q) => q.value.cooked ?? q.value.raw).join('0')
}

/**
 * literalLeaves 返回表达式中会作为文字显示的字面量（条件表达式的两个分支、&& || ?? 的两边、字符串拼接、模板字面量本身和 ${…} 中的这些字面量）；
 * 比较、函数调用等其余运算不算。
 */
function literalLeaves(node, out = []) {
  switch (node.type) {
    case 'Literal':
      if (typeof node.value === 'string') out.push(node)
      break
    case 'TemplateLiteral':
      out.push(node)
      for (const expr of node.expressions) literalLeaves(expr, out)
      break
    case 'ConditionalExpression':
      literalLeaves(node.consequent, out)
      literalLeaves(node.alternate, out)
      break
    case 'LogicalExpression':
      literalLeaves(node.left, out)
      literalLeaves(node.right, out)
      break
    case 'BinaryExpression':
      if (node.operator === '+') {
        literalLeaves(node.left, out)
        literalLeaves(node.right, out)
      }
      break
    case 'TSAsExpression':
    case 'TSSatisfiesExpression':
    case 'TSNonNullExpression':
      literalLeaves(node.expression, out)
      break
  }
  return out
}

function literalText(node) {
  return node.type === 'TemplateLiteral' ? templateText(node) : node.value
}

function elementName(name) {
  if (name.type === 'JSXIdentifier') return name.name
  if (name.type === 'JSXNamespacedName') return `${name.namespace.name}:${name.name.name}`
  return ''
}

/** inCode：node 在 <code>、<pre>、<kbd>、<samp> 之中。 */
function inCode(node) {
  for (let p = node.parent; p; p = p.parent) {
    if (p.type === 'JSXElement' && CODE_TAGS.has(elementName(p.openingElement.name))) return true
  }
  return false
}

/** isErrorMessage：node 是 new Error('…') / Error('…') 等的参数（编程错误的说明）。 */
function isErrorMessage(node) {
  const p = node.parent
  if (!p || (p.type !== 'NewExpression' && p.type !== 'CallExpression') || !p.arguments.includes(node)) return false
  return p.callee.type === 'Identifier' && /Error$/.test(p.callee.name)
}

const preview = (s) => {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > 40 ? `${t.slice(0, 40)}…` : t
}

/** @type {import('eslint').Rule.RuleModule} */
export const noHardcodedUiText = {
  meta: {
    type: 'problem',
    docs: { description: '界面文字都来自词典（t、<Msg>、<Trans>），不在业务源码中写死中文或英文' },
    schema: [],
    messages: {
      jsxText: 'JSX 中写死了界面文字「{{text}}」：改用 t(…) / <Trans>，在 zh-CN.json 和 en.json 中加文案（docs/I18N.md）',
      attribute: '属性 {{name}} 写死了界面文字「{{text}}」：改用 t(…)，在两种语言的词典中加文案（docs/I18N.md）',
      expression: 'JSX 表达式中写死了界面文字「{{text}}」：改用 t(…)，在两种语言的词典中加文案（docs/I18N.md）',
      cjkLiteral: '字符串中有中文「{{text}}」：界面文字放进词典，不要用译文做判断；编程错误的说明写在 Error 中（docs/I18N.md）',
    },
  },
  create(context) {
    /** 已经按 JSX 的位置报过的字面量，不再按"含中文的字符串"重复报。 */
    const done = new WeakSet()

    const checkLeaves = (expr, messageId, data) => {
      for (const leaf of literalLeaves(expr)) {
        done.add(leaf)
        const text = literalText(leaf)
        if (uiTextKind(text, inCode(leaf))) context.report({ node: leaf, messageId, data: { ...data, text: preview(text) } })
      }
    }

    const checkCJK = (node, text) => {
      if (done.has(node) || isErrorMessage(node) || !CJK.test(text)) return
      context.report({ node, messageId: 'cjkLiteral', data: { text: preview(text) } })
    }

    return {
      JSXText(node) {
        if (uiTextKind(node.value, inCode(node))) context.report({ node, messageId: 'jsxText', data: { text: preview(node.value) } })
      },
      JSXAttribute(node) {
        const name = elementName(node.name)
        if (!TEXT_ATTRS.has(name) || !node.value) return
        const v = node.value
        if (v.type === 'Literal') {
          checkLeaves(v, 'attribute', { name })
        } else if (v.type === 'JSXExpressionContainer' && v.expression.type !== 'JSXEmptyExpression') {
          checkLeaves(v.expression, 'attribute', { name })
        }
      },
      JSXExpressionContainer(node) {
        if (node.parent.type === 'JSXAttribute' || node.expression.type === 'JSXEmptyExpression') return
        checkLeaves(node.expression, 'expression', {})
      },
      Literal(node) {
        if (typeof node.value === 'string') checkCJK(node, node.value)
      },
      TemplateLiteral(node) {
        checkCJK(node, templateText(node))
      },
    }
  },
}

export default {
  meta: { name: 'kiln' },
  rules: { 'no-hardcoded-ui-text': noHardcodedUiText },
}

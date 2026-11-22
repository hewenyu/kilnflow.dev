import { maxChars, stripAnsi, type Match } from './log'

// 日志的 DOM：每行一个元素（行号由 CSS 计数器显示，复制时不带行号），直接操作 DOM 而不经过 React，
// 几万行的日志追加时也不必重新渲染。超过 maxChars 个字符时丢弃最早的行，行号从丢弃的行数之后接着数。

export const lineClass = 'kiln-log-line'

export class LogBuffer {
  private pending = ''
  private texts: string[] = []
  private nodes: Text[] = []
  /** 最后一行还没有换行符，下一段文字接在它后面。 */
  private open = false
  private chars = 0
  private dropped = 0

  constructor(
    private readonly root: HTMLElement,
    private readonly limit = maxChars,
  ) {}

  /** 页面上保留的字符数。 */
  get shown(): number {
    return this.chars
  }

  /** 保留的行（不含换行符），用于搜索。 */
  get lines(): readonly string[] {
    return this.texts
  }

  /** 日志的总行数（含已丢弃的行；最后一行没有换行符也算一行）。 */
  get lineCount(): number {
    return this.dropped + this.texts.length
  }

  /** 是否丢弃过最早的部分。 */
  get truncated(): boolean {
    return this.dropped > 0
  }

  /** append 追加一段文字（去掉 ANSI 转义）；返回是否追加了内容。之后要调用 trim 丢弃超出上限的最早的行。 */
  append(chunk: string): boolean {
    const r = stripAnsi(this.pending, chunk)
    this.pending = r.pending
    const text = r.text
    if (!text) return false
    const segs = text.split('\n')
    const doc = this.root.ownerDocument
    const frag = doc.createDocumentFragment()
    segs.forEach((seg, i) => {
      if (i === 0 && this.open) {
        const last = this.texts.length - 1
        this.texts[last] = (this.texts[last] ?? '') + seg
        this.nodes[last]?.appendData(seg)
        return
      }
      if (i === segs.length - 1 && seg === '') return
      const line = doc.createElement('div')
      line.className = lineClass
      const node = doc.createTextNode(seg)
      line.append(node)
      frag.append(line)
      this.texts.push(seg)
      this.nodes.push(node)
    })
    this.root.append(frag)
    this.open = !text.endsWith('\n')
    this.chars += text.length
    return true
  }

  /**
   * trim 在超过上限时丢弃最早的行，返回丢弃的行数。与 append 分开，调用方可以在两步之间量出追加后的高度，
   * 从而只按丢弃部分的高度修正滚动位置。
   */
  trim(): number {
    let n = 0
    while (this.chars > this.limit && this.texts.length - n > 1) {
      this.chars -= (this.texts[n] ?? '').length + 1
      n++
    }
    if (n === 0) return 0
    for (let i = 0; i < n; i++) this.root.firstChild?.remove()
    this.texts.splice(0, n)
    this.nodes.splice(0, n)
    this.dropped += n
    this.root.style.counterReset = `kiln-line ${this.dropped}`
    return n
  }

  /** clear 清空（切换到新的尝试）。 */
  clear(): void {
    this.root.replaceChildren()
    this.root.style.counterReset = ''
    this.pending = ''
    this.texts = []
    this.nodes = []
    this.open = false
    this.chars = 0
    this.dropped = 0
  }

  /** range 返回一处搜索结果在 DOM 中的范围。 */
  range(m: Match): Range | null {
    const node = this.nodes[m.line]
    if (!node) return null
    const r = this.root.ownerDocument.createRange()
    r.setStart(node, m.start)
    r.setEnd(node, m.start + m.length)
    return r
  }
}

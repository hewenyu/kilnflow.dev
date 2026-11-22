package decisioninbox

import (
	"fmt"
	"slices"
	"strings"
)

// 「待拍板」清单的 Markdown 生成与精确查证。
//
// 这里只做纯文本处理：把一个条目渲染成 Markdown 块、插到顶层 `## 待拍板` 标题的正下方（最新在上），以及在一篇文档里
// 精确查找条目的 marker。所有 Agent 提供的内容都不可信：放进引用块、转义反引号，并打断 `kiln-dec:` 字样，避免破坏文档
// 结构或伪造别的条目的 marker。

// 条目 marker 的前缀与格式：`kiln-dec:<repo_id>:wi<work_item_id>:t<task_id>:<entry_idx>`。
// 数据库约束要求 marker 必须由这些列拼出来。
const markerPrefix = "kiln-dec:"

// anchorTitle 是「待拍板」清单的顶层标题文本。
const anchorTitle = "待拍板"

// EntryKind 是同步条目的类型：一条决策记录，或一条受阻摘要（每个交付报告至多一条）。
type EntryKind string

const (
	EntryDecision EntryKind = "decision"
	EntryBlocked  EntryKind = "blocked"
)

// Marker 返回条目在文档里的唯一标识。
func Marker(repoID, workItemID, taskID int64, entryIdx int) string {
	return fmt.Sprintf("%s%d:wi%d:t%d:%d", markerPrefix, repoID, workItemID, taskID, entryIdx)
}

// UTF16Units 按 JavaScript 的 UTF-16 code unit 计算字符串长度：BMP 之外的字符（大部分 emoji）计 2。
// Outline 的篇幅限制按这个口径计算。
func UTF16Units(s string) int64 {
	var n int64
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// line 是一行文本及其原始范围：content 不含行尾，eol 是 "\n" 或 "\r\n"（最后一行可能为空）。
type line struct {
	start, end int    // [start, end) 是整行（含行尾）
	content    string // 不含行尾
	eol        string
}

// splitLines 按 \n 切分文本，保留每行的原始范围和行尾风格。
func splitLines(s string) []line {
	var out []line
	start := 0
	for i := range len(s) {
		if s[i] != '\n' {
			continue
		}
		content := s[start:i]
		eol := "\n"
		if c, ok := strings.CutSuffix(content, "\r"); ok {
			content, eol = c, "\r\n"
		}
		out = append(out, line{start: start, end: i + 1, content: content, eol: eol})
		start = i + 1
	}
	if start < len(s) {
		out = append(out, line{start: start, end: len(s), content: s[start:]})
	}
	return out
}

// tabStop 是制表符的列宽（CommonMark 按 4 列对齐）。
const tabStop = 4

// lineCursor 按 CommonMark 的"列"模型扫描一行：off 是下一个未消费的字节下标，col 是下一个未消费字符的显示列。
// Tab 对齐到下一个 tab stop；只消费 Tab 的一部分时，剩下的空白列记在 pending 里，此时 s[off] 位于 col+pending 列。
//
// 列宽和字节下标是两个不同的量：Tab 占 1 个字节但占多列，多字节 UTF-8 字符占多字节但占 1 列。所有切片都用字节下标
// （off），所有缩进 / 容器判定都用显示列，两者绝不混用。
type lineCursor struct {
	s       string
	off     int
	col     int
	pending int
}

// blank 返回从当前位置到行尾是否只有空白。
func (c *lineCursor) blank() bool {
	return strings.TrimLeft(c.s[c.off:], " \t") == ""
}

// rest 返回剥离已消费前缀后的内容；只消费了一部分的 Tab 把剩余列补成空格。
func (c *lineCursor) rest() string {
	if c.pending == 0 {
		return c.s[c.off:]
	}
	return strings.Repeat(" ", c.pending) + c.s[c.off:]
}

// skip 最多消费 width 列行首空白（空格或 Tab，Tab 对齐到下一个 tab stop），返回实际消费的列数。
// 遇到非空白字符或行尾时提前返回；只消费 Tab 的一部分时把剩余列记进 pending，游标不会越过行尾。
func (c *lineCursor) skip(width int) int {
	consumed := 0
	for consumed < width {
		if c.pending > 0 {
			k := min(c.pending, width-consumed)
			c.pending -= k
			c.col += k
			consumed += k
			continue
		}
		if c.off >= len(c.s) {
			break
		}
		switch c.s[c.off] {
		case ' ':
			c.off++
			c.col++
			consumed++
		case '\t':
			tab := tabStop - c.col%tabStop
			c.off++
			if tab > width-consumed {
				k := width - consumed
				c.pending = tab - k
				c.col += k
				consumed += k
			} else {
				c.col += tab
				consumed += tab
			}
		default:
			return consumed
		}
	}
	return consumed
}

// skipAll 消费从当前位置到第一个非空白字符之间的全部空白，返回消费的列数。
func (c *lineCursor) skipAll() int {
	// 每个字节最多占 tabStop 列，len(s)+tabStop 是安全的上界。
	return c.skip(len(c.s) + tabStop)
}

// consumeQuote 尝试消费一个引用标记：最多 3 列缩进、`>`、可选的一个空格或 Tab。失败时游标保持不变。
//
// 标记后的 Tab 按 CommonMark #2.2 只消费 1 列空白（等价于一个空格），展开后的其余列留在 pending 里作为内容缩进，
// 不能整块吃掉。
func (c *lineCursor) consumeQuote() bool {
	save := *c
	if c.skip(maxFenceIndent+1) > maxFenceIndent {
		*c = save
		return false
	}
	if c.pending > 0 || c.off >= len(c.s) || c.s[c.off] != '>' {
		*c = save
		return false
	}
	c.off++
	c.col++
	if c.off < len(c.s) {
		switch c.s[c.off] {
		case ' ':
			c.off++
			c.col++
		case '\t':
			c.skip(1)
		}
	}
	return true
}

// consumeList 尝试消费一个列表项标记（最多 3 列缩进），成功时返回内容所在的绝对显示列。失败时游标保持不变。
//
// 内容列按 CommonMark #5.2：标记后 1–4 列空白属于容器 padding，内容紧随其后；5 列以上只消费 1 列，其余留给内容
// （缩进代码）；标记后直接到行尾或只有空白是空列表项，内容列固定为标记后一列，尾随空白不算 padding。
func (c *lineCursor) consumeList() (indent int, ok bool) {
	save := *c
	if c.skip(maxFenceIndent+1) > maxFenceIndent {
		*c = save
		return 0, false
	}
	if c.pending > 0 {
		*c = save
		return 0, false
	}
	marker, ok := listMarker(c.s[c.off:])
	if !ok {
		*c = save
		return 0, false
	}
	c.off += marker
	c.col += marker
	markerEnd := c.col
	if c.blank() {
		c.skipAll()
		return markerEnd + 1, true
	}
	// 探出标记后空白的总列数，再决定消费多少：部分消费 Tab 会留下 pending，游标仍然精确。
	probe := *c
	ws := probe.skipAll()
	if ws > tabStop {
		c.skip(1)
		return markerEnd + 1, true
	}
	c.skip(ws)
	return markerEnd + ws, true
}

// fenceOpen 判断一行是否是顶层围栏代码块的开头（``` 或 ~~~，至少 3 个），返回围栏字符和长度。
func fenceOpen(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	// 反引号围栏的 info string 不能含有反引号；有的话不是围栏。
	if c == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return 0, 0, false
	}
	return c, n, true
}

// maxFenceIndent 是 CommonMark 中围栏的缩进上限：0–3 个前导空格仍属于当前容器，4 个及以上（或 Tab）是缩进代码。
const maxFenceIndent = 3

// containerKind 是块容器类型。
type containerKind int

const (
	// kindQuote 是引用容器（`>`），按行首标记匹配，内容列随行变化。
	kindQuote containerKind = iota
	// kindList 是列表项容器，indent 是它的内容所在的绝对显示列。
	kindList
)

// container 是一个打开中的块容器。indent 只对列表项有意义（内容所在的绝对显示列）；引用按标记匹配，不用它。
type container struct {
	kind   containerKind
	indent int
}

// blockScanner 判断一行是否属于代码块（围栏或缩进代码），并维护容器栈与围栏状态。锚点扫描和 marker 查证共用同一套
// 规则，避免两处对"什么算代码"的判断不一致。
//
// 容器用统一的栈表示（引用与列表混排、任意层数），逐行按 CommonMark #5.2 匹配：引用要求行首有 `>` 标记；列表项要求
// 行的缩进达到它的内容列（空行不结束列表项）。匹配完已有容器后，再从内容位置循环开启本行新的容器，所以 `- - ``` `、
// `- > ``` `、`> - - ``` ` 这种同一行的多层嵌套都能被正确识别，围栏的缩进按最内层容器的内容列判断。
//
// 围栏打开时冻结容器层数（fenceDepth）：围栏内部只匹配前 fenceDepth 个容器，内部以 `>` / `-` 开头的行只是代码，
// 不会再被解析成新的容器；围栏里的空行（包括只有空白的行）保留围栏与容器。任一外层容器断掉就关闭围栏，并把这一行
// 按围栏外重新解析。
//
// 列表容器是栈而不是单层：子列表结束后要能退回父列表继续（`- 项目` → `  - 子项` → 回到 `  正文`），否则子列表的
// 内容列会一直残留，把父列表里缩进更深的行误判成已离开列表。
type blockScanner struct {
	containers []container
	in         bool // 是否在围栏代码块里
	char       byte
	n          int
	// fenceDepth 是围栏打开时的容器层数，围栏内部只匹配这些容器。
	fenceDepth int
}

// match 让行首依次匹配已打开的前 limit 个容器，返回从头开始连续匹配上的数量。引用要求 `>` 标记；列表项要求缩进达到
// 它的内容列，空行不结束列表项（可以跨空行）。
//
// 每次尝试匹配单个容器前都先保存游标：匹配失败时立即回滚，绝不让"消费了一半的缩进"漏给调用方。否则列表匹配失败
// 后残留的缩进会让列表外的缩进代码（例如 `100. item` 之后 4 个空格的行）被当成普通正文。
func (s *blockScanner) match(c *lineCursor, limit int) int {
	for i := range limit {
		saved := *c
		ct := s.containers[i]
		switch ct.kind {
		case kindQuote:
			if !c.consumeQuote() {
				*c = saved
				return i
			}
		case kindList:
			if c.blank() {
				continue
			}
			if c.col < ct.indent {
				need := ct.indent - c.col
				if c.skip(need) < need {
					*c = saved
					return i
				}
			}
		}
	}
	return limit
}

// step 处理一行并返回处理后的块结构状态。blockScanner 是"这一行属于什么块"的唯一事实来源，锚点扫描与 marker
// 查证都只读它的结论。
func (s *blockScanner) step(content string) blockState {
	before := slices.Clone(s.containers)
	var st blockState
	var opened bool
	if s.in {
		var matched int
		var still bool
		st, matched, still = s.matchFence(content)
		if !still {
			// 围栏所属的容器结束：围栏关闭，这一行按围栏外重新判断。
			s.in = false
			s.containers = s.containers[:matched]
			st, opened = s.scanOut(content)
		}
	} else {
		st, opened = s.scanOut(content)
	}
	for _, ct := range s.containers {
		if ct.kind == kindQuote {
			st.HasQuote = true
			break
		}
	}
	st.TopLevel = !st.InCode && len(s.containers) == 0
	// 容器栈变化（包括同层列表项结束又开启一个新的：栈内容可能恰好相同）都意味着新的叶子块，段落要分段。
	st.BlockBreak = opened || !slices.Equal(before, s.containers)
	return st
}

// matchFence 处理围栏内的一行：匹配围栏所属的容器，并顺带处理关闭围栏。still=false 表示外层容器已经结束，
// 调用方应关闭围栏、按围栏外重新解析这一行；此时 matched 是断掉之前匹配上的容器数。
func (s *blockScanner) matchFence(content string) (st blockState, matched int, still bool) {
	c := lineCursor{s: content}
	if matched = s.match(&c, s.fenceDepth); matched < s.fenceDepth {
		return blockState{}, matched, false
	}
	st = blockState{InCode: true}
	if c.blank() {
		// 围栏里的空行（包括只有空白的行）永远是内容，不关闭围栏。
		return st, matched, true
	}
	ws := c.skipAll()
	st.Rest = c.rest()
	if ws <= maxFenceIndent && fenceClose(st.Rest, s.char, s.n) {
		// 关闭围栏的那一行同样是围栏内容，不能同时被当成新的开围栏。
		s.in = false
	}
	return st, matched, true
}

// scanOut 在围栏外解析一行：匹配已有容器，再开启本行新增的容器（同一行可以有多层），最后按内容位置的相对缩进判断
// 缩进代码与围栏。opened 表示这一行开启了新的容器（用于段落分段）。
func (s *blockScanner) scanOut(content string) (st blockState, opened bool) {
	c := lineCursor{s: content}
	s.containers = s.containers[:s.match(&c, len(s.containers))]
	for !c.blank() {
		// 主题分隔线是叶子块（CommonMark #4.1）：`- - -`、`* * *` 不能再被当成列表项开启新容器。
		if thematicAt(&c) {
			break
		}
		if c.consumeQuote() {
			s.containers = append(s.containers, container{kind: kindQuote})
			opened = true
			continue
		}
		if indent, ok := c.consumeList(); ok {
			s.containers = append(s.containers, container{kind: kindList, indent: indent})
			opened = true
			continue
		}
		break
	}
	if c.blank() {
		// 空行不开启代码块；容器（列表项）可以跨空行保留。
		return blockState{Blank: true}, opened
	}
	// 内容位置剩余的缩进：4 列及以上（含 Tab）是缩进代码块，绝不能再被解析成列表项或开围栏。
	ws := c.skipAll()
	st.Rest = c.rest()
	if ws > maxFenceIndent {
		st.InCode = true
		return st, opened
	}
	if ch, n, ok := fenceOpen(st.Rest); ok {
		s.in, s.char, s.n, s.fenceDepth = true, ch, n, len(s.containers)
		st.InCode = true
	}
	return st, opened
}

// fenceClose 判断一行是否关闭围栏（同样的字符，至少与开头一样长，后面只有空白）。
func fenceClose(trimmed string, c byte, n int) bool {
	i := 0
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	return i >= n && strings.TrimSpace(trimmed[i:]) == ""
}

// listMarker 判断一行（去掉缩进后）是否以列表项标记开头：- / * / + 或 1–9 位数字加 . / )。标记后可以是行尾
// （空列表项）或至少一个空白。返回标记的字节长度（标记本身都是 ASCII）；标记后的空白、内容列与空列表项由调用方处理。
func listMarker(trimmed string) (marker int, ok bool) {
	if trimmed == "" {
		return 0, false
	}
	switch trimmed[0] {
	case '-', '*', '+':
		marker = 1
	default:
		i := 0
		for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
			i++
			if i > 9 {
				// CommonMark 的有序列表标记最多 9 位数字；更长的数字串是普通文本。
				return 0, false
			}
		}
		if i == 0 || i >= len(trimmed) || (trimmed[i] != '.' && trimmed[i] != ')') {
			return 0, false
		}
		marker = i + 1
	}
	if marker < len(trimmed) && trimmed[marker] != ' ' && trimmed[marker] != '\t' {
		return 0, false
	}
	return marker, true
}

// isAnchor 判断一行（去掉缩进后）是否恰好是顶层二级标题 `## 待拍板`。
// `#`、`###`、`##待拍板`（没有空格）都不算；允许行尾空白和合法的收尾 #。
func isAnchor(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "##") {
		return false
	}
	rest := trimmed[2:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	rest = strings.TrimRight(rest, " \t")
	// 收尾的 # 只有在前面是空白时才属于标题标记（CommonMark）。
	if hashes := strings.TrimRight(rest, "#"); hashes != rest {
		if hashes == "" || hashes[len(hashes)-1] == ' ' || hashes[len(hashes)-1] == '\t' {
			rest = strings.TrimRight(hashes, " \t")
		}
	}
	return rest == anchorTitle
}

// blockState 是处理完一行之后的块结构状态。
type blockState struct {
	// InCode 表示这一行属于代码块（围栏、关闭围栏的那一行或缩进代码）。
	InCode bool
	// HasQuote 表示处理完这一行后仍处在引用容器里：引用里的 marker 是引用内容，不是条目本身。
	HasQuote bool
	// TopLevel 表示这一行处在顶层：不是代码、不在引用里、也不在列表项里。
	TopLevel bool
	// Blank 表示围栏外、剥离所有容器前缀后是空行（段落边界）。
	Blank bool
	// Rest 是剥离所有容器前缀后的行内容（部分消费的 Tab 把剩余列补成空格）。
	Rest string
	// BlockBreak 表示这一行的容器栈发生了变化（或开启了新的容器），是新的叶子块、段落要分段。
	BlockBreak bool
}

// anchorLines 返回文档里所有顶层 `## 待拍板` 标题所在的行号。是否顶层完全由 blockScanner 判断：跳过围栏代码、
// 缩进代码、引用和列表内的假标题。
func anchorLines(doc string) []int {
	var out []int
	var sc blockScanner
	for i, ln := range splitLines(doc) {
		st := sc.step(ln.content)
		if st.TopLevel && isAnchor(strings.TrimLeft(st.Rest, " \t")) {
			out = append(out, i)
		}
	}
	return out
}

// docLineEnding 返回文档主要使用的换行风格。
func docLineEnding(doc string) string {
	if strings.Contains(doc, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// InsertEntry 把一个条目块插入文档：存在顶层 `## 待拍板` 时插在首个标题行的正下方（最新在上），否则在文末新建
// `## 待拍板` 一节。除插入位置外，原文所有字节与换行风格保持不变。multiple 表示文档里有多个锚点（只用了首个）。
func InsertEntry(doc, block string) (out string, multiple bool) {
	lines := splitLines(doc)
	anchors := anchorLines(doc)
	block = strings.ReplaceAll(strings.ReplaceAll(block, "\r\n", "\n"), "\n\r", "\n")
	if len(anchors) > 0 {
		ln := lines[anchors[0]]
		eol := ln.eol
		head := doc[:ln.end]
		rest := doc[ln.end:]
		if eol == "" {
			// 锚点在最后一行且没有末尾换行：先补一个换行，否则条目会直接粘在标题后面、破坏锚点。
			eol = docLineEnding(doc)
			head = doc + eol
			rest = ""
		}
		normalized := strings.ReplaceAll(block, "\n", eol)
		// 条目自身的结尾换行；原文紧接的内容不是空行时再补一个空行分隔。
		sep := eol
		if rest != "" && !strings.HasPrefix(rest, "\n") && !strings.HasPrefix(rest, "\r\n") {
			sep = eol + eol
		}
		return head + normalized + sep + rest, len(anchors) > 1
	}
	eol := docLineEnding(doc)
	prefix := ""
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		prefix = eol
	}
	normalized := strings.ReplaceAll(block, "\n", eol)
	out = doc + prefix + eol + "## " + anchorTitle + eol + eol + normalized + eol
	return out, false
}

// hasMarker 判断文档正文里是否有完整的行内代码标记 `marker`（单个反引号包裹、未被转义）。
//
// 按 CommonMark 的块结构跳过不是条目正文的内容：代码块（围栏、缩进代码）与引用块。行内代码片段可以跨行（CommonMark
// #6.1）：在同一段连续段落（叶子块）内累积各行内容再整体扫描，所以未闭合的双反引号不会吞掉后续段落的真实 marker；
// 代码片段内部不做反斜杠转义。代码块、引用、空行、容器变化、ATX 标题、Setext 标题下划线与主题分隔线都会结束段落；
// ATX 标题作为独立的单行叶子块单独扫描，不与前后行合并。
//
// marker 按源码精确匹配：只认单个反引号包裹、内容与目标逐字节相等（不做 CommonMark 代码片段的首尾空格折叠）。
// 这里实现的是判定 marker 所需的块结构子集，不声称是完整的 CommonMark 解析器。
func hasMarker(doc, marker string) bool {
	var sc blockScanner
	var para strings.Builder
	active := false
	// flush 结算当前段落；命中 marker 时返回 true。
	flush := func() bool {
		if !active {
			return false
		}
		active = false
		seg := para.String()
		para.Reset()
		return scanInlineCodeSpans(seg, marker)
	}
	for _, ln := range splitLines(doc) {
		st := sc.step(ln.content)
		// 容器栈变化意味着新的叶子块：先结算上一段，本行再按它自己的块结构处理。
		if st.BlockBreak && flush() {
			return true
		}
		if st.InCode || st.HasQuote || st.Blank {
			if flush() {
				return true
			}
			continue
		}
		// Setext 标题下划线（`===`、`--` 等）只有在上一段仍打开时才有意义：它把当前段落变成标题并立即结束，
		// 下划线本身不进入段落内容，避免标题里未闭合的反引号跨到后面的真实段落。
		if active && setextUnderline(st.Rest) {
			if flush() {
				return true
			}
			continue
		}
		if atxHeading(st.Rest) || thematicBreak(st.Rest) {
			if flush() {
				return true
			}
			// 标题是独立的单行叶子块：单独扫描，既不与上一行合并（上一行未闭合的反引号不会吞掉标题），
			// 也不污染下一行。
			if atxHeading(st.Rest) && scanInlineCodeSpans(st.Rest, marker) {
				return true
			}
			continue
		}
		if active {
			para.WriteByte('\n')
		}
		para.WriteString(st.Rest)
		active = true
	}
	return flush()
}

// scanInlineCodeSpans 在一段连续的段落文本里寻找单个反引号包裹、内容恰好等于 target 的行内代码片段。
//
// 代码片段外：`\` 转义下一个字节（反斜杠本身不再生效）。遇到 n 个连续反引号时，向后寻找精确长度 n 的闭合反引号串；
// 找不到就把这段反引号当作普通文本（i += n）继续扫描，未闭合的反引号不会吞掉后面的文本；找到时只有 n==1 且内容恰好
// 是 target 才算命中，否则完整跳过整个代码片段。代码片段内部按 CommonMark 不做反斜杠转义，所以 “ ` “ 里的 `\“
// 不会让闭合反引号失效。
func scanInlineCodeSpans(seg, target string) bool {
	for i := 0; i < len(seg); {
		if seg[i] == '\\' {
			i += 2
			continue
		}
		if seg[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(seg) && seg[i+n] == '`' {
			n++
		}
		closed := -1
		for j := i + n; j < len(seg); {
			if seg[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(seg) && seg[j+m] == '`' {
				m++
			}
			if m == n {
				closed = j
				break
			}
			j += m
		}
		if closed < 0 {
			i += n
			continue
		}
		if n == 1 && seg[i+1:closed] == target {
			return true
		}
		i = closed + n
	}
	return false
}

// atxHeading 判断一行（去掉缩进后）是否是 ATX 标题（1–6 个 `#`，后面是空白或行尾）。
func atxHeading(s string) bool {
	s = strings.TrimLeft(s, " \t")
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return false
	}
	return n == len(s) || s[n] == ' ' || s[n] == '\t'
}

// thematicBreak 判断一行（去掉缩进后）是否是主题分隔线（三个及以上的 `-` / `*` / `_`，中间只能有空白）。
func thematicBreak(s string) bool {
	s = strings.TrimLeft(s, " \t")
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := range len(s) {
		switch s[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// thematicAt 判断从当前游标（最多 3 列缩进）开始是否是一条主题分隔线。主题分隔线是叶子块，不能开启容器。
func thematicAt(c *lineCursor) bool {
	probe := *c
	if probe.skipAll() > maxFenceIndent {
		return false
	}
	return thematicBreak(probe.rest())
}

// setextUnderline 判断一行（去掉缩进后）是否是 Setext 标题下划线：1 个及以上的纯 `=` 或纯 `-`，之后只有空白。
// 中间不能夹空白（`- - -` 是主题分隔线，不是 Setext 下划线）。
func setextUnderline(s string) bool {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return false
	}
	c := s[0]
	if c != '=' && c != '-' {
		return false
	}
	i := 0
	for i < len(s) && s[i] == c {
		i++
	}
	return strings.TrimRight(s[i:], " \t") == ""
}

// MarkerInDocument 是 hasMarker 的导出包装：给定完整 marker（Marker 的返回值）判断文档里是否已存在精确条目。
func MarkerInDocument(doc, marker string) bool { return hasMarker(doc, marker) }

// quoteBlock 把不可信的文本放进引用块：逐行加 `> `，转义反引号和行首的波浪围栏，并打断 `kiln-dec:` 字样。
// 这样内容不会破坏文档结构（假标题、围栏、列表），也不会伪造出别的条目的 marker。
func quoteBlock(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, s := range lines {
		s = strings.ReplaceAll(s, "`", "\\`")
		s = strings.ReplaceAll(s, "kiln-dec:", "kiln-dec\u200b:")
		trimmed := strings.TrimLeft(s, " \t")
		if strings.HasPrefix(trimmed, "~~~") {
			s = strings.Replace(s, "~~~", "\\~~~", 1)
		}
		lines[i] = "> " + s
	}
	return strings.Join(lines, "\n")
}

// cleanInline 处理放在普通文本里的不可信短字段：去掉换行、转义反引号并打断 marker 字样。
func cleanInline(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "kiln-dec:", "kiln-dec\u200b:")
	return strings.TrimSpace(s)
}

// section 渲染一个带小标题的引用块；text 为空时返回空串。
func section(title, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "**" + title + "**\n\n" + quoteBlock(strings.TrimSpace(text)) + "\n\n"
}

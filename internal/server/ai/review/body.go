package review

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// AI 审计 PR review 的正文：让只读 review 的人直接看懂"是否通过、有几个问题、具体是什么、证据是什么"。
//
//  1. 结论：GitHub 的提示框（通过为绿色 TIP、未通过为红色 CAUTION、未完成为黄色 WARNING），写明问题总数和严重程度分布，
//     附摘要和审计范围。
//  2. 问题：先是目录表格（编号、严重程度、标题、位置、证据等级），再按 1、2、3……逐个展开（影响、触发条件、
//     实际与预期行为、证据、修复方向）。落在 diff 中的问题另作为行内评论，编号与正文一致；行内评论只是定位的补充，不替代正文。
//  3. 验证：Agent 整理的精简验证结果表格（本次改动、整体验证、变异验证），而不是命令流水；
//     完整的命令记录只在 Kiln 中展示，正文末尾给出链接。
//  4. 未验证、环境备注、顺带发现的已有问题分开列出，不与本次发现的问题混在一起。
//
// 正文超过 GitHub 的上限时：
//   - 结论、问题目录表格和未验证的部分（prioKeep）与每个问题的标题和影响（prioHead）作为整体预算：放不下时依次收紧
//     其中每项文字的长度上限（keepCaps）重新生成，最紧的一级去掉代码链接，保证结论、目录和未验证项一定放得下
//     （单字段的上限约束不了总量，不能靠最后的统一截尾）；
//   - 其余按优先级省略：v1 报告的命令记录、备注和已有问题、验证结果、从后往前的问题细节；
//     仍放不下时（问题很多）再从后往前省略问题的标题和影响，目录表格中仍有全部问题。

// githubWebURL 是 GitHub 网站的地址（正文中指向代码的链接）。
const githubWebURL = "https://github.com"

// maxCellText 是表格单元格中文本的长度上限；maxLinkedPath 是生成代码链接的路径长度上限（更长的只显示缩短的路径）。
const (
	maxCellText   = 200
	maxLinkedPath = 300
)

// keepCaps 是必须保留的部分（摘要、范围、问题标题、影响、未验证项、目录单元格）中单个文本的长度上限，从宽到紧依次尝试。
// 最紧的一级不生成代码链接：此时每个问题在目录中约 200 字节、每个未验证项约 70 字节，
// 即使问题和未验证项都达到 aireport 的数量上限，必须保留的部分也远小于 maxReviewBody。
var keepCaps = []int{2000, 500, 160, 60}

// 正文各部分的省略优先级：数字越大越先省略，prioKeep 总是保留。
const (
	prioKeep = iota
	prioHead // 每个问题的标题和影响：目录中已有全部问题，问题极多时可以从后往前省略
	prioDetail
	prioChecks
	prioNotes
	prioCommands
)

// omittedNote 是正文省略了部分内容时的说明。
const omittedNote = "\n> 篇幅所限，部分内容已省略，完整报告见 Kiln。\n"

var (
	severityLabel = map[string]string{aireport.SeverityHigh: "🔴 高", aireport.SeverityMedium: "🟠 中", aireport.SeverityLow: "🟡 低"}
	// severityAlert 是行内评论按严重程度使用的 GitHub 提示框。
	severityAlert = map[string]string{aireport.SeverityHigh: "CAUTION", aireport.SeverityMedium: "WARNING", aireport.SeverityLow: "NOTE"}
	evidenceLabel = map[string]string{aireport.EvidenceReproduced: "已复现", aireport.EvidenceCode: "代码证据", aireport.EvidenceSuspicion: "推测（未验证）"}
	checkResult   = map[string]string{aireport.CheckPassed: "✅ 通过", aireport.CheckFailed: "❌ 失败", aireport.CheckNotRun: "⚪ 未运行"}
	// checkKinds 是验证结果的类别（按显示顺序）。
	checkKinds = []struct{ kind, label string }{
		{aireport.CheckChange, "本次改动"}, {aireport.CheckSuite, "整体验证"}, {aireport.CheckMutation, "变异验证"},
	}
)

// reviewPart 是正文的一部分。
type reviewPart struct {
	prio int
	text string
}

// reviewDoc 生成 review 的正文和行内评论。
type reviewDoc struct {
	rep        *aireport.Report
	repo, head string
	// limit 是必须保留的部分中单个文本的长度上限（keepCaps 之一）；compact 为 true 时不生成代码链接。
	limit   int
	compact bool
	parts   []reviewPart
}

func (d *reviewDoc) add(prio int, text string) {
	d.parts = append(d.parts, reviewPart{prio: prio, text: text})
}

// buildAIReview 生成 PR review：正文完整列出全部问题；落在 lines（文件 → 可评论的行）中的问题另作为行内评论。
// repo 是仓库的 owner/name，用于生成指向被审 commit 中代码的链接。
func buildAIReview(rep *aireport.Report, row *aiReviewRow, repo string, lines map[string]map[int]bool, details, marker string) *gh.PullReview {
	r := &gh.PullReview{CommitID: row.HeadSHA, Event: gh.ReviewEventComment}
	inline := make([]bool, len(rep.Findings))
	full := &reviewDoc{rep: rep, repo: repo, head: row.HeadSHA, limit: keepCaps[0]}
	for i := range rep.Findings {
		f := &rep.Findings[i]
		if f.Path != "" && f.Line > 0 && lines[f.Path][f.Line] && len(r.Comments) < maxInlineComments {
			inline[i] = true
			r.Comments = append(r.Comments, gh.ReviewComment{Path: f.Path, Line: f.Line, Side: "RIGHT", Body: shorten.Text(full.inlineComment(i), maxCommentBody)})
		}
	}

	footer := "\n---\n\n<sub>Kiln AI 审计 · " + row.Agent
	if row.Platform != "" {
		// 一组平台中由哪台执行的要看得出（PLAN #26.1）
		footer += fmt.Sprintf(" · `%s`（节点 %s）", row.Platform, escapeControl(row.Runner))
	}
	footer += fmt.Sprintf(" · head `%s`", shorten.Hash(row.HeadSHA))
	if row.BaseSHA != "" {
		footer += fmt.Sprintf(" · base `%s`", shorten.Hash(row.BaseSHA))
	}
	footer += " · [" + recordLinkText(rep) + "](" + details + ")</sub>\n" + marker
	limit := maxReviewBody - len(footer)
	// 从宽到紧找到第一个能放下必须保留的部分和全部问题标题、影响的上限；都放不下时用最紧的一级
	var parts []reviewPart
	for i, c := range keepCaps {
		d := &reviewDoc{rep: rep, repo: repo, head: row.HeadSHA, limit: c, compact: i == len(keepCaps)-1}
		parts = d.body(inline)
		if partsSize(parts, prioHead) <= limit-len(omittedNote) {
			break
		}
	}
	r.Body = assemble(parts, limit) + footer
	return r
}

// body 返回正文（不含末尾的链接和标记）的各个部分。
func (d *reviewDoc) body(inline []bool) []reviewPart {
	rep := d.rep
	d.add(prioKeep, d.conclusion())
	if len(rep.Findings) > 0 {
		d.add(prioKeep, d.findingTable(inline))
		for i := range rep.Findings {
			d.add(prioHead, d.findingHead(i, inline[i]))
			if s := findingDetail(&rep.Findings[i]); s != "" {
				d.add(prioDetail, s)
			}
		}
	}
	d.addChecks()
	if len(rep.Unverified) > 0 {
		var b strings.Builder
		b.WriteString("\n### ⚠️ 未验证\n\n")
		for _, u := range rep.Unverified {
			b.WriteString(listItem("", d.short(u)))
		}
		d.add(prioKeep, b.String())
	}
	d.addNotes()
	if !rep.V2() {
		d.addLegacyCommands()
	}
	return d.parts
}

// short 把必须保留的文本缩短到 d.limit。
func (d *reviewDoc) short(s string) string {
	return shorten.Text(strings.TrimSpace(s), d.limit)
}

// partsSize 返回优先级不超过 prio 的部分的总长度。
func partsSize(parts []reviewPart, prio int) int {
	n := 0
	for _, p := range parts {
		if p.prio <= prio {
			n += len(p.text)
		}
	}
	return n
}

// conclusion 返回正文开头的结论：标题和 GitHub 提示框（结论、摘要、审计范围）。
func (d *reviewDoc) conclusion() string {
	alert := map[string]string{aireport.VerdictNoIssues: "TIP", aireport.VerdictIssuesFound: "CAUTION"}[d.rep.Verdict]
	if alert == "" {
		alert = "WARNING"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Kiln AI 审计\n\n> [!%s]\n> **%s**\n>\n%s", alert, headline(d.rep), quote(d.short(d.rep.Summary)))
	if s := strings.TrimSpace(d.rep.Scope); s != "" {
		b.WriteString(">\n> **范围**：" + oneLine(d.short(s)) + "\n")
	}
	return b.String()
}

// headline 返回结论：是否通过、问题总数和严重程度分布。门禁的语义不变：只有 no_issues 算通过。
func headline(rep *aireport.Report) string {
	n := len(rep.Findings)
	switch rep.Verdict {
	case aireport.VerdictNoIssues:
		return "✅ 审计通过：未发现本次改动引入的新问题（0 项）"
	case aireport.VerdictIssuesFound:
		return fmt.Sprintf("❌ 审计未通过：发现 %d 个问题（%s）", n, severityCounts(rep.Findings))
	default:
		if n == 0 {
			return "⚠️ 审计未完成：部分内容没能验证，不能作为通过的依据（已发现 0 个问题）"
		}
		return fmt.Sprintf("⚠️ 审计未完成：部分内容没能验证，不能作为通过的依据（已发现 %d 个问题：%s）", n, severityCounts(rep.Findings))
	}
}

// findingTable 返回问题的目录表格。
func (d *reviewDoc) findingTable(inline []bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### 🐞 问题（%d）\n\n| # | 严重程度 | 问题 | 位置 | 证据 |\n|:-:|:-:|---|---|:-:|\n", len(d.rep.Findings))
	for i := range d.rep.Findings {
		f := &d.rep.Findings[i]
		// 合法的路径中也可能有竖线：在表格中转义，否则会被当作列分隔符（链接中已编码为 %7C）
		loc := strings.ReplaceAll(d.location(f.Path, f.Line), "|", `\|`)
		if inline[i] {
			loc += " 💬"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", i+1, severityLabel[f.Severity], cell(f.Title, min(maxCellText, d.limit)), loc, evidenceLabel[f.Evidence])
	}
	if slices.Contains(inline, true) {
		b.WriteString("\n<sub>💬 同时作为行内评论发布在对应的代码行上，编号与本文一致。</sub>\n")
	}
	return b.String()
}

// severityCounts 返回问题的严重程度分布，例如"高 1、中 0、低 2"。
func severityCounts(fs []aireport.Finding) string {
	c := map[string]int{}
	for i := range fs {
		c[fs[i].Severity]++
	}
	return fmt.Sprintf("高 %d、中 %d、低 %d", c[aireport.SeverityHigh], c[aireport.SeverityMedium], c[aireport.SeverityLow])
}

// findingTitle 返回问题 i 的标题（严重程度和标题，不含编号）。
func (d *reviewDoc) findingTitle(i int) string {
	f := &d.rep.Findings[i]
	return fmt.Sprintf("%s · %s", severityLabel[f.Severity], oneLine(d.short(f.Title)))
}

// findingHead 返回正文中问题 i 总是保留的部分：编号标题、位置、证据等级和影响。
func (d *reviewDoc) findingHead(i int, inline bool) string {
	f := &d.rep.Findings[i]
	var b strings.Builder
	fmt.Fprintf(&b, "\n#### %d. %s\n\n", i+1, d.findingTitle(i))
	meta := d.location(f.Path, f.Line) + " · " + evidenceLabel[f.Evidence]
	if inline {
		meta += " · 💬 行内评论"
	}
	b.WriteString(meta + "\n\n")
	if f.Impact != "" {
		b.WriteString(listItem("影响", d.short(f.Impact)))
	}
	return b.String()
}

// findingDetail 返回问题的细节：触发条件、实际与预期行为、证据、修复方向和补充说明（v1 报告只有说明）。
func findingDetail(f *aireport.Finding) string {
	var b strings.Builder
	for _, x := range []struct{ label, text string }{
		{"触发条件", f.Scenario}, {"实际结果", f.Actual}, {"预期结果", f.Expected}, {"证据", f.Proof}, {"修复方向", f.Fix}, {"说明", f.Body},
	} {
		if strings.TrimSpace(x.text) != "" {
			b.WriteString(listItem(x.label, x.text))
		}
	}
	return b.String()
}

// inlineComment 返回问题 i 的行内评论：按严重程度用 GitHub 提示框标出，编号与正文一致，包含完整的细节（位置就是评论所在的行）。
func (d *reviewDoc) inlineComment(i int) string {
	f := &d.rep.Findings[i]
	var b strings.Builder
	fmt.Fprintf(&b, "> [!%s]\n> **Kiln AI 审计 · 问题 %d / %d · %s**\n> %s\n\n", severityAlert[f.Severity], i+1, len(d.rep.Findings), d.findingTitle(i), evidenceLabel[f.Evidence])
	if f.Impact != "" {
		b.WriteString(listItem("影响", f.Impact))
	}
	b.WriteString(findingDetail(f))
	return b.String()
}

// location 返回指向被审 commit 中代码的链接；path 为空时说明不针对具体文件。
func (d *reviewDoc) location(path string, line int) string {
	if path == "" {
		return "不针对具体文件"
	}
	// 路径太长时保留结尾（文件名最有用）；换行等控制字符显示为转义，不拆开所在的行
	label, anchor := escapeControl(shortTail(path, min(maxCellText, d.limit))), ""
	if line > 0 {
		label += fmt.Sprintf(":%d", line)
		anchor = fmt.Sprintf("#L%d", line)
	}
	label = "`" + strings.ReplaceAll(label, "`", "'") + "`"
	if d.compact || d.repo == "" || d.head == "" || len(path) > maxLinkedPath {
		return label
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("[%s](%s/%s/blob/%s/%s%s)", label, githubWebURL, d.repo, d.head, strings.Join(segs, "/"), anchor)
}

// addChecks 用表格写出精简的验证结果（按类别排序）。
func (d *reviewDoc) addChecks() {
	if len(d.rep.Checks) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("\n### 🧪 验证\n\n| 结果 | 类别 | 验证项 | 说明 |\n|:-:|:-:|---|---|\n")
	for _, k := range checkKinds {
		for _, c := range d.rep.Checks {
			if c.Kind == k.kind {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", checkResult[c.Result], k.label, cell(c.Title, maxCellText), cell(c.Detail, maxCellText))
			}
		}
	}
	if slices.ContainsFunc(d.rep.Checks, func(c aireport.Check) bool { return c.Kind == aireport.CheckMutation }) {
		b.WriteString("\n<sub>变异验证：改坏关键逻辑后，对应的测试应当失败；✅ 表示测试按预期发现了改动。</sub>\n")
	}
	d.add(prioChecks, b.String())
}

// addNotes 写出环境备注和顺带发现的已有问题（都不是本次发现的问题，不影响结论）。
func (d *reviewDoc) addNotes() {
	if len(d.rep.EnvNotes) > 0 {
		var b strings.Builder
		b.WriteString("\n### 🛠️ 环境备注\n\n<sub>环境问题不是本次改动的问题，不影响结论。</sub>\n\n")
		for _, n := range d.rep.EnvNotes {
			b.WriteString(listItem("", n))
		}
		d.add(prioNotes, b.String())
	}
	if len(d.rep.Preexisting) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "\n<details><summary>顺带发现的已有问题（%d，与本次改动无关，不影响结论）</summary>\n\n", len(d.rep.Preexisting))
		for _, p := range d.rep.Preexisting {
			text := oneLine(p.Title)
			if p.Path != "" {
				text = d.location(p.Path, p.Line) + " " + text
			}
			if p.Detail != "" {
				text += "：" + p.Detail
			}
			b.WriteString(listItem("", text))
		}
		b.WriteString("\n</details>\n")
		d.add(prioNotes, b.String())
	}
}

// addLegacyCommands 写出 v1 报告的命令记录（v1 没有精简的验证结果，只能折叠展示命令）。
func (d *reviewDoc) addLegacyCommands() {
	if len(d.rep.CommandsRun) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n<details><summary>执行的命令（%d）</summary>\n\n", len(d.rep.CommandsRun))
	for _, c := range d.rep.CommandsRun {
		fmt.Fprintf(&b, "- `%s` → %d", strings.ReplaceAll(oneLine(c.Command), "`", "'"), c.ExitCode)
		if c.Note != "" {
			b.WriteString("：" + oneLine(c.Note))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n</details>\n")
	d.add(prioCommands, b.String())
}

// recordLinkText 返回正文末尾指向 Kiln 的链接文字：完整的命令记录只在 Kiln 中展示。
func recordLinkText(rep *aireport.Report) string {
	n := len(rep.CommandsRun)
	if n == 0 {
		return "在 Kiln 中查看完整报告"
	}
	failed := 0
	for _, c := range rep.CommandsRun {
		if c.Outcome == aireport.OutcomeFailed {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Sprintf("完整执行记录与审计证据（%d 条命令，%d 条最终失败）", n, failed)
	}
	return fmt.Sprintf("完整执行记录与审计证据（%d 条命令）", n)
}

// listItem 返回一个列表项：label 不为空时写成"**label**：text"；多行的 text 缩进到列表项内（保留代码块等格式）。
func listItem(label, text string) string {
	text = strings.TrimSpace(text)
	prefix := "- "
	if label != "" {
		prefix += "**" + label + "**："
	}
	if !strings.Contains(text, "\n") {
		return prefix + text + "\n"
	}
	return prefix + "\n\n" + indent(text, "  ") + "\n\n"
}

// indent 给 s 的每个非空行加上前缀。
func indent(s, prefix string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if strings.TrimSpace(l) != "" {
			ls[i] = prefix + l
		}
	}
	return strings.Join(ls, "\n")
}

// escapeControl 把 s 中的控制字符（换行、回车等）写成可见的转义（\n、\r、\x01），用于在 Markdown 中显示路径。
func escapeControl(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteRune(r) // 例如 '\n'
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// shortTail 把 s 缩短到最多 n 字节，保留结尾，开头换成省略号；不切开多字节字符。
func shortTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - max(n, 0)
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return shorten.Ellipsis + s[i:]
}

// quote 把多行文本写成引用块（提示框）中的行。
func quote(s string) string {
	var b strings.Builder
	for l := range strings.Lines(s) {
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			b.WriteString(">\n")
		} else {
			b.WriteString("> " + l + "\n")
		}
	}
	return b.String()
}

// cell 返回表格单元格中的文本：合成一行、限制在 n 字节并转义竖线。
func cell(s string, n int) string {
	s = oneLine(shorten.Text(s, n))
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

// oneLine 把多行文本合成一行（用于标题等只能写一行的地方）。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// assemble 拼接正文，不超过 limit 字节：超出时按优先级从高到低（同一优先级从后往前）省略部分内容，
// prioKeep 的部分总是保留；只剩它们仍然超出时硬截断（keepCaps 保证正常的报告不会走到这一步）。
func assemble(parts []reviewPart, limit int) string {
	join := func() string {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.text)
		}
		return b.String()
	}
	total := 0
	for _, p := range parts {
		total += len(p.text)
	}
	if total <= limit {
		return join()
	}
	budget := limit - len(omittedNote)
	for prio := prioCommands; prio > prioKeep && total > budget; prio-- {
		for i := len(parts) - 1; i >= 0 && total > budget; i-- {
			if parts[i].prio == prio {
				total -= len(parts[i].text)
				parts[i].text = ""
			}
		}
	}
	body := join()
	if len(body) > budget {
		body = shorten.Text(body, budget-len(shorten.Ellipsis))
	}
	return body + omittedNote
}

package aidev

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// PR 描述和评论的文字（PLAN #26.11）。Agent 写的内容（摘要、验收、验证命令）都标为"Agent 自报"：真实执行的证据只有 CI
// （#26.14）。只有完整解决（full）的 Issue 写 Fixes，部分解决写 Refs（I7：只有完整解决的 Issue 在合并后被关闭）。
//
// Agent 写的文字可能被 Issue 内容注入（PLAN #26.12），写进 PR 描述和评论之前先 defuse：引用（#N、GH-N、Issue / PR 的链接）
// 不再被 GitHub 识别，关闭关键字就关不掉别的 Issue，也不会在别的 Issue 留下引用；@ 不再提及人；< 转义，不能藏 HTML 注释
// 或伪造 Kiln 的标记。提交说明 Server 改不了：推进时检查，有关闭关键字时加 kiln:hold（closingRefs）。

// 这些正则只读。
var (
	// hashRefRE、ghRefRE 匹配 GitHub 会自动链接的 Issue / PR 引用：#N、owner/repo#N；GH-N。
	hashRefRE = regexp.MustCompile(`#(\d)`)
	ghRefRE   = regexp.MustCompile(`(?i)\b(gh)-(\d)`)
	// issueURLRE 匹配 Issue / PR 的链接。
	issueURLRE = regexp.MustCompile(`https?://\S+/(?:issues|pull)/\d+\S*`)
	// mentionRE 匹配 @用户 / @组织/团队。
	mentionRE = regexp.MustCompile(`@([A-Za-z0-9])`)
	// closingRE 匹配关闭关键字和它关闭的引用（GitHub 的规则：close、fix、resolve 的各种形式，可选冒号）。
	closingRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b:?\s+((?:[\w.-]+/[\w.-]+)?#\d+|gh-\d+|https?://\S+/(?:issues|pull)/\d+)`)
)

// workItemRef 返回写在 GitHub 上的工作项编号：放在代码里，不被当成 Issue / PR 的引用（工作项的编号不是 Issue 编号）。
func workItemRef(id int64) string { return fmt.Sprintf("工作项 `#%d`", id) }

// defuse 返回可以安全写进 PR 描述和评论的 Agent 文字（见上）。
func defuse(s string) string {
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = issueURLRE.ReplaceAllStringFunc(s, func(u string) string { return "`" + strings.ReplaceAll(u, "`", "'") + "`" })
	s = hashRefRE.ReplaceAllString(s, "＃$1")      // 全角 #
	s = ghRefRE.ReplaceAllString(s, "$1\u2011$2") // 不断行连字符
	return mentionRE.ReplaceAllString(s, "＠$1")
}

// closingRefs 返回提交说明中关闭关键字引用的、不在 allowed 中的 Issue（allowed 是工作项中完整解决的 Issue 的 #N）。
func closingRefs(messages []string, allowed []int) []string {
	var out []string
	for _, m := range messages {
		for _, match := range closingRE.FindAllStringSubmatch(m, -1) {
			ref := match[1]
			if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil && strings.HasPrefix(ref, "#") && slices.Contains(allowed, n) {
				continue
			}
			if !slices.Contains(out, ref) {
				out = append(out, ref)
			}
		}
	}
	return out
}

// maxPRBodyLen 是 PR 描述的上限（GitHub 限制 65536 字符，留出余量）。
const maxPRBodyLen = 60000

// maxTitleLen 是 PR 标题的上限。
const maxTitleLen = 200

// workItemMarker 是 PR 描述中的标记：人和工具据此认出工作项的 PR。
const workItemMarker = "<!-- kiln:work-item:%d -->"

// designTitlePrefix 是设计 PR 标题的前缀（PLAN #31.5）。
const designTitlePrefix = "设计："

// prTitle 返回 PR 的标题：交付报告的 pr_title，没有时用第一个 Issue 的标题。设计 PR 以"设计："开头。
func prTitle(w *WorkItem, rep *devreport.Delivery, issues []WorkIssue) string {
	t := ""
	if rep != nil {
		t = defuse(strings.TrimSpace(rep.PRTitle)) // squash 合并时标题是提交说明的第一行
	}
	if t == "" && len(issues) > 0 {
		// Issue 标题同样是仓库内容（可由外部输入），兜底路径也要 defuse，否则 #N、@ 和 < 会原样进入 PR 标题。
		t = defuse(strings.TrimSpace(issues[0].Title))
	}
	if t == "" {
		t = "AI 开发：" + workItemRef(w.ID)
	}
	if w.Phase == PhaseDesign && !strings.HasPrefix(t, designTitlePrefix) {
		t = designTitlePrefix + t
	}
	return shorten.Text(strings.ReplaceAll(t, "\n", " "), maxTitleLen)
}

// prBody 返回 PR 的描述。holds 是加 holdLabel 的原因。
func prBody(w *WorkItem, rep *devreport.Delivery, issues []WorkIssue, holdLabel string, holds []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Kiln AI 开发的%s（Agent：%s）。CI 和另一台机器的 AI 审计照常运行；审计或 CI 不通过时同一个工作项返修（最多 %d 轮）。\n\n",
		workItemRef(w.ID), w.Agent, w.MaxFixRounds)
	switch {
	case w.Phase == PhaseDesign:
		b.WriteString("**设计 PR**：只改文档，不关闭 Issue；合并后 Kiln 自动开始实现，实现在另一个 PR 中交付。\n\n")
	case w.DesignPR != nil:
		fmt.Fprintf(&b, "**实现 PR**：按已合并的设计（#%d", *w.DesignPR)
		if w.DesignSHA != "" {
			b.WriteString("，" + shorten.Hash(w.DesignSHA))
		}
		b.WriteString("）实现。\n\n")
	}
	if w.ImplRound > 1 {
		fmt.Fprintf(&b, "第 %d 轮实现：之前的轮次已经合并了一部分。\n\n", w.ImplRound)
	}
	if len(holds) > 0 {
		fmt.Fprintf(&b, "> 已加 `%s`（不自动入队，请人确认后去掉）：\n", holdLabel)
		for _, h := range holds {
			b.WriteString(">\n> - " + h + "\n")
		}
		b.WriteString("\n")
	}
	if w.RequirementsChanged {
		b.WriteString("> 领取之后有 Issue 的标题或正文被修改过。\n\n")
	}
	if rep != nil && rep.Summary != "" {
		b.WriteString("## 摘要（Agent 自报）\n\n" + defuse(rep.Summary) + "\n\n")
	}
	b.WriteString("## Issue\n\n")
	for _, is := range issues {
		res := is.Resolution
		if rep != nil {
			res = rep.Resolution(is.Number)
		}
		switch {
		case w.Phase == PhaseDesign:
			fmt.Fprintf(&b, "- Refs #%d：设计（合并后不会关闭，实现后再按完成度关闭）\n", is.Number)
			continue
		case res == devreport.ResolutionFull:
			fmt.Fprintf(&b, "- Fixes #%d：完整解决\n", is.Number)
		case res == devreport.ResolutionPartial:
			fmt.Fprintf(&b, "- Refs #%d：部分解决（合并后不会关闭）\n", is.Number)
		default:
			fmt.Fprintf(&b, "- Refs #%d：没有解决\n", is.Number)
		}
		if rep == nil {
			continue
		}
		for _, r := range rep.Issues {
			if r.Number != is.Number {
				continue
			}
			for _, a := range r.Acceptance {
				mark := " "
				if a.Done {
					mark = "x"
				}
				fmt.Fprintf(&b, "  - [%s] %s", mark, defuse(a.Item))
				if a.Evidence != "" {
					b.WriteString("：" + defuse(a.Evidence))
				}
				b.WriteString("\n")
			}
			if r.Notes != "" {
				b.WriteString("  - 说明：" + defuse(r.Notes) + "\n")
			}
		}
	}
	if rep != nil {
		designSections(&b, rep)
	}
	if rep != nil && len(rep.Verification) > 0 {
		b.WriteString("\n## 验证（Agent 自报，不是执行证据；以 CI 为准）\n\n")
		for _, v := range rep.Verification {
			fmt.Fprintf(&b, "- `%s`：%s", defuse(strings.ReplaceAll(strings.ReplaceAll(v.Command, "`", "'"), "\n", " ")), v.Outcome)
			if v.Note != "" {
				b.WriteString("（" + defuse(v.Note) + "）")
			}
			b.WriteString("\n")
		}
	}
	if rep != nil && strings.TrimSpace(rep.Handoff.Remaining) != "" {
		b.WriteString("\n## 剩余（Agent 自报）\n\n" + defuse(rep.Handoff.Remaining) + "\n")
	}
	body := shorten.Text(b.String(), maxPRBodyLen)
	return body + "\n\n" + fmt.Sprintf(workItemMarker, w.ID) + "\n"
}

// designSections 写出交付报告的设计文档、决策记录、外部输入和与设计的出入（PLAN #31.5，都是 Agent 自报）。
func designSections(b *strings.Builder, rep *devreport.Delivery) {
	d := rep.Design
	if len(d.Documents) > 0 {
		b.WriteString("\n## 设计文档\n\n")
		for _, p := range d.Documents {
			b.WriteString("- `" + strings.ReplaceAll(defuse(p), "`", "'") + "`\n")
		}
	}
	if len(d.Decisions) > 0 {
		b.WriteString("\n## 决策记录（Agent 自报）\n\n")
		for i, x := range d.Decisions {
			fmt.Fprintf(b, "%d. **%s**", i+1, decisionCategory(x.Category))
			if x.Ref != "" {
				b.WriteString("（" + defuse(x.Ref) + "）")
			}
			b.WriteString("\n")
			for _, kv := range [][2]string{
				{"现状和依据", x.Context}, {"考虑过的方案", x.Options}, {"选择及理由", x.Choice}, {"影响", x.Impact}, {"复盘时看", x.Review},
			} {
				if strings.TrimSpace(kv[1]) != "" {
					b.WriteString("   - " + kv[0] + "：" + defuse(kv[1]) + "\n")
				}
			}
		}
	}
	if len(d.ExternalInputs) > 0 {
		b.WriteString("\n## 需要人提供的条件（Agent 自报）\n\n")
		for _, x := range d.ExternalInputs {
			b.WriteString("- " + defuse(x.What))
			if x.Blocks != "" {
				b.WriteString("；阻塞：" + defuse(x.Blocks))
			}
			if x.Resume != "" {
				b.WriteString("；恢复条件：" + defuse(x.Resume))
			}
			b.WriteString("\n")
		}
	}
	if len(rep.DesignDeviations) > 0 {
		b.WriteString("\n## 与已合并设计的出入（Agent 自报）\n\n")
		for _, x := range rep.DesignDeviations {
			b.WriteString("- " + defuse(x.Item) + "：" + defuse(x.Reason) + "\n")
		}
	}
}

// decisionCategory 返回决定类别的说明。
func decisionCategory(c string) string {
	switch c {
	case devreport.DecisionConfirmed:
		return "改已确认的决定（需要人确认）"
	case devreport.DecisionAcceptance:
		return "删减或改变验收项"
	case devreport.DecisionPolicy:
		return "预算、范围等取舍"
	}
	return "其他"
}

// dispositionsComment 返回返修对审计问题的处置汇总（每轮一条评论）。
func dispositionsComment(w *WorkItem, rep *devreport.Delivery) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Kiln AI 开发第 %d 轮返修对审计问题的处置（Agent 自报；下一轮审计会在新的 head 上逐个复核）：\n\n", w.FixRounds)
	for _, d := range rep.Dispositions {
		fmt.Fprintf(&b, "- **%s** %s", defuse(d.Finding), d.Action)
		switch d.Action {
		case devreport.ActionFixed:
			fmt.Fprintf(&b, "（%s）", defuse(shorten.Hash(d.Commit)))
		case devreport.ActionDuplicate:
			fmt.Fprintf(&b, "（同 %s）", defuse(d.DuplicateOf))
		}
		if d.Reason != "" {
			b.WriteString("：" + defuse(d.Reason))
		}
		b.WriteString("\n")
	}
	return b.String()
}

package aiprompt

import (
	"fmt"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
)

// DevInput 是生成开发 prompt 需要的信息。
type DevInput struct {
	// Repository 是仓库（owner/name）。
	Repository string
	// CloneURL 是仓库的 clone 地址（clone 中 origin 的地址，gitRule 中说明）。
	CloneURL string
	// SHA 是任务的起点（AITask.sha：选题、实现是默认分支的 head，返修是正式分支的期望 head）。
	SHA string
	// Spec 是任务的开发内容（冻结的执行输入）。
	Spec *aispec.DevelopSpec
	// Platform 是这次执行的平台（领取时节点上报的，例如 linux/amd64）：工具链按它选。
	Platform string
}

// Select 返回选题的 prompt（启动 Agent 时给出）。选题报告写出后由 claim 工具校验和领取，下一步说明由工具输出
// （见 Develop、Reselect、Ended）。
func Select(in DevInput) string {
	s := in.Spec
	var b strings.Builder
	b.WriteString("你是 Kiln 的开发 Agent（development 角色）。这一轮只选题、不改代码。\n")
	b.WriteString(skillRule())
	b.WriteString("\n## 任务\n\n")
	fmt.Fprintf(&b, "- 仓库：%s（当前目录是它的 clone，HEAD 是默认分支的 %s）\n", in.Repository, in.SHA)
	fmt.Fprintf(&b, "- 候选 Issue 在 `%s`（JSON：number、title、body、labels、url），来自仓库的任务池（人加了 kiln:ready 的 Issue）。\n",
		taskPath(FileCandidates))
	fmt.Fprintf(&b, "- 同一个仓库中进行中的工作项（可能由其他 Agent 或节点在开发）在 `%s`（JSON：work_item_id、state、issues、plan）。"+
		"不要选会和它们改到同一处的 Issue。\n", taskPath(FileInProgress))
	fmt.Fprintf(&b, "- 选出这一批要做的 Issue：最多 %d 个，具体几个由你判断。每个未选的候选都写进 skipped，写明原因和重新评估的条件。\n",
		s.MaxIssues)
	b.WriteString("- 这一轮只读：不改文件、不 commit。\n\n")
	b.WriteString(selectGuide)
	b.WriteString("\n")
	devRules(&b, in)
	b.WriteString("\n")
	reportSection(&b, "选题报告", FileSelection, selectionSchema(), "claim")
	b.WriteString("\nclaim 会校验选题报告并领取 Issue，输出下一步说明（领取成功后的设计与开发要求，或者没有领取时的说明）；" +
		"按它的输出继续，不要自己判断领取结果。\n")
	return b.String()
}

// Develop 返回领取成功后交给 Agent 的下一步说明（ClaimDevIssuesResponse.message，由 claim 工具输出）。
// phase 是领取后的阶段：design 只交付设计 PR；implement 先设计再开发，一个 PR 完成。
func Develop(in DevInput, workItem int64, branch, phase string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "领取成功（工作项 #%d）。当前分支是 %s，从 %s 开始。", workItem, branch, in.SHA)
	if phase == devreport.PhaseDesign {
		b.WriteString("现在开始设计。你选择了先交付设计 PR：这一轮**只做设计**，只写 Markdown 文档，不改代码；设计 PR 合并后 Kiln 会开始实现。\n\n")
	} else {
		b.WriteString("现在开始开发：先设计、再实现，设计和实现在同一个 PR 中交付。\n\n")
	}
	b.WriteString(designGuide)
	b.WriteString("\n")
	docRule(&b, in.Spec.Docs)
	writeDelivery(&b, in, phase)
	return b.String()
}

// Reselect 返回领取冲突后让 Agent 重新选题的说明（候选和进行中的工作项已经更新）。
func Reselect(in DevInput, reason string) string {
	return fmt.Sprintf("领取没有成功：%s。候选（`%s`）和进行中的工作项（`%s`）已更新，请重新选题，覆盖写 `%s`，然后重新运行 `%s claim`。"+
		"如果新候选中没有值得做的，写空的 selected 并写明原因。\n",
		reason, taskPath(FileCandidates), taskPath(FileInProgress), taskPath(FileSelection), Tool)
}

// Ended 返回没有领取到工作项时交给 Agent 的结束说明（claim 工具输出）。
func Ended(reason string) string {
	if reason == "" {
		reason = "没有领取 Issue"
	}
	return fmt.Sprintf("这一轮选题结束：%s。Kiln 已经保存选题报告，不需要改代码，也不要再领取。现在%s\n", reason, doneRule())
}

// Implement 返回实现的 prompt（先设计的设计合并后、部分完成后的下一轮，PLAN #31.3）。
func Implement(in DevInput) string {
	s := in.Spec
	var b strings.Builder
	b.WriteString("你是 Kiln 的开发 Agent（development 角色），这一轮是实现。\n")
	b.WriteString(skillRule())
	b.WriteString("\n## 任务\n\n")
	fmt.Fprintf(&b, "- 仓库：%s（当前目录是它的 clone），工作项 #%d，分支 %s（HEAD 是默认分支的 %s），目标分支 %s，第 %d 轮实现。\n",
		in.Repository, s.WorkItemID, s.Branch, in.SHA, s.BaseRef, s.ImplRound)
	if s.DesignPR != 0 {
		fmt.Fprintf(&b, "- 已合并的设计：PR #%d（commit %s），设计文档：%s。\n", s.DesignPR, s.DesignSHA, docList(s.DesignDocs))
	}
	fmt.Fprintf(&b, "- 工作项的 Issue、上一轮的交接都在 `%s`（JSON）。\n", taskPath(FileWork))
	if s.RequirementsChanged {
		b.WriteString("- 注意：工作项领取之后，有 Issue 的标题或正文被修改过；以文件中的当前内容为准，交付报告中说明对设计的影响。\n")
	}
	b.WriteString("\n")
	b.WriteString(implementGuide)
	b.WriteString("\n")
	devRules(&b, in)
	b.WriteString("\n")
	writeDelivery(&b, in, devPhase(s.Phase))
	return b.String()
}

// Fix 返回返修的 prompt（启动 Agent 时给出）。
func Fix(in DevInput) string {
	s := in.Spec
	var b strings.Builder
	b.WriteString("你是 Kiln 的开发 Agent（development 角色），这一轮是返修。\n")
	b.WriteString(skillRule())
	b.WriteString("\n## 任务\n\n")
	fmt.Fprintf(&b, "- 仓库：%s（当前目录是它的 clone），工作项 #%d，分支 %s（HEAD %s），PR #%d → %s，第 %d 轮返修。\n",
		in.Repository, s.WorkItemID, s.Branch, in.SHA, s.PRNumber, s.BaseRef, s.Round)
	switch {
	case s.Phase == devreport.PhaseDesign:
		b.WriteString("- 这是**设计 PR** 的返修：只能改 Markdown 文档，不改代码。\n")
	case s.DesignPR != 0:
		fmt.Fprintf(&b, "- 这是按已合并的设计（PR #%d，commit %s）实现的 PR。\n", s.DesignPR, s.DesignSHA)
	}
	fmt.Fprintf(&b, "- 工作项的 Issue、要处理的审计问题（稳定 ID，例如 F123-1）、失败的 CI、上一轮的交接都在 `%s`（JSON）。\n", taskPath(FileWork))
	if s.RequirementsChanged {
		b.WriteString("- 注意：工作项领取之后，有 Issue 的标题或正文被修改过；以文件中的当前内容为准，交付报告中说明对已有改动的影响。\n")
	}
	b.WriteString("\n")
	if c := s.Conflict; c != nil {
		conflictSection(&b, c)
	}
	b.WriteString(fixGuide)
	b.WriteString("\n")
	devRules(&b, in)
	b.WriteString("\n")
	writeDelivery(&b, in, devPhase(s.Phase))
	return b.String()
}

// conflictSection 写出解决合并冲突的要求（PLAN #26.11）：把目标分支合并进当前分支（正式分支只快进，Server 拒绝改写历史的
// 交付），保留两边的有效改动，合并后按原需求验证。
func conflictSection(b *strings.Builder, c *aispec.DevConflict) {
	b.WriteString("## 合并冲突\n\n")
	fmt.Fprintf(b, "- 合并队列把目标分支 %s 合并进这个 PR 时平台报告冲突：PR head %s 与 %s 的 commit %s 冲突（`%s` 的 conflict）。"+
		"这一轮要解决这个冲突。\n", c.BaseRef, c.Head, c.BaseRef, c.BaseSHA, taskPath(FileWork))
	fmt.Fprintf(b, "- 目标分支的 head 已经 fetch 到 `refs/remotes/origin/%s`；它不包含 %s 时先取最新的：`%s git fetch origin`。\n",
		c.BaseRef, c.BaseSHA, ExecCommand)
	fmt.Fprintf(b, "- 用 `git merge refs/remotes/origin/%s` 把目标分支**合并**进当前分支，逐个解决冲突，保留两边已经合并、已经开发的有效改动"+
		"（目标分支上别人的改动不能丢）。**不要 rebase、不要 reset、不要改写已有的提交**：正式分支只能快进，改写历史的交付会被拒绝。\n", c.BaseRef)
	b.WriteString("- 合并后按原需求重新验证行为（仓库规定的构建、静态检查、测试），交付报告的 handoff 写明冲突在哪里、怎么取舍。\n\n")
}

// writeDelivery 写出交付的要求和方式（设计阶段只能改 Markdown，PLAN #31.3 I12）。
func writeDelivery(b *strings.Builder, in DevInput, phase string) {
	b.WriteString("## 交付\n\n")
	if phase == devreport.PhaseDesign {
		b.WriteString("把设计文档 commit 在当前分支上（提交信息与仓库的语言一致），只能改 Markdown（*.md）文件，不要 push、不要切换分支。" +
			"完成后工作区要干净（没有未提交的改动和未跟踪的文件）。提交说明中不要写关闭 Issue 的关键字（Fixes、Closes 等）。\n")
		b.WriteString("设计不解决 Issue：每个 Issue 的 resolution 写 none，acceptance 列出原验收项（done 写 false）；" +
			"design.documents 列出设计文档的路径，design.decisions、design.external_inputs 按设计如实填写；pr_title 写设计 PR 的标题。" +
			"无法继续时 status 写 blocked 并在 handoff.remaining 写明还剩什么、需要什么条件。\n\n")
		reportSection(b, "交付报告", FileDelivery, deliverySchema(), "deliver")
		return
	}
	b.WriteString("实现选中的 Issue，在当前分支上 commit（提交信息与仓库的语言一致），不要 push、不要切换分支。" +
		"能运行的验证都运行（构建、静态检查、相关测试）。完成后工作区要干净（没有未提交的改动和未跟踪的文件）。")
	if in.Spec.BaseRef != "" {
		fmt.Fprintf(b, "目标分支的 head 已经 fetch 到 `refs/remotes/origin/%s`（设计阶段只改 Markdown 的检查按它算）。", in.Spec.BaseRef)
	}
	b.WriteString("\n")
	b.WriteString("报告中每个 Issue 的 resolution 必须如实：验收项没有全部完成的不能写 full（full 的 Issue 会在合并后被关闭）；" +
		"verification 只写实际运行过的命令。有设计文档、有疑问的决定或外部输入时填写 design（documents 只列这个 PR 中改动的设计文档）；" +
		"按已合并的设计实现时，出入写在 design_deviations。无法继续时 status 写 blocked 并在 handoff.remaining 写明还剩什么、需要什么条件；" +
		"不需要改代码时写 no_change 并逐个说明原因。\n\n")
	reportSection(b, "交付报告", FileDelivery, deliverySchema(), "deliver")
}

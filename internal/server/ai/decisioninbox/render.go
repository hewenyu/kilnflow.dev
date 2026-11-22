package decisioninbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
)

// 入队与渲染：把一个有效的交付报告拆成要同步的条目（每条决策记录一条，必要时再加一条受阻摘要），
// 并渲染成不可变、可精确查证的 Markdown 块。

// PayloadSchemaVersion 是 payload JSON 的结构版本。
const PayloadSchemaVersion = 1

// ReportInput 是 AI 开发结束、要同步到「待拍板」清单的一次交付报告。
type ReportInput struct {
	RepoID int64
	// RepoFullName 是 owner/name（写进条目，方便人直接看懂来源）。
	RepoFullName string
	WorkItemID   int64
	TaskID       int64
	Phase        string
	ImplRound    int
	Issues       []int
	// PRNumber、DesignPR 是入队时已经知道的工作项 PR（首次交付时可能还没有；绝不在这里调用 GitHub 补齐）。
	PRNumber *int
	DesignPR *int
	// Fix 表示这次执行是返修（no_change 在返修里实际转 blocked）。
	Fix bool
	// ReportStatus、TaskOutcome 是交付报告的状态和任务最终结果（payload 里显式记录）。
	ReportStatus string
	TaskOutcome  string
	// Raw 是交付报告的原始字节；Report 是已经校验过的报告。
	Raw    []byte
	Report *devreport.Delivery
}

// ExternalInput 是 payload 里记录的外部输入。
type ExternalInput struct {
	What   string `json:"what"`
	Blocks string `json:"blocks"`
	Resume string `json:"resume"`
}

// Payload 是入队时冻结的条目内容（jsonb 列），用于排查、Dashboard 展示和人工对账。
type Payload struct {
	SchemaVersion int       `json:"schema_version"`
	AcceptedAt    time.Time `json:"accepted_at"`
	Repo          string    `json:"repo"`
	WorkItemID    int64     `json:"work_item_id"`
	TaskID        int64     `json:"task_id"`
	Phase         string    `json:"phase"`
	ImplRound     int       `json:"impl_round"`
	Issues        []int     `json:"issues"`
	PRNumber      *int      `json:"pr_number"`
	DesignPR      *int      `json:"design_pr"`
	ReportStatus  string    `json:"report_status"`
	TaskOutcome   string    `json:"task_outcome"`
	EntryKind     EntryKind `json:"entry_kind"`

	// 决策记录。
	Category       string          `json:"category,omitempty"`
	Ref            string          `json:"ref,omitempty"`
	Context        string          `json:"context,omitempty"`
	Options        string          `json:"options,omitempty"`
	Choice         string          `json:"choice,omitempty"`
	Impact         string          `json:"impact,omitempty"`
	Review         string          `json:"review,omitempty"`
	Documents      []string        `json:"documents,omitempty"`
	ExternalInputs []ExternalInput `json:"external_inputs,omitempty"`

	// 受阻摘要。
	Summary   string `json:"summary,omitempty"`
	Remaining string `json:"remaining,omitempty"`
	Next      string `json:"next,omitempty"`
}

// ReportSHA256 返回交付报告规范化字节的 SHA-256：压掉 JSON 中的空白，避免同一份报告因格式差异得到不同的摘要。
func ReportSHA256(raw []byte) (string, error) {
	compact, err := compactJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(compact)
	return hex.EncodeToString(sum[:]), nil
}

// compactJSON 去掉 JSON 中不影响语义的空白。报告已经过严格校验，这里只用于得到稳定的摘要。
func compactJSON(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("规范化交付报告：%w", err)
	}
	return buf.Bytes(), nil
}

// entries 把一份报告拆成条目，顺序不可变：先按原数组下标排列每条决策，最后（需要时）是受阻摘要。
func entries(in *ReportInput, now time.Time) []Payload {
	base := Payload{
		SchemaVersion: PayloadSchemaVersion,
		AcceptedAt:    now.UTC(),
		Repo:          in.RepoFullName,
		WorkItemID:    in.WorkItemID,
		TaskID:        in.TaskID,
		Phase:         in.Phase,
		ImplRound:     in.ImplRound,
		Issues:        in.Issues,
		PRNumber:      in.PRNumber,
		DesignPR:      in.DesignPR,
		ReportStatus:  in.ReportStatus,
		TaskOutcome:   in.TaskOutcome,
	}
	rep := in.Report
	out := make([]Payload, 0, len(rep.Design.Decisions)+1)
	for _, d := range rep.Design.Decisions {
		p := base
		p.EntryKind = EntryDecision
		p.Category = d.Category
		p.Ref = d.Ref
		p.Context = d.Context
		p.Options = d.Options
		p.Choice = d.Choice
		p.Impact = d.Impact
		p.Review = d.Review
		p.Documents = rep.Design.Documents
		p.ExternalInputs = externalInputs(rep.Design.ExternalInputs)
		out = append(out, p)
	}
	if blockedEntryNeeded(in) {
		p := base
		p.EntryKind = EntryBlocked
		p.Summary = rep.Summary
		p.Remaining = rep.Handoff.Remaining
		p.Next = rep.Handoff.Next
		p.ExternalInputs = externalInputs(rep.Design.ExternalInputs)
		out = append(out, p)
	}
	return out
}

// blockedEntryNeeded 判断是否要额外记一条受阻摘要：报告本身受阻（无论有没有决策记录），或者 no_change 在返修 / 设计阶段
// 实际转成了受阻（初次实现阶段的 no_change 是正常终态，不加）。
func blockedEntryNeeded(in *ReportInput) bool {
	if in.Report.Status == devreport.StatusBlocked {
		return true
	}
	return in.Report.Status == devreport.StatusNoChange && (in.Fix || in.Phase == devreport.PhaseDesign)
}

func externalInputs(in []devreport.ExternalInput) []ExternalInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]ExternalInput, 0, len(in))
	for _, x := range in {
		out = append(out, ExternalInput{What: x.What, Blocks: x.Blocks, Resume: x.Resume})
	}
	return out
}

// categoryLabel 返回类别在文档里的中文说明。
func categoryLabel(c string) string {
	switch c {
	case devreport.DecisionConfirmed:
		return "改动已确认的决定（需人确认）"
	case devreport.DecisionAcceptance:
		return "验收口径"
	case devreport.DecisionPolicy:
		return "策略"
	case devreport.DecisionOther:
		return "其他"
	}
	return c
}

// RenderEntry 把一个条目渲染成 Markdown 块。marker 由 Server 生成；payload 里的内容全部是不可信输入，放进引用块并转义。
func RenderEntry(marker string, p Payload) string {
	var b strings.Builder
	switch p.EntryKind {
	case EntryBlocked:
		b.WriteString("#### 待拍板 · 受阻摘要\n\n")
	default:
		b.WriteString("#### 待拍板 · 决策记录\n\n")
	}
	b.WriteString("`" + marker + "`\n\n")
	b.WriteString("**来源**：" + sourceLine(p) + "\n\n")
	if p.EntryKind == EntryDecision {
		line := "**类别**：" + categoryLabel(p.Category)
		if p.Category == devreport.DecisionConfirmed {
			ref := cleanInline(p.Ref)
			if ref == "" {
				ref = "未注明"
			}
			line += " · 改动已确认的决定 " + ref + "，会影响 `kiln:hold` 门禁：需要人确认"
		}
		b.WriteString(line + "\n\n")
		b.WriteString(section("上下文", p.Context))
		b.WriteString(section("备选方案", p.Options))
		b.WriteString(section("当前选择与理由", p.Choice))
		b.WriteString(section("影响与复盘要点", p.Impact+"\n\n"+p.Review))
		if len(p.Documents) > 0 {
			b.WriteString(section("关联文档", strings.Join(p.Documents, "\n")))
		}
		b.WriteString(externalInputsSection(p.ExternalInputs))
	} else {
		b.WriteString(section("摘要", p.Summary))
		b.WriteString(section("剩余事项", p.Remaining))
		b.WriteString(section("下一步", p.Next))
		b.WriteString(externalInputsSection(p.ExternalInputs))
	}
	b.WriteString("---\n")
	return strings.TrimRight(b.String(), "\n")
}

// sourceLine 渲染来源行：仓库、工作项 / 任务 / Issue / PR、阶段与轮次、报告与任务状态、受理时间。
func sourceLine(p Payload) string {
	var parts []string
	if p.Repo != "" {
		parts = append(parts, cleanInline(p.Repo))
	}
	parts = append(parts, fmt.Sprintf("工作项 #%d", p.WorkItemID), fmt.Sprintf("Task #%d", p.TaskID))
	if len(p.Issues) > 0 {
		nums := make([]string, 0, len(p.Issues))
		for _, n := range p.Issues {
			nums = append(nums, fmt.Sprintf("#%d", n))
		}
		parts = append(parts, "Issue "+strings.Join(nums, "、"))
	}
	switch {
	case p.PRNumber != nil:
		parts = append(parts, fmt.Sprintf("PR #%d", *p.PRNumber))
	case p.DesignPR != nil:
		parts = append(parts, fmt.Sprintf("设计 PR #%d", *p.DesignPR))
	default:
		parts = append(parts, "PR 尚未创建/未关联")
	}
	phase := p.Phase
	if phase == "" {
		phase = "未知"
	}
	parts = append(parts, fmt.Sprintf("阶段 %s 第 %d 轮", phase, max(p.ImplRound, 1)))
	parts = append(parts, fmt.Sprintf("报告 %s（任务 %s）", p.ReportStatus, p.TaskOutcome))
	parts = append(parts, p.AcceptedAt.Format("2006-01-02 15:04 MST"))
	return strings.Join(parts, " · ")
}

func externalInputsSection(in []ExternalInput) string {
	if len(in) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**需要人提供的条件**\n\n")
	for _, x := range in {
		b.WriteString(quoteBlock(fmt.Sprintf("- %s（阻塞：%s；恢复条件：%s）", x.What, x.Blocks, x.Resume)) + "\n\n")
	}
	return b.String()
}

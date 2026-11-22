// Package devreport 是 AI 开发（PLAN #26.10）的选题报告和交付报告：Agent 按 JSON Schema 写出，AI 执行节点严格校验后提交，
// Server 再校验一次（不信任 Runner 之外的内容）。两者共用这里的格式与规则。
//
// 报告是 Agent 的自述：其中的验证命令、验收证据都是"Agent 自报"，不是执行证据（#26.14）；交付的 commit、分支和改动由
// Runner 用 git、Server 用 GitHub API 另行核对。
package devreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Version 是报告格式的版本。
const Version = 2

// 工作项的阶段（PLAN #31.3）：先设计时先交付设计 PR（design），合并后实现（implement）；一个 PR 完成时只有 implement。
const (
	PhaseDesign    = "design"
	PhaseImplement = "implement"
)

// MaxSize 是一份报告的大小上限。
const MaxSize = 256 << 10

// 交付的状态（Delivery.Status）。
const (
	// StatusDelivered：有改动（commit），交给 Server 推进为 PR。
	StatusDelivered = "delivered"
	// StatusNoChange：不需要改代码（例如需求已经满足），每个 Issue 写明原因。
	StatusNoChange = "no_change"
	// StatusBlocked：无法继续（缺少条件、需求不清等），写明原因和恢复条件，交给人。
	StatusBlocked = "blocked"
)

// Issue 的完成度（IssueResult.Resolution）。只有 full 的 Issue 在 PR 中写 Fixes #N（合并后关闭）。
const (
	ResolutionFull    = "full"
	ResolutionPartial = "partial"
	ResolutionNone    = "none"
)

// 问题的处置（Disposition.Action）。处置只是开发方的说法：阻塞问题是否解决由独立审计在新 head 上复核。
const (
	ActionFixed     = "fixed"
	ActionRejected  = "rejected"
	ActionDuplicate = "duplicate"
	ActionUnable    = "unable"
)

// 验证命令的结果（Verification.Outcome，Agent 自报）。
const (
	OutcomePassed = "passed"
	OutcomeFailed = "failed"
	OutcomeNotRun = "not_run"
)

// Skip 是一个未选的候选 Issue。
type Skip struct {
	Number int    `json:"number"`
	Reason string `json:"reason"`
	// Reconsider 是重新评估的条件（例如"需求补齐验收标准后"）。
	Reconsider string `json:"reconsider"`
	// Later 为真表示 Issue 本身可以做，只是不放进这一批（与这一批无关、超过一批的数量、与进行中的工作项会改到同一处）：
	// 留在任务池中，之后的选题（可能在另一台机器上）还会看到它。为假表示现在不适合做（超出授权、缺少人才能提供的输入
	// 而且连设计都无法推进），内容不变就不再进入候选。
	Later bool `json:"later"`
	// WaitFor 是它依赖、还没有完成的 Issue（PLAN #31.6）：这些 Issue 都关闭后自动回到候选。与 later=true 不能同时写。
	WaitFor []int `json:"wait_for"`
}

// UnmarshalJSON 要求 later 写明 true 或 false：缺少或为 null 时不能当作 false（那会写跳过记录，把只是延后的 Issue
// 排除出之后的候选）。
func (k *Skip) UnmarshalJSON(data []byte) error {
	var r struct {
		Number     int    `json:"number"`
		Reason     string `json:"reason"`
		Reconsider string `json:"reconsider"`
		Later      *bool  `json:"later"`
		WaitFor    []int  `json:"wait_for"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return err
	}
	if r.Later == nil {
		return fmt.Errorf("skipped 中 #%d 缺少 later（true：只是不放进这一批，留给之后的选题；false：现在不适合做）", r.Number)
	}
	*k = Skip{Number: r.Number, Reason: r.Reason, Reconsider: r.Reconsider, Later: *r.Later, WaitFor: r.WaitFor}
	return nil
}

// Selection 是选题报告。
type Selection struct {
	Version int `json:"version"`
	// Selected 是要领取的 Issue（一批，有内聚目标，一个 PR）；为空表示没有合适的。
	Selected    []int  `json:"selected"`
	BatchReason string `json:"batch_reason"`
	Plan        string `json:"plan"`
	// DesignFirst 是这一批的交付方式（PLAN #31.4）：false 在同一次执行中先设计再开发、一个 PR 完成；true 先交付设计 PR，
	// 合并后再实现。选了 Issue 时必须写明（缺少时报告无效）。DesignReason 是理由。
	DesignFirst  *bool  `json:"design_first"`
	DesignReason string `json:"design_reason"`
	// Skipped 是每个未选候选的原因（每个未选的候选都要有）。
	Skipped []Skip `json:"skipped"`
}

// FirstPhase 返回领取后工作项的起始阶段。
func (s *Selection) FirstPhase() string {
	if s.DesignFirst != nil && *s.DesignFirst {
		return PhaseDesign
	}
	return PhaseImplement
}

// AcceptanceItem 是一个验收项的实现与验证（Agent 自报）。
type AcceptanceItem struct {
	Item     string `json:"item"`
	Done     bool   `json:"done"`
	Evidence string `json:"evidence"`
}

// IssueResult 是一个 Issue 的结果。
type IssueResult struct {
	Number     int              `json:"number"`
	Resolution string           `json:"resolution"`
	Acceptance []AcceptanceItem `json:"acceptance"`
	Notes      string           `json:"notes"`
}

// Verification 是一条验证命令（Agent 自报，不是执行证据）。
type Verification struct {
	Command string `json:"command"`
	Outcome string `json:"outcome"`
	Note    string `json:"note"`
}

// Disposition 是对一个审计问题的处置。
type Disposition struct {
	Finding string `json:"finding"`
	Action  string `json:"action"`
	// Commit 是修复的 commit（fixed 时必填）。
	Commit string `json:"commit"`
	// DuplicateOf 是重复的另一个问题（duplicate 时必填）。
	DuplicateOf string `json:"duplicate_of"`
	// Reason 是说明：修复了什么、不采纳的依据、无法完成的原因。
	Reason string `json:"reason"`
}

// Handoff 是交给下一轮的交接（换机器、CLI 会话丢失后仍能继续同一个 PR）。
type Handoff struct {
	Done      string `json:"done"`
	Remaining string `json:"remaining"`
	Decisions string `json:"decisions"`
	Next      string `json:"next"`
}

// 决定的类别（Decision.Category，PLAN #31.4）。只有 confirmed_decision（改已确认的决定）让 PR 暂停等人确认。
const (
	DecisionConfirmed  = "confirmed_decision"
	DecisionAcceptance = "acceptance"
	DecisionPolicy     = "policy"
	DecisionOther      = "other"
)

// Decision 是一个有疑问的决定的决策记录（供复盘，Agent 自报）。
type Decision struct {
	Category string `json:"category"`
	// Ref 是被改变的已确认决定的编号（confirmed_decision 时必填，例如 C20）。
	Ref string `json:"ref"`
	// Context 是现状和依据；Options 是考虑过的方案；Choice 是选择及理由；Impact 是影响；Review 是复盘时要看什么。
	Context string `json:"context"`
	Options string `json:"options"`
	Choice  string `json:"choice"`
	Impact  string `json:"impact"`
	Review  string `json:"review"`
}

// ExternalInput 是真实验证需要、只有人能提供的条件（项目、凭据、费用）。
type ExternalInput struct {
	What string `json:"what"`
	// Blocks 是它阻塞的验收项；Resume 是恢复的条件。
	Blocks string `json:"blocks"`
	Resume string `json:"resume"`
}

// Design 是交付的设计段（PLAN #31.4）：设计 PR 必须有；一个 PR 完成时有设计文档、决定或外部输入时写。
type Design struct {
	// Documents 是设计文档的路径（Markdown，Server 核对它们在 PR 的改动中）。
	Documents      []string        `json:"documents"`
	Decisions      []Decision      `json:"decisions"`
	ExternalInputs []ExternalInput `json:"external_inputs"`
}

// Deviation 是实现与已合并设计的出入（小的出入写在这里，方向性的冲突写 blocked）。
type Deviation struct {
	Item   string `json:"item"`
	Reason string `json:"reason"`
}

// Delivery 是交付报告。
type Delivery struct {
	Version      int            `json:"version"`
	Status       string         `json:"status"`
	Summary      string         `json:"summary"`
	PRTitle      string         `json:"pr_title"`
	Issues       []IssueResult  `json:"issues"`
	Verification []Verification `json:"verification"`
	Dispositions []Disposition  `json:"dispositions"`
	Handoff      Handoff        `json:"handoff"`
	Design       Design         `json:"design"`
	// DesignDeviations 是实现与已合并设计的出入（先设计时的实现）。
	DesignDeviations []Deviation `json:"design_deviations"`
}

// HasConfirmedDecision 判断交付是否声明了要改已确认的决定（PR 要暂停等人确认）。
func (d *Delivery) HasConfirmedDecision() bool {
	return slices.ContainsFunc(d.Design.Decisions, func(x Decision) bool { return x.Category == DecisionConfirmed })
}

// decodeStrict 严格解码：未知字段、多余的内容、超过大小上限都报错。
func decodeStrict(data []byte, v any) error {
	if len(data) > MaxSize {
		return fmt.Errorf("报告超过 %d KiB", MaxSize>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("报告不是有效的 JSON：%w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("报告只能是一个 JSON 对象")
	}
	return nil
}

// ParseSelection 解析并校验选题报告。candidates 是本次的候选 Issue，max 是一批最多领取的数量。
// version 由 Runner 写入：data 中没有 version 时按当前版本处理。
func ParseSelection(data []byte, candidates []int, maxIssues int) (*Selection, error) {
	var s Selection
	if err := decodeStrict(data, &s); err != nil {
		return nil, err
	}
	if s.Version == 0 {
		s.Version = Version
	}
	return &s, s.Validate(candidates, maxIssues)
}

// Validate 校验选题报告：选中的 Issue 必须是候选、不重复、不超过上限；选了就要有计划；每个未选的候选都要说明原因。
func (s *Selection) Validate(candidates []int, maxIssues int) error {
	if s.Version != Version {
		return fmt.Errorf("报告版本 %d 不受支持（应为 %d）", s.Version, Version)
	}
	seen := map[int]bool{}
	for _, n := range s.Selected {
		switch {
		case !slices.Contains(candidates, n):
			return fmt.Errorf("selected 中的 #%d 不是候选 Issue", n)
		case seen[n]:
			return fmt.Errorf("selected 中的 #%d 重复", n)
		}
		seen[n] = true
	}
	if len(s.Selected) > maxIssues {
		return fmt.Errorf("一批最多领取 %d 个 Issue，选了 %d 个", maxIssues, len(s.Selected))
	}
	if len(s.Selected) > 0 && strings.TrimSpace(s.Plan) == "" {
		return errors.New("选了 Issue 时 plan（实施计划）不能为空")
	}
	if len(s.Selected) > 1 && strings.TrimSpace(s.BatchReason) == "" {
		return errors.New("一次选多个 Issue 时 batch_reason（合批理由）不能为空")
	}
	if len(s.Selected) > 0 && (s.DesignFirst == nil || strings.TrimSpace(s.DesignReason) == "") {
		return errors.New("选了 Issue 时要写明 design_first（true：先交付设计 PR；false：一个 PR 完成）和 design_reason")
	}
	skipped := map[int]bool{}
	for _, k := range s.Skipped {
		switch {
		case !slices.Contains(candidates, k.Number):
			return fmt.Errorf("skipped 中的 #%d 不是候选 Issue", k.Number)
		case seen[k.Number]:
			return fmt.Errorf("#%d 同时在 selected 和 skipped 中", k.Number)
		case skipped[k.Number]:
			return fmt.Errorf("skipped 中的 #%d 重复", k.Number)
		case strings.TrimSpace(k.Reason) == "":
			return fmt.Errorf("skipped 中的 #%d 没有写原因", k.Number)
		case k.Later && len(k.WaitFor) > 0:
			return fmt.Errorf("skipped 中的 #%d 不能同时写 later=true 和 wait_for（等待依赖写 later=false）", k.Number)
		case slices.ContainsFunc(k.WaitFor, func(n int) bool { return n <= 0 || n == k.Number }):
			return fmt.Errorf("skipped 中 #%d 的 wait_for %v 无效（Issue 编号，不能是它自己）", k.Number, k.WaitFor)
		}
		skipped[k.Number] = true
	}
	for _, n := range candidates {
		if !seen[n] && !skipped[n] {
			return fmt.Errorf("候选 #%d 既没有选也没有写不选的原因（skipped）", n)
		}
	}
	return nil
}

// Marshal 返回报告的 JSON。
func Marshal(v any) []byte {
	b, _ := json.Marshal(v) //nolint:errchkjson // 只有字符串、整数、布尔和它们的切片，不会失败
	return b
}

// Expect 是交付报告要回应的内容。
type Expect struct {
	// Issues 是工作项的 Issue；Findings 是本轮要处置的审计问题（返修时）。
	Issues   []int
	Findings []string
	// Phase 是工作项的阶段：design 时交付的是设计（每个 Issue 都是 none，必须有设计文档）。
	Phase string
}

// ParseDelivery 解析并校验交付报告。
func ParseDelivery(data []byte, e Expect) (*Delivery, error) {
	var d Delivery
	if err := decodeStrict(data, &d); err != nil {
		return nil, err
	}
	if d.Version == 0 {
		d.Version = Version
	}
	return &d, d.Validate(e)
}

// Validate 校验交付报告（PLAN #26.10、#26.11）：
//   - 每个 Issue 恰好一个结果；full 要有验收项且全部完成，partial 要有完成和未完成的验收项；
//   - delivered 至少有一个 Issue 是 full 或 partial；no_change 的每个 Issue 都是 none 并写明原因；blocked 要写明还剩什么；
//   - 返修时每个问题恰好一个处置：fixed 写 commit，duplicate 指向本轮的另一个问题，都要写说明；
//   - 部分完成不能写成全部完成（full 要求全部验收项完成）；
//   - 设计阶段（PLAN #31.4）：delivered 的每个 Issue 都是 none（设计不解决 Issue），至少一个设计文档（*.md）；
//   - 决策记录写明类别和选择，改已确认的决定时写明编号；外部输入写明是什么。
func (d *Delivery) Validate(e Expect) error {
	issues, findings := e.Issues, e.Findings
	if d.Version != Version {
		return fmt.Errorf("报告版本 %d 不受支持（应为 %d）", d.Version, Version)
	}
	if !slices.Contains([]string{StatusDelivered, StatusNoChange, StatusBlocked}, d.Status) {
		return fmt.Errorf("status %q 无效（delivered、no_change、blocked）", d.Status)
	}
	if strings.TrimSpace(d.Summary) == "" {
		return errors.New("summary 不能为空")
	}
	if err := d.validateIssues(issues); err != nil {
		return err
	}
	if err := d.validateDispositions(findings); err != nil {
		return err
	}
	if err := d.validateDesign(); err != nil {
		return err
	}
	for i, v := range d.Verification {
		if strings.TrimSpace(v.Command) == "" || !slices.Contains([]string{OutcomePassed, OutcomeFailed, OutcomeNotRun}, v.Outcome) {
			return fmt.Errorf("verification[%d]：command 不能为空，outcome 只能是 passed、failed、not_run", i)
		}
	}
	switch d.Status {
	case StatusDelivered:
		if strings.TrimSpace(d.PRTitle) == "" {
			return errors.New("delivered 时 pr_title 不能为空")
		}
		if e.Phase == PhaseDesign {
			if slices.ContainsFunc(d.Issues, func(r IssueResult) bool { return r.Resolution != ResolutionNone }) {
				return errors.New("设计阶段的交付不解决 Issue：每个 Issue 的 resolution 都写 none（实现阶段再写完成度）")
			}
			if len(d.Design.Documents) == 0 {
				return errors.New("设计阶段的交付要在 design.documents 中列出设计文档")
			}
			break
		}
		if !slices.ContainsFunc(d.Issues, func(r IssueResult) bool { return r.Resolution != ResolutionNone }) {
			return errors.New("delivered 时至少一个 Issue 是 full 或 partial（都没有完成应写 blocked 或 no_change）")
		}
	case StatusNoChange:
		for _, r := range d.Issues {
			if r.Resolution != ResolutionNone || strings.TrimSpace(r.Notes) == "" {
				return fmt.Errorf("no_change 时每个 Issue 的 resolution 都是 none，并在 notes 中写明不需要改代码的原因（#%d）", r.Number)
			}
		}
	case StatusBlocked:
		if strings.TrimSpace(d.Handoff.Remaining) == "" {
			return errors.New("blocked 时 handoff.remaining 要写明还剩什么、需要什么条件才能继续")
		}
	}
	return nil
}

// validateDesign 校验设计段和与设计的出入。
func (d *Delivery) validateDesign() error {
	for i, p := range d.Design.Documents {
		if !IsMarkdown(p) || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
			return fmt.Errorf("design.documents[%d] %q 要是仓库中的 Markdown 文件（相对路径，*.md）", i, p)
		}
	}
	for i, x := range d.Design.Decisions {
		switch {
		case !slices.Contains([]string{DecisionConfirmed, DecisionAcceptance, DecisionPolicy, DecisionOther}, x.Category):
			return fmt.Errorf("design.decisions[%d] 的 category %q 无效（confirmed_decision、acceptance、policy、other）", i, x.Category)
		case strings.TrimSpace(x.Choice) == "":
			return fmt.Errorf("design.decisions[%d] 要写 choice（选择及理由）", i)
		case x.Category == DecisionConfirmed && strings.TrimSpace(x.Ref) == "":
			return fmt.Errorf("design.decisions[%d] 改已确认的决定时要在 ref 中写明编号（例如 C20）", i)
		}
	}
	for i, x := range d.Design.ExternalInputs {
		if strings.TrimSpace(x.What) == "" {
			return fmt.Errorf("design.external_inputs[%d] 要写 what（需要人提供什么）", i)
		}
	}
	for i, x := range d.DesignDeviations {
		if strings.TrimSpace(x.Item) == "" || strings.TrimSpace(x.Reason) == "" {
			return fmt.Errorf("design_deviations[%d] 要写 item 和 reason", i)
		}
	}
	return nil
}

// IsMarkdown 判断路径是否是 Markdown 文件（设计 PR 只能改这些，I12）。
func IsMarkdown(path string) bool { return strings.HasSuffix(strings.ToLower(path), ".md") }

func (d *Delivery) validateIssues(issues []int) error {
	seen := map[int]bool{}
	for _, r := range d.Issues {
		switch {
		case !slices.Contains(issues, r.Number):
			return fmt.Errorf("issues 中的 #%d 不属于这个工作项", r.Number)
		case seen[r.Number]:
			return fmt.Errorf("issues 中的 #%d 重复", r.Number)
		}
		seen[r.Number] = true
		done, todo := 0, 0
		for _, a := range r.Acceptance {
			if strings.TrimSpace(a.Item) == "" {
				return fmt.Errorf("#%d 的验收项不能为空", r.Number)
			}
			if a.Done {
				if strings.TrimSpace(a.Evidence) == "" {
					return fmt.Errorf("#%d 已完成的验收项 %q 要写 evidence（实现与验证）", r.Number, a.Item)
				}
				done++
			} else {
				todo++
			}
		}
		switch r.Resolution {
		case ResolutionFull:
			if done == 0 || todo > 0 {
				return fmt.Errorf("#%d 是 full 时验收项要全部完成（完成 %d、未完成 %d）；部分完成写 partial", r.Number, done, todo)
			}
		case ResolutionPartial:
			if done == 0 || todo == 0 {
				return fmt.Errorf("#%d 是 partial 时要同时有完成和未完成的验收项（完成 %d、未完成 %d）", r.Number, done, todo)
			}
		case ResolutionNone:
		default:
			return fmt.Errorf("#%d 的 resolution %q 无效（full、partial、none）", r.Number, r.Resolution)
		}
	}
	for _, n := range issues {
		if !seen[n] {
			return fmt.Errorf("缺少 #%d 的结果", n)
		}
	}
	return nil
}

func (d *Delivery) validateDispositions(findings []string) error {
	seen := map[string]bool{}
	for _, p := range d.Dispositions {
		switch {
		case !slices.Contains(findings, p.Finding):
			return fmt.Errorf("dispositions 中的 %q 不是本轮要处理的问题", p.Finding)
		case seen[p.Finding]:
			return fmt.Errorf("dispositions 中的 %s 重复", p.Finding)
		case strings.TrimSpace(p.Reason) == "":
			return fmt.Errorf("%s 的处置要写 reason", p.Finding)
		}
		seen[p.Finding] = true
		switch p.Action {
		case ActionFixed:
			if strings.TrimSpace(p.Commit) == "" {
				return fmt.Errorf("%s 是 fixed 时要写修复的 commit", p.Finding)
			}
		case ActionDuplicate:
			if p.DuplicateOf == p.Finding || !slices.Contains(findings, p.DuplicateOf) {
				return fmt.Errorf("%s 是 duplicate 时 duplicate_of 要指向本轮的另一个问题", p.Finding)
			}
		case ActionRejected, ActionUnable:
		default:
			return fmt.Errorf("%s 的 action %q 无效（fixed、rejected、duplicate、unable）", p.Finding, p.Action)
		}
	}
	for _, f := range findings {
		if !seen[f] {
			return fmt.Errorf("缺少问题 %s 的处置（每个问题都要有：fixed、rejected、duplicate 或 unable）", f)
		}
	}
	return nil
}

// FullyResolved 返回 full 的 Issue（合并后关闭，PR 中写 Fixes #N）。
func (d *Delivery) FullyResolved() []int {
	var out []int
	for _, r := range d.Issues {
		if r.Resolution == ResolutionFull {
			out = append(out, r.Number)
		}
	}
	return out
}

// Resolution 返回 Issue n 的完成度（没有时为空）。
func (d *Delivery) Resolution(n int) string {
	for _, r := range d.Issues {
		if r.Number == n {
			return r.Resolution
		}
	}
	return ""
}

// AllFixed 判断是否每个处置都是 fixed（或指向 fixed 的 duplicate）。
func (d *Delivery) AllFixed() bool {
	act := map[string]string{}
	for _, p := range d.Dispositions {
		act[p.Finding] = p.Action
	}
	for _, p := range d.Dispositions {
		a := p.Action
		if a == ActionDuplicate {
			a = act[p.DuplicateOf]
		}
		if a != ActionFixed {
			return false
		}
	}
	return true
}

// 分支（PLAN #26.8、#31.3）：工作项的正式分支只由 Server 快进；一次执行的交付或 checkpoint 推到 attempt 分支，
// Server 推进后或 12 小时后删除。两者不能互为前缀（git 中 kiln/dev-5 与 kiln/dev-5/a1 不能同时存在）。正式分支带阶段
// 和轮次：设计 kiln/design-<id>，第一轮实现 kiln/dev-<id>，之后的轮次 kiln/dev-<id>-r<n>。一个分支只属于一轮。
const (
	WorkItemBranchPrefix = "kiln/dev-"
	DesignBranchPrefix   = "kiln/design-"
	AttemptBranchPrefix  = "kiln/attempt-"
	roundSep             = "-r"
)

// BranchRef 是正式分支所属的工作项、阶段和实现轮次（设计阶段为 1）。
type BranchRef struct {
	ID    int64
	Phase string
	Round int
}

// WorkItemBranch 返回工作项 id 在阶段 phase、实现第 round 轮的正式分支。
func WorkItemBranch(id int64, phase string, round int) string {
	switch {
	case phase == PhaseDesign:
		return fmt.Sprintf("%s%d", DesignBranchPrefix, id)
	case round > 1:
		return fmt.Sprintf("%s%d%s%d", WorkItemBranchPrefix, id, roundSep, round)
	}
	return fmt.Sprintf("%s%d", WorkItemBranchPrefix, id)
}

// ParseWorkItemBranch 返回正式分支 branch 所属的工作项、阶段和轮次；不是正式分支时 ok 为假。
func ParseWorkItemBranch(branch string) (ref BranchRef, ok bool) {
	phase, rest := PhaseImplement, ""
	switch {
	case strings.HasPrefix(branch, DesignBranchPrefix):
		phase, rest = PhaseDesign, strings.TrimPrefix(branch, DesignBranchPrefix)
	case strings.HasPrefix(branch, WorkItemBranchPrefix):
		rest = strings.TrimPrefix(branch, WorkItemBranchPrefix)
	default:
		return BranchRef{}, false
	}
	idPart, roundPart, hasRound := strings.Cut(rest, roundSep)
	round := 1
	if hasRound {
		n, err := strconv.Atoi(roundPart)
		if err != nil {
			return BranchRef{}, false
		}
		round = n
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 || WorkItemBranch(id, phase, round) != branch {
		return BranchRef{}, false
	}
	return BranchRef{ID: id, Phase: phase, Round: round}, true
}

// AttemptBranch 返回 attempt 的分支。
func AttemptBranch(attemptID int64) string {
	return fmt.Sprintf("%s%d", AttemptBranchPrefix, attemptID)
}

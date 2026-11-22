// Package aireport 定义 AI 审计报告的格式：Agent 按 Schema 写出 JSON，AI 执行节点清洗、校验后随
// Complete 提交，Server 再次校验后保存到 ai_reviews，并据此发布 PR review。
//
// 报告是 Agent 生成的内容，Server 不信任它：解析时拒绝未知字段、超长内容和越界路径，发布前再清洗本机路径。
package aireport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Version 是报告格式的版本（AiReviewSpec.report_version）。AI 执行节点把它写进报告的 version 字段。
//
// v1（version 为 0 或 1）：finding 只有 title、body 等，commands_run 没有 outcome。
// v2：finding 用 scenario、actual、expected、impact、proof 写清触发条件、实际与预期行为、影响和证据；
// 增加 scope、checks（精简的验证结果）、env_notes、preexisting；commands_run 的每条命令标明 outcome。
// Server 仍然接受 v1 的报告（已保存的报告、升级前领取的任务）。
const Version = 2

// 审计结论。
const (
	VerdictNoIssues    = "no_issues"
	VerdictIssuesFound = "issues_found"
	VerdictIncomplete  = "incomplete"
)

// 问题的严重程度与证据等级。
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"

	EvidenceReproduced = "reproduced"
	EvidenceCode       = "code_evidence"
	EvidenceSuspicion  = "suspicion"
)

// 验证项（Check）的类别与结果。
const (
	CheckChange   = "change"   // 本次改动（修复、新行为）是否达到目的
	CheckSuite    = "suite"    // 构建、静态检查、测试等整体验证
	CheckMutation = "mutation" // 变异验证：改坏关键逻辑后测试是否失败（passed 表示测试按预期失败）

	CheckPassed = "passed"
	CheckFailed = "failed"
	CheckNotRun = "not_run"
)

// 命令的结果（Command.Outcome）：退出码不能说明命令对结论意味着什么，由 Agent 标明。
const (
	OutcomePassed          = "passed"           // 达到了目的（通常退出码为 0）
	OutcomeFailed          = "failed"           // 最终失败：没有解决的失败
	OutcomeEnvResolved     = "env_resolved"     // 环境问题导致的失败，之后已经解决（重跑通过）
	OutcomeExpectedFailure = "expected_failure" // 预期的失败（例如变异验证中改坏代码后测试失败）
	OutcomeNoMatch         = "no_match"         // 搜索没有匹配（例如确认没有被跳过的测试）
	OutcomeExplore         = "explore"          // 探索性操作（读文件、列目录等），结果不影响结论
)

// 大小上限。
const (
	// MaxSize 是报告 JSON 的大小上限。
	MaxSize = 512 << 10
	// MaxFindings、MaxCommands、MaxUnverified 是各列表的长度上限。
	MaxFindings    = 100
	MaxCommands    = 200
	MaxUnverified  = 100
	MaxChecks      = 100
	MaxEnvNotes    = 50
	MaxPreexisting = 50
	// maxText 是单个文本字段的长度上限（字节）。
	maxText = 32 << 10
)

// Command 是 Agent 执行过的一条命令（完整的执行记录，只在 Kiln 中展示）。
type Command struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	// Outcome 是命令对结论意味着什么（Outcome* 常量，v2 必填）。
	Outcome string `json:"outcome"`
	Note    string `json:"note"`
}

// Finding 是一个问题。Path、Line 指向被审 commit 中的文件和行（仓库相对路径；Line 为 0 表示不针对具体行）。
type Finding struct {
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	// Scenario 是触发条件或示例（前置条件 → 触发步骤）；Actual、Expected 是实际与预期的行为；Impact 是影响；
	// Proof 是证据（复现的命令、关键断言和输出，或者静态分析的调用链）；Fix 是修复方向（可选）。v2 中除 Fix 外必填。
	Scenario string `json:"scenario"`
	Actual   string `json:"actual"`
	Expected string `json:"expected"`
	Impact   string `json:"impact"`
	Proof    string `json:"proof"`
	Fix      string `json:"fix"`
	// Body 在 v1 中是问题的全部说明（必填）；v2 中是可选的补充说明。
	Body string `json:"body"`
}

// Check 是一项重要的验证结果（GitHub review 中展示的精简验证摘要）。
type Check struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// Preexisting 是审计中顺带发现、与本次改动无关的已有问题（不是 finding，不影响结论）。
type Preexisting struct {
	Title  string `json:"title"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Detail string `json:"detail"`
}

// Report 是一次审计的结构化报告。
type Report struct {
	// Version 是报告格式的版本，由 AI 执行节点写入（Agent 不需要写）；0 表示 v1。
	Version int    `json:"version"`
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	// Scope 是审计的范围（例如"12 个变更文件及相关调用链"）。
	Scope       string        `json:"scope"`
	Findings    []Finding     `json:"findings"`
	Checks      []Check       `json:"checks"`
	Unverified  []string      `json:"unverified"`
	EnvNotes    []string      `json:"env_notes"`
	Preexisting []Preexisting `json:"preexisting"`
	CommandsRun []Command     `json:"commands_run"`
}

// V2 判断报告是否是 v2 及以后的格式。
func (r *Report) V2() bool {
	return r.Version >= 2
}

// Parse 严格解析报告：拒绝未知字段、尾随内容、超出上限的内容和不合法的取值；v2 报告还要求 Schema 中的必填字段都在
// （值可以是 null：kiln-ai-runner v0.1.36 及以前把空数组写成 null，已保存的报告也是这样）。
// Agent 写出的报告由 AI 执行节点用 CheckRequired 更严格地检查（null 也算缺少）。
func Parse(data []byte) (*Report, error) {
	r, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if r.V2() {
		if err := checkRequired(data, true); err != nil {
			return nil, err
		}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// ParseAgent 解析并校验 Agent 写出的报告（PLAN #33.5）：Agent 按当前 Schema 写，没有 version（version 由 Kiln 写入）；
// 必填字段都要写出（没有内容时写空数组，null 也算缺少），再按当前版本严格校验。Server 的 ValidateReport 和保存审计报告
// 都用它，规则只定义一次。
func ParseAgent(data []byte) (*Report, error) {
	r, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if err := CheckRequired(data); err != nil {
		return nil, err
	}
	r.Version = Version
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// CheckRequired 检查 Agent 写出的报告 JSON 是否有当前 Schema 的全部必填字段（包括每个 finding、check 等中的字段）。
// 值为 null 也算缺少；没有内容的数组要写成 []。Go 解码时缺少的字段是零值，Validate 无法区分“没写”和“写了空值”，
// 所以在解码前按原始 JSON 检查。
func CheckRequired(data []byte) error {
	return checkRequired(data, false)
}

// maxMissingShown 是错误信息中最多列出的缺少字段数。
const maxMissingShown = 10

func checkRequired(data []byte, nullOK bool) error {
	var missing []string
	schemaRoot.missing(data, "", nullOK, &missing)
	if len(missing) == 0 {
		return nil
	}
	shown := missing[:min(len(missing), maxMissingShown)]
	more := ""
	if len(missing) > len(shown) {
		more = fmt.Sprintf(" 等 %d 个", len(missing))
	}
	return fmt.Errorf("缺少必填字段：%s%s", strings.Join(shown, "、"), more)
}

// schemaNode 是 JSON Schema 中检查必填字段用到的部分。
type schemaNode struct {
	Required   []string               `json:"required"`
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
}

// schemaRoot 是解析后的报告 Schema（必填字段以 Schema 为准，不另外维护）。
var schemaRoot = func() *schemaNode {
	var n schemaNode
	if err := json.Unmarshal([]byte(schemaJSON), &n); err != nil {
		panic(err)
	}
	return &n
}()

// missing 把 data 中缺少的必填字段（以 at 为前缀的路径）追加到 out。data 的类型已经由 Decode 检查过，
// 类型不符的部分跳过。
func (n *schemaNode) missing(data json.RawMessage, at string, nullOK bool, out *[]string) {
	if n.Items != nil {
		var items []json.RawMessage
		if json.Unmarshal(data, &items) == nil {
			for i, it := range items {
				n.Items.missing(it, fmt.Sprintf("%s[%d]", at, i), nullOK, out)
			}
		}
		return
	}
	if len(n.Required) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	for _, k := range n.Required {
		p := k
		if at != "" {
			p = at + "." + k
		}
		v, ok := obj[k]
		if !ok || (!nullOK && isNull(v)) {
			*out = append(*out, p)
			continue
		}
		if c := n.Properties[k]; c != nil && !isNull(v) {
			c.missing(v, p, nullOK, out)
		}
	}
}

func isNull(v json.RawMessage) bool {
	return string(bytes.TrimSpace(v)) == "null"
}

// Decode 严格解码报告的 JSON（拒绝未知字段、尾随内容和超出上限的大小），不校验内容。
// AI 执行节点先把 Agent 写出的本机路径换成仓库相对路径（Rewrite），再 Validate。
func Decode(data []byte) (*Report, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("报告超过 %d KiB", MaxSize>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("报告不是有效的 JSON：%w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("报告在 JSON 之后还有多余的内容")
	}
	return &r, nil
}

// Validate 校验报告的内容（按报告的版本）。
func (r *Report) Validate() error {
	if r.Version < 0 || r.Version > Version {
		return fmt.Errorf("version 无效：%d", r.Version)
	}
	switch r.Verdict {
	case VerdictNoIssues, VerdictIssuesFound, VerdictIncomplete:
	default:
		return fmt.Errorf("verdict 无效：%q", r.Verdict)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("缺少 summary")
	}
	switch {
	case len(r.Findings) > MaxFindings:
		return fmt.Errorf("findings 超过 %d 个", MaxFindings)
	case len(r.CommandsRun) > MaxCommands:
		return fmt.Errorf("commands_run 超过 %d 条", MaxCommands)
	case len(r.Unverified) > MaxUnverified:
		return fmt.Errorf("unverified 超过 %d 条", MaxUnverified)
	case len(r.Checks) > MaxChecks:
		return fmt.Errorf("checks 超过 %d 条", MaxChecks)
	case len(r.EnvNotes) > MaxEnvNotes:
		return fmt.Errorf("env_notes 超过 %d 条", MaxEnvNotes)
	case len(r.Preexisting) > MaxPreexisting:
		return fmt.Errorf("preexisting 超过 %d 条", MaxPreexisting)
	case r.Verdict == VerdictIssuesFound && len(r.Findings) == 0:
		return errors.New("verdict 为 issues_found，但没有 findings")
	case r.Verdict == VerdictNoIssues && len(r.Findings) > 0:
		return errors.New("verdict 为 no_issues，但有 findings")
	}
	texts := []string{r.Summary, r.Scope}
	for i, c := range r.CommandsRun {
		if err := c.validate(r.V2()); err != nil {
			return fmt.Errorf("commands_run[%d]：%w", i, err)
		}
		texts = append(texts, c.Command, c.Note)
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		if err := f.validate(r.V2()); err != nil {
			return fmt.Errorf("findings[%d]：%w", i, err)
		}
		texts = append(texts, f.Title, f.Scenario, f.Actual, f.Expected, f.Impact, f.Proof, f.Fix, f.Body)
	}
	for i, c := range r.Checks {
		if err := c.validate(); err != nil {
			return fmt.Errorf("checks[%d]：%w", i, err)
		}
		texts = append(texts, c.Title, c.Detail)
	}
	for i, p := range r.Preexisting {
		if err := p.validate(); err != nil {
			return fmt.Errorf("preexisting[%d]：%w", i, err)
		}
		texts = append(texts, p.Title, p.Detail)
	}
	texts = append(texts, r.Unverified...)
	texts = append(texts, r.EnvNotes...)
	for _, t := range texts {
		if len(t) > maxText {
			return fmt.Errorf("文本超过 %d KiB", maxText>>10)
		}
		if !utf8.ValidString(t) {
			return errors.New("文本不是合法的 UTF-8")
		}
	}
	return nil
}

func (c *Command) validate(v2 bool) error {
	switch c.Outcome {
	case OutcomePassed, OutcomeFailed, OutcomeEnvResolved, OutcomeExpectedFailure, OutcomeNoMatch, OutcomeExplore:
	case "":
		if v2 {
			return errors.New("缺少 outcome")
		}
	default:
		return fmt.Errorf("outcome 无效：%q", c.Outcome)
	}
	return nil
}

func (c *Check) validate() error {
	switch c.Kind {
	case CheckChange, CheckSuite, CheckMutation:
	default:
		return fmt.Errorf("kind 无效：%q", c.Kind)
	}
	switch c.Result {
	case CheckPassed, CheckFailed, CheckNotRun:
	default:
		return fmt.Errorf("result 无效：%q", c.Result)
	}
	if strings.TrimSpace(c.Title) == "" {
		return errors.New("缺少 title")
	}
	return nil
}

func (p *Preexisting) validate() error {
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("缺少 title")
	}
	return validLocation(p.Path, p.Line)
}

// validLocation 校验问题指向的位置。
func validLocation(p string, line int) error {
	if line < 0 {
		return errors.New("line 不能为负数")
	}
	if p != "" && !ValidPath(p) {
		return fmt.Errorf("path %q 必须是仓库内的相对路径", p)
	}
	return nil
}

func (f *Finding) validate(v2 bool) error {
	switch f.Severity {
	case SeverityHigh, SeverityMedium, SeverityLow:
	default:
		return fmt.Errorf("severity 无效：%q", f.Severity)
	}
	switch f.Evidence {
	case EvidenceReproduced, EvidenceCode, EvidenceSuspicion:
	default:
		return fmt.Errorf("evidence 无效：%q", f.Evidence)
	}
	if strings.TrimSpace(f.Title) == "" {
		return errors.New("缺少 title")
	}
	if !v2 {
		if strings.TrimSpace(f.Body) == "" {
			return errors.New("缺少 body")
		}
	} else {
		for _, x := range []struct{ name, v string }{
			{"scenario", f.Scenario}, {"actual", f.Actual}, {"expected", f.Expected}, {"impact", f.Impact}, {"proof", f.Proof},
		} {
			if strings.TrimSpace(x.v) == "" {
				return fmt.Errorf("缺少 %s", x.name)
			}
		}
	}
	return validLocation(f.Path, f.Line)
}

// ValidPath 判断 p 是否是规范的仓库相对路径（不以 / 开头，不含 . 或 .. 段）。
func ValidPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && !strings.HasPrefix(p, "../") && p != ".."
}

// Marshal 返回报告的 JSON。
func (r *Report) Marshal() []byte {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err) // 只含字符串和整数，不会失败
	}
	return b
}

// Rewrite 对报告中所有文本字段（包括路径）执行 f，返回新的报告。用于清洗本机路径和脱敏。
// 空数组保持为空数组（序列化为 []，而不是 null）。
func (r *Report) Rewrite(f func(string) string) *Report {
	return &Report{
		Version: r.Version, Verdict: r.Verdict, Summary: f(r.Summary), Scope: f(r.Scope),
		CommandsRun: mapSlice(r.CommandsRun, func(c Command) Command {
			c.Command, c.Note = f(c.Command), f(c.Note)
			return c
		}),
		Findings: mapSlice(r.Findings, func(x Finding) Finding {
			x.Title, x.Path, x.Body = f(x.Title), f(x.Path), f(x.Body)
			x.Scenario, x.Actual, x.Expected, x.Impact, x.Proof, x.Fix = f(x.Scenario), f(x.Actual), f(x.Expected), f(x.Impact), f(x.Proof), f(x.Fix)
			return x
		}),
		Checks: mapSlice(r.Checks, func(c Check) Check {
			c.Title, c.Detail = f(c.Title), f(c.Detail)
			return c
		}),
		Preexisting: mapSlice(r.Preexisting, func(p Preexisting) Preexisting {
			p.Title, p.Path, p.Detail = f(p.Title), f(p.Path), f(p.Detail)
			return p
		}),
		Unverified: mapSlice(r.Unverified, f),
		EnvNotes:   mapSlice(r.EnvNotes, f),
	}
}

// mapSlice 返回对 in 的每个元素执行 f 的结果；in 为 nil 时返回 nil，空数组时返回空数组。
func mapSlice[T any](in []T, f func(T) T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i, x := range in {
		out[i] = f(x)
	}
	return out
}

// localPathRE 匹配常见的用户主目录与临时目录（macOS、Linux）中的绝对路径，用于在发布前去掉本机路径。
// 路径必须在开头或分隔符之后（第 1 组），不误伤仓库相对路径中的同名目录（例如 internal/tmp/x.go）。
// 主目录只匹配到用户名（第 3 组），其后的部分保留；临时目录整段替换。
var localPathRE = regexp.MustCompile(`(^|[\s"'` + "`" + `(\[=:,])(?:/private)?(?:(/(?:Users|home)/[^/\s"'` + "`" + `)\]]+)|/var/folders/[^\s"'` + "`" + `)\]]+|/tmp/[^\s"'` + "`" + `)\]]+)`)

// ScrubLocalPaths 把文本中残留的本机绝对路径替换为占位符：用户主目录换成 ~，/tmp、/var/folders 下的路径换成 <tmp>。
// AI 执行节点已经把工作区路径换成仓库相对路径，这是发布到 GitHub 前的兜底。
func ScrubLocalPaths(s string) string {
	ms := localPathRE.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		b.WriteString(s[last:m[3]]) // 到分隔符为止
		if m[4] >= 0 {
			b.WriteString("~")
		} else {
			b.WriteString("<tmp>")
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Schema 返回报告的 JSON Schema（AI 执行节点写进给 Agent 的说明）。
func Schema() string {
	return schemaJSON
}

// schemaJSON 是交给 Agent 的 JSON Schema（当前版本）。version 由 AI 执行节点写入，不在其中。
const schemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "summary", "scope", "findings", "checks", "unverified", "env_notes", "preexisting", "commands_run"],
  "properties": {
    "verdict": {"type": "string", "enum": ["no_issues", "issues_found", "incomplete"],
      "description": "no_issues：没有发现本次改动引入的问题；issues_found：findings 中有问题；incomplete：没能完成审计"},
    "summary": {"type": "string", "description": "一两句话的结论，不要罗列命令和过程"},
    "scope": {"type": "string", "description": "审计范围，例如：12 个变更文件及相关调用链"},
    "findings": {"type": "array", "description": "本次改动引入的问题。已修复的问题、与本次改动无关的已有问题、环境问题都不是 finding",
      "items": {"type": "object", "additionalProperties": false,
      "required": ["title", "severity", "evidence", "path", "line", "scenario", "actual", "expected", "impact", "proof", "fix", "body"],
      "properties": {
        "title": {"type": "string", "description": "一句话说明问题"},
        "severity": {"type": "string", "enum": ["high", "medium", "low"]},
        "evidence": {"type": "string", "enum": ["reproduced", "code_evidence", "suspicion"],
          "description": "reproduced 只用于实际运行复现过的问题；有明确代码路径为 code_evidence；推测为 suspicion"},
        "path": {"type": "string", "description": "仓库相对路径"},
        "line": {"type": "integer", "description": "被审 commit 中的行号；0 表示不针对具体行"},
        "scenario": {"type": "string", "description": "触发条件或具体示例：前置条件/输入 → 触发步骤（设计问题写实际的误用场景或维护影响）"},
        "actual": {"type": "string", "description": "实际行为"},
        "expected": {"type": "string", "description": "预期行为"},
        "impact": {"type": "string", "description": "影响"},
        "proof": {"type": "string", "description": "证据：复现时写实际执行的命令、关键断言和输出；没有复现时写具体的代码调用链，不要伪造运行结果"},
        "fix": {"type": "string", "description": "修复方向，可为空"},
        "body": {"type": "string", "description": "补充说明，可为空"}}}},
    "checks": {"type": "array", "description": "重要的验证结果（精简，每项一行），会展示在 PR review 中",
      "items": {"type": "object", "additionalProperties": false,
      "required": ["kind", "title", "result", "detail"],
      "properties": {
        "kind": {"type": "string", "enum": ["change", "suite", "mutation"],
          "description": "change：本次改动（修复、新行为）是否达到目的；suite：构建、静态检查、测试等整体验证；mutation：变异验证"},
        "title": {"type": "string", "description": "验证的对象，例如：make test、重复领取请求超额分配任务的修复"},
        "result": {"type": "string", "enum": ["passed", "failed", "not_run"],
          "description": "mutation 的 passed 表示改坏后测试按预期失败，failed 表示改坏后测试仍然通过"},
        "detail": {"type": "string", "description": "简短说明，例如：195 个前端测试通过"}}}},
    "unverified": {"type": "array", "items": {"type": "string"}, "description": "没能验证的部分及原因"},
    "env_notes": {"type": "array", "items": {"type": "string"}, "description": "环境备注：遇到并已解决、或不影响结论的环境问题"},
    "preexisting": {"type": "array", "description": "顺带发现、与本次改动无关的已有问题（不影响结论）",
      "items": {"type": "object", "additionalProperties": false,
      "required": ["title", "path", "line", "detail"],
      "properties": {"title": {"type": "string"}, "path": {"type": "string"}, "line": {"type": "integer"}, "detail": {"type": "string"}}}},
    "commands_run": {"type": "array", "description": "运行过的全部命令（完整执行记录，只在 Kiln 中展示）",
      "items": {"type": "object", "additionalProperties": false,
      "required": ["command", "exit_code", "outcome", "note"],
      "properties": {
        "command": {"type": "string"},
        "exit_code": {"type": "integer"},
        "outcome": {"type": "string", "enum": ["passed", "failed", "env_resolved", "expected_failure", "no_match", "explore"],
          "description": "passed：达到目的；failed：最终没有解决的失败；env_resolved：环境问题导致的失败，之后已解决；expected_failure：预期的失败（例如变异验证）；no_match：搜索没有匹配；explore：探索性操作，结果不影响结论"},
        "note": {"type": "string"}}}}
  }
}`

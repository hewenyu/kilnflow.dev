package review

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// AI 审计 PR review 的正文。

const (
	bodyTestRepo = "tester/demo"
	bodyTestHead = "0123456789abcdef0123456789abcdef01234567"
	bodyTestURL  = "https://kiln.example/ai/tasks/1"
)

var (
	// numberedRE 匹配正文中问题的编号标题"#### N. 严重程度 · 标题"。
	numberedRE = regexp.MustCompile(`(?m)^#### (\d+)\. (🔴 高|🟠 中|🟡 低) · (.+)$`)
	// tableRowRE 匹配问题目录表格的一行"| N | 严重程度 | 标题 | 位置 | 证据 |"。
	tableRowRE = regexp.MustCompile(`(?m)^\| (\d+) \| (🔴 高|🟠 中|🟡 低) \| (.+?) \| .+ \| (已复现|代码证据|推测（未验证）) \|$`)
)

// assertNumberedFindings 检查正文的问题目录和逐个展开的部分都按 1、2、3……列出了全部问题，
// 编号、严重程度、标题和证据等级与报告一致。
func assertNumberedFindings(t *testing.T, body string, fs []aireport.Finding) {
	t.Helper()
	for _, re := range []*regexp.Regexp{numberedRE, tableRowRE} {
		ms := re.FindAllStringSubmatch(body, -1)
		if len(ms) != len(fs) {
			t.Fatalf("正文列出 %d 个问题（%s），报告有 %d 个：\n%s", len(ms), re, len(fs), body)
		}
		for i, m := range ms {
			if m[1] != strconv.Itoa(i+1) || m[2] != severityLabel[fs[i].Severity] || m[3] != fs[i].Title {
				t.Errorf("第 %d 个问题 = %q，应为 %d、%s、%s", i+1, m[0], i+1, severityLabel[fs[i].Severity], fs[i].Title)
			}
			if re == tableRowRE && m[4] != evidenceLabel[fs[i].Evidence] {
				t.Errorf("第 %d 个问题的证据等级 = %s，应为 %s", i+1, m[4], evidenceLabel[fs[i].Evidence])
			}
		}
	}
}

func buildTestReview(rep *aireport.Report, lines map[string]map[int]bool) (string, []string) {
	r := buildAIReview(rep, &aiReviewRow{HeadSHA: bodyTestHead, BaseSHA: "basesha", Agent: "codex"}, bodyTestRepo, lines, bodyTestURL, aiReviewMarker(1))
	var comments []string
	for _, c := range r.Comments {
		comments = append(comments, fmt.Sprintf("%s:%d %s", c.Path, c.Line, c.Body))
	}
	return r.Body, comments
}

func assertContains(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("正文缺少 %q：\n%s", w, body)
		}
	}
}

func assertNotContains(t *testing.T, body string, nots ...string) {
	t.Helper()
	for _, w := range nots {
		if strings.Contains(body, w) {
			t.Errorf("正文不应包含 %q：\n%s", w, body)
		}
	}
}

// TestBuildAIReview_noIssues：通过时写清 0 个新问题，列出精简的验证结果；已修复的问题、环境备注、已有问题分开，
// 命令流水（包括已解决的环境失败、预期失败的变异验证、没有匹配的搜索）不进入正文。
func TestBuildAIReview_noIssues(t *testing.T) {
	rep := &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictNoIssues, Summary: "改动正确，测试覆盖了修复。", Scope: "12 个变更文件及相关调用链",
		Checks: []aireport.Check{
			{Kind: aireport.CheckChange, Title: "重复领取请求超额分配任务", Result: aireport.CheckPassed},
			{Kind: aireport.CheckChange, Title: "多行密钥跨日志分片泄露", Result: aireport.CheckPassed},
			{Kind: aireport.CheckSuite, Title: "make test", Result: aireport.CheckPassed, Detail: "Go race 测试和 195 个前端测试通过"},
			{Kind: aireport.CheckSuite, Title: "make build", Result: aireport.CheckNotRun, Detail: "与本次改动无关"},
			{Kind: aireport.CheckMutation, Title: "撤掉五处关键修复", Result: aireport.CheckPassed, Detail: "对应测试均失败"},
		},
		EnvNotes:    []string{"pnpm 入口和旧 Lint 工具存在兼容问题，临时处理后验证通过"},
		Preexisting: []aireport.Preexisting{{Title: "旧的竞态", Path: "internal/x.go", Line: 9, Detail: "与本次无关"}},
		CommandsRun: []aireport.Command{
			{Command: "make test", ExitCode: 2, Outcome: aireport.OutcomeEnvResolved, Note: "pnpm 入口 ENOEXEC"},
			{Command: "make test", ExitCode: 0, Outcome: aireport.OutcomePassed},
			{Command: "go test -run TestAcquire ./internal/server/runnerapi/", ExitCode: 1, Outcome: aireport.OutcomeExpectedFailure, Note: "变异"},
			{Command: "rg -- '--- SKIP' out.txt", ExitCode: 1, Outcome: aireport.OutcomeNoMatch},
			{Command: "cat README.md", ExitCode: 0, Outcome: aireport.OutcomeExplore},
		},
	}
	body, comments := buildTestReview(rep, nil)
	if len(comments) != 0 {
		t.Fatalf("没有问题时不应有行内评论：%v", comments)
	}
	assertContains(t, body,
		"## Kiln AI 审计\n\n> [!TIP]\n> **✅ 审计通过：未发现本次改动引入的新问题（0 项）**\n>\n> 改动正确，测试覆盖了修复。\n>\n> **范围**：12 个变更文件及相关调用链\n",
		// 验证结果按类别排序：本次改动、整体验证、变异验证
		"### 🧪 验证\n\n| 结果 | 类别 | 验证项 | 说明 |\n|:-:|:-:|---|---|\n"+
			"| ✅ 通过 | 本次改动 | 重复领取请求超额分配任务 | — |\n"+
			"| ✅ 通过 | 本次改动 | 多行密钥跨日志分片泄露 | — |\n"+
			"| ✅ 通过 | 整体验证 | make test | Go race 测试和 195 个前端测试通过 |\n"+
			"| ⚪ 未运行 | 整体验证 | make build | 与本次改动无关 |\n"+
			"| ✅ 通过 | 变异验证 | 撤掉五处关键修复 | 对应测试均失败 |\n",
		"### 🛠️ 环境备注", "- pnpm 入口和旧 Lint 工具存在兼容问题",
		"顺带发现的已有问题（1，与本次改动无关，不影响结论）",
		"[`internal/x.go:9`](https://github.com/tester/demo/blob/"+bodyTestHead+"/internal/x.go#L9) 旧的竞态：与本次无关",
		"head `0123456789ab` · base `basesha` · [完整执行记录与审计证据（5 条命令）]("+bodyTestURL+")",
	)
	// 已修复的问题、已有问题不是本次发现的问题；命令流水只在 Kiln 中
	assertNotContains(t, body, "🐞 问题", "####", "ENOEXEC", "rg --", "cat README.md", "最终失败")
	if !strings.HasSuffix(body, aiReviewMarker(1)) {
		t.Error("正文应以 job 标记结尾")
	}
}

// TestBuildAIReview_allInline：全部问题都能写行内评论时，正文仍然完整列出，行内评论的编号与正文一致。
func TestBuildAIReview_allInline(t *testing.T) {
	rep := &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "s",
		Findings: []aireport.Finding{
			v2Finding("重复领取请求可能突破 max_jobs", aireport.SeverityHigh, aireport.EvidenceReproduced, "a.go", 3),
			v2Finding("常量重复定义", aireport.SeverityLow, aireport.EvidenceCode, "b/c.go", 7),
		},
	}
	body, comments := buildTestReview(rep, map[string]map[int]bool{"a.go": {3: true}, "b/c.go": {7: true}})
	assertNumberedFindings(t, body, rep.Findings)
	loc := "[`a.go:3`](https://github.com/tester/demo/blob/" + bodyTestHead + "/a.go#L3)"
	assertContains(t, body,
		"> [!CAUTION]\n> **❌ 审计未通过：发现 2 个问题（高 1、中 0、低 1）**",
		"| 1 | 🔴 高 | 重复领取请求可能突破 max_jobs | "+loc+" 💬 | 已复现 |\n",
		"💬 同时作为行内评论发布在对应的代码行上，编号与本文一致",
		"#### 1. 🔴 高 · 重复领取请求可能突破 max_jobs\n\n"+loc+" · 已复现 · 💬 行内评论\n\n- **影响**：重复领取请求可能突破 max_jobs的影响\n",
		"- **触发条件**：重复领取请求可能突破 max_jobs的触发条件\n- **实际结果**：", "- **预期结果**：", "- **证据**：", "- **修复方向**：",
		"[`b/c.go:7`](https://github.com/tester/demo/blob/"+bodyTestHead+"/b/c.go#L7) · 代码证据 · 💬 行内评论",
	)
	// 行内评论按严重程度用提示框标出，编号与正文一致，包含完整的细节
	if len(comments) != 2 ||
		!strings.HasPrefix(comments[0], "a.go:3 > [!CAUTION]\n> **Kiln AI 审计 · 问题 1 / 2 · 🔴 高 · 重复领取请求可能突破 max_jobs**\n> 已复现\n") ||
		!strings.HasPrefix(comments[1], "b/c.go:7 > [!NOTE]\n> **Kiln AI 审计 · 问题 2 / 2 · 🟡 低 · 常量重复定义**\n> 代码证据\n") {
		t.Fatalf("行内评论 = %q", comments)
	}
	for _, c := range comments {
		assertContains(t, c, "**影响**", "**触发条件**", "**实际结果**", "**预期结果**", "**证据**", "**修复方向**")
	}
}

// TestBuildAIReview_incomplete：审计未完成时写明不能作为通过的依据，显示已发现的问题数，保留未验证的部分。
func TestBuildAIReview_incomplete(t *testing.T) {
	rep := &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictIncomplete, Summary: "数据库测试没能运行。",
		Unverified: []string{"没有 Docker，数据库测试没有运行"},
	}
	body, _ := buildTestReview(rep, nil)
	assertContains(t, body, "> [!WARNING]\n> **⚠️ 审计未完成：部分内容没能验证，不能作为通过的依据（已发现 0 个问题）**", "### ⚠️ 未验证\n\n- 没有 Docker，数据库测试没有运行\n")

	rep.Findings = []aireport.Finding{v2Finding("问题", aireport.SeverityMedium, aireport.EvidenceCode, "a.go", 1)}
	body, _ = buildTestReview(rep, nil)
	assertContains(t, body, "（已发现 1 个问题：高 0、中 1、低 0）")
	assertNumberedFindings(t, body, rep.Findings)
}

// TestBuildAIReview_multiline：多行的证据（例如代码块）缩进到列表项内，保留格式。
func TestBuildAIReview_multiline(t *testing.T) {
	f := v2Finding("问题", aireport.SeverityHigh, aireport.EvidenceReproduced, "a.go", 1)
	f.Proof = "运行：\n```\ngo test ./x\n--- FAIL: TestX\n```"
	rep := &aireport.Report{Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "s", Findings: []aireport.Finding{f}}
	body, _ := buildTestReview(rep, nil)
	assertContains(t, body, "- **证据**：\n\n  运行：\n  ```\n  go test ./x\n  --- FAIL: TestX\n  ```\n\n")
}

// TestBuildAIReview_v1：v1 的报告（没有结构化字段和验证结果）照常发布：说明写在问题中，命令记录折叠展示。
func TestBuildAIReview_v1(t *testing.T) {
	rep := &aireport.Report{
		Verdict: aireport.VerdictIssuesFound, Summary: "s",
		Findings:    []aireport.Finding{{Title: "旧问题", Severity: aireport.SeverityHigh, Evidence: aireport.EvidenceCode, Path: "a.go", Line: 2, Body: "旧格式的说明"}},
		CommandsRun: []aireport.Command{{Command: "go test ./...", ExitCode: 1, Note: "失败"}},
	}
	body, _ := buildTestReview(rep, nil)
	assertNumberedFindings(t, body, rep.Findings)
	assertContains(t, body, "- **说明**：旧格式的说明", "<details><summary>执行的命令（1）</summary>", "- `go test ./...` → 1：失败", "完整执行记录与审计证据（1 条命令）")
	assertNotContains(t, body, "**影响**")
}

// TestBuildAIReview_long：报告很长时正文不超过上限，结论、全部问题的目录和未验证的部分保留，先省略次要内容。
func TestBuildAIReview_long(t *testing.T) {
	long := strings.Repeat("很长的内容。", 1500) // 约 18 KB
	rep := &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "s",
		Unverified: []string{"关键的未验证部分"},
		EnvNotes:   []string{long},
	}
	for i := range 30 {
		f := v2Finding(fmt.Sprintf("问题%d", i+1), aireport.SeverityMedium, aireport.EvidenceCode, "a.go", i+1)
		f.Proof = long
		rep.Findings = append(rep.Findings, f)
	}
	for range 100 {
		rep.Checks = append(rep.Checks, aireport.Check{Kind: aireport.CheckSuite, Title: "检查", Result: aireport.CheckPassed, Detail: long[:300]})
	}
	for range 200 {
		rep.CommandsRun = append(rep.CommandsRun, aireport.Command{Command: long[:200], Outcome: aireport.OutcomePassed})
	}
	body, _ := buildTestReview(rep, nil)
	if len(body) > maxReviewBody {
		t.Fatalf("正文 %d 字节，超过上限 %d", len(body), maxReviewBody)
	}
	assertNumberedFindings(t, body, rep.Findings)
	assertContains(t, body, "**❌ 审计未通过：发现 30 个问题（高 0、中 30、低 0）**", "### ⚠️ 未验证\n\n- 关键的未验证部分", "问题30的影响",
		strings.TrimPrefix(omittedNote, "\n"), "完整执行记录与审计证据（200 条命令）")
	// 先省略备注和验证，再从后往前省略问题的细节：前面的问题仍有细节，最后的没有
	assertNotContains(t, body, "环境备注", "### 🧪 验证", "问题30的触发条件")
	assertContains(t, body, "问题1的触发条件")
	if !strings.HasSuffix(body, aiReviewMarker(1)) {
		t.Error("正文应以 job 标记结尾")
	}
}

// TestBuildAIReview_longKeptContent：必须保留的内容合计超过上限时（例如 30 个问题、每个影响 1800 字节；
// 这里用 40 个，只去掉代码链接也放不下），收紧每项文字的上限，而不是对整篇正文统一截尾：全部问题的标题、影响和未验证的部分都还在。
func TestBuildAIReview_longKeptContent(t *testing.T) {
	rep := &aireport.Report{Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "s", Unverified: []string{"UNVERIFIED_SENTINEL"}}
	for i := range 40 {
		f := v2Finding(fmt.Sprintf("问题%d", i+1), aireport.SeverityMedium, aireport.EvidenceCode, "a.go", i+1)
		f.Impact = fmt.Sprintf("影响%d：", i+1) + strings.Repeat("影响说明", 150)
		rep.Findings = append(rep.Findings, f)
	}
	if _, err := aireport.Parse(rep.Marshal()); err != nil {
		t.Fatalf("报告应当有效：%v", err)
	}
	body, _ := buildTestReview(rep, nil)
	if len(body) > maxReviewBody {
		t.Fatalf("正文 %d 字节，超过上限 %d", len(body), maxReviewBody)
	}
	assertNumberedFindings(t, body, rep.Findings)
	for i := range rep.Findings {
		assertContains(t, body, fmt.Sprintf("- **影响**：影响%d：影响说明", i+1))
	}
	assertContains(t, body, "### ⚠️ 未验证\n\n- UNVERIFIED_SENTINEL\n")
	if strings.Contains(body, shorten.Ellipsis+omittedNote) {
		t.Error("不应对正文统一截尾")
	}
}

// TestBuildAIReview_maxKeptContent：问题和未验证项都达到数量上限、每项都很长时，结论、问题目录的每一行和
// 每个未验证项仍然都在（缩短后），正文不超过上限，没有统一截尾。
func TestBuildAIReview_maxKeptContent(t *testing.T) {
	// 每项都超过最宽的几级上限；报告整体仍在 aireport.MaxSize 之内
	longText := strings.Repeat("长", 400)
	longPath := strings.Repeat("dir/", 150) + "file.go"
	rep := &aireport.Report{Version: aireport.Version, Verdict: aireport.VerdictIncomplete, Summary: longText, Scope: longText}
	for i := range aireport.MaxFindings {
		rep.Findings = append(rep.Findings, aireport.Finding{
			Title: fmt.Sprintf("问题%03d", i+1) + longText, Severity: aireport.SeverityHigh, Evidence: aireport.EvidenceSuspicion,
			Path: longPath, Line: i + 1, Scenario: "s", Actual: "a", Expected: "e", Impact: longText, Proof: "p",
		})
	}
	for i := range aireport.MaxUnverified {
		rep.Unverified = append(rep.Unverified, fmt.Sprintf("未验证%03d", i+1)+longText)
	}
	if _, err := aireport.Parse(rep.Marshal()); err != nil {
		t.Fatalf("报告应当有效：%v", err)
	}
	body, _ := buildTestReview(rep, nil)
	if len(body) > maxReviewBody || !strings.HasSuffix(body, aiReviewMarker(1)) {
		t.Fatalf("正文 %d 字节，结尾 %q", len(body), body[len(body)-80:])
	}
	assertContains(t, body, "（已发现 100 个问题：高 100、中 0、低 0）", "`…/dir/dir/", "/file.go:1` | 推测（未验证） |")
	rows := regexp.MustCompile(`(?m)^\| (\d+) \| 🔴 高 \| 问题(\d{3})长+… \| `+"`"+`…[^|]*file\.go:\d+`+"`"+` \| 推测（未验证） \|$`).FindAllStringSubmatch(body, -1)
	if len(rows) != aireport.MaxFindings {
		t.Fatalf("目录有 %d 行，应为 %d", len(rows), aireport.MaxFindings)
	}
	for i, m := range rows {
		if m[1] != strconv.Itoa(i+1) || m[2] != fmt.Sprintf("%03d", i+1) {
			t.Errorf("目录第 %d 行 = %q", i+1, m[0])
		}
	}
	for i := range aireport.MaxUnverified {
		assertContains(t, body, fmt.Sprintf("\n- 未验证%03d长", i+1))
	}
	if strings.Contains(body, shorten.Ellipsis+omittedNote) {
		t.Error("不应对正文统一截尾")
	}
}

// TestBuildAIReview_specialPaths：合法路径中的竖线、换行和反引号不能破坏问题目录表格：
// 每个问题仍是一个五列的目录行，证据等级在原来的列中；展开部分和已有问题列表也不被拆开。
func TestBuildAIReview_specialPaths(t *testing.T) {
	rep := &aireport.Report{
		Version: aireport.Version, Verdict: aireport.VerdictIssuesFound, Summary: "s",
		Findings: []aireport.Finding{
			v2Finding("竖线", aireport.SeverityMedium, aireport.EvidenceCode, "src/a|b.go", 3),
			v2Finding("换行", aireport.SeverityLow, aireport.EvidenceReproduced, "src/a\nb.go", 4),
			v2Finding("回车与反引号", aireport.SeverityHigh, aireport.EvidenceSuspicion, "src/a\r`b`.go", 0),
		},
		Preexisting: []aireport.Preexisting{{Title: "已有", Path: "x\ny|z.go", Line: 1}},
	}
	if _, err := aireport.Parse(rep.Marshal()); err != nil {
		t.Fatalf("报告应当有效：%v", err)
	}
	for _, lines := range []map[string]map[int]bool{nil, {"src/a|b.go": {3: true}}} {
		body, _ := buildTestReview(rep, lines)
		assertNumberedFindings(t, body, rep.Findings)
		assertContains(t, body,
			"| 1 | 🟠 中 | 竖线 | [`src/a\\|b.go:3`](https://github.com/tester/demo/blob/"+bodyTestHead+"/src/a%7Cb.go#L3)",
			"| 2 | 🟡 低 | 换行 | [`src/a\\nb.go:4`](https://github.com/tester/demo/blob/"+bodyTestHead+"/src/a%0Ab.go#L4) | 已复现 |",
			"| 3 | 🔴 高 | 回车与反引号 | [`src/a\\r'b'.go`](https://github.com/tester/demo/blob/"+bodyTestHead+"/src/a%0D%60b%60.go) | 推测（未验证） |",
			// 表格之外不转义竖线（代码片段中的 \| 会原样显示）
			"#### 1. 🟠 中 · 竖线\n\n[`src/a|b.go:3`]",
			"- [`x\\ny|z.go:1`](https://github.com/tester/demo/blob/"+bodyTestHead+"/x%0Ay%7Cz.go#L1) 已有\n",
		)
	}
}

func TestAssemble(t *testing.T) {
	a, b, d, e, n := strings.Repeat("A", 100), strings.Repeat("B", 100), strings.Repeat("d", 50), strings.Repeat("e", 50), strings.Repeat("n", 50)
	parts := func() []reviewPart {
		return []reviewPart{{prioKeep, a}, {prioDetail, d}, {prioNotes, n}, {prioKeep, b}, {prioDetail, e}}
	}
	if got := assemble(parts(), 1000); got != a+d+n+b+e {
		t.Fatalf("没有超出时 = %q", got)
	}
	// 超出：先省略 prioNotes，再从后往前省略 prioDetail
	if got := assemble(parts(), len(omittedNote)+len(a+d+b)); got != a+d+b+omittedNote {
		t.Fatalf("省略后 = %q", got)
	}
	// 只剩 prioKeep 仍然超出：硬截断
	got := assemble(parts(), len(omittedNote)+150)
	if len(got) > len(omittedNote)+150 || !strings.HasPrefix(got, a) || !strings.HasSuffix(got, shorten.Ellipsis+omittedNote) {
		t.Fatalf("硬截断后 = %q", got)
	}
}

package aireport

import (
	"encoding/json"
	"strings"
	"testing"
)

func finding() Finding {
	return Finding{Title: "t", Severity: SeverityHigh, Evidence: EvidenceReproduced, Path: "a/b.go", Line: 3, Body: "b"}
}

func TestParse(t *testing.T) {
	r := &Report{
		Verdict: VerdictIssuesFound, Summary: "s", Findings: []Finding{finding()},
		CommandsRun: []Command{{Command: "go test", ExitCode: 1}}, Unverified: []string{"u"},
	}
	got, err := Parse(r.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if got.Findings[0] != finding() || got.CommandsRun[0].Command != "go test" {
		t.Fatalf("解析结果 = %+v", got)
	}
}

func TestParseTrailingContent(t *testing.T) {
	const report = `{"verdict":"no_issues","summary":"s"}`
	for _, suffix := range []string{"}", "]", "} garbage", "{}", "null", "invalid"} {
		t.Run(suffix, func(t *testing.T) {
			if _, err := Parse([]byte(report + suffix)); err == nil {
				t.Fatal("尾随内容应被拒绝")
			}
		})
	}
	if _, err := Parse([]byte(report + " \n\t\r")); err != nil {
		t.Fatalf("应允许尾随空白：%v", err)
	}
}

func TestParse_errors(t *testing.T) {
	valid := func(mod func(*Report)) string {
		r := &Report{Verdict: VerdictIssuesFound, Summary: "s", Findings: []Finding{finding()}}
		mod(r)
		return string(r.Marshal())
	}
	cases := []struct{ name, json, want string }{
		{"不是 JSON", "nope", "不是有效的 JSON"},
		{"未知字段", `{"verdict":"no_issues","summary":"s","x":1}`, "unknown field"},
		{"多余内容", `{"verdict":"no_issues","summary":"s"} {}`, "多余的内容"},
		{"verdict", `{"verdict":"ok","summary":"s"}`, "verdict 无效"},
		{"缺 summary", `{"verdict":"no_issues","summary":" "}`, "缺少 summary"},
		{"issues_found 没有问题", `{"verdict":"issues_found","summary":"s"}`, "没有 findings"},
		{"no_issues 有问题", valid(func(r *Report) { r.Verdict = VerdictNoIssues }), "但有 findings"},
		{"severity", valid(func(r *Report) { r.Findings[0].Severity = "critical" }), "severity 无效"},
		{"evidence", valid(func(r *Report) { r.Findings[0].Evidence = "x" }), "evidence 无效"},
		{"缺 title", valid(func(r *Report) { r.Findings[0].Title = "" }), "缺少 title"},
		{"负行号", valid(func(r *Report) { r.Findings[0].Line = -1 }), "负数"},
		{"绝对路径", valid(func(r *Report) { r.Findings[0].Path = "/etc/passwd" }), "相对路径"},
		{"越界路径", valid(func(r *Report) { r.Findings[0].Path = "../x" }), "相对路径"},
		{"不规范路径", valid(func(r *Report) { r.Findings[0].Path = "a/../b" }), "相对路径"},
		{"文本太长", valid(func(r *Report) { r.Summary = strings.Repeat("x", maxText+1) }), "KiB"},
		{"太多问题", valid(func(r *Report) {
			for range MaxFindings {
				r.Findings = append(r.Findings, finding())
			}
		}), "findings 超过"},
		{"报告太大", `{"summary":"` + strings.Repeat("x", MaxSize) + `"}`, "KiB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.json))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
		})
	}
}

func v2Report() *Report {
	return &Report{
		Version: Version, Verdict: VerdictIssuesFound, Summary: "s", Scope: "1 个文件",
		Findings: []Finding{{
			Title: "t", Severity: SeverityHigh, Evidence: EvidenceReproduced, Path: "a.go", Line: 3,
			Scenario: "sc", Actual: "ac", Expected: "ex", Impact: "im", Proof: "pr",
		}},
		Checks:      []Check{{Kind: CheckMutation, Title: "撤掉修复", Result: CheckPassed}},
		EnvNotes:    []string{"e"},
		Preexisting: []Preexisting{{Title: "旧问题", Path: "b.go", Line: 1}},
		CommandsRun: []Command{{Command: "go test", ExitCode: 1, Outcome: OutcomeExpectedFailure}},
	}
}

// TestValidate_v2：v2 的报告要求问题写清触发条件、实际与预期行为、影响和证据，命令标明 outcome；v1 的报告仍然有效。
func TestValidate_v2(t *testing.T) {
	if err := v2Report().Validate(); err != nil {
		t.Fatalf("有效的 v2 报告：%v", err)
	}
	got, err := Parse(v2Report().Marshal())
	if err != nil || got.Version != Version || got.Findings[0].Proof != "pr" || got.Checks[0].Kind != CheckMutation ||
		got.Preexisting[0].Path != "b.go" || got.CommandsRun[0].Outcome != OutcomeExpectedFailure {
		t.Fatalf("解析 v2 报告 = %+v，%v", got, err)
	}
	cases := []struct {
		name string
		mod  func(*Report)
		want string
	}{
		{"缺 scenario", func(r *Report) { r.Findings[0].Scenario = " " }, "缺少 scenario"},
		{"缺 actual", func(r *Report) { r.Findings[0].Actual = "" }, "缺少 actual"},
		{"缺 expected", func(r *Report) { r.Findings[0].Expected = "" }, "缺少 expected"},
		{"缺 impact", func(r *Report) { r.Findings[0].Impact = "" }, "缺少 impact"},
		{"缺 proof", func(r *Report) { r.Findings[0].Proof = "" }, "缺少 proof"},
		{"缺 outcome", func(r *Report) { r.CommandsRun[0].Outcome = "" }, "缺少 outcome"},
		{"outcome 无效", func(r *Report) { r.CommandsRun[0].Outcome = "ok" }, "outcome 无效"},
		{"check kind", func(r *Report) { r.Checks[0].Kind = "x" }, "kind 无效"},
		{"check result", func(r *Report) { r.Checks[0].Result = "ok" }, "result 无效"},
		{"check title", func(r *Report) { r.Checks[0].Title = "" }, "缺少 title"},
		{"preexisting title", func(r *Report) { r.Preexisting[0].Title = "" }, "缺少 title"},
		{"preexisting path", func(r *Report) { r.Preexisting[0].Path = "../x" }, "相对路径"},
		{"preexisting line", func(r *Report) { r.Preexisting[0].Line = -1 }, "负数"},
		{"version 太大", func(r *Report) { r.Version = Version + 1 }, "version 无效"},
		{"太多 checks", func(r *Report) { r.Checks = make([]Check, MaxChecks+1) }, "checks 超过"},
		{"scope 太长", func(r *Report) { r.Scope = strings.Repeat("x", maxText+1) }, "KiB"},
		{"proof 太长", func(r *Report) { r.Findings[0].Proof = strings.Repeat("x", maxText+1) }, "KiB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := v2Report()
			c.mod(r)
			if err := r.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
		})
	}
	// v1：问题只有 body，命令没有 outcome
	v1 := &Report{Verdict: VerdictIssuesFound, Summary: "s", Findings: []Finding{finding()}, CommandsRun: []Command{{Command: "c"}}}
	if err := v1.Validate(); err != nil {
		t.Fatalf("v1 报告应有效：%v", err)
	}
	v1.Findings[0].Body = ""
	if err := v1.Validate(); err == nil || !strings.Contains(err.Error(), "缺少 body") {
		t.Fatalf("v1 的问题缺少 body 时 err = %v", err)
	}
}

// TestRequiredFields：v2 报告要有 Schema 中的全部必填字段。Agent 的输出（CheckRequired）中 null 也算缺少；
// Server 解析时（Parse）允许 null（旧版 Runner 把空数组写成 null）。v1 报告不检查。
func TestRequiredFields(t *testing.T) {
	full := string(v2Report().Rewrite(func(s string) string { return s }).Marshal())
	if err := CheckRequired([]byte(strings.Replace(full, `"unverified":null`, `"unverified":[]`, 1))); err != nil {
		t.Fatalf("完整的报告：%v", err)
	}
	if err := CheckRequired([]byte(full)); err == nil || !strings.Contains(err.Error(), "缺少必填字段：unverified") {
		t.Fatalf("null 的数组：%v", err)
	}
	if _, err := Parse([]byte(full)); err != nil {
		t.Fatalf("Server 解析时允许 null：%v", err)
	}
	cases := []struct{ name, json, want string }{
		{"只有结论", `{"version":2,"verdict":"no_issues","summary":"ok"}`, "缺少必填字段：scope、findings、checks"},
		{"finding 缺字段", strings.Replace(full, `"fix":"",`, "", 1), "缺少必填字段：findings[0].fix"},
		{"check 缺字段", strings.Replace(full, `,"detail":""}],"unverified"`, `}],"unverified"`, 1), "checks[0].detail"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.json == full {
				t.Fatal("测试数据没有改动")
			}
			if _, err := Parse([]byte(c.json)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse：%v，应包含 %q", err, c.want)
			}
			if err := CheckRequired([]byte(c.json)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("CheckRequired：%v，应包含 %q", err, c.want)
			}
		})
	}
	// 缺少的字段太多时只列出前几个
	if err := CheckRequired([]byte(`{"findings":[{},{}]}`)); err == nil || !strings.Contains(err.Error(), "等 32 个") {
		t.Fatalf("缺少很多字段：%v", err)
	}
	// v1 报告（没有 version）不要求这些字段
	if _, err := Parse([]byte(`{"verdict":"no_issues","summary":"ok"}`)); err != nil {
		t.Fatalf("v1 报告：%v", err)
	}
}

func TestValidPath(t *testing.T) {
	for p, want := range map[string]bool{"a.go": true, "a/b.go": true, "": false, ".": false, "..": false, "../a": false, "/a": false, "a//b": false, "./a": false} {
		if got := ValidPath(p); got != want {
			t.Errorf("ValidPath(%q) = %v", p, got)
		}
	}
}

func TestScrubLocalPaths(t *testing.T) {
	cases := map[string]string{
		"见 /Users/alice/code/x/a.go:3":    "见 ~/code/x/a.go:3",
		"/home/bob/src 下":                 "~/src 下",
		"写到 /tmp/kiln-123/out.json 失败":    "写到 <tmp> 失败",
		"`/private/var/folders/ab/T/x` 中": "`<tmp>` 中",
		"仓库内 internal/a.go":               "仓库内 internal/a.go",
		"脚本在 /Users/bob/tmp/x.sh":         "脚本在 ~/tmp/x.sh",
		"internal/tmp/x.go 与 a/home/b":    "internal/tmp/x.go 与 a/home/b",
		"(/tmp/a) 和 /home/c":              "(<tmp>) 和 ~",
		"/Users/x":                        "~",
	}
	for in, want := range cases {
		if got := ScrubLocalPaths(in); got != want {
			t.Errorf("ScrubLocalPaths(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestRewrite(t *testing.T) {
	r := &Report{
		Verdict: VerdictIssuesFound, Summary: "s", Findings: []Finding{finding()},
		CommandsRun: []Command{{Command: "c", Note: "n", ExitCode: 2, Outcome: OutcomeFailed}}, Unverified: []string{"u"},
	}
	got := r.Rewrite(strings.ToUpper)
	if got.Summary != "S" || got.Findings[0].Title != "T" || got.Findings[0].Body != "B" || got.Findings[0].Path != "A/B.GO" ||
		got.CommandsRun[0].Command != "C" || got.CommandsRun[0].Note != "N" || got.CommandsRun[0].ExitCode != 2 ||
		got.CommandsRun[0].Outcome != OutcomeFailed || got.Unverified[0] != "U" {
		t.Fatalf("Rewrite = %+v", got)
	}
	// v2 的字段都要清洗（例如本机路径），枚举值保持不变
	got = v2Report().Rewrite(strings.ToUpper)
	f, c, p := got.Findings[0], got.Checks[0], got.Preexisting[0]
	if got.Version != Version || got.Scope != "1 个文件" || f.Scenario != "SC" || f.Actual != "AC" || f.Expected != "EX" || f.Impact != "IM" ||
		f.Proof != "PR" || c.Title != "撤掉修复" || c.Kind != CheckMutation || p.Path != "B.GO" || got.EnvNotes[0] != "E" {
		t.Fatalf("Rewrite v2 = %+v", got)
	}
	if r.Summary != "s" || r.Findings[0].Title != "t" {
		t.Error("Rewrite 不应修改原报告")
	}
	// 空数组保持为空数组（序列化为 []），nil 保持为 nil
	got = (&Report{Checks: []Check{}}).Rewrite(strings.ToUpper)
	if got.Checks == nil || got.Findings != nil {
		t.Errorf("Rewrite 空数组 = %#v，nil = %#v", got.Checks, got.Findings)
	}
}

func TestSchema(t *testing.T) {
	var m struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(Schema()), &m); err != nil {
		t.Fatalf("Schema 不是合法的 JSON：%v", err)
	}
	// Schema 中的字段与报告的 JSON 一致（version 由 AI 执行节点写入，不在 schema 中）
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(v2Report().Marshal(), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "version")
	if len(m.Required) != len(fields) || len(m.Properties) != len(fields) {
		t.Fatalf("schema 的字段 %v 与报告的字段数 %d 不一致", m.Required, len(fields))
	}
	for _, k := range m.Required {
		if _, ok := fields[k]; !ok {
			t.Errorf("schema 要求的字段 %q 不在报告中", k)
		}
	}
}

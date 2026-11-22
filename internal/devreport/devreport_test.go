package devreport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSelection(t *testing.T) {
	cands := []int{3, 5, 8}
	ok := `{"selected":[5],"batch_reason":"","plan":"改 a.go","design_first":true,"design_reason":"要先定状态机","skipped":[{"number":3,"reason":"需求不清","reconsider":"补齐后","later":false},{"number":8,"reason":"与本批无关","reconsider":"","later":true}]}`
	s, err := ParseSelection([]byte(ok), cands, 3)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != Version || len(s.Selected) != 1 || s.Selected[0] != 5 || len(s.Skipped) != 2 || s.Skipped[0].Later || !s.Skipped[1].Later ||
		s.FirstPhase() != PhaseDesign {
		t.Fatalf("%+v", s)
	}
	// design_first=false：一个 PR 完成（起始阶段是实现）；wait_for 记下依赖
	one := `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"小改动","skipped":[{"number":3,"reason":"依赖 #5","reconsider":"#5 关闭后","later":false,"wait_for":[5,9]},{"number":8,"reason":"b","reconsider":"","later":true,"wait_for":[]}]}`
	if s, err := ParseSelection([]byte(one), cands, 3); err != nil || s.FirstPhase() != PhaseImplement || len(s.Skipped[0].WaitFor) != 2 {
		t.Fatalf("%+v，%v", s, err)
	}
	none := `{"selected":[],"batch_reason":"","plan":"","skipped":[{"number":3,"reason":"a","reconsider":"","later":false},{"number":5,"reason":"b","reconsider":"","later":false},{"number":8,"reason":"c","reconsider":"","later":false}]}`
	if _, err := ParseSelection([]byte(none), cands, 3); err != nil {
		t.Fatalf("什么也不选也是有效的报告：%v", err)
	}
	for _, tc := range []struct{ name, json, want string }{
		{"不是候选", `{"selected":[9],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[]}`, "#9 不是候选"},
		{"重复", `{"selected":[5,5],"batch_reason":"x","plan":"p","skipped":[]}`, "重复"},
		{"超过上限", `{"selected":[3,5,8],"batch_reason":"x","plan":"p","skipped":[]}`, "最多领取 2 个"},
		{"没有计划", `{"selected":[5],"batch_reason":"","plan":" ","skipped":[{"number":3,"reason":"a","reconsider":"","later":false},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "plan"},
		{"多个没有合批理由", `{"selected":[3,5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":8,"reason":"b","reconsider":"","later":false}]}`, "batch_reason"},
		{"未选没有原因", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":"","later":false}]}`, "候选 #8 既没有选"},
		{"原因为空", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":3,"reason":"","reconsider":"","later":false},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "#3 没有写原因"},
		{"又选又跳过", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":5,"reason":"a","reconsider":"","later":false}]}`, "同时在"},
		{"跳过非候选", `{"selected":[],"batch_reason":"","plan":"","skipped":[{"number":1,"reason":"a","reconsider":"","later":false}]}`, "#1 不是候选"},
		{"选了没写方式", `{"selected":[5],"batch_reason":"","plan":"p","design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":"","later":false},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "design_first"},
		{"方式为 null", `{"selected":[5],"batch_reason":"","plan":"p","design_first":null,"design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":"","later":false},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "design_first"},
		{"方式没有理由", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":" ","skipped":[{"number":3,"reason":"a","reconsider":"","later":false},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "design_reason"},
		{"later 又等依赖", `{"selected":[],"batch_reason":"","plan":"","skipped":[{"number":3,"reason":"a","reconsider":"","later":true,"wait_for":[5]},{"number":5,"reason":"b","reconsider":"","later":false},{"number":8,"reason":"c","reconsider":"","later":false}]}`, "later=true 和 wait_for"},
		{"等自己", `{"selected":[],"batch_reason":"","plan":"","skipped":[{"number":3,"reason":"a","reconsider":"","later":false,"wait_for":[3]},{"number":5,"reason":"b","reconsider":"","later":false},{"number":8,"reason":"c","reconsider":"","later":false}]}`, "wait_for"},
		{"缺少 later", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":""},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "#3 缺少 later"},
		{"later 为 null", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":"","later":null},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "#3 缺少 later"},
		{"skipped 中的未知字段", `{"selected":[5],"batch_reason":"","plan":"p","design_first":false,"design_reason":"r","skipped":[{"number":3,"reason":"a","reconsider":"","later":false,"x":1},{"number":8,"reason":"b","reconsider":"","later":false}]}`, "unknown field"},
		{"未知字段", `{"selected":[],"plan":"","batch_reason":"","skipped":[],"x":1}`, "unknown field"},
		{"两个对象", `{"selected":[],"plan":"","batch_reason":"","skipped":[]}{}`, "只能是一个"},
		{"版本", `{"version":9,"selected":[],"plan":"","batch_reason":"","skipped":[]}`, "版本 9"},
		{"太大", `{"plan":"` + strings.Repeat("x", MaxSize) + `"}`, "KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := 3
			if tc.name == "超过上限" {
				limit = 2
			}
			if _, err := ParseSelection([]byte(tc.json), cands, limit); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，应包含 %q", err, tc.want)
			}
		})
	}
}

// delivery 返回一份有效的交付报告（#3 full、#5 partial，返修 F1-1 fixed、F1-2 rejected），f 修改它。
func delivery(f func(d *Delivery)) []byte {
	d := &Delivery{
		Status: StatusDelivered, Summary: "实现了 A", PRTitle: "实现 A",
		Issues: []IssueResult{
			{Number: 3, Resolution: ResolutionFull, Acceptance: []AcceptanceItem{{Item: "A", Done: true, Evidence: "a.go + TestA"}}},
			{Number: 5, Resolution: ResolutionPartial, Acceptance: []AcceptanceItem{{Item: "B", Done: true, Evidence: "b.go"}, {Item: "C"}}},
		},
		Verification: []Verification{{Command: "make test", Outcome: OutcomePassed}},
		Dispositions: []Disposition{
			{Finding: "F1-1", Action: ActionFixed, Commit: "abc", Reason: "加了锁"},
			{Finding: "F1-2", Action: ActionRejected, Reason: "调用方已经保证"},
		},
		Handoff: Handoff{Remaining: "C"},
	}
	if f != nil {
		f(d)
	}
	b, _ := json.Marshal(d)
	return b
}

func TestParseDelivery(t *testing.T) {
	issues, findings := []int{3, 5}, []string{"F1-1", "F1-2"}
	e := Expect{Issues: issues, Findings: findings}
	d, err := ParseDelivery(delivery(nil), e)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.FullyResolved(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("FullyResolved = %v（partial 的 #5 不能关闭）", got)
	}
	if d.AllFixed() {
		t.Fatal("有不采纳的问题时 AllFixed 应为 false")
	}
	for _, tc := range []struct {
		name string
		f    func(d *Delivery)
		want string
	}{
		{"状态", func(d *Delivery) { d.Status = "done" }, "status"},
		{"摘要", func(d *Delivery) { d.Summary = "" }, "summary"},
		{"缺 Issue", func(d *Delivery) { d.Issues = d.Issues[:1] }, "缺少 #5"},
		{"多 Issue", func(d *Delivery) { d.Issues = append(d.Issues, IssueResult{Number: 9, Resolution: ResolutionNone}) }, "#9 不属于"},
		{"重复 Issue", func(d *Delivery) { d.Issues = append(d.Issues, d.Issues[0]) }, "重复"},
		{"部分完成冒充全部", func(d *Delivery) { d.Issues[1].Resolution = ResolutionFull }, "#5 是 full 时验收项要全部完成"},
		{"full 没有验收项", func(d *Delivery) { d.Issues[0].Acceptance = nil }, "#3 是 full"},
		{"partial 全部完成", func(d *Delivery) { d.Issues[1].Acceptance = d.Issues[1].Acceptance[:1] }, "#5 是 partial"},
		{"完成没有证据", func(d *Delivery) { d.Issues[0].Acceptance[0].Evidence = "" }, "evidence"},
		{"完成度无效", func(d *Delivery) { d.Issues[0].Resolution = "done" }, "resolution"},
		{"delivered 都没完成", func(d *Delivery) {
			d.Issues[0] = IssueResult{Number: 3, Resolution: ResolutionNone}
			d.Issues[1] = IssueResult{Number: 5, Resolution: ResolutionNone}
		}, "至少一个 Issue"},
		{"没有 PR 标题", func(d *Delivery) { d.PRTitle = "" }, "pr_title"},
		{"no_change 有完成", func(d *Delivery) { d.Status = StatusNoChange }, "no_change"},
		{"blocked 没写剩余", func(d *Delivery) { d.Status, d.Handoff.Remaining = StatusBlocked, "" }, "handoff.remaining"},
		{"缺处置", func(d *Delivery) { d.Dispositions = d.Dispositions[:1] }, "缺少问题 F1-2 的处置"},
		{"处置非本轮", func(d *Delivery) { d.Dispositions[0].Finding = "F9-9" }, "不是本轮"},
		{"fixed 没有 commit", func(d *Delivery) { d.Dispositions[0].Commit = "" }, "commit"},
		{"处置没有理由", func(d *Delivery) { d.Dispositions[1].Reason = "" }, "reason"},
		{"重复指向自己", func(d *Delivery) { d.Dispositions[1].Action, d.Dispositions[1].DuplicateOf = ActionDuplicate, "F1-2" }, "duplicate_of"},
		{"处置无效", func(d *Delivery) { d.Dispositions[1].Action = "ignored" }, "action"},
		{"验证命令", func(d *Delivery) { d.Verification[0].Outcome = "ok" }, "verification[0]"},
		{"设计文档不是 Markdown", func(d *Delivery) { d.Design.Documents = []string{"docs/a.go"} }, "design.documents[0]"},
		{"设计文档是绝对路径", func(d *Delivery) { d.Design.Documents = []string{"/etc/a.md"} }, "design.documents[0]"},
		{"设计文档在仓库外", func(d *Delivery) { d.Design.Documents = []string{"docs/../../a.md"} }, "design.documents[0]"},
		{"决定类别", func(d *Delivery) { d.Design.Decisions = []Decision{{Category: "x", Choice: "c"}} }, "category"},
		{"决定没有选择", func(d *Delivery) { d.Design.Decisions = []Decision{{Category: DecisionPolicy}} }, "choice"},
		{"改已确认的决定没写编号", func(d *Delivery) { d.Design.Decisions = []Decision{{Category: DecisionConfirmed, Choice: "c"}} }, "ref"},
		{"外部输入", func(d *Delivery) { d.Design.ExternalInputs = []ExternalInput{{Blocks: "b"}} }, "external_inputs[0]"},
		{"与设计的出入", func(d *Delivery) { d.DesignDeviations = []Deviation{{Item: "a"}} }, "design_deviations[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDelivery(delivery(tc.f), e); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，应包含 %q", err, tc.want)
			}
		})
	}
	// no_change：每个 Issue 都是 none 并写明原因
	nc := delivery(func(d *Delivery) {
		d.Status, d.PRTitle, d.Dispositions = StatusNoChange, "", nil
		d.Issues = []IssueResult{{Number: 3, Resolution: ResolutionNone, Notes: "已由 #2 实现"}, {Number: 5, Resolution: ResolutionNone, Notes: "同上"}}
	})
	if _, err := ParseDelivery(nc, Expect{Issues: issues}); err != nil {
		t.Fatalf("no_change：%v", err)
	}
	// 重复指向已修复的问题：算全部修复
	all := delivery(func(d *Delivery) {
		d.Dispositions[1] = Disposition{Finding: "F1-2", Action: ActionDuplicate, DuplicateOf: "F1-1", Reason: "同一处"}
	})
	if d, err := ParseDelivery(all, e); err != nil || !d.AllFixed() {
		t.Fatalf("AllFixed：%v，%v", d, err)
	}
	decided := delivery(func(d *Delivery) {
		d.Design = Design{Documents: []string{"docs/design/a.md"}, Decisions: []Decision{{Category: DecisionConfirmed, Ref: "C20", Choice: "改"}}}
	})
	if d, err := ParseDelivery(decided, e); err != nil || !d.HasConfirmedDecision() {
		t.Fatalf("HasConfirmedDecision：%v，%v", d, err)
	}
	if d, _ := ParseDelivery(delivery(nil), e); d.HasConfirmedDecision() {
		t.Fatal("没有声明改已确认的决定")
	}
}

// TestParseDeliveryDesign：设计阶段的交付（PLAN #31.4）每个 Issue 都是 none、至少一个设计文档；实现阶段 delivered 全是
// none 不行。
func TestParseDeliveryDesign(t *testing.T) {
	issues := []int{3, 5}
	design := func(f func(d *Delivery)) []byte {
		return delivery(func(d *Delivery) {
			d.Dispositions = nil
			d.Issues = []IssueResult{
				{Number: 3, Resolution: ResolutionNone, Acceptance: []AcceptanceItem{{Item: "A"}}},
				{Number: 5, Resolution: ResolutionNone},
			}
			d.Design.Documents = []string{"docs/design/x.md"}
			if f != nil {
				f(d)
			}
		})
	}
	de := Expect{Issues: issues, Phase: PhaseDesign}
	if _, err := ParseDelivery(design(nil), de); err != nil {
		t.Fatalf("设计交付：%v", err)
	}
	if _, err := ParseDelivery(design(nil), Expect{Issues: issues, Phase: PhaseImplement}); err == nil || !strings.Contains(err.Error(), "至少一个 Issue") {
		t.Fatalf("实现阶段全是 none：%v", err)
	}
	for _, tc := range []struct {
		name, want string
		f          func(d *Delivery)
	}{
		{"设计解决了 Issue", "resolution 都写 none", func(d *Delivery) {
			d.Issues[0] = IssueResult{Number: 3, Resolution: ResolutionFull, Acceptance: []AcceptanceItem{{Item: "A", Done: true, Evidence: "e"}}}
		}},
		{"没有设计文档", "design.documents", func(d *Delivery) { d.Design.Documents = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDelivery(design(tc.f), de); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，应包含 %q", err, tc.want)
			}
		})
	}
}

// TestSchemas：Schema 是有效的 JSON，必填字段与 Go 结构一致（Agent 按它写，Runner 按结构严格解码）。
func TestSchemas(t *testing.T) {
	for name, tc := range map[string]struct {
		schema string
		v      any
	}{"selection": {SelectionSchema(), Selection{}}, "delivery": {DeliverySchema(), Delivery{}}} {
		var s struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(tc.schema), &s); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		var fields map[string]any
		b, _ := json.Marshal(tc.v)
		_ = json.Unmarshal(b, &fields)
		delete(fields, "version")
		if len(fields) != len(s.Required) {
			t.Errorf("%s：结构有 %d 个字段，schema 要求 %d 个（%v）", name, len(fields), len(s.Required), s.Required)
		}
		for _, r := range s.Required {
			if _, ok := fields[r]; !ok {
				t.Errorf("%s：schema 要求的 %s 不在结构中", name, r)
			}
		}
	}
}

// TestParseWorkItemBranch：正式分支带阶段和轮次（PLAN #31.3），解析与生成互逆；第一轮实现的分支与 PLAN #26 相同。
func TestParseWorkItemBranch(t *testing.T) {
	for want, ref := range map[string]BranchRef{
		"kiln/dev-42":    {ID: 42, Phase: PhaseImplement, Round: 1},
		"kiln/dev-42-r3": {ID: 42, Phase: PhaseImplement, Round: 3},
		"kiln/design-42": {ID: 42, Phase: PhaseDesign, Round: 1},
	} {
		if got := WorkItemBranch(ref.ID, ref.Phase, ref.Round); got != want {
			t.Errorf("WorkItemBranch(%+v) = %q，应为 %q", ref, got, want)
		}
		if got, ok := ParseWorkItemBranch(want); !ok || got != ref {
			t.Errorf("ParseWorkItemBranch(%q) = %+v, %v", want, got, ok)
		}
	}
	for _, b := range []string{
		"main", "kiln/dev-", "kiln/dev-0", "kiln/dev-042", "kiln/dev-4x", "kiln/dev-4/x", "kiln/attempt-4", "x/kiln/dev-4",
		"kiln/dev-4-r1", "kiln/dev-4-r0", "kiln/dev-4-r", "kiln/dev-4-r02", "kiln/design-4-r2", "kiln/design-", "kiln/design-0",
	} {
		if ref, ok := ParseWorkItemBranch(b); ok {
			t.Errorf("%q 不是正式分支，却解析为 %+v", b, ref)
		}
	}
}

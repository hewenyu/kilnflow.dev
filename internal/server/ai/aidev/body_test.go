package aidev

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/devreport"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
)

// TestPRBody：只有完整解决的 Issue 写 Fixes（I7），部分解决、没有解决写 Refs；hold 的原因逐条列出；Agent 写的内容标为自报；
// 领取后需求变了要写明；末尾有工作项标记。
func TestPRBody(t *testing.T) {
	w := &WorkItem{ID: 7, Agent: "claude-code", MaxFixRounds: 2, RequirementsChanged: true}
	issues := []WorkIssue{{Number: 1, Title: "一"}, {Number: 2, Title: "二"}, {Number: 3, Title: "三"}}
	rep := &devreport.Delivery{
		Version: devreport.Version, Status: devreport.StatusDelivered, Summary: "做了", PRTitle: "标题",
		Issues: []devreport.IssueResult{
			{Number: 1, Resolution: devreport.ResolutionFull, Acceptance: []devreport.AcceptanceItem{{Item: "能登录", Done: true}}},
			{Number: 2, Resolution: devreport.ResolutionPartial, Acceptance: []devreport.AcceptanceItem{{Item: "a", Done: true}, {Item: "b"}}},
			{Number: 3, Resolution: devreport.ResolutionNone, Notes: "不清楚"},
		},
	}
	body := prBody(w, rep, issues, mergecfg.HoldLabel, []string{"原因甲", "原因乙"})
	for _, want := range []string{
		"Fixes #1：", "Refs #2：部分解决", "Refs #3：没有解决", "- [x] 能登录", "- [ ] b", "原因甲", "原因乙",
		"kiln:hold", "摘要（Agent 自报）", "被修改过", "<!-- kiln:work-item:7 -->",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR 描述缺少 %q：\n%s", want, body)
		}
	}
	for _, bad := range []string{"Fixes #2", "Fixes #3"} {
		if strings.Contains(body, bad) {
			t.Errorf("PR 描述有 %q", bad)
		}
	}
	// 没有报告时按记下的完成度；没有 hold 时不提
	plain := prBody(&WorkItem{ID: 8}, nil, []WorkIssue{{Number: 1, Resolution: devreport.ResolutionFull}}, mergecfg.HoldLabel, nil)
	if !strings.Contains(plain, "Fixes #1") || strings.Contains(plain, "kiln:hold") || strings.Contains(plain, "被修改过") {
		t.Fatalf("PR 描述：\n%s", plain)
	}
	if got := prTitle(&WorkItem{ID: 8}, nil, nil); got != "AI 开发："+workItemRef(8) {
		t.Fatalf("标题 = %q", got)
	}
	if got := prTitle(w, &devreport.Delivery{PRTitle: "a\nb"}, issues); got != "a b" {
		t.Fatalf("标题 = %q", got)
	}
	// 没有 pr_title 时用 Issue 标题兜底：兜底路径同样要 defuse（Issue 标题可以被注入）。
	if got := prTitle(&WorkItem{ID: 9}, nil, []WorkIssue{{Number: 1, Title: "修 #12 的 @alice <b>"}}); strings.Contains(got, "#12") ||
		strings.Contains(got, "@alice") || strings.Contains(got, "<b>") {
		t.Fatalf("兜底的标题没有 defuse：%q", got)
	}
}

package web

import (
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
)

// mqSampleParams 是契约测试中原因码的参数：每个原因码只用到其中的一部分。
var mqSampleParams = mergequeue.NoteParams{
	Branch: "dev", From: "main", Allowed: []string{"main", "dev"}, Pos: 2, Done: 1, Total: 3, Tries: 2, Detail: "boom",
	Checks: []string{"ci", "ai-review"}, Runs: []mergequeue.RunRef{{Name: "CI", ID: 3, State: "failed"}},
	SHA: "0123456789abcdef0123", WorkItem: 12, State: "stopped", Closed: []int{3, 4},
}

// zhList 把领域层中文说明中的列表分隔符换成消息参数的分隔符（参数是纯文字，不按语言连接）。
func zhList(s string) string { return strings.ReplaceAll(s, "、", listSep) }

// TestMergeQueueNoteText：合并队列的每个原因码都映射为自己的消息（登记过、在两种语言的词典中都有），页面上的中文与领域层
// 写给 CLI、日志和评论的中文说明一致（列表的分隔符除外）。
func TestMergeQueueNoteText(t *testing.T) {
	seen := map[msgID]string{}
	codes := append(mergequeue.StatusCodes(), mergequeue.ReasonCodes()...)
	for _, code := range codes {
		n := mergequeue.Note{Code: code, Params: mqSampleParams}
		if code == mergequeue.ReasonUpdateUnconfirmed {
			// 领域层把当时的状态（等待平台确认可合并状态）记在 Detail 中
			n.Params.Detail = mergequeue.Note{Code: mergequeue.StatusMergeability, Params: n.Params}.Text()
		}
		got := mqNoteText(n, "原样的说明")
		if got.id == "" {
			t.Errorf("%s 没有对应的消息", code)
			continue
		}
		if _, ok := messages[got.id]; !ok {
			t.Errorf("%s 的消息 %q 没有登记", code, got.id)
		}
		if prev, dup := seen[got.id]; dup {
			t.Errorf("%s 与 %s 是同一条消息 %q", code, prev, got.id)
		}
		seen[got.id] = code
		zh := textZH(t, got)
		want := n.Text()
		if code != mergequeue.ReasonNoChecks {
			want = zhList(want)
		}
		if code == mergequeue.ReasonChecksFailed {
			// run 的名称、编号和状态值原样作为参数，括号按参数的写法
			want = "验证没有通过：CI (run #3, failed)"
		}
		if zh != want {
			t.Errorf("%s：页面上是 %q，领域层的说明是 %q", code, zh, want)
		}
	}
	// 关闭关联 issue 的进度和失败：按结果和平台是否拒绝各有自己的消息
	variants := []mergequeue.Note{
		{Code: mergequeue.StatusIssuesDone, Params: mergequeue.NoteParams{Unknown: []int{5}}},
		{Code: mergequeue.StatusIssuesDone, Params: mergequeue.NoteParams{Closed: []int{3}, Unknown: []int{5}}},
		{Code: mergequeue.StatusIssuesRetry, Params: mergequeue.NoteParams{Tries: 1, Detail: "403", Rejected: true}},
		{Code: mergequeue.StatusIssuesGaveUp, Params: mergequeue.NoteParams{Tries: 4, Detail: "403", Rejected: true}},
	}
	for _, n := range variants {
		got := mqNoteText(n, "")
		if prev, dup := seen[got.id]; dup || got.id == "" {
			t.Errorf("%s %+v：消息 %q 与 %s 重复", n.Code, n.Params, got.id, prev)
		}
		seen[got.id] = n.Code
		if zh, want := textZH(t, got), zhList(n.Text()); zh != want {
			t.Errorf("%s %+v：页面上是 %q，领域层的说明是 %q", n.Code, n.Params, zh, want)
		}
	}
	// 没有原因码的旧记录、不认识的原因码原样显示记下的中文说明，不按中文反推
	for _, code := range []string{"", "future"} {
		if got := mqNoteText(mergequeue.Note{Code: code}, "关闭关联的 issue 失败 3 次，已放弃"); !isRaw(got, "关闭关联的 issue 失败 3 次，已放弃") {
			t.Errorf("原因码 %q：%+v", code, got)
		}
	}
	if !mqNoteText(mergequeue.Note{}, "").IsZero() {
		t.Error("没有状态时应为空")
	}
}

// TestAutoTagNoteText：自动打 tag 的每个原因码都映射为自己的消息；有领域层说明的，页面上的中文与它一致。
func TestAutoTagNoteText(t *testing.T) {
	p := autotag.NoteParams{
		Tag: "v1.2.3", SHA: "0123456789abcdef0123", Branch: "dev", Release: "main", Label: autotag.LabelSkip, Login: "bot[bot]",
		WorkItem: 12, Detail: "boom",
	}
	seen := map[msgID]string{}
	for _, code := range autotag.ReasonCodes() {
		n := autotag.Note{Code: code, Params: p}
		got := autoTagNoteText(n, "原样的说明")
		if got.id == "" {
			t.Errorf("%s 没有对应的消息", code)
			continue
		}
		if _, ok := messages[got.id]; !ok {
			t.Errorf("%s 的消息 %q 没有登记", code, got.id)
		}
		if prev, dup := seen[got.id]; dup {
			t.Errorf("%s 与 %s 是同一条消息 %q", code, prev, got.id)
		}
		seen[got.id] = code
		zh := textZH(t, got)
		if want := n.Text(); want != "" && zh != want {
			t.Errorf("%s：页面上是 %q，领域层的说明是 %q", code, zh, want)
		}
	}
	// AI 开发给出的归属说明（中文）与页面上的中文一致
	for code, want := range map[string]string{
		autotag.ReasonOriginChecking:     "AI 开发工作项 #12 的 PR 创建归属正在核验，确认后可重试",
		autotag.ReasonOriginUnconfirmed:  "AI 开发工作项 #12 的 PR 创建归属尚未确认，确认后可重试",
		autotag.ReasonOriginHeadMismatch: "PR 的来源分支与 AI 开发工作项 #12 的记录不一致，不自动发布",
		autotag.ReasonOriginBaseMismatch: "PR 的目标分支与 AI 开发工作项 #12 的记录不一致，不自动发布",
		autotag.ReasonOriginStopped:      "AI 开发工作项 #12 已终止，不自动发布",
		autotag.ReasonOriginNoChange:     "AI 开发工作项 #12 没有改动，不自动发布",
		autotag.ReasonOriginNoRecord:     "AI 开发工作项 #12 缺少领取或交付记录，不自动发布",
	} {
		if zh := textZH(t, autoTagNoteText(autotag.Note{Code: code, Params: p}, "")); zh != want {
			t.Errorf("%s：页面上是 %q，应为 %q", code, zh, want)
		}
	}
	for _, code := range []string{"", "future"} {
		if got := autoTagNoteText(autotag.Note{Code: code}, "暂时失败，稍后重试：x"); !isRaw(got, "暂时失败，稍后重试：x") {
			t.Errorf("原因码 %q：%+v", code, got)
		}
	}
}

// TestMergeQueueStatusTone：已合并的项按原因码（旧记录按 issues_state 和失败次数）给出语义级别，不看中文说明。
func TestMergeQueueStatusTone(t *testing.T) {
	merged := func(code string, p mergequeue.NoteParams, issues string, tries int) *mergequeue.Entry {
		return &mergequeue.Entry{
			State: mergequeue.StateMerged, StatusNote: mergequeue.Note{Code: code, Params: p}, IssuesState: issues, IssuesTries: tries,
			// 中文说明里有没有"失败"都不影响判断
			Status: "随便什么",
		}
	}
	cases := []struct {
		name string
		e    *mergequeue.Entry
		want string
	}{
		{"正在关闭", merged(mergequeue.StatusClosingIssues, mergequeue.NoteParams{}, "pending", 0), toneNone},
		{"已关闭", merged(mergequeue.StatusIssuesDone, mergequeue.NoteParams{Closed: []int{3}}, "done", 0), toneNone},
		{"结果不明", merged(mergequeue.StatusIssuesDone, mergequeue.NoteParams{Unknown: []int{3}}, "done", 0), toneWarn},
		{"失败重试中", merged(mergequeue.StatusIssuesRetry, mergequeue.NoteParams{Tries: 1}, "pending", 1), toneBad},
		{"放弃", merged(mergequeue.StatusIssuesGaveUp, mergequeue.NoteParams{Tries: 5}, "failed", 5), toneBad},
		{"仓库停用", merged(mergequeue.StatusIssuesInactive, mergequeue.NoteParams{}, "failed", 0), toneBad},
		{"旧记录：放弃", merged("", mergequeue.NoteParams{}, "failed", 3), toneBad},
		{"旧记录：失败重试中", merged("", mergequeue.NoteParams{}, "pending", 1), toneBad},
		{"旧记录：正在关闭", merged("", mergequeue.NoteParams{}, "pending", 0), toneNone},
		{"旧记录：已完成", merged("", mergequeue.NoteParams{}, "done", 0), toneNone},
		{"排队中", &mergequeue.Entry{State: mergequeue.StateQueued, StatusNote: mergequeue.Note{Code: mergequeue.StatusUpdateRetry}}, toneNone},
	}
	for _, c := range cases {
		if got := mqStatusTone(c.e); got != c.want {
			t.Errorf("%s：%q，应为 %q", c.name, got, c.want)
		}
	}
}

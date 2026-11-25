package web

import (
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// TestDocsIssueText：设计依据记录不一致的每个原因码都映射为自己的消息（不是原样的中文），编号作为参数传递；
// 页面上的中文与领域层给 CLI、日志的说明（DocsIssue.Detail）一致，同一个判断给出两种文字。
func TestDocsIssueText(t *testing.T) {
	cases := []struct {
		e    ai.DocsIssue
		want msgID
	}{
		{ai.DocsIssue{Code: ai.IssueSnapshotSetMissing, SnapshotSetID: 7}, msgIssueSetMissing},
		{ai.DocsIssue{Code: ai.IssueSnapshotSetOtherRepo, SnapshotSetID: 7}, msgIssueSetOtherRepo},
		{ai.DocsIssue{Code: ai.IssueSnapshotSetDigest}, msgIssueSetDigest},
		{ai.DocsIssue{Code: ai.IssueRequestSetDigest}, msgIssueReqSetDigest},
		{ai.DocsIssue{Code: ai.IssueRequestWorkItemSnapshot}, msgIssueReqItemSnapshot},
		{ai.DocsIssue{Code: ai.IssueRequestWorkItemDigest}, msgIssueReqItemDigest},
		{ai.DocsIssue{Code: ai.IssueRequestSourceUnknown}, msgIssueReqSourceUnknown},
		{ai.DocsIssue{Code: ai.IssueBasisDigestInvalid}, msgIssueBasisDigest},
		{ai.DocsIssue{Code: ai.IssueTaskWorkItemMissing, TaskID: 3, WorkItemID: 9}, msgIssueTaskItemMissing},
		{ai.DocsIssue{Code: ai.IssueTaskWorkItemOtherRepo, TaskID: 3, WorkItemID: 9}, msgIssueTaskItemOtherRepo},
		{ai.DocsIssue{Code: ai.IssueTaskWorkItemSnapshot, TaskID: 3}, msgIssueTaskItemSnapshot},
		{ai.DocsIssue{Code: ai.IssueTaskDevRequestMismatch, TaskID: 3, RequestID: 5}, msgIssueTaskDevRequest},
		{ai.DocsIssue{Code: ai.IssueTaskNoWorkItem, TaskID: 3}, msgIssueTaskNoWorkItem},
		{ai.DocsIssue{Code: ai.IssueTaskClaimMismatch, TaskID: 3}, msgIssueTaskClaim},
		{ai.DocsIssue{Code: ai.IssueTaskSpecMismatch, TaskID: 3}, msgIssueTaskSpec},
		{ai.DocsIssue{Code: ai.IssueTaskSpecDigest, TaskID: 3}, msgIssueTaskSpecDigest},
		{ai.DocsIssue{Code: ai.IssueTaskRefsMismatch, TaskID: 3}, msgIssueTaskRefsMismatch},
		{ai.DocsIssue{Code: ai.IssueTaskRefsUnexpected, TaskID: 3}, msgIssueTaskRefsExtra},
	}
	seen := map[msgID]bool{}
	for _, c := range cases {
		got := docsIssueText(&c.e)
		if got.id != c.want {
			t.Errorf("%s：消息 %q，应为 %q", c.e.Code, got.id, c.want)
			continue
		}
		if seen[got.id] {
			t.Errorf("%s：消息 %q 重复", c.e.Code, got.id)
		}
		seen[got.id] = true
		if zh := textZH(t, got); zh != c.e.Detail() {
			t.Errorf("%s：页面上是 %q，领域层的说明是 %q", c.e.Code, zh, c.e.Detail())
		}
	}
	// 不认识的原因码（版本差异）原样显示领域层的说明；没有原因码的错误（例如 spec 无法解析）原样显示错误原文
	if got := docsIssueText(&ai.DocsIssue{Code: "future"}); got.id != "" || got.raw != "future" {
		t.Errorf("不认识的原因码：%+v", got)
	}
	if got := docsErrorText(nil, "AI 任务 #3 的 spec 无法解析"); got.id != "" || got.raw != "AI 任务 #3 的 spec 无法解析" {
		t.Errorf("没有原因码：%+v", got)
	}
	if !docsErrorText(nil, "").IsZero() {
		t.Error("没有错误时应为空")
	}
}

// TestRelationNoteText：关联任务的每个依据关系说明都有自己的消息，中文与领域层的说明（RelationNote.String）一致。
func TestRelationNoteText(t *testing.T) {
	notes := ai.RelationNotes()
	if len(notes) != len(relationNoteMsgs) {
		t.Errorf("领域层有 %d 个关系说明，页面登记了 %d 个", len(notes), len(relationNoteMsgs))
	}
	for _, n := range notes {
		got := relationNoteText(n, "")
		if got.id == "" {
			t.Errorf("%s 没有对应的消息", n)
			continue
		}
		if zh := textZH(t, got); zh != n.String() {
			t.Errorf("%s：页面上是 %q，领域层的说明是 %q", n, zh, n.String())
		}
	}
	if got := relationNoteText("future", "原样的说明"); got.id != "" || got.raw != "原样的说明" {
		t.Errorf("不认识的原因码：%+v", got)
	}
}

// TestIntegrityText：快照完整性问题的每个原因码都映射为消息（篇数是整数参数），中文与 IntegrityIssue.String 一致。
func TestIntegrityText(t *testing.T) {
	for _, i := range []docsync.IntegrityIssue{
		{Code: docsync.IntegrityCountMismatch, Count: 5, Actual: 1},
		{Code: docsync.IntegrityDocDigestInvalid, DocID: "d1"},
		{Code: docsync.IntegrityManifestInvalid},
		{Code: docsync.IntegrityManifestMismatch},
	} {
		got := integrityText(&docsync.SnapshotView{Integrity: &i, IntegrityError: i.String()})
		if got.id == "" {
			t.Errorf("%s 没有对应的消息", i.Code)
			continue
		}
		if zh := textZH(t, got); zh != i.String() {
			t.Errorf("%s：页面上是 %q，领域层的说明是 %q", i.Code, zh, i.String())
		}
	}
	if got := integrityText(&docsync.SnapshotView{}); !got.IsZero() {
		t.Errorf("没有问题时应为空：%+v", got)
	}
	unknown := docsync.IntegrityIssue{Code: "future"}
	if got := integrityText(&docsync.SnapshotView{Integrity: &unknown}); got.raw != "future" {
		t.Errorf("不认识的原因码：%+v", got)
	}
}

// TestAITitles：页面标题按请求类型给出消息，任务名（原样的内容）作为参数。
func TestAITitles(t *testing.T) {
	if got := textZH(t, aiRequestTitle(agentcfg.TaskReview, 12)); got != "AI 审计请求 #12" {
		t.Errorf("审计请求的标题 = %q", got)
	}
	if got := textZH(t, aiRequestTitle(agentcfg.TaskDevelop, 13)); got != "AI 开发请求 #13" {
		t.Errorf("开发请求的标题 = %q", got)
	}
	task := &ai.Task{Type: agentcfg.TaskDevelop, DisplayName: "claude · 选题", RequestID: 13}
	if got := textZH(t, aiTaskTitle(task)); got != "claude · 选题 · AI 开发请求 #13" {
		t.Errorf("任务的标题 = %q", got)
	}
}

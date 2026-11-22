package ingest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/gitref"
)

// recordingAIDev 记录 ingest 交给 AI 开发的事件（AIDev）。
type recordingAIDev struct {
	mu     sync.Mutex
	issues []IssueEvent
	closed []string
}

func (a *recordingAIDev) IssueEvent(_ context.Context, _ int64, ev IssueEvent) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issues = append(a.issues, ev)
	return "", nil
}

func (a *recordingAIDev) PullClosed(_ context.Context, _ int64, pr int, headRef, headSHA string, merged bool, mergeSHA, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = append(a.closed, fmt.Sprintf("#%d %s %s merged=%v %s", pr, headRef, headSHA, merged, mergeSHA))
	return "", nil
}

func (a *recordingAIDev) takeClosed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.closed
	a.closed = nil
	return out
}

func (a *recordingAIDev) PullHead(context.Context, int64, int, string) error { return nil }

func (a *recordingAIDev) take() []IssueEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.issues
	a.issues = nil
	return out
}

// TestIssueEventsToAIDev：issues 事件连同 Issue 的当前内容交给 AI 开发；新建时就带 kiln:ready 的 Issue 只有 opened 事件，
// 也要交过去。PR 的 issues 事件不交。
func TestIssueEventsToAIDev(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main"))) // 让 Kiln 认识这个仓库
	updated := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	issue := func(action string, extra map[string]any) map[string]any {
		is := map[string]any{
			"number": 42, "title": "新需求", "body": "内容", "state": "open", "html_url": "https://github.com/tester/demo/issues/42",
			"labels": []any{map[string]any{"name": "kiln:ready"}}, "updated_at": updated.Format(time.RFC3339),
		}
		maps.Copy(is, extra)
		return map[string]any{
			"action": action, "installation": map[string]any{"id": h.fake.InstallationID},
			"repository": h.fake.RepositoryPayload(h.demo, "x")["repository"], "issue": is, "sender": map[string]any{"login": "alice"},
		}
	}
	h.deliverOK("issues", issue("opened", nil))
	got := h.aiDev.take()
	if len(got) != 1 {
		t.Fatalf("opened 交给 AI 开发 %d 次，应为 1", len(got))
	}
	if ev := got[0]; ev.Action != "opened" || ev.Number != 42 || !ev.Open || !slices.Equal(ev.Labels, []string{"kiln:ready"}) ||
		ev.Title != "新需求" || ev.Body != "内容" || !ev.UpdatedAt.Equal(updated) || ev.Sender != "alice" {
		t.Fatalf("交给 AI 开发的事件 = %+v", ev)
	}
	h.deliverOK("issues", issue("opened", map[string]any{"pull_request": map[string]any{}}))
	if got := h.aiDev.take(); len(got) != 0 {
		t.Fatalf("PR 的 issues 事件交给了 AI 开发：%+v", got)
	}
}

// TestPullClosedToAIDev：PR 关闭事件连同来源分支交给 AI 开发（Server 记下 PR 编号之前按正式分支关联工作项）；fork 来的 PR 的分支不算，交空。合并时带上合并得到的 commit（设计 PR 合并后记为 design_sha，PLAN #31.3）。
func TestPullClosedToAIDev(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverOK("push", h.pushPayload(h.demo, "refs/heads/main", gitref.ZeroSHA, h.git.head("main"))) // 让 Kiln 认识这个仓库
	head := h.git.head("main")
	closed := func(pr int, merged bool) map[string]any {
		p := h.prPayload(h.demo, "closed", pr, "kiln/dev-3", head, "main")
		field(t, p, "pull_request")["merged"] = merged
		if merged {
			field(t, p, "pull_request")["merge_commit_sha"] = head
		}
		return p
	}
	h.deliverOK("pull_request", closed(7, false))
	h.deliverOK("pull_request", closed(8, true))
	fork := closed(9, false)
	field(t, field(t, fork, "pull_request"), "head")["repo"] = map[string]any{"id": 999, "full_name": "someone/demo"}
	h.deliverOK("pull_request", fork)
	// 合并时带上 PR 的 head（完成度按合并的那次交付判断，PLAN #31.3）
	want := []string{"#7 kiln/dev-3  merged=false ", "#8 kiln/dev-3 " + head + " merged=true " + head, "#9   merged=false "}
	if got := h.aiDev.takeClosed(); !slices.Equal(got, want) {
		t.Fatalf("交给 AI 开发的 PR 关闭 = %q，想要 %q", got, want)
	}
}

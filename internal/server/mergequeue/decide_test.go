package mergequeue

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// req 解析 required 的各项（测试数据）。
func req(items ...string) []mergecfg.Requirement {
	var out []mergecfg.Requirement
	for _, it := range items {
		w, j, _ := strings.Cut(it, "/")
		out = append(out, mergecfg.Requirement{Workflow: w, Job: j})
	}
	return out
}

// ciRun 是 .kiln/workflows/ci.yml 的 run；aiRun 是 AI 审计的 run。
func ciRun(state store.RunState, jobs ...jobInfo) runInfo {
	return runInfo{ID: 1, Name: "ci", State: state, Source: "ci", Jobs: jobs}
}

func aiRun(state store.RunState) runInfo {
	return runInfo{ID: 2, Name: "AI 审计", State: state, Source: mergecfg.AIReview, Builtin: true}
}

// TestDecide 覆盖 PLAN #28.3 决策表的每一行。
func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	stale := now.Add(-updateTimeout - time.Second)
	const head, newHead, base = "aaaa", "bbbb", "cccc"
	openPull := func(f func(p *forge.Pull)) *forge.Pull {
		p := &forge.Pull{Number: 7, State: forge.PullOpen, HeadSHA: head, BaseRef: "main", Labels: []string{Label}}
		if f != nil {
			f(p)
		}
		return p
	}
	passed := []runInfo{{ID: 1, Name: "CI", State: store.RunSucceeded}, {ID: 2, Name: "AI 审计", State: store.RunSucceeded}}
	tests := []struct {
		name string
		s    snapshot
		want decision
		// reason 非空时只比较原因中是否包含它（原因较长）
		reason string
	}{
		{
			name: "已合并",
			s:    snapshot{Pull: openPull(func(p *forge.Pull) { p.State, p.MergeSHA = forge.PullMerged, "m" })},
			want: decision{Kind: actMerged},
		},
		{
			name: "已关闭（还有 label）",
			s:    snapshot{Pull: openPull(func(p *forge.Pull) { p.State = forge.PullClosed })},
			want: decision{Kind: actRemove, Reason: "PR 已关闭", DropLabel: true},
		},
		{
			name: "已关闭（没有 label）",
			s:    snapshot{Pull: openPull(func(p *forge.Pull) { p.State, p.Labels = forge.PullClosed, nil })},
			want: decision{Kind: actRemove, Reason: "PR 已关闭"},
		},
		{
			name: "继续没有完成的自动出队（label 已去掉）",
			s: snapshot{
				Entry: &Entry{HeadSHA: head, Action: actionRemoving, Reason: "验证没有通过"},
				Pull:  openPull(func(p *forge.Pull) { p.Labels = nil }),
			},
			want: decision{Kind: actRemove, Reason: "验证没有通过", Comment: true},
		},
		{
			name: "去掉了 label",
			s:    snapshot{Pull: openPull(func(p *forge.Pull) { p.Labels = []string{"other"} })},
			want: decision{Kind: actRemove, Reason: "去掉了 " + Label + " label"},
		},
		{
			name:   "fork",
			s:      snapshot{Pull: openPull(func(p *forge.Pull) { p.Fork = true })},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "fork",
		},
		{
			// 目标分支是否允许合并由执行者按 .kiln/merge.yml 判断（BranchRemoval，PLAN #28.5），decide 本身只看验证
			name: "目标分支不是默认分支",
			s:    snapshot{Pull: openPull(func(p *forge.Pull) { p.BaseRef = "release" })},
			want: wait(notPlannedStatus("release")),
		},
		{
			name:   "draft",
			s:      snapshot{Pull: openPull(func(p *forge.Pull) { p.Draft = true })},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "draft",
		},
		{
			name: "入队时不知道 head",
			s:    snapshot{Entry: &Entry{}, Pull: openPull(nil)},
			want: decision{Kind: actAcceptHead, Head: head},
		},
		{
			name:   "用户推送了新 commit",
			s:      snapshot{Pull: openPull(func(p *forge.Pull) { p.HeadSHA = newHead })},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "推送了新的 commit",
		},
		{
			name: "队列更新产生的新 head",
			s: snapshot{
				Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head, ActionAt: &recent},
				Pull:  openPull(func(p *forge.Pull) { p.HeadSHA = newHead }), Parents: []string{head, base},
			},
			want: decision{Kind: actAcceptHead, Head: newHead},
		},
		{
			name: "正在更新时用户推送了普通 commit",
			s: snapshot{
				Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head, ActionAt: &recent},
				Pull:  openPull(func(p *forge.Pull) { p.HeadSHA = newHead }), Parents: []string{head},
			},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "推送了新的 commit",
		},
		{
			name: "正在更新，新 head 还没到",
			s:    snapshot{Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head, ActionAt: &recent, ActionTries: 1}, Pull: openPull(nil)},
			want: wait(Note{Code: StatusUpdating}),
		},
		{
			name: "更新超时，重新请求",
			s:    snapshot{Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head, ActionAt: &stale, ActionTries: 1}, Pull: openPull(nil)},
			want: decision{Kind: actUpdate},
		},
		{
			name:   "更新超时次数用完",
			s:      snapshot{Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head, ActionAt: &stale, ActionTries: maxUpdateTries}, Pull: openPull(nil)},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "都没有完成",
		},
		{
			name: "上一次请求暂时失败，新 head 没到：立即重新请求",
			s:    snapshot{Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head}, Pull: openPull(nil)},
			want: decision{Kind: actUpdate},
		},
		{
			name: "上一次请求暂时失败，但平台已经完成更新",
			s: snapshot{
				Entry: &Entry{HeadSHA: head, Action: actionUpdating, ActionHead: head},
				Pull:  openPull(func(p *forge.Pull) { p.HeadSHA = newHead }), Parents: []string{head, base},
			},
			want: decision{Kind: actAcceptHead, Head: newHead},
		},
		{
			name: "配置错误",
			s:    snapshot{Pull: openPull(nil), ConfigErr: "method 只能是", Processed: true, Runs: passed},
			want: wait(configErrorStatus("method 只能是")),
		},
		{
			name: "还没有按默认分支规划这个 head",
			s:    snapshot{Pull: openPull(nil)},
			want: wait(notPlannedStatus("main")),
		},
		{
			name: "验证中",
			s:    snapshot{Pull: openPull(nil), Processed: true, Runs: []runInfo{passed[0], {ID: 2, Name: "AI 审计", State: store.RunActive}}},
			want: wait(Note{Code: StatusWaitRuns, Params: NoteParams{Done: 1, Total: 2}}),
		},
		{
			name: "验证失败（有的还在跑也出队）",
			s: snapshot{Pull: openPull(nil), Processed: true, Runs: []runInfo{
				{ID: 1, Name: "CI", State: store.RunFailed}, {ID: 2, Name: "AI 审计", State: store.RunActive},
			}},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "验证没有通过：CI（run #1，failed）",
		},
		{
			name:   "取消的 run 也算没有通过",
			s:      snapshot{Pull: openPull(nil), Processed: true, Runs: []runInfo{{ID: 3, Name: "CI", State: store.RunCancelled}}},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "cancelled",
		},
		{
			name: "通过但落后",
			s:    snapshot{Pull: openPull(nil), Processed: true, Runs: passed, Behind: 2},
			want: decision{Kind: actUpdate},
		},
		{
			name: "通过且不落后",
			s:    snapshot{Pull: openPull(nil), Processed: true, Runs: passed},
			want: decision{Kind: actMerge},
		},
		{
			name: "跳过的 run 不影响合并",
			s:    snapshot{Pull: openPull(nil), Processed: true, Runs: []runInfo{{ID: 1, Name: "CI", State: store.RunSkipped}, passed[1]}},
			want: decision{Kind: actMerge},
		},
		{
			// PR 把自己 head 中的 paths 改成匹配不到任何文件，run 被跳过
			name:   "只有跳过的 run",
			s:      snapshot{Pull: openPull(nil), Processed: true, Runs: []runInfo{{ID: 1, Name: "CI", State: store.RunSkipped}}},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "没有任何验证",
		},
		{
			// #115：以前直接合并，PR 删掉 .kiln/workflows 中的 workflow 就能绕过验证
			name:   "没有任何 run",
			s:      snapshot{Pull: openPull(nil), Processed: true},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "没有任何验证",
		},
		{
			name:   "必需的 workflow 没有 run（PR 删掉了它）",
			s:      snapshot{Pull: openPull(nil), Processed: true, Required: req("ci/test", "ai-review"), Runs: []runInfo{aiRun(store.RunSucceeded)}},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "缺少必需的验证：ci/test",
		},
		{
			name: "必需的 job 不存在（PR 删掉或改名了它），不等其他 run 结束",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ci/test"),
				Runs: []runInfo{ciRun(store.RunActive, jobInfo{"lint", store.JobRunning}), aiRun(store.RunActive)},
			},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "缺少必需的验证：ci/test",
		},
		{
			name: "必需的验证还在跑",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ci/test"),
				Runs: []runInfo{ciRun(store.RunActive, jobInfo{"test", store.JobRunning})},
			},
			want: wait(Note{Code: StatusWaitRuns, Params: NoteParams{Total: 1}}),
		},
		{
			name: "必需的 job 被跳过（run 仍算成功）",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ci/test"),
				Runs: []runInfo{ciRun(store.RunSucceeded, jobInfo{"test", store.JobSkipped}, jobInfo{"lint", store.JobSucceeded})},
			},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "必需的验证没有成功：ci/test",
		},
		{
			name: "必需的矩阵 job 有一个变体被跳过",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ci/build"),
				Runs: []runInfo{ciRun(store.RunSucceeded, jobInfo{"build", store.JobSucceeded}, jobInfo{"build", store.JobSkipped})},
			},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "ci/build",
		},
		{
			// 普通 workflow ai-review.yml 不能冒充 AI 审计（PR 删掉 AI 审计配置、加上一个什么都不做的 ai-review.yml）
			name: "普通 workflow ai-review.yml 不算 AI 审计",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ai-review"),
				Runs: []runInfo{{ID: 3, Name: "ai-review", State: store.RunSucceeded, Source: "ai-review"}},
			},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "缺少必需的验证：ai-review",
		},
		{
			name:   "必需的 run 被跳过",
			s:      snapshot{Pull: openPull(nil), Processed: true, Required: req("ai-review"), Runs: []runInfo{ciRun(store.RunSucceeded), aiRun(store.RunSkipped)}},
			want:   decision{Kind: actRemove, Comment: true, DropLabel: true},
			reason: "必需的验证没有成功：ai-review",
		},
		{
			name: "必需的验证全部成功（矩阵的全部变体）",
			s: snapshot{
				Pull: openPull(nil), Processed: true, Required: req("ci", "ci/build", "ai-review"),
				Runs: []runInfo{ciRun(store.RunSucceeded, jobInfo{"build", store.JobSucceeded}, jobInfo{"build", store.JobSucceeded}), aiRun(store.RunSucceeded)},
			},
			want: decision{Kind: actMerge},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.s
			if s.Entry == nil {
				s.Entry = &Entry{HeadSHA: head, Action: actionNone}
			}
			s.DefaultBranch, s.Now = "main", now
			got := decide(&s)
			// 原因码和中文说明来自同一个判断：有原因码时中文说明就是它的 Text（继续旧行的自动出队时沿用记下的说明）
			for _, n := range []struct {
				text string
				note Note
			}{{got.Status, got.StatusNote}, {got.Reason, got.ReasonNote}} {
				if n.note.Code != "" && n.note.Text() != n.text {
					t.Fatalf("原因码 %s 的说明 %q，记下的是 %q", n.note.Code, n.note.Text(), n.text)
				}
			}
			if got.Kind == actRemove && got.ReasonNote.Code == "" && s.Entry.Action != actionRemoving {
				t.Fatalf("新的出队原因没有原因码：%+v", got)
			}
			got.StatusNote, got.ReasonNote = Note{}, Note{}
			tt.want.StatusNote, tt.want.ReasonNote = Note{}, Note{}
			if tt.reason != "" {
				if !strings.Contains(got.Reason, tt.reason) {
					t.Fatalf("原因 %q 应包含 %q", got.Reason, tt.reason)
				}
				got.Reason = ""
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("decide() = %+v，想要 %+v", got, tt.want)
			}
		})
	}
}

// TestDecidePullNotHead：不在队首的项只看 PR 本身；没有问题时返回 false（由调用方显示排在第几个）。
func TestDecidePullNotHead(t *testing.T) {
	s := &snapshot{
		Entry:         &Entry{HeadSHA: "aaaa"},
		Pull:          &forge.Pull{State: forge.PullOpen, HeadSHA: "aaaa", BaseRef: "main", Labels: []string{Label}},
		DefaultBranch: "main",
	}
	if d, ok := decidePull(s); ok {
		t.Fatalf("PR 没有问题时 decidePull = %+v", d)
	}
	s.Pull.Draft = true
	if d, ok := decidePull(s); !ok || d.Kind != actRemove {
		t.Fatalf("draft 的 PR 应出队：%+v", d)
	}
}

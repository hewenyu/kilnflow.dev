package ingest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// recordingAutoTag 记录 ingest 记下的合并 commit（AutoTag）。
type recordingAutoTag struct {
	mu   sync.Mutex
	recs []mqevents.AutoTagRecord
	fail bool
}

func (a *recordingAutoTag) Record(_ context.Context, r mqevents.AutoTagRecord) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return false, errors.New("写入失败（注入）")
	}
	added := !slices.ContainsFunc(a.recs, func(x mqevents.AutoTagRecord) bool { return x.SHA == r.SHA })
	if added {
		a.recs = append(a.recs, r)
	}
	return added, nil
}

func (a *recordingAutoTag) take() []mqevents.AutoTagRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.recs
	a.recs = nil
	return out
}

// closedPull 返回 PR pr 以 base 合入（merged 为 false 时不合并）的 pull_request.closed 事件：sender 是事件发送者，
// mergedBy 非空时写入 merged_by，label 固定带 release:minor 和 bug。
func (h *harness) closedPull(pr int, head, base string, merged bool, sender, senderType, mergedBy, mergedByType string) map[string]any {
	h.t.Helper()
	p := h.prPayload(h.demo, "closed", pr, "feature", head, base)
	pull := field(h.t, p, "pull_request")
	pull["state"] = "closed"
	pull["merged"] = merged
	if merged {
		pull["merge_commit_sha"] = mergedPullSHA
		pull["merged_by"] = map[string]any{"login": mergedBy, "id": 2, "type": mergedByType}
	}
	pull["labels"] = []any{map[string]any{"name": "release:minor"}, map[string]any{"name": "bug"}}
	return withSender(p, sender, senderType)
}

// mergedPullSHA 是这些测试中 PR 的合并提交。
const mergedPullSHA = "1234567890123456789012345678901234567890"

// TestMergedPullRecorded（PLAN #29.4，T4）：经过 PR 合入的 commit 由 pull_request.closed 记下，并写清目标分支与合并那一刻
// 的默认分支（是否发版由自动打 tag 按 .kiln/merge.yml 裁决，PLAN #28.5）。没有合并、fork 来的、由本 App（合并队列，自己
// 在合并的事务中记录）合并的，以及队列没有记录的本 App 合并不重复记。
func TestMergedPullRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := &recordingAutoTag{}
	h.ing.autoTag = rec
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	h.git.git("checkout", "-q", "main")
	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "main")) // 让 Kiln 认识这个仓库

	got := h.deliverOK("pull_request", h.closedPull(7, head, "main", true, "bob", "User", "bob", "User"))
	if !strings.Contains(got, "已记下合并的 commit") {
		t.Fatalf("结果：%q", got)
	}
	want := mqevents.AutoTagRecord{
		RepoID: demoRepoID, SHA: mergedPullSHA, PR: 7, Labels: []string{"release:minor", "bug"}, MergedBy: "bob",
		HeadRef: "feature", BaseRef: "main", Source: mqevents.AutoTagFromWebhook,
	}
	if recs := rec.take(); len(recs) != 1 || recs[0].SHA != want.SHA || recs[0].PR != want.PR || recs[0].MergedBy != want.MergedBy ||
		recs[0].HeadRef != want.HeadRef || recs[0].BaseRef != want.BaseRef || recs[0].DefaultBranchAtMerge != "main" ||
		recs[0].Source != want.Source || recs[0].RepoID != want.RepoID || !slices.Equal(recs[0].Labels, want.Labels) {
		t.Fatalf("记录：%+v，想要 %+v", recs, want)
	}

	// fork 来的 PR（PLAN #16.1）：合入默认分支、合并者是人，也不记录
	fork := h.closedPull(17, head, "main", true, "bob", "User", "bob", "User")
	// head.repo 换成另一个仓库（payload 中 head 和 base 的 repo 是同一个对象，不能直接改 id）
	field(t, field(t, fork, "pull_request"), "head")["repo"] = map[string]any{"id": 999, "full_name": "someone/demo"}
	for name, p := range map[string]map[string]any{
		"没有合并":    h.closedPull(9, head, "main", false, "bob", "User", "", ""),
		"fork 来的": fork,
	} {
		h.deliverOK("pull_request", p)
		if recs := rec.take(); len(recs) != 0 {
			t.Errorf("%s：不应记录：%+v", name, recs)
		}
	}

	// 由合并队列合并的（merge_queue 中记着这次合并）不重复记：队列在合并的同一事务中已经按请求合并时的 label 记下
	h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, head_sha, enqueued_by, status, merge_sha, action, reason, finished_at)
		VALUES ($1,18,'merged',$2,'auto','',$3,'none','',now())`, demoRepoID, head, mergedPullSHA)
	h.deliverOK("pull_request", h.closedPull(18, head, "main", true, testBot, "Bot", testBot, "Bot"))
	if recs := rec.take(); len(recs) != 0 {
		t.Errorf("合并队列已经记录的不应重复记：%+v", recs)
	}
}

// TestMergedPullSenderFallback（PLAN #29.4）：手动合并的发送者取合并者（merged_by），合并者为空或者是机器人时取 PR 作者；
// 合并者和 PR 作者都是机器人（或者未知）时照实记录并标记 sender_is_bot，交给自动打 tag 裁决。队列没有记录的本 App 合并
// （结果不明、后来才看到已合并）照常记下，不静默丢失。
func TestMergedPullSenderFallback(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := &recordingAutoTag{}
	h.ing.autoTag = rec
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	h.git.git("checkout", "-q", "main")
	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "main"))

	author, ok := field(t, field(t, h.prPayload(h.demo, "closed", 1, "feature", head, "main"), "pull_request"), "user")["login"].(string)
	if !ok {
		t.Fatal("PR 作者的 login 不是字符串")
	}
	noMerger := h.closedPull(13, head, "main", true, "bob", "User", "", "")
	delete(field(t, noMerger, "pull_request"), "merged_by")
	for name, p := range map[string]map[string]any{
		"其他机器人合并": h.closedPull(11, head, "main", true, "mergify[bot]", "Bot", "mergify[bot]", "Bot"),
		"合并者为空":   h.closedPull(12, head, "main", true, "bob", "User", "", "User"),
		"没有合并者":   noMerger,
	} {
		h.deliverOK("pull_request", p)
		if recs := rec.take(); len(recs) != 1 || recs[0].MergedBy != author {
			t.Errorf("%s：应记录、发送者是 PR 作者 %s：%+v", name, author, recs)
		}
	}

	// 本 App 合并、合并队列却没有记录：发送者是 PR 作者（人）
	h.deliverOK("pull_request", h.closedPull(10, head, "main", true, testBot, "Bot", testBot, "Bot"))
	if recs := rec.take(); len(recs) != 1 || recs[0].MergedBy != author || recs[0].SenderIsBot {
		t.Errorf("队列没有记录的本 App 合并应记录、发送者是 PR 作者：%+v", recs)
	}

	// 合并者和 PR 作者都是机器人：照实记录并标记，交给自动打 tag 裁决（不静默跳过）
	botAuthor := h.closedPull(14, head, "main", true, "renovate[bot]", "Bot", "renovate[bot]", "Bot")
	field(t, botAuthor, "pull_request")["user"] = map[string]any{"login": "renovate[bot]", "id": 3, "type": "Bot"}
	h.deliverOK("pull_request", botAuthor)
	if recs := rec.take(); len(recs) != 1 || !recs[0].SenderIsBot || recs[0].MergedBy != "renovate[bot]" {
		t.Errorf("机器人的合并应记录并标记机器人发起：%+v", recs)
	}
}

// TestMergedPullTargetBranch（PLAN #28.5、#29.4）：记录的默认分支取事件中合并那一刻的值（默认分支改过名、事件晚到时也按
// 它裁决是否发版）；合入非默认分支的也记录，目标分支和默认分支都写清楚，由自动打 tag 跳过。
func TestMergedPullTargetBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := &recordingAutoTag{}
	h.ing.autoTag = rec
	h.git.git("checkout", "-q", "-b", "feature")
	head := h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
	h.git.git("checkout", "-q", "main")
	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", head, "main"))

	late := func(pr int, base string) map[string]any {
		p := h.closedPull(pr, head, base, true, "bob", "User", "bob", "User")
		field(t, p, "repository")["default_branch"] = "release"
		return p
	}
	h.deliverOK("pull_request", late(15, "release"))
	if recs := rec.take(); len(recs) != 1 || recs[0].PR != 15 || recs[0].BaseRef != "release" || recs[0].DefaultBranchAtMerge != "release" {
		t.Errorf("合入当时默认分支的应记录：%+v", recs)
	}
	h.deliverOK("pull_request", late(16, "main"))
	if recs := rec.take(); len(recs) != 1 || recs[0].PR != 16 || recs[0].BaseRef != "main" || recs[0].DefaultBranchAtMerge != "release" {
		t.Errorf("合入其他分支的也应记录（按合并当时的默认分支裁决）：%+v", recs)
	}

	// 合入非默认分支的也记录：由自动打 tag 跳过（不发布版本）
	h.deliverOK("pull_request", h.closedPull(8, head, "release", true, "bob", "User", "bob", "User"))
	if recs := rec.take(); len(recs) != 1 || recs[0].BaseRef != "release" || recs[0].DefaultBranchAtMerge != "main" {
		t.Errorf("合入其他分支的应记录：%+v", recs)
	}
}

// TestMergedPullRecordFailure：记录失败时事件稍后重试（不会漏掉合并的 commit）。
func TestMergedPullRecordFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := &recordingAutoTag{fail: true}
	h.ing.autoTag = rec
	h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 6, "main", h.git.head("main"), "main")) // 让 Kiln 认识这个仓库
	p := h.prPayload(h.demo, "closed", 7, "feature", h.git.head("main"), "main")
	pull := field(t, p, "pull_request")
	pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, h.git.head("main")
	pull["merged_by"] = map[string]any{"login": "bob", "id": 2, "type": "User"}
	ev := h.deliver("pull_request", p)
	if ev.Processed || !strings.Contains(ev.Error, "记录合并的 commit") {
		t.Fatalf("记录失败时事件应稍后重试：%+v", ev)
	}
}

// dbAutoTag 用真实的 mqevents.RecordAutoTag 写 Postgres：复现 webhook 与合并队列先后写同一个 commit 时的唯一约束冲突。
type dbAutoTag struct{ db *store.DB }

func (a dbAutoTag) Record(ctx context.Context, r mqevents.AutoTagRecord) (bool, error) {
	return mqevents.RecordAutoTag(ctx, a.db, r)
}

// autoTagAt 返回 auto_tags 中某个 commit 的记录（没有时 ok 为 false），用于断言 webhook 有没有抢先写。
func (h *harness) autoTagAt(sha string) (source string, labels []string, ok bool) {
	h.t.Helper()
	source, labels, _, _, ok = h.autoTagRow(sha)
	return source, labels, ok
}

// autoTagRow 返回 auto_tags 中某个 commit 的完整记录（没有时 ok 为 false）。
func (h *harness) autoTagRow(sha string) (source string, labels []string, mergedBy string, senderIsBot, ok bool) {
	h.t.Helper()
	err := h.db.QueryRow(h.ctx, `SELECT source, labels, merged_by, sender_is_bot FROM auto_tags WHERE repo_id=$1 AND sha=$2`,
		demoRepoID, sha).Scan(&source, &labels, &mergedBy, &senderIsBot)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, "", false, false
	}
	if err != nil {
		h.t.Fatalf("读 auto_tags：%v", err)
	}
	return source, labels, mergedBy, senderIsBot, true
}

// mergedClosed 返回一个合并的 pull_request.closed payload（labels 是事件中的快照）。merger 为 nil 时不写 merged_by
// （平台没有记录合并者）。
func mergedClosed(t *testing.T, h *harness, pr int, sha string, merger *gh.Account, labels []any) map[string]any {
	t.Helper()
	p := h.prPayload(h.demo, "closed", pr, "feature", h.git.head("feature"), "main")
	pull := field(t, p, "pull_request")
	pull["state"], pull["merged"], pull["merge_commit_sha"] = "closed", true, sha
	if merger != nil {
		pull["merged_by"] = map[string]any{"login": merger.Login, "id": 2, "type": merger.Type}
	}
	pull["labels"] = labels
	return p
}

// TestQueueMergedNotPreemptedWithoutAppIdentity（PLAN #29.4）：合并队列正在合并（merge_queue 中
// action='merging' 且已冻结 merge_labels）时，Server 重启、读取 App 信息失败（isSelf 暂时返回 false）后收到的本 App
// 合并 webhook 不能抢先用事件中未冻结的 label 记下 auto_tags——否则队列随后按冻结的 label 写时被唯一约束忽略，
// release:skip / release:major 等标记永久丢失。
func TestQueueMergedNotPreemptedWithoutAppIdentity(t *testing.T) {
	t.Parallel()
	const sha = "1234567890123456789012345678901234567890"
	for _, tt := range []struct {
		name          string
		identityReady bool
	}{
		{"App 身份未就绪", false},
		{"App 身份已就绪", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.ing.autoTag = dbAutoTag{db: h.db}
			if !tt.identityReady {
				// 模拟 App 信息还没读到：所有 login 都不是"自己"
				h.ing.isSelf = func(string) bool { return false }
			}
			h.git.git("checkout", "-q", "-b", "feature")
			h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
			h.git.git("checkout", "-q", "main")
			h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", h.git.head("feature"), "main"))
			// 队列请求合并：记下请求合并时的 label（release:skip 已冻结）
			h.exec(`INSERT INTO merge_queue (repo_id, pr_number, state, action, head_sha, enqueued_by, merge_labels)
				VALUES ($1,7,'queued','merging',$2,'auto',$3)`,
				demoRepoID, h.git.head("feature"), []string{"release:skip"})
			// 事件中的 label 是合并之后重新整理过的（这里为空），不能用来记录
			h.deliverOK("pull_request", withSender(mergedClosed(t, h, 7, sha, &gh.Account{Login: testBot, Type: "Bot"}, []any{}),
				testBot, "Bot"))
			if source, labels, ok := h.autoTagAt(sha); ok {
				t.Fatalf("队列拥有这次合并时 webhook 不应抢先记录：source=%s labels=%v", source, labels)
			}
			// 队列恢复落库：按冻结的 label 写，必须写进去并且保留
			added, err := mqevents.RecordAutoTag(h.ctx, h.db, mqevents.AutoTagRecord{
				RepoID: demoRepoID, SHA: sha, PR: 7, Labels: []string{"release:skip"}, MergedBy: testAccount,
				HeadRef: "feature", BaseRef: "main", Source: mqevents.AutoTagFromQueue,
			})
			if err != nil || !added {
				t.Fatalf("队列恢复写 auto_tags：added=%v err=%v", added, err)
			}
			source, labels, ok := h.autoTagAt(sha)
			if !ok || source != mqevents.AutoTagFromQueue || !slices.Equal(labels, []string{"release:skip"}) {
				t.Fatalf("冻结的 label 应保留：source=%s labels=%v ok=%v", source, labels, ok)
			}
		})
	}
}

// TestMergedPullQueueOwnershipBoundaries 覆盖 recordMerged 委托给合并队列的互补边界（PLAN #29.4）：
// 队列正在合并、但期间由人手动合并时 webhook 必须按事件快照记录（队列按"在队列之外合并"处理，不写 auto_tags）；
// 队列已经合并的（merge_sha 相同、没有出队原因）webhook 不重复写；队列没有记录的 Bot 合并（例如 AI 开发 PR、外部
// Bot）webhook 照常记录，不静默丢弃；合并者是机器人（类型 Bot，或者 login 带 [bot] 后缀）或者未知、队列又正在合并时，
// 一律委托队列，由队列按冻结的 label 记下——两边的判定必须互补，不能同时跳过。
func TestMergedPullQueueOwnershipBoundaries(t *testing.T) {
	t.Parallel()
	const sha = "1234567890123456789012345678901234567890"
	const mergingQueue = `INSERT INTO merge_queue (repo_id, pr_number, state, action, head_sha, enqueued_by, merge_labels)
		VALUES ($1,7,'queued','merging','head','auto',$2)`
	for _, tt := range []struct {
		name     string
		queueSQL string // 空表示没有队列记录
		// merger 是事件中的 merged_by；nil 表示 payload 中没有这个字段。
		merger *gh.Account
		// author 非 nil 时替换 PR 作者（nil 为 payload 默认的人）。
		author      *gh.Account
		want        bool
		wantSource  string
		wantLabels  []string
		wantMerged  string
		wantSenderB bool
	}{
		{
			name:     "队列正在合并时人手动合并",
			queueSQL: mergingQueue,
			merger:   &gh.Account{Login: "bob", Type: "User"},
			want:     true, wantSource: mqevents.AutoTagFromWebhook, wantLabels: []string{"release:minor"}, wantMerged: "bob",
		},
		{
			name: "队列已经合并",
			queueSQL: `INSERT INTO merge_queue (repo_id, pr_number, state, head_sha, enqueued_by, merge_sha, action, reason, finished_at)
				VALUES ($1,7,'merged','head','auto',$2,'none','',now())`,
			merger: &gh.Account{Login: testBot, Type: "Bot"},
			want:   false,
		},
		{
			name:   "队列没有记录的 Bot 作者与 Bot 合并",
			merger: &gh.Account{Login: "renovate[bot]", Type: "Bot"},
			author: &gh.Account{Login: "renovate[bot]", Type: "Bot"},
			want:   true, wantSource: mqevents.AutoTagFromWebhook, wantLabels: []string{"release:minor"},
			wantMerged: "renovate[bot]", wantSenderB: true,
		},
		{
			name:     "队列正在合并时合并者未知",
			queueSQL: mergingQueue,
			merger:   nil,
			want:     false,
		},
		{
			name:     "队列正在合并时合并者为空账号",
			queueSQL: mergingQueue,
			merger:   &gh.Account{},
			want:     false,
		},
		{
			name:     "队列正在合并时合并者是带后缀、没有类型的机器人",
			queueSQL: mergingQueue,
			merger:   &gh.Account{Login: "legacy-bot[bot]"},
			want:     false,
		},
		{
			name:     "队列正在合并时合并者是类型 Bot、没有后缀的机器人",
			queueSQL: mergingQueue,
			merger:   &gh.Account{Login: "legacy-bot", Type: "Bot"},
			want:     false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.ing.autoTag = dbAutoTag{db: h.db}
			// 都按 App 身份未就绪处理：判定不能依赖它
			h.ing.isSelf = func(string) bool { return false }
			h.git.git("checkout", "-q", "-b", "feature")
			h.git.commit(map[string]string{"f.txt": "1\n"}, "feature")
			h.git.git("checkout", "-q", "main")
			h.deliverOK("pull_request", h.prPayload(h.demo, "opened", 7, "feature", h.git.head("feature"), "main"))
			if tt.queueSQL != "" {
				if strings.Contains(tt.queueSQL, "merge_labels") {
					h.exec(tt.queueSQL, demoRepoID, []string{"release:skip"})
				} else {
					h.exec(tt.queueSQL, demoRepoID, sha)
				}
			}
			p := mergedClosed(t, h, 7, sha, tt.merger, []any{map[string]any{"name": "release:minor"}})
			if tt.author != nil {
				field(t, p, "pull_request")["user"] = map[string]any{"login": tt.author.Login, "id": 3, "type": tt.author.Type}
			}
			h.deliverOK("pull_request", p)
			source, labels, mergedBy, senderIsBot, ok := h.autoTagRow(sha)
			if ok != tt.want {
				t.Fatalf("webhook 记录=%v，想要 %v", ok, tt.want)
			}
			if !tt.want {
				return
			}
			if source != tt.wantSource || !slices.Equal(labels, tt.wantLabels) || mergedBy != tt.wantMerged || senderIsBot != tt.wantSenderB {
				t.Fatalf("记录：source=%s labels=%v merged_by=%q sender_is_bot=%v，想要 %s %v %q %v",
					source, labels, mergedBy, senderIsBot, tt.wantSource, tt.wantLabels, tt.wantMerged, tt.wantSenderB)
			}
		})
	}
}

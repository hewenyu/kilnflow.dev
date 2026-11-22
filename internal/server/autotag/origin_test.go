package autotag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自动打 tag 的来源裁决（PLAN #29.4）：人的合并总是允许；机器人发起时只有可信的 AI 开发实现工作项才允许。
// 授权检查在认出已有版本 tag 之前，Retry 不能绕过；异常记录（tagging/tagged 但没有可信归属）不继续打 tag 或规划发布。

// originFunc 是 OriginResolver 的替身。
type originFunc func(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (DevOrigin, error)

func (f originFunc) Resolve(ctx context.Context, q store.Querier, repoID int64, pr int, headRef, baseRef string) (DevOrigin, error) {
	return f(ctx, q, repoID, pr, headRef, baseRef)
}

// botRecord 记下一条机器人发起的合并（labels 是 PR 的 label）。
func (h *harness) botRecord(sha string, pr int, labels ...string) {
	h.t.Helper()
	if _, err := h.svc.Record(h.ctx, mqevents.AutoTagRecord{
		RepoID: repoID, SHA: sha, PR: pr, Labels: labels, MergedBy: "kiln-test[bot]", SenderIsBot: true,
		HeadRef: "kiln/dev-1", BaseRef: "main", Source: mqevents.AutoTagFromQueue,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func TestBotImplementationReleases(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginImplementation, WorkItemID: 12, Phase: "implement", ImplRound: 2, BaseRef: "main", Issues: []int{3, 4}}, nil
	})
	sha := h.commit(nil, "PR")
	h.botRecord(sha, 7, LabelMinor)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
	reqs := h.planner.requests()
	if len(reqs) != 1 || !reqs[0].SenderIsBot || reqs[0].Sender != "kiln-test[bot]" {
		t.Fatalf("规划请求应保留真实的机器人发送者：%+v", reqs)
	}
	var workItem *int64
	if err := h.db.QueryRow(h.ctx, `SELECT work_item_id FROM auto_tags WHERE repo_id=$1 AND sha=$2`, repoID, sha).Scan(&workItem); err != nil {
		t.Fatal(err)
	}
	if workItem == nil || *workItem != 12 {
		t.Fatalf("应记下工作项：%v", workItem)
	}
}

func TestBotUnknownSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginUnknown, Reason: "没有可信的 AI 开发工作项归属"}, nil
	})
	sha := h.commit(nil, "PR")
	h.botRecord(sha, 8)
	h.mustProcess()
	r := h.wantState(sha, StateSkipped, "")
	if !strings.Contains(r.Reason, "机器人") || !strings.Contains(r.Reason, "kiln-test[bot]") {
		t.Fatalf("原因：%s", r.Reason)
	}
	if h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
		t.Error("不应建 tag 或规划发布")
	}
}

// TestDesignSkippedWithoutSkipLabel：设计 PR 即使有人删掉了 release:skip（或者根本没加），也不打版本 tag。
func TestDesignSkippedWithoutSkipLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginDesign, WorkItemID: 5, Phase: "design", Reason: "这是 AI 开发工作项 #5 的设计 PR，不打版本 tag"}, nil
	})
	sha := h.commit(nil, "设计 PR")
	h.botRecord(sha, 9, LabelMinor, LabelMajor)
	h.mustProcess()
	r := h.wantState(sha, StateSkipped, "")
	if !strings.Contains(r.Reason, "设计 PR") || h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
		t.Fatalf("设计 PR 应跳过：%+v", r)
	}
}

// TestRecordingSkippedThenRetry：PR 创建归属还没确认时跳过；Server 补上归属之后 Retry 可以发版。
func TestRecordingSkippedThenRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginRecording, WorkItemID: 6, Reason: "AI 开发工作项 #6 的 PR 创建归属尚未确认，确认后可重试"}, nil
	})
	sha := h.commit(nil, "PR")
	h.botRecord(sha, 10)
	h.mustProcess()
	if r := h.wantState(sha, StateSkipped, ""); !strings.Contains(r.Reason, "尚未确认") {
		t.Fatalf("原因：%s", r.Reason)
	}
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginImplementation, WorkItemID: 6, Phase: "implement", ImplRound: 1, BaseRef: "main"}, nil
	})
	if err := h.svc.Retry(h.ctx, repoID, sha, testAccount); err != nil {
		t.Fatal(err)
	}
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
}

// TestDeniedSkipped：有归属但状态不满足（终止、没有改动等）时跳过。
func TestDeniedSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginDenied, WorkItemID: 7, Reason: "AI 开发工作项 #7 已终止，不自动发布"}, nil
	})
	sha := h.commit(nil, "PR")
	h.botRecord(sha, 11)
	h.mustProcess()
	if r := h.wantState(sha, StateSkipped, ""); !strings.Contains(r.Reason, "已终止") {
		t.Fatalf("原因：%s", r.Reason)
	}
}

// TestAuthorizationBeforeExistingTag（PLAN #29.4）：commit 已经有版本 tag 时，第三方 Bot / 未知来源仍然被拒绝
// ——授权检查在"认出已有 tag、认领它"之前，Retry 也不能绕过。
func TestAuthorizationBeforeExistingTag(t *testing.T) {
	t.Parallel()
	for name, kind := range map[string]OriginKind{"第三方 Bot": OriginUnknown, "归属未确认": OriginRecording} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tagConfig)
			h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
				return DevOrigin{Kind: kind, Reason: "没有可信的 AI 开发实现工作项归属"}, nil
			})
			sha := h.commit(nil, "PR")
			h.git("tag", "v1.0.0") // 这个 commit 已经有版本 tag
			h.botRecord(sha, 12)
			for range 2 { // 第一次处理 + Retry 之后
				h.mustProcess()
				h.wantState(sha, StateSkipped, "")
				if got := h.tagsAt(sha); len(got) != 1 || got[0] != "v1.0.0" {
					t.Fatalf("不应新建 tag：%v", got)
				}
				if h.countRequests(refsRoute) != 0 || len(h.planner.requests()) != 0 {
					t.Fatal("不应建 tag 或规划发布")
				}
				if err := h.svc.Retry(h.ctx, repoID, sha, testAccount); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestOriginResolveErrorKeepsPending：判断来源失败时保持 pending，由 outbox 重试（不能伪装成策略拒绝）。
func TestOriginResolveErrorKeepsPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{}, errors.New("数据库暂时不可用（注入）")
	})
	sha := h.commit(nil, "PR")
	h.record(sha, 13)
	if err := h.process(); err == nil {
		t.Fatal("应返回错误稍后重试")
	}
	if r := h.get(sha); r.State != StatePending || r.Tries != 1 {
		t.Fatalf("记录：%+v", r)
	}
}

// TestBotRecoveryProtection：机器人发起、又没有已落库的可信归属的异常记录（tagging/tagged）不继续打 tag、不规划发布。
func TestBotRecoveryProtection(t *testing.T) {
	t.Parallel()
	t.Run("tagging", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, tagConfig)
		sha := h.commit(nil, "PR")
		h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, source, state, tag)
			VALUES ($1,$2,14,'other[bot]',true,'webhook','tagging','v0.1.0')`, repoID, sha)
		h.mustProcess()
		if r := h.wantState(sha, StateFailed, "v0.1.0"); !strings.Contains(r.Reason, "机器人") {
			t.Fatalf("原因：%s", r.Reason)
		}
		if h.countRequests(refsRoute) != 0 {
			t.Error("不应创建 tag")
		}
	})
	t.Run("tagged", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, tagConfig)
		sha := h.commit(nil, "PR")
		h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, source, state, tag)
			VALUES ($1,$2,15,'other[bot]',true,'webhook','tagged','v0.1.1')`, repoID, sha)
		h.mustProcess()
		if r := h.wantState(sha, StateFailed, "v0.1.1"); !strings.Contains(r.Reason, "机器人") {
			t.Fatalf("原因：%s", r.Reason)
		}
		if len(h.planner.requests()) != 0 {
			t.Error("不应规划发布")
		}
	})
}

// TestSingleConnectionPoolWithOrigin：池上限为 1 时，来源判定必须用持有锁的连接（q），不能再借一个连接（会死锁）。
func TestSingleConnectionPoolWithOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	cfg := h.db.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(h.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := store.New(pool)
	h.svc.db, h.planner.db = db, db
	h.svc.devOrigin = originFunc(func(ctx context.Context, q store.Querier, _ int64, _ int, _, _ string) (DevOrigin, error) {
		var one int
		if err := q.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			return DevOrigin{}, err
		}
		return DevOrigin{Kind: OriginUnknown}, nil
	})
	sha := h.commit(nil, "PR")
	h.record(sha, 20)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.svc.Process(ctx, repoID); err != nil {
		t.Fatalf("Process：%v", err)
	}
	h.wantState(sha, StatePlanned, "v0.1.0")
}

// jsonString 把值编码成 JSON 文本（比较审计说明里的字段，数字类型不敏感）。
func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// auditDetail 返回动作 action 的最后一条审计日志的 detail。
func (h *harness) auditDetail(action string) map[string]any {
	h.t.Helper()
	var raw []byte
	if err := h.db.QueryRow(h.ctx, `SELECT detail FROM audit_log WHERE action=$1 ORDER BY id DESC LIMIT 1`, action).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		h.t.Fatalf("%s 的审计说明不是 JSON 对象：%v", action, err)
	}
	return m
}

// wantAudit 确认动作 action 的最后一条审计日志里有 want 中的字段（按 JSON 比较，数字类型不敏感）。
func (h *harness) wantAudit(action string, want map[string]any) {
	h.t.Helper()
	m := h.auditDetail(action)
	for k, v := range want {
		if jsonString(m[k]) != jsonString(v) {
			h.t.Errorf("%s 审计的 %s = %v，想要 %v（整条：%v）", action, k, m[k], v, m)
		}
	}
}

// TestWorkItemOnlyForTrustedOrigin：只有可信的实现 PR 才写 auto_tags.work_item_id 可信列；未知、归属未确认、
// 设计、拒绝都不写，诊断用的编号只留在审计说明和原因里。
func TestWorkItemOnlyForTrustedOrigin(t *testing.T) {
	t.Parallel()
	for name, origin := range map[string]DevOrigin{
		"未知来源":  {Kind: OriginUnknown, Reason: "没有可信的 AI 开发工作项归属"},
		"归属未确认": {Kind: OriginRecording, WorkItemID: 6, Reason: "AI 开发工作项 #6 的 PR 创建归属尚未确认"},
		"设计 PR": {Kind: OriginDesign, WorkItemID: 5, Phase: "design", Reason: "这是 AI 开发工作项 #5 的设计 PR，不打版本 tag"},
		"状态不满足": {Kind: OriginDenied, WorkItemID: 7, Reason: "AI 开发工作项 #7 已终止，不自动发布"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tagConfig)
			h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
				return origin, nil
			})
			sha := h.commit(nil, "PR")
			h.botRecord(sha, 21)
			h.mustProcess()
			h.wantState(sha, StateSkipped, "")
			if r := h.get(sha); r.WorkItemID != nil {
				t.Fatalf("不可信的来源不应写可信列：%d", *r.WorkItemID)
			}
			// 诊断用的编号仍出现在审计说明里（排查看得出来是哪个工作项）
			if origin.WorkItemID > 0 {
				h.wantAudit("autotag.skipped", map[string]any{"work_item_id": origin.WorkItemID})
			}
		})
	}
}

// TestRetryClearsUntrustedWorkItem：Retry 把记录重新置为 pending 时清掉上次认定的工作项；之后判定为未知时
// 也不写入可信列。
func TestRetryClearsUntrustedWorkItem(t *testing.T) {
	t.Parallel()
	t.Run("归属未确认到未知", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, tagConfig)
		h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
			return DevOrigin{Kind: OriginRecording, WorkItemID: 6, Reason: "AI 开发工作项 #6 的 PR 创建归属尚未确认"}, nil
		})
		sha := h.commit(nil, "PR")
		h.botRecord(sha, 22)
		h.mustProcess()
		h.wantState(sha, StateSkipped, "")
		if r := h.get(sha); r.WorkItemID != nil {
			t.Fatalf("未确认的归属不应写可信列：%d", *r.WorkItemID)
		}
		// 确认失败（例如平台上的 PR 不是本 App 创建的）：Retry 之后仍不写可信列
		h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
			return DevOrigin{Kind: OriginUnknown, Reason: "没有可信的 AI 开发工作项归属"}, nil
		})
		if err := h.svc.Retry(h.ctx, repoID, sha, testAccount); err != nil {
			t.Fatal(err)
		}
		h.mustProcess()
		h.wantState(sha, StateSkipped, "")
		if r := h.get(sha); r.WorkItemID != nil {
			t.Fatalf("未知来源不应写可信列：%d", *r.WorkItemID)
		}
	})
	t.Run("清掉旧的认定", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, tagConfig)
		sha := h.commit(nil, "PR")
		// 旧数据：跳过的记录上还留着以前认定的工作项
		h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, head_ref, base_ref, work_item_id, source, state)
			VALUES ($1,$2,23,'kiln-test[bot]',true,'kiln/dev-1','main',12,'webhook','skipped')`, repoID, sha)
		h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
			return DevOrigin{Kind: OriginUnknown, Reason: "没有可信的 AI 开发工作项归属"}, nil
		})
		if err := h.svc.Retry(h.ctx, repoID, sha, testAccount); err != nil {
			t.Fatal(err)
		}
		if r := h.get(sha); r.WorkItemID != nil {
			t.Fatalf("Retry 应清掉可信列：%d", *r.WorkItemID)
		}
		h.mustProcess()
		h.wantState(sha, StateSkipped, "")
		if r := h.get(sha); r.WorkItemID != nil {
			t.Fatalf("未知来源不应写可信列：%d", *r.WorkItemID)
		}
	})
}

// TestAuditReleaseBasis（PLAN #29.7）：autotag.tagged、autotag.planned 的审计说明带上发布依据——合并者、真实
// 发起者是不是机器人、来源，以及认定的 AI 开发工作项的阶段、轮次、Issue、创建者。
func TestAuditReleaseBasis(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{
			Kind: OriginImplementation, WorkItemID: 12, Phase: "implement", ImplRound: 2, BaseRef: "main",
			CreatorLogin: "kiln-test[bot]", Issues: []int{3, 4},
		}, nil
	})
	sha := h.commit(nil, "PR")
	h.botRecord(sha, 7, LabelMinor)
	h.mustProcess()
	h.wantState(sha, StatePlanned, "v0.1.0")
	want := map[string]any{
		"repo_id": repoID, "sha": sha, "pr": 7, "tag": "v0.1.0", "merged_by": "kiln-test[bot]", "sender_is_bot": true,
		"source": mqevents.AutoTagFromQueue, "work_item_id": 12, "phase": "implement", "impl_round": 2, "issues": []int{3, 4},
		"creator": "kiln-test[bot]",
	}
	h.wantAudit("autotag.tagged", want)
	h.wantAudit("autotag.planned", want)
}

// TestRecoveryAuditReleaseBasis（PLAN #29.7）：tagging / tagged 崩溃恢复后内存中的来源详情已经丢失，写审计之前
// 按已落库的可信工作项只读补齐；工作项后来终止（来源判定不再放行）也照常认下已建好的 tag、规划发布（T2，不重新否决）。
func TestRecoveryAuditReleaseBasis(t *testing.T) {
	t.Parallel()
	for _, state := range []string{StateTagging, StateTagged} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tagConfig)
			h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
				// 工作项后来终止了：现在判定为拒绝，但记录已经带着可信的工作项
				return DevOrigin{
					Kind: OriginDenied, WorkItemID: 12, Phase: "implement", ImplRound: 2, BaseRef: "main",
					CreatorLogin: "kiln-test[bot]", Issues: []int{3, 4}, Reason: "AI 开发工作项 #12 已终止，不自动发布",
				}, nil
			})
			sha := h.commit(nil, "PR")
			h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, head_ref, base_ref, work_item_id, source, state, tag)
				VALUES ($1,$2,7,'kiln-test[bot]',true,'kiln/dev-1','main',12,'queue',$3,'v0.1.0')`, repoID, sha, state)
			h.mustProcess()
			h.wantState(sha, StatePlanned, "v0.1.0")
			if got := h.planner.requests(); len(got) != 1 || got[0].Tag != "v0.1.0" || !got[0].SenderIsBot {
				t.Fatalf("应照常规划发布：%+v", got)
			}
			want := map[string]any{
				"pr": 7, "tag": "v0.1.0", "merged_by": "kiln-test[bot]", "sender_is_bot": true, "work_item_id": 12,
				"phase": "implement", "impl_round": 2, "issues": []int{3, 4}, "creator": "kiln-test[bot]",
			}
			if state == StateTagging {
				h.wantAudit("autotag.tagged", want)
			}
			h.wantAudit("autotag.planned", want)
		})
	}
}

// TestFailedAuditEnrichedAfterRestart（PLAN #29.7）：恢复的可信 Bot tagging 记录从库读出后（模拟重启，内存中
// 没有来源详情），创建 tag 被平台明确拒绝、转到 failed 时，写 autotag.failed 审计之前按已落库的可信工作项只读补齐来源
// 详情——合并者、发送者类型、工作项、阶段、轮次、Issue、创建者都要在审计里。
func TestFailedAuditEnrichedAfterRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, head_ref, base_ref, work_item_id, source, state, tag)
		VALUES ($1,$2,7,'kiln-test[bot]',true,'kiln/dev-1','main',12,'queue','tagging','v0.1.0')`, repoID, sha)

	// 模拟重启：新建 Service，内存里没有来源详情，只有库里记下的可信工作项
	h.svc = New(Deps{
		DB: h.db, Forge: h.svc.forge, Planner: h.planner, Outbox: h.svc.outbox, Hub: h.svc.hub, Log: h.svc.log,
		DevOrigin: originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
			return DevOrigin{
				Kind: OriginImplementation, WorkItemID: 12, Phase: "implement", ImplRound: 2, BaseRef: "main",
				CreatorLogin: "kiln-test[bot]", Issues: []int{3, 4},
			}, nil
		}),
	})
	h.fake.SetFault(refsRoute, fakegithub.Fault{Status: http.StatusForbidden})
	h.mustProcess()
	if r := h.wantState(sha, StateFailed, "v0.1.0"); !strings.Contains(r.Reason, "Contents") {
		t.Fatalf("原因应提示权限：%s", r.Reason)
	}
	h.wantAudit("autotag.failed", map[string]any{
		"merged_by": "kiln-test[bot]", "sender_is_bot": true, "work_item_id": 12, "phase": "implement",
		"impl_round": 2, "issues": []int{3, 4}, "creator": "kiln-test[bot]",
	})
}

// TestProcessClearsStaleWorkItemForUntrustedOrigin：处理时判定来源不可信（这里是在核验中），记录上旧的可信
// 工作项要清回 NULL——不经过 Retry 也要清，不能把不可信的来源留成可信归属。
func TestProcessClearsStaleWorkItemForUntrustedOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tagConfig)
	sha := h.commit(nil, "PR")
	h.exec(`INSERT INTO auto_tags (repo_id, sha, pr_number, merged_by, sender_is_bot, head_ref, base_ref, work_item_id, source, state)
		VALUES ($1,$2,24,'kiln-test[bot]',true,'kiln/dev-1','main',12,'webhook','pending')`, repoID, sha)
	h.svc.devOrigin = originFunc(func(context.Context, store.Querier, int64, int, string, string) (DevOrigin, error) {
		return DevOrigin{Kind: OriginRecording, WorkItemID: 12, Reason: "AI 开发工作项 #12 的 PR 创建归属正在核验"}, nil
	})
	h.mustProcess()
	h.wantState(sha, StateSkipped, "")
	if r := h.get(sha); r.WorkItemID != nil {
		t.Fatalf("归属未确认时不应留着可信列：%d", *r.WorkItemID)
	}
	h.wantAudit("autotag.skipped", map[string]any{"work_item_id": 12})
}

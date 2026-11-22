package mqevents

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自动打 tag 的记录（PLAN #29.4，migration 0022_auto_tags.sql）：合并队列（与合并结果同一事务）和事件接入都要写。

// AutoTagSource 是 auto_tags.source 的取值。
const (
	AutoTagFromQueue   = "queue"
	AutoTagFromWebhook = "webhook"
)

// SkipCodeBranchNotRelease 是 auto_tags.skip_code 的取值（PLAN #29.4）：合并进的不是发版分支，不自动打 tag。这样的记录
// 不在页面上刷屏；同一个 commit 之后从发版分支合入时（fast-forward）会重置为 pending 重新裁决。
const SkipCodeBranchNotRelease = "branch_not_release"

// AutoTagRecord 是一次经过 PR 的合并。
type AutoTagRecord struct {
	RepoID int64
	SHA    string // 合并进的 commit
	PR     int
	Labels []string // 合并那一刻 PR 的 label
	// MergedBy 是规划发布时的发送者（GitHub login）；SenderIsBot 表示真实发起者是机器人（或者未知）。
	MergedBy    string
	SenderIsBot bool
	HeadRef     string // PR 的来源分支（未知时为空）
	BaseRef     string // PR 的目标分支（未知时为空）
	// DefaultBranchAtMerge 是合并那一刻仓库的默认分支（省略 tag.branch 时它是发版分支）：默认分支改过名后，迟到的 webhook
	// 仍按合并当时的默认分支裁决。未知时为空（按仓库现在的默认分支）。
	DefaultBranchAtMerge string
	// WorkItemID 是已知的 AI 开发工作项归属（判定由自动打 tag 的执行者完成，这里通常为空）。
	WorkItemID *int64
	Source     string // AutoTagFromQueue | AutoTagFromWebhook
}

// RecordAutoTag 记下一次经过 PR 的合并，返回是否新写入（或者重新激活）了记录。调用方随后写 outbox（EnqueueAutoTag）
// 通知执行者。q 必须是事务（候选和重新激活要与执行者裁决串行化）。
//
// 同一个 commit 已经有记录时通常什么也不做（一次合并只有一个事实）。例外有两种：
//
//  1. 旧记录是因为合入的不是发版分支而跳过的（skip_code=branch_not_release），而这次它从另一个分支合入——例如 PR#10
//     先被合入 dev（fast-forward 后 dev 与 master 指向同一个 commit），随后 dev -> master 的合并产生同一个
//     merge_commit_sha。这时按新的分支重新裁决，否则发版分支上的合并永远拿不到 tag。已进入 tagging/tagged/planned
//     （可能已经建出 tag）、failed 或其他业务跳过（release:skip、设计 PR、拒绝）的记录绝不覆盖。
//  2. 旧记录还是 pending（执行者还没处理），这次从另一个分支合入：不能直接覆盖（否则乱序晚到的 dev webhook 会把还没处理
//     的 master 合并反过来盖掉），先作为待裁决候选记进 auto_tag_candidates。执行者离开 pending 之前按候选里能发版的那
//     一条重新裁决（autotag.decide），并清空这个 commit 的候选。已经 tagging/tagged/planned/failed、其他业务跳过或者
//     同一分支的记录照旧忽略。
//
// 两条 SQL 之间必须串行化：执行者裁决候选时先对 auto_tags 的父行 FOR UPDATE（autotag.resolveCandidates），这里也在
// 同一个事务里锁住父行后再读写候选。否则 READ COMMITTED 下，候选可能在执行者"查空之后、清候选之前"提交而被误删，
// 发版分支上的合成就永远拿不到 tag。
func RecordAutoTag(ctx context.Context, q recordQuerier, r AutoTagRecord) (bool, error) {
	labels := slices.Clone(r.Labels)
	if labels == nil {
		labels = []string{}
	}
	// 旧记录因为不是发版分支跳过、这次从另一个分支合入：重新激活（ON CONFLICT DO UPDATE 会锁住父行）
	tag, err := q.Exec(ctx, `INSERT INTO auto_tags (repo_id, sha, pr_number, labels, merged_by, sender_is_bot, head_ref, base_ref,
		default_branch_at_merge, work_item_id, source) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (repo_id, sha) DO UPDATE SET
			pr_number=EXCLUDED.pr_number, labels=EXCLUDED.labels, merged_by=EXCLUDED.merged_by,
			sender_is_bot=EXCLUDED.sender_is_bot, head_ref=EXCLUDED.head_ref, base_ref=EXCLUDED.base_ref,
			default_branch_at_merge=EXCLUDED.default_branch_at_merge, work_item_id=NULL, source=EXCLUDED.source,
			state='pending', tag='', reason='', skip_code='', tries=0, updated_at=now()
		WHERE auto_tags.state='skipped' AND auto_tags.skip_code=$12 AND auto_tags.base_ref <> EXCLUDED.base_ref`,
		r.RepoID, r.SHA, r.PR, labels, r.MergedBy, r.SenderIsBot, r.HeadRef, r.BaseRef, r.DefaultBranchAtMerge, r.WorkItemID, r.Source,
		SkipCodeBranchNotRelease)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	// 锁住父行之后再做判断：执行者（autotag.decide）也在同一行上 FOR UPDATE，两者串行化，候选不会被误删、也不会漏掉
	// 发版分支的合并事实。
	var id int64
	var state, skipCode, baseRef string
	if err := q.QueryRow(ctx, `SELECT id, state, skip_code, base_ref FROM auto_tags
		WHERE repo_id=$1 AND sha=$2 FOR UPDATE`, r.RepoID, r.SHA).Scan(&id, &state, &skipCode, &baseRef); err != nil {
		return false, err
	}
	switch {
	case state == "skipped" && skipCode == SkipCodeBranchNotRelease && baseRef != r.BaseRef:
		// 上面的 ON CONFLICT DO UPDATE 因为并发（执行者在同一个窗口里把它置为 branch_not_release 跳过）没有命中：在锁下
		// 重新激活，绝不丢掉发版分支上的合并。
		if _, err := q.Exec(ctx, `UPDATE auto_tags SET pr_number=$2, labels=$3, merged_by=$4, sender_is_bot=$5, head_ref=$6,
			base_ref=$7, default_branch_at_merge=$8, work_item_id=NULL, source=$9, state='pending', tag='', reason='',
			skip_code='', tries=0, updated_at=now() WHERE id=$1`,
			id, r.PR, labels, r.MergedBy, r.SenderIsBot, r.HeadRef, r.BaseRef, r.DefaultBranchAtMerge, r.Source); err != nil {
			return false, err
		}
		return true, nil
	case state == "pending" && baseRef != r.BaseRef:
		// 记录还在 pending 而这次来自另一个分支：先留作候选，由执行者裁决
		cand, err := q.Exec(ctx, `INSERT INTO auto_tag_candidates (repo_id, sha, pr_number, labels, merged_by, sender_is_bot,
			head_ref, base_ref, default_branch_at_merge, source) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (repo_id, sha, base_ref) DO NOTHING`,
			r.RepoID, r.SHA, r.PR, labels, r.MergedBy, r.SenderIsBot, r.HeadRef, r.BaseRef, r.DefaultBranchAtMerge, r.Source)
		if err != nil {
			return false, err
		}
		return cand.RowsAffected() > 0, nil
	default:
		// 已经 tagging/tagged/planned/failed、其他业务跳过，或者同一个分支：什么也不做
		return false, nil
	}
}

// recordQuerier 是 RecordAutoTag 需要的事务能力：写语句，以及锁住父行后读取它。
type recordQuerier interface {
	store.Execer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

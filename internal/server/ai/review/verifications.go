package review

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// verificationName 是 AI 审计作为验证时的显示名称；verificationSource 是它的来源（与 .kiln/merge.yml 中 required 的
// ai-review 对应，迁移前审计 run 的 workflow 也是它）。
const (
	verificationName   = "AI 审计"
	verificationSource = workflowName
)

// PRVerifications 返回 PR pr 的 head 针对 baseRef 的 AI 审计（每个请求族最新的一次请求，PLAN #30.3 的 Verifications）；
// withJobs 时带上各平台的任务（名称为 review）。实现 store.VerificationSource。
func PRVerifications(ctx context.Context, q store.Querier, repoID int64, pr int, head, baseRef string, withJobs bool) ([]store.Verification, error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.state,
			COALESCE(array_agg(t.state ORDER BY t.id) FILTER (WHERE t.id IS NOT NULL), '{}')
		FROM (SELECT DISTINCT ON (q.family_id) q.id, q.state FROM ai_requests q JOIN ai_review_requests d ON d.request_id=q.id
		      WHERE q.type=$1 AND q.repo_id=$2 AND q.event=$3 AND d.pr_number=$4 AND q.sha=$5 AND d.base_ref=$6
		      ORDER BY q.family_id, q.id DESC) r
		LEFT JOIN ai_tasks t ON $7 AND t.request_id=r.id
		GROUP BY r.id, r.state ORDER BY r.id`,
		agentcfg.TaskReview, repoID, eventPullRequest, pr, head, baseRef, withJobs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Verification, error) {
		v := store.Verification{Name: verificationName, Source: verificationSource, Builtin: true}
		var states []string
		if err := row.Scan(&v.ID, &v.State, &states); err != nil {
			return v, err
		}
		for _, s := range states {
			v.Jobs = append(v.Jobs, store.VerificationJob{Name: agentcfg.TaskReview, State: s})
		}
		return v, nil
	})
}

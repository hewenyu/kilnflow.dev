package review

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ai_reviews.review_state 的取值（发布到 GitHub 的 PR review）。发布没有幂等键，按"先写意图、最多尝试一次"处理
// （PLAN C19、#26.12）：调用前记为 publishing；结果不明且之后找不到时记为 unknown，不自动重发，交给人（手动重试）。
const (
	StateNone       = "none"
	StatePending    = "pending"
	StatePublishing = "publishing"
	StatePublished  = "published"
	StateFailed     = "failed"
	StateUnknown    = "unknown"
)

// Report 是一个 AI 审计任务的结果（ai_reviews）。
type Report struct {
	TaskID         int64
	PRNumber       *int
	HeadSHA        string
	BaseSHA        string
	Agent          string
	Verdict        string
	Summary        string
	Findings       int
	Report         []byte
	ReviewState    string
	GitHubReviewID *int64
	ReviewError    *string
	CreatedAt      time.Time
	PublishedAt    *time.Time
}

// GetReport 返回审计任务的结果；没有时返回 nil, nil。
func GetReport(ctx context.Context, q store.Querier, taskID int64) (*Report, error) {
	var r Report
	err := q.QueryRow(ctx, `SELECT task_id, pr_number, head_sha, base_sha, agent, verdict, summary, findings, report, review_state,
		github_review_id, review_error, created_at, published_at FROM ai_reviews WHERE task_id=$1`, taskID).
		Scan(&r.TaskID, &r.PRNumber, &r.HeadSHA, &r.BaseSHA, &r.Agent, &r.Verdict, &r.Summary, &r.Findings, &r.Report, &r.ReviewState,
			&r.GitHubReviewID, &r.ReviewError, &r.CreatedAt, &r.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

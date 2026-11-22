package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/aireport"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 审计结果的 PR review：正文按编号完整列出全部问题，落在 PR diff 中的问题另作为编号相同的行内评论；
// 正文的格式见 aireview_body.go。review 的事件总是 COMMENT（门禁由 Check Run 表达）。
//
// 至多一次：正文末尾带任务的标记，创建前先在 PR 已有的 review 中查找，调用前记下"已尝试"（见 publish）。

const (
	// maxInlineComments 是一个 review 中行内评论的数量上限，其余写进正文。
	maxInlineComments = 50
	// maxReviewBody 是 review 正文的长度上限（GitHub 限制为 65536 个字符）。
	maxReviewBody = 60000
	// maxCommentBody 是单条行内评论的长度上限。
	maxCommentBody = 8000
)

// aiReviewMarker 返回写在 review 正文中的标记，用于查找已经发布的 review（PLAN #30.5）。
func aiReviewMarker(taskID int64) string {
	return fmt.Sprintf("<!-- kiln-ai-review ai-task:%d -->", taskID)
}

// KindPRReview 是把审计报告发布为 PR review 的 outbox 记录类型（payload 为 prReviewPayload）。
const KindPRReview = "ai.pr_review"

// prReviewPayload 是 KindPRReview 的 payload。
type prReviewPayload struct {
	TaskID int64 `json:"task_id"`
}

// prReviewKey 返回任务的 PR review 记录的去重键（带 AI 前缀，PLAN #30.4）。
func prReviewKey(taskID int64) string { return fmt.Sprintf("ai-review:task:%d", taskID) }

// Publisher 把审计报告发布为 PR review（outbox 的 KindPRReview 记录由它投递）。
type Publisher struct {
	db        *store.DB
	gh        *gh.Client
	publicURL string
	// bot 是本 App 的 bot 账号（<slug>[bot]），第一次需要时读取（botLogin）。
	bot atomic.Pointer[string]
}

// NewPublisher 返回 Publisher。
func NewPublisher(db *store.DB, client *gh.Client, publicURL string) *Publisher {
	return &Publisher{db: db, gh: client, publicURL: publicURL}
}

// Deliver 投递一条 KindPRReview 记录（outbox.Handler）。
func (w *Publisher) Deliver(ctx context.Context, payload []byte) error {
	if w.gh == nil {
		return nil
	}
	var p prReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("outbox 记录的 payload 无法解析：%w", err)
	}
	return w.publish(ctx, p.TaskID)
}

// OnDead 在 KindPRReview 记录被放弃时把 PR review 标记为发布失败（outbox.DeadHandler）：还没有尝试过创建的记为失败；
// 已经尝试过、结果不明的记为 unknown（可能已经发布，不能说失败）。Dashboard 不会一直显示"等待发布"。
func OnDead(ctx context.Context, tx pgx.Tx, payload []byte, msg string) error {
	var p prReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil //nolint:nilerr // payload 无法解析时没有可标记的 review
	}
	_, err := tx.Exec(ctx, `UPDATE ai_reviews SET review_state=CASE WHEN review_state=$3 THEN $2 ELSE $4 END, review_error=$5
		WHERE task_id=$1 AND review_state IN ($3, $6)`,
		p.TaskID, StateFailed, StatePending, StateUnknown, "多次重试后放弃："+msg, StatePublishing)
	return err
}

// OnRetry 在手动重试 KindPRReview 记录时把发布失败或结果不明的 PR review 恢复为 pending（outbox.RetryHook）：
// 手动重试是人的决定（人确认 PR 上没有这个 review）。
func OnRetry(ctx context.Context, tx pgx.Tx, payload []byte) error {
	var p prReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil //nolint:nilerr // payload 无法解析时没有可恢复的 review
	}
	_, err := tx.Exec(ctx, `UPDATE ai_reviews SET review_state=$2, review_error=NULL WHERE task_id=$1 AND review_state IN ($3, $4)`,
		p.TaskID, StatePending, StateFailed, StateUnknown)
	return err
}

// aiReviewRow 是待发布的一行 ai_reviews。
type aiReviewRow struct {
	RepoID  int64
	PR      *int
	HeadSHA string
	BaseSHA string
	Agent   string
	// Platform、Runner 是执行这次审计的平台和节点（一组平台中由哪台领到）。
	Platform string
	Runner   string
	State    string
	Report   []byte
	ReviewID *int64
}

// publish 把任务的 AI 审计报告发布为 PR review。已经发布过的直接返回。
//
// 发布没有幂等键，按"先写意图、最多尝试一次"处理（PLAN C19、#26.12）：
//   - pending：先在 PR 已有的 review 中找本 App 发布的同一标记（找到即 published）；没有就把状态改为 publishing
//     （调用前记下"已尝试"），再创建。
//   - 创建肯定没有生效（GitHub 明确拒绝、限流）：退回 pending，由 outbox 稍后重试。行内评论的位置不被接受（422）时
//     不带行内评论再发一次（第一次肯定没有创建）。
//   - 结果不明（网络错误、超时、5xx）：保持 publishing，返回错误；下一次投递只查找、不再创建：找到即 published，
//     找不到记为 unknown（可能读到了滞后的列表），不自动重发，Dashboard 显示，手动重试后恢复为 pending。
func (w *Publisher) publish(ctx context.Context, taskID int64) error {
	var row aiReviewRow
	err := w.db.QueryRow(ctx, `SELECT v.repo_id, v.pr_number, v.head_sha, v.base_sha, v.agent, coalesce(a.platform, ''),
		coalesce(a.runner_name, ''), v.review_state, v.report, v.github_review_id
		FROM ai_reviews v LEFT JOIN ai_attempts a ON a.id=v.attempt_id WHERE v.task_id=$1`, taskID).
		Scan(&row.RepoID, &row.PR, &row.HeadSHA, &row.BaseSHA, &row.Agent, &row.Platform, &row.Runner, &row.State, &row.Report, &row.ReviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if (row.State != StatePending && row.State != StatePublishing) || row.PR == nil {
		return nil
	}
	rep, err := aireport.Parse(row.Report)
	if err != nil {
		return w.markAIReview(ctx, taskID, row.State, StateFailed, 0, "保存的报告无效："+err.Error())
	}
	repo, err := store.GetRepo(ctx, w.db, row.RepoID)
	if err != nil {
		return err
	}
	inst := w.gh.Inst(repo.InstallationID)
	marker := aiReviewMarker(taskID)
	found, err := w.findAIReview(ctx, inst, repo.FullName(), *row.PR, marker)
	if err != nil {
		return err
	}
	if found != 0 {
		return w.markAIReview(ctx, taskID, row.State, StatePublished, found, "")
	}
	if row.State == StatePublishing {
		// 上一次创建的结果不明，现在又找不到：不再创建（可能读到了滞后的列表，重发会重复），记录直接放弃、交给人
		const msg = "上一次发布的结果不明（响应丢失或记录结果前重启），之后在 PR 中没有找到这个 review；没有自动重发，确认 PR 上没有后可手动重试"
		if err := w.markAIReview(ctx, taskID, row.State, StateUnknown, 0, msg); err != nil {
			return err
		}
		return fmt.Errorf("%s：%w", msg, outbox.ErrGiveUp)
	}
	files, err := inst.ListPullFilePatches(ctx, repo.FullName(), *row.PR)
	if err != nil {
		return fmt.Errorf("读取 PR #%d 的变更：%w", *row.PR, err)
	}
	if ok, err := w.casAIReview(ctx, taskID, StatePending, StatePublishing); err != nil || !ok {
		return err
	}
	review := buildAIReview(rep, &row, repo.FullName(), commentableLines(files), w.taskURL(taskID), marker)
	created, err := inst.CreatePullReview(ctx, repo.FullName(), *row.PR, review)
	if gh.IsUnprocessable(err) {
		// 行内评论的位置不被接受（例如审计期间 PR 推送了新的 commit）：不带行内评论、不指定 commit 再发一次（正文本来就有全部问题）
		review = buildAIReview(rep, &row, repo.FullName(), nil, w.taskURL(taskID), marker)
		review.CommitID = ""
		created, err = inst.CreatePullReview(ctx, repo.FullName(), *row.PR, review)
	}
	if err != nil {
		if gh.NotApplied(err) {
			// 肯定没有创建：退回 pending，稍后照常重试
			if _, dbErr := w.casAIReview(ctx, taskID, StatePublishing, StatePending); dbErr != nil {
				return errors.Join(err, dbErr)
			}
		}
		return fmt.Errorf("发布 PR #%d 的 review：%w", *row.PR, err)
	}
	return w.markAIReview(ctx, taskID, StatePublishing, StatePublished, created.ID, "")
}

// findAIReview 在 PR 已有的 review 中找本 App 发布的、带 marker 的 review，返回它的 ID（没有时为 0）。
func (w *Publisher) findAIReview(ctx context.Context, inst *gh.Inst, repo string, pr int, marker string) (int64, error) {
	existing, err := inst.ListPullReviews(ctx, repo, pr)
	if err != nil {
		return 0, fmt.Errorf("读取 PR #%d 的 review：%w", pr, err)
	}
	bot, err := w.botLogin(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range existing {
		// 只认本 App 发布的 review：其他用户或 bot 也能在正文中写同样的标记，
		// 认了就会压制真正的报告、把发布状态记成已发布
		if isBotReview(&r, bot) && strings.Contains(r.Body, marker) {
			return r.ID, nil
		}
	}
	return 0, nil
}

// casAIReview 在 review_state 仍为 from 时改为 to，返回是否改了（期间被手动重试、新的报告改写时不改）。
func (w *Publisher) casAIReview(ctx context.Context, taskID int64, from, to string) (bool, error) {
	tag, err := w.db.Exec(ctx, `UPDATE ai_reviews SET review_state=$3, review_error=NULL WHERE task_id=$1 AND review_state=$2`, taskID, from, to)
	return tag.RowsAffected() > 0, err
}

// isBotReview 判断 review 是否由 bot 账号 bot（本 App 的 <slug>[bot]）发布。
func isBotReview(r *gh.PullReview, bot string) bool {
	return r.User != nil && r.User.Type == "Bot" && strings.EqualFold(r.User.Login, bot)
}

// botLogin 返回本 App 的 bot 账号（<slug>[bot]）。第一次调用时读取 App 信息并缓存；读取失败时返回错误，由 outbox 稍后重试。
func (w *Publisher) botLogin(ctx context.Context) (string, error) {
	if b := w.bot.Load(); b != nil {
		return *b, nil
	}
	app, err := w.gh.GetApp(ctx)
	if err != nil {
		return "", fmt.Errorf("读取 App 信息（用于识别本 App 发布的 review）：%w", err)
	}
	if app.Slug == "" {
		return "", errors.New("读取的 App 信息中没有 slug，无法识别本 App 发布的 review")
	}
	b := app.Slug + "[bot]"
	w.bot.Store(&b)
	return b, nil
}

// markAIReview 记录 PR review 的发布结果（review_state 仍为 from 时）。
func (w *Publisher) markAIReview(ctx context.Context, taskID int64, from, state string, reviewID int64, errMsg string) error {
	var id *int64
	if reviewID != 0 {
		id = &reviewID
	}
	_, err := w.db.Exec(ctx, `UPDATE ai_reviews SET review_state=$2, github_review_id=coalesce($3, github_review_id), review_error=nullif($4,''),
		published_at=CASE WHEN $2='published' THEN now() ELSE published_at END WHERE task_id=$1 AND review_state=$5`, taskID, state, id, errMsg, from)
	return err
}

// commentableLines 返回 PR 每个文件中可以写行内评论的行。
func commentableLines(files []gh.PullFile) map[string]map[int]bool {
	out := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		if f.Patch != "" {
			out[f.Filename] = gh.CommentableLines(f.Patch)
		}
	}
	return out
}

// taskURL 返回 Dashboard 中审计任务的地址。
func (w *Publisher) taskURL(taskID int64) string {
	return fmt.Sprintf("%s/ai/tasks/%d", w.publicURL, taskID)
}

// Package checkrun 是 CI 与 AI 共用的 Check Run 同步机制（PLAN #13.3、#30.3）：创建或更新 Check Run、按 external_id 查找
// 崩溃前已经创建的 Check Run。内容（名称、摘要、external_id）由各领域生成，这个包不访问数据库。
package checkrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// Check Run 摘要的长度限制。
const (
	// MaxSummaryBytes 是摘要的上限（GitHub 限制 output.summary 最多 65535 个字符）。
	MaxSummaryBytes = 65000
	// FailedLogTailLines 是失败的摘要中附上的日志行数。
	FailedLogTailLines = 50
	// maxLogTailBytes 是摘要中日志部分的上限。
	maxLogTailBytes = 60000
)

// SetCompleted 把 Check Run 置为 completed；没有记录结束时间时用当前时间。
func SetCompleted(cr *gh.CheckRun, conclusion string, finishedAt *time.Time) {
	cr.Status = gh.CheckStatusCompleted
	cr.Conclusion = conclusion
	cr.CompletedAt = finishedAt
	if cr.CompletedAt == nil {
		now := time.Now()
		cr.CompletedAt = &now
	}
}

// LogTail 返回摘要中的日志末尾部分（tail 为空时返回空）。
func LogTail(tail string) string {
	if tail == "" {
		return ""
	}
	tail = shorten.Text(tail, maxLogTailBytes)
	return fmt.Sprintf("\n最后 %d 行日志：\n\n```\n%s\n```\n", FailedLogTailLines, strings.ReplaceAll(tail, "```", "`‵`"))
}

// Target 是要同步的 Check Run 所在的仓库和 commit。
type Target struct {
	Inst *gh.Inst
	// Repo 是 owner/name。
	Repo string
	SHA  string
	// AppID 是本 App 的 ID（按 external_id 查找本 App 创建的 Check Run）。
	AppID int64
}

// Upsert 创建或更新 Check Run。existing 是已经记下的 Check Run ID；创建前先按 external_id 查找，避免崩溃后重复创建
// （PLAN #7.8）。新的 ID 由 save 记下。
func Upsert(ctx context.Context, t Target, cr *gh.CheckRun, existing *int64, save func(int64) error) error {
	if existing != nil {
		err := t.Inst.UpdateCheckRun(ctx, t.Repo, *existing, cr)
		if err == nil || !gh.IsNotFound(err) {
			return err
		}
	}
	found, err := t.Inst.FindCheckRun(ctx, t.Repo, t.SHA, cr.Name, cr.ExternalID, t.AppID)
	if err != nil {
		return err
	}
	if found != nil {
		if err := save(found.ID); err != nil {
			return err
		}
		return t.Inst.UpdateCheckRun(ctx, t.Repo, found.ID, cr)
	}
	if cr.Status == gh.CheckStatusCompleted && cr.StartedAt == nil {
		// 结束后才第一次创建：GitHub 默认把 started_at 设为创建时间，会晚于 completed_at
		cr.StartedAt = cr.CompletedAt
	}
	created, err := t.Inst.CreateCheckRun(ctx, t.Repo, cr)
	if err != nil {
		return err
	}
	if created.ID == 0 {
		return errors.New("创建 Check Run 没有返回 ID")
	}
	return save(created.ID)
}

package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// PR review（AI 审计的结果）：需要 App 的 Pull requests 写权限。

// ReviewEventComment 是只评论、不批准也不要求修改的 review（门禁由 Check Run 表达）。
const ReviewEventComment = "COMMENT"

// ReviewComment 是 PR review 中的一条行内评论：Line 是 Side 一侧文件中的行号，必须落在 PR 的 diff 中。
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side,omitempty"`
	Body string `json:"body"`
}

// PullReview 是 PR review 的创建参数和返回值。
type PullReview struct {
	ID       int64           `json:"id,omitempty"`
	Body     string          `json:"body,omitempty"`
	CommitID string          `json:"commit_id,omitempty"`
	Event    string          `json:"event,omitempty"`
	Comments []ReviewComment `json:"comments,omitempty"`
	User     *Account        `json:"user,omitempty"`
}

// reviewPages 是列出 PR review 时最多读取的页数。
const reviewPages = 10

// CreatePullReview 在 PR 上创建一个 review（带行内评论）。
func (i *Inst) CreatePullReview(ctx context.Context, repo string, number int, r *PullReview) (*PullReview, error) {
	return call[PullReview](ctx, i.Do, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), r)
}

// ListPullReviews 返回 PR 的 review（最多 reviewPages 页）。
func (i *Inst) ListPullReviews(ctx context.Context, repo string, number int) ([]PullReview, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), reviewPages, unwrapList[PullReview])
}

// PullFile 是 PR 中一个变更文件及其 patch（大文件或二进制文件没有 patch）。
type PullFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Patch    string `json:"patch,omitempty"`
}

// ListPullFilePatches 返回 PR 变更的文件和 patch（最多 3000 个文件）。
func (i *Inst) ListPullFilePatches(ctx context.Context, repo string, number int) ([]PullFile, error) {
	return paginate(ctx, i.get, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, number), pullFilePages, unwrapList[PullFile])
}

// hunkHeader 匹配 unified diff 的 hunk 头，取出新文件一侧的起始行和行数。
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// CommentableLines 返回 patch 中新文件一侧（RIGHT）可以写行内评论的行：新增行和上下文行。
func CommentableLines(patch string) map[int]bool {
	lines := map[int]bool{}
	cur := 0
	for l := range strings.SplitSeq(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			cur, _ = strconv.Atoi(m[1])
			continue
		}
		if cur == 0 {
			continue
		}
		// 删除行（-）和 "\ No newline at end of file" 不占新文件的行
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, " ") {
			lines[cur] = true
			cur++
		}
	}
	return lines
}

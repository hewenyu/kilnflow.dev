package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 合并队列（PLAN #28）：/merge-queue 按仓库列出排队中的 PR 和最近结束的项，管理员可以入队、移除；
// run 详情页顶部显示 PR 在队列中的状态（runMergeQueue）。入队和移除等同于给 PR 加上、去掉 label kiln:merge，
// 由 admin.Service 调平台接口执行，队列的处理由 internal/server/mergequeue 完成。

// mergeQueuePath 是合并队列页的地址。
const mergeQueuePath = "/merge-queue"

// mergeQueueURL 返回合并队列页的地址；repo 为空时是全部仓库。
func mergeQueueURL(repo string) string {
	if repo == "" {
		return mergeQueuePath
	}
	return mergeQueuePath + "?" + url.Values{"repo": {repo}}.Encode()
}

// mergeQueueEntry 是队列中的一项（mergequeue.Entry 中页面用到的字段）。
type mergeQueueEntry struct {
	PR int
	// State 是 queued、merged 或 removed。
	State   string
	HeadSHA string
	// BaseRef 是 PR 最近观测到的目标分支（PLAN #28.5）：合并队列按目标分支分别排队。
	BaseRef string
	// EnqueuedBy 是入队的操作者（github:<login>，或 Dashboard、CLI 的操作者）。
	EnqueuedBy string
	// Status 是排队中时当前在等什么；Reason 是出队原因（合并时可能说明是在队列之外合并的）。
	Status, Reason string
	// Removal 是自动出队的类别（mergequeue.RemovalConflictDev、RemovalConflictAuthor；其他为空）。
	Removal    string
	MergeSHA   string
	EnqueuedAt time.Time
	FinishedAt *time.Time
}

func newMergeQueueEntry(e *mergequeue.Entry) mergeQueueEntry {
	return mergeQueueEntry{
		PR: e.PR, State: e.State, HeadSHA: e.HeadSHA, BaseRef: e.BaseRef, EnqueuedBy: e.EnqueuedBy, Status: e.Status, Reason: e.Reason,
		Removal: e.Removal, MergeSHA: e.MergeSHA, EnqueuedAt: e.EnqueuedAt, FinishedAt: e.FinishedAt,
	}
}

func mergeQueueEntries(list []*mergequeue.Entry) []mergeQueueEntry {
	out := make([]mergeQueueEntry, 0, len(list))
	for _, e := range list {
		out = append(out, newMergeQueueEntry(e))
	}
	return out
}

// mergeQueueView 是一个仓库的合并队列。
type mergeQueueView struct {
	Repo string
	// URL 是只看这个仓库的地址。
	URL string
	// Queued 是排队中的项，第一个是队首；Recent 是最近结束的项，新的在前。
	Queued, Recent []mergeQueueEntry
	// AutoTags 是最近的自动打 tag 记录（PLAN #29），新的在前。有合并进默认分支的 PR 时就有记录，不受 .kiln/merge.yml
	// 是否有 tag: 影响（没有配置的记录为 skipped）。
	AutoTags []autoTagView
}

// mergeQueueAutoTags 是合并队列页每个仓库列出的自动打 tag 记录数。
const mergeQueueAutoTags = 10

// autoTagView 是一条自动打 tag 记录：PR 合入默认分支的 commit 按 .kiln/merge.yml 的 tag 规则打 tag 并规划发布。
type autoTagView struct {
	SHA string
	PR  int
	// BaseRef 是 PR 的目标分支（PLAN #29.4）：只有合入发版分支的才会打 tag。
	BaseRef string
	// Labels 是 PR 合入时的 label（决定版本号升哪一位）。
	Labels []string
	// MergedBy 是真实的发布发起者（人，或者合并 PR 的机器人）；SenderIsBot 表示它是机器人。
	MergedBy    string
	SenderIsBot bool
	// WorkItemID 是认定的 AI 开发工作项（0 表示没有可信归属）。
	WorkItemID int64
	// Source 是记录的来源：queue（合并队列合入）或 webhook（在队列之外合入）。
	Source string
	// State 是 pending、tagging、tagged、planned、skipped 或 failed。
	State string
	// Tag 是打的（或要打的）tag；Reason 是跳过或失败的原因。
	Tag, Reason string
	Tries       int
	Created     time.Time
	Updated     time.Time
	// Retryable 表示可以重试（failed 或 skipped）。
	Retryable bool
}

func newAutoTagView(r *autotag.Record) autoTagView {
	v := autoTagView{
		SHA: r.SHA, PR: r.PR, BaseRef: r.BaseRef, Labels: r.Labels, MergedBy: r.MergedBy, SenderIsBot: r.SenderIsBot, Source: r.Source,
		State: r.State,
		Tag:   r.Tag, Reason: r.Reason, Tries: r.Tries, Created: r.CreatedAt, Updated: r.UpdatedAt,
		Retryable: r.State == autotag.StateFailed || r.State == autotag.StateSkipped,
	}
	if r.WorkItemID != nil {
		v.WorkItemID = *r.WorkItemID
	}
	return v
}

// mergeQueuePage 是合并队列页的数据。
type mergeQueuePage struct {
	// Repo 是只看的仓库（owner/name）；为空表示全部仓库。
	Repo   string
	Queues []mergeQueueView
	// Repos 是"加入合并队列"表单的仓库选项（Kiln 中启用的仓库）；只有管理员有。
	Repos []string
	// Label 是表示入队的 PR label。
	Label string
	// GitHub 是 GitHub 网页的地址（为空时不显示指向 GitHub 的链接）。
	GitHub string
}

func (d *Dashboard) pageMergeQueue(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	p := &mergeQueuePage{Repo: strings.TrimSpace(r.URL.Query().Get("repo")), Label: mergequeue.Label, GitHub: d.githubWeb()}
	var queues []*admin.MergeQueue
	if p.Repo != "" {
		q, err := d.admin.ListMergeQueue(ctx, p.Repo)
		// 写的不是 owner/name 时 store.FindRepo 返回的不是 ErrRepoNotFound，同样不可能是任何仓库
		if errors.Is(err, store.ErrRepoNotFound) || (err != nil && !admin.ValidRepoFullName(p.Repo)) {
			d.notFound(w, r, fmt.Sprintf("Kiln 中没有仓库 %s：名称可能写错了，或者仓库还没有同步。", p.Repo))
			return
		}
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		p.Repo = q.Repo.FullName()
		queues = []*admin.MergeQueue{q}
	} else {
		var err error
		if queues, err = d.admin.ListMergeQueues(ctx); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	for _, q := range queues {
		name := q.Repo.FullName()
		v := mergeQueueView{Repo: name, URL: mergeQueueURL(name), Queued: mergeQueueEntries(q.Queued), Recent: mergeQueueEntries(q.Recent)}
		tags, err := d.admin.AutoTagsOf(ctx, q.Repo.ID, mergeQueueAutoTags)
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		for _, t := range tags {
			v.AutoTags = append(v.AutoTags, newAutoTagView(t))
		}
		p.Queues = append(p.Queues, v)
	}
	if se.Admin() {
		var err error
		if p.Repos, err = d.activeRepos(ctx); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	title := "合并队列"
	if p.Repo != "" {
		title += " · " + p.Repo
	}
	d.render(w, r, "merge_queue", title, p)
}

// mergeQueueForm 读取入队、移除表单的仓库和 PR 号；PR 号无效时返回错误说明。
func mergeQueueForm(r *http.Request) (repo string, pr int, msg string) {
	repo = strings.TrimSpace(r.PostFormValue("repo"))
	if repo == "" {
		return "", 0, "请选择仓库"
	}
	raw := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("pr")), "#")
	pr, err := strconv.Atoi(raw)
	if err != nil || pr <= 0 {
		return repo, 0, fmt.Sprintf("PR 号 %q 无效，应为正整数", raw)
	}
	return repo, pr, ""
}

// postMergeQueueAdd 把 PR 加入合并队列（给 PR 加上 label kiln:merge），回到这个仓库的合并队列页。
func (d *Dashboard) postMergeQueueAdd(w http.ResponseWriter, r *http.Request, se *session) {
	repo, pr, msg := mergeQueueForm(r)
	back := mergeQueueURL(repo)
	if msg != "" {
		d.flashBack(w, r, back, "加入合并队列失败："+msg)
		return
	}
	added, err := d.admin.AddToMergeQueue(r.Context(), repo, pr, se.Login)
	switch {
	case err != nil:
		d.flashBack(w, r, back, fmt.Sprintf("把 %s#%d 加入合并队列失败：%s", repo, pr, d.failure(r, err)))
	case !added:
		d.flashBack(w, r, back, fmt.Sprintf("%s#%d 已经在合并队列中", repo, pr))
	default:
		d.flashBack(w, r, back, fmt.Sprintf("已把 %s#%d 加入合并队列（加上了 label %s）", repo, pr, mergequeue.Label))
	}
}

// postMergeQueueRemove 把 PR 移出合并队列（去掉 PR 的 label kiln:merge），回到这个仓库的合并队列页。
// 出队由队列的执行者完成，回到页面时可能还显示在排队中，稍后刷新即可。
func (d *Dashboard) postMergeQueueRemove(w http.ResponseWriter, r *http.Request, se *session) {
	repo, pr, msg := mergeQueueForm(r)
	back := mergeQueueURL(repo)
	if msg != "" {
		d.flashBack(w, r, back, "移出合并队列失败："+msg)
		return
	}
	if err := d.admin.RemoveFromMergeQueue(r.Context(), repo, pr, se.Login); err != nil {
		d.flashBack(w, r, back, fmt.Sprintf("把 %s#%d 移出合并队列失败：%s", repo, pr, d.failure(r, err)))
		return
	}
	d.flashBack(w, r, back, fmt.Sprintf("已请求把 %s#%d 移出合并队列（去掉了 label %s）", repo, pr, mergequeue.Label))
}

// postAutoTagRetry 重新处理失败或跳过的自动打 tag 记录（从头计算版本号，已经建好的 tag 会被认出来），回到这个仓库的合并队列页。
func (d *Dashboard) postAutoTagRetry(w http.ResponseWriter, r *http.Request, se *session) {
	repo, sha := strings.TrimSpace(r.PostFormValue("repo")), strings.TrimSpace(r.PostFormValue("sha"))
	back := mergeQueueURL(repo)
	if repo == "" || sha == "" {
		d.flashBack(w, r, back, "没有重试：缺少仓库或 commit")
		return
	}
	err := d.admin.RetryAutoTag(r.Context(), repo, sha, se.Login)
	switch {
	case errors.Is(err, autotag.ErrNotRetryable):
		d.flashBack(w, r, back, fmt.Sprintf("没有重试 %s：%s（可能已经在处理或已经完成，请刷新）", shortSHA(sha), err.Error()))
	case err != nil:
		d.flashBack(w, r, back, fmt.Sprintf("重试 %s 的自动打 tag 失败：%s", shortSHA(sha), d.failure(r, err)))
	default:
		d.flashBack(w, r, back, fmt.Sprintf("已重新处理 %s：重新计算版本号后打 tag 并规划发布", shortSHA(sha)))
	}
}

// shortSHA 返回 commit 的前 7 位。
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// runMergeQueue 是 PR 的 run 详情页顶部显示的合并队列状态。
type runMergeQueue struct {
	// Repo、PR 是"加入合并队列"表单的字段。
	Repo string
	PR   int
	// Entry 是 PR 在队列中最近的一项（排队中的优先）；没有记录时为 nil。
	Entry *mergeQueueEntry
	// Pos 是排队中的位置（从 1 开始），不在排队时为 0。
	Pos int
	// URL 是这个仓库的合并队列页。
	URL string
}

// runMergeQueueState 返回 PR 的 run 的合并队列状态；不是 pull_request 事件或没有 PR 号时返回 nil。
func (d *Dashboard) runMergeQueueState(ctx context.Context, run *cistore.Run, repo string) (*runMergeQueue, error) {
	if run.Event != "pull_request" || run.PRNumber == nil {
		return nil, nil
	}
	pr := *run.PRNumber
	e, pos, err := d.admin.PullMergeQueue(ctx, run.RepoID, pr)
	if err != nil {
		return nil, fmt.Errorf("读取 PR #%d 的合并队列状态：%w", pr, err)
	}
	v := &runMergeQueue{Repo: repo, PR: pr, Pos: pos, URL: mergeQueueURL(repo)}
	if e != nil {
		entry := newMergeQueueEntry(e)
		v.Entry = &entry
	}
	return v, nil
}

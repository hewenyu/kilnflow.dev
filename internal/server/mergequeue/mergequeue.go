// Package mergequeue 实现合并队列（PLAN #28）：PR 加 label kiln:merge 即入队，按入队顺序一次处理一个；
// 队首的 head 验证通过（pull_request run 全部通过，包括 AI 审计）且不落后于默认分支时合并，落后时更新 PR 分支，
// 等新 head 验证通过后再合并。
//
// 每个仓库只有一个执行者（Process，持有仓库级的 advisory lock），每次按最新状态重新判断（decide）。webhook、run 结束、
// 默认分支 push、Dashboard 只发出通知（outbox 记录 mergequeue.repo）；入队只插入新行。写操作由 Server 调平台接口
// （internal/forge）执行。
package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/forge"
	"github.com/hewenyu/kilnflow.dev/internal/server/mqevents"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Label 是表示"加入合并队列"的 PR label。
const Label = "kiln:merge"

// merge_queue.state 的取值。
const (
	StateQueued  = "queued"
	StateMerged  = "merged"
	StateRemoved = "removed"
)

// merge_queue.action 的取值。
const (
	actionNone     = "none"
	actionUpdating = "updating"
	actionRemoving = "removing"
	// actionMerging：已经请求合并、还不知道结果。之后看到 PR 已合并时按队列自己合并的处理（关闭关联的 issue）。
	actionMerging = "merging"
)

// sweepInterval 是兜底检查的间隔：队列非空的仓库每隔这么久检查一次（处理超时和丢失的 webhook）。
const sweepInterval = 5 * time.Minute

// auditActor 是审计日志中合并队列自己的操作者。
const auditActor = "mergequeue"

// ForgeFunc 返回仓库的平台接口。
type ForgeFunc func(repo *store.Repo) forge.Repo

// Deps 是 Service 的依赖。
type Deps struct {
	DB *store.DB
	// Forge 为 nil（没有配置 GitHub App）时不处理队列。
	Forge  ForgeFunc
	Outbox *outbox.Writer
	// Hub 在写入通知后唤醒 outbox worker；可以为 nil（等 worker 的兜底轮询）。
	Hub *notify.Hub
	Log *slog.Logger
	// PublicURL 是 Dashboard 的地址，用于 PR 评论中的链接；可以为空。
	PublicURL string
	// Verifications 是 PR 的验证来源（CI、AI 审计），由组装层注入（PLAN #30.3）。
	Verifications []store.VerificationSource
	// HoldPending 判断 PR 是否有待加的 kiln:hold（AI 开发的工作项，PLAN #26.12），由组装层注入；为 nil 表示没有。
	HoldPending HoldPendingFunc
	// DevConflicts 是 AI 开发对合并冲突的处理（PLAN #28.3、#30.3），由组装层注入；为 nil 时所有冲突的 PR 都按第三方处理
	// （出队、请提交者解决）。
	DevConflicts DevConflicts
}

// DevConflicts 是 AI 开发对合并冲突的处理（PLAN #30.3，AI → 合并）：在合并队列持有锁的连接、事务上调用。
type DevConflicts interface {
	// Owner 返回仓库 repoID 的 PR pr（head 分支 headRef）由哪个 AI 开发的工作项负责（只读）。只按 Server 记录的工作项与
	// PR 判断，不看作者、分支名或正文标记。
	Owner(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (DevOwner, error)
	// HandBack 在事务 tx 中把冲突交给负责 PR 的工作项（记下冲突的 head 和 base，通知 AI 开发的执行者）。返回 false 表示
	// 工作项期间已经结束或换了 PR。
	HandBack(ctx context.Context, tx pgx.Tx, c DevConflict) (bool, error)
}

// DevOwnerKind 是冲突的 PR 的归属。
type DevOwnerKind int

const (
	// DevOwnerNone：没有 AI 开发的工作项负责（第三方 PR）：出队，请提交者解决。
	DevOwnerNone DevOwnerKind = iota
	// DevOwnerActive：未结束的工作项负责：出队并交回 AI 开发返修。
	DevOwnerActive
	// DevOwnerEnded：负责它的工作项已经结束：出队，不自动返修，需要人处理。
	DevOwnerEnded
	// DevOwnerRecording：工作项建了 PR、还没记下编号：等它记下之后再判断（不出队、不评论）。
	DevOwnerRecording
)

// DevOwner 是冲突的 PR 的归属：Kind 不是 DevOwnerNone 时 WorkItem 是负责的工作项，State 是它的状态；DevOwnerActive 时
// BaseRef 是工作项本阶段的目标分支（AI 开发只能把这个分支合并进来解决冲突）。
type DevOwner struct {
	Kind     DevOwnerKind
	WorkItem int64
	State    string
	BaseRef  string
}

// DevConflict 是交回 AI 开发的一个冲突：仓库的 PR（head 分支 HeadRef）在 Head 上与目标分支 BaseRef（默认分支）的 Base 冲突。
type DevConflict struct {
	RepoID  int64
	PR      int
	HeadRef string
	Head    string
	BaseRef string
	Base    string
}

// merge_queue.removal 的取值：自动出队的类别（其他出队为空）。
const (
	// RemovalConflictDev：与目标分支冲突，已交回 AI 开发返修。自动入队时"merge commit 的父提交是被移出的 head"一条不适用
	// （AI 解决冲突产生的正是这种 commit，autoenqueue.go）。
	RemovalConflictDev = "conflict_dev"
	// RemovalConflictAuthor：与目标分支冲突，等待提交者（或人）解决。
	RemovalConflictAuthor = "conflict_author"
)

// HoldPendingFunc 判断仓库 repoID 的 PR pr（head 分支 headRef）是否是 AI 开发的工作项的 PR、而工作项决定要加 kiln:hold
// 但还没有确认加上。加上之后 PR 有 label，按 label 判断。
type HoldPendingFunc func(ctx context.Context, q store.Querier, repoID int64, pr int, headRef string) (bool, error)

// Service 是合并队列：入队、移除、列出，以及执行者 Process。
type Service struct {
	db        *store.DB
	forge     ForgeFunc
	outbox    *outbox.Writer
	hub       *notify.Hub
	log       *slog.Logger
	publicURL string
	now       func() time.Time
	// verifications 是 PR 的验证来源。
	verifications []store.VerificationSource
	holdPending   HoldPendingFunc
	devConflicts  DevConflicts
}

// New 返回 Service。
func New(d Deps) *Service {
	return &Service{
		db: d.DB, forge: d.Forge, outbox: d.Outbox, hub: d.Hub, log: d.Log, publicURL: d.PublicURL, now: time.Now, verifications: d.Verifications,
		holdPending: d.HoldPending, devConflicts: d.DevConflicts,
	}
}

// Entry 是 merge_queue 的一行。
type Entry struct {
	ID      int64
	RepoID  int64
	PR      int
	State   string
	HeadSHA string
	// BaseRef 是排队项最近观测到的目标分支（PLAN #28.5）：用于展示、按目标分支计算排位、action=updating 期间 PR 改投
	// 目标分支的检测，以及审计沿用核对。调度时以平台上的 PR 目标分支为唯一权威，读到后回填这里。旧行为空串。
	BaseRef    string
	EnqueuedBy string
	Action     string
	ActionHead string
	// ActionBase 是发起 action=updating 时那次更新的目标分支（PLAN #28.5）：期间 PR 改投分支时，接受更新产生的新 head 和
	// 审计沿用都按它核对，绝不把 dev 更新出来的合并提交当成 main 的更新。旧行为空串（未知，按 BaseRef 兼容判断）。
	ActionBase  string
	ActionAt    *time.Time
	ActionTries int
	Status      string
	Reason      string
	MergeSHA    string
	// CommentState 是出队评论的状态：attempted 表示已经尝试过（PLAN #29.8，结果不明时不再评论）。
	CommentState string
	// MergeLabels 是请求合并时 PR 的 label（merge_labels，与 action=merging 一起写下；nil 表示没有记下）：自动打 tag 用它
	// 决定版本号，合并的响应丢失后重新读到的 label 可能已经被人整理过（PLAN #29.4）。
	MergeLabels []string
	// Removal 是自动出队的类别（RemovalConflictDev、RemovalConflictAuthor，其他出队为空）；ConflictBase 是交回 AI 开发时
	// 默认分支的 head。与 action=removing 一起记下：评论之后崩溃时，重试也知道要交回。
	Removal      string
	ConflictBase string
	EnqueuedAt   time.Time
	UpdatedAt    time.Time
	FinishedAt   *time.Time
}

const entryCols = `id, repo_id, pr_number, state, head_sha, base_ref, enqueued_by, action, action_head, action_base, action_at, action_tries,
	status, reason, merge_sha, comment_state, merge_labels, removal, conflict_base, enqueued_at, updated_at, finished_at`

func scanEntries(rows pgx.Rows) ([]*Entry, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Entry, error) {
		var e Entry
		err := r.Scan(&e.ID, &e.RepoID, &e.PR, &e.State, &e.HeadSHA, &e.BaseRef, &e.EnqueuedBy, &e.Action, &e.ActionHead, &e.ActionBase,
			&e.ActionAt, &e.ActionTries, &e.Status, &e.Reason, &e.MergeSHA, &e.CommentState, &e.MergeLabels, &e.Removal, &e.ConflictBase,
			&e.EnqueuedAt, &e.UpdatedAt, &e.FinishedAt)
		return &e, err
	})
}

// Queued 返回仓库正在排队的项，按入队顺序（第一个是队首）。
func Queued(ctx context.Context, q store.Querier, repoID int64) ([]*Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryCols+` FROM merge_queue WHERE repo_id=$1 AND state='queued' ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// Recent 返回仓库最近结束（合并或出队）的 limit 项，按结束时间新的在前。
func Recent(ctx context.Context, q store.Querier, repoID int64, limit int) ([]*Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryCols+` FROM merge_queue WHERE repo_id=$1 AND state<>'queued'
		ORDER BY finished_at DESC, id DESC LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// Latest 返回 PR 最近的一项（正在排队的优先）；没有时返回 nil。
func Latest(ctx context.Context, q store.Querier, repoID int64, pr int) (*Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryCols+` FROM merge_queue WHERE repo_id=$1 AND pr_number=$2
		ORDER BY state='queued' DESC, id DESC LIMIT 1`, repoID, pr)
	if err != nil {
		return nil, err
	}
	es, err := scanEntries(rows)
	if err != nil || len(es) == 0 {
		return nil, err
	}
	return es[0], nil
}

// UpdatedFrom 返回队列为 PR 更新分支时的旧 head（action_head）：PR 正在排队，并且正在更新分支，或者已经接受了新 head head
// （接受时 action_head 保留）。不是这种情况时返回空。规划新 head 的 AI 审计时据此沿用旧 head 的审计（PLAN #28.4）；
// 新 head 是否真的由这次更新产生（父提交）由调用方确认。
//
// baseRef 是这次要审计的目标分支：只按发起更新时记下的 action_base 判断，不能看展示用的 base_ref——acceptHead 之后 PR
// 改投分支时 action_head 保留、base_ref 已经换成新分支，看 base_ref 会让 main 的审计沿用 dev 那次更新的旧结论
// （PLAN #28.5）。升级前的旧行没有 action_base，退回按 base_ref（为空时放行）兼容判断。
func UpdatedFrom(ctx context.Context, q store.Querier, repoID int64, pr int, head, baseRef string) (string, error) {
	var from string
	err := q.QueryRow(ctx, `SELECT action_head FROM merge_queue WHERE repo_id=$1 AND pr_number=$2 AND state='queued'
		AND (action=$4 OR head_sha=$3)
		AND ((action_base<>'' AND action_base=$5) OR (action_base='' AND (base_ref='' OR base_ref=$5)))`,
		repoID, pr, head, actionUpdating, baseRef).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return from, err
}

// Enqueue 把 PR 加入队列（PR 已经在队列中时什么也不做），并通知执行者。head 是入队时 PR 的 head（可以为空，
// 第一次处理时记录），baseRef 是 PR 的目标分支，actor 是操作者。返回是否新加入。入队的条件（PR 打开、目标是允许合并
// 的分支、不是 draft）由执行者检查，不满足时出队并评论原因。
func (s *Service) Enqueue(ctx context.Context, repoID int64, pr int, head, baseRef, actor string) (bool, error) {
	var added bool
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO merge_queue (repo_id, pr_number, head_sha, base_ref, enqueued_by) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (repo_id, pr_number) WHERE state='queued' DO NOTHING`, repoID, pr, head, baseRef, actor)
		if err != nil {
			return err
		}
		added = tag.RowsAffected() > 0
		if !added {
			return nil
		}
		if err := store.Audit(ctx, tx, actor, "mergequeue.enqueue", map[string]any{"repo_id": repoID, "pr": pr, "head": head, "base_ref": baseRef}); err != nil {
			return err
		}
		return mqevents.EnqueueMergeQueue(ctx, s.outbox, tx, repoID)
	})
	if added && err == nil {
		s.wake()
	}
	return added, err
}

// wake 唤醒 outbox worker 投递刚写入的通知。
func (s *Service) wake() {
	if s.hub != nil {
		s.hub.All()
	}
}

// Notify 在 PR pr（0 表示任意 PR）在队列中时通知执行者检查仓库的队列。
func (s *Service) Notify(ctx context.Context, repoID int64, pr int) error {
	if err := mqevents.KickMergeQueue(ctx, s.outbox, s.db, repoID, pr); err != nil {
		return err
	}
	s.wake()
	return nil
}

// ErrNoForge 表示没有配置代码托管平台（GitHub App），不能操作 PR。
var ErrNoForge = errors.New("没有配置 GitHub App，不能操作 PR")

// Add 在 Dashboard 或 CLI 中把 PR 加入队列：给 PR 加上 label（与在平台上加 label 等价）并入队。
func (s *Service) Add(ctx context.Context, repo *store.Repo, pr int, actor string) (bool, error) {
	if s.forge == nil {
		return false, ErrNoForge
	}
	f := s.forge(repo)
	p, err := f.GetPull(ctx, pr)
	if err != nil {
		return false, fmt.Errorf("读取 PR #%d：%w", pr, err)
	}
	if p.State != forge.PullOpen {
		return false, fmt.Errorf("PR #%d 不是打开的", pr)
	}
	if !p.HasLabel(Label) {
		if err := f.AddLabel(ctx, pr, Label); err != nil {
			return false, fmt.Errorf("给 PR #%d 加 %s：%w", pr, Label, err)
		}
	}
	return s.Enqueue(ctx, repo.ID, pr, p.HeadSHA, p.BaseRef, actor)
}

// Remove 在 Dashboard 或 CLI 中把 PR 移出队列：去掉 PR 的 label（与在平台上去掉 label 等价），由执行者出队。
func (s *Service) Remove(ctx context.Context, repo *store.Repo, pr int, actor string) error {
	if s.forge == nil {
		return ErrNoForge
	}
	if err := s.forge(repo).RemoveLabel(ctx, pr, Label); err != nil {
		return fmt.Errorf("去掉 PR #%d 的 %s：%w", pr, Label, err)
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.Audit(ctx, tx, actor, "mergequeue.remove", map[string]any{"repo_id": repo.ID, "pr": pr}); err != nil {
			return err
		}
		return mqevents.EnqueueMergeQueue(ctx, s.outbox, tx, repo.ID)
	})
	if err == nil {
		s.wake()
	}
	return err
}

// HandleOutbox 投递 outbox 的 mergequeue.repo 记录（注册到 outbox.Worker）。
func (s *Service) HandleOutbox(ctx context.Context, payload []byte) error {
	var p mqevents.RepoPayload
	if err := unmarshal(payload, &p); err != nil {
		return err
	}
	return s.Process(ctx, p.RepoID)
}

// Run 是兜底检查：每 sweepInterval 通知一次队列非空（或有待关闭关联 issue）的仓库，直到 ctx 结束。队列为空的仓库不检查，不请求平台；
// 停用的仓库在处理时出队（removeInactive），之后不再被检查。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("合并队列兜底检查", "err", err)
		}
	}
}

func (s *Service) sweep(ctx context.Context) error {
	// 已合并、还没关闭关联 issue 的仓库也要检查（关闭失败后重试）
	ids, err := store.QueryIDs(ctx, s.db, `SELECT DISTINCT repo_id FROM merge_queue WHERE state='queued' OR issues_state='pending'
		UNION SELECT repo_id FROM merge_auto_candidates`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := mqevents.EnqueueMergeQueue(ctx, s.outbox, s.db, id); err != nil {
			return err
		}
	}
	return nil
}

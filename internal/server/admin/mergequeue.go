package admin

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 合并队列（PLAN #28）：Dashboard 和 CLI 共用。入队和移除等同于在平台上给 PR 加上、去掉 label kiln:merge。

// recentMergeQueue 是列出合并队列时附带的最近结束的项数。
const recentMergeQueue = 20

// MergeQueue 是仓库的合并队列：排队中的项（第一个是队首）和最近结束的项。
type MergeQueue struct {
	Repo   *store.Repo
	Queued []*mergequeue.Entry
	Recent []*mergequeue.Entry
}

// ListMergeQueue 列出仓库 repo（owner/name）的合并队列。
func (s *Service) ListMergeQueue(ctx context.Context, repo string) (*MergeQueue, error) {
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return nil, err
	}
	return s.mergeQueueOf(ctx, r)
}

// ListMergeQueues 列出有合并队列记录（排队中或最近结束）或自动打 tag 记录的全部仓库的队列，按仓库全名排序。只有因目标
// 分支不是发版分支而跳过的自动打 tag 记录的仓库不算：这些是高频的开发分支合并，不应让仓库出现在合并队列页（PLAN #29.4）。
func (s *Service) ListMergeQueues(ctx context.Context) ([]*MergeQueue, error) {
	// 有合并队列记录或自动打 tag 记录（在队列之外合入的 PR 也会自动打 tag）的仓库
	ids, err := store.QueryIDs(ctx, s.db, `SELECT r.id FROM repos r WHERE r.active
		AND (EXISTS (SELECT 1 FROM merge_queue q WHERE q.repo_id=r.id)
			OR EXISTS (SELECT 1 FROM auto_tags t WHERE t.repo_id=r.id AND NOT (t.state='skipped' AND t.skip_code=$1)))
		ORDER BY lower(r.owner), lower(r.name)`, autotag.SkipCodeBranchNotRelease)
	if err != nil {
		return nil, err
	}
	out := make([]*MergeQueue, 0, len(ids))
	for _, id := range ids {
		r, err := store.GetRepo(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		q, err := s.mergeQueueOf(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

// mergeQueueOf 读取仓库 r 的合并队列：排队中的项和最近结束的 recentMergeQueue 项。
func (s *Service) mergeQueueOf(ctx context.Context, r *store.Repo) (*MergeQueue, error) {
	q := &MergeQueue{Repo: r}
	var err error
	if q.Queued, err = mergequeue.Queued(ctx, s.db, r.ID); err != nil {
		return nil, err
	}
	if q.Recent, err = mergequeue.Recent(ctx, s.db, r.ID, recentMergeQueue); err != nil {
		return nil, err
	}
	return q, nil
}

// PullMergeQueue 返回 PR 在合并队列中最近的一项（正在排队的优先），以及它在队列中的位置（从 1 开始，不在排队时为 0）。
// 多目标分支时按同一个目标分支算排位（PLAN #28.5）：不同分支各自排队、互不阻塞。旧行的 base_ref 为空（升级前入队），
// 与任何分支一起计数。没有记录时返回 nil。
func (s *Service) PullMergeQueue(ctx context.Context, repoID int64, pr int) (*mergequeue.Entry, int, error) {
	e, err := mergequeue.Latest(ctx, s.db, repoID, pr)
	if err != nil || e == nil || e.State != mergequeue.StateQueued {
		return e, 0, err
	}
	var pos int
	if e.BaseRef == "" {
		err = s.db.QueryRow(ctx, `SELECT count(*) FROM merge_queue WHERE repo_id=$1 AND state='queued' AND id<=$2`, repoID, e.ID).Scan(&pos)
	} else {
		err = s.db.QueryRow(ctx, `SELECT count(*) FROM merge_queue WHERE repo_id=$1 AND state='queued' AND (base_ref=$3 OR base_ref='')
			AND id<=$2`, repoID, e.ID, e.BaseRef).Scan(&pos)
	}
	return e, pos, err
}

// AddToMergeQueue 把仓库 repo 的 PR 加入合并队列，返回是否新加入（false 表示已经在队列中）。
func (s *Service) AddToMergeQueue(ctx context.Context, repo string, pr int, actor string) (bool, error) {
	r, err := s.mergeQueueRepo(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	return s.mergeQueue.Add(ctx, r, pr, actor)
}

// RemoveFromMergeQueue 把仓库 repo 的 PR 移出合并队列。
func (s *Service) RemoveFromMergeQueue(ctx context.Context, repo string, pr int, actor string) error {
	r, err := s.mergeQueueRepo(ctx, repo, pr)
	if err != nil {
		return err
	}
	return s.mergeQueue.Remove(ctx, r, pr, actor)
}

func (s *Service) mergeQueueRepo(ctx context.Context, repo string, pr int) (*store.Repo, error) {
	if s.mergeQueue == nil {
		return nil, mergequeue.ErrNoForge
	}
	if pr <= 0 {
		return nil, invalid(fmt.Errorf("PR 号 %d 无效", pr))
	}
	return store.FindRepo(ctx, s.db, repo)
}

func mergeQueueEntryMsg(e *mergequeue.Entry) *apiv1.MergeQueueEntry {
	m := &apiv1.MergeQueueEntry{
		Id: e.ID, Pr: int32(e.PR), State: e.State, HeadSha: e.HeadSHA, EnqueuedBy: e.EnqueuedBy, //nolint:gosec // PR 号不会超过 int32
		Status: e.Status, Reason: e.Reason, MergeSha: e.MergeSHA, EnqueuedAt: e.EnqueuedAt.Unix(), Removal: e.Removal, BaseRef: e.BaseRef,
	}
	if e.FinishedAt != nil {
		m.FinishedAt = e.FinishedAt.Unix()
	}
	return m
}

// mergeQueueErr 把合并队列操作的错误转换成 RPC 错误：仓库不存在为 NotFound，平台拒绝等为 FailedPrecondition，
// 内部错误原样返回（见 rpcError）。
func mergeQueueErr(err error) error {
	if errors.Is(err, store.ErrRepoNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return rpcError(connect.CodeFailedPrecondition, err)
}

func (a *rpcAPI) ListMergeQueue(ctx context.Context, req *connect.Request[apiv1.ListMergeQueueRequest]) (*connect.Response[apiv1.ListMergeQueueResponse], error) {
	q, err := a.s.ListMergeQueue(ctx, req.Msg.Repo)
	if err != nil {
		return nil, mergeQueueErr(err)
	}
	resp := &apiv1.ListMergeQueueResponse{}
	for _, e := range q.Queued {
		resp.Queued = append(resp.Queued, mergeQueueEntryMsg(e))
	}
	for _, e := range q.Recent {
		resp.Recent = append(resp.Recent, mergeQueueEntryMsg(e))
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) AddToMergeQueue(ctx context.Context, req *connect.Request[apiv1.AddToMergeQueueRequest]) (*connect.Response[apiv1.AddToMergeQueueResponse], error) {
	added, err := a.s.AddToMergeQueue(ctx, req.Msg.Repo, int(req.Msg.Pr), rpcActor(ctx))
	if err != nil {
		return nil, mergeQueueErr(err)
	}
	return connect.NewResponse(&apiv1.AddToMergeQueueResponse{Added: added}), nil
}

func (a *rpcAPI) RemoveFromMergeQueue(ctx context.Context, req *connect.Request[apiv1.RemoveFromMergeQueueRequest]) (*connect.Response[apiv1.RemoveFromMergeQueueResponse], error) {
	if err := a.s.RemoveFromMergeQueue(ctx, req.Msg.Repo, int(req.Msg.Pr), rpcActor(ctx)); err != nil {
		return nil, mergeQueueErr(err)
	}
	return connect.NewResponse(&apiv1.RemoveFromMergeQueueResponse{}), nil
}

package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 自动打 tag（PLAN #29.7）：Dashboard 和 CLI 共用。

// defaultAutoTags 是列出自动打 tag 记录的默认条数，maxAutoTags 是上限。
const (
	defaultAutoTags = 50
	maxAutoTags     = 500
)

// ListAutoTags 列出仓库 repo（owner/name）最近的自动打 tag 记录（新的在前）。
func (s *Service) ListAutoTags(ctx context.Context, repo string, limit int) (*store.Repo, []*autotag.Record, error) {
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = defaultAutoTags
	}
	list, err := autotag.List(ctx, s.db, r.ID, min(limit, maxAutoTags))
	return r, list, err
}

// AutoTagsOf 返回仓库 repoID 最近的 limit 条记录（Dashboard 的合并队列页）。因目标分支不是发版分支而跳过的记录不显示：
// 高频的开发分支合并不把正式版本的记录挤出列表（PLAN #29.4）。
func (s *Service) AutoTagsOf(ctx context.Context, repoID int64, limit int) ([]*autotag.Record, error) {
	return autotag.ListVisible(ctx, s.db, repoID, limit)
}

// RetryAutoTag 重新处理仓库 repo 中 commit sha 的失败或跳过的记录。
func (s *Service) RetryAutoTag(ctx context.Context, repo, sha, actor string) error {
	if s.autoTag == nil {
		return errors.New("自动打 tag 没有启用")
	}
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return err
	}
	return s.autoTag.Retry(ctx, r.ID, sha, actor)
}

func autoTagMsg(r *autotag.Record) *apiv1.AutoTag {
	m := &apiv1.AutoTag{
		Id: r.ID, Sha: r.SHA, Pr: int32(r.PR), Labels: r.Labels, MergedBy: r.MergedBy, Source: r.Source, //nolint:gosec // PR 号不会超过 int32
		State: r.State, Tag: r.Tag, Reason: r.Reason, Tries: int32(r.Tries), SenderIsBot: r.SenderIsBot, //nolint:gosec // 重试次数不会超过 int32
		CreatedAt: r.CreatedAt.Unix(), UpdatedAt: r.UpdatedAt.Unix(), BaseRef: r.BaseRef,
	}
	if r.WorkItemID != nil {
		m.WorkItemId = *r.WorkItemID
	}
	return m
}

func (a *rpcAPI) ListAutoTags(ctx context.Context, req *connect.Request[apiv1.ListAutoTagsRequest]) (*connect.Response[apiv1.ListAutoTagsResponse], error) {
	_, list, err := a.s.ListAutoTags(ctx, req.Msg.Repo, int(req.Msg.Limit))
	if err != nil {
		return nil, mergeQueueErr(err)
	}
	resp := &apiv1.ListAutoTagsResponse{}
	for _, r := range list {
		resp.Tags = append(resp.Tags, autoTagMsg(r))
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) RetryAutoTag(ctx context.Context, req *connect.Request[apiv1.RetryAutoTagRequest]) (*connect.Response[apiv1.RetryAutoTagResponse], error) {
	if err := a.s.RetryAutoTag(ctx, req.Msg.Repo, req.Msg.Sha, rpcActor(ctx)); err != nil {
		if errors.Is(err, autotag.ErrNotRetryable) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, mergeQueueErr(err)
	}
	return connect.NewResponse(&apiv1.RetryAutoTagResponse{}), nil
}

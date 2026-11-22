package admin

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 开发（PLAN #26.11、#26.15）：Dashboard 和 CLI 共用。从 GitHub 全量同步任务池、暂停 / 恢复仓库的新领取、终止工作项、
// 让受阻的工作项再返修一轮；列出和查看工作项、任务池。

// defaultWorkItems 是列出工作项的默认个数，maxWorkItems 是上限。
const (
	defaultWorkItems = 50
	maxWorkItems     = 500
)

// errAIDevOff 表示 Server 没有启用 AI 开发（没有配置 GitHub App）。
var errAIDevOff = errors.New("AI 开发没有启用（需要配置 GitHub App）")

// SyncAIDev 从 GitHub 全量同步仓库 repo（owner/name）的任务池（webhook 可能丢失）。
func (s *Service) SyncAIDev(ctx context.Context, repo, actor string) (*aidev.SyncResult, error) {
	if s.aiDev == nil {
		return nil, errAIDevOff
	}
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return nil, err
	}
	return s.aiDev.SyncPool(ctx, r.ID, actor)
}

// PauseAIDev 暂停（paused 为 true）或恢复仓库 repo 的新领取。
func (s *Service) PauseAIDev(ctx context.Context, repo string, paused bool, actor string) error {
	if s.aiDev == nil {
		return errAIDevOff
	}
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return err
	}
	return s.aiDev.SetPaused(ctx, r.ID, paused, actor)
}

// ListWorkItems 列出仓库 repo（为空时全部仓库）的工作项，新的在前。
func (s *Service) ListWorkItems(ctx context.Context, repo string, limit int) ([]*aidev.View, error) {
	var repoID int64
	if repo != "" {
		r, err := store.FindRepo(ctx, s.db, repo)
		if err != nil {
			return nil, err
		}
		repoID = r.ID
	}
	if limit <= 0 {
		limit = defaultWorkItems
	}
	return aidev.ListViews(ctx, s.db, repoID, min(limit, maxWorkItems))
}

// RepoOccupancy 返回仓库 repo 的 AI 开发占用（PLAN #26.9 I13）。
func (s *Service) RepoOccupancy(ctx context.Context, repo string) (*aidev.Occupancy, error) {
	r, err := store.FindRepo(ctx, s.db, repo)
	if err != nil {
		return nil, err
	}
	return aidev.RepoOccupancy(ctx, s.db, r.ID)
}

// WorkItem 返回工作项 id 的全部记录。
func (s *Service) WorkItem(ctx context.Context, id int64) (*aidev.Detail, error) {
	return aidev.GetDetail(ctx, s.db, id)
}

// StopWorkItem 终止工作项 id（取消执行中的 job，释放 Issue；PR 保持原样）。
func (s *Service) StopWorkItem(ctx context.Context, id int64, actor string) error {
	if s.aiDev == nil {
		return errAIDevOff
	}
	return s.aiDev.Stop(ctx, id, "管理员终止（"+actor+"）", actor)
}

// RetryWorkItem 让受阻的工作项 id 再返修一轮。
func (s *Service) RetryWorkItem(ctx context.Context, id int64, actor string) error {
	if s.aiDev == nil {
		return errAIDevOff
	}
	return s.aiDev.Retry(ctx, id, actor)
}

// aiDevErr 把 AI 开发操作的错误转换为 RPC 错误。
func aiDevErr(err error) error {
	switch {
	case errors.Is(err, store.ErrRepoNotFound), errors.Is(err, aidev.ErrRepoNotFound), errors.Is(err, aidev.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, aidev.ErrNotRetryable), errors.Is(err, aidev.ErrFinished), errors.Is(err, errAIDevOff):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return rpcError(connect.CodeInternal, err)
}

func workItemMsg(v *aidev.View) *apiv1.WorkItem {
	m := &apiv1.WorkItem{
		Id: v.ID, Repo: v.Repo, State: v.State, Branch: v.Branch(), HeadSha: v.HeadSHA,
		FixRounds: int32(v.FixRounds), MaxFixRounds: int32(v.MaxFixRounds), //nolint:gosec // 轮数很小
		Agent: v.Agent, Reason: v.Reason, CreatedAt: v.CreatedAt.Unix(), UpdatedAt: v.UpdatedAt.Unix(),
		Hold: v.HoldLabel, RequirementsChanged: v.RequirementsChanged,
		Phase: v.Phase, DesignFirst: v.DesignFirst(), ImplRound: int32(v.ImplRound), RoundFixes: int32(v.RoundCheckFixes()), //nolint:gosec // 轮数很小
		RoundConflicts: int32(v.RoundConflicts), //nolint:gosec // 轮数很小
	}
	if v.DesignPR != nil {
		m.DesignPr = int32(*v.DesignPR) //nolint:gosec // PR 号不会超过 int32
	}
	if v.PRNumber != nil {
		m.PrNumber = int32(*v.PRNumber) //nolint:gosec // PR 号不会超过 int32
	}
	for _, is := range v.Issues {
		m.Issues = append(m.Issues, int32(is.Number)) //nolint:gosec // Issue 号不会超过 int32
	}
	return m
}

// occupancyMsg 返回仓库占用的消息。
func occupancyMsg(o *aidev.Occupancy) *apiv1.RepoOccupancy {
	m := &apiv1.RepoOccupancy{Reason: o.Why(), Waiting: o.Waiting}
	if o.Occupant != nil {
		m.WorkItem = o.Occupant.ID
	}
	return m
}

func (a *rpcAPI) SyncAIDev(ctx context.Context, req *connect.Request[apiv1.SyncAIDevRequest]) (*connect.Response[apiv1.SyncAIDevResponse], error) {
	res, err := a.s.SyncAIDev(ctx, req.Msg.Repo, rpcActor(ctx))
	if err != nil {
		return nil, aiDevErr(err)
	}
	return connect.NewResponse(&apiv1.SyncAIDevResponse{
		Listed: clampInt32(res.Listed), Complete: res.Complete, Changed: clampInt32(res.Changed), Candidates: clampInt32(res.Candidates),
	}), nil
}

func (a *rpcAPI) PauseAIDev(ctx context.Context, req *connect.Request[apiv1.PauseAIDevRequest]) (*connect.Response[apiv1.PauseAIDevResponse], error) {
	if err := a.s.PauseAIDev(ctx, req.Msg.Repo, req.Msg.Paused, rpcActor(ctx)); err != nil {
		return nil, aiDevErr(err)
	}
	return connect.NewResponse(&apiv1.PauseAIDevResponse{}), nil
}

func (a *rpcAPI) ListWorkItems(ctx context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	list, err := a.s.ListWorkItems(ctx, req.Msg.Repo, int(req.Msg.Limit))
	if err != nil {
		return nil, aiDevErr(err)
	}
	resp := &apiv1.ListWorkItemsResponse{}
	for _, v := range list {
		resp.Items = append(resp.Items, workItemMsg(v))
	}
	if req.Msg.Repo != "" {
		occ, err := a.s.RepoOccupancy(ctx, req.Msg.Repo)
		if err != nil {
			return nil, aiDevErr(err)
		}
		resp.Occupancy = occupancyMsg(occ)
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) StopWorkItem(ctx context.Context, req *connect.Request[apiv1.StopWorkItemRequest]) (*connect.Response[apiv1.StopWorkItemResponse], error) {
	if err := a.s.StopWorkItem(ctx, req.Msg.Id, rpcActor(ctx)); err != nil {
		return nil, aiDevErr(err)
	}
	return connect.NewResponse(&apiv1.StopWorkItemResponse{}), nil
}

func (a *rpcAPI) RetryWorkItem(ctx context.Context, req *connect.Request[apiv1.RetryWorkItemRequest]) (*connect.Response[apiv1.RetryWorkItemResponse], error) {
	if err := a.s.RetryWorkItem(ctx, req.Msg.Id, rpcActor(ctx)); err != nil {
		return nil, aiDevErr(err)
	}
	return connect.NewResponse(&apiv1.RetryWorkItemResponse{}), nil
}

// AIDevBranchTTL 返回 AI 开发 attempt 分支的保留时间（没有配置 AI 开发时为默认值）。
func (s *Service) AIDevBranchTTL() time.Duration {
	if s.aiDev == nil {
		return aidev.DefaultBranchTTL
	}
	return s.aiDev.BranchTTL()
}

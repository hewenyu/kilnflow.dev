package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/decisioninbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 「待决策清单」的配置、列表和人工对账。业务规则在 decisioninbox 服务里，这里只做参数转换和仓库名解析。

const (
	// decisionSyncDefaultLimit 是列表默认条数。
	decisionSyncDefaultLimit = 50
	// decisionSyncMaxLimit 是列表条数上限。
	decisionSyncMaxLimit = 500
)

func (a *rpcAPI) ListDecisionDocs(ctx context.Context, _ *connect.Request[apiv1.ListDecisionDocsRequest]) (*connect.Response[apiv1.ListDecisionDocsResponse], error) {
	list, err := a.s.ListDecisionDocs(ctx)
	if err != nil {
		return nil, rpcError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ListDecisionDocsResponse{DecisionDocs: list}), nil
}

func (a *rpcAPI) SetDecisionDoc(ctx context.Context, req *connect.Request[apiv1.SetDecisionDocRequest]) (*connect.Response[apiv1.SetDecisionDocResponse], error) {
	m := req.Msg
	out, cancelled, unknown, err := a.s.SetDecisionDoc(ctx, m.Repo, m.BaseUrl, m.DocId, m.Token, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.SetDecisionDocResponse{
		DecisionDoc: out, CancelledCount: cancelled, UnknownCount: unknown,
	}), nil
}

func (a *rpcAPI) ClearDecisionDoc(ctx context.Context, req *connect.Request[apiv1.ClearDecisionDocRequest]) (*connect.Response[apiv1.ClearDecisionDocResponse], error) {
	cancelled, unknown, err := a.s.ClearDecisionDoc(ctx, req.Msg.Repo, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.ClearDecisionDocResponse{CancelledCount: cancelled, UnknownCount: unknown}), nil
}

func (a *rpcAPI) ListDecisionSyncs(ctx context.Context, req *connect.Request[apiv1.ListDecisionSyncsRequest]) (*connect.Response[apiv1.ListDecisionSyncsResponse], error) {
	m := req.Msg
	list, err := a.s.ListDecisionSyncs(ctx, m.Repo, m.State, int(m.Limit))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.ListDecisionSyncsResponse{Syncs: list}), nil
}

func (a *rpcAPI) ReconcileDecisionSync(ctx context.Context, req *connect.Request[apiv1.ReconcileDecisionSyncRequest]) (*connect.Response[apiv1.ReconcileDecisionSyncResponse], error) {
	sync, found, err := a.s.ReconcileDecisionSync(ctx, req.Msg.Id, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.ReconcileDecisionSyncResponse{Sync: sync, Found: found}), nil
}

func (a *rpcAPI) ResolveDecisionSync(ctx context.Context, req *connect.Request[apiv1.ResolveDecisionSyncRequest]) (*connect.Response[apiv1.ResolveDecisionSyncResponse], error) {
	sync, err := a.s.ResolveDecisionSync(ctx, req.Msg.Id, req.Msg.State, req.Msg.Note, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.ResolveDecisionSyncResponse{Sync: sync}), nil
}

// ListDecisionDocs 返回全部仓库的「待决策清单」配置（不含 token），附带待处理 / 结果不明的条目数量。
func (s *Service) ListDecisionDocs(ctx context.Context) ([]*apiv1.DecisionDoc, error) {
	docs, err := store.ListDecisionDocs(ctx, s.db)
	if err != nil {
		return nil, err
	}
	names, counts, err := s.decisionDocMeta(ctx, docs)
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.DecisionDoc, 0, len(docs))
	for i := range docs {
		out = append(out, decisionDocMsg(names[docs[i].RepoID], &docs[i], counts[docs[i].RepoID]))
	}
	return out, nil
}

// SetDecisionDoc 设置或重设仓库的「待决策清单」。
func (s *Service) SetDecisionDoc(ctx context.Context, fullName, baseURL, docID, token, actor string) (
	*apiv1.DecisionDoc, int64, int64, error,
) {
	if s.decision == nil {
		return nil, 0, 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有启用待决策清单同步"))
	}
	repo, err := store.FindRepo(ctx, s.db, fullName)
	if err != nil {
		return nil, 0, 0, err
	}
	res, err := s.decision.SetDecisionDoc(ctx, repo.ID, baseURL, docID, token, actor)
	if err != nil {
		return nil, 0, 0, err
	}
	pending, unknown, err := decisionDocCounts(ctx, s.db, repo.ID)
	if err != nil {
		return nil, 0, 0, err
	}
	return decisionDocMsg(repo.FullName(), res.Doc, counts{pending: pending, unknown: unknown}), res.Cancelled, res.Unknown, nil
}

// ClearDecisionDoc 删除仓库的「待决策清单」配置，返回被取消 / 转为 unknown 的条目数量。
func (s *Service) ClearDecisionDoc(ctx context.Context, fullName, actor string) (int64, int64, error) {
	if s.decision == nil {
		return 0, 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有启用待决策清单同步"))
	}
	repo, err := store.FindRepo(ctx, s.db, fullName)
	if err != nil {
		return 0, 0, err
	}
	res, err := s.decision.ClearDecisionDoc(ctx, repo.ID, actor)
	if err != nil {
		return 0, 0, err
	}
	return res.Cancelled, res.Unknown, nil
}

// ListDecisionSyncs 返回同步记录（可按仓库、状态筛选）。
func (s *Service) ListDecisionSyncs(ctx context.Context, fullName, state string, limit int) ([]*apiv1.DecisionSync, error) {
	if limit <= 0 {
		limit = decisionSyncDefaultLimit
	}
	limit = min(limit, decisionSyncMaxLimit)
	var repoID *int64
	if fullName != "" {
		repo, err := store.FindRepo(ctx, s.db, fullName)
		if err != nil {
			return nil, err
		}
		repoID = &repo.ID
	}
	list, err := decisioninbox.ListSyncs(ctx, s.db, repoID, state, limit)
	if err != nil {
		return nil, err
	}
	names, err := s.repoNames(ctx, syncRepoIDs(list))
	if err != nil {
		return nil, err
	}
	out := make([]*apiv1.DecisionSync, 0, len(list))
	for i := range list {
		out = append(out, decisionSyncMsg(names[list[i].RepoID], &list[i]))
	}
	return out, nil
}

// ReconcileDecisionSync 对 unknown / attempted 的条目做一次只读查证。
func (s *Service) ReconcileDecisionSync(ctx context.Context, id int64, actor string) (*apiv1.DecisionSync, bool, error) {
	if s.decision == nil {
		return nil, false, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有启用待决策清单同步"))
	}
	res, err := s.decision.ReconcileSync(ctx, id, actor)
	if err != nil {
		return nil, false, err
	}
	names, err := s.repoNames(ctx, []int64{res.Sync.RepoID})
	if err != nil {
		return nil, false, err
	}
	return decisionSyncMsg(names[res.Sync.RepoID], res.Sync), res.Found, nil
}

// ResolveDecisionSync 人工把 unknown 的条目结案。
func (s *Service) ResolveDecisionSync(ctx context.Context, id int64, targetState, note, actor string) (*apiv1.DecisionSync, error) {
	if s.decision == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("没有启用待决策清单同步"))
	}
	sync, err := s.decision.ResolveSync(ctx, id, targetState, note, actor)
	if err != nil {
		return nil, err
	}
	names, err := s.repoNames(ctx, []int64{sync.RepoID})
	if err != nil {
		return nil, err
	}
	return decisionSyncMsg(names[sync.RepoID], sync), nil
}

// counts 是仓库的条目数量。
type counts struct {
	pending, unknown int64
}

func decisionDocCounts(ctx context.Context, q store.Querier, repoID int64) (pending, unknown int64, err error) {
	err = q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='pending'), count(*) FILTER (WHERE state='unknown')
		FROM dev_decision_syncs WHERE repo_id=$1`, repoID).Scan(&pending, &unknown)
	return pending, unknown, err
}

// decisionDocMeta 一次查出仓库名与各仓库的条目数量。
func (s *Service) decisionDocMeta(ctx context.Context, docs []store.DecisionDoc) (map[int64]string, map[int64]counts, error) {
	ids := make([]int64, 0, len(docs))
	for i := range docs {
		ids = append(ids, docs[i].RepoID)
	}
	names, err := s.repoNames(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[int64]counts, len(ids))
	rows, err := s.db.Query(ctx, `SELECT repo_id, count(*) FILTER (WHERE state='pending'), count(*) FILTER (WHERE state='unknown')
		FROM dev_decision_syncs WHERE repo_id = ANY($1) GROUP BY repo_id`, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c counts
		if err := rows.Scan(&id, &c.pending, &c.unknown); err != nil {
			return nil, nil, err
		}
		out[id] = c
	}
	return names, out, rows.Err()
}

func syncRepoIDs(list []decisioninbox.Sync) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for i := range list {
		if !seen[list[i].RepoID] {
			seen[list[i].RepoID] = true
			out = append(out, list[i].RepoID)
		}
	}
	return out
}

func decisionDocMsg(fullName string, d *store.DecisionDoc, c counts) *apiv1.DecisionDoc {
	if d == nil {
		return nil
	}
	out := &apiv1.DecisionDoc{
		Repo: fullName, Provider: d.Provider, BaseUrl: d.BaseURL, DocId: d.DocID,
		CollectionId: d.CollectionID, Generation: d.Generation, HasToken: true,
		UpdatedBy: d.UpdatedBy, UpdatedAt: d.UpdatedAt.Unix(),
		WarningCode: d.WarningCode, WarnedAt: unixOrZero(d.WarnedAt),
		PendingCount: c.pending, UnknownCount: c.unknown,
	}
	if d.ObservedUnits != nil {
		out.ObservedUnits = *d.ObservedUnits
	}
	out.ObservedAt = unixOrZero(d.ObservedAt)
	return out
}

func decisionSyncMsg(fullName string, v *decisioninbox.Sync) *apiv1.DecisionSync {
	return &apiv1.DecisionSync{
		Id: v.ID, Repo: fullName, WorkItemId: v.WorkItemID, TaskId: v.TaskID, EntryIdx: clampInt32(v.EntryIdx),
		EntryKind: v.EntryKind, Category: v.Category, ConfirmedDecision: v.ConfirmedDecision, Marker: v.Marker,
		TargetDocId: v.TargetDocID, State: v.State, Attempts: clampInt32(v.Attempts),
		LastErrorCode: v.LastErrorCode, LastError: v.LastError, CreatedAt: v.CreatedAt.Unix(),
		AttemptedAt: unixOrZero(v.AttemptedAt), DeliveredAt: unixOrZero(v.DeliveredAt),
		ResolvedBy: v.ResolvedBy, ResolutionNote: v.ResolutionNote,
	}
}

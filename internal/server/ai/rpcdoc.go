package ai

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// docsBasisError 把文档依据的永久性错误映射为 FailedPrecondition（Runner 重试也不会成功）：依据记录与实际快照不一致、
// 任务 spec 无法解析。其他错误（数据库、网络）原样返回，由 RPC 层按内部错误处理，Runner 稍后重试。
func docsBasisError(err error) error {
	if errors.Is(err, ErrDocsBasisConflict) || errors.Is(err, ErrTaskSpec) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return err
}

// SetDocs 设置外部设计文档快照的读取（组装时调用，在 Server 开始服务之前）。为 nil 时文档命令返回空。
func (c *Core) SetDocs(d *docsync.Service) { c.docs = d }

// workItemByAttempt 返回这次 attempt 领取到的工作项（选题任务在同一 attempt 中领取，spec 里的 work_item_id 仍是 0，
// 只能按 claim_attempt_id 找）；没有时返回 0。
func workItemByAttempt(ctx context.Context, q store.Querier, attemptID int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM dev_work_items WHERE claim_attempt_id=$1`, attemptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// requestRepoID 返回任务所属请求的仓库。
func requestRepoID(ctx context.Context, q store.Querier, requestID int64) (int64, error) {
	var id int64
	if err := q.QueryRow(ctx, `SELECT repo_id FROM ai_requests WHERE id=$1`, requestID).Scan(&id); err != nil {
		return 0, fmt.Errorf("读取 AI 请求 #%d 的仓库：%w", requestID, err)
	}
	return id, nil
}

// devRequestBinding 是 ai_dev_requests 中与文档依据有关的权威记录：请求已经绑定的工作项（没有绑定时为 0）与开发方式。
type devRequestBinding struct {
	WorkItemID int64
	Mode       string
}

// loadDevRequestBinding 读取开发请求的权威关联。
func loadDevRequestBinding(ctx context.Context, q store.Querier, requestID int64) (devRequestBinding, error) {
	var (
		b        devRequestBinding
		workItem *int64
	)
	if err := q.QueryRow(ctx, `SELECT work_item_id, mode FROM ai_dev_requests WHERE request_id=$1`, requestID).
		Scan(&workItem, &b.Mode); err != nil {
		return devRequestBinding{}, fmt.Errorf("读取 AI 开发请求 #%d 的工作项：%w", requestID, err)
	}
	b.WorkItemID = deref64(workItem)
	return b, nil
}

// devTaskWorkItem 按权威记录核对开发任务的工作项，返回任务实际可以读取的工作项（0 表示尚未领取 / 没有依据）。
//
// ai_dev_requests.work_item_id 是权威记录：请求已经绑定工作项时，任务 spec 必须与它一致（缺失也算不一致）。
// 只有真正合法的选题任务（mode=select 且请求还没有绑定工作项）在领取前不知道工作项，才允许按 claim_attempt_id
// 回退；其他方式缺失工作项、spec 与领取到的（或缺失的）工作项不一致，都是冲突——绝不能凭 spec 去读无关的工作项。
func (c *Core) devTaskWorkItem(ctx context.Context, q store.Querier, t *Task, spec aispec.DevelopSpec,
	binding devRequestBinding, attemptID int64,
) (int64, error) {
	if binding.WorkItemID != 0 {
		if spec.WorkItemID != binding.WorkItemID {
			return 0, fmt.Errorf("%w：AI 任务 #%d 的工作项与开发请求 #%d 的记录不一致",
				ErrDocsBasisConflict, t.ID, t.RequestID)
		}
		return binding.WorkItemID, nil
	}
	// 请求还没有绑定工作项：只有真正的选题任务（mode=select）可以在领取后按 attempt 回退，
	// 其余方式（implement / fix）缺失工作项本身就是坏记录，不能凭 spec 去读无关工作项。
	if binding.Mode != agentcfg.DevModeSelect {
		return 0, fmt.Errorf("%w：AI 任务 #%d 不是选题任务，开发请求却没有记录工作项", ErrDocsBasisConflict, t.ID)
	}
	claimed, err := workItemByAttempt(ctx, q, attemptID)
	if err != nil {
		return 0, err
	}
	if spec.WorkItemID != 0 && spec.WorkItemID != claimed {
		return 0, fmt.Errorf("%w：AI 任务 #%d 的工作项与该任务领取到的工作项不一致", ErrDocsBasisConflict, t.ID)
	}
	return claimed, nil
}

// devTaskBasis 核对工作项属于任务所在的仓库后，返回它的冻结快照依据（没有冻结时 Source=none）。
func (c *Core) devTaskBasis(ctx context.Context, q store.Querier, t *Task, repoID, workItemID int64) (docsync.Basis, error) {
	var gotRepo int64
	err := q.QueryRow(ctx, `SELECT repo_id FROM dev_work_items WHERE id=$1`, workItemID).Scan(&gotRepo)
	if errors.Is(err, pgx.ErrNoRows) {
		return docsync.Basis{}, fmt.Errorf("%w：AI 任务 #%d 引用的工作项 #%d 不存在", ErrDocsBasisConflict, t.ID, workItemID)
	}
	if err != nil {
		return docsync.Basis{}, err
	}
	if gotRepo != repoID {
		return docsync.Basis{}, fmt.Errorf("%w：AI 任务 #%d 引用的工作项 #%d 不属于请求的仓库",
			ErrDocsBasisConflict, t.ID, workItemID)
	}
	return c.docs.WorkItemBasis(ctx, q, repoID, workItemID)
}

// taskDocsBasis 返回这个任务可以读取的文档依据。审计任务以数据库关系为权威（ai_requests + ai_review_requests），
// 并要求任务 spec 与数据库一致；开发任务以 ai_dev_requests.work_item_id 为权威（合法的选题任务在领取后按
// claim_attempt_id 回退），核对工作项属于任务仓库后读取工作项快照。
//
// 审计任务绝不按 claim_attempt_id 回退，也不允许"工作项优先"掩盖双引用或坏 spec：任何不一致都返回错误。
func (c *Core) taskDocsBasis(ctx context.Context, q store.Querier, t *Task, attemptID int64) (docsync.Basis, error) {
	if t.Type == agentcfg.TaskReview {
		return c.reviewDocsBasis(ctx, q, t)
	}
	repoID, err := requestRepoID(ctx, q, t.RequestID)
	if err != nil {
		return docsync.Basis{}, err
	}
	var spec aispec.DevelopSpec
	if err := t.DecodeSpec(&spec); err != nil {
		return docsync.Basis{}, err
	}
	binding, err := loadDevRequestBinding(ctx, q, t.RequestID)
	if err != nil {
		return docsync.Basis{}, err
	}
	wi, err := c.devTaskWorkItem(ctx, q, t, spec, binding, attemptID)
	if err != nil {
		return docsync.Basis{}, err
	}
	if wi == 0 {
		return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, nil
	}
	return c.devTaskBasis(ctx, q, t, repoID, wi)
}

// reviewDocsBasis 按审计请求的明细与任务 spec 决定可读的文档依据：snapshot_set 读独立快照集，work_item 读工作项快照，
// none / unplanned 一律没有文档（即使工作项后来有了快照也不放行），legacy 只有在 spec 的引用与工作项快照逐篇一致时
// 才允许读取。
func (c *Core) reviewDocsBasis(ctx context.Context, q store.Querier, t *Task) (docsync.Basis, error) {
	var (
		repoID   int64
		source   string
		workItem *int64
		setID    *int64
		digest   string
	)
	err := q.QueryRow(ctx, `SELECT r.repo_id, d.docs_source, d.work_item_id, d.doc_snapshot_set_id, d.docs_manifest_digest
		FROM ai_tasks t JOIN ai_requests r ON r.id=t.request_id JOIN ai_review_requests d ON d.request_id=r.id
		WHERE t.id=$1`, t.ID).Scan(&repoID, &source, &workItem, &setID, &digest)
	if err != nil {
		return docsync.Basis{}, fmt.Errorf("读取 AI 任务 #%d 的文档依据：%w", t.ID, err)
	}
	var spec aispec.ReviewSpec
	if err := t.DecodeSpec(&spec); err != nil {
		return docsync.Basis{}, err
	}
	wi, sid := deref64(workItem), deref64(setID)
	migratedBasis, migrated, err := c.reviewSpecBasis(ctx, q, t, spec, source, repoID, wi, sid, digest)
	if err != nil {
		return docsync.Basis{}, err
	}
	if migrated {
		return migratedBasis, nil
	}
	switch source {
	case docsync.SourceSnapshotSet:
		var gotRepo int64
		if err := q.QueryRow(ctx, `SELECT repo_id FROM doc_snapshot_sets WHERE id=$1`, sid).Scan(&gotRepo); err != nil {
			return docsync.Basis{}, fmt.Errorf("读取文档快照集 #%d：%w", sid, err)
		}
		if gotRepo != repoID {
			return docsync.Basis{}, fmt.Errorf("%w：文档快照集 #%d 不属于请求的仓库", ErrDocsBasisConflict, sid)
		}
		return docsync.Basis{Source: docsync.SourceSnapshotSet, RepoID: repoID, SnapshotSetID: sid, ManifestDigest: digest}, nil
	case docsync.SourceWorkItem:
		basis, err := c.docs.WorkItemBasis(ctx, q, repoID, wi)
		if err != nil {
			return docsync.Basis{}, err
		}
		if basis.Source != docsync.SourceWorkItem {
			return docsync.Basis{}, fmt.Errorf("%w：AI 任务 #%d 引用的工作项快照不存在", ErrDocsBasisConflict, t.ID)
		}
		return basis, nil
	case docsync.SourceNone, docsync.SourceUnplanned:
		return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, nil
	case docsync.SourceLegacy:
		if wi == 0 || len(spec.Docs) == 0 {
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, nil
		}
		basis, err := c.docs.WorkItemBasis(ctx, q, repoID, wi)
		if err != nil {
			return docsync.Basis{}, err
		}
		if basis.Source != docsync.SourceWorkItem || !docsync.SameDocRefs(spec.Docs, basis.Docs) {
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, nil
		}
		return basis, nil
	}
	return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, nil
}

// reviewSpecBasis 核对任务 spec 与请求明细。迁移前（legacy）的任务 spec 没有来源 / 快照集 / 摘要字段，只要求
// 没有显式声明别的来源；迁移前的族根可能在重跑时被固化成 work_item / none，此时旧任务的 spec 仍是迁移前格式，
// 按任务记录中的引用核对固化的依据（migrated=true 时返回这份依据）。其他任何不一致都拒绝，不用"工作项优先"掩盖坏 spec。
func (c *Core) reviewSpecBasis(ctx context.Context, q store.Querier, t *Task, spec aispec.ReviewSpec,
	source string, repoID, wi, sid int64, digest string,
) (docsync.Basis, bool, error) {
	migrated := migratedLegacyTask(spec, source, wi)
	switch {
	case source == docsync.SourceLegacy:
		// 迁移前的任务 spec 没有来源字段，但仍记录工作项：必须与请求明细指向同一个工作项（缺字段也不行），
		// 否则就可能把任务记录的引用对到别的工作项快照上。
		if spec.DocsSource != "" || spec.DocSnapshotSetID != 0 || spec.WorkItemID != wi {
			return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 的文档依据与请求明细不一致", ErrDocsBasisConflict, t.ID)
		}
	case migrated:
		// 迁移前的合法固化态：不比对 spec 里不存在的来源 / 摘要字段，按下面的引用核对。
	case spec.DocsSource != source || spec.DocSnapshotSetID != sid || spec.WorkItemID != wi:
		return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 的文档依据与请求明细不一致", ErrDocsBasisConflict, t.ID)
	}
	if !migrated {
		if source != docsync.SourceLegacy && spec.DocsManifestDigest != digest {
			return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 的文档依据摘要与请求明细不一致", ErrDocsBasisConflict, t.ID)
		}
		return docsync.Basis{}, false, nil
	}
	switch source {
	case docsync.SourceWorkItem:
		// 族根固化成 work_item：旧任务必须逐篇引用工作项快照，否则仍然是不一致。
		if len(spec.Docs) == 0 || wi == 0 {
			return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 的文档依据与请求明细不一致", ErrDocsBasisConflict, t.ID)
		}
		basis, err := c.docs.WorkItemBasis(ctx, q, repoID, wi)
		if err != nil {
			return docsync.Basis{}, false, err
		}
		if basis.Source != docsync.SourceWorkItem || !docsync.SameDocRefs(spec.Docs, basis.Docs) {
			return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 的文档引用与固化的依据不一致", ErrDocsBasisConflict, t.ID)
		}
		return basis, true, nil
	case docsync.SourceNone:
		// 族根固化成 none：旧任务不能记录任何文档引用。
		if len(spec.Docs) != 0 {
			return docsync.Basis{}, false, fmt.Errorf("%w：AI 任务 #%d 记录了文档引用，与固化的依据不一致", ErrDocsBasisConflict, t.ID)
		}
		return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, true, nil
	}
	return docsync.Basis{}, false, nil
}

func deref64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// migratedLegacyTask 判断这是否是"迁移前规划、族根后来在重跑时固化"的合法历史任务：请求明细已经固化为
// work_item / none，但任务 spec 仍是迁移前格式（没有来源、快照集与整批摘要字段），并且 spec 指向同一个工作项。
// 固化只发生在有工作项的请求族（review/rerun.go 的 legacyFamily），所以 wi 必须非零。
func migratedLegacyTask(spec aispec.ReviewSpec, source string, workItemID int64) bool {
	if spec.DocsSource != "" || spec.DocSnapshotSetID != 0 || spec.DocsManifestDigest != "" {
		return false
	}
	if workItemID == 0 || spec.WorkItemID != workItemID {
		return false
	}
	return source == docsync.SourceWorkItem || source == docsync.SourceNone
}

// ListTaskDocs 列出这个任务的文档依据（标题、修订、摘要，不含正文）。只读；与其它 attempt 级调用一样校验节点、epoch、
// 任务状态和租约。没有文档依据（没有绑定、工作项没有冻结、none / unplanned）时返回空。
func (s *Service) ListTaskDocs(ctx context.Context, req *connect.Request[airunnerv2.ListTaskDocsRequest]) (*connect.Response[airunnerv2.ListTaskDocsResponse], error) {
	runnerID, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	c := s.c
	_, t, err := c.attemptTask(ctx, c.db, runnerID, req.Msg.GetRef(), false)
	if err != nil {
		return nil, err
	}
	resp := &airunnerv2.ListTaskDocsResponse{}
	if c.docs == nil {
		return connect.NewResponse(resp), nil
	}
	basis, err := c.taskDocsBasis(ctx, c.db, t, req.Msg.GetRef().GetAttemptId())
	if err != nil {
		return nil, docsBasisError(err)
	}
	docs, err := c.docs.ListBasis(ctx, c.db, basis)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		resp.Docs = append(resp.Docs, &airunnerv2.TaskDocRef{
			Id: d.DocID, Title: d.Title, Revision: d.Revision, Sha256: d.SHA256, Url: d.URL,
		})
	}
	return connect.NewResponse(resp), nil
}

// GetTaskDoc 返回这个任务一篇文档的正文（冻结时的内容）。文档不存在时返回 NotFound；正文已过保留期清理时返回
// FailedPrecondition（元数据还能列出，正文不可读）。
func (s *Service) GetTaskDoc(ctx context.Context, req *connect.Request[airunnerv2.GetTaskDocRequest]) (*connect.Response[airunnerv2.GetTaskDocResponse], error) {
	runnerID, err := s.authorize(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	c := s.c
	_, t, err := c.attemptTask(ctx, c.db, runnerID, req.Msg.GetRef(), false)
	if err != nil {
		return nil, err
	}
	docID := req.Msg.GetDocId()
	if docID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("缺少 doc_id"))
	}
	if c.docs == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("这个任务没有设计文档快照"))
	}
	basis, err := c.taskDocsBasis(ctx, c.db, t, req.Msg.GetRef().GetAttemptId())
	if err != nil {
		return nil, docsBasisError(err)
	}
	d, err := c.docs.GetBasis(ctx, c.db, basis, docID)
	switch {
	case errors.Is(err, docsync.ErrSnapshotExpired):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("这篇设计文档的快照正文已过保留期清理：%w", err))
	case err != nil:
		return nil, err
	case d == nil:
		return nil, connect.NewError(connect.CodeNotFound, errors.New("没有这篇设计文档的快照"))
	}
	return connect.NewResponse(&airunnerv2.GetTaskDocResponse{
		Id: d.DocID, Title: d.Title, Revision: d.Revision, Sha256: d.SHA256, Content: d.Content, Url: d.URL,
	}), nil
}

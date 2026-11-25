package ai

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Dashboard 的设计依据视图：把审计请求、AI 任务、AI 开发工作项各自固定的文档依据统一解析成只读的
// 元数据视图。它与 Runner RPC（ListTaskDocs / GetTaskDoc）共用同一套依据解析（taskDocsBasis / reviewDocsBasis），
// 但只读、不需要节点 / epoch / 租约授权。
//
// 规则与 RPC 一致：以数据库关系为权威，绝不按当前仓库绑定补造历史来源；依据记录与快照不一致时明确报冲突，
// 不过度推断。列出的只有元数据，正文按需经单独的读取方法返回。

var (
	// ErrDocsBasisMissing 表示这个对象没有可读的设计依据（没有绑定、没有冻结、none / unplanned / legacy）。
	ErrDocsBasisMissing = errors.New("没有设计依据")
	// ErrDocsBasisConflict 表示依据记录与实际快照不一致（摘要、来源、工作项或任务 spec）。
	ErrDocsBasisConflict = errors.New("设计依据记录不一致")
)

// RequestDocsView 是一个 AI 请求固定的文档依据。DeclaredSource 是数据库里权威记录的来源（审计的 docs_source；
// 开发请求按工作项是否关联记为 work_item / none）。View 是实际可读的快照；两者不一致时 Error 说明原因。
type RequestDocsView struct {
	RequestID      int64
	RepoID         int64
	Type           string
	DeclaredSource string
	WorkItemID     int64
	SnapshotSetID  int64
	DeclaredDigest string
	// Declared 为假表示历史请求没有记录文档依据（迁移前的 legacy）。
	Declared bool
	View     *docsync.SnapshotView
	// Issue 非 nil 表示声明与实际快照不一致（不展示为正常依据）：原因码和参数给页面按语言显示；Error 是它的中文说明。
	Issue *DocsIssue
	Error string
}

// TaskDocsView 是一个 AI 任务实际可读的文档依据。DeclaredSource 是所属请求记录的来源，BasisSource 是依据解析
// （与 Runner RPC 相同）实际给出的来源；两者不同时（例如 legacy 请求重跑时核对并固化的历史依据）如实分别展示。
type TaskDocsView struct {
	TaskID         int64
	RequestID      int64
	Type           string
	DeclaredSource string
	BasisSource    string
	View           *docsync.SnapshotView
	// Error 非空表示任务的 spec 与请求明细不一致，或依据不可核对（中文说明）；Issue 是记录不一致时的原因码和参数，
	// 依据因为其他原因（例如 spec 无法解析）不可核对时为 nil，页面原样显示 Error。
	Issue *DocsIssue
	Error string
}

// RelatedTask 是工作项的一条关联任务及它与工作项冻结依据的关系。
type RelatedTask struct {
	TaskID    int64
	RequestID int64
	Type      string
	// Mode 是开发方式（select / implement / fix）或审计触发方式（pull_request / manual）。
	Mode string
	// Relation 是 same_basis / refs_match / claimed_dynamic / no_basis / unverifiable / legacy_pinned / mismatch。
	Relation string
	// NoteCode 是关系说明的原因码；Note 是它的中文说明（给人看，不含推断，依据无法解析时带上具体原因）。
	NoteCode RelationNote
	Note     string
	// 依据无法解析时（NoteCode 为 basis_conflict / basis_unverifiable）的具体原因：记录不一致时 Issue 是原因码和参数，
	// 其他原因时为 nil，Detail 是错误的原文。
	Issue  *DocsIssue
	Detail string
}

// setIssue 记录请求的依据与实际快照不一致的原因（Error 是它的中文说明，不带前缀）。
func (v *RequestDocsView) setIssue(e *DocsIssue) { v.Issue, v.Error = e, e.Detail() }

// setIssue 记录任务的依据不可核对的原因（Error 是它的中文说明，不带前缀）。
func (v *TaskDocsView) setIssue(e *DocsIssue) { v.Issue, v.Error = e, e.Detail() }

// WorkItemDocsView 返回工作项冻结快照的展示数据；没有冻结时 Source=none。
func (c *Core) WorkItemDocsView(ctx context.Context, workItemID int64) (*docsync.SnapshotView, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	repoID, err := workItemRepo(ctx, c.db, workItemID)
	if err != nil {
		return nil, err
	}
	return c.docs.WorkItemSnapshotView(ctx, c.db, repoID, workItemID)
}

// RequestDocsView 返回一个 AI 请求固定的文档依据。
func (c *Core) RequestDocsView(ctx context.Context, requestID int64) (*RequestDocsView, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	basis, v, err := c.requestDocsBasis(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if v.View, err = c.docsViewForBasis(ctx, basis); err != nil {
		return nil, err
	}
	return v, nil
}

// TaskDocsView 返回一个 AI 任务实际可读的文档依据（与 Runner RPC 共用 taskDocsBasis）。
func (c *Core) TaskDocsView(ctx context.Context, taskID int64) (*TaskDocsView, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	t, err := GetTask(ctx, c.db, taskID, false)
	if err != nil {
		return nil, err
	}
	attemptID, err := firstAttemptID(ctx, c.db, taskID)
	if err != nil {
		return nil, err
	}
	basis, basisErr := c.taskDocsBasis(ctx, c.db, t, attemptID)
	v := &TaskDocsView{TaskID: taskID, RequestID: t.RequestID, Type: t.Type, BasisSource: basis.Source}
	declared, err := c.declaredSource(ctx, t.RequestID)
	if err != nil {
		return nil, err
	}
	v.DeclaredSource = declared
	if basisErr != nil {
		v.Error = basisErr.Error()
		v.Issue, _ = errors.AsType[*DocsIssue](basisErr)
		if v.View, err = c.emptyDocsView(ctx, t.RequestID); err != nil {
			return nil, err
		}
		return v, nil
	}
	if v.View, err = c.docsViewForBasis(ctx, basis); err != nil {
		return nil, err
	}
	if basis.Source != docsync.SourceNone && !validBasisDigest(basis.ManifestDigest) {
		v.setIssue(&DocsIssue{Code: IssueBasisDigestInvalid})
	}
	return v, nil
}

// WorkItemDocContent 读取工作项快照中一篇文档的正文：没有依据时返回 ErrDocsBasisMissing，不存在时返回 nil，
// 正文已清理时返回 docsync.ErrSnapshotExpired。
func (c *Core) WorkItemDocContent(ctx context.Context, workItemID int64, docID string) (*docsync.Snapshot, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	repoID, err := workItemRepo(ctx, c.db, workItemID)
	if err != nil {
		return nil, err
	}
	basis, err := c.docs.WorkItemBasis(ctx, c.db, repoID, workItemID)
	if err != nil {
		return nil, err
	}
	if basis.Source == docsync.SourceNone {
		return nil, ErrDocsBasisMissing
	}
	return c.docs.GetBasis(ctx, c.db, basis, docID)
}

// RequestDocContent 读取一个 AI 请求依据中一篇文档的正文。
func (c *Core) RequestDocContent(ctx context.Context, requestID int64, docID string) (*docsync.Snapshot, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	basis, v, err := c.requestDocsBasis(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if v.Issue != nil {
		return nil, v.Issue
	}
	if basis.Source == docsync.SourceNone {
		return nil, ErrDocsBasisMissing
	}
	return c.docs.GetBasis(ctx, c.db, basis, docID)
}

// TaskDocContent 读取一个 AI 任务依据中一篇文档的正文。
func (c *Core) TaskDocContent(ctx context.Context, taskID int64, docID string) (*docsync.Snapshot, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	t, err := GetTask(ctx, c.db, taskID, false)
	if err != nil {
		return nil, err
	}
	attemptID, err := firstAttemptID(ctx, c.db, taskID)
	if err != nil {
		return nil, err
	}
	basis, err := c.taskDocsBasis(ctx, c.db, t, attemptID)
	if err != nil {
		// taskDocsBasis 已经把依据不一致标为 ErrDocsBasisConflict；其他错误（数据库等）原样返回，不能伪装成冲突。
		return nil, err
	}
	if basis.Source == docsync.SourceNone {
		return nil, ErrDocsBasisMissing
	}
	return c.docs.GetBasis(ctx, c.db, basis, docID)
}

// RelatedTasks 返回与工作项关联的全部 AI 任务（选题 / 开发 / 返修 / 审计），并如实给出每个任务与工作项冻结依据的关系。
func (c *Core) RelatedTasks(ctx context.Context, workItemID int64) ([]RelatedTask, error) {
	if c.docs == nil {
		return nil, ErrDocsBasisMissing
	}
	repoID, err := workItemRepo(ctx, c.db, workItemID)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.Query(ctx, `SELECT DISTINCT t.id, t.request_id, t.type, t.spec
		FROM ai_tasks t JOIN ai_requests r ON r.id=t.request_id
		WHERE r.repo_id=$2 AND t.id IN (
			SELECT t2.id FROM ai_tasks t2 JOIN ai_dev_requests d ON d.request_id=t2.request_id WHERE d.work_item_id=$1
			UNION
			SELECT t2.id FROM ai_tasks t2 JOIN ai_review_requests v ON v.request_id=t2.request_id WHERE v.work_item_id=$1
			UNION
			SELECT a.task_id FROM ai_attempts a JOIN dev_work_items w ON w.claim_attempt_id=a.id WHERE w.id=$1
		) ORDER BY t.id`, workItemID, repoID)
	if err != nil {
		return nil, err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (relatedTaskRow, error) {
		var r relatedTaskRow
		err := row.Scan(&r.ID, &r.RequestID, &r.Type, &r.Spec)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	wiBasis, err := c.docs.WorkItemBasis(ctx, c.db, repoID, workItemID)
	if err != nil {
		return nil, err
	}
	// 首个 attempt 与请求来源各批量预取一次，不在循环里逐个任务查询。
	taskIDs := make([]int64, len(tasks))
	requestIDs := make([]int64, len(tasks))
	for i, r := range tasks {
		taskIDs[i], requestIDs[i] = r.ID, r.RequestID
	}
	attempts, err := firstAttemptIDs(ctx, c.db, taskIDs)
	if err != nil {
		return nil, err
	}
	declared, err := c.declaredSources(ctx, requestIDs)
	if err != nil {
		return nil, err
	}
	out := make([]RelatedTask, 0, len(tasks))
	for _, r := range tasks {
		v := RelatedTask{TaskID: r.ID, RequestID: r.RequestID, Type: r.Type}
		var spec specDocs
		_ = json.Unmarshal(r.Spec, &spec) // 解析失败时按"没有文档引用"处理，关系会落到 unverifiable
		v.Mode = spec.displayMode(r.Type)
		attemptID := attempts[r.ID]
		basis, basisErr := c.taskDocsBasis(ctx, c.db, &Task{ID: r.ID, RequestID: r.RequestID, Type: r.Type, Spec: r.Spec}, attemptID)
		v.Relation, v.NoteCode = classifyRelation(r.Type, declared[r.RequestID], spec, basis, basisErr, wiBasis, repoID, workItemID)
		v.Note = v.NoteCode.String()
		if basisErr != nil {
			v.Detail = basisErr.Error()
			v.Issue, _ = errors.AsType[*DocsIssue](basisErr)
			v.Note += "：" + v.Detail
		}
		out = append(out, v)
	}
	return out, nil
}

// firstAttemptIDs 批量返回每个任务的第一次 attempt（没有 attempt 的任务不在结果中，取用时得到 0）。
func firstAttemptIDs(ctx context.Context, q store.Querier, taskIDs []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT task_id, min(id) FROM ai_attempts WHERE task_id = ANY($1) GROUP BY task_id`, taskIDs)
	if err != nil {
		return nil, err
	}
	var id, attempt int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &attempt}, func() error {
		out[id] = attempt
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// declaredSources 批量返回请求的文档来源（规则与 declaredSource 相同）：开发请求按工作项关联给出 work_item / none，
// 审计请求给出 docs_source；请求不存在或没有对应明细时为空。
func (c *Core) declaredSources(ctx context.Context, requestIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(requestIDs))
	if len(requestIDs) == 0 {
		return out, nil
	}
	types := map[int64]string{}
	rows, err := c.db.Query(ctx, `SELECT id, type FROM ai_requests WHERE id = ANY($1)`, requestIDs)
	if err != nil {
		return nil, err
	}
	var id int64
	var typ string
	if _, err := pgx.ForEachRow(rows, []any{&id, &typ}, func() error {
		types[id] = typ
		return nil
	}); err != nil {
		return nil, err
	}
	devItems := map[int64]bool{}
	rows, err = c.db.Query(ctx, `SELECT request_id FROM ai_dev_requests WHERE request_id = ANY($1) AND work_item_id IS NOT NULL`, requestIDs)
	if err != nil {
		return nil, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&id}, func() error {
		devItems[id] = true
		return nil
	}); err != nil {
		return nil, err
	}
	reviewSources := map[int64]string{}
	rows, err = c.db.Query(ctx, `SELECT request_id, docs_source FROM ai_review_requests WHERE request_id = ANY($1)`, requestIDs)
	if err != nil {
		return nil, err
	}
	var source string
	if _, err := pgx.ForEachRow(rows, []any{&id, &source}, func() error {
		reviewSources[id] = source
		return nil
	}); err != nil {
		return nil, err
	}
	for reqID, typ := range types {
		if typ == agentcfg.TaskDevelop {
			if devItems[reqID] {
				out[reqID] = docsync.SourceWorkItem
			} else {
				out[reqID] = docsync.SourceNone
			}
		} else {
			out[reqID] = reviewSources[reqID]
		}
	}
	return out, nil
}

// docsViewForBasis 把依据解析成展示视图。
func (c *Core) docsViewForBasis(ctx context.Context, b docsync.Basis) (*docsync.SnapshotView, error) {
	switch b.Source {
	case docsync.SourceSnapshotSet:
		v, err := c.docs.SnapshotSetView(ctx, c.db, b.RepoID, b.SnapshotSetID)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, &DocsIssue{Code: IssueSnapshotSetMissing, SnapshotSetID: b.SnapshotSetID}
		}
		if b.ManifestDigest != "" && v.ManifestDigest != b.ManifestDigest {
			return nil, &DocsIssue{Code: IssueSnapshotSetDigest}
		}
		return v, nil
	case docsync.SourceWorkItem:
		return c.docs.WorkItemSnapshotView(ctx, c.db, b.RepoID, b.WorkItemID)
	}
	return c.emptyDocsView(ctx, 0)
}

// emptyDocsView 返回一个"没有可读依据"的视图（保留仓库与请求信息）。
func (c *Core) emptyDocsView(ctx context.Context, requestID int64) (*docsync.SnapshotView, error) {
	v := &docsync.SnapshotView{Source: docsync.SourceNone, Docs: []docsync.DocMeta{}}
	if requestID != 0 {
		if repoID, err := requestRepoID(ctx, c.db, requestID); err == nil {
			v.RepoID = repoID
		}
	}
	return v, nil
}

// requestDocsBasis 按请求类型解析固定的文档依据，并核对声明与实际快照。返回的 RequestDocsView 总是非 nil。
func (c *Core) requestDocsBasis(ctx context.Context, requestID int64) (docsync.Basis, *RequestDocsView, error) {
	var (
		repoID int64
		typ    string
	)
	if err := c.db.QueryRow(ctx, `SELECT repo_id, type FROM ai_requests WHERE id=$1`, requestID).Scan(&repoID, &typ); err != nil {
		return docsync.Basis{}, nil, err
	}
	v := &RequestDocsView{RequestID: requestID, RepoID: repoID, Type: typ, Declared: true}
	if typ == agentcfg.TaskDevelop {
		var workItem *int64
		err := c.db.QueryRow(ctx, `SELECT work_item_id FROM ai_dev_requests WHERE request_id=$1`, requestID).Scan(&workItem)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return docsync.Basis{}, nil, err
		}
		if workItem == nil {
			v.DeclaredSource = docsync.SourceNone
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		v.DeclaredSource, v.WorkItemID = docsync.SourceWorkItem, *workItem
		basis, err := c.docs.WorkItemBasis(ctx, c.db, repoID, *workItem)
		if err != nil {
			return docsync.Basis{}, nil, err
		}
		return basis, v, nil
	}

	var (
		source   string
		workItem *int64
		setID    *int64
		digest   string
	)
	if err := c.db.QueryRow(ctx, `SELECT docs_source, work_item_id, doc_snapshot_set_id, docs_manifest_digest
		FROM ai_review_requests WHERE request_id=$1`, requestID).Scan(&source, &workItem, &setID, &digest); err != nil {
		return docsync.Basis{}, nil, err
	}
	v.DeclaredSource, v.WorkItemID, v.SnapshotSetID, v.DeclaredDigest = source, deref64(workItem), deref64(setID), digest
	v.Declared = source != docsync.SourceLegacy
	switch source {
	case docsync.SourceSnapshotSet:
		var gotRepo int64
		var manifest string
		err := c.db.QueryRow(ctx, `SELECT repo_id, manifest_digest FROM doc_snapshot_sets WHERE id=$1`, v.SnapshotSetID).
			Scan(&gotRepo, &manifest)
		if errors.Is(err, pgx.ErrNoRows) {
			v.setIssue(&DocsIssue{Code: IssueSnapshotSetMissing, SnapshotSetID: v.SnapshotSetID})
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		if err != nil {
			return docsync.Basis{}, nil, err
		}
		if gotRepo != repoID {
			v.setIssue(&DocsIssue{Code: IssueSnapshotSetOtherRepo, SnapshotSetID: v.SnapshotSetID})
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		if digest == "" || digest != manifest {
			v.setIssue(&DocsIssue{Code: IssueRequestSetDigest})
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		return docsync.Basis{Source: docsync.SourceSnapshotSet, RepoID: repoID, SnapshotSetID: v.SnapshotSetID, ManifestDigest: digest}, v, nil
	case docsync.SourceWorkItem:
		basis, err := c.docs.WorkItemBasis(ctx, c.db, repoID, v.WorkItemID)
		if err != nil {
			return docsync.Basis{}, nil, err
		}
		if basis.Source != docsync.SourceWorkItem {
			v.setIssue(&DocsIssue{Code: IssueRequestWorkItemSnapshot})
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		if digest == "" || digest != basis.ManifestDigest {
			v.setIssue(&DocsIssue{Code: IssueRequestWorkItemDigest})
			return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
		}
		return basis, v, nil
	case docsync.SourceNone, docsync.SourceUnplanned, docsync.SourceLegacy:
		return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
	}
	v.setIssue(&DocsIssue{Code: IssueRequestSourceUnknown})
	return docsync.Basis{Source: docsync.SourceNone, RepoID: repoID}, v, nil
}

// declaredSource 返回请求记录的来源（开发请求按工作项关联给出 work_item / none）。
func (c *Core) declaredSource(ctx context.Context, requestID int64) (string, error) {
	var typ string
	if err := c.db.QueryRow(ctx, `SELECT type FROM ai_requests WHERE id=$1`, requestID).Scan(&typ); err != nil {
		return "", err
	}
	if typ == agentcfg.TaskDevelop {
		var one int
		err := c.db.QueryRow(ctx, `SELECT 1 FROM ai_dev_requests WHERE request_id=$1 AND work_item_id IS NOT NULL`, requestID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return docsync.SourceNone, nil
		}
		if err != nil {
			return "", err
		}
		return docsync.SourceWorkItem, nil
	}
	var source string
	if err := c.db.QueryRow(ctx, `SELECT docs_source FROM ai_review_requests WHERE request_id=$1`, requestID).Scan(&source); err != nil {
		return "", err
	}
	return source, nil
}

// specDocs 只取任务 spec 中与文档依据有关的字段（开发与审计的字段名相同）。
type specDocs struct {
	Mode string          `json:"mode"`
	Docs []aispec.DocRef `json:"docs"`
	// 审计任务的 spec 还记录来源、快照集与整批摘要；迁移前的任务没有这些字段。
	DocsSource         string `json:"docs_source"`
	DocSnapshotSetID   int64  `json:"doc_snapshot_set_id"`
	DocsManifestDigest string `json:"docs_manifest_digest"`
	WorkItemID         int64  `json:"work_item_id"`
}

// preMigrationReview 判断审计任务是否属于迁移前格式（没有记录来源、快照集与整批摘要）。
// 开发任务的 spec 本来就不记录这些字段，不能据此判断。
func (s specDocs) preMigrationReview(taskType string) bool {
	return taskType == agentcfg.TaskReview && s.DocsSource == "" && s.DocSnapshotSetID == 0 && s.DocsManifestDigest == ""
}

// displayMode 返回任务的显示方式。
func (s specDocs) displayMode(taskType string) string {
	if taskType == agentcfg.TaskDevelop {
		return s.Mode
	}
	return ""
}

// relatedTaskRow 是关联任务查询读到的一行。
type relatedTaskRow struct {
	ID        int64
	RequestID int64
	Type      string
	Spec      []byte
}

// firstAttemptID 返回任务的第一次 attempt（选题任务的领取 attempt）；没有 attempt 时返回 0。
func firstAttemptID(ctx context.Context, q store.Querier, taskID int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT coalesce(min(id), 0) FROM ai_attempts WHERE task_id=$1`, taskID).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// workItemRepo 返回工作项所属的仓库。
func workItemRepo(ctx context.Context, q store.Querier, workItemID int64) (int64, error) {
	var id int64
	if err := q.QueryRow(ctx, `SELECT repo_id FROM dev_work_items WHERE id=$1`, workItemID).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// classifyRelation 按权威记录与任务 spec 给出关系分类，绝不过度推断。workItemID / repoID 是当前页面上的工作项：
// 任务解析出的依据指向别的工作项或仓库时，绝不拿它与当前工作项比较。
func classifyRelation(taskType, declared string, spec specDocs, basis docsync.Basis, basisErr error,
	wiBasis docsync.Basis, repoID, workItemID int64,
) (string, RelationNote) {
	refs := spec.Docs
	if basisErr != nil {
		// 依据记录本身不一致（工作项、来源或摘要对不上）是明确的冲突，不是"无法核对"。
		if errors.Is(basisErr, ErrDocsBasisConflict) {
			return "mismatch", NoteBasisConflict
		}
		return "unverifiable", NoteBasisUnverifiable
	}
	// 任务解析出的依据指向另一个仓库 / 工作项：不能与当前工作项的依据比较（哪怕文档引用碰巧一致）。
	if basis.Source == docsync.SourceWorkItem && (basis.RepoID != repoID || basis.WorkItemID != workItemID) {
		return "mismatch", NoteOtherWorkItem
	}
	// 迁移前规划、族根后来在重跑时固化：旧任务 spec 保留迁移前格式，按任务记录的引用核对固化的依据。
	if spec.preMigrationReview(taskType) {
		switch declared {
		case docsync.SourceWorkItem:
			if len(refs) > 0 && basis.Source == docsync.SourceWorkItem && docsync.SameDocRefs(refs, basis.Docs) {
				return "legacy_pinned", NoteLegacyDigestPinned
			}
			return "mismatch", NoteLegacyRefsMismatch
		case docsync.SourceNone:
			if len(refs) == 0 {
				return "no_basis", NoteLegacyNoneOK
			}
			return "mismatch", NoteLegacyNoneRefs
		}
	}
	// 选题任务在领取前不知道工作项：领取之后按 claim_attempt_id 关联到工作项快照，spec 里没有文档引用。
	// 这是开发任务特有的"领取后关联"，与审计请求的 none 不同；依据必须指向当前工作项（上面已核对）。
	if taskType == agentcfg.TaskDevelop && len(refs) == 0 && basis.Source == docsync.SourceWorkItem && basis.WorkItemID != 0 {
		return "claimed_dynamic", NoteClaimedDynamic
	}
	switch declared {
	case docsync.SourceLegacy:
		if basis.Source == docsync.SourceWorkItem && len(refs) > 0 && docsync.SameDocRefs(refs, basis.Docs) {
			return "legacy_pinned", NoteLegacyPinned
		}
		if basis.Source == docsync.SourceNone {
			return "no_basis", NoteLegacyNoBasis
		}
		return "unverifiable", NoteLegacyInsufficient
	case docsync.SourceNone, docsync.SourceUnplanned:
		return "no_basis", NoteDeclaredNone
	}
	switch basis.Source {
	case docsync.SourceWorkItem:
		return classifyWorkItemRelation(spec, refs, wiBasis)
	case docsync.SourceSnapshotSet:
		return classifySetRelation(spec, refs, basis)
	case docsync.SourceNone:
		return "no_basis", NoteDeclaredNone
	}
	return "unverifiable", NoteUnknown
}

// classifyWorkItemRelation 把任务的文档引用与当前工作项的冻结依据（wiBasis）比较。摘要也一致时才能说"同一份依据"；
// 只有逐篇引用一致、任务没有记录整批摘要时说"文档引用一致"。
func classifyWorkItemRelation(spec specDocs, refs []aispec.DocRef, wiBasis docsync.Basis) (string, RelationNote) {
	if wiBasis.Source != docsync.SourceWorkItem {
		return "unverifiable", NoteWorkItemNoBasis
	}
	digestKnown := spec.DocsManifestDigest != ""
	if len(refs) == 0 {
		if wiBasis.DocCount != 0 {
			return "unverifiable", NoteNoRefs
		}
		if !digestKnown {
			return "refs_match", NoteItemEmptyNoDigest
		}
		if spec.DocsManifestDigest != wiBasis.ManifestDigest {
			return "mismatch", NoteItemEmptyDigest
		}
		return "same_basis", NoteSameEmpty
	}
	if !docsync.SameDocRefs(refs, wiBasis.Docs) {
		return "mismatch", NoteItemRefsMismatch
	}
	if !digestKnown {
		return "refs_match", NoteItemRefsNoDigest
	}
	if spec.DocsManifestDigest != wiBasis.ManifestDigest {
		return "mismatch", NoteItemDigestMismatch
	}
	return "same_basis", NoteSameBasis
}

// classifySetRelation 把任务的文档引用与独立冻结快照集比较。
func classifySetRelation(spec specDocs, refs []aispec.DocRef, basis docsync.Basis) (string, RelationNote) {
	digestKnown := spec.DocsManifestDigest != ""
	if len(refs) == 0 {
		if basis.DocCount != 0 {
			return "unverifiable", NoteNoRefs
		}
		if !digestKnown {
			return "unverifiable", NoteSetEmptyNoDigest
		}
		if spec.DocsManifestDigest != basis.ManifestDigest {
			return "mismatch", NoteSetEmptyDigest
		}
		return "same_basis", NoteSameEmpty
	}
	if !docsync.SameDocRefs(refs, basis.Docs) {
		return "mismatch", NoteSetRefsMismatch
	}
	if !digestKnown {
		return "refs_match", NoteSetRefsNoDigest
	}
	if spec.DocsManifestDigest != basis.ManifestDigest {
		return "mismatch", NoteSetDigestMismatch
	}
	return "same_basis", NoteSameBasis
}

// validBasisDigest 判断摘要是否是 64 位小写 hex。
func validBasisDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

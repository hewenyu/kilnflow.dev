package docsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 页面展示用的只读视图：列出一份冻结依据的来源、冻结状态和每篇文档的元数据。
//
// 这里只有元数据（DocMeta 没有 Content），普通页面 GET 与 5 秒自动刷新都不会携带正文；正文由单独的按需读取接口返回。
// 历史快照没有记录来源（迁移前写入的行）时 SourceKnown 为假，绝不借用当前的 repo_doc_bindings 补造来源。

// DocMeta 是一篇冻结文档的元数据（不含正文）。
type DocMeta struct {
	DocID    string
	Title    string
	Revision int64
	SHA256   string
	URL      string
	// Purged 表示正文已按保留策略清理（或旧清理器留下的空正文）；合法的空文档不是 Purged。
	Purged bool
}

// SnapshotView 是一份冻结依据的展示数据。
type SnapshotView struct {
	// Source 是 work_item / snapshot_set / none。
	Source        string
	RepoID        int64
	WorkItemID    int64
	SnapshotSetID int64
	// Frozen 表示确实存在一份冻结（含空集合）；没有冻结时为假。
	Frozen   bool
	FrozenAt *time.Time
	// Provider / BaseURL / CollectionID / CollectionName / BindingGeneration 是冻结时记录的来源。
	Provider          string
	BaseURL           string
	CollectionID      string
	CollectionName    string
	BindingGeneration int64
	// SourceKnown 为假表示历史快照没有记录来源（迁移前写入），不要用当前绑定补造。
	SourceKnown    bool
	DocCount       int
	ManifestDigest string
	// ContentPurgedAt 是整批正文被清理的时间；没有清理时为 nil。
	ContentPurgedAt *time.Time
	Docs            []DocMeta
	// IntegrityError 非空表示记录的元数据与冻结状态不一致（篇数、摘要或文档内容摘要不合法）。
	IntegrityError string
}

// WorkItemSnapshotView 返回工作项冻结快照的展示数据；没有冻结时 Source=none、Frozen=false。
// 它不读取当前仓库绑定，也不新建冻结。
func (s *Service) WorkItemSnapshotView(ctx context.Context, q store.Querier, repoID, workItemID int64) (*SnapshotView, error) {
	if err := requireWorkItemRepo(ctx, q, repoID, workItemID); err != nil {
		return nil, err
	}
	v := &SnapshotView{Source: SourceNone, RepoID: repoID, WorkItemID: workItemID, Docs: []DocMeta{}}
	var (
		count        int
		frozenAt     time.Time
		stored       *string
		purged       *time.Time
		provider     *string
		baseURL      *string
		collectionID *string
		name         *string
		generation   *int64
	)
	err := q.QueryRow(ctx, `SELECT doc_count, frozen_at, manifest_digest, content_purged_at, provider, base_url,
			collection_id, collection_name, binding_generation
		FROM work_item_doc_freezes WHERE work_item_id=$1`, workItemID).
		Scan(&count, &frozenAt, &stored, &purged, &provider, &baseURL, &collectionID, &name, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	docs, err := s.workItemDocMeta(ctx, q, workItemID)
	if err != nil {
		return nil, err
	}
	v.Source, v.Frozen, v.FrozenAt = SourceWorkItem, true, &frozenAt
	v.DocCount, v.ContentPurgedAt, v.Docs = count, purged, docs
	v.Provider, v.BaseURL = deref(provider), deref(baseURL)
	v.CollectionID, v.CollectionName, v.BindingGeneration = deref(collectionID), deref(name), derefInt(generation)
	v.SourceKnown = v.Provider != "" && v.BaseURL != ""
	v.ManifestDigest = validDigestOr(deref(stored), v)
	v.IntegrityError = checkViewIntegrity(SourceWorkItem, count, v)
	return v, nil
}

// SnapshotSetView 返回独立冻结快照集的展示数据；不存在时返回 nil（不报错），不属于这个仓库时返回错误。
func (s *Service) SnapshotSetView(ctx context.Context, q store.Querier, repoID, setID int64) (*SnapshotView, error) {
	var (
		gotRepo    int64
		provider   string
		baseURL    string
		colID      string
		colName    string
		generation int64
		count      int
		digest     string
		frozenAt   time.Time
		purged     *time.Time
	)
	err := q.QueryRow(ctx, `SELECT repo_id, provider, base_url, collection_id, collection_name, binding_generation,
			doc_count, manifest_digest, frozen_at, content_purged_at
		FROM doc_snapshot_sets WHERE id=$1`, setID).
		Scan(&gotRepo, &provider, &baseURL, &colID, &colName, &generation, &count, &digest, &frozenAt, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if gotRepo != repoID {
		return nil, fmt.Errorf("文档快照集 #%d 不属于仓库 #%d", setID, repoID)
	}
	docs, err := s.setDocMeta(ctx, q, setID)
	if err != nil {
		return nil, err
	}
	v := &SnapshotView{
		Source: SourceSnapshotSet, RepoID: repoID, SnapshotSetID: setID, Frozen: true, FrozenAt: &frozenAt,
		Provider: provider, BaseURL: baseURL, CollectionID: colID, CollectionName: colName,
		BindingGeneration: generation, SourceKnown: provider != "" && baseURL != "", DocCount: count,
		ManifestDigest: digest, ContentPurgedAt: purged, Docs: docs,
	}
	v.IntegrityError = checkViewIntegrity(SourceSnapshotSet, count, v)
	return v, nil
}

// ReadWorkItemDoc 读取工作项快照中一篇文档的正文；不存在时返回 nil，正文已清理时返回 ErrSnapshotExpired。
func (s *Service) ReadWorkItemDoc(ctx context.Context, q store.Querier, repoID, workItemID int64, docID string) (*Snapshot, error) {
	if err := requireWorkItemRepo(ctx, q, repoID, workItemID); err != nil {
		return nil, err
	}
	return s.getWorkItem(ctx, q, workItemID, docID)
}

// ReadSetDoc 读取快照集中一篇文档的正文；不存在时返回 nil，正文已清理时返回 ErrSnapshotExpired。
func (s *Service) ReadSetDoc(ctx context.Context, q store.Querier, repoID, setID int64, docID string) (*Snapshot, error) {
	var gotRepo int64
	err := q.QueryRow(ctx, `SELECT repo_id FROM doc_snapshot_sets WHERE id=$1`, setID).Scan(&gotRepo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if gotRepo != repoID {
		return nil, fmt.Errorf("文档快照集 #%d 不属于仓库 #%d", setID, repoID)
	}
	return s.GetBasis(ctx, q, Basis{Source: SourceSnapshotSet, RepoID: repoID, SnapshotSetID: setID}, docID)
}

// workItemDocMeta 返回工作项快照的文档元数据，purged 由清理标记或"空正文但摘要非空"计算，与 unreadable 的规则一致。
func (s *Service) workItemDocMeta(ctx context.Context, q store.Querier, workItemID int64) ([]DocMeta, error) {
	rows, err := q.Query(ctx, `SELECT s.doc_id, s.title, s.revision, s.content_sha256, s.url,
			(f.content_purged_at IS NOT NULL OR (s.content='' AND s.content_sha256 <> $2)) AS purged
		FROM work_item_doc_snapshots s JOIN work_item_doc_freezes f ON f.work_item_id=s.work_item_id
		WHERE s.work_item_id=$1 ORDER BY s.title, s.doc_id`, workItemID, EmptyContentSHA256)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanDocMeta)
}

// setDocMeta 返回快照集的文档元数据。
func (s *Service) setDocMeta(ctx context.Context, q store.Querier, setID int64) ([]DocMeta, error) {
	rows, err := q.Query(ctx, `SELECT s.doc_id, s.title, s.revision, s.content_sha256, s.url,
			(set.content_purged_at IS NOT NULL OR (s.content='' AND s.content_sha256 <> $2)) AS purged
		FROM doc_snapshot_docs s JOIN doc_snapshot_sets set ON set.id=s.snapshot_set_id
		WHERE s.snapshot_set_id=$1 ORDER BY s.title, s.doc_id`, setID, EmptyContentSHA256)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanDocMeta)
}

func scanDocMeta(row pgx.CollectableRow) (DocMeta, error) {
	var d DocMeta
	err := row.Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.URL, &d.Purged)
	return d, err
}

// validDigestOr 返回存储的摘要。快照集总是记录摘要：缺失或不合法说明记录不可信，原样返回（空串）交给完整性核对报出来，
// 绝不按当前元数据补一个看似正确的摘要；工作项的旧记录没有摘要时按历史元数据确定性计算（来源未知），不回写数据库。
func validDigestOr(stored string, v *SnapshotView) string {
	if validDigest(stored) || v.Source == SourceSnapshotSet {
		return stored
	}
	return ManifestDigest(SourceWorkItem, v.manifestSource(), viewSnapshots(v.Docs))
}

// manifestSource 返回摘要计算用的来源域：展示视图与整批摘要核对共用同一份字段，两处不会算出不同的摘要。
func (v *SnapshotView) manifestSource() ManifestSource {
	return ManifestSource{
		Provider: v.Provider, BaseURL: v.BaseURL, CollectionID: v.CollectionID,
		BindingGeneration: v.BindingGeneration, Unknown: !v.SourceKnown,
	}
}

// checkViewIntegrity 核对记录的篇数、每篇摘要与整批摘要；不一致时返回给人看的说明，绝不自动修复。
// 整批摘要对空集合同样核对：摘要里含来源域和种类，空集合的摘要被改坏同样是记录不一致。
func checkViewIntegrity(kind string, count int, v *SnapshotView) string {
	if len(v.Docs) != count {
		return fmt.Sprintf("记录的篇数（%d）与文档行数（%d）不一致", count, len(v.Docs))
	}
	for _, d := range v.Docs {
		if !validDigest(d.SHA256) {
			return fmt.Sprintf("文档 %s 的内容摘要不合法", d.DocID)
		}
	}
	if !validDigest(v.ManifestDigest) {
		return "文档快照的整批摘要缺失或不合法"
	}
	if want := ManifestDigest(kind, v.manifestSource(), viewSnapshots(v.Docs)); want != v.ManifestDigest {
		return "文档快照的整批摘要与记录的元数据不一致"
	}
	return ""
}

// viewSnapshots 把展示元数据换成摘要计算用的快照（不含正文）。
func viewSnapshots(docs []DocMeta) []Snapshot {
	out := make([]Snapshot, 0, len(docs))
	for _, d := range docs {
		out = append(out, Snapshot{DocID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	return out
}

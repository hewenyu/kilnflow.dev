// Package docsync 把仓库绑定的外部文档集（首期 Outline）冻结成不可变快照，供 AI 开发与审计 Agent 读取。
// 快照可以按工作项冻结一次（开发与审计读到同一份），也可以按审计请求冻结成独立的快照集
// （独立 PR 审计、手动全面审计没有工作项）。仓库 docs 之后再变也不影响在途任务（T4）。
//
// 执行节点不持有外部文档凭据：连接器经 kiln-orca task doc 向 Server 要这份冻结快照（airunner.v2 的 ListTaskDocs /
// GetTaskDoc），Server 只读地返回数据库里的快照内容。
//
// 冻结分两步完成，让"外部拉取"与"数据库提交"可以分别放在正确的位置：
//   - PrepareFreeze 在数据库事务之外读取绑定、拉取整批正文并校验、算好确定性摘要，不落库；
//   - CommitSetTx 只在调用方的事务里执行：重新核对绑定未变，然后写入快照集与文档行。
//
// 这样审计请求、快照集与任务 spec 在同一个事务中原子提交，事务重试时不会留下孤立的快照集。
package docsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Factory 按绑定信息构造 Provider（测试注入进程内的假 Provider）。
type Factory func(baseURL, token string) (docprovider.Provider, error)

const (
	// docFetchConcurrency 是拉取一批文档正文时的并发数：文档集较大或单篇延迟较高时，串行读取很容易超过一次 RPC 的期限。
	docFetchConcurrency = 8
	// FreezeTimeout 是一次冻结拉取（从外部文档系统读取整批正文）的服务端总时限。它**独立于**调用方的 RPC 期限：
	// 首次领取的 Claim RPC 只有约 60 秒，但冻结整批文档可能更久；冻结完成后写入完成标记，调用方超时重试时会直接复用
	// 已完成的结果（不会从头再拉一遍）。审计的同步规划也共享这一份预算，不按阶段重新计时。
	FreezeTimeout = 10 * time.Minute
	// manifestVersion 是文档依据摘要的编码版本：编码有任何变化都要递增，旧摘要自然失效。
	manifestVersion = 1
	// sha256HexLen 是十六进制 SHA-256 的字符数。
	sha256HexLen = 64
)

// 文档依据的来源（ai_review_requests.docs_source / Basis.Source）。
const (
	// SourceSnapshotSet 是审计请求自己冻结的独立快照集。
	SourceSnapshotSet = "snapshot_set"
	// SourceWorkItem 是工作项冻结的快照（开发与审计共用同一份）。
	SourceWorkItem = "work_item"
	// SourceNone 是没有可用的文档依据：没有绑定、绑定空集合的观察结果、或工作项还没有冻结。
	SourceNone = "none"
	// SourceLegacy 是迁移前的审计请求（没有记录文档依据）。
	SourceLegacy = "legacy"
	// SourceUnplanned 是配置错误、没有进入冻结的请求族（没有任务，也没有文档依据）。
	SourceUnplanned = "unplanned"
)

var (
	// ErrBindingChanged 表示拉取期间仓库的文档绑定变了（重绑、解绑、解绑再重绑）：整批作废，不写入。
	ErrBindingChanged = errors.New("仓库的文档绑定在拉取期间发生了变化")
	// ErrSnapshotExpired 表示文档快照的正文已过保留期被清理；元数据还在，正文不可读。
	ErrSnapshotExpired = errors.New("文档快照的正文已过保留期清理")
	// ErrInvalidDocument 表示外部文档不满足冻结条件（ID 不一致、跨集合、未发布 / 已归档 / 已删除、列表重复）。
	ErrInvalidDocument = errors.New("外部文档不满足冻结条件")
)

// IsPermanentError 判断冻结拉取失败是否重试也不会成功、只能由人修正外部文档或绑定：外部文档不满足冻结条件
// （ErrInvalidDocument），或提供方明确拒绝读取（HTTP 401 / 403 / 404 等，docprovider.PermanentReadError）。
// 网络错误、超时、5xx、限流、拉取期间绑定变化、数据库错误都返回 false（临时失败，可以稍后重试）。
func IsPermanentError(err error) bool {
	return errors.Is(err, ErrInvalidDocument) || docprovider.PermanentReadError(err)
}

// Deps 是 Service 的依赖。
type Deps struct {
	DB        *store.DB
	MasterKey []byte
	// Provider 可选；为 nil 时用 Outline 客户端。
	Provider Factory
	// BaseCtx 是服务生命周期 context：取消时立即中断进行中的外部拉取与同步提交（信号退出、HTTP 异常退出、Close）。
	// 为 nil 时只受操作自身的预算约束。
	BaseCtx context.Context
	// Track 可选：登记一次将要进行的同步操作，返回的 done 在操作结束时调用。Server 关闭数据库前等它们结束；
	// 服务已经停止接纳新操作时返回错误，调用方不得再查数据库。
	Track func() (done func(), err error)
}

// Service 冻结、读取工作项与审计请求的外部设计文档快照。
type Service struct {
	db        *store.DB
	masterKey []byte
	provider  Factory
	baseCtx   context.Context
	track     func() (done func(), err error)
	// inflight 按工作项串行化同一进程内的冻结，避免多个重试同时拉取同一文档集、放大外部文档服务的负载；
	// 跨请求 / 跨事务的正确性仍由写事务的工作项锁 + 完成标记保证。条目按使用者计数，最后一个使用者结束后删除。
	// inflightMu 保护 inflight 的增减和删除；acquire / release 都在它下面改计数，删除只发生在计数归零的同一个临界区，
	// 因此新的调用不会拿到与等待者不同的锁。
	inflightMu sync.Mutex
	inflight   map[int64]*workItemLock
}

// workItemLock 是同一进程内按工作项串行化冻结的锁：ch 容量为 1，写入即持有；refs 是持有 + 正在等待的调用数
// （受 inflightMu 保护），为 0 时条目从 inflight 中删除。
type workItemLock struct {
	ch   chan struct{}
	refs int
}

// acquireWorkItemLock 取得工作项的冻结锁：先登记一个使用者，再等锁。等待可以被 ctx 取消（服务退出时立即返回，
// 不占着 Close 的等待）；取消时登记的使用者也一并撤销。返回的 *workItemLock 要交给 releaseWorkItemLock。
func (s *Service) acquireWorkItemLock(ctx context.Context, id int64) (*workItemLock, error) {
	s.inflightMu.Lock()
	lk := s.inflight[id]
	if lk == nil {
		lk = &workItemLock{ch: make(chan struct{}, 1)}
		s.inflight[id] = lk
	}
	lk.refs++
	s.inflightMu.Unlock()
	select {
	case lk.ch <- struct{}{}:
		return lk, nil
	case <-ctx.Done():
		s.dropWorkItemUser(id, lk)
		return nil, ctx.Err()
	}
}

// releaseWorkItemLock 解锁并在没有其他使用者时删除条目。
func (s *Service) releaseWorkItemLock(id int64, lk *workItemLock) {
	<-lk.ch
	s.dropWorkItemUser(id, lk)
}

// dropWorkItemUser 撤销一个使用者（持有者释放或等待者取消），最后一个使用者结束时删除条目。
// 计数与删除在同一个临界区里完成，新调用不会拿到与等待者不同的锁。
func (s *Service) dropWorkItemUser(id int64, lk *workItemLock) {
	s.inflightMu.Lock()
	lk.refs--
	if lk.refs == 0 {
		delete(s.inflight, id)
	}
	s.inflightMu.Unlock()
}

// New 返回 Service。
func New(d Deps) *Service {
	f := d.Provider
	if f == nil {
		f = func(baseURL, token string) (docprovider.Provider, error) {
			return docprovider.NewOutline(docprovider.Config{BaseURL: baseURL, Token: token})
		}
	}
	return &Service{
		db: d.DB, masterKey: d.MasterKey, provider: f, baseCtx: d.BaseCtx, track: d.Track,
		inflight: map[int64]*workItemLock{},
	}
}

// opContext 返回一次外部拉取的 context：不受调用方 RPC 取消影响，总时限 FreezeTimeout，并随服务生命周期取消。
func (s *Service) opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FreezeTimeout)
	var stop func() bool
	if s.baseCtx != nil {
		stop = context.AfterFunc(s.baseCtx, cancel)
	}
	return octx, func() {
		if stop != nil {
			stop()
		}
		cancel()
	}
}

// beginOp 登记一次同步操作；没有登记函数时返回空操作。服务正在退出（门闩已关）时返回错误，调用方不得再查数据库。
func (s *Service) beginOp() (func(), error) {
	if s.track == nil {
		return func() {}, nil
	}
	return s.track()
}

// Snapshot 是一篇冻结的设计文档。列表不返回 Content（避免把整棵树塞进 prompt），读取正文时才带上。
type Snapshot struct {
	DocID    string
	Title    string
	Revision int64
	SHA256   string
	Content  string
	URL      string
}

// List 返回工作项已冻结的文档元数据（不含正文），按标题、doc_id 排序；没有时返回空。
func (s *Service) List(ctx context.Context, workItemID int64) ([]Snapshot, error) {
	rows, err := s.db.Query(ctx, `SELECT doc_id, title, revision, content_sha256, url
		FROM work_item_doc_snapshots WHERE work_item_id=$1 ORDER BY title, doc_id`, workItemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Snapshot, error) {
		var d Snapshot
		err := row.Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.URL)
		return d, err
	})
}

// Get 返回工作项某一篇已冻结文档（含正文）；不存在时返回 nil。正文已被保留期清理时返回 ErrSnapshotExpired。
func (s *Service) Get(ctx context.Context, workItemID int64, docID string) (*Snapshot, error) {
	return s.getWorkItem(ctx, s.db, workItemID, docID)
}

// getWorkItem 读取工作项快照中一篇文档的正文，使用调用方给的查询器：在读事务里调用时读到的是这个事务的一致快照，
// 而不是服务连接池上另一次查询的结果。
func (s *Service) getWorkItem(ctx context.Context, q store.Querier, workItemID int64, docID string) (*Snapshot, error) {
	var d Snapshot
	var purged *time.Time
	err := q.QueryRow(ctx, `SELECT s.doc_id, s.title, s.revision, s.content_sha256, s.content, s.url, f.content_purged_at
		FROM work_item_doc_snapshots s JOIN work_item_doc_freezes f ON f.work_item_id=s.work_item_id
		WHERE s.work_item_id=$1 AND s.doc_id=$2`, workItemID, docID).
		Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.Content, &d.URL, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if purged != nil || unreadable(d.Content, d.SHA256) {
		return nil, fmt.Errorf("工作项 #%d 的文档 %s：%w", workItemID, docID, ErrSnapshotExpired)
	}
	return &d, nil
}

// ManifestSource 是文档依据的来源域。历史工作项的来源未知（迁移前没有记录），用 Unknown 标记。
type ManifestSource struct {
	Provider          string
	BaseURL           string
	CollectionID      string
	BindingGeneration int64
	Unknown           bool
}

// manifestDoc 是摘要中的一篇文档（只含元数据，不含正文）。
type manifestDoc struct {
	DocID    string `json:"doc_id"`
	Title    string `json:"title"`
	Revision int64  `json:"revision"`
	SHA256   string `json:"content_sha256"`
	URL      string `json:"url"`
}

// manifest 是文档依据的确定性编码：版本、种类、来源域和按 doc_id 排序的文档元数据。不含快照集 / 工作项编号、冻结时间、
// 清理时间或纯展示字段（集合名）。
type manifest struct {
	V                 int           `json:"v"`
	Kind              string        `json:"kind"`
	Provider          string        `json:"provider,omitempty"`
	BaseURL           string        `json:"base_url,omitempty"`
	CollectionID      string        `json:"collection_id,omitempty"`
	BindingGeneration int64         `json:"binding_generation,omitempty"`
	SourceUnknown     bool          `json:"source_unknown,omitempty"`
	Docs              []manifestDoc `json:"docs"`
}

// ManifestDigest 返回一份文档依据的 64 位小写 hex SHA-256。docs 先按 doc_id 字节序排序，列表顺序不影响结果；
// 来源域（provider、base_url、collection_id、绑定代次）与每篇文档的标题、修订号、摘要和地址都纳入，任何变化都会改变摘要。
func ManifestDigest(kind string, src ManifestSource, docs []Snapshot) string {
	m := manifest{V: manifestVersion, Kind: kind, Docs: make([]manifestDoc, 0, len(docs))}
	if src.Unknown {
		m.SourceUnknown = true
	} else {
		m.Provider, m.BaseURL, m.CollectionID, m.BindingGeneration = src.Provider, src.BaseURL, src.CollectionID, src.BindingGeneration
	}
	for _, d := range docs {
		m.Docs = append(m.Docs, manifestDoc{DocID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	slices.SortFunc(m.Docs, func(a, b manifestDoc) int { return strings.Compare(a.DocID, b.DocID) })
	b, _ := json.Marshal(m) //nolint:errchkjson // 只有字符串和整数，不会失败
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NoneManifestDigest 返回"没有文档依据"的摘要。它与绑定空集合的摘要不同（后者带来源域和 snapshot_set 种类）。
func NoneManifestDigest() string { return ManifestDigest(SourceNone, ManifestSource{}, nil) }

// Basis 是一次规划固定的文档依据（写进 ai_tasks.spec 和 ai_review_requests）。Docs 只有元数据。
type Basis struct {
	Source         string
	RepoID         int64
	WorkItemID     int64
	SnapshotSetID  int64
	ManifestDigest string
	Docs           []Snapshot
	// DocCount 是冻结时的篇数；Purged 表示正文已过保留期清理。
	DocCount int
	Purged   bool
	Provider string
	BaseURL  string
	// CollectionID / CollectionName / BindingGeneration 是冻结时的来源（历史工作项为空）。
	CollectionID      string
	CollectionName    string
	BindingGeneration int64
}

// PreparedFreeze 是一次已经拉取完成、还没有落库的整批文档（不含 token）。
type PreparedFreeze struct {
	RepoID int64
	// Bound 为假表示仓库没有绑定文档集：这是一次"没有文档"的观察结果，提交时不加仓库级绑定锁。
	Bound             bool
	Provider          string
	BaseURL           string
	CollectionID      string
	CollectionName    string
	BindingGeneration int64
	// Docs 是完整抓取的文档（含正文）。
	Docs           []Snapshot
	ManifestDigest string
}

// PrepareFreeze 在数据库事务之外读取仓库绑定、拉取整批文档正文并校验，算好确定性摘要；不落库。
// 没有绑定时返回 Bound=false、none 摘要。拉取失败或校验不通过时返回错误（不带空快照继续）。
//
// 总预算与生命周期取消由调用方统一管理（审计规划用 review.opContext）：这里不再发放预算，但把 ctx 与服务的生命周期
// 绑在一起（服务退出时立即中断外部拉取），否则"等锁 9 分钟 + 拉取 10 分钟"会突破调用方的总时限、也收不到退出信号。
func (s *Service) PrepareFreeze(ctx context.Context, repoID int64) (*PreparedFreeze, error) {
	done, err := s.beginOp()
	if err != nil {
		return nil, fmt.Errorf("服务正在退出，跳过这次文档冻结：%w", err)
	}
	defer done()
	if s.baseCtx != nil && s.baseCtx.Err() != nil {
		return nil, fmt.Errorf("服务正在退出：%w", s.baseCtx.Err())
	}
	ctx, cancelCtx := context.WithCancel(ctx)
	defer cancelCtx()
	var stop func() bool
	if s.baseCtx != nil {
		stop = context.AfterFunc(s.baseCtx, cancelCtx)
		defer stop()
	}
	binding, err := store.ResolveDocToken(ctx, s.db, s.masterKey, repoID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		return &PreparedFreeze{RepoID: repoID, ManifestDigest: NoneManifestDigest()}, nil
	}
	docs, err := s.fetchBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return &PreparedFreeze{
		RepoID: repoID, Bound: true, Provider: binding.Provider, BaseURL: binding.BaseURL,
		CollectionID: binding.CollectionID, CollectionName: binding.CollectionName, BindingGeneration: binding.Generation,
		Docs: docs, ManifestDigest: ManifestDigest(SourceSnapshotSet, bindingSource(binding), docs),
	}, nil
}

// CommitSetTx 在调用方的事务里写入 PreparedFreeze 的快照集与文档行（空集合也写，doc_count=0），返回引用它的 Basis。
// 只在数据库事务内执行，绝不请求外部文档系统；绑定在拉取期间变了（重绑、解绑、解绑再重绑）时返回 ErrBindingChanged。
func (s *Service) CommitSetTx(ctx context.Context, tx pgx.Tx, repoID int64, p *PreparedFreeze) (Basis, error) {
	if p == nil || !p.Bound {
		return Basis{Source: SourceNone, RepoID: repoID, ManifestDigest: NoneManifestDigest()}, nil
	}
	if err := verifyBindingTx(ctx, tx, repoID, p.Provider, p.BaseURL, p.CollectionID, p.BindingGeneration); err != nil {
		return Basis{}, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO doc_snapshot_sets (repo_id, provider, base_url, collection_id, collection_name,
			binding_generation, doc_count, manifest_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		repoID, p.Provider, p.BaseURL, p.CollectionID, p.CollectionName, p.BindingGeneration, len(p.Docs), p.ManifestDigest).Scan(&id); err != nil {
		return Basis{}, fmt.Errorf("写入文档快照集：%w", err)
	}
	for _, d := range p.Docs {
		if _, err := tx.Exec(ctx, `INSERT INTO doc_snapshot_docs (snapshot_set_id, doc_id, title, revision, content_sha256, content, url)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, d.DocID, d.Title, d.Revision, d.SHA256, d.Content, d.URL); err != nil {
			return Basis{}, fmt.Errorf("写入文档快照 %s：%w", d.DocID, err)
		}
	}
	return Basis{
		Source: SourceSnapshotSet, RepoID: repoID, SnapshotSetID: id, ManifestDigest: p.ManifestDigest,
		Docs: metadata(p.Docs), DocCount: len(p.Docs), Provider: p.Provider, BaseURL: p.BaseURL,
		CollectionID: p.CollectionID, CollectionName: p.CollectionName, BindingGeneration: p.BindingGeneration,
	}, nil
}

// WorkItemBasis 返回工作项固定的文档依据：已冻结时为 work_item（正文可能已清理，元数据仍在），没有冻结标记时为 none。
// 不会为工作项新建冻结（独立审计的冻结走 PrepareFreeze / CommitSetTx）。工作项不属于 repoID 时返回错误。
func (s *Service) WorkItemBasis(ctx context.Context, q store.Querier, repoID, workItemID int64) (Basis, error) {
	if err := requireWorkItemRepo(ctx, q, repoID, workItemID); err != nil {
		return Basis{}, err
	}
	b := Basis{Source: SourceNone, RepoID: repoID, WorkItemID: workItemID, ManifestDigest: NoneManifestDigest()}
	var (
		count        int
		stored       string
		purged       *time.Time
		provider     *string
		baseURL      *string
		collectionID *string
		name         *string
		generation   *int64
	)
	err := q.QueryRow(ctx, `SELECT doc_count, coalesce(manifest_digest, ''), content_purged_at, provider, base_url, collection_id,
			collection_name, binding_generation
		FROM work_item_doc_freezes WHERE work_item_id=$1`, workItemID).
		Scan(&count, &stored, &purged, &provider, &baseURL, &collectionID, &name, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, nil
	}
	if err != nil {
		return Basis{}, err
	}
	docs, err := s.listWorkItem(ctx, q, workItemID)
	if err != nil {
		return Basis{}, err
	}
	digest := stored
	if !validDigest(digest) {
		// 迁移前的旧记录没有摘要：按历史元数据确定性计算（来源未知），同一份元数据每次得到相同摘要。
		digest = ManifestDigest(SourceWorkItem, ManifestSource{Unknown: true}, docs)
	}
	b.Source, b.ManifestDigest, b.Docs, b.DocCount, b.Purged = SourceWorkItem, digest, docs, count, purged != nil
	b.Provider, b.BaseURL = deref(provider), deref(baseURL)
	b.CollectionID, b.CollectionName, b.BindingGeneration = deref(collectionID), deref(name), derefInt(generation)
	return b, nil
}

// LockBasisTx 在调用方的事务里锁住依据对应的对象，直到事务结束。快照集用 kiln:doc-set:<id>，工作项用
// kiln:doc-freeze:<id>，与保留清理（retention.Purge）使用同一把锁，保证清理与新引用 / 重跑提交严格串行。
func LockBasisTx(ctx context.Context, tx pgx.Tx, b Basis) error {
	switch b.Source {
	case SourceSnapshotSet:
		return store.LockXact(ctx, tx, DocSetLockKey(b.SnapshotSetID))
	case SourceWorkItem:
		return store.LockXact(ctx, tx, DocFreezeLockKey(b.WorkItemID))
	}
	return nil
}

// DocSetLockKey 是快照集的互斥锁键（冻结后的正文清理与新引用共用）。
func DocSetLockKey(id int64) string { return fmt.Sprintf("kiln:doc-set:%d", id) }

// DocFreezeLockKey 是工作项冻结的互斥锁键（冻结写入、正文清理与新引用共用）。
func DocFreezeLockKey(workItemID int64) string { return fmt.Sprintf("kiln:doc-freeze:%d", workItemID) }

// RequireReadableTx 在已持锁的事务里核对依据的正文仍然可读：正文已清理（或旧清理器留下的空正文）时返回
// ErrSnapshotExpired。空集合没有正文，直接通过。SourceNone 没有需要核对的内容。
func (s *Service) RequireReadableTx(ctx context.Context, tx pgx.Tx, repoID int64, b Basis) error {
	switch b.Source {
	case SourceSnapshotSet:
		return requireSetReadable(ctx, tx, repoID, b.SnapshotSetID)
	case SourceWorkItem:
		return requireWorkItemReadable(ctx, tx, repoID, b.WorkItemID)
	}
	return nil
}

// ListBasis 返回依据的文档元数据（按标题、doc_id 排序）；正文已清理也返回元数据。没有文档依据时返回空。
func (s *Service) ListBasis(ctx context.Context, q store.Querier, b Basis) ([]Snapshot, error) {
	switch b.Source {
	case SourceSnapshotSet:
		rows, err := q.Query(ctx, `SELECT doc_id, title, revision, content_sha256, url
			FROM doc_snapshot_docs WHERE snapshot_set_id=$1 ORDER BY title, doc_id`, b.SnapshotSetID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Snapshot, error) {
			var d Snapshot
			err := row.Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.URL)
			return d, err
		})
	case SourceWorkItem:
		return s.listWorkItem(ctx, q, b.WorkItemID)
	}
	return nil, nil
}

// GetBasis 返回依据中某一篇文档的正文；不存在时返回 nil，正文已清理或与摘要不一致时返回 ErrSnapshotExpired
// （不自动修复）。调用方应先确认依据仍可读。
func (s *Service) GetBasis(ctx context.Context, q store.Querier, b Basis, docID string) (*Snapshot, error) {
	switch b.Source {
	case SourceSnapshotSet:
		var d Snapshot
		var purged *time.Time
		err := q.QueryRow(ctx, `SELECT s.doc_id, s.title, s.revision, s.content_sha256, s.content, s.url, set.content_purged_at
			FROM doc_snapshot_docs s JOIN doc_snapshot_sets set ON set.id=s.snapshot_set_id
			WHERE s.snapshot_set_id=$1 AND s.doc_id=$2`, b.SnapshotSetID, docID).
			Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.Content, &d.URL, &purged)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if purged != nil || unreadable(d.Content, d.SHA256) {
			return nil, fmt.Errorf("文档快照集 #%d 的文档 %s：%w", b.SnapshotSetID, docID, ErrSnapshotExpired)
		}
		return &d, nil
	case SourceWorkItem:
		return s.getWorkItem(ctx, q, b.WorkItemID, docID)
	}
	return nil, nil
}

// Freeze 冻结仓库绑定文档集的快照到工作项。已经冻结过时直接返回已有快照（freeze-once：开发与审计读到同一份，仓库
// 之后更新文档不影响在途任务）——即使仓库后来解绑 / 重绑，也不拒绝读取已有的快照。
//
// 冻结以**工作项**为单位原子完成（不是逐篇去重）：并发进入的调用只有一个能提交完整快照，其余丢弃自己拉到的批次、读取
// 已提交的集合；空文档集也会记一个完成标记，之后不再重新拉取（否则集合由空变非空时会把新文档混进已经冻结的输入）。
//
// 仓库没有绑定文档集时返回空、且**不记完成标记**（管理员之后绑定、在首次真正的开发规划时仍能读到文档）。
//
// Freeze 会访问外部文档系统，调用方必须在数据库事务之外调用（领取、规划开发任务时）。拉取失败时返回错误，不带着空快照
// 执行；调用方可以让任务本次失败、稍后重试（重试会重新尝试冻结）。
func (s *Service) Freeze(ctx context.Context, repoID, workItemID int64) ([]Snapshot, error) {
	// 先登记再查库：服务正在退出（门闩已关）时不得再开始读写，避免在数据库关闭之后才动手。
	done, err := s.beginOp()
	if err != nil {
		return nil, fmt.Errorf("服务正在退出，跳过这次文档冻结：%w", err)
	}
	defer done()
	// 登记之后立刻建立这份操作自己的 context，覆盖 Freeze 的全部数据库查询、工作项锁等待与 List：
	// 不受调用方取消影响（首次领取的 Claim RPC 只有约 60 秒），总时限 FreezeTimeout，并随服务生命周期取消。
	// 这样服务退出（opGate.Close）时即使调用方传的是 Background，也能立即打断，Close 不会死等查询或锁。
	fctx, cancel := s.opContext(ctx)
	defer cancel()
	if s.baseCtx != nil && s.baseCtx.Err() != nil {
		return nil, fmt.Errorf("服务正在退出：%w", s.baseCtx.Err())
	}
	frozen, err := s.isFrozen(fctx, workItemID)
	if err != nil {
		return nil, err
	}
	if frozen {
		return s.List(fctx, workItemID)
	}
	lk, err := s.acquireWorkItemLock(fctx, workItemID)
	if err != nil {
		return nil, fmt.Errorf("等待工作项 #%d 的冻结锁：%w", workItemID, err)
	}
	defer s.releaseWorkItemLock(workItemID, lk)
	// 拿到锁后先复查冻结状态：等锁期间别的调用可能已经提交了快照（甚至管理员随后解绑），此时必须直接返回已冻结的快照，
	// 不能再去解析绑定、把空结果返回给等待者。
	if frozen, err := s.isFrozen(fctx, workItemID); err != nil {
		return nil, err
	} else if frozen {
		return s.List(fctx, workItemID)
	}
	// 绑定在拿到锁之后才读取：等锁期间绑定可能被重绑或解绑，锁之前读到的绑定会白拉一批文档，
	// 再被写入时的核对整批拒掉（ErrBindingChanged）
	binding, err := store.ResolveDocToken(fctx, s.db, s.masterKey, repoID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		return nil, nil
	}
	// 整批拉取与写入在这份预算下完成；超时后调用方重试会看到完成标记、直接复用结果，不会从头再拉一遍。
	// 整批快照仍在全部读取成功后才一次性写入。
	docs, err := s.fetchBinding(fctx, binding)
	if err != nil {
		return nil, err
	}
	digest := ManifestDigest(SourceWorkItem, bindingSource(binding), docs)
	if err := s.writeOnce(fctx, binding, workItemID, docs, digest); err != nil {
		return nil, err
	}
	return s.List(fctx, workItemID)
}

// fetchBinding 拉取绑定文档集的整批文档并校验：列表无空 ID、无重复 doc_id；每篇正文的 ID 与列表一致、仍在同一个集合、
// 且当前状态是已发布、未归档、未删除。任一不满足整批失败（ErrInvalidDocument），绝不静默跳过。
//
// 这些校验防止跨集合、非发布态与重绑污染，但不宣称具备 Outline 跨篇 ACID 原子性：读取是逐篇的，文档可能在读取期间
// 变化；绑定代次在写入时再核对一次（见 writeOnce / CommitSetTx）。
func (s *Service) fetchBinding(ctx context.Context, binding *store.DocBindingToken) ([]Snapshot, error) {
	prov, err := s.provider(binding.BaseURL, binding.Token)
	if err != nil {
		return nil, fmt.Errorf("构造 %s 文档客户端：%w", binding.Provider, err)
	}
	metas, err := prov.ListDocuments(ctx, binding.CollectionID)
	if err != nil {
		return nil, fmt.Errorf("拉取文档集 %s：%w", binding.CollectionID, err)
	}
	return s.fetchAll(ctx, prov, binding.CollectionID, metas)
}

// fetchAll 有界并发地读取一批文档正文并校验，按 (title, doc_id) 稳定排序返回快照；任一读取或校验失败则整体失败
// （不写入任何快照），并且在第一篇失败时取消其余还在进行 / 还没开始的读取（errgroup 的 context），不把整批拉完。
// 排序与外部系统的返回顺序无关：任务 spec 的文档引用、快照集里的行顺序、重跑读到的顺序都一致。
func (s *Service) fetchAll(ctx context.Context, prov docprovider.Provider, collectionID string, metas []docprovider.Document) ([]Snapshot, error) {
	if err := validateMetas(metas); err != nil {
		return nil, err
	}
	docs := make([]Snapshot, len(metas))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(docFetchConcurrency)
	for i := range metas {
		g.Go(func() error {
			full, err := prov.GetDocument(gctx, metas[i].ID)
			if err != nil {
				return fmt.Errorf("读取文档 %s：%w", metas[i].ID, err)
			}
			if err := validateFull(full, metas[i].ID, collectionID); err != nil {
				return err
			}
			docs[i] = Snapshot{DocID: full.ID, Title: full.Title, Revision: full.Revision, SHA256: docprovider.Sum(full.Content), Content: full.Content, URL: full.URL}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sortSnapshots(docs)
	return docs, nil
}

// sortSnapshots 按 (title, doc_id) 排序：标题给人看，doc_id 让同标题的文档也有确定的顺序（不依赖外部系统的列举顺序）。
func sortSnapshots(docs []Snapshot) {
	slices.SortFunc(docs, func(a, b Snapshot) int {
		if c := strings.Compare(a.Title, b.Title); c != 0 {
			return c
		}
		return strings.Compare(a.DocID, b.DocID)
	})
}

// validateMetas 校验文档列表：没有空 ID，没有重复的 doc_id（重复会让 doc_count / manifest_digest 与实际行数不一致）。
func validateMetas(metas []docprovider.Document) error {
	seen := make(map[string]struct{}, len(metas))
	for _, m := range metas {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("文档列表中有空 ID：%w", ErrInvalidDocument)
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("文档列表中有重复的 doc_id %s：%w", m.ID, ErrInvalidDocument)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// validateFull 校验读取到的文档正文：ID 与列表一致、仍在绑定的集合、当前是已发布且未归档、未删除。
func validateFull(full docprovider.Document, wantID, collectionID string) error {
	switch {
	case full.ID == "" || full.ID != wantID:
		return fmt.Errorf("文档 %s 的 ID 不一致（读到 %q）：%w", wantID, full.ID, ErrInvalidDocument)
	case full.CollectionID != collectionID:
		return fmt.Errorf("文档 %s 不在绑定的文档集 %s（当前 %q）：%w", wantID, collectionID, full.CollectionID, ErrInvalidDocument)
	case !full.Published || full.Archived || full.Deleted:
		return fmt.Errorf("文档 %s 不是可用的发布状态（published=%v archived=%v deleted=%v）：%w",
			wantID, full.Published, full.Archived, full.Deleted, ErrInvalidDocument)
	}
	return nil
}

// isFrozen 返回工作项是否已经完成过一次冻结（含空集合）。
func (s *Service) isFrozen(ctx context.Context, workItemID int64) (bool, error) {
	var one int
	err := s.db.QueryRow(ctx, `SELECT 1 FROM work_item_doc_freezes WHERE work_item_id=$1`, workItemID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// writeOnce 在一个事务里、按工作项加锁后一次性写入整批快照并打上完成标记；已经由别的调用冻结过时不做任何改动。
// 写入前核对绑定仍是拉取开始时的那个（重绑、解绑、解绑再重绑都拒绝写入）。空批次也写入完成标记（doc_count=0）。
func (s *Service) writeOnce(ctx context.Context, binding *store.DocBindingToken, workItemID int64, docs []Snapshot, digest string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.LockXact(ctx, tx, DocFreezeLockKey(workItemID)); err != nil {
			return err
		}
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM work_item_doc_freezes WHERE work_item_id=$1`, workItemID).Scan(&one)
		switch {
		case err == nil:
			return nil // 另一个调用已经冻结，丢弃自己的批次
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := verifyBindingTx(ctx, tx, binding.RepoID, binding.Provider, binding.BaseURL, binding.CollectionID, binding.Generation); err != nil {
			return err
		}
		for _, d := range docs {
			if _, err := tx.Exec(ctx, `INSERT INTO work_item_doc_snapshots
				(work_item_id, doc_id, title, revision, content_sha256, content, url)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, workItemID, d.DocID, d.Title, d.Revision, d.SHA256, d.Content, d.URL); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO work_item_doc_freezes (work_item_id, doc_count, provider, base_url, collection_id,
				collection_name, binding_generation, manifest_digest)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (work_item_id) DO NOTHING`,
			workItemID, len(docs), binding.Provider, binding.BaseURL, binding.CollectionID, binding.CollectionName,
			binding.Generation, digest)
		return err
	})
}

// verifyBindingTx 在事务里核对仓库绑定仍是拉取开始时的那个（FOR SHARE 挡住并发的重绑 / 解绑，直到事务提交）。
func verifyBindingTx(ctx context.Context, tx pgx.Tx, repoID int64, provider, baseURL, collectionID string, generation int64) error {
	var got bindingRow
	err := tx.QueryRow(ctx, `SELECT provider, base_url, collection_id, collection_name, generation
		FROM repo_doc_bindings WHERE repo_id=$1 FOR SHARE`, repoID).
		Scan(&got.Provider, &got.BaseURL, &got.CollectionID, &got.CollectionName, &got.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("仓库 #%d 的文档绑定已被解绑：%w", repoID, ErrBindingChanged)
	}
	if err != nil {
		return err
	}
	if got.Provider != provider || got.BaseURL != baseURL || got.CollectionID != collectionID || got.Generation != generation {
		return fmt.Errorf("仓库 #%d 的文档绑定已改变（代次 %d → %d）：%w", repoID, generation, got.Generation, ErrBindingChanged)
	}
	return nil
}

// bindingRow 是绑定核对读到的列。
type bindingRow struct {
	Provider       string
	BaseURL        string
	CollectionID   string
	CollectionName string
	Generation     int64
}

// bindingSource 是绑定的来源域（摘要用）。
func bindingSource(b *store.DocBindingToken) ManifestSource {
	return ManifestSource{Provider: b.Provider, BaseURL: b.BaseURL, CollectionID: b.CollectionID, BindingGeneration: b.Generation}
}

// metadata 去掉快照中的正文（Basis / 明细只保存元数据）。
func metadata(docs []Snapshot) []Snapshot {
	out := make([]Snapshot, 0, len(docs))
	for _, d := range docs {
		out = append(out, Snapshot{DocID: d.DocID, Title: d.Title, Revision: d.Revision, SHA256: d.SHA256, URL: d.URL})
	}
	return out
}

// listWorkItem 返回工作项已冻结文档的元数据（按标题、doc_id 排序）。
func (s *Service) listWorkItem(ctx context.Context, q store.Querier, workItemID int64) ([]Snapshot, error) {
	rows, err := q.Query(ctx, `SELECT doc_id, title, revision, content_sha256, url
		FROM work_item_doc_snapshots WHERE work_item_id=$1 ORDER BY title, doc_id`, workItemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Snapshot, error) {
		var d Snapshot
		err := row.Scan(&d.DocID, &d.Title, &d.Revision, &d.SHA256, &d.URL)
		return d, err
	})
}

// requireWorkItemRepo 校验工作项属于 repoID。
func requireWorkItemRepo(ctx context.Context, q store.Querier, repoID, workItemID int64) error {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM dev_work_items WHERE id=$1 AND repo_id=$2`, workItemID, repoID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("工作项 #%d 不属于仓库 #%d", workItemID, repoID)
	}
	return err
}

// requireSetReadable 核对快照集的正文仍可读：属于同一个仓库、篇数与记录一致、没有被清理、每篇摘要合法且没有
// "空正文但摘要非空"的篡改迹象。
func requireSetReadable(ctx context.Context, tx pgx.Tx, repoID, setID int64) error {
	var (
		gotRepo int64
		count   int
		purged  *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT repo_id, doc_count, content_purged_at FROM doc_snapshot_sets WHERE id=$1`, setID).
		Scan(&gotRepo, &count, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("文档快照集 #%d 不存在：%w", setID, ErrSnapshotExpired)
	}
	if err != nil {
		return err
	}
	if gotRepo != repoID {
		return fmt.Errorf("文档快照集 #%d 不属于仓库 #%d", setID, repoID)
	}
	if count == 0 {
		return nil // 空集合没有正文可丢
	}
	if purged != nil {
		return fmt.Errorf("文档快照集 #%d：%w", setID, ErrSnapshotExpired)
	}
	var rows, bad int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE content_sha256 !~ '^[0-9a-f]{64}$' OR (content='' AND content_sha256 <> $2))
		FROM doc_snapshot_docs WHERE snapshot_set_id=$1`, setID, emptySHA()).Scan(&rows, &bad); err != nil {
		return err
	}
	if rows != count || bad > 0 {
		return fmt.Errorf("文档快照集 #%d 的正文不完整：%w", setID, ErrSnapshotExpired)
	}
	return nil
}

// requireWorkItemReadable 核对工作项快照的正文仍可读。迁移前的旧记录没有 content_purged_at，可能已被旧清理器清空：
// 同样按"篇数一致、摘要合法、没有空正文配非空摘要"识别，命中即视为过期（绝不伪造 content_purged_at）。
func requireWorkItemReadable(ctx context.Context, tx pgx.Tx, repoID, workItemID int64) error {
	if err := requireWorkItemRepo(ctx, tx, repoID, workItemID); err != nil {
		return err
	}
	var (
		count  int
		purged *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT doc_count, content_purged_at FROM work_item_doc_freezes WHERE work_item_id=$1`, workItemID).
		Scan(&count, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("工作项 #%d：%w", workItemID, ErrSnapshotExpired)
	}
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if purged != nil {
		return fmt.Errorf("工作项 #%d 的文档快照：%w", workItemID, ErrSnapshotExpired)
	}
	var rows, bad int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE content_sha256 !~ '^[0-9a-f]{64}$' OR (content='' AND content_sha256 <> $2))
		FROM work_item_doc_snapshots WHERE work_item_id=$1`, workItemID, emptySHA()).Scan(&rows, &bad); err != nil {
		return err
	}
	if rows != count || bad > 0 {
		return fmt.Errorf("工作项 #%d 的文档正文不完整：%w", workItemID, ErrSnapshotExpired)
	}
	return nil
}

// EmptyContentSHA256 是空正文的 SHA-256：合法的空文档与"正文被保留清理"的区别（单一来源，读取与展示共用）。
// 写成常量：它必须等于 docprovider.Sum("")，也不能被任何地方改写（测试 TestEmptyContentSHA256）。
const EmptyContentSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// emptySHA 是空正文的 SHA-256（合法的空文档与"正文被清空"的区别）。
func emptySHA() string { return EmptyContentSHA256 }

// unreadable 判断一行文档的正文是否不可读：正文为空但摘要不是空正文的摘要（已被清空或篡改）。
func unreadable(content, digest string) bool { return content == "" && digest != emptySHA() }

// validDigest 判断是否是 64 位小写 hex SHA-256。
func validDigest(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

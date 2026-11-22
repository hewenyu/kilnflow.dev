// Package docprovider 是外部设计文档系统的只读客户端（PLAN #31）：Kiln 把仓库绑定到第三方文档集
// （首期 Outline），在任务领取 / 规划时冻结不可变快照，再交给 AI 开发与审计 Agent 读取。
//
// 这里只定义 Provider 的窄接口和两份数据结构。Provider 不持有 Kiln 的凭据：凭据由 Server 加密保存，构造 provider 时
// 交给它；执行节点永远拿不到外部文档凭据（它经 kiln-orca task doc 向 Server 要冻结快照）。
package docprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Document 是一篇设计文档的一个修订快照。Content 是该修订的 Markdown 正文；Revision 是提供方单调递增的修订号
// （Outline 的 documents.revision，每次保存自增）；SHA256 是 Content 的 sha256（十六进制），用于核对内容未变。
type Document struct {
	ID       string
	Title    string
	Revision int64
	SHA256   string
	Content  string
	URL      string
	// CollectionID 是文档当前所在文档集的 id（Outline 的 collectionId，可能为空表示不在任何集合）。
	// 只读拉取方据此校验文档仍属于绑定的文档集（文档可能被移到别的集合）。
	CollectionID string
	// Published / Archived / Deleted 是提供方当前的发布状态（Outline 的 publishedAt / archivedAt / deletedAt 是否有值）。
	// 三者都由显式时间字段解析：缺字段或 null 一律为 false，不把零值当成已发布。冻结只接受
	// Published && !Archived && !Deleted 的文档。
	Published bool
	Archived  bool
	Deleted   bool
}

// Sum 返回 content 的 sha256（十六进制），与 Document.SHA256 的口径一致。
func Sum(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// Collection 是一个外部文档集（Outline 的 Collection，文档树的根）。
type Collection struct {
	ID   string
	Name string
	// URL 是文档集在提供方站点上的地址（Outline 的 collection.url）；提供方没有返回时为空。
	URL string
}

// Provider 是外部设计文档的只读来源。实现必须只读：Kiln 不修改第三方文档，也不把快照回写成文档。
type Provider interface {
	// ListCollections 列出可访问的文档集（供管理员绑定仓库时选择）。
	ListCollections(ctx context.Context) ([]Collection, error)
	// ListDocuments 列出一个文档集里的所有文档，只填 ID、Title、Revision、URL（Content 为空，避免拉取整棵树的正文）。
	ListDocuments(ctx context.Context, collectionID string) ([]Document, error)
	// GetDocument 读取一篇文档的当前内容（含 Content、Revision、SHA256）。
	GetDocument(ctx context.Context, docID string) (Document, error)
}

// UpdateDocumentRequest 是修改一篇文档正文的请求。LastRevision 是读取时看到的修订号：提供方按它做乐观并发
// （Outdated 时拒绝），避免覆盖别人在这之间做的编辑。只改正文，不改标题、集合和发布状态。
type UpdateDocumentRequest struct {
	ID           string
	Text         string
	LastRevision int64
}

// DocumentWriter 是外部设计文档的受限写接口：只支持读取一篇文档和修改它的正文。
//
// 它与只读的 Provider 分开：只读路径只持有 Provider，写路径只拿得到这一个窄接口。实现必须遵守：
//   - UpdateDocument 只在提供方支持 CAS（LastRevision）时使用，绝不覆盖未知的并发修改；
//   - 写请求的结果不明时返回 EffectMayHaveApplied 的错误，调用方只做只读查证、绝不重发。
type DocumentWriter interface {
	GetDocument(ctx context.Context, docID string) (Document, error)
	UpdateDocument(ctx context.Context, req UpdateDocumentRequest) (Document, error)
}

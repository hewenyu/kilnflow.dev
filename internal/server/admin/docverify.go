package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 显式只读连接验证：管理员在 Dashboard 上触发一次，确认文档集可访问、列举完整、抽查一篇正文能读。
// 它只访问只读接口，不冻结快照、不读整批正文、不修改外部文档，也不代表「待决策清单」的写 token 有写权限。
//
// 验证在数据库事务与锁之外进行（不持有 DocBindingLockKey / 行锁 / DB 连接）；结果在短事务里按绑定代次写入。
// 普通页面 GET、自动刷新与正文读取都不会触发这里的外部请求。

// 显式验证的时限。外部只读探测与收尾落库分开计时：外部调用可能耗尽自己的预算，但结论仍要在
// 总预算内写进数据库；服务退出时（BaseCtx 取消）两者都立即停止，且绝不落库。
const (
	// docVerifyTimeout 是一次显式验证的总时限（含外部探测与落库）。
	docVerifyTimeout = 60 * time.Second
	// docVerifyExternalTimeout 是外部 Outline 只读调用的期限：给收尾落库留出余量。
	docVerifyExternalTimeout = 50 * time.Second
	// docVerifySaveTimeout 是收尾落库与审计的期限（独立于外部调用，不受已经过期的外部期限影响）。
	docVerifySaveTimeout = 10 * time.Second
)

// ErrDocBindingChangedDuringVerify 表示验证期间仓库的文档绑定被重绑或解绑：本次结果属于旧配置，没有写入。
var ErrDocBindingChangedDuringVerify = errors.New("验证期间文档绑定已变更，本次检查结果未应用")

// DocVerifyResult 是一次显式验证的结果（不含 token 与远端原始信息）。
type DocVerifyResult struct {
	Repo string
	// Status 是 ok / empty / failed。
	Status string
	// ErrorCode 是 failed 时的固定枚举错误码。
	ErrorCode string
	// DocCount 是列举到的篇数；列举没有完成时为 nil。
	DocCount *int
	// SampleDocID 是抽查了正文的文档 id。
	SampleDocID string
	// CollectionURL / CollectionName 是 collections.list 观测到的文档集地址与名称。
	CollectionURL  string
	CollectionName string
	CheckedAt      time.Time
	// Applied 表示结果是否写进了当前绑定（false 表示验证期间绑定变了）。
	Applied bool
}

// VerifyDocBinding 对仓库当前绑定的文档集做一次显式只读验证，写入检查状态与审计日志，返回结果。
// 已停用的仓库也能验证（只读，不改变绑定），方便查看历史配置。没有绑定时返回错误；外部访问失败仍返回结果
// （Status=failed），只有本地错误（查库、写库）才返回 error。
func (s *Service) VerifyDocBinding(ctx context.Context, fullName, actor string) (*DocVerifyResult, error) {
	// 先登记再查库：服务正在退出时必须立即拒绝，不再开始读写。done 覆盖整次操作（含最终落库与审计）。
	if s.track != nil {
		done, err := s.track()
		if err != nil {
			return nil, fmt.Errorf("服务正在退出，跳过这次文档验证：%w", err)
		}
		defer done()
	}
	// 唯一的总预算：查库、外部探测与收尾落库都从它派生，加起来不会超过 docVerifyTimeout。
	octx, cancel := s.verifyContext(ctx)
	defer cancel()

	// 查库与外部探测只能用总预算里扣掉落库预算之后的部分：外部调用再慢也要给落库留下时间。
	probeCtx, probeCancel := context.WithTimeout(octx, s.probeTimeout())
	defer probeCancel()

	repo, err := store.FindRepoAny(probeCtx, s.db, fullName)
	if err != nil {
		return nil, err
	}
	tok, err := store.ResolveDocToken(probeCtx, s.db, s.cfg.MasterKey, repo.ID)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, fmt.Errorf("%s 没有文档绑定", repo.FullName())
	}
	startedAt := time.Now().UTC()
	// 外部只读探测另外再受 externalTimeout 限制；即使耗尽，下面的收尾事务仍在总预算内把结论写进去。
	extCtx, extCancel := context.WithTimeout(probeCtx, s.docVerifyExternalTimeout)
	res := s.checkBinding(extCtx, tok)
	extCancel()
	res.Repo = repo.FullName()
	res.CheckedAt = time.Now().UTC()

	saveCtx, saveCancel, err := s.saveContext(octx)
	if err != nil {
		return nil, err
	}
	defer saveCancel()

	applied := false
	err = s.db.Tx(saveCtx, func(tx pgx.Tx) error {
		ok, err := store.SaveDocBindingCheck(saveCtx, tx, repo.ID, tok.Generation, store.DocCheckResult{
			StartedAt: startedAt, CheckedAt: res.CheckedAt, CheckedBy: actor, Status: res.Status,
			ErrorCode: res.ErrorCode, DocCount: res.DocCount, SampleDocID: res.SampleDocID,
			CollectionURL: res.CollectionURL, ObservedCollectionName: res.CollectionName,
		})
		if err != nil {
			return err
		}
		applied = ok
		if !ok {
			return nil // 绑定已经被重绑 / 解绑：不写审计（这次结果不属于任何当前配置）
		}
		detail := map[string]any{"repo": repo.FullName(), "status": res.Status}
		if res.ErrorCode != "" {
			detail["error_code"] = res.ErrorCode
		}
		if res.DocCount != nil {
			detail["doc_count"] = *res.DocCount
		}
		if res.SampleDocID != "" {
			detail["sample_doc_id"] = res.SampleDocID
		}
		return store.Audit(saveCtx, tx, actor, "doc.verify", detail)
	})
	if err != nil {
		return nil, err
	}
	res.Applied = applied
	if !applied {
		return res, ErrDocBindingChangedDuringVerify
	}
	return res, nil
}

// verifyContext 返回验证使用的总 context：不受调用方断开影响、总时限 docVerifyTimeout，并随服务生命周期取消。
func (s *Service) verifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.docVerifyTimeout)
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

// probeTimeout 返回查库与外部探测可用的预算：总预算减去收尾落库预算。配置不当时（落库预算不小于总预算）
// 退回总预算，保证仍然是正数而不是立即过期。
func (s *Service) probeTimeout() time.Duration {
	if d := s.docVerifyTimeout - s.docVerifySaveTimeout; d > 0 {
		return d
	}
	return s.docVerifyTimeout
}

// saveContext 返回收尾落库使用的 context：它从总预算派生，期限再收紧到 docVerifySaveTimeout，所以查库、外部探测
// 与落库加起来不会超过总预算；服务正在退出时不落库——先检查 BaseCtx，且总 context 已随它取消。
func (s *Service) saveContext(totalCtx context.Context) (context.Context, context.CancelFunc, error) {
	if s.baseCtx != nil {
		if err := s.baseCtx.Err(); err != nil {
			return nil, nil, fmt.Errorf("服务正在退出，没有保存这次文档验证的结果：%w", err)
		}
	}
	octx, cancel := context.WithTimeout(totalCtx, s.docVerifySaveTimeout)
	return octx, cancel, nil
}

// checkBinding 在数据库之外调用只读接口，按固定枚举给出结论；不返回、不保存远端原始信息。
func (s *Service) checkBinding(ctx context.Context, tok *store.DocBindingToken) *DocVerifyResult {
	res := &DocVerifyResult{Status: docsync.DocCheckStatusFailed}
	prov, err := s.docProvider(tok.BaseURL, tok.Token)
	if err != nil {
		res.ErrorCode = docsync.DocCheckInvalidBaseURL
		return res
	}
	collections, err := prov.ListCollections(ctx)
	if err != nil {
		res.ErrorCode = docsync.ClassifyDocProviderError(err, "collections")
		return res
	}
	var collection *docprovider.Collection
	for i := range collections {
		if collections[i].ID == tok.CollectionID {
			collection = &collections[i]
			break
		}
	}
	if collection == nil {
		res.ErrorCode = docsync.DocCheckCollectionNotFound
		return res
	}
	res.CollectionURL, res.CollectionName = collection.URL, collection.Name

	metas, err := prov.ListDocuments(ctx, tok.CollectionID)
	if err != nil {
		res.ErrorCode = docsync.ClassifyDocProviderError(err, "documents")
		return res
	}
	count := len(metas)
	res.DocCount = &count
	if err := validateCheckMetas(metas, tok.CollectionID); err != nil {
		res.ErrorCode = docsync.DocCheckInvalidDocument
		return res
	}
	if count == 0 {
		res.Status = docsync.DocCheckStatusEmpty
		return res
	}
	sampleID := sampleDocID(metas)
	full, err := prov.GetDocument(ctx, sampleID)
	if err != nil {
		res.ErrorCode = docsync.ClassifyDocProviderError(err, "document")
		return res
	}
	if full.ID != sampleID || full.CollectionID != tok.CollectionID || !full.Published || full.Archived || full.Deleted {
		res.ErrorCode = docsync.DocCheckSampleInvalid
		return res
	}
	res.Status = docsync.DocCheckStatusOK
	res.SampleDocID = sampleID
	return res
}

// validateCheckMetas 校验列举结果：没有空 id、没有重复、没有越出文档集、没有未发布 / 已归档 / 已删除的文档。
// 这些问题会让「冻结」在真正执行时失败，显式验证不应该给出通过的结论。
func validateCheckMetas(metas []docprovider.Document, collectionID string) error {
	seen := make(map[string]struct{}, len(metas))
	for _, m := range metas {
		if strings.TrimSpace(m.ID) == "" {
			return errors.New("文档列表中有空 id")
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("文档列表中有重复的 id %s", m.ID)
		}
		seen[m.ID] = struct{}{}
		if m.CollectionID != "" && m.CollectionID != collectionID {
			return fmt.Errorf("文档 %s 不在文档集 %s", m.ID, collectionID)
		}
		if !m.Published || m.Archived || m.Deleted {
			return fmt.Errorf("文档 %s 不是可用的发布状态", m.ID)
		}
	}
	return nil
}

// sampleDocID 按稳定的 doc_id 升序返回第 1 篇（与外部系统的返回顺序无关）。
func sampleDocID(metas []docprovider.Document) string {
	best := metas[0].ID
	for _, m := range metas[1:] {
		if m.ID < best {
			best = m.ID
		}
	}
	return best
}

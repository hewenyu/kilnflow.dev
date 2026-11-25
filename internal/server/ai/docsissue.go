package ai

import "fmt"

// 设计依据的原因码（多语言，见 docs/I18N.md）：领域层不接收语言，依据记录不一致的原因和关联任务的依据关系说明都给出
// 稳定的原因码和原始参数，由 internal/server/web 映射为消息；中文说明（Error、Detail、String）保留给 CLI、日志和 RPC。

// DocsIssueCode 是设计依据记录不一致的原因码（DocsIssue.Code）。
type DocsIssueCode string

const (
	// IssueSnapshotSetMissing：文档快照集 SnapshotSetID 不存在。
	IssueSnapshotSetMissing DocsIssueCode = "snapshot_set_missing"
	// IssueSnapshotSetOtherRepo：文档快照集 SnapshotSetID 不属于请求的仓库。
	IssueSnapshotSetOtherRepo DocsIssueCode = "snapshot_set_other_repo"
	// IssueSnapshotSetDigest：快照集的摘要与记录不一致。
	IssueSnapshotSetDigest DocsIssueCode = "snapshot_set_digest_mismatch"
	// IssueRequestSetDigest：请求记录的摘要与文档快照集不一致。
	IssueRequestSetDigest DocsIssueCode = "request_set_digest_mismatch"
	// IssueRequestWorkItemSnapshot：请求引用的工作项快照不存在。
	IssueRequestWorkItemSnapshot DocsIssueCode = "request_work_item_snapshot_missing"
	// IssueRequestWorkItemDigest：请求记录的摘要与工作项快照不一致。
	IssueRequestWorkItemDigest DocsIssueCode = "request_work_item_digest_mismatch"
	// IssueRequestSourceUnknown：请求的文档依据来源无法识别。
	IssueRequestSourceUnknown DocsIssueCode = "request_source_unknown"
	// IssueBasisDigestInvalid：文档依据缺少合法摘要。
	IssueBasisDigestInvalid DocsIssueCode = "basis_digest_invalid"
	// IssueTaskWorkItemMissing：AI 任务 TaskID 引用的工作项 WorkItemID 不存在。
	IssueTaskWorkItemMissing DocsIssueCode = "task_work_item_missing"
	// IssueTaskWorkItemOtherRepo：AI 任务 TaskID 引用的工作项 WorkItemID 不属于请求的仓库。
	IssueTaskWorkItemOtherRepo DocsIssueCode = "task_work_item_other_repo"
	// IssueTaskWorkItemSnapshot：AI 任务 TaskID 引用的工作项快照不存在。
	IssueTaskWorkItemSnapshot DocsIssueCode = "task_work_item_snapshot_missing"
	// IssueTaskDevRequestMismatch：AI 任务 TaskID 的工作项与开发请求 RequestID 的记录不一致。
	IssueTaskDevRequestMismatch DocsIssueCode = "task_dev_request_mismatch"
	// IssueTaskNoWorkItem：AI 任务 TaskID 不是选题任务，开发请求却没有记录工作项。
	IssueTaskNoWorkItem DocsIssueCode = "task_no_work_item"
	// IssueTaskClaimMismatch：AI 任务 TaskID 的工作项与该任务领取到的工作项不一致。
	IssueTaskClaimMismatch DocsIssueCode = "task_claim_mismatch"
	// IssueTaskSpecMismatch：AI 任务 TaskID 的文档依据与请求明细不一致。
	IssueTaskSpecMismatch DocsIssueCode = "task_spec_mismatch"
	// IssueTaskSpecDigest：AI 任务 TaskID 的文档依据摘要与请求明细不一致。
	IssueTaskSpecDigest DocsIssueCode = "task_spec_digest_mismatch"
	// IssueTaskRefsMismatch：AI 任务 TaskID 的文档引用与固化的依据不一致。
	IssueTaskRefsMismatch DocsIssueCode = "task_refs_mismatch"
	// IssueTaskRefsUnexpected：AI 任务 TaskID 记录了文档引用，与固化的（没有文档的）依据不一致。
	IssueTaskRefsUnexpected DocsIssueCode = "task_refs_unexpected"
)

// DocsIssue 是设计依据记录与实际快照不一致的一个原因：原因码加原始参数（只有对应的原因码用到的字段有值）。
// 作为错误时它是 ErrDocsBasisConflict（errors.Is 成立），Error() 与原来的中文说明相同。
type DocsIssue struct {
	Code                                         DocsIssueCode
	TaskID, RequestID, WorkItemID, SnapshotSetID int64
}

// Detail 返回原因的中文说明（不带"设计依据记录不一致"的前缀）。
func (e *DocsIssue) Detail() string {
	switch e.Code {
	case IssueSnapshotSetMissing:
		return fmt.Sprintf("文档快照集 #%d 不存在", e.SnapshotSetID)
	case IssueSnapshotSetOtherRepo:
		return fmt.Sprintf("文档快照集 #%d 不属于请求的仓库", e.SnapshotSetID)
	case IssueSnapshotSetDigest:
		return "快照集的摘要与记录不一致"
	case IssueRequestSetDigest:
		return "请求记录的摘要与文档快照集不一致"
	case IssueRequestWorkItemSnapshot:
		return "请求引用的工作项快照不存在"
	case IssueRequestWorkItemDigest:
		return "请求记录的摘要与工作项快照不一致"
	case IssueRequestSourceUnknown:
		return "请求的文档依据来源无法识别"
	case IssueBasisDigestInvalid:
		return "文档依据缺少合法摘要"
	case IssueTaskWorkItemMissing:
		return fmt.Sprintf("AI 任务 #%d 引用的工作项 #%d 不存在", e.TaskID, e.WorkItemID)
	case IssueTaskWorkItemOtherRepo:
		return fmt.Sprintf("AI 任务 #%d 引用的工作项 #%d 不属于请求的仓库", e.TaskID, e.WorkItemID)
	case IssueTaskWorkItemSnapshot:
		return fmt.Sprintf("AI 任务 #%d 引用的工作项快照不存在", e.TaskID)
	case IssueTaskDevRequestMismatch:
		return fmt.Sprintf("AI 任务 #%d 的工作项与开发请求 #%d 的记录不一致", e.TaskID, e.RequestID)
	case IssueTaskNoWorkItem:
		return fmt.Sprintf("AI 任务 #%d 不是选题任务，开发请求却没有记录工作项", e.TaskID)
	case IssueTaskClaimMismatch:
		return fmt.Sprintf("AI 任务 #%d 的工作项与该任务领取到的工作项不一致", e.TaskID)
	case IssueTaskSpecMismatch:
		return fmt.Sprintf("AI 任务 #%d 的文档依据与请求明细不一致", e.TaskID)
	case IssueTaskSpecDigest:
		return fmt.Sprintf("AI 任务 #%d 的文档依据摘要与请求明细不一致", e.TaskID)
	case IssueTaskRefsMismatch:
		return fmt.Sprintf("AI 任务 #%d 的文档引用与固化的依据不一致", e.TaskID)
	case IssueTaskRefsUnexpected:
		return fmt.Sprintf("AI 任务 #%d 记录了文档引用，与固化的依据不一致", e.TaskID)
	}
	return string(e.Code)
}

func (e *DocsIssue) Error() string { return ErrDocsBasisConflict.Error() + "：" + e.Detail() }

// Unwrap 让 errors.Is(err, ErrDocsBasisConflict) 成立。
func (e *DocsIssue) Unwrap() error { return ErrDocsBasisConflict }

// RelationNote 是关联任务与工作项冻结依据的关系说明的原因码（RelatedTask.NoteCode）。
type RelationNote string

const (
	// NoteBasisConflict：任务的文档依据记录与工作项不一致（具体原因见 RelatedTask.Issue / Detail）。
	NoteBasisConflict RelationNote = "basis_conflict"
	// NoteBasisUnverifiable：任务的文档依据无法核对（具体原因见 RelatedTask.Issue / Detail）。
	NoteBasisUnverifiable  RelationNote = "basis_unverifiable"
	NoteOtherWorkItem      RelationNote = "other_work_item"
	NoteLegacyDigestPinned RelationNote = "legacy_digest_pinned"
	NoteLegacyRefsMismatch RelationNote = "legacy_refs_mismatch"
	NoteLegacyNoneOK       RelationNote = "legacy_none_confirmed"
	NoteLegacyNoneRefs     RelationNote = "legacy_none_refs"
	NoteClaimedDynamic     RelationNote = "claimed_dynamic"
	NoteLegacyPinned       RelationNote = "legacy_pinned"
	NoteLegacyNoBasis      RelationNote = "legacy_no_basis"
	NoteLegacyInsufficient RelationNote = "legacy_insufficient"
	NoteDeclaredNone       RelationNote = "declared_none"
	NoteUnknown            RelationNote = "unknown"
	NoteWorkItemNoBasis    RelationNote = "work_item_no_basis"
	NoteNoRefs             RelationNote = "no_refs"
	NoteSameEmpty          RelationNote = "same_empty"
	NoteSameBasis          RelationNote = "same_basis"
	NoteItemEmptyNoDigest  RelationNote = "work_item_empty_no_digest"
	NoteItemEmptyDigest    RelationNote = "work_item_empty_digest_mismatch"
	NoteItemRefsMismatch   RelationNote = "work_item_refs_mismatch"
	NoteItemRefsNoDigest   RelationNote = "work_item_refs_no_digest"
	NoteItemDigestMismatch RelationNote = "work_item_digest_mismatch"
	NoteSetEmptyNoDigest   RelationNote = "set_empty_no_digest"
	NoteSetEmptyDigest     RelationNote = "set_empty_digest_mismatch"
	NoteSetRefsMismatch    RelationNote = "set_refs_mismatch"
	NoteSetRefsNoDigest    RelationNote = "set_refs_no_digest"
	NoteSetDigestMismatch  RelationNote = "set_digest_mismatch"
)

// relationNotes 是每个原因码的中文说明（给人看，不含推断）。
var relationNotes = map[RelationNote]string{
	NoteBasisConflict:      "任务的文档依据记录与工作项不一致",
	NoteBasisUnverifiable:  "任务的文档依据无法核对",
	NoteOtherWorkItem:      "任务记录的依据指向另一个工作项，与当前工作项不一致",
	NoteLegacyDigestPinned: "迁移前没有记录整批摘要；重跑时按任务记录中的引用核对了工作项冻结快照，逐篇一致",
	NoteLegacyRefsMismatch: "迁移前的任务记录中的引用与重跑时固化的依据不一致",
	NoteLegacyNoneOK:       "迁移前的请求没有记录文档依据；重跑时核对确认任务也没有文档引用",
	NoteLegacyNoneRefs:     "迁移前的任务记录了文档引用，与重跑时固化的“没有依据”不一致",
	NoteClaimedDynamic:     "任务没有在 spec 中固定文档；领取后按工作项冻结快照解析",
	NoteLegacyPinned:       "迁移前没有记录依据；重跑时按任务记录中的引用核对了工作项冻结快照，逐篇一致",
	NoteLegacyNoBasis:      "迁移前的请求没有记录文档依据",
	NoteLegacyInsufficient: "迁移前的依据记录不足以确认",
	NoteDeclaredNone:       "请求记录为没有文档依据",
	NoteUnknown:            "无法确认依据关系",
	NoteWorkItemNoBasis:    "当前工作项没有可读的冻结依据，无法核对",
	NoteNoRefs:             "任务没有记录文档引用，无法逐篇核对",
	NoteSameEmpty:          "依据是同一份冻结的空文档集",
	NoteSameBasis:          "任务记录中的文档引用与整批摘要都与这份冻结依据一致",
	NoteItemEmptyNoDigest:  "任务没有记录文档引用；工作项的冻结依据也是空集合，但没有整批摘要可核对",
	NoteItemEmptyDigest:    "任务记录的整批摘要与当前工作项的空集合不一致",
	NoteItemRefsMismatch:   "任务记录中的文档引用与当前工作项的冻结依据不一致",
	NoteItemRefsNoDigest:   "任务记录的文档引用与当前工作项的冻结依据逐篇一致；任务没有记录整批摘要",
	NoteItemDigestMismatch: "任务记录的整批摘要与当前工作项的冻结依据不一致",
	NoteSetEmptyNoDigest:   "任务没有记录文档引用，也没有整批摘要可核对空快照集",
	NoteSetEmptyDigest:     "任务记录的整批摘要与这份空快照集不一致",
	NoteSetRefsMismatch:    "任务记录中的文档引用与这份冻结依据不一致",
	NoteSetRefsNoDigest:    "任务记录的文档引用与这份冻结依据逐篇一致；任务没有记录整批摘要",
	NoteSetDigestMismatch:  "任务记录的整批摘要与这份冻结依据不一致",
}

// String 返回关系说明的中文（不认识的原因码原样返回）。
func (n RelationNote) String() string {
	if s, ok := relationNotes[n]; ok {
		return s
	}
	return string(n)
}

// RelationNotes 返回全部关系说明的原因码（测试检查每个原因码都有页面上的消息）。
func RelationNotes() []RelationNote {
	out := make([]RelationNote, 0, len(relationNotes))
	for n := range relationNotes {
		out = append(out, n)
	}
	return out
}

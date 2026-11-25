package web

import (
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/docsync"
)

// AI 请求、AI 任务、AI 开发工作项和设计依据页面的文字（多语言，见 docs/I18N.md）：领域层（ai、docsync）给出原因码和原始参数，
// 这里映射为消息；不认识的原因码（升级 kiln-server 时的版本差异）原样显示领域层的中文说明。

// aiTypeText 返回 AI 请求类型（ai_requests.type）的名称。
func aiTypeText(typ string) uiText {
	if typ == agentcfg.TaskDevelop {
		return message(msgAITypeDevelop)
	}
	return message(msgAITypeReview)
}

// aiRequestTitle 返回 AI 请求的标题，例如"AI 审计请求 #12"。
func aiRequestTitle(typ string, id int64) uiText {
	if typ == agentcfg.TaskDevelop {
		return message(msgAIRequestTitleDev, intArg(argID, id))
	}
	return message(msgAIRequestTitleReview, intArg(argID, id))
}

// aiTaskTitle 返回 AI 任务页的标题，例如"review · AI 审计请求 #12"。
func aiTaskTitle(t *ai.Task) uiText {
	if t.Type == agentcfg.TaskDevelop {
		return message(msgAITaskTitleDev, strArg(argName, t.DisplayName), intArg(argID, t.RequestID))
	}
	return message(msgAITaskTitleReview, strArg(argName, t.DisplayName), intArg(argID, t.RequestID))
}

// aiTypeOptions 返回 AI 请求列表按类型筛选的选项。
func aiTypeOptions() []textOption {
	return []textOption{
		{agentcfg.TaskReview, aiTypeText(agentcfg.TaskReview)},
		{agentcfg.TaskDevelop, aiTypeText(agentcfg.TaskDevelop)},
	}
}

// aiRequestStateOptions 返回 AI 请求列表按状态筛选的选项（与 ai.RequestState 一致）。
func aiRequestStateOptions() []textOption {
	return []textOption{
		{string(ai.RequestQueued), message(msgAIReqStateQueued)},
		{string(ai.RequestActive), message(msgAIReqStateActive)},
		{string(ai.RequestSucceeded), message(msgAIReqStateSucceeded)},
		{string(ai.RequestFailed), message(msgAIReqStateFailed)},
		{string(ai.RequestCancelled), message(msgAIReqStateCancelled)},
		{string(ai.RequestSkipped), message(msgAIReqStateSkipped)},
	}
}

// textOptionValid 表示 v 是选项 opts 中的一个取值。
func textOptionValid(opts []textOption, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

// integrityText 返回冻结快照的完整性问题（docsync.IntegrityIssue）的消息；没有问题时为空。
func integrityText(v *docsync.SnapshotView) uiText {
	i := v.Integrity
	if i == nil {
		return rawText(v.IntegrityError)
	}
	switch i.Code {
	case docsync.IntegrityCountMismatch:
		return message(msgIntegrityCount, intArg(argCount, int64(i.Count)), intArg(argActual, int64(i.Actual)))
	case docsync.IntegrityDocDigestInvalid:
		return message(msgIntegrityDocDigest, strArg(argDoc, i.DocID))
	case docsync.IntegrityManifestInvalid:
		return message(msgIntegrityManifest)
	case docsync.IntegrityManifestMismatch:
		return message(msgIntegrityMismatch)
	}
	return rawText(i.String())
}

// docsIssueText 返回设计依据记录不一致的原因（ai.DocsIssue）的消息（不带"记录不一致"的前缀，页面上的标题已经说明）。
func docsIssueText(e *ai.DocsIssue) uiText {
	task, item := intArg(argTask, e.TaskID), intArg(argItem, e.WorkItemID)
	switch e.Code {
	case ai.IssueSnapshotSetMissing:
		return message(msgIssueSetMissing, intArg(argSet, e.SnapshotSetID))
	case ai.IssueSnapshotSetOtherRepo:
		return message(msgIssueSetOtherRepo, intArg(argSet, e.SnapshotSetID))
	case ai.IssueSnapshotSetDigest:
		return message(msgIssueSetDigest)
	case ai.IssueRequestSetDigest:
		return message(msgIssueReqSetDigest)
	case ai.IssueRequestWorkItemSnapshot:
		return message(msgIssueReqItemSnapshot)
	case ai.IssueRequestWorkItemDigest:
		return message(msgIssueReqItemDigest)
	case ai.IssueRequestSourceUnknown:
		return message(msgIssueReqSourceUnknown)
	case ai.IssueBasisDigestInvalid:
		return message(msgIssueBasisDigest)
	case ai.IssueTaskWorkItemMissing:
		return message(msgIssueTaskItemMissing, task, item)
	case ai.IssueTaskWorkItemOtherRepo:
		return message(msgIssueTaskItemOtherRepo, task, item)
	case ai.IssueTaskWorkItemSnapshot:
		return message(msgIssueTaskItemSnapshot, task)
	case ai.IssueTaskDevRequestMismatch:
		return message(msgIssueTaskDevRequest, task, intArg(argRequest, e.RequestID))
	case ai.IssueTaskNoWorkItem:
		return message(msgIssueTaskNoWorkItem, task)
	case ai.IssueTaskClaimMismatch:
		return message(msgIssueTaskClaim, task)
	case ai.IssueTaskSpecMismatch:
		return message(msgIssueTaskSpec, task)
	case ai.IssueTaskSpecDigest:
		return message(msgIssueTaskSpecDigest, task)
	case ai.IssueTaskRefsMismatch:
		return message(msgIssueTaskRefsMismatch, task)
	case ai.IssueTaskRefsUnexpected:
		return message(msgIssueTaskRefsExtra, task)
	}
	return rawText(e.Detail())
}

// docsErrorText 返回设计依据不可用的原因：有原因码时是消息，否则（例如任务的 spec 无法解析）原样显示领域层的说明 raw。
func docsErrorText(e *ai.DocsIssue, raw string) uiText {
	if e != nil {
		return docsIssueText(e)
	}
	return rawText(raw)
}

// relationNoteMsgs 是关联任务的依据关系说明（ai.RelationNote）对应的消息。
var relationNoteMsgs = map[ai.RelationNote]msgID{
	ai.NoteBasisConflict:      msgNoteBasisConflict,
	ai.NoteBasisUnverifiable:  msgNoteBasisUnverifiable,
	ai.NoteOtherWorkItem:      msgNoteOtherWorkItem,
	ai.NoteLegacyDigestPinned: msgNoteLegacyDigestPinned,
	ai.NoteLegacyRefsMismatch: msgNoteLegacyRefsMismatch,
	ai.NoteLegacyNoneOK:       msgNoteLegacyNoneOK,
	ai.NoteLegacyNoneRefs:     msgNoteLegacyNoneRefs,
	ai.NoteClaimedDynamic:     msgNoteClaimedDynamic,
	ai.NoteLegacyPinned:       msgNoteLegacyPinned,
	ai.NoteLegacyNoBasis:      msgNoteLegacyNoBasis,
	ai.NoteLegacyInsufficient: msgNoteLegacyInsufficient,
	ai.NoteDeclaredNone:       msgNoteDeclaredNone,
	ai.NoteUnknown:            msgNoteUnknown,
	ai.NoteWorkItemNoBasis:    msgNoteWorkItemNoBasis,
	ai.NoteNoRefs:             msgNoteNoRefs,
	ai.NoteSameEmpty:          msgNoteSameEmpty,
	ai.NoteSameBasis:          msgNoteSameBasis,
	ai.NoteItemEmptyNoDigest:  msgNoteItemEmptyNoDigest,
	ai.NoteItemEmptyDigest:    msgNoteItemEmptyDigest,
	ai.NoteItemRefsMismatch:   msgNoteItemRefsMismatch,
	ai.NoteItemRefsNoDigest:   msgNoteItemRefsNoDigest,
	ai.NoteItemDigestMismatch: msgNoteItemDigestMismatch,
	ai.NoteSetEmptyNoDigest:   msgNoteSetEmptyNoDigest,
	ai.NoteSetEmptyDigest:     msgNoteSetEmptyDigest,
	ai.NoteSetRefsMismatch:    msgNoteSetRefsMismatch,
	ai.NoteSetRefsNoDigest:    msgNoteSetRefsNoDigest,
	ai.NoteSetDigestMismatch:  msgNoteSetDigestMismatch,
}

// relationNoteText 返回关联任务的依据关系说明；没有原因码或不认识时原样显示领域层的中文说明 raw。
func relationNoteText(code ai.RelationNote, raw string) uiText {
	if id, ok := relationNoteMsgs[code]; ok {
		return message(id)
	}
	return rawText(raw)
}

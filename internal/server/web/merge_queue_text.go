package web

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/autotag"
	"github.com/hewenyu/kilnflow.dev/internal/server/mergequeue"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 合并队列与自动打 tag 页面的文字（多语言，见 docs/I18N.md 4.3）：领域层（mergequeue、autotag）给出原因码和原始参数，
// 这里映射为消息。没有原因码的旧记录和不认识的原因码（升级 kiln-server 时的版本差异）原样显示领域层记下的中文说明，
// 不按中文反推原因码。

// listSep 是消息参数中列表（分支、验证、run、issue）的分隔符：参数是纯文字，不按语言连接。
const listSep = ", "

// issueList 把 issue 号写成 "#3, #4"。
func issueList(nums []int) string {
	refs := make([]string, len(nums))
	for i, n := range nums {
		refs[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(refs, listSep)
}

// runList 把没有通过的 run 写成 "CI (run #3, failed), …"（run 的名称和状态值原样显示）。
func runList(runs []mergequeue.RunRef) string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = fmt.Sprintf("%s (run #%d, %s)", r.Name, r.ID, r.State)
	}
	return strings.Join(out, listSep)
}

// conflictArgs 是冲突原因的共同参数：目标分支和它的 head（短 SHA）。
func conflictArgs(p mergequeue.NoteParams, more ...msgArg) []msgArg {
	return append([]msgArg{strArg(argBranch, p.Branch), strArg(argSHA, shorten.Hash(p.SHA))}, more...)
}

// mqNoteText 返回合并队列的状态或原因（mergequeue.Note）的消息；raw 是领域层记下的中文说明（没有原因码或者不认识时原样显示）。
func mqNoteText(n mergequeue.Note, raw string) uiText {
	if t := mqStatusText(n.Code, n.Params); !t.IsZero() {
		return t
	}
	if t := mqReasonText(n.Code, n.Params); !t.IsZero() {
		return t
	}
	return rawText(raw)
}

// mqStatusText 返回状态码 code 的消息；不是状态码时为空。
func mqStatusText(code string, p mergequeue.NoteParams) uiText {
	switch code {
	case mergequeue.StatusWaitPlan:
		return message(msgMQStatusWaitPlan, strArg(argBranch, p.Branch))
	case mergequeue.StatusConfigError:
		return message(msgMQStatusConfigError, strArg(argDetail, p.Detail))
	case mergequeue.StatusUpdating:
		return message(msgMQStatusUpdating)
	case mergequeue.StatusWaitRuns:
		return message(msgMQStatusWaitRuns, intArg(argDone, int64(p.Done)), intArg(argTotal, int64(p.Total)))
	case mergequeue.StatusPosition:
		return message(msgMQStatusPosition, intArg(argPos, int64(p.Pos)))
	case mergequeue.StatusPositionBranch:
		return message(msgMQStatusPosBranch, strArg(argBranch, p.Branch), intArg(argPos, int64(p.Pos)))
	case mergequeue.StatusRemoving:
		return message(msgMQStatusRemoving)
	case mergequeue.StatusMerging:
		return message(msgMQStatusMerging)
	case mergequeue.StatusUpdateRetry:
		return message(msgMQStatusUpdateRetry, strArg(argDetail, p.Detail))
	case mergequeue.StatusConflictReadFailed:
		return message(msgMQStatusConflictRead, strArg(argDetail, p.Detail))
	case mergequeue.StatusMergeability:
		return message(msgMQStatusMergeability, strArg(argState, p.State))
	case mergequeue.StatusBaseReadFailed:
		return message(msgMQStatusBaseRead, strArg(argDetail, p.Detail))
	case mergequeue.StatusConflictRecording:
		return message(msgMQStatusConflictRec, conflictArgs(p, intArg(argItem, p.WorkItem))...)
	case mergequeue.StatusClosingIssues:
		return message(msgMQStatusClosingIssues)
	case mergequeue.StatusIssuesDone:
		switch {
		case len(p.Closed) > 0 && len(p.Unknown) > 0:
			return message(msgMQStatusIssuesBoth, strArg(argClosed, issueList(p.Closed)), strArg(argUnknown, issueList(p.Unknown)))
		case len(p.Unknown) > 0:
			return message(msgMQStatusIssuesUnknown, strArg(argUnknown, issueList(p.Unknown)))
		}
		return message(msgMQStatusIssuesClosed, strArg(argClosed, issueList(p.Closed)))
	case mergequeue.StatusIssuesRetry:
		id := msgMQStatusIssuesRetry
		if p.Rejected {
			id = msgMQStatusIssuesRetryDeny
		}
		return message(id, intArg(argTries, int64(p.Tries)), strArg(argDetail, p.Detail))
	case mergequeue.StatusIssuesGaveUp:
		id := msgMQStatusIssuesGaveUp
		if p.Rejected {
			id = msgMQStatusIssuesGaveUpDeny
		}
		return message(id, intArg(argCount, int64(p.Tries)), strArg(argDetail, p.Detail))
	case mergequeue.StatusIssuesInactive:
		return message(msgMQStatusIssuesInactive)
	}
	return uiText{}
}

// mqReasonText 返回出队原因码 code 的消息；不是出队原因码时为空。
func mqReasonText(code string, p mergequeue.NoteParams) uiText {
	switch code {
	case mergequeue.ReasonPRClosed:
		return message(msgMQReasonPRClosed)
	case mergequeue.ReasonLabelRemoved:
		return message(msgMQReasonLabelRemoved, strArg(argLabel, mergequeue.Label))
	case mergequeue.ReasonFork:
		return message(msgMQReasonFork)
	case mergequeue.ReasonDraft:
		return message(msgMQReasonDraft)
	case mergequeue.ReasonNewCommit:
		return message(msgMQReasonNewCommit, strArg(argLabel, mergequeue.Label))
	case mergequeue.ReasonUpdateStalled:
		return message(msgMQReasonUpdateStalled, intArg(argCount, int64(p.Tries)), strArg(argDetail, p.Detail))
	case mergequeue.ReasonUpdateUnconfirmed:
		return message(msgMQReasonUpdateUnconfirmed, intArg(argCount, int64(p.Tries)), strArg(argState, p.State))
	case mergequeue.ReasonUpdateTimeout:
		return message(msgMQReasonUpdateTimeout, intArg(argCount, int64(p.Tries)))
	case mergequeue.ReasonChecksFailed:
		return message(msgMQReasonChecksFailed, strArg(argRuns, runList(p.Runs)))
	case mergequeue.ReasonNoChecks:
		return message(msgMQReasonNoChecks)
	case mergequeue.ReasonMissingRequired:
		return message(msgMQReasonMissing, strArg(argChecks, strings.Join(p.Checks, listSep)), strArg(argPath, mergecfg.Path))
	case mergequeue.ReasonRequiredFailed:
		return message(msgMQReasonRequiredFailed, strArg(argChecks, strings.Join(p.Checks, listSep)), strArg(argPath, mergecfg.Path))
	case mergequeue.ReasonBranchNotAllowed:
		return message(msgMQReasonBranch, strArg(argBranch, p.Branch), strArg(argAllowed, strings.Join(p.Allowed, listSep)))
	case mergequeue.ReasonUpdateRejected:
		return message(msgMQReasonUpdateRejected, strArg(argDetail, p.Detail))
	case mergequeue.ReasonMergeRejected:
		return message(msgMQReasonMergeRejected, strArg(argDetail, p.Detail))
	case mergequeue.ReasonConflictAuthor:
		return message(msgMQReasonConflictAuthor, conflictArgs(p)...)
	case mergequeue.ReasonConflictDev:
		return message(msgMQReasonConflictDev, conflictArgs(p, intArg(argItem, p.WorkItem))...)
	case mergequeue.ReasonConflictRetargeted:
		return message(msgMQReasonConflictRetarget, conflictArgs(p, intArg(argItem, p.WorkItem), strArg(argFromBranch, p.From))...)
	case mergequeue.ReasonConflictEnded:
		return message(msgMQReasonConflictEnded, conflictArgs(p, intArg(argItem, p.WorkItem), strArg(argState, p.State))...)
	case mergequeue.ReasonConflictDevUnavailable:
		return message(msgMQReasonConflictGone, conflictArgs(p, intArg(argItem, p.WorkItem))...)
	case mergequeue.ReasonRepoInactive:
		return message(msgMQReasonRepoInactive)
	case mergequeue.ReasonMergedOutside:
		return message(msgMQReasonMergedOutside)
	}
	return uiText{}
}

// 队列项的状态的语义级别（mergeQueueEntry.StatusTone）：页面按它决定样式，不看译文。
const (
	toneNone = ""
	// toneBad：失败（合并后关闭关联的 issue 失败、放弃，或者仓库停用没有关闭）。
	toneBad = "bad"
	// toneWarn：需要人确认（关闭部分 issue 的结果不明）。
	toneWarn = "warn"
)

// mqStatusTone 返回队列项状态的语义级别：只有已合并的项（关闭关联 issue 的进度）有级别。没有原因码的旧行按 issues_state
// 判断：放弃（failed）或者失败过、还在重试（pending 且失败次数大于 0）是失败。
func mqStatusTone(e *mergequeue.Entry) string {
	if e.State != mergequeue.StateMerged {
		return toneNone
	}
	switch e.StatusNote.Code {
	case mergequeue.StatusIssuesRetry, mergequeue.StatusIssuesGaveUp, mergequeue.StatusIssuesInactive:
		return toneBad
	case mergequeue.StatusIssuesDone:
		if len(e.StatusNote.Params.Unknown) > 0 {
			return toneWarn
		}
		return toneNone
	case "":
		if e.IssuesState == "failed" || (e.IssuesState == "pending" && e.IssuesTries > 0) {
			return toneBad
		}
	}
	return toneNone
}

// autoTagNoteText 返回自动打 tag 的原因（autotag.Note）的消息；raw 是领域层记下的中文说明（没有原因码或者不认识时原样显示）。
func autoTagNoteText(n autotag.Note, raw string) uiText {
	p := n.Params
	item := intArg(argItem, p.WorkItem)
	switch n.Code {
	case autotag.ReasonRepoInactive:
		return message(msgATReasonRepoInactive)
	case autotag.ReasonRetrying:
		return message(msgATReasonRetrying, strArg(argDetail, p.Detail))
	case autotag.ReasonConfigError:
		return message(msgATReasonConfigError, strArg(argDetail, p.Detail))
	case autotag.ReasonNoTagConfig:
		return message(msgATReasonNoTagConfig, strArg(argPath, mergecfg.Path))
	case autotag.ReasonBranchUnknown:
		return message(msgATReasonBranchUnknown, strArg(argRelease, p.Release))
	case autotag.ReasonNotTagBranch:
		return message(msgATReasonNotTagBranch, strArg(argBranch, p.Branch), strArg(argRelease, p.Release))
	case autotag.ReasonNotDefaultBranch:
		return message(msgATReasonNotDefault, strArg(argBranch, p.Branch), strArg(argRelease, p.Release))
	case autotag.ReasonSkipLabel:
		return message(msgATReasonSkipLabel, strArg(argLabel, p.Label))
	case autotag.ReasonDesignPR:
		return message(msgATReasonDesignPR, item)
	case autotag.ReasonOriginChecking:
		return message(msgATReasonOriginChecking, item)
	case autotag.ReasonOriginUnconfirmed:
		return message(msgATReasonOriginUnconfirm, item)
	case autotag.ReasonOriginPending:
		return message(msgATReasonOriginPending)
	case autotag.ReasonOriginHeadMismatch:
		return message(msgATReasonOriginHead, item)
	case autotag.ReasonOriginBaseMismatch:
		return message(msgATReasonOriginBase, item)
	case autotag.ReasonOriginStopped:
		return message(msgATReasonOriginStopped, item)
	case autotag.ReasonOriginNoChange:
		return message(msgATReasonOriginNoChange, item)
	case autotag.ReasonOriginNoRecord:
		return message(msgATReasonOriginNoRecord, item)
	case autotag.ReasonBotSender:
		return message(msgATReasonBotSender, strArg(argLogin, p.Login))
	case autotag.ReasonUnknownSender:
		return message(msgATReasonUnknownSender)
	case autotag.ReasonBotUnauthorized:
		return message(msgATReasonBotUnauthorized)
	case autotag.ReasonAlreadyTagged:
		return message(msgATReasonAlreadyTagged)
	case autotag.ReasonVersionOrder:
		return message(msgATReasonVersionOrder, strArg(argTag, p.Tag))
	case autotag.ReasonVersionOverflow:
		return message(msgATReasonVersionOverflow, strArg(argTag, p.Tag))
	case autotag.ReasonTagOccupied:
		return message(msgATReasonTagOccupied, strArg(argTag, p.Tag), strArg(argSHA, shorten.Hash(p.SHA)))
	case autotag.ReasonTagRejected:
		return message(msgATReasonTagRejected, strArg(argTag, p.Tag), strArg(argDetail, p.Detail))
	case autotag.ReasonPermission:
		return message(msgATReasonPermission, strArg(argDetail, p.Detail))
	}
	return rawText(raw)
}

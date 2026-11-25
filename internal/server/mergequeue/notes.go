package mergequeue

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 队列项的状态和原因（Dashboard 多语言，docs/I18N.md 4.3）：领域层不接收语言，给出稳定的原因码和原始参数（Note，写进
// status_code / status_params、reason_code / reason_params），由 internal/server/web 映射为消息；中文说明（Note.Text，写进
// status / reason）给 CLI、日志、审计日志和 PR / issue 评论用，与原因码来自同一个判断。升级前的旧行没有原因码（Code 为空），
// Dashboard 原样显示中文说明。

// Note 是一条状态或原因：Code 是原因码（Status*、Reason*；为空表示没有，或者是升级前的旧行），Params 是原始参数。
type Note struct {
	Code   string
	Params NoteParams
}

// NoteParams 是状态和原因的原始参数（JSONB）：只有原因码用到的字段有值。
type NoteParams struct {
	// Branch 是 PR 的目标分支；From 是 AI 开发工作项开始时的目标分支（PR 改投之前）。
	Branch string `json:"branch,omitempty"`
	From   string `json:"from,omitempty"`
	// Allowed 是配置允许合并的目标分支。
	Allowed []string `json:"allowed,omitempty"`
	// Pos 是排在第几个；Done、Total 是已结束的 run 数和全部 run 数。
	Pos   int `json:"pos,omitempty"`
	Done  int `json:"done,omitempty"`
	Total int `json:"total,omitempty"`
	// Tries 是尝试的次数（更新 PR 分支、关闭关联的 issue）。
	Tries int `json:"tries,omitempty"`
	// Detail 是原样的说明：平台或配置的错误原文；更新 PR 分支用完次数时是当时的状态（中文说明）。
	Detail string `json:"detail,omitempty"`
	// Rejected 表示平台拒绝了读取或关闭 issue（App 可能缺少 Issues 的读写权限）。
	Rejected bool `json:"rejected,omitempty"`
	// Checks 是缺少或没有成功的必需验证（workflow 或 workflow/job）；Runs 是没有通过的 run。
	Checks []string `json:"checks,omitempty"`
	Runs   []RunRef `json:"runs,omitempty"`
	// SHA 是冲突时目标分支的 head。
	SHA string `json:"sha,omitempty"`
	// WorkItem 是负责 PR 的 AI 开发工作项；State 是工作项的状态，或者 PR 的可合并状态（mergeable_state）。
	WorkItem int64  `json:"work_item,omitempty"`
	State    string `json:"state,omitempty"`
	// Closed、Unknown 是已关闭的和关闭结果不明的关联 issue。
	Closed  []int `json:"closed,omitempty"`
	Unknown []int `json:"unknown,omitempty"`
}

// RunRef 是没有通过验证的一个 run。
type RunRef struct {
	Name  string `json:"name"`
	ID    int64  `json:"id"`
	State string `json:"state"`
}

// merge_queue.status_code 的取值。
const (
	// StatusWaitPlan：等待 Kiln 按目标分支（Branch）规划 head 的验证。
	StatusWaitPlan = "wait_plan"
	// StatusConfigError：默认分支的 .kiln/merge.yml 有错误（Detail），队列保持不动。
	StatusConfigError = "config_error"
	// StatusUpdating：正在更新 PR 分支，等待新的 commit。
	StatusUpdating = "updating"
	// StatusWaitRuns：等待验证（Done/Total 个 run 已结束）。
	StatusWaitRuns = "wait_runs"
	// StatusPosition、StatusPositionBranch：不在队首，排在第 Pos 个（多个目标分支或者不是默认分支时带 Branch）。
	StatusPosition       = "position"
	StatusPositionBranch = "position_branch"
	// StatusRemoving：正在移出队列（自动出队还没完成）。
	StatusRemoving = "removing"
	// StatusMerging：正在合并。
	StatusMerging = "merging"
	// StatusUpdateRetry：更新 PR 分支暂时失败（Detail），稍后重试。
	StatusUpdateRetry = "update_retry"
	// StatusConflictReadFailed：平台报告冲突后读取 PR 失败（Detail），稍后重试。
	StatusConflictReadFailed = "conflict_read_failed"
	// StatusMergeability：平台报告冲突，但 PR 的可合并状态（State）还没确认冲突。
	StatusMergeability = "mergeability_pending"
	// StatusBaseReadFailed：读取目标分支失败（Detail），稍后重试。
	StatusBaseReadFailed = "base_read_failed"
	// StatusConflictRecording：与目标分支（Branch、SHA）冲突，等待 AI 开发记下 PR 归属（WorkItem）。
	StatusConflictRecording = "conflict_recording"
	// StatusClosingIssues：队列合并之后正在关闭关联的 issue。
	StatusClosingIssues = "closing_issues"
	// StatusIssuesDone：关联的 issue 处理完了：已关闭的（Closed）和结果不明、需要人工确认的（Unknown）。
	StatusIssuesDone = "issues_done"
	// StatusIssuesRetry、StatusIssuesGaveUp：关闭关联的 issue 失败（第 Tries 次，原因 Detail，Rejected 表示平台拒绝），
	// 稍后重试或已放弃。
	StatusIssuesRetry  = "issues_retry"
	StatusIssuesGaveUp = "issues_gave_up"
	// StatusIssuesInactive：仓库已停用，没有关闭关联的 issue。
	StatusIssuesInactive = "issues_inactive"
)

// merge_queue.reason_code 的取值。
const (
	ReasonPRClosed     = "pr_closed"
	ReasonLabelRemoved = "label_removed"
	ReasonFork         = "fork"
	ReasonDraft        = "draft"
	// ReasonNewCommit：入队后推送了新的 commit。
	ReasonNewCommit = "new_commit"
	// ReasonUpdateStalled：更新 PR 分支 Tries 次都没有完成，Detail 是当时的状态。ReasonUpdateUnconfirmed 是其中一种：
	// 平台一直没有确认 PR 的可合并状态（State）。ReasonUpdateTimeout：每次都等不到新的 commit。
	ReasonUpdateStalled     = "update_stalled"
	ReasonUpdateUnconfirmed = "update_unconfirmed"
	ReasonUpdateTimeout     = "update_timeout"
	// ReasonChecksFailed：验证没有通过（Runs）。
	ReasonChecksFailed = "checks_failed"
	// ReasonNoChecks：这个 commit 没有任何验证。
	ReasonNoChecks = "no_checks"
	// ReasonMissingRequired、ReasonRequiredFailed：缺少、没有成功的必需验证（Checks）。
	ReasonMissingRequired = "missing_required"
	ReasonRequiredFailed  = "required_failed"
	// ReasonBranchNotAllowed：目标分支（Branch）不是允许合并的分支（Allowed）。
	ReasonBranchNotAllowed = "branch_not_allowed"
	// ReasonUpdateRejected、ReasonMergeRejected：平台拒绝更新 PR 分支、拒绝合并（Detail）。
	ReasonUpdateRejected = "update_rejected"
	ReasonMergeRejected  = "merge_rejected"
	// 与目标分支（Branch、SHA）冲突：等待提交者解决；已交给 AI 开发返修（WorkItem）；PR 改投了别的目标分支（From → Branch）；
	// 负责的工作项已经结束（State）；记下交回之后工作项已经结束或者目标分支不同（ReasonConflictDevUnavailable）。
	ReasonConflictAuthor         = "conflict_author"
	ReasonConflictDev            = "conflict_dev"
	ReasonConflictRetargeted     = "conflict_retargeted"
	ReasonConflictEnded          = "conflict_ended"
	ReasonConflictDevUnavailable = "conflict_dev_unavailable"
	// ReasonRepoInactive：仓库已停用。
	ReasonRepoInactive = "repo_inactive"
	// ReasonMergedOutside：PR 在合并队列之外被合并（finishMerged 的 reason 非空表示不是队列合并的）。
	ReasonMergedOutside = "merged_outside"
)

// StatusCodes 返回全部状态码（契约测试用：Dashboard 为每一个给出消息）。
func StatusCodes() []string {
	return []string{
		StatusWaitPlan, StatusConfigError, StatusUpdating, StatusWaitRuns, StatusPosition, StatusPositionBranch, StatusRemoving,
		StatusMerging, StatusUpdateRetry, StatusConflictReadFailed, StatusMergeability, StatusBaseReadFailed, StatusConflictRecording,
		StatusClosingIssues, StatusIssuesDone, StatusIssuesRetry, StatusIssuesGaveUp, StatusIssuesInactive,
	}
}

// ReasonCodes 返回全部出队原因码（契约测试用：Dashboard 为每一个给出消息）。
func ReasonCodes() []string {
	return []string{
		ReasonPRClosed, ReasonLabelRemoved, ReasonFork, ReasonDraft, ReasonNewCommit, ReasonUpdateStalled, ReasonUpdateUnconfirmed,
		ReasonUpdateTimeout, ReasonChecksFailed, ReasonNoChecks, ReasonMissingRequired, ReasonRequiredFailed, ReasonBranchNotAllowed,
		ReasonUpdateRejected, ReasonMergeRejected, ReasonConflictAuthor, ReasonConflictDev, ReasonConflictRetargeted, ReasonConflictEnded,
		ReasonConflictDevUnavailable, ReasonRepoInactive, ReasonMergedOutside,
	}
}

// note 返回原因码 code（参数 p）。原样的说明（Detail）按保存的上限截断。
func note(code string, p NoteParams) Note {
	p.Detail = shorten.Text(p.Detail, maxReasonLen)
	return Note{Code: code, Params: p}
}

// permissionHint 是平台拒绝读取或关闭 issue 时附在原因后面的说明。
const permissionHint = "（GitHub App 需要 Issues 的读写权限，见 DEPLOY.md #3）"

// devUnavailableSuffix 是冲突交回 AI 开发时工作项已经结束或目标分支不同、改为等待人处理时附在原因后面的说明。
const devUnavailableSuffix = "；负责它的 AI 开发工作项已经结束或者目标分支不同，不自动返修，需要人处理"

// Text 返回中文说明（写进 status / reason，CLI、日志和评论用）。没有原因码时为空。
func (n Note) Text() string {
	if s, ok := statusText(n.Code, n.Params); ok {
		return s
	}
	return reasonText(n.Code, n.Params)
}

// statusText 返回状态码 code 的中文说明；ok 为 false 表示 code 不是状态码。
func statusText(code string, p NoteParams) (string, bool) {
	switch code {
	case StatusWaitPlan:
		return "等待 Kiln 按 " + p.Branch + " 规划这个 commit 的验证（接入 Kiln 或升级到合并队列之前打开的 PR 需要推送新的 commit，或关闭后重新打开）", true
	case StatusConfigError:
		return "配置错误：" + p.Detail, true
	case StatusUpdating:
		return "正在更新 PR 分支，等待新的 commit", true
	case StatusWaitRuns:
		return fmt.Sprintf("等待验证：%d/%d 个 run 已结束", p.Done, p.Total), true
	case StatusPosition:
		return fmt.Sprintf("排在第 %d 个", p.Pos), true
	case StatusPositionBranch:
		return fmt.Sprintf("目标 %s · 排在第 %d 个", p.Branch, p.Pos), true
	case StatusRemoving:
		return "正在移出队列", true
	case StatusMerging:
		return "正在合并", true
	case StatusUpdateRetry:
		return "更新 PR 分支暂时失败，稍后重试：" + p.Detail, true
	case StatusConflictReadFailed:
		return "更新 PR 分支时平台报告冲突，读取 PR 失败，稍后重试：" + p.Detail, true
	case StatusMergeability:
		return fmt.Sprintf("平台报告冲突，但 PR 的可合并状态是 %s（可能还在计算）：等待 GitHub 确认后重新更新 PR 分支", p.State), true
	case StatusBaseReadFailed:
		return "读取目标分支失败，稍后重试：" + p.Detail, true
	case StatusConflictRecording:
		return fmt.Sprintf("%s：等待 AI 开发记下 PR 归属（工作项 #%d）", conflictWhat(p), p.WorkItem), true
	case StatusClosingIssues:
		return "正在关闭关联的 issue", true
	case StatusIssuesDone:
		var parts []string
		if len(p.Closed) > 0 {
			parts = append(parts, "已关闭关联的 issue "+issueRefs(p.Closed))
		}
		if len(p.Unknown) > 0 {
			parts = append(parts, "关闭 "+issueRefs(p.Unknown)+" 的结果不明（之后读到仍然打开），没有再关闭，请人工确认")
		}
		return strings.Join(parts, "；"), true
	case StatusIssuesRetry:
		return fmt.Sprintf("关闭关联的 issue 失败（第 %d 次，稍后重试）：%s", p.Tries, issuesCause(p)), true
	case StatusIssuesGaveUp:
		return fmt.Sprintf("关闭关联的 issue 失败 %d 次，已放弃：%s", p.Tries, issuesCause(p)), true
	case StatusIssuesInactive:
		return "仓库已停用，没有关闭关联的 issue", true
	}
	return "", false
}

// reasonText 返回出队原因码 code 的中文说明；不认识时为空。
func reasonText(code string, p NoteParams) string {
	switch code {
	case ReasonPRClosed:
		return "PR 已关闭"
	case ReasonLabelRemoved:
		return "去掉了 " + Label + " label"
	case ReasonFork:
		return "fork 的 PR 不能合并"
	case ReasonDraft:
		return "PR 是 draft"
	case ReasonNewCommit:
		return "入队后推送了新的 commit，确认后重新加上 " + Label + " 即可再次入队"
	case ReasonUpdateStalled, ReasonUpdateUnconfirmed:
		return fmt.Sprintf("更新 PR 分支 %d 次都没有完成（%s）", p.Tries, p.Detail)
	case ReasonUpdateTimeout:
		return fmt.Sprintf("更新 PR 分支 %d 次都没有完成", p.Tries)
	case ReasonChecksFailed:
		runs := make([]string, len(p.Runs))
		for i, r := range p.Runs {
			runs[i] = fmt.Sprintf("%s（run #%d，%s）", r.Name, r.ID, r.State)
		}
		return "验证没有通过：" + strings.Join(runs, "、")
	case ReasonNoChecks:
		return "这个 commit 没有任何验证（没有 pull_request 的 run，或者都被跳过了，例如 PR 删掉了 .kiln/workflows 中的 workflow、改了 paths）：合并队列不合并没有经过验证的 PR"
	case ReasonMissingRequired:
		return fmt.Sprintf("缺少必需的验证：%s（默认分支的 %s 要求；PR 不能删除或改名它们）", strings.Join(p.Checks, "、"), mergecfg.Path)
	case ReasonRequiredFailed:
		return fmt.Sprintf("必需的验证没有成功：%s（默认分支的 %s 要求）", strings.Join(p.Checks, "、"), mergecfg.Path)
	case ReasonBranchNotAllowed:
		return fmt.Sprintf("目标分支是 %s，合并队列只合并到 %s", p.Branch, strings.Join(p.Allowed, "、"))
	case ReasonUpdateRejected:
		return "更新 PR 分支失败：" + p.Detail
	case ReasonMergeRejected:
		return "合并被拒绝：" + p.Detail
	case ReasonConflictAuthor:
		return conflictWhat(p) + "，请提交者把目标分支合并进来（或 rebase）解决冲突"
	case ReasonConflictDev:
		return fmt.Sprintf("%s，已交给 AI 开发返修（工作项 #%d）", conflictWhat(p), p.WorkItem)
	case ReasonConflictRetargeted:
		return fmt.Sprintf("%s；PR 的目标分支已从 AI 开发工作项 #%d 的目标分支 %s 改为 %s，不自动返修，需要人处理",
			conflictWhat(p), p.WorkItem, p.From, p.Branch)
	case ReasonConflictEnded:
		return fmt.Sprintf("%s；负责它的 AI 开发工作项 #%d 已经结束（%s），不自动返修，需要人处理", conflictWhat(p), p.WorkItem, p.State)
	case ReasonConflictDevUnavailable:
		return reasonText(ReasonConflictDev, p) + devUnavailableSuffix
	case ReasonRepoInactive:
		return "仓库已停用（GitHub App 不再能访问这个仓库），移出队列"
	case ReasonMergedOutside:
		return "PR 在合并队列之外被合并"
	}
	return ""
}

// conflictWhat 是冲突原因的开头："与目标分支 main（abc1234）冲突"。
func conflictWhat(p NoteParams) string {
	return fmt.Sprintf("与目标分支 %s（%s）冲突", p.Branch, shorten.Hash(p.SHA))
}

// issuesCause 是关闭关联的 issue 失败的原因：错误原文，平台拒绝时加上权限说明。
func issuesCause(p NoteParams) string {
	if p.Rejected {
		return p.Detail + permissionHint
	}
	return p.Detail
}

// issueRefs 把 issue 号写成 "#3、#4"。
func issueRefs(nums []int) string {
	refs := make([]string, len(nums))
	for i, n := range nums {
		refs[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(refs, "、")
}

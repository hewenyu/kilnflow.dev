package autotag

import (
	"fmt"

	"github.com/hewenyu/kilnflow.dev/internal/mergecfg"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 记录的原因（Dashboard 多语言，docs/I18N.md 4.3）：领域层不接收语言，给出稳定的原因码和原始参数（Note，写进
// auto_tags.reason_code / reason_params），由 internal/server/web 映射为消息；中文说明（写进 reason）给 CLI、日志和审计日志用，
// 与原因码来自同一个判断。升级前的旧记录没有原因码（Code 为空），Dashboard 原样显示中文说明。

// Note 是一条原因：Code 是原因码（Reason*；为空表示没有，或者是升级前的旧记录），Params 是原始参数。
type Note struct {
	Code   string
	Params NoteParams
}

// NoteParams 是原因的原始参数（JSONB）：只有原因码用到的字段有值。
type NoteParams struct {
	// Tag 是 tag 名；SHA 是占用这个 tag 名的 commit。
	Tag string `json:"tag,omitempty"`
	SHA string `json:"sha,omitempty"`
	// Branch 是 PR 的目标分支；Release 是发版分支（配置的 tag.branch，或者合并那一刻的默认分支）。
	Branch  string `json:"branch,omitempty"`
	Release string `json:"release,omitempty"`
	// Label 是跳过发布的 label；Login 是发布发起者（机器人）。
	Label string `json:"label,omitempty"`
	Login string `json:"login,omitempty"`
	// WorkItem 是 AI 开发工作项。
	WorkItem int64 `json:"work_item,omitempty"`
	// Detail 是原样的说明：平台或配置的错误原文。
	Detail string `json:"detail,omitempty"`
}

// auto_tags.reason_code 的取值。
const (
	// ReasonRepoInactive：仓库已停用，放弃还没开始的记录。
	ReasonRepoInactive = "repo_inactive"
	// ReasonRetrying：暂时失败（Detail），稍后重试。
	ReasonRetrying = "retrying"
	// ReasonConfigError：默认分支的 .kiln/merge.yml 有错误（Detail）。
	ReasonConfigError = "config_error"
	// ReasonNoTagConfig：默认分支的 .kiln/merge.yml 没有 tag:。
	ReasonNoTagConfig = "no_tag_config"
	// ReasonBranchUnknown：旧记录没有目标分支，无法确认是否合入配置的发版分支（Release）。
	ReasonBranchUnknown = "branch_unknown"
	// ReasonNotTagBranch：目标分支（Branch）不是配置的发版分支 tag.branch（Release）。
	ReasonNotTagBranch = "not_tag_branch"
	// ReasonNotDefaultBranch：省略 tag.branch 时目标分支（Branch）不是合并那一刻的默认分支（Release）。
	ReasonNotDefaultBranch = "not_default_branch"
	// ReasonSkipLabel：PR 有跳过发布的 label（Label）。
	ReasonSkipLabel = "skip_label"
	// ReasonDesignPR：AI 开发工作项（WorkItem）的设计 PR。
	ReasonDesignPR = "design_pr"
	// AI 开发工作项（WorkItem）的 PR 归属：正在核验、尚未确认（没有工作项编号时是 ReasonOriginPending）；来源分支、目标分支
	// 与记录不一致；工作项已终止、没有改动、缺少领取或交付记录。
	ReasonOriginChecking     = "origin_checking"
	ReasonOriginUnconfirmed  = "origin_unconfirmed"
	ReasonOriginPending      = "origin_pending"
	ReasonOriginHeadMismatch = "origin_head_mismatch"
	ReasonOriginBaseMismatch = "origin_base_mismatch"
	ReasonOriginStopped      = "origin_stopped"
	ReasonOriginNoChange     = "origin_no_change"
	ReasonOriginNoRecord     = "origin_no_record"
	// ReasonBotSender：发布发起者是机器人（Login），没有可信的 AI 开发实现工作项归属；ReasonUnknownSender：合并者和 PR 作者
	// 未知；ReasonBotUnauthorized：建 tag 或规划发布时发现机器人发起、又没有已记下的可信归属（恢复保护）。
	ReasonBotSender       = "bot_sender"
	ReasonUnknownSender   = "unknown_sender"
	ReasonBotUnauthorized = "bot_unauthorized"
	// ReasonAlreadyTagged：这个 commit 已经有版本 tag。
	ReasonAlreadyTagged = "already_tagged"
	// ReasonVersionOrder：最新的版本 tag（Tag）不在这个 commit 的历史中。
	ReasonVersionOrder = "version_order"
	// ReasonVersionOverflow：下一个版本（Tag）超出范围。
	ReasonVersionOverflow = "version_overflow"
	// ReasonTagOccupied：tag 名（Tag）已经指向别的 commit（SHA），重新计算。
	ReasonTagOccupied = "tag_occupied"
	// ReasonTagRejected：平台拒绝创建 tag（Tag，Detail）。
	ReasonTagRejected = "tag_rejected"
	// ReasonPermission：平台拒绝（Detail），GitHub App 需要 Contents 的读写权限。
	ReasonPermission = "permission"
)

// ReasonCodes 返回全部原因码（契约测试用：Dashboard 为每一个给出消息）。
func ReasonCodes() []string {
	return []string{
		ReasonRepoInactive, ReasonRetrying, ReasonConfigError, ReasonNoTagConfig, ReasonBranchUnknown, ReasonNotTagBranch,
		ReasonNotDefaultBranch, ReasonSkipLabel, ReasonDesignPR, ReasonOriginChecking, ReasonOriginUnconfirmed, ReasonOriginPending,
		ReasonOriginHeadMismatch, ReasonOriginBaseMismatch, ReasonOriginStopped, ReasonOriginNoChange, ReasonOriginNoRecord,
		ReasonBotSender, ReasonUnknownSender, ReasonBotUnauthorized, ReasonAlreadyTagged, ReasonVersionOrder, ReasonVersionOverflow,
		ReasonTagOccupied, ReasonTagRejected, ReasonPermission,
	}
}

// note 返回原因码 code（参数 p）。原样的说明（Detail）按保存的上限截断。
func note(code string, p NoteParams) Note {
	p.Detail = shorten.Text(p.Detail, maxReasonLen)
	return Note{Code: code, Params: p}
}

// Text 返回中文说明（写进 reason）。没有原因码时为空。AI 开发归属的原因（ReasonOrigin*，ReasonOriginPending 除外）的说明由
// AI 开发给出（DevOrigin.Reason，见 originNote），这里为空。
func (n Note) Text() string {
	p := n.Params
	switch n.Code {
	case ReasonRepoInactive:
		return "仓库已停用（GitHub App 不再能访问这个仓库）"
	case ReasonRetrying:
		return "暂时失败，稍后重试：" + p.Detail
	case ReasonConfigError:
		return "默认分支的配置错误：" + p.Detail
	case ReasonNoTagConfig:
		return "默认分支的 " + mergecfg.Path + " 没有 tag:"
	case ReasonBranchUnknown:
		return "历史记录未记下目标分支，无法确认是否合入发版分支 " + p.Release
	case ReasonNotTagBranch:
		return fmt.Sprintf("目标分支 %s 不是配置的发版分支 %s，不自动打 tag", p.Branch, p.Release)
	case ReasonNotDefaultBranch:
		return fmt.Sprintf("目标分支 %s 不是发版分支 %s，不自动打 tag", p.Branch, p.Release)
	case ReasonSkipLabel:
		return "PR 有 " + p.Label
	case ReasonDesignPR:
		return fmt.Sprintf("这是 AI 开发工作项 #%d 的设计 PR，不打版本 tag", p.WorkItem)
	case ReasonOriginPending:
		return "AI 开发工作项的 PR 创建归属正在核验，确认后可重试"
	case ReasonBotSender:
		return fmt.Sprintf("发布发起者是机器人（%s）且没有可信的 AI 开发实现工作项归属，按发布策略不自动打 tag", p.Login)
	case ReasonUnknownSender:
		return "合并者和 PR 作者未知且没有可信的 AI 开发实现工作项归属，按发布策略不自动打 tag"
	case ReasonBotUnauthorized:
		return "发布发起者是机器人且没有可信的 AI 开发实现工作项归属，按发布策略不发布"
	case ReasonAlreadyTagged:
		return "这个 commit 已经有版本 tag"
	case ReasonVersionOrder:
		return fmt.Sprintf("最新的版本 tag %s 不在这个 commit 的历史中（已有更新的提交打了 tag，或者有人在其他分支打了更大的版本）", p.Tag)
	case ReasonVersionOverflow:
		return fmt.Sprintf("下一个版本 %s 超出范围（X.Y.Z 的每个数字最多 9 位）", p.Tag)
	case ReasonTagOccupied:
		return fmt.Sprintf("tag %s 已经指向 %s，重新计算", p.Tag, shorten.Hash(p.SHA))
	case ReasonTagRejected:
		return fmt.Sprintf("创建 tag %s 被拒绝：%s", p.Tag, p.Detail)
	case ReasonPermission:
		return p.Detail + "（GitHub App 需要 Contents 的读写权限）"
	}
	return ""
}

// originNote 返回 AI 开发归属的原因：origin.ReasonCode 是 AI 开发给出的原因码（组装层换成这里的取值），没有时用 fallback。
// 中文说明用 AI 开发给出的（origin.Reason），没有时用原因码的说明。
func originNote(origin DevOrigin, fallback Note) (string, Note) {
	n := fallback
	if origin.ReasonCode != "" {
		n = Note{Code: origin.ReasonCode, Params: NoteParams{WorkItem: origin.WorkItemID}}
	}
	if origin.Reason != "" {
		if origin.ReasonCode == "" {
			// 有说明、没有原因码（AI 开发增加了新的原因）：原样显示说明
			return origin.Reason, Note{}
		}
		return origin.Reason, n
	}
	return n.Text(), n
}

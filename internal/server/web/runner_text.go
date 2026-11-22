package web

import (
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// CI Runner 与 AI 执行节点页面的文字（多语言，见 docs/I18N.md）：领域层（upgradectl、admin）给出原因码和原始参数，
// 这里映射为消息；时长按秒传给前端（{name, duration}），由前端按语言显示。

// seconds 返回时长的秒数（页面数据和提示中的时长参数）。
func seconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }

// upgradeReasonText 返回不能升级或拒绝升级操作的原因（upgradectl.Reason）的消息；零值返回空。
// 版本号和 Runner 上报的错误原文作为参数原样显示；不认识的原因码（升级 kiln-server 时的版本差异）显示原因码。
func upgradeReasonText(r upgradectl.Reason) uiText {
	switch r.Code {
	case "":
		return uiText{}
	case upgradectl.ReasonAINode:
		return message(msgUpgradeReasonAINode)
	case upgradectl.ReasonRevoked:
		return message(msgUpgradeReasonRevoked)
	case upgradectl.ReasonUnsupported:
		return message(msgUpgradeReasonUnsupported, strArg(argProgram, upgrade.ProgramCI))
	case upgradectl.ReasonNoLauncher:
		return message(msgUpgradeReasonNoLauncher, strArg(argProgram, upgrade.ProgramCI))
	case upgradectl.ReasonDisabled:
		return message(msgUpgradeReasonDisabled)
	case upgradectl.ReasonCheckFailed:
		return message(msgUpgradeReasonCheckFailed, strArg(argDetail, r.Detail))
	case upgradectl.ReasonNotChecked:
		return message(msgUpgradeReasonNotChecked)
	case upgradectl.ReasonUpToDate:
		return message(msgUpgradeReasonUpToDate)
	case upgradectl.ReasonNewer:
		return message(msgUpgradeReasonNewer, strArg(argCurrent, r.Current), strArg(argLatest, r.Latest))
	case upgradectl.ReasonUpgrading:
		return message(msgUpgradeReasonUpgrading, strArg(argVersion, r.Version))
	case upgradectl.ReasonLatestChanged:
		return message(msgUpgradeReasonLatestChanged, strArg(argLatest, r.Latest), strArg(argExpected, r.Expected))
	case upgradectl.ReasonNoPending:
		return message(msgUpgradeReasonNoPending)
	case upgradectl.ReasonSwitching:
		return message(msgUpgradeReasonSwitching, strArg(argVersion, r.Version))
	}
	return message(msgUpgradeReasonUnknown, strArg(argCode, string(r.Code)))
}

// cacheNoteText 返回清理预览的一条说明（admin.CacheNote）的消息；时间是 Unix 秒，由前端按语言和时区显示。
func cacheNoteText(n admin.CacheNote) uiText {
	switch n.Code {
	case admin.CacheNoteNoReport:
		return message(msgCacheNoteNoReport)
	case admin.CacheNoteReport:
		return message(msgCacheNoteReport, intArg(argAt, n.GeneratedAt))
	case admin.CacheNoteBusy:
		return message(msgCacheNoteBusy, strArg(argName, n.Runner), intArg(argCount, int64(n.Jobs)))
	case admin.CacheNoteSharedKept:
		return message(msgCacheNoteSharedKept)
	}
	return message(msgCacheNoteUnknown, strArg(argCode, string(n.Code)))
}

// cacheNoteTexts 返回一组预览说明的消息。
func cacheNoteTexts(notes []admin.CacheNote) []uiText {
	out := make([]uiText, 0, len(notes))
	for _, n := range notes {
		out = append(out, cacheNoteText(n))
	}
	return out
}

package web

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// Runner 详情页（PLAN #27.14）：一台 Runner 的连接和接单状态、平台、版本、容量、正在执行的任务，以及按台升级：
// 当前版本、npm 包、Runner 查到的最新正式版本，"升级到 vX.Y.Z"、取消、检查更新和最近的升级请求。
// 升级操作都经 admin.Service（与 CLI kiln runner upgrade 共用 upgradectl 的校验和审计），只有管理员可以执行。

// runnerHistoryLimit 是详情页显示的升级请求条数。
const runnerHistoryLimit = 10

// 升级操作（POST /runners/upgrade/{name}/{op}）。
const (
	upgradeOpStart  = "start"
	upgradeOpCancel = "cancel"
	upgradeOpCheck  = "check"
)

// runnerURL 返回 Runner 详情页的地址。
func runnerURL(name string) string { return "/runners/detail/" + url.PathEscape(name) }

// runnerPage 是 Runner 详情页的数据。
type runnerPage struct {
	Runner *runnerView
	// History 是最近的升级请求（新的在前）。
	History []*upgradeRequestView
	// Registry 是 Runner 查询和下载升级包用的 registry；RegistryDefault 表示是官方 registry（没有设置）。
	Registry        string
	RegistryDefault bool
	// OfflineAfter、LongPoll 是页面说明中用到的时长（秒）。
	OfflineAfter, LongPoll int64
}

func (d *Dashboard) pageRunner(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	name := r.PathValue("name")
	v, err := d.runnerByName(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		// AI 执行节点的详情在 AI 执行节点页面
		if a, err := d.aiRunnerByName(ctx, name); err == nil && a != nil {
			d.redirect(w, r, aiRunnerURL(name))
			return
		}
		d.notFoundText(w, r, message(msgRunnerNotFound, strArg(argName, name)))
		return
	}
	reqs, err := upgradectl.History(ctx, d.db, v.ID, runnerHistoryLimit)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	ov, err := d.admin.GetRunnerUpgrade(ctx)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &runnerPage{
		Runner: v, Registry: upgrade.Registry(ov.Registry), RegistryDefault: ov.Registry == "",
		OfflineAfter: seconds(admin.RunnerOfflineAfter), LongPoll: seconds(d.longPoll()),
	}
	for _, q := range reqs {
		p.History = append(p.History, newUpgradeRequestView(q))
	}
	d.renderPage(w, r, "runner", message(msgRunnerTitle, strArg(argName, name)), p, http.StatusOK)
}

// postRunnerUpgradeOp 是一台 Runner 的升级操作：升级到它查到的最新版本（start）、取消进行中的升级（cancel）、
// 立即查询最新版本（check）。完成后回到详情页并给出结果。
func (d *Dashboard) postRunnerUpgradeOp(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	name, op := r.PathValue("name"), r.PathValue("op")
	v, err := d.runnerTarget(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	back := runnerURL(name)
	if v != nil && v.AI {
		back = aiRunnerURL(name)
	}
	var msg uiText
	switch op {
	case upgradeOpStart:
		req, created, err := d.admin.RequestRunnerUpgrade(ctx, name, r.PostFormValue("version"), se.Login)
		switch {
		case err != nil:
			msg = d.upgradeFailure(r, err, name, msgUpgradeStartFailed, msgUpgradeStartFailedRef)
		case !created:
			msg = message(msgUpgradeAlready, strArg(argName, name), strArg(argVersion, req.Version))
		case v != nil && !v.Online:
			msg = message(msgUpgradeRequestedOff, strArg(argName, name), strArg(argVersion, req.Version))
		default:
			msg = message(msgUpgradeRequested, strArg(argName, name), strArg(argVersion, req.Version))
		}
	case upgradeOpCancel:
		req, err := d.admin.CancelRunnerUpgrade(ctx, name, se.Login)
		if err != nil {
			msg = d.upgradeFailure(r, err, name, msgUpgradeCancelFailed, msgUpgradeCancelFailedRef)
		} else {
			msg = message(msgUpgradeCancelled, strArg(argName, name), strArg(argVersion, req.Version))
		}
	case upgradeOpCheck:
		if _, err := d.admin.CheckRunnerUpgrade(ctx, name, se.Login); err != nil {
			msg = d.upgradeFailure(r, err, name, msgUpgradeCheckFailed, msgUpgradeCheckFailedRef)
		} else {
			msg = message(msgUpgradeCheckRequested, strArg(argName, name), intArg(argPoll, seconds(d.longPoll())))
		}
	default:
		d.notFoundText(w, r, message(msgUpgradeOpUnknown))
		return
	}
	d.flashMsg(w, r, back, msg)
}

// upgradeFailure 返回升级操作失败后的提示：Runner 当前的状态不允许（已是最新、需要 service install 等）时按 upgradectl 的
// 原因码显示原因；Runner 不存在时说明；参数错误和其他错误按 failureFlash 处理（withReason 带原样的说明，withRef 带错误编号）。
func (d *Dashboard) upgradeFailure(r *http.Request, err error, name string, withReason, withRef msgID) uiText {
	if re, ok := errors.AsType[*upgradectl.RefusedError](err); ok && !re.Reason.IsZero() {
		return upgradeReasonText(re.Reason)
	}
	if errors.Is(err, upgradectl.ErrRunnerNotFound) {
		return message(msgUpgradeRunnerMissing, strArg(argName, name))
	}
	return d.failureFlash(r, err, withReason, withRef)
}

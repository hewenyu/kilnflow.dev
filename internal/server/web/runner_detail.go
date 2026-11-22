package web

import (
	"errors"
	"fmt"
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
	// OfflineAfter、LongPoll 是页面说明中用到的时长。
	OfflineAfter, LongPoll string
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
		d.notFound(w, r, fmt.Sprintf("没有名为 %s 的 Runner。", name))
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
		OfflineAfter: humanDuration(admin.RunnerOfflineAfter), LongPoll: humanDuration(d.longPoll()),
	}
	for _, q := range reqs {
		p.History = append(p.History, newUpgradeRequestView(q))
	}
	d.render(w, r, "runner", "Runner · "+name, p)
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
	var msg string
	switch op {
	case upgradeOpStart:
		req, created, err := d.admin.RequestRunnerUpgrade(ctx, name, r.PostFormValue("version"), se.Login)
		switch {
		case err != nil:
			msg = "没有升级：" + d.upgradeFailure(r, err)
		case !created:
			msg = fmt.Sprintf("%s 已经在升级到 %s，没有重复请求。", name, req.Version)
		default:
			msg = fmt.Sprintf("已请求把 %s 升级到 %s：Runner 下载、核对后等正在执行的任务结束再切换，新版本启动并确认后完成。", name, req.Version)
			if v != nil && !v.Online {
				msg += "它目前离线，重新连接后执行。"
			}
		}
	case upgradeOpCancel:
		req, err := d.admin.CancelRunnerUpgrade(ctx, name, se.Login)
		if err != nil {
			msg = "没有取消：" + d.upgradeFailure(r, err)
		} else {
			msg = fmt.Sprintf("已取消 %s 升级到 %s；已经下载的包保留，之后再升级时直接使用。", name, req.Version)
		}
	case upgradeOpCheck:
		if _, err := d.admin.CheckRunnerUpgrade(ctx, name, se.Login); err != nil {
			msg = "没有检查：" + d.upgradeFailure(r, err)
		} else {
			msg = fmt.Sprintf("已请求 %s 查询最新版本：它下一次连接 Server 时查询（在线时最长约 %s）。", name, humanDuration(d.longPoll()))
		}
	default:
		d.notFound(w, r, "没有这个升级操作，没有执行任何操作。")
		return
	}
	d.flashBack(w, r, back, msg)
}

// upgradeFailure 返回升级操作失败的说明：Runner 当前的状态不允许（已是最新、需要 service install 等）、参数错误时
// 直接显示原因，其他错误按 failure 处理。
func (d *Dashboard) upgradeFailure(r *http.Request, err error) string {
	if re, ok := errors.AsType[*upgradectl.RefusedError](err); ok {
		return re.Msg
	}
	if ie, ok := errors.AsType[*upgradectl.InvalidError](err); ok {
		return ie.Msg
	}
	if errors.Is(err, upgradectl.ErrRunnerNotFound) {
		return "没有这台 Runner"
	}
	return d.failure(r, err)
}

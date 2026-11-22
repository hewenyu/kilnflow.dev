package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// 设置 · Runner 升级（PLAN #27.14）：/settings/runner-upgrade 设置 Runner 查询和下载升级包用的 npm registry（留空用官方），
// 列出每台 Runner 的版本、查到的最新版本和升级请求（升级在 Runner 详情页按台操作），可以让全部 Runner 立即检查更新。
// 写操作都经 admin.Service（与 CLI kiln runner upgrade 共用 upgradectl 的校验和审计，改完唤醒 Runner），只有管理员可以访问。

// runnerUpgradeURL 是 Runner 升级设置页的地址。
const runnerUpgradeURL = "/settings/runner-upgrade"

// sectionUpgrade 是 Runner 升级的设置分区。
const sectionUpgrade = "upgrade"

// runnerUpgradePage 是 Runner 升级设置页的数据。
type runnerUpgradePage struct {
	settingsBase
	// Registry 是设置的 registry（空表示官方 registry），DefaultRegistry 是官方 registry 的地址。
	Registry, DefaultRegistry string
	// Runners 是没有吊销的 Runner（CI Runner 在前，AI 执行节点在后；版本、最新版本和升级请求）。
	Runners []*runnerView
	// Form 是 registry 表单的值和错误。
	Form *formState
}

// loadRunnerUpgrade 读取页面数据；f 不为 nil 时（校验失败）替换按当前设置预填的表单。
func (d *Dashboard) loadRunnerUpgrade(ctx context.Context, f *formState) (*runnerUpgradePage, error) {
	ov, err := d.admin.GetRunnerUpgrade(ctx)
	if err != nil {
		return nil, err
	}
	p := &runnerUpgradePage{Section: sectionUpgrade, Registry: ov.Registry, DefaultRegistry: ov.DefaultRegistry, Form: f}
	if p.Form == nil {
		p.Form = newForm()
		p.Form.Values["registry"] = ov.Registry
	}
	all, err := d.loadRunners(ctx, store.RunnerFilter{})
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		if !v.Revoked {
			p.Runners = append(p.Runners, v)
		}
	}
	// AI 执行节点不在这里：它们没有按台升级（连接器用 npm 升级，PLAN #33.3）
	return p, nil
}

func (d *Dashboard) renderRunnerUpgrade(w http.ResponseWriter, r *http.Request, f *formState, status int) {
	p, err := d.loadRunnerUpgrade(r.Context(), f)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	d.renderStatus(w, r, "settings_upgrade", "设置 · Runner 升级", p, status)
}

func (d *Dashboard) pageRunnerUpgrade(w http.ResponseWriter, r *http.Request, _ *session) {
	d.renderRunnerUpgrade(w, r, nil, http.StatusOK)
}

// postRunnerUpgradeRegistry 设置 registry；留空表示官方 registry。
func (d *Dashboard) postRunnerUpgradeRegistry(w http.ResponseWriter, r *http.Request, se *session) {
	f := newForm()
	registry := strings.TrimSpace(r.PostFormValue("registry"))
	f.Values["registry"] = registry
	if registry != "" {
		if err := npm.CheckRegistry(registry); err != nil {
			f.fail("registry", err.Error())
		}
	}
	if !f.Failed() {
		if err := d.admin.SetRunnerUpgradeRegistry(r.Context(), registry, se.Login); err != nil {
			f.fail("registry", d.upgradeFailure(r, err))
		}
	}
	if f.Failed() {
		d.renderRunnerUpgrade(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	if registry == "" {
		d.flashBack(w, r, runnerUpgradeURL, "Runner 使用官方 registry（"+upgrade.DefaultRegistry+"）查询和下载升级包")
		return
	}
	d.flashBack(w, r, runnerUpgradeURL, "Runner 使用 "+registry+" 查询和下载升级包，下一次连接 Server 时重新查询最新版本")
}

// postRunnerUpgradeCheck 让全部没有吊销的 Runner 立即查询最新版本。
func (d *Dashboard) postRunnerUpgradeCheck(w http.ResponseWriter, r *http.Request, se *session) {
	n, err := d.admin.CheckRunnerUpgrade(r.Context(), "", se.Login)
	if err != nil {
		d.flashBack(w, r, runnerUpgradeURL, "没有检查："+d.upgradeFailure(r, err))
		return
	}
	d.flashBack(w, r, runnerUpgradeURL, fmt.Sprintf("已请求 %d 台 Runner 查询最新版本：在线的 Runner 下一次连接 Server 时查询（最长约 %s）", n, humanDuration(d.longPoll())))
}

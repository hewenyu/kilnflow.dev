package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 总览（/）：登录后的首页。回答"现在在烧什么、最近有没有坏、窑够不够用"：
// 进行中和排队的 run、过去 24 小时的结果统计、最近失败的 run、自己最近发起的 run、Runner 概况；
// 管理员还没完成初始设置（绑定 GitHub 账号、注册 Runner、同步仓库、关闭 admin token 登录）时，在最上方列出待办。

const (
	// overviewActiveLimit 是总览中运行中、排队中各列出的 run 数上限。
	overviewActiveLimit = 10
	// overviewRecentLimit 是总览中最近失败、我发起的各列出的 run 数上限。
	overviewRecentLimit = 5
	// overviewRunnersLimit 是总览中列出的 Runner 数上限。
	overviewRunnersLimit = 8
	// overviewWindow 是总览统计的时间窗口。
	overviewWindow = 24 * time.Hour
)

// overviewStats 是过去 overviewWindow 内创建的 run 的统计。
type overviewStats struct {
	Total, Succeeded, Failed, Cancelled, Active int
}

// Rate 返回成功率（已结束的 run 中成功的比例，取整的百分数）；没有已结束的 run 时返回空。
func (s overviewStats) Rate() string {
	done := s.Succeeded + s.Failed + s.Cancelled
	if done == 0 {
		return ""
	}
	return strconv.Itoa(s.Succeeded*100/done) + "%"
}

// setupItem 是总览中的一项初始设置。
type setupItem struct {
	// Key 是这一项的稳定标识（前端列表的 key，不随语言变化）。
	Key         string
	Title, Text uiText
	// URL 是去完成这一项的页面；Action 是按钮文字。
	URL    string
	Action uiText
	Done   bool
}

// overviewPage 是总览的数据。
type overviewPage struct {
	// Me 是当前用户的 GitHub 登录名；没有绑定 GitHub 账号的 admin token 登录为空。
	Me              string
	Running, Queued []runView
	Failures, Mine  []runView
	Stats           overviewStats
	Runners         []*runnerView
	RunnerCounts    runnerCounts
	// MoreRunners 是没有列出的 Runner 数。
	MoreRunners int
	// AIRunnerCounts 是 AI 执行节点的统计（AI 执行节点不在 Runners 中，见 AI 执行节点页面）。
	AIRunnerCounts aiRunnerCounts
	// Setup 是还没完成的初始设置（只给管理员看，全部完成后为空）；SetupDone 是已完成的项数。
	Setup     []setupItem
	SetupDone int
	MineURL   string
}

func (d *Dashboard) pageOverview(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	p := &overviewPage{Me: se.githubLogin()}
	if p.Me != "" {
		p.MineURL = runFilter{Actor: p.Me}.url("", 0)
	}
	type runList struct {
		dst *[]runView
		f   cistore.RunFilter
	}
	lists := []runList{
		{&p.Running, cistore.RunFilter{State: store.RunActive, Limit: overviewActiveLimit}},
		{&p.Queued, cistore.RunFilter{State: store.RunQueued, Limit: overviewActiveLimit}},
		{&p.Failures, cistore.RunFilter{State: store.RunFailed, Limit: overviewRecentLimit}},
	}
	if p.Me != "" {
		lists = append(lists, runList{&p.Mine, cistore.RunFilter{Actor: p.Me, Limit: overviewRecentLimit}})
	}
	for _, l := range lists {
		runs, names, err := cistore.QueryRuns(ctx, d.db, &l.f)
		if err != nil {
			d.serverError(w, r, err)
			return
		}
		if *l.dst, err = d.runViews(ctx, runs, names); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	counts, err := cistore.RunCounts(ctx, d.db, time.Now().Add(-overviewWindow))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	s := &p.Stats
	for state, n := range counts {
		s.Total += n
		switch state {
		case store.RunSucceeded:
			s.Succeeded += n
		case store.RunFailed:
			s.Failed += n
		case store.RunCancelled:
			s.Cancelled += n
		case store.RunQueued, store.RunActive:
			s.Active += n
		}
	}
	runners, err := d.loadRunners(ctx, store.RunnerFilter{})
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.RunnerCounts = countRunners(runners)
	aiRunners, err := d.loadAIRunners(ctx, 0)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p.AIRunnerCounts = countAIRunners(aiRunners)
	for _, v := range runners {
		if v.Revoked {
			continue
		}
		if len(p.Runners) == overviewRunnersLimit {
			p.MoreRunners++
			continue
		}
		p.Runners = append(p.Runners, v)
	}
	if se.Admin() {
		if err := d.overviewSetup(ctx, p); err != nil {
			d.serverError(w, r, err)
			return
		}
	}
	// 两列都没有达到上限时，Running+Queued 就是全部活跃 run（QueryRuns 每个状态最多取 overviewActiveLimit 个）；
	// 交给 renderPage 复用，省掉同一次渲染里再查一次 count。达到上限时不知道上限之外还有多少，照常查询。
	if len(p.Running) < overviewActiveLimit && len(p.Queued) < overviewActiveLimit {
		r = r.WithContext(withActiveRuns(r.Context(), len(p.Running)+len(p.Queued)))
	}
	d.renderPage(w, r, "overview", message(msgNavOverview), p, http.StatusOK)
}

// overviewSetup 检查初始设置的完成情况；全部完成时 p.Setup 为空。
func (d *Dashboard) overviewSetup(ctx context.Context, p *overviewPage) error {
	bind, err := d.adminBinding(ctx)
	if err != nil {
		return err
	}
	var repos int
	if err := d.db.QueryRow(ctx, `SELECT count(*) FROM repos WHERE active`).Scan(&repos); err != nil {
		return err
	}
	items := []setupItem{
		{
			Key: "repos", Title: message(msgSetupReposTitle), Text: message(msgSetupReposText),
			URL: "/settings/repos", Action: message(msgSetupReposAction), Done: repos > 0,
		},
		{
			Key: "runner", Title: message(msgSetupRunnerTitle), Text: message(msgSetupRunnerText),
			URL: "/runners/new", Action: message(msgSetupRunnerAction), Done: p.RunnerCounts.Total > p.RunnerCounts.Revoked,
		},
		{
			Key: "bind", Title: message(msgSetupBindTitle), Text: message(msgSetupBindText),
			URL: accountURL, Action: message(msgSetupBindAction), Done: bind != nil,
		},
		{
			Key: "token", Title: message(msgSetupTokenTitle), Text: message(msgSetupTokenText),
			URL: accountURL, Action: message(msgSetupTokenAction), Done: bind != nil && !bind.TokenLogin,
		},
	}
	for _, it := range items {
		if it.Done {
			p.SetupDone++
		}
	}
	if p.SetupDone < len(items) {
		p.Setup = items
	}
	return nil
}

// countRunners 统计 Runner 的连接和接单状态。
func countRunners(all []*runnerView) runnerCounts {
	var c runnerCounts
	for _, v := range all {
		c.Total++
		c.Jobs += len(v.Jobs)
		switch {
		case v.Revoked:
			c.Revoked++
		case !v.Online:
			c.Offline++
		default:
			c.Online++
			if v.Drain {
				c.Paused++
			} else {
				c.Accepting++
			}
		}
	}
	return c
}

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 执行节点页面（PLAN #30.9、PLAN #33）：AI 执行节点（执行机器上的 Orca Server + kiln-orca 连接器）与 CI Runner 分开列出。
// 节点的连接和接单状态与 CI Runner 相同；另外显示角色、平台、协议版本、并发上限、Orca Server / Docker 的就绪状态、本机 Agent
// 的状态和正在执行的 AI 任务，以及每个任务在 Orca 中的关联。暂停、恢复、吊销、删除与 CI Runner 共用表单操作
// （POST /runners/{name}/{op}），完成后回到这个页面；AI 执行节点没有按台升级（连接器用 npm 升级，PLAN #33.3）。

// aiRunnersPath 是 AI 执行节点页面的地址。
const aiRunnersPath = "/ai/runners"

// aiRunnerURL 返回 AI 执行节点详情页的地址。
func aiRunnerURL(name string) string { return aiRunnersPath + "/" + url.PathEscape(name) }

// aiRunningTask 是 AI 执行节点正在执行的一个 AI 任务。
type aiRunningTask struct {
	RunnerID int64
	TaskID   int64
	// RequestID、Type 是任务所属的请求和类型（review、develop）。
	RequestID int64
	Type      string
	Name      string
	Repo      string
	// Orca 是这次执行在 Orca 中的关联（PLAN #33.4）；Stage、Text 是心跳上报的进度。
	Orca  ai.OrcaBinding
	Stage string
	Text  string
	// TaskURL 是任务详情页的地址（Orca 的关联在那里查看）。
	TaskURL string
	Started time.Time
}

// aiAgentView 是 AI 执行节点本机的一个 Agent（PLAN #30.7 的显示状态）。
type aiAgentView struct {
	Agent, Name, Version string
	// State 是 available、unavailable、backoff 或 unknown；Reason 是不可用、退避或未知的原因。
	State, Reason string
	CheckedAt     *time.Time
	BackoffUntil  *time.Time
}

// aiRunnerView 是页面上的一台 AI 执行节点。
type aiRunnerView struct {
	ID   int64
	Name string
	// Role 是 review（审计）或 development（开发），角色的名称由前端按语言显示。
	Role     string
	Version  string
	Platform string
	// Protocol 是上报的 AI 协议版本；NeedsUpgrade 表示不是 Server 支持的版本（不派任务）。
	Protocol     int
	NeedsUpgrade bool
	// HostID 是节点上报的机器标识（只用于展示）。
	HostID  string
	Revoked bool
	Drain   bool
	Online  bool
	// Capacity 是同时执行的任务数上限。
	Capacity int
	LastSeen *time.Time
	// OrcaState、DockerState 是 ready、unavailable 或 unknown；*Reason 是不可用的原因。
	OrcaState, OrcaReason      string
	OrcaVersion, OrcaRuntimeID string
	DockerState, DockerReason  string
	// ToolchainState、ToolchainReason 是受管理的工具链的就绪状态（PLAN #33.12）。
	ToolchainState, ToolchainReason string
	// ReportedAt 是最近一次上报状态的时间。
	ReportedAt *time.Time
	Agents     []aiAgentView
	Tasks      []aiRunningTask
}

// Accepting 表示 AI 执行节点在线并且接单。
func (v *aiRunnerView) Accepting() bool { return !v.Revoked && v.Online && !v.Drain }

// ConnClass 返回连接状态的徽章样式（与 CI Runner 相同）。
func (v *aiRunnerView) ConnClass() string { return connClass(v.Revoked, v.Online) }

func newAIRunnerView(r *admin.AIRunner) *aiRunnerView {
	v := &aiRunnerView{
		ID: r.ID, Name: r.Name, Role: r.Role, Version: r.Version, Platform: r.Platform,
		Protocol: r.Protocol, NeedsUpgrade: r.NeedsUpgrade, HostID: r.HostID, Capacity: r.Capacity,
		Revoked: r.State == store.RunnerRevoked, Drain: r.Drain, Online: r.Online, LastSeen: r.LastSeen,
		OrcaState: r.OrcaState, OrcaReason: r.OrcaReason, OrcaVersion: r.OrcaVersion, OrcaRuntimeID: r.OrcaRuntimeID,
		DockerState: r.DockerState, DockerReason: r.DockerReason, ToolchainState: r.ToolchainState, ToolchainReason: r.ToolchainReason,
		ReportedAt: r.ReportedAt, Agents: make([]aiAgentView, 0, len(r.Agents)),
	}
	for _, a := range r.Agents {
		v.Agents = append(v.Agents, aiAgentView{
			Agent: a.Agent, Name: a.Name, Version: a.Version, State: a.Show, Reason: a.Reason, CheckedAt: a.CheckedAt, BackoffUntil: a.BackoffUntil,
		})
	}
	return v
}

// loadAIRunners 返回 AI 执行节点（id 不为 0 时只返回这一台）及其正在执行的 AI 任务，按名称排序。
func (d *Dashboard) loadAIRunners(ctx context.Context, id int64) ([]*aiRunnerView, error) {
	list, err := d.admin.ListAIRunners(ctx, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	out := make([]*aiRunnerView, len(list))
	ids := make([]int64, len(list))
	byID := make(map[int64]*aiRunnerView, len(list))
	for i, r := range list {
		out[i] = newAIRunnerView(r)
		ids[i] = r.ID
		byID[r.ID] = out[i]
	}
	rows, err := d.db.Query(ctx, `SELECT a.runner_id, t.id, t.request_id, q.type, t.display_name, p.owner || '/' || p.name,
		a.orca, a.progress, a.started_at
		FROM ai_tasks t JOIN ai_attempts a ON a.id=t.current_attempt_id JOIN ai_requests q ON q.id=t.request_id JOIN repos p ON p.id=q.repo_id
		WHERE t.state='running' AND a.runner_id = ANY($1) ORDER BY a.started_at, t.id`, ids)
	if err != nil {
		return nil, err
	}
	var orca, progress []byte
	var cur aiRunningTask
	if _, err := pgx.ForEachRow(rows, []any{
		&cur.RunnerID, &cur.TaskID, &cur.RequestID, &cur.Type,
		&cur.Name, &cur.Repo, &orca, &progress, &cur.Started,
	}, func() error {
		a := &ai.Attempt{Orca: orca, Progress: progress}
		b, err := a.DecodeOrca()
		if err != nil {
			return err
		}
		pr, err := a.DecodeProgress()
		if err != nil {
			return err
		}
		cur.Orca, cur.Stage, cur.Text = b, pr.Stage, pr.Text
		cur.TaskURL = fmt.Sprintf("%s/ai/tasks/%d", d.cfg.PublicURL, cur.TaskID)
		byID[cur.RunnerID].Tasks = append(byID[cur.RunnerID].Tasks, cur)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// aiRunnerByName 返回名为 name 的 AI 执行节点，不存在（或是 CI Runner）时返回 nil。
func (d *Dashboard) aiRunnerByName(ctx context.Context, name string) (*aiRunnerView, error) {
	var id int64
	err := d.db.QueryRow(ctx, `SELECT id FROM runners WHERE name=$1 AND kind=$2`, name, store.RunnerKindAI).Scan(&id)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := d.loadAIRunners(ctx, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// aiRunnerCounts 是全部 AI 执行节点的统计。
type aiRunnerCounts struct {
	Total, Online, Accepting, Paused, Offline, Revoked, Tasks int
}

func countAIRunners(all []*aiRunnerView) aiRunnerCounts {
	var c aiRunnerCounts
	for _, v := range all {
		c.Total++
		c.Tasks += len(v.Tasks)
		switch {
		case v.Revoked:
			c.Revoked++
			continue
		case v.Online:
			c.Online++
		default:
			c.Offline++
		}
		if v.Drain {
			c.Paused++
		} else if v.Online {
			c.Accepting++
		}
	}
	return c
}

// aiRunnersPage 是 AI 执行节点页面的数据。
type aiRunnersPage struct {
	Runners []*aiRunnerView
	Counts  aiRunnerCounts
	// Self 是当前页面的地址，操作后回到这里。
	Self string
	// OfflineAfter、LeaseTTL、LongPoll 是页面说明中用到的时长（秒）。
	OfflineAfter, LeaseTTL, LongPoll int64
}

func (d *Dashboard) pageAIRunners(w http.ResponseWriter, r *http.Request, _ *session) {
	all, err := d.loadAIRunners(r.Context(), 0)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &aiRunnersPage{
		Runners: all, Counts: countAIRunners(all), Self: aiRunnersPath,
		OfflineAfter: seconds(admin.RunnerOfflineAfter), LeaseTTL: seconds(d.leaseTTL()), LongPoll: seconds(d.longPoll()),
	}
	d.renderPage(w, r, "ai_runners", message(msgAIRunnersTitle), p, http.StatusOK)
}

// aiRunnerPage 是 AI 执行节点详情页的数据（没有按台升级，PLAN #33.3）；OfflineAfter、LongPoll 是时长（秒）。
type aiRunnerPage struct {
	Runner                 *aiRunnerView
	OfflineAfter, LongPoll int64
}

func (d *Dashboard) pageAIRunner(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	name := r.PathValue("name")
	v, err := d.aiRunnerByName(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		// CI Runner 的详情在 CI 的 Runner 页面
		if ci, err := d.runnerByName(ctx, name); err == nil && ci != nil {
			d.redirect(w, r, runnerURL(name))
			return
		}
		d.notFoundText(w, r, message(msgAIRunnerNotFound, strArg(argName, name)))
		return
	}
	p := &aiRunnerPage{
		Runner: v, OfflineAfter: seconds(admin.RunnerOfflineAfter), LongPoll: seconds(d.longPoll()),
	}
	d.renderPage(w, r, "ai_runner", message(msgAIRunnerTitle, strArg(argName, name)), p, http.StatusOK)
}

// asRunnerView 把 AI 执行节点换成注册页使用的 Runner 摘要（名称、连接和接单状态、平台、版本；没有 pool 和 executor，
// 也没有按台升级）。
func (v *aiRunnerView) asRunnerView() *runnerView {
	rv := &runnerView{
		ID: v.ID, Name: v.Name, Kind: store.RunnerKindAI, Role: v.Role, URL: aiRunnerURL(v.Name), Version: v.Version, Revoked: v.Revoked, Drain: v.Drain,
		Online: v.Online, LastSeen: v.LastSeen,
	}
	if v.Platform != "" {
		rv.Platforms = []string{v.Platform}
	}
	return rv
}

// opTarget 是 Runner 操作的对象：CI Runner 或 AI 执行节点的共同部分。
type opTarget struct {
	Name    string
	AI      bool
	Revoked bool
	Online  bool
	// Running 是正在执行的任务数（CI 的 job 或 AI 任务）。
	Running int
}

// runnerTarget 按名称找到 Runner（CI 或 AI），不存在时返回 nil。
func (d *Dashboard) runnerTarget(ctx context.Context, name string) (*opTarget, error) {
	v, err := d.runnerByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if v != nil {
		return &opTarget{Name: v.Name, Revoked: v.Revoked, Online: v.Online, Running: len(v.Jobs)}, nil
	}
	a, err := d.aiRunnerByName(ctx, name)
	if err != nil || a == nil {
		return nil, err
	}
	return &opTarget{Name: a.Name, AI: true, Revoked: a.Revoked, Online: a.Online, Running: len(a.Tasks)}, nil
}

// MarshalJSON 输出 AI 执行节点，加上接单和连接状态（与 runnerView 相同的计算值）。
func (v *aiRunnerView) MarshalJSON() ([]byte, error) {
	type plain aiRunnerView
	return json.Marshal(struct {
		*plain
		Accepting bool
		ConnClass string
	}{(*plain)(v), v.Accepting(), v.ConnClass()})
}

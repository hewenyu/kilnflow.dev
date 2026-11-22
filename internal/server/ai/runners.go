package ai

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 执行节点的状态（PLAN #33.4、#33.6）：ai_runners 是角色、平台、协议版本、并发上限、Orca / Docker 的就绪状态；
// ai_runner_agents 是这个节点对外提供的每个 Agent 的检测结果。节点是 Orca Server + kiln-orca 连接器，没有 Herdr、
// 没有主机指纹，也不需要按台升级（连接器用 npm 升级，PLAN #33.3）。

// 就绪状态（ai_runners.orca_state、docker_state）。
const (
	ReadyStateReady       = "ready"
	ReadyStateUnavailable = "unavailable"
	ReadyStateUnknown     = "unknown"
)

// EnrollHook 是 AI 执行节点注册时写入 ai_runners 的钩子（runnerauth.EnrollHook）：同名的节点换了角色时拒绝
// （换角色要用新名字注册，PLAN #26.7）。
func EnrollHook(ctx context.Context, tx pgx.Tx, e runnerauth.Enrolled) error {
	role, err := store.RunnerRole(store.RunnerKindAI, e.Role)
	if err != nil {
		return runnerauth.Reject("%s", err.Error())
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT role FROM ai_runners WHERE runner_id=$1 FOR UPDATE`, e.ID).Scan(&existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO ai_runners (runner_id, role) VALUES ($1, $2)`, e.ID, role)
		return err
	case err != nil:
		return err
	case existing != role:
		return runnerauth.Reject("名称 %s 已被一台%s使用，不能用%s的令牌重新注册；请换一个名称", e.Name, RoleLabel(existing), RoleLabel(role))
	}
	// 重新注册同一台机器：清掉旧的上报，等它重新上报
	_, err = tx.Exec(ctx, `UPDATE ai_runners SET protocol=0, orca_state='unknown', orca_reason='', docker_state='unknown', docker_reason='',
		toolchain_state='unknown', toolchain_reason='', running=0, reported_at=NULL WHERE runner_id=$1`, e.ID)
	return err
}

// RoleLabel 返回 AI 执行节点角色的显示名称（前后带空格，用在句子中）。
func RoleLabel(role string) string {
	if role == store.RunnerRoleDevelopment {
		return " AI 开发节点 "
	}
	return " AI 审计节点 "
}

// touchRunner 记录 AI 执行节点在线和这次上报的状态，返回领取时用到的节点信息。st 为 nil 时只记录在线。
func (c *Core) touchRunner(ctx context.Context, runnerID int64, st *airunnerv2.AIRunnerState) (*Runner, int, bool, error) {
	var r *Runner
	var protocol int
	var drain bool
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		r, protocol, drain, err = c.touchRunnerTx(ctx, tx, runnerID, st)
		return err
	})
	return r, protocol, drain, err
}

func (c *Core) touchRunnerTx(ctx context.Context, tx pgx.Tx, runnerID int64, st *airunnerv2.AIRunnerState) (*Runner, int, bool, error) {
	r := &Runner{ID: runnerID, Agents: map[string]bool{}, AgentReasons: map[string]string{}}
	var drain bool
	if err := tx.QueryRow(ctx, `UPDATE runners SET last_seen_at=now(), version=coalesce(nullif($2, ''), version) WHERE id=$1
		RETURNING name, version, drain`, runnerID, st.GetVersion()).Scan(&r.Name, &r.Version, &drain); err != nil {
		return nil, 0, false, err
	}
	var protocol int
	var orca, docker, toolchain string
	if st != nil {
		err := tx.QueryRow(ctx, `UPDATE ai_runners SET platform=$2, protocol=$3, host_id=coalesce(nullif($4, ''), host_id),
			capacity=greatest($5, 1), orca_state=$6, orca_reason=$7, orca_version=$8, orca_runtime_id=$9,
			docker_state=$10, docker_reason=$11, running=$12, toolchain_state=$13, toolchain_reason=$14, reported_at=now() WHERE runner_id=$1
			RETURNING role, platform, protocol, host_id, capacity, orca_state, orca_reason, docker_state, docker_reason, toolchain_state, toolchain_reason`,
			runnerID, st.GetPlatform(), st.GetProtocol(), st.GetHostId(), st.GetCapacity(),
			orcaReady(st.GetOrca()), st.GetOrca().GetReason(), st.GetOrca().GetVersion(), st.GetOrca().GetRuntimeId(),
			readyState(st.GetDocker()), st.GetDocker().GetReason(), st.GetRunning(),
			readyState(st.GetToolchain()), st.GetToolchain().GetReason()).
			Scan(&r.Role, &r.Platform, &protocol, &r.HostID, &r.Capacity, &orca, &r.OrcaReason, &docker, &r.DockerReason, &toolchain, &r.ToolchainReason)
		if err != nil {
			return nil, 0, false, fmt.Errorf("记录 AI 执行节点的状态：%w", err)
		}
		if err := c.saveAgents(ctx, tx, runnerID, st.GetAgents()); err != nil {
			return nil, 0, false, err
		}
	} else if err := tx.QueryRow(ctx, `SELECT role, platform, protocol, host_id, capacity, orca_state, orca_reason, docker_state, docker_reason,
		toolchain_state, toolchain_reason FROM ai_runners WHERE runner_id=$1`, runnerID).
		Scan(&r.Role, &r.Platform, &protocol, &r.HostID, &r.Capacity, &orca, &r.OrcaReason, &docker, &r.DockerReason,
			&toolchain, &r.ToolchainReason); err != nil {
		return nil, 0, false, err
	}
	r.OrcaReady, r.DockerReady, r.ToolchainReady = orca == ReadyStateReady, docker == ReadyStateReady, toolchain == ReadyStateReady
	agents, err := runnerAgents(ctx, tx, []int64{runnerID})
	if err != nil {
		return nil, 0, false, err
	}
	now := time.Now()
	for _, a := range agents[runnerID] {
		if ok, why := a.usable(now); ok {
			r.Agents[a.Agent] = true
		} else {
			r.AgentReasons[a.Agent] = why
		}
	}
	return r, protocol, drain, nil
}

// orcaReady 把上报的 Orca Server 状态转换为 ai_runners.orca_state 的取值；没有上报时为 unknown。
func orcaReady(o *airunnerv2.OrcaState) string {
	switch {
	case o == nil:
		return ReadyStateUnknown
	case o.GetReady():
		return ReadyStateReady
	}
	return ReadyStateUnavailable
}

// readyState 把上报的就绪状态转换为 ai_runners 的取值；没有上报时为 unknown。
func readyState(r *airunnerv2.Readiness) string {
	switch {
	case r == nil:
		return ReadyStateUnknown
	case r.GetReady():
		return ReadyStateReady
	}
	return ReadyStateUnavailable
}

// saveAgents 用这次上报的 Agent 替换 ai_runner_agents 中的记录（没有上报的 Agent 是这个节点不再提供的，删除）。
// 上报中 Catalog 没有的 Agent 忽略并记日志（连接器可能比 Server 新），不写入、不调度。
func (c *Core) saveAgents(ctx context.Context, tx pgx.Tx, runnerID int64, agents []*airunnerv2.AgentStatus) error {
	keep := []string{}
	for _, a := range agents {
		if !agentcfg.ValidAgent(a.GetName()) {
			c.log.Warn("AI 执行节点上报了不认识的 Agent，忽略", "runner", runnerID, "agent", a.GetName())
			continue
		}
		if slices.Contains(keep, a.GetName()) {
			continue
		}
		state := a.GetState()
		if state != agentcfg.AgentAvailable {
			state = agentcfg.AgentUnavailable
		}
		keep = append(keep, a.GetName())
		if _, err := tx.Exec(ctx, `INSERT INTO ai_runner_agents (runner_id, agent, installed, version, state, reason, checked_at, backoff_until)
			VALUES ($1,$2,$3,$4,$5,$6, CASE WHEN $7::bigint > 0 THEN to_timestamp($7) END, CASE WHEN $8::bigint > 0 THEN to_timestamp($8) END)
			ON CONFLICT (runner_id, agent) DO UPDATE SET installed=EXCLUDED.installed, version=EXCLUDED.version, state=EXCLUDED.state,
			  reason=EXCLUDED.reason, checked_at=EXCLUDED.checked_at, backoff_until=EXCLUDED.backoff_until`,
			runnerID, a.GetName(), a.GetInstalled(), a.GetVersion(), state, a.GetReason(), a.GetCheckedAt(), a.GetBackoffUntil()); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM ai_runner_agents WHERE runner_id=$1 AND NOT (agent = ANY($2))`, runnerID, keep)
	return err
}

// AgentRow 是 ai_runner_agents 中的一行。
type AgentRow struct {
	RunnerID     int64
	Agent        string
	Installed    *bool
	Version      string
	State        string
	Reason       string
	CheckedAt    *time.Time
	BackoffUntil *time.Time
}

// usable 判断这个 Agent 此刻能否用来执行任务（领取时，节点正在联系 Server）：已安装、检测为可用、不在退避中。
func (a *AgentRow) usable(now time.Time) (bool, string) {
	switch {
	case a.Installed != nil && !*a.Installed:
		return false, "没有安装"
	case a.State != agentcfg.AgentAvailable:
		return false, cmpOr(a.Reason, "检测为不可用")
	case a.BackoffUntil != nil && a.BackoffUntil.After(now):
		return false, "最近一次任务无法启动或跟踪它，" + a.BackoffUntil.Local().Format("15:04") + " 前不用"
	}
	return true, ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// runnerAgents 返回节点提供的 Agent（按 Catalog 的顺序）。
func runnerAgents(ctx context.Context, q store.Querier, runnerIDs []int64) (map[int64][]*AgentRow, error) {
	rows, err := q.Query(ctx, `SELECT runner_id, agent, installed, version, state, reason, checked_at, backoff_until FROM ai_runner_agents
		WHERE runner_id = ANY($1)`, runnerIDs)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[AgentRow])
	if err != nil {
		return nil, err
	}
	order := agentcfg.Agents()
	slices.SortFunc(list, func(a, b *AgentRow) int { return slices.Index(order, a.Agent) - slices.Index(order, b.Agent) })
	out := map[int64][]*AgentRow{}
	for _, a := range list {
		out[a.RunnerID] = append(out[a.RunnerID], a)
	}
	return out, nil
}

// Agent 的显示状态（PLAN #30.7，Dashboard、Admin API、CLI 共用）。
const (
	AgentShowAvailable   = "available"
	AgentShowBackoff     = "backoff"
	AgentShowUnavailable = "unavailable"
	AgentShowUnknown     = "unknown"
)

// AgentStatus 是一个 Agent 在一个 AI 执行节点上的显示状态。
type AgentStatus struct {
	Agent string
	// Name 是 Agent 的显示名。
	Name         string
	Version      string
	Installed    *bool
	Show         string
	Reason       string
	CheckedAt    *time.Time
	BackoffUntil *time.Time
}

// agentShow 按 PLAN #30.7 的表判定显示状态：退避中 → 不可用 → 可用（节点在线且检测没有过期）→ 未知。
func agentShow(a *AgentRow, online bool, now time.Time) (string, string) {
	switch {
	case a.BackoffUntil != nil && a.BackoffUntil.After(now):
		return AgentShowBackoff, "最近一次任务以 infra.agent 失败，" + a.BackoffUntil.Local().Format("15:04") + " 前不用"
	case a.Installed != nil && !*a.Installed:
		return AgentShowUnavailable, cmpOr(a.Reason, "没有安装")
	case a.State == agentcfg.AgentUnavailable:
		return AgentShowUnavailable, cmpOr(a.Reason, "检测为不可用")
	case !online:
		return AgentShowUnknown, "节点离线"
	case a.CheckedAt == nil || now.Sub(*a.CheckedAt) > 2*agentcfg.AgentProbeInterval:
		return AgentShowUnknown, "检测结果已过期"
	}
	return AgentShowAvailable, ""
}

// RunnerView 是一个 AI 执行节点的展示信息（runners + ai_runners + ai_runner_agents）。
type RunnerView struct {
	ID       int64
	Name     string
	Role     string
	Version  string
	State    string
	Drain    bool
	LastSeen *time.Time
	Online   bool
	Platform string
	Protocol int
	// HostID 是节点上报的机器标识（只用于展示，不参与调度，PLAN #33.7）。
	HostID string
	// Capacity 是同时执行的任务数上限（连接器配置 max_tasks）。
	Capacity int
	// OrcaState、OrcaReason 是节点上 Orca Server 的就绪状态；OrcaVersion、OrcaRuntimeID 是它的版本和 runtime id。
	OrcaState     string
	OrcaReason    string
	OrcaVersion   string
	OrcaRuntimeID string
	// DockerState、DockerReason 是 Docker / Compose 的就绪状态。
	DockerState  string
	DockerReason string
	// ToolchainState、ToolchainReason 是受管理的工具链的就绪状态（PLAN #33.12；旧连接器没有上报时为 unknown）。
	ToolchainState  string
	ToolchainReason string
	Running         int
	ReportedAt      *time.Time
	Agents          []AgentStatus
	// NeedsUpgrade 表示上报的协议版本不是 Server 支持的版本（不派任务）。
	NeedsUpgrade bool
}

// ListRunners 返回 AI 执行节点（id 不为 0 时只返回这一台），按名称排序。
func ListRunners(ctx context.Context, q store.Querier, id int64) ([]*RunnerView, error) {
	rows, err := q.Query(ctx, `SELECT r.id, r.name, a.role, r.version, r.state, r.drain, r.last_seen_at,
		coalesce(r.last_seen_at > now() - make_interval(secs => $2), false), a.platform, a.protocol, a.host_id, a.capacity,
		a.orca_state, a.orca_reason, a.orca_version, a.orca_runtime_id, a.docker_state, a.docker_reason, a.toolchain_state, a.toolchain_reason,
		a.running, a.reported_at FROM runners r JOIN ai_runners a ON a.runner_id=r.id WHERE ($1::bigint=0 OR r.id=$1) ORDER BY r.name`, id, store.RunnerOnlineWindow.Seconds())
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*RunnerView, error) {
		var v RunnerView
		err := row.Scan(&v.ID, &v.Name, &v.Role, &v.Version, &v.State, &v.Drain, &v.LastSeen, &v.Online, &v.Platform, &v.Protocol, &v.HostID,
			&v.Capacity, &v.OrcaState, &v.OrcaReason, &v.OrcaVersion, &v.OrcaRuntimeID, &v.DockerState, &v.DockerReason,
			&v.ToolchainState, &v.ToolchainReason, &v.Running, &v.ReportedAt)
		v.NeedsUpgrade = v.Protocol != agentcfg.AIProtocol
		return &v, err
	})
	if err != nil || len(list) == 0 {
		return list, err
	}
	ids := make([]int64, 0, len(list))
	for _, v := range list {
		ids = append(ids, v.ID)
	}
	agents, err := runnerAgents(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, v := range list {
		for _, a := range agents[v.ID] {
			show, reason := agentShow(a, v.Online && v.State == store.RunnerActive, now)
			v.Agents = append(v.Agents, AgentStatus{
				Agent: a.Agent, Name: agentcfg.AgentName(a.Agent), Version: a.Version, Installed: a.Installed, Show: show, Reason: reason,
				CheckedAt: a.CheckedAt, BackoffUntil: a.BackoffUntil,
			})
		}
	}
	return list, nil
}

package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// AI 的调度（PLAN #30.6、#30.7、#33.8）：领取即 running（同一事务中领取、epoch+1、签发租约、建 attempt、写 Check Run 意图）；
// 一个节点同时执行的任务数不超过它的 capacity；只派给就绪的节点。能否领取某个任务按 角色 → 协议版本 → 就绪（Orca Server、
// Agent、需要时 Docker / Compose）→ 任务类型特有的排除 → 仓库 AI 并发 的顺序给出第一个不满足的原因。
//
// 加锁顺序：节点行 → 候选任务（FOR UPDATE SKIP LOCKED，不等待）→ AI 配额 advisory lock（仓库 ID 升序）。领取路径不写请求行。

// RunnerFreeSlotsSQL 是 AI 执行节点的空闲槽位数（还能再领取的任务数，PLAN #33.8）的 SQL 表达式，节点的 ai_runners 行的
// 别名必须是 ar：并发上限（ar.capacity，连接器配置 max_tasks）减去执行中的任务数，下限 0。执行中的任务数取两边中较大的：
// Server 的账本（当前 attempt 属于这个节点、状态为 running 的任务，不分类型）和节点最近一次在 Acquire 中上报的（ar.running），
// 任一边到了上限都不再派任务。领取（tryAcquire）和开发选题的规划（aidev 的 freeDevSlots）都用这一条规则。
const RunnerFreeSlotsSQL = `greatest(ar.capacity - greatest(ar.running, (SELECT count(*) FROM ai_tasks slot_t
	JOIN ai_attempts slot_a ON slot_a.id=slot_t.current_attempt_id WHERE slot_t.state='running' AND slot_a.runner_id=ar.runner_id)), 0)`

// candidateLimit 是一次领取（一个事务）最多考察的候选数，即一页。
const candidateLimit = 50

// cursor 是分页考察候选时上一页最后一个任务的排序键（priority DESC, queued_at, id）：一页的候选都领不了时从它之后继续，
// 后面的任务不会被前面的饿死。
type cursor struct {
	Priority int
	QueuedAt time.Time
	ID       int64
}

func (c *cursor) args() []any {
	if c == nil {
		return []any{nil, nil, nil}
	}
	return []any{c.Priority, c.QueuedAt, c.ID}
}

// errDrained、errRevoked 表示领取时节点已被暂停接单、吊销。
var (
	errDrained = errors.New("AI 执行节点已暂停接单")
	errRevoked = errors.New("AI 执行节点已被吊销或不存在")
)

// acquireResult 是一次领取（考察一页候选）的结果。
type acquireResult struct {
	task *airunnerv2.AITask
	// replayed 表示 task 是同一个 request_id 之前已经领取的任务。
	replayed bool
	// next 不为 nil 表示这一页满了却没有领到：从这里继续考察下一页。
	next *cursor
	// again 表示选中的任务无法生成下发内容、已经置为失败：立即从同一位置重新考察。
	again bool
}

// lockRunner 锁住节点行并重新检查状态：同一个节点的领取串行进行；长轮询期间可能被暂停接单或吊销。同一个
// request_id 已经领取过时返回那个任务（与它重叠的长轮询不会再分配一个）。
func (c *Core) lockRunner(ctx context.Context, tx pgx.Tx, r *Runner, requestID string) (*airunnerv2.AITask, error) {
	var state string
	var drain bool
	err := tx.QueryRow(ctx, `SELECT state, drain FROM runners WHERE id=$1 FOR UPDATE`, r.ID).Scan(&state, &drain)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, errRevoked
	case err != nil:
		return nil, err
	case state != store.RunnerActive:
		return nil, errRevoked
	case drain:
		return nil, errDrained
	}
	return c.replay(ctx, tx, r.ID, requestID)
}

// replay 返回节点用 requestID 已经领取、还在执行的任务（没有时为 nil）。
func (c *Core) replay(ctx context.Context, q store.Querier, runnerID int64, requestID string) (*airunnerv2.AITask, error) {
	if requestID == "" {
		return nil, nil
	}
	var attemptID, epoch, taskID int64
	var platform string
	err := q.QueryRow(ctx, `SELECT a.id, a.epoch, a.task_id, a.platform FROM ai_attempts a JOIN ai_tasks t ON t.id=a.task_id
		WHERE a.runner_id=$1 AND a.acquire_request_id=$2 AND t.state='running' AND t.epoch=a.epoch`, runnerID, requestID).Scan(&attemptID, &epoch, &taskID, &platform)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := GetTask(ctx, q, taskID, false)
	if err != nil {
		return nil, err
	}
	return c.buildTask(ctx, q, t, attemptID, epoch, platform)
}

// tryAcquire 在一个事务中考察从 after 开始的一页候选，为这个节点领取一个任务。
func (c *Core) tryAcquire(ctx context.Context, r *Runner, requestID string, after *cursor) (acquireResult, error) {
	var res acquireResult
	types := c.types.ForRole(r.Role)
	if len(types) == 0 || r.Platform == "" {
		return res, nil
	}
	err := c.db.Tx(ctx, func(tx pgx.Tx) error {
		res = acquireResult{}
		replay, err := c.lockRunner(ctx, tx, r, requestID)
		if err != nil || replay != nil {
			res.task, res.replayed = replay, replay != nil
			return err
		}
		// 节点的并发上限（连接器配置 max_tasks，PLAN #33.8）：没有空闲槽位时不领取。规则见 RunnerFreeSlotsSQL（开发选题的
		// 规划共用它）；节点这次上报的 capacity、running 已经由 touchRunner 写入 ai_runners
		var free int
		if err := tx.QueryRow(ctx, `SELECT `+RunnerFreeSlotsSQL+` FROM ai_runners ar WHERE ar.runner_id=$1`, r.ID).Scan(&free); err != nil {
			return err
		}
		if free <= 0 {
			return nil
		}
		// state='pending' 写成字面量：部分索引 ai_tasks_pending 依赖字面量才能命中
		cands, err := QueryTasks(ctx, tx, `state='pending' AND type = ANY($1) AND $2 = ANY(platforms)
			AND ($3::int IS NULL OR priority < $3 OR (priority = $3 AND (queued_at, id) > ($4::timestamptz, $5::bigint)))
			ORDER BY priority DESC, queued_at, id LIMIT $6 FOR UPDATE SKIP LOCKED`,
			append(append([]any{types, r.Platform}, after.args()...), candidateLimit)...)
		if err != nil || len(cands) == 0 {
			return err
		}
		chosen, err := c.choose(ctx, tx, r, cands)
		if err != nil {
			return err
		}
		if chosen == nil {
			res.next = nextCursor(cands)
			return nil
		}
		res.task, err = c.claim(ctx, tx, r, requestID, chosen)
		res.again, res.next = res.task == nil, after
		return err
	})
	return res, err
}

// nextCursor 返回一页候选之后的游标：这一页满了（后面可能还有）时为最后一个任务的排序键，否则为 nil。
func nextCursor(cands []*Task) *cursor {
	if len(cands) < candidateLimit {
		return nil
	}
	last := cands[len(cands)-1]
	if last.QueuedAt == nil {
		return nil
	}
	return &cursor{Priority: last.Priority, QueuedAt: *last.QueuedAt, ID: last.ID}
}

// choose 按 就绪 → 任务类型的排除 → 仓库并发 的顺序选出第一个能领取的候选，其余记下不能领取的原因（PLAN #30.7）。
// 生成下发内容失败的候选直接失败（只让这个任务失败，不回滚整次领取），继续考察下一个。
func (c *Core) choose(ctx context.Context, tx pgx.Tx, r *Runner, cands []*Task) (*Task, error) {
	skip := map[int64]string{}
	provided, err := c.providedAgents(ctx, tx, r.Role)
	if err != nil {
		return nil, err
	}
	ready := make([]*Task, 0, len(cands))
	for _, t := range cands {
		typ, err := c.types.Get(t.Type)
		if err != nil {
			return nil, err
		}
		if why := readiness(r, typ, t, provided); why != "" {
			skip[t.ID] = why
			continue
		}
		ready = append(ready, t)
	}
	byType := map[string][]*Task{}
	for _, t := range ready {
		byType[t.Type] = append(byType[t.Type], t)
	}
	for _, name := range slices.Sorted(maps.Keys(byType)) {
		typ, _ := c.types.Get(name)
		m, err := typ.Mismatch(ctx, tx, r, byType[name])
		if err != nil {
			return nil, err
		}
		maps.Copy(skip, m)
	}
	quotas, err := lockQuotas(ctx, tx, c.types, ready)
	if err != nil {
		return nil, err
	}
	var chosen *Task
	for _, t := range ready {
		if _, ok := skip[t.ID]; ok {
			continue
		}
		if why := quotas.full(t); why != "" {
			skip[t.ID] = why
			continue
		}
		chosen = t
		break
	}
	for id, why := range skip {
		if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET last_skip_reason=$2 WHERE id=$1 AND last_skip_reason IS DISTINCT FROM $2`, id, why); err != nil {
			return nil, err
		}
	}
	return chosen, nil
}

// providedAgents 返回在线、没有吊销的 role 角色的 AI 执行节点提供的 Agent（"没有提供 X 的 AI 审计节点"的依据）。
func (c *Core) providedAgents(ctx context.Context, tx pgx.Tx, role string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT g.agent FROM ai_runner_agents g JOIN ai_runners a ON a.runner_id=g.runner_id
		JOIN runners r ON r.id=g.runner_id WHERE a.role=$1 AND r.state='active' AND r.last_seen_at > now() - make_interval(secs => $2)`,
		role, store.RunnerOnlineWindow.Seconds())
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range list {
		out[a] = true
	}
	return out, nil
}

// readiness 返回这个节点因为就绪条件不能领取 t 的原因（Orca Server、任务指定的 Agent、需要测试服务时 Docker / Compose、
// 声明了工具链时节点的工具链）。
func readiness(r *Runner, typ TaskType, t *Task, provided map[string]bool) string {
	name := agentcfg.AgentName(t.Agent)
	switch {
	case !r.OrcaReady:
		return "这个节点的 Orca Server 没有就绪：" + cmpOr(r.OrcaReason, "没有上报")
	case !provided[t.Agent]:
		return fmt.Sprintf("没有提供 %s 的%s", t.Agent, strings.TrimSpace(RoleLabel(r.Role)))
	case !r.Agents[t.Agent]:
		if why, ok := r.AgentReasons[t.Agent]; ok {
			return fmt.Sprintf("需要 %s（%s），这个节点上不可用：%s", t.Agent, name, why)
		}
		return fmt.Sprintf("需要 %s（%s），这个节点没有提供", t.Agent, name)
	case len(typ.Services(t)) > 0 && !r.DockerReady:
		return fmt.Sprintf("需要测试服务（%s），这个节点的 Docker / Compose 没有就绪：%s", strings.Join(typ.Services(t), "、"),
			cmpOr(r.DockerReason, "没有上报"))
	case len(typ.Tools(t, r.Platform)) > 0 && !r.ToolchainReady:
		return fmt.Sprintf("需要受管理的工具链（%s），这个节点不能准备：%s", toolNames(typ.Tools(t, r.Platform)),
			cmpOr(r.ToolchainReason, toolchainNotReported))
	}
	return ""
}

// RunningReason 是任务执行中的说明：在哪个节点、哪个平台上执行（一组平台中由哪台领到，Dashboard 和 Check Run 中看得出）。
func RunningReason(runner, platform string) string {
	return fmt.Sprintf("在节点 %s（%s）上执行", runner, platform)
}

// toolchainNotReported 是节点没有上报工具链状态时的原因：连接器的版本早于受管理的工具链（PLAN #33.12）。
const toolchainNotReported = "连接器（kiln-orca）没有上报工具链状态，版本过旧，请升级 kiln-orca"

// toolNames 返回工具链的名称和版本（例如 go 1.27.3、node 24.11.0）。
func toolNames(tools []aispec.ToolRef) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name+" "+t.Version)
	}
	return strings.Join(names, "、")
}

// quotas 是一页候选所在仓库的 AI 并发情况（按任务类型）。
type quotas struct {
	reg     *Registry
	repoOf  map[int64]int64          // 请求 → 仓库
	running map[int64]map[string]int // 仓库 → 任务类型 → 执行中的数量
}

// lockQuotas 锁住候选中设置了并发上限的仓库的 AI 配额（事务级 advisory lock），再统计这些仓库执行中的任务：避免两台
// AI 执行节点同时读到未满后各自领取。按仓库 ID 升序一次锁完，避免互相等待。
func lockQuotas(ctx context.Context, tx pgx.Tx, reg *Registry, cands []*Task) (quotas, error) {
	q := quotas{reg: reg, repoOf: map[int64]int64{}, running: map[int64]map[string]int{}}
	var reqIDs []int64
	for _, t := range cands {
		typ, err := reg.Get(t.Type)
		if err != nil {
			return q, err
		}
		if typ.Limit(t) > 0 {
			reqIDs = append(reqIDs, t.RequestID)
		}
	}
	if len(reqIDs) == 0 {
		return q, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, repo_id FROM ai_requests WHERE id = ANY($1)`, reqIDs)
	if err != nil {
		return q, err
	}
	var reqID, repoID int64
	if _, err := pgx.ForEachRow(rows, []any{&reqID, &repoID}, func() error {
		q.repoOf[reqID] = repoID
		return nil
	}); err != nil {
		return q, err
	}
	repos := slices.Compact(slices.Sorted(maps.Values(q.repoOf)))
	for _, id := range repos {
		if err := store.LockXact(ctx, tx, quotaLockKey(id)); err != nil {
			return q, err
		}
	}
	rows, err = tx.Query(ctx, `SELECT r.repo_id, t.type, count(*) FROM ai_tasks t JOIN ai_requests r ON r.id=t.request_id
		WHERE t.state='running' AND r.repo_id = ANY($1) GROUP BY 1, 2`, repos)
	if err != nil {
		return q, err
	}
	var typ string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&repoID, &typ, &n}, func() error {
		if q.running[repoID] == nil {
			q.running[repoID] = map[string]int{}
		}
		q.running[repoID][typ] = n
		return nil
	})
	return q, err
}

// full 返回 t 所在仓库的这类任务并发已满时不能领取的原因（没有满或没有设置上限时为空）。
func (q quotas) full(t *Task) string {
	typ, err := q.reg.Get(t.Type)
	if err != nil {
		return ""
	}
	limit := typ.Limit(t)
	if limit <= 0 {
		return ""
	}
	repoID, ok := q.repoOf[t.RequestID]
	if ok && q.running[repoID][t.Type] >= limit {
		return fmt.Sprintf("仓库的 AI %s并发上限 %d", typeLabel(t.Type), limit)
	}
	return ""
}

// typeLabel 是任务类型的中文名称。
func typeLabel(typ string) string {
	switch typ {
	case agentcfg.TaskReview:
		return "审计"
	case agentcfg.TaskDevelop:
		return "开发"
	}
	return typ + " "
}

// quotaLockKey 是仓库 AI 配额 advisory lock 的键（与 CI 的配额、并发组的键不会重复）。
func quotaLockKey(repoID int64) string { return fmt.Sprintf("kiln:ai-quota:%d", repoID) }

// claim 领取一个任务：置为 running、epoch+1、签发租约、建 attempt（记下节点和它上报的平台，即实际执行的平台）、写 Check Run
// 意图（PLAN #30.6）。生成下发内容失败时让这个任务失败并返回 nil。
func (c *Core) claim(ctx context.Context, tx pgx.Tx, r *Runner, requestID string, t *Task) (*airunnerv2.AITask, error) {
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return nil, err
	}
	var epoch int64
	if err := tx.QueryRow(ctx, `UPDATE ai_tasks SET state='running', epoch=epoch+1, attempt_count=attempt_count+1,
		lease_expires_at=now()+make_interval(secs => $2), started_at=now(), last_skip_reason=NULL, reason=$3 WHERE id=$1 RETURNING epoch`,
		t.ID, c.cfg.LeaseTTL.Seconds(), RunningReason(r.Name, r.Platform)).Scan(&epoch); err != nil {
		return nil, err
	}
	var aid int64
	if err := tx.QueryRow(ctx, `INSERT INTO ai_attempts (task_id, epoch, runner_id, runner_name, host_id, platform, acquire_request_id, runner_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, t.ID, epoch, r.ID, r.Name, r.HostID, r.Platform, requestID, r.Version).Scan(&aid); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET current_attempt_id=$2 WHERE id=$1`, t.ID, aid); err != nil {
		return nil, err
	}
	t.State, t.Epoch, t.CurrentAttemptID = TaskRunning, epoch, &aid
	spec, err := c.buildTask(ctx, tx, t, aid, epoch, r.Platform)
	if bad, ok := errors.AsType[*SpecError](err); ok {
		c.log.Warn("无法生成下发给 AI 执行节点的任务，任务置为失败", "task", t.ID, "err", bad)
		return nil, c.failSpec(ctx, tx, t, bad)
	}
	if err != nil {
		return nil, err
	}
	if err := typ.Claimed(ctx, tx, t, r); err != nil {
		return nil, err
	}
	return spec, c.enqueueTaskCheck(ctx, tx, typ, t.ID, t.RequestID)
}

// failSpec 让无法生成下发内容的任务失败。领取路径不锁请求行（加锁顺序），请求的汇总由 sweeper 补做。
func (c *Core) failSpec(ctx context.Context, tx pgx.Tx, t *Task, bad *SpecError) error {
	if _, err := tx.Exec(ctx, `UPDATE ai_attempts SET outcome='failed', error_class='spec', message=$2, finished_at=now() WHERE id=$1`,
		*t.CurrentAttemptID, bad.Error()); err != nil {
		return err
	}
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return err
	}
	return c.setTaskTerminal(ctx, tx, typ, t, TaskFailed, "无法生成下发给 AI 执行节点的任务："+bad.Error(), nil)
}

// buildTask 生成下发给 AI 执行节点的任务：任务类型填写 oneof kind 和 prompt，这里统一填写 Agent、需要的测试服务、
// 这个平台的工具链和 prompt 的摘要（PLAN #33.5、#33.6）。platform 是 attempt 执行的平台（领取时节点上报的）。
func (c *Core) buildTask(ctx context.Context, q store.Querier, t *Task, attemptID, epoch int64, platform string) (*airunnerv2.AITask, error) {
	typ, err := c.types.Get(t.Type)
	if err != nil {
		return nil, err
	}
	r, err := GetRequest(ctx, q, t.RequestID, false)
	if err != nil {
		return nil, err
	}
	repo, err := store.GetRepo(ctx, q, r.RepoID)
	if err != nil {
		return nil, err
	}
	out := &airunnerv2.AITask{
		Ref:             &airunnerv2.TaskRef{AttemptId: attemptID, Epoch: epoch},
		TaskId:          t.ID,
		RequestId:       r.ID,
		DisplayName:     t.DisplayName,
		RepoId:          repo.ID,
		Repository:      repo.FullName(),
		CloneUrl:        repo.CloneURL,
		Sha:             r.SHA,
		Agent:           t.Agent,
		Services:        typ.Services(t),
		Tools:           toolRefs(typ.Tools(t, platform)),
		Repositories:    typ.Repositories(t),
		TimeoutSeconds:  clampInt32(int64(t.TimeoutMinutes) * secondsPerMinute),
		LeaseTtlSeconds: clampInt32(int64(c.cfg.LeaseTTL / time.Second)),
	}
	if err := typ.Spec(ctx, q, SpecInput{Task: t, Request: r, Repo: repo, Platform: platform}, out); err != nil {
		return nil, err
	}
	out.PromptDigest = promptDigest(out.Prompt)
	return out, nil
}

// toolRefs 把冻结的工具链转换为下发的 ToolRef。
func toolRefs(tools []aispec.ToolRef) []*airunnerv2.ToolRef {
	out := make([]*airunnerv2.ToolRef, 0, len(tools))
	for _, t := range tools {
		out = append(out, &airunnerv2.ToolRef{Name: t.Name, Version: t.Version, Platform: t.Platform, Digest: t.Digest})
	}
	return out
}

// promptDigest 返回 prompt 的 sha256（记录实际交给 Agent 的规则版本）。
func promptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// secondsPerMinute 用于把分钟换算成秒。
const secondsPerMinute = 60

// clampInt32 把整数转换为 int32，超出范围时取边界值（用于填写 protobuf 的字段）。
func clampInt32[T int | int64](n T) int32 {
	switch {
	case int64(n) > math.MaxInt32:
		return math.MaxInt32
	case int64(n) < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

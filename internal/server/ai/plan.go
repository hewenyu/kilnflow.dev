package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 规划请求（PLAN #30.6）：请求和它的任务在一个事务中建成；先取并发组的 advisory lock，在同一事务中处理取代（审计）或排队
// （返修）。去重键相同的请求只建一个；GitHub 触发的重跑按 (family_id, delivery_id) 至多建一次。

// NewRequest 是要建的请求。
type NewRequest struct {
	Type   string
	RepoID int64
	Event  string
	Ref    string
	SHA    string
	// Trigger 是触发来源（审计：pull_request | manual），Source 是事件来源（webhook、dashboard、cli、aidev 等）。
	Trigger, Source string
	Sender          string
	// DedupeKey 是首次请求的去重键；重跑为空。
	DedupeKey string
	// GroupKey 是并发组（同一仓库内有效），为空时没有并发组。
	GroupKey string
	// RerunOf 是被重跑的请求（首次请求为 nil）；DeliveryID 是触发重跑的 webhook delivery（Dashboard、CLI 的重跑为空）。
	RerunOf    *Request
	DeliveryID string
	// ConfigError 不为空时请求直接为 failed，没有任务。
	ConfigError string
	Warnings    string
	Priority    int
	Tasks       []NewTask
	// Detail 在同一事务中写入类型的明细（ai_review_requests、ai_dev_requests）。
	Detail func(ctx context.Context, tx pgx.Tx, requestID int64) error
}

// NewTask 是请求中的一个任务。
type NewTask struct {
	Key string
	// Platforms 是可以执行这个任务的平台（Runner 平台，至少一个）：节点上报的平台在其中即可领取。
	Platforms   []string
	Agent       string
	DisplayName string
	// Spec 是冻结的执行输入（JSON 编码后写入 ai_tasks.spec）。
	Spec           any
	TimeoutMinutes int
	// ReuseFrom 不为空时任务不执行，直接沿用这个已结束任务的结果和状态（重跑只重跑失败的、重跑单个任务、合并队列更新后
	// 沿用审计）。
	ReuseFrom   *Task
	ReuseReason string
}

// Planned 是规划的结果。
type Planned struct {
	Request *Request
	// Created 为假表示请求已经存在（去重键或 delivery 相同），返回的是已有的请求。
	Created bool
}

// ReuseSource 返回沿用 t 的结果时记录的来源：t 本身也是沿用的时为它的来源（沿用不叠加）。
func ReuseSource(t *Task) int64 {
	if t.ReusedFrom != nil {
		return *t.ReusedFrom
	}
	return t.ID
}

// RequestByDedupeKey 返回去重键为 key 的请求，没有时为 nil。
func RequestByDedupeKey(ctx context.Context, q store.Querier, key string) (*Request, error) {
	list, err := QueryRequests(ctx, q, `dedupe_key=$1`, key)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// RerunByDelivery 返回请求族 family 中由 webhook delivery 建的重跑，没有时为 nil。
func RerunByDelivery(ctx context.Context, q store.Querier, family int64, delivery string) (*Request, error) {
	list, err := QueryRequests(ctx, q, `family_id=$1 AND delivery_id=$2`, family, delivery)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// Plan 建请求和它的任务。去重键（或者重跑的 delivery）相同的请求已经存在时返回已有的请求。
func (c *Core) Plan(ctx context.Context, nr *NewRequest) (*Planned, error) {
	typ, err := c.types.Get(nr.Type)
	if err != nil {
		return nil, err
	}
	if nr.RerunOf != nil && !typ.Policy().Rerunnable {
		return nil, ErrNotRerunnable
	}
	// 事务外先查一次（常见的重复事件不取并发组锁），取锁后再查一次
	if r, err := c.existing(ctx, c.db, nr); err != nil || r != nil {
		return &Planned{Request: r}, err
	}
	var out *Planned
	err = c.db.Tx(ctx, func(tx pgx.Tx) error {
		out = nil
		if err := lockGroup(ctx, tx, nr.RepoID, nonEmpty(nr.GroupKey)); err != nil {
			return err
		}
		r, err := c.existing(ctx, tx, nr)
		if err != nil || r != nil {
			out = &Planned{Request: r}
			return err
		}
		r, err = c.insertRequest(ctx, tx, typ, nr)
		if err != nil {
			return err
		}
		out = &Planned{Request: r, Created: true}
		return nil
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation {
		// 并发规划同一个去重键（或同一个 delivery 的重跑）而没有并发组锁时，唯一索引挡住后一个：返回先建成的请求
		if r, qerr := c.existing(ctx, c.db, nr); qerr == nil && r != nil {
			return &Planned{Request: r}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if out.Created {
		c.hub.All()
	}
	return out, nil
}

// uniqueViolation 是 Postgres 违反唯一约束的错误码。
const uniqueViolation = "23505"

// ErrNotRerunnable 表示这类请求不能重跑（开发请求由 aidev 按工作项规划）。
var ErrNotRerunnable = errors.New("AI 开发的请求不能重跑：受阻的工作项用 kiln ai dev retry（或 Dashboard 的重试），没有领取的 Issue 留在任务池中，由空闲的开发机重新选题")

// existing 返回与 nr 相同的已有请求（去重键相同，或者同一个请求族中同一个 delivery 的重跑），没有时为 nil。
func (c *Core) existing(ctx context.Context, q store.Querier, nr *NewRequest) (*Request, error) {
	switch {
	case nr.DedupeKey != "":
		return RequestByDedupeKey(ctx, q, nr.DedupeKey)
	case nr.RerunOf != nil && nr.DeliveryID != "":
		return RerunByDelivery(ctx, q, nr.RerunOf.FamilyID, nr.DeliveryID)
	}
	return nil, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insertRequest 在事务中建请求、明细和任务，处理并发组，写 Check Run 意图；请求建成即结束时（配置错误、全部沿用）执行结束钩子。
func (c *Core) insertRequest(ctx context.Context, tx pgx.Tx, typ TaskType, nr *NewRequest) (*Request, error) {
	pol := typ.Policy()
	state := RequestActive
	if nr.ConfigError == "" && pol.GroupMode == GroupQueue && nr.GroupKey != "" {
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ai_requests WHERE repo_id=$1 AND group_key=$2 AND state='active'`,
			nr.RepoID, nr.GroupKey).Scan(&active); err != nil {
			return nil, err
		}
		if active > 0 {
			state = RequestQueued
		}
	}
	var family, rerunOf *int64
	if nr.RerunOf != nil {
		family, rerunOf = &nr.RerunOf.FamilyID, &nr.RerunOf.ID
	}
	var id int64
	err := tx.QueryRow(ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, trigger, source, sender, dedupe_key, family_id, delivery_id, group_key,
		  rerun_of, state, config_error, warnings)
		SELECT n.id, $1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), coalesce($10, n.id), nullif($11, ''), nullif($12, ''), $13, $14,
		  nullif($15, ''), nullif($16, '') FROM n RETURNING id`,
		nr.Type, nr.RepoID, nr.Event, nr.Ref, nr.SHA, nr.Trigger, nr.Source, nr.Sender, nr.DedupeKey, family, nr.DeliveryID, nr.GroupKey,
		rerunOf, state, nr.ConfigError, nr.Warnings).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("建 AI 请求：%w", err)
	}
	if nr.Detail != nil {
		if err := nr.Detail(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if nr.ConfigError == "" {
		if err := c.insertTasks(ctx, tx, typ, id, state, nr); err != nil {
			return nil, err
		}
	}
	r, err := GetRequest(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if nr.ConfigError == "" && pol.GroupMode == GroupSupersede && nr.RerunOf == nil && nr.GroupKey != "" {
		if err := c.supersede(ctx, tx, r); err != nil {
			return nil, err
		}
	}
	if nr.ConfigError == "" && pol.GroupMode == GroupQueue && nr.GroupKey != "" {
		if err := c.dropQueued(ctx, tx, r); err != nil {
			return nil, err
		}
	}
	if err := c.enqueueRequestCheck(ctx, tx, typ, r); err != nil {
		return nil, err
	}
	if nr.ConfigError != "" {
		return r, c.finishRequest(ctx, tx, typ, r, RequestFailed)
	}
	return r, c.settle(ctx, tx, r)
}

// insertTasks 建请求的任务：请求 active 时为 pending，排队时为 waiting，沿用的直接为来源的状态。
func (c *Core) insertTasks(ctx context.Context, tx pgx.Tx, typ TaskType, requestID int64, state RequestState, nr *NewRequest) error {
	prio := nr.Priority
	if prio == 0 {
		prio = defaultPriority
	}
	for _, t := range nr.Tasks {
		ts, reason := TaskPending, waitReason(typ, t.Platforms)
		var reused *int64
		if t.ReuseFrom != nil {
			src := ReuseSource(t.ReuseFrom)
			ts, reason, reused = t.ReuseFrom.State, t.ReuseReason, &src
		} else if state == RequestQueued {
			ts, reason = TaskWaiting, waitingReason
		}
		var id int64
		if len(t.Platforms) == 0 {
			return fmt.Errorf("建 AI 任务 %s：没有可以执行的平台", t.Key)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, reason, priority,
			timeout_minutes, reused_from, queued_at, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, CASE WHEN $8='pending' THEN now() END, CASE WHEN $12::bigint IS NOT NULL THEN now() END)
			RETURNING id`, requestID, t.Key, nr.Type, t.Platforms, t.Agent, t.DisplayName, store.MustJSON(t.Spec), ts, reason, prio,
			t.TimeoutMinutes, reused).Scan(&id); err != nil {
			return fmt.Errorf("建 AI 任务 %s：%w", t.Key, err)
		}
		// 沿用的任务也建 Check Run：分支保护按名称要求的检查在新的 head 上也要有
		if err := c.enqueueTaskCheck(ctx, tx, typ, id, requestID); err != nil {
			return err
		}
	}
	return nil
}

// waitReason 是任务待领取时的说明：任务类型的说明和可以执行的平台（例如"等待 AI 审计节点领取（平台 darwin/arm64 或
// linux/amd64）"），没有这些平台的节点在线时看得出在等什么。
func waitReason(typ TaskType, platforms []string) string {
	return fmt.Sprintf("%s（平台 %s）", typ.WaitReason(), PlatformsText(platforms))
}

const (
	// defaultPriority 是任务的默认优先级（与 CI 的普通 run 相同）。
	defaultPriority = 1
	// waitingReason 是请求在并发组中排队时任务的说明。
	waitingReason = "并发组中有执行中的请求，排队等待"
)

// CancelReason 是取消的原因（ai_tasks.cancel_reason）。
type CancelReason string

const (
	CancelByUser       CancelReason = "user"
	CancelSuperseded   CancelReason = "superseded"
	CancelByQueue      CancelReason = "queue_replaced"
	CancelPRClosed     CancelReason = "pr_closed"
	CancelWorkItemDone CancelReason = "work_item"
)

// Text 是展示给用户的取消原因。
func (r CancelReason) Text() string {
	switch r {
	case CancelSuperseded:
		return "被同一个 PR 的新 head 的审计取代"
	case CancelByQueue:
		return "被排队中更新的请求取代"
	case CancelPRClosed:
		return "PR 已关闭"
	case CancelWorkItemDone:
		return "工作项已结束或终止"
	}
	return "已取消"
}

// supersede 取消同组中 head 不同的未结束请求（PLAN #30.6：审计的取代；重跑不取代，由调用方保证）。
func (c *Core) supersede(ctx context.Context, tx pgx.Tx, r *Request) error {
	ids, err := store.QueryIDs(ctx, tx, `SELECT id FROM ai_requests WHERE repo_id=$1 AND group_key=$2 AND state IN ('queued','active')
		AND sha<>$3 AND id<>$4 ORDER BY id`, r.RepoID, *r.GroupKey, r.SHA, r.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.cancelLocked(ctx, tx, id, CancelSuperseded); err != nil {
			return err
		}
	}
	return nil
}

// dropQueued 取消同组中排队的旧请求（排队只留最新的一个）。
func (c *Core) dropQueued(ctx context.Context, tx pgx.Tx, r *Request) error {
	ids, err := store.QueryIDs(ctx, tx, `SELECT id FROM ai_requests WHERE repo_id=$1 AND group_key=$2 AND state='queued' AND id<>$3 ORDER BY id`,
		r.RepoID, *r.GroupKey, r.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.cancelLocked(ctx, tx, id, CancelByQueue); err != nil {
			return err
		}
	}
	return nil
}

// Cancel 取消请求：没有开始的任务立即结束，执行中的任务请求取消（由心跳下发，Runner 确认后结束）。已经结束的请求不变。
func (c *Core) Cancel(ctx context.Context, requestID int64, reason CancelReason, actor string) error {
	err := c.db.Tx(ctx, func(tx pgx.Tx) error { return c.CancelTx(ctx, tx, requestID, reason, actor) })
	if err == nil {
		c.hub.All()
	}
	return err
}

// CancelTx 在调用方的事务中取消请求（加锁顺序：并发组 → 请求 → 任务）。actor 不为空时写审计日志。
func (c *Core) CancelTx(ctx context.Context, tx pgx.Tx, requestID int64, reason CancelReason, actor string) error {
	r, err := GetRequest(ctx, tx, requestID, false)
	if err != nil {
		return err
	}
	if err := lockGroup(ctx, tx, r.RepoID, r.GroupKey); err != nil {
		return err
	}
	if err := c.cancelLocked(ctx, tx, requestID, reason); err != nil {
		return err
	}
	if actor == "" {
		return nil
	}
	return store.Audit(ctx, tx, actor, "ai_request.cancel", map[string]any{"ai_request_id": requestID, "reason": string(reason)})
}

// cancelLocked 取消请求；调用方已经持有并发组的锁。
func (c *Core) cancelLocked(ctx context.Context, tx pgx.Tx, requestID int64, reason CancelReason) error {
	r, err := GetRequest(ctx, tx, requestID, true)
	if err != nil || r.State.Terminal() {
		return err
	}
	typ, err := c.types.Get(r.Type)
	if err != nil {
		return err
	}
	tasks, err := RequestTasks(ctx, tx, r.ID, true)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		switch {
		case t.State.Queued():
			if err := c.setTaskTerminal(ctx, tx, typ, t, TaskCancelled, reason.Text(), &reason); err != nil {
				return err
			}
		case t.State == TaskRunning && !t.CancelRequested:
			if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET cancel_requested=true, cancel_reason=$2 WHERE id=$1`, t.ID, reason); err != nil {
				return err
			}
		}
	}
	if r.State == RequestQueued {
		return c.finishRequest(ctx, tx, typ, r, RequestCancelled)
	}
	return c.settle(ctx, tx, r)
}

// setTaskTerminal 把没有在执行的任务直接置为终态。
func (c *Core) setTaskTerminal(ctx context.Context, tx pgx.Tx, typ TaskType, t *Task, state TaskState, reason string, cancel *CancelReason) error {
	if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET state=$2, reason=$3, cancel_reason=coalesce($4, cancel_reason), finished_at=now(),
		lease_expires_at=NULL WHERE id=$1`, t.ID, state, reason, cancel); err != nil {
		return err
	}
	t.State = state
	return c.enqueueTaskCheck(ctx, tx, typ, t.ID, t.RequestID)
}

// settle 在请求的任务都结束后汇总请求的状态并执行结束钩子（PLAN #30.6）。调用方已经锁住请求。请求不是 active，或者还有
// 未结束的任务时什么也不做。
func (c *Core) settle(ctx context.Context, tx pgx.Tx, r *Request) error {
	if r.State != RequestActive {
		return nil
	}
	tasks, err := RequestTasks(ctx, tx, r.ID, false)
	if err != nil {
		return err
	}
	state := RequestSucceeded
	skipped := len(tasks) > 0
	for _, t := range tasks {
		switch {
		case !t.State.Terminal():
			return nil
		case t.State.Failed():
			state = RequestFailed
		case t.State == TaskCancelled && state != RequestFailed:
			state = RequestCancelled
		}
		skipped = skipped && t.State == TaskSkipped
	}
	if skipped {
		state = RequestSkipped
	}
	typ, err := c.types.Get(r.Type)
	if err != nil {
		return err
	}
	return c.finishRequest(ctx, tx, typ, r, state)
}

// finishRequest 把请求置为终态并执行结束钩子（同一事务）：请求级 Check Run 意图、类型的钩子（PR review、通知合并队列、aidev）、
// 释放并发组（启动排队的请求）。
func (c *Core) finishRequest(ctx context.Context, tx pgx.Tx, typ TaskType, r *Request, state RequestState) error {
	if _, err := tx.Exec(ctx, `UPDATE ai_requests SET state=$2, finished_at=now(), updated_at=now() WHERE id=$1`, r.ID, state); err != nil {
		return err
	}
	r.State = state
	now := time.Now()
	r.FinishedAt = &now
	if err := c.enqueueRequestCheck(ctx, tx, typ, r); err != nil {
		return err
	}
	if err := typ.AfterFinish(ctx, tx, r); err != nil {
		return err
	}
	return c.releaseGroup(ctx, tx, r)
}

// releaseGroup 在请求结束后启动并发组中排队的请求（组内没有其他执行中的请求时）。调用方已经持有并发组的锁。
func (c *Core) releaseGroup(ctx context.Context, tx pgx.Tx, r *Request) error {
	if r.GroupKey == nil {
		return nil
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ai_requests WHERE repo_id=$1 AND group_key=$2 AND state='active'`,
		r.RepoID, *r.GroupKey).Scan(&active); err != nil || active > 0 {
		return err
	}
	ids, err := store.QueryIDs(ctx, tx, `SELECT id FROM ai_requests WHERE repo_id=$1 AND group_key=$2 AND state='queued' ORDER BY id DESC LIMIT 1`,
		r.RepoID, *r.GroupKey)
	if err != nil || len(ids) == 0 {
		return err
	}
	return c.activate(ctx, tx, ids[0])
}

// activate 把排队的请求置为 active，它的任务进入待领取。
func (c *Core) activate(ctx context.Context, tx pgx.Tx, id int64) error {
	r, err := GetRequest(ctx, tx, id, true)
	if err != nil || r.State != RequestQueued {
		return err
	}
	typ, err := c.types.Get(r.Type)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_requests SET state='active', updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	tasks, err := QueryTasks(ctx, tx, `request_id=$1 AND state='waiting' ORDER BY id`, id)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if _, err := tx.Exec(ctx, `UPDATE ai_tasks SET state='pending', queued_at=now(), reason=$2 WHERE id=$1`, t.ID, waitReason(typ, t.Platforms)); err != nil {
			return err
		}
		if err := c.enqueueTaskCheck(ctx, tx, typ, t.ID, id); err != nil {
			return err
		}
	}
	r.State = RequestActive
	if err := c.enqueueRequestCheck(ctx, tx, typ, r); err != nil {
		return err
	}
	return c.settle(ctx, tx, r)
}

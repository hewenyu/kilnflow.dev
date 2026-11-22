package admin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// Runner 管理（CLI 和 Dashboard 共用）。

const (
	// RunnerOfflineAfter 是 Runner 多久没有联系 Server 后显示为 offline（CLI 和 Dashboard 共用）。
	RunnerOfflineAfter = 2 * time.Minute
	// defaultEnrollTokenTTL 是注册令牌的默认有效期。
	defaultEnrollTokenTTL = time.Hour
)

// maxEnrollTokenTTLMinutes 是 CreateEnrollToken 请求中有效期（分钟）的上限：再大换算成 time.Duration 时会溢出，
// 得到负数（被当作默认值）或任意的有效期。
const maxEnrollTokenTTLMinutes = int64(math.MaxInt64 / time.Minute)

func (a *rpcAPI) CreateEnrollToken(ctx context.Context, req *connect.Request[apiv1.CreateEnrollTokenRequest]) (*connect.Response[apiv1.CreateEnrollTokenResponse], error) {
	if int64(req.Msg.TtlMinutes) > maxEnrollTokenTTLMinutes {
		return nil, invalid(fmt.Errorf("有效期太长：%d 分钟", req.Msg.TtlMinutes))
	}
	ttl := time.Duration(req.Msg.TtlMinutes) * time.Minute
	tok, exp, err := a.s.CreateEnrollToken(ctx, req.Msg.Pool, req.Msg.Kind, req.Msg.Role, ttl, rpcActor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.CreateEnrollTokenResponse{Token: tok, ExpiresAt: exp.Unix()}), nil
}

// CreateEnrollToken 生成 Runner 的一次性注册令牌，在同一个事务中记录审计日志（没有审计记录的令牌不会生成）。
// pool 为空时是 default；kind 为空时是 CI Runner 的令牌，不是 ci 或 ai 时返回 InvalidArgument；
// ttl 不大于 0 时使用默认有效期（1 小时）。
func (s *Service) CreateEnrollToken(ctx context.Context, pool, kind, role string, ttl time.Duration, actor string) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = defaultEnrollTokenTTL
	}
	if pool == "" {
		pool = store.DefaultPool
	}
	if kind == "" {
		kind = store.RunnerKindCI
	}
	if !store.ValidRunnerKind(kind) {
		return "", time.Time{}, invalid(fmt.Errorf("无效的 Runner 类型 %q（%s 或 %s）", kind, store.RunnerKindCI, store.RunnerKindAI))
	}
	role, err := store.RunnerRole(kind, role)
	if err != nil {
		return "", time.Time{}, invalid(err)
	}
	var tok string
	var exp time.Time
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if tok, exp, err = auth.CreateEnrollToken(ctx, tx, pool, kind, role, ttl); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "runner.token_create", map[string]any{"pool": pool, "kind": kind, "role": role})
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

func (a *rpcAPI) ListRunners(ctx context.Context, _ *connect.Request[apiv1.ListRunnersRequest]) (*connect.Response[apiv1.ListRunnersResponse], error) {
	list, err := a.s.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.ListRunnersResponse{Runners: list}), nil
}

// RunnerOnline 判断 Runner 在 now 时是否在线：最近 RunnerOfflineAfter 内联系过 Server（CLI 和 Dashboard 共用）。
func RunnerOnline(r *store.Runner, now time.Time) bool {
	return r.LastSeen != nil && now.Sub(*r.LastSeen) <= RunnerOfflineAfter
}

// RunnerState 返回 Runner 在 now 时的显示状态：已吊销为 revoked；否则暂停接单为 draining，离线为 offline，其余为 active。
func RunnerState(r *store.Runner, now time.Time) string {
	switch {
	case r.State != store.RunnerActive:
		return r.State
	case r.Drain:
		return store.RunnerDraining
	case !RunnerOnline(r, now):
		return store.RunnerOffline
	}
	return store.RunnerActive
}

// ListRunners 返回全部 CI Runner 及其显示状态（见 RunnerState）和正在执行的 job 数（AI 执行节点见 ListAIRunners）。
func (s *Service) ListRunners(ctx context.Context) ([]*apiv1.Runner, error) {
	runners, err := cistore.ListCIRunners(ctx, s.db, store.RunnerFilter{})
	if err != nil || len(runners) == 0 {
		return nil, err
	}
	ids := make([]int64, len(runners))
	for i, r := range runners {
		ids[i] = r.ID
	}
	jobs, err := cistore.RunningJobs(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	running := map[int64]int{}
	for _, j := range jobs {
		running[j.RunnerID]++
	}
	alloc, err := cistore.RunnerAllocations(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	upgrades, err := upgradectl.RunnerInfos(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]*apiv1.Runner, 0, len(runners))
	for _, r := range runners {
		v := &apiv1.Runner{
			Id: r.ID, Name: r.Name, Pool: r.Pool, Kind: r.Kind, Platforms: r.Platforms, Executors: r.Executors, Version: r.Version,
			State: RunnerState(&r.Runner, now), HostFingerprint: r.HostFingerprint, RunningJobs: clampInt32(running[r.ID]),
			Capacity: capacityView(r.Capacity, alloc[r.ID]), CacheBytes: cacheBytes(r.CacheReport), Upgrade: upgrades[r.ID],
		}
		if r.CacheReportedAt != nil {
			v.CacheReportedAt = r.CacheReportedAt.Unix()
		}
		if r.LastSeen != nil {
			// 与原来 SQL 中 extract(epoch ...)::bigint 一致：四舍五入到秒
			v.LastSeenAt = r.LastSeen.Round(time.Second).Unix()
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *rpcAPI) RevokeRunner(ctx context.Context, req *connect.Request[apiv1.RevokeRunnerRequest]) (*connect.Response[apiv1.RevokeRunnerResponse], error) {
	if err := a.s.RevokeRunner(ctx, req.Msg.Name, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.RevokeRunnerResponse{}), nil
}

func (a *rpcAPI) DrainRunner(ctx context.Context, req *connect.Request[apiv1.DrainRunnerRequest]) (*connect.Response[apiv1.DrainRunnerResponse], error) {
	if err := a.s.SetRunnerDrain(ctx, req.Msg.Name, req.Msg.Drain, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.DrainRunnerResponse{}), nil
}

// RevokeRunner 吊销 Runner（立即生效，PLAN #9.3），同时取消它进行中的升级请求（PLAN #27.13）。
func (s *Service) RevokeRunner(ctx context.Context, name, actor string) error {
	defer s.hub.All()
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `UPDATE runners SET state=$2 WHERE name=$1 RETURNING id`, name, store.RunnerRevoked).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("没有 Runner %s", name)
		}
		if err != nil {
			return err
		}
		if err := upgradectl.CancelForRevoke(ctx, tx, id, name, actor); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "runner.revoke", map[string]any{"name": name})
	})
}

func (a *rpcAPI) DeleteRunner(ctx context.Context, req *connect.Request[apiv1.DeleteRunnerRequest]) (*connect.Response[apiv1.DeleteRunnerResponse], error) {
	if err := a.s.DeleteRunner(ctx, req.Msg.Name, rpcActor(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.DeleteRunnerResponse{}), nil
}

// DeleteRunner 删除已吊销的 Runner（PLAN #9.3）：从列表中移除，名称可以重新注册。
// 它的 attempt 属于结果、保留，记下 Runner 的名称（attempts、ai_attempts 的 runner_name）；缓存清理请求、认证随机数随之删除。
// 没有吊销、或还有没结束的任务（吊销时正在执行、租约还没过期）时拒绝。
// 锁住 runners 行：并发的同名注册会等到删除提交后再按新 Runner 注册（吊销的 Runner 也不能再领取任务）。
func (s *Service) DeleteRunner(ctx context.Context, name, actor string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var id int64
		var state string
		err := tx.QueryRow(ctx, `SELECT id, state FROM runners WHERE name=$1 FOR UPDATE`, name).Scan(&id, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("没有 Runner %s", name)
		}
		if err != nil {
			return err
		}
		if state != store.RunnerRevoked {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("没有吊销 Runner %s，先吊销再删除", name))
		}
		// 两个领域分别检查执行中的任务（PLAN #30.9）：CI 的 job、AI 的任务
		var running int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id
				WHERE j.state='running' AND a.runner_id=$1)
			+ (SELECT count(*) FROM ai_tasks t JOIN ai_attempts a ON a.id=t.current_attempt_id
				WHERE t.state='running' AND a.runner_id=$1)`, id).Scan(&running); err != nil {
			return err
		}
		if running > 0 {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s 还有 %d 个任务没有结束（租约过期后判为 Runner 失联），之后再删除", name, running))
		}
		// attempt 属于结果、保留：两个领域的 attempt 都记下 Runner 的名称（删除后 runner_id 置空）
		var attempts int64
		for _, q := range []string{
			`UPDATE attempts SET runner_name=$2 WHERE runner_id=$1`,
			`UPDATE ai_attempts SET runner_name=$2 WHERE runner_id=$1`,
		} {
			tag, err := tx.Exec(ctx, q, id, name)
			if err != nil {
				return err
			}
			attempts += tag.RowsAffected()
		}
		for _, q := range []string{
			`DELETE FROM auth_nonces WHERE runner_id=$1`,
			`UPDATE enrollment_tokens SET runner_id=NULL WHERE runner_id=$1`,
			`DELETE FROM runners WHERE id=$1`,
		} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, actor, "runner.delete", map[string]any{"name": name, "runner_id": id, "attempts": attempts})
	})
}

// SetRunnerDrain 设置或取消 Runner 的 drain（不再领取新 job）。
func (s *Service) SetRunnerDrain(ctx context.Context, name string, drain bool, actor string) error {
	action := "runner.undrain"
	if drain {
		action = "runner.drain"
	}
	return s.updateRunner(ctx, name, actor, action, `UPDATE runners SET drain=$2 WHERE name=$1`, drain)
}

// updateRunner 按名称更新一个 Runner 并记录审计日志。sql 的 $1 是 Runner 名称，$2 是 arg。
// 提交后唤醒长轮询中的 Acquire：暂停接单、吊销立即生效，Runner 也能及时收到最新的指令。
func (s *Service) updateRunner(ctx context.Context, name, actor, action, sql string, arg any) error {
	defer s.hub.All()
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, name, arg)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return notFound("没有 Runner %s", name)
		}
		return store.Audit(ctx, tx, actor, action, map[string]any{"name": name})
	})
}

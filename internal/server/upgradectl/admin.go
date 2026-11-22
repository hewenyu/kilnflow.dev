package upgradectl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// 管理操作（PLAN #27.13、#27.14，CLI 和 Dashboard 共用）。参数错误返回 *InvalidError，当前状态不允许时返回 *RefusedError。

// InvalidError 是管理操作的参数错误。Msg 是给 CLI 和日志的说明。
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// RefusedError 表示 Runner 当前的状态不允许这个操作（已是最新、需要 service install、没有进行中的请求等）。
// Msg 是给 CLI 和日志的中文说明（带 Runner 名称）；Reason 是稳定的原因码和原始参数，Dashboard 按它显示当前语言的说明。
type RefusedError struct {
	Msg    string
	Reason Reason
}

func (e *RefusedError) Error() string { return e.Msg }

// refusedf 返回 Reason 为 reason、说明为 format 的 *RefusedError。
func refusedf(reason Reason, format string, args ...any) error {
	return &RefusedError{Msg: fmt.Sprintf(format, args...), Reason: reason}
}

// ReasonCode 是不能升级或拒绝升级操作的稳定原因码（不随语言变化；Dashboard 按它取文案，见 internal/server/web）。
type ReasonCode string

// 原因码。Blocker 给出前 10 个（现在不能请求升级的原因），RunnerInfos 另外给出 ReasonUpgrading；
// 其余只出现在 *RefusedError 中。
const (
	// ReasonAINode：AI 执行节点没有按台升级（连接器用 npm 升级）。
	ReasonAINode ReasonCode = "ai_node"
	// ReasonRevoked：Runner 已吊销。
	ReasonRevoked ReasonCode = "revoked"
	// ReasonUnsupported：Runner 没有上报过升级状态（版本不支持按台升级）。
	ReasonUnsupported ReasonCode = "unsupported"
	// ReasonNoLauncher：没有经过启动器运行，不能自动切换。
	ReasonNoLauncher ReasonCode = "no_launcher"
	// ReasonDisabled：config.toml 中 upgrade = false。
	ReasonDisabled ReasonCode = "disabled"
	// ReasonCheckFailed：还没有查到最新版本，最近一次查询失败（Detail 是 Runner 上报的错误原文）。
	ReasonCheckFailed ReasonCode = "check_failed"
	// ReasonNotChecked：还没有查到最新版本。
	ReasonNotChecked ReasonCode = "not_checked"
	// ReasonUpToDate：已是最新版本。
	ReasonUpToDate ReasonCode = "up_to_date"
	// ReasonNewer：当前版本（Current）高于 npm 上的最新正式版本（Latest），不降级。
	ReasonNewer ReasonCode = "newer"
	// ReasonUpgrading：正在升级到 Version。
	ReasonUpgrading ReasonCode = "upgrading"
	// ReasonLatestChanged：Runner 现在查到的最新版本（Latest）不是管理员确认的版本（Expected）。
	ReasonLatestChanged ReasonCode = "latest_changed"
	// ReasonNoPending：没有进行中的升级请求。
	ReasonNoPending ReasonCode = "no_pending"
	// ReasonSwitching：已经切换到 Version、正在确认，不能取消。
	ReasonSwitching ReasonCode = "switching"
)

// Reason 是不能升级或拒绝操作的原因：原因码和原始参数（版本号、错误原文都不翻译）。零值表示没有原因（可以升级）。
type Reason struct {
	Code ReasonCode
	// Current 是 Runner 的版本，Latest 是 Runner 查到的最新正式版本，Version 是升级请求的目标版本，
	// Expected 是管理员确认的目标版本，Detail 是 Runner 上报的查询错误原文。没有用到的为空。
	Current, Latest, Version, Expected, Detail string
}

// IsZero 表示没有原因。
func (r Reason) IsZero() bool { return r.Code == "" }

// String 返回原因的中文说明（CLI 的 kiln runner list、日志和 RunnerUpgradeInfo.reason）；没有原因时为空。
func (r Reason) String() string {
	switch r.Code {
	case ReasonAINode:
		return "AI 执行节点没有按台升级：在执行机器上用 npm 升级连接器（npm i -g @<scope>/kiln-orca）"
	case ReasonRevoked:
		return "Runner 已吊销"
	case ReasonUnsupported:
		return "这台 Runner 的版本不支持按台升级：在机器上手动换成新版本并执行一次 " + upgrade.ProgramCI + " service install"
	case ReasonNoLauncher:
		return "没有经过启动器运行（前台 run 或旧的服务入口），不能自动切换：在机器上执行一次 " + upgrade.ProgramCI + " service install"
	case ReasonDisabled:
		return "这台机器的 config.toml 中 upgrade = false，本地关闭了自升级"
	case ReasonCheckFailed:
		return "还没有查到最新版本：" + r.Detail
	case ReasonNotChecked:
		return "还没有查到最新版本（Runner 连接 Server 后会查询 npm）"
	case ReasonUpToDate:
		return "已是最新版本"
	case ReasonNewer:
		return fmt.Sprintf("当前版本 %s 高于 npm 上的最新正式版本 %s，不降级", r.Current, r.Latest)
	case ReasonUpgrading:
		return "正在升级到 " + r.Version
	case ReasonLatestChanged:
		return fmt.Sprintf("现在查到的最新版本是 %s，不是确认的 %s；刷新后重新确认", r.Latest, r.Expected)
	case ReasonNoPending:
		return "没有进行中的升级请求"
	case ReasonSwitching:
		return fmt.Sprintf("已经切换到 %s、正在确认，不能取消；确认失败时会自动回滚", r.Version)
	}
	return string(r.Code)
}

// ErrRunnerNotFound 表示 Runner 不存在。
var ErrRunnerNotFound = errors.New("找不到这台 Runner")

// registryKey 是 kv 中保存 registry 设置的键。
const registryKey = "runner_upgrade.registry"

// GetRegistry 返回设置的 registry；没有设置时为空（Runner 用官方 registry）。
func GetRegistry(ctx context.Context, q store.Querier) (string, error) {
	var v string
	err := q.QueryRow(ctx, `SELECT value FROM kv WHERE key=$1`, registryKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Overview 返回 registry 设置。
func Overview(ctx context.Context, q store.Querier) (*apiv1.GetRunnerUpgradeResponse, error) {
	reg, err := GetRegistry(ctx, q)
	if err != nil {
		return nil, err
	}
	return &apiv1.GetRunnerUpgradeResponse{Registry: reg, DefaultRegistry: upgrade.DefaultRegistry}, nil
}

// SetRegistry 设置 Runner 查询和下载升级包用的 registry（只有地址，不带 token）；为空表示恢复为官方 registry。
func SetRegistry(ctx context.Context, db *store.DB, registry, actor string) error {
	registry = strings.TrimSpace(registry)
	if registry != "" {
		if err := npm.CheckRegistry(registry); err != nil {
			return &InvalidError{Msg: err.Error()}
		}
	}
	return db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		if registry == "" {
			_, err = tx.Exec(ctx, `DELETE FROM kv WHERE key=$1`, registryKey)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO kv (key, value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`,
				registryKey, registry)
		}
		if err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "runner.upgrade.registry", map[string]any{"registry": upgrade.Registry(registry)})
	})
}

// Blocker 返回现在不能请求升级的原因（零值表示可以升级到 st.Latest）。current 是 Runner 上报的版本，kind 是 Runner 类型。
// 离线的 Runner 可以请求（上线后执行），这里不判断。
func Blocker(kind, current string, revoked bool, st *runnerv1.UpgradeStatus) Reason {
	switch {
	case kind == store.RunnerKindAI:
		// AI 执行节点的连接器（kiln-orca）用 npm 升级，Server 不参与（PLAN #33.3）
		return Reason{Code: ReasonAINode}
	case revoked:
		return Reason{Code: ReasonRevoked}
	case st == nil:
		return Reason{Code: ReasonUnsupported}
	case !st.Launcher:
		return Reason{Code: ReasonNoLauncher}
	case st.Disabled:
		return Reason{Code: ReasonDisabled}
	case st.Latest == "" && st.CheckError != "":
		return Reason{Code: ReasonCheckFailed, Detail: st.CheckError}
	case st.Latest == "":
		return Reason{Code: ReasonNotChecked}
	case upgrade.Normalize(current) == st.Latest:
		return Reason{Code: ReasonUpToDate, Current: current, Latest: st.Latest}
	case !upgrade.Newer(st.Latest, current):
		return Reason{Code: ReasonNewer, Current: current, Latest: st.Latest}
	}
	return Reason{}
}

// lockedRunner 是锁住的 Runner 行。
type lockedRunner struct {
	ID                  int64
	Kind, Version, Name string
	Revoked             bool
	Status              *runnerv1.UpgradeStatus
}

// lockRunner 按名称锁住 Runner 行（先锁 Runner 再锁请求，与 Acquire 记录状态的顺序相同）。
func lockRunner(ctx context.Context, tx pgx.Tx, name string) (*lockedRunner, error) {
	r := &lockedRunner{Name: name}
	var (
		state string
		raw   []byte
	)
	err := tx.QueryRow(ctx, `SELECT id, kind, version, state, upgrade FROM runners WHERE name=$1 FOR UPDATE`, name).
		Scan(&r.ID, &r.Kind, &r.Version, &state, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunnerNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Revoked = state != store.RunnerActive
	r.Status = parseStatus(raw)
	return r, nil
}

func parseStatus(raw []byte) *runnerv1.UpgradeStatus {
	if raw == nil {
		return nil
	}
	var st runnerv1.UpgradeStatus
	if err := protojson.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

const requestColumns = `id, version, from_version, state, error, actor, created_at, finished_at`

func scanRequest(row pgx.Row) (*apiv1.RunnerUpgradeRequest, error) {
	var (
		m        apiv1.RunnerUpgradeRequest
		created  time.Time
		finished *time.Time
	)
	if err := row.Scan(&m.Id, &m.Version, &m.FromVersion, &m.State, &m.Error, &m.Actor, &created, &finished); err != nil {
		return nil, err
	}
	m.CreatedAt = created.Unix()
	if finished != nil {
		m.FinishedAt = finished.Unix()
	}
	return &m, nil
}

// pending 返回 Runner 进行中的请求（锁住），没有时为 nil。
func pending(ctx context.Context, tx pgx.Tx, runnerID int64) (*apiv1.RunnerUpgradeRequest, error) {
	m, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM runner_upgrade_requests
		WHERE runner_id=$1 AND state='pending' FOR UPDATE`, runnerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// RequestUpgrade 请求把 Runner 升级到它最后一次上报的最新版本（PLAN #27.13"创建"）。已有进行中的请求时返回那一个
// （created 为 false）：重复点击、浏览器重试不会生成第二个。目标在这时固定，之后 npm 上发布了新版本也不变。
//
// expect 是管理员确认的目标版本：不为空时必须等于 Runner 现在上报的 latest，否则拒绝（页面打开、确认期间 Runner 查到了
// 更新的版本），不会升级到没有确认过的版本。
func RequestUpgrade(ctx context.Context, db *store.DB, runner, expect, actor string) (req *apiv1.RunnerUpgradeRequest, created bool, err error) {
	err = db.Tx(ctx, func(tx pgx.Tx) error {
		req, created = nil, false
		r, err := lockRunner(ctx, tx, runner)
		if err != nil {
			return err
		}
		if !r.Revoked {
			if req, err = pending(ctx, tx, r.ID); err != nil || req != nil {
				return err
			}
		}
		if reason := Blocker(r.Kind, r.Version, r.Revoked, r.Status); !reason.IsZero() {
			return &RefusedError{Msg: runner + "：" + reason.String(), Reason: reason}
		}
		latest := r.Status.Latest
		if expect != "" && upgrade.Normalize(expect) != latest {
			reason := Reason{Code: ReasonLatestChanged, Latest: latest, Expected: upgrade.Normalize(expect)}
			return refusedf(reason, "%s %s", runner, reason)
		}
		if req, err = scanRequest(tx.QueryRow(ctx, `INSERT INTO runner_upgrade_requests (runner_id, version, from_version, actor)
			VALUES ($1,$2,$3,$4) RETURNING `+requestColumns, r.ID, latest, upgrade.Normalize(r.Version), actor)); err != nil {
			return err
		}
		created = true
		return store.Audit(ctx, tx, actor, "runner.upgrade.requested", map[string]any{
			"runner_id": r.ID, "runner": runner, "kind": r.Kind, "from": r.Version, "to": latest, "request_id": req.Id,
		})
	})
	return req, created, err
}

// CancelUpgrade 取消 Runner 进行中的请求。Runner 已经切换到目标版本、正在确认（switching）时不能取消。
func CancelUpgrade(ctx context.Context, db *store.DB, runner, actor string) (*apiv1.RunnerUpgradeRequest, error) {
	var out *apiv1.RunnerUpgradeRequest
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		r, err := lockRunner(ctx, tx, runner)
		if err != nil {
			return err
		}
		req, err := pending(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if req == nil {
			reason := Reason{Code: ReasonNoPending}
			return refusedf(reason, "%s %s", runner, reason)
		}
		if st := r.Status; st != nil && st.State == StateSwitching && upgrade.Normalize(st.Target) == req.Version {
			reason := Reason{Code: ReasonSwitching, Version: req.Version}
			return refusedf(reason, "%s %s", runner, reason)
		}
		if err := finish(ctx, tx, req.Id, RequestCancelled, "", actor, map[string]any{
			"runner_id": r.ID, "runner": runner, "kind": r.Kind, "to": req.Version,
		}); err != nil {
			return err
		}
		out, err = scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM runner_upgrade_requests WHERE id=$1`, req.Id))
		return err
	})
	return out, err
}

// CancelForRevoke 在吊销 Runner 的事务中取消它进行中的请求（调用方已经锁住或更新了 Runner 行）。
func CancelForRevoke(ctx context.Context, tx pgx.Tx, runnerID int64, name, actor string) error {
	req, err := pending(ctx, tx, runnerID)
	if err != nil || req == nil {
		return err
	}
	return finish(ctx, tx, req.Id, RequestCancelled, "Runner 已吊销", actor, map[string]any{
		"runner_id": runnerID, "runner": name, "to": req.Version,
	})
}

// RequestCheck 让 Runner 立即查询最新版本（检查序号 + 1，随下一次 Acquire 下发）。runner 为空表示全部没有吊销的 Runner。
// 返回请求检查的 Runner 数量。
func RequestCheck(ctx context.Context, db *store.DB, runner, actor string) (int, error) {
	n := 0
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		sql := `UPDATE runners SET upgrade_check=upgrade_check+1 WHERE state='active'`
		args := []any{}
		if runner != "" {
			var state string
			err := tx.QueryRow(ctx, `SELECT state FROM runners WHERE name=$1`, runner).Scan(&state)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRunnerNotFound
			}
			if err != nil {
				return err
			}
			if state != store.RunnerActive {
				return refusedf(Reason{Code: ReasonRevoked}, "%s 已吊销", runner)
			}
			sql += ` AND name=$1`
			args = append(args, runner)
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return store.Audit(ctx, tx, actor, "runner.upgrade.check", map[string]any{"runner": runner, "count": n})
	})
	return n, err
}

// RunnerInfo 是一台 Runner 的升级信息：Info 是 Admin API 的格式（reason 是中文说明），Reason 是不能请求升级的原因码和参数
// （Info.can_upgrade 为 true 时是零值）。
type RunnerInfo struct {
	Info   *apiv1.RunnerUpgradeInfo
	Reason Reason
}

// RunnerInfos 返回 Runner 的升级信息（ids 中的每一个）：上报的状态、最新版本、最近一次请求，以及现在能否请求升级。
func RunnerInfos(ctx context.Context, q store.Querier, ids []int64) (map[int64]RunnerInfo, error) {
	rows, err := q.Query(ctx, `SELECT id, kind, version, state, upgrade, upgrade_check FROM runners WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID                   int64
		Kind, Version, State string
		Status               []byte
		Check                int64
	}
	rs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	reqs, err := latestRequests(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]RunnerInfo, len(rs))
	for _, r := range rs {
		st := parseStatus(r.Status)
		info := &apiv1.RunnerUpgradeInfo{Request: reqs[r.ID]}
		if st != nil {
			info.State, info.Target, info.Error, info.Launcher, info.Disabled = st.State, st.Target, st.Error, st.Launcher, st.Disabled
			info.Current, info.Previous = st.Current, st.Previous
			info.Package, info.Latest, info.CheckedAt, info.CheckError = st.Package, st.Latest, st.CheckedAt, st.CheckError
			info.CheckPending = r.State == store.RunnerActive && st.CheckSeq < r.Check
		}
		reason := Blocker(r.Kind, r.Version, r.State != store.RunnerActive, st)
		if req := info.Request; req != nil && req.State == RequestPending && reason.IsZero() {
			reason = Reason{Code: ReasonUpgrading, Version: req.Version}
		}
		info.Reason, info.CanUpgrade = reason.String(), reason.IsZero()
		out[r.ID] = RunnerInfo{Info: info, Reason: reason}
	}
	return out, nil
}

// latestRequests 返回每台 Runner 最近一次的升级请求。
func latestRequests(ctx context.Context, q store.Querier, ids []int64) (map[int64]*apiv1.RunnerUpgradeRequest, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (runner_id) runner_id, `+requestColumns+` FROM runner_upgrade_requests
		WHERE runner_id = ANY($1) ORDER BY runner_id, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*apiv1.RunnerUpgradeRequest{}
	for rows.Next() {
		var (
			runnerID int64
			m        apiv1.RunnerUpgradeRequest
			created  time.Time
			finished *time.Time
		)
		if err := rows.Scan(&runnerID, &m.Id, &m.Version, &m.FromVersion, &m.State, &m.Error, &m.Actor, &created, &finished); err != nil {
			return nil, err
		}
		m.CreatedAt = created.Unix()
		if finished != nil {
			m.FinishedAt = finished.Unix()
		}
		out[runnerID] = &m
	}
	return out, rows.Err()
}

// History 返回 Runner 最近的升级请求（新的在前，最多 limit 个）。
func History(ctx context.Context, q store.Querier, runnerID int64, limit int) ([]*apiv1.RunnerUpgradeRequest, error) {
	rows, err := q.Query(ctx, `SELECT `+requestColumns+` FROM runner_upgrade_requests WHERE runner_id=$1 ORDER BY id DESC LIMIT $2`,
		runnerID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*apiv1.RunnerUpgradeRequest, error) { return scanRequest(row) })
}

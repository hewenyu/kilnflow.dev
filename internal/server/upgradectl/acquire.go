// Package upgradectl 是 Runner 按台升级的控制面（PLAN #27.11–#27.13）：记录 Runner 上报的升级状态、按管理员的请求
// 下发升级指令并在 Runner 上报结果时结束请求，以及管理操作（CLI 和 Dashboard 共用）。Server 不访问 npm：最新版本由
// Runner 自己查询并上报，升级包由 Runner 自己下载、核对。
package upgradectl

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// Runner 上报的升级状态（UpgradeStatus.state）。
const (
	StateIdle        = "idle"
	StateDownloading = "downloading"
	StateReady       = "ready"
	StateDraining    = "draining"
	StateSwitching   = "switching"
	StateConfirmed   = "confirmed"
	StateFailed      = "failed"
	StateRolledBack  = "rolled_back"
	StateManual      = "manual"
)

// 升级请求的状态（runner_upgrade_requests.state）。
const (
	RequestPending   = "pending"
	RequestSucceeded = "succeeded"
	RequestFailed    = "failed"
	RequestCancelled = "cancelled"
)

// auditActor 是审计日志中 Runner 上报导致的自动操作（请求成功、失败）的操作者。
const auditActor = "runner-upgrade"

// maxErrorLen 是请求中记下的错误的长度上限。
const maxErrorLen = 1000

// DBTX 是可以查询和执行写语句的连接（*store.DB 或 pgx.Tx）。
type DBTX interface {
	store.Querier
	store.Execer
}

// AcquireRunner 是 Acquire 时的 Runner。
type AcquireRunner struct {
	ID   int64
	Name string
	Kind string
	// Version 是这次上报的版本（buildinfo，发布构建带 v）。
	Version string
	// Status 是这次上报的升级状态（旧 Runner 为 nil）。
	Status *runnerv1.UpgradeStatus
}

// Result 是 Acquire 时升级控制面要下发的内容。
type Result struct {
	// Directive 是进行中的请求对应的指令；没有请求时为 nil。
	Directive *runnerv1.UpgradeDirective
	// Registry 是设置的 registry（为空表示官方 registry），Check 是检查序号。
	Registry string
	Check    int64
}

// Fill 把结果写到 d，返回是否与之前不同。
func (r *Result) Fill(d *runnerv1.RunnerDirectives) bool {
	changed := !proto.Equal(d.Upgrade, r.Directive) || d.UpgradeRegistry != r.Registry || d.UpgradeCheck != r.Check
	d.Upgrade, d.UpgradeRegistry, d.UpgradeCheck = r.Directive, r.Registry, r.Check
	return changed
}

// Acquire 记录 Runner 上报的升级状态（必要时结束进行中的请求），返回这一轮要下发的内容（PLAN #27.13）。每次 Acquire 调用一次。
func Acquire(ctx context.Context, db *store.DB, r AcquireRunner) (*Result, error) {
	if err := recordStatus(ctx, db, r); err != nil {
		return nil, fmt.Errorf("记录升级状态：%w", err)
	}
	return Refresh(ctx, db, r.ID)
}

// Refresh 按当前的请求、registry 和检查序号重新决定要下发的内容，不记录上报的状态。长轮询被唤醒和到期返回前用它：
// 期间创建、取消了请求或请求了检查时下发最新的。
func Refresh(ctx context.Context, q store.Querier, runnerID int64) (*Result, error) {
	registry, err := GetRegistry(ctx, q)
	if err != nil {
		return nil, err
	}
	res := &Result{Registry: registry}
	var (
		reqID   int64
		version string
	)
	err = q.QueryRow(ctx, `SELECT r.upgrade_check, COALESCE(u.id, 0), COALESCE(u.version, '') FROM runners r
		LEFT JOIN runner_upgrade_requests u ON u.runner_id = r.id AND u.state = 'pending' WHERE r.id = $1`, runnerID).
		Scan(&res.Check, &reqID, &version)
	if err != nil {
		return nil, err
	}
	if reqID != 0 {
		res.Directive = &runnerv1.UpgradeDirective{Version: version, Generation: reqID}
	}
	return res, nil
}

// settle 判断 Runner 上报的状态是否结束了请求 id（目标 version）：返回请求的新状态和原因，没有结束时返回空。
// 之前的请求留下的失败不会结束新的请求：failed 按 generation 区分，失败列表按记录的 generation（不小于请求的 ID）区分。
// 失败列表中有这一轮的记录（integrity 不符、回滚）时，不论现在上报的是什么状态都结束为失败：Runner 不会再尝试这一轮，
// 而 Runner 重启后上报的是 idle。
func settle(id int64, version, current string, st *runnerv1.UpgradeStatus) (state, msg string) {
	target, cur := upgrade.Normalize(st.Target), upgrade.Normalize(current)
	switch {
	case cur == version && st.State != StateSwitching:
		// 新版本在本地确认（写 state.json）之后才上报 confirmed；试用中（switching）的上报不算成功：确认可能失败、会回滚
		return RequestSucceeded, ""
	case cur != "" && upgrade.Compare(cur, version) > 0 && st.State != StateSwitching:
		// 期间手动升级到了更高的版本：不再执行（否则 Runner 会降级）
		return RequestCancelled, "Runner 已经是更高的版本 " + cur
	case st.State == StateFailed && target == version && st.Generation == id:
		return RequestFailed, nonEmpty(st.Error, "升级失败")
	case slices.ContainsFunc(st.Failed, func(f *runnerv1.FailedVersion) bool { return f.Version == version && f.Generation >= id }):
		if st.State == StateRolledBack && target == version {
			return RequestFailed, "新版本没有通过确认，已回滚到原来的版本：" + nonEmpty(st.Error, "原因未知")
		}
		return RequestFailed, "Runner 记录了这个版本这一轮安装失败（升级包核对不符或启动后回滚），不会再尝试；重新请求升级会再试一次"
	case st.State == StateManual && target == version:
		return RequestFailed, nonEmpty(st.Error, "需要手动升级")
	}
	return "", ""
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// recordStatus 保存 Runner 上报的升级状态，并在同一事务中按它结束进行中的请求（成功、失败、回滚、需要手动升级），
// 写审计 runner.upgrade.succeeded / runner.upgrade.failed。
func recordStatus(ctx context.Context, db *store.DB, r AcquireRunner) error {
	if r.Status == nil {
		return nil
	}
	b, err := protojson.Marshal(r.Status)
	if err != nil {
		return err
	}
	return db.Tx(ctx, func(tx pgx.Tx) error {
		// 先锁 Runner 行再锁请求，与创建、取消请求的顺序相同
		if _, err := tx.Exec(ctx, `UPDATE runners SET upgrade=$2 WHERE id=$1`, r.ID, b); err != nil {
			return err
		}
		var (
			id            int64
			version, from string
		)
		err := tx.QueryRow(ctx, `SELECT id, version, from_version FROM runner_upgrade_requests WHERE runner_id=$1 AND state='pending' FOR UPDATE`, r.ID).
			Scan(&id, &version, &from)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		state, msg := settle(id, version, r.Version, r.Status)
		if state == "" {
			return nil
		}
		return finish(ctx, tx, id, state, msg, auditActor, map[string]any{
			"runner_id": r.ID, "runner": r.Name, "kind": r.Kind, "from": from, "to": version, "version": r.Version,
		})
	})
}

// finish 结束请求 id（只在仍是 pending 时）并写审计 runner.upgrade.<state>。
func finish(ctx context.Context, tx pgx.Tx, id int64, state, msg, actor string, detail map[string]any) error {
	msg = shorten.Text(msg, maxErrorLen)
	tag, err := tx.Exec(ctx, `UPDATE runner_upgrade_requests SET state=$2, error=$3, finished_at=now() WHERE id=$1 AND state='pending'`,
		id, state, msg)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	detail["request_id"] = id
	if msg != "" {
		detail["error"] = msg
	}
	return store.Audit(ctx, tx, actor, "runner.upgrade."+state, detail)
}

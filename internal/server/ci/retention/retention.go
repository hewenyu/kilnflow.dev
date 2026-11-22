// Package retention 按保留策略清理 Server 上的过程数据（PLAN #10.3、#10.4）：run 结束超过保留期（默认 12 小时）后，
// 删除它的日志和不再被引用的产物（CAS 对象），以及过期的缓存条目、已处理的 webhook 事件、已投递的 outbox 记录、
// 过期的注册令牌和发布 job 残留的工作目录。只保留结果：run、job、attempt 的状态、时间、原因和退出码，以及发布记录和审计日志。
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const (
	// DefaultRetention 是默认的保留时间。
	DefaultRetention = 12 * time.Hour
	// DefaultInterval 是默认的清理间隔。
	DefaultInterval = time.Hour
	// startDelay 是启动后第一次清理前的等待时间（避开启动时的其他工作）。
	startDelay = time.Minute
	// runBatch 是每次读取的待清理 run 数。
	runBatch = 100
	// maxRunErrors 是一轮清理中最多容忍的 run 清理失败数，达到后停止清理 run（下一轮再试）。
	maxRunErrors = 10
	// blobBatch 是每次读取的待清理对象数，也是登记孤立文件时每条语句的对象数。
	blobBatch = 1000
	// auditActor 是清理写入审计日志时的操作者。
	auditActor = "kiln"
)

// Blobs 是清理需要的 CAS 操作（cas.FSStore 实现）。
type Blobs interface {
	cas.Store
	Delete(ctx context.Context, digest string) error
	Walk(ctx context.Context, fn func(cas.Object) error) error
}

// Config 是保留策略的配置。
type Config struct {
	// Retention 是 run 结束后过程数据的保留时间，0 表示 DefaultRetention。
	Retention time.Duration
	// Interval 是两次清理的间隔，0 表示 DefaultInterval。
	Interval time.Duration
	// PublishWorkDir 是发布 job 的工作目录：其中超过保留期的子目录是进程中断时留下的，一并删除。为空时不处理。
	PublishWorkDir string
}

// Deps 是 Collector 的依赖。
type Deps struct {
	DB   *store.DB
	CAS  Blobs
	Logs *logstore.Store
	Log  *slog.Logger
	// Purgers 是其他领域的清理（AI 的过程数据，PLAN #30.1），由组装层注入，每轮清理时调用。
	Purgers []Purger
}

// Purger 是一个领域的过程数据清理：删除结束时间早于 cutoff 的过程数据，返回各项删除的数量（写入审计日志）。
type Purger interface {
	Purge(ctx context.Context, cutoff time.Time) (map[string]int64, error)
}

// Collector 定期执行清理。同一时刻只应有一个 Collector（v0 的 Server 是单实例）。
type Collector struct {
	cfg  Config
	db   *store.DB
	cas  Blobs
	logs *logstore.Store
	log  *slog.Logger
	// purgers 是其他领域的清理。
	purgers []Purger
}

// New 创建 Collector。
func New(cfg Config, deps Deps) *Collector {
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Collector{cfg: cfg, db: deps.DB, cas: deps.CAS, logs: deps.Logs, log: deps.Log, purgers: deps.Purgers}
}

// Result 是一次清理的结果。
type Result struct {
	// Runs 是清理了过程数据的 run 数，Logs 是删除的日志数。
	Runs int `json:"runs"`
	Logs int `json:"logs"`
	// CacheEntries 是删除的过期缓存条目数。
	CacheEntries int64 `json:"cache_entries"`
	// Blobs、BlobBytes 是删除的 CAS 对象数和字节数（包括上传中断留下的临时文件）。
	Blobs     int   `json:"blobs"`
	BlobBytes int64 `json:"blob_bytes"`
	// Events、Outbox、EnrollTokens 是删除的 webhook 事件、outbox 记录和注册令牌数。
	Events       int64 `json:"events"`
	Outbox       int64 `json:"outbox"`
	EnrollTokens int64 `json:"enroll_tokens"`
	// WorkDirs 是删除的发布 job 残留工作目录数。
	WorkDirs int `json:"work_dirs"`
	// Domains 是其他领域（Purgers）各项删除的数量。
	Domains map[string]int64 `json:"domains,omitempty"`
}

// empty 表示这次清理什么也没有删除。
func (r *Result) empty() bool {
	if r.Runs != 0 || r.Logs != 0 || r.CacheEntries != 0 || r.Blobs != 0 || r.BlobBytes != 0 || r.Events != 0 || r.Outbox != 0 ||
		r.EnrollTokens != 0 || r.WorkDirs != 0 {
		return false
	}
	for _, n := range r.Domains {
		if n != 0 {
			return false
		}
	}
	return true
}

// Run 每隔 Interval 清理一次，直到 ctx 结束。
func (c *Collector) Run(ctx context.Context) {
	wait := startDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = c.cfg.Interval
		c.runOnce(ctx)
	}
}

// runOnce 清理一次，记录日志；删除了内容时写审计日志（PLAN #10.4）。
func (c *Collector) runOnce(ctx context.Context) {
	res, err := c.Collect(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.log.Error("清理过程数据", "err", err)
	}
	if res.empty() {
		return
	}
	c.log.Info("清理过程数据", "retention", c.cfg.Retention, "runs", res.Runs, "logs", res.Logs, "cache_entries", res.CacheEntries,
		"blobs", res.Blobs, "blob_mib", res.BlobBytes>>20, "events", res.Events, "outbox", res.Outbox,
		"enroll_tokens", res.EnrollTokens, "work_dirs", res.WorkDirs, "domains", res.Domains)
	if err := store.Audit(context.WithoutCancel(ctx), c.db, auditActor, "retention.collect", res); err != nil {
		c.log.Warn("记录清理结果", "err", err)
	}
}

// Collect 执行一次清理，返回删除的内容。各步骤互不阻塞：一步出错时其余步骤照常执行，返回已经完成的部分和全部错误。
// 产物的清除放在 run 的清理和缓存过期之后，只是为了这一轮就能回收它们释放的对象；前面的步骤失败时，
// 没有清理的 run 和没有过期的缓存条目仍然是标记的根，它们引用的对象不会被删除。
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	var res Result
	cutoff := time.Now().Add(-c.cfg.Retention)
	var errs []error
	if err := c.purgeRuns(ctx, cutoff, &res); err != nil {
		errs = append(errs, fmt.Errorf("清理 run 的日志：%w", err))
	}
	if err := c.expireCache(ctx, cutoff, &res); err != nil {
		errs = append(errs, fmt.Errorf("清理过期的缓存条目：%w", err))
	}
	if err := c.sweepBlobs(ctx, cutoff, &res); err != nil {
		errs = append(errs, fmt.Errorf("清理产物：%w", err))
	}
	if err := c.deleteRows(ctx, cutoff, &res); err != nil {
		errs = append(errs, err)
	}
	if err := c.sweepWorkDirs(cutoff, &res); err != nil {
		errs = append(errs, fmt.Errorf("清理发布工作目录：%w", err))
	}
	if err := c.runPurgers(ctx, cutoff, &res); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

// runPurgers 执行其他领域的清理，各项数量累加到 res.Domains；一个出错时其余照常执行。
func (c *Collector) runPurgers(ctx context.Context, cutoff time.Time, res *Result) error {
	var errs []error
	for _, p := range c.purgers {
		n, err := p.Purge(ctx, cutoff)
		for k, v := range n {
			if res.Domains == nil {
				res.Domains = map[string]int64{}
			}
			res.Domains[k] += v
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// unpurgedRuns 是 SQL 条件：已结束、最近一次运行还没有清理的 run（与索引 runs_unpurged 的条件一致）。
const unpurgedRuns = `finished_at IS NOT NULL AND (purged_at IS NULL OR purged_at < finished_at)`

// purgeRuns 清理结束时间早于 cutoff 的 run：删除其中所有 job 的日志（各次 attempt 和发布 job），记录 purged_at。
// 按 (finished_at, id) 依次向后读取：清理失败的 run 跳过（下一轮再试），不会挡住其后的 run；
// 失败达到 maxRunErrors 个时停止这一轮（多半是存储或数据库本身有问题）。
func (c *Collector) purgeRuns(ctx context.Context, cutoff time.Time, res *Result) error {
	type pending struct {
		ID         int64
		FinishedAt time.Time
	}
	var (
		errs      []error
		afterTime time.Time // 零值早于任何结束时间
		afterID   int64
	)
	for {
		rows, err := c.db.Query(ctx, `SELECT id, finished_at FROM runs WHERE `+unpurgedRuns+` AND finished_at < $1
			AND (finished_at, id) > ($2, $3) ORDER BY finished_at, id LIMIT $4`, cutoff, afterTime, afterID, runBatch)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
		if err != nil || len(batch) == 0 {
			return errors.Join(append(errs, err)...)
		}
		last := batch[len(batch)-1]
		afterTime, afterID = last.FinishedAt, last.ID
		for _, r := range batch {
			var purged bool
			var n int
			err := c.db.Tx(ctx, func(tx pgx.Tx) error {
				var err error
				purged, n, err = c.purgeRun(ctx, tx, r.ID, cutoff)
				return err
			})
			// 日志在事务中删除，事务失败时删除的那部分也不会恢复，照样计入
			res.Logs += n
			if err != nil {
				if ctx.Err() != nil {
					return errors.Join(append(errs, ctx.Err())...)
				}
				if errs = append(errs, fmt.Errorf("run %d：%w", r.ID, err)); len(errs) >= maxRunErrors {
					return errors.Join(errs...)
				}
				continue
			}
			if purged {
				res.Runs++
			}
		}
	}
}

// purgeRun 锁住 run、确认仍然需要清理后删除日志并记录 purged_at，返回是否清理了这个 run 和删除的日志数。
// 锁住 run 与重跑互斥：重跑在同一行锁下创建新的 job，不会出现日志被删、新的 job 却被标为已清理的情况。
func (c *Collector) purgeRun(ctx context.Context, tx pgx.Tx, id int64, cutoff time.Time) (bool, int, error) {
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(`+unpurgedRuns+` AND finished_at < $2, false) FROM runs WHERE id=$1 FOR UPDATE`, id, cutoff).Scan(&eligible); err != nil {
		return false, 0, err
	}
	if !eligible {
		// 最后一次运行可能已经清理过（purged_at 不为空），但锁住 run 前有重跑开始；
		// 仍然记录一次 purged_at 会把新的 job 标为已清理，所以什么也不做
		return false, 0, nil
	}
	// 日志的编号：attempt 的 ID，发布 job 没有 attempt，用 logstore.PublishLogID
	rows, err := tx.Query(ctx, `SELECT a.id, false FROM attempts a JOIN jobs j ON j.id=a.job_id WHERE j.run_id=$1
		UNION ALL SELECT j.id, true FROM jobs j WHERE j.run_id=$1 AND j.kind=$2`, id, workflow.JobKindPublish)
	if err != nil {
		return false, 0, err
	}
	logIDs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (int64, error) {
		var id int64
		var publish bool
		if err := row.Scan(&id, &publish); err != nil {
			return 0, err
		}
		if publish {
			return logstore.PublishLogID(id), nil
		}
		return id, nil
	})
	if err != nil {
		return false, 0, err
	}
	n := 0
	for _, lid := range logIDs {
		if err := c.logs.Delete(lid); err != nil {
			return false, n, err
		}
		n++
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET purged_at=now() WHERE id=$1`, id)
	return err == nil, n, err
}

// expireCache 删除最后一次创建或命中早于 cutoff 的缓存条目（之后不再命中，引用的产物随之可以清理）。
// 命中缓存时在同一行上更新 last_hit_at，与这里的删除互斥。
func (c *Collector) expireCache(ctx context.Context, cutoff time.Time, res *Result) error {
	tag, err := c.db.Exec(ctx, `DELETE FROM action_cache WHERE coalesce(last_hit_at, created_at) < $1`, cutoff)
	if err != nil {
		return err
	}
	res.CacheEntries = tag.RowsAffected()
	return nil
}

// deleteRows 删除超过保留期的 webhook 事件（已处理的）、outbox 记录（已投递或放弃的）和注册令牌（已过期的）。
func (c *Collector) deleteRows(ctx context.Context, cutoff time.Time, res *Result) error {
	steps := []struct {
		name string
		sql  string
		args []any // $1 总是 cutoff，其后是这里的参数
		n    *int64
	}{
		{"webhook 事件", `DELETE FROM events WHERE processed_at < $1`, nil, &res.Events},
		{"outbox 记录", `DELETE FROM outbox WHERE updated_at < $1 AND state = ANY($2)`, []any{[]string{store.OutboxDone, store.OutboxDead}}, &res.Outbox},
		{"注册令牌", `DELETE FROM enrollment_tokens WHERE expires_at < $1`, nil, &res.EnrollTokens},
	}
	var errs []error
	for _, st := range steps {
		tag, err := c.db.Exec(ctx, st.sql, append([]any{cutoff}, st.args...)...)
		if err != nil {
			errs = append(errs, fmt.Errorf("清理%s：%w", st.name, err))
			continue
		}
		*st.n = tag.RowsAffected()
	}
	return errors.Join(errs...)
}

// sweepWorkDirs 删除发布工作目录中超过保留期的子目录（发布 job 结束时会删除自己的目录，残留的是进程中断留下的）。
func (c *Collector) sweepWorkDirs(cutoff time.Time, res *Result) error {
	if c.cfg.PublishWorkDir == "" {
		return nil
	}
	entries, err := os.ReadDir(c.cfg.PublishWorkDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.cfg.PublishWorkDir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		res.WorkDirs++
	}
	return errors.Join(errs...)
}

// Package publish 执行发布 job（PLAN #13.4）。实现 GitHub Release（release_github.go）、
// HTTP 上传（release_upload.go）、容器镜像（release_images.go）和 npm（release_npm.go，PLAN #27.3）。流程在 job.go，文件类目标的抽象和记录在 targets.go，
// asset 的打包在 assets.go：打包在 Server 上完成；阶段 B 直接执行而不经过 outbox，失败后通过重跑该 job 重试。
// 打包、校验和、changelog 等纯函数在 internal/release。
package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
	gh "github.com/hewenyu/kilnflow.dev/internal/github"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cicheck"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/engine"
	"github.com/hewenyu/kilnflow.dev/internal/server/logstore"
	"github.com/hewenyu/kilnflow.dev/internal/server/notify"
	"github.com/hewenyu/kilnflow.dev/internal/server/outbox"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
)

const (
	// publishClaimTTL 是"正在由本进程执行"标记的有效期（借用 jobs.lease_expires_at）。发布在同一个 goroutine 中
	// 逐个执行，标记还在、job 仍是 running 而 worker 已经回来领取，说明结束 job 失败了（例如数据库暂时不可用）：
	// 过期后重新领取，再执行一次（每一步都是幂等的）并结束它，而不是一直停在 running 直到 Server 重启。
	publishClaimTTL = time.Hour
	// publishPollInterval 是没有被唤醒时检查待发布 job 的间隔。
	publishPollInterval = 5 * time.Second
)

var (
	// errPublishCancelled 表示发布在开始上传或提交前被取消。
	errPublishCancelled = errors.New("已取消")
	// errNoGitHubApp 表示 Server 没有配置 GitHub App（KILN_GITHUB_APP_ID）。
	errNoGitHubApp = errors.New("没有配置 GitHub App")
)

// Config 是发布 worker 需要的配置（由 server.Config 映射）。
type Config struct {
	// WorkDir 是打包 asset 的工作目录，每个发布 job 使用其中的一个子目录，结束后删除。
	WorkDir string
	// MasterKey 用于解密发布凭据（release.images、release.uploads 引用的 secret 和账号级 npm token）。
	MasterKey []byte
}

// Deps 是 Worker 的依赖。
type Deps struct {
	DB *store.DB
	// GitHub 没有配置 GitHub App 时为 nil，此时发布 job 失败。
	GitHub *gh.Client
	// CAS 用于读取上游 job 的输出。
	CAS cas.Store
	// Engine 在发布结束时结束 job，并读取上游输出的 manifest。
	Engine *engine.Engine
	Outbox *outbox.Writer
	// Logs 保存发布 job 的日志（编号见 logstore.PublishLogID）。
	Logs *logstore.Store
	Hub  *notify.Hub
	Log  *slog.Logger
}

// Worker 领取并执行发布 job。
type Worker struct {
	cfg    Config
	db     *store.DB
	gh     *gh.Client
	cas    cas.Store
	engine *engine.Engine
	outbox *outbox.Writer
	logs   *logstore.Store
	hub    *notify.Hub
	log    *slog.Logger
	// uploadOpts 调整 HTTP 上传的客户端（测试中调小重试的退避）。
	uploadOpts []upload.Option
	// npmOpts 调整 npm registry 的客户端（测试中调小重试的退避）。
	npmOpts []npm.Option
}

// New 返回发布 worker。
func New(cfg Config, d Deps) *Worker {
	return &Worker{
		cfg: cfg, db: d.DB, gh: d.GitHub, cas: d.CAS, engine: d.Engine, outbox: d.Outbox,
		logs: d.Logs, hub: d.Hub, log: d.Log,
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Run 循环领取并执行发布 job，直到 ctx 结束（被 notify.Hub.Publish 唤醒，或每隔 publishPollInterval 检查一次）。
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(publishPollInterval)
	defer t.Stop()
	for {
		for w.publishOnce(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.hub.Publish():
		}
	}
}

// publishOnce 领取一个待发布（或中断后需要继续）的发布 job 并执行，返回是否做了事。
func (w *Worker) publishOnce(ctx context.Context) bool {
	j, handled, err := w.claimPublishJob(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("领取发布 job", "err", err)
		}
		return false
	}
	if handled {
		w.hub.All()
	}
	if j == nil {
		return handled
	}
	plog := &publishLog{w: w, id: logstore.PublishLogID(j.ID)}
	plog.printf("开始发布（job #%d）", j.ID)
	perr := w.publishJob(ctx, j, plog)
	if ctx.Err() != nil {
		return false
	}
	res := engine.AttemptResult{Outcome: store.OutcomeSucceeded, ErrClass: "publish"}
	switch {
	case errors.Is(perr, errPublishCancelled):
		res.Outcome = store.OutcomeCancelled
		plog.printf("已取消")
	case perr != nil:
		res.Outcome, res.Message = store.OutcomeFailed, "发布失败："+perr.Error()
		plog.printf("发布失败：%v", perr)
	default:
		plog.printf("发布完成")
	}
	if err := w.finishPublishJob(ctx, j.ID, res); err != nil {
		w.log.Error("结束发布 job", "job", j.ID, "err", err)
	}
	w.hub.All()
	return true
}

// claimablePublish 是可以领取的发布 job：pending 的，以及 running 但没有执行标记（或标记已过期）的。
const claimablePublish = `kind='publish' AND (state='pending' OR (state='running' AND (lease_expires_at IS NULL OR lease_expires_at < now())))`

// claimPublishJob 领取一个发布 job：pending 的置为 running，并标记为正在由本进程执行；
// running 的（中断后没有标记，或者标记已过期，见 publishClaimTTL）继续执行。
// 返回 nil 表示没有需要执行的 job；handled 表示改变了某个 job 的状态（包括直接取消了已请求取消的 job）。
func (w *Worker) claimPublishJob(ctx context.Context) (j *cistore.Job, handled bool, err error) {
	err = w.db.Tx(ctx, func(tx pgx.Tx) error {
		j, handled = nil, false
		// 先不加锁选出候选，按加锁顺序锁住它的 run（取消时要推进 run）后再锁 job 并复查条件（cistore.LockRuns）。
		// 发布 worker 每个进程只有一个，不需要 SKIP LOCKED 分配候选。
		ids, err := store.QueryIDs(ctx, tx, `SELECT id FROM jobs WHERE `+claimablePublish+` ORDER BY state DESC, `+cistore.QueueOrderSQL+` LIMIT 1`)
		if err != nil || len(ids) == 0 {
			return err
		}
		runs, err := cistore.LockRunsOfJob(ctx, tx, ids[0])
		if err != nil {
			return err
		}
		jobs, err := cistore.QueryJobs(ctx, tx, claimablePublish+` AND id=$1 FOR UPDATE`, ids[0])
		if err != nil || len(jobs) == 0 {
			return err
		}
		cand := jobs[0]
		handled = true
		if cand.State == store.JobPending {
			if cand.CancelRequested {
				return w.engine.FinishAttempt(ctx, tx, cand, engine.AttemptResult{Outcome: store.OutcomeCancelled}, runs)
			}
			if _, err := tx.Exec(ctx, `UPDATE jobs SET state='running', started_at=now(), reason='正在发布', attempt_count=attempt_count+1 WHERE id=$1`, cand.ID); err != nil {
				return err
			}
			cand.State = store.JobRunning
			if err := cicheck.EnqueueJob(ctx, w.outbox, tx, cand); err != nil {
				return err
			}
		}
		// 用租约字段标记"正在由本进程执行"；进程重启后由 ResetClaims 清掉，让中断的发布继续
		if _, err := tx.Exec(ctx, `UPDATE jobs SET lease_expires_at=now() + make_interval(secs => $2) WHERE id=$1`, cand.ID, publishClaimTTL.Seconds()); err != nil {
			return err
		}
		j = cand
		return nil
	})
	return j, handled, err
}

// finishPublishJob 记录发布结果并结束 job。
func (w *Worker) finishPublishJob(ctx context.Context, jobID int64, res engine.AttemptResult) error {
	return w.db.Tx(ctx, func(tx pgx.Tx) error {
		runs, err := cistore.LockRunsOfJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		j, err := cistore.GetJob(ctx, tx, jobID, true)
		if err != nil || j.State != store.JobRunning {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE releases SET state=$2, error=nullif($3,''), updated_at=now() WHERE job_id=$1`,
			j.ID, res.Outcome.ReleaseState(), res.Message); err != nil {
			return err
		}
		return w.engine.FinishAttempt(ctx, tx, j, res, runs)
	})
}

// ResetClaims 在启动时（启动发布循环之前）清掉上一个进程留下的执行标记，让中断的发布继续执行。
func (w *Worker) ResetClaims(ctx context.Context) error {
	_, err := w.db.Exec(ctx, `UPDATE jobs SET lease_expires_at=NULL WHERE kind='publish' AND state='running'`)
	return err
}

// checkCancel 在发布的关键步骤之前检查是否已请求取消。已经进入阶段 B（commit_started_at 非空）的发布不可取消：
// 重启续做时要完成剩余目标，不因为之前记录的取消请求停在中途（PLAN #13.4）。
func (w *Worker) checkCancel(ctx context.Context, jobID int64) error {
	var c bool
	if err := w.db.QueryRow(ctx, `SELECT j.cancel_requested AND COALESCE(r.commit_started_at IS NULL, true)
		FROM jobs j LEFT JOIN releases r ON r.job_id = j.id
		WHERE j.id = $1`, jobID).Scan(&c); err != nil {
		return err
	}
	if c {
		return errPublishCancelled
	}
	return nil
}

// enterCommitStage 原子地进入阶段 B：在同一个事务中按 jobs -> releases 的锁序锁住两行，只有尚未进入过阶段 B 时才检查
// cancel_requested（阶段 B 不可取消，PLAN #13.4）。commit_started_at 单调：重跑、重启续做只更新 stage，不改写它。
func (w *Worker) enterCommitStage(ctx context.Context, jobID int64) error {
	return w.db.Tx(ctx, func(tx pgx.Tx) error {
		var cancelRequested bool
		if err := tx.QueryRow(ctx, `SELECT cancel_requested FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&cancelRequested); err != nil {
			return err
		}
		var committed bool
		if err := tx.QueryRow(ctx, `SELECT commit_started_at IS NOT NULL FROM releases WHERE job_id=$1 FOR UPDATE`, jobID).Scan(&committed); err != nil {
			return err
		}
		if !committed && cancelRequested {
			return errPublishCancelled
		}
		if committed {
			_, err := tx.Exec(ctx, `UPDATE releases SET stage=$2, updated_at=now() WHERE job_id=$1`, jobID, stageCommit)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE releases SET commit_started_at=now(), stage=$2, progress='{}', updated_at=now() WHERE job_id=$1`,
			jobID, stageCommit)
		return err
	})
}

// publishLog 把发布过程写入 job 的日志（只在发布 worker 中使用，不需要加锁）。
type publishLog struct {
	w   *Worker
	id  int64
	off int64
}

// printf 追加一行日志。写日志失败不影响发布，只记录告警。
func (l *publishLog) printf(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
	if l.off == 0 {
		size, err := l.w.logs.Size(l.id)
		if err != nil {
			l.w.log.Warn("读取发布日志", "log", l.id, "err", err)
		}
		l.off = size
	}
	n, err := l.w.logs.Append(l.id, l.off, []byte(line))
	if err != nil {
		l.w.log.Warn("写发布日志", "log", l.id, "err", err)
		return
	}
	l.off = n
}

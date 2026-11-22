package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// sourceDateEpoch 是固定的 SOURCE_DATE_EPOCH（1980-01-01，PLAN #17.1）。
const sourceDateEpoch = "315532800"

const (
	// defaultJobTimeout 是 JobSpec 没有给出超时时的默认值。
	defaultJobTimeout = time.Hour
	// maxHeartbeatInterval 是心跳间隔的上限（心跳同时用于接收取消指令）。
	maxHeartbeatInterval = 5 * time.Second
	// heartbeatTimeout 是单次心跳 RPC 的超时。
	heartbeatTimeout = 20 * time.Second
	// minBeatTimeout 是心跳快到断连上限时单次心跳的最短超时。
	minBeatTimeout = time.Second
	// finalBeatTimeout 是提交结果前补发的那次心跳的超时（只是展示用，不能拖慢提交结果）。
	finalBeatTimeout = 5 * time.Second
	// heartbeatWarnAfter 是连续失败多少次后记录警告。
	heartbeatWarnAfter = 3
	// runnerMaxDisconnected 是连不上 Server 时 job 继续执行的上限，超过后 Runner 自行停止 job（PLAN #6.3）：
	// Server 那边租约早已过期，job 可能已经交给别的 Runner，旧的执行继续下去只会重复占用资源。
	runnerMaxDisconnected = 10 * time.Minute
	// defaultLeaseTTL 是 JobSpec 没有给出租约时长时的默认值（与 Server 的默认值一致）。
	defaultLeaseTTL = time.Minute
	// completeTimeout 是单次提交结果 RPC 的超时，completeTotalTimeout 是提交结果的总时长上限。
	completeTimeout      = time.Minute
	completeTotalTimeout = 30 * time.Minute
	// rpcAttempts 是普通 RPC 的重试次数。
	rpcAttempts = 5
)

type job struct {
	r    *Runner
	spec *runnerv1.JobSpec
	log  *runnerkit.LogShipper
	// prog 是当前阶段与进度，随心跳上报。
	prog *progress

	step      atomic.Int32
	cancelled atomic.Bool
	stale     atomic.Bool
	// disconnected 表示连续连不上 Server 超过上限，Runner 已经自行停止 job。
	disconnected atomic.Bool

	// hbMu 保护 finishing：心跳循环在持有它时处理响应（记录取消、写日志）。
	hbMu sync.Mutex
	// finishing 为 true 后 execute 已经返回、结果已经确定：心跳只续约（持续到提交结果之后），
	// 不再记录取消、不再写日志，stale / cancelled 不再影响结果。
	finishing bool
}

func (r *Runner) newJob(spec *runnerv1.JobSpec) *job {
	j := &job{r: r, spec: spec, prog: newProgress()}
	j.step.Store(-1)
	return j
}

// jobResult 是一次执行的结果。
type jobResult struct {
	outcome  runnerv1.Outcome
	class    string
	exitCode int
	msg      string
	manifest string
}

func (j *job) run(parent context.Context) {
	sp := j.spec
	logger := j.r.log.With("job", sp.JobId, "attempt", sp.Ref.AttemptId, "name", sp.DisplayName)
	logger.Info("开始执行", "run", sp.RunId, "repo", sp.GetSource().GetRepository())
	start := time.Now()
	j.log = runnerkit.NewLogShipper(j.appendLog)

	timeout := time.Duration(sp.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	tctx, tcancel := context.WithTimeoutCause(ctx, timeout, errTimeout)
	defer tcancel()

	// 心跳在 job 被取消后仍要继续（清理、发送剩余日志、提交结果期间都要续约），
	// 所以不继承 ctx 的取消；提交结果之后才停止
	hbCtx, stopHeartbeat := context.WithCancel(context.WithoutCancel(ctx))
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		j.heartbeat(hbCtx, cancel)
	}()
	defer func() {
		stopHeartbeat()
		<-hbDone
	}()

	res := j.execute(tctx)
	// 进入收尾：心跳之后只续约，stale / cancelled 不会再变化，也不会再写日志
	j.hbMu.Lock()
	j.finishing = true
	j.hbMu.Unlock()
	switch {
	case j.stale.Load():
		res = nil
	case j.cancelled.Load():
		res = &jobResult{outcome: runnerv1.Outcome_OUTCOME_CANCELLED, msg: "已取消"}
	case j.disconnected.Load() && res.outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED:
		msg := fmt.Sprintf("超过 %s 连不上 Kiln Server，Runner 已停止执行", j.maxDisconnected())
		res = &jobResult{outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: classDisconnected, msg: msg}
	case errors.Is(context.Cause(tctx), errTimeout) && res.outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED:
		res = &jobResult{outcome: runnerv1.Outcome_OUTCOME_TIMED_OUT, class: classTimeout, msg: fmt.Sprintf("超过 %s", timeout)}
	case parent.Err() != nil && res.outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED:
		res = &jobResult{outcome: runnerv1.Outcome_OUTCOME_INFRA_ERROR, class: classRunnerShutdown, msg: "Runner 正在退出"}
	}
	if res != nil {
		label := outcomeLabel(res.outcome)
		if res.msg != "" && res.msg != label && res.outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED {
			j.log.Printf("结果：%s（%s）", label, res.msg)
		} else {
			j.log.Printf("结果：%s，用时 %s", label, time.Since(start).Round(time.Second))
		}
	}
	j.log.Close()
	if res != nil {
		j.prog.set(runnerv1.Phase_PHASE_COMPLETE, "")
		j.beatOnce(parent)
	}
	if res == nil {
		logger.Warn("attempt 已失效（epoch 过期），放弃结果")
		return
	}
	if err := j.complete(parent, res); err != nil {
		logger.Error("提交结果失败", "err", err)
		return
	}
	logger.Info("执行结束", "outcome", res.outcome.String(), "duration", time.Since(start).Round(time.Second))
}

// complete 向 Server 提交结果。Runner 退出时也要提交，所以不继承 parent 的取消，但总时长有上限。
// 结果说明会显示在 Dashboard 和 GitHub Check Run 中，与日志一样先脱敏（错误信息中可能带有命令输出）。
func (j *job) complete(parent context.Context, res *jobResult) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(parent), completeTotalTimeout)
	defer cancel()
	req := &runnerv1.CompleteRequest{
		Ref:                  j.spec.Ref,
		Outcome:              res.outcome,
		ErrorClass:           res.class,
		ExitCode:             runnerkit.ClampInt32(res.exitCode),
		ResultManifestDigest: res.manifest,
		Message:              runnerkit.TruncateMsg(j.log.MaskString(res.msg)),
	}
	_, err := runnerkit.Retry(cctx, 0, func() (*runnerv1.CompleteResponse, error) {
		c, cancel := context.WithTimeout(cctx, completeTimeout)
		defer cancel()
		return runnerkit.Call(c, j.r.c, j.r.c.RPC().Complete, req)
	})
	return err
}

func outcomeLabel(o runnerv1.Outcome) string {
	switch o {
	case runnerv1.Outcome_OUTCOME_SUCCEEDED:
		return "成功"
	case runnerv1.Outcome_OUTCOME_FAILED:
		return "失败"
	case runnerv1.Outcome_OUTCOME_INFRA_ERROR:
		return "基础设施错误"
	case runnerv1.Outcome_OUTCOME_CANCELLED:
		return "已取消"
	case runnerv1.Outcome_OUTCOME_TIMED_OUT:
		return "超时"
	}
	return ""
}

// heartbeat 定期续约，直到 ctx 结束；收到取消指令时取消 job；epoch 过期时立即停止。
func (j *job) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	// 间隔取 maxHeartbeatInterval 和租约的 1/4 中较小的
	interval := time.Duration(j.spec.LeaseTtlSeconds) * time.Second / 4
	if interval <= 0 || interval > maxHeartbeatInterval {
		interval = maxHeartbeatInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	failures := 0
	// lastOK 是最后一次确认与 Server 连通的时间（time.Now 带单调时钟，不受系统时间调整影响）：领取到 job 时是连通的
	lastOK := time.Now()
	limit := j.maxDisconnected()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// 单次心跳最多等到断连上限（至少 minBeatTimeout）：Server 不响应时不会因为等待这一次心跳而超过上限
		bctx, bcancel := context.WithTimeout(ctx, max(min(heartbeatTimeout, limit-time.Since(lastOK)), minBeatTimeout))
		resp, err := j.beat(bctx)
		bcancel()
		if stop := j.handleBeat(ctx, resp, err, cancel); stop {
			return
		}
		if err != nil && ctx.Err() == nil {
			failures++
			if failures == heartbeatWarnAfter {
				j.r.log.Warn("心跳失败", "job", j.spec.JobId, "err", err)
			}
			if d := time.Since(lastOK); d > limit {
				j.disconnect(d, limit, err, cancel)
			}
			continue
		}
		failures = 0
		lastOK = time.Now()
	}
}

// maxDisconnected 返回连不上 Server 时 job 继续执行的上限：普通 job 是 runner_max_disconnected（PLAN #6.3），
// 有副作用的 job 是租约时长（#6.7：租约过期时旧的执行通常已经停止，减少副作用重复执行的机会）。
func (j *job) maxDisconnected() time.Duration {
	if !j.spec.Effects {
		return j.r.maxDisconnected
	}
	if ttl := time.Duration(j.spec.LeaseTtlSeconds) * time.Second; ttl > 0 {
		return ttl
	}
	return defaultLeaseTTL
}

// disconnect 在连续 d 连不上 Server（超过上限 limit）时停止 job。心跳继续：之后连上时照常续约、提交结果，
// 由 Server 按 epoch 判断是否接受。收尾阶段（结果已经确定）不再停止。
func (j *job) disconnect(d, limit time.Duration, err error, cancel context.CancelCauseFunc) {
	j.hbMu.Lock()
	defer j.hbMu.Unlock()
	if j.finishing || j.disconnected.Load() {
		return
	}
	j.disconnected.Store(true)
	j.r.log.Error("连不上 Server 超过上限，停止 job", "job", j.spec.JobId, "disconnected", d.Round(time.Second), "limit", limit, "err", err)
	j.log.Printf("已经 %s 连不上 Kiln Server（上限 %s），停止执行：%v", d.Round(time.Second), limit, err)
	cancel(errDisconnected)
}

// handleBeat 处理一次心跳的结果，返回 true 表示停止心跳（attempt 已失效，或者 ctx 已结束）。
// 收尾阶段（finishing）只续约：结果已经确定，不再记录取消，也不再写日志（日志可能已经关闭）。
func (j *job) handleBeat(ctx context.Context, resp *runnerv1.HeartbeatResponse, err error, cancel context.CancelCauseFunc) bool {
	j.hbMu.Lock()
	defer j.hbMu.Unlock()
	switch {
	case err != nil && (errors.Is(err, runnerkit.ErrStale) || errors.Is(err, runnerkit.ErrRevoked)):
		if !j.finishing {
			j.stale.Store(true)
			cancel(runnerkit.ErrStale)
		}
		return true
	case err != nil:
		return ctx.Err() != nil
	}
	if resp.CancelRequested && !j.finishing && !j.cancelled.Load() {
		j.cancelled.Store(true)
		j.log.Printf("收到取消指令")
		cancel(errCancelled)
	}
	return false
}

// beat 发送一次心跳：续约，并上报当前 step 和进度。
func (j *job) beat(ctx context.Context) (*runnerv1.HeartbeatResponse, error) {
	hctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	return runnerkit.Call(hctx, j.r.c, j.r.c.RPC().Heartbeat, &runnerv1.HeartbeatRequest{
		Ref: j.spec.Ref, CurrentStep: j.step.Load(), Progress: j.prog.snapshot(),
	})
}

// beatOnce 在提交结果前补发一次心跳，让 Dashboard 立即显示"正在提交结果"（不等下一次周期心跳）。
// 只是展示用，失败时忽略：续约由心跳循环负责，结果是否有效由 Complete 按 epoch 判断。
func (j *job) beatOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), finalBeatTimeout)
	defer cancel()
	if _, err := j.beat(ctx); err != nil {
		j.r.log.Debug("提交结果前的心跳失败", "job", j.spec.JobId, "err", err)
	}
}

func durIf(d time.Duration) string {
	if d < time.Second {
		return ""
	}
	return "，下载用时 " + d.Round(time.Second).String()
}

// appendLog 把日志追加到 Server（runnerkit.LogShipper 的 AppendFunc）。
func (j *job) appendLog(ctx context.Context, offset int64, data []byte) (int64, error) {
	resp, err := runnerkit.Call(ctx, j.r.c, j.r.c.RPC().AppendLog, &runnerv1.AppendLogRequest{Ref: j.spec.Ref, Offset: offset, Data: data})
	if err != nil {
		return 0, err
	}
	return resp.Committed, nil
}

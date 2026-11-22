package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

const (
	// defaultIdlePoll 是没有空闲槽位或处于 drain 状态时的检查间隔。
	defaultIdlePoll = 10 * time.Second
	// workRetention 是保留的失败现场的保存时间（PLAN #8.9）。
	workRetention = 12 * time.Hour
	// acquireTimeout 是单次 Acquire 的总超时：长轮询等待 + 余量。Server 长时间不回时释放这次请求、按退避重试，
	// 不会一直占着连接。
	acquireTimeout = runnerkit.AcquireWaitSeconds*time.Second + 35*time.Second
)

// Runner 是一个正在运行的 kiln-runner 进程。
type Runner struct {
	cfg   *Config
	state *State
	c     *runnerkit.Client
	log   *slog.Logger
	host  hostInfo

	mu      sync.Mutex
	running map[int64]*job // attempt_id → job
	usedCPU float64
	usedMem int64
	// done 在有 job 结束时收到信号。缓冲为 1、发送不阻塞：等待者还没取走时，
	// 之后的信号合并为一个（等待者只需要知道"至少有一个 job 结束了"）。
	done chan struct{}

	gitLocksMu sync.Mutex
	gitLocks   map[int64]*sync.Mutex

	// lastGC 是上一次按保留策略清理的时间（gc.go；只在 Run 的循环中读写）。
	lastGC time.Time
	// 以下只在 Run 的循环中读写（cache.go，PLAN #8.3）：
	// report 是还没有上报的缓存清单，lastReport 是上一次生成清单的时间；
	// purges 是 Server 下发、还没有执行的清理请求，purgeResults 是已经执行、还没有上报的结果，
	// purgeDone 是本进程执行过的请求（Server 确认前可能再次下发，不重复执行）。
	report       *runnerv1.CacheReport
	lastReport   time.Time
	purges       []*runnerv1.CachePurge
	purgeResults []*runnerv1.CachePurgeResult
	purgeDone    map[int64]bool

	// up 是自升级的状态（PLAN #27.7）。
	up *runnerkit.Upgrader

	// maxDisconnected 是普通 job 连不上 Server 时继续执行的上限（runnerMaxDisconnected，测试中缩短）。
	maxDisconnected time.Duration
	// idlePoll 是没有空闲槽位或处于 drain 状态时的检查间隔（为 0 时用 defaultIdlePoll，测试中缩短）。
	idlePoll time.Duration

	// exe 是 kiln-runner 自己的路径：step 中 git 的传输包装和 credential helper 都是它（gitchannel.go）。
	exe string
	// gitSocketParent 非空时代替系统临时目录，作为 git 凭据通道的 socket 目录的父目录（测试用）。
	gitSocketParent string

	// userHome 非空时代替当前用户的主目录（signing 安装 provisioning profile 的位置，测试用；
	// 临时 keychain 加入的搜索列表总是 Runner 用户真正的主目录中的，trustd 只读那里）。
	userHome string
}

// Enroll 用一次性 token 注册 Runner，保存身份信息，并把 Server 地址和名称写回 config.toml。
func Enroll(ctx context.Context, cfg *Config, server, token, name string) (*State, error) {
	host := detectHost(ctx, cfg)
	st, err := runnerkit.Enroll(ctx, cfg.home(), server, token, name, host.state(cfg, 0, 0))
	if err != nil {
		return nil, err
	}
	cfg.Server, cfg.Name = st.Server, st.Name
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	return st, nil
}

// New 创建 Runner（需要已经注册）。
func New(ctx context.Context, cfg *Config, log *slog.Logger) (*Runner, error) {
	r, err := newRunner(ctx, cfg, log, detectHost(ctx, cfg))
	if err != nil {
		return nil, err
	}
	for _, d := range []string{casDirName, rtDirName, gitDirName, workDirName, toolcacheDirName} {
		if err := os.MkdirAll(cfg.path(d), 0o755); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// newRunner 读取注册信息和私钥，创建 Runner。
func newRunner(_ context.Context, cfg *Config, log *slog.Logger, host hostInfo) (*Runner, error) {
	h := cfg.home()
	st, err := h.LoadState()
	if err != nil {
		return nil, err
	}
	key, err := h.Key(false)
	if err != nil {
		return nil, err
	}
	exe, err := selfPath()
	if err != nil {
		return nil, err
	}
	r := &Runner{
		exe:             exe,
		cfg:             cfg,
		state:           st,
		log:             log,
		c:               runnerkit.NewClient(cfg.ServerURL(st), st.RunnerID, key),
		host:            host,
		running:         map[int64]*job{},
		maxDisconnected: runnerMaxDisconnected,
		done:            make(chan struct{}, 1),
		up:              runnerkit.NewUpgrader(h, cfg.UpgradeEnabled(), log, runnerkit.UpgraderOptions{}),
	}
	return r, nil
}

// stateMsg 返回上报给 Server 的状态，包括正在执行的 job。
func (r *Runner) stateMsg() *runnerv1.RunnerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.host.state(r.cfg, r.usedCPU, r.usedMem)
	st.RunningJobs = runnerkit.ClampInt32(len(r.running))
	st.Upgrade = r.up.StatusMsg()
	for _, j := range r.running {
		st.Running = append(st.Running, j.spec.Ref)
	}
	return st
}

// Run 循环领取并执行任务，直到 ctx 结束或 drain 完成。
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info(Program.Name+" 已启动", "name", r.state.Name, "server", r.c.Server(), "platforms", r.host.platforms, "executors", r.host.executors,
		"cpu", r.host.totalCPU, "memory_gib", r.host.totalMem>>30, "version", buildinfo.Version())
	unlock, err := r.cfg.home().Lock()
	if err != nil {
		return err
	}
	defer unlock()
	r.up.Cleanup() // 持有数据目录锁之后才清理升级的残留
	if err := runnerkit.AuthenticateTrial(ctx, r.c, r.up, r.log.Warn); err != nil {
		return err
	}
	// 收到退出信号后：不再领取新任务，等当前任务结束（最长 timeout），再退出。
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	var wg sync.WaitGroup
	reqID := rand.Text()
	backoff := runnerkit.AcquireBackoffMin
	draining := false
	// upDrain 为真时新版本已经准备好：不再领取新任务，没有正在执行的 job 时切换（PLAN #27.7 第 2、3 步）
	upDrain := false
	for ctx.Err() == nil {
		if upDrain && r.idle() {
			if err := r.up.SwitchTo(); err != nil {
				r.log.Error("自升级：切换失败，继续运行当前版本", "err", err)
			}
			upDrain = false
		}
		r.maybeGC(ctx)
		if len(r.purges) > 0 {
			// 有待执行的清理请求：不再领取新任务，等正在执行的 job 结束后清理
			if !r.idle() {
				r.waitIdle(ctx, true)
				continue
			}
			r.runPurges(ctx)
		}
		fileDrain := r.cfg.home().Draining()
		slots := 0
		if !fileDrain && !upDrain {
			var full bool
			if slots, full = r.acquireSlots(); full {
				r.waitIdle(ctx, true)
				continue
			}
		}
		state := r.stateMsg()
		state.CacheReport, state.PurgeResults = r.report, r.purgeResults
		tctx, cancelTrial := r.up.TrialContext(ctx) // 试用的新版本：到期时取消进行中的 Acquire
		actx, cancelAcquire := context.WithTimeout(tctx, acquireTimeout)
		resp, err := runnerkit.Call(actx, r.c, r.c.RPC().Acquire, &runnerv1.AcquireRequest{
			State: state, MaxJobs: runnerkit.ClampInt32(slots), RequestId: reqID, WaitSeconds: runnerkit.AcquireWaitSeconds,
		})
		cancelAcquire()
		cancelTrial()
		if err != nil {
			if errors.Is(err, runnerkit.ErrRevoked) {
				cancelJobs()
				wg.Wait()
				return err
			}
			if ctx.Err() != nil {
				break
			}
			if !r.up.TrialRetry(err, time.Now()) {
				// 试用的新版本被 Server 拒绝或超过试用期限：退出，由启动器计一次启动（连续 3 次后回滚，PLAN #27.7 第 5 步）
				cancelJobs()
				wg.Wait()
				return fmt.Errorf("新版本没有通过确认：%w", err)
			}
			r.log.Warn("Acquire 失败，稍后重试", "err", err)
			runnerkit.SleepCtx(ctx, backoff)
			backoff = min(backoff*2, runnerkit.AcquireBackoffMax)
			continue // 保持同一个 request_id，Server 会返回同一批任务
		}
		backoff = runnerkit.AcquireBackoffMin
		reqID = rand.Text()
		// Server 已经收到清单和清理结果
		r.report, r.purgeResults = nil, nil
		// 试用的新版本完成了第一次 Acquire（这一轮没有领取任务）：确认后下一轮才开始接任务。确认写不进 state.json 时退出，
		// 由启动器计一次启动，不在没有确认的版本上执行任务（否则之后的网络错误会按试用期限中断正在执行的任务）
		if err := r.up.Confirm(); err != nil {
			cancelJobs()
			wg.Wait()
			return err
		}
		upDrain = r.up.Directives(ctx, resp.GetDirectives())
		r.queuePurges(resp.GetDirectives().GetCachePurges())
		if d := fileDrain || resp.GetDirectives().GetDrain(); d != draining {
			draining = d
			if draining {
				r.log.Info("drain：不再领取新任务（正在执行的任务继续）")
			} else {
				r.log.Info("恢复领取任务")
			}
		}
		if (draining || upDrain) && len(resp.Jobs) == 0 {
			r.waitIdle(ctx, false)
		}
		for _, spec := range resp.Jobs {
			r.start(jobCtx, &wg, spec)
		}
	}
	r.mu.Lock()
	n := len(r.running)
	r.mu.Unlock()
	if n > 0 {
		r.log.Info("等待正在执行的任务结束", "jobs", n)
	}
	wg.Wait()
	return nil
}

// acquireSlots 返回这一轮 Acquire 的 max_jobs。槽位用满时 full 为真（等有 job 结束再领取）；试用的新版本确认之前
// 不领取任务（第一次 Acquire 只上报版本，Server 立即返回，确认后下一轮才领取，PLAN #27.7 第 5 步）。
func (r *Runner) acquireSlots() (slots int, full bool) {
	slots = r.freeSlots()
	if slots <= 0 {
		return 0, true
	}
	if r.up.Trial() {
		return 0, false
	}
	return slots, false
}

// maybeGC 在没有正在执行的 job、并且距上一次清理超过 GCInterval 时按保留策略清理（gc.go；启动后第一轮即清理）。
// 清理在领取任务的循环中同步进行，期间不会开始新的 job。
// 清理时同时按容量回收（PLAN #8.3），并重新生成缓存清单；一直忙碌、清理不了时也每隔 GCInterval 更新一次清单。
func (r *Runner) maybeGC(ctx context.Context) {
	if time.Since(r.lastGC) < runnerkit.GCInterval || !r.idle() {
		if time.Since(r.lastReport) >= runnerkit.GCInterval {
			r.refreshReport(ctx)
		}
		return
	}
	r.lastGC = time.Now()
	GC(ctx, r.cfg, GCOptions{Docker: r.docker(), RunnerID: r.state.RunnerID, Evict: true}, r.log)
	r.refreshReport(ctx)
}

// idle 表示没有正在执行的 job。
func (r *Runner) idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running) == 0
}

// docker 返回 oci executor 可用时的 docker 命令，否则为空。
func (r *Runner) docker() string {
	if slices.Contains(r.host.executors, string(workflow.ExecutorOCI)) {
		return r.cfg.Docker
	}
	return ""
}

// refreshReport 重新生成缓存清单，在下一次 Acquire 时上报。
func (r *Runner) refreshReport(ctx context.Context) {
	r.lastReport = time.Now()
	rep, err := Inventory(ctx, r.cfg, r.docker())
	if err != nil {
		r.log.Warn("生成缓存清单", "err", err)
	}
	r.report = rep
}

// queuePurges 登记 Server 下发的清理请求（已经执行过或已经在队列中的忽略）。
func (r *Runner) queuePurges(list []*runnerv1.CachePurge) {
	for _, p := range list {
		if r.purgeDone[p.Id] || slices.ContainsFunc(r.purges, func(q *runnerv1.CachePurge) bool { return q.Id == p.Id }) {
			continue
		}
		r.log.Info("收到缓存清理请求，等正在执行的 job 结束后执行", "id", p.Id, "repo_id", p.RepoId, "kinds", p.Kinds, "domain", p.Domain)
		r.purges = append(r.purges, p)
	}
}

// runPurges 执行全部待执行的清理请求（没有正在执行的 job 时调用），结果在下一次 Acquire 时上报。
func (r *Runner) runPurges(ctx context.Context) {
	if r.purgeDone == nil {
		r.purgeDone = map[int64]bool{}
	}
	for _, p := range r.purges {
		rm := newRemover(r.cfg, r.docker(), r.state.RunnerID)
		rep := Purge(ctx, r.cfg, rm, p)
		if rm.used {
			r.recordImages([]string{rm.image})
		}
		r.log.Info("缓存清理请求已执行", "id", p.Id, "removed", rep.Removed, "removed_mib", rep.RemovedBytes>>20, "failures", len(rep.Failures))
		for _, f := range rep.Failures {
			r.log.Warn("清理失败", "kind", f.Kind, "path", f.Path, "err", f.Error)
		}
		saveGCReport(r.cfg, rep)
		r.purgeResults = append(r.purgeResults, &runnerv1.CachePurgeResult{
			Id: p.Id, Removed: int64(rep.Removed), RemovedBytes: rep.RemovedBytes, Failures: rep.failures(r.cfg.Dir),
		})
		r.purgeDone[p.Id] = true
	}
	r.purges = nil
	r.refreshReport(ctx)
}

// freeSlots 返回还能同时执行的 job 数。
func (r *Runner) freeSlots() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.MaxJobs - len(r.running)
}

// waitIdle 等待 idlePoll；jobDone 为 true 时有 job 结束也会提前返回。
func (r *Runner) waitIdle(ctx context.Context, jobDone bool) {
	done := r.done
	if !jobDone {
		done = nil // nil channel 永远不会就绪
	}
	poll := r.idlePoll
	if poll <= 0 {
		poll = defaultIdlePoll
	}
	select {
	case <-ctx.Done():
	case <-done:
	case <-time.After(poll):
	}
}

// start 登记并在新的 goroutine 中执行一个 job；同一个 attempt 已经在执行时忽略。
func (r *Runner) start(ctx context.Context, wg *sync.WaitGroup, spec *runnerv1.JobSpec) {
	j := r.newJob(spec)
	id := spec.Ref.AttemptId
	r.mu.Lock()
	if _, dup := r.running[id]; dup {
		r.mu.Unlock()
		return
	}
	r.running[id] = j
	r.usedCPU += spec.GetResources().GetCpu()
	r.usedMem += spec.GetResources().GetMemoryBytes()
	r.mu.Unlock()
	wg.Go(func() {
		j.run(ctx)
		r.mu.Lock()
		delete(r.running, id)
		r.usedCPU -= spec.GetResources().GetCpu()
		r.usedMem -= spec.GetResources().GetMemoryBytes()
		r.mu.Unlock()
		select {
		case r.done <- struct{}{}:
		default:
		}
	})
}

// repoLock 保证同一个仓库的 bare 镜像同一时刻只有一个 git 操作。
func (r *Runner) repoLock(repoID int64) *sync.Mutex {
	r.gitLocksMu.Lock()
	defer r.gitLocksMu.Unlock()
	if r.gitLocks == nil {
		r.gitLocks = map[int64]*sync.Mutex{}
	}
	mu, ok := r.gitLocks[repoID]
	if !ok {
		mu = &sync.Mutex{}
		r.gitLocks[repoID] = mu
	}
	return mu
}

func (r *Runner) workDir(attemptID int64) string {
	return r.cfg.path(workDirName, strconv.FormatInt(attemptID, 10))
}

// SetDrain 设置或清除本地 drain 标记（kiln-runner drain / undrain）。
func SetDrain(cfg *Config, on bool) error { return cfg.home().SetDrain(on) }

// selfPath 返回 kiln-runner 可执行文件的真实路径（解析符号链接）。
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("找不到 kiln-runner 自己的路径：%w", err)
	}
	return filepath.EvalSymlinks(p)
}

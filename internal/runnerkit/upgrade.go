package runnerkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/npm"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

// Runner 自升级（PLAN #27.7、#27.12）：Runner 自己查询 npm 上本程序、本平台的包的 latest 并上报；收到管理员请求的
// 升级指令（版本和 generation）后在后台从 registry 读这个版本的 dist、下载、核对 integrity、只取出程序这一个文件、
// 运行 version 核对；然后排空（不领新任务，等正在执行的 job 结束），写 trial 后 exec 启动器；新版本完成第一次 Acquire
// 后确认。

// 升级状态（UpgradeStatus.state，与 Server 的 upgradectl 相同）。
const (
	UpgradeIdle        = "idle"
	UpgradeDownloading = "downloading"
	UpgradeReady       = "ready"
	UpgradeDraining    = "draining"
	UpgradeSwitching   = "switching"
	UpgradeConfirmed   = "confirmed"
	UpgradeFailed      = "failed"
	UpgradeRolledBack  = "rolled_back"
	UpgradeManual      = "manual"
)

const (
	// maxPackageSize 是 tarball 的大小上限（多了就失败）。
	maxPackageSize = 512 << 20
	// maxProgramSize 是包内程序的大小上限。
	maxProgramSize = 512 << 20
	// registryTimeout 是一次查询 registry 的超时。
	registryTimeout = 2 * time.Minute
	// checkInterval / checkRetryInterval 是查询 latest 成功、失败后再次查询的间隔（PLAN #27.12）。
	checkInterval      = time.Hour
	checkRetryInterval = 10 * time.Minute
	// downloadTimeout 是一次下载的超时。
	downloadTimeout = 10 * time.Minute
	// versionCheckTimeout 是运行新程序 version 的超时。
	versionCheckTimeout = 30 * time.Second
)

// errIntegrity 表示下载的内容与 registry 的记录不符（integrity、超过大小上限）或包的内容不对：同一 generation 内不再重试。
var errIntegrity = errors.New("下载的内容与 registry 的记录不符")

// Upgrader 是 Runner 进程中的自升级状态（nil 表示不处理升级，测试中直接构造的 Runner）。方法只在 Run 的循环中调用；后台下载通过 mu 交回结果。
type Upgrader struct {
	l        layout
	log      *slog.Logger
	version  string // 本进程的版本（npm 版本号，开发构建为空）
	launched bool   // 经过启动器运行（KILN_LAUNCHER=1 且布局存在）
	disabled bool   // config.toml 中 upgrade = false
	pkg      string // 本程序、本平台的 npm 包名
	client   *http.Client
	// npmOpts 是 registry 客户端的选项（测试中调小重试）。
	npmOpts []npm.Option
	// exec 切换时替换进程（测试中替换），为 nil 时用 syscall.Exec。
	exec func(argv0 string, argv, env []string) error

	mu     sync.Mutex
	status runnerv1.UpgradeStatus
	boot   *BootState // state.json（没有经过启动器时为 nil）
	// baseCurrent / basePrevious 是本进程启动时（以及本进程确认新版本之后）state.json 的 current、previous：切换前与
	// 磁盘上的比较，不同说明期间执行了 service install。u.boot 会在记录失败等更新时刷新成磁盘上最新的，不能用它比较。
	baseCurrent, basePrevious string
	busy                      bool   // 正在下载
	ready                     string // 已经下载、核对好的版本
	// target / targetGen 是最后一次收到的（有效的）指令的版本和 generation，撤回时清空。排空和切换只认它：后台还在下载的
	// 旧目标完成时不能替换已经选定的排空目标
	target    string
	targetGen int64
	// registry 是最后一次收到的 registry（已经把空值换成官方 registry），下载时用它。
	registry string
	// failedGen 是本进程中失败过的最大 generation：一轮（一个升级请求）中任何一步失败都结束这个请求（PLAN #27.6），
	// 不重试同一 generation，一直上报这一轮的 failed 直到 Server 结束请求、不再下发；管理员重新请求（更大的
	// generation）时立即执行。
	failedGen int64
	// 查询 latest（PLAN #27.12）：checking 为真时后台正在查询；lastCheck 是最后一次开始查询的时间，checkReg 是那次用的
	// registry；查询结束时 status 的 latest、checked_at、check_error、check_seq 更新。
	checking  bool
	lastCheck time.Time
	checkReg  string
	// confirmPending 为真时本进程是正在试的新版本，第一次 Acquire 成功后确认。
	confirmPending bool
	// started 是本进程开始的时间（试用期限从这里算）。
	started time.Time
}

// trialTimeout：试用的新版本启动后这么久还没有完成确认（一直连不上 Server）就退出，由启动器计一次启动
// （PLAN #27.7 第 5 步）。新版本自己的网络处理坏了时（代理、TLS、DNS），否则会一直重试、永远不回滚。
const trialTimeout = 10 * time.Minute

// UpgraderOptions 是 NewUpgrader 的可选参数（测试用）：零值表示本进程的版本和是否由启动器启动按实际情况取。
type UpgraderOptions struct {
	// Version 不为空时代替本进程的版本（buildinfo.Version）。
	Version string
	// ViaLauncher 不为 nil 时代替是否由启动器启动（KILN_LAUNCHER=1）。
	ViaLauncher *bool
	// Exec 不为 nil 时代替切换时替换进程的 syscall.Exec。
	Exec func(argv0 string, argv, env []string) error
}

// NewUpgrader 返回 Runner 的自升级状态（读 state.json）：h 是数据目录和程序，enabled 是 config.toml 的 upgrade。
func NewUpgrader(h *Home, enabled bool, log *slog.Logger, opts UpgraderOptions) *Upgrader {
	version, viaLauncher := buildinfo.Version(), os.Getenv(launcherEnv) == "1"
	if opts.Version != "" {
		version = opts.Version
	}
	if opts.ViaLauncher != nil {
		viaLauncher = *opts.ViaLauncher
	}
	return newUpgraderVersion(h, enabled, log, version, viaLauncher, opts.Exec)
}

// newUpgraderVersion 与 NewUpgrader 相同，本进程的版本、是否由启动器启动和 exec 由参数给出。
func newUpgraderVersion(h *Home, enabled bool, log *slog.Logger, version string, viaLauncher bool, execFn func(argv0 string, argv, env []string) error) *Upgrader {
	u := &Upgrader{
		l: newLayout(h.Dir, h.Prog.Name), log: log, version: upgrade.Normalize(version),
		disabled: !enabled, client: &http.Client{Timeout: downloadTimeout}, exec: execFn,
		started: time.Now(), registry: upgrade.DefaultRegistry,
	}
	u.pkg = upgrade.PackageName(u.l.prog, runtime.GOOS, runtime.GOARCH)
	u.status.State = UpgradeIdle
	u.status.Package = u.pkg
	if viaLauncher && u.l.installed() {
		if st, err := u.l.load(); err == nil {
			u.launched, u.boot = true, st
			u.baseCurrent, u.basePrevious = st.Current, st.Previous
		} else {
			log.Warn("自升级：读取启动状态失败，只下载不切换", "err", err)
		}
	}
	if u.boot != nil {
		u.status.Current, u.status.Previous = u.boot.Current, u.boot.Previous
		if t := u.boot.Trial; t != nil && t.Version == u.version {
			u.confirmPending = true
			u.status.State, u.status.Target = UpgradeSwitching, t.Version
		}
		if rb := u.boot.RolledBack; rb != nil && !u.confirmPending {
			u.status.State, u.status.Target, u.status.Error = UpgradeRolledBack, rb.Version, rb.Reason
		}
	}
	return u
}

// Cleanup 清掉上次下载中、解包中崩溃留下的残留（bin/tmp，解包目录也在其中；versions/ 中只有完整的版本）。
// 只能在持有数据目录锁之后调用（Run）：因为锁冲突而退出的第二个实例不能删掉正在运行的实例的解包目录。
func (u *Upgrader) Cleanup() {
	if u == nil {
		return
	}
	_ = os.RemoveAll(u.l.tmpDir())
}

// StatusMsg 返回上报给 Server 的升级状态。
func (u *Upgrader) StatusMsg() *runnerv1.UpgradeStatus {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := &runnerv1.UpgradeStatus{
		State: u.status.State, Target: u.status.Target, Error: u.status.Error, Generation: u.status.Generation,
		Launcher: u.launched, Disabled: u.disabled,
		Current: u.status.Current, Previous: u.status.Previous,
		Package: u.pkg, Latest: u.status.Latest, CheckedAt: u.status.CheckedAt, CheckError: u.status.CheckError, CheckSeq: u.status.CheckSeq,
	}
	if u.boot != nil {
		for _, f := range u.boot.Failed {
			st.Failed = append(st.Failed, &runnerv1.FailedVersion{Version: f.Version, Generation: f.Generation})
		}
	}
	return st
}

func (u *Upgrader) set(state, target, msg string) {
	u.status.State, u.status.Target, u.status.Error, u.status.Generation = state, target, msg, 0
}

// setGen 与 set 相同，并记下这个状态对应的指令的 generation（Server 据此区分这一轮的暂时失败和 retry 之前的失败）。
func (u *Upgrader) setGen(state, target string, gen int64, msg string) {
	u.set(state, target, msg)
	u.status.Generation = gen
}

// Trial 判断本进程是否是还没有确认的新版本。
func (u *Upgrader) Trial() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.confirmPending
}

// TrialRetry 判断试用的新版本遇到 err 时是否继续重试：只有网络错误（没有收到 Server 的响应）、并且还没超过试用期限时
// 重试；其余情况退出，由启动器计一次启动。不是试用版本时总是重试。
func (u *Upgrader) TrialRetry(err error, now time.Time) bool {
	if !u.Trial() {
		return true
	}
	return NetworkError(err) && now.Sub(u.started) < trialTimeout
}

// TrialContext 返回试用期间的请求用的 context：试用的新版本在启动后 trialTimeout 到期时取消进行中的请求（认证、第一次
// Acquire），响应头到了、正文一直不结束时也不会一直等下去（独立审查发现）；之后的错误按超过试用期限退出、由启动器计次。
// 不是试用版本时返回 ctx。
func (u *Upgrader) TrialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if !u.Trial() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, u.started.Add(trialTimeout))
}

// Confirm 在新版本第一次 Acquire 成功后调用（PLAN #27.7 第 5 步）：current = 新版本、previous = 旧版本、清空 trial，
// 删除不在 current / previous 中的版本目录。写 state.json 失败时返回错误：没有确认的版本不能开始执行任务（调用方退出，
// 由启动器计一次启动）。不是试用版本时什么也不做。
func (u *Upgrader) Confirm() error {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.confirmPending || u.boot == nil || u.boot.Trial == nil {
		return nil
	}
	trial := u.boot.Trial.Version
	err := u.update(func(st *BootState) error {
		if st.Trial == nil || st.Trial.Version != trial {
			return errLayoutChanged
		}
		st.Previous, st.Current, st.Trial, st.RolledBack = st.Current, trial, nil, nil
		return nil
	})
	switch {
	case errors.Is(err, errLayoutChanged):
		// 期间执行了 service install（trial 被清掉、current 换了）：本进程不再是试用版本，照常运行，等安装重启服务
		u.log.Warn("自升级：启动状态已被其他进程修改，不确认", "version", trial)
		u.confirmPending = false
		u.set(UpgradeIdle, "", "")
		return nil
	case err != nil:
		u.log.Error("自升级：确认新版本失败（退出，下次启动按试用计次）", "err", err)
		return fmt.Errorf("确认新版本：%w", err)
	}
	u.confirmPending = false
	u.baseCurrent, u.basePrevious = u.boot.Current, u.boot.Previous
	u.status.Current, u.status.Previous = u.boot.Current, u.boot.Previous
	u.set(UpgradeConfirmed, u.boot.Current, "")
	u.log.Info("自升级：新版本已确认", "version", u.boot.Current, "previous", u.boot.Previous)
	u.prune()
	return nil
}

// errLayoutChanged 表示 state.json 在本进程启动后被其他进程（service install）改过，按缓存的状态做的决定不再成立。
var errLayoutChanged = errors.New("启动状态已被其他进程修改（例如 service install），不切换")

// update 在布局锁下读磁盘上的 state.json、交给 fn 修改后写回，成功后更新 u.boot（调用方持有 mu）。fn 返回错误时不写。
func (u *Upgrader) update(fn func(st *BootState) error) error {
	unlock, err := u.l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := u.l.load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	if err := u.l.save(st); err != nil {
		return err
	}
	u.boot = st
	return nil
}

// prune 在布局锁下按磁盘上的 state.json 删除多余的版本（调用方持有 mu）。
func (u *Upgrader) prune() {
	unlock, err := u.l.lock()
	if err != nil {
		u.log.Warn("自升级：锁住启动状态失败，不清理已下载的版本", "err", err)
		return
	}
	defer unlock()
	if keep, ok := u.keepVersions(); ok {
		u.pruneVersions(keep...)
	}
}

// keepVersions 返回本地要保留的版本（调用方持有 mu）：本进程的版本、已经下载好等待切换的版本，以及 state.json 中的
// current、previous、正在试的版本。目标换了几次、或者一直不切换（前台运行、旧的服务入口只下载不切换）时，之前下载的
// 版本在下一次下载完成时删掉，磁盘占用有上限（独立审查发现）。
//
// state.json 总是重新读磁盘上的（调用方持有布局锁），不用 u.boot：不经过启动器运行时（前台 run、旧的服务入口）没有
// u.boot，但同一个数据目录的服务启动器仍按它启动（U2）；经过启动器运行时期间也可能执行了 service install。读不出（损坏、已安装启动器却没有 state.json）时返回 false，调用方不删除任何版本。
func (u *Upgrader) keepVersions() ([]string, bool) {
	keep := []string{u.version, u.ready, u.target}
	var b *BootState
	st, err := u.l.load()
	switch {
	case err == nil:
		b = st
	case errors.Is(err, fs.ErrNotExist) && !u.l.installed():
	default:
		u.log.Warn("自升级：读不出启动状态，不清理已下载的版本", "err", err)
		return nil, false
	}
	if b != nil {
		keep = append(keep, b.Current, b.Previous)
		if b.Trial != nil {
			keep = append(keep, b.Trial.Version)
		}
	}
	return slices.DeleteFunc(keep, func(v string) bool { return v == "" }), true
}

// pruneVersions 删除不在 keep 中的版本目录（调用方持有 mu）。
func (u *Upgrader) pruneVersions(keep ...string) {
	entries, err := os.ReadDir(u.l.versionsRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			if err := os.RemoveAll(filepath.Join(u.l.versionsRoot(), e.Name())); err != nil {
				u.log.Warn("自升级：删除旧版本", "dir", e.Name(), "err", err)
			}
		}
	}
}

// Directives 处理一次 Acquire 收到的指令中与升级有关的部分：记下 registry、需要时开始查询 latest、处理升级指令。
// 返回是否要排空（不再领取新任务，准备切换）。
func (u *Upgrader) Directives(ctx context.Context, d *runnerv1.RunnerDirectives) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	u.registry = upgrade.Registry(d.GetUpgradeRegistry())
	u.maybeCheck(ctx, d.GetUpgradeCheck(), time.Now())
	u.mu.Unlock()
	return u.handle(ctx, d.GetUpgrade())
}

// maybeCheck 在需要时开始在后台查询 latest（调用方持有 mu）：从没查过、registry 变了、Server 下发的检查序号变了
// （管理员点了"检查更新"），或者距上一次查询超过了间隔（成功 1 小时、失败 10 分钟）。
func (u *Upgrader) maybeCheck(ctx context.Context, seq int64, now time.Time) {
	if u.checking {
		return
	}
	interval := checkInterval
	if u.status.CheckError != "" {
		interval = checkRetryInterval
	}
	if !u.lastCheck.IsZero() && u.registry == u.checkReg && seq == u.status.CheckSeq && now.Sub(u.lastCheck) < interval {
		return
	}
	u.checking, u.lastCheck, u.checkReg = true, now, u.registry
	go u.check(context.WithoutCancel(ctx), u.registry, seq)
}

// check 查询 registry 中本程序、本平台的包的 dist-tags.latest（PLAN #27.12），结果写回 u.status。只认正式的 semver；
// 包不存在、latest 不是正式版本时清空 latest；网络等错误保留上一次查到的 latest。
func (u *Upgrader) check(ctx context.Context, registry string, seq int64) {
	latest, gone, err := u.queryLatest(ctx, registry)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checking = false
	u.status.CheckSeq = seq
	if err != nil {
		u.status.CheckError = err.Error()
		if gone {
			u.status.Latest = ""
		}
		u.log.Warn("自升级：查询最新版本失败", "package", u.pkg, "registry", registry, "err", err)
		return
	}
	u.status.Latest, u.status.CheckError, u.status.CheckedAt = latest, "", time.Now().Unix()
}

// queryLatest 返回包的 latest；gone 为真表示包不存在或 latest 不可用（不是暂时的错误）。
func (u *Upgrader) queryLatest(ctx context.Context, registry string) (latest string, gone bool, err error) {
	doc, err := u.packument(ctx, registry)
	switch {
	case err != nil:
		return "", false, err
	case doc == nil:
		return "", true, fmt.Errorf("%s 上没有 %s（这个平台暂无升级包）", registry, u.pkg)
	}
	latest = doc.DistTags["latest"]
	if !upgrade.Valid(latest) || upgrade.Prerelease(latest) {
		return "", true, fmt.Errorf("%s 的 dist-tags.latest 是 %q，不是正式版本", u.pkg, latest)
	}
	return latest, false, nil
}

// packument 查询 registry 中本程序、本平台的包（不存在时返回 nil, nil），不带 token。
func (u *Upgrader) packument(ctx context.Context, registry string) (*npm.Packument, error) {
	c, err := npm.NewClient(registry, "", u.l.prog+"/"+buildinfo.Version(), u.npmOpts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	return c.Get(ctx, u.pkg)
}

// handle 处理一次 Acquire 收到的升级指令，返回是否要排空（不再领取新任务，准备切换）。
func (u *Upgrader) handle(ctx context.Context, d *runnerv1.UpgradeDirective) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.confirmPending {
		return false
	}
	if d == nil || d.Version == u.version {
		// 没有指令（请求被取消、已经结束）：不再排空、不切换；上报的 ready / draining 是旧目标的，改回 idle
		// （已经下载好的版本保留，再次下发时直接排空）
		u.withdraw()
		return false
	}
	switch {
	case !upgrade.Valid(d.Version):
		u.fail(d.Version, d.Generation, "指令中的版本号无效")
		return false
	case u.disabled:
		u.set(UpgradeManual, d.Version, "config.toml 中 upgrade = false，需要手动升级")
		return false
	case u.boot != nil && u.boot.failedAt(d.Version, d.Generation):
		return false // 失败过（回滚、integrity 不符），等管理员重试
	}
	// 复用之前下载好的包时也按本轮指令的 generation 写 trial：回滚后的失败记录要挡住这一轮（U3），否则管理员
	// 改了目标（generation+1）又指定回同一版本时，回滚后会被同一条指令再试一次
	u.target, u.targetGen = d.Version, d.Generation
	if d.Generation <= u.failedGen {
		return false // 这一轮已经失败（上报的 failed 结束请求）；管理员重新请求（更大的 generation）立即执行
	}
	if u.ready == d.Version {
		if !u.launched {
			u.set(UpgradeManual, d.Version, "没有经过启动器运行（前台 run 或旧的服务），需要手动升级或重新执行 service install")
			return false
		}
		u.setGen(UpgradeDraining, d.Version, d.Generation, "")
		return true
	}
	if u.busy {
		return false
	}
	u.busy = true
	u.setGen(UpgradeDownloading, d.Version, d.Generation, "")
	if u.boot != nil && u.boot.RolledBack != nil {
		_ = u.update(func(st *BootState) error { st.RolledBack = nil; return nil })
	}
	go u.download(context.WithoutCancel(ctx), d.Version, d.Generation, u.registry)
	return false
}

// withdraw 在指令被撤回时把旧目标的 ready / draining 改回 idle（调用方持有 mu）。下载中的不改：下载结束后改为 ready，
// 下一次 Acquire 没有指令时再改回 idle。
func (u *Upgrader) withdraw() {
	u.target, u.targetGen = "", 0
	if u.status.State == UpgradeReady || u.status.State == UpgradeDraining {
		u.set(UpgradeIdle, "", "")
	}
}

// download 在后台从 registry 下载并核对新版本 version，结果写回 u。gen 是指令的 generation。
func (u *Upgrader) download(ctx context.Context, version string, gen int64, registry string) {
	d := &runnerv1.UpgradeDirective{Version: version, Generation: gen}
	err := u.prepare(ctx, version, registry)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.busy = false
	// integrity 不符另外记入失败列表（写进 state.json，重启后同一 generation 也不再尝试）；记不下时只靠 failedGen
	if errors.Is(err, errIntegrity) && u.boot != nil {
		uerr := u.update(func(st *BootState) error {
			st.addFailed(FailedVersion{Version: d.Version, Generation: d.Generation, Reason: err.Error()})
			return nil
		})
		if uerr != nil {
			u.log.Warn("自升级：记录失败的版本", "version", d.Version, "err", uerr)
		}
	}
	if d.Version != u.target {
		// 下载期间指令换了（撤回、改成别的版本）：结果作废，不替换已经选定的排空目标；上报的下载中改回 idle
		u.log.Info("自升级：下载完成时指令已经换了，丢弃", "version", d.Version, "target", u.target, "err", err)
		if u.status.State == UpgradeDownloading && u.status.Target == d.Version {
			u.set(UpgradeIdle, "", "")
		}
		u.prune()
		return
	}
	if err == nil {
		u.ready = d.Version
		u.setGen(UpgradeReady, d.Version, d.Generation, "")
		u.log.Info("自升级：新版本已下载并核对", "version", d.Version)
		u.prune()
		return
	}
	u.log.Warn("自升级：准备新版本失败", "version", d.Version, "err", err)
	u.fail(d.Version, d.Generation, err.Error())
}

// fail 记下 generation 为 gen 的这一轮失败（调用方持有 mu）：上报 failed（带原因和 generation），Server 收到后结束请求；
// 本进程不再尝试这一轮（不论 Server 什么时候收到，failed 都一直上报，不会因为时间到期变回 idle 而丢失失败结果）。
func (u *Upgrader) fail(version string, gen int64, msg string) {
	u.setGen(UpgradeFailed, version, gen, msg)
	u.failedGen = max(u.failedGen, gen)
}

// prepare 从 registry 读 version 的 dist、下载、核对、解出程序到 versions/<版本>/ 并运行 version 核对（PLAN #27.7
// 第 1 步、#27.12）。目录已经存在且 version 输出正确时直接复用（例如 rename 之后、写 state.json 之前崩溃）。
func (u *Upgrader) prepare(ctx context.Context, version, registry string) error {
	final := u.l.versionDir(version)
	if err := u.checkVersion(ctx, u.l.programIn(final), version); err == nil {
		return nil
	}
	dist, err := u.resolve(ctx, version, registry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(u.l.tmpDir(), 0o755); err != nil {
		return err
	}
	data, err := u.fetch(ctx, dist)
	if err != nil {
		return err
	}
	file := upgrade.PackageFile(u.l.prog)
	staging := u.l.stagingDir(version)
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	// 没有 rename 到 versions/ 之前，任何失败（解包、核对、加锁）都清掉解包目录
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.MkdirAll(u.l.versionsRoot(), 0o755); err != nil {
		return err
	}
	if err := extractProgram(data, file, u.l.programIn(staging)); err != nil {
		return err
	}
	if err := u.checkVersion(ctx, u.l.programIn(staging), version); err != nil {
		return fmt.Errorf("%w：%w", errIntegrity, err)
	}
	// 安装到 versions/ 时持有布局锁：service install、启动器和清理也在这把锁下改动版本目录
	unlock, err := u.l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	// 其他进程可能已经装好了同一个版本：直接用它的，不删除、不覆盖（正在运行的进程可能正在执行它）
	if err := u.checkVersion(ctx, u.l.programIn(final), version); err == nil {
		return nil
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	installed = true
	return syncDir(u.l.versionsRoot())
}

// resolve 从 registry 读 version 的 dist（下载地址和 integrity）：包或版本不存在、integrity 不是 sha512、地址不是
// http(s) 时失败（可以重试：registry 的缓存可能还没有更新）。
func (u *Upgrader) resolve(ctx context.Context, version, registry string) (*npm.Dist, error) {
	doc, err := u.packument(ctx, registry)
	if err != nil {
		return nil, fmt.Errorf("查询 %s：%w", u.pkg, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s 上没有 %s", registry, u.pkg)
	}
	v, ok := doc.Versions[version]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s 上没有 %s@%s", registry, u.pkg, version)
	case !strings.HasPrefix(v.Dist.Integrity, "sha512-"):
		return nil, fmt.Errorf("%s@%s 的 integrity %q 不是 sha512", u.pkg, version, v.Dist.Integrity)
	case !strings.HasPrefix(v.Dist.Tarball, "https://") && !strings.HasPrefix(v.Dist.Tarball, "http://"):
		return nil, fmt.Errorf("%s@%s 的下载地址 %q 无效", u.pkg, version, v.Dist.Tarball)
	}
	return &v.Dist, nil
}

// fetch 下载 tarball：最多读 maxPackageSize 字节（多了就失败），sha512 integrity 必须与 registry 的记录一致。
func (u *Upgrader) fetch(ctx context.Context, d *npm.Dist) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Tarball, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s：%w", d.Tarball, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s：%s", d.Tarball, resp.Status)
	}
	h := sha512.New()
	var buf bytes.Buffer
	n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(resp.Body, maxPackageSize+1))
	if err != nil {
		return nil, fmt.Errorf("下载 %s：%w", d.Tarball, err)
	}
	if n > maxPackageSize {
		return nil, fmt.Errorf("%w：超过大小上限 %d 字节", errIntegrity, int64(maxPackageSize))
	}
	if got := "sha512-" + base64.StdEncoding.EncodeToString(h.Sum(nil)); got != d.Integrity {
		return nil, fmt.Errorf("%w：integrity %s，registry 中是 %s", errIntegrity, got, d.Integrity)
	}
	return buf.Bytes(), nil
}

// extractProgram 从 npm 包中只取出 name 这一个普通文件写到 dst（可执行）。其他条目（路径穿越、符号链接、硬链接等）
// 一律不解，name 不是普通文件或超过大小上限时失败（PLAN #27.5 威胁模型）。
func extractProgram(data []byte, name, dst string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w：不是 gzip：%w", errIntegrity, err)
	}
	// 只读，关闭失败不影响已经解出的内容
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w：包中没有 %s", errIntegrity, name)
		}
		if err != nil {
			return fmt.Errorf("%w：读取 tar：%w", errIntegrity, err)
		}
		if h.Name != name {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w：%s 不是普通文件", errIntegrity, name)
		}
		if h.Size <= 0 || h.Size > maxProgramSize {
			return fmt.Errorf("%w：%s 的大小 %d 超出范围", errIntegrity, name, h.Size)
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) //nolint:gosec // 要运行的程序，需要可执行权限
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(tr, h.Size))
		if err == nil && n != h.Size {
			err = fmt.Errorf("%w：%s 不完整", errIntegrity, name)
		}
		if err == nil {
			err = f.Sync()
		}
		return errors.Join(err, f.Close())
	}
}

// checkVersion 运行 program version，输出必须是"<程序名> <版本>"（版本可以带 v）。
func (u *Upgrader) checkVersion(ctx context.Context, program, version string) error {
	if !executable(program) {
		return fmt.Errorf("%s 不存在或不能执行", program)
	}
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, program, "version").Output()
	if err != nil {
		return fmt.Errorf("运行 %s version：%w", program, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || f[0] != u.l.prog || upgrade.Normalize(f[1]) != version {
		return fmt.Errorf("%s version 输出 %q，想要 %s %s", program, strings.TrimSpace(string(out)), u.l.prog, version)
	}
	return nil
}

// SwitchTo 切换到已经准备好的版本（没有正在执行的 job 时调用，PLAN #27.7 第 3 步）：原子写 trial，然后 exec 启动器。
// 只在失败时返回。
func (u *Upgrader) SwitchTo() error {
	if u == nil {
		return errors.New("没有启用自升级")
	}
	u.mu.Lock()
	// 切换到最后一次指令选定的版本（和它的 generation），并且它已经下载好；排空期间指令撤回或换成别的版本时不切换
	v := u.target
	if !u.launched || u.boot == nil || v == "" || v != u.ready {
		u.mu.Unlock()
		return errors.New("没有可以切换的版本")
	}
	// 在布局锁下按磁盘上的 state.json 写 trial：current / previous 与本进程启动时不同（期间执行了 service install），或者
	// 已经有 trial 时不切换，不在别人刚安装的版本上叠一个旧下载的 trial
	cur, prev := u.baseCurrent, u.basePrevious
	if err := u.update(func(st *BootState) error {
		if st.Current != cur || st.Previous != prev || st.Trial != nil {
			return errLayoutChanged
		}
		st.Trial = &BootTrial{Version: v, Generation: u.targetGen}
		return nil
	}); err != nil {
		u.fail(v, u.targetGen, "写启动状态："+err.Error())
		u.mu.Unlock()
		return err
	}
	u.setGen(UpgradeSwitching, v, u.targetGen, "")
	u.mu.Unlock()
	u.log.Info("自升级：切换到新版本", "version", v)
	execFn := u.exec
	if execFn == nil {
		execFn = syscall.Exec
	}
	launcher := u.l.launcher()
	err := execFn(launcher, []string{launcher, "launch"}, os.Environ())
	// exec 失败：撤销 trial（否则下次启动会去试一个没有切换过去的版本）
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.update(func(st *BootState) error {
		if st.Trial == nil || st.Trial.Version != v {
			return errLayoutChanged
		}
		st.Trial = nil
		return nil
	})
	u.fail(v, u.targetGen, "启动器启动失败："+err.Error())
	return err
}

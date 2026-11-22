// kiln-runner 在构建机上执行 job，只发出站连接（PLAN #8）。
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/logging"
	"github.com/hewenyu/kilnflow.dev/internal/runner"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/toolchainctl"
)

const usage = `用法：kiln-runner <命令> [参数]

  enroll --server URL --token TOKEN [--name NAME]   用一次性令牌注册
  run                                               前台运行（不自动切换版本：收到升级指令时只下载核对，上报需要手动升级）
  launch                                            服务入口：按 bin/state.json 启动当前版本或正在试的新版本（自升级，service install 写入）
  service install | uninstall                       安装为开机自启的服务（macOS LaunchAgent / systemd 用户服务）；
                                                    入口是启动器 bin/launcher，收到升级指令后自动切换、失败自动回滚
  drain | undrain                                   停止 / 恢复领取新任务（正在执行的任务继续）
  doctor [--network]                                检查运行环境；--network 再以 host job 的代理设置（[proxy.host]）
                                                    访问常见下载入口，区分 DNS、代理、认证、TLS、超时等失败
  gc [--json]                                       按保留策略清理：超过 12 小时的保留工作区，12 小时没有用到的缓存，30 天没有用到的
                                                    runtime，拉取超过 12 小时的镜像；服务没有运行时还按容量回收（toolcache_max_size、
                                                    runtime_max_size、cache_max_size、磁盘水位；先删缓存，最后才删 runtime）
                                                    （服务运行时每小时在空闲时自动清理，一般不需要手动执行）；有对象删除失败时退出码为 1
  cache [--json]                                    列出本地缓存：类别、所属仓库、大小、最后使用和到期时间，以及上一次清理的结果
  version
  git-credential get | store | erase                由 step 中的 git 调用：经本 job 的凭据通道取得这次传输的只读 token；
                                                    git exec 镜像中的 git-remote-http(s) 也是 kiln-runner（传输包装）
  toolchain …                                       给 AI 执行节点的连接器（kiln-orca）安装、回收任务用的工具链（PLAN #33.12）；
                                                    不读配置、不连接 Server，目录由 --root 给出：
%s
数据目录：$KILN_RUNNER_DIR，默认 ~/kiln（config.toml、密钥、缓存、工作区）
日志：KILN_DEBUG 设置后输出调试日志；KILN_LOG_FORMAT 为 text（默认）或 json；
      KILN_LOG_DIR 设置后 run 的日志写到该目录中按天切分的 runner-<日期>.log，保留 7 天（service install 自动设置）
退出码：0 成功，1 运行错误，2 用法错误
`

// 退出码：0 成功，1 运行错误，2 用法错误。
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError 表示命令行用法错误，退出码为 2。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// app 持有一次命令执行的输出。
type app struct {
	// stdin 只有 toolchain ensure 读取。
	stdin          io.Reader
	stdout, stderr io.Writer
	// runGC 执行清理，为 nil 时用 runner.GC（测试替换它，观察清理期间的状态）。
	runGC func(context.Context, *runner.Config, runner.GCOptions, *slog.Logger) *runner.GCReport
}

func main() {
	// step 中的 git 调用的 kiln-runner（传输包装、credential helper）：按 argv[0] 或子命令分派，不读配置、不处理信号
	if code, ok := runner.GitCommand(os.Args, os.Stdin, os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 执行 kiln-runner 并返回退出码；所有清理都在这里通过 defer 完成。
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 收到第一个信号后恢复默认的信号处理：Runner 会等正在执行的 job 结束（可能很久），
	// 期间再按一次 Ctrl-C 立即退出。正常退出时 defer 的 stop 也会结束这个 goroutine。
	go func() {
		<-ctx.Done()
		stop()
	}()
	a := &app{stdin: os.Stdin, stdout: stdout, stderr: stderr}
	return a.exitCode(a.execute(ctx, args))
}

func (a *app) execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usageText())
		return &usageError{}
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "enroll":
		return a.enroll(ctx, rest)
	case "run":
		return a.runForeground(ctx)
	case "launch":
		// 服务入口（PLAN #27.7）：按 bin/state.json 选版本，exec 它的 run（只在失败时返回）
		return runnerkit.Launch(runner.DefaultDir(), runner.Program.Name, []string{"run"}, syscall.Exec)
	case "service":
		return a.service(ctx, rest)
	case "drain", "undrain":
		return a.drain(cmd == "drain")
	case "doctor":
		return a.doctor(ctx, rest)
	case "gc":
		return a.gc(ctx, rest)
	case "cache":
		return a.cache(ctx, rest)
	case "toolchain":
		return a.toolchain(ctx, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, "kiln-runner", buildinfo.Version())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, usageText())
		return nil
	default:
		return usageErrorf("未知的命令 %s\n运行 kiln-runner help 查看用法", cmd)
	}
}

// usageText 是完整的用法（toolchain 子命令的用法由 toolchainctl 给出）。
func usageText() string { return fmt.Sprintf(usage, toolchainctl.Usage) }

// toolchain 执行 toolchain 子命令（toolchainctl）：不读配置、不取数据目录锁、不连接 Server。
func (a *app) toolchain(ctx context.Context, args []string) error {
	err := toolchainctl.Run(ctx, args, a.stdin, a.stdout, a.stderr, toolchainctl.Options{
		TempRetention: runnerkit.Retention, Remove: runnerkit.RemoveTree,
	})
	if ue, ok := errors.AsType[*toolchainctl.UsageError](err); ok {
		return &usageError{msg: ue.Msg}
	}
	return err
}

// exitCode 输出错误并把它映射为退出码。
func (a *app) exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if ue, ok := errors.AsType[*usageError](err); ok {
		if ue.msg != "" {
			fmt.Fprintf(a.stderr, "错误：%s\n", ue.msg)
		}
		return exitUsage
	}
	fmt.Fprintf(a.stderr, "错误：%v\n", err)
	return exitError
}

// loadConfig 读取数据目录中的 config.toml；只有需要配置的命令才调用，
// 这样配置损坏时 version、help 仍然可用。
func loadConfig() (*runner.Config, error) {
	return runner.LoadConfig(runner.DefaultDir())
}

func (a *app) logger() (*slog.Logger, error) {
	log, err := logging.FromEnv(a.stderr)
	if err != nil {
		return nil, &usageError{msg: err.Error()}
	}
	slog.SetDefault(log)
	return log, nil
}

// serviceLogger 是 run 使用的 Logger：设置了 KILN_LOG_DIR（service install 写入服务定义）时
// 写到该目录中按天切分的 runner-<日期>.log 并保留 7 天，否则写到 stderr。
func (a *app) serviceLogger() (*slog.Logger, func() error, error) {
	log, closeLog, err := logging.Setup(a.stderr, runner.LogName)
	if err != nil {
		return nil, nil, &usageError{msg: err.Error()}
	}
	slog.SetDefault(log)
	return log, closeLog, nil
}

// enroll 先解析参数再读配置：配置损坏时 enroll -h 仍然可用。
func (a *app) enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	server := fs.String("server", "", "Server 地址，默认为 config.toml 中的 server")
	token := fs.String("token", "", "注册令牌（在 Dashboard 的 Runners 页面或 kiln runner token 生成）")
	name := fs.String("name", "", "Runner 名称，默认主机名")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &usageError{} // flag 已经输出了错误和参数说明
	}
	if fs.NArg() > 0 {
		return usageErrorf("多余的参数 %q", fs.Args())
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	serverURL := cmp.Or(*server, cfg.Server)
	if serverURL == "" || *token == "" {
		return usageErrorf("需要 --server 和 --token")
	}
	st, err := runner.Enroll(ctx, cfg, serverURL, *token, *name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已注册：%s（id %d，pool %s）\n配置在 %s\n下一步：kiln-runner service install（或 kiln-runner run 前台运行）\n",
		st.Name, st.RunnerID, st.Pool, cfg.Dir)
	return nil
}

// runForeground 前台运行 Runner，直到收到信号或被吊销（这两种情况以 0 退出，服务不再拉起；其余错误以非 0 退出，由服务拉起）。
func (a *app) runForeground(ctx context.Context) error {
	log, closeLog, err := a.serviceLogger()
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }() // 进程即将退出，关闭失败无法处理
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	r, err := runner.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	// 过期的工作区、缓存和镜像在 Run 中空闲时清理（启动后第一轮即清理）
	if err := r.Run(ctx); err != nil {
		if errors.Is(err, runner.ErrRevoked) {
			log.Error("Runner 已被吊销，退出；需要重新注册")
			// 退出码 0：服务只在非 0 退出或被信号杀死时重启（launchd SuccessfulExit=false、systemd Restart=on-failure），
			// 被吊销后不再被拉起
			return nil
		}
		return err
	}
	return nil
}

func (a *app) service(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("用法：kiln-runner service install | uninstall")
	}
	switch args[0] {
	case "install":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if _, err := cfg.LoadState(); err != nil {
			return err
		}
		p, err := runner.InstallService(ctx, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "服务已安装并启动：%s\n日志：%s（按天切分，保留 7 天）\n", p, filepath.Join(cfg.LogDir(), runner.LogName+"-<日期>.log"))
	case "uninstall":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if err := runner.UninstallService(ctx, cfg); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "服务已卸载")
	default:
		return usageErrorf("未知的子命令 service %s", args[0])
	}
	return nil
}

func (a *app) drain(on bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := runner.SetDrain(cfg, on); err != nil {
		return err
	}
	if on {
		fmt.Fprintln(a.stdout, "已设置 drain：不再领取新任务，正在执行的任务继续")
	} else {
		fmt.Fprintln(a.stdout, "已恢复领取任务")
	}
	return nil
}

func (a *app) doctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	network := fs.Bool("network", false, "以 host job 的代理设置访问常见下载入口")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &usageError{}
	}
	if fs.NArg() > 0 {
		return usageErrorf("多余的参数 %q", fs.Args())
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ok := runner.Doctor(ctx, cfg, runner.DoctorOptions{Network: *network}, func(ok bool, item, detail string) {
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		fmt.Fprintf(a.stdout, "%s %-12s %s\n", mark, item, detail)
	})
	if !ok {
		return errors.New("运行环境检查未通过")
	}
	return nil
}

// jsonFlag 解析只有 --json 一个参数的命令。
func (a *app) jsonFlag(name string, args []string) (bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, err
		}
		return false, &usageError{}
	}
	if fs.NArg() > 0 {
		return false, usageErrorf("多余的参数 %q", fs.Args())
	}
	return *asJSON, nil
}

// gc 按保留策略清理过期的工作区、本地缓存和镜像；只读写数据目录和 docker，未注册的机器上也能执行。
// 服务正在运行时（数据目录被锁住）不按容量回收、不删除残留的容器：服务可能正在执行 job，它会在空闲时自己清理。
// 有对象删除失败时返回错误（退出码 1），失败的对象下次清理时重试。
func (a *app) gc(ctx context.Context, args []string) error {
	asJSON, err := a.jsonFlag("gc", args)
	if err != nil {
		return err
	}
	log, err := a.logger()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// 服务没有运行时在清理期间锁住数据目录：服务这时启动会失败（由服务管理器稍后重启），不会与清理同时进行
	unlock, running, err := runner.TryLockDataDir(cfg)
	if err != nil {
		return err
	}
	if unlock != nil {
		defer unlock()
	}
	opts := runner.GCOptions{Docker: runner.DockerIfAvailable(ctx, cfg), Evict: !running}
	if st, err := cfg.LoadState(); err == nil {
		opts.LabelID = st.RunnerID
		if !running {
			opts.RunnerID = st.RunnerID
		}
	}
	runGC := a.runGC
	if runGC == nil {
		runGC = runner.GC
	}
	rep := runGC(ctx, cfg, opts, log)
	if running {
		rep.Notes = append(rep.Notes, "kiln-runner 服务正在运行：只按保留期清理，跳过按容量回收和残留容器的清理（服务会在空闲时自己执行）")
	}
	if asJSON {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.stdout, "删除 %d 个对象，共 %s\n", rep.Removed, bytesize.Format(rep.RemovedBytes))
		for _, n := range rep.Notes {
			fmt.Fprintf(a.stdout, "说明：%s\n", n)
		}
		for _, f := range rep.Failures {
			fmt.Fprintf(a.stdout, "失败：[%s] %s：%s\n", f.Kind, f.Path, f.Error)
		}
	}
	return rep.Err()
}

// cache 列出本地缓存（与上报给 Server 的清单相同）。
func (a *app) cache(ctx context.Context, args []string) error {
	asJSON, err := a.jsonFlag("cache", args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	rep, err := runner.Inventory(ctx, cfg, runner.DockerIfAvailable(ctx, cfg))
	if err != nil {
		fmt.Fprintf(a.stderr, "警告：%v\n", err)
	}
	if asJSON {
		b, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: true}.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.stdout, string(b))
		return err
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "类别\t仓库\t平台\t执行身份\t名称\t大小\t最后使用\t到期")
	var total int64
	for _, e := range rep.Entries {
		repo := "—"
		if e.RepoId != 0 {
			repo = strconv.FormatInt(e.RepoId, 10)
		}
		exp := "—"
		if e.ExpiresAt != 0 {
			exp = time.Unix(e.ExpiresAt, 0).Format(time.DateTime)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Kind, repo, dash(e.Platform), dash(e.Domain), e.Name, bytesize.Format(e.SizeBytes),
			time.Unix(e.LastUsedAt, 0).Format(time.DateTime), exp)
		total += e.SizeBytes
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "\n共 %s", bytesize.Format(total))
	if rep.DiskTotalBytes > 0 {
		fmt.Fprintf(a.stdout, "；磁盘 %s，可用 %s", bytesize.Format(rep.DiskTotalBytes), bytesize.Format(rep.DiskFreeBytes))
	}
	fmt.Fprintln(a.stdout)
	if rep.GcAt != 0 {
		fmt.Fprintf(a.stdout, "上一次清理：%s，删除 %d 个对象（%s），失败 %d 个\n", time.Unix(rep.GcAt, 0).Format(time.DateTime),
			rep.GcRemoved, bytesize.Format(rep.GcRemovedBytes), len(rep.GcFailures))
		for _, f := range rep.GcFailures {
			fmt.Fprintf(a.stdout, "  失败：[%s] %s：%s\n", f.Kind, f.Path, f.Error)
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

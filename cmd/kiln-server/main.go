// kiln-server 是 Kiln 的控制面：接收 GitHub webhook、规划、调度、Runner API、发布和 Dashboard。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hewenyu/kilnflow.dev/internal/logging"
	"github.com/hewenyu/kilnflow.dev/internal/server"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
)

// 退出码：0 成功，1 运行错误，2 用法或配置错误。
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 执行 kiln-server 并返回退出码；所有清理都在这里通过 defer 完成。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Fprintln(stdout, "kiln-server", buildinfo.Version())
			return exitOK
		case "help":
			fmt.Fprint(stdout, usage)
			return exitOK
		}
	}

	fs := flag.NewFlagSet("kiln-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 错误由下面统一输出，避免重复
	envFile := fs.String("env-file", "", "KEY=VALUE 格式的环境变量文件")
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "输出版本")
	fs.BoolVar(&showVersion, "v", false, "输出版本")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return exitOK
		}
		fmt.Fprintf(stderr, "错误：%v\n运行 kiln-server --help 查看用法\n", err)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误：多余的参数 %q\n运行 kiln-server --help 查看用法\n", fs.Args())
		return exitUsage
	}
	if showVersion {
		fmt.Fprintln(stdout, "kiln-server", buildinfo.Version())
		return exitOK
	}

	if *envFile != "" {
		if err := server.LoadEnvFile(*envFile); err != nil {
			fmt.Fprintf(stderr, "错误：读取 env 文件：%v\n", err)
			return exitUsage
		}
	}
	log, closeLog, err := logging.Setup(stderr, "kiln-server")
	if err != nil {
		fmt.Fprintf(stderr, "错误：%v\n", err)
		return exitUsage
	}
	defer func() { _ = closeLog() }() // 进程即将退出，关闭失败无法处理
	cfg, err := server.LoadConfig()
	if err != nil {
		log.Error("配置错误", "err", err)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := server.New(ctx, cfg, log)
	if err != nil {
		log.Error("启动失败", "err", err)
		return exitError
	}
	defer s.Close()
	if err := s.Run(ctx); err != nil {
		log.Error("退出", "err", err)
		return exitError
	}
	return exitOK
}

const usage = `kiln-server [--env-file FILE]：Kiln 的控制面。配置来自环境变量（或 --env-file 指定的 KEY=VALUE 文件）：

  KILN_DATABASE_URL            Postgres 连接串（必填）
  KILN_ADMIN_TOKEN[_FILE]      管理 token，至少 16 个字符（必填，CLI 和 Dashboard 登录用）
  KILN_MASTER_KEY[_FILE]       32 字节主密钥（hex 或 base64），用于加密 secret（必填）
  KILN_PUBLIC_URL              外部访问地址，例如 https://kiln.example.com
  KILN_LISTEN                  监听地址，默认 :8080
  KILN_DATA_DIR                数据目录（CAS、日志），默认 ./kiln-data
  KILN_GITHUB_APP_ID           GitHub App ID
  KILN_GITHUB_PRIVATE_KEY[_FILE]  App 私钥（PEM）
  KILN_GITHUB_WEBHOOK_SECRET   webhook secret
  KILN_ALLOWED_ACCOUNTS        允许安装 App 的账号（登录名或 ID，逗号分隔）
  KILN_GITHUB_CLIENT_ID        可选：Dashboard 的 GitHub 登录（与 KILN_GITHUB_CLIENT_SECRET 一起设置）
  KILN_GITHUB_CLIENT_SECRET    可选：同上
  KILN_GITHUB_API_URL          可选：GitHub API 地址，默认 https://api.github.com
  KILN_GITHUB_UPLOAD_URL       可选：Release asset 上传地址，默认 https://uploads.github.com
  KILN_ADMINS / KILN_USERS     可选：可以用 GitHub 登录 Dashboard 的账号
  KILN_LEASE_TTL               可选：Runner 租约时长，默认 60s
  KILN_RECONCILE_INTERVAL      可选：周期对账的间隔（至少 1m），默认不对账，只按 webhook 事件工作
  KILN_RECONCILE_REPOS         可选：周期对账的范围，owner/name 逗号分隔，* 表示全部仓库（与上一项同时设置）
  KILN_RETENTION               可选：run 结束后日志、产物、缓存等过程数据的保留时间（例如 12h、7d，至少 1h），默认 12h；
                               超过后只保留结果（状态、耗时、原因），每小时检查一次
  KILN_DEBUG                   设置后输出调试日志
  KILN_LOG_FORMAT              日志格式：text（默认）或 json
  KILN_LOG_DIR                 可选：日志写到该目录中按天切分的 kiln-server-<日期>.log，保留 7 天（默认写到 stderr）
`

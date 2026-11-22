// kiln 是 Kiln 的命令行工具：管理 Server（runner、secret、变量、environment、run）和本地检查（lint、runtime lock）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"connectrpc.com/connect"

	"github.com/hewenyu/kilnflow.dev/internal/buildinfo"
)

const usage = `用法：kiln <命令> [参数]

连接 Server
  login --server URL [--token TOKEN]       保存 Server 地址和 admin token（不给 --token 时从标准输入读取）
  admin [status]                            管理员绑定的 GitHub 账号和 Dashboard 的 admin token 登录状态
  admin token-login on|off                  开启 / 关闭 Dashboard 的 admin token 登录（关闭前先在 Dashboard 绑定 GitHub 账号）
  admin unbind                              解除绑定的 GitHub 账号（同时恢复 admin token 登录）

Runner
  runner token [--kind ci|ai] [--role review|development] [--pool P] [--ttl 分钟]
                                            生成一次性注册令牌（--kind ai 用于 kiln-orca）
  runner list                               CI Runner 列表：状态、平台、规格（核数 / 内存）、已分配 / 有效容量、本地缓存（AI 执行节点见 ai runners）
  runner drain NAME | undrain NAME | revoke NAME
  runner delete NAME                        删除已吊销、没有未结束任务的 Runner（名称可重新注册，尝试记录保留名称）
  runner cache NAME                         Runner 上报的本地缓存：类别、仓库、大小、最后使用和到期时间，上一次清理的结果
  runner cache purge NAME [--repo R] [--kind K ...] [--domain D] [--dry-run]
                                            清理 Runner 的本地缓存（先预览；Runner 在没有正在执行的 job 时执行）
  runner upgrade                            按台升级：registry、每台 Runner 的版本、npm 上的最新版本、升级请求
  runner upgrade start RUNNER [VERSION]     把这台 Runner 升级到它查到的最新版本（start 可省略）；cancel RUNNER 取消
  runner upgrade check [RUNNER]             让 Runner 立即查询最新版本；registry URL|default 设置 npm registry

仓库与 run
  repo list                                 列出 Kiln 中的仓库（只读本地数据，不请求 GitHub）
  repo sync                                 刷新仓库目录：列出 App 的安装和仓库（不读取分支、tag、PR）
  repo reconcile REPO                       对一个仓库补对账，补上漏掉的 webhook（第一次只记录基线）
  cache bump REPO                           让该仓库的全部缓存失效
  runs [--repo R] [-n 30]                   最近的 run
  run ID                                    run 详情和 job 列表
  logs JOB_ID [-f] [--attempt ID]           job 日志（-f 持续输出）
  cancel RUN_ID
  rerun RUN_ID [--mode failed|all|all_no_cache]
  rerun-job JOB_ID                          重跑一个 job 及其下游
  dispatch REPO WORKFLOW [--ref REF] [-i key=value ...]

AI（与 CI 的 run、job、Runner 分开）
  ai review REPO [--ref REF]                手动触发 AI 审计（仓库需要 .kiln/agent/config.yml，由 AI 执行节点执行）
  ai requests [--repo R] [--type review|develop] [--pr N] [--state S] [-n 30]
                                            最近的 AI 请求（审计、开发）
  ai show ID                                AI 请求详情和各平台任务
  ai cancel ID                              取消 AI 请求
  ai rerun ID [--mode failed|all] [--task TASK_ID]
                                            重跑已结束的 AI 审计请求（默认只重跑没有成功的任务），建出新的请求
  ai log TASK_ID [-f] [--attempt ID]        AI 任务的日志（-f 持续输出）
  ai runners                                AI 执行节点：角色、状态、平台、协议版本、Orca / Docker、Agent
  ai dev sync REPO                          从 GitHub 全量同步 AI 开发的任务池（有 kiln:ready 的打开的 Issue）
  ai dev pause REPO | resume REPO           暂停 / 恢复新的领取（已有的工作项照常收尾）
  ai dev list [REPO] [-n 50]                AI 开发的工作项：状态、Issue、PR、返修轮次、原因
  ai dev stop ID | retry ID                 终止工作项（释放 Issue，PR 保持原样）/ 让受阻的工作项再返修一轮

合并队列与自动 tag
  merge-queue list REPO                     合并队列：排队中的 PR 及其状态，最近合并 / 移出的 PR
  merge-queue add REPO PR | remove REPO PR  加入 / 移出合并队列（等同于给 PR 加上 / 去掉 label kiln:merge）
  autotag list REPO [-n 50]                 自动打 tag 的记录：合并的 commit、PR、tag、状态和原因
  autotag retry REPO SHA                    重新处理失败或跳过的记录

配置（作用域：--account A 账号级；--repo owner/name 仓库级；再加 --env E 为 environment 级）
  secret set NAME [作用域]                  值从标准输入读取
  secret list [作用域] | secret delete NAME [作用域]
  var set NAME VALUE [作用域] | var list [作用域] | var delete NAME [作用域]
  env set NAME (--account A | --repo R) [--ref PATTERN ...] [--publish-to owner/name ...]
  env list [--account A] | env delete NAME (--account A | --repo R)
  npm set --account A --scope @SCOPE [--registry URL]
                                            账号级 npm 配置，token 从标准输入读取；release.npm 发布到 @SCOPE/<name>
  npm list | npm delete --account A

外部设计文档（Outline）
  doc list                                  仓库与文档集（Outline Collection）的绑定
  doc bind REPO --base-url URL --collection ID [--name NAME]
                                            绑定仓库到文档集，Outline API token 从标准输入读取（加密保存）
  doc unbind REPO                           删除绑定

待决策清单（Outline，单向同步 AI 开发的决策疑难点）
  doc decision-doc set REPO --doc-id UUID [--base-url URL]
                                            设置待决策清单文档，独立受限的写 token 从标准输入读取
  doc decision-doc clear REPO               删除待决策清单配置（未写入的条目取消，已发请求的转为 unknown）
  doc decision-doc list                     列出配置、篇幅告警和待处理 / 结果不明的条目数量
  doc decision-sync list [--repo REPO] [--state STATE] [--limit N]
                                            查看决策同步记录
  doc decision-sync reconcile ID            对 unknown / attempted 的条目做一次只读查证（绝不重写文档）
  doc decision-sync resolve ID --state delivered|cancelled --note REASON
                                            人工结案（只影响通知投递记录，不改变 kiln:hold 门禁）

GitHub 请求
  outbox [--state dead|pending|done]        查看待发送 / 失败的 GitHub 请求
  outbox retry ID

本地
  lint [文件...]                            检查 .kiln/workflows/*.yml（不给文件时也检查 .kiln/merge.yml）
  runtime lock                              根据 workflow 和 go.mod、package.json 等生成 .kiln/runtimes.lock
                                            （monorepo 中可在 .kiln/runtimes.toml 的 [declarations] 指定声明文件）

环境变量 KILN_SERVER、KILN_ADMIN_TOKEN 可以覆盖保存的配置。
`

// 退出码：0 成功，1 运行错误，2 用法错误，130 被信号中断。
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// usageError 表示命令行用法错误，退出码为 2。msg 为空表示说明已经输出过。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// command 是一个顶层命令，args 不含命令名本身。
type command func(ctx context.Context, args []string) error

// app 持有一次命令执行的输入输出和 CLI 配置位置。
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	configPath     string
	// server 是 client() 加载配置后得到的 Server 地址，用于输出链接和命令提示。
	server string
}

func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	return &app{stdin: stdin, stdout: stdout, stderr: stderr, configPath: defaultConfigPath()}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run 执行 kiln 并返回退出码；所有清理都在这里通过 defer 完成。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := newApp(stdin, stdout, stderr)
	return a.exitCode(ctx, a.execute(ctx, args))
}

func (a *app) commands() map[string]command {
	version := func(context.Context, []string) error {
		fmt.Fprintln(a.stdout, "kiln", buildinfo.Version())
		return nil
	}
	help := func(context.Context, []string) error {
		fmt.Fprint(a.stdout, usage)
		return nil
	}
	return map[string]command{
		"login":       a.login,
		"admin":       a.admin,
		"runner":      a.runner,
		"repo":        a.repo,
		"cache":       a.cache,
		"runs":        a.runs,
		"run":         a.showRun,
		"logs":        a.logs,
		"cancel":      a.cancel,
		"rerun":       a.rerun,
		"rerun-job":   a.rerunJob,
		"dispatch":    a.dispatch,
		"ai":          a.ai,
		"merge-queue": a.mergeQueue,
		"autotag":     a.autotag,
		"secret":      a.secret,
		"var":         a.variable,
		"env":         a.environment,
		"npm":         a.npmCmd,
		"doc":         a.doc,
		"outbox":      a.outbox,
		"lint":        a.lint,
		"runtime":     a.runtime,
		"version":     version,
		"--version":   version,
		"-v":          version,
		"help":        help,
		"--help":      help,
		"-h":          help,
	}
}

func (a *app) execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return &usageError{}
	}
	cmd, ok := a.commands()[args[0]]
	if !ok {
		return usageErrorf("未知的命令 %s\n运行 kiln help 查看用法", args[0])
	}
	return cmd(ctx, args[1:])
}

// exitCode 输出错误并把它映射为退出码。
func (a *app) exitCode(ctx context.Context, err error) int {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case ctx.Err() != nil:
		fmt.Fprintln(a.stderr, "已中断")
		return exitInterrupted
	}
	if ue, ok := errors.AsType[*usageError](err); ok {
		if ue.msg != "" {
			fmt.Fprintf(a.stderr, "错误：%s\n", ue.msg)
		}
		return exitUsage
	}
	fmt.Fprintf(a.stderr, "错误：%s\n", formatError(err))
	return exitError
}

// formatError 把 Server 返回的 connect 错误显示为“消息（错误码）”，并保留外层添加的上下文。
func formatError(err error) string {
	ce, ok := errors.AsType[*connect.Error](err)
	if !ok {
		return err.Error()
	}
	msg := ce.Message()
	if msg == "" {
		msg = "请求失败"
	}
	detail := fmt.Sprintf("%s（%s）", msg, ce.Code())
	if prefix, found := strings.CutSuffix(err.Error(), ce.Error()); found {
		return prefix + detail
	}
	return detail
}

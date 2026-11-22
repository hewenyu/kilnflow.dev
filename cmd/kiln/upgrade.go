package main

import (
	"context"
	"fmt"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1/apiv1connect"
)

// runnerUpgradeUsage 是 runner upgrade 子命令的用法。
const runnerUpgradeUsage = `用法：kiln runner upgrade                       registry 和每台 Runner 的版本、最新版本、升级请求
       kiln runner upgrade start RUNNER [VERSION] 升级到这台 Runner 查到的最新版本（离线时上线后执行）；
                                                 写 VERSION 时它必须仍是最新版本，否则不升级
       kiln runner upgrade RUNNER                同 start（名称与子命令相同时必须用 start）
       kiln runner upgrade cancel RUNNER         取消进行中的升级
       kiln runner upgrade check [RUNNER]        让 Runner 立即查询最新版本（不写名称表示全部）
       kiln runner upgrade registry URL|default  Runner 查询和下载升级包用的 npm registry（default 为官方）`

// requestStateText 是升级请求状态的说明。
var requestStateText = map[string]string{
	"pending":   "进行中",
	"succeeded": "已升级",
	"failed":    "失败",
	"cancelled": "已取消",
}

// runnerUpgrade 是 Runner 按台升级（PLAN #27.14）。
func (a *app) runnerUpgrade(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	var call func(apiv1connect.AdminServiceClient) error
	switch {
	case sub == "":
		call = a.showRunnerUpgrade(ctx)
	case sub == "start" && (len(rest) == 1 || len(rest) == 2):
		version := ""
		if len(rest) == 2 {
			version = rest[1]
		}
		call = a.requestRunnerUpgrade(ctx, rest[0], version)
	case sub == "cancel" && len(rest) == 1:
		call = func(cl apiv1connect.AdminServiceClient) error {
			resp, err := cl.CancelRunnerUpgrade(ctx, req(&apiv1.CancelRunnerUpgradeRequest{Runner: rest[0]}))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "已取消 %s 升级到 %s\n", rest[0], resp.Msg.GetRequest().GetVersion())
			return nil
		}
	case sub == "check" && len(rest) <= 1:
		runner := ""
		if len(rest) == 1 {
			runner = rest[0]
		}
		call = func(cl apiv1connect.AdminServiceClient) error {
			resp, err := cl.CheckRunnerUpgrade(ctx, req(&apiv1.CheckRunnerUpgradeRequest{Runner: runner}))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "已请求 %d 台 Runner 查询最新版本（下一次连接 Server 时查询）\n", resp.Msg.Count)
			return nil
		}
	case sub == "registry" && len(rest) == 1:
		registry := rest[0]
		if registry == "default" {
			registry = ""
		}
		call = func(cl apiv1connect.AdminServiceClient) error {
			if _, err := cl.SetRunnerUpgradeRegistry(ctx, req(&apiv1.SetRunnerUpgradeRegistryRequest{Registry: registry})); err != nil {
				return err
			}
			if registry == "" {
				fmt.Fprintln(a.stdout, "Runner 使用官方 registry")
			} else {
				fmt.Fprintf(a.stdout, "Runner 使用 registry %s\n", registry)
			}
			return nil
		}
	case len(args) == 1 && !upgradeSubcommands[sub]:
		call = a.requestRunnerUpgrade(ctx, sub, "")
	default:
		return usageErrorf(runnerUpgradeUsage)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	return call(cl)
}

// upgradeSubcommands 是 runner upgrade 的子命令：与它们同名的 Runner 只能用 start RUNNER 升级。
var upgradeSubcommands = map[string]bool{"start": true, "cancel": true, "check": true, "registry": true}

// requestRunnerUpgrade 请求把 runner 升级到它查到的最新版本；version 不为空时它必须仍是最新版本。
func (a *app) requestRunnerUpgrade(ctx context.Context, runner, version string) func(apiv1connect.AdminServiceClient) error {
	return func(cl apiv1connect.AdminServiceClient) error {
		resp, err := cl.RequestRunnerUpgrade(ctx, req(&apiv1.RequestRunnerUpgradeRequest{Runner: runner, Version: version}))
		if err != nil {
			return err
		}
		r := resp.Msg.GetRequest()
		if resp.Msg.Created {
			fmt.Fprintf(a.stdout, "%s 将升级到 %s：下载核对后等正在执行的任务结束再切换，新版本确认后完成（离线时上线后执行）\n", runner, r.GetVersion())
		} else {
			fmt.Fprintf(a.stdout, "%s 已经在升级到 %s\n", runner, r.GetVersion())
		}
		return nil
	}
}

// upgradeRequestText 是一台 Runner 最近一次升级请求的说明（没有时为 -）。
func upgradeRequestText(u *apiv1.RunnerUpgradeInfo) string {
	r := u.GetRequest()
	if r == nil {
		return "-"
	}
	s := requestStateText[r.State] + " " + r.Version
	switch {
	case r.State == "pending" && u.GetState() != "":
		s += "（Runner：" + u.GetState() + "）"
	case r.Error != "":
		s += "：" + truncate(r.Error, 80)
	}
	return s
}

// upgradeRow 返回升级一览中的一行：名称、类型、版本、最新版本、升级请求、说明。
func upgradeRow(name, kind, version string, u *apiv1.RunnerUpgradeInfo) []string {
	latest := dashText(u.GetLatest())
	if u.GetCheckedAt() != 0 {
		latest += "（" + ago(u.GetCheckedAt()) + " 前）"
	}
	note := ""
	switch {
	case u.GetCanUpgrade():
		note = "可以升级：kiln runner upgrade start " + name + " " + u.GetLatest()
	case u.GetReason() != "":
		note = u.GetReason()
	}
	if e := u.GetCheckError(); e != "" && u.GetLatest() != "" {
		note += "；最后一次查询失败：" + truncate(e, 60)
	}
	return []string{name, kind, dashText(version), latest, upgradeRequestText(u), dashText(note)}
}

func (a *app) showRunnerUpgrade(ctx context.Context) func(apiv1connect.AdminServiceClient) error {
	return func(cl apiv1connect.AdminServiceClient) error {
		resp, err := cl.GetRunnerUpgrade(ctx, req(&apiv1.GetRunnerUpgradeRequest{}))
		if err != nil {
			return err
		}
		if m := resp.Msg; m.Registry == "" {
			fmt.Fprintf(a.stdout, "registry：官方（%s）\n", m.DefaultRegistry)
		} else {
			fmt.Fprintf(a.stdout, "registry：%s\n", m.Registry)
		}
		// 按台升级只针对 CI Runner：AI 执行节点的连接器用 npm 升级，Server 不参与（PLAN #33.3）
		runners, err := cl.ListRunners(ctx, req(&apiv1.ListRunnersRequest{}))
		if err != nil {
			return err
		}
		if len(runners.Msg.Runners) == 0 {
			return nil
		}
		fmt.Fprintln(a.stdout)
		rows := make([][]string, 0, len(runners.Msg.Runners))
		for _, r := range runners.Msg.Runners {
			rows = append(rows, upgradeRow(r.Name, kindText(runnerKindCI), r.Version, r.GetUpgrade()))
		}
		a.table("名称\t类型\t版本\t最新版本\t升级\t说明", rows)
		return nil
	}
}

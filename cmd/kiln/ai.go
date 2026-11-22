package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// AI 的命令（PLAN #30.9）：AI 请求、任务和 AI 执行节点与 CI 的 run、job、Runner 分开。
//   - ai review REPO [--ref REF]：手动触发 AI 审计；
//   - ai requests / ai show / ai cancel / ai rerun / ai log：AI 请求和任务；
//   - ai runners：AI 执行节点；
//   - ai dev ...：AI 开发（aidev.go）。

// aiUsage 是 ai 的子命令。
const aiUsage = "ai review、ai requests、ai show、ai cancel、ai rerun、ai log、ai runners、ai dev"

// aiTypeText 返回 AI 请求类型的显示名称。
func aiTypeText(t string) string {
	if t == agentcfg.TaskDevelop {
		return "开发"
	}
	return "审计"
}

func (a *app) ai(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "dev":
		return a.aiDev(ctx, rest)
	case "review":
		return a.aiReview(ctx, rest)
	case "requests":
		return a.aiRequests(ctx, rest)
	case "show":
		return a.aiShow(ctx, rest)
	case "cancel":
		return a.aiCancel(ctx, rest)
	case "rerun":
		return a.aiRerun(ctx, rest)
	case "log":
		return a.aiLog(ctx, rest)
	case "runners":
		return a.aiRunners(ctx, rest)
	}
	return usageErrorf("未知的子命令 ai %s（可用：%s）", sub, aiUsage)
}

func (a *app) aiReview(ctx context.Context, args []string) error {
	fs := newFlags("ai review")
	ref := fs.String("ref", "", "分支、tag 或 commit sha，默认为默认分支")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "ai review REPO [--ref REF]"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.TriggerAIReview(ctx, req(&apiv1.TriggerAIReviewRequest{Repo: pos[0], Ref: *ref}))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已创建 AI 审计请求 #%d（commit %s）\n", resp.Msg.AiRequestId, resp.Msg.Sha)
	return nil
}

// aiRequests 列出最近的 AI 请求。
func (a *app) aiRequests(ctx context.Context, args []string) error {
	fs := newFlags("ai requests")
	repo := fs.String("repo", "", "仓库")
	typ := fs.String("type", "", "review 或 develop")
	pr := int32Flag(fs, "pr", 0, "PR 编号")
	state := fs.String("state", "", "queued、active、succeeded、failed、cancelled 或 skipped")
	n := int32Flag(fs, "n", 30, "数量")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListAIRequests(ctx, req(&apiv1.ListAIRequestsRequest{Repo: *repo, Type: *typ, PrNumber: *pr, State: *state, Limit: *n}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Requests))
	for _, r := range resp.Msg.Requests {
		rows = append(rows, []string{
			"#" + strconv.FormatInt(r.Id, 10), aiTypeText(r.Type), r.State, r.Repo, prText(r.PrNumber), r.Ref, shorten.Hash(r.Sha), rerunOfText(r.RerunOf), ago(r.CreatedAt),
		})
	}
	a.table("请求\t类型\t状态\t仓库\tPR\tref\tcommit\t重跑\t创建", rows)
	return nil
}

func prText(n int32) string {
	if n == 0 {
		return "-"
	}
	return "#" + strconv.Itoa(int(n))
}

func rerunOfText(id int64) string {
	if id == 0 {
		return "-"
	}
	return "#" + strconv.FormatInt(id, 10)
}

// aiShow 输出 AI 请求的详情和各平台任务。
func (a *app) aiShow(ctx context.Context, args []string) error {
	if err := need(args, 1, "ai show ID"); err != nil {
		return err
	}
	id, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.GetAIRequest(ctx, req(&apiv1.GetAIRequestRequest{Id: id}))
	if err != nil {
		return err
	}
	r := resp.Msg.Request
	fmt.Fprintf(a.stdout, "AI %s请求 #%d  %s  %s  %s %s@%s  %s/ai/requests/%d\n",
		aiTypeText(r.Type), r.Id, r.State, r.Repo, r.Event, r.Ref, shorten.Hash(r.Sha), a.server, r.Id)
	if r.PrNumber != 0 {
		fmt.Fprintf(a.stdout, "PR #%d\n", r.PrNumber)
	}
	if r.RerunOf != 0 {
		fmt.Fprintf(a.stdout, "重跑自 #%d\n", r.RerunOf)
	}
	if r.ConfigError != "" {
		fmt.Fprintln(a.stdout, "\n配置错误：\n"+r.ConfigError)
	}
	rows := make([][]string, 0, len(resp.Msg.Tasks))
	for _, t := range resp.Msg.Tasks {
		st := t.State
		if t.ReusedFrom != 0 {
			st += fmt.Sprintf("（沿用 #%d）", t.ReusedFrom)
		}
		rows = append(rows, []string{
			strconv.FormatInt(t.Id, 10), t.DisplayName, st, taskPlatform(t), t.Agent, t.Runner, progressText(t), durText(t.StartedAt, t.FinishedAt),
			truncate(t.Reason, 60),
		})
	}
	fmt.Fprintln(a.stdout)
	a.table("任务\t名称\t状态\t平台\tAgent\t节点\t进度\t耗时\t说明", rows)
	// 在 Orca 中执行的任务：给出 run / dispatch / worktree，排查时到 Orca 里找这次执行（PLAN #33.4）
	for _, t := range resp.Msg.Tasks {
		if o := t.GetOrca(); o != nil && (o.GetRunId() != "" || o.GetDispatchId() != "") {
			fmt.Fprintf(a.stdout, "\n任务 %d 在 Orca 中：run %s，dispatch %s，worktree %s\n", t.Id, dashText(o.GetRunId()), dashText(o.GetDispatchId()),
				dashText(o.GetWorktreeName()))
		}
	}
	return nil
}

// durText 返回从 start 到 end（为 0 时到现在）的耗时；没有开始时为空。
func durText(start, end int64) string {
	if start == 0 {
		return ""
	}
	if end == 0 {
		end = time.Now().Unix()
	}
	return (time.Duration(end-start) * time.Second).String()
}

func (a *app) aiCancel(ctx context.Context, args []string) error {
	if err := need(args, 1, "ai cancel ID"); err != nil {
		return err
	}
	id, err := parseID(args[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.CancelAIRequest(ctx, req(&apiv1.CancelAIRequestRequest{Id: id})); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已请求取消 AI 请求 #%d\n", id)
	return nil
}

// aiRerun 重跑已结束的 AI 审计请求：默认只重跑没有成功的任务；--mode all 重跑全部；--task 只重跑一个任务。
func (a *app) aiRerun(ctx context.Context, args []string) error {
	const use = "ai rerun ID [--mode failed|all] [--task TASK_ID]"
	fs := newFlags("ai rerun")
	mode := fs.String("mode", "", "failed（默认）| all")
	task := fs.Int64("task", 0, "只重跑这个任务")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, use); err != nil {
		return err
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	r := &apiv1.RerunAIRequestRequest{Id: id, Mode: *mode}
	if *task != 0 {
		if *mode != "" {
			return usageErrorf("--task 与 --mode 不能同时使用")
		}
		r.Mode, r.TaskId = "task", *task
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.RerunAIRequest(ctx, req(r))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "已创建 AI 审计请求 #%d\n", resp.Msg.Id)
	return nil
}

// aiLog 输出 AI 任务的日志；-f 时持续输出直到结束或被中断。
func (a *app) aiLog(ctx context.Context, args []string) error {
	fs := newFlags("ai log")
	follow := fs.Bool("f", false, "持续输出直到结束")
	attempt := fs.Int64("attempt", 0, "attempt ID，默认最新")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "ai log TASK_ID [-f] [--attempt ID]"); err != nil {
		return err
	}
	taskID, err := parseID(pos[0])
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	var off int64
	for {
		resp, err := cl.GetAITaskLog(ctx, req(&apiv1.GetAITaskLogRequest{TaskId: taskID, AttemptId: *attempt, Offset: off}))
		if err != nil {
			return err
		}
		if _, err := a.stdout.Write(resp.Msg.Data); err != nil {
			return err
		}
		off = resp.Msg.NextOffset
		if len(resp.Msg.Data) > 0 {
			continue
		}
		if !*follow || resp.Msg.Finished {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// taskPlatform 返回任务的平台：已经领取的是实际执行的平台，还没有领取的是可以执行的平台（一组平台时用 | 分隔）。
func taskPlatform(t *apiv1.AITask) string {
	if t.Platform != "" {
		return t.Platform
	}
	return strings.Join(t.Platforms, "|")
}

// progressText 返回任务的进度（连接器心跳上报的阶段和说明）。
func progressText(t *apiv1.AITask) string {
	p := strings.TrimSpace(t.GetStage() + " " + t.GetText())
	return truncate(dashText(p), 40)
}

// aiRoleText 返回 AI 执行节点角色的显示名称。
func aiRoleText(role string) string {
	if role == runnerRoleDevelopment {
		return "开发"
	}
	return "审计"
}

// aiRunners 列出 AI 执行节点：角色、状态、平台、协议、Orca Server / Docker、Agent、运行中的任务和并发上限。
func (a *app) aiRunners(ctx context.Context, args []string) error {
	if _, err := a.parse(newFlags("ai runners"), args); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListAIRunners(ctx, req(&apiv1.ListAIRunnersRequest{}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Runners))
	for _, r := range resp.Msg.Runners {
		agents := make([]string, 0, len(r.Agents))
		for _, ag := range r.Agents {
			agents = append(agents, ag.Agent+":"+ag.State)
		}
		proto := strconv.Itoa(int(r.Protocol))
		if r.NeedsUpgrade {
			proto += "（需升级）"
		}
		rows = append(rows, []string{
			r.Name, aiRoleText(r.Role), r.State, r.Platform, proto, r.OrcaState, r.DockerState, strings.Join(agents, ","),
			strconv.Itoa(int(r.Running)) + "/" + strconv.Itoa(int(r.Capacity)), r.Version, ago(r.LastSeenAt),
		})
	}
	a.table("名称\t角色\t状态\t平台\t协议\tOrca\tDocker\tAgent\t运行中\t版本\t最后在线", rows)
	return nil
}

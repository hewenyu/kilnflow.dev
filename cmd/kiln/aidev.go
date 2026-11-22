package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// aiDevUsage 是 ai dev 子命令的用法。
const aiDevUsage = "用法：kiln ai dev sync REPO | pause REPO | resume REPO | list [REPO] [-n 50] | stop ID | retry ID"

// aiDev 管理 AI 开发（PLAN #26.11、#26.15）：从 GitHub 全量同步任务池、暂停 / 恢复新的领取、列出、终止、重试工作项。
func (a *app) aiDev(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("ai dev " + sub)
	n := int32Flag(fs, "n", 50, "最多列出的工作项数")
	pos, err := a.parse(fs, rest)
	if err != nil {
		return err
	}
	var id int64
	switch {
	case (sub == "sync" || sub == "pause" || sub == "resume") && len(pos) == 1:
	case sub == "list" && len(pos) <= 1:
	case (sub == "stop" || sub == "retry") && len(pos) == 1:
		if id, err = strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64); err != nil || id <= 0 {
			return usageErrorf("工作项编号 %q 无效\n%s", pos[0], aiDevUsage)
		}
	default:
		return usageErrorf(aiDevUsage)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	switch sub {
	case "sync":
		resp, err := cl.SyncAIDev(ctx, req(&apiv1.SyncAIDevRequest{Repo: pos[0]}))
		if err != nil {
			return err
		}
		m := resp.Msg
		fmt.Fprintf(a.stdout, "已同步任务池：GitHub 上有 %d 个 kiln:ready 的 Issue，变化 %d 个，可选 %d 个（有空闲的开发机时开始选题）\n",
			m.Listed, m.Changed, m.Candidates)
		if !m.Complete {
			fmt.Fprintln(a.stdout, "Issue 太多没有读完：没读到的不会移出任务池")
		}
	case "pause", "resume":
		paused := sub == "pause"
		if _, err := cl.PauseAIDev(ctx, req(&apiv1.PauseAIDevRequest{Repo: pos[0], Paused: paused})); err != nil {
			return err
		}
		if paused {
			fmt.Fprintf(a.stdout, "已暂停 %s 的 AI 开发：不再领取新的 Issue，已有的工作项照常收尾\n", pos[0])
		} else {
			fmt.Fprintf(a.stdout, "已恢复 %s 的 AI 开发\n", pos[0])
		}
	case "stop":
		if _, err := cl.StopWorkItem(ctx, req(&apiv1.StopWorkItemRequest{Id: id})); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "已终止工作项 #%d（执行中的 job 已取消，Issue 已释放；PR 保持原样）\n", id)
	case "retry":
		if _, err := cl.RetryWorkItem(ctx, req(&apiv1.RetryWorkItemRequest{Id: id})); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "工作项 #%d 将继续：本轮执行过任务时再返修一轮，部分完成后开始下一轮实现\n", id)
	default:
		repo := ""
		if len(pos) == 1 {
			repo = pos[0]
		}
		resp, err := cl.ListWorkItems(ctx, req(&apiv1.ListWorkItemsRequest{Repo: repo, Limit: *n}))
		if err != nil {
			return err
		}
		if o := resp.Msg.Occupancy; o != nil {
			// 同一个仓库按工作项串行（PLAN #26.9 I14）：谁占着仓库、在等什么
			if o.WorkItem == 0 {
				fmt.Fprintf(a.stdout, "仓库占用：没有（可以开始下一个工作项）\n\n")
			} else {
				fmt.Fprintf(a.stdout, "仓库占用：%s\n\n", o.Reason)
			}
		}
		if len(resp.Msg.Items) == 0 {
			fmt.Fprintln(a.stdout, "没有工作项")
			return nil
		}
		rows := make([][]string, 0, len(resp.Msg.Items))
		for _, w := range resp.Msg.Items {
			rows = append(rows, workItemRow(w))
		}
		a.table("工作项\t仓库\t状态\tIssue\tPR\t返修\t说明\t更新", rows)
	}
	return nil
}

// workItemRow 返回工作项列表的一行。
func workItemRow(w *apiv1.WorkItem) []string {
	issues := make([]string, 0, len(w.Issues))
	for _, n := range w.Issues {
		issues = append(issues, "#"+strconv.Itoa(int(n)))
	}
	pr := "-"
	if w.PrNumber > 0 {
		pr = "#" + strconv.Itoa(int(w.PrNumber)) + " " + shorten.Hash(w.HeadSha)
	}
	var notes []string
	if w.Hold {
		notes = append(notes, "kiln:hold")
	}
	if w.RequirementsChanged {
		notes = append(notes, "需求改过")
	}
	if w.Reason != "" {
		notes = append(notes, w.Reason)
	}
	rounds := fmt.Sprintf("%d/%d", w.RoundFixes, w.MaxFixRounds)
	if w.RoundConflicts > 0 {
		rounds += fmt.Sprintf("（冲突 %d/%d）", w.RoundConflicts, w.MaxFixRounds)
	}
	return []string{
		"#" + strconv.FormatInt(w.Id, 10), w.Repo, workItemStage(w), strings.Join(issues, " "), pr,
		rounds, truncate(strings.Join(notes, "；"), 80), ago(w.UpdatedAt),
	}
}

// workItemStage 返回工作项的方式、阶段和状态（PLAN #31.6）：一个 PR 完成的只显示状态；先设计的显示"设计"或"实现"；
// 部分完成之后的轮次显示轮次。
func workItemStage(w *apiv1.WorkItem) string {
	stage := w.State
	switch {
	case w.Phase == "design":
		stage = "设计 " + stage
	case w.DesignFirst:
		stage = "实现 " + stage
	}
	if w.ImplRound > 1 {
		stage += fmt.Sprintf("（第 %d 轮）", w.ImplRound)
	}
	return stage
}

package main

import (
	"context"
	"fmt"
	"strconv"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// mergeQueueUsage 是 merge-queue 子命令的用法。
const mergeQueueUsage = "用法：kiln merge-queue list REPO | add REPO PR | remove REPO PR"

// mergeQueueMerged 是合并队列中已合并的项的状态（与 Server 的 merge_queue.state 相同）。
const mergeQueueMerged = "merged"

// mergeQueue 管理合并队列（PLAN #28）：list、add、remove。add / remove 等同于给 PR 加上、去掉 label kiln:merge。
func (a *app) mergeQueue(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	pos, err := a.parse(newFlags("merge-queue "+sub), rest)
	if err != nil {
		return err
	}
	var pr int32
	switch {
	case sub == "list" && len(pos) == 1:
	case (sub == "add" || sub == "remove") && len(pos) == 2:
		// PR 号在协议中是 int32：超出范围的直接拒绝，不截断成另一个 PR
		n, err := strconv.ParseInt(pos[1], 10, 32)
		if err != nil || n <= 0 {
			return usageErrorf("PR 号 %q 无效\n%s", pos[1], mergeQueueUsage)
		}
		pr = int32(n)
	default:
		return usageErrorf(mergeQueueUsage)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	repo := pos[0]
	switch sub {
	case "add":
		resp, err := cl.AddToMergeQueue(ctx, req(&apiv1.AddToMergeQueueRequest{Repo: repo, Pr: pr}))
		if err != nil {
			return err
		}
		if resp.Msg.Added {
			fmt.Fprintf(a.stdout, "已把 %s#%d 加入合并队列\n", repo, pr)
		} else {
			fmt.Fprintf(a.stdout, "%s#%d 已经在合并队列中\n", repo, pr)
		}
	case "remove":
		if _, err := cl.RemoveFromMergeQueue(ctx, req(&apiv1.RemoveFromMergeQueueRequest{Repo: repo, Pr: pr})); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "已去掉 %s#%d 的 kiln:merge，合并队列稍后将它移出\n", repo, pr)
	default:
		resp, err := cl.ListMergeQueue(ctx, req(&apiv1.ListMergeQueueRequest{Repo: repo}))
		if err != nil {
			return err
		}
		if len(resp.Msg.Queued) == 0 {
			fmt.Fprintln(a.stdout, "队列为空")
		} else {
			rows := make([][]string, 0, len(resp.Msg.Queued))
			// 每条目标分支各自排队（PLAN #28.5）：排位按同一目标分支计算，与状态文案"目标 dev · 排在第 2 个"一致
			pos := make(map[string]int, len(resp.Msg.Queued))
			for _, e := range resp.Msg.Queued {
				pos[e.BaseRef]++
				rows = append(rows, []string{
					strconv.Itoa(pos[e.BaseRef]), "#" + strconv.Itoa(int(e.Pr)), dashText(e.BaseRef), shorten.Hash(e.HeadSha),
					e.Status, e.EnqueuedBy, ago(e.EnqueuedAt),
				})
			}
			a.table("顺序\tPR\t目标分支\thead\t状态\t入队者\t入队时间", rows)
		}
		if len(resp.Msg.Recent) > 0 {
			fmt.Fprintln(a.stdout, "\n最近结束：")
			rows := make([][]string, 0, len(resp.Msg.Recent))
			for _, e := range resp.Msg.Recent {
				result := "已合并 " + shorten.Hash(e.MergeSha)
				if e.State != mergeQueueMerged {
					result = "已移出"
				}
				rows = append(rows, []string{"#" + strconv.Itoa(int(e.Pr)), dashText(e.BaseRef), result, e.Reason, ago(e.FinishedAt)})
			}
			a.table("PR\t目标分支\t结果\t原因\t时间", rows)
		}
	}
	return nil
}

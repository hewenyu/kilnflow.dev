package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// autotagUsage 是 autotag 子命令的用法。
const autotagUsage = "用法：kiln autotag list REPO [-n 50] | retry REPO SHA"

// autotag 查看和重试自动打 tag 的记录（PLAN #29.7）。
func (a *app) autotag(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	fs := newFlags("autotag " + sub)
	n := int32Flag(fs, "n", 50, "最多显示的条数")
	pos, err := a.parse(fs, rest)
	if err != nil {
		return err
	}
	switch {
	case sub == "list" && len(pos) == 1:
	case sub == "retry" && len(pos) == 2:
	default:
		return usageErrorf(autotagUsage)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	repo := pos[0]
	if sub == "retry" {
		if _, err := cl.RetryAutoTag(ctx, req(&apiv1.RetryAutoTagRequest{Repo: repo, Sha: pos[1]})); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "已重新排队：%s %s\n", repo, shorten.Hash(pos[1]))
		return nil
	}
	resp, err := cl.ListAutoTags(ctx, req(&apiv1.ListAutoTagsRequest{Repo: repo, Limit: *n}))
	if err != nil {
		return err
	}
	if len(resp.Msg.Tags) == 0 {
		fmt.Fprintln(a.stdout, "没有记录（经过 PR 合入的 commit 才会记录；.kiln/merge.yml 中有 tag: 且合入发版分支时才打 tag）")
		return nil
	}
	rows := make([][]string, 0, len(resp.Msg.Tags))
	for _, t := range resp.Msg.Tags {
		rows = append(rows, []string{
			shorten.Hash(t.Sha), "#" + strconv.Itoa(int(t.Pr)), dashText(t.BaseRef), workItemText(t.WorkItemId), dashText(t.Tag), t.State,
			senderText(t.MergedBy, t.SenderIsBot), strings.Join(t.Labels, ","), t.Reason, ago(t.UpdatedAt),
		})
	}
	a.table("commit\tPR\t目标分支\t工作项\ttag\t状态\t合并者\tlabel\t说明\t更新时间", rows)
	return nil
}

// workItemText 返回自动打 tag 记录关联的 AI 开发工作项（没有可信归属时为 -）。
func workItemText(id int64) string {
	if id <= 0 {
		return "-"
	}
	return "工作项 #" + strconv.FormatInt(id, 10)
}

// senderText 返回真实的发布发起者；机器人时加上标识。
func senderText(login string, isBot bool) string {
	switch {
	case login == "":
		return "-"
	case isBot:
		return login + "（机器人）"
	}
	return login
}

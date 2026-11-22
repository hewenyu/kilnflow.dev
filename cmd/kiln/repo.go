package main

import (
	"context"
	"fmt"
	"strconv"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

// repo 列出仓库、刷新仓库目录或对一个仓库补对账。
func (a *app) repo(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "list", "":
		cl, err := a.client()
		if err != nil {
			return err
		}
		resp, err := cl.ListRepos(ctx, req(&apiv1.ListReposRequest{}))
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(resp.Msg.Repos))
		for _, r := range resp.Msg.Repos {
			rows = append(rows, []string{
				r.FullName, r.DefaultBranch, boolText(r.Private, "private", "public"),
				strconv.FormatInt(r.InstallationId, 10), boolText(r.Active, "active", "inactive"),
			})
		}
		a.table("仓库\t默认分支\t可见性\tinstallation\t状态", rows)
	case "sync":
		cl, err := a.client()
		if err != nil {
			return err
		}
		resp, err := cl.SyncInstallations(ctx, req(&apiv1.SyncInstallationsRequest{}))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "已刷新仓库：%d 个 installation，%d 个启用中的仓库\n", resp.Msg.Installations, resp.Msg.Repos)
	case "reconcile":
		return a.repoReconcile(ctx, rest)
	default:
		return usageErrorf("未知的子命令 repo %s", sub)
	}
	return nil
}

// repoReconcile 对一个仓库补对账（PLAN #7.3）。
func (a *app) repoReconcile(ctx context.Context, args []string) error {
	if err := need(args, 1, "repo reconcile owner/name"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ReconcileRepo(ctx, req(&apiv1.ReconcileRepoRequest{Repo: args[0]}))
	if err != nil {
		return err
	}
	m := resp.Msg
	fmt.Fprintf(a.stdout, "%s：%d 个分支、%d 个 tag、%d 个打开的 PR，补上 %d 个 run\n", m.Repo, m.Branches, m.Tags, m.Pulls, m.Runs)
	if m.Baseline {
		fmt.Fprintln(a.stdout, "这是第一次对账：之前没有记录过的 ref 只记录为基线，不构建")
	}
	return nil
}

// cache 管理仓库缓存：目前只有 bump。
func (a *app) cache(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	if sub != "bump" || len(rest) < 1 {
		return usageErrorf("用法：kiln cache bump REPO")
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	if _, err := cl.BumpCacheSalt(ctx, req(&apiv1.BumpCacheSaltRequest{Repo: rest[0]})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已更换", rest[0], "的缓存盐，之后的 job 不再命中旧缓存")
	return nil
}

// outbox 查看或重试待发送的 GitHub 请求。
func (a *app) outbox(ctx context.Context, args []string) error {
	if sub, rest := splitSub(args); sub == "retry" {
		return a.outboxRetry(ctx, rest)
	}
	fs := newFlags("outbox")
	state := fs.String("state", "", "pending | dead | done")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListOutbox(ctx, req(&apiv1.ListOutboxRequest{State: *state}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Items))
	for _, it := range resp.Msg.Items {
		rows = append(rows, []string{
			strconv.FormatInt(it.Id, 10), it.State, it.DedupeKey, strconv.Itoa(int(it.Attempts)),
			ago(it.UpdatedAt), truncate(it.LastError, 80),
		})
	}
	a.table("ID\t状态\tkey\t次数\t更新\t错误", rows)
	return nil
}

func (a *app) outboxRetry(ctx context.Context, args []string) error {
	if err := need(args, 1, "outbox retry ID"); err != nil {
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
	if _, err := cl.RetryOutbox(ctx, req(&apiv1.RetryOutboxRequest{Id: id})); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "已重新排队")
	return nil
}

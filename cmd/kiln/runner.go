package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
)

// runner 管理 Runner：token、list、drain、undrain、revoke。
func (a *app) runner(ctx context.Context, args []string) error {
	sub, rest := splitSub(args)
	switch sub {
	case "token":
		return a.runnerToken(ctx, rest)
	case "list", "":
		return a.runnerList(ctx)
	case "cache":
		return a.runnerCache(ctx, rest)
	case "upgrade":
		return a.runnerUpgrade(ctx, rest)
	case "drain", "undrain":
		if err := need(rest, 1, "runner "+sub+" NAME"); err != nil {
			return err
		}
		cl, err := a.client()
		if err != nil {
			return err
		}
		if _, err := cl.DrainRunner(ctx, req(&apiv1.DrainRunnerRequest{Name: rest[0], Drain: sub == "drain"})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已完成")
	case "revoke":
		if err := need(rest, 1, "runner revoke NAME"); err != nil {
			return err
		}
		cl, err := a.client()
		if err != nil {
			return err
		}
		if _, err := cl.RevokeRunner(ctx, req(&apiv1.RevokeRunnerRequest{Name: rest[0]})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已吊销", rest[0])
	case "delete":
		if err := need(rest, 1, "runner delete NAME"); err != nil {
			return err
		}
		cl, err := a.client()
		if err != nil {
			return err
		}
		if _, err := cl.DeleteRunner(ctx, req(&apiv1.DeleteRunnerRequest{Name: rest[0]})); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "已删除", rest[0])
	default:
		return usageErrorf("未知的子命令 runner %s", sub)
	}
	return nil
}

// Runner 的类型（与 Server 的 runners.kind 相同）。
const (
	runnerKindCI = "ci"
	runnerKindAI = "ai"
)

// AI 执行节点的角色（与 Server 的 ai_runners.role 相同，PLAN #26.7）。
const (
	runnerRoleReview      = "review"
	runnerRoleDevelopment = "development"
)

func (a *app) runnerToken(ctx context.Context, args []string) error {
	fs := newFlags("runner token")
	pool := fs.String("pool", "", "pool，默认 default")
	ttl := int32Flag(fs, "ttl", 60, "有效期（分钟）")
	kind := fs.String("kind", runnerKindCI, "Runner 类型：ci（kiln-runner）或 ai（执行机器上的 kiln-orca 连接器）")
	role := fs.String("role", "", "AI 执行节点的角色：review（审计，默认）或 development（开发）；注册后不能修改")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	switch {
	case *role != "" && *kind != runnerKindAI:
		return usageErrorf("--role 只用于 --kind ai")
	case *role != "" && *role != runnerRoleReview && *role != runnerRoleDevelopment:
		return usageErrorf("--role 只能是 review 或 development：%s", *role)
	}
	switch *kind {
	case runnerKindCI, runnerKindAI:
	default:
		return usageErrorf("--kind 只能是 ci 或 ai：%s", *kind)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	r := &apiv1.CreateEnrollTokenRequest{Pool: *pool, TtlMinutes: *ttl, Role: *role}
	if *kind != runnerKindCI {
		r.Kind = *kind
	}
	resp, err := cl.CreateEnrollToken(ctx, req(r))
	if err != nil {
		return err
	}
	if *kind == runnerKindAI {
		// AI 执行节点的连接器是 npm 包 kiln-orca（PLAN #33.3）：注册和安装服务用它自己的命令
		fmt.Fprintf(a.stdout, "%s\n\n在执行机器上执行（%s 前有效，只能使用一次）：\n  kiln-orca enroll --server %s --token %s\n"+
			"  kiln-orca service install\n", resp.Msg.Token, expiryText(time.Unix(resp.Msg.ExpiresAt, 0), time.Now()), a.server, resp.Msg.Token)
		return nil
	}
	fmt.Fprintf(a.stdout, "%s\n\n在 Runner 机器上执行（%s 前有效，只能使用一次）：\n  kiln-runner enroll --server %s --token %s\n"+
		"  kiln-runner service install\n", resp.Msg.Token, expiryText(time.Unix(resp.Msg.ExpiresAt, 0), time.Now()), a.server, resp.Msg.Token)
	return nil
}

// expiryText 返回到期时间的写法：与 now 同一天时只写时刻（15:04），否则带上日期（--ttl 超过一天时只写时刻会被误读为今天）。
func expiryText(exp, now time.Time) string {
	if y, m, d := exp.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return exp.Format("15:04")
	}
	return exp.Format("2006-01-02 15:04")
}

func (a *app) runnerList(ctx context.Context) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.ListRunners(ctx, req(&apiv1.ListRunnersRequest{}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.Msg.Runners))
	for _, r := range resp.Msg.Runners {
		spec, alloc := "-", "-"
		if c := r.Capacity; c != nil {
			spec = resources(c.Machine)
			alloc = resources(c.Allocated) + " / " + resources(c.Allocatable)
		}
		cache := "-"
		if r.CacheReportedAt != 0 {
			cache = bytesize.Format(r.CacheBytes)
		}
		rows = append(rows, []string{
			r.Name, r.State, r.Pool, strings.Join(r.Platforms, ","), strings.Join(r.Executors, ","), spec, alloc,
			strconv.Itoa(int(r.RunningJobs)), cache, r.Version, ago(r.LastSeenAt),
		})
	}
	a.table("名称\t状态\tpool\t平台\texecutor\t规格\t已分配 / 可分配\t运行中\t缓存\t版本\t最后在线", rows)
	return nil
}

// kindText 返回 Runner 类型的显示名称：ci 为 CI，ai 为 AI。
func kindText(k string) string {
	if k == runnerKindAI {
		return "AI"
	}
	return "CI"
}

// resources 返回"2 核 / 1.9 GiB"这样的写法；nil 为 "-"。
func resources(r *apiv1.ResourceAmount) string {
	if r == nil {
		return "-"
	}
	cpu := strconv.FormatFloat(r.Cpu, 'f', -1, 64)
	if r.Cpu != math.Trunc(r.Cpu) {
		cpu = strconv.FormatFloat(r.Cpu, 'f', 2, 64)
	}
	return cpu + " 核 / " + bytesize.Format(r.MemoryBytes)
}

// purgeStateDone 是清理请求（runner_cache_purges.state）已由 Runner 执行完毕的状态。
const purgeStateDone = "done"

// runnerCache 显示 Runner 的缓存清单（kiln runner cache NAME），或清理（kiln runner cache purge NAME ...）。
func (a *app) runnerCache(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "purge" {
		return a.runnerCachePurge(ctx, args[1:])
	}
	pos, err := a.parse(newFlags("runner cache"), args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "runner cache NAME"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.GetRunnerCache(ctx, req(&apiv1.GetRunnerCacheRequest{Name: pos[0]}))
	if err != nil {
		return err
	}
	m := resp.Msg
	if m.GeneratedAt == 0 {
		fmt.Fprintf(a.stdout, "%s 还没有上报缓存清单（需要新版 kiln-runner）\n", pos[0])
	} else {
		a.cacheTable(m.Entries)
		fmt.Fprintf(a.stdout, "\n清单时间 %s", time.Unix(m.GeneratedAt, 0).Format(time.DateTime))
		if m.DiskTotalBytes > 0 {
			fmt.Fprintf(a.stdout, "；磁盘 %s，可用 %s", bytesize.Format(m.DiskTotalBytes), bytesize.Format(m.DiskFreeBytes))
		}
		fmt.Fprintln(a.stdout)
	}
	if m.GcAt != 0 {
		fmt.Fprintf(a.stdout, "上一次清理 %s：删除 %d 个对象（%s），失败 %d 个\n", time.Unix(m.GcAt, 0).Format(time.DateTime),
			m.GcRemoved, bytesize.Format(m.GcRemovedBytes), len(m.GcFailures))
		for _, f := range m.GcFailures {
			fmt.Fprintf(a.stdout, "  失败 [%s] %s：%s\n", f.Kind, f.Path, f.Error)
		}
	}
	if len(m.Purges) > 0 {
		fmt.Fprintln(a.stdout, "\n清理请求：")
		rows := make([][]string, 0, len(m.Purges))
		for _, p := range m.Purges {
			result := "等待 Runner 空闲后执行"
			if p.State == purgeStateDone {
				result = fmt.Sprintf("删除 %d 个（%s），失败 %d 个", p.Removed, bytesize.Format(p.RemovedBytes), len(p.Failures))
			}
			rows = append(rows, []string{
				"#" + strconv.FormatInt(p.Id, 10), dashText(p.Repo), dashText(strings.Join(p.Kinds, ",")), dashText(p.Domain),
				p.Actor, ago(p.CreatedAt), result,
			})
		}
		a.table("ID\t仓库\t类别\t执行身份\t发起人\t时间\t结果", rows)
	}
	return nil
}

// cacheTable 输出缓存清单。
func (a *app) cacheTable(entries []*apiv1.RunnerCacheEntry) {
	rows := make([][]string, 0, len(entries))
	var total int64
	for _, e := range entries {
		repo := e.Repo
		if repo == "" && e.RepoId != 0 {
			repo = "#" + strconv.FormatInt(e.RepoId, 10)
		}
		exp := "-"
		if e.ExpiresAt != 0 {
			exp = time.Unix(e.ExpiresAt, 0).Format(time.DateTime)
		}
		rows = append(rows, []string{e.Kind, dashText(repo), dashText(e.Platform), dashText(e.Domain), e.Name, bytesize.Format(e.SizeBytes), ago(e.LastUsedAt), exp})
		total += e.SizeBytes
	}
	a.table("类别\t仓库\t平台\t执行身份\t名称\t大小\t最后使用\t到期", rows)
	fmt.Fprintf(a.stdout, "共 %d 项，%s\n", len(entries), bytesize.Format(total))
}

func dashText(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runnerCachePurge 预览并发起缓存清理；--dry-run 时只预览。
func (a *app) runnerCachePurge(ctx context.Context, args []string) error {
	fs := newFlags("runner cache purge")
	repo := fs.String("repo", "", "只清理这个仓库（owner/name）的 toolcache、git 镜像和工作区")
	var kinds multi
	fs.Var(&kinds, "kind", "类别（toolcache、runtime、git、cas、buildkit、work、image、legacy），可重复；默认全部")
	domain := fs.String("domain", "", "toolcache 的执行身份（host、oci-root…）")
	dry := fs.Bool("dry-run", false, "只预览，不清理")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "runner cache purge NAME [--repo R] [--kind K ...] [--domain D] [--dry-run]"); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	resp, err := cl.PurgeRunnerCache(ctx, req(&apiv1.PurgeRunnerCacheRequest{Name: pos[0], Repo: *repo, Kinds: kinds, Domain: *domain, DryRun: *dry}))
	if err != nil {
		return err
	}
	m := resp.Msg
	fmt.Fprintf(a.stdout, "范围内的缓存（%s）：\n", bytesize.Format(m.MatchedBytes))
	a.cacheTable(m.Matched)
	if len(m.RetainedShared) > 0 {
		fmt.Fprintln(a.stdout, "\n继续保留的共享缓存：")
		a.cacheTable(m.RetainedShared)
	}
	for _, n := range m.Notes {
		fmt.Fprintln(a.stdout, "说明：", n)
	}
	if m.PurgeId != 0 {
		fmt.Fprintf(a.stdout, "已发起清理请求 #%d；用 kiln runner cache %s 查看结果\n", m.PurgeId, pos[0])
	} else {
		fmt.Fprintln(a.stdout, "只是预览，没有清理（去掉 --dry-run 执行）")
	}
	return nil
}

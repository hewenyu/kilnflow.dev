package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// lines 把输出拆成行，并把每行的连续空白压缩为一个空格，便于断言表格内容而不依赖列宽。
func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return out
}

// wantLines 检查输出（压缩空白后）逐行等于 want。
func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	got := lines(out)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("输出：\n%s\n期望：\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestServerCommands 覆盖调用 Server 的命令：请求内容（含 flag 与位置参数混排）和输出。
func TestServerCommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		stdin   string
		setup   func(f *fakeAdmin)
		method  string        // 期望调用的方法
		request proto.Message // 期望的请求
		stdout  []string      // 期望的输出（压缩空白后逐行比较）；nil 表示不检查
		// contains 是输出中应包含的片段（空白压缩为一个空格后比较）。
		contains []string
	}{
		{
			name: "runs 默认 30 条", args: []string{"runs"},
			setup: func(f *fakeAdmin) {
				f.reply("ListRuns", &apiv1.ListRunsResponse{Runs: []*apiv1.Run{
					{Id: 12, State: "success", Repo: "o/r", Workflow: "ci", Event: "push", Ref: "refs/heads/main", Sha: testSHA},
					{Id: 11, State: "failure", Repo: "o/r", Workflow: "release", Event: "push", Ref: "refs/tags/v1", Sha: "abc"},
				}})
			},
			method: "ListRuns", request: &apiv1.ListRunsRequest{Limit: 30},
			stdout: []string{
				"RUN 状态 仓库 workflow 事件 ref commit 创建",
				"#12 success o/r ci push refs/heads/main 0123456789ab -",
				"#11 failure o/r release push refs/tags/v1 abc -",
			},
		},
		{
			name: "runs 带仓库和数量", args: []string{"runs", "-n", "5", "--repo", "o/r"},
			method: "ListRuns", request: &apiv1.ListRunsRequest{Repo: "o/r", Limit: 5},
			stdout: []string{"RUN 状态 仓库 workflow 事件 ref commit 创建"},
		},
		{
			name: "run 详情", args: []string{"run", "#12"},
			setup: func(f *fakeAdmin) {
				f.reply("GetRun", &apiv1.GetRunResponse{
					Run: &apiv1.Run{Id: 12, State: "failure", Repo: "o/r", Workflow: "ci", Event: "push", Ref: "refs/heads/main", Sha: testSHA, Rerun: 2},
					Jobs: []*apiv1.Job{
						{Id: 101, DisplayName: "build", State: "success", Platform: "linux/arm64", Runner: "mini", StartedAt: 1000, FinishedAt: 1090},
						{Id: 102, DisplayName: "test", State: "success", Platform: "linux/arm64", CachedFromJobId: 55},
						{Id: 103, DisplayName: "lint", State: "failure", Platform: "linux/arm64", Reason: "第一行\n" + strings.Repeat("长", 70)},
					},
				})
			},
			method: "GetRun", request: &apiv1.GetRunRequest{Id: 12},
			stdout: []string{
				"run #12 failure o/r ci push refs/heads/main@0123456789ab rerun 2 {URL}/runs/12",
				"",
				"JOB 名称 状态 平台 Runner 耗时 说明",
				"101 build success linux/arm64 mini 1m30s",
				"102 test success（缓存 #55） linux/arm64",
				"103 lint failure linux/arm64 第一行 " + strings.Repeat("长", 56) + "…",
			},
		},
		{
			name: "run 有配置错误", args: []string{"run", "3"},
			setup: func(f *fakeAdmin) {
				f.reply("GetRun", &apiv1.GetRunResponse{Run: &apiv1.Run{Id: 3, State: "failure", Event: "push", Error: "ci.yml:3:1: 错误：不支持 uses"}})
			},
			method: "GetRun", request: &apiv1.GetRunRequest{Id: 3},
			stdout: []string{
				"run #3 failure push @ rerun 0 {URL}/runs/3",
				"",
				"配置错误：",
				"ci.yml:3:1: 错误：不支持 uses",
				"",
				"JOB 名称 状态 平台 Runner 耗时 说明",
			},
		},
		{
			name: "cancel", args: []string{"cancel", "#9"},
			method: "CancelRun", request: &apiv1.CancelRunRequest{Id: 9},
			stdout: []string{"已请求取消"},
		},
		{
			name: "rerun 默认只重跑失败的 job", args: []string{"rerun", "9"},
			setup:  func(f *fakeAdmin) { f.reply("RerunRun", &apiv1.RerunRunResponse{Rerun: 3}) },
			method: "RerunRun", request: &apiv1.RerunRunRequest{Id: 9, Mode: "failed"},
			stdout: []string{"已创建 rerun 3"},
		},
		{
			name: "rerun flag 在位置参数前", args: []string{"rerun", "--mode", "all_no_cache", "9"},
			method: "RerunRun", request: &apiv1.RerunRunRequest{Id: 9, Mode: "all_no_cache"},
		},
		{
			name: "rerun flag 在位置参数后", args: []string{"rerun", "9", "--mode=all"},
			method: "RerunRun", request: &apiv1.RerunRunRequest{Id: 9, Mode: "all"},
		},
		{
			name: "rerun-job", args: []string{"rerun-job", "101"},
			setup:  func(f *fakeAdmin) { f.reply("RerunJob", &apiv1.RerunJobResponse{Rerun: 4}) },
			method: "RerunJob", request: &apiv1.RerunJobRequest{JobId: 101},
			stdout: []string{"已创建 rerun 4"},
		},
		{
			name: "dispatch 默认分支", args: []string{"dispatch", "o/r", "ci.yml"},
			setup:  func(f *fakeAdmin) { f.reply("Dispatch", &apiv1.DispatchResponse{RunId: 77}) },
			method: "Dispatch", request: &apiv1.DispatchRequest{Repo: "o/r", Workflow: "ci.yml", Inputs: map[string]string{}},
			stdout: []string{"已创建 run #77"},
		},
		{
			name: "dispatch flag 与位置参数混排，值中可以有等号", args: []string{"dispatch", "-i", "a=1", "o/r", "--ref", "v1.2.0", "release.yml", "-i", "b=x=y", "-i", "empty="},
			method: "Dispatch", request: &apiv1.DispatchRequest{Repo: "o/r", Workflow: "release.yml", Ref: "v1.2.0", Inputs: map[string]string{"a": "1", "b": "x=y", "empty": ""}},
		},
		{
			name: "runner token", args: []string{"runner", "token", "--pool", "mac", "--ttl", "10"},
			setup: func(f *fakeAdmin) {
				f.reply("CreateEnrollToken", &apiv1.CreateEnrollTokenResponse{Token: "kiln-enroll-abc", ExpiresAt: 0})
			},
			method: "CreateEnrollToken", request: &apiv1.CreateEnrollTokenRequest{Pool: "mac", TtlMinutes: 10},
		},
		{
			name: "runner token --kind ai", args: []string{"runner", "token", "--kind", "ai"},
			method: "CreateEnrollToken", request: &apiv1.CreateEnrollTokenRequest{TtlMinutes: 60, Kind: "ai"},
		},
		{
			name: "runner token --role development", args: []string{"runner", "token", "--kind", "ai", "--role", "development"},
			method: "CreateEnrollToken", request: &apiv1.CreateEnrollTokenRequest{TtlMinutes: 60, Kind: "ai", Role: "development"},
		},
		{
			name: "ai review", args: []string{"ai", "review", "o/r", "--ref", "feat"},
			setup: func(f *fakeAdmin) {
				f.reply("TriggerAIReview", &apiv1.TriggerAIReviewResponse{AiRequestId: 12, Sha: "abc"})
			},
			method: "TriggerAIReview", request: &apiv1.TriggerAIReviewRequest{Repo: "o/r", Ref: "feat"},
			stdout: []string{"已创建 AI 审计请求 #12（commit abc）"},
		},
		{
			name: "ai requests 带筛选", args: []string{"ai", "requests", "--repo", "o/r", "--type", "review", "--pr", "7", "--state", "failed", "-n", "5"},
			setup: func(f *fakeAdmin) {
				f.reply("ListAIRequests", &apiv1.ListAIRequestsResponse{Requests: []*apiv1.AIRequest{
					{Id: 9, Type: "review", State: "failed", Repo: "o/r", PrNumber: 7, Ref: "refs/pull/7/head", Sha: testSHA, RerunOf: 8},
					{Id: 3, Type: "develop", State: "succeeded", Repo: "o/r", Ref: "refs/heads/main", Sha: "abc"},
				}})
			},
			method: "ListAIRequests", request: &apiv1.ListAIRequestsRequest{Repo: "o/r", Type: "review", PrNumber: 7, State: "failed", Limit: 5},
			stdout: []string{
				"请求 类型 状态 仓库 PR ref commit 重跑 创建",
				"#9 审计 failed o/r #7 refs/pull/7/head 0123456789ab #8 -",
				"#3 开发 succeeded o/r - refs/heads/main abc - -",
			},
		},
		{
			name: "ai requests 默认 30 条", args: []string{"ai", "requests"},
			method: "ListAIRequests", request: &apiv1.ListAIRequestsRequest{Limit: 30},
		},
		{
			name: "ai show", args: []string{"ai", "show", "#9"},
			setup: func(f *fakeAdmin) {
				f.reply("GetAIRequest", &apiv1.GetAIRequestResponse{
					Request: &apiv1.AIRequest{Id: 9, Type: "review", State: "failed", Repo: "o/r", Event: "pull_request", Ref: "refs/pull/7/head", Sha: testSHA, PrNumber: 7, RerunOf: 8},
					Tasks: []*apiv1.AITask{
						{
							Id: 31, DisplayName: "codex · macos-arm64", State: "failed", Platform: "darwin/arm64", Agent: "codex", Runner: "mini",
							Reason: "AI 审计发现 1 个问题", Stage: "running", Text: "审计中",
							Orca: &apiv1.AIOrca{RunId: "run-1", DispatchId: "dispatch-1", WorktreeName: "kiln-a31-e1"},
						},
						{Id: 32, DisplayName: "codex · linux-amd64", State: "succeeded", Platform: "linux/amd64", Agent: "codex", ReusedFrom: 20},
						// 一组平台、还没有领取：显示可以执行的平台
						{Id: 33, DisplayName: "codex · any", State: "pending", Platforms: []string{"darwin/arm64", "linux/amd64"}, Agent: "codex"},
					},
				})
			},
			method: "GetAIRequest", request: &apiv1.GetAIRequestRequest{Id: 9},
			contains: []string{
				"AI 审计请求 #9 failed o/r pull_request refs/pull/7/head@0123456", "/ai/requests/9", "PR #7", "重跑自 #8",
				"31 codex · macos-arm64 failed darwin/arm64 codex mini running 审计中 AI 审计发现 1 个问题", "succeeded（沿用 #20）",
				"33 codex · any pending darwin/arm64|linux/amd64 codex",
				"任务 31 在 Orca 中：run run-1，dispatch dispatch-1，worktree kiln-a31-e1",
			},
		},
		{
			name: "ai cancel", args: []string{"ai", "cancel", "9"},
			method: "CancelAIRequest", request: &apiv1.CancelAIRequestRequest{Id: 9},
			stdout: []string{"已请求取消 AI 请求 #9"},
		},
		{
			name: "ai rerun 默认只重跑没有成功的", args: []string{"ai", "rerun", "9"},
			setup:  func(f *fakeAdmin) { f.reply("RerunAIRequest", &apiv1.RerunAIRequestResponse{Id: 10}) },
			method: "RerunAIRequest", request: &apiv1.RerunAIRequestRequest{Id: 9},
			stdout: []string{"已创建 AI 审计请求 #10"},
		},
		{
			name: "ai rerun --mode all", args: []string{"ai", "rerun", "--mode", "all", "9"},
			method: "RerunAIRequest", request: &apiv1.RerunAIRequestRequest{Id: 9, Mode: "all"},
		},
		{
			name: "ai rerun --task", args: []string{"ai", "rerun", "9", "--task", "31"},
			method: "RerunAIRequest", request: &apiv1.RerunAIRequestRequest{Id: 9, Mode: "task", TaskId: 31},
		},
		{
			name: "ai runners", args: []string{"ai", "runners"},
			setup: func(f *fakeAdmin) {
				f.reply("ListAIRunners", &apiv1.ListAIRunnersResponse{Runners: []*apiv1.AIRunner{
					{
						Name: "mac", Role: "review", State: "active", Platform: "darwin/arm64", Protocol: 5, OrcaState: "ready", DockerState: "unavailable",
						Agents:  []*apiv1.AIRunnerAgent{{Agent: "codex", State: "available"}, {Agent: "claude-code", State: "backoff"}},
						Running: 1, Capacity: 2, Version: "v0.3.0",
					},
					{
						Name: "dev", Role: "development", State: "offline", Platform: "darwin/arm64", Protocol: 4, NeedsUpgrade: true,
						OrcaState: "unknown", DockerState: "unknown", Version: "v0.2.0",
					},
				}})
			},
			method: "ListAIRunners", request: &apiv1.ListAIRunnersRequest{},
			stdout: []string{
				"名称 角色 状态 平台 协议 Orca Docker Agent 运行中 版本 最后在线",
				"mac 审计 active darwin/arm64 5 ready unavailable codex:available,claude-code:backoff 1/2 v0.3.0 -",
				"dev 开发 offline darwin/arm64 4（需升级） unknown unknown 0/0 v0.2.0 -",
			},
		},
		{
			name: "runner token 默认 60 分钟", args: []string{"runner", "token"},
			method: "CreateEnrollToken", request: &apiv1.CreateEnrollTokenRequest{TtlMinutes: 60},
		},
		{
			name: "runner list", args: []string{"runner", "list"},
			setup: func(f *fakeAdmin) {
				f.reply("ListRunners", &apiv1.ListRunnersResponse{Runners: []*apiv1.Runner{
					{Name: "mini", State: "online", Pool: "default", Platforms: []string{"darwin/arm64", "linux/arm64"}, Executors: []string{"host", "oci"}, RunningJobs: 2, Version: "v0.1.0"},
				}})
			},
			method: "ListRunners", request: &apiv1.ListRunnersRequest{},
			stdout: []string{
				"名称 状态 pool 平台 executor 规格 已分配 / 可分配 运行中 缓存 版本 最后在线",
				"mini online default darwin/arm64,linux/arm64 host,oci - - 2 - v0.1.0 -",
			},
		},
		{
			name: "runner list 带规格", args: []string{"runner", "list"},
			setup: func(f *fakeAdmin) {
				f.reply("ListRunners", &apiv1.ListRunnersResponse{Runners: []*apiv1.Runner{{
					Name: "amd", State: "online", Pool: "default", Platforms: []string{"linux/amd64"}, Executors: []string{"host"}, Version: "v0.2.0",
					Capacity: &apiv1.RunnerCapacity{
						Machine:     &apiv1.ResourceAmount{Cpu: 2, MemoryBytes: 2062835712},
						Allocatable: &apiv1.ResourceAmount{Cpu: 2, MemoryBytes: 2062835712},
						Allocated:   &apiv1.ResourceAmount{Cpu: 1, MemoryBytes: 1 << 30},
					},
					CacheBytes: 3 << 30, CacheReportedAt: 1,
				}}})
			},
			method: "ListRunners", request: &apiv1.ListRunnersRequest{},
			stdout: []string{
				"名称 状态 pool 平台 executor 规格 已分配 / 可分配 运行中 缓存 版本 最后在线",
				"amd online default linux/amd64 host 2 核 / 1.9 GiB 1 核 / 1.0 GiB / 2 核 / 1.9 GiB 0 3.0 GiB v0.2.0 -",
			},
		},
		{
			name: "runner cache", args: []string{"runner", "cache", "amd"},
			setup: func(f *fakeAdmin) {
				f.reply("GetRunnerCache", &apiv1.GetRunnerCacheResponse{
					GeneratedAt: 1, GcAt: 1, GcRemoved: 2, GcRemovedBytes: 2048,
					Entries:    []*apiv1.RunnerCacheEntry{{Kind: "toolcache", Repo: "o/r", Platform: "linux/amd64", Domain: "host", Name: "gomod", SizeBytes: 1 << 20, ExpiresAt: 0}},
					GcFailures: []*apiv1.RunnerCacheFailure{{Kind: "work", Path: "work/1", Error: "permission denied"}},
					Purges:     []*apiv1.RunnerCachePurge{{Id: 3, Repo: "o/r", State: "pending", Actor: "admin"}},
				})
			},
			method: "GetRunnerCache", request: &apiv1.GetRunnerCacheRequest{Name: "amd"},
			contains: []string{
				"toolcache o/r linux/amd64 host gomod 1.0 MiB", "共 1 项，1.0 MiB", "删除 2 个对象（2.0 KiB），失败 1 个",
				"失败 [work] work/1：permission denied", "#3 o/r - - admin", "等待 Runner 空闲后执行",
			},
		},
		{
			name: "runner cache purge 预览", args: []string{"runner", "cache", "purge", "amd", "--repo", "o/r", "--kind", "toolcache", "--kind", "git", "--domain", "oci-root", "--dry-run"},
			setup: func(f *fakeAdmin) {
				f.reply("PurgeRunnerCache", &apiv1.PurgeRunnerCacheResponse{
					MatchedBytes: 1 << 20, Matched: []*apiv1.RunnerCacheEntry{{Kind: "toolcache", Repo: "o/r", Name: "gomod", SizeBytes: 1 << 20}},
					Notes: []string{"共享缓存继续保留"},
				})
			},
			method:   "PurgeRunnerCache",
			request:  &apiv1.PurgeRunnerCacheRequest{Name: "amd", Repo: "o/r", Kinds: []string{"toolcache", "git"}, Domain: "oci-root", DryRun: true},
			contains: []string{"范围内的缓存（1.0 MiB）", "说明： 共享缓存继续保留", "只是预览"},
		},
		{
			name: "runner 默认 list", args: []string{"runner"},
			method: "ListRunners", request: &apiv1.ListRunnersRequest{},
		},
		{
			name: "runner drain", args: []string{"runner", "drain", "mini"},
			method: "DrainRunner", request: &apiv1.DrainRunnerRequest{Name: "mini", Drain: true},
			stdout: []string{"已完成"},
		},
		{
			name: "runner undrain", args: []string{"runner", "undrain", "mini"},
			method: "DrainRunner", request: &apiv1.DrainRunnerRequest{Name: "mini"},
			stdout: []string{"已完成"},
		},
		{
			name: "runner revoke", args: []string{"runner", "revoke", "mini"},
			method: "RevokeRunner", request: &apiv1.RevokeRunnerRequest{Name: "mini"},
			stdout: []string{"已吊销 mini"},
		},
		{
			name: "runner delete", args: []string{"runner", "delete", "mini"},
			method: "DeleteRunner", request: &apiv1.DeleteRunnerRequest{Name: "mini"},
			stdout: []string{"已删除 mini"},
		},
		{
			name: "repo list", args: []string{"repo", "list"},
			setup: func(f *fakeAdmin) {
				f.reply("ListRepos", &apiv1.ListReposResponse{Repos: []*apiv1.Repo{
					{FullName: "o/app", DefaultBranch: "main", Private: true, InstallationId: 42, Active: true},
					{FullName: "o/dl", DefaultBranch: "master", InstallationId: 42},
				}})
			},
			method: "ListRepos", request: &apiv1.ListReposRequest{},
			stdout: []string{
				"仓库 默认分支 可见性 installation 状态",
				"o/app main private 42 active",
				"o/dl master public 42 inactive",
			},
		},
		{
			name: "repo 默认 list", args: []string{"repo"},
			method: "ListRepos", request: &apiv1.ListReposRequest{},
		},
		{
			name: "repo sync", args: []string{"repo", "sync"},
			setup: func(f *fakeAdmin) {
				f.reply("SyncInstallations", &apiv1.SyncInstallationsResponse{Installations: 2, Repos: 5})
			},
			method: "SyncInstallations", request: &apiv1.SyncInstallationsRequest{},
			stdout: []string{"已刷新仓库：2 个 installation，5 个启用中的仓库"},
		},
		{
			name: "repo reconcile", args: []string{"repo", "reconcile", "o/app"},
			setup: func(f *fakeAdmin) {
				f.reply("ReconcileRepo", &apiv1.ReconcileRepoResponse{Repo: "o/app", Branches: 3, Tags: 2, Pulls: 1, Runs: 4})
			},
			method: "ReconcileRepo", request: &apiv1.ReconcileRepoRequest{Repo: "o/app"},
			stdout: []string{"o/app：3 个分支、2 个 tag、1 个打开的 PR，补上 4 个 run"},
		},
		{
			name: "repo reconcile 第一次只记录基线", args: []string{"repo", "reconcile", "o/app"},
			setup: func(f *fakeAdmin) {
				f.reply("ReconcileRepo", &apiv1.ReconcileRepoResponse{Repo: "o/app", Branches: 1, Baseline: true})
			},
			method: "ReconcileRepo", request: &apiv1.ReconcileRepoRequest{Repo: "o/app"},
			stdout: []string{
				"o/app：1 个分支、0 个 tag、0 个打开的 PR，补上 0 个 run",
				"这是第一次对账：之前没有记录过的 ref 只记录为基线，不构建",
			},
		},
		{
			name: "cache bump", args: []string{"cache", "bump", "o/app"},
			method: "BumpCacheSalt", request: &apiv1.BumpCacheSaltRequest{Repo: "o/app"},
			stdout: []string{"已更换 o/app 的缓存盐，之后的 job 不再命中旧缓存"},
		},
		{
			name: "outbox", args: []string{"outbox", "--state", "dead"},
			setup: func(f *fakeAdmin) {
				f.reply("ListOutbox", &apiv1.ListOutboxResponse{Items: []*apiv1.OutboxItem{
					{Id: 5, State: "dead", DedupeKey: "check:1", Attempts: 8, LastError: "502\nbad gateway"},
				}})
			},
			method: "ListOutbox", request: &apiv1.ListOutboxRequest{State: "dead"},
			stdout: []string{
				"ID 状态 key 次数 更新 错误",
				"5 dead check:1 8 - 502 bad gateway",
			},
		},
		{
			name: "admin 未绑定", args: []string{"admin"},
			setup:  func(f *fakeAdmin) { f.reply("GetAdminBinding", &apiv1.GetAdminBindingResponse{TokenLogin: true}) },
			method: "GetAdminBinding", request: &apiv1.GetAdminBindingRequest{},
			stdout: []string{"GitHub 账号：未绑定（在 Dashboard 的 设置 → 登录与身份 中绑定）", "Dashboard admin token 登录：开启（没有绑定时总是开启）"},
		},
		{
			name: "admin token-login on", args: []string{"admin", "token-login", "on"},
			method: "SetTokenLogin", request: &apiv1.SetTokenLoginRequest{Enabled: true},
			stdout: []string{"已开启 Dashboard 的 admin token 登录"},
		},
		{
			name: "admin unbind", args: []string{"admin", "unbind"},
			method: "UnbindAdmin", request: &apiv1.UnbindAdminRequest{},
			stdout: []string{"已解除绑定，Dashboard 的 admin token 登录已恢复"},
		},
		{
			name: "outbox retry", args: []string{"outbox", "retry", "#5"},
			method: "RetryOutbox", request: &apiv1.RetryOutboxRequest{Id: 5},
			stdout: []string{"已重新排队"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, url := loggedIn(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			got := runApp(t, cfg, tt.stdin, tt.args...)
			if got.code != exitOK {
				t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
			}
			wantRequest(t, f, tt.method, tt.request)
			if tt.stdout != nil {
				want := make([]string, len(tt.stdout))
				for i, l := range tt.stdout {
					want[i] = strings.ReplaceAll(l, "{URL}", url)
				}
				wantLines(t, got.stdout, want...)
			}
			out := strings.Join(strings.Fields(got.stdout), " ")
			for _, c := range tt.contains {
				if !strings.Contains(out, c) {
					t.Errorf("输出中没有 %q：\n%s", c, got.stdout)
				}
			}
		})
	}
}

// runner token 输出令牌和在 Runner 机器上执行的命令（带 Server 地址）。
func TestRunnerTokenOutput(t *testing.T) {
	cfg, f, url := loggedIn(t)
	f.reply("CreateEnrollToken", &apiv1.CreateEnrollTokenResponse{Token: "kiln-enroll-abc", ExpiresAt: 1})
	got := runApp(t, cfg, "", "runner", "token")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	for _, want := range []string{"kiln-enroll-abc\n", "kiln-runner enroll --server " + url + " --token kiln-enroll-abc", "kiln-runner service install", "只能使用一次"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout = %q，应包含 %q", got.stdout, want)
		}
	}
	// AI 执行节点的令牌提示连接器自己的命令（kiln-orca，PLAN #33.3）
	f.reply("CreateEnrollToken", &apiv1.CreateEnrollTokenResponse{Token: "kiln-enroll-ai", ExpiresAt: 1})
	got = runApp(t, cfg, "", "runner", "token", "--kind", "ai")
	if !strings.Contains(got.stdout, "kiln-orca enroll --server "+url+" --token kiln-enroll-ai") || !strings.Contains(got.stdout, "kiln-orca service install") {
		t.Errorf("AI 令牌的提示 = %q", got.stdout)
	}
}

// 令牌的到期时间：当天只写时刻；--ttl 超过一天时带上日期，修复前只写时刻，会被误读为当天到期。
func TestExpiryText(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	for _, c := range []struct {
		exp  time.Time
		want string
	}{
		{now.Add(time.Hour), "11:00"},
		{now.Add(14 * time.Hour), "2026-11-09 00:00"},
		{now.Add(72 * time.Hour), "2026-11-11 10:00"},
	} {
		if got := expiryText(c.exp, now); got != c.want {
			t.Errorf("expiryText(%v) = %q，期望 %q", c.exp, got, c.want)
		}
	}
}

// 正在运行的 job 的耗时按当前时间计算，不显示为负数或空。
func TestShowRunRunningJob(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	f.reply("GetRun", &apiv1.GetRunResponse{
		Run:  &apiv1.Run{Id: 1, State: "running"},
		Jobs: []*apiv1.Job{{Id: 2, DisplayName: "build", State: "running", StartedAt: 1}},
	})
	got := runApp(t, cfg, "", "run", "1")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	l := lines(got.stdout)
	if last := l[len(l)-1]; !strings.HasPrefix(last, "2 build running ") || !strings.HasSuffix(last, "s") || strings.Contains(last, "-") {
		t.Errorf("运行中的 job 行 = %q，应显示到现在的耗时", last)
	}
}

func TestShowRunWaitingJob(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	why := "没有 Runner 的容量满足 job 声明的 resources（需要 4 CPU / 8.0 GiB）：mini 容量 2 CPU / 1.9 GiB、空闲 2 CPU / 1.9 GiB。调小 resources 或增加更大的机器"
	f.reply("GetRun", &apiv1.GetRunResponse{
		Run:  &apiv1.Run{Id: 1, State: "active"},
		Jobs: []*apiv1.Job{{Id: 2, DisplayName: "build", State: "pending", WaitReason: why}},
	})
	got := runApp(t, cfg, "", "run", "1")
	if got.code != exitOK {
		t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "等待：没有 Runner 的容量满足 job 声明的 resources（需要 4 CPU / 8.0 GiB）") {
		t.Errorf("等待中的 job 应显示等待原因：\n%s", got.stdout)
	}
}

func TestLogs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		chunks   []*apiv1.GetJobLogResponse
		want     string
		requests []*apiv1.GetJobLogRequest
	}{
		{
			name: "按 offset 读到没有新数据为止",
			args: []string{"logs", "7"},
			chunks: []*apiv1.GetJobLogResponse{
				{Data: []byte("第一段\n"), NextOffset: 10},
				{Data: []byte("第二段\n"), NextOffset: 20},
				{NextOffset: 20},
			},
			want: "第一段\n第二段\n",
			requests: []*apiv1.GetJobLogRequest{
				{JobId: 7}, {JobId: 7, Offset: 10}, {JobId: 7, Offset: 20},
			},
		},
		{
			name:     "指定 attempt，flag 在位置参数后",
			args:     []string{"logs", "#7", "--attempt", "3"},
			chunks:   []*apiv1.GetJobLogResponse{{}},
			requests: []*apiv1.GetJobLogRequest{{JobId: 7, AttemptId: 3}},
		},
		{
			name: "-f 在 job 结束后退出",
			args: []string{"logs", "-f", "7"},
			chunks: []*apiv1.GetJobLogResponse{
				{Data: []byte("x"), NextOffset: 1},
				{NextOffset: 1, Finished: true},
			},
			want:     "x",
			requests: []*apiv1.GetJobLogRequest{{JobId: 7}, {JobId: 7, Offset: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			for _, c := range tt.chunks {
				f.reply("GetJobLog", c)
			}
			got := runApp(t, cfg, "", tt.args...)
			if got.code != exitOK {
				t.Fatalf("退出码 = %d，stderr = %q", got.code, got.stderr)
			}
			if got.stdout != tt.want {
				t.Errorf("stdout = %q，期望 %q", got.stdout, tt.want)
			}
			reqs := f.requests("GetJobLog")
			if len(reqs) != len(tt.requests) {
				t.Fatalf("GetJobLog 请求了 %d 次，期望 %d 次", len(reqs), len(tt.requests))
			}
			for i, want := range tt.requests {
				if !proto.Equal(reqs[i], want) {
					t.Errorf("第 %d 个请求 = %v，期望 %v", i, reqs[i], want)
				}
			}
		})
	}
}

// TestAILog：ai log 按 offset 读到没有新数据为止；-f 在任务结束后退出；--attempt 指定 attempt。
func TestAILog(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		chunks   []*apiv1.GetAITaskLogResponse
		want     string
		requests []*apiv1.GetAITaskLogRequest
	}{
		{
			name:     "按 offset 读到没有新数据为止",
			args:     []string{"ai", "log", "31", "--attempt", "40"},
			chunks:   []*apiv1.GetAITaskLogResponse{{Data: []byte("第一段\n"), NextOffset: 10}, {NextOffset: 10}},
			want:     "第一段\n",
			requests: []*apiv1.GetAITaskLogRequest{{TaskId: 31, AttemptId: 40}, {TaskId: 31, AttemptId: 40, Offset: 10}},
		},
		{
			name:     "-f 在任务结束后退出",
			args:     []string{"ai", "log", "-f", "31"},
			chunks:   []*apiv1.GetAITaskLogResponse{{Data: []byte("x"), NextOffset: 1}, {NextOffset: 1, Finished: true}},
			want:     "x",
			requests: []*apiv1.GetAITaskLogRequest{{TaskId: 31}, {TaskId: 31, Offset: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			for _, c := range tt.chunks {
				f.reply("GetAITaskLog", c)
			}
			got := runApp(t, cfg, "", tt.args...)
			if got.code != exitOK || got.stdout != tt.want {
				t.Fatalf("退出码 = %d，stdout = %q（期望 %q），stderr = %q", got.code, got.stdout, tt.want, got.stderr)
			}
			reqs := f.requests("GetAITaskLog")
			if len(reqs) != len(tt.requests) {
				t.Fatalf("GetAITaskLog 请求了 %d 次，期望 %d 次", len(reqs), len(tt.requests))
			}
			for i, want := range tt.requests {
				if !proto.Equal(reqs[i], want) {
					t.Errorf("第 %d 个请求 = %v，期望 %v", i, reqs[i], want)
				}
			}
		})
	}
	// --task 与 --mode 不能同时使用；未知的子命令
	cfg, _, _ := loggedIn(t)
	if got := runApp(t, cfg, "", "ai", "rerun", "9", "--task", "1", "--mode", "all"); got.code == exitOK || !strings.Contains(got.stderr, "不能同时使用") {
		t.Errorf("--task 与 --mode：%d %q", got.code, got.stderr)
	}
	if got := runApp(t, cfg, "", "ai", "bogus"); got.code == exitOK || !strings.Contains(got.stderr, "ai requests") {
		t.Errorf("未知的子命令：%d %q", got.code, got.stderr)
	}
}

// logs -f 等待新日志时被中断（Ctrl-C）：退出码 130。
func TestLogsFollowInterrupted(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// 第一次请求返回时就取消：命令在等待下一次轮询时应立即发现中断，而不是等满轮询间隔
	f.setOnCall(func(string) { cancel() })
	f.reply("GetJobLog", &apiv1.GetJobLogResponse{NextOffset: 0})

	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr)
	a.configPath = cfg
	code := a.exitCode(ctx, a.execute(ctx, []string{"logs", "-f", "7"}))
	if code != exitInterrupted || !strings.Contains(stderr.String(), "已中断") {
		t.Errorf("退出码 = %d，stderr = %q；期望 130 并提示已中断", code, stderr.String())
	}
}

// Server 返回的错误：退出码 1，显示消息和错误码。
func TestServerError(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		method string
		err    error
		want   string
	}{
		{name: "run 不存在", args: []string{"run", "99"}, method: "GetRun", err: connect.NewError(connect.CodeNotFound, errors.New("run 99 不存在")), want: "错误：run 99 不存在（not_found）"},
		{name: "cancel 已结束", args: []string{"cancel", "1"}, method: "CancelRun", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("run 已结束")), want: "run 已结束（failed_precondition）"},
		{name: "rerun 参数错误", args: []string{"rerun", "1", "--mode", "x"}, method: "RerunRun", err: connect.NewError(connect.CodeInvalidArgument, errors.New("未知的 mode")), want: "未知的 mode（invalid_argument）"},
		{name: "dispatch", args: []string{"dispatch", "o/r", "ci.yml"}, method: "Dispatch", err: connect.NewError(connect.CodeNotFound, errors.New("没有 workflow_dispatch")), want: "没有 workflow_dispatch（not_found）"},
		{name: "logs", args: []string{"logs", "1"}, method: "GetJobLog", err: connect.NewError(connect.CodeNotFound, errors.New("job 不存在")), want: "job 不存在（not_found）"},
		{name: "runs", args: []string{"runs"}, method: "ListRuns", err: connect.NewError(connect.CodeInternal, errors.New("数据库不可用")), want: "数据库不可用（internal）"},
		{name: "runner revoke", args: []string{"runner", "revoke", "x"}, method: "RevokeRunner", err: connect.NewError(connect.CodeNotFound, errors.New("没有 Runner x")), want: "没有 Runner x（not_found）"},
		{name: "repo sync", args: []string{"repo", "sync"}, method: "SyncInstallations", err: connect.NewError(connect.CodeUnavailable, errors.New("GitHub 限流")), want: "GitHub 限流（unavailable）"},
		{name: "secret list", args: []string{"secret", "list", "--repo", "o/r"}, method: "ListSecrets", err: connect.NewError(connect.CodeNotFound, errors.New("仓库不存在")), want: "仓库不存在（not_found）"},
		{name: "env set", args: []string{"env", "set", "prod", "--repo", "o/r"}, method: "UpsertEnvironment", err: connect.NewError(connect.CodeInvalidArgument, errors.New("ref 策略无效")), want: "ref 策略无效（invalid_argument）"},
		{name: "outbox retry", args: []string{"outbox", "retry", "1"}, method: "RetryOutbox", err: connect.NewError(connect.CodeNotFound, errors.New("不存在")), want: "不存在（not_found）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			f.fail(tt.method, tt.err)
			got := runApp(t, cfg, "", tt.args...)
			if got.code != exitError {
				t.Errorf("退出码 = %d，期望 %d", got.code, exitError)
			}
			if !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.want)
			}
			if got.stdout != "" {
				t.Errorf("失败时不应输出到 stdout：%q", got.stdout)
			}
		})
	}
}

// 保存的 token 已失效：退出码 1，提示 unauthenticated。
func TestStaleToken(t *testing.T) {
	cfg, f, _ := loggedIn(t)
	f.setToken("rotated-token-value")
	got := runApp(t, cfg, "", "runs")
	if got.code != exitError || !strings.Contains(got.stderr, "（unauthenticated）") {
		t.Errorf("退出码 = %d，stderr = %q；期望 1 且包含 unauthenticated", got.code, got.stderr)
	}
}

// 用法错误：退出码 2，在请求 Server 之前发现。
func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "run 缺少 ID", args: []string{"run"}, want: "用法：kiln run RUN_ID"},
		{name: "admin token-login 缺少 on/off", args: []string{"admin", "token-login"}, want: "用法：kiln admin token-login on|off"},
		{name: "admin 未知子命令", args: []string{"admin", "bind"}, want: "未知的子命令 admin bind"},
		{name: "cancel 缺少 ID", args: []string{"cancel"}, want: "用法：kiln cancel RUN_ID"},
		{name: "cancel 非数字 ID", args: []string{"cancel", "x1"}, want: "需要数字 ID：x1"},
		{name: "rerun 缺少 ID", args: []string{"rerun", "--mode", "all"}, want: "用法：kiln rerun RUN_ID"},
		{name: "rerun 非数字 ID", args: []string{"rerun", "abc"}, want: "需要数字 ID：abc"},
		{name: "rerun --mode 缺少值", args: []string{"rerun", "1", "--mode"}, want: "flag needs an argument"},
		{name: "rerun-job 缺少 ID", args: []string{"rerun-job"}, want: "用法：kiln rerun-job JOB_ID"},
		{name: "rerun-job 非数字 ID", args: []string{"rerun-job", "j"}, want: "需要数字 ID：j"},
		{name: "logs 缺少 ID", args: []string{"logs", "-f"}, want: "用法：kiln logs JOB_ID"},
		{name: "logs 非数字 ID", args: []string{"logs", "x"}, want: "需要数字 ID：x"},
		{name: "logs attempt 不是数字", args: []string{"logs", "1", "--attempt", "x"}, want: "kiln logs："},
		{name: "runs -n 不是数字", args: []string{"runs", "-n", "many"}, want: "需要整数"},
		{name: "runner token kind 无效", args: []string{"runner", "token", "--kind", "gpu"}, want: "--kind 只能是 ci 或 ai：gpu"},
		{name: "runner token CI 不能有角色", args: []string{"runner", "token", "--role", "review"}, want: "--role 只用于 --kind ai"},
		{name: "runner token 角色无效", args: []string{"runner", "token", "--kind", "ai", "--role", "admin"}, want: "--role 只能是 review 或 development：admin"},
		{name: "ai review 缺少仓库", args: []string{"ai", "review"}, want: "用法：kiln ai review REPO"},
		{name: "ai 未知子命令", args: []string{"ai", "fix"}, want: "未知的子命令 ai fix"},
		{name: "dispatch 缺少 workflow", args: []string{"dispatch", "o/r"}, want: "用法：kiln dispatch REPO WORKFLOW"},
		{name: "dispatch 输入没有等号", args: []string{"dispatch", "o/r", "ci.yml", "-i", "k"}, want: "输入应写成 key=value：k"},
		{name: "runner 未知子命令", args: []string{"runner", "bogus"}, want: "未知的子命令 runner bogus"},
		{name: "runner drain 缺少名称", args: []string{"runner", "drain"}, want: "用法：kiln runner drain NAME"},
		{name: "runner undrain 缺少名称", args: []string{"runner", "undrain"}, want: "用法：kiln runner undrain NAME"},
		{name: "runner revoke 缺少名称", args: []string{"runner", "revoke"}, want: "用法：kiln runner revoke NAME"},
		{name: "runner token ttl 越界", args: []string{"runner", "token", "--ttl", "-9999999999"}, want: "超出范围"},
		{name: "repo 未知子命令", args: []string{"repo", "rm"}, want: "未知的子命令 repo rm"},
		{name: "repo reconcile 缺少仓库", args: []string{"repo", "reconcile"}, want: "用法：kiln repo reconcile owner/name"},
		{name: "cache 未知子命令", args: []string{"cache", "clear", "o/r"}, want: "用法：kiln cache bump REPO"},
		{name: "cache bump 缺少仓库", args: []string{"cache", "bump"}, want: "用法：kiln cache bump REPO"},
		{name: "outbox 未知 state flag", args: []string{"outbox", "--status", "dead"}, want: "status"},
		{name: "outbox retry 缺少 ID", args: []string{"outbox", "retry"}, want: "用法：kiln outbox retry ID"},
		{name: "outbox retry 非数字", args: []string{"outbox", "retry", "x"}, want: "需要数字 ID：x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, f, _ := loggedIn(t)
			got := runApp(t, cfg, "", tt.args...)
			if got.code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d；stderr = %q", got.code, exitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %q，应包含 %q", got.stderr, tt.want)
			}
			wantNoCalls(t, f)
		})
	}
}

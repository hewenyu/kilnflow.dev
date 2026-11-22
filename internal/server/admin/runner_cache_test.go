package admin

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// setCacheReport 写入 Runner 上报的缓存清单。
func (e *testEnv) setCacheReport(runnerID int64, rep *runnerv1.CacheReport) {
	e.t.Helper()
	b, err := protojson.Marshal(rep)
	e.must(err)
	e.exec(`UPDATE ci_runners SET cache_report=$2, cache_reported_at=now() WHERE runner_id=$1`, runnerID, b)
}

// 列表中带上报的容量、正在执行的 job 声明的资源之和、缓存总大小。
func TestService_ListRunnersCapacity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := time.Now()
	id := e.addRunner("arm", runnerOpts{lastSeen: &now})
	e.addRunner("old", runnerOpts{lastSeen: &now})
	e.exec(`UPDATE ci_runners SET capacity=$2 WHERE runner_id=$1`, id, store.MustJSON(store.RunnerCapacity{
		Machine: store.Resources{CPU: 4, Memory: 24 << 30}, Reserved: store.Resources{CPU: 0, Memory: 2 << 30},
		Limits: map[string]store.Resources{"host": {CPU: 2, Memory: 6 << 30}}, MaxJobs: 2,
	}))
	run := e.addRun(runOpts{state: store.RunActive})
	j := e.addJob(jobOpts{runID: run, key: "a", state: store.JobRunning})
	e.exec(`UPDATE jobs SET spec='{"cpu":1.5,"memory_bytes":1073741824}' WHERE id=$1`, j)
	e.addAttempt(j, id, "")
	e.setCacheReport(id, &runnerv1.CacheReport{GeneratedAt: 1, Entries: []*runnerv1.CacheEntry{{Kind: "toolcache", SizeBytes: 3}, {Kind: "cas", SizeBytes: 4}}})

	list, err := e.svc.ListRunners(e.ctx)
	e.must(err)
	byName := map[string]*apiv1.Runner{}
	for _, r := range list {
		byName[r.Name] = r
	}
	c := byName["arm"].GetCapacity()
	if c.GetMachine().GetCpu() != 4 || c.GetAllocatable().GetMemoryBytes() != 22<<30 || c.GetAllocated().GetCpu() != 1.5 ||
		c.GetAllocated().GetMemoryBytes() != 1<<30 || c.GetExecutorLimits()["host"].GetMemoryBytes() != 6<<30 || c.GetMaxJobs() != 2 {
		t.Errorf("容量 = %v", c)
	}
	if byName["arm"].CacheBytes != 7 || byName["arm"].CacheReportedAt == 0 {
		t.Errorf("缓存 = %d，%d", byName["arm"].CacheBytes, byName["arm"].CacheReportedAt)
	}
	if byName["old"].Capacity != nil {
		t.Error("旧版 Runner 没有上报容量")
	}
}

// 清单：仓库 ID 换成仓库名；按仓库预览只包含这个仓库的项，并列出继续保留的共享缓存；发起清理写入请求和审计日志。
func TestService_RunnerCachePurge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seedRepo()
	e.addRepo(502, "other")
	now := time.Now()
	id := e.addRunner("arm", runnerOpts{lastSeen: &now})
	e.setCacheReport(id, &runnerv1.CacheReport{
		GeneratedAt: now.Unix(), DiskTotalBytes: 100, DiskFreeBytes: 40, GcAt: 5, GcRemoved: 2,
		GcFailures: []*runnerv1.CacheFailure{{Kind: "toolcache", Path: "toolcache/oci-root/501/linux-arm64/gocache", Error: "permission denied"}},
		Entries: []*runnerv1.CacheEntry{
			{Kind: "toolcache", RepoId: testRepoID, Domain: "host", Name: "gomod", SizeBytes: 10},
			{Kind: "toolcache", RepoId: testRepoID, Domain: "oci-root", Name: "gomod", SizeBytes: 20},
			{Kind: "git", RepoId: testRepoID, Name: "501.git", SizeBytes: 5},
			{Kind: "toolcache", RepoId: 502, Domain: "host", Name: "gomod", SizeBytes: 7},
			{Kind: "runtime", Name: "abcd", SizeBytes: 100, ExpiresAt: now.Add(time.Hour).Unix()},
		},
	})

	got, err := e.svc.GetRunnerCache(e.ctx, "arm")
	e.must(err)
	if len(got.Entries) != 5 || got.Entries[0].Repo != testAccount+"/demo" || got.DiskFreeBytes != 40 || got.GcRemoved != 2 || len(got.GcFailures) != 1 {
		t.Fatalf("GetRunnerCache = %v", got)
	}

	prev, err := e.svc.PurgeRunnerCache(e.ctx, &apiv1.PurgeRunnerCacheRequest{Name: "arm", Repo: "tester/demo", DryRun: true}, "admin")
	e.must(err)
	if prev.MatchedBytes != 35 || len(prev.Matched) != 3 || len(prev.RetainedShared) != 1 || prev.RetainedShared[0].Kind != "runtime" || prev.PurgeId != 0 {
		t.Fatalf("预览 = %v", prev)
	}
	if n := e.count(`SELECT count(*) FROM runner_cache_purges`); n != 0 {
		t.Fatalf("预览不应写入请求：%d", n)
	}
	only, err := e.svc.PurgeRunnerCache(e.ctx, &apiv1.PurgeRunnerCacheRequest{Name: "arm", Repo: "tester/demo", Kinds: []string{"toolcache"}, Domain: "oci-root", DryRun: true}, "admin")
	e.must(err)
	if only.MatchedBytes != 20 {
		t.Fatalf("按类别和执行身份预览 = %v", only)
	}

	resp, err := e.svc.PurgeRunnerCache(e.ctx, &apiv1.PurgeRunnerCacheRequest{Name: "arm", Repo: "tester/demo", Kinds: []string{"toolcache", "git"}}, "admin")
	e.must(err)
	if resp.PurgeId == 0 {
		t.Fatal("应发起清理请求")
	}
	var repoID int64
	var kinds []string
	if err := e.db.QueryRow(e.ctx, `SELECT repo_id, kinds FROM runner_cache_purges WHERE id=$1 AND runner_id=$2 AND state='pending' AND actor='admin'`,
		resp.PurgeId, id).Scan(&repoID, &kinds); err != nil || repoID != testRepoID || !slices.Equal(kinds, []string{"toolcache", "git"}) {
		t.Fatalf("清理请求 = %d %v，%v", repoID, kinds, err)
	}
	e.wantAudit("runner.cache_purge", "admin", map[string]any{"name": "arm", "repo": "tester/demo"})
	got, err = e.svc.GetRunnerCache(e.ctx, "arm")
	e.must(err)
	if len(got.Purges) != 1 || got.Purges[0].State != "pending" || got.Purges[0].Repo != "tester/demo" {
		t.Fatalf("清理请求列表 = %v", got.Purges)
	}

	for _, bad := range []*apiv1.PurgeRunnerCacheRequest{
		{Name: "arm", Kinds: []string{"everything"}},
		{Name: "arm", Repo: "tester/missing"},
		{Name: "missing"},
	} {
		if _, err := e.svc.PurgeRunnerCache(e.ctx, bad, "admin"); err == nil {
			t.Errorf("PurgeRunnerCache(%v) 应报错", bad)
		} else if ce := new(connect.Error); !errors.As(err, &ce) || (ce.Code() != connect.CodeInvalidArgument && ce.Code() != connect.CodeNotFound) {
			t.Errorf("PurgeRunnerCache(%v) = %v", bad, err)
		}
	}
	if _, err := e.svc.GetRunnerCache(e.ctx, "missing"); err == nil || !strings.Contains(err.Error(), "没有 Runner") {
		t.Errorf("GetRunnerCache(missing) = %v", err)
	}
}

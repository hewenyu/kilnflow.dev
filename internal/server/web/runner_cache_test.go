package web

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// ---- Runner 的规格与容量和本地缓存 ----

// cacheFixture 写入 Runner arm（上报了容量和缓存清单，正在执行一个声明 1.5 核 / 1 GiB 的 job）和 old（旧版，没有上报）。
// 清单中 tester/demo 有 host、oci-root 两份 toolcache 和 git 镜像，tester/other 有一份 toolcache，另有一个共享的 runtime。
func (e *webEnv) cacheFixture() (arm int64) {
	e.t.Helper()
	e.addRepo(502, "other")
	now := time.Now()
	arm = e.addRunner("arm", runnerOpts{lastSeen: &now})
	e.addRunner("old", runnerOpts{lastSeen: &now})
	e.exec(`UPDATE ci_runners SET capacity=$2 WHERE runner_id=$1`, arm, store.MustJSON(store.RunnerCapacity{
		Machine: store.Resources{CPU: 4, Memory: 24 << 30}, Reserved: store.Resources{Memory: 2 << 30},
		Limits: map[string]store.Resources{"host": {CPU: 2, Memory: 6 << 30}, "oci": {}}, MaxJobs: 2,
	}))
	run := e.addRun(runOpts{state: store.RunActive})
	job := e.addJob(jobOpts{runID: run, key: "build", state: store.JobRunning})
	e.exec(`UPDATE jobs SET spec='{"cpu":1.5,"memory_bytes":1073741824}' WHERE id=$1`, job)
	e.addAttempt(job, arm, "")
	rep, err := protojson.Marshal(&runnerv1.CacheReport{
		GeneratedAt: now.Unix(), DiskTotalBytes: 1000, DiskFreeBytes: 400, GcAt: now.Add(-time.Hour).Unix(), GcRemoved: 3, GcRemovedBytes: 30,
		GcFailures: []*runnerv1.CacheFailure{{Kind: "toolcache", Path: "toolcache/oci-root/501/linux-arm64/gocache", Error: "permission denied"}},
		Entries: []*runnerv1.CacheEntry{
			{Kind: "toolcache", RepoId: testRepoID, Platform: "linux/arm64", Domain: "host", Name: "gomod", Path: "toolcache/host/501/linux-arm64/gomod", SizeBytes: 10, LastUsedAt: now.Unix()},
			{Kind: "toolcache", RepoId: testRepoID, Platform: "linux/arm64", Domain: "oci-root", Name: "gomod", Path: "toolcache/oci-root/501/linux-arm64/gomod", SizeBytes: 20},
			{Kind: "git", RepoId: testRepoID, Name: "501.git", Path: "git/501.git", SizeBytes: 5},
			{Kind: "toolcache", RepoId: 502, Platform: "linux/arm64", Domain: "host", Name: "gomod", Path: "toolcache/host/502/linux-arm64/gomod", SizeBytes: 7},
			{Kind: "runtime", Name: "go-1.27.3", Path: "rt/abcd", SizeBytes: 100, ExpiresAt: now.Add(time.Hour).Unix()},
		},
	})
	e.must(err)
	e.exec(`UPDATE ci_runners SET cache_report=$2, cache_reported_at=now() WHERE runner_id=$1`, arm, rep)
	return arm
}

// Runners 页面：上报了容量的 Runner 显示规格、已分配（正在执行的 job 声明的资源）、可分配和 executor 上限，以及缓存总大小。
func TestPageRunners_CapacityAndCache(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.cacheFixture()

	w := e.get(e.bob, "/runners")
	wantStatus(t, "Runners 页面", w, http.StatusOK)
	byName := map[string]runnerData{}
	for _, v := range pageData[runnersData](t, w.Body.String()).Runners {
		byName[v.Name] = v
	}
	arm := byName["arm"]
	want := capacityView{
		Machine: resourcesView{"4", "24.0 GiB"}, Reserved: resourcesView{"0", "2.0 GiB"}, Allocatable: resourcesView{"4", "22.0 GiB"},
		Allocated: resourcesView{"1.5", "1.0 GiB"},
		// oci 的上限都是 0（不限制），不列出
		Limits: []limitView{{Executor: "host", resourcesView: resourcesView{"2", "6.0 GiB"}}}, MaxJobs: 2,
	}
	if c := arm.Capacity; c == nil || c.Machine != want.Machine || c.Reserved != want.Reserved || c.Allocatable != want.Allocatable ||
		c.Allocated != want.Allocated || !slices.Equal(c.Limits, want.Limits) || c.MaxJobs != want.MaxJobs {
		t.Errorf("arm 的容量 = %+v，应为 %+v", c, want)
	}
	if arm.CacheBytes != 142 || arm.CacheReportedAt == nil {
		t.Errorf("arm 的缓存 = %d，上报于 %v", arm.CacheBytes, arm.CacheReportedAt)
	}
	old := byName["old"]
	if old.Capacity != nil || old.CacheBytes != 0 || old.CacheReportedAt != nil {
		t.Errorf("旧版 Runner 没有上报：%+v", old)
	}
}

// 成员：可以查看清单、上一次清理和清理请求，没有清理表单的选项，也不能预览。
func TestPageRunnerCache_Member(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.cacheFixture()

	w := e.get(e.bob, "/runners/cache/arm?repo=tester/demo&preview=1")
	wantStatus(t, "成员查看缓存页", w, http.StatusOK)
	body := w.Body.String()
	if pg := parsePage(t, body); pg.Page != "runner_cache" {
		t.Fatalf("页面 = %q", pg.Page)
	}
	p := pageData[runnerCachePage](t, body)
	if p.Runner == nil || p.Runner.Name != "arm" || p.Generated == nil || len(p.Entries) != 5 || p.TotalBytes != 142 ||
		p.DiskTotal != 1000 || p.DiskFree != 400 || p.Self != "/runners/cache/arm" {
		t.Fatalf("缓存页 = %+v", p)
	}
	if e0 := p.Entries[0]; e0.Repo != "tester/demo" || e0.Kind != "toolcache" || e0.LastUsed == nil || e0.Size != 10 {
		t.Errorf("清单的第一项 = %+v", e0)
	}
	if p.Entries[2].Kind != "git" || p.Entries[4].Repo != "" || p.Entries[4].Expires == nil {
		t.Errorf("清单 = %+v", p.Entries)
	}
	if p.GC == nil || p.GC.Removed != 3 || p.GC.RemovedBytes != 30 || len(p.GC.Failures) != 1 || p.GC.Failures[0].Error != "permission denied" {
		t.Errorf("上一次清理 = %+v", p.GC)
	}
	if !slices.Equal(p.Domains, []string{"host", "oci-root"}) || !slices.Equal(p.Kinds, admin.CacheKinds()) {
		t.Errorf("表单选项：Domains %v Kinds %v", p.Domains, p.Kinds)
	}
	if p.Preview != nil || p.Repos != nil || p.Repo != "" {
		t.Errorf("成员不应有预览和仓库选项：%+v %v %q", p.Preview, p.Repos, p.Repo)
	}

	// 没有上报过的 Runner：清单为空；不存在的 Runner：404
	p = pageData[runnerCachePage](t, e.get(e.bob, "/runners/cache/old").Body.String())
	if p.Generated != nil || len(p.Entries) != 0 || p.GC != nil {
		t.Errorf("旧版 Runner 的缓存页 = %+v", p)
	}
	w = e.get(e.bob, "/runners/cache/ghost")
	wantStatus(t, "不存在的 Runner", w, http.StatusNotFound)
	mustHave(t, "页面", pageTextZH(t, w.Body.String()), "没有名为 ghost 的 Runner")
}

// 管理员预览：只包含所选范围的项，列出继续保留的共享缓存；预览不写入清理请求。
func TestPageRunnerCache_Preview(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.cacheFixture()

	w := e.get(e.alice, "/runners/cache/arm?repo=tester/demo&kind=toolcache&kind=bogus&domain=&preview=1")
	wantStatus(t, "管理员预览", w, http.StatusOK)
	p := pageData[runnerCachePage](t, w.Body.String())
	if !slices.Equal(p.Repos, []string{"tester/demo", "tester/other"}) || p.Repo != "tester/demo" || !slices.Equal(p.KindsPicked, []string{"toolcache"}) {
		t.Errorf("表单取值：Repos %v Repo %q Kinds %v", p.Repos, p.Repo, p.KindsPicked)
	}
	if p.Preview == nil || !p.Preview.Error.IsZero() {
		t.Fatalf("预览 = %+v", p.Preview)
	}
	var matched []string
	for _, m := range p.Preview.Matched {
		matched = append(matched, m.Path)
	}
	if !slices.Equal(matched, []string{"toolcache/host/501/linux-arm64/gomod", "toolcache/oci-root/501/linux-arm64/gomod"}) || p.Preview.MatchedBytes != 30 {
		t.Errorf("预览匹配 %v（%d 字节）", matched, p.Preview.MatchedBytes)
	}
	// 说明是原因码映射的消息：清单的生成时间是 Unix 秒（前端按语言和时区显示），正在执行的 job，共享缓存继续保留
	if n := p.Preview.Notes; len(p.Preview.Shared) == 0 || len(n) != 3 || n[0].id != msgCacheNoteReport ||
		toInt64(t, n[0].params[argAt]) != p.Generated.Unix() || textZH(t, n[1]) != "arm 正在执行 1 个 job：清理在它们结束后进行，期间不领取新任务" ||
		n[2].id != msgCacheNoteSharedKept {
		t.Errorf("预览应列出保留的共享缓存和说明：%+v", p.Preview)
	}
	// 逗号分隔的类别与重复的 kind 相同；指定执行身份时只匹配这个执行身份的项（两个仓库的 host toolcache）
	p = pageData[runnerCachePage](t, e.get(e.alice, "/runners/cache/arm?kind=git,toolcache&domain=host&preview=1").Body.String())
	if !slices.Equal(p.KindsPicked, []string{"git", "toolcache"}) || p.Domain != "host" || p.Preview == nil || p.Preview.MatchedBytes != 17 {
		t.Errorf("按类别和执行身份预览：Kinds %v Domain %q 预览 %+v", p.KindsPicked, p.Domain, p.Preview)
	}
	// 不在 Kiln 中的仓库：预览失败，页面仍然正常显示
	p = pageData[runnerCachePage](t, e.get(e.alice, "/runners/cache/arm?repo=tester/ghost&preview=1").Body.String())
	if p.Preview == nil || textZH(t, p.Preview.Error) != "仓库 tester/ghost 不在 Kiln 中，不能预览。" || len(p.Entries) != 5 {
		t.Errorf("未知仓库的预览 = %+v", p.Preview)
	}
	if n := e.count(`SELECT count(*) FROM runner_cache_purges`); n != 0 {
		t.Errorf("预览不应写入清理请求：%d", n)
	}
}

func TestPostRunnerCachePurge(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	arm := e.cacheFixture()
	purges := func() int { return e.count(`SELECT count(*) FROM runner_cache_purges`) }

	// 成员、CSRF 不对：拒绝，不写入
	w := e.post(e.bob, "/runners/cache/arm/purge", url.Values{"kind": {"toolcache"}})
	wantStatus(t, "成员", w, http.StatusForbidden)
	mustHave(t, "页面", pageTextZH(t, w.Body.String()), "需要管理员权限")
	w = e.post(e.alice, "/runners/cache/arm/purge", url.Values{"csrf": {e.bob.csrf}})
	wantStatus(t, "其他会话的 CSRF", w, http.StatusForbidden)
	if n := purges(); n != 0 {
		t.Fatalf("被拒绝的请求不应写入：%d", n)
	}

	// 管理员：写入待执行的清理请求（类别用逗号分隔，与预览页的确认表单相同），回到缓存页
	flash := flashZH(t, wantRedirect(t, "发起清理", e.post(e.alice, "/runners/cache/arm/purge", url.Values{
		"repo": {"tester/demo"}, "kind": {"toolcache,git"}, "domain": {"host"},
	}), "/runners/cache/arm"))
	mustHave(t, "操作结果", flash, "已发起清理请求 #", "arm 在没有正在执行的 job 时执行")
	var (
		runnerID, repoID int64
		kinds            []string
		domain, state    string
		actor            string
	)
	e.must(e.db.QueryRow(e.ctx, `SELECT runner_id, repo_id, kinds, domain, state, actor FROM runner_cache_purges`).
		Scan(&runnerID, &repoID, &kinds, &domain, &state, &actor))
	if runnerID != arm || repoID != testRepoID || !slices.Equal(kinds, []string{"toolcache", "git"}) || domain != "host" || state != "pending" || actor != "alice" {
		t.Errorf("清理请求：runner %d repo %d kinds %v domain %q state %q actor %q", runnerID, repoID, kinds, domain, state, actor)
	}
	p := pageData[runnerCachePage](t, e.get(e.bob, "/runners/cache/arm").Body.String())
	if len(p.Purges) != 1 || p.Purges[0].State != "pending" || p.Purges[0].Repo != "tester/demo" || p.Purges[0].Actor != "alice" {
		t.Errorf("缓存页的清理请求 = %+v", p.Purges)
	}

	// 未知仓库：失败，不写入；不存在的 Runner：回到列表
	flash = flashZH(t, wantRedirect(t, "未知仓库", e.post(e.alice, "/runners/cache/arm/purge", url.Values{"repo": {"tester/ghost"}}), "/runners/cache/arm"))
	mustHave(t, "操作结果", flash, "发起清理失败")
	flash = flashZH(t, wantRedirect(t, "不存在的 Runner", e.post(e.alice, "/runners/cache/ghost/purge", nil), "/runners"))
	mustHave(t, "操作结果", flash, "没有名为 ghost 的 Runner")
	if n := purges(); n != 1 {
		t.Errorf("失败的请求不应写入：%d", n)
	}
}

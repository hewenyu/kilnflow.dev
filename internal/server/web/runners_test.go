package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hewenyu/kilnflow.dev/internal/server/auth"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// testRunners 覆盖连接和接单的各种组合：在线接单、在线暂停且忙碌、离线（暂停标记仍在）、离线接单、已吊销。
func testRunners(now time.Time) []*runnerView {
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	return []*runnerView{
		{
			ID: 1, Name: "mini-01", Pool: "default", Platforms: []string{"darwin/arm64", "linux/arm64"}, Executors: []string{"host", "oci"},
			Version: "v0.1.11", Online: true, LastSeen: ago(5 * time.Second),
			Upgrade: &runnerUpgradeView{
				State: "downloading", Target: "0.1.12", Launcher: true, Current: "0.1.11", Previous: "0.1.10",
				Package: "@yuebanlaosiji/kilnflow-runner-darwin-arm64", Latest: "0.1.12", CheckedAt: ago(time.Minute),
				Reason:  upgradeReasonText(upgradectl.Reason{Code: upgradectl.ReasonUpgrading, Version: "0.1.12"}),
				Request: &upgradeRequestView{ID: 1001, Version: "0.1.12", From: "0.1.11", State: "pending", Actor: "alice", Created: now},
			},
		},
		{
			ID: 2, Name: "mini-02", Pool: "default", Platforms: []string{"darwin/arm64"}, Executors: []string{"host"},
			Version: "0123456789abcdef0123456789abcdef01234567", Online: true, Drain: true, LastSeen: ago(3 * time.Second),
			Jobs: []cistore.RunningJob{
				{RunnerID: 2, JobID: 41, Name: "build (a)", RunID: 9, Workflow: "ci", Repo: "tester/demo", Started: now.Add(-time.Minute)},
				{RunnerID: 2, JobID: 42, Name: "test", RunID: 9, Workflow: "ci", Repo: "tester/demo", Started: now},
			},
		},
		{
			ID: 3, Name: "linux-01", Pool: "linux", Platforms: []string{"linux/amd64"}, Executors: []string{"host", "oci", "buildkit"},
			Version: "dev-0123456789ab-dirty", Drain: true, LastSeen: ago(time.Hour),
		},
		{
			ID: 4, Name: "mac-01", Pool: "default", Version: "v0.1.0", LastSeen: ago(30 * time.Minute),
			Upgrade: &runnerUpgradeView{
				State: "rolled_back", Target: "0.1.12", Error: "新版本连续 3 次启动失败", Launcher: true, Latest: "0.1.12", CheckError: "registry 不可达",
				CanUpgrade: true,
				Request:    &upgradeRequestView{ID: 1002, Version: "0.1.12", State: "failed", Error: "新版本没有通过确认，已回滚", Actor: "alice", Created: now, Finished: &now},
			},
		},
		{ID: 5, Name: "retired", Pool: "linux", Revoked: true, LastSeen: ago(48 * time.Hour)},
	}
}

func testRunnersPage(now time.Time) *runnersPage {
	return &runnersPage{
		Runners: testRunners(now), Counts: runnerCounts{Total: 5, Online: 2, Accepting: 1, Paused: 1, Offline: 2, Revoked: 1, Jobs: 2},
		States: runnerStateOptions, Pools: []string{"default", "linux"}, Self: "/runners", Now: now,
		OfflineAfter: 120, LeaseTTL: 60, LongPoll: 25,
	}
}

func testEnrollPage(now time.Time, status enrollStatus, token string) *runnerNewPage {
	f := newForm()
	f.Values["pool"], f.Values["name"] = "linux", "linux-02"
	return &runnerNewPage{
		Form: f, RequestID: "r1", RegenID: "r2", PublicURL: "https://kiln.example.com", ServerVersion: "v0.1.11",
		Pools: []string{"default", "linux"},
		Reveal: &enrollView{
			ID: "0123456789abcdef0123456789abcdef", Token: token, Pool: "linux", Name: "linux-02",
			Created: now, Expires: now.Add(time.Hour), Status: status,
		},
	}
}

// renderAs 以 role（admin / user）的会话把 data 编码为页面 page 的数据（JSON），返回 JSON 文本。
func renderAs(t *testing.T, page, role string, data any) string {
	t.Helper()
	d := &Dashboard{log: slog.New(slog.DiscardHandler)}
	w := httptest.NewRecorder()
	se := &session{Login: "alice", Role: role, Exp: time.Now().Add(time.Hour).Unix()}
	d.writeJSON(w, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, &uiPage{Page: page, Title: rawText(page), Session: newUISession(se), CSRF: "csrf", Nav: "runners", Status: http.StatusOK, Data: data})
	dumpUI("render "+page, w)
	if w.Code != http.StatusOK {
		t.Fatalf("编码 %s：HTTP %d：%s", page, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// runnerData 是页面数据中的一台 Runner（runnerView），另带计算的连接状态和是否接单。
type runnerData struct {
	runnerView
	Accepting bool
	ConnClass string
}

// runnersData 是 Runners 页面的页面数据（runnersPage），另带计算的 Filtered、StateLabel。
type runnersData struct {
	runnersPage
	Runners    []runnerData
	Filtered   bool
	StateLabel uiText
}

// runnerNewData 是注册新的 Runner 页面的页面数据（runnerNewPage），另带计算的注册命令和表单的 Failed。
type runnerNewData struct {
	runnerNewPage
	Form struct {
		Values map[string]string
		Errors map[string]uiText
		Failed bool
	}
	Reveal *struct {
		enrollView
		Runner, Existing *runnerData
	}
	Command string
}

func TestRunnersRender(t *testing.T) {
	now := time.Now()
	body := renderAs(t, "runners", "admin", testRunnersPage(now))
	if se := parsePage(t, body).Session; se == nil || !se.Admin {
		t.Errorf("管理员（前端据此显示暂停、恢复、吊销和注册入口）：%+v", se)
	}
	p := pageData[runnersData](t, body)
	// 连接和接单分开：ConnClass 是连接状态的样式（文字由前端按 Revoked、Online 显示），Accepting 表示在线并接单
	type conn struct {
		online, revoked bool
		class           string
		accepting       bool
	}
	want := map[string]conn{
		"mini-01": {true, false, "ok", true}, "mini-02": {true, false, "ok", false}, "linux-01": {false, false, "warn", false},
		"mac-01": {false, false, "warn", false}, "retired": {false, true, "bad", false},
	}
	if len(p.Runners) != len(want) {
		t.Fatalf("Runner 列表：%+v", p.Runners)
	}
	for _, v := range p.Runners {
		if got := (conn{v.Online, v.Revoked, v.ConnClass, v.Accepting}); got != want[v.Name] {
			t.Errorf("%s 的连接状态 %+v，应为 %+v", v.Name, got, want[v.Name])
		}
	}
	// 正在执行的任务（前端链接到 job）、完整的版本号（前端缩短显示）
	v := p.Runners[1]
	if expect(t, "mini-02", v, len(v.Jobs) == 2, v.Version == "0123456789abcdef0123456789abcdef01234567", v.Drain) {
		if j := v.Jobs[0]; j != (cistore.RunningJob{RunnerID: 2, JobID: 41, Name: "build (a)", RunID: 9, Workflow: "ci", Repo: "tester/demo", Started: j.Started}) ||
			!j.Started.Equal(now.Add(-time.Minute)) {
			t.Errorf("mini-02 正在执行的任务：%+v", j)
		}
	}
	expect(t, "统计和说明", p.runnersPage, p.Counts == (runnerCounts{Total: 5, Online: 2, Accepting: 1, Paused: 1, Offline: 2, Revoked: 1, Jobs: 2}), !p.Filtered,
		p.LeaseTTL == 60, p.LongPoll == 25, p.OfflineAfter == 120, slices.Equal(p.Pools, []string{"default", "linux"}),
		textsZH(t, textOptionLabels(p.States))[2] == "已暂停接单")

	// 普通用户：数据相同，前端按 Session.Admin 不显示操作和注册入口
	if se := parsePage(t, renderAs(t, "runners", "user", testRunnersPage(now))).Session; se == nil || se.Admin {
		t.Errorf("普通用户：%+v", se)
	}

	// 零台
	p = pageData[runnersData](t, renderAs(t, "runners", "admin", &runnersPage{States: runnerStateOptions, Self: "/runners", Now: now}))
	expect(t, "没有 Runner", p, len(p.Runners) == 0, p.Counts.Total == 0, !p.Filtered)

	// 筛选后没有结果；全部离线（Counts.Accepting 为 0）时前端提示新任务会排队
	p = pageData[runnersData](t, renderAs(t, "runners", "admin", &runnersPage{
		States: runnerStateOptions, Pools: []string{"default"}, State: "revoked", Pool: "default", Self: "/runners?pool=default&state=revoked", Now: now,
		Counts: runnerCounts{Total: 1, Offline: 1},
	}))
	expect(t, "筛选无结果", p, p.Filtered, textZH(t, p.StateLabel) == "已吊销", p.Pool == "default", len(p.Runners) == 0, p.Counts.Accepting == 0, p.Counts.Total == 1)
}

func TestRunnerNewRender(t *testing.T) {
	now := time.Now()
	// 生成令牌的表单：校验失败时保留输入、字段旁报错
	f := newForm()
	f.Values["pool"], f.Values["name"] = "bad pool", "mini 01"
	f.fail("pool", "pool 只能包含字母")
	f.fail("name", "名称只能包含字母")
	p := pageData[runnerNewData](t, renderAs(t, "runner_new", "admin", &runnerNewPage{Form: f, RequestID: "r1", PublicURL: "https://kiln.example.com", ServerVersion: "v0.1.11", Pools: []string{"default"}}))
	expect(t, "生成令牌的表单", p, p.Form.Failed, p.Form.Values["pool"] == "bad pool", !p.Form.Errors["pool"].IsZero(), !p.Form.Errors["name"].IsZero(),
		p.RequestID == "r1", p.PublicURL == "https://kiln.example.com", p.Reveal == nil, p.Command == "")

	// reveal 返回展示页的页面数据和其中的 Reveal（不能为 nil）。
	reveal := func(ep *runnerNewPage) (runnerNewData, string) {
		t.Helper()
		body := renderAs(t, "runner_new", "admin", ep)
		p := pageData[runnerNewData](t, body)
		if p.Reveal == nil {
			t.Fatalf("展示页应有 Reveal：%s", body)
		}
		return p, body
	}

	// 第一次打开展示页：令牌、注册命令、到期时间
	p, body := reveal(testEnrollPage(now, enrollUnused, "kiln-enroll-x"))
	if want := "kiln-runner enroll --server https://kiln.example.com --token kiln-enroll-x --name linux-02\nkiln-runner doctor\nkiln-runner service install"; p.Command != want {
		t.Errorf("注册命令 %q，应为 %q", p.Command, want)
	}
	expect(t, "一次性展示", p.Reveal, p.Reveal.Token == "kiln-enroll-x", p.Reveal.Status == enrollUnused, p.Reveal.Expires.Equal(now.Add(time.Hour)))
	mustHave(t, "一次性展示", body, `"Expires":"`+jsonTime(now.Add(time.Hour))+`"`)

	// 刷新后：不再有令牌和命令，可以重新生成（RegenID）
	p, body = reveal(testEnrollPage(now, enrollUnused, ""))
	expect(t, "已经显示过", p, p.Command == "", p.Reveal.Token == "", p.RegenID == "r2", p.Reveal.Pool == "linux", p.Reveal.Name == "linux-02")
	if strings.Contains(body, "kiln-enroll-") || strings.Contains(body, "--token") {
		t.Error("刷新后不应再有令牌")
	}

	// 过期
	ep := testEnrollPage(now, enrollExpired, "")
	ep.Reveal.Expires = now.Add(-time.Minute)
	p, _ = reveal(ep)
	expect(t, "已过期", p.Reveal, p.Reveal.Status == enrollExpired, p.Command == "")

	// 已使用：带着使用它的 Runner 的连接状态
	ep = testEnrollPage(now, enrollUsed, "")
	used := now.Add(-time.Minute)
	ep.Reveal.UsedAt, ep.Reveal.Runner = &used, testRunners(now)[0]
	p, _ = reveal(ep)
	v := p.Reveal
	expect(t, "已使用", v, v.Status == enrollUsed, v.UsedAt != nil, v.Runner != nil && v.Runner.Name == "mini-01" && v.Runner.ConnClass == "ok" && v.Runner.Accepting)
	ep.Reveal.Runner = testRunners(now)[3]
	p, _ = reveal(ep)
	expect(t, "已使用但离线", p.Reveal, p.Reveal.Runner != nil && !p.Reveal.Runner.Online && p.Reveal.Runner.ConnClass == "warn")

	// 同名 Runner 已存在
	ep = testEnrollPage(now, enrollUnused, "kiln-enroll-x")
	ep.Reveal.Existing = testRunners(now)[4]
	p, _ = reveal(ep)
	expect(t, "同名（已吊销）", p.Reveal, p.Reveal.Existing != nil && p.Reveal.Existing.Name == "retired" && p.Reveal.Existing.Revoked)
	ep.Reveal.Existing = testRunners(now)[0]
	p, _ = reveal(ep)
	expect(t, "同名（在用）", p.Reveal, p.Reveal.Existing != nil && !p.Reveal.Existing.Revoked && p.Reveal.Existing.Online)

	// 找不到
	p = pageData[runnerNewData](t, renderAs(t, "runner_new", "admin", &runnerNewPage{Form: newForm(), RequestID: "r1", Missing: true}))
	expect(t, "找不到", p, p.Missing, p.Reveal == nil)
}

func TestEnrollReveals(t *testing.T) {
	s := newEnrollReveals()
	now := time.Now()
	s.put("a", &enrollReveal{login: "alice", tokenHash: "h1", token: "kiln-enroll-a", created: now, expires: now.Add(time.Hour)})
	if !s.has("a", "alice") || s.has("a", "bob") || s.has("b", "alice") {
		t.Fatal("has 应只对生成令牌的用户和请求 ID 为真")
	}
	if _, ok := s.take("a", "bob"); ok {
		t.Fatal("其他用户不能取到令牌")
	}
	v, ok := s.take("a", "alice")
	if !ok || v.token != "kiln-enroll-a" || v.tokenHash != "h1" {
		t.Fatalf("第一次应取到令牌：%+v %v", v, ok)
	}
	v, ok = s.take("a", "alice")
	if !ok || v.token != "" || v.tokenHash != "h1" {
		t.Fatalf("第二次只取到状态，不再有令牌：%+v %v", v, ok)
	}
	// 过期一小时后清理；条目数有上限
	s.put("old", &enrollReveal{login: "alice", created: now.Add(-3 * time.Hour), expires: now.Add(-2 * time.Hour)})
	s.put("c", &enrollReveal{login: "alice", created: now, expires: now.Add(time.Hour)})
	if s.has("old", "alice") {
		t.Error("过期超过 revealKeep 的条目应被清理")
	}
	for i := range maxReveals + 10 {
		s.put(strings.Repeat("x", i+1), &enrollReveal{login: "alice", created: now.Add(time.Duration(i) * time.Second), expires: now.Add(time.Hour)})
	}
	if n := len(s.m); n > maxReveals {
		t.Errorf("条目数 %d 超过上限 %d", n, maxReveals)
	}
	if !s.has(strings.Repeat("x", maxReveals+10), "alice") {
		t.Error("最新的条目不应被清理")
	}
}

func TestRunnerMatch(t *testing.T) {
	rs := testRunners(time.Now())
	want := map[string][]string{
		"":          {"mini-01", "mini-02", "linux-01", "mac-01", "retired"},
		"online":    {"mini-01", "mini-02"},
		"accepting": {"mini-01"},
		"paused":    {"mini-02", "linux-01"},
		"busy":      {"mini-02"},
		"offline":   {"linux-01", "mac-01"},
		"revoked":   {"retired"},
	}
	for state, names := range want {
		var got []string
		for _, v := range rs {
			if v.match(state) {
				got = append(got, v.Name)
			}
		}
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("状态 %q：%v，应为 %v", state, got, names)
		}
	}
	if !runnerStateLabel("bogus").IsZero() || textZH(t, runnerStateLabel("paused")) != "已暂停接单" {
		t.Error("runnerStateLabel")
	}
}

func TestRunnersURL(t *testing.T) {
	for _, c := range []struct{ state, pool, want string }{
		{"", "", "/runners"},
		{"paused", "", "/runners?state=paused"},
		{"", "linux", "/runners?pool=linux"},
		{"busy", "a&b", "/runners?pool=a%26b&state=busy"},
	} {
		if got := runnersURL(c.state, c.pool); got != c.want {
			t.Errorf("runnersURL(%q, %q) = %q，应为 %q", c.state, c.pool, got, c.want)
		}
	}
}

func TestRunnerOpResult(t *testing.T) {
	rs := testRunners(time.Now())
	for _, c := range []struct {
		v      *runnerView
		op     string
		wants  []string
		absent string
	}{
		{rs[1], "drain", []string{"已暂停 mini-02 接收新任务（它正在进行的一轮领取结束后生效，最长约 25 秒）", "正在执行的 2 个任务会继续执行到结束"}, ""},
		{rs[0], "drain", []string{"它现在没有正在执行的任务"}, ""},
		{rs[1], "undrain", []string{"mini-02 已恢复接单，有空闲时会领取新任务"}, "离线"},
		{rs[2], "undrain", []string{"目前离线，重新连接后才会领取"}, ""},
		{rs[1], "revoke", []string{"已吊销 mini-02", "2 个任务会被中断，约 1 分钟后", "重新注册"}, ""},
		{rs[0], "revoke", []string{"已吊销 mini-01"}, "中断"},
		{rs[0], "delete", []string{"已删除 mini-01", "名称可以重新注册"}, "中断"},
	} {
		got := textZH(t, runnerOpResult(&opTarget{Name: c.v.Name, Revoked: c.v.Revoked, Online: c.v.Online, Running: len(c.v.Jobs)}, c.op, time.Minute, 25*time.Second))
		for _, w := range c.wants {
			if !strings.Contains(got, w) {
				t.Errorf("%s %s：%q 中没有 %q", c.op, c.v.Name, got, w)
			}
		}
		if c.absent != "" && strings.Contains(got, c.absent) {
			t.Errorf("%s %s：%q 中不应有 %q", c.op, c.v.Name, got, c.absent)
		}
	}
	if humanDuration(90*time.Second) != "90 秒" || humanDuration(2*time.Minute) != "2 分钟" {
		t.Error("humanDuration")
	}
}

// TestEnrollRevealsEnsure：并发重复提交同一个表单只生成一个令牌；不同用户的相同请求 ID 互不影响；生成失败后可以重试。
func TestEnrollRevealsEnsure(t *testing.T) {
	s := newEnrollReveals()
	var mu sync.Mutex
	calls := map[string]int{}
	create := func(login string) func() (*enrollReveal, error) {
		return func() (*enrollReveal, error) {
			mu.Lock()
			calls[login]++
			n := calls[login]
			mu.Unlock()
			time.Sleep(20 * time.Millisecond) // 让并发的请求都到达检查之后
			return &enrollReveal{token: fmt.Sprintf("kiln-enroll-%s-%d", login, n), created: time.Now(), expires: time.Now().Add(time.Hour)}, nil
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for range 20 {
		wg.Go(func() { errs <- s.ensure("r1", "alice", create("alice")) })
		wg.Go(func() { errs <- s.ensure("r1", "bob", create("bob")) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls["alice"] != 1 || calls["bob"] != 1 {
		t.Fatalf("每个用户的同一个请求 ID 只应生成一次：%v", calls)
	}
	if v, ok := s.take("r1", "alice"); !ok || v.token != "kiln-enroll-alice-1" || v.login != "alice" {
		t.Errorf("alice 的令牌：%+v", v)
	}
	if v, ok := s.take("r1", "bob"); !ok || v.token != "kiln-enroll-bob-1" {
		t.Errorf("bob 的令牌不应被 alice 覆盖：%+v", v)
	}

	// 生成失败：不保存，用同一个请求 ID 重试会再次生成
	boom := errors.New("boom")
	if err := s.ensure("r2", "alice", func() (*enrollReveal, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("应返回生成的错误：%v", err)
	}
	if s.has("r2", "alice") {
		t.Fatal("生成失败时不应保存")
	}
	if err := s.ensure("r2", "alice", create("alice")); err != nil || calls["alice"] != 2 || !s.has("r2", "alice") {
		t.Fatalf("重试应生成：%v %v", err, calls)
	}
}

// ---- 使用数据库的 Runners 页面和操作 ----

// runnerFixture 写入一组状态各不相同的 Runner：
// mac-1 在线接单并在执行一个 job，mac-2 在线已暂停，linux-1（pool linux）离线，old-1 已吊销。
func (e *webEnv) runnerFixture() map[string]int64 {
	e.t.Helper()
	now, stale := time.Now(), time.Now().Add(-time.Hour)
	ids := map[string]int64{
		"mac-1":   e.addRunner("mac-1", runnerOpts{lastSeen: &now}),
		"mac-2":   e.addRunner("mac-2", runnerOpts{lastSeen: &now, drain: true}),
		"linux-1": e.addRunner("linux-1", runnerOpts{pool: "linux", lastSeen: &stale}),
		"old-1":   e.addRunner("old-1", runnerOpts{lastSeen: &now, state: store.RunnerRevoked}),
	}
	run := e.addRun(runOpts{state: store.RunActive})
	job := e.addJob(jobOpts{runID: run, key: "compile", state: store.JobRunning})
	e.addAttempt(job, ids["mac-1"], "")
	return ids
}

// listedRunners 返回 Runners 页面数据中的 Runner 名称（排序后）。
func listedRunners(t *testing.T, body string) []string {
	t.Helper()
	var names []string
	for _, v := range pageData[runnersData](t, body).Runners {
		names = append(names, v.Name)
	}
	slices.Sort(names)
	return names
}

func TestPageRunners_Filters(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.runnerFixture()

	w := e.get(e.bob, "/runners")
	wantStatus(t, "Runners 页面", w, http.StatusOK)
	body := w.Body.String()
	p := pageData[runnersData](t, body)
	if p.Counts != (runnerCounts{Total: 4, Online: 2, Accepting: 1, Paused: 1, Offline: 1, Revoked: 1, Jobs: 1}) {
		t.Errorf("统计 = %+v", p.Counts)
	}
	for _, v := range p.Runners {
		if busy := len(v.Jobs) == 1 && v.Jobs[0].Name == "compile"; busy != (v.Name == "mac-1") {
			t.Errorf("%s 正在执行的任务：%+v", v.Name, v.Jobs)
		}
	}
	// 成员看不到 Runner 的操作和注册入口：前端按 Session.Admin 决定
	if se := parsePage(t, body).Session; se == nil || se.Admin {
		t.Errorf("成员的身份：%+v", se)
	}
	if se := parsePage(t, e.get(e.alice, "/runners").Body.String()).Session; se == nil || !se.Admin {
		t.Errorf("管理员的身份：%+v", se)
	}

	all := []string{"linux-1", "mac-1", "mac-2", "old-1"}
	for _, c := range []struct {
		query string
		want  []string
		// state、pool 是生效的筛选条件，label 是状态筛选的文字
		state, pool, label string
	}{
		{"", all, "", "", ""},
		{"?state=online", []string{"mac-1", "mac-2"}, "online", "", "在线"},
		{"?state=accepting", []string{"mac-1"}, "accepting", "", "在线并接单"},
		{"?state=paused", []string{"mac-2"}, "paused", "", "已暂停接单"},
		{"?state=busy", []string{"mac-1"}, "busy", "", "正在执行任务"},
		{"?state=offline", []string{"linux-1"}, "offline", "", "离线"},
		{"?state=revoked", []string{"old-1"}, "revoked", "", "已吊销"},
		{"?pool=linux", []string{"linux-1"}, "", "linux", ""},
		{"?state=online&pool=linux", nil, "online", "linux", "在线"},
		// 不认识的状态和不存在的 pool 被忽略，显示全部
		{"?state=bogus&pool=nope", all, "", "", ""},
	} {
		t.Run(c.query, func(t *testing.T) {
			w := e.get(e.bob, "/runners"+c.query)
			wantStatus(t, c.query, w, http.StatusOK)
			body := w.Body.String()
			if got := listedRunners(t, body); !slices.Equal(got, c.want) {
				t.Errorf("列出 %v，应为 %v", got, c.want)
			}
			p := pageData[runnersData](t, body)
			if p.State != c.state || p.Pool != c.pool || textZH(t, p.StateLabel) != c.label || p.Filtered != (c.state != "" || c.pool != "") {
				t.Errorf("筛选条件：State %q Pool %q StateLabel %q Filtered %v", p.State, p.Pool, p.StateLabel, p.Filtered)
			}
			// 统计不受筛选影响
			if p.Counts.Total != 4 {
				t.Errorf("统计 = %+v", p.Counts)
			}
		})
	}
}

func TestPageRunners_NoneAccepting(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	w := e.get(e.alice, "/runners")
	wantStatus(t, "没有 Runner", w, http.StatusOK)
	if p := pageData[runnersData](t, w.Body.String()); p.Counts != (runnerCounts{}) || len(p.Runners) != 0 {
		t.Errorf("没有 Runner 时统计应为零：%+v", p.Counts)
	}
	// 只有暂停接单的 Runner：Accepting 为 0（前端提示新任务会排队，可以恢复接单）
	now := time.Now()
	e.addRunner("paused", runnerOpts{lastSeen: &now, drain: true})
	if p := pageData[runnersData](t, e.get(e.alice, "/runners").Body.String()); p.Counts != (runnerCounts{Total: 1, Online: 1, Paused: 1}) {
		t.Errorf("统计 = %+v", p.Counts)
	}
}

func TestPostRunnerOp(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.runnerFixture()
	drain := func(name string) bool {
		t.Helper()
		var v bool
		e.must(e.db.QueryRow(e.ctx, `SELECT drain FROM runners WHERE name=$1`, name).Scan(&v))
		return v
	}
	state := func(name string) string {
		t.Helper()
		var v string
		e.must(e.db.QueryRow(e.ctx, `SELECT state FROM runners WHERE name=$1`, name).Scan(&v))
		return v
	}

	// 暂停：回到原来的筛选条件，提示正在执行的任务会继续
	keep := url.Values{"state": {"online"}, "pool": {"default"}}
	flash := flashZH(t, wantRedirect(t, "暂停 mac-1", e.post(e.alice, "/runners/mac-1/drain", keep), "/runners?pool=default&state=online"))
	mustHave(t, "操作结果", flash, "已暂停 mac-1 接收新任务", "最长约 20 秒", "正在执行的 1 个任务会继续执行到结束")
	if !drain("mac-1") {
		t.Error("mac-1 应已暂停接单")
	}
	// 恢复：离线的 Runner 提示重新连接后才会领取
	flash = flashZH(t, wantRedirect(t, "恢复 linux-1", e.post(e.alice, "/runners/linux-1/undrain", nil), "/runners"))
	mustHave(t, "操作结果", flash, "linux-1 已恢复接单，但它目前离线")
	flash = flashZH(t, wantRedirect(t, "恢复 mac-1", e.post(e.alice, "/runners/mac-1/undrain", nil), "/runners"))
	mustHave(t, "操作结果", flash, "mac-1 已恢复接单，有空闲时会领取新任务")
	if drain("mac-1") {
		t.Error("mac-1 应已恢复接单")
	}
	// 吊销：提示正在执行的任务会被中断
	flash = flashZH(t, wantRedirect(t, "吊销 mac-1", e.post(e.alice, "/runners/mac-1/revoke", nil), "/runners"))
	mustHave(t, "操作结果", flash, "已吊销 mac-1", "正在执行的 1 个任务会被中断", "约 90 秒后")
	if state("mac-1") != store.RunnerRevoked {
		t.Error("mac-1 应已吊销")
	}
	for _, action := range []string{"runner.drain", "runner.undrain", "runner.revoke"} {
		list := e.audits(action)
		if len(list) == 0 || list[len(list)-1].Actor != "alice" {
			t.Errorf("%s 的审计 = %+v，应记录操作者 alice", action, list)
		}
	}

	// 已吊销、不存在的 Runner：不执行任何操作，也不写审计
	before := e.count(`SELECT count(*) FROM audit_log`)
	for _, op := range []string{"drain", "undrain", "revoke"} {
		flash = flashZH(t, wantRedirect(t, op+" 已吊销的 old-1", e.post(e.alice, "/runners/old-1/"+op, nil), "/runners"))
		mustHave(t, "操作结果", flash, "old-1 已经被吊销，没有执行任何操作")
	}
	if drain("old-1") {
		t.Error("已吊销的 Runner 不应被暂停")
	}
	flash = flashZH(t, wantRedirect(t, "不存在的 Runner", e.post(e.alice, "/runners/ghost/drain", nil), "/runners"))
	mustHave(t, "操作结果", flash, "没有名为 ghost 的 Runner")
	if n := e.count(`SELECT count(*) FROM audit_log`); n != before {
		t.Errorf("没有执行的操作不应写审计：%d → %d", before, n)
	}

	// 删除：没有吊销的、还有未结束任务的不删除；已吊销的删除后名称不再出现
	flash = flashZH(t, wantRedirect(t, "删除没有吊销的 mac-2", e.post(e.alice, "/runners/mac-2/delete", nil), "/runners"))
	mustHave(t, "操作结果", flash, "mac-2 没有吊销，没有执行任何操作")
	flash = flashZH(t, wantRedirect(t, "删除有任务的 mac-1", e.post(e.alice, "/runners/mac-1/delete", nil), "/runners"))
	mustHave(t, "操作结果", flash, "操作 mac-1 失败", "1 个任务没有结束")
	if n := e.count(`SELECT count(*) FROM audit_log`); n != before {
		t.Errorf("没有执行的删除不应写审计：%d → %d", before, n)
	}
	flash = flashZH(t, wantRedirect(t, "删除 old-1", e.post(e.alice, "/runners/old-1/delete", keep), "/runners?pool=default&state=online"))
	mustHave(t, "操作结果", flash, "已删除 old-1")
	if n := e.count(`SELECT count(*) FROM runners WHERE name='old-1'`); n != 0 {
		t.Error("old-1 应已删除")
	}
	if list := e.audits("runner.delete"); len(list) != 1 || list[0].Actor != "alice" {
		t.Errorf("runner.delete 的审计 = %+v，应记录操作者 alice", list)
	}

	// 不认识的操作、成员、CSRF 不对、未登录：都不执行
	w := e.post(e.alice, "/runners/mac-2/explode", nil)
	wantStatus(t, "不认识的操作", w, http.StatusNotFound)
	mustHave(t, "页面", pageTextZH(t, w.Body.String()), "没有这个 Runner 操作")
	w = e.post(e.bob, "/runners/mac-2/undrain", nil)
	wantStatus(t, "成员", w, http.StatusForbidden)
	mustHave(t, "页面", pageTextZH(t, w.Body.String()), "需要管理员权限")
	w = e.post(e.alice, "/runners/mac-2/undrain", url.Values{"csrf": {e.bob.csrf}})
	wantStatus(t, "其他会话的 CSRF", w, http.StatusForbidden)
	mustHave(t, "页面", pageTextZH(t, w.Body.String()), "页面已过期，操作没有执行")
	wantRedirect(t, "未登录", e.post(nil, "/runners/mac-2/undrain", nil), "/login?next=%2Fruns")
	if !drain("mac-2") {
		t.Error("被拒绝的操作不应改变 mac-2")
	}
}

var enrollTokenRe = regexp.MustCompile(`--token (\S+)`)

// enrollToken 从注册展示页的页面数据中取出令牌：注册命令中的令牌，没有时返回空字符串。
// 命令中的令牌必须与 Reveal.Token 相同。
func enrollToken(t *testing.T, body string) string {
	t.Helper()
	p := pageData[runnerNewData](t, body)
	var tok string
	if m := enrollTokenRe.FindStringSubmatch(p.Command); m != nil {
		tok = m[1]
	}
	if p.Reveal != nil && p.Reveal.Token != tok {
		t.Errorf("注册命令中的令牌 %q 与 Reveal.Token %q 不同", tok, p.Reveal.Token)
	}
	return tok
}

func TestPostRunnerToken(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	tokens := func() int { t.Helper(); return e.count(`SELECT count(*) FROM enrollment_tokens`) }

	// 表单校验失败：422，保留输入，不生成令牌
	w := e.post(e.alice, "/runners/token", url.Values{"pool": {"-bad"}, "name": {"ok-name"}})
	wantStatus(t, "pool 不合法", w, http.StatusUnprocessableEntity)
	p := pageData[runnerNewData](t, w.Body.String())
	mustHave(t, "页面", textZH(t, p.Form.Errors["pool"]), "pool 只能包含字母")
	expect(t, "校验失败应保留输入", p.Form, p.Form.Values["pool"] == "-bad", p.Form.Values["name"] == "ok-name", p.Form.Failed)
	w = e.post(e.alice, "/runners/token", url.Values{"name": {"has space"}})
	wantStatus(t, "名称不合法", w, http.StatusUnprocessableEntity)
	mustHave(t, "页面", textZH(t, pageData[runnerNewData](t, w.Body.String()).Form.Errors["name"]), "名称只能包含字母")
	if n := tokens(); n != 0 {
		t.Fatalf("校验失败时不应生成令牌：%d", n)
	}

	// 生成：跳转到一次性展示页；同一个表单重复提交（双击、后退后再提交）只生成一个令牌
	id := "0123456789abcdef0123456789abcdef"
	form := url.Values{"pool": {"mac"}, "name": {"mac-9"}, "request_id": {id}}
	wantRedirect(t, "生成令牌", e.post(e.alice, "/runners/token", form), "/runners/enroll/"+id)
	wantRedirect(t, "重复提交", e.post(e.alice, "/runners/token", form), "/runners/enroll/"+id)
	if n := tokens(); n != 1 {
		t.Fatalf("同一个请求 ID 应只生成一个令牌：%d", n)
	}
	var pool string
	e.must(e.db.QueryRow(e.ctx, `SELECT pool FROM enrollment_tokens`).Scan(&pool))
	if pool != "mac" {
		t.Errorf("令牌的 pool = %q，应为 mac", pool)
	}
	if a := e.audits("runner.token_create"); len(a) != 1 || a[0].Actor != "alice" || a[0].Detail["pool"] != "mac" {
		t.Errorf("生成令牌的审计 = %+v", a)
	}

	// 其他用户打开这个地址：找不到，也不会消耗 alice 的一次性展示
	w = e.get(e.login("carol"), "/runners/enroll/"+id)
	wantStatus(t, "其他管理员", w, http.StatusNotFound)
	if p := pageData[runnerNewData](t, w.Body.String()); !p.Missing || p.Reveal != nil {
		t.Errorf("其他管理员打开：%+v", p)
	}

	w = e.get(e.alice, "/runners/enroll/"+id)
	wantStatus(t, "第一次打开", w, http.StatusOK)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("展示页的 Cache-Control = %q，应为 no-store", got)
	}
	body := w.Body.String()
	tok := enrollToken(t, body)
	if tok == "" {
		t.Fatalf("第一次打开应显示令牌：\n%s", body)
	}
	if n := e.count(`SELECT count(*) FROM enrollment_tokens WHERE token_hash=$1`, auth.HashEnrollToken(tok)); n != 1 {
		t.Error("显示的令牌应与数据库中的哈希对应")
	}
	p = pageData[runnerNewData](t, body)
	expect(t, "注册命令", p.Command, strings.Contains(p.Command, "--server https://kiln.example.com"), strings.Contains(p.Command, "--name mac-9"),
		p.Reveal != nil && p.Reveal.Status == enrollUnused)

	w = e.get(e.alice, "/runners/enroll/"+id)
	wantStatus(t, "再次打开", w, http.StatusOK)
	body = w.Body.String()
	if strings.Contains(body, tok) || enrollToken(t, body) != "" {
		t.Error("再次打开不应显示令牌")
	}
	p = pageData[runnerNewData](t, body)
	expect(t, "再次打开", p.Reveal, p.Reveal != nil && p.Reveal.Status == enrollUnused && p.Reveal.Name == "mac-9")

	// 令牌被使用后显示使用它的 Runner
	runner := e.addRunner("mac-9", runnerOpts{pool: "mac"})
	e.exec(`UPDATE enrollment_tokens SET used_at=now(), runner_id=$1`, runner)
	v := pageData[runnerNewData](t, e.get(e.alice, "/runners/enroll/"+id).Body.String()).Reveal
	expect(t, "令牌被使用后", v, v != nil && v.Status == enrollUsed && v.Runner != nil && v.Runner.Name == "mac-9")
	// 过期没有使用
	id2 := "fedcba9876543210fedcba9876543210"
	wantRedirect(t, "再生成一个", e.post(e.alice, "/runners/token", url.Values{"request_id": {id2}}), "/runners/enroll/"+id2)
	e.exec(`UPDATE enrollment_tokens SET expires_at=now() - interval '1 minute' WHERE used_at IS NULL`)
	body = e.get(e.alice, "/runners/enroll/"+id2).Body.String()
	v = pageData[runnerNewData](t, body).Reveal
	expect(t, "过期没有使用", v, v != nil && v.Status == enrollExpired && v.Pool == "default")
	if enrollToken(t, body) != "" {
		t.Error("过期的令牌不应显示")
	}
	// 过期的令牌被保留策略删除后（展示页的条目还在内存中）：仍按已过期显示，不是服务器错误
	id3 := "00112233445566778899aabbccddeeff"
	wantRedirect(t, "再生成一个", e.post(e.alice, "/runners/token", url.Values{"request_id": {id3}}), "/runners/enroll/"+id3)
	e.exec(`DELETE FROM enrollment_tokens WHERE used_at IS NULL AND expires_at > now()`)
	w = e.get(e.alice, "/runners/enroll/"+id3)
	wantStatus(t, "令牌已被清理", w, http.StatusOK)
	v = pageData[runnerNewData](t, w.Body.String()).Reveal
	expect(t, "令牌已被清理", v, v != nil && v.Status == enrollExpired && v.Runner == nil)
	if enrollToken(t, w.Body.String()) != "" {
		t.Error("已清理的令牌不应显示")
	}

	// 地址中的 ID 格式不对、成员：拒绝
	wantStatus(t, "格式不对的 ID", e.get(e.alice, "/runners/enroll/xyz"), http.StatusNotFound)
	wantStatus(t, "成员生成令牌", e.post(e.bob, "/runners/token", url.Values{}), http.StatusForbidden)
	wantStatus(t, "成员打开展示页", e.get(e.bob, "/runners/enroll/"+id), http.StatusForbidden)
	if n := tokens(); n != 2 {
		t.Errorf("被拒绝的请求不应生成令牌：%d", n)
	}
}

func TestPageRunnerNew(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.addRunner("linux-1", runnerOpts{pool: "linux"})
	w := e.get(e.alice, "/runners/new?pool=linux&name=old-1")
	wantStatus(t, "注册页面", w, http.StatusOK)
	// 预填地址中的 pool 和名称；pool 的提示包含已有的 pool 和 default
	if p := pageData[runnerNewData](t, w.Body.String()); p.Form.Values["pool"] != "linux" || p.Form.Values["name"] != "old-1" || !slices.Equal(p.Pools, []string{"default", "linux"}) {
		t.Errorf("注册页面：%+v %v", p.Form, p.Pools)
	}
	if p := pageData[runnerNewData](t, e.get(e.alice, "/runners/new?name=-bad").Body.String()); p.Form.Values["name"] == "-bad" {
		t.Error("不合法的名称不应预填")
	}
	// 重新注册 AI 执行节点时预填类型；无效的类型不预填
	if p := pageData[runnerNewData](t, e.get(e.alice, "/runners/new?name=mac-03&kind=ai-dev").Body.String()); p.Form.Values["kind"] != formKindAIDev {
		t.Errorf("类型应预填为 ai-dev：%+v", p.Form)
	}
	if p := pageData[runnerNewData](t, e.get(e.alice, "/runners/new?kind=gpu").Body.String()); p.Form.Values["kind"] != "" {
		t.Errorf("无效的类型不应预填：%+v", p.Form)
	}
	wantStatus(t, "成员", e.get(e.bob, "/runners/new"), http.StatusForbidden)
}

// TestPostRunnerTokenRole：表单的 Runner 类型决定令牌的类型和 AI 执行节点的角色（PLAN #26.7）；无效的取值不生成令牌。
func TestPostRunnerTokenRole(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	w := e.post(e.alice, "/runners/token", url.Values{"kind": {"ai-admin"}})
	wantStatus(t, "类型无效", w, http.StatusUnprocessableEntity)
	mustHave(t, "页面", textZH(t, pageData[runnerNewData](t, w.Body.String()).Form.Errors["kind"]), "AI 开发节点")
	if n := e.count(`SELECT count(*) FROM enrollment_tokens`); n != 0 {
		t.Fatalf("类型无效时不应生成令牌：%d", n)
	}
	for i, tc := range []struct{ form, kind, role string }{
		{"", store.RunnerKindCI, ""},
		{"ai", store.RunnerKindAI, store.RunnerRoleReview},
		{"ai-dev", store.RunnerKindAI, store.RunnerRoleDevelopment},
	} {
		id := fmt.Sprintf("%032x", i+1)
		wantRedirect(t, tc.form, e.post(e.alice, "/runners/token", url.Values{"kind": {tc.form}, "request_id": {id}}), "/runners/enroll/"+id)
		v := pageData[runnerNewData](t, e.get(e.alice, "/runners/enroll/"+id).Body.String()).Reveal
		if v == nil || v.Kind != tc.kind || v.Role != tc.role {
			t.Fatalf("%q：展示页 = %+v，应为 %s/%s", tc.form, v, tc.kind, tc.role)
		}
		var kind, role string
		e.must(e.db.QueryRow(e.ctx, `SELECT kind, role FROM enrollment_tokens ORDER BY id DESC LIMIT 1`).Scan(&kind, &role))
		if kind != tc.kind || role != tc.role {
			t.Errorf("%q：令牌 = %s/%s，应为 %s/%s", tc.form, kind, role, tc.kind, tc.role)
		}
	}
}

// textOptionLabels 返回选项的文字。
func textOptionLabels(opts []textOption) []uiText {
	out := make([]uiText, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Label)
	}
	return out
}

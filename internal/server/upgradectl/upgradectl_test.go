package upgradectl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/pgtest"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/upgrade"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t: t, ctx: t.Context(), db: store.New(pgtest.Pool(t))}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

// addRunner 插入一台在线的 Runner，返回 ID。
func (e *env) addRunner(name, kind, version string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO runners (name, kind, public_key, version, last_seen_at)
		VALUES ($1, $2, '\x00', $3, now()) RETURNING id`, name, kind, version).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// acquire 以 Runner 的身份上报 version 和 st，返回要下发的内容。
func (e *env) acquire(id int64, name, version string, st *runnerv1.UpgradeStatus) *Result {
	e.t.Helper()
	res, err := Acquire(e.ctx, e.db, AcquireRunner{ID: id, Name: name, Kind: store.RunnerKindCI, Version: version, Status: st})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// status 返回经过启动器运行、查到 latest 的 Runner 上报的状态。
func status(state, latest string) *runnerv1.UpgradeStatus {
	return &runnerv1.UpgradeStatus{State: state, Launcher: true, Latest: latest, Package: upgrade.PackageName(upgrade.ProgramCI, "darwin", "arm64")}
}

// request 请求升级，期望成功。
func (e *env) request(name string) (int64, string, bool) {
	e.t.Helper()
	req, created, err := RequestUpgrade(e.ctx, e.db, name, "", "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	return req.Id, req.Version, created
}

func (e *env) requestState(id int64) (state, msg string) {
	e.t.Helper()
	if err := e.db.QueryRow(e.ctx, `SELECT state, error FROM runner_upgrade_requests WHERE id=$1`, id).Scan(&state, &msg); err != nil {
		e.t.Fatal(err)
	}
	return state, msg
}

func (e *env) audits(action string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action=$1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestRequestUpgrade（PLAN #27.13"创建"）：目标是 Runner 最后一次上报的 latest，之后 latest 变了也不变；只下发给
// 被请求的 Runner；重复请求返回同一个；新版本确认后成功。
func TestRequestUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addRunner("mac-a", store.RunnerKindCI, "v0.1.40")
	b := e.addRunner("mac-b", store.RunnerKindCI, "v0.1.40")
	e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42"))
	e.acquire(b, "mac-b", "v0.1.40", status(StateIdle, "0.1.42"))
	if res := e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42")); res.Directive != nil {
		t.Fatalf("发现新版本本身不应触发升级：%+v", res.Directive)
	}
	id, version, created := e.request("mac-a")
	if !created || version != "0.1.42" || id < 1000 {
		t.Fatalf("请求：id %d version %s created %v", id, version, created)
	}
	if again, _, created := e.request("mac-a"); again != id || created {
		t.Fatalf("重复请求应返回同一个：%d %v", again, created)
	}
	// npm 上又发布了 0.1.43：这次请求的目标不变
	res := e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.43"))
	if d := res.Directive; d == nil || d.Version != "0.1.42" || d.Generation != id {
		t.Fatalf("应下发请求时固定的版本和请求的 ID：%+v", d)
	}
	if res := e.acquire(b, "mac-b", "v0.1.40", status(StateIdle, "0.1.43")); res.Directive != nil {
		t.Fatalf("没有被请求的 Runner 不应收到指令：%+v", res.Directive)
	}
	// 管理员确认的是 0.1.42，期间 mac-b 查到了 0.1.43：拒绝，不升级到没有确认过的版本
	_, _, err := RequestUpgrade(e.ctx, e.db, "mac-b", "v0.1.42", "admin")
	if re, ok := errors.AsType[*RefusedError](err); !ok || !strings.Contains(err.Error(), "最新版本是 0.1.43") ||
		re.Reason != (Reason{Code: ReasonLatestChanged, Latest: "0.1.43", Expected: "0.1.42"}) {
		t.Fatalf("确认的版本与 latest 不同时应拒绝：%v", err)
	}
	if req, created, err := RequestUpgrade(e.ctx, e.db, "mac-b", "v0.1.43", "admin"); err != nil || !created || req.Version != "0.1.43" {
		t.Fatalf("确认的版本与 latest 相同时创建：%+v %v %v", req, created, err)
	}
	// 下载、排空中、试用中（switching，已经是新版本但还没确认）都不算结束
	for _, st := range []string{StateDownloading, StateDraining} {
		e.acquire(a, "mac-a", "v0.1.40", &runnerv1.UpgradeStatus{State: st, Target: "0.1.42", Generation: id, Launcher: true})
	}
	e.acquire(a, "mac-a", "v0.1.42", &runnerv1.UpgradeStatus{State: StateSwitching, Target: "0.1.42", Launcher: true})
	if s, _ := e.requestState(id); s != RequestPending {
		t.Fatalf("确认之前不应结束：%s", s)
	}
	res = e.acquire(a, "mac-a", "v0.1.42", &runnerv1.UpgradeStatus{State: StateConfirmed, Target: "0.1.42", Launcher: true})
	if s, _ := e.requestState(id); s != RequestSucceeded || res.Directive != nil {
		t.Fatalf("新版本确认后应成功、不再下发：%s %+v", s, res.Directive)
	}
	if e.audits("runner.upgrade.requested") != 2 || e.audits("runner.upgrade.succeeded") != 1 {
		t.Fatal("审计日志")
	}
}

// TestRequestUpgradeConcurrent：并发的点击只生成一个进行中的请求。
func TestRequestUpgradeConcurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addRunner("mac-a", store.RunnerKindCI, "v0.1.40")
	e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42"))
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[int64]bool{}
		created int
	)
	for range 8 {
		wg.Go(func() {
			req, c, err := RequestUpgrade(e.ctx, e.db, "mac-a", "", "admin")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[req.Id] = true
			if c {
				created++
			}
		})
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		t.Fatalf("应只有一个请求：%v，创建了 %d 次", ids, created)
	}
}

// TestRequestUpgradeRefused：不能升级时说明原因，不创建请求。
func TestRequestUpgradeRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cases := []struct {
		name, version string
		st            *runnerv1.UpgradeStatus
		want          string
		reason        Reason
	}{
		{"旧版本 Runner", "v0.1.40", nil, "service install", Reason{Code: ReasonUnsupported}},
		{"前台运行", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Latest: "0.1.42"}, "service install", Reason{Code: ReasonNoLauncher}},
		{"本地关闭", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Launcher: true, Disabled: true, Latest: "0.1.42"}, "upgrade = false", Reason{Code: ReasonDisabled}},
		{"还没有查到", "v0.1.40", status(StateIdle, ""), "还没有查到最新版本", Reason{Code: ReasonNotChecked}},
		{
			"查询失败", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Launcher: true, CheckError: "npm 上没有这个包"}, "npm 上没有这个包",
			Reason{Code: ReasonCheckFailed, Detail: "npm 上没有这个包"},
		},
		{"已是最新", "v0.1.42", status(StateIdle, "0.1.42"), "已是最新", Reason{Code: ReasonUpToDate, Current: "v0.1.42", Latest: "0.1.42"}},
		{"当前更高", "v0.1.43", status(StateIdle, "0.1.42"), "不降级", Reason{Code: ReasonNewer, Current: "v0.1.43", Latest: "0.1.42"}},
	}
	for i, tc := range cases {
		name := "r" + string(rune('a'+i))
		id := e.addRunner(name, store.RunnerKindCI, tc.version)
		e.acquire(id, name, tc.version, tc.st)
		_, _, err := RequestUpgrade(e.ctx, e.db, name, "", "admin")
		re, ok := errors.AsType[*RefusedError](err)
		if !ok || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：应拒绝并说明 %q：%v", tc.name, tc.want, err)
			continue
		}
		// Dashboard 按原因码显示：原因码和原始参数与 CLI 的说明来自同一个判断
		if re.Reason != tc.reason || !strings.HasPrefix(re.Msg, name+"：") || !strings.HasSuffix(re.Msg, tc.reason.String()) {
			t.Errorf("%s：原因 %+v，说明 %q，应为 %+v", tc.name, re.Reason, re.Msg, tc.reason)
		}
	}
	revoked := e.addRunner("gone", store.RunnerKindCI, "v0.1.40")
	e.acquire(revoked, "gone", "v0.1.40", status(StateIdle, "0.1.42"))
	e.exec(`UPDATE runners SET state='revoked' WHERE id=$1`, revoked)
	if _, _, err := RequestUpgrade(e.ctx, e.db, "gone", "", "admin"); err == nil || !strings.Contains(err.Error(), "吊销") ||
		reasonCode(err) != ReasonRevoked {
		t.Errorf("已吊销：%v", err)
	}
	if _, _, err := RequestUpgrade(e.ctx, e.db, "nobody", "", "admin"); !errors.Is(err, ErrRunnerNotFound) {
		t.Errorf("不存在：%v", err)
	}
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM runner_upgrade_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("不应创建请求：%d %v", n, err)
	}
	// 开发构建可以升级到合法的 latest
	dev := e.addRunner("dev", store.RunnerKindCI, "dev-abc123")
	e.acquire(dev, "dev", "dev-abc123", status(StateIdle, "0.1.42"))
	if _, version, _ := e.request("dev"); version != "0.1.42" {
		t.Fatalf("开发构建：%s", version)
	}
}

// TestRequestFails（PLAN #27.13"结束"）：这一轮的失败、回滚、需要手动升级结束请求并记下原因；之前的请求留下的
// failed、rolled_back 不会结束新的请求；重试生成更大的 generation。
func TestRequestFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addRunner("mac-a", store.RunnerKindCI, "v0.1.40")
	e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42"))
	first, _, _ := e.request("mac-a")
	// 下载失败
	fail := &runnerv1.UpgradeStatus{State: StateFailed, Target: "0.1.42", Generation: first, Error: "下载失败：503", Launcher: true, Latest: "0.1.42"}
	if res := e.acquire(a, "mac-a", "v0.1.40", fail); res.Directive != nil {
		t.Fatalf("失败后不应再下发：%+v", res.Directive)
	}
	if s, msg := e.requestState(first); s != RequestFailed || msg != "下载失败：503" {
		t.Fatalf("应失败并记下原因：%s %q", s, msg)
	}
	// 重试：新请求的 generation 更大；Runner 仍上报上一轮的 failed，不会结束新的请求
	second, _, created := e.request("mac-a")
	if !created || second <= first {
		t.Fatalf("重试应生成新的请求：%d（之前 %d）", second, first)
	}
	if res := e.acquire(a, "mac-a", "v0.1.40", fail); res.Directive == nil || res.Directive.Generation != second {
		t.Fatalf("上一轮的失败不应结束新的请求：%+v", res.Directive)
	}
	// 回滚：之前的失败列表中只有更早的 generation 时不算
	rolled := &runnerv1.UpgradeStatus{
		State: StateRolledBack, Target: "0.1.42", Error: "启动 3 次没有确认", Launcher: true, Latest: "0.1.42",
		Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: first}},
	}
	e.acquire(a, "mac-a", "v0.1.40", rolled)
	if s, _ := e.requestState(second); s != RequestPending {
		t.Fatalf("之前的回滚不应结束新的请求：%s", s)
	}
	rolled.Failed = append(rolled.Failed, &runnerv1.FailedVersion{Version: "0.1.42", Generation: second})
	e.acquire(a, "mac-a", "v0.1.40", rolled)
	if s, msg := e.requestState(second); s != RequestFailed || !strings.Contains(msg, "回滚") || !strings.Contains(msg, "启动 3 次") {
		t.Fatalf("这一轮回滚应结束请求：%s %q", s, msg)
	}
	// 需要手动升级
	third, _, _ := e.request("mac-a")
	e.acquire(a, "mac-a", "v0.1.40", &runnerv1.UpgradeStatus{State: StateManual, Target: "0.1.42", Error: "config.toml 中 upgrade = false", Launcher: true, Latest: "0.1.42"})
	if s, msg := e.requestState(third); s != RequestFailed || !strings.Contains(msg, "upgrade = false") {
		t.Fatalf("需要手动升级应结束请求：%s %q", s, msg)
	}
	// 期间手动升级到了更高的版本：取消，不降级
	fourth, _, _ := e.request("mac-a")
	e.acquire(a, "mac-a", "v0.1.50", status(StateIdle, "0.1.50"))
	if s, msg := e.requestState(fourth); s != RequestCancelled || !strings.Contains(msg, "0.1.50") {
		t.Fatalf("已经是更高的版本应取消：%s %q", s, msg)
	}
	// 核对不符记入失败列表，上报的 failed 在 Server 收到之前已经变回 idle（退避到期、重启）：按失败记录结束，
	// 之后可以重新请求（不会一直停在 pending）
	if _, err := e.db.Exec(e.ctx, `UPDATE runners SET version='v0.1.50' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	e.acquire(a, "mac-a", "v0.1.50", status(StateIdle, "0.1.52"))
	fifth, target, _ := e.request("mac-a")
	if target != "0.1.52" {
		t.Fatalf("目标 = %s", target)
	}
	late := &runnerv1.UpgradeStatus{
		State: StateIdle, Launcher: true, Latest: "0.1.52",
		Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: second}, {Version: "0.1.52", Generation: fifth}},
	}
	if res := e.acquire(a, "mac-a", "v0.1.50", late); res.Directive != nil {
		t.Fatalf("失败记录结束请求后不应再下发：%+v", res.Directive)
	}
	if s, msg := e.requestState(fifth); s != RequestFailed || !strings.Contains(msg, "不会再尝试") {
		t.Fatalf("这一轮的失败记录应结束请求：%s %q", s, msg)
	}
	if sixth, _, created := e.request("mac-a"); !created || sixth <= fifth {
		t.Fatalf("失败后应能重新请求：%d %v", sixth, created)
	}
	if e.audits("runner.upgrade.failed") != 4 {
		t.Fatalf("审计 runner.upgrade.failed：%d", e.audits("runner.upgrade.failed"))
	}
}

// TestCancelUpgrade：取消进行中的请求后不再下发；已经切换到新版本、正在确认时不能取消；吊销时取消。
func TestCancelUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addRunner("mac-a", store.RunnerKindCI, "v0.1.40")
	e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42"))
	if _, err := CancelUpgrade(e.ctx, e.db, "mac-a", "admin"); err == nil || reasonCode(err) != ReasonNoPending {
		t.Fatalf("没有进行中的请求时应拒绝：%v", err)
	}
	id, _, _ := e.request("mac-a")
	e.acquire(a, "mac-a", "v0.1.42", &runnerv1.UpgradeStatus{State: StateSwitching, Target: "0.1.42", Launcher: true})
	if _, err := CancelUpgrade(e.ctx, e.db, "mac-a", "admin"); err == nil || !strings.Contains(err.Error(), "正在确认") ||
		reasonCode(err) != ReasonSwitching {
		t.Fatalf("切换中不应取消：%v", err)
	}
	e.acquire(a, "mac-a", "v0.1.40", &runnerv1.UpgradeStatus{State: StateDraining, Target: "0.1.42", Generation: id, Launcher: true, Latest: "0.1.42"})
	req, err := CancelUpgrade(e.ctx, e.db, "mac-a", "admin")
	if err != nil || req.State != RequestCancelled || req.FinishedAt == 0 {
		t.Fatalf("排空中可以取消：%+v %v", req, err)
	}
	if res, _ := Refresh(e.ctx, e.db, a); res.Directive != nil {
		t.Fatalf("取消后不应下发：%+v", res.Directive)
	}
	id, _, _ = e.request("mac-a")
	if err := e.db.Tx(e.ctx, func(tx pgx.Tx) error { return CancelForRevoke(e.ctx, tx, a, "mac-a", "admin") }); err != nil {
		t.Fatal(err)
	}
	if s, msg := e.requestState(id); s != RequestCancelled || msg != "Runner 已吊销" {
		t.Fatalf("吊销时应取消：%s %q", s, msg)
	}
}

// TestRegistryAndCheck：registry 设置随指令下发（空表示官方）；检查序号按台或全部增加，Runner 处理之前显示"检查中"。
func TestRegistryAndCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addRunner("mac-a", store.RunnerKindCI, "v0.1.40")
	b := e.addRunner("mac-b", store.RunnerKindAI, "v0.1.40")
	if res := e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42")); res.Registry != "" || res.Check != 0 {
		t.Fatalf("默认：%+v", res)
	}
	if err := SetRegistry(e.ctx, e.db, "http://example.com/npm/", "admin"); err == nil {
		t.Fatal("非本机地址不应允许 http")
	}
	if err := SetRegistry(e.ctx, e.db, " https://npm.example/repo/ ", "admin"); err != nil {
		t.Fatal(err)
	}
	if ov, _ := Overview(e.ctx, e.db); ov.Registry != "https://npm.example/repo/" || ov.DefaultRegistry != upgrade.DefaultRegistry {
		t.Fatalf("Overview：%+v", ov)
	}
	if n, err := RequestCheck(e.ctx, e.db, "mac-a", "admin"); err != nil || n != 1 {
		t.Fatalf("按台检查：%d %v", n, err)
	}
	res := e.acquire(a, "mac-a", "v0.1.40", status(StateIdle, "0.1.42"))
	if res.Registry != "https://npm.example/repo/" || res.Check != 1 {
		t.Fatalf("应下发 registry 和检查序号：%+v", res)
	}
	infos, err := RunnerInfos(e.ctx, e.db, []int64{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !infos[a].Info.CheckPending || !infos[a].Info.CanUpgrade || infos[a].Info.Latest != "0.1.42" || !infos[a].Reason.IsZero() {
		t.Fatalf("mac-a：%+v", infos[a])
	}
	if infos[b].Info.CanUpgrade || !strings.Contains(infos[b].Info.Reason, "没有按台升级") || infos[b].Reason.Code != ReasonAINode {
		t.Fatalf("AI 执行节点不按台升级（PLAN #33.3）：%+v", infos[b])
	}
	st := status(StateIdle, "0.1.42")
	st.CheckSeq = 1
	e.acquire(a, "mac-a", "v0.1.40", st)
	if n, err := RequestCheck(e.ctx, e.db, "", "admin"); err != nil || n != 2 {
		t.Fatalf("全部检查：%d %v", n, err)
	}
	e.request("mac-a")
	infos, _ = RunnerInfos(e.ctx, e.db, []int64{a})
	if infos[a].Info.CanUpgrade || infos[a].Info.Request.GetState() != RequestPending || !strings.Contains(infos[a].Info.Reason, "正在升级到 0.1.42") ||
		infos[a].Reason != (Reason{Code: ReasonUpgrading, Version: "0.1.42"}) {
		t.Fatalf("进行中：%+v", infos[a])
	}
	if err := SetRegistry(e.ctx, e.db, "", "admin"); err != nil {
		t.Fatal(err)
	}
	if res, _ := Refresh(e.ctx, e.db, a); res.Registry != "" || res.Check != 2 {
		t.Fatalf("恢复为官方 registry：%+v", res)
	}
}

// TestSettle 覆盖 settle 的各个分支。
func TestSettle(t *testing.T) {
	t.Parallel()
	const id = 1001
	for _, tc := range []struct {
		name, current string
		st            *runnerv1.UpgradeStatus
		want          string
	}{
		{"确认", "v0.1.42", &runnerv1.UpgradeStatus{State: StateConfirmed}, RequestSucceeded},
		{"手动换成了目标版本", "v0.1.42", &runnerv1.UpgradeStatus{State: StateIdle}, RequestSucceeded},
		{"试用中", "v0.1.42", &runnerv1.UpgradeStatus{State: StateSwitching, Target: "0.1.42"}, ""},
		{"更高的版本", "v0.2.0", &runnerv1.UpgradeStatus{State: StateIdle}, RequestCancelled},
		{"这一轮失败", "v0.1.40", &runnerv1.UpgradeStatus{State: StateFailed, Target: "0.1.42", Generation: id}, RequestFailed},
		{"之前的失败", "v0.1.40", &runnerv1.UpgradeStatus{State: StateFailed, Target: "0.1.42", Generation: id - 1}, ""},
		{"其他版本的失败", "v0.1.40", &runnerv1.UpgradeStatus{State: StateFailed, Target: "0.1.41", Generation: id}, ""},
		{"下载中", "v0.1.40", &runnerv1.UpgradeStatus{State: StateDownloading, Target: "0.1.42", Generation: id}, ""},
		{"开发构建", "dev-abc", &runnerv1.UpgradeStatus{State: StateIdle}, ""},
		{"回滚", "v0.1.40", &runnerv1.UpgradeStatus{State: StateRolledBack, Target: "0.1.42", Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: id}}}, RequestFailed},
		{"之前的回滚", "v0.1.40", &runnerv1.UpgradeStatus{State: StateRolledBack, Target: "0.1.42", Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: id - 1}}}, ""},
		// failed 在 Server 收到之前已经变回 idle（退避到期、重启），失败列表中仍有这一轮的记录
		{"失败记录", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: id}}}, RequestFailed},
		{"之前的失败记录", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Failed: []*runnerv1.FailedVersion{{Version: "0.1.42", Generation: id - 1}}}, ""},
		{"其他版本的失败记录", "v0.1.40", &runnerv1.UpgradeStatus{State: StateIdle, Failed: []*runnerv1.FailedVersion{{Version: "0.1.41", Generation: id}}}, ""},
	} {
		if got, _ := settle(id, "0.1.42", tc.current, tc.st); got != tc.want {
			t.Errorf("%s：%q，想要 %q", tc.name, got, tc.want)
		}
	}
}

// reasonCode 返回 *RefusedError 的原因码（不是时为空）。
func reasonCode(err error) ReasonCode {
	if re, ok := errors.AsType[*RefusedError](err); ok {
		return re.Reason.Code
	}
	return ""
}

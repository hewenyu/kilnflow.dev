package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/shorten"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/ci/planner"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// run、job 的取消和重跑，以及仓库的刷新和补对账（Dashboard 的表单操作）。

// dispatch 手动触发 build.yml（需要 fakegithub），返回 run ID。
func (e *webEnv) dispatch() int64 {
	e.t.Helper()
	id, err := e.svc.Dispatch(e.ctx, &admin.DispatchRequest{
		Repo: testRepo, Workflow: "build", Inputs: map[string]string{"channel": "beta"}, Actor: "setup", Source: planner.SourceDashboard,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// runState 返回 run 的状态和当前的 rerun 序号。
func (e *webEnv) runState(id int64) (store.RunState, int) {
	e.t.Helper()
	var state store.RunState
	var rerun int
	e.must(e.db.QueryRow(e.ctx, `SELECT state, current_rerun FROM runs WHERE id=$1`, id).Scan(&state, &rerun))
	return state, rerun
}

// jobOf 返回 run 第 rerun 次运行中 job_key 为 key 的 job。
func (e *webEnv) jobOf(runID int64, rerun int, key string) int64 {
	e.t.Helper()
	var id int64
	e.must(e.db.QueryRow(e.ctx, `SELECT id FROM jobs WHERE run_id=$1 AND rerun=$2 AND job_key=$3`, runID, rerun, key).Scan(&id))
	return id
}

func TestPostCancelAndRerun(t *testing.T) {
	t.Parallel()
	e := newGitHubWebEnv(t)
	id := e.dispatch()
	runPath := fmt.Sprintf("/runs/%d", id)

	// 还没有结束的 run 不能重跑；成员也可以取消和重跑
	flash := flashZH(t, wantRedirect(t, "重跑运行中的 run", e.post(e.bob, runPath+"/rerun", url.Values{"mode": {"all"}}), runPath))
	mustHave(t, "提示", flash, "重跑失败：", "还没有结束")
	flash = flashZH(t, wantRedirect(t, "取消", e.post(e.bob, runPath+"/cancel", nil), runPath))
	if flash != "已请求取消" {
		t.Errorf("取消的提示 = %q", flash)
	}
	if state, _ := e.runState(id); state != store.RunCancelled {
		t.Fatalf("取消后 run 的状态 = %s", state)
	}
	if a := e.audits("run.cancel"); len(a) != 1 || a[0].Actor != "bob" {
		t.Errorf("取消的审计 = %+v，应记录 bob", a)
	}

	// 重跑整个 run：跳到新的一次运行
	flash = flashZH(t, wantRedirect(t, "重跑", e.post(e.bob, runPath+"/rerun", url.Values{"mode": {"all"}, "keep": {"on"}}), runPath+"?rerun=1"))
	if flash != "已开始第 1 次重跑" {
		t.Errorf("重跑的提示 = %q", flash)
	}
	if state, rerun := e.runState(id); rerun != 1 || state.Terminal() {
		t.Errorf("重跑后 run = %s、rerun %d", state, rerun)
	}
	if n := e.count(`SELECT count(*) FROM run_reruns WHERE run_id=$1 AND rerun=1 AND keep_on_failure`, id); n != 1 {
		t.Error("应记录失败时保留现场")
	}
	// 重跑一个 job：同样跳到新的一次运行
	wantRedirect(t, "再次取消", e.post(e.alice, runPath+"/cancel", nil), runPath)
	build := e.jobOf(id, 1, "build")
	flash = flashZH(t, wantRedirect(t, "重跑 job", e.post(e.alice, fmt.Sprintf("/jobs/%d/rerun", build), nil), runPath+"?rerun=2"))
	if flash != "已开始第 2 次重跑" {
		t.Errorf("重跑 job 的提示 = %q", flash)
	}
	// run 运行中时重跑 job：回到 job 页面并说明
	build2 := e.jobOf(id, 2, "build")
	flash = flashZH(t, wantRedirect(t, "运行中重跑 job", e.post(e.alice, fmt.Sprintf("/jobs/%d/rerun", build2), nil), fmt.Sprintf("/jobs/%d", build2)))
	mustHave(t, "提示", flash, "重跑失败：")

	// 不存在的 run、job
	missing := fmt.Sprintf("/runs/%d", id+1000)
	flash = flashZH(t, wantRedirect(t, "取消不存在的 run", e.post(e.alice, missing+"/cancel", nil), missing))
	mustHave(t, "提示", flash, "取消失败：", fmt.Sprintf("没有 run #%d", id+1000))
	flash = flashZH(t, wantRedirect(t, "重跑不存在的 run", e.post(e.alice, missing+"/rerun", nil), missing))
	mustHave(t, "提示", flash, "重跑失败：", fmt.Sprintf("没有 run #%d", id+1000))
	w := e.post(e.alice, fmt.Sprintf("/jobs/%d/rerun", build+1000), nil)
	wantErrorPage(t, "重跑不存在的 job", w, http.StatusNotFound, []string{fmt.Sprintf("没有编号为 %d 的 job", build+1000)}, []string{"/runs"})

	// CSRF 不对：不执行
	before := e.count(`SELECT count(*) FROM run_reruns WHERE run_id=$1`, id)
	wantStatus(t, "CSRF 不对", e.post(e.alice, runPath+"/cancel", url.Values{"csrf": {"bad"}}), http.StatusForbidden)
	if state, _ := e.runState(id); state == store.RunCancelled {
		t.Error("CSRF 校验失败时不应取消")
	}
	if n := e.count(`SELECT count(*) FROM run_reruns WHERE run_id=$1`, id); n != before {
		t.Error("CSRF 校验失败时不应改变 run")
	}
}

func TestPostSyncAndReconcile(t *testing.T) {
	t.Parallel()
	e := newGitHubWebEnv(t)
	back := url.Values{"q": {"demo"}, "page": {"1"}}

	// 刷新仓库：回到仓库页（保留搜索词），页面记录最近一次刷新
	flash := wantRedirect(t, "刷新仓库", e.post(e.alice, "/sync", back), "/settings/repos?q=demo")
	if flashZH(t, flash) != "已刷新仓库：1 个 installation，1 个启用中的仓库。" {
		t.Errorf("刷新仓库的提示 = %q", flash)
	}
	// store.SyncStatus 按它的 json 标签编码（小写）
	sync := pageData[struct{ Sync *store.SyncStatus }](t, e.get(e.alice, "/settings/repos").Body.String()).Sync
	if sync == nil || sync.Actor != "alice" || sync.Installations != 1 || sync.Repos != 1 || sync.Error != "" || sync.At.IsZero() {
		t.Errorf("仓库页的最近一次刷新：%+v", sync)
	}

	// 补对账：第一次只记录基线
	form := url.Values{"repo": {testRepo}, "q": {"demo"}, "page": {"2"}}
	flash = wantRedirect(t, "第一次补对账", e.post(e.alice, "/settings/repos/reconcile", form), "/settings/repos?page=2&q=demo")
	mustHave(t, "提示", flashZH(t, flash), "tester/demo 补对账完成：1 个分支、0 个 tag、0 个打开的 PR，补上 0 个 run", "这是第一次对账")
	// 推送了新的提交而 webhook 丢失：补上 push 的 run
	if err := os.WriteFile(filepath.Join(e.repoDir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, e.repoDir, "commit", "-q", "-am", "change")
	flash = wantRedirect(t, "第二次补对账", e.post(e.alice, "/settings/repos/reconcile", url.Values{"repo": {testRepo}}), "/settings/repos")
	if flashZH(t, flash) != "tester/demo 补对账完成：1 个分支、0 个 tag、0 个打开的 PR，补上 1 个 run。" {
		t.Errorf("第二次补对账的提示 = %q", flash)
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE source='reconcile'`); n != 1 {
		t.Errorf("应补上 1 个 run：%d", n)
	}
	flash = wantRedirect(t, "补对账不存在的仓库", e.post(e.alice, "/settings/repos/reconcile", url.Values{"repo": {"tester/nope"}}), "/settings/repos")
	mustHave(t, "提示", flashZH(t, flash), "仓库 tester/nope 不在 Kiln 中")
	if a := e.audits("repo.reconcile"); len(a) != 2 || a[0].Actor != "alice" {
		t.Errorf("补对账的审计 = %+v", a)
	}
}

func TestPostSyncWithoutGitHub(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	flash := wantRedirect(t, "没有连接 GitHub 时刷新仓库", e.post(e.alice, "/sync", nil), "/settings/repos")
	mustHave(t, "提示", flashZH(t, flash), "刷新仓库失败：")
	flash = wantRedirect(t, "没有连接 GitHub 时补对账", e.post(e.alice, "/settings/repos/reconcile", url.Values{"repo": {testRepo}}), "/settings/repos")
	mustHave(t, "提示", flashZH(t, flash), "tester/demo 补对账失败：")
}

// 操作失败的说明：给用户看的说明（校验失败、不存在、前提不满足）显示原文；内部错误（数据库、网络、超时、Internal）
// 只显示通用说明和错误编号，详细信息只写日志。不是 *connect.Error 的说明也写一条日志。
func TestFailure(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "42P01", Message: `relation "audit_log" does not exist`}
	tests := []struct {
		name     string
		err      error
		want     string // 给用户看的说明；为空表示应是内部错误
		warnLogs bool   // 是否写了"操作没有完成"的日志
		detail   string // 内部错误的详细信息：只在日志中
	}{
		{"校验失败", errors.New("名称只能包含字母、数字和 _"), "名称只能包含字母、数字和 _", true, ""},
		{"不存在", connect.NewError(connect.CodeNotFound, errors.New("没有 run #9")), "没有 run #9", false, ""},
		{"前提不满足", connect.NewError(connect.CodeFailedPrecondition, errors.New("run 已经结束")), "run 已经结束", false, ""},
		{"SQL 错误", fmt.Errorf("取消 run：%w", pgErr), "", false, "audit_log"},
		{"数据库连接失败", fmt.Errorf("查找仓库：%w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused 10.0.0.7:5432")}), "", false, "10.0.0.7"},
		{"超时", fmt.Errorf("读取：%w", context.DeadlineExceeded), "", false, "deadline"},
		{"行不存在", fmt.Errorf("读取 run：%w", pgx.ErrNoRows), "", false, "no rows"},
		{"Internal", connect.NewError(connect.CodeInternal, errors.New("boom 10.0.0.7")), "", false, "10.0.0.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, logs := testDashboard(t, "")
			r := httptest.NewRequest(http.MethodPost, "/runs/9/cancel", nil)
			got := textZH(t, d.failureText(r, tt.err))
			out := logs.String()
			if tt.want != "" {
				if got != tt.want {
					t.Errorf("说明 = %q，期望 %q", got, tt.want)
				}
				if warned := strings.Contains(out, "操作没有完成"); warned != tt.warnLogs || strings.Contains(out, "level=ERROR") {
					t.Errorf("日志 = %s，期望写了说明 %v、没有错误", out, tt.warnLogs)
				}
				return
			}
			ref := regexp.MustCompile(`错误编号 ([0-9a-f]{12})`).FindStringSubmatch(got)
			if ref == nil || strings.Contains(got, tt.detail) {
				t.Fatalf("内部错误应只显示错误编号：%q", got)
			}
			if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "ref="+ref[1]) || !strings.Contains(out, "path=/runs/9/cancel") ||
				!strings.Contains(out, tt.detail) {
				t.Errorf("内部错误的详细信息应写在日志中（编号 %s）：%s", ref[1], out)
			}
		})
	}
}

// 表单操作遇到数据库错误：提示中只有错误编号，不显示 SQL 错误的原文（表名等），详细信息写日志。
func TestPostActionDatabaseError(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	id := e.addRun(runOpts{state: store.RunActive})
	// 取消要写审计日志：表不存在时事务失败
	e.exec(`ALTER TABLE audit_log RENAME TO audit_log_gone`)
	raw := wantRedirect(t, "取消", e.post(e.bob, fmt.Sprintf("/runs/%d/cancel", id), nil), fmt.Sprintf("/runs/%d", id))
	if flash := flashZH(t, raw); !strings.HasPrefix(flash, "取消失败：服务器出错了（错误编号 ") || strings.Contains(raw, "audit_log") {
		t.Errorf("提示 = %q，应只有错误编号", flash)
	}
	if out := e.log.String(); !strings.Contains(out, "操作出错") || !strings.Contains(out, "audit_log") {
		t.Errorf("日志中应有详细信息：%s", out)
	}
	if state, _ := e.runState(id); state != store.RunActive {
		t.Errorf("取消失败后 run 的状态 = %s", state)
	}
}

// 作用域配置页读取作用域时遇到数据库错误：500 错误页（不是"作用域无效"的提示，也不显示 SQL 错误的原文）。
func TestPageScopeDatabaseError(t *testing.T) {
	t.Parallel()
	e := newWebEnv(t)
	e.exec(`ALTER TABLE installations RENAME TO installations_gone`)
	w := e.get(e.alice, "/settings/scope?account="+testAccount)
	wantStatus(t, "数据库错误", w, http.StatusInternalServerError)
	if strings.Contains(w.Body.String(), "installations") {
		t.Errorf("页面中有 SQL 错误的原文：%s", w.Body.String())
	}
}

// failureText、failureFlash：给用户看的说明原样作为文字或参数（过长、带换行时整理成能放进地址的参数），
// 内部错误是带错误编号的消息（按语言显示），原文不进入页面数据或地址。
func TestFailureTextAndFlash(t *testing.T) {
	t.Parallel()
	d, _, logs := testDashboard(t, "")
	r := httptest.NewRequest(http.MethodPost, "/runs/9/cancel", nil)
	user := connect.NewError(connect.CodeFailedPrecondition, errors.New("run 已经结束"))
	if got := d.failureText(r, user); got.id != "" || got.raw != "run 已经结束" {
		t.Errorf("给用户看的说明 = %+v", got)
	}
	internal := connect.NewError(connect.CodeInternal, errors.New("boom 10.0.0.7"))
	got := d.failureText(r, internal)
	ref, _ := got.params[argRef].(string)
	if got.id != msgActionInternal || len(ref) != 12 || strings.Contains(textZH(t, got), "10.0.0.7") || !strings.Contains(logs.String(), "ref="+ref) {
		t.Errorf("内部错误 = %+v，日志 %s", got, logs.String())
	}

	// flash：参数中的换行换成空格，超长的截断，整条提示仍然合格（parseFlash 接受）
	long := connect.NewError(connect.CodeFailedPrecondition, errors.New("第一行\n"+strings.Repeat("长", 2*maxFlashArgRunes)))
	v, err := flashValue(d.failureFlash(r, long, msgRunCancelFailed, msgRunCancelFailedRef))
	if err != nil {
		t.Fatalf("过长的说明应截断后放进地址：%v", err)
	}
	shown := flashZH(t, v)
	if !strings.HasPrefix(shown, "取消失败：第一行 长") || !strings.HasSuffix(shown, shorten.Ellipsis) || strings.Contains(shown, "\n") {
		t.Errorf("提示 = %q", shown)
	}
	v, err = flashValue(d.failureFlash(r, internal, msgRunCancelFailed, msgRunCancelFailedRef))
	if err != nil || !strings.HasPrefix(flashZH(t, v), "取消失败：服务器出错了（错误编号 ") || strings.Contains(v, "10.0.0.7") {
		t.Errorf("内部错误的提示 = %q（%v）", v, err)
	}
}

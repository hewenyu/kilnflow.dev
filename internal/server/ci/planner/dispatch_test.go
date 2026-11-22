package planner

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// dispatchRepo 准备一个可以手动触发的仓库：main 上有 manual.yml（只能手动触发）和 ev.yml（不能手动触发），
// 分支 dev 和 annotated tag v1 指向更早的 commit，名字 both 同时是分支和 tag（分别指向不同的 commit）。
// 返回 main、dev（= v1）和 tag both 的 commit。
func dispatchRepo(e *testEnv) (mainSHA, devSHA, bothTagSHA string) {
	e.t.Helper()
	devSHA = e.commit(map[string]string{wf("manual.yml"): dispatchWorkflow, wf("ev.yml"): eventWorkflow})
	e.git("branch", "dev")
	e.git("tag", "-a", "v1", "-m", "v1")
	bothTagSHA = e.commit(map[string]string{"x.txt": "tag"})
	e.git("tag", "both")
	mainSHA = e.commit(map[string]string{"x.txt": "main"})
	e.git("branch", "both")
	return mainSHA, devSHA, bothTagSHA
}

// TestDispatch：手动触发解析 ref（空为默认分支；不带前缀时先当作分支再当作 tag），只规划指定的 workflow，
// 以 workflow_dispatch 事件建 run 并记录 inputs、来源和审计日志。
func TestDispatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mainSHA, devSHA, _ := dispatchRepo(e)
	tests := []struct {
		name     string
		workflow string
		ref      string
		wantRef  string
		wantSHA  string
	}{
		{"默认分支", "manual", "", "refs/heads/main", mainSHA},
		{"带扩展名", "manual.yml", "main", "refs/heads/main", mainSHA},
		{"短分支名", "manual", "dev", "refs/heads/dev", devSHA},
		{"短 tag 名（annotated tag 解开到 commit）", "manual", "v1", "refs/tags/v1", devSHA},
		{"同名时分支优先", "manual", "both", "refs/heads/both", mainSHA},
		{"完整的分支 ref", "manual", "refs/heads/dev", "refs/heads/dev", devSHA},
		{"完整的 tag ref", "manual", "refs/tags/v1", "refs/tags/v1", devSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqID := "req-" + tt.name
			id, err := e.p.Dispatch(e.ctx, &DispatchRequest{
				Repo: "Tester/Demo", Workflow: tt.workflow, Ref: tt.ref, Inputs: map[string]string{"level": "high"},
				RequestID: reqID, Actor: "alice", Source: SourceDashboard,
			})
			if err != nil {
				t.Fatalf("Dispatch：%v", err)
			}
			r := e.run(id)
			if r.Event != workflow.EventWorkflowDispatch || r.Ref != tt.wantRef || r.SHA != tt.wantSHA || r.Workflow != "manual.yml" {
				t.Fatalf("run：event=%s ref=%s sha=%s workflow=%s，期望 ref=%s sha=%s", r.Event, r.Ref, r.SHA, r.Workflow, tt.wantRef, tt.wantSHA)
			}
			if r.Source != SourceDashboard || r.SenderLogin != "alice" || r.Inputs["level"] != "high" || r.Priority != priorityNormal {
				t.Errorf("run：source=%s sender=%s inputs=%v priority=%d", r.Source, r.SenderLogin, r.Inputs, r.Priority)
			}
			var key string
			if err := e.db.QueryRow(e.ctx, `SELECT dedupe_key FROM runs WHERE id=$1`, id).Scan(&key); err != nil || key != "dispatch:"+reqID {
				t.Errorf("dedupe_key = %q（%v），期望 dispatch:%s", key, err, reqID)
			}
			// 各子测试的 job 内容相同时 action key 相同：第一个进入 pending，之后的跟随它（同 key 合并）
			j := e.jobs(id, 0)["go"]
			if j == nil || !strings.Contains(j.Spec.Steps[0].Run, "high") {
				t.Fatalf("job 应使用 inputs 展开：%+v", j)
			}
			if j.State != store.JobPending && (j.State != store.JobWaiting || j.Follows == nil) {
				t.Errorf("job 应进入 pending 或跟随同 key 的 job：state=%s follows=%v", j.State, j.Follows)
			}
			var audits int
			if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action='run.dispatch' AND actor='alice' AND (detail->>'run_id')::bigint=$1`, id).Scan(&audits); err != nil || audits != 1 {
				t.Errorf("应记录 1 条审计日志，实际 %d（%v）", audits, err)
			}
		})
	}
}

// TestDispatchIdempotent：同一个 RequestID 只建一个 run（重复提交返回同一个 run）；没有 RequestID 时每次都新建。
func TestDispatchIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dispatchRepo(e)
	req := func(id string) *DispatchRequest {
		return &DispatchRequest{Repo: "tester/demo", Workflow: "manual", RequestID: id, Source: SourceCLI}
	}
	first, err := e.p.Dispatch(e.ctx, req("same"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.p.Dispatch(e.ctx, req("same"))
	if err != nil || again != first {
		t.Fatalf("同一个 RequestID 应返回同一个 run：%d、%d（%v）", first, again, err)
	}
	a, errA := e.p.Dispatch(e.ctx, req(""))
	b, errB := e.p.Dispatch(e.ctx, req(""))
	if errA != nil || errB != nil || a == b || a == first {
		t.Fatalf("没有 RequestID 时应各建一个 run：%d、%d（%v、%v）", a, b, errA, errB)
	}
	if n := e.countRuns(); n != 3 {
		t.Fatalf("应共有 3 个 run，实际 %d 个", n)
	}
}

// TestDispatchErrors：ref 找不到、workflow 不存在或不能手动触发时返回 *DispatchError（Field 指出是哪个字段），不建 run。
func TestDispatchErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dispatchRepo(e)
	tests := []struct {
		name      string
		repo      string
		workflow  string
		ref       string
		wantField string // 空表示不是 DispatchError
		inErr     string
	}{
		{"分支不存在", "tester/demo", "manual", "refs/heads/nope", "ref", "找不到 ref refs/heads/nope"},
		{"tag 不存在", "tester/demo", "manual", "refs/tags/nope", "ref", "找不到 ref refs/tags/nope"},
		{"短 ref 不存在", "tester/demo", "manual", "nope", "ref", "找不到 ref nope"},
		{"workflow 文件不存在", "tester/demo", "missing", "", "workflow", ".kiln/workflows/missing.yml"},
		{"没有声明 workflow_dispatch", "tester/demo", "ev", "", "workflow", "没有声明 on.workflow_dispatch"},
		{"仓库不在 Kiln 中", "tester/none", "manual", "", "", "仓库不在 Kiln 中"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.p.Dispatch(e.ctx, &DispatchRequest{Repo: tt.repo, Workflow: tt.workflow, Ref: tt.ref, Source: SourceCLI})
			if err == nil || !strings.Contains(err.Error(), tt.inErr) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tt.inErr)
			}
			var de *DispatchError
			isDE := errors.As(err, &de)
			if tt.wantField == "" {
				if isDE {
					t.Fatalf("不应是 DispatchError：%v", err)
				}
				if !errors.Is(err, store.ErrRepoNotFound) {
					t.Fatalf("应包装 store.ErrRepoNotFound：%v", err)
				}
				return
			}
			if !isDE || de.Field != tt.wantField {
				t.Fatalf("应返回 Field=%s 的 DispatchError：%#v", tt.wantField, err)
			}
			if errors.Unwrap(de) == nil {
				t.Error("DispatchError 应能取出原始错误")
			}
		})
	}
	if n := e.countRuns(); n != 0 {
		t.Fatalf("出错时不应建 run，实际 %d 个", n)
	}
}

// TestDispatchWorkflows：列出 ref 下的 workflow 及能否手动触发（按文件名排序），不建 run。
func TestDispatchWorkflows(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha := e.commit(map[string]string{
		wf("manual.yml"): dispatchWorkflow,
		wf("ev.yml"):     eventWorkflow,
		wf("bad.yml"):    "on:\n  workflow_dispatch: {}\njobs:\n  a:\n    runs-on: linux/arm64\n    steps:\n      - uses: x\n",
	})
	got, err := e.p.DispatchWorkflows(e.ctx, "tester/demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "refs/heads/main" || got.SHA != sha || got.Repo.ID != testRepoID {
		t.Fatalf("目标：ref=%s sha=%s repo=%d", got.Ref, got.SHA, got.Repo.ID)
	}
	var files []string
	for _, w := range got.Workflows {
		files = append(files, w.File)
	}
	if want := []string{"bad.yml", "ev.yml", "manual.yml"}; !slices.Equal(files, want) {
		t.Fatalf("workflow = %v，期望 %v", files, want)
	}
	bad, ev, manual := got.Workflows[0], got.Workflows[1], got.Workflows[2]
	if !bad.Dispatchable || bad.ConfigError == "" {
		t.Errorf("bad.yml 能手动触发但有配置错误：%+v", bad)
	}
	if ev.Dispatchable || ev.Name != "ev" || ev.ConfigError != "" {
		t.Errorf("ev.yml 不能手动触发：%+v", ev)
	}
	if !manual.Dispatchable || len(manual.Inputs) != 1 || manual.Inputs[0].Name != "level" || manual.Inputs[0].Default != "low" {
		t.Errorf("manual.yml 能手动触发并声明了 input level：%+v", manual)
	}
	if n := e.countRuns(); n != 0 {
		t.Fatalf("DispatchWorkflows 不应建 run，实际 %d 个", n)
	}

	_, err = e.p.DispatchWorkflows(e.ctx, "tester/demo", "nope")
	var de *DispatchError
	if !errors.As(err, &de) || de.Field != "ref" {
		t.Fatalf("ref 找不到时应返回 Field=ref 的 DispatchError：%v", err)
	}
}

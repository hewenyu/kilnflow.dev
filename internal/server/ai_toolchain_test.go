package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 受管理的工具链（PLAN #33.12）：声明了 requirements.tools 的任务只派给上报工具链就绪的节点，下发的任务带着按 lockfile
// 解析出的工具（名称、版本、平台、digest）；没有上报（旧连接器）或不就绪时不派，等待原因写明；不声明工具的任务不受影响。
func TestAIToolchainDispatch(t *testing.T) {
	config := strings.Replace(portConfig, "version: 1\n", "version: 1\nrequirements: {tools: [go, pnpm]}\n", 1)
	p := newSplitPort(t, config)
	goDigest, pnpmDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	p.base = p.commit("main", map[string]string{toolchain.LockPath: "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + goDigest + "\"\n" +
		"[pnpm]\nversion = \"12.14.0\"\nany = \"" + pnpmDigest + "\"\n"})
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})

	acquire := func(tc *airunnerv2.Readiness) *airunnerv2.AITask {
		t.Helper()
		st := p.state(r)
		st.Toolchain = tc
		resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: st, RequestId: fmt.Sprintf("req-%d", time.Now().UnixNano())}))
		if err != nil {
			t.Fatalf("领取：%v", err)
		}
		return resp.Msg.GetTask()
	}
	// 旧连接器（不上报工具链）：不派
	if task := acquire(nil); task != nil {
		t.Fatalf("没有上报工具链的节点领到了任务 #%d", task.TaskId)
	}
	if v := p.task(id); v.state != "pending" || !strings.Contains(v.skipReason, "需要受管理的工具链（go 1.27.3、pnpm 12.14.0）") || !strings.Contains(v.skipReason, "版本过旧") {
		t.Fatalf("任务 = %+v", v)
	}
	// 上报了不就绪：不派，原因来自节点
	if task := acquire(&airunnerv2.Readiness{Ready: false, Reason: "没有找到 kiln-runner"}); task != nil {
		t.Fatalf("工具链不就绪的节点领到了任务 #%d", task.TaskId)
	}
	if v := p.task(id); !strings.Contains(v.skipReason, "没有找到 kiln-runner") {
		t.Fatalf("等待原因 = %q", v.skipReason)
	}
	// 就绪：派出，带着工具
	task := acquire(&airunnerv2.Readiness{Ready: true})
	if task == nil || task.TaskId != id {
		t.Fatalf("领到 %v，期望任务 #%d", task, id)
	}
	want := []*airunnerv2.ToolRef{
		{Name: "go", Version: "1.27.3", Platform: "darwin/arm64", Digest: goDigest},
		{Name: "pnpm", Version: "12.14.0", Platform: "any", Digest: pnpmDigest},
	}
	if len(task.Tools) != len(want) {
		t.Fatalf("工具 = %v", task.Tools)
	}
	for i := range want {
		if !proto.Equal(task.Tools[i], want[i]) {
			t.Errorf("第 %d 个工具 = %v，期望 %v", i, task.Tools[i], want[i])
		}
	}
	// 节点上报的工具链状态保存下来（Dashboard 显示）
	var state, reason string
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT toolchain_state, toolchain_reason FROM ai_runners WHERE runner_id=$1`, r.id).Scan(&state, &reason))
	if state != "ready" || reason != "" {
		t.Errorf("保存的状态 = %s（%s）", state, reason)
	}
}

// 没有声明工具的任务照常派给不支持工具链的旧连接器，下发的任务中没有工具。
func TestAIToolchainNotRequired(t *testing.T) {
	p := newSplitPort(t, portConfig)
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	st := p.state(r)
	resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: st, RequestId: "req-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if task := resp.Msg.GetTask(); task == nil || task.TaskId != id || len(task.Tools) != 0 {
		t.Fatalf("领到 %v，期望没有工具的任务 #%d", task, id)
	}
	var state string
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT toolchain_state FROM ai_runners WHERE runner_id=$1`, r.id).Scan(&state))
	if state != "unknown" {
		t.Errorf("没有上报时的状态 = %s，期望 unknown", state)
	}
}

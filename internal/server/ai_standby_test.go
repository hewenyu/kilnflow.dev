package server

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
)

// standbyLongPoll 是这个测试的长轮询时间：远大于 standbyMaxWait，按时返回说明没有长轮询。
const (
	standbyLongPoll = time.Minute
	standbyMaxWait  = 10 * time.Second
)

// AI 执行节点这一轮不接任务（standby：本地 drain、正在恢复上次的任务）或者执行中的任务已经达到并发上限时，Server 只记录
// 状态、下发指令，不派任务、不长轮询（PLAN #33.4、#33.6）；之后正常的 Acquire 照常领取。
func TestAIAcquireStandby(t *testing.T) {
	p := newSplitPort(t, portConfig, func(c *Config) { c.AcquireLongPoll = standbyLongPoll })
	_, id := planned(t, p, 7)
	r := p.runner(runnerSpec{})
	busy := p.state(r)
	busy.Running, busy.Capacity = 1, 1
	for name, req := range map[string]*airunnerv2.AcquireRequest{
		"standby": {State: p.state(r), RequestId: "standby", Standby: true},
		"running": {State: busy, RequestId: "running"},
	} {
		start := time.Now()
		resp, err := p.ai.Acquire(p.ctx, withToken(r.token, req))
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if resp.Msg.GetTask() != nil || resp.Msg.GetDirectives() == nil {
			t.Fatalf("%s：不应派任务、应下发指令：%v", name, resp.Msg)
		}
		if d := time.Since(start); d > standbyMaxWait {
			t.Fatalf("%s：Acquire 用了 %v，不应长轮询", name, d)
		}
	}
	if task := mustAcquire(t, p, r); task.id != id {
		t.Fatalf("领到任务 #%d，应为 #%d", task.id, id)
	}
}

// AI 执行节点只能用 airunner.v2：调 runner.v1 的 Acquire（CI Runner 的协议）被拒绝，不下发任何东西（PLAN #33.6）。
func TestRunnerV1RejectsAIRunner(t *testing.T) {
	p := newSplitPort(t, portConfig)
	r := p.runner(runnerSpec{})
	_, err := p.rpc.Acquire(p.ctx, withToken(r.token, &runnerv1.AcquireRequest{RequestId: "old", WaitSeconds: 1}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("AI 执行节点调 runner.v1 Acquire = %v，应为 PermissionDenied", err)
	}
}

package planner

import (
	"maps"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/server/ci/cistore"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// 可信的自动 tag push（PLAN #29.4）：只有 Server 自己自动打的 tag（来源自 autotag、push 事件、tag ref）才放行
// Bot 执行引用 secret 的 job 和发布 job；手动推送的 tag、重跑都按普通 Bot 规则处理。首次规划和重跑用同一条规则。

const tokenWorkflow = `name: token
on:
  push:
    tags: ["v*"]
jobs:
  secret:
    runs-on: linux/arm64
    env:
      TOKEN: ${{ secrets.TOKEN }}
    steps:
      - run: echo secret
`

func TestIsTrustedAutoTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		source, event, ref string
		want               bool
	}{
		{"自动 tag push", store.SourceAutoTag, "push", "refs/tags/v1.0.0", true},
		{"webhook 的 tag push", store.SourceWebhook, "push", "refs/tags/v1.0.0", false},
		{"自动来源但 ref 是分支", store.SourceAutoTag, "push", "refs/heads/main", false},
		{"自动来源但不是 push", store.SourceAutoTag, "pull_request", "refs/tags/v1.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTrustedAutoTag(tt.source, tt.event, tt.ref); got != tt.want {
				t.Fatalf("isTrustedAutoTag(%q,%q,%q) = %v，期望 %v", tt.source, tt.event, tt.ref, got, tt.want)
			}
		})
	}
}

// tagPushJobs 规划一次 tag push（两个 workflow 各一个 run），返回所有 job（按 job key）。
func tagPushJobs(e *testEnv, sha string, bot bool, source string) map[string]*cistore.Job {
	e.t.Helper()
	req := e.pushReq("refs/tags/v1.0.0", sha)
	req.SenderLogin, req.SenderIsBot, req.Source = "kiln-test[bot]", bot, source
	if !bot {
		req.SenderLogin = testOwner
	}
	out := map[string]*cistore.Job{}
	for _, id := range e.plan(req) {
		maps.Copy(out, e.jobs(id, 0))
	}
	return out
}

// TestTrustedAutoTagJobs：可信的自动 tag push 保留 Bot 发送者，但 secret 和发布 job 照常执行；手动 tag push 的 Bot 仍然被跳过。
func TestTrustedAutoTagJobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		bot    bool
		source string
		skip   bool
	}{
		{"手动 tag push 的 Bot", true, store.SourceWebhook, true},
		{"自动 tag push 的 Bot", true, store.SourceAutoTag, false},
		{"自动 tag push 的人", false, store.SourceAutoTag, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("token.yml"): tokenWorkflow, wf("rel.yml"): publishWorkflow("", "")})
			jobs := tagPushJobs(e, sha, tt.bot, tt.source)
			if len(jobs) != 3 {
				t.Fatalf("应有 3 个 job：%v", jobs)
			}
			secret, publish := jobs["secret"], jobs["publish"]
			if !tt.skip {
				if secret.State != store.JobPending {
					t.Errorf("secret job 应照常执行：%s %q", secret.State, deref(secret.Reason))
				}
				if publish.Kind != workflow.JobKindPublish || publish.State != store.JobWaiting {
					t.Errorf("发布 job 应等待 build：%s %q", publish.State, deref(publish.Reason))
				}
				return
			}
			if secret.State != store.JobSkipped || !strings.Contains(deref(secret.Reason), "Bot") {
				t.Errorf("secret job 应被跳过：%s %q", secret.State, deref(secret.Reason))
			}
			if publish.State != store.JobSkipped || !strings.Contains(deref(publish.Reason), "Bot") {
				t.Errorf("发布 job 应被跳过：%s %q", publish.State, deref(publish.Reason))
			}
		})
	}
}

// TestTrustedAutoTagRerun：重跑同一条规则（run 记录的来源是 autotag 才可信，手动 tag push 仍然跳过）。
func TestTrustedAutoTagRerun(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{"自动 tag push": store.SourceAutoTag, "手动 tag push": store.SourceWebhook} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("token.yml"): tokenWorkflow})
			req := e.pushReq("refs/tags/v1.0.0", sha)
			req.SenderLogin, req.SenderIsBot, req.Source = "kiln-test[bot]", true, source
			r := e.planOne(req)
			e.finishRun(r.ID, map[string]store.JobState{"secret": store.JobFailed})
			n, err := e.p.Rerun(e.ctx, RerunRequest{RunID: r.ID, Mode: store.RerunAll, Actor: "alice"})
			if err != nil || n != 1 {
				t.Fatalf("Rerun = %d, %v，期望第 1 次重跑", n, err)
			}
			j := e.jobs(r.ID, n)["secret"]
			if source == store.SourceAutoTag {
				if j.State != store.JobPending {
					t.Fatalf("可信自动 tag 的重跑应执行 secret job：%s %q", j.State, deref(j.Reason))
				}
			} else if j.State != store.JobSkipped || !strings.Contains(deref(j.Reason), "Bot") {
				t.Fatalf("手动 tag push 的重跑仍然跳过：%s %q", j.State, deref(j.Reason))
			}
		})
	}
}

// TestAutoTagAndWebhookShareRun（PLAN #29.4）：自动打 tag 和手动推送同一个 tag 共用去重键，不论谁先，只建一个 run。
func TestAutoTagAndWebhookShareRun(t *testing.T) {
	t.Parallel()
	for name, order := range map[string][]string{
		"手动在前": {store.SourceWebhook, store.SourceAutoTag},
		"自动在前": {store.SourceAutoTag, store.SourceWebhook},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			sha := e.commit(map[string]string{wf("token.yml"): tokenWorkflow})
			for i, source := range order {
				req := e.pushReq("refs/tags/v0.3.15", sha)
				req.Source = source
				ids := e.plan(req)
				want := 0
				if i == 0 {
					want = 1
				}
				if len(ids) != want {
					t.Fatalf("第 %d 次规划（%s）新建 %d 个 run，期望 %d", i+1, source, len(ids), want)
				}
			}
			if n := e.countRuns(); n != 1 {
				t.Fatalf("只应有一个 run：%d", n)
			}
		})
	}
}

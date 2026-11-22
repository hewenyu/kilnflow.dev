package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	airunnerv2 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/airunner/v2"
	"github.com/hewenyu/kilnflow.dev/internal/agentcfg"
	"github.com/hewenyu/kilnflow.dev/internal/fakegithub"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aidev"
	"github.com/hewenyu/kilnflow.dev/internal/server/ai/aispec"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
)

// 审计平台（PLAN #26.1、#30.7）：任务记录可以执行的平台，节点上报的平台在其中才能领取；一组平台（any_of）的任务由组内
// 先来领取的节点执行，只执行一次；领取时记下实际执行的平台和节点，Check Run 和 PR review 中看得出。

const (
	platMac        = "darwin/arm64"
	platLinux      = "linux/amd64"
	platLinuxARM64 = "linux/arm64"
)

// platformConfig 返回启用审计的配置：required_platforms 是给出的 YAML 列表项。
func platformConfig(requirements string, items ...string) string {
	var b strings.Builder
	b.WriteString("version: 1\n" + requirements + "review:\n  agent: codex\n  required_platforms:\n")
	for _, it := range items {
		b.WriteString("    - " + it + "\n")
	}
	return b.String()
}

// platformRunner 注册一台上报 platform 的审计节点（容量 2：领不到不是因为满了）。
func platformRunner(p *splitPort, platform string) *portRunner {
	r := p.runner(runnerSpec{})
	r.platform, r.capacity = platform, 2
	return r
}

// acquireTask 让节点领取一次，返回领到的任务（没有领到时为 nil）。
func acquireTask(t *testing.T, p *splitPort, r *portRunner, st *airunnerv2.AIRunnerState) *airunnerv2.AITask {
	t.Helper()
	if st == nil {
		st = p.state(r)
	}
	resp, err := p.ai.Acquire(p.ctx, withToken(r.token, &airunnerv2.AcquireRequest{State: st, RequestId: fmt.Sprintf("req-%d", p.next())}))
	if err != nil {
		t.Fatalf("领取：%v", err)
	}
	return resp.Msg.GetTask()
}

// attemptPlatform 返回任务当前 attempt 记下的平台和节点。
func attemptPlatform(t *testing.T, p *splitPort, taskID int64) (platform, runner string) {
	t.Helper()
	mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT a.platform, a.runner_name FROM ai_tasks t JOIN ai_attempts a ON a.id=t.current_attempt_id
		WHERE t.id=$1`, taskID).Scan(&platform, &runner))
	return platform, runner
}

// 单个平台的任务只派给上报这个平台的节点：linux/amd64 的审计不派给 macOS 节点，反之亦然；linux/arm64 的节点两个都领不到。
func TestAIPlatformDispatch(t *testing.T) {
	p := newSplitPort(t, platformConfig("", "{os: linux, arch: amd64}", "{os: macos, arch: arm64}"))
	req := p.planReview(7, p.prHead(7))
	ids := p.tasks(req)
	if len(ids) != 2 {
		t.Fatalf("任务 = %v，应为两个（每个平台一个）", ids)
	}
	byPlatform := map[string]int64{}
	for _, id := range ids {
		var platforms []string
		mustNil(t, p.s.db.QueryRow(p.ctx, `SELECT platforms FROM ai_tasks WHERE id=$1`, id).Scan(&platforms))
		if len(platforms) != 1 {
			t.Fatalf("任务 #%d 的平台 = %v", id, platforms)
		}
		byPlatform[platforms[0]] = id
	}
	if byPlatform[platLinux] == 0 || byPlatform[platMac] == 0 {
		t.Fatalf("任务的平台 = %v", byPlatform)
	}

	arm := platformRunner(p, platLinuxARM64)
	if task := acquireTask(t, p, arm, nil); task != nil {
		t.Fatalf("linux/arm64 的节点领到了任务 #%d", task.TaskId)
	}
	for _, c := range []struct{ platform, other string }{{platMac, platLinux}, {platLinux, platMac}} {
		r := platformRunner(p, c.platform)
		task := acquireTask(t, p, r, nil)
		if task == nil || task.TaskId != byPlatform[c.platform] {
			t.Fatalf("%s 的节点领到 %v，应为任务 #%d", c.platform, task, byPlatform[c.platform])
		}
		if !strings.Contains(task.Prompt, "执行平台："+c.platform) {
			t.Errorf("%s 的 prompt 中没有执行平台", c.platform)
		}
		if plat, name := attemptPlatform(t, p, task.TaskId); plat != c.platform || name != r.name {
			t.Errorf("attempt 记下的平台 = %s、节点 = %s，应为 %s、%s", plat, name, c.platform, r.name)
		}
		// 还有空闲槽位，也领不到另一个平台的任务
		if again := acquireTask(t, p, r, nil); again != nil {
			t.Fatalf("%s 的节点又领到了任务 #%d（%s 的任务）", c.platform, again.TaskId, c.other)
		}
	}
	if task := acquireTask(t, p, arm, nil); task != nil {
		t.Fatalf("linux/arm64 的节点领到了任务 #%d", task.TaskId)
	}
}

// 一组平台的任务：组内先来领取的节点领走，只执行一次，组内另一台、组外的节点都领不到；等待原因列出这一组平台；
// 领取后任务的说明、attempt、Check Run 和 PR review 写明实际执行的平台和节点。两个领取顺序都要成立。
func TestAIPlatformGroup(t *testing.T) {
	for _, first := range []string{platLinux, platMac} {
		t.Run(first, func(t *testing.T) {
			p := newSplitPort(t, platformConfig("", "{id: any, any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]}"))
			e := p.portEnv
			req, id := planned(t, p, 7)
			if v := p.task(id); v.state != "pending" || v.reason != "等待 AI 审计节点领取（平台 darwin/arm64 或 linux/amd64）" {
				t.Fatalf("任务 = %+v", v)
			}
			sha := p.reqSHA(req)
			e.githubChecks(sha)
			if s := taskCheckSummary(t, e, sha); !strings.Contains(s, "平台：`darwin/arm64` 或 `linux/amd64`") {
				t.Errorf("领取前的 Check Run 摘要 = %q", s)
			}

			outside := platformRunner(p, platLinuxARM64)
			if task := acquireTask(t, p, outside, nil); task != nil {
				t.Fatalf("组外的 linux/arm64 节点领到了任务 #%d", task.TaskId)
			}
			second := platMac
			if first == platMac {
				second = platLinux
			}
			r1, r2 := platformRunner(p, first), platformRunner(p, second)
			task := acquireTask(t, p, r1, nil)
			if task == nil || task.TaskId != id {
				t.Fatalf("%s 的节点领到 %v，应为任务 #%d", first, task, id)
			}
			if again := acquireTask(t, p, r2, nil); again != nil {
				t.Fatalf("任务已被 %s 的节点领走，%s 的节点又领到了任务 #%d", first, second, again.TaskId)
			}
			if plat, name := attemptPlatform(t, p, id); plat != first || name != r1.name {
				t.Errorf("attempt 记下的平台 = %s、节点 = %s，应为 %s、%s", plat, name, first, r1.name)
			}
			want := fmt.Sprintf("在节点 %s（%s）上执行", r1.name, first)
			if v := p.task(id); v.state != "running" || v.attempts != 1 || v.reason != want {
				t.Fatalf("任务 = %+v，说明应为 %q", v, want)
			}
			e.githubChecks(sha)
			if s := taskCheckSummary(t, e, sha); !strings.Contains(s, fmt.Sprintf("平台：`%s`（节点 %s）", first, r1.name)) {
				t.Errorf("领取后的 Check Run 摘要 = %q", s)
			}

			pt := &portTask{id: task.TaskId, attempt: task.Ref.AttemptId, epoch: task.Ref.Epoch}
			if _, err := p.complete(r1, pt, succeeded(okReport)); err != nil {
				t.Fatalf("提交结果：%v", err)
			}
			if st := p.reqState(req); st != "succeeded" {
				t.Fatalf("请求 = %s，应为 succeeded（只审计一次）", st)
			}
			e.githubChecks(sha)
			reviews := e.fake.Reviews(portOwner+"/demo", 7)
			if len(reviews) != 1 || !strings.Contains(reviews[0].Body, fmt.Sprintf("· `%s`（节点 %s）", first, r1.name)) {
				t.Fatalf("PR review = %+v，应写明执行的平台和节点", reviews)
			}
			if again := acquireTask(t, p, r2, nil); again != nil {
				t.Fatalf("审计结束后 %s 的节点又领到了任务 #%d", second, again.TaskId)
			}
		})
	}
}

// taskCheckSummary 返回 commit sha 上任务级 Check Run（审计任务 review-any）的摘要。
func taskCheckSummary(t *testing.T, e *portEnv, sha string) string {
	t.Helper()
	for _, c := range e.fake.CheckRuns(sha) {
		if !strings.Contains(c.Name, "any") {
			continue
		}
		var out struct {
			Summary string `json:"summary"`
		}
		mustNil(t, json.Unmarshal(c.Output, &out))
		return out.Summary
	}
	t.Fatalf("没有任务级的 Check Run：%+v", e.fake.CheckRuns(sha))
	return ""
}

// 一组平台的工具链（PLAN #33.12）：规划时为组内每个平台各解析一份，领取时按节点的平台下发对应的那一份（同一个版本、
// 不同的 digest）。
func TestAIPlatformGroupTools(t *testing.T) {
	macGo, linuxGo := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("c", 64)
	lock := "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + macGo + "\"\n\"linux/amd64\" = \"" + linuxGo + "\"\n"
	for platform, digest := range map[string]string{platMac: macGo, platLinux: linuxGo} {
		t.Run(platform, func(t *testing.T) {
			p := newSplitPort(t, platformConfig("requirements: {tools: [go]}\n", "{id: any, any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]}"))
			p.base = p.commit("main", map[string]string{toolchain.LockPath: lock})
			_, id := planned(t, p, 7)
			r := platformRunner(p, platform)
			st := p.state(r)
			st.Toolchain = &airunnerv2.Readiness{Ready: true}
			task := acquireTask(t, p, r, st)
			if task == nil || task.TaskId != id {
				t.Fatalf("领到 %v，应为任务 #%d", task, id)
			}
			got := make([]string, 0, len(task.Tools))
			for _, tl := range task.Tools {
				got = append(got, tl.Name+" "+tl.Version+" "+tl.Platform+" "+tl.Digest)
			}
			if want := []string{"go 1.27.3 " + platform + " " + digest}; !slices.Equal(got, want) {
				t.Fatalf("下发的工具 = %v，应为 %v", got, want)
			}
		})
	}
}

// 开发的平台（PLAN #26.7）：development.platform 是一组平台时，选题任务由组内先来领取的开发机执行，只执行一次，组内另一台、
// 组外的开发机都领不到；prompt、attempt 写明实际执行的平台，下发的工具链是这个平台的那一份。两个领取顺序都要成立。
func TestAIDevPlatformGroup(t *testing.T) {
	macGo, linuxGo := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("c", 64)
	lock := "[go]\nversion = \"1.27.3\"\n\"darwin/arm64\" = \"" + macGo + "\"\n\"linux/amd64\" = \"" + linuxGo + "\"\n"
	digest := map[string]string{platMac: macGo, platLinux: linuxGo}
	for _, first := range []string{platLinux, platMac} {
		t.Run(first, func(t *testing.T) {
			p := newSplitPort(t, platformConfig("requirements: {tools: [go]}\n", "{os: macos, arch: arm64}")+
				"development:\n  agent: codex\n  platform:\n    any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]\n")
			p.base = p.commit("main", map[string]string{toolchain.LockPath: lock})
			p.fake.SetIssue(portOwner+"/demo", fakegithub.Issue{Number: 1, Title: "需求", Body: "x", Labels: []string{aidev.LabelReady}, UpdatedAt: time.Now()})
			if _, err := p.s.aiDev.SyncPool(p.ctx, portRepoID, "bob"); err != nil {
				t.Fatal(err)
			}
			second := platMac
			if first == platMac {
				second = platLinux
			}
			dev := func(platform string) (*portRunner, *airunnerv2.AIRunnerState) {
				r := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
				r.platform, r.capacity = platform, 2
				st := p.state(r)
				st.Toolchain = &airunnerv2.Readiness{Ready: true}
				// 先上报一次状态（还没有任务可领）：选题按上报的平台数空闲槽位
				if task := acquireTask(t, p, r, st); task != nil {
					t.Fatalf("还没规划就领到了任务 #%d", task.TaskId)
				}
				return r, st
			}
			outside, outsideSt := dev(platLinuxARM64)
			r1, st1 := dev(first)
			r2, st2 := dev(second)
			res, err := p.s.aiDev.PlanSelect(p.ctx, portRepoID, "bob")
			if err != nil || res.RequestID == 0 {
				t.Fatalf("规划选题：%+v, %v", res, err)
			}
			req := aiReq{id: res.RequestID}
			ids := p.tasks(req)
			if len(ids) != 1 {
				t.Fatalf("任务 = %v", ids)
			}
			id := ids[0]
			if v := p.task(id); v.state != "pending" || v.reason != "等待 AI 开发节点领取（平台 darwin/arm64 或 linux/amd64）" {
				t.Fatalf("任务 = %+v", v)
			}
			if task := acquireTask(t, p, outside, outsideSt); task != nil {
				t.Fatalf("组外的 linux/arm64 开发机领到了任务 #%d", task.TaskId)
			}
			task := acquireTask(t, p, r1, st1)
			if task == nil || task.TaskId != id {
				t.Fatalf("%s 的开发机领到 %v，应为任务 #%d", first, task, id)
			}
			if again := acquireTask(t, p, r2, st2); again != nil {
				t.Fatalf("任务已被 %s 的开发机领走，%s 的开发机又领到了任务 #%d", first, second, again.TaskId)
			}
			if plat, name := attemptPlatform(t, p, id); plat != first || name != r1.name {
				t.Errorf("attempt 记下的平台 = %s、节点 = %s，应为 %s、%s", plat, name, first, r1.name)
			}
			if want := fmt.Sprintf("在节点 %s（%s）上执行", r1.name, first); p.task(id).reason != want {
				t.Errorf("任务的说明 = %q，应为 %q", p.task(id).reason, want)
			}
			if !strings.Contains(task.Prompt, "执行平台："+first) {
				t.Errorf("prompt 中没有执行平台 %s", first)
			}
			got := make([]string, 0, len(task.Tools))
			for _, tl := range task.Tools {
				got = append(got, tl.Platform+" "+tl.Digest)
			}
			if want := []string{first + " " + digest[first]}; !slices.Equal(got, want) {
				t.Errorf("下发的工具 = %v，应为 %v", got, want)
			}
		})
	}
}

// 开发平台为一组（macOS arm64 或 Linux amd64），macOS 开发机暂停接单，只剩一台空闲的 Linux 开发机，排队中有只能在 macOS
// 上执行的同 Agent 开发任务：照样规划选题，Linux 开发机领到它；macOS 专属的任务留在队列中（AI 审计复现的场景）。
func TestAIDevPlatformGroup_macOnlyQueued(t *testing.T) {
	p := newSplitPort(t, platformConfig("", "{os: macos, arch: arm64}")+
		"development:\n  agent: codex\n  platform:\n    any_of: [{os: macos, arch: arm64}, {os: linux, arch: amd64}]\n")
	p.fake.SetIssue(portOwner+"/demo", fakegithub.Issue{Number: 1, Title: "需求", Body: "x", Labels: []string{aidev.LabelReady}, UpdatedAt: time.Now()})
	if _, err := p.s.aiDev.SyncPool(p.ctx, portRepoID, "bob"); err != nil {
		t.Fatal(err)
	}
	dev := func(platform string) (*portRunner, *airunnerv2.AIRunnerState) {
		r := p.runner(runnerSpec{role: store.RunnerRoleDevelopment})
		r.platform = platform
		st := p.state(r)
		if task := acquireTask(t, p, r, st); task != nil {
			t.Fatalf("还没规划就领到了任务 #%d", task.TaskId)
		}
		return r, st
	}
	mac, _ := dev(platMac)
	linux, linuxSt := dev(platLinux)
	var reqID, macOnly int64
	mustNil(t, p.s.db.QueryRow(p.ctx, `WITH n AS (SELECT nextval('ai_requests_id_seq') AS id)
		INSERT INTO ai_requests (id, type, repo_id, event, ref, sha, family_id, state)
		SELECT id, $1, $2, $3, 'refs/heads/main', $4, id, 'active' FROM n RETURNING id`,
		agentcfg.TaskDevelop, portRepoID, aispec.EventDevelop, p.base).Scan(&reqID))
	mustNil(t, p.s.db.QueryRow(p.ctx, `INSERT INTO ai_tasks (request_id, key, type, platforms, agent, display_name, spec, state, timeout_minutes)
		VALUES ($1, 'develop', $2, $3, 'codex', '只能在 macOS 上执行', '{}', 'pending', 1) RETURNING id`,
		reqID, agentcfg.TaskDevelop, []string{platMac}).Scan(&macOnly))
	p.exec(`UPDATE runners SET drain=true WHERE id=$1`, mac.id)

	res, err := p.s.aiDev.PlanSelect(p.ctx, portRepoID, "bob")
	if err != nil || res.RequestID == 0 {
		t.Fatalf("Linux 开发机空闲，却没有规划选题：%+v, %v", res, err)
	}
	ids := p.tasks(aiReq{id: res.RequestID})
	if len(ids) != 1 {
		t.Fatalf("任务 = %v", ids)
	}
	if task := acquireTask(t, p, linux, linuxSt); task == nil || task.TaskId != ids[0] {
		t.Fatalf("Linux 开发机领到 %v，应为选题任务 #%d", task, ids[0])
	}
	if v := p.task(macOnly); v.state != "pending" {
		t.Errorf("macOS 专属的任务 = %+v，应仍在排队", v)
	}
}
